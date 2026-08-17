package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Clouditera/lathe/internal/integration/agent"
)

// AgentEvent 是 agent_events 表的一行（docs/04-agent-visibility.md §3）。
//
// 写入侧直接消费 agent.Digest 提炼出的 Entry；读取侧多了游标 id 与
// 时间戳 at，供详情页日志面板增量轮询。
type AgentEvent struct {
	ID      int64          `json:"id"`
	Phase   string         `json:"phase"`
	Kind    string         `json:"kind"`
	Tool    *string        `json:"tool,omitempty"`
	Body    string         `json:"body"`
	Payload map[string]any `json:"payload"`
	At      time.Time      `json:"at"`
}

// InsertAgentEvents 批量落一阶段提炼后的 agent 事件。
//
// 用 COPY 而非逐行 INSERT：事件量每任务成百上千行，而调用方（EventSink）
// 已经把事件攒成了批。entries 为空时直接返回，不碰数据库。
func (s *Store) InsertAgentEvents(ctx context.Context, taskID int64, phase string, entries []agent.Entry) error {
	if len(entries) == 0 {
		return nil
	}

	_, err := s.pool.CopyFrom(ctx,
		pgx.Identifier{"agent_events"},
		[]string{"task_id", "phase", "kind", "tool", "body", "payload"},
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
			return []any{taskID, phase, e.Kind, tool, e.Body, payload}, nil
		}))
	if err != nil {
		return fmt.Errorf("store: 批量落 agent 事件失败（task %d, %d 条）: %w", taskID, len(entries), err)
	}
	return nil
}

// AgentEventsAfter 增量拉取一个任务的 agent 事件（游标 id 严格单调）。
//
// userID 是隔离边界，与 TaskDetail 同一原则：事件经由 JOIN tasks 限定
// 属主，别人的任务在这里与"没有事件"不可区分 —— 是否 404 由调用方
// （API 层先做归属判定）决定。
//
// 返回的 lastID 是下一轮的 after 游标：本批为空时原样返回 after，
// 前端无需特判。
func (s *Store) AgentEventsAfter(ctx context.Context, taskID, userID, after int64, limit int) ([]AgentEvent, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}

	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.phase, a.kind, a.tool, a.body, a.payload, a.at
		FROM agent_events a
		JOIN tasks t ON t.id = a.task_id
		WHERE a.task_id = $1 AND t.user_id = $2 AND a.id > $3
		ORDER BY a.id
		LIMIT $4`, taskID, userID, after, limit)
	if err != nil {
		return nil, after, fmt.Errorf("store: 查询 agent 事件失败: %w", err)
	}
	defer rows.Close()

	out := []AgentEvent{}
	lastID := after
	for rows.Next() {
		var e AgentEvent
		if err := rows.Scan(&e.ID, &e.Phase, &e.Kind, &e.Tool, &e.Body, &e.Payload, &e.At); err != nil {
			return nil, after, fmt.Errorf("store: 读取 agent 事件行失败: %w", err)
		}
		out = append(out, e)
		lastID = e.ID
	}
	return out, lastID, rows.Err()
}

// SetAgentSummary 落实现阶段的终局摘要（docs/04 §3.5）。
//
// 含 fail 路径：只要 agent 给出了 result 事件就存 —— 失败现场的自述
// （它认为自己卡在哪）是排障信息，不该只在成功时才留。
func (s *Store) SetAgentSummary(ctx context.Context, taskID int64, summary string, costUSD float64, durationMS int64, numTurns int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE tasks SET
			agent_summary     = $2,
			agent_cost_usd    = $3,
			agent_duration_ms = $4,
			agent_num_turns   = $5
		WHERE id = $1`, taskID, summary, costUSD, durationMS, numTurns)
	if err != nil {
		return fmt.Errorf("store: 落 agent 摘要失败（task %d）: %w", taskID, err)
	}
	return nil
}
