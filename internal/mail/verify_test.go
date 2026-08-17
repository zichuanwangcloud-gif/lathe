package mail

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/textproto"
	"strings"
	"testing"

	"github.com/Clouditera/lathe/internal/store"
)

// fakeSMTP 起一个只按脚本应答的最小 SMTP 服务端。
//
// 用它而不是真的邮件服务器：能精确制造 535 / 550 / 4xx 等分支，
// 且不依赖任何外部服务。
type fakeSMTP struct {
	addr string
	// 各阶段的应答，留空则用默认的成功应答
	authReply string
	mailReply string
	rcptReply string
	// 是否在 EHLO 里宣告 STARTTLS
	offerStartTLS bool
	received      chan string
}

func startFakeSMTP(t *testing.T, f *fakeSMTP) *fakeSMTP {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	f.addr = ln.Addr().String()
	f.received = make(chan string, 4)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeSMTP) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	send := func(s string) {
		_, _ = w.WriteString(s + "\r\n")
		_ = w.Flush()
	}
	reply := func(custom, def string) string {
		if custom != "" {
			return custom
		}
		return def
	}

	send("220 fake.smtp.local ESMTP")
	var body strings.Builder
	inData := false

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if inData {
			if strings.TrimRight(line, "\r\n") == "." {
				inData = false
				select {
				case f.received <- body.String():
				default:
				}
				send("250 2.0.0 Ok: queued")
				continue
			}
			body.WriteString(line)
			continue
		}

		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			if f.offerStartTLS {
				send("250-fake.smtp.local\r\n250-STARTTLS\r\n250 AUTH PLAIN LOGIN")
			} else {
				send("250-fake.smtp.local\r\n250 AUTH PLAIN LOGIN")
			}
		case strings.HasPrefix(cmd, "AUTH"):
			send(reply(f.authReply, "235 2.7.0 Authentication successful"))
		case strings.HasPrefix(cmd, "MAIL FROM"):
			send(reply(f.mailReply, "250 2.1.0 Ok"))
		case strings.HasPrefix(cmd, "RCPT TO"):
			send(reply(f.rcptReply, "250 2.1.5 Ok"))
		case strings.HasPrefix(cmd, "DATA"):
			inData = true
			send("354 End data with <CR><LF>.<CR><LF>")
		case strings.HasPrefix(cmd, "QUIT"):
			send("221 2.0.0 Bye")
			return
		default:
			send("250 2.0.0 Ok")
		}
	}
}

// cfgFor 生成指向假服务器的配置。
func cfgFor(f *fakeSMTP, mode string) store.SMTPConfig {
	host, portStr, _ := net.SplitHostPort(f.addr)
	port := 0
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}
	return store.SMTPConfig{
		Host: host, Port: port,
		FromAddr: "lathe@example.com", FromName: "Lathe",
		TLSMode: mode,
	}
}

func TestVerifySMTPSuccess(t *testing.T) {
	f := startFakeSMTP(t, &fakeSMTP{})
	res := Verifier{}.VerifySMTP(context.Background(), cfgFor(f, store.TLSNone), "", "to@example.com")

	if !res.OK {
		t.Fatalf("应验证通过，得到: %s", res.Error)
	}
	if !strings.Contains(res.Detail, "to@example.com") {
		t.Errorf("说明里应写清投递到了哪，得到 %q", res.Detail)
	}

	select {
	case body := <-f.received:
		if !strings.Contains(body, "Subject: =?UTF-8?") {
			t.Errorf("服务端收到的报文主题应是编码过的:\n%s", body)
		}
	default:
		t.Fatal("服务端应当真的收到一封信 —— 只连上不投递就算过是危险的")
	}
}

func TestVerifySMTPRelayDenied(t *testing.T) {
	f := startFakeSMTP(t, &fakeSMTP{rcptReply: "550 5.7.1 Relay access denied"})
	res := Verifier{}.VerifySMTP(context.Background(), cfgFor(f, store.TLSNone), "", "to@example.com")

	if res.OK {
		t.Fatal("中继被拒时不该算通过")
	}
	// 这正是「只查到 AUTH 就算过」会漏掉的失败模式
	if !strings.Contains(res.Error, "中继") {
		t.Errorf("应说明是中继/收件地址问题，得到 %q", res.Error)
	}
}

func TestVerifySMTPSenderRejected(t *testing.T) {
	f := startFakeSMTP(t, &fakeSMTP{mailReply: "553 5.7.1 Sender address rejected"})
	res := Verifier{}.VerifySMTP(context.Background(), cfgFor(f, store.TLSNone), "", "to@example.com")

	if res.OK {
		t.Fatal("发件地址被拒时不该算通过")
	}
	if !strings.Contains(res.Error, "发件地址") {
		t.Errorf("应指出是发件地址的问题，得到 %q", res.Error)
	}
}

func TestVerifySMTPTemporaryFailure(t *testing.T) {
	f := startFakeSMTP(t, &fakeSMTP{rcptReply: "421 4.7.0 Too many requests"})
	res := Verifier{}.VerifySMTP(context.Background(), cfgFor(f, store.TLSNone), "", "to@example.com")

	if res.OK {
		t.Fatal("4xx 不该算通过")
	}
	if !strings.Contains(res.Error, "稍后再试") {
		t.Errorf("应提示这是临时失败，得到 %q", res.Error)
	}
}

// 服务器不宣告 STARTTLS 时必须失败，绝不能静默降级成明文 ——
// 降级意味着紧接着的 AUTH 会把密码明文发到网线上。
func TestVerifySMTPRefusesToDowngradeFromStartTLS(t *testing.T) {
	f := startFakeSMTP(t, &fakeSMTP{offerStartTLS: false})
	res := Verifier{}.VerifySMTP(context.Background(), cfgFor(f, store.TLSStartTLS), "pw", "to@example.com")

	if res.OK {
		t.Fatal("服务器不支持 STARTTLS 时不该算通过")
	}
	if !strings.Contains(res.Error, "STARTTLS") {
		t.Errorf("应说明服务器不支持 STARTTLS，得到 %q", res.Error)
	}
}

// 明文连接上不许发用户名密码。Go 自己也会拒，但那句英文错误没人看得懂。
func TestVerifySMTPRefusesPlaintextAuth(t *testing.T) {
	f := startFakeSMTP(t, &fakeSMTP{})
	cfg := cfgFor(f, store.TLSNone)
	cfg.Username = "someone"

	res := Verifier{}.VerifySMTP(context.Background(), cfg, "pw", "to@example.com")
	if res.OK {
		t.Fatal("明文连接上带认证不该算通过")
	}
	if !strings.Contains(res.Error, "未加密") {
		t.Errorf("应说清为什么拒绝，得到 %q", res.Error)
	}
}

func TestVerifySMTPUnreachableHost(t *testing.T) {
	// 监听后立刻关掉，端口必然连不上
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	f := &fakeSMTP{addr: addr}
	res := Verifier{}.VerifySMTP(context.Background(), cfgFor(f, store.TLSNone), "", "to@example.com")

	if res.OK {
		t.Fatal("连不上时不该算通过")
	}
	if !strings.Contains(res.Error, "连不上") {
		t.Errorf("应给出端口/加密方式的排查线索，得到 %q", res.Error)
	}
}

func TestVerifySMTPTLSModeMismatch(t *testing.T) {
	f := startFakeSMTP(t, &fakeSMTP{})
	// 对一个明文端口发起直接 TLS 握手
	res := Verifier{}.VerifySMTP(context.Background(), cfgFor(f, store.TLSImplicit), "", "to@example.com")

	if res.OK {
		t.Fatal("对明文端口做 TLS 握手不该成功")
	}
	if !strings.Contains(res.Error, "STARTTLS") {
		t.Errorf("应建议改用 STARTTLS，得到 %q", res.Error)
	}
}

// 服务器接上立刻挂断 —— 这是用明文/STARTTLS 连 465 时的典型现象，
// 用户看到的不该是裸的「EOF」。
func TestVerifySMTPServerClosesConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	f := &fakeSMTP{addr: ln.Addr().String()}
	res := Verifier{}.VerifySMTP(context.Background(), cfgFor(f, store.TLSStartTLS), "", "to@example.com")

	if res.OK {
		t.Fatal("被挂断时不该算通过")
	}
	if !strings.Contains(res.Error, "465") {
		t.Errorf("应给出加密方式与端口不匹配的排查线索，得到 %q", res.Error)
	}
}

// 用户名是短格式、发件地址是完整地址时（163 上最常见的配错），
// 提示里必须把两个值摆出来对比，而不是泛泛地说「要求一致」。
func TestClassifySenderRejectedShowsMismatch(t *testing.T) {
	err := fmt.Errorf("发件地址被拒绝: %w",
		&textproto.Error{Code: 553, Msg: "Mail from must equal authorized user"})
	cfg := store.SMTPConfig{
		Host: "smtp.163.com", Port: 465,
		Username: "sy_wzch", FromAddr: "sy_wzch@163.com",
		TLSMode: store.TLSImplicit,
	}

	res := classify(err, cfg)
	if res.OK {
		t.Fatal("553 不该算通过")
	}
	if !strings.Contains(res.Error, `"sy_wzch"`) {
		t.Errorf("应指出认证用户名的当前值，得到 %q", res.Error)
	}
}

func TestVerifySMTPRejectsInvalidConfig(t *testing.T) {
	res := Verifier{}.VerifySMTP(context.Background(), store.SMTPConfig{}, "", "to@example.com")
	if res.OK {
		t.Fatal("空配置不该通过")
	}
}
