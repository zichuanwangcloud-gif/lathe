package gitlab

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zichuanwangcloud-gif/lathe/internal/vcs"
)

// GitLab webhook 头。
const (
	// HeaderToken 是项目 webhook 配置的 Secret Token（明文回传，不是签名）。
	HeaderToken = "X-Gitlab-Token"
	// HeaderEvent 是事件类型（关心 Note Hook）。
	HeaderEvent = "X-Gitlab-Event"
	// HeaderEventUUID 是本次投递的唯一 ID（较新版本才有）。
	HeaderEventUUID = "X-Gitlab-Event-UUID"
	// EventNote 是评论事件。
	EventNote = "Note Hook"
)

// ErrBadToken 表示 Secret Token 不匹配或缺失。
var ErrBadToken = errors.New("gitlab: webhook Secret Token 校验失败")

// ErrUnsupportedEvent 表示事件不在处理范围内（调用方应忽略，不是错误）。
var ErrUnsupportedEvent = errors.New("gitlab: 不处理的 webhook 事件")

// VerifyToken 常数时间比较 X-Gitlab-Token 与所配密钥。
func VerifyToken(secret, got string) error {
	if secret == "" {
		return fmt.Errorf("gitlab: 未配置 webhook Secret Token")
	}
	if got == "" || subtle.ConstantTimeCompare([]byte(secret), []byte(got)) != 1 {
		return ErrBadToken
	}
	return nil
}

// NoteEvent 是一次 Note Hook（MR 上的评论）提炼后的结果，尚未经信任判定。
type NoteEvent struct {
	Repo      string // path_with_namespace，如 group/sub/demo
	MRIID     int    // merge_request.iid（项目内序号）
	MRAuthor  int64  // merge_request.author_id
	NoteID    int64
	NoteAt    string // object_attributes.updated_at（投递 ID 兜底用）
	Action    string // object_attributes.action（较新版本：create/update）
	AuthorID  int64  // object_attributes.author_id
	Username  string // user.username（评论者）
	Body      string
	Path      string
	Line      int
	URL       string
	NoteOnMR  bool
	EventName string
}

type notePayload struct {
	ObjectKind string `json:"object_kind"`
	User       struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	Project struct {
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	ObjectAttributes struct {
		ID           int64  `json:"id"`
		Note         string `json:"note"`
		NoteableType string `json:"noteable_type"`
		AuthorID     int64  `json:"author_id"`
		URL          string `json:"url"`
		UpdatedAt    string `json:"updated_at"`
		Action       string `json:"action"`
		System       bool   `json:"system"`
		Position     *struct {
			NewPath string `json:"new_path"`
			NewLine *int   `json:"new_line"`
		} `json:"position"`
	} `json:"object_attributes"`
	MergeRequest *struct {
		IID      int   `json:"iid"`
		AuthorID int64 `json:"author_id"`
	} `json:"merge_request"`
}

// ParseNoteHook 把（已验签的）载荷解析成 NoteEvent。非 Note Hook、或评论不在 MR 上 →
// ErrUnsupportedEvent。
func ParseNoteHook(eventHeader string, body []byte) (*NoteEvent, error) {
	if eventHeader != EventNote {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedEvent, eventHeader)
	}
	var p notePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("gitlab: 解析 webhook 载荷失败: %w", err)
	}
	if p.ObjectAttributes.NoteableType != "MergeRequest" || p.MergeRequest == nil {
		return nil, fmt.Errorf("%w: 评论不在 MR 上（%s）", ErrUnsupportedEvent, p.ObjectAttributes.NoteableType)
	}
	if p.Project.PathWithNamespace == "" || p.MergeRequest.IID <= 0 || p.ObjectAttributes.ID == 0 {
		return nil, fmt.Errorf("gitlab: webhook 载荷缺少项目、MR 编号或评论 ID")
	}
	a := p.ObjectAttributes
	ev := &NoteEvent{
		Repo: p.Project.PathWithNamespace, MRIID: p.MergeRequest.IID, MRAuthor: p.MergeRequest.AuthorID,
		NoteID: a.ID, NoteAt: a.UpdatedAt, Action: a.Action, AuthorID: a.AuthorID, Username: p.User.Username,
		Body: a.Note, URL: a.URL, NoteOnMR: true, EventName: eventHeader,
	}
	if ev.AuthorID == 0 {
		ev.AuthorID = p.User.ID
	}
	if a.System {
		ev.Action = "system"
	}
	if a.Position != nil {
		ev.Path = a.Position.NewPath
		if a.Position.NewLine != nil {
			ev.Line = *a.Position.NewLine
		}
	}
	return ev, nil
}

// DeliveryID 是投递去重键（不含平台前缀）：优先 X-Gitlab-Event-UUID，缺失时用
// 评论 ID + updated_at 拼。
func (e *NoteEvent) DeliveryID(uuid string) string {
	if strings.TrimSpace(uuid) != "" {
		return uuid
	}
	return fmt.Sprintf("note-%d-%s", e.NoteID, e.NoteAt)
}

// MemberChecker 是成员查询能力（*Client 实现；测试可注入）。
type MemberChecker interface {
	MemberAccessLevel(ctx context.Context, repoRef string, userID int64) (int, error)
}

// Actionable 判定这条评论能否驱动一次二轮；能则返回中立事件。
//
// trust 为真表示是信任判定拒绝的（成员关系不足/查询失败）：调用方把理由落进
// 投递记录，但【不放进响应体】—— 不让响应变成探测他人项目成员关系的手段。
//
// 两道守卫与 GitHub 同构：先「不是 MR 作者本人」（Lathe 以属主令牌开 MR，自身回环），
// 再「对项目有 ≥ Reporter 的权限」——成员接口 404/403/5xx/超时一律拒绝（fail-closed），
// 拒绝理由给投递记录。
func Actionable(ctx context.Context, members MemberChecker, e *NoteEvent) (ev *vcs.ReviewEvent, reason string, trust bool) {
	if e == nil {
		return nil, "空事件", false
	}
	if e.Action != "" && e.Action != "create" {
		return nil, "评论动作 " + e.Action + " 不触发二轮（只处理新建）", false
	}
	if strings.TrimSpace(e.Body) == "" {
		return nil, "空正文的评论", false
	}
	if e.AuthorID != 0 && e.AuthorID == e.MRAuthor {
		return nil, "评论来自 MR 作者本身（Lathe 开 MR 所用的账号），不回流给自己", false
	}
	who := e.Username
	if who == "" {
		who = fmt.Sprintf("（用户 #%d）", e.AuthorID)
	}
	if members == nil {
		return nil, "无法查询 " + who + " 的项目成员关系（未接线），按不可信拒绝", true
	}
	lvl, err := members.MemberAccessLevel(ctx, e.Repo, e.AuthorID)
	if err != nil {
		return nil, "无法确认 " + who + " 是项目成员（" + err.Error() + "），按不可信拒绝：二轮会以任务属主的凭据运行 agent", true
	}
	if lvl < ReporterAccess {
		return nil, fmt.Sprintf("评论者 %s 的项目权限 access_level=%d 低于 Reporter(20)，不驱动二轮", who, lvl), true
	}
	return &vcs.ReviewEvent{
		Provider: vcs.ProviderGitLab,
		Event:    "note",
		Action:   "created",
		Repo:     e.Repo,
		PRNumber: e.MRIID,
		Key:      NoteKey(e.NoteID),
		Author:   e.Username,
		Body:     e.Body,
		Path:     e.Path,
		Line:     e.Line,
		URL:      e.URL,
	}, "", false
}
