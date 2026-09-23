package prd

import (
	"strings"
	"testing"
	"time"
)

// 导出渲染的测试。纯字符串，不连库。

func exportFixture() ExportInput {
	approved := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	flowID := int64(42)
	accepted := true
	return ExportInput{
		PRDID:         12,
		Repo:          "acme/lathe",
		Type:          TypeFeature,
		State:         string(StateApproved),
		OriginalInput: "预览阈值写死了，想让管理员能配",
		CreatedAt:     time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC),
		UpdatedAt:     approved,
		ApprovedAt:    &approved,
		FlowID:        &flowID,
		Document: &Document{
			Type:     TypeFeature,
			OneLiner: Section{Status: StatusConfirmed, Body: "让管理员能配预览阈值"},
			Problem:  Section{Status: StatusInferred, Body: "阈值写死在代码里"},
			Risks:    Section{Status: StatusPending, Questions: []string{"Q-1"}},
			GoalList: []Goal{{ID: "G-1", Text: "管理员能改并发上限", Measure: "改完立即生效"}},
			NonGoals: []NonGoal{{Text: "不做按用户限流", Reason: "当前没有多租户诉求"}},
			ScenarioList: []Scenario{
				{ID: "S-1", Title: "管理员调高上限", Given: "已登录", When: "改成 5", Then: "立即生效"},
			},
			Criteria: []Criterion{
				{
					ID: "AC-1", Category: ACCategory("happy_path"),
					Given: "管理员已登录", When: "把并发上限改成 5", Then: "设置页显示 5",
					Evidence: "GET /api/admin/settings 返回 5", Verify: VerifyAuto,
					GoalRefs: []string{"G-1"}, ScenarioRefs: []string{"S-1"}, TaskKey: "T1",
					ReverseConfirm:  "按这份 AC，填 -1 不会被拦，你接受吗？",
					ReverseAccepted: &accepted,
				},
				{ID: "AC-7", Category: ACCategory("i18n"), NA: true, Reason: "纯后端配置，无文案"},
			},
			Questions: []Question{{ID: "Q-1", Text: "上限最大值定多少？"}},
			Decisions: []Decision{
				{Round: 2, Kind: DecisionOptionChoice, Question: "存哪", Decision: "system_settings", Reason: "已有表"},
			},
			Evidences: []Evidence{
				{Ref: "internal/preview/manager.go:42", Note: "阈值写死在这里"},
				{Ref: "internal/preview/manager.go:42", Note: "重复项应被去重"},
			},
			DoD: []string{"迁移已在预发跑过 up/down"},
		},
		Blocks: &TaskBlocks{
			Version: BlocksVersion, Repo: "acme/lathe",
			Tasks: []TaskBlock{
				{
					Key: "T1", Title: "阈值改为可配置", Kind: KindFeature,
					Description: "把常量挪进 system_settings", Acceptance: []string{"AC-1"},
					FilesHint:  []string{"internal/preview/manager.go"},
					VerifyHint: HintHeavy, Estimate: Estimate{Lines: 200, Files: 4},
				},
			},
		},
		Review: &ReviewReport{
			Verdict: "找到一个洞",
			Findings: []ReviewFinding{
				{
					Kind: FindingMalicious, Title: "只改显示不改实际生效也能过",
					Detail: "AC-1 只断言设置页显示 5", ACRefs: []string{"AC-1"},
					Disposition: DispositionFix, DispositionNote: "补一条真实生效的 AC",
				},
			},
		},
		Rounds: []RoundSummary{
			{Round: 1, Stage: string(StageUnderstand), Questions: []string{"要配哪几个阈值？"}, Notes: "先问清范围"},
			{Round: 2, Stage: string(StageExplore), UserInput: "只要并发上限", Notes: "读了 preview 包"},
		},
	}
}

func TestRenderMarkdownCoversTemplate(t *testing.T) {
	md := RenderMarkdown(exportFixture())

	for _, want := range []string{
		"# PRD #12 · 让管理员能配预览阈值",
		"| 目标仓库 | acme/lathe |",
		"| 编排图 | #42 |",
		"预览阈值写死了，想让管理员能配", // 原始需求原样引用
		"## §1 一句话 [已确认]",
		"## §2 问题陈述 [推断]",
		"## §9 风险与未决 [待答]",
		"**G-1** 管理员能改并发上限",
		"达成判据：改完立即生效",
		"不做按用户限流 —— 当前没有多租户诉求",
		"### S-1 管理员调高上限",
		"### AC-1（happy_path）",
		"**回指** G-1 / S-1",
		"**交付任务** T1",
		"（人已接受）",
		"### AC-7（i18n）",
		"不适用：纯后端配置，无文案",
		"### 上线条件（系统附加，非 AC）",
		"### T1 阈值改为可配置",
		"**估算** 约 200 行 / 4 个文件",
		"**Q-1** 上限最大值定多少？",
		"### D1（第 2 轮 · option_choice）",
		"## 附录 A 对话记录",
		"### 第 1 轮（understand）",
		"## 附录 B 代码证据索引",
		"## 附录 C 对抗复核报告",
		"### 发现 1（malicious_compliance）只改显示不改实际生效也能过",
		"**处置** fix —— 补一条真实生效的 AC",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("导出件缺少 %q", want)
		}
	}
}

// 附录 B 要去重：同一个 file:line 在 §2 与 §5 都被引用是常态。
func TestRenderMarkdownDedupesEvidence(t *testing.T) {
	md := RenderMarkdown(exportFixture())
	if got := strings.Count(md, "internal/preview/manager.go:42`"); got != 1 {
		t.Errorf("同一条证据应只出现一次，got=%d 次", got)
	}
}

// 节状态标记必须以文本形式保留（§7）——导出件脱离界面后，读者仍要能
// 看出哪几节是智能体推断、哪几节人拍过板。
func TestSectionStatusLabels(t *testing.T) {
	cases := map[SectionStatus]string{
		StatusConfirmed:   "[已确认]",
		StatusInferred:    "[推断]",
		StatusPending:     "[待答]",
		SectionStatus(""): "[未标记]",
	}
	for status, want := range cases {
		if got := sectionStatusLabel(status); got != want {
			t.Errorf("sectionStatusLabel(%q) got=%q want=%q", status, got, want)
		}
	}
}

// 空 PRD（刚建好、还没跑过一轮）也要能导出，不能 panic。
func TestRenderMarkdownHandlesEmptyPRD(t *testing.T) {
	md := RenderMarkdown(ExportInput{PRDID: 3, OriginalInput: "随便改改", Type: TypeDefect})
	if !strings.Contains(md, "# PRD #3 · 随便改改") {
		t.Errorf("没有一句话时标题应退回原始需求\n%s", md)
	}
	if !strings.Contains(md, "## §1 一句话 [未标记]") {
		t.Error("空 PRD 也应列出全部节")
	}
}

// 「攻不动」是有价值的结论，不能因为 findings 为空就把整节省掉。
func TestRenderMarkdownKeepsEmptyReviewVerdict(t *testing.T) {
	in := exportFixture()
	in.Review = &ReviewReport{Verdict: "攻不动"}
	md := RenderMarkdown(in)

	if !strings.Contains(md, "## 附录 C 对抗复核报告") {
		t.Error("复核没找到问题时仍应有附录 C")
	}
	if !strings.Contains(md, "攻不动") {
		t.Error("结论应出现在导出件里")
	}
}

func TestRenderMarkdownOneLineTitle(t *testing.T) {
	in := exportFixture()
	in.Document.OneLiner.Body = "第一行\n第二行\n\n第三行"
	md := RenderMarkdown(in)

	first := strings.SplitN(md, "\n", 2)[0]
	if strings.Contains(first, "\n") || !strings.Contains(first, "第一行 第二行 第三行") {
		t.Errorf("多行一句话应压成一行标题，got=%q", first)
	}
}

func TestExportFilename(t *testing.T) {
	for _, tc := range []struct{ ext, want string }{
		{"md", "prd-12.md"},
		{"json", "prd-12.json"},
	} {
		if got := ExportFilename(12, tc.ext); got != tc.want {
			t.Errorf("ExportFilename(12, %q) got=%q want=%q", tc.ext, got, tc.want)
		}
	}
}
