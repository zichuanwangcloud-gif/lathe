package task

import (
	"strings"
	"testing"
)

func TestAllStatesValid(t *testing.T) {
	all := AllStates()
	if len(all) != 11 {
		t.Errorf("状态总数 = %d，设计约定 11 个（docs/02-design.md §3）", len(all))
	}
	for _, s := range all {
		if !s.Valid() {
			t.Errorf("状态 %s 不在转移表中", s)
		}
	}
	if State("bogus").Valid() {
		t.Error("未知状态 bogus 被判为合法")
	}

	// AllStates 必须与转移表的 key 集合完全一致，防止加了状态忘了登记
	if len(all) != len(transitions) {
		t.Errorf("AllStates 有 %d 个，转移表有 %d 个 key，两者必须一致", len(all), len(transitions))
	}
}

func TestTerminalAndActive(t *testing.T) {
	wantTerminal := map[State]bool{
		StateMerged: true, StateFailed: true, StateCancelled: true,
	}
	for _, s := range AllStates() {
		if got, want := s.Terminal(), wantTerminal[s]; got != want {
			t.Errorf("%s.Terminal() = %v，期望 %v", s, got, want)
		}
		if got, want := s.Active(), !wantTerminal[s]; got != want {
			t.Errorf("%s.Active() = %v，期望 %v", s, got, want)
		}
	}
	if State("bogus").Active() {
		t.Error("未知状态不应被判为 Active")
	}
}

func TestLegalTransitions(t *testing.T) {
	legal := [][2]State{
		{StateQueued, StateTriaging},
		{StateTriaging, StateImplementing},     // direct 档：分诊通过直接实现
		{StateTriaging, StateBlockedSpec},      // 单子不明确
		{StateTriaging, StateAwaitingApproval}, // plan-first / manual 档
		{StateBlockedSpec, StateQueued},        // 人补充需求后重新入队
		{StateAwaitingApproval, StateImplementing},
		{StateImplementing, StateVerifying},
		{StateVerifying, StatePROpen},
		{StateVerifying, StateBlockedSpec}, // 红-绿：改前跑不失败 ⇒ 没理解 bug
		{StatePROpen, StateReviewFeedback},
		{StatePROpen, StateMerged},
		{StateReviewFeedback, StateImplementing}, // 二轮
		{StateReviewFeedback, StateMerged},
		{StateImplementing, StateQueued}, // 租约到期重新派发
		{StateVerifying, StateQueued},
		{StateFailed, StateQueued}, // 人工重试
	}
	for _, tc := range legal {
		if !CanTransition(tc[0], tc[1]) {
			t.Errorf("%s → %s 应合法，却被拒绝", tc[0], tc[1])
		}
		if err := Validate(tc[0], tc[1]); err != nil {
			t.Errorf("Validate(%s, %s) 应通过，得到: %v", tc[0], tc[1], err)
		}
	}
}

func TestIllegalTransitions(t *testing.T) {
	illegal := [][2]State{
		{StateQueued, StateImplementing},      // 必须先经分诊
		{StateQueued, StateVerifying},         // 不能跳过实现
		{StateImplementing, StatePROpen},      // 不能跳过验证 ★核心不变式
		{StateTriaging, StatePROpen},          // 不能跳过实现与验证
		{StateMerged, StateQueued},            // 终态不可复活
		{StateCancelled, StateQueued},         // 终态不可复活
		{StateMerged, StateImplementing},      // 终态不可复活
		{StateBlockedSpec, StateImplementing}, // 必须回 queued 重新分诊
		{State("bogus"), StateQueued},         // 未知源状态
		{StateQueued, State("bogus")},         // 未知目标状态
	}
	for _, tc := range illegal {
		if CanTransition(tc[0], tc[1]) {
			t.Errorf("%s → %s 应被拒绝，却判为合法", tc[0], tc[1])
		}
		if err := Validate(tc[0], tc[1]); err == nil {
			t.Errorf("Validate(%s, %s) 应报错，却通过了", tc[0], tc[1])
		}
	}
}

// 核心不变式：任何路径都不能绕过 verifying 到达 pr_open。
// 这是本产品的立足点 —— 未经验证的改动不许开 PR。
func TestNoPathToPROpenBypassesVerification(t *testing.T) {
	for _, from := range AllStates() {
		if !CanTransition(from, StatePROpen) {
			continue
		}
		if from != StateVerifying {
			t.Errorf("%s → pr_open 绕过了验证；只有 verifying 可进入 pr_open", from)
		}
	}
}

// 每个非终态都必须能走到某个终态，否则任务会永久卡死。
func TestEveryStateCanReachTerminal(t *testing.T) {
	for _, start := range AllStates() {
		if start.Terminal() {
			continue
		}
		if !reaches(start, func(s State) bool { return s.Terminal() }) {
			t.Errorf("状态 %s 无法到达任何终态，任务会卡死", start)
		}
	}
}

// 每个状态都必须能从 queued 到达，否则是死代码。
func TestEveryStateReachableFromQueued(t *testing.T) {
	for _, target := range AllStates() {
		if target == StateQueued {
			continue
		}
		if !reaches(StateQueued, func(s State) bool { return s == target }) {
			t.Errorf("状态 %s 无法从 queued 到达，是不可达的死状态", target)
		}
	}
}

// reaches 从 start 出发做 BFS，判断是否存在满足 pred 的可达状态。
func reaches(start State, pred func(State) bool) bool {
	seen := map[State]bool{start: true}
	queue := []State{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range transitions[cur] {
			if pred(next) {
				return true
			}
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return false
}

func TestNextStatesSorted(t *testing.T) {
	got := NextStates(StateTriaging)
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Errorf("NextStates 未按字母序返回: %v", got)
			break
		}
	}
	// 返回的必须是副本，改它不能污染转移表
	if len(got) > 0 {
		got[0] = "tampered"
		if NextStates(StateTriaging)[0] == "tampered" {
			t.Error("NextStates 返回了内部切片，调用方可篡改转移表")
		}
	}
	if n := NextStates(State("bogus")); len(n) != 0 {
		t.Errorf("未知状态的后继应为空，得到 %v", n)
	}
}

func TestErrIllegalTransitionMessages(t *testing.T) {
	cases := []struct {
		from, to State
		want     string
	}{
		{State("bogus"), StateQueued, "源状态"},
		{StateQueued, State("bogus"), "目标状态"},
		{StateMerged, StateQueued, "终态"},
		{StateQueued, StateVerifying, "不允许从"},
	}
	for _, tc := range cases {
		err := Validate(tc.from, tc.to)
		if err == nil {
			t.Fatalf("Validate(%s, %s) 应报错", tc.from, tc.to)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("错误 %q 应包含 %q", err.Error(), tc.want)
		}
	}
}

// docs/02-design.md §3 约束①：review 二轮必须 resume 原会话。
func TestRequiresSession(t *testing.T) {
	if !RequiresSession(StateReviewFeedback, StateImplementing) {
		t.Error("review_feedback → implementing 必须要求 agent_session_id（--resume）")
	}
	notRequired := [][2]State{
		{StateTriaging, StateImplementing},         // 首轮，还没有会话
		{StateAwaitingApproval, StateImplementing}, // 首轮
		{StateImplementing, StateVerifying},
		{StateFailed, StateQueued},
	}
	for _, tc := range notRequired {
		if RequiresSession(tc[0], tc[1]) {
			t.Errorf("%s → %s 不应强制要求会话", tc[0], tc[1])
		}
	}
}
