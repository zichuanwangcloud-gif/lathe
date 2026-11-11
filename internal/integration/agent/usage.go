package agent

// usage.go 用量账本的调用方视角（docs/features/usage-stats.md）。
//
// 为什么放在 agent 包：它是 store、runner、prd、preview 四方都已经 import
// 的唯一叶子包，记账接口与记录结构放这里不新增任何依赖边，也就不会造环。
// 这里只有纯类型、一个薄包装（RecordRun）和一个纯映射函数（UsageFromCodex）；
// 归属（user_id/repo_id）、阶段桶、状态判定、rune 截断全在 store 一侧。

import (
	"context"
	"log/slog"
	"time"
)

// ModelUsage 是一次运行里单个模型的用量（claude result 事件的 modelUsage 一项）。
type ModelUsage struct {
	Model               string  `json:"model"`
	InputTokens         int64   `json:"inputTokens"`
	OutputTokens        int64   `json:"outputTokens"`
	CacheReadTokens     int64   `json:"cacheReadTokens"`
	CacheCreationTokens int64   `json:"cacheCreationTokens"`
	CostUSD             float64 `json:"costUsd"`
}

// Usage 是一次 agent 运行的 token 用量。
//
// 多条 result（f01 插话开出第二个 turn）时随**最后一条** result 整组覆盖、
// 不跨 result 相加：claude 的 modelUsage 是进程累计值，末条即全量。末条没有
// modelUsage 时回落末条的 usage（单 turn 量，少记方向安全）；末条两个键都没有
// 时 Present=false。取值表与 f01 PRD §3.2 第 5 条逐字相同。
type Usage struct {
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	// Models 是逐模型明细（只有 modelUsage 才有；回落 usage 时为 nil）。
	Models []ModelUsage
	// Present 表示 CLI 报了用量：没报 ≠ 报了 0，账本据此写 tokens_known。
	Present bool
}

// RunRecord 是一次 agent 运行的账本行（调用方视角：只有目标 id，不含属主）。
type RunRecord struct {
	Kind        string // "task" | "prd" | "preview"
	TaskID      int64  // Kind=task/preview
	PRDID       int64  // Kind=prd
	Phase       string
	ReviewRound bool
	// VerifyTier 是任务运行时本次 Execute 内存里的档位（rc.tier）；空 = 尚未定档 → NULL。
	// preview 由 store 从任务行取，prd 恒 NULL —— 这两种不填。
	VerifyTier string
	SessionID  string
	Resume     bool
	StartedAt  time.Time
	EndedAt    time.Time
	Result     *Result // 可为 nil（没拿到 result 事件）
	Err        error   // Run 的返回错误；只用于判 status，不落库
}

// UsageRecorder 落一次 agent 运行的用量账本。store.Store 实现它。
type UsageRecorder interface {
	RecordAgentRun(ctx context.Context, r RunRecord) error
}

// TierBackfiller 是 runner 额外需要的定档回补：本任务里此前没有档位的行
// 补成首次定下的档。单独成接口是为了 prd / preview 的假实现不必写用不到的方法。
type TierBackfiller interface {
	SetAgentRunTier(ctx context.Context, taskID int64, tier string) error
}

// recordTimeout 是单次记账的写库上限。
const recordTimeout = 3 * time.Second

// RecordRun 是各调用点共用的记账薄包装：rec 为 nil 时什么都不做；
// 用 WithoutCancel(ctx) + 3s 超时写（任务被取消时账本不跟着丢）；
// 落库失败只告警 —— 可观测性不能反过来成为任务的故障源。
func RecordRun(ctx context.Context, rec UsageRecorder, r RunRecord) {
	if rec == nil {
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := rec.RecordAgentRun(wctx, r); err != nil {
		slog.Warn("agent 用量落库失败", "kind", r.Kind, "task", r.TaskID, "prd", r.PRDID,
			"phase", r.Phase, "err", err)
	}
}

// CodexUsage 是 codex turn.completed 事件里的 usage 对象（OpenAI 口径）。
type CodexUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
}

// UsageFromCodex 把一次 Run 里各条 turn.completed 的 usage 映射成 agent.Usage。
//
//   - OpenAI 的 input_tokens **含**缓存部分，claude 的不含：减掉两项缓存（下限 0）
//     后两边同义，input+cacheRead+cacheCreation 都等于「总输入」；
//   - reasoning_output_tokens 已含在 output_tokens 里，不另加；
//   - 每条是该 turn 的量，多条逐条相加；
//   - codex 不报模型名：Models 为 nil；
//   - 一条都没有（只有 turn.failed 之类）→ Present=false（未知，不是 0）。
//
// 由 codex 驱动在构造 Result 时调用（f03 / f06 后合入的一方接上那一行），
// CostKnown 保持零值 false —— codex 不报费用。
func UsageFromCodex(turns []CodexUsage) Usage {
	var u Usage
	for _, t := range turns {
		u.Present = true
		in := t.InputTokens - t.CachedInputTokens - t.CacheWriteInputTokens
		if in < 0 {
			in = 0
		}
		u.InputTokens += in
		u.OutputTokens += t.OutputTokens
		u.CacheReadTokens += t.CachedInputTokens
		u.CacheCreationTokens += t.CacheWriteInputTokens
	}
	return u
}
