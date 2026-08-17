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

	// 读。全部按 CurrentUser 隔离（P1.5 第二步）：任何人只能看到
	// 自己名下的任务与仓库配置，管理员也不例外 —— 跨用户读取的
	// 后门宁可没有，排障走数据库。
	mux.Handle("GET /api/tasks", a.Auth.RequireFunc(a.listTasks))
	mux.Handle("GET /api/tasks/{id}", a.Auth.RequireFunc(a.taskDetail))
	mux.Handle("GET /api/tasks/{id}/events", a.Auth.RequireFunc(a.taskEvents))
	mux.Handle("GET /api/stats", a.Auth.RequireFunc(a.stats))
	mux.Handle("GET /api/repos", a.Auth.RequireFunc(a.listRepos))
	mux.Handle("GET /api/config", a.Auth.RequireFunc(a.config))

	// 写
	mux.Handle("POST /api/tasks", a.Auth.RequireFunc(a.triggerTask))
	mux.Handle("POST /api/tasks/{id}/retry", a.Auth.RequireFunc(a.retryTask))
	mux.Handle("POST /api/tasks/{id}/cancel", a.Auth.RequireFunc(a.cancelTask))
	mux.Handle("POST /api/repos", a.Auth.RequireFunc(a.createRepo))
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
		UserID: CurrentUser(r).ID, States: states, Limit: limit, Offset: offset,
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
	detail, err := a.Store.TaskDetail(r.Context(), id, CurrentUser(r).ID)
	if err != nil {
		// 别人的任务与不存在同等处理：对非属主隐瞒存在
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "任务不存在"})
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// taskEvents 增量拉取 agent 执行事件（docs/04 §3.3）。
//
// 轮询协议：after 传上次的 last_id（首轮传 0），游标 id 严格单调，
// 将来换 SSE + Last-Event-ID 时客户端协议不变，只是传输升级。
func (a *API) taskEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}

	// 权限与 taskDetail 同一原则：不是自己的任务按 404 处理
	tk, err := a.Tasks.Get(r.Context(), id)
	if err != nil || tk.UserID != CurrentUser(r).ID {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "任务不存在"})
		return
	}

	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))

	events, lastID, err := a.Store.AgentEventsAfter(r.Context(), id, tk.UserID, after, limit)
	if err != nil {
		serverError(w, "查询执行日志失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "last_id": lastID})
}

func (a *API) stats(w http.ResponseWriter, r *http.Request) {
	st, err := a.Store.Stats(r.Context(), CurrentUser(r).ID)
	if err != nil {
		serverError(w, "统计失败", err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (a *API) listRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := a.Store.ListRepos(r.Context(), CurrentUser(r).ID)
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

	if err := a.Queue.Enqueue(r.Context(), CurrentUser(r).ID, body.IssueID, body.IssueKey); err != nil {
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
	if err != nil || tk.UserID != CurrentUser(r).ID {
		// 不是自己的任务 = 不存在（与 taskDetail 同一原则）
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "任务不存在"})
		return
	}

	if _, err := a.Tasks.Transition(r.Context(), id, task.StateQueued, actorOf(r), &task.TransitionOpts{
		Payload: map[string]any{"reason": "manual_retry"},
	}); err != nil {
		transitionError(w, err)
		return
	}

	// 状态已回到 queued，再排进执行队列；归属仍是原属主
	if err := a.Queue.Enqueue(r.Context(), tk.UserID, tk.LinearIssueKey, tk.LinearIssueKey); err != nil {
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
	tk, err := a.Tasks.Get(r.Context(), id)
	if err != nil || tk.UserID != CurrentUser(r).ID {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "任务不存在"})
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

// createRepo 登记一个仓库配置到当前用户名下。
//
// 数据隔离之后这是新用户的必经入口：没有人能替你把仓库配到你名下，
// 缺了它新用户永远等不到「管理员手工 INSERT」之外的第二条路。
func (a *API) createRepo(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProviderRepo  string `json:"providerRepo"`
		DefaultBranch string `json:"defaultBranch"`
		HotfixBase    string `json:"hotfixBase"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	body.ProviderRepo = strings.TrimSpace(body.ProviderRepo)
	if body.ProviderRepo == "" || !strings.Contains(body.ProviderRepo, "/") {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "providerRepo 须为 owner/repo 形式，如 Clouditera/CloudRouter",
		})
		return
	}

	repo, err := a.Store.CreateRepo(r.Context(), CurrentUser(r).ID, store.CreateRepoParams{
		ProviderRepo:  body.ProviderRepo,
		DefaultBranch: strings.TrimSpace(body.DefaultBranch),
		HotfixBase:    strings.TrimSpace(body.HotfixBase),
	})
	if err != nil {
		if errors.Is(err, store.ErrRepoExists) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
			return
		}
		serverError(w, "创建仓库配置失败", err)
		return
	}
	writeJSON(w, http.StatusCreated, repo)
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
		// ExcludeDirs：nil（未传）= 不动；空数组 = 清回默认排除；
		// 非空 = 整体替换。JSON 数组天然区分这三种语义，无需指针。
		ExcludeDirs []string `json:"excludeDirs"`
		// 指针区分「未传」（不动）与「空串」（清回自动档）
		VerifyTierOverride *string `json:"verifyTierOverride"`
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
	if body.VerifyTierOverride != nil {
		switch *body.VerifyTierOverride {
		case "", "light", "heavy":
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "验证档位须为 light / heavy，或留空表示按改动面自动判定",
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

	repo, err := a.Store.UpdateRepo(r.Context(), id, CurrentUser(r).ID, store.UpdateRepoParams{
		DefaultBranch:      body.DefaultBranch,
		HotfixBase:         body.HotfixBase,
		ProtectedBranches:  body.ProtectedBranches,
		BranchPattern:      body.BranchPattern,
		GateMode:           body.GateMode,
		ExcludeDirs:        body.ExcludeDirs,
		VerifyTierOverride: body.VerifyTierOverride,
	})
	if err != nil {
		if errors.Is(err, store.ErrRepoNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "仓库不存在"})
			return
		}
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
