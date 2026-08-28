package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Clouditera/lathe/internal/config"
	"github.com/Clouditera/lathe/internal/runner"
	"github.com/Clouditera/lathe/internal/store"
	"github.com/Clouditera/lathe/internal/task"
)

// claimLeaseDuration 是 ClaimReady 打的租约时长。
//
// 这个租约不是用来防"worker 卡死不放手"的（那种情形下任务已经离开
// queued 进了 triaging/implementing/verifying，早就不在 ClaimReady 的
// 候选范围内了，见 task.Machine.ClaimReady 的文档）。它只是为了堵一条
// 理论竞态：两个 worker 几乎同时执行 ClaimReady 的
// UPDATE ... RETURNING 语句时，SKIP LOCKED 保证不会选中同一行，但从
// "挑出候选"到"这次 UPDATE 提交"之间仍有极短的窗口。租约时长因此只
// 需要盖住这个窗口，与真正的任务处理时长无关——几分钟足够宽松。
const claimLeaseDuration = 2 * time.Minute

// claimPollInterval 是 worker 领不到任务时的退避间隔。
//
// 没有活干是正常状态（ClaimReady 返回 (nil, nil)），但如果不退避会
// 变成忙等，把数据库打爆。几百毫秒量级足够及时（比人能感知到的延迟
// 小得多），又不会造成明显的查询压力。
const claimPollInterval = 300 * time.Millisecond

// pipelineExecutor 是 queue 依赖的流水线执行面：只取 Execute 这一个
// 方法。定义成接口而不是直接用 *runner.Pipeline，是为了让并发压测与
// 单元测试能注入假件——真跑一次 Execute 需要 git mirror、Linear/GitHub
// 凭据、agent 子进程等一整套环境，测调度正确性时这些都是噪音。
type pipelineExecutor interface {
	Execute(ctx context.Context, params runner.ExecuteParams) error
}

// worktreeInspector 是 queue 依赖的工作区体检面：智能重试决策要
// Inspect 现场，Fresh 重建前要 Discard 旧现场。同样只取用到的两个
// 方法，出于与 pipelineExecutor 一致的可测试性考虑。
type worktreeInspector interface {
	Inspect(ctx context.Context, providerRepo, path, branch, base string) *runner.WorktreeState
	Discard(ctx context.Context, providerRepo, path, branch string)
}

// queue 是任务执行队列：DB 领单调度器（docs/06-orchestration.md §2.1）。
//
// 旧形态是 channel FIFO + worker 池：Enqueue 把 job 塞进 channel，worker
// 从 channel 取。这在编排图（依赖就绪判定）出现后不再可行——channel
// 只知道"有没有任务"，不知道"这个任务的前驱是否已放行"，没法表达
// 依赖关系。现形态：Enqueue/Requeue 只负责让任务行落库（或转回
// queued），真正"谁能跑"的判定完全交给数据库——每个 worker
// goroutine 自己轮询 task.Machine.ClaimReady，它在一条 SQL 里原子地
// 完成"挑一个 state=queued 且依赖已就绪的任务 + 打租约"，多个 worker
// 并发调用时同一任务只会被其中恰好一个领到（FOR UPDATE ... SKIP
// LOCKED，见 machine.go）。
//
// worker 数 = light + heavy 槽位之和，语义不变（docs/02-design.md §8）：
// 真正的资源闸门在验证阶段的双通道限流，不在这里。
type queue struct {
	store     *store.Store
	tasks     *task.Machine
	pipeline  pipelineExecutor
	worktrees worktreeInspector
	clients   runner.ClientFactory // 接单失败时回帖用
	cfg       config.Config
}

func newQueue(st *store.Store, tm *task.Machine, p *runner.Pipeline, cf runner.ClientFactory, cfg config.Config) *queue {
	return &queue{
		store: st, tasks: tm, pipeline: p, worktrees: p.Worktrees, clients: cf, cfg: cfg,
	}
}

// Enqueue 实现 httpapi.TaskEnqueuer：解析属主的仓库配置，立即把任务行
// 建到数据库里（state 默认就是 queued）。
//
// 为什么创建要前移到这里，而不是等 worker 捡到时才建（旧实现）：DB
// 领单要求 ClaimReady 能在数据库里 SELECT 到 state='queued' 的行——
// 如果行要等 worker 处理才创建，数据库里永远看不到"还没人处理"的
// 任务，领单查询会一直查到空，调度直接失效。
func (q *queue) Enqueue(ctx context.Context, ownerUserID int64, issueID, issueKey string) error {
	repoID, err := q.resolveRepoID(ctx, ownerUserID)
	if err != nil {
		slog.Error("无法确定任务归属仓库", "issue", issueKey, "owner", ownerUserID, "err", err)
		// 接单却建不出任务不能沉默——人在 Linear 那边指派完就干等。
		// 尽力回帖说明原因；凭据也没配的话只能放弃，日志里已有痕迹。
		q.commentUnresolved(ctx, ownerUserID, issueID, issueKey, err)
		return err
	}

	if _, err := q.tasks.Create(ctx, task.CreateParams{
		UserID: ownerUserID, RepoID: repoID, LinearIssueKey: issueKey, LinearIssueID: issueID,
	}); err != nil {
		// 同一 issue 已有活任务时会撞上部分唯一索引——这是预期行为，非错误
		slog.Warn("建任务失败（可能已有进行中的同名任务）", "issue", issueKey, "err", err)
		return err
	}
	return nil
}

// Requeue 实现 httpapi.TaskEnqueuer：重新派发一个已存在的任务行。
//
// 这里之所以什么都不用做：调用方（httpapi.retryTask 的手动重试，或
// 本文件 Reconcile 的崩溃恢复）在调用 Requeue 之前，已经先把任务行
// Transition 回了 state=queued，并把这次重试的决策依据（mode /
// interrupted_state）写进了那次转移的 task_events payload——任务此刻
// 已经是一条数据库里"就绪待领"的行。DB 领单调度器的轮询循环本来就会
// 持续扫描 state='queued' 的行，不需要谁再"叫醒"它去处理；
// runOneClaimed 领到手时会自己回读事件流找回这次重试的 mode，见该
// 函数的注释。mode 参数因此只用于满足 httpapi.TaskEnqueuer 接口签名，
// 不在这里消费。
func (q *queue) Requeue(ctx context.Context, taskID int64, mode string) error {
	return nil
}

// Reconcile 在启动时把"进程重启=agent 子进程已死"这一事实同步进状态机
// （§6.4 的单机形态：没有租约，进程重启即视为节点崩溃）：
//
//   - in-flight 行（triaging/implementing/verifying）：agent 进程已随
//     服务退出死亡，转回 queued 重新派发（从头重跑，不 resume ——
//     resume 留给修复回路）；旧工作区与分支由重派路径丢弃。
//
// pr_open 不算 in-flight：流水线已跑完，在等人工 review。
//
// 旧实现还有第二段——把已经是 queued 的行重新塞进 channel。DB 领单
// 调度器下这段整个不需要了：worker 的轮询循环本来就会持续扫描
// state='queued' 的行，它们已经在数据库里等着被 ClaimReady 捞到，
// 不需要重启时特意"重新入队"。
//
// 必须在 worker 启动前调用。
func (q *queue) Reconcile(ctx context.Context) error {
	rows, err := q.store.Pool().Query(ctx, `
		SELECT id, state FROM tasks
		WHERE state IN ('triaging', 'implementing', 'verifying')`)
	if err != nil {
		return fmt.Errorf("查询在途任务失败: %w", err)
	}
	var inflight []struct {
		id    int64
		state string
	}
	for rows.Next() {
		var id int64
		var state string
		if err := rows.Scan(&id, &state); err != nil {
			rows.Close()
			return err
		}
		inflight = append(inflight, struct {
			id    int64
			state string
		}{id, state})
	}
	rows.Close()

	for _, it := range inflight {
		// 把中断前的状态记进这次转移的事件 payload：崩溃任务没有
		// failure_stage，智能重试靠它推导断点（崩在 verifying 的任务
		// 不该重跑 agent）。runOneClaimed 领到手时会回读这条事件。
		if _, err := q.tasks.Transition(ctx, it.id, task.StateQueued, "system", &task.TransitionOpts{
			Payload: map[string]any{"reason": "restart_reconcile", "interrupted_state": it.state},
		}); err != nil {
			slog.Error("在途任务恢复失败", "task", it.id, "err", err)
			continue
		}
		slog.Info("在途任务已恢复", "task", it.id, "interrupted_state", it.state)
	}

	if len(inflight) > 0 {
		slog.Info("启动恢复完成", "requeued_inflight", len(inflight))
	}
	return nil
}

// work 启动 worker 协程池，每个协程独立轮询 ClaimReady 直到 ctx 结束。
func (q *queue) work(ctx context.Context) {
	workers := q.cfg.LightSlots + q.cfg.HeavySlots
	if workers < 1 {
		workers = 1
	}
	slog.Info("执行队列已启动（DB 领单）", "workers", workers,
		"light_slots", q.cfg.LightSlots, "heavy_slots", q.cfg.HeavySlots,
		"lease", claimLeaseDuration, "poll_interval", claimPollInterval)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			q.pollLoop(ctx)
		}(i)
	}

	<-ctx.Done()
	slog.Info("执行队列停止，等待在途任务收尾")
	wg.Wait()
}

// pollLoop 是单个 worker 的轮询主体：领到任务就处理，没领到就退避重试。
func (q *queue) pollLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		tk, err := q.tasks.ClaimReady(ctx, claimLeaseDuration)
		if err != nil {
			slog.Error("领单失败", "err", err)
			// 数据库暂时性错误：退避后重试，不要忙等打爆连接池
			select {
			case <-ctx.Done():
				return
			case <-time.After(claimPollInterval):
			}
			continue
		}
		if tk == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(claimPollInterval):
			}
			continue
		}

		q.runOneClaimed(ctx, tk)
	}
}

// runOneClaimed 处理一个刚被 ClaimReady 领到的任务。
//
// 旧实现的 runOne 需要区分"全新任务"（TaskID==0，先建行）与"重派任务"
// （TaskID>0，走智能重试）。现在两者统一了：所有任务都先在 Enqueue/
// Requeue 时刻落库，ClaimReady 领到的永远是一条"已存在的 queued
// 行"——不管它是刚创建的全新任务，还是失败后被人工/自动重派的旧任务。
// 智能重试的体检/决策逻辑（q.planRetry）因此对所有任务统一跑一遍：
// 全新任务没有 worktree、没有 failure_stage，喂给 runner.PlanRetry 后
// 自然落在"现场不可用"的默认分支，算出 Fresh=true/EntryTriage——
// 效果与旧实现里全新任务跳过智能重试、直接从头跑完全一致，只是不再
// 需要用一个 if 分支特殊处理。
func (q *queue) runOneClaimed(ctx context.Context, tk *task.Task) {
	slog.Info("开始处理", "task", tk.ID, "issue", tk.LinearIssueKey, "owner", tk.UserID)

	if tk.LinearIssueID == nil {
		// 旧数据没有 Linear issue UUID（migration 0010 前），分诊/续跑
		// 都要靠它去调 Linear API，没有就没法处理。取消而非失败：这不是
		// 任务本身的错，是数据不够。
		reason := "缺少 Linear issue ID，无法处理（请重新触发该 issue）"
		if _, err := q.tasks.Transition(ctx, tk.ID, task.StateCancelled, "system", &task.TransitionOpts{
			FailureReason: &reason,
		}); err != nil {
			slog.Error("标记无法处理的任务失败", "task", tk.ID, "err", err)
		}
		return
	}
	issueID := *tk.LinearIssueID

	repoCfg, cloneURL, err := q.loadRepoConfig(ctx, tk.RepoID)
	if err != nil {
		slog.Error("无法确定任务归属仓库", "task", tk.ID, "issue", tk.LinearIssueKey, "err", err)
		// 正常情况下这不该发生：repo_id 是任务创建时（Enqueue 里）就
		// 校验过的外键，能失败大概只有仓库配置事后被删掉这种边缘情形。
		// 依然尽力回帖，别让任务悄无声息地卡住。
		q.commentUnresolved(ctx, tk.UserID, issueID, tk.LinearIssueKey, err)
		return
	}

	// F3.1：栈式 PR 后继任务在真正派发前落实分叉基线。
	q.fillBaseRef(ctx, tk, &repoCfg)

	// mode（auto/resume/fresh）与 interrupted_state 都曾经靠内存里的
	// job 结构体从"请求重试/崩溃恢复那一刻"传到"worker 真正处理这一
	// 刻"；DB 领单调度器里这两者之间可以隔着任意长的轮询间隔，内存
	// 传递已经不成立了。好在两者本来就已经写进了任务事件流：
	// httpapi.retryTask 转 queued 时把 mode 写进那次转移的 payload，
	// Reconcile 转 queued 时把 interrupted_state 写进 payload——
	// 事件流本身就是唯一权威来源，直接回读即可，不需要另开一条内存
	// 通道。
	events, err := q.tasks.Events(ctx, tk.ID)
	if err != nil {
		// 读事件流失败不应该阻塞任务处理：退化为"没找到"，按默认策略
		// （auto 模式、无中断状态）处理，比整个放弃这个任务更划算。
		slog.Error("读取任务事件失败，按默认策略处理", "task", tk.ID, "err", err)
	}
	mode := lastEventString(events, "mode")

	var interruptedState task.State
	if tk.FailureStage == nil {
		// FailureStage 非空说明任务走过 fail()，失败阶段已经明确，不
		// 需要靠中断状态兜底（PlanRetry 只在 Stage 为空时才看
		// InterruptedState）。FailureStage 为空说明任务没走过 fail()——
		// 可能是全新任务（事件流里根本没有这个 key，下面取到空串，
		// 对全新任务没有影响），也可能是崩溃恢复（Reconcile 把它从
		// triaging/implementing/verifying 转回 queued 时写下的
		// interrupted_state，断点位置只能从这里还原）。
		if s := lastEventString(events, "interrupted_state"); s != "" {
			interruptedState = task.State(s)
		}
	}

	plan := q.planRetry(ctx, tk, repoCfg, mode, interruptedState)
	if plan.Fresh && tk.WorktreePath != nil && *tk.WorktreePath != "" {
		// 重建路径必须丢弃旧现场（工作区 + 分支），否则 worktree add -b
		// 会撞同名残留直接失败。全新任务没有 worktree_path，这里天然
		// 跳过。
		branch := ""
		if tk.BranchName != nil {
			branch = *tk.BranchName
		}
		q.worktrees.Discard(ctx, repoCfg.ProviderRepo, *tk.WorktreePath, branch)
	}
	slog.Info("重试决策", "task", tk.ID, "plan", plan.String(), "reasons", plan.Reasons)

	if err := q.pipeline.Execute(ctx, runner.ExecuteParams{
		TaskID:   tk.ID,
		Repo:     repoCfg,
		CloneURL: cloneURL,
		IssueID:  issueID,
		Actor:    "node:" + q.cfg.NodeName,
		Retry:    &plan,
	}); err != nil {
		// 失败三件套已在 pipeline 内部完成，这里只记日志
		slog.Error("任务处理失败", "issue", tk.LinearIssueKey, "task", tk.ID, "err", err)
		return
	}
	slog.Info("任务处理完成", "issue", tk.LinearIssueKey, "task", tk.ID)
}

// fillBaseRef 是 F3.1（栈式 PR 的地基）：DependsOn 非空的后继任务在
// 派发前需要知道该从哪个分支分叉。
//
// base_ref 只在第一次派发时写入并从此固定：任务如果失败重试，前驱可能
// 已经被丢弃重建过，分支名会变，但后继当初分叉出去的那个分支不该跟着
// 变——这正是 docs/06-orchestration.md §2.2 里"base_ref 冗余存一份"
// 的设计理由。所以准确的条件是仅当 tk.BaseRef 为 nil 时才去看前驱当前
// 的分支名并落库；已经写过的 base_ref 永远不会被覆盖。
func (q *queue) fillBaseRef(ctx context.Context, tk *task.Task, repoCfg *runner.RepoConfig) {
	if tk.DependsOn == nil {
		return // 独立根：不覆盖 BaseRefOverride，行为与改造前完全一致
	}

	if tk.BaseRef == nil {
		predecessor, err := q.tasks.Get(ctx, *tk.DependsOn)
		if err != nil {
			slog.Error("读取前驱任务失败，base_ref 暂不填充", "task", tk.ID, "depends_on", *tk.DependsOn, "err", err)
		} else if predecessor.State == task.StateMerged {
			// 前驱已经合并：它的分支很快会被 MergePoller 回收（F4.2），
			// 即使还没回收，把它当 base_ref override 也没有意义——前驱的
			// 改动已经在 default_branch 里了。什么都不做，让 base_ref
			// 保持 nil，后继走 RepoConfig.BaseBranch(kind) 的默认逻辑，
			// 直接从已经包含前驱改动的 default_branch 分叉，效果正确且
			// 更简单（不用等回收、不用管分支还在不在）。
			slog.Info("前驱已合并，跳过 base_ref 填充，后继走默认分支", "task", tk.ID, "depends_on", *tk.DependsOn)
		} else if predecessor.BranchName != nil {
			if err := q.tasks.SetBaseRef(ctx, tk.ID, predecessor.BranchName); err != nil {
				slog.Error("写入 base_ref 失败", "task", tk.ID, "err", err)
			} else {
				tk.BaseRef = predecessor.BranchName
			}
		}
	}

	if tk.BaseRef != nil {
		repoCfg.BaseRefOverride = *tk.BaseRef
	}
}

// lastEventString 从最近到最早扫描事件流，返回第一个 payload 里带有
// key 的值；找不到返回空串。用于从事件流回读原本靠内存 job 结构体
// 传递的调度决策依据（见 runOneClaimed 的注释）。
func lastEventString(events []task.Event, key string) string {
	for i := len(events) - 1; i >= 0; i-- {
		if v, ok := events[i].Payload[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

// planRetry 体检任务现场并产出重试计划（智能重试的派发侧入口）。
//
// 决策是纯逻辑（runner.PlanRetry），这里负责凑齐输入：worktree 体检、
// 失败阶段、会话凭据、崩溃恢复的中断状态。mode 与 interruptedState
// 由 runOneClaimed 从事件流回读得到（见其注释）。
func (q *queue) planRetry(ctx context.Context, tk *task.Task, repoCfg runner.RepoConfig, mode string, interruptedState task.State) runner.RetryPlan {
	m := runner.RetryMode(mode)
	if !m.Valid() {
		m = runner.RetryAuto
	}

	path, branch := "", ""
	if tk.WorktreePath != nil {
		path = *tk.WorktreePath
	}
	if tk.BranchName != nil {
		branch = *tk.BranchName
	}

	var wtState *runner.WorktreeState
	if path != "" {
		kind := runner.KindFix
		if tk.TaskKind != nil && *tk.TaskKind != "" {
			kind = runner.TaskKind(*tk.TaskKind)
		}
		base, err := repoCfg.BaseBranch(kind)
		if err != nil {
			base = repoCfg.DefaultBranch
		}
		wtState = q.worktrees.Inspect(ctx, repoCfg.ProviderRepo, path, branch, base)
	}

	var stage runner.Stage
	if tk.FailureStage != nil {
		stage = runner.Stage(*tk.FailureStage)
	}

	plan := runner.PlanRetry(m, runner.RetryInput{
		Stage:            stage,
		InterruptedState: interruptedState,
		HasSession:       tk.AgentSessionID != nil && *tk.AgentSessionID != "",
		WT:               wtState,
	})

	// 强制续跑（resume）但现场在 API 预检后的窗口里失效了：人明确说了
	// 要续，静默重建违背意图；但任务已回到 queued，没有合法边能再判死。
	// 降级为重建并把违背意图这件事写进决策理由，事件流里可见。
	if !plan.Fresh && plan.Entry == runner.EntryTriage && m == runner.RetryResume {
		plan = runner.RetryPlan{
			Fresh:   true,
			Entry:   runner.EntryTriage,
			Reasons: append(plan.Reasons, "现场在派发前失效，降级为从头重建"),
		}
	}
	return plan
}

// resolveRepoID 查出属主名下要用的仓库 id：第一条配置。
//
// 数据隔离（P1.5 第二步）后，每个用户各自在设置页登记仓库；按 issue 的
// team/project 路由到不同仓库仍是后续项（docs/02-design.md §8）。
func (q *queue) resolveRepoID(ctx context.Context, ownerUserID int64) (int64, error) {
	var repoID int64
	err := q.store.Pool().QueryRow(ctx,
		`SELECT id FROM repos WHERE user_id = $1 ORDER BY id LIMIT 1`, ownerUserID,
	).Scan(&repoID)
	if err != nil {
		return 0, fmt.Errorf("你的账号下没有仓库配置（请在设置页添加仓库）: %w", err)
	}
	return repoID, nil
}

// loadRepoConfig 按 repo id 读出分支策略配置，供派发时构造
// runner.ExecuteParams。任务行创建时已经把 repo_id 钉死在了那一刻的
// 仓库配置上，因此这里按 id 查，不再按属主查——就算属主事后又加了别的
// 仓库，也不会影响已经在跑/排队的任务该用哪份配置。
func (q *queue) loadRepoConfig(ctx context.Context, repoID int64) (cfg runner.RepoConfig, cloneURL string, err error) {
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
		SELECT provider_repo, default_branch, hotfix_base, protected_branches, branch_pattern,
		       COALESCE(verify_tier_override, ''), exclude_dirs
		FROM repos WHERE id = $1`, repoID,
	).Scan(&providerRepo, &defaultBranch, &hotfixBase, &protected, &pattern, &tierOverride, &excludeDirs)
	if err != nil {
		return cfg, "", fmt.Errorf("读取仓库配置失败（repo_id=%d）: %w", repoID, err)
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
	return cfg, "git@github.com:" + providerRepo + ".git", nil
}

// commentUnresolved 在无法接单时尽力回帖说明原因。
//
// 多用户之后这一步更常见：新用户配好了 webhook 却还没登记仓库，
// 指派事件照样投递。不回帖的话 Linear 那边看起来就是「指派了但
// 毫无反应」，比明确的拒绝难受得多。
func (q *queue) commentUnresolved(ctx context.Context, ownerUserID int64, issueID, issueKey string, cause error) {
	if q.clients == nil {
		return
	}
	clients, err := q.clients.ForUser(ctx, ownerUserID)
	if err != nil {
		return
	}
	lin, err := clients.Linear(ctx)
	if err != nil {
		return // 凭据也没配 —— 无从回帖，日志已留痕
	}
	body := "Lathe 无法接单：" + cause.Error() + "\n\n配置完成后重新指派即可触发。"
	if _, err := lin.Comment(ctx, issueID, body); err != nil {
		slog.Warn("接单失败回帖也没发出去", "issue", issueKey, "err", err)
	}
}
