package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Clouditera/lathe/internal/store"
	"github.com/Clouditera/lathe/internal/task"
)

const apiTestToken = "test-admin-token"

func apiFixture(t *testing.T) (*API, *store.Store, *task.Machine, int64) {
	t.Helper()

	st := testStoreForAPI(t)
	userID := mustUser(t, st, "api-"+t.Name()+"@example.com")

	var repoID int64
	if err := st.Pool().QueryRow(context.Background(),
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2)
		 ON CONFLICT (user_id, provider_repo) DO UPDATE SET updated_at=now() RETURNING id`,
		userID, "acme/api-test").Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}

	m := task.NewMachine(st.Pool())
	api := &API{
		Store: st, Tasks: m, Queue: &fakeEnqueuer{}, Auth: authAs(userID, "api-fixture@example.com"),
		ConfigStatus: func() map[string]any {
			return map[string]any{"linear": map[string]any{"configured": true, "source": "env"}}
		},
	}
	return api, st, m, repoID
}

// mustUser 建一个测试用户，测试结束连其名下数据一起清掉。
func mustUser(t *testing.T, st *store.Store, email string) int64 {
	t.Helper()
	var id int64
	if err := st.Pool().QueryRow(context.Background(),
		`INSERT INTO users (email) VALUES ($1) ON CONFLICT (email) DO UPDATE SET updated_at=now() RETURNING id`,
		email).Scan(&id); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM users WHERE id=$1`, id)
	})
	return id
}

// authAs 构造一个把令牌通道映射到指定用户的鉴权组件。
//
// 数据隔离测试的关键工具：同一台服务、同一份数据，换的是「谁登录着」。
func authAs(userID int64, email string) *Auth {
	a := NewAuth(apiTestToken)
	a.Bootstrap = &store.User{ID: userID, Email: email, Role: store.RoleMember}
	return a
}

func apiServer(t *testing.T, api *API) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	api.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path, body string, auth bool) *http.Response {
	t.Helper()
	var r *http.Request
	var err error
	if body == "" {
		r, err = http.NewRequest(method, srv.URL+path, nil)
	} else {
		r, err = http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	}
	if err != nil {
		t.Fatal(err)
	}
	if auth {
		r.Header.Set("Authorization", "Bearer "+apiTestToken)
	}
	resp, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	return v
}

// ★所有 API 端点都必须要求认证 —— 任务详情含 issue 标题、分支名、
// 失败原因等内部信息，不该匿名可读。
func TestAPIRequiresAuth(t *testing.T) {
	api, _, _, repoID := apiFixture(t)
	srv := apiServer(t, api)

	endpoints := []struct{ method, path, body string }{
		{"GET", "/api/tasks", ""},
		{"GET", "/api/tasks/1", ""},
		{"GET", "/api/tasks/1/events", ""},
		{"GET", "/api/stats", ""},
		{"GET", "/api/repos", ""},
		{"GET", "/api/config", ""},
		{"POST", "/api/tasks", `{"issueKey":"CR-1"}`},
		{"POST", "/api/tasks/1/retry", ""},
		{"POST", "/api/tasks/1/cancel", ""},
		{"POST", "/api/repos", `{"providerRepo":"acme/x"}`},
		{"PUT", "/api/repos/" + itoa(repoID), `{"gateMode":"direct"}`},
	}

	for _, e := range endpoints {
		resp := do(t, srv, e.method, e.path, e.body, false)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s 未认证时状态码 = %d，期望 401", e.method, e.path, resp.StatusCode)
		}
	}
}

func TestAPIListAndDetail(t *testing.T) {
	api, _, m, repoID := apiFixture(t)
	srv := apiServer(t, api)
	ctx := context.Background()

	var userID int64
	_ = api.Store.Pool().QueryRow(ctx, `SELECT user_id FROM repos WHERE id=$1`, repoID).Scan(&userID)

	tk, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "CR-9001",
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	if _, err := m.Transition(ctx, tk.ID, task.StateTriaging, "test", nil); err != nil {
		t.Fatal(err)
	}

	// 列表
	resp := do(t, srv, "GET", "/api/tasks", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("列表状态码 = %d", resp.StatusCode)
	}
	body := decode(t, resp)
	tasks, _ := body["tasks"].([]any)
	if len(tasks) == 0 {
		t.Fatal("列表应包含刚建的任务")
	}

	// 按状态过滤
	resp = do(t, srv, "GET", "/api/tasks?state=triaging", "", true)
	body = decode(t, resp)
	tasks, _ = body["tasks"].([]any)
	found := false
	for _, x := range tasks {
		if row, ok := x.(map[string]any); ok && row["linearIssueKey"] == "CR-9001" {
			found = true
			if row["state"] != "triaging" {
				t.Errorf("过滤结果含非目标状态: %v", row["state"])
			}
		}
	}
	if !found {
		t.Error("按 triaging 过滤应能找到该任务")
	}

	// 非法状态值应被拒绝，而不是当成空过滤返回全部
	resp = do(t, srv, "GET", "/api/tasks?state=bogus", "", true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("非法状态值应返回 400，得到 %d", resp.StatusCode)
	}

	// 详情：必须带状态轨迹
	resp = do(t, srv, "GET", "/api/tasks/"+itoa(tk.ID), "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("详情状态码 = %d", resp.StatusCode)
	}
	detail := decode(t, resp)
	events, _ := detail["events"].([]any)
	if len(events) != 2 { // 创建 + 一次转移
		t.Errorf("状态轨迹应有 2 条事件，得到 %d", len(events))
	}
	if _, ok := detail["verifications"]; !ok {
		t.Error("详情应含 verifications 字段（哪怕为空数组）")
	}

	// 不存在的任务
	resp = do(t, srv, "GET", "/api/tasks/99999999", "", true)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("不存在的任务应返回 404，得到 %d", resp.StatusCode)
	}
	// 非法 ID
	resp = do(t, srv, "GET", "/api/tasks/abc", "", true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("非法 ID 应返回 400，得到 %d", resp.StatusCode)
	}
}

func TestAPITriggerTask(t *testing.T) {
	api, _, _, _ := apiFixture(t)
	q := &fakeEnqueuer{}
	api.Queue = q
	srv := apiServer(t, api)

	resp := do(t, srv, "POST", "/api/tasks", `{"issueKey":"CR-9002"}`, true)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("手动触发状态码 = %d，期望 202", resp.StatusCode)
	}
	if len(q.issues) != 1 || q.issues[0] != "CR-9002" {
		t.Errorf("应入队 CR-9002，实际 %v", q.issues)
	}

	// 两个字段都缺
	resp = do(t, srv, "POST", "/api/tasks", `{}`, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("缺 issue 标识应返回 400，得到 %d", resp.StatusCode)
	}
}

// 重试必须走 failed→queued 这条合法边，且非法转移返回 409 而非 400 ——
// 请求本身没问题，是资源当前状态不允许。
func TestAPIRetryAndCancel(t *testing.T) {
	api, _, m, repoID := apiFixture(t)
	q := &fakeEnqueuer{}
	api.Queue = q
	srv := apiServer(t, api)
	ctx := context.Background()

	var userID int64
	_ = api.Store.Pool().QueryRow(ctx, `SELECT user_id FROM repos WHERE id=$1`, repoID).Scan(&userID)

	tk, _ := m.Create(ctx, task.CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-9003"})

	// queued 状态下重试是非法转移（queued→queued 不存在）
	resp := do(t, srv, "POST", "/api/tasks/"+itoa(tk.ID)+"/retry", "", true)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("对 queued 任务重试应返回 409，得到 %d", resp.StatusCode)
	}

	// 走到 failed
	for _, s := range []task.State{task.StateTriaging, task.StateFailed} {
		if _, err := m.Transition(ctx, tk.ID, s, "test", nil); err != nil {
			t.Fatal(err)
		}
	}

	resp = do(t, srv, "POST", "/api/tasks/"+itoa(tk.ID)+"/retry", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("失败任务重试应成功，得到 %d", resp.StatusCode)
	}
	after, _ := m.Get(ctx, tk.ID)
	if after.State != task.StateQueued {
		t.Errorf("重试后状态 = %s，期望 queued", after.State)
	}
	if len(q.requeued) != 1 || q.requeued[0] != tk.ID {
		t.Errorf("重试应重派原任务行（不新建），实际 requeued=%v", q.requeued)
	}

	// 取消
	resp = do(t, srv, "POST", "/api/tasks/"+itoa(tk.ID)+"/cancel", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("取消应成功，得到 %d", resp.StatusCode)
	}
	after, _ = m.Get(ctx, tk.ID)
	if after.State != task.StateCancelled {
		t.Errorf("取消后状态 = %s", after.State)
	}

	// 终态不可再取消
	resp = do(t, srv, "POST", "/api/tasks/"+itoa(tk.ID)+"/cancel", "", true)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("终态再取消应返回 409，得到 %d", resp.StatusCode)
	}
}

func TestAPIUpdateRepo(t *testing.T) {
	api, _, _, repoID := apiFixture(t)
	srv := apiServer(t, api)

	resp := do(t, srv, "PUT", "/api/repos/"+itoa(repoID), `{"gateMode":"guarded","defaultBranch":"develop"}`, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("更新仓库状态码 = %d", resp.StatusCode)
	}
	body := decode(t, resp)
	if body["gateMode"] != "guarded" || body["defaultBranch"] != "develop" {
		t.Errorf("更新未生效: %+v", body)
	}

	// 非法档位
	resp = do(t, srv, "PUT", "/api/repos/"+itoa(repoID), `{"gateMode":"随便填"}`, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("非法准入档位应返回 400，得到 %d", resp.StatusCode)
	}

	// ★清空受保护分支等于关掉最后一道闸门，必须拒绝
	resp = do(t, srv, "PUT", "/api/repos/"+itoa(repoID), `{"protectedBranches":[]}`, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("清空受保护分支应被拒绝，得到 %d", resp.StatusCode)
	}
}

// 配置接口绝不能返回 token 本身。
func TestAPIConfigNeverLeaksSecrets(t *testing.T) {
	api, _, _, _ := apiFixture(t)
	api.ConfigStatus = func() map[string]any {
		return map[string]any{
			"linear": map[string]any{"configured": true, "source": "env:LATHE_LINEAR_TOKEN"},
			"github": map[string]any{"configured": false},
		}
	}
	srv := apiServer(t, api)

	resp := do(t, srv, "GET", "/api/config", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	var raw strings.Builder
	body := decode(t, resp)
	for k := range body {
		raw.WriteString(k)
	}
	if !strings.Contains(raw.String(), "linear") {
		t.Error("应返回各集成的配置状态")
	}
	// 结构上只允许 configured/source 两类字段
	lin, _ := body["linear"].(map[string]any)
	if lin["configured"] != true {
		t.Errorf("应报告已配置: %+v", lin)
	}
	if _, bad := lin["token"]; bad {
		t.Error("配置接口绝不能返回 token 本身")
	}
}

func TestAPIStats(t *testing.T) {
	api, _, _, _ := apiFixture(t)
	srv := apiServer(t, api)

	resp := do(t, srv, "GET", "/api/stats", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	body := decode(t, resp)
	if _, ok := body["byState"]; !ok {
		t.Error("应含 byState")
	}
	if _, ok := body["total"]; !ok {
		t.Error("应含 total")
	}
}

// ★P1.5 第二步的验收标准：同一台服务上，用户 B 看不到、也动不了
// 用户 A 的任务与仓库配置 —— 对非属主一律 404，不用 403 暴露存在。
func TestAPIIsolationBetweenUsers(t *testing.T) {
	st := testStoreForAPI(t)
	ctx := context.Background()

	userA := mustUser(t, st, "iso-a-"+t.Name()+"@example.com")
	userB := mustUser(t, st, "iso-b-"+t.Name()+"@example.com")

	var repoA int64
	if err := st.Pool().QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		userA, "acme/iso-a").Scan(&repoA); err != nil {
		t.Fatalf("建 A 的仓库失败: %v", err)
	}
	m := task.NewMachine(st.Pool())
	tkA, err := m.Create(ctx, task.CreateParams{UserID: userA, RepoID: repoA, LinearIssueKey: "ISO-1"})
	if err != nil {
		t.Fatalf("建 A 的任务失败: %v", err)
	}

	// B 视角的 API（同一台服务、同一份数据，只是登录的是 B）
	apiB := &API{Store: st, Tasks: m, Queue: &fakeEnqueuer{}, Auth: authAs(userB, "b@example.com")}
	srvB := apiServer(t, apiB)

	// B 的任务列表必须是空的（哪怕 A 有活任务）
	resp := do(t, srvB, "GET", "/api/tasks", "", true)
	body := decode(t, resp)
	if tasks, _ := body["tasks"].([]any); len(tasks) != 0 {
		t.Errorf("B 不应看到任何任务，实际 %d 条", len(tasks))
	}
	if total, _ := body["total"].(float64); total != 0 {
		t.Errorf("B 的任务总数应为 0，实际 %v", total)
	}

	// B 读 A 的任务详情 → 404
	resp = do(t, srvB, "GET", "/api/tasks/"+itoa(tkA.ID), "", true)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("B 读 A 的任务应 404，得到 %d", resp.StatusCode)
	}

	// B 取消/重试 A 的任务 → 404，且 A 的任务状态不能被撼动
	for _, action := range []string{"cancel", "retry"} {
		resp = do(t, srvB, "POST", "/api/tasks/"+itoa(tkA.ID)+"/"+action, "", true)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("B %s A 的任务应 404，得到 %d", action, resp.StatusCode)
		}
	}
	after, _ := m.Get(ctx, tkA.ID)
	if after.State != task.StateQueued {
		t.Errorf("A 的任务状态不应被 B 的操作改变，现在 %s", after.State)
	}

	// B 的仓库列表、统计、A 的仓库更新 → 各自隔离
	resp = do(t, srvB, "GET", "/api/repos", "", true)
	body = decode(t, resp)
	if repos, _ := body["repos"].([]any); len(repos) != 0 {
		t.Errorf("B 不应看到 A 的仓库，实际 %d 条", len(repos))
	}
	resp = do(t, srvB, "PUT", "/api/repos/"+itoa(repoA), `{"gateMode":"guarded"}`, true)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("B 改 A 的仓库应 404，得到 %d", resp.StatusCode)
	}
	resp = do(t, srvB, "GET", "/api/stats", "", true)
	body = decode(t, resp)
	if total, _ := body["total"].(float64); total != 0 {
		t.Errorf("B 的统计应不含 A 的任务，total = %v", total)
	}
}

// 登记仓库是隔离后新用户的必经入口：POST /api/repos。
func TestAPICreateRepo(t *testing.T) {
	st := testStoreForAPI(t)
	userID := mustUser(t, st, "create-repo-"+t.Name()+"@example.com")
	api := &API{
		Store: st, Tasks: task.NewMachine(st.Pool()), Queue: &fakeEnqueuer{},
		Auth: authAs(userID, "cr@example.com"),
	}
	srv := apiServer(t, api)

	resp := do(t, srv, "POST", "/api/repos", `{"providerRepo":"acme/new-repo"}`, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("登记仓库状态码 = %d，期望 201", resp.StatusCode)
	}
	body := decode(t, resp)
	if body["providerRepo"] != "acme/new-repo" {
		t.Errorf("应返回新仓库: %+v", body)
	}
	if body["defaultBranch"] != "dev" || body["hotfixBase"] != "main" {
		t.Errorf("默认基线应为 dev/main: %+v", body)
	}

	// 重复登记 → 409
	resp = do(t, srv, "POST", "/api/repos", `{"providerRepo":"acme/new-repo"}`, true)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("重复登记应 409，得到 %d", resp.StatusCode)
	}

	// 非法格式 → 400
	resp = do(t, srv, "POST", "/api/repos", `{"providerRepo":"没有斜线"}`, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("非法 providerRepo 应 400，得到 %d", resp.StatusCode)
	}

	// 同名仓库别人可以登 —— 隔离是 (user, repo) 二元组
	other := mustUser(t, st, "create-repo-other-"+t.Name()+"@example.com")
	api2 := &API{
		Store: st, Tasks: task.NewMachine(st.Pool()), Queue: &fakeEnqueuer{},
		Auth: authAs(other, "cr2@example.com"),
	}
	srv2 := apiServer(t, api2)
	resp = do(t, srv2, "POST", "/api/repos", `{"providerRepo":"acme/new-repo"}`, true)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("另一用户登记同名仓库应成功，得到 %d", resp.StatusCode)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
