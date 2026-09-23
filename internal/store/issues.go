package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// 内置工单体系（docs/09-internal-issues.md）：需求的手动载体 ——
// 标题/描述/评论/状态都活在 Lathe 自己的库里，与 Linear 工单同构，
// pipeline 通过 tracker.Tracker 窄接口消费，感知不到差别。

// 工单状态取值域（与 migration 0021 的 issues_state_check 一致）。
const (
	IssueOpen       = "open"
	IssueInProgress = "in_progress"
	IssueDone       = "done"
	IssueCancelled  = "cancelled"
)

// issueTransitions 是人工改态的合法转移表。
//
// 刻意比任务状态机宽松：工单是"需求的便签"，人有权来回摆弄
// （done  reopen、cancelled  reopen 都允许）；真正严格的流转
// 在任务状态机上。自动联动不走这张表，走 ApplyTaskOutcome 的
// 单调规则（人改过的不被自动覆盖）。
var issueTransitions = map[string][]string{
	IssueOpen:       {IssueInProgress, IssueCancelled},
	IssueInProgress: {IssueOpen, IssueDone, IssueCancelled},
	IssueDone:       {IssueOpen},
	IssueCancelled:  {IssueOpen},
}

// ErrIssueNotFound 表示工单不存在或不属于当前用户（两者刻意不分，
// 与 ErrRepoNotFound 同一原则：不对非属主暴露存在）。
var ErrIssueNotFound = errors.New("store: 工单不存在")

// ErrIssueHasTasks 表示工单已有关联任务（含历史终态），只能取消不能删
// —— 任务是审计留痕，删工单会让任务行变成没有需求载体的孤儿。
var ErrIssueHasTasks = errors.New("store: 工单已有关联任务，只能取消不能删除")

// ErrIssueTransition 表示非法的工单状态转移。
type ErrIssueTransition struct{ From, To string }

func (e ErrIssueTransition) Error() string {
	return fmt.Sprintf("store: 工单状态不能从 %s 改为 %s", e.From, e.To)
}

// IssueRow 是 issues 表的一行（含 JSON 标签，直接供 API 层返回）。
type IssueRow struct {
	ID          int64     `json:"id"`
	UserID      int64     `json:"userId"`
	RepoID      int64     `json:"repoId"`
	Key         string    `json:"key"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	State       string    `json:"state"`
	Priority    int       `json:"priority"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// IssueCommentRow 是 issue_comments 表的一行。
// Author 二选一（migration 0021 的 CHECK）：UserID 非空 = 人，
// Actor 非空 = agent/系统（'task-<id>'，UI 渲染「lathe · 任务 #id」）。
type IssueCommentRow struct {
	ID        int64     `json:"id"`
	IssueID   int64     `json:"issueId"`
	UserID    *int64    `json:"userId,omitempty"`
	Actor     *string   `json:"actor,omitempty"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	// AuthorName 是展示用名字：人的 email / agent 的 actor 标记。
	// 非表列，查询时 join 得出。
	AuthorName string `json:"authorName"`
}

// CreateIssueParams 是建工单的输入。Key 由库内全局序列生成（LT-<n>）。
type CreateIssueParams struct {
	UserID      int64
	RepoID      int64
	Title       string
	Description string
	Priority    int
}

// CreateIssue 建工单。仓库归属必须已经由调用方校验过（RepoID 属于
// UserID）；这里只靠外键兜底，不重复查。
func (s *Store) CreateIssue(ctx context.Context, p CreateIssueParams) (*IssueRow, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO issues (user_id, repo_id, key, title, description, priority)
		VALUES ($1, $2, 'LT-' || nextval('issue_key_seq'), $3, $4, $5)
		RETURNING id, user_id, repo_id, key, title, description, state, priority, created_at, updated_at`,
		p.UserID, p.RepoID, p.Title, p.Description, p.Priority)
	return scanIssue(row)
}

// GetIssue 按 id 读工单；非属主一律 ErrIssueNotFound（404 语义）。
func (s *Store) GetIssue(ctx context.Context, id, userID int64) (*IssueRow, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, user_id, repo_id, key, title, description, state, priority, created_at, updated_at
		FROM issues WHERE id = $1 AND user_id = $2`, id, userID)
	return scanIssue(row)
}

// GetIssueByKey 按 (属主, key) 读工单 —— 内置 tracker 的定位路径
// （tasks.external_id 对内置任务为 NULL，key 就是全部身份）。
func (s *Store) GetIssueByKey(ctx context.Context, userID int64, key string) (*IssueRow, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, user_id, repo_id, key, title, description, state, priority, created_at, updated_at
		FROM issues WHERE user_id = $1 AND key = $2`, userID, key)
	return scanIssue(row)
}

func scanIssue(row interface{ Scan(...any) error }) (*IssueRow, error) {
	var it IssueRow
	if err := row.Scan(&it.ID, &it.UserID, &it.RepoID, &it.Key, &it.Title,
		&it.Description, &it.State, &it.Priority, &it.CreatedAt, &it.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrIssueNotFound
		}
		return nil, fmt.Errorf("store: 读取工单失败: %w", err)
	}
	return &it, nil
}

// ListIssuesParams 是工单列表的过滤条件。
type ListIssuesParams struct {
	UserID int64
	// State 为空串表示不过滤。
	State  string
	Limit  int
	Offset int
}

// ListIssues 按更新时间倒序返回属主名下的工单。
func (s *Store) ListIssues(ctx context.Context, p ListIssuesParams) ([]IssueRow, int, error) {
	if p.Limit <= 0 || p.Limit > 200 {
		p.Limit = 50
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	var state any
	if p.State != "" {
		state = p.State
	}

	var total int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM issues
		WHERE user_id = $1 AND ($2::text IS NULL OR state = $2)`,
		p.UserID, state).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: 统计工单数失败: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, repo_id, key, title, description, state, priority, created_at, updated_at
		FROM issues
		WHERE user_id = $1 AND ($2::text IS NULL OR state = $2)
		ORDER BY updated_at DESC
		LIMIT $3 OFFSET $4`, p.UserID, state, p.Limit, p.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("store: 查询工单列表失败: %w", err)
	}
	defer rows.Close()

	out := make([]IssueRow, 0, p.Limit)
	for rows.Next() {
		var it IssueRow
		if err := rows.Scan(&it.ID, &it.UserID, &it.RepoID, &it.Key, &it.Title,
			&it.Description, &it.State, &it.Priority, &it.CreatedAt, &it.UpdatedAt); err != nil {
			return nil, 0, fmt.Errorf("store: 读取工单行失败: %w", err)
		}
		out = append(out, it)
	}
	return out, total, rows.Err()
}

// UpdateIssueParams 是工单编辑的输入；指针为 nil 的字段不动。
type UpdateIssueParams struct {
	Title       *string
	Description *string
	Priority    *int
	State       *string
}

// UpdateIssue 编辑工单。State 非 nil 时校验转移合法性（人工改态走
// issueTransitions；自动联动不走这里，走 ApplyTaskOutcome）。
func (s *Store) UpdateIssue(ctx context.Context, id, userID int64, p UpdateIssueParams) (*IssueRow, error) {
	if p.State != nil {
		cur, err := s.GetIssue(ctx, id, userID)
		if err != nil {
			return nil, err
		}
		if !issueTransitionOK(cur.State, *p.State) {
			return nil, ErrIssueTransition{From: cur.State, To: *p.State}
		}
	}
	row := s.pool.QueryRow(ctx, `
		UPDATE issues SET
		  title       = COALESCE($3, title),
		  description = COALESCE($4, description),
		  priority    = COALESCE($5, priority),
		  state       = COALESCE($6, state)
		WHERE id = $1 AND user_id = $2
		RETURNING id, user_id, repo_id, key, title, description, state, priority, created_at, updated_at`,
		id, userID, p.Title, p.Description, p.Priority, p.State)
	return scanIssue(row)
}

func issueTransitionOK(from, to string) bool {
	if from == to {
		return true
	}
	for _, t := range issueTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// TaskOutcome 是任务生命周期对工单的三种自动联动（09 §3 F4-AC3）。
type TaskOutcome string

const (
	// OutcomeStarted 任务建出：open/done → in_progress
	// （done 重开跑意味着"其实没做完"）。
	OutcomeStarted TaskOutcome = "started"
	// OutcomeMerged 任务 merged：→ done（人手动 cancelled 的不动）。
	OutcomeMerged TaskOutcome = "merged"
	// OutcomeTaskCancelled 任务被取消：in_progress → open。
	OutcomeTaskCancelled TaskOutcome = "task_cancelled"
)

// ApplyTaskOutcome 把任务生命周期联动应用到工单上。
//
// 单调且宽容：人的手动改态优先 —— 联动只在这三个方向发生，
// 当前状态不在联动起点集合里时静默不动（返回 false, nil），不报错：
// 联动是"帮人省事"，不是"替人做主"。返回是否真正改了状态。
func (s *Store) ApplyTaskOutcome(ctx context.Context, userID int64, key string, oc TaskOutcome) (bool, error) {
	var q string
	switch oc {
	case OutcomeStarted:
		// open/done → in_progress（done 重开跑意味着"其实没做完"）
		q = `UPDATE issues SET state = $3 WHERE user_id = $1 AND key = $2 AND state IN ('open','done')`
	case OutcomeMerged:
		// → done（人手动 cancelled 的不动）
		q = `UPDATE issues SET state = $3 WHERE user_id = $1 AND key = $2 AND state IN ('open','in_progress')`
	case OutcomeTaskCancelled:
		// in_progress → open
		q = `UPDATE issues SET state = $3 WHERE user_id = $1 AND key = $2 AND state = 'in_progress'`
	default:
		return false, fmt.Errorf("store: 未知任务联动 %q", oc)
	}
	tag, err := s.pool.Exec(ctx, q, userID, key, outcomeTarget(oc))
	if err != nil {
		return false, fmt.Errorf("store: 工单联动失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// outcomeTarget 给出每种联动要写入的目标状态。
func outcomeTarget(oc TaskOutcome) string {
	switch oc {
	case OutcomeStarted:
		return IssueInProgress
	case OutcomeMerged:
		return IssueDone
	case OutcomeTaskCancelled:
		return IssueOpen
	}
	return ""
}

// DeleteIssue 删除工单。有关联任务（含历史终态）时报 ErrIssueHasTasks
// —— 只能取消不能删（F1-AC5）。评论随 CASCADE 一并删除。
func (s *Store) DeleteIssue(ctx context.Context, id, userID int64) error {
	it, err := s.GetIssue(ctx, id, userID)
	if err != nil {
		return err
	}
	var n int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM tasks
		WHERE tracker_provider = 'internal' AND external_key = $1`, it.Key,
	).Scan(&n); err != nil {
		return fmt.Errorf("store: 检查工单关联任务失败: %w", err)
	}
	if n > 0 {
		return ErrIssueHasTasks
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM issues WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("store: 删除工单失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrIssueNotFound
	}
	return nil
}

// AddComment 在工单下追加评论。userID 与 actor 必须且只能给一个
// （schema 的 CHECK 也这么约束；这里提前拦，错误信息说人话）。
func (s *Store) AddComment(ctx context.Context, issueID int64, userID *int64, actor *string, body string) (*IssueCommentRow, error) {
	if (userID == nil) == (actor == nil) {
		return nil, fmt.Errorf("store: 评论作者必须且只能给一个（user_id 或 actor）")
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO issue_comments (issue_id, user_id, actor, body)
		VALUES ($1, $2, $3, $4)
		RETURNING id, issue_id, user_id, actor, body, created_at`,
		issueID, userID, actor, body)
	var c IssueCommentRow
	if err := row.Scan(&c.ID, &c.IssueID, &c.UserID, &c.Actor, &c.Body, &c.CreatedAt); err != nil {
		return nil, fmt.Errorf("store: 写入评论失败: %w", err)
	}
	return &c, nil
}

// ListComments 按时间正序返回工单的全部评论（分诊上下文与详情页同源）。
// AuthorName 的口径：人 → users.email；agent → actor 标记本身。
func (s *Store) ListComments(ctx context.Context, issueID int64) ([]IssueCommentRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.id, c.issue_id, c.user_id, c.actor, c.body, c.created_at,
		       COALESCE(u.email, c.actor, '') AS author_name
		FROM issue_comments c
		LEFT JOIN users u ON u.id = c.user_id
		WHERE c.issue_id = $1
		ORDER BY c.id`, issueID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询评论失败: %w", err)
	}
	defer rows.Close()

	out := []IssueCommentRow{}
	for rows.Next() {
		var c IssueCommentRow
		if err := rows.Scan(&c.ID, &c.IssueID, &c.UserID, &c.Actor, &c.Body, &c.CreatedAt, &c.AuthorName); err != nil {
			return nil, fmt.Errorf("store: 读取评论行失败: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// IssueTask 是工单详情页「关联任务」区块的一行（轻量视图）。
type IssueTask struct {
	ID        int64     `json:"id"`
	State     string    `json:"state"`
	Branch    *string   `json:"branchName"`
	PRURL     *string   `json:"prUrl"`
	CreatedAt time.Time `json:"createdAt"`
}

// ListIssueTasks 返回某内置工单名下的全部任务（含历史终态），新的在前。
func (s *Store) ListIssueTasks(ctx context.Context, userID int64, key string) ([]IssueTask, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, state, branch_name, pr_url, created_at
		FROM tasks
		WHERE user_id = $1 AND tracker_provider = 'internal' AND external_key = $2
		ORDER BY id DESC`, userID, key)
	if err != nil {
		return nil, fmt.Errorf("store: 查询工单关联任务失败: %w", err)
	}
	defer rows.Close()

	out := []IssueTask{}
	for rows.Next() {
		var t IssueTask
		if err := rows.Scan(&t.ID, &t.State, &t.Branch, &t.PRURL, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: 读取关联任务行失败: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
