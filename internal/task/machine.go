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
	State          State
	GateMode       string
	TaskKind       *string
	VerifyTier     *string
	AgentSessionID *string
	WorktreePath   *string
	BranchName     *string
	PRURL          *string
	FailureReason  *string
	NodeID         *int64
	LeaseExpiresAt *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
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
	id, user_id, repo_id, linear_issue_key, state, gate_mode,
	task_kind, verify_tier, agent_session_id, worktree_path,
	branch_name, pr_url, failure_reason, node_id, lease_expires_at,
	created_at, updated_at`

// CreateParams 是建任务所需的最小输入。
type CreateParams struct {
	UserID         int64
	RepoID         int64
	LinearIssueKey string
	GateMode       string
	TaskKind       *string
}

// Create 新建任务（初始状态 queued）并记录创建事件。
//
// 若同一 issue 已有活任务，数据库的部分唯一索引会拒绝插入。
func (m *Machine) Create(ctx context.Context, p CreateParams) (*Task, error) {
	if p.GateMode == "" {
		p.GateMode = "direct"
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("task: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx, `
		INSERT INTO tasks (user_id, repo_id, linear_issue_key, state, gate_mode, task_kind)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+taskColumns,
		p.UserID, p.RepoID, p.LinearIssueKey, StateQueued, p.GateMode, p.TaskKind)

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
			node_id           = COALESCE($10, node_id),
			lease_expires_at  = COALESCE($11, lease_expires_at)
		WHERE id = $1
		RETURNING `+taskColumns,
		id, to,
		opts.AgentSessionID, opts.WorktreePath, opts.BranchName, opts.PRURL,
		opts.VerifyTier, opts.TaskKind, opts.FailureReason, opts.NodeID, opts.LeaseExpiresAt,
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
		&t.ID, &t.UserID, &t.RepoID, &t.LinearIssueKey, &state, &t.GateMode,
		&t.TaskKind, &t.VerifyTier, &t.AgentSessionID, &t.WorktreePath,
		&t.BranchName, &t.PRURL, &t.FailureReason, &t.NodeID, &t.LeaseExpiresAt,
		&t.CreatedAt, &t.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	t.State = State(state)
	return &t, nil
}
