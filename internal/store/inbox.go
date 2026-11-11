package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
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

// inboxTaskCandidatesSQL 取任务侧候选行。
//
//   - mine：本人的全部任务，窗口函数算出同一工单 (repo, provider, key) 下最新
//     的任务行 id。failed 只在它是最新行时才算（issue 重开会新建任务行，旧的
//     failed 行永远是 failed，不排除就会无限积累）。不用 NOT EXISTS：工单键上
//     只有一个覆盖活任务的部分唯一索引，反查用不上。
//   - LATERAL ev：锚点 = 进入当前状态的那次转移。只认
//     from_state IS DISTINCT FROM to_state —— 收割机对终态任务追加的
//     failed→failed 记账事件（ClaimForReap，payload kind=worktree_reaped）不是
//     转移，取最新一条事件的话，已读/已忽略的失败会在现场回收后换键重现。
//     NULL（建任务首条事件）与任何状态都 distinct，算一次进入。
//   - p：blocked_dep 的前驱，只给本人的、仍存在的前驱。类型转换必须包在 CASE
//     里：AND 不保证短路，规划器会把 ::bigint 提进 Hash Cond 先算，脏 payload
//     （"abc"、7.5）会让整条查询报错、整个收件箱 500。正则限 18 位以内，
//     一定落在 bigint 范围。
const inboxTaskCandidatesSQL = `
WITH mine AS (
  SELECT t.*, max(t.id) OVER (PARTITION BY t.repo_id, t.tracker_provider, t.external_key) AS newest
  FROM tasks t WHERE t.user_id = $1
)
SELECT m.id, m.state, m.external_key, m.tracker_provider, m.failure_reason, m.failure_stage,
       r.provider_repo, i.id AS issue_id, i.title AS issue_title,
       ev.id AS event_id, ev.at AS since, ev.payload,
       p.id AS predecessor_id
FROM mine m
JOIN repos r ON r.id = m.repo_id
LEFT JOIN issues i ON m.tracker_provider = 'internal' AND i.user_id = m.user_id AND i.key = m.external_key
CROSS JOIN LATERAL (
  SELECT id, at, payload FROM task_events
  WHERE task_id = m.id AND from_state IS DISTINCT FROM to_state
  ORDER BY at DESC, id DESC LIMIT 1
) ev
LEFT JOIN tasks p
  ON m.state = 'blocked_dep'
 AND p.id = CASE
       WHEN jsonb_typeof(ev.payload->'blocked_by') = 'number'
        AND ev.payload->>'blocked_by' ~ '^[1-9][0-9]{0,17}$'
       THEN (ev.payload->>'blocked_by')::bigint
     END
 AND p.user_id = $1
WHERE m.state IN ('blocked_spec','awaiting_approval','review_feedback','blocked_dep','failed')
  AND (m.state <> 'failed' OR m.id = m.newest)`

// InboxTaskCandidates 返回 userID 名下处于等人状态的任务候选行。
func (s *Store) InboxTaskCandidates(ctx context.Context, userID int64) ([]InboxTaskRow, error) {
	rows, err := s.pool.Query(ctx, inboxTaskCandidatesSQL, userID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询收件箱任务候选失败: %w", err)
	}
	defer rows.Close()

	var out []InboxTaskRow
	for rows.Next() {
		var (
			r       InboxTaskRow
			payload []byte
		)
		if err := rows.Scan(&r.ID, &r.State, &r.ExternalKey, &r.TrackerProvider,
			&r.FailureReason, &r.FailureStage, &r.ProviderRepo, &r.IssueID, &r.IssueTitle,
			&r.EventID, &r.Since, &payload, &r.PredecessorID); err != nil {
			return nil, fmt.Errorf("store: 读取收件箱任务候选失败: %w", err)
		}
		if len(payload) > 0 {
			// payload 是展示用的辅助信息：解不开就当没有，不让一行脏数据拖垮整页
			_ = json.Unmarshal(payload, &r.Payload)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InboxPRDCandidates 返回 userID 名下的活 PRD 候选行。
//
// 只取 drafting / awaiting_answers / ready_for_review：其余状态是终态或在等
// 系统，不进收件箱。活 PRD 的数量就是人正在推进的数量，带 document 也不大。
func (s *Store) InboxPRDCandidates(ctx context.Context, userID int64) ([]InboxPRDRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.id, p.state, p.round, p.processing, p.document, p.last_error,
		       p.original_input, r.provider_repo, p.updated_at
		FROM prds p
		JOIN repos r ON r.id = p.repo_id
		WHERE p.user_id = $1 AND p.state IN ('drafting','awaiting_answers','ready_for_review')`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询收件箱 PRD 候选失败: %w", err)
	}
	defer rows.Close()

	var out []InboxPRDRow
	for rows.Next() {
		var r InboxPRDRow
		if err := rows.Scan(&r.ID, &r.State, &r.Round, &r.Processing, &r.Document,
			&r.LastError, &r.OriginalInput, &r.ProviderRepo, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: 读取收件箱 PRD 候选失败: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InboxMarks 返回 userID 的全部标记，按 item_key 索引。
func (s *Store) InboxMarks(ctx context.Context, userID int64) (map[string]InboxMark, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT item_key, read_at, dismissed_at FROM inbox_marks WHERE user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询收件箱标记失败: %w", err)
	}
	defer rows.Close()

	out := map[string]InboxMark{}
	for rows.Next() {
		var m InboxMark
		if err := rows.Scan(&m.Key, &m.ReadAt, &m.DismissedAt); err != nil {
			return nil, fmt.Errorf("store: 读取收件箱标记失败: %w", err)
		}
		out[m.Key] = m
	}
	return out, rows.Err()
}

// subjectArgs 把主体拆成 (task_id, prd_id) 两个可空参数。
func subjectArgs(subj InboxSubject) (taskID, prdID *int64, err error) {
	switch {
	case subj.TaskID > 0 && subj.PRDID == 0:
		v := subj.TaskID
		return &v, nil, nil
	case subj.PRDID > 0 && subj.TaskID == 0:
		v := subj.PRDID
		return nil, &v, nil
	}
	return nil, nil, fmt.Errorf("store: 收件箱标记的主体必须是任务或 PRD 二选一")
}

// pruneInboxMarks 剪掉同一用户、同一主体、键不同的旧标记：旧键对应的停留
// 已经结束，不会再显示。每人每个主体因此至多一行。
func pruneInboxMarks(ctx context.Context, tx pgx.Tx, userID int64, key string, taskID, prdID *int64) error {
	if _, err := tx.Exec(ctx, `
		DELETE FROM inbox_marks
		WHERE user_id = $1 AND item_key <> $2
		  AND (($3::bigint IS NOT NULL AND task_id = $3) OR ($4::bigint IS NOT NULL AND prd_id = $4))`,
		userID, key, taskID, prdID); err != nil {
		return fmt.Errorf("store: 剪除收件箱旧标记失败: %w", err)
	}
	return nil
}

// UpsertInboxMark 对一条事项落一次标记，并在同一事务里剪掉同一主体的旧键标记。
//
// 并发安全：INSERT … ON CONFLICT DO UPDATE，多个标签页/节点同时点已读最终
// 只有一行。已读/忽略的时刻取第一次（COALESCE），重复点击不推后。
func (s *Store) UpsertInboxMark(ctx context.Context, userID int64, key string, subj InboxSubject, op InboxMarkOp) error {
	taskID, prdID, err := subjectArgs(subj)
	if err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	switch op {
	case InboxMarkRead:
		_, err = tx.Exec(ctx, `
			INSERT INTO inbox_marks (user_id, item_key, task_id, prd_id, read_at)
			VALUES ($1, $2, $3, $4, now())
			ON CONFLICT (user_id, item_key) DO UPDATE
			  SET read_at = COALESCE(inbox_marks.read_at, now())`,
			userID, key, taskID, prdID)
	case InboxMarkDismiss:
		_, err = tx.Exec(ctx, `
			INSERT INTO inbox_marks (user_id, item_key, task_id, prd_id, dismissed_at)
			VALUES ($1, $2, $3, $4, now())
			ON CONFLICT (user_id, item_key) DO UPDATE
			  SET dismissed_at = COALESCE(inbox_marks.dismissed_at, now())`,
			userID, key, taskID, prdID)
	case InboxMarkUnread:
		// 未忽略：删行（CHECK 不许留下既没读也没忽略的行）；已忽略：只清 read_at。
		if _, err = tx.Exec(ctx, `
			DELETE FROM inbox_marks
			WHERE user_id = $1 AND item_key = $2 AND dismissed_at IS NULL`, userID, key); err == nil {
			_, err = tx.Exec(ctx, `
				UPDATE inbox_marks SET read_at = NULL
				WHERE user_id = $1 AND item_key = $2 AND dismissed_at IS NOT NULL`, userID, key)
		}
	default:
		return fmt.Errorf("store: 未知的收件箱标记操作 %q", op)
	}
	if err != nil {
		return fmt.Errorf("store: 写收件箱标记失败: %w", err)
	}
	if err := pruneInboxMarks(ctx, tx, userID, key, taskID, prdID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: 提交收件箱标记失败: %w", err)
	}
	return nil
}

// MarkInboxRead 把一批事项标为已读（「全部已读」），一条 INSERT … SELECT unnest。
func (s *Store) MarkInboxRead(ctx context.Context, userID int64, items []InboxKeySubject) error {
	if len(items) == 0 {
		return nil
	}
	keys := make([]string, 0, len(items))
	taskIDs := make([]*int64, 0, len(items))
	prdIDs := make([]*int64, 0, len(items))
	for _, it := range items {
		tid, pid, err := subjectArgs(it.Subject)
		if err != nil {
			return err
		}
		keys = append(keys, it.Key)
		taskIDs = append(taskIDs, tid)
		prdIDs = append(prdIDs, pid)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO inbox_marks (user_id, item_key, task_id, prd_id, read_at)
		SELECT $1, x.k, x.t, x.p, now()
		FROM unnest($2::text[], $3::bigint[], $4::bigint[]) AS x(k, t, p)
		ON CONFLICT (user_id, item_key) DO UPDATE
		  SET read_at = COALESCE(inbox_marks.read_at, now())`,
		userID, keys, taskIDs, prdIDs); err != nil {
		return fmt.Errorf("store: 批量标记收件箱已读失败: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM inbox_marks m
		USING unnest($2::text[], $3::bigint[], $4::bigint[]) AS x(k, t, p)
		WHERE m.user_id = $1 AND m.item_key <> x.k
		  AND ((x.t IS NOT NULL AND m.task_id = x.t) OR (x.p IS NOT NULL AND m.prd_id = x.p))`,
		userID, keys, taskIDs, prdIDs); err != nil {
		return fmt.Errorf("store: 剪除收件箱旧标记失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: 提交收件箱标记失败: %w", err)
	}
	return nil
}

// InboxSubjectOwned 报告主体是否存在且属于 userID。
//
// 「不存在」与「属于别人」都返回 false：调用方据此回同一个 404，
// 不对非属主暴露存在性。
func (s *Store) InboxSubjectOwned(ctx context.Context, userID int64, subj InboxSubject) (bool, error) {
	var ok bool
	var err error
	switch {
	case subj.TaskID > 0:
		err = s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM tasks WHERE id = $1 AND user_id = $2)`, subj.TaskID, userID).Scan(&ok)
	case subj.PRDID > 0:
		err = s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM prds WHERE id = $1 AND user_id = $2)`, subj.PRDID, userID).Scan(&ok)
	default:
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: 查询收件箱主体归属失败: %w", err)
	}
	return ok, nil
}
