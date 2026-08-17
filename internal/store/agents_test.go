package store

import (
	"context"
	"testing"

	"github.com/Clouditera/lathe/internal/integration/agent"
)

// agentEventFixture 建「用户 + 仓库 + 任务」的最小夹具，返回任务 ID。
func agentEventFixture(t *testing.T, st *Store, email string) (userID, taskID int64) {
	t.Helper()
	ctx := context.Background()

	if err := st.pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) ON CONFLICT (email) DO UPDATE SET updated_at=now() RETURNING id`,
		email).Scan(&userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})

	var repoID int64
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		userID, "acme/agent-events-"+email).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO tasks (user_id, repo_id, linear_issue_key) VALUES ($1,$2,$3) RETURNING id`,
		userID, repoID, "CR-AGENT").Scan(&taskID); err != nil {
		t.Fatalf("建 task 失败: %v", err)
	}
	return userID, taskID
}

func TestAgentEventsRoundTrip(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, taskID := agentEventFixture(t, st, "ae-round@example.com")

	// 空批不碰库，也不该报错
	if err := st.InsertAgentEvents(ctx, taskID, "triage", nil); err != nil {
		t.Fatalf("空批应直接返回: %v", err)
	}

	batch := []agent.Entry{
		{Kind: agent.KindInit, Body: "会话就绪 · 模型 claude", Payload: map[string]any{"model": "claude"}},
		{Kind: agent.KindText, Body: "先读一下代码"},
		{Kind: agent.KindToolUse, Tool: "Bash", Body: "Bash go build ./..."},
		{Kind: agent.KindToolResult, Body: "ok", Payload: map[string]any{"isError": false}},
	}
	if err := st.InsertAgentEvents(ctx, taskID, "implement", batch); err != nil {
		t.Fatalf("批量落库失败: %v", err)
	}

	events, lastID, err := st.AgentEventsAfter(ctx, taskID, userID, 0, 100)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(events) != len(batch) {
		t.Fatalf("应读回 %d 条，得到 %d", len(batch), len(events))
	}

	// 顺序与游标：id 严格递增，last_id 是最后一条的 id
	for i, e := range events {
		if e.Phase != "implement" || e.Kind != batch[i].Kind || e.Body != batch[i].Body {
			t.Errorf("第 %d 条不符: %+v", i, e)
		}
		if i > 0 && e.ID <= events[i-1].ID {
			t.Errorf("游标应严格递增: %d 后接 %d", events[i-1].ID, e.ID)
		}
	}
	if events[2].Tool == nil || *events[2].Tool != "Bash" {
		t.Errorf("tool_use 应带工具名: %+v", events[2])
	}
	if events[0].Tool != nil {
		t.Errorf("非工具事件不应有 tool: %+v", events[0].Tool)
	}
	if lastID != events[len(events)-1].ID {
		t.Errorf("last_id = %d，应为 %d", lastID, events[len(events)-1].ID)
	}

	// 增量：after 之后只返回新事件；没有新事件时 last_id 原样返回
	again, last2, err := st.AgentEventsAfter(ctx, taskID, userID, lastID, 100)
	if err != nil {
		t.Fatalf("增量查询失败: %v", err)
	}
	if len(again) != 0 || last2 != lastID {
		t.Errorf("无新事件时应为空且游标不动: len=%d last=%d→%d", len(again), lastID, last2)
	}

	if err := st.InsertAgentEvents(ctx, taskID, "verify", []agent.Entry{
		{Kind: "verify_step", Body: "✓ build (.) · 2s", Payload: map[string]any{"status": "passed"}},
	}); err != nil {
		t.Fatalf("追加失败: %v", err)
	}
	more, _, err := st.AgentEventsAfter(ctx, taskID, userID, lastID, 100)
	if err != nil {
		t.Fatalf("再查询失败: %v", err)
	}
	if len(more) != 1 || more[0].Kind != "verify_step" {
		t.Errorf("增量应只拿到新的一条: %+v", more)
	}
}

// 事件查询走 JOIN tasks 限定属主：别人的任务与「没有事件」不可区分。
func TestAgentEventsUserIsolation(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	_, taskID := agentEventFixture(t, st, "ae-a@example.com")

	if err := st.InsertAgentEvents(ctx, taskID, "triage", []agent.Entry{{Kind: agent.KindText, Body: "A 的现场"}}); err != nil {
		t.Fatalf("落库失败: %v", err)
	}

	// 非属主的视角查这个任务：空，且不报错（404 决策在 API 层）
	events, _, err := st.AgentEventsAfter(ctx, taskID, 999999, 0, 100)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("非属主应读不到事件: %+v", events)
	}
}

func TestSetAgentSummary(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, taskID := agentEventFixture(t, st, "ae-summary@example.com")

	if err := st.SetAgentSummary(ctx, taskID, "## 改了什么\n补了测试", 0.42, 61000, 12); err != nil {
		t.Fatalf("落摘要失败: %v", err)
	}

	detail, err := st.TaskDetail(ctx, taskID, userID)
	if err != nil {
		t.Fatalf("读详情失败: %v", err)
	}
	got := detail.Task
	if got.AgentSummary == nil || *got.AgentSummary != "## 改了什么\n补了测试" {
		t.Errorf("摘要未读回: %v", got.AgentSummary)
	}
	if got.AgentCostUSD == nil || *got.AgentCostUSD != 0.42 {
		t.Errorf("成本未读回: %v", got.AgentCostUSD)
	}
	if got.AgentDurationMS == nil || *got.AgentDurationMS != 61000 {
		t.Errorf("耗时未读回: %v", got.AgentDurationMS)
	}
	if got.AgentNumTurns == nil || *got.AgentNumTurns != 12 {
		t.Errorf("轮数未读回: %v", got.AgentNumTurns)
	}
}
