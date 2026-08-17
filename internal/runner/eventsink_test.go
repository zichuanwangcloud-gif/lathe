package runner

import (
	"context"
	"encoding/json"
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
	s := newEventSink(context.Background(), rec, 42, "implement")

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
	s := newEventSink(context.Background(), rec, 42, "triage")
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

	if got := newEventSink(context.Background(), nil, 1, "triage"); got != nil {
		t.Fatalf("recorder 为 nil 应返回 nil sink，得到 %v", got)
	}
}
