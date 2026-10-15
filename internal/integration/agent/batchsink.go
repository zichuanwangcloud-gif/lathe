package agent

// batchsink.go 事件的有界缓冲 + 批量落库。
//
// 原来只有 runner.EventSink 有这套机制，PRD 规划链路（internal/prd）当时
// 嫌 EventSink 绑死了 task_id，自己用一个无界切片把整轮事件攒在内存里，
// 等 agent 跑完才一次性写库：一轮最长 10 分钟，内存跟着事件数涨，界面上
// 的「实时」事件流在一轮跑完之前也什么都拉不到。
//
// 抽到 agent 包是因为 Entry 本来就在这里，runner 与 prd 都已依赖本包 ——
// 放进 runner 会逼 prd 依赖 runner（prd 包刻意避开了这条依赖）。
// 写到哪张表由调用方给的 BatchWriter 决定，缓冲、溢出、刷库节奏只有一份。

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

// 缓冲与刷库参数（docs/04 §3.2）。
const (
	// sinkCap 是缓冲上限。Offer 在 driver 的读协程里同步执行，绝不允许
	// 阻塞 —— 缓冲满时丢事件（优先 thinking）并计数，Close 时补一条溢出
	// 记录，不静默丢。
	sinkCap = 256
	// thinkingWatermark 是 thinking 事件的投递水位：缓冲占用超过它时
	// 只丢 thinking（界面价值最低、量最大的一类），给 text/tool_use
	// 这些高价值事件留出余量。
	thinkingWatermark = 192
	// sinkFlushEvery 与 sinkFlushBatch 是刷库节奏：到点或攒满即批量写。
	sinkFlushEvery = 200 * time.Millisecond
	sinkFlushBatch = 20
	// sinkWriteTimeout 限制单次落库耗时。用 WithoutCancel 的 ctx：
	// 调用方 ctx 取消（失败/超时路径）时缓冲区里恰是排障最关键的现场，
	// 不能跟着 ctx 一起被掐掉。
	sinkWriteTimeout = 5 * time.Second
)

// BatchWriter 把一批事件写进持久层。返回错误只会被记日志、该批丢弃 ——
// 可观测性不该反过来成为任务的故障源。
type BatchWriter func(ctx context.Context, entries []Entry) error

// BatchSink 是有界缓冲 + 单 flush 协程的批量落库器。
//
// 数据通路：Offer（非阻塞投递）→ 有界 channel → flush 协程到点或攒满批量写。
type BatchSink struct {
	write   BatchWriter
	label   string // 日志里标明是谁的事件，如 "task=42 phase=implement"
	baseCtx context.Context

	ch      chan Entry
	stop    chan struct{}
	flushed chan struct{} // flush 协程退出时关闭

	closed  atomic.Bool
	dropped atomic.Int64
}

// NewBatchSink 启动一个 sink 并立即起 flush 协程。write 为 nil 时返回 nil，
// nil 接收者上的所有方法都是空操作 —— 调用方不接线则整个机制消失。
func NewBatchSink(ctx context.Context, write BatchWriter, label string) *BatchSink {
	if write == nil {
		return nil
	}
	s := &BatchSink{
		write:   write,
		label:   label,
		baseCtx: ctx,
		ch:      make(chan Entry, sinkCap),
		stop:    make(chan struct{}),
		flushed: make(chan struct{}),
	}
	go s.flushLoop()
	return s
}

// OnEvent 适配 RunParams.OnEvent：提炼 + 非阻塞投递。
func (s *BatchSink) OnEvent(ev Event) {
	if s == nil || s.closed.Load() {
		return
	}
	for _, e := range Digest(ev) {
		s.Offer(e)
	}
}

// Offer 非阻塞地投递一条已提炼的条目。缓冲紧张时丢弃并计数。
func (s *BatchSink) Offer(e Entry) {
	if s == nil {
		return
	}
	// thinking 走低水位：缓冲一紧张先丢它
	limit := sinkCap
	if e.Kind == KindThinking {
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

// Dropped 返回至今丢弃的条数。
func (s *BatchSink) Dropped() int64 {
	if s == nil {
		return 0
	}
	return s.dropped.Load()
}

// Close 停止接收并 drain：缓冲区里剩余的事件全部落库后才返回，
// 有丢弃时在最后补一条溢出记录。幂等。成功与失败路径都必须调用。
func (s *BatchSink) Close() {
	if s == nil || !s.closed.CompareAndSwap(false, true) {
		return
	}
	close(s.stop)
	<-s.flushed

	// 溢出记录补在最后一条，保证时间线顺序：丢了多少、丢了哪类，界面可见
	if n := s.dropped.Load(); n > 0 {
		s.insert([]Entry{{
			Kind: KindRaw,
			Body: fmt.Sprintf("事件缓冲溢出，丢弃 %d 条（优先丢弃 thinking）", n),
			Payload: map[string]any{
				"type":    "overflow",
				"dropped": n,
			},
		}})
	}
}

func (s *BatchSink) flushLoop() {
	defer close(s.flushed)
	ticker := time.NewTicker(sinkFlushEvery)
	defer ticker.Stop()

	buf := make([]Entry, 0, sinkFlushBatch)
	flush := func() {
		if len(buf) == 0 {
			return
		}
		// 交出去的切片可能被 writer 持有（如测试假件），每批用新底层数组
		s.insert(buf)
		buf = make([]Entry, 0, sinkFlushBatch)
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
			// drain：ch 从不 close（Close 后 Offer 仍可能在非阻塞投递），
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

func (s *BatchSink) insert(entries []Entry) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.baseCtx), sinkWriteTimeout)
	defer cancel()
	if err := s.write(ctx, entries); err != nil {
		slog.Warn("agent 事件落库失败（已丢弃该批）", "sink", s.label, "条数", len(entries), "err", err)
	}
}
