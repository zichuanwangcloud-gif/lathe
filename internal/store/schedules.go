package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// 定时建单的存储层（docs/features/scheduled-issue.md，迁移 0059/0060）。
//
// 边界：本文件只读写 schedules / schedule_runs 两张表，对 tasks 只有
// runs 列表里的一条只读 LATERAL join（派生「这张单被接成了哪个任务」）。
// 调度器绝不写 tasks —— 任务行只能经 webhook → Enqueue 产生。

// ErrScheduleNotFound 表示规则不存在或不属于当前用户（两者刻意不分，404 语义）。
var ErrScheduleNotFound = errors.New("store: 定时规则不存在")

// ErrScheduleNameTaken 表示本人名下已有同名规则（409）。
var ErrScheduleNameTaken = errors.New("store: 已有同名定时规则")

// ErrRunSlotTaken 表示该计划时刻已经有一条运行记录（立即运行同一分钟点两次 → 409）。
var ErrRunSlotTaken = errors.New("store: 该计划时刻已有运行记录")

// 运行记录的状态 / 触发方式 / 原因取值（与 0060 的 CHECK 一致；reason 无 CHECK）。
const (
	RunPending = "pending"
	RunCreated = "created"
	RunFailed  = "failed"
	RunSkipped = "skipped"

	TriggerSchedule = "schedule"
	TriggerCatchup  = "catchup"
	TriggerManual   = "manual"

	ReasonMissedWindow     = "missed_window"
	ReasonOwnerDisabled    = "owner_disabled"
	ReasonScheduleDisabled = "schedule_disabled"
	ReasonInterrupted      = "interrupted"
)

// ScheduleRow 是 schedules 表的一行（JSON 标签直接供 API 返回）。
type ScheduleRow struct {
	ID                  int64      `json:"id"`
	UserID              int64      `json:"userId"`
	Name                string     `json:"name"`
	Enabled             bool       `json:"enabled"`
	EnabledAt           time.Time  `json:"enabledAt"`
	Kind                string     `json:"kind"`
	MinuteOfDay         *int       `json:"minuteOfDay"`
	Weekdays            *int       `json:"weekdays"`
	IntervalMinutes     *int       `json:"intervalMinutes"`
	AnchorAt            *time.Time `json:"anchorAt"`
	Timezone            string     `json:"timezone"`
	LinearTeamID        string     `json:"linearTeamId"`
	LinearTeamKey       string     `json:"linearTeamKey"`
	IntakeMode          string     `json:"intakeMode"`
	TitleTemplate       string     `json:"titleTemplate"`
	BodyTemplate        string     `json:"bodyTemplate"`
	Priority            int        `json:"priority"`
	NextFireAt          *time.Time `json:"nextFireAt"`
	LastSlotAt          *time.Time `json:"lastSlotAt"`
	ConsecutiveFailures int        `json:"consecutiveFailures"`
	LastError           *string    `json:"lastError"`
	PausedReason        *string    `json:"pausedReason"`
	CreatedAt           time.Time  `json:"createdAt"`
	UpdatedAt           time.Time  `json:"updatedAt"`

	// LastRun 是最近一次运行的摘要，只在列表里填。
	LastRun *ScheduleRunRow `json:"lastRun,omitempty"`
}

// RunTask 是从 tasks 只读派生的「这张单被接成了哪个任务」。
type RunTask struct {
	ID    int64  `json:"id"`
	State string `json:"state"`
}

// ScheduleRunRow 是 schedule_runs 表的一行。
type ScheduleRunRow struct {
	ID                    int64      `json:"id"`
	ScheduleID            int64      `json:"scheduleId"`
	UserID                int64      `json:"userId"`
	ScheduledFor          time.Time  `json:"scheduledFor"`
	Trigger               string     `json:"trigger"`
	Status                string     `json:"status"`
	Reason                *string    `json:"reason"`
	MissedCount           int        `json:"missedCount"`
	LinearIssueUUID       string     `json:"linearIssueUuid"`
	LinearIssueID         *string    `json:"linearIssueId"`
	LinearIssueIdentifier *string    `json:"linearIssueIdentifier"`
	LinearIssueURL        *string    `json:"linearIssueUrl"`
	Error                 *string    `json:"error"`
	Attempts              int        `json:"attempts"`
	Node                  string     `json:"node"`
	LeaseToken            *string    `json:"-"` // 租约令牌不出 API：它是写终态的凭证
	LeaseNode             *string    `json:"leaseNode"`
	LeaseUntil            *time.Time `json:"leaseUntil"`
	CreateSentAt          *time.Time `json:"createSentAt"`
	CreatedAt             time.Time  `json:"createdAt"`
	FinishedAt            *time.Time `json:"finishedAt"`

	// Task 只在 runs 列表里填（LATERAL join 派生），没有就是 null。
	Task *RunTask `json:"task"`
}

const scheduleCols = `id, user_id, name, enabled, enabled_at, kind, minute_of_day, weekdays,
	interval_minutes, anchor_at, timezone, linear_team_id, linear_team_key, intake_mode,
	title_template, body_template, priority, next_fire_at, last_slot_at,
	consecutive_failures, last_error, paused_reason, created_at, updated_at`

func scanSchedule(row pgx.Row) (*ScheduleRow, error) {
	var s ScheduleRow
	var weekdays *int16
	var priority int16
	if err := row.Scan(&s.ID, &s.UserID, &s.Name, &s.Enabled, &s.EnabledAt, &s.Kind,
		&s.MinuteOfDay, &weekdays, &s.IntervalMinutes, &s.AnchorAt, &s.Timezone,
		&s.LinearTeamID, &s.LinearTeamKey, &s.IntakeMode, &s.TitleTemplate, &s.BodyTemplate,
		&priority, &s.NextFireAt, &s.LastSlotAt, &s.ConsecutiveFailures, &s.LastError,
		&s.PausedReason, &s.CreatedAt, &s.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrScheduleNotFound
		}
		return nil, fmt.Errorf("store: 读取定时规则失败: %w", err)
	}
	if weekdays != nil {
		w := int(*weekdays)
		s.Weekdays = &w
	}
	s.Priority = int(priority)
	return &s, nil
}

const runCols = `id, schedule_id, user_id, scheduled_for, trigger, status, reason, missed_count,
	linear_issue_uuid::text, linear_issue_id, linear_issue_identifier, linear_issue_url, error,
	attempts, node, lease_token::text, lease_node, lease_until, create_sent_at, created_at, finished_at`

func scanRunInto(row pgx.Row, extra ...any) (*ScheduleRunRow, error) {
	var r ScheduleRunRow
	dest := []any{&r.ID, &r.ScheduleID, &r.UserID, &r.ScheduledFor, &r.Trigger, &r.Status,
		&r.Reason, &r.MissedCount, &r.LinearIssueUUID, &r.LinearIssueID, &r.LinearIssueIdentifier,
		&r.LinearIssueURL, &r.Error, &r.Attempts, &r.Node, &r.LeaseToken, &r.LeaseNode,
		&r.LeaseUntil, &r.CreateSentAt, &r.CreatedAt, &r.FinishedAt}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	return &r, nil
}

// ScheduleParams 是新建 / 全量更新一条规则的输入。
//
// NextFireAt 由调用方按规则算好：Enabled 时必须非空，停用时必须为 nil ——
// 二者在同一条语句里写，schedules_enabled_next 这条 CHECK 是数据库兜底。
// Now 是启用基线（enabled_at、last_slot_at），只在 Enabled 时使用。
type ScheduleParams struct {
	Name            string
	Enabled         bool
	Kind            string
	MinuteOfDay     *int
	Weekdays        *int
	IntervalMinutes *int
	AnchorAt        *time.Time
	Timezone        string
	LinearTeamID    string
	LinearTeamKey   string
	IntakeMode      string
	TitleTemplate   string
	BodyTemplate    string
	Priority        int
	NextFireAt      *time.Time
	Now             time.Time
}

func scheduleWriteErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" && pe.ConstraintName == "schedules_user_name_unique" {
		return ErrScheduleNameTaken
	}
	return err
}

// CreateSchedule 新建规则。启用时 enabled_at = last_slot_at = p.Now（不补启用之前的时刻）。
func (s *Store) CreateSchedule(ctx context.Context, userID int64, p ScheduleParams) (*ScheduleRow, error) {
	var lastSlot *time.Time
	enabledAt := p.Now
	if p.Enabled {
		lastSlot = &p.Now
	}
	if enabledAt.IsZero() {
		enabledAt = time.Now()
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO schedules (user_id, name, enabled, enabled_at, kind, minute_of_day, weekdays,
			interval_minutes, anchor_at, timezone, linear_team_id, linear_team_key, intake_mode,
			title_template, body_template, priority, next_fire_at, last_slot_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		RETURNING `+scheduleCols,
		userID, p.Name, p.Enabled, enabledAt, p.Kind, p.MinuteOfDay, p.Weekdays,
		p.IntervalMinutes, p.AnchorAt, p.Timezone, p.LinearTeamID, p.LinearTeamKey, p.IntakeMode,
		p.TitleTemplate, p.BodyTemplate, p.Priority, p.NextFireAt, lastSlot)
	sc, err := scanSchedule(row)
	if err != nil {
		return nil, scheduleWriteErr(err)
	}
	return sc, nil
}

// UpdateSchedule 全量更新属主的规则。
//
// 启用（含编辑一条启用中的规则、由停用转启用）：基线重置为 p.Now、连续失败清零、
// 自动暂停原因清空 —— 改个标题不该立刻补一张单。停用：next_fire_at 置空。
func (s *Store) UpdateSchedule(ctx context.Context, id, userID int64, p ScheduleParams) (*ScheduleRow, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE schedules SET
			name = $3, enabled = $4, kind = $5, minute_of_day = $6, weekdays = $7,
			interval_minutes = $8, anchor_at = $9, timezone = $10, linear_team_id = $11,
			linear_team_key = $12, intake_mode = $13, title_template = $14, body_template = $15,
			priority = $16, next_fire_at = $17,
			enabled_at           = CASE WHEN $4 THEN $18 ELSE enabled_at END,
			last_slot_at         = CASE WHEN $4 THEN $18 ELSE last_slot_at END,
			consecutive_failures = CASE WHEN $4 THEN 0 ELSE consecutive_failures END,
			paused_reason        = CASE WHEN $4 THEN NULL ELSE paused_reason END
		WHERE id = $1 AND user_id = $2
		RETURNING `+scheduleCols,
		id, userID, p.Name, p.Enabled, p.Kind, p.MinuteOfDay, p.Weekdays, p.IntervalMinutes,
		p.AnchorAt, p.Timezone, p.LinearTeamID, p.LinearTeamKey, p.IntakeMode, p.TitleTemplate,
		p.BodyTemplate, p.Priority, p.NextFireAt, p.Now)
	sc, err := scanSchedule(row)
	if err != nil {
		return nil, scheduleWriteErr(err)
	}
	return sc, nil
}

// GetSchedule 读属主的一条规则；非属主与不存在一律 ErrScheduleNotFound。
func (s *Store) GetSchedule(ctx context.Context, id, userID int64) (*ScheduleRow, error) {
	return scanSchedule(s.pool.QueryRow(ctx,
		`SELECT `+scheduleCols+` FROM schedules WHERE id = $1 AND user_id = $2`, id, userID))
}

// ListSchedules 返回属主的全部规则（按 id），每条带最近一次运行摘要。
func (s *Store) ListSchedules(ctx context.Context, userID int64) ([]ScheduleRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+scheduleCols+` FROM schedules WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: 列出定时规则失败: %w", err)
	}
	defer rows.Close()
	out := []ScheduleRow{}
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 列出定时规则失败: %w", err)
	}
	for i := range out {
		runs, err := s.ListScheduleRuns(ctx, out[i].ID, userID, 1)
		if err != nil {
			return nil, err
		}
		if len(runs) > 0 {
			out[i].LastRun = &runs[0]
		}
	}
	return out, nil
}

// CountSchedules 返回属主名下规则数（每人上限的判定依据）。
func (s *Store) CountSchedules(ctx context.Context, userID int64) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM schedules WHERE user_id = $1`, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: 统计定时规则失败: %w", err)
	}
	return n, nil
}

// DeleteSchedule 删除属主的规则；运行记录级联删除，Linear 上已建的单不动。
func (s *Store) DeleteSchedule(ctx context.Context, id, userID int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM schedules WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("store: 删除定时规则失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrScheduleNotFound
	}
	return nil
}

// ListScheduleRuns 按计划时刻倒序返回运行记录，并只读派生对应任务。
//
// 派生 join 带 tasks.user_id = r.user_id：不会把别人的任务挂到自己的记录上。
func (s *Store) ListScheduleRuns(ctx context.Context, scheduleID, userID int64, limit int) ([]ScheduleRunRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT r.`+prefixCols(runCols, "r.")+`, t.id, t.state
		  FROM schedule_runs r
		  LEFT JOIN LATERAL (
		        SELECT id, state FROM tasks
		         WHERE tasks.user_id = r.user_id
		           AND tasks.tracker_provider = 'linear'
		           AND tasks.external_id = r.linear_issue_id
		         ORDER BY id DESC LIMIT 1) t ON true
		 WHERE r.schedule_id = $1 AND r.user_id = $2
		 ORDER BY r.scheduled_for DESC, r.id DESC
		 LIMIT $3`, scheduleID, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 列出运行记录失败: %w", err)
	}
	defer rows.Close()
	out := []ScheduleRunRow{}
	for rows.Next() {
		var taskID *int64
		var taskState *string
		r, err := scanRunInto(rows, &taskID, &taskState)
		if err != nil {
			return nil, fmt.Errorf("store: 读取运行记录失败: %w", err)
		}
		if taskID != nil {
			r.Task = &RunTask{ID: *taskID, State: deref(taskState)}
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// prefixCols 给逗号分隔的列清单逐个加表别名前缀（第一个由调用方自己写）。
func prefixCols(cols, prefix string) string {
	out := []byte{}
	for i := 0; i < len(cols); i++ {
		out = append(out, cols[i])
		if cols[i] == ',' {
			j := i + 1
			for j < len(cols) && (cols[j] == ' ' || cols[j] == '\n' || cols[j] == '\t') {
				out = append(out, cols[j])
				j++
			}
			out = append(out, prefix...)
			i = j - 1
		}
	}
	return string(out)
}

// GetScheduleRun 读一条运行记录（不做属主过滤：调用方已按 run id 归属）。
func (s *Store) GetScheduleRun(ctx context.Context, runID int64) (*ScheduleRunRow, error) {
	r, err := scanRunInto(s.pool.QueryRow(ctx, `SELECT `+runCols+` FROM schedule_runs WHERE id = $1`, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrScheduleNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读取运行记录失败: %w", err)
	}
	return r, nil
}

// ---------------------------------------------------------------- 调度器

// NewRun 是阶段 A 要插入的一条运行记录。LeaseToken 只在 pending 行上非空。
type NewRun struct {
	ScheduledFor time.Time
	Trigger      string
	Status       string
	Reason       string
	MissedCount  int
	UUID         string
	Node         string
	LeaseToken   string
	LeaseTTL     time.Duration
}

// ClaimDecision 是阶段 A 对一条到期规则的判定结果，由调度器（纯 Go）算出。
//
// Insert 为 nil 表示不建 run（理论上不会发生：到期即应有一个时刻）。
// NextFireAt 为 nil 表示停用该规则（前移防御失败），此时 PausedReason 必填。
type ClaimDecision struct {
	Insert       *NewRun
	NextFireAt   *time.Time
	LastSlotAt   *time.Time
	PausedReason string
}

// ClaimedSchedule 是 ClaimDueSchedule 认领到的规则与本轮结果。
type ClaimedSchedule struct {
	Schedule *ScheduleRow
	// Run 是本轮新插入的那一行；ON CONFLICT 冲突（时刻已被别人占）时为 nil。
	Run *ScheduleRunRow
}

// ClaimDueSchedule 认领一条到期规则（FOR UPDATE SKIP LOCKED LIMIT 1），在同一个短事务里：
// 调 decide 得出判定 → INSERT ... ON CONFLICT DO NOTHING → **无条件**前移规则。
//
// 冲突（同一时刻已有 run）时规则照样前移：冲突只意味着「这一轮没有新 run」，
// 不意味着「规则不推进」，否则规则会每个 tick 被重新认领一次空转到下一个时刻。
// 没有到期规则时返回 (nil, nil)。
func (s *Store) ClaimDueSchedule(ctx context.Context, now time.Time,
	decide func(sc *ScheduleRow, ownerDisabled bool) ClaimDecision) (*ClaimedSchedule, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: 开启认领事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var disabledAt *time.Time
	row := tx.QueryRow(ctx, `
		SELECT `+prefixCols("s."+scheduleCols, "s.")+`, u.disabled_at
		  FROM schedules s JOIN users u ON u.id = s.user_id
		 WHERE s.enabled AND s.next_fire_at <= $1
		 ORDER BY s.next_fire_at, s.id
		 FOR UPDATE OF s SKIP LOCKED
		 LIMIT 1`, now)
	sc, err := scanScheduleWith(row, &disabledAt)
	if errors.Is(err, ErrScheduleNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	d := decide(sc, disabledAt != nil)
	out := &ClaimedSchedule{Schedule: sc}
	if d.Insert != nil {
		in := d.Insert
		var tok, leaseNode any
		var ttl any
		attempts := 0
		if in.Status == RunPending {
			tok, leaseNode, ttl, attempts = in.LeaseToken, in.Node, in.LeaseTTL.Seconds(), 1
		}
		r, err := scanRunInto(tx.QueryRow(ctx, `
			INSERT INTO schedule_runs (schedule_id, user_id, scheduled_for, trigger, status, reason,
				missed_count, linear_issue_uuid, node, attempts, lease_token, lease_node, lease_until)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8, $9, $10, $11::uuid, $12,
			        CASE WHEN $13::float8 IS NULL THEN NULL ELSE now() + make_interval(secs => $13::float8) END)
			ON CONFLICT (schedule_id, scheduled_for) DO NOTHING
			RETURNING `+runCols,
			sc.ID, sc.UserID, in.ScheduledFor, in.Trigger, in.Status, in.Reason, in.MissedCount,
			in.UUID, in.Node, attempts, tok, leaseNode, ttl))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// 冲突：该时刻已被手动运行、别的节点或上一轮占了，本轮不建 run
		case err != nil:
			return nil, fmt.Errorf("store: 插入运行记录失败: %w", err)
		default:
			out.Run = r
		}
	}

	// ↓ 无论上面插没插进去都执行（冲突也前移）
	if d.NextFireAt != nil {
		_, err = tx.Exec(ctx, `UPDATE schedules SET next_fire_at = $2, last_slot_at = COALESCE($3, last_slot_at) WHERE id = $1`,
			sc.ID, *d.NextFireAt, d.LastSlotAt)
	} else {
		_, err = tx.Exec(ctx, `UPDATE schedules SET enabled = false, next_fire_at = NULL,
			last_slot_at = COALESCE($2, last_slot_at), paused_reason = $3 WHERE id = $1`,
			sc.ID, d.LastSlotAt, d.PausedReason)
	}
	if err != nil {
		return nil, fmt.Errorf("store: 前移定时规则失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: 提交认领事务失败: %w", err)
	}
	return out, nil
}

func scanScheduleWith(row pgx.Row, extra ...any) (*ScheduleRow, error) {
	return scanSchedule(rowWithExtra{row: row, extra: extra})
}

// rowWithExtra 让 scanSchedule 复用于「规则列 + 额外列」的查询。
type rowWithExtra struct {
	row   pgx.Row
	extra []any
}

func (r rowWithExtra) Scan(dest ...any) error { return r.row.Scan(append(dest, r.extra...)...) }

// InsertManualRun 为「立即运行」插入一条 pending（trigger=manual，带租约，attempts=1）。
//
// 规则必须属于 userID（否则 ErrScheduleNotFound）；同一计划时刻已有记录 → ErrRunSlotTaken。
func (s *Store) InsertManualRun(ctx context.Context, scheduleID, userID int64, slot time.Time,
	uuid, node, token string, ttl time.Duration) (*ScheduleRunRow, error) {
	r, err := scanRunInto(s.pool.QueryRow(ctx, `
		WITH sc AS (SELECT id, user_id FROM schedules WHERE id = $1 AND user_id = $2),
		ins AS (
			INSERT INTO schedule_runs (schedule_id, user_id, scheduled_for, trigger, status,
				linear_issue_uuid, node, attempts, lease_token, lease_node, lease_until)
			SELECT sc.id, sc.user_id, $3, 'manual', 'pending', $4, $5, 1, $6::uuid, $5,
			       now() + make_interval(secs => $7::float8)
			  FROM sc
			ON CONFLICT (schedule_id, scheduled_for) DO NOTHING
			RETURNING `+runCols+`)
		SELECT `+runCols+` FROM ins`,
		scheduleID, userID, slot, uuid, node, token, ttl.Seconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := s.GetSchedule(ctx, scheduleID, userID); gerr != nil {
			return nil, gerr
		}
		return nil, ErrRunSlotTaken
	}
	if err != nil {
		return nil, fmt.Errorf("store: 插入手动运行记录失败: %w", err)
	}
	return r, nil
}

// RecoverExpiredRuns 原子回收租约已过期的 pending（节点崩溃或被围栏后遗留）。
//
// 时间一律用数据库 now()；子查询外再判一次 lease_until < now()，
// 防止 Read Committed 下拿到刚被续约的行。attempts +1。
func (s *Store) RecoverExpiredRuns(ctx context.Context, token, node string, ttl time.Duration, limit int) ([]ScheduleRunRow, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE schedule_runs r
		   SET lease_token = $1::uuid, lease_node = $2,
		       lease_until = now() + make_interval(secs => $3::float8),
		       attempts = r.attempts + 1
		 WHERE r.id IN (SELECT id FROM schedule_runs
		                 WHERE status = 'pending' AND lease_until < now()
		                 ORDER BY lease_until, id
		                 LIMIT $4 FOR UPDATE SKIP LOCKED)
		   AND r.status = 'pending' AND r.lease_until < now()
		RETURNING `+prefixCols("r."+runCols, "r."), token, node, ttl.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: 回收过期运行记录失败: %w", err)
	}
	defer rows.Close()
	var out []ScheduleRunRow
	for rows.Next() {
		r, err := scanRunInto(rows)
		if err != nil {
			return nil, fmt.Errorf("store: 读取回收记录失败: %w", err)
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// RenewLeases 给本节点持有的全部 token 名下、仍是 pending 的 run 续约（节点级心跳）。
func (s *Store) RenewLeases(ctx context.Context, tokens []string, ttl time.Duration) (int64, error) {
	if len(tokens) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE schedule_runs SET lease_until = now() + make_interval(secs => $2::float8)
		 WHERE lease_token = ANY($1::uuid[]) AND status = 'pending'`, tokens, ttl.Seconds())
	if err != nil {
		return 0, fmt.Errorf("store: 续约失败: %w", err)
	}
	return tag.RowsAffected(), nil
}

// MarkCreateSent 在发出 issueCreate 之前记下 create_sent_at（只有租约持有者能写）。
// 返回 false = 已丢租约，调用方必须放弃、不发请求。已写过的不改写。
func (s *Store) MarkCreateSent(ctx context.Context, runID int64, token string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE schedule_runs SET create_sent_at = COALESCE(create_sent_at, now())
		 WHERE id = $1 AND status = 'pending' AND lease_token = $2::uuid`, runID, token)
	if err != nil {
		return false, fmt.Errorf("store: 写 create_sent_at 失败: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RunContext 是执行一条 run 之前要重新读的东西（阶段 B 第 1 步）。
type RunContext struct {
	Run           *ScheduleRunRow
	Schedule      *ScheduleRow
	OwnerDisabled bool
}

// LoadRunContext 读 run、所属规则与属主是否停用。run 不存在（规则被删、级联删除）→ ErrScheduleNotFound。
func (s *Store) LoadRunContext(ctx context.Context, runID int64) (*RunContext, error) {
	run, err := s.GetScheduleRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	var disabledAt *time.Time
	sc, err := scanScheduleWith(s.pool.QueryRow(ctx, `
		SELECT `+prefixCols("s."+scheduleCols, "s.")+`, u.disabled_at
		  FROM schedules s JOIN users u ON u.id = s.user_id
		 WHERE s.id = $1`, run.ScheduleID), &disabledAt)
	if err != nil {
		return nil, err
	}
	return &RunContext{Run: run, Schedule: sc, OwnerDisabled: disabledAt != nil}, nil
}

// RuleEffect 决定一次终态写入要不要动规则计数。
type RuleEffect int

const (
	// RuleUntouched：不计入连续失败、不改 last_error（skipped、interrupted、停用拦截）。
	RuleUntouched RuleEffect = iota
	// RuleSuccess：连续失败清零、last_error 置空。
	RuleSuccess
	// RuleFailure：连续失败 +1、写 last_error，到上限自动暂停。
	RuleFailure
)

// FinishRunParams 是一次终态写入。
type FinishRunParams struct {
	RunID      int64
	Token      string
	Status     string
	Reason     string
	Error      string
	Issue      *CreatedIssueRef
	Effect     RuleEffect
	MaxFailure int
	// PausePrefix 是自动暂停原因的前缀，后面接最近一次错误。
	PausePrefix string
}

// CreatedIssueRef 是建单成功后写入 run 的三个字段。
type CreatedIssueRef struct {
	ID, Identifier, URL string
}

// FinishRun 条件写终态：只有当前租约持有者（status=pending 且 token 匹配）能写。
// 规则计数与 run 终态同一事务，且只在 run 那条 UPDATE 真的改到一行时才写。
// 返回 false = 已丢租约或该行已有终态，什么都没改。
func (s *Store) FinishRun(ctx context.Context, p FinishRunParams) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: 开启终态事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var issueID, identifier, url any
	if p.Issue != nil {
		issueID, identifier, url = p.Issue.ID, p.Issue.Identifier, p.Issue.URL
	}
	var scheduleID int64
	err = tx.QueryRow(ctx, `
		UPDATE schedule_runs SET status = $3, reason = NULLIF($4, ''), error = NULLIF($5, ''),
		       linear_issue_id = COALESCE($6, linear_issue_id),
		       linear_issue_identifier = COALESCE($7, linear_issue_identifier),
		       linear_issue_url = COALESCE($8, linear_issue_url),
		       finished_at = now()
		 WHERE id = $1 AND status = 'pending' AND lease_token = $2::uuid
		RETURNING schedule_id`,
		p.RunID, p.Token, p.Status, p.Reason, p.Error, issueID, identifier, url).Scan(&scheduleID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: 写运行终态失败: %w", err)
	}

	switch p.Effect {
	case RuleSuccess:
		_, err = tx.Exec(ctx, `UPDATE schedules SET consecutive_failures = 0, last_error = NULL WHERE id = $1`, scheduleID)
	case RuleFailure:
		_, err = tx.Exec(ctx, `
			UPDATE schedules SET
			       consecutive_failures = consecutive_failures + 1,
			       last_error = $2,
			       enabled = CASE WHEN consecutive_failures + 1 >= $3 THEN false ELSE enabled END,
			       next_fire_at = CASE WHEN consecutive_failures + 1 >= $3 THEN NULL ELSE next_fire_at END,
			       paused_reason = CASE WHEN consecutive_failures + 1 >= $3 THEN $4 || $2 ELSE paused_reason END
			 WHERE id = $1`, scheduleID, p.Error, p.MaxFailure, p.PausePrefix)
	}
	if err != nil {
		return false, fmt.Errorf("store: 更新规则计数失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: 提交终态事务失败: %w", err)
	}
	return true, nil
}
