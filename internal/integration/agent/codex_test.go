package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// codex_test.go —— codex 驱动：解析、参数、env/Dir、进程监管（与 claude 同一组断言）。

const (
	codexThreadLine    = `{"type":"thread.started","thread_id":"019a-thread"}`
	codexTurnStarted   = `{"type":"turn.started"}`
	codexMsgLine       = `{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"改完了"}}`
	codexTurnDoneLine  = `{"type":"turn.completed","usage":{"input_tokens":11,"cached_input_tokens":0,"output_tokens":3}}`
	codexTurnFailLine  = `{"type":"turn.failed","error":{"message":"stream disconnected before completion"}}`
	codexTopErrorLine  = `{"type":"error","message":"Reconnecting... 1/5"}`
	codexUnknownItem   = `{"type":"item.completed","item":{"id":"item_9","type":"brand_new_thing","foo":1}}`
	codexCmdStartLine  = `{"type":"item.started","item":{"id":"item_2","type":"command_execution","command":"/bin/bash -lc 'go test ./...'","aggregated_output":"","exit_code":null,"status":"in_progress"}}`
	codexCmdDoneLine   = `{"type":"item.completed","item":{"id":"item_2","type":"command_execution","command":"/bin/bash -lc 'go test ./...'","aggregated_output":"ok\n","exit_code":0,"status":"completed"}}`
	codexCmdFailedLine = `{"type":"item.completed","item":{"id":"item_3","type":"command_execution","command":"/bin/bash -lc 'false'","aggregated_output":"","exit_code":2,"status":"failed"}}`
)

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读 testdata/%s 失败: %v", name, err)
	}
	return string(b)
}

// parseCodexForTest 直接跑 codex 的 parse，收集事件与 OnSession 回调。
func parseCodexForTest(stream string) (*Result, []Event, []string, *codexParseState, error) {
	var evs []Event
	var sessions []string
	st := &codexParseState{start: time.Now(), onSession: func(id string) { sessions = append(sessions, id) }}
	res, err := st.parse(strings.NewReader(stream), parseHooks{
		Touch: func() {},
		Emit:  func(e Event) { evs = append(evs, e) },
	})
	return res, evs, sessions, st, err
}

// codexTestHome 给经 CodexDriver 起**假脚本**的测试一个专用 CODEX_HOME：
// root/ch 不在子进程的 TMPDIR（root/tmp）之下，起进程前的临时目录复查放行
// （P17 的 F 组）。只用于假脚本；真实 codex 的测试把 home 建在证据目录下。
func codexTestHome(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	ch, tmp := filepath.Join(root, "ch"), filepath.Join(root, "tmp")
	for _, d := range []string{ch, tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMPDIR", tmp)
	return ch
}

func fakeCodex(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("#!/bin/bash\n"+script), 0o755); err != nil {
		t.Fatalf("写假 codex 失败: %v", err)
	}
	return path
}

func newTestCodex(t *testing.T, bin string, timeout time.Duration) *CodexDriver {
	t.Helper()
	d := NewCodexDriver(bin, codexTestHome(t), timeout)
	return d
}

// ---------------------------------------------------------------- 解析

func TestCodexParseStream(t *testing.T) {
	cases := []struct {
		name        string
		stream      string
		wantNil     bool
		wantSuccess bool
		wantIsError bool
		wantSubtype string
		wantText    string
		wantSession string
		wantTurns   int
		// wantTypes 是按顺序期望的 Event.Type（nil = 不检查）
		wantTypes []EventType
	}{
		{
			name:        "给定 probe1 真实输出 断言 init 事件带 thread_id 且终局成功取最后一条 agent_message",
			stream:      readTestdata(t, "codex_probe1.jsonl"),
			wantSuccess: true, wantSubtype: "success", wantText: "F06STUBOK",
			wantSession: "01a1c9c0-5d69-7233-a188-0e41e191b3ee", wantTurns: 1,
			wantTypes: []EventType{EventSystem, EventOther, EventOther, EventAssistant, EventResult},
		},
		{
			name:        "给定 probe2 工具调用 断言 command_execution 发起是 assistant 结果是 user",
			stream:      readTestdata(t, "codex_probe2.jsonl"),
			wantSuccess: true, wantSubtype: "success", wantText: "DONE-F06",
			wantSession: "01a1c9c0-72ef-7703-851c-4e2430beec38", wantTurns: 1,
			wantTypes: []EventType{EventSystem, EventOther, EventOther, EventAssistant, EventUser, EventAssistant, EventResult},
		},
		{
			name:        "给定 turn.failed 断言 IsError 且 Text 是 error.message",
			stream:      strings.Join([]string{codexThreadLine, codexTurnStarted, codexMsgLine, codexTurnFailLine}, "\n"),
			wantIsError: true, wantSubtype: "error_turn_failed",
			wantText: "stream disconnected before completion", wantSession: "019a-thread", wantTurns: 1,
		},
		{
			name: "给定顶层 error 行、不认识的 item、坏 JSON 和噪声 断言不报错照常取终局",
			stream: strings.Join([]string{"Reading prompt from stdin...", codexThreadLine, codexTopErrorLine,
				`{"type":"item.completed","item":`, codexUnknownItem, codexMsgLine, codexTurnDoneLine}, "\n"),
			wantSuccess: true, wantSubtype: "success", wantText: "改完了", wantSession: "019a-thread", wantTurns: 1,
			wantTypes: []EventType{EventSystem, EventOther, EventOther, EventAssistant, EventResult},
		},
		{
			name:    "给定只有 turn.started 断言解析结果为 nil（由 supervise 报 ErrNoResultEvent）",
			stream:  strings.Join([]string{codexThreadLine, codexTurnStarted}, "\n"),
			wantNil: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, evs, _, _, err := parseCodexForTest(tc.stream)
			if err != nil {
				t.Fatalf("parse 不应报错，got %v", err)
			}
			if tc.wantNil {
				if res != nil {
					t.Fatalf("没有终局事件时 result 应为 nil，got %+v", res)
				}
				return
			}
			if res == nil {
				t.Fatal("result 不应为 nil")
			}
			if res.Success != tc.wantSuccess || res.IsError != tc.wantIsError || res.Subtype != tc.wantSubtype {
				t.Errorf("Success/IsError/Subtype got %v/%v/%q want %v/%v/%q",
					res.Success, res.IsError, res.Subtype, tc.wantSuccess, tc.wantIsError, tc.wantSubtype)
			}
			if res.Text != tc.wantText {
				t.Errorf("Text got %q want %q", res.Text, tc.wantText)
			}
			if res.SessionID != tc.wantSession {
				t.Errorf("SessionID got %q want %q", res.SessionID, tc.wantSession)
			}
			if res.NumTurns != tc.wantTurns {
				t.Errorf("NumTurns got %d want %d", res.NumTurns, tc.wantTurns)
			}
			if res.CostUSD != 0 {
				t.Errorf("codex 不报费用，CostUSD got %v want 0", res.CostUSD)
			}
			for _, e := range evs {
				if e.Driver != DriverCodex {
					t.Errorf("每条事件的 Driver 都应是 codex，got %q", e.Driver)
				}
			}
			if tc.wantTypes != nil {
				var got []EventType
				for _, e := range evs {
					got = append(got, e.Type)
				}
				if !slices.Equal(got, tc.wantTypes) {
					t.Errorf("事件类型序列 got %v want %v", got, tc.wantTypes)
				}
			}
			if len(evs) > 0 && evs[0].Subtype == "init" && evs[0].SessionID != tc.wantSession {
				t.Errorf("init 事件 SessionID got %q want %q", evs[0].SessionID, tc.wantSession)
			}
		})
	}

	t.Run("给定 resume 时再次出现同一 thread.started 断言 OnSession 只回调一次", func(t *testing.T) {
		_, _, sessions, _, err := parseCodexForTest(strings.Join([]string{codexThreadLine, codexThreadLine, codexMsgLine, codexTurnDoneLine}, "\n"))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(sessions, []string{"019a-thread"}) {
			t.Errorf("OnSession 回调 got %v want [019a-thread]", sessions)
		}
	})

	t.Run("给定 CLI 只吐 turn.started 就退出 断言 Run 返回 ErrNoResultEvent", func(t *testing.T) {
		bin := fakeCodex(t, fmt.Sprintf("cat >/dev/null\necho '%s'\necho '%s'\n", codexThreadLine, codexTurnStarted))
		d := newTestCodex(t, bin, 30*time.Second)
		_, err := d.Run(context.Background(), RunParams{Prompt: "x", Dir: t.TempDir()})
		if !errors.Is(err, ErrNoResultEvent) {
			t.Fatalf("got %v want ErrNoResultEvent", err)
		}
	})
}

// f06 先合入时给 f03 留好的接口：解析状态按 turn 收集 usage 原文。
func TestCodexParseCollectsTurnUsage(t *testing.T) {
	usageOf := func(line string) string {
		i := strings.Index(line, `"usage":`)
		if i < 0 {
			t.Fatalf("样本行里没有 usage: %s", line)
		}
		return strings.TrimSuffix(line[i+len(`"usage":`):], "}")
	}
	p1, p2 := readTestdata(t, "codex_probe1.jsonl"), readTestdata(t, "codex_probe2.jsonl")
	last := func(s string) string {
		lines := strings.Split(strings.TrimSpace(s), "\n")
		return lines[len(lines)-1]
	}
	cases := []struct {
		name   string
		stream string
		want   []string
	}{
		{"给定 probe1+probe2 两条 turn.completed 断言逐字节收集到 2 个 usage",
			p1 + p2, []string{usageOf(last(p1)), usageOf(last(p2))}},
		{"给定只有 turn.failed 断言收集到 0 个 usage",
			strings.Join([]string{codexThreadLine, codexTurnFailLine}, "\n"), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, st, err := parseCodexForTest(tc.stream)
			if err != nil {
				t.Fatal(err)
			}
			if len(st.usages) != len(tc.want) {
				t.Fatalf("usage 个数 got %d want %d", len(st.usages), len(tc.want))
			}
			for i := range tc.want {
				if string(st.usages[i]) != tc.want[i] {
					t.Errorf("usage[%d] got %s want %s", i, st.usages[i], tc.want[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------- 参数

func TestCodexBuildArgs(t *testing.T) {
	const dir = "/work/tree"
	const prompt = "请修复 PROMPT-MARKER-不该进 argv"
	cases := []struct {
		name    string
		p       RunParams
		want    []string // 完全相等（nil = 不检查）
		check   func(t *testing.T, args []string)
		wantErr bool
	}{
		{
			name: "给定新会话+acceptEdits 断言参数完全等于约定序列",
			p:    RunParams{Prompt: prompt, Dir: dir, SessionID: "pre-uuid", PermissionMode: "acceptEdits"},
			want: []string{"exec", "--json", "--skip-git-repo-check", "-C", dir,
				"-c", `approval_policy="never"`, "-c", `sandbox_mode="workspace-write"`,
				"-c", "sandbox_workspace_write.network_access=true",
				"-c", `project_doc_fallback_filenames=["CLAUDE.md"]`, "-"},
		},
		{
			name: "给定 plan 断言只读沙箱且不含 workspace-write",
			p:    RunParams{Prompt: prompt, Dir: dir, PermissionMode: "plan"},
			check: func(t *testing.T, args []string) {
				if !slices.Contains(args, `sandbox_mode="read-only"`) {
					t.Errorf("plan 应含 read-only，got %v", args)
				}
				if strings.Contains(strings.Join(args, " "), "workspace-write") {
					t.Errorf("plan 不应含 workspace-write，got %v", args)
				}
			},
		},
		{
			name: "给定 Resume 断言以 exec resume --json 开头、倒数第二是会话 ID、最后是 - 且没有 -C",
			p:    RunParams{Prompt: prompt, Dir: dir, SessionID: "019a-thread", Resume: true, PermissionMode: "acceptEdits", ExtraArgs: []string{"-m", "x"}},
			check: func(t *testing.T, args []string) {
				if !slices.Equal(args[:4], []string{"exec", "resume", "--json", "--skip-git-repo-check"}) {
					t.Errorf("开头 got %v", args[:4])
				}
				if args[len(args)-2] != "019a-thread" || args[len(args)-1] != "-" {
					t.Errorf("末两位 got %v", args[len(args)-2:])
				}
				if slices.Contains(args, "-C") {
					t.Errorf("resume 不应含 -C，got %v", args)
				}
				if i := slices.Index(args, "-m"); i < 0 || i > len(args)-3 {
					t.Errorf("ExtraArgs 应在会话 ID 之前，got %v", args)
				}
			},
		},
		{
			name: "给定 ExtraArgs 断言原样追加在 - 之前",
			p:    RunParams{Prompt: prompt, Dir: dir, ExtraArgs: []string{"-m", "gpt-x"}},
			check: func(t *testing.T, args []string) {
				if !slices.Equal(args[len(args)-3:], []string{"-m", "gpt-x", "-"}) {
					t.Errorf("末尾 got %v", args[len(args)-3:])
				}
			},
		},
		{name: "给定 FromPR 断言报错", p: RunParams{Prompt: prompt, Dir: dir, FromPR: "12"}, wantErr: true},
		{name: "给定未知 PermissionMode 断言报错", p: RunParams{Prompt: prompt, Dir: dir, PermissionMode: "weird"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, err := codexBuildArgs(tc.p)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应报错，got args %v", args)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if tc.want != nil && !slices.Equal(args, tc.want) {
				t.Errorf("args\n got %q\nwant %q", args, tc.want)
			}
			if tc.check != nil {
				tc.check(t, args)
			}
			for _, a := range args {
				if strings.Contains(a, "PROMPT-MARKER") {
					t.Errorf("prompt 不应出现在 argv 中（只走 stdin），got %q", a)
				}
			}
			if slices.Contains(args, "--ignore-user-config") {
				t.Errorf("args 永远不应含 --ignore-user-config，got %v", args)
			}
		})
	}
}

// ---------------------------------------------------------------- env / Dir

func envLookup(env []string, key string) (string, int) {
	val, n := "", 0
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			val, n = kv[len(key)+1:], n+1
		}
	}
	return val, n
}

func TestCodexRunEnvAndDir(t *testing.T) {
	t.Run("给定敏感变量与 ExtraEnv 覆盖企图 断言白名单生效、CODEX_HOME/HOME 强制且各一次、Dir 与 stdin 正确", func(t *testing.T) {
		out := t.TempDir()
		bin := fakeCodex(t, fmt.Sprintf(`
env > %[1]s/env.txt
pwd > %[1]s/pwd.txt
cat > %[1]s/stdin.txt
echo '%[2]s'
`, out, codexTurnDoneLine))
		for k, v := range map[string]string{
			"LATHE_ADMIN_TOKEN": "adm-secret", "LATHE_DB_PASSWORD": "db-secret",
			"LATHE_GITHUB_TOKEN": "ghp_secret", "OPENAI_API_KEY": "sk-secret", "CLAUDECODE": "1",
		} {
			t.Setenv(k, v)
		}
		t.Setenv("HOME", t.TempDir())
		d := newTestCodex(t, bin, 30*time.Second)
		wt := t.TempDir()
		const prompt = "多行\nprompt 走 stdin"
		if _, err := d.Run(context.Background(), RunParams{
			Prompt: prompt, Dir: wt, ExtraEnv: []string{"CODEX_HOME=/evil", "HOME=/evil", "LATHE_AGENT_CHANNEL=x"},
		}); err != nil {
			t.Fatalf("Run 报错: %v", err)
		}
		raw, err := os.ReadFile(filepath.Join(out, "env.txt"))
		if err != nil {
			t.Fatal(err)
		}
		env := strings.Split(strings.TrimSpace(string(raw)), "\n")
		for _, k := range []string{"LATHE_ADMIN_TOKEN", "LATHE_DB_PASSWORD", "LATHE_GITHUB_TOKEN", "OPENAI_API_KEY", "CLAUDECODE"} {
			if _, n := envLookup(env, k); n != 0 {
				t.Errorf("%s 不应进子进程环境", k)
			}
		}
		if v, n := envLookup(env, "CODEX_HOME"); n != 1 || v != d.home {
			t.Errorf("CODEX_HOME got %q（出现 %d 次） want %q（1 次）", v, n, d.home)
		}
		wantHome := filepath.Join(d.home, "home")
		if v, n := envLookup(env, "HOME"); n != 1 || v != wantHome {
			t.Errorf("HOME got %q（出现 %d 次） want %q（1 次）", v, n, wantHome)
		}
		if fi, err := os.Stat(wantHome); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Errorf("子进程 HOME 目录应已建出且权限 0700，got %v %v", fi, err)
		}
		if _, n := envLookup(env, "PATH"); n == 0 {
			t.Error("PATH 应在子进程环境里")
		}
		if v, _ := envLookup(env, "LATHE_AGENT_CHANNEL"); v != "x" {
			t.Errorf("ExtraEnv 的普通变量应到达子进程，got %q", v)
		}
		pwd, _ := os.ReadFile(filepath.Join(out, "pwd.txt"))
		gotDir, _ := filepath.EvalSymlinks(strings.TrimSpace(string(pwd)))
		wantDir, _ := filepath.EvalSymlinks(wt)
		if gotDir != wantDir {
			t.Errorf("pwd got %q want %q", gotDir, wantDir)
		}
		stdin, _ := os.ReadFile(filepath.Join(out, "stdin.txt"))
		if string(stdin) != prompt {
			t.Errorf("stdin got %q want %q", stdin, prompt)
		}
	})

	t.Run("给定 ExtraEnv 把 TMPDIR 改到 CODEX_HOME 的上级 断言 ErrDriverUnavailable 且不起进程", func(t *testing.T) {
		B := t.TempDir()
		ch := filepath.Join(B, "ch")
		if err := os.MkdirAll(ch, 0o755); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(t.TempDir(), "started")
		bin := fakeCodex(t, fmt.Sprintf("touch %s\necho '%s'\n", marker, codexTurnDoneLine))
		d := NewCodexDriver(bin, ch, 30*time.Second)
		_, err := d.Run(context.Background(), RunParams{Prompt: "x", Dir: t.TempDir(), ExtraEnv: []string{"TMPDIR=" + B}})
		if !errors.Is(err, ErrDriverUnavailable) {
			t.Fatalf("got %v want ErrDriverUnavailable", err)
		}
		if !strings.Contains(err.Error(), "Refusing to create helper binaries") {
			t.Errorf("错误应带 codex 原话，got %v", err)
		}
		if _, serr := os.Stat(marker); serr == nil {
			t.Error("不应起进程（标记文件不该存在）")
		}
	})

	t.Run("给定 ExtraEnv 的 TMPDIR 在 CODEX_HOME 之外 断言正常运行且子进程看到该 TMPDIR", func(t *testing.T) {
		B := t.TempDir()
		ch := filepath.Join(B, "ch")
		if err := os.MkdirAll(ch, 0o755); err != nil {
			t.Fatal(err)
		}
		other := t.TempDir()
		out := t.TempDir()
		marker := filepath.Join(out, "started")
		bin := fakeCodex(t, fmt.Sprintf("echo \"$TMPDIR\" > %s\necho '%s'\n", marker, codexTurnDoneLine))
		d := NewCodexDriver(bin, ch, 30*time.Second)
		if _, err := d.Run(context.Background(), RunParams{Prompt: "x", Dir: t.TempDir(), ExtraEnv: []string{"TMPDIR=" + other}}); err != nil {
			t.Fatalf("Run 报错: %v", err)
		}
		got, err := os.ReadFile(marker)
		if err != nil {
			t.Fatalf("应起进程（标记文件应存在）: %v", err)
		}
		if strings.TrimSpace(string(got)) != other {
			t.Errorf("子进程 TMPDIR got %q want %q", strings.TrimSpace(string(got)), other)
		}
	})
}

// ---------------------------------------------------------------- 进程监管

func waitGone(t *testing.T, pidFile string, within time.Duration) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Skipf("假脚本未写出 pid，跳过回收检查: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Skipf("pid 不可解析: %v", err)
	}
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("进程 %d 仍存活 —— 进程树未被完整回收", pid)
}

func TestCodexRunTimeout(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	bin := fakeCodex(t, fmt.Sprintf("sleep 120 &\necho $! > %s\nsleep 120\n", pidFile))
	d := newTestCodex(t, bin, 800*time.Millisecond)
	start := time.Now()
	_, err := d.Run(context.Background(), RunParams{Prompt: "慢活", Dir: t.TempDir()})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "执行超时") {
		t.Fatalf("应报总超时（DeadlineExceeded + 「执行超时」），got %v", err)
	}
	if errors.Is(err, ErrIdle) {
		t.Errorf("总超时不该被报成卡死: %v", err)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Errorf("超时后应迅速返回，实际 %v", el)
	}
	waitGone(t, pidFile, 5*time.Second)
}

func TestCodexRunIdle(t *testing.T) {
	bin := fakeCodex(t, fmt.Sprintf("echo '%s'\nsleep 60\n", codexThreadLine))
	d := newTestCodex(t, bin, 30*time.Second)
	d.idle = 500 * time.Millisecond
	start := time.Now()
	_, err := d.Run(context.Background(), RunParams{Prompt: "卡住", Dir: t.TempDir()})
	if !errors.Is(err, ErrIdle) {
		t.Fatalf("应报 ErrIdle，got %v", err)
	}
	if strings.Contains(err.Error(), "执行超时") {
		t.Errorf("卡死不该报成总超时: %v", err)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Errorf("静默判死后应迅速返回，实际 %v", el)
	}
}

func TestCodexRunContextCancel(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "self.pid")
	bin := fakeCodex(t, fmt.Sprintf("echo $$ > %s\nsleep 120\n", pidFile))
	d := newTestCodex(t, bin, 30*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := d.Run(ctx, RunParams{Prompt: "慢活", Dir: t.TempDir()})
	if err == nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "取消") {
		t.Fatalf("应报被取消，got %v", err)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Errorf("取消后应迅速返回，实际 %v", el)
	}
	waitGone(t, pidFile, 5*time.Second)
}

func TestCodexRunBackgroundChild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "bg.pid")
	bin := fakeCodex(t, fmt.Sprintf("sleep 120 &\necho $! > %s\necho '%s'\necho '%s'\nexit 0\n",
		pidFile, codexMsgLine, codexTurnDoneLine))
	d := newTestCodex(t, bin, 20*time.Second)
	start := time.Now()
	res, err := d.Run(context.Background(), RunParams{Prompt: "x", Dir: t.TempDir()})
	el := time.Since(start)
	if err != nil {
		t.Fatalf("已给出成功终局，不该报错: %v（耗时 %v）", err, el)
	}
	if res == nil || !res.Success || res.Text != "改完了" {
		t.Errorf("结果不符: %+v", res)
	}
	if el > 10*time.Second {
		t.Errorf("应在宽限期内返回，实际 %v", el)
	}
	waitGone(t, pidFile, 3*time.Second)
}

func TestCodexRunEscapedChild(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skipf("本机没有 setsid: %v", err)
	}
	pidFile := filepath.Join(t.TempDir(), "escaped.pid")
	bin := fakeCodex(t, fmt.Sprintf(`
setsid sh -c 'echo $$ > %s; exec sleep 120' &
sleep 0.2
echo '%s'
exit 0
`, pidFile, codexTurnDoneLine))
	t.Cleanup(func() {
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	d := newTestCodex(t, bin, 60*time.Second)
	start := time.Now()
	res, err := d.Run(context.Background(), RunParams{Prompt: "x", Dir: t.TempDir()})
	el := time.Since(start)
	if err != nil {
		t.Fatalf("终局已到手，强关读端不该算失败: %v（耗时 %v）", err, el)
	}
	if res == nil || !res.Success {
		t.Errorf("结果不符: %+v", res)
	}
	if el < pipeDrainGrace || el > pipeDrainGrace+5*time.Second {
		t.Errorf("应在宽限期 %v 后不久返回，实际 %v", pipeDrainGrace, el)
	}
}

func TestCodexRunStderrBounded(t *testing.T) {
	bin := fakeCodex(t, `
echo 'STDERR-HEAD' >&2
head -c 8000000 /dev/zero | tr '\0' 'x' >&2
echo >&2
echo 'STDERR-TAIL' >&2
exit 3
`)
	d := newTestCodex(t, bin, 30*time.Second)
	_, err := d.Run(context.Background(), RunParams{Prompt: "x", Dir: t.TempDir()})
	if err == nil {
		t.Fatal("没有终局事件应报错")
	}
	msg := err.Error()
	if len(msg) > 8<<10 {
		t.Errorf("错误信息应有界，实际 %d 字节", len(msg))
	}
	for _, want := range []string{"STDERR-HEAD", "STDERR-TAIL"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应同时带上 stderr 的开头与结尾（缺 %s）", want)
		}
	}
}

// P14：turn.failed 时 codex 以 1 退出。有错误终局时以终局为准，不再额外报错。
func TestCodexRunNonZeroExitWithTurnFailed(t *testing.T) {
	cases := []struct {
		name    string
		script  string
		wantErr bool
		wantRes func(t *testing.T, r *Result)
	}{
		{
			name:   "给定 turn.failed 且退出码 1 断言返回错误终局、不额外报错",
			script: fmt.Sprintf("echo '%s'\necho '%s'\nexit 1\n", codexThreadLine, codexTurnFailLine),
			wantRes: func(t *testing.T, r *Result) {
				if r == nil || !r.IsError || r.Subtype != "error_turn_failed" || r.Text != "stream disconnected before completion" {
					t.Errorf("got %+v", r)
				}
			},
		},
		{
			name:    "给定 turn.completed 但退出码 1 断言报非零退出",
			script:  fmt.Sprintf("echo '%s'\nexit 1\n", codexTurnDoneLine),
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestCodex(t, fakeCodex(t, tc.script), 30*time.Second)
			res, err := d.Run(context.Background(), RunParams{Prompt: "x", Dir: t.TempDir()})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err got %v wantErr %v", err, tc.wantErr)
			}
			if tc.wantRes != nil {
				tc.wantRes(t, res)
			}
		})
	}
}

// ---------------------------------------------------------------- 字段表态

// setNonZero 把字段设成一个非零值（只用于「只设这一个字段」的拒绝用例）。
func setNonZero(t *testing.T, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString("1")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64, reflect.Int32:
		v.SetInt(1)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
	case reflect.Func:
		v.Set(reflect.MakeFunc(v.Type(), func(args []reflect.Value) []reflect.Value {
			out := make([]reflect.Value, v.Type().NumOut())
			for i := range out {
				out[i] = reflect.Zero(v.Type().Out(i))
			}
			return out
		}))
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
	default:
		t.Fatalf("不会给 %s 类型的字段设非零值，请补 setNonZero", v.Kind())
	}
}

func TestCodexHandlesEveryRunParamsField(t *testing.T) {
	known := map[string]string{}
	for _, f := range codexConsumes {
		known[f] = "consumes"
	}
	for _, f := range codexRejects {
		if _, dup := known[f]; dup {
			t.Errorf("字段 %s 同时出现在 codexConsumes 与 codexRejects", f)
		}
		known[f] = "rejects"
	}
	rt := reflect.TypeOf(RunParams{})
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if _, ok := known[name]; !ok {
			t.Errorf("RunParams.%s 没有在 codex 驱动里表态：放进 codexConsumes（并实现）或 codexRejects（并在 Capabilities/requires 里登记）", name)
		}
	}

	for _, name := range codexRejects {
		t.Run("给定只设了 "+name+" 断言 Router 返回 ErrUnsupportedParam 且不起进程", func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "started")
			bin := fakeCodex(t, fmt.Sprintf("touch %s\necho '%s'\n", marker, codexTurnDoneLine))
			r := NewRouter(NewDriver(bin, time.Minute), newTestCodex(t, bin, time.Minute))
			p := RunParams{Driver: DriverCodex, Prompt: "x", Dir: t.TempDir()}
			fv := reflect.ValueOf(&p).Elem().FieldByName(name)
			if !fv.IsValid() {
				t.Fatalf("codexRejects 里的 %s 不是 RunParams 的字段", name)
			}
			setNonZero(t, fv)
			_, err := r.Run(context.Background(), p)
			if !errors.Is(err, ErrUnsupportedParam) {
				t.Fatalf("got %v want ErrUnsupportedParam", err)
			}
			if _, serr := os.Stat(marker); serr == nil {
				t.Error("不应起进程")
			}
		})
	}
}
