package yunxiao

import (
	"crypto/hmac"
	"encoding/json"
	"errors"
	"strings"
)

// HeaderSignature 是云效自动化规则 Webhook 配了 Secret 后携带的请求头。
// 它的值就是 Secret 原文（官方机制，**不是 HMAC**），不保护载荷完整性，
// 所以载荷除 id 外一字不信（§3.7）。
const HeaderSignature = "X-Projex-Signature"

// maxWorkitemIDLen 是工作项 id 的长度上限。
const maxWorkitemIDLen = 64

var (
	// ErrBadSignature 表示 webhook 密钥不匹配（含缺头、未配置密钥）。
	ErrBadSignature = errors.New("yunxiao: webhook 签名校验失败")
	// ErrBadPayload 表示载荷里没有合法的工作项 id。
	ErrBadPayload = errors.New("yunxiao: webhook 载荷缺少合法的工作项 id")
)

// VerifySecret 常数时间比较共享密钥。空密钥或缺头一律失败。
func VerifySecret(secret, got string) error {
	if secret == "" || got == "" {
		return ErrBadSignature
	}
	if !hmac.Equal([]byte(secret), []byte(got)) {
		return ErrBadSignature
	}
	return nil
}

// ValidWorkitemID 报告 id 是否是可安全拼进路径的工作项 id：
// 非空、≤64、字符集 [A-Za-z0-9_-]（因此天然不含 / 与 ..）。
func ValidWorkitemID(id string) bool {
	if id == "" || len(id) > maxWorkitemIDLen {
		return false
	}
	for _, r := range id {
		if !isLower(r) && !isUpper(r) && !isDigit(r) && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

// ParseWorkitemID 从 webhook 载荷（Body 选【工作项数据】，即 GetWorkitem
// 同形对象）里只取 id。
func ParseWorkitemID(body []byte) (string, error) {
	var p struct {
		ID flexString `json:"id"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return "", ErrBadPayload
	}
	id := strings.TrimSpace(string(p.ID))
	if !ValidWorkitemID(id) {
		return "", ErrBadPayload
	}
	return id, nil
}

// HasLabel 报告 labels 里是否有名为 name 的标签：按名字、trim 后大小写
// 不敏感；name trim 后为空恒不匹配（与 Linear 的标签接单口径一致）。
func HasLabel(labels []Label, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l.Name), name) {
			return true
		}
	}
	return false
}

// MatchStatus 报告状态 s 是否就是用户配置的状态名 name：按 displayName /
// name / nameEn 任一，trim 后大小写不敏感；name 为空恒不匹配。
func MatchStatus(s Status, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, c := range []string{s.DisplayName, s.Name, s.NameEn} {
		if c != "" && strings.EqualFold(strings.TrimSpace(c), name) {
			return true
		}
	}
	return false
}
