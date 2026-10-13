package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// notify.go 通知类邮件的收件人解析（docs/08-debt-cleanup.md T3）。

// ErrNoRecipient 表示这个任务/PRD 查不到收件人（对象不存在，或属主已被删除）。
var ErrNoRecipient = errors.New("store: 该对象没有可用的通知收件人")

// NotifyEmailForTask 返回任务属主的通知收件人。
//
// 口径：优先 users.notify_email，为 NULL 或空串时回退登录邮箱 users.email。
// 这与密码重置刻意不同 —— 重置邮件永远发登录邮箱（那封信的意义就是
// 「证明你拥有这个登录邮箱」），而通知类邮件是发给「这个人」的，
// 他想收在哪儿就收在哪儿。
//
// 单独一个 JOIN 查询而不是「先查任务再查用户」，是为了让 runner 侧
// 只需要一次往返：通知发生在终态转移之后的热路径上，能省一次就省一次。
func (s *Store) NotifyEmailForTask(ctx context.Context, taskID int64) (string, error) {
	var email string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(NULLIF(u.notify_email, ''), u.email)
		FROM tasks t JOIN users u ON u.id = t.user_id
		WHERE t.id = $1`, taskID).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoRecipient
	}
	if err != nil {
		return "", fmt.Errorf("store: 查通知收件人失败: %w", err)
	}
	if email == "" {
		return "", ErrNoRecipient
	}
	return email, nil
}

// NotifyEmailForPRD 返回 PRD 属主的通知收件人（docs/10 §9 的搁置提醒）。
//
// 口径与 NotifyEmailForTask 完全一致（同一句 COALESCE、同一个 ErrNoRecipient），
// 刻意不共用一个「按 userID 查」的下层函数再各自转一层：两处各是一条
// 单表（+一次 JOIN）的点查，抽出来的公共部分只有那句 COALESCE，
// 而代价是调用方要多绕一层、错误信息里也看不出是谁的收件人。
//
// 搁置扫描器按行传来的 prd.user_id 就够查，不必再 JOIN prds；
// 但这里仍然按 PRD 查，是为了让「PRD 被删了」与「属主被删了」走同一条
// 错误路径 —— 扫描器拿到 ErrNoRecipient 就跳过，不需要区分。
func (s *Store) NotifyEmailForPRD(ctx context.Context, prdID int64) (string, error) {
	var email string
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(NULLIF(u.notify_email, ''), u.email)
		FROM prds p JOIN users u ON u.id = p.user_id
		WHERE p.id = $1`, prdID).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoRecipient
	}
	if err != nil {
		return "", fmt.Errorf("store: 查 PRD 通知收件人失败: %w", err)
	}
	if email == "" {
		return "", ErrNoRecipient
	}
	return email, nil
}
