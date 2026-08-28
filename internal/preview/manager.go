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
	"strconv"
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
	// Env 是人填写的变量（仅 compose）：对应文件里 ${VAR} 引用。
	// 口令类必填变量留空时由系统自动生成（不让人填口令）；
	// 其余必填项缺值会被拒绝。
	Env map[string]string `json:"env,omitempty"`
}

// DatabasePlan 是数据库策略（通常由 AI 推荐产生，人采用后随启动传入）。
type DatabasePlan struct {
	// Strategy: reuse=连主部署在跑的库（有 SQL 变更时会被拒绝）；
	// clone=从源容器克隆一份独立库；fresh=全新空库；none=不需要；
	// baseline=连仓库配置的基线目录（见 baseline.go）已经在跑的中间件。
	Strategy string `json:"strategy"`
	Source   string `json:"source,omitempty"` // reuse/clone/baseline 的源容器名（baseline 唯一匹配时可留空）
	DBName   string `json:"dbName,omitempty"` // clone 的源库名（空则取源容器配置）
	// Dir 仅 baseline 策略使用：基线目录路径。只能由服务端从仓库配置注入
	// （httpapi 层），不接受客户端直接传入——避免把任意路径检测暴露成
	// 一个客户端可控的参数。
	Dir string `json:"-"`
}

// resolvedDatabase 是 Start 校验后定案的数据库执行计划（内部态）。
type resolvedDatabase struct {
	plan DatabasePlan
	// 连接参数标量：供 compose 变量名映射与 dockerfile 约定 env。
	host, port, user, password, dbName string
	env                                map[string]string // dockerfile 流注入应用容器的约定连接串
	hostGateway                        bool              // 应用容器需要 host.docker.internal 解析
	// clone 执行参数
	srcName, srcImage, srcUser, srcDB string
	cloneCname                        string
	// autoFilled 是自动生成的口令类变量名（compose 流），进进度日志留痕
	autoFilled []string
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
	// Database 是数据库策略（AI 推荐产生，人采用后随启动传入）。
	Database *DatabasePlan `json:"database,omitempty"`
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
	envs    []EnvVarSpec // 扫描出的变量表（Start 校验阶段填充）
}

// infraSpec 描述一种可附加的基础设施服务。
type infraSpec struct {
	Image string // 官方镜像（优先选本机已缓存的 tag，避免冷拉取）
	Alias string // 任务网络内的别名，应用容器用它连接
	// Env 返回基础设施容器自身的 env（pw 是本次启动生成的口令 ——
	// 口令类值永远由系统生成，不让人填）。
	Env func(pw string) []string
	// AppEnv 返回注入到该任务所有应用容器的约定连接串。覆盖常见命名
	// （DATABASE_* / POSTGRES_* / REDIS_*），应用不认就静默忽略，
	// 人还可以在额外 env 里覆盖。
	AppEnv func(pw string) map[string]string
	// Ready 返回容器内的就绪探测命令（pw 同 Env）；应用 entrypoint
	// 常在启动时跑迁移，库没就绪就起应用只会崩给人看。
	Ready func(pw string) []string
}

// InfraCatalog 是可附加基础设施的目录（键即界面选项）。
var InfraCatalog = map[string]infraSpec{
	"postgres": {
		Image: "postgres:18-alpine",
		Alias: "pg",
		Env: func(pw string) []string {
			return []string{"POSTGRES_USER=lathe", "POSTGRES_PASSWORD=" + pw, "POSTGRES_DB=app"}
		},
		AppEnv: func(pw string) map[string]string {
			return map[string]string{
				"DATABASE_HOST": "pg", "DATABASE_PORT": "5432",
				"DATABASE_USER": "lathe", "DATABASE_PASSWORD": pw, "DATABASE_DBNAME": "app",
				"DATABASE_URL":  fmt.Sprintf("postgres://lathe:%s@pg:5432/app", pw),
				"POSTGRES_HOST": "pg", "POSTGRES_PORT": "5432",
				"POSTGRES_USER": "lathe", "POSTGRES_PASSWORD": pw, "POSTGRES_DB": "app",
			}
		},
		Ready: func(string) []string {
			// pg_isready 在 init 临时服务阶段就返回成功（自建库还没建出来）
			// —— 必须探测到目标库可查，才算就绪
			return []string{"psql", "-U", "lathe", "-d", "app", "-tAc", "SELECT 1"}
		},
	},
	"redis": {
		Image: "redis:8.4-alpine",
		Alias: "redis",
		Env:   func(string) []string { return nil },
		AppEnv: func(string) map[string]string {
			return map[string]string{
				"REDIS_HOST": "redis", "REDIS_PORT": "6379",
				"REDIS_URL": "redis://redis:6379",
			}
		},
		Ready: func(string) []string { return []string{"redis-cli", "ping"} },
	},
	"mysql": {
		Image: "mysql:8.4",
		Alias: "mysql",
		Env: func(pw string) []string {
			return []string{"MYSQL_ROOT_PASSWORD=" + pw, "MYSQL_DATABASE=app"}
		},
		AppEnv: func(pw string) map[string]string {
			return map[string]string{
				"MYSQL_HOST": "mysql", "MYSQL_PORT": "3306",
				"MYSQL_DATABASE": "app", "MYSQL_ROOT_PASSWORD": pw,
			}
		},
		Ready: func(pw string) []string {
			// 同理：探到自建库可查（mysqladmin ping 在 init 阶段就会成功）
			return []string{"mysql", "-uroot", "-p" + pw, "app", "-e", "SELECT 1"}
		},
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
			for k := range s.Env {
				if !envNameRe.MatchString(k) {
					return fmt.Errorf("preview: 非法环境变量名 %q", k)
				}
			}
			cplans = append(cplans, composePlan{sel: s, absFile: abs, envs: ScanComposeEnv(string(data))})
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

	// 数据库策略定案：reuse/clone 的源容器必须真实存在且是 DB 家族；
	// reuse 前机械复查 SQL 变更（不信任上游计划，危险位系统把守）。
	db := &resolvedDatabase{}
	if req.Database != nil && req.Database.Strategy != "" && req.Database.Strategy != "none" {
		db, err = m.resolveDatabase(ctx, taskID, worktree, *req.Database)
		if err != nil {
			return err
		}
		if db.plan.Strategy == "fresh" && hasDockerfile {
			has := false
			for _, n := range req.Infra {
				if n == "postgres" {
					has = true
				}
			}
			if !has {
				req.Infra = append(req.Infra, "postgres")
			}
		}
	}

	// compose 必填变量解析顺序：人/agent 已填 > 数据库策略映射 >
	// 口令类自动生成；仍空的才拒绝。自动生成的进进度日志留痕。
	for i := range cplans {
		if err := resolveComposeEnv(&cplans[i], db); err != nil {
			return err
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

	go m.run(bctx, taskID, op, req, plans, cplans, db)
	return nil
}

// resolveComposeEnv 按序填充 compose 必填变量：人/agent 已填 >
// 数据库策略映射 > 口令类自动生成；仍空的拒绝并点名。
func resolveComposeEnv(cp *composePlan, db *resolvedDatabase) error {
	if cp.sel.Env == nil {
		cp.sel.Env = map[string]string{}
	}
	for _, ev := range cp.envs {
		if !ev.Required || strings.TrimSpace(cp.sel.Env[ev.Name]) != "" {
			continue
		}
		if db != nil {
			if v, ok := db.mapVar(ev.Name); ok {
				cp.sel.Env[ev.Name] = v
				continue
			}
		}
		if passwordClassRe.MatchString(ev.Name) {
			cp.sel.Env[ev.Name] = generatePassword()
			db.autoFilled = append(db.autoFilled, ev.Name)
			continue
		}
		return fmt.Errorf("preview: %s 的必填变量 %s 未填写", cp.sel.Path, ev.Name)
	}
	return nil
}

// resolveDatabase 把数据库策略定案为可执行计划：源容器核查、SQL
// 变更硬护栏、连接参数与口令生成。所有事实在此固定，run 只管执行。
func (m *Manager) resolveDatabase(ctx context.Context, taskID int64, worktree string, plan DatabasePlan) (*resolvedDatabase, error) {
	r := &resolvedDatabase{plan: plan}
	switch plan.Strategy {
	case "fresh":
		return r, nil // 全新空库 = infra postgres（Start 负责补进 infra 列表）
	case "reuse", "clone", "baseline":
	default:
		return nil, fmt.Errorf("preview: 未知的数据库策略 %q", plan.Strategy)
	}

	// 源容器核查：baseline 从仓库配置的基线目录检测结果里找，
	// reuse/clone 沿用现有的「在全机 docker ps 里按名字找」。
	var src *RunningContainer
	if plan.Strategy == "baseline" {
		s, err := m.resolveBaselineSource(ctx, plan)
		if err != nil {
			return nil, err
		}
		src = s
	} else {
		containers := m.runningContainers(ctx)
		for i := range containers {
			if containers[i].Name == plan.Source {
				src = &containers[i]
				break
			}
		}
		if src == nil {
			return nil, fmt.Errorf("preview: 数据库源容器 %q 不在运行中", plan.Source)
		}
		if src.DBKind == "" {
			return nil, fmt.Errorf("preview: 源容器 %q（%s）不是数据库镜像", plan.Source, src.Image)
		}
	}

	if plan.Strategy == "reuse" || plan.Strategy == "baseline" {
		// 复用走网络：源库必须有已发布端口，预览容器经宿主网关到达
		if src.HostPort == 0 {
			return nil, fmt.Errorf("preview: 源容器 %q 没有已发布的宿主端口，预览容器无法到达", src.Name)
		}
		// 硬护栏：有 SQL/迁移变更禁止复用共享库 —— 迁移可能改坏
		// 别人在用的数据。推荐层会纠正，这里是执行前的最后防线。
		// baseline 与 reuse 是同一条纪律：换个策略名字不能绕过它，
		// 撞上时人需要显式改选克隆策略（同 reuse 现有行为）。
		if prof := m.changeProfile(ctx, worktree); prof.HasSQL {
			return nil, errors.New("preview: 检测到 SQL/迁移变更，不能复用共享库（请改用克隆策略）")
		}
		r.host = "host.docker.internal"
		r.hostGateway = true
		r.port = strconv.Itoa(src.HostPort)
		r.user, r.password, r.dbName = dbCredentials(src, plan.DBName)
		r.env = buildDBEnv(src.DBKind, r.host, r.port, r.user, r.password, r.dbName)
		return r, nil
	}

	// clone：本期仅支持 postgres（pg_dump | psql 管道，版本由源容器保证）
	if src.DBKind != "postgres" {
		return nil, fmt.Errorf("preview: %s 的克隆暂不支持（本期仅 postgres）", src.DBKind)
	}
	r.srcName, r.srcImage = src.Name, src.Image
	r.srcUser = src.Env["POSTGRES_USER"]
	if r.srcUser == "" {
		r.srcUser = "postgres"
	}
	r.srcDB = plan.DBName
	if r.srcDB == "" {
		r.srcDB = src.Env["POSTGRES_DB"]
	}
	if r.srcDB == "" {
		r.srcDB = "postgres"
	}
	// 克隆库用全新口令与超级用户（源库口令不需要出源容器）
	r.host, r.port = "pg", "5432"
	r.user, r.password = "postgres", generatePassword()
	r.dbName = r.srcDB // 库名保持一致，应用配置不用改
	r.cloneCname = fmt.Sprintf("lathe-preview-t%d-infra-postgres", taskID)
	r.env = buildDBEnv("postgres", r.host, r.port, r.user, r.password, r.dbName)
	return r, nil
}

// resolveBaselineSource 把 baseline 策略定案到 DetectBaseline 结果里
// 一个具体的在跑服务：plan.Source 非空时按容器名/服务名精确匹配消歧；
// 为空时要求"在跑且有 DBKind 的服务"里只能有唯一一个匹配，否则报错
// 让人显式指定（不猜——基线目录理论上可以有多个数据库）。
func (m *Manager) resolveBaselineSource(ctx context.Context, plan DatabasePlan) (*RunningContainer, error) {
	if strings.TrimSpace(plan.Dir) == "" {
		return nil, errors.New("preview: baseline 策略缺少基线目录（仓库未配置基线目录，或未正确注入）")
	}
	status, err := m.DetectBaseline(ctx, plan.Dir, "")
	if err != nil {
		return nil, fmt.Errorf("preview: 检测基线目录失败: %w", err)
	}

	var matches []BaselineService
	for _, svc := range status.Services {
		if !svc.Running || svc.DBKind == "" {
			continue
		}
		if plan.Source != "" && svc.ContainerName != plan.Source && svc.Service != plan.Source {
			continue
		}
		matches = append(matches, svc)
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf(
			"preview: 基线目录 %s 里没有检测到在跑的中间件（先在仓库配置页部署基线，或改用其他数据库策略）",
			plan.Dir)
	case 1:
		svc := matches[0]
		if svc.HostPort == 0 {
			return nil, fmt.Errorf("preview: 基线中间件 %q 没有已发布的宿主端口，预览容器无法到达", svc.ContainerName)
		}
		return &RunningContainer{
			Name: svc.ContainerName, Image: svc.Image, DBKind: svc.DBKind,
			HostPort: svc.HostPort, Env: svc.Env,
		}, nil
	default:
		names := make([]string, 0, len(matches))
		for _, s := range matches {
			names = append(names, s.ContainerName)
		}
		return nil, fmt.Errorf("preview: 基线目录里有多个在跑的中间件匹配（%s），请指定 source 消歧",
			strings.Join(names, ", "))
	}
}

// dbCredentials 从源容器 env 提取连接三元组。
func dbCredentials(src *RunningContainer, dbNameOverride string) (user, pass, db string) {
	switch src.DBKind {
	case "postgres":
		user = orDefault(src.Env["POSTGRES_USER"], "postgres")
		pass = src.Env["POSTGRES_PASSWORD"]
		db = orDefault(dbNameOverride, src.Env["POSTGRES_DB"], "postgres")
	case "mysql":
		user = orDefault(src.Env["MYSQL_USER"], "root")
		pass = orDefault(src.Env["MYSQL_PASSWORD"], src.Env["MYSQL_ROOT_PASSWORD"])
		db = orDefault(dbNameOverride, src.Env["MYSQL_DATABASE"], "app")
	}
	return user, pass, db
}

// buildDBEnv 生成注入应用容器的约定连接串（覆盖常见命名，应用不认
// 就静默忽略，人可用额外 env 覆盖）。
func buildDBEnv(kind, host, port, user, pass, db string) map[string]string {
	switch kind {
	case "postgres":
		return map[string]string{
			"DATABASE_HOST": host, "DATABASE_PORT": port,
			"DATABASE_USER": user, "DATABASE_PASSWORD": pass,
			"DATABASE_DBNAME": db, "DATABASE_NAME": db,
			"DATABASE_URL":  fmt.Sprintf("postgres://%s:%s@%s:%s/%s", user, pass, host, port, db),
			"POSTGRES_HOST": host, "POSTGRES_PORT": port,
			"POSTGRES_USER": user, "POSTGRES_PASSWORD": pass, "POSTGRES_DB": db,
			"PGHOST": host, "PGPORT": port,
			"PGUSER": user, "PGPASSWORD": pass, "PGDATABASE": db,
		}
	case "mysql":
		return map[string]string{
			"MYSQL_HOST": host, "MYSQL_PORT": port,
			"MYSQL_USER": user, "MYSQL_PASSWORD": pass, "MYSQL_DATABASE": db,
			"DATABASE_HOST": host, "DATABASE_PORT": port,
			"DATABASE_URL": fmt.Sprintf("mysql://%s:%s@%s:%s/%s", user, pass, host, port, db),
		}
	case "redis":
		return map[string]string{
			"REDIS_HOST": host, "REDIS_PORT": port,
			"REDIS_URL": fmt.Sprintf("redis://%s:%s", host, port),
		}
	}
	return nil
}

// mapVar 把 compose 必填变量名映射到数据库策略的连接参数。
// 家族感知：postgres 策略不会去吃 REDIS_HOST 这类其他中间件的变量。
func (r *resolvedDatabase) mapVar(name string) (string, bool) {
	if r.host == "" {
		return "", false
	}
	up := strings.ToUpper(name)
	// 其他中间件家族的变量不映射（由各自策略或自动生成处理）
	for _, f := range []string{"REDIS", "KAFKA", "RABBIT", "MONGO", "ELASTIC", "MINIO", "_MQ_", "S3_"} {
		if strings.Contains(up, f) {
			return "", false
		}
	}
	switch {
	case strings.HasSuffix(up, "_HOST") || up == "PGHOST":
		return r.host, true
	case strings.HasSuffix(up, "_PORT") || up == "PGPORT":
		return r.port, true
	case strings.HasSuffix(up, "_USER") || up == "PGUSER":
		return r.user, true
	case strings.Contains(up, "PASSWORD") || strings.Contains(up, "PASSWD") || up == "PGPASSWORD":
		// 只有口令词才映射 DB 口令；JWT_SECRET 这类走自动生成
		return r.password, true
	case strings.HasSuffix(up, "_DB") || strings.HasSuffix(up, "_DATABASE") ||
		strings.HasSuffix(up, "_DBNAME") || up == "PGDATABASE" ||
		(strings.HasSuffix(up, "_NAME") && (strings.Contains(up, "DATABASE") ||
			strings.Contains(up, "DB") || strings.Contains(up, "POSTGRES") ||
			strings.Contains(up, "MYSQL"))):
		// _NAME 泛后缀必须带 DB 提示词，否则 APP_NAME 会被喂库名
		return r.dbName, true
	}
	return "", false
}

func orDefault(v string, defs ...string) string {
	if v != "" {
		return v
	}
	for _, d := range defs {
		if d != "" {
			return d
		}
	}
	return ""
}

// run 分阶段启动预览环境：任务网络 → 附加基础设施（等就绪）→
// 数据库策略（克隆灌数据）→ Dockerfile 镜像构建与启动 → compose 编排。
// 任一失败即终止并记录原因；已起来的容器留给 Stop 统一清理。
func (m *Manager) run(ctx context.Context, taskID int64, op *Op, req StartRequest, plans []buildPlan, cplans []composePlan, db *resolvedDatabase) {
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

	// 阶段 1：任务网络。同一任务的容器（应用 + 基础设施 + 克隆库）
	// 进同一网络按别名互访；网络打标签，Stop 时随容器一起清理。
	netName := fmt.Sprintf("lathe-preview-t%d-infra", taskID)
	needNet := len(plans) > 0 || (db != nil && db.plan.Strategy == "clone")
	if needNet {
		if _, stderr, err := m.exec(ctx, m.DockerBin, "network", "create",
			"--label", labelPreview+"=1", "--label", fmt.Sprintf("%s=%d", labelTask, taskID),
			netName); err != nil {
			fail(fmt.Errorf("创建预览网络失败: %s", tail(stderr, 500)))
			return
		}
	}
	if db != nil && len(db.autoFilled) > 0 {
		progress("== 已自动生成口令类变量（不让人填口令）: " + strings.Join(db.autoFilled, ", "))
	}

	// 阶段 2：附加基础设施，并就绪等待 —— 应用 entrypoint 常在启动
	// 时跑迁移，库没就绪就起应用只会崩给人看。口令每次启动生成。
	appEnv := map[string]string{}
	for _, name := range req.Infra {
		spec := InfraCatalog[name]
		pw := generatePassword()
		cname := fmt.Sprintf("lathe-preview-t%d-infra-%s", taskID, name)
		progress(fmt.Sprintf("== 启动基础设施 %s（%s）", name, spec.Image))
		args := []string{"run", "-d", "--name", cname,
			"--network", netName, "--network-alias", spec.Alias,
			"--label", labelPreview + "=1", "--label", fmt.Sprintf("%s=%d", labelTask, taskID)}
		for _, e := range spec.Env(pw) {
			args = append(args, "-e", e)
		}
		args = append(args, spec.Image)
		if _, stderr, err := m.exec(ctx, m.DockerBin, args...); err != nil {
			fail(fmt.Errorf("启动基础设施 %s 失败: %s", name, tail(stderr, 500)))
			return
		}
		if err := m.waitReady(ctx, cname, spec.Ready(pw)); err != nil {
			fail(fmt.Errorf("基础设施 %s 就绪等待失败: %w", name, err))
			return
		}
		for k, v := range spec.AppEnv(pw) {
			appEnv[k] = v
		}
	}

	// 阶段 2.5：数据库克隆 —— 与源同镜像的 PG 进任务网络，就绪后
	// pg_dump | psql 管道灌数据（不用 CREATE DATABASE ... TEMPLATE：
	// 它要求源库零连接，对在跑的共享库有风险）。
	if db != nil && db.plan.Strategy == "clone" {
		progress(fmt.Sprintf("== 克隆数据库 %s/%s → %s", db.srcName, db.srcDB, db.cloneCname))
		args := []string{"run", "-d", "--name", db.cloneCname,
			"--network", netName, "--network-alias", "pg",
			"--label", labelPreview + "=1", "--label", fmt.Sprintf("%s=%d", labelTask, taskID),
			"-e", "POSTGRES_PASSWORD=" + db.password, "-e", "POSTGRES_DB=" + db.dbName,
			db.srcImage}
		if _, stderr, err := m.exec(ctx, m.DockerBin, args...); err != nil {
			fail(fmt.Errorf("启动克隆库失败: %s", tail(stderr, 500)))
			return
		}
		if err := m.waitReady(ctx, db.cloneCname,
			[]string{"psql", "-U", "postgres", "-d", db.dbName, "-tAc", "SELECT 1"}); err != nil {
			fail(fmt.Errorf("克隆库就绪等待失败: %w", err))
			return
		}
		// 管道不过磁盘；--no-owner/--no-privileges 避免源库角色在
		// 克隆库里不存在导致恢复失败
		dctx, dcancel := context.WithTimeout(ctx, 10*time.Minute)
		_, stderr, err := m.exec(dctx, "sh", "-c",
			fmt.Sprintf("docker exec %s pg_dump -U %s --no-owner --no-privileges %s | "+
				"docker exec -i %s psql -U postgres -d %s -q",
				shellQuote(db.srcName), shellQuote(db.srcUser), shellQuote(db.srcDB),
				shellQuote(db.cloneCname), shellQuote(db.dbName)))
		dcancel()
		if err != nil {
			fail(fmt.Errorf("克隆灌数据失败: %s", tail(stderr, 800)))
			return
		}
		progress("== 数据库克隆完成")
	}
	// 数据库策略的连接串优先级：高于 infra 约定值、低于人填的额外 env
	if db != nil {
		for k, v := range db.env {
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
		if db != nil && db.hostGateway {
			// 复用主部署的库：经宿主网关到达它发布的端口
			args = append(args, "--add-host", "host.docker.internal:host-gateway")
		}
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
		if err := m.composeUp(ctx, taskID, cp, db != nil && db.hostGateway, progress); err != nil {
			if ctx.Err() != nil {
				return
			}
			fail(err)
			return
		}
		// 克隆库跨网络挂接：compose 服务在项目自己的网络里，
		// 把克隆容器以 pg 别名接进去，应用按别名直达
		if db != nil && db.plan.Strategy == "clone" {
			project := ComposeProject(taskID)
			if _, stderr, err := m.exec(ctx, m.DockerBin, "network", "connect",
				"--alias", "pg", project+"_default", db.cloneCname); err != nil {
				fail(fmt.Errorf("克隆库挂接 compose 网络失败: %s", tail(stderr, 500)))
				return
			}
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

// shellQuote 最小化 shell 引号包裹（容器名/库名/用户名都是受控
// 标识符，但走 sh -c 管道就按规矩引用）。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
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
func (m *Manager) composeUp(ctx context.Context, taskID int64, cp composePlan, hostGateway bool, progress func(string)) error {
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
	if override := buildOverrideYAML(svcPorts, hostGateway); override != "" {
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
