package prd

import "testing"

// TestStateInvariants 守的是 docs/10 §1.1 生命周期的几条不变量。
// 每个子测试对应一条「如果被改坏，产品语义就错了」的断言。
func TestStateInvariants(t *testing.T) {
	t.Run("每个非终态都有出边，不存在死锁", func(t *testing.T) {
		for _, s := range AllStates() {
			if s.Terminal() {
				continue
			}
			if got := NextStates(s); len(got) == 0 {
				t.Errorf("非终态 %s 没有任何合法后继：卡死在这里就再也出不去了", s)
			}
		}
	})

	t.Run("approved 只能转到 converted（D10-4 冻结）", func(t *testing.T) {
		got := NextStates(StateApproved)
		want := []State{StateConverted}
		if len(got) != len(want) || got[0] != want[0] {
			t.Errorf("approved 的后继 got=%v want=%v —— 签字后 PRD 不可变，"+
				"多一条出边就等于允许事后改需求", got, want)
		}
	})

	t.Run("不存在绕过 ready_for_review 直达 approved 的捷径", func(t *testing.T) {
		for _, from := range AllStates() {
			if from == StateReadyForReview {
				continue
			}
			if CanTransition(from, StateApproved) {
				t.Errorf("%s → approved 不该存在：批准前必须过自检清单与对抗复核", from)
			}
		}
	})

	t.Run("挂着未回答问题时不能进评审", func(t *testing.T) {
		if CanTransition(StateAwaitingAnswers, StateReadyForReview) {
			t.Error("awaiting_answers → ready_for_review 不该存在：" +
				"还有 ❓ 的 PRD 必须先回到 drafting 消化掉（docs/10 §1.2）")
		}
	})

	t.Run("终态没有出边", func(t *testing.T) {
		for _, s := range []State{StateConverted, StateAbandoned} {
			if got := NextStates(s); len(got) != 0 {
				t.Errorf("终态 %s 不该有后继，got=%v", s, got)
			}
		}
	})

	t.Run("活跃态都能被放弃，终态与冻结态不能", func(t *testing.T) {
		for _, s := range []State{StateDrafting, StateAwaitingAnswers, StateReadyForReview} {
			if !CanTransition(s, StateAbandoned) {
				t.Errorf("%s 应该能放弃：人随时有权停下", s)
			}
		}
		for _, s := range []State{StateApproved, StateConverted, StateAbandoned} {
			if CanTransition(s, StateAbandoned) {
				t.Errorf("%s → abandoned 不该存在", s)
			}
		}
	})
}

func TestFrozen(t *testing.T) {
	tests := []struct {
		state State
		want  bool
	}{
		{StateDrafting, false},
		{StateAwaitingAnswers, false},
		{StateReadyForReview, false},
		{StateApproved, true},
		{StateConverted, true},
		// 放弃的 PRD 没有「内容冻结」的语义 —— 它不会再被读，
		// 也不需要保证导出稳定。
		{StateAbandoned, false},
	}
	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			if got := tc.state.Frozen(); got != tc.want {
				t.Errorf("%s.Frozen() got=%v want=%v", tc.state, got, tc.want)
			}
		})
	}
}

func TestValidateRejectsUnknownStates(t *testing.T) {
	tests := []struct {
		name     string
		from, to State
	}{
		{"源状态未知", State("nonexistent"), StateDrafting},
		{"目标状态未知", StateDrafting, State("nonexistent")},
		{"两端都未知", State("a"), State("b")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.from, tc.to)
			if err == nil {
				t.Fatalf("Validate(%q, %q) 应当报错", tc.from, tc.to)
			}
			var ill ErrIllegalTransition
			if !asIllegal(err, &ill) {
				t.Fatalf("错误类型 got=%T want=ErrIllegalTransition", err)
			}
		})
	}
}

func TestValidateAcceptsMainLoop(t *testing.T) {
	// 多轮对话的主循环 + 一条通到底的快乐路径。
	path := []State{
		StateDrafting, StateAwaitingAnswers, StateDrafting,
		StateReadyForReview, StateApproved, StateConverted,
	}
	for i := 0; i+1 < len(path); i++ {
		if err := Validate(path[i], path[i+1]); err != nil {
			t.Errorf("%s → %s 应当合法：%v", path[i], path[i+1], err)
		}
	}
}

// asIllegal 是 errors.As 的窄化包装，避免测试里到处写类型断言。
func asIllegal(err error, target *ErrIllegalTransition) bool {
	if e, ok := err.(ErrIllegalTransition); ok {
		*target = e
		return true
	}
	return false
}
