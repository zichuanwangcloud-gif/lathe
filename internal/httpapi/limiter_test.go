package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLimiterAllowsUpToLimit(t *testing.T) {
	l := newLimiter()
	now := time.Now()

	for i := 1; i <= 3; i++ {
		if !l.allowAt("k", 3, time.Minute, now) {
			t.Fatalf("第 %d 次应放行", i)
		}
	}
	if l.allowAt("k", 3, time.Minute, now) {
		t.Fatal("第 4 次应被拒")
	}
}

// 窗口滑过之后配额要恢复，否则被限一次就永久锁死。
func TestLimiterWindowSlides(t *testing.T) {
	l := newLimiter()
	now := time.Now()

	for i := 0; i < 3; i++ {
		l.allowAt("k", 3, time.Minute, now)
	}
	if l.allowAt("k", 3, time.Minute, now.Add(30*time.Second)) {
		t.Fatal("窗口内不该恢复")
	}
	if !l.allowAt("k", 3, time.Minute, now.Add(61*time.Second)) {
		t.Fatal("窗口滑过后应恢复")
	}
}

func TestLimiterKeysAreIndependent(t *testing.T) {
	l := newLimiter()
	now := time.Now()

	for i := 0; i < 3; i++ {
		l.allowAt("a", 3, time.Minute, now)
	}
	if !l.allowAt("b", 3, time.Minute, now) {
		t.Fatal("不同 key 之间不该互相影响")
	}
}

func TestClientIPIgnoresForwardedHeaderByDefault(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/login", nil)
	r.RemoteAddr = "192.0.2.10:1234"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")

	// 默认不信任：否则任何人加一行请求头就能绕开按 IP 的限流
	if got := clientIP(r, false); got != "192.0.2.10" {
		t.Fatalf("默认应取 RemoteAddr，得到 %q", got)
	}
	if got := clientIP(r, true); got != "1.2.3.4" {
		t.Fatalf("信任代理时应取 XFF 最左段，得到 %q", got)
	}
}

func TestClientIPTakesLeftmostForwardedEntry(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/login", nil)
	r.RemoteAddr = "192.0.2.10:1234"
	r.Header.Set("X-Forwarded-For", " 1.2.3.4 , 10.0.0.1 ")

	if got := clientIP(r, true); got != "1.2.3.4" {
		t.Fatalf("应取最左段并去空白，得到 %q", got)
	}
}

func TestValidEmail(t *testing.T) {
	good := []string{"a@b.co", "first.last+tag@sub.example.com"}
	bad := []string{
		"", "not-an-email", "@example.com", "a@",
		"Name <a@b.co>",        // 必须是纯地址
		"a@b.co\r\nBcc: x@y.z", // 头注入
	}
	for _, s := range good {
		if err := validEmail(s); err != nil {
			t.Errorf("%q 应通过，得到 %v", s, err)
		}
	}
	for _, s := range bad {
		if err := validEmail(s); err == nil {
			t.Errorf("%q 应被拒绝", s)
		}
	}
}

// 重置邮件必须带上完整链接，否则用户收到一封没法用的信。
func TestResetMailContainsLink(t *testing.T) {
	link := "https://lathe.test/reset-password?token=abc123"
	subject, body := resetMail("someone@example.com", link)

	if subject == "" {
		t.Fatal("主题不能为空")
	}
	if !strings.Contains(body, link) {
		t.Fatalf("正文里应含重置链接，得到:\n%s", body)
	}
	if !strings.Contains(body, "someone@example.com") {
		t.Fatal("正文里应写明是给哪个账号发的")
	}
}
