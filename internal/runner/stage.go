package runner

// stage.go 定义流水线的失败阶段代码（FailureStage）。
//
// 用途：fail() 把阶段代码落进 tasks.failure_stage，智能重试（retry.go）
// 据此决策断点续跑的入口。failure_reason 是给人看的自由文本，不适合
// 机器判定；这里的 code 是稳定的程序内契约，UI 与事件流共用。

// Stage 是流水线的一个失败阶段代码。
type Stage string

const (
	// StageFetchIssue 拉取 issue 失败（Linear API 侧）。
	StageFetchIssue Stage = "fetch_issue"
	// StageTriageRun 分诊 agent 执行失败。
	StageTriageRun Stage = "triage_run"
	// StageTriageParse 分诊输出无法解析。
	StageTriageParse Stage = "triage_parse"
	// StageCreateWorktree 创建工作区失败。
	StageCreateWorktree Stage = "create_worktree"

	// StageImplementRun 实现 agent 执行失败（进程错误/超时等）。
	StageImplementRun Stage = "implement_run"
	// StageImplementIncomplete 实现 agent 未成功完成（IsError）。
	StageImplementIncomplete Stage = "implement_incomplete"
	// StageImplementNoChanges 实现结束但工作区无任何改动。
	StageImplementNoChanges Stage = "implement_no_changes"
	// StageCheckChanges 检查改动失败（git status 本身出错）。
	StageCheckChanges Stage = "check_changes"
	// StageCommit 提交改动失败。
	StageCommit Stage = "commit"

	// StageListChanges 定档前列改动文件失败。
	StageListChanges Stage = "list_changes"
	// StageVerifyGate 等验证槽位超时。
	StageVerifyGate Stage = "verify_gate"
	// StageVerifyDetect 无法确定验证步骤。
	StageVerifyDetect Stage = "verify_detect"
	// StageVerifyRun 验证执行错误（不是未通过，是跑不起来）。
	StageVerifyRun Stage = "verify_run"
	// StageVerifyFailed 验证未通过（含修复回路用尽）。
	StageVerifyFailed Stage = "verify_failed"

	// StagePush 推送分支失败。
	StagePush Stage = "push"
	// StageCreatePR 创建 PR 失败。
	StageCreatePR Stage = "create_pr"
)

// Label 是 label() 的导出形式。
//
// 存在的唯一理由：失败分类直方图（平台自度量）要把 failure_stage 渲染成
// 中文。措辞是本包的知识，不该复制一份到 httpapi 或前端去 —— 那边复制
// 一份就一定会漏改（stage.go 的集合会随流水线演进而增加，这正是 0012
// 迁移不给这一列加 CHECK 的原因）。
func (s Stage) Label() string { return s.label() }

// label 是给人看的阶段名（failure_reason 前缀与回帖沿用中文）。
func (s Stage) label() string {
	switch s {
	case StageFetchIssue:
		return "拉取 issue 失败"
	case StageTriageRun:
		return "分诊执行失败"
	case StageTriageParse:
		return "分诊结果无法解析"
	case StageCreateWorktree:
		return "创建工作区失败"
	case StageImplementRun:
		return "实现执行失败"
	case StageImplementIncomplete:
		return "实现未成功完成"
	case StageImplementNoChanges:
		return "agent 没有产生任何改动"
	case StageCheckChanges:
		return "检查改动失败"
	case StageCommit:
		return "提交改动失败"
	case StageListChanges:
		return "列出改动文件失败"
	case StageVerifyGate:
		return "等待验证槽位超时"
	case StageVerifyDetect:
		return "无法确定验证步骤"
	case StageVerifyRun:
		return "验证执行失败"
	case StageVerifyFailed:
		return "验证未通过"
	case StagePush:
		return "推送分支失败"
	case StageCreatePR:
		return "创建 PR 失败"
	case StageRebaseConflict:
		// 这条常量定义在 pipeline.go（它的触发路径是 MergePoller 而不是
		// Pipeline.Execute，见那里的注释）。**在这里补上标签**是因为它现在
		// 有了一个人读的消费方：平台自度量的失败分类直方图 ——
		// 落到 default 分支会把 "rebase_conflict" 这个英文标识原样混进
		// 一堆中文标签里。
		return "自动 rebase 冲突"
	case StageProfileInvalid:
		// 常量在 profile.go。同样是为了直方图（见上面那条的理由）。
		return "节点画像非法"
	case StageSkillMissing:
		// 常量在 skills.go。
		return "技能缺失"
	}
	return string(s)
}

// before 报告 s 是否在 other 之前（按流水线阶段序）。用于「失败点是否
// 还没建出现场」这类判断。未知阶段按最靠后处理（保守：有现场就尽量用）。
func (s Stage) before(other Stage) bool { return stageOrder(s) < stageOrder(other) }

func stageOrder(s Stage) int {
	order := []Stage{
		StageFetchIssue, StageTriageRun, StageTriageParse, StageCreateWorktree,
		StageImplementRun, StageImplementIncomplete, StageImplementNoChanges,
		StageCheckChanges, StageCommit,
		StageListChanges, StageVerifyGate, StageVerifyDetect, StageVerifyRun, StageVerifyFailed,
		StagePush, StageCreatePR,
	}
	for i, x := range order {
		if x == s {
			return i
		}
	}
	return len(order)
}
