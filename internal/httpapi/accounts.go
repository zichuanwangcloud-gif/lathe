package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/Clouditera/lathe/internal/auth"
	"github.com/Clouditera/lathe/internal/store"
)

// Mailer 发送邮件。由 internal/mail 实现，main.go 注入。
//
// 定义在这里而不是直接 import internal/mail：与 Verifier 的处理一致，
// 保持 mail → httpapi 的单向依赖，否则两个包会成环。
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
	Ready(ctx context.Context) bool
}

// AccountAPI 提供注册、登录与密码相关的接口。
type AccountAPI struct {
	Users    *store.Users
	Sessions SessionStore
	Resets   *store.Resets
	Auth     *Auth
	Mail     Mailer

	// BaseURL 用于拼接重置链接，来自 config.PublicURL()。
	BaseURL string
	// TrustProxy 决定限流时是否采信 X-Forwarded-For。
	TrustProxy bool

	limiter     *limiter
	limiterOnce sync.Once
}

// Routes 注册账号相关接口。
//
// 登录、登出、me 由 Auth 直接提供（它持有会话），其余在此。
func (a *AccountAPI) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/register", a.register)
	mux.HandleFunc("POST /api/password/forgot", a.forgotPassword)
	mux.HandleFunc("POST /api/password/reset", a.resetPassword)
	mux.Handle("POST /api/password/change", a.Auth.RequireFunc(a.changePassword))
	mux.Handle("PUT /api/me/notify-email", a.Auth.RequireFunc(a.setNotifyEmail))
}

func (a *AccountAPI) lim() *limiter {
	a.limiterOnce.Do(func() { a.limiter = newLimiter() })
	return a.limiter
}

// register 开放注册。
//
// 不做邮箱验证：产品上选择「谁都能注册」。这意味着重复邮箱必然返回 409，
// 于是注册接口天然可以用来枚举已注册邮箱 —— 这是「开放注册」与
// 「不泄露账号是否存在」之间的固有矛盾，选了前者就得接受后者。
// 忘记密码接口仍然严格不泄露，别让这里的妥协扩散过去。
func (a *AccountAPI) register(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r, a.TrustProxy)
	if !a.lim().allow("reg:"+ip, 5, time.Hour) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "注册过于频繁，请稍后再试"})
		return
	}

	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}

	email := strings.TrimSpace(body.Email)
	if err := validEmail(email); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := auth.Policy(body.Password); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	hash, err := auth.Hash(body.Password)
	if err != nil {
		serverError(w, "生成密码哈希失败", err)
		return
	}

	u, err := a.Users.Create(r.Context(), email, hash, store.RoleMember)
	if err != nil {
		if errors.Is(err, store.ErrEmailTaken) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "该邮箱已注册"})
			return
		}
		serverError(w, "创建账号失败", err)
		return
	}
	slog.Info("新用户注册", "user", u.ID, "email", u.Email)

	// 注册完直接登录，省掉「注册成功请登录」这一步多余的往返
	a.Auth.issueSession(w, r, u)
}

// changePassword 修改自己的密码。
func (a *AccountAPI) changePassword(w http.ResponseWriter, r *http.Request) {
	u := CurrentUser(r)
	if u == nil || u.ID == 0 {
		// Bearer 通道的合成身份没有真实账号，改不了密码
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "当前身份不是账号登录（管理令牌通道无密码可改）",
		})
		return
	}

	var body struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}

	// 强制改密状态下也要验旧口令：那个初始口令就在启动日志里，不给旁路。
	if !auth.Verify(u.PasswordHash, body.CurrentPassword) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "当前密码不正确"})
		return
	}
	if err := auth.Policy(body.NewPassword); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	hash, err := auth.Hash(body.NewPassword)
	if err != nil {
		serverError(w, "生成密码哈希失败", err)
		return
	}
	if err := a.Users.SetPassword(r.Context(), u.ID, hash, false); err != nil {
		serverError(w, "更新密码失败", err)
		return
	}

	// 密码换了，在途的重置链接必须立刻作废 —— 否则一封旧邮件还能改回去
	_ = a.Resets.DeleteUser(r.Context(), u.ID)

	// 踢掉其它设备，但保住当前会话，免得用户改完密码就被自己踢下线
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = a.Sessions.DeleteUserExcept(r.Context(), u.ID, c.Value)
	} else {
		_ = a.Sessions.DeleteUser(r.Context(), u.ID)
	}

	slog.Info("用户修改了密码", "user", u.ID)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// forgotPassword 发起密码重置。
//
// 无论邮箱是否存在、是否被停用、SMTP 有没有配好，一律返回同一个 200。
// 任何差异（响应体、状态码、耗时）都能被用来枚举已注册邮箱，所以真正的
// 工作全部丢到后台 goroutine 里做，处理器本身立刻返回 —— 这样响应快慢
// 也不受「查库 + 发信」的影响。
// setNotifyEmail 设置个人通知邮箱；空串清除，回退用登录邮箱收信。
//
// 通知邮箱只服务于通知类邮件。密码重置邮件始终发往登录邮箱 ——
// 找回密码的前提是证明对账号邮箱的所有权，允许改道自填地址等于
// 把账号接管的后门递给任何能登录的人（比如短暂离开的 unlocked session）。
func (a *AccountAPI) setNotifyEmail(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	email := store.NormalizeEmail(body.Email)
	if email != "" {
		if err := validEmail(email); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	}

	u := CurrentUser(r)
	if err := a.Users.SetNotifyEmail(r.Context(), u.ID, email); err != nil {
		serverError(w, "保存通知邮箱失败", err)
		return
	}

	// 返回最新用户形状，前端直接替换 auth.user，不必再发一次 /api/me
	fresh, err := a.Users.ByID(r.Context(), u.ID)
	if err != nil {
		serverError(w, "读取用户信息失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": a.Auth.userJSON(r.Context(), fresh)})
}

func (a *AccountAPI) forgotPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	email := store.NormalizeEmail(body.Email)

	ip := clientIP(r, a.TrustProxy)
	// 限流是唯一会改变响应的因素 —— 它按 IP 和邮箱计数，不泄露账号是否存在
	if !a.lim().allow("fpip:"+ip, 10, time.Hour) || !a.lim().allow("fp:"+email, 3, time.Hour) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "请求过于频繁，请稍后再试"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})

	if email == "" {
		return
	}
	// 不能用 r.Context()：响应已经发出，它马上就会被取消
	go a.deliverReset(email)
}

// deliverReset 在后台查账号、签发令牌并发信。任何失败只记日志。
func (a *AccountAPI) deliverReset(email string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	u, err := a.Users.ByEmail(ctx, email)
	if err != nil {
		if !errors.Is(err, store.ErrUserNotFound) {
			slog.Warn("找回密码：查询账号失败", "err", err)
		}
		return
	}
	if u.Disabled() {
		slog.Info("找回密码：账号已停用，不发信", "user", u.ID)
		return
	}
	if a.Mail == nil || !a.Mail.Ready(ctx) {
		slog.Warn("找回密码：SMTP 未配置，无法发信。请在设置页配置发信通道", "email", email)
		return
	}

	token := randomHex(32)
	if err := a.Resets.Create(ctx, u.ID, token, store.ResetTokenTTL); err != nil {
		slog.Error("找回密码：签发重置令牌失败", "user", u.ID, "err", err)
		return
	}

	link := a.BaseURL + "/reset-password?token=" + token
	subject, text := resetMail(u.Email, link)
	if err := a.Mail.Send(ctx, u.Email, subject, text); err != nil {
		slog.Error("找回密码：发信失败", "user", u.ID, "err", err)
		return
	}
	slog.Info("已发出密码重置邮件", "user", u.ID)
}

// resetMail 渲染重置邮件的主题与正文。
//
// 单独成函数是为了能脱离 SMTP 测正文里确实带上了链接。
func resetMail(email, link string) (subject, body string) {
	subject = "【Lathe】重置你的登录密码"
	body = "你好，\n\n" +
		"有人为账号 " + email + " 请求了重置密码。点击下面的链接设置新密码：\n\n" +
		link + "\n\n" +
		"链接 30 分钟内有效，且只能使用一次。\n\n" +
		"如果这不是你本人的操作，忽略这封邮件即可 —— 你的密码不会有任何变化。\n\n" +
		"—— Lathe\n"
	return subject, body
}

// resetPassword 用重置令牌设置新密码。
func (a *AccountAPI) resetPassword(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r, a.TrustProxy)
	if !a.lim().allow("rp:"+ip, 10, time.Hour) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "尝试过于频繁，请稍后再试"})
		return
	}

	var body struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	if err := auth.Policy(body.Password); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	// 先算哈希再消费令牌：哈希失败时令牌不该被浪费掉
	hash, err := auth.Hash(body.Password)
	if err != nil {
		serverError(w, "生成密码哈希失败", err)
		return
	}

	userID, err := a.Resets.Consume(r.Context(), strings.TrimSpace(body.Token))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "重置链接无效或已过期，请重新发起找回密码"})
		return
	}

	if err := a.Users.SetPassword(r.Context(), userID, hash, false); err != nil {
		serverError(w, "更新密码失败", err)
		return
	}
	// 改完密码把该账号的其余令牌与全部会话一起清掉：
	// 走到这一步往往是「密码可能已泄露」，在线会话不该幸存
	_ = a.Resets.DeleteUser(r.Context(), userID)
	_ = a.Sessions.DeleteUser(r.Context(), userID)

	slog.Info("用户通过重置链接改了密码", "user", userID)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// validEmail 校验邮箱格式。
//
// 用 net/mail 而非正则：RFC 5322 的地址语法不是正则能覆盖的，
// 自己写一个只会既漏又误杀。
func validEmail(email string) error {
	if email == "" {
		return errors.New("邮箱不能为空")
	}
	if len(email) > 254 {
		return errors.New("邮箱过长")
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email {
		return errors.New("邮箱格式不正确")
	}
	// 换行符会被用来往邮件头里注入额外的头字段
	if strings.ContainsAny(email, "\r\n") {
		return errors.New("邮箱含非法字符")
	}
	return nil
}
