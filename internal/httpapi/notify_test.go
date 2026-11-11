package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/notify"
	"github.com/zichuanwangcloud-gif/lathe/internal/secret"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// ---------------------------------------------------------------- 夹具

var notifyT0 = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// fakeWebhook 是 TLS 假 Slack/飞书：计数、记请求体、按 respond 回响应。
type fakeWebhook struct {
	srv     *httptest.Server
	mu      sync.Mutex
	bodies  []string
	respond func(w http.ResponseWriter)
}

func newFakeWebhook(t *testing.T, respond func(w http.ResponseWriter)) *fakeWebhook {
	t.Helper()
	f := &fakeWebhook{respond: respond}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(b))
		resp := f.respond
		f.mu.Unlock()
		resp(w)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func feishuOK(w http.ResponseWriter) {
	_, _ = io.WriteString(w, `{"code":0,"msg":"success","data":{}}`)
}

func (f *fakeWebhook) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeWebhook) body(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[i]
}

func randHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type notifyFix struct {
	st     *store.Store
	nc     *store.NotifyChannels
	sender *notify.Sender
	api    *NotifyAPI
	srv    *httptestServer
	userID int64
	addr   string
}

// newNotifyFix 搭 NotifyAPI：真 NC + 真 Sender，白名单只放 addr（假服务器的 host:port）。
func newNotifyFix(t *testing.T, addr string, client *http.Client) *notifyFix {
	t.Helper()
	st := testStoreForAPI(t)
	key := make([]byte, secret.KeySize)
	_, _ = rand.Read(key)
	sealer, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}
	f := &notifyFix{st: st, addr: addr}
	f.nc = st.NewNotifyChannels(sealer, []string{addr})
	f.sender = notify.NewSender([]string{addr}, notify.HardenClient(client))
	f.userID = mustUser(t, st, "notify-"+strconv.FormatInt(time.Now().UnixNano(), 10)+"-"+randHex(t)+"@example.com")
	f.api, f.srv = f.serverFor(t, f.userID)
	return f
}

func (f *notifyFix) serverFor(t *testing.T, userID int64) (*NotifyAPI, *httptestServer) {
	t.Helper()
	api := &NotifyAPI{Channels: f.nc, Sender: f.sender, Auth: authAs(userID, "n@example.com")}
	mux := http.NewServeMux()
	api.Routes(mux)
	return api, newTestServer(t, mux)
}

func (f *notifyFix) mkTask(t *testing.T, userID int64, key string) int64 {
	t.Helper()
	ctx := context.Background()
	var repoID, id int64
	if err := f.st.Pool().QueryRow(ctx, `INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		userID, "acme/n-"+randHex(t)).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	if err := f.st.Pool().QueryRow(ctx, `INSERT INTO tasks (user_id, repo_id, external_key) VALUES ($1,$2,$3) RETURNING id`,
		userID, repoID, key).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

const allEventsJSON = `{"failed":true,"blockedSpec":true,"awaitingApproval":true,"prOpen":true,"prdReady":true}`

func putBody(url, secret string, extra string) string {
	m := map[string]any{"enabled": true}
	var ev map[string]any
	_ = json.Unmarshal([]byte(allEventsJSON), &ev)
	m["events"] = ev
	if url != "" {
		m["url"] = url
	}
	if secret != "" {
		m["secret"] = secret
	}
	b, _ := json.Marshal(m)
	s := string(b)
	if extra != "" {
		s = strings.TrimSuffix(s, "}") + "," + extra + "}"
	}
	return s
}

type deliveryState struct {
	Status    string
	Attempts  int
	LastError *string
	SentAt    *time.Time
	EventType string
}

func (f *notifyFix) delivery(t *testing.T, id int64) deliveryState {
	t.Helper()
	var d deliveryState
	if err := f.st.Pool().QueryRow(context.Background(),
		`SELECT status, attempts, last_error, sent_at, event_type FROM notify_deliveries WHERE id=$1`, id).Scan(
		&d.Status, &d.Attempts, &d.LastError, &d.SentAt, &d.EventType); err != nil {
		t.Fatalf("读投递 %d: %v", id, err)
	}
	return d
}

func (f *notifyFix) countDeliveries(t *testing.T, userID int64, where string) int {
	t.Helper()
	var n int
	if err := f.st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM notify_deliveries d
		JOIN notify_channels c ON c.id=d.channel_id WHERE c.user_id=$1 AND `+where, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// drainMine 循环 DrainOnce 直到本用户没有 pending/sending 行（3s 上限）。
func (f *notifyFix) drainMine(t *testing.T, w *notify.Worker) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for f.countDeliveries(t, f.userID, `d.status IN ('pending','sending')`) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("3s 内本用户投递没有处理完")
		}
		if err := w.DrainOnce(context.Background()); err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}
	}
}

func testResult(t *testing.T, v map[string]any) map[string]any {
	t.Helper()
	tr, _ := v["test"].(map[string]any)
	return tr
}

// ---------------------------------------------------------------- AC-18

func TestDeliveryIsolationBetweenUsers(t *testing.T) {
	hook := newFakeWebhook(t, func(w http.ResponseWriter) { w.WriteHeader(500) })
	f := newNotifyFix(t, hook.srv.Listener.Addr().String(), hook.srv.Client())
	resp := f.srv.do(t, "PUT", "/api/notify/channels/feishu", putBody(hook.srv.URL+"/hook", "", ""), true)
	v := f.srv.decode(t, resp)
	tr := testResult(t, v)
	if tr == nil {
		t.Fatalf("PUT 响应 got %v, want 带 test", v)
	}
	aID := int64(tr["deliveryId"].(float64))

	userB := mustUser(t, f.st, "notify-b-"+randHex(t)+"@example.com")
	_, srvB := f.serverFor(t, userB)

	raw := srvB.raw(t, srvB.do(t, "GET", "/api/notify/deliveries", "", true))
	if strings.Contains(raw, `"id":`+strconv.FormatInt(aID, 10)+",") {
		t.Errorf("B 看到了 A 的投递：%s", raw)
	}
	if resp := srvB.do(t, "POST", fmt.Sprintf("/api/notify/deliveries/%d/retry", aID), "", true); resp.StatusCode != http.StatusNotFound {
		t.Errorf("B 重试 A 的投递 got %d, want 404", resp.StatusCode)
	}
	if resp := srvB.do(t, "POST", "/api/notify/deliveries/999999999999/retry", "", true); resp.StatusCode != http.StatusNotFound {
		t.Errorf("不存在的投递 got %d, want 404", resp.StatusCode)
	}
	if resp := srvB.do(t, "POST", "/api/notify/channels/feishu/test", "", true); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("B 没配渠道时重发测试 got %d, want 400", resp.StatusCode)
	}
	if hook.count() != 1 {
		t.Errorf("假服务器请求数 got %d, want 1", hook.count())
	}
}

// ---------------------------------------------------------------- AC-25

func TestChannelSaveReportsTestFailureButKeepsRow(t *testing.T) {
	hook := newFakeWebhook(t, func(w http.ResponseWriter) { w.WriteHeader(500); _, _ = io.WriteString(w, "boom") })
	f := newNotifyFix(t, hook.srv.Listener.Addr().String(), hook.srv.Client())
	u := hook.srv.URL + "/hook/" + randHex(t)

	v := f.srv.decode(t, f.srv.do(t, "PUT", "/api/notify/channels/feishu", putBody(u, "sig", ""), true))
	tr := testResult(t, v)
	if v["saved"] != true || tr == nil || tr["ok"] != false {
		t.Fatalf("got %v, want saved:true test.ok:false", v)
	}
	first := int64(tr["deliveryId"].(float64))

	var testOK *bool
	var testErr *string
	if err := f.st.Pool().QueryRow(context.Background(), `SELECT test_ok, test_error FROM notify_channels WHERE user_id=$1 AND kind='feishu'`,
		f.userID).Scan(&testOK, &testErr); err != nil {
		t.Fatalf("渠道行应仍在: %v", err)
	}
	if testOK == nil || *testOK || testErr == nil || *testErr == "" || strings.Contains(*testErr, u) || strings.Contains(*testErr, "/hook") {
		t.Errorf("test_ok/test_error got %v/%v, want false/非空且不含 URL", testOK, testErr)
	}
	d := f.delivery(t, first)
	if d.Status != "gave_up" || d.Attempts != 1 || d.LastError == nil || !strings.HasPrefix(*d.LastError, "500") {
		t.Errorf("测试投递行 got %+v, want gave_up/1/500…", d)
	}

	t.Run("worker 不补发测试行", func(t *testing.T) {
		w := notify.NewWorker(f.nc, f.sender, "")
		w.Now = func() time.Time { return notifyT0 }
		if err := w.DrainOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if hook.count() != 1 {
			t.Errorf("请求数 got %d, want 1", hook.count())
		}
	})
	t.Run("测试行 /retry 返回 409", func(t *testing.T) {
		if resp := f.srv.do(t, "POST", fmt.Sprintf("/api/notify/deliveries/%d/retry", first), "", true); resp.StatusCode != http.StatusConflict {
			t.Errorf("got %d, want 409", resp.StatusCode)
		}
	})
	t.Run("重发测试：新 deliveryId、新一行", func(t *testing.T) {
		v := f.srv.decode(t, f.srv.do(t, "POST", "/api/notify/channels/feishu/test", "", true))
		tr := testResult(t, v)
		if tr == nil {
			t.Fatalf("got %v", v)
		}
		if id := int64(tr["deliveryId"].(float64)); id == first {
			t.Errorf("deliveryId got %d, want ≠ %d", id, first)
		}
		if hook.count() != 2 {
			t.Errorf("请求数 got %d, want 2", hook.count())
		}
		if n := f.countDeliveries(t, f.userID, `d.event_type='test'`); n != 2 {
			t.Errorf("test 行 got %d, want 2", n)
		}
	})
}

// ---------------------------------------------------------------- AC-25b

func TestChannelPutTogglesOnlyWithoutTestMessage(t *testing.T) {
	hook := newFakeWebhook(t, feishuOK)
	f := newNotifyFix(t, hook.srv.Listener.Addr().String(), hook.srv.Client())
	ctx := context.Background()

	encs := func(t *testing.T) (u, s []byte, evFailed, evPR bool) {
		t.Helper()
		if err := f.st.Pool().QueryRow(ctx, `SELECT url_enc, sign_enc, event_failed, event_pr_open FROM notify_channels
			WHERE user_id=$1 AND kind='feishu'`, f.userID).Scan(&u, &s, &evFailed, &evPR); err != nil {
			t.Fatal(err)
		}
		return
	}

	// ①
	v := f.srv.decode(t, f.srv.do(t, "PUT", "/api/notify/channels/feishu", putBody(hook.srv.URL+"/hook", "sig-1", ""), true))
	if tr := testResult(t, v); tr == nil || tr["ok"] != true || hook.count() != 1 {
		t.Fatalf("① got %v / 请求 %d, want test.ok=true / 1", v, hook.count())
	}
	u1, s1, _, _ := encs(t)

	// ②
	resp := f.srv.do(t, "PUT", "/api/notify/channels/feishu",
		`{"enabled":true,"events":{"failed":false,"blockedSpec":true,"awaitingApproval":true,"prOpen":true,"prdReady":true}}`, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("② status got %d", resp.StatusCode)
	}
	v = f.srv.decode(t, resp)
	if v["saved"] != true || v["test"] != nil {
		t.Errorf("② got %v, want saved:true test:null", v)
	}
	if hook.count() != 1 {
		t.Errorf("② 请求数 got %d, want 1（只改开关不发测试）", hook.count())
	}
	if n := f.countDeliveries(t, f.userID, `d.event_type='test'`); n != 1 {
		t.Errorf("② test 行 got %d, want 1", n)
	}
	u2, s2, evFailed, _ := encs(t)
	if evFailed || !bytes.Equal(u1, u2) || !bytes.Equal(s1, s2) {
		t.Errorf("② event_failed=%v url/sign 字节相等=%v/%v, want false/true/true", evFailed, bytes.Equal(u1, u2), bytes.Equal(s1, s2))
	}

	// ③
	taskA := f.mkTask(t, f.userID, "CR-25B-"+randHex(t))
	if n, err := f.nc.EnqueueTaskEvent(ctx, notify.TaskEvent{TaskID: taskA, UserID: f.userID, ToState: "failed", EventType: notify.EventFailed, At: notifyT0}); err != nil || n != 0 {
		t.Errorf("③ failed 入队 got (%d,%v), want 0", n, err)
	}
	if n, err := f.nc.EnqueueTaskEvent(ctx, notify.TaskEvent{TaskID: taskA, UserID: f.userID, ToState: "pr_open", EventType: notify.EventPROpen, At: notifyT0}); err != nil || n != 1 {
		t.Errorf("③ pr_open 入队 got (%d,%v), want 1", n, err)
	}

	// ④
	resp = f.srv.do(t, "PUT", "/api/notify/channels/feishu",
		`{"enabled":true,"events":{"failed":true,"blockedSpec":true,"awaitingApproval":true,"prdReady":true}}`, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("④ 缺 prOpen got %d, want 400", resp.StatusCode)
	}
	if raw := f.srv.raw(t, resp); !strings.Contains(raw, "prOpen") {
		t.Errorf("④ 错误应点名缺的键，got %s", raw)
	}
	if _, _, _, evPR := encs(t); !evPR {
		t.Error("④ event_pr_open 应仍为 true")
	}
	for _, body := range []string{`{"events":` + allEventsJSON + `}`, `{"enabled":true}`} {
		if resp := f.srv.do(t, "PUT", "/api/notify/channels/feishu", body, true); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("④ body %s got %d, want 400", body, resp.StatusCode)
		}
	}

	// ⑤
	v = f.srv.decode(t, f.srv.do(t, "PUT", "/api/notify/channels/feishu", putBody("", "", `"clearSecret":true`), true))
	if tr := testResult(t, v); tr == nil || tr["ok"] != true {
		t.Errorf("⑤ got %v, want 发了测试", v)
	}
	if hook.count() != 2 {
		t.Fatalf("⑤ 请求数 got %d, want 2", hook.count())
	}
	if strings.Contains(hook.body(1), `"sign"`) {
		t.Errorf("⑤ 清签名后请求体不该带 sign：%s", hook.body(1))
	}

	// ⑥
	v = f.srv.decode(t, f.srv.do(t, "GET", "/api/notify/channels", "", true))
	chans, _ := v["channels"].([]any)
	var feishu map[string]any
	for _, c := range chans {
		if m := c.(map[string]any); m["kind"] == "feishu" {
			feishu = m
		}
	}
	if feishu == nil || feishu["hasSecret"] != false || feishu["configured"] != true {
		t.Errorf("⑥ feishu got %v, want configured=true hasSecret=false", feishu)
	}
	labels, _ := v["eventLabels"].(map[string]any)
	if labels["prOpen"] != "已开 PR，待你评审" || labels["awaitingApproval"] != "验证已通过，等你放行" {
		t.Errorf("⑥ eventLabels got %v", labels)
	}

	t.Run("非法输入一律 400 且不回显 URL", func(t *testing.T) {
		cases := []struct{ path, body, wantSub string }{
			{"/api/notify/channels/dingtalk", putBody(hook.srv.URL+"/x", "", ""), "未知通知渠道"},
			{"/api/notify/channels/slack", putBody("https://evil.example/secret-path", "", ""), "url rejected"},
			{"/api/notify/channels/slack", putBody(hook.srv.URL+"/x", "sig", ""), "Slack"},
			{"/api/notify/channels/feishu", putBody("", "s", `"clearSecret":true`), "clearSecret"},
			{"/api/notify/channels/feishu", `not json`, "格式"},
		}
		for _, c := range cases {
			resp := f.srv.do(t, "PUT", c.path, c.body, true)
			raw := f.srv.raw(t, resp)
			if resp.StatusCode != http.StatusBadRequest || !strings.Contains(raw, c.wantSub) {
				t.Errorf("PUT %s %s got %d %s, want 400 含 %q", c.path, c.body, resp.StatusCode, raw, c.wantSub)
			}
			if strings.Contains(raw, "secret-path") {
				t.Errorf("错误回显了 URL：%s", raw)
			}
		}
		other := mustUser(t, f.st, "notify-first-"+randHex(t)+"@example.com")
		_, srvO := f.serverFor(t, other)
		resp := srvO.do(t, "PUT", "/api/notify/channels/slack", putBody("", "", ""), true)
		if raw := srvO.raw(t, resp); resp.StatusCode != http.StatusBadRequest || !strings.Contains(raw, "首次配置") {
			t.Errorf("首次不给 URL got %d %s, want 400 首次配置…", resp.StatusCode, raw)
		}
	})
	t.Run("DELETE 连带投递行", func(t *testing.T) {
		v := f.srv.decode(t, f.srv.do(t, "DELETE", "/api/notify/channels/feishu", "", true))
		if v["deleted"] != true {
			t.Errorf("got %v, want deleted:true", v)
		}
		if n := f.countDeliveries(t, f.userID, `true`); n != 0 {
			t.Errorf("投递行 got %d, want 0", n)
		}
	})
}

// ---------------------------------------------------------------- AC-25c

func TestUnreachableChannelNeverLeaksURL(t *testing.T) {
	dead := httptest.NewTLSServer(http.NotFoundHandler())
	addr, cl := dead.Listener.Addr().String(), dead.Client()
	tok := "tok-" + randHex(t)
	u := dead.URL + "/hook/T000/B000/" + tok
	dead.Close()
	leak := func(s string) bool {
		return strings.Contains(s, addr) || strings.Contains(s, "/hook") || strings.Contains(s, tok)
	}

	checkAll := func(t *testing.T, f *notifyFix) {
		t.Helper()
		// maskedURL 按设计就是 https://<host>/…（生产里 host 是 hooks.slack.com 这类公开域名）；
		// 这里 host 恰好是假服务器的 addr，所以单独断言它「只有 host、不带路径任何字符」，
		// 再把它从响应里剔掉，对其余整段响应体做 !leak。
		wantMask := "https://" + addr + "/…"
		for _, p := range []string{"/api/notify/deliveries", "/api/notify/channels"} {
			raw := f.srv.raw(t, f.srv.do(t, "GET", p, "", true))
			if p == "/api/notify/channels" {
				if !strings.Contains(raw, `"maskedURL":"`+wantMask+`"`) {
					t.Errorf("maskedURL 应恰为 %q：%s", wantMask, raw)
				}
				raw = strings.ReplaceAll(raw, wantMask, "")
			}
			if leak(raw) {
				t.Errorf("GET %s 泄漏 URL：%s", p, raw)
			}
		}
		rows, err := f.st.Pool().Query(context.Background(), `
			SELECT coalesce(d.last_error,'') FROM notify_deliveries d JOIN notify_channels c ON c.id=d.channel_id WHERE c.user_id=$1
			UNION ALL SELECT coalesce(test_error,'') FROM notify_channels WHERE user_id=$1`, f.userID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			if leak(s) {
				t.Errorf("库里错误串泄漏 URL：%q", s)
			}
		}
	}

	t.Run("测试消息路径", func(t *testing.T) {
		f := newNotifyFix(t, addr, cl)
		v := f.srv.decode(t, f.srv.do(t, "PUT", "/api/notify/channels/feishu", putBody(u, "sig", ""), true))
		tr := testResult(t, v)
		if v["saved"] != true || tr == nil || tr["ok"] != false || tr["error"] != "connection refused" {
			t.Fatalf("got %v, want saved / test.ok=false / error=connection refused", v)
		}
		id := int64(tr["deliveryId"].(float64))
		if d := f.delivery(t, id); d.LastError == nil || *d.LastError != "connection refused" {
			t.Errorf("last_error got %v, want connection refused", d.LastError)
		}
		var te string
		_ = f.st.Pool().QueryRow(context.Background(), `SELECT test_error FROM notify_channels WHERE user_id=$1`, f.userID).Scan(&te)
		if te != "connection refused" {
			t.Errorf("test_error got %q", te)
		}
		cv := f.srv.decode(t, f.srv.do(t, "GET", "/api/notify/channels", "", true))
		for _, c := range cv["channels"].([]any) {
			if m := c.(map[string]any); m["kind"] == "feishu" && m["testError"] != "connection refused" {
				t.Errorf("GET testError got %v", m["testError"])
			}
		}
		checkAll(t, f)
	})
	t.Run("worker 路径", func(t *testing.T) {
		f := newNotifyFix(t, addr, cl)
		if _, err := f.nc.SaveChannel(context.Background(), f.userID, store.SaveChannelParams{
			Kind: notify.KindFeishu, URL: u, Enabled: true, Events: notify.EventSwitches{Failed: true},
		}); err != nil {
			t.Fatal(err)
		}
		taskID := f.mkTask(t, f.userID, "CR-25C-"+randHex(t))
		if n, err := f.nc.EnqueueTaskEvent(context.Background(), notify.TaskEvent{TaskID: taskID, UserID: f.userID,
			ExternalKey: "CR-25C", ToState: "failed", EventType: notify.EventFailed, At: notifyT0}); err != nil || n != 1 {
			t.Fatalf("enqueue got (%d,%v)", n, err)
		}
		w := notify.NewWorker(f.nc, f.sender, "")
		w.Now = func() time.Time { return notifyT0 }
		deadline := time.Now().Add(3 * time.Second)
		for f.countDeliveries(t, f.userID, `d.status='pending' AND d.attempts=1`) == 0 {
			if time.Now().After(deadline) {
				t.Fatal("3s 内没处理到本行")
			}
			_ = w.DrainOnce(context.Background())
		}
		var le string
		_ = f.st.Pool().QueryRow(context.Background(), `SELECT last_error FROM notify_deliveries WHERE target_task_id=$1`, taskID).Scan(&le)
		if le != "connection refused" {
			t.Errorf("last_error got %q, want connection refused", le)
		}
		checkAll(t, f)
	})
}

// ---------------------------------------------------------------- AC-26

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func captureSlog(t *testing.T) *lockedBuf {
	t.Helper()
	buf := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

func TestNotifyLogsNeverContainWebhookURL(t *testing.T) {
	run := func(t *testing.T, f *notifyFix, u string) {
		t.Helper()
		f.srv.do(t, "PUT", "/api/notify/channels/feishu", putBody(u, "sig-sekrit-value", ""), true)
		taskID := f.mkTask(t, f.userID, "CR-26-"+randHex(t))
		if _, err := f.nc.EnqueueTaskEvent(context.Background(), notify.TaskEvent{TaskID: taskID, UserID: f.userID,
			ExternalKey: "CR-26", ToState: "failed", EventType: notify.EventFailed, At: notifyT0}); err != nil {
			t.Fatal(err)
		}
		w := notify.NewWorker(f.nc, f.sender, "")
		now := notifyT0
		w.Now = func() time.Time { return now }
		for i := 0; i < 5; i++ {
			deadline := time.Now().Add(3 * time.Second)
			for f.countDeliveries(t, f.userID, fmt.Sprintf(`d.event_type='failed' AND (d.attempts>%d OR d.status='gave_up')`, i)) == 0 {
				if time.Now().After(deadline) {
					t.Fatalf("第 %d 轮没处理到本行", i+1)
				}
				_ = w.DrainOnce(context.Background())
			}
			var next time.Time
			_ = f.st.Pool().QueryRow(context.Background(), `SELECT next_attempt_at FROM notify_deliveries WHERE target_task_id=$1`, taskID).Scan(&next)
			now = next
		}
		var status string
		_ = f.st.Pool().QueryRow(context.Background(), `SELECT status FROM notify_deliveries WHERE target_task_id=$1`, taskID).Scan(&status)
		if status != "gave_up" {
			t.Errorf("5 轮后 status got %q, want gave_up", status)
		}
	}

	t.Run("服务器 500", func(t *testing.T) {
		hook := newFakeWebhook(t, func(w http.ResponseWriter) { w.WriteHeader(500) })
		f := newNotifyFix(t, hook.srv.Listener.Addr().String(), hook.srv.Client())
		tok := "tok-" + randHex(t)
		buf := captureSlog(t)
		run(t, f, hook.srv.URL+"/hook/"+tok)
		if out := buf.String(); strings.Contains(out, "/hook/"+tok) || strings.Contains(out, "sig-sekrit-value") {
			t.Errorf("日志泄漏 URL/密钥：%s", out)
		} else if out == "" {
			t.Error("日志为空：断言没测到东西")
		}
	})
	t.Run("服务器不可达", func(t *testing.T) {
		dead := httptest.NewTLSServer(http.NotFoundHandler())
		addr, cl, u := dead.Listener.Addr().String(), dead.Client(), dead.URL+"/hook/tok-"+randHex(t)
		dead.Close()
		f := newNotifyFix(t, addr, cl)
		buf := captureSlog(t)
		run(t, f, u)
		if out := buf.String(); strings.Contains(out, addr) || strings.Contains(out, "/hook") {
			t.Errorf("日志泄漏 host:port 或路径：%s", out)
		}
	})
}
