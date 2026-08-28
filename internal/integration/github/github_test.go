package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubServer 起一个假 GitHub API，返回按路径注册的响应。
func stubServer(t *testing.T, routes map[string]http.HandlerFunc) *Client {
	t.Helper()

	mux := http.NewServeMux()
	for pattern, h := range routes {
		mux.HandleFunc(pattern, h)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("未预期的请求: %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"message":"unexpected"}`, http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := NewClientWithBaseURL("fake-token", srv.URL)
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}
	return c
}

func TestNewClientRequiresToken(t *testing.T) {
	if _, err := NewClient(""); err == nil {
		t.Error("空 token 应报错")
	}
	if _, err := NewClient("   "); err == nil {
		t.Error("空白 token 应报错")
	}
}

func TestSplitRepo(t *testing.T) {
	owner, name, err := SplitRepo("Clouditera/CloudRouter")
	if err != nil || owner != "Clouditera" || name != "CloudRouter" {
		t.Errorf("SplitRepo = (%q, %q, %v)", owner, name, err)
	}
	for _, bad := range []string{"", "noslash", "a/b/c", "/b", "a/"} {
		if _, _, err := SplitRepo(bad); err == nil {
			t.Errorf("%q 应被判为非法仓库标识", bad)
		}
	}
}

func TestCreatePRValidation(t *testing.T) {
	c := stubServer(t, nil)
	ctx := context.Background()

	cases := []struct {
		name    string
		p       PRParams
		wantErr string
	}{
		{"缺head", PRParams{ProviderRepo: "a/b", Base: "dev", Title: "t"}, "不能为空"},
		{"缺base", PRParams{ProviderRepo: "a/b", Head: "fix/x", Title: "t"}, "不能为空"},
		{"head等于base", PRParams{ProviderRepo: "a/b", Head: "dev", Base: "dev", Title: "t"}, "不能相同"},
		{"缺标题", PRParams{ProviderRepo: "a/b", Head: "fix/x", Base: "dev"}, "标题"},
		{"仓库格式错", PRParams{ProviderRepo: "bad", Head: "fix/x", Base: "dev", Title: "t"}, "owner/name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.CreatePR(ctx, tc.p); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("期望错误含 %q，得到 %v", tc.wantErr, err)
			}
		})
	}
}

func TestCreatePRSuccess(t *testing.T) {
	var gotBody map[string]any

	c := stubServer(t, map[string]http.HandlerFunc{
		"/repos/acme/demo/pulls": func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet: // 查既有 PR：无
				_, _ = w.Write([]byte(`[]`))
			case http.MethodPost:
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"number":42,"html_url":"https://github.com/acme/demo/pull/42","state":"open"}`))
			}
		},
	})

	pr, err := c.CreatePR(context.Background(), PRParams{
		ProviderRepo: "acme/demo",
		Head:         "fix/cr-1-thing",
		Base:         "dev",
		Title:        "fix(cr-1): thing",
		Body:         "正文",
	})
	if err != nil {
		t.Fatalf("CreatePR 失败: %v", err)
	}
	if pr.Number != 42 {
		t.Errorf("PR 号 = %d，期望 42", pr.Number)
	}
	if pr.URL != "https://github.com/acme/demo/pull/42" {
		t.Errorf("PR URL = %q", pr.URL)
	}
	if pr.Existing {
		t.Error("新建的 PR 不应标记为 Existing")
	}
	if gotBody["head"] != "fix/cr-1-thing" || gotBody["base"] != "dev" {
		t.Errorf("请求体 head/base 不符: %+v", gotBody)
	}
}

// 已有同 head→base 的开放 PR 时应复用，而不是报错。
// 任务重试或租约重派会走到这一步，既有 PR 是正确结果。
func TestCreatePRReusesExisting(t *testing.T) {
	posted := false

	c := stubServer(t, map[string]http.HandlerFunc{
		"/repos/acme/demo/pulls": func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				posted = true
				t.Error("已有 PR 时不应再发创建请求")
				return
			}
			_, _ = w.Write([]byte(`[{"number":7,"html_url":"https://github.com/acme/demo/pull/7","state":"open","head":{"ref":"fix/cr-1-thing"}}]`))
		},
	})

	pr, err := c.CreatePR(context.Background(), PRParams{
		ProviderRepo: "acme/demo", Head: "fix/cr-1-thing", Base: "dev", Title: "t",
	})
	if err != nil {
		t.Fatalf("CreatePR 失败: %v", err)
	}
	if pr.Number != 7 || !pr.Existing {
		t.Errorf("应复用既有 PR #7 并标记 Existing，得到 %+v", pr)
	}
	if posted {
		t.Error("不应发出创建请求")
	}
}

// head 与 base 无差异时 GitHub 返回 422，应转成可识别的 ErrNoCommits。
func TestCreatePRNoCommits(t *testing.T) {
	c := stubServer(t, map[string]http.HandlerFunc{
		"/repos/acme/demo/pulls": func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"message":"No commits between dev and fix/cr-1"}]}`))
		},
	})

	_, err := c.CreatePR(context.Background(), PRParams{
		ProviderRepo: "acme/demo", Head: "fix/cr-1", Base: "dev", Title: "t",
	})
	if !errors.Is(err, ErrNoCommits) {
		t.Errorf("应返回 ErrNoCommits，得到 %v", err)
	}
}

// 并发下 PR 可能刚被别处创建：422 后重查应能捞到并复用。
func TestCreatePRRaceRecoversByRelookup(t *testing.T) {
	getCount := 0

	c := stubServer(t, map[string]http.HandlerFunc{
		"/repos/acme/demo/pulls": func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				getCount++
				if getCount == 1 {
					_, _ = w.Write([]byte(`[]`)) // 首查：还没有
					return
				}
				// 重查：已被别处创建
				_, _ = w.Write([]byte(`[{"number":9,"html_url":"u","state":"open","head":{"ref":"fix/cr-1"}}]`))
				return
			}
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"A pull request already exists"}`))
		},
	})

	pr, err := c.CreatePR(context.Background(), PRParams{
		ProviderRepo: "acme/demo", Head: "fix/cr-1", Base: "dev", Title: "t",
	})
	if err != nil {
		t.Fatalf("并发场景应能复用既有 PR，得到错误: %v", err)
	}
	if pr.Number != 9 || !pr.Existing {
		t.Errorf("应复用 PR #9，得到 %+v", pr)
	}
}

func TestReviewComments(t *testing.T) {
	c := stubServer(t, map[string]http.HandlerFunc{
		"/repos/acme/demo/pulls/42/comments": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[{"id":1,"body":"这里改一下","path":"main.go","line":10,"user":{"login":"reviewer"}}]`))
		},
		"/repos/acme/demo/issues/42/comments": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[{"id":2,"body":"整体看着行","user":{"login":"zichuan"}}]`))
		},
	})

	comments, err := c.ReviewComments(context.Background(), "acme/demo", 42)
	if err != nil {
		t.Fatalf("ReviewComments 失败: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("应得到 2 条评论，得到 %d", len(comments))
	}

	inline := comments[0]
	if inline.Path != "main.go" || inline.Line != 10 || inline.Author != "reviewer" {
		t.Errorf("行内评论解析不符: %+v", inline)
	}
	if comments[1].Body != "整体看着行" {
		t.Errorf("普通评论解析不符: %+v", comments[1])
	}
}

func TestGetPR(t *testing.T) {
	c := stubServer(t, map[string]http.HandlerFunc{
		"/repos/acme/demo/pulls/42": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"number":42,"state":"closed","merged":true}`))
		},
	})

	pr, err := c.GetPR(context.Background(), "acme/demo", 42)
	if err != nil {
		t.Fatalf("GetPR 失败: %v", err)
	}
	if !pr.GetMerged() {
		t.Error("应能读出已合并状态")
	}
}

// TestGetPRInfoExtractsHeadSHA 验证 F4.4（前驱被改重验）赖以判定
// "PR 仍 open 但内容被 force-push 改写"的信号——GetPRInfo 必须能从
// PR 的 head.sha 字段正确取值填进 PRInfo.HeadSHA。
func TestGetPRInfoExtractsHeadSHA(t *testing.T) {
	c := stubServer(t, map[string]http.HandlerFunc{
		"/repos/acme/demo/pulls/7": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"number":7,"state":"open","merged":false,
				"base":{"ref":"dev"},"head":{"sha":"abc123deadbeef"}}`))
		},
	})

	info, err := c.GetPRInfo(context.Background(), "acme/demo", 7)
	if err != nil {
		t.Fatalf("GetPRInfo 失败: %v", err)
	}
	if info.HeadSHA != "abc123deadbeef" {
		t.Errorf("HeadSHA = %q，期望 %q", info.HeadSHA, "abc123deadbeef")
	}
	if info.State != "open" || info.Merged {
		t.Errorf("state/merged 应保持 open/false，实际 state=%q merged=%v", info.State, info.Merged)
	}
	if info.BaseRef != "dev" {
		t.Errorf("BaseRef = %q，期望 dev", info.BaseRef)
	}
}

// PR 正文必须带上验证证据 —— 这是本产品与「随手产出的 diff」的区别。
func TestBuildPRBody(t *testing.T) {
	body := BuildPRBody(
		"CR-1326",
		"https://linear.app/x/issue/CR-1326",
		"验证通过（light 档）\n  ✓ build (.) 1.5s\n",
		"改了导入逻辑",
	)

	for _, want := range []string{
		"CR-1326",
		"https://linear.app/x/issue/CR-1326",
		"## 改动说明",
		"改了导入逻辑",
		"## 验证结果",
		"验证通过（light 档）",
		"合并前请人工复核",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("PR 正文应含 %q\n实际:\n%s", want, body)
		}
	}
}

func TestBuildPRBodyMinimal(t *testing.T) {
	body := BuildPRBody("", "", "", "")
	if !strings.Contains(body, "合并前请人工复核") {
		t.Errorf("即使无其他信息也应保留人工复核提示: %s", body)
	}
	if strings.Contains(body, "## 改动说明") {
		t.Error("无内容时不应留空标题")
	}
}
