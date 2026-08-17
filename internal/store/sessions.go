package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrSessionInvalid 表示会话不存在、已过期，或其账号已被禁用。
//
// 三种情况合并成一个错误：调用方的处理完全一样（当作未登录），
// 区分它们只会诱导上层把「这个账号存在但被禁了」之类的细节回给客户端。
var ErrSessionInvalid = errors.New("store: 会话无效")

// SessionTTL 是会话有效期。
const SessionTTL = 12 * time.Hour

// hashToken 计算令牌的存储形态。
//
// 会话令牌与密码重置令牌都只存哈希：数据库转储泄露时，攻击者拿到的
// 哈希无法用来冒用会话。这里不用加盐也不用慢哈希 —— 令牌本身是 32 字节
// 随机数，没有字典可爆破，SHA-256 足够且够快（每个请求都要算一次）。
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Sessions 读写登录会话。
type Sessions struct {
	store *Store
}

// NewSessions 构造会话读写器。
func (s *Store) NewSessions() *Sessions { return &Sessions{store: s} }

// Create 登记一个新会话。传入的是令牌原值，落库的是其哈希。
func (s *Sessions) Create(ctx context.Context, userID int64, token string, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = SessionTTL
	}
	// 过期时间在数据库侧算：控制面与数据库的时钟可能有偏差，统一以库为准。
	// 用「秒数乘以 interval」而不是把 Go 的 duration 字符串丢给 Postgres ——
	// "12h0m0s" 不是合法的 interval 字面量。
	if _, err := s.store.pool.Exec(ctx, `
		INSERT INTO sessions (id, user_id, expires_at)
		VALUES ($1, $2, now() + $3 * interval '1 second')`,
		hashToken(token), userID, ttl.Seconds()); err != nil {
		return fmt.Errorf("store: 创建会话失败: %w", err)
	}
	return nil
}

// Lookup 用令牌换取账号。
//
// 一次查询同时完成「会话存在且未过期」与「账号未被禁用」两项判定，
// 顺带把账号信息带回来 —— 每个受保护请求都要走这条路，能省一次往返就省。
//
// 禁用判定放在这里而不是上层：管理员一按禁用，对方的下一个请求就该被拒，
// 不能依赖上层记得去查。
func (s *Sessions) Lookup(ctx context.Context, token string) (*User, error) {
	var u User
	// 列清单与 users.go 的 userCols 保持一致（带 u. 前缀）—— 漏一列
	// 不会报错，只会让 User 的对应字段悄悄是零值（webhook_slug 就踩过）。
	err := s.store.pool.QueryRow(ctx, `
		SELECT u.id, u.email, coalesce(u.password_hash, ''), u.role,
		       u.disabled_at, u.must_change_password, u.last_login_at, u.created_at,
		       coalesce(u.webhook_slug, ''), u.notify_email
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.id = $1 AND s.expires_at > now() AND u.disabled_at IS NULL`,
		hashToken(token),
	).Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role,
		&u.DisabledAt, &u.MustChangePassword, &u.LastLoginAt, &u.CreatedAt,
		&u.WebhookSlug, &u.NotifyEmail)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSessionInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("store: 查询会话失败: %w", err)
	}
	return &u, nil
}

// Delete 注销单个会话（退出登录）。
func (s *Sessions) Delete(ctx context.Context, token string) error {
	if _, err := s.store.pool.Exec(ctx,
		`DELETE FROM sessions WHERE id = $1`, hashToken(token)); err != nil {
		return fmt.Errorf("store: 删除会话失败: %w", err)
	}
	return nil
}

// DeleteUser 踢掉某账号的全部会话。
//
// 禁用账号、改密、管理员代重置密码都要调它 —— 否则对方的旧会话还能继续用，
// 「禁用」和「重置」就都是假的。
func (s *Sessions) DeleteUser(ctx context.Context, userID int64) error {
	if _, err := s.store.pool.Exec(ctx,
		`DELETE FROM sessions WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("store: 清除用户会话失败: %w", err)
	}
	return nil
}

// DeleteUserExcept 踢掉某账号除当前会话外的全部会话。
//
// 用户自己改密时用这个：踢掉其它设备，但不让他改完密码就被自己踢下线。
func (s *Sessions) DeleteUserExcept(ctx context.Context, userID int64, keepToken string) error {
	if _, err := s.store.pool.Exec(ctx,
		`DELETE FROM sessions WHERE user_id = $1 AND id <> $2`,
		userID, hashToken(keepToken)); err != nil {
		return fmt.Errorf("store: 清除其它会话失败: %w", err)
	}
	return nil
}

// GC 清理过期会话与过期/已用的密码重置令牌。
//
// 两件事放一起：都是「定期扫掉没用的行」，调用方只需要一个定时器。
// 返回删除的行数便于日志观察。
func (s *Sessions) GC(ctx context.Context) (int64, error) {
	tag, err := s.store.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("store: 清理过期会话失败: %w", err)
	}
	n := tag.RowsAffected()

	// 已用过的令牌也一并清掉，但留 24 小时 —— 万一要排查「这个链接是什么时候用的」
	tag2, err := s.store.pool.Exec(ctx, `
		DELETE FROM password_reset_tokens
		WHERE expires_at <= now() OR used_at <= now() - interval '24 hours'`)
	if err != nil {
		return n, fmt.Errorf("store: 清理过期重置令牌失败: %w", err)
	}
	return n + tag2.RowsAffected(), nil
}
