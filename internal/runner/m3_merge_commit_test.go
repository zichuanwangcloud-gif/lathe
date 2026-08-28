package runner

import (
	"context"
	"testing"
	"time"

	"github.com/Clouditera/lathe/internal/task"
)

// ================================================================
// docs/07-prd-orchestration.md §5 · M3 出口条件端到端集成测试
//
// F4.3-AC6：M3-squash 测试（m3_squash_merge_test.go）只验过 squash/
// cherry-pick 模拟出来的合并效果，没有真正测过 "git merge --no-ff"
// （保留原提交、生成一个 merge commit）这条合并方式。本文件补上这
// 条路径，证明 rebase 级联算法对合并方式不敏感。
//
// 与 squash 场景的关键区别：merge commit 方式下，前驱分支的原始提交
// （task1Tip）在合并之后【本来就是】dev 新 tip 的祖先（squash 场景里
// dev 的新提交是独立生成的、内容等价但 SHA 不同的提交，task1Tip 不是
// 它的祖先）——理论上这让 rebase --onto 的应用范围更干净、冲突概率
// 更低。本文件不重复 squash 测试的每一条细节断言，只验证核心结论：
// 合并检测触发、rebase 级联收敛、diff 干净、重验通过。
// ================================================================

// TestM3MergeCommitCascadeConverges 验证前驱 PR 以 --no-ff merge
// commit 方式并入 dev 时，F4.3 的 rebase 级联同样能让后继收敛。
func TestM3MergeCommitCascadeConverges(t *testing.T) {
	_, m, userID, repoID, repo, src := rebaseFollowupFixture(t)
	ctx := context.Background()

	task1, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "M3M-1", LinearIssueID: "uuid-m3m-1",
	})
	if err != nil {
		t.Fatalf("建 task1 失败: %v", err)
	}
	task2, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "M3M-2", LinearIssueID: "uuid-m3m-2",
		DependsOn: &task1.ID,
	})
	if err != nil {
		t.Fatalf("建 task2 失败: %v", err)
	}

	wm, err := NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatalf("建工作区管理器失败: %v", err)
	}
	lin := newRebaseFollowupLinear()
	lin.add(rebaseFollowupIssue("uuid-m3m-1", "M3M-1"))
	lin.add(rebaseFollowupIssue("uuid-m3m-2", "M3M-2"))
	gh := newSquashMergeGitHub(9600)
	clients := &fakeClients{lin: lin, gh: gh}

	ag := chainedAgent([][2]string{
		{"fileA.txt", "task1 的产出\n"},
		{"fileB.txt", "task2 的产出\n"},
	})

	verifs := &fakeVerifications{}
	pipe := &Pipeline{
		Tasks: m, Worktrees: wm, Verifier: NewVerifier(3*time.Minute, ""),
		Agent: ag, Clients: clients, Notifier: &fakeNotifier{},
		Verifications: verifs, PermissionMode: "acceptEdits", SettingSources: "project",
	}

	// ---- task1、task2 走完整调度到 pr_open（task2 栈在 task1 上）----
	if err := pipe.Execute(ctx, ExecuteParams{
		TaskID: task1.ID, Repo: repo, CloneURL: src, IssueID: "uuid-m3m-1", Actor: "node:test",
	}); err != nil {
		t.Fatalf("task1 Execute 失败: %v", err)
	}
	t1, err := m.Get(ctx, task1.ID)
	if err != nil || t1.State != task.StatePROpen {
		t.Fatalf("task1 应到达 pr_open，实际 state=%v err=%v", t1, err)
	}
	task1Branch := *t1.BranchName
	task1WorktreePath := *t1.WorktreePath
	if t1.PRNumber == nil {
		t.Fatal("task1 应落库 pr_number")
	}
	task1PRNumber := *t1.PRNumber
	task1Tip := gitOut(t, task1WorktreePath, "rev-parse", "HEAD")

	if err := m.SetBaseRef(ctx, task2.ID, &task1Branch); err != nil {
		t.Fatalf("设置 task2.base_ref 失败: %v", err)
	}
	repo2 := repo
	repo2.BaseRefOverride = task1Branch
	if err := pipe.Execute(ctx, ExecuteParams{
		TaskID: task2.ID, Repo: repo2, CloneURL: src, IssueID: "uuid-m3m-2", Actor: "node:test",
	}); err != nil {
		t.Fatalf("task2 Execute 失败: %v", err)
	}
	t2, err := m.Get(ctx, task2.ID)
	if err != nil || t2.State != task.StatePROpen {
		t.Fatalf("task2 应到达 pr_open，实际 state=%v err=%v", t2, err)
	}
	task2WorktreePath := *t2.WorktreePath

	verifRowsBeforeReverify := len(verifs.rows)

	// ---- 模拟 merge commit 方式的合并：git merge --no-ff ----
	//
	// 与 squash 相反，--no-ff 保留 task1 的原始提交：task1Tip 在合并
	// 之后本来就是 dev 新 tip 的祖先（走的是父提交链，不是内容等价的
	// 独立新提交）。
	gitOut(t, src, "checkout", "-q", "dev")
	gitOut(t, src, "-c", "user.email=t@e.st", "-c", "user.name=t",
		"merge", "--no-ff", "--quiet", task1Branch, "-m", "merge commit M3M-1")
	devTip := gitOut(t, src, "rev-parse", "dev")
	if devTip == task1Tip {
		t.Fatalf("--no-ff 合并应产生一个新的 merge commit SHA，不应等于 task1 分支自己的 tip(%s)", task1Tip)
	}
	if !isAncestor(t, src, task1Tip, devTip) {
		t.Fatalf("merge commit 方式下，task1Tip(%s) 应是 dev 新 tip(%s) 的祖先", task1Tip[:8], devTip[:8])
	}

	gh.setMerged(task1PRNumber)

	mp := &MergePoller{
		Tasks: m, Worktrees: wm, Pipeline: pipe, Notifier: &fakeNotifier{},
		ClientFactory: fixedClientFactory{clients: clients},
		RepoLookup:    repoLookupFixed(repo),
	}

	if err := mp.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce 失败: %v", err)
	}

	// ---- 断言：task1 转 merged ----
	t1After, err := m.Get(ctx, task1.ID)
	if err != nil {
		t.Fatalf("读取 task1 失败: %v", err)
	}
	if t1After.State != task.StateMerged {
		t.Fatalf("task1 终态 = %s，期望 merged", t1After.State)
	}

	// ---- 断言：task2 的分支历史被自动 rebase 到 dev 的新 tip ----
	if !isAncestor(t, task2WorktreePath, devTip, "HEAD") {
		t.Errorf("task2 的分支历史应以 dev 的新 tip(merge commit %s) 为祖先——rebase 应已完成", devTip[:8])
	}

	// ---- 断言：base_ref 被清空（rebase 到了 default_branch）----
	t2AfterRebase, err := m.Get(ctx, task2.ID)
	if err != nil {
		t.Fatalf("读取 task2 失败: %v", err)
	}
	if t2AfterRebase.BaseRef != nil {
		t.Errorf("task2.base_ref 应被清空（rebase 到 default_branch），实际 %q", *t2AfterRebase.BaseRef)
	}

	// ---- 断言：diff 仍只含 task2 自己的改动 ----
	diffOut := gitOut(t, task2WorktreePath, "diff", MirrorBaseRef(repo.DefaultBranch)+"...HEAD", "--name-only")
	gotFiles := map[string]bool{}
	for _, f := range splitNonEmptyLines(diffOut) {
		gotFiles[f] = true
	}
	if gotFiles["fileA.txt"] {
		t.Errorf("task2 的 diff 不应包含前驱 task1 的改动 fileA.txt，实际清单: %v", gotFiles)
	}
	if !gotFiles["fileB.txt"] || len(gotFiles) != 1 {
		t.Errorf("task2 的 diff 应恰好只有 fileB.txt，实际: %v", gotFiles)
	}

	// ---- 断言：自动重验通过，且没有重跑 agent ----
	if t2AfterRebase.State != task.StatePROpen {
		t.Fatalf("task2 重验后终态 = %s，期望 pr_open", t2AfterRebase.State)
	}
	if len(verifs.rows) <= verifRowsBeforeReverify {
		t.Errorf("task2 的 rebase 跟进重验应产生新的验证记录，重验前 %d 行，重验后 %d 行",
			verifRowsBeforeReverify, len(verifs.rows))
	}
	if len(ag.calls) != 4 {
		t.Errorf("rebase 跟进的重验不应调用 agent，调用次数应仍是 4，实际 %d", len(ag.calls))
	}
}
