package mail

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Clouditera/lathe/internal/store"
)

func testCfg() store.SMTPConfig {
	return store.SMTPConfig{
		Host: "smtp.example.com", Port: 587,
		FromAddr: "lathe@example.com", FromName: "Lathe",
		TLSMode: store.TLSStartTLS,
	}
}

func TestComposeEncodesChineseSubject(t *testing.T) {
	msg, err := Compose(testCfg(), "to@example.com", "【Lathe】重置你的登录密码", "正文")
	if err != nil {
		t.Fatal(err)
	}
	s := string(msg)

	// 中文主题必须走 RFC 2047 编码字，否则多数客户端显示成乱码
	if !strings.Contains(s, "Subject: =?UTF-8?") {
		t.Fatalf("主题应被 RFC 2047 编码，得到:\n%s", s)
	}
	// 原始中文不该以裸字节出现在头里
	if strings.Contains(strings.SplitN(s, "\r\n\r\n", 2)[0], "重置") {
		t.Fatal("邮件头里不该出现未编码的中文")
	}
}

func TestComposeBodyIsBase64AndRoundTrips(t *testing.T) {
	body := "第一行\n第二行\n.\n以点开头的一行"
	msg, err := Compose(testCfg(), "to@example.com", "test", body)
	if err != nil {
		t.Fatal(err)
	}

	parts := strings.SplitN(string(msg), "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatal("报文应有头体分隔的空行")
	}
	head, enc := parts[0], strings.ReplaceAll(parts[1], "\r\n", "")

	if !strings.Contains(head, "Content-Transfer-Encoding: base64") {
		t.Fatal("应声明 base64 编码")
	}
	got, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("正文不是合法 base64: %v", err)
	}
	if string(got) != body {
		t.Fatalf("正文应能原样解回，得到 %q", got)
	}
}

// 用 base64 的一个附带好处：正文里的单独一行 "." 不会提前终止报文。
// net/smtp 的 Data() writer 不做点填充，明文传就会踩这个坑。
func TestComposeBodyNeverStartsLineWithDot(t *testing.T) {
	msg, err := Compose(testCfg(), "to@example.com", "test", "上一行\n.\n下一行")
	if err != nil {
		t.Fatal(err)
	}
	body := strings.SplitN(string(msg), "\r\n\r\n", 2)[1]
	for _, line := range strings.Split(body, "\r\n") {
		if strings.HasPrefix(line, ".") {
			t.Fatalf("正文里不该出现以点开头的行: %q", line)
		}
	}
}

func TestComposeAllLinesUseCRLF(t *testing.T) {
	msg, err := Compose(testCfg(), "to@example.com", "test", strings.Repeat("长正文内容", 40))
	if err != nil {
		t.Fatal(err)
	}
	// 把 CRLF 去掉后不该还剩裸 LF
	if strings.Contains(strings.ReplaceAll(string(msg), "\r\n", ""), "\n") {
		t.Fatal("报文里出现了裸 LF，SMTP 要求 CRLF")
	}
}

// 地址里的换行会被用来往邮件头里注入额外字段（抄送给攻击者之类）。
func TestComposeRejectsHeaderInjection(t *testing.T) {
	cases := []struct{ name, to, subject string }{
		{"收件人换行", "victim@example.com\r\nBcc: attacker@evil.com", "test"},
		{"主题换行", "to@example.com", "test\r\nBcc: attacker@evil.com"},
		{"收件人带显示名", "Someone <to@example.com>", "test"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Compose(testCfg(), tc.to, tc.subject, "正文"); err == nil {
				t.Fatal("应当被拒绝")
			}
		})
	}
}

func TestComposeRejectsBadFromAddr(t *testing.T) {
	cfg := testCfg()
	cfg.FromAddr = "lathe@example.com\r\nBcc: attacker@evil.com"
	if _, err := Compose(cfg, "to@example.com", "test", "正文"); err == nil {
		t.Fatal("非法发件地址应被拒绝")
	}
}

func TestComposeIncludesFromDisplayName(t *testing.T) {
	msg, err := Compose(testCfg(), "to@example.com", "test", "正文")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(msg), `<lathe@example.com>`) {
		t.Fatal("From 头里应含发件地址")
	}
}
