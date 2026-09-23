package prd

import "testing"

// 自检清单的测试（docs/10 §6.1）。
//
// 每个用例的形状都是「拿一份能过的 PRD，只破坏一处，断言恰好被这一条
// 校验拦住」—— 这样测的是「校验真的在拦这件事」，而不是「校验存在」。
// 全绿的清单如果拦不住任何东西，它就是 §5.5 里那种「任何实现都能过的
// AC」，没有资格声称自己在守门。

// okDoc 造一份能通过全部机器可判校验的最小 PRD。
//
// 刻意造得「刚好够过」而不是「丰满」：后面每个用例都从它身上破坏一处，
// 起点越贴着及格线，破坏的效果就越明确。
func okDoc() (*Document, *TaskBlocks) {
	accepted := true
	d := &Document{Type: TypeFeature}

	// 十节全 ✅（人已逐节确认）。
	for _, s := range d.Sections() {
		s.Section.Status = StatusConfirmed
	}
	d.OneLiner.Body = "让管理员在系统设置里配置任务预览的资源阈值"

	d.GoalList = []Goal{{ID: "G-1", Text: "阈值可配", Measure: "设置页改完立即生效"}}
	d.ScenarioList = []Scenario{{
		ID: "S-1", Title: "管理员改阈值",
		Given: "已登录管理员", When: "把阈值改为 75 并保存", Then: "预览在占用 80% 时被拒",
	}}

	// 七类固定行：一条真 AC + 六条 N/A + 理由。
	d.Criteria = []Criterion{{
		ID: "AC-1", Category: CatHappyPath,
		Given: "阈值设为 75", When: "内存占用 80% 时点预览", Then: "返回 409 且不起容器",
		Evidence: "HTTP 409 + docker ps 无新容器", Verify: VerifyAuto,
		GoalRefs: []string{"G-1"}, ScenarioRefs: []string{"S-1"},
		ReverseConfirm: "占用 74% 时不会被拦，接受吗", ReverseAccepted: &accepted,
	}}
	for _, cat := range StandardCategories() {
		if cat == CatHappyPath {
			continue
		}
		d.Criteria = append(d.Criteria, Criterion{
			ID: "AC-" + string(cat), Category: cat,
			NA: true, Reason: "本 PRD 不涉及",
		})
	}

	blocks := &TaskBlocks{Version: BlocksVersion, Repo: "acme/demo", Tasks: []TaskBlock{{
		Key: "T1", Title: "阈值改为系统设置项", Kind: KindFeature,
		Description: "把常量改为读 system_settings；必须交付测试证明阈值可配且非法值被拒",
		Acceptance:  []string{"AC-1"},
		Estimate:    Estimate{Lines: 120, Files: 3},
	}}}
	return d, blocks
}

func stdLimits() Limits { return Limits{MaxLines: 400, MaxFiles: 8, MaxChain: 4} }

// 先证明基准是绿的 —— 否则后面「破坏后变红」说明不了任何事。
func TestCheckPassesOnCompleteDocument(t *testing.T) {
	d, blocks := okDoc()
	r := Check(d, blocks, stdLimits(), true, 0)
	if !r.OK() {
		t.Fatalf("基准 PRD 应当通过自检，实际被拦：%v", r.Blocking)
	}
}

// 每个用例破坏一处，断言「被拦住」且「拦的是这一条」。
func TestCheckCatchesEachDefect(t *testing.T) {
	tests := []struct {
		name     string
		wantCode string
		break_   func(*Document, *TaskBlocks)
	}{
		{
			name: "挂着 ❓ 的节不许进评审", wantCode: CodePendingSection,
			break_: func(d *Document, _ *TaskBlocks) { d.Problem.Status = StatusPending },
		},
		{
			name: "🤖 推断未经人确认", wantCode: CodeUnconfirmed,
			break_: func(d *Document, _ *TaskBlocks) { d.Solution.Status = StatusInferred },
		},
		{
			name: "一句话超 60 字", wantCode: CodeOneLinerTooLong,
			break_: func(d *Document, _ *TaskBlocks) {
				for len([]rune(d.OneLiner.Body)) <= oneLinerMaxRunes {
					d.OneLiner.Body += "再补一点描述"
				}
			},
		},
		{
			name: "AC 表缺一类固定行", wantCode: CodeCategoryMissing,
			break_: func(d *Document, _ *TaskBlocks) {
				// 删掉「兼容与默认」那一行。
				out := d.Criteria[:0]
				for _, c := range d.Criteria {
					if c.Category != CatCompatibility {
						out = append(out, c)
					}
				}
				d.Criteria = out
			},
		},
		{
			name: "N/A 没写理由", wantCode: CodeNAWithoutReason,
			break_: func(d *Document, _ *TaskBlocks) { d.Criteria[1].Reason = "" },
		},
		{
			name: "AC 缺判据", wantCode: CodeACIncomplete,
			break_: func(d *Document, _ *TaskBlocks) { d.Criteria[0].Evidence = "" },
		},
		{
			name: "形容词冒充标准且无判据", wantCode: CodeBannedWord,
			break_: func(d *Document, _ *TaskBlocks) {
				d.Criteria[0].Evidence = ""
				d.Criteria[0].Then = "行为正确"
			},
		},
		{
			name: "人工走查没写步骤", wantCode: CodeManualNoSteps,
			break_: func(d *Document, _ *TaskBlocks) { d.Criteria[0].Verify = VerifyManual },
		},
		{
			name: "反向确认没答", wantCode: CodeReverseUnanswered,
			break_: func(d *Document, _ *TaskBlocks) { d.Criteria[0].ReverseAccepted = nil },
		},
		{
			name: "目标没有 AC 验它", wantCode: CodeOrphanGoal,
			break_: func(d *Document, _ *TaskBlocks) {
				d.GoalList = append(d.GoalList, Goal{ID: "G-2", Text: "顺手加的", Measure: "?"})
			},
		},
		{
			name: "场景没有 AC 覆盖", wantCode: CodeOrphanScenario,
			break_: func(d *Document, _ *TaskBlocks) {
				d.ScenarioList = append(d.ScenarioList, Scenario{ID: "S-2", Title: "漏了的场景"})
			},
		},
		{
			name: "AC 引用不存在的目标", wantCode: CodeDanglingRef,
			break_: func(d *Document, _ *TaskBlocks) { d.Criteria[0].GoalRefs = []string{"G-9"} },
		},
		{
			name: "AC 没回指 G/S", wantCode: CodeOrphanAC,
			break_: func(d *Document, _ *TaskBlocks) { d.Criteria[0].ScenarioRefs = nil },
		},
		{
			name: "AC 没有任务交付", wantCode: CodeOrphanAC,
			break_: func(_ *Document, b *TaskBlocks) { b.Tasks[0].Acceptance = []string{} },
		},
		{
			name: "任务 key 重复", wantCode: CodeTaskKeyDup,
			break_: func(_ *Document, b *TaskBlocks) {
				dup := b.Tasks[0]
				b.Tasks = append(b.Tasks, dup)
			},
		},
		{
			name: "任务类型不合法", wantCode: CodeTaskKindInvalid,
			break_: func(_ *Document, b *TaskBlocks) { b.Tasks[0].Kind = "chore" },
		},
		{
			name: "依赖的任务不存在", wantCode: CodeTaskDepMissing,
			break_: func(_ *Document, b *TaskBlocks) { b.Tasks[0].DependsOn = "T9" },
		},
		{
			name: "任务依赖自己", wantCode: CodeTaskCycle,
			break_: func(_ *Document, b *TaskBlocks) { b.Tasks[0].DependsOn = "T1" },
		},
		{
			name: "任务估算超行数上限", wantCode: CodeTaskOversize,
			break_: func(_ *Document, b *TaskBlocks) { b.Tasks[0].Estimate.Lines = 900 },
		},
		{
			name: "任务估算超文件数上限", wantCode: CodeTaskOversize,
			break_: func(_ *Document, b *TaskBlocks) { b.Tasks[0].Estimate.Files = 20 },
		},
		{
			name: "未决问题没清零", wantCode: CodeOpenQuestion,
			break_: func(d *Document, _ *TaskBlocks) {
				d.Questions = []Question{{ID: "Q-1", Text: "阈值算内存还是磁盘", Status: QuestionOpen}}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, blocks := okDoc()
			tc.break_(d, blocks)

			r := Check(d, blocks, stdLimits(), true, 0)
			if r.OK() {
				t.Fatalf("破坏后自检仍然通过 —— 这条校验没在拦它，want code=%s", tc.wantCode)
			}
			for _, f := range r.Blocking {
				if f.Code == tc.wantCode {
					return
				}
			}
			t.Errorf("want code=%s，got=%v", tc.wantCode, r.Blocking)
		})
	}
}

// 对抗复核必跑（D10-6）：没跑、或跑了但发现没处置，都不许进评审。
func TestCheckRequiresAdversarialReview(t *testing.T) {
	t.Run("没跑复核直接拒", func(t *testing.T) {
		d, blocks := okDoc()
		r := Check(d, blocks, stdLimits(), false, 0)
		if r.OK() {
			t.Fatal("对抗复核没跑就能进评审 —— D10-6「必跑」落空")
		}
		found := false
		for _, f := range r.Blocking {
			if f.Code == CodeNoReviewReport {
				found = true
			}
		}
		if !found {
			t.Errorf("want code=%s，got=%v", CodeNoReviewReport, r.Blocking)
		}
	})

	t.Run("复核发现没标处置也拒", func(t *testing.T) {
		d, blocks := okDoc()
		r := Check(d, blocks, stdLimits(), true, 2)
		if r.OK() {
			t.Fatal("复核发现没处置就能进评审：报告白跑了")
		}
	})
}

// 超链长是警告不是拒绝（07 §F3.3）：长链只是慢，不是错。
func TestChainTooLongWarnsButDoesNotBlock(t *testing.T) {
	d, blocks := okDoc()
	prev := "T1"
	for i := 2; i <= 6; i++ {
		key := "T" + string(rune('0'+i))
		blocks.Tasks = append(blocks.Tasks, TaskBlock{
			Key: key, Title: "第 " + key + " 节", Kind: KindFeature,
			DependsOn: prev, Description: "接着上一节做，交付对应测试",
			Acceptance: []string{"AC-1"}, Estimate: Estimate{Lines: 50, Files: 2},
		})
		prev = key
	}

	r := Check(d, blocks, stdLimits(), true, 0)
	if !r.OK() {
		t.Fatalf("超链长不该阻塞（只是警告），实际被拦：%v", r.Blocking)
	}
	found := false
	for _, w := range r.Warnings {
		if w.Code == CodeChainTooLong {
			found = true
		}
	}
	if !found {
		t.Errorf("超链长应当给出警告，warnings=%v", r.Warnings)
	}
}

// §10 记了「接受超限」的任务放行 —— 这是自检清单的唯一豁免口。
func TestOversizeAcceptedInDecisionLog(t *testing.T) {
	d, blocks := okDoc()
	blocks.Tasks[0].Estimate.Lines = 900

	if r := Check(d, blocks, stdLimits(), true, 0); r.OK() {
		t.Fatal("先确认超限本来会被拦，否则下面的豁免测试说明不了什么")
	}

	d.Decisions = []Decision{{
		Round: 4, Kind: DecisionAcceptOversize,
		Question: "T1 估算 900 行要不要拆",
		Decision: "接受 T1 超限，迁移与读取逻辑耦合太紧，拆开反而要来回改",
	}}
	if r := Check(d, blocks, stdLimits(), true, 0); !r.OK() {
		t.Errorf("§10 记了接受超限后应放行，实际仍被拦：%v", r.Blocking)
	}
}

// 重构类变体：首节点必须是补特征测试，AC 只允许两类。
func TestRefactorVariant(t *testing.T) {
	newRefactorDoc := func() (*Document, *TaskBlocks) {
		d, blocks := okDoc()
		d.Type = TypeRefactor
		d.Criteria = []Criterion{
			{
				ID: "AC-1", Category: CatBehaviorPreserved,
				Given: "重构前", When: "跑现有测试", Then: "全绿",
				Evidence: "go test ./internal/preview 全通过", Verify: VerifyAuto,
				GoalRefs: []string{"G-1"}, ScenarioRefs: []string{"S-1"},
			},
			{
				ID: "AC-2", Category: CatStructuralMetric,
				NA: true, Reason: "本次只拆函数，不改依赖方向",
			},
		}
		return d, blocks
	}

	t.Run("首节点不是特征测试任务则拒", func(t *testing.T) {
		d, blocks := newRefactorDoc()
		r := Check(d, blocks, stdLimits(), true, 0)
		if r.OK() {
			t.Fatal("重构类首节点不是特征测试却通过了：行为保持无从证明")
		}
		found := false
		for _, f := range r.Blocking {
			if f.Code == CodeRefactorFirstNode {
				found = true
			}
		}
		if !found {
			t.Errorf("want code=%s，got=%v", CodeRefactorFirstNode, r.Blocking)
		}
	})

	t.Run("首节点是特征测试任务则放行", func(t *testing.T) {
		d, blocks := newRefactorDoc()
		blocks.Tasks[0].Title = "补特征测试：预览阈值判定"
		if r := Check(d, blocks, stdLimits(), true, 0); !r.OK() {
			t.Errorf("首节点已是特征测试任务，不该被拦：%v", r.Blocking)
		}
	})

	t.Run("重构类不要求七类固定行", func(t *testing.T) {
		d, blocks := newRefactorDoc()
		blocks.Tasks[0].Title = "补特征测试：预览阈值判定"
		for _, f := range Check(d, blocks, stdLimits(), true, 0).Blocking {
			if f.Code == CodeCategoryMissing {
				t.Errorf("重构类只要两类 AC，不该要求七类固定行：%s", f.Message)
			}
		}
	})
}
