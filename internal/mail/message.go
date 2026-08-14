package mail

import (
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/mail"
	"strings"
	"time"

	"github.com/Clouditera/lathe/internal/store"
)

// Compose 按 RFC 5322 编排一封纯文本邮件。
//
// 正文用 base64 而非 8bit，有两个理由：
//
//  1. 中文正文的行很容易超过 SMTP 的行长限制，base64 自带折行。
//  2. 顺带绕开 dot-stuffing —— net/smtp 的 Data() writer 不做点填充，
//     正文里出现单独一行 "." 会让报文提前结束、后半截被当成 SMTP 命令。
//     base64 的输出不可能产生以 "." 开头的行。
func Compose(cfg store.SMTPConfig, to, subject, body string) ([]byte, error) {
	if err := checkAddr(cfg.FromAddr); err != nil {
		return nil, fmt.Errorf("发件地址不合法: %w", err)
	}
	if err := checkAddr(to); err != nil {
		return nil, fmt.Errorf("收件地址不合法: %w", err)
	}
	// 主题里的换行会被用来注入额外的邮件头
	if strings.ContainsAny(subject, "\r\n") {
		return nil, errors.New("邮件主题不能含换行符")
	}

	from := (&mail.Address{Name: cfg.FromName, Address: cfg.FromAddr}).String()

	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	// 中文主题必须走 RFC 2047 编码字，否则收件端显示成乱码
	b.WriteString("Subject: " + mime.BEncoding.Encode("UTF-8", subject) + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	b.WriteString("\r\n")

	enc := base64.StdEncoding.EncodeToString([]byte(body))
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	if enc != "" {
		b.WriteString(enc + "\r\n")
	}
	return []byte(b.String()), nil
}

// checkAddr 校验一个邮件地址可安全放进邮件头。
func checkAddr(addr string) error {
	if strings.ContainsAny(addr, "\r\n") {
		return errors.New("含换行符")
	}
	a, err := mail.ParseAddress(addr)
	if err != nil {
		return err
	}
	if a.Address != addr {
		return errors.New("必须是纯地址，不能带显示名")
	}
	return nil
}
