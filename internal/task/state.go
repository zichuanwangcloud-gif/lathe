// Package task 实现 Lathe 的任务状态机。
//
// 本文件只含纯逻辑（状态集合与合法转移），不依赖数据库，
// 因此可被完整单测覆盖。持久化与事件落库见 machine.go。
//
// 设计依据：docs/02-design.md §3
package task

import (
	"fmt"
	"sort"
)

// State 是任务生命周期中的一个状态。
type State string

const (
	// StateQueued 已接单，等待派发。
	StateQueued State = "queued"
	// StateTriaging 判断单子明确度、定位涉及范围。
	StateTriaging State = "triaging"
	// StateBlockedSpec 单子不明确，已回帖提问，等人补充。
	StateBlockedSpec State = "blocked_spec"
	// StateAwaitingApproval 等人工放行（仅 plan-first / manual 档使用）。
	StateAwaitingApproval State = "awaiting_approval"
	// StateImplementing agent 正在写代码。
	StateImplementing State = "implementing"
	// StateVerifying 按档位跑验证。
	StateVerifying State = "verifying"
	// StatePROpen PR 已开，已回帖 Linear。
	StatePROpen State = "pr_open"
	// StateReviewFeedback 收到 review 意见，待二轮（必须 resume 原会话）。
	StateReviewFeedback State = "review_feedback"
	// StateMerged 终态：已合并。
	StateMerged State = "merged"
	// StateFailed 终态：失败，保留现场。
	StateFailed State = "failed"
	// StateCancelled 终态：人工中止。
	StateCancelled State = "cancelled"
)

// AllStates 按生命周期顺序列出全部状态。
func AllStates() []State {
	return []State{
		StateQueued, StateTriaging, StateBlockedSpec, StateAwaitingApproval,
		StateImplementing, StateVerifying, StatePROpen, StateReviewFeedback,
		StateMerged, StateFailed, StateCancelled,
	}
}

// transitions 是合法转移表：key 可转到 value 中的任一状态。
//
// 三条不显然但必要的边：
//
//  1. triaging/implementing/verifying → queued —— 节点崩溃后租约到期，任务被
//     重新派发（docs/02-design.md §6.4）；单机形态下即服务重启后的启动恢复。
//     没有这条边，进程故障就等于任务永久卡死。
//  2. failed → queued —— 失败任务人工排查后可重新入队（D4 不自动重试，
//     但允许人工重试）。
//  3. review_feedback → implementing —— 二轮实现，调用方必须带上
//     agent_session_id 走 --resume，见 RequiresSession。
var transitions = map[State][]State{
	StateQueued:   {StateTriaging, StateCancelled},
	StateTriaging: {StateQueued, StateImplementing, StateAwaitingApproval, StateBlockedSpec, StateFailed, StateCancelled},

	// 等人补充需求；补齐后重新入队
	StateBlockedSpec: {StateQueued, StateCancelled},
	// 等人放行
	StateAwaitingApproval: {StateImplementing, StateCancelled},

	StateImplementing: {StateVerifying, StateQueued, StateFailed, StateCancelled},
	StateVerifying:    {StatePROpen, StateBlockedSpec, StateQueued, StateFailed, StateCancelled},

	StatePROpen:         {StateReviewFeedback, StateMerged, StateFailed, StateCancelled},
	StateReviewFeedback: {StateImplementing, StateMerged, StateFailed, StateCancelled},

	// 终态
	StateMerged:    {},
	StateFailed:    {StateQueued},
	StateCancelled: {},
}

// terminal 标记终态。终态不再自动流转（failed 允许人工重新入队）。
var terminal = map[State]bool{
	StateMerged:    true,
	StateFailed:    true,
	StateCancelled: true,
}

// Valid 报告 s 是否是已知状态。
func (s State) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// Terminal 报告 s 是否为终态。
func (s State) Terminal() bool { return terminal[s] }

// Active 报告任务是否还"活着"。
//
// 与数据库中的部分唯一索引 tasks_one_active_per_issue 保持一致：
// 同一 issue 同时只能有一个活任务，但 issue 重开后可再建。
func (s State) Active() bool { return s.Valid() && !s.Terminal() }

// String 实现 fmt.Stringer。
func (s State) String() string { return string(s) }

// CanTransition 报告能否从 from 转到 to。
func CanTransition(from, to State) bool {
	allowed, ok := transitions[from]
	if !ok {
		return false
	}
	for _, s := range allowed {
		if s == to {
			return true
		}
	}
	return false
}

// NextStates 返回 from 的全部合法后继，按字母序（便于测试与展示稳定）。
func NextStates(from State) []State {
	allowed := transitions[from]
	out := make([]State, len(allowed))
	copy(out, allowed)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ErrIllegalTransition 描述一次被拒绝的状态转移。
type ErrIllegalTransition struct {
	From, To State
}

func (e ErrIllegalTransition) Error() string {
	if !e.From.Valid() {
		return fmt.Sprintf("task: 源状态 %q 未知", e.From)
	}
	if !e.To.Valid() {
		return fmt.Sprintf("task: 目标状态 %q 未知", e.To)
	}
	if e.From.Terminal() {
		return fmt.Sprintf("task: %s 是终态，不能转到 %s", e.From, e.To)
	}
	return fmt.Sprintf("task: 不允许从 %s 转到 %s（合法后继：%v）", e.From, e.To, NextStates(e.From))
}

// Validate 校验一次转移，非法则返回 ErrIllegalTransition。
func Validate(from, to State) error {
	if !CanTransition(from, to) {
		return ErrIllegalTransition{From: from, To: to}
	}
	return nil
}

// RequiresSession 报告进入 to 状态是否必须已持有 agent_session_id。
//
// 这是 docs/02-design.md §3 约束① 的代码化：review 二轮必须 resume
// 原会话，重开会话会丢掉第一轮的全部推理与代码定位上下文。
func RequiresSession(from, to State) bool {
	return from == StateReviewFeedback && to == StateImplementing
}
