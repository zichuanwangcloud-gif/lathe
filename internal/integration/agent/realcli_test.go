package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// AC-14：驱动级真实运行证据。用 Lathe 自己的驱动（不是裸 CLI）起**真实 claude
// 二进制**，在 Bash sleep 窗口里 Deliver 一句话。默认跳过（要真 CLI，不进 make test）：
//
//	go test ./internal/integration/agent -run TestRealCLIInterject -v -count=1 \
//	  -args -real-bin "$(command -v claude)" -real-out /opt/lathe-wt/evidence/f01/real-cli
//
// 模型端是本测试进程里的 httptest stub provider（ANTHROPIC_BASE_URL 指向它，
// 假 token），不用真凭据、不连外网。stub 按 Messages API 的 SSE 形状应答：
// 首帧要求 sleep → 回一个 Bash tool_use；之后的请求里能看到什么标记就原样
// 回什么 —— 于是「result 里有 MARK」证明的是：注入帧被真实 CLI 并进了**同一个
// turn** 的下一次模型请求（CLI 收到 + 转给了模型），而不是模型的理解能力。
// 子进程显式 Dir（mktemp 仓库）+ 白名单 env（sanitizedEnv）+ 隔离 HOME。
var (
	realBin = flag.String("real-bin", "", "AC-14：真实 claude 可执行文件路径（空 = 跳过）")
	realOut = flag.String("real-out", "", "AC-14：证据输出目录")
)

var markRe = regexp.MustCompile(`(?:ZEBRA|MARK1|MARK2)-[0-9a-f]{8}`)

// stubProvider 是最小的 Anthropic Messages API 替身。
type stubProvider struct {
	mu  sync.Mutex
	log []map[string]any // 只记每次请求里的消息正文摘要（不含任何凭据头）
}

func (s *stubProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if !strings.HasPrefix(r.URL.Path, "/v1/messages") || strings.Contains(r.URL.Path, "count_tokens") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":10}`))
		return
	}
	var req struct {
		Model    string            `json:"model"`
		Stream   bool              `json:"stream"`
		Messages []json.RawMessage `json:"messages"`
	}
	_ = json.Unmarshal(body, &req)
	msgs := string(body)
	// 只看 messages 部分判定（system prompt 里可能出现任何词）
	if i := strings.Index(msgs, `"messages"`); i >= 0 {
		msgs = msgs[i:]
	}
	marks := uniq(markRe.FindAllString(msgs, -1))
	// 只看「最后一次 sleep 要求」之后有没有工具结果：--resume 的会话历史里
	// 带着上一段的 tool_result，不能拿它当「这一段已经 sleep 过」。
	last := strings.LastIndex(msgs, "F01-SLEEP")
	wantsSleep := last >= 0
	hasToolResult := wantsSleep && strings.Contains(msgs[last:], `"tool_result"`)

	var reply map[string]any
	switch {
	case wantsSleep && !hasToolResult:
		reply = map[string]any{"tool": "sleep 8"}
	default:
		reply = map[string]any{"text": "DONE " + strings.Join(marks, " ")}
	}
	s.mu.Lock()
	s.log = append(s.log, map[string]any{
		"at": time.Now().Format(time.RFC3339Nano), "messages": len(req.Messages),
		"hasToolResult": hasToolResult, "marksSeen": marks, "reply": reply,
	})
	s.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	ev := func(name string, data any) {
		b, _ := json.Marshal(data)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b)
		if fl != nil {
			fl.Flush()
		}
	}
	model := req.Model
	if model == "" {
		model = "stub"
	}
	ev("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_stub", "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 5, "output_tokens": 1}}})
	stop := "end_turn"
	if cmd, ok := reply["tool"]; ok {
		stop = "tool_use"
		ev("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_stub_%d", time.Now().UnixNano()), "name": "Bash", "input": map[string]any{}}})
		in, _ := json.Marshal(map[string]any{"command": cmd, "description": "wait"})
		ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(in)}})
	} else {
		ev("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""}})
		ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": reply["text"]}})
	}
	ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	ev("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 4}})
	ev("message_stop", map[string]any{"type": "message_stop"})
}

func uniq(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

type segMeta struct {
	Segment            string   `json:"segment"`
	Args               []string `json:"args"`
	SessionID          string   `json:"sessionId"`
	DeliveryID         string   `json:"deliveryId,omitempty"`
	DeliverErr         string   `json:"deliverErr,omitempty"`
	ReplayAcked        bool     `json:"replayAcked"`
	ReplayLine         int      `json:"replayLine,omitempty"`
	ResultCount        int      `json:"resultCount"`
	ResultText         string   `json:"resultText"`
	ResultSessionID    string   `json:"resultSessionId"`
	ResultAtMS         int64    `json:"resultAtMs"`
	ExitAtMS           int64    `json:"exitAtMs"`
	ExitAfterResultS   float64  `json:"exitAfterResultS"`
	RunErr             string   `json:"runErr,omitempty"`
	IsIdle             bool     `json:"isIdle"`
	LateDeliverErr     string   `json:"lateDeliverErr,omitempty"`
	StdoutFile         string   `json:"stdoutFile"`
	FilteredEventTypes []string `json:"filteredEventTypes"`
}

func TestRealCLIInterject(t *testing.T) {
	if *realBin == "" {
		t.Skip("AC-14 真实 CLI 证据：用 -args -real-bin <claude> -real-out <dir> 运行")
	}
	out := *realOut
	if out == "" {
		out = t.TempDir()
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Format("20060102-150405")

	stub := &stubProvider{}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	home := t.TempDir()
	repo := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home,
			"GIT_AUTHOR_NAME=f01", "GIT_AUTHOR_EMAIL=f01@example.invalid",
			"GIT_COMMITTER_NAME=f01", "GIT_COMMITTER_EMAIL=f01@example.invalid"}
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, b)
		}
		return string(b)
	}
	git("init", "-q")
	git("commit", "-q", "--allow-empty", "-m", "init")

	// tee 包装：真实 claude 的 stdout 全量落盘（含被驱动过滤掉的 replay/lifecycle）
	wrapper := filepath.Join(t.TempDir(), "claude-tee")
	if err := os.WriteFile(wrapper, []byte("#!/bin/bash\n\"$F01_REAL_BIN\" \"$@\" | tee -a \"$F01_TEE_OUT\"\nexit ${PIPESTATUS[0]}\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	short := func() string { return newFrameID()[:8] }
	zebra, mark1, mark2 := "ZEBRA-"+short(), "MARK1-"+short(), "MARK2-"+short()
	sid := newFrameID()
	d := &Driver{bin: wrapper, timeout: 3 * time.Minute, idle: 2 * time.Minute}

	runSeg := func(name string, p RunParams, inject string) segMeta {
		stdout := filepath.Join(out, fmt.Sprintf("real-cli-%s-%s.jsonl", stamp, name))
		_ = os.Remove(stdout)
		p.Dir = repo
		p.StreamInput = true
		p.PermissionMode = "bypassPermissions"
		p.SettingSources = "project"
		p.ExtraEnv = []string{
			"HOME=" + home, "CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude"),
			"ANTHROPIC_BASE_URL=" + srv.URL, "ANTHROPIC_AUTH_TOKEN=stub-not-a-token", "ANTHROPIC_API_KEY=",
			"DISABLE_AUTOUPDATER=1", "DISABLE_TELEMETRY=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
			"F01_REAL_BIN=" + *realBin, "F01_TEE_OUT=" + stdout,
		}
		meta := segMeta{Segment: name, Args: d.buildArgs(p), SessionID: p.SessionID, StdoutFile: filepath.Base(stdout)}
		var mu sync.Mutex
		var sess *Session
		acked := make(chan struct{}, 1)
		deliveryID := newFrameID()
		start := time.Now()
		p.OnReady = func(s *Session) {
			mu.Lock()
			sess = s
			mu.Unlock()
			if inject == "" {
				return
			}
			meta.DeliveryID = deliveryID
			go func() {
				time.Sleep(4 * time.Second) // 落在 Bash sleep 8 的窗口里
				if err := s.Deliver(context.Background(), deliveryID, inject); err != nil {
					mu.Lock()
					meta.DeliverErr = err.Error()
					mu.Unlock()
				}
			}()
		}
		p.OnDelivery = func(id string) {
			if id == deliveryID {
				select {
				case acked <- struct{}{}:
				default:
				}
			}
		}
		p.OnEvent = func(e Event) {
			mu.Lock()
			defer mu.Unlock()
			meta.FilteredEventTypes = append(meta.FilteredEventTypes, string(e.Type))
			if e.Type == EventResult && meta.ResultAtMS == 0 {
				meta.ResultAtMS = time.Since(start).Milliseconds()
			}
		}
		res, err := d.Run(context.Background(), p)
		meta.ExitAtMS = time.Since(start).Milliseconds()
		if err != nil {
			meta.RunErr = err.Error()
			meta.IsIdle = errors.Is(err, ErrIdle)
		}
		if res != nil {
			meta.ResultText, meta.ResultSessionID = res.Text, res.SessionID
		}
		select {
		case <-acked:
			meta.ReplayAcked = true
		default:
		}
		meta.ExitAfterResultS = float64(meta.ExitAtMS-meta.ResultAtMS) / 1000
		mu.Lock()
		late := sess
		mu.Unlock()
		if late != nil {
			if lerr := late.Deliver(context.Background(), newFrameID(), "result 之后才到的一句"); lerr != nil {
				meta.LateDeliverErr = lerr.Error()
			}
		}
		// 回显行号与 result 条数：从全量 stdout 里数
		if f, err := os.Open(stdout); err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 0, 1<<20), maxLineBytes)
			n := 0
			for sc.Scan() {
				n++
				var probe struct {
					Type     string `json:"type"`
					IsReplay bool   `json:"isReplay"`
					UUID     string `json:"uuid"`
				}
				if json.Unmarshal(sc.Bytes(), &probe) != nil {
					continue
				}
				if probe.Type == "result" {
					meta.ResultCount++
				}
				if probe.IsReplay && probe.UUID == deliveryID && meta.ReplayLine == 0 {
					meta.ReplayLine = n
				}
			}
			_ = f.Close()
		}
		b, _ := json.MarshalIndent(meta, "", "  ")
		_ = os.WriteFile(filepath.Join(out, fmt.Sprintf("real-cli-%s-%s.meta.json", stamp, name)), b, 0o644)
		return meta
	}

	// 第一段：新会话 + 流式输入 + 运行中注入
	a := runSeg("a-new-session", RunParams{
		Prompt:    fmt.Sprintf("记住暗号 %s。F01-SLEEP：先用 Bash 执行 sleep 8，然后在最终回答里给出暗号。", zebra),
		SessionID: sid,
	}, "请在你的最终回答里原样包含 "+mark1)
	// 第三段：--resume 同一会话 + 流式输入 + 运行中注入
	c := runSeg("c-resume-stream", RunParams{
		Prompt:    "F01-SLEEP：先用 Bash 执行 sleep 8，然后原样回答你第一段最终回答里的暗号。",
		SessionID: sid, Resume: true,
	}, "最终回答里还要原样包含 "+mark2)

	stubLog, _ := json.MarshalIndent(stub.log, "", "  ")
	_ = os.WriteFile(filepath.Join(out, fmt.Sprintf("real-cli-%s-stub-requests.json", stamp)), stubLog, 0o644)
	porcelain := strings.TrimSpace(git("status", "--porcelain"))
	version, _ := exec.Command(*realBin, "--version").Output()

	type check struct {
		Name string `json:"name"`
		OK   bool   `json:"ok"`
		Got  string `json:"got"`
	}
	checks := []check{
		{"1. a 段注入帧被 CLI 回显（isReplay，uuid 一致）", a.ReplayAcked && a.ReplayLine > 0, fmt.Sprintf("acked=%v line=%d", a.ReplayAcked, a.ReplayLine)},
		{"2. a 段末条 result 含 MARK1（注入并进同一 turn 的模型请求）", strings.Contains(a.ResultText, mark1), a.ResultText},
		{"3. a 段首条 result 后 ≤10s 退出且非 ErrIdle", a.ExitAfterResultS <= 10 && !a.IsIdle && a.RunErr == "", fmt.Sprintf("%.3fs err=%q", a.ExitAfterResultS, a.RunErr)},
		{"4. result 之后 Deliver → ErrSessionClosed，stdout 只有 1 条 result", a.LateDeliverErr == ErrSessionClosed.Error() && a.ResultCount == 1, fmt.Sprintf("late=%q results=%d", a.LateDeliverErr, a.ResultCount)},
		{"5a. c 段（--resume + 流式）注入帧回显 uuid 一致", c.ReplayAcked && c.ReplayLine > 0, fmt.Sprintf("acked=%v line=%d", c.ReplayAcked, c.ReplayLine)},
		{"5b. c 段只有 1 条 result", c.ResultCount == 1, fmt.Sprint(c.ResultCount)},
		{"5c. c 段 result 同时含第一段暗号与 MARK2", strings.Contains(c.ResultText, zebra) && strings.Contains(c.ResultText, mark2), c.ResultText},
		{"5d. c 段 result 的 session_id 等于 sid", c.ResultSessionID == sid, c.ResultSessionID},
		{"5e. c 段 result 后 ≤10s 退出且非 ErrIdle", c.ExitAfterResultS <= 10 && !c.IsIdle && c.RunErr == "", fmt.Sprintf("%.3fs err=%q", c.ExitAfterResultS, c.RunErr)},
		{"5f. c 段参数形状 = --resume <sid> --input-format stream-json --replay-user-messages", strings.Contains(strings.Join(c.Args, " "), "--resume "+sid) && strings.Contains(strings.Join(c.Args, " "), "--input-format stream-json --replay-user-messages"), strings.Join(c.Args, " ")},
		{"6. 临时仓库 git status --porcelain 为空", porcelain == "", porcelain},
		{"驱动交给 OnEvent 的事件里没有 user(replay)/command_lifecycle", !containsAny(a.FilteredEventTypes, "command_lifecycle") && !containsAny(c.FilteredEventTypes, "command_lifecycle"), strings.Join(append(a.FilteredEventTypes, c.FilteredEventTypes...), ",")},
	}
	summary := map[string]any{
		"cliVersion": strings.TrimSpace(string(version)), "provider": "httptest stub（127.0.0.1，假 token，不连外网）",
		"sessionId": sid, "zebra": zebra, "mark1": mark1, "mark2": mark2, "checks": checks,
	}
	b, _ := json.MarshalIndent(summary, "", "  ")
	_ = os.WriteFile(filepath.Join(out, fmt.Sprintf("real-cli-%s-summary.json", stamp)), b, 0o644)
	t.Logf("summary:\n%s", b)
	for _, c := range checks {
		if !c.OK {
			t.Errorf("AC-14 %s 未通过：got=%s", c.Name, c.Got)
		}
	}
}

func containsAny(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
