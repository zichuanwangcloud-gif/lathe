package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"

	"github.com/Clouditera/lathe/internal/preview"
	"github.com/Clouditera/lathe/internal/store"
)

// PreviewManager 是预览环境的生命周期能力。
// *preview.Manager 实现此接口；测试可注入假件。
type PreviewManager interface {
	CheckResources(ctx context.Context) (*preview.ResourceStatus, error)
	Start(ctx context.Context, taskID int64, worktree string, sels []preview.Selection) error
	Status(ctx context.Context, taskID int64) (*preview.Status, error)
	Stop(ctx context.Context, taskID int64) (containers, images int, err error)
}

// PreviewAPI 提供任务预览环境接口：发现 Dockerfile、一键启动、
// 状态查询、停止清理。全部要求登录且只能操作自己名下的任务
// （与任务读接口同一隔离边界）。
type PreviewAPI struct {
	Store    *store.Store
	Auth     *Auth
	Previews PreviewManager
}

// Routes 注册预览接口。
func (a *PreviewAPI) Routes(mux *http.ServeMux) {
	mux.Handle("GET /api/tasks/{id}/preview/candidates", a.Auth.RequireFunc(a.candidates))
	mux.Handle("GET /api/tasks/{id}/preview", a.Auth.RequireFunc(a.status))
	mux.Handle("POST /api/tasks/{id}/preview/start", a.Auth.RequireFunc(a.start))
	mux.Handle("POST /api/tasks/{id}/preview/stop", a.Auth.RequireFunc(a.stop))
}

// taskWorktree 解析任务并取出 worktree 路径。
// 任务不存在/不属于当前用户 → 404；worktree 未建或已回收 → 409。
func (a *PreviewAPI) taskWorktree(w http.ResponseWriter, r *http.Request) (int64, string, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return 0, "", false
	}
	detail, err := a.Store.TaskDetail(r.Context(), id, CurrentUser(r).ID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "任务不存在"})
		return 0, "", false
	}
	wt := ""
	if detail.Task.WorktreePath != nil {
		wt = *detail.Task.WorktreePath
	}
	if wt == "" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "任务尚未创建工作区（还在排队或分诊）"})
		return 0, "", false
	}
	if _, err := os.Stat(wt); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "工作区已回收（任务合并后现场即释放），无法预览"})
		return 0, "", false
	}
	return id, wt, true
}

func (a *PreviewAPI) candidates(w http.ResponseWriter, r *http.Request) {
	_, wt, ok := a.taskWorktree(w, r)
	if !ok {
		return
	}
	found, err := preview.Discover(wt)
	if err != nil {
		serverError(w, "扫描 Dockerfile 失败", err)
		return
	}
	rs, err := a.Previews.CheckResources(r.Context())
	if err != nil {
		serverError(w, "测量资源水位失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"candidates": found, "resources": rs,
	})
}

func (a *PreviewAPI) status(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	// 状态查询不要求 worktree 在场：容器可能还活着而现场已回收。
	if _, err := a.Store.TaskDetail(r.Context(), id, CurrentUser(r).ID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "任务不存在"})
		return
	}
	st, err := a.Previews.Status(r.Context(), id)
	if err != nil {
		serverError(w, "查询预览状态失败", err)
		return
	}
	rs, _ := a.Previews.CheckResources(r.Context()) // 水位是附带信息，失败不挡主路
	writeJSON(w, http.StatusOK, map[string]any{
		"op": st.Op, "containers": st.Containers, "resources": rs,
	})
}

func (a *PreviewAPI) start(w http.ResponseWriter, r *http.Request) {
	id, wt, ok := a.taskWorktree(w, r)
	if !ok {
		return
	}
	var body struct {
		Selections []preview.Selection `json:"selections"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxJSONBody)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是合法 JSON"})
		return
	}
	if len(body.Selections) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "至少选择一个镜像"})
		return
	}
	if err := a.Previews.Start(r.Context(), id, wt, body.Selections); err != nil {
		switch {
		case errors.Is(err, preview.ErrOverThreshold), errors.Is(err, preview.ErrBuildInProgress):
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		case errors.Is(err, preview.ErrDockerUnavailable):
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"state": "building"})
}

func (a *PreviewAPI) stop(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if _, err := a.Store.TaskDetail(r.Context(), id, CurrentUser(r).ID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "任务不存在"})
		return
	}
	containers, images, err := a.Previews.Stop(r.Context(), id)
	if err != nil {
		serverError(w, "停止预览失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stoppedContainers": containers, "removedImages": images})
}
