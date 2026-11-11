package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/yunxiao"
	"github.com/zichuanwangcloud-gif/lathe/internal/secret"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// ---------------------------------------------------------------- 假云效

const yxSentinel = "SENTINEL-YX-UPSTREAM-91c2e"

// fakeYX 是一个可编程的 httptest 云效：按工作项 id 返回不同数据。
type fakeYX struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	items    map[string]map[string]any // id → GetWorkitem 对象
	failCode int                       // 非 0：所有请求都以该码失败（响应体含哨兵）
	userID   string                    // CurrentUser 返回的 id
	userName string
	count    atomic.Int64
	tokens   []string
	paths    []string
}

func newFakeYX(t *testing.T) *fakeYX {
	t.Helper()
	f := &fakeYX{t: t, items: map[string]map[string]any{}, userID: "acct-me", userName: "我"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeYX) serve(w http.ResponseWriter, r *http.Request) {
	f.count.Add(1)
	f.mu.Lock()
	f.tokens = append(f.tokens, r.Header.Get(yunxiao.HeaderToken))
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	fail := f.failCode
	f.mu.Unlock()
	if fail != 0 {
		w.WriteHeader(fail)
		_, _ = io.WriteString(w, `<html><body>`+yxSentinel+`</body></html>`)
		return
	}
	switch {
	case r.URL.Path == "/oapi/v1/platform/user":
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": f.userID, "name": f.userName})
	case strings.HasSuffix(r.URL.Path, "/comments"):
		_, _ = io.WriteString(w, `[]`)
	default:
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		f.mu.Lock()
		it, ok := f.items[id]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, yxSentinel)
			return
		}
		_ = json.NewEncoder(w).Encode(it)
	}
}

// set 设置工作项权威数据。
func (f *fakeYX) set(id string, assignee string, labels []string, status string, gmt int64, archived bool) {
	ls := []map[string]any{}
	for i, l := range labels {
		ls = append(ls, map[string]any{"id": fmt.Sprint("l", i), "name": l})
	}
	logical := "normal"
	if archived {
		logical = "archived"
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[id] = map[string]any{
		"id": id, "serialNumber": "SN-" + id, "subject": "标题 " + id,
		"status":     map[string]any{"id": "s-" + status, "name": status, "displayName": status},
		"assignedTo": map[string]any{"id": assignee, "name": "x"},
		"labels":     ls, "logicalStatus": logical, "gmtModified": gmt,
	}
}

func (f *fakeYX) setFail(code int) {
	f.mu.Lock()
	f.failCode = code
	f.mu.Unlock()
}

func (f *fakeYX) lastToken() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tokens) == 0 {
		return ""
	}
	return f.tokens[len(f.tokens)-1]
}

// clientFor 把任何接入点都指向假云效（生产里是 yunxiao.NewClient）。
// 允许列表校验仍按生产口径做，然后才换成 httptest 地址 —— 这样
// 「管理员撤掉某主机后下一次调用被拒」在 API 层也测得到。
func (f *fakeYX) clientFor(cfg yunxiao.Config) (*yunxiao.Client, error) {
	if _, err := yunxiao.ValidateEndpoint(cfg.Endpoint, cfg.AllowedHosts); err != nil {
		return nil, err
	}
	cfg.Endpoint = f.srv.URL
	return yunxiao.NewClientWithHTTP(cfg, f.srv.Client(), true)
}

// ---------------------------------------------------------------- 假入队器

type fakeYXEnqueuer struct {
	mu        sync.Mutex
	enqueued  []string // workitem id
	owners    []int64
	cancelled []string
	err       error
	nextID    int64
}

func (f *fakeYXEnqueuer) EnqueueYunxiao(ctx context.Context, owner int64, id, serial string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.enqueued = append(f.enqueued, id)
	f.owners = append(f.owners, owner)
	f.nextID++
	return f.nextID, nil
}

func (f *fakeYXEnqueuer) CancelForYunxiao(ctx context.Context, owner int64, key string) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, key)
	return []int64{7}, nil
}

func (f *fakeYXEnqueuer) snapshot() (enq []string, can []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.enqueued...), append([]string(nil), f.cancelled...)
}

// ---------------------------------------------------------------- 夹具

func yxSealer(t *testing.T) secret.Sealer {
	t.Helper()
	key := make([]byte, secret.KeySize)
	for i := range key {
		key[i] = byte(255 - i)
	}
	s, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type yxEnv struct {
	st    *store.Store
	conns *store.YunxiaoConnections
	yx    *fakeYX
	enq   *fakeYXEnqueuer
	nonce string
}

func newYXEnv(t *testing.T) *yxEnv {
	t.Helper()
	st := testStoreForAPI(t)
	return &yxEnv{
		st: st, conns: st.NewYunxiaoConnections(yxSealer(t)), yx: newFakeYX(t),
		enq: &fakeYXEnqueuer{}, nonce: strconv.FormatInt(time.Now().UnixNano(), 36),
	}
}

// user 建一个带随机邮箱的用户（经 store.Users.Create，带 webhook_slug）。
func (e *yxEnv) user(t *testing.T, prefix, role string) *store.User {
	t.Helper()
	u, err := e.st.NewUsers().Create(context.Background(),
		fmt.Sprintf("%s-%s-%d@example.com", prefix, e.nonce, time.Now().UnixNano()), "", role)
	if err != nil {
		t.Fatalf("建用户: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.st.Pool().Exec(context.Background(), `DELETE FROM users WHERE id = $1`, u.ID)
	})
	return u
}

func (e *yxEnv) token(uid int64) string    { return fmt.Sprintf("yx-pat-%s-%d", e.nonce, uid) }
func (e *yxEnv) whSecret(uid int64) string { return fmt.Sprintf("yx-whsec-%s-%d", e.nonce, uid) }

// connect 直接经 store 给用户配好连接（已验证、account_id=acct-me）。
func (e *yxEnv) connect(t *testing.T, uid int64, label, cancel string) {
	t.Helper()
	ctx := context.Background()
	if err := e.conns.Save(ctx, uid, store.YunxiaoSaveParams{
		Endpoint: yunxiao.DefaultEndpoint, OrganizationID: "org1",
		Token: e.token(uid), WebhookSecret: e.whSecret(uid), TriggerLabel: label, StatusCancel: cancel,
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.conns.MarkVerified(ctx, uid, "acct-me", "我"); err != nil {
		t.Fatal(err)
	}
}

func (e *yxEnv) resolver() *YunxiaoResolver {
	return &YunxiaoResolver{Users: e.st.NewUsers(), Conns: e.conns, Store: e.st, ClientFor: e.yx.clientFor}
}

// webhookServer 用真实 resolver + 真实快照 + 真实去重表，入队器是假件。
func (e *yxEnv) webhookServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("POST /webhooks/yunxiao/{slug}", &YunxiaoWebhook{
		Resolver: e.resolver(), Deliveries: e.st, Triggers: e.conns, Tasks: e.enq,
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func yxPost(t *testing.T, url, secretHdr, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if secretHdr != "" {
		req.Header.Set(yunxiao.HeaderSignature, secretHdr)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var m map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &m)
	if m == nil {
		m = map[string]any{"_raw": string(raw)}
	}
	return resp.StatusCode, m
}

func cleanDeliveries(t *testing.T, st *store.Store, uid int64) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(),
			`DELETE FROM webhook_deliveries WHERE delivery_id LIKE $1`, fmt.Sprintf("yunxiao:%d:%%", uid))
	})
}

// ---------------------------------------------------------------- AC-7

func TestYunxiaoWebhook(t *testing.T) {
	e := newYXEnv(t)
	srv := e.webhookServer(t)
	ctx := context.Background()

	owner := e.user(t, "yx-wh", store.RoleMember)
	cleanDeliveries(t, e.st, owner.ID)
	e.connect(t, owner.ID, "lathe:go", "已取消")
	url := srv.URL + "/webhooks/yunxiao/" + owner.WebhookSlug
	sec := e.whSecret(owner.ID)
	gmt := time.Now().UnixMilli()
	tick := func() int64 { gmt++; return gmt }
	wid := func(name string) string { return "wi" + e.nonce + name }
	deliver := func(t *testing.T, id string) (int, map[string]any) {
		t.Helper()
		return yxPost(t, url, sec, `{"id":"`+id+`","subject":"载荷里的字段一律不信"}`)
	}
	wantStatus := func(t *testing.T, code int, body map[string]any, wantCode int, want string) {
		t.Helper()
		if code != wantCode || body["status"] != want {
			t.Errorf("got %d %v want %d status=%s", code, body, wantCode, want)
		}
	}
	enqueuedCount := func(id string) int {
		enq, _ := e.enq.snapshot()
		n := 0
		for _, x := range enq {
			if x == id {
				n++
			}
		}
		return n
	}

	t.Run("unknown_slug_404", func(t *testing.T) {
		code, _ := yxPost(t, srv.URL+"/webhooks/yunxiao/no-such-slug-"+e.nonce, sec, `{"id":"x"}`)
		if code != http.StatusNotFound {
			t.Errorf("got %d want 404", code)
		}
	})
	t.Run("disabled_user_404", func(t *testing.T) {
		u := e.user(t, "yx-wh-disabled", store.RoleMember)
		e.connect(t, u.ID, "", "")
		if err := e.st.NewUsers().SetDisabled(ctx, u.ID, true); err != nil {
			t.Fatal(err)
		}
		code, _ := yxPost(t, srv.URL+"/webhooks/yunxiao/"+u.WebhookSlug, e.whSecret(u.ID), `{"id":"x"}`)
		if code != http.StatusNotFound {
			t.Errorf("got %d want 404", code)
		}
	})
	t.Run("no_webhook_secret_404", func(t *testing.T) {
		u := e.user(t, "yx-wh-nosec", store.RoleMember)
		if err := e.conns.Save(ctx, u.ID, store.YunxiaoSaveParams{Endpoint: yunxiao.DefaultEndpoint, Token: e.token(u.ID)}); err != nil {
			t.Fatal(err)
		}
		code, _ := yxPost(t, srv.URL+"/webhooks/yunxiao/"+u.WebhookSlug, "anything-at-all-123", `{"id":"x"}`)
		if code != http.StatusNotFound {
			t.Errorf("got %d want 404", code)
		}
		// 未配置连接同样 404
		u2 := e.user(t, "yx-wh-noconn", store.RoleMember)
		code, _ = yxPost(t, srv.URL+"/webhooks/yunxiao/"+u2.WebhookSlug, "anything-at-all-123", `{"id":"x"}`)
		if code != http.StatusNotFound {
			t.Errorf("未配置连接 got %d want 404", code)
		}
	})
	t.Run("bad_secret_401", func(t *testing.T) {
		before := e.yx.count.Load()
		for _, h := range []string{"", sec + "x", "wrong"} {
			code, _ := yxPost(t, url, h, `{"id":"x"}`)
			if code != http.StatusUnauthorized {
				t.Errorf("header=%q got %d want 401", h, code)
			}
		}
		if got := e.yx.count.Load() - before; got != 0 {
			t.Errorf("验签失败不应回源，上游请求 got %d want 0", got)
		}
	})
	t.Run("missing_id_400", func(t *testing.T) {
		for _, b := range []string{`{}`, `{"id":"a/b"}`, `not-json`} {
			code, _ := yxPost(t, url, sec, b)
			if code != http.StatusBadRequest {
				t.Errorf("body=%s got %d want 400", b, code)
			}
		}
	})
	t.Run("upstream_fail_503_not_recorded", func(t *testing.T) {
		id := wid("up")
		e.yx.set(id, "acct-me", nil, "待处理", tick(), false)
		e.yx.setFail(http.StatusInternalServerError)
		code, body := deliver(t, id)
		e.yx.setFail(0)
		if code != http.StatusServiceUnavailable {
			t.Errorf("got %d want 503", code)
		}
		if b, _ := json.Marshal(body); bytes.Contains(b, []byte(yxSentinel)) {
			t.Errorf("响应回显了上游响应体: %s", b)
		}
		var n int
		_ = e.st.Pool().QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries WHERE delivery_id LIKE $1`,
			fmt.Sprintf("yunxiao:%d:%s:%%", owner.ID, id)).Scan(&n)
		if n != 0 {
			t.Errorf("回源失败不应登记投递，got %d 行", n)
		}
		// 对端重投后能正常处理
		code, body = deliver(t, id)
		wantStatus(t, code, body, 200, "queued")
	})
	t.Run("first_seen_assigned_queued_once", func(t *testing.T) {
		id := wid("first")
		e.yx.set(id, "acct-me", nil, "待处理", tick(), false)
		code, body := deliver(t, id)
		wantStatus(t, code, body, 200, "queued")
		if got := enqueuedCount(id); got != 1 {
			t.Errorf("入队次数 got %d want 1", got)
		}
		// 两条自动化规则（字段变更 + 状态变更）几乎同时投递同一版本：去重
		code, body = deliver(t, id)
		wantStatus(t, code, body, 200, "duplicate")
		if got := enqueuedCount(id); got != 1 {
			t.Errorf("入队次数 got %d want 1", got)
		}
	})
	t.Run("redelivery_duplicate", func(t *testing.T) {
		id := wid("redeliv")
		e.yx.set(id, "someone-else", nil, "待处理", tick(), false)
		code, body := deliver(t, id)
		wantStatus(t, code, body, 200, "ignored")
		code, body = deliver(t, id)
		wantStatus(t, code, body, 200, "duplicate")
	})
	t.Run("title_edit_still_assigned_ignored", func(t *testing.T) {
		id := wid("title")
		e.yx.set(id, "acct-me", nil, "待处理", tick(), false)
		code, body := deliver(t, id)
		wantStatus(t, code, body, 200, "queued")
		e.yx.set(id, "acct-me", nil, "待处理", tick(), false) // 改了标题：gmtModified 变、指派没变
		code, body = deliver(t, id)
		wantStatus(t, code, body, 200, "ignored")
		if got := enqueuedCount(id); got != 1 {
			t.Errorf("入队次数 got %d want 1", got)
		}
	})
	t.Run("unassign_then_reassign_queued", func(t *testing.T) {
		id := wid("reassign")
		e.yx.set(id, "acct-me", nil, "待处理", tick(), false)
		deliver(t, id)
		e.yx.set(id, "", nil, "待处理", tick(), false)
		code, body := deliver(t, id)
		wantStatus(t, code, body, 200, "ignored")
		e.yx.set(id, "acct-me", nil, "待处理", tick(), false)
		code, body = deliver(t, id)
		wantStatus(t, code, body, 200, "queued")
		if got := enqueuedCount(id); got != 2 {
			t.Errorf("入队次数 got %d want 2", got)
		}
	})
	t.Run("label_rise_queued", func(t *testing.T) {
		id := wid("label")
		e.yx.set(id, "someone-else", []string{"别的"}, "待处理", tick(), false)
		code, body := deliver(t, id)
		wantStatus(t, code, body, 200, "ignored")
		e.yx.set(id, "someone-else", []string{"别的", " LATHE:GO "}, "待处理", tick(), false)
		code, body = deliver(t, id)
		wantStatus(t, code, body, 200, "queued")
	})
	t.Run("label_trigger_off_ignored", func(t *testing.T) {
		u := e.user(t, "yx-wh-nolabel", store.RoleMember)
		cleanDeliveries(t, e.st, u.ID)
		e.connect(t, u.ID, "", "")
		id := wid("labeloff")
		e.yx.set(id, "someone-else", []string{"lathe:go"}, "待处理", tick(), false)
		code, body := yxPost(t, srv.URL+"/webhooks/yunxiao/"+u.WebhookSlug, e.whSecret(u.ID), `{"id":"`+id+`"}`)
		wantStatus(t, code, body, 200, "ignored")
	})
	t.Run("cancel_status_calls_cancel", func(t *testing.T) {
		id := wid("cancel")
		e.yx.set(id, "acct-me", nil, "已取消", tick(), false)
		code, body := deliver(t, id)
		wantStatus(t, code, body, 200, "cancelled")
		_, can := e.enq.snapshot()
		if len(can) == 0 || can[len(can)-1] != "SN-"+id {
			t.Errorf("CancelForYunxiao got %v want 末项 SN-%s", can, id)
		}
		if got := enqueuedCount(id); got != 0 {
			t.Errorf("取消状态不应接单，入队 got %d", got)
		}
	})
	t.Run("forged_payload_assignee_ignored", func(t *testing.T) {
		id := wid("forged")
		e.yx.set(id, "someone-else", nil, "待处理", tick(), false)
		code, body := yxPost(t, url, sec, `{"id":"`+id+`","assignedTo":{"id":"acct-me"},"labels":[{"name":"lathe:go"}]}`)
		wantStatus(t, code, body, 200, "ignored")
		if got := enqueuedCount(id); got != 0 {
			t.Errorf("伪造载荷不应接单，入队 got %d", got)
		}
	})
	t.Run("archived_ignored", func(t *testing.T) {
		id := wid("archived")
		e.yx.set(id, "acct-me", nil, "待处理", tick(), true)
		code, body := deliver(t, id)
		wantStatus(t, code, body, 200, "ignored")
		if got := enqueuedCount(id); got != 0 {
			t.Errorf("归档工作项不应接单，入队 got %d", got)
		}
	})
}

// ---------------------------------------------------------------- API 夹具

// yxAPIServer 搭一套 YunxiaoAPI + 任务详情接口，身份走真实会话（cookie）。
type yxAPIServer struct {
	*yxEnv
	srv *httptest.Server
	ses *store.Sessions
}

func newYXAPIServer(t *testing.T, e *yxEnv) *yxAPIServer {
	t.Helper()
	users := e.st.NewUsers()
	ses := e.st.NewSessions()
	a := NewAuth("").WithStore(users, ses, nil, false)
	mux := http.NewServeMux()
	(&YunxiaoAPI{Conns: e.conns, Store: e.st, Auth: a, Tasks: e.enq, ClientFor: e.yx.clientFor}).Routes(mux)
	(&API{Store: e.st, Auth: a}).Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &yxAPIServer{yxEnv: e, srv: srv, ses: ses}
}

// session 给用户开一个会话，返回 cookie 值。
func (s *yxAPIServer) session(t *testing.T, uid int64) string {
	t.Helper()
	tok := fmt.Sprintf("yx-sess-%s-%d-%d", s.nonce, uid, time.Now().UnixNano())
	if err := s.ses.Create(context.Background(), uid, tok, time.Hour); err != nil {
		t.Fatal(err)
	}
	return tok
}

func (s *yxAPIServer) call(t *testing.T, sess, method, path, body string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, s.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if sess != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func jsonMap(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("响应不是 JSON: %q", s)
	}
	return m
}

func (s *yxAPIServer) saveBody(uid int64, extra map[string]any) string {
	m := map[string]any{
		"endpoint": yunxiao.DefaultEndpoint, "organizationId": "org1",
		"token": s.token(uid), "webhookSecret": s.whSecret(uid),
	}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func clearAllowedHosts(t *testing.T, st *store.Store) {
	t.Helper()
	reset := func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM system_settings WHERE key = $1`, store.SettingYunxiaoAllowedHosts)
	}
	reset()
	t.Cleanup(reset)
}

// ---------------------------------------------------------------- AC-9（API）

func TestYunxiaoAPINoPlaintext(t *testing.T) {
	s := newYXAPIServer(t, newYXEnv(t))
	u := s.user(t, "yx-api-plain", store.RoleMember)
	sess := s.session(t, u.ID)
	if code, body := s.call(t, sess, "PUT", "/api/yunxiao/connection", s.saveBody(u.ID, nil)); code != 200 {
		t.Fatalf("PUT got %d %s want 200", code, body)
	}
	code, body := s.call(t, sess, "GET", "/api/yunxiao/connection", "")
	if code != 200 {
		t.Fatalf("GET got %d", code)
	}
	for _, frag := range []string{s.token(u.ID), s.whSecret(u.ID)} {
		if strings.Contains(body, frag) {
			t.Errorf("GET 响应含明文 %q: %s", frag, body)
		}
	}
	m := jsonMap(t, body)
	if m["configured"] != true || m["tokenMasked"] != secret.Mask(s.token(u.ID)) || m["hasWebhookSecret"] != true {
		t.Errorf("GET got %v want 掩码与 configured", m)
	}
	if want := "/webhooks/yunxiao/" + u.WebhookSlug; m["webhookPath"] != want {
		t.Errorf("webhookPath got %v want %s", m["webhookPath"], want)
	}
	if m["accountName"] != "我" {
		t.Errorf("保存后应立即验证并回写账号名，got %v", m["accountName"])
	}
}

// ---------------------------------------------------------------- AC-10

func TestYunxiaoAPIRequiresAuth(t *testing.T) {
	s := newYXAPIServer(t, newYXEnv(t))
	cases := []struct{ name, method, path string }{
		{"get_connection", "GET", "/api/yunxiao/connection"},
		{"put_connection", "PUT", "/api/yunxiao/connection"},
		{"verify_connection", "POST", "/api/yunxiao/connection/verify"},
		{"delete_connection", "DELETE", "/api/yunxiao/connection"},
		{"run_workitem", "POST", "/api/yunxiao/workitems/abc/run"},
		{"admin_hosts_get", "GET", "/api/admin/yunxiao/allowed-hosts"},
		{"admin_hosts_put", "PUT", "/api/admin/yunxiao/allowed-hosts"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if code, body := s.call(t, "", c.method, c.path, "{}"); code != http.StatusUnauthorized {
				t.Errorf("未登录 got %d %s want 401", code, body)
			}
		})
	}
	t.Run("admin_hosts_member_403", func(t *testing.T) {
		u := s.user(t, "yx-auth-member", store.RoleMember)
		sess := s.session(t, u.ID)
		for _, m := range []string{"GET", "PUT"} {
			if code, _ := s.call(t, sess, m, "/api/admin/yunxiao/allowed-hosts", `{"hosts":[]}`); code != http.StatusForbidden {
				t.Errorf("%s 成员访问管理员接口 got %d want 403", m, code)
			}
		}
	})
}

func TestYunxiaoAPIIsolation(t *testing.T) {
	e := newYXEnv(t)
	s := newYXAPIServer(t, e)
	a := s.user(t, "yx-iso-a", store.RoleMember)
	b := s.user(t, "yx-iso-b", store.RoleMember)
	sa, sb := s.session(t, a.ID), s.session(t, b.ID)
	aSaved, aSaveBody := s.call(t, sa, "PUT", "/api/yunxiao/connection", s.saveBody(a.ID, nil))
	requireASaved := func(t *testing.T) {
		t.Helper()
		if aSaved != 200 {
			t.Fatalf("A 保存 got %d %s want 200", aSaved, aSaveBody)
		}
	}

	t.Run("b_get_sees_unconfigured", func(t *testing.T) {
		requireASaved(t)
		code, body := s.call(t, sb, "GET", "/api/yunxiao/connection", "")
		if code != 200 || jsonMap(t, body)["configured"] != false {
			t.Errorf("B GET got %d %s want configured:false", code, body)
		}
	})
	t.Run("b_delete_keeps_a_connection", func(t *testing.T) {
		requireASaved(t)
		if code, _ := s.call(t, sb, "DELETE", "/api/yunxiao/connection", ""); code != http.StatusNoContent {
			t.Errorf("B DELETE got %d want 204", code)
		}
		if _, err := e.conns.Get(context.Background(), a.ID); err != nil {
			t.Errorf("B 删除后 A 的连接应还在，got %v", err)
		}
	})
	t.Run("b_cannot_read_a_task_404", func(t *testing.T) {
		ctx := context.Background()
		var repoID, taskID int64
		if err := e.st.Pool().QueryRow(ctx, `INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
			a.ID, "acme/yx-iso-"+e.nonce).Scan(&repoID); err != nil {
			t.Fatal(err)
		}
		if err := e.st.Pool().QueryRow(ctx, `
			INSERT INTO tasks (user_id, repo_id, external_key, external_id, tracker_provider)
			VALUES ($1,$2,$3,$3,'yunxiao') RETURNING id`, a.ID, repoID, "YX-ISO-"+e.nonce).Scan(&taskID); err != nil {
			t.Fatal(err)
		}
		path := "/api/tasks/" + strconv.FormatInt(taskID, 10)
		if code, _ := s.call(t, sa, "GET", path, ""); code != 200 {
			t.Errorf("A 读自己的任务 got %d want 200", code)
		}
		if code, _ := s.call(t, sb, "GET", path, ""); code != http.StatusNotFound {
			t.Errorf("B 读 A 的任务 got %d want 404", code)
		}
	})
}

// ---------------------------------------------------------------- AC-11

func TestYunxiaoAPISaveValidation(t *testing.T) {
	e := newYXEnv(t)
	s := newYXAPIServer(t, e)
	clearAllowedHosts(t, e.st)
	const region = "devops.cn-hangzhou.yunxiao-test.example"
	if err := e.st.SetYunxiaoAllowedHosts(context.Background(), []string{region}); err != nil {
		t.Fatal(err)
	}
	u := s.user(t, "yx-val", store.RoleMember)
	sess := s.session(t, u.ID)

	cases := []struct {
		name  string
		extra map[string]any
		want  int
	}{
		{"http_scheme_400", map[string]any{"endpoint": "http://openapi-rdc.aliyuncs.com"}, 400},
		{"localhost_400", map[string]any{"endpoint": "https://localhost"}, 400},
		{"loopback_literal_400", map[string]any{"endpoint": "https://127.0.0.1"}, 400},
		{"private_literal_400", map[string]any{"endpoint": "https://10.0.0.1"}, 400},
		{"ipv6_loopback_400", map[string]any{"endpoint": "https://[::1]"}, 400},
		{"host_not_allowlisted_400", map[string]any{"endpoint": "https://evil.example.com"}, 400},
		{"userinfo_400", map[string]any{"endpoint": "https://u:p@openapi-rdc.aliyuncs.com"}, 400},
		{"non_443_port_400", map[string]any{"endpoint": "https://openapi-rdc.aliyuncs.com:8443"}, 400},
		{"org_slash_400", map[string]any{"organizationId": "a/b"}, 400},
		{"cancel_equals_merged_400", map[string]any{"statusMerged": "已完成", "statusCancel": " 已完成 "}, 400},
		{"label_too_long_400", map[string]any{"triggerLabel": strings.Repeat("a", 201)}, 400},
		{"secret_too_short_400", map[string]any{"webhookSecret": "short-123"}, 400},
		{"center_valid_200", map[string]any{"statusStarted": "处理中", "statusCancel": "已取消", "triggerLabel": "lathe:go"}, 200},
		{"allowlisted_region_valid_200", map[string]any{"endpoint": "https://" + region, "organizationId": ""}, 200},
	}
	// 200 的子用例排在最后：在它们之前，任何 400 都不应留下连接行。
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := s.call(t, sess, "PUT", "/api/yunxiao/connection", s.saveBody(u.ID, c.extra))
			if code != c.want {
				t.Errorf("got %d %s want %d", code, body, c.want)
			}
			if c.want == http.StatusBadRequest {
				if _, err := e.conns.Get(context.Background(), u.ID); !errors.Is(err, store.ErrYunxiaoNotConfigured) {
					t.Errorf("400 时不应保存，Get got %v want ErrYunxiaoNotConfigured", err)
				}
			}
		})
	}
}

// ---------------------------------------------------------------- AC-24（API）

func TestYunxiaoAPIEndpointAllowlist(t *testing.T) {
	e := newYXEnv(t)
	s := newYXAPIServer(t, e)
	clearAllowedHosts(t, e.st)
	admin := s.user(t, "yx-allow-admin", store.RoleAdmin)
	member := s.user(t, "yx-allow-member", store.RoleMember)
	sa, sm := s.session(t, admin.ID), s.session(t, member.ID)
	const region = "devops.cn-beijing.yunxiao-test.example"

	t.Run("admin_registers_region_host_then_member_saves", func(t *testing.T) {
		body := s.saveBody(member.ID, map[string]any{"endpoint": "https://" + region, "organizationId": ""})
		if code, _ := s.call(t, sm, "PUT", "/api/yunxiao/connection", body); code != 400 {
			t.Errorf("登记前 got %d want 400", code)
		}
		if code, b := s.call(t, sa, "PUT", "/api/admin/yunxiao/allowed-hosts", `{"hosts":["  DevOps.CN-Beijing.yunxiao-test.example. "]}`); code != 200 {
			t.Fatalf("管理员登记 got %d %s", code, b)
		}
		code, b := s.call(t, sa, "GET", "/api/admin/yunxiao/allowed-hosts", "")
		if code != 200 || !strings.Contains(b, `"`+region+`"`) {
			t.Errorf("GET hosts got %d %s want 含规范化后的 %s", code, b, region)
		}
		if code, b := s.call(t, sm, "PUT", "/api/yunxiao/connection", body); code != 200 {
			t.Errorf("登记后 got %d %s want 200", code, b)
		}
	})
	t.Run("admin_removal_blocks_next_call", func(t *testing.T) {
		if code, _ := s.call(t, sm, "POST", "/api/yunxiao/connection/verify", ""); code != 200 {
			t.Fatalf("撤销前 verify got %d", code)
		}
		if code, _ := s.call(t, sa, "PUT", "/api/admin/yunxiao/allowed-hosts", `{"hosts":[]}`); code != 200 {
			t.Fatal("撤销失败")
		}
		before := e.yx.count.Load()
		code, b := s.call(t, sm, "POST", "/api/yunxiao/connection/verify", "")
		m := jsonMap(t, b)
		v, _ := m["verify"].(map[string]any)
		if code != 200 || v["ok"] != false || !strings.Contains(fmt.Sprint(v["error"]), "允许列表") {
			t.Errorf("撤销后 verify got %d %s want ok:false 且提示允许列表", code, b)
		}
		if got := e.yx.count.Load() - before; got != 0 {
			t.Errorf("撤销后不应发出站请求，got %d", got)
		}
		code, b = s.call(t, sm, "POST", "/api/yunxiao/workitems/wi1/run", "")
		if code != http.StatusBadGateway {
			t.Errorf("撤销后 /run got %d %s want 502", code, b)
		}
	})
	t.Run("allowlist_rejects_ip_and_localhost", func(t *testing.T) {
		for _, h := range []string{"127.0.0.1", "10.0.0.1", "[::1]", "::1", "localhost", "yx.localhost", "*.aliyuncs.com", "a.example.com:8443", ""} {
			body, _ := json.Marshal(map[string]any{"hosts": []string{h}})
			if code, _ := s.call(t, sa, "PUT", "/api/admin/yunxiao/allowed-hosts", string(body)); code != 400 {
				t.Errorf("host=%q got %d want 400", h, code)
			}
		}
		many := make([]string, 51)
		for i := range many {
			many[i] = fmt.Sprintf("h%d.example.com", i)
		}
		body, _ := json.Marshal(map[string]any{"hosts": many})
		if code, _ := s.call(t, sa, "PUT", "/api/admin/yunxiao/allowed-hosts", string(body)); code != 400 {
			t.Errorf("51 项 got %d want 400", code)
		}
	})
}

func TestYunxiaoAPINoBodyEcho(t *testing.T) {
	e := newYXEnv(t)
	s := newYXAPIServer(t, e)
	u := s.user(t, "yx-echo", store.RoleMember)
	sess := s.session(t, u.ID)
	e.connect(t, u.ID, "", "")
	e.yx.setFail(http.StatusInternalServerError)

	t.Run("verify_response", func(t *testing.T) {
		code, b := s.call(t, sess, "POST", "/api/yunxiao/connection/verify", "")
		if code != 200 || strings.Contains(b, yxSentinel) || !strings.Contains(b, "500") {
			t.Errorf("got %d %s want 200、含 500、不含哨兵", code, b)
		}
	})
	t.Run("verify_error_column", func(t *testing.T) {
		var ve *string
		if err := e.st.Pool().QueryRow(context.Background(),
			`SELECT verify_error FROM yunxiao_connections WHERE user_id = $1`, u.ID).Scan(&ve); err != nil {
			t.Fatal(err)
		}
		if ve == nil || strings.Contains(*ve, yxSentinel) || !strings.Contains(*ve, "500") {
			t.Errorf("verify_error got %v want 含 500、不含哨兵", ve)
		}
	})
	t.Run("run_502_response", func(t *testing.T) {
		code, b := s.call(t, sess, "POST", "/api/yunxiao/workitems/wi1/run", "")
		if code != http.StatusBadGateway || strings.Contains(b, yxSentinel) {
			t.Errorf("got %d %s want 502 且不含哨兵", code, b)
		}
	})
}

// ---------------------------------------------------------------- AC-25

func TestYunxiaoAPIRun(t *testing.T) {
	e := newYXEnv(t)
	s := newYXAPIServer(t, e)
	u := s.user(t, "yx-run", store.RoleMember)
	sess := s.session(t, u.ID)
	ctx := context.Background()

	triggerRows := func(t *testing.T) int {
		t.Helper()
		var n int
		_ = e.st.Pool().QueryRow(ctx, `SELECT count(*) FROM yunxiao_trigger_state WHERE user_id = $1`, u.ID).Scan(&n)
		return n
	}

	t.Run("unconfigured_400", func(t *testing.T) {
		before := e.yx.count.Load()
		if code, b := s.call(t, sess, "POST", "/api/yunxiao/workitems/wi1/run", ""); code != 400 || !strings.Contains(b, "云效连接") {
			t.Errorf("got %d %s want 400 请先配置云效连接", code, b)
		}
		if e.yx.count.Load() != before {
			t.Error("未配置不应发出站请求")
		}
	})

	e.connect(t, u.ID, "", "")
	for _, c := range []struct{ name, id string }{
		{"invalid_id_slash_400_no_upstream", "a%2Fb"},
		{"invalid_id_dotdot_400_no_upstream", "%2E%2E"},
		{"invalid_id_too_long_400_no_upstream", strings.Repeat("a", 65)},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := e.yx.count.Load()
			if code, b := s.call(t, sess, "POST", "/api/yunxiao/workitems/"+c.id+"/run", ""); code != 400 {
				t.Errorf("got %d %s want 400", code, b)
			}
			if got := e.yx.count.Load() - before; got != 0 {
				t.Errorf("上游请求 got %d want 0", got)
			}
		})
	}
	t.Run("upstream_404_502_no_task", func(t *testing.T) {
		enqBefore, _ := e.enq.snapshot()
		code, b := s.call(t, sess, "POST", "/api/yunxiao/workitems/missing1/run", "")
		if code != http.StatusBadGateway || strings.Contains(b, yxSentinel) || !strings.Contains(b, "404") {
			t.Errorf("got %d %s want 502、含 404、不含哨兵", code, b)
		}
		if enq, _ := e.enq.snapshot(); len(enq) != len(enqBefore) {
			t.Error("上游失败不应入队")
		}
	})
	t.Run("upstream_500_502_no_echo", func(t *testing.T) {
		e.yx.setFail(500)
		defer e.yx.setFail(0)
		code, b := s.call(t, sess, "POST", "/api/yunxiao/workitems/wi1/run", "")
		if code != http.StatusBadGateway || strings.Contains(b, yxSentinel) {
			t.Errorf("got %d %s want 502 不含哨兵", code, b)
		}
	})
	t.Run("resolve_repo_fail_400", func(t *testing.T) {
		e.yx.set("wirepo", "acct-me", nil, "待处理", 1, false)
		e.enq.mu.Lock()
		e.enq.err = fmt.Errorf("%w：你的账号下没有仓库配置", ErrYunxiaoNoRepo)
		e.enq.mu.Unlock()
		defer func() { e.enq.mu.Lock(); e.enq.err = nil; e.enq.mu.Unlock() }()
		if code, b := s.call(t, sess, "POST", "/api/yunxiao/workitems/wirepo/run", ""); code != 400 || !strings.Contains(b, "仓库") {
			t.Errorf("got %d %s want 400 含原因", code, b)
		}
	})
	t.Run("task_active_409", func(t *testing.T) {
		e.yx.set("wiactive", "acct-me", nil, "待处理", 1, false)
		e.enq.mu.Lock()
		e.enq.err = ErrYunxiaoTaskActive
		e.enq.mu.Unlock()
		defer func() { e.enq.mu.Lock(); e.enq.err = nil; e.enq.mu.Unlock() }()
		if code, b := s.call(t, sess, "POST", "/api/yunxiao/workitems/wiactive/run", ""); code != http.StatusConflict {
			t.Errorf("got %d %s want 409", code, b)
		}
	})
	t.Run("no_trigger_state_write", func(t *testing.T) {
		e.yx.set("wiok", "acct-me", nil, "待处理", 1, false)
		before := triggerRows(t)
		code, b := s.call(t, sess, "POST", "/api/yunxiao/workitems/wiok/run", "")
		if code != http.StatusCreated {
			t.Fatalf("got %d %s want 201", code, b)
		}
		m := jsonMap(t, b)
		if m["externalKey"] != "SN-wiok" || m["taskId"] == nil {
			t.Errorf("响应 got %v want externalKey=SN-wiok 与 taskId", m)
		}
		if got := triggerRows(t); got != before {
			t.Errorf("/run 不应写 yunxiao_trigger_state，行数 %d → %d", before, got)
		}
	})
}

// ---------------------------------------------------------------- AC-26

func TestYunxiaoAPIDeleteClearsTriggerState(t *testing.T) {
	e := newYXEnv(t)
	s := newYXAPIServer(t, e)
	ctx := context.Background()
	u := s.user(t, "yx-del", store.RoleMember)
	other := s.user(t, "yx-del-other", store.RoleMember)
	sess := s.session(t, u.ID)
	cleanDeliveries(t, e.st, u.ID)
	e.connect(t, u.ID, "", "")
	e.connect(t, other.ID, "", "")
	wid := "widel" + e.nonce
	for _, x := range []string{wid, wid + "b"} {
		if _, err := e.conns.ObserveTrigger(ctx, u.ID, x, true, false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.conns.ObserveTrigger(ctx, other.ID, wid, true, false); err != nil {
		t.Fatal(err)
	}
	count := func(uid int64) int {
		var n int
		_ = e.st.Pool().QueryRow(ctx, `SELECT count(*) FROM yunxiao_trigger_state WHERE user_id = $1`, uid).Scan(&n)
		return n
	}
	code, _ := s.call(t, sess, "DELETE", "/api/yunxiao/connection", "")

	t.Run("deletes_connection_row", func(t *testing.T) {
		if code != http.StatusNoContent {
			t.Errorf("DELETE got %d want 204", code)
		}
		if _, err := e.conns.Get(ctx, u.ID); !errors.Is(err, store.ErrYunxiaoNotConfigured) {
			t.Errorf("连接行应已删除，Get got %v", err)
		}
	})
	t.Run("clears_all_own_trigger_rows", func(t *testing.T) {
		if got := count(u.ID); got != 0 {
			t.Errorf("本人快照行 got %d want 0", got)
		}
	})
	t.Run("keeps_other_users_rows", func(t *testing.T) {
		if got := count(other.ID); got != 1 {
			t.Errorf("他人快照行 got %d want 1", got)
		}
	})
	t.Run("unconfigured_204", func(t *testing.T) {
		if code, _ := s.call(t, sess, "DELETE", "/api/yunxiao/connection", ""); code != http.StatusNoContent {
			t.Errorf("再次 DELETE got %d want 204", code)
		}
	})
	t.Run("reconnect_first_assigned_delivery_queued", func(t *testing.T) {
		if code, b := s.call(t, sess, "PUT", "/api/yunxiao/connection", s.saveBody(u.ID, nil)); code != 200 {
			t.Fatalf("重新连接 got %d %s", code, b)
		}
		e.yx.set(wid, "acct-me", nil, "待处理", time.Now().UnixMilli(), false)
		srv := e.webhookServer(t)
		code, body := yxPost(t, srv.URL+"/webhooks/yunxiao/"+u.WebhookSlug, e.whSecret(u.ID), `{"id":"`+wid+`"}`)
		if code != 200 || body["status"] != "queued" {
			t.Errorf("重连后首次已指派投递 got %d %v want queued", code, body)
		}
	})
}

// ---------------------------------------------------------------- AC-27

func TestYunxiaoAPIVerify(t *testing.T) {
	e := newYXEnv(t)
	s := newYXAPIServer(t, e)
	ctx := context.Background()
	u := s.user(t, "yx-verify", store.RoleMember)
	sess := s.session(t, u.ID)

	read := func(t *testing.T) (acct *string, at *time.Time, verr *string) {
		t.Helper()
		if err := e.st.Pool().QueryRow(ctx,
			`SELECT account_id, verified_at, verify_error FROM yunxiao_connections WHERE user_id = $1`, u.ID,
		).Scan(&acct, &at, &verr); err != nil {
			t.Fatal(err)
		}
		return
	}

	t.Run("unconfigured_400", func(t *testing.T) {
		if code, _ := s.call(t, sess, "POST", "/api/yunxiao/connection/verify", ""); code != 400 {
			t.Errorf("got %d want 400", code)
		}
	})

	if err := e.conns.Save(ctx, u.ID, store.YunxiaoSaveParams{Endpoint: yunxiao.DefaultEndpoint, OrganizationID: "org1", Token: e.token(u.ID)}); err != nil {
		t.Fatal(err)
	}
	if err := e.conns.MarkVerifyFailed(ctx, u.ID, "旧的失败原因"); err != nil {
		t.Fatal(err)
	}
	e.yx.mu.Lock()
	e.yx.userID, e.yx.userName = "acct-verified", "验证用户"
	e.yx.mu.Unlock()
	start := time.Now().Add(-time.Second) // 容忍库与本机的亚秒级时钟差
	code, body := s.call(t, sess, "POST", "/api/yunxiao/connection/verify", "")

	t.Run("success_writes_account_and_verified_at", func(t *testing.T) {
		v, _ := jsonMap(t, body)["verify"].(map[string]any)
		if code != 200 || v["ok"] != true || v["accountName"] != "验证用户" {
			t.Errorf("got %d %s want ok:true accountName=验证用户", code, body)
		}
		acct, at, _ := read(t)
		if acct == nil || *acct != "acct-verified" {
			t.Errorf("account_id got %v want acct-verified", acct)
		}
		if at == nil || at.Before(start) {
			t.Errorf("verified_at got %v want 晚于 %v", at, start)
		}
	})
	t.Run("success_clears_verify_error", func(t *testing.T) {
		if _, _, verr := read(t); verr != nil {
			t.Errorf("verify_error got %q want NULL", *verr)
		}
	})
	t.Run("uses_stored_token", func(t *testing.T) {
		if got := e.yx.lastToken(); got != e.token(u.ID) {
			t.Errorf("x-yunxiao-token got %q want 库里存的 %q", got, e.token(u.ID))
		}
	})

	e.yx.setFail(http.StatusUnauthorized)
	code2, body2 := s.call(t, sess, "POST", "/api/yunxiao/connection/verify", "")
	e.yx.setFail(0)

	t.Run("failure_writes_error_clears_verified_at", func(t *testing.T) {
		v, _ := jsonMap(t, body2)["verify"].(map[string]any)
		if code2 != 200 || v["ok"] != false || !strings.Contains(fmt.Sprint(v["error"]), "401") {
			t.Errorf("got %d %s want ok:false 含 401", code2, body2)
		}
		_, at, verr := read(t)
		if at != nil {
			t.Errorf("verified_at got %v want NULL", at)
		}
		if verr == nil || !strings.Contains(*verr, "令牌无效") {
			t.Errorf("verify_error got %v want 分类人话", verr)
		}
	})
	t.Run("failure_keeps_account_id", func(t *testing.T) {
		if acct, _, _ := read(t); acct == nil || *acct != "acct-verified" {
			t.Errorf("account_id got %v want 保留 acct-verified", acct)
		}
	})
}

// ---------------------------------------------------------------- AC-22（webhook/API）

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func captureLogs(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func TestYunxiaoNoSecretInLogs(t *testing.T) {
	t.Run("webhook_paths", func(t *testing.T) {
		buf := captureLogs(t)
		e := newYXEnv(t)
		srv := e.webhookServer(t)
		u := e.user(t, "yx-log-wh", store.RoleMember)
		cleanDeliveries(t, e.st, u.ID)
		e.connect(t, u.ID, "lathe:go", "已取消")
		url := srv.URL + "/webhooks/yunxiao/" + u.WebhookSlug
		sec := e.whSecret(u.ID)
		id := "wilog" + e.nonce
		yxPost(t, srv.URL+"/webhooks/yunxiao/nope-"+e.nonce, sec, `{"id":"x"}`)
		yxPost(t, url, sec+"-wrong", `{"id":"x"}`)
		yxPost(t, url, sec, `{}`)
		e.yx.setFail(500)
		yxPost(t, url, sec, `{"id":"`+id+`"}`)
		e.yx.setFail(0)
		e.yx.set(id, "acct-me", []string{"lathe:go"}, "待处理", 1, false)
		yxPost(t, url, sec, `{"id":"`+id+`"}`)
		yxPost(t, url, sec, `{"id":"`+id+`"}`)
		e.yx.set(id, "acct-me", nil, "已取消", 2, false)
		yxPost(t, url, sec, `{"id":"`+id+`"}`)
		e.enq.mu.Lock()
		e.enq.err = errors.New("入队失败")
		e.enq.mu.Unlock()
		e.yx.set(id+"x", "acct-me", nil, "待处理", 3, false)
		yxPost(t, url, sec, `{"id":"`+id+`x"}`)
		out := buf.String()
		if out == "" {
			t.Fatal("没捕获到日志")
		}
		for _, frag := range []string{e.token(u.ID), sec, yxSentinel} {
			if strings.Contains(out, frag) {
				t.Errorf("日志含敏感片段 %q", frag)
			}
		}
	})
	t.Run("api_paths", func(t *testing.T) {
		buf := captureLogs(t)
		e := newYXEnv(t)
		s := newYXAPIServer(t, e)
		u := s.user(t, "yx-log-api", store.RoleMember)
		sess := s.session(t, u.ID)
		s.call(t, sess, "PUT", "/api/yunxiao/connection", s.saveBody(u.ID, nil))
		s.call(t, sess, "PUT", "/api/yunxiao/connection", s.saveBody(u.ID, map[string]any{"endpoint": "http://x"}))
		s.call(t, sess, "GET", "/api/yunxiao/connection", "")
		e.yx.setFail(500)
		s.call(t, sess, "POST", "/api/yunxiao/connection/verify", "")
		s.call(t, sess, "POST", "/api/yunxiao/workitems/wi1/run", "")
		e.yx.setFail(0)
		s.call(t, sess, "DELETE", "/api/yunxiao/connection", "")
		out := buf.String()
		for _, frag := range []string{s.token(u.ID), s.whSecret(u.ID), yxSentinel} {
			if strings.Contains(out, frag) {
				t.Errorf("日志含敏感片段 %q", frag)
			}
		}
	})
}
