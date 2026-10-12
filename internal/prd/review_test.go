package prd

import (
	"strings"
	"testing"
)

// 对抗复核的测试。两个分支都要测：攻下来了（有发现）与攻不动（空发现），
// 后者不是「测试没意义」而是 §5.5 明确认可的结论 —— 人审 PRD 时要能看到
// 「有人试过攻它，攻不动」。

// 复核 prompt 不许泄露 §5 现状分析与 §6 方案 —— 看了就会被作者的推理
// 带偏，变成复述而不是攻击。这是 §5.5 的核心约束，值得一条专门的测试。
func TestReviewPromptWithholdsAuthorReasoning(t *testing.T) {
	d, _ := okDoc()
	d.CodeAnalysis.Body = "机密现状分析：阈值常量在 gate.go:31，SENTINEL_ANALYSIS"
	d.Solution.Body = "机密方案：改为读 system_settings，SENTINEL_SOLUTION"

	prompt := BuildReviewPrompt(d)

	for _, leaked := range []string{"SENTINEL_ANALYSIS", "SENTINEL_SOLUTION"} {
		if strings.Contains(prompt, leaked) {
			t.Errorf("复核 prompt 泄露了作者的推理（%s）：复核会变成复述作者想法，"+
				"而不是攻击他的标准", leaked)
		}
	}

	// 该喂的必须喂到，否则复核无从下手。
	for _, want := range []string{d.OneLiner.Body, "AC-1", "S-1", "G-1"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("复核 prompt 缺了必需的上下文：%q", want)
		}
	}
}

func TestParseReviewResultFindsHole(t *testing.T) {
	d, _ := okDoc()
	text := `这是我的分析：
	{"findings":[{
		"kind":"malicious_compliance",
		"title":"只把阈值读进来但不参与判定",
		"detail":"实现读取 system_settings.preview_threshold_pct 存进变量，日志里打印它，但 gate 判定仍用写死的 90。AC-1 只验「阈值设为 75 时占用 80% 返回 409」——把 409 硬编码在占用 >75 的分支上即可过，判定逻辑本身没改。",
		"acRefs":["AC-1"],
		"scenarioRefs":["S-1"]
	}],"verdict":"攻下来了：AC-1 只观察 HTTP 状态码，没有观察阈值真的进了判定路径"}`

	got, err := ParseReviewResult(text, d)
	if err != nil {
		t.Fatalf("解析复核报告失败: %v", err)
	}
	if len(got.Findings) != 1 {
		t.Fatalf("应有 1 条发现，got=%d", len(got.Findings))
	}
	if got.Findings[0].Kind != FindingMalicious {
		t.Errorf("kind: got %q, want %s", got.Findings[0].Kind, FindingMalicious)
	}
	if got.Unhandled() != 1 {
		t.Errorf("新报告的发现都还没处置，Unhandled 应为 1，got=%d", got.Unhandled())
	}
	if got.RawText != text {
		t.Error("原文必须一字不改存下来（§5.5 要求报告原样进附录 C）")
	}
}

// 攻不动是合法结论，不是解析失败。
func TestParseReviewResultAcceptsNoFindings(t *testing.T) {
	d, _ := okDoc()
	text := `{"findings":[],"verdict":"攻不动。试过三个方向：只读不判、读了判但忽略非法值、把阈值当字符串比较 —— 都会被 AC-1 的判据（docker ps 无新容器）挡住。"}`

	got, err := ParseReviewResult(text, d)
	if err != nil {
		t.Fatalf("空发现应当是合法结论: %v", err)
	}
	if len(got.Findings) != 0 {
		t.Errorf("应为空发现，got=%d", len(got.Findings))
	}
	if got.Unhandled() != 0 {
		t.Errorf("没有发现就没有待处置，got=%d", got.Unhandled())
	}
	if got.Verdict == "" {
		t.Error("攻不动也要留下 verdict：人要看到它试过哪些方向")
	}
}

// 复核也会编编号，同样不许悬空。
func TestParseReviewResultRejectsDanglingRefs(t *testing.T) {
	d, _ := okDoc()
	tests := []struct {
		name            string
		text            string
		wantErrContains string
	}{
		{
			name:            "引用不存在的 AC",
			text:            `{"findings":[{"kind":"malicious_compliance","title":"洞","acRefs":["AC-99"]}]}`,
			wantErrContains: "AC-99",
		},
		{
			name:            "引用不存在的场景",
			text:            `{"findings":[{"kind":"uncovered_branch","title":"漏了","scenarioRefs":["S-99"]}]}`,
			wantErrContains: "S-99",
		},
		{
			name:            "kind 未知",
			text:            `{"findings":[{"kind":"nitpick","title":"小问题"}]}`,
			wantErrContains: "kind",
		},
		{
			name:            "缺 title",
			text:            `{"findings":[{"kind":"malicious_compliance","detail":"有个洞"}]}`,
			wantErrContains: "title",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseReviewResult(tc.text, d)
			if err == nil {
				t.Fatalf("应当被拒，want err contains %q", tc.wantErrContains)
			}
			if !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Errorf("got %v，want contains %q", err, tc.wantErrContains)
			}
		})
	}
}

// 复核 agent 不许替人填处置 —— 处置是人的责任。
func TestParseReviewResultStripsSelfDisposition(t *testing.T) {
	d, _ := okDoc()
	text := `{"findings":[{"kind":"malicious_compliance","title":"洞",
	          "disposition":"reject","dispositionNote":"我觉得没事"}]}`

	got, err := ParseReviewResult(text, d)
	if err != nil {
		t.Fatal(err)
	}
	if got.Findings[0].Disposition != "" {
		t.Errorf("复核不许自己填处置（那是人的责任），got=%q", got.Findings[0].Disposition)
	}
	if got.Unhandled() != 1 {
		t.Error("剥掉自填处置后，这条发现应仍算未处置")
	}
}

// 报告能落库往返，处置状态不丢。
func TestReviewReportRoundTrip(t *testing.T) {
	r := &ReviewReport{
		Findings: []ReviewFinding{
			{Kind: FindingMalicious, Title: "洞一", Disposition: DispositionFix,
				DispositionNote: "已补 AC-8 观察判定路径"},
			{Kind: FindingUncovered, Title: "洞二"},
		},
		Verdict: "一条已补，一条待议",
		RawText: "原始输出",
	}

	raw, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseReviewReport(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Findings) != 2 {
		t.Fatalf("发现数不对: %d", len(back.Findings))
	}
	if back.Findings[0].Disposition != DispositionFix {
		t.Errorf("处置状态丢了: %q", back.Findings[0].Disposition)
	}
	if back.Unhandled() != 1 {
		t.Errorf("往返后待处置数应为 1，got=%d", back.Unhandled())
	}
	if back.RawText != "原始输出" {
		t.Error("原文往返后丢了")
	}
}

// 空 jsonb 表示还没跑过复核，返回 nil 而不是报错。
func TestParseReviewReportNilOnEmpty(t *testing.T) {
	got, err := ParseReviewReport(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Error("没跑过复核应返回 nil")
	}
	if got.Unhandled() != 0 {
		t.Error("nil 报告的 Unhandled 应为 0（不该 panic）")
	}
}
