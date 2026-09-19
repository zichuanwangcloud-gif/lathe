package httpapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/Clouditera/lathe/internal/task"
)

// /api/stats/cost 返回聚合，且按登录用户隔离。
func TestAPICostStats(t *testing.T) {
	api, st, m, repoID := apiFixture(t)
	srv := apiServer(t, api)
	ctx := context.Background()

	var userID int64
	if err := st.Pool().QueryRow(ctx, `SELECT user_id FROM repos WHERE id=$1`, repoID).Scan(&userID); err != nil {
		t.Fatalf("查属主失败: %v", err)
	}
	tk, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: "CR-COST-1",
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	// 分诊 + 实现 + 一轮 fix
	for _, e := range []struct {
		phase string
		usd   float64
	}{{"triage", 0.01}, {"implement", 0.40}, {"fix-1", 0.09}} {
		if _, err := st.Pool().Exec(ctx, `
			INSERT INTO agent_events (task_id, phase, kind, payload)
			VALUES ($1, $2, 'result', jsonb_build_object('costUsd', $3::float8))`,
			tk.ID, e.phase, e.usd); err != nil {
			t.Fatalf("插事件失败: %v", err)
		}
	}

	resp := do(t, srv, "GET", "/api/stats/cost", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	body := decode(t, resp)

	total, ok := body["totalUsd"].(float64)
	if !ok {
		t.Fatalf("totalUsd 缺失或类型不对: %#v", body["totalUsd"])
	}
	if diff := total - 0.50; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("totalUsd = %v，期望 0.50", total)
	}

	// AC4：集合字段必须是 {} / [] 而不是 null，前端不必到处判空
	if body["byPhase"] == nil {
		t.Error("byPhase 不该是 null")
	}
	if body["byDay"] == nil {
		t.Error("byDay 不该是 null")
	}
	if body["topTasks"] == nil {
		t.Error("topTasks 不该是 null")
	}
}

// 没有任何成本数据的账号拿到的是零值，不是 null，也不是 500。
func TestAPICostStatsEmptyAccount(t *testing.T) {
	api, _, _, _ := apiFixture(t)
	srv := apiServer(t, api)

	resp := do(t, srv, "GET", "/api/stats/cost", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.StatusCode)
	}
	body := decode(t, resp)
	if v, ok := body["totalUsd"].(float64); !ok || v != 0 {
		t.Errorf("空账号的 totalUsd 应为 0，得到 %#v", body["totalUsd"])
	}
	if body["byPhase"] == nil || body["byDay"] == nil || body["topTasks"] == nil {
		t.Errorf("空账号的集合字段也不该是 null: %#v", body)
	}
}
