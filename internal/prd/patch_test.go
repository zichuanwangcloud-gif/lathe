package prd

import (
	"strings"
	"testing"
)

// 增量补丁的合并语义（patch.go 文件头的六条）。每条都是「整份重写」时代
// 真出过的问题的反面：没给的节不许丢、人的确认不许被冲掉、人的反向确认
// 答复不许被抹、决策不许被改写、删除必须显式。

func baseDoc() *Document {
	yes := true
	return &Document{
		Type:     TypeFeature,
		OneLiner: Section{Status: StatusConfirmed, Body: "让管理员配预览阈值"},
		Problem:  Section{Status: StatusInferred, Body: "阈值写死在 gate.go:31"},
		GoalList: []Goal{{ID: "G-1", Text: "阈值可配", Measure: "改完生效"}},
		ScenarioList: []Scenario{
			{ID: "S-1", Title: "改阈值", Given: "已登录", When: "改 75", Then: "80% 被拒"},
		},
		Criteria: []Criterion{{
			ID: "AC-1", Category: CatHappyPath, Given: "阈值 75", When: "占用 80%", Then: "409",
			ReverseConfirm: "按此 AC，75% 整不会被拦，你接受吗", ReverseAccepted: &yes,
		}},
		Tasks: []TaskBlock{
			{Key: "T1", Title: "阈值改配置项", Kind: "feature", Acceptance: []string{"AC-1"}},
		},
		NonGoals:  []NonGoal{{Text: "不做按仓库配置", Reason: "没有需求"}},
		Decisions: []Decision{{Round: 2, Kind: DecisionOptionChoice, Question: "阈值放哪", Decision: "系统设置"}},
	}
}

func TestApplyKeepsUntouchedSectionsAndConfirmation(t *testing.T) {
	base := baseDoc()
	got, err := base.Apply(&DocPatch{
		Sections: map[string]*Section{"problem": {Status: StatusConfirmed, Body: "阈值写死在 gate.go:31 与 gate.go:58"}},
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got.OneLiner.Status != StatusConfirmed || got.OneLiner.Body != "让管理员配预览阈值" {
		t.Errorf("没给的节必须原样保留（含人的 confirmed），got=%+v", got.OneLiner)
	}
	if got.Problem.Body != "阈值写死在 gate.go:31 与 gate.go:58" {
		t.Errorf("给了的节应整节替换，got=%q", got.Problem.Body)
	}
	if got.Problem.Status != StatusInferred {
		t.Errorf("模型改过的节一律回到 inferred（不许自封 confirmed），got=%q", got.Problem.Status)
	}
	if base.Problem.Body != "阈值写死在 gate.go:31" {
		t.Error("Apply 不许改动 base（上一版快照还要留作对照）")
	}
}

func TestApplyKeepsPendingStatus(t *testing.T) {
	got, err := baseDoc().Apply(&DocPatch{
		Sections: map[string]*Section{"solution": {Status: StatusPending, Body: "两种方案待选", Questions: []string{"Q-1"}}},
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got.Solution.Status != StatusPending {
		t.Errorf("挂着问题的节应保留 pending，got=%q", got.Solution.Status)
	}
}

func TestApplyRejectsUnknownSection(t *testing.T) {
	_, err := baseDoc().Apply(&DocPatch{Sections: map[string]*Section{"background": {Body: "x"}}}, 3)
	if err == nil || !strings.Contains(err.Error(), "background") {
		t.Errorf("未知节 key 应报错并点名，got=%v", err)
	}
}

func TestApplyUpsertsByIDAndPreservesOrder(t *testing.T) {
	got, err := baseDoc().Apply(&DocPatch{
		GoalList: []Goal{
			{ID: "G-2", Text: "非法值被拒", Measure: "返回 400"},
			{ID: "G-1", Text: "阈值可配（内存与磁盘）", Measure: "改完下一次预览生效"},
		},
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.GoalList) != 2 {
		t.Fatalf("应为 2 条目标，got=%+v", got.GoalList)
	}
	if got.GoalList[0].ID != "G-1" || got.GoalList[0].Text != "阈值可配（内存与磁盘）" {
		t.Errorf("同编号应原位整条替换，got[0]=%+v", got.GoalList[0])
	}
	if got.GoalList[1].ID != "G-2" {
		t.Errorf("新编号应追加在后，got[1]=%+v", got.GoalList[1])
	}
}

func TestApplyNoMentionNoRemoval(t *testing.T) {
	got, err := baseDoc().Apply(&DocPatch{Tasks: []TaskBlock{{Key: "T2", Title: "前端", Kind: "feature"}}}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tasks) != 2 || got.Tasks[0].Key != "T1" {
		t.Errorf("没在 remove 清单里的任务不许因为「这轮没提」而消失，got=%+v", got.Tasks)
	}
	if len(got.Criteria) != 1 || len(got.ScenarioList) != 1 {
		t.Error("没提到的 AC / 场景必须原样保留")
	}
}

func TestApplyExplicitRemove(t *testing.T) {
	got, err := baseDoc().Apply(&DocPatch{
		Remove: PatchRemove{Tasks: []string{"T1"}, GoalList: []string{"G-9"}},
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tasks) != 0 {
		t.Errorf("remove 清单里的任务应被删，got=%+v", got.Tasks)
	}
	if len(got.GoalList) != 1 {
		t.Errorf("删不存在的编号应无副作用，got=%+v", got.GoalList)
	}
}

func TestApplyKeepsHumanReverseAnswer(t *testing.T) {
	cases := []struct {
		name    string
		reverse string
		want    bool // 是否沿用人的答复
	}{
		{"反向确认原文没变 → 沿用人的答复", "按此 AC，75% 整不会被拦，你接受吗", true},
		{"反向确认改写了 → 答复作废，要人重新表态", "按此 AC，74.9% 不会被拦，你接受吗", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := baseDoc().Apply(&DocPatch{Criteria: []Criterion{{
				ID: "AC-1", Category: CatHappyPath, Given: "阈值 75", When: "占用 80%", Then: "返回 409",
				ReverseConfirm: tc.reverse,
			}}}, 3)
			if err != nil {
				t.Fatal(err)
			}
			kept := got.Criteria[0].ReverseAccepted != nil
			if kept != tc.want {
				t.Errorf("reverseAccepted 沿用 got=%v want=%v", kept, tc.want)
			}
			if got.Criteria[0].Then != "返回 409" {
				t.Errorf("AC 其余字段应按补丁替换，got then=%q", got.Criteria[0].Then)
			}
		})
	}
}

func TestApplyDecisionsAppendOnly(t *testing.T) {
	got, err := baseDoc().Apply(&DocPatch{Decisions: []Decision{
		{Kind: DecisionOptionChoice, Question: "阈值放哪", Decision: "系统设置"}, // 重复 → 不追加
		{Kind: DecisionBoundary, Question: "75% 整算不算超", Decision: "不算"},
	}}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Decisions) != 2 {
		t.Fatalf("决策只增不改且去重，got=%+v", got.Decisions)
	}
	if got.Decisions[0].Round != 2 {
		t.Error("已有决策不许被改写")
	}
	if got.Decisions[1].Round != 4 {
		t.Errorf("新决策没写轮次时补本轮，got=%d", got.Decisions[1].Round)
	}
}

func TestApplyNonGoalsReplaceWhole(t *testing.T) {
	keep, err := baseDoc().Apply(&DocPatch{}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(keep.NonGoals) != 1 {
		t.Error("没给 nonGoals 时应保留原表")
	}
	empty := []NonGoal{}
	cleared, err := baseDoc().Apply(&DocPatch{NonGoals: &empty}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared.NonGoals) != 0 {
		t.Errorf("给了空数组应整表清空，got=%+v", cleared.NonGoals)
	}
}

func TestApplyOnEmptyBase(t *testing.T) {
	var base *Document
	got, err := base.Apply(&DocPatch{Sections: map[string]*Section{"oneLiner": {Body: "x"}}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.OneLiner.Body != "x" || got.OneLiner.Status != StatusInferred {
		t.Errorf("首轮在空文档上合并，got=%+v", got.OneLiner)
	}
}
