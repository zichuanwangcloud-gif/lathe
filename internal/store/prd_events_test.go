package store

import (
	"context"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
)

// 规划管线的事件流沿用 agent_events 的 ?after=last_id 轮询协议，
// 但落在独立的 prd_events 表（见 prd_events.go 顶部的理由）。

func TestPRDEventsIncrementalPolling(t *testing.T) {
	st := testStore(t)
	userID, repoID := prdFixture(t, st)
	ctx := context.Background()
	p := newPRD(t, st, userID, repoID)

	round := 1
	if err := st.InsertPRDEvents(ctx, p.ID, &round, PRDPhasePlan, []agent.Entry{
		{Kind: "init", Body: "只读进仓库"},
		{Kind: "tool_use", Tool: "Read", Body: "internal/preview/gate.go"},
	}); err != nil {
		t.Fatalf("落规划事件失败: %v", err)
	}

	first, cursor, err := st.PRDEventsAfter(ctx, p.ID, userID, 0, 0)
	if err != nil {
		t.Fatalf("拉取事件失败: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("应拉到 2 条，got=%d", len(first))
	}
	if first[0].Round == nil || *first[0].Round != 1 {
		t.Error("轮次必须落库：不分轮次的事件流是一锅粥")
	}
	if first[1].Tool == nil || *first[1].Tool != "Read" {
		t.Errorf("工具名应留痕，got=%v", first[1].Tool)
	}
	if cursor != first[1].ID {
		t.Errorf("游标应是本批末条 id，got=%d want=%d", cursor, first[1].ID)
	}

	// 对抗复核不属于任何一轮：round 写 NULL。
	if err := st.InsertPRDEvents(ctx, p.ID, nil, PRDPhasePlanReview, []agent.Entry{
		{Kind: "text", Body: "尝试构造恶意合规实现"},
	}); err != nil {
		t.Fatalf("落复核事件失败: %v", err)
	}

	next, cursor2, err := st.PRDEventsAfter(ctx, p.ID, userID, cursor, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 {
		t.Fatalf("增量拉取只该返回新增的 1 条，got=%d", len(next))
	}
	if next[0].Phase != PRDPhasePlanReview {
		t.Errorf("复核事件的 phase 应可区分，got=%q", next[0].Phase)
	}
	if next[0].Round != nil {
		t.Error("对抗复核不在轮次序列里，round 应为 NULL")
	}

	// 游标追平后再拉应为空，且原样返回 after —— 前端无需特判。
	empty, cursor3, err := st.PRDEventsAfter(ctx, p.ID, userID, cursor2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Errorf("无新事件时应返回空批，got=%d 条", len(empty))
	}
	if cursor3 != cursor2 {
		t.Errorf("空批应原样返回 after，got=%d want=%d", cursor3, cursor2)
	}
}

// 事件流也是隔离边界：别人的 PRD 在这里与「没有事件」不可区分。
func TestPRDEventsHiddenFromNonOwner(t *testing.T) {
	st := testStore(t)
	userID, repoID := prdFixture(t, st)
	otherID, _ := prdFixture(t, st)
	ctx := context.Background()
	p := newPRD(t, st, userID, repoID)

	round := 1
	if err := st.InsertPRDEvents(ctx, p.ID, &round, PRDPhasePlan,
		[]agent.Entry{{Kind: "text", Body: "机密推理"}}); err != nil {
		t.Fatal(err)
	}

	got, _, err := st.PRDEventsAfter(ctx, p.ID, otherID, 0, 0)
	if err != nil {
		t.Fatalf("非属主拉取不该报错（应只是空）: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("非属主不该看到别人 PRD 的事件，got=%d 条", len(got))
	}
}

func TestInsertPRDEventsEmptyIsNoop(t *testing.T) {
	st := testStore(t)
	userID, repoID := prdFixture(t, st)
	ctx := context.Background()
	p := newPRD(t, st, userID, repoID)

	if err := st.InsertPRDEvents(ctx, p.ID, nil, PRDPhasePlan, nil); err != nil {
		t.Errorf("空批不该碰数据库也不该报错: %v", err)
	}
}
