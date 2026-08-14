package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Clouditera/lathe/internal/auth"
	"github.com/Clouditera/lathe/internal/store"
)

// accountFixture 搭一套完整的账号相关接口，返回带 cookie jar 的客户端。
//
// 现有两套 harness 都只走 Bearer，Cookie 这条路径此前从未被覆盖 ——
// 而它才是真实用户走的那条。
type accountFixture struct {
	srv    *httptest.Server
	client *http.Client
	users  *store.Users
	auth   *Auth
	mail   *fakeMailer
	st     *store.Store
}

// fakeMailer 记下发出的邮件，让忘记密码的链路能脱离 SMTP 测试。
type fakeMailer struct {
	ready bool
	sent  chan sentMail
}

type sentMail struct{ to, subject, body string }

func newFakeMailer() *fakeMailer {
	// 带缓冲：发信在后台 goroutine 里跑，测试还没来得及收就先发完了
	return &fakeMailer{ready: true, sent: make(chan sentMail, 8)}
}

func (m *fakeMailer) Ready(context.Context) bool { return m.ready }

func (m *fakeMailer) Send(_ context.Context, to, subject, body string) error {
	m.sent <- sentMail{to, subject, body}
	return nil
}

func newAccountFixture(t *testing.T) *accountFixture {
	t.Helper()

	st := testStoreForAPI(t)
	users := st.NewUsers()
	sessions := st.NewSessions()

	a := NewAuth(apiTestToken).WithStore(users, sessions, nil, false)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", a.Login)
	mux.HandleFunc("POST /api/logout", a.Logout)
	mux.HandleFunc("GET /api/me", a.Me)
	mux.Handle("GET /api/protected", a.RequireFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"actor": actorOf(r)})
	}))
	mux.Handle("GET /api/admin-only", a.RequireAdminFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))

	mailer := newFakeMailer()
	acct := &AccountAPI{
		Users: users, Sessions: sessions, Resets: st.NewResets(),
		Auth: a, Mail: mailer, BaseURL: "https://lathe.test",
	}
	acct.Routes(mux)

	admin := &AdminAPI{Users: users, Sessions: sessions, Resets: st.NewResets(), Auth: a}
	admin.Routes(mux)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}

	return &accountFixture{
		srv: srv, client: &http.Client{Jar: jar},
		users: users, auth: a, mail: mailer, st: st,
	}
}

// email 生成本测试专用的邮箱，与既有 fixture 的隔离约定一致。
func (f *accountFixture) email(t *testing.T, prefix string) string {
	t.Helper()
	return strings.ToLower(prefix + "-" + strings.ReplaceAll(t.Name(), "/", "-") + "@example.com")
}

// mkUser 直接建号（不走 HTTP），并登记清理。
func (f *accountFixture) mkUser(t *testing.T, email, password, role string) *store.User {
	t.Helper()
	hash, err := auth.Hash(password)
	if err != nil {
		t.Fatal(err)
	}
	u, err := f.users.Create(context.Background(), email, hash, role)
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.st.Pool().Exec(context.Background(), `DELETE FROM users WHERE id = $1`, u.ID)
	})
	return u
}

// req 发一个带 cookie jar 的请求。
func (f *accountFixture) req(t *testing.T, method, path, body string) (*http.Response, map[string]any) {
	t.Helper()

	var r *http.Request
	var err error
	if body == "" {
		r, err = http.NewRequest(method, f.srv.URL+path, nil)
	} else {
		r, err = http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	}
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")

	resp, err := f.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var v map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return resp, v
}

func (f *accountFixture) login(t *testing.T, email, password string) *http.Response {
	t.Helper()
	resp, _ := f.req(t, "POST", "/api/login",
		`{"email":"`+email+`","password":"`+password+`"}`)
	return resp
}

// newAccountFixtureClient 复制一份 fixture，但换一个独立的 cookie jar。
//
// 用来模拟「另一个人在另一台设备上」—— 测停用/代重置是否真的把对方踢下线，
// 必须有两个互不相干的会话才测得出来。
func newAccountFixtureClient(t *testing.T, f *accountFixture) *accountFixture {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	clone := *f
	clone.client = &http.Client{Jar: jar}
	return &clone
}

// replaceID 把路径模板里的 {id} 换成实际 id。
func replaceID(path string, id int64) string {
	return strings.Replace(path, "{id}", strconv.FormatInt(id, 10), 1)
}
