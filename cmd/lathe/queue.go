package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/Clouditera/lathe/internal/config"
	"github.com/Clouditera/lathe/internal/runner"
	"github.com/Clouditera/lathe/internal/store"
	"github.com/Clouditera/lathe/internal/task"
)

// queueDepth 是待处理任务的缓冲上限。
//
// 有界而非无界：队列堆积说明处理速度跟不上接单速度，此时快速失败
// 并回帖告知，比默默积压几百个任务更诚实。
const queueDepth = 64

// job 是一条待执行的任务。
type job struct {
	// OwnerID 是任务归属用户（P1.5 第二步）：仓库配置、Linear/GitHub
	// 凭据、看板可见性全部按它隔离。
	OwnerID  int64
	IssueID  string
	IssueKey string

	// TaskID 非零表示重派一个【已存在】的任务行（手动重试 / 启动恢复），
	// 此时不再新建任务 —— 同一 issue 的活任务唯一索引会把新建挡掉，
	// 重试因此永远卡死（任务 #313 的教训）。OwnerID/IssueID/IssueKey
	// 从任务行上取，此字段优先。
	TaskID int64
}

// queue 是任务执行队列。
//
// P1 起支持并发（docs/02-design.md §8）：worker 数 = light + heavy 槽位
// 之和。真正的资源闸门不在派发这里，而在验证阶段 —— 档位要等 diff
// 产出后才可判定（§5.1），因此实现阶段可以并发，验证按定档结果在
// 各自通道里排队（§6.2 双通道限流）。同一 issue 的重复任务由数据库
// 的部分唯一索引挡住；同一仓库 mirror 的 git 管理操作由 runner 内部
// 的互斥串行化。
type queue struct {
	store    *store.Store
	tasks    *task.Machine
	pipeline *runner.Pipeline
	clients  runner.ClientFactory // 接单失败时回帖用
	cfg      config.Config
	ch       chan job
}

func newQueue(st *store.Store, tm *task.Machine, p *runner.Pipeline, cf runner.ClientFactory, cfg config.Config) *queue {
	return &queue{
		store: st, tasks: tm, pipeline: p, clients: cf, cfg: cfg,
		ch: make(chan job, queueDepth),
	}
}

// Enqueue 实现 httpapi.TaskEnqueuer。
func (q *queue) Enqueue(ctx context.Context, ownerUserID int64, issueID, issueKey string) error {
	select {
	case q.ch <- job{OwnerID: ownerUserID, IssueID: issueID, IssueKey: issueKey}:
		return nil
	default:
		return fmt.Errorf("执行队列已满（上限 %d），暂时无法接单", queueDepth)
	}
}

// Requeue 实现 httpapi.TaskEnqueuer：重派已存在的任务行（不新建）。
func (q *queue) Requeue(ctx context.Context, taskID int64) error {
	select {
	case q.ch <- job{TaskID: taskID}:
		return nil
	default:
		return fmt.Errorf("执行队列已满（上限 %d），暂时无法重试", queueDepth)
	}
}

// Reconcile 在启动时对齐数据库与内存队列（§6.4 的单机形态：
// 没有租约，进程重启即视为节点崩溃）：
//
//   - in-flight 行（triaging/implementing/verifying）：agent 进程已随服务
//     退出死亡，按设计边转回 queued 重新派发（从头重跑，不 resume ——
//     resume 留给修复回路）；旧工作区与分支由重派路径丢弃。
//   - queued 行：重新入队，重启前已接单的任务不丢。
//
// pr_open 不算 in-flight：流水线已跑完，在等人工 review。
// 必须在 worker 启动前调用。
func (q *queue) Reconcile(ctx context.Context) error {
	rows, err := q.store.Pool().Query(ctx, `
		SELECT id, state FROM tasks
		WHERE state IN ('triaging', 'implementing', 'verifying')`)
	if err != nil {
		return fmt.Errorf("查询在途任务失败: %w", err)
	}
	var inflight []int64
	for rows.Next() {
		var id int64
		var state string
		if err := rows.Scan(&id, &state); err != nil {
			rows.Close()
			return err
		}
		inflight = append(inflight, id)
	}
	rows.Close()

	for _, id := range inflight {
		if _, err := q.tasks.Transition(ctx, id, task.StateQueued, "system", &task.TransitionOpts{
			Payload: map[string]any{"reason": "restart_reconcile"},
		}); err != nil {
			slog.Error("在途任务恢复失败", "task", id, "err", err)
			continue
		}
		if err := q.Requeue(ctx, id); err != nil {
			slog.Error("在途任务重新入队失败", "task", id, "err", err)
		}
		slog.Info("在途任务已恢复", "task", id)
	}

	rows2, err := q.store.Pool().Query(ctx, `SELECT id FROM tasks WHERE state = 'queued' ORDER BY id`)
	if err != nil {
		return fmt.Errorf("查询排队任务失败: %w", err)
	}
	var queued []int64
	for rows2.Next() {
		var id int64
		if err := rows2.Scan(&id); err != nil {
			rows2.Close()
			return err
		}
		queued = append(queued, id)
	}
	rows2.Close()
	for _, id := range queued {
		if err := q.Requeue(ctx, id); err != nil {
			slog.Error("排队任务重新入队失败", "task", id, "err", err)
		}
	}
	if len(inflight)+len(queued) > 0 {
		slog.Info("启动恢复完成", "requeued_inflight", len(inflight), "requeued_queued", len(queued))
	}
	return nil
}

// work 启动 worker 协程池消费队列，直到 ctx 结束。
func (q *queue) work(ctx context.Context) {
	workers := q.cfg.LightSlots + q.cfg.HeavySlots
	if workers < 1 {
		workers = 1
	}
	slog.Info("执行队列已启动", "workers", workers,
		"light_slots", q.cfg.LightSlots, "heavy_slots", q.cfg.HeavySlots, "depth", queueDepth)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case j := <-q.ch:
					q.runOne(ctx, j)
				}
			}
		}(i)
	}

	<-ctx.Done()
	slog.Info("执行队列停止，等待在途任务收尾")
	wg.Wait()
}

func (q *queue) runOne(ctx context.Context, j job) {
	// 重派路径（TaskID 非零）：任务行已存在，从行上取属主与 issue。
	// 新建路径：job 里带着 webhook/API 调用方给的属主与 issue。
	if j.TaskID > 0 {
		tk, err := q.tasks.Get(ctx, j.TaskID)
		if err != nil {
			slog.Error("重派任务读取失败", "task", j.TaskID, "err", err)
			return
		}
		j.OwnerID = tk.UserID
		j.IssueKey = tk.LinearIssueKey
		if tk.LinearIssueID != nil {
			j.IssueID = *tk.LinearIssueID
		}
	}
	slog.Info("开始处理", "issue", j.IssueKey, "owner", j.OwnerID)

	repoID, repoCfg, cloneURL, err := q.resolveRepo(ctx, j)
	if err != nil {
		slog.Error("无法确定任务归属仓库", "issue", j.IssueKey, "owner", j.OwnerID, "err", err)
		// 接单却跑不了不能沉默 —— 人在 Linear 那边指派完就干等。
		// 尽力回帖说明原因；凭据也没配的话只能放弃，日志里已有痕迹。
		q.commentUnresolved(ctx, j, err)
		return
	}

	var taskID int64
	if j.TaskID > 0 {
		// 重派：丢弃旧现场（工作区 + 分支），否则 worktree add -b 会
		// 撞同名残留直接失败。现场的使命是留给人接手（D4）；人选择
		// 重试即表示交给机器重跑，旧现场作废。
		tk, _ := q.tasks.Get(ctx, j.TaskID)
		if tk != nil {
			if tk.WorktreePath != nil && *tk.WorktreePath != "" {
				branch := ""
				if tk.BranchName != nil {
					branch = *tk.BranchName
				}
				q.pipeline.Worktrees.Discard(ctx, repoCfg.ProviderRepo, *tk.WorktreePath, branch)
			}
		}
		if j.IssueID == "" || j.IssueID == j.IssueKey {
			// 旧数据没有 Linear issue UUID（migration 0010 前），无法重跑。
			// 取消而非失败：这不是任务本身的错，是数据不够。
			reason := "缺少 Linear issue ID，无法重跑（请重新触发该 issue）"
			if _, err := q.tasks.Transition(ctx, j.TaskID, task.StateCancelled, "system", &task.TransitionOpts{
				FailureReason: &reason,
			}); err != nil {
				slog.Error("标记无法重跑的任务失败", "task", j.TaskID, "err", err)
			}
			return
		}
		taskID = j.TaskID
	} else {
		tk, err := q.tasks.Create(ctx, task.CreateParams{
			UserID: j.OwnerID, RepoID: repoID, LinearIssueKey: j.IssueKey,
			LinearIssueID: j.IssueID,
		})
		if err != nil {
			// 同一 issue 已有活任务时会撞上部分唯一索引 —— 这是预期行为，非错误
			slog.Warn("建任务失败（可能已有进行中的同名任务）", "issue", j.IssueKey, "err", err)
			return
		}
		taskID = tk.ID
	}

	if err := q.pipeline.Execute(ctx, runner.ExecuteParams{
		TaskID:   taskID,
		Repo:     repoCfg,
		CloneURL: cloneURL,
		IssueID:  j.IssueID,
		Actor:    "node:" + q.cfg.NodeName,
	}); err != nil {
		// 失败三件套已在 pipeline 内部完成，这里只记日志
		slog.Error("任务处理失败", "issue", j.IssueKey, "task", taskID, "err", err)
		return
	}
	slog.Info("任务处理完成", "issue", j.IssueKey, "task", taskID)
}

// resolveRepo 查出要操作的仓库：属主名下的第一条配置。
//
// 数据隔离（P1.5 第二步）后，每个用户各自在设置页登记仓库；按 issue 的
// team/project 路由到不同仓库仍是后续项（docs/02-design.md §8）。
func (q *queue) resolveRepo(ctx context.Context, j job) (repoID int64, cfg runner.RepoConfig, cloneURL string, err error) {
	var (
		providerRepo  string
		defaultBranch string
		hotfixBase    string
		protected     []string
		pattern       string
		tierOverride  string
		excludeDirs   []string
	)
	err = q.store.Pool().QueryRow(ctx, `
		SELECT id, provider_repo, default_branch, hotfix_base, protected_branches, branch_pattern,
		       COALESCE(verify_tier_override, ''), exclude_dirs
		FROM repos WHERE user_id = $1 ORDER BY id LIMIT 1`, j.OwnerID,
	).Scan(&repoID, &providerRepo, &defaultBranch, &hotfixBase, &protected, &pattern, &tierOverride, &excludeDirs)
	if err != nil {
		return 0, cfg, "", fmt.Errorf("你的账号下没有仓库配置（请在设置页添加仓库）: %w", err)
	}

	cfg = runner.RepoConfig{
		ProviderRepo:       providerRepo,
		DefaultBranch:      defaultBranch,
		HotfixBase:         hotfixBase,
		ProtectedBranches:  protected,
		BranchPattern:      pattern,
		ExcludeDirs:        excludeDirs,
		VerifyTierOverride: tierOverride,
	}
	return repoID, cfg, "git@github.com:" + providerRepo + ".git", nil
}

// commentUnresolved 在无法接单时尽力回帖说明原因。
//
// 多用户之后这一步更常见：新用户配好了 webhook 却还没登记仓库，
// 指派事件照样投递。不回帖的话 Linear 那边看起来就是「指派了但
// 毫无反应」，比明确的拒绝难受得多。
func (q *queue) commentUnresolved(ctx context.Context, j job, cause error) {
	if q.clients == nil {
		return
	}
	clients, err := q.clients.ForUser(ctx, j.OwnerID)
	if err != nil {
		return
	}
	lin, err := clients.Linear(ctx)
	if err != nil {
		return // 凭据也没配 —— 无从回帖，日志已留痕
	}
	body := "Lathe 无法接单：" + cause.Error() + "\n\n配置完成后重新指派即可触发。"
	if _, err := lin.Comment(ctx, j.IssueID, body); err != nil {
		slog.Warn("接单失败回帖也没发出去", "issue", j.IssueKey, "err", err)
	}
}
