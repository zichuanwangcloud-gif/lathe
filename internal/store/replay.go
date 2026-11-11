package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// replay.go —— 任务详情页「运行回放」的只读查询（docs/features/run-replay.md）。
//
// 全部是 WHERE task_id = $1 ... ORDER BY id 的形状，走 0005 的 (task_id, id)
// 索引 agent_events_task_id 后在单任务范围内过滤（单任务事件量百到千级）；
// TestEventsQueriesUseTaskIndex 用 EXPLAIN 钉住。不加索引、不写回。

// FailurePredicateSQL 是「失败事件」的唯一判定（error 与 overflow 警告）。
//
// summary 的 failures 列表与 /events?failures=1 共用这一段，前端 replay.js 的
// isFailure/severityOf 与它由同一张表（web/src/testdata/failure-cases.json）
// 驱动测试 —— 失败判定只有一份真相。只认现有四种 payload 形状，未知形状
// 不判失败（宁可漏判，不误报；兼容 f06 的新 CLI 事件）。
const FailurePredicateSQL = `(
	   (a.kind = 'tool_result' AND a.payload->>'isError' = 'true')
	OR (a.kind = 'verify_step' AND a.payload->>'status' IN ('failed', 'error'))
	OR (a.kind = 'result' AND (a.payload->>'isError' = 'true' OR a.payload->>'subtype' <> 'success'))
	OR (a.kind = 'raw' AND a.payload->>'type' = 'overflow'))`

// failureErrorSQL 只算 error：overflow 警告不会导致任务失败，不计入失败数、
// 也不当 firstFailure/lastFailure。
const failureErrorSQL = `(` + FailurePredicateSQL + ` AND NOT (a.kind = 'raw'))`

// summaryFailuresMax 是 summary 返回的失败列表上限；超出时如实标 truncated，
// 导航本身不靠这个数组走到底（/events?failures=1 按当前 id 取下一个）。
const summaryFailuresMax = 200

// eventsPageMax 是单页事件上限（沿用 AgentEventsAfter 的 500）。
const eventsPageMax = 500

// EventFilter 是 /events 的可选过滤。零值 = 不过滤。
//
// Phases/Kinds 由调用方（httpapi）先按白名单校验，这里只做参数化绑定，
// 不把任何字符串拼进 SQL。
type EventFilter struct {
	Phases       []string
	Kinds        []string
	FailuresOnly bool
}

func (f EventFilter) where(args []any) (string, []any) {
	var b strings.Builder
	if len(f.Phases) > 0 {
		args = append(args, f.Phases)
		b.WriteString(" AND a.phase = ANY($" + strconv.Itoa(len(args)) + ")")
	}
	if len(f.Kinds) > 0 {
		args = append(args, f.Kinds)
		b.WriteString(" AND a.kind = ANY($" + strconv.Itoa(len(args)) + ")")
	}
	if f.FailuresOnly {
		b.WriteString(" AND " + FailurePredicateSQL)
	}
	return b.String(), args
}

func clampLimit(limit int) int {
	if limit <= 0 || limit > eventsPageMax {
		return 200
	}
	return limit
}

const eventColumns = `a.id, a.phase, a.kind, a.tool, a.body, a.payload, a.agent_id, a.at`

// agentEventsAfterQuery 构造 after 方向的查询（多取一条判 has_more）。
func agentEventsAfterQuery(taskID, userID, after int64, f EventFilter, limit int) (string, []any) {
	args := []any{taskID, userID, after}
	extra, args := f.where(args)
	args = append(args, limit+1)
	return `SELECT ` + eventColumns + `
		FROM agent_events a
		JOIN tasks t ON t.id = a.task_id
		WHERE a.task_id = $1 AND t.user_id = $2 AND a.id > $3` + extra + `
		ORDER BY a.id
		LIMIT $` + strconv.Itoa(len(args)), args
}

// agentEventsBeforeQuery 构造 before 方向的查询：取 id < before 的最近 limit+1 条（降序）。
func agentEventsBeforeQuery(taskID, userID, before int64, f EventFilter, limit int) (string, []any) {
	args := []any{taskID, userID, before}
	extra, args := f.where(args)
	args = append(args, limit+1)
	return `SELECT ` + eventColumns + `
		FROM agent_events a
		JOIN tasks t ON t.id = a.task_id
		WHERE a.task_id = $1 AND t.user_id = $2 AND a.id < $3` + extra + `
		ORDER BY a.id DESC
		LIMIT $` + strconv.Itoa(len(args)), args
}

func (s *Store) queryEvents(ctx context.Context, sql string, args []any) ([]AgentEvent, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询 agent 事件失败: %w", err)
	}
	defer rows.Close()
	out := []AgentEvent{}
	for rows.Next() {
		var e AgentEvent
		if err := rows.Scan(&e.ID, &e.Phase, &e.Kind, &e.Tool, &e.Body, &e.Payload, &e.AgentID, &e.At); err != nil {
			return nil, fmt.Errorf("store: 读取 agent 事件行失败: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AgentEventsAfterFiltered 是 AgentEventsAfter 的过滤 + has_more 版本。
//
// f 为零值时返回的 events 与 AgentEventsAfter 逐行相同（同一 WHERE、同一
// ORDER、同一 limit 规则），只多一个 hasMore（多取一条判定，不额外 count）。
// userID 仍是第二道隔离（JOIN tasks）。
func (s *Store) AgentEventsAfterFiltered(ctx context.Context, taskID, userID, after int64, f EventFilter, limit int) (events []AgentEvent, hasMore bool, err error) {
	limit = clampLimit(limit)
	sql, args := agentEventsAfterQuery(taskID, userID, after, f, limit)
	events, err = s.queryEvents(ctx, sql, args)
	if err != nil {
		return nil, false, err
	}
	if len(events) > limit {
		return events[:limit], true, nil
	}
	return events, false, nil
}

// AgentEventsBefore 反向分页：返回 id < before 的最近 limit 条，按 id 升序。
// hasMore 表示还有更早的。
func (s *Store) AgentEventsBefore(ctx context.Context, taskID, userID, before int64, f EventFilter, limit int) (events []AgentEvent, hasMore bool, err error) {
	limit = clampLimit(limit)
	sql, args := agentEventsBeforeQuery(taskID, userID, before, f, limit)
	desc, err := s.queryEvents(ctx, sql, args)
	if err != nil {
		return nil, false, err
	}
	if len(desc) > limit {
		hasMore = true
		desc = desc[:limit]
	}
	out := make([]AgentEvent, len(desc))
	for i, e := range desc {
		out[len(desc)-1-i] = e
	}
	return out, hasMore, nil
}

// EventPhaseSummary 是 summary 里的一个阶段。
type EventPhaseSummary struct {
	Phase    string    `json:"phase"`
	Count    int       `json:"count"`
	FirstID  int64     `json:"firstId"`
	LastID   int64     `json:"lastId"`
	FirstAt  time.Time `json:"firstAt"`
	LastAt   time.Time `json:"lastAt"`
	Failures int       `json:"failures"`
}

// EventFailure 是一条失败（或警告）事件的定位信息。
type EventFailure struct {
	ID       int64     `json:"id"`
	Phase    string    `json:"phase"`
	Kind     string    `json:"kind"`
	At       time.Time `json:"at"`
	Severity string    `json:"severity"` // error | warn
}

// EventsSummary 是 /events/summary 的响应体：不受前端内存窗口影响的全局视图。
type EventsSummary struct {
	Total             int                 `json:"total"`
	FirstID           *int64              `json:"firstId"`
	LastID            *int64              `json:"lastId"`
	FirstAt           *time.Time          `json:"firstAt"`
	LastAt            *time.Time          `json:"lastAt"`
	Phases            []EventPhaseSummary `json:"phases"`
	Failures          []EventFailure      `json:"failures"`
	FailuresTruncated bool                `json:"failuresTruncated"`
	FailureCount      int                 `json:"failureCount"`
	FirstFailure      *EventFailure       `json:"firstFailure"`
	LastFailure       *EventFailure       `json:"lastFailure"`
}

// summaryPhasesSQL 按阶段聚合（单任务范围，走 (task_id, id) 索引）。
const summaryPhasesSQL = `
	SELECT a.phase, count(*), min(a.id), max(a.id), min(a.at), max(a.at),
	       count(*) FILTER (WHERE ` + failureErrorSQL + `)
	FROM agent_events a
	WHERE a.task_id = $1
	GROUP BY a.phase
	ORDER BY min(a.id)`

const severityColumnSQL = `CASE WHEN a.kind = 'raw' THEN 'warn' ELSE 'error' END`

// summaryFailuresSQL 取前 N 条失败/警告（$2 = 上限 + 1，用来判 truncated）。
const summaryFailuresSQL = `
	SELECT a.id, a.phase, a.kind, a.at, ` + severityColumnSQL + `
	FROM agent_events a
	WHERE a.task_id = $1 AND ` + FailurePredicateSQL + `
	ORDER BY a.id
	LIMIT $2`

// summaryEdgeFailureSQL 取 id 最小（ASC）或最大（DESC）的那条 error，
// 与 failures 数组的截断无关。dir 只接受两个常量。
func summaryEdgeFailureSQL(dir string) string {
	if dir != "DESC" {
		dir = "ASC"
	}
	return `
	SELECT a.id, a.phase, a.kind, a.at, 'error'
	FROM agent_events a
	WHERE a.task_id = $1 AND ` + failureErrorSQL + `
	ORDER BY a.id ` + dir + `
	LIMIT 1`
}

// AgentEventsSummary 汇总一个任务的事件：总数、各阶段起止与失败数、失败列表。
//
// 调用方（API 层）先做归属判定；这里按 task_id 查。
func (s *Store) AgentEventsSummary(ctx context.Context, taskID int64) (*EventsSummary, error) {
	out := &EventsSummary{Phases: []EventPhaseSummary{}, Failures: []EventFailure{}}

	rows, err := s.pool.Query(ctx, summaryPhasesSQL, taskID)
	if err != nil {
		return nil, fmt.Errorf("store: 汇总 agent 事件失败: %w", err)
	}
	for rows.Next() {
		var p EventPhaseSummary
		if err := rows.Scan(&p.Phase, &p.Count, &p.FirstID, &p.LastID, &p.FirstAt, &p.LastAt, &p.Failures); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: 读取阶段汇总失败: %w", err)
		}
		out.Phases = append(out.Phases, p)
		out.Total += p.Count
		out.FailureCount += p.Failures
		if out.FirstID == nil || p.FirstID < *out.FirstID {
			id, at := p.FirstID, p.FirstAt
			out.FirstID, out.FirstAt = &id, &at
		}
		if out.LastID == nil || p.LastID > *out.LastID {
			id, at := p.LastID, p.LastAt
			out.LastID, out.LastAt = &id, &at
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	frows, err := s.pool.Query(ctx, summaryFailuresSQL, taskID, summaryFailuresMax+1)
	if err != nil {
		return nil, fmt.Errorf("store: 查询失败事件失败: %w", err)
	}
	for frows.Next() {
		var f EventFailure
		if err := frows.Scan(&f.ID, &f.Phase, &f.Kind, &f.At, &f.Severity); err != nil {
			frows.Close()
			return nil, fmt.Errorf("store: 读取失败事件失败: %w", err)
		}
		out.Failures = append(out.Failures, f)
	}
	frows.Close()
	if err := frows.Err(); err != nil {
		return nil, err
	}
	if len(out.Failures) > summaryFailuresMax {
		out.Failures = out.Failures[:summaryFailuresMax]
		out.FailuresTruncated = true
	}

	if out.FailureCount > 0 {
		for _, edge := range []struct {
			dir string
			dst **EventFailure
		}{{"ASC", &out.FirstFailure}, {"DESC", &out.LastFailure}} {
			var f EventFailure
			if err := s.pool.QueryRow(ctx, summaryEdgeFailureSQL(edge.dir), taskID).
				Scan(&f.ID, &f.Phase, &f.Kind, &f.At, &f.Severity); err != nil {
				return nil, fmt.Errorf("store: 查询首末失败失败: %w", err)
			}
			*edge.dst = &f
		}
	}
	return out, nil
}

// VerificationLogRef 取一条验证行的 log_ref 与落库时刻。
//
// 同时按 task_id 限定：vid 不属于该任务时与「不存在」不可区分（found=false）。
// 归属判定（任务属主）由 API 层的 ownedTask 先做。
func (s *Store) VerificationLogRef(ctx context.Context, taskID, vid int64) (ref *string, at time.Time, found bool, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT log_ref, at FROM verifications WHERE id = $1 AND task_id = $2`, vid, taskID).Scan(&ref, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, time.Time{}, false, nil
	}
	if err != nil {
		return nil, time.Time{}, false, fmt.Errorf("store: 查询验证日志引用失败: %w", err)
	}
	return ref, at, true, nil
}
