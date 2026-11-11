// Package apiclient 是 GitLab/Gitea 客户端共用的最小 JSON-over-HTTP 封装。
//
// 只用标准库（AGENTS §4：不引入新依赖）。令牌只出现在请求头里，错误文案
// 不回显请求头，也不回显响应体里可能出现的令牌（按值精确替换）。
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client 是带固定鉴权头的 JSON 客户端。
type Client struct {
	Base       string // 实例地址（无尾斜杠）
	Header     string // 鉴权头名
	HeaderVal  string // 鉴权头值（含令牌）
	Secret     string // 令牌本身，用于从错误文案里替换掉
	HTTP       *http.Client
	ServiceTag string // 错误前缀，如 "gitlab"
}

// New 构造客户端；超时与 github 包一致（30s）。
func New(tag, base, header, headerVal, secret string) *Client {
	return &Client{
		Base: strings.TrimRight(base, "/"), Header: header, HeaderVal: headerVal, Secret: secret,
		HTTP: &http.Client{Timeout: 30 * time.Second}, ServiceTag: tag,
	}
}

// StatusError 是非 2xx 响应。
type StatusError struct {
	Tag    string
	Method string
	Path   string
	Code   int
	Body   string // 截断、脱敏后的响应体
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: %s %s 返回 %d：%s", e.Tag, e.Method, e.Path, e.Code, e.Body)
}

// Response 是一次调用的结果（成功时）。
type Response struct {
	Code   int
	Header http.Header
}

// Do 发请求；in 非 nil 时编码为 JSON 请求体，out 非 nil 时解码 2xx 响应体。
// path 以 / 开头，已按需 URL 编码（例如 GitLab 的 projects/<url-encoded path>）。
func (c *Client) Do(ctx context.Context, method, path string, in, out any) (*Response, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return nil, fmt.Errorf("%s: 构造请求失败: %w", c.ServiceTag, err)
	}
	req.Header.Set(c.Header, c.HeaderVal)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %s %s 失败: %s", c.ServiceTag, method, path, c.scrub(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return &Response{Code: resp.StatusCode, Header: resp.Header},
			&StatusError{Tag: c.ServiceTag, Method: method, Path: path, Code: resp.StatusCode, Body: c.scrub(msg)}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, fmt.Errorf("%s: 解析 %s %s 的响应失败: %w", c.ServiceTag, method, path, err)
		}
	}
	return &Response{Code: resp.StatusCode, Header: resp.Header}, nil
}

func (c *Client) scrub(s string) string {
	if c.Secret != "" {
		s = strings.ReplaceAll(s, c.Secret, "[REDACTED]")
	}
	return s
}

// Code 取 err 里的 HTTP 状态码（不是 StatusError 时为 0）。
func Code(err error) int {
	if se, ok := err.(*StatusError); ok {
		return se.Code
	}
	return 0
}

// Classify 把连通性错误翻译成人读文案（保存凭据时的验证用）。
func Classify(platform string, err error, howToFix string) error {
	switch Code(err) {
	case http.StatusUnauthorized:
		return fmt.Errorf("令牌无效或已过期，请在 %s 重新签发", howToFix)
	case http.StatusForbidden:
		return fmt.Errorf("令牌被拒（权限不足或被实例策略限制）：%w", err)
	case 0:
		msg := err.Error()
		if strings.Contains(msg, "no such host") || strings.Contains(msg, "connection refused") ||
			strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") ||
			strings.Contains(msg, "certificate") {
			return fmt.Errorf("连不上 %s 实例（地址、网络或证书问题）：%w", platform, err)
		}
	}
	return err
}
