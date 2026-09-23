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

	"github.com/zichuanwangcloud-gif/lathe/internal/tracker"
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
//
// 类型本体住在 internal/tracker（它是 pipeline 与各平台实现之间的契约），
// 这里只是别名 —— 本包内部与其它老调用方继续写 linear.Issue 即可，零改动。
// 见 internal/tracker/tracker.go 的包注释。
type Issue = tracker.Issue

// Comment 是 issue 下的一条评论。
type Comment = tracker.Comment

// IssueSummary 是列表用的轻量视图 —— 不含描述与评论，
// 那是详情（Issue）才需要拉的东西。
type IssueSummary struct {
	ID         string   `json:"id"`
	Identifier string   `json:"identifier"`
	Title      string   `json:"title"`
	URL        string   `json:"url"`
	StateName  string   `json:"state"`
	Priority   int      `json:"priority"`
	Labels     []string `json:"labels"`
	UpdatedAt  string   `json:"updatedAt"`
}

const assignedIssuesQuery = `query($first: Int!) {
  viewer {
    assignedIssues(
      first: $first
      filter: { state: { type: { nin: ["completed", "canceled"] } } }
      orderBy: updatedAt
    ) {
      nodes {
        id identifier title url priority updatedAt
        state { name }
        labels { nodes { name } }
      }
    }
  }
}`

// AssignedIssues 拉取「指派给我、尚未完结」的 issue，按最近更新排序。
//
// 看板的「同步 Linear」用它。只取未完成的是刻意为之：已完成/已取消的
// 单子同步下来没有任何可做的动作，只会把列表淹掉。
func (c *Client) AssignedIssues(ctx context.Context, first int) ([]IssueSummary, error) {
	if first <= 0 || first > 100 {
		first = 50
	}

	var resp struct {
		Viewer *struct {
			AssignedIssues struct {
				Nodes []struct {
					ID         string  `json:"id"`
					Identifier string  `json:"identifier"`
					Title      string  `json:"title"`
					URL        string  `json:"url"`
					Priority   float64 `json:"priority"`
					UpdatedAt  string  `json:"updatedAt"`
					State      *struct {
						Name string `json:"name"`
					} `json:"state"`
					Labels struct {
						Nodes []struct {
							Name string `json:"name"`
						} `json:"nodes"`
					} `json:"labels"`
				} `json:"nodes"`
			} `json:"assignedIssues"`
		} `json:"viewer"`
	}

	if err := c.do(ctx, assignedIssuesQuery, map[string]any{"first": first}, &resp); err != nil {
		return nil, classifyVerifyError(err)
	}
	if resp.Viewer == nil {
		return nil, fmt.Errorf("linear: 令牌有效但未取到账号信息")
	}

	out := make([]IssueSummary, 0, len(resp.Viewer.AssignedIssues.Nodes))
	for _, n := range resp.Viewer.AssignedIssues.Nodes {
		s := IssueSummary{
			ID: n.ID, Identifier: n.Identifier, Title: n.Title,
			URL: n.URL, Priority: int(n.Priority), UpdatedAt: n.UpdatedAt,
		}
		if n.State != nil {
			s.StateName = n.State.Name
		}
		for _, l := range n.Labels.Nodes {
			s.Labels = append(s.Labels, l.Name)
		}
		out = append(out, s)
	}
	return out, nil
}

// Issue.Context() 已随类型本体迁往 internal/tracker —— 分诊上下文的
// 拼装格式（标题/描述/评论）必须全平台只有一份，见 tracker.Issue.Context。

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
		// LabelIDs / Labels 是 T7 标签驱动接单需要的字段。
		// Linear 的 issue webhook 同时给这两种形态（前者只有 id，
		// 后者带 name），按 name 判定更符合人的直觉 ——
		// 人在界面上打的是「lathe:go」这个名字，不是一个 UUID。
		LabelIDs []string `json:"labelIds"`
		Labels   []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"labels"`
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

// IsLabelTriggered 判断该事件是否表示 issue 被打上了触发标签（T7）。
//
// label 为空时**恒返回 false** —— 这是「未配置时行为与现状一致」的实现：
// 不能让存量部署因为升级就突然开始按标签接单，尤其是某个仓库可能早就
// 在用 lathe:go 这个标签表示别的意思。
//
// 判定套路与 IsAssignedTo 对齐：create 直接算，update 必须看
// updatedFrom 里有没有 labelIds —— 否则 issue 的任何一次编辑
// （改标题、改描述）都会因为标签还在而重复接单。
func (e *WebhookEvent) IsLabelTriggered(label string) bool {
	if e == nil || e.Type != "Issue" || label == "" {
		return false
	}
	if !e.hasLabel(label) {
		return false
	}
	switch e.Action {
	case "create":
		return true
	case "update":
		if e.UpdatedFrom == nil {
			return false
		}
		_, changed := e.UpdatedFrom["labelIds"]
		return changed
	default:
		return false
	}
}

// hasLabel 报告 issue 当前是否带这个标签（按名字，大小写不敏感）。
func (e *WebhookEvent) hasLabel(label string) bool {
	want := strings.ToLower(strings.TrimSpace(label))
	// trim 之后为空则一律不匹配。IsLabelTriggered 判的是 `label == ""`，
	// 纯空白的 " " 能穿过那道判断，到这里 want 变成空串，于是任何**空名**
	// 标签都会命中 —— 等于在没配置的部署上悄悄打开自动接单。
	//
	// 生产上够不到（唯一入口 settings.WebhookTriggerLabel 与写入侧 admin
	// 各自 TrimSpace 过），但「关闭」这条语义不该寄托在两个上游都记得
	// trim；判据放在使用点才是真防线。
	if want == "" {
		return false
	}
	for _, l := range e.Data.Labels {
		if strings.ToLower(strings.TrimSpace(l.Name)) == want {
			return true
		}
	}
	return false
}

// IsCancelled 判断该事件是否表示 issue 被取消（T7 取消联动）。
//
// 判据是 Linear 的工作流状态类型 canceled，而不是状态名 ——
// 状态名是每个团队自己起的（「Cancelled」「废弃」「不做了」），
// 类型才是 Linear 的稳定枚举
// （triage/backlog/unstarted/started/completed/canceled）。
//
// 刻意**不把 completed 也算作取消**：issue 被人手工标记完成，
// 不代表平台这边的任务该作废 —— 那个任务可能正在跑，或者已经开出了 PR
// 等人合并。「取消」是明确的作废意图，「完成」不是。
//
// remove（issue 被删）也算：目标都不存在了，在途任务没有意义。
func (e *WebhookEvent) IsCancelled() bool {
	if e == nil || e.Type != "Issue" {
		return false
	}
	if e.Action == "remove" {
		return true
	}
	if e.Action != "update" || e.Data.State == nil {
		return false
	}
	if strings.ToLower(e.Data.State.Type) != "canceled" {
		return false
	}
	// 与 IsAssignedTo 同理：必须是【这次】改的状态，否则 issue 停在
	// 取消态时的任何一次编辑都会重复触发取消。
	if e.UpdatedFrom == nil {
		return false
	}
	_, changed := e.UpdatedFrom["stateId"]
	return changed
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(已截断)"
}
