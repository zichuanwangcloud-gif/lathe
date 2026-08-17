package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/Clouditera/lathe/internal/integration/agent"
	"github.com/Clouditera/lathe/internal/store"
	"github.com/Clouditera/lathe/internal/task"
)

// 执行日志端点：增量拉取 + 游标语义（docs/04 §3.3）。
func TestTaskEventsEndpoint(t *testing.T) {
	api, st, m, repoID := apiFixture(t)
	srv := apiServer(t, api)
	ctx := context.Background()

	var userID int64
	if err := st.Pool().QueryRow(ctx, `SELECT user_id FROM repos WHERE id=$1`, repoID).Scan(&userID); err != nil {
		t.Fatalf("取 user 失败: %v", err)
	}
	tk, err := m.Create(ctx, task.CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-EV"})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}

	if err := st.InsertAgentEvents(ctx, tk.ID, "implement", []agent.Entry{
		{Kind: agent.KindText, Body: "第一条"},
		{Kind: agent.KindText, Body: "第二条"},
	}); err != nil {
		t.Fatalf("落事件失败: %v", err)
	}

	// 首轮：after=0 拿到全部，last_id 指向最后一条
	resp := do(t, srv, "GET", "/api/tasks/"+itoa(tk.ID)+"/events?after=0", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("查询事件应 200，得到 %d", resp.StatusCode)
	}
	body := decode(t, resp)
	events, _ := body["events"].([]any)
	if len(events) != 2 {
		t.Fatalf("应返回 2 条事件，得到 %d: %v", len(events), body)
	}
	first, _ := events[0].(map[string]any)
	if first["kind"] != "text" || first["phase"] != "implement" || first["body"] != "第一条" {
		t.Errorf("首条事件不符: %v", first)
	}
	lastID := int64(body["last_id"].(float64))
	if lastID <= 0 {
		t.Fatalf("last_id 应为正数游标，得到 %d", lastID)
	}

	// 增量：after=last_id 应为空，且游标不动
	resp = do(t, srv, "GET", "/api/tasks/"+itoa(tk.ID)+"/events?after="+itoa(lastID), "", true)
	body = decode(t, resp)
	if events, _ := body["events"].([]any); len(events) != 0 {
		t.Errorf("无新事件时应为空: %v", events)
	}
	if got := int64(body["last_id"].(float64)); got != lastID {
		t.Errorf("游标不应动: %d → %d", lastID, got)
	}

	// 未认证 → 401（内部信息不匿名可读）
	resp = do(t, srv, "GET", "/api/tasks/"+itoa(tk.ID)+"/events", "", false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("未认证应 401，得到 %d", resp.StatusCode)
	}
}

// 权限与 taskDetail 同一原则：别人的任务按 404 处理，不暴露存在。
func TestTaskEventsIsolation(t *testing.T) {
	_, st, m, repoID := apiFixture(t)
	ctx := context.Background()

	var userID int64
	if err := st.Pool().QueryRow(ctx, `SELECT user_id FROM repos WHERE id=$1`, repoID).Scan(&userID); err != nil {
		t.Fatalf("取 user 失败: %v", err)
	}
	tk, err := m.Create(ctx, task.CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-EV2"})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	if err := st.InsertAgentEvents(ctx, tk.ID, "triage", []agent.Entry{{Kind: agent.KindText, Body: "别人的现场"}}); err != nil {
		t.Fatalf("落事件失败: %v", err)
	}

	// 同一台服务换成 B 的视角
	userB := mustUser(t, st, "events-b@example.com")
	apiB := &API{Store: st, Tasks: m, Queue: &fakeEnqueuer{}, Auth: authAs(userB, "b@example.com")}
	srvB := apiServer(t, apiB)

	resp := do(t, srvB, "GET", "/api/tasks/"+itoa(tk.ID)+"/events", "", true)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("B 读 A 的任务事件应 404，得到 %d", resp.StatusCode)
	}
}

var _ = store.ErrRepoNotFound // 保持 store 引用（本文件以 API 行为为主）
