package runner

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/Clouditera/lathe/internal/integration/agent"
)

// AgentEventRecorder 把提炼后的 agent 事件与终局摘要落库。
//
// store.Store 实现此接口。沿用 VerificationRecorder 的立场：落库失败
// 只告警不阻断流水线 —— 可观测性不该反过来成为任务的故障源。
type AgentEventRecorder interface {
	InsertAgentEvents(ctx context.Context, taskID int64, phase string, entries []agent.Entry) error
	SetAgentSummary(ctx context.Context, taskID int64, summary string, costUSD float64, durationMS int64, numTurns int) error
}

// EventSink 的缓冲与刷库参数（docs/04 §3.2）。
const (
	// sinkCap 是每任务事件缓冲的上限。OnEvent 在驱动的读协程里同步
	// 执行，绝不允许阻塞 —— 缓冲满时丢事件（优先 thinking）并计数，
	// 任务结束时补一条溢出记录，不静默丢。
	sinkCap = 256
	// thinkingWatermark 是 thinking 事件的投递水位：缓冲占用超过它时
	// 只丢 thinking（界面价值最低、量最大的一类），给 text/tool_use
	// 这些高价值事件留出余量。
	thinkingWatermark = 192
	// sinkFlushEvery 与 sinkFlushBatch 是刷库节奏：到点或攒满即批量 INSERT。
	sinkFlushEvery = 200 * time.Millisecond
	sinkFlushBatch = 20
	// sinkWriteTimeout 限制单次落库耗时。用 WithoutCancel 的 ctx：
	// 任务 ctx 取消（fail/超时路径）时缓冲区里恰是排障最关键的现场，
	// 不能跟着 ctx 一起被掐掉。
	sinkWriteTimeout = 5 * time.Second
)

// EventSink 把一个执行阶段的事件流批量落库（每任务每阶段一个）。
//
// 数据通路：agent.Driver 的 OnEvent → Digest 提炼 → 有界 channel →
// 单 flush 协程批量 INSERT。OnEvent 只做提炼与非阻塞投递，把 DB 往返
// 从 stdout 读取回路里摘出去。
type EventSink struct {
	rec     AgentEventRecorder
	taskID  int64
	phase   string
	baseCtx context.Context

	ch      chan agent.Entry
	stop    chan struct{}
	flushed chan struct{} // flush 协程退出时关闭

	closed  atomic.Bool
	dropped atomic.Int64
}

// newEventSink 启动一个 sink 并立即起 flush 协程。
// rec 为 nil 时返回 nil —— 调用方（含测试）不接线则整个机制消失。
func newEventSink(ctx context.Context, rec AgentEventRecorder, taskID int64, phase string) *EventSink {
	if rec == nil {
		return nil
	}
	s := &EventSink{
		rec:     rec,
		taskID:  taskID,
		phase:   phase,
		baseCtx: ctx,
		ch:      make(chan agent.Entry, sinkCap),
		stop:    make(chan struct{}),
		flushed: make(chan struct{}),
	}
	go s.flushLoop()
	return s
}

// OnEvent 适配 agent.RunParams.OnEvent：提炼 + 非阻塞投递。
// nil 接收者合法（未接线时），让 pipeline 不必处处判空。
func (s *EventSink) OnEvent(ev agent.Event) {
	if s == nil || s.closed.Load() {
		return
	}
	for _, e := range agent.Digest(ev) {
		s.offer(e)
	}
}

func (s *EventSink) offer(e agent.Entry) {
	// thinking 走低水位：缓冲一紧张先丢它
	limit := sinkCap
	if e.Kind == agent.KindThinking {
		limit = thinkingWatermark
	}
	if len(s.ch) >= limit {
		s.dropped.Add(1)
		return
	}
	select {
	case s.ch <- e:
	default:
		s.dropped.Add(1)
	}
}

// Close 停止接收并 drain：缓冲区里剩余的事件全部落库后才返回。
// 成功与失败路径都必须调用 —— fail 时缓冲里恰是排障现场。
func (s *EventSink) Close() {
	if s == nil || !s.closed.CompareAndSwap(false, true) {
		return
	}
	close(s.stop)
	<-s.flushed

	// 溢出记录补在最后一条，保证时间线顺序：丢了多少、丢了哪类，界面可见
	if n := s.dropped.Load(); n > 0 {
		s.insert([]agent.Entry{{
			Kind: agent.KindRaw,
			Body: fmt.Sprintf("事件缓冲溢出，丢弃 %d 条（优先丢弃 thinking）", n),
			Payload: map[string]any{
				"type":    "overflow",
				"dropped": n,
			},
		}})
	}
}

func (s *EventSink) flushLoop() {
	defer close(s.flushed)
	ticker := time.NewTicker(sinkFlushEvery)
	defer ticker.Stop()

	buf := make([]agent.Entry, 0, sinkFlushBatch)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		s.insert(buf)
		buf = buf[:0]
	}

	for {
		select {
		case e := <-s.ch:
			buf = append(buf, e)
			if len(buf) >= sinkFlushBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-s.stop:
			// drain：ch 从不 close（Close 后 offer 仍在非阻塞投递），
			// 读到空为止，剩余一批不落就丢现场了
			for {
				select {
				case e := <-s.ch:
					buf = append(buf, e)
					if len(buf) >= sinkFlushBatch {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

func (s *EventSink) insert(entries []agent.Entry) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.baseCtx), sinkWriteTimeout)
	defer cancel()
	if err := s.rec.InsertAgentEvents(ctx, s.taskID, s.phase, entries); err != nil {
		slog.Warn("agent 事件落库失败（已丢弃该批）", "task", s.taskID, "phase", s.phase, "条数", len(entries), "err", err)
	}
}
