package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Clouditera/lathe/internal/task"
)

// fakeReaperTasks 是 ReaperTasks 的假件：让测试完全掌控「有哪些可回收任务、
// 分支有没有活依赖」，不必造一整套数据库状态。
type fakeReaperTasks struct {
	reapable []*task.Task
	// liveBranches 里的分支被视为「仍有未合并后继依赖」
	liveBranches map[string]bool
	liveErr      error
	cleared      []int64
	clearErr     error
	cutoff       time.Time
	// referenced 是「已被任务行认领」的路径，孤儿清扫据此判断
	referenced []string
	refErr     error
}

func (f *fakeReaperTasks) ListReapableTasks(ctx context.Context, olderThan time.Time) ([]*task.Task, error) {
	f.cutoff = olderThan
	return f.reapable, nil
}
func (f *fakeReaperTasks) HasLiveDependentOnBranch(ctx context.Context, branch string) (bool, error) {
	if f.liveErr != nil {
		return false, f.liveErr
	}
	return f.liveBranches[branch], nil
}
func (f *fakeReaperTasks) ClearWorktreePath(ctx context.Context, id int64) error {
	f.cleared = append(f.cleared, id)
	return f.clearErr
}
func (f *fakeReaperTasks) ReferencedWorktreePaths(ctx context.Context) ([]string, error) {
	if f.refErr != nil {
		return nil, f.refErr
	}
	return f.referenced, nil
}

// reaperEnv 造一个工作区根目录（含 .mirrors / .verify 两个隐藏目录）
// 与一个 WorktreeManager。
func reaperEnv(t *testing.T) (*WorktreeManager, string) {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{".mirrors", ".verify"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("造隐藏目录失败: %v", err)
		}
	}
	wm, err := NewWorktreeManager(root)
	if err != nil {
		t.Fatalf("NewWorktreeManager 失败: %v", err)
	}
	return wm, root
}

func terminalTask(id int64, path, branch string, state task.State) *task.Task {
	return &task.Task{
		ID: id, RepoID: 1, LinearIssueKey: "CR-" + string(rune('0'+id)),
		State: state, WorktreePath: &path, BranchName: &branch,
	}
}

// ★ AC5：绝不触碰 . 开头的目录。
//
// workspaces 下有 .mirrors/（所有仓库的 bare mirror）与 .verify/
// （heavy 档基线工作区）。误删 .mirrors/ 等于把所有仓库的镜像清了。
// 一条被手工改过的 worktree_path 就足以让收割机去删它。
func TestReaperNeverTouchesHiddenDirs(t *testing.T) {
	wm, root := reaperEnv(t)
	for _, name := range []string{".mirrors", ".verify"} {
		p := filepath.Join(root, name)
		r := &WorktreeReaper{
			Tasks:      &fakeReaperTasks{reapable: []*task.Task{terminalTask(1, p, "", task.StateFailed)}},
			Worktrees:  wm,
			RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil },
		}
		n, err := r.ReapOnce(context.Background(), time.Hour)
		if err != nil {
			t.Fatalf("ReapOnce 失败: %v", err)
		}
		if n != 0 {
			t.Errorf("%s 不该被回收，却报告回收了 %d 个", name, n)
		}
		if _, err := os.Stat(p); err != nil {
			t.Errorf("AC5：%s 被删了！%v", name, err)
		}
	}
}

// 工作区根目录之外的路径一律拒绝。
//
// 用 filepath.Rel 判而不是字符串前缀：前缀判会把
// /root/workspaces-backup 当成 /root/workspaces 的子目录。
func TestReaperRejectsPathsOutsideRoot(t *testing.T) {
	wm, root := reaperEnv(t)
	outside := t.TempDir() // 另一个目录，不在 root 下
	sentinel := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 前缀相同但不是子目录的兄弟目录
	sibling := root + "-backup"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sibling) })

	for _, p := range []string{outside, sibling, root, filepath.Join(root, "..")} {
		r := &WorktreeReaper{
			Tasks:      &fakeReaperTasks{reapable: []*task.Task{terminalTask(1, p, "", task.StateFailed)}},
			Worktrees:  wm,
			RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil },
		}
		n, _ := r.ReapOnce(context.Background(), time.Hour)
		if n != 0 {
			t.Errorf("根目录之外的路径 %q 不该被回收", p)
		}
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("根目录之外的文件被删了！%v", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("同前缀的兄弟目录被删了！%v", err)
	}
}

// ★ AC6：删分支前查 HasLiveDependentOnBranch。
//
// 栈式 PR 的后继可能还把这个分支当 base（F4.2-AC2）。不查就会把
// 别人的 base 删掉，后继再也 rebase 不上去。
// 有活依赖时只回收目录、保留分支。
func TestReaperKeepsBranchWithLiveDependent(t *testing.T) {
	wm, root := reaperEnv(t)
	wtPath := filepath.Join(root, "cr-1")
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatal(err)
	}

	ft := &fakeReaperTasks{
		reapable:     []*task.Task{terminalTask(1, wtPath, "fix/cr-1", task.StateFailed)},
		liveBranches: map[string]bool{"fix/cr-1": true},
	}
	r := &WorktreeReaper{
		Tasks:      ft,
		Worktrees:  wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil },
	}
	n, err := r.ReapOnce(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("有活依赖时仍该回收目录，得到 %d", n)
	}
	// 目录被清掉（没有 mirror 时 Discard 直接 return，所以这里只断言
	// 记账发生了 —— 真实分支删除的行为由 WorktreeManager 自己的测试覆盖）
	if len(ft.cleared) != 1 || ft.cleared[0] != 1 {
		t.Errorf("回收后应清空 worktree_path，实际 cleared=%v", ft.cleared)
	}
}

// 查活依赖出错时保守处理：只回收目录，不删分支，且不让整轮失败。
func TestReaperConservativeWhenDependencyCheckFails(t *testing.T) {
	wm, root := reaperEnv(t)
	wtPath := filepath.Join(root, "cr-2")
	if err := os.MkdirAll(wtPath, 0o755); err != nil {
		t.Fatal(err)
	}
	ft := &fakeReaperTasks{
		reapable: []*task.Task{terminalTask(2, wtPath, "fix/cr-2", task.StateCancelled)},
		liveErr:  errors.New("数据库抖了一下"),
	}
	r := &WorktreeReaper{
		Tasks:      ft,
		Worktrees:  wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil },
	}
	n, err := r.ReapOnce(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("单个任务的依赖查询失败不该让整轮报错: %v", err)
	}
	if n != 1 {
		t.Errorf("应仍回收目录，得到 %d", n)
	}
}

// TTL 换算成 cutoff 时间点，传给查询层。
//
// AC2（非终态绝不回收）由查询层的 state = ANY(终态) 保证，
// 那一条在 internal/task 的测试里覆盖；这里只验 TTL 被正确换算。
func TestReaperPassesCutoffFromTTL(t *testing.T) {
	wm, _ := reaperEnv(t)
	ft := &fakeReaperTasks{}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	before := time.Now()
	if _, err := r.ReapOnce(context.Background(), 72*time.Hour); err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	want := before.Add(-72 * time.Hour)
	if ft.cutoff.Before(want.Add(-time.Minute)) || ft.cutoff.After(want.Add(time.Minute)) {
		t.Errorf("cutoff = %v，期望约 %v（now - 72h）", ft.cutoff, want)
	}
}

// 没有 worktree_path 的任务跳过，不算回收。
func TestReaperSkipsTasksWithoutWorktree(t *testing.T) {
	wm, _ := reaperEnv(t)
	empty := ""
	ft := &fakeReaperTasks{reapable: []*task.Task{
		{ID: 9, State: task.StateFailed, WorktreePath: nil},
		{ID: 10, State: task.StateFailed, WorktreePath: &empty},
	}}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	n, err := r.ReapOnce(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	if n != 0 {
		t.Errorf("无现场的任务不该被算作回收，得到 %d", n)
	}
	if len(ft.cleared) != 0 {
		t.Errorf("不该对无现场的任务做记账，实际 %v", ft.cleared)
	}
}

// 依赖未装配时静默不动（照 Gates 为 nil 即放行的立场）。
func TestReaperNoopWithoutDependencies(t *testing.T) {
	r := &WorktreeReaper{}
	n, err := r.ReapOnce(context.Background(), time.Hour)
	if err != nil || n != 0 {
		t.Errorf("未装配时应静默不动，得到 n=%d err=%v", n, err)
	}
}

// ---------------------------------------------------------------- 孤儿目录清扫

// 孤儿目录：磁盘上有、数据库里没有任何任务行指向它。
//
// 主回收路径由数据库驱动，结构上看不见这种目录 —— 实测就有：
// 任务 649 是 failed 但 worktree_path 为 NULL，cr-1367 那个目录
// 因此永远没人回收。
func TestReaperSweepsUnclaimedDirs(t *testing.T) {
	wm, root := reaperEnv(t)
	orphan := filepath.Join(root, "cr-orphan")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	// 把 mtime 推到很久以前，模拟一个陈旧的遗留目录
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}

	ft := &fakeReaperTasks{referenced: nil} // 没有任何任务行认领它
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	n, err := r.ReapOnce(context.Background(), 72*time.Hour)
	if err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	if n != 1 {
		t.Errorf("应清掉 1 个孤儿目录，得到 %d", n)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("孤儿目录应被清掉，实际仍在（err=%v）", err)
	}
}

// ★ 安全机制：新目录绝不删。
//
// worktree 目录先被 Create 建出来，之后才在转入 implementing 那次转移里
// 把 worktree_path 写进任务行 —— 这两步之间目录是无人认领的。
// 不看 mtime 就删，正在跑的任务会突然找不到自己的工作区。
func TestReaperSweepSparesFreshUnclaimedDirs(t *testing.T) {
	wm, root := reaperEnv(t)
	fresh := filepath.Join(root, "cr-just-created")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	// mtime 就是现在 —— 模拟「刚 Create 出来、还没写进任务行」

	ft := &fakeReaperTasks{referenced: nil}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	n, err := r.ReapOnce(context.Background(), 72*time.Hour)
	if err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	if n != 0 {
		t.Errorf("新建的无主目录不该被清（可能是在途任务的现场），得到 %d", n)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("新目录被删了！这会让在途任务找不到自己的工作区: %v", err)
	}
}

// 有任务行认领的目录不走孤儿清扫（交给主路径按状态与 TTL 判断）。
//
// 这一条防的是双重回收：pr_open 这种非终态任务的现场被主路径正确排除后，
// 若孤儿清扫又按 mtime 把它删了，AC2 就形同虚设。
func TestReaperSweepSkipsClaimedDirs(t *testing.T) {
	wm, root := reaperEnv(t)
	claimed := filepath.Join(root, "cr-pr-open")
	if err := os.MkdirAll(claimed, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(claimed, old, old); err != nil {
		t.Fatal(err)
	}

	// 有任务行指向它（但该任务是 pr_open，不在 ListReapableTasks 结果里）
	ft := &fakeReaperTasks{referenced: []string{claimed}}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	n, err := r.ReapOnce(context.Background(), 72*time.Hour)
	if err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	if n != 0 {
		t.Errorf("被任务行认领的目录不该走孤儿清扫，得到 %d", n)
	}
	if _, err := os.Stat(claimed); err != nil {
		t.Errorf("AC2 被绕过了：非终态任务的现场被孤儿清扫删了: %v", err)
	}
}

// 孤儿清扫也绝不碰 . 开头的目录（哪怕它们很旧、也没人认领）。
func TestReaperSweepNeverTouchesHiddenDirs(t *testing.T) {
	wm, root := reaperEnv(t)
	old := time.Now().Add(-90 * 24 * time.Hour)
	for _, name := range []string{".mirrors", ".verify"} {
		p := filepath.Join(root, name)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	ft := &fakeReaperTasks{referenced: nil}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	if _, err := r.ReapOnce(context.Background(), 72*time.Hour); err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	for _, name := range []string{".mirrors", ".verify"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("%s 被孤儿清扫删了！%v", name, err)
		}
	}
}
