package runner

import (
	"context"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
	"github.com/zichuanwangcloud-gif/lathe/internal/integration/github"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// rebase_followup_driver_test.go —— f06 AC-25：Execute 的第二个调用点
// （MergePoller.rebaseFollowup → EntryVerify → 修复回路）也走任务钉死的驱动。
// 它的 RepoConfig 来自只查 repos 的 RepoLookup，里面没有驱动；驱动只能来自任务行。
func TestRebaseFollowupCodexFixLoopKeepsDriver(t *testing.T) {
	cases := []struct {
		name string
		// driver 是 task2 建任务那刻仓库的驱动（之后仓库改回 claude）
		driver string
		// nullSession 为真时 task2 没有会话（fix-1 开新会话）
		nullSession bool
		reported    string
		wantResume  bool
		wantFixSess string // fix-1 收到的 SessionID（空 = 不检查，新会话是预分配 UUID）
		wantFinal   string // 结束后 agent_session_id（空 = 等于 fix-1 收到的 SessionID）
	}{
		{name: "给定 task2 钉死 codex 且有会话 019d-thread 断言 fix-1 resume codex 原会话",
			driver: agent.DriverCodex, reported: "019d-thread", wantResume: true,
			wantFixSess: "019d-thread", wantFinal: "019d-thread"},
		{name: "给定 task2 钉死 codex 且无会话 断言 fix-1 新会话仍是 codex 并采纳上报的 019e-new",
			driver: agent.DriverCodex, nullSession: true, reported: "019e-new", wantResume: false,
			wantFinal: "019e-new"},
		{name: "给定 task2 钉死 claude（对照）断言 fix-1 收到 claude",
			driver: agent.DriverClaude, wantResume: true, wantFixSess: "019d-thread", wantFinal: "019d-thread"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool, m, userID, repoID, repo, src := rebaseFollowupFixture(t)
			ctx := context.Background()
			st, err := store.Open(ctx, dsnForTest())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()

			nonce := time.Now().Format("150405.000000")
			task1, err := m.Create(ctx, task.CreateParams{
				UserID: userID, RepoID: repoID, ExternalKey: "RBD-1-" + nonce, ExternalID: "uuid-rbd-1",
			})
			if err != nil {
				t.Fatal(err)
			}
			// task2 建任务那一刻仓库是 tc.driver；建完把仓库改回 claude ——
			// 之后任何经 RepoConfig 拿驱动的实现都会拿错
			if _, err := pool.Exec(ctx, `UPDATE repos SET agent_driver = $2 WHERE id = $1`, repoID, tc.driver); err != nil {
				t.Fatal(err)
			}
			task2, err := m.Create(ctx, task.CreateParams{
				UserID: userID, RepoID: repoID, ExternalKey: "RBD-2-" + nonce, ExternalID: "uuid-rbd-2",
				DependsOn: &task1.ID,
			})
			if err != nil {
				t.Fatal(err)
			}
			if task2.AgentDriver != tc.driver {
				t.Fatalf("task2 应钉死为 %s，got %q", tc.driver, task2.AgentDriver)
			}
			if _, err := pool.Exec(ctx, `UPDATE repos SET agent_driver = 'claude' WHERE id = $1`, repoID); err != nil {
				t.Fatal(err)
			}

			wm, err := NewWorktreeManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			lin := newRebaseFollowupLinear()
			lin.add(rebaseFollowupIssue("uuid-rbd-1", "RBD-1"))
			lin.add(rebaseFollowupIssue("uuid-rbd-2", "RBD-2"))
			gh := &rebaseFollowupGitHub{pr: &github.PullRequest{Number: 910, URL: "https://github.com/acme/rbd/pull/910"}}
			ag := &driverAgent{steps: []driverStep{
				{res: &agent.Result{Success: true, Text: triageOK}},
				{res: &agent.Result{Success: true, Text: "task1 完成"}, mutate: writeReadme("# task1 的说明\n")},
				{res: &agent.Result{Success: true, Text: triageOK}},
				{res: &agent.Result{Success: true, Text: "task2 完成"}, mutate: writeGreet(greetRight)},
				// rebase 跟进重验里的 fix-1
				{res: &agent.Result{Success: true, Text: "修好了"}, mutate: writeFix(greetRight), session: tc.reported,
					emit: []string{`{"type":"thread.started","thread_id":"` + tc.reported + `"}`}},
			}}
			pipe := &Pipeline{
				Tasks: m, Worktrees: wm, Verifier: NewVerifier(3*time.Minute, ""),
				Agent: ag, Clients: &fakeClients{lin: lin, gh: gh}, Notifier: &fakeNotifier{},
				Verifications: &fakeVerifications{}, AgentEvents: st,
				PermissionMode: "acceptEdits", SettingSources: "project", MaxFixAttempts: 2,
			}

			if err := pipe.Execute(ctx, ExecuteParams{TaskID: task1.ID, Repo: repo, CloneURL: src, IssueRef: "uuid-rbd-1", Actor: "node:test"}); err != nil {
				t.Fatalf("task1 Execute 失败: %v", err)
			}
			t1, _ := m.Get(ctx, task1.ID)
			if t1.State != task.StatePROpen {
				t.Fatalf("task1 应到 pr_open，got %s", t1.State)
			}
			task1Branch := *t1.BranchName
			task1Tip := gitOut(t, *t1.WorktreePath, "rev-parse", "HEAD")

			if err := m.SetBaseRef(ctx, task2.ID, &task1Branch); err != nil {
				t.Fatal(err)
			}
			repo2 := repo
			repo2.BaseRefOverride = task1Branch
			if err := pipe.Execute(ctx, ExecuteParams{TaskID: task2.ID, Repo: repo2, CloneURL: src, IssueRef: "uuid-rbd-2", Actor: "node:test"}); err != nil {
				t.Fatalf("task2 Execute 失败: %v", err)
			}
			t2, _ := m.Get(ctx, task2.ID)
			if t2.State != task.StatePROpen {
				t.Fatalf("task2 应到 pr_open，got %s（%v）", t2.State, t2.FailureReason)
			}

			// 预置一个让验证失败的改动（已提交）：重验时 heavy 档绿阶段必挂 → 修复回路
			wt2 := &Worktree{Path: *t2.WorktreePath, Branch: *t2.BranchName}
			if err := writeFix(greetWrong)(wt2.Path); err != nil {
				t.Fatal(err)
			}
			if err := wm.Commit(ctx, wt2, "chore: 测试预置的回归"); err != nil {
				t.Fatalf("提交预置回归失败: %v", err)
			}
			if err := m.SetSessionID(ctx, task2.ID, "019d-thread"); err != nil {
				t.Fatal(err)
			}
			if tc.nullSession {
				// 先走合法边到 implementing（review_feedback→implementing 要求持有会话），
				// 再清掉会话：reenterForVerify 见到 implementing 原样跳过
				for _, s := range []task.State{task.StateReviewFeedback, task.StateImplementing} {
					if _, err := m.Transition(ctx, task2.ID, s, "test", nil); err != nil {
						t.Fatalf("转 %s 失败: %v", s, err)
					}
				}
				if _, err := pool.Exec(ctx, `UPDATE tasks SET agent_session_id = NULL WHERE id = $1`, task2.ID); err != nil {
					t.Fatal(err)
				}
			}

			gitOut(t, src, "checkout", "-q", "dev")
			gitOut(t, src, "-c", "user.email=t@e.st", "-c", "user.name=t", "cherry-pick", "-x", task1Tip)

			before := len(ag.snapshot())
			mp := &MergePoller{Tasks: m, Worktrees: wm, Pipeline: pipe, Notifier: &fakeNotifier{}, RepoLookup: repoLookupFixed(repo)}
			if err := mp.rebaseFollowup(ctx, task1Branch, task1Tip, repo.DefaultBranch); err != nil {
				t.Fatalf("rebaseFollowup 失败: %v", err)
			}

			calls := ag.snapshot()[before:]
			if len(calls) == 0 {
				t.Fatal("重验应进入修复回路调用 agent（EntryVerify 不跑分诊与实现，第一次就是 fix-1）")
			}
			for i, c := range calls {
				if c.Driver != tc.driver {
					t.Errorf("rebase 跟进里第 %d 次 Run 的 Driver got %q want %q", i, c.Driver, tc.driver)
				}
			}
			fix1 := calls[0]
			if fix1.Resume != tc.wantResume {
				t.Errorf("fix-1 Resume got %v want %v", fix1.Resume, tc.wantResume)
			}
			if tc.wantFixSess != "" && fix1.SessionID != tc.wantFixSess {
				t.Errorf("fix-1 SessionID got %q want %q", fix1.SessionID, tc.wantFixSess)
			}
			var fixRows int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_events WHERE task_id = $1 AND phase = 'fix-1'`, task2.ID).Scan(&fixRows); err != nil {
				t.Fatal(err)
			}
			if fixRows == 0 {
				t.Error("agent_events 里应有 phase='fix-1' 的行（交叉确认 calls[0] 就是 fix-1）")
			}
			want := tc.wantFinal
			if want == "" {
				want = fix1.SessionID
			}
			if got := sessionOf(t, pool, task2.ID); got != want {
				t.Errorf("结束后 agent_session_id got %q want %q", got, want)
			}
			final, _ := m.Get(ctx, task2.ID)
			if final.State != task.StatePROpen {
				t.Errorf("修复后 task2 应回到 pr_open，got %s（%v）", final.State, final.FailureReason)
			}
		})
	}
}
