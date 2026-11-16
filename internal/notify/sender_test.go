package notify

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHook 是一台 TLS 假 webhook：记录请求、按预设回响应。
type fakeHook struct {
	srv    *httptest.Server
	hits   atomic.Int32
	bodies chan []byte
	ctypes chan string
}

func newFakeHook(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *fakeHook {
	t.Helper()
	f := &fakeHook{bodies: make(chan []byte, 64), ctypes: make(chan string, 64)}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		f.bodies <- b
		f.ctypes <- r.Header.Get("Content-Type")
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHook) sender() *Sender {
	return NewSender([]string{f.srv.Listener.Addr().String()}, HardenClient(f.srv.Client()))
}

func randTok(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "tok-" + hex.EncodeToString(b)
}

func reply(code int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

// AC-5
func TestSendSlackPostsTextPayload(t *testing.T) {
	f := newFakeHook(t, reply(200, "ok"))
	res := f.sender().Send(context.Background(), Channel{Kind: KindSlack, URL: f.srv.URL + "/hook"},
		Message{Title: "[Lathe] CR-1326 处理失败", Text: "任务 #42（CR-1326）处理失败。"})
	if !res.OK {
		t.Fatalf("got %+v, want OK", res)
	}
	if got := <-f.ctypes; got != "application/json" {
		t.Errorf("Content-Type got %q, want application/json", got)
	}
	var body map[string]any
	if err := json.Unmarshal(<-f.bodies, &body); err != nil {
		t.Fatal(err)
	}
	text, _ := body["text"].(string)
	if !strings.Contains(text, "CR-1326") || len(body) != 1 {
		t.Errorf("body got %v, want 只有 text 且含 CR-1326", body)
	}
}

// feishuSign 在测试侧独立复算签名（飞书《自定义机器人使用指南》签名校验：
// stringToSign = timestamp + "\n" + secret 作 HmacSHA256 的 key，消息为空，结果 Base64）。
func feishuSignForTest(ts, secret string) string { return feishuSign(ts, secret) }

// AC-6
func TestSendFeishuSignsWithTimestampAndSecret(t *testing.T) {
	t.Run("固定向量（openssl dgst -sha256 -hmac 与官方 Python 样例复算一致）", func(t *testing.T) {
		if got, want := feishuSignForTest("1700000000", "test-secret"), "mbm4Y4oluIPQ00qlBIhX8vAZ0EKv3nw0LuTb91jPL84="; got != want {
			t.Errorf("sign got %q, want %q", got, want)
		}
	})
	t.Run("请求体带 timestamp 与 sign 且与公式一致", func(t *testing.T) {
		f := newFakeHook(t, reply(200, `{"code":0,"msg":"success","data":{}}`))
		s := f.sender()
		s.now = func() time.Time { return time.Unix(1700000000, 0) }
		res := s.Send(context.Background(), Channel{Kind: KindFeishu, URL: f.srv.URL + "/hook", SignSecret: "test-secret"},
			Message{Title: "t", Text: "x"})
		if !res.OK {
			t.Fatalf("got %+v, want OK", res)
		}
		var body map[string]any
		if err := json.Unmarshal(<-f.bodies, &body); err != nil {
			t.Fatal(err)
		}
		if body["msg_type"] != "text" {
			t.Errorf("msg_type got %v, want text", body["msg_type"])
		}
		if got, want := body["timestamp"], "1700000000"; got != want {
			t.Errorf("timestamp got %v, want %v", got, want)
		}
		if got, want := body["sign"], "mbm4Y4oluIPQ00qlBIhX8vAZ0EKv3nw0LuTb91jPL84="; got != want {
			t.Errorf("sign got %v, want %v", got, want)
		}
	})
}

// AC-7
func TestSendFeishuWithoutSecretOmitsSignature(t *testing.T) {
	f := newFakeHook(t, reply(200, `{"code":0}`))
	res := f.sender().Send(context.Background(), Channel{Kind: KindFeishu, URL: f.srv.URL + "/hook"}, Message{Title: "t", Text: "x"})
	if !res.OK {
		t.Fatalf("got %+v, want OK", res)
	}
	var body map[string]any
	if err := json.Unmarshal(<-f.bodies, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["timestamp"]; ok {
		t.Errorf("body got %v, want 无 timestamp", body)
	}
	if _, ok := body["sign"]; ok {
		t.Errorf("body got %v, want 无 sign", body)
	}
}

// AC-8
func TestSendClassifiesOutcomes(t *testing.T) {
	type want struct {
		ok, retryable bool
		reasonHas     string
		reasonEq      string
	}
	cases := []struct {
		name, kind string
		code       int
		body       string
		want       want
	}{
		{"slack 200 ok", KindSlack, 200, "ok", want{ok: true}},
		{"slack 200 ok 带换行", KindSlack, 200, "ok\n", want{ok: true}},
		{"slack 200 JSON 不算成功", KindSlack, 200, `{"ok":true}`, want{}},
		{"slack 400 invalid_payload", KindSlack, 400, "invalid_payload", want{reasonHas: "400 invalid_payload"}},
		{"slack 404 no_service", KindSlack, 404, "no_service", want{reasonHas: "no_service"}},
		{"slack 403 channel_is_archived", KindSlack, 403, "channel_is_archived", want{}},
		{"slack 403 action_prohibited", KindSlack, 403, "action_prohibited", want{}},
		{"slack 429 可重试", KindSlack, 429, "rate_limited", want{retryable: true}},
		{"slack 500 可重试", KindSlack, 500, "oops", want{retryable: true, reasonHas: "500"}},
		{"feishu code 0", KindFeishu, 200, `{"code":0,"msg":"success","data":{}}`, want{ok: true}},
		{"feishu 旧形态 StatusCode 0", KindFeishu, 200, `{"StatusCode":0,"StatusMessage":"success"}`, want{ok: true}},
		{"feishu 9499", KindFeishu, 200, `{"code":9499,"msg":"Bad Request"}`, want{reasonHas: "9499"}},
		{"feishu 19021 签名错", KindFeishu, 200, `{"code":19021,"msg":"sign match fail or timestamp is not within one hour from current time"}`, want{reasonHas: "19021"}},
		{"feishu 19024", KindFeishu, 200, `{"code":19024,"msg":"Key Words Not Found"}`, want{}},
		{"feishu 11232 限流可重试", KindFeishu, 200, `{"code":11232,"msg":"frequency limited"}`, want{retryable: true}},
		{"feishu 未知码", KindFeishu, 200, `{"code":12345}`, want{}},
		{"feishu 500 可重试", KindFeishu, 500, "", want{retryable: true}},
	}
	check := func(t *testing.T, res Result, w want, addr, tok string) {
		t.Helper()
		if res.OK != w.ok || res.Retryable != w.retryable {
			t.Errorf("got ok=%v retryable=%v reason=%q, want ok=%v retryable=%v", res.OK, res.Retryable, res.Reason, w.ok, w.retryable)
		}
		if w.reasonHas != "" && !strings.Contains(res.Reason, w.reasonHas) {
			t.Errorf("reason got %q, want 含 %q", res.Reason, w.reasonHas)
		}
		if w.reasonEq != "" && res.Reason != w.reasonEq {
			t.Errorf("reason got %q, want == %q", res.Reason, w.reasonEq)
		}
		if strings.Contains(res.Reason, addr) || strings.Contains(res.Reason, "/hook") || strings.Contains(res.Reason, tok) {
			t.Errorf("reason 泄漏 URL：got %q", res.Reason)
		}
		if !w.ok && res.Reason == "" {
			t.Error("失败时 reason 不能为空")
		}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeHook(t, reply(c.code, c.body))
			tok := randTok(t)
			res := f.sender().Send(context.Background(), Channel{Kind: c.kind, URL: f.srv.URL + "/hook/T000/B000/" + tok}, Message{Title: "t", Text: "x"})
			check(t, res, c.want, f.srv.Listener.Addr().String(), tok)
		})
	}
	for _, kind := range []string{KindSlack, KindFeishu} {
		t.Run(kind+" 超时：可重试且 reason == timeout", func(t *testing.T) {
			release := make(chan struct{})
			f := newFakeHook(t, func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-release:
				case <-time.After(2 * time.Second):
				}
			})
			defer close(release)
			c := HardenClient(f.srv.Client())
			c.Timeout = 100 * time.Millisecond
			s := NewSender([]string{f.srv.Listener.Addr().String()}, c)
			tok := randTok(t)
			res := s.Send(context.Background(), Channel{Kind: kind, URL: f.srv.URL + "/hook/T000/B000/" + tok}, Message{Title: "t", Text: "x"})
			check(t, res, want{retryable: true, reasonEq: "timeout"}, f.srv.Listener.Addr().String(), tok)
		})
		t.Run(kind+" 连接被拒：可重试且 reason == connection refused", func(t *testing.T) {
			dead := httptest.NewTLSServer(http.NotFoundHandler())
			u, addr, cl := dead.URL, dead.Listener.Addr().String(), dead.Client()
			dead.Close()
			s := NewSender([]string{addr}, HardenClient(cl))
			tok := randTok(t)
			res := s.Send(context.Background(), Channel{Kind: kind, URL: u + "/hook/T000/B000/" + tok}, Message{Title: "t", Text: "x"})
			check(t, res, want{retryable: true, reasonEq: "connection refused"}, addr, tok)
		})
	}
}

// AC-9
func TestSenderRefusesBadURLs(t *testing.T) {
	good := newFakeHook(t, reply(200, "ok"))
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits.Add(1)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(plain.Close)
	s := good.sender()

	for _, raw := range []string{
		"http://" + plain.Listener.Addr().String() + "/x",
		"http://" + good.srv.Listener.Addr().String() + "/x",
		"https://evil.example/x",
		"https://127.0.0.1/x",
		"https://hooks.slack.com.evil.com/x",
		"",
		"https://",
	} {
		t.Run("拒绝 "+raw, func(t *testing.T) {
			res := s.Send(context.Background(), Channel{Kind: KindSlack, URL: raw}, Message{Title: "t", Text: "x"})
			if res.OK || res.Retryable {
				t.Errorf("got %+v, want 失败且不可重试", res)
			}
			if !strings.HasPrefix(res.Reason, "url rejected: ") {
				t.Errorf("reason got %q, want url rejected: …", res.Reason)
			}
			if raw != "" && raw != "https://" && strings.Contains(res.Reason, raw) {
				t.Errorf("reason 不得回显入参：got %q", res.Reason)
			}
		})
	}
	if good.hits.Load() != 0 || plainHits.Load() != 0 {
		t.Errorf("请求计数 got tls=%d plain=%d, want 0/0", good.hits.Load(), plainHits.Load())
	}
	t.Run("正例：白名单内的 TLS 地址能打通", func(t *testing.T) {
		if res := s.Send(context.Background(), Channel{Kind: KindSlack, URL: good.srv.URL + "/hook"}, Message{Title: "t", Text: "x"}); !res.OK {
			t.Errorf("got %+v, want OK", res)
		}
	})
	t.Run("ValidateURL 与生产白名单", func(t *testing.T) {
		cases := []struct {
			raw string
			ok  bool
		}{
			{"https://hooks.slack.com/services/T/B/X", true},
			{"https://open.feishu.cn/open-apis/bot/v2/hook/x", true},
			{"https://open.larksuite.com/open-apis/bot/v2/hook/x", true},
			{"https://hooks.slack.com:8443/services/x", false},
			{"https://hooks.slack.com@evil.com/x", false},
			{"http://hooks.slack.com/services/x", false},
			{"::not a url", false},
		}
		for _, c := range cases {
			if err := ValidateURL(c.raw, AllowedHosts); (err == nil) != c.ok {
				t.Errorf("ValidateURL(%q) got err=%v, want ok=%v", c.raw, err, c.ok)
			}
		}
	})
}

// AC-10
func TestSenderDoesNotFollowRedirects(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(target.Close)
	f := newFakeHook(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/internal", http.StatusFound)
	})
	res := f.sender().Send(context.Background(), Channel{Kind: KindSlack, URL: f.srv.URL + "/hook"}, Message{Title: "t", Text: "x"})
	if res.OK || res.Retryable {
		t.Errorf("got %+v, want 失败且不可重试", res)
	}
	if n := targetHits.Load(); n != 0 {
		t.Errorf("重定向目标请求数 got %d, want 0", n)
	}
}

// AC-10b
func TestProductionSenderClientIsHardened(t *testing.T) {
	s := NewProductionSender()
	if s.client.Timeout != 30*time.Second {
		t.Errorf("Timeout got %v, want 30s", s.client.Timeout)
	}
	if s.client.CheckRedirect == nil {
		t.Fatal("CheckRedirect got nil, want ErrUseLastResponse")
	}
	if err := s.client.CheckRedirect(&http.Request{}, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("CheckRedirect got %v, want http.ErrUseLastResponse", err)
	}
	if strings.Join(s.allow, ",") != strings.Join(AllowedHosts, ",") {
		t.Errorf("allow got %v, want %v", s.allow, AllowedHosts)
	}
	if got, want := strings.Join(AllowedHosts, ","), "hooks.slack.com,open.feishu.cn,open.larksuite.com"; got != want {
		t.Errorf("AllowedHosts got %s, want %s", got, want)
	}
}

func TestMaskURL(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"https://hooks.slack.com/services/T000/B000/XXXXabcd", "https://hooks.slack.com/…"},
		{"https://open.feishu.cn/open-apis/bot/v2/hook/abcd-1234", "https://open.feishu.cn/…"},
		{"::bad", "…"},
		{"", "…"},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			if got := MaskURL(c.raw); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestChannelStringHidesSecrets(t *testing.T) {
	ch := Channel{ID: 1, Kind: KindFeishu, URL: "https://open.feishu.cn/open-apis/bot/v2/hook/sekrit", SignSecret: "sig-sekrit"}
	for _, s := range []string{ch.String(), func() string { b, _ := json.Marshal(ch); return string(b) }()} {
		if strings.Contains(s, "sekrit") {
			t.Errorf("got %q, want 不含 URL/密钥明文", s)
		}
	}
}
