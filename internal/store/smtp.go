package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Clouditera/lathe/internal/secret"
	"github.com/jackc/pgx/v5"
)

// SMTP 的加密方式。与 smtp_settings 的 tls_mode CHECK 约束一致。
const (
	// TLSStartTLS 是明文连接后用 STARTTLS 升级（587 端口的常见做法）。
	TLSStartTLS = "starttls"
	// TLSImplicit 是一上来就 TLS（465 端口的常见做法）。
	TLSImplicit = "tls"
	// TLSNone 是完全不加密，只适合内网自建中继。
	TLSNone = "none"
)

// ValidTLSMode 报告加密方式取值是否合法。
func ValidTLSMode(m string) bool {
	return m == TLSStartTLS || m == TLSImplicit || m == TLSNone
}

// ErrSMTPNotConfigured 表示尚未配置发信通道。
var ErrSMTPNotConfigured = errors.New("store: SMTP 未配置")

// SMTPConfig 是发信通道的非机密配置。
type SMTPConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	FromAddr string `json:"fromAddr"`
	FromName string `json:"fromName"`
	TLSMode  string `json:"tlsMode"`
}

// Validate 校验配置是否可用于发信。
func (c SMTPConfig) Validate() error {
	if c.Host == "" {
		return errors.New("SMTP 服务器地址不能为空")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return errors.New("SMTP 端口须在 1..65535 之间")
	}
	if c.FromAddr == "" {
		return errors.New("发件地址不能为空")
	}
	if !ValidTLSMode(c.TLSMode) {
		return errors.New("加密方式须为 starttls / tls / none 之一")
	}
	return nil
}

// SMTPStatus 是可安全展示给界面的发信配置状态。
//
// 与 IntegrationStatus 同一套约定：密码只给掩码，界面永远拿不到明文。
type SMTPStatus struct {
	Configured bool `json:"configured"`
	SMTPConfig

	PasswordSet    bool       `json:"passwordSet"`
	PasswordMasked string     `json:"passwordMasked"`
	VerifiedAt     *time.Time `json:"verifiedAt"`
	VerifyError    *string    `json:"verifyError"`
	UpdatedAt      *time.Time `json:"updatedAt"`
}

// SMTPStatus 读取发信配置状态。未配置时返回 Configured=false 而非错误 ——
// 界面要能渲染一张空白的待填卡片。
func (s *Secrets) SMTPStatus(ctx context.Context) (*SMTPStatus, error) {
	var (
		st  SMTPStatus
		enc []byte
	)
	err := s.store.pool.QueryRow(ctx, `
		SELECT host, port, username, password_enc, from_addr, from_name, tls_mode,
		       verified_at, verify_error, updated_at
		FROM smtp_settings WHERE id = 1`,
	).Scan(&st.Host, &st.Port, &st.Username, &enc, &st.FromAddr, &st.FromName, &st.TLSMode,
		&st.VerifiedAt, &st.VerifyError, &st.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// 给界面一个可用的默认值，省得每个前端各写一份
		return &SMTPStatus{SMTPConfig: SMTPConfig{Port: 587, TLSMode: TLSStartTLS, FromName: "Lathe"}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读取 SMTP 配置失败: %w", err)
	}

	st.Configured = true
	if len(enc) > 0 {
		st.PasswordSet = true
		// 掩码要先解密；解不开说明主密钥换了，如实告知而不是装作配好了
		if plain, err := s.sealer.Open(enc); err == nil {
			st.PasswordMasked = secret.Mask(plain)
		} else {
			st.PasswordSet = false
			msg := "SMTP 密码无法解密（主密钥可能已更换），请重新填写"
			st.VerifyError = &msg
		}
	}
	return &st, nil
}

// SaveSMTP 保存发信配置。
//
// password 为空串表示「保留原密码」—— 界面上密码框留空即不改，这样管理员
// 改个端口不必重新翻一遍密码。要清空密码得走 DeleteSMTP。
//
// 与 Secrets.Save 一致：保存时清空上一次的验证结果，配置变了旧结论就不再有效。
func (s *Secrets) SaveSMTP(ctx context.Context, cfg SMTPConfig, password string) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	// nil 让 COALESCE 保留原值；非 nil 则覆盖
	var enc any
	if password != "" {
		sealed, err := s.sealer.Seal(password)
		if err != nil {
			return err
		}
		enc = sealed
	}

	if _, err := s.store.pool.Exec(ctx, `
		INSERT INTO smtp_settings
			(id, host, port, username, password_enc, from_addr, from_name, tls_mode)
		VALUES (1, $1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (id) DO UPDATE SET
			host         = EXCLUDED.host,
			port         = EXCLUDED.port,
			username     = EXCLUDED.username,
			password_enc = COALESCE(EXCLUDED.password_enc, smtp_settings.password_enc),
			from_addr    = EXCLUDED.from_addr,
			from_name    = EXCLUDED.from_name,
			tls_mode     = EXCLUDED.tls_mode,
			verified_at  = NULL,
			verify_error = NULL,
			updated_at   = now()`,
		cfg.Host, cfg.Port, cfg.Username, enc, cfg.FromAddr, cfg.FromName, cfg.TLSMode); err != nil {
		return fmt.Errorf("store: 保存 SMTP 配置失败: %w", err)
	}
	return nil
}

// LoadSMTP 读出配置与明文密码，供发信与验证使用。
//
// 这是唯一会返回明文密码的方法，只给 internal/mail 用 —— 面向界面的那条路
// 走 SMTPStatus，拿到的是掩码。
func (s *Secrets) LoadSMTP(ctx context.Context) (SMTPConfig, string, error) {
	var (
		cfg SMTPConfig
		enc []byte
	)
	err := s.store.pool.QueryRow(ctx, `
		SELECT host, port, username, password_enc, from_addr, from_name, tls_mode
		FROM smtp_settings WHERE id = 1`,
	).Scan(&cfg.Host, &cfg.Port, &cfg.Username, &enc, &cfg.FromAddr, &cfg.FromName, &cfg.TLSMode)
	if errors.Is(err, pgx.ErrNoRows) {
		return cfg, "", ErrSMTPNotConfigured
	}
	if err != nil {
		return cfg, "", fmt.Errorf("store: 读取 SMTP 配置失败: %w", err)
	}
	if len(enc) == 0 {
		return cfg, "", nil // 匿名中继，没有密码
	}
	plain, err := s.sealer.Open(enc)
	if err != nil {
		return cfg, "", err
	}
	return cfg, plain, nil
}

// MarkSMTPVerified 记录一次成功的发信验证。
func (s *Secrets) MarkSMTPVerified(ctx context.Context) error {
	if _, err := s.store.pool.Exec(ctx, `
		UPDATE smtp_settings SET verified_at = now(), verify_error = NULL, updated_at = now()
		WHERE id = 1`); err != nil {
		return fmt.Errorf("store: 记录 SMTP 验证结果失败: %w", err)
	}
	return nil
}

// MarkSMTPVerifyFailed 记录一次失败的发信验证及原因。
func (s *Secrets) MarkSMTPVerifyFailed(ctx context.Context, reason string) error {
	if _, err := s.store.pool.Exec(ctx, `
		UPDATE smtp_settings SET verify_error = $1, verified_at = NULL, updated_at = now()
		WHERE id = 1`, reason); err != nil {
		return fmt.Errorf("store: 记录 SMTP 验证失败原因失败: %w", err)
	}
	return nil
}

// DeleteSMTP 清除发信配置（含密码）。
func (s *Secrets) DeleteSMTP(ctx context.Context) error {
	if _, err := s.store.pool.Exec(ctx, `DELETE FROM smtp_settings WHERE id = 1`); err != nil {
		return fmt.Errorf("store: 删除 SMTP 配置失败: %w", err)
	}
	return nil
}
