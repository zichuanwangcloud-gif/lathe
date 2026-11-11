package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// AllowedHosts 是生产的 webhook host 白名单（按 url.URL.Host 精确比较，含端口）。
//
// **不是环境变量**：放开它等于给用户一个「让服务器替我 POST 内网地址」的
// SSRF 原语。扩展它就是改产品行为，要在 PR 里说明理由。
var AllowedHosts = []string{"hooks.slack.com", "open.feishu.cn", "open.larksuite.com"}

// ValidateURL 的固定错误文案：**不回显入参**（URL 本身就是凭据）。
var (
	ErrURLUnparsable     = errors.New("url rejected: unparsable")
	ErrURLNotHTTPS       = errors.New("url rejected: not https")
	ErrURLHostNotAllowed = errors.New("url rejected: host not allowed")
)

// ValidateURL 是唯一的 URL 校验函数：Sender.Send 与 store.SaveChannel 都调它。
// 必须 https、host 命中白名单、不带 userinfo。
func ValidateURL(raw string, allowHosts []string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ErrURLUnparsable
	}
	if u.Scheme != "https" {
		return ErrURLNotHTTPS
	}
	if u.User != nil || u.Host == "" {
		return ErrURLHostNotAllowed
	}
	for _, h := range allowHosts {
		if u.Host == h {
			return nil
		}
	}
	return ErrURLHostNotAllowed
}

// MaskURL 给界面看的掩码：只回 scheme://host/…，路径的任何字符都不带
// （Slack/飞书 webhook 的末尾正是 token，不复用保留尾 4 位的 secret.Mask）。
func MaskURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "…"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// Message 是一条待发送的消息。
type Message struct {
	Title string
	Text  string
}

// Result 是一次发送的结论。Reason 是 last_error / test_error 的**唯一**来源，
// 生成规则见 resultFrom*：绝不出现 err.Error()（*url.Error 自带完整 URL）。
type Result struct {
	OK, Retryable bool
	Reason        string
}

// Sender 发送 webhook。纯 HTTP，无状态。
type Sender struct {
	allow  []string
	client *http.Client
	now    func() time.Time // 飞书签名的时间戳；测试可替换
}

// HardenClient 把任意 *http.Client 加固成生产口径：Timeout=SendTimeout、
// 不跟随重定向（302 到内网就是 SSRF）。原地修改并返回。
func HardenClient(c *http.Client) *http.Client {
	c.Timeout = SendTimeout
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

// NewSender 用注入的白名单与 client 构造发送器。client 为 nil 时 panic（装配错误要早暴露）。
func NewSender(allowHosts []string, client *http.Client) *Sender {
	if client == nil {
		panic("notify: NewSender 需要非 nil 的 http.Client")
	}
	return &Sender{allow: append([]string(nil), allowHosts...), client: client, now: time.Now}
}

// NewProductionSender 是 cmd/lathe 唯一的构造入口。
func NewProductionSender() *Sender {
	return NewSender(AllowedHosts, HardenClient(&http.Client{}))
}

// maxRespBytes 是读响应体的上限：响应体不落库，只取前 200 rune 作原因。
const maxRespBytes = 4 << 10

// maxReasonRunesInError 是错误串里平台原文的上界。
const maxReasonRunesInError = 200

// Send 发送一条消息并判定成败。不重试 —— 重试是 outbox 的事。
func (s *Sender) Send(ctx context.Context, ch Channel, msg Message) Result {
	res := s.send(ctx, ch, msg)
	res.Reason = scrubURL(res.Reason, ch.URL)
	return res
}

func (s *Sender) send(ctx context.Context, ch Channel, msg Message) Result {
	if err := ValidateURL(ch.URL, s.allow); err != nil {
		return Result{Reason: err.Error()}
	}
	body, err := s.payload(ch, msg)
	if err != nil {
		return Result{Reason: "unknown channel kind"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSpace(ch.URL), bytes.NewReader(body))
	if err != nil {
		return Result{Reason: ErrURLUnparsable.Error()}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return Result{Retryable: true, Reason: transportErrClass(err)}
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if rerr != nil {
		return Result{
			Retryable: retryableStatus(resp.StatusCode),
			Reason:    strconv.Itoa(resp.StatusCode) + " read body: " + transportErrClass(rerr),
		}
	}
	if ch.Kind == KindFeishu {
		return classifyFeishu(resp.StatusCode, raw)
	}
	return classifySlack(resp.StatusCode, raw)
}

func (s *Sender) payload(ch Channel, msg Message) ([]byte, error) {
	text := msg.Title
	if msg.Text != "" {
		text += "\n" + msg.Text
	}
	switch ch.Kind {
	case KindSlack:
		return json.Marshal(map[string]string{"text": text})
	case KindFeishu:
		m := map[string]any{"msg_type": "text", "content": map[string]string{"text": text}}
		if ch.SignSecret != "" {
			ts := strconv.FormatInt(s.now().Unix(), 10)
			m["timestamp"] = ts
			m["sign"] = feishuSign(ts, ch.SignSecret)
		}
		return json.Marshal(m)
	}
	return nil, fmt.Errorf("notify: 未知渠道种类")
}

// feishuSign 按飞书开放平台《自定义机器人使用指南》的签名校验算法：
// stringToSign = timestamp + "\n" + secret 作 HmacSHA256 的 **key**，消息为空，结果 Base64。
func feishuSign(timestamp, secret string) string {
	h := hmac.New(sha256.New, []byte(timestamp+"\n"+secret))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func retryableStatus(code int) bool { return code == http.StatusTooManyRequests || code >= 500 }

// classifySlack：Slack incoming webhook **不回 JSON** —— 成功是 200 + 纯文本 ok，
// 失败是 4xx + 纯文本错误串。所以只看状态码与 TrimSpace(body)=="ok"，绝不 Unmarshal。
func classifySlack(code int, raw []byte) Result {
	body := strings.TrimSpace(string(raw))
	switch {
	case code == http.StatusOK && body == "ok":
		return Result{OK: true}
	case retryableStatus(code):
		return Result{Retryable: true, Reason: statusReason(code, body)}
	default:
		return Result{Reason: statusReason(code, body)}
	}
}

// feishuResp 兼容新旧两种形态：以 code 为准，缺失时才看旧的 StatusCode。
type feishuResp struct {
	Code          *int   `json:"code"`
	Msg           string `json:"msg"`
	StatusCode    *int   `json:"StatusCode"`
	StatusMessage string `json:"StatusMessage"`
}

// feishuRateLimited 是飞书系统压力触发的限流码（官方建议避开整点重发）。
const feishuRateLimited = 11232

func classifyFeishu(code int, raw []byte) Result {
	body := strings.TrimSpace(string(raw))
	if code != http.StatusOK {
		return Result{Retryable: retryableStatus(code), Reason: statusReason(code, body)}
	}
	var fr feishuResp
	if err := json.Unmarshal(raw, &fr); err != nil {
		return Result{Reason: statusReason(code, body)}
	}
	c, msg := fr.Code, fr.Msg
	if c == nil {
		c, msg = fr.StatusCode, fr.StatusMessage
	}
	if c == nil {
		return Result{Reason: statusReason(code, body)}
	}
	if *c == 0 {
		return Result{OK: true}
	}
	reason := fmt.Sprintf("%d code=%d %s", code, *c, clipRunes(msg, maxReasonRunesInError))
	// 9499（请求体错/超 20 KB）、19021（签名错）、19022（IP 白名单）、19024（关键词）
	// 与未知码都不可重试：重发同一个请求不会变好，宁可让人看到 gave_up + 原因。
	return Result{Retryable: *c == feishuRateLimited, Reason: strings.TrimSpace(reason)}
}

func statusReason(code int, body string) string {
	return strings.TrimSpace(strconv.Itoa(code) + " " + clipRunes(body, maxReasonRunesInError))
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// transportErrClass 把无 HTTP 响应的错误压成**固定类别串**。
//
// 绝不返回 err.Error()：*url.Error 自带完整 URL（Slack token 在路径里），
// 内层 *net.OpError 还带 dial tcp <host:port>。日志与落库共用这一个函数。
func transportErrClass(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns error"
	}
	var (
		certErr   *tls.CertificateVerificationError
		recErr    tls.RecordHeaderError
		unknownCA x509.UnknownAuthorityError
		hostErr   x509.HostnameError
		invalid   x509.CertificateInvalidError
	)
	if errors.As(err, &certErr) || errors.As(err, &recErr) || errors.As(err, &unknownCA) ||
		errors.As(err, &hostErr) || errors.As(err, &invalid) {
		return "tls error"
	}
	return "network error"
}

// redactedMark 替换错误串里出现的 URL 片段。
const redactedMark = "«redacted»"

// scrubURL 是最后一道纵深防御：把原始 URL、host、path 的任何出现替换掉。
// path 为空或为 "/" 时跳过，免得把所有 "/" 都换掉。
func scrubURL(reason, raw string) string {
	raw = strings.TrimSpace(raw)
	if reason == "" || raw == "" {
		return reason
	}
	reason = strings.ReplaceAll(reason, raw, redactedMark)
	if u, err := url.Parse(raw); err == nil {
		if u.Host != "" {
			reason = strings.ReplaceAll(reason, u.Host, redactedMark)
		}
		if u.Path != "" && u.Path != "/" {
			reason = strings.ReplaceAll(reason, u.Path, redactedMark)
		}
	}
	return reason
}
