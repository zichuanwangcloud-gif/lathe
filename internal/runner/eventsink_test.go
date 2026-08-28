package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Clouditera/lathe/internal/integration/agent"
)

// eventRow 是 recorder 收到的一条事件及其归属（任务 + 阶段）。
type eventRow struct {
	taskID int64
	phase  string
	entry  agent.Entry
}

// fakeEventRecorder 收集落库的事件与摘要；InsertAgentEvents 在 flush
// 协程里被调，必须加锁。
type fakeEventRecorder struct {
	mu        sync.Mutex
	rows      []eventRow
	summaries []string
}

func (f *fakeEventRecorder) InsertAgentEvents(ctx context.Context, taskID int64, phase string, entries []agent.Entry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range entries {
		f.rows = append(f.rows, eventRow{taskID: taskID, phase: phase, entry: e})
	}
	return nil
}

func (f *fakeEventRecorder) SetAgentSummary(ctx context.Context, taskID int64, summary string, costUSD float64, durationMS int64, numTurns int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.summaries = append(f.summaries, summary)
	return nil
}

func (f *fakeEventRecorder) collected() []eventRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]eventRow(nil), f.rows...)
}

// assistantTextEvent 造一条含正文块的 assistant 事件。Marshal 的输入是
// 静态结构，不会失败。
func assistantTextEvent(text string) agent.Event {
	raw, _ := json.Marshal(map[string]any{
		"type":    "assistant",
		"message": map[string]any{"content": []map[string]any{{"type": "text", "text": text}}},
	})
	return agent.Event{Type: agent.EventAssistant, Raw: raw}
}

// Close 必须 drain：不依赖 200ms 的 tick，剩余事件也要全部落库，
// 且顺序与投递顺序一致。
func TestEventSinkDrainOnClose(t *testing.T) {
	rec := &fakeEventRecorder{}
	s := newEventSink(context.Background(), rec, 42, "implement", "", "")

	s.OnEvent(assistantTextEvent("第一步"))
	s.OnEvent(assistantTextEvent("第二步"))
	s.OnEvent(assistantTextEvent("第三步"))
	s.Close()

	got := rec.collected()
	if len(got) != 3 {
		t.Fatalf("drain 后应有 3 条事件，得到 %d", len(got))
	}
	for i, want := range []string{"第一步", "第二步", "第三步"} {
		if got[i].entry.Kind != agent.KindText || got[i].entry.Body != want {
			t.Errorf("第 %d 条 = %v，期望 text/%q", i, got[i].entry, want)
		}
		if got[i].taskID != 42 || got[i].phase != "implement" {
			t.Errorf("第 %d 条归属不符: task=%d phase=%s", i, got[i].taskID, got[i].phase)
		}
	}
}

// 缓冲满时优先丢 thinking 并计数（docs/04 §3.2）。
//
// 直接构造 struct、不起 flush 协程：channel 只进不出，水位行为是确定的。
// 因此本测试不调 Close（没有协程可 drain，会挂住）。
func TestEventSinkOverflowDropsThinkingFirst(t *testing.T) {
	s := &EventSink{ch: make(chan agent.Entry, sinkCap)}

	// 填满到 thinking 水位：thinking 开始被丢，text 还能进
	for i := 0; i < thinkingWatermark; i++ {
		s.offer(agent.Entry{Kind: agent.KindText, Body: "x"})
	}
	s.offer(agent.Entry{Kind: agent.KindThinking, Body: "想了想"})
	if n := s.dropped.Load(); n != 1 {
		t.Fatalf("thinking 到水位线应被丢，dropped=%d", n)
	}
	s.offer(agent.Entry{Kind: agent.KindText, Body: "重要"})
	if n := s.dropped.Load(); n != 1 {
		t.Fatalf("text 在水位之上、容量之下不该被丢，dropped=%d", n)
	}

	// 填满容量后 text 也丢
	for len(s.ch) < sinkCap {
		s.offer(agent.Entry{Kind: agent.KindText, Body: "y"})
	}
	s.offer(agent.Entry{Kind: agent.KindText, Body: "溢出"})
	if n := s.dropped.Load(); n != 2 {
		t.Fatalf("缓冲满后 text 也应被丢，dropped=%d", n)
	}
}

// 溢出记录由 Close 追加在最后，带丢弃计数，不静默丢。
func TestEventSinkCloseAppendsOverflowRecord(t *testing.T) {
	rec := &fakeEventRecorder{}
	s := newEventSink(context.Background(), rec, 42, "triage", "", "")
	s.OnEvent(assistantTextEvent("正常事件"))
	s.dropped.Add(7)
	s.Close()

	got := rec.collected()
	if len(got) != 2 {
		t.Fatalf("应为 1 条正常事件 + 1 条溢出记录，得到 %d 条", len(got))
	}
	last := got[len(got)-1].entry
	if last.Kind != agent.KindRaw || !strings.Contains(last.Body, "7") {
		t.Errorf("溢出记录不符: %+v", last)
	}
	if last.Payload["dropped"] != int64(7) {
		t.Errorf("溢出记录 payload 应带 dropped=7: %+v", last.Payload)
	}
}

// 未接线（recorder 为 nil）时整个机制消失：OnEvent/Close 都安全。
func TestEventSinkNilSafe(t *testing.T) {
	var s *EventSink
	s.OnEvent(assistantTextEvent("不会 panic"))
	s.Close()

	if got := newEventSink(context.Background(), nil, 1, "triage", "", ""); got != nil {
		t.Fatalf("recorder 为 nil 应返回 nil sink，得到 %v", got)
	}
}

// ---------------------------------------------------------------- subagent（0014）

// subagentFixture 在 CLAUDE_CONFIG_DIR 下摆出 claude 的目录布局，
// 返回那个 subagents 目录。
func subagentFixture(t *testing.T, cwd, session string) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	dir := agent.SubagentDir(agent.ProjectsRoot(), cwd, session)
	if dir == "" {
		t.Fatalf("拼不出 subagent 目录（cwd=%q session=%q）", cwd, session)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建夹具目录失败: %v", err)
	}
	return dir
}

const fixtureSubagentPrompt = `{"isSidechain":true,"agentId":"a1","type":"user",` +
	`"message":{"role":"user","content":"去找 group 模型的测试"},"uuid":"u1"}`

const fixtureSubagentTool = `{"isSidechain":true,"agentId":"a1","type":"assistant",` +
	`"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_S1","name":"Grep",` +
	`"input":{"pattern":"func TestGroup"}}]},"uuid":"u2"}`

// stdout 拿不到的那部分：subagent 的内部步骤要经由 watcher 落到同一条流上，
// 并带上 AgentID。
func TestEventSinkCollectsSubagentEvents(t *testing.T) {
	cwd, session := "/opt/lathe/workspaces/cr-1363", "sess-abc"
	dir := subagentFixture(t, cwd, session)

	rec := &fakeEventRecorder{}
	s := newEventSink(context.Background(), rec, 7, "implement", cwd, session)

	// sink 起好之后 agent 才派活 —— 这才是真实时序
	if err := os.WriteFile(filepath.Join(dir, "agent-a1.jsonl"),
		[]byte(fixtureSubagentPrompt+"\n"+fixtureSubagentTool+"\n"), 0o644); err != nil {
		t.Fatalf("写夹具失败: %v", err)
	}

	s.OnEvent(assistantTextEvent("主 agent 派了个子 agent"))
	// Close 会补最后一次轮询，因此不必等 3s 的 tick
	s.Close()

	var mainCount, subCount int
	var sawStart, sawTool bool
	for _, r := range rec.collected() {
		if r.phase != "implement" || r.taskID != 7 {
			t.Errorf("归属不符: task=%d phase=%s", r.taskID, r.phase)
		}
		if r.entry.AgentID == "" {
			mainCount++
			continue
		}
		subCount++
		if r.entry.AgentID != "a1" {
			t.Errorf("AgentID = %q，期望 a1", r.entry.AgentID)
		}
		switch r.entry.Kind {
		case agent.KindAgentStart:
			sawStart = true
			if !strings.Contains(r.entry.Body, "group 模型") {
				t.Errorf("分组头应是派活描述，得到 %q", r.entry.Body)
			}
		case agent.KindToolUse:
			sawTool = true
			if r.entry.Payload["toolUseId"] != "toolu_S1" {
				t.Errorf("subagent 的工具调用应保住配对 key: %+v", r.entry.Payload)
			}
		}
	}
	if mainCount != 1 {
		t.Errorf("主 agent 的事件数 = %d，期望 1", mainCount)
	}
	if subCount != 2 || !sawStart || !sawTool {
		t.Errorf("subagent 事件数 = %d（start=%v tool=%v），期望 2 条且两类都在", subCount, sawStart, sawTool)
	}
}

// 修复轮走 --resume，共用同一个 session ID：subagents/ 里上一轮的记录
// 已经落过库，不能再灌一遍。
func TestEventSinkSkipsPreexistingSubagentRecords(t *testing.T) {
	cwd, session := "/opt/lathe/workspaces/cr-1363", "sess-resume"
	dir := subagentFixture(t, cwd, session)

	// 上一轮留下的记录 —— 在 sink 起来之前就已存在
	path := filepath.Join(dir, "agent-a1.jsonl")
	if err := os.WriteFile(path, []byte(fixtureSubagentPrompt+"\n"+fixtureSubagentTool+"\n"), 0o644); err != nil {
		t.Fatalf("写夹具失败: %v", err)
	}

	rec := &fakeEventRecorder{}
	s := newEventSink(context.Background(), rec, 7, "fix-1", cwd, session)

	// 本轮新追加的一条
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("追加失败: %v", err)
	}
	newLine := `{"isSidechain":true,"agentId":"a1","type":"assistant",` +
		`"message":{"role":"assistant","content":[{"type":"text","text":"这轮的新发现"}]},"uuid":"u3"}`
	if _, err := f.WriteString(newLine + "\n"); err != nil {
		t.Fatalf("追加失败: %v", err)
	}
	f.Close()

	s.Close()

	var subEntries []agent.Entry
	for _, r := range rec.collected() {
		if r.entry.AgentID != "" {
			subEntries = append(subEntries, r.entry)
		}
	}
	if len(subEntries) != 1 {
		t.Fatalf("只应落本轮新增的 1 条，得到 %d 条: %+v", len(subEntries), subEntries)
	}
	if subEntries[0].Kind != agent.KindText || !strings.Contains(subEntries[0].Body, "新发现") {
		t.Errorf("落库的应是本轮那条，得到 %+v", subEntries[0])
	}
}

// 目录压根不存在（agent 没派活）是常态，不该报错也不该拖慢 Close。
func TestEventSinkSubagentDirAbsent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)

	rec := &fakeEventRecorder{}
	s := newEventSink(context.Background(), rec, 7, "implement", "/opt/lathe/workspaces/nope", "sess-x")
	s.OnEvent(assistantTextEvent("只有主 agent"))
	s.Close()

	got := rec.collected()
	if len(got) != 1 {
		t.Fatalf("应只有主 agent 那 1 条，得到 %d", len(got))
	}
	if got[0].entry.AgentID != "" {
		t.Errorf("主 agent 的事件不该带 AgentID: %+v", got[0].entry)
	}
}
