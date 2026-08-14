package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ResetTokenTTL 是密码重置链接的有效期。
//
// 30 分钟：够一个人去收件箱点链接，又短到邮箱事后被翻出来时链接已失效。
const ResetTokenTTL = 30 * time.Minute

// ErrResetTokenInvalid 表示重置令牌不存在、已过期或已被用过。
//
// 同样合并三种情况：对使用者来说处理一致（重新发起一次忘记密码），
// 而且不该告诉他「这个令牌存在但过期了」——那等于确认了令牌猜对了。
var ErrResetTokenInvalid = errors.New("store: 重置令牌无效")

// Resets 读写密码重置令牌。
type Resets struct {
	store *Store
}

// NewResets 构造重置令牌读写器。
func (s *Store) NewResets() *Resets { return &Resets{store: s} }

// Create 签发一个重置令牌。传入原值，落库的是其哈希。
//
// 不清除该用户此前未用的令牌：连点两次「忘记密码」时，先收到的那封邮件
// 里的链接不该突然失效 —— 人往往点的是第一封。两个都有效，且都只能用一次。
func (r *Resets) Create(ctx context.Context, userID int64, token string, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = ResetTokenTTL
	}
	if _, err := r.store.pool.Exec(ctx, `
		INSERT INTO password_reset_tokens (token_hash, user_id, expires_at)
		VALUES ($1, $2, now() + $3 * interval '1 second')`,
		hashToken(token), userID, ttl.Seconds()); err != nil {
		return fmt.Errorf("store: 创建重置令牌失败: %w", err)
	}
	return nil
}

// Consume 校验并一次性消费令牌，返回它属于哪个用户。
//
// 「校验」和「标记已用」必须是同一条 UPDATE：分成先 SELECT 再 UPDATE 的话，
// 两个并发请求会同时通过校验，同一个链接就能改两次密码。这里沿用
// ClaimDelivery 的做法 —— 靠 WHERE 条件加 RowsAffected 判定是否抢到。
func (r *Resets) Consume(ctx context.Context, token string) (int64, error) {
	var userID int64
	err := r.store.pool.QueryRow(ctx, `
		UPDATE password_reset_tokens SET used_at = now()
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
		RETURNING user_id`, hashToken(token)).Scan(&userID)
	if err != nil {
		// 没有 RETURNING 行 = 没抢到 = 令牌不存在/已用/已过期
		return 0, ErrResetTokenInvalid
	}
	return userID, nil
}

// DeleteUser 作废某账号全部未用的重置令牌。
//
// 密码一旦成功改掉（无论走重置还是自己改），在途的重置链接都该立刻失效：
// 否则一封旧邮件还能把密码改回去。
func (r *Resets) DeleteUser(ctx context.Context, userID int64) error {
	if _, err := r.store.pool.Exec(ctx,
		`DELETE FROM password_reset_tokens WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("store: 清除重置令牌失败: %w", err)
	}
	return nil
}
