package runner

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Clouditera/lathe/internal/task"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RepoLookup 按 repoID 查询该仓库的分支策略配置。
//
// runner 包不能反向依赖 cmd/lathe（那是 main 包）,因此不能直接复用
// cmd/lathe/queue.go 的 loadRepoConfig；NewRepoLookup 在本文件里单独
// 提供一份等价实现，供 cmd/lathe 组装 MergePoller 时传入。
type RepoLookup func(ctx context.Context, repoID int64) (RepoConfig, error)

// NewRepoLookup 构造一个直接查 repos 表的 RepoLookup。
//
// 查询字段与 cmd/lathe/queue.go 的 loadRepoConfig 保持一致（provider_repo/
// default_branch/hotfix_base/protected_branches/branch_pattern/
// verify_tier_override/exclude_dirs），但两处各自维护一份 SQL——runner
// 包不能导入 cmd/lathe，reverse 不成立。
func NewRepoLookup(pool *pgxpool.Pool) RepoLookup {
	return func(ctx context.Context, repoID int64) (RepoConfig, error) {
		var (
			cfg           RepoConfig
			providerRepo  string
			defaultBranch string
			hotfixBase    string
			protected     []string
			pattern       string
			tierOverride  string
			excludeDirs   []string
		)
		err := pool.QueryRow(ctx, `
			SELECT provider_repo, default_branch, hotfix_base, protected_branches, branch_pattern,
			       COALESCE(verify_tier_override, ''), exclude_dirs
			FROM repos WHERE id = $1`, repoID,
		).Scan(&providerRepo, &defaultBranch, &hotfixBase, &protected, &pattern, &tierOverride, &excludeDirs)
		if err != nil {
			return cfg, fmt.Errorf("runner: 读取仓库配置失败（repo_id=%d）: %w", repoID, err)
		}
		cfg = RepoConfig{
			ProviderRepo:       providerRepo,
			DefaultBranch:      defaultBranch,
			HotfixBase:         hotfixBase,
			ProtectedBranches:  protected,
			BranchPattern:      pattern,
			ExcludeDirs:        excludeDirs,
			VerifyTierOverride: tierOverride,
		}
		return cfg, nil
	}
}

// defaultMergePollInterval 是 F4.1-AC1（PR 合并后 5 分钟内转 merged）
// 的轮询间隔取值：45s 留足够裕量吸收单轮里若干个任务顺序查询 GitHub
// 的耗时，同时远小于 5 分钟的收敛上限。
const defaultMergePollInterval = 45 * time.Second

// MergePoller 是 F4.1 合并检测的轮询兜底，兼 F4.2 现场回收与
// F2.3-AC2（前驱 PR 被关闭未合并 → blocked_dep）的触发源。
//
// 纯轮询，没有 webhook 接收器：F4.1-AC3 要求"检测机制在 webhook 丢失
// 的情况下仍能收敛"——本实现干脆没有 webhook 这条主路径，轮询本身就是
// 唯一且常驻的机制，天然满足"仍能收敛"，不需要另外的对账逻辑。
type MergePoller struct {
	Tasks     *task.Machine
	Worktrees *WorktreeManager
	// ClientFactory 按任务属主解析 GitHub/Linear 客户端——PR 查询与
	// 阻塞回帖都要以任务属主的身份进行，与 Pipeline.setup 的凭据解析
	// 同一立场（多用户下不能借管理员账号跑别人的任务）。
	ClientFactory ClientFactory
	Notifier      Notifier
	// RepoLookup 按 repoID 查仓库配置（ProviderRepo 等）。
	RepoLookup RepoLookup
	// Pipeline 供 F4.3 rebase 跟进在 rebase+force push 成功后重验后继
	// 任务用（params.Retry = &RetryPlan{Fresh: false, Entry:
	// EntryVerify}，复用 stageVerify 全套逻辑，不重跑 agent）。
	Pipeline *Pipeline
	// Interval 是轮询间隔；<=0 时取 defaultMergePollInterval。
	Interval time.Duration

	// lastSeenHead 是 F4.4（前驱被改重验：PR 仍 open，但被 force-push
	// 改了内容）的检测状态——记录每个任务（key 是 task ID）上一轮观察
	// 到的 PR head commit SHA，本轮如果不一致就判定发生了 force-push。
	//
	// 已知限制（不在本次范围内解决）：这是纯进程内存态，不落库。之所
	// 以不做持久化，是因为持久化需要给 tasks 表新增一列记 last_seen_
	// head，这次改动明确不做数据库迁移变更。代价是 Lathe 进程重启后
	// 这份缓存会清空——重启后第一轮轮询会把当时观察到的 head 当成新
	// 基线记下来，不会因为"跟重启前记的旧基线不一致"而触发；如果恰好
	// 有一次 force-push 发生在"重启前最后一轮轮询之后、重启后第一轮
	// 轮询之前"这个窗口内，这一次 force-push 会被漏检一次（不会造成
	// 错误的 rebase，只是这一次不会被自动跟进；此后再发生 force-push
	// 仍会被正常检测到）。这是一个可接受的已知局限。
	lastSeenHead map[int64]string
	// lastSeenMu 保护 lastSeenHead。Run 目前是单 goroutine 定时轮询，
	// 理论上不需要加锁，但如果将来有人改成并发轮询这会是隐患——加锁
	// 是防御性的，成本很低。
	lastSeenMu sync.Mutex
}

// Run 定时轮询直到 ctx 结束。
func (p *MergePoller) Run(ctx context.Context) {
	interval := p.Interval
	if interval <= 0 {
		interval = defaultMergePollInterval
	}
	slog.Info("合并检测轮询已启动", "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("合并检测轮询停止")
			return
		case <-ticker.C:
			if err := p.pollOnce(ctx); err != nil {
				slog.Warn("合并检测轮询失败", "err", err)
			}
		}
	}
}

// pollOnce 跑一轮合并检测：遍历所有 pr_open 任务，逐个查询 PR 状态。
//
// 单个任务处理出错（GitHub API 失败等）只告警并继续处理下一个任务，
// 不能因为一个任务查询失败就让整轮轮询提前退出——那样会让排在它后面
// 的任务白等一轮，累积起来可能违反 F4.1-AC1 的 5 分钟收敛上限。
func (p *MergePoller) pollOnce(ctx context.Context) error {
	tasks, err := p.Tasks.ListOpenPRTasks(ctx)
	if err != nil {
		return fmt.Errorf("runner: 查询 pr_open 任务失败: %w", err)
	}
	for _, tk := range tasks {
		if err := p.pollTask(ctx, tk); err != nil {
			slog.Warn("合并检测处理任务失败", "task", tk.ID, "pr_number", derefInt(tk.PRNumber), "err", err)
		}
	}
	return nil
}

// pollTask 查询单个任务对应 PR 的当前状态并据此推进状态机。
func (p *MergePoller) pollTask(ctx context.Context, tk *task.Task) error {
	if tk.PRNumber == nil {
		return nil // ListOpenPRTasks 已经按 pr_number IS NOT NULL 过滤，这里是防御性判断
	}

	clients, err := p.ClientFactory.ForUser(ctx, tk.UserID)
	if err != nil {
		return fmt.Errorf("解析任务属主客户端失败: %w", err)
	}
	gh, err := clients.GitHub(ctx)
	if err != nil {
		return fmt.Errorf("获取 GitHub 客户端失败: %w", err)
	}
	repoCfg, err := p.RepoLookup(ctx, tk.RepoID)
	if err != nil {
		return fmt.Errorf("查询仓库配置失败: %w", err)
	}

	info, err := gh.GetPRInfo(ctx, repoCfg.ProviderRepo, *tk.PRNumber)
	if err != nil {
		return fmt.Errorf("查询 PR #%d 状态失败: %w", *tk.PRNumber, err)
	}

	switch {
	case info.Merged:
		return p.handleMerged(ctx, tk)
	case info.State == "closed":
		// closed 且未合并：PR 被关闭未合并（F2.3-AC2 的触发源）
		return p.handleClosedUnmerged(ctx, tk, clients)
	case info.State == "open":
		// 仍是 open：正常情况——但 F4.4 要求进一步检查这个 open 的 PR
		// 有没有被 force-push 改过内容（前驱 PR 还开着，只是收到
		// review 意见后修改重推）。
		p.checkForcePush(ctx, tk, info.HeadSHA)
		return nil
	default:
		return nil // 未知状态（GitHub 理论上只有 open/closed），下一轮再看
	}
}

// checkForcePush 是 F4.4（前驱被改重验）的检测入口：把本轮观察到的
// head 与 lastSeenHead 缓存的上一轮记录比较，据此判定是否发生了
// force-push。
//
// 三种情形：
//  1. 从未记录过这个任务（第一次观察到它，通常是任务刚开出 PR 或
//     Lathe 进程刚启动）：只记录当前 head 当基线，不触发任何动作——
//     避免任务刚开 PR 就被误判成"变了"。
//  2. 记录过且与本轮一致：正常情况，不触发。
//  3. 记录过且与本轮不一致：发生了 force-push。前驱自己的分支名没有
//     变（只是内容变了），直接复用 F4.3 的 rebaseFollowup，把
//     oldBaseBranch 与 newBaseBranch 都填成前驱自己的分支名——这是它
//     与 F4.3 顶层调用（前驱刚合并，oldBaseBranch 是前驱分支、
//     newBaseBranch 是 default_branch）唯一的区别。rebaseFollowup 内部
//     的失败处理（冲突转 failed）与成功后的递归级联已经在 F4.3 阶段
//     做好，这里不需要重复处理；没有后继依赖这个分支时，
//     TasksWithBaseRef 自然查不到东西，是空操作。
//
// headSHA 为空串时直接跳过（既不记录也不判定）：真实 GitHub 的 PR
// head 不可能是空 SHA，出现空值只说明上游异常或测试未配置，用它当
// 基线或拿它跟基线比较都没有意义，还可能在异常自愈后被误判成一次
// force-push。
func (p *MergePoller) checkForcePush(ctx context.Context, tk *task.Task, headSHA string) {
	if headSHA == "" {
		return
	}

	p.lastSeenMu.Lock()
	if p.lastSeenHead == nil {
		p.lastSeenHead = make(map[int64]string)
	}
	oldHead, seen := p.lastSeenHead[tk.ID]
	p.lastSeenHead[tk.ID] = headSHA
	p.lastSeenMu.Unlock()

	if !seen || oldHead == headSHA {
		return
	}

	if tk.BranchName == nil || *tk.BranchName == "" {
		slog.Warn("检测到 PR head 变化但任务缺少 branch_name，跳过 F4.4 rebase 跟进（数据异常）",
			"task", tk.ID)
		return
	}

	slog.Info("检测到前驱 PR 被 force-push，触发后继 rebase 跟进（F4.4）",
		"task", tk.ID, "branch", *tk.BranchName, "old_head", oldHead, "new_head", headSHA)
	if err := p.rebaseFollowup(ctx, *tk.BranchName, oldHead, *tk.BranchName); err != nil {
		slog.Warn("F4.4 rebase 跟进失败", "task", tk.ID, "err", err)
	}
}

// handleMerged 把任务转到 merged（F4.1-AC1/AC2），随后交给 onMerged
// 处理合并后的跟进动作。
func (p *MergePoller) handleMerged(ctx context.Context, tk *task.Task) error {
	merged, err := p.Tasks.Transition(ctx, tk.ID, task.StateMerged, "system:merge-poll", &task.TransitionOpts{
		Payload: map[string]any{"pr_number": *tk.PRNumber},
	})
	if err != nil {
		return fmt.Errorf("转移到 merged 失败: %w", err)
	}
	slog.Info("任务已合并", "task", merged.ID, "pr_number", *tk.PRNumber)

	if err := p.onMerged(ctx, merged); err != nil {
		// 合并这个事实本身已经落库成功；跟进动作（现场回收等）失败
		// 不该反过来污染合并检测的主结果，只告警。
		slog.Warn("合并后跟进处理失败", "task", merged.ID, "err", err)
	}
	return nil
}

// onMerged 处理任务转入 merged 之后的跟进动作。
//
// F4.3（后继链自动跟进：rebase --onto <newBaseBranch> <旧基线 tip> +
// 重验）在【这一行之下、下面 HasLiveDependentOnBranch 调用之前】触发：
// rebase 级联完成后，直接后继任务的 base_ref 会被清空（转而指向
// default_branch），下面的 HasLiveDependentOnBranch 判定才能如实反映
// "这个分支现在是否还被活着的后继依赖"，回收现场的时机才对
// （F4.2-AC2：级联完成前不会误删）。
//
// 现场回收本身是 F4.2-AC1/AC2：只有在没有活着的后继依赖这个分支时才
// 回收 worktree（F4.2-AC2），且只回收工作目录，不删分支之外的东西——
// Worktrees.Remove(force=true) 本身会顺带删掉本地分支副本，远端分支
// 不受影响（Lathe 从不删除远端分支）。
func (p *MergePoller) onMerged(ctx context.Context, merged *task.Task) error {
	if merged.BranchName == nil || *merged.BranchName == "" ||
		merged.WorktreePath == nil || *merged.WorktreePath == "" {
		return nil // 没有可回收的现场（字段缺失：老数据或异常路径）
	}

	repoCfg, err := p.RepoLookup(ctx, merged.RepoID)
	if err != nil {
		return fmt.Errorf("查询仓库配置失败: %w", err)
	}

	// F4.3：oldBaseTip 必须在任何改写发生之前捕获——顶层入口这里，
	// "改写"指的是下面即将对直接后继做的 rebase，merged 分支自己此时
	// 还没被任何人动过，直接解析它在镜像里的当前 tip 即可。
	if oldTip, terr := p.resolveMirrorTip(ctx, repoCfg, *merged.BranchName); terr != nil {
		slog.Warn("解析已合并分支当前 tip 失败，跳过 rebase 跟进",
			"task", merged.ID, "branch", *merged.BranchName, "err", terr)
	} else if ferr := p.rebaseFollowup(ctx, *merged.BranchName, oldTip, repoCfg.DefaultBranch); ferr != nil {
		slog.Warn("rebase 跟进失败", "task", merged.ID, "err", ferr)
	}

	hasLive, err := p.Tasks.HasLiveDependentOnBranch(ctx, *merged.BranchName)
	if err != nil {
		return fmt.Errorf("查询分支 %q 的活依赖失败: %w", *merged.BranchName, err)
	}
	if hasLive {
		slog.Info("分支仍被未合并后继依赖，暂不回收现场（F4.2-AC2）",
			"task", merged.ID, "branch", *merged.BranchName)
		return nil
	}

	wt := &Worktree{
		Path:   *merged.WorktreePath,
		Branch: *merged.BranchName,
		Mirror: p.Worktrees.MirrorPath(repoCfg.ProviderRepo),
	}
	if err := p.Worktrees.Remove(ctx, wt, true); err != nil {
		return fmt.Errorf("回收工作区失败: %w", err)
	}
	slog.Info("任务合并后现场已回收（F4.2-AC1）", "task", merged.ID, "path", wt.Path, "branch", wt.Branch)
	return nil
}

// resolveMirrorTip 解析某分支在仓库镜像里的当前 tip commit。
//
// 先刷新镜像（只 fetch，不重新 clone；EnsureMirror 首次调用才会 clone，
// 此时镜像必然早就存在），再按镜像命名空间（refs/remotes/origin/*）
// 解析，避免与 refs/heads/* 下可能存在的同名残留分支产生歧义——
// 与 ChangedFiles/HasCommitsAhead 解析基线的立场一致。
func (p *MergePoller) resolveMirrorTip(ctx context.Context, repoCfg RepoConfig, branch string) (string, error) {
	cloneURL := cloneURLFor(repoCfg.ProviderRepo)
	mirror, err := p.Worktrees.EnsureMirror(ctx, repoCfg.ProviderRepo, cloneURL)
	if err != nil {
		return "", fmt.Errorf("刷新 mirror 失败: %w", err)
	}
	out, err := p.Worktrees.git(ctx, mirror, "rev-parse", "--verify", "--quiet", MirrorBaseRef(branch)+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("解析分支 %q 当前 tip 失败: %w", branch, err)
	}
	return strings.TrimSpace(out), nil
}

// cloneURLFor 按 provider repo 拼 SSH clone URL，与 cmd/lathe/queue.go
// 的 loadRepoConfig 用的是同一套约定（"git@github.com:<repo>.git"）；
// runner 包不能反向依赖 cmd/lathe，因此这里单独拼一份，跟 RepoLookup
// 的 NewRepoLookup 是同一类"各自维护一份等价实现"的理由。
func cloneURLFor(providerRepo string) string {
	return "git@github.com:" + providerRepo + ".git"
}

// rebaseFollowup 是 F4.3 后继链自动跟进的统一入口，处理一次"分支历史
// 被改写"事件对其直接后继的连锁影响。
//
// oldBaseBranch 是这次要被替换掉的旧基线的分支名；oldBaseTip 是这个旧
// 基线在被替换之前的 commit（必须由调用方在任何改写发生之前捕获，本
// 函数不负责捕获）；newBaseBranch 是新基线的分支名。顶层调用（前驱刚
// 合并）里 oldBaseBranch 是前驱分支、newBaseBranch 是 default_branch；
// 级联到下一层时 oldBaseBranch 与 newBaseBranch 都是刚 rebase 完的
// 前驱自己的分支名（分支名没变，只是内容变了）。
//
// 找出 base_ref 当前等于 oldBaseBranch 且状态非终结的任务——这些就是
// 直接后继。正常业务流程下只会有一个，但不假设恰好一个：逐个处理，
// 互不影响，一个失败不影响其它任务的处理。
func (p *MergePoller) rebaseFollowup(ctx context.Context, oldBaseBranch, oldBaseTip, newBaseBranch string) error {
	succs, err := p.Tasks.TasksWithBaseRef(ctx, oldBaseBranch)
	if err != nil {
		return fmt.Errorf("查询分支 %q 的直接后继失败: %w", oldBaseBranch, err)
	}
	for _, succ := range succs {
		p.rebaseFollowupOne(ctx, succ, oldBaseTip, newBaseBranch)
	}
	return nil
}

// rebaseFollowupOne 处理 rebaseFollowup 里单个直接后继任务的完整流程：
// rebase → force push → 落 base_ref → 重验 → （仅重验后到达 pr_open 时）
// 递归级联到它自己的后继。任何一步失败都只影响这一个任务，不返回
// error 给调用方（调用方按"逐个处理，互不影响"的语义，只需要遍历完
// 所有直接后继）。
func (p *MergePoller) rebaseFollowupOne(ctx context.Context, succ *task.Task, oldBaseTip, newBaseBranch string) {
	if succ.WorktreePath == nil || *succ.WorktreePath == "" {
		// 还没派发创建 worktree（仍在 queued）：base_ref 的值——分支
		// 名——没有变化，只是那个分支的内容变了，succ 将来被派发创建
		// worktree 时会自然基于这个分支【当时】的最新内容分叉，天然
		// 正确，不需要 rebase。
		slog.Info("后继任务尚未开始，rebase 跟进无需动作（分支名未变，内容将来自然拿到最新）",
			"task", succ.ID, "branch", newBaseBranch)
		return
	}
	if succ.BranchName == nil || *succ.BranchName == "" {
		slog.Warn("后继任务已有 worktree_path 却没有 branch_name，跳过 rebase 跟进（数据异常）",
			"task", succ.ID, "worktree_path", *succ.WorktreePath)
		return
	}

	repoCfg, err := p.RepoLookup(ctx, succ.RepoID)
	if err != nil {
		slog.Warn("查询后继任务仓库配置失败，跳过 rebase 跟进", "task", succ.ID, "err", err)
		return
	}

	wt := &Worktree{
		Path:       *succ.WorktreePath,
		Branch:     *succ.BranchName,
		BaseBranch: newBaseBranch,
		Mirror:     p.Worktrees.MirrorPath(repoCfg.ProviderRepo),
	}

	// 必须在做任何改动之前捕获：这是下一级递归要用的 oldBaseTip——
	// rebase 一旦跑完，succ 自己这条分支原来的 HEAD 就再也读不到了。
	tipOut, err := p.Worktrees.git(ctx, wt.Path, "rev-parse", "HEAD")
	if err != nil {
		slog.Warn("捕获后继任务当前 HEAD 失败，跳过 rebase 跟进", "task", succ.ID, "err", err)
		return
	}
	succThisOldTip := strings.TrimSpace(tipOut)

	if err := p.Worktrees.RebaseOnto(ctx, wt, oldBaseTip, newBaseBranch); err != nil {
		p.failRebaseFollowup(ctx, succ, "自动 rebase", err)
		return
	}

	if err := p.Worktrees.ForcePush(ctx, wt, repoCfg, nil); err != nil {
		p.failRebaseFollowup(ctx, succ, "推送改写后的分支（force-with-lease）", err)
		return
	}

	// base_ref 非空才走 override，为空走 RepoConfig.BaseBranch(kind)
	// 默认逻辑（F3.1）：rebase 到 default_branch 的清空 base_ref；
	// rebase 到另一个未合并前驱分支（级联的中间层）的，succ 逻辑上仍
	// 栈在 newBaseBranch 上面，要把 base_ref 更新成 newBaseBranch，
	// 不能清空。
	if newBaseBranch == repoCfg.DefaultBranch {
		if err := p.Tasks.SetBaseRef(ctx, succ.ID, nil); err != nil {
			slog.Warn("清空后继任务 base_ref 失败", "task", succ.ID, "err", err)
		}
	} else {
		nb := newBaseBranch
		if err := p.Tasks.SetBaseRef(ctx, succ.ID, &nb); err != nil {
			slog.Warn("更新后继任务 base_ref 失败", "task", succ.ID, "err", err)
		}
	}

	// 重验不重跑 agent（F4.3-AC3）：复用 stageVerify 全套逻辑（含修复
	// 回路、红绿判定），结果自然落库（pr_open 或 failed）。执行本身的
	// Repo 要带上 BaseRefOverride=newBaseBranch，diff/PR base 才会算对
	// ——rc.wt.BaseBranch 就是从 params.Repo.BaseBranch(kind) 来的。
	//
	// stageVerify 内部会把任务转移到 StateVerifying，而这条边只能从
	// queued 或 implementing 出发（task/state.go 的合法转移表，本阶段
	// 不改它）。succ 此刻可能正停在 pr_open/review_feedback/verifying
	// ——这些都是"已经有 worktree"之后完全正常的现实状态（算法说明里
	// 明确列出的"正在实现/验证/已开 PR/review 中"），先归一到合法起点，
	// 全程只走 state.go 里已经存在的边，不新增规则。
	if _, err := p.reenterForVerify(ctx, succ); err != nil {
		slog.Warn("重验前状态归一失败，跳过重验（rebase 与推送已完成，base_ref 已按上面的规则更新）",
			"task", succ.ID, "err", err)
		return
	}

	execRepo := repoCfg
	execRepo.BaseRefOverride = newBaseBranch
	if err := p.Pipeline.Execute(ctx, ExecuteParams{
		TaskID:   succ.ID,
		Repo:     execRepo,
		CloneURL: cloneURLFor(repoCfg.ProviderRepo),
		IssueID:  deref(succ.LinearIssueID),
		Actor:    "system:rebase-followup",
		Retry:    &RetryPlan{Fresh: false, Entry: EntryVerify},
	}); err != nil {
		// 重验失败本身已经被 stageVerify 自己的失败路径处理好了（正常
		// 的 StateFailed），这里只告警继续，不能因为一个任务重验失败
		// 就中断整个 rebaseFollowup 调用。
		slog.Warn("rebase 跟进后重验失败", "task", succ.ID, "err", err)
		return
	}

	// 只有 succ 重验后成功到达 pr_open 才递归（F4.3-AC5：链式跟进按
	// 拓扑序逐级进行，这一级没完成就不该碰下一级）。
	latest, err := p.Tasks.Get(ctx, succ.ID)
	if err != nil {
		slog.Warn("重验后查询后继任务状态失败，不递归到下一级", "task", succ.ID, "err", err)
		return
	}
	if latest.State != task.StatePROpen {
		slog.Info("后继任务重验后未到达 pr_open，不递归到下一级",
			"task", succ.ID, "state", latest.State)
		return
	}

	branch := deref(succ.BranchName)
	if err := p.rebaseFollowup(ctx, branch, succThisOldTip, branch); err != nil {
		slog.Warn("级联 rebase 跟进失败", "task", succ.ID, "err", err)
	}
}

// reenterForVerify 把 succ 归一到 stageVerify 能合法从其转移到
// StateVerifying 的起点（queued 或 implementing），全程只走
// task/state.go 里已经存在的合法转移边，不新增、不改任何转移规则：
//
//	pr_open         → review_feedback → implementing
//	review_feedback → implementing（RequiresSession 要求已有
//	                  agent_session_id——succ 走过一次真实实现，早就有）
//	verifying       → queued
//	queued/implementing 本身已经站在合法起点上，原样跳过
//
// succ 已经有 worktree 之后，"实现/验证/已开 PR/review 中"是完全正常
// 会出现的现实状态（F4.3 算法说明原文列出的四种）：前驱刚合并时，
// 后继可能早就顺利开出了 PR（最常见）、也可能还卡在 review 意见的
// 二轮里——都需要在重验前归一，否则 stageVerify 自己那次
// Transition(..., StateVerifying, ...) 会因为非法转移直接报错。
func (p *MergePoller) reenterForVerify(ctx context.Context, succ *task.Task) (*task.Task, error) {
	cur := succ
	if cur.State == task.StatePROpen {
		tk, err := p.Tasks.Transition(ctx, cur.ID, task.StateReviewFeedback, "system:rebase-followup",
			&task.TransitionOpts{Payload: map[string]any{"reason": "rebase_followup_reverify"}})
		if err != nil {
			return nil, fmt.Errorf("状态归一失败（pr_open→review_feedback）: %w", err)
		}
		cur = tk
	}
	if cur.State == task.StateReviewFeedback {
		tk, err := p.Tasks.Transition(ctx, cur.ID, task.StateImplementing, "system:rebase-followup",
			&task.TransitionOpts{Payload: map[string]any{"reason": "rebase_followup_reverify"}})
		if err != nil {
			return nil, fmt.Errorf("状态归一失败（review_feedback→implementing）: %w", err)
		}
		cur = tk
	}
	if cur.State == task.StateVerifying {
		tk, err := p.Tasks.Transition(ctx, cur.ID, task.StateQueued, "system:rebase-followup",
			&task.TransitionOpts{Payload: map[string]any{"reason": "rebase_followup_reverify"}})
		if err != nil {
			return nil, fmt.Errorf("状态归一失败（verifying→queued）: %w", err)
		}
		cur = tk
	}
	return cur, nil
}

// failRebaseFollowup 是 rebase 跟进链路里"任务转 failed"的专用路径：
// 这是合并检测触发的跟进动作，没有 runCtx，调不了 Pipeline.fail()，
// 这里照抄它做的事——回帖说明 + 保留现场（不删 worktree）+ 推送通知 +
// 转 StateFailed + 失败传播，机器码用 StageRebaseConflict。
//
// 调用方处理完这个失败后必须停止：不清空 base_ref、不重验、不级联到
// 更深层的后继——这个任务需要人工介入解决冲突。
func (p *MergePoller) failRebaseFollowup(ctx context.Context, succ *task.Task, action string, cause error) {
	reason := fmt.Sprintf("%s失败: %v", action, cause)
	slog.Error("rebase 跟进失败，任务转 failed", "task", succ.ID, "action", action, "err", cause)

	body := fmt.Sprintf(
		"**Lathe 自动 rebase 失败**\n\n前驱分支已更新，本任务需要跟进 rebase，但%s失败，请人工处理冲突。\n\n```\n%s\n```\n",
		action, truncate(cause.Error(), 3000))
	if succ.WorktreePath != nil && succ.BranchName != nil {
		body += fmt.Sprintf("\n工作区已保留在 `%s`（分支 `%s`），可直接进去接手解决冲突后手动推送、开 PR。\n",
			*succ.WorktreePath, *succ.BranchName)
	}

	// clients/haveClients 在下面失败传播（第 5 步）给 blocked_dep 后继
	// 回帖时复用——与 pipeline.go 的 fail() 一致：blocked 后继理应与
	// succ 同属主，用同一份已解析好的客户端即可，不需要按 bt.UserID
	// 再解析一次。
	var clients Clients
	var haveClients bool
	if p.ClientFactory != nil {
		if c, err := p.ClientFactory.ForUser(ctx, succ.UserID); err != nil {
			slog.Warn("解析任务属主客户端失败，跳过失败回帖", "task", succ.ID, "err", err)
		} else {
			clients = c
			haveClients = true
			if succ.LinearIssueID != nil && *succ.LinearIssueID != "" {
				if lin, err := clients.Linear(ctx); err != nil {
					slog.Warn("获取 Linear 客户端失败，跳过失败回帖", "task", succ.ID, "err", err)
				} else if _, err := lin.Comment(ctx, *succ.LinearIssueID, body); err != nil {
					slog.Warn("失败回帖失败", "task", succ.ID, "err", err)
				}
			}
		}
	}

	if p.Notifier != nil {
		msg := fmt.Sprintf("Lathe 任务 %d 自动 rebase 失败（%s），需要人工处理冲突", succ.ID, action)
		if err := p.Notifier.Notify(ctx, msg); err != nil {
			slog.Warn("推送通知失败", "task", succ.ID, "err", err)
		}
	}

	if _, err := p.Tasks.Transition(ctx, succ.ID, task.StateFailed, "system:rebase-followup", &task.TransitionOpts{
		FailureReason: &reason,
		FailureStage:  strPtr(string(StageRebaseConflict)),
		Payload:       map[string]any{"stage": string(StageRebaseConflict)},
	}); err != nil {
		slog.Error("rebase 跟进失败后转 failed 也失败", "task", succ.ID, "err", err)
		return
	}

	// 5) 失败传播（F2.3-AC1~AC4，与 pipeline.go 的 fail() 一致）：
	// depends_on 链上所有传递后继里仍排队的任务转 blocked_dep，并回帖
	// 说明是被 succ 的 rebase 冲突连累的。这是本函数相对 fail() 之前
	// 唯一漏掉的一步——没有这一步，链路更深处仍处于 queued 的后继会
	// 永远静默卡在 queued，不满足 ClaimReady 的就绪判定，也不会转
	// blocked_dep（F2.3-AC1 明确禁止的情形）。传播出错或某个回帖失败
	// 都只告警，不影响 failRebaseFollowup 本身没有返回值的语义。
	blocked, err := p.Tasks.PropagateBlocked(ctx, succ.ID, reason)
	if err != nil {
		slog.Warn("失败传播失败", "task", succ.ID, "err", err)
		return
	}
	if len(blocked) == 0 || !haveClients {
		return
	}
	lin, err := clients.Linear(ctx)
	if err != nil {
		slog.Warn("获取 Linear 客户端失败，阻塞回帖跳过", "task", succ.ID, "err", err)
		return
	}
	for _, bt := range blocked {
		if bt.UserID != succ.UserID {
			// 当前是单用户/管理员凭据模型，同一 flow 下的任务理应同属主；
			// 出现不一致说明建图或数据有问题，先告警观察，不静默假设
			// （与 pipeline.go 的 fail() 一致）。
			slog.Warn("阻塞传播发现跨属主后继", "task", succ.ID, "taskOwner", succ.UserID,
				"blockedTask", bt.ID, "blockedOwner", bt.UserID)
		}
		if bt.LinearIssueID == nil || *bt.LinearIssueID == "" {
			continue
		}
		blockedBody := fmt.Sprintf(
			"**Lathe 已阻塞**\n\n前驱任务 #%d（issue `%s`）因自动 rebase 冲突失败，本任务因依赖它而被阻塞（blocked_dep），"+
				"等前驱恢复（人工处理冲突后重试）后会自动回到排队。\n\n前驱失败原因：\n```\n%s\n```\n",
			succ.ID, succ.LinearIssueKey, truncate(reason, 1000))
		if _, cerr := lin.Comment(ctx, *bt.LinearIssueID, blockedBody); cerr != nil {
			slog.Warn("阻塞回帖失败", "task", bt.ID, "err", cerr)
		}
	}
}

// handleClosedUnmerged 处理"PR 被关闭但未合并"：任务本身转 cancelled，
// 并按 F2.3-AC2 把这件事作为触发源，走既有的 PropagateBlocked 传播给
// 所有传递后继，逐个回帖说明。
//
// 回帖风格抄自 pipeline.go 的 fail()：同一 flow 下的任务理应同属主，
// 出现不一致只告警观察，不静默假设。
func (p *MergePoller) handleClosedUnmerged(ctx context.Context, tk *task.Task, clients Clients) error {
	reason := fmt.Sprintf("PR #%d 已被关闭但未合并", *tk.PRNumber)

	if _, err := p.Tasks.Transition(ctx, tk.ID, task.StateCancelled, "system:merge-poll", &task.TransitionOpts{
		FailureReason: &reason,
		Payload:       map[string]any{"pr_number": *tk.PRNumber, "reason": reason},
	}); err != nil {
		return fmt.Errorf("转移到 cancelled 失败: %w", err)
	}
	slog.Info("PR 被关闭未合并，任务已转 cancelled", "task", tk.ID, "pr_number", *tk.PRNumber)

	blocked, err := p.Tasks.PropagateBlocked(ctx, tk.ID, reason)
	if err != nil {
		slog.Warn("阻塞传播失败", "task", tk.ID, "err", err)
		return nil
	}
	if len(blocked) == 0 {
		return nil
	}

	lin, err := clients.Linear(ctx)
	if err != nil {
		slog.Warn("获取 Linear 客户端失败，阻塞回帖跳过", "task", tk.ID, "err", err)
		return nil
	}
	for _, bt := range blocked {
		if bt.UserID != tk.UserID {
			// 当前是单用户/管理员凭据模型，同一 flow 下的任务理应同属主；
			// 出现不一致说明建图或数据有问题，先告警观察，不静默假设
			// （与 pipeline.go 的 fail() 一致）。
			slog.Warn("阻塞传播发现跨属主后继", "task", tk.ID, "taskOwner", tk.UserID,
				"blockedTask", bt.ID, "blockedOwner", bt.UserID)
		}
		if bt.LinearIssueID == nil || *bt.LinearIssueID == "" {
			continue
		}
		body := fmt.Sprintf(
			"**Lathe 已阻塞**\n\n前驱任务 #%d（issue `%s`）的 PR 被关闭但未合并，本任务因依赖它而被阻塞（blocked_dep），"+
				"等前驱恢复（重新开出 PR 或人工处理）后会自动回到排队。\n\n原因：\n```\n%s\n```\n",
			tk.ID, tk.LinearIssueKey, reason)
		if _, cerr := lin.Comment(ctx, *bt.LinearIssueID, body); cerr != nil {
			slog.Warn("阻塞回帖失败", "task", bt.ID, "err", cerr)
		}
	}
	return nil
}

func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}
