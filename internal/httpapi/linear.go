package httpapi

import (
	"context"
	"net/http"

	"github.com/Clouditera/lathe/internal/integration/linear"
)

// LinearClientFor 按用户产出 Linear 客户端。
//
// 走函数注入而不是直接依赖 creds.Factory：creds.Verifier 已经依赖
// 本包（VerifyResult），反过来 import 就成环了。main 里用
// factory.ProviderFor(userID).Linear 接线，与执行队列是同一条凭据通路。
type LinearClientFor func(ctx context.Context, userID int64) (*linear.Client, error)

// LinearAPI 是看板「同步 Linear」用的只读接口。
//
// 刻意只读：这里只列 issue、看详情。真正的动作（开始执行）复用
// POST /api/tasks 那条路，不在 Linear 侧制造第二个写入入口。
type LinearAPI struct {
	ClientFor LinearClientFor
	Auth      *Auth
}

// Routes 注册到 mux。
func (a *LinearAPI) Routes(mux *http.ServeMux) {
	mux.Handle("GET /api/linear/issues", a.Auth.RequireFunc(a.listIssues))
	mux.Handle("GET /api/linear/issues/{id}", a.Auth.RequireFunc(a.issueDetail))
}

// client 取当前用户的 Linear 客户端；失败时已写好响应，返回 nil。
func (a *LinearAPI) client(w http.ResponseWriter, r *http.Request) *linear.Client {
	c, err := a.ClientFor(r.Context(), CurrentUser(r).ID)
	if err != nil {
		// 凭据未配置与解密失败走同一个提示 —— 对界面上的人来说，
		// 动作都是去设置页把凭据配好。
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "Linear 凭据不可用，请先到「设置」页配置并验证（" + err.Error() + "）",
		})
		return nil
	}
	return c
}

// listIssues 返回「指派给当前用户、尚未完结」的 Linear issue 列表。
func (a *LinearAPI) listIssues(w http.ResponseWriter, r *http.Request) {
	c := a.client(w, r)
	if c == nil {
		return
	}
	issues, err := c.AssignedIssues(r.Context(), 50)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": issues})
}

// issueDetail 拉单个 issue 的完整信息（含描述与评论），供执行前确认。
//
// id 同时接受 UUID 与 identifier（CR-1326），与 linear.Client.Issue 一致。
func (a *LinearAPI) issueDetail(w http.ResponseWriter, r *http.Request) {
	c := a.client(w, r)
	if c == nil {
		return
	}
	issue, err := c.Issue(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":          issue.ID,
		"identifier":  issue.Identifier,
		"title":       issue.Title,
		"url":         issue.URL,
		"state":       issue.StateName,
		"priority":    issue.Priority,
		"labels":      issue.Labels,
		"description": issue.Description,
		"comments":    issue.Comments,
	})
}
