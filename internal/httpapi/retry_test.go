package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/Clouditera/lathe/internal/runner"
	"github.com/Clouditera/lathe/internal/task"
)

// fakeScenes 注入预设的现场体检结果。
type fakeScenes struct{ state *runner.WorktreeState }

func (f fakeScenes) Inspect(ctx context.Context, providerRepo, path, branch, base string) *runner.WorktreeState {
	return f.state
}

// 智能重试的 API 行为：mode 校验、resume 预检拒绝、retry-plan 预览。
func TestAPIRetryModesAndPlan(t *testing.T) {
	api, _, m, repoID := apiFixture(t)
	q := &fakeEnqueuer{}
	api.Queue = q
	// 现场可用：目录/注册/分支/提交俱在
	api.Scenes = fakeScenes{state: &runner.WorktreeState{
		Exists: true, Registered: true, BranchExists: true, HasCommits: true, Commits: 3,
	}}
	srv := apiServer(t, api)
	ctx := context.Background()

	var userID int64
	_ = api.Store.Pool().QueryRow(ctx, `SELECT user_id FROM repos WHERE id=$1`, repoID).Scan(&userID)

	seq := 0
	newFailedTask := func(stage string) *task.Task {
		seq++
		tk, err := m.Create(ctx, task.CreateParams{
			UserID: userID, RepoID: repoID,
			LinearIssueKey: "CR-R" + itoa(int64(seq)),
		})
		if err != nil {
			t.Fatal(err)
		}
		wtPath, branch, sess := "/tmp/scene", "fix/cr-r-x", "sess-1"
		if _, err := m.Transition(ctx, tk.ID, task.StateTriaging, "test", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := m.Transition(ctx, tk.ID, task.StateImplementing, "test", &task.TransitionOpts{
			WorktreePath: &wtPath, BranchName: &branch, AgentSessionID: &sess,
		}); err != nil {
			t.Fatal(err)
		}
		opts := &task.TransitionOpts{FailureReason: &[]string{"构造失败"}[0]}
		if stage != "" {
			opts.FailureStage = &stage
		}
		if _, err := m.Transition(ctx, tk.ID, task.StateFailed, "test", opts); err != nil {
			t.Fatal(err)
		}
		return tk
	}

	// 1) 非法 mode → 400
	tk := newFailedTask("push")
	resp := do(t, srv, "POST", "/api/tasks/"+itoa(tk.ID)+"/retry", `{"mode":"随便"}`, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("非法 mode 应 400，得到 %d", resp.StatusCode)
	}

	// 2) mode=fresh → 200，任务回 queued
	resp = do(t, srv, "POST", "/api/tasks/"+itoa(tk.ID)+"/retry", `{"mode":"fresh"}`, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fresh 重试应成功，得到 %d", resp.StatusCode)
	}
	if len(q.requeued) != 1 {
		t.Fatalf("应重派原任务行，requeued=%v", q.requeued)
	}

	// 3) mode=resume + 现场可用（push 阶段失败）→ 200
	tk2 := newFailedTask("push")
	resp = do(t, srv, "POST", "/api/tasks/"+itoa(tk2.ID)+"/retry", `{"mode":"resume"}`, true)
	if resp.StatusCode != http.StatusOK {
		body := decode(t, resp)
		t.Fatalf("现场可用时 resume 应成功，得到 %d: %v", resp.StatusCode, body)
	}

	// 4) mode=resume + 现场不可用 → 409 且说明理由
	api.Scenes = fakeScenes{state: &runner.WorktreeState{}}
	tk3 := newFailedTask("push")
	resp = do(t, srv, "POST", "/api/tasks/"+itoa(tk3.ID)+"/retry", `{"mode":"resume"}`, true)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("现场不可用时 resume 应 409，得到 %d", resp.StatusCode)
	}
	body := decode(t, resp)
	if body["error"] == nil {
		t.Error("409 响应应说明拒绝原因")
	}
	// 被拒绝的任务状态应保持 failed（没被骗进 queued）
	after, _ := m.Get(ctx, tk3.ID)
	if after.State != task.StateFailed {
		t.Errorf("resume 被拒后状态应保持 failed，得到 %s", after.State)
	}

	// 5) retry-plan 预览：push 失败 + 现场可用 → 从推送续跑
	api.Scenes = fakeScenes{state: &runner.WorktreeState{
		Exists: true, Registered: true, BranchExists: true, HasCommits: true, Commits: 3,
	}}
	tk4 := newFailedTask("push")
	resp = do(t, srv, "GET", "/api/tasks/"+itoa(tk4.ID)+"/retry-plan", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("预览应成功，得到 %d", resp.StatusCode)
	}
	plan := decode(t, resp)
	if plan["fresh"] != false || plan["entry"] != "push" {
		t.Errorf("push 失败+现场可用应预览为 push 续跑: %v", plan)
	}
	if plan["entryLabel"] == nil || plan["entryLabel"] == "" {
		t.Error("预览应带人读入口名")
	}
	wt, _ := plan["worktree"].(map[string]any)
	if wt == nil || wt["commits"] != float64(3) {
		t.Errorf("预览应带现场体检结果: %v", plan["worktree"])
	}
	reasons, _ := plan["reasons"].([]any)
	if len(reasons) == 0 {
		t.Error("预览应带决策理由")
	}

	// 6) retry-plan 对他人任务隐藏存在
	var otherID int64
	_ = api.Store.Pool().QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) ON CONFLICT (email) DO UPDATE SET updated_at=now() RETURNING id`,
		"api-retry-other@example.com").Scan(&otherID)
	defer func() {
		_, _ = api.Store.Pool().Exec(context.Background(), `DELETE FROM users WHERE id=$1`, otherID)
	}()
	var otherRepo int64
	_ = api.Store.Pool().QueryRow(ctx, `INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		otherID, "acme/other").Scan(&otherRepo)
	otherTk, _ := m.Create(ctx, task.CreateParams{UserID: otherID, RepoID: otherRepo, LinearIssueKey: "CR-X"})
	resp = do(t, srv, "GET", "/api/tasks/"+itoa(otherTk.ID)+"/retry-plan", "", true)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("他人任务的预览应 404，得到 %d", resp.StatusCode)
	}
}
