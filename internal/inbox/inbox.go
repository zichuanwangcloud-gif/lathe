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
	"errors"
	"fmt"
	"time"

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
// 桩（先红后绿第一次提交）：实现见下一次提交。

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

// ParseKey 按封闭语法解析事项键（桩）。
func ParseKey(s string) (Key, error) { return Key{}, nil }

func taskKey(taskID, eventID int64) string { return fmt.Sprintf("task:%d:e%d", taskID, eventID) }

func prdKey(prdID int64, kind string, round int) string {
	return fmt.Sprintf("prd:%d:%s:r%d", prdID, kind, round)
}

func truncateRunes(s string, n int) string { return s }

func reasonText(raw string, red Redactor) string { return raw }

// TaskItem 把一条任务候选行派生成事项（桩）。
func TaskItem(r store.InboxTaskRow, red Redactor) (Item, bool) { return Item{}, false }

// PRDItem 把一条 PRD 候选行派生成事项（桩）。
func PRDItem(r store.InboxPRDRow, doc *prd.Document, red Redactor) (Item, bool) { return Item{}, false }
