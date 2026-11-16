package gitea

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zichuanwangcloud-gif/lathe/internal/vcs"
)

// Gitea webhook 头（services/webhook/deliver.go）。
const (
	HeaderSignature = "X-Gitea-Signature" // HMAC-SHA256 hex（无前缀）
	HeaderEvent     = "X-Gitea-Event"     // HookEventType.Event()
	HeaderEventType = "X-Gitea-Event-Type"
	HeaderDelivery  = "X-Gitea-Delivery"
)

// ErrBadSignature 表示签名不匹配或缺失。
var ErrBadSignature = errors.New("gitea: webhook 签名校验失败")

// VerifySignature 校验 X-Gitea-Signature（hmac.Equal 常数时间比较，十六进制按小写）。
func VerifySignature(secret string, body []byte, signature string) error {
	if secret == "" {
		return fmt.Errorf("gitea: 未配置 webhook secret")
	}
	sig := strings.ToLower(strings.TrimSpace(signature))
	if sig == "" {
		return fmt.Errorf("%w（缺少签名头）", ErrBadSignature)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(sig)) {
		return ErrBadSignature
	}
	return nil
}

// CommentEvent 是 PR 对话里一条普通评论提炼后的结果，尚未经信任判定。
type CommentEvent struct {
	Repo      string // repository.full_name
	PRNumber  int    // issue.number（PR 与 issue 共用编号空间）
	PRAuthor  string // issue.user.login（PR 的发起人）
	CommentID int64
	Author    string // comment.user.login
	Body      string
	URL       string
	Action    string
}

type commentPayload struct {
	Action string `json:"action"`
	Issue  *struct {
		Number int      `json:"number"`
		User   userJSON `json:"user"`
	} `json:"issue"`
	Comment *struct {
		ID      int64    `json:"id"`
		Body    string   `json:"body"`
		HTMLURL string   `json:"html_url"`
		User    userJSON `json:"user"`
	} `json:"comment"`
	Repository *struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	IsPull bool `json:"is_pull"`
}

// Parse 按 Gitea 实际发送形态识别事件（docs/features/vcs-gitlab-gitea.md §3.5.3
// 「Gitea 事件识别」，对照上游 modules/webhook/type.go、services/webhook/deliver.go）：
//
//  1. X-Gitea-Event == issue_comment 且载荷 is_pull == true → PR 对话里的普通评论，
//     按 IssueCommentPayload 解析。X-Gitea-Event-Type 若存在且不是
//     pull_request_comment，按头与载荷不一致拒绝（ignore）。
//  2. issue_comment 且 is_pull == false → 普通 issue 评论（ignore）。
//  3. X-Gitea-Event ∈ {pull_request_comment, pull_request_approved, pull_request_rejected}
//     是 review 事件：载荷是 PullRequestPayload + review，没有逐条评论与评论 ID，
//     本轮不作为触发源（ignore 并说明；review 总评与行内评论由二轮全量拉取带上）。
//  4. 其余 → ignore。
//
// ignore 非空表示安全忽略（理由给投递记录）；err 只用于载荷解析失败。
func Parse(event, eventType string, body []byte) (ev *CommentEvent, ignore string, err error) {
	switch event {
	case "issue_comment":
	case "pull_request_comment", "pull_request_approved", "pull_request_rejected":
		return nil, "Gitea review 事件本轮不触发二轮；在 PR 对话里发一条评论即可触发，review 总评与行内评论会在二轮全量拉取时一并带上", nil
	default:
		return nil, "不处理的事件类型 " + event, nil
	}
	var p commentPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, "", fmt.Errorf("gitea: 解析 webhook 载荷失败: %w", err)
	}
	if !p.IsPull {
		return nil, "普通 issue 评论，与 PR 无关", nil
	}
	if eventType != "" && eventType != "pull_request_comment" {
		return nil, fmt.Sprintf("事件头与载荷不一致（%s=%s，载荷 is_pull=true）", HeaderEventType, eventType), nil
	}
	if p.Repository == nil || p.Repository.FullName == "" || p.Issue == nil || p.Issue.Number <= 0 || p.Comment == nil || p.Comment.ID == 0 {
		return nil, "", fmt.Errorf("gitea: webhook 载荷缺少仓库、PR 编号或评论")
	}
	return &CommentEvent{
		Repo: p.Repository.FullName, PRNumber: p.Issue.Number, PRAuthor: p.Issue.User.Login,
		CommentID: p.Comment.ID, Author: p.Comment.User.Login, Body: p.Comment.Body,
		URL: p.Comment.HTMLURL, Action: p.Action,
	}, "", nil
}

// Collaborators 是协作者权限查询能力（*Client 实现；测试可注入）。
type Collaborators interface {
	Trusted(ctx context.Context, repoRef, login string) (bool, error)
}

// Actionable 判定这条评论能否驱动一次二轮；能则返回中立事件。两道守卫与 GitHub 同构：
// 先「不是 PR 发起人本人」（issue.user.login；1.20/1.22 的载荷没有 pull_request 字段），
// 再「协作者权限 ∈ {write, admin, owner}」——非 200 一律拒绝（fail-closed）。
//
// trust 为真表示是信任判定拒绝的：理由落投递记录，但不放进响应体。
func Actionable(ctx context.Context, collab Collaborators, e *CommentEvent) (ev *vcs.ReviewEvent, reason string, trust bool) {
	if e == nil {
		return nil, "空事件", false
	}
	if e.Action != "created" {
		return nil, "评论动作 " + e.Action + " 不触发二轮（只处理新建）", false
	}
	if strings.TrimSpace(e.Body) == "" {
		return nil, "空正文的评论", false
	}
	if e.Author != "" && strings.EqualFold(e.Author, e.PRAuthor) {
		return nil, "评论来自 PR 作者本身（Lathe 开 PR 所用的账号），不回流给自己", false
	}
	who := e.Author
	if who == "" {
		who = "（未知账号）"
	}
	if collab == nil {
		return nil, "无法查询 " + who + " 的协作者权限（未接线），按不可信拒绝", true
	}
	ok, err := collab.Trusted(ctx, e.Repo, e.Author)
	if err != nil || !ok {
		why := "权限不足"
		if err != nil {
			why = err.Error()
		}
		return nil, "评论者 " + who + " 不是有写权限的协作者（" + why + "），不驱动二轮：二轮会以任务属主的凭据运行 agent", true
	}
	return &vcs.ReviewEvent{
		Provider: vcs.ProviderGitea,
		Event:    "issue_comment",
		Action:   "created",
		Repo:     e.Repo,
		PRNumber: e.PRNumber,
		PRAuthor: e.PRAuthor,
		Key:      CommentKey(e.CommentID),
		Author:   e.Author,
		Body:     e.Body,
		URL:      e.URL,
	}, "", false
}
