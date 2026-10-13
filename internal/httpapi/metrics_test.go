package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// /api/stats/metrics 返回环节时长与失败分类，且按登录用户隔离
// （docs/05-roadmap.md §3.6 第 15 条）。
func TestAPIMetricsStats(t *testing.T) {
	api, st, m, repoID := apiFixture(t)
	srv := apiServer(t, api)
	ctx := context.Background()

	var userID int64
	if err := st.Pool().QueryRow(ctx, `SELECT user_id FROM repos WHERE id=$1`, repoID).Scan(&userID); err != nil {
		t.Fatalf("查属主失败: %v", err)
	}
	tk, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: "CR-MET-1",
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}

	// 造一条完整链路：queue 30 秒 → 分诊 10 秒 → 实现 20 秒 → 验证 5 秒。
	// at 显式指定，让分位数有确定的期望值。
	base := time.Now().Add(-time.Hour)
	for _, e := range []struct {
		to string
		ms int64
	}{{"queued", 0}, {"triaging", 30_000}, {"implementing", 40_000}, {"verifying", 60_000}, {"pr_open", 65_000}} {
		if _, err := st.Pool().Exec(ctx, `
			INSERT INTO task_events (task_id, to_state, actor, at) VALUES ($1,$2,'system',$3)`,
			tk.ID, e.to, base.Add(time.Duration(e.ms)*time.Millisecond)); err != nil {
			t.Fatalf("插事件 %s 失败: %v", e.to, err)
		}
	}

	resp := do(t, srv, "GET", "/api/stats/metrics", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	body := decode(t, resp)

	segs, ok := body["segments"].([]any)
	if !ok {
		t.Fatalf("segments 缺失或类型不对: %#v", body["segments"])
	}
	if len(segs) != 5 {
		t.Fatalf("环节数 = %d，期望 5（queue/triage/implement/verify/total）", len(segs))
	}

	byKey := map[string]map[string]any{}
	for _, s := range segs {
		item, _ := s.(map[string]any)
		key, _ := item["key"].(string)
		byKey[key] = item
	}
	// queue 段 = 30000ms
	if q := byKey["queue"]; q == nil {
		t.Error("缺少 queue 环节")
	} else if v, _ := q["p50Ms"].(float64); v != 30_000 {
		t.Errorf("queue p50 = %v，期望 30000", v)
	}
	// total 段 = 65000ms
	if tot := byKey["total"]; tot == nil {
		t.Error("缺少 total 环节")
	} else if v, _ := tot["p50Ms"].(float64); v != 65_000 {
		t.Errorf("total p50 = %v，期望 65000", v)
	}

	// 失败分类：这个任务没失败，桶列表应为空数组而非 null
	if body["failureStages"] == nil {
		t.Error("failureStages 不该是 null")
	}
	if v, ok := body["failed"].(float64); !ok || v != 0 {
		t.Errorf("failed 应为 0，得到 %#v", body["failed"])
	}
}

// 失败分类必须带上中文标签 —— 措辞是 runner 包的知识，不该让前端或
// store 各存一份（复制一份就会随流水线演进漏改）。
func TestAPIMetricsFailureStageLabels(t *testing.T) {
	api, st, m, repoID := apiFixture(t)
	srv := apiServer(t, api)
	ctx := context.Background()

	var userID int64
	if err := st.Pool().QueryRow(ctx, `SELECT user_id FROM repos WHERE id=$1`, repoID).Scan(&userID); err != nil {
		t.Fatalf("查属主失败: %v", err)
	}
	tk, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: "CR-MET-FAIL",
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	// 置成 failed 并写下阶段代码
	if _, err := st.Pool().Exec(ctx,
		`UPDATE tasks SET state='failed', failure_stage='verify_failed' WHERE id=$1`, tk.ID); err != nil {
		t.Fatalf("置 failed 失败: %v", err)
	}

	resp := do(t, srv, "GET", "/api/stats/metrics", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	body := decode(t, resp)

	stages, ok := body["failureStages"].([]any)
	if !ok || len(stages) == 0 {
		t.Fatalf("failureStages 应有内容: %#v", body["failureStages"])
	}
	first, _ := stages[0].(map[string]any)
	if first["stage"] != "verify_failed" {
		t.Errorf("阶段代码 = %#v，期望 verify_failed", first["stage"])
	}
	// 中文标签来自 runner.Stage 的标签方法
	label, _ := first["label"].(string)
	if label == "" || label == "verify_failed" {
		t.Errorf("应给出中文标签而非原样回显代码，得到 %#v", first["label"])
	}
}

// 没有任何数据的账号拿到空集合，不是 null 也不是 500。
func TestAPIMetricsEmptyAccount(t *testing.T) {
	api, _, _, _ := apiFixture(t)
	srv := apiServer(t, api)

	resp := do(t, srv, "GET", "/api/stats/metrics", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	body := decode(t, resp)
	if body["segments"] == nil || body["failureStages"] == nil {
		t.Errorf("空账号的集合字段不该是 null: %#v", body)
	}
	if segs, ok := body["segments"].([]any); !ok || len(segs) != 0 {
		t.Errorf("空账号的 segments 应为空数组，得到 %#v", body["segments"])
	}
}

// ★ 多租户隔离：统计只含当前用户的数据。
//
// 两个用户各造一条链路，A 的响应里绝不能出现 B 的样本。这是 P1.5 数据
// 隔离在度量端点上的红线（task_events 没有 user_id，靠 JOIN tasks 过滤）。
//
// 写法沿用 TestAPIIsolationBetweenUsers：同一台服务、同一份数据，换的
// 只是「谁登录着」（authAs）。
func TestAPIMetricsIsolatesUsers(t *testing.T) {
	st := testStoreForAPI(t)
	ctx := context.Background()

	userA := mustUser(t, st, "metrics-iso-a-"+t.Name()+"@example.com")
	userB := mustUser(t, st, "metrics-iso-b-"+t.Name()+"@example.com")

	m := task.NewMachine(st.Pool())
	base := time.Now().Add(-time.Hour)

	// A：queue 段 60 秒
	var repoA int64
	if err := st.Pool().QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		userA, "acme/metrics-iso-a").Scan(&repoA); err != nil {
		t.Fatalf("建 A 仓库失败: %v", err)
	}
	tkA, err := m.Create(ctx, task.CreateParams{UserID: userA, RepoID: repoA, ExternalKey: "MET-ISO-A"})
	if err != nil {
		t.Fatalf("建 A 任务失败: %v", err)
	}
	// B：queue 段 1 秒（明显不同），且任务更多 —— 隔离若失效，A 的分位数
	// 会被 B 拉低，一眼可辨。
	var repoB int64
	if err := st.Pool().QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		userB, "acme/metrics-iso-b").Scan(&repoB); err != nil {
		t.Fatalf("建 B 仓库失败: %v", err)
	}

	insertMark := func(taskID int64, to string, ms int64) {
		t.Helper()
		if _, err := st.Pool().Exec(ctx, `
			INSERT INTO task_events (task_id, to_state, actor, at) VALUES ($1,$2,'system',$3)`,
			taskID, to, base.Add(time.Duration(ms)*time.Millisecond)); err != nil {
			t.Fatalf("插事件 %s 失败: %v", to, err)
		}
	}
	insertMark(tkA.ID, "queued", 0)
	insertMark(tkA.ID, "triaging", 60_000)

	for i := 0; i < 3; i++ {
		tkB, err := m.Create(ctx, task.CreateParams{
			UserID: userB, RepoID: repoB, ExternalKey: "MET-ISO-B" + itoa(int64(i)),
		})
		if err != nil {
			t.Fatalf("建 B 任务失败: %v", err)
		}
		insertMark(tkB.ID, "queued", 0)
		insertMark(tkB.ID, "triaging", 1_000)
	}

	queueP50Of := func(srv *httptest.Server) (float64, int) {
		t.Helper()
		resp := do(t, srv, "GET", "/api/stats/metrics", "", true)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
		}
		segs, _ := decode(t, resp)["segments"].([]any)
		for _, s := range segs {
			item, _ := s.(map[string]any)
			if item["key"] == "queue" {
				v, _ := item["p50Ms"].(float64)
				n, _ := item["count"].(float64)
				return v, int(n)
			}
		}
		t.Fatal("响应缺少 queue 环节")
		return 0, 0
	}

	// A 看到自己那一条：p50 = 60000，样本数 1
	srvA := apiServer(t, &API{
		Store: st, Tasks: m, Queue: &fakeEnqueuer{}, Auth: authAs(userA, "ma@example.com"),
	})
	p50A, nA := queueP50Of(srvA)
	if p50A != 60_000 || nA != 1 {
		t.Errorf("A 看到的 queue = p50 %v / 样本 %d，期望 60000 / 1 —— 对不上说明 B 的样本漏进来了",
			p50A, nA)
	}

	// B 看到自己那三条：p50 = 1000，样本数 3
	srvB := apiServer(t, &API{
		Store: st, Tasks: m, Queue: &fakeEnqueuer{}, Auth: authAs(userB, "mb@example.com"),
	})
	p50B, nB := queueP50Of(srvB)
	if p50B != 1_000 || nB != 3 {
		t.Errorf("B 看到的 queue = p50 %v / 样本 %d，期望 1000 / 3", p50B, nB)
	}
}
