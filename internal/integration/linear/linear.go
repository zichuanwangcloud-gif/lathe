// Package linear 接入 Linear：接收 webhook、拉取 issue、回帖。
package linear

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultAPIURL 是 Linear GraphQL 端点。
const DefaultAPIURL = "https://api.linear.app/graphql"

// Client 访问 Linear GraphQL API。
type Client struct {
	apiURL string
	token  string
	http   *http.Client
}

// NewClient 构造客户端。
func NewClient(token string) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("linear: 缺少 API token")
	}
	return &Client{
		apiURL: DefaultAPIURL,
		token:  token,
		http:   &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// NewClientWithURL 供测试注入桩服务地址。
func NewClientWithURL(token, apiURL string) (*Client, error) {
	c, err := NewClient(token)
	if err != nil {
		return nil, err
	}
	c.apiURL = apiURL
	return c, nil
}

// Issue 是 Lathe 需要的 issue 字段。
type Issue struct {
	ID          string
	Identifier  string // 形如 CR-1326
	Title       string
	Description string
	URL         string
	StateName   string
	Priority    int
	Labels      []string
	AssigneeID  string
	Comments    []Comment
}

// Comment 是 issue 下的一条评论。
type Comment struct {
	ID       string
	Body     string
	UserName string
}

// Context 把 issue 及其评论拼成交给 agent 的任务描述。
//
// 评论必须带上：真实工作中补充的复现步骤、澄清、变更要求
// 往往在评论里而不在正文里。
func (i Issue) Context() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s: %s\n\n", i.Identifier, i.Title)
	if len(i.Labels) > 0 {
		fmt.Fprintf(&b, "标签: %s\n", strings.Join(i.Labels, ", "))
	}
	if i.URL != "" {
		fmt.Fprintf(&b, "链接: %s\n", i.URL)
	}
	b.WriteString("\n## 描述\n\n")
	if strings.TrimSpace(i.Description) == "" {
		b.WriteString("（无描述）\n")
	} else {
		b.WriteString(strings.TrimSpace(i.Description) + "\n")
	}
	if len(i.Comments) > 0 {
		b.WriteString("\n## 评论\n\n")
		for _, c := range i.Comments {
			name := c.UserName
			if name == "" {
				name = "（未知）"
			}
			fmt.Fprintf(&b, "**%s**: %s\n\n", name, strings.TrimSpace(c.Body))
		}
	}
	return b.String()
}

const issueQuery = `query($id: String!) {
  issue(id: $id) {
    id identifier title description url priority
    state { name }
    labels { nodes { name } }
    assignee { id }
    comments { nodes { id body user { name } } }
  }
}`

// Issue 按 ID 或 identifier（CR-1326）拉取 issue 及其评论。
func (c *Client) Issue(ctx context.Context, id string) (*Issue, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("linear: issue 标识为空")
	}

	var resp struct {
		Issue *struct {
			ID          string  `json:"id"`
			Identifier  string  `json:"identifier"`
			Title       string  `json:"title"`
			Description string  `json:"description"`
			URL         string  `json:"url"`
			Priority    float64 `json:"priority"`
			State       *struct {
				Name string `json:"name"`
			} `json:"state"`
			Labels struct {
				Nodes []struct {
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"labels"`
			Assignee *struct {
				ID string `json:"id"`
			} `json:"assignee"`
			Comments struct {
				Nodes []struct {
					ID   string `json:"id"`
					Body string `json:"body"`
					User *struct {
						Name string `json:"name"`
					} `json:"user"`
				} `json:"nodes"`
			} `json:"comments"`
		} `json:"issue"`
	}

	if err := c.do(ctx, issueQuery, map[string]any{"id": id}, &resp); err != nil {
		return nil, err
	}
	if resp.Issue == nil {
		return nil, fmt.Errorf("linear: issue %q 不存在或无权访问", id)
	}

	out := &Issue{
		ID:          resp.Issue.ID,
		Identifier:  resp.Issue.Identifier,
		Title:       resp.Issue.Title,
		Description: resp.Issue.Description,
		URL:         resp.Issue.URL,
		Priority:    int(resp.Issue.Priority),
	}
	if resp.Issue.State != nil {
		out.StateName = resp.Issue.State.Name
	}
	if resp.Issue.Assignee != nil {
		out.AssigneeID = resp.Issue.Assignee.ID
	}
	for _, l := range resp.Issue.Labels.Nodes {
		out.Labels = append(out.Labels, l.Name)
	}
	for _, cm := range resp.Issue.Comments.Nodes {
		c := Comment{ID: cm.ID, Body: cm.Body}
		if cm.User != nil {
			c.UserName = cm.User.Name
		}
		out.Comments = append(out.Comments, c)
	}
	return out, nil
}

const commentMutation = `mutation($issueId: String!, $body: String!) {
  commentCreate(input: {issueId: $issueId, body: $body}) {
    success
    comment { id }
  }
}`

// Comment 在 issue 下发一条评论。
func (c *Client) Comment(ctx context.Context, issueID, body string) (string, error) {
	if strings.TrimSpace(issueID) == "" {
		return "", fmt.Errorf("linear: issue ID 为空")
	}
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("linear: 评论内容为空")
	}

	var resp struct {
		CommentCreate struct {
			Success bool `json:"success"`
			Comment *struct {
				ID string `json:"id"`
			} `json:"comment"`
		} `json:"commentCreate"`
	}
	if err := c.do(ctx, commentMutation, map[string]any{"issueId": issueID, "body": body}, &resp); err != nil {
		return "", err
	}
	if !resp.CommentCreate.Success {
		return "", fmt.Errorf("linear: 发表评论失败（服务端返回 success=false）")
	}
	if resp.CommentCreate.Comment == nil {
		return "", nil
	}
	return resp.CommentCreate.Comment.ID, nil
}

// graphQLError 是 Linear 返回的单条错误。
type graphQLError struct {
	Message string `json:"message"`
}

// do 发起一次 GraphQL 请求并把 data 解进 out。
func (c *Client) do(ctx context.Context, query string, vars map[string]any, out any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return fmt.Errorf("linear: 序列化请求失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("linear: 构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("linear: 请求失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("linear: 读取响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("linear: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []graphQLError  `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("linear: 解析响应失败: %w", err)
	}
	// GraphQL 的错误走 200 + errors 字段，必须显式检查
	if len(envelope.Errors) > 0 {
		msgs := make([]string, 0, len(envelope.Errors))
		for _, e := range envelope.Errors {
			msgs = append(msgs, e.Message)
		}
		return fmt.Errorf("linear: GraphQL 错误: %s", strings.Join(msgs, "; "))
	}
	if len(envelope.Data) == 0 {
		return fmt.Errorf("linear: 响应缺少 data 字段")
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("linear: 解析 data 失败: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- Webhook

// ErrBadSignature 表示 webhook 签名校验不通过。
var ErrBadSignature = errors.New("linear: webhook 签名校验失败")

// VerifySignature 校验 Linear webhook 的 HMAC-SHA256 签名。
//
// 用 hmac.Equal 做常数时间比较，避免时序侧信道。
func VerifySignature(secret string, body []byte, signature string) error {
	if secret == "" {
		return fmt.Errorf("linear: 未配置 webhook secret")
	}
	if signature == "" {
		return fmt.Errorf("%w（缺少签名头）", ErrBadSignature)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expected), []byte(strings.TrimSpace(signature))) {
		return ErrBadSignature
	}
	return nil
}

// Webhook HTTP 头。
const (
	// HeaderSignature 携带 HMAC-SHA256 签名。
	HeaderSignature = "Linear-Signature"
	// HeaderDelivery 携带本次投递的唯一 ID，用于幂等去重。
	//
	// 刻意取自 HTTP 头而非载荷：载荷里的 webhookTimestamp 在重投递时
	// 保持不变但语义是"事件时间"，不是"投递标识"。
	HeaderDelivery = "Linear-Delivery"
)

// WebhookEvent 是 Lathe 关心的 webhook 载荷字段。
type WebhookEvent struct {
	Action    string `json:"action"` // create | update | remove
	Type      string `json:"type"`   // Issue | Comment | ...
	WebhookID string `json:"webhookId"`
	CreatedAt string `json:"createdAt"`
	Data      struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
		Title      string `json:"title"`
		AssigneeID string `json:"assigneeId"`
		Assignee   *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"assignee"`
		State *struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"state"`
	} `json:"data"`
	UpdatedFrom map[string]any `json:"updatedFrom"`
}

// ParseWebhook 校验签名并解析载荷。
func ParseWebhook(secret string, body []byte, signature string) (*WebhookEvent, error) {
	if err := VerifySignature(secret, body, signature); err != nil {
		return nil, err
	}
	var ev WebhookEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, fmt.Errorf("linear: 解析 webhook 载荷失败: %w", err)
	}
	return &ev, nil
}

// IsAssignedTo 判断该事件是否表示 issue 被指派给了 userID。
//
// D2「指派给我即自动接单」的判定：只在指派发生变化时触发，
// 否则 issue 的任何一次编辑都会重复接单。
func (e *WebhookEvent) IsAssignedTo(userID string) bool {
	if e == nil || e.Type != "Issue" || userID == "" {
		return false
	}

	assignee := e.Data.AssigneeID
	if assignee == "" && e.Data.Assignee != nil {
		assignee = e.Data.Assignee.ID
	}
	if assignee != userID {
		return false
	}

	switch e.Action {
	case "create":
		return true
	case "update":
		// updatedFrom 里出现 assigneeId 才说明这次改的是指派人
		if e.UpdatedFrom == nil {
			return false
		}
		_, changed := e.UpdatedFrom["assigneeId"]
		return changed
	default:
		return false
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(已截断)"
}
