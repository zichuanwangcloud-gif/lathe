package httpapi

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/Clouditera/lathe/internal/runner"
	"github.com/Clouditera/lathe/internal/task"
)

// mode=approved 是人工闸门放行的内部信号，只能由 approveTask 构造。
//
// 这条用例锁死的是 PR #5 的闸门不被自己的重试端点绕开：修复前 retryTask
// 用 mode.Valid() 校验，而 Valid 认 RetryApproved，于是持 token 的合法
// 用户对【任意自己名下的任务】POST {"mode":"approved"} 就能让任务走
// EntryPush（跳过实现与验证，直接补 push + 开 PR）—— 闸门形同虚设，
// 连没停在闸门上的任务也能被这样推走。
//
// 断言三件事：400 拒绝、任务状态没被动过、没有发生重派。
func TestAPIRetryRejectsApprovedMode(t *testing.T) {
	api, _, m, repoID := apiFixture(t)
	q := &fakeEnqueuer{}
	api.Queue = q
	api.Scenes = fakeScenes{state: &runner.WorktreeState{
		Exists: true, Registered: true, BranchExists: true, HasCommits: true, Commits: 3,
	}}
	srv := apiServer(t, api)
	ctx := context.Background()

	var userID int64
	_ = api.Store.Pool().QueryRow(ctx, `SELECT user_id FROM repos WHERE id=$1`, repoID).Scan(&userID)

	// 一条普通的失败任务：有可用现场，只要 mode 能通过校验就会走 EntryPush。
	tk, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "CR-APPROVED-GUARD",
	})
	if err != nil {
		t.Fatal(err)
	}
	wtPath, branch, stage := "/tmp/scene", "fix/cr-approved-guard", string(runner.StagePush)
	if _, err := m.Transition(ctx, tk.ID, task.StateTriaging, "test", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Transition(ctx, tk.ID, task.StateImplementing, "test", &task.TransitionOpts{
		WorktreePath: &wtPath, BranchName: &branch,
	}); err != nil {
		t.Fatal(err)
	}
	reason := "构造失败"
	if _, err := m.Transition(ctx, tk.ID, task.StateFailed, "test", &task.TransitionOpts{
		FailureReason: &reason, FailureStage: &stage,
	}); err != nil {
		t.Fatal(err)
	}

	for _, body := range []string{`{"mode":"approved"}`,
		`{"mode":" approved "}`, `{"mode":"APPROVED"}`} {
		resp := do(t, srv, "POST", "/api/tasks/"+itoa(tk.ID)+"/retry", body, true)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("retry 传 mode=%s 应 400，得到 %d", body, resp.StatusCode)
		}
	}

	after, err := m.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != task.StateFailed {
		t.Errorf("被拒绝的重试不该改动状态，得到 %s", after.State)
	}
	if len(q.requeued) != 0 {
		t.Errorf("被拒绝的重试不该重派任务，requeued=%v", q.requeued)
	}
}

// approveTask 的状态前提必须落在持行锁的转移里判定，不能只靠锁外那次读。
//
// 两个并发确认请求都会读到 awaiting_approval 并双双走过锁外检查；
// 修复前两边都会各自发出一次 Transition，于是同一个任务被重派两次
// （放行是副作用不幂等的动作：重推一次分支、再开一次 PR）。这里要求
// 恰好一个成功、另一个以冲突被拒，且事件流里只留下一次放行。
func TestAPIApproveConcurrentOnlyOneWins(t *testing.T) {
	api, _, m, repoID := apiFixture(t)
	q := &fakeEnqueuer{}
	api.Queue = q
	api.Scenes = fakeScenes{state: &runner.WorktreeState{
		Exists: true, Registered: true, BranchExists: true, HasCommits: true, Commits: 2,
	}}
	srv := apiServer(t, api)
	ctx := context.Background()

	tk := awaitingTask(t, api, m, repoID, "CR-APPROVE-CONCURRENT")

	const n = 4
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := do(t, srv, "POST", "/api/tasks/"+itoa(tk.ID)+"/approve", "", true)
			codes[i] = resp.StatusCode
		}()
	}
	wg.Wait()

	ok, conflict := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		default:
			t.Errorf("并发确认只该产生 200 或 409，得到 %d（%v）", c, codes)
		}
	}
	if ok != 1 {
		t.Errorf("并发确认应恰好一个成功，得到 %d 个 200：%v", ok, codes)
	}
	if conflict != n-1 {
		t.Errorf("其余 %d 个请求应以冲突被拒，得到 %d 个 409：%v", n-1, conflict, codes)
	}

	// 只放行一次：转 queued 的转移只能有一条，重派也只能有一次。
	events, err := m.Events(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	approvals := 0
	for _, e := range events {
		if e.Payload == nil {
			continue
		}
		if v, ok := e.Payload["mode"]; ok && v == string(runner.RetryApproved) {
			approvals++
		}
	}
	if approvals != 1 {
		t.Errorf("放行只应发生一次，事件流里有 %d 条 mode=approved", approvals)
	}
	if len(q.requeued) != 1 {
		t.Errorf("并发确认后应恰好重派一次，requeued=%v", q.requeued)
	}
}
