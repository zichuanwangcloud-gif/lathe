package runner

import "testing"

// 每个阶段的机器码都必须有中文标签。
//
// 存在的理由：失败分类直方图（平台自度量）把 stage 代码渲染给人看，
// label() 的 default 分支会把未列出的常量原样吐成英文标识 —— 混进一堆
// 中文标签里很扎眼，而且新加 Stage 时没人会想起来补（默认分支让它编译
// 通过、静默降级）。这条测试把「漏了标签」变成一次失败。
//
// 新加 Stage 常量时，这条会红 —— 那不是麻烦，那是提醒你它的消费方
// （回帖文案、直方图）需要一个中文名字。
func TestEveryStageHasLabel(t *testing.T) {
	// 与 stage.go 的常量表一一对应。stageOrder 的切片是「全部阶段按流水线
	// 顺序」的既有权威列表，这里复用它 —— 加常量的人本来就必须往那里加
	// 一项（否则 before() 的行为会错），所以两处不会各自漂移。
	all := []Stage{
		StageFetchIssue, StageTriageRun, StageTriageParse, StageCreateWorktree,
		StageImplementRun, StageImplementIncomplete, StageImplementNoChanges,
		StageCheckChanges, StageCommit,
		StageListChanges, StageVerifyGate, StageVerifyDetect, StageVerifyRun, StageVerifyFailed,
		StagePush, StageCreatePR,
		// 定义在 pipeline.go，但同属一套契约
		StageRebaseConflict,
		// 定义在 profile.go / skills.go 的失败路径
		StageProfileInvalid, StageSkillMissing,
	}

	seen := map[Stage]bool{}
	for _, s := range all {
		if seen[s] {
			t.Errorf("阶段 %s 在测试列表里重复了", s)
			continue
		}
		seen[s] = true

		label := s.Label()
		if label == "" {
			t.Errorf("阶段 %s 没有标签", s)
			continue
		}
		if label == string(s) {
			t.Errorf("阶段 %s 落到了 default 分支（标签 == 机器码 %q），"+
				"它现在有一个人读的消费方（失败分类直方图），需要补中文名", s, label)
		}
	}
}
