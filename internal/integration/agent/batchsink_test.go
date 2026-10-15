package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// recordingWriter 收集 BatchSink 写出的批次；write 在 flush 协程里调，须加锁。
type recordingWriter struct {
	mu   sync.Mutex
	rows []Entry
}

func (w *recordingWriter) write(_ context.Context, entries []Entry) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rows = append(w.rows, entries...)
	return nil
}

func (w *recordingWriter) collected() []Entry {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Entry(nil), w.rows...)
}

// Close 必须 drain：不依赖 200ms 的 tick，剩余条目全部落库且保序。
func TestBatchSinkDrainOnClose(t *testing.T) {
	w := &recordingWriter{}
	s := NewBatchSink(context.Background(), w.write, "test")
	for _, b := range []string{"一", "二", "三"} {
		s.Offer(Entry{Kind: KindText, Body: b})
	}
	s.Close()

	got := w.collected()
	if len(got) != 3 {
		t.Fatalf("drain 后应有 3 条，got %d", len(got))
	}
	for i, want := range []string{"一", "二", "三"} {
		if got[i].Body != want {
			t.Errorf("第 %d 条 got %q, want %q", i, got[i].Body, want)
		}
	}
}

// 缓冲满时优先丢 thinking 并计数（docs/04 §3.2）。
//
// 直接构造 struct、不起 flush 协程：channel 只进不出，水位行为是确定的。
// 因此本测试不调 Close（没有协程可 drain，会挂住）。
func TestBatchSinkOverflowDropsThinkingFirst(t *testing.T) {
	s := &BatchSink{ch: make(chan Entry, sinkCap)}

	for i := 0; i < thinkingWatermark; i++ {
		s.Offer(Entry{Kind: KindText, Body: "x"})
	}
	s.Offer(Entry{Kind: KindThinking, Body: "想了想"})
	if n := s.Dropped(); n != 1 {
		t.Fatalf("thinking 到水位线应被丢，dropped got %d want 1", n)
	}
	s.Offer(Entry{Kind: KindText, Body: "重要"})
	if n := s.Dropped(); n != 1 {
		t.Fatalf("text 在水位之上、容量之下不该被丢，dropped got %d want 1", n)
	}

	for len(s.ch) < sinkCap {
		s.Offer(Entry{Kind: KindText, Body: "y"})
	}
	s.Offer(Entry{Kind: KindText, Body: "溢出"})
	if n := s.Dropped(); n != 2 {
		t.Fatalf("缓冲满后 text 也应被丢，dropped got %d want 2", n)
	}
}

// 溢出记录由 Close 追加在最后，带丢弃计数，不静默丢。
func TestBatchSinkCloseAppendsOverflowRecord(t *testing.T) {
	w := &recordingWriter{}
	s := NewBatchSink(context.Background(), w.write, "test")
	s.Offer(Entry{Kind: KindText, Body: "正常事件"})
	s.dropped.Add(7)
	s.Close()

	got := w.collected()
	if len(got) != 2 {
		t.Fatalf("应为 1 条正常事件 + 1 条溢出记录，got %d 条", len(got))
	}
	last := got[len(got)-1]
	if last.Kind != KindRaw || !strings.Contains(last.Body, "7") {
		t.Errorf("溢出记录不符: %+v", last)
	}
	if last.Payload["dropped"] != int64(7) {
		t.Errorf("溢出记录 payload got %v, want dropped=7", last.Payload)
	}
}

// write 为 nil 时整个机制消失，nil 接收者上的方法都安全。
func TestBatchSinkNilSafe(t *testing.T) {
	s := NewBatchSink(context.Background(), nil, "test")
	if s != nil {
		t.Fatalf("write 为 nil 应返回 nil sink")
	}
	s.Offer(Entry{Kind: KindText})
	s.OnEvent(Event{Type: EventAssistant})
	s.Close()
	if s.Dropped() != 0 {
		t.Errorf("nil sink 的 Dropped 应为 0")
	}
}
