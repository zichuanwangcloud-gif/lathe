package schedule

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/linear"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// 调度器的常量。刻意不做成配置：不同部署之间没有调整它们的理由，
// 做成配置只会多出没人用的配置项（铁律 3）。测试经 Runner 的未导出字段缩短时间。
const (
	// DefaultTick 是认领循环的节拍。
	DefaultTick = 30 * time.Second
	// ClaimLimit 是阶段 A 每 tick 最多认领的规则数（以及每 tick 最多回收的 pending 数）。
	// 阶段 A 只碰库，200 条约百毫秒级；超出的顺延一个 tick。
	ClaimLimit = 200
	// ExecConcurrency 是阶段 B（Linear 网络调用）的并发上限。
	ExecConcurrency = 4

	// LeaseTTL / LeaseHeartbeat / FenceMargin 是执行租约的三个时间：
	// LeaseTTL ≥ 6·LeaseHeartbeat（容忍连续 5 次心跳失败前不围栏）；
	// FenceMargin > linear 客户端超时（围栏后最后一个已发出的请求在租约真正过期前已返回）；
	// LeaseTTL − FenceMargin > LeaseHeartbeat。init 里断言。
	LeaseTTL       = 2 * time.Minute
	LeaseHeartbeat = 20 * time.Second
	FenceMargin    = 45 * time.Second

	// MaxAttempts 是一条 run 最多执行的次数（含首次）；回收时超过就不再重放。
	MaxAttempts = 3
	// MaxConsecutiveFailures 次连续建单失败后自动暂停规则。
	MaxConsecutiveFailures = 5
	// MaxSchedulesPerUser 是每人规则数上限（API 层校验）。
	MaxSchedulesPerUser = 20

	missedCountCap      = 1000
	linearClientTimeout = 30 * time.Second
	autoPausePrefix     = "连续 5 次建单失败，已自动暂停："
)

// ReplayStalePending 是 R1 取证结论的唯一承载点（docs/features/scheduled-issue.md §6 R1）：
// 回收来的、结果未知且按 UUID 查不到的 run，要不要用同一个 UUID 重放建单。
//
// 默认 false（至多一次）：Linear 对客户端指定 id 的重放行为没有对真 API 取证之前，
// 不押宝重放 —— 记 failed/interrupted 并提示「结果未知，请到 Linear 确认」。
// 只有 evidence 里两份取证文件都在、证明同 id 重放不会建第二张单时才改成 true。
// 不做成环境变量：它描述外部 API 的行为，不是部署偏好。
const ReplayStalePending = false

func init() {
	if err := checkLeaseConstants(LeaseTTL, LeaseHeartbeat, FenceMargin, linearClientTimeout); err != nil {
		panic(err)
	}
}

// checkLeaseConstants 校验租约三常量的约束关系。
func checkLeaseConstants(ttl, hb, fence, clientTimeout time.Duration) error {
	switch {
	case ttl < 6*hb:
		return fmt.Errorf("schedule: LeaseTTL=%v 应 ≥ 6·LeaseHeartbeat=%v", ttl, 6*hb)
	case fence <= clientTimeout:
		return fmt.Errorf("schedule: FenceMargin=%v 应 > linear 客户端超时 %v", fence, clientTimeout)
	case ttl-fence <= hb:
		return fmt.Errorf("schedule: LeaseTTL−FenceMargin=%v 应 > LeaseHeartbeat=%v", ttl-fence, hb)
	}
	return nil
}

// IssueCreator 是调度器对 Linear 的窄需求，*linear.Client 满足它。
type IssueCreator interface {
	CreateIssue(ctx context.Context, in linear.CreateIssueInput) (*linear.CreatedIssue, error)
	// 查到 → (issue, nil)；确认不存在 → (nil, nil)；其余一切 → (nil, err)。
	IssueByID(ctx context.Context, id string) (*linear.CreatedIssue, error)
	LabelIDByName(ctx context.Context, teamID, name string) (string, error)
}

// LinearHandle 是一次建单所需的全部 Linear 侧能力。
type LinearHandle struct {
	Client IssueCreator
	// UserID 是「指派给我」用的 Linear 用户 ID，可能为空。
	UserID string
	// Redact 把本次用到的凭据值精确替换成 [REDACTED]。由装配方用它手里的
	// token 构造 —— 调度器本身永远拿不到 token 明文，只拿到一个会抹掉它的函数。
	Redact func(string) string
}

// LinearFor 按属主取 Linear 能力（装配方用属主自己的凭据构造）。
type LinearFor func(ctx context.Context, userID int64) (LinearHandle, error)

// Settings 是系统设置里调度器要现取的值。
type Settings interface {
	WebhookTriggerLabel(ctx context.Context) string
}

// Store 是调度器对存储层的需求，*store.Store 满足它。
type Store interface {
	ClaimDueSchedule(ctx context.Context, now time.Time,
		decide func(sc *store.ScheduleRow, ownerDisabled bool) store.ClaimDecision) (*store.ClaimedSchedule, error)
	RecoverExpiredRuns(ctx context.Context, token, node string, ttl time.Duration, limit int) ([]store.ScheduleRunRow, error)
	RenewLeases(ctx context.Context, tokens []string, ttl time.Duration) (int64, error)
	MarkCreateSent(ctx context.Context, runID int64, token string) (bool, error)
	LoadRunContext(ctx context.Context, runID int64) (*store.RunContext, error)
	FinishRun(ctx context.Context, p store.FinishRunParams) (bool, error)
	InsertManualRun(ctx context.Context, scheduleID, userID int64, slot time.Time,
		uuid, node, token string, ttl time.Duration) (*store.ScheduleRunRow, error)
	GetScheduleRun(ctx context.Context, runID int64) (*store.ScheduleRunRow, error)
}

type leaseRenewer interface {
	RenewLeases(ctx context.Context, tokens []string, ttl time.Duration) (int64, error)
}

// RunRef 标识本轮认领 / 回收到的一条 run。RunID 为 0 表示认领了规则但 INSERT 冲突
// （该时刻已被手动运行或别的节点占了），本轮没有新 run。
type RunRef struct {
	RunID      int64
	ScheduleID int64
}

// TickResult 是一次 TickOnce 在本节点认领 / 回收到的东西，供调用方按归属过滤。
type TickResult struct {
	Claimed   []RunRef
	Recovered []RunRef
}

// Runner 是定时建单调度器。
//
// 结构：TickOnce 只做数据库工作（阶段 A 认领 + 回收过期租约），把 pending 交给执行器
// 就返回；阶段 B（Linear 网络调用）在执行器的工作 goroutine 里有界并发地跑。所以
// 认领循环永远按节拍到来，别的规则的 Linear 再慢也不会让这条规则被判成迟到。
//
// 生命周期：执行器与节点级心跳在第一次 TickOnce / FireNow 时惰性启动，挂在 Runner
// 自己的生命期 ctx 上（不挂调用方的 ctx），只由 Close 结束。构造本身无副作用。
type Runner struct {
	Store    Store
	Linear   LinearFor
	Settings Settings
	Now      func() time.Time
	// Tick 为 0 时取 DefaultTick；准点宽限 OnTimeGrace = 2·Tick。
	Tick time.Duration
	// CatchUpWindow 只管「认领时迟到超过 OnTimeGrace 的那次补不补」（LATHE_SCHEDULE_CATCHUP_WINDOW）。
	CatchUpWindow time.Duration
	// Node 是本节点名（LATHE_NODE_NAME），记进 run 供排障。
	Node string

	// 以下只给测试缩短时间 / 注入用，零值取常量；不对应任何环境变量。
	leaseTTL, heartbeat, fenceMargin time.Duration
	concurrency                      int
	replay                           *bool
	renewer                          leaseRenewer
	hold                             chan struct{}
	log                              *slog.Logger

	startOnce sync.Once
	closeOnce sync.Once
	life      context.Context
	lifeStop  context.CancelFunc
	wg        sync.WaitGroup
	wake      chan struct{}

	mu       sync.Mutex
	started  bool
	gen      *generation
	queue    []job
	held     map[string]int
	inflight int
	okAt     time.Time
}

// generation 是执行器的「一代」：围栏时取消当前一代，之后的 tick 用新一代。
type generation struct {
	ctx    context.Context
	cancel context.CancelFunc
}

type job struct {
	runID      int64
	scheduleID int64
	token      string
	recovered  bool
	gen        *generation
}

func (r *Runner) tick() time.Duration {
	if r.Tick > 0 {
		return r.Tick
	}
	return DefaultTick
}

// OnTimeGrace 是准点宽限：认领时 lag ≤ 2·Tick 一律算准点，不受补偿窗口约束。
func (r *Runner) OnTimeGrace() time.Duration { return 2 * r.tick() }

func (r *Runner) ttl() time.Duration     { return orDefault(r.leaseTTL, LeaseTTL) }
func (r *Runner) hbEvery() time.Duration { return orDefault(r.heartbeat, LeaseHeartbeat) }
func (r *Runner) fence() time.Duration   { return orDefault(r.fenceMargin, FenceMargin) }
func (r *Runner) logger() *slog.Logger {
	if r.log != nil {
		return r.log
	}
	return slog.Default()
}
func (r *Runner) replayStale() bool {
	if r.replay != nil {
		return *r.replay
	}
	return ReplayStalePending
}

func orDefault(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// ---------------------------------------------------------------- 阶段 A

// TickOnce 跑一轮：认领到期规则、回收过期租约，把 pending 交给执行器后返回。
// 不等任何 Linear 请求。
func (r *Runner) TickOnce(ctx context.Context) (TickResult, error) {
	r.ensureStarted()
	now := r.Now()
	token := newUUID()
	r.mu.Lock()
	gen := r.gen
	r.mu.Unlock()

	var res TickResult
	var jobs []job
	var firstErr error
	for i := 0; i < ClaimLimit; i++ {
		c, err := r.Store.ClaimDueSchedule(ctx, now, func(sc *store.ScheduleRow, ownerDisabled bool) store.ClaimDecision {
			return r.decide(sc, ownerDisabled, now, token)
		})
		if err != nil {
			firstErr = err
			break
		}
		if c == nil {
			break
		}
		ref := RunRef{ScheduleID: c.Schedule.ID}
		if c.Run != nil {
			ref.RunID = c.Run.ID
			if c.Run.Status == store.RunPending {
				jobs = append(jobs, job{runID: c.Run.ID, scheduleID: c.Schedule.ID, token: token, gen: gen})
			}
			r.logger().Info("定时规则已认领", "schedule", c.Schedule.ID, "run", c.Run.ID,
				"slot", c.Run.ScheduledFor, "status", c.Run.Status, "trigger", c.Run.Trigger, "node", r.Node)
		}
		res.Claimed = append(res.Claimed, ref)
	}

	if firstErr == nil {
		rec, err := r.Store.RecoverExpiredRuns(ctx, token, r.Node, r.ttl(), ClaimLimit)
		if err != nil {
			firstErr = err
		}
		for _, run := range rec {
			res.Recovered = append(res.Recovered, RunRef{RunID: run.ID, ScheduleID: run.ScheduleID})
			jobs = append(jobs, job{runID: run.ID, scheduleID: run.ScheduleID, token: token, recovered: true, gen: gen})
			r.logger().Warn("回收租约已过期的定时运行", "schedule", run.ScheduleID, "run", run.ID,
				"attempts", run.Attempts, "node", r.Node)
		}
	}

	if len(jobs) > 0 {
		r.register(token, len(jobs))
		r.enqueue(jobs)
	}
	return res, firstErr
}

// decide 是阶段 A 对一条到期规则的判定（纯 Go，在认领事务内被调用）。
func (r *Runner) decide(sc *store.ScheduleRow, ownerDisabled bool, now time.Time, token string) store.ClaimDecision {
	spec, err := SpecOf(sc)
	if err != nil {
		r.logger().Error("定时规则无法解析，已暂停", "schedule", sc.ID, "err", err)
		return store.ClaimDecision{PausedReason: "内部错误：规则无法解析（" + err.Error() + "）"}
	}
	var d store.ClaimDecision
	if latest, ok := spec.Prev(now); ok {
		baseline := sc.EnabledAt
		if sc.LastSlotAt != nil && sc.LastSlotAt.After(baseline) {
			baseline = *sc.LastSlotAt
		}
		if latest.After(baseline) {
			missed := spec.CountBetween(baseline, latest, missedCountCap)
			lag := now.Sub(latest)
			trigger := store.TriggerSchedule
			if missed > 0 {
				trigger = store.TriggerCatchup
			}
			in := &store.NewRun{ScheduledFor: latest, Trigger: trigger, MissedCount: missed, UUID: newUUID(), Node: r.Node}
			// 判定顺序：命中第一条就停
			switch {
			case ownerDisabled:
				in.Status, in.Reason = store.RunSkipped, store.ReasonOwnerDisabled
			case lag <= r.OnTimeGrace():
				in.Status = store.RunPending
			case lag <= r.CatchUpWindow:
				in.Status, in.Trigger = store.RunPending, store.TriggerCatchup
			default:
				in.Status, in.Reason = store.RunSkipped, store.ReasonMissedWindow
			}
			if in.Status == store.RunPending {
				in.LeaseToken, in.LeaseTTL = token, r.ttl()
			}
			d.Insert = in
			d.LastSlotAt = &latest
		}
	}
	// 前移防御：Next(now) 必须严格晚于 now，否则（只可能是实现 bug）停下让人看见，
	// 不让这条规则在同一 tick 里被反复认领空转。
	next := spec.Next(now)
	if !next.After(now) {
		r.logger().Error("定时规则下次触发时刻未前进，已暂停", "schedule", sc.ID, "now", now, "next", next)
		d.PausedReason = fmt.Sprintf("内部错误：下次触发时刻未前进（%s，now=%s）", spec.Describe(), now.Format(time.RFC3339))
		return d
	}
	d.NextFireAt = &next
	return d
}

// SpecOf 把规则行还原成 Spec（时区加载 + 形状校验）。
func SpecOf(sc *store.ScheduleRow) (Spec, error) {
	loc, err := LoadLocation(sc.Timezone)
	if err != nil {
		return Spec{}, err
	}
	s := Spec{Kind: sc.Kind, Loc: loc}
	if sc.MinuteOfDay != nil {
		s.MinuteOfDay = *sc.MinuteOfDay
	}
	if sc.Weekdays != nil {
		s.Weekdays = *sc.Weekdays
	}
	if sc.IntervalMinutes != nil {
		s.IntervalMinutes = *sc.IntervalMinutes
	}
	if sc.AnchorAt != nil {
		s.Anchor = *sc.AnchorAt
	}
	return s, s.Validate()
}

// ---------------------------------------------------------------- 立即运行

// FireNow 是「立即运行」：scheduled_for = now 截到分钟、trigger=manual，在调用方
// goroutine 里同步执行阶段 B（与执行器同一个执行函数），返回终态后的 run。
//
// 同一分钟已有记录 → store.ErrRunSlotTaken；规则不属于 userID → store.ErrScheduleNotFound。
// 它占掉的时刻会让随后的 tick 在 INSERT 处冲突，但规则照常前移。
func (r *Runner) FireNow(ctx context.Context, scheduleID, userID int64) (*store.ScheduleRunRow, error) {
	r.ensureStarted()
	slot := r.Now().Truncate(time.Minute)
	token := newUUID()
	run, err := r.Store.InsertManualRun(ctx, scheduleID, userID, slot, newUUID(), r.Node, token, r.ttl())
	if err != nil {
		return nil, err
	}
	r.register(token, 1)
	r.mu.Lock()
	r.inflight++
	gen := r.gen
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.inflight--
		r.mu.Unlock()
		r.release(token)
	}()

	// 执行 ctx 同时受请求 ctx 与执行器当前一代 ctx 约束：围栏同样能中止它
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(gen.ctx, cancel)
	defer stop()
	r.execute(jctx, job{runID: run.ID, scheduleID: scheduleID, token: token, gen: gen})

	return r.Store.GetScheduleRun(context.WithoutCancel(ctx), run.ID)
}

// ---------------------------------------------------------------- Run / Drain / Close

// Run 按节拍循环 TickOnce，ctx 取消后先停循环、再 Close 才返回。
func (r *Runner) Run(ctx context.Context) {
	defer r.Close()
	t := time.NewTicker(r.tick())
	defer t.Stop()
	for {
		if _, err := r.TickOnce(ctx); err != nil && ctx.Err() == nil {
			r.logger().Warn("定时建单 tick 失败（下一轮重试）", "err", err, "node", r.Node)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Drain 等执行器把已交付的 run 全部写完终态（只给测试用）。未启动时立即返回。
func (r *Runner) Drain(ctx context.Context) error {
	for {
		r.mu.Lock()
		started, n := r.started, r.inflight
		r.mu.Unlock()
		if !started || n == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Close 结束执行器与心跳：取消进行中的请求，排队中的 run 不再开始（它们的租约
// 不再续约，至多 LeaseTTL 后被任一节点回收）。幂等；未启动过时直接返回。
func (r *Runner) Close() {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		started := r.started
		r.mu.Unlock()
		if !started {
			return
		}
		r.lifeStop()
		r.wg.Wait()
		r.mu.Lock()
		r.inflight -= len(r.queue)
		r.queue = nil
		r.mu.Unlock()
	})
}

// ---------------------------------------------------------------- 脱敏

// linKeyRe 是 Linear API key 的形态（与 internal/runner/redact.go 同一条正则；
// 不 import runner，复制一行）。
var linKeyRe = regexp.MustCompile(`\blin_api_[A-Za-z0-9]{16,}\b`)

// redactErr 两层脱敏，顺序即优先级：先精确替换本次用到的 token（不看形态），
// 再按 lin_api_ 形态兜底；最后按 rune 截到 1000（AGENTS.md §9）。
func redactErr(exact func(string) string, s string) string {
	if exact != nil {
		s = exact(s)
	}
	s = linKeyRe.ReplaceAllString(s, "lin_api_[REDACTED]")
	return truncateRunes(s, maxStoredErrRunes)
}
