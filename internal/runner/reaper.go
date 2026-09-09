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
// 六个不查就会误删的地方
// ---------------------------------------------------------------------------
//
//  1. **只碰终态任务。** failed 可以转回 queued —— 人随时可能重试续跑，
//     而 D4 保留现场的全部目的就是让人能接手。所以 TTL 默认值必须保守
//     （天级），激进的 TTL 会把人正要重试的现场删掉。
//
//  2. **绝不碰 `.` 开头的目录。** workspaces 下有 `.mirrors/`（所有仓库的
//     bare mirror）与 `.verify/`（heavy 档基线工作区）。误删 `.mirrors/`
//     等于把所有仓库的镜像清了，下一个任务要重新 clone。
//     注意 preview.Discover 的跳过列表**不含** `.` 前缀通配，别照抄它。
//
//  3. **删分支前查 HasLiveDependentOnBranch。** 栈式 PR 的后继可能还在把
//     这个分支当 base（F4.2-AC2）。不查就会把别人的 base 删掉，
//     后继再也 rebase 不上去。
//
//  4. **用 Discard 而不是 Remove。** 尸体可能是残缺的（目录被手工删过、
//     分支已不存在），Remove 在这种情形下会报错，而 Discard 尽力清理
//     每一步并 prune 兜底。
//
//  5. **`worktree_path` 置空要走专门语句。** Transition 的 UPDATE 是
//     COALESCE 语义（只增不清空），传 nil 表示「这次不改」。
//     见 task.Machine.ClearWorktreePath。
//
//  6. **孤儿目录的 TTL 不只是策略，是安全机制。** 主回收路径由数据库驱动，
//     看不见「磁盘上有、没有任务行指向」的目录（实测就有：任务 649 是
//     failed 但 worktree_path 为 NULL，cr-1367 那个目录因此永远没人回收）。
//     补的清扫按 mtime 判超期 —— 而 worktree 目录是先被 Create 建出来、
//     之后才在转入 implementing 时把路径写进任务行的，两步之间目录无人认领。
//     不看 mtime 就删，正在跑的任务会突然找不到自己的工作区。

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Clouditera/lathe/internal/task"
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
)

// ReaperTasks 是收割机需要的任务查询与记账。
//
// 声明窄接口而不是直接吃 *task.Machine：测试要能注入假件，
// 而 Machine 的方法集远大于这里需要的四个（沿用本包
// VerificationRecorder / TaskMail 的做法）。
type ReaperTasks interface {
	ListReapableTasks(ctx context.Context, olderThan time.Time) ([]*task.Task, error)
	HasLiveDependentOnBranch(ctx context.Context, branchName string) (bool, error)
	ClearWorktreePath(ctx context.Context, id int64) error
	ReferencedWorktreePaths(ctx context.Context) ([]string, error)
}

// WorktreeReaper 按 TTL 回收终态任务的工作区现场。
type WorktreeReaper struct {
	Tasks      ReaperTasks
	Worktrees  *WorktreeManager
	RepoLookup RepoLookup

	// TTL 是现场保留时长；<=0 时取 defaultReapTTL。
	TTL time.Duration
	// Interval 是扫描间隔；<=0 时取 defaultReapInterval。
	Interval time.Duration
}

// Run 启动轮询循环，直到 ctx 取消。
//
// 结构照 MergePoller.Run：ticker + ctx.Done 优雅退出，单轮失败只 warn。
func (r *WorktreeReaper) Run(ctx context.Context) {
	ttl := r.TTL
	if ttl <= 0 {
		ttl = defaultReapTTL
	}
	interval := r.Interval
	if interval <= 0 {
		interval = defaultReapInterval
	}
	slog.Info("worktree 收割机已启动", "ttl", ttl, "interval", interval)

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

// ReapOnce 扫一轮并回收超期现场，返回回收数量。
//
// 导出是为了让测试能直接驱动一轮，不必等 ticker。
func (r *WorktreeReaper) ReapOnce(ctx context.Context, ttl time.Duration) (int, error) {
	if r.Tasks == nil || r.Worktrees == nil {
		return 0, nil
	}
	cutoff := time.Now().Add(-ttl)
	tasks, err := r.Tasks.ListReapableTasks(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("查询可回收现场失败: %w", err)
	}

	reaped := 0
	for _, tk := range tasks {
		// 单个任务失败只 warn 并继续 —— 不让一个坏任务毁掉整轮
		// （照 MergePoller.pollOnce 的容错立场）。
		if r.reapTask(ctx, tk) {
			reaped++
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
			claimed[abs] = true
		}
	}

	swept := 0
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue // .mirrors / .verify 等一律跳过
		}
		abs := filepath.Join(root, e.Name())
		if claimed[abs] {
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

// reapTask 回收单个任务的现场，返回是否真的回收了。
func (r *WorktreeReaper) reapTask(ctx context.Context, tk *task.Task) bool {
	if tk.WorktreePath == nil || *tk.WorktreePath == "" {
		return false
	}
	path := *tk.WorktreePath

	// 防呆：只回收工作区根目录之下的路径，且绝不碰 . 开头的目录。
	// 数据库里的路径理论上都是 Create 造出来的，但一条手工改过的行
	// 就足以让收割机去删 /（或者删掉 .mirrors）。
	if !r.safeToRemove(path) {
		slog.Warn("拒绝回收可疑路径（不在工作区根下，或是隐藏目录）",
			"task", tk.ID, "path", path, "root", r.Worktrees.Root())
		return false
	}

	branch := ""
	if tk.BranchName != nil {
		branch = *tk.BranchName
	}

	// 分支可能还被活着的后继当作 base（栈式 PR，F4.2-AC2）。
	// 查不确定时保守处理：只删目录、留分支。
	if branch != "" {
		live, err := r.Tasks.HasLiveDependentOnBranch(ctx, branch)
		if err != nil {
			slog.Warn("查分支活依赖失败，保守起见只回收目录不删分支",
				"task", tk.ID, "branch", branch, "err", err)
			branch = ""
		} else if live {
			slog.Info("分支仍有未合并后继依赖，只回收目录不删分支",
				"task", tk.ID, "branch", branch)
			branch = ""
		}
	}

	repoCfg, err := r.RepoLookup(ctx, tk.RepoID)
	if err != nil {
		slog.Warn("回收现场时查不到仓库配置", "task", tk.ID, "repo", tk.RepoID, "err", err)
		return false
	}

	// Discard 而非 Remove：尸体可能残缺（目录被手工删过、分支已不存在），
	// Discard 尽力清理每一步并 prune 兜底。它无返回值 —— 尽力而为的语义。
	r.Worktrees.Discard(ctx, repoCfg.ProviderRepo, path, branch)

	// 记账：置空 worktree_path，别让下一轮反复扫到同一行。
	// 失败只 warn —— 磁盘已经清了，这一列没改成 NULL 只会让下一轮
	// 再走一次（Discard 幂等），比让整轮报错好。
	if err := r.Tasks.ClearWorktreePath(ctx, tk.ID); err != nil {
		slog.Warn("回收后清空 worktree_path 失败（下一轮会重试）", "task", tk.ID, "err", err)
	}

	slog.Info("已回收超期现场",
		"task", tk.ID, "issue", tk.LinearIssueKey, "state", tk.State,
		"path", path, "branch_deleted", branch != "")
	return true
}

// safeToRemove 判断这个路径是否可以交给 Discard。
//
// 两条硬约束：
//   - 必须在工作区根目录之下（用 filepath.Rel 判，不是字符串前缀 ——
//     前缀判会把 /opt/lathe/workspaces-backup 当成 /opt/lathe/workspaces 的子目录）
//   - 相对根的第一段不能以 . 开头（`.mirrors` 是所有仓库的 bare mirror，
//     `.verify` 是 heavy 档基线工作区，两者都不是任务现场）
func (r *WorktreeReaper) safeToRemove(path string) bool {
	root := r.Worktrees.Root()
	if root == "" {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, abs)
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
