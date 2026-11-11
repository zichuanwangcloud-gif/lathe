package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// ---------------------------------------------------------------- 团队测试环境
//
// 同一份数据、每个用户一台服务（authAs 把令牌通道映射到该用户）——与
// TestAPIIsolationBetweenUsers 同一手法。fixture 的团队/成员/共享一律用 SQL
// 直接造，不经被测函数，红灯才落在行为断言上。

type teamEnv struct {
	t     *testing.T
	st    *store.Store
	m     *task.Machine
	mu    sync.Mutex
	srvs  map[int64]*httptest.Server
	apis  map[int64]*TeamAPI
	lim   *limiter
	clock func() time.Time
	proxy bool
}

func newTeamEnv(t *testing.T) *teamEnv {
	t.Helper()
	st := testStoreForAPI(t)
	return &teamEnv{
		t: t, st: st, m: task.NewMachine(st.Pool()),
		srvs: map[int64]*httptest.Server{}, apis: map[int64]*TeamAPI{},
		lim: newLimiter(), clock: time.Now,
	}
}

func teamRand() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (e *teamEnv) user(prefix string) int64 {
	e.t.Helper()
	return mustUser(e.t, e.st, fmt.Sprintf("f12-%s-%s@example.com", prefix, teamRand()))
}

func (e *teamEnv) repo(uid int64, infra ...string) int64 {
	e.t.Helper()
	if infra == nil {
		infra = []string{}
	}
	var id int64
	if err := e.st.Pool().QueryRow(context.Background(),
		`INSERT INTO repos (user_id, provider_repo, verify_infra) VALUES ($1,$2,$3) RETURNING id`,
		uid, "acme/f12-"+teamRand(), infra).Scan(&id); err != nil {
		e.t.Fatalf("建仓库失败: %v", err)
	}
	return id
}

func (e *teamEnv) task(uid, repoID int64) int64 {
	e.t.Helper()
	tk, err := e.m.Create(context.Background(), task.CreateParams{
		UserID: uid, RepoID: repoID, ExternalKey: "F12-" + teamRand(),
	})
	if err != nil {
		e.t.Fatalf("建任务失败: %v", err)
	}
	return tk.ID
}

// team 用 SQL 建团队；members 的 key 是 user id，value 是团队内角色。
func (e *teamEnv) team(members map[int64]string) int64 {
	e.t.Helper()
	ctx := context.Background()
	var id int64
	if err := e.st.Pool().QueryRow(ctx, `INSERT INTO teams (name) VALUES ($1) RETURNING id`,
		"f12-"+teamRand()).Scan(&id); err != nil {
		e.t.Fatalf("建团队失败: %v", err)
	}
	e.t.Cleanup(func() { _, _ = e.st.Pool().Exec(context.Background(), `DELETE FROM teams WHERE id=$1`, id) })
	for uid, role := range members {
		if _, err := e.st.Pool().Exec(ctx,
			`INSERT INTO team_members (team_id, user_id, role) VALUES ($1,$2,$3)`, id, uid, role); err != nil {
			e.t.Fatalf("加成员失败: %v", err)
		}
	}
	return id
}

func (e *teamEnv) share(repoID, teamID int64) {
	e.t.Helper()
	if _, err := e.st.Pool().Exec(context.Background(),
		`UPDATE repos SET team_id=$2 WHERE id=$1`, repoID, teamID); err != nil {
		e.t.Fatalf("共享失败: %v", err)
	}
}

func (e *teamEnv) server(uid int64) *httptest.Server {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s, ok := e.srvs[uid]; ok {
		return s
	}
	a := authAs(uid, fmt.Sprintf("u%d@example.com", uid))
	mux := http.NewServeMux()
	(&API{Store: e.st, Tasks: e.m, Queue: &fakeEnqueuer{}, Auth: a}).Routes(mux)
	ta := &TeamAPI{Store: e.st, Auth: a, TrustProxy: e.proxy, limiter: e.lim, now: func() time.Time { return e.clock() }}
	ta.Routes(mux)
	(&PreviewAPI{Store: e.st, Auth: a, Previews: &fakePreviews{}}).Routes(mux)
	s := httptest.NewServer(mux)
	e.t.Cleanup(s.Close)
	e.srvs[uid], e.apis[uid] = s, ta
	return s
}

// raw 发请求，返回状态码与原始响应体；不调用 t.Fatal，可在 goroutine 里用。
func (e *teamEnv) raw(uid int64, method, path, body string, hdr ...string) (int, []byte, error) {
	srv := e.server(uid)
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r, err := http.NewRequest(method, srv.URL+path, rd)
	if err != nil {
		return 0, nil, err
	}
	r.Header.Set("Authorization", "Bearer "+apiTestToken)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := srv.Client().Do(r)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func (e *teamEnv) call(uid int64, method, path, body string, hdr ...string) (int, map[string]any) {
	e.t.Helper()
	code, b, err := e.raw(uid, method, path, body, hdr...)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	var v map[string]any
	_ = json.Unmarshal(b, &v)
	return code, v
}

func (e *teamEnv) status(uid int64, method, path, body string) int {
	e.t.Helper()
	code, _ := e.call(uid, method, path, body)
	return code
}

func (e *teamEnv) role(teamID, uid int64) string {
	e.t.Helper()
	var role string
	err := e.st.Pool().QueryRow(context.Background(),
		`SELECT role FROM team_members WHERE team_id=$1 AND user_id=$2`, teamID, uid).Scan(&role)
	if err != nil {
		return ""
	}
	return role
}

func (e *teamEnv) count(q string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.st.Pool().QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (e *teamEnv) repoTeam(repoID int64) *int64 {
	e.t.Helper()
	var tid *int64
	if err := e.st.Pool().QueryRow(context.Background(), `SELECT team_id FROM repos WHERE id=$1`, repoID).Scan(&tid); err != nil {
		e.t.Fatalf("查 team_id 失败: %v", err)
	}
	return tid
}

// invite 用 SQL 造一条 pending 邀请码，返回明文码与邀请 id。
func (e *teamEnv) invite(teamID int64, role string) (string, int64) {
	e.t.Helper()
	code := "f12code" + teamRand() + teamRand()
	var id int64
	if err := e.st.Pool().QueryRow(context.Background(), `
		INSERT INTO team_invitations (team_id, code_hash, code_hint, role, expires_at)
		VALUES ($1, sha256(convert_to($2::text, 'UTF8')), right($2::text, 4), $3, now() + interval '48 hours') RETURNING id`,
		teamID, code, role).Scan(&id); err != nil {
		e.t.Fatalf("造邀请失败: %v", err)
	}
	return code, id
}

func p(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// sharedSetup：属主 owner 的仓库共享到团队 T（owner 是 T 的 member，mate 是 T 的 member），
// 外加一个与团队无关的 stranger。返回各 id 与属主的一条任务。
type sharedSetup struct {
	owner, mate, stranger, team, repo, task int64
}

func newSharedSetup(e *teamEnv) sharedSetup {
	e.t.Helper()
	s := sharedSetup{owner: e.user("owner"), mate: e.user("mate"), stranger: e.user("stranger")}
	s.team = e.team(map[int64]string{s.owner: store.TeamRoleOwner, s.mate: store.TeamRoleMember})
	s.repo = e.repo(s.owner)
	e.share(s.repo, s.team)
	s.task = e.task(s.owner, s.repo)
	return s
}

// taskReadRoutes 是每一个任务【读】接口的显式表（AC-54）。f04 合入后由后合入方补
// GET /api/tasks/{id}/events/summary 与 GET /api/tasks/{id}/verifications/{vid}/log。
var taskReadRoutes = []struct{ method, path string }{
	{"GET", "/api/tasks/{id}"},
	{"GET", "/api/tasks/{id}/events?after=0"},
}

// ---------------------------------------------------------------- 5.2 隔离与可见性

// AC-6
func TestTeamSharedRepoInvisibleToNonMember(t *testing.T) {
	e := newTeamEnv(t)
	s := newSharedSetup(e)
	cases := []struct {
		name string
		uid  int64
		path string
		want int
	}{
		{"非成员读任务详情 → 404", s.stranger, p("/api/tasks/%d", s.task), 404},
		{"非成员读事件流 → 404", s.stranger, p("/api/tasks/%d/events", s.task), 404},
		{"非成员读团队详情 → 404", s.stranger, p("/api/teams/%d", s.team), 404},
		{"对照：成员读任务详情 → 200", s.mate, p("/api/tasks/%d", s.task), 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := e.status(c.uid, "GET", c.path, ""); got != c.want {
				t.Errorf("got %d want %d", got, c.want)
			}
		})
	}
}

// AC-7
func TestTeamMemberRemovalTakesEffectImmediately(t *testing.T) {
	e := newTeamEnv(t)
	s := newSharedSetup(e)
	path := p("/api/tasks/%d", s.task)
	if got := e.status(s.mate, "GET", path, ""); got != 200 {
		t.Fatalf("移除前 got %d want 200", got)
	}
	// 团队 owner（属主）移除 mate
	if got := e.status(s.owner, "DELETE", p("/api/teams/%d/members/%d", s.team, s.mate), ""); got != 200 {
		t.Fatalf("移除 got %d want 200", got)
	}
	if got := e.status(s.mate, "GET", path, ""); got != 404 {
		t.Errorf("移除后 got %d want 404", got)
	}
}

// AC-8
func TestRepoUnshareRevokesVisibilityImmediately(t *testing.T) {
	e := newTeamEnv(t)
	s := newSharedSetup(e)
	path := p("/api/tasks/%d", s.task)
	if got := e.status(s.mate, "GET", path, ""); got != 200 {
		t.Fatalf("取消前 got %d want 200", got)
	}
	if got := e.status(s.owner, "POST", p("/api/repos/%d/sharing", s.repo), `{"teamId":null}`); got != 200 {
		t.Fatalf("取消共享 got %d want 200", got)
	}
	if got := e.status(s.mate, "GET", path, ""); got != 404 {
		t.Errorf("取消后 got %d want 404", got)
	}
}

// AC-9
func TestTeamMemberReadsOthersTask(t *testing.T) {
	e := newTeamEnv(t)
	s := newSharedSetup(e)
	code, body := e.call(s.mate, "GET", p("/api/tasks/%d", s.task), "")
	if code != 200 {
		t.Fatalf("详情 got %d want 200", code)
	}
	tk, _ := body["task"].(map[string]any)
	if got := int64(tk["userId"].(float64)); got != s.owner {
		t.Errorf("task.userId got %d want %d", got, s.owner)
	}
	if got, _ := tk["repoId"].(float64); int64(got) != s.repo {
		t.Errorf("task.repoId got %v want %d", tk["repoId"], s.repo)
	}
	if got := body["viewerRole"]; got != "team_member" {
		t.Errorf("viewerRole got %v want team_member", got)
	}
	// 属主自己看：viewerRole = owner
	_, own := e.call(s.owner, "GET", p("/api/tasks/%d", s.task), "")
	if got := own["viewerRole"]; got != "owner" {
		t.Errorf("属主 viewerRole got %v want owner", got)
	}
	for _, rt := range taskReadRoutes {
		path := strings.Replace(rt.path, "{id}", itoa(s.task), 1)
		if got := e.status(s.mate, rt.method, path, ""); got != 200 {
			t.Errorf("%s %s got %d want 200", rt.method, path, got)
		}
	}
}

// AC-54 ①
func TestTeamMemberReadsEveryTaskReadRoute(t *testing.T) {
	e := newTeamEnv(t)
	s := newSharedSetup(e)
	_, notFound, err := e.raw(s.stranger, "GET", "/api/tasks/999999999", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, rt := range taskReadRoutes {
		path := strings.Replace(rt.path, "{id}", itoa(s.task), 1)
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			if got := e.status(s.mate, rt.method, path, ""); got != 200 {
				t.Errorf("成员 got %d want 200", got)
			}
			code, b, err := e.raw(s.stranger, rt.method, path, "")
			if err != nil {
				t.Fatal(err)
			}
			if code != 404 || !bytes.Equal(b, notFound) {
				t.Errorf("非成员 got %d %s want 404 %s", code, b, notFound)
			}
		})
	}
}

// AC-10
func TestListTasksScopeTeam(t *testing.T) {
	e := newTeamEnv(t)
	s := newSharedSetup(e)
	myRepo := e.repo(s.mate)
	mine := e.task(s.mate, myRepo)

	_, def := e.call(s.mate, "GET", "/api/tasks", "")
	_, team := e.call(s.mate, "GET", "/api/tasks?scope=team", "")

	ids := func(body map[string]any) map[int64]map[string]any {
		out := map[int64]map[string]any{}
		for _, x := range body["tasks"].([]any) {
			row := x.(map[string]any)
			out[int64(row["id"].(float64))] = row
		}
		return out
	}
	d, tm := ids(def), ids(team)
	if got := def["total"].(float64); got != 1 {
		t.Errorf("默认 total got %v want 1", got)
	}
	if got := team["total"].(float64); got != 2 {
		t.Errorf("scope=team total got %v want 2", got)
	}
	if _, ok := d[mine]; !ok {
		t.Errorf("默认视图应含自己的任务")
	}
	if _, ok := d[s.task]; ok {
		t.Errorf("默认视图不应含团队任务")
	}
	row, ok := tm[s.task]
	if !ok {
		t.Fatalf("scope=team 应含团队任务 %d", s.task)
	}
	if got, _ := row["repoId"].(float64); int64(got) != s.repo {
		t.Errorf("团队任务 repoId got %v want %d", row["repoId"], s.repo)
	}
	if _, ok := tm[mine]; !ok {
		t.Errorf("scope=team 应含自己的任务")
	}
}

// ---------------------------------------------------------------- 5.3 写权限

// AC-11
func TestTeamMemberCannotMutateOthersTask(t *testing.T) {
	e := newTeamEnv(t)
	s := newSharedSetup(e)
	before, _ := e.m.Get(context.Background(), s.task)
	for _, c := range []struct{ action, body string }{
		{"retry", `{"mode":"auto"}`}, {"cancel", `{}`}, {"approve", `{}`}, {"review-retry", `{}`},
	} {
		t.Run(c.action, func(t *testing.T) {
			if got := e.status(s.mate, "POST", p("/api/tasks/%d/%s", s.task, c.action), c.body); got != 404 {
				t.Errorf("got %d want 404", got)
			}
		})
	}
	after, _ := e.m.Get(context.Background(), s.task)
	if after.State != before.State {
		t.Errorf("任务状态 got %s want %s（不变）", after.State, before.State)
	}
	t.Run("对照：属主用同一 body retry 不是 400", func(t *testing.T) {
		if got := e.status(s.owner, "POST", p("/api/tasks/%d/retry", s.task), `{"mode":"auto"}`); got == 400 {
			t.Errorf("got 400：body 不合法，测的就不是归属了")
		}
	})
}

// matrixTeam 是成员管理逐格表的一套团队：owner、两个 admin、两个 member。
type matrixTeam struct {
	id, owner, admin, admin2, member, member2 int64
}

func newMatrixTeam(e *teamEnv) matrixTeam {
	e.t.Helper()
	m := matrixTeam{owner: e.user("mo"), admin: e.user("ma"), admin2: e.user("ma2"), member: e.user("mm"), member2: e.user("mm2")}
	m.id = e.team(map[int64]string{
		m.owner: store.TeamRoleOwner, m.admin: store.TeamRoleAdmin, m.admin2: store.TeamRoleAdmin,
		m.member: store.TeamRoleMember, m.member2: store.TeamRoleMember,
	})
	return m
}

// AC-12：§3.6「成员管理逐格判定表」的每一格 + 主矩阵其余行。
func TestTeamRoleMatrix(t *testing.T) {
	e := newTeamEnv(t)

	// caller 与 target 都按角色挑人：同角色时挑另一位，target=self 时就是 caller 本人
	pick := func(m matrixTeam, caller, target string) (int64, int64) {
		callers := map[string]int64{"owner": m.owner, "admin": m.admin, "member": m.member}
		c := callers[caller]
		switch target {
		case "self":
			return c, c
		case "owner":
			return c, m.owner
		case "admin":
			if caller == "admin" {
				return c, m.admin2
			}
			return c, m.admin
		default:
			if caller == "member" {
				return c, m.member2
			}
			return c, m.member
		}
	}
	type cell struct {
		caller, action, target string
		want                   int
	}
	cells := []cell{
		{"owner", "patch-admin", "admin", 200}, {"owner", "patch-admin", "member", 200}, {"owner", "patch-admin", "self", 409},
		{"owner", "patch-member", "admin", 200}, {"owner", "patch-member", "member", 200}, {"owner", "patch-member", "self", 409},
		{"owner", "delete", "admin", 200}, {"owner", "delete", "member", 200}, {"owner", "delete", "self", 409},
		{"admin", "patch-admin", "owner", 404}, {"admin", "patch-admin", "admin", 404}, {"admin", "patch-admin", "member", 404}, {"admin", "patch-admin", "self", 404},
		{"admin", "patch-member", "owner", 404}, {"admin", "patch-member", "admin", 404}, {"admin", "patch-member", "member", 404}, {"admin", "patch-member", "self", 404},
		{"admin", "delete", "owner", 404}, {"admin", "delete", "admin", 404}, {"admin", "delete", "member", 200}, {"admin", "delete", "self", 200},
		{"member", "patch-admin", "owner", 404}, {"member", "patch-admin", "admin", 404}, {"member", "patch-admin", "member", 404}, {"member", "patch-admin", "self", 404},
		{"member", "patch-member", "owner", 404}, {"member", "patch-member", "admin", 404}, {"member", "patch-member", "member", 404}, {"member", "patch-member", "self", 404},
		{"member", "delete", "owner", 404}, {"member", "delete", "admin", 404}, {"member", "delete", "member", 404}, {"member", "delete", "self", 200},
	}
	for _, c := range cells {
		t.Run(p("%s/%s/%s", c.caller, c.action, c.target), func(t *testing.T) {
			m := newMatrixTeam(e)
			caller, target := pick(m, c.caller, c.target)
			path := p("/api/teams/%d/members/%d", m.id, target)
			beforeRole := e.role(m.id, target)
			var got int
			switch c.action {
			case "patch-admin":
				got = e.status(caller, "PATCH", path, `{"role":"admin"}`)
			case "patch-member":
				got = e.status(caller, "PATCH", path, `{"role":"member"}`)
			case "delete":
				got = e.status(caller, "DELETE", path, "")
			}
			if got != c.want {
				t.Fatalf("caller=%s action=%s target=%s got %d want %d", c.caller, c.action, c.target, got, c.want)
			}
			if c.want != 200 && e.role(m.id, target) != beforeRole {
				t.Errorf("被拒后角色 got %q want %q（不变）", e.role(m.id, target), beforeRole)
			}
			if c.want == 200 {
				afterRole := e.role(m.id, target)
				switch c.action {
				case "patch-admin":
					if afterRole != "admin" {
						t.Errorf("角色 got %q want admin", afterRole)
					}
				case "patch-member":
					if afterRole != "member" {
						t.Errorf("角色 got %q want member", afterRole)
					}
				case "delete":
					if afterRole != "" {
						t.Errorf("移除后仍是成员（%q）", afterRole)
					}
				}
			}
		})
	}

	// PATCH {role:"owner"} → 400（授权之后的字段校验），非 owner 先得到 404
	for _, c := range []struct {
		caller string
		want   int
	}{{"owner", 400}, {"admin", 404}, {"member", 404}, {"stranger", 404}} {
		t.Run("patch-owner/"+c.caller, func(t *testing.T) {
			m := newMatrixTeam(e)
			callers := map[string]int64{"owner": m.owner, "admin": m.admin, "member": m.member, "stranger": e.user("ms")}
			if got := e.status(callers[c.caller], "PATCH", p("/api/teams/%d/members/%d", m.id, m.member2), `{"role":"owner"}`); got != c.want {
				t.Errorf("got %d want %d", got, c.want)
			}
		})
	}

	// 主矩阵其余动作：调用者 owner/admin/member/非成员
	type rowCase struct {
		name string
		do   func(m matrixTeam, caller int64) int
		want map[string]int
	}
	rows := []rowCase{
		{"看团队详情", func(m matrixTeam, c int64) int { return e.status(c, "GET", p("/api/teams/%d", m.id), "") },
			map[string]int{"owner": 200, "admin": 200, "member": 200, "stranger": 404}},
		{"改团队名", func(m matrixTeam, c int64) int {
			return e.status(c, "PATCH", p("/api/teams/%d", m.id), `{"name":"renamed"}`)
		}, map[string]int{"owner": 200, "admin": 404, "member": 404, "stranger": 404}},
		{"删团队", func(m matrixTeam, c int64) int { return e.status(c, "DELETE", p("/api/teams/%d", m.id), "") },
			map[string]int{"owner": 200, "admin": 404, "member": 404, "stranger": 404}},
		{"转让 owner", func(m matrixTeam, c int64) int {
			return e.status(c, "POST", p("/api/teams/%d/transfer", m.id), p(`{"userId":%d}`, m.member2))
		}, map[string]int{"owner": 200, "admin": 404, "member": 404, "stranger": 404}},
		{"生成邀请码", func(m matrixTeam, c int64) int {
			return e.status(c, "POST", p("/api/teams/%d/invitations", m.id), `{"role":"member"}`)
		}, map[string]int{"owner": 201, "admin": 201, "member": 404, "stranger": 404}},
		{"列邀请", func(m matrixTeam, c int64) int { return e.status(c, "GET", p("/api/teams/%d/invitations", m.id), "") },
			map[string]int{"owner": 200, "admin": 200, "member": 404, "stranger": 404}},
		{"撤销邀请", func(m matrixTeam, c int64) int {
			_, iid := e.invite(m.id, "member")
			return e.status(c, "DELETE", p("/api/teams/%d/invitations/%d", m.id, iid), "")
		}, map[string]int{"owner": 200, "admin": 200, "member": 404, "stranger": 404}},
	}
	for _, r := range rows {
		for _, who := range []string{"owner", "admin", "member", "stranger"} {
			t.Run(r.name+"/"+who, func(t *testing.T) {
				m := newMatrixTeam(e)
				callers := map[string]int64{"owner": m.owner, "admin": m.admin, "member": m.member, "stranger": e.user("ms")}
				if got := r.do(m, callers[who]); got != r.want[who] {
					t.Errorf("caller=%s got %d want %d", who, got, r.want[who])
				}
			})
		}
	}
}

// AC-13
func TestTeamCannotUpdateRepoConfig(t *testing.T) {
	e := newTeamEnv(t)
	owner, admin, member := e.user("rc-owner"), e.user("rc-admin"), e.user("rc-member")
	team := e.team(map[int64]string{owner: "owner", admin: "admin", member: "member"})
	repo := e.repo(owner, "redis")
	if _, err := e.st.Pool().Exec(context.Background(), `
		UPDATE repos SET gate_mode='guarded', verify_tier_override='heavy', baseline_dir='/opt/base'
		WHERE id=$1`, repo); err != nil {
		t.Fatal(err)
	}
	e.share(repo, team)

	snapshot := func() string {
		var gm, vto, bd, db string
		var pb, vi []string
		if err := e.st.Pool().QueryRow(context.Background(), `
			SELECT gate_mode, COALESCE(verify_tier_override,''), protected_branches, COALESCE(baseline_dir,''), verify_infra, default_branch
			FROM repos WHERE id=$1`, repo).Scan(&gm, &vto, &pb, &bd, &vi, &db); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%s|%s|%v|%s|%v|%s", gm, vto, pb, bd, vi, db)
	}
	before := snapshot()
	body := `{"defaultBranch":"hijack","gateMode":"direct","verifyTierOverride":"light",
		"protectedBranches":["x"],"baselineDir":"/tmp/evil","verifyInfra":["postgres"]}`
	for _, c := range []struct {
		name string
		uid  int64
	}{{"team admin", admin}, {"team member", member}} {
		t.Run(c.name, func(t *testing.T) {
			if got := e.status(c.uid, "PUT", p("/api/repos/%d", repo), body); got != 404 {
				t.Errorf("got %d want 404", got)
			}
			if got := snapshot(); got != before {
				t.Errorf("仓库配置被改：got %s want %s", got, before)
			}
		})
	}
}

// AC-14
func TestTeamAdminCannotOwn(t *testing.T) {
	e := newTeamEnv(t)
	m := newMatrixTeam(e)
	var name string
	_ = e.st.Pool().QueryRow(context.Background(), `SELECT name FROM teams WHERE id=$1`, m.id).Scan(&name)
	for _, c := range []struct{ method, path, body string }{
		{"PATCH", p("/api/teams/%d", m.id), `{"name":"x"}`},
		{"DELETE", p("/api/teams/%d", m.id), ""},
		{"POST", p("/api/teams/%d/transfer", m.id), p(`{"userId":%d}`, m.admin)},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			if got := e.status(m.admin, c.method, c.path, c.body); got != 404 {
				t.Errorf("got %d want 404", got)
			}
		})
	}
	var after string
	_ = e.st.Pool().QueryRow(context.Background(), `SELECT name FROM teams WHERE id=$1`, m.id).Scan(&after)
	if after != name || e.role(m.id, m.owner) != "owner" || e.role(m.id, m.admin) != "admin" {
		t.Errorf("团队名/成员表被改：name=%q owner=%q admin=%q", after, e.role(m.id, m.owner), e.role(m.id, m.admin))
	}
}

// AC-15
func TestTeamAdminCannotTouchPeers(t *testing.T) {
	e := newTeamEnv(t)
	m := newMatrixTeam(e)
	for _, c := range []struct {
		name, method string
		target       int64
		body         string
	}{
		{"PATCH owner", "PATCH", m.owner, `{"role":"member"}`},
		{"PATCH 另一 admin", "PATCH", m.admin2, `{"role":"member"}`},
		{"DELETE owner", "DELETE", m.owner, ""},
		{"DELETE 另一 admin", "DELETE", m.admin2, ""},
		{"升 member 为 admin", "PATCH", m.member, `{"role":"admin"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := e.status(m.admin, c.method, p("/api/teams/%d/members/%d", m.id, c.target), c.body); got != 404 {
				t.Errorf("got %d want 404", got)
			}
		})
	}
	if e.role(m.id, m.owner) != "owner" || e.role(m.id, m.admin2) != "admin" || e.role(m.id, m.member) != "member" {
		t.Errorf("库里角色被改")
	}
}

// AC-15b
func TestTeamMemberCanLeave(t *testing.T) {
	e := newTeamEnv(t)
	for _, role := range []string{"member", "admin"} {
		t.Run(role, func(t *testing.T) {
			s := newSharedSetup(e)
			if role == "admin" {
				_, _ = e.st.Pool().Exec(context.Background(), `UPDATE team_members SET role='admin' WHERE team_id=$1 AND user_id=$2`, s.team, s.mate)
			}
			if got := e.status(s.mate, "DELETE", p("/api/teams/%d/members/%d", s.team, s.mate), ""); got != 200 {
				t.Fatalf("退出 got %d want 200", got)
			}
			if got := e.status(s.mate, "GET", p("/api/teams/%d", s.team), ""); got != 404 {
				t.Errorf("退出后团队详情 got %d want 404", got)
			}
			if got := e.status(s.mate, "GET", p("/api/tasks/%d", s.task), ""); got != 404 {
				t.Errorf("退出后共享任务 got %d want 404", got)
			}
		})
	}
}

// AC-16
func TestTeamLastOwnerGuard(t *testing.T) {
	e := newTeamEnv(t)
	m := newMatrixTeam(e)
	self := p("/api/teams/%d/members/%d", m.id, m.owner)
	for _, c := range []struct{ method, body string }{
		{"PATCH", `{"role":"admin"}`}, {"PATCH", `{"role":"member"}`}, {"DELETE", ""},
	} {
		t.Run(c.method+" "+c.body, func(t *testing.T) {
			if got := e.status(m.owner, c.method, self, c.body); got != 409 {
				t.Errorf("got %d want 409", got)
			}
		})
	}
	if n := e.count(`SELECT count(*) FROM team_members WHERE team_id=$1 AND role='owner'`, m.id); n != 1 || e.role(m.id, m.owner) != "owner" {
		t.Errorf("owner 数 got %d want 1（且仍是原 owner）", n)
	}
}

// AC-17
func TestTeamTransferOwner(t *testing.T) {
	e := newTeamEnv(t)
	m := newMatrixTeam(e)
	if got := e.status(m.owner, "POST", p("/api/teams/%d/transfer", m.id), p(`{"userId":%d}`, m.member)); got != 200 {
		t.Fatalf("转让 got %d want 200", got)
	}
	if e.role(m.id, m.owner) != "admin" || e.role(m.id, m.member) != "owner" {
		t.Errorf("转让后 旧 owner=%q 新 owner=%q want admin/owner", e.role(m.id, m.owner), e.role(m.id, m.member))
	}
	if n := e.count(`SELECT count(*) FROM team_members WHERE team_id=$1 AND role='owner'`, m.id); n != 1 {
		t.Errorf("owner 数 got %d want 1", n)
	}
	t.Run("目标不是本团队成员 → 404", func(t *testing.T) {
		if got := e.status(m.member, "POST", p("/api/teams/%d/transfer", m.id), p(`{"userId":%d}`, e.user("x"))); got != 404 {
			t.Errorf("got %d want 404", got)
		}
	})
}

// AC-18
func TestRepoSharingOnlyByOwner(t *testing.T) {
	e := newTeamEnv(t)
	m := newMatrixTeam(e)
	memberRepo := e.repo(m.member)
	for _, c := range []struct {
		name string
		uid  int64
	}{{"团队 owner 共享别人的仓库", m.owner}, {"团队 admin 共享别人的仓库", m.admin}} {
		t.Run(c.name, func(t *testing.T) {
			if got := e.status(c.uid, "POST", p("/api/repos/%d/sharing", memberRepo), p(`{"teamId":%d}`, m.id)); got != 404 {
				t.Errorf("got %d want 404", got)
			}
			if e.repoTeam(memberRepo) != nil {
				t.Errorf("team_id 被改")
			}
		})
	}
	t.Run("member 角色的属主共享自己的仓库 → 200", func(t *testing.T) {
		if got := e.status(m.member, "POST", p("/api/repos/%d/sharing", memberRepo), p(`{"teamId":%d}`, m.id)); got != 200 {
			t.Fatalf("got %d want 200", got)
		}
		if tid := e.repoTeam(memberRepo); tid == nil || *tid != m.id {
			t.Errorf("team_id got %v want %d", tid, m.id)
		}
	})
	t.Run("团队 owner 取消别人的共享 → 404", func(t *testing.T) {
		if got := e.status(m.owner, "POST", p("/api/repos/%d/sharing", memberRepo), `{"teamId":null}`); got != 404 {
			t.Errorf("got %d want 404", got)
		}
	})
}

// AC-45
func TestRepoShareRequiresTeamMembership(t *testing.T) {
	e := newTeamEnv(t)
	jia, yi := e.user("sm-jia"), e.user("sm-yi")
	other := e.team(map[int64]string{yi: "owner"})
	repo := e.repo(jia)
	e.task(jia, repo)
	var maxID int64
	_ = e.st.Pool().QueryRow(context.Background(), `SELECT COALESCE(max(id),0) FROM teams`).Scan(&maxID)

	code1, b1, _ := e.raw(jia, "POST", p("/api/repos/%d/sharing", repo), p(`{"teamId":%d}`, other))
	code2, b2, _ := e.raw(jia, "POST", p("/api/repos/%d/sharing", repo), p(`{"teamId":%d}`, maxID+1000))
	if code1 != 404 || code2 != 404 {
		t.Errorf("非成员团队 got %d、不存在团队 got %d want 404/404", code1, code2)
	}
	if !bytes.Equal(b1, b2) {
		t.Errorf("两种 404 的响应体不同：%s vs %s", b1, b2)
	}
	if e.repoTeam(repo) != nil {
		t.Errorf("repos.team_id 被改")
	}
	_, body := e.call(yi, "GET", "/api/tasks?scope=team", "")
	if n := body["total"].(float64); n != 0 {
		t.Errorf("乙的团队视图被注入了 %v 条甲的任务", n)
	}
}

// AC-46 / AC-47
func TestTeamMemberRemovalUnsharesTheirRepos(t *testing.T) {
	testUnshareOnLeave(t, false)
}

func TestTeamMemberLeaveUnsharesTheirRepos(t *testing.T) {
	testUnshareOnLeave(t, true)
}

func testUnshareOnLeave(t *testing.T, selfLeave bool) {
	e := newTeamEnv(t)
	owner, yi, bing := e.user("ul-owner"), e.user("ul-yi"), e.user("ul-bing")
	team := e.team(map[int64]string{owner: "owner", yi: "member", bing: "member"})
	r := e.repo(yi)
	e.share(r, team)
	tk := e.task(yi, r)
	bingRepo := e.repo(bing)
	e.share(bingRepo, team)

	if got := e.status(bing, "GET", p("/api/tasks/%d", tk), ""); got != 200 {
		t.Fatalf("丙移除前读乙的任务 got %d want 200", got)
	}
	actor := owner
	if selfLeave {
		actor = yi
	}
	if got := e.status(actor, "DELETE", p("/api/teams/%d/members/%d", team, yi), ""); got != 200 {
		t.Fatalf("移除/退出 got %d want 200", got)
	}
	if tid := e.repoTeam(r); tid != nil {
		t.Errorf("乙的仓库 team_id got %d want NULL", *tid)
	}
	if got := e.status(bing, "GET", p("/api/tasks/%d", tk), ""); got != 404 {
		t.Errorf("丙读乙的任务 got %d want 404", got)
	}
	if got := e.status(yi, "GET", p("/api/tasks/%d", tk), ""); got != 200 {
		t.Errorf("乙读自己的任务 got %d want 200", got)
	}
	if tid := e.repoTeam(bingRepo); tid == nil || *tid != team {
		t.Errorf("丙共享的仓库 team_id got %v want %d（不受影响）", tid, team)
	}
}

// AC-48
func TestTeamShareRemoveRace(t *testing.T) {
	e := newTeamEnv(t)
	owner, yi := e.user("race-owner"), e.user("race-yi")
	team := e.team(map[int64]string{owner: "owner"})
	r := e.repo(yi)
	e.server(owner)
	e.server(yi)
	ctx := context.Background()
	before := store.TeamTxRetries()

	for round := 0; round < 50; round++ {
		if _, err := e.st.Pool().Exec(ctx, `INSERT INTO team_members (team_id,user_id,role) VALUES ($1,$2,'member') ON CONFLICT DO NOTHING`, team, yi); err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.Pool().Exec(ctx, `UPDATE repos SET team_id=NULL WHERE id=$1`, r); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		var codeA, codeB int
		var errA, errB error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			codeA, _, errA = e.raw(yi, "POST", p("/api/repos/%d/sharing", r), p(`{"teamId":%d}`, team))
		}()
		go func() {
			defer wg.Done()
			<-start
			codeB, _, errB = e.raw(owner, "DELETE", p("/api/teams/%d/members/%d", team, yi), "")
		}()
		close(start)
		wg.Wait()
		if errA != nil || errB != nil {
			t.Fatalf("round %d: 请求失败 %v / %v", round, errA, errB)
		}
		if codeA != 200 && codeA != 404 {
			t.Errorf("round %d: share status got %d want 200/404", round, codeA)
		}
		if codeB != 200 {
			t.Errorf("round %d: remove status got %d want 200", round, codeB)
		}
		if n := e.count(`SELECT count(*) FROM repos r WHERE r.team_id=$1 AND r.user_id=$2
			AND NOT EXISTS (SELECT 1 FROM team_members m WHERE m.team_id=$1 AND m.user_id=$2)`, team, yi); n != 0 {
			t.Errorf("round %d: 非成员的仓库仍共享在团队里（%d 行）", round, n)
		}
	}
	if d := store.TeamTxRetries() - before; d != 0 {
		t.Errorf("TeamTxRetries delta got %d want 0", d)
	}
}

// AC-50（httpapi 侧）
func TestTeamBusyMapsTo503(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantBody string
	}{
		{"ErrTeamBusy → 503", fmt.Errorf("wrap: %w", store.ErrTeamBusy), 503, `{"error":"团队数据正忙，请重试"}`},
		{"ErrTeamNotFound → 404", store.ErrTeamNotFound, 404, `{"error":"团队不存在"}`},
		{"ErrTeamNeedsOwner → 409", store.ErrTeamNeedsOwner, 409, ""},
		{"ErrInvalidTeamRole → 400", store.ErrInvalidTeamRole, 400, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			teamError(rec, c.err)
			if rec.Code != c.wantCode {
				t.Errorf("code got %d want %d", rec.Code, c.wantCode)
			}
			if c.wantBody != "" && strings.TrimSpace(rec.Body.String()) != c.wantBody {
				t.Errorf("body got %s want %s", rec.Body.String(), c.wantBody)
			}
		})
	}
}

// AC-19
func TestTeamAllDenyPathsAre404(t *testing.T) {
	e := newTeamEnv(t)
	s := newSharedSetup(e)
	var first []byte
	for _, c := range []struct{ path, body string }{
		{"retry", `{"mode":"auto"}`}, {"cancel", `{}`}, {"approve", `{}`}, {"review-retry", `{}`},
		{"preview/start", `{}`}, {"preview/stop", `{}`}, {"preview/recommend", `{}`},
	} {
		t.Run(c.path, func(t *testing.T) {
			code, b, err := e.raw(s.mate, "POST", p("/api/tasks/%d/%s", s.task, c.path), c.body)
			if err != nil {
				t.Fatal(err)
			}
			if code != 404 {
				t.Errorf("got %d want 404", code)
			}
			if first == nil {
				first = b
			} else if !bytes.Equal(first, b) {
				t.Errorf("响应体 got %s want %s（与其他写接口一致）", b, first)
			}
		})
	}
}

// denyCases：teamRoutes 里每个 Write 路由的一条拒绝用例（AC-20）。
// 每个用例自己造数据，返回拒绝时的状态码（期望 404）。
var denyCases = map[string]func(e *teamEnv) int{
	"PATCH /api/teams/{id}": func(e *teamEnv) int {
		m := newMatrixTeam(e)
		return e.status(m.admin, "PATCH", p("/api/teams/%d", m.id), `{"name":"x"}`)
	},
	"DELETE /api/teams/{id}": func(e *teamEnv) int {
		m := newMatrixTeam(e)
		return e.status(m.admin, "DELETE", p("/api/teams/%d", m.id), "")
	},
	"POST /api/teams/{id}/invitations": func(e *teamEnv) int {
		m := newMatrixTeam(e)
		return e.status(m.member, "POST", p("/api/teams/%d/invitations", m.id), `{"role":"member"}`)
	},
	"DELETE /api/teams/{id}/invitations/{iid}": func(e *teamEnv) int {
		m := newMatrixTeam(e)
		_, iid := e.invite(m.id, "member")
		return e.status(m.member, "DELETE", p("/api/teams/%d/invitations/%d", m.id, iid), "")
	},
	"POST /api/team-invitations/accept": func(e *teamEnv) int {
		return e.status(e.user("dc"), "POST", "/api/team-invitations/accept", `{"code":"definitely-wrong"}`)
	},
	"PATCH /api/teams/{id}/members/{uid}": func(e *teamEnv) int {
		m := newMatrixTeam(e)
		return e.status(m.admin, "PATCH", p("/api/teams/%d/members/%d", m.id, m.member), `{"role":"admin"}`)
	},
	"DELETE /api/teams/{id}/members/{uid}": func(e *teamEnv) int {
		m := newMatrixTeam(e)
		return e.status(m.member, "DELETE", p("/api/teams/%d/members/%d", m.id, m.member2), "")
	},
	"POST /api/teams/{id}/transfer": func(e *teamEnv) int {
		m := newMatrixTeam(e)
		return e.status(m.admin, "POST", p("/api/teams/%d/transfer", m.id), p(`{"userId":%d}`, m.admin))
	},
	"POST /api/repos/{id}/sharing": func(e *teamEnv) int {
		m := newMatrixTeam(e)
		r := e.repo(m.member)
		return e.status(m.owner, "POST", p("/api/repos/%d/sharing", r), p(`{"teamId":%d}`, m.id))
	},
}

// AC-20
func TestTeamEveryWriteRouteHasDenyCase(t *testing.T) {
	e := newTeamEnv(t)
	for _, rt := range teamRoutes {
		if !rt.Write {
			continue
		}
		key := rt.Method + " " + rt.Pattern
		t.Run(key, func(t *testing.T) {
			dc, ok := denyCases[key]
			if !ok {
				t.Fatalf("route %s has no deny case", key)
			}
			if got := dc(e); got != 404 {
				t.Errorf("deny case got %d want 404", got)
			}
		})
	}
}

// ---------------------------------------------------------------- 5.4 邀请

// AC-21
func TestTeamEmailAloneCannotJoin(t *testing.T) {
	e := newTeamEnv(t)
	jia := e.user("ea-jia")
	team := e.team(map[int64]string{jia: "owner"})
	r := e.repo(jia)
	e.share(r, team)
	tk := e.task(jia, r)
	victim := fmt.Sprintf("victim-%s@example.com", teamRand())

	code, inv := e.call(jia, "POST", p("/api/teams/%d/invitations", team), p(`{"role":"member","inviteeEmail":%q}`, victim))
	if code != 201 {
		t.Fatalf("发邀请 got %d want 201", code)
	}
	// 乙抢注 victim 的邮箱
	yi := mustUser(t, e.st, victim)
	code, me := e.call(yi, "GET", "/api/me", "")
	if code != 200 {
		t.Fatalf("/api/me got %d", code)
	}
	if b, _ := json.Marshal(me); strings.Contains(strings.ToLower(string(b)), "invit") {
		t.Errorf("/api/me 含邀请相关字段: %s", b)
	}
	if got := e.status(yi, "GET", "/api/invitations", ""); got != 404 {
		t.Errorf("GET /api/invitations got %d want 404（路由不存在）", got)
	}
	for _, body := range []string{`{"code":""}`, `{"code":"wrong-code"}`, `{}`} {
		if got := e.status(yi, "POST", "/api/team-invitations/accept", body); got != 404 {
			t.Errorf("accept %s got %d want 404", body, got)
		}
	}
	if got := e.status(yi, "GET", p("/api/teams/%d", team), ""); got != 404 {
		t.Errorf("乙读团队 got %d want 404", got)
	}
	if got := e.status(yi, "GET", p("/api/tasks/%d", tk), ""); got != 404 {
		t.Errorf("乙读团队任务 got %d want 404", got)
	}
	// 正向对照：持有正确码的丙可以加入
	bing := e.user("ea-bing")
	c, _ := inv["code"].(string)
	if got := e.status(bing, "POST", "/api/team-invitations/accept", p(`{"code":%q}`, c)); got != 200 {
		t.Errorf("持码的丙 accept got %d want 200", got)
	}
}

// AC-22
func TestTeamInviteCodeSingleUse(t *testing.T) {
	e := newTeamEnv(t)
	owner, yi, bing := e.user("su-owner"), e.user("su-yi"), e.user("su-bing")
	team := e.team(map[int64]string{owner: "owner"})
	_, inv := e.call(owner, "POST", p("/api/teams/%d/invitations", team), `{"role":"member"}`)
	code, _ := inv["code"].(string)
	if code == "" {
		t.Fatalf("创建响应应带一次性明文码: %v", inv)
	}
	if got := e.status(yi, "POST", "/api/team-invitations/accept", p(`{"code":%q}`, code)); got != 200 {
		t.Fatalf("首次 accept got %d want 200", got)
	}
	if e.role(team, yi) != "member" {
		t.Errorf("乙角色 got %q want member", e.role(team, yi))
	}
	for _, uid := range []int64{bing, yi} {
		if got := e.status(uid, "POST", "/api/team-invitations/accept", p(`{"code":%q}`, code)); got != 404 {
			t.Errorf("第二次 accept（uid=%d）got %d want 404", uid, got)
		}
	}
	var state string
	var by *int64
	_ = e.st.Pool().QueryRow(context.Background(),
		`SELECT state, accepted_by FROM team_invitations WHERE team_id=$1`, team).Scan(&state, &by)
	if state != "accepted" || by == nil || *by != yi {
		t.Errorf("邀请 state=%s accepted_by=%v want accepted/%d", state, by, yi)
	}
	// 库里不存明文
	if n := e.count(`SELECT count(*) FROM team_invitations WHERE team_id=$1 AND (code_hash = convert_to($2,'UTF8') OR code_hint = $2)`, team, code); n != 0 {
		t.Errorf("库里出现了明文码")
	}
}

// AC-23
func TestTeamInviteDoesNotLeakAccountExistence(t *testing.T) {
	e := newTeamEnv(t)
	owner := e.user("le-owner")
	team := e.team(map[int64]string{owner: "owner"})
	registered := fmt.Sprintf("le-reg-%s@example.com", teamRand())
	mustUser(t, e.st, registered)
	usersBefore := e.count(`SELECT count(*) FROM users`)

	shape := func(body string) (int, string) {
		code, v := e.call(owner, "POST", p("/api/teams/%d/invitations", team), body)
		var keys []string
		for k := range v {
			keys = append(keys, k)
		}
		return code, strings.Join(sortStrings(keys), ",")
	}
	c1, k1 := shape(p(`{"role":"member","inviteeEmail":%q}`, registered))
	c2, k2 := shape(p(`{"role":"member","inviteeEmail":%q}`, "nobody-"+teamRand()+"@example.com"))
	c3, k3 := shape(`{"role":"member"}`)
	if c1 != 201 || c1 != c2 || c2 != c3 {
		t.Errorf("状态码 got %d/%d/%d want 都是 201", c1, c2, c3)
	}
	// 邮箱字段本身随输入有无而出现，不算身份信息；比较时剔掉
	strip := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, "inviteeEmail,", ""), ",inviteeEmail", "")
	}
	if strip(k1) != strip(k2) || strip(k2) != strip(k3) {
		t.Errorf("响应字段形态不一致：%s / %s / %s", k1, k2, k3)
	}
	if n := e.count(`SELECT count(*) FROM users`); n != usersBefore {
		t.Errorf("users 行数 got %d want %d", n, usersBefore)
	}
}

func sortStrings(s []string) []string {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
	return s
}

// AC-24
func TestTeamInviteExpiredOrRevokedRejected(t *testing.T) {
	e := newTeamEnv(t)
	owner := e.user("er-owner")
	team := e.team(map[int64]string{owner: "owner"})
	cases := []struct {
		name   string
		break_ func(code string, id int64)
	}{
		{"过期", func(_ string, id int64) {
			_, _ = e.st.Pool().Exec(context.Background(), `UPDATE team_invitations SET expires_at = now() - interval '1 minute' WHERE id=$1`, id)
		}},
		{"撤销", func(_ string, id int64) {
			if got := e.status(owner, "DELETE", p("/api/teams/%d/invitations/%d", team, id), ""); got != 200 {
				t.Fatalf("撤销 got %d want 200", got)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, id := e.invite(team, "member")
			c.break_(code, id)
			if got := e.status(e.user("er"), "POST", "/api/team-invitations/accept", p(`{"code":%q}`, code)); got != 404 {
				t.Errorf("got %d want 404", got)
			}
		})
	}
}

// AC-25
func TestTeamInvitationAcceptGrantsVisibility(t *testing.T) {
	e := newTeamEnv(t)
	s := newSharedSetup(e)
	newbie := e.user("ag-new")
	path := p("/api/tasks/%d", s.task)
	if got := e.status(newbie, "GET", path, ""); got != 404 {
		t.Fatalf("接受前 got %d want 404", got)
	}
	code, _ := e.invite(s.team, "member")
	if got := e.status(newbie, "POST", "/api/team-invitations/accept", p(`{"code":%q}`, code)); got != 200 {
		t.Fatalf("accept got %d want 200", got)
	}
	if got := e.status(newbie, "GET", path, ""); got != 200 {
		t.Errorf("接受后 got %d want 200", got)
	}
	code2, id2 := e.invite(s.team, "member")
	if got := e.status(newbie, "POST", "/api/team-invitations/accept", p(`{"code":%q}`, code2)); got != 404 {
		t.Errorf("已是成员再 accept got %d want 404", got)
	}
	var state string
	_ = e.st.Pool().QueryRow(context.Background(), `SELECT state FROM team_invitations WHERE id=$1`, id2).Scan(&state)
	if state != "pending" {
		t.Errorf("未被消耗的码 state got %s want pending", state)
	}
}

// AC-26
func TestTeamAcceptRateLimited(t *testing.T) {
	e := newTeamEnv(t)
	e.proxy = true
	now := time.Now()
	var mu sync.Mutex
	e.clock = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	owner := e.user("rl-owner")
	team := e.team(map[int64]string{owner: "owner"})
	code1, id1 := e.invite(team, "member")
	code2, _ := e.invite(team, "member")
	guesser, other, later := e.user("rl-g"), e.user("rl-o"), e.user("rl-l")
	ipA := []string{"X-Forwarded-For", "203.0.113.7"}
	ipB := []string{"X-Forwarded-For", "198.51.100.9"}

	for i := 1; i <= 10; i++ {
		if got, _ := e.call(guesser, "POST", "/api/team-invitations/accept", `{"code":"bad"}`, ipA...); got != 404 {
			t.Fatalf("第 %d 次错码 got %d want 404", i, got)
		}
	}
	if got, _ := e.call(guesser, "POST", "/api/team-invitations/accept", `{"code":"bad"}`, ipA...); got != 429 {
		t.Errorf("第 11 次 got %d want 429", got)
	}
	if got, _ := e.call(guesser, "POST", "/api/team-invitations/accept", p(`{"code":%q}`, code1), ipA...); got != 429 {
		t.Errorf("第 12 次正确码 got %d want 429", got)
	}
	var state string
	_ = e.st.Pool().QueryRow(context.Background(), `SELECT state FROM team_invitations WHERE id=$1`, id1).Scan(&state)
	if state != "pending" {
		t.Errorf("被限流的码 state got %s want pending", state)
	}
	if got, _ := e.call(other, "POST", "/api/team-invitations/accept", p(`{"code":%q}`, code1), ipB...); got != 200 {
		t.Errorf("另一 IP 正确码 got %d want 200", got)
	}
	mu.Lock()
	now = now.Add(15*time.Minute + time.Second)
	mu.Unlock()
	if got, _ := e.call(later, "POST", "/api/team-invitations/accept", p(`{"code":%q}`, code2), ipA...); got != 200 {
		t.Errorf("窗口过后同 IP 正确码 got %d want 200", got)
	}
}

// ---------------------------------------------------------------- 5.5 并发

// AC-29
func TestTeamLastOwnerGuardUnderConcurrency(t *testing.T) {
	e := newTeamEnv(t)
	for round := 0; round < 10; round++ {
		m := newMatrixTeam(e)
		e.server(m.owner)
		var wg sync.WaitGroup
		start := make(chan struct{})
		codes := make([]int, 2)
		for i, target := range []int64{m.admin, m.member} {
			wg.Add(1)
			go func(i int, target int64) {
				defer wg.Done()
				<-start
				codes[i], _, _ = e.raw(m.owner, "POST", p("/api/teams/%d/transfer", m.id), p(`{"userId":%d}`, target))
			}(i, target)
		}
		close(start)
		wg.Wait()
		ok := 0
		for _, c := range codes {
			switch c {
			case 200:
				ok++
			case 404:
			default:
				t.Errorf("round %d: 状态码 got %d want 200/404", round, c)
			}
		}
		if ok != 1 {
			t.Errorf("round %d: 成功的转让 got %d want 1（%v）", round, ok, codes)
		}
		if n := e.count(`SELECT count(*) FROM team_members WHERE team_id=$1 AND role='owner'`, m.id); n != 1 {
			t.Errorf("round %d: owner 数 got %d want 1", round, n)
		}
		if r := e.role(m.id, m.owner); r != "admin" {
			t.Errorf("round %d: 原 owner 角色 got %q want admin", round, r)
		}
	}
}

// ---------------------------------------------------------------- 5.6 回归守卫

// AC-31
func TestTeamNonMemberStillGets404OnPrivateRepo(t *testing.T) {
	e := newTeamEnv(t)
	owner, mate := e.user("pv-owner"), e.user("pv-mate")
	e.team(map[int64]string{owner: "owner", mate: "member"})
	r := e.repo(owner) // 私有
	tk := e.task(owner, r)
	_, body := e.call(mate, "GET", "/api/tasks?scope=team", "")
	if n := body["total"].(float64); n != 0 {
		t.Errorf("列表 total got %v want 0", n)
	}
	for _, path := range []string{p("/api/tasks/%d", tk), p("/api/tasks/%d/events", tk)} {
		if got := e.status(mate, "GET", path, ""); got != 404 {
			t.Errorf("%s got %d want 404", path, got)
		}
	}
}

// AC-32
func TestTeamMemberCannotStartPreviewOnOthersTask(t *testing.T) {
	e := newTeamEnv(t)
	s := newSharedSetup(e)
	for _, a := range []string{"start", "stop", "recommend"} {
		if got := e.status(s.mate, "POST", p("/api/tasks/%d/preview/%s", s.task, a), `{}`); got != 404 {
			t.Errorf("preview/%s got %d want 404", a, got)
		}
	}
}

// AC-34
func TestRepoUpdateStillOwnerOnly(t *testing.T) {
	e := newTeamEnv(t)
	s := newSharedSetup(e)
	before := e.count(`SELECT count(*) FROM repos WHERE id=$1 AND gate_mode='direct'`, s.repo)
	for _, uid := range []int64{s.mate, s.stranger} {
		if got := e.status(uid, "PUT", p("/api/repos/%d", s.repo), `{"gateMode":"manual"}`); got != 404 {
			t.Errorf("uid=%d got %d want 404", uid, got)
		}
	}
	if after := e.count(`SELECT count(*) FROM repos WHERE id=$1 AND gate_mode='direct'`, s.repo); after != before {
		t.Errorf("gate_mode 被改")
	}
}

// AC-51③（HTTP 侧）：不带 scope 的 GET /api/repos 响应没有 owned/teamId 键，且只含自己的仓库
func TestListReposDefaultResponseUnchanged(t *testing.T) {
	e := newTeamEnv(t)
	s := newSharedSetup(e)
	mine := e.repo(s.mate)
	code, b, _ := e.raw(s.mate, "GET", "/api/repos", "")
	if code != 200 {
		t.Fatalf("got %d", code)
	}
	if bytes.Contains(b, []byte(`"owned"`)) || bytes.Contains(b, []byte(`"teamId"`)) {
		t.Errorf("默认响应不应带 owned/teamId: %s", b)
	}
	var v struct {
		Repos []struct {
			ID int64 `json:"id"`
		} `json:"repos"`
	}
	_ = json.Unmarshal(b, &v)
	if len(v.Repos) != 1 || v.Repos[0].ID != mine {
		t.Errorf("默认仓库列表 got %+v want 只有 %d", v.Repos, mine)
	}
	_, team := e.call(s.mate, "GET", "/api/repos?scope=team", "")
	if n := len(team["repos"].([]any)); n != 2 {
		t.Errorf("scope=team 仓库数 got %d want 2", n)
	}
}

// AC-42
func TestTeamAPINeverLeaksSecrets(t *testing.T) {
	e := newTeamEnv(t)
	s := newSharedSetup(e)
	secret := "ghp_f12SECRET" + teamRand()
	if err := e.st.NewSecrets(testSealer(t)).Save(context.Background(), s.owner, store.KindGitHub, secret); err != nil {
		t.Fatalf("存凭据失败: %v", err)
	}
	for _, path := range []string{
		p("/api/teams/%d", s.team), "/api/teams", "/api/repos?scope=team",
		p("/api/tasks/%d", s.task), "/api/tasks?scope=team",
	} {
		_, b, err := e.raw(s.mate, "GET", path, "")
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(secret)) {
			t.Errorf("%s 泄露了凭据明文", path)
		}
	}
}

// AC-44
func TestTeamNameDuplicateAllowed(t *testing.T) {
	e := newTeamEnv(t)
	jia, yi := e.user("dn-jia"), e.user("dn-yi")
	name := "同名团队-" + teamRand()
	code1, t1 := e.call(jia, "POST", "/api/teams", p(`{"name":%q}`, name))
	code2, t2 := e.call(yi, "POST", "/api/teams", p(`{"name":%q}`, name))
	if code1 != 201 || code2 != 201 {
		t.Fatalf("建同名团队 got %d/%d want 201/201", code1, code2)
	}
	id1, id2 := int64(t1["id"].(float64)), int64(t2["id"].(float64))
	t.Cleanup(func() {
		_, _ = e.st.Pool().Exec(context.Background(), `DELETE FROM teams WHERE id IN ($1,$2)`, id1, id2)
	})
	if got := e.status(jia, "GET", p("/api/teams/%d", id1), ""); got != 200 {
		t.Errorf("甲看自己的团队 got %d want 200", got)
	}
	if got := e.status(jia, "GET", p("/api/teams/%d", id2), ""); got != 404 {
		t.Errorf("甲看乙的同名团队 got %d want 404", got)
	}
	if r := e.role(id1, jia); r != "owner" {
		t.Errorf("创建者角色 got %q want owner", r)
	}
}
