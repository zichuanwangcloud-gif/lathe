// Package inbox 把「流水线上刻意停下来等人」的事项聚合成一个收件箱
// （docs/features/inbox.md）。
//
// 事项全部从现有状态派生：任务侧的真相源是 tasks.state 与 task_events，
// PRD 侧是 prds。本包不复制任何状态，唯一落库的是 inbox_marks 里每个人对
// 每条事项「读过 / 忽略过」的视图状态。收件箱只读状态、不提供任何状态转移
// 入口 —— 处理动作仍走原来的详情页与端点，不会成为绕过闸门的捷径。
//
// 本文件是派生规则（纯函数，不连库）；组合与标记见 service.go。
package inbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zichuanwangcloud-gif/lathe/internal/prd"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// Kind 是事项种类。
type Kind string

const (
	// KindTaskBlockedSpec 单子不明确，等人补充需求。
	KindTaskBlockedSpec Kind = "task_blocked_spec"
	// KindTaskAwaitingApproval manual 档验证已过，等人放行。
	KindTaskAwaitingApproval Kind = "task_awaiting_approval"
	// KindTaskReviewParked review 意见已记下但无法自动续跑二轮。
	KindTaskReviewParked Kind = "task_review_parked"
	// KindTaskBlockedDep 前驱失败，后继被阻塞。
	KindTaskBlockedDep Kind = "task_blocked_dep"
	// KindTaskFailed 失败待处置（唯一可忽略的种类）。
	KindTaskFailed Kind = "task_failed"
	// KindPRDRunFailed PRD 后台轮次失败。
	KindPRDRunFailed Kind = "prd_run_failed"
	// KindPRDAnswers PRD 等人回答问题。
	KindPRDAnswers Kind = "prd_answers"
	// KindPRDReview PRD 等人批准 / 打回。
	KindPRDReview Kind = "prd_review"
	// KindPRDConfirm PRD 有推断节待人逐节确认。
	KindPRDConfirm Kind = "prd_confirm"
)

// Kinds 按展示顺序列出全部种类。
func Kinds() []Kind {
	return []Kind{
		KindTaskBlockedSpec, KindTaskAwaitingApproval, KindTaskReviewParked,
		KindTaskBlockedDep, KindTaskFailed,
		KindPRDRunFailed, KindPRDAnswers, KindPRDReview, KindPRDConfirm,
	}
}

// Valid 报告种类是否已知。
func (k Kind) Valid() bool {
	for _, v := range Kinds() {
		if v == k {
			return true
		}
	}
	return false
}

// 主体类型。
const (
	SubjectTask = "task"
	SubjectPRD  = "prd"
)

// maxReasonRunes 是 Item.Reason 的展示上限（按 rune）。完整原因看详情页。
const maxReasonRunes = 200

// maxTitleRunes 是 Item.Title 的展示上限（按 rune）。
const maxTitleRunes = 120

// maxKeyLen 是事项键的长度上限（与 inbox_marks_key_len 约束一致）。
const maxKeyLen = 128

// reviewParkedReason 是 review 停车的固定原因：刻意不带 review 评论原文
// （那是第三方写的，详情页才展示）。
const reviewParkedReason = "review 意见已记下，但无法自动续跑二轮，需要你处理"

// Link 是一条站内链接（相对路径，不含主机名）。
type Link struct {
	Label string `json:"label"`
	Href  string `json:"href"`
}

// Item 是一条收件箱事项。
type Item struct {
	Key         string    `json:"key"`
	Kind        Kind      `json:"kind"`
	SubjectType string    `json:"subjectType"` // "task" | "prd"
	SubjectID   int64     `json:"subjectId"`
	Title       string    `json:"title"`  // 内置工单标题 / 外部 key / PRD 一句话或原文首行
	Repo        string    `json:"repo"`   // 仓库 owner/name
	Reason      string    `json:"reason"` // 先脱敏、后按 rune 截断到 maxReasonRunes
	Since       time.Time `json:"since"`  // 进入当前停留的时刻
	Link        string    `json:"link"`   // 站内相对路径
	Extra       []Link    `json:"extra,omitempty"`
	Dismissible bool      `json:"dismissible"`
	Read        bool      `json:"read"`
}

// Redactor 是展示前的脱敏函数。由 cmd/lathe 装配为
//
//	func(s string) string { return runner.RedactText(s, cfg.KnownSecrets()) }
//
// 本包不 import runner；NewService 收到 nil 直接返回错误（启动即失败，不静默降级）。
type Redactor func(string) string

// ---------------------------------------------------------------- 键

// keyRe 是事项键的封闭语法。不合法的键直接 400，既不查库也不回显输入。
var keyRe = regexp.MustCompile(`^(task:[1-9][0-9]{0,18}:e[1-9][0-9]{0,18}|prd:[1-9][0-9]{0,18}:(answers|review|confirm|run_failed):r[0-9]{1,9}(:[0-9a-f]{8})?)$`)

// ErrBadKey 表示事项键不合法。
var ErrBadKey = errors.New("inbox: 事项键不合法")

// Key 是解析后的事项键。
type Key struct {
	Raw         string
	SubjectType string
	SubjectID   int64
}

// Subject 返回 store 层的主体表示。
func (k Key) Subject() store.InboxSubject {
	if k.SubjectType == SubjectTask {
		return store.InboxSubject{TaskID: k.SubjectID}
	}
	return store.InboxSubject{PRDID: k.SubjectID}
}

// ParseKey 按封闭语法解析事项键，返回主体类型与主体 id。
func ParseKey(s string) (Key, error) {
	if len(s) == 0 || len(s) > maxKeyLen || !keyRe.MatchString(s) {
		return Key{}, ErrBadKey
	}
	parts := strings.SplitN(s, ":", 3)
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		// 19 位数字可能超出 int64：正则放过了，这里兜住
		return Key{}, ErrBadKey
	}
	if parts[0] == SubjectTask {
		ev, err := strconv.ParseInt(strings.TrimPrefix(parts[2], "e"), 10, 64)
		if err != nil || ev <= 0 {
			return Key{}, ErrBadKey
		}
	}
	return Key{Raw: s, SubjectType: parts[0], SubjectID: id}, nil
}

func taskKey(taskID, eventID int64) string {
	return fmt.Sprintf("task:%d:e%d", taskID, eventID)
}

func prdKey(prdID int64, kind string, round int) string {
	return fmt.Sprintf("prd:%d:%s:r%d", prdID, kind, round)
}

// errHash8 是 last_error 的 sha256 前 8 个十六进制字符：同一轮第二次失败、
// 原因不同时就是新事项（processing_started_at 跑完被置 NULL，当不了锚点）。
func errHash8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// ---------------------------------------------------------------- 文本

// truncateRunes 按 rune 截断到 n，超长时补一个省略号。结果保证 utf8.ValidString
// （按 byte 切会把多字节字符切断，事件落库因此出过 SQLSTATE 22021）。
func truncateRunes(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos] + "…"
		}
		i++
	}
	return s
}

// reasonText 是全部自由文本原因的唯一出口：**先脱敏、再截断**。
// 顺序不能反：先截断可能把一个 token 切成形态表认不出的残段。
func reasonText(raw string, red Redactor) string {
	if red != nil {
		raw = red(raw)
	}
	return truncateRunes(strings.TrimSpace(raw), maxReasonRunes)
}

func payloadString(p map[string]any, key string) string {
	if v, ok := p[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// ---------------------------------------------------------------- 任务

// TaskItem 把一条任务候选行派生成事项。不在等人状态的行返回 false。
func TaskItem(r store.InboxTaskRow, red Redactor) (Item, bool) {
	var (
		kind   Kind
		reason string
	)
	switch r.State {
	case "blocked_spec":
		kind = KindTaskBlockedSpec
		if q := payloadString(r.Payload, "question"); q != "" {
			reason = q
		} else {
			reason = deref(r.FailureReason)
		}
		reason = reasonText(reason, red)
	case "awaiting_approval":
		kind = KindTaskAwaitingApproval
		reason = reasonText(payloadString(r.Payload, "reason"), red)
	case "review_feedback":
		kind = KindTaskReviewParked
		reason = reviewParkedReason
	case "blocked_dep":
		kind = KindTaskBlockedDep
		reason = reasonText(deref(r.FailureReason), red)
	case "failed":
		kind = KindTaskFailed
		reason = reasonText(deref(r.FailureReason), red)
	default:
		return Item{}, false
	}

	title := r.ExternalKey
	if r.IssueTitle != nil && strings.TrimSpace(*r.IssueTitle) != "" {
		title = r.ExternalKey + " " + strings.TrimSpace(*r.IssueTitle)
	}

	it := Item{
		Key:         taskKey(r.ID, r.EventID),
		Kind:        kind,
		SubjectType: SubjectTask,
		SubjectID:   r.ID,
		Title:       truncateRunes(title, maxTitleRunes),
		Repo:        r.ProviderRepo,
		Reason:      reason,
		Since:       r.Since,
		Link:        fmt.Sprintf("/tasks/%d", r.ID),
		Dismissible: kind == KindTaskFailed,
	}
	if r.TrackerProvider == "internal" && r.IssueID != nil {
		it.Extra = append(it.Extra, Link{Label: "工单", Href: fmt.Sprintf("/issues/%d", *r.IssueID)})
	}
	// 前驱链接只认 SQL 判过归属的 PredecessorID，从不读 payload 的 blocked_by 拼链接。
	if kind == KindTaskBlockedDep && r.PredecessorID != nil {
		it.Extra = append(it.Extra, Link{Label: "前驱任务", Href: fmt.Sprintf("/tasks/%d", *r.PredecessorID)})
	}
	return it, true
}

// ---------------------------------------------------------------- PRD

// PRDItem 把一条 PRD 候选行派生成事项（每份 PRD 至多一条）。doc 为 nil 表示
// document 为空或解析失败。优先级 run_failed > answers > review > confirm。
func PRDItem(r store.InboxPRDRow, doc *prd.Document, red Redactor) (Item, bool) {
	it := Item{
		SubjectType: SubjectPRD,
		SubjectID:   r.ID,
		Title:       truncateRunes(prdTitle(r, doc), maxTitleRunes),
		Repo:        r.ProviderRepo,
		Since:       r.UpdatedAt,
		Link:        fmt.Sprintf("/prds/%d", r.ID),
	}

	active := r.State == "drafting" || r.State == "awaiting_answers"
	switch {
	case active && !r.Processing && r.LastError != nil:
		it.Kind = KindPRDRunFailed
		it.Key = prdKey(r.ID, "run_failed", r.Round) + ":" + errHash8(*r.LastError)
		it.Reason = reasonText(*r.LastError, red)
	case r.State == "awaiting_answers" && !r.Processing:
		it.Kind = KindPRDAnswers
		it.Key = prdKey(r.ID, "answers", r.Round)
		if n := openQuestions(doc); n > 0 {
			it.Reason = fmt.Sprintf("%d 个问题待回答", n)
		} else {
			it.Reason = "有问题待回答"
		}
	case r.State == "ready_for_review":
		it.Kind = KindPRDReview
		it.Key = prdKey(r.ID, "review", r.Round)
		it.Reason = "已提交评审，等你批准或打回"
	case r.State == "drafting" && !r.Processing && r.LastError == nil:
		first, n := inferredSections(doc)
		if n == 0 {
			return Item{}, false
		}
		it.Kind = KindPRDConfirm
		it.Key = prdKey(r.ID, "confirm", r.Round)
		it.Reason = fmt.Sprintf("%d 节待确认", n)
		// first 只可能取 Document.Sections() 的固定 key
		it.Link += "?section=" + first
	default:
		return Item{}, false
	}
	return it, true
}

func prdTitle(r store.InboxPRDRow, doc *prd.Document) string {
	if doc != nil {
		if t := firstLine(doc.OneLiner.Body); t != "" {
			return t
		}
	}
	if t := firstLine(r.OriginalInput); t != "" {
		return t
	}
	return fmt.Sprintf("PRD #%d", r.ID)
}

func openQuestions(doc *prd.Document) int {
	if doc == nil {
		return 0
	}
	n := 0
	for _, q := range doc.Questions {
		if q.Status == "" || q.Status == prd.QuestionOpen {
			n++
		}
	}
	return n
}

func inferredSections(doc *prd.Document) (first string, n int) {
	if doc == nil {
		return "", 0
	}
	for _, s := range doc.Sections() {
		if s.Section.Status == prd.StatusInferred {
			if n == 0 {
				first = s.Key
			}
			n++
		}
	}
	return first, n
}
