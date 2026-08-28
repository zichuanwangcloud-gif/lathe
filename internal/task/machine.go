package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Task 是 tasks 表的一行。
type Task struct {
	ID             int64
	UserID         int64
	RepoID         int64
	LinearIssueKey string
	// LinearIssueID 是 issue 的 UUID（Linear API 的定位主键）。
	// 重试与启动恢复靠它重新定位 issue —— key 只是给人看的编号。
	// 旧行可能为 NULL（migration 0010 前的数据）。
	LinearIssueID  *string
	State          State
	GateMode       string
	TaskKind       *string
	VerifyTier     *string
	AgentSessionID *string
	WorktreePath   *string
	BranchName     *string
	PRURL          *string
	FailureReason  *string
	// FailureStage 是机器可读的失败阶段代码（runner 包定义），
	// 智能重试的断点续跑决策依据。仅 state=failed 时有意义。
	FailureStage   *string
	NodeID         *int64
	LeaseExpiresAt *time.Time
	// FlowID 标记任务所属的编排图；NULL 表示不属于任何 flow（独立任务）。
	FlowID *int64
	// DependsOn 是前驱任务 ID（自引用）；NULL 表示独立根，无需等待任何前驱。
	DependsOn *int64
	// DependsOnAt 是前驱放行的判定时机：'pr_open'（前驱开出 PR 即放行，
	// 栈式 PR 默认形态）或 'merged'（前驱真合并才放行）。
	DependsOnAt string
	// BaseRef 非空时表示"这是栈式 PR 的后继任务，应从这个分支分叉"，
	// 覆盖 RepoConfig.BaseBranch(kind) 的默认逻辑。
	BaseRef *string
	// Priority 用于就绪集排序（ClaimReady 的 ORDER BY priority DESC），
	// 建图时从平台的 priority 取初值，之后可在 Lathe 里改。
	Priority int
	// Profile 是节点执行画像的原始 jsonb 字节（model_channel/skills/
	// verify_tier/gate_mode/max_fix_attempts/prompt_template 等），
	// task 包不解析其结构，避免耦合具体 schema —— 上层自行 Unmarshal。
	Profile []byte
	// PRNumber 是 GitHub PR 编号，供合并检测的轮询兜底按 (repo, pr_number)
	// 遍历 pr_open 任务查询（webhook 丢失时仍能收敛）。
	PRNumber  *int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Machine 在数据库上执行状态转移，并把每次转移写入 task_events。
type Machine struct {
	pool *pgxpool.Pool
}

// NewMachine 构造状态机。
func NewMachine(pool *pgxpool.Pool) *Machine {
	return &Machine{pool: pool}
}

// ErrTaskNotFound 表示目标任务不存在。
var ErrTaskNotFound = errors.New("task: 任务不存在")

// ErrSessionRequired 表示该转移必须已持有 agent_session_id。
//
// 对应 docs/02-design.md §3 约束①：review 二轮必须 --resume 原会话。
var ErrSessionRequired = errors.New("task: 该转移要求已持有 agent_session_id（review 二轮必须 resume 原会话）")

const taskColumns = `
	id, user_id, repo_id, linear_issue_key, linear_issue_id, state, gate_mode,
	task_kind, verify_tier, agent_session_id, worktree_path,
	branch_name, pr_url, failure_reason, failure_stage, node_id, lease_expires_at,
	flow_id, depends_on, depends_on_at, base_ref, priority, profile, pr_number,
	created_at, updated_at`

// taskColumnsQualified 是 taskColumns 的 tasks. 限定版本，供 ClaimReady 这类
// 联表（FROM candidate）查询使用 —— 不限定会因列名歧义报错。
const taskColumnsQualified = `
	tasks.id, tasks.user_id, tasks.repo_id, tasks.linear_issue_key, tasks.linear_issue_id, tasks.state, tasks.gate_mode,
	tasks.task_kind, tasks.verify_tier, tasks.agent_session_id, tasks.worktree_path,
	tasks.branch_name, tasks.pr_url, tasks.failure_reason, tasks.failure_stage, tasks.node_id, tasks.lease_expires_at,
	tasks.flow_id, tasks.depends_on, tasks.depends_on_at, tasks.base_ref, tasks.priority, tasks.profile, tasks.pr_number,
	tasks.created_at, tasks.updated_at`

// CreateParams 是建任务所需的最小输入。
type CreateParams struct {
	UserID         int64
	RepoID         int64
	LinearIssueKey string
	// LinearIssueID 是 issue 的 UUID；为空时存 NULL（兼容旧调用方）。
	LinearIssueID string
	GateMode      string
	TaskKind      *string
	// FlowID 非 nil 时把任务挂到指定编排图；nil 表示独立任务（NULL）。
	FlowID *int64
	// DependsOn 非 nil 时声明前驱任务（自引用）；nil 表示独立根。
	DependsOn *int64
	// DependsOnAt 是前驱放行时机，空串按 schema 默认值 'pr_open' 处理。
	DependsOnAt string
	// Priority 就绪集排序用；零值即无优先级提升。
	Priority int
	// BaseRef 非 nil 时预置分叉基线（栈式 PR 后继任务）。
	BaseRef *string
	// Profile 是节点执行画像的原始 jsonb 字节（见 Task.Profile 的文档
	// 注释）；nil 时落 schema 默认值 '{}'（SQL 侧 COALESCE 到
	// '{}'::jsonb，不传空字符串——空字符串不是合法 jsonb，会在插入时
	// 报错）。
	Profile []byte
}

// nilIfEmptyJSON 把长度为 0 的 profile 字节（未设置的零值 []byte{}，
// 与"根本没传"的 nil 语义等价）规整为 nil，让 SQL 侧的
// COALESCE($n, '{}'::jsonb) 落到默认值——一段空字节不是合法 jsonb，
// 直接传给 jsonb 列会在插入时报语法错误。
func nilIfEmptyJSON(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

// Create 新建任务（初始状态 queued）并记录创建事件。
//
// 若同一 issue 已有活任务，数据库的部分唯一索引会拒绝插入。
func (m *Machine) Create(ctx context.Context, p CreateParams) (*Task, error) {
	if p.GateMode == "" {
		p.GateMode = "direct"
	}
	dependsOnAt := p.DependsOnAt
	if dependsOnAt == "" {
		dependsOnAt = "pr_open"
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("task: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx, `
		INSERT INTO tasks (
			user_id, repo_id, linear_issue_key, linear_issue_id, state, gate_mode, task_kind,
			flow_id, depends_on, depends_on_at, priority, base_ref, profile
		)
		VALUES ($1, $2, $3, NULLIF($7, ''), $4, $5, $6, $8, $9, $10, $11, $12, COALESCE($13, '{}'::jsonb))
		RETURNING `+taskColumns,
		p.UserID, p.RepoID, p.LinearIssueKey, StateQueued, p.GateMode, p.TaskKind, p.LinearIssueID,
		p.FlowID, p.DependsOn, dependsOnAt, p.Priority, p.BaseRef, nilIfEmptyJSON(p.Profile))

	t, err := scanTask(row)
	if err != nil {
		return nil, fmt.Errorf("task: 创建任务失败: %w", err)
	}

	// from_state 为 NULL 表示"任务创建"，重放时是事件流的起点
	if err := insertEvent(ctx, tx, t.ID, nil, StateQueued, "system",
		map[string]any{"issue": p.LinearIssueKey}); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("task: 提交创建失败: %w", err)
	}
	return t, nil
}

// Get 按 ID 读取任务。
func (m *Machine) Get(ctx context.Context, id int64) (*Task, error) {
	row := m.pool.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = $1`, id)
	t, err := scanTask(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("task: 读取任务 %d 失败: %w", id, err)
	}
	return t, nil
}

// TransitionOpts 携带随转移一起写入的字段更新。
//
// 只有非 nil 的字段会被写入，便于在一次转移里同时记录副产物
// （如进入 implementing 时记下 worktree_path 与 branch_name）。
type TransitionOpts struct {
	AgentSessionID *string
	WorktreePath   *string
	BranchName     *string
	PRURL          *string
	VerifyTier     *string
	TaskKind       *string
	FailureReason  *string
	// FailureStage 随失败转移写入（runner 包的阶段代码）。
	FailureStage   *string
	NodeID         *int64
	LeaseExpiresAt *time.Time
	Payload        map[string]any
}

// Transition 把任务转到 to 状态。
//
// 整个操作在一个事务内完成，并对任务行加行锁（SELECT ... FOR UPDATE），
// 因此并发调用不会出现"两边都读到旧状态各自转移"的竞态。
// 非法转移会被拒绝且不产生任何写入。
func (m *Machine) Transition(ctx context.Context, id int64, to State, actor string, opts *TransitionOpts) (*Task, error) {
	if opts == nil {
		opts = &TransitionOpts{}
	}
	if actor == "" {
		actor = "system"
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("task: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 行锁：拿到当前状态的独占视图
	row := tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = $1 FOR UPDATE`, id)
	cur, err := scanTask(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("task: 锁定任务 %d 失败: %w", id, err)
	}

	if err := Validate(cur.State, to); err != nil {
		return nil, err
	}

	// 约束①：review 二轮必须已有会话 —— 本次转移带上或此前已持有
	if RequiresSession(cur.State, to) {
		has := cur.AgentSessionID != nil && *cur.AgentSessionID != ""
		bringing := opts.AgentSessionID != nil && *opts.AgentSessionID != ""
		if !has && !bringing {
			return nil, ErrSessionRequired
		}
	}

	updated, err := scanTask(tx.QueryRow(ctx, `
		UPDATE tasks SET
			state             = $2,
			agent_session_id  = COALESCE($3,  agent_session_id),
			worktree_path     = COALESCE($4,  worktree_path),
			branch_name       = COALESCE($5,  branch_name),
			pr_url            = COALESCE($6,  pr_url),
			verify_tier       = COALESCE($7,  verify_tier),
			task_kind         = COALESCE($8,  task_kind),
			failure_reason    = COALESCE($9,  failure_reason),
			failure_stage     = COALESCE($10, failure_stage),
			node_id           = COALESCE($11, node_id),
			lease_expires_at  = COALESCE($12, lease_expires_at)
		WHERE id = $1
		RETURNING `+taskColumns,
		id, to,
		opts.AgentSessionID, opts.WorktreePath, opts.BranchName, opts.PRURL,
		opts.VerifyTier, opts.TaskKind, opts.FailureReason, opts.FailureStage,
		opts.NodeID, opts.LeaseExpiresAt,
	))
	if err != nil {
		return nil, fmt.Errorf("task: 更新任务 %d 失败: %w", id, err)
	}

	from := cur.State
	if err := insertEvent(ctx, tx, id, &from, to, actor, opts.Payload); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("task: 提交转移失败: %w", err)
	}
	return updated, nil
}

// SetSessionID 在不改变状态的情况下替换任务的 agent 会话 ID。
//
// 为什么不是转移：断点续跑的降级路径（resume 失败换新会话）与修复回路
// 开新会话都发生在状态不变的中途 —— 这是会话凭据的记账，不是状态变化。
// 崩溃恢复与再次重试都靠这个值 --resume，必须即写即持久。
func (m *Machine) SetSessionID(ctx context.Context, id int64, sessionID string) error {
	tag, err := m.pool.Exec(ctx, `UPDATE tasks SET agent_session_id = $2 WHERE id = $1`, id, sessionID)
	if err != nil {
		return fmt.Errorf("task: 更新会话 ID 失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTaskNotFound
	}
	return nil
}

// SetBaseRef 在不经过状态机的情况下设置任务的分叉基线分支。
//
// 为什么不是转移：base_ref 是调度器在派发前为栈式 PR 后继任务预置的
// 路由信息（RepoConfig.BaseRefOverride 的来源），不代表状态变化，
// 与 SetSessionID 是同一类"字段记账，非状态转移"的操作。传 nil 清空
// （回到"走 RepoConfig.BaseBranch(kind) 原逻辑"）。
func (m *Machine) SetBaseRef(ctx context.Context, id int64, baseRef *string) error {
	tag, err := m.pool.Exec(ctx, `UPDATE tasks SET base_ref = $2 WHERE id = $1`, id, baseRef)
	if err != nil {
		return fmt.Errorf("task: 更新 base_ref 失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTaskNotFound
	}
	return nil
}

// SetPRNumber 在不经过状态机的情况下记下任务对应的 GitHub PR 编号。
//
// 为什么不是转移：PR 编号是 stagePushAndPR 开出 PR 后的字段记账（此前只
// 写进 task_events.payload，不可查询），与 SetBaseRef/SetSessionID 是
// 同一类操作。F4.1 合并检测的轮询兜底靠 tasks.pr_number 才能按
// (repo, pr_number) 遍历 pr_open 任务查 GitHub GetPR。
func (m *Machine) SetPRNumber(ctx context.Context, id int64, prNumber int) error {
	tag, err := m.pool.Exec(ctx, `UPDATE tasks SET pr_number = $2 WHERE id = $1`, id, prNumber)
	if err != nil {
		return fmt.Errorf("task: 更新 pr_number 失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTaskNotFound
	}
	return nil
}

// ListOpenPRTasks 返回所有处于 pr_open 且已记下 PR 编号的任务，按 id
// 排序，供 F4.1 合并检测的轮询兜底遍历。
func (m *Machine) ListOpenPRTasks(ctx context.Context) ([]*Task, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT `+taskColumns+`
		FROM tasks WHERE state = $1 AND pr_number IS NOT NULL
		ORDER BY id`, StatePROpen)
	if err != nil {
		return nil, fmt.Errorf("task: 查询 pr_open 任务失败: %w", err)
	}
	defer rows.Close()

	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("task: 读取 pr_open 任务失败: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// HasLiveDependentOnBranch 报告是否存在非终结状态的任务，其当前
// base_ref 等于 branchName —— F4.2-AC2 现场回收的判定条件：
// "仍有未合并后继依赖该分支时，不删除该分支"。
//
// 判定条件刻意是"当前 base_ref == 这个分支名"，而不是"depends_on 指向
// 这个任务 ID"：后继一旦经过 rebase（F4.3）转而基于 default_branch，
// 它的 base_ref 会被清空，即使 depends_on 结构上仍指向这个已合并的
// 前驱，这个分支也已经不再被它依赖，可以安全删除。本方法只按 base_ref
// 现值判定，不关心它以后会不会变。
func (m *Machine) HasLiveDependentOnBranch(ctx context.Context, branchName string) (bool, error) {
	var exists bool
	err := m.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM tasks
			WHERE base_ref = $1 AND state NOT IN ('merged', 'failed', 'cancelled')
		)`, branchName).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("task: 查询分支 %q 的活依赖失败: %w", branchName, err)
	}
	return exists, nil
}

// TasksWithBaseRef 返回所有 base_ref 当前等于 branchName 且状态非终结的
// 任务，按 id 排序 —— F4.3 rebase 跟进逐个处理"这个分支的直接后继"的
// 判定条件与 HasLiveDependentOnBranch 完全一致，只是这里要拿到完整的
// 任务行而不只是一个布尔值。
//
// 森林结构下入度<=1，正常业务流程只会有一个后继指向同一分支名，但
// 历史或手工原因可能有多个恰好指向同一分支名，调用方按"可能有多个，
// 逐个处理，互不影响"处理，这里不假设恰好一个。
func (m *Machine) TasksWithBaseRef(ctx context.Context, branchName string) ([]*Task, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT `+taskColumns+`
		FROM tasks
		WHERE base_ref = $1 AND state NOT IN ('merged', 'failed', 'cancelled')
		ORDER BY id`, branchName)
	if err != nil {
		return nil, fmt.Errorf("task: 查询分支 %q 的后继任务失败: %w", branchName, err)
	}
	defer rows.Close()

	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("task: 读取分支 %q 的后继任务失败: %w", branchName, err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ClaimReady 原子地挑一个就绪任务并打租约，供 DB 领单调度器轮询使用。
//
// "就绪" = state=queued 且租约未生效（从未打过租约，或租约已过期）
// 且前驱放行：depends_on 为空（独立根），或按 depends_on_at 语义前驱
// 状态已满足（'pr_open' 时前驱处于 pr_open/merged 皆可放行；'merged'
// 时前驱必须已 merged）。挑选与打租约在同一条 SQL 内完成
// （FOR UPDATE OF t SKIP LOCKED），避免"先查后更新"的竞态窗口 ——
// 多个调用者并发调用时，同一任务只会被其中恰好一个领到。
//
// 没有就绪任务时返回 (nil, nil)：这是正常状态（"暂时没活干"），
// 调用方应退避轮询，不应当作错误处理。
//
// 注意：ClaimReady 不改变 state，只推进 lease_expires_at。真正的状态
// 变化仍由调用方随后自己发起（如 pipeline.stageTriage 调
// Transition(id, StateTriaging, ...)）—— state.go 的合法转移表里没有
// queued→queued 这条边，如果 ClaimReady 顺带改 state 会跟调用方自己的
// Transition 调用打架（两次转移，同一次派发）。
func (m *Machine) ClaimReady(ctx context.Context, leaseDuration time.Duration) (*Task, error) {
	row := m.pool.QueryRow(ctx, `
		WITH candidate AS (
			SELECT t.id FROM tasks t
			LEFT JOIN tasks p ON p.id = t.depends_on
			WHERE t.state = 'queued'
			  AND (t.lease_expires_at IS NULL OR t.lease_expires_at < now())
			  AND ( t.depends_on IS NULL
			     OR (t.depends_on_at = 'pr_open' AND p.state IN ('pr_open','merged'))
			     OR (t.depends_on_at = 'merged'  AND p.state = 'merged') )
			ORDER BY t.priority DESC, t.id
			FOR UPDATE OF t SKIP LOCKED
			LIMIT 1
		)
		UPDATE tasks SET lease_expires_at = now() + $1 * interval '1 second'
		FROM candidate WHERE tasks.id = candidate.id
		RETURNING `+taskColumnsQualified,
		leaseDuration.Seconds())

	t, err := scanTask(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("task: 领单失败: %w", err)
	}
	return t, nil
}

// PropagateBlocked 是 F2.3 失败传播的核心：failedTaskID 已处于终结状态
// （调用方保证，本方法不再校验），把 depends_on 链上【所有传递后继】里
// 仍是 queued 的任务转到 blocked_dep。
//
// "传递后继"覆盖间接依赖：1→2→3 这条链，1 失败时 2 和 3 都要处理，
// 哪怕 3 的 depends_on 指向的是 2 而不是 1 —— 用递归 CTE 一次性找全。
//
// 某个后继若已经不是 queued（已在跑、或已到达其它终态），直接跳过、
// 不报错：这是已知的未覆盖场景，pipeline 层的报告会说明。
//
// 返回所有成功转移的 *Task，供调用方（不是本方法的职责）用其
// LinearIssueKey/LinearIssueID 去回帖。
func (m *Machine) PropagateBlocked(ctx context.Context, failedTaskID int64, reason string) ([]*Task, error) {
	rows, err := m.pool.Query(ctx, `
		WITH RECURSIVE descendants AS (
			SELECT id FROM tasks WHERE depends_on = $1
			UNION ALL
			SELECT t.id FROM tasks t JOIN descendants d ON t.depends_on = d.id
		)
		SELECT id FROM descendants`, failedTaskID)
	if err != nil {
		return nil, fmt.Errorf("task: 查询失败传播的后继失败: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("task: 读取后继 ID 失败: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("task: 遍历后继失败: %w", err)
	}

	var blocked []*Task
	for _, id := range ids {
		t, err := m.Transition(ctx, id, StateBlockedDep, "system", &TransitionOpts{
			FailureReason: &reason,
			Payload:       map[string]any{"blocked_by": failedTaskID, "reason": reason},
		})
		if err != nil {
			// 目标已不是 queued（在跑/已终结）：跳过，不是错误。
			var illegal ErrIllegalTransition
			if errors.As(err, &illegal) {
				continue
			}
			return nil, fmt.Errorf("task: 传播阻塞到任务 %d 失败: %w", id, err)
		}
		blocked = append(blocked, t)
	}
	return blocked, nil
}

// WakeBlockedSuccessors 是 F2.3-AC5 的恢复路径：recoveredTaskID 已到达
// 某个"后继可以恢复"的状态（调用方保证），把它【直接】后继
// （depends_on = recoveredTaskID）里状态为 blocked_dep 的任务转回 queued。
//
// 只处理直接后继，不递归：间接后继会在直接后继重新跑完、自身也到达
// 可放行状态时，被同一机制再唤醒一层，天然逐级传播。
func (m *Machine) WakeBlockedSuccessors(ctx context.Context, recoveredTaskID int64) ([]*Task, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT id FROM tasks WHERE depends_on = $1 AND state = $2`,
		recoveredTaskID, StateBlockedDep)
	if err != nil {
		return nil, fmt.Errorf("task: 查询待唤醒的直接后继失败: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("task: 读取待唤醒后继 ID 失败: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("task: 遍历待唤醒后继失败: %w", err)
	}

	var woken []*Task
	for _, id := range ids {
		t, err := m.Transition(ctx, id, StateQueued, "system", &TransitionOpts{
			Payload: map[string]any{"unblocked_by": recoveredTaskID},
		})
		if err != nil {
			// 并发下状态可能已经变化（如人工同时取消了它）：跳过，不是错误。
			var illegal ErrIllegalTransition
			if errors.As(err, &illegal) {
				continue
			}
			return nil, fmt.Errorf("task: 唤醒任务 %d 失败: %w", id, err)
		}
		woken = append(woken, t)
	}
	return woken, nil
}

// Event 是 task_events 表的一行。
type Event struct {
	ID        int64
	TaskID    int64
	FromState *State
	ToState   State
	Actor     string
	Payload   map[string]any
	At        time.Time
}

// Events 按时间升序返回任务的全部事件。
func (m *Machine) Events(ctx context.Context, taskID int64) ([]Event, error) {
	rows, err := m.pool.Query(ctx, `
		SELECT id, task_id, from_state, to_state, actor, payload, at
		FROM task_events WHERE task_id = $1 ORDER BY at, id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("task: 查询事件失败: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var (
			e       Event
			from    *string
			to      string
			payload []byte
		)
		if err := rows.Scan(&e.ID, &e.TaskID, &from, &to, &e.Actor, &payload, &e.At); err != nil {
			return nil, fmt.Errorf("task: 读取事件失败: %w", err)
		}
		e.ToState = State(to)
		if from != nil {
			s := State(*from)
			e.FromState = &s
		}
		if len(payload) > 0 {
			_ = json.Unmarshal(payload, &e.Payload)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Replay 从事件流重算任务的当前状态。
//
// 用途是排障与自检：Replay 的结果必须与 tasks.state 一致；
// 不一致说明有人绕过状态机直接改了表。
func (m *Machine) Replay(ctx context.Context, taskID int64) (State, error) {
	events, err := m.Events(ctx, taskID)
	if err != nil {
		return "", err
	}
	if len(events) == 0 {
		return "", ErrTaskNotFound
	}

	var cur State
	for i, e := range events {
		if i == 0 {
			if e.FromState != nil {
				return "", fmt.Errorf("task: 任务 %d 的首个事件应为创建（from_state 为空），实际为 %s", taskID, *e.FromState)
			}
			cur = e.ToState
			continue
		}
		if e.FromState == nil {
			return "", fmt.Errorf("task: 任务 %d 出现第二个创建事件（id=%d）", taskID, e.ID)
		}
		if *e.FromState != cur {
			return "", fmt.Errorf("task: 任务 %d 事件流断裂：事件 %d 声称从 %s 出发，但此前状态是 %s",
				taskID, e.ID, *e.FromState, cur)
		}
		cur = e.ToState
	}
	return cur, nil
}

func insertEvent(ctx context.Context, tx pgx.Tx, taskID int64, from *State, to State, actor string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("task: 序列化事件 payload 失败: %w", err)
	}
	var fromStr *string
	if from != nil {
		s := string(*from)
		fromStr = &s
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO task_events (task_id, from_state, to_state, actor, payload)
		VALUES ($1, $2, $3, $4, $5)`,
		taskID, fromStr, string(to), actor, raw,
	); err != nil {
		return fmt.Errorf("task: 写入事件失败: %w", err)
	}
	return nil
}

func scanTask(row pgx.Row) (*Task, error) {
	var (
		t     Task
		state string
	)
	err := row.Scan(
		&t.ID, &t.UserID, &t.RepoID, &t.LinearIssueKey, &t.LinearIssueID, &state, &t.GateMode,
		&t.TaskKind, &t.VerifyTier, &t.AgentSessionID, &t.WorktreePath,
		&t.BranchName, &t.PRURL, &t.FailureReason, &t.FailureStage, &t.NodeID, &t.LeaseExpiresAt,
		&t.FlowID, &t.DependsOn, &t.DependsOnAt, &t.BaseRef, &t.Priority, &t.Profile, &t.PRNumber,
		&t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	t.State = State(state)
	return &t, nil
}
