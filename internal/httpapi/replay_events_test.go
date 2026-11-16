package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
	"github.com/zichuanwangcloud-gif/lathe/internal/runner"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// f04 回放：/events 扩展参数、summary、日志读取的共用夹具。

// rhex 给 fixture 标识加随机量（AGENTS.md §5.5：-p 1 挡不住跨轮次残留）。
func rhex(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("取随机数失败: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// replayTask 在 apiFixture 的属主名下建一个带随机 external_key 的任务，
// 测试结束按自己的 task_id 删除（事件、验证行随 ON DELETE CASCADE 一起删）。
func replayTask(t *testing.T, api *API, m *task.Machine, repoID int64, prefix string) *task.Task {
	t.Helper()
	ctx := context.Background()
	userID := mustUserOfRepo(t, api, repoID)
	tk, err := m.Create(ctx, task.CreateParams{UserID: userID, RepoID: repoID, ExternalKey: prefix + "-" + rhex(t)})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = api.Store.Pool().Exec(context.Background(), `DELETE FROM tasks WHERE id=$1`, tk.ID)
	})
	return tk
}

// AC-1：字面量与常量的绑定。store 包的测试只能写 "implement-reentry" 字面量
// （import cycle），这里用 runner 的常量本身插入并按 phase 过滤读回 ——
// 常量哪天改名或改值，这条与 /events 的 phase 白名单会一起报错。
func TestEventsPhaseFilterAcceptsRunnerReentryPhase(t *testing.T) {
	api, st, m, repoID := apiFixture(t)
	srv := apiServer(t, api)
	ctx := context.Background()
	tk := replayTask(t, api, m, repoID, "f04-ac1")

	if err := st.InsertAgentEvents(ctx, tk.ID, runner.PhaseImplementReentry, []agent.Entry{{Kind: "text", Body: "重入会话的第一句"}}); err != nil {
		t.Fatalf("以 runner.PhaseImplementReentry 落事件失败: got %v, want nil", err)
	}
	if err := st.InsertAgentEvents(ctx, tk.ID, "implement", []agent.Entry{{Kind: "text", Body: "原会话"}}); err != nil {
		t.Fatalf("落 implement 事件失败: %v", err)
	}

	resp := do(t, srv, "GET", "/api/tasks/"+itoa(tk.ID)+"/events?after=0&phase="+runner.PhaseImplementReentry, "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("phase=%s 状态码 got %d, want 200", runner.PhaseImplementReentry, resp.StatusCode)
	}
	body := decode(t, resp)
	events, _ := body["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("应只读回重入阶段的 1 条: got %d 条 %v", len(events), events)
	}
	if ev := events[0].(map[string]any); ev["phase"] != runner.PhaseImplementReentry || ev["body"] != "重入会话的第一句" {
		t.Errorf("读回不符: got %v, want phase=%s", ev, runner.PhaseImplementReentry)
	}
}

// insertEvents 落一批事件并按插入顺序返回它们的 id。
func insertEvents(t *testing.T, api *API, taskID int64, phase string, entries []agent.Entry) []int64 {
	t.Helper()
	ctx := context.Background()
	var before int64
	_ = api.Store.Pool().QueryRow(ctx, `SELECT COALESCE(max(id),0) FROM agent_events WHERE task_id=$1`, taskID).Scan(&before)
	if err := api.Store.InsertAgentEvents(ctx, taskID, phase, entries); err != nil {
		t.Fatalf("落事件失败: %v", err)
	}
	rows, err := api.Store.Pool().Query(ctx, `SELECT id FROM agent_events WHERE task_id=$1 AND id>$2 ORDER BY id`, taskID, before)
	if err != nil {
		t.Fatalf("取 id 失败: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	if len(ids) != len(entries) {
		t.Fatalf("落库条数 got %d, want %d", len(ids), len(entries))
	}
	return ids
}

func texts(n int, prefix string) []agent.Entry {
	out := make([]agent.Entry, n)
	for i := range out {
		out[i] = agent.Entry{Kind: agent.KindText, Body: prefix + itoa(int64(i))}
	}
	return out
}

func eventIDs(t *testing.T, body map[string]any) []int64 {
	t.Helper()
	evs, _ := body["events"].([]any)
	out := make([]int64, 0, len(evs))
	for _, e := range evs {
		out = append(out, int64(e.(map[string]any)["id"].(float64)))
	}
	return out
}

func keySet(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func getJSON(t *testing.T, srv *httptest.Server, path string, wantStatus int) map[string]any {
	t.Helper()
	resp := do(t, srv, "GET", path, "", true)
	if resp.StatusCode != wantStatus {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s 状态码 got %d, want %d；body=%s", path, resp.StatusCode, wantStatus, b)
	}
	return decode(t, resp)
}

// AC-2：反向分页。250 条，before 从尾部往前翻，并集恰为全部、无重复无遗漏。
func TestTaskEventsBeforePaging(t *testing.T) {
	api, _, m, repoID := apiFixture(t)
	srv := apiServer(t, api)
	tk := replayTask(t, api, m, repoID, "f04-ac2")
	ids := insertEvents(t, api, tk.ID, "implement", texts(250, "e"))
	base := "/api/tasks/" + itoa(tk.ID) + "/events"

	t.Run("给定 before=lastId+1&limit=100 断言返回 id 最大的 100 条且升序", func(t *testing.T) {
		body := getJSON(t, srv, base+"?before="+itoa(ids[249]+1)+"&limit=100", http.StatusOK)
		got := eventIDs(t, body)
		if len(got) != 100 || got[0] != ids[150] || got[99] != ids[249] {
			t.Fatalf("got %d 条 [%v..%v], want 100 条 [%d..%d]", len(got), first(got), last(got), ids[150], ids[249])
		}
		if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i] < got[j] }) {
			t.Errorf("应按 id 升序: got %v", got)
		}
		if body["has_more"] != true {
			t.Errorf("has_more got %v, want true", body["has_more"])
		}
		if int64(body["first_id"].(float64)) != ids[150] || int64(body["last_id"].(float64)) != ids[249] {
			t.Errorf("first_id/last_id got %v/%v, want %d/%d", body["first_id"], body["last_id"], ids[150], ids[249])
		}
	})

	t.Run("给定沿 first_id 连续翻页 断言并集等于全部 250 条且末页 has_more=false", func(t *testing.T) {
		seen := map[int64]bool{}
		var all []int64
		cursor := ids[249] + 1
		pages := 0
		for {
			body := getJSON(t, srv, base+"?before="+itoa(cursor)+"&limit=100", http.StatusOK)
			got := eventIDs(t, body)
			for _, id := range got {
				if seen[id] {
					t.Fatalf("id %d 重复出现", id)
				}
				seen[id] = true
			}
			all = append(got, all...)
			pages++
			if body["has_more"] != true {
				break
			}
			if pages > 5 {
				t.Fatal("翻页不收敛")
			}
			cursor = int64(body["first_id"].(float64))
		}
		if pages != 3 {
			t.Errorf("页数 got %d, want 3", pages)
		}
		if len(all) != 250 || all[0] != ids[0] || all[249] != ids[249] {
			t.Errorf("并集 got %d 条, want 250 条且首尾与库一致", len(all))
		}
	})

	t.Run("给定 before 与 after 同时传 断言 400", func(t *testing.T) {
		getJSON(t, srv, base+"?before=10&after=1", http.StatusBadRequest)
	})
	t.Run("给定 before=abc 断言 400", func(t *testing.T) {
		getJSON(t, srv, base+"?before=abc", http.StatusBadRequest)
	})
}

func first(s []int64) any {
	if len(s) == 0 {
		return nil
	}
	return s[0]
}

func last(s []int64) any {
	if len(s) == 0 {
		return nil
	}
	return s[len(s)-1]
}

// AC-2：兼容契约 —— 不带新参数时响应与旧版逐字段一致、顶层键恰为 {events, last_id}
// （与 f01 AC-17 在 TestTaskEventsEndpoint 里的断言同一条契约；本线不改那个函数）。
func TestTaskEventsLegacyShapeUnchanged(t *testing.T) {
	api, _, m, repoID := apiFixture(t)
	srv := apiServer(t, api)
	tk := replayTask(t, api, m, repoID, "f04-ac2l")
	ids := insertEvents(t, api, tk.ID, "implement", texts(3, "l"))
	base := "/api/tasks/" + itoa(tk.ID) + "/events"
	lastID := ids[2]

	legacy := []struct {
		name, query string
		wantIDs     []int64
		wantLast    int64
	}{
		{"给定 after=0 断言全部 3 条", "?after=0", ids, lastID},
		{"给定 after=last_id 断言为空且游标不动", "?after=" + itoa(lastID), nil, lastID},
		{"给定 after=0&limit=1 断言只第 1 条", "?after=0&limit=1", ids[:1], ids[0]},
		{"给定无查询串 断言全部 3 条", "", ids, lastID},
	}
	for _, c := range legacy {
		t.Run(c.name+"且顶层键恰为 {events,last_id}", func(t *testing.T) {
			body := getJSON(t, srv, base+c.query, http.StatusOK)
			if got, want := keySet(body), []string{"events", "last_id"}; !reflect.DeepEqual(got, want) {
				t.Errorf("顶层键 got %v, want %v", got, want)
			}
			if got := eventIDs(t, body); !reflect.DeepEqual(got, append([]int64{}, c.wantIDs...)) {
				t.Errorf("events ids got %v, want %v", got, c.wantIDs)
			}
			if got := int64(body["last_id"].(float64)); got != c.wantLast {
				t.Errorf("last_id got %d, want %d", got, c.wantLast)
			}
		})
	}

	t.Run("给定 after=0&paged=1 断言 events/last_id 不变且多 first_id/has_more", func(t *testing.T) {
		plain := getJSON(t, srv, base+"?after=0", http.StatusOK)
		paged := getJSON(t, srv, base+"?after=0&paged=1", http.StatusOK)
		if got, want := keySet(paged), []string{"events", "first_id", "has_more", "last_id"}; !reflect.DeepEqual(got, want) {
			t.Errorf("顶层键 got %v, want %v", got, want)
		}
		if !reflect.DeepEqual(plain["events"], paged["events"]) || plain["last_id"] != paged["last_id"] {
			t.Errorf("paged=1 改变了 events/last_id: got %v / %v, want %v / %v", paged["events"], paged["last_id"], plain["events"], plain["last_id"])
		}
		if int64(paged["first_id"].(float64)) != ids[0] {
			t.Errorf("first_id got %v, want %d", paged["first_id"], ids[0])
		}
	})
	t.Run("给定 after=last_id&paged=1 断言 first_id=null 且 has_more=false", func(t *testing.T) {
		body := getJSON(t, srv, base+"?after="+itoa(lastID)+"&paged=1", http.StatusOK)
		if v, ok := body["first_id"]; !ok || v != nil {
			t.Errorf("first_id got %v (present=%v), want null", v, ok)
		}
		if body["has_more"] != false {
			t.Errorf("has_more got %v, want false", body["has_more"])
		}
	})
	for _, q := range []string{"?paged=2", "?failures=yes"} {
		t.Run("给定 "+q+" 断言 400", func(t *testing.T) {
			getJSON(t, srv, base+q, http.StatusBadRequest)
		})
	}
}

// AC-2：failures=1 —— 以当前失败 id 为游标逐个走（不受 summary 200 截断约束）。
func TestTaskEventsFailuresParam(t *testing.T) {
	api, _, m, repoID := apiFixture(t)
	srv := apiServer(t, api)
	tk := replayTask(t, api, m, repoID, "f04-ac2f")
	ids := insertEvents(t, api, tk.ID, "implement", []agent.Entry{
		{Kind: agent.KindText, Body: "ok"},
		{Kind: agent.KindToolResult, Body: "boom1", Payload: map[string]any{"isError": true}}, // 1
		{Kind: agent.KindToolResult, Body: "fine", Payload: map[string]any{"isError": false}},
		{Kind: "verify_step", Body: "✗ build", Payload: map[string]any{"status": "failed"}}, // 3
		{Kind: "verify_step", Body: "✓ lint", Payload: map[string]any{"status": "passed"}},
		{Kind: agent.KindResult, Body: "max turns", Payload: map[string]any{"subtype": "error_max_turns", "isError": false}}, // 5
		{Kind: agent.KindResult, Body: "done", Payload: map[string]any{"subtype": "success", "isError": false}},
		{Kind: agent.KindRaw, Body: "溢出", Payload: map[string]any{"type": "overflow", "dropped": 3}}, // 7 警告
	})
	base := "/api/tasks/" + itoa(tk.ID) + "/events"
	want := []int64{ids[1], ids[3], ids[5], ids[7]}

	t.Run("给定 after 游标 + failures=1&limit=1 断言依次得到 3 条失败、1 条警告、然后为空", func(t *testing.T) {
		cursor := int64(0)
		for i := 0; i <= len(want); i++ {
			body := getJSON(t, srv, base+"?after="+itoa(cursor)+"&paged=1&failures=1&limit=1", http.StatusOK)
			got := eventIDs(t, body)
			if i == len(want) {
				if len(got) != 0 {
					t.Errorf("第 %d 次应为空: got %v", i+1, got)
				}
				break
			}
			if len(got) != 1 || got[0] != want[i] {
				t.Fatalf("第 %d 次 got %v, want [%d]", i+1, got, want[i])
			}
			cursor = got[0]
		}
	})
	t.Run("给定 before=第 3 条失败 id&failures=1&limit=1 断言返回第 2 条失败", func(t *testing.T) {
		body := getJSON(t, srv, base+"?before="+itoa(ids[5])+"&failures=1&limit=1", http.StatusOK)
		if got := eventIDs(t, body); len(got) != 1 || got[0] != ids[3] {
			t.Errorf("got %v, want [%d]", got, ids[3])
		}
	})
}

// AC-3：phase/kind 过滤取交集；非白名单值 400 且错误信息带参数名。
func TestTaskEventsPhaseKindFilter(t *testing.T) {
	api, _, m, repoID := apiFixture(t)
	srv := apiServer(t, api)
	tk := replayTask(t, api, m, repoID, "f04-ac3")
	mix := []agent.Entry{
		{Kind: agent.KindText, Body: "t"},
		{Kind: agent.KindThinking, Body: "th"},
		{Kind: agent.KindToolUse, Tool: "Bash", Body: "Bash ls"},
		{Kind: agent.KindToolResult, Body: "r"},
	}
	impl := insertEvents(t, api, tk.ID, "implement", mix)
	fix := insertEvents(t, api, tk.ID, "fix-1", mix)
	ver := insertEvents(t, api, tk.ID, "verify", mix)
	base := "/api/tasks/" + itoa(tk.ID) + "/events?after=0"

	cases := []struct {
		name, query string
		want        []int64
	}{
		{"给定 phase=verify 断言只返回 verify 的 4 条", "&phase=verify", ver},
		{"给定 phase=implement,fix-1&kind=tool_use 断言取交集", "&phase=implement,fix-1&kind=tool_use", []int64{impl[2], fix[2]}},
		{"给定 kind=text,thinking 断言跨阶段 6 条", "&kind=text,thinking", []int64{impl[0], impl[1], fix[0], fix[1], ver[0], ver[1]}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := getJSON(t, srv, base+c.query, http.StatusOK)
			if got := eventIDs(t, body); !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
	for _, c := range []struct{ query, param string }{
		{"&phase=bogus", "phase"},
		{"&phase=fix-", "phase"},
		{"&kind=bogus", "kind"},
	} {
		t.Run("给定 "+c.query+" 断言 400 且错误信息含参数名 "+c.param, func(t *testing.T) {
			body := getJSON(t, srv, base+c.query, http.StatusBadRequest)
			if msg, _ := body["error"].(string); !strings.Contains(msg, c.param) {
				t.Errorf("error got %q, want 含 %q", msg, c.param)
			}
		})
	}
}

// AC-3：扩展参数不绕过归属判定（回归守卫）。
func TestTaskEventsOtherUserGets404(t *testing.T) {
	api, st, m, repoID := apiFixture(t)
	tk := replayTask(t, api, m, repoID, "f04-ac3o")
	insertEvents(t, api, tk.ID, "verify", texts(2, "x"))

	userB := mustUser(t, st, "f04-ac3-b-"+rhex(t)+"@example.com")
	srvB := apiServer(t, &API{Store: st, Tasks: m, Queue: &fakeEnqueuer{}, Auth: authAs(userB, "b@example.com")})
	for _, q := range []string{"?before=999999999&phase=verify", "?after=0&paged=1&failures=1", ""} {
		t.Run("给定非属主请求 events"+q+" 断言 404 任务不存在", func(t *testing.T) {
			resp := do(t, srvB, "GET", "/api/tasks/"+itoa(tk.ID)+"/events"+q, "", true)
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusNotFound || strings.TrimSpace(string(b)) != `{"error":"任务不存在"}` {
				t.Errorf("got %d %s, want 404 {\"error\":\"任务不存在\"}", resp.StatusCode, b)
			}
		})
	}
}
