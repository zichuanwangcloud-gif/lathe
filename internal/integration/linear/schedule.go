package linear

// 定时建单（docs/features/scheduled-issue.md）需要的 Linear 写入与查询。
//
// 刻意放在新文件、只挂在具体类型 *Client 上：tracker.Tracker 窄接口
// 「没有真实消费方之前不加方法」，调度器经它自己声明的窄接口消费这些方法。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// CreateIssueInput 是 issueCreate 的输入。
//
// ID 是调用方预生成的 UUID v4（Linear 允许客户端指定 issue id）：
// 同一个计划时刻无论重试几次都用同一个 id，是「只建一张单」在 Linear 侧的钥匙。
// AssigneeID 与 LabelIDs 为空时不出现在请求里。
type CreateIssueInput struct {
	ID          string
	TeamID      string
	Title       string
	Description string
	Priority    int
	AssigneeID  string
	LabelIDs    []string
}

// CreatedIssue 是建单/查单返回的三个字段。
type CreatedIssue struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	URL        string `json:"url"`
}

// Team 是 Linear 团队（定时规则选团队用）。
type Team struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
}

const issueCreateMutation = `mutation($input: IssueCreateInput!) {
  issueCreate(input: $input) { success issue { id identifier url } }
}`

// CreateIssue 在 Linear 上建一张单。
//
// 错误里不带请求体：请求体是用户的模板内容，没有必要出现在 run 的错误记录里。
func (c *Client) CreateIssue(ctx context.Context, in CreateIssueInput) (*CreatedIssue, error) {
	if strings.TrimSpace(in.TeamID) == "" {
		return nil, fmt.Errorf("linear: 建单缺少团队")
	}
	if strings.TrimSpace(in.Title) == "" {
		return nil, fmt.Errorf("linear: 建单缺少标题")
	}
	input := map[string]any{
		"id":          in.ID,
		"teamId":      in.TeamID,
		"title":       in.Title,
		"description": in.Description,
		"priority":    in.Priority,
	}
	if in.AssigneeID != "" {
		input["assigneeId"] = in.AssigneeID
	}
	if len(in.LabelIDs) > 0 {
		input["labelIds"] = in.LabelIDs
	}

	var resp struct {
		IssueCreate struct {
			Success bool          `json:"success"`
			Issue   *CreatedIssue `json:"issue"`
		} `json:"issueCreate"`
	}
	if err := c.do(ctx, issueCreateMutation, map[string]any{"input": input}, &resp); err != nil {
		return nil, err
	}
	if !resp.IssueCreate.Success || resp.IssueCreate.Issue == nil {
		return nil, fmt.Errorf("linear: 建单失败（服务端返回 success=false）")
	}
	return resp.IssueCreate.Issue, nil
}

const teamsQuery = `query { teams(first: 100) { nodes { id key name } } }`

// Teams 列出当前令牌可见的团队。
func (c *Client) Teams(ctx context.Context) ([]Team, error) {
	var resp struct {
		Teams struct {
			Nodes []Team `json:"nodes"`
		} `json:"teams"`
	}
	if err := c.do(ctx, teamsQuery, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Teams.Nodes, nil
}

const labelsByNameQuery = `query($name: String!) {
  issueLabels(first: 50, filter: { name: { eqIgnoreCase: $name } }) { nodes { id name team { id } } }
}`

// LabelIDByName 按名字找标签 id：优先本团队的标签，其次工作区标签（team 为 null）。
// 找不到返回错误。
func (c *Client) LabelIDByName(ctx context.Context, teamID, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("linear: 标签名为空")
	}
	var resp struct {
		IssueLabels struct {
			Nodes []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Team *struct {
					ID string `json:"id"`
				} `json:"team"`
			} `json:"nodes"`
		} `json:"issueLabels"`
	}
	if err := c.do(ctx, labelsByNameQuery, map[string]any{"name": name}, &resp); err != nil {
		return "", err
	}
	workspace := ""
	for _, n := range resp.IssueLabels.Nodes {
		if n.Team != nil && n.Team.ID == teamID {
			return n.ID, nil
		}
		if n.Team == nil && workspace == "" {
			workspace = n.ID
		}
	}
	if workspace != "" {
		return workspace, nil
	}
	return "", fmt.Errorf("linear: 团队与工作区里都找不到标签 %q", name)
}

const issueByIDQuery = `query($id: String!) { issue(id: $id) { id identifier url } }`

// gqlErrorDetail 是一条 GraphQL 错误的完整形状（do() 的 graphQLError 只解 message）。
type gqlErrorDetail struct {
	Message    string `json:"message"`
	Path       []any  `json:"path"`
	Extensions struct {
		Code                   string `json:"code"`
		Type                   string `json:"type"`
		UserPresentableMessage string `json:"userPresentableMessage"`
	} `json:"extensions"`
}

// isNotFound 报告这条错误是否属于「确认不存在」的白名单形状。
func (e gqlErrorDetail) isNotFound() bool {
	if e.Extensions.Code == "ENTITY_NOT_FOUND" {
		return true
	}
	return e.Extensions.Type == "invalid input" && strings.HasPrefix(e.Message, "Entity not found")
}

// IssueByID 按 UUID 查单：查到 → (issue, nil)；确认不存在 → (nil, nil)；
// 其余一切（HTTP 非 200、网络错误、白名单外的 GraphQL 错误）→ (nil, err)。
//
// 白名单而不是黑名单：把「临时故障/无权限」误判成不存在，调度器就可能重放建单；
// 把「不存在」误判成错误只会让那次记 interrupted、界面可见。所以只放过能确认的形状。
func (c *Client) IssueByID(ctx context.Context, id string) (*CreatedIssue, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("linear: issue id 为空")
	}
	data, errs, err := c.doEnvelope(ctx, issueByIDQuery, map[string]any{"id": id})
	if err != nil {
		return nil, err
	}

	var resp struct {
		Issue *CreatedIssue `json:"issue"`
	}
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &resp); err != nil {
			return nil, fmt.Errorf("linear: 解析 data 失败: %w", err)
		}
	}
	if resp.Issue != nil && len(errs) == 0 {
		return resp.Issue, nil
	}
	if resp.Issue == nil {
		allNotFound := true
		for _, e := range errs {
			if !e.isNotFound() {
				allNotFound = false
				break
			}
		}
		if allNotFound {
			return nil, nil
		}
	}
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		code := e.Extensions.Code
		if code == "" {
			code = e.Extensions.Type
		}
		if code == "" {
			code = "UNKNOWN"
		}
		msgs = append(msgs, code+": "+e.Message)
	}
	return nil, fmt.Errorf("linear: 按 UUID 查单的 GraphQL 错误: %s", truncate(strings.Join(msgs, "; "), 500))
}

// doEnvelope 发一次 GraphQL 请求，200 时把 data 与 errors[] 原样交回。
//
// 复制了 do() 的请求代码而不是改 do()：do() 把所有 GraphQL 错误合并成无类型
// 字符串，调用方分不清「不存在」与「无权限/临时故障」；改它会动到邻居也在读的
// 既有函数，重复约 25 行的代价更小（与 prd/uuid.go 的取舍同理）。
func (c *Client) doEnvelope(ctx context.Context, query string, vars map[string]any) (json.RawMessage, []gqlErrorDetail, error) {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return nil, nil, fmt.Errorf("linear: 序列化请求失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, fmt.Errorf("linear: 构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("linear: 请求失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("linear: 读取响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("linear: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}
	var envelope struct {
		Data   json.RawMessage  `json:"data"`
		Errors []gqlErrorDetail `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, nil, fmt.Errorf("linear: 解析响应失败: %w", err)
	}
	return envelope.Data, envelope.Errors, nil
}
