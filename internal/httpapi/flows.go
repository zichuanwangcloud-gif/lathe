package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Clouditera/lathe/internal/flow"
)

// FlowAPI 提供"一键批量入队"（PRD F1.4）的 HTTP 端点：一次请求建一整
// 张编排图（一批带依赖关系的任务）。
type FlowAPI struct {
	Flow *flow.Service
	Auth *Auth
}

// Routes 注册编排图接口。
func (f *FlowAPI) Routes(mux *http.ServeMux) {
	mux.Handle("POST /api/flows", f.Auth.RequireFunc(f.create))
	mux.Handle("GET /api/flows/{id}", f.Auth.RequireFunc(f.get))
}

// flowNodeRequest 是 POST /api/flows 请求体里的一个节点。
type flowNodeRequest struct {
	IssueKey       string          `json:"issueKey"`
	IssueID        string          `json:"issueId"`
	Title          string          `json:"title"`
	Priority       int             `json:"priority"`
	DependsOnIndex *int            `json:"dependsOnIndex"`
	DependsOnAt    string          `json:"dependsOnAt"`
	// Profile 是节点执行画像（F7.1），原样透传给 flow.NodeInput.Profile，
	// 不在建图时校验其内部结构——校验交给 pipeline 执行时的
	// runner.ParseProfile，读到非法画像时任务本身失败，不是建图时拒绝
	// （"建图"与"执行"两阶段职责分明，符合现有架构风格）。
	Profile json.RawMessage `json:"profile"`
}

// create 一次请求建一整张编排图：POST /api/flows
// body: { name, repoId, nodes: [{issueKey, issueId, title, priority,
//
//	dependsOnIndex, dependsOnAt, profile}, ...] }（nodes 必须按拓扑序
//
// 提交，dependsOnIndex 只能指向本批次里更早的节点；profile 是 F7.1
// 节点执行画像，原样透传不校验，见 flowNodeRequest.Profile 注释）。
//
// 成功返回 flowId、每个创建任务的精简视图，以及 warnings（F3.3-AC2）：
// 链长超过系统设置 flow_max_chain_length（未配置默认 4）的节点在这里
// 原样透传，不阻止创建——F3.3-AC1 的"UI 给出警告"是 UI 职责，这次没有
// UI，能做到的最大程度就是让这份 warnings 出现在响应体里，供未来 M5
// 的画布 UI 消费；失败（校验错误 / 上限超出 / 幂等冲突）返回清楚的
// 4xx + error 字段，不 500。
func (f *FlowAPI) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string            `json:"name"`
		RepoID int64             `json:"repoId"`
		Nodes  []flowNodeRequest `json:"nodes"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.RepoID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "repoId 非法"})
		return
	}
	if len(body.Nodes) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "nodes 不能为空"})
		return
	}

	nodes := make([]flow.NodeInput, len(body.Nodes))
	for i, n := range body.Nodes {
		nodes[i] = flow.NodeInput{
			IssueKey:       strings.TrimSpace(n.IssueKey),
			IssueID:        strings.TrimSpace(n.IssueID),
			Title:          n.Title,
			Priority:       n.Priority,
			DependsOnIndex: n.DependsOnIndex,
			DependsOnAt:    n.DependsOnAt,
			Profile:        n.Profile,
		}
	}

	flowID, created, warnings, err := f.Flow.CreateFlow(r.Context(), CurrentUser(r).ID, body.RepoID, body.Name, nodes)
	if err != nil {
		writeJSON(w, flowErrorStatus(err), map[string]any{"error": err.Error()})
		return
	}

	tasks := make([]map[string]any, len(created))
	for i, t := range created {
		tasks[i] = map[string]any{
			"id":       t.ID,
			"issueKey": t.LinearIssueKey,
			"state":    string(t.State),
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"flowId": flowID, "tasks": tasks, "warnings": warnings})
}

// get 查询一个 flow 的信息与其下全部任务的当前状态：GET /api/flows/{id}
//
// 这是本包里"查询一个 flow 下所有任务当前状态"能力的落点（供集成测试
// 断言用），没有另外复用 GET /api/tasks + flowId 过滤参数 —— 独立端点
// 一次往返就能拿到 flow 元信息 + 全部任务，不用先查 flow 详情再拼
// query string。
func (f *FlowAPI) get(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "flow ID 非法"})
		return
	}

	fs, err := f.Flow.GetFlow(r.Context(), CurrentUser(r).ID, id)
	if err != nil {
		if errors.Is(err, flow.ErrFlowNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "编排图不存在"})
			return
		}
		serverError(w, "查询编排图失败", err)
		return
	}

	tasks := make([]map[string]any, len(fs.Tasks))
	for i, t := range fs.Tasks {
		tasks[i] = map[string]any{
			"id":        t.ID,
			"issueKey":  t.IssueKey,
			"state":     t.State,
			"dependsOn": t.DependsOn,
			"priority":  t.Priority,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     fs.ID,
		"repoId": fs.RepoID,
		"name":   fs.Name,
		"tasks":  tasks,
	})
}

// flowErrorStatus 把 flow 包的错误映射成合适的 HTTP 状态码。
//
// 校验错误（非法下标/超出上限/字段缺失）与幂等冲突（issue 已占用）都是
// "请求本身有问题"，一律 400——不像 task 状态机的非法转移那样是
// "资源当前状态不允许"（那才该用 409）。
func flowErrorStatus(err error) int {
	var invalid flow.ErrInvalidIndex
	var tooMany flow.ErrTooMany
	var issueActive flow.ErrIssueActive
	switch {
	case errors.As(err, &invalid), errors.As(err, &tooMany), errors.As(err, &issueActive):
		return http.StatusBadRequest
	case errors.Is(err, flow.ErrEmpty):
		return http.StatusBadRequest
	default:
		return http.StatusBadRequest
	}
}
