package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/Clouditera/lathe/internal/store"
)

// SMTPVerifier 验证发信通道的连通性。
//
// 不并入 Verifier 接口：那个接口的签名是 Verify(ctx, kind, token)，
// 只接受一个字符串；SMTP 需要主机、端口、加密方式、发件人、用户名、密码
// 六个字段，硬塞进去会污染那个已经很干净的 switch。
type SMTPVerifier interface {
	VerifySMTP(ctx context.Context, cfg store.SMTPConfig, password, testTo string) VerifyResult
}

// SMTPAPI 管理发信通道的配置与验证。
//
// 全部要求管理员：SMTP 是平台级设置，一个普通成员改掉它就能把所有人的
// 密码重置邮件劫持到自己的服务器上。
type SMTPAPI struct {
	Secrets  *store.Secrets
	Verifier SMTPVerifier
	Auth     *Auth
}

// Routes 注册发信配置接口。
func (s *SMTPAPI) Routes(mux *http.ServeMux) {
	mux.Handle("GET /api/smtp", s.Auth.RequireAdminFunc(s.get))
	mux.Handle("PUT /api/smtp", s.Auth.RequireAdminFunc(s.save))
	mux.Handle("POST /api/smtp/verify", s.Auth.RequireAdminFunc(s.verify))
	mux.Handle("DELETE /api/smtp", s.Auth.RequireAdminFunc(s.remove))
}

func (s *SMTPAPI) get(w http.ResponseWriter, r *http.Request) {
	st, err := s.Secrets.SMTPStatus(r.Context())
	if err != nil {
		serverError(w, "读取 SMTP 配置失败", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// save 保存配置并立即投一封测试邮件。
//
// 与凭据接口同构：配完就知道能不能用，不必再点一次验证。验证失败也保存
// —— 可能只是网络暂时不通，配置本身没问题。
func (s *SMTPAPI) save(w http.ResponseWriter, r *http.Request) {
	var body struct {
		store.SMTPConfig
		Password string `json:"password"`
		TestTo   string `json:"testTo"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}

	cfg := body.SMTPConfig
	cfg.Host = strings.TrimSpace(cfg.Host)
	cfg.Username = strings.TrimSpace(cfg.Username)
	cfg.FromAddr = strings.TrimSpace(cfg.FromAddr)
	cfg.FromName = strings.TrimSpace(cfg.FromName)

	if err := s.Secrets.SaveSMTP(r.Context(), cfg, body.Password); err != nil {
		// Validate 的报错是给人看的配置问题，不是服务端故障
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	result := s.runVerify(r.Context(), currentUserEmail(r, body.TestTo))
	writeJSON(w, http.StatusOK, map[string]any{"saved": true, "verify": result})
}

func (s *SMTPAPI) verify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TestTo string `json:"testTo"`
	}
	_ = decodeJSON(r, &body)
	writeJSON(w, http.StatusOK, map[string]any{
		"verify": s.runVerify(r.Context(), currentUserEmail(r, body.TestTo)),
	})
}

func (s *SMTPAPI) remove(w http.ResponseWriter, r *http.Request) {
	if err := s.Secrets.DeleteSMTP(r.Context()); err != nil {
		serverError(w, "删除 SMTP 配置失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// runVerify 执行验证并把结果记进数据库，供界面展示「上次验证情况」。
func (s *SMTPAPI) runVerify(ctx context.Context, testTo string) VerifyResult {
	if s.Verifier == nil {
		return VerifyResult{OK: false, Error: "未配置验证器"}
	}

	cfg, password, err := s.Secrets.LoadSMTP(ctx)
	if err != nil {
		return VerifyResult{OK: false, Error: "尚未配置 SMTP"}
	}

	res := s.Verifier.VerifySMTP(ctx, cfg, password, testTo)
	if res.OK {
		_ = s.Secrets.MarkSMTPVerified(ctx)
	} else {
		_ = s.Secrets.MarkSMTPVerifyFailed(ctx, res.Error)
	}
	return res
}

// currentUserEmail 决定测试邮件发给谁：请求里指定了就用指定的，
// 否则发给当前管理员自己 —— 他最有条件立刻去收件箱确认。
func currentUserEmail(r *http.Request, override string) string {
	if override = strings.TrimSpace(override); override != "" {
		return override
	}
	if u := CurrentUser(r); u != nil {
		return u.Email
	}
	return ""
}
