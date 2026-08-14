// Package mail 按需读取 SMTP 配置并发信。
//
// 不在启动时把配置固定住：SMTP 可以在设置页里随时改，改完必须即刻生效
// —— 与 creds.Provider 每次现取凭据的理由一样。这里连缓存都不做：
// 发信只发生在「忘记密码」这一条低频路径上，每次多查一次库可以忽略，
// 省掉一个需要在保存时记得调用的 Invalidate 钩子。
package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"time"

	"github.com/Clouditera/lathe/internal/store"
)

// ErrNotConfigured 表示尚未配置发信通道。
var ErrNotConfigured = errors.New("mail: 未配置 SMTP，无法发信")

// dialTimeout 是连接与握手的上限。
//
// 发信在后台 goroutine 里跑，超时只影响那一封信；但没有超时的话
// 一个不响应的 SMTP 主机会把 goroutine 永久挂住。
const dialTimeout = 15 * time.Second

// Loader 提供当前的 SMTP 配置与明文密码。
//
// store.Secrets.LoadSMTP 的签名恰好就是它。
type Loader func(ctx context.Context) (store.SMTPConfig, string, error)

// Sender 发送邮件。
type Sender struct {
	load Loader
}

// NewSender 构造发信器。
func NewSender(load Loader) *Sender { return &Sender{load: load} }

// Ready 报告 SMTP 是否已配置好，供上层提示「未配 SMTP，找回密码不可用」。
func (s *Sender) Ready(ctx context.Context) bool {
	cfg, _, err := s.load(ctx)
	return err == nil && cfg.Validate() == nil
}

// Send 发一封纯文本邮件。
func (s *Sender) Send(ctx context.Context, to, subject, body string) error {
	cfg, password, err := s.load(ctx)
	if err != nil {
		if errors.Is(err, store.ErrSMTPNotConfigured) {
			return ErrNotConfigured
		}
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	msg, err := Compose(cfg, to, subject, body)
	if err != nil {
		return err
	}
	return deliver(ctx, cfg, password, to, msg)
}

// deliver 建立连接并投递一封已编排好的报文。
func deliver(ctx context.Context, cfg store.SMTPConfig, password, to string, msg []byte) error {
	c, err := dial(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	if err := startTLSIfNeeded(c, cfg); err != nil {
		return err
	}
	if err := authenticate(c, cfg, password); err != nil {
		return err
	}

	if err := c.Mail(cfg.FromAddr); err != nil {
		return fmt.Errorf("发件地址被拒绝: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("收件地址被拒绝: %w", err)
	}

	wc, err := c.Data()
	if err != nil {
		return fmt.Errorf("准备投递失败: %w", err)
	}
	if _, err := wc.Write(msg); err != nil {
		_ = wc.Close()
		return fmt.Errorf("写入邮件正文失败: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("投递失败: %w", err)
	}
	return c.Quit()
}

// dial 按加密方式建立到 SMTP 服务器的连接。
func dial(ctx context.Context, cfg store.SMTPConfig) (*smtp.Client, error) {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	d := &net.Dialer{Timeout: dialTimeout}

	if cfg.TLSMode == store.TLSImplicit {
		// 隐式 TLS：一上来就握手（465 端口的常见做法）
		conn, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: cfg.Host})
		if err != nil {
			return nil, err
		}
		return smtp.NewClient(conn, cfg.Host)
	}

	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	return smtp.NewClient(conn, cfg.Host)
}

// startTLSIfNeeded 在 starttls 模式下把明文连接升级为加密连接。
//
// 服务器不宣告 STARTTLS 时直接失败，绝不静默降级 —— 降级意味着
// 待会儿的 AUTH 会把密码明文发到网线上。
func startTLSIfNeeded(c *smtp.Client, cfg store.SMTPConfig) error {
	if cfg.TLSMode != store.TLSStartTLS {
		return nil
	}
	if ok, _ := c.Extension("STARTTLS"); !ok {
		return errors.New("服务器不支持 STARTTLS")
	}
	return c.StartTLS(&tls.Config{ServerName: cfg.Host})
}

// authenticate 在需要时做 AUTH。
func authenticate(c *smtp.Client, cfg store.SMTPConfig, password string) error {
	if cfg.Username == "" {
		return nil // 匿名中继
	}
	// 明文连接上发用户名密码，Go 自己也会拒绝（smtp.PlainAuth 的
	// unencrypted connection 检查）。与其让人撞上那句英文错误，
	// 不如提前说清楚该怎么改。
	if cfg.TLSMode == store.TLSNone {
		return errors.New("未加密连接下不能发送用户名密码，请改用 STARTTLS/TLS，或留空用户名走匿名中继")
	}
	return c.Auth(smtp.PlainAuth("", cfg.Username, password, cfg.Host))
}
