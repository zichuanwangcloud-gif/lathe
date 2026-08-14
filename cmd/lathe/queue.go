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
	slog.Info("开始处理", "issue", j.IssueKey, "owner", j.OwnerID)

	repoID, repoCfg, cloneURL, err := q.resolveRepo(ctx, j)
	if err != nil {
		slog.Error("无法确定任务归属仓库", "issue", j.IssueKey, "owner", j.OwnerID, "err", err)
		// 接单却跑不了不能沉默 —— 人在 Linear 那边指派完就干等。
		// 尽力回帖说明原因；凭据也没配的话只能放弃，日志里已有痕迹。
		q.commentUnresolved(ctx, j, err)
		return
	}

	tk, err := q.tasks.Create(ctx, task.CreateParams{
		UserID: j.OwnerID, RepoID: repoID, LinearIssueKey: j.IssueKey,
	})
	if err != nil {
		// 同一 issue 已有活任务时会撞上部分唯一索引 —— 这是预期行为，非错误
		slog.Warn("建任务失败（可能已有进行中的同名任务）", "issue", j.IssueKey, "err", err)
		return
	}

	if err := q.pipeline.Execute(ctx, runner.ExecuteParams{
		TaskID:   tk.ID,
		Repo:     repoCfg,
		CloneURL: cloneURL,
		IssueID:  j.IssueID,
		Actor:    "node:" + q.cfg.NodeName,
	}); err != nil {
		// 失败三件套已在 pipeline 内部完成，这里只记日志
		slog.Error("任务处理失败", "issue", j.IssueKey, "task", tk.ID, "err", err)
		return
	}
	slog.Info("任务处理完成", "issue", j.IssueKey, "task", tk.ID)
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
	)
	err = q.store.Pool().QueryRow(ctx, `
		SELECT id, provider_repo, default_branch, hotfix_base, protected_branches, branch_pattern,
		       COALESCE(verify_tier_override, '')
		FROM repos WHERE user_id = $1 ORDER BY id LIMIT 1`, j.OwnerID,
	).Scan(&repoID, &providerRepo, &defaultBranch, &hotfixBase, &protected, &pattern, &tierOverride)
	if err != nil {
		return 0, cfg, "", fmt.Errorf("你的账号下没有仓库配置（请在设置页添加仓库）: %w", err)
	}

	cfg = runner.RepoConfig{
		ProviderRepo:       providerRepo,
		DefaultBranch:      defaultBranch,
		HotfixBase:         hotfixBase,
		ProtectedBranches:  protected,
		BranchPattern:      pattern,
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
