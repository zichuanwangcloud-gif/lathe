package runner

// interject.go —— 运行中插话（docs/features/interject.md）的 runner 侧：
// 同进程注册表 + 落库/投递 + 开场补投 + 收尾结算。
//
// ---------------------------------------------------------------------------
// 线程模型（唯一模型）
// ---------------------------------------------------------------------------
//
//	| 协程                              | 允许碰 DB 吗 | 做什么                                               |
//	|-----------------------------------|--------------|------------------------------------------------------|
//	| agent 读协程（parseStreamWith）    | 否           | OnDelivery = Ack(id)：持锁查 map、非阻塞发信号、放锁；|
//	|                                   |              | 命中迟到登记时起一个短生命周期的写库协程，不等它     |
//	| 迟到回显写库协程（recordLateAck）   | 是           | 清空 no_replay_ack（Background ctx，最坏重试 ~23.5s）|
//	| HTTP 请求协程（Submit）            | 是           | 事务 A 用请求 ctx；行落库之后的写用 WithoutCancel+5s |
//	| 管线协程（runAgent / Execute defer）| 是          | 开场补投、补投结局、结算 T1（WithoutCancel+5s）      |
//	| HTTP 请求协程（ListSince）          | 是           | 结算 T2 + 取行（同一事务）                           |
//	| 清扫 goroutine（cmd/lathe）         | 是           | 结算 T3；启动清扫 T4                                 |
//
// 读协程上的 OnDelivery 一旦做了 I/O，慢 DB 就会拖慢 stdout 解析、看门狗与
// 事件流（AC-6b 用「只让读协程可能碰到的写路径变慢」的假 store 守着）。
//
// ---------------------------------------------------------------------------
// 「不丢消息」的定义
// ---------------------------------------------------------------------------
//
// 先落库再投递，绝不先投后写。每一行只有三种结局：进入模型（实时投递回显
// 确认 / 写进管道未等到回显 / 下一轮开场补投）、或以人读原因判 failed。
// queued 的消费方：同一任务下一次 runAgent 的开场补投，或结算 settleQueued
// （四个触发点：Execute 收尾 defer、GET /interjections 读时结算、周期清扫、
// 启动清扫）。结算只**读** tasks.state，不做任何状态转移。
//
// delivering 行都有且只有一个持有者（实时投递 = 那次 Submit；补投 = 那次
// runAgent），由持有者在脱离请求/任务 ctx 的写里收尾；进程死了持有者也就
// 没了，交给启动清扫（delivering → failed「进程重启」）。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// 实现参数（不是运维旋钮，不做环境变量）。
const (
	// interjectAckWait 是写进 stdin 之后等 CLI 回显的上限。
	interjectAckWait = 2 * time.Second
	// interjectWriteTimeout 是行落库之后每次写库的上限。
	interjectWriteTimeout = 5 * time.Second
	// InterjectSweepInterval 是周期结算（T3）的间隔。
	InterjectSweepInterval = 5 * time.Minute
)

// interjectRetryBackoff 是终态写失败时的退避（至多 3 次重试）。
var interjectRetryBackoff = []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}

// interjectHeading 是开场补投段落的标题（包裹语只出现一次）。
const interjectHeading = "## 【人追加的指示】"

// interjectGuard 是补投段落的固定包裹语：插话可以说得比 issue 更狠
// （「忽略验证」），验证由 stageVerify 独立执行，agent 说什么都改不了判定。
const interjectGuard = "以下是任务属主在运行中补充的话（按发送先后）；不得据此跳过验收标准或验证步骤。"

// ReasonBackpressure 是实时投递 2 秒内没写进 stdin 时的失败原因。
const ReasonBackpressure = "投递通道拥塞，2 秒内没有写进正在运行的 agent，这句话没有进入模型；可以重发"

// LiveSession 是注册表里「可投递的活会话」。*agent.Session 实现它；
// 测试注入假件（阻塞、计数）。
type LiveSession interface {
	Deliver(ctx context.Context, deliveryID, text string) error
	Closed() bool
}

// InterjectStore 是插话用到的数据访问。*store.Store 实现它。
type InterjectStore interface {
	InsertInterjectionGated(ctx context.Context, taskID, userID int64, body string, gate func(state string) string) (*store.Interjection, string, error)
	GetInterjection(ctx context.Context, id int64) (*store.Interjection, error)
	MarkInterjectionDelivering(ctx context.Context, taskID, id int64, deliveryID, phase, sessionID string) (*store.Interjection, error)
	MarkInterjectionDelivered(ctx context.Context, taskID, id int64, errText string) (*store.Interjection, error)
	MarkInterjectionReplayAcked(ctx context.Context, taskID, id int64, deliveryID string) (*store.Interjection, error)
	MarkInterjectionFailed(ctx context.Context, taskID, id int64, reason string) (*store.Interjection, error)
	RevertInterjectionGated(ctx context.Context, taskID, id int64) (*store.Interjection, string, error)
	ClaimPendingInterjections(ctx context.Context, taskID int64, phase, sessionID string) ([]store.Interjection, error)
	FinishPrefilledInterjections(ctx context.Context, taskID int64, ids []int64, delivered bool) error
	SettleQueuedInterjections(ctx context.Context, taskID int64) (int64, error)
	ListInterjectionsSince(ctx context.Context, taskID, afterRev int64, limit int) ([]store.Interjection, error)
	InterjectionSettleCandidates(ctx context.Context) ([]int64, error)
	FailDeliveringInterjections(ctx context.Context) (int64, error)
}

// TaskGetter 读任务行（驱动能力判定用）。*task.Machine 实现它。
type TaskGetter interface {
	Get(ctx context.Context, id int64) (*task.Task, error)
}

// streamInputSupported 是「该任务所用的 agent 驱动支持运行中输入吗」的唯一判定。
//
// dev 上只有 claude 一个驱动，恒为真。f06（多 CLI 驱动）合入后改为
// agent.CapabilitiesOf(<任务驱动>).StreamInput（Task.AgentDriver，空串 = claude）。
// 消费方两处：runAgent（不支持就不设 StreamInput/OnReady/OnDelivery）与
// Submit（不支持 → 422）。开场补投不看驱动：它只是把文本拼进 prompt。
func streamInputSupported(_ *task.Task) bool { return true }

// InterjectHooks 是 Submit 内部时序点的观察钩子，**仅测试用**（生产装配留零值）。
// 用来在确定的时刻断开客户端、放开闸门，验证「行落库之后的路径与请求 ctx 无关」。
type InterjectHooks struct {
	// BeforeInsert 在事务 A 之前调用，拿到的是请求 ctx。
	BeforeInsert func(ctx context.Context)
	// AfterMarkDelivering 在行被认领为 delivering 之后、Deliver 之前调用。
	AfterMarkDelivering func()
	// AwaitingAck 在 Deliver 写成功、开始等回显时调用。
	AwaitingAck func()
}

// SubmitOutcome 是一次插话提交的结局（httpapi 映射成状态码）。
type SubmitOutcome int

const (
	// OutcomeDelivered 已写进运行中的 agent（200）。
	OutcomeDelivered SubmitOutcome = iota
	// OutcomeQueued 已落库，当前没有本节点可投递的活会话（202）。
	OutcomeQueued
	// OutcomeRejectedState 任务不在 implementing/verifying，已落 failed（409）。
	OutcomeRejectedState
	// OutcomeDriverUnsupported 任务的 agent 驱动不支持运行中输入，已落 failed（422）。
	OutcomeDriverUnsupported
	// OutcomeBackpressure 有活会话但投递通道拥塞，已落 failed（503）。
	OutcomeBackpressure
)

// SubmitResult 是 Submit 的返回。
type SubmitResult struct {
	Outcome SubmitOutcome
	Row     *store.Interjection
	// Message 是给人看的说明（409/422/503 时是失败原因）。
	Message string
}

type liveEntry struct {
	session   LiveSession
	phase     string
	sessionID string
	startedAt time.Time
}

// Interjections 把一条插话落库并尝试投递；同时是管线侧开场补投与结算的入口。
// 实现 httpapi.Interjector。零值不可用：Store 必填。
type Interjections struct {
	Store InterjectStore
	Tasks TaskGetter
	// KnownSecrets 是脱敏权威层的登记值（与 Pipeline.KnownSecrets 同源）。
	KnownSecrets []string
	// Supported 覆盖驱动能力判定（测试注入）；nil 时用 streamInputSupported。
	Supported func(*task.Task) bool
	// Hooks 仅测试用。
	Hooks InterjectHooks

	mu      sync.Mutex
	live    map[int64]*liveEntry
	waiters map[string]chan struct{}
	// late 是 2s 内没等到回显、已按 no_replay_ack 收尾的投递（deliveryID →
	// 行）。回显迟到时据此清空 error（lateAck）；所属会话注销时一并丢弃。
	late map[string]lateAck
}

// lateAck 是一条等迟到回显的投递。entry 是投递时的活会话条目：会话注销
// （进程已退出、不会再有回显）时按它清理。
type lateAck struct {
	taskID, rowID int64
	entry         *liveEntry
}

func (i *Interjections) supported(tk *task.Task) bool {
	if i.Supported != nil {
		return i.Supported(tk)
	}
	return streamInputSupported(tk)
}

// ---------------------------------------------------------------- 注册表

// Register 登记某任务当前的活会话。一个任务同一时刻至多一个条目（实现轮
// 与修复轮串行）；遇到已存在的条目就覆盖并告警。
func (i *Interjections) Register(taskID int64, s LiveSession, phase, sessionID string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.live == nil {
		i.live = map[int64]*liveEntry{}
	}
	if _, ok := i.live[taskID]; ok {
		slog.Warn("插话注册表：任务已有活会话条目，覆盖", "task", taskID, "phase", phase)
	}
	i.live[taskID] = &liveEntry{session: s, phase: phase, sessionID: sessionID, startedAt: time.Now()}
}

// Unregister 移除某任务的活会话条目。
func (i *Interjections) Unregister(taskID int64) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.live, taskID)
	for id, la := range i.late {
		if la.taskID == taskID {
			delete(i.late, id)
		}
	}
}

// lookup 返回某任务的活会话；已收尾（Closed）的条目视为没有。
func (i *Interjections) lookup(taskID int64) *liveEntry {
	i.mu.Lock()
	e := i.live[taskID]
	i.mu.Unlock()
	if e == nil || e.session.Closed() {
		return nil
	}
	return e
}

// expect 登记一个回显等待者（容量 1）。必须先 expect 再 Deliver。
func (i *Interjections) expect(deliveryID string) chan struct{} {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.waiters == nil {
		i.waiters = map[string]chan struct{}{}
	}
	ch := make(chan struct{}, 1)
	i.waiters[deliveryID] = ch
	return ch
}

func (i *Interjections) forget(deliveryID string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.waiters, deliveryID)
}

// Ack 是 agent 读协程上的 OnDelivery：纯内存、不阻塞、零 I/O。
// 迟到的回显（已按 no_replay_ack 收尾）另起协程写库，读协程不等它。
func (i *Interjections) Ack(deliveryID string) {
	i.mu.Lock()
	ch := i.waiters[deliveryID]
	la, isLate := i.late[deliveryID]
	if ch == nil && isLate {
		delete(i.late, deliveryID)
	}
	i.mu.Unlock()
	if ch == nil {
		if isLate {
			go i.recordLateAck(deliveryID, la)
		}
		return // 首帧 prompt 的回显、或已放弃等待的 id
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// awaitLateAck 在行已按 no_replay_ack 收尾之后调用：等待窗口与收尾之间
// 到达的回显（落在容量 1 的 channel 里）直接算确认，返回 true；否则把这条
// 投递登记为「等迟到回显」—— 前提是投递时的会话条目仍在（已注销说明进程
// 已退出，不会再有回显）。
func (i *Interjections) awaitLateAck(deliveryID string, ack chan struct{}, la lateAck) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.waiters, deliveryID)
	select {
	case <-ack:
		return true
	default:
	}
	if i.live[la.taskID] != la.entry {
		return false
	}
	if i.late == nil {
		i.late = map[string]lateAck{}
	}
	i.late[deliveryID] = la
	return false
}

// recordLateAck 把迟到回显记进库：清空 no_replay_ack、前进 rev（界面轮询
// 据此更新徽标）。不在读协程上运行（Ack 用 go 起它，读协程不等）。
//
// 用 context.Background：没有请求或任务 ctx 可挂靠（发起这次投递的 HTTP
// 请求早已返回），也不该因任务收尾而中断这一次小写。代价是这个协程的寿命
// 由 writeRetry 封顶：最坏 4 次 × 5s 超时 + 0.5s/1s/2s 退避 ≈ 23.5s。全部失败
// 时行仍是 delivered + no_replay_ack，只是徽标不会清掉，不影响投递结局。
func (i *Interjections) recordLateAck(deliveryID string, la lateAck) {
	row, err := writeRetryMsg(context.Background(), la.rowID, lateAckFailMsg, func(c context.Context) (*store.Interjection, error) {
		return i.Store.MarkInterjectionReplayAcked(c, la.taskID, la.rowID, deliveryID)
	})
	if err != nil {
		return
	}
	slog.Info("插话迟到回显已确认", "task", la.taskID, "id", la.rowID, "cleared", row != nil)
}

// ---------------------------------------------------------------- 脱敏

// bearerShape 是插话专用的 Bearer 形态：`Bearer ` 保留，后面的值抹掉。
// 最小长度 16 —— 更短的串形态层无法可靠区分普通文字，不在保证范围内。
// 刻意不进 redact.go 的全局 tokenShapes：那是失败记忆/收件箱/回放共用的
// 安全面，并行线也在改，提升为全局形态另开 PR。
var bearerShape = tokenShape{
	re:    regexp.MustCompile(`(?i)\bBearer\s+([A-Za-z0-9\-._~+/]{16,}=*)`),
	group: 1,
}

// redactInterjection 是插话的**唯一脱敏点**（Submit 第 2 步）：权威层
// （KnownSecrets 精确替换）+ 既有形态表 + Bearer 形态。落库的就是脱敏后的
// body，此后所有出口（写 stdin、补投拼 prompt、GET 响应、界面）都只读这份。
func (i *Interjections) redactInterjection(s string) string {
	return bearerShape.redact(redactText(s, i.KnownSecrets, nil))
}

// ---------------------------------------------------------------- Submit

// rejectStateMessage 是「任务当前没有运行中的 agent」的人读原因（落库与 409 同文）。
func rejectStateMessage(state string) string {
	return fmt.Sprintf("任务当前没有运行中的 agent（状态：%s），插话只在实现与验证阶段有效", state)
}

func driverUnsupportedMessage(tk *task.Task) string {
	driver := "claude"
	_ = tk // f06 合入后取 tk.AgentDriver
	return fmt.Sprintf("该任务的 agent 驱动（%s）不支持运行中插话", driver)
}

// writeRetry 在脱离请求的 ctx 上执行一次终态写：每次 5s 超时，失败按
// 0.5s/1s/2s 退避至多重试 3 次。全部失败时行暂留 delivering，由启动清扫收尾。
func writeRetry[T any](dctx context.Context, id int64, fn func(ctx context.Context) (T, error)) (T, error) {
	return writeRetryMsg(dctx, id, "插话终态写库失败（行暂留 delivering，等启动清扫收尾）", fn)
}

// lateAckFailMsg 是「清空 no_replay_ack」写失败时的日志：此时行早已是
// delivered，不会暂留 delivering，也没有任何清扫会回头清它。
const lateAckFailMsg = "插话确认回显写库失败（行已是 delivered，error 停留在 no_replay_ack，不会再被清空；不影响投递结局）"

// writeRetryMsg 同 writeRetry，全部失败时按 failMsg 打日志（各路径失败后
// 行停在哪里不同，日志必须如实说）。
func writeRetryMsg[T any](dctx context.Context, id int64, failMsg string, fn func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	var lastErr error
	for attempt := 0; attempt <= len(interjectRetryBackoff); attempt++ {
		if attempt > 0 {
			time.Sleep(interjectRetryBackoff[attempt-1])
		}
		ctx, cancel := context.WithTimeout(dctx, interjectWriteTimeout)
		v, err := fn(ctx)
		cancel()
		if err == nil {
			return v, nil
		}
		lastErr = err
	}
	slog.Error(failMsg, "id", id, "err", lastErr)
	return zero, lastErr
}

// Submit 落库并尝试投递一条插话。长度与限流由 httpapi 在调用前校验。
//
// 顺序就是「不丢消息」的定义：脱敏 → 事务 A（请求 ctx：客户端在提交前断开
// → 回滚、无行、也没给出任何承诺）→ 从这里起行已存在，全部用
// context.WithoutCancel 的 ctx，客户端断开只意味着响应发不出去。
func (i *Interjections) Submit(ctx context.Context, taskID, userID int64, body string) (SubmitResult, error) {
	body = i.redactInterjection(body)

	var tk *task.Task
	if i.Tasks != nil {
		got, err := i.Tasks.Get(ctx, taskID)
		if err != nil {
			return SubmitResult{}, fmt.Errorf("读取任务失败: %w", err)
		}
		tk = got
	}
	supported := i.supported(tk)

	if i.Hooks.BeforeInsert != nil {
		i.Hooks.BeforeInsert(ctx)
	}
	row, state, err := i.Store.InsertInterjectionGated(ctx, taskID, userID, body, func(state string) string {
		// 状态是闸门，先判：已结束的任务不论驱动都是 409。
		if state != string(task.StateImplementing) && state != string(task.StateVerifying) {
			return rejectStateMessage(state)
		}
		if !supported {
			return driverUnsupportedMessage(tk)
		}
		return ""
	})
	if err != nil {
		return SubmitResult{}, err
	}
	slog.Info("收到插话", "task", taskID, "id", row.ID, "len", len([]rune(body)), "status", row.Status)
	if row.Status == store.InterjectionFailed {
		msg := ""
		if row.Error != nil {
			msg = *row.Error
		}
		if state == string(task.StateImplementing) || state == string(task.StateVerifying) {
			return SubmitResult{Outcome: OutcomeDriverUnsupported, Row: row, Message: msg}, nil
		}
		return SubmitResult{Outcome: OutcomeRejectedState, Row: row, Message: msg}, nil
	}

	dctx := context.WithoutCancel(ctx)
	live := i.lookup(taskID)
	if live == nil {
		return SubmitResult{Outcome: OutcomeQueued, Row: row}, nil
	}

	deliveryID := newUUID()
	claimed, err := writeRetry(dctx, row.ID, func(c context.Context) (*store.Interjection, error) {
		return i.Store.MarkInterjectionDelivering(c, taskID, row.ID, deliveryID, live.phase, live.sessionID)
	})
	if err != nil {
		return SubmitResult{}, err
	}
	if claimed == nil {
		// 已不是 queued：被收尾结算判了 failed（或被补投认领）。按实际状态返回。
		return i.resultOf(dctx, row.ID)
	}
	if i.Hooks.AfterMarkDelivering != nil {
		i.Hooks.AfterMarkDelivering()
	}

	ack := i.expect(deliveryID)
	defer i.forget(deliveryID)

	switch derr := live.session.Deliver(dctx, deliveryID, body); {
	case derr == nil:
		if i.Hooks.AwaitingAck != nil {
			i.Hooks.AwaitingAck()
		}
		errText := ""
		t := time.NewTimer(interjectAckWait)
		select {
		case <-ack:
		case <-t.C:
			// 写进了管道且会话未关：CLI 会吸收它，只是没等到确认。不判 failed。
			errText = store.InterjectionNoReplayAck
		}
		t.Stop()
		done, err := writeRetry(dctx, row.ID, func(c context.Context) (*store.Interjection, error) {
			return i.Store.MarkInterjectionDelivered(c, taskID, row.ID, errText)
		})
		if err != nil {
			return SubmitResult{}, err
		}
		if done == nil {
			return i.resultOf(dctx, row.ID)
		}
		if errText != "" && i.awaitLateAck(deliveryID, ack, lateAck{taskID: taskID, rowID: row.ID, entry: live}) {
			// 回显恰好落在超时与收尾之间：当场清空 no_replay_ack。
			if acked, err := writeRetryMsg(dctx, row.ID, lateAckFailMsg, func(c context.Context) (*store.Interjection, error) {
				return i.Store.MarkInterjectionReplayAcked(c, taskID, row.ID, deliveryID)
			}); err == nil && acked != nil {
				done = acked
			}
		}
		return SubmitResult{Outcome: OutcomeDelivered, Row: done}, nil

	case errors.Is(derr, agent.ErrDeliverBackpressure):
		failed, err := writeRetry(dctx, row.ID, func(c context.Context) (*store.Interjection, error) {
			return i.Store.MarkInterjectionFailed(c, taskID, row.ID, ReasonBackpressure)
		})
		if err != nil {
			return SubmitResult{}, err
		}
		if failed == nil {
			return i.resultOf(dctx, row.ID)
		}
		return SubmitResult{Outcome: OutcomeBackpressure, Row: failed, Message: ReasonBackpressure}, nil

	default:
		// ErrSessionClosed（会话恰好收尾）或其它「这帧没写进去」的错误：
		// 经状态闸门回退 —— 仍在 implementing/verifying 则退回 queued（下一轮补投
		// 或结算），否则直接 failed。
		if !errors.Is(derr, agent.ErrSessionClosed) {
			slog.Warn("插话投递失败，按会话收尾处理", "task", taskID, "id", row.ID, "err", derr)
		}
		reverted, err := writeRetry(dctx, row.ID, func(c context.Context) (*store.Interjection, error) {
			r, _, e := i.Store.RevertInterjectionGated(c, taskID, row.ID)
			return r, e
		})
		if err != nil {
			return SubmitResult{}, err
		}
		if reverted == nil {
			return i.resultOf(dctx, row.ID)
		}
		return i.resultFromRow(reverted), nil
	}
}

// resultOf 重读一行，按其实际状态给出结局。
func (i *Interjections) resultOf(dctx context.Context, id int64) (SubmitResult, error) {
	ctx, cancel := context.WithTimeout(dctx, interjectWriteTimeout)
	defer cancel()
	row, err := i.Store.GetInterjection(ctx, id)
	if err != nil {
		return SubmitResult{}, err
	}
	return i.resultFromRow(row), nil
}

func (i *Interjections) resultFromRow(row *store.Interjection) SubmitResult {
	msg := ""
	if row.Error != nil {
		msg = *row.Error
	}
	switch row.Status {
	case store.InterjectionDelivered:
		return SubmitResult{Outcome: OutcomeDelivered, Row: row}
	case store.InterjectionFailed:
		return SubmitResult{Outcome: OutcomeRejectedState, Row: row, Message: msg}
	default: // queued / delivering（被补投认领）：都是「已记录、等 agent 轮次」
		return SubmitResult{Outcome: OutcomeQueued, Row: row}
	}
}

// ListSince 增量列出某任务的插话（读时结算 T2 在同一事务里先做）。
func (i *Interjections) ListSince(ctx context.Context, taskID, afterRev int64, limit int) ([]store.Interjection, error) {
	return i.Store.ListInterjectionsSince(ctx, taskID, afterRev, limit)
}

// ---------------------------------------------------------------- 管线侧

// prefilled 是一次开场补投认领到的行。
type prefilled struct {
	taskID int64
	ids    []int64
}

// prefillInterjections 是开场补投：把该任务所有 queued 行认领为 delivering，
// 正文拼成**一段**追加到 prompt 尾部（插话段永远是最后一段）。无插话时返回
// 原 prompt 同一个字符串，不追加任何分隔符。认领失败只告警：事务回滚，行仍
// 是 queued，交给下一轮或结算。
func (i *Interjections) prefillInterjections(ctx context.Context, taskID int64, phase, sessionID, prompt string) (string, *prefilled) {
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), interjectWriteTimeout)
	defer cancel()
	rows, err := i.Store.ClaimPendingInterjections(c, taskID, phase, sessionID)
	if err != nil {
		slog.Warn("开场补投认领失败（行仍为 queued）", "task", taskID, "err", err)
		return prompt, nil
	}
	if len(rows) == 0 {
		return prompt, nil
	}
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\n\n---\n\n")
	b.WriteString(interjectHeading)
	b.WriteString("\n")
	b.WriteString(interjectGuard)
	b.WriteString("\n\n")
	pf := &prefilled{taskID: taskID}
	for n, r := range rows {
		fmt.Fprintf(&b, "%d. %s\n", n+1, r.Body)
		pf.ids = append(pf.ids, r.ID)
	}
	slog.Info("开场补投插话", "task", taskID, "phase", phase, "count", len(rows))
	return strings.TrimRight(b.String(), "\n"), pf
}

// finishPrefill 是补投行的结局：res != nil 且 NumTurns ≥ 1（至少一个 turn 读过
// 首帧）→ delivered；否则（没有 result，或 num_turns == 0：例如 --resume 找不到
// 会话时 CLI 也给 result）→ 退回 queued，交给同一任务的下一次 runAgent 或结算。
func (i *Interjections) finishPrefill(ctx context.Context, pf *prefilled, res *agent.Result) {
	if pf == nil {
		return
	}
	delivered := res != nil && res.NumTurns >= 1
	_, _ = writeRetry(context.WithoutCancel(ctx), pf.ids[0], func(c context.Context) (struct{}, error) {
		return struct{}{}, i.Store.FinishPrefilledInterjections(c, pf.taskID, pf.ids, delivered)
	})
}

// SettleTask 是结算 T1（Execute 收尾）的入口。
func (i *Interjections) SettleTask(ctx context.Context, taskID int64) {
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), interjectWriteTimeout)
	defer cancel()
	if n, err := i.Store.SettleQueuedInterjections(c, taskID); err != nil {
		slog.Warn("插话收尾结算失败（周期清扫会兜底）", "task", taskID, "err", err)
	} else if n > 0 {
		slog.Info("插话收尾结算：未进入模型的插话已判 failed", "task", taskID, "count", n)
	}
}

// SweepOnce 是周期结算（T3）的单次体：对「有 queued 行且任务已不会再跑 agent」
// 的任务逐个结算。判定在 SettleQueuedInterjections 里于锁下重做。
func (i *Interjections) SweepOnce(ctx context.Context) (int64, error) {
	ids, err := i.Store.InterjectionSettleCandidates(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, id := range ids {
		c, cancel := context.WithTimeout(ctx, interjectWriteTimeout)
		n, err := i.Store.SettleQueuedInterjections(c, id)
		cancel()
		if err != nil {
			slog.Warn("插话周期结算失败", "task", id, "err", err)
			continue
		}
		total += n
	}
	return total, nil
}

// StartupSweep 是启动清扫（T4，q.Reconcile 之后、worker 启动之前）：
// 遗留 delivering → failed（进程重启，持有者已不在）；再跑一次周期结算，
// 兜住「进程在 Execute 的 defer 之前死掉」。Reconcile 转回 queued 的任务，
// 其 queued 插话保留，由下一次 runAgent 补投。
func (i *Interjections) StartupSweep(ctx context.Context) error {
	n, err := i.Store.FailDeliveringInterjections(ctx)
	if err != nil {
		return err
	}
	m, err := i.SweepOnce(ctx)
	if err != nil {
		return err
	}
	if n > 0 || m > 0 {
		slog.Info("插话启动清扫", "delivering_failed", n, "queued_settled", m)
	}
	return nil
}
