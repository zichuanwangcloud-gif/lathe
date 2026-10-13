package httpapi

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/runner"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
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
		UserID: userID, RepoID: repoID, ExternalKey: "CR-APPROVED-GUARD",
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

// MAJOR-1（A2 审计）：review_feedback → queued 这条新边不能被通用重试端点
// 当成「任意状态 → queued」的入口用。
//
// 修复前 retryTask 从任意状态转 queued 且只校验 mode，于是
// {"mode":"fresh"} 与 {"mode":"auto"} 都返回 200 把 parked 的任务带回
// queued —— 接着派发侧按 fresh 走「丢弃现场 + 分诊」：第一轮早已推到远端
// 的分支会在二轮推送时撞非快进失败（整轮白烧），PR 上的 review 意见也
// 一并丢掉。这正是 parking 要避免的事。
//
// 断言：通用重试端点对 review_feedback 一律 409、状态没被动过、没有重派；
// 只有显式的 review-retry 端点能把它带走。
func TestAPIRetryRejectsParkedReviewFeedback(t *testing.T) {
	api, _, m, repoID := apiFixture(t)
	q := &fakeEnqueuer{}
	api.Queue = q
	// 现场可用：通用重试在修复前正是靠这一点一路走到转 queued 的
	api.Scenes = fakeScenes{state: &runner.WorktreeState{
		Exists: true, Registered: true, BranchExists: true, HasCommits: true, Commits: 2,
	}}
	srv := apiServer(t, api)
	ctx := context.Background()

	// 两种 parked：有会话的（约束①已满足，修复前通用重试会一路 200 走
	// PlanRetry(fresh) 丢弃现场）与没有会话的（约束①无从满足）。
	// 两种都必须只能走显式出口。
	tk := parkedReviewTask(t, api, m, repoID, "CR-PARKED-GUARD", true)

	for _, body := range []string{`{"mode":"fresh"}`, `{"mode":"auto"}`, `{"mode":"resume"}`, ``} {
		resp := do(t, srv, "POST", "/api/tasks/"+itoa(tk.ID)+"/retry", body, true)
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("对 review_feedback 的通用重试（body=%q）应 409，得到 %d", body, resp.StatusCode)
		}
	}

	after, err := m.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != task.StateReviewFeedback {
		t.Errorf("被拒的通用重试不该改动状态，得到 %s", after.State)
	}
	if len(q.requeued) != 0 {
		t.Errorf("被拒的通用重试不该重派任务，requeued=%v", q.requeued)
	}

	// 没有会话的 parked 同样只能走显式出口（约束①在这里无从满足，
	// 通用重试更不该把它带去任何地方）
	noSess := parkedReviewTask(t, api, m, repoID, "CR-PARKED-NOSESS", false)
	for _, body := range []string{`{"mode":"fresh"}`, `{"mode":"auto"}`, ``} {
		resp := do(t, srv, "POST", "/api/tasks/"+itoa(noSess.ID)+"/retry", body, true)
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("对无会话 parked 任务的通用重试（body=%q）应 409，得到 %d", body, resp.StatusCode)
		}
	}
	if got, _ := m.Get(ctx, noSess.ID); got.State != task.StateReviewFeedback {
		t.Errorf("无会话的 parked 任务状态不该被动过，得到 %s", got.State)
	}

	// 显式出口：同一任务经 review-retry 可以出队
	resp := do(t, srv, "POST", "/api/tasks/"+itoa(tk.ID)+"/review-retry", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("显式 review-retry 应 200，得到 %d", resp.StatusCode)
	}
	after, err = m.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != task.StateQueued {
		t.Errorf("review-retry 后 state = %s，want queued", after.State)
	}
	// 重派时必须带内部模式：派发侧据此走 review 二轮（不是 fresh/auto）
	if len(q.requeued) != 1 || q.modes[0] != string(runner.RetryReviewRetry) {
		t.Errorf("应恰好重派一次且带 review-retry 模式，requeued=%v modes=%v", q.requeued, q.modes)
	}
	// 事件流里记下的是内部模式，而不是用户可传的 auto/fresh
	events, err := m.Events(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Payload["mode"] != string(runner.RetryReviewRetry) {
		t.Errorf("转 queued 的事件应记 mode=%s，得到 %v", runner.RetryReviewRetry, last.Payload["mode"])
	}
}

// review-retry 只认真的停在 review_feedback 的任务：不加这道检查，它就成了
// 一台「把任意任务推去重建现场 + 跑 agent」的机器（与 approveTask 同一立场）。
func TestAPIReviewRetryRejectsOtherStates(t *testing.T) {
	api, _, m, repoID := apiFixture(t)
	q := &fakeEnqueuer{}
	api.Queue = q
	api.Scenes = fakeScenes{state: &runner.WorktreeState{Exists: true, Registered: true, BranchExists: true}}
	srv := apiServer(t, api)
	ctx := context.Background()

	var userID int64
	if err := api.Store.Pool().QueryRow(ctx, `SELECT user_id FROM repos WHERE id=$1`, repoID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	// 一条普通的 queued 任务
	tk, err := m.Create(ctx, task.CreateParams{UserID: userID, RepoID: repoID, ExternalKey: "CR-NOT-PARKED"})
	if err != nil {
		t.Fatal(err)
	}

	resp := do(t, srv, "POST", "/api/tasks/"+itoa(tk.ID)+"/review-retry", "", true)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("非 review_feedback 状态应 409，得到 %d", resp.StatusCode)
	}
	if len(q.requeued) != 0 {
		t.Errorf("被拒的请求不该重派，requeued=%v", q.requeued)
	}
}

// parkedReviewTask 造一条真的停在 review_feedback 的任务：开出 PR、记下
// pr_number，然后走 pr_open → review_feedback（与回流入口同一条边）。
// withSession 控制它是否持有可 resume 的会话 —— 两种都在 parked 的现实
// 形态里（有会话但现场不可用、或会话丢了）。
func parkedReviewTask(t *testing.T, api *API, m *task.Machine, repoID int64, key string, withSession bool) *task.Task {
	t.Helper()
	ctx := context.Background()
	var userID int64
	if err := api.Store.Pool().QueryRow(ctx, `SELECT user_id FROM repos WHERE id=$1`, repoID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	tk, err := m.Create(ctx, task.CreateParams{UserID: userID, RepoID: repoID, ExternalKey: key})
	if err != nil {
		t.Fatal(err)
	}
	wtPath, branch := "/tmp/scene-parked", "fix/"+strings.ToLower(key)
	opts := &task.TransitionOpts{WorktreePath: &wtPath, BranchName: &branch}
	if withSession {
		sess := "sess-parked"
		opts.AgentSessionID = &sess
	}
	if _, err := m.Transition(ctx, tk.ID, task.StateImplementing, "test", opts); err != nil {
		t.Fatal(err)
	}
	for _, s := range []task.State{task.StateVerifying, task.StatePROpen} {
		if _, err := m.Transition(ctx, tk.ID, s, "test", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.SetPRNumber(ctx, tk.ID, 4242); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Transition(ctx, tk.ID, task.StateReviewFeedback, "system:github-review", nil); err != nil {
		t.Fatal(err)
	}
	return tk
}
