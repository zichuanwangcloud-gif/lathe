package preview

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 预览资源一律打这两个标签：容器与镜像的发现、清理、跨重启归属
// 全靠它们，不依赖任何数据库状态 —— serve 重启后现场仍在。
const (
	labelPreview = "lathe.preview"
	labelTask    = "lathe.task"
)

// buildTimeout 是单个镜像的构建上限：冷构建（无缓存装依赖）十分钟
// 量级很正常，给 20 分钟；超时杀进程，避免一个写坏的 Dockerfile
// 永久占住构建位。
const buildTimeout = 20 * time.Minute

var (
	// ErrOverThreshold 表示资源水位超过阈值，启动被闸门拦下。
	ErrOverThreshold = errors.New("preview: 资源占用超过阈值")
	// ErrBuildInProgress 表示该任务已有一次构建在进行中。
	ErrBuildInProgress = errors.New("preview: 该任务已有构建进行中")
	// ErrDockerUnavailable 表示 docker 守护进程够不着。
	ErrDockerUnavailable = errors.New("preview: docker 不可用")
)

// Selection 是人工选定要启动的一个镜像。
type Selection struct {
	// Path 是 Dockerfile 相对 worktree 根的路径（来自 Discover）。
	Path string `json:"path"`
	// Ports 是要映射到宿主机的容器端口。宿主机端口由 docker 随机
	// 分配（-p 0:<port>），避免多任务预览撞端口。
	Ports []int `json:"ports"`
}

// buildPlan 是一个选中镜像的构建与启动参数。
type buildPlan struct {
	sel    Selection
	absDF  string
	absCtx string
	image  string
	cname  string
}

// Op 是一次进行中的构建操作的状态（内存态；成功完成后移除，
// 之后容器自身即状态；失败保留到下次启动或停止，供界面展示原因）。
type Op struct {
	State string `json:"state"` // building / failed
	Error string `json:"error,omitempty"`
	// Progress 是构建输出的尾部（实时更新）。构建是分钟级黑盒，
	// 没有进度人无法分辨「在编译」还是「卡死了」—— 2026-08-25 一次
	// 健康的构建就因 CLI 进程静默被误判卡死而遭误杀。
	Progress  string    `json:"progress,omitempty"`
	StartedAt time.Time `json:"startedAt"`

	cancel context.CancelFunc // 停止按钮取消构建用；不序列化
}

// PortMapping 是一个容器端口到宿主机端口的映射。
type PortMapping struct {
	Container int `json:"container"`
	Host      int `json:"host"`
}

// Container 是一个预览容器的运行状态。
type Container struct {
	Name  string        `json:"name"`
	Image string        `json:"image"`
	State string        `json:"state"` // running / exited / ...
	Ports []PortMapping `json:"ports"`
}

// Status 是一个任务预览环境的完整现状。
type Status struct {
	Op         *Op         `json:"op,omitempty"`
	Containers []Container `json:"containers"`
}

// Manager 管理预览容器与镜像的生命周期。
type Manager struct {
	// DockerBin 是 docker CLI 路径，默认 "docker"。
	DockerBin string
	// WorkspaceRoot 用于磁盘水位测量（预览镜像与 worktree 同盘）。
	WorkspaceRoot string
	// Thresholds 现取（内存, 磁盘）百分比阈值 —— 系统设置里改完
	// 即刻生效，不用重启。
	Thresholds func(ctx context.Context) (mem, disk int, err error)

	// exec 执行外部命令，返回 stdout/stderr；测试注入假件。
	exec func(ctx context.Context, name string, args ...string) (string, string, error)
	// execStream 执行构建命令：逐行回调输出（docker build 的进度走
	// stderr），返回输出尾部供错误展示。测试注入假件。
	execStream func(ctx context.Context, name string, args []string, onLine func(string)) (string, error)

	mu  sync.Mutex
	ops map[int64]*Op
}

// NewManager 用真实 docker CLI 构造 Manager。
func NewManager(workspaceRoot string, thresholds func(context.Context) (int, int, error)) *Manager {
	return &Manager{
		DockerBin:     "docker",
		WorkspaceRoot: workspaceRoot,
		Thresholds:    thresholds,
		exec:          realExec,
		execStream:    realStreamExec,
		ops:           map[int64]*Op{},
	}
}

func realExec(ctx context.Context, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// realStreamExec 流式执行命令：stdout/stderr 都逐行回调（buildkit 的
// 进度在 stderr 上），同时维护一个尾部环形缓冲供错误展示。
func realStreamExec(ctx context.Context, name string, args []string, onLine func(string)) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}

	var tb tailBuf
	var wg sync.WaitGroup
	scan := func(r io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1<<20) // buildkit 单行可能很长
		for sc.Scan() {
			line := sc.Text()
			tb.Write(line)
			if onLine != nil {
				onLine(line)
			}
		}
	}
	wg.Add(2)
	go scan(stdout)
	go scan(stderr)

	err = cmd.Wait() // 等进程退出
	wg.Wait()        // 再等管道读完（Wait 之前读完管道是调用方责任）
	return tb.String(), err
}

// tailBuf 是并发安全的尾部缓冲：只留最后 cap 字节。
type tailBuf struct {
	mu  sync.Mutex
	buf string
}

func (t *tailBuf) Write(line string) {
	t.mu.Lock()
	t.buf = tail(t.buf+line+"\n", 64*1024)
	t.mu.Unlock()
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf
}

// CheckResources 测量当前水位并对照阈值，给出是否允许启动。
func (m *Manager) CheckResources(ctx context.Context) (*ResourceStatus, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil, fmt.Errorf("preview: 读取 meminfo 失败: %w", err)
	}
	defer f.Close()
	memPct, err := parseMeminfo(f)
	if err != nil {
		return nil, err
	}
	diskPct, err := diskUsedPct(m.WorkspaceRoot)
	if err != nil {
		return nil, err
	}

	memTh, diskTh := 90, 90
	if m.Thresholds != nil {
		if mm, dd, e := m.Thresholds(ctx); e == nil {
			memTh, diskTh = mm, dd
		}
	}

	st := &ResourceStatus{
		MemUsedPct: memPct, DiskUsedPct: diskPct,
		MemThreshold: memTh, DiskThreshold: diskTh,
		DockerOK: m.dockerOK(ctx),
		Allowed:  true,
	}
	switch {
	case !st.DockerOK:
		st.Allowed, st.Reason = false, "docker 守护进程不可用"
	case memPct >= memTh:
		st.Allowed = false
		st.Reason = fmt.Sprintf("内存占用 %d%% 已达阈值 %d%%", memPct, memTh)
	case diskPct >= diskTh:
		st.Allowed = false
		st.Reason = fmt.Sprintf("磁盘占用 %d%% 已达阈值 %d%%", diskPct, diskTh)
	}
	return st, nil
}

func (m *Manager) dockerOK(ctx context.Context) bool {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, _, err := m.exec(c, m.DockerBin, "version", "--format", "{{.Server.Version}}")
	return err == nil
}

// Start 校验后异步构建并启动选中的镜像。返回时构建已在后台进行，
// 用 Status 跟踪。资源超限或已有构建在进行时同步报错。
func (m *Manager) Start(ctx context.Context, taskID int64, worktree string, sels []Selection) error {
	if len(sels) == 0 {
		return errors.New("preview: 至少选择一个镜像")
	}
	rs, err := m.CheckResources(ctx)
	if err != nil {
		return err
	}
	if !rs.Allowed {
		return fmt.Errorf("%w: %s", ErrOverThreshold, rs.Reason)
	}

	// 校验选择合法：路径必须落在 worktree 内且真是文件，端口合法。
	var plans []buildPlan
	seen := map[string]bool{}
	for _, s := range sels {
		// Clean 加前导斜杠把 ../ 中和在根部，再去掉斜杠回到相对形态
		// （slugify 与 Join 都需要相对路径）。
		rel := strings.TrimPrefix(filepath.Clean("/"+s.Path), string(filepath.Separator))
		absDF := filepath.Join(worktree, rel)
		if _, err := os.Stat(absDF); err != nil {
			return fmt.Errorf("preview: Dockerfile %s 不存在: %w", s.Path, err)
		}
		for _, p := range s.Ports {
			if p <= 0 || p > 65535 {
				return fmt.Errorf("preview: 端口 %d 非法", p)
			}
		}
		slug := slugify(rel)
		if seen[slug] {
			return fmt.Errorf("preview: %s 与另一选择撞了镜像名，请只选一个", s.Path)
		}
		seen[slug] = true
		plans = append(plans, buildPlan{
			sel:    s,
			absDF:  absDF,
			absCtx: filepath.Join(worktree, filepath.Dir(rel)),
			image:  fmt.Sprintf("lathe-preview-t%d-%s", taskID, slug),
			cname:  fmt.Sprintf("lathe-preview-t%d-%s", taskID, slug),
		})
	}

	m.mu.Lock()
	if op, ok := m.ops[taskID]; ok && op.State == "building" {
		m.mu.Unlock()
		return ErrBuildInProgress
	}
	// 构建上下文可取消（停止按钮）且脱离请求生命周期；进程退出即取消，
	// 半成品由 Stop 清理。
	bctx, cancel := context.WithCancel(context.Background())
	op := &Op{State: "building", StartedAt: time.Now(), cancel: cancel}
	m.ops[taskID] = op
	m.mu.Unlock()

	go m.run(bctx, taskID, op, plans)
	return nil
}

// run 依次构建并启动所有选中的镜像。任一失败即终止并记录原因；
// 已起来的容器留给 Stop 统一清理（界面会展示它们）。
func (m *Manager) run(ctx context.Context, taskID int64, op *Op, plans []buildPlan) {
	fail := func(err error) {
		slog.Warn("预览构建失败", "task", taskID, "err", err)
		m.mu.Lock()
		defer m.mu.Unlock()
		// Stop 已把 op 摘走（人主动放弃）时不再复活失败态
		if cur, ok := m.ops[taskID]; ok && cur == op {
			op.State, op.Error = "failed", err.Error()
		}
	}

	for _, p := range plans {
		bctx, cancel := context.WithTimeout(ctx, buildTimeout)
		output, err := m.execStream(bctx, m.DockerBin, []string{"build",
			"--label", labelPreview + "=1", "--label", fmt.Sprintf("%s=%d", labelTask, taskID),
			"-t", p.image, "-f", p.absDF, p.absCtx,
		}, func(line string) {
			m.mu.Lock()
			op.Progress = tail(op.Progress+line+"\n", 4096)
			m.mu.Unlock()
		})
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return // 被人为停止：Stop 已清理状态
			}
			fail(fmt.Errorf("构建 %s 失败: %s", p.sel.Path, tail(output, 2000)))
			return
		}

		args := []string{"run", "-d", "--name", p.cname,
			"--label", labelPreview + "=1", "--label", fmt.Sprintf("%s=%d", labelTask, taskID)}
		for _, port := range p.sel.Ports {
			// 0 = 宿主机随机端口：多任务同时预览不能撞端口。
			// 绑 0.0.0.0 —— 用户从局域网其他机器访问（本机回环够不着）。
			args = append(args, "-p", fmt.Sprintf("0:%d", port))
		}
		args = append(args, p.image)
		if _, stderr, err := m.exec(ctx, m.DockerBin, args...); err != nil {
			fail(fmt.Errorf("启动 %s 失败: %s", p.image, tail(stderr, 500)))
			return
		}
	}

	m.mu.Lock()
	// Stop 可能已摘走 op（人主动停止后构建才跑完）：不干扰
	if cur, ok := m.ops[taskID]; ok && cur == op {
		delete(m.ops, taskID) // 成功：容器自身即状态
	}
	m.mu.Unlock()
	slog.Info("预览环境已启动", "task", taskID, "images", len(plans))
}

// Status 汇总进行中的构建操作与现有容器。
func (m *Manager) Status(ctx context.Context, taskID int64) (*Status, error) {
	st := &Status{}
	m.mu.Lock()
	if op, ok := m.ops[taskID]; ok {
		cp := *op
		st.Op = &cp
	}
	m.mu.Unlock()

	names, err := m.containerNames(ctx, taskID)
	if err != nil {
		// docker 暂时够不着时，至少把 op 状态给界面；但错误必须留痕 ——
		// 静默吞错曾让「过滤器语法错误」表现为「容器消失」，极难排查
		slog.Warn("预览状态：列容器失败", "task", taskID, "err", err)
		return st, nil
	}
	if len(names) == 0 {
		return st, nil
	}
	out, _, err := m.exec(ctx, m.DockerBin, append([]string{"container", "inspect"}, names...)...)
	if err != nil {
		slog.Warn("预览状态：inspect 失败", "task", taskID, "err", err)
		return st, nil
	}
	st.Containers = parseInspect(out)
	return st, nil
}

// Stop 取消进行中的构建，强删该任务的全部预览容器，再删掉对应镜像。
// 返回（删掉的容器数, 删掉的镜像数）。
func (m *Manager) Stop(ctx context.Context, taskID int64) (int, int, error) {
	m.mu.Lock()
	if op, ok := m.ops[taskID]; ok {
		if op.cancel != nil {
			op.cancel() // 取消进行中的构建
		}
		delete(m.ops, taskID)
	}
	m.mu.Unlock()

	names, err := m.containerNames(ctx, taskID)
	if err != nil {
		return 0, 0, err
	}
	if len(names) > 0 {
		if _, stderr, err := m.exec(ctx, m.DockerBin, append([]string{"rm", "-f"}, names...)...); err != nil {
			return 0, 0, fmt.Errorf("preview: 删除容器失败: %s", tail(stderr, 500))
		}
	}

	out, _, err := m.exec(ctx, m.DockerBin, "image", "ls", "-q",
		"--filter", "label="+labelPreview+"=1", "--filter", "label="+fmt.Sprintf("%s=%d", labelTask, taskID))
	if err != nil {
		return len(names), 0, fmt.Errorf("preview: 查询镜像失败: %w", err)
	}
	ids := strings.Fields(out)
	if len(ids) > 0 {
		// rmi 失败（如镜像被别的容器引用）不视为整体失败：容器已删，
		// 镜像留着只是占盘，由阈值闸门兜住。
		if _, stderr, err := m.exec(ctx, m.DockerBin, append([]string{"rmi"}, ids...)...); err != nil {
			slog.Warn("预览镜像清理不完整", "task", taskID, "err", tail(stderr, 300))
		}
	}
	return len(names), len(ids), nil
}

func (m *Manager) containerNames(ctx context.Context, taskID int64) ([]string, error) {
	out, _, err := m.exec(ctx, m.DockerBin, "ps", "-aq",
		"--filter", "label="+labelPreview+"=1", "--filter", "label="+fmt.Sprintf("%s=%d", labelTask, taskID))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
	}
	return strings.Fields(out), nil
}

// inspectJSON 是 docker container inspect 输出里我们关心的子集。
type inspectJSON struct {
	Name  string `json:"Name"`
	State struct {
		Status string `json:"Status"`
	} `json:"State"`
	Config struct {
		Image string `json:"Image"`
	} `json:"Config"`
	NetworkSettings struct {
		Ports map[string][]struct {
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
	} `json:"NetworkSettings"`
}

func parseInspect(out string) []Container {
	var raw []inspectJSON
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil
	}
	var out2 []Container
	for _, c := range raw {
		ct := Container{
			Name:  strings.TrimPrefix(c.Name, "/"),
			Image: c.Config.Image,
			State: c.State.Status,
		}
		for cp, bindings := range c.NetworkSettings.Ports {
			if len(bindings) == 0 {
				continue
			}
			var cport, hport int
			fmt.Sscanf(cp, "%d/", &cport)
			fmt.Sscanf(bindings[0].HostPort, "%d", &hport)
			if cport > 0 && hport > 0 {
				ct.Ports = append(ct.Ports, PortMapping{Container: cport, Host: hport})
			}
		}
		out2 = append(out2, ct)
	}
	return out2
}

var slugInvalid = regexp.MustCompile(`[^a-z0-9-]+`)

// slugify 把 Dockerfile 相对路径编成镜像/容器名片段：
// "Dockerfile" → "root"，"apps/web/Dockerfile" → "apps-web"，
// "docker/Dockerfile.api" → "docker-api"。
func slugify(rel string) string {
	rel = filepath.ToSlash(rel)
	dir := filepath.Dir(rel)
	base := filepath.Base(rel)
	var s string
	switch {
	case dir == "." && base == "Dockerfile":
		s = "root"
	case base == "Dockerfile":
		s = dir
	default:
		// 变体名：Dockerfile.api → api，worker.Dockerfile → worker
		v := strings.TrimPrefix(base, "Dockerfile.")
		v = strings.TrimSuffix(v, ".Dockerfile")
		if dir == "." {
			s = v
		} else {
			s = dir + "-" + v
		}
	}
	s = slugInvalid.ReplaceAllString(strings.ToLower(strings.ReplaceAll(s, "/", "-")), "-")
	return strings.Trim(s, "-")
}

// tail 取输出末尾（错误信息最有用的部分通常在最后）。
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
