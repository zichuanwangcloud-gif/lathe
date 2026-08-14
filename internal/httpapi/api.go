package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Clouditera/lathe/internal/store"
	"github.com/Clouditera/lathe/internal/task"
)

// maxJSONBody 限制请求体大小。
const maxJSONBody = 1 << 20

// API 提供管理界面所需的读写接口。
type API struct {
	Store *store.Store
	Tasks *task.Machine
	Queue TaskEnqueuer
	Auth  *Auth

	// ConfigStatus 返回凭据配置状态（不含 token 本身）。
	ConfigStatus func() map[string]any
}

// Routes 把 API 注册到 mux。
//
// 读接口同样要求登录：任务详情里含 issue 标题、分支名、失败原因，
// 属于内部信息，不该匿名可读。
func (a *API) Routes(mux *http.ServeMux) {
	// 鉴权自身的端点不设保护
	mux.HandleFunc("POST /api/login", a.Auth.Login)
	mux.HandleFunc("POST /api/logout", a.Auth.Logout)
	mux.HandleFunc("GET /api/me", a.Auth.Me)

	// 读
	mux.Handle("GET /api/tasks", a.Auth.RequireFunc(a.listTasks))
	mux.Handle("GET /api/tasks/{id}", a.Auth.RequireFunc(a.taskDetail))
	mux.Handle("GET /api/stats", a.Auth.RequireFunc(a.stats))
	mux.Handle("GET /api/repos", a.Auth.RequireFunc(a.listRepos))
	mux.Handle("GET /api/config", a.Auth.RequireFunc(a.config))

	// 写
	mux.Handle("POST /api/tasks", a.Auth.RequireFunc(a.triggerTask))
	mux.Handle("POST /api/tasks/{id}/retry", a.Auth.RequireFunc(a.retryTask))
	mux.Handle("POST /api/tasks/{id}/cancel", a.Auth.RequireFunc(a.cancelTask))
	mux.Handle("PUT /api/repos/{id}", a.Auth.RequireFunc(a.updateRepo))
}

func (a *API) listTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	var states []string
	if s := strings.TrimSpace(q.Get("state")); s != "" {
		for _, part := range strings.Split(s, ",") {
			part = strings.TrimSpace(part)
			// 只接受已知状态，避免把任意字符串带进查询
			if task.State(part).Valid() {
				states = append(states, part)
			}
		}
		if len(states) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state 参数不含任何已知状态"})
			return
		}
	}

	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	rows, total, err := a.Store.ListTasks(r.Context(), store.ListTasksParams{
		States: states, Limit: limit, Offset: offset,
	})
	if err != nil {
		serverError(w, "查询任务列表失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks": rows, "total": total, "limit": limit, "offset": offset,
	})
}

func (a *API) taskDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	detail, err := a.Store.TaskDetail(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "任务不存在"})
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (a *API) stats(w http.ResponseWriter, r *http.Request) {
	st, err := a.Store.Stats(r.Context())
	if err != nil {
		serverError(w, "统计失败", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (a *API) listRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := a.Store.ListRepos(r.Context())
	if err != nil {
		serverError(w, "查询仓库失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos})
}

// config 返回凭据配置状态。
//
// 只报告「配没配、从哪读的」，绝不返回 token 本身 —— 界面上能看到的
// 东西一律不该包含密钥。P0 的凭据仍走环境变量，界面不提供填写入口，
// 因为把明文密钥写进库与 integrations 表的设计约定冲突（token_ref
// 指向外部 secret store），那要等 P2 接了真正的 secret store 再做。
func (a *API) config(w http.ResponseWriter, r *http.Request) {
	if a.ConfigStatus == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	writeJSON(w, http.StatusOK, a.ConfigStatus())
}

func (a *API) triggerTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IssueID  string `json:"issueId"`
		IssueKey string `json:"issueKey"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	body.IssueID = strings.TrimSpace(body.IssueID)
	body.IssueKey = strings.TrimSpace(body.IssueKey)

	if body.IssueID == "" && body.IssueKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "需要 issueId 或 issueKey"})
		return
	}
	// Linear 的 issue 查询同时接受 UUID 与 identifier，缺一个就用另一个顶上
	if body.IssueID == "" {
		body.IssueID = body.IssueKey
	}
	if body.IssueKey == "" {
		body.IssueKey = body.IssueID
	}

	if err := a.Queue.Enqueue(r.Context(), body.IssueID, body.IssueKey); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "issue": body.IssueKey})
}

// retryTask 把失败任务重新入队。
//
// 走 failed→queued 这条合法边（见 internal/task/state.go），
// 而不是绕过状态机直接改表。
func (a *API) retryTask(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}

	tk, err := a.Tasks.Get(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "任务不存在"})
		return
	}

	if _, err := a.Tasks.Transition(r.Context(), id, task.StateQueued, actorOf(r), &task.TransitionOpts{
		Payload: map[string]any{"reason": "manual_retry"},
	}); err != nil {
		transitionError(w, err)
		return
	}

	// 状态已回到 queued，再排进执行队列
	if err := a.Queue.Enqueue(r.Context(), tk.LinearIssueKey, tk.LinearIssueKey); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "queued", "taskId": id})
}

func (a *API) cancelTask(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := a.Tasks.Transition(r.Context(), id, task.StateCancelled, actorOf(r), &task.TransitionOpts{
		Payload: map[string]any{"reason": "manual_cancel"},
	}); err != nil {
		transitionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "cancelled", "taskId": id})
}

func (a *API) updateRepo(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}

	var body struct {
		DefaultBranch     string   `json:"defaultBranch"`
		HotfixBase        string   `json:"hotfixBase"`
		ProtectedBranches []string `json:"protectedBranches"`
		BranchPattern     string   `json:"branchPattern"`
		GateMode          string   `json:"gateMode"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}

	if body.GateMode != "" {
		switch body.GateMode {
		case "direct", "guarded", "plan-first", "manual":
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "准入档位须为 direct / guarded / plan-first / manual 之一",
			})
			return
		}
	}
	// 受保护分支列表清空等于关掉最后一道闸门，必须拒绝
	if body.ProtectedBranches != nil && len(body.ProtectedBranches) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "受保护分支列表不能为空 —— 这是禁止直接推送的最后一道闸门",
		})
		return
	}

	repo, err := a.Store.UpdateRepo(r.Context(), id, store.UpdateRepoParams{
		DefaultBranch:     body.DefaultBranch,
		HotfixBase:        body.HotfixBase,
		ProtectedBranches: body.ProtectedBranches,
		BranchPattern:     body.BranchPattern,
		GateMode:          body.GateMode,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, repo)
}

// ---------------------------------------------------------------- 辅助

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "任务 ID 非法"})
		return 0, false
	}
	return id, true
}

// transitionError 把状态机错误映射成合适的 HTTP 状态码。
//
// 非法转移是 409 而非 400：请求本身没问题，是资源当前状态不允许。
func transitionError(w http.ResponseWriter, err error) {
	var illegal task.ErrIllegalTransition
	if errors.As(err, &illegal) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	if errors.Is(err, task.ErrTaskNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "任务不存在"})
		return
	}
	if errors.Is(err, task.ErrSessionRequired) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	serverError(w, "状态转移失败", err)
}

func serverError(w http.ResponseWriter, msg string, err error) {
	writeJSON(w, http.StatusInternalServerError, map[string]any{
		"error": fmt.Sprintf("%s: %v", msg, err),
	})
}

func decodeJSON(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil // 允许空体，各字段取零值
	}
	return json.Unmarshal(body, v)
}
