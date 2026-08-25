package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------- 纯解析

const (
	sysInitLine   = `{"type":"system","subtype":"init","session_id":"sess-1","cwd":"/tmp","model":"claude-opus-5"}`
	assistantLine = `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"干活"}]},"session_id":"sess-1"}`
	resultOKLine  = `{"type":"result","subtype":"success","is_error":false,"session_id":"sess-1","result":"完成","num_turns":3,"total_cost_usd":0.1234,"duration_ms":6236,"terminal_reason":"completed","permission_denials":[]}`
	resultErrLine = `{"type":"result","subtype":"error_during_execution","is_error":true,"session_id":"sess-1","result":"炸了","num_turns":1,"terminal_reason":"error","permission_denials":[{"tool":"Bash","reason":"denied"}]}`
)

func TestParseStreamSuccess(t *testing.T) {
	stream := strings.Join([]string{sysInitLine, assistantLine, resultOKLine}, "\n")

	var seen []EventType
	res, err := parseStream(strings.NewReader(stream), func(e Event) {
		seen = append(seen, e.Type)
	})
	if err != nil {
		t.Fatalf("parseStream 报错: %v", err)
	}
	if res == nil {
		t.Fatal("应返回 result")
	}
	if !res.Success {
		t.Errorf("Success 应为 true，得到 %+v", res)
	}
	if res.SessionID != "sess-1" {
		t.Errorf("SessionID = %q，期望 sess-1", res.SessionID)
	}
	if res.Text != "完成" {
		t.Errorf("Text = %q，期望 完成", res.Text)
	}
	if res.NumTurns != 3 {
		t.Errorf("NumTurns = %d，期望 3", res.NumTurns)
	}
	if res.CostUSD != 0.1234 {
		t.Errorf("CostUSD = %v，期望 0.1234", res.CostUSD)
	}
	if res.DurationMS != 6236 {
		t.Errorf("DurationMS = %d，期望 6236", res.DurationMS)
	}

	want := []EventType{EventSystem, EventAssistant, EventResult}
	if len(seen) != len(want) {
		t.Fatalf("回调事件数 = %d，期望 %d（%v）", len(seen), len(want), seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("第 %d 个事件 = %s，期望 %s", i, seen[i], want[i])
		}
	}
}

// 权限被拒是一种独立的失败模式，必须能被识别出来。
func TestParseStreamErrorWithPermissionDenials(t *testing.T) {
	res, err := parseStream(strings.NewReader(resultErrLine), nil)
	if err != nil {
		t.Fatalf("parseStream 报错: %v", err)
	}
	if res.Success {
		t.Error("is_error=true 时 Success 应为 false")
	}
	if !res.IsError {
		t.Error("IsError 应为 true")
	}
	if res.Subtype != "error_during_execution" {
		t.Errorf("Subtype = %q", res.Subtype)
	}
	if len(res.PermissionDenials) != 1 {
		t.Errorf("PermissionDenials 应有 1 条，得到 %d", len(res.PermissionDenials))
	}
}

// 坏行、空行、非 JSON 噪声都不应终止整次执行。
func TestParseStreamToleratesGarbage(t *testing.T) {
	stream := strings.Join([]string{
		"",
		"这不是 JSON",
		`{"type":"system"`, // 截断的 JSON
		sysInitLine,
		"   ",
		resultOKLine,
	}, "\n")

	res, err := parseStream(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatalf("遇到噪声不应报错: %v", err)
	}
	if res == nil || !res.Success {
		t.Errorf("应仍能解析出成功的 result，得到 %+v", res)
	}
}

// init 事件可能极大（工具/技能/插件清单），必须超过 bufio 默认 64KB 上限。
func TestParseStreamHandlesHugeLine(t *testing.T) {
	tools := make([]string, 5000)
	for i := range tools {
		tools[i] = fmt.Sprintf("Tool_%d_with_a_fairly_long_name", i)
	}
	blob, _ := json.Marshal(map[string]any{
		"type": "system", "subtype": "init", "session_id": "sess-1", "tools": tools,
	})
	if len(blob) < 64*1024 {
		t.Fatalf("构造的行只有 %d 字节，未超过 64KB，测不到目标场景", len(blob))
	}

	stream := string(blob) + "\n" + resultOKLine
	res, err := parseStream(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatalf("大行应能解析: %v", err)
	}
	if res == nil || !res.Success {
		t.Error("大 init 行之后的 result 应被正常解析")
	}
}

func TestParseStreamNoResult(t *testing.T) {
	res, err := parseStream(strings.NewReader(sysInitLine), nil)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if res != nil {
		t.Errorf("没有 result 事件时应返回 nil，得到 %+v", res)
	}
}

// ---------------------------------------------------------------- 参数拼装

func TestBuildArgs(t *testing.T) {
	d := NewDriver("claude", time.Minute)

	cases := []struct {
		name     string
		p        RunParams
		contains []string
		absent   []string
	}{
		{
			name:     "首轮_预指定会话",
			p:        RunParams{Prompt: "修 bug", SessionID: "uuid-1"},
			contains: []string{"--print", "--output-format", "stream-json", "--session-id", "uuid-1", "修 bug"},
			absent:   []string{"--resume", "--from-pr"},
		},
		{
			name:     "续跑会话",
			p:        RunParams{Prompt: "继续", SessionID: "uuid-1", Resume: true},
			contains: []string{"--resume", "uuid-1"},
			absent:   []string{"--session-id"},
		},
		{
			name:     "按PR续跑",
			p:        RunParams{Prompt: "改 review 意见", FromPR: "2735"},
			contains: []string{"--from-pr", "2735"},
			absent:   []string{"--resume", "--session-id"},
		},
		{
			name:     "权限模式与额外参数",
			p:        RunParams{Prompt: "x", PermissionMode: "acceptEdits", ExtraArgs: []string{"--max-turns", "20"}},
			contains: []string{"--permission-mode", "acceptEdits", "--max-turns", "20"},
		},
		{
			name:     "收敛配置源\u4e3a仓库自身",
			p:        RunParams{Prompt: "x", SettingSources: "project"},
			contains: []string{"--setting-sources", "project"},
		},
		{
			name:   "未指定则不带该参数",
			p:      RunParams{Prompt: "x"},
			absent: []string{"--setting-sources"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(d.buildArgs(tc.p), " ")
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Errorf("参数应含 %q，实际: %s", want, got)
				}
			}
			for _, bad := range tc.absent {
				if strings.Contains(got, bad) {
					t.Errorf("参数不应含 %q，实际: %s", bad, got)
				}
			}
		})
	}

	// prompt 必须在最后（位置参数）
	args := d.buildArgs(RunParams{Prompt: "最后一个", SessionID: "u"})
	if args[len(args)-1] != "最后一个" {
		t.Errorf("prompt 应是最后一个参数，实际: %v", args)
	}
}

func TestRunParamValidation(t *testing.T) {
	d := NewDriver("claude", time.Minute)
	ctx := context.Background()

	cases := []struct {
		name    string
		p       RunParams
		wantErr string
	}{
		{"空prompt且非续跑", RunParams{}, "无事可做"},
		{"Resume与FromPR互斥", RunParams{Prompt: "x", Resume: true, SessionID: "s", FromPR: "1"}, "互斥"},
		{"Resume缺SessionID", RunParams{Prompt: "x", Resume: true}, "需要 SessionID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := d.Run(ctx, tc.p); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("期望错误含 %q，得到 %v", tc.wantErr, err)
			}
		})
	}
}

// 从 Claude Code 会话内启动时泄漏的环境变量必须被剔除。
// 白名单语义：敏感值（serve 进程 env 里的 token/口令）必须被挡在
// agent 子进程之外；只有操作性变量放行；LATHE_AGENT_ENV_EXTRA 是
// 显式开口的逃生门。
func TestSanitizedEnv(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	t.Setenv("LATHE_ADMIN_TOKEN", "secret-admin-token")
	t.Setenv("LATHE_KEEP_ME", "yes")
	t.Setenv("MY_CUSTOM_VAR", "custom")
	t.Setenv("LATHE_AGENT_ENV_EXTRA", "MY_CUSTOM_VAR, ,GOPROXY")

	env := sanitizedEnv()
	joined := strings.Join(env, "\n")
	for _, bad := range []string{"CLAUDECODE=", "CLAUDE_CODE_ENTRYPOINT=", "LATHE_ADMIN_TOKEN=", "LATHE_KEEP_ME=", "LATHE_AGENT_ENV_EXTRA="} {
		if strings.Contains(joined, bad) {
			t.Errorf("环境变量 %s 不应进入 agent 子进程", bad)
		}
	}
	if !strings.Contains(joined, "MY_CUSTOM_VAR=custom") {
		t.Error("LATHE_AGENT_ENV_EXTRA 显式放行的变量应保留")
	}
	// PATH/HOME 是运行基石，必须在
	if !strings.Contains(joined, "PATH=") || !strings.Contains(joined, "HOME=") {
		t.Error("PATH/HOME 必须保留")
	}
}

// 截断必须回退到 rune 边界：按字节硬切会切断多字节 UTF-8 字符，
// Postgres 拒绝非法 UTF-8（SQLSTATE 22021）导致整批事件落库失败。
func TestTruncateRuneSafe(t *testing.T) {
	// “中”是 3 字节；在第 5 字节处截断会正好切穿第二个“中”
	s := "ab中中中中"
	out := truncate(s, 5)
	if !utf8.ValidString(out) {
		t.Fatalf("截断结果不是合法 UTF-8: %q", out)
	}
	if !strings.HasSuffix(out, "…(已截断)") {
		t.Errorf("应带截断标记: %q", out)
	}
	if strings.HasPrefix(out, "ab中中") {
		t.Errorf("不应包含被切穿的字符: %q", out)
	}
}

// ---------------------------------------------------------------- 进程监管

// fakeClaude 写一个假的 claude 可执行脚本，避免测试消耗真实 API。
func fakeClaude(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/bash\n"+script), 0o755); err != nil {
		t.Fatalf("写假 claude 失败: %v", err)
	}
	return path
}

func TestRunWithFakeBinary(t *testing.T) {
	bin := fakeClaude(t, fmt.Sprintf(`
echo '%s'
echo '%s'
echo '%s'
`, sysInitLine, assistantLine, resultOKLine))

	d := NewDriver(bin, 30*time.Second)
	res, err := d.Run(context.Background(), RunParams{Prompt: "干活", SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	if !res.Success || res.Text != "完成" {
		t.Errorf("结果不符: %+v", res)
	}
}

// ExtraEnv 是调用方的显式注入（B2-2 按阶段路由通道）：必须到达子进程，
// 且与白名单同名时后者生效（子进程取最后出现的值）。
func TestRunExtraEnvInjection(t *testing.T) {
	out := filepath.Join(t.TempDir(), "env.txt")
	bin := fakeClaude(t, fmt.Sprintf(`
echo "channel=$LATHE_AGENT_CHANNEL" > %s
echo "tmpdir=$TMPDIR" >> %s
echo '%s'
`, out, out, resultOKLine))

	t.Setenv("TMPDIR", "/tmp/whitelist-value")
	d := NewDriver(bin, 30*time.Second)
	_, err := d.Run(context.Background(), RunParams{
		Prompt:    "x",
		SessionID: "s",
		ExtraEnv: []string{
			"LATHE_AGENT_CHANNEL=cheap",
			"TMPDIR=/tmp/extra-env-value",
		},
	})
	if err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("读取子进程环境输出失败: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "channel=cheap") {
		t.Errorf("ExtraEnv 应到达子进程: %q", got)
	}
	if !strings.Contains(got, "tmpdir=/tmp/extra-env-value") {
		t.Errorf("ExtraEnv 与白名单同名时应以后者为准: %q", got)
	}
}

// CLI 非零退出但给了 result 事件时，以 result 为准。
func TestRunNonZeroExitWithResult(t *testing.T) {
	bin := fakeClaude(t, fmt.Sprintf("echo '%s'\nexit 1\n", resultErrLine))

	d := NewDriver(bin, 30*time.Second)
	res, err := d.Run(context.Background(), RunParams{Prompt: "x", SessionID: "s"})
	if res == nil {
		t.Fatalf("即使非零退出也应返回已解析的 result，err=%v", err)
	}
	if !res.IsError || res.Subtype != "error_during_execution" {
		t.Errorf("应保留 result 里的错误信息: %+v", res)
	}
}

func TestRunNoResultEvent(t *testing.T) {
	bin := fakeClaude(t, "echo 'garbage'\necho 'to stderr' >&2\nexit 3\n")

	d := NewDriver(bin, 30*time.Second)
	_, err := d.Run(context.Background(), RunParams{Prompt: "x", SessionID: "s"})
	if err == nil {
		t.Fatal("没有 result 事件应报错")
	}
	if !strings.Contains(err.Error(), "result") {
		t.Errorf("错误应说明缺少 result 事件，得到: %v", err)
	}
	if !strings.Contains(err.Error(), "to stderr") {
		t.Errorf("错误应带上 stderr 便于排障，得到: %v", err)
	}
}

// ★核心安全属性：超时后整棵进程树必须被回收，不留孤儿。
//
// 假 claude 会派生一个长命孙子进程；驱动超时杀进程组后，
// 该孙子进程必须也已消失。
func TestRunTimeoutKillsEntireProcessTree(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")

	bin := fakeClaude(t, fmt.Sprintf(`
sleep 120 &
echo $! > %s
sleep 120
`, pidFile))

	d := NewDriver(bin, 800*time.Millisecond)
	start := time.Now()
	_, err := d.Run(context.Background(), RunParams{Prompt: "慢活", SessionID: "s"})
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("应因超时报错，得到: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("超时后应迅速返回，实际耗时 %v", elapsed)
	}

	raw, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Skipf("假脚本未写出孙子进程 pid，跳过孤儿检查: %v", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if convErr != nil {
		t.Skipf("孙子进程 pid 不可解析: %v", convErr)
	}

	// 给杀进程组留一点收敛时间
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return // 进程已不存在 —— 符合预期
		}
		time.Sleep(100 * time.Millisecond)
	}
	// 走到这里说明孙子进程还活着，这是必须修的孤儿泄漏
	_ = syscall.Kill(pid, syscall.SIGKILL) // 清理，避免污染测试机
	t.Errorf("孙子进程 %d 在超时后仍存活 —— 进程树未被完整回收（孤儿泄漏）", pid)
}

// 外部取消 context 也必须杀掉进程树。
func TestRunContextCancelKillsProcess(t *testing.T) {
	bin := fakeClaude(t, "sleep 120\n")

	d := NewDriver(bin, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := d.Run(ctx, RunParams{Prompt: "慢活", SessionID: "s"})
	if err == nil {
		t.Fatal("context 取消后应报错")
	}
	if !strings.Contains(err.Error(), "取消") {
		t.Errorf("错误应说明被取消，得到: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("取消后应迅速返回，实际 %v", elapsed)
	}
}
