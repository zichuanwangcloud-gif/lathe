package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// supervise_test.go —— supervise 的契约（f01 S1/S2/S4/S5 与 f03 的 DeadlineExceeded）。

func writeScript(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cli")
	if err := os.WriteFile(path, []byte("#!/bin/bash\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSuperviseStdinOwnedByDriver(t *testing.T) {
	t.Run("给定 claude 构造的 launch 断言 Stdin 为 nil（今天的行为）", func(t *testing.T) {
		l := NewDriver("claude", time.Minute).claudeLaunch(RunParams{Prompt: "x", SessionID: "s", Dir: "/w"})
		if l.Stdin != nil {
			t.Errorf("claude 的 Stdin got %T want nil", l.Stdin)
		}
		if l.Dir != "/w" {
			t.Errorf("Dir got %q want /w", l.Dir)
		}
	})

	t.Run("给定 codex 构造的 launch 断言 Stdin 读完等于 Prompt 随后 EOF", func(t *testing.T) {
		d := newTestCodex(t, "codex", time.Minute)
		l, err := d.codexLaunch(RunParams{Prompt: "提示词正文", Dir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(l.Stdin)
		if err != nil || string(b) != "提示词正文" {
			t.Fatalf("stdin got %q %v want 提示词正文", b, err)
		}
		if n, err := l.Stdin.Read(make([]byte, 1)); n != 0 || err != io.EOF {
			t.Errorf("读完后应 EOF，got n=%d err=%v", n, err)
		}
	})

	t.Run("给定 os.Pipe 读端作 Stdin 且写端一直不关 断言 supervise 不被 Wait 卡死", func(t *testing.T) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = w.Close() }() // 测试结束才关：期间故意不关
		bin := writeScript(t, fmt.Sprintf("echo '%s'\nexit 0\n", resultOKLine))
		start := time.Now()
		res, err := supervise(context.Background(),
			launch{Bin: bin, Dir: t.TempDir(), Env: os.Environ(), Stdin: r},
			superviseLimits{Timeout: 30 * time.Second, Idle: 10 * time.Second}, parseClaude, nil)
		if err != nil || res == nil || !res.Success {
			t.Fatalf("got %+v %v", res, err)
		}
		if el := time.Since(start); el > pipeDrainGrace+3*time.Second {
			t.Errorf("应在管道宽限期内返回，实际 %v", el)
		}
	})

	t.Run("给定 Started/Exited 钩子 断言各调一次、Started 早于首个事件、Exited 晚于进程退出", func(t *testing.T) {
		pidFile := filepath.Join(t.TempDir(), "pid")
		bin := writeScript(t, fmt.Sprintf("echo $$ > %s\necho '%s'\necho '%s'\n", pidFile, sysInitLine, resultOKLine))
		var mu sync.Mutex
		var order []string
		rec := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
		exitedAlive := true
		_, err := supervise(context.Background(), launch{
			Bin: bin, Dir: t.TempDir(), Env: os.Environ(),
			Started: func() { rec("started") },
			Exited: func() {
				rec("exited")
				raw, _ := os.ReadFile(pidFile)
				pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
				exitedAlive = pid > 0 && syscall.Kill(pid, 0) == nil
			},
		}, superviseLimits{Timeout: 30 * time.Second}, parseClaude, func(Event) { rec("event") })
		if err != nil {
			t.Fatal(err)
		}
		var started, exited int
		for _, s := range order {
			switch s {
			case "started":
				started++
			case "exited":
				exited++
			}
		}
		if started != 1 || exited != 1 {
			t.Errorf("Started/Exited 次数 got %d/%d want 1/1（%v）", started, exited, order)
		}
		if len(order) == 0 || order[0] != "started" {
			t.Errorf("Started 应早于第一个事件，order=%v", order)
		}
		if exitedAlive {
			t.Error("Exited 调用时进程应已退出")
		}
	})

	t.Run("给定三个钩子都为 nil 断言不 panic", func(t *testing.T) {
		bin := writeScript(t, fmt.Sprintf("echo '%s'\n", resultOKLine))
		if _, err := supervise(context.Background(), launch{Bin: bin, Dir: t.TempDir(), Env: os.Environ()},
			superviseLimits{Timeout: 30 * time.Second}, parseClaude, nil); err != nil {
			t.Fatal(err)
		}
	})
}

// f01 S1：看门狗看的是 parse 的 Touch，而不是「哪些行被转发」。
func TestSuperviseParseHooksTouchBeforeFilter(t *testing.T) {
	bin := writeScript(t, `for i in 1 2 3 4 5 6 7 8; do echo '{"type":"keepalive"}'; sleep 0.3; done`)
	filtering := func(touch bool) func(r io.Reader, h parseHooks) (*Result, error) {
		return func(r io.Reader, h parseHooks) (*Result, error) {
			sc := bufio.NewScanner(r)
			for sc.Scan() {
				if touch && json.Valid(sc.Bytes()) {
					h.Touch()
				}
				// 全部过滤掉：一个都不 Emit
			}
			return &Result{Success: true, Subtype: "success"}, nil
		}
	}
	cases := []struct {
		name     string
		touch    bool
		wantIdle bool
	}{
		{"给定 parse 每行 Touch 但全部过滤 断言不判死且 onEvent 0 次", true, false},
		{"给定 parse 不调 Touch 断言 ErrIdle（supervise 没有在别处替 parse touch）", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var events atomic.Int32
			_, err := supervise(context.Background(), launch{Bin: bin, Dir: t.TempDir(), Env: os.Environ()},
				superviseLimits{Timeout: 30 * time.Second, Idle: time.Second}, filtering(tc.touch),
				func(Event) { events.Add(1) })
			if got := errors.Is(err, ErrIdle); got != tc.wantIdle {
				t.Fatalf("ErrIdle got %v want %v（err=%v）", got, tc.wantIdle, err)
			}
			if n := events.Load(); n != 0 {
				t.Errorf("onEvent 次数 got %d want 0", n)
			}
		})
	}

	t.Run("给定同一份输入 断言 claude 与 codex 的生产 parse Touch 次数 = 可解析 JSON 行数", func(t *testing.T) {
		inputs := map[string]struct {
			stream string
			parse  func(io.Reader, parseHooks) (*Result, error)
		}{
			"claude": {strings.Join([]string{sysInitLine, "噪声", `{"type":`, assistantLine, resultOKLine}, "\n"), parseClaude},
			"codex": {strings.Join([]string{codexThreadLine, "Reading prompt from stdin...", `{"type":`, codexMsgLine, codexTurnDoneLine}, "\n"),
				(&codexParseState{start: time.Now()}).parse},
		}
		for name, in := range inputs {
			var touches, emits int
			if _, err := in.parse(strings.NewReader(in.stream), parseHooks{
				Touch: func() { touches++ }, Emit: func(Event) { emits++ },
			}); err != nil {
				t.Fatalf("%s parse: %v", name, err)
			}
			want := 0
			for _, l := range strings.Split(in.stream, "\n") {
				if json.Valid(bytes.TrimSpace([]byte(l))) {
					want++
				}
			}
			if touches != want || emits != want {
				t.Errorf("%s: Touch/Emit got %d/%d want %d/%d", name, touches, emits, want, want)
			}
		}
	})
}

// f01 S2：kill 前同步调用 BeforeKill 恰好一次；正常退出不调。
func TestSuperviseBeforeKill(t *testing.T) {
	cases := []struct {
		name       string
		script     string
		limits     superviseLimits
		cancelAt   time.Duration
		wantBefore int32
	}{
		{"给定总超时 1s 断言 BeforeKill 1 次且早于杀进程", "echo $$ > %s\nsleep 30\n",
			superviseLimits{Timeout: time.Second, Idle: time.Minute}, 0, 1},
		{"给定 ctx 在 500ms 被取消 断言 BeforeKill 1 次且早于杀进程", "echo $$ > %s\nsleep 30\n",
			superviseLimits{Timeout: time.Minute, Idle: time.Minute}, 500 * time.Millisecond, 1},
		{"给定静默看门狗 1s 断言 BeforeKill 1 次且早于杀进程", "echo $$ > %s\nsleep 30\n",
			superviseLimits{Timeout: time.Minute, Idle: time.Second}, 0, 1},
		{"给定进程正常退出 断言 BeforeKill 0 次", "echo $$ > %s\ntrue\n",
			superviseLimits{Timeout: time.Minute, Idle: time.Minute}, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			bin := writeScript(t, fmt.Sprintf(tc.script, pidFile))
			var before, exited atomic.Int32
			var aliveAtHook atomic.Bool
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelAt > 0 {
				go func() { time.Sleep(tc.cancelAt); cancel() }()
			}
			start := time.Now()
			_, _ = supervise(ctx, launch{
				Bin: bin, Dir: t.TempDir(), Env: os.Environ(),
				BeforeKill: func() {
					before.Add(1)
					raw, _ := os.ReadFile(pidFile)
					pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
					aliveAtHook.Store(pid > 0 && syscall.Kill(pid, 0) == nil)
				},
				Exited: func() { exited.Add(1) },
			}, tc.limits, parseClaude, nil)
			if el := time.Since(start); el > 3*time.Second+500*time.Millisecond && tc.wantBefore > 0 {
				t.Errorf("Run 应在 3s 内返回，实际 %v", el)
			}
			if got := before.Load(); got != tc.wantBefore {
				t.Errorf("BeforeKill 次数 got %d want %d", got, tc.wantBefore)
			}
			if tc.wantBefore > 0 && !aliveAtHook.Load() {
				t.Error("BeforeKill 调用时进程应仍活着（早于 killProcessGroup）")
			}
			if got := exited.Load(); got != 1 {
				t.Errorf("Exited 次数 got %d want 1", got)
			}
		})
	}
}

// f03 的状态判定依赖：总超时 → errors.Is(err, context.DeadlineExceeded)，文案仍含「执行超时」。
func TestSuperviseTimeoutWrapsDeadlineExceeded(t *testing.T) {
	type runner func(t *testing.T, ctx context.Context, timeout time.Duration) error
	claude := func(t *testing.T, ctx context.Context, timeout time.Duration) error {
		_, err := NewDriver(fakeClaude(t, "sleep 30\n"), timeout).Run(ctx, RunParams{Prompt: "x", SessionID: "s"})
		return err
	}
	codex := func(t *testing.T, ctx context.Context, timeout time.Duration) error {
		_, err := newTestCodex(t, fakeCodex(t, "sleep 30\n"), timeout).Run(ctx, RunParams{Prompt: "x", Dir: t.TempDir()})
		return err
	}
	for name, run := range map[string]runner{"claude": claude, "codex": codex} {
		t.Run("给定 "+name+" 总超时 1s 断言 DeadlineExceeded 且含「执行超时」且不是 ErrIdle", func(t *testing.T) {
			err := run(t, context.Background(), time.Second)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("errors.Is(DeadlineExceeded) got false，err=%v", err)
			}
			if err == nil || !strings.Contains(err.Error(), "执行超时") {
				t.Errorf("文案应含「执行超时」，got %v", err)
			}
			if errors.Is(err, ErrIdle) {
				t.Errorf("不应是 ErrIdle: %v", err)
			}
		})
		t.Run("给定 "+name+" 调用方取消 断言 Canceled 且不是 DeadlineExceeded", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			go func() { time.Sleep(300 * time.Millisecond); cancel() }()
			err := run(t, ctx, time.Minute)
			if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("got %v want Canceled 且非 DeadlineExceeded", err)
			}
		})
	}
}
