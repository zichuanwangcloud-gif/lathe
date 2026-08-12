// Package github 负责推分支与开 PR。
//
// 硬约束（docs/02-design.md §1 产品边界）：Lathe 只开 PR，
// 永不合并、永不推送受保护分支。
package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	gh "github.com/google/go-github/v66/github"
)

// Client 封装 GitHub API 访问。
type Client struct {
	api *gh.Client
}

// NewClient 用 token 构造客户端。
func NewClient(token string) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("github: 缺少访问令牌")
	}
	httpc := &http.Client{Timeout: 30 * time.Second}
	return &Client{api: gh.NewClient(httpc).WithAuthToken(token)}, nil
}

// NewClientWithBaseURL 供测试注入桩服务地址。
//
// 直接设置 BaseURL 而非用 WithEnterpriseURLs：后者会给所有路径加上
// /api/v3 前缀，与 httptest 桩注册的路径对不上。
func NewClientWithBaseURL(token, baseURL string) (*Client, error) {
	c, err := NewClient(token)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("github: 解析 base URL 失败: %w", err)
	}
	c.api.BaseURL = u
	return c, nil
}

// SplitRepo 把 owner/name 拆开。
func SplitRepo(providerRepo string) (owner, name string, err error) {
	parts := strings.Split(strings.TrimSpace(providerRepo), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("github: 仓库标识 %q 格式应为 owner/name", providerRepo)
	}
	return parts[0], parts[1], nil
}

// PRParams 描述要创建的 PR。
type PRParams struct {
	ProviderRepo string // owner/name
	Head         string // 任务分支
	Base         string // 目标分支（dev / main）
	Title        string
	Body         string
	// Draft 为真时开草稿 PR。
	Draft bool
}

// PullRequest 是已创建或已存在的 PR。
type PullRequest struct {
	Number int
	URL    string
	State  string
	// Existing 为真表示这是复用的既有 PR，而非本次新建。
	Existing bool
}

// ErrNoCommits 表示 head 与 base 无差异，没东西可开 PR。
var ErrNoCommits = errors.New("github: head 与 base 无差异，无可提交的改动")

// CreatePR 开 PR；若该 head→base 的 PR 已存在则复用。
//
// 复用而非报错的理由：任务重试或租约重派时会走到这一步，
// 此时既有 PR 是正确结果，不该让整个任务失败。
func (c *Client) CreatePR(ctx context.Context, p PRParams) (*PullRequest, error) {
	owner, name, err := SplitRepo(p.ProviderRepo)
	if err != nil {
		return nil, err
	}
	if p.Head == "" || p.Base == "" {
		return nil, fmt.Errorf("github: head 与 base 均不能为空")
	}
	if p.Head == p.Base {
		return nil, fmt.Errorf("github: head 与 base 不能相同（%s）", p.Head)
	}
	if strings.TrimSpace(p.Title) == "" {
		return nil, fmt.Errorf("github: PR 标题不能为空")
	}

	// 先查是否已有同 head→base 的开放 PR
	if existing, err := c.findOpenPR(ctx, owner, name, p.Head, p.Base); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	created, resp, err := c.api.PullRequests.Create(ctx, owner, name, &gh.NewPullRequest{
		Title: gh.String(p.Title),
		Head:  gh.String(p.Head),
		Base:  gh.String(p.Base),
		Body:  gh.String(p.Body),
		Draft: gh.Bool(p.Draft),
	})
	if err != nil {
		// GitHub 用 422 表达「无差异」和「PR 已存在」两种情况
		if resp != nil && resp.StatusCode == http.StatusUnprocessableEntity {
			if strings.Contains(err.Error(), "No commits between") {
				return nil, fmt.Errorf("%w（%s → %s）", ErrNoCommits, p.Head, p.Base)
			}
			// 并发下可能刚好被别处创建，再查一次
			if existing, ferr := c.findOpenPR(ctx, owner, name, p.Head, p.Base); ferr == nil && existing != nil {
				return existing, nil
			}
		}
		return nil, fmt.Errorf("github: 创建 PR 失败(%s → %s): %w", p.Head, p.Base, err)
	}

	return &PullRequest{
		Number: created.GetNumber(),
		URL:    created.GetHTMLURL(),
		State:  created.GetState(),
	}, nil
}

func (c *Client) findOpenPR(ctx context.Context, owner, name, head, base string) (*PullRequest, error) {
	list, _, err := c.api.PullRequests.List(ctx, owner, name, &gh.PullRequestListOptions{
		State:       "open",
		Head:        owner + ":" + head,
		Base:        base,
		ListOptions: gh.ListOptions{PerPage: 10},
	})
	if err != nil {
		return nil, fmt.Errorf("github: 查询既有 PR 失败: %w", err)
	}
	for _, pr := range list {
		if pr.GetHead().GetRef() == head {
			return &PullRequest{
				Number:   pr.GetNumber(),
				URL:      pr.GetHTMLURL(),
				State:    pr.GetState(),
				Existing: true,
			}, nil
		}
	}
	return nil, nil
}

// GetPR 读取 PR 当前状态，用于判断是否已合并。
func (c *Client) GetPR(ctx context.Context, providerRepo string, number int) (*gh.PullRequest, error) {
	owner, name, err := SplitRepo(providerRepo)
	if err != nil {
		return nil, err
	}
	pr, _, err := c.api.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		return nil, fmt.Errorf("github: 读取 PR #%d 失败: %w", number, err)
	}
	return pr, nil
}

// ReviewComment 是一条需要 agent 处理的 review 意见。
type ReviewComment struct {
	ID     int64
	Author string
	Body   string
	Path   string
	Line   int
}

// ReviewComments 拉取 PR 的 review 意见（含行内评论与普通评论）。
//
// 用于 review_feedback 状态：把意见汇总后交给 agent 走二轮。
func (c *Client) ReviewComments(ctx context.Context, providerRepo string, number int) ([]ReviewComment, error) {
	owner, name, err := SplitRepo(providerRepo)
	if err != nil {
		return nil, err
	}

	var out []ReviewComment

	inline, _, err := c.api.PullRequests.ListComments(ctx, owner, name, number,
		&gh.PullRequestListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}})
	if err != nil {
		return nil, fmt.Errorf("github: 读取 PR #%d 行内评论失败: %w", number, err)
	}
	for _, cm := range inline {
		out = append(out, ReviewComment{
			ID:     cm.GetID(),
			Author: cm.GetUser().GetLogin(),
			Body:   cm.GetBody(),
			Path:   cm.GetPath(),
			Line:   cm.GetLine(),
		})
	}

	issueComments, _, err := c.api.Issues.ListComments(ctx, owner, name, number,
		&gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}})
	if err != nil {
		return nil, fmt.Errorf("github: 读取 PR #%d 评论失败: %w", number, err)
	}
	for _, cm := range issueComments {
		out = append(out, ReviewComment{
			ID:     cm.GetID(),
			Author: cm.GetUser().GetLogin(),
			Body:   cm.GetBody(),
		})
	}
	return out, nil
}

// BuildPRBody 生成带验证证据的 PR 正文。
//
// 验证证据必须进 PR 正文：审阅者第一眼要能看到这份改动被验证过什么，
// 这是本产品与「agent 随手产出的 diff」的根本区别。
func BuildPRBody(issueKey, issueURL, verifySummary, agentSummary string) string {
	var b strings.Builder

	if issueKey != "" {
		if issueURL != "" {
			fmt.Fprintf(&b, "关联 issue: [%s](%s)\n\n", issueKey, issueURL)
		} else {
			fmt.Fprintf(&b, "关联 issue: %s\n\n", issueKey)
		}
	}
	if agentSummary != "" {
		fmt.Fprintf(&b, "## 改动说明\n\n%s\n\n", strings.TrimSpace(agentSummary))
	}
	if verifySummary != "" {
		fmt.Fprintf(&b, "## 验证结果\n\n```\n%s```\n\n", verifySummary)
	}
	b.WriteString("---\n由 [Lathe](https://github.com/zichuanwangcloud-gif/lathe) 自动生成。**合并前请人工复核。**\n")
	return b.String()
}
