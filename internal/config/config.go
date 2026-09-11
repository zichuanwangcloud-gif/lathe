// Package config 加载 Lathe 的运行配置。
//
// 只依赖标准库：配置来源为环境变量，缺失时取默认值。
// 密钥类字段（Linear token、GitHub token）不写入日志，见 Redacted。
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config 是 Lathe 控制面与节点代理的完整配置。
type Config struct {
	// 控制面
	HTTPAddr string // 监听地址
	Database Database

	// NodeName 是本实例的节点标识。
	//
	// 消费方：task_events 的 actor 前缀（cmd/lathe/queue.go 的 "node:"+NodeName，
	// 用于在审计流里区分是哪个实例推进了状态）与管理界面的运行时状态面板
	// （cmd/lathe/main.go 的 configStatus）。原先的节点代理已删除，
	// 见 docs/08-debt-cleanup.md D8-1 —— 但这两个消费方与多节点无关，故保留。
	NodeName string

	// DataDir 存放运行时数据：凭据主密钥（secret.key）与验证日志
	// （verify-logs/task-<id>/round-<n>/，见 T4）。
	//
	// 验证日志刻意不放 worktree 里：worktree 会被回收（合并后回收、
	// 同名尸体回收、TTL 收割），而日志的全部价值就在于「现场没了之后
	// 还能查」—— 放在 worktree 里等于排障时正好没有。
	DataDir string

	// 工作区
	WorkspaceRoot string // Lathe 创建 worktree 的根目录
	PnpmStore     string // 共享 pnpm store，避免每任务装一份依赖

	// WorktreeTTL 是终态任务的工作区现场保留时长（T6 收割机）。
	//
	// 默认三天：D4 保留现场是为了让人能进去接手，而 failed 可以转回
	// queued —— 人下班前看到失败、第二天上班接手是正常节奏，
	// 激进的 TTL 会把人正要重试的现场删掉。
	//
	// **注意 time.ParseDuration 不支持 d 单位**：配置里写 72h，不能写 3d。
	// 默认值因此是 72*time.Hour 而不是解析出来的。
	//
	// 下限 MinWorktreeTTL：TTL 不只是「保留多久」的策略，它还是孤儿清扫
	// 那层「新建目录绝不动」的安全机制 —— Create 建出目录到把路径写进
	// 任务行之间有一个无人认领的窗口，只有 mtime 比 cutoff 老才会被删。
	// TTL 配成分钟级，在途任务的现场就会落进那个窗口。想「先清一批存量
	// 目录」是自然的运维动作，但正确的做法是 LATHE_REAP_DRY_RUN。
	WorktreeTTL time.Duration
	// ReapInterval 是收割机的扫描间隔。回收是低频维护动作，
	// 不必像 mergepoll 那样 45 秒一轮。
	ReapInterval time.Duration

	// ReapEnabled 是收割机总开关（LATHE_REAP_ENABLED，默认开）。
	//
	// 这个组件会删磁盘目录与 git 分支，是控制面里破坏力最大的一环，
	// 必须有一个能立刻关掉它的开关。false 时 main 根本不启动收割循环。
	//
	// 用字符串而不是 bool：`LATHE_REAP_ENABLED=false` 与「未设置」必须
	// 能区分 —— 未设置取默认（开），而空字符串的 bool 解析只能是 false，
	// 那会让「没配」变成「关了」。
	ReapEnabled string
	// ReapDryRun 为真时收割机只记「本轮会删什么」，不碰磁盘
	// （LATHE_REAP_DRY_RUN=true）。首次启用或调整 TTL 后先干跑一轮，
	// 看清候选再放它动手。
	ReapDryRun string

	// Agent 执行
	ClaudeBin    string        // claude CLI 路径
	AgentTimeout time.Duration // 单次 agent 执行上限，超时杀进程树
	// SettingSources 传给 claude --setting-sources。默认 "project"：只加载
	// 目标仓库自己的配置，排除执行者个人环境的插件（§9 上下文基线成本）。
	SettingSources string
	// FixAttempts 是验证失败后的修复轮回数上限（docs/02-design.md §5
	// 就地修复）：resume 原实现会话，把失败输出喂回去让 agent 修。
	// 0 关闭修复回路，验证一挂即任务失败。默认 2。
	FixAttempts int
	// TriageChannel / ImplementChannel 是 cc-switch 通道名（B2-2 模型
	// 路由）：分诊走便宜通道、实现与修复走强通道。非空时 pipeline 按
	// 阶段以 LATHE_AGENT_CHANNEL 注入 agent 子进程，由 claude wrapper
	// 解析；为空则跟随 cc-switch 当前激活通道。
	TriageChannel    string
	ImplementChannel string

	// 验证双通道（docs/02-design.md §6.2）：light/heavy 各自独立配额，
	// 不共用一个数字 —— 资源画像差一个量级。任务worker总数 = 两者之和。
	// §6.3 的动态水位推导（按内存/磁盘余量调整）留到多节点时再做。
	LightSlots int // light 档验证并发上限，默认 2
	HeavySlots int // heavy 档验证并发上限，默认 1

	// AdminEmail 是内置超级管理员的邮箱。
	AdminEmail string

	// AdminPassword 是内置超管的初始口令。
	//
	// 留空时启动逻辑随机生成一个并打印到日志里（只打印这一次）。
	// 刻意不写进 Redacted —— 它是货真价实的口令。
	AdminPassword string

	// BaseURL 是本实例的对外访问地址，例如 https://lathe.example.com。
	//
	// 密码重置邮件里的链接必须用它拼，而不能从请求的 Host 头推导：
	// 发起「忘记密码」的请求是未认证的，Host 头可被任意伪造，据此拼出的
	// 链接会把重置令牌送到攻击者的域名上。
	BaseURL string

	// CookieSecure 显式覆盖会话 Cookie 的 Secure 标志。
	// 留空表示按 BaseURL 的协议推断，见 SecureCookies。
	CookieSecure string

	// TrustedProxy 决定是否信任 X-Forwarded-For 里的客户端地址。
	//
	// 默认不信任：无条件读 XFF 会让按 IP 的限流被一行请求头绕过。
	// 只有确实部署在反向代理后面时才打开。
	TrustedProxy bool

	// 集成凭据的环境变量兜底值。
	//
	// 优先级低于界面配置：界面里配了就以界面为准，这样改完即刻生效；
	// 保留环境变量是为了让既有部署方式不受影响。
	LinearToken         string
	LinearWebhookSecret string
	GitHubToken         string
}

// Database 是 Postgres 连接配置。
type Database struct {
	Host     string
	Port     int
	User     string
	Password string
	Name     string
	SSLMode  string
}

// DSN 返回 pgx 可用的连接串。
func (d Database) DSN() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
		d.User, d.Password, d.Host, d.Port, d.Name, d.SSLMode)
}

// MinWorktreeTTL 是工作区现场保留时长的下限（1 小时）。
//
// 为什么是硬下限而不是「<=0 兜回默认」：TTL 同时是孤儿清扫的安全机制。
// 收割机对「磁盘上有、没有任何任务行认领」的目录只按 mtime 判超期，
// 而 Create 建出目录到转入 implementing 写进 worktree_path 之间有窗口
// —— 正在跑的任务的目录在那个窗口里正是「无主的且很新」。1 小时足够
// 盖住这个窗口（Create 本身只跑几条 git 命令），同时不挡「想尽快回收」
// 的正常诉求；比它更激进的值换不来什么，只会把在途现场推进被删区间。
const MinWorktreeTTL = time.Hour

// validateBoolEnv 校验形如 `x == "true"` 的布尔环境变量。
//
// 拼错（"yes" / "1" / "TRUE"）会让开关静默失效 —— 对
// LATHE_REAP_ENABLED 这种「必须能关掉」的开关，静默失效的后果是
// 以为关了其实还在删。所以在启动时报错而不是猜。
func validateBoolEnv(name, v string) error {
	if v == "" || v == "true" || v == "false" {
		return nil
	}
	return fmt.Errorf("config: %s 只能是 true 或 false，得到 %q", name, v)
}

// workspaceRootBlocklist 是绝不能当工作区根目录的路径。
//
// 孤儿清扫会删掉根下所有「非 . 开头、无人认领、mtime 超期」的顶层目录，
// 配成这些值等于让收割机去清理系统目录。
var workspaceRootBlocklist = map[string]bool{
	"/": true, "/bin": true, "/boot": true, "/dev": true, "/etc": true,
	"/home": true, "/lib": true, "/lib64": true, "/media": true, "/mnt": true,
	"/opt": true, "/proc": true, "/root": true, "/run": true, "/sbin": true,
	"/srv": true, "/sys": true, "/tmp": true, "/usr": true, "/var": true,
}

// validateWorkspaceRoot 校验工作区根目录足够「专用」。
//
// 只校验非空 + 绝对路径是不够的：配成 "/" 或 "/opt" 时，孤儿清扫会把
// 下面所有顶层目录都当候选删掉。要求深度至少两层（/opt/lathe/workspaces
// 这种形状），并显式拒绝一批系统目录。
func validateWorkspaceRoot(root string) error {
	clean := filepath.Clean(root)
	if workspaceRootBlocklist[clean] {
		return fmt.Errorf(
			"config: WorkspaceRoot 不能是 %q —— 收割机会清掉根下无人认领的顶层目录", clean)
	}
	// 去掉开头与结尾的分隔符后按段计数："/opt/lathe/workspaces" 是 3 段。
	// 要求 >= 2 段：一层（"/lathe"）意味着根下直接放的就是系统级目录。
	trimmed := strings.Trim(clean, string(filepath.Separator))
	if trimmed == "" || !strings.Contains(trimmed, string(filepath.Separator)) {
		return fmt.Errorf(
			"config: WorkspaceRoot 至少要是两层目录（如 /opt/lathe/workspaces），得到 %q", root)
	}
	return nil
}

// ReapingEnabled 报告 worktree 收割机是否启用（默认启用）。
func (c Config) ReapingEnabled() bool { return c.ReapEnabled != "false" }

// ReapingDryRun 报告收割机是否只干跑（默认关）。
func (c Config) ReapingDryRun() bool { return c.ReapDryRun == "true" }

// Load 从环境变量读取配置并校验。
func Load() (Config, error) {
	c := Config{
		HTTPAddr: env("LATHE_HTTP_ADDR", ":8200"),
		Database: Database{
			Host:     env("LATHE_DB_HOST", "127.0.0.1"),
			Port:     envInt("LATHE_DB_PORT", 55432),
			User:     env("LATHE_DB_USER", "lathe"),
			Password: env("LATHE_DB_PASSWORD", "lathe"),
			Name:     env("LATHE_DB_NAME", "lathe"),
			SSLMode:  env("LATHE_DB_SSLMODE", "disable"),
		},
		NodeName:            env("LATHE_NODE_NAME", hostnameOr("local")),
		DataDir:             env("LATHE_DATA_DIR", "/opt/lathe/data"),
		AdminEmail:          env("LATHE_ADMIN_EMAIL", "admin@lathe.local"),
		AdminPassword:       env("LATHE_ADMIN_PASSWORD", ""),
		BaseURL:             strings.TrimRight(env("LATHE_BASE_URL", ""), "/"),
		CookieSecure:        env("LATHE_COOKIE_SECURE", ""),
		TrustedProxy:        env("LATHE_TRUSTED_PROXY", "") == "true",
		WorkspaceRoot:       env("LATHE_WORKSPACE_ROOT", "/opt/lathe/workspaces"),
		PnpmStore:           env("LATHE_PNPM_STORE", "/opt/lathe/.pnpm-store"),
		ClaudeBin:           env("LATHE_CLAUDE_BIN", "claude"),
		AgentTimeout:        envDuration("LATHE_AGENT_TIMEOUT", 45*time.Minute),
		WorktreeTTL:         envDuration("LATHE_WORKTREE_TTL", 72*time.Hour),
		ReapInterval:        envDuration("LATHE_REAP_INTERVAL", time.Hour),
		ReapEnabled:         env("LATHE_REAP_ENABLED", "true"),
		ReapDryRun:          env("LATHE_REAP_DRY_RUN", "false"),
		SettingSources:      env("LATHE_SETTING_SOURCES", "project"),
		FixAttempts:         envInt("LATHE_FIX_ATTEMPTS", 2),
		TriageChannel:       env("LATHE_TRIAGE_CHANNEL", ""),
		ImplementChannel:    env("LATHE_IMPLEMENT_CHANNEL", ""),
		LightSlots:          envInt("LATHE_LIGHT_SLOTS", 2),
		HeavySlots:          envInt("LATHE_HEAVY_SLOTS", 1),
		LinearToken:         env("LATHE_LINEAR_TOKEN", ""),
		LinearWebhookSecret: env("LATHE_LINEAR_WEBHOOK_SECRET", ""),
		GitHubToken:         env("LATHE_GITHUB_TOKEN", ""),
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// Validate 检查配置自身一致性。
//
// 刻意不在此要求 LinearToken / GitHubToken 非空：控制面在只跑迁移或只提供 UI 时
// 无需外部凭据，缺失应在真正调用集成时报错，而不是拒绝启动。
func (c Config) Validate() error {
	if c.HTTPAddr == "" {
		return fmt.Errorf("config: HTTPAddr 不能为空")
	}
	if c.Database.Host == "" || c.Database.Name == "" {
		return fmt.Errorf("config: 数据库 Host 与 Name 必填")
	}
	if c.Database.Port <= 0 || c.Database.Port > 65535 {
		return fmt.Errorf("config: 数据库端口 %d 非法", c.Database.Port)
	}
	if c.WorkspaceRoot == "" {
		return fmt.Errorf("config: WorkspaceRoot 不能为空")
	}
	if c.DataDir == "" || !strings.HasPrefix(c.DataDir, "/") {
		return fmt.Errorf("config: DataDir 必须是绝对路径，得到 %q", c.DataDir)
	}
	if !strings.HasPrefix(c.WorkspaceRoot, "/") {
		return fmt.Errorf("config: WorkspaceRoot 必须是绝对路径，得到 %q", c.WorkspaceRoot)
	}
	if err := validateWorkspaceRoot(c.WorkspaceRoot); err != nil {
		return err
	}
	if c.AgentTimeout <= 0 {
		return fmt.Errorf("config: AgentTimeout 必须为正，得到 %v", c.AgentTimeout)
	}
	// TTL 与扫描间隔都必须过下限。孤儿清扫的安全完全建立在「mtime 比
	// cutoff 老才删」之上，而 cutoff = now - TTL —— TTL 太短，先建目录、
	// 后写任务行那个无人认领的窗口就盖不住了。
	if c.WorktreeTTL < MinWorktreeTTL {
		return fmt.Errorf(
			"config: WorktreeTTL 不得小于 %v，得到 %v。"+
				"TTL 是孤儿清扫的安全机制而非单纯的保留时长：太短会把「刚建出来、还没记进任务行」的在途现场当孤儿删掉。"+
				"想清存量目录请用 LATHE_REAP_DRY_RUN=true 先干跑一轮看清候选",
			MinWorktreeTTL, c.WorktreeTTL)
	}
	if c.ReapInterval <= 0 {
		return fmt.Errorf("config: ReapInterval 必须为正，得到 %v", c.ReapInterval)
	}
	if err := validateBoolEnv("LATHE_REAP_ENABLED", c.ReapEnabled); err != nil {
		return err
	}
	if err := validateBoolEnv("LATHE_REAP_DRY_RUN", c.ReapDryRun); err != nil {
		return err
	}
	// BaseURL 可以不配（此时由 HTTPAddr 兜底并告警），但配了就必须是
	// 能直接放进邮件正文的绝对地址 —— 拼错的链接要在启动时炸，
	// 而不是等用户点开重置邮件才发现打不开。
	if c.BaseURL != "" {
		u, err := url.Parse(c.BaseURL)
		if err != nil {
			return fmt.Errorf("config: BaseURL 无法解析: %w", err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("config: BaseURL 必须是 http:// 或 https:// 开头的绝对地址，得到 %q", c.BaseURL)
		}
	}
	if c.CookieSecure != "" && c.CookieSecure != "true" && c.CookieSecure != "false" {
		return fmt.Errorf("config: LATHE_COOKIE_SECURE 只能是 true 或 false，得到 %q", c.CookieSecure)
	}
	return nil
}

// PublicURL 返回用于拼接外链的基地址。
//
// 未配 BaseURL 时退回本机地址：能让单机自用跑通，但邮件里的链接外网点不开，
// 所以调用方应当在此时告警。
func (c Config) PublicURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	addr := c.HTTPAddr
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	return "http://" + addr
}

// SecureCookies 报告会话 Cookie 是否应带 Secure 标志。
//
// 默认按 BaseURL 的协议推断：一个配置项同时说明「我的对外地址」与
// 「我是不是 HTTPS」。TLS 卸载在反代上、BaseURL 却写了 http 这类情况，
// 用 LATHE_COOKIE_SECURE 显式覆盖。
func (c Config) SecureCookies() bool {
	switch c.CookieSecure {
	case "true":
		return true
	case "false":
		return false
	}
	return strings.HasPrefix(c.BaseURL, "https://")
}

// Redacted 返回可安全写入日志的配置摘要，密钥一律脱敏。
//
// BaseURL 原样输出：它不是密钥，且启动时看到它很有用 ——
// 「重置邮件里的链接指向哪」正是最容易配错的一项。
func (c Config) Redacted() string {
	return fmt.Sprintf(
		"Config{HTTPAddr:%s BaseURL:%s DB:%s@%s:%d/%s Node:%s Workspace:%s Claude:%s Timeout:%v Linear:%s GitHub:%s}",
		c.HTTPAddr, c.PublicURL(), c.Database.User, c.Database.Host, c.Database.Port, c.Database.Name,
		c.NodeName, c.WorkspaceRoot, c.ClaudeBin, c.AgentTimeout,
		mask(c.LinearToken), mask(c.GitHubToken),
	)
}

func mask(s string) string {
	if s == "" {
		return "<unset>"
	}
	return "<set>"
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func hostnameOr(def string) string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return def
}
