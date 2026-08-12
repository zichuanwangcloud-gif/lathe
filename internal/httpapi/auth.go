package httpapi

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// sessionCookie 是登录态 Cookie 名。
const sessionCookie = "lathe_session"

// Auth 保护写接口与管理界面。
//
// P0 单用户形态：用一个管理令牌换取会话 Cookie。多用户的 OAuth
// 留到 P2（见 docs/02-design.md §8）。
type Auth struct {
	// Token 是管理令牌。为空表示未配置 —— 此时所有受保护路由一律拒绝，
	// 而不是默认放行：把「忘了配」的后果做成不可用而非不设防。
	Token string

	mu       sync.RWMutex
	sessions map[string]time.Time
}

// NewAuth 构造鉴权组件。
func NewAuth(token string) *Auth {
	return &Auth{Token: token, sessions: map[string]time.Time{}}
}

// Enabled 报告是否已配置管理令牌。
func (a *Auth) Enabled() bool { return a.Token != "" }

// sessionTTL 是会话有效期。
const sessionTTL = 12 * time.Hour

// Login 校验令牌并签发会话 Cookie。
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	if !a.Enabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "未配置 LATHE_ADMIN_TOKEN，管理界面不可用",
		})
		return
	}

	var body struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}

	// 常数时间比较，避免通过响应耗时逐字节猜令牌
	if subtle.ConstantTimeCompare([]byte(body.Token), []byte(a.Token)) != 1 {
		slog.Warn("管理登录失败", "remote", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "令牌不正确"})
		return
	}

	sid := randomHex(32)
	a.mu.Lock()
	a.sessions[sid] = time.Now().Add(sessionTTL)
	a.gcLocked()
	a.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
	})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// Logout 注销当前会话。
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// Me 返回当前登录状态，供前端决定是否显示登录页。
func (a *Auth) Me(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": a.authenticated(r),
		"authEnabled":   a.Enabled(),
	})
}

// Require 包装受保护的处理器。
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.Enabled() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "未配置 LATHE_ADMIN_TOKEN，写接口已禁用",
			})
			return
		}
		if !a.authenticated(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireFunc 是 Require 的 HandlerFunc 版本。
func (a *Auth) RequireFunc(next http.HandlerFunc) http.Handler {
	return a.Require(next)
}

// authenticated 判断请求是否已认证：接受会话 Cookie 或 Bearer 令牌。
//
// 同时支持 Bearer 是为了让 curl / 脚本能直接调用，不必先登录。
func (a *Auth) authenticated(r *http.Request) bool {
	if !a.Enabled() {
		return false
	}

	if h := r.Header.Get("Authorization"); h != "" {
		token, ok := strings.CutPrefix(h, "Bearer ")
		if ok && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(a.Token)) == 1 {
			return true
		}
	}

	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	a.mu.RLock()
	exp, ok := a.sessions[c.Value]
	a.mu.RUnlock()
	return ok && time.Now().Before(exp)
}

// gcLocked 清理过期会话，调用方须持有写锁。
func (a *Auth) gcLocked() {
	now := time.Now()
	for sid, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, sid)
		}
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败属系统级异常，此处无从降级
		panic("httpapi: 生成随机数失败: " + err.Error())
	}
	return hex.EncodeToString(b)
}
