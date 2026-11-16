// Package notify 把任务/PRD 的「等人出手」事件推到站外渠道（Slack incoming
// webhook、飞书自定义机器人）。
//
// 本包是**叶子包**：只 import 标准库（TestNotifyImportsStdlibOnly 守着）。
// 依赖方向写死为 runner/prd/store/httpapi → notify：
//
//   - runner / prd 只拿值类型 TaskEvent / PRDEvent 与 Event* 常量；
//   - store 实现 Outbox，并用本包的纯函数拼去重键与 payload；
//   - Sender 只在 cmd/lathe（worker）与 httpapi（测试消息）被构造与调用。
//
// 核心不变量：**通知是状态转移之后的副作用，永不参与判定**。入队失败、
// 发送失败、渠道方限流，一律只落在 notify_deliveries 这张旁路表与日志里，
// 任务状态机看不到它们。
package notify

import (
	"context"
	"fmt"
	"time"
)

// 渠道种类。与迁移 0056 的 notify_channels_kind_check 一致。
const (
	KindSlack  = "slack"
	KindFeishu = "feishu"
)

// Kinds 是可配置的渠道种类（界面顺序）。
func Kinds() []string { return []string{KindSlack, KindFeishu} }

// ValidKind 报告 kind 是否是已知渠道。
func ValidKind(kind string) bool { return kind == KindSlack || kind == KindFeishu }

// 事件种类。与迁移 0057 的 notify_deliveries_event_check 一致
// （TestEventConstantsMatchStoreCheck 读迁移文本做集合比较）。
const (
	EventFailed           = "failed"
	EventBlockedSpec      = "blocked_spec"
	EventAwaitingApproval = "awaiting_approval"
	EventPROpen           = "pr_open"
	EventPRDReady         = "prd_ready_for_review"
	EventTest             = "test"
)

// EventTypes 返回全部事件种类（五类业务事件 + test）。
func EventTypes() []string {
	return []string{EventFailed, EventBlockedSpec, EventAwaitingApproval, EventPROpen, EventPRDReady, EventTest}
}

// 投递状态。与 notify_deliveries_status_check 一致。
const (
	StatusPending = "pending"
	StatusSending = "sending"
	StatusSent    = "sent"
	StatusSkipped = "skipped"
	StatusGaveUp  = "gave_up"
)

// 行为常量（不是配置项：它们不需要按部署调，做成配置只会多一个没人设的旋钮）。
// 导出是因为 store / 外部测试包要用它们算期望值。
const (
	// MaxAttempts 是一条投递最多发送的次数（含第一次）：第 5 次失败即 gave_up。
	MaxAttempts = 5
	// BaseBackoff 是第一次失败后的等待；之后每次翻倍。
	BaseBackoff = 30 * time.Second
	// MaxBackoff 是退避上界。MaxAttempts=5 时最大退避 240s，不会触发；
	// 保留它是为了常量被改大时公式仍有上界。
	MaxBackoff = 15 * time.Minute
	// DrainInterval 是后台 worker 的轮询周期。
	DrainInterval = 15 * time.Second
	// SendTimeout 是一次 HTTP 发送（连接 + 读响应）的上界。
	SendTimeout = 30 * time.Second
	// ClaimLease 是领取租约：sending 超过它视为死掉的尝试，回收重排。
	// 逐行领取、领到即发，所以它只需覆盖「一次发送」的上界（≥ 2*SendTimeout+1m）。
	ClaimLease = 5 * time.Minute
	// DrainMaxPerRound 是一轮 DrainOnce 最多处理的行数。
	DrainMaxPerRound = 10
)

// TaskEvent 是任务事件的**转移时刻快照**。
//
// 调用方（runner.chatTerminal）在起 goroutine 之前填好，之后只传值；
// 实现方不得回读 tasks 表 —— 异步执行时任务可能已被放行、updated_at 已前进。
type TaskEvent struct {
	TaskID, UserID int64
	ExternalKey    string
	ToState        string    // tk.State
	EventType      string    // Event* 常量
	At             time.Time // tk.UpdatedAt（Transition 的 RETURNING 带回）
	FailureStage   string    // 仅 failed / blocked_spec
	PRURL          string    // 仅 pr_open
	Reason         string    // runner 侧已脱敏 + 字节截断
}

// PRDEvent 是 PRD 进入待签字的快照。
type PRDEvent struct {
	PRDID, UserID int64
	ToState       string    // "ready_for_review"
	EventType     string    // EventPRDReady
	At            time.Time // TransitionPRD 返回行的 UpdatedAt
	Repo          string    // RepoForPRD 的 ProviderRepo
}

// EventTime 把时间截到微秒并转 UTC：与 pgx 把 time.Time 编进 timestamptz
// 的口径一致（截断而非四舍五入）。去重键与 next_attempt_at 列都经过它，
// 二者对任何输入都不分叉。生产里 At 来自 RETURNING，本来就是微秒，这是恒等。
func EventTime(at time.Time) time.Time { return at.UTC().Truncate(time.Microsecond) }

// TaskEventKey 是任务事件的去重键 —— 全文唯一的拼法。
//
// 用「转移那一刻的 updated_at」而不是当前时间：同一转移重放得到同一个键，
// 唯一索引 (channel_id, event_key) 才挡得住重复投递。
func TaskEventKey(ev TaskEvent) string {
	return fmt.Sprintf("task:%d:%s:%s", ev.TaskID, ev.ToState, EventTime(ev.At).Format(time.RFC3339Nano))
}

// PRDEventKey 是 PRD 事件的去重键。
func PRDEventKey(ev PRDEvent) string {
	return fmt.Sprintf("prd:%d:%s:%s", ev.PRDID, ev.ToState, EventTime(ev.At).Format(time.RFC3339Nano))
}

// TestEventKey 是测试消息的去重键：每次一个新 nonce，每次测试都留痕。
func TestEventKey(nonce string) string { return "test:" + nonce }

// Payload 是 notify_deliveries.payload 的全部字段 —— worker 渲染的**唯一**输入。
//
// 刻意没有 worktree 路径、分支名、会话 ID、凭据：正文的全部来源就是这里，
// 结构体里没有的东西就不可能被外发（TestRenderNeverLeaksInternalPaths 守着键集合）。
type Payload struct {
	TaskID       int64  `json:"task_id,omitempty"`
	ExternalKey  string `json:"external_key,omitempty"`
	ToState      string `json:"to_state,omitempty"`
	FailureStage string `json:"failure_stage,omitempty"`
	Reason       string `json:"reason,omitempty"`
	PRURL        string `json:"pr_url,omitempty"`
	PRDID        int64  `json:"prd_id,omitempty"`
	PRDRepo      string `json:"prd_repo,omitempty"`
	TestNonce    string `json:"test_nonce,omitempty"`
}

// TaskPayload 从任务快照生成 payload。只带该事件有意义的字段。
func TaskPayload(ev TaskEvent) Payload {
	p := Payload{TaskID: ev.TaskID, ExternalKey: ev.ExternalKey, ToState: ev.ToState}
	switch ev.EventType {
	case EventFailed, EventBlockedSpec:
		p.FailureStage = ev.FailureStage
		p.Reason = ev.Reason
	case EventPROpen:
		p.PRURL = ev.PRURL
	}
	return p
}

// PRDPayload 从 PRD 快照生成 payload。
func PRDPayload(ev PRDEvent) Payload {
	return Payload{PRDID: ev.PRDID, PRDRepo: ev.Repo}
}

// FactsFromPayload 把 payload 映射成渲染输入。BaseURL 是部署配置，不在 payload 里。
func FactsFromPayload(eventType string, p Payload, baseURL string) Facts {
	_ = eventType // 预留：目前所有事件的映射相同，按事件裁剪由 Render 负责
	return Facts{
		TaskID: p.TaskID, ExternalKey: p.ExternalKey, State: p.ToState,
		FailureReason: p.Reason, FailureStage: p.FailureStage, PRURL: p.PRURL,
		PRDID: p.PRDID, PRDRepo: p.PRDRepo, TestNonce: p.TestNonce,
		BaseURL: baseURL,
	}
}

// NextAttempt 是退避的唯一公式（MarkRetry 与 ReclaimStale 共用）。
//
// n 是本次失败 +1 之后的 attempts。next = now + min(BaseBackoff*2^(n-1), MaxBackoff)；
// n >= MaxAttempts 时 giveUp=true（此时 next 无意义，但仍按公式给出，便于测试封顶）。
func NextAttempt(n int, now time.Time) (next time.Time, giveUp bool) {
	if n < 1 {
		n = 1
	}
	gap := MaxBackoff
	// 30s << 20 已远超 MaxBackoff；先限位移量，免得大 n 溢出成负数。
	if n-1 < 20 {
		if d := BaseBackoff << uint(n-1); d < MaxBackoff {
			gap = d
		}
	}
	return now.Add(gap), n >= MaxAttempts
}

// Delivery 是 outbox 的一行（store 填、worker 读）。
type Delivery struct {
	ID, ChannelID       int64
	EventType, EventKey string
	Payload             Payload
	Attempts            int
	ClaimedAt           time.Time // 领取时 RETURNING 的值，CAS 写回用
}

// EventSwitches 是渠道上的五个事件开关。
type EventSwitches struct {
	Failed           bool `json:"failed"`
	BlockedSpec      bool `json:"blockedSpec"`
	AwaitingApproval bool `json:"awaitingApproval"`
	PROpen           bool `json:"prOpen"`
	PRDReady         bool `json:"prdReady"`
}

// Allows 报告该事件是否放行。test 恒放行（人主动点的），未知事件一律不放。
func (e EventSwitches) Allows(eventType string) bool {
	switch eventType {
	case EventFailed:
		return e.Failed
	case EventBlockedSpec:
		return e.BlockedSpec
	case EventAwaitingApproval:
		return e.AwaitingApproval
	case EventPROpen:
		return e.PROpen
	case EventPRDReady:
		return e.PRDReady
	case EventTest:
		return true
	}
	return false
}

// Channel 是一条已解密的渠道。URL 与 SignSecret 只在内存里：
// JSON 与 String() 都不带它们，误打日志也不会漏。
type Channel struct {
	ID, UserID int64
	Kind       string
	URL        string `json:"-"` // 已解密；webhook URL 本身就是凭据
	SignSecret string `json:"-"` // 已解密；Slack 为空
	Enabled    bool
	Events     EventSwitches
}

// String 只打种类与 id，绝不打 URL / 密钥。
func (c Channel) String() string { return fmt.Sprintf("notify.Channel{id=%d kind=%s}", c.ID, c.Kind) }

// GoString 同 String（%#v 也不漏）。
func (c Channel) GoString() string { return c.String() }

// Outbox 是 worker 需要的全部库操作；*store.NotifyChannels 实现它。
//
// 所有时间都由调用方传入，SQL 里不出现 now()：测试可确定性复现。
// bool 返回值 = CAS 是否命中（false 表示该行已被回收/改写，调用方只 Warn）。
type Outbox interface {
	ReclaimStale(ctx context.Context, now time.Time, lease time.Duration) (int, error)
	ClaimDueDelivery(ctx context.Context, now time.Time) (*Delivery, error) // 无到期行返回 (nil, nil)
	DeliveryChannel(ctx context.Context, channelID int64) (*Channel, error) // 解密后的本行渠道；已删返回 (nil, nil)
	MarkSent(ctx context.Context, id int64, claimedAt, now time.Time) (bool, error)
	MarkRetry(ctx context.Context, id int64, claimedAt, now time.Time, reason string) (bool, error)
	MarkGaveUp(ctx context.Context, id int64, claimedAt, now time.Time, reason string) (bool, error)
	MarkSkipped(ctx context.Context, id int64, claimedAt, now time.Time, reason string) (bool, error)
}
