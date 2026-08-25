package preview

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDocker 记录所有调用并按子命令返回预设输出。
type fakeDocker struct {
	mu    sync.Mutex
	calls [][]string
	// 按首参数（子命令）定制输出；未匹配的返回空成功。
	outputs map[string]fakeResult
	// stream 自定义构建行为；nil 时默认喂两行进度并返回成功。
	stream func(ctx context.Context, args []string, onLine func(string)) (string, error)
}

type fakeResult struct {
	stdout string
	stderr string
	err    error
}

func (f *fakeDocker) run(ctx context.Context, name string, args ...string) (string, string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, args...))
	f.mu.Unlock()
	if r, ok := f.outputs[args[0]]; ok {
		return r.stdout, r.stderr, r.err
	}
	return "", "", nil
}

func (f *fakeDocker) runStream(ctx context.Context, name string, args []string, onLine func(string)) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, args...))
	f.mu.Unlock()
	if f.stream != nil {
		return f.stream(ctx, args, onLine)
	}
	onLine("#1 [internal] load build definition")
	onLine("#5 [5/5] RUN make")
	return "#5 [5/5] RUN make\n", nil
}

func (f *fakeDocker) has(sub ...string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		joined := strings.Join(c, " ")
		ok := true
		for _, s := range sub {
			if !strings.Contains(joined, s) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func newTestManager(t *testing.T, fd *fakeDocker, memTh, diskTh int) (*Manager, string) {
	t.Helper()
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "Dockerfile"), []byte("FROM alpine\nEXPOSE 3000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		DockerBin:     "docker",
		WorkspaceRoot: wt,
		Thresholds:    func(context.Context) (int, int, error) { return memTh, diskTh, nil },
		exec:          fd.run,
		execStream:    fd.runStream,
		ops:           map[int64]*Op{},
	}
	return m, wt
}

// waitOpGone 等异步构建收尾（成功删除 op 或转 failed）。
func waitOp(t *testing.T, m *Manager, taskID int64) *Op {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		op, ok := m.ops[taskID]
		m.mu.Unlock()
		if !ok {
			return nil // 成功收尾
		}
		if op.State == "failed" {
			return op
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("构建操作 5 秒内未收尾")
	return nil
}

func TestStartRefusedOverThreshold(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{}}
	m, wt := newTestManager(t, fd, 1, 100) // 内存阈值 1%：任何真实水位都超

	err := m.Start(context.Background(), 7, wt, StartRequest{Selections: []Selection{{Path: "Dockerfile", Ports: []int{3000}}}})
	if !errors.Is(err, ErrOverThreshold) {
		t.Fatalf("应被资源闸门拦下，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "内存") {
		t.Errorf("原因应点名内存: %v", err)
	}
}

func TestStartValidation(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{}}
	m, wt := newTestManager(t, fd, 100, 100)

	cases := []struct {
		name string
		sels []Selection
		want string
	}{
		{"空选择", nil, "至少选择"},
		{"非法端口", []Selection{{Path: "Dockerfile", Ports: []int{0}}}, "端口 0 非法"},
		{"Dockerfile 不存在", []Selection{{Path: "apps/x/Dockerfile"}}, "不存在"},
		{"路径穿越被中和", []Selection{{Path: "../../../etc/passwd"}}, "不存在"},
		{"撞镜像名", []Selection{{Path: "Dockerfile"}, {Path: "./Dockerfile"}}, "撞了名"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := m.Start(context.Background(), 7, wt, StartRequest{Selections: tc.sels})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误 = %v，期望含 %q", err, tc.want)
			}
		})
	}
}

func TestStartBuildsAndRuns(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{}}
	m, wt := newTestManager(t, fd, 100, 100)

	if err := m.Start(context.Background(), 7, wt, StartRequest{Selections: []Selection{{Path: "Dockerfile", Ports: []int{3000, 8080}}}}); err != nil {
		t.Fatalf("Start 报错: %v", err)
	}
	if op := waitOp(t, m, 7); op != nil {
		t.Fatalf("构建应成功，得到 failed: %s", op.Error)
	}

	// 构建：标签（跨重启归属的事实源）、镜像名、Dockerfile 与上下文
	if !fd.has("build", "--label", "lathe.task=7", "-t", "lathe-preview-t7-root", "-f", filepath.Join(wt, "Dockerfile"), wt) {
		t.Errorf("构建命令不符: %v", fd.calls)
	}
	// 运行：随机宿主机端口映射两个容器端口
	if !fd.has("run", "-d", "--name", "lathe-preview-t7-root", "-p", "0:3000", "-p", "0:8080") {
		t.Errorf("运行命令不符: %v", fd.calls)
	}
}

func TestStartBuildFailureKeepsOp(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{}}
	fd.stream = func(ctx context.Context, args []string, onLine func(string)) (string, error) {
		onLine("#9 [3/7] RUN npm ci")
		return "#9 [3/7] RUN npm ci\nstep 3/7 failed: npm ci error\n", errors.New("exit 1")
	}
	m, wt := newTestManager(t, fd, 100, 100)

	if err := m.Start(context.Background(), 7, wt, StartRequest{Selections: []Selection{{Path: "Dockerfile", Ports: []int{3000}}}}); err != nil {
		t.Fatalf("Start 报错: %v", err)
	}
	op := waitOp(t, m, 7)
	if op == nil || op.State != "failed" {
		t.Fatalf("构建失败应留下 failed op，得到 %+v", op)
	}
	if !strings.Contains(op.Error, "npm ci error") {
		t.Errorf("失败原因应带构建输出尾部: %s", op.Error)
	}
	if fd.has("run", "-d") {
		t.Error("构建失败不应启动容器")
	}

	// 构建失败后再来一次应允许（op 不是 building 态）
	fd.stream = nil
	if err := m.Start(context.Background(), 7, wt, StartRequest{Selections: []Selection{{Path: "Dockerfile", Ports: []int{3000}}}}); err != nil {
		t.Fatalf("失败后应可重新启动，得到 %v", err)
	}
	waitOp(t, m, 7)
}

func TestStartRejectsConcurrentBuild(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	fd := &fakeDocker{outputs: map[string]fakeResult{}}
	// build 卡住，模拟分钟级构建
	fd.stream = func(ctx context.Context, args []string, onLine func(string)) (string, error) {
		onLine("#1 load definition")
		close(started)
		select {
		case <-release:
			return "", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	m, wt := newTestManager(t, fd, 100, 100)

	if err := m.Start(context.Background(), 7, wt, StartRequest{Selections: []Selection{{Path: "Dockerfile"}}}); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := m.Start(context.Background(), 7, wt, StartRequest{Selections: []Selection{{Path: "Dockerfile"}}}); !errors.Is(err, ErrBuildInProgress) {
		t.Fatalf("并发构建应报 ErrBuildInProgress，得到 %v", err)
	}
	close(release)
	waitOp(t, m, 7)
}

func TestStopRemovesContainersAndImages(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{
		"ps":    {stdout: "c1\nc2\n"},
		"image": {stdout: "i1\ni2\n"},
	}}
	m, _ := newTestManager(t, fd, 100, 100)

	containers, images, err := m.Stop(context.Background(), 7)
	if err != nil {
		t.Fatalf("Stop 报错: %v", err)
	}
	if containers != 2 || images != 2 {
		t.Errorf("清理计数 = (%d,%d)，期望 (2,2)", containers, images)
	}
	if !fd.has("rm", "-f", "c1", "c2") {
		t.Errorf("应强删两个容器: %v", fd.calls)
	}
	if !fd.has("rmi", "i1", "i2") {
		t.Errorf("应删掉两个镜像: %v", fd.calls)
	}
}

func TestStatusMergesOpAndContainers(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{
		"ps": {stdout: "abc123\n"},
		"container": {stdout: `[{
			"Name": "/lathe-preview-t7-root",
			"State": {"Status": "running"},
			"Config": {"Image": "lathe-preview-t7-root"},
			"NetworkSettings": {"Ports": {"3000/tcp": [{"HostIp": "0.0.0.0", "HostPort": "32771"}]}}
		}]`},
	}}
	m, _ := newTestManager(t, fd, 100, 100)

	st, err := m.Status(context.Background(), 7)
	if err != nil {
		t.Fatalf("Status 报错: %v", err)
	}
	if len(st.Containers) != 1 {
		t.Fatalf("应有 1 个容器: %+v", st.Containers)
	}
	c := st.Containers[0]
	if c.Name != "lathe-preview-t7-root" || c.State != "running" {
		t.Errorf("容器状态解析不符: %+v", c)
	}
	if len(c.Ports) != 1 || c.Ports[0] != (PortMapping{Container: 3000, Host: 32771}) {
		t.Errorf("端口映射解析不符: %+v", c.Ports)
	}
}

func TestCheckResourcesGate(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{}}

	m, _ := newTestManager(t, fd, 100, 100)
	rs, err := m.CheckResources(context.Background())
	if err != nil {
		t.Fatalf("CheckResources 报错: %v", err)
	}
	if !rs.Allowed || !rs.DockerOK {
		t.Errorf("阈值 100 且 docker 可用时应放行: %+v", rs)
	}

	m2, _ := newTestManager(t, fd, 1, 100)
	rs2, _ := m2.CheckResources(context.Background())
	if rs2.Allowed || !strings.Contains(rs2.Reason, "内存") {
		t.Errorf("内存阈值 1%% 应拦下: %+v", rs2)
	}

	// docker 不可用 → 一律不放行
	fdDown := &fakeDocker{outputs: map[string]fakeResult{
		"version": {err: fmt.Errorf("connection refused")},
	}}
	m3, _ := newTestManager(t, fdDown, 100, 100)
	rs3, _ := m3.CheckResources(context.Background())
	if rs3.Allowed || rs3.DockerOK {
		t.Errorf("docker 不可用应拦下: %+v", rs3)
	}
}

// 构建进度必须实时可见：op.Progress 随构建输出更新 —— 分钟级黑盒
// 里「在编译」与「卡死了」的唯一区分手段。
func TestBuildProgressVisibleMidFlight(t *testing.T) {
	inStep := make(chan struct{})
	release := make(chan struct{})
	fd := &fakeDocker{outputs: map[string]fakeResult{}}
	fd.stream = func(ctx context.Context, args []string, onLine func(string)) (string, error) {
		onLine("#31 [12/15] RUN pnpm build")
		close(inStep)
		<-release
		return "", nil
	}
	m, wt := newTestManager(t, fd, 100, 100)

	if err := m.Start(context.Background(), 7, wt, StartRequest{Selections: []Selection{{Path: "Dockerfile"}}}); err != nil {
		t.Fatal(err)
	}
	<-inStep

	st, err := m.Status(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if st.Op == nil || st.Op.State != "building" {
		t.Fatalf("构建中应有 building op: %+v", st.Op)
	}
	if !strings.Contains(st.Op.Progress, "RUN pnpm build") {
		t.Errorf("进度应含当前步骤，得到 %q", st.Op.Progress)
	}
	close(release)
	waitOp(t, m, 7)
}

// 停止必须取消进行中的构建（context 取消传导到构建进程）。
func TestStopCancelsBuild(t *testing.T) {
	started := make(chan struct{})
	fd := &fakeDocker{outputs: map[string]fakeResult{}}
	fd.stream = func(ctx context.Context, args []string, onLine func(string)) (string, error) {
		close(started)
		<-ctx.Done() // 等取消
		return "", ctx.Err()
	}
	m, wt := newTestManager(t, fd, 100, 100)

	if err := m.Start(context.Background(), 7, wt, StartRequest{Selections: []Selection{{Path: "Dockerfile"}}}); err != nil {
		t.Fatal(err)
	}
	<-started

	if _, _, err := m.Stop(context.Background(), 7); err != nil {
		t.Fatalf("Stop 报错: %v", err)
	}
	// 构建被取消后 op 不复活为 failed（人主动放弃不是失败）
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		_, ok := m.ops[7]
		m.mu.Unlock()
		if !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("Stop 后 op 应被清除且不复活")
}

// 所有 --filter 参数必须是合法过滤器（key=value，key 属于 docker 支持的
// 过滤器集合）。回归：label 过滤曾漏写 label= 前缀，docker 报
// "invalid filter" 而 Status 吞错，表现为容器凭空消失。
func TestListFiltersWellFormed(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{
		"ps":        {stdout: "c1\n"},
		"container": {stdout: `[{"Name":"/c1","State":{"Status":"running"},"Config":{"Image":"img"},"NetworkSettings":{"Ports":{}}}]`},
	}}
	m, _ := newTestManager(t, fd, 100, 100)

	if _, err := m.Status(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Stop(context.Background(), 7); err != nil {
		t.Fatal(err)
	}

	validKeys := map[string]bool{"label": true, "status": true}
	for _, call := range fd.calls {
		for i, arg := range call {
			if arg != "--filter" || i+1 >= len(call) {
				continue
			}
			kv := strings.SplitN(call[i+1], "=", 2)
			if len(kv) != 2 || !validKeys[kv[0]] {
				t.Errorf("非法过滤器 %q（调用 %v）", call[i+1], call)
			}
		}
	}
}
