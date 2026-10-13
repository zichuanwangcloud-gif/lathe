// Package prd 实现模糊任务的规划管线：多轮对话式智能体读代码产出 PRD，
// 经对抗复核与人工逐节确认后，一键生成正式任务与编排图。
//
// 本文件只含纯逻辑（状态集合与合法转移），不依赖数据库，
// 因此可被完整单测覆盖。持久化见 store.go，对话编排见 service.go。
//
// 设计依据：docs/10-prd-template.md §1 生命周期
//
// 为什么不复用 task 包的状态机：两套不变量互不相干。task 的核心约束是
// 「任何路径到 pr_open 必经 verifying」；PRD 的核心约束是「进
// ready_for_review 前无 ❓ 且对抗复核已跑」。共享类型只会让两张转移表
// 互相牵制，而它们没有一条边是共用的。
package prd

import (
	"fmt"
	"sort"
)

// State 是一份 PRD 生命周期中的一个状态。
type State string

const (
	// StateDrafting 智能体在填写/修订。
	StateDrafting State = "drafting"
	// StateAwaitingAnswers 本轮问题已提出，等人回答。
	StateAwaitingAnswers State = "awaiting_answers"
	// StateReadyForReview 自检清单全过、对抗复核完成，等人批准。
	StateReadyForReview State = "ready_for_review"
	// StateApproved 人已签字，PRD 冻结（D10-4：approved 后不可变）。
	StateApproved State = "approved"
	// StateConverted 终态：已生成正式任务与编排图。
	StateConverted State = "converted"
	// StateAbandoned 终态：人放弃，或超时无人回答被系统搁置（docs/10 §9）。
	// 两者共用一个状态，可由人唤醒回 drafting。
	StateAbandoned State = "abandoned"
)

// AllStates 按生命周期顺序列出全部状态。
func AllStates() []State {
	return []State{
		StateDrafting, StateAwaitingAnswers, StateReadyForReview,
		StateApproved, StateConverted, StateAbandoned,
	}
}

// transitions 是合法转移表：key 可转到 value 中的任一状态。
//
// 四条需要解释的边：
//
//  1. drafting ⇄ awaiting_answers —— 多轮对话的主循环（D10-7）。智能体提完
//     问题挂起等人，人答完回到起草。这是 PRD 绝大多数时间所处的地方。
//
//  2. ready_for_review → drafting —— 人打回。审的时候发现哪节没写对，退回
//     继续对话，而不是只能批准或放弃。
//
//  3. approved 只有一条出边 → converted —— D10-4 的代码化：签字后 PRD 冻结，
//     既不能回到起草（那就是事后改需求），也不能放弃（已经签过字的东西
//     要作废，走的是「生成出来的任务各自取消」，不是把 PRD 本身抹掉）。
//     漏了要改的，开一份新 PRD 引用这份（prds.ref_prd_id）。
//
//  4. drafting → ready_for_review 是唯一进入评审的入口 —— 没有从
//     awaiting_answers 直达的边：还挂着未回答问题（❓）的 PRD 不许进评审
//     （docs/10 §1.2 的进入条件）。要进评审必须先回到 drafting 把问题消化掉。
//
//  5. abandoned → drafting —— 搁置/放弃之后还能回来（docs/10 §9 第 1 条已决）。
//     终态的含义是「不再自动流转」，不是「不可复活」：与 task.StateFailed
//     （终态但允许人工重新入队）同形。超时搁置最可能的情形恰恰是「其实
//     还想做，只是那阵子没人顾上」，此时让人重建整段对话与已确认的节，
//     代价远大于给他一条回来的路。
//
//     复活走这一条边而不是新增一个 stale 状态：两种搁置（人主动放弃、
//     超时自动放弃）在语义上都是「这份 PRD 停了」，事后区分它们的价值
//     （产线信号：多少 PRD 是被晾掉的）不足以换来一个新状态、一条改
//     CHECK 约束的迁移与两套前端徽标。这个代价是明确接受的 —— 事后只看
//     状态分不出这两种，要那个信号就得另找入口（例如给 prd_events 加一类
//     状态事件，但那是另一个改动，且会污染「prd_events 只装 agent 事件」
//     这条现有约束）。
//
//     注意这条边是**人工**唤醒，不是自动回流：没有任何后台组件会把人
//     搁置的 PRD 悄悄拉回 drafting（那会让「放弃」这个动作失去意义）。
//     唯一触发者是人点「继续这份 PRD」（httpapi 的 resume 端点）。
var transitions = map[State][]State{
	StateDrafting:        {StateAwaitingAnswers, StateReadyForReview, StateAbandoned},
	StateAwaitingAnswers: {StateDrafting, StateAbandoned},
	StateReadyForReview:  {StateApproved, StateDrafting, StateAbandoned},

	// 冻结：只能往前走到生成
	StateApproved: {StateConverted},

	// 终态
	StateConverted: {},
	// 搁置/放弃后可由人唤醒回起草（见上第 5 条）。
	StateAbandoned: {StateDrafting},
}

// terminal 标记终态。
//
// 与 task 包同一立场：终态 = 「不会自己动」（没有自动出边、后台轮询器
// 不碰它），不等于「死透」。failed 能人工重新入队，abandoned 也能人工
// 唤醒回起草。converted 是唯一真正没有出路的终态 —— 任务已经发出去了，
// 要改只能开新 PRD 并引用这份（D10-4）。
var terminal = map[State]bool{
	StateConverted: true,
	StateAbandoned: true,
}

// Valid 报告 s 是否是已知状态。
func (s State) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// Terminal 报告 s 是否为终态。
func (s State) Terminal() bool { return terminal[s] }

// Active 报告这份 PRD 是否还在进行中。
func (s State) Active() bool { return s.Valid() && !s.Terminal() }

// Frozen 报告 PRD 内容是否已冻结（D10-4）。
//
// 冻结后 document / task_blocks / review_report 一律不许再写 —— store 层
// 的更新路径据此拒绝，而不是靠调用方自觉。approved 之后导出内容也据此
// 保证稳定（docs/10 §7）。
func (s State) Frozen() bool {
	return s == StateApproved || s == StateConverted
}

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
		return fmt.Sprintf("prd: 源状态 %q 未知", e.From)
	}
	if !e.To.Valid() {
		return fmt.Sprintf("prd: 目标状态 %q 未知", e.To)
	}
	if e.From.Terminal() && len(transitions[e.From]) == 0 {
		// converted 之外的终态（abandoned）还有一条人工唤醒的边，
		// 走下面的通用分支把合法后继列出来更有用。
		return fmt.Sprintf("prd: %s 是终态，不能转到 %s", e.From, e.To)
	}
	if e.From == StateApproved {
		return fmt.Sprintf("prd: %s 已冻结，只能转到 %s（要改内容请开新 PRD 并引用这份）",
			StateApproved, StateConverted)
	}
	return fmt.Sprintf("prd: 不允许从 %s 转到 %s（合法后继：%v）", e.From, e.To, NextStates(e.From))
}

// Validate 校验一次转移，非法则返回 ErrIllegalTransition。
func Validate(from, to State) error {
	if !CanTransition(from, to) {
		return ErrIllegalTransition{From: from, To: to}
	}
	return nil
}
