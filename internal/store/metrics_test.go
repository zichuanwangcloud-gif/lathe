package store

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// metricsFixture 造一个用户 + 一个仓库，返回 userID / repoID。
// 任务由各用例自己建 —— 度量测试要精确控制每个任务的事件流。
func metricsFixture(t *testing.T, st *Store) (userID, repoID int64) {
	t.Helper()
	ctx := context.Background()
	nonce := strconv.FormatInt(time.Now().UnixNano(), 10)

	if err := st.pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"metrics-"+t.Name()+"-"+nonce+"@example.com").Scan(&userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		userID, "acme/metrics-"+nonce).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	return userID, repoID
}

// metricsTask 建一个任务行，返回 taskID。
func metricsTask(t *testing.T, st *Store, userID, repoID int64, key string) int64 {
	t.Helper()
	var id int64
	if err := st.pool.QueryRow(context.Background(),
		`INSERT INTO tasks (user_id, repo_id, external_key) VALUES ($1,$2,$3) RETURNING id`,
		userID, repoID, key).Scan(&id); err != nil {
		t.Fatalf("建 task 失败: %v", err)
	}
	return id
}

// metricsEvent 在 base 之后的第 offsetMS 毫秒插一条状态转移事件。
//
// at 显式指定而不是用 now()：度量断言的是【时间差】，用 now() 的话
// 所有事件的时间差都是亚毫秒级噪声，分位数断言就变成了掷骰子。
func metricsEvent(t *testing.T, st *Store, taskID int64, to string, base time.Time, offsetMS int64) {
	t.Helper()
	if _, err := st.pool.Exec(context.Background(),
		`INSERT INTO task_events (task_id, to_state, actor, at) VALUES ($1,$2,'system',$3)`,
		taskID, to, base.Add(time.Duration(offsetMS)*time.Millisecond)); err != nil {
		t.Fatalf("插事件 %s 失败: %v", to, err)
	}
}

// metricsTaskFailed 把一个任务行置成 failed 并写下失败阶段。
func metricsTaskFailed(t *testing.T, st *Store, taskID int64, stage string) {
	t.Helper()
	if _, err := st.pool.Exec(context.Background(),
		`UPDATE tasks SET state='failed', failure_stage=$2 WHERE id=$1`,
		taskID, stage); err != nil {
		t.Fatalf("置 failed 失败: %v", err)
	}
}

// findSegment 在结果里取指定键的环节，不存在则失败。
func findSegment(t *testing.T, ms *MetricsStats, key string) SegmentStats {
	t.Helper()
	for _, s := range ms.Segments {
		if s.Key == key {
			return s
		}
	}
	t.Fatalf("结果里没有环节 %s，实际: %+v", key, ms.Segments)
	return SegmentStats{}
}

// ★ AC1 的核心断言：造已知时间戳序列 → 分位数结果必须等于手算值。
//
// 造 4 个任务，各自的 queue 段刻意取 10/20/30/100 秒（毫秒值
// 10000/20000/30000/100000），验 p50 与 p95 落在算得出来的位置上。
func TestMetricsSegmentsPercentiles(t *testing.T) {
	st := testStore(t)
	userID, repoID := metricsFixture(t, st)
	ctx := context.Background()
	base := time.Now().Add(-1 * time.Hour) // 固定的基准，避开 now() 的漂移

	// queue 段（queued → triaging）依次 10s / 20s / 30s / 100s；
	// triage 段（triaging → implementing）统一给 5s，让各环节互不干扰。
	queueMS := []int64{10_000, 20_000, 30_000, 100_000}
	for i, q := range queueMS {
		tk := metricsTask(t, st, userID, repoID, "MT-"+strconv.Itoa(i))
		b := base.Add(time.Duration(i) * time.Hour)
		metricsEvent(t, st, tk, "queued", b, 0)
		metricsEvent(t, st, tk, "triaging", b, q)
		metricsEvent(t, st, tk, "implementing", b, q+5_000)
	}

	ms, err := st.MetricsStatsFor(ctx, userID)
	if err != nil {
		t.Fatalf("MetricsStatsFor 失败: %v", err)
	}

	q := findSegment(t, ms, SegmentQueue)
	if q.Count != 4 {
		t.Fatalf("queue 样本数 = %d，期望 4", q.Count)
	}
	// percentile_cont 线性插值：4 个点 [10,20,30,100] 秒
	//   p50 = 位置 0.5*(4-1) = 1.5 → 在 20 与 30 之间 → 25 秒
	//   p95 = 位置 0.95*3 = 2.85 → 在 30 与 100 之间 → 30 + 0.85*70 = 89.5 秒
	if want := 25_000.0; !closeEnough(q.P50MS, want) {
		t.Errorf("queue p50 = %v ms，期望 %v", q.P50MS, want)
	}
	if want := 89_500.0; !closeEnough(q.P95MS, want) {
		t.Errorf("queue p95 = %v ms，期望 %v", q.P95MS, want)
	}
	if want := 100_000.0; !closeEnough(q.MaxMS, want) {
		t.Errorf("queue max = %v ms，期望 %v", q.MaxMS, want)
	}

	// triage 段 4 个样本全是 5 秒 → 分位数退化到同一个值
	tr := findSegment(t, ms, SegmentTriage)
	if tr.Count != 4 {
		t.Fatalf("triage 样本数 = %d，期望 4", tr.Count)
	}
	if !closeEnough(tr.P50MS, 5_000) || !closeEnough(tr.P95MS, 5_000) {
		t.Errorf("triage 各样本等长时 p50/p95 应都等于 5000，得到 %v / %v", tr.P50MS, tr.P95MS)
	}

	// 这 4 个任务还没开 PR：implement / verify / total 都不该有样本
	for _, key := range []string{SegmentImplement, SegmentVerify, SegmentTotal} {
		for _, s := range ms.Segments {
			if s.Key == key {
				t.Errorf("环节 %s 不该有样本（任务未到 pr_open），得到 Count=%d", key, s.Count)
			}
		}
	}

	// 参与统计的任务数是 4（不是样本数之和）
	if ms.Tasks != 4 {
		t.Errorf("参与统计的任务数 = %d，期望 4", ms.Tasks)
	}
}

// ★ AC1 的另一半：空数据集不崩，且返回零值而非 null。
//
// 除零是这里最实际的风险：percentile_cont 在没有行时返回 NULL，
// count 是 0 —— 拿 0 去算比值就炸。这里断言的是「一个全新用户查出来
// 是空切片 + 0，而不是错误也不是 panic」。
func TestMetricsEmptyDataset(t *testing.T) {
	st := testStore(t)
	userID, _ := metricsFixture(t, st)

	ms, err := st.MetricsStatsFor(context.Background(), userID)
	if err != nil {
		t.Fatalf("空数据集查询不该报错: %v", err)
	}
	if ms.Segments == nil {
		t.Error("Segments 应为空切片而非 nil（前端不该判 null）")
	}
	if len(ms.Segments) != 0 {
		t.Errorf("空数据集不该有环节样本，得到 %+v", ms.Segments)
	}
	if ms.FailureStages == nil {
		t.Error("FailureStages 应为空切片而非 nil")
	}
	if len(ms.FailureStages) != 0 {
		t.Errorf("空数据集不该有失败分类，得到 %+v", ms.FailureStages)
	}
	if ms.Failed != 0 || ms.Tasks != 0 {
		t.Errorf("空数据集各项计数应为 0，得到 failed=%d tasks=%d", ms.Failed, ms.Tasks)
	}
}

// ★ AC2：多租户隔离 —— 统计只含当前用户的数据。
//
// 造两个用户，各自跑一条完整链路，断言互相看不见。这是 P1.5 数据隔离
// 在度量侧的红线：task_events 没有 user_id，漏掉 JOIN tasks 的过滤
// 就会把别人的单子算进你的分位数里。
func TestMetricsIsolatesUsers(t *testing.T) {
	st := testStore(t)
	userA, repoA := metricsFixture(t, st)
	userB, repoB := metricsFixture(t, st)
	ctx := context.Background()
	base := time.Now().Add(-1 * time.Hour)

	// A 的任务：queue 段 60 秒（明显区别于 B）
	tkA := metricsTask(t, st, userA, repoA, "ISO-A")
	metricsEvent(t, st, tkA, "queued", base, 0)
	metricsEvent(t, st, tkA, "triaging", base, 60_000)
	metricsEvent(t, st, tkA, "implementing", base, 61_000)
	metricsEvent(t, st, tkA, "verifying", base, 62_000)
	metricsEvent(t, st, tkA, "pr_open", base, 63_000)
	metricsTaskFailed(t, st, tkA, "verify_failed")

	// B 的任务：queue 段 1 秒，且失败阶段不同
	tkB := metricsTask(t, st, userB, repoB, "ISO-B")
	metricsEvent(t, st, tkB, "queued", base, 0)
	metricsEvent(t, st, tkB, "triaging", base, 1_000)
	metricsTaskFailed(t, st, tkB, "implement_run")

	msA, err := st.MetricsStatsFor(ctx, userA)
	if err != nil {
		t.Fatalf("查 A 失败: %v", err)
	}
	if msA.Tasks != 1 {
		t.Errorf("A 的任务数 = %d，期望 1（B 的不该被算进来）", msA.Tasks)
	}
	if got := findSegment(t, msA, SegmentQueue); !closeEnough(got.P50MS, 60_000) {
		t.Errorf("A 的 queue p50 = %v，期望 60000（B 的 1 秒样本漏进来了？）", got.P50MS)
	}
	if len(msA.FailureStages) != 1 || msA.FailureStages[0].Stage != "verify_failed" {
		t.Errorf("A 的失败分类 = %+v，期望只有 verify_failed", msA.FailureStages)
	}

	msB, err := st.MetricsStatsFor(ctx, userB)
	if err != nil {
		t.Fatalf("查 B 失败: %v", err)
	}
	if msB.Tasks != 1 {
		t.Errorf("B 的任务数 = %d，期望 1", msB.Tasks)
	}
	if got := findSegment(t, msB, SegmentQueue); !closeEnough(got.P50MS, 1_000) {
		t.Errorf("B 的 queue p50 = %v，期望 1000", got.P50MS)
	}
	// B 只走到 triaging，没有 pr_open —— total 段不该有样本
	for _, s := range msB.Segments {
		if s.Key == SegmentTotal {
			t.Errorf("B 未开 PR，total 段不该有样本，得到 Count=%d", s.Count)
		}
	}
}

// AC1 的补充：断点续跑跳过环节时产生的负差值必须被丢掉。
//
// 真实场景：EntryVerify 续跑让任务直接 queued → verifying，之后
// 才（在修复轮的续跑里）出现 implementing。取 min 之后
// implementing > verifying，「实现段」的差值就是负的。负样本不该进
// 分布 —— 留着会把 p50 拉向零，让人误以为实现变快了。
func TestMetricsSkipsNegativeSegments(t *testing.T) {
	st := testStore(t)
	userID, repoID := metricsFixture(t, st)
	ctx := context.Background()
	base := time.Now().Add(-1 * time.Hour)

	// 任务 1：正常的完整链路，实现段 = 20 秒
	ok := metricsTask(t, st, userID, repoID, "NEG-OK")
	metricsEvent(t, st, ok, "queued", base, 0)
	metricsEvent(t, st, ok, "triaging", base, 1_000)
	metricsEvent(t, st, ok, "implementing", base, 2_000)
	metricsEvent(t, st, ok, "verifying", base, 22_000)
	metricsEvent(t, st, ok, "pr_open", base, 23_000)

	// 任务 2：续跑跳过实现 —— 先 verifying，很久之后才有 implementing
	skip := metricsTask(t, st, userID, repoID, "NEG-SKIP")
	metricsEvent(t, st, skip, "queued", base, 0)
	metricsEvent(t, st, skip, "verifying", base, 1_000)
	metricsEvent(t, st, skip, "implementing", base, 500_000)
	metricsEvent(t, st, skip, "verifying", base, 501_000)
	metricsEvent(t, st, skip, "pr_open", base, 502_000)

	ms, err := st.MetricsStatsFor(ctx, userID)
	if err != nil {
		t.Fatalf("MetricsStatsFor 失败: %v", err)
	}

	// 实现段只该有任务 1 的样本（20000ms）。任务 2 的差值是
	// 1000 - 500000 = 负的，必须被丢掉。
	impl := findSegment(t, ms, SegmentImplement)
	if impl.Count != 1 {
		t.Fatalf("实现段样本数 = %d，期望 1（负差值样本应被丢弃）", impl.Count)
	}
	if !closeEnough(impl.P50MS, 20_000) {
		t.Errorf("实现段 p50 = %v，期望 20000", impl.P50MS)
	}

	// 验证段（verifying → pr_open）取 min(verifying)：任务 2 的
	// min(verifying)=1000，差值 502000-1000 = 501000，是正的，计入。
	ver := findSegment(t, ms, SegmentVerify)
	if ver.Count != 2 {
		t.Errorf("验证段样本数 = %d，期望 2", ver.Count)
	}
}

// 重试会多次进入同一个状态，里程碑取 **首次** 进入的时刻 ——
// 度量想问的是「平台处理这类单子要多久」，混进重跑轮会把两个量纲
// 不同的数搅在一起（重跑轮的排队段往往只有几秒）。
func TestMetricsUsesFirstEntryOnRetry(t *testing.T) {
	st := testStore(t)
	userID, repoID := metricsFixture(t, st)
	ctx := context.Background()
	base := time.Now().Add(-1 * time.Hour)

	// 首轮：queue 60 秒（0 → 60000），分诊 10 秒。
	tk := metricsTask(t, st, userID, repoID, "RETRY-1")
	metricsEvent(t, st, tk, "queued", base, 0)
	metricsEvent(t, st, tk, "triaging", base, 60_000)
	metricsEvent(t, st, tk, "implementing", base, 70_000)

	// 失败重试：回到 queued（事件流里第二次出现 queued），这次只等了 1 秒
	// 就重新进分诊。若里程碑取 max，queue 段会算成 600000-60000 = 540 秒。
	metricsEvent(t, st, tk, "queued", base, 600_000)
	metricsEvent(t, st, tk, "triaging", base, 601_000)

	ms, err := st.MetricsStatsFor(ctx, userID)
	if err != nil {
		t.Fatalf("MetricsStatsFor 失败: %v", err)
	}
	q := findSegment(t, ms, SegmentQueue)
	if q.Count != 1 {
		t.Fatalf("queue 样本数 = %d，期望 1（一个任务一个样本）", q.Count)
	}
	if !closeEnough(q.P50MS, 60_000) {
		t.Errorf("queue p50 = %v，期望 60000（首轮）—— 取到别的值说明里程碑没取「首次进入」", q.P50MS)
	}
}

// 失败分类直方图：按阶段计数、按次数降序、历史无阶段的进空键桶。
func TestMetricsFailureStages(t *testing.T) {
	st := testStore(t)
	userID, repoID := metricsFixture(t, st)
	ctx := context.Background()

	// verify_failed × 2、implement_run × 1、无阶段（模拟 0012 前的历史数据）× 1
	for i, stage := range []string{"verify_failed", "verify_failed", "implement_run", ""} {
		tk := metricsTask(t, st, userID, repoID, "FS-"+strconv.Itoa(i))
		metricsTaskFailed(t, st, tk, stage)
	}

	ms, err := st.MetricsStatsFor(ctx, userID)
	if err != nil {
		t.Fatalf("MetricsStatsFor 失败: %v", err)
	}
	if ms.Failed != 4 {
		t.Errorf("失败总数 = %d，期望 4", ms.Failed)
	}
	if len(ms.FailureStages) != 3 {
		t.Fatalf("失败分类桶数 = %d，期望 3（两个已知阶段 + 一个空键），得到 %+v",
			len(ms.FailureStages), ms.FailureStages)
	}
	// 降序：verify_failed(2) 在最前
	if ms.FailureStages[0].Stage != "verify_failed" || ms.FailureStages[0].Count != 2 {
		t.Errorf("首格 = %+v，期望 verify_failed×2", ms.FailureStages[0])
	}
	// 空键桶必须存在且计数为 1 —— 历史失败不该被悄悄从分母里拿掉
	found := false
	for _, fc := range ms.FailureStages {
		if fc.Stage == "" {
			found = true
			if fc.Count != 1 {
				t.Errorf("无阶段桶计数 = %d，期望 1", fc.Count)
			}
		}
	}
	if !found {
		t.Error("无阶段的历史失败应单独成桶，不该被丢弃")
	}

	// 各格之和等于失败总数（同一 WHERE 口径）
	sum := 0
	for _, fc := range ms.FailureStages {
		sum += fc.Count
	}
	if sum != ms.Failed {
		t.Errorf("直方图之和 %d ≠ 失败总数 %d", sum, ms.Failed)
	}
}

// segmentOrder 决定输出顺序：前端按响应顺序渲染图例，
// 顺序必须稳定，不能随数据量或 SQL 的返回次序变。
func TestMetricsSegmentOrderStable(t *testing.T) {
	st := testStore(t)
	userID, repoID := metricsFixture(t, st)
	ctx := context.Background()
	base := time.Now().Add(-1 * time.Hour)

	tk := metricsTask(t, st, userID, repoID, "ORD-1")
	metricsEvent(t, st, tk, "queued", base, 0)
	metricsEvent(t, st, tk, "triaging", base, 1_000)
	metricsEvent(t, st, tk, "implementing", base, 2_000)
	metricsEvent(t, st, tk, "verifying", base, 3_000)
	metricsEvent(t, st, tk, "pr_open", base, 4_000)

	ms, err := st.MetricsStatsFor(ctx, userID)
	if err != nil {
		t.Fatalf("MetricsStatsFor 失败: %v", err)
	}
	if len(ms.Segments) != len(segmentOrder) {
		t.Fatalf("环节数 = %d，期望 %d", len(ms.Segments), len(segmentOrder))
	}
	for i, want := range segmentOrder {
		if ms.Segments[i].Key != want {
			t.Errorf("第 %d 个环节 = %s，期望 %s（顺序是前端图例的契约）",
				i, ms.Segments[i].Key, want)
		}
	}
}

// closeEnough 比较浮点毫秒值：EXTRACT(EPOCH) 是 numeric → float8，
// 有舍入误差，不能用 ==。
func closeEnough(got, want float64) bool {
	diff := got - want
	return diff < 0.5 && diff > -0.5
}
