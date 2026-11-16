package vcs

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
)

// Target 是一个仓库所在的代码平台实例：(平台, 实例地址)。GitHub 的 BaseURL 为空。
//
// 它只来自 repos 行（登记时钉死），不来自凭据 —— 凭据地址事后被改，
// 派发时比对二者、不一致即拒绝（ErrInstanceMismatch）。
type Target struct {
	Provider Provider
	BaseURL  string
}

// RepoKey 是镜像/工作区的磁盘键，只吃 repos 行上的三个非密字段。
//
// GitHub 的键就是 providerRepo 本身 —— 与引入平台之前逐字节相同，现网 mirror
// 与在途 worktree 零迁移。GitLab/Gitea 的键带实例主机（含端口与路径前缀），
// 两个实例上的同名仓库在磁盘上不会撞到同一个 mirror。
func RepoKey(p Provider, baseURL, providerRepo string) string {
	if p == ProviderGitHub || p == "" {
		return providerRepo
	}
	host := baseURL
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
		host = u.Host + strings.TrimRight(u.Path, "/")
	}
	return host + "/" + providerRepo
}

// CloneURL 是 git 远端地址。GitHub 走宿主机 SSH（与引入平台之前逐字节相同）；
// GitLab/Gitea 用 token 只能走 HTTPS（loopback 联调为 http），见 runner 的 transport。
func CloneURL(p Provider, baseURL, providerRepo string) string {
	if p == ProviderGitHub || p == "" {
		return "git@github.com:" + providerRepo + ".git"
	}
	return strings.TrimRight(baseURL, "/") + "/" + providerRepo + ".git"
}

// 实例地址的长度约束，与迁移 0050/0051 的 CHECK 一致。
const (
	minBaseURLLen = 8
	maxBaseURLLen = 300
)

// NormalizeBaseURL 校验并规范化自建实例地址（PUT /api/integrations/{gitlab,gitea}）。
//
// 规则：url.Parse 成功；scheme 为 https（仅 loopback localhost/127.0.0.1/[::1]
// 允许 http，供本地联调）；无 userinfo、无 query/fragment；允许路径前缀
// （GitLab 可装在 https://host/gitlab）；去掉末尾 / 后长度 8..300。
func NormalizeBaseURL(raw string) (string, error) {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	if s == "" {
		return "", fmt.Errorf("实例地址不能为空，形如 https://gitlab.example.com")
	}
	if len(s) < minBaseURLLen || len(s) > maxBaseURLLen {
		return "", fmt.Errorf("实例地址长度须在 %d..%d 之间", minBaseURLLen, maxBaseURLLen)
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("实例地址无法解析: %v", err)
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("实例地址缺少主机名，形如 https://gitlab.example.com")
	}
	if u.User != nil {
		return "", fmt.Errorf("实例地址不能包含用户名或口令（令牌单独填写）")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(s, "?#") {
		return "", fmt.Errorf("实例地址不能包含查询串或片段")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopback(u.Hostname()) {
			return "", fmt.Errorf("实例地址必须使用 https（仅 localhost/127.0.0.1/[::1] 允许 http 联调）")
		}
	default:
		return "", fmt.Errorf("实例地址必须是 https:// 开头")
	}
	return s, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback))
}

// GitAuth 是一次带鉴权 git 调用所需的 HTTP Basic 凭据。
//
// 不透明：字段不导出，String/GoString/Format/LogValue/MarshalJSON 全部脱敏，
// 嵌在父结构里被 %v/%+v/%#v、slog 文本与 JSON handler、encoding/json 打印时
// 都不出明文。零值表示「无鉴权」——GitHub 走宿主 SSH，永远是零值，执行的
// git 序列与引入平台之前完全一致。
type GitAuth struct {
	user  string
	token string
}

// NewGitAuth 构造鉴权。token 为空时返回零值。
func NewGitAuth(user, token string) GitAuth {
	if token == "" {
		return GitAuth{}
	}
	if user == "" {
		user = "oauth2"
	}
	return GitAuth{user: user, token: token}
}

// IsZero 报告是否无鉴权。
func (a GitAuth) IsZero() bool { return a.token == "" }

// Credential 返回 Basic 鉴权的用户名与口令。
//
// 唯一的合法消费方是 runner 的 transport：它把口令写进只读一次的管道 fd，
// 交给 git 的 credential helper。不要把返回值放进任何环境变量、argv、日志。
func (a GitAuth) Credential() (user, password string) { return a.user, a.token }

// RedactValues 返回需要从输出里精确替换掉的值（脱敏权威层用）。零值返回 nil。
func (a GitAuth) RedactValues() []string {
	if a.token == "" {
		return nil
	}
	return []string{a.token}
}

const redacted = "[REDACTED]"

func (a GitAuth) String() string   { return redacted }
func (a GitAuth) GoString() string { return "vcs.GitAuth{" + redacted + "}" }

// Format 覆盖全部动词（%v %+v %#v %s %q …），防止 fmt 绕过 String 反射字段。
func (a GitAuth) Format(f fmt.State, verb rune) {
	if verb == 'v' && f.Flag('#') {
		_, _ = f.Write([]byte(a.GoString()))
		return
	}
	_, _ = f.Write([]byte(redacted))
}

// LogValue 让 slog 打印脱敏值。
func (a GitAuth) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON 输出空对象。
func (a GitAuth) MarshalJSON() ([]byte, error) { return json.Marshal(struct{}{}) }
