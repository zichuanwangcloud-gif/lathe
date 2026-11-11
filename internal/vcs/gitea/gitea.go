// Package gitea 是代码平台契约（internal/vcs）的 Gitea 实现（含自建实例）：
// 开 PR、查合并状态、拉 review 意见、保存凭据时验证、webhook 验签、事件识别与
// 信任判定。只用标准库。
//
// 信任判定用仓库协作者权限查询（write/admin/owner 才可信），fail-closed。
package gitea

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/zichuanwangcloud-gif/lathe/internal/vcs"
	"github.com/zichuanwangcloud-gif/lathe/internal/vcs/internal/apiclient"
)

var (
	_ vcs.Client               = (*Client)(nil)
	_ vcs.ReviewFeedbackSource = (*Client)(nil)
)

// maxFeedbackPages 与 GitHub 实现同口径：意见总翻页上限 10 页。
const maxFeedbackPages = 10

// pageLimit 是每页条数（Gitea 默认上限 50）。
const pageLimit = 50

// Client 是 Gitea REST v1 客户端。
type Client struct {
	api *apiclient.Client
}

// New 用实例地址与访问令牌构造客户端（Authorization: token <tok>）。
func New(baseURL, token string) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("gitea: 缺少访问令牌")
	}
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("gitea: 缺少实例地址")
	}
	return &Client{api: apiclient.New("gitea", baseURL, "Authorization", "token "+token, token)}, nil
}

// repoPath 返回 /api/v1/repos/<owner>/<repo>（逐段转义）。
func repoPath(ref string) (string, error) {
	ns, name, err := vcs.SplitRef(vcs.ProviderGitea, ref)
	if err != nil {
		return "", err
	}
	segs := strings.Split(ns, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	return "/api/v1/repos/" + strings.Join(segs, "/") + "/" + url.PathEscape(name), nil
}

type userJSON struct {
	Login string `json:"login"`
}

type pull struct {
	Number  int      `json:"number"`
	HTMLURL string   `json:"html_url"`
	State   string   `json:"state"`
	Merged  bool     `json:"merged"`
	User    userJSON `json:"user"`
	Head    struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// CreatePR 开 PR；同 head→base 的开放 PR 已存在则复用。
func (c *Client) CreatePR(ctx context.Context, p vcs.PRParams) (*vcs.PullRequest, error) {
	rp, err := repoPath(p.ProviderRepo)
	if err != nil {
		return nil, err
	}
	if p.Head == "" || p.Base == "" {
		return nil, fmt.Errorf("gitea: head 与 base 均不能为空")
	}
	if p.Head == p.Base {
		return nil, fmt.Errorf("gitea: head 与 base 不能相同（%s）", p.Head)
	}
	if strings.TrimSpace(p.Title) == "" {
		return nil, fmt.Errorf("gitea: PR 标题不能为空")
	}
	if existing, err := c.findOpen(ctx, rp, p.Head, p.Base); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	title := p.Title
	if p.Draft {
		title = "WIP: " + title
	}
	var created pull
	_, err = c.api.Do(ctx, http.MethodPost, rp+"/pulls", map[string]any{
		"head": p.Head, "base": p.Base, "title": title, "body": p.Body,
	}, &created)
	if err != nil {
		var se *apiclient.StatusError
		if errors.As(err, &se) && (se.Code == http.StatusUnprocessableEntity || se.Code == http.StatusConflict || se.Code == http.StatusBadRequest) {
			low := strings.ToLower(se.Body)
			if strings.Contains(low, "no changes") || strings.Contains(low, "no commits") {
				return nil, fmt.Errorf("%w（%s → %s）", vcs.ErrNoCommits, p.Head, p.Base)
			}
			if existing, ferr := c.findOpen(ctx, rp, p.Head, p.Base); ferr == nil && existing != nil {
				return existing, nil
			}
		}
		return nil, fmt.Errorf("gitea: 创建 PR 失败(%s → %s): %w", p.Head, p.Base, err)
	}
	return &vcs.PullRequest{Number: created.Number, URL: created.HTMLURL, State: created.State}, nil
}

func (c *Client) findOpen(ctx context.Context, rp, head, base string) (*vcs.PullRequest, error) {
	for page := 1; page <= maxFeedbackPages; page++ {
		var list []pull
		if _, err := c.api.Do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls?state=open&limit=%d&page=%d", rp, pageLimit, page), nil, &list); err != nil {
			return nil, fmt.Errorf("gitea: 查询既有 PR 失败: %w", err)
		}
		for _, pr := range list {
			if pr.Head.Ref == head && pr.Base.Ref == base {
				return &vcs.PullRequest{Number: pr.Number, URL: pr.HTMLURL, State: pr.State, Existing: true}, nil
			}
		}
		if len(list) < pageLimit {
			break
		}
	}
	return nil, nil
}

// GetPRInfo 读取 PR 当前状态（§3.6：merged==true → Merged；state 原样 open/closed）。
func (c *Client) GetPRInfo(ctx context.Context, repoRef string, number int) (*vcs.PRInfo, error) {
	rp, err := repoPath(repoRef)
	if err != nil {
		return nil, err
	}
	var pr pull
	if _, err := c.api.Do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls/%d", rp, number), nil, &pr); err != nil {
		return nil, fmt.Errorf("gitea: 读取 PR #%d 失败: %w", number, err)
	}
	state := pr.State
	if pr.Merged {
		state = "closed"
	}
	return &vcs.PRInfo{Number: pr.Number, Merged: pr.Merged, State: state, BaseRef: pr.Base.Ref, HeadSHA: pr.Head.SHA}, nil
}

// 意见 Key 前缀。
func commentKey(id int64) string       { return fmt.Sprintf("gitea:comment:%d", id) }
func reviewKey(id int64) string        { return fmt.Sprintf("gitea:review:%d", id) }
func reviewCommentKey(id int64) string { return fmt.Sprintf("gitea:review-comment:%d", id) }

// CommentKey 是 webhook 触发意见的稳定标识（与二轮拉取的普通评论同一个 Key）。
func CommentKey(id int64) string { return commentKey(id) }

type commentJSON struct {
	ID       int64    `json:"id"`
	Body     string   `json:"body"`
	User     userJSON `json:"user"`
	HTMLURL  string   `json:"html_url"`
	Path     string   `json:"path"`
	Position int      `json:"position"`
}

type reviewJSON struct {
	ID      int64    `json:"id"`
	Body    string   `json:"body"`
	State   string   `json:"state"`
	User    userJSON `json:"user"`
	HTMLURL string   `json:"html_url"`
}

// ReviewFeedback 拉取 PR 上的全部意见：普通评论（issues/<n>/comments，只含
// CommentTypeComment）、review 总评（正文非空的）、每个 review 的行内评论，三者
// 合并、各自按作者走同一道协作者权限过滤。翻页上限沿用 GitHub 实现的口径：
// 每类意见最多 10 页（每个 review 的行内评论各一页）。
func (c *Client) ReviewFeedback(ctx context.Context, repoRef string, number int) (*vcs.ReviewFeedback, error) {
	rp, err := repoPath(repoRef)
	if err != nil {
		return nil, err
	}
	var pr pull
	if _, err := c.api.Do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls/%d", rp, number), nil, &pr); err != nil {
		return nil, fmt.Errorf("gitea: 读取 PR #%d 失败: %w", number, err)
	}
	fb := &vcs.ReviewFeedback{PRAuthor: pr.User.Login}
	trusted := map[string]bool{}
	trust := func(login string) bool {
		if ok, cached := trusted[login]; cached {
			return ok
		}
		ok, _ := c.Trusted(ctx, repoRef, login)
		trusted[login] = ok
		return ok
	}
	for page := 1; page <= maxFeedbackPages; page++ {
		var list []commentJSON
		if _, err := c.api.Do(ctx, http.MethodGet, fmt.Sprintf("%s/issues/%d/comments?limit=%d&page=%d", rp, number, pageLimit, page), nil, &list); err != nil {
			return nil, fmt.Errorf("gitea: 读取 PR #%d 评论失败: %w", number, err)
		}
		for _, cm := range list {
			if strings.TrimSpace(cm.Body) == "" || !trust(cm.User.Login) {
				continue
			}
			fb.Comments = append(fb.Comments, vcs.ReviewComment{ID: cm.ID, Key: commentKey(cm.ID),
				Author: cm.User.Login, Body: cm.Body, URL: cm.HTMLURL})
		}
		if len(list) < pageLimit {
			break
		}
	}

	var reviews []reviewJSON
	for page := 1; page <= maxFeedbackPages; page++ {
		var list []reviewJSON
		if _, err := c.api.Do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls/%d/reviews?limit=%d&page=%d", rp, number, pageLimit, page), nil, &list); err != nil {
			return nil, fmt.Errorf("gitea: 读取 PR #%d review 失败: %w", number, err)
		}
		reviews = append(reviews, list...)
		if len(list) < pageLimit {
			break
		}
	}
	for _, rv := range reviews {
		state := strings.ToUpper(rv.State)
		if strings.TrimSpace(rv.Body) != "" && state != "APPROVED" && state != "DISMISSED" && trust(rv.User.Login) {
			fb.Comments = append(fb.Comments, vcs.ReviewComment{ID: rv.ID, Key: reviewKey(rv.ID),
				Author: rv.User.Login, Body: rv.Body, URL: rv.HTMLURL})
		}
		var list []commentJSON
		if _, err := c.api.Do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls/%d/reviews/%d/comments", rp, number, rv.ID), nil, &list); err != nil {
			return nil, fmt.Errorf("gitea: 读取 review %d 的行内评论失败: %w", rv.ID, err)
		}
		for _, cm := range list {
			if strings.TrimSpace(cm.Body) == "" || !trust(cm.User.Login) {
				continue
			}
			fb.Comments = append(fb.Comments, vcs.ReviewComment{ID: cm.ID, Key: reviewCommentKey(cm.ID),
				Author: cm.User.Login, Body: cm.Body, Path: cm.Path, Line: cm.Position, URL: cm.HTMLURL})
		}
	}
	return fb, nil
}

// trustedPermissions 是能驱动二轮的协作者权限（白名单：未知取值不可信）。
var trustedPermissions = map[string]bool{"write": true, "admin": true, "owner": true}

// Trusted 查 login 在仓库上的协作者权限；非 200 或权限不在白名单 → false（fail-closed）。
func (c *Client) Trusted(ctx context.Context, repoRef, login string) (bool, error) {
	rp, err := repoPath(repoRef)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(login) == "" {
		return false, fmt.Errorf("gitea: 评论者账号为空")
	}
	var perm struct {
		Permission string `json:"permission"`
	}
	if _, err := c.api.Do(ctx, http.MethodGet, rp+"/collaborators/"+url.PathEscape(login)+"/permission", nil, &perm); err != nil {
		return false, err
	}
	if !trustedPermissions[strings.ToLower(perm.Permission)] {
		return false, fmt.Errorf("权限为 %q", perm.Permission)
	}
	return true, nil
}

// Account 是令牌对应的 Gitea 账号。
type Account struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

// Verify 校验令牌可用（GET /api/v1/user）。
func (c *Client) Verify(ctx context.Context) (*Account, error) {
	var a Account
	if _, err := c.api.Do(ctx, http.MethodGet, "/api/v1/user", nil, &a); err != nil {
		return nil, apiclient.Classify("Gitea", err, "Gitea → 设置 → 应用 → 管理访问令牌（需要仓库读写权限）")
	}
	return &a, nil
}
