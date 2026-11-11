package httpapi

// 团队与角色（f12，docs/features/team-roles.md）—— 红阶段空壳：路由表齐全，
// handler 一律 404，供先红后绿的第一个提交编译通过。

import (
	"net/http"
	"sync"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// TeamAPI 提供团队、成员、邀请码与仓库共享接口。
type TeamAPI struct {
	Store *store.Store
	Auth  *Auth
	// TrustProxy 决定猜码限流是否采信 X-Forwarded-For（与 AccountAPI 同口径）。
	TrustProxy bool

	// now 是限流用的时钟；nil 时用 time.Now。不导出、不进 cmd/lathe 装配，只给测试注入假时钟。
	now func() time.Time

	limiter     *limiter
	limiterOnce sync.Once
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
	{"GET", "/api/teams", (*TeamAPI).notImplemented, false},
	{"POST", "/api/teams", (*TeamAPI).notImplemented, false},
	{"GET", "/api/teams/{id}", (*TeamAPI).notImplemented, false},
	{"PATCH", "/api/teams/{id}", (*TeamAPI).notImplemented, true},
	{"DELETE", "/api/teams/{id}", (*TeamAPI).notImplemented, true},
	{"POST", "/api/teams/{id}/invitations", (*TeamAPI).notImplemented, true},
	{"GET", "/api/teams/{id}/invitations", (*TeamAPI).notImplemented, false},
	{"DELETE", "/api/teams/{id}/invitations/{iid}", (*TeamAPI).notImplemented, true},
	{"POST", "/api/team-invitations/accept", (*TeamAPI).notImplemented, true},
	{"PATCH", "/api/teams/{id}/members/{uid}", (*TeamAPI).notImplemented, true},
	{"DELETE", "/api/teams/{id}/members/{uid}", (*TeamAPI).notImplemented, true},
	{"POST", "/api/teams/{id}/transfer", (*TeamAPI).notImplemented, true},
	{"POST", "/api/repos/{id}/sharing", (*TeamAPI).notImplemented, true},
}

// Routes 把团队接口注册到 mux。全部要求登录；团队内角色在 handler / store 里判定。
func (a *TeamAPI) Routes(mux *http.ServeMux) {
	for _, rt := range teamRoutes {
		h := rt.Handler
		mux.Handle(rt.Method+" "+rt.Pattern, a.Auth.RequireFunc(func(w http.ResponseWriter, r *http.Request) {
			h(a, w, r)
		}))
	}
}

func (a *TeamAPI) notImplemented(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]any{"error": "团队不存在"})
}

// teamError 把 store 的团队错误映射成 HTTP（空壳：一律 500）。
func teamError(w http.ResponseWriter, err error) {
	serverError(w, "团队操作失败", err)
}
