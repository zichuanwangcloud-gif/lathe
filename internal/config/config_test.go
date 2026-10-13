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

// ★ B4：TTL 下限。
//
// TTL 不只是「保留多久」的策略，它还是孤儿清扫那层「新建目录绝不动」的
// 安全机制：Create 建出目录到把路径写进任务行之间有一个无人认领的窗口，
// 只有 mtime 比 cutoff 老才会被删。TTL 配成分钟级，在途任务的现场就会
// 落进那个窗口。「先清一批存量目录」是自然的运维动作，但正确做法是
// LATHE_REAP_DRY_RUN，不是把 TTL 拧小。
func TestValidateRejectsTinyWorktreeTTL(t *testing.T) {
	base := func() Config {
		c, err := Load()
		if err != nil {
			t.Fatalf("基准配置构造失败: %v", err)
		}
		return c
	}

	for _, ttl := range []time.Duration{time.Minute, 59 * time.Minute, time.Second, 0, -time.Hour} {
		c := base()
		c.WorktreeTTL = ttl
		err := c.Validate()
		if err == nil {
			t.Errorf("TTL=%v 应被拒绝（下限 %v）", ttl, MinWorktreeTTL)
			continue
		}
		if !strings.Contains(err.Error(), "WorktreeTTL") {
			t.Errorf("TTL=%v 的错误应提到 WorktreeTTL，得到: %v", ttl, err)
		}
		// 错误信息要指路，否则运维只会去改代码里的下限
		if !strings.Contains(err.Error(), "LATHE_REAP_DRY_RUN") {
			t.Errorf("TTL 下限的错误应提示用干跑代替，得到: %v", err)
		}
	}

	// 正好等于下限：放行
	c := base()
	c.WorktreeTTL = MinWorktreeTTL
	if err := c.Validate(); err != nil {
		t.Errorf("TTL 恰好等于下限时应放行，得到: %v", err)
	}
}

// LATHE_WORKTREE_TTL=0 曾经不是关停而是回落 72h —— 现在关停靠
// LATHE_REAP_ENABLED，而 TTL=0 直接报错（envDuration 解析 "0s" 得到 0）。
func TestLoadRejectsZeroTTLEnv(t *testing.T) {
	t.Setenv("LATHE_WORKTREE_TTL", "0s")
	if _, err := Load(); err == nil {
		t.Error("LATHE_WORKTREE_TTL=0s 应报错（关停请用 LATHE_REAP_ENABLED=false）")
	}
}

// ReapInterval 必须为正。
func TestValidateRejectsNonPositiveReapInterval(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load() 报错: %v", err)
	}
	for _, iv := range []time.Duration{0, -time.Second} {
		c.ReapInterval = iv
		if verr := c.Validate(); verr == nil || !strings.Contains(verr.Error(), "ReapInterval") {
			t.Errorf("ReapInterval=%v 应被拒绝，得到: %v", iv, verr)
		}
	}
}

// ★ B4：收割机的关停与干跑开关。
func TestReapSwitches(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load() 报错: %v", err)
	}
	// 默认：开启、不干跑
	if !c.ReapingEnabled() {
		t.Error("默认应启用收割机")
	}
	if c.ReapingDryRun() {
		t.Error("默认不该是干跑模式")
	}

	c.ReapEnabled = "false"
	if c.ReapingEnabled() {
		t.Error("LATHE_REAP_ENABLED=false 应关停收割机")
	}
	c.ReapDryRun = "true"
	if !c.ReapingDryRun() {
		t.Error("LATHE_REAP_DRY_RUN=true 应进入干跑模式")
	}
}

// 布尔开关拼错要在启动时炸，而不是静默失效。
//
// 「以为关了其实还在删」是这个组件最不能接受的失效方式。
func TestValidateRejectsMalformedReapSwitches(t *testing.T) {
	for _, tc := range []struct{ field, value, want string }{
		{"enabled", "yes", "LATHE_REAP_ENABLED"},
		{"enabled", "1", "LATHE_REAP_ENABLED"},
		{"enabled", "TRUE", "LATHE_REAP_ENABLED"},
		{"dryrun", "on", "LATHE_REAP_DRY_RUN"},
	} {
		c, err := Load()
		if err != nil {
			t.Fatalf("Load() 报错: %v", err)
		}
		if tc.field == "enabled" {
			c.ReapEnabled = tc.value
		} else {
			c.ReapDryRun = tc.value
		}
		verr := c.Validate()
		if verr == nil {
			t.Errorf("%s=%q 应被拒绝（静默失效意味着以为关了其实还在删）", tc.want, tc.value)
			continue
		}
		if !strings.Contains(verr.Error(), tc.want) {
			t.Errorf("错误应提到 %s，得到: %v", tc.want, verr)
		}
	}
}

// ★ WorkspaceRoot 专用目录校验。
//
// 只校验非空 + 绝对路径是不够的：配成 "/" 或 "/opt" 时，孤儿清扫会把
// 根下所有「非 . 开头、无人认领、mtime 超期」的顶层目录都删掉。
func TestValidateRejectsDangerousWorkspaceRoot(t *testing.T) {
	dangerous := []string{
		"/", "/opt", "/home", "/var", "/tmp", "/usr", "/etc", "/root",
		"/opt/", "//", "/opt/.", // 清理后仍是黑名单里的值
		"/lathe", // 只有一层：根下直接放的就是系统级目录
	}
	for _, root := range dangerous {
		c, err := Load()
		if err != nil {
			t.Fatalf("Load() 报错: %v", err)
		}
		c.WorkspaceRoot = root
		if verr := c.Validate(); verr == nil {
			t.Errorf("WorkspaceRoot=%q 应被拒绝（收割机会清掉根下无人认领的顶层目录）", root)
		} else if !strings.Contains(verr.Error(), "WorkspaceRoot") {
			t.Errorf("WorkspaceRoot=%q 的错误应提到 WorkspaceRoot，得到: %v", root, verr)
		}
	}

	// 正常的两层及以上路径放行
	for _, root := range []string{"/opt/lathe/workspaces", "/data/lathe", "/srv/lathe/wt"} {
		c, err := Load()
		if err != nil {
			t.Fatalf("Load() 报错: %v", err)
		}
		c.WorkspaceRoot = root
		if verr := c.Validate(); verr != nil {
			t.Errorf("WorkspaceRoot=%q 应放行，得到: %v", root, verr)
		}
	}
}

// ★ 验收标准：PRD 搁置阈值的下限与默认值（docs/10 §9 第 1 条）。
//
// 下限的存在理由与 WorktreeTTL 不同：那边是安全机制（防误删在途现场），
// 这边纯粹是防手滑 —— 调小不会损坏任何数据，只会把还在等人回答的 PRD
// 判成「已搁置」。那是一次**可见的误判**（人在列表上看到自己的对话被
// 放弃了），比数据损坏更难修：数据能恢复，信任不能。
func TestValidateRejectsTinyPRDStallTimeout(t *testing.T) {
	base := func() Config {
		c, err := Load()
		if err != nil {
			t.Fatalf("基准配置构造失败: %v", err)
		}
		return c
	}

	for _, d := range []time.Duration{time.Second, time.Minute, 59 * time.Minute, 0, -time.Hour} {
		c := base()
		c.PRDStallTimeout = d
		err := c.Validate()
		if err == nil {
			t.Errorf("PRDStallTimeout=%v 应被拒绝（下限 %v）", d, MinPRDStallTimeout)
			continue
		}
		if !strings.Contains(err.Error(), "PRDStallTimeout") {
			t.Errorf("错误应提到 PRDStallTimeout，得到: %v", err)
		}
		// 错误信息要指路：Go 的时长解析不支持 d，写 7d 会静默拿不到值。
		if !strings.Contains(err.Error(), "168h") {
			t.Errorf("下限错误应提示写法（168h 而不是 7d），得到: %v", err)
		}
	}

	// 正好等于下限：放行。
	c := base()
	c.PRDStallTimeout = MinPRDStallTimeout
	if err := c.Validate(); err != nil {
		t.Errorf("恰好等于下限时应放行，得到: %v", err)
	}
}

// 默认值：七天，够人想清楚，也不至于让列表页堆满陈旧条目。
func TestPRDStallDefaults(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load() 报错: %v", err)
	}
	if c.PRDStallTimeout != 168*time.Hour {
		t.Errorf("PRDStallTimeout 默认 got=%v want=168h（7 天）", c.PRDStallTimeout)
	}
	if c.PRDStallSweepInterval != time.Hour {
		t.Errorf("PRDStallSweepInterval 默认 got=%v want=1h", c.PRDStallSweepInterval)
	}
	// 提醒默认开：搁置发生在后台，不看列表就完全无感，邮件是唯一的主动告知。
	if !c.PRDStallNotifyEnabled() {
		t.Error("搁置提醒应默认开启")
	}
}

func TestPRDStallNotifySwitch(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want bool
	}{
		{"false", false},
		{"true", true},
		{"", true}, // 未设置 = 默认开
	} {
		t.Run("LATHE_PRD_STALL_NOTIFY="+tc.env, func(t *testing.T) {
			t.Setenv("LATHE_PRD_STALL_NOTIFY", tc.env)
			c, err := Load()
			if err != nil {
				t.Fatalf("Load() 报错: %v", err)
			}
			if got := c.PRDStallNotifyEnabled(); got != tc.want {
				t.Errorf("PRDStallNotifyEnabled() got=%v want=%v", got, tc.want)
			}
		})
	}
}

// 拼错的布尔值必须在启动时报错，不能静默失效。
//
// 对 LATHE_PRD_STALL_NOTIFY 尤其要紧：写成 "no" 的话，静默按「开」处理
// 与按「关」处理都说得通，而配置的人以为自己的意思传达到了。
func TestValidateRejectsMalformedPRDStallNotify(t *testing.T) {
	for _, v := range []string{"yes", "1", "TRUE", "no"} {
		t.Run(v, func(t *testing.T) {
			c, err := Load()
			if err != nil {
				t.Fatalf("Load() 报错: %v", err)
			}
			c.PRDStallNotify = v
			verr := c.Validate()
			if verr == nil || !strings.Contains(verr.Error(), "LATHE_PRD_STALL_NOTIFY") {
				t.Errorf("LATHE_PRD_STALL_NOTIFY=%q 应在启动时报错，得到: %v", v, verr)
			}
		})
	}
}

// 扫描间隔必须为正（同 ReapInterval）。
func TestValidateRejectsNonPositivePRDStallSweepInterval(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("Load() 报错: %v", err)
	}
	for _, iv := range []time.Duration{0, -time.Second} {
		c.PRDStallSweepInterval = iv
		if verr := c.Validate(); verr == nil || !strings.Contains(verr.Error(), "PRDStallSweepInterval") {
			t.Errorf("PRDStallSweepInterval=%v 应被拒绝，得到: %v", iv, verr)
		}
	}
}

// ★ 本 PRD 新增的两个时长变量写错必须在 Load 时报错，不能静默回落默认值。
//
// 存量的 envDuration 解析失败会悄悄用默认值：`LATHE_PRD_STALL_TIMEOUT=30d`
// 能正常启动、实际生效 168h，人以为配成了 30 天。这违反「非法值启动报错」
// （AGENTS.md §3 配置行）。错误要点名变量并给出 Go duration 写法（168h）。
func TestLoadRejectsMalformedPRDStallDurations(t *testing.T) {
	for _, key := range []string{"LATHE_PRD_STALL_TIMEOUT", "LATHE_PRD_STALL_SWEEP_INTERVAL"} {
		for _, tc := range []struct {
			val     string
			wantErr bool
		}{
			{"30d", true},   // Go 不支持 d 单位
			{"abc", true},   // 根本不是时长
			{"7 d", true},   // 带空格也不行
			{"168h", false}, // 合法
		} {
			t.Run(key+"="+tc.val, func(t *testing.T) {
				t.Setenv(key, tc.val)
				c, err := Load()
				if !tc.wantErr {
					if err != nil {
						t.Fatalf("合法值 %q 不该报错，got: %v", tc.val, err)
					}
					got := c.PRDStallTimeout
					if key == "LATHE_PRD_STALL_SWEEP_INTERVAL" {
						got = c.PRDStallSweepInterval
					}
					if got != 168*time.Hour {
						t.Errorf("%s 生效值 got=%v want=168h", key, got)
					}
					return
				}
				if err == nil {
					t.Fatalf("%s=%q 应在启动时报错，却静默生效（PRDStallTimeout=%v, PRDStallSweepInterval=%v）",
						key, tc.val, c.PRDStallTimeout, c.PRDStallSweepInterval)
				}
				for _, want := range []string{key, tc.val, "168h"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("错误信息应包含 %q，got: %v", want, err)
					}
				}
			})
		}
	}
}
