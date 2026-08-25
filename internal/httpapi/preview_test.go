package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Clouditera/lathe/internal/preview"
	"github.com/Clouditera/lathe/internal/store"
	"github.com/Clouditera/lathe/internal/task"
)

// fakePreviews 记录调用并返回预设结果。
type fakePreviews struct {
	startErr error
	started  []preview.Selection
	stopped  int
}

func (f *fakePreviews) CheckResources(ctx context.Context) (*preview.ResourceStatus, error) {
	return &preview.ResourceStatus{MemUsedPct: 40, DiskUsedPct: 50, MemThreshold: 90, DiskThreshold: 90, DockerOK: true, Allowed: true}, nil
}
func (f *fakePreviews) Start(ctx context.Context, taskID int64, worktree string, sels []preview.Selection) error {
	if f.startErr != nil {
		return f.startErr
	}
	f.started = sels
	return nil
}
func (f *fakePreviews) Status(ctx context.Context, taskID int64) (*preview.Status, error) {
	return &preview.Status{Containers: []preview.Container{{Name: "c1", State: "running"}}}, nil
}
func (f *fakePreviews) Stop(ctx context.Context, taskID int64) (int, int, error) {
	f.stopped++
	return 1, 1, nil
}

// previewFixture 建用户/仓库/任务（带真实 worktree 目录与 Dockerfile），
// 返回可打的测试服务器。
func previewFixture(t *testing.T, fp *fakePreviews) (*httptest.Server, *store.Store, *task.Machine, int64, int64) {
	t.Helper()
	st := testStoreForAPI(t)
	userID := mustUser(t, st, "preview-"+t.Name()+"@example.com")

	var repoID int64
	if err := st.Pool().QueryRow(context.Background(),
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		userID, "acme/preview-"+t.Name()).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	m := task.NewMachine(st.Pool())

	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "Dockerfile"), []byte("FROM alpine\nEXPOSE 3000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tk, err := m.Create(context.Background(), task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "CR-PV-" + t.Name(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Transition(context.Background(), tk.ID, task.StateTriaging, "test", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Transition(context.Background(), tk.ID, task.StateImplementing, "test", &task.TransitionOpts{
		WorktreePath: &wt,
	}); err != nil {
		t.Fatal(err)
	}

	api := &PreviewAPI{Store: st, Auth: authAs(userID, "pv@example.com"), Previews: fp}
	mux := http.NewServeMux()
	api.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, st, m, userID, tk.ID
}

func TestPreviewCandidatesAndLifecycle(t *testing.T) {
	fp := &fakePreviews{}
	srv, _, _, _, taskID := previewFixture(t, fp)
	base := "/api/tasks/" + itoa(taskID)

	// 候选发现：应找到 worktree 里的 Dockerfile 与 EXPOSE 端口
	resp := do(t, srv, "GET", base+"/preview/candidates", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("candidates 应 200，得到 %d", resp.StatusCode)
	}
	body := decode(t, resp)
	cands, _ := body["candidates"].([]any)
	if len(cands) != 1 {
		t.Fatalf("应发现 1 个 Dockerfile: %v", body)
	}
	c0 := cands[0].(map[string]any)
	if c0["path"] != "Dockerfile" {
		t.Errorf("候选路径不符: %v", c0)
	}
	if ports, _ := c0["ports"].([]any); len(ports) != 1 || ports[0] != float64(3000) {
		t.Errorf("EXPOSE 端口应解析出 3000: %v", c0["ports"])
	}
	if rs := body["resources"].(map[string]any); rs["allowed"] != true {
		t.Errorf("资源水位应放行: %v", rs)
	}

	// 启动：202，选择透传给 Manager
	resp = do(t, srv, "POST", base+"/preview/start",
		`{"selections":[{"path":"Dockerfile","ports":[3000]}]}`, true)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start 应 202，得到 %d: %v", resp.StatusCode, decode(t, resp))
	}
	if len(fp.started) != 1 || fp.started[0].Path != "Dockerfile" || fp.started[0].Ports[0] != 3000 {
		t.Errorf("选择未透传: %+v", fp.started)
	}

	// 状态：容器列表
	resp = do(t, srv, "GET", base+"/preview/status", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status 应 200，得到 %d", resp.StatusCode)
	}
	if cs, _ := decode(t, resp)["containers"].([]any); len(cs) != 1 {
		t.Errorf("应有 1 个容器: %v", decode(t, resp))
	}

	// 停止：返回清理计数
	resp = do(t, srv, "POST", base+"/preview/stop", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stop 应 200，得到 %d", resp.StatusCode)
	}
	if fp.stopped != 1 {
		t.Error("Stop 未被调用")
	}
}

func TestPreviewStartOverThreshold409(t *testing.T) {
	fp := &fakePreviews{startErr: errors.New("preview: 资源占用超过阈值: 内存占用 95% 已达阈值 90%: " + preview.ErrOverThreshold.Error())}
	// 用 errors.Is 可识别的包装
	fp.startErr = preview.ErrOverThreshold
	srv, _, _, _, taskID := previewFixture(t, fp)

	resp := do(t, srv, "POST", "/api/tasks/"+itoa(taskID)+"/preview/start",
		`{"selections":[{"path":"Dockerfile","ports":[3000]}]}`, true)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("资源超限应 409，得到 %d", resp.StatusCode)
	}
}

func TestPreviewWorktreeMissing409(t *testing.T) {
	fp := &fakePreviews{}
	srv, st, m, userID, _ := previewFixture(t, fp)

	// 另建一个还在 queued 的任务（无 worktree）
	var repoID int64
	_ = st.Pool().QueryRow(context.Background(), `SELECT id FROM repos WHERE user_id=$1`, userID).Scan(&repoID)
	tk, err := m.Create(context.Background(), task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "CR-PV2-" + t.Name(),
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := do(t, srv, "GET", "/api/tasks/"+itoa(tk.ID)+"/preview/candidates", "", true)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("无工作区应 409，得到 %d", resp.StatusCode)
	}
}

func TestPreviewCrossUserIs404(t *testing.T) {
	fp := &fakePreviews{}
	_, st, _, _, taskID := previewFixture(t, fp)

	// 另一个用户的视角：任务不可见
	other := mustUser(t, st, "preview-other-"+t.Name()+"@example.com")
	api := &PreviewAPI{Store: st, Auth: authAs(other, "other@example.com"), Previews: fp}
	mux := http.NewServeMux()
	api.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for _, path := range []string{"/preview/status", "/preview/candidates"} {
		resp := do(t, srv, "GET", "/api/tasks/"+itoa(taskID)+path, "", true)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("跨用户 %s 应 404，得到 %d", path, resp.StatusCode)
		}
	}
}
