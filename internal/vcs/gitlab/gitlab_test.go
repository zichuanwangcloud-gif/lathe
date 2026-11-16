package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/vcs"
)

// stub 是 GitLab API 的 httptest 假服务器：按 EscapedPath 路由，记录请求。
type stub struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string]http.HandlerFunc // "METHOD escapedPath"
	seen   []string
	tokens []string
}

func newStub(t *testing.T) *stub {
	s := &stub{t: t, routes: map[string]http.HandlerFunc{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.EscapedPath()
		s.mu.Lock()
		s.seen = append(s.seen, key+"?"+r.URL.RawQuery)
		s.tokens = append(s.tokens, r.Header.Get("PRIVATE-TOKEN"))
		h := s.routes[key]
		s.mu.Unlock()
		if h == nil {
			http.Error(w, `{"message":"404 Not Found"}`, http.StatusNotFound)
			return
		}
		h(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stub) on(method, path string, h http.HandlerFunc) { s.routes[method+" "+path] = h }

func (s *stub) count(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, k := range s.seen {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n
}

func jsonReply(v any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

func client(t *testing.T, s *stub) *Client {
	t.Helper()
	c, err := New(s.srv.URL, "glpat-test-token")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// AC-6：开 MR。
func TestGitLabCreateMR(t *testing.T) {
	s := newStub(t)
	const mrs = "/api/v4/projects/acme%2Fdemo/merge_requests"
	s.on("GET", mrs, jsonReply([]any{}))
	var got map[string]any
	s.on("POST", mrs, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		jsonReply(map[string]any{"iid": 7, "id": 9001, "web_url": "https://gitlab.example.com/acme/demo/-/merge_requests/7", "state": "opened"})(w, r)
	})
	pr, err := client(t, s).CreatePR(context.Background(), vcs.PRParams{
		ProviderRepo: "acme/demo", Head: "fix/cr-1-x", Base: "dev", Title: "fix(cr-1): x", Body: "正文",
	})
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if pr.Number != 7 || pr.URL != "https://gitlab.example.com/acme/demo/-/merge_requests/7" || pr.Existing {
		t.Errorf("PullRequest = %+v，want {Number:7, URL:web_url, Existing:false}", pr)
	}
	want := map[string]any{"source_branch": "fix/cr-1-x", "target_branch": "dev", "title": "fix(cr-1): x", "description": "正文"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("请求体 %s = %v，want %v", k, got[k], v)
		}
	}
	if s.count("POST "+mrs) != 1 {
		t.Errorf("应 POST 一次 %s", mrs)
	}
	for _, tok := range s.tokens {
		if tok != "glpat-test-token" {
			t.Errorf("请求头 PRIVATE-TOKEN = %q", tok)
		}
	}
}

func TestGitLabCreateMRSubgroupPathEncoded(t *testing.T) {
	s := newStub(t)
	const mrs = "/api/v4/projects/group%2Fsub%2Fdemo/merge_requests"
	s.on("GET", mrs, jsonReply([]any{}))
	s.on("POST", mrs, jsonReply(map[string]any{"iid": 3, "web_url": "u"}))
	if _, err := client(t, s).CreatePR(context.Background(), vcs.PRParams{ProviderRepo: "group/sub/demo", Head: "a", Base: "dev", Title: "t"}); err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if s.count("POST "+mrs) != 1 {
		t.Errorf("子组路径应整体 URL 编码，seen=%v", s.seen)
	}
}

// AC-6：已有开放 MR 则复用，不再 POST。
func TestGitLabCreateMRReusesExisting(t *testing.T) {
	s := newStub(t)
	const mrs = "/api/v4/projects/acme%2Fdemo/merge_requests"
	var q string
	s.on("GET", mrs, func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.RawQuery
		jsonReply([]any{map[string]any{"iid": 5, "web_url": "https://g/x/5", "state": "opened", "source_branch": "fix/a", "target_branch": "dev"}})(w, r)
	})
	s.on("POST", mrs, jsonReply(map[string]any{"iid": 99}))
	pr, err := client(t, s).CreatePR(context.Background(), vcs.PRParams{ProviderRepo: "acme/demo", Head: "fix/a", Base: "dev", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if !pr.Existing || pr.Number != 5 {
		t.Errorf("PullRequest = %+v，want 复用 #5", pr)
	}
	for _, want := range []string{"state=opened", "source_branch=fix%2Fa", "target_branch=dev"} {
		if !strings.Contains(q, want) {
			t.Errorf("查询串 %q 应含 %q", q, want)
		}
	}
	if n := s.count("POST " + mrs); n != 0 {
		t.Errorf("复用时不应 POST，得到 %d 次", n)
	}
}

// AC-6：无差异 → ErrNoCommits。
func TestGitLabCreateMRNoCommits(t *testing.T) {
	s := newStub(t)
	const mrs = "/api/v4/projects/acme%2Fdemo/merge_requests"
	s.on("GET", mrs, jsonReply([]any{}))
	s.on("POST", mrs, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":["You can't use same project/branch for source and target","Validate branches Cannot create a merge request without any commits"]}`))
	})
	_, err := client(t, s).CreatePR(context.Background(), vcs.PRParams{ProviderRepo: "acme/demo", Head: "a", Base: "dev", Title: "t"})
	if !errors.Is(err, vcs.ErrNoCommits) {
		t.Fatalf("err = %v，want ErrNoCommits", err)
	}
}

// AC-7：合并状态映射到统一三态。
func TestGitLabGetPRInfoMapsState(t *testing.T) {
	cases := []struct {
		state      string
		wantMerged bool
		wantState  string
	}{
		{"opened", false, "open"},
		{"closed", false, "closed"},
		{"merged", true, "closed"},
		{"locked", false, "open"},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			s := newStub(t)
			s.on("GET", "/api/v4/projects/acme%2Fdemo/merge_requests/7", jsonReply(map[string]any{
				"iid": 7, "state": tc.state, "target_branch": "dev", "sha": "abc123",
				"diff_refs": map[string]any{"head_sha": "zzz"},
			}))
			info, err := client(t, s).GetPRInfo(context.Background(), "acme/demo", 7)
			if err != nil {
				t.Fatal(err)
			}
			if info.Merged != tc.wantMerged || info.State != tc.wantState || info.BaseRef != "dev" || info.HeadSHA != "abc123" || info.Number != 7 {
				t.Errorf("PRInfo = %+v，want Merged=%v State=%s BaseRef=dev HeadSHA=abc123", info, tc.wantMerged, tc.wantState)
			}
		})
	}
	t.Run("sha 缺省退回 diff_refs.head_sha", func(t *testing.T) {
		s := newStub(t)
		s.on("GET", "/api/v4/projects/acme%2Fdemo/merge_requests/7", jsonReply(map[string]any{
			"iid": 7, "state": "opened", "target_branch": "dev", "diff_refs": map[string]any{"head_sha": "fallback"},
		}))
		info, err := client(t, s).GetPRInfo(context.Background(), "acme/demo", 7)
		if err != nil || info.HeadSHA != "fallback" {
			t.Errorf("HeadSHA = %v %v，want fallback", info, err)
		}
	})
}

func note(id int64, authorID int64, username, body string, system bool) map[string]any {
	return map[string]any{"id": id, "body": body, "system": system,
		"author": map[string]any{"id": authorID, "username": username}}
}

// AC-8：notes 与 discussions 合并、去重；system 笔记跳过；非成员过滤；超过 10 页截断。
func TestGitLabReviewFeedback(t *testing.T) {
	s := newStub(t)
	const base = "/api/v4/projects/acme%2Fdemo"
	s.on("GET", base+"/merge_requests/7", jsonReply(map[string]any{"iid": 7, "author": map[string]any{"id": 1, "username": "lathe-bot"}}))
	notePages := 0
	s.on("GET", base+"/merge_requests/7/notes", func(w http.ResponseWriter, r *http.Request) {
		notePages++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 0 {
			page = 1
		}
		var list []any
		if page == 1 {
			list = []any{
				note(101, 2, "alice", "普通评论", false),
				note(102, 3, "stranger", "路人评论", false),
				note(103, 2, "alice", "changed the description", true),
			}
		} else {
			list = []any{note(int64(200+page), 2, "alice", fmt.Sprintf("第 %d 页", page), false)}
		}
		w.Header().Set("X-Next-Page", strconv.Itoa(page+1)) // 永远有下一页：测上限
		jsonReply(list)(w, r)
	})
	s.on("GET", base+"/merge_requests/7/discussions", jsonReply([]any{
		map[string]any{"id": "d1", "notes": []any{
			map[string]any{"id": 301, "body": "行内意见", "system": false, "type": "DiffNote",
				"author":   map[string]any{"id": 2, "username": "alice"},
				"position": map[string]any{"new_path": "a.go", "new_line": 12}},
			note(101, 2, "alice", "普通评论", false), // 与 notes 重复，按 Key 去重
		}},
	}))
	s.on("GET", base+"/members/all/2", jsonReply(map[string]any{"id": 2, "access_level": 30}))
	s.on("GET", base+"/members/all/3", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })

	fb, err := client(t, s).ReviewFeedback(context.Background(), "acme/demo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if fb.PRAuthor != "lathe-bot" {
		t.Errorf("PRAuthor = %q", fb.PRAuthor)
	}
	keys := map[string]vcs.ReviewComment{}
	for _, c := range fb.Comments {
		if _, dup := keys[c.Key]; dup {
			t.Errorf("重复的意见 Key %s", c.Key)
		}
		keys[c.Key] = c
		if !strings.HasPrefix(c.Key, "gitlab:note:") {
			t.Errorf("Key %q 应以 gitlab:note: 开头", c.Key)
		}
	}
	if _, ok := keys["gitlab:note:101"]; !ok {
		t.Error("缺普通评论 101")
	}
	if c, ok := keys["gitlab:note:301"]; !ok || c.Path != "a.go" || c.Line != 12 {
		t.Errorf("行内意见 301 = %+v，want Path=a.go Line=12", c)
	}
	if _, ok := keys["gitlab:note:102"]; ok {
		t.Error("非成员（成员接口 404）的评论应被过滤")
	}
	if _, ok := keys["gitlab:note:103"]; ok {
		t.Error("system 笔记应被跳过")
	}
	if notePages != 10 {
		t.Errorf("notes 翻页 %d 次，want 截断在 10 页", notePages)
	}
}

// AC-10：保存时验证连通性。
func TestVerifyGitLabIdentity(t *testing.T) {
	s := newStub(t)
	s.on("GET", "/api/v4/user", jsonReply(map[string]any{"id": 5, "username": "alice", "name": "Alice"}))
	acct, err := client(t, s).Verify(context.Background())
	if err != nil || acct.Username != "alice" {
		t.Fatalf("Verify = %+v %v，want alice", acct, err)
	}
}

func TestVerifyGitLabUnauthorizedSurfacesMessage(t *testing.T) {
	s := newStub(t)
	s.on("GET", "/api/v4/user", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"401 Unauthorized"}`, http.StatusUnauthorized)
	})
	_, err := client(t, s).Verify(context.Background())
	if err == nil || !strings.Contains(err.Error(), "令牌") {
		t.Fatalf("err = %v，want 含「令牌」", err)
	}
	if strings.Contains(err.Error(), "glpat-test-token") {
		t.Error("错误文案泄露了令牌")
	}
}
