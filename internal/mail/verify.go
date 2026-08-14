package mail

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/textproto"
	"strings"

	"github.com/Clouditera/lathe/internal/httpapi"
	"github.com/Clouditera/lathe/internal/store"
)

// Verifier 用给定配置实际投递一封测试邮件，验证发信通道可用。
type Verifier struct{}

// VerifySMTP 实现 httpapi.SMTPVerifier。
//
// 刻意走完整链路（连接 → 加密 → AUTH → MAIL FROM → RCPT TO → DATA），
// 而不是连上并认证成功就算过：只查到 AUTH 的话，「认证通过但服务器不给你
// 中继」这种配置会验证成功，然后真正的重置邮件在 RCPT 阶段被悄悄拒掉。
// 那是最难排查的失败模式，必须在验证阶段就暴露出来。
func (Verifier) VerifySMTP(ctx context.Context, cfg store.SMTPConfig, password, testTo string) httpapi.VerifyResult {
	if err := cfg.Validate(); err != nil {
		return httpapi.VerifyResult{OK: false, Error: err.Error()}
	}
	if testTo == "" {
		testTo = cfg.FromAddr
	}

	msg, err := Compose(cfg, testTo,
		"【Lathe】SMTP 配置测试",
		"这是一封测试邮件。\n\n收到它说明 Lathe 的发信通道已经配好，"+
			"「忘记密码」功能可以正常使用了。\n\n—— Lathe\n")
	if err != nil {
		return httpapi.VerifyResult{OK: false, Error: err.Error()}
	}

	if err := deliver(ctx, cfg, password, testTo, msg); err != nil {
		return classify(err, cfg)
	}
	return httpapi.VerifyResult{
		OK: true,
		Detail: fmt.Sprintf("已成功投递测试邮件到 %s。若收件箱里没有，请检查垃圾邮件。",
			testTo),
	}
}

// classify 把底层错误翻译成能指导下一步动作的说明。
//
// 沿用 Linear/GitHub verifier 的约定：绝不把原始传输错误直接甩给用户。
// 「connection refused」对配置的人毫无帮助，「端口可能填错了，587 走
// STARTTLS，465 走 TLS」才有。
func classify(err error, cfg store.SMTPConfig) httpapi.VerifyResult {
	bad := func(msg string) httpapi.VerifyResult {
		return httpapi.VerifyResult{OK: false, Error: msg}
	}

	// ---- 网络层 ----
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return bad(fmt.Sprintf("无法解析主机名 %s，请检查是否拼错", cfg.Host))
	}

	// ---- TLS 层 ----
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		return bad(fmt.Sprintf("TLS 证书与主机名 %s 不匹配", cfg.Host))
	}
	var authErr x509.UnknownAuthorityError
	if errors.As(err, &authErr) {
		return bad("TLS 证书不被信任（可能是自签证书）。若是内网自建服务器，可改用 STARTTLS 或不加密")
	}
	var recErr tls.RecordHeaderError
	if errors.As(err, &recErr) {
		return bad("TLS 握手失败：对一个非 TLS 端口发起了直接 TLS 连接。" +
			"加密方式改为 STARTTLS 试试（587 端口通常用 STARTTLS，465 才用 TLS）")
	}

	// ---- SMTP 协议层 ----
	// 服务端的拒绝都是 *textproto.Error，带一个三位响应码
	var protoErr *textproto.Error
	if errors.As(err, &protoErr) {
		if res, ok := classifyCode(protoErr, err, cfg); ok {
			return res
		}
	}

	msg := err.Error()
	switch {
	case strings.Contains(msg, "不支持 STARTTLS"):
		return bad("服务器不支持 STARTTLS。改用 TLS（通常是 465 端口），或确认端口填对了")
	case strings.Contains(msg, "未加密连接下不能发送用户名密码"):
		return bad(msg)
	case strings.Contains(msg, "connection refused"):
		return bad(fmt.Sprintf("连不上 %s:%d（端口没开或被拒绝）。587 走 STARTTLS，465 走 TLS，25 走不加密",
			cfg.Host, cfg.Port))
	case strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "deadline exceeded"):
		return bad(fmt.Sprintf("连接 %s:%d 超时，通常是端口填错或被防火墙拦截", cfg.Host, cfg.Port))
	}
	return bad("发信失败：" + msg)
}

// classifyCode 按 SMTP 响应码与出错阶段给出说明。
//
// 阶段信息来自 deliver 包装的中文前缀 —— 同一个 550 出现在 MAIL FROM
// 和 RCPT TO 时，该改的东西完全不同。
func classifyCode(protoErr *textproto.Error, err error, cfg store.SMTPConfig) (httpapi.VerifyResult, bool) {
	bad := func(msg string) (httpapi.VerifyResult, bool) {
		return httpapi.VerifyResult{OK: false, Error: msg}, true
	}
	stage := err.Error()

	switch protoErr.Code {
	case 535:
		return bad(fmt.Sprintf("用户名或密码不正确（535）。注意 Gmail、QQ、163 等邮箱"+
			"需要使用「应用专用密码」而不是登录密码；当前用户名为 %q", cfg.Username))
	case 534, 530:
		return bad(fmt.Sprintf("服务器要求先建立加密连接或使用其它认证方式（%d）", protoErr.Code))
	}

	// 4xx 先于按阶段分类：临时失败在哪个阶段发生都是临时失败。
	// 反过来的话，RCPT 阶段的 421 限流会被说成「收件地址不存在」，
	// 把人引去查地址，而正确的动作是过一会儿重试。
	if protoErr.Code >= 400 && protoErr.Code < 500 {
		return bad(fmt.Sprintf("服务器暂时拒绝（%d %s），可能是限流，稍后再试",
			protoErr.Code, protoErr.Msg))
	}

	switch {
	case strings.HasPrefix(stage, "发件地址被拒绝"):
		return bad(fmt.Sprintf("发件地址 %s 被拒绝（%d）：多数服务器要求发件地址与认证账号一致",
			cfg.FromAddr, protoErr.Code))
	case strings.HasPrefix(stage, "收件地址被拒绝"):
		return bad(fmt.Sprintf("收件地址被拒绝（%d）：地址不存在，或服务器不为你中继邮件",
			protoErr.Code))
	}

	return httpapi.VerifyResult{}, false
}
