package gitea

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

type stub struct {
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string]http.HandlerFunc
	seen   []string
	auth   []string
}

func newStub(t *testing.T) *stub {
	s := &stub{routes: map[string]http.HandlerFunc{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.EscapedPath()
		s.mu.Lock()
		s.seen = append(s.seen, key)
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		h := s.routes[key]
		s.mu.Unlock()
		if h == nil {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		h(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stub) on(method, path string, h http.HandlerFunc) { s.routes[method+" "+path] = h }

func (s *stub) count(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, k := range s.seen {
		if k == key {
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
	c, err := New(s.srv.URL, "gitea-test-token")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// AC-9：开 PR，头 Authorization: token …
func TestGiteaCreatePR(t *testing.T) {
	s := newStub(t)
	const pulls = "/api/v1/repos/acme/demo/pulls"
	s.on("GET", pulls, jsonReply([]any{}))
	var got map[string]any
	s.on("POST", pulls, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		jsonReply(map[string]any{"number": 11, "html_url": "https://gitea.example.com/acme/demo/pulls/11", "state": "open"})(w, r)
	})
	pr, err := client(t, s).CreatePR(context.Background(), vcs.PRParams{ProviderRepo: "acme/demo", Head: "fix/a", Base: "dev", Title: "t", Body: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 11 || pr.URL != "https://gitea.example.com/acme/demo/pulls/11" || pr.Existing {
		t.Errorf("PullRequest = %+v", pr)
	}
	for k, v := range map[string]any{"head": "fix/a", "base": "dev", "title": "t", "body": "b"} {
		if got[k] != v {
			t.Errorf("请求体 %s = %v，want %v", k, got[k], v)
		}
	}
	for _, a := range s.auth {
		if a != "token gitea-test-token" {
			t.Errorf("Authorization = %q，want token …", a)
		}
	}
}

func TestGiteaCreatePRReusesExisting(t *testing.T) {
	s := newStub(t)
	const pulls = "/api/v1/repos/acme/demo/pulls"
	s.on("GET", pulls, jsonReply([]any{
		map[string]any{"number": 3, "html_url": "u3", "state": "open", "head": map[string]any{"ref": "other"}, "base": map[string]any{"ref": "dev"}},
		map[string]any{"number": 4, "html_url": "u4", "state": "open", "head": map[string]any{"ref": "fix/a"}, "base": map[string]any{"ref": "dev"}},
	}))
	s.on("POST", pulls, jsonReply(map[string]any{"number": 99}))
	pr, err := client(t, s).CreatePR(context.Background(), vcs.PRParams{ProviderRepo: "acme/demo", Head: "fix/a", Base: "dev", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if !pr.Existing || pr.Number != 4 {
		t.Errorf("PullRequest = %+v，want 复用 #4", pr)
	}
	if n := s.count("POST " + pulls); n != 0 {
		t.Errorf("复用时不应 POST，得到 %d", n)
	}
}

func TestGiteaCreatePRNoCommits(t *testing.T) {
	s := newStub(t)
	const pulls = "/api/v1/repos/acme/demo/pulls"
	s.on("GET", pulls, jsonReply([]any{}))
	s.on("POST", pulls, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"There are no changes between the head and the base"}`))
	})
	_, err := client(t, s).CreatePR(context.Background(), vcs.PRParams{ProviderRepo: "acme/demo", Head: "a", Base: "dev", Title: "t"})
	if !errors.Is(err, vcs.ErrNoCommits) {
		t.Fatalf("err = %v，want ErrNoCommits", err)
	}
}

func TestGiteaGetPRInfoMapsState(t *testing.T) {
	cases := []struct {
		name       string
		body       map[string]any
		wantMerged bool
		wantState  string
	}{
		{"open", map[string]any{"number": 7, "state": "open", "merged": false}, false, "open"},
		{"closed 未合并", map[string]any{"number": 7, "state": "closed", "merged": false}, false, "closed"},
		{"merged", map[string]any{"number": 7, "state": "closed", "merged": true}, true, "closed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t)
			tc.body["base"] = map[string]any{"ref": "dev"}
			tc.body["head"] = map[string]any{"ref": "fix/a", "sha": "deadbeef"}
			s.on("GET", "/api/v1/repos/acme/demo/pulls/7", jsonReply(tc.body))
			info, err := client(t, s).GetPRInfo(context.Background(), "acme/demo", 7)
			if err != nil {
				t.Fatal(err)
			}
			if info.Merged != tc.wantMerged || info.State != tc.wantState || info.BaseRef != "dev" || info.HeadSHA != "deadbeef" {
				t.Errorf("PRInfo = %+v", info)
			}
		})
	}
}

func user(login string) map[string]any { return map[string]any{"login": login} }

// AC-9（v6 扩充）：issue 评论 + review 总评 + 每个 review 的行内评论三者合并，
// 非协作者过滤，超过 10 页截断。
func TestGiteaReviewFeedback(t *testing.T) {
	s := newStub(t)
	const base = "/api/v1/repos/acme/demo"
	s.on("GET", base+"/pulls/7", jsonReply(map[string]any{"number": 7, "user": user("lathe-bot")}))
	commentPages := 0
	s.on("GET", base+"/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		commentPages++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if page <= 1 {
			list := []any{
				map[string]any{"id": 21, "body": "普通评论", "user": user("alice"), "html_url": "c21"},
				map[string]any{"id": 22, "body": "路人", "user": user("stranger")},
			}
			for len(list) < limit {
				list = append(list, map[string]any{"id": 1000 + len(list), "body": "填充", "user": user("alice")})
			}
			jsonReply(list)(w, r)
			return
		}
		list := make([]any, 0, limit)
		for i := 0; i < limit; i++ { // 每页都满：测 10 页上限
			list = append(list, map[string]any{"id": page*1000 + i, "body": fmt.Sprintf("p%d", page), "user": user("alice")})
		}
		jsonReply(list)(w, r)
	})
	s.on("GET", base+"/pulls/7/reviews", jsonReply([]any{
		map[string]any{"id": 31, "body": "总评：请改", "state": "REQUEST_CHANGES", "user": user("alice")},
		map[string]any{"id": 32, "body": "", "state": "COMMENT", "user": user("alice")},
	}))
	s.on("GET", base+"/pulls/7/reviews/31/comments", jsonReply([]any{
		map[string]any{"id": 41, "body": "行内意见", "path": "a.go", "position": 9, "user": user("alice")},
	}))
	s.on("GET", base+"/pulls/7/reviews/32/comments", jsonReply([]any{
		map[string]any{"id": 42, "body": "空总评下的行内意见", "path": "b.go", "position": 3, "user": user("alice")},
	}))
	s.on("GET", base+"/collaborators/alice/permission", jsonReply(map[string]any{"permission": "write"}))
	s.on("GET", base+"/collaborators/stranger/permission", jsonReply(map[string]any{"permission": "read"}))

	fb, err := client(t, s).ReviewFeedback(context.Background(), "acme/demo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if fb.PRAuthor != "lathe-bot" {
		t.Errorf("PRAuthor = %q", fb.PRAuthor)
	}
	keys := map[string]vcs.ReviewComment{}
	for _, c := range fb.Comments {
		keys[c.Key] = c
	}
	if _, ok := keys["gitea:comment:21"]; !ok {
		t.Error("缺普通评论 gitea:comment:21")
	}
	if _, ok := keys["gitea:comment:22"]; ok {
		t.Error("非协作者（read 权限）的评论应被过滤")
	}
	if c, ok := keys["gitea:review:31"]; !ok || c.Body != "总评：请改" {
		t.Errorf("缺 review 总评 31：%+v", c)
	}
	if _, ok := keys["gitea:review:32"]; ok {
		t.Error("正文为空的 review 总评应被跳过")
	}
	if c, ok := keys["gitea:review-comment:41"]; !ok || c.Path != "a.go" || c.Line != 9 {
		t.Errorf("行内意见 41 = %+v，want Path=a.go Line=9", c)
	}
	if _, ok := keys["gitea:review-comment:42"]; !ok {
		t.Error("空总评 review 下的行内意见仍应收集")
	}
	if commentPages != 10 {
		t.Errorf("issue 评论翻页 %d 次，want 截断在 10 页", commentPages)
	}
}

func TestVerifyGiteaIdentity(t *testing.T) {
	s := newStub(t)
	s.on("GET", "/api/v1/user", jsonReply(map[string]any{"id": 5, "login": "bob"}))
	acct, err := client(t, s).Verify(context.Background())
	if err != nil || acct.Login != "bob" {
		t.Fatalf("Verify = %+v %v，want bob", acct, err)
	}
}

func TestVerifyGiteaUnauthorized(t *testing.T) {
	s := newStub(t)
	s.on("GET", "/api/v1/user", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"token is required"}`, http.StatusUnauthorized)
	})
	if _, err := client(t, s).Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "令牌") {
		t.Fatalf("err = %v，want 含「令牌」", err)
	}
}
