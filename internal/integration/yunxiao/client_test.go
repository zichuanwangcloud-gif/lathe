package yunxiao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// testToken 带随机量：断言「错误串不含令牌」时不会被某个固定串的巧合
// 命中骗过去。
var testToken = "yx-token-" + strconv.FormatInt(time.Now().UnixNano(), 36)

// recorder 记下假云效收到的每个请求（方法、路径、token 头、正文）。
type recorder struct {
	mu    sync.Mutex
	reqs  []recorded
	count atomic.Int64
}

type recorded struct {
	Method, Path, Token, Body string
}

func (r *recorder) add(req *http.Request) recorded {
	b, _ := io.ReadAll(req.Body)
	rec := recorded{Method: req.Method, Path: req.URL.Path, Token: req.Header.Get(HeaderToken), Body: string(b)}
	r.mu.Lock()
	r.reqs = append(r.reqs, rec)
	r.mu.Unlock()
	r.count.Add(1)
	return rec
}

func (r *recorder) all() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.reqs...)
}

// fakeYunxiao 起一个 httptest 假云效：handler 按路径分派。
func fakeYunxiao(t *testing.T, h func(w http.ResponseWriter, r *http.Request, rec recorded)) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := rec.add(r)
		h(w, r, got)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func testClient(t *testing.T, srv *httptest.Server, org string) *Client {
	t.Helper()
	c, err := NewClientWithHTTP(Config{Endpoint: srv.URL, OrganizationID: org, Token: testToken}, srv.Client(), true)
	if err != nil {
		t.Fatalf("NewClientWithHTTP: got err %v, want nil", err)
	}
	return c
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("读 fixture %s: %v", name, err)
	}
	return string(b)
}

// AC-1
func TestClientGetWorkitem(t *testing.T) {
	body := fixture(t, "get_workitem.json")
	ctx := context.Background()

	t.Run("center_path", func(t *testing.T) {
		srv, rec := fakeYunxiao(t, func(w http.ResponseWriter, r *http.Request, _ recorded) { _, _ = io.WriteString(w, body) })
		if _, err := testClient(t, srv, "ORG").GetWorkitem(ctx, "WI"); err != nil {
			t.Fatalf("GetWorkitem: got err %v, want nil", err)
		}
		reqs := rec.all()
		if len(reqs) != 1 {
			t.Fatalf("请求数 got %d want 1", len(reqs))
		}
		if got, want := reqs[0].Path, "/oapi/v1/projex/organizations/ORG/workitems/WI"; got != want {
			t.Errorf("path got %q want %q", got, want)
		}
		if reqs[0].Method != http.MethodGet {
			t.Errorf("method got %q want GET", reqs[0].Method)
		}
		if reqs[0].Token != testToken {
			t.Errorf("x-yunxiao-token got %q want %q", reqs[0].Token, testToken)
		}
	})

	t.Run("region_path", func(t *testing.T) {
		srv, rec := fakeYunxiao(t, func(w http.ResponseWriter, r *http.Request, _ recorded) { _, _ = io.WriteString(w, body) })
		if _, err := testClient(t, srv, "").GetWorkitem(ctx, "WI"); err != nil {
			t.Fatalf("GetWorkitem: got err %v, want nil", err)
		}
		if got, want := rec.all()[0].Path, "/oapi/v1/projex/workitems/WI"; got != want {
			t.Errorf("path got %q want %q", got, want)
		}
	})

	t.Run("field_mapping", func(t *testing.T) {
		srv, _ := fakeYunxiao(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
			if strings.HasSuffix(rec.Path, "/comments") {
				_, _ = io.WriteString(w, "[]")
				return
			}
			_, _ = io.WriteString(w, body)
		})
		tr := NewTracker(testClient(t, srv, "ORG"), StatusMapping{})
		iss, err := tr.Issue(ctx, "a1b2c3d4e5f6a7b8c9d0e1f2")
		if err != nil {
			t.Fatalf("Issue: got err %v, want nil", err)
		}
		checks := []struct{ name, got, want string }{
			{"ID", iss.ID, "a1b2c3d4e5f6a7b8c9d0e1f2"},
			{"Identifier", iss.Identifier, "LATHE-42"},
			{"Title", iss.Title, "导入按钮点了没反应"},
			{"StateName", iss.StateName, "待处理"},
			{"AssigneeID", iss.AssigneeID, "user-0001"},
			{"Labels", strings.Join(iss.Labels, ","), "lathe:go,前端"},
			{"URL", iss.URL, ""},
		}
		for _, c := range checks {
			if c.got != c.want {
				t.Errorf("%s got %q want %q", c.name, c.got, c.want)
			}
		}
		if !strings.Contains(iss.Description, "点导入没反应") || strings.Contains(iss.Description, "<p>") {
			t.Errorf("Description 应为纯文本，got %q", iss.Description)
		}
	})

	t.Run("serial_empty_falls_back_to_id", func(t *testing.T) {
		var m map[string]any
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatal(err)
		}
		m["serialNumber"] = ""
		b, _ := json.Marshal(m)
		srv, _ := fakeYunxiao(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
			if strings.HasSuffix(rec.Path, "/comments") {
				_, _ = io.WriteString(w, "[]")
				return
			}
			_, _ = w.Write(b)
		})
		iss, err := NewTracker(testClient(t, srv, "ORG"), StatusMapping{}).Issue(ctx, "a1b2c3d4e5f6a7b8c9d0e1f2")
		if err != nil {
			t.Fatalf("Issue: got err %v, want nil", err)
		}
		if got, want := iss.Identifier, "a1b2c3d4e5f6a7b8c9d0e1f2"; got != want {
			t.Errorf("Identifier got %q want %q", got, want)
		}
	})
}

const commentsBody = `[
  {"id":"c1","content":"<p>复现步骤：</p><ol><li>打开页面</li><li>点导入</li></ol>","contentFormat":"RICHTEXT","user":{"id":"u1","name":"王五"},"gmtCreate":1731300000000},
  {"id":"c2","content":"**补充**：只在 Safari 上","contentFormat":"MARKDOWN","user":{"id":"u2","name":"赵六"},"gmtCreate":1731300001000}
]`

// AC-2
func TestClientListComments(t *testing.T) {
	ctx := context.Background()
	list := func(t *testing.T) ([]Comment, *recorder) {
		t.Helper()
		srv, rec := fakeYunxiao(t, func(w http.ResponseWriter, r *http.Request, _ recorded) { _, _ = io.WriteString(w, commentsBody) })
		cs, err := testClient(t, srv, "ORG").ListComments(ctx, "WI")
		if err != nil {
			t.Fatalf("ListComments: got err %v, want nil", err)
		}
		return cs, rec
	}

	t.Run("bare_array", func(t *testing.T) {
		cs, rec := list(t)
		if len(cs) != 2 {
			t.Fatalf("评论数 got %d want 2", len(cs))
		}
		if cs[0].ID != "c1" || cs[0].User.Name != "王五" || cs[1].ContentFormat != "MARKDOWN" {
			t.Errorf("字段映射不符: got %+v", cs)
		}
		if got, want := rec.all()[0].Path, "/oapi/v1/projex/organizations/ORG/workitems/WI/comments"; got != want {
			t.Errorf("path got %q want %q", got, want)
		}
	})

	t.Run("richtext_to_plain", func(t *testing.T) {
		cs, _ := list(t)
		if len(cs) < 2 {
			t.Fatalf("评论数 got %d want 2", len(cs))
		}
		got := cs[0].Content
		if strings.Contains(got, "<") || !strings.Contains(got, "打开页面") || !strings.Contains(got, "点导入") {
			t.Errorf("RICHTEXT 评论应转纯文本，got %q", got)
		}
		if got, want := cs[1].Content, "**补充**：只在 Safari 上"; got != want {
			t.Errorf("MARKDOWN 评论应原样，got %q want %q", got, want)
		}
	})
}

// AC-2
func TestTrackerIssueContext(t *testing.T) {
	body := fixture(t, "get_workitem.json")
	srv, _ := fakeYunxiao(t, func(w http.ResponseWriter, r *http.Request, rec recorded) {
		if strings.HasSuffix(rec.Path, "/comments") {
			_, _ = io.WriteString(w, commentsBody)
			return
		}
		_, _ = io.WriteString(w, body)
	})
	t.Run("description_and_comments", func(t *testing.T) {
		iss, err := NewTracker(testClient(t, srv, "ORG"), StatusMapping{}).Issue(context.Background(), "a1b2c3d4e5f6a7b8c9d0e1f2")
		if err != nil {
			t.Fatalf("Issue: got err %v", err)
		}
		ctxText := iss.Context()
		for _, want := range []string{"LATHE-42", "期望弹出文件选择框", "**王五**", "打开页面", "**赵六**", "只在 Safari 上"} {
			if !strings.Contains(ctxText, want) {
				t.Errorf("Context() 缺 %q，got:\n%s", want, ctxText)
			}
		}
		if strings.Contains(ctxText, "链接:") {
			t.Errorf("URL 为空时不应输出链接行，got:\n%s", ctxText)
		}
	})
}

// AC-3
func TestClientCreateComment(t *testing.T) {
	ctx := context.Background()
	t.Run("markdown_body_exact", func(t *testing.T) {
		srv, rec := fakeYunxiao(t, func(w http.ResponseWriter, r *http.Request, _ recorded) {
			_, _ = io.WriteString(w, `{"id":"cm-99"}`)
		})
		id, err := testClient(t, srv, "ORG").CreateComment(ctx, "WI", "Lathe 已开出 PR：\nhttps://x/pull/1 \"引号\"")
		if err != nil {
			t.Fatalf("CreateComment: got err %v", err)
		}
		if id != "cm-99" {
			t.Errorf("id got %q want cm-99", id)
		}
		reqs := rec.all()
		if len(reqs) != 1 {
			t.Fatalf("请求数 got %d want 1", len(reqs))
		}
		if reqs[0].Method != http.MethodPost || reqs[0].Path != "/oapi/v1/projex/organizations/ORG/workitems/WI/comments" {
			t.Errorf("got %s %s want POST …/workitems/WI/comments", reqs[0].Method, reqs[0].Path)
		}
		want, _ := json.Marshal(map[string]string{"content": "Lathe 已开出 PR：\nhttps://x/pull/1 \"引号\"", "formatType": "MARKDOWN"})
		var gotM, wantM map[string]any
		if err := json.Unmarshal([]byte(reqs[0].Body), &gotM); err != nil {
			t.Fatalf("body 不是 JSON: %q", reqs[0].Body)
		}
		_ = json.Unmarshal(want, &wantM)
		if fmt.Sprint(gotM) != fmt.Sprint(wantM) || len(gotM) != 2 {
			t.Errorf("body got %s want %s", reqs[0].Body, want)
		}
	})
	t.Run("empty_body_no_request", func(t *testing.T) {
		srv, rec := fakeYunxiao(t, func(w http.ResponseWriter, r *http.Request, _ recorded) { _, _ = io.WriteString(w, `{"id":"x"}`) })
		if _, err := testClient(t, srv, "ORG").CreateComment(ctx, "WI", "  \n "); err == nil {
			t.Error("空 body: got nil err, want error")
		}
		if n := rec.count.Load(); n != 0 {
			t.Errorf("请求数 got %d want 0", n)
		}
	})
}

const sentinel = "SENTINEL-UPSTREAM-BODY-7f3a9c"

// AC-4
func TestClientErrorClassification(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		status int
		body   string
		want   []string
	}{
		{"status_401", 401, `{"errorMessage":"` + sentinel + `"}`, []string{"401", "令牌无效"}},
		{"status_403", 403, `{"errorMessage":"` + sentinel + `"}`, []string{"403", "权限"}},
		{"status_404", 404, `<html>` + sentinel + `</html>`, []string{"404", "不存在"}},
		{"status_429", 429, sentinel, []string{"429", "限流"}},
		{"status_500", 500, sentinel, []string{"500", "服务端错误"}},
		{"status_418_other", 418, sentinel, []string{"418", "非预期"}},
		{"shape_mismatch_2xx", 200, `["` + sentinel + `"]`, []string{"200", "形状"}},
	}
	var errs []string
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := fakeYunxiao(t, func(w http.ResponseWriter, r *http.Request, _ recorded) {
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
			})
			_, err := testClient(t, srv, "ORG").GetWorkitem(ctx, "WI")
			if err == nil {
				t.Fatalf("got nil err, want error")
			}
			msg := err.Error()
			errs = append(errs, msg)
			for _, w := range c.want {
				if !strings.Contains(msg, w) {
					t.Errorf("错误串 %q 缺 %q", msg, w)
				}
			}
			var ae *APIError
			if !asAPIError(err, &ae) || ae.Status != c.status {
				t.Errorf("应为 *APIError 且 Status=%d，got %T %v", c.status, err, err)
			}
		})
	}
	t.Run("body_never_echoed", func(t *testing.T) {
		if len(errs) != len(cases) {
			t.Fatalf("收集到的错误串 got %d want %d", len(errs), len(cases))
		}
		for _, m := range errs {
			for _, frag := range []string{sentinel, "SENTINEL", "7f3a9c", "errorMessage", "<html>"} {
				if strings.Contains(m, frag) {
					t.Errorf("错误串 %q 回显了响应体片段 %q", m, frag)
				}
			}
		}
	})
	t.Run("token_never_in_error", func(t *testing.T) {
		if len(errs) == 0 {
			t.Fatal("没有错误串")
		}
		// 连接失败的错误路径也要查：拨号错误串里常常带 URL
		srv := httptest.NewServer(http.NotFoundHandler())
		c := testClient(t, srv, "ORG")
		srv.Close()
		_, err := c.GetWorkitem(ctx, "WI")
		if err == nil {
			t.Fatal("连不上: got nil err")
		}
		for _, m := range append(errs, err.Error()) {
			if strings.Contains(m, testToken) {
				t.Errorf("错误串 %q 含令牌", m)
			}
		}
	})
	t.Run("valid_utf8", func(t *testing.T) {
		if len(errs) == 0 {
			t.Fatal("没有错误串")
		}
		for _, m := range errs {
			if !utf8.ValidString(m) {
				t.Errorf("错误串不是合法 UTF-8: %q", m)
			}
		}
	})
}

func asAPIError(err error, target **APIError) bool { return errors.As(err, target) }
