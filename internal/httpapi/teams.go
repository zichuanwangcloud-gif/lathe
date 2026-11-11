package httpapi

// 团队与角色（f12，docs/features/team-roles.md）。
//
// 团队共享的是【可见性】和团队内的组织管理，不是执行身份，也不是仓库配置的编辑权。
// 本文件只放团队、成员、邀请码与仓库共享的接口；任务/仓库读接口的团队可见性
// 改在 api.go（taskDetail → TaskDetailForViewer、ownedTask、scope=team）。
//
// 拒绝形态全站只有一种：看不见或不能做，都是 404（「团队不存在」/「仓库不存在」）。
// 唯一的非 404 拒绝是授权通过之后的业务约束：owner 降/退自己、删有共享仓库的团队
// → 409，字段非法 → 400，锁冲突重试耗尽 → 503。

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// 猜码限流：每 IP 15 分钟 10 次（每次 accept 请求都计数，不论成败），超出 429。
// 128 位码不可暴力，限流挡的是脚本扫码与日志刷屏；10 次/15 分钟远高于人手输错的
// 频率。业务常量，不做环境变量。
const (
	inviteAcceptLimit  = 10
	inviteAcceptWindow = 15 * time.Minute
)

// TeamAPI 提供团队、成员、邀请码与仓库共享接口。
type TeamAPI struct {
	Store *store.Store
	Auth  *Auth
	// TrustProxy 决定猜码限流是否采信 X-Forwarded-For（与 AccountAPI 同口径）。
	TrustProxy bool

	// now 是限流用的时钟；nil 时用 time.Now。不导出、不进 cmd/lathe 装配，只给测试注入假时钟。
	now func() time.Time

	// limiter 是本 API 私有的限流实例（与 AccountAPI.lim()、Auth.loginLimiter() 同构，
	// 不与它们共享）。
	limiter     *limiter
	limiterOnce sync.Once
}

func (a *TeamAPI) lim() *limiter {
	a.limiterOnce.Do(func() {
		if a.limiter == nil {
			a.limiter = newLimiter()
		}
	})
	return a.limiter
}

func (a *TeamAPI) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// teamRoute 是 teams.go 的全部路由。Routes() 与权限覆盖测试（AC-20）都从这张表读：
// net/http.ServeMux 不能枚举已注册路由，所以表必须由代码自己维护。
// 新增路由【必须】在这里加一行，否则 TestTeamEveryWriteRouteHasDenyCase 会失败。
type teamRoute struct {
	Method  string
	Pattern string
	Handler func(*TeamAPI, http.ResponseWriter, *http.Request)
	// Write 表示需要团队内授权（或仓库属主身份）的写路由，必须配一条返回 404 的拒绝用例。
	// 建团队对任何登录用户都放行、没有拒绝形态，因此不标 Write。
	Write bool
}

var teamRoutes = []teamRoute{
	{"GET", "/api/teams", (*TeamAPI).listTeams, false},
	{"POST", "/api/teams", (*TeamAPI).createTeam, false},
	{"GET", "/api/teams/{id}", (*TeamAPI).getTeam, false},
	{"PATCH", "/api/teams/{id}", (*TeamAPI).renameTeam, true},
	{"DELETE", "/api/teams/{id}", (*TeamAPI).deleteTeam, true},
	{"POST", "/api/teams/{id}/invitations", (*TeamAPI).createInvitation, true},
	{"GET", "/api/teams/{id}/invitations", (*TeamAPI).listInvitations, false},
	{"DELETE", "/api/teams/{id}/invitations/{iid}", (*TeamAPI).revokeInvitation, true},
	{"POST", "/api/team-invitations/accept", (*TeamAPI).acceptInvitation, true},
	{"PATCH", "/api/teams/{id}/members/{uid}", (*TeamAPI).setMemberRole, true},
	{"DELETE", "/api/teams/{id}/members/{uid}", (*TeamAPI).removeMember, true},
	{"POST", "/api/teams/{id}/transfer", (*TeamAPI).transferOwner, true},
	{"POST", "/api/repos/{id}/sharing", (*TeamAPI).shareRepo, true},
}

// Routes 把团队接口注册到 mux。全部要求登录；团队内角色在 store 的事务里判定
// （锁内重读调用者角色，不在 handler 里先读后写）。
func (a *TeamAPI) Routes(mux *http.ServeMux) {
	for _, rt := range teamRoutes {
		h := rt.Handler
		mux.Handle(rt.Method+" "+rt.Pattern, a.Auth.RequireFunc(func(w http.ResponseWriter, r *http.Request) {
			h(a, w, r)
		}))
	}
}

var teamNotFound = map[string]any{"error": "团队不存在"}

// teamError 把 store 的团队错误映射成 HTTP。未知错误才是 500。
func teamError(w http.ResponseWriter, err error) {
	var owns *store.UserOwnsTeamError
	switch {
	case errors.Is(err, store.ErrTeamBusy):
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "团队数据正忙，请重试"})
	case errors.Is(err, store.ErrTeamNotFound), errors.Is(err, store.ErrInvitationNotFound):
		writeJSON(w, http.StatusNotFound, teamNotFound)
	case errors.Is(err, store.ErrRepoNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "仓库不存在"})
	case errors.Is(err, store.ErrInviteInvalid):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "邀请码无效或已失效"})
	case errors.Is(err, store.ErrTeamNeedsOwner), errors.Is(err, store.ErrTeamHasSharedRepos):
		writeJSON(w, http.StatusConflict, map[string]any{"error": strings.TrimPrefix(err.Error(), "store: ")})
	case errors.As(err, &owns):
		writeJSON(w, http.StatusConflict, map[string]any{"error": strings.TrimPrefix(owns.Error(), "store: ")})
	case errors.Is(err, store.ErrInvalidTeamRole), errors.Is(err, store.ErrInvalidTeamName):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": strings.TrimPrefix(err.Error(), "store: ")})
	default:
		serverError(w, "团队操作失败", err)
	}
}

// pathInt 解析路径参数；非法时按「团队不存在」处理（不给 ID 格式探针多一种响应）。
func pathInt(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusNotFound, teamNotFound)
		return 0, false
	}
	return id, true
}

func (a *TeamAPI) listTeams(w http.ResponseWriter, r *http.Request) {
	teams, err := a.Store.ListMyTeams(r.Context(), CurrentUser(r).ID)
	if err != nil {
		teamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"teams": teams})
}

func (a *TeamAPI) createTeam(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	team, err := a.Store.CreateTeam(r.Context(), CurrentUser(r).ID, strings.TrimSpace(body.Name))
	if err != nil {
		teamError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, team)
}

func (a *TeamAPI) getTeam(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	d, err := a.Store.GetTeam(r.Context(), id, CurrentUser(r).ID)
	if err != nil {
		teamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (a *TeamAPI) renameTeam(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	if err := a.Store.RenameTeam(r.Context(), id, CurrentUser(r).ID, strings.TrimSpace(body.Name)); err != nil {
		teamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (a *TeamAPI) deleteTeam(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	if err := a.Store.DeleteTeam(r.Context(), id, CurrentUser(r).ID); err != nil {
		teamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// createInvitation 生成一次性邀请码。响应里带明文码（仅此一次）；不查 users 表。
func (a *TeamAPI) createInvitation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	var body struct {
		Role         string `json:"role"`
		InviteeEmail string `json:"inviteeEmail"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	role := strings.TrimSpace(body.Role)
	if role == "" {
		role = store.TeamRoleMember
	}
	inv, err := a.Store.CreateInvitation(r.Context(), id, CurrentUser(r).ID, role, strings.TrimSpace(body.InviteeEmail))
	if err != nil {
		teamError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, inv)
}

func (a *TeamAPI) listInvitations(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	invs, err := a.Store.ListInvitations(r.Context(), id, CurrentUser(r).ID)
	if err != nil {
		teamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invitations": invs})
}

func (a *TeamAPI) revokeInvitation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	iid, ok := pathInt(w, r, "iid")
	if !ok {
		return
	}
	if err := a.Store.RevokeInvitation(r.Context(), id, iid, CurrentUser(r).ID); err != nil {
		teamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// acceptInvitation 用一次性码加入团队。先限流（窗口内正确码也不放行），再判码。
func (a *TeamAPI) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	key := "invaccept:" + clientIP(r, a.TrustProxy)
	if !a.lim().allowAt(key, inviteAcceptLimit, inviteAcceptWindow, a.clock()) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "尝试过于频繁，请稍后再试"})
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	teamID, err := a.Store.AcceptInvitation(r.Context(), strings.TrimSpace(body.Code), CurrentUser(r).ID)
	if err != nil {
		teamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "teamId": teamID})
}

func (a *TeamAPI) setMemberRole(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	uid, ok := pathInt(w, r, "uid")
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
	if err := a.Store.SetMemberRole(r.Context(), id, CurrentUser(r).ID, uid, strings.TrimSpace(body.Role)); err != nil {
		teamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// removeMember 移除成员；uid = 自己时是退出。同一事务内收回离队者的共享。
func (a *TeamAPI) removeMember(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	uid, ok := pathInt(w, r, "uid")
	if !ok {
		return
	}
	unshared, err := a.Store.RemoveMember(r.Context(), id, CurrentUser(r).ID, uid)
	if err != nil {
		teamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "unshared": unshared})
}

func (a *TeamAPI) transferOwner(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	var body struct {
		UserID int64 `json:"userId"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	if err := a.Store.TransferOwner(r.Context(), id, CurrentUser(r).ID, body.UserID); err != nil {
		teamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// shareRepo 把仓库共享到团队（{teamId: T}）或取消共享（{teamId: null}）。只有仓库属主。
// 非属主 → 404「仓库不存在」；团队不存在与「不是该团队成员」→ 同一个 404「团队不存在」。
func (a *TeamAPI) shareRepo(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "仓库不存在"})
		return
	}
	var body struct {
		TeamID *int64 `json:"teamId"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	if err := a.Store.ShareRepo(r.Context(), id, CurrentUser(r).ID, body.TeamID); err != nil {
		teamError(w, err)
		return
	}
	slog.Info("仓库共享已变更", "repo", id, "team", body.TeamID, "by", actorOf(r))
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "teamId": body.TeamID})
}
