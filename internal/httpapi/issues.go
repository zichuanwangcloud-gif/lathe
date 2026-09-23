package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// IssuesAPI 是内置工单体系的 HTTP 面（docs/09-internal-issues.md）。
//
// 工单 = 需求的手动载体：人在这里建单、写需求、评论区问答；agent 的
// 提问（blocked_spec）与回帖也落在同一条评论流里（内置 tracker 写
// 库，不经 HTTP）。任务联动（开始执行/取消）走 TaskEnqueuer，
// 与 Linear webhook 路径同一语义。
type IssuesAPI struct {
	Store *store.Store
	Auth  *Auth
	// Queue 承担「开始执行 / 取消联动」；为 nil 时这两个动作返回 503
	// （测试装配允许缺省，生产装配必须接上）。
	Queue IssueQueue
}

// IssueQueue 是工单体系对执行队列的窄需求。
// 与 TaskEnqueuer（webhook.go）刻意分开：那个接口服务 Linear webhook
// 的语义（issueID+issueKey 双标识、UUID 权威），这个接口服务内置工单
// （行内 id / key 定位）。queue 结构体两者都实现。
type IssueQueue interface {
	// EnqueueInternal 为内置工单建出执行任务（state=queued）。
	EnqueueInternal(ctx context.Context, ownerUserID, issueID int64) error
	// CancelForInternalIssue 把该工单名下所有在途任务转 cancelled，
	// 返回被取消的任务 id。只改数据库状态，不停在途 agent
	// （与 T7 取消联动同一已知边界）。
	CancelForInternalIssue(ctx context.Context, ownerUserID int64, issueKey string) ([]int64, error)
}

// Routes 注册内置工单接口。全部按 CurrentUser 隔离：非属主一律 404。
func (a *IssuesAPI) Routes(mux *http.ServeMux) {
	mux.Handle("GET /api/issues", a.Auth.RequireFunc(a.list))
	mux.Handle("POST /api/issues", a.Auth.RequireFunc(a.create))
	mux.Handle("GET /api/issues/{id}", a.Auth.RequireFunc(a.get))
	mux.Handle("PUT /api/issues/{id}", a.Auth.RequireFunc(a.update))
	mux.Handle("DELETE /api/issues/{id}", a.Auth.RequireFunc(a.remove))
	mux.Handle("POST /api/issues/{id}/comments", a.Auth.RequireFunc(a.addComment))
	mux.Handle("POST /api/issues/{id}/start", a.Auth.RequireFunc(a.start))
}

// list：GET /api/issues?state=open&limit=&offset=
func (a *IssuesAPI) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := strings.TrimSpace(q.Get("state"))
	if state != "" && !validIssueState(state) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state 必须是 open / in_progress / done / cancelled"})
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	rows, total, err := a.Store.ListIssues(r.Context(), store.ListIssuesParams{
		UserID: CurrentUser(r).ID, State: state, Limit: limit, Offset: offset,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": rows, "total": total})
}

func validIssueState(s string) bool {
	switch s {
	case store.IssueOpen, store.IssueInProgress, store.IssueDone, store.IssueCancelled:
		return true
	}
	return false
}

// create：POST /api/issues  {repoId, title, description?, priority?}
func (a *IssuesAPI) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RepoID      int64  `json:"repoId"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Priority    int    `json:"priority"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	body.Title = strings.TrimSpace(body.Title)
	if body.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "标题不能为空"})
		return
	}
	if body.RepoID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "必须选择仓库（工单决定任务在哪个仓库上执行）"})
		return
	}
	user := CurrentUser(r)
	// 仓库归属必须现查：不查的话任何登录用户传别人的 repoId 就能把
	// 工单挂到别人仓库下，任务执行时写的也是别人的仓库（越权口子）。
	if _, err := a.Store.GetRepo(r.Context(), body.RepoID, user.ID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "仓库不存在"})
		return
	}

	it, err := a.Store.CreateIssue(r.Context(), store.CreateIssueParams{
		UserID: user.ID, RepoID: body.RepoID,
		Title: body.Title, Description: body.Description, Priority: body.Priority,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"issue": it})
}

// get：GET /api/issues/{id} —— 工单 + 评论 + 关联任务（同源一页数据）。
func (a *IssuesAPI) get(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	user := CurrentUser(r)
	it, err := a.Store.GetIssue(r.Context(), id, user.ID)
	if err != nil {
		issueNotFound(w)
		return
	}
	comments, err := a.Store.ListComments(r.Context(), it.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	tasks, err := a.Store.ListIssueTasks(r.Context(), user.ID, it.Key)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"issue": it, "comments": comments, "tasks": tasks,
	})
}

// update：PUT /api/issues/{id}  {title?, description?, priority?, state?}
//
// state 改为 cancelled 时联动取消在途任务（09 §3 F4-AC4）：
// 与 Linear webhook 的取消联动同一条执行体，只是触发点在应用内。
func (a *IssuesAPI) update(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Title       *string `json:"title"`
		Description *string `json:"description"`
		Priority    *int    `json:"priority"`
		State       *string `json:"state"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	if body.Title != nil {
		*body.Title = strings.TrimSpace(*body.Title)
		if *body.Title == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "标题不能为空"})
			return
		}
	}
	if body.State != nil && !validIssueState(*body.State) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state 必须是 open / in_progress / done / cancelled"})
		return
	}
	user := CurrentUser(r)
	before, err := a.Store.GetIssue(r.Context(), id, user.ID)
	if err != nil {
		issueNotFound(w)
		return
	}
	it, err := a.Store.UpdateIssue(r.Context(), id, user.ID, store.UpdateIssueParams{
		Title: body.Title, Description: body.Description, Priority: body.Priority, State: body.State,
	})
	if err != nil {
		var te store.ErrIssueTransition
		if errors.As(err, &te) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": te.Error()})
			return
		}
		if errors.Is(err, store.ErrIssueNotFound) {
			issueNotFound(w)
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	// 取消联动：open/in_progress → cancelled 才触发（done/cancelled →
	// cancelled 不存在于转移表，到这里一定是前者）。被取消的任务 id
	// 透传给人，与 webhook 路径的可观测性一致。
	var cancelledTasks []int64
	if body.State != nil && *body.State == store.IssueCancelled && before.State != store.IssueCancelled {
		if a.Queue == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "执行队列未接线，工单状态已改但任务取消未执行"})
			return
		}
		cancelledTasks, err = a.Queue.CancelForInternalIssue(r.Context(), user.ID, it.Key)
		if err != nil {
			// 工单状态已落库；取消失败不滚回（与 webhook 路径一致：
			// 部分完成 + 告警，人可以在任务详情页手动取消剩余任务）。
			writeJSON(w, http.StatusOK, map[string]any{
				"issue": it, "warning": "工单已取消，但联动取消任务失败: " + err.Error(),
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"issue": it, "cancelledTasks": cancelledTasks})
}

// remove：DELETE /api/issues/{id}
//
// 有关联任务（含历史终态）的工单只能取消不能删 —— 任务是审计留痕，
// 删了工单任务行就变成没有需求载体的孤儿（F1-AC5）。
func (a *IssuesAPI) remove(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	err := a.Store.DeleteIssue(r.Context(), id, CurrentUser(r).ID)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrIssueHasTasks):
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		case errors.Is(err, store.ErrIssueNotFound):
			issueNotFound(w)
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

// addComment：POST /api/issues/{id}/comments  {body}
//
// 人是作者（user_id），agent 的评论由内置 tracker 以 actor 写入，
// 两条作者通道在 schema 层互斥（issue_comments_author_check）。
func (a *IssuesAPI) addComment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	body.Body = strings.TrimSpace(body.Body)
	if body.Body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "评论不能为空"})
		return
	}
	if len(body.Body) > 10000 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "评论过长（上限 10000 字符）"})
		return
	}
	user := CurrentUser(r)
	it, err := a.Store.GetIssue(r.Context(), id, user.ID)
	if err != nil {
		issueNotFound(w)
		return
	}
	c, err := a.Store.AddComment(r.Context(), it.ID, &user.ID, nil, body.Body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"comment": c})
}

// start：POST /api/issues/{id}/start —— 为工单建出执行任务（202 入队）。
func (a *IssuesAPI) start(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if a.Queue == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "执行队列未接线"})
		return
	}
	user := CurrentUser(r)
	it, err := a.Store.GetIssue(r.Context(), id, user.ID)
	if err != nil {
		issueNotFound(w)
		return
	}
	if err := a.Queue.EnqueueInternal(r.Context(), user.ID, it.ID); err != nil {
		status := http.StatusInternalServerError
		msg := err.Error()
		// 状态闸门与唯一索引冲突都是「人可理解的拒绝」，不是服务器错误
		if strings.Contains(msg, "已取消") || strings.Contains(msg, "进行中") ||
			strings.Contains(msg, "duplicate key") {
			status = http.StatusConflict
		} else if errors.Is(err, store.ErrIssueNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]any{"error": msg})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "issue": it.Key})
}

func issueNotFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{"error": "工单不存在"})
}
