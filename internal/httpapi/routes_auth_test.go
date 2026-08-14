package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Clouditera/lathe/internal/secret"
)

// 新增的管理端与 SMTP 接口同样必须挡住未认证请求。
// 与 TestAPIRequiresAuth / TestCredentialRequiresAuth 一脉相承：
// 加了路由就往这张表里补一行，别让新接口悄悄裸奔。
func TestAdminAndSMTPRequireAuth(t *testing.T) {
	st := testStoreForAPI(t)
	a := NewAuth(apiTestToken).WithStore(st.NewUsers(), st.NewSessions(), nil, false)

	mux := http.NewServeMux()
	(&AdminAPI{Users: st.NewUsers(), Sessions: st.NewSessions(), Resets: st.NewResets(), Auth: a}).Routes(mux)
	(&SMTPAPI{Secrets: st.NewSecrets(testSealer(t)), Auth: a}).Routes(mux)
	(&AccountAPI{Users: st.NewUsers(), Sessions: st.NewSessions(), Resets: st.NewResets(), Auth: a}).Routes(mux)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	endpoints := []struct{ method, path, body string }{
		{"GET", "/api/admin/users", ""},
		{"POST", "/api/admin/users/1/enable", ""},
		{"POST", "/api/admin/users/1/disable", ""},
		{"POST", "/api/admin/users/1/role", `{"role":"admin"}`},
		{"POST", "/api/admin/users/1/password", `{}`},
		{"DELETE", "/api/admin/users/1", ""},
		{"GET", "/api/smtp", ""},
		{"PUT", "/api/smtp", `{"host":"h","port":25,"fromAddr":"a@b.co","tlsMode":"none"}`},
		{"POST", "/api/smtp/verify", ""},
		{"DELETE", "/api/smtp", ""},
		// 改密要求登录；注册与忘记密码是公开的，不在此列
		{"POST", "/api/password/change", `{"currentPassword":"a","newPassword":"b"}`},
	}

	for _, e := range endpoints {
		resp := do(t, srv, e.method, e.path, e.body, false)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s 未认证时状态码 = %d，期望 401", e.method, e.path, resp.StatusCode)
		}
	}
}

// 公开接口不需要登录，别把它们一起关进去。
func TestPublicAccountRoutesDoNotRequireAuth(t *testing.T) {
	st := testStoreForAPI(t)
	a := NewAuth(apiTestToken).WithStore(st.NewUsers(), st.NewSessions(), nil, false)

	mux := http.NewServeMux()
	(&AccountAPI{
		Users: st.NewUsers(), Sessions: st.NewSessions(), Resets: st.NewResets(),
		Auth: a, Mail: newFakeMailer(), BaseURL: "https://lathe.test",
	}).Routes(mux)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for _, e := range []struct{ method, path, body string }{
		{"POST", "/api/password/forgot", `{"email":"nobody@example.com"}`},
		{"POST", "/api/password/reset", `{"token":"bad","password":"whatever-123"}`},
	} {
		resp := do(t, srv, e.method, e.path, e.body, false)
		if resp.StatusCode == http.StatusUnauthorized {
			t.Errorf("%s %s 是公开接口，不该要求登录", e.method, e.path)
		}
	}
}

// testSealer 造一个确定性的加密器，与 credFixture 的做法一致。
func testSealer(t *testing.T) secret.Sealer {
	t.Helper()
	key := make([]byte, secret.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	s, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
