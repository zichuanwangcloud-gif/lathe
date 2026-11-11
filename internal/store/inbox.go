package store

import (
	"context"
	"encoding/json"
	"time"
)

// 收件箱（docs/features/inbox.md）的持久化面。
//
// 收件箱事项不入库：任务侧从 tasks + task_events 派生，PRD 侧从 prds 派生，
// 本文件只负责「取候选行」与「读写视图标记（inbox_marks）」两件事。
// 派生规则（哪个状态算哪种事项、键怎么拼、原因怎么取）在 internal/inbox，
// store 不认识 prd 包（prd 已 import store，反过来会成环）。

// InboxTaskRow 是一条任务侧的候选行：当前处于等人状态的任务 + 它进入
// 当前状态的那次转移事件（锚点）。
type InboxTaskRow struct {
	ID              int64
	State           string
	ExternalKey     string
	TrackerProvider string
	FailureReason   *string
	FailureStage    *string
	ProviderRepo    string
	// IssueID / IssueTitle 仅内置工单（tracker_provider = 'internal'）填充。
	IssueID    *int64
	IssueTitle *string
	// EventID / Since / Payload 取自锚点事件：最新一条
	// from_state IS DISTINCT FROM to_state 的 task_events 行。
	EventID int64
	Since   time.Time
	Payload map[string]any
	// PredecessorID 仅 blocked_dep 且前驱存在、属于同一用户时非 nil。
	// 归属在 SQL 里判过，派生层只认这个字段，从不读 payload 拼链接。
	PredecessorID *int64
}

// InboxPRDRow 是一条 PRD 侧的候选行（只含活 PRD）。
type InboxPRDRow struct {
	ID            int64
	State         string
	Round         int
	Processing    bool
	Document      json.RawMessage
	LastError     *string
	OriginalInput string
	ProviderRepo  string
	UpdatedAt     time.Time
}

// InboxSubject 是标记所属的主体：任务或 PRD，二选一。
type InboxSubject struct {
	TaskID int64
	PRDID  int64
}

// InboxMark 是一个人对一条事项的视图状态。
type InboxMark struct {
	Key         string
	ReadAt      *time.Time
	DismissedAt *time.Time
}

// InboxMarkOp 是一次标记操作。
type InboxMarkOp string

const (
	// InboxMarkRead 标为已读。
	InboxMarkRead InboxMarkOp = "read"
	// InboxMarkUnread 标为未读：未忽略则删行，已忽略则只清 read_at。
	InboxMarkUnread InboxMarkOp = "unread"
	// InboxMarkDismiss 忽略。
	InboxMarkDismiss InboxMarkOp = "dismiss"
)

// InboxKeySubject 是「全部已读」的一项输入。
type InboxKeySubject struct {
	Key     string
	Subject InboxSubject
}

// 桩（先红后绿第一次提交）：实现见下一次提交。

// InboxTaskCandidates 返回 userID 名下处于等人状态的任务候选行。
func (s *Store) InboxTaskCandidates(ctx context.Context, userID int64) ([]InboxTaskRow, error) {
	return nil, nil
}

// InboxPRDCandidates 返回 userID 名下的活 PRD 候选行。
func (s *Store) InboxPRDCandidates(ctx context.Context, userID int64) ([]InboxPRDRow, error) {
	return nil, nil
}

// InboxMarks 返回 userID 的全部标记，按 item_key 索引。
func (s *Store) InboxMarks(ctx context.Context, userID int64) (map[string]InboxMark, error) {
	return map[string]InboxMark{}, nil
}

// UpsertInboxMark 对一条事项落一次标记，并剪掉同一主体的旧键标记。
func (s *Store) UpsertInboxMark(ctx context.Context, userID int64, key string, subj InboxSubject, op InboxMarkOp) error {
	return nil
}

// MarkInboxRead 把一批事项标为已读（「全部已读」）。
func (s *Store) MarkInboxRead(ctx context.Context, userID int64, items []InboxKeySubject) error {
	return nil
}

// InboxSubjectOwned 报告主体是否存在且属于 userID。
func (s *Store) InboxSubjectOwned(ctx context.Context, userID int64, subj InboxSubject) (bool, error) {
	return false, nil
}
