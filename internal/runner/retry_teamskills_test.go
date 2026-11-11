package runner

import "testing"

func wtUsable(commits, dirty bool) *WorktreeState {
	return &WorktreeState{Exists: true, Registered: true, BranchExists: true, HasCommits: commits, Dirty: dirty}
}

// AC-13：team_skill_failed 无论分支有没有提交都回到 EntryImplement 重新物化 ——
// 任何跳过 stageImplement 的入口（提交/验证/推送）都会让团队技能被静默绕过。
func TestPlanRetryTeamSkillFailedAlwaysReturnsToImplement(t *testing.T) {
	cases := []struct {
		name       string
		wt         *WorktreeState
		session    bool
		wantFresh  bool
		wantEntry  EntryStage
		wantResume bool
	}{
		{"现场在无提交有会话续原会话", wtUsable(false, false), true, false, EntryImplement, true},
		{"现场在有提交不dirty即review二轮也回实现而非验证", wtUsable(true, false), true, false, EntryImplement, true},
		{"现场在有提交且dirty也回实现而非提交", wtUsable(true, true), true, false, EntryImplement, true},
		{"现场在有提交无会话回实现开新会话", wtUsable(true, false), false, false, EntryImplement, false},
		{"现场不可用从头重建", nil, true, true, EntryTriage, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PlanRetry(RetryAuto, RetryInput{Stage: StageTeamSkillFailed, HasSession: c.session, WT: c.wt})
			if got.Fresh != c.wantFresh || got.Entry != c.wantEntry || got.ResumeSession != c.wantResume {
				t.Errorf("PlanRetry got {Fresh:%v Entry:%s Resume:%v}, want {Fresh:%v Entry:%s Resume:%v}",
					got.Fresh, got.Entry, got.ResumeSession, c.wantFresh, c.wantEntry, c.wantResume)
			}
			if len(got.Reasons) == 0 {
				t.Error("决策应附人读理由")
			}
		})
	}
}

// 回归锁：未知阶段的 default 分支四格决策与改动前逐格一致（只加了一个 case）。
func TestPlanRetryUnknownStageDefaultUnchanged(t *testing.T) {
	cases := []struct {
		name      string
		wt        *WorktreeState
		wantFresh bool
		wantEntry EntryStage
	}{
		{"有提交不dirty到验证", wtUsable(true, false), false, EntryVerify},
		{"有提交dirty到提交", wtUsable(true, true), false, EntryCommit},
		{"无提交回实现", wtUsable(false, false), false, EntryImplement},
		{"无现场重建", nil, true, EntryTriage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PlanRetry(RetryAuto, RetryInput{Stage: Stage("bogus"), HasSession: true, WT: c.wt})
			if got.Fresh != c.wantFresh || got.Entry != c.wantEntry {
				t.Errorf("got {Fresh:%v Entry:%s}, want {Fresh:%v Entry:%s}", got.Fresh, got.Entry, c.wantFresh, c.wantEntry)
			}
		})
	}
}
