package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// f06 AC-12：仓库的执行 agent 选择与 /api/agent-drivers。
func TestUpdateRepoAgentDriver(t *testing.T) {
	cases := []struct {
		name         string
		codexEnabled bool
		body         string
		wantStatus   int
		wantContain  string // 错误文本须含（400 时）
		wantDriver   string // 之后 GET /api/repos 里这一项的 agentDriver
	}{
		{name: "给定 codex 已启用 选 codex 断言 200 且列表里是 codex", codexEnabled: true,
			body: `{"agentDriver":"codex"}`, wantStatus: http.StatusOK, wantDriver: "codex"},
		{name: "给定 codex 未启用 选 codex 断言 400 且提示 LATHE_CODEX_HOME", codexEnabled: false,
			body: `{"agentDriver":"codex"}`, wantStatus: http.StatusBadRequest, wantContain: "LATHE_CODEX_HOME", wantDriver: "claude"},
		{name: "给定未知驱动 gpt 断言 400", codexEnabled: true,
			body: `{"agentDriver":"gpt"}`, wantStatus: http.StatusBadRequest, wantContain: "claude / codex", wantDriver: "claude"},
		{name: "给定不传 agentDriver 断言原值不变", codexEnabled: true,
			body: `{"gateMode":"direct"}`, wantStatus: http.StatusOK, wantDriver: "claude"},
		{name: "给定显式选回 claude 断言 200", codexEnabled: false,
			body: `{"agentDriver":"claude"}`, wantStatus: http.StatusOK, wantDriver: "claude"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, _, _, repoID := apiFixture(t)
			api.CodexEnabled = tc.codexEnabled
			srv := apiServer(t, api)

			resp := do(t, srv, "PUT", "/api/repos/"+itoa(repoID), tc.body, true)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("状态码 got %d want %d（%v）", resp.StatusCode, tc.wantStatus, decode(t, resp))
			}
			if tc.wantContain != "" {
				if msg, _ := decode(t, resp)["error"].(string); !strings.Contains(msg, tc.wantContain) {
					t.Errorf("错误文本应含 %q，got %q", tc.wantContain, msg)
				}
			}
			list := decode(t, do(t, srv, "GET", "/api/repos", "", true))
			repos, _ := list["repos"].([]any)
			found := false
			for _, r := range repos {
				m, _ := r.(map[string]any)
				if id, _ := m["id"].(float64); int64(id) == repoID {
					found = true
					if m["agentDriver"] != tc.wantDriver {
						t.Errorf("agentDriver got %v want %s", m["agentDriver"], tc.wantDriver)
					}
				}
			}
			if !found {
				t.Fatalf("GET /api/repos 里没有仓库 %d", repoID)
			}
		})
	}

	t.Run("给定别人的仓库 断言 404 不是 403 且对方仓库不变", func(t *testing.T) {
		api, st, _, repoA := apiFixture(t)
		userB := mustUser(t, st, "f06-b-"+t.Name()+"-"+time.Now().Format("150405.000000")+"@example.com")
		apiB := &API{Store: st, Tasks: api.Tasks, Queue: &fakeEnqueuer{}, Auth: authAs(userB, "b@example.com"), CodexEnabled: true}
		srvB := apiServer(t, apiB)
		resp := do(t, srvB, "PUT", "/api/repos/"+itoa(repoA), `{"agentDriver":"codex"}`, true)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("改别人的仓库 got %d want 404", resp.StatusCode)
		}
		var driver string
		if err := st.Pool().QueryRow(context.Background(), `SELECT agent_driver FROM repos WHERE id=$1`, repoA).Scan(&driver); err != nil {
			t.Fatal(err)
		}
		if driver != "claude" {
			t.Errorf("A 的仓库不应被改动，agent_driver got %s", driver)
		}
	})

	t.Run("给定任务行 断言任务详情带 agentDriver（NULL 读成 claude）", func(t *testing.T) {
		api, st, m, repoID := apiFixture(t)
		srv := apiServer(t, api)
		ctx := context.Background()
		var userID int64
		if err := st.Pool().QueryRow(ctx, `SELECT user_id FROM repos WHERE id=$1`, repoID).Scan(&userID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool().Exec(ctx, `UPDATE repos SET agent_driver='codex' WHERE id=$1`, repoID); err != nil {
			t.Fatal(err)
		}
		tk, err := m.Create(ctx, task.CreateParams{UserID: userID, RepoID: repoID, ExternalKey: "F06-DETAIL-" + time.Now().Format("150405.000000")})
		if err != nil {
			t.Fatal(err)
		}
		detail := decode(t, do(t, srv, "GET", "/api/tasks/"+itoa(tk.ID), "", true))
		if got := detail["task"].(map[string]any)["agentDriver"]; got != "codex" {
			t.Errorf("任务详情 agentDriver got %v want codex", got)
		}
		if _, err := st.Pool().Exec(ctx, `UPDATE tasks SET agent_driver=NULL WHERE id=$1`, tk.ID); err != nil {
			t.Fatal(err)
		}
		detail = decode(t, do(t, srv, "GET", "/api/tasks/"+itoa(tk.ID), "", true))
		if got := detail["task"].(map[string]any)["agentDriver"]; got != "claude" {
			t.Errorf("NULL 的 agentDriver 应读成 claude，got %v", got)
		}
	})
}

func TestAgentDriversEndpoint(t *testing.T) {
	cases := []struct {
		name         string
		codexEnabled bool
		auth         bool
		wantStatus   int
		wantCodex    bool
	}{
		{name: "给定未登录 断言 401", auth: false, wantStatus: http.StatusUnauthorized},
		{name: "给定 codex 已启用 断言 codex.enabled=true", codexEnabled: true, auth: true, wantStatus: http.StatusOK, wantCodex: true},
		{name: "给定 codex 未启用 断言 codex.enabled=false", codexEnabled: false, auth: true, wantStatus: http.StatusOK, wantCodex: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, _, _, _ := apiFixture(t)
			api.CodexEnabled = tc.codexEnabled
			srv := apiServer(t, api)
			resp := do(t, srv, "GET", "/api/agent-drivers", "", tc.auth)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("状态码 got %d want %d", resp.StatusCode, tc.wantStatus)
			}
			if tc.wantStatus != http.StatusOK {
				return
			}
			body := decode(t, resp)
			drivers, _ := body["drivers"].([]any)
			if len(drivers) != 2 {
				t.Fatalf("drivers got %v want claude 与 codex 两项", body)
			}
			got := map[string]bool{}
			for _, d := range drivers {
				m := d.(map[string]any)
				if len(m) != 2 {
					t.Errorf("每项只应有 name/enabled 两个键，got %v", m)
				}
				for k, v := range m {
					if k != "name" && k != "enabled" {
						t.Errorf("不应出现键 %q", k)
					}
					if s, ok := v.(string); ok && strings.Contains(s, "/") {
						t.Errorf("不应含任何路径，got %q", s)
					}
				}
				got[m["name"].(string)] = m["enabled"].(bool)
			}
			if !got["claude"] || got["codex"] != tc.wantCodex {
				t.Errorf("enabled got %v want claude=true codex=%v", got, tc.wantCodex)
			}
		})
	}
}
