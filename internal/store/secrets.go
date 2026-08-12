package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Clouditera/lathe/internal/secret"
	"github.com/jackc/pgx/v5"
)

// 凭据类型。与 integrations 表的 kind CHECK 约束一致。
const (
	KindLinear        = "linear"
	KindLinearWebhook = "linear_webhook"
	KindGitHub        = "github"
)

// KnownKinds 是界面可配置的凭据类型。
var KnownKinds = []string{KindLinear, KindLinearWebhook, KindGitHub}

// ValidKind 报告 kind 是否可配置。
func ValidKind(kind string) bool {
	for _, k := range KnownKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// ErrIntegrationNotFound 表示该类型的凭据尚未配置。
var ErrIntegrationNotFound = errors.New("store: 凭据未配置")

// IntegrationStatus 是可安全展示给界面的凭据状态。
//
// 刻意不含凭据本身 —— 只有 Masked（末尾几位）供人确认配的是哪一个。
type IntegrationStatus struct {
	Kind        string     `json:"kind"`
	Configured  bool       `json:"configured"`
	Masked      string     `json:"masked"`
	AccountName *string    `json:"accountName"`
	VerifiedAt  *time.Time `json:"verifiedAt"`
	VerifyError *string    `json:"verifyError"`
	// Source 说明凭据从哪来：db（界面配置）或 env（环境变量兜底）。
	Source string `json:"source"`
}

// Secrets 读写加密凭据。
type Secrets struct {
	sealer secret.Sealer
	store  *Store
}

// NewSecrets 构造凭据读写器。
func (s *Store) NewSecrets(sealer secret.Sealer) *Secrets {
	return &Secrets{sealer: sealer, store: s}
}

// Save 加密保存凭据。同一 user_id + kind 覆盖写。
//
// 保存时清空上一次的验证结果：凭据变了，旧的验证结论不再有效，
// 界面上继续显示「已验证」会误导人。
func (s *Secrets) Save(ctx context.Context, userID int64, kind, plaintext string) error {
	if !ValidKind(kind) {
		return fmt.Errorf("store: 未知凭据类型 %q", kind)
	}
	enc, err := s.sealer.Seal(plaintext)
	if err != nil {
		return err
	}

	_, err = s.store.pool.Exec(ctx, `
		INSERT INTO integrations (user_id, kind, secret_enc, token_ref, scopes)
		VALUES ($1, $2, $3, NULL, '{}')
		ON CONFLICT (user_id, kind) DO UPDATE SET
			secret_enc   = EXCLUDED.secret_enc,
			account_name = NULL,
			verified_at  = NULL,
			verify_error = NULL,
			updated_at   = now()`,
		userID, kind, enc)
	if err != nil {
		return fmt.Errorf("store: 保存凭据失败: %w", err)
	}
	return nil
}

// Get 解密读取凭据。
func (s *Secrets) Get(ctx context.Context, userID int64, kind string) (string, error) {
	var enc []byte
	err := s.store.pool.QueryRow(ctx,
		`SELECT secret_enc FROM integrations WHERE user_id = $1 AND kind = $2`,
		userID, kind).Scan(&enc)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrIntegrationNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: 读取凭据失败: %w", err)
	}
	if len(enc) == 0 {
		return "", ErrIntegrationNotFound
	}
	return s.sealer.Open(enc)
}

// Delete 删除凭据。
func (s *Secrets) Delete(ctx context.Context, userID int64, kind string) error {
	if _, err := s.store.pool.Exec(ctx,
		`DELETE FROM integrations WHERE user_id = $1 AND kind = $2`, userID, kind); err != nil {
		return fmt.Errorf("store: 删除凭据失败: %w", err)
	}
	return nil
}

// MarkVerified 记录一次成功的验证结果。
func (s *Secrets) MarkVerified(ctx context.Context, userID int64, kind, accountName string) error {
	if _, err := s.store.pool.Exec(ctx, `
		UPDATE integrations SET
			account_name = $3, verified_at = now(), verify_error = NULL, updated_at = now()
		WHERE user_id = $1 AND kind = $2`, userID, kind, accountName); err != nil {
		return fmt.Errorf("store: 记录验证结果失败: %w", err)
	}
	return nil
}

// MarkVerifyFailed 记录一次失败的验证结果。
//
// 保留失败原因供界面展示 —— 「连不上」和「令牌过期」需要不同的处理。
func (s *Secrets) MarkVerifyFailed(ctx context.Context, userID int64, kind, reason string) error {
	if _, err := s.store.pool.Exec(ctx, `
		UPDATE integrations SET
			verify_error = $3, verified_at = NULL, updated_at = now()
		WHERE user_id = $1 AND kind = $2`, userID, kind, reason); err != nil {
		return fmt.Errorf("store: 记录验证失败原因失败: %w", err)
	}
	return nil
}

// SetAccountName 更新账号名（例如 Linear 验证时顺带拿到的用户 ID）。
func (s *Secrets) SetAccountName(ctx context.Context, userID int64, kind, accountID string) error {
	if _, err := s.store.pool.Exec(ctx,
		`UPDATE integrations SET external_account_id = $3, updated_at = now()
		 WHERE user_id = $1 AND kind = $2`, userID, kind, accountID); err != nil {
		return fmt.Errorf("store: 更新账号标识失败: %w", err)
	}
	return nil
}

// ExternalAccountID 读取验证时保存的外部账号 ID。
//
// Linear 场景下这就是「只接指派给我的 issue」所需的用户 ID，
// 验证时自动获取，无需人工填写。
func (s *Secrets) ExternalAccountID(ctx context.Context, userID int64, kind string) (string, error) {
	var id *string
	err := s.store.pool.QueryRow(ctx,
		`SELECT external_account_id FROM integrations WHERE user_id = $1 AND kind = $2`,
		userID, kind).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) || id == nil {
		return "", ErrIntegrationNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: 读取账号标识失败: %w", err)
	}
	return *id, nil
}

// Status 返回全部可配置凭据的状态，未配置的也会出现（Configured=false），
// 这样界面能展示完整清单而不必自己拼。
func (s *Secrets) Status(ctx context.Context, userID int64) ([]IntegrationStatus, error) {
	rows, err := s.store.pool.Query(ctx, `
		SELECT kind, secret_enc, account_name, verified_at, verify_error
		FROM integrations WHERE user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询凭据状态失败: %w", err)
	}
	defer rows.Close()

	found := map[string]IntegrationStatus{}
	for rows.Next() {
		var (
			kind    string
			enc     []byte
			account *string
			at      *time.Time
			verr    *string
		)
		if err := rows.Scan(&kind, &enc, &account, &at, &verr); err != nil {
			return nil, fmt.Errorf("store: 读取凭据状态失败: %w", err)
		}

		st := IntegrationStatus{
			Kind: kind, Configured: len(enc) > 0,
			AccountName: account, VerifiedAt: at, VerifyError: verr,
			Source: "db",
		}
		// 掩码需要解密后才能生成；解不开说明主密钥换了，如实告知
		if len(enc) > 0 {
			if plain, err := s.sealer.Open(enc); err == nil {
				st.Masked = secret.Mask(plain)
			} else {
				st.Configured = false
				msg := "凭据无法解密（主密钥可能已更换），请重新配置"
				st.VerifyError = &msg
			}
		}
		found[kind] = st
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]IntegrationStatus, 0, len(KnownKinds))
	for _, k := range KnownKinds {
		if st, ok := found[k]; ok {
			out = append(out, st)
			continue
		}
		out = append(out, IntegrationStatus{Kind: k})
	}
	return out, nil
}
