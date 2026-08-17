package linear

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sign(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// ---------------------------------------------------------------- 签名

func TestVerifySignature(t *testing.T) {
	secret := "s3cret"
	body := []byte(`{"action":"create"}`)
	good := sign(t, secret, body)

	if err := VerifySignature(secret, body, good); err != nil {
		t.Errorf("正确签名应通过: %v", err)
	}
	// 容忍首尾空白
	if err := VerifySignature(secret, body, "  "+good+"  "); err != nil {
		t.Errorf("带空白的正确签名应通过: %v", err)
	}

	if err := VerifySignature(secret, body, "deadbeef"); !errors.Is(err, ErrBadSignature) {
		t.Errorf("错误签名应返回 ErrBadSignature，得到 %v", err)
	}
	if err := VerifySignature(secret, []byte(`{"action":"tampered"}`), good); !errors.Is(err, ErrBadSignature) {
		t.Errorf("载荷被篡改应返回 ErrBadSignature，得到 %v", err)
	}
	if err := VerifySignature(secret, body, ""); !errors.Is(err, ErrBadSignature) {
		t.Errorf("缺签名应返回 ErrBadSignature，得到 %v", err)
	}
	if err := VerifySignature("", body, good); err == nil {
		t.Error("未配置 secret 应报错")
	}
	// 换个 secret 必须失败 —— 否则等于没校验
	if err := VerifySignature("other", body, good); !errors.Is(err, ErrBadSignature) {
		t.Errorf("不同 secret 应失败，得到 %v", err)
	}
}

func TestParseWebhook(t *testing.T) {
	secret := "s3cret"
	body := []byte(`{"action":"update","type":"Issue","data":{"id":"abc","identifier":"CR-1326","title":"标题"}}`)

	ev, err := ParseWebhook(secret, body, sign(t, secret, body))
	if err != nil {
		t.Fatalf("ParseWebhook 失败: %v", err)
	}
	if ev.Action != "update" || ev.Type != "Issue" {
		t.Errorf("解析结果不符: %+v", ev)
	}
	if ev.Data.Identifier != "CR-1326" {
		t.Errorf("identifier = %q", ev.Data.Identifier)
	}

	if _, err := ParseWebhook(secret, body, "bad"); !errors.Is(err, ErrBadSignature) {
		t.Errorf("签名错误时不应解析载荷，得到 %v", err)
	}
	bad := []byte(`{not json`)
	if _, err := ParseWebhook(secret, bad, sign(t, secret, bad)); err == nil {
		t.Error("非法 JSON 应报错")
	}
}

// D2 判定：只在「指派发生变化」时接单，否则 issue 每次编辑都会重复接单。
func TestIsAssignedTo(t *testing.T) {
	const me = "user-me"

	cases := []struct {
		name string
		json string
		want bool
	}{
		{
			name: "新建即指派给我",
			json: `{"action":"create","type":"Issue","data":{"id":"1","assigneeId":"user-me"}}`,
			want: true,
		},
		{
			name: "改派给我",
			json: `{"action":"update","type":"Issue","data":{"id":"1","assigneeId":"user-me"},"updatedFrom":{"assigneeId":"user-other"}}`,
			want: true,
		},
		{
			name: "指派人在嵌套对象里",
			json: `{"action":"create","type":"Issue","data":{"id":"1","assignee":{"id":"user-me"}}}`,
			want: true,
		},
		{
			name: "★只改了标题_指派没变_不应重复接单",
			json: `{"action":"update","type":"Issue","data":{"id":"1","assigneeId":"user-me"},"updatedFrom":{"title":"旧标题"}}`,
			want: false,
		},
		{
			name: "指派给别人",
			json: `{"action":"update","type":"Issue","data":{"id":"1","assigneeId":"user-other"},"updatedFrom":{"assigneeId":null}}`,
			want: false,
		},
		{
			name: "无人指派",
			json: `{"action":"update","type":"Issue","data":{"id":"1"},"updatedFrom":{"assigneeId":"user-me"}}`,
			want: false,
		},
		{
			name: "删除事件",
			json: `{"action":"remove","type":"Issue","data":{"id":"1","assigneeId":"user-me"}}`,
			want: false,
		},
		{
			name: "评论事件不是 issue",
			json: `{"action":"create","type":"Comment","data":{"id":"1","assigneeId":"user-me"}}`,
			want: false,
		},
		{
			name: "update 但无 updatedFrom",
			json: `{"action":"update","type":"Issue","data":{"id":"1","assigneeId":"user-me"}}`,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ev WebhookEvent
			if err := json.Unmarshal([]byte(tc.json), &ev); err != nil {
				t.Fatalf("测试数据非法: %v", err)
			}
			if got := ev.IsAssignedTo(me); got != tc.want {
				t.Errorf("IsAssignedTo = %v，期望 %v", got, tc.want)
			}
		})
	}

	var nilEv *WebhookEvent
	if nilEv.IsAssignedTo(me) {
		t.Error("nil 事件不应判为已指派")
	}
	var ev WebhookEvent
	if ev.IsAssignedTo("") {
		t.Error("空 userID 不应判为已指派")
	}
}

// ---------------------------------------------------------------- GraphQL

func stubAPI(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := NewClientWithURL("fake-token", srv.URL)
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}
	return c
}

func TestNewClientRequiresToken(t *testing.T) {
	if _, err := NewClient(""); err == nil {
		t.Error("空 token 应报错")
	}
}

func TestFetchIssue(t *testing.T) {
	var gotAuth, gotQuery string

	c := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotQuery = req.Query

		_, _ = w.Write([]byte(`{"data":{"issue":{
			"id":"uuid-1","identifier":"CR-1326","title":"导入失败",
			"description":"点击导入没反应","url":"https://linear.app/x/CR-1326","priority":2,
			"state":{"name":"Todo"},
			"labels":{"nodes":[{"name":"bug"},{"name":"console"}]},
			"assignee":{"id":"user-me"},
			"comments":{"nodes":[{"id":"c1","body":"复现步骤：1. 打开设置","user":{"name":"张三"}}]}
		}}}`))
	})

	issue, err := c.Issue(context.Background(), "CR-1326")
	if err != nil {
		t.Fatalf("Issue 失败: %v", err)
	}

	if gotAuth != "fake-token" {
		t.Errorf("Authorization 头 = %q", gotAuth)
	}
	if !strings.Contains(gotQuery, "comments") {
		t.Error("查询应带上评论 —— 复现步骤常在评论里")
	}
	if issue.Identifier != "CR-1326" || issue.Title != "导入失败" {
		t.Errorf("issue 解析不符: %+v", issue)
	}
	if issue.StateName != "Todo" || issue.AssigneeID != "user-me" || issue.Priority != 2 {
		t.Errorf("字段解析不符: %+v", issue)
	}
	if len(issue.Labels) != 2 || issue.Labels[0] != "bug" {
		t.Errorf("标签解析不符: %v", issue.Labels)
	}
	if len(issue.Comments) != 1 || issue.Comments[0].UserName != "张三" {
		t.Errorf("评论解析不符: %+v", issue.Comments)
	}
}

// GraphQL 的错误走 HTTP 200 + errors 字段，必须显式检查否则会静默当成功。
func TestFetchIssueGraphQLError(t *testing.T) {
	c := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Authentication required"}]}`))
	})

	_, err := c.Issue(context.Background(), "CR-1")
	if err == nil {
		t.Fatal("GraphQL errors 应导致报错，而非静默当成功")
	}
	if !strings.Contains(err.Error(), "Authentication required") {
		t.Errorf("错误应带上服务端消息: %v", err)
	}
}

func TestFetchIssueNotFound(t *testing.T) {
	c := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"issue":null}}`))
	})
	if _, err := c.Issue(context.Background(), "CR-404"); err == nil {
		t.Error("issue 不存在应报错")
	}
	if _, err := c.Issue(context.Background(), ""); err == nil {
		t.Error("空标识应报错")
	}
}

func TestFetchIssueHTTPError(t *testing.T) {
	c := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"unauthorized"}`))
	})
	_, err := c.Issue(context.Background(), "CR-1")
	if err == nil {
		t.Fatal("HTTP 401 应报错")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("错误应带上状态码: %v", err)
	}
}

func TestComment(t *testing.T) {
	var gotVars map[string]any

	c := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotVars = req.Variables
		_, _ = w.Write([]byte(`{"data":{"commentCreate":{"success":true,"comment":{"id":"c-new"}}}}`))
	})

	id, err := c.Comment(context.Background(), "uuid-1", "验证通过，已开 PR")
	if err != nil {
		t.Fatalf("Comment 失败: %v", err)
	}
	if id != "c-new" {
		t.Errorf("评论 ID = %q", id)
	}
	if gotVars["issueId"] != "uuid-1" || gotVars["body"] != "验证通过，已开 PR" {
		t.Errorf("请求变量不符: %+v", gotVars)
	}

	if _, err := c.Comment(context.Background(), "", "x"); err == nil {
		t.Error("空 issue ID 应报错")
	}
	if _, err := c.Comment(context.Background(), "id", " "); err == nil {
		t.Error("空评论内容应报错")
	}
}

func TestCommentFailure(t *testing.T) {
	c := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"commentCreate":{"success":false}}}`))
	})
	if _, err := c.Comment(context.Background(), "id", "body"); err == nil {
		t.Error("success=false 应报错")
	}
}

// 交给 agent 的上下文必须包含评论 —— 复现步骤常写在评论而非正文里。
func TestIssueContext(t *testing.T) {
	i := Issue{
		Identifier:  "CR-1326",
		Title:       "导入失败",
		Description: "点击导入没反应",
		URL:         "https://linear.app/x/CR-1326",
		Labels:      []string{"bug"},
		Comments: []Comment{
			{Body: "复现步骤：1. 打开设置 2. 点导入", UserName: "张三"},
			{Body: "补充：只在 Safari 复现", UserName: ""},
		},
	}

	ctx := i.Context()
	for _, want := range []string{
		"CR-1326", "导入失败", "点击导入没反应",
		"bug", "复现步骤", "只在 Safari 复现", "张三",
	} {
		if !strings.Contains(ctx, want) {
			t.Errorf("上下文应含 %q\n实际:\n%s", want, ctx)
		}
	}
}

func TestIssueContextEmptyDescription(t *testing.T) {
	i := Issue{Identifier: "CR-1", Title: "无描述的单"}
	ctx := i.Context()
	if !strings.Contains(ctx, "（无描述）") {
		t.Errorf("空描述应显式标注，便于分诊判定不明确: %s", ctx)
	}
}

func TestAssignedIssues(t *testing.T) {
	var gotQuery string
	c := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotQuery = req.Query

		_, _ = w.Write([]byte(`{"data":{"viewer":{"assignedIssues":{"nodes":[
			{"id":"uuid-1","identifier":"CR-1326","title":"导入失败",
			 "url":"https://linear.app/x/CR-1326","priority":2,"updatedAt":"2025-01-02T03:04:05Z",
			 "state":{"name":"Todo"},"labels":{"nodes":[{"name":"bug"}]}},
			{"id":"uuid-2","identifier":"CR-1327","title":"文案调整",
			 "url":"https://linear.app/x/CR-1327","priority":0,"updatedAt":"2025-01-01T00:00:00Z",
			 "state":{"name":"In Progress"},"labels":{"nodes":[]}}
		]}}}}`))
	})

	issues, err := c.AssignedIssues(context.Background(), 50)
	if err != nil {
		t.Fatalf("AssignedIssues 失败: %v", err)
	}

	// 已完结的单子不该同步下来 —— 必须在查询层就过滤掉
	for _, want := range []string{"completed", "canceled"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("查询应排除 %s 状态的单子:\n%s", want, gotQuery)
		}
	}

	if len(issues) != 2 {
		t.Fatalf("应解析出 2 条，得到 %d", len(issues))
	}
	first := issues[0]
	if first.ID != "uuid-1" || first.Identifier != "CR-1326" || first.StateName != "Todo" {
		t.Errorf("首条解析不符: %+v", first)
	}
	if first.Priority != 2 || len(first.Labels) != 1 || first.UpdatedAt == "" {
		t.Errorf("字段解析不符: %+v", first)
	}
}

// 同步失败的提示必须可操作：令牌错了说令牌，网络断了说网络。
func TestAssignedIssuesAuthError(t *testing.T) {
	c := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"errors":[{"message":"Authentication required"}]}`))
	})
	if _, err := c.AssignedIssues(context.Background(), 50); err == nil ||
		!strings.Contains(err.Error(), "令牌") {
		t.Errorf("认证失败应提示重新签发令牌，得到: %v", err)
	}
}
