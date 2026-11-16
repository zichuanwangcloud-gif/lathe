package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PATPrefix 是个人访问令牌（PAT）明文的固定前缀。
//
// 固定前缀让日志扫描、失败摘要脱敏（internal/runner/redact.go 的 tokenShapes）
// 与鉴权入口（httpapi.Auth.resolve 的 PAT 支路）都能一眼认出它，
// 且与部署者随意填写的 LATHE_ADMIN_TOKEN 不会撞形态。
const PATPrefix = "lathe_pat_"

// patDisplayPrefixLen 是列表页展示的明文片段长度（PATPrefix 之后的字符数）。
// 剩下的 35 个 base64url 字符仍有 210 位熵，展示它不降低强度。
const patDisplayPrefixLen = 8

// ErrPATInvalid 表示令牌不存在、已吊销、已过期，或其账号已被停用。
//
// 与 ErrSessionInvalid 同理：四种情况合并成一个错误，区分它们只会变成预言机。
var ErrPATInvalid = errors.New("store: 个人访问令牌无效")

// PAT 是列表接口上的一把令牌。不含哈希，更不含明文。
type PAT struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	// UseCount 是近似值：Touch 有 60 秒节流，节流窗口内的请求不计。
	UseCount  int64      `json:"useCount"`
	ExpiresAt *time.Time `json:"expiresAt"`
	Expired   bool       `json:"expired"`
}

// PersonalAccessTokens 读写个人访问令牌。
//
// 存储口径与 sessions 相同：库里只存 SHA-256(明文) 的十六进制（hashToken）。
// 不加盐也不用慢哈希 —— 明文是 32 字节 crypto/rand 输出，没有字典可爆破，
// SHA-256 足够且每个请求都要算一次。这里不是「存密码用 SHA-256」的错误，
// 口令仍是 bcrypt（internal/auth）。也不用 AES-GCM：平台自己签发、自己比对，
// 从不需要还原明文。
type PersonalAccessTokens struct {
	store *Store
}

// NewPATs 构造个人访问令牌读写器。
func (s *Store) NewPATs() *PersonalAccessTokens { return &PersonalAccessTokens{store: s} }

// newPATPlain 生成一把明文：PATPrefix + 43 个 base64url 字符（32 字节随机）。
func newPATPlain() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: 无法获取随机数: %w", err)
	}
	return PATPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// Create 生成一把新令牌并入库，返回明文（只此一次）与行 id。
//
// 只负责「生成 + 入库」：不判每用户上限、不验口令 —— 那是 httpapi.PATAPI
// 的职责。expiresAt 为 nil 表示永不过期；合法性由调用方校验。
func (p *PersonalAccessTokens) Create(ctx context.Context, userID int64, name string, expiresAt *time.Time) (string, int64, error) {
	plain, err := newPATPlain()
	if err != nil {
		return "", 0, err
	}
	prefix := plain[len(PATPrefix) : len(PATPrefix)+patDisplayPrefixLen]
	var id int64
	if err := p.store.pool.QueryRow(ctx, `
		INSERT INTO personal_access_tokens (user_id, name, token_hash, prefix, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`,
		userID, name, hashToken(plain), prefix, expiresAt).Scan(&id); err != nil {
		return "", 0, fmt.Errorf("store: 创建个人访问令牌失败: %w", err)
	}
	return plain, id, nil
}

// List 列出某用户的全部未吊销令牌（含已过期的，Expired 标真），按创建先后。
func (p *PersonalAccessTokens) List(ctx context.Context, userID int64) ([]PAT, error) {
	rows, err := p.store.pool.Query(ctx, `
		SELECT id, name, prefix, created_at, last_used_at, use_count, expires_at,
		       (expires_at IS NOT NULL AND expires_at <= now())
		FROM personal_access_tokens
		WHERE user_id = $1 AND revoked_at IS NULL
		ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: 列出个人访问令牌失败: %w", err)
	}
	defer rows.Close()
	out := []PAT{}
	for rows.Next() {
		var t PAT
		if err := rows.Scan(&t.ID, &t.Name, &t.Prefix, &t.CreatedAt, &t.LastUsedAt,
			&t.UseCount, &t.ExpiresAt, &t.Expired); err != nil {
			return nil, fmt.Errorf("store: 读取个人访问令牌失败: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CountActive 统计某用户未吊销且未过期的令牌数（每用户上限的口径）。
func (p *PersonalAccessTokens) CountActive(ctx context.Context, userID int64) (int, error) {
	var n int
	if err := p.store.pool.QueryRow(ctx, `
		SELECT count(*) FROM personal_access_tokens
		WHERE user_id = $1 AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > now())`, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: 统计个人访问令牌失败: %w", err)
	}
	return n, nil
}

// Revoke 吊销某用户自己的一把令牌。不是他的、不存在或已吊销都回 false，
// 调用方据此统一回 404（不暴露存在性）。
func (p *PersonalAccessTokens) Revoke(ctx context.Context, userID, id int64) (bool, error) {
	tag, err := p.store.pool.Exec(ctx, `
		UPDATE personal_access_tokens SET revoked_at = now()
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, id, userID)
	if err != nil {
		return false, fmt.Errorf("store: 吊销个人访问令牌失败: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RevokeUser 吊销某用户的全部未吊销令牌，返回吊销的行数。
//
// 停用账号、本人改密、邮件找回重置、管理员代重置四处调用 —— 与清会话同一个位置。
// 行保留（软吊销），由 GC 在满 30 天后清理。
func (p *PersonalAccessTokens) RevokeUser(ctx context.Context, userID int64) (int64, error) {
	tag, err := p.store.pool.Exec(ctx, `
		UPDATE personal_access_tokens SET revoked_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	if err != nil {
		return 0, fmt.Errorf("store: 吊销用户全部个人访问令牌失败: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Lookup 用明文换取账号与令牌行 id。
//
// 一次查询完成「存在 + 未吊销 + 未过期 + 账号未停用」。列清单与 users.go 的
// userCols 逐列对齐（带 u. 前缀）—— 漏一列不报错，只会让字段悄悄是零值
// （sessions.Lookup 的注释有前科）。
func (p *PersonalAccessTokens) Lookup(ctx context.Context, plain string) (*User, int64, error) {
	var u User
	var tokenID int64
	err := p.store.pool.QueryRow(ctx, `
		SELECT t.id, u.id, u.email, coalesce(u.password_hash, ''), u.role,
		       u.disabled_at, u.must_change_password, u.last_login_at, u.created_at,
		       coalesce(u.webhook_slug, ''), u.notify_email
		FROM personal_access_tokens t
		JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = $1
		  AND t.revoked_at IS NULL
		  AND (t.expires_at IS NULL OR t.expires_at > now())
		  AND u.disabled_at IS NULL`,
		hashToken(plain),
	).Scan(&tokenID, &u.ID, &u.Email, &u.PasswordHash, &u.Role,
		&u.DisabledAt, &u.MustChangePassword, &u.LastLoginAt, &u.CreatedAt,
		&u.WebhookSlug, &u.NotifyEmail)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, ErrPATInvalid
	}
	if err != nil {
		return nil, 0, fmt.Errorf("store: 查询个人访问令牌失败: %w", err)
	}
	return &u, tokenID, nil
}

// Touch 节流记录最后使用时间：距上次记录不足 60 秒时什么也不写。
//
// 判定与写入同一条语句，没有先查后写的窗口；丢失窗口内某次的精确时刻可接受，
// use_count 因此是近似值。
func (p *PersonalAccessTokens) Touch(ctx context.Context, tokenID int64) error {
	if _, err := p.store.pool.Exec(ctx, `
		UPDATE personal_access_tokens
		   SET last_used_at = now(), use_count = use_count + 1
		 WHERE id = $1
		   AND (last_used_at IS NULL OR last_used_at < now() - interval '60 seconds')`,
		tokenID); err != nil {
		return fmt.Errorf("store: 更新个人访问令牌使用时间失败: %w", err)
	}
	return nil
}

// GC 清理吊销或过期满 30 天的令牌行，返回删除行数。
//
// 这是 PAT 行清理的唯一归属（不进 Sessions.GC，免得混进会话的计数与日志）。
// 留 30 天是为了回答「这把钥匙是什么时候没的」。
func (p *PersonalAccessTokens) GC(ctx context.Context) (int64, error) {
	tag, err := p.store.pool.Exec(ctx, `
		DELETE FROM personal_access_tokens
		WHERE (revoked_at IS NOT NULL AND revoked_at < now() - interval '30 days')
		   OR (expires_at IS NOT NULL AND expires_at < now() - interval '30 days')`)
	if err != nil {
		return 0, fmt.Errorf("store: 清理个人访问令牌失败: %w", err)
	}
	return tag.RowsAffected(), nil
}
