// Package yunxiao 接入阿里云云效（Projex）工作项，作为 Lathe 的第二个
// 外部需求平台（docs/features/tracker-yunxiao.md）。
//
// 鉴权只做个人访问令牌（请求头 x-yunxiao-token）。接入点分两种形态：
// 中心版统一 https://openapi-rdc.aliyuncs.com，路径带
// organizations/{organizationId}；Region 版用实例域名，路径不带组织段。
//
// 安全立场（§3.7）：
//   - 出站只能到允许列表里的主机，拨号期再校验实际 IP（guard.go）；
//   - 错误串只有「分类人话 + HTTP 状态码」，永不回显上游响应体、永不含令牌。
package yunxiao

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// DefaultHost 是中心版 OpenAPI 的主机名，永远在允许列表里。
	DefaultHost = "openapi-rdc.aliyuncs.com"
	// DefaultEndpoint 是中心版接入点。
	DefaultEndpoint = "https://" + DefaultHost
	// HeaderToken 是个人访问令牌的请求头。
	HeaderToken = "x-yunxiao-token"

	requestTimeout  = 30 * time.Second
	maxResponseBody = 8 << 20 // 8MB
	maxTokenLen     = 512
)

// orgIDPattern 是组织 ID 的合法形态（空 = Region 版）。
var orgIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Config 是构造客户端所需的连接配置。
type Config struct {
	// Endpoint 形如 https://openapi-rdc.aliyuncs.com（不带路径）。
	Endpoint string
	// OrganizationID 非空走中心版路径；空 = Region 版。
	OrganizationID string
	// Token 是个人访问令牌。
	Token string
	// AllowedHosts 是管理员登记的 Region 版实例域名（系统设置
	// yunxiao_allowed_hosts），中心版主机无需登记。
	AllowedHosts []string
}

// Client 是云效 OpenAPI 的最小客户端（只覆盖 Lathe 用到的 6 个接口）。
type Client struct {
	base  string // scheme://host[:port]，不带尾 /
	org   string
	token string
	hc    *http.Client
}

// NewClient 构造生产客户端：校验接入点与组织 ID（防线放在使用点），
// 传输层是出站守卫（guard.go）。
func NewClient(cfg Config) (*Client, error) {
	base, err := ValidateEndpoint(cfg.Endpoint, cfg.AllowedHosts)
	if err != nil {
		return nil, err
	}
	return newClient(base, cfg, newHTTPClient(guardedTransport(nil)))
}

// NewClientWithHTTP 供测试注入 httptest 地址与 http.Client。
//
// allowInsecure 为真时跳过接入点允许列表校验（httptest 是 http://127.0.0.1）；
// 生产代码只调 NewClient，这条路径不可达。无论如何都会强制「不跟随重定向」
// 与 30s 超时。
func NewClientWithHTTP(cfg Config, hc *http.Client, allowInsecure bool) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	if !allowInsecure {
		var err error
		if base, err = ValidateEndpoint(cfg.Endpoint, cfg.AllowedHosts); err != nil {
			return nil, err
		}
	}
	if hc == nil {
		hc = &http.Client{}
	}
	clone := *hc
	clone.CheckRedirect = refuseRedirect
	if clone.Timeout == 0 {
		clone.Timeout = requestTimeout
	}
	return newClient(base, cfg, &clone)
}

func newClient(base string, cfg Config, hc *http.Client) (*Client, error) {
	org := strings.TrimSpace(cfg.OrganizationID)
	if org != "" && !orgIDPattern.MatchString(org) {
		return nil, errors.New("云效组织 ID 格式不合法（只允许字母、数字、_、-，至多 64 位）")
	}
	token := strings.TrimSpace(cfg.Token)
	if token == "" {
		return nil, errors.New("云效令牌未配置")
	}
	if len(token) > maxTokenLen {
		return nil, errors.New("云效令牌过长")
	}
	return &Client{base: base, org: org, token: token, hc: hc}, nil
}

// ---------------------------------------------------------------- 响应形状

// flexString 兼容「字段时而是字符串、时而是数字」的 JSON（文档示例里
// id、gmtModified 两种写法都出现过）。null 解成空串。
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		*f = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexString(n.String())
	return nil
}

// Status 是工作项状态（项目模板定义，不能硬编码）。
type Status struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	NameEn      string `json:"nameEn"`
}

type rawStatus struct {
	ID          flexString `json:"id"`
	Name        string     `json:"name"`
	DisplayName string     `json:"displayName"`
	NameEn      string     `json:"nameEn"`
}

func (r rawStatus) status() Status {
	return Status{ID: string(r.ID), Name: r.Name, DisplayName: r.DisplayName, NameEn: r.NameEn}
}

// Person 是用户引用（指派人、评论作者）。
type Person struct {
	ID   string
	Name string
}

type rawPerson struct {
	ID   flexString `json:"id"`
	Name string     `json:"name"`
}

// Label 是工作项标签。
type Label struct {
	ID   string
	Name string
}

type rawLabel struct {
	ID   flexString `json:"id"`
	Name string     `json:"name"`
}

// Workitem 是 GetWorkitem 的权威数据（只取 Lathe 用得到的字段）。
type Workitem struct {
	ID            string
	SerialNumber  string
	Subject       string
	Description   string // 原文（RICHTEXT 时是 HTML）
	FormatType    string // RICHTEXT / MARKDOWN
	Status        Status
	AssignedTo    Person
	Labels        []Label
	LogicalStatus string // normal / archived
	GmtModified   string
}

// Key 是任务行的 external_key：serialNumber，为空时退回 id。
func (w *Workitem) Key() string {
	if s := strings.TrimSpace(w.SerialNumber); s != "" {
		return s
	}
	return w.ID
}

type rawWorkitem struct {
	ID            flexString `json:"id"`
	SerialNumber  flexString `json:"serialNumber"`
	Subject       string     `json:"subject"`
	Description   string     `json:"description"`
	FormatType    string     `json:"formatType"`
	Status        rawStatus  `json:"status"`
	AssignedTo    rawPerson  `json:"assignedTo"`
	Labels        []rawLabel `json:"labels"`
	LogicalStatus string     `json:"logicalStatus"`
	GmtModified   flexString `json:"gmtModified"`
}

// Comment 是工作项下的一条评论；Content 已转成纯文本。
type Comment struct {
	ID            string
	Content       string
	ContentFormat string
	User          Person
}

type rawComment struct {
	ID            flexString `json:"id"`
	Content       string     `json:"content"`
	ContentFormat string     `json:"contentFormat"`
	User          rawPerson  `json:"user"`
}

// Workflow 是工作项所在的工作流（状态名 → id 的解析来源）。
type Workflow struct {
	ID       string
	Statuses []Status
}

type rawWorkflow struct {
	ID       flexString  `json:"id"`
	Statuses []rawStatus `json:"statuses"`
}

// User 是 GetUserByToken 的结果。
type User struct {
	ID       string
	Name     string
	NickName string
	Username string
	Email    string
}

type rawUser struct {
	ID       flexString `json:"id"`
	Name     string     `json:"name"`
	NickName string     `json:"nickName"`
	Username string     `json:"username"`
	Email    string     `json:"email"`
}

// ---------------------------------------------------------------- 接口

// CurrentUser 用令牌查当前用户（验令牌、取「指派给我」判定用的账号 ID）。
func (c *Client) CurrentUser(ctx context.Context) (*User, error) {
	var r rawUser
	if err := c.do(ctx, http.MethodGet, "/oapi/v1/platform/user", nil, &r); err != nil {
		return nil, err
	}
	if r.ID == "" {
		return nil, shapeError(http.StatusOK)
	}
	return &User{ID: string(r.ID), Name: r.Name, NickName: r.NickName, Username: r.Username, Email: r.Email}, nil
}

// GetWorkitem 取工作项。
func (c *Client) GetWorkitem(ctx context.Context, id string) (*Workitem, error) {
	p, err := c.workitemPath(id, "")
	if err != nil {
		return nil, err
	}
	var r rawWorkitem
	if err := c.do(ctx, http.MethodGet, p, nil, &r); err != nil {
		return nil, err
	}
	if r.ID == "" {
		return nil, shapeError(http.StatusOK)
	}
	wi := &Workitem{
		ID: string(r.ID), SerialNumber: string(r.SerialNumber), Subject: r.Subject,
		Description: r.Description, FormatType: r.FormatType, Status: r.Status.status(),
		AssignedTo:    Person{ID: string(r.AssignedTo.ID), Name: r.AssignedTo.Name},
		LogicalStatus: r.LogicalStatus, GmtModified: string(r.GmtModified),
	}
	for _, l := range r.Labels {
		wi.Labels = append(wi.Labels, Label{ID: string(l.ID), Name: l.Name})
	}
	return wi, nil
}

// ListComments 列出工作项评论（官方接口无分页）；RICHTEXT 转纯文本。
func (c *Client) ListComments(ctx context.Context, id string) ([]Comment, error) {
	p, err := c.workitemPath(id, "/comments")
	if err != nil {
		return nil, err
	}
	var rs []rawComment
	if err := c.do(ctx, http.MethodGet, p, nil, &rs); err != nil {
		return nil, err
	}
	out := make([]Comment, 0, len(rs))
	for _, r := range rs {
		out = append(out, Comment{
			ID: string(r.ID), Content: ToPlainText(r.Content, r.ContentFormat), ContentFormat: r.ContentFormat,
			User: Person{ID: string(r.User.ID), Name: r.User.Name},
		})
	}
	return out, nil
}

// CreateComment 以 MARKDOWN 格式发评论，返回评论 id。空正文本地拒绝。
func (c *Client) CreateComment(ctx context.Context, id, body string) (string, error) {
	if strings.TrimSpace(body) == "" {
		return "", errors.New("云效评论内容为空")
	}
	p, err := c.workitemPath(id, "/comments")
	if err != nil {
		return "", err
	}
	req := struct {
		Content    string `json:"content"`
		FormatType string `json:"formatType"`
	}{Content: body, FormatType: "MARKDOWN"}
	var r struct {
		ID flexString `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, p, req, &r); err != nil {
		return "", err
	}
	if r.ID == "" {
		return "", shapeError(http.StatusOK)
	}
	return string(r.ID), nil
}

// Workflow 取工作项所在工作流的状态列表。
func (c *Client) Workflow(ctx context.Context, id string) (*Workflow, error) {
	p, err := c.workitemPath(id, "/workflow")
	if err != nil {
		return nil, err
	}
	var r rawWorkflow
	if err := c.do(ctx, http.MethodGet, p, nil, &r); err != nil {
		return nil, err
	}
	wf := &Workflow{ID: string(r.ID)}
	for _, s := range r.Statuses {
		wf.Statuses = append(wf.Statuses, s.status())
	}
	return wf, nil
}

// UpdateStatus 把工作项推到 statusID。
//
// 官方文档只给出 Region 形态路径（§6 R2）；这里与其余接口一致，有组织 ID
// 时加组织段。上线条件 ④ 用真实租户确认，不符只改 workitemPath 一处。
func (c *Client) UpdateStatus(ctx context.Context, id, statusID string) error {
	p, err := c.workitemPath(id, "")
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, p, map[string]string{"status": statusID}, nil)
}

// workitemPath 拼工作项路径；路径段一律 PathEscape，id 先过白名单。
func (c *Client) workitemPath(id, suffix string) (string, error) {
	if !ValidWorkitemID(id) {
		return "", errors.New("云效工作项 id 不合法")
	}
	p := "/oapi/v1/projex"
	if c.org != "" {
		p += "/organizations/" + url.PathEscape(c.org)
	}
	return p + "/workitems/" + url.PathEscape(id) + suffix, nil
}

// ---------------------------------------------------------------- 传输与错误

// APIError 是云效请求失败的分类结果。Error() 只有分类人话与状态码。
type APIError struct {
	// Status 是 HTTP 状态码（3xx 表示被拒绝跟随的重定向）。
	Status int
	msg    string
}

func (e *APIError) Error() string { return e.msg }

func shapeError(code int) error {
	return &APIError{Status: code, msg: fmt.Sprintf("云效返回 %d，但响应形状不符合预期", code)}
}

// classify 把非 2xx 状态码转成人话。刻意不看响应体：B4 的结论是任何
// 回显都可能被用来读回内网服务的内容。
func classify(code int) error {
	var reason string
	switch {
	case code >= 300 && code < 400:
		reason = "接入点返回了重定向，已拒绝跟随"
	case code == http.StatusUnauthorized:
		reason = "令牌无效或已过期"
	case code == http.StatusForbidden:
		reason = "令牌缺少权限（需要「项目协作」相关权限）"
	case code == http.StatusNotFound:
		reason = "工作项不存在或令牌无权访问"
	case code == http.StatusTooManyRequests:
		reason = "请求被限流，请稍后再试"
	case code >= 500:
		reason = "云效服务端错误"
	default:
		reason = "非预期的响应状态"
	}
	return &APIError{Status: code, msg: fmt.Sprintf("云效返回 %d：%s", code, reason)}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("云效请求编码失败: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return errors.New("云效请求构造失败（接入点不合法）")
	}
	req.Header.Set(HeaderToken, c.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		if errors.Is(err, ErrForbiddenAddress) {
			return fmt.Errorf("云效请求被拒绝：%w", ErrForbiddenAddress)
		}
		if ctx.Err() != nil {
			return errors.New("云效请求已取消或超时")
		}
		// 底层错误串里可能有 URL / IP，不含令牌，但也没必要进错误串：
		// 只在 Debug 里记类型，排障够用。
		slog.Debug("云效请求网络错误", "method", method, "errType", fmt.Sprintf("%T", err))
		return errors.New("云效请求失败：网络不可达或超时")
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return errors.New("云效响应读取失败")
	}
	if len(data) > maxResponseBody {
		return &APIError{Status: resp.StatusCode, msg: fmt.Sprintf("云效返回 %d，但响应体超过上限", resp.StatusCode)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 响应体只记长度，不记内容（§3.7 第 4 条）。
		slog.Debug("云效返回非 2xx", "method", method, "status", resp.StatusCode, "bytes", len(data))
		return classify(resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		slog.Debug("云效响应形状不符", "method", method, "status", resp.StatusCode, "bytes", len(data))
		return shapeError(resp.StatusCode)
	}
	return nil
}
