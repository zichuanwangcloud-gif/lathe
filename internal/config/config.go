// Package config 加载 Lathe 的运行配置。
//
// 只依赖标准库：配置来源为环境变量，缺失时取默认值。
// 密钥类字段（Linear token、GitHub token）不写入日志，见 Redacted。
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 是 Lathe 控制面与节点代理的完整配置。
type Config struct {
	// 控制面
	HTTPAddr string // 监听地址
	Database Database

	// 节点身份（lathe-runner 用）
	NodeName string

	// DataDir 存放运行时数据（凭据主密钥等）。
	DataDir string

	// 工作区
	WorkspaceRoot string // Lathe 创建 worktree 的根目录
	PnpmStore     string // 共享 pnpm store，避免每任务装一份依赖

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
	if c.AgentTimeout <= 0 {
		return fmt.Errorf("config: AgentTimeout 必须为正，得到 %v", c.AgentTimeout)
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
