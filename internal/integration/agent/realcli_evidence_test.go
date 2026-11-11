package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// realcli_evidence_test.go —— 驱动级真实运行证据（f06 AC-18 / AC-22）。
//
// opt-in：没设 LATHE_F06_REAL_CLI=1 就 t.Skipf，make test 不跑（AGENTS.md §5.2：
// 不引入外部服务）。打开后用**真实的 codex / claude 二进制**，但模型端是本测试
// 用 httptest 起的 127.0.0.1 stub（Responses API / Messages API）：不用真凭据、
// 不连外网、不碰任何真实仓库（临时 git 仓库）。
//
// 专用 CODEX_HOME 建在证据目录下，不用 t.TempDir()：P17 实测 CODEX_HOME 位于
// 临时目录下时 codex 静默拒绝执行 shell 工具。

func requireRealCLI(t *testing.T, bins ...string) string {
	t.Helper()
	if os.Getenv("LATHE_F06_REAL_CLI") != "1" {
		t.Skipf("真实 CLI 证据测试是 opt-in 的：设 LATHE_F06_REAL_CLI=1 打开")
	}
	dir := os.Getenv("LATHE_F06_EVIDENCE_DIR")
	if dir == "" {
		t.Fatalf("LATHE_F06_REAL_CLI=1 时必须给 LATHE_F06_EVIDENCE_DIR（证据写在那里）")
	}
	for _, b := range bins {
		if _, err := exec.LookPath(b); err != nil {
			t.Fatalf("本机没有 %s: %v", b, err)
		}
	}
	return dir
}

// evidenceCodexHome 在证据目录下建一个专用 CODEX_HOME，测试结束即删
// （rollout / installation 文件不留在证据目录）。
func evidenceCodexHome(t *testing.T, evidenceDir, configTOML string) string {
	t.Helper()
	ch, err := os.MkdirTemp(evidenceDir, ".codex-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(ch) })
	if err := CheckCodexHomeNotInTempDir(ch, os.TempDir()); err != nil {
		t.Fatalf("专用 CODEX_HOME 落在了临时目录下（证据目录不能放在 %s 下）: %v", os.TempDir(), err)
	}
	if err := os.WriteFile(filepath.Join(ch, "config.toml"), []byte(configTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	return ch
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q", "."},
		{"-c", "user.email=f06@example.com", "-c", "user.name=f06", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func writeSkill(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"),
		[]byte("---\nname: "+name+"\ndescription: "+body+"\n---\n\n"+body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sse(w io.Writer, ev string, data any) {
	b, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, b)
}

// responsesStub 是 Responses API 的最小 stub：
//   - 请求 input 里没有 function_call_output 且 prompt 含 F06TURN1 → 一次 exec_command（printf F06 > f06.txt）；
//   - 其余 → 一条 agent_message。
//
// 只记录请求体里标记串的出现次数，不落请求体（含设备 installation_id）。
type responsesStub struct {
	mu                sync.Mutex
	markers           map[string]int
	resumeCarriesTurn bool
	requests          int
}

func (s *responsesStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		str := string(body)
		s.mu.Lock()
		s.requests++
		for _, m := range []string{"go-testing", "personalmarker", "agentsmarker"} {
			s.markers[m] += strings.Count(str, m)
		}
		if strings.Contains(str, "F06TURN2") && strings.Contains(str, "F06TURN1") {
			s.resumeCarriesTurn = true
		}
		s.mu.Unlock()

		var req struct {
			Model string            `json:"model"`
			Input []json.RawMessage `json:"input"`
		}
		_ = json.Unmarshal(body, &req)
		hasToolOut := strings.Contains(str, `"function_call_output"`)
		now := time.Now().Unix()
		var buf bytes.Buffer
		sse(&buf, "response.created", map[string]any{"type": "response.created", "response": map[string]any{
			"id": "resp_1", "object": "response", "created_at": now, "status": "in_progress", "model": req.Model, "output": []any{}}})
		var item map[string]any
		if !hasToolOut && strings.Contains(str, "F06TURN1") && !strings.Contains(str, "F06TURN2") {
			args := `{"cmd":"printf F06 > f06.txt"}`
			item = map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "exec_command", "arguments": args}
			sse(&buf, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0,
				"item": map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "exec_command", "arguments": ""}})
			sse(&buf, "response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta",
				"item_id": "fc_1", "output_index": 0, "delta": args})
		} else {
			text := "DONE-F06"
			item = map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
			sse(&buf, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0,
				"item": map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "status": "in_progress", "content": []any{}}})
			sse(&buf, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": "msg_1",
				"output_index": 0, "content_index": 0, "delta": text})
		}
		sse(&buf, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
		sse(&buf, "response.completed", map[string]any{"type": "response.completed", "response": map[string]any{
			"id": "resp_1", "object": "response", "created_at": now, "status": "completed", "model": req.Model, "output": []any{item},
			"usage": map[string]any{"input_tokens": 11, "input_tokens_details": map[string]any{"cached_tokens": 0},
				"output_tokens": 3, "output_tokens_details": map[string]any{"reasoning_tokens": 0}, "total_tokens": 14}}})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(buf.Bytes())
	})
}

// messagesStub 是 Anthropic Messages API 的最小 stub（流式回一段文本）。
func messagesStub() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/messages") {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var buf bytes.Buffer
		sse(&buf, "message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": "msg_stub_1", "type": "message", "role": "assistant", "model": req.Model, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 5, "output_tokens": 1}}})
		sse(&buf, "content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""}})
		sse(&buf, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": "F06CLAUDEOK"}})
		sse(&buf, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		sse(&buf, "message_delta", map[string]any{"type": "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 4}})
		sse(&buf, "message_stop", map[string]any{"type": "message_stop"})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(buf.Bytes())
	})
}

// recorder 把一次 Run 的原始事件行写进文件，并收集 Digest 结果。
type recorder struct {
	mu      sync.Mutex
	f       *os.File
	entries []map[string]any
}

func newRecorder(t *testing.T, path string) *recorder {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return &recorder{f: f}
}

func (r *recorder) onEvent(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, _ = r.f.Write(append(append([]byte(nil), ev.Raw...), '\n'))
	for _, e := range Digest(ev) {
		r.entries = append(r.entries, map[string]any{"kind": e.Kind, "tool": e.Tool, "body": e.Body, "payload": e.Payload})
	}
}

func TestRealCLIEvidence(t *testing.T) {
	evidence := requireRealCLI(t, "codex", "claude")

	stub := &responsesStub{markers: map[string]int{}}
	rs := httptest.NewServer(stub.handler())
	defer rs.Close()
	ms := httptest.NewServer(messagesStub())
	defer ms.Close()

	ch := evidenceCodexHome(t, evidence, fmt.Sprintf(`model_provider = "stub"
model = "stub-model"

[model_providers.stub]
name = "Stub"
base_url = "%s/v1"
wire_api = "responses"
env_key = "F06_STUB_KEY"
requires_openai_auth = false
`, rs.URL))
	t.Setenv("F06_STUB_KEY", "stub-not-a-key")
	t.Setenv("LATHE_AGENT_ENV_EXTRA", "F06_STUB_KEY")

	// 伪 HOME：个人技能（.codex/skills 与 .agents/skills）放在这里，
	// codex 子进程的 HOME 被驱动换成 <CODEX_HOME>/home，它们不该进请求。
	fakeHome := t.TempDir()
	writeSkill(t, filepath.Join(fakeHome, ".codex", "skills", "personalmarker"), "personalmarker", "personalmarker skill")
	writeSkill(t, filepath.Join(fakeHome, ".agents", "skills", "agentsmarker"), "agentsmarker", "agentsmarker skill")
	t.Setenv("HOME", fakeHome)

	wt := t.TempDir()
	gitInit(t, wt)
	skill, err := os.ReadFile(filepath.Join("..", "..", "..", "skills", "go-testing", "1.0.0", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wt, ".codex", "skills", "go-testing"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".codex", "skills", "go-testing", "SKILL.md"), skill, 0o644); err != nil {
		t.Fatal(err)
	}

	router := NewRouter(NewDriver("claude", 5*time.Minute), NewCodexDriver("codex", ch, 5*time.Minute))
	ctx := context.Background()

	// ---- codex 第一轮：新会话，执行一次 shell 工具 ----
	rec1 := newRecorder(t, filepath.Join(evidence, "codex-run1.ndjson"))
	var reported []string
	res1, err := router.Run(ctx, RunParams{
		Driver: DriverCodex, Prompt: "F06TURN1：在仓库根写一个 f06.txt", Dir: wt,
		PermissionMode: "acceptEdits", OnEvent: rec1.onEvent,
		OnSession: func(id string) { reported = append(reported, id) },
	})
	if err != nil {
		t.Fatalf("codex 第一轮失败: %v", err)
	}
	if !res1.Success || res1.SessionID == "" {
		t.Fatalf("codex 第一轮结果不符: %+v", res1)
	}
	if len(reported) != 1 || reported[0] != res1.SessionID {
		t.Errorf("OnSession 应恰好上报一次 thread_id，got %v（Result.SessionID=%s）", reported, res1.SessionID)
	}
	content, _ := os.ReadFile(filepath.Join(wt, "f06.txt"))

	// ---- codex 第二轮：resume 同一会话 ----
	rec2 := newRecorder(t, filepath.Join(evidence, "codex-run2-resume.ndjson"))
	res2, err := router.Run(ctx, RunParams{
		Driver: DriverCodex, Prompt: "F06TURN2：继续", Dir: wt, Resume: true, SessionID: res1.SessionID,
		PermissionMode: "acceptEdits", OnEvent: rec2.onEvent,
	})
	if err != nil {
		t.Fatalf("codex resume 失败: %v", err)
	}

	// ---- claude 一轮：同一套驱动接口，Messages API stub ----
	claudeCfg := t.TempDir()
	rec3 := newRecorder(t, filepath.Join(evidence, "claude-run.ndjson"))
	res3, err := router.Run(ctx, RunParams{
		Prompt: "probe", Dir: wt, SessionID: "f06a1c9c-0000-4000-8000-000000000001",
		PermissionMode: "plan", SettingSources: "project", OnEvent: rec3.onEvent,
		ExtraEnv: []string{"ANTHROPIC_BASE_URL=" + ms.URL, "ANTHROPIC_AUTH_TOKEN=stub", "CLAUDE_CONFIG_DIR=" + claudeCfg},
	})
	if err != nil {
		t.Fatalf("claude 一轮失败: %v", err)
	}

	stub.mu.Lock()
	markers := map[string]int{
		"worktreeSkill": stub.markers["go-testing"],
		"personalSkill": stub.markers["personalmarker"],
		"agentsSkill":   stub.markers["agentsmarker"],
	}
	carries := stub.resumeCarriesTurn
	stub.mu.Unlock()

	summary := map[string]any{
		"codex": map[string]any{
			"codexHomeOutsideTempDir":   CheckCodexHomeNotInTempDir(ch, os.TempDir()) == nil,
			"run1SessionID":             res1.SessionID,
			"run2SessionID":             res2.SessionID,
			"run1Success":               res1.Success,
			"run2Success":               res2.Success,
			"resumeRequestCarriesTurn1": carries,
			"fileWrittenInWorktree":     string(content) == "F06",
			"requestMarkers":            markers,
			"entries":                   append(append([]map[string]any(nil), rec1.entries...), rec2.entries...),
		},
		"claude": map[string]any{
			"success":   res3.Success,
			"sessionID": res3.SessionID,
			"text":      res3.Text,
			"entries":   rec3.entries,
		},
	}
	b, _ := json.MarshalIndent(summary, "", "  ")
	if err := os.WriteFile(filepath.Join(evidence, "summary.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	if string(content) != "F06" {
		t.Errorf("codex 应在临时仓库里写出 f06.txt=F06，got %q", content)
	}
	if res2.SessionID != res1.SessionID || !carries {
		t.Errorf("resume 应保持原会话：run1=%s run2=%s carries=%v", res1.SessionID, res2.SessionID, carries)
	}
	if markers["worktreeSkill"] < 1 || markers["personalSkill"]+markers["agentsSkill"] != 0 {
		t.Errorf("请求里的技能标记 got %v want worktree≥1 且个人技能 0", markers)
	}
	if !res3.Success {
		t.Errorf("claude 一轮应成功: %+v", res3)
	}
}

// AC-22：codex 的 $HOME 隔离用真实二进制机检（带阳性对照与文件访问计数）。
func TestCodexHomeIsolationRealCLI(t *testing.T) {
	evidence := requireRealCLI(t, "codex")
	ch := evidenceCodexHome(t, evidence, "")

	fakeHome := t.TempDir()
	writeSkill(t, filepath.Join(fakeHome, ".codex", "skills", "personalmarker"), "personalmarker", "personalmarker skill")
	writeSkill(t, filepath.Join(fakeHome, ".agents", "skills", "agentsmarker"), "agentsmarker", "agentsmarker skill")
	if err := os.WriteFile(filepath.Join(fakeHome, ".codex", "hooks.json"),
		[]byte(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"touch `+filepath.Join(fakeHome, "hook-ran")+`"}]}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeHome, ".codex", "config.toml"), []byte("# personal config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)

	wt := t.TempDir()
	gitInit(t, wt)
	skill, err := os.ReadFile(filepath.Join("..", "..", "..", "skills", "go-testing", "1.0.0", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wt, ".codex", "skills", "go-testing"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".codex", "skills", "go-testing", "SKILL.md"), skill, 0o644); err != nil {
		t.Fatal(err)
	}

	// 生产代码构造的 env / Dir（不是测试自己拼）
	l, err := NewCodexDriver("codex", ch, time.Minute).codexLaunch(RunParams{Prompt: "hi", Dir: wt})
	if err != nil {
		t.Fatal(err)
	}
	run := func(env []string, wrap ...string) string {
		t.Helper()
		args := append(append([]string(nil), wrap...), "codex", "debug", "prompt-input", "hi")
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir, cmd.Env = l.Dir, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v 失败: %v\n%s", args, err, out)
		}
		return string(out)
	}

	isolated := run(l.Env)
	if !strings.Contains(isolated, "go-testing") {
		t.Error("worktree 的 .codex/skills/go-testing 应可见")
	}
	for _, m := range []string{"personalmarker", "agentsmarker"} {
		if strings.Contains(isolated, m) {
			t.Errorf("隔离下不应看到个人技能 %s", m)
		}
	}

	// 阳性对照：不隔离（HOME=伪 HOME、不设 CODEX_HOME）时两个标记都能看到
	var open []string
	for _, kv := range l.Env {
		switch envKey(kv) {
		case "HOME", "CODEX_HOME":
			continue
		}
		open = append(open, kv)
	}
	open = append(open, "HOME="+fakeHome)
	unisolated := run(open)
	for _, m := range []string{"personalmarker", "agentsmarker"} {
		if !strings.Contains(unisolated, m) {
			t.Errorf("阳性对照：不隔离时应能看到 %s（否则上面的「不含」没有意义）", m)
		}
	}

	if _, err := exec.LookPath("strace"); err != nil {
		t.Logf("PATH 里没有 strace，跳过文件访问计数（PR 里附手工 strace 结果）")
		return
	}
	traceFile := filepath.Join(t.TempDir(), "strace.txt")
	run(l.Env, "strace", "-f", "-qq", "-e", "trace=%file", "-o", traceFile)
	trace, err := os.ReadFile(traceFile)
	if err != nil {
		t.Fatal(err)
	}
	n := strings.Count(string(trace), fakeHome)
	t.Logf("strace 断言已执行：%d 行文件系统调用里，伪 HOME（%s）出现 %d 次", bytes.Count(trace, []byte("\n")), fakeHome, n)
	if n != 0 {
		t.Errorf("隔离下 codex 不应访问伪 HOME 下的任何文件（hooks.json / config.toml / 技能），got %d 次", n)
	}
	if _, err := os.Stat(filepath.Join(fakeHome, "hook-ran")); err == nil {
		t.Error("个人 hooks 不应被执行")
	}
}
