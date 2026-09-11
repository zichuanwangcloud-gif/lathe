package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/Clouditera/lathe/internal/runner"
	"github.com/Clouditera/lathe/internal/task"
)

// awaitingTask 造一条停在 awaiting_approval 的任务（gate_mode=manual 走到的位置）。
func awaitingTask(t *testing.T, api *API, m *task.Machine, repoID int64, key string) *task.Task {
	t.Helper()
	ctx := context.Background()
	var userID int64
	if err := api.Store.Pool().QueryRow(ctx,
		`SELECT user_id FROM repos WHERE id=$1`, repoID).Scan(&userID); err != nil {
		t.Fatalf("查 repo 属主失败: %v", err)
	}
	tk, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: key, GateMode: task.GateManual,
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	wtPath, branch := "/tmp/scene", "fix/"+key
	for _, step := range []struct {
		to   task.State
		opts *task.TransitionOpts
	}{
		{task.StateTriaging, nil},
		{task.StateImplementing, &task.TransitionOpts{WorktreePath: &wtPath, BranchName: &branch}},
		{task.StateVerifying, nil},
		{task.StateAwaitingApproval, nil},
	} {
		if _, err := m.Transition(ctx, tk.ID, step.to, "test", step.opts); err != nil {
			t.Fatalf("转移到 %s 失败: %v", step.to, err)
		}
	}
	return tk
}

// 确认端点把任务转回 queued，并在事件 payload 里留下 mode=approved ——
// worker 回读它才能给出 EntryPush（只补 push + 开 PR，不重跑实现与验证）。
//
// docs/08-debt-cleanup.md T2 AC3。
func TestAPIApproveGatedTask(t *testing.T) {
	api, _, m, repoID := apiFixture(t)
	q := &fakeEnqueuer{}
	api.Queue = q
	api.Scenes = fakeScenes{state: &runner.WorktreeState{
		Exists: true, Registered: true, BranchExists: true, HasCommits: true, Commits: 2,
	}}
	srv := apiServer(t, api)
	ctx := context.Background()

	tk := awaitingTask(t, api, m, repoID, "CR-APPROVE-1")

	resp := do(t, srv, "POST", "/api/tasks/"+itoa(tk.ID)+"/approve", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("确认应返回 200，得到 %d：%v", resp.StatusCode, decode(t, resp))
	}

	after, err := m.Get(ctx, tk.ID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if after.State != task.StateQueued {
		t.Fatalf("确认后应转回 queued 等 worker 领，得到 %s", after.State)
	}

	// payload 里必须有 mode=approved：这是 worker 决定走 EntryPush 的唯一依据
	events, err := m.Events(ctx, tk.ID)
	if err != nil {
		t.Fatalf("读事件失败: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Payload == nil {
			continue
		}
		if v, ok := e.Payload["mode"]; ok && v == string(runner.RetryApproved) {
			found = true
		}
	}
	if !found {
		t.Error("转 queued 那次转移的 payload 里应写下 mode=approved")
	}

	// 必须重派原任务行（不新建 —— 新建会撞同一 issue 的活任务唯一索引）
	if len(q.requeued) != 1 || q.requeued[0] != tk.ID {
		t.Errorf("应重派原任务行 %d，实际 requeued=%v", tk.ID, q.requeued)
	}
	if len(q.issues) != 0 {
		t.Errorf("确认不该新建任务，却调了 Enqueue：%v", q.issues)
	}
}

// 只有停在 awaiting_approval 的任务能被确认。其余状态一律拒绝 ——
// 否则「确认」就变成了一个能把任意任务推去开 PR 的后门。
func TestAPIApproveRejectsWrongState(t *testing.T) {
	api, _, m, repoID := apiFixture(t)
	api.Queue = &fakeEnqueuer{}
	srv := apiServer(t, api)
	ctx := context.Background()

	tk, err := m.Create(ctx, task.CreateParams{
		UserID: mustUserOfRepo(t, api, repoID), RepoID: repoID,
		LinearIssueKey: "CR-APPROVE-2",
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}

	resp := do(t, srv, "POST", "/api/tasks/"+itoa(tk.ID)+"/approve", "", true)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("queued 状态的任务不该可确认，期望 409，得到 %d：%v", resp.StatusCode, decode(t, resp))
	}
	after, _ := m.Get(ctx, tk.ID)
	if after.State != task.StateQueued {
		t.Errorf("被拒绝的确认不该改动状态，得到 %s", after.State)
	}
}

// 跨用户隔离：别人的任务 = 不存在（与 taskDetail 同一原则）。
func TestAPIApproveIsolatedByOwner(t *testing.T) {
	api, st, m, repoID := apiFixture(t)
	api.Queue = &fakeEnqueuer{}

	tk := awaitingTask(t, api, m, repoID, "CR-APPROVE-3")

	// 换一个登录用户
	otherID := mustUser(t, st, "approve-other-"+t.Name()+"@example.com")
	api.Auth = authAs(otherID, "approve-other@example.com")
	srv2 := apiServer(t, api)

	resp := do(t, srv2, "POST", "/api/tasks/"+itoa(tk.ID)+"/approve", "", true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("别人的任务应返回 404，得到 %d", resp.StatusCode)
	}
	after, _ := m.Get(context.Background(), tk.ID)
	if after.State != task.StateAwaitingApproval {
		t.Errorf("跨用户确认不该改动状态，得到 %s", after.State)
	}
}

func mustUserOfRepo(t *testing.T, api *API, repoID int64) int64 {
	t.Helper()
	var userID int64
	if err := api.Store.Pool().QueryRow(context.Background(),
		`SELECT user_id FROM repos WHERE id=$1`, repoID).Scan(&userID); err != nil {
		t.Fatalf("查 repo 属主失败: %v", err)
	}
	return userID
}
