package runner

import (
	"fmt"

	"github.com/Clouditera/lathe/internal/task"
)

// retry.go 智能重试的决策引擎。
//
// 问题：失败任务重试时，旧现场（worktree/分支/提交/agent 会话）是
// 该利用还是该丢弃？旧实现无条件丢弃重建 —— 失败发生在越靠后的阶段，
// 从头重跑的浪费越大（创建 PR 抖动一下就要重烧一遍实现+验证）。
//
// 这里的决策把流水线看作阶段链：
//
//	triage → implement → commit → verify → push → pr
//
// 按【失败阶段】与【现场体检】选择续跑入口；现场不可用或会话不可续时
// 逐级降级，最终兜底总是从头重建 —— 任何误判都不会比旧行为更糟。

// RetryMode 是调用方指定的重试模式。
type RetryMode string

const (
	// RetryAuto 由系统按失败阶段与现场状态决策（默认）。
	RetryAuto RetryMode = "auto"
	// RetryResume 尽量续跑；现场不可用时报错而非静默重建
	//（人明确说「我要续」时，悄悄重建是违背意图）。
	RetryResume RetryMode = "resume"
	// RetryFresh 强制丢弃现场从头重建。
	RetryFresh RetryMode = "fresh"
)

// Valid 报告模式是否合法。空串按 auto 处理（缺省值）。
func (m RetryMode) Valid() bool {
	switch m {
	case "", RetryAuto, RetryResume, RetryFresh:
		return true
	}
	return false
}

// EntryStage 是续跑进入流水线的位置。
type EntryStage string

const (
	// EntryTriage 从头跑（全新或重建）。
	EntryTriage EntryStage = "triage"
	// EntryImplement 回到实现阶段：resume 原会话继续/修复，
	// 或会话不可续时在同 worktree 开新会话收尾。
	EntryImplement EntryStage = "implement"
	// EntryCommit 从提交开始：工作区有（人手工或中断留下的）未提交
	// 改动，先提交再走验证。
	EntryCommit EntryStage = "commit"
	// EntryVerify 从验证开始：分支已提交，直接重验，不动用 agent。
	EntryVerify EntryStage = "verify"
	// EntryPush 从推送开始：验证已过，只补 push + 开 PR（均幂等）。
	EntryPush EntryStage = "push"
)

// Label 给人看的入口名（事件与 UI 展示）。
func (e EntryStage) Label() string {
	switch e {
	case EntryTriage:
		return "分诊（从头开始）"
	case EntryImplement:
		return "实现"
	case EntryCommit:
		return "提交"
	case EntryVerify:
		return "验证"
	case EntryPush:
		return "推送与开 PR"
	}
	return string(e)
}

// RetryPlan 是一次重试的决策结果。
type RetryPlan struct {
	// Fresh 为真表示丢弃现场从头重建；为假表示断点续跑。
	Fresh bool
	// Entry 是续跑入口（Fresh 时恒为 EntryTriage）。
	Entry EntryStage
	// ResumeSession 为真时实现阶段以 --resume 续原 agent 会话。
	ResumeSession bool
	// Reasons 是决策依据（落任务事件 + UI 展示），按决策顺序排列。
	Reasons []string
}

// RetryInput 是 PlanRetry 的全部输入。
type RetryInput struct {
	// Stage 是失败阶段代码（tasks.failure_stage）。崩溃恢复场景任务没
	// 走过 fail()，此值为空，由 InterruptedState 推导。
	Stage Stage
	// InterruptedState 是崩溃恢复时任务中断前的状态
	//（triaging/implementing/verifying），仅 Stage 为空时使用。
	InterruptedState task.State
	// HasSession 表示任务行持有 agent_session_id。
	HasSession bool
	// WT 是现场体检结果；nil 或不可用表示没有可续的现场。
	WT *WorktreeState
}

// PlanRetry 是纯逻辑决策：给定失败阶段与现场体检，产出重试计划。
// 决策表见函数体内的分支注释，每条规则都附带人读理由。
func PlanRetry(mode RetryMode, in RetryInput) RetryPlan {
	fresh := func(reasons ...string) RetryPlan {
		return RetryPlan{Fresh: true, Entry: EntryTriage, Reasons: reasons}
	}

	if mode == RetryFresh {
		return fresh("指定了从头重跑")
	}

	// ---------- 失败阶段归一化 ----------
	stage := in.Stage
	if stage == "" && in.InterruptedState != "" {
		stage = stageFromState(in.InterruptedState)
	}

	// 现场可用性是几乎所有续跑分支的前置条件，先算好。
	usable := in.WT.Usable()

	if mode == RetryResume && !usable {
		// 强制续跑但现场没了：不静默重建（违背意图），由调用方报错。
		// 用 Fresh=false + EntryTriage 表达「无法满足」，调用方应拒绝。
		return RetryPlan{Fresh: false, Entry: EntryTriage,
			Reasons: []string{"指定了续跑，但工作区现场已不可用（目录/分支缺失）"}}
	}

	// ---------- auto 决策 ----------
	switch {
	case stage.before(StageImplementRun) && stage != "":
		// 分诊及之前失败：现场还没建起来（或只有空 worktree），
		// 重跑分诊本来就便宜，且 issue 可能已被补充过，值得重新分诊。
		return fresh("失败发生在建现场之前，从头重跑代价最小")

	case stage == StageImplementRun || stage == StageImplementIncomplete ||
		stage == StageImplementNoChanges:
		if !usable {
			return fresh("实现阶段失败，但工作区现场已不可用")
		}
		if in.HasSession {
			return RetryPlan{Entry: EntryImplement, ResumeSession: true,
				Reasons: []string{"实现中断，工作区与 agent 会话均在，续跑原会话接着干"}}
		}
		return RetryPlan{Entry: EntryImplement,
			Reasons: []string{"实现中断，工作区在但无会话凭据，在同工作区开新会话收尾"}}

	case stage == StageCheckChanges || stage == StageCommit:
		if !usable {
			return fresh("提交阶段失败，但工作区现场已不可用")
		}
		return RetryPlan{Entry: EntryCommit,
			Reasons: []string{"实现已产出改动，从提交处续跑"}}

	case stage == StageListChanges || stage == StageVerifyGate ||
		stage == StageVerifyDetect || stage == StageVerifyRun:
		if !usable || !in.WT.HasCommits {
			return fresh("验证前失败，但工作区提交已不可用")
		}
		if in.WT.Dirty {
			return RetryPlan{Entry: EntryCommit,
				Reasons: []string{"验证未跑成，分支已有提交但工作区还有未提交改动，先提交再验证"}}
		}
		return RetryPlan{Entry: EntryVerify,
			Reasons: []string{"验证未跑成（环境/槽位问题），分支提交完好，直接重新验证，不动用 agent"}}

	case stage == StageVerifyFailed:
		if !usable {
			return fresh("验证未通过，但工作区现场已不可用")
		}
		if in.WT.Dirty {
			// 验证失败后工作区出现未提交改动 —— 多半是人进现场手工
			// 修了（D4 保留现场的本意）。先提交人的改动去验证，别让
			// agent 在人的半成品上画蛇添足。
			return RetryPlan{Entry: EntryCommit,
				Reasons: []string{"验证未通过，但检测到工作区有新的未提交改动（人工介入？），先提交再重验"}}
		}
		if in.HasSession {
			return RetryPlan{Entry: EntryImplement, ResumeSession: true,
				Reasons: []string{"验证未通过，resume 原实现会话进入修复回路（agent 还记得自己的思路）"}}
		}
		return RetryPlan{Entry: EntryVerify,
			Reasons: []string{"验证未通过且无会话可续，直接重验一次确认当前状态"}}

	case stage == StagePush || stage == StageCreatePR:
		if usable && in.WT.HasCommits {
			return RetryPlan{Entry: EntryPush,
				Reasons: []string{"验证已通过，仅推送/开 PR 未完成，直接补跑（幂等），不重烧实现与验证"}}
		}
		return fresh("推送/开 PR 失败，但本地现场已不可用")

	default:
		// 未知阶段（旧数据无 failure_stage）：有现场就保守续到验证前，
		// 没现场就重建。
		if usable && in.WT.HasCommits {
			if in.WT.Dirty {
				return RetryPlan{Entry: EntryCommit,
					Reasons: []string{"失败阶段未知，现场有提交与未提交改动，从提交处续跑"}}
			}
			return RetryPlan{Entry: EntryVerify,
				Reasons: []string{"失败阶段未知，现场提交完好，从验证处续跑"}}
		}
		if usable {
			return RetryPlan{Entry: EntryImplement, ResumeSession: in.HasSession,
				Reasons: []string{"失败阶段未知，现场在但无提交，回到实现阶段续跑"}}
		}
		return fresh("失败阶段未知且无可用现场，从头重建")
	}
}

// stageFromState 把崩溃中断时的任务状态映射为失败阶段，供启动恢复
// （Reconcile）场景决策：崩溃的任务没走过 fail()，没有 failure_stage，
// 但中断前所在的状态就是断点位置。
func stageFromState(s task.State) Stage {
	switch s {
	case task.StateTriaging:
		return StageTriageRun
	case task.StateImplementing:
		return StageImplementRun
	case task.StateVerifying:
		return StageVerifyRun
	}
	return ""
}

// String 汇总计划，用于日志。
func (p RetryPlan) String() string {
	mode := "续跑"
	if p.Fresh {
		mode = "重建"
	}
	return fmt.Sprintf("%s@%s", mode, p.Entry)
}
