package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Clouditera/lathe/internal/store"
)

// VerifyResult 是一次连接验证的结果。
type VerifyResult struct {
	OK bool `json:"ok"`
	// AccountName 是验证成功时拿到的账号标识，用来确认"配的是哪个账号"。
	AccountName string `json:"accountName,omitempty"`
	// AccountID 是外部账号 ID（Linear 场景下即接单判定所需的用户 ID）。
	AccountID string `json:"accountId,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Verifier 验证某类凭据的连通性。
type Verifier interface {
	Verify(ctx context.Context, kind, token string) VerifyResult
}

// CredentialAPI 管理凭据的配置与验证。
//
// P1.5 第二步：凭据按 CurrentUser 归属，各配各的。第一步那个
// 「人人写的都是超管名下凭据」的洞就此堵上 —— 配套的缓存失效回调
// 也要带上用户，否则 A 改凭据会把 B 的缓存也清掉（或更糟：清不掉）。
type CredentialAPI struct {
	Secrets  *store.Secrets
	Verifier Verifier
	Auth     *Auth
	// OnChange 在凭据变更后调用，用于让该用户的缓存立即失效。
	OnChange func(userID int64, kind string)
	// EnvConfigured 报告某类凭据是否有环境变量兜底值。
	// 环境变量是部署者（超管）的兜底，普通成员不该看到「已配置（来自环境变量）」
	// —— 那会让成员以为自己的任务有凭据可用，实际并不会用到。
	EnvConfigured func(kind string) bool
}

// owner 返回本次请求的凭据属主。
func (c *CredentialAPI) owner(r *http.Request) int64 {
	return CurrentUser(r).ID
}

// Routes 注册凭据管理接口。
func (c *CredentialAPI) Routes(mux *http.ServeMux) {
	mux.Handle("GET /api/integrations", c.Auth.RequireFunc(c.list))
	mux.Handle("PUT /api/integrations/{kind}", c.Auth.RequireFunc(c.save))
	mux.Handle("POST /api/integrations/{kind}/verify", c.Auth.RequireFunc(c.verify))
	mux.Handle("DELETE /api/integrations/{kind}", c.Auth.RequireFunc(c.remove))
}

func (c *CredentialAPI) list(w http.ResponseWriter, r *http.Request) {
	u := CurrentUser(r)
	items, err := c.Secrets.Status(r.Context(), u.ID)
	if err != nil {
		serverError(w, "查询凭据状态失败", err)
		return
	}

	// 库里没有但环境变量里有的，标为 env 来源 —— 让人看清当前生效的是哪个，
	// 避免"我明明配了环境变量，界面却说未配置"的困惑。
	// 只对管理员展示：环境变量兜底只对内置管理员生效（见 creds.Factory），
	// 给成员看到这个标注是误导。
	if c.EnvConfigured != nil && u.IsAdmin() {
		for i := range items {
			if !items[i].Configured && c.EnvConfigured(items[i].Kind) {
				items[i].Configured = true
				items[i].Source = "env"
				items[i].Masked = "（来自环境变量）"
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"integrations": items})
}

// save 保存凭据并立即验证。
//
// 保存后马上验证是刻意的：配完就知道能不能用，不必再点一次。
// 验证失败也保存 —— 可能只是网络暂时不通，凭据本身没问题。
func (c *CredentialAPI) save(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if !store.ValidKind(kind) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未知凭据类型 " + kind})
		return
	}

	var body struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	body.Token = strings.TrimSpace(body.Token)
	if body.Token == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "凭据不能为空"})
		return
	}

	if err := c.Secrets.Save(r.Context(), c.owner(r), kind, body.Token); err != nil {
		serverError(w, "保存凭据失败", err)
		return
	}
	c.invalidate(c.owner(r), kind)

	result := c.runVerify(r.Context(), c.owner(r), kind, body.Token)
	writeJSON(w, http.StatusOK, map[string]any{"saved": true, "verify": result})
}

// verify 验证已保存的凭据。
func (c *CredentialAPI) verify(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if !store.ValidKind(kind) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未知凭据类型 " + kind})
		return
	}

	token, err := c.Secrets.Get(r.Context(), c.owner(r), kind)
	if err != nil {
		if errors.Is(err, store.ErrIntegrationNotFound) {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "该凭据尚未配置（管理员也可配在环境变量里；请填入此处以便验证）",
			})
			return
		}
		serverError(w, "读取凭据失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"verify": c.runVerify(r.Context(), c.owner(r), kind, token)})
}

func (c *CredentialAPI) remove(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if !store.ValidKind(kind) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未知凭据类型 " + kind})
		return
	}
	if err := c.Secrets.Delete(r.Context(), c.owner(r), kind); err != nil {
		serverError(w, "删除凭据失败", err)
		return
	}
	c.invalidate(c.owner(r), kind)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// runVerify 执行验证并把结果记进数据库，供界面展示"上次验证情况"。
func (c *CredentialAPI) runVerify(ctx context.Context, userID int64, kind, token string) VerifyResult {
	if c.Verifier == nil {
		return VerifyResult{OK: false, Error: "未配置验证器"}
	}
	res := c.Verifier.Verify(ctx, kind, token)

	if res.OK {
		_ = c.Secrets.MarkVerified(ctx, userID, kind, res.AccountName)
		if res.AccountID != "" {
			// Linear 场景：把账号 ID 存下来，接单判定直接用，
			// 免去人工去 Linear 里翻自己的 user id
			_ = c.Secrets.SetAccountName(ctx, userID, kind, res.AccountID)
		}
	} else {
		_ = c.Secrets.MarkVerifyFailed(ctx, userID, kind, res.Error)
	}
	return res
}

func (c *CredentialAPI) invalidate(userID int64, kind string) {
	if c.OnChange != nil {
		c.OnChange(userID, kind)
	}
}
