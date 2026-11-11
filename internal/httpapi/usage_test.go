package httpapi

// usage_test.go 用量统计接口（f03，docs/features/usage-stats.md §3.3）。
// 每条用例新建自己的用户（email 带随机量），断言按归属过滤（AGENTS.md §5.5）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// usageEnv 是一个用户 + 一个仓库 + 以该用户身份登录的测试服务器。
type usageEnv struct {
	st     *store.Store
	userID int64
	repoID int64
	srv    *httptest.Server
}

func newUsageUser(t *testing.T, st *store.Store, role string) *usageEnv {
	t.Helper()
	nonce := fmt.Sprint(time.Now().UnixNano())
	email := "usage-" + nonce + "@example.com"
	userID := mustUser(t, st, email)
	var repoID int64
	if err := st.Pool().QueryRow(context.Background(),
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		userID, "acme/usage-"+nonce).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	t.Cleanup(func() {
		// issues → repos 是 RESTRICT：先删工单，再让 users 级联
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM issues WHERE user_id=$1`, userID)
	})
	a := NewAuth(apiTestToken)
	a.Bootstrap = &store.User{ID: userID, Email: email, Role: role}
	api := &API{Store: st, Tasks: task.NewMachine(st.Pool()), Queue: &fakeEnqueuer{}, Auth: a}
	return &usageEnv{st: st, userID: userID, repoID: repoID, srv: apiServer(t, api)}
}

func (e *usageEnv) task(t *testing.T, key, provider string) int64 {
	t.Helper()
	var id int64
	if err := e.st.Pool().QueryRow(context.Background(),
		`INSERT INTO tasks (user_id, repo_id, external_key, tracker_provider) VALUES ($1,$2,$3,$4) RETURNING id`,
		e.userID, e.repoID, key, provider).Scan(&id); err != nil {
		t.Fatalf("建 task 失败: %v", err)
	}
	return id
}

func (e *usageEnv) prd(t *testing.T, document string) int64 {
	t.Helper()
	p, err := e.st.CreatePRD(context.Background(), store.CreatePRDParams{
		UserID: e.userID, RepoID: e.repoID, PRDType: "feature", OriginalInput: "用量夹具"})
	if err != nil {
		t.Fatal(err)
	}
	if document != "" {
		if _, err := e.st.Pool().Exec(context.Background(), `UPDATE prds SET document=$2::jsonb WHERE id=$1`, p.ID, document); err != nil {
			t.Fatal(err)
		}
	}
	return p.ID
}

func (e *usageEnv) run(t *testing.T, r agent.RunRecord) {
	t.Helper()
	if r.EndedAt.IsZero() {
		r.EndedAt = r.StartedAt.Add(time.Second)
	}
	if err := e.st.RecordAgentRun(context.Background(), r); err != nil {
		t.Fatalf("RecordAgentRun: %v", err)
	}
}

func (e *usageEnv) get(t *testing.T, path string) (int, string) {
	t.Helper()
	resp := do(t, e.srv, "GET", path, "", true)
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func paidRes(usd float64) *agent.Result {
	return &agent.Result{Success: true, Subtype: "success", CostUSD: usd, CostKnown: true}
}

func asMap(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("响应不是 JSON: %v\n%s", err, body)
	}
	return m
}

func items(t *testing.T, m map[string]any, key string) []map[string]any {
	t.Helper()
	raw, ok := m[key].([]any)
	if !ok {
		t.Fatalf("%s got %#v, want 数组", key, m[key])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		out = append(out, it.(map[string]any))
	}
	return out
}

func byKey(list []map[string]any, key string) map[string]any {
	for _, it := range list {
		if it["key"] == key {
			return it
		}
	}
	return nil
}

// AC-7 ③：group=task 的标签只来自库内写死的来源；admin 端点结构上不带任何标签。
func TestUsageGroupTaskHTTP(t *testing.T) {
	st := testStoreForAPI(t)
	ctx := context.Background()
	a := newUsageUser(t, st, store.RoleMember)
	b := newUsageUser(t, st, store.RoleMember)
	r := fmt.Sprintf("r%d", time.Now().UnixNano())

	issueB, err := st.CreateIssue(ctx, store.CreateIssueParams{UserID: b.userID, RepoID: b.repoID, Title: "B的工单-" + r})
	if err != nil {
		t.Fatal(err)
	}
	issueA, err := st.CreateIssue(ctx, store.CreateIssueParams{UserID: a.userID, RepoID: a.repoID, Title: "内置标题-" + r})
	if err != nil {
		t.Fatal(err)
	}
	tl := a.task(t, "ENG-"+r, "linear")
	ti := a.task(t, issueA.Key, "internal")
	tx := a.task(t, issueB.Key, "linear") // external_key 故意等于 B 的工单号
	p1 := a.prd(t, `{"oneLiner":{"body":"  一句话-`+r+`  "}}`)
	p2 := a.prd(t, "")
	p3 := a.prd(t, `{"oneLiner":{"body":"`+strings.Repeat("长", 81)+`"}}`)

	start := time.Now().Add(-time.Hour)
	for i, id := range []int64{tl, ti, tx} {
		a.run(t, agent.RunRecord{Kind: "task", TaskID: id, Phase: "implement", StartedAt: start.Add(time.Duration(i) * time.Second), Result: paidRes(0.1)})
	}
	for i, id := range []int64{p1, p2, p3} {
		a.run(t, agent.RunRecord{Kind: "prd", PRDID: id, Phase: "plan", StartedAt: start.Add(time.Duration(10+i) * time.Second), Result: paidRes(0.1)})
	}

	code, body := a.get(t, "/api/usage?group=task&limit=200")
	if code != http.StatusOK {
		t.Fatalf("状态码 got %d, want 200: %s", code, body)
	}
	list := items(t, asMap(t, body), "byGroup")
	str := func(v any) string {
		if v == nil {
			return "<null>"
		}
		return fmt.Sprint(v)
	}
	cases := []struct {
		name string
		key  string
		want map[string]string
	}{
		{"Linear 任务：issueTitle 为 null", fmt.Sprintf("task:%d", tl), map[string]string{"externalKey": "ENG-" + r, "trackerProvider": "linear", "issueTitle": "<null>"}},
		{"内置任务：issueTitle 取自己的工单", fmt.Sprintf("task:%d", ti), map[string]string{"externalKey": issueA.Key, "trackerProvider": "internal", "issueTitle": "内置标题-" + r}},
		{"Linear 任务撞上 B 的工单号：不跨属主取标题", fmt.Sprintf("task:%d", tx), map[string]string{"externalKey": issueB.Key, "trackerProvider": "linear", "issueTitle": "<null>"}},
		{"PRD：oneLiner 去空白", fmt.Sprintf("prd:%d", p1), map[string]string{"oneLiner": "一句话-" + r}},
		{"PRD 还没有 document：oneLiner 为 null", fmt.Sprintf("prd:%d", p2), map[string]string{"oneLiner": "<null>"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			it := byKey(list, tc.key)
			if it == nil {
				t.Fatalf("byGroup 缺 %s", tc.key)
			}
			for k, want := range tc.want {
				v, present := it[k]
				if !present {
					t.Errorf("%s 键不存在, want %s", k, want)
				} else if got := str(v); got != want {
					t.Errorf("%s got %s, want %s", k, got, want)
				}
			}
		})
	}
	t.Run("PRD 81 个汉字：按 rune 截到 80 且是合法 UTF-8", func(t *testing.T) {
		it := byKey(list, fmt.Sprintf("prd:%d", p3))
		s, _ := it["oneLiner"].(string)
		if utf8.RuneCountInString(s) != 80 || !utf8.ValidString(s) {
			t.Errorf("oneLiner rune 数 got %d (valid=%v), want 80", utf8.RuneCountInString(s), utf8.ValidString(s))
		}
	})
	t.Run("任何项都没有 title 键", func(t *testing.T) {
		for _, it := range list {
			if _, ok := it["title"]; ok {
				t.Errorf("项 %v 带了 title 键", it["key"])
			}
		}
	})

	admin := newUsageUser(t, st, store.RoleAdmin)
	allowed := map[string]bool{"key": true, "kind": true, "taskId": true, "prdId": true, "repoId": true, "runs": true,
		"costUsd": true, "durationMs": true, "tokens": true, "tokensKnown": true, "costKnown": true}
	for _, g := range []string{"task&limit=200", "repo"} {
		t.Run("admin group="+g+"：键集合不越界、响应不含标签原文", func(t *testing.T) {
			code, body := admin.get(t, "/api/admin/usage?group="+g)
			if code != http.StatusOK {
				t.Fatalf("状态码 got %d, want 200", code)
			}
			for _, it := range items(t, asMap(t, body), "byGroup") {
				for k := range it {
					if !allowed[k] {
						t.Errorf("项 %v 出现越界键 %q", it["key"], k)
					}
				}
			}
			if strings.Contains(body, r) {
				t.Errorf("admin 响应体含随机串 %s（标签泄漏）", r)
			}
		})
	}
}

// AC-7c：任务明细的正向内容。
func TestTaskUsageDetail(t *testing.T) {
	st := testStoreForAPI(t)
	ctx := context.Background()
	e := newUsageUser(t, st, store.RoleMember)
	other := newUsageUser(t, st, store.RoleMember)
	base := time.Now().Add(-time.Hour).Truncate(time.Microsecond)

	type check func(t *testing.T, sum map[string]any, runs []json.RawMessage)
	field := func(t *testing.T, raw json.RawMessage, key string) json.RawMessage {
		t.Helper()
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		v, ok := m[key]
		if !ok {
			t.Fatalf("行缺 %s 键: %s", key, raw)
		}
		return v
	}
	cases := []struct {
		name  string
		seed  func(t *testing.T, taskID int64)
		query string
		check check
	}{
		{"混合：时序、null 语义、回补、review、preview", func(t *testing.T, id int64) {
			e.run(t, agent.RunRecord{Kind: "task", TaskID: id, Phase: "triage", StartedAt: base, Result: paidRes(0.1)})
			e.run(t, agent.RunRecord{Kind: "task", TaskID: id, Phase: "implement", StartedAt: base.Add(time.Second), Err: fmt.Errorf("%w", agent.ErrIdle)})
			if err := st.SetAgentRunTier(ctx, id, "light"); err != nil {
				t.Fatal(err)
			}
			e.run(t, agent.RunRecord{Kind: "task", TaskID: id, Phase: "fix-1", ReviewRound: true, VerifyTier: "light", StartedAt: base.Add(2 * time.Second), Result: paidRes(0.2)})
			e.run(t, agent.RunRecord{Kind: "preview", TaskID: id, Phase: "recommend", StartedAt: base.Add(3 * time.Second), Result: paidRes(0)})
		}, "", func(t *testing.T, sum map[string]any, runs []json.RawMessage) {
			if len(runs) != 4 {
				t.Fatalf("len(runs) got %d, want 4", len(runs))
			}
			var phases []string
			for _, r := range runs {
				var p string
				_ = json.Unmarshal(field(t, r, "phase"), &p)
				phases = append(phases, p)
			}
			if got := strings.Join(phases, ","); got != "triage,implement,fix-1,recommend" {
				t.Errorf("phase 序列 got %s", got)
			}
			if got := string(field(t, runs[1], "costUsd")); got != "null" {
				t.Errorf("runs[1].costUsd got %s, want null", got)
			}
			if got := string(field(t, runs[1], "tokensKnown")); got != "false" {
				t.Errorf("runs[1].tokensKnown got %s, want false", got)
			}
			if got := string(field(t, runs[0], "verifyTier")); got != `"light"` {
				t.Errorf("runs[0].verifyTier got %s, want \"light\"（回补后）", got)
			}
			if string(field(t, runs[2], "reviewRound")) != "true" || string(field(t, runs[2], "bucket")) != `"review"` {
				t.Errorf("runs[2] reviewRound/bucket got %s/%s", field(t, runs[2], "reviewRound"), field(t, runs[2], "bucket"))
			}
			if got := string(field(t, runs[3], "runKind")); got != `"preview"` {
				t.Errorf("runs[3].runKind got %s", got)
			}
			if c, _ := sum["costUsd"].(float64); sum["runs"] != float64(4) || math.Abs(c-0.3) > 1e-9 || sum["costKnown"] != float64(3) {
				t.Errorf("summary runs/costUsd/costKnown got %v/%v/%v, want 4/0.3/3", sum["runs"], sum["costUsd"], sum["costKnown"])
			}
		}},
		{"从未定档：verifyTier 是 JSON null", func(t *testing.T, id int64) {
			e.run(t, agent.RunRecord{Kind: "task", TaskID: id, Phase: "triage", StartedAt: base, Result: paidRes(0.1)})
		}, "", func(t *testing.T, _ map[string]any, runs []json.RawMessage) {
			if got := string(field(t, runs[0], "verifyTier")); got != "null" {
				t.Errorf("verifyTier got %s, want null（不是 \"\"）", got)
			}
		}},
		{"同一 started_at 并列：按写入顺序稳定", func(t *testing.T, id int64) {
			e.run(t, agent.RunRecord{Kind: "task", TaskID: id, Phase: "implement", StartedAt: base, Result: paidRes(0.1)})
			e.run(t, agent.RunRecord{Kind: "task", TaskID: id, Phase: "fix-1", StartedAt: base, Result: paidRes(0.1)})
		}, "", nil},
		{"不受时间范围影响：60 天前的行照样返回", func(t *testing.T, id int64) {
			e.run(t, agent.RunRecord{Kind: "task", TaskID: id, Phase: "implement", StartedAt: time.Now().Add(-60 * 24 * time.Hour), Result: paidRes(0.1)})
		}, "?from=" + time.Now().Add(-24*time.Hour).UTC().Format("2006-01-02"), func(t *testing.T, sum map[string]any, runs []json.RawMessage) {
			if len(runs) != 1 || sum["runs"] != float64(1) {
				t.Errorf("len(runs)/summary.runs got %d/%v, want 1/1", len(runs), sum["runs"])
			}
		}},
		{"空任务：runs=[]、summary.costUsd=null", func(*testing.T, int64) {}, "", func(t *testing.T, sum map[string]any, runs []json.RawMessage) {
			if runs == nil || len(runs) != 0 || sum["runs"] != float64(0) || sum["costUsd"] != nil {
				t.Errorf("runs/summary got %v/%v", runs, sum)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := e.task(t, fmt.Sprintf("TU-%d", time.Now().UnixNano()), "linear")
			tc.seed(t, id)
			var first string
			for i := 0; i < 10; i++ {
				code, body := e.get(t, fmt.Sprintf("/api/tasks/%d/usage%s", id, tc.query))
				if code != http.StatusOK {
					t.Fatalf("状态码 got %d, want 200: %s", code, body)
				}
				if i == 0 {
					first = body
				} else if body != first {
					t.Fatalf("第 %d 次响应与第 1 次不同（顺序不稳定）", i+1)
				}
			}
			var resp struct {
				Summary map[string]any    `json:"summary"`
				Runs    []json.RawMessage `json:"runs"`
			}
			if err := json.Unmarshal([]byte(first), &resp); err != nil {
				t.Fatal(err)
			}
			for i, r := range resp.Runs {
				for _, k := range []string{"models", "reviewRound", "tokens"} {
					if string(field(t, r, k)) == "null" {
						t.Errorf("runs[%d].%s got null", i, k)
					}
				}
			}
			if tc.check != nil {
				tc.check(t, resp.Summary, resp.Runs)
			}
		})
	}
	t.Run("他人任务 → 404", func(t *testing.T) {
		id := other.task(t, fmt.Sprintf("TU-O-%d", time.Now().UnixNano()), "linear")
		if code, _ := e.get(t, fmt.Sprintf("/api/tasks/%d/usage", id)); code != http.StatusNotFound {
			t.Errorf("状态码 got %d, want 404", code)
		}
	})
}

// AC-8：属主隔离，非属主 404，未登录 401。
func TestUsageIsolatedByOwner(t *testing.T) {
	st := testStoreForAPI(t)
	a := newUsageUser(t, st, store.RoleMember)
	b := newUsageUser(t, st, store.RoleMember)
	ta := a.task(t, fmt.Sprintf("ISO-%d", time.Now().UnixNano()), "linear")
	a.run(t, agent.RunRecord{Kind: "task", TaskID: ta, Phase: "implement", StartedAt: time.Now().Add(-time.Hour), Result: paidRes(0.5)})

	cases := []struct {
		name     string
		path     string
		auth     bool
		wantCode int
		wantRuns float64 // 200 时断言 totals.runs
	}{
		{"B 看 /api/usage 只有自己的（空）", "/api/usage", true, 200, 0},
		{"B 按 A 的仓库筛选 → 404", fmt.Sprintf("/api/usage?repo=%d", a.repoID), true, 404, 0},
		{"B 看 A 的任务明细 → 404", fmt.Sprintf("/api/tasks/%d/usage", ta), true, 404, 0},
		{"未登录 → 401", "/api/usage", false, 401, 0},
		{"A 自己看得到", "/api/usage", true, 200, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := b
			if strings.HasPrefix(tc.name, "A ") {
				env = a
			}
			resp := do(t, env.srv, "GET", tc.path, "", tc.auth)
			if resp.StatusCode != tc.wantCode {
				t.Fatalf("状态码 got %d, want %d", resp.StatusCode, tc.wantCode)
			}
			if tc.wantCode == 200 {
				body := decode(t, resp)
				if got := body["totals"].(map[string]any)["runs"]; got != tc.wantRuns {
					t.Errorf("totals.runs got %v, want %v", got, tc.wantRuns)
				}
			}
		})
	}
}

// AC-9：管理员全局视图按归属断言；totals 只断言下界与 Σ byUser 不变量。
func TestAdminUsageGlobal(t *testing.T) {
	st := testStoreForAPI(t)
	a := newUsageUser(t, st, store.RoleMember)
	b := newUsageUser(t, st, store.RoleMember)
	admin := newUsageUser(t, st, store.RoleAdmin)
	ta := a.task(t, fmt.Sprintf("ADM-A-%d", time.Now().UnixNano()), "linear")
	tb := b.task(t, fmt.Sprintf("ADM-B-%d", time.Now().UnixNano()), "linear")
	s := time.Now().Add(-time.Hour)
	a.run(t, agent.RunRecord{Kind: "task", TaskID: ta, Phase: "implement", StartedAt: s, Result: paidRes(0.30)})
	a.run(t, agent.RunRecord{Kind: "task", TaskID: ta, Phase: "fix-1", StartedAt: s.Add(time.Second), Result: &agent.Result{Success: true}})
	b.run(t, agent.RunRecord{Kind: "task", TaskID: tb, Phase: "implement", StartedAt: s.Add(2 * time.Second), Result: paidRes(0.05)})

	code, body := admin.get(t, "/api/admin/usage")
	if code != http.StatusOK {
		t.Fatalf("状态码 got %d, want 200: %s", code, body)
	}
	m := asMap(t, body)
	users := items(t, m, "byUser")
	find := func(id int64) map[string]any {
		for _, u := range users {
			if u["userId"] == float64(id) {
				return u
			}
		}
		return nil
	}
	cases := []struct {
		name      string
		user      int64
		runs      float64
		cost      float64
		costKnown float64
	}{{"A 两次运行（含一条费用未知）", a.userID, 2, 0.30, 1}, {"B 一次运行", b.userID, 1, 0.05, 1}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := find(tc.user)
			if u == nil {
				t.Fatalf("byUser 缺 user %d", tc.user)
			}
			c, _ := u["costUsd"].(float64)
			if u["runs"] != tc.runs || math.Abs(c-tc.cost) > 1e-9 || u["costKnown"] != tc.costKnown {
				t.Errorf("runs/costUsd/costKnown got %v/%v/%v, want %v/%v/%v", u["runs"], u["costUsd"], u["costKnown"], tc.runs, tc.cost, tc.costKnown)
			}
		})
	}
	totals := m["totals"].(map[string]any)
	tc, _ := totals["costUsd"].(float64)
	if totals["runs"].(float64) < 3 || tc < 0.35-1e-9 {
		t.Errorf("totals runs/costUsd got %v/%v, want ≥3/≥0.35", totals["runs"], totals["costUsd"])
	}
	sum := 0.0
	for _, u := range users {
		sum += u["runs"].(float64)
	}
	if sum != totals["runs"].(float64) {
		t.Errorf("Σ byUser.runs got %v, want totals.runs %v", sum, totals["runs"])
	}
	for _, k := range []string{"email", "externalKey", "issueKey", "trackerProvider", "issueTitle", "oneLiner", "providerRepo", "title"} {
		if strings.Contains(body, `"`+k+`"`) {
			t.Errorf("admin 响应体含禁用键 %q", k)
		}
	}
}

// AC-9：普通成员访问管理员视图 → 403（与 /api/admin/* 一致）。
func TestAdminUsageForbiddenForMember(t *testing.T) {
	st := testStoreForAPI(t)
	m := newUsageUser(t, st, store.RoleMember)
	if code, _ := m.get(t, "/api/admin/usage"); code != http.StatusForbidden {
		t.Errorf("状态码 got %d, want 403", code)
	}
}

// AC-10：非法参数一律 400 + 中文原因，属主端点与管理员端点同表。
func TestUsageRejectsBadParams(t *testing.T) {
	st := testStoreForAPI(t)
	member := newUsageUser(t, st, store.RoleMember)
	admin := newUsageUser(t, st, store.RoleAdmin)
	cases := []string{
		"from=abc", "from=2026-11-10&to=2026-11-01", "limit=0", "limit=201", "limit=abc", "limit=0&group=phase",
		"group=nope", "repo=abc", "tier=medium", "bucket=nope", "bucket=fix-1",
	}
	hasHan := func(s string) bool {
		for _, r := range s {
			if r >= 0x4e00 && r <= 0x9fff {
				return true
			}
		}
		return false
	}
	for _, ep := range []struct {
		name string
		env  *usageEnv
		path string
	}{{"属主", member, "/api/usage"}, {"管理员", admin, "/api/admin/usage"}} {
		for _, q := range cases {
			t.Run(ep.name+" "+q, func(t *testing.T) {
				code, body := ep.env.get(t, ep.path+"?"+q)
				if code != http.StatusBadRequest {
					t.Fatalf("状态码 got %d, want 400: %s", code, body)
				}
				msg, _ := asMap(t, body)["error"].(string)
				if msg == "" || !hasHan(msg) {
					t.Errorf("error got %q, want 非空中文", msg)
				}
			})
		}
	}
}

// AC-10：未知参数（含 phase=）被忽略，响应逐字节相同。
func TestUsageIgnoresUnknownParams(t *testing.T) {
	st := testStoreForAPI(t)
	e := newUsageUser(t, st, store.RoleMember)
	id := e.task(t, fmt.Sprintf("UNK-%d", time.Now().UnixNano()), "linear")
	s := time.Now().Add(-time.Hour)
	e.run(t, agent.RunRecord{Kind: "task", TaskID: id, Phase: "implement", StartedAt: s, Result: paidRes(0.2)})
	e.run(t, agent.RunRecord{Kind: "task", TaskID: id, Phase: "fix-1", StartedAt: s.Add(time.Second), Result: paidRes(0.1)})
	window := "from=" + time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339) + "&to=" + time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	_, base := e.get(t, "/api/usage?group=phase&"+window)
	for _, extra := range []string{"phase=fix-1", "foo=bar"} {
		t.Run(extra, func(t *testing.T) {
			code, body := e.get(t, "/api/usage?group=phase&"+window+"&"+extra)
			if code != http.StatusOK || body != base {
				t.Errorf("带 %s 的响应与不带时不同（code=%d）:\ngot  %s\nwant %s", extra, code, body, base)
			}
		})
	}
}

// AC-10：空账号 200，计数为 0、costUsd 为 null、集合是 []。
func TestUsageZeroValue(t *testing.T) {
	st := testStoreForAPI(t)
	e := newUsageUser(t, st, store.RoleMember)
	for _, g := range []string{"phase", "task", "model"} {
		t.Run("group="+g, func(t *testing.T) {
			code, body := e.get(t, "/api/usage?group="+g)
			if code != http.StatusOK {
				t.Fatalf("状态码 got %d, want 200", code)
			}
			m := asMap(t, body)
			totals := m["totals"].(map[string]any)
			if totals["runs"] != float64(0) || totals["costKnown"] != float64(0) || totals["costUsd"] != nil {
				t.Errorf("totals got %v, want runs 0、costKnown 0、costUsd null", totals)
			}
			for _, k := range []string{"byBucket", "byGroup", "byDay", "byModel"} {
				if v, ok := m[k].([]any); !ok || len(v) != 0 {
					t.Errorf("%s got %#v, want []", k, m[k])
				}
			}
		})
	}
}
