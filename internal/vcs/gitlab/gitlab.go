// Package gitlab 是代码平台契约（internal/vcs）的 GitLab 实现（含自建实例）：
// 开 MR、查合并状态、拉 review 意见、保存凭据时验证、Note Hook 验签与信任判定。
//
// 只用标准库。MR 编号一律用 iid（项目内序号，与 UI 显示一致），不是全局 id。
// 信任判定（谁的评论能驱动二轮）是安全核心：GitLab 没有 author_association，
// 用项目成员查询等价替换，且 fail-closed（查不到、查询出错一律拒绝）。
package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/zichuanwangcloud-gif/lathe/internal/vcs"
	"github.com/zichuanwangcloud-gif/lathe/internal/vcs/internal/apiclient"
)

// 编译期断言：满足代码平台契约。
var (
	_ vcs.Client               = (*Client)(nil)
	_ vcs.ReviewFeedbackSource = (*Client)(nil)
)

// ReporterAccess 是能驱动二轮的最低项目权限（Reporter = 20）。
const ReporterAccess = 20

// maxFeedbackPages 与 GitHub 实现同口径：每类意见最多翻 10 页（每页 100 条）。
const maxFeedbackPages = 10

// Client 是 GitLab REST v4 客户端。
type Client struct {
	api *apiclient.Client
}

// New 用实例地址与个人访问令牌构造客户端。
func New(baseURL, token string) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("gitlab: 缺少访问令牌")
	}
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("gitlab: 缺少实例地址")
	}
	return &Client{api: apiclient.New("gitlab", baseURL, "PRIVATE-TOKEN", token, token)}, nil
}

// project 返回 /api/v4/projects/<url-encoded path>。
func project(repo string) (string, error) {
	if _, _, err := vcs.SplitRef(vcs.ProviderGitLab, repo); err != nil {
		return "", err
	}
	return "/api/v4/projects/" + url.PathEscape(repo), nil
}

type mr struct {
	ID           int64  `json:"id"`
	IID          int    `json:"iid"`
	State        string `json:"state"`
	WebURL       string `json:"web_url"`
	SourceBranch string `json:"source_branch"`
	TargetBranch string `json:"target_branch"`
	SHA          string `json:"sha"`
	DiffRefs     *struct {
		HeadSHA string `json:"head_sha"`
	} `json:"diff_refs"`
	Author struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	} `json:"author"`
}

// CreatePR 开 MR；同 source→target 的开放 MR 已存在则复用（与 GitHub 实现同一理由：
// 重试/租约重派时既有 MR 是正确结果）。
func (c *Client) CreatePR(ctx context.Context, p vcs.PRParams) (*vcs.PullRequest, error) {
	proj, err := project(p.ProviderRepo)
	if err != nil {
		return nil, err
	}
	if p.Head == "" || p.Base == "" {
		return nil, fmt.Errorf("gitlab: source 与 target 分支均不能为空")
	}
	if p.Head == p.Base {
		return nil, fmt.Errorf("gitlab: source 与 target 不能相同（%s）", p.Head)
	}
	if strings.TrimSpace(p.Title) == "" {
		return nil, fmt.Errorf("gitlab: MR 标题不能为空")
	}

	if existing, err := c.findOpen(ctx, proj, p.Head, p.Base); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	title := p.Title
	if p.Draft {
		title = "Draft: " + title
	}
	var created mr
	_, err = c.api.Do(ctx, http.MethodPost, proj+"/merge_requests", map[string]any{
		"source_branch": p.Head,
		"target_branch": p.Base,
		"title":         title,
		"description":   p.Body,
	}, &created)
	if err != nil {
		var se *apiclient.StatusError
		if errors.As(err, &se) && (se.Code == http.StatusBadRequest || se.Code == http.StatusUnprocessableEntity || se.Code == http.StatusConflict) {
			if strings.Contains(strings.ToLower(se.Body), "without any commits") {
				return nil, fmt.Errorf("%w（%s → %s）", vcs.ErrNoCommits, p.Head, p.Base)
			}
			// 并发下可能刚被别处创建，再查一次
			if existing, ferr := c.findOpen(ctx, proj, p.Head, p.Base); ferr == nil && existing != nil {
				return existing, nil
			}
		}
		return nil, fmt.Errorf("gitlab: 创建 MR 失败(%s → %s): %w", p.Head, p.Base, err)
	}
	return &vcs.PullRequest{Number: created.IID, URL: created.WebURL, State: mapState(created.State)}, nil
}

func (c *Client) findOpen(ctx context.Context, proj, head, base string) (*vcs.PullRequest, error) {
	q := url.Values{"state": {"opened"}, "source_branch": {head}, "target_branch": {base}, "per_page": {"10"}}
	var list []mr
	if _, err := c.api.Do(ctx, http.MethodGet, proj+"/merge_requests?"+q.Encode(), nil, &list); err != nil {
		return nil, fmt.Errorf("gitlab: 查询既有 MR 失败: %w", err)
	}
	for _, m := range list {
		if m.SourceBranch == head || m.SourceBranch == "" {
			return &vcs.PullRequest{Number: m.IID, URL: m.WebURL, State: mapState(m.State), Existing: true}, nil
		}
	}
	return nil, nil
}

// mapState 把 GitLab 的 MR 状态映射到统一的 open/closed（§3.6）。
func mapState(s string) string {
	switch s {
	case "closed", "merged":
		return "closed"
	default: // opened / locked
		return "open"
	}
}

// GetPRInfo 读取 MR 当前状态。
func (c *Client) GetPRInfo(ctx context.Context, repoRef string, number int) (*vcs.PRInfo, error) {
	proj, err := project(repoRef)
	if err != nil {
		return nil, err
	}
	var m mr
	if _, err := c.api.Do(ctx, http.MethodGet, fmt.Sprintf("%s/merge_requests/%d", proj, number), nil, &m); err != nil {
		return nil, fmt.Errorf("gitlab: 读取 MR !%d 失败: %w", number, err)
	}
	head := m.SHA
	if head == "" && m.DiffRefs != nil {
		head = m.DiffRefs.HeadSHA
	}
	return &vcs.PRInfo{
		Number:  m.IID,
		Merged:  m.State == "merged",
		State:   mapState(m.State),
		BaseRef: m.TargetBranch,
		HeadSHA: head,
	}, nil
}

type noteJSON struct {
	ID       int64  `json:"id"`
	Body     string `json:"body"`
	System   bool   `json:"system"`
	Type     string `json:"type"`
	Author   author `json:"author"`
	Position *struct {
		NewPath string `json:"new_path"`
		NewLine *int   `json:"new_line"`
		OldPath string `json:"old_path"`
		OldLine *int   `json:"old_line"`
	} `json:"position"`
}

type author struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

// ReviewFeedback 拉取 MR 上的全部意见：普通评论（notes）与行内评论（discussions），
// 按 Key 去重；system 笔记跳过；作者逐一走成员查询（≥ Reporter 才收，fail-closed）。
func (c *Client) ReviewFeedback(ctx context.Context, repoRef string, number int) (*vcs.ReviewFeedback, error) {
	proj, err := project(repoRef)
	if err != nil {
		return nil, err
	}
	var m mr
	mrPath := fmt.Sprintf("%s/merge_requests/%d", proj, number)
	if _, err := c.api.Do(ctx, http.MethodGet, mrPath, nil, &m); err != nil {
		return nil, fmt.Errorf("gitlab: 读取 MR !%d 失败: %w", number, err)
	}
	fb := &vcs.ReviewFeedback{PRAuthor: m.Author.Username}

	var all []noteJSON
	if err := c.paged(ctx, mrPath+"/notes", func(page int) (int, error) {
		var list []noteJSON
		resp, err := c.api.Do(ctx, http.MethodGet, fmt.Sprintf("%s/notes?per_page=100&page=%d&sort=asc", mrPath, page), nil, &list)
		if err != nil {
			return 0, err
		}
		all = append(all, list...)
		return nextPage(resp), nil
	}); err != nil {
		return nil, fmt.Errorf("gitlab: 读取 MR !%d 评论失败: %w", number, err)
	}
	if err := c.paged(ctx, mrPath+"/discussions", func(page int) (int, error) {
		var list []struct {
			Notes []noteJSON `json:"notes"`
		}
		resp, err := c.api.Do(ctx, http.MethodGet, fmt.Sprintf("%s/discussions?per_page=100&page=%d", mrPath, page), nil, &list)
		if err != nil {
			return 0, err
		}
		for _, d := range list {
			all = append(all, d.Notes...)
		}
		return nextPage(resp), nil
	}); err != nil {
		return nil, fmt.Errorf("gitlab: 读取 MR !%d 讨论失败: %w", number, err)
	}

	seen := map[string]int{}
	trusted := map[int64]bool{}
	for _, n := range all {
		if n.System || strings.TrimSpace(n.Body) == "" {
			continue
		}
		key := NoteKey(n.ID)
		path, line := notePosition(n)
		if i, dup := seen[key]; dup {
			// notes 与 discussions 会返回同一条笔记：保留带位置的那份
			if fb.Comments[i].Path == "" && path != "" {
				fb.Comments[i].Path, fb.Comments[i].Line = path, line
			}
			continue
		}
		ok, cached := trusted[n.Author.ID]
		if !cached {
			lvl, err := c.MemberAccessLevel(ctx, repoRef, n.Author.ID)
			ok = err == nil && lvl >= ReporterAccess
			trusted[n.Author.ID] = ok
		}
		if !ok {
			continue
		}
		seen[key] = len(fb.Comments)
		fb.Comments = append(fb.Comments, vcs.ReviewComment{
			ID: n.ID, Key: key, Author: n.Author.Username, Body: n.Body, Path: path, Line: line,
			URL: fmt.Sprintf("%s#note_%d", m.WebURL, n.ID),
		})
	}
	return fb, nil
}

func notePosition(n noteJSON) (string, int) {
	if n.Position == nil {
		return "", 0
	}
	path, line := n.Position.NewPath, 0
	if n.Position.NewLine != nil {
		line = *n.Position.NewLine
	} else if n.Position.OldLine != nil {
		line = *n.Position.OldLine
	}
	if path == "" {
		path = n.Position.OldPath
	}
	return path, line
}

// paged 按 X-Next-Page 翻页，最多 maxFeedbackPages 页。
func (c *Client) paged(ctx context.Context, _ string, fetch func(page int) (next int, err error)) error {
	for page, n := 1, 0; page != 0 && n < maxFeedbackPages; n++ {
		next, err := fetch(page)
		if err != nil {
			return err
		}
		page = next
	}
	return nil
}

func nextPage(resp *apiclient.Response) int {
	if resp == nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("X-Next-Page")))
	if err != nil {
		return 0
	}
	return n
}

// NoteKey 是 GitLab 意见的稳定标识。
func NoteKey(id int64) string { return fmt.Sprintf("gitlab:note:%d", id) }

// MemberAccessLevel 查用户在项目上的有效权限（含继承自群组的，members/all）。
// 404/403/超时都返回错误 —— 调用方按「不可信」处理（fail-closed）。
func (c *Client) MemberAccessLevel(ctx context.Context, repoRef string, userID int64) (int, error) {
	proj, err := project(repoRef)
	if err != nil {
		return 0, err
	}
	var mem struct {
		AccessLevel int `json:"access_level"`
	}
	if _, err := c.api.Do(ctx, http.MethodGet, fmt.Sprintf("%s/members/all/%d", proj, userID), nil, &mem); err != nil {
		return 0, err
	}
	return mem.AccessLevel, nil
}

// Account 是令牌对应的 GitLab 账号。
type Account struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

// Verify 校验令牌可用（GET /api/v4/user）。
func (c *Client) Verify(ctx context.Context) (*Account, error) {
	var a Account
	if _, err := c.api.Do(ctx, http.MethodGet, "/api/v4/user", nil, &a); err != nil {
		return nil, apiclient.Classify("GitLab", err, "GitLab → Preferences → Access Tokens（需要 api 权限）")
	}
	return &a, nil
}
