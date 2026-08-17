package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Clouditera/lathe/internal/integration/linear"
)

// stubLinear 按查询内容分发应答的最小 Linear GraphQL 桩。
func stubLinear(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		switch {
		case strings.Contains(req.Query, "assignedIssues"):
			_, _ = w.Write([]byte(`{"data":{"viewer":{"assignedIssues":{"nodes":[
				{"id":"uuid-1","identifier":"CR-1326","title":"导入失败",
				 "url":"https://linear.app/x/CR-1326","priority":2,"updatedAt":"2025-01-02T03:04:05Z",
				 "state":{"name":"Todo"},"labels":{"nodes":[{"name":"bug"}]}}
			]}}}}`))
		case strings.Contains(req.Query, "issue(id:"):
			_, _ = w.Write([]byte(`{"data":{"issue":{
				"id":"uuid-1","identifier":"CR-1326","title":"导入失败",
				"description":"点击导入没反应","url":"https://linear.app/x/CR-1326","priority":2,
				"state":{"name":"Todo"},"labels":{"nodes":[{"name":"bug"}]},
				"comments":{"nodes":[{"id":"c1","body":"复现步骤：1. 打开设置","user":{"name":"张三"}}]}
			}}}`))
		default:
			_, _ = w.Write([]byte(`{"errors":[{"message":"unexpected query"}]}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newLinearAPITestServer 用管理令牌通道（免数据库）起被测服务。
func newLinearAPITestServer(t *testing.T, clientFor LinearClientFor) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	(&LinearAPI{ClientFor: clientFor, Auth: NewAuth("test-token")}).Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func getAuthed(t *testing.T, srv *httptest.Server, path string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func TestLinearListIssues(t *testing.T) {
	stub := stubLinear(t)
	srv := newLinearAPITestServer(t, func(ctx context.Context, userID int64) (*linear.Client, error) {
		return linear.NewClientWithURL("fake-token", stub.URL)
	})

	code, body := getAuthed(t, srv, "/api/linear/issues")
	if code != http.StatusOK {
		t.Fatalf("应 200，得到 %d: %v", code, body)
	}
	issues, _ := body["issues"].([]any)
	if len(issues) != 1 {
		t.Fatalf("应返回 1 条 issue，得到 %v", body)
	}
	first, _ := issues[0].(map[string]any)
	if first["identifier"] != "CR-1326" || first["state"] != "Todo" {
		t.Errorf("issue 字段不符: %v", first)
	}
}

func TestLinearIssueDetail(t *testing.T) {
	stub := stubLinear(t)
	srv := newLinearAPITestServer(t, func(ctx context.Context, userID int64) (*linear.Client, error) {
		return linear.NewClientWithURL("fake-token", stub.URL)
	})

	code, body := getAuthed(t, srv, "/api/linear/issues/CR-1326")
	if code != http.StatusOK {
		t.Fatalf("应 200，得到 %d: %v", code, body)
	}
	if body["description"] != "点击导入没反应" {
		t.Errorf("详情应含描述: %v", body)
	}
	comments, _ := body["comments"].([]any)
	if len(comments) != 1 {
		t.Fatalf("详情应含评论 —— 复现步骤常在评论里: %v", body)
	}
	c0, _ := comments[0].(map[string]any)
	if c0["userName"] != "张三" {
		t.Errorf("评论应带作者名: %v", c0)
	}
}

// 没配凭据时不能说「同步失败」就完事，得指到设置页去。
func TestLinearIssuesWithoutCredentials(t *testing.T) {
	srv := newLinearAPITestServer(t, func(ctx context.Context, userID int64) (*linear.Client, error) {
		return nil, errors.New("creds: 凭据未配置（linear）")
	})

	code, body := getAuthed(t, srv, "/api/linear/issues")
	if code != http.StatusBadRequest {
		t.Fatalf("凭据缺失应 400，得到 %d", code)
	}
	if !strings.Contains(body["error"].(string), "设置") {
		t.Errorf("应指引去设置页配置凭据，得到 %q", body["error"])
	}
}

// 未登录一律 401：issue 标题与描述是内部信息，不该匿名可读。
func TestLinearIssuesRequireAuth(t *testing.T) {
	srv := newLinearAPITestServer(t, func(ctx context.Context, userID int64) (*linear.Client, error) {
		return linear.NewClient("fake-token")
	})

	resp, err := http.Get(srv.URL + "/api/linear/issues")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("未登录应 401，得到 %d", resp.StatusCode)
	}
}
