package store

// metrics.go 平台自度量（docs/05-roadmap.md §3.6 第 15 条）。
//
// 回答的问题是「迭代平台本身时，改动有没有用」。既有 Stats() 只有状态计数与
// 成功率，CostStatsFor() 只有花钱 —— 都不足以回答「慢在哪个环节」「一类失败
// 是不是在变多」。
//
// ---------------------------------------------------------------------------
// 数据源：task_events，不是 tasks 的 created_at/updated_at
// ---------------------------------------------------------------------------
//
// tasks 行只有 created_at / updated_at 两个时间戳，而 updated_at 被
// set_updated_at 触发器在【每次】UPDATE 时刷新 —— 它记的是「最后一次动过」，
// 不是「进入 verifying 的时刻」。拿它算环节时长只能得到「整单总耗时」，
// 算不出分段。
//
// task_events 是 append-only 的状态转移审计流（0001 迁移的设计），每行带
// 转移时刻，且任务状态可由它完整重放 —— 环节时长天然是它的一阶差分。
// 「失败分类直方图」同样：failure_stage 早就落在 tasks 上了（0012），
// 缺的一直是聚合视图。
//
// ---------------------------------------------------------------------------
// 多租户隔离
// ---------------------------------------------------------------------------
//
// task_events 没有 user_id（它按 task_id 级联），所以每个查询都必须
// JOIN tasks 并按 t.user_id 过滤。与 cost.go 是同一道红线：漏掉 JOIN
// 就是跨用户统计。

import (
	"context"
	"fmt"
)

// 环节的稳定键。前端按这些键映射中文名与展示顺序 ——
// 键是契约，标签是展示，与 STATE_META 的分工一致。
const (
	// SegmentQueue 排队等待：任务创建 → 首次进入分诊。
	SegmentQueue = "queue"
	// SegmentTriage 分诊：首次进入分诊 → 首次进入实现。
	SegmentTriage = "triage"
	// SegmentImplement 实现：首次进入实现 → 首次进入验证。
	SegmentImplement = "implement"
	// SegmentVerify 验证：首次进入验证 → 开出 PR。
	SegmentVerify = "verify"
	// SegmentTotal 端到端：任务创建 → 开出 PR。
	SegmentTotal = "total"
)

// SegmentStats 是单个环节的时长分布（毫秒）。
type SegmentStats struct {
	Key string `json:"key"`
	// Count 是样本数 —— 不是任务数。只有两端都在事件流里出现过
	// 的任务才计入某个环节，所以各环节的 Count 可以不同。
	Count int `json:"count"`
	// P50MS / P95MS 是分位数，MaxMS 是最大值，TotalMS 是该环节的
	// 累计耗时（回答「这段时间总共耗在验证上多少」，与花费面板的
	// 累计耗时口径一致）。
	P50MS   float64 `json:"p50Ms"`
	P95MS   float64 `json:"p95Ms"`
	MaxMS   float64 `json:"maxMs"`
	TotalMS float64 `json:"totalMs"`
}

// FailureStageCount 是失败分类直方图的一格。
type FailureStageCount struct {
	// Stage 是机器可读的失败阶段代码（runner.Stage）；空串表示
	// 历史数据里没有记阶段（0012 之前的失败任务）。中文名由 API 层
	// 经 runner.Stage 的标签方法补 —— store 不认识 runner 的措辞。
	Stage string `json:"stage"`
	Count int    `json:"count"`
}

// MetricsStats 是平台自度量的聚合结果。
//
// 与 CostStats 同一条约定：所有集合初始化成空而不是 nil，前端不必判空。
type MetricsStats struct {
	// Segments 按环节顺序（queue → triage → implement → verify → total）
	// 给出分位数。某个环节一个样本都没有时该项不出现。
	Segments []SegmentStats `json:"segments"`
	// FailureStages 是失败任务的分类直方图，按次数降序。
	FailureStages []FailureStageCount `json:"failureStages"`
	// Failed 是失败任务总数 = 直方图各格之和（同一条 WHERE）。
	// 留着它是为了让前端能显示「失败 N 单」而不必自己求和。
	Failed int `json:"failed"`
	// Tasks 是参与环节统计的任务数（事件流里有相关转移、无论终态）。
	// 用来判断样本是否足以采信上面那些分位数。
	Tasks int `json:"tasks"`
}

// segmentMarksCTE 把每个任务的里程碑收敛成一行「首次进入各状态的时刻」。
//
// 取 **min(at)** 即「第一次进入该状态」：重试会让同一个任务多次回到
// queued / implementing，取 min 得到的是首轮的环节时长 —— 这正是
// 「平台处理这类单子要多久」想问的东西。混合首轮与重跑轮会把两个
// 量纲不同的数搅在一起（重跑轮的 queue 段可能只有几秒）。
//
// 里程碑只取五个状态。pr_open → merged 不在分段里：合并是人的动作，
// 不是平台的环节耗时。
const segmentMarksCTE = `
WITH marks AS (
    SELECT e.task_id, e.to_state, min(e.at) AS at
    FROM task_events e
    JOIN tasks t ON t.id = e.task_id
    WHERE t.user_id = $1
      AND e.to_state IN ('queued', 'triaging', 'implementing', 'verifying', 'pr_open')
    GROUP BY e.task_id, e.to_state
),
per_task AS (
    SELECT task_id,
           min(at) FILTER (WHERE to_state = 'queued')       AS queued_at,
           min(at) FILTER (WHERE to_state = 'triaging')     AS triaging_at,
           min(at) FILTER (WHERE to_state = 'implementing') AS implementing_at,
           min(at) FILTER (WHERE to_state = 'verifying')    AS verifying_at,
           min(at) FILTER (WHERE to_state = 'pr_open')      AS pr_open_at
    FROM marks
    GROUP BY task_id
)`

// segmentDeltasCTE 把里程碑差分成环节时长。
//
// **ms >= 0 的过滤在消费端**：断点续跑会跳过环节（PlanRetry 的
// EntryVerify 让任务直接 queued → verifying），于是「首次进入 verifying」
// 可能早于「首次进入 implementing」，差值为负。负样本不是数据错误，是
// 「这个环节被跳过了」，不该进分布 —— 留着会把 p50 拉向零，让人误以为
// 实现变快了。
const segmentDeltasCTE = `
seg AS (
    SELECT 'queue' AS key, task_id,
           EXTRACT(EPOCH FROM (triaging_at - queued_at)) * 1000 AS ms
      FROM per_task WHERE queued_at IS NOT NULL AND triaging_at IS NOT NULL
    UNION ALL
    SELECT 'triage', task_id,
           EXTRACT(EPOCH FROM (implementing_at - triaging_at)) * 1000
      FROM per_task WHERE triaging_at IS NOT NULL AND implementing_at IS NOT NULL
    UNION ALL
    SELECT 'implement', task_id,
           EXTRACT(EPOCH FROM (verifying_at - implementing_at)) * 1000
      FROM per_task WHERE implementing_at IS NOT NULL AND verifying_at IS NOT NULL
    UNION ALL
    SELECT 'verify', task_id,
           EXTRACT(EPOCH FROM (pr_open_at - verifying_at)) * 1000
      FROM per_task WHERE verifying_at IS NOT NULL AND pr_open_at IS NOT NULL
    UNION ALL
    SELECT 'total', task_id,
           EXTRACT(EPOCH FROM (pr_open_at - queued_at)) * 1000
      FROM per_task WHERE queued_at IS NOT NULL AND pr_open_at IS NOT NULL
)`

// segmentQuery 一次算出五个环节的分位数。
//
// percentile_cont（线性插值）而非 percentile_disc（取实际存在的那一个）：
// 样本量在自用平台上不大，disc 会让 p95 在二十来个样本时直接等于最大值，
// 失去「95% 的单子在这个量级以内」的意义。
const segmentQuery = segmentMarksCTE + `,
` + segmentDeltasCTE + `
SELECT key,
       count(*),
       COALESCE(percentile_cont(0.5)  WITHIN GROUP (ORDER BY ms), 0),
       COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY ms), 0),
       COALESCE(max(ms), 0),
       COALESCE(sum(ms), 0)
FROM seg
WHERE ms >= 0
GROUP BY key`

// segmentTaskCountQuery 数参与统计的任务数。
//
// 单独一条而不是从上面结果的 count 求和：count 是样本数（一个任务
// 在多个环节各算一个样本），求和得到的是「环节样本总数」，拿来当
// 「任务数」是错的。
const segmentTaskCountQuery = segmentMarksCTE + `
SELECT count(*) FROM per_task`

// failureStageQuery 按失败阶段分类计数。
//
// 不限定 failure_stage IS NOT NULL：0012 之前的失败任务没有阶段代码，
// 把它们并进一个空键的桶，比悄悄从分母里拿掉更诚实（「有一批历史失败
// 没有分类」本身就是该被看见的信息）。COALESCE 成空串而不是在 Go 里
// 处理 NULL，是为了让扫描目标保持 string。
const failureStageQuery = `
SELECT COALESCE(failure_stage, ''), count(*)
FROM tasks
WHERE user_id = $1 AND state = 'failed'
GROUP BY 1
ORDER BY 2 DESC, 1`

// segmentOrder 固定输出顺序：前端按这个序渲染，避免图例随数据重排。
var segmentOrder = []string{SegmentQueue, SegmentTriage, SegmentImplement, SegmentVerify, SegmentTotal}

// MetricsStatsFor 聚合指定用户名下的环节时长分布与失败分类直方图。
//
// 刻意不挂在 Stats() 里：看板每 5 秒轮询 /api/stats，而这两个聚合都要
// 扫 task_events 并按任务分组（task_events 是库里最大的一张）。
// 与 costStats 同一个理由，独立端点按需拉取。
func (s *Store) MetricsStatsFor(ctx context.Context, userID int64) (*MetricsStats, error) {
	ms := &MetricsStats{Segments: []SegmentStats{}, FailureStages: []FailureStageCount{}}

	// 1) 环节时长分布
	rows, err := s.pool.Query(ctx, segmentQuery, userID)
	if err != nil {
		return nil, fmt.Errorf("store: 统计环节时长失败: %w", err)
	}
	defer rows.Close()

	// 先收进 map：SQL 的 GROUP BY 不保证顺序，而输出要有稳定次序。
	byKey := map[string]SegmentStats{}
	for rows.Next() {
		var st SegmentStats
		if err := rows.Scan(&st.Key, &st.Count, &st.P50MS, &st.P95MS, &st.MaxMS, &st.TotalMS); err != nil {
			return nil, fmt.Errorf("store: 读取环节时长失败: %w", err)
		}
		byKey[st.Key] = st
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, k := range segmentOrder {
		if st, ok := byKey[k]; ok {
			ms.Segments = append(ms.Segments, st)
		}
	}

	// 2) 参与统计的任务数
	if err := s.pool.QueryRow(ctx, segmentTaskCountQuery, userID).Scan(&ms.Tasks); err != nil {
		return nil, fmt.Errorf("store: 统计任务数失败: %w", err)
	}

	// 3) 失败分类直方图
	failRows, err := s.pool.Query(ctx, failureStageQuery, userID)
	if err != nil {
		return nil, fmt.Errorf("store: 统计失败分类失败: %w", err)
	}
	defer failRows.Close()
	for failRows.Next() {
		var fc FailureStageCount
		if err := failRows.Scan(&fc.Stage, &fc.Count); err != nil {
			return nil, fmt.Errorf("store: 读取失败分类失败: %w", err)
		}
		ms.FailureStages = append(ms.FailureStages, fc)
		ms.Failed += fc.Count
	}
	return ms, failRows.Err()
}
