package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrRepoNotFound 表示仓库不存在或不属于当前用户（两者刻意不分）。
var ErrRepoNotFound = errors.New("store: 仓库不存在")

// ErrRepoExists 表示该用户名下已登记过这个仓库。
var ErrRepoExists = errors.New("store: 该仓库已在你的配置中")

// TaskRow 是任务列表里的一行（含展示所需的关联字段）。
type TaskRow struct {
	ID             int64      `json:"id"`
	UserID         int64      `json:"userId"`
	LinearIssueKey string     `json:"linearIssueKey"`
	State          string     `json:"state"`
	TaskKind       *string    `json:"taskKind"`
	VerifyTier     *string    `json:"verifyTier"`
	BranchName     *string    `json:"branchName"`
	PRURL          *string    `json:"prUrl"`
	FailureReason  *string    `json:"failureReason"`
	WorktreePath   *string    `json:"worktreePath"`
	ProviderRepo   string     `json:"providerRepo"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	AgentSessionID *string    `json:"agentSessionId"`
	LeaseExpiresAt *time.Time `json:"leaseExpiresAt"`
}

// ListTasksParams 是任务列表的过滤条件。
type ListTasksParams struct {
	// UserID 是数据隔离的边界（P1.5 第二步）：只能看到「自己名下」的任务。
	// 调用方从登录身份取值，没有任何「看全部」的旁路 —— 管理员排障走
	// 用户管理页的计数或数据库，不在产品里留跨用户读取的后门。
	UserID int64
	// States 为空表示不过滤。
	States []string
	Limit  int
	Offset int
}

// ListTasks 按更新时间倒序返回指定用户名下的任务列表。
func (s *Store) ListTasks(ctx context.Context, p ListTasksParams) ([]TaskRow, int, error) {
	if p.Limit <= 0 || p.Limit > 200 {
		p.Limit = 50
	}
	if p.Offset < 0 {
		p.Offset = 0
	}

	// 空数组在 SQL 里用 NULL 表示"不过滤"，避免拼接动态 SQL
	var states any
	if len(p.States) > 0 {
		states = p.States
	}

	var total int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM tasks
		WHERE user_id = $1 AND ($2::text[] IS NULL OR state = ANY($2))`,
		p.UserID, states).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: 统计任务数失败: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.user_id, t.linear_issue_key, t.state, t.task_kind, t.verify_tier,
		       t.branch_name, t.pr_url, t.failure_reason, t.worktree_path,
		       r.provider_repo, t.created_at, t.updated_at,
		       t.agent_session_id, t.lease_expires_at
		FROM tasks t
		JOIN repos r ON r.id = t.repo_id
		WHERE t.user_id = $1 AND ($2::text[] IS NULL OR t.state = ANY($2))
		ORDER BY t.updated_at DESC
		LIMIT $3 OFFSET $4`, p.UserID, states, p.Limit, p.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("store: 查询任务列表失败: %w", err)
	}
	defer rows.Close()

	out := make([]TaskRow, 0, p.Limit)
	for rows.Next() {
		var t TaskRow
		if err := rows.Scan(
			&t.ID, &t.UserID, &t.LinearIssueKey, &t.State, &t.TaskKind, &t.VerifyTier,
			&t.BranchName, &t.PRURL, &t.FailureReason, &t.WorktreePath,
			&t.ProviderRepo, &t.CreatedAt, &t.UpdatedAt,
			&t.AgentSessionID, &t.LeaseExpiresAt,
		); err != nil {
			return nil, 0, fmt.Errorf("store: 读取任务行失败: %w", err)
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// EventRow 是状态轨迹里的一条事件。
type EventRow struct {
	ID        int64          `json:"id"`
	FromState *string        `json:"fromState"`
	ToState   string         `json:"toState"`
	Actor     string         `json:"actor"`
	Payload   map[string]any `json:"payload"`
	At        time.Time      `json:"at"`
}

// VerificationRow 是一条验证步骤结果。
type VerificationRow struct {
	ID         int64     `json:"id"`
	Tier       string    `json:"tier"`
	Step       string    `json:"step"`
	Status     string    `json:"status"`
	DurationMS *int64    `json:"durationMs"`
	LogRef     *string   `json:"logRef"`
	At         time.Time `json:"at"`
}

// TaskDetail 是任务详情页所需的全部数据。
type TaskDetail struct {
	Task          TaskRow           `json:"task"`
	Events        []EventRow        `json:"events"`
	Verifications []VerificationRow `json:"verifications"`
}

// TaskDetail 读取单个任务的完整信息。
//
// userID 是隔离边界：任务不属于该用户时返回错误，API 层映射成 404
// —— 对非属主隐瞒任务的存在，不用 403 暴露「有这个任务但不是你的」。
func (s *Store) TaskDetail(ctx context.Context, id, userID int64) (*TaskDetail, error) {
	var t TaskRow
	err := s.pool.QueryRow(ctx, `
		SELECT t.id, t.user_id, t.linear_issue_key, t.state, t.task_kind, t.verify_tier,
		       t.branch_name, t.pr_url, t.failure_reason, t.worktree_path,
		       r.provider_repo, t.created_at, t.updated_at,
		       t.agent_session_id, t.lease_expires_at
		FROM tasks t JOIN repos r ON r.id = t.repo_id
		WHERE t.id = $1 AND t.user_id = $2`, id, userID,
	).Scan(
		&t.ID, &t.UserID, &t.LinearIssueKey, &t.State, &t.TaskKind, &t.VerifyTier,
		&t.BranchName, &t.PRURL, &t.FailureReason, &t.WorktreePath,
		&t.ProviderRepo, &t.CreatedAt, &t.UpdatedAt,
		&t.AgentSessionID, &t.LeaseExpiresAt,
	)
	if err != nil {
		return nil, fmt.Errorf("store: 读取任务 %d 失败: %w", id, err)
	}

	detail := &TaskDetail{Task: t, Events: []EventRow{}, Verifications: []VerificationRow{}}

	evRows, err := s.pool.Query(ctx, `
		SELECT id, from_state, to_state, actor, payload, at
		FROM task_events WHERE task_id = $1 ORDER BY at, id`, id)
	if err != nil {
		return nil, fmt.Errorf("store: 读取状态轨迹失败: %w", err)
	}
	defer evRows.Close()
	for evRows.Next() {
		var e EventRow
		if err := evRows.Scan(&e.ID, &e.FromState, &e.ToState, &e.Actor, &e.Payload, &e.At); err != nil {
			return nil, fmt.Errorf("store: 读取事件失败: %w", err)
		}
		detail.Events = append(detail.Events, e)
	}
	if err := evRows.Err(); err != nil {
		return nil, err
	}

	vRows, err := s.pool.Query(ctx, `
		SELECT id, tier, step, status, duration_ms, log_ref, at
		FROM verifications WHERE task_id = $1 ORDER BY at, id`, id)
	if err != nil {
		return nil, fmt.Errorf("store: 读取验证结果失败: %w", err)
	}
	defer vRows.Close()
	for vRows.Next() {
		var v VerificationRow
		if err := vRows.Scan(&v.ID, &v.Tier, &v.Step, &v.Status, &v.DurationMS, &v.LogRef, &v.At); err != nil {
			return nil, fmt.Errorf("store: 读取验证行失败: %w", err)
		}
		detail.Verifications = append(detail.Verifications, v)
	}
	return detail, vRows.Err()
}

// Stats 是看板顶部的汇总指标。
type Stats struct {
	ByState map[string]int `json:"byState"`
	Total   int            `json:"total"`
	// Active 是仍在流转中的任务数（非终态）。
	Active int `json:"active"`
	// SuccessRate 是已终结任务里走到 pr_open/merged 的比例，无终结任务时为 -1。
	SuccessRate float64 `json:"successRate"`
}

// Stats 汇总指定用户名下的任务状态分布。
func (s *Store) Stats(ctx context.Context, userID int64) (*Stats, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT state, count(*) FROM tasks WHERE user_id = $1 GROUP BY state`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: 统计任务状态失败: %w", err)
	}
	defer rows.Close()

	st := &Stats{ByState: map[string]int{}, SuccessRate: -1}
	terminal := map[string]bool{"merged": true, "failed": true, "cancelled": true}
	var settled, succeeded int

	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, fmt.Errorf("store: 读取状态统计失败: %w", err)
		}
		st.ByState[state] = n
		st.Total += n
		if terminal[state] {
			settled += n
			if state == "merged" {
				succeeded += n
			}
		} else {
			st.Active += n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// pr_open 已产出可合并的结果，计入成功
	if n, ok := st.ByState["pr_open"]; ok {
		settled += n
		succeeded += n
		st.Active -= n
	}
	if settled > 0 {
		st.SuccessRate = float64(succeeded) / float64(settled)
	}
	return st, nil
}

// RepoRow 是仓库配置。
type RepoRow struct {
	ID                int64    `json:"id"`
	ProviderRepo      string   `json:"providerRepo"`
	DefaultBranch     string   `json:"defaultBranch"`
	HotfixBase        string   `json:"hotfixBase"`
	ProtectedBranches []string `json:"protectedBranches"`
	BranchPattern     string   `json:"branchPattern"`
	DepStrategy       string   `json:"depStrategy"`
	GateMode          string   `json:"gateMode"`
	// ExcludeDirs 是验证扫描要跳过的目录（相对根路径或纯目录名），
	// 如停止维护的 apps/console。空切片表示只用内置默认排除。
	ExcludeDirs []string `json:"excludeDirs"`
	// VerifyTierOverride 强制验证档位；空串表示按 §5.1 自动判定
	VerifyTierOverride string `json:"verifyTierOverride"`
}

// ListRepos 返回指定用户名下的仓库配置。
func (s *Store) ListRepos(ctx context.Context, userID int64) ([]RepoRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, provider_repo, default_branch, hotfix_base,
		       protected_branches, branch_pattern, dep_strategy, gate_mode,
		       exclude_dirs, COALESCE(verify_tier_override, '')
		FROM repos WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询仓库列表失败: %w", err)
	}
	defer rows.Close()

	out := []RepoRow{}
	for rows.Next() {
		var r RepoRow
		if err := rows.Scan(&r.ID, &r.ProviderRepo, &r.DefaultBranch, &r.HotfixBase,
			&r.ProtectedBranches, &r.BranchPattern, &r.DepStrategy, &r.GateMode,
			&r.ExcludeDirs, &r.VerifyTierOverride); err != nil {
			return nil, fmt.Errorf("store: 读取仓库行失败: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateRepoParams 是可通过界面修改的仓库配置项。
type UpdateRepoParams struct {
	DefaultBranch     string
	HotfixBase        string
	ProtectedBranches []string
	BranchPattern     string
	GateMode          string
	// ExcludeDirs 为 nil 表示不修改；非 nil（含空切片）表示整体替换 ——
	// 空数组是"清回默认排除"的合法取值，不能走 nilIfEmpty 惯例。
	ExcludeDirs []string
	// VerifyTierOverride 是"light"|"heavy"|""（自动）。注意空串语义：
	// 该字段允许被显式清空回自动档，因此不能用 NULLIF COALESCE 的
	// "空即不动"惯例 —— 调用方传 nil 表示不修改。
	VerifyTierOverride *string
}

// UpdateRepo 更新仓库配置。
//
// userID 是隔离边界：WHERE 同时限定 id 与属主，改别人的仓库会得到
// ErrRepoNotFound —— 与 TaskDetail 一样，对非属主隐瞒存在。
func (s *Store) UpdateRepo(ctx context.Context, id, userID int64, p UpdateRepoParams) (*RepoRow, error) {
	var r RepoRow
	err := s.pool.QueryRow(ctx, `
		UPDATE repos SET
			default_branch     = COALESCE(NULLIF($3,''), default_branch),
			hotfix_base        = COALESCE(NULLIF($4,''), hotfix_base),
			protected_branches = COALESCE($5, protected_branches),
			branch_pattern     = COALESCE(NULLIF($6,''), branch_pattern),
			gate_mode          = COALESCE(NULLIF($7,''), gate_mode),
			exclude_dirs       = COALESCE($8, exclude_dirs),
			verify_tier_override = CASE
				WHEN $9::text IS NULL THEN verify_tier_override
				WHEN $9::text = '' THEN NULL
				ELSE $9::text
			END
		WHERE id = $1 AND user_id = $2
		RETURNING id, provider_repo, default_branch, hotfix_base,
		          protected_branches, branch_pattern, dep_strategy, gate_mode,
		          exclude_dirs, COALESCE(verify_tier_override, '')`,
		id, userID, p.DefaultBranch, p.HotfixBase, nilIfEmpty(p.ProtectedBranches), p.BranchPattern, p.GateMode,
		p.ExcludeDirs, p.VerifyTierOverride,
	).Scan(&r.ID, &r.ProviderRepo, &r.DefaultBranch, &r.HotfixBase,
		&r.ProtectedBranches, &r.BranchPattern, &r.DepStrategy, &r.GateMode,
		&r.ExcludeDirs, &r.VerifyTierOverride)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRepoNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 更新仓库 %d 失败: %w", id, err)
	}
	return &r, nil
}

// CreateRepoParams 是新建仓库配置的必填项。
type CreateRepoParams struct {
	ProviderRepo  string
	DefaultBranch string
	HotfixBase    string
}

// CreateRepo 给用户登记一个仓库配置。
//
// 数据隔离之后这是新用户的必经入口 —— 第一步可以靠管理员手工 INSERT
// 顶过去，各归各之后没人能替你把仓库配到你的名下。分叉基线与 hotfix
// 基线取默认值 dev/main，之后可以在界面上改。
func (s *Store) CreateRepo(ctx context.Context, userID int64, p CreateRepoParams) (*RepoRow, error) {
	var r RepoRow
	err := s.pool.QueryRow(ctx, `
		INSERT INTO repos (user_id, provider_repo, default_branch, hotfix_base)
		VALUES ($1, $2,
		        COALESCE(NULLIF($3,''), 'dev'),
		        COALESCE(NULLIF($4,''), 'main'))
		RETURNING id, provider_repo, default_branch, hotfix_base,
		          protected_branches, branch_pattern, dep_strategy, gate_mode,
		          COALESCE(verify_tier_override, '')`,
		userID, p.ProviderRepo, p.DefaultBranch, p.HotfixBase,
	).Scan(&r.ID, &r.ProviderRepo, &r.DefaultBranch, &r.HotfixBase,
		&r.ProtectedBranches, &r.BranchPattern, &r.DepStrategy, &r.GateMode,
		&r.VerifyTierOverride)
	if err != nil {
		var pgErr *pgconn.PgError
		// 23505 = unique_violation，这里只可能是 (user_id, provider_repo)
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrRepoExists
		}
		return nil, fmt.Errorf("store: 创建仓库配置失败: %w", err)
	}
	return &r, nil
}

func nilIfEmpty(s []string) any {
	if len(s) == 0 {
		return nil
	}
	return s
}
