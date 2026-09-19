package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// fakeReaperTasks 是 ReaperTasks 的假件：让测试完全掌控「有哪些可回收任务、
// 分支有没有活依赖」，不必造一整套数据库状态。
type fakeReaperTasks struct {
	reapable []*task.Task
	// liveBranches 里的分支被视为「仍有未合并后继依赖」
	liveBranches map[string]bool
	liveErr      error
	cleared      []int64
	cutoff       time.Time
	// referenced 是「已被任务行认领」的路径，孤儿清扫据此判断
	referenced []string
	refErr     error
	// claimed 是「被非终态任务认领」的路径，主路径删盘前的最后一道闸
	claimed  []string
	claimErr error
	// claimFail 为真时 ClaimForReap 一律返回 false（模拟「快照之后
	// 任务行被动过」）
	claimFail bool
	claimErr2 error
	// claimCalls 按顺序记录每次认领（含 commit 阶段），供断言顺序
	claimCalls []claimCall
	// claimedCalls 统计 ClaimedWorktreePaths 被调用了几次
	claimedCalls int
	// claimedAfter 非 nil 时，第 2 次及以后的 ClaimedWorktreePaths
	// 返回它 —— 模拟「认领校验通过之后有人把活任务指向了同一路径」
	claimedAfter []string
	// commitLost 为真时 commit 阶段的认领失配（模拟并发重试）
	commitLost bool
}

type claimCall struct {
	id     int64
	path   string
	commit bool
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

// ReferencedWorktreePaths 还原生产语义：**任何**任务行引用的路径，
// 不分状态 —— 包括 reapable 里那些终态行自己。
//
// 这一点必须还原，否则测试会漏掉一整类失效：主路径正确跳过一个现场
// （认领失败/脏工作区/未超期）之后，同一次 ReapOnce 里的 sweepOrphans
// 若把它当「无人认领」删掉，B1/B2/B3 的全部修复都会被绕过。生产里
// ReferencedWorktreePaths 包含终态行（这正是另开 ClaimedWorktreePaths
// 的理由），所以被跳过的现场在孤儿清扫眼里始终是有主的。
//
// 取并集而不是让每条测试自己写 referenced：让「忘了设」这个夹具缺口
// 从结构上不可能再出现。测试仍可用 referenced 额外声明「别的任务行
// 也指着某条路径」。
func (f *fakeReaperTasks) ReferencedWorktreePaths(ctx context.Context) ([]string, error) {
	if f.refErr != nil {
		return nil, f.refErr
	}
	out := append([]string(nil), f.referenced...)
	for _, tk := range f.reapable {
		if tk.WorktreePath != nil && *tk.WorktreePath != "" {
			out = append(out, *tk.WorktreePath)
		}
	}
	return out, nil
}
func (f *fakeReaperTasks) ClaimedWorktreePaths(ctx context.Context) ([]string, error) {
	f.claimedCalls++
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	// 主路径每轮查一次（开头），认领校验之后再查一次；claimedAfter
	// 让第二次返回「路径已被在途任务接管」。
	if f.claimedCalls > 1 && f.claimedAfter != nil {
		return f.claimedAfter, nil
	}
	return f.claimed, nil
}
func (f *fakeReaperTasks) ClaimForReap(ctx context.Context, id int64, path string, snapshotUpdatedAt time.Time, commit bool, actor string, reason map[string]any) (bool, error) {
	f.claimCalls = append(f.claimCalls, claimCall{id: id, path: path, commit: commit})
	if f.claimErr2 != nil {
		return false, f.claimErr2
	}
	if f.claimFail {
		return false, nil
	}
	if commit {
		if f.commitLost {
			return false, nil
		}
		f.cleared = append(f.cleared, id)
	}
	return true, nil
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

// terminalTask 造一条「已进终态、留了现场、且早该被回收」的任务行。
//
// UpdatedAt 显式设成很久以前：主路径现在拿 mtime 与任务行的 updated_at
// 两把尺子量同一件事（都超期才删），零值时间在真实数据库里不会出现，
// 在这里却会让候选看起来"刚刚更新过"而错过回收。
func terminalTask(id int64, path, branch string, state task.State) *task.Task {
	return &task.Task{
		ID: id, RepoID: 1, LinearIssueKey: "CR-" + string(rune('0'+id)),
		State: state, WorktreePath: &path, BranchName: &branch,
		UpdatedAt: time.Now().Add(-30 * 24 * time.Hour),
	}
}

// chtimes 把目录 mtime 推到很久以前，模拟一个陈旧的现场。
func chtimes(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
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
//
// 这一条现在用真仓库跑：目录要被删掉就得有 mirror（Discard 在没有
// mirror 时什么都不做），而分支保留与否也只有真 git 能验。
func TestReaperKeepsBranchWithLiveDependent(t *testing.T) {
	src := sourceRepo(t)
	wm := newManager(t)
	ctx := context.Background()

	wt, err := wm.Create(ctx, CreateParams{
		Repo: DefaultRepoConfig("acme/demo"), CloneURL: src,
		Kind: KindFix, IssueKey: "CR-1", Title: "t", TaskID: 1,
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	chtimes(t, wt.Path)

	ft := &fakeReaperTasks{
		reapable:     []*task.Task{terminalTask(1, wt.Path, wt.Branch, task.StateFailed)},
		liveBranches: map[string]bool{wt.Branch: true},
	}
	r := &WorktreeReaper{
		Tasks:      ft,
		Worktrees:  wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil },
	}
	n, err := r.ReapOnce(ctx, time.Hour)
	if err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("有活依赖时仍该回收目录，得到 %d", n)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Errorf("目录应被回收，实际仍在（err=%v）", err)
	}
	// ★ 分支必须还在：还有未合并的后继依赖它
	if out := gitOut(t, wt.Mirror, "branch", "--list", wt.Branch); !strings.Contains(out, wt.Branch) {
		t.Errorf("AC6：有活依赖时分支被删了！branch --list = %q", out)
	}
	// 回收完成后应落账（commit 阶段解除路径认领）
	if len(ft.cleared) != 1 || ft.cleared[0] != 1 {
		t.Errorf("回收后应落账，实际 cleared=%v", ft.cleared)
	}
}

// 查活依赖出错时保守处理：仍回收目录（分支留着），且不让整轮失败。
func TestReaperConservativeWhenDependencyCheckFails(t *testing.T) {
	src := sourceRepo(t)
	wm := newManager(t)
	ctx := context.Background()

	wt, err := wm.Create(ctx, CreateParams{
		Repo: DefaultRepoConfig("acme/demo"), CloneURL: src,
		Kind: KindFix, IssueKey: "CR-2", Title: "t", TaskID: 2,
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	chtimes(t, wt.Path)

	ft := &fakeReaperTasks{
		reapable: []*task.Task{terminalTask(2, wt.Path, wt.Branch, task.StateCancelled)},
		liveErr:  errors.New("数据库抖了一下"),
	}
	r := &WorktreeReaper{
		Tasks:      ft,
		Worktrees:  wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil },
	}
	n, err := r.ReapOnce(ctx, time.Hour)
	if err != nil {
		t.Fatalf("单个任务的依赖查询失败不该让整轮报错: %v", err)
	}
	if n != 1 {
		t.Errorf("应仍回收目录，得到 %d", n)
	}
	if out := gitOut(t, wt.Mirror, "branch", "--list", wt.Branch); !strings.Contains(out, wt.Branch) {
		t.Errorf("依赖查询失败时应保留分支，实际 branch --list = %q", out)
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

// ★ B4：TTL 下限。ReapOnce 拿到一个荒谬的小 TTL（直接构造
// WorktreeReaper 绕过了 config.Validate）时，也要抬到下限再算 cutoff ——
// cutoff 是孤儿清扫唯一的安全边界，1 分钟的 cutoff 会把在途任务
// 「刚建出来、还没写进任务行」的目录判成孤儿。
func TestReaperClampsSmallTTL(t *testing.T) {
	wm, root := reaperEnv(t)
	fresh := filepath.Join(root, "cr-just-created")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	// mtime 做成「30 分钟前」—— 这个年龄恰好落在两个 cutoff 之间：
	//   传入的 1m TTL   → cutoff = now-1m  → 30 分钟前的目录「已超期」，会被删
	//   抬到下限的 1h   → cutoff = now-1h  → 30 分钟前的目录「还新」，应保留
	// 所以它能真正区分「有没有抬下限」。
	// （原先写 -2h 是错的：2 小时前的目录在 1h 下限下本来就该被回收，
	// 那个断言无论有没有抬下限都会失败。）
	halfHour := time.Now().Add(-30 * time.Minute)
	if err := os.Chtimes(fresh, halfHour, halfHour); err != nil {
		t.Fatal(err)
	}

	ft := &fakeReaperTasks{referenced: nil}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	// 传 1 分钟：若不抬到下限，这个目录会被当孤儿删掉
	if _, err := r.ReapOnce(context.Background(), time.Minute); err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("TTL 小于下限时应被抬到 1h，30 分钟前的目录不该被当孤儿删掉: %v", err)
	}

	// 反证：同一个目录在一个真正宽裕的 TTL 下也保留（说明上面的保留
	// 来自下限而不是别的原因），而把它推到 2 小时前就会被回收 ——
	// 证明清扫逻辑本身是活的，不是整体失灵。
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(fresh, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReapOnce(context.Background(), time.Minute); err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Errorf("超过 1h 下限的孤儿目录应被清掉（否则上一条断言可能是假阳性），实际 err=%v", err)
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

// ---------------------------------------------------------------- B1 误删推演

// ★ B1 主线：老 failed 行指着的目录，正被另一条在途任务用着 —— 不能删。
//
// 误删推演（默认 72h，无需任何配置错误）：
//
//	任务 A（CR-100，repo X）failed 超期，worktree_path 仍在；
//	issue 重开建出任务 B，老命名下 Create 复用同名目录，B 正在里面跑；
//	ListReapableTasks 返回 A；旧实现只看 A.WorktreePath 这个字符串，
//	HasLiveDependentOnBranch 查的是别人的 base_ref（B 不是栈式后继）
//	返回 false，于是 Discard 把 B 正在跑的工作区连同未提交改动删掉。
//
// 跨用户版本同样成立（tasks_one_active_per_issue 带 repo_id 维度）。
// 现在同一轮已经查出来的「非终态任务认领的路径」就是那道闸。
func TestReaperRefusesPathClaimedByLiveTask(t *testing.T) {
	wm, root := reaperEnv(t)
	shared := filepath.Join(root, "cr-100")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	inflight := filepath.Join(shared, "wip.txt")
	if err := os.WriteFile(inflight, []byte("任务 B 正在写的东西\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	chtimes(t, shared)

	ft := &fakeReaperTasks{
		// 任务 A：failed、超期、指着这条路径
		reapable: []*task.Task{terminalTask(1, shared, "fix/cr-100", task.StateFailed)},
		// 任务 B（另一个 repo / 另一个用户 / 同 issue 重开）：在途，认领同一路径
		claimed: []string{shared},
		// 孤儿清扫侧也认领（任何行都算），别让它绕过来
		referenced: []string{shared},
	}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	n, err := r.ReapOnce(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	if n != 0 {
		t.Errorf("被在途任务占用的路径不该被回收，得到 %d", n)
	}
	if _, err := os.Stat(inflight); err != nil {
		t.Errorf("B1：在途任务正在跑的工作区被删了！未提交改动一起没了: %v", err)
	}
	// 连认领都不该发起 —— 判据在碰数据库之前就该拦住
	for _, c := range ft.claimCalls {
		if c.commit {
			t.Errorf("B1：不该对被在途任务占用的路径落账，实际 claimCalls=%v", ft.claimCalls)
		}
	}
}

// ★ B1 放大变体：落账失败时**不能**让下一轮继续对同一目录动手。
//
// 旧实现里 ClearWorktreePath 失败只 warn，路径永远留在候选里，于是
// 收割机每小时对那个目录执行一次 Discard —— 定时炸弹。现在的护栏是：
// 每一轮都重新查活任务认领 + 每一轮都重新 mtime 判超期，路径若被接管
// 就再也不会被删。这里验的是「落账失败不会让它把在途现场删掉」。
func TestReaperRecheckClaimAfterCommitFailure(t *testing.T) {
	wm, root := reaperEnv(t)
	p := filepath.Join(root, "cr-200")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	chtimes(t, p)

	ft := &fakeReaperTasks{
		reapable:   []*task.Task{terminalTask(1, p, "fix/cr-200", task.StateFailed)},
		commitLost: true, // 落账阶段失配（人正好点了重试）
		// 认领校验之后，路径已被在途任务接管
		claimedAfter: []string{p},
	}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	n, err := r.ReapOnce(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	if n != 0 {
		t.Errorf("认领后复查发现路径被接管时不该回收，得到 %d", n)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("认领后被接管的目录被删了: %v", err)
	}
}

// ---------------------------------------------------------------- B2 认领竞态

// ★ B2：认领必须在删盘之前，且认领失败一律不碰磁盘。
//
// 候选快照与真正删除之间没有 CAS 时，「人点重试」与「收割机删除」并发
// 会让正在运行的现场被删、分支被强删、worktree_path 被置 NULL。
// 窗口不是毫秒级：循环里每个任务跑若干条 git 命令（gitTimeout 15 分钟）。
func TestReaperSkipsWhenClaimLost(t *testing.T) {
	wm, root := reaperEnv(t)
	p := filepath.Join(root, "cr-300")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(p, "resumed.txt")
	if err := os.WriteFile(sentinel, []byte("重试续跑正在用这个现场\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	chtimes(t, p)

	ft := &fakeReaperTasks{
		reapable:  []*task.Task{terminalTask(1, p, "fix/cr-300", task.StateFailed)},
		claimFail: true, // 快照之后任务行被动过（failed → queued）
	}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	n, err := r.ReapOnce(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	if n != 0 {
		t.Errorf("认领失败时不该算作回收，得到 %d", n)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("B2：认领失败却删了磁盘！重试续跑的现场没了: %v", err)
	}
	if len(ft.cleared) != 0 {
		t.Errorf("认领失败时不该落账，实际 cleared=%v", ft.cleared)
	}
}

// 认领查询报错也一律跳过（不碰磁盘）。
func TestReaperSkipsWhenClaimErrors(t *testing.T) {
	wm, root := reaperEnv(t)
	p := filepath.Join(root, "cr-301")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	chtimes(t, p)

	ft := &fakeReaperTasks{
		reapable:  []*task.Task{terminalTask(1, p, "fix/cr-301", task.StateFailed)},
		claimErr2: errors.New("数据库连接断了"),
	}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	if _, err := r.ReapOnce(context.Background(), time.Hour); err != nil {
		t.Fatalf("单个任务的认领出错不该让整轮报错: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("认领出错时不该删磁盘: %v", err)
	}
}

// ★ 认领必须先校验（commit=false）、删盘之后再落账（commit=true）。
//
// 顺序反了的后果：落账先置空 worktree_path，随后若因为脏现场决定保留，
// 那份现场就变成了「磁盘上有、没人认领」的孤儿，被下一轮清扫收走。
func TestReaperClaimsBeforeDiskThenCommits(t *testing.T) {
	src := sourceRepo(t)
	wm := newManager(t)
	ctx := context.Background()

	wt, err := wm.Create(ctx, CreateParams{
		Repo: DefaultRepoConfig("acme/demo"), CloneURL: src,
		Kind: KindFix, IssueKey: "CR-400", Title: "t", TaskID: 400,
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	chtimes(t, wt.Path)

	ft := &fakeReaperTasks{
		reapable: []*task.Task{terminalTask(1, wt.Path, wt.Branch, task.StateMerged)},
	}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	if _, err := r.ReapOnce(ctx, time.Hour); err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	if len(ft.claimCalls) != 2 {
		t.Fatalf("应两阶段认领（校验 + 落账），实际 %d 次: %+v", len(ft.claimCalls), ft.claimCalls)
	}
	if ft.claimCalls[0].commit {
		t.Error("第一次认领应是只读校验（commit=false）")
	}
	if !ft.claimCalls[1].commit {
		t.Error("第二次认领应是落账（commit=true）")
	}
}

// ---------------------------------------------------------------- B3 mtime/dirty 闸

// ★ B3：主路径的 mtime 闸 —— 任务行超期但目录还新时不删。
//
// 孤儿清扫那侧看了 mtime，主路径此前不看，两条路径安全标准不对称。
// 目录很新意味着「刚被 Create 重建过」，任务行的 updated_at 落后于事实。
func TestReaperSkipsFreshDirEvenWhenTaskRowIsStale(t *testing.T) {
	wm, root := reaperEnv(t)
	p := filepath.Join(root, "cr-500")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	// mtime 就是现在 —— 目录刚被建/改过
	ft := &fakeReaperTasks{
		reapable: []*task.Task{terminalTask(1, p, "fix/cr-500", task.StateFailed)},
	}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	n, err := r.ReapOnce(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	if n != 0 {
		t.Errorf("目录 mtime 未超期时不该回收，得到 %d", n)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("B3：mtime 还新的目录被删了: %v", err)
	}
	if len(ft.claimCalls) != 0 {
		t.Errorf("mtime 闸应在认领之前就拦住，实际 claimCalls=%+v", ft.claimCalls)
	}
}

// 任务行本身没超期时也不删（cutoff 是两把尺子共同的基准）。
func TestReaperSkipsWhenTaskRowNotStale(t *testing.T) {
	wm, root := reaperEnv(t)
	p := filepath.Join(root, "cr-501")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	chtimes(t, p) // 目录很旧

	tk := terminalTask(1, p, "fix/cr-501", task.StateFailed)
	tk.UpdatedAt = time.Now() // 但任务行刚被动过
	ft := &fakeReaperTasks{reapable: []*task.Task{tk}}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	n, err := r.ReapOnce(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	if n != 0 {
		t.Errorf("任务行未超期时不该回收，得到 %d", n)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("任务行刚被动过的现场被删了: %v", err)
	}
}

// ★ B3：脏工作区（有未提交改动）只告警不删 —— D4「保留现场」的落实。
//
// worktree.go 的 Remove(force=false) 注释写得很明白：git 拒绝删除有未提交
// 改动的工作区，「这正是失败任务保留现场（D4）所需的保护」。收割机走
// Discard（--force）绕开了它，所以要在收割机这一侧补一道闸。
func TestReaperHoldsDirtyWorktree(t *testing.T) {
	src := sourceRepo(t)
	wm := newManager(t)
	ctx := context.Background()

	wt, err := wm.Create(ctx, CreateParams{
		Repo: DefaultRepoConfig("acme/demo"), CloneURL: src,
		Kind: KindFix, IssueKey: "CR-600", Title: "t", TaskID: 600,
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	// 未提交改动：agent 中断的半成品，或人手工介入
	wip := filepath.Join(wt.Path, "wip.txt")
	if err := os.WriteFile(wip, []byte("half done\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	chtimes(t, wt.Path)

	ft := &fakeReaperTasks{
		reapable: []*task.Task{terminalTask(1, wt.Path, wt.Branch, task.StateFailed)},
	}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	if _, err := r.ReapOnce(ctx, time.Hour); err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	if _, err := os.Stat(wip); err != nil {
		t.Errorf("B3：脏现场被删了！未提交改动没了（D4 保护被绕过）: %v", err)
	}
	if out := gitOut(t, wt.Mirror, "branch", "--list", wt.Branch); !strings.Contains(out, wt.Branch) {
		t.Errorf("脏现场的分支也不该删，实际 branch --list = %q", out)
	}
	// 保留现场时**不能**落账：worktree_path 必须留着，否则这份现场
	// 会变成「磁盘上有、没人认领」的孤儿，被清扫收走。
	if len(ft.cleared) != 0 {
		t.Errorf("保留现场时不该置空 worktree_path，实际 cleared=%v", ft.cleared)
	}
}

// ★ B3：分支有未推送提交时，只删目录、保留分支。
//
// 目录可以删（内容在分支里），但分支是那些提交的唯一副本。
func TestReaperKeepsBranchWithUnpushedCommits(t *testing.T) {
	src := sourceRepo(t)
	wm := newManager(t)
	ctx := context.Background()

	wt, err := wm.Create(ctx, CreateParams{
		Repo: DefaultRepoConfig("acme/demo"), CloneURL: src,
		Kind: KindFix, IssueKey: "CR-700", Title: "t", TaskID: 700,
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	// 有提交但没推送
	if err := os.WriteFile(filepath.Join(wt.Path, "done.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := wm.Commit(ctx, wt, "fix: 做完了但没推"); err != nil {
		t.Fatalf("Commit 失败: %v", err)
	}
	chtimes(t, wt.Path)

	ft := &fakeReaperTasks{
		reapable: []*task.Task{terminalTask(1, wt.Path, wt.Branch, task.StateFailed)},
	}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	n, err := r.ReapOnce(ctx, time.Hour)
	if err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	if n != 1 {
		t.Errorf("目录应被回收，得到 %d", n)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Errorf("目录应被删掉，实际仍在（err=%v）", err)
	}
	if out := gitOut(t, wt.Mirror, "branch", "--list", wt.Branch); !strings.Contains(out, wt.Branch) {
		t.Errorf("B3：有未推送提交的分支被删了！那些提交是唯一副本: %q", out)
	}
}

// 干净、无未推送提交的现场：目录与分支都删。
func TestReaperRemovesCleanWorktreeAndBranch(t *testing.T) {
	src := sourceRepo(t)
	wm := newManager(t)
	ctx := context.Background()

	wt, err := wm.Create(ctx, CreateParams{
		Repo: DefaultRepoConfig("acme/demo"), CloneURL: src,
		Kind: KindFix, IssueKey: "CR-800", Title: "t", TaskID: 800,
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	chtimes(t, wt.Path)

	ft := &fakeReaperTasks{
		reapable: []*task.Task{terminalTask(1, wt.Path, wt.Branch, task.StateMerged)},
	}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	n, err := r.ReapOnce(ctx, time.Hour)
	if err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	if n != 1 {
		t.Errorf("干净现场应被回收，得到 %d", n)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Errorf("目录应被删掉，实际仍在（err=%v）", err)
	}
	if out := gitOut(t, wt.Mirror, "branch", "--list", wt.Branch); strings.TrimSpace(out) != "" {
		t.Errorf("干净现场的分支应被删掉，实际 branch --list = %q", out)
	}
	if len(ft.cleared) != 1 {
		t.Errorf("回收完成应落账一次，实际 cleared=%v", ft.cleared)
	}
}

// ---------------------------------------------------------------- B4 干跑

// ★ B4：干跑模式什么都不删、什么都不改，只打日志。
//
// 「先清一批存量目录」是很自然的运维动作，而正确的做法是先干跑一轮
// 看清候选，而不是把 TTL 配成 1m。
func TestReaperDryRunTouchesNothing(t *testing.T) {
	wm, root := reaperEnv(t)
	main := filepath.Join(root, "cr-900")
	orphan := filepath.Join(root, "cr-901-orphan")
	for _, p := range []string{main, orphan} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		chtimes(t, p)
	}

	ft := &fakeReaperTasks{
		reapable: []*task.Task{terminalTask(1, main, "fix/cr-900", task.StateFailed)},
	}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm, DryRun: true,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	if _, err := r.ReapOnce(context.Background(), time.Hour); err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	for _, p := range []string{main, orphan} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("B4：干跑模式删了目录 %s: %v", p, err)
		}
	}
	if len(ft.claimCalls) != 0 {
		t.Errorf("干跑不该认领任何任务行，实际 claimCalls=%+v", ft.claimCalls)
	}
	if len(ft.cleared) != 0 {
		t.Errorf("干跑不该改任何状态，实际 cleared=%v", ft.cleared)
	}
}

// ---------------------------------------------------------------- symlink 越界

// ★ symlink 越界：<root>/<name> 是指向根外的符号链接时必须拒绝。
//
// filepath.Rel 是纯字符串运算，看不见符号链接：一条 <root>/evil →
// /etc 的链接会被判成「在根下」，而 os.RemoveAll 会顺着它删到根外。
func TestReaperRejectsSymlinkEscape(t *testing.T) {
	wm, root := reaperEnv(t)
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("根目录之外的东西\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(root, "cr-evil")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("本环境不支持 symlink: %v", err)
	}
	chtimes(t, outside)

	ft := &fakeReaperTasks{
		reapable: []*task.Task{terminalTask(1, link, "", task.StateFailed)},
	}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	if _, err := r.ReapOnce(context.Background(), time.Hour); err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("symlink 越界：根目录之外的文件被删了！%v", err)
	}
	if len(ft.claimCalls) != 0 {
		t.Errorf("可疑路径不该走到认领，实际 claimCalls=%+v", ft.claimCalls)
	}
}

// 中间段是 symlink 的路径同样拒绝（<root>/link/sub）。
func TestReaperRejectsSymlinkInMiddleSegment(t *testing.T) {
	wm, root := reaperEnv(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "sub")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(victim, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(outside, filepath.Join(root, "hop")); err != nil {
		t.Skipf("本环境不支持 symlink: %v", err)
	}
	chtimes(t, victim)

	ft := &fakeReaperTasks{
		reapable: []*task.Task{terminalTask(1, filepath.Join(root, "hop", "sub"), "", task.StateFailed)},
	}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	if _, err := r.ReapOnce(context.Background(), time.Hour); err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("中间段 symlink 越界：根外文件被删了！%v", err)
	}
}

// 工作区根目录自己是 symlink 时，正常路径不能被误判成「不在根下」。
//
// /opt/lathe/workspaces 指向另一块盘是很常见的挂载手法；只解析候选路径
// 而不解析 root，会让每一条正常路径都被拒绝，收割机彻底罢工。
func TestReaperWorksWhenRootIsSymlink(t *testing.T) {
	real := t.TempDir()
	linkRoot := filepath.Join(t.TempDir(), "workspaces")
	if err := os.Symlink(real, linkRoot); err != nil {
		t.Skipf("本环境不支持 symlink: %v", err)
	}
	wm, err := NewWorktreeManager(linkRoot)
	if err != nil {
		t.Fatalf("NewWorktreeManager 失败: %v", err)
	}

	p := filepath.Join(linkRoot, "cr-1000")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	chtimes(t, p)

	ft := &fakeReaperTasks{
		reapable: []*task.Task{terminalTask(1, p, "", task.StateFailed)},
	}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	if _, err := r.ReapOnce(context.Background(), time.Hour); err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	// 该走到认领（说明 safeToRemove 没把它误拒）
	if len(ft.claimCalls) == 0 {
		t.Error("root 是 symlink 时正常路径被误判为可疑，收割机会彻底罢工")
	}
}

// ---------------------------------------------------------------- 整轮容错

// 查活任务认领失败时，整轮不动主路径 —— 判据不全会误删在途现场。
func TestReaperSkipsMainPathWhenClaimedLookupFails(t *testing.T) {
	wm, root := reaperEnv(t)
	p := filepath.Join(root, "cr-1100")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	chtimes(t, p)

	ft := &fakeReaperTasks{
		reapable: []*task.Task{terminalTask(1, p, "fix/cr-1100", task.StateFailed)},
		claimErr: errors.New("数据库抖了一下"),
	}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	n, err := r.ReapOnce(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("认领路径查询失败不该让整轮报错: %v", err)
	}
	if n != 0 {
		t.Errorf("判据拿不到时不该回收，得到 %d", n)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("判据不全时删了目录: %v", err)
	}
}

// ---------------------------------------------------------------- 两条路径的分工

// ★ 守卫：主路径跳过的现场，绝不能被同一轮的孤儿清扫收走。
//
// 这条语义是 B1/B2/B3 全部修复的隐含前提，此前没有任何测试保护它。
// ReapOnce 一轮里跑两条路径：
//
//	reapTask     —— 数据库驱动，按状态 + 双 mtime + 认领判「能不能删」
//	sweepOrphans —— 磁盘驱动，按「有没有任何任务行指着 + mtime」判「是不是无主」
//
// 若第二条的「有主」判据漏掉了终态行，那么第一条每一次正确的「跳过」
// 都会被第二条当成「无主目录」补刀 —— 认领守卫、脏现场保护、mtime 闸
// 全部形同虚设。所以 ReferencedWorktreePaths 必须**不分状态**地包含
// 所有引用（这正是另开 ClaimedWorktreePaths 只算非终态的理由）。
func TestReaperSkippedSceneIsNotSweptAsOrphan(t *testing.T) {
	// 四种「主路径会跳过」的成因，逐一确认目录都还在
	cases := []struct {
		name  string
		setup func(p string, tk *task.Task) *fakeReaperTasks
	}{
		{
			name: "认领失配（人点了重试）",
			setup: func(p string, tk *task.Task) *fakeReaperTasks {
				return &fakeReaperTasks{reapable: []*task.Task{tk}, claimFail: true}
			},
		},
		{
			name: "认领查询报错",
			setup: func(p string, tk *task.Task) *fakeReaperTasks {
				return &fakeReaperTasks{reapable: []*task.Task{tk}, claimErr2: errors.New("库抖了")}
			},
		},
		{
			name: "路径被在途任务占用",
			setup: func(p string, tk *task.Task) *fakeReaperTasks {
				return &fakeReaperTasks{reapable: []*task.Task{tk}, claimed: []string{p}}
			},
		},
		{
			name: "活任务认领查询失败（整轮不动主路径）",
			setup: func(p string, tk *task.Task) *fakeReaperTasks {
				return &fakeReaperTasks{reapable: []*task.Task{tk}, claimErr: errors.New("库抖了")}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wm, root := reaperEnv(t)
			p := filepath.Join(root, "cr-guard")
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(p, "keep.txt")
			if err := os.WriteFile(sentinel, []byte("这份现场必须留住\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// mtime 推到很久以前 —— 孤儿清扫的 mtime 闸拦不住它，
			// 唯一能拦住的就是「有任务行指着它」。
			chtimes(t, p)

			tk := terminalTask(1, p, "fix/cr-guard", task.StateFailed)
			ft := tc.setup(p, tk)
			r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
				RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) {
					return DefaultRepoConfig("acme/demo"), nil
				}}

			n, err := r.ReapOnce(context.Background(), time.Hour)
			if err != nil {
				t.Fatalf("ReapOnce 失败: %v", err)
			}
			if n != 0 {
				t.Errorf("主路径跳过时本轮不该有任何回收，得到 %d", n)
			}
			if _, serr := os.Stat(sentinel); serr != nil {
				t.Errorf("主路径跳过的现场被孤儿清扫收走了！认领/脏/mtime 守卫全被绕过: %v", serr)
			}
		})
	}
}

// 反面：任务行真的不指着它时，孤儿清扫照样该收 —— 上一条不能是
// 「孤儿清扫整体失灵」造成的假阳性。
func TestReaperStillSweepsTrulyUnreferencedDir(t *testing.T) {
	wm, root := reaperEnv(t)
	orphan := filepath.Join(root, "cr-really-orphan")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	chtimes(t, orphan)

	// 候选里有一条任务，但它指着**另一条**路径
	other := filepath.Join(root, "cr-other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	ft := &fakeReaperTasks{
		reapable:  []*task.Task{terminalTask(1, other, "fix/cr-other", task.StateFailed)},
		claimFail: true, // 让主路径跳过，把舞台留给孤儿清扫
	}
	r := &WorktreeReaper{Tasks: ft, Worktrees: wm,
		RepoLookup: func(ctx context.Context, id int64) (RepoConfig, error) { return DefaultRepoConfig("acme/demo"), nil }}

	if _, err := r.ReapOnce(context.Background(), time.Hour); err != nil {
		t.Fatalf("ReapOnce 失败: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("真正无人引用的陈旧目录应被清掉，实际 err=%v", err)
	}
	// 被任务行指着的那条仍在（哪怕主路径跳过了它）
	if _, err := os.Stat(other); err != nil {
		t.Errorf("有任务行指着的目录不该被孤儿清扫收走: %v", err)
	}
}
