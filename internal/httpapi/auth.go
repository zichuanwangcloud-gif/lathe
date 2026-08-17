package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Clouditera/lathe/internal/auth"
	"github.com/Clouditera/lathe/internal/store"
)

// sessionCookie 是登录态 Cookie 名。
const sessionCookie = "lathe_session"

// sessionTTL 是会话有效期。
const sessionTTL = 12 * time.Hour

// SessionStore 是会话的持久化后端。
//
// 抽成接口只为一件事：没接数据库时（纯单元测试）用内存实现兜底，
// 让 Auth 自身只有一条代码路径。*store.Sessions 天然满足这个接口。
type SessionStore interface {
	Create(ctx context.Context, userID int64, token string, ttl time.Duration) error
	Lookup(ctx context.Context, token string) (*store.User, error)
	Delete(ctx context.Context, token string) error
	DeleteUser(ctx context.Context, userID int64) error
	DeleteUserExcept(ctx context.Context, userID int64, keepToken string) error
}

// Auth 认证用户并保护接口。
//
// 两条认证通道并存：
//   - 会话 Cookie —— 人在浏览器里的正常登录态。
//   - Authorization: Bearer <LATHE_ADMIN_TOKEN> —— 给脚本和 curl 用，
//     同时是「SMTP 挂了且管理员把自己锁在门外」时的应急入口。
//
// 两者都为空（没配令牌又没接库）时一律拒绝，而不是默认放行：
// 把「忘了配」的后果做成不可用而非不设防。
type Auth struct {
	// Token 是遗留的 LATHE_ADMIN_TOKEN。为空表示这条通道关闭。
	Token string

	// Users 为 nil 表示未接数据库，此时只有 Token 通道可用。
	Users *store.Users

	// Sessions 默认是内存实现，WithStore 之后换成 sessions 表。
	Sessions SessionStore

	// Bootstrap 是 Token 通道映射到的身份。为 nil 时合成一个内存管理员。
	Bootstrap *store.User

	// SecureCookie 决定会话 Cookie 是否带 Secure 标志。
	SecureCookie bool

	// TrustProxy 决定限流时是否采信 X-Forwarded-For。
	TrustProxy bool

	// HasLinearToken 报告某用户是否已绑定 Linear API 令牌，
	// 前端据此决定要不要显示「Linear 任务」菜单。
	// 凭据通路与执行队列同一条（含管理员的 env 兜底），由 main 注入；
	// 为 nil 时一律按未绑定处理。
	HasLinearToken func(ctx context.Context, userID int64) bool

	logins     *limiter
	loginsOnce sync.Once
}

// loginLimiter 惰性构造登录限流器。
func (a *Auth) loginLimiter() *limiter {
	a.loginsOnce.Do(func() { a.logins = newLimiter() })
	return a.logins
}

// NewAuth 构造鉴权组件。
//
// 签名与 P0 保持一致：一批不需要数据库的单元测试直接用它，
// 为了接入用户体系而把那些测试改造成依赖 Postgres 是净损失。
func NewAuth(token string) *Auth {
	return &Auth{Token: token, Sessions: newMemorySessions()}
}

// WithStore 接上数据库，把会话与用户身份换成持久化实现。
func (a *Auth) WithStore(users *store.Users, sessions SessionStore, bootstrap *store.User, secure bool) *Auth {
	a.Users = users
	a.Sessions = sessions
	a.Bootstrap = bootstrap
	a.SecureCookie = secure
	return a
}

// Enabled 报告管理界面是否可用。
//
// 接了用户库就算启用 —— 有了账号体系之后，不配 LATHE_ADMIN_TOKEN
// 也应该能正常注册登录。
func (a *Auth) Enabled() bool { return a.Token != "" || a.Users != nil }

// ---------------------------------------------------------------- 请求上下文

type ctxKey struct{}

var userCtxKey ctxKey

// CurrentUser 返回本次请求的登录用户，未登录时为 nil。
//
// 只有经 Require / RequireAdmin 包装过的处理器能拿到非 nil 值。
// 这是第二步做 owner_id 数据隔离时的关键接缝：那时各处理器把
// CurrentUser(r).ID 传进 store 层即可，不必再设计一套传递机制。
func CurrentUser(r *http.Request) *store.User {
	u, _ := r.Context().Value(userCtxKey).(*store.User)
	return u
}

// actorOf 生成 task_events.actor 的取值。
//
// 格式见 0001_init.up.sql：system | user:<id> | node:<name>。
// 多用户之后审计轨迹必须记清是谁操作的，不能再写死 user:admin。
func actorOf(r *http.Request) string {
	if u := CurrentUser(r); u != nil {
		return "user:" + strconv.FormatInt(u.ID, 10)
	}
	return "system"
}

// ---------------------------------------------------------------- 登录

// Login 校验身份并签发会话 Cookie。
//
// 接受两种请求体：{"email","password"} 是正常登录；{"token"} 是遗留的
// 管理令牌通道，前端已不再使用，但保留作为应急入口。
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	if !a.Enabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "鉴权未配置，管理界面不可用",
		})
		return
	}

	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Token    string `json:"token"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}

	if body.Token != "" {
		a.loginWithToken(w, r, body.Token)
		return
	}
	a.loginWithPassword(w, r, body.Email, body.Password)
}

// loginWithToken 走遗留的管理令牌通道。
func (a *Auth) loginWithToken(w http.ResponseWriter, r *http.Request, token string) {
	if a.Token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(a.Token)) != 1 {
		slog.Warn("管理令牌登录失败", "remote", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "令牌不正确"})
		return
	}
	a.issueSession(w, r, a.bootstrapUser())
}

// loginWithPassword 走邮箱口令通道。
//
// 「邮箱不存在」与「口令错误」返回完全一致的响应：任何差异都能被用来
// 枚举已注册邮箱。同理，邮箱不存在时也照样跑一次哈希校验，让两条路径
// 的耗时相当 —— 否则响应快慢本身就是一个预言机。
func (a *Auth) loginWithPassword(w http.ResponseWriter, r *http.Request, email, password string) {
	const badCreds = "邮箱或密码不正确"

	if a.Users == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "账号体系不可用（数据库未就绪）",
		})
		return
	}
	email = store.NormalizeEmail(email)
	if email == "" || password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "邮箱与密码都不能为空"})
		return
	}

	// 两道限流：按「IP + 邮箱」挡住针对某个账号的暴力试口令，
	// 按 IP 挡住拿一个口令去撞一批邮箱（credential stuffing）。
	ip := clientIP(r, a.TrustProxy)
	lim := a.loginLimiter()
	if !lim.allow("login:"+ip+"|"+email, 5, 5*time.Minute) || !lim.allow("loginip:"+ip, 30, 5*time.Minute) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "尝试过于频繁，请稍后再试"})
		return
	}

	u, err := a.Users.ByEmail(r.Context(), email)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			auth.Verify("", password) // 恒定耗时用的空跑
			slog.Warn("登录失败：邮箱不存在", "remote", r.RemoteAddr)
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": badCreds})
			return
		}
		serverError(w, "查询账号失败", err)
		return
	}

	if !auth.Verify(u.PasswordHash, password) {
		slog.Warn("登录失败：口令不匹配", "user", u.ID, "remote", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": badCreds})
		return
	}

	// 停用判定放在口令校验之后：先校验再判状态，避免把「这个邮箱确实注册过」
	// 的信息透给一个连口令都不知道的人。
	if u.Disabled() {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "账号已停用，请联系管理员"})
		return
	}

	_ = a.Users.TouchLogin(r.Context(), u.ID)
	a.issueSession(w, r, u)
}

// issueSession 生成会话、种 Cookie 并返回当前用户。
func (a *Auth) issueSession(w http.ResponseWriter, r *http.Request, u *store.User) {
	token := randomHex(32)
	if err := a.Sessions.Create(r.Context(), u.ID, token, sessionTTL); err != nil {
		serverError(w, "创建会话失败", err)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.SecureCookie,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL / time.Second),
		Expires:  time.Now().Add(sessionTTL),
	})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "user": a.userJSON(r.Context(), u)})
}

// Logout 注销当前会话。
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = a.Sessions.Delete(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
		Secure: a.SecureCookie, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// Me 返回当前登录状态，供前端决定渲染登录页还是应用界面。
//
// authenticated / authEnabled 两个键的名字与语义刻意保持不变，
// 新增的用户信息挂在 user 下 —— 前端与既有测试都不必因此改动。
func (a *Auth) Me(w http.ResponseWriter, r *http.Request) {
	u := a.resolve(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": u != nil,
		"authEnabled":   a.Enabled(),
		"user":          a.userJSON(r.Context(), u),
	})
}

// userJSON 是账号在接口上的形状。nil 进 nil 出，方便直接塞进响应。
//
// 是 *Auth 的方法而非自由函数：hasLinearToken 要走注入的凭据通路，
// 登录响应与 /api/me 都得带上它，否则菜单显隐在登录当刻是错的。
func (a *Auth) userJSON(ctx context.Context, u *store.User) map[string]any {
	if u == nil {
		return nil
	}
	hasLinear := false
	if a.HasLinearToken != nil {
		hasLinear = a.HasLinearToken(ctx, u.ID)
	}
	return map[string]any{
		"id":                 u.ID,
		"email":              u.Email,
		"role":               u.Role,
		"disabled":           u.Disabled(),
		"mustChangePassword": u.MustChangePassword,
		"createdAt":          u.CreatedAt,
		"lastLoginAt":        u.LastLoginAt,
		// 专属 webhook 回调路径段（P1.5 第二步）：设置页拼成完整地址
		// 让用户配进 Linear。它本身不是密钥 —— 真正防伪的是按用户
		// 各自校验的签名密钥。
		"webhookSlug": u.WebhookSlug,
		// 个人通知邮箱；未设置为 null，前端提示「回退用登录邮箱」。
		"notifyEmail": u.NotifyEmail,
		// 是否已绑定 Linear API 令牌 —— 「Linear 任务」菜单的显隐依据。
		"hasLinearToken": hasLinear,
	}
}

// ---------------------------------------------------------------- 中间件

// changePasswordAllowlist 是强制改密状态下仍可访问的端点，
// 少到只够完成「改密」这一件事本身。
var changePasswordAllowlist = map[string]bool{
	"/api/password/change": true,
	"/api/logout":          true,
	"/api/me":              true,
}

// Require 包装需要登录的处理器。
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.Enabled() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "鉴权未配置，写接口已禁用",
			})
			return
		}
		u := a.resolve(r)
		if u == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
			return
		}
		// 初始口令未改前，除改密本身外一律挡住。用 409 而非 403：
		// 请求本身合法，是账号当前状态不允许 —— 与 transitionError 的取舍一致。
		if u.MustChangePassword && !changePasswordAllowlist[r.URL.Path] {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "请先修改初始密码", "mustChangePassword": true,
			})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userCtxKey, u)))
	})
}

// RequireFunc 是 Require 的 HandlerFunc 版本。
func (a *Auth) RequireFunc(next http.HandlerFunc) http.Handler {
	return a.Require(next)
}

// RequireAdmin 在 Require 之上再要求管理员角色。
//
// 复用 Require 而不是复制它：未登录仍是 401、越权才是 403，两层次序正确。
func (a *Auth) RequireAdmin(next http.Handler) http.Handler {
	return a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := CurrentUser(r); u == nil || !u.IsAdmin() {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "需要管理员权限"})
			return
		}
		next.ServeHTTP(w, r)
	}))
}

// RequireAdminFunc 是 RequireAdmin 的 HandlerFunc 版本。
func (a *Auth) RequireAdminFunc(next http.HandlerFunc) http.Handler {
	return a.RequireAdmin(next)
}

// resolve 解析当前请求的身份：Bearer 令牌优先，其次会话 Cookie。
func (a *Auth) resolve(r *http.Request) *store.User {
	if !a.Enabled() {
		return nil
	}

	if h := r.Header.Get("Authorization"); h != "" && a.Token != "" {
		token, ok := strings.CutPrefix(h, "Bearer ")
		if ok && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(a.Token)) == 1 {
			return a.bootstrapUser()
		}
	}

	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	u, err := a.Sessions.Lookup(r.Context(), c.Value)
	if err != nil {
		return nil
	}
	return u
}

// bootstrapUser 返回管理令牌通道对应的身份。
//
// 刻意清掉 MustChangePassword：这是给脚本和应急场景用的通道，
// 对一个 curl 调用强制「先去改密码」没有意义，反而会在管理员被锁在
// 门外时把最后一条路也堵死。
func (a *Auth) bootstrapUser() *store.User {
	if a.Bootstrap != nil {
		clone := *a.Bootstrap
		clone.MustChangePassword = false
		return &clone
	}
	// 未接数据库（纯单元测试）时合成一个身份，让下游代码只有一条路径
	return &store.User{ID: 0, Email: "bootstrap@lathe.local", Role: store.RoleAdmin}
}

// authenticated 报告请求是否已认证。
func (a *Auth) authenticated(r *http.Request) bool { return a.resolve(r) != nil }

// ---------------------------------------------------------------- 内存会话

// memorySessions 是没接数据库时的会话后端。
//
// 只在纯单元测试里用到。它不区分用户 —— 没有数据库就只有管理令牌
// 这一条通道，通道背后永远是同一个 bootstrap 身份。
type memorySessions struct {
	mu       sync.RWMutex
	sessions map[string]memSession
}

type memSession struct {
	userID  int64
	expires time.Time
}

func newMemorySessions() *memorySessions {
	return &memorySessions{sessions: map[string]memSession{}}
}

func (m *memorySessions) Create(_ context.Context, userID int64, token string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[token] = memSession{userID: userID, expires: time.Now().Add(ttl)}
	now := time.Now()
	for t, s := range m.sessions {
		if now.After(s.expires) {
			delete(m.sessions, t)
		}
	}
	return nil
}

func (m *memorySessions) Lookup(_ context.Context, token string) (*store.User, error) {
	m.mu.RLock()
	s, ok := m.sessions[token]
	m.mu.RUnlock()
	if !ok || time.Now().After(s.expires) {
		return nil, store.ErrSessionInvalid
	}
	return &store.User{ID: s.userID, Email: "bootstrap@lathe.local", Role: store.RoleAdmin}, nil
}

func (m *memorySessions) Delete(_ context.Context, token string) error {
	m.mu.Lock()
	delete(m.sessions, token)
	m.mu.Unlock()
	return nil
}

func (m *memorySessions) DeleteUser(_ context.Context, userID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for t, s := range m.sessions {
		if s.userID == userID {
			delete(m.sessions, t)
		}
	}
	return nil
}

func (m *memorySessions) DeleteUserExcept(_ context.Context, userID int64, keep string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for t, s := range m.sessions {
		if s.userID == userID && t != keep {
			delete(m.sessions, t)
		}
	}
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败属系统级异常，此处无从降级
		panic("httpapi: 生成随机数失败: " + err.Error())
	}
	return hex.EncodeToString(b)
}
