package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
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
		`INSERT INTO tasks (user_id, repo_id, external_key) VALUES ($1,$2,$3) RETURNING id`,
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

// subagent 的归属（0014）必须原样往返：写 AgentID、读回 agentId。
// 主 agent 的事件要落成 NULL 而不是空串 —— 前端按「有无 agentId」分组。
func TestAgentEventsSubagentAttribution(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, taskID := agentEventFixture(t, st, "ae-sub@example.com")

	batch := []agent.Entry{
		{Kind: agent.KindToolUse, Tool: "Agent", Body: "Agent 找 group 模型的测试",
			Payload: map[string]any{"toolUseId": "toolu_P1"}},
		{Kind: agent.KindAgentStart, AgentID: "a89b3f", Body: "去找 group 模型的测试",
			Payload: map[string]any{"agentId": "a89b3f"}},
		{Kind: agent.KindToolUse, AgentID: "a89b3f", Tool: "Grep", Body: "Grep func TestGroup",
			Payload: map[string]any{"toolUseId": "toolu_S1"}},
		{Kind: agent.KindText, AgentID: "a89b3f", Body: "找到了 model_test.go"},
	}
	if err := st.InsertAgentEvents(ctx, taskID, "implement", batch); err != nil {
		t.Fatalf("落库失败: %v", err)
	}

	events, _, err := st.AgentEventsAfter(ctx, taskID, userID, 0, 100)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(events) != len(batch) {
		t.Fatalf("应读回 %d 条，得到 %d", len(batch), len(events))
	}

	// 主 agent 那条：agent_id 必须是 NULL
	if events[0].AgentID != nil {
		t.Errorf("主 agent 的事件应落成 NULL，得到 %q", *events[0].AgentID)
	}
	// subagent 的三条：归属原样带回
	for i := 1; i < 4; i++ {
		if events[i].AgentID == nil || *events[i].AgentID != "a89b3f" {
			t.Errorf("第 %d 条应属于 subagent a89b3f，得到 %v", i, events[i].AgentID)
		}
	}
	if events[1].Kind != agent.KindAgentStart {
		t.Errorf("分组头的 kind = %q，期望 %q（0014 扩了 CHECK）", events[1].Kind, agent.KindAgentStart)
	}
	// 配对 key 在 subagent 内部同样要保住
	if events[2].Payload["toolUseId"] != "toolu_S1" {
		t.Errorf("subagent 工具调用的 toolUseId = %v，期望 toolu_S1", events[2].Payload["toolUseId"])
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

// AC-1（f04）：流水线会写的每一个 phase 都必须能落库。
//
// implement-reentry 来自 runner.PhaseImplementReentry（internal/runner/eventsink.go）；
// 这里只能写字面量 —— runner 的非测试代码 import 了 store，store 测试再 import
// runner 会成环。字面量与常量的绑定由 httpapi 的
// TestEventsPhaseFilterAcceptsRunnerReentryPhase 负责。
func TestInsertAgentEventsAcceptsEveryPipelinePhase(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	tag := "f04-ac1-" + randHex(t)
	userID, taskID := agentEventFixture(t, st, tag+"@example.com")
	// external_key 带随机量，清理按自己建的 task_id 删（agent_events 级联）
	if _, err := st.pool.Exec(ctx, `UPDATE tasks SET external_key=$2 WHERE id=$1`, taskID, tag); err != nil {
		t.Fatalf("改 external_key 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `DELETE FROM tasks WHERE id=$1`, taskID)
	})

	for _, phase := range []string{
		"triage", "implement",
		"implement-reentry", // = runner.PhaseImplementReentry
		"verify", "push", "fix-1", "fix-12", "review",
	} {
		t.Run("给定 phase="+phase+" 断言可落库并读回", func(t *testing.T) {
			_, last, err := st.AgentEventsAfter(ctx, taskID, userID, 0, 500)
			if err != nil {
				t.Fatalf("查询失败: %v", err)
			}
			body := "probe-" + phase
			if err := st.InsertAgentEvents(ctx, taskID, phase, []agent.Entry{{Kind: agent.KindText, Body: body}}); err != nil {
				t.Fatalf("落库失败: got err=%v, want nil", err)
			}
			got, _, err := st.AgentEventsAfter(ctx, taskID, userID, last, 500)
			if err != nil {
				t.Fatalf("读回失败: %v", err)
			}
			if len(got) != 1 || got[0].Phase != phase || got[0].Body != body {
				t.Errorf("读回不符: got %+v, want 1 条 phase=%s body=%s", got, phase, body)
			}
		})
	}
}

// randHex 给 fixture 标识加随机量（AGENTS.md §5.5）。
func randHex(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("取随机数失败: %v", err)
	}
	return hex.EncodeToString(b[:])
}
