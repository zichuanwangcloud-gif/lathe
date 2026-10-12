package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
)

// 规划管线的 agent 事件（docs/10 阶段 B/E）。
//
// 为什么不复用 agent_events：那张表的 task_id 是 NOT NULL REFERENCES
// tasks(id)，而模糊任务不在 tasks 里（见 migration 0023 的说明）。要复用
// 就得把 task_id 放开为可空 —— 但「每条 agent 事件都属于某个可执行任务」
// 是执行管线的强约束，不该为规划管线破例。代价是两套读写代码，换来的是
// 两条管线的约束互不污染；轮询协议（?after=last_id）与前端组件完全共用。

// PRDEvent 是 prd_events 表的一行。
//
// 比 AgentEvent 多一个 Round、少一个 AgentID：
//   - Round 让前端把事件按对话轮次分组（同一份 PRD 跑了五轮，事件流不分
//     轮次就是一锅粥）。
//   - 规划与复核都不起 subagent（只读探索用不上），所以没有 AgentID。
type PRDEvent struct {
	ID int64 `json:"id"`
	// Round 为 nil 表示不属于某一轮（当前只有对抗复核会这样 —— 它是
	// 定稿前的一次性动作，不在轮次序列里）。
	Round   *int           `json:"round,omitempty"`
	Phase   string         `json:"phase"`
	Kind    string         `json:"kind"`
	Tool    *string        `json:"tool,omitempty"`
	Body    string         `json:"body"`
	Payload map[string]any `json:"payload"`
	At      time.Time      `json:"at"`
}

// 规划管线的两个 phase 取值（与 migration 0025 的 CHECK 一致）。
//
// 刻意把对抗复核分成独立 phase 而不是复用 plan：人在事件流里要一眼
// 分清「作者说的」与「攻击者说的」。混在一起的话，复核那句「我构造出了
// 一个能过全部 AC 的恶意实现」会被当成规划智能体的自述。
const (
	PRDPhasePlan       = "plan"
	PRDPhasePlanReview = "plan-review"
)

// InsertPRDEvents 批量落一轮规划对话提炼后的 agent 事件。
//
// round 为 nil 时写 NULL（对抗复核路径）。entries 为空直接返回。
func (s *Store) InsertPRDEvents(ctx context.Context, prdID int64, round *int, phase string, entries []agent.Entry) error {
	if len(entries) == 0 {
		return nil
	}

	_, err := s.pool.CopyFrom(ctx,
		pgx.Identifier{"prd_events"},
		[]string{"prd_id", "round", "phase", "kind", "tool", "body", "payload"},
		pgx.CopyFromSlice(len(entries), func(i int) ([]any, error) {
			e := entries[i]
			var tool any
			if e.Tool != "" {
				tool = e.Tool
			}
			payload := e.Payload
			if payload == nil {
				payload = map[string]any{} // 列是 NOT NULL DEFAULT '{}'
			}
			var r any
			if round != nil {
				r = *round
			}
			return []any{prdID, r, phase, e.Kind, tool, e.Body, payload}, nil
		}))
	if err != nil {
		return fmt.Errorf("store: 批量落 PRD 事件失败（prd %d, %d 条）: %w", prdID, len(entries), err)
	}
	return nil
}

// PRDEventsAfter 增量拉取一份 PRD 的规划事件（游标 id 严格单调）。
//
// userID 是隔离边界：经 JOIN prds 限定属主，别人的 PRD 在这里与「没有
// 事件」不可区分 —— 是否 404 由 API 层的归属判定决定（同 AgentEventsAfter）。
//
// 返回的 lastID 是下一轮的 after 游标：本批为空时原样返回 after，
// 前端无需特判。
func (s *Store) PRDEventsAfter(ctx context.Context, prdID, userID, after int64, limit int) ([]PRDEvent, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}

	rows, err := s.pool.Query(ctx, `
		SELECT e.id, e.round, e.phase, e.kind, e.tool, e.body, e.payload, e.at
		FROM prd_events e
		JOIN prds p ON p.id = e.prd_id
		WHERE e.prd_id = $1 AND p.user_id = $2 AND e.id > $3
		ORDER BY e.id
		LIMIT $4`, prdID, userID, after, limit)
	if err != nil {
		return nil, after, fmt.Errorf("store: 查询 PRD 事件失败: %w", err)
	}
	defer rows.Close()

	out := []PRDEvent{}
	lastID := after
	for rows.Next() {
		var e PRDEvent
		if err := rows.Scan(&e.ID, &e.Round, &e.Phase, &e.Kind, &e.Tool,
			&e.Body, &e.Payload, &e.At); err != nil {
			return nil, after, fmt.Errorf("store: 读取 PRD 事件行失败: %w", err)
		}
		out = append(out, e)
		lastID = e.ID
	}
	return out, lastID, rows.Err()
}
