package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthDisabledRejectsEverything(t *testing.T) {
	a := NewAuth("") // 未配置令牌

	if a.Enabled() {
		t.Error("空令牌不应算作已启用")
	}

	// 未配置时必须拒绝，而不是默认放行 ——
	// 把「忘了配」的后果做成不可用，而非不设防
	rec := httptest.NewRecorder()
	a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("未配置令牌时不应放行到业务处理器")
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tasks", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("状态码 = %d，期望 503", rec.Code)
	}

	// 即使带上"正确"的空令牌也不行
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer ")
	if a.authenticated(req) {
		t.Error("未配置令牌时任何请求都不应通过认证")
	}
}

func TestAuthLoginAndSession(t *testing.T) {
	a := NewAuth("secret-token")

	// 错误令牌
	rec := httptest.NewRecorder()
	a.Login(rec, httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"token":"wrong"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("错误令牌应返回 401，得到 %d", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("登录失败不应签发 Cookie")
	}

	// 正确令牌
	rec = httptest.NewRecorder()
	a.Login(rec, httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"token":"secret-token"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("正确令牌应返回 200，得到 %d: %s", rec.Code, rec.Body)
	}

	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			session = c
		}
	}
	if session == nil {
		t.Fatal("应签发会话 Cookie")
	}
	if !session.HttpOnly {
		t.Error("会话 Cookie 必须是 HttpOnly，防止脚本读取")
	}
	if session.Value == "" {
		t.Error("会话 ID 不应为空")
	}

	// 用 Cookie 访问受保护资源
	passed := false
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.AddCookie(session)
	rec2 := httptest.NewRecorder()
	a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		passed = true
	})).ServeHTTP(rec2, req)
	if !passed {
		t.Errorf("持有效会话应放行，得到 %d: %s", rec2.Code, rec2.Body)
	}

	// 注销后失效
	logoutRec := httptest.NewRecorder()
	logoutReq := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	logoutReq.AddCookie(session)
	a.Logout(logoutRec, logoutReq)

	req2 := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req2.AddCookie(session)
	if a.authenticated(req2) {
		t.Error("注销后会话应失效")
	}
}

func TestAuthBearerToken(t *testing.T) {
	a := NewAuth("secret-token")

	// 支持 Bearer 是为了让 curl / 脚本免登录直接调用
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	if !a.authenticated(req) {
		t.Error("正确的 Bearer 令牌应通过")
	}

	for _, bad := range []string{"Bearer wrong", "secret-token", "Basic secret-token", ""} {
		req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
		if bad != "" {
			req.Header.Set("Authorization", bad)
		}
		if a.authenticated(req) {
			t.Errorf("Authorization=%q 不应通过", bad)
		}
	}
}

func TestAuthRejectsForgedSession(t *testing.T) {
	a := NewAuth("secret-token")

	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "伪造的会话ID"})
	if a.authenticated(req) {
		t.Error("伪造的会话 ID 不应通过")
	}

	rec := httptest.NewRecorder()
	a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("伪造会话不应放行")
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("状态码 = %d，期望 401", rec.Code)
	}
}

func TestAuthMe(t *testing.T) {
	a := NewAuth("secret-token")

	rec := httptest.NewRecorder()
	a.Me(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `"authenticated":false`) {
		t.Errorf("未登录时应报告 false: %s", body)
	}
	if !strings.Contains(body, `"authEnabled":true`) {
		t.Errorf("应报告鉴权已启用: %s", body)
	}

	// 未配置令牌时前端要能区分「没登录」与「功能未开」
	disabled := NewAuth("")
	rec2 := httptest.NewRecorder()
	disabled.Me(rec2, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if !strings.Contains(rec2.Body.String(), `"authEnabled":false`) {
		t.Errorf("未配置令牌应报告 authEnabled=false: %s", rec2.Body)
	}
}

func TestAuthLoginWhenDisabled(t *testing.T) {
	a := NewAuth("")
	rec := httptest.NewRecorder()
	a.Login(rec, httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"token":"anything"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("未配置令牌时登录应返回 503，得到 %d", rec.Code)
	}
}
