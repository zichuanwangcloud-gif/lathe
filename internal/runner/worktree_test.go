package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sourceRepo 造一个带 dev/main 两条分支的本地仓库，充当远端。
func sourceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@e.st",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@e.st",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s 失败: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	run("init", "--quiet", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# src\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "--quiet", "-m", "初始提交")
	run("branch", "dev")
	return dir
}

func newManager(t *testing.T) *WorktreeManager {
	t.Helper()
	m, err := NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewWorktreeManager 失败: %v", err)
	}
	return m
}

func TestNewWorktreeManagerRejectsRelativePath(t *testing.T) {
	if _, err := NewWorktreeManager("relative/path"); err == nil {
		t.Error("相对路径应被拒绝")
	}
}

func TestEnsureMirrorCloneThenFetch(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()

	mirror, err := m.EnsureMirror(ctx, "acme/demo", src)
	if err != nil {
		t.Fatalf("首次 EnsureMirror 失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err != nil {
		t.Errorf("mirror 应是有效的 bare 仓库: %v", err)
	}

	// 再次调用应走 fetch 分支且不报错（幂等）
	mirror2, err := m.EnsureMirror(ctx, "acme/demo", src)
	if err != nil {
		t.Fatalf("二次 EnsureMirror 失败: %v", err)
	}
	if mirror2 != mirror {
		t.Errorf("mirror 路径应稳定: %q vs %q", mirror, mirror2)
	}

	if _, err := m.EnsureMirror(ctx, "acme/demo", ""); err == nil {
		t.Error("空 clone URL 应报错")
	}

	// 远端分支应落在 refs/remotes/origin/* 命名空间（任务分支住
	// refs/heads/*，两个命名空间隔离，prune 互不相扰）
	gitOut(t, mirror, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/dev^{commit}")
}

// 回归 #494：并发任务启动触发的 fetch --prune 不得剪掉在途任务分支。
//
// 旧实现的 fetch refspec 是 +refs/heads/*:refs/heads/*，--prune 会把
// 「远端不存在」的本地任务分支（即使正被 worktree 占用）删掉。任务
// 流水线随后的 commit 落在不存在的 ref 上，变成无父 root commit，
// ChangedFiles 的 diff base...HEAD 报 no merge base，任务含冤失败。
func TestEnsureMirrorFetchPruneKeepsInflightTaskBranch(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()
	repo := DefaultRepoConfig("acme/demo")

	wt, err := m.Create(ctx, CreateParams{
		Repo: repo, CloneURL: src, Kind: KindFix, IssueKey: "CR-494", Title: "t",
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	// 模拟第二个任务在同一仓库上启动：EnsureMirror 会 fetch --prune
	if _, err := m.EnsureMirror(ctx, "acme/demo", src); err != nil {
		t.Fatalf("并发 EnsureMirror 失败: %v", err)
	}

	// 在途任务分支必须还活着
	if out := gitOut(t, wt.Mirror, "branch", "--list", wt.Branch); !strings.Contains(out, wt.Branch) {
		t.Fatalf("fetch --prune 后在途任务分支 %s 被删除（#494 根因）", wt.Branch)
	}

	// 提交改动并 diff 基线：必须正常工作，不能退化成 root commit
	if err := os.WriteFile(filepath.Join(wt.Path, "fix.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Commit(ctx, wt, "fix: test"); err != nil {
		t.Fatalf("Commit 失败: %v", err)
	}
	files, err := m.ChangedFiles(ctx, wt)
	if err != nil {
		t.Fatalf("ChangedFiles 失败（#494 的报错点）: %v", err)
	}
	if len(files) != 1 || files[0] != "fix.txt" {
		t.Errorf("ChangedFiles = %v，期望 [fix.txt]", files)
	}
}

func TestCreateWorktreeUsesCorrectBase(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()
	repo := DefaultRepoConfig("acme/demo")

	cases := []struct {
		kind     TaskKind
		issueKey string
		wantBase string
		wantPfx  string
	}{
		{KindFix, "CR-100", "dev", "fix/"},
		{KindFeature, "CR-101", "dev", "feature/"},
		{KindHotfix, "CR-102", "main", "hotfix/"},
	}

	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			wt, err := m.Create(ctx, CreateParams{
				Repo: repo, CloneURL: src, Kind: tc.kind,
				IssueKey: tc.issueKey, Title: "some thing",
			})
			if err != nil {
				t.Fatalf("Create 失败: %v", err)
			}
			if wt.BaseBranch != tc.wantBase {
				t.Errorf("基线 = %q，期望 %q", wt.BaseBranch, tc.wantBase)
			}
			if !strings.HasPrefix(wt.Branch, tc.wantPfx) {
				t.Errorf("分支 %q 应以 %q 开头", wt.Branch, tc.wantPfx)
			}
			if _, err := os.Stat(filepath.Join(wt.Path, "README.md")); err != nil {
				t.Errorf("工作区应已 checkout 出文件: %v", err)
			}

			// 工作区里当前分支应确实是新建的任务分支
			out, err := exec.Command("git", "-C", wt.Path, "branch", "--show-current").Output()
			if err != nil {
				t.Fatalf("读取当前分支失败: %v", err)
			}
			if got := strings.TrimSpace(string(out)); got != wt.Branch {
				t.Errorf("工作区当前分支 = %q，期望 %q", got, wt.Branch)
			}
		})
	}
}

func TestCreateRejectsMissingBaseBranch(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)

	repo := DefaultRepoConfig("acme/demo")
	repo.DefaultBranch = "nonexistent"

	_, err := m.Create(context.Background(), CreateParams{
		Repo: repo, CloneURL: src, Kind: KindFix, IssueKey: "CR-1", Title: "t",
	})
	if err == nil {
		t.Fatal("基线分支不存在时应报错")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("错误应说明基线分支不存在，得到: %v", err)
	}
}

// 尸体回收：同 issue 的下一次尝试接管槽位，而不是被上一任务按 D4
// 保留的现场永久卡死（任务 #345/#466）。D4 语义不变 —— 现场一直留到
// 下一次尝试需要这个槽位为止。
func TestCreateReclaimsCorpseWorktree(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()
	repo := DefaultRepoConfig("acme/demo")

	p := CreateParams{Repo: repo, CloneURL: src, Kind: KindFix, IssueKey: "CR-1", Title: "t"}
	wt1, err := m.Create(ctx, p)
	if err != nil {
		t.Fatalf("首次 Create 失败: %v", err)
	}
	// 留下失败任务现场：未提交改动 + 已注册 worktree + 任务分支
	if err := os.WriteFile(filepath.Join(wt1.Path, "wip.txt"), []byte("half done\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wt2, err := m.Create(ctx, p)
	if err != nil {
		t.Fatalf("尸体存在时 Create 应回收重建，得到错误: %v", err)
	}
	if wt2.Path != wt1.Path || wt2.Branch != wt1.Branch {
		t.Errorf("同 issue 应复用同一槽位: %q/%q vs %q/%q", wt2.Path, wt2.Branch, wt1.Path, wt1.Branch)
	}
	if _, err := os.Stat(filepath.Join(wt2.Path, "wip.txt")); !os.IsNotExist(err) {
		t.Error("旧现场的未提交改动应随尸体一起被回收")
	}
	// 重建的分支应干净地停在基线上（旧分支尸体被删除后重建）
	out, err := exec.Command("git", "-C", wt2.Path, "rev-list", "--count",
		MirrorBaseRef(wt2.BaseBranch)+"..HEAD").Output()
	if err != nil {
		t.Fatalf("统计提交失败: %v", err)
	}
	if strings.TrimSpace(string(out)) != "0" {
		t.Errorf("重建分支不应带有旧提交，得到 %s", out)
	}
}

// 分支尸体：目录被人手工删掉后，refs/heads/<branch> 还在，
// worktree add -b 会撞 branch already exists —— 同样要回收。
func TestCreateReclaimsBranchOnlyCorpse(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()
	repo := DefaultRepoConfig("acme/demo")

	p := CreateParams{Repo: repo, CloneURL: src, Kind: KindFix, IssueKey: "CR-1", Title: "t"}
	wt1, err := m.Create(ctx, p)
	if err != nil {
		t.Fatalf("首次 Create 失败: %v", err)
	}
	if err := os.RemoveAll(wt1.Path); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Create(ctx, p); err != nil {
		t.Fatalf("仅剩分支尸体时 Create 应回收重建，得到错误: %v", err)
	}
}

// 普通目录尸体：路径存在但不是注册的 worktree（半残/手工创建），
// git worktree remove 拒绝删除，需兜底 RemoveAll 后才能重建。
func TestCreateReclaimsPlainDirectoryCorpse(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()
	repo := DefaultRepoConfig("acme/demo")

	p := CreateParams{Repo: repo, CloneURL: src, Kind: KindFix, IssueKey: "CR-1", Title: "t"}
	corpse := filepath.Join(m.Root(), "cr-1")
	if err := os.MkdirAll(corpse, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corpse, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	wt, err := m.Create(ctx, p)
	if err != nil {
		t.Fatalf("普通目录尸体时 Create 应回收重建，得到错误: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt.Path, "stray.txt")); !os.IsNotExist(err) {
		t.Error("尸体目录的内容应被清除")
	}
	if _, err := os.Stat(filepath.Join(wt.Path, "README.md")); err != nil {
		t.Errorf("新工作区应已 checkout 出基线文件: %v", err)
	}
}

// D4「保留现场」：有未提交改动时，非 force 回收必须失败。
func TestRemovePreservesDirtyWorktree(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()

	wt, err := m.Create(ctx, CreateParams{
		Repo: DefaultRepoConfig("acme/demo"), CloneURL: src,
		Kind: KindFix, IssueKey: "CR-1", Title: "t",
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	// 制造未提交改动，模拟失败任务留下的现场
	if err := os.WriteFile(filepath.Join(wt.Path, "wip.txt"), []byte("half done\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := m.Remove(ctx, wt, false); err == nil {
		t.Error("有未提交改动时非 force 回收应失败（失败任务要保留现场）")
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Errorf("被拒绝回收后工作区应仍在: %v", err)
	}

	// 成功任务才用 force 回收
	if err := m.Remove(ctx, wt, true); err != nil {
		t.Fatalf("force 回收应成功: %v", err)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Errorf("force 回收后工作区应已删除")
	}
}

func TestRemoveCleanWorktreeAndBranch(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()

	wt, err := m.Create(ctx, CreateParams{
		Repo: DefaultRepoConfig("acme/demo"), CloneURL: src,
		Kind: KindFix, IssueKey: "CR-1", Title: "clean",
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	if err := m.Remove(ctx, wt, true); err != nil {
		t.Fatalf("回收失败: %v", err)
	}

	// 任务分支也应被删掉，不在 mirror 里堆积
	out, _ := exec.Command("git", "-C", wt.Mirror, "branch", "--list", wt.Branch).Output()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("任务分支应被删除，仍存在: %q", string(out))
	}

	if err := m.Remove(ctx, nil, true); err == nil {
		t.Error("传 nil 工作区应报错")
	}
}

func TestListAndPrune(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()
	repo := DefaultRepoConfig("acme/demo")

	// 未建 mirror 时不应报错
	if paths, err := m.List(ctx, "never/seen"); err != nil || paths != nil {
		t.Errorf("未知仓库 List 应返回 (nil, nil)，得到 (%v, %v)", paths, err)
	}
	if err := m.Prune(ctx, "never/seen"); err != nil {
		t.Errorf("未知仓库 Prune 不应报错: %v", err)
	}

	wt1, err := m.Create(ctx, CreateParams{Repo: repo, CloneURL: src, Kind: KindFix, IssueKey: "CR-1", Title: "a"})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	if _, err := m.Create(ctx, CreateParams{Repo: repo, CloneURL: src, Kind: KindFix, IssueKey: "CR-2", Title: "b"}); err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	paths, err := m.List(ctx, "acme/demo")
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(paths) != 2 {
		t.Errorf("应列出 2 个工作区，得到 %d: %v", len(paths), paths)
	}
	// bare mirror 自身不应出现在列表里
	for _, p := range paths {
		if filepath.Clean(p) == filepath.Clean(m.MirrorPath("acme/demo")) {
			t.Errorf("列表中不应包含 bare mirror 自身: %s", p)
		}
	}

	// 目录被外部删除后，prune 应清掉注册记录
	if err := os.RemoveAll(wt1.Path); err != nil {
		t.Fatal(err)
	}
	if err := m.Prune(ctx, "acme/demo"); err != nil {
		t.Fatalf("Prune 失败: %v", err)
	}
	paths, _ = m.List(ctx, "acme/demo")
	if len(paths) != 1 {
		t.Errorf("prune 后应只剩 1 个工作区，得到 %d: %v", len(paths), paths)
	}
}

func TestMirrorPathIsStableAndSafe(t *testing.T) {
	m := newManager(t)
	p := m.MirrorPath("Clouditera/CloudRouter")

	if strings.Contains(filepath.Base(p), "/") {
		t.Errorf("mirror 目录名不应含路径分隔符: %q", p)
	}
	if !strings.HasSuffix(p, ".git") {
		t.Errorf("mirror 路径应以 .git 结尾: %q", p)
	}
	if p != m.MirrorPath("Clouditera/CloudRouter") {
		t.Error("同一仓库的 mirror 路径应稳定")
	}
	if p == m.MirrorPath("Other/CloudRouter") {
		t.Error("不同 owner 的仓库不应映射到同一 mirror")
	}
}
