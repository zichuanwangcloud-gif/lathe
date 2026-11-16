package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/secret"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/teamskills"
)

// ---------------------------------------------------------------- 夹具

type tsUser struct {
	id     int64
	repoID int64
	srv    *httptest.Server
}

type tsAPIEnv struct {
	st      *store.Store
	secrets *store.Secrets
}

func newTSAPIEnv(t *testing.T) *tsAPIEnv {
	t.Helper()
	st := testStoreForAPI(t)
	key := make([]byte, secret.KeySize)
	for i := range key {
		key[i] = byte(i + 7)
	}
	sealer, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return &tsAPIEnv{st: st, secrets: st.NewSecrets(sealer)}
}

// user 建一个带随机量的用户 + 仓库，并起一台以该用户身份登录的 API 服务。
func (e *tsAPIEnv) user(t *testing.T, tag string) *tsUser {
	t.Helper()
	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	email := "tsapi-" + tag + "-" + t.Name() + "-" + nonce + "@example.com"
	id := mustUser(t, e.st, email)
	u := &tsUser{id: id, repoID: e.repo(t, id, "acme/tsapi-"+tag+"-"+nonce)}
	api := &TeamSkillAPI{Store: e.st, Secrets: e.secrets, Auth: authAs(id, email)}
	mux := http.NewServeMux()
	api.Routes(mux)
	u.srv = httptest.NewServer(mux)
	t.Cleanup(u.srv.Close)
	return u
}

func (e *tsAPIEnv) repo(t *testing.T, userID int64, provider string) int64 {
	t.Helper()
	var id int64
	if err := e.st.Pool().QueryRow(context.Background(),
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`, userID, provider).Scan(&id); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	return id
}

// skillMD 是测试里 SKILL.md 的统一模板（请求体与期望 sha 共用，不会漂移）。
func skillMD(slug, body string) string {
	return "---\nname: " + slug + "\ndescription: 团队规范\n---\n\n" + body + "\n"
}

func skillJSON(slug, body string) string {
	md := skillMD(slug, body)
	b, _ := json.Marshal(map[string]any{
		"slug": slug, "title": "", "files": []map[string]string{{"path": "SKILL.md", "content": md}},
	})
	return string(b)
}

func versionJSON(slug, body string) string {
	md := skillMD(slug, body)
	b, _ := json.Marshal(map[string]any{"files": []map[string]string{{"path": "SKILL.md", "content": md}}})
	return string(b)
}

func (u *tsUser) base() string { return fmt.Sprintf("/api/repos/%d/team-skills", u.repoID) }

func (u *tsUser) create(t *testing.T, slug, body string) (int, map[string]any) {
	t.Helper()
	resp := do(t, u.srv, "POST", u.base(), skillJSON(slug, body), true)
	return resp.StatusCode, decode(t, resp)
}

func skillID(t *testing.T, body map[string]any) int64 {
	t.Helper()
	sk, ok := body["skill"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 skill: %v", body)
	}
	return int64(sk["id"].(float64))
}

func (e *tsAPIEnv) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.st.Pool().QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ---------------------------------------------------------------- AC-6

func TestCreateTeamSkill_RepoLimit(t *testing.T) {
	e := newTSAPIEnv(t)
	u := e.user(t, "limit")
	for i := 1; i <= 20; i++ {
		if code, body := u.create(t, fmt.Sprintf("s%02d", i), "x"); code != http.StatusCreated {
			t.Fatalf("第 %d 个 got %d (%v), want 201", i, code, body)
		}
	}
	code, body := u.create(t, "s21", "x")
	if code != http.StatusBadRequest {
		t.Fatalf("第 21 个 got %d, want 400", code)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "20") {
		t.Errorf("文案应含 20，got %q", msg)
	}
}

// ---------------------------------------------------------------- AC-7

// 非属主一律 404（不用 403）。
func TestTeamSkillIsolationBetweenUsers(t *testing.T) {
	e := newTSAPIEnv(t)
	a := e.user(t, "a")
	b := e.user(t, "b")
	code, body := a.create(t, "acme-testing", "A 的")
	if code != http.StatusCreated {
		t.Fatalf("A 新建 got %d (%v)", code, body)
	}
	sid := skillID(t, body)
	skill := fmt.Sprintf("%s/%d", a.base(), sid)
	cases := []struct {
		name, method, path, body string
	}{
		{"GET列表", "GET", a.base(), ""},
		{"GET详情", "GET", skill, ""},
		{"PATCH", "PATCH", skill, `{"enabled":false}`},
		{"DELETE", "DELETE", skill, ""},
		{"GET版本", "GET", skill + "/versions/1", ""},
		{"POST新版本", "POST", skill + "/versions", versionJSON("acme-testing", "B 改")},
		{"POST恢复", "POST", skill + "/versions/1/restore", ""},
		{"用A的repo_id新建", "POST", a.base(), skillJSON("b-skill", "B 的")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := do(t, b.srv, c.method, c.path, c.body, true)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("B %s %s got %d, want 404", c.method, c.path, resp.StatusCode)
			}
		})
	}
	if n := e.count(t, `SELECT count(*) FROM team_skill_versions WHERE skill_id=$1`, sid); n != 1 {
		t.Errorf("B 的写操作不应生效：A 的技能版本数 got %d, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM team_skills WHERE id=$1 AND enabled AND archived_at IS NULL`, sid); n != 1 {
		t.Error("B 的 PATCH/DELETE 不应生效")
	}
}

// B 读 A 的任务钉住的技能 → 404；A 自己能读。
func TestTaskTeamSkillsIsolation(t *testing.T) {
	e := newTSAPIEnv(t)
	a := e.user(t, "ta")
	b := e.user(t, "tb")
	var taskID int64
	if err := e.st.Pool().QueryRow(context.Background(),
		`INSERT INTO tasks (user_id, repo_id, external_key) VALUES ($1,$2,'TS-ISO') RETURNING id`, a.id, a.repoID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if err := e.st.TeamSkillPinsSave(context.Background(), taskID, a.id, a.repoID,
		[]store.TeamSkillPin{{SkillID: 1, Slug: "team-x", Version: 2, SHA256: "h"}}); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/tasks/%d/team-skills", taskID)
	if resp := do(t, b.srv, "GET", path, "", true); resp.StatusCode != http.StatusNotFound {
		t.Errorf("B 读 A 的任务 got %d, want 404", resp.StatusCode)
	}
	resp := do(t, a.srv, "GET", path, "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("A 读自己的任务 got %d", resp.StatusCode)
	}
	body := decode(t, resp)
	if body["pinned"] != true {
		t.Errorf("pinned got %v, want true", body["pinned"])
	}
	if skills, _ := body["skills"].([]any); len(skills) != 1 {
		t.Errorf("skills got %v, want 1 项", body["skills"])
	}
}

// 同仓库同名在役技能 → 409（中文原因，不透出约束名/SQLSTATE）；并发 {201,409}。
func TestCreateTeamSkill_DuplicateActiveSlugReturns409(t *testing.T) {
	e := newTSAPIEnv(t)
	t.Run("顺序", func(t *testing.T) {
		u := e.user(t, "dup")
		if code, body := u.create(t, "acme-testing", "x"); code != http.StatusCreated {
			t.Fatalf("首次 got %d (%v)", code, body)
		}
		for _, slug := range []string{"acme-testing", "Acme Testing"} {
			resp := do(t, u.srv, "POST", u.base(), skillJSON(slug, "y"), true)
			body := decode(t, resp)
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("slug %q got %d (%v), want 409", slug, resp.StatusCode, body)
			}
			msg, _ := body["error"].(string)
			for _, leak := range []string{"23505", "team_skills_active_slug", "duplicate key"} {
				if strings.Contains(msg, leak) {
					t.Errorf("409 原因透出了 %q：%q", leak, msg)
				}
			}
		}
		if n := e.count(t, `SELECT count(*) FROM team_skills WHERE repo_id=$1 AND slug='acme-testing' AND archived_at IS NULL`, u.repoID); n != 1 {
			t.Errorf("在役 acme-testing got %d 行, want 1", n)
		}
		if n := e.count(t, `SELECT count(*) FROM team_skill_versions v JOIN team_skills s ON s.id=v.skill_id WHERE s.repo_id=$1`, u.repoID); n != 1 {
			t.Errorf("版本行 got %d, want 1", n)
		}
	})
	t.Run("并发", func(t *testing.T) {
		for round := 0; round < 10; round++ {
			u := e.user(t, "dupc"+strconv.Itoa(round))
			codes := concurrently(2, func(int) int {
				resp := do(t, u.srv, "POST", u.base(), skillJSON("acme-testing", "x"), true)
				return resp.StatusCode
			})
			if got := multiset(codes); got != "201,409" {
				t.Fatalf("第 %d 轮状态码 got {%s}, want {201,409}", round, got)
			}
		}
	})
	t.Run("另一个仓库同名可建", func(t *testing.T) {
		u := e.user(t, "dupo")
		if code, _ := u.create(t, "acme-testing", "x"); code != http.StatusCreated {
			t.Fatalf("首次 got %d", code)
		}
		other := e.repo(t, u.id, "acme/tsapi-other-"+strconv.FormatInt(time.Now().UnixNano(), 36))
		resp := do(t, u.srv, "POST", fmt.Sprintf("/api/repos/%d/team-skills", other), skillJSON("acme-testing", "x"), true)
		if resp.StatusCode != http.StatusCreated {
			t.Errorf("另一仓库同名 got %d, want 201", resp.StatusCode)
		}
	})
}

// concurrently 用同一个栅栏同时放行 n 个调用，返回各自结果。
func concurrently(n int, fn func(i int) int) []int {
	var start, done sync.WaitGroup
	start.Add(1)
	out := make([]int, n)
	for i := 0; i < n; i++ {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait()
			out[i] = fn(i)
		}(i)
	}
	start.Done()
	done.Wait()
	return out
}

func multiset(xs []int) string {
	s := append([]int(nil), xs...)
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	parts := make([]string, len(s))
	for i, x := range s {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ",")
}

// ---------------------------------------------------------------- AC-8

func (u *tsUser) appendV(t *testing.T, sid int64, body string) (int, int) {
	t.Helper()
	resp := do(t, u.srv, "POST", fmt.Sprintf("%s/%d/versions", u.base(), sid), versionJSON("acme-testing", body), true)
	b := decode(t, resp)
	v, _ := b["version"].(float64)
	return resp.StatusCode, int(v)
}

func (u *tsUser) getV(t *testing.T, sid int64, v int) map[string]any {
	t.Helper()
	resp := do(t, u.srv, "GET", fmt.Sprintf("%s/%d/versions/%d", u.base(), sid, v), "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET v%d got %d", v, resp.StatusCode)
	}
	return decode(t, resp)
}

func newSkill(t *testing.T, u *tsUser, body string) int64 {
	t.Helper()
	code, b := u.create(t, "acme-testing", body)
	if code != http.StatusCreated {
		t.Fatalf("新建 got %d (%v)", code, b)
	}
	return skillID(t, b)
}

func TestVersionAppendIncrements(t *testing.T) {
	e := newTSAPIEnv(t)
	u := e.user(t, "inc")
	sid := newSkill(t, u, "C0")
	v1 := u.getV(t, sid, 1)
	if code, v := u.appendV(t, sid, "C1"); code != http.StatusCreated || v != 2 {
		t.Fatalf("第一次 append got (%d, v%d), want (201, v2)", code, v)
	}
	if code, v := u.appendV(t, sid, "C2"); code != http.StatusCreated || v != 3 {
		t.Fatalf("第二次 append got (%d, v%d), want (201, v3)", code, v)
	}
	again := u.getV(t, sid, 1)
	if fmt.Sprint(again["files"]) != fmt.Sprint(v1["files"]) {
		t.Errorf("v1 内容被改动：got %v, want %v", again["files"], v1["files"])
	}
}

func TestVersionSameContentNoBump(t *testing.T) {
	e := newTSAPIEnv(t)
	u := e.user(t, "same")
	sid := newSkill(t, u, "C0")
	if code, v := u.appendV(t, sid, "C0"); code != http.StatusOK || v != 1 {
		t.Fatalf("同内容 got (%d, v%d), want (200, v1)", code, v)
	}
	if n := e.count(t, `SELECT count(*) FROM team_skill_versions WHERE skill_id=$1`, sid); n != 1 {
		t.Errorf("版本行 got %d, want 1", n)
	}
}

func TestVersionRestoreCreatesNewVersion(t *testing.T) {
	e := newTSAPIEnv(t)
	u := e.user(t, "rest")
	sid := newSkill(t, u, "C0")
	if code, v := u.appendV(t, sid, "C1"); code != http.StatusCreated || v != 2 {
		t.Fatalf("append got (%d, v%d)", code, v)
	}
	before1, before2 := u.getV(t, sid, 1), u.getV(t, sid, 2)
	resp := do(t, u.srv, "POST", fmt.Sprintf("%s/%d/versions/1/restore", u.base(), sid), "", true)
	body := decode(t, resp)
	if resp.StatusCode != http.StatusCreated || body["version"] != float64(3) {
		t.Fatalf("restore got (%d, %v), want (201, 3)", resp.StatusCode, body["version"])
	}
	v3 := u.getV(t, sid, 3)
	if v3["contentSha256"] != before1["contentSha256"] {
		t.Errorf("v3 sha got %v, want 与 v1 相同 %v", v3["contentSha256"], before1["contentSha256"])
	}
	if fmt.Sprint(u.getV(t, sid, 1)) != fmt.Sprint(before1) || fmt.Sprint(u.getV(t, sid, 2)) != fmt.Sprint(before2) {
		t.Error("v1/v2 行被改动")
	}
}

func TestVersionConcurrentAppend(t *testing.T) {
	e := newTSAPIEnv(t)
	cases := []struct {
		name      string
		bodies    [2]string
		wantCodes string
		wantVers  string // 返回版本号多重集合
		wantRows  string
	}{
		{"并发写入不同内容", [2]string{"C1", "C2"}, "201,201", "2,3", "{1,2,3}"},
		{"并发写入同内容", [2]string{"C1", "C1"}, "200,201", "2,2", "{1,2}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for round := 0; round < 20; round++ {
				u := e.user(t, "conc"+strconv.Itoa(round))
				sid := newSkill(t, u, "C0")
				vers := make([]int, 2)
				codes := concurrently(2, func(i int) int {
					code, v := u.appendV(t, sid, c.bodies[i])
					vers[i] = v
					return code
				})
				if got := multiset(codes); got != c.wantCodes {
					t.Fatalf("第 %d 轮状态码 got {%s}, want {%s}", round, got, c.wantCodes)
				}
				if got := multiset(vers); got != c.wantVers {
					t.Fatalf("第 %d 轮版本号 got {%s}, want {%s}", round, got, c.wantVers)
				}
				var rows string
				if err := e.st.Pool().QueryRow(context.Background(),
					`SELECT array_agg(version ORDER BY version)::text FROM team_skill_versions WHERE skill_id=$1`, sid).Scan(&rows); err != nil {
					t.Fatal(err)
				}
				if rows != c.wantRows {
					t.Fatalf("第 %d 轮库里版本 got %s, want %s", round, rows, c.wantRows)
				}
				// v1 之后各版本的 content_sha256 集合 = 两次请求内容的 sha 去重集合：
				// 不同内容格 = {sha(C1), sha(C2)}；同内容格 = {sha(C1)}（即 v2 = sha(C1)）。
				var gotSHAs []string
				if err := e.st.Pool().QueryRow(context.Background(),
					`SELECT array_agg(content_sha256 ORDER BY content_sha256) FROM team_skill_versions
					 WHERE skill_id=$1 AND version>1`, sid).Scan(&gotSHAs); err != nil {
					t.Fatal(err)
				}
				if got, want := strings.Join(gotSHAs, ","), wantSHASet(c.bodies[:]); got != want {
					t.Fatalf("第 %d 轮 v2+ 的 content_sha256 集合 got {%s}, want {%s}", round, got, want)
				}
			}
		})
	}
}

// wantSHASet 是 versionJSON 发出去的那些内容的 content_sha256 去重、排序后的集合，
// 与 handler 落库用同一个 teamskills.ContentSHA256。
func wantSHASet(bodies []string) string {
	seen := map[string]bool{}
	var out []string
	for _, b := range bodies {
		sha := teamskills.ContentSHA256([]teamskills.File{{Path: "SKILL.md", Content: skillMD("acme-testing", b)}})
		if !seen[sha] {
			seen[sha] = true
			out = append(out, sha)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// ---------------------------------------------------------------- AC-9

func TestArchiveHidesFromListAndPins(t *testing.T) {
	e := newTSAPIEnv(t)
	u := e.user(t, "arch")
	sid := newSkill(t, u, "x")
	if resp := do(t, u.srv, "DELETE", fmt.Sprintf("%s/%d", u.base(), sid), "", true); resp.StatusCode != http.StatusOK {
		t.Fatalf("归档 got %d", resp.StatusCode)
	}
	body := decode(t, do(t, u.srv, "GET", u.base(), "", true))
	if skills, _ := body["skills"].([]any); len(skills) != 0 {
		t.Errorf("归档后列表应为空，got %v", skills)
	}
	pins, err := e.st.ListEnabledTeamSkills(context.Background(), u.repoID, u.id)
	if err != nil || len(pins) != 0 {
		t.Errorf("归档后新任务的钉集合应为空，got %v %v", pins, err)
	}
	// 归档的技能不能再写版本（404，与不存在同等）
	if code, _ := u.appendV(t, sid, "y"); code != http.StatusNotFound {
		t.Errorf("归档技能写新版本 got %d, want 404", code)
	}
}

func TestArchiveFreesSlug(t *testing.T) {
	e := newTSAPIEnv(t)
	u := e.user(t, "free")
	sid := newSkill(t, u, "x")
	do(t, u.srv, "DELETE", fmt.Sprintf("%s/%d", u.base(), sid), "", true)
	if code, body := u.create(t, "acme-testing", "y"); code != http.StatusCreated {
		t.Errorf("归档后同名新建 got %d (%v), want 201", code, body)
	}
}

// AC-9：PATCH 停用 → 列表仍可见、enabled=false；再启用恢复。
func TestDisabledSkillNotPinnedForNewTasks_API(t *testing.T) {
	e := newTSAPIEnv(t)
	u := e.user(t, "dis")
	newSkill(t, u, "x")
	code, b := u.create(t, "s-two", "y")
	if code != http.StatusCreated {
		t.Fatalf("新建 s-two got %d", code)
	}
	s2 := skillID(t, b)
	patch := func(v bool) {
		resp := do(t, u.srv, "PATCH", fmt.Sprintf("%s/%d", u.base(), s2), fmt.Sprintf(`{"enabled":%v}`, v), true)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PATCH enabled=%v got %d", v, resp.StatusCode)
		}
	}
	patch(false)
	pins, _ := e.st.ListEnabledTeamSkills(context.Background(), u.repoID, u.id)
	if len(pins) != 1 || pins[0].Slug != "team-acme-testing" {
		t.Errorf("停用后 ListEnabled got %+v, want 只有 team-acme-testing", pins)
	}
	body := decode(t, do(t, u.srv, "GET", u.base(), "", true))
	var sawDisabled bool
	for _, s := range body["skills"].([]any) {
		m := s.(map[string]any)
		if int64(m["id"].(float64)) == s2 && m["enabled"] == false {
			sawDisabled = true
		}
	}
	if !sawDisabled {
		t.Errorf("列表应仍含 s-two 且 enabled=false，got %v", body["skills"])
	}
	patch(true)
	if pins, _ := e.st.ListEnabledTeamSkills(context.Background(), u.repoID, u.id); len(pins) != 2 {
		t.Errorf("重新启用后 ListEnabled got %d 个, want 2", len(pins))
	}
}

// 校验失败一律 400 + 中文原因（slug、路径、title 超长）。
func TestCreateTeamSkill_ValidationErrorsAre400(t *testing.T) {
	e := newTSAPIEnv(t)
	u := e.user(t, "val")
	cases := []struct{ name, body string }{
		{"保留名", skillJSON("go-testing", "x")},
		{"team前缀", skillJSON("team-x", "x")},
		{"路径穿越", `{"slug":"acme","files":[{"path":"SKILL.md","content":"---\nname: a\ndescription: d\n---\n"},{"path":"../x","content":"y"}]}`},
		{"title超长", `{"slug":"acme","title":"` + strings.Repeat("测", 101) + `","files":[{"path":"SKILL.md","content":"---\nname: a\ndescription: d\n---\n"}]}`},
		{"请求体不是JSON", `{`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := do(t, u.srv, "POST", u.base(), c.body, true)
			body := decode(t, resp)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("got %d (%v), want 400", resp.StatusCode, body)
			}
			if msg, _ := body["error"].(string); msg == "" {
				t.Error("应有中文原因")
			}
		})
	}
}
