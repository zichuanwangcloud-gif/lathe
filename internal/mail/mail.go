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
	"sync"
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

// sessionTimeout 是整个 SMTP 会话的总预算（TCP connect 之后开始计）。
//
// 为什么光有 dialTimeout 不够：net.Dialer.Timeout 只作用于 TCP connect
// 与随后的 TLS 握手。连上之后，conn 上没有任何 deadline，每一次 SMTP
// 命令（等 220 欢迎语、EHLO、AUTH、MAIL FROM、RCPT TO、DATA 的收尾
// 响应）都会阻塞在网络读上直到对端给出字节或 EOF。
//
// 那个「能建立 TCP 连接但从不应答」的服务器（防火墙静默 DROP、
// SMTP 假死、负载均衡把包吞掉）因此能把发信永久挂住：connect 秒过，
// 然后卡在第一个 read 上等一辈子。ctx 取消也救不了——net/smtp 不感知
// ctx，阻塞在 syscall.Read 上的 goroutine 只能靠 conn 上的 deadline
// 或 Close 打断（下面 watchContext 做的正是同一件事的另一半）。
//
// 20s 的取值：比 dialTimeout 略宽，给一个真实服务器的 banner + EHLO +
// STARTTLS + AUTH + DATA 留足余量（公网 RTT 也就几百毫秒，余量几乎是
// 两个数量级），同时保证假死情形下 SendTaskMail 在有界时间内返回。
const sessionTimeout = 20 * time.Second

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
//
// 整条链路被两个机制夹住，缺一不可：
//
//  1. conn 上的绝对 deadline（sessionTimeout）—— 每次读写的最后防线，
//     假死的对端在这一刻被强制解开。
//  2. watchContext 的 ctx 监视 —— 调用方提前取消（用户关掉设置页、
//     关服）时立刻 Close 连接，不必等满 sessionTimeout。
//
// 两者都指向同一个 conn：net/smtp 不感知 context，把 conn 关掉/设上
// deadline 是唯一能打断阻塞读的手段。
func deliver(ctx context.Context, cfg store.SMTPConfig, password, to string, msg []byte) error {
	// ctx 监视由 dial 装配（它必须早于 smtp.NewClient 读 220 欢迎语那一步，
	// 详见 newSMTPClient 的注释）。这里只负责在返回前停掉它。
	c, stopWatch, err := dial(ctx, cfg)
	if err != nil {
		return err
	}
	defer stopWatch()
	defer func() { _ = c.Close() }()

	// 会话总预算已在 newSMTPClient 里从「连接已建立」那一刻起算，覆盖
	// 等欢迎语在内的每一步；这里不再重设，否则等于把已经过去的握手
	// 时间又白送一次。

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

// smtpClient 是带 conn 句柄的 smtp.Client。
//
// net/smtp 把底层 conn 藏在未导出字段里，客户端只能拿到 *smtp.Client
// 这个黑盒——而「给会话设 deadline」恰恰需要那个 conn。这里在 Dial 的
// 出口把它记下来，此后 deliver 才设得上 SetDeadline。
//
// 用组合而不是给 *smtp.Client 建别名：方法集原样继承（Mail/Rcpt/Data/
// Quit/StartTLS/Auth 直接可用），只多出一个 conn 字段。
type smtpClient struct {
	*smtp.Client
	conn net.Conn
}

// watchContext 监视 ctx，取消时关闭连接。
//
// 返回的停止函数必须被调用（defer 即可）—— 否则这个 goroutine 会一直
// 活到 ctx 被取消为止，而 ctx 常常是长命的后台任务 context。
//
// 停止函数用 sync.Once 兜住重复调用：close 一个已关闭的 channel 会
// panic，而这个函数的返回值天然是拿来 defer 的，防一手比让调用点
// 去记「只能调一次」更稳妥。
func watchContext(ctx context.Context, conn net.Conn) (stop func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// 关连接是打断阻塞读的唯一手段。这里的错误只可能是
			// 「已经关过了」，无需上报。
			_ = conn.Close()
		case <-done:
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// dial 按加密方式建立到 SMTP 服务器的连接。
//
// 两条分支都返回带 conn 的 smtpClient，理由见上面 smtpClient 的注释。
func dial(ctx context.Context, cfg store.SMTPConfig) (*smtpClient, func(), error) {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	d := &net.Dialer{Timeout: dialTimeout}

	if cfg.TLSMode == store.TLSImplicit {
		// 隐式 TLS：一上来就握手（465 端口的常见做法）
		conn, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: cfg.Host})
		if err != nil {
			return nil, nil, err
		}
		return newSMTPClient(ctx, conn, cfg.Host)
	}

	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	return newSMTPClient(ctx, conn, cfg.Host)
}

// newSMTPClient 包一层 smtp.Client，并在构造失败时自己关掉连接。
//
// smtp.NewClient 会同步读服务器的 220 欢迎语：对端只连不应答时，
// 它在这里就会阻塞。所以 deadline 要在构造【之前】设——但 deadline
// 又只能设在包装好的 conn 上，于是这个先设后包的顺序是必须的。
//
// 构造失败时连接归调用方管（smtp.NewClient 的约定是失败即不接管），
// 不关就是一次 fd 泄漏；这是「传进来的 conn 一定不会漏」的唯一出口。
func newSMTPClient(ctx context.Context, conn net.Conn, host string) (*smtpClient, func(), error) {
	if err := conn.SetDeadline(time.Now().Add(sessionTimeout)); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("设置会话超时失败: %w", err)
	}

	// ctx 监视必须装在 smtp.NewClient **之前**：那一步会同步读服务器的
	// 220 欢迎语，而这正是「能连上但从不应答」的对端最常卡住的地方。
	// 装在它之后，调用方取消就只能靠 conn deadline 兜住——干等满
	// sessionTimeout，监视器形同虚设。
	stop := watchContext(ctx, conn)

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		stop()
		_ = conn.Close()
		return nil, nil, err
	}
	return &smtpClient{Client: c, conn: conn}, stop, nil
}

// startTLSIfNeeded 在 starttls 模式下把明文连接升级为加密连接。
//
// 服务器不宣告 STARTTLS 时直接失败，绝不静默降级 —— 降级意味着
// 待会儿的 AUTH 会把密码明文发到网线上。
func startTLSIfNeeded(c *smtpClient, cfg store.SMTPConfig) error {
	if cfg.TLSMode != store.TLSStartTLS {
		return nil
	}
	if ok, _ := c.Extension("STARTTLS"); !ok {
		return errors.New("服务器不支持 STARTTLS")
	}
	return c.StartTLS(&tls.Config{ServerName: cfg.Host})
}

// authenticate 在需要时做 AUTH。
func authenticate(c *smtpClient, cfg store.SMTPConfig, password string) error {
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
