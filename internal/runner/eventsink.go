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
	// subagentPollEvery 是 subagent 记录的轮询间隔（0014）。
	//
	// 3s 而非更快：这是给人看进度的，秒级足够；而每轮要 stat + 读若干文件，
	// 太密只是白烧 IO。3s 的间隔下，EventSink 的排空能力（200ms 一批、
	// 每批 20 条）足以吃掉一轮的产出，缓冲不会被冲垮。
	subagentPollEvery = 3 * time.Second
)

// EventSink 把一个执行阶段的事件流批量落库（每任务每阶段一个）。
//
// 数据通路：agent.Driver 的 OnEvent → Digest 提炼 → 有界 channel →
// 单 flush 协程批量 INSERT。OnEvent 只做提炼与非阻塞投递，把 DB 往返
// 从 stdout 读取回路里摘出去。
//
// 第二条数据源（0014）：subagent 的内部活动不出现在 stdout 里，落在
// ~/.claude/projects/<slug>/<session>/subagents/ 下。sink 起一个 watcher
// 协程轮询那个目录，提炼出的条目并入同一个 channel —— 于是落库、缓冲、
// 溢出计数这些机制全部复用，两条源在下游是同一条流。
type EventSink struct {
	rec     AgentEventRecorder
	taskID  int64
	phase   string
	baseCtx context.Context

	ch      chan agent.Entry
	stop    chan struct{}
	flushed chan struct{} // flush 协程退出时关闭

	// reader 只被 watcher 协程访问（它不是并发安全的）。watcher 退出后
	// Close 才碰它，靠 watchDone 保证这个先后关系。
	reader    *agent.SubagentReader
	watchStop chan struct{}
	watchDone chan struct{}

	closed  atomic.Bool
	dropped atomic.Int64
}

// newEventSink 启动一个 sink 并立即起 flush 协程。
// rec 为 nil 时返回 nil —— 调用方（含测试）不接线则整个机制消失。
//
// cwd 与 sessionID 用于定位 subagent 记录目录；任一为空则只走 stdout 一条源
// （例如测试里不关心 subagent 时）。
func newEventSink(ctx context.Context, rec AgentEventRecorder, taskID int64, phase, cwd, sessionID string) *EventSink {
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
	s.startWatch(cwd, sessionID)
	return s
}

// startWatch 起 subagent 记录的轮询协程。
//
// 目录不存在是常态（agent 压根没派活），此时 Poll 一直返回空 —— 不预先
// 判断目录存在性，因为它是在 agent 跑起来之后才被创建的。
func (s *EventSink) startWatch(cwd, sessionID string) {
	dir := agent.SubagentDir(agent.ProjectsRoot(), cwd, sessionID)
	if dir == "" {
		return
	}
	s.reader = agent.NewSubagentReader(dir)
	// 只读本次执行新追加的内容：--resume 的修复轮共用同一个 session ID，
	// 上一轮的记录已经落过库了（见 SeekToEnd 的注释）。
	s.reader.SeekToEnd()

	s.watchStop = make(chan struct{})
	s.watchDone = make(chan struct{})
	go func() {
		defer close(s.watchDone)
		t := time.NewTicker(subagentPollEvery)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				for _, e := range s.reader.Poll() {
					s.offer(e)
				}
			case <-s.watchStop:
				return
			}
		}
	}()
}

// finishWatch 停掉 watcher 并补最后一次轮询。
//
// 这一次不能省：agent 刚退出，它最后几秒写下的 subagent 记录还没被轮到 ——
// 而那恰恰常常是「它到底卡在哪」的部分。
func (s *EventSink) finishWatch() {
	if s.reader == nil {
		return
	}
	close(s.watchStop)
	<-s.watchDone // watcher 已退出，此后 reader 归 Close 独占

	for _, e := range s.reader.Poll() {
		s.offer(e)
	}
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
	// 先收尾 subagent 再停 flush 协程：最后一次轮询的产出也要走同一条落库路
	s.finishWatch()

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
