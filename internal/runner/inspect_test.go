package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Inspect 是智能重试的现场体检：每个字段都决定决策走向，逐项覆盖。
func TestInspectWorktreeStates(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()
	repo := DefaultRepoConfig("acme/demo")

	wt, err := m.Create(ctx, CreateParams{
		Repo: repo, CloneURL: src, Kind: KindFix, IssueKey: "CR-900", Title: "t",
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	// 1) 刚建好：存在、已注册、分支在、无提交、干净
	st := m.Inspect(ctx, "acme/demo", wt.Path, wt.Branch, wt.BaseBranch)
	if !st.Usable() {
		t.Fatalf("新建工作区应可用: %+v", st)
	}
	if st.HasCommits || st.Dirty || st.RemoteBranch || st.Commits != 0 {
		t.Errorf("新建工作区应无提交、干净、未推送: %+v", st)
	}

	// 2) 未提交改动 → Dirty
	if err := os.WriteFile(filepath.Join(wt.Path, "wip.txt"), []byte("half\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st := m.Inspect(ctx, "acme/demo", wt.Path, wt.Branch, wt.BaseBranch); !st.Dirty {
		t.Error("有未提交改动应 Dirty")
	}

	// 3) 提交后 → HasCommits 且不再 Dirty
	if err := m.Commit(ctx, wt, "wip"); err != nil {
		t.Fatalf("Commit 失败: %v", err)
	}
	st = m.Inspect(ctx, "acme/demo", wt.Path, wt.Branch, wt.BaseBranch)
	if !st.HasCommits || st.Commits != 1 || st.Dirty {
		t.Errorf("提交后应有 1 个提交且干净: %+v", st)
	}

	// 4) 目录被手动删掉 → 全部不可用（注册残留不算可用）
	if err := m.Remove(ctx, wt, true); err != nil {
		t.Fatalf("Remove 失败: %v", err)
	}
	st = m.Inspect(ctx, "acme/demo", wt.Path, wt.Branch, wt.BaseBranch)
	if st.Usable() {
		t.Errorf("工作区删除后应不可用: %+v", st)
	}

	// 5) 路径为空 / 不存在：零值，不报错
	if st := m.Inspect(ctx, "acme/demo", "", "", "dev"); st.Usable() {
		t.Error("空路径应不可用")
	}
	if st := m.Inspect(ctx, "acme/demo", filepath.Join(t.TempDir(), "nope"), "x", "dev"); st.Exists {
		t.Error("不存在的路径应 Exists=false")
	}
}

// HasCommitsAhead 区分「没干活」与「已提交过」。
func TestHasCommitsAhead(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()

	wt, err := m.Create(ctx, CreateParams{
		Repo: DefaultRepoConfig("acme/demo"), CloneURL: src,
		Kind: KindFix, IssueKey: "CR-901", Title: "t",
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	ahead, err := m.HasCommitsAhead(ctx, wt)
	if err != nil {
		t.Fatalf("HasCommitsAhead 失败: %v", err)
	}
	if ahead {
		t.Error("新分支不应有领先提交")
	}

	if err := os.WriteFile(filepath.Join(wt.Path, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Commit(ctx, wt, "add a"); err != nil {
		t.Fatal(err)
	}
	ahead, err = m.HasCommitsAhead(ctx, wt)
	if err != nil {
		t.Fatalf("HasCommitsAhead 失败: %v", err)
	}
	if !ahead {
		t.Error("提交后应有领先提交")
	}
}
