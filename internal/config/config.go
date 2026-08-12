// Package config 加载 Lathe 的运行配置。
//
// 只依赖标准库：配置来源为环境变量，缺失时取默认值。
// 密钥类字段（Linear token、GitHub token）不写入日志，见 Redacted。
package config

import (
	"fmt"
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

	// 工作区
	WorkspaceRoot string // Lathe 创建 worktree 的根目录
	PnpmStore     string // 共享 pnpm store，避免每任务装一份依赖

	// Agent 执行
	ClaudeBin    string        // claude CLI 路径
	AgentTimeout time.Duration // 单次 agent 执行上限，超时杀进程树

	// 集成（P0 用静态 token；OAuth / GitHub App 留到 P2）
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
		WorkspaceRoot:       env("LATHE_WORKSPACE_ROOT", "/opt/lathe/workspaces"),
		PnpmStore:           env("LATHE_PNPM_STORE", "/opt/lathe/.pnpm-store"),
		ClaudeBin:           env("LATHE_CLAUDE_BIN", "claude"),
		AgentTimeout:        envDuration("LATHE_AGENT_TIMEOUT", 45*time.Minute),
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
	if !strings.HasPrefix(c.WorkspaceRoot, "/") {
		return fmt.Errorf("config: WorkspaceRoot 必须是绝对路径，得到 %q", c.WorkspaceRoot)
	}
	if c.AgentTimeout <= 0 {
		return fmt.Errorf("config: AgentTimeout 必须为正，得到 %v", c.AgentTimeout)
	}
	return nil
}

// Redacted 返回可安全写入日志的配置摘要，密钥一律脱敏。
func (c Config) Redacted() string {
	return fmt.Sprintf(
		"Config{HTTPAddr:%s DB:%s@%s:%d/%s Node:%s Workspace:%s Claude:%s Timeout:%v Linear:%s GitHub:%s}",
		c.HTTPAddr, c.Database.User, c.Database.Host, c.Database.Port, c.Database.Name,
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
