package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	// 不设任何环境变量时应取默认值并通过校验。
	c, err := Load()
	if err != nil {
		t.Fatalf("Load() 报错: %v", err)
	}
	if c.HTTPAddr != ":8200" {
		t.Errorf("HTTPAddr = %q, 期望 :8200", c.HTTPAddr)
	}
	if c.Database.Port != 55432 {
		t.Errorf("Database.Port = %d, 期望 55432（刻意避开宿主机 5432）", c.Database.Port)
	}
	if c.AgentTimeout != 45*time.Minute {
		t.Errorf("AgentTimeout = %v, 期望 45m", c.AgentTimeout)
	}
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv("LATHE_HTTP_ADDR", ":9000")
	t.Setenv("LATHE_DB_PORT", "6000")
	t.Setenv("LATHE_AGENT_TIMEOUT", "10m")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load() 报错: %v", err)
	}
	if c.HTTPAddr != ":9000" {
		t.Errorf("HTTPAddr = %q, 期望 :9000", c.HTTPAddr)
	}
	if c.Database.Port != 6000 {
		t.Errorf("Database.Port = %d, 期望 6000", c.Database.Port)
	}
	if c.AgentTimeout != 10*time.Minute {
		t.Errorf("AgentTimeout = %v, 期望 10m", c.AgentTimeout)
	}
}

func TestValidate(t *testing.T) {
	base := func() Config {
		c, err := Load()
		if err != nil {
			t.Fatalf("基准配置构造失败: %v", err)
		}
		return c
	}

	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"空监听地址", func(c *Config) { c.HTTPAddr = "" }, "HTTPAddr"},
		{"空数据库名", func(c *Config) { c.Database.Name = "" }, "Host 与 Name"},
		{"端口越界", func(c *Config) { c.Database.Port = 70000 }, "端口"},
		{"端口为零", func(c *Config) { c.Database.Port = 0 }, "端口"},
		{"工作区为空", func(c *Config) { c.WorkspaceRoot = "" }, "WorkspaceRoot 不能为空"},
		{"工作区相对路径", func(c *Config) { c.WorkspaceRoot = "workspaces" }, "绝对路径"},
		{"超时为零", func(c *Config) { c.AgentTimeout = 0 }, "AgentTimeout"},
		{"超时为负", func(c *Config) { c.AgentTimeout = -time.Second }, "AgentTimeout"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("期望报错含 %q，但通过了校验", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("错误 = %v, 期望包含 %q", err, tc.wantErr)
			}
		})
	}
}

// 缺少 Linear / GitHub 凭据不应阻止启动：只跑 migrate 或只提供 UI 时无需外部凭据。
func TestValidateAllowsMissingCredentials(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load() 报错: %v", err)
	}
	c.LinearToken = ""
	c.GitHubToken = ""
	if err := c.Validate(); err != nil {
		t.Errorf("缺少凭据时 Validate() 应通过，得到: %v", err)
	}
}

func TestRedactedHidesSecrets(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load() 报错: %v", err)
	}
	c.LinearToken = "lin_api_supersecret"
	c.GitHubToken = "ghp_supersecret"

	got := c.Redacted()
	for _, secret := range []string{"lin_api_supersecret", "ghp_supersecret"} {
		if strings.Contains(got, secret) {
			t.Errorf("Redacted() 泄漏了密钥 %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "<set>") {
		t.Errorf("Redacted() 应把已设置的密钥标为 <set>: %s", got)
	}

	c.LinearToken = ""
	if !strings.Contains(c.Redacted(), "<unset>") {
		t.Errorf("Redacted() 应把未设置的密钥标为 <unset>: %s", c.Redacted())
	}
}

func TestDSN(t *testing.T) {
	d := Database{
		Host: "db.example", Port: 5432, User: "u", Password: "p",
		Name: "lathe", SSLMode: "require",
	}
	want := "postgres://u:p@db.example:5432/lathe?sslmode=require"
	if got := d.DSN(); got != want {
		t.Errorf("DSN() = %q, 期望 %q", got, want)
	}
}
