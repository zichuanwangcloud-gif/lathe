package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/auth"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// MaxPATsPerUser 是每用户未吊销且未过期的个人 API 令牌上限。
//
// 常量而非配置：它防误操作、限泄漏面，调大只会扩大后者（PRD §3.8）。
const MaxPATsPerUser = 20

// MaxPATTTL 是个人 API 令牌有效期上限。「永不过期」另有显式写法。
const MaxPATTTL = 365 * 24 * time.Hour

// maxPATNameBytes 与迁移里的 CHECK (octet_length(name) BETWEEN 1 AND 64) 同口径。
// handler 先判，CHECK 只是兜底 —— 靠数据库报错会变成 500。
const maxPATNameBytes = 64

// 签发接口的口令尝试限流：每用户 15 分钟 10 次（成功失败都计）。
// 调用点常量，不做成字段或配置（PRD §3.5）。
const (
	patCreateLimit  = 10
	patCreateWindow = 15 * time.Minute
)

// PATRevoker 吊销某用户全部个人 API 令牌。停用账号与三条改口令路径共用。
type PATRevoker interface {
	RevokeUser(ctx context.Context, userID int64) (int64, error)
}

// PATStore 是令牌管理接口所需的持久化面。*store.PersonalAccessTokens 天然满足。
type PATStore interface {
	Create(ctx context.Context, userID int64, name string, expiresAt *time.Time) (string, int64, error)
	List(ctx context.Context, userID int64) ([]store.PAT, error)
	CountActive(ctx context.Context, userID int64) (int, error)
	Revoke(ctx context.Context, userID, id int64) (bool, error)
}

// PATAPI 提供个人 API 令牌的签发、列出与吊销。
//
// 三个接口都只接受会话 Cookie：一把泄漏的 PAT 不该能凭空造出更多 PAT，
// LATHE_ADMIN_TOKEN 也不因「更高级」就越过这条人机边界。签发另需当前口令：
// 只持有 Cookie 不足以签出长期凭据。
type PATAPI struct {
	PATs     PATStore
	Auth     *Auth
	Sessions SessionStore

	limiter     *limiter
	limiterOnce sync.Once
}

// Routes 注册令牌管理接口。
func (p *PATAPI) Routes(mux *http.ServeMux) {
	mux.Handle("GET /api/pat", p.Auth.RequireFunc(p.list))
	mux.Handle("POST /api/pat", p.Auth.RequireFunc(p.create))
	mux.Handle("DELETE /api/pat/{id}", p.Auth.RequireFunc(p.revoke))
}

func (p *PATAPI) lim() *limiter {
	p.limiterOnce.Do(func() { p.limiter = newLimiter() })
	return p.limiter
}

// requireCookieIdentity 是三个 handler 的共同前置。
//
// 只要请求带了 Authorization 头就 403 —— 不是「Bearer 解析失败才 403」：
// Bearer 是 resolve 的优先通道，若放行「别人的 PAT + 自己的 Cookie」，
// Require 认出的身份与 Cookie 判出的身份会是两个人。一刀切掉二义性。
// 之后再核一遍 Cookie 能独立判出与 CurrentUser 相同的用户。
func (p *PATAPI) requireCookieIdentity(w http.ResponseWriter, r *http.Request) (*store.User, bool) {
	const msg = "令牌管理只能用浏览器登录态（不接受 Authorization 头）"
	if r.Header.Get("Authorization") != "" {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": msg})
		return nil, false
	}
	u := CurrentUser(r)
	c, err := r.Cookie(sessionCookie)
	if u == nil || err != nil || p.Sessions == nil {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": msg})
		return nil, false
	}
	cu, err := p.Sessions.Lookup(r.Context(), c.Value)
	if err != nil || cu == nil || cu.ID != u.ID || u.ID == 0 {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": msg})
		return nil, false
	}
	return u, true
}

func (p *PATAPI) list(w http.ResponseWriter, r *http.Request) {
	u, ok := p.requireCookieIdentity(w, r)
	if !ok {
		return
	}
	tokens, err := p.PATs.List(r.Context(), u.ID)
	if err != nil {
		serverError(w, "列出个人 API 令牌失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

// create 签发一把令牌。校验顺序写死（PRD §3.5）：
// Require（401/409）→ Authorization 头（403）→ 限流（429，放行即记一次）
// → 解析体 → 口令（400/403）→ 名字/过期（400）→ 上限（409）→ 入库。
func (p *PATAPI) create(w http.ResponseWriter, r *http.Request) {
	u, ok := p.requireCookieIdentity(w, r)
	if !ok {
		return
	}
	// 限流在读体与验口令之前：被限流的请求不跑 bcrypt，且「第 11 次带正确口令」
	// 同样 429 —— 否则持 Cookie 的攻击者可以把这里当无上限的口令爆破器。
	if !p.lim().allow("patpw:"+strconv.FormatInt(u.ID, 10), patCreateLimit, patCreateWindow) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "尝试过于频繁，请稍后再试"})
		return
	}

	var body struct {
		Name            string     `json:"name"`
		ExpiresInDays   int        `json:"expiresInDays"`
		ExpiresAt       *time.Time `json:"expiresAt"`
		CurrentPassword *string    `json:"currentPassword"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}

	// 口令先于名字校验：不让未验口令的请求探出「名字合法性 / 上限是否已满」。
	// 口令错回 403 而不是 401：前端把任何 401 当登录失效，输错口令不该被踢下线。
	if body.CurrentPassword == nil || *body.CurrentPassword == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "签发令牌需要输入当前密码"})
		return
	}
	if !auth.Verify(u.PasswordHash, *body.CurrentPassword) {
		slog.Info("签发个人 API 令牌被拒：口令不正确", "user_id", u.ID)
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "当前密码不正确"})
		return
	}

	name := body.Name
	if strings.TrimSpace(name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "令牌名字不能为空"})
		return
	}
	if len(name) > maxPATNameBytes {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "令牌名字过长（最多 " + strconv.Itoa(maxPATNameBytes) + " 字节）",
		})
		return
	}

	expiresAt, msg := patExpiry(body.ExpiresInDays, body.ExpiresAt, time.Now())
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg})
		return
	}

	// 先 count 再 insert 有并发窗口（可能变成 21 把）：上限防误操作，不是
	// 安全边界，接受这个偏差，不为它引入锁（PRD §3.10）。
	n, err := p.PATs.CountActive(r.Context(), u.ID)
	if err != nil {
		serverError(w, "统计个人 API 令牌失败", err)
		return
	}
	if n >= MaxPATsPerUser {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "个人 API 令牌已达上限（" + strconv.Itoa(MaxPATsPerUser) + " 把），请先吊销不用的令牌",
		})
		return
	}

	plain, id, err := p.PATs.Create(r.Context(), u.ID, name, expiresAt)
	if err != nil {
		serverError(w, "签发个人 API 令牌失败", err)
		return
	}
	prefix := plain[len(store.PATPrefix) : len(store.PATPrefix)+8]
	slog.Info("签发了个人 API 令牌", "user_id", u.ID, "token_id", id, "prefix", prefix)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":        id,
		"name":      name,
		"prefix":    prefix,
		"token":     plain, // 明文只在这一次响应里出现
		"createdAt": time.Now().UTC(),
		"expiresAt": expiresAt,
	})
}

// patExpiry 把请求里的过期参数换算成绝对时间。nil = 永不过期。
//
// expiresAt（RFC3339）优先于 expiresInDays；两者都缺或天数为 0 即永不过期。
// 非空时必须晚于现在且不超过 MaxPATTTL。返回非空 msg 表示参数非法。
func patExpiry(days int, at *time.Time, now time.Time) (*time.Time, string) {
	const tooLong = "过期时间不能超过 365 天（或选择永不过期）"
	if at != nil {
		if !at.After(now) {
			return nil, "过期时间必须晚于当前时间"
		}
		if at.Sub(now) > MaxPATTTL {
			return nil, tooLong
		}
		t := at.UTC()
		return &t, ""
	}
	if days < 0 {
		return nil, "有效天数不能为负"
	}
	if days == 0 {
		return nil, ""
	}
	if time.Duration(days)*24*time.Hour > MaxPATTTL {
		return nil, tooLong
	}
	t := now.Add(time.Duration(days) * 24 * time.Hour).UTC()
	return &t, ""
}

// revoke 吊销自己的一把令牌。不存在与不是自己的一律 404（不暴露存在性）。
func (p *PATAPI) revoke(w http.ResponseWriter, r *http.Request) {
	u, ok := p.requireCookieIdentity(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "令牌不存在"})
		return
	}
	done, err := p.PATs.Revoke(r.Context(), u.ID, id)
	if err != nil {
		serverError(w, "吊销个人 API 令牌失败", err)
		return
	}
	if !done {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "令牌不存在"})
		return
	}
	slog.Info("吊销了个人 API 令牌", "user_id", u.ID, "token_id", id)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}
