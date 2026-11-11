package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// ---------------------------------------------------------------- 夹具

// patRand 生成随机后缀：fixture 标识必须带随机量（AGENTS.md §5.5）。
func patRand(t *testing.T) string {
	t.Helper()
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// patActor 是一个独立用户 + 一个只属于他的 cookie jar（已登录）。
type patActor struct {
	u  *store.User
	pw string
	c  *http.Client
}

// newPATActor 建一个随机邮箱的新用户并登录。
//
// 每个会调 POST /api/pat 的表驱动行都用自己的 patActor：签发限流按用户计，
// 行与行之间不能共享额度（PRD §5 rev.4 通用前置）。
func newPATActor(t *testing.T, f *accountFixture, role string) *patActor {
	t.Helper()
	pw := "pat-password-" + patRand(t)
	email := "pat-" + patRand(t) + "@pat-test.example.com"
	u := f.mkUser(t, email, pw, role)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &patActor{u: u, pw: pw, c: &http.Client{Jar: jar}}
	code, body := patDo(t, f, a.c, "POST", "/api/login",
		`{"email":"`+email+`","password":"`+pw+`"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("登录失败 got=%d want=200: %s", code, body)
	}
	return a
}

// patDo 用指定 client（nil = 无 Cookie）发请求，可带额外请求头。
func patDo(t *testing.T, f *accountFixture, c *http.Client, method, path, body string, hdr map[string]string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r, err := http.NewRequest(method, f.srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	if c == nil {
		c = &http.Client{}
	}
	resp, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

func jsonOf(t *testing.T, s string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("响应不是 JSON: %v: %s", err, s)
	}
	return v
}

// issue 用 actor 的会话 + 正确口令签发一把 PAT，返回明文与 id。
func (a *patActor) issue(t *testing.T, f *accountFixture, name string, extra string) (string, int64) {
	t.Helper()
	code, body := patDo(t, f, a.c, "POST", "/api/pat",
		`{"name":"`+name+`","currentPassword":"`+a.pw+`"`+extra+`}`, nil)
	if code != http.StatusCreated {
		t.Fatalf("签发 PAT got=%d want=201: %s", code, body)
	}
	v := jsonOf(t, body)
	tok, _ := v["token"].(string)
	id, _ := v["id"].(float64)
	if tok == "" || id == 0 {
		t.Fatalf("签发响应缺 token/id: %s", body)
	}
	return tok, int64(id)
}

// actorOn 用 PAT 打 /api/protected，返回状态码与 actor。
func actorOn(t *testing.T, f *accountFixture, c *http.Client, tok string) (int, string) {
	t.Helper()
	code, body := patDo(t, f, c, "GET", "/api/protected", "", bearer(tok))
	if code != http.StatusOK {
		return code, ""
	}
	actor, _ := jsonOf(t, body)["actor"].(string)
	return code, actor
}

func userActor(u *store.User) string { return "user:" + strconv.FormatInt(u.ID, 10) }

func patCount(t *testing.T, f *accountFixture, userID int64) int {
	t.Helper()
	var n int
	if err := f.st.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM personal_access_tokens WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func patRevokedAt(t *testing.T, f *accountFixture, id int64) *time.Time {
	t.Helper()
	var at *time.Time
	if err := f.st.Pool().QueryRow(context.Background(),
		`SELECT revoked_at FROM personal_access_tokens WHERE id = $1`, id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// meUnauthenticated 断言 /api/me 回 200 + authenticated:false + user:null。
func meUnauthenticated(t *testing.T, f *accountFixture, c *http.Client, hdr map[string]string, path string) {
	t.Helper()
	if path == "" {
		path = "/api/me"
	}
	code, body := patDo(t, f, c, "GET", path, "", hdr)
	v := jsonOf(t, body)
	if code != http.StatusOK || v["authenticated"] != false || v["user"] != nil {
		t.Errorf("/api/me got=(%d, authenticated=%v, user=%v) want=(200,false,nil)", code, v["authenticated"], v["user"])
	}
}

// ---------------------------------------------------------------- AC-2 / AC-3

func TestPATCreateStoresOnlyHash(t *testing.T) {
	f := newAccountFixture(t)
	a := newPATActor(t, f, store.RoleMember)
	plain, id := a.issue(t, f, "ci-deploy", `,"expiresInDays":90`)

	if !strings.HasPrefix(plain, "lathe_pat_") || len(plain) != 10+43 {
		t.Fatalf("明文形态 got=%q len=%d want=lathe_pat_+43（len=53）", plain, len(plain))
	}
	sum := sha256.Sum256([]byte(plain))
	var gotID int64
	if err := f.st.Pool().QueryRow(context.Background(),
		`SELECT id FROM personal_access_tokens WHERE token_hash = $1`, hex.EncodeToString(sum[:])).Scan(&gotID); err != nil {
		t.Fatalf("按 SHA-256 查不到行: %v", err)
	}
	if gotID != id {
		t.Errorf("哈希对应的行 got=%d want=%d", gotID, id)
	}
	var dump string
	if err := f.st.Pool().QueryRow(context.Background(),
		`SELECT coalesce(string_agg(row_to_json(t)::text, E'\n'), '') FROM personal_access_tokens t`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dump, plain) {
		t.Errorf("库里出现了明文 got=contains want=absent")
	}
	if strings.Contains(dump, plain[10:]) {
		t.Errorf("库里出现了明文主体 got=contains want=absent")
	}
}

func TestPATPlaintextReturnedOnlyOnCreate(t *testing.T) {
	f := newAccountFixture(t)
	a := newPATActor(t, f, store.RoleMember)
	plain, _ := a.issue(t, f, "once", "")

	for i := 0; i < 2; i++ {
		code, body := patDo(t, f, a.c, "GET", "/api/pat", "", nil)
		if code != http.StatusOK {
			t.Fatalf("第 %d 次 GET /api/pat got=%d want=200: %s", i+1, code, body)
		}
		if strings.Contains(body, plain) || strings.Contains(body, plain[10:]) {
			t.Errorf("第 %d 次列表里出现明文", i+1)
		}
		tokens, _ := jsonOf(t, body)["tokens"].([]any)
		if len(tokens) != 1 {
			t.Fatalf("列表项数 got=%d want=1", len(tokens))
		}
		for _, raw := range tokens {
			item, _ := raw.(map[string]any)
			for _, k := range []string{"prefix", "lastUsedAt", "id", "name", "createdAt", "useCount", "expiresAt", "expired"} {
				if _, ok := item[k]; !ok {
					t.Errorf("列表项缺键 %q: %v", k, item)
				}
			}
			if _, ok := item["token"]; ok {
				t.Errorf("列表项不该有 token 键: %v", item)
			}
			if item["prefix"] != plain[10:18] {
				t.Errorf("prefix got=%v want=%s", item["prefix"], plain[10:18])
			}
		}
	}
}

// ---------------------------------------------------------------- AC-4 / AC-5

func TestPATBearerActsAsOwner(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	a := newPATActor(t, f, store.RoleMember)
	b := newPATActor(t, f, store.RoleMember)
	m := newPATActor(t, f, store.RoleAdmin)
	aTok, _ := a.issue(t, f, "a", "")
	mTok, _ := m.issue(t, f, "m", "")

	taskOf := func(u *store.User) int64 {
		var repoID, taskID int64
		if err := f.st.Pool().QueryRow(ctx, `INSERT INTO repos (user_id, provider_repo) VALUES ($1, $2) RETURNING id`,
			u.ID, "pat/"+patRand(t)).Scan(&repoID); err != nil {
			t.Fatal(err)
		}
		if err := f.st.Pool().QueryRow(ctx, `INSERT INTO tasks (user_id, repo_id, external_key, state)
			VALUES ($1, $2, $3, 'merged') RETURNING id`, u.ID, repoID, "PAT-"+patRand(t)).Scan(&taskID); err != nil {
			t.Fatal(err)
		}
		return taskID
	}
	aTask, bTask := taskOf(a.u), taskOf(b.u)

	tests := []struct {
		name  string
		tok   string
		path  string
		want  int
		check func(t *testing.T, body string)
	}{
		{"给定普通用户 A 的 PAT_断言 /api/tasks 只列 A 的任务", aTok, "/api/tasks?limit=200", http.StatusOK, func(t *testing.T, body string) {
			seen := map[int64]bool{}
			tasks, _ := jsonOf(t, body)["tasks"].([]any)
			for _, raw := range tasks {
				item, _ := raw.(map[string]any)
				id, _ := item["id"].(float64)
				seen[int64(id)] = true
				if uid, _ := item["userId"].(float64); int64(uid) != a.u.ID {
					t.Errorf("出现非 A 的任务 userId got=%v want=%d", uid, a.u.ID)
				}
			}
			if !seen[aTask] || seen[bTask] {
				t.Errorf("任务可见性 got=(A:%v,B:%v) want=(true,false)", seen[aTask], seen[bTask])
			}
		}},
		{"给定普通用户 A 的 PAT_断言管理接口 403（不提权）", aTok, "/api/admin/users", http.StatusForbidden, nil},
		{"给定管理员 M 的 PAT_断言管理接口 200（不降权）", mTok, "/api/admin/users", http.StatusOK, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, body := patDo(t, f, nil, "GET", tc.path, "", bearer(tc.tok))
			if code != tc.want {
				t.Fatalf("got=%d want=%d: %s", code, tc.want, body)
			}
			if tc.check != nil {
				tc.check(t, body)
			}
		})
	}

	if code, actor := actorOn(t, f, nil, aTok); code != http.StatusOK || actor != userActor(a.u) {
		t.Errorf("/api/protected got=(%d,%q) want=(200,%q)", code, actor, userActor(a.u))
	}
}

func TestPATRevokeOtherUserReturns404(t *testing.T) {
	f := newAccountFixture(t)
	a := newPATActor(t, f, store.RoleMember)
	b := newPATActor(t, f, store.RoleMember)
	_, bID := b.issue(t, f, "b", "")

	tests := []struct {
		name string
		path string
	}{
		{"给定 B 的令牌 id_断言 A 吊销得到 404", "/api/pat/" + strconv.FormatInt(bID, 10)},
		{"给定不存在的令牌 id_断言 404（与越权同口径）", "/api/pat/999999999"},
		{"给定非数字 id_断言 404", "/api/pat/abc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, body := patDo(t, f, a.c, "DELETE", tc.path, "", nil)
			if code != http.StatusNotFound {
				t.Errorf("got=%d want=404: %s", code, body)
			}
		})
	}
	if at := patRevokedAt(t, f, bID); at != nil {
		t.Errorf("B 的令牌被误吊销 revoked_at got=%v want=nil", at)
	}
}

// ---------------------------------------------------------------- AC-6

func TestDisabledUserPATRejected(t *testing.T) {
	f := newAccountFixture(t)
	a := newPATActor(t, f, store.RoleMember)
	admin := newPATActor(t, f, store.RoleAdmin)
	tok, id := a.issue(t, f, "a", "")

	if code, actor := actorOn(t, f, nil, tok); code != http.StatusOK || actor != userActor(a.u) {
		t.Fatalf("停用前 got=(%d,%q) want=(200,%q)", code, actor, userActor(a.u))
	}
	if code, body := patDo(t, f, admin.c, "POST", "/api/admin/users/"+strconv.FormatInt(a.u.ID, 10)+"/disable", "", nil); code != http.StatusOK {
		t.Fatalf("停用 got=%d want=200: %s", code, body)
	}
	if code, _ := actorOn(t, f, nil, tok); code != http.StatusUnauthorized {
		t.Errorf("停用后 got=%d want=401", code)
	}
	if at := patRevokedAt(t, f, id); at == nil {
		t.Errorf("停用后 revoked_at got=nil want=非空")
	}
}

// ---------------------------------------------------------------- AC-6b / AC-9

// fakePATs 是 PATLookup 的假件：记录调用次数，命中时回 user。
type fakePATs struct {
	user    *store.User
	lookups int
	touches int
}

func (p *fakePATs) Lookup(_ context.Context, _ string) (*store.User, int64, error) {
	p.lookups++
	if p.user == nil {
		return nil, 0, errors.New("fake: 不存在")
	}
	return p.user, 1, nil
}

func (p *fakePATs) Touch(context.Context, int64) error { p.touches++; return nil }

func TestAdminTokenEmptyRejectsWhitespaceBearer(t *testing.T) {
	st := testStoreForAPI(t)
	marker := "bootstrap-marker-" + patRand(t) + "@pat-test.example.com"
	boot := &store.User{ID: 987654321, Email: marker, Role: store.RoleAdmin}
	a := NewAuth("").WithStore(st.NewUsers(), st.NewSessions(), boot, false).WithPATs(st.NewPATs())

	probe := a.RequireFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"actor": actorOf(r), "user": CurrentUser(r).Email})
	})
	mux := http.NewServeMux()
	mux.Handle("GET /probe", probe)
	mux.HandleFunc("GET /me", a.Me)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	const inproc, real = "进程内", "真实net/http"
	rows := []struct {
		id      string
		header  *string
		deliver string
	}{
		{"1_NBSP", ptr("Bearer  "), real},
		{"2_EM_SPACE", ptr("Bearer  "), real},
		{"3_NEL", ptr("Bearer \u0085"), real},
		{"4a_空令牌", ptr("Bearer "), inproc},
		{"4b_空令牌", ptr("Bearer "), real},
		{"5a_TAB", ptr("Bearer \t"), inproc},
		{"5b_TAB", ptr("Bearer \t"), real},
		{"6_无头", nil, real},
		{"7_形态对库里没有", ptr("Bearer lathe_pat_" + strings.Repeat("A", 43)), real},
	}
	send := func(t *testing.T, deliver, path string, h *string) (int, string) {
		t.Helper()
		if deliver == inproc {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			if h != nil {
				req.Header.Set("Authorization", *h)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			return rec.Code, rec.Body.String()
		}
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if h != nil {
			req.Header.Set("Authorization", *h)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for _, row := range rows {
		t.Run(row.id+"_"+row.deliver, func(t *testing.T) {
			code, body := send(t, row.deliver, "/probe", row.header)
			if code != http.StatusUnauthorized {
				t.Errorf("受保护探针 got=%d want=401: %s", code, body)
			}
			if strings.Contains(body, marker) {
				t.Errorf("响应里出现 bootstrap 身份（提权）: %s", body)
			}
			code, body = send(t, row.deliver, "/me", row.header)
			var v map[string]any
			_ = json.Unmarshal([]byte(body), &v)
			if code != http.StatusOK || v["authenticated"] != false || v["user"] != nil {
				t.Errorf("/me got=(%d,authenticated=%v,user=%v) want=(200,false,nil)", code, v["authenticated"], v["user"])
			}
			if strings.Contains(body, marker) {
				t.Errorf("/me 里出现 bootstrap 身份（提权）: %s", body)
			}
		})
	}
}

func ptr(s string) *string { return &s }

func TestAdminTokenStillWinsOverPAT(t *testing.T) {
	st := testStoreForAPI(t)
	me := func(a *Auth, h string) map[string]any {
		req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
		req.Header.Set("Authorization", h)
		rec := httptest.NewRecorder()
		a.Me(rec, req)
		var v map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		return v
	}

	t.Run("给定配置了管理员令牌_断言管理员令牌先于 PAT 判定且不触 PAT 库", func(t *testing.T) {
		never := &fakePATs{}
		a := NewAuth("secret-token").WithStore(st.NewUsers(), st.NewSessions(), nil, false).WithPATs(never)
		v := me(a, "Bearer secret-token")
		if v["authenticated"] != true {
			t.Errorf("authenticated got=%v want=true", v["authenticated"])
		}
		if never.lookups != 0 {
			t.Errorf("PAT Lookup 调用次数 got=%d want=0", never.lookups)
		}
	})
	t.Run("给定未配置管理员令牌但接了库_断言 PAT 仍可用且 Lookup 调用 1 次", func(t *testing.T) {
		fake := &fakePATs{user: &store.User{ID: 4242, Email: "fake-pat@pat-test.example.com", Role: store.RoleMember}}
		a := NewAuth("").WithStore(st.NewUsers(), st.NewSessions(), nil, false).WithPATs(fake)
		v := me(a, "Bearer lathe_pat_"+strings.Repeat("x", 43))
		if v["authenticated"] != true {
			t.Errorf("authenticated got=%v want=true", v["authenticated"])
		}
		if fake.lookups != 1 || fake.touches != 1 {
			t.Errorf("Lookup/Touch 调用次数 got=(%d,%d) want=(1,1)", fake.lookups, fake.touches)
		}
	})
}

// ---------------------------------------------------------------- AC-7 / AC-8

func TestPATExpiryAndRevoke(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()

	t.Run("1_给定过去的 expiresAt_断言签发 400", func(t *testing.T) {
		a := newPATActor(t, f, store.RoleMember)
		past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		code, body := patDo(t, f, a.c, "POST", "/api/pat",
			`{"name":"x","currentPassword":"`+a.pw+`","expiresAt":"`+past+`"}`, nil)
		if code != http.StatusBadRequest {
			t.Errorf("got=%d want=400: %s", code, body)
		}
	})
	t.Run("2_给定 expiresInDays=400_断言签发 400（超过 365 天）", func(t *testing.T) {
		a := newPATActor(t, f, store.RoleMember)
		code, body := patDo(t, f, a.c, "POST", "/api/pat",
			`{"name":"x","currentPassword":"`+a.pw+`","expiresInDays":400}`, nil)
		if code != http.StatusBadRequest {
			t.Errorf("got=%d want=400: %s", code, body)
		}
	})
	t.Run("3_给定库里已过期的令牌_断言 401 且 /api/me 未认证", func(t *testing.T) {
		a := newPATActor(t, f, store.RoleMember)
		tok, id := a.issue(t, f, "x", `,"expiresInDays":30`)
		if code, _ := actorOn(t, f, nil, tok); code != http.StatusOK {
			t.Fatalf("过期前 got=%d want=200", code)
		}
		if _, err := f.st.Pool().Exec(ctx, `UPDATE personal_access_tokens SET expires_at = now() - interval '1 minute' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		if code, _ := actorOn(t, f, nil, tok); code != http.StatusUnauthorized {
			t.Errorf("过期后 got=%d want=401", code)
		}
		meUnauthenticated(t, f, nil, bearer(tok), "")
	})
	t.Run("4_给定吊销后的令牌_断言 401、列表不含、带有效 Cookie 也不回落", func(t *testing.T) {
		a := newPATActor(t, f, store.RoleMember)
		tok, id := a.issue(t, f, "x", "")
		if code, body := patDo(t, f, a.c, "DELETE", "/api/pat/"+strconv.FormatInt(id, 10), "", nil); code != http.StatusOK {
			t.Fatalf("吊销 got=%d want=200: %s", code, body)
		}
		if code, _ := actorOn(t, f, nil, tok); code != http.StatusUnauthorized {
			t.Errorf("吊销后 got=%d want=401", code)
		}
		meUnauthenticated(t, f, nil, bearer(tok), "")
		_, body := patDo(t, f, a.c, "GET", "/api/pat", "", nil)
		if strings.Contains(body, `"id":`+strconv.FormatInt(id, 10)+`,`) {
			t.Errorf("吊销后列表仍含该令牌: %s", body)
		}
		// 加强：已吊销 Bearer + 有效会话 Cookie 仍须 401
		if code, _ := actorOn(t, f, a.c, tok); code != http.StatusUnauthorized {
			t.Errorf("已吊销 Bearer + 有效 Cookie got=%d want=401（不得回落成会话身份）", code)
		}
		if code, _ := patDo(t, f, a.c, "GET", "/api/protected", "", nil); code != http.StatusOK {
			t.Errorf("只带 Cookie got=%d want=200（会话本身仍有效）", code)
		}
	})
}

func TestPATLastUsedAtRecordedAndThrottled(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	a := newPATActor(t, f, store.RoleMember)
	tok, id := a.issue(t, f, "x", "")

	state := func() (*time.Time, int64) {
		var at *time.Time
		var n int64
		if err := f.st.Pool().QueryRow(ctx, `SELECT last_used_at, use_count FROM personal_access_tokens WHERE id = $1`, id).Scan(&at, &n); err != nil {
			t.Fatal(err)
		}
		return at, n
	}
	if at, n := state(); at != nil || n != 0 {
		t.Fatalf("新令牌 got=(%v,%d) want=(nil,0)", at, n)
	}
	actorOn(t, f, nil, tok)
	if at, n := state(); at == nil || n != 1 {
		t.Fatalf("首次使用后 got=(%v,%d) want=(非空,1)", at, n)
	}
	for i := 0; i < 5; i++ {
		actorOn(t, f, nil, tok)
	}
	if _, n := state(); n != 1 {
		t.Errorf("节流窗口内 use_count got=%d want=1", n)
	}
	if _, err := f.st.Pool().Exec(ctx, `UPDATE personal_access_tokens SET last_used_at = now() - interval '2 minutes' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	actorOn(t, f, nil, tok)
	if _, n := state(); n != 2 {
		t.Errorf("回拨后 use_count got=%d want=2", n)
	}
}

// ---------------------------------------------------------------- AC-10 / AC-11

func TestPATManagementRequiresCookie(t *testing.T) {
	f := newAccountFixture(t)

	type shape struct {
		id         string
		cookie     bool
		auth       string // "" | "pat" | "admin" | "junk"
		wantStatus map[string]int
	}
	forbidAll := map[string]int{"GET": 403, "POST": 403, "DELETE": 403}
	shapes := []shape{
		{"1_PAT作Bearer无Cookie", false, "pat", forbidAll},
		{"2_管理员令牌作Bearer无Cookie", false, "admin", forbidAll},
		{"3_PAT作Bearer加有效Cookie", true, "pat", forbidAll},
		{"4_仅有效Cookie", true, "", map[string]int{"GET": 200, "POST": 201, "DELETE": 200}},
		{"5_都没有", false, "", map[string]int{"GET": 401, "POST": 401, "DELETE": 401}},
		{"6_垃圾Bearer加有效Cookie", true, "junk", forbidAll},
	}
	for _, method := range []string{"GET", "POST", "DELETE"} {
		for _, s := range shapes {
			t.Run(method+"_"+s.id, func(t *testing.T) {
				a := newPATActor(t, f, store.RoleMember)
				tok, id := a.issue(t, f, "seed", "")
				var c *http.Client
				if s.cookie {
					c = a.c
				}
				var hdr map[string]string
				switch s.auth {
				case "pat":
					hdr = bearer(tok)
				case "admin":
					hdr = bearer(apiTestToken)
				case "junk":
					hdr = bearer("not-a-real-token")
				}
				path, body := "/api/pat", ""
				switch method {
				case "POST":
					body = `{"name":"via-` + s.id + `","currentPassword":"` + a.pw + `"}`
				case "DELETE":
					path = "/api/pat/" + strconv.FormatInt(id, 10)
				}
				before := patCount(t, f, a.u.ID)
				code, resp := patDo(t, f, c, method, path, body, hdr)
				if code != s.wantStatus[method] {
					t.Fatalf("got=%d want=%d: %s", code, s.wantStatus[method], resp)
				}
				if code == http.StatusForbidden {
					if !strings.Contains(resp, "浏览器登录态") {
						t.Errorf("403 文案 got=%s want 含「浏览器登录态」", resp)
					}
					if after := patCount(t, f, a.u.ID); after != before {
						t.Errorf("被拒后 PAT 行数 got=%d want=%d（不得先做后拒）", after, before)
					}
					if method == "DELETE" && patRevokedAt(t, f, id) != nil {
						t.Errorf("被拒的 DELETE 仍吊销了令牌")
					}
				}
			})
		}
	}
}

func TestPATQueryParamIgnored(t *testing.T) {
	f := newAccountFixture(t)
	a := newPATActor(t, f, store.RoleMember)
	tok, _ := a.issue(t, f, "x", "")
	for _, q := range []string{"token", "access_token"} {
		t.Run("给定 ?"+q+"=PAT_断言 /api/me 未认证且受保护端点 401", func(t *testing.T) {
			meUnauthenticated(t, f, nil, nil, "/api/me?"+q+"="+tok)
			if code, body := patDo(t, f, nil, "GET", "/api/protected?"+q+"="+tok, "", nil); code != http.StatusUnauthorized {
				t.Errorf("/api/protected got=%d want=401: %s", code, body)
			}
		})
	}
}

// ---------------------------------------------------------------- AC-13 / AC-14 / AC-15

func TestPATMaxPerUser(t *testing.T) {
	f := newAccountFixture(t)
	ctx := context.Background()
	a := newPATActor(t, f, store.RoleMember)
	pats := f.st.NewPATs()
	var ids []int64
	for i := 0; i < 20; i++ {
		_, id, err := pats.Create(ctx, a.u.ID, "seed-"+strconv.Itoa(i), nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	post := func() (int, string) {
		return patDo(t, f, a.c, "POST", "/api/pat", `{"name":"http","currentPassword":"`+a.pw+`"}`, nil)
	}

	if code, body := post(); code != http.StatusConflict || !strings.Contains(body, "吊销") {
		t.Fatalf("第 21 把 got=(%d,%s) want=(409,含「吊销」)", code, body)
	}
	if code, _ := patDo(t, f, a.c, "DELETE", "/api/pat/"+strconv.FormatInt(ids[0], 10), "", nil); code != http.StatusOK {
		t.Fatalf("吊销一把 got=%d want=200", code)
	}
	if code, body := post(); code != http.StatusCreated {
		t.Fatalf("吊销一把后 got=%d want=201: %s", code, body)
	}
	if _, err := f.st.Pool().Exec(ctx, `UPDATE personal_access_tokens SET expires_at = now() - interval '1 minute' WHERE id = $1`, ids[1]); err != nil {
		t.Fatal(err)
	}
	if code, body := post(); code != http.StatusCreated {
		t.Fatalf("过期一把后 got=%d want=201（过期不占名额）: %s", code, body)
	}
	_, body := patDo(t, f, a.c, "GET", "/api/pat", "", nil)
	tokens, _ := jsonOf(t, body)["tokens"].([]any)
	expired, live := 0, 0
	for _, raw := range tokens {
		item, _ := raw.(map[string]any)
		if id, _ := item["id"].(float64); int64(id) == ids[0] {
			t.Errorf("已吊销的令牌出现在列表里: %v", item)
		}
		if item["expired"] == true {
			expired++
		} else {
			live++
		}
	}
	if len(tokens) != 21 || expired != 1 || live != 20 {
		t.Errorf("列表 got=(总%d,过期%d,有效%d) want=(21,1,20)", len(tokens), expired, live)
	}
}

func TestPATNameValidation(t *testing.T) {
	f := newAccountFixture(t)
	tests := []struct {
		name     string
		patName  string
		want     int
		wantEcho bool
	}{
		{"给定空名_断言 400", "", http.StatusBadRequest, false},
		{"给定纯空白名_断言 400", "   \t ", http.StatusBadRequest, false},
		{"给定 63 字节 ASCII_断言 201", strings.Repeat("a", 63), http.StatusCreated, true},
		{"给定 64 字节 ASCII_断言 201（边界）", strings.Repeat("b", 64), http.StatusCreated, true},
		{"给定 65 字节 ASCII_断言 400", strings.Repeat("c", 65), http.StatusBadRequest, false},
		{"给定 22 个四字节 emoji（88 字节）_断言 400", strings.Repeat("😀", 22), http.StatusBadRequest, false},
		{"给定带 <script> 的名字_断言原样存取", `<script>alert(1)</script>`, http.StatusCreated, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newPATActor(t, f, store.RoleMember)
			nameJSON, _ := json.Marshal(tc.patName)
			code, body := patDo(t, f, a.c, "POST", "/api/pat",
				`{"name":`+string(nameJSON)+`,"currentPassword":"`+a.pw+`"}`, nil)
			if code != tc.want {
				t.Fatalf("got=%d want=%d: %s", code, tc.want, body)
			}
			if tc.wantEcho {
				if got := jsonOf(t, body)["name"]; got != tc.patName {
					t.Errorf("响应 name got=%v want=%q", got, tc.patName)
				}
				var stored string
				if err := f.st.Pool().QueryRow(context.Background(),
					`SELECT name FROM personal_access_tokens WHERE user_id = $1`, a.u.ID).Scan(&stored); err != nil {
					t.Fatal(err)
				}
				if stored != tc.patName {
					t.Errorf("库里 name got=%q want=%q", stored, tc.patName)
				}
			} else if n := patCount(t, f, a.u.ID); n != 0 {
				t.Errorf("400 后行数 got=%d want=0", n)
			}
		})
	}
}

func TestPATListIsOwnerScoped(t *testing.T) {
	f := newAccountFixture(t)
	a := newPATActor(t, f, store.RoleMember)
	b := newPATActor(t, f, store.RoleMember)
	_, a1 := a.issue(t, f, "a1", "")
	_, a2 := a.issue(t, f, "a2", "")
	_, b1 := b.issue(t, f, "b1", "")

	ids := func(c *http.Client) map[int64]bool {
		_, body := patDo(t, f, c, "GET", "/api/pat", "", nil)
		out := map[int64]bool{}
		tokens, _ := jsonOf(t, body)["tokens"].([]any)
		for _, raw := range tokens {
			item, _ := raw.(map[string]any)
			id, _ := item["id"].(float64)
			out[int64(id)] = true
		}
		return out
	}
	tests := []struct {
		name string
		c    *http.Client
		want map[int64]bool
	}{
		{"给定 A 有两把_断言 A 只看到自己的两把", a.c, map[int64]bool{a1: true, a2: true}},
		{"给定 B 有一把_断言 B 只看到自己的一把", b.c, map[int64]bool{b1: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ids(tc.c)
			if len(got) != len(tc.want) {
				t.Fatalf("项数 got=%v want=%v", got, tc.want)
			}
			for id := range tc.want {
				if !got[id] {
					t.Errorf("缺 id %d: got=%v want=%v", id, got, tc.want)
				}
			}
		})
	}
}

// ---------------------------------------------------------------- AC-19

func TestPasswordChangeRevokesPATs(t *testing.T) {
	f := newAccountFixture(t)

	tests := []struct {
		name   string
		change func(t *testing.T, a *patActor, tokens []string)
	}{
		{"1_给定本人改密_断言两把 PAT 401 且当前会话仍有效", func(t *testing.T, a *patActor, _ []string) {
			code, body := patDo(t, f, a.c, "POST", "/api/password/change",
				`{"currentPassword":"`+a.pw+`","newPassword":"changed-password-`+patRand(t)+`"}`, nil)
			if code != http.StatusOK {
				t.Fatalf("改密 got=%d want=200: %s", code, body)
			}
			if code, _ := patDo(t, f, a.c, "GET", "/api/protected", "", nil); code != http.StatusOK {
				t.Errorf("改密后当前会话 got=%d want=200", code)
			}
		}},
		{"2_给定邮件找回重置_断言两把 PAT 401", func(t *testing.T, a *patActor, _ []string) {
			patDo(t, f, nil, "POST", "/api/password/forgot", `{"email":"`+a.u.Email+`"}`, nil)
			var tok string
			deadline := time.After(5 * time.Second)
			for tok == "" {
				select {
				case m := <-f.mail.sent:
					if m.to == a.u.Email {
						if i := strings.Index(m.body, "token="); i >= 0 {
							tok = m.body[i+6 : i+6+64]
						}
					}
				case <-deadline:
					t.Fatal("没有收到重置邮件")
				}
			}
			code, body := patDo(t, f, nil, "POST", "/api/password/reset",
				`{"token":"`+tok+`","password":"reset-password-`+patRand(t)+`"}`, nil)
			if code != http.StatusOK {
				t.Fatalf("重置 got=%d want=200: %s", code, body)
			}
		}},
		{"3_给定管理员代重置后本人再改密_断言两把 PAT 先 401 且改密后仍 401", func(t *testing.T, a *patActor, tokens []string) {
			admin := newPATActor(t, f, store.RoleAdmin)
			newPW := "admin-set-password-" + patRand(t)
			code, body := patDo(t, f, admin.c, "POST", "/api/admin/users/"+strconv.FormatInt(a.u.ID, 10)+"/password",
				`{"password":"`+newPW+`"}`, nil)
			if code != http.StatusOK {
				t.Fatalf("代重置 got=%d want=200: %s", code, body)
			}
			for i, tok := range tokens {
				if code, _ := actorOn(t, f, nil, tok); code != http.StatusUnauthorized {
					t.Errorf("代重置后第 %d 把 got=%d want=401（不是 409）", i+1, code)
				}
			}
			jar, _ := cookiejar.New(nil)
			c := &http.Client{Jar: jar}
			if code, body := patDo(t, f, c, "POST", "/api/login", `{"email":"`+a.u.Email+`","password":"`+newPW+`"}`, nil); code != http.StatusOK {
				t.Fatalf("新口令登录 got=%d want=200: %s", code, body)
			}
			if code, body := patDo(t, f, c, "POST", "/api/password/change",
				`{"currentPassword":"`+newPW+`","newPassword":"final-password-`+patRand(t)+`"}`, nil); code != http.StatusOK {
				t.Fatalf("解除强制改密 got=%d want=200: %s", code, body)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newPATActor(t, f, store.RoleMember)
			t90, id90 := a.issue(t, f, "ninety", `,"expiresInDays":90`)
			tInf, idInf := a.issue(t, f, "forever", "")
			tokens := []string{t90, tInf}
			for i, tok := range tokens {
				if code, _ := actorOn(t, f, nil, tok); code != http.StatusOK {
					t.Fatalf("改口令前第 %d 把 got=%d want=200", i+1, code)
				}
			}
			tc.change(t, a, tokens)
			for i, tok := range tokens {
				if code, _ := actorOn(t, f, nil, tok); code != http.StatusUnauthorized {
					t.Errorf("改口令后第 %d 把 got=%d want=401", i+1, code)
				}
			}
			for _, id := range []int64{id90, idInf} {
				if patRevokedAt(t, f, id) == nil {
					t.Errorf("令牌 %d revoked_at got=nil want=非空", id)
				}
			}
		})
	}
}

// failingRevoker 的 RevokeUser 必返回错误。
type failingRevoker struct{}

func (failingRevoker) RevokeUser(context.Context, int64) (int64, error) {
	return 0, errors.New("fake: 吊销失败")
}

func TestPasswordChangeAbortsIfRevokeFails(t *testing.T) {
	tests := []struct {
		name   string
		revoke PATRevoker // nil 表示「未接线」
		path   func(f *accountFixture, a *patActor) (client *http.Client, method, path, body string)
	}{
		{"给定本人改密且吊销失败_断言 500 且旧口令仍可登录", failingRevoker{}, func(f *accountFixture, a *patActor) (*http.Client, string, string, string) {
			return a.c, "POST", "/api/password/change", `{"currentPassword":"` + a.pw + `","newPassword":"never-applied-pw-1"}`
		}},
		{"给定本人改密且 PATs 未接线_断言 500 且旧口令仍可登录", nil, func(f *accountFixture, a *patActor) (*http.Client, string, string, string) {
			return a.c, "POST", "/api/password/change", `{"currentPassword":"` + a.pw + `","newPassword":"never-applied-pw-1"}`
		}},
		{"给定管理员代重置且吊销失败_断言 500 且旧口令仍可登录", failingRevoker{}, nil},
		{"给定管理员停用且吊销失败_断言 500", failingRevoker{}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newAccountFixture(t)
			a := newPATActor(t, f, store.RoleMember)
			f.acct.PATs = tc.revoke
			f.admin.PATs = tc.revoke
			var c *http.Client
			var method, path, body string
			if tc.path != nil {
				c, method, path, body = tc.path(f, a)
			} else {
				admin := newPATActor(t, f, store.RoleAdmin)
				c, method = admin.c, "POST"
				if strings.Contains(tc.name, "停用") {
					path = "/api/admin/users/" + strconv.FormatInt(a.u.ID, 10) + "/disable"
				} else {
					path, body = "/api/admin/users/"+strconv.FormatInt(a.u.ID, 10)+"/password", `{"password":"never-applied-pw-2"}`
				}
			}
			if code, resp := patDo(t, f, c, method, path, body, nil); code != http.StatusInternalServerError {
				t.Fatalf("got=%d want=500: %s", code, resp)
			}
			if strings.Contains(tc.name, "停用") {
				return
			}
			jar, _ := cookiejar.New(nil)
			if code, resp := patDo(t, f, &http.Client{Jar: jar}, "POST", "/api/login",
				`{"email":"`+a.u.Email+`","password":"`+a.pw+`"}`, nil); code != http.StatusOK {
				t.Errorf("旧口令登录 got=%d want=200（口令不该被改）: %s", code, resp)
			}
		})
	}
}

// ---------------------------------------------------------------- AC-20

func TestPATCreateRequiresCurrentPassword(t *testing.T) {
	f := newAccountFixture(t)
	type step struct {
		body func(a *patActor) string
		want int
		msg  string
	}
	wrong := func(*patActor) string { return `{"name":"x","currentPassword":"wrong-password-1"}` }
	tests := []struct {
		name  string
		steps []step
	}{
		{"1_给定不带 currentPassword_断言 400", []step{{func(*patActor) string { return `{"name":"x"}` }, 400, "当前密码"}}},
		{"2_给定空 currentPassword_断言 400", []step{{func(*patActor) string { return `{"name":"x","currentPassword":""}` }, 400, "当前密码"}}},
		{"3_给定错误口令_断言 403 而不是 401", []step{{wrong, 403, "当前密码不正确"}}},
		{"4_给定 10 次错口令后带正确口令_断言前 10 次 403 第 11 次 429", func() []step {
			var s []step
			for i := 0; i < 10; i++ {
				s = append(s, step{wrong, 403, ""})
			}
			return append(s, step{func(a *patActor) string { return `{"name":"x","currentPassword":"` + a.pw + `"}` }, 429, ""})
		}()},
		{"5_给定正确口令_断言 201", []step{{func(a *patActor) string { return `{"name":"x","currentPassword":"` + a.pw + `"}` }, 201, ""}}},
		{"6_给定空名且口令错误_断言 403（口令先于名字）", []step{{func(*patActor) string { return `{"name":"","currentPassword":"wrong-password-1"}` }, 403, ""}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := newPATActor(t, f, store.RoleMember)
			for i, s := range tc.steps {
				code, body := patDo(t, f, a.c, "POST", "/api/pat", s.body(a), nil)
				if code != s.want {
					t.Fatalf("第 %d 次 got=%d want=%d: %s", i+1, code, s.want, body)
				}
				if s.msg != "" && !strings.Contains(body, s.msg) {
					t.Errorf("第 %d 次文案 got=%s want 含 %q", i+1, body, s.msg)
				}
			}
			wantRows := 0
			if tc.steps[len(tc.steps)-1].want == http.StatusCreated {
				wantRows = 1
			}
			if n := patCount(t, f, a.u.ID); n != wantRows {
				t.Errorf("PAT 行数 got=%d want=%d", n, wantRows)
			}
			if code, _ := patDo(t, f, a.c, "GET", "/api/protected", "", nil); code != http.StatusOK {
				t.Errorf("口令错不该踢人：会话 got=%d want=200", code)
			}
			if strings.HasPrefix(tc.name, "4_") {
				other := newPATActor(t, f, store.RoleMember)
				if code, body := patDo(t, f, other.c, "POST", "/api/pat",
					`{"name":"x","currentPassword":"`+other.pw+`"}`, nil); code != http.StatusCreated {
					t.Errorf("另一个用户紧接着签发 got=%d want=201（额度按用户隔离）: %s", code, body)
				}
			}
		})
	}
}
