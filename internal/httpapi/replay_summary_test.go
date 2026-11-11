package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
)

func idOf(v any) int64 {
	m, _ := v.(map[string]any)
	f, _ := m["id"].(float64)
	return int64(f)
}

// AC-4：summary 的各阶段计数、失败列表、firstFailure/lastFailure。
func TestTaskEventsSummary(t *testing.T) {
	api, _, m, repoID := apiFixture(t)
	srv := apiServer(t, api)

	t.Run("给定混合事件 断言 failures 恰为 5 条失败加 1 条警告且各阶段计数正确", func(t *testing.T) {
		tk := replayTask(t, api, m, repoID, "f04-ac4")
		impl := insertEvents(t, api, tk.ID, "implement", []agent.Entry{
			{Kind: agent.KindToolResult, Body: "e1", Payload: map[string]any{"isError": true}},
			{Kind: agent.KindToolResult, Body: "ok", Payload: map[string]any{"isError": false}},
			{Kind: agent.KindToolResult, Body: "e2", Payload: map[string]any{"isError": true}},
		})
		ver := insertEvents(t, api, tk.ID, "verify", []agent.Entry{
			{Kind: "verify_step", Body: "f", Payload: map[string]any{"status": "failed"}},
			{Kind: "verify_step", Body: "e", Payload: map[string]any{"status": "error"}},
			{Kind: "verify_step", Body: "p", Payload: map[string]any{"status": "passed"}},
			{Kind: "verify_step", Body: "s", Payload: map[string]any{"status": "skipped"}},
			{Kind: agent.KindResult, Body: "r", Payload: map[string]any{"subtype": "error_max_turns", "isError": false}},
			{Kind: agent.KindRaw, Body: "of", Payload: map[string]any{"type": "overflow"}},
		})
		body := getJSON(t, srv, "/api/tasks/"+itoa(tk.ID)+"/events/summary", http.StatusOK)

		if got := int(body["total"].(float64)); got != 9 {
			t.Errorf("total got %d, want 9", got)
		}
		if idOf(map[string]any{"id": body["firstId"]}) != impl[0] || idOf(map[string]any{"id": body["lastId"]}) != ver[5] {
			t.Errorf("firstId/lastId got %v/%v, want %d/%d", body["firstId"], body["lastId"], impl[0], ver[5])
		}
		wantFail := []int64{impl[0], impl[2], ver[0], ver[1], ver[4], ver[5]}
		var gotFail []int64
		var sev []string
		for _, f := range body["failures"].([]any) {
			gotFail = append(gotFail, idOf(f))
			sev = append(sev, f.(map[string]any)["severity"].(string))
		}
		if !reflect.DeepEqual(gotFail, wantFail) {
			t.Errorf("failures ids got %v, want %v", gotFail, wantFail)
		}
		if want := []string{"error", "error", "error", "error", "error", "warn"}; !reflect.DeepEqual(sev, want) {
			t.Errorf("severity got %v, want %v", sev, want)
		}
		if body["failuresTruncated"] != false {
			t.Errorf("failuresTruncated got %v, want false", body["failuresTruncated"])
		}
		if got := int(body["failureCount"].(float64)); got != 5 {
			t.Errorf("failureCount got %d, want 5", got)
		}
		if got := idOf(body["firstFailure"]); got != impl[0] {
			t.Errorf("firstFailure.id got %d, want %d", got, impl[0])
		}
		if got := idOf(body["lastFailure"]); got != ver[4] {
			t.Errorf("lastFailure.id got %d, want %d（overflow 警告 id 更大但不是失败）", got, ver[4])
		}

		type ph struct {
			count, failures int
			firstID, lastID int64
		}
		want := map[string]ph{
			"implement": {3, 2, impl[0], impl[2]},
			"verify":    {6, 3, ver[0], ver[5]},
		}
		phases := body["phases"].([]any)
		if len(phases) != 2 {
			t.Fatalf("phases got %d 项, want 2: %v", len(phases), phases)
		}
		for _, p := range phases {
			pm := p.(map[string]any)
			w, ok := want[pm["phase"].(string)]
			if !ok {
				t.Errorf("意外的 phase %v", pm["phase"])
				continue
			}
			got := ph{int(pm["count"].(float64)), int(pm["failures"].(float64)), int64(pm["firstId"].(float64)), int64(pm["lastId"].(float64))}
			if got != w {
				t.Errorf("phase %v got %+v, want %+v", pm["phase"], got, w)
			}
			if pm["firstAt"] == nil || pm["lastAt"] == nil {
				t.Errorf("phase %v 缺 firstAt/lastAt", pm["phase"])
			}
		}
	})

	t.Run("给定 205 条失败 断言 failures 截断到 200 而 lastFailure 是库里 error 的 max(id)", func(t *testing.T) {
		tk := replayTask(t, api, m, repoID, "f04-ac4t")
		var batch []agent.Entry
		for i := 0; i < 205; i++ {
			batch = append(batch,
				agent.Entry{Kind: agent.KindToolResult, Body: "bad", Payload: map[string]any{"isError": true}},
				agent.Entry{Kind: agent.KindText, Body: "filler"})
		}
		batch = append(batch, agent.Entry{Kind: agent.KindRaw, Body: "of", Payload: map[string]any{"type": "overflow"}})
		insertEvents(t, api, tk.ID, "implement", batch)

		body := getJSON(t, srv, "/api/tasks/"+itoa(tk.ID)+"/events/summary", http.StatusOK)
		fails := body["failures"].([]any)
		if len(fails) != 200 || body["failuresTruncated"] != true {
			t.Fatalf("failures got %d 条 truncated=%v, want 200 条 truncated=true", len(fails), body["failuresTruncated"])
		}
		if got := int(body["failureCount"].(float64)); got != 205 {
			t.Errorf("failureCount got %d, want 205", got)
		}
		var wantLast int64
		if err := api.Store.Pool().QueryRow(context.Background(),
			`SELECT max(id) FROM agent_events WHERE task_id=$1 AND kind='tool_result' AND payload->>'isError'='true'`, tk.ID).Scan(&wantLast); err != nil {
			t.Fatal(err)
		}
		gotLast := idOf(body["lastFailure"])
		if gotLast != wantLast {
			t.Errorf("lastFailure.id got %d, want %d", gotLast, wantLast)
		}
		if gotLast <= idOf(fails[199]) {
			t.Errorf("lastFailure.id=%d 应大于 failures[199].id=%d（不是截断数组的末项）", gotLast, idOf(fails[199]))
		}
	})

	t.Run("给定无失败的任务 断言 lastFailure/firstFailure 为 null", func(t *testing.T) {
		tk := replayTask(t, api, m, repoID, "f04-ac4n")
		insertEvents(t, api, tk.ID, "triage", texts(2, "n"))
		body := getJSON(t, srv, "/api/tasks/"+itoa(tk.ID)+"/events/summary", http.StatusOK)
		for _, k := range []string{"lastFailure", "firstFailure"} {
			if v, ok := body[k]; !ok || v != nil {
				t.Errorf("%s got %v (present=%v), want null", k, v, ok)
			}
		}
		if got := len(body["failures"].([]any)); got != 0 {
			t.Errorf("failures got %d, want 0", got)
		}
	})

	t.Run("给定非属主 断言 404 任务不存在", func(t *testing.T) {
		tk := replayTask(t, api, m, repoID, "f04-ac4o")
		userB := mustUser(t, api.Store, "f04-ac4-b-"+rhex(t)+"@example.com")
		srvB := apiServer(t, &API{Store: api.Store, Tasks: m, Queue: &fakeEnqueuer{}, Auth: authAs(userB, "b@example.com")})
		resp := do(t, srvB, "GET", "/api/tasks/"+itoa(tk.ID)+"/events/summary", "", true)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("got %d, want 404", resp.StatusCode)
		}
		if b := decode(t, resp); b["error"] != "任务不存在" {
			t.Errorf("body got %v, want {error:任务不存在}", b)
		}
	})
}

// AC-6 的 Go 侧：同一张失败判定表（web/src/testdata/failure-cases.json）
// 驱动服务端的 SQL 判定，node 测试用同一份文件驱动 replay.js 的 isFailure ——
// 前后端失败判定只有一份真相。
func TestFailurePredicateMatchesSharedFixture(t *testing.T) {
	raw, err := os.ReadFile("../../web/src/testdata/failure-cases.json")
	if err != nil {
		t.Fatalf("读共享 fixture 失败: %v", err)
	}
	var cases []struct {
		Name     string         `json:"name"`
		Kind     string         `json:"kind"`
		Payload  map[string]any `json:"payload"`
		Severity *string        `json:"severity"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("解析 fixture 失败: %v", err)
	}
	api, _, m, repoID := apiFixture(t)
	srv := apiServer(t, api)
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			tk := replayTask(t, api, m, repoID, "f04-ac6")
			ids := insertEvents(t, api, tk.ID, "implement", []agent.Entry{{Kind: c.Kind, Body: "x", Payload: c.Payload}})
			body := getJSON(t, srv, "/api/tasks/"+itoa(tk.ID)+"/events/summary", http.StatusOK)
			fails := body["failures"].([]any)
			var got *string
			if len(fails) == 1 && idOf(fails[0]) == ids[0] {
				s := fails[0].(map[string]any)["severity"].(string)
				got = &s
			}
			if (got == nil) != (c.Severity == nil) || (got != nil && *got != *c.Severity) {
				t.Errorf("severity got %v, want %v", deref(got), deref(c.Severity))
			}
			// failures=1 与 summary 同一口径
			ev := getJSON(t, srv, "/api/tasks/"+itoa(tk.ID)+"/events?after=0&failures=1", http.StatusOK)
			if n := len(eventIDs(t, ev)); (n == 1) != (c.Severity != nil) {
				t.Errorf("failures=1 返回 %d 条, want 命中=%v", n, c.Severity != nil)
			}
		})
	}
}

func deref(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
