package runner

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
)

// AgentEventRecorder 把提炼后的 agent 事件与终局摘要落库。
//
// store.Store 实现此接口。沿用 VerificationRecorder 的立场：落库失败
// 只告警不阻断流水线 —— 可观测性不该反过来成为任务的故障源。
type AgentEventRecorder interface {
	InsertAgentEvents(ctx context.Context, taskID int64, phase string, entries []agent.Entry) error
	SetAgentSummary(ctx context.Context, taskID int64, summary string, costUSD float64, durationMS int64, numTurns int) error
}

// subagentPollEvery 是 subagent 记录的轮询间隔（0014）。
//
// 3s 而非更快：这是给人看进度的，秒级足够；而每轮要 stat + 读若干文件，
// 太密只是白烧 IO。3s 的间隔下，BatchSink 的排空能力（200ms 一批、
// 每批 20 条）足以吃掉一轮的产出，缓冲不会被冲垮。
const subagentPollEvery = 3 * time.Second

// PhaseImplementReentry 是「resume 失败降级、同工作区开新会话」那次调用的
// 阶段名（stageImplement 的降级分支）。
//
// 提成常量而不是就地写字面量：这个字符串是**跨分支的接口约定**（pipeline
// 的 injectFailureMemory 按它决定要不要注入失败记忆，A2 的 PR review 二轮
// 也按它写），同时是事件流的阶段标签 —— 两处各写一份字面量时，改一处就
// 静默改变另一处语义。
const PhaseImplementReentry = "implement-reentry"

// EventSink 把一个执行阶段的事件流批量落库（每任务每阶段一个）。
//
// 缓冲、溢出计数、批量刷库都在 agent.BatchSink 里（规划链路共用同一份）；
// EventSink 在它之上只多做一件事 —— 第二条数据源（0014）：subagent 的
// 内部活动不出现在 stdout 里，落在 ~/.claude/projects/<slug>/<session>/subagents/
// 下。sink 起一个 watcher 协程轮询那个目录，提炼出的条目投进同一个
// BatchSink，两条源在下游是同一条流。
type EventSink struct {
	batch *agent.BatchSink

	// reader 只被 watcher 协程访问（它不是并发安全的）。watcher 退出后
	// Close 才碰它，靠 watchDone 保证这个先后关系。
	reader    *agent.SubagentReader
	watchStop chan struct{}
	watchDone chan struct{}

	closed atomic.Bool
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
	write := func(ctx context.Context, entries []agent.Entry) error {
		return rec.InsertAgentEvents(ctx, taskID, phase, entries)
	}
	s := &EventSink{
		batch: agent.NewBatchSink(ctx, write, fmt.Sprintf("task=%d phase=%s", taskID, phase)),
	}
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
					s.batch.Offer(e)
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
		s.batch.Offer(e)
	}
}

// OnEvent 适配 agent.RunParams.OnEvent：提炼 + 非阻塞投递。
// nil 接收者合法（未接线时），让 pipeline 不必处处判空。
func (s *EventSink) OnEvent(ev agent.Event) {
	if s == nil || s.closed.Load() {
		return
	}
	s.batch.OnEvent(ev)
}

// Close 停止接收并 drain：缓冲区里剩余的事件全部落库后才返回。
// 成功与失败路径都必须调用 —— fail 时缓冲里恰是排障现场。幂等。
func (s *EventSink) Close() {
	if s == nil || !s.closed.CompareAndSwap(false, true) {
		return
	}
	// 先收尾 subagent 再停 flush 协程：最后一次轮询的产出也要走同一条落库路
	s.finishWatch()
	s.batch.Close()
}
