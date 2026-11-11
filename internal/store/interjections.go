package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// interjections.go —— 运行中插话（task_interjections，迁移 0032）的数据访问。
//
// 设计见 docs/features/interject.md。这里只有两条不变量，所有方法都守着：
//
//  1. **每次写都在 pg_advisory_xact_lock(task_id) 下进行并推进 rev**
//     （INSERT 取默认值，UPDATE 显式 SET rev = nextval(...)）。同一任务内
//     rev 的大小顺序因此就是提交顺序，前端按 rev 增量轮询不会漏掉晚提交的小号。
//  2. **读 tasks.state 一律 FOR SHARE**，与状态机 Transition 的 FOR UPDATE 互斥：
//     插入要么发生在转出 implementing/verifying 之前（于是一定被随后的收尾
//     结算看见），要么读到新状态。锁顺序固定为「advisory → tasks 行」，
//     Transition 只拿 tasks 行锁，无环。
//
// 本文件不做状态转移：它只**读** tasks.state。

// 插话的投递状态（与 0032 的 CHECK 约束一一对应）。
const (
	InterjectionQueued     = "queued"
	InterjectionDelivering = "delivering"
	InterjectionDelivered  = "delivered"
	InterjectionFailed     = "failed"
)

// InterjectionNoReplayAck 是「写进了 stdin 但 2 秒内没等到 CLI 回显」的
// delivered 行的 error 值：写进管道且会话未关，CLI 会吸收它（实测 P5），
// 只是这次没拿到确认。不判 failed。
const InterjectionNoReplayAck = "no_replay_ack"

// ReasonRestartUndelivered 是启动清扫给遗留 delivering 行的原因。
const ReasonRestartUndelivered = "进程重启，投递未完成；这句话可能没有进入模型，如需生效请重新发送"

// Interjection 是 task_interjections 的一行。
type Interjection struct {
	ID          int64      `json:"id"`
	TaskID      int64      `json:"-"`
	UserID      int64      `json:"-"`
	Status      string     `json:"status"`
	Body        string     `json:"body"`
	Error       *string    `json:"error"`
	Phase       *string    `json:"phase"`
	SessionID   *string    `json:"sessionId"`
	DeliveryID  *string    `json:"deliveryId"`
	CreatedAt   time.Time  `json:"createdAt"`
	DeliveredAt *time.Time `json:"deliveredAt"`
	Rev         int64      `json:"rev"`
}

const interjectionCols = `id, task_id, user_id, status, body, error, phase, session_id,
	delivery_id::text, created_at, delivered_at, rev`

func scanInterjection(row pgx.Row) (*Interjection, error) {
	var it Interjection
	if err := row.Scan(&it.ID, &it.TaskID, &it.UserID, &it.Status, &it.Body, &it.Error,
		&it.Phase, &it.SessionID, &it.DeliveryID, &it.CreatedAt, &it.DeliveredAt, &it.Rev); err != nil {
		return nil, err
	}
	return &it, nil
}

// RunnableForInterjection 报告某任务状态下插话是否可能进入模型
// （= 收尾结算「还会再跑 agent」的集合）。
func RunnableForInterjection(state string) bool {
	switch state {
	case "queued", "triaging", "implementing", "verifying":
		return true
	}
	return false
}

// SettleReason 是收尾结算给 queued 行判 failed 时的人读原因（按任务状态取）。
// 实时投递在会话收尾时经状态闸门回退、读到终态时也用同一份文案。
func SettleReason(state string) string {
	switch state {
	case "pr_open", "awaiting_approval":
		return "验证已通过、没有后续 agent 轮次，这句话没有进入模型；如需生效请在 PR review 里提出"
	case "failed":
		return "任务已失败，这句话没有进入模型；重试时请重新发送"
	case "cancelled":
		return "任务已取消，这句话没有进入模型"
	case "blocked_spec":
		return "任务因需求不明确已停下，这句话没有进入模型；请补充到 issue 里"
	case "blocked_dep":
		return "任务因前置任务失败被阻塞，这句话没有进入模型；恢复后请重新发送"
	}
	return fmt.Sprintf("任务已离开实现/验证阶段（状态：%s），这句话没有进入模型", state)
}

// ErrTaskNotFound 表示插话针对的任务行不存在。
var ErrTaskNotFound = errors.New("store: 任务不存在")

// withTaskLock 开一个事务，拿该任务的 advisory 锁并以 FOR SHARE 读出状态，
// 再交给 fn。fn 返回错误时回滚。
func (s *Store) withTaskLock(ctx context.Context, taskID int64, fn func(tx pgx.Tx, state string) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: 开插话事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, taskID); err != nil {
		return fmt.Errorf("store: 取任务锁失败: %w", err)
	}
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM tasks WHERE id=$1 FOR SHARE`, taskID).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTaskNotFound
		}
		return fmt.Errorf("store: 读任务状态失败: %w", err)
	}
	if err := fn(tx, state); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: 提交插话事务失败: %w", err)
	}
	return nil
}

// InsertInterjectionGated 是 Submit 的「事务 A」：在任务锁下读状态，交给
// gate 判定；gate 返回非空原因则落 failed（不会进入模型），否则落 queued。
// 返回新行与读到的任务状态。**绝不先投后写**：调用方拿到行之后才投递。
func (s *Store) InsertInterjectionGated(ctx context.Context, taskID, userID int64, body string,
	gate func(state string) (failReason string)) (*Interjection, string, error) {
	var out *Interjection
	var seen string
	err := s.withTaskLock(ctx, taskID, func(tx pgx.Tx, state string) error {
		seen = state
		status, reason := InterjectionQueued, gate(state)
		var errCol any
		if reason != "" {
			status, errCol = InterjectionFailed, reason
		}
		it, err := scanInterjection(tx.QueryRow(ctx, `
			INSERT INTO task_interjections (task_id, user_id, body, status, error)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING `+interjectionCols, taskID, userID, body, status, errCol))
		if err != nil {
			return fmt.Errorf("store: 落插话失败（task %d）: %w", taskID, err)
		}
		out = it
		return nil
	})
	return out, seen, err
}

// GetInterjection 读一行（按 id）。
func (s *Store) GetInterjection(ctx context.Context, id int64) (*Interjection, error) {
	it, err := scanInterjection(s.pool.QueryRow(ctx,
		`SELECT `+interjectionCols+` FROM task_interjections WHERE id=$1`, id))
	if err != nil {
		return nil, fmt.Errorf("store: 读插话失败（id %d）: %w", id, err)
	}
	return it, nil
}

// updateLocked 在任务锁下执行一条带条件的 UPDATE（语句需自带
// `rev = nextval(...)` 与 RETURNING），返回更新后的行；条件不满足时返回 nil。
func (s *Store) updateLocked(ctx context.Context, taskID int64, sql string, args ...any) (*Interjection, error) {
	var out *Interjection
	err := s.withTaskLock(ctx, taskID, func(tx pgx.Tx, _ string) error {
		it, err := scanInterjection(tx.QueryRow(ctx, sql, args...))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = it
		return nil
	})
	return out, err
}

// MarkInterjectionDelivering 把一条 queued 行认领为「正在实时投递」
// （条件更新 WHERE status='queued'）。返回 nil 表示它已不是 queued
// （被收尾结算判了 failed），调用方应重读并按实际状态返回。
func (s *Store) MarkInterjectionDelivering(ctx context.Context, taskID, id int64, deliveryID, phase, sessionID string) (*Interjection, error) {
	it, err := s.updateLocked(ctx, taskID, `
		UPDATE task_interjections
		SET status='delivering', delivery_id=$2::uuid, phase=NULLIF($3,''), session_id=NULLIF($4,''),
		    rev=nextval('task_interjections_rev_seq')
		WHERE id=$1 AND status='queued'
		RETURNING `+interjectionCols, id, deliveryID, phase, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: 标记投递中失败（id %d）: %w", id, err)
	}
	return it, nil
}

// MarkInterjectionDelivered 把一条 delivering 行收尾为 delivered；
// errText 非空时记在 error 列（仅 no_replay_ack）。
func (s *Store) MarkInterjectionDelivered(ctx context.Context, taskID, id int64, errText string) (*Interjection, error) {
	it, err := s.updateLocked(ctx, taskID, `
		UPDATE task_interjections
		SET status='delivered', delivered_at=now(), error=NULLIF($2,''),
		    rev=nextval('task_interjections_rev_seq')
		WHERE id=$1 AND status='delivering'
		RETURNING `+interjectionCols, id, errText)
	if err != nil {
		return nil, fmt.Errorf("store: 标记已送达失败（id %d）: %w", id, err)
	}
	return it, nil
}

// MarkInterjectionReplayAcked 处理迟到的 isReplay 回显：行已按 2s 超时收尾为
// delivered + no_replay_ack，之后 CLI 才回显（它只在当前步骤 —— 一次 API 往返
// 或一次工具调用 —— 结束、把排队的输入并进对话时回显）。只清空 error 并前进
// rev；status 不变。不是这条投递（delivery_id 不符）、不是 no_replay_ack、或
// 还没收尾的行一律不动，返回 nil。
func (s *Store) MarkInterjectionReplayAcked(ctx context.Context, taskID, id int64, deliveryID string) (*Interjection, error) {
	it, err := s.updateLocked(ctx, taskID, `
		UPDATE task_interjections
		SET error=NULL, rev=nextval('task_interjections_rev_seq')
		WHERE id=$1 AND status='delivered' AND error=$2 AND delivery_id::text=$3
		RETURNING `+interjectionCols, id, InterjectionNoReplayAck, deliveryID)
	if err != nil {
		return nil, fmt.Errorf("store: 记迟到回显失败（id %d）: %w", id, err)
	}
	return it, nil
}

// MarkInterjectionFailed 把一条尚未终结（queued/delivering）的行判 failed。
func (s *Store) MarkInterjectionFailed(ctx context.Context, taskID, id int64, reason string) (*Interjection, error) {
	it, err := s.updateLocked(ctx, taskID, `
		UPDATE task_interjections
		SET status='failed', error=$2, rev=nextval('task_interjections_rev_seq')
		WHERE id=$1 AND status IN ('queued','delivering')
		RETURNING `+interjectionCols, id, reason)
	if err != nil {
		return nil, fmt.Errorf("store: 标记失败（id %d）: %w", id, err)
	}
	return it, nil
}

// RevertInterjectionGated 处理「实时投递时会话恰好收尾」（ErrSessionClosed）：
// 在任务锁下读状态 —— 仍在可运行集合（implementing/verifying）则退回 queued
// （清空 delivery_id/phase/session_id，等下一轮补投或结算）；否则直接判
// failed（文案取 SettleReason）。只动 delivering 行。
//
// 与收尾结算互斥于同一把锁：回退先拿到锁且读到 verifying → 行变 queued，
// 随后的结算一定看见它；结算先拿到锁 → 它跳过 delivering 行，回退随后读到
// 的已是终态 → failed。两种交错都不会留下 queued/delivering。
func (s *Store) RevertInterjectionGated(ctx context.Context, taskID, id int64) (*Interjection, string, error) {
	var out *Interjection
	var seen string
	err := s.withTaskLock(ctx, taskID, func(tx pgx.Tx, state string) error {
		seen = state
		var row pgx.Row
		if state == "implementing" || state == "verifying" {
			row = tx.QueryRow(ctx, `
				UPDATE task_interjections
				SET status='queued', delivery_id=NULL, phase=NULL, session_id=NULL,
				    rev=nextval('task_interjections_rev_seq')
				WHERE id=$1 AND status='delivering'
				RETURNING `+interjectionCols, id)
		} else {
			row = tx.QueryRow(ctx, `
				UPDATE task_interjections
				SET status='failed', error=$2, rev=nextval('task_interjections_rev_seq')
				WHERE id=$1 AND status='delivering'
				RETURNING `+interjectionCols, id, SettleReason(state))
		}
		it, err := scanInterjection(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = it
		return nil
	})
	if err != nil {
		return nil, seen, fmt.Errorf("store: 投递回退失败（id %d）: %w", id, err)
	}
	return out, seen, nil
}

// ClaimPendingInterjections 是开场补投的认领：把该任务所有 queued 行（按 id
// 升序）改为 delivering 并写上 phase/session_id，返回认领到的行。
// delivery_id 留 NULL —— 补投不经 stdin，编一个 uuid 只会让审计失真。
func (s *Store) ClaimPendingInterjections(ctx context.Context, taskID int64, phase, sessionID string) ([]Interjection, error) {
	var out []Interjection
	err := s.withTaskLock(ctx, taskID, func(tx pgx.Tx, _ string) error {
		rows, err := tx.Query(ctx, `
			UPDATE task_interjections
			SET status='delivering', phase=NULLIF($2,''), session_id=NULLIF($3,''),
			    rev=nextval('task_interjections_rev_seq')
			WHERE id IN (SELECT id FROM task_interjections WHERE task_id=$1 AND status='queued')
			RETURNING `+interjectionCols, taskID, phase, sessionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			it, err := scanInterjection(rows)
			if err != nil {
				return err
			}
			out = append(out, *it)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("store: 认领待补投插话失败（task %d）: %w", taskID, err)
	}
	sortByID(out)
	return out, nil
}

func sortByID(xs []Interjection) {
	sort.Slice(xs, func(i, j int) bool { return xs[i].ID < xs[j].ID })
}

// FinishPrefilledInterjections 是补投行的结局：delivered 为真 → delivered；
// 否则退回 queued（清空 phase/session_id），交给同一任务的下一次 agent 调用
// 或收尾结算。只动 delivering 行。
func (s *Store) FinishPrefilledInterjections(ctx context.Context, taskID int64, ids []int64, delivered bool) error {
	if len(ids) == 0 {
		return nil
	}
	sql := `UPDATE task_interjections
		SET status='queued', phase=NULL, session_id=NULL, rev=nextval('task_interjections_rev_seq')
		WHERE id = ANY($1) AND status='delivering'`
	if delivered {
		sql = `UPDATE task_interjections
		SET status='delivered', delivered_at=now(), rev=nextval('task_interjections_rev_seq')
		WHERE id = ANY($1) AND status='delivering'`
	}
	err := s.withTaskLock(ctx, taskID, func(tx pgx.Tx, _ string) error {
		_, err := tx.Exec(ctx, sql, ids)
		return err
	})
	if err != nil {
		return fmt.Errorf("store: 补投收尾失败（task %d）: %w", taskID, err)
	}
	return nil
}

// settleLocked 是结算的唯一判定（在已持锁的事务内）：任务仍在可运行集合 → 不动；
// 否则把它所有 queued 行判 failed（文案按状态）。返回改动行数。
func settleLocked(ctx context.Context, tx pgx.Tx, taskID int64, state string) (int64, error) {
	if RunnableForInterjection(state) {
		return 0, nil
	}
	tag, err := tx.Exec(ctx, `
		UPDATE task_interjections
		SET status='failed', error=$2, rev=nextval('task_interjections_rev_seq')
		WHERE id IN (SELECT id FROM task_interjections WHERE task_id=$1 AND status='queued')`,
		taskID, SettleReason(state))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// SettleQueuedInterjections 是收尾结算（T1 Execute 收尾 / T3 周期清扫 / T4 启动清扫
// 共用；T2 读时结算在 ListInterjectionsSince 里与取行同一事务）。幂等。
// 只处理 queued，不碰 delivering —— delivering 行都有活着的持有者。
func (s *Store) SettleQueuedInterjections(ctx context.Context, taskID int64) (int64, error) {
	var n int64
	err := s.withTaskLock(ctx, taskID, func(tx pgx.Tx, state string) error {
		var err error
		n, err = settleLocked(ctx, tx, taskID, state)
		return err
	})
	if errors.Is(err, ErrTaskNotFound) {
		return 0, nil // 任务行已删（CASCADE 连插话一起删了），无可结算
	}
	if err != nil {
		return 0, fmt.Errorf("store: 结算插话失败（task %d）: %w", taskID, err)
	}
	return n, nil
}

// ListInterjectionsSince 增量列出某任务 rev > afterRev 的插话（按 rev 升序，
// 至多 limit 行）。取行之前在同一事务、同一把锁下先做一次读时结算（T2）：
// 任务已离开可运行状态而 Execute 不会再跑时，有人看就当场把 queued 落成
// failed，让 rev 游标把变化推给所有读者。
func (s *Store) ListInterjectionsSince(ctx context.Context, taskID, afterRev int64, limit int) ([]Interjection, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	out := []Interjection{}
	err := s.withTaskLock(ctx, taskID, func(tx pgx.Tx, state string) error {
		if _, err := settleLocked(ctx, tx, taskID, state); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT `+interjectionCols+` FROM task_interjections
			WHERE task_id=$1 AND rev > $2
			ORDER BY rev
			LIMIT $3`, taskID, afterRev, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			it, err := scanInterjection(rows)
			if err != nil {
				return err
			}
			out = append(out, *it)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("store: 查询插话失败（task %d）: %w", taskID, err)
	}
	return out, nil
}

// InterjectionSettleCandidates 是周期/启动清扫的候选集：有 queued 行、且任务
// 已不在可运行集合的任务 ID。只是候选 —— 判定在 SettleQueuedInterjections
// 里于锁下重做，不依赖这里的快照。
func (s *Store) InterjectionSettleCandidates(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT i.task_id
		FROM task_interjections i JOIN tasks t ON t.id = i.task_id
		WHERE i.status='queued'
		  AND t.state NOT IN ('queued','triaging','implementing','verifying')
		ORDER BY i.task_id`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询待结算插话失败: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// FailDeliveringInterjections 是启动清扫的前半：进程重启后不可能还有持有者，
// 遗留的 delivering 行一律判 failed。逐任务在锁下做，保持 rev 不变量。
func (s *Store) FailDeliveringInterjections(ctx context.Context) (int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT task_id FROM task_interjections WHERE status='delivering' ORDER BY task_id`)
	if err != nil {
		return 0, fmt.Errorf("store: 查询遗留投递中插话失败: %w", err)
	}
	var tasks []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		tasks = append(tasks, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var total int64
	for _, taskID := range tasks {
		err := s.withTaskLock(ctx, taskID, func(tx pgx.Tx, _ string) error {
			tag, err := tx.Exec(ctx, `
				UPDATE task_interjections
				SET status='failed', error=$2, rev=nextval('task_interjections_rev_seq')
				WHERE task_id=$1 AND status='delivering'`, taskID, ReasonRestartUndelivered)
			if err != nil {
				return err
			}
			total += tag.RowsAffected()
			return nil
		})
		if err != nil && !errors.Is(err, ErrTaskNotFound) {
			return total, fmt.Errorf("store: 清扫投递中插话失败（task %d）: %w", taskID, err)
		}
	}
	return total, nil
}
