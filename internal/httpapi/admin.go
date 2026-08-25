package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/Clouditera/lathe/internal/auth"
	"github.com/Clouditera/lathe/internal/store"
)

// AdminAPI 提供用户管理与统计。全部要求管理员。
type AdminAPI struct {
	Users    *store.Users
	Sessions SessionStore
	Resets   *store.Resets
	Auth     *Auth
	// Store 读写系统设置（预览资源阈值等）；为 nil 时设置端点不可用。
	Store *store.Store
}

// Routes 注册用户管理接口。
func (a *AdminAPI) Routes(mux *http.ServeMux) {
	mux.Handle("GET /api/admin/users", a.Auth.RequireAdminFunc(a.list))
	mux.Handle("POST /api/admin/users/{id}/enable", a.Auth.RequireAdminFunc(a.enable))
	mux.Handle("POST /api/admin/users/{id}/disable", a.Auth.RequireAdminFunc(a.disable))
	mux.Handle("POST /api/admin/users/{id}/role", a.Auth.RequireAdminFunc(a.setRole))
	mux.Handle("POST /api/admin/users/{id}/password", a.Auth.RequireAdminFunc(a.resetPassword))
	mux.Handle("DELETE /api/admin/users/{id}", a.Auth.RequireAdminFunc(a.remove))
	mux.Handle("GET /api/admin/settings", a.Auth.RequireAdminFunc(a.getSettings))
	mux.Handle("PUT /api/admin/settings", a.Auth.RequireAdminFunc(a.putSettings))
}

// getSettings 返回系统设置（当前只有预览资源阈值）。
func (a *AdminAPI) getSettings(w http.ResponseWriter, r *http.Request) {
	mem, disk, err := a.Store.PreviewThresholds(r.Context())
	if err != nil {
		serverError(w, "读取系统设置失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"previewMemThreshold":  mem,
		"previewDiskThreshold": disk,
	})
}

// putSettings 保存系统设置。阈值口径 1..100（100 = 不启用该闸门）。
func (a *AdminAPI) putSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PreviewMemThreshold  int `json:"previewMemThreshold"`
		PreviewDiskThreshold int `json:"previewDiskThreshold"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxJSONBody)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON"})
		return
	}
	if err := store.ValidateThreshold(body.PreviewMemThreshold); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "内存" + err.Error()})
		return
	}
	if err := store.ValidateThreshold(body.PreviewDiskThreshold); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "磁盘" + err.Error()})
		return
	}
	ctx := r.Context()
	if err := a.Store.SetSetting(ctx, store.SettingPreviewMemThreshold, strconv.Itoa(body.PreviewMemThreshold)); err != nil {
		serverError(w, "保存内存阈值失败", err)
		return
	}
	if err := a.Store.SetSetting(ctx, store.SettingPreviewDiskThreshold, strconv.Itoa(body.PreviewDiskThreshold)); err != nil {
		serverError(w, "保存磁盘阈值失败", err)
		return
	}
	slog.Info("系统设置已更新", "previewMem", body.PreviewMemThreshold, "previewDisk", body.PreviewDiskThreshold, "by", actorOf(r))
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (a *AdminAPI) list(w http.ResponseWriter, r *http.Request) {
	rows, err := a.Users.List(r.Context())
	if err != nil {
		serverError(w, "查询用户列表失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": rows, "total": len(rows)})
}

// target 解析并加载被操作的账号，同时挡住「拿自己开刀」。
//
// selfAllowed 为 false 时禁止操作自己：管理员把自己停用或删掉，就再没人
// 能进设置页了 —— 这类误操作要在入口挡住，而不是靠人小心。
func (a *AdminAPI) target(w http.ResponseWriter, r *http.Request, selfAllowed bool) (*store.User, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "用户 ID 非法"})
		return nil, false
	}

	if !selfAllowed {
		if me := CurrentUser(r); me != nil && me.ID == id {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "不能对自己执行这个操作"})
			return nil, false
		}
	}

	u, err := a.Users.ByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "用户不存在"})
			return nil, false
		}
		serverError(w, "查询用户失败", err)
		return nil, false
	}
	return u, true
}

// guardLastAdmin 挡住「把最后一个管理员降级/停用/删除」。
//
// 平台必须始终至少有一个可用的管理员，否则设置页就永远进不去了 ——
// 只能靠改数据库救回来。
func (a *AdminAPI) guardLastAdmin(w http.ResponseWriter, r *http.Request, u *store.User) bool {
	if !u.IsAdmin() {
		return true
	}
	n, err := a.Users.CountAdmins(r.Context(), u.ID)
	if err != nil {
		serverError(w, "统计管理员数失败", err)
		return false
	}
	if n == 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "这是最后一个可用的管理员，不能停用、降级或删除",
		})
		return false
	}
	return true
}

func (a *AdminAPI) enable(w http.ResponseWriter, r *http.Request) {
	u, ok := a.target(w, r, true)
	if !ok {
		return
	}
	if err := a.Users.SetDisabled(r.Context(), u.ID, false); err != nil {
		serverError(w, "启用账号失败", err)
		return
	}
	slog.Info("管理员启用了账号", "target", u.ID, "by", actorOf(r))
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// disable 停用账号并立刻踢掉它的全部在线会话。
//
// 两件事缺一不可：不删会话的话，对方手里的 Cookie 还能继续用到过期，
// 「停用」就成了摆设。（Sessions.Lookup 里那个 disabled_at IS NULL 是
// 第二道防线，防的是有人直接改库停用。）
func (a *AdminAPI) disable(w http.ResponseWriter, r *http.Request) {
	u, ok := a.target(w, r, false)
	if !ok {
		return
	}
	if !a.guardLastAdmin(w, r, u) {
		return
	}
	if err := a.Users.SetDisabled(r.Context(), u.ID, true); err != nil {
		serverError(w, "停用账号失败", err)
		return
	}
	if err := a.Sessions.DeleteUser(r.Context(), u.ID); err != nil {
		serverError(w, "清除会话失败", err)
		return
	}
	slog.Info("管理员停用了账号", "target", u.ID, "by", actorOf(r))
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (a *AdminAPI) setRole(w http.ResponseWriter, r *http.Request) {
	u, ok := a.target(w, r, false)
	if !ok {
		return
	}

	var body struct {
		Role string `json:"role"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	if !store.ValidRole(body.Role) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "角色须为 admin 或 member"})
		return
	}
	// 降级同样可能干掉最后一个管理员
	if body.Role == store.RoleMember && !a.guardLastAdmin(w, r, u) {
		return
	}

	if err := a.Users.SetRole(r.Context(), u.ID, body.Role); err != nil {
		serverError(w, "更新角色失败", err)
		return
	}
	slog.Info("管理员改了账号角色", "target", u.ID, "role", body.Role, "by", actorOf(r))
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "role": body.Role})
}

// resetPassword 代用户设置新密码。
//
// 不要求知道对方旧密码 —— 这正是它存在的意义：SMTP 挂了、或者用户
// 连注册邮箱都进不去时，这是唯一能把人救回来的路。
//
// 请求体不给密码就随机生成一个，明文只在这一次响应里返回。
func (a *AdminAPI) resetPassword(w http.ResponseWriter, r *http.Request) {
	u, ok := a.target(w, r, true)
	if !ok {
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}

	pw, generated := body.Password, false
	if pw == "" {
		pw, generated = auth.RandomPassword(), true
	}
	if err := auth.Policy(pw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	hash, err := auth.Hash(pw)
	if err != nil {
		serverError(w, "生成密码哈希失败", err)
		return
	}
	// 置 mustChange：这个密码要经第三方之手转交，必须用一次就换掉
	if err := a.Users.SetPassword(r.Context(), u.ID, hash, true); err != nil {
		serverError(w, "重置密码失败", err)
		return
	}
	_ = a.Resets.DeleteUser(r.Context(), u.ID)
	_ = a.Sessions.DeleteUser(r.Context(), u.ID)

	slog.Info("管理员重置了账号密码", "target", u.ID, "by", actorOf(r))
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "password": pw, "generated": generated,
	})
}

// remove 删除账号及其名下全部数据。
//
// 任务、仓库配置、凭据、会话、重置令牌都由外键 ON DELETE CASCADE 带走。
// 唯一带不走的是 worktree 占的磁盘目录 —— 先把路径打进日志，让人能手工
// 回收；自动回收留到第二步（那时任务归属清晰）。
func (a *AdminAPI) remove(w http.ResponseWriter, r *http.Request) {
	u, ok := a.target(w, r, false)
	if !ok {
		return
	}
	if !a.guardLastAdmin(w, r, u) {
		return
	}

	if paths, err := a.Users.WorktreePaths(r.Context(), u.ID); err == nil && len(paths) > 0 {
		slog.Warn("删除用户后以下工作区目录需手工清理", "user", u.ID, "paths", paths)
	}

	if err := a.Users.Delete(r.Context(), u.ID); err != nil {
		serverError(w, "删除用户失败", err)
		return
	}
	slog.Info("管理员删除了账号", "target", u.ID, "email", u.Email, "by", actorOf(r))
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}
