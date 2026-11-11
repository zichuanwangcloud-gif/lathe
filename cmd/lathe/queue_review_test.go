package main

import (
	"context"
	"strings"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/github"
	"github.com/zichuanwangcloud-gif/lathe/internal/runner"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// PR 评论回流（A2）的派发侧：ReviewIntake 把任务转回 queued 并写下
// mode=review，worker 领到后应从事件流回读出 review 模式、产出二轮计划。
//
// 刻意先埋一条第一轮的崩溃恢复事件（interrupted_state=implementing）：
// 回流若不显式重置 interrupted_state，派发侧会把这条旧值当成「二轮途中
// 崩溃」，走普通崩溃恢复而不是 review 二轮。两条路径的决策理由文案不同，
// 据此区分。
func TestRunOneClaimedDispatchesReviewRound(t *testing.T) {
	st := testStore(t)
	userID, repoID := fixture(t, st)
	ctx := context.Background()
	pipe := &fakePipeline{}
	q := testQueue(st, pipe)

	tk, err := q.tasks.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: uniqueKey("Q-REVIEW"),
		ExternalID: uniqueKey("uuid-review"),
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	// 第一轮：实现途中进程崩溃 → Reconcile 转回 queued（留下 interrupted_state）
	for _, s := range []task.State{task.StateTriaging, task.StateImplementing} {
		if _, err := q.tasks.Transition(ctx, tk.ID, s, "system", nil); err != nil {
			t.Fatalf("转 %s 失败: %v", s, err)
		}
	}
	if err := q.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile 失败: %v", err)
	}
	// 恢复后跑完第一轮、开出 PR。worktree 路径故意不存在：现场不可用时
	// review 计划给出的是「从头重建」，而理由文案能区分走的是哪条路径。
	if _, err := q.tasks.Transition(ctx, tk.ID, task.StateImplementing, "system", &task.TransitionOpts{
		AgentSessionID: ptr("sess-round-1"),
		WorktreePath:   ptr("/tmp/does-not-exist-" + uniqueKey("wt")),
		BranchName:     ptr("fix/does-not-exist"),
	}); err != nil {
		t.Fatalf("转 implementing 失败: %v", err)
	}
	for _, s := range []task.State{task.StateVerifying, task.StatePROpen} {
		if _, err := q.tasks.Transition(ctx, tk.ID, s, "system", nil); err != nil {
			t.Fatalf("转 %s 失败: %v", s, err)
		}
	}
	if err := q.tasks.SetPRNumber(ctx, tk.ID, 5); err != nil {
		t.Fatalf("SetPRNumber 失败: %v", err)
	}
	var repoName string
	if err := st.Pool().QueryRow(ctx, `SELECT provider_repo FROM repos WHERE id = $1`, repoID).Scan(&repoName); err != nil {
		t.Fatalf("读仓库名失败: %v", err)
	}

	out, err := (&runner.ReviewIntake{Tasks: q.tasks}).AcceptReview(ctx, userID, &github.ReviewEvent{
		Event: github.EventReviewComment, Action: "created", Repo: repoName, PRNumber: 5,
		PRAuthor: "lathe-bot", Key: "inline:1", Author: "alice", Body: "改一下",
	}, "deliv-q")
	if err != nil || out.Status != runner.ReviewQueued {
		t.Fatalf("回流失败：%+v %v", out, err)
	}

	claimed := claimOwn(t, q, ctx, userID)
	if claimed == nil || claimed.ID != tk.ID {
		t.Fatalf("应领到回流的任务，得到 %v", claimed)
	}
	q.runOneClaimed(ctx, claimed)

	calls := pipe.snapshot()
	if len(calls) != 1 || calls[0].Retry == nil {
		t.Fatalf("Execute 应被调用 1 次且带计划，得到 %+v", calls)
	}
	reasons := strings.Join(calls[0].Retry.Reasons, " | ")
	if !strings.Contains(reasons, "收到 PR review 意见") {
		t.Errorf("应按 review 二轮决策（mode=review 回读成功、旧 interrupted_state 已被重置），实际理由：%s", reasons)
	}
}

// f01 AC-16 搭配的 A 类红线：Reconcile 只动 triaging/implementing/verifying，
// 终态与 pr_open 任务一个都不碰（启动清扫插在它之后，不能改变它的语义）。
func TestReconcileDoesNotTouchTerminalTasks(t *testing.T) {
	st := testStore(t)
	userID, repoID := fixture(t, st)
	ctx := context.Background()
	q := testQueue(st, &fakePipeline{})

	mk := func(path []task.State) *task.Task {
		tk, err := q.tasks.Create(ctx, task.CreateParams{
			UserID: userID, RepoID: repoID, ExternalKey: uniqueKey("Q-RECON"),
		})
		if err != nil {
			t.Fatalf("Create 失败: %v", err)
		}
		for _, s := range path {
			if _, err := q.tasks.Transition(ctx, tk.ID, s, "system", nil); err != nil {
				t.Fatalf("转 %s 失败: %v", s, err)
			}
		}
		return tk
	}
	cases := []struct {
		name string
		path []task.State
		want task.State
	}{
		{"pr_open 不动", []task.State{task.StateTriaging, task.StateImplementing, task.StateVerifying, task.StatePROpen}, task.StatePROpen},
		{"failed 不动", []task.State{task.StateTriaging, task.StateFailed}, task.StateFailed},
		{"cancelled 不动", []task.State{task.StateCancelled}, task.StateCancelled},
		{"implementing 转回 queued", []task.State{task.StateTriaging, task.StateImplementing}, task.StateQueued},
	}
	ids := make([]int64, len(cases))
	for i, tc := range cases {
		ids[i] = mk(tc.path).ID
	}
	if err := q.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := q.tasks.Get(ctx, ids[i])
			if err != nil {
				t.Fatal(err)
			}
			if got.State != tc.want {
				t.Errorf("state got=%s want=%s", got.State, tc.want)
			}
		})
	}
}
