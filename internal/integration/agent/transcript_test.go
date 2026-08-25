package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectSlug(t *testing.T) {
	cases := []struct{ cwd, want string }{
		// 实测吻合的样本（本机 22 个项目目录逐一比对）
		{"/opt/lathe/workspaces/cr-1363", "-opt-lathe-workspaces-cr-1363"},
		{"/opt/ai-Knowledge-Graph", "-opt-ai-Knowledge-Graph"},
		// '.' 也被替换，因此出现连续的两个 '-'
		{"/opt/CloudRouter/.claude/worktrees/fix-x", "-opt-CloudRouter--claude-worktrees-fix-x"},
		{"", ""},
	}
	for _, c := range cases {
		if got := ProjectSlug(c.cwd); got != c.want {
			t.Errorf("ProjectSlug(%q) = %q，期望 %q", c.cwd, got, c.want)
		}
	}
}

func TestSubagentDirEmptyInputs(t *testing.T) {
	// 缺任一段都拼不出有意义的路径，必须返回空让调用方跳过，
	// 而不是拼出 <root>/-/ 之类会误读到别人目录的路径
	if got := SubagentDir("/root", "", "sess"); got != "" {
		t.Errorf("cwd 为空时应返回空，得到 %q", got)
	}
	if got := SubagentDir("/root", "/cwd", ""); got != "" {
		t.Errorf("sessionID 为空时应返回空，得到 %q", got)
	}
	if got := SubagentDir("", "/cwd", "sess"); got != "" {
		t.Errorf("root 为空时应返回空，得到 %q", got)
	}
}

// ---------------------------------------------------------------- 测试夹具

// 按真实 transcript 的形状构造行（字段名与实测一致）。
const (
	subPromptLine = `{"parentUuid":null,"isSidechain":true,"agentId":"a89b3fd92e02be0f8","type":"user",` +
		`"message":{"role":"user","content":"定位 group 模型的测试代码"},"uuid":"u1",` +
		`"timestamp":"2026-08-25T01:00:00Z","cwd":"/opt/lathe/workspaces/cr-1363"}`

	subToolUseLine = `{"isSidechain":true,"agentId":"a89b3fd92e02be0f8","type":"assistant",` +
		`"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_S1","name":"Grep",` +
		`"input":{"pattern":"func TestGroup"}}]},"uuid":"u2"}`

	subToolResultLine = `{"isSidechain":true,"agentId":"a89b3fd92e02be0f8","type":"user",` +
		`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_S1",` +
		`"content":"internal/group/model_test.go:12"}]},"uuid":"u3"}`

	subTextLine = `{"isSidechain":true,"agentId":"a89b3fd92e02be0f8","type":"assistant",` +
		`"message":{"role":"assistant","content":[{"type":"text","text":"找到了 model_test.go"}]},"uuid":"u4"}`

	// 记账行：transcript 里混着这类东西，不是「agent 干了什么」
	bookkeepingLine = `{"type":"file-history-snapshot","messageId":"m1","snapshot":{"trackedFileBackups":{}}}`
)

func writeSubagentFile(t *testing.T, dir, agentID string, lines ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	path := filepath.Join(dir, "agent-"+agentID+".jsonl")
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("写夹具失败: %v", err)
	}
	return path
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("打开夹具失败: %v", err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatalf("追加夹具失败: %v", err)
		}
	}
}

func kindsOf(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Kind
	}
	return out
}

// ---------------------------------------------------------------- 行为

// 首行是派给 subagent 的任务描述。走 Digest 的 user 路径会被误标成
// 「工具结果」——那条路只认 tool_result 块。必须单独成为分组头。
func TestSubagentReaderFirstLineIsAgentStart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "subagents")
	writeSubagentFile(t, dir, "a89b3fd92e02be0f8", subPromptLine)

	got := NewSubagentReader(dir).Poll()
	e := onlyEntry(t, got)
	if e.Kind != KindAgentStart {
		t.Errorf("Kind = %q，期望 %q（不能标成工具结果）", e.Kind, KindAgentStart)
	}
	if !strings.Contains(e.Body, "group 模型") {
		t.Errorf("Body 应是派给它的任务描述，得到 %q", e.Body)
	}
	if e.AgentID != "a89b3fd92e02be0f8" {
		t.Errorf("AgentID = %q，期望取自 agentId 字段", e.AgentID)
	}
}

// 每条事件都要带上 AgentID，否则界面分不清哪些步骤属于哪个 subagent。
func TestSubagentReaderTagsEveryEntry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "subagents")
	writeSubagentFile(t, dir, "a89b3fd92e02be0f8",
		subPromptLine, subToolUseLine, subToolResultLine, subTextLine)

	got := NewSubagentReader(dir).Poll()
	want := []string{KindAgentStart, KindToolUse, KindToolResult, KindText}
	if diff := kindsOf(got); len(diff) != len(want) {
		t.Fatalf("提炼出的 kind 序列 = %v，期望 %v", diff, want)
	}
	for i, k := range want {
		if got[i].Kind != k {
			t.Errorf("第 %d 条 Kind = %q，期望 %q", i, got[i].Kind, k)
		}
		if got[i].AgentID != "a89b3fd92e02be0f8" {
			t.Errorf("第 %d 条缺 AgentID: %+v", i, got[i])
		}
	}

	// 工具配对的 key 必须保住：subagent 内部的调用同样要能算耗时与成败
	if got[1].Payload["toolUseId"] != "toolu_S1" {
		t.Errorf("tool_use 的 toolUseId = %v，期望 toolu_S1", got[1].Payload["toolUseId"])
	}
	if got[2].Payload["toolUseId"] != "toolu_S1" {
		t.Errorf("tool_result 的 toolUseId = %v，期望 toolu_S1", got[2].Payload["toolUseId"])
	}
}

// 增量读：第二次 Poll 只能吐出新追加的部分，不能重复。
func TestSubagentReaderIncremental(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "subagents")
	path := writeSubagentFile(t, dir, "a1", subPromptLine, subToolUseLine)

	r := NewSubagentReader(dir)
	first := r.Poll()
	if len(first) != 2 {
		t.Fatalf("首轮应读到 2 条，得到 %d: %v", len(first), kindsOf(first))
	}

	if again := r.Poll(); len(again) != 0 {
		t.Errorf("无新内容时应返回空，得到 %v", kindsOf(again))
	}

	appendLines(t, path, subToolResultLine, subTextLine)
	second := r.Poll()
	if len(second) != 2 {
		t.Fatalf("第二轮应只读到新增的 2 条，得到 %d: %v", len(second), kindsOf(second))
	}
	if second[0].Kind != KindToolResult || second[1].Kind != KindText {
		t.Errorf("第二轮 kind = %v，期望 [tool_result text]", kindsOf(second))
	}
	// 第二轮不该再发一次分组头
	for _, e := range second {
		if e.Kind == KindAgentStart {
			t.Errorf("分组头重复发送了")
		}
	}
}

// claude 正在写入的半行不能被吞掉：偏移不前进，等它写完下一轮再读。
func TestSubagentReaderSkipsPartialLine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "subagents")
	path := writeSubagentFile(t, dir, "a1", subPromptLine)

	// 追加一个没有换行符结尾的半行
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("打开夹具失败: %v", err)
	}
	half := subToolUseLine[:len(subToolUseLine)/2]
	if _, err := f.WriteString(half); err != nil {
		t.Fatalf("写半行失败: %v", err)
	}
	f.Close()

	r := NewSubagentReader(dir)
	got := r.Poll()
	if len(got) != 1 || got[0].Kind != KindAgentStart {
		t.Fatalf("半行不该被消费，本轮应只有分组头，得到 %v", kindsOf(got))
	}

	// 把后半截补齐，这一行应当完整地被读到一次
	appendLines(t, path, subToolUseLine[len(subToolUseLine)/2:])
	got = r.Poll()
	if len(got) != 1 || got[0].Kind != KindToolUse {
		t.Fatalf("补齐后应读到那条 tool_use，得到 %v", kindsOf(got))
	}
	if got[0].Payload["toolUseId"] != "toolu_S1" {
		t.Errorf("补齐后的行解析错了: %+v", got[0])
	}
}

// 记账行不是「agent 干了什么」，提炼出来只是噪声。
func TestSubagentReaderSkipsBookkeeping(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "subagents")
	writeSubagentFile(t, dir, "a1", subPromptLine, bookkeepingLine, subTextLine)

	got := NewSubagentReader(dir).Poll()
	if len(got) != 2 {
		t.Fatalf("记账行应被跳过，期望 2 条，得到 %d: %v", len(got), kindsOf(got))
	}
	for _, e := range got {
		if e.Kind == KindRaw {
			t.Errorf("不该产出 raw 条目（那是整行记账字段的噪声）: %q", e.Body)
		}
	}
}

// agentId 字段不是每行都有，缺了就从文件名兜底 —— 分组必须始终有值。
func TestSubagentReaderAgentIDFallsBackToFilename(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "subagents")
	noID := `{"isSidechain":true,"type":"assistant","message":{"role":"assistant",` +
		`"content":[{"type":"text","text":"干完了"}]},"uuid":"u9"}`
	writeSubagentFile(t, dir, "abc123", noID)

	got := NewSubagentReader(dir).Poll()
	if len(got) == 0 {
		t.Fatal("应至少产出分组头")
	}
	for _, e := range got {
		if e.AgentID != "abc123" {
			t.Errorf("AgentID = %q，期望从文件名兜底为 abc123", e.AgentID)
		}
	}
}

// agent 压根没派活时目录不存在，这是常态而非错误。
func TestSubagentReaderMissingDir(t *testing.T) {
	r := NewSubagentReader(filepath.Join(t.TempDir(), "nope", "subagents"))
	if got := r.Poll(); got != nil {
		t.Errorf("目录不存在时应返回空，得到 %v", kindsOf(got))
	}
	// 空 reader 与空目录都不得 panic
	if got := NewSubagentReader("").Poll(); got != nil {
		t.Errorf("空目录应返回空，得到 %v", kindsOf(got))
	}
	var nilReader *SubagentReader
	if got := nilReader.Poll(); got != nil {
		t.Errorf("nil reader 应返回空，得到 %v", kindsOf(got))
	}
}

// 多个 subagent 的事件顺序必须稳定，不能随目录枚举顺序抖动。
func TestSubagentReaderStableOrderAcrossAgents(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "subagents")
	// 用不带 agentId 的行，让归属完全由文件名决定，这样断言的就是排序本身
	line := `{"isSidechain":true,"type":"assistant","message":{"role":"assistant",` +
		`"content":[{"type":"text","text":"干完了"}]},"uuid":"u9"}`
	writeSubagentFile(t, dir, "b-second", line)
	writeSubagentFile(t, dir, "a-first", line)

	got := NewSubagentReader(dir).Poll()
	if len(got) < 2 {
		t.Fatalf("两个文件应都被读到，得到 %v", kindsOf(got))
	}
	if got[0].AgentID != "a-first" {
		t.Errorf("首条应来自 a-first（按文件名排序），得到 %q", got[0].AgentID)
	}
	if got[len(got)-1].AgentID != "b-second" {
		t.Errorf("末条应来自 b-second，得到 %q", got[len(got)-1].AgentID)
	}
}

// ---------------------------------------------------------------- 真实数据探针
//
// 这个格式是 Claude Code 的内部实现，无文档、可能随版本变化。本机若有真实
// transcript 就拿它对一遍，作为格式漂移的哨兵；没有则跳过（CI 环境不该依赖
// 开发机的 ~/.claude）。
func TestSubagentReaderAgainstRealTranscript(t *testing.T) {
	root := ProjectsRoot()
	if root == "" {
		t.Skip("取不到 ~/.claude/projects")
	}
	matches, _ := filepath.Glob(filepath.Join(root, "*", "*", "subagents", "agent-*.jsonl"))
	if len(matches) == 0 {
		t.Skip("本机没有真实的 subagent 记录可比对")
	}

	dir := filepath.Dir(matches[0])
	got := NewSubagentReader(dir).Poll()
	if len(got) == 0 {
		t.Fatalf("真实记录 %s 一条都没提炼出来 —— 格式可能变了", dir)
	}
	if got[0].Kind != KindAgentStart {
		t.Errorf("首条 Kind = %q，期望 %q（真实文件首行应是派活描述）", got[0].Kind, KindAgentStart)
	}

	var tools int
	for _, e := range got {
		if e.AgentID == "" {
			t.Errorf("真实记录里有条目缺 AgentID: %+v", e)
			break
		}
		if e.Kind == KindToolUse {
			tools++
		}
	}
	if tools == 0 {
		t.Errorf("真实 subagent 记录里没读到任何工具调用 —— 提炼逻辑可能失效了")
	}
	t.Logf("真实记录 %s：%d 条事件，其中 %d 次工具调用", filepath.Base(dir), len(got), tools)
}
