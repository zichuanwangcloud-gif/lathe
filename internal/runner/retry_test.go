package runner

import (
	"strings"
	"testing"

	"github.com/Clouditera/lathe/internal/task"
)

// 现场体检的便捷构造
func wtOK() *WorktreeState {
	return &WorktreeState{Exists: true, Registered: true, BranchExists: true, HasCommits: true, Commits: 2}
}

func wtDirty() *WorktreeState {
	s := wtOK()
	s.Dirty = true
	return s
}

func wtNoCommits() *WorktreeState {
	return &WorktreeState{Exists: true, Registered: true, BranchExists: true}
}

func wtGone() *WorktreeState { return &WorktreeState{} }

func TestPlanRetryDecisionTable(t *testing.T) {
	cases := []struct {
		name string
		mode RetryMode
		in   RetryInput
		// 期望
		fresh         bool
		entry         EntryStage
		resumeSession bool
		reasonLike    string // 理由里应包含的子串
	}{
		// ---- 分诊及之前：现场还没建起来，从头跑最便宜 ----
		{"分诊失败无现场", RetryAuto,
			RetryInput{Stage: StageTriageRun}, true, EntryTriage, false, "建现场之前"},
		{"分诊失败即使有残留现场也重跑分诊", RetryAuto,
			RetryInput{Stage: StageTriageParse, WT: wtNoCommits()}, true, EntryTriage, false, "建现场之前"},
		{"拉issue失败", RetryAuto,
			RetryInput{Stage: StageFetchIssue}, true, EntryTriage, false, ""},

		// ---- 实现阶段中断：有现场有会话 → resume 续跑 ----
		{"实现中断+现场+会话", RetryAuto,
			RetryInput{Stage: StageImplementRun, HasSession: true, WT: wtNoCommits()},
			false, EntryImplement, true, "续跑原会话"},
		{"实现未完工+现场+会话", RetryAuto,
			RetryInput{Stage: StageImplementIncomplete, HasSession: true, WT: wtNoCommits()},
			false, EntryImplement, true, ""},
		{"实现无改动+现场+会话", RetryAuto,
			RetryInput{Stage: StageImplementNoChanges, HasSession: true, WT: wtOK()},
			false, EntryImplement, true, ""},
		{"实现中断+现场无会话", RetryAuto,
			RetryInput{Stage: StageImplementRun, WT: wtNoCommits()},
			false, EntryImplement, false, "新会话收尾"},
		{"实现中断+现场丢失", RetryAuto,
			RetryInput{Stage: StageImplementRun, HasSession: true, WT: wtGone()},
			true, EntryTriage, false, "现场已不可用"},

		// ---- 提交阶段 ----
		{"提交失败+现场在", RetryAuto,
			RetryInput{Stage: StageCommit, WT: wtDirty()},
			false, EntryCommit, false, "从提交处续跑"},
		{"检查改动失败+现场丢失", RetryAuto,
			RetryInput{Stage: StageCheckChanges, WT: nil},
			true, EntryTriage, false, ""},

		// ---- 验证没跑成（环境/槽位）：直接重验，不动用 agent ----
		{"槽位超时+已提交", RetryAuto,
			RetryInput{Stage: StageVerifyGate, WT: wtOK()},
			false, EntryVerify, false, "不动用 agent"},
		{"验证执行错误+已提交", RetryAuto,
			RetryInput{Stage: StageVerifyRun, WT: wtOK()},
			false, EntryVerify, false, ""},
		{"验证执行错误+有未提交改动", RetryAuto,
			RetryInput{Stage: StageVerifyRun, WT: wtDirty()},
			false, EntryCommit, false, "先提交再验证"},
		{"验证执行错误+无提交", RetryAuto,
			RetryInput{Stage: StageVerifyRun, WT: wtNoCommits()},
			true, EntryTriage, false, ""},

		// ---- 验证未通过 ----
		{"验证未通过+会话在", RetryAuto,
			RetryInput{Stage: StageVerifyFailed, HasSession: true, WT: wtOK()},
			false, EntryImplement, true, "修复回路"},
		{"验证未通过+人工改动", RetryAuto,
			RetryInput{Stage: StageVerifyFailed, HasSession: true, WT: wtDirty()},
			false, EntryCommit, false, "人工介入"},
		{"验证未通过+无会话", RetryAuto,
			RetryInput{Stage: StageVerifyFailed, WT: wtOK()},
			false, EntryVerify, false, "重验一次"},
		{"验证未通过+现场丢失", RetryAuto,
			RetryInput{Stage: StageVerifyFailed, HasSession: true, WT: wtGone()},
			true, EntryTriage, false, ""},

		// ---- 推送/开 PR：验证已过，只补尾巴 ----
		{"推送失败+提交完好", RetryAuto,
			RetryInput{Stage: StagePush, WT: wtOK()},
			false, EntryPush, false, "不重烧实现与验证"},
		{"创建PR失败+提交完好", RetryAuto,
			RetryInput{Stage: StageCreatePR, WT: wtOK()},
			false, EntryPush, false, ""},
		{"推送失败+现场丢失", RetryAuto,
			RetryInput{Stage: StagePush, WT: wtGone()},
			true, EntryTriage, false, ""},

		// ---- 崩溃恢复（无 failure_stage，由中断状态推导）----
		{"崩溃于分诊", RetryAuto,
			RetryInput{InterruptedState: task.StateTriaging}, true, EntryTriage, false, ""},
		{"崩溃于实现+会话", RetryAuto,
			RetryInput{InterruptedState: task.StateImplementing, HasSession: true, WT: wtNoCommits()},
			false, EntryImplement, true, ""},
		{"崩溃于验证+已提交", RetryAuto,
			RetryInput{InterruptedState: task.StateVerifying, WT: wtOK()},
			false, EntryVerify, false, ""},

		// ---- 旧数据（无 stage 无 hint）----
		{"旧数据+提交完好", RetryAuto,
			RetryInput{WT: wtOK()}, false, EntryVerify, false, "失败阶段未知"},
		{"旧数据+无提交", RetryAuto,
			RetryInput{HasSession: true, WT: wtNoCommits()}, false, EntryImplement, true, ""},
		{"旧数据+无现场", RetryAuto,
			RetryInput{}, true, EntryTriage, false, ""},

		// ---- 显式模式 ----
		{"强制重建", RetryFresh,
			RetryInput{Stage: StagePush, WT: wtOK()}, true, EntryTriage, false, "从头重跑"},
		{"强制续跑+现场可用", RetryResume,
			RetryInput{Stage: StagePush, WT: wtOK()}, false, EntryPush, false, ""},
		{"强制续跑+现场丢失=拒绝", RetryResume,
			RetryInput{Stage: StagePush, WT: wtGone()}, false, EntryTriage, false, "不可用"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := PlanRetry(tc.mode, tc.in)
			if plan.Fresh != tc.fresh {
				t.Errorf("Fresh = %v，期望 %v（理由：%v）", plan.Fresh, tc.fresh, plan.Reasons)
			}
			if plan.Entry != tc.entry {
				t.Errorf("Entry = %s，期望 %s（理由：%v）", plan.Entry, tc.entry, plan.Reasons)
			}
			if plan.ResumeSession != tc.resumeSession {
				t.Errorf("ResumeSession = %v，期望 %v", plan.ResumeSession, tc.resumeSession)
			}
			if tc.reasonLike != "" {
				joined := strings.Join(plan.Reasons, " | ")
				if !strings.Contains(joined, tc.reasonLike) {
					t.Errorf("理由 %q 应包含 %q", joined, tc.reasonLike)
				}
			}
			if len(plan.Reasons) == 0 {
				t.Error("任何决策都必须给出人读理由")
			}
		})
	}
}

// 非法模式与空模式。
func TestRetryModeValid(t *testing.T) {
	for _, m := range []RetryMode{"", RetryAuto, RetryResume, RetryFresh} {
		if !m.Valid() {
			t.Errorf("模式 %q 应合法", m)
		}
	}
	if RetryMode("bogus").Valid() {
		t.Error("未知模式应非法")
	}
}

// 阶段序：before 是决策「现场是否还没建起来」的基础。
func TestStageOrder(t *testing.T) {
	if !StageTriageRun.before(StageImplementRun) {
		t.Error("triage 应在 implement 之前")
	}
	if !StageCreateWorktree.before(StageImplementRun) {
		t.Error("create_worktree 应在 implement 之前")
	}
	if StagePush.before(StageVerifyFailed) {
		t.Error("push 不应在 verify 之前")
	}
	// 未知阶段按最靠后处理（保守：有现场就尽量用）
	if (Stage("bogus")).before(StagePush) {
		t.Error("未知阶段应按最靠后处理")
	}
}
