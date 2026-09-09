package store

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// costFixture 造一个用户 + 一个任务，返回 userID 与 taskID。
func costFixture(t *testing.T, st *Store) (userID, taskID int64) {
	t.Helper()
	ctx := context.Background()
	nonce := strconv.FormatInt(time.Now().UnixNano(), 10)

	if err := st.pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"cost-"+t.Name()+"-"+nonce+"@example.com").Scan(&userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})
	var repoID int64
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		userID, "acme/cost-"+nonce).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO tasks (user_id, repo_id, linear_issue_key) VALUES ($1,$2,$3) RETURNING id`,
		userID, repoID, "CT-"+nonce).Scan(&taskID); err != nil {
		t.Fatalf("建 task 失败: %v", err)
	}
	return userID, taskID
}

// costEvent 插一条 kind=result 的 agent 事件。
func costEvent(t *testing.T, st *Store, taskID int64, phase string, usd float64, turns int, ms int64) {
	t.Helper()
	if _, err := st.pool.Exec(context.Background(), `
		INSERT INTO agent_events (task_id, phase, kind, payload)
		VALUES ($1, $2, 'result', jsonb_build_object(
			'costUsd', $3::float8, 'numTurns', $4::int, 'durationMs', $5::bigint))`,
		taskID, phase, usd, turns, ms); err != nil {
		t.Fatalf("插 result 事件失败: %v", err)
	}
}

// ★ T5-AC2 的核心断言：成本必须从 agent_events 求和，不能用
// tasks.agent_cost_usd —— 那一列被修复回路每轮 UPDATE 覆盖，
// 存的是最后一轮的单轮成本，不是任务累计。
//
// 造一个「分诊 + 实现 + 2 轮 fix」的任务，并刻意把 tasks.agent_cost_usd
// 设成最后一轮那个小数字。聚合结果必须等于各轮之和，而不是那一列。
func TestCostStatsSumsEventsNotTaskColumn(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, taskID := costFixture(t, st)

	costEvent(t, st, taskID, "triage", 0.02, 3, 1000)
	costEvent(t, st, taskID, "implement", 0.50, 20, 60000)
	costEvent(t, st, taskID, "fix-1", 0.30, 12, 30000)
	costEvent(t, st, taskID, "fix-2", 0.08, 5, 9000)

	// 模拟真实写入行为：SetAgentSummary 覆盖式 UPDATE，最终留下最后一轮
	if _, err := st.pool.Exec(ctx,
		`UPDATE tasks SET agent_cost_usd = 0.08 WHERE id = $1`, taskID); err != nil {
		t.Fatalf("设置 tasks.agent_cost_usd 失败: %v", err)
	}

	cs, err := st.CostStatsFor(ctx, userID)
	if err != nil {
		t.Fatalf("CostStatsFor 失败: %v", err)
	}

	const want = 0.02 + 0.50 + 0.30 + 0.08
	if diff := cs.TotalUSD - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("AC2：累计花费 = %.4f，期望各轮之和 %.4f（若得到 0.08 说明用了 tasks.agent_cost_usd）",
			cs.TotalUSD, want)
	}
	if cs.Runs != 4 {
		t.Errorf("执行次数 = %d，期望 4（分诊 + 实现 + 2 轮 fix）", cs.Runs)
	}
	if cs.Turns != 3+20+12+5 {
		t.Errorf("累计轮数 = %d，期望 %d", cs.Turns, 3+20+12+5)
	}
	if cs.DurationMS != 1000+60000+30000+9000 {
		t.Errorf("累计耗时 = %d，期望 %d", cs.DurationMS, 1000+60000+30000+9000)
	}

	// 阶段分桶：fix-N 必须归到 implement，不能自成一桶
	tri, ok := cs.ByPhase["triage"]
	if !ok || tri.USD != 0.02 {
		t.Errorf("分诊桶 = %+v，期望 0.02", tri)
	}
	impl, ok := cs.ByPhase["implement"]
	if !ok {
		t.Fatalf("应有 implement 桶，实际 %+v", cs.ByPhase)
	}
	if diff := impl.USD - (0.50 + 0.30 + 0.08); diff > 1e-9 || diff < -1e-9 {
		t.Errorf("实现桶 = %.4f，期望 %.4f（fix-N 应归入实现）", impl.USD, 0.50+0.30+0.08)
	}
	if impl.Runs != 3 {
		t.Errorf("实现桶执行次数 = %d，期望 3（implement + fix-1 + fix-2）", impl.Runs)
	}
}

// AC3：聚合按 user_id 隔离。agent_events 没有 user_id 列，
// 必须靠 JOIN tasks 过滤 —— 漏掉就是跨用户求和，P1.5 数据隔离的红线。
func TestCostStatsIsolatedByOwner(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	userA, taskA := costFixture(t, st)
	userB, taskB := costFixture(t, st)

	costEvent(t, st, taskA, "implement", 1.00, 10, 1000)
	costEvent(t, st, taskB, "implement", 99.00, 10, 1000)

	csA, err := st.CostStatsFor(ctx, userA)
	if err != nil {
		t.Fatalf("CostStatsFor(A) 失败: %v", err)
	}
	if diff := csA.TotalUSD - 1.00; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("A 的花费 = %.4f，期望 1.00（含了 B 的 99 就是跨用户求和）", csA.TotalUSD)
	}
	for _, tc := range csA.TopTasks {
		if tc.TaskID == taskB {
			t.Errorf("A 的排行里出现了 B 的任务 %d", taskB)
		}
	}

	csB, err := st.CostStatsFor(ctx, userB)
	if err != nil {
		t.Fatalf("CostStatsFor(B) 失败: %v", err)
	}
	if diff := csB.TotalUSD - 99.00; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("B 的花费 = %.4f，期望 99.00", csB.TotalUSD)
	}
}

// AC4：无成本数据时返回零值而非 null，集合都是空而不是 nil。
func TestCostStatsZeroValueWhenNoData(t *testing.T) {
	st := testStore(t)
	userID, _ := costFixture(t, st)

	cs, err := st.CostStatsFor(context.Background(), userID)
	if err != nil {
		t.Fatalf("CostStatsFor 失败: %v", err)
	}
	if cs.TotalUSD != 0 || cs.Runs != 0 {
		t.Errorf("无数据时应为零值，得到 %+v", cs)
	}
	if cs.ByPhase == nil {
		t.Error("ByPhase 应是空 map 而非 nil（否则 JSON 里是 null，前端要判空）")
	}
	if cs.ByDay == nil {
		t.Error("ByDay 应是空切片而非 nil")
	}
	if cs.TopTasks == nil {
		t.Error("TopTasks 应是空切片而非 nil")
	}
}

// 缺 costUsd 字段的 result 事件不该被计入 —— 少了 payload ? 'costUsd'
// 这条谓词，SUM 会忽略 NULL 但 COUNT 会虚高，执行次数就说谎了。
func TestCostStatsSkipsEventsWithoutCost(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, taskID := costFixture(t, st)

	costEvent(t, st, taskID, "implement", 0.25, 5, 1000)
	// 一条没有 costUsd 的 result 事件（早期数据形态）
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO agent_events (task_id, phase, kind, payload)
		VALUES ($1, 'implement', 'result', '{"subtype":"success"}'::jsonb)`, taskID); err != nil {
		t.Fatalf("插无成本事件失败: %v", err)
	}

	cs, err := st.CostStatsFor(ctx, userID)
	if err != nil {
		t.Fatalf("CostStatsFor 失败: %v", err)
	}
	if cs.Runs != 1 {
		t.Errorf("执行次数 = %d，期望 1（缺 costUsd 的事件不该计入）", cs.Runs)
	}
}

// 按日曲线按日期升序，且用 UTC 口径。
func TestCostStatsByDayOrderedAscending(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, taskID := costFixture(t, st)

	// 造三天的数据（今天、昨天、前天）
	for i, usd := range []float64{0.10, 0.20, 0.30} {
		if _, err := st.pool.Exec(ctx, `
			INSERT INTO agent_events (task_id, phase, kind, payload, at)
			VALUES ($1, 'implement', 'result',
			        jsonb_build_object('costUsd', $2::float8),
			        now() - make_interval(days => $3))`,
			taskID, usd, i); err != nil {
			t.Fatalf("插第 %d 天事件失败: %v", i, err)
		}
	}

	cs, err := st.CostStatsFor(ctx, userID)
	if err != nil {
		t.Fatalf("CostStatsFor 失败: %v", err)
	}
	if len(cs.ByDay) != 3 {
		t.Fatalf("应有 3 天数据，得到 %d：%+v", len(cs.ByDay), cs.ByDay)
	}
	for i := 1; i < len(cs.ByDay); i++ {
		if cs.ByDay[i-1].Day >= cs.ByDay[i].Day {
			t.Errorf("按日曲线应升序，%q 排在 %q 之前", cs.ByDay[i-1].Day, cs.ByDay[i].Day)
		}
	}
}
