package prd

import (
	"fmt"
	"strings"
)

// 本文件是 §5.1 可追溯闭环与 §8 结构块校验。
//
// 闭环的判据（docs/10 §5.1）：每条 AC 回指一个 G-n 和一个 S-n；每个 G、
// 每个 S 至少一条 AC；每条 AC 至少一个 T 交付。三对双向，**任何方向出现
// 孤儿都是缺陷**。
//
// 孤儿的两种含义都致命：G 没有 AC = 漏了目标（说要做但没人验）；
// AC 没有 G = 测了没人要的东西（浪费一个任务的红-绿回路）。

// checkTraceability 校验 G↔AC、S↔AC、AC↔T 三对双向覆盖。
func checkTraceability(d *Document, blocks *TaskBlocks, r *Report) {
	goals := map[string]bool{}
	for _, g := range d.GoalList {
		goals[g.ID] = false
	}
	scenarios := map[string]bool{}
	for _, s := range d.ScenarioList {
		scenarios[s.ID] = false
	}

	// AC → G/S：引用必须存在（拦幻觉：智能体编一个 G-9 出来）。
	acCovered := map[string]bool{}
	for _, c := range d.Criteria {
		if c.NA {
			continue
		}
		acCovered[c.ID] = false

		if len(c.GoalRefs) == 0 || len(c.ScenarioRefs) == 0 {
			r.Blocking = append(r.Blocking, Finding{
				Code: CodeOrphanAC, Section: "acceptance",
				Message: fmt.Sprintf("%s 没有回指 G-n / S-n：无法判断它在验谁要的东西", c.ID),
			})
		}
		for _, ref := range c.GoalRefs {
			if _, ok := goals[ref]; !ok {
				r.Blocking = append(r.Blocking, Finding{
					Code: CodeDanglingRef, Section: "acceptance",
					Message: fmt.Sprintf("%s 引用了不存在的目标 %s", c.ID, ref),
				})
				continue
			}
			goals[ref] = true
		}
		for _, ref := range c.ScenarioRefs {
			if _, ok := scenarios[ref]; !ok {
				r.Blocking = append(r.Blocking, Finding{
					Code: CodeDanglingRef, Section: "acceptance",
					Message: fmt.Sprintf("%s 引用了不存在的场景 %s", c.ID, ref),
				})
				continue
			}
			scenarios[ref] = true
		}
	}

	// G/S → AC：每个目标、每个场景至少一条 AC。
	for _, g := range d.GoalList {
		if !goals[g.ID] {
			r.Blocking = append(r.Blocking, Finding{
				Code: CodeOrphanGoal, Section: "goals",
				Message: fmt.Sprintf("目标 %s 没有任何 AC 验它：说要做但没人验 = 漏了目标", g.ID),
			})
		}
	}
	for _, s := range d.ScenarioList {
		if !scenarios[s.ID] {
			r.Blocking = append(r.Blocking, Finding{
				Code: CodeOrphanScenario, Section: "scenarios",
				Message: fmt.Sprintf("场景 %s 没有任何 AC 覆盖", s.ID),
			})
		}
	}

	// AC → T：每条 AC 至少被一个任务交付。
	if blocks != nil {
		for _, t := range blocks.Tasks {
			for _, ac := range t.Acceptance {
				if _, ok := acCovered[ac]; !ok {
					r.Blocking = append(r.Blocking, Finding{
						Code: CodeDanglingRef, Section: "taskSplit",
						Message: fmt.Sprintf("任务 %s 引用了不存在或标了 N/A 的 %s", t.Key, ac),
					})
					continue
				}
				acCovered[ac] = true
			}
		}
		for _, c := range d.Criteria {
			if c.NA {
				continue
			}
			if !acCovered[c.ID] {
				r.Blocking = append(r.Blocking, Finding{
					Code: CodeOrphanAC, Section: "taskSplit",
					Message: fmt.Sprintf("%s 没有任何任务交付它：这条 AC 上线时不会被验", c.ID),
				})
			}
		}
	}
}

// checkTasks 校验 §4.1 的结构约束与 §4.2 的量级上限。
func checkTasks(d *Document, blocks *TaskBlocks, lim Limits, r *Report) {
	if blocks == nil || len(blocks.Tasks) == 0 {
		r.Blocking = append(r.Blocking, Finding{
			Code: CodeTaskNoAcceptance, Section: "taskSplit",
			Message: "§8 没有任何任务：PRD 批准后无从生成",
		})
		return
	}

	// 允许超限的任务（§10 有 accept_oversize 记录指向它）。
	oversizeAccepted := map[string]bool{}
	for _, dec := range d.Decisions {
		if dec.Kind == DecisionAcceptOversize {
			for _, t := range blocks.Tasks {
				if strings.Contains(dec.Decision, t.Key) {
					oversizeAccepted[t.Key] = true
				}
			}
		}
	}

	keys := map[string]bool{}
	for _, t := range blocks.Tasks {
		if keys[t.Key] {
			r.Blocking = append(r.Blocking, Finding{
				Code: CodeTaskKeyDup, Section: "taskSplit",
				Message: fmt.Sprintf("任务 key %s 重复", t.Key),
			})
		}
		keys[t.Key] = true

		if !t.Kind.Valid() {
			r.Blocking = append(r.Blocking, Finding{
				Code: CodeTaskKindInvalid, Section: "taskSplit",
				Message: fmt.Sprintf("任务 %s 的类型 %q 不在 fix/feature/hotfix 内", t.Key, t.Kind),
			})
		}
		if len(t.Acceptance) == 0 {
			r.Blocking = append(r.Blocking, Finding{
				Code: CodeTaskNoAcceptance, Section: "taskSplit",
				Message: fmt.Sprintf("任务 %s 没绑任何 AC：它交付什么无从判断", t.Key),
			})
		}
		if strings.TrimSpace(t.Description) == "" {
			r.Blocking = append(r.Blocking, Finding{
				Code: CodeTaskNoAcceptance, Section: "taskSplit",
				Message: fmt.Sprintf("任务 %s 缺 description：过不了分诊", t.Key),
			})
		}

		if !oversizeAccepted[t.Key] {
			if lim.MaxLines > 0 && t.Estimate.Lines > lim.MaxLines {
				r.Blocking = append(r.Blocking, Finding{
					Code: CodeTaskOversize, Section: "taskSplit",
					Message: fmt.Sprintf("任务 %s 估算 %d 行，超仓库上限 %d（拆开，或在 §10 记录接受超限）",
						t.Key, t.Estimate.Lines, lim.MaxLines),
				})
			}
			if lim.MaxFiles > 0 && t.Estimate.Files > lim.MaxFiles {
				r.Blocking = append(r.Blocking, Finding{
					Code: CodeTaskOversize, Section: "taskSplit",
					Message: fmt.Sprintf("任务 %s 估算 %d 个文件，超仓库上限 %d",
						t.Key, t.Estimate.Files, lim.MaxFiles),
				})
			}
		}
	}

	// 依赖引用必须存在，且入度 ≤ 1 由数据形状天然保证（DependsOn 是单值）。
	for _, t := range blocks.Tasks {
		if t.DependsOn == "" {
			continue
		}
		if !keys[t.DependsOn] {
			r.Blocking = append(r.Blocking, Finding{
				Code: CodeTaskDepMissing, Section: "taskSplit",
				Message: fmt.Sprintf("任务 %s 依赖的 %s 不存在", t.Key, t.DependsOn),
			})
		}
		if t.DependsOn == t.Key {
			r.Blocking = append(r.Blocking, Finding{
				Code: CodeTaskCycle, Section: "taskSplit",
				Message: fmt.Sprintf("任务 %s 依赖自己", t.Key),
			})
		}
	}

	// 环：ChainDepth 会检出，它同时给出链长。
	depth, err := blocks.MaxChainDepth()
	if err != nil {
		r.Blocking = append(r.Blocking, Finding{
			Code: CodeTaskCycle, Section: "taskSplit", Message: err.Error(),
		})
	} else if lim.MaxChain > 0 && depth > lim.MaxChain {
		// 超链长是警告不禁止（07 §F3.3）：长链只是慢，不是错。
		r.Warnings = append(r.Warnings, Finding{
			Code: CodeChainTooLong, Section: "taskSplit",
			Message: fmt.Sprintf("最长链 %d 节，超建议上限 %d：每多一节就多一份 worktree 与一次前驱合并后的 rebase",
				depth, lim.MaxChain),
		})
	}

	// 重构类的首节点必须是补特征测试（docs/10 §3 变体速查）：
	// 没有特征测试打底的重构，「行为保持」无从证明。
	if d.Type == TypeRefactor {
		roots := blocks.Roots()
		index := blocks.ByKey()
		ok := false
		for _, k := range roots {
			t := index[k]
			if t != nil && strings.Contains(t.Title, "特征测试") {
				ok = true
				break
			}
		}
		if !ok {
			r.Blocking = append(r.Blocking, Finding{
				Code: CodeRefactorFirstNode, Section: "taskSplit",
				Message: "重构类 PRD 的首节点必须是「补特征测试」任务，否则行为保持无从证明",
			})
		}
	}
}

// checkQuestions 校验 §9 未决问题定稿前清零。
func checkQuestions(d *Document, r *Report) {
	for _, q := range d.Questions {
		if q.Status == QuestionOpen {
			r.Blocking = append(r.Blocking, Finding{
				Code: CodeOpenQuestion, Section: "risks",
				Message: fmt.Sprintf("%s 仍未答：要么答，要么人显式标「接受不答」并进 §10", q.ID),
			})
		}
	}
}

// checkReview 校验对抗复核已跑且每条发现都有处置（D10-6 必跑）。
func checkReview(hasReview bool, unhandled int, r *Report) {
	if !hasReview {
		r.Blocking = append(r.Blocking, Finding{
			Code:    CodeNoReviewReport,
			Message: "对抗复核未跑：一组任何实现都能过的 AC 无权声称覆盖了意图（§5.5）",
		})
		return
	}
	if unhandled > 0 {
		r.Blocking = append(r.Blocking, Finding{
			Code:    CodeReviewUnhandled,
			Message: fmt.Sprintf("对抗复核还有 %d 条发现没标处置", unhandled),
		})
	}
}
