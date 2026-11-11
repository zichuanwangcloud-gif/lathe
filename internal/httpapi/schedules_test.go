package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/linear"
	"github.com/zichuanwangcloud-gif/lathe/internal/schedule"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// 定时建单 API（docs/features/scheduled-issue.md §3.5）。

// schedLinear 起一个回固定团队 / 标签 / 建单的假 Linear；teamsStatus 非 200 时 teams 查询返回该状态码。
func schedLinear(t *testing.T, teamsStatus int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		q := string(raw)
		switch {
		case strings.Contains(q, "teams("):
			if teamsStatus != http.StatusOK {
				w.WriteHeader(teamsStatus)
				return
			}
			_, _ = io.WriteString(w, `{"data":{"teams":{"nodes":[{"id":"team-1","key":"ENG","name":"Engineering"}]}}}`)
		case strings.Contains(q, "issueLabels"):
			_, _ = io.WriteString(w, `{"data":{"issueLabels":{"nodes":[{"id":"label-1","name":"lathe:go","team":null}]}}}`)
		case strings.Contains(q, "issueCreate"):
			var body struct {
				Variables struct {
					Input map[string]any `json:"input"`
				} `json:"variables"`
			}
			_ = json.Unmarshal(raw, &body)
			_, _ = fmt.Fprintf(w, `{"data":{"issueCreate":{"success":true,"issue":{"id":%q,"identifier":"ENG-9","url":"https://linear.app/x/ENG-9"}}}}`, body.Variables.Input["id"])
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type fakeIntake struct {
	url    string
	linErr error
	secret bool
	userID string
	label  string
}

func (f *fakeIntake) Linear(ctx context.Context, userID int64) (ScheduleLinear, error) {
	if f.linErr != nil {
		return nil, f.linErr
	}
	return linear.NewClientWithURL("tok", f.url)
}
func (f *fakeIntake) WebhookSecretConfigured(ctx context.Context, userID int64) bool { return f.secret }
func (f *fakeIntake) LinearUserID(ctx context.Context, userID int64) string          { return f.userID }
func (f *fakeIntake) TriggerLabel(ctx context.Context) string                        { return f.label }

type countingFirer struct {
	inner ScheduleFirer
	calls atomic.Int64
}

func (c *countingFirer) FireNow(ctx context.Context, scheduleID, userID int64) (*store.ScheduleRunRow, error) {
	c.calls.Add(1)
	if c.inner == nil {
		return nil, errors.New("测试未接 Runner")
	}
	return c.inner.FireNow(ctx, scheduleID, userID)
}

type schedFixture struct {
	st     *store.Store
	uid    int64
	other  int64
	srv    *httptest.Server // 属主视角
	osrv   *httptest.Server // 另一个用户视角
	api    *SchedulesAPI
	intake *fakeIntake
	firer  *countingFirer
	now    time.Time
}

func schedUser(t *testing.T, st *store.Store, tag string) int64 {
	t.Helper()
	return mustUser(t, st, fmt.Sprintf("sched-%s-%d@example.com", tag, time.Now().UnixNano()))
}

func newSchedFixture(t *testing.T) *schedFixture {
	t.Helper()
	st := testStoreForAPI(t)
	f := &schedFixture{st: st, uid: schedUser(t, st, "api"), other: schedUser(t, st, "api-other"),
		now: time.Date(2026, 10, 19, 8, 0, 10, 0, time.UTC)}
	lin := schedLinear(t, http.StatusOK)
	f.intake = &fakeIntake{url: lin.URL, secret: true, userID: "viewer-1", label: "lathe:go"}

	uid := f.uid
	runner := &schedule.Runner{
		Store: st, Settings: fakeLabel("lathe:go"), Now: func() time.Time { return f.now }, Node: "api-test",
		Linear: func(ctx context.Context, u int64) (schedule.LinearHandle, error) {
			if u != uid {
				return schedule.LinearHandle{}, fmt.Errorf("不是本测试的用户")
			}
			c, err := linear.NewClientWithURL("tok", lin.URL)
			return schedule.LinearHandle{Client: c, UserID: "viewer-1", Redact: func(s string) string { return s }}, err
		},
	}
	t.Cleanup(runner.Close)
	f.firer = &countingFirer{inner: runner}
	f.api = &SchedulesAPI{Store: st, Auth: authAs(f.uid, "owner@example.com"), Fire: f.firer,
		SchedulerEnabled: true, Preconditions: f.intake, Now: func() time.Time { return f.now }}
	f.srv = schedServer(t, f.api)
	other := *f.api
	other.Auth = authAs(f.other, "other@example.com")
	f.osrv = schedServer(t, &other)
	return f
}

type fakeLabel string

func (l fakeLabel) WebhookTriggerLabel(context.Context) string { return string(l) }

func schedServer(t *testing.T, api *SchedulesAPI) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	api.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func dailyBody(name string) string {
	return fmt.Sprintf(`{"name":%q,"kind":"daily","minuteOfDay":540,"timezone":"Asia/Shanghai","linearTeamId":"team-1","intakeMode":"assign","titleTemplate":"每日 {{date}}","bodyTemplate":"正文","priority":2}`, name)
}

func (f *schedFixture) create(t *testing.T, name string) int64 {
	t.Helper()
	resp, v := doReq(t, f.srv, "POST", "/api/schedules", dailyBody(name))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("新建: got %d %v, want 201", resp.StatusCode, v)
	}
	return int64(v["schedule"].(map[string]any)["id"].(float64))
}

func TestSchedulesCRUD(t *testing.T) {
	f := newSchedFixture(t)
	name := fmt.Sprint("crud-", time.Now().UnixNano())
	resp, v := doReq(t, f.srv, "POST", "/api/schedules", dailyBody(name))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST: got %d %v, want 201", resp.StatusCode, v)
	}
	sc := v["schedule"].(map[string]any)
	for _, k := range []string{"id", "name", "kind", "minuteOfDay", "timezone", "linearTeamId", "linearTeamKey", "intakeMode", "titleTemplate", "nextFireAt", "enabled", "description"} {
		if _, ok := sc[k]; !ok {
			t.Errorf("响应缺 camelCase 字段 %s: %v", k, sc)
		}
	}
	if sc["linearTeamKey"] != "ENG" || sc["enabled"] != true {
		t.Errorf("got teamKey=%v enabled=%v, want ENG/true", sc["linearTeamKey"], sc["enabled"])
	}
	// 上海 09:00 = 01:00Z；now 是 08:00Z → 次日 01:00Z
	if sc["nextFireAt"] != "2026-10-20T01:00:00Z" {
		t.Errorf("nextFireAt: got %v, want 2026-10-20T01:00:00Z", sc["nextFireAt"])
	}
	id := int64(sc["id"].(float64))

	_, v = doReq(t, f.srv, "GET", "/api/schedules", "")
	if list := v["schedules"].([]any); len(list) != 1 || int64(list[0].(map[string]any)["id"].(float64)) != id {
		t.Errorf("列表: got %v, want 只含 %d", list, id)
	}
	resp, v = doReq(t, f.srv, "GET", fmt.Sprintf("/api/schedules/%d", id), "")
	if resp.StatusCode != http.StatusOK || v["schedule"].(map[string]any)["name"] != name {
		t.Errorf("GET: got %d %v", resp.StatusCode, v)
	}
	upd := strings.Replace(dailyBody(name+"-2"), `"priority":2`, `"priority":3,"enabled":false`, 1)
	resp, v = doReq(t, f.srv, "PUT", fmt.Sprintf("/api/schedules/%d", id), upd)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT: got %d %v", resp.StatusCode, v)
	}
	if u := v["schedule"].(map[string]any); u["enabled"] != false || u["nextFireAt"] != nil || u["priority"] != float64(3) {
		t.Errorf("PUT 后: got %v, want enabled=false nextFireAt=null priority=3", u)
	}
	if resp, _ := doReq(t, f.srv, "DELETE", fmt.Sprintf("/api/schedules/%d", id), ""); resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE: got %d, want 204", resp.StatusCode)
	}
	if resp, _ := doReq(t, f.srv, "GET", fmt.Sprintf("/api/schedules/%d", id), ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("删后 GET: got %d, want 404", resp.StatusCode)
	}
}

func TestSchedulesIsolation404(t *testing.T) {
	f := newSchedFixture(t)
	id := f.create(t, fmt.Sprint("iso-", time.Now().UnixNano()))
	cases := []struct{ method, path, body string }{
		{"GET", fmt.Sprintf("/api/schedules/%d", id), ""},
		{"PUT", fmt.Sprintf("/api/schedules/%d", id), dailyBody("hijack")},
		{"DELETE", fmt.Sprintf("/api/schedules/%d", id), ""},
		{"POST", fmt.Sprintf("/api/schedules/%d/run", id), ""},
		{"GET", fmt.Sprintf("/api/schedules/%d/runs", id), ""},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			if resp, _ := doReq(t, f.osrv, tc.method, tc.path, tc.body); resp.StatusCode != http.StatusNotFound {
				t.Errorf("非属主: got %d, want 404", resp.StatusCode)
			}
		})
	}
	_, v := doReq(t, f.osrv, "GET", "/api/schedules", "")
	if list := v["schedules"].([]any); len(list) != 0 {
		t.Errorf("非属主列表: got %v, want 空", list)
	}
	if f.firer.calls.Load() != 0 {
		t.Errorf("非属主不应触发 Fire: got %d", f.firer.calls.Load())
	}
}

func TestSchedulesValidation(t *testing.T) {
	f := newSchedFixture(t)
	base := fmt.Sprint("v-", time.Now().UnixNano())
	dup := f.create(t, base+"-dup")
	_ = dup
	cases := []struct {
		name   string
		body   string
		status int
		errHas string
	}{
		{"bad_kind", strings.Replace(dailyBody(base+"1"), `"kind":"daily"`, `"kind":"cron"`, 1), 400, "cron"},
		{"interval_30", fmt.Sprintf(`{"name":%q,"kind":"interval","intervalMinutes":30,"linearTeamId":"team-1","titleTemplate":"x"}`, base+"2"), 400, "30"},
		{"unknown_placeholder", strings.Replace(dailyBody(base+"3"), "{{date}}", "{{dat}}", 1), 400, "dat"},
		{"name_too_long", dailyBody(strings.Repeat("名", 101)), 400, "100"},
		{"bad_timezone", strings.Replace(dailyBody(base+"4"), "Asia/Shanghai", "Mars/Base", 1), 400, "Mars/Base"},
		{"bad_priority", strings.Replace(dailyBody(base+"5"), `"priority":2`, `"priority":9`, 1), 400, "优先级"},
		{"unknown_team", strings.Replace(dailyBody(base+"6"), "team-1", "team-x", 1), 400, "团队"},
		{"duplicate_name_409", dailyBody(base + "-dup"), 409, "同名"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, v := doReq(t, f.srv, "POST", "/api/schedules", tc.body)
			if resp.StatusCode != tc.status || !strings.Contains(fmt.Sprint(v["error"]), tc.errHas) {
				t.Errorf("got %d %v, want %d 且错误含 %q", resp.StatusCode, v["error"], tc.status, tc.errHas)
			}
		})
	}
	t.Run("21st_schedule", func(t *testing.T) {
		for i := 0; i < schedule.MaxSchedulesPerUser-1; i++ { // 已有 1 条 dup
			f.create(t, fmt.Sprintf("%s-n%d", base, i))
		}
		resp, v := doReq(t, f.srv, "POST", "/api/schedules", dailyBody(base+"-21"))
		if resp.StatusCode != 400 || !strings.Contains(fmt.Sprint(v["error"]), "20") {
			t.Errorf("第 21 条: got %d %v, want 400 且含 20", resp.StatusCode, v["error"])
		}
	})
}

func TestSchedulesRequireIntakePreconditions(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*fakeIntake)
		body   func(string) string
		errHas string
	}{
		{"no_webhook_secret", func(i *fakeIntake) { i.secret = false }, dailyBody, "webhook"},
		{"label_mode_without_system_label", func(i *fakeIntake) { i.label = "" },
			func(n string) string {
				return strings.Replace(dailyBody(n), `"intakeMode":"assign"`, `"intakeMode":"label"`, 1)
			}, "标签"},
		{"assign_without_linear_user", func(i *fakeIntake) { i.userID = "" }, dailyBody, "Linear 用户"},
		{"linear_creds_unavailable", func(i *fakeIntake) { i.linErr = errors.New("未配置") }, dailyBody, "Linear 凭据不可用"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSchedFixture(t)
			tc.mutate(f.intake)
			resp, v := doReq(t, f.srv, "POST", "/api/schedules", tc.body(fmt.Sprint("pre-", time.Now().UnixNano())))
			if resp.StatusCode != 400 || !strings.Contains(fmt.Sprint(v["error"]), tc.errHas) {
				t.Errorf("got %d %v, want 400 且含 %q", resp.StatusCode, v["error"], tc.errHas)
			}
		})
	}
	t.Run("disabled_only_checks_shape", func(t *testing.T) {
		f := newSchedFixture(t)
		f.intake.secret = false
		body := strings.Replace(dailyBody(fmt.Sprint("pre-off-", time.Now().UnixNano())), `"priority":2`, `"priority":2,"enabled":false`, 1)
		if resp, v := doReq(t, f.srv, "POST", "/api/schedules", body); resp.StatusCode != http.StatusCreated {
			t.Errorf("停用的规则只做形状校验: got %d %v, want 201", resp.StatusCode, v)
		}
	})
}

func TestSchedulesRunNowDedupSameMinute(t *testing.T) {
	f := newSchedFixture(t)
	id := f.create(t, fmt.Sprint("run-", time.Now().UnixNano()))
	resp, v := doReq(t, f.srv, "POST", fmt.Sprintf("/api/schedules/%d/run", id), "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("第一次: got %d %v, want 202", resp.StatusCode, v)
	}
	run := v["run"].(map[string]any)
	if run["trigger"] != "manual" || run["status"] != "created" || run["scheduledFor"] != "2026-10-19T08:00:00Z" {
		t.Errorf("run: got %v, want manual/created/08:00", run)
	}
	if resp, v := doReq(t, f.srv, "POST", fmt.Sprintf("/api/schedules/%d/run", id), ""); resp.StatusCode != http.StatusConflict {
		t.Errorf("同一分钟第二次: got %d %v, want 409", resp.StatusCode, v)
	}
}

func TestSchedulesRunsShowDerivedTask(t *testing.T) {
	f := newSchedFixture(t)
	id := f.create(t, fmt.Sprint("derive-", time.Now().UnixNano()))
	ctx := context.Background()
	nonce := time.Now().UnixNano()
	issueID := fmt.Sprintf("iss-%d", nonce)
	if _, err := f.st.Pool().Exec(ctx, `
		INSERT INTO schedule_runs (schedule_id, user_id, scheduled_for, trigger, status, linear_issue_uuid, linear_issue_id)
		VALUES ($1,$2,$3,'schedule','created',gen_random_uuid(),$4)`, id, f.uid, f.now, issueID); err != nil {
		t.Fatal(err)
	}
	insertTask := func(uid int64) int64 {
		var repoID, taskID int64
		if err := f.st.Pool().QueryRow(ctx, `INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
			uid, fmt.Sprintf("acme/sched-%d-%d", uid, nonce)).Scan(&repoID); err != nil {
			t.Fatal(err)
		}
		if err := f.st.Pool().QueryRow(ctx, `
			INSERT INTO tasks (user_id, repo_id, external_key, external_id, tracker_provider, state)
			VALUES ($1,$2,'ENG-9',$3,'linear','queued') RETURNING id`, uid, repoID, issueID).Scan(&taskID); err != nil {
			t.Fatal(err)
		}
		return taskID
	}

	// 别人的任务行：不得挂到自己的记录上
	insertTask(f.other)
	_, v := doReq(t, f.srv, "GET", fmt.Sprintf("/api/schedules/%d/runs", id), "")
	runs := v["runs"].([]any)
	if len(runs) != 1 || runs[0].(map[string]any)["task"] != nil {
		t.Fatalf("别人的任务: got %v, want task=null", runs)
	}

	mine := insertTask(f.uid)
	_, v = doReq(t, f.srv, "GET", fmt.Sprintf("/api/schedules/%d/runs", id), "")
	task, _ := v["runs"].([]any)[0].(map[string]any)["task"].(map[string]any)
	if task == nil || int64(task["id"].(float64)) != mine || task["state"] != "queued" {
		t.Errorf("派生任务: got %v, want id=%d state=queued", task, mine)
	}
}

func TestSchedulesPreview(t *testing.T) {
	f := newSchedFixture(t)
	resp, v := doReq(t, f.srv, "POST", "/api/schedules/preview",
		`{"kind":"weekly","minuteOfDay":540,"weekdays":10,"timezone":"Asia/Shanghai"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d %v", resp.StatusCode, v)
	}
	next := v["next"].([]any)
	if len(next) != 5 {
		t.Fatalf("got %d 个时刻, want 5", len(next))
	}
	var prev time.Time
	for i, s := range next {
		ts, err := time.Parse(time.RFC3339, s.(string))
		if err != nil || (i > 0 && !ts.After(prev)) {
			t.Errorf("第 %d 个时刻 %v 应严格递增（前一个 %v）", i, s, prev)
		}
		prev = ts
	}
	if v["description"] != "每周一、三 09:00（Asia/Shanghai）" {
		t.Errorf("description: got %v", v["description"])
	}
	if resp, _ := doReq(t, f.srv, "POST", "/api/schedules/preview", `{"kind":"interval","intervalMinutes":59}`); resp.StatusCode != 400 {
		t.Errorf("非法规则预览: got %d, want 400", resp.StatusCode)
	}
}

func TestSchedulesRunNowSchedulerDisabled(t *testing.T) {
	f := newSchedFixture(t)
	id := f.create(t, fmt.Sprint("off-", time.Now().UnixNano()))
	f.api.SchedulerEnabled = false
	srv := schedServer(t, f.api)
	other := *f.api
	other.Auth = authAs(f.other, "other@example.com")
	osrv := schedServer(t, &other)

	resp, v := doReq(t, srv, "POST", fmt.Sprintf("/api/schedules/%d/run", id), "")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(fmt.Sprint(v["error"]), "调度器已关闭") {
		t.Errorf("属主: got %d %v, want 503 调度器已关闭", resp.StatusCode, v)
	}
	if f.firer.calls.Load() != 0 {
		t.Errorf("Fire 调用: got %d, want 0", f.firer.calls.Load())
	}
	var n int
	if err := f.st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM schedule_runs WHERE schedule_id=$1`, id).Scan(&n); err != nil || n != 0 {
		t.Errorf("schedule_runs: got %d (%v), want 0", n, err)
	}
	if resp, _ := doReq(t, osrv, "POST", fmt.Sprintf("/api/schedules/%d/run", id), ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("非属主: got %d, want 404（先 404 后 503）", resp.StatusCode)
	}
	if resp, _ := doReq(t, srv, "GET", "/api/schedules", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("列表: got %d, want 200", resp.StatusCode)
	}
}

func TestSchedulesCreateLinearUnreachable(t *testing.T) {
	f := newSchedFixture(t)
	f.intake.url = schedLinear(t, http.StatusServiceUnavailable).URL
	resp, v := doReq(t, f.srv, "POST", "/api/schedules", dailyBody(fmt.Sprint("down-", time.Now().UnixNano())))
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("got %d %v, want 502", resp.StatusCode, v)
	}
	if n, _ := f.st.CountSchedules(context.Background(), f.uid); n != 0 {
		t.Errorf("不保存: got %d 行, want 0", n)
	}
}

func TestLinearTeamsEndpoint(t *testing.T) {
	t.Run("creds_unavailable_400", func(t *testing.T) {
		srv := newLinearAPITestServer(t, func(ctx context.Context, userID int64) (*linear.Client, error) {
			return nil, errors.New("未配置")
		})
		if code, _ := getAuthed(t, srv, "/api/linear/teams"); code != http.StatusBadRequest {
			t.Errorf("got %d, want 400", code)
		}
	})
	t.Run("lists_teams", func(t *testing.T) {
		lin := schedLinear(t, http.StatusOK)
		srv := newLinearAPITestServer(t, func(ctx context.Context, userID int64) (*linear.Client, error) {
			return linear.NewClientWithURL("tok", lin.URL)
		})
		code, v := getAuthed(t, srv, "/api/linear/teams")
		teams, _ := v["teams"].([]any)
		if code != http.StatusOK || len(teams) != 1 || teams[0].(map[string]any)["key"] != "ENG" {
			t.Errorf("got %d %v, want 200 且一个 ENG 团队", code, v)
		}
	})
	t.Run("linear_down_502", func(t *testing.T) {
		lin := schedLinear(t, http.StatusServiceUnavailable)
		srv := newLinearAPITestServer(t, func(ctx context.Context, userID int64) (*linear.Client, error) {
			return linear.NewClientWithURL("tok", lin.URL)
		})
		if code, _ := getAuthed(t, srv, "/api/linear/teams"); code != http.StatusBadGateway {
			t.Errorf("got %d, want 502", code)
		}
	})
}
