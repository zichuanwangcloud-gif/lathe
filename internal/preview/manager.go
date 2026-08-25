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
	"sort"
	"strings"
	"sync"
	"time"
)

// 预览资源一律打这两个标签：容器与镜像的发现、清理、跨重启归属
// 全靠它们，不依赖任何数据库状态 —— serve 重启后现场仍在。
// compose 编排起来的资源不带这两个标签，但 compose 会自动打
// com.docker.compose.project=lathe-preview-t<id>，发现时两路并查。
const (
	labelPreview = "lathe.preview"
	labelTask    = "lathe.task"
	// labelComposeProject 是 compose 自动打的项目标签。
	labelComposeProject = "com.docker.compose.project"
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

// Selection 是人工选定要启动的一个单元（镜像或编排）。
type Selection struct {
	// Path 是相对 worktree 根的路径（来自 Discover）。
	Path string `json:"path"`
	// Kind 是 "dockerfile"（默认）或 "compose"。
	Kind string `json:"kind"`
	// Ports 是要映射到宿主机的容器端口（仅 dockerfile）。宿主机
	// 端口由 docker 随机分配（-p 0:<port>），避免多任务预览撞端口。
	Ports []int `json:"ports,omitempty"`
	// Env 是人填写的变量（仅 compose）：对应文件里 ${VAR} 引用，
	// 必填项缺值会被拒绝。连不连共享测试库这类有数据风险的决定
	// 由人拍板，系统不自动探测。
	Env map[string]string `json:"env,omitempty"`
}

// StartRequest 是一次启动的完整请求。
type StartRequest struct {
	Selections []Selection `json:"selections"`
	// Infra 是附加基础设施（"postgres"/"redis"/"mysql"）：起官方
	// 镜像进任务网络，并把约定连接串注入所有选中的 Dockerfile 容器。
	// 仅对 Dockerfile 选择生效（compose 的依赖由编排文件自己声明）。
	Infra []string `json:"infra,omitempty"`
	// Env 是注入所有选中 Dockerfile 容器的额外变量（KEY=VALUE）。
	Env map[string]string `json:"env,omitempty"`
}

// buildPlan 是一个选中镜像的构建与启动参数。
type buildPlan struct {
	sel    Selection
	absDF  string
	absCtx string
	image  string
	cname  string
}

// composePlan 是一个选中编排文件的启动参数。
type composePlan struct {
	sel     Selection
	absFile string
}

// infraSpec 描述一种可附加的基础设施服务。
type infraSpec struct {
	Image string   // 官方镜像（优先选本机已缓存的 tag，避免冷拉取）
	Alias string   // 任务网络内的别名，应用容器用它连接
	Env   []string // 基础设施容器自身的 env（初始化口令/库名）
	// AppEnv 注入到该任务所有应用容器的约定连接串。覆盖常见命名
	// （DATABASE_* / POSTGRES_* / REDIS_*），应用不认就静默忽略，
	// 人还可以在额外 env 里覆盖。
	AppEnv map[string]string
	// Ready 是容器内的就绪探测命令；应用 entrypoint 常在启动时
	// 跑迁移，库没就绪就起应用只会崩给人看。
	Ready []string
}

// InfraCatalog 是可附加基础设施的目录（键即界面选项）。
var InfraCatalog = map[string]infraSpec{
	"postgres": {
		Image: "postgres:18-alpine",
		Alias: "pg",
		Env:   []string{"POSTGRES_USER=lathe", "POSTGRES_PASSWORD=lathe-preview", "POSTGRES_DB=app"},
		AppEnv: map[string]string{
			"DATABASE_HOST": "pg", "DATABASE_PORT": "5432",
			"DATABASE_USER": "lathe", "DATABASE_PASSWORD": "lathe-preview", "DATABASE_DBNAME": "app",
			"DATABASE_URL":  "postgres://lathe:lathe-preview@pg:5432/app",
			"POSTGRES_HOST": "pg", "POSTGRES_PORT": "5432",
			"POSTGRES_USER": "lathe", "POSTGRES_PASSWORD": "lathe-preview", "POSTGRES_DB": "app",
		},
		Ready: []string{"pg_isready", "-U", "lathe"},
	},
	"redis": {
		Image: "redis:8.4-alpine",
		Alias: "redis",
		AppEnv: map[string]string{
			"REDIS_HOST": "redis", "REDIS_PORT": "6379",
			"REDIS_URL": "redis://redis:6379",
		},
		Ready: []string{"redis-cli", "ping"},
	},
	"mysql": {
		Image: "mysql:8.4",
		Alias: "mysql",
		Env:   []string{"MYSQL_ROOT_PASSWORD=lathe-preview", "MYSQL_DATABASE=app"},
		AppEnv: map[string]string{
			"MYSQL_HOST": "mysql", "MYSQL_PORT": "3306",
			"MYSQL_DATABASE": "app", "MYSQL_ROOT_PASSWORD": "lathe-preview",
		},
		Ready: []string{"mysqladmin", "ping", "-h127.0.0.1", "-uroot", "-plathe-preview"},
	},
}

// infraReadyTimeout 是基础设施容器就绪的等待上限。
const infraReadyTimeout = 30 * time.Second

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

	mu     sync.Mutex
	ops    map[int64]*Op
	recOps map[int64]*RecommendOp

	// AI 推荐（SetRecommender 装配；nil 时推荐不可用，其余功能不受影响）
	agent          AgentRunner
	agentChannel   string
	settingSources string
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
		recOps:        map[int64]*RecommendOp{},
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
func (m *Manager) Start(ctx context.Context, taskID int64, worktree string, req StartRequest) error {
	if len(req.Selections) == 0 {
		return errors.New("preview: 至少选择一个镜像或编排文件")
	}
	rs, err := m.CheckResources(ctx)
	if err != nil {
		return err
	}
	if !rs.Allowed {
		return fmt.Errorf("%w: %s", ErrOverThreshold, rs.Reason)
	}

	// 校验选择合法：路径必须落在 worktree 内且真是文件；dockerfile
	// 端口合法；compose 必填变量已填齐。
	var plans []buildPlan
	var cplans []composePlan
	seen := map[string]bool{}
	hasDockerfile := false
	for _, s := range req.Selections {
		// Clean 加前导斜杠把 ../ 中和在根部，再去掉斜杠回到相对形态
		// （slugify 与 Join 都需要相对路径）。
		rel := strings.TrimPrefix(filepath.Clean("/"+s.Path), string(filepath.Separator))
		abs := filepath.Join(worktree, rel)
		if _, err := os.Stat(abs); err != nil {
			return fmt.Errorf("preview: %s 不存在: %w", s.Path, err)
		}
		slug := slugify(rel)
		if seen[slug] {
			return fmt.Errorf("preview: %s 与另一选择撞了名，请只选一个", s.Path)
		}
		seen[slug] = true

		if s.Kind == "compose" {
			data, err := readHead(abs, 1<<20)
			if err != nil {
				return fmt.Errorf("preview: 读取 %s 失败: %w", s.Path, err)
			}
			for _, ev := range ScanComposeEnv(string(data)) {
				if ev.Required && strings.TrimSpace(s.Env[ev.Name]) == "" {
					return fmt.Errorf("preview: %s 的必填变量 %s 未填写", s.Path, ev.Name)
				}
			}
			for k := range s.Env {
				if !envNameRe.MatchString(k) {
					return fmt.Errorf("preview: 非法环境变量名 %q", k)
				}
			}
			cplans = append(cplans, composePlan{sel: s, absFile: abs})
			continue
		}

		hasDockerfile = true
		for _, p := range s.Ports {
			if p <= 0 || p > 65535 {
				return fmt.Errorf("preview: 端口 %d 非法", p)
			}
		}
		plans = append(plans, buildPlan{
			sel:    s,
			absDF:  abs,
			absCtx: filepath.Join(worktree, filepath.Dir(rel)),
			image:  fmt.Sprintf("lathe-preview-t%d-%s", taskID, slug),
			cname:  fmt.Sprintf("lathe-preview-t%d-%s", taskID, slug),
		})
	}

	// 附加基础设施只服务 Dockerfile 容器；compose 的依赖拓扑由编排
	// 文件自己声明，两套依赖体系不混。
	if len(req.Infra) > 0 && !hasDockerfile {
		return errors.New("preview: 附加基础设施仅对 Dockerfile 镜像生效（compose 的依赖请在编排文件里声明）")
	}
	for _, name := range req.Infra {
		if _, ok := InfraCatalog[name]; !ok {
			return fmt.Errorf("preview: 未知的基础设施 %q（可选 postgres/redis/mysql）", name)
		}
	}
	for k := range req.Env {
		if !envNameRe.MatchString(k) {
			return fmt.Errorf("preview: 非法环境变量名 %q", k)
		}
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

	go m.run(bctx, taskID, op, req, plans, cplans)
	return nil
}

// run 分阶段启动预览环境：任务网络 → 附加基础设施（等就绪）→
// Dockerfile 镜像构建与启动 → compose 编排。任一失败即终止并记录
// 原因；已起来的容器留给 Stop 统一清理（界面会展示它们）。
func (m *Manager) run(ctx context.Context, taskID int64, op *Op, req StartRequest, plans []buildPlan, cplans []composePlan) {
	fail := func(err error) {
		slog.Warn("预览构建失败", "task", taskID, "err", err)
		m.mu.Lock()
		defer m.mu.Unlock()
		// Stop 已把 op 摘走（人主动放弃）时不再复活失败态
		if cur, ok := m.ops[taskID]; ok && cur == op {
			op.State, op.Error = "failed", err.Error()
		}
	}
	progress := func(line string) {
		m.mu.Lock()
		op.Progress = tail(op.Progress+line+"\n", 4096)
		m.mu.Unlock()
	}

	// 阶段 1：任务网络。同一任务的容器（应用 + 基础设施）进同一
	// 网络按别名互访；网络打标签，Stop 时随容器一起清理。
	netName := fmt.Sprintf("lathe-preview-t%d", taskID)
	if len(plans) > 0 {
		if _, stderr, err := m.exec(ctx, m.DockerBin, "network", "create",
			"--label", labelPreview+"=1", "--label", fmt.Sprintf("%s=%d", labelTask, taskID),
			netName); err != nil {
			fail(fmt.Errorf("创建预览网络失败: %s", tail(stderr, 500)))
			return
		}
	}

	// 阶段 2：附加基础设施，并就绪等待 —— 应用 entrypoint 常在启动
	// 时跑迁移，库没就绪就起应用只会崩给人看。
	appEnv := map[string]string{}
	for _, name := range req.Infra {
		spec := InfraCatalog[name]
		cname := fmt.Sprintf("%s-infra-%s", netName, name)
		progress(fmt.Sprintf("== 启动基础设施 %s（%s）", name, spec.Image))
		args := []string{"run", "-d", "--name", cname,
			"--network", netName, "--network-alias", spec.Alias,
			"--label", labelPreview + "=1", "--label", fmt.Sprintf("%s=%d", labelTask, taskID)}
		for _, e := range spec.Env {
			args = append(args, "-e", e)
		}
		args = append(args, spec.Image)
		if _, stderr, err := m.exec(ctx, m.DockerBin, args...); err != nil {
			fail(fmt.Errorf("启动基础设施 %s 失败: %s", name, tail(stderr, 500)))
			return
		}
		if err := m.waitReady(ctx, cname, spec.Ready); err != nil {
			fail(fmt.Errorf("基础设施 %s 就绪等待失败: %w", name, err))
			return
		}
		for k, v := range spec.AppEnv {
			appEnv[k] = v
		}
	}
	// 人填的额外 env 优先级最高（覆盖约定连接串）。
	for k, v := range req.Env {
		appEnv[k] = v
	}

	// 阶段 3：Dockerfile 镜像构建与启动。
	for _, p := range plans {
		bctx, cancel := context.WithTimeout(ctx, buildTimeout)
		output, err := m.execStream(bctx, m.DockerBin, []string{"build",
			"--label", labelPreview + "=1", "--label", fmt.Sprintf("%s=%d", labelTask, taskID),
			"-t", p.image, "-f", p.absDF, p.absCtx,
		}, progress)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return // 被人为停止：Stop 已清理状态
			}
			fail(fmt.Errorf("构建 %s 失败: %s", p.sel.Path, tail(output, 2000)))
			return
		}

		args := []string{"run", "-d", "--name", p.cname,
			"--network", netName, "--network-alias", slugify(p.sel.Path),
			"--label", labelPreview + "=1", "--label", fmt.Sprintf("%s=%d", labelTask, taskID)}
		for _, port := range p.sel.Ports {
			// 0 = 宿主机随机端口：多任务同时预览不能撞端口。
			// 绑 0.0.0.0 —— 用户从局域网其他机器访问（本机回环够不着）。
			args = append(args, "-p", fmt.Sprintf("0:%d", port))
		}
		envKeys := make([]string, 0, len(appEnv))
		for k := range appEnv {
			envKeys = append(envKeys, k)
		}
		sort.Strings(envKeys)
		for _, k := range envKeys {
			args = append(args, "-e", k+"="+appEnv[k])
		}
		args = append(args, p.image)
		if _, stderr, err := m.exec(ctx, m.DockerBin, args...); err != nil {
			fail(fmt.Errorf("启动 %s 失败: %s", p.image, tail(stderr, 500)))
			return
		}
	}

	// 阶段 4：compose 编排。env 文件 + 端口重置 override 都是临时
	// 文件，up 完成即删 —— 之后的停止/清理全靠项目标签，不再
	// 需要编排文件在场（worktree 可能已回收）。
	for _, cp := range cplans {
		if err := m.composeUp(ctx, taskID, cp, progress); err != nil {
			if ctx.Err() != nil {
				return
			}
			fail(err)
			return
		}
	}

	m.mu.Lock()
	// Stop 可能已摘走 op（人主动停止后构建才跑完）：不干扰
	if cur, ok := m.ops[taskID]; ok && cur == op {
		delete(m.ops, taskID) // 成功：容器自身即状态
	}
	m.mu.Unlock()
	slog.Info("预览环境已启动", "task", taskID, "dockerfile", len(plans), "compose", len(cplans), "infra", len(req.Infra))
}

// waitReady 轮询容器内的就绪探测命令直到成功或超时。
func (m *Manager) waitReady(ctx context.Context, cname string, probe []string) error {
	if len(probe) == 0 {
		return nil
	}
	deadline := time.Now().Add(infraReadyTimeout)
	for {
		args := append([]string{"exec", cname}, probe...)
		if _, _, err := m.exec(ctx, m.DockerBin, args...); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s 超过 %s 未就绪", cname, infraReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// composeUp 用编排文件启动一组服务：先写 env 文件，再 config 解析
// 实际端口并生成随机端口 override，最后 up -d --build。
func (m *Manager) composeUp(ctx context.Context, taskID int64, cp composePlan, progress func(string)) error {
	tmp, err := os.MkdirTemp("", "lathe-preview-compose-*")
	if err != nil {
		return fmt.Errorf("preview: 创建临时目录失败: %w", err)
	}
	defer os.RemoveAll(tmp)

	envFile, err := writeEnvFile(tmp, cp.sel.Env)
	if err != nil {
		return err
	}
	project := ComposeProject(taskID)
	base := []string{"compose", "-f", cp.absFile, "--env-file", envFile, "-p", project}

	progress(fmt.Sprintf("== 解析编排 %s", cp.sel.Path))
	cctx, ccancel := context.WithTimeout(ctx, time.Minute)
	cfgOut, stderr, err := m.exec(cctx, m.DockerBin, append(base, "config", "--format", "json")...)
	ccancel()
	if err != nil {
		return fmt.Errorf("编排 %s 解析失败: %s", cp.sel.Path, tail(stderr, 800))
	}
	svcPorts, err := parseComposeConfig(cfgOut)
	if err != nil {
		return err
	}

	args := base
	if override := buildOverrideYAML(svcPorts); override != "" {
		ovPath := filepath.Join(tmp, "override.yml")
		if err := os.WriteFile(ovPath, []byte(override), 0o600); err != nil {
			return fmt.Errorf("preview: 写 override 失败: %w", err)
		}
		// override 必须排在主文件之后（后者覆盖前者）
		args = []string{"compose", "-f", cp.absFile, "-f", ovPath,
			"--env-file", envFile, "-p", project}
	}

	progress(fmt.Sprintf("== 启动编排 %s（项目 %s）", cp.sel.Path, project))
	uctx, ucancel := context.WithTimeout(ctx, buildTimeout)
	output, err := m.execStream(uctx, m.DockerBin, append(args, "up", "-d", "--build"), progress)
	ucancel()
	if err != nil {
		return fmt.Errorf("编排 %s 启动失败: %s", cp.sel.Path, tail(output, 2000))
	}
	return nil
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

	// 网络随容器清理：任务网络（dockerfile 流）与 compose 项目网络
	// 都靠标签发现。网络删除失败（如还有别的容器接着）不挡主路。
	if nets, err := m.networkNames(ctx, taskID); err == nil && len(nets) > 0 {
		if _, stderr, err := m.exec(ctx, m.DockerBin, append([]string{"network", "rm"}, nets...)...); err != nil {
			slog.Warn("预览网络清理不完整", "task", taskID, "err", tail(stderr, 300))
		}
	}

	// 镜像：dockerfile 流打 lathe.* 标签；compose 构建的镜像自动带
	// 项目标签。注意 compose 里 image: 拉取的镜像不带项目标签，
	// 不会被误删 —— 那是共享的注册表镜像，不是我们的构建产物。
	ids := map[string]bool{}
	for _, filter := range [][]string{
		{"--filter", "label=" + labelPreview + "=1", "--filter", "label=" + fmt.Sprintf("%s=%d", labelTask, taskID)},
		{"--filter", "label=" + fmt.Sprintf("%s=%s", labelComposeProject, ComposeProject(taskID))},
	} {
		out, _, err := m.exec(ctx, m.DockerBin, append([]string{"image", "ls", "-q"}, filter...)...)
		if err != nil {
			return len(names), 0, fmt.Errorf("preview: 查询镜像失败: %w", err)
		}
		for _, id := range strings.Fields(out) {
			ids[id] = true
		}
	}
	if len(ids) > 0 {
		list := make([]string, 0, len(ids))
		for id := range ids {
			list = append(list, id)
		}
		// rmi 失败（如镜像被别的容器引用）不视为整体失败：容器已删，
		// 镜像留着只是占盘，由阈值闸门兜住。
		if _, stderr, err := m.exec(ctx, m.DockerBin, append([]string{"rmi"}, list...)...); err != nil {
			slog.Warn("预览镜像清理不完整", "task", taskID, "err", tail(stderr, 300))
		}
	}
	return len(names), len(ids), nil
}

// containerNames 找出该任务的全部预览容器：dockerfile 流的
// lathe.* 标签与 compose 流的项目标签两路并查（OR 语义，
// docker 的多个 --filter 是 AND，所以分两次查再合并）。
func (m *Manager) containerNames(ctx context.Context, taskID int64) ([]string, error) {
	seen := map[string]bool{}
	var names []string
	for _, filter := range [][]string{
		{"--filter", "label=" + labelPreview + "=1", "--filter", "label=" + fmt.Sprintf("%s=%d", labelTask, taskID)},
		{"--filter", "label=" + fmt.Sprintf("%s=%s", labelComposeProject, ComposeProject(taskID))},
	} {
		out, _, err := m.exec(ctx, m.DockerBin, append([]string{"ps", "-aq"}, filter...)...)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
		}
		for _, n := range strings.Fields(out) {
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	return names, nil
}

// networkNames 找出该任务的预览网络（两路标签同上）。
func (m *Manager) networkNames(ctx context.Context, taskID int64) ([]string, error) {
	seen := map[string]bool{}
	var names []string
	for _, filter := range [][]string{
		{"--filter", "label=" + fmt.Sprintf("%s=%d", labelTask, taskID)},
		{"--filter", "label=" + fmt.Sprintf("%s=%s", labelComposeProject, ComposeProject(taskID))},
	} {
		out, _, err := m.exec(ctx, m.DockerBin, append([]string{"network", "ls", "-q"}, filter...)...)
		if err != nil {
			return nil, err
		}
		for _, n := range strings.Fields(out) {
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	return names, nil
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
