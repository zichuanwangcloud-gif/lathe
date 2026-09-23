package runner

// reaper.go worktree TTL 收割机（docs/08-debt-cleanup.md T6）。
//
// 问题：D4「失败保留现场」让人能进去接手，但**从来没有回收策略**。
// 现有的两个回收触发点都是被动的：
//
//   - mergepoll 的 onMerged：合并后回收（只覆盖 merged）
//   - WorktreeManager.Create 的尸体回收：同 issue 下一次尝试需要这个槽位时
//
// 失败与取消的现场因此只增不减。`/opt/lathe/workspaces/` 里躺着 9 个
// `cr-*` 目录就是证据。store/users.go 的 WorktreePaths 更是一份自白：
// 删账号时它只把路径**打进日志让人手工回收**，注释写着「数据库的行会被
// 外键级联带走，磁盘上的目录不会」。
//
// 顺带修一处文档错：02-design §8 的 P0 那行声称「worktree 自动回收」
// 已交付，实际只有上面那两个被动触发点。
//
// ---------------------------------------------------------------------------
// 不查就会误删的地方
// ---------------------------------------------------------------------------
//
//  1. **只碰终态任务。** failed 可以转回 queued —— 人随时可能重试续跑，
//     而 D4 保留现场的全部目的就是让人能接手。所以 TTL 默认值必须保守
//     （天级），激进的 TTL 会把人正要重试的现场删掉。
//
//  2. **删盘前必须先原子认领。** 候选列表是一次查询的结果，而候选与
//     真正删盘之间隔着若干条 git 命令（gitTimeout 15 分钟）。这段时间里
//     人点一下重试，任务就回到 queued、pipeline 复用现场续跑 —— 收割机
//     随后把这个正在使用的现场删掉。所以顺序是：ClaimForReap（一条带
//     state/路径/updated_at 守卫的 CAS）成功，才允许碰磁盘。
//
//  3. **路径还要判「活任务认领」。** 目录名只由 issue key 决定，同 issue
//     的不同 repo / 不同用户可以是两个合法活任务、共用一个目录；而老的
//     failed 行不受 tasks_one_active_per_issue 约束。认领的是**任务行**，
//     不代表这条路径上没有别的活任务 —— 两个判据都要。
//
//  4. **绝不碰 `.` 开头的目录。** workspaces 下有 `.mirrors/`（所有仓库的
//     bare mirror）与 `.verify/`（heavy 档基线工作区）。误删 `.mirrors/`
//     等于把所有仓库的镜像清了，下一个任务要重新 clone。
//     注意 preview.Discover 的跳过列表**不含** `.` 前缀通配，别照抄它。
//
//  5. **删分支前查 HasLiveDependentOnBranch。** 栈式 PR 的后继可能还在把
//     这个分支当 base（F4.2-AC2）。不查就会把别人的 base 删掉，
//     后继再也 rebase 不上去。
//
//  6. **脏现场与未推送的分支只告警不删。** 主路径此前不看工作区是否
//     dirty 就 `worktree remove --force`——那正是 Remove(force=false) 的
//     注释里写明要保护的东西（D4）。孤儿清扫那侧反而看了 mtime，两条
//     路径的安全标准不能不对称。
//
//  7. **用 Discard 而不是 Remove。** 尸体可能是残缺的（目录被手工删过、
//     分支已不存在），Remove 在这种情形下会报错，而 Discard 尽力清理
//     每一步并 prune 兜底。
//
//  8. **`worktree_path` 置空要走专门语句。** Transition 的 UPDATE 是
//     COALESCE 语义（只增不清空），传 nil 表示「这次不改」。
//     回收一律走 task.Machine.ClaimForReap —— 它把状态判定与置空放进同一条
//     带守卫的 UPDATE（state/路径/updated_at/未被认领），杜绝「基于过期快照
//     动手」。刻意没有留无守卫的置空原语：那种 API 迟早会被人图省事调用。
//
//  9. **孤儿目录的 TTL 不只是策略，是安全机制。** 主回收路径由数据库驱动，
//     看不见「磁盘上有、没有任务行指向」的目录（实测就有：任务 649 是
//     failed 但 worktree_path 为 NULL，cr-1367 那个目录因此永远没人回收）。
//     补的清扫按 mtime 判超期 —— 而 worktree 目录是先被 Create 建出来、
//     之后才在转入 implementing 时把路径写进任务行的，两步之间目录无人认领。
//     不看 mtime 就删，正在跑的任务会突然找不到自己的工作区。这也是
//     config.MinWorktreeTTL 存在的原因：TTL 是这条安全机制的一半。

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

const (
	// defaultReapTTL 是现场保留时长的默认值。
	//
	// 三天而非更短：failed 能转回 queued，人下班前看到失败、第二天上班
	// 接手是完全正常的节奏。写成 72*time.Hour 而不是 "72h" 字面量是因为
	// time.ParseDuration **不支持 d 单位** —— 配置里要写 72h，不能写 3d。
	defaultReapTTL = 72 * time.Hour
	// defaultReapInterval 是扫描间隔。回收是低频维护动作，
	// 一小时一次足够，没必要跟 mergepoll 一样 45 秒。
	defaultReapInterval = time.Hour

	// minReapTTL 是收割机自身的时间下限，与 config.MinWorktreeTTL 同一个
	// 理由（配置层已拒绝更小的值，这里再兜一次：WorktreeReaper 也能被
	// 测试或别的调用方直接构造，绕过 config）。
	minReapTTL = time.Hour
)

// ReaperTasks 是收割机需要的任务查询与记账。
//
// 声明窄接口而不是直接吃 *task.Machine：测试要能注入假件，
// 而 Machine 的方法集远大于这里需要的几个（沿用本包
// VerificationRecorder / TaskMail 的做法）。
type ReaperTasks interface {
	// ListReapableTasks 返回终态、留现场、且 updated_at 早于 olderThan 的任务。
	ListReapableTasks(ctx context.Context, olderThan time.Time) ([]*task.Task, error)
	// HasLiveDependentOnBranch 报告是否有非终态任务把这个分支当 base。
	HasLiveDependentOnBranch(ctx context.Context, branchName string) (bool, error)
	// ReferencedWorktreePaths 返回**任何**任务行认领的路径（判孤儿用）。
	ReferencedWorktreePaths(ctx context.Context) ([]string, error)
	// ClaimedWorktreePaths 返回**非终态**任务认领的路径（删盘前的最后一道闸）。
	ClaimedWorktreePaths(ctx context.Context) ([]string, error)
	// ClaimForReap 校验（并可选落账）一次现场认领。四个守卫见实现注释；
	// 返回 false 表示这一次收割对该任务行无效，**必须跳过磁盘操作**。
	// commit == false 时只校验不改任何状态；reason 只在 commit 阶段
	// 写进任务事件流。
	ClaimForReap(ctx context.Context, id int64, path string, snapshotUpdatedAt time.Time, commit bool, actor string, reason map[string]any) (bool, error)
}

// ReapOutcome 是一次收割尝试的结局，用于统计与日志。
type ReapOutcome int

const (
	// ReapSkipped 没动磁盘（认领失败、路径可疑、被活任务占用、还没到期、
	// 干跑模式等），也不算回收。
	ReapSkipped ReapOutcome = iota
	// ReapRemoved 真的删掉了目录（或分支），计入回收数。
	ReapRemoved
	// ReapHeld 校验通过但出于安全考虑保留了现场（脏工作区、查不到仓库
	// 配置）。**不计入回收数**：空间没被释放，任务行的 worktree_path
	// 也刻意留着（下一轮还会再看一次，届时脏现场若已被人处理掉就能回收）。
	ReapHeld
)

// WorktreeReaper 按 TTL 回收终态任务的工作区现场。
type WorktreeReaper struct {
	Tasks      ReaperTasks
	Worktrees  *WorktreeManager
	RepoLookup RepoLookup

	// TTL 是现场保留时长；<=0 时取 defaultReapTTL。
	TTL time.Duration
	// Interval 是扫描间隔；<=0 时取 defaultReapInterval。
	Interval time.Duration

	// DryRun 为真时只打「本轮会删什么」的日志，不碰磁盘
	// （config.ReapingDryRun）。干跑**不**认领任务行 —— 认领会把
	// worktree_path 置空，那就真的改了状态，不是干跑了。
	DryRun bool

	// Actor 是写进任务事件流的操作者（见 ClaimForReap）。留空时用
	// "system"；生产由 main 传 "node:<NodeName>"，让审计流里能看出是
	// 哪个实例删的。
	Actor string
}

// Run 启动轮询循环，直到 ctx 取消。
//
// 结构照 MergePoller.Run：ticker + ctx.Done 优雅退出，单轮失败只 warn。
func (r *WorktreeReaper) Run(ctx context.Context) {
	ttl := r.effectiveTTL()
	interval := r.Interval
	if interval <= 0 {
		interval = defaultReapInterval
	}
	slog.Info("worktree 收割机已启动", "ttl", ttl, "interval", interval, "dry_run", r.DryRun)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("worktree 收割机停止")
			return
		case <-ticker.C:
			if n, err := r.ReapOnce(ctx, ttl); err != nil {
				slog.Warn("worktree 回收轮次失败", "err", err)
			} else if n > 0 {
				// 只在真回收了东西时打 info，照 gcSessions 的做法 ——
				// 一小时一条「回收了 0 个」是纯噪声。
				slog.Info("worktree 回收完成", "reaped", n)
			}
		}
	}
}

// effectiveTTL 返回生效的 TTL，并把配置层可能漏过的过小值抬到下限。
func (r *WorktreeReaper) effectiveTTL() time.Duration {
	ttl := r.TTL
	if ttl <= 0 {
		ttl = defaultReapTTL
	}
	if ttl < minReapTTL {
		// config.Validate 已经会拒绝这种配置；这里再兜一次是因为
		// WorktreeReaper 也可能被别处直接构造。静默抬高而不是报错：
		// 收割机报错停摆的后果是所有现场只增不减（正是这个 PR 要治的病）。
		slog.Warn("worktree TTL 过小，已抬到下限（TTL 是孤儿清扫的安全机制）",
			"配置值", ttl, "生效值", minReapTTL)
		ttl = minReapTTL
	}
	return ttl
}

// ReapOnce 扫一轮并回收超期现场，返回回收数量。
//
// 导出是为了让测试能直接驱动一轮，不必等 ticker。ttl 会先过下限
// （见 effectiveTTL）—— cutoff 是孤儿清扫唯一的安全边界，不能被一个
// 分钟级的 TTL 抹掉。
func (r *WorktreeReaper) ReapOnce(ctx context.Context, ttl time.Duration) (int, error) {
	if r.Tasks == nil || r.Worktrees == nil {
		return 0, nil
	}
	// 保存并复用调用方传进来的 TTL：effectiveTTL 读的是 r.TTL，
	// 而这里的 ttl 是本轮的实参（Run 传的就是 r.effectiveTTL()，
	// 测试可能直接传别的值）。两条路都必须过下限。
	if ttl <= 0 {
		ttl = defaultReapTTL
	}
	if ttl < minReapTTL {
		slog.Warn("本轮 TTL 过小，已抬到下限（TTL 是孤儿清扫的安全机制）",
			"传入值", ttl, "生效值", minReapTTL)
		ttl = minReapTTL
	}
	cutoff := time.Now().Add(-ttl)
	tasks, err := r.Tasks.ListReapableTasks(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("查询可回收现场失败: %w", err)
	}

	// 活任务认领的路径集合：在本轮开头查一次，供所有候选共用。
	// 查询失败时**整轮不动主路径** —— 判据拿不到就不知道哪条路径还在
	// 被在途任务用，宁可这轮不回收也不能猜。
	claimed, claimErr := r.Tasks.ClaimedWorktreePaths(ctx)
	if claimErr != nil {
		slog.Warn("查活任务认领路径失败，本轮不回收主路径（判据不全会误删在途现场）", "err", claimErr)
	}

	reaped := 0
	if claimErr == nil {
		for _, tk := range tasks {
			// 单个任务失败只 warn 并继续 —— 不让一个坏任务毁掉整轮
			//（照 MergePoller.pollOnce 的容错立场）。
			//
			// 只有 ReapRemoved 计数：ReapHeld 是「出于安全考虑保留了现场」，
			// 把它算进「本轮回收了 N 个」会让运维以为空间被回收了。
			if r.reapTask(ctx, tk, cutoff, claimed) == ReapRemoved {
				reaped++
			}
		}
	}

	// 孤儿目录清扫：磁盘上存在、却没有任何任务行指向的目录。
	// 主回收路径结构上看不见它们（ListReapableTasks 要求 worktree_path
	// 非空），实测就有这种目录。
	n, err := r.sweepOrphans(ctx, cutoff)
	if err != nil {
		// 孤儿清扫失败不该盖掉主路径已经完成的工作
		slog.Warn("孤儿目录清扫失败（主回收已完成）", "err", err)
	}
	return reaped + n, nil
}

// sweepOrphans 清掉「磁盘上有、数据库里没人认领」的工作区目录。
//
// **TTL 在这里不只是策略，是安全机制。** worktree 的目录先被 Create 建出来，
// 之后才在转入 implementing 那次转移里把 worktree_path 写进任务行 ——
// 这两步之间目录是无人认领的。如果不看 mtime 就删，正在跑的任务会突然
// 找不到自己的工作区。默认 72h 的 TTL 让这个窗口安全得离谱。
func (r *WorktreeReaper) sweepOrphans(ctx context.Context, cutoff time.Time) (int, error) {
	root := r.Worktrees.Root()
	if root == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("读工作区根目录失败: %w", err)
	}

	referenced, err := r.Tasks.ReferencedWorktreePaths(ctx)
	if err != nil {
		return 0, fmt.Errorf("查已引用路径失败: %w", err)
	}
	claimed := make(map[string]bool, len(referenced))
	for _, p := range referenced {
		if abs, aerr := filepath.Abs(p); aerr == nil {
			claimed[filepath.Clean(abs)] = true
		}
	}

	swept := 0
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue // .mirrors / .verify 等一律跳过
		}
		abs := filepath.Join(root, e.Name())
		absKey := filepath.Clean(abs)
		if claimed[absKey] {
			continue // 有任务行认领，交给主路径
		}
		info, ierr := e.Info()
		if ierr != nil {
			slog.Warn("读孤儿目录信息失败（跳过）", "path", abs, "err", ierr)
			continue
		}
		if info.ModTime().After(cutoff) {
			continue // 还新，可能是刚建出来、还没记进任务行的现场
		}
		if !r.safeToRemove(abs) {
			continue
		}
		if r.DryRun {
			slog.Info("[干跑] 本轮会清除孤儿工作区目录（数据库里没有任务行指向它）",
				"path", abs, "mtime", info.ModTime(), "cutoff", cutoff)
			swept++
			continue
		}
		if err := os.RemoveAll(abs); err != nil {
			slog.Warn("删除孤儿目录失败", "path", abs, "err", err)
			continue
		}
		slog.Info("已清除孤儿工作区目录（数据库里没有任务行指向它）",
			"path", abs, "mtime", info.ModTime())
		swept++
	}
	return swept, nil
}

// reapTask 回收单个任务的现场，返回本轮对它的处置。
//
// 顺序是刻意的，每一步都必须在碰磁盘之前：
//
//	safeToRemove → 活任务认领 → mtime/TTL → 原子认领 → 体检 → 删
//
// 前四步都是只读或只改数据库，最后一步才删盘。
func (r *WorktreeReaper) reapTask(ctx context.Context, tk *task.Task, cutoff time.Time, claimed []string) ReapOutcome {
	if tk.WorktreePath == nil || *tk.WorktreePath == "" {
		return ReapSkipped
	}
	path := *tk.WorktreePath

	// 防呆：只回收工作区根目录之下的路径，且绝不碰 . 开头的目录。
	// 数据库里的路径理论上都是 Create 造出来的，但一条手工改过的行
	// 就足以让收割机去删 /（或者删掉 .mirrors）。
	if !r.safeToRemove(path) {
		slog.Warn("拒绝回收可疑路径（不在工作区根下，或是隐藏目录）",
			"task", tk.ID, "path", path, "root", r.Worktrees.Root())
		return ReapSkipped
	}

	// 活任务认领：这条路径现在是不是别人正在跑的工作区？
	// 目录名只由 issue key 决定时，两个不同 repo/user 的同名 issue 可以
	// 各有活任务、共用同一目录，而老任务行照样在候选里。
	if claimedBy(claimed, path) {
		slog.Warn("路径仍被在途任务占用，跳过回收（目录名相同不等于同一个任务）",
			"task", tk.ID, "path", path)
		return ReapSkipped
	}

	// TTL 的第二半：任务行超期之外，目录本身也得超期。
	//
	// updated_at 是任务行的时间，mtime 是磁盘的时间，两者可能背离 ——
	// 例如任务行被手工改过（或 Create 刚重建了目录、任务行还没更新）。
	// 孤儿清扫那一侧只有 mtime 可用；主路径两把尺子都能量，就都量：
	// 拿不准的时候不删。
	if tk.UpdatedAt.After(cutoff) {
		return ReapSkipped
	}
	info, statErr := os.Stat(path)
	switch {
	case os.IsNotExist(statErr):
		// 目录已经没了（人手工清过）。这仍是「现场已回收」的一种：
		// 认领后解除任务行的路径认领，别让它永远留在候选里。
	case statErr != nil:
		slog.Warn("读工作区目录信息失败，跳过", "task", tk.ID, "path", path, "err", statErr)
		return ReapSkipped
	case info.ModTime().After(cutoff):
		slog.Info("工作区目录 mtime 未超期，跳过（任务行超期但磁盘上还新，可能刚被重建）",
			"task", tk.ID, "path", path, "mtime", info.ModTime())
		return ReapSkipped
	}

	branch := ""
	if tk.BranchName != nil {
		branch = *tk.BranchName
	}
	// reason 是落进 task_events 的「按什么规则删的」。删盘之后还会补上
	// 「实际删掉了什么」（dir_removed / branch_deleted），让事件流里那条
	// 记录能独立回答「现场是谁在什么时候按什么规则删的、删成了什么样」。
	reason := map[string]any{
		"issue":       tk.ExternalKey,
		"state":       string(tk.State),
		"path":        path,
		"branch":      branch,
		"ttl_seconds": int64(r.effectiveTTL().Seconds()),
		"cutoff":      cutoff.UTC().Format(time.RFC3339),
		"rule":        "终态超期（任务行 updated_at 与目录 mtime 均早于 cutoff）、无在途任务占用",
	}

	// 干跑：所有只读判据都已经过完（safeToRemove / 活任务占用 / 双 mtime
	// 闸），把「本轮会删什么、为什么」打出来就收工。刻意**不**做认领
	// 校验也不做工作区体检 —— 前者会加行锁、后者要跑一串 git 命令，
	// 干跑应当是零副作用、零成本的。
	//
	// 代价是：干跑列出的候选里可能有几条真跑起来会因为脏现场或未推送
	// 提交而被保留。日志里说清楚这件事，别让人以为干跑等于精确预演。
	if r.DryRun {
		slog.Info("[干跑] 本轮会回收超期现场（不删任何东西；实跑时脏现场与未推送分支仍会被保留）",
			"task", tk.ID, "issue", tk.ExternalKey, "state", tk.State,
			"path", path, "branch", branch,
			"理由", reason["rule"], "cutoff", reason["cutoff"])
		return ReapSkipped // 干跑不改任何状态，不算回收
	}

	// ★ 先校验认领，再碰磁盘。校验是一条带守卫的 CAS（见
	// task.Machine.ClaimForReap）：快照之后任务被重试回 queued、路径被
	// 换掉、已被另一轮认领，都会失配 —— 此时**绝不能**删盘。
	//
	// 校验阶段刻意不置空 worktree_path：随后可能因为脏工作区或未推送的
	// 分支决定保留现场，那时任务行里的路径必须还在，否则孤儿清扫会把
	// 这份「本来还想留着」的现场当无主目录收走。置空放在删盘之后。
	ok, cerr := r.Tasks.ClaimForReap(ctx, tk.ID, path, tk.UpdatedAt, false, r.Actor, reason)
	if cerr != nil {
		slog.Warn("认领校验失败，跳过（不碰磁盘）", "task", tk.ID, "path", path, "err", cerr)
		return ReapSkipped
	}
	if !ok {
		slog.Info("任务行在候选快照后已变动（重试/换路径/已被认领），跳过回收",
			"task", tk.ID, "path", path, "state", tk.State)
		return ReapSkipped
	}

	// 认领校验之后再查一次认领者：校验成功只说明「这条任务行还是老样子」，
	// 不排除在两次查询之间有人把另一条活任务指向了同一路径。
	// 成本是每条候选一次查询，换来的是「删之前再确认一次」。
	if fresh, rerr := r.Tasks.ClaimedWorktreePaths(ctx); rerr != nil {
		slog.Warn("认领后复查活任务占用失败，为安全起见不删磁盘（路径认领保持不变，下一轮再看）",
			"task", tk.ID, "path", path, "err", rerr)
		return ReapSkipped
	} else if claimedBy(fresh, path) {
		slog.Warn("认领后发现路径已被另一条在途任务占用，跳过（不删磁盘、不动任务行）",
			"task", tk.ID, "path", path)
		return ReapSkipped
	}

	repoCfg, err := r.RepoLookup(ctx, tk.RepoID)
	if err != nil {
		slog.Warn("回收现场时查不到仓库配置，保留现场（不删磁盘、不动任务行；下一轮再看）",
			"task", tk.ID, "repo", tk.RepoID, "err", err)
		return ReapHeld
	}

	// 分支可能还被活着的后继当作 base（栈式 PR，F4.2-AC2）。
	// 查不确定时保守处理：只删目录、留分支。
	keepBranch := ""
	if branch != "" {
		live, lerr := r.Tasks.HasLiveDependentOnBranch(ctx, branch)
		if lerr != nil {
			slog.Warn("查分支活依赖失败，保守起见只回收目录不删分支",
				"task", tk.ID, "branch", branch, "err", lerr)
			keepBranch = "活依赖查询失败"
		} else if live {
			keepBranch = "仍有未合并后继依赖"
		}
	}

	// 文化性闸：脏现场与未推送的分支不删。这是 D4「保留现场」在收割机
	// 这一侧的落实 —— Remove(force=false) 靠 git 的拒绝来保护的东西，
	// Discard 用 --force 绕过去了，所以在这里补回来。
	//
	// 基线只影响 Inspect 里「相对基线有几个提交」这一项（HasCommits），
	// 按任务类型取即可；取不到就用仓库默认分支。不设 BaseRefOverride：
	// 那是派发侧给栈式 PR 后继用的语义，这里是事后体检。
	kind := KindFix
	if tk.TaskKind != nil && *tk.TaskKind != "" {
		kind = TaskKind(*tk.TaskKind)
	}
	base, berr := repoCfg.BaseBranch(kind)
	if berr != nil {
		base = repoCfg.DefaultBranch
	}
	state := r.Worktrees.Inspect(ctx, repoCfg.ProviderRepo, path, branch, base)
	if !state.Exists {
		// 目录不在（上面已判过）或不是 git 工作树：没有「文化性现场」
		// 可保护，走 Discard 清掉残留的分支与注册。
	} else if state.Dirty {
		slog.Warn("工作区有未提交改动，保留现场不删（D4：人可能正要接手）",
			"task", tk.ID, "path", path, "issue", tk.ExternalKey,
			"branch", branch,
			"提示", "处理掉未提交改动后下一轮会正常回收；确认不要就手工删目录")
		return ReapHeld
	} else if state.HasCommits && !state.RemoteBranch {
		// 分支上有未推送的提交：目录可以删（内容在分支里），但分支是
		// 唯一副本，删了就真没了。只删目录、留分支，并明确告警。
		keepBranch = "分支有未推送提交"
	}

	discardBranch := ""
	if keepBranch == "" {
		discardBranch = branch
	} else {
		slog.Warn("保留分支不删", "task", tk.ID, "branch", branch, "理由", keepBranch)
	}

	// 删前一条、删后一条：原实现只在删完之后打「已回收」，而 Discard
	// 无返回值、mirror 缺失时直接 return —— 什么都没删也照样记成已回收。
	// 现在 Discard 返回实际做成了什么，日志也分成删前（即将删除 X，
	// 因为 Y）与删后（真删掉了什么）两条。
	slog.Info("即将删除超期现场",
		"task", tk.ID, "issue", tk.ExternalKey, "state", tk.State,
		"path", path, "branch", branch, "keep_branch", keepBranch,
		"理由", "终态超期、无人认领、已校验认领")

	res := r.Worktrees.Discard(ctx, repoCfg.ProviderRepo, path, discardBranch)

	// 事件流里要看得见「真删掉了什么」，不是「打算删什么」。
	reason["dir_removed"] = res.DirRemoved
	reason["branch_deleted"] = res.BranchDeleted
	reason["mirror_missing"] = res.MirrorMissing
	if keepBranch != "" {
		reason["branch_kept_because"] = keepBranch
	}

	// 落账放在删盘之后：账目宁可少于事实（下一轮还能再扫到、Discard
	// 幂等），也不能多于事实（声称删了其实没删）。
	//
	// 三种「没真删掉」的情形都照样落账，理由是同一个：不落账就意味着这条
	// 路径永远留在候选里，收割机每小时对它重来一次 —— 那正是首版被批的
	// 「定时炸弹」形状（B1 放大变体）。
	//
	//   - 目录与分支此前都已不在：现场确实已经没了，落账是如实记账。
	//   - mirror 不存在（Discard 什么都没做）：目录可能还在盘上。落账后
	//     它变成「无人认领」的目录，由孤儿清扫按 mtime 收走 —— 那条路径
	//     才是这种残骸的正确归属。日志里 warn 出来让人知道发生了什么。
	//
	// payload 里 dir_removed / mirror_missing 如实记录，事件流不会因此说谎。
	committed, cerrr := r.Tasks.ClaimForReap(ctx, tk.ID, path, tk.UpdatedAt, true, r.Actor, reason)
	if cerrr != nil {
		// ClaimForReap 把置空路径与写事件放在同一个事务里，失败就是两者
		// 都没发生：路径还在，下一轮会再来一次（Discard 幂等）。
		slog.Warn("回收后落账失败（下一轮会重试；磁盘可能已清）",
			"task", tk.ID, "path", path, "err", cerrr)
	}
	if !committed && cerrr == nil {
		// 校验阶段通过、落账阶段却失配：中间有人改了这一行（典型：人点了
		// 重试）。这时磁盘已经删了，而我们**不能**再写任务行 —— 那条行
		// 已经由重试方接管。把它告警出来，现场已重建，任务能照跑。
		slog.Warn("落账前任务行已被改动（疑似并发重试），磁盘已清但不动这一行；重试方会重建现场",
			"task", tk.ID, "path", path, "dir_removed", res.DirRemoved)
	}

	if !res.Removed() {
		if res.MirrorMissing {
			slog.Warn("mirror 不存在，未删除任何东西",
				"task", tk.ID, "path", path, "repo", repoCfg.ProviderRepo)
		} else {
			slog.Info("丢弃动作没有清掉任何东西（目录与分支此前都已不在）",
				"task", tk.ID, "path", path)
		}
		return ReapSkipped
	}

	slog.Info("已回收超期现场",
		"task", tk.ID, "issue", tk.ExternalKey, "state", tk.State,
		"path", path, "dir_removed", res.DirRemoved,
		"branch_deleted", res.BranchDeleted, "keep_branch", keepBranch,
		"errs", len(res.Errs))
	return ReapRemoved
}

// safeToRemove 判断这个路径是否可以交给 Discard。
//
// 三条硬约束：
//   - 必须在工作区根目录之下（用 filepath.Rel 判，不是字符串前缀 ——
//     前缀判会把 /opt/lathe/workspaces-backup 当成 /opt/lathe/workspaces 的子目录）
//   - 相对根的第一段不能以 . 开头（`.mirrors` 是所有仓库的 bare mirror，
//     `.verify` 是 heavy 档基线工作区，两者都不是任务现场）
//   - 中间段不能是 symlink 指到根外的目录（见 safeAbsWithinRoot）
//
// 两侧都用解析过 symlink 的形态再比：root 自己也解析（它可能是一个指向
// 另一块盘的 symlink），否则每个正常路径都会被判成「不在根下」。
func (r *WorktreeReaper) safeToRemove(path string) bool {
	root := r.Worktrees.Root()
	if root == "" {
		return false
	}
	abs, err := safeAbsWithinRoot(root, path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(resolveRoot(root), abs)
	if err != nil {
		return false
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false
	}
	// 逐段检查：任何一段以 . 开头都拒绝
	for _, seg := range strings.Split(rel, string(os.PathSeparator)) {
		if strings.HasPrefix(seg, ".") {
			return false
		}
	}
	return true
}

// safeAbs 把路径规范化，并把中间段的 symlink 解析掉（简写形态）。
func (r *WorktreeReaper) safeAbs(path string) (string, error) {
	return safeAbsWithinRoot(r.Worktrees.Root(), path)
}

// safeAbsWithinRoot 把候选路径规范化到「真实位置」，供 filepath.Rel 使用。
//
// 为什么不能只做 filepath.Rel：它是纯字符串运算，看不见符号链接。
// 若 <root>/cr-100 本身是指向 /etc 的 symlink，或路径中间某一段是，
// Rel 会判出它「在根下」，而 os.RemoveAll 会顺着链接删到根外。
// EvalSymlinks 之后 Rel 判的就是真实位置了。
//
// 解析失败（多半是路径不存在）时退回「解析已存在的父链 + 保留末段」：
// 路径不存在就没有越界可谈，而且后续 os.Stat 会按「已消失」处理 ——
// 那正是我们要的（既不能当成可删的目录，也不该当成删除错误）。
func safeAbsWithinRoot(root, path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)

	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		return filepath.Clean(resolved), nil
	}
	if parentResolved, perr := filepath.EvalSymlinks(filepath.Dir(abs)); perr == nil {
		return filepath.Join(parentResolved, filepath.Base(abs)), nil
	}
	_ = root
	return abs, nil
}

// resolveRoot 解析工作区根目录的真实路径；解析不了就退回清理过的原值。
func resolveRoot(root string) string {
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		return filepath.Clean(resolved)
	}
	if abs, err := filepath.Abs(root); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(root)
}
