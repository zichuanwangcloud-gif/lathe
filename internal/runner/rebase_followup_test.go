package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Clouditera/lathe/internal/integration/agent"
	"github.com/Clouditera/lathe/internal/integration/github"
	"github.com/Clouditera/lathe/internal/integration/linear"
	"github.com/Clouditera/lathe/internal/task"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 本文件测 F4.3（后继链自动跟进）的核心：MergePoller.rebaseFollowup。
// 全部用真实 Postgres + 真实 Pipeline.Execute + 真实 git 操作（本地
// 仓库当 origin），不 fake 任何 git 层面的东西——这条链路全是真实
// git 命令，fake 测不出 rebase 是否真的对（任务说明原话）。

// ---------------------------------------------------------------- 假件

// rebaseFollowupLinear 按 issueID 分别存取 issue/回帖——不是
// pipeline_test.go 里 fakeLinear 那种单一固定 issue。MergePoller.
// Pipeline 在生产环境里是同一个用户共享的一份 Pipeline，rebase 跟进会
// 依次对链上不同任务（不同 issue）重验，必须能按 id 分别应答。
type rebaseFollowupLinear struct {
	issues   map[string]*linear.Issue
	comments map[string][]string
}

func newRebaseFollowupLinear() *rebaseFollowupLinear {
	return &rebaseFollowupLinear{issues: map[string]*linear.Issue{}, comments: map[string][]string{}}
}

func (f *rebaseFollowupLinear) add(issue *linear.Issue) { f.issues[issue.ID] = issue }

func (f *rebaseFollowupLinear) Issue(ctx context.Context, id string) (*linear.Issue, error) {
	iss, ok := f.issues[id]
	if !ok {
		return nil, fmt.Errorf("测试假件：未知 issue %q", id)
	}
	return iss, nil
}

func (f *rebaseFollowupLinear) Comment(ctx context.Context, issueID, body string) (string, error) {
	f.comments[issueID] = append(f.comments[issueID], body)
	return "c", nil
}

// rebaseFollowupGitHub 记录每次 CreatePR 调用的参数，固定返回同一个 PR
// 对象——跟 pipeline_test.go 的 fakeGitHub 一样极简，不做幂等判断
// （任务说明原话："结果...不强求"）。
type rebaseFollowupGitHub struct {
	pr     *github.PullRequest
	params []github.PRParams
}

func (f *rebaseFollowupGitHub) CreatePR(ctx context.Context, p github.PRParams) (*github.PullRequest, error) {
	f.params = append(f.params, p)
	return f.pr, nil
}

func (f *rebaseFollowupGitHub) GetPRInfo(ctx context.Context, providerRepo string, number int) (*github.PRInfo, error) {
	return nil, nil
}

// chainedAgent 造一个能连续跑 N 个任务的短路 agent：每个任务分诊通过、
// 实现阶段只写一个纯文案文件（避免 heavy 档红-绿证明的复杂度，跟
// stacked_pr_pipeline_test.go 的 stackedAgent 同一立场）。files 的每一
// 项是 {文件名, 内容}，按顺序对应链上第 1、2、3...个任务的初次实现。
//
// F4.3 的重验（Retry Entry=EntryVerify）不重跑 agent——本文件的 rebase
// 跟进重验调用永远不会消费到这个 agent 的调用队列之外的条目，这正是
// "不重跑 agent"应该有的效果，用同一个 agent 假件贯穿全程反而更能
// 证明这一点：调用次数不多不少，恰好是 2*len(files)。
func chainedAgent(files [][2]string) *fakeAgent {
	var results []*agent.Result
	var mutate []func(string) error
	for _, f := range files {
		fname, content := f[0], f[1]
		results = append(results,
			&agent.Result{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象和期望行为","question":""}`},
			&agent.Result{Success: true, Text: "实现完成"},
		)
		mutate = append(mutate, nil, func(dir string) error {
			return os.WriteFile(filepath.Join(dir, fname), []byte(content), 0o644)
		})
	}
	return &fakeAgent{results: results, mutate: mutate}
}

func rebaseFollowupIssue(id, key string) *linear.Issue {
	return &linear.Issue{
		ID: id, Identifier: key, Title: "F4.3 rebase 跟进测试 " + key,
		Description: "端到端固定文案", URL: "https://linear.app/x/" + key,
	}
}

// rebaseFollowupFixture 建独立的 user/repo 与源仓库，供本文件的场景
// 共用——与 orchestrationFixture/stackedPipelineFixture 同一套手法。
func rebaseFollowupFixture(t *testing.T) (pool *pgxpool.Pool, m *task.Machine, userID, repoID int64, repo RepoConfig, src string) {
	t.Helper()
	pool = testPoolForPipeline(t)
	m = task.NewMachine(pool)
	ctx := context.Background()

	providerRepo := "acme/rebase-followup-" + t.Name()
	email := "rebase-followup-" + t.Name() + "@example.com"
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) ON CONFLICT (email) DO UPDATE SET updated_at=now() RETURNING id`,
		email).Scan(&userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2)
		 ON CONFLICT (user_id, provider_repo) DO UPDATE SET updated_at=now() RETURNING id`,
		userID, providerRepo).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})
	return pool, m, userID, repoID, DefaultRepoConfig(providerRepo), goSourceRepo(t)
}

// repoLookupFixed 是最简单的 RepoLookup：不管 repoID 是谁，都返回同一份
// 固定配置——本文件的场景全程只有一个仓库，不需要按 id 查表。
func repoLookupFixed(repo RepoConfig) RepoLookup {
	return func(ctx context.Context, repoID int64) (RepoConfig, error) { return repo, nil }
}

// ----------------------------------------------------------------
// 场景一：task1→task2→task3 三层栈式 PR 链，task1 合并后触发级联
// rebase 跟进——F4.3 本阶段的重点测试。
// ----------------------------------------------------------------

// TestRebaseFollowupCascadeThreeLevels 造一条真实的三层栈式 PR 链，全部
// 走完整 Pipeline.Execute 到 pr_open；然后模拟"task1 合并"（直接调用
// rebaseFollowup 顶层入口，不经过 GitHub），断言：
//
//   - task2 的分支历史确实被 rebase 到 default_branch（git 祖先关系 +
//     工作区文件内容），base_ref 被清空，重验后仍是 pr_open；
//   - task3 的分支历史被 rebase 到 task2 的【新】分支（不是
//     default_branch 本身！），base_ref 更新为 task2 的分支名（不是
//     清空），重验后仍是 pr_open——这是级联正确性的核心断言：如果代码
//     误用 default_branch 而不是 task2 的分支去 rebase task3，task3
//     的工作区里就不会有 task2 自己那个文件（task2_notes.txt），
//     下面的断言会当场抓到这个错误。
func TestRebaseFollowupCascadeThreeLevels(t *testing.T) {
	_, m, userID, repoID, repo, src := rebaseFollowupFixture(t)
	ctx := context.Background()

	task1, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "RB-1", LinearIssueID: "uuid-rb-1",
	})
	if err != nil {
		t.Fatalf("建 task1 失败: %v", err)
	}
	task2, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "RB-2", LinearIssueID: "uuid-rb-2",
		DependsOn: &task1.ID,
	})
	if err != nil {
		t.Fatalf("建 task2 失败: %v", err)
	}
	task3, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "RB-3", LinearIssueID: "uuid-rb-3",
		DependsOn: &task2.ID,
	})
	if err != nil {
		t.Fatalf("建 task3 失败: %v", err)
	}

	wm, err := NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatalf("建工作区管理器失败: %v", err)
	}
	lin := newRebaseFollowupLinear()
	lin.add(rebaseFollowupIssue("uuid-rb-1", "RB-1"))
	lin.add(rebaseFollowupIssue("uuid-rb-2", "RB-2"))
	lin.add(rebaseFollowupIssue("uuid-rb-3", "RB-3"))
	gh := &rebaseFollowupGitHub{pr: &github.PullRequest{
		Number: 900, URL: "https://github.com/acme/rebase-followup/pull/900",
	}}
	ag := chainedAgent([][2]string{
		{"task1_notes.txt", "task1 的产出\n"},
		{"task2_notes.txt", "task2 的产出\n"},
		{"task3_notes.txt", "task3 的产出\n"},
	})

	// 唯一一份 Pipeline，贯穿三个任务的初次执行【与】后面 rebase 跟进
	// 触发的重验——跟生产环境的形态一致（MergePoller.Pipeline 复用
	// buildPipeline 建出来的那一份），不是每个任务各自一份。
	pipe := &Pipeline{
		Tasks: m, Worktrees: wm, Verifier: NewVerifier(3*time.Minute, ""),
		Agent: ag, Clients: &fakeClients{lin: lin, gh: gh}, Notifier: &fakeNotifier{},
		Verifications: &fakeVerifications{}, PermissionMode: "acceptEdits", SettingSources: "project",
	}

	// ---- task1：独立根，正常从 dev 分叉 ----
	if err := pipe.Execute(ctx, ExecuteParams{
		TaskID: task1.ID, Repo: repo, CloneURL: src, IssueID: "uuid-rb-1", Actor: "node:test",
	}); err != nil {
		t.Fatalf("task1 Execute 失败: %v", err)
	}
	t1, err := m.Get(ctx, task1.ID)
	if err != nil || t1.State != task.StatePROpen {
		t.Fatalf("task1 应到达 pr_open，实际 state=%v err=%v", t1, err)
	}
	task1Branch := *t1.BranchName
	// 必须在任何改写发生之前捕获（本例中"改写"是下面对 task2 的
	// rebase）——task1 自己此刻还没被任何人动过，直接读它现在的 HEAD。
	task1Tip := gitOut(t, *t1.WorktreePath, "rev-parse", "HEAD")

	// ---- task2：调度器 fillBaseRef 的等价手工步骤——栈在 task1 上面 ----
	if err := m.SetBaseRef(ctx, task2.ID, &task1Branch); err != nil {
		t.Fatalf("设置 task2.base_ref 失败: %v", err)
	}
	repo2 := repo
	repo2.BaseRefOverride = task1Branch
	if err := pipe.Execute(ctx, ExecuteParams{
		TaskID: task2.ID, Repo: repo2, CloneURL: src, IssueID: "uuid-rb-2", Actor: "node:test",
	}); err != nil {
		t.Fatalf("task2 Execute 失败: %v", err)
	}
	t2, err := m.Get(ctx, task2.ID)
	if err != nil || t2.State != task.StatePROpen {
		t.Fatalf("task2 应到达 pr_open，实际 state=%v err=%v", t2, err)
	}
	task2Branch := *t2.BranchName

	// ---- task3：栈在 task2 上面 ----
	if err := m.SetBaseRef(ctx, task3.ID, &task2Branch); err != nil {
		t.Fatalf("设置 task3.base_ref 失败: %v", err)
	}
	repo3 := repo
	repo3.BaseRefOverride = task2Branch
	if err := pipe.Execute(ctx, ExecuteParams{
		TaskID: task3.ID, Repo: repo3, CloneURL: src, IssueID: "uuid-rb-3", Actor: "node:test",
	}); err != nil {
		t.Fatalf("task3 Execute 失败: %v", err)
	}
	t3, err := m.Get(ctx, task3.ID)
	if err != nil || t3.State != task.StatePROpen {
		t.Fatalf("task3 应到达 pr_open，实际 state=%v err=%v", t3, err)
	}

	if len(ag.calls) != 6 {
		t.Fatalf("三个任务的初次执行应恰好调用 agent 6 次（各 2 次），实际 %d", len(ag.calls))
	}

	// ---- 模拟 task1 合并：dev 通过 cherry-pick 拿到 task1 的内容——
	// 内容等价但走的是完全不同的 commit（squash-merge 的真实效果），
	// 这样后面 task2 的 rebase 才不会因为"dev 里根本没有 task1 的
	// 内容"而悄悄丢掉它 ----
	gitOut(t, src, "checkout", "-q", "dev")
	gitOut(t, src, "-c", "user.email=t@e.st", "-c", "user.name=t", "cherry-pick", "-x", task1Tip)
	devTip := gitOut(t, src, "rev-parse", "dev")

	mp := &MergePoller{
		Tasks: m, Worktrees: wm, Pipeline: pipe, Notifier: &fakeNotifier{},
		RepoLookup: repoLookupFixed(repo),
	}

	// ---- 顶层入口：等价于 MergePoller.onMerged 里 task1 转 merged 之后
	// 触发的那一次调用 ----
	if err := mp.rebaseFollowup(ctx, task1Branch, task1Tip, repo.DefaultBranch); err != nil {
		t.Fatalf("rebaseFollowup 失败: %v", err)
	}

	// ---- agent 调用次数不应增加：重验不重跑 agent（F4.3-AC3） ----
	if len(ag.calls) != 6 {
		t.Errorf("rebase 跟进的重验不应调用 agent，调用次数应仍是 6，实际 %d", len(ag.calls))
	}

	// ---- 断言：task2 ----
	t2After, err := m.Get(ctx, task2.ID)
	if err != nil {
		t.Fatalf("读取 task2 失败: %v", err)
	}
	if t2After.State != task.StatePROpen {
		t.Errorf("task2 重验后 state = %s，期望 pr_open", t2After.State)
	}
	if t2After.BaseRef != nil {
		t.Errorf("task2.base_ref 应被清空（rebase 到 default_branch），实际 %q", *t2After.BaseRef)
	}
	if t2After.WorktreePath == nil {
		t.Fatal("task2 应仍保有 worktree_path")
	}
	if !isAncestor(t, *t2After.WorktreePath, devTip, "HEAD") {
		t.Errorf("task2 的分支历史应以 dev 新 tip(%s) 为祖先", devTip[:8])
	}
	for _, f := range []string{"task1_notes.txt", "task2_notes.txt"} {
		if _, err := os.Stat(filepath.Join(*t2After.WorktreePath, f)); err != nil {
			t.Errorf("task2 rebase 后的工作区应包含 %s: %v", f, err)
		}
	}
	task2NewTip := gitOut(t, *t2After.WorktreePath, "rev-parse", "HEAD")

	// task2 的重验 PR：base 应是 dev（不是别的分支）
	assertHasCreatePRWithBase(t, gh.params, task2Branch, "dev")

	// ---- 断言：task3（级联正确性的核心） ----
	t3After, err := m.Get(ctx, task3.ID)
	if err != nil {
		t.Fatalf("读取 task3 失败: %v", err)
	}
	if t3After.State != task.StatePROpen {
		t.Errorf("task3 重验后 state = %s，期望 pr_open", t3After.State)
	}
	if t3After.BaseRef == nil || *t3After.BaseRef != task2Branch {
		t.Errorf("task3.base_ref 应更新为 task2 的分支名 %q（不应清空），实际 %v", task2Branch, t3After.BaseRef)
	}
	if t3After.WorktreePath == nil {
		t.Fatal("task3 应仍保有 worktree_path")
	}
	if !isAncestor(t, *t3After.WorktreePath, task2NewTip, "HEAD") {
		t.Errorf("task3 的分支历史应以 task2 rebase 后的新 tip(%s) 为祖先", task2NewTip[:8])
	}
	// ★核心断言：task3 的工作区里必须有 task2_notes.txt——如果级联时
	// 误用了 default_branch 而不是 task2 的分支去 rebase task3，dev
	// 里没有这个文件，task3 rebase 后也就不会有它。
	for _, f := range []string{"task1_notes.txt", "task2_notes.txt", "task3_notes.txt"} {
		if _, err := os.Stat(filepath.Join(*t3After.WorktreePath, f)); err != nil {
			t.Errorf("task3 rebase 后的工作区应包含 %s（证明级联到的是 task2 的分支，不是 default_branch）: %v", f, err)
		}
	}

	// task3 的重验 PR：base 应是 task2 的分支名（栈式 PR 的核心效果）
	assertHasCreatePRWithBase(t, gh.params, *t3After.BranchName, task2Branch)
}

// assertHasCreatePRWithBase 断言 params 里【最后一次】head==wantHead 的
// CreatePR 调用的 base==wantBase——同一个 head 会被调用两次（初次实现
// 开 PR + rebase 跟进重验后再开一次），要看的是重验之后这一次。
func assertHasCreatePRWithBase(t *testing.T, params []github.PRParams, wantHead, wantBase string) {
	t.Helper()
	for i := len(params) - 1; i >= 0; i-- {
		p := params[i]
		if p.Head == wantHead {
			if p.Base != wantBase {
				t.Errorf("head=%q 最后一次 CreatePR 调用 base=%q，期望 %q", wantHead, p.Base, wantBase)
			}
			return
		}
	}
	t.Errorf("未找到 head=%q 的 CreatePR 调用，实际调用: %+v", wantHead, params)
}

// ----------------------------------------------------------------
// 场景二：rebase 冲突——任务转 failed、机器码正确、现场保留、不级联。
// ----------------------------------------------------------------

// fixedClients 让 ClientFactory.ForUser 无论传入哪个 userID 都返回同一
// 份固定 Clients——本文件的场景全程只有一个用户。
type fixedClientFactory struct{ clients Clients }

func (f fixedClientFactory) ForUser(ctx context.Context, userID int64) (Clients, error) {
	return f.clients, nil
}

// multiStepAgent 造一个按顺序消费 muts 的短路 agent：每一步分诊通过，
// 实现阶段跑对应的 mutate 函数——比 chainedAgent 更通用（mutate 内容
// 由调用方直接给，不限定成"写一个新文件"）。
func multiStepAgent(muts []func(dir string) error) *fakeAgent {
	var results []*agent.Result
	var mutate []func(string) error
	for _, mu := range muts {
		results = append(results,
			&agent.Result{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象和期望行为","question":""}`},
			&agent.Result{Success: true, Text: "改完了"},
		)
		mutate = append(mutate, nil, mu)
	}
	return &fakeAgent{results: results, mutate: mutate}
}

// writeSharedFile 造一个把 shared.txt 整份替换成 content 的 mutate 函数。
func writeSharedFile(content string) func(string) error {
	return func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "shared.txt"), []byte(content), 0o644)
	}
}

// TestRebaseFollowupConflictFailsPreservesSceneNoCascade 制造一个真实
// 会冲突的 rebase 场景：taskA 把 shared.txt 第二行改成 "line2-A"；
// dev 在 taskA 合并之外【独立地】把同一行改成了 "line2-DEV"（模拟旧
// 基线与新基线在同一处改了不同内容——squash-merge 之外，dev 上还发生
// 了别的事）；taskB（栈在 taskA 上面）自己又把那一行改成了
// "line2-B"——三方在同一处touch，rebase --onto 必然冲突。
//
// 链是 taskA→taskB→taskC（taskC 栈在 taskB 上面，改动是与 shared.txt
// 无关的新文件），断言：
//
//   - taskB 转 failed，failure_stage 是新增的机器码 StageRebaseConflict；
//   - taskB 的 worktree 没被删除（现场保留，人可以直接进去解决冲突）；
//   - 回帖与推送通知都发生了；
//   - taskC 完全没被动过（没有级联到更深层）：状态、base_ref、分支历史
//     都和 taskB 失败之前一样——因为 taskB 这一级没有完成，F4.3-AC5
//     要求 3 不该被碰。
func TestRebaseFollowupConflictFailsPreservesSceneNoCascade(t *testing.T) {
	_, m, userID, repoID, repo, src := rebaseFollowupFixture(t)
	ctx := context.Background()

	// dev 上先加一个三行的共享文件，taskA/taskB 都会在第二行做冲突改动。
	gitOut(t, src, "checkout", "-q", "dev")
	if err := os.WriteFile(filepath.Join(src, "shared.txt"), []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, src, "add", "shared.txt")
	gitOut(t, src, "-c", "user.email=t@e.st", "-c", "user.name=t", "commit", "-qm", "加 shared.txt")

	taskA, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "RB-CONFLICT-A", LinearIssueID: "uuid-rb-conflict-a",
	})
	if err != nil {
		t.Fatalf("建 taskA 失败: %v", err)
	}
	taskB, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "RB-CONFLICT-B", LinearIssueID: "uuid-rb-conflict-b",
		DependsOn: &taskA.ID,
	})
	if err != nil {
		t.Fatalf("建 taskB 失败: %v", err)
	}
	taskC, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "RB-CONFLICT-C", LinearIssueID: "uuid-rb-conflict-c",
		DependsOn: &taskB.ID,
	})
	if err != nil {
		t.Fatalf("建 taskC 失败: %v", err)
	}

	wm, err := NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatalf("建工作区管理器失败: %v", err)
	}
	lin := newRebaseFollowupLinear()
	lin.add(rebaseFollowupIssue("uuid-rb-conflict-a", "RB-CONFLICT-A"))
	lin.add(rebaseFollowupIssue("uuid-rb-conflict-b", "RB-CONFLICT-B"))
	lin.add(rebaseFollowupIssue("uuid-rb-conflict-c", "RB-CONFLICT-C"))
	gh := &rebaseFollowupGitHub{pr: &github.PullRequest{
		Number: 910, URL: "https://github.com/acme/rebase-followup/pull/910",
	}}
	ag := multiStepAgent([]func(string) error{
		writeSharedFile("line1\nline2-A\nline3\n"), // taskA：改第二行
		writeSharedFile("line1\nline2-B\nline3\n"), // taskB：在 taskA 的基础上再改第二行
		func(dir string) error { // taskC：与 shared.txt 无关的新文件
			return os.WriteFile(filepath.Join(dir, "taskc_notes.txt"), []byte("taskC 产出\n"), 0o644)
		},
	})

	pipe := &Pipeline{
		Tasks: m, Worktrees: wm, Verifier: NewVerifier(3*time.Minute, ""),
		Agent: ag, Clients: &fakeClients{lin: lin, gh: gh}, Notifier: &fakeNotifier{},
		Verifications: &fakeVerifications{}, PermissionMode: "acceptEdits", SettingSources: "project",
	}

	if err := pipe.Execute(ctx, ExecuteParams{
		TaskID: taskA.ID, Repo: repo, CloneURL: src, IssueID: "uuid-rb-conflict-a", Actor: "node:test",
	}); err != nil {
		t.Fatalf("taskA Execute 失败: %v", err)
	}
	tA, err := m.Get(ctx, taskA.ID)
	if err != nil || tA.State != task.StatePROpen {
		t.Fatalf("taskA 应到达 pr_open，实际 state=%v err=%v", tA, err)
	}
	taskABranch := *tA.BranchName
	// 必须在任何改写发生之前捕获。
	taskATip := gitOut(t, *tA.WorktreePath, "rev-parse", "HEAD")

	if err := m.SetBaseRef(ctx, taskB.ID, &taskABranch); err != nil {
		t.Fatalf("设置 taskB.base_ref 失败: %v", err)
	}
	repoB := repo
	repoB.BaseRefOverride = taskABranch
	if err := pipe.Execute(ctx, ExecuteParams{
		TaskID: taskB.ID, Repo: repoB, CloneURL: src, IssueID: "uuid-rb-conflict-b", Actor: "node:test",
	}); err != nil {
		t.Fatalf("taskB Execute 失败: %v", err)
	}
	tB, err := m.Get(ctx, taskB.ID)
	if err != nil || tB.State != task.StatePROpen {
		t.Fatalf("taskB 应到达 pr_open，实际 state=%v err=%v", tB, err)
	}
	taskBBranch := *tB.BranchName

	if err := m.SetBaseRef(ctx, taskC.ID, &taskBBranch); err != nil {
		t.Fatalf("设置 taskC.base_ref 失败: %v", err)
	}
	repoC := repo
	repoC.BaseRefOverride = taskBBranch
	if err := pipe.Execute(ctx, ExecuteParams{
		TaskID: taskC.ID, Repo: repoC, CloneURL: src, IssueID: "uuid-rb-conflict-c", Actor: "node:test",
	}); err != nil {
		t.Fatalf("taskC Execute 失败: %v", err)
	}
	tC, err := m.Get(ctx, taskC.ID)
	if err != nil || tC.State != task.StatePROpen {
		t.Fatalf("taskC 应到达 pr_open，实际 state=%v err=%v", tC, err)
	}
	taskCWorktreeBefore := *tC.WorktreePath
	taskCHeadBefore := gitOut(t, taskCWorktreeBefore, "rev-parse", "HEAD")

	// ---- 制造冲突：dev 在 taskA 分叉之后，独立地在同一行改了不同内容 ----
	gitOut(t, src, "checkout", "-q", "dev")
	if err := os.WriteFile(filepath.Join(src, "shared.txt"), []byte("line1\nline2-DEV\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, src, "add", "shared.txt")
	gitOut(t, src, "-c", "user.email=t@e.st", "-c", "user.name=t", "commit", "-qm", "dev 上独立的冲突改动")
	devTip := gitOut(t, src, "rev-parse", "dev")

	notifier := &fakeNotifier{}
	mp := &MergePoller{
		Tasks: m, Worktrees: wm, Pipeline: pipe, Notifier: notifier,
		ClientFactory: fixedClientFactory{clients: &fakeClients{lin: lin, gh: gh}},
		RepoLookup:    repoLookupFixed(repo),
	}

	// ---- 顶层入口：等价于 taskA 合并触发的那一次调用 ----
	if err := mp.rebaseFollowup(ctx, taskABranch, taskATip, repo.DefaultBranch); err != nil {
		t.Fatalf("rebaseFollowup 本身不应返回 error（单个后继失败只影响它自己）: %v", err)
	}

	// ---- 断言一：taskB 转 failed，机器码正确 ----
	tBAfter, err := m.Get(ctx, taskB.ID)
	if err != nil {
		t.Fatalf("读取 taskB 失败: %v", err)
	}
	if tBAfter.State != task.StateFailed {
		t.Fatalf("taskB 应转 failed，实际 state=%s", tBAfter.State)
	}
	if tBAfter.FailureStage == nil || Stage(*tBAfter.FailureStage) != StageRebaseConflict {
		t.Errorf("taskB.failure_stage = %v，期望 %q", tBAfter.FailureStage, StageRebaseConflict)
	}
	if tBAfter.FailureReason == nil || *tBAfter.FailureReason == "" {
		t.Error("taskB 应有可读的失败原因")
	}

	// ---- 断言二：taskB 的 worktree 没被删除（现场保留） ----
	if tBAfter.WorktreePath == nil {
		t.Fatal("taskB.worktree_path 不应被清空")
	}
	if _, err := os.Stat(*tBAfter.WorktreePath); err != nil {
		t.Errorf("taskB 的 worktree 目录应仍存在（现场保留，供人工解决冲突）: %v", err)
	}

	// ---- 断言三：D4 三件套里的回帖与推送通知都发生了 ----
	if len(lin.comments["uuid-rb-conflict-b"]) == 0 {
		t.Error("taskB 的 issue 应收到失败回帖")
	} else {
		body := lin.comments["uuid-rb-conflict-b"][len(lin.comments["uuid-rb-conflict-b"])-1]
		if !containsAny(body, "rebase", "冲突") {
			t.Errorf("失败回帖应能看出是自动 rebase / 冲突相关，实际: %s", body)
		}
	}
	if len(notifier.msgs) == 0 {
		t.Error("应推送失败通知")
	}

	// ---- 断言四（核心）：taskC 完全没被动过，没有级联到更深层 ----
	tCAfter, err := m.Get(ctx, taskC.ID)
	if err != nil {
		t.Fatalf("读取 taskC 失败: %v", err)
	}
	if tCAfter.State != task.StatePROpen {
		t.Errorf("taskC 状态不应变化（taskB 没完成，F4.3-AC5 不该级联），实际 state=%s", tCAfter.State)
	}
	if tCAfter.BaseRef == nil || *tCAfter.BaseRef != taskBBranch {
		t.Errorf("taskC.base_ref 不应变化，应仍是 taskB 的分支名 %q，实际 %v", taskBBranch, tCAfter.BaseRef)
	}
	if tCAfter.WorktreePath == nil {
		t.Fatal("taskC.worktree_path 不应被清空")
	}
	taskCHeadAfter := gitOut(t, *tCAfter.WorktreePath, "rev-parse", "HEAD")
	if taskCHeadAfter != taskCHeadBefore {
		t.Errorf("taskC 的分支历史不应有任何变化，rebase 前 HEAD=%s，之后 HEAD=%s", taskCHeadBefore, taskCHeadAfter)
	}
	if isAncestor(t, *tCAfter.WorktreePath, devTip, "HEAD") {
		t.Error("taskC 不应以 dev 的新 tip 为祖先——它根本没被 rebase 过")
	}
}

// ----------------------------------------------------------------
// 场景三：深层 queued 后继在 rebase 冲突失败时被正确传播为
// blocked_dep，而不是静默永久卡在 queued——对抗式审查抓到的回归
// （failRebaseFollowup 复刻 pipeline.go 的 fail() 时漏掉了第五步
// PropagateBlocked）。
// ----------------------------------------------------------------

// TestFailRebaseFollowupPropagatesBlockedDepToQueuedSuccessors 造四层链
// task1→task2→task3→task4：task1 合并触发 rebase 跟进，task2（复用
// TestRebaseFollowupConflictFailsPreservesSceneNoCascade 同一套冲突制造
// 手法）在跟进 rebase 时冲突失败；关键是 task3/task4 全程【没有被
// Execute 过】——一直停在 queued，没有 worktree_path/branch_name。
//
// 这正是暴露该回归的关键设置：上一轮的测试（场景二 taskC）里后继提前
// 跑到了 pr_open，PropagateBlocked 缺位与否都看不出差别（pr_open 不是
// queued，PropagateBlocked 本来就不会碰它，跟这次测的"queued 后继"
// 是完全不同的路径）。只有后继仍是 queued，才会撞上 ClaimReady 的就绪
// 判定死循环：前驱转 failed 后 base_ref/前驱状态永远到不了
// pr_open/merged，C 就永远不满足就绪条件，若没有 PropagateBlocked 把
// 它转出 queued，它会静默永久卡住，不转 blocked_dep，不回帖——
// docs/07-prd-orchestration.md F2.3-AC1 明确禁止的情形。
func TestFailRebaseFollowupPropagatesBlockedDepToQueuedSuccessors(t *testing.T) {
	_, m, userID, repoID, repo, src := rebaseFollowupFixture(t)
	ctx := context.Background()

	// dev 上先加一个三行的共享文件，task1/task2 都会在第二行做冲突
	// 改动——与场景二完全相同的冲突制造手法。
	gitOut(t, src, "checkout", "-q", "dev")
	if err := os.WriteFile(filepath.Join(src, "shared.txt"), []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, src, "add", "shared.txt")
	gitOut(t, src, "-c", "user.email=t@e.st", "-c", "user.name=t", "commit", "-qm", "加 shared.txt")

	task1, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "RB-DEEP-1", LinearIssueID: "uuid-rb-deep-1",
	})
	if err != nil {
		t.Fatalf("建 task1 失败: %v", err)
	}
	task2, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "RB-DEEP-2", LinearIssueID: "uuid-rb-deep-2",
		DependsOn: &task1.ID,
	})
	if err != nil {
		t.Fatalf("建 task2 失败: %v", err)
	}
	// task3/task4 建了就不再动——全程停在 queued，没有 worktree。
	task3, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "RB-DEEP-3", LinearIssueID: "uuid-rb-deep-3",
		DependsOn: &task2.ID,
	})
	if err != nil {
		t.Fatalf("建 task3 失败: %v", err)
	}
	task4, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "RB-DEEP-4", LinearIssueID: "uuid-rb-deep-4",
		DependsOn: &task3.ID,
	})
	if err != nil {
		t.Fatalf("建 task4 失败: %v", err)
	}

	wm, err := NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatalf("建工作区管理器失败: %v", err)
	}
	lin := newRebaseFollowupLinear()
	lin.add(rebaseFollowupIssue("uuid-rb-deep-1", "RB-DEEP-1"))
	lin.add(rebaseFollowupIssue("uuid-rb-deep-2", "RB-DEEP-2"))
	lin.add(rebaseFollowupIssue("uuid-rb-deep-3", "RB-DEEP-3"))
	lin.add(rebaseFollowupIssue("uuid-rb-deep-4", "RB-DEEP-4"))
	gh := &rebaseFollowupGitHub{pr: &github.PullRequest{
		Number: 920, URL: "https://github.com/acme/rebase-followup/pull/920",
	}}
	ag := multiStepAgent([]func(string) error{
		writeSharedFile("line1\nline2-A\nline3\n"), // task1：改第二行
		writeSharedFile("line1\nline2-B\nline3\n"), // task2：在 task1 的基础上再改第二行
	})

	pipe := &Pipeline{
		Tasks: m, Worktrees: wm, Verifier: NewVerifier(3*time.Minute, ""),
		Agent: ag, Clients: &fakeClients{lin: lin, gh: gh}, Notifier: &fakeNotifier{},
		Verifications: &fakeVerifications{}, PermissionMode: "acceptEdits", SettingSources: "project",
	}

	if err := pipe.Execute(ctx, ExecuteParams{
		TaskID: task1.ID, Repo: repo, CloneURL: src, IssueID: "uuid-rb-deep-1", Actor: "node:test",
	}); err != nil {
		t.Fatalf("task1 Execute 失败: %v", err)
	}
	t1, err := m.Get(ctx, task1.ID)
	if err != nil || t1.State != task.StatePROpen {
		t.Fatalf("task1 应到达 pr_open，实际 state=%v err=%v", t1, err)
	}
	task1Branch := *t1.BranchName
	// 必须在任何改写发生之前捕获。
	task1Tip := gitOut(t, *t1.WorktreePath, "rev-parse", "HEAD")

	if err := m.SetBaseRef(ctx, task2.ID, &task1Branch); err != nil {
		t.Fatalf("设置 task2.base_ref 失败: %v", err)
	}
	repo2 := repo
	repo2.BaseRefOverride = task1Branch
	if err := pipe.Execute(ctx, ExecuteParams{
		TaskID: task2.ID, Repo: repo2, CloneURL: src, IssueID: "uuid-rb-deep-2", Actor: "node:test",
	}); err != nil {
		t.Fatalf("task2 Execute 失败: %v", err)
	}
	t2, err := m.Get(ctx, task2.ID)
	if err != nil || t2.State != task.StatePROpen {
		t.Fatalf("task2 应到达 pr_open，实际 state=%v err=%v", t2, err)
	}

	// ---- 前置条件：task3/task4 从未被派发，仍是 queued，没有
	// worktree_path/branch_name。这是暴露本回归的关键设置。 ----
	t3Before, err := m.Get(ctx, task3.ID)
	if err != nil {
		t.Fatalf("读取 task3 失败: %v", err)
	}
	if t3Before.State != task.StateQueued || t3Before.WorktreePath != nil {
		t.Fatalf("前置条件不满足：task3 应是 queued 且没有 worktree，实际 state=%s worktree=%v",
			t3Before.State, t3Before.WorktreePath)
	}
	t4Before, err := m.Get(ctx, task4.ID)
	if err != nil {
		t.Fatalf("读取 task4 失败: %v", err)
	}
	if t4Before.State != task.StateQueued {
		t.Fatalf("前置条件不满足：task4 应是 queued，实际 state=%s", t4Before.State)
	}

	// ---- 制造冲突：dev 在 task1 分叉之后，独立地在同一行改了不同内容 ----
	gitOut(t, src, "checkout", "-q", "dev")
	if err := os.WriteFile(filepath.Join(src, "shared.txt"), []byte("line1\nline2-DEV\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, src, "add", "shared.txt")
	gitOut(t, src, "-c", "user.email=t@e.st", "-c", "user.name=t", "commit", "-qm", "dev 上独立的冲突改动")

	notifier := &fakeNotifier{}
	mp := &MergePoller{
		Tasks: m, Worktrees: wm, Pipeline: pipe, Notifier: notifier,
		ClientFactory: fixedClientFactory{clients: &fakeClients{lin: lin, gh: gh}},
		RepoLookup:    repoLookupFixed(repo),
	}

	// ---- 顶层入口：等价于 task1 合并触发的那一次调用 ----
	if err := mp.rebaseFollowup(ctx, task1Branch, task1Tip, repo.DefaultBranch); err != nil {
		t.Fatalf("rebaseFollowup 本身不应返回 error（单个后继失败只影响它自己）: %v", err)
	}

	// ---- 断言一：task2 转 failed（rebase 冲突） ----
	t2After, err := m.Get(ctx, task2.ID)
	if err != nil {
		t.Fatalf("读取 task2 失败: %v", err)
	}
	if t2After.State != task.StateFailed {
		t.Fatalf("task2 应转 failed，实际 state=%s", t2After.State)
	}

	// ---- 断言二（本次要验证的核心）：task3 转 blocked_dep，不是停留
	// 在 queued！ ----
	t3After, err := m.Get(ctx, task3.ID)
	if err != nil {
		t.Fatalf("读取 task3 失败: %v", err)
	}
	if t3After.State != task.StateBlockedDep {
		t.Fatalf("task3 应转 blocked_dep（不是停留在 queued！），实际 state=%s", t3After.State)
	}

	// ---- 断言三：task3 收到回帖，说明因 task2 失败而阻塞 ----
	if len(lin.comments["uuid-rb-deep-3"]) == 0 {
		t.Error("task3 应收到阻塞回帖")
	} else {
		body := lin.comments["uuid-rb-deep-3"][len(lin.comments["uuid-rb-deep-3"])-1]
		if !containsAny(body, "阻塞", "blocked") {
			t.Errorf("阻塞回帖应能看出因前驱失败而阻塞，实际: %s", body)
		}
		if !containsAny(body, "rebase", "冲突") {
			t.Errorf("阻塞回帖应能看出前驱是因 rebase 冲突失败，实际: %s", body)
		}
	}

	// ---- 断言四：task4（间接后继，depends_on=task3）也传播到
	// blocked_dep——PropagateBlocked 本身已处理传递闭包，这里只确认
	// 接线正确。 ----
	t4After, err := m.Get(ctx, task4.ID)
	if err != nil {
		t.Fatalf("读取 task4 失败: %v", err)
	}
	if t4After.State != task.StateBlockedDep {
		t.Errorf("task4（间接后继）应转 blocked_dep，实际 state=%s", t4After.State)
	}
	if len(lin.comments["uuid-rb-deep-4"]) == 0 {
		t.Error("task4（间接后继）也应收到阻塞回帖")
	}
}

// containsAny 报告 s 是否包含 subs 中的任意一个子串。
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
