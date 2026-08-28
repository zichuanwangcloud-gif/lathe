package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件用真实 git 操作（本地 bare repo 当 origin，不碰真 GitHub）验证
// docs/07-prd-orchestration.md F3（栈式 PR）base_ref 穿透机制确实产生了
// 正确的栈式结构。机制本身（RepoConfig.BaseRefOverride + BaseBranch 短路
// + 调度器 fillBaseRef）在上一阶段已实现，本文件只补验证，不改生产代码。
//
// 复用 worktree_test.go 的 sourceRepo/newManager 与 push_test.go 的 gitOut，
// 不新造测试基础设施。

// isAncestor 报告 ancestor 是否是 dir 仓库里 descendant 的祖先提交。
//
// git merge-base --is-ancestor 退出码语义：0 = 是祖先，1 = 不是祖先，
// 其余退出码代表命令本身出错（如对象不存在）——那种情况直接 t.Fatal，
// 不能悄悄归为「不是祖先」。
func isAncestor(t *testing.T, dir, ancestor, descendant string) bool {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "merge-base", "--is-ancestor", ancestor, descendant)
	err := cmd.Run()
	if err == nil {
		return true
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return false
	}
	t.Fatalf("git merge-base --is-ancestor %s %s 执行出错: %v", ancestor, descendant, err)
	return false
}

// TestStackedPRSuccessorAncestryIncludesPredecessorTip 验证 F3.1-AC2：
// 后继 worktree 的祖先链包含前驱分支的 tip commit。
//
// 步骤严格对应真实场景：前驱任务先在自己的 worktree 里提交实现产出并
// push 到 origin（真实场景下后继开始跑之前，前驱必然已经到了
// pr_open，意味着已经 push 过）；后继任务的 RepoConfig.BaseRefOverride
// 设为前驱分支名，走完整的 WorktreeManager.Create 流程。
func TestStackedPRSuccessorAncestryIncludesPredecessorTip(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()
	repo := DefaultRepoConfig("acme/demo")

	// 前驱任务：正常从 dev 分叉（BaseRefOverride 为空）
	predWt, err := m.Create(ctx, CreateParams{
		Repo: repo, CloneURL: src, Kind: KindFeature,
		IssueKey: "CR-200", Title: "predecessor task",
	})
	if err != nil {
		t.Fatalf("创建前驱 worktree 失败: %v", err)
	}
	if predWt.BaseBranch != "dev" {
		t.Fatalf("前驱应从 dev 分叉，得到 %q", predWt.BaseBranch)
	}

	writeFile(t, filepath.Join(predWt.Path, "fileA.txt"), "前驱任务的实现产出\n")
	if err := m.Commit(ctx, predWt, "feat: 前驱任务新增 fileA"); err != nil {
		t.Fatalf("前驱提交失败: %v", err)
	}
	predTip := gitOut(t, predWt.Path, "rev-parse", "HEAD")

	// 前驱到 pr_open 前必然已 push —— 只有这样它的 tip 才会出现在
	// origin 的 refs/remotes/origin/* 命名空间。
	if err := m.Push(ctx, predWt, repo, nil); err != nil {
		t.Fatalf("前驱 push 失败: %v", err)
	}

	// 后继任务：调度器已把前驱分支名填进 base_ref → BaseRefOverride
	succRepo := repo
	succRepo.BaseRefOverride = predWt.Branch

	succWt, err := m.Create(ctx, CreateParams{
		Repo: succRepo, CloneURL: src, Kind: KindFeature,
		IssueKey: "CR-201", Title: "successor task",
	})
	if err != nil {
		t.Fatalf("创建后继 worktree 失败: %v", err)
	}
	if succWt.BaseBranch != predWt.Branch {
		t.Fatalf("后继 BaseBranch = %q，期望前驱分支 %q", succWt.BaseBranch, predWt.Branch)
	}

	// 断言 1：git merge-base --is-ancestor <前驱 tip> HEAD（在后继
	// worktree 目录里跑），退出码 0 表示前驱 tip 是后继 HEAD 的祖先。
	if !isAncestor(t, succWt.Path, predTip, "HEAD") {
		t.Errorf("前驱 tip %s 应是后继 HEAD 的祖先，但 merge-base --is-ancestor 判定不是", predTip[:8])
	}

	// 断言 2：git log --oneline 里能看到前驱那次提交的 commit message。
	log := gitOut(t, succWt.Path, "log", "--oneline")
	if !strings.Contains(log, "前驱任务新增 fileA") {
		t.Errorf("后继分支的 git log 应包含前驱提交信息，得到:\n%s", log)
	}

	// 顺带确认：既然是从前驱 tip 分叉，checkout 出来的工作区里
	// fileA.txt 应该已经在（后继能直接在前驱产出之上开发）。
	if _, err := os.Stat(filepath.Join(succWt.Path, "fileA.txt")); err != nil {
		t.Errorf("后继工作区应已带有前驱产出的 fileA.txt: %v", err)
	}
}

// TestStackedPRSuccessorDiffExcludesPredecessorChanges 验证 F3.2-AC2
// （用户原话点名的场景）：后继 PR 的 changed files 不含前驱的改动。
//
// 用 "git diff <后继 BaseBranch>...HEAD --name-only" 断言，后继
// BaseBranch 是前驱分支名，git diff 时限定到镜像命名空间
// （MirrorBaseRef），与 WorktreeManager.ChangedFiles 的实现完全一致 ——
// 直接调用 ChangedFiles 就是在验证生产代码里真实跑的那条命令。
func TestStackedPRSuccessorDiffExcludesPredecessorChanges(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()
	repo := DefaultRepoConfig("acme/demo")

	predWt, err := m.Create(ctx, CreateParams{
		Repo: repo, CloneURL: src, Kind: KindFeature,
		IssueKey: "CR-210", Title: "predecessor task",
	})
	if err != nil {
		t.Fatalf("创建前驱 worktree 失败: %v", err)
	}
	writeFile(t, filepath.Join(predWt.Path, "fileA.txt"), "前驱任务的实现产出\n")
	if err := m.Commit(ctx, predWt, "feat: 前驱任务新增 fileA"); err != nil {
		t.Fatalf("前驱提交失败: %v", err)
	}
	if err := m.Push(ctx, predWt, repo, nil); err != nil {
		t.Fatalf("前驱 push 失败: %v", err)
	}

	succRepo := repo
	succRepo.BaseRefOverride = predWt.Branch
	succWt, err := m.Create(ctx, CreateParams{
		Repo: succRepo, CloneURL: src, Kind: KindFeature,
		IssueKey: "CR-211", Title: "successor task",
	})
	if err != nil {
		t.Fatalf("创建后继 worktree 失败: %v", err)
	}

	writeFile(t, filepath.Join(succWt.Path, "fileB.txt"), "后继任务自己的改动\n")
	if err := m.Commit(ctx, succWt, "feat: 后继任务新增 fileB"); err != nil {
		t.Fatalf("后继提交失败: %v", err)
	}

	// 核心断言：用 ChangedFiles（内部就是 git diff base...HEAD
	// --name-only，base 经 MirrorBaseRef 限定到镜像命名空间）
	files, err := m.ChangedFiles(ctx, succWt)
	if err != nil {
		t.Fatalf("ChangedFiles 失败: %v", err)
	}
	if len(files) != 1 || files[0] != "fileB.txt" {
		t.Fatalf("后继 changed files = %v，期望只有 [fileB.txt]（不含前驱的 fileA.txt）", files)
	}

	// 用同样语义的原始 git 命令再验一遍，措辞精确对应用户的验收描述：
	// "git diff base...head --name-only"。
	raw := gitOut(t, succWt.Path, "diff", "--name-only",
		MirrorBaseRef(succWt.BaseBranch)+"...HEAD")
	rawFiles := strings.Fields(raw)
	if len(rawFiles) != 1 || rawFiles[0] != "fileB.txt" {
		t.Fatalf("git diff %s...HEAD --name-only = %v，期望只有 [fileB.txt]",
			MirrorBaseRef(succWt.BaseBranch), rawFiles)
	}
	for _, f := range rawFiles {
		if f == "fileA.txt" {
			t.Errorf("后继的 diff 不应包含前驱的改动 fileA.txt")
		}
	}
}

// TestStackedPRWithoutBaseRefOverridePollutesDiff 是上一个测试的对照组：
// 如果不做 base_ref 穿透（后继从 dev 直接分叉，而不是从前驱分支分叉），
// 后继任务若要在前驱未合并进 dev 的改动之上继续开发，只能自己在分支里
// 重新产出前驱那部分改动（因为 dev 上还没有它）—— 这样 diff 就会同时
// 包含 fileA.txt 和 fileB.txt，PR 里混进了本不属于这次改动的内容。
// 这正是栈式 PR（base_ref 穿透）要解决的问题：让后继的 diff 天然只剩
// 它自己的净改动。
func TestStackedPRWithoutBaseRefOverridePollutesDiff(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()
	repo := DefaultRepoConfig("acme/demo")

	// 前驱任务照常产出 fileA.txt 并 push（但后继完全不引用它的分支）。
	predWt, err := m.Create(ctx, CreateParams{
		Repo: repo, CloneURL: src, Kind: KindFeature,
		IssueKey: "CR-220", Title: "predecessor task",
	})
	if err != nil {
		t.Fatalf("创建前驱 worktree 失败: %v", err)
	}
	writeFile(t, filepath.Join(predWt.Path, "fileA.txt"), "前驱任务的实现产出\n")
	if err := m.Commit(ctx, predWt, "feat: 前驱任务新增 fileA"); err != nil {
		t.Fatalf("前驱提交失败: %v", err)
	}
	if err := m.Push(ctx, predWt, repo, nil); err != nil {
		t.Fatalf("前驱 push 失败: %v", err)
	}

	// 后继任务：BaseRefOverride 为空 —— 没有栈式穿透，直接从 dev 分叉。
	succWt, err := m.Create(ctx, CreateParams{
		Repo: repo, CloneURL: src, Kind: KindFeature,
		IssueKey: "CR-221", Title: "successor without stacking",
	})
	if err != nil {
		t.Fatalf("创建后继 worktree 失败: %v", err)
	}
	if succWt.BaseBranch != "dev" {
		t.Fatalf("未做穿透时后继应仍从 dev 分叉，得到 %q", succWt.BaseBranch)
	}

	// dev 上没有 fileA.txt，后继若要在它之上继续开发，只能自己重新
	// 产出一份 —— 模拟真实世界里"没有栈式 PR 支持时的常见做法"。
	writeFile(t, filepath.Join(succWt.Path, "fileA.txt"), "前驱任务的实现产出\n")
	writeFile(t, filepath.Join(succWt.Path, "fileB.txt"), "后继任务自己的改动\n")
	if err := m.Commit(ctx, succWt, "feat: 后继任务（未栈式）重复产出 fileA 并新增 fileB"); err != nil {
		t.Fatalf("后继提交失败: %v", err)
	}

	files, err := m.ChangedFiles(ctx, succWt)
	if err != nil {
		t.Fatalf("ChangedFiles 失败: %v", err)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f] = true
	}
	if !got["fileA.txt"] || !got["fileB.txt"] {
		t.Fatalf("未做 base_ref 穿透时，diff 应同时包含 fileA.txt 与 fileB.txt（对照组），得到 %v", files)
	}
	if len(files) != 2 {
		t.Errorf("对照组 changed files 应恰好是 [fileA.txt fileB.txt]，得到 %v", files)
	}
}

// TestStackedPRIndependentRootUnaffectedByPredecessor 补 F3.1-AC3 的端到端
// 覆盖：即使同一 mirror 里已经存在一个已 push 的前驱分支，独立根任务
// （BaseRefOverride 为空）走完整 WorktreeManager.Create 流程后，产生的分支
// 仍然是从 dev 分叉，而不是被前驱分支意外污染。
//
// branch_test.go 的 TestBaseBranchOverride 已经把 BaseBranch() 这个纯函数
// 的短路逻辑测得很清楚；这里补的是"端到端走完整 Create 流程、且前驱确实
// 存在"这个更接近真实调度场景的路径。
func TestStackedPRIndependentRootUnaffectedByPredecessor(t *testing.T) {
	src := sourceRepo(t)
	m := newManager(t)
	ctx := context.Background()
	repo := DefaultRepoConfig("acme/demo")

	// 先造一个已经 push 过的前驱分支，制造"污染源"
	predWt, err := m.Create(ctx, CreateParams{
		Repo: repo, CloneURL: src, Kind: KindFeature,
		IssueKey: "CR-230", Title: "unrelated predecessor",
	})
	if err != nil {
		t.Fatalf("创建前驱 worktree 失败: %v", err)
	}
	writeFile(t, filepath.Join(predWt.Path, "fileA.txt"), "与本任务无关的前驱改动\n")
	if err := m.Commit(ctx, predWt, "feat: 无关前驱的改动"); err != nil {
		t.Fatalf("前驱提交失败: %v", err)
	}
	predTip := gitOut(t, predWt.Path, "rev-parse", "HEAD")
	if err := m.Push(ctx, predWt, repo, nil); err != nil {
		t.Fatalf("前驱 push 失败: %v", err)
	}

	// 独立根任务：同一仓库、同一 mirror，但 BaseRefOverride 为空
	rootWt, err := m.Create(ctx, CreateParams{
		Repo: repo, CloneURL: src, Kind: KindFix,
		IssueKey: "CR-231", Title: "independent root task",
	})
	if err != nil {
		t.Fatalf("创建独立根 worktree 失败: %v", err)
	}

	if rootWt.BaseBranch != "dev" {
		t.Errorf("独立根应从 dev 分叉，得到 %q", rootWt.BaseBranch)
	}
	if isAncestor(t, rootWt.Path, predTip, "HEAD") {
		t.Errorf("独立根不应带有无关前驱分支的 tip commit（%s），但 merge-base --is-ancestor 判定是祖先", predTip[:8])
	}
	if _, err := os.Stat(filepath.Join(rootWt.Path, "fileA.txt")); !os.IsNotExist(err) {
		t.Errorf("独立根的工作区不应出现无关前驱的 fileA.txt")
	}
}
