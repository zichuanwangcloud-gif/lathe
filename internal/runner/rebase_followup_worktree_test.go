package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件测 F4.3 rebase 跟进在 WorktreeManager 这一层新增的两个方法：
// RebaseOnto（改写历史）与 ForcePush（推送改写后的历史）。全部用真实
// git 命令 + 本地仓库当"远端"，不 fake git 操作——这条链路全是真实
// git 命令，fake 测不出 rebase 是否真的对（见任务说明）。

// TestWorktreeManagerRebaseOnto 验证 RebaseOnto 的核心效果：
//
//   - "新基线"（dev）通过 cherry-pick 拿到了"旧基线"（pred 分支）的
//     内容，但走的是一条完全不同的历史（不同的 commit，模拟真实世界
//     squash-merge 之后 default 分支的提交跟前驱分支的原始提交不是
//     同一个 commit，只是内容等价）；
//   - 任务分支（succ）在 pred 分支的旧 tip 之上加了自己的改动；
//   - RebaseOnto 之后：succ 的历史确实基于 dev 的新 tip（祖先关系）、
//     succ 自己的改动（succ.txt）还在工作区里。
func TestWorktreeManagerRebaseOnto(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()
	repo := DefaultRepoConfig("acme/demo")

	// ---- 旧基线：pred 分支，加一个提交 ----
	predWT, err := m.Create(ctx, CreateParams{
		Repo: repo, CloneURL: src, Kind: KindFix, IssueKey: "CR-500", Title: "pred",
	})
	if err != nil {
		t.Fatalf("创建 pred 工作区失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(predWT.Path, "pred.txt"), []byte("pred 内容\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, predWT.Path, "add", ".")
	gitOut(t, predWT.Path, "-c", "user.email=t@e.st", "-c", "user.name=t", "commit", "-qm", "pred 的改动")
	oldBaseTip := gitOut(t, predWT.Path, "rev-parse", "HEAD")

	if err := m.Push(ctx, predWT, repo, nil); err != nil {
		t.Fatalf("推送 pred 分支失败: %v", err)
	}

	// ---- 任务分支：从 pred 分支分叉，加自己的改动 ----
	succRepo := repo
	succRepo.BaseRefOverride = predWT.Branch
	succWT, err := m.Create(ctx, CreateParams{
		Repo: succRepo, CloneURL: src, Kind: KindFix, IssueKey: "CR-501", Title: "succ",
	})
	if err != nil {
		t.Fatalf("创建 succ 工作区失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(succWT.Path, "succ.txt"), []byte("succ 自己的改动\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, succWT.Path, "add", ".")
	gitOut(t, succWT.Path, "-c", "user.email=t@e.st", "-c", "user.name=t", "commit", "-qm", "succ 的改动")

	// ---- 新基线：在 src 上把 dev 推进——cherry-pick pred 的提交，
	// 制造"内容等价但完全是另一个 commit"的新历史（模拟 squash-merge）----
	gitOut(t, src, "checkout", "-q", "dev")
	// -x：在提交信息里附一行 "cherry picked from commit ..."，确保这个
	// 新 commit 的对象哈希必然与 pred 原提交不同（哪怕树内容、父提交、
	// 作者信息都相同，消息不同也会让哈希不同）——避免在执行飞快、
	// 秒级时间戳可能碰巧重合的极端情况下被误判成"同一个 commit"。
	gitOut(t, src, "-c", "user.email=t@e.st", "-c", "user.name=t", "cherry-pick", "-x", oldBaseTip)
	newDevTip := gitOut(t, src, "rev-parse", "dev")
	if newDevTip == oldBaseTip {
		t.Fatal("cherry-pick 之后 dev 的 tip 应是一个全新的 commit，不应等于 pred 原 tip")
	}

	// ---- 执行 RebaseOnto ----
	if err := m.RebaseOnto(ctx, succWT, oldBaseTip, "dev"); err != nil {
		t.Fatalf("RebaseOnto 失败: %v", err)
	}

	// ---- 断言一：succ 的历史确实基于 dev 的新 tip（祖先关系） ----
	if _, err := m.git(ctx, succWT.Path, "merge-base", "--is-ancestor", newDevTip, "HEAD"); err != nil {
		t.Errorf("rebase 后 succ 的历史应以 dev 新 tip(%s) 为祖先，实际不是: %v", newDevTip[:8], err)
	}
	// 且不再以 pred 的旧提交为直接祖先链上唯一依据——dev 新 tip 是
	// 另一个 commit，succ 现在的第一个"基线"提交应是 newDevTip 或其
	// 后代，而不再是 oldBaseTip 本身残留在历史里作为分叉点。
	out := gitOut(t, succWT.Path, "log", "--format=%H", "HEAD")
	if !strings.Contains(out, newDevTip) {
		t.Errorf("rebase 后 succ 的提交历史里应包含新基线 tip %s，实际历史:\n%s", newDevTip, out)
	}

	// ---- 断言二：succ 自己的改动还在（文件内容还在工作区里） ----
	got, err := os.ReadFile(filepath.Join(succWT.Path, "succ.txt"))
	if err != nil {
		t.Fatalf("succ.txt 应仍存在于工作区: %v", err)
	}
	if string(got) != "succ 自己的改动\n" {
		t.Errorf("succ.txt 内容 = %q，期望保留原样", got)
	}
	// pred.txt（来自新基线）也应该在工作区里，证明确实基于新基线的内容
	if _, err := os.Stat(filepath.Join(succWT.Path, "pred.txt")); err != nil {
		t.Errorf("rebase 后工作区应包含来自新基线的 pred.txt: %v", err)
	}
}

// TestWorktreeManagerRebaseOntoConflict 验证冲突原样返回，不吞不重试。
func TestWorktreeManagerRebaseOntoConflict(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()
	repo := DefaultRepoConfig("acme/demo")

	wt, err := m.Create(ctx, CreateParams{
		Repo: repo, CloneURL: src, Kind: KindFix, IssueKey: "CR-510", Title: "t",
	})
	if err != nil {
		t.Fatalf("创建工作区失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "README.md"), []byte("任务分支改的内容\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, wt.Path, "add", ".")
	gitOut(t, wt.Path, "-c", "user.email=t@e.st", "-c", "user.name=t", "commit", "-qm", "改 README")
	oldBaseTip := gitOut(t, src, "rev-parse", "dev")

	// 新基线在同一个文件的同一处改了不同内容
	gitOut(t, src, "checkout", "-q", "dev")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("dev 上改的不同内容\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, src, "add", ".")
	gitOut(t, src, "-c", "user.email=t@e.st", "-c", "user.name=t", "commit", "-qm", "dev 冲突改动")

	err = m.RebaseOnto(ctx, wt, oldBaseTip, "dev")
	if err == nil {
		t.Fatal("同一处改了不同内容应产生 rebase 冲突，RebaseOnto 应报错")
	}
}

// TestWorktreeManagerForcePush 验证 ForcePush 能推送改写过的历史，而
// 普通 Push 会被 non-fast-forward 拒绝。
func TestWorktreeManagerForcePush(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()
	repo := DefaultRepoConfig("acme/demo")

	wt, err := m.Create(ctx, CreateParams{
		Repo: repo, CloneURL: src, Kind: KindFix, IssueKey: "CR-520", Title: "t",
	})
	if err != nil {
		t.Fatalf("创建工作区失败: %v", err)
	}
	gitOut(t, wt.Path, "-c", "user.email=t@e.st", "-c", "user.name=t",
		"commit", "-q", "--allow-empty", "-m", "第一版")
	if err := m.Push(ctx, wt, repo, nil); err != nil {
		t.Fatalf("首次 Push 失败: %v", err)
	}

	// 改写本地历史（模拟 rebase 之后的效果）
	gitOut(t, wt.Path, "-c", "user.email=t@e.st", "-c", "user.name=t",
		"commit", "-q", "--amend", "--allow-empty", "-m", "改写后的历史")
	rewrittenHead := gitOut(t, wt.Path, "rev-parse", "HEAD")

	// 普通 Push 应被拒绝（non-fast-forward）
	if err := m.Push(ctx, wt, repo, nil); err == nil {
		t.Fatal("改写历史后的普通 Push 应被拒绝")
	}

	// ForcePush 应成功
	if err := m.ForcePush(ctx, wt, repo, nil); err != nil {
		t.Fatalf("ForcePush 应成功: %v", err)
	}

	// 远端分支确实更新到了新历史：fetch 后镜像命名空间里的 tip 应等于
	// 改写后的 HEAD；顺带确认此时再来一次普通 Push 也不再被拒绝
	// （远端已与本地一致，"Everything up-to-date"）。
	if err := m.Push(ctx, wt, repo, nil); err != nil {
		t.Errorf("ForcePush 之后远端应已与本地一致，普通 Push 不应再被拒绝: %v", err)
	}
	remoteHead := gitOut(t, src, "rev-parse", wt.Branch)
	if remoteHead != rewrittenHead {
		t.Errorf("远端分支 tip = %s，期望等于改写后的本地 HEAD %s", remoteHead, rewrittenHead)
	}
}

// TestWorktreeManagerForcePushRejectsProtectedBranch 复用 Push 的保护
// 分支校验——ForcePush 是照抄 Push 的结构，这道防线不能漏。
func TestWorktreeManagerForcePushRejectsProtectedBranch(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()
	repo := DefaultRepoConfig("acme/demo")

	wt, err := m.Create(ctx, CreateParams{
		Repo: repo, CloneURL: src, Kind: KindFix, IssueKey: "CR-521", Title: "t",
	})
	if err != nil {
		t.Fatalf("创建工作区失败: %v", err)
	}
	tampered := *wt
	tampered.Branch = "dev"

	if err := m.ForcePush(ctx, &tampered, repo, nil); err == nil {
		t.Error("推送到受保护分支应被拒绝")
	}
	if err := m.ForcePush(ctx, nil, repo, nil); err == nil {
		t.Error("传 nil 工作区应报错")
	}
}
