package store

// cost.go agent 花费的聚合视图（docs/08-debt-cleanup.md T5）。
//
// 这是回答「什么单子值得交给 agent」的决策数据。roadmap §3.2 记的第 5 条：
// 成本字段早就解析落库了，缺的是聚合视图。
//
// ---------------------------------------------------------------------------
// 一个必须知道的坑：**不能用 tasks.agent_cost_usd**
// ---------------------------------------------------------------------------
//
// 那一列看起来正好是「这个任务花了多少钱」，但它不是：
//
//   - 写入它的 store.SetAgentSummary 是 UPDATE 整行覆盖
//   - 而 pipeline.persistAgentResult 在实现阶段与【修复回路的每一轮】
//     都会调用它
//
// 所以那一列最终存的是**最后一轮 fix 的单轮成本**，不是任务累计。
// 拿它做成本面板会严重低估 —— 恰恰在「修复回路烧了很多轮」这种最该
// 被看见的情形下低估得最厉害。
//
// 另外 migration 0009 的头注释明说**分诊阶段的 result 不落 tasks 表**
// （它的正文是结构化 JSON 判定，不是人读摘要），所以「分诊 vs 实现占比」
// 从那一列根本算不出来。
//
// 真正的数据源是 agent_events：kind='result' 的 payload->>'costUsd'，
// 写入点在 internal/integration/agent/digest.go。每个阶段一行，不覆盖。
//
// ---------------------------------------------------------------------------
// 多租户隔离
// ---------------------------------------------------------------------------
//
// agent_events **没有 user_id 列**（它按 task_id 级联），所以每个查询都必须
// JOIN tasks 并按 t.user_id 过滤。漏掉 JOIN 就是跨用户求和 —— P1.5 数据隔离
// 的红线。

import (
	"context"
	"fmt"
)

// costDays 是按日曲线回看的天数。
//
// 30 天足够看出趋势又不至于让前端画不下；真要更长的历史，
// 那是「平台自度量」（roadmap §3.6 第 15 条）该解决的问题，不是这个面板。
const costDays = 30

// CostStats 是 agent 花费的聚合结果。
//
// 所有字段都是值类型、非指针：AC4 要求「无成本数据时返回零值而非 null」，
// 前端不必到处判空。map 与 slice 一律初始化成空而不是 nil，
// 沿用 Stats.ByState 用 map[string]int{} 的既有做法。
type CostStats struct {
	// TotalUSD 是累计花费（美元）。
	TotalUSD float64 `json:"totalUsd"`
	// Runs 是 agent 执行次数（result 事件数），不是任务数 ——
	// 一个任务可能跑分诊 + 实现 + N 轮修复。
	Runs int `json:"runs"`
	// DurationMS 是累计墙钟耗时。
	DurationMS int64 `json:"durationMs"`
	// Turns 是累计对话轮数。
	Turns int `json:"turns"`

	// ByPhase 是按阶段桶的花费：triage（分诊）/ implement（实现，
	// 含修复回路各轮）/ other（verify/review/push 等，正常应接近 0）。
	ByPhase map[string]PhaseCost `json:"byPhase"`

	// ByDay 是最近 costDays 天的日花费，按日期升序。没有数据的日子不出现
	// （不补零：补零是画图时的事，聚合层不该假设消费方要怎么画）。
	ByDay []DayCost `json:"byDay"`

	// TopTasks 是花费最高的若干个任务，按花费降序。
	// 这是「什么单子值得交给 agent」最直接的入口。
	TopTasks []TaskCost `json:"topTasks"`
}

// PhaseCost 是单个阶段桶的花费。
type PhaseCost struct {
	USD  float64 `json:"usd"`
	Runs int     `json:"runs"`
}

// DayCost 是单日花费。
type DayCost struct {
	// Day 是 UTC 日期（YYYY-MM-DD）。刻意用 UTC 而不是服务器本地时区：
	// 本地时区会随部署环境变，同一份数据在两台机器上画出不同的曲线。
	Day  string  `json:"day"`
	USD  float64 `json:"usd"`
	Runs int     `json:"runs"`
}

// TaskCost 是单个任务的累计花费。
type TaskCost struct {
	TaskID   int64   `json:"taskId"`
	IssueKey string  `json:"issueKey"`
	State    string  `json:"state"`
	USD      float64 `json:"usd"`
	Runs     int     `json:"runs"`
}

// newCostStats 造一个各集合都已初始化的零值 —— 避免 JSON 里出现 null。
func newCostStats() *CostStats {
	return &CostStats{
		ByPhase:  map[string]PhaseCost{},
		ByDay:    []DayCost{},
		TopTasks: []TaskCost{},
	}
}

// costFilter 是四个查询共用的谓词。
//
// `payload ? 'costUsd'` 挡掉没有成本字段的 result 事件（早期数据、
// 或将来新增的 result 形态）：少了它，`(payload->>'costUsd')::numeric`
// 会在遇到缺字段的行时拿到 NULL —— SUM 会忽略，但 count 会虚高。
const costFilter = `
	FROM agent_events ae
	JOIN tasks t ON t.id = ae.task_id
	WHERE t.user_id = $1
	  AND ae.kind = 'result'
	  AND ae.payload ? 'costUsd'`

// CostStatsFor 聚合指定用户名下的 agent 花费。
//
// 刻意不挂在 Stats() 里：看板每 5 秒轮询一次 /api/stats，而成本是决策视图、
// 不需要 5 秒新鲜度。四个跨 agent_events 的聚合查询压进那个轮询里
// 纯属浪费（agent_events 是全表最大的一张）。独立端点按需拉取。
func (s *Store) CostStatsFor(ctx context.Context, userID int64) (*CostStats, error) {
	cs := newCostStats()

	// 1) 总计
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM((ae.payload->>'costUsd')::numeric), 0)::float8,
		       COUNT(*),
		       COALESCE(SUM((ae.payload->>'durationMs')::bigint), 0),
		       COALESCE(SUM((ae.payload->>'numTurns')::int), 0)`+costFilter,
		userID).Scan(&cs.TotalUSD, &cs.Runs, &cs.DurationMS, &cs.Turns)
	if err != nil {
		return nil, fmt.Errorf("store: 统计 agent 总花费失败: %w", err)
	}

	// 2) 按阶段。implement 与 fix-N 归一个桶：修复回路是实现的延续，
	// 分开看没有决策价值，合起来才回答「实现这件事总共花了多少」。
	phaseRows, err := s.pool.Query(ctx, `
		SELECT CASE
		         WHEN ae.phase = 'triage' THEN 'triage'
		         WHEN ae.phase = 'implement' OR ae.phase ~ '^fix-[0-9]+$' THEN 'implement'
		         ELSE 'other'
		       END AS bucket,
		       COALESCE(SUM((ae.payload->>'costUsd')::numeric), 0)::float8,
		       COUNT(*)`+costFilter+`
		GROUP BY 1`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: 统计阶段花费失败: %w", err)
	}
	defer phaseRows.Close()
	for phaseRows.Next() {
		var bucket string
		var pc PhaseCost
		if err := phaseRows.Scan(&bucket, &pc.USD, &pc.Runs); err != nil {
			return nil, fmt.Errorf("store: 读取阶段花费失败: %w", err)
		}
		cs.ByPhase[bucket] = pc
	}
	if err := phaseRows.Err(); err != nil {
		return nil, err
	}

	// 3) 按日（UTC）
	dayRows, err := s.pool.Query(ctx, `
		SELECT to_char(ae.at AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day,
		       COALESCE(SUM((ae.payload->>'costUsd')::numeric), 0)::float8,
		       COUNT(*)`+costFilter+`
		  AND ae.at >= now() - make_interval(days => $2)
		GROUP BY 1 ORDER BY 1`, userID, costDays)
	if err != nil {
		return nil, fmt.Errorf("store: 统计日花费失败: %w", err)
	}
	defer dayRows.Close()
	for dayRows.Next() {
		var d DayCost
		if err := dayRows.Scan(&d.Day, &d.USD, &d.Runs); err != nil {
			return nil, fmt.Errorf("store: 读取日花费失败: %w", err)
		}
		cs.ByDay = append(cs.ByDay, d)
	}
	if err := dayRows.Err(); err != nil {
		return nil, err
	}

	// 4) 花费最高的任务
	topRows, err := s.pool.Query(ctx, `
		SELECT t.id, t.linear_issue_key, t.state,
		       COALESCE(SUM((ae.payload->>'costUsd')::numeric), 0)::float8,
		       COUNT(*)`+costFilter+`
		GROUP BY t.id, t.linear_issue_key, t.state
		ORDER BY 4 DESC, t.id
		LIMIT 10`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: 统计任务花费排行失败: %w", err)
	}
	defer topRows.Close()
	for topRows.Next() {
		var tc TaskCost
		if err := topRows.Scan(&tc.TaskID, &tc.IssueKey, &tc.State, &tc.USD, &tc.Runs); err != nil {
			return nil, fmt.Errorf("store: 读取任务花费排行失败: %w", err)
		}
		cs.TopTasks = append(cs.TopTasks, tc)
	}
	return cs, topRows.Err()
}
