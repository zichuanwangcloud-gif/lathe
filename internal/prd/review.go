package prd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// 本文件是对抗复核（docs/10 §5.5）—— 验收标准自己的「红阶段」。
//
// 红-绿的逻辑：一个不能先失败的测试无权声称验证了什么。同理：**一组任何
// 实现都能过的 AC，无权声称覆盖了意图。** 所以定稿前必须有人试着攻它。
//
// 三条设计要点，每条都有代价与理由：
//
//  1. **独立上下文、全新 SessionID。** 复核 agent 只看 §1–§4 与 §7，
//     **不看 §5 现状分析与 §6 方案** —— 看了就会被作者的推理带偏，
//     变成「复述作者想法」而不是「攻击作者的标准」。代价是它可能提出
//     方案里已排除的攻击，由人在处置时驳回。
//  2. **必跑，不按类型跳过**（D10-6）。它是 AC 唯一的红阶段，跳过它
//     等于把红-绿验证里的「红」删掉。代价是每份 PRD 多一次 agent 调用。
//  3. **走强通道。** 要「想得出恶意实现」，便宜通道不够用（docs/10 §9 已决）。

// ReviewFinding 是复核的一条发现。
type ReviewFinding struct {
	// Kind 区分两类发现，它们的严重程度不同：
	// malicious_compliance 是 AC 有洞（能过全部 AC 却违背意图）；
	// uncovered_branch 是场景里有分支没 AC 覆盖。
	Kind string `json:"kind"`
	// Title 是一句话概括。
	Title string `json:"title"`
	// Detail 是具体构造：恶意实现长什么样，或哪个分支没被覆盖。
	Detail string `json:"detail"`
	// ACRefs 是相关的 AC 编号（可空 —— 「缺一条 AC」本身就没有编号可指）。
	ACRefs []string `json:"acRefs,omitempty"`
	// ScenarioRefs 是相关场景编号。
	ScenarioRefs []string `json:"scenarioRefs,omitempty"`

	// Disposition 是人的处置，由 UI 填写，agent 不产出。
	// 空表示还没处置 —— 自检清单会拦住（CodeReviewUnhandled）。
	Disposition string `json:"disposition,omitempty"`
	// DispositionNote 是处置理由。
	DispositionNote string `json:"dispositionNote,omitempty"`
}

// 发现的两类。
const (
	// FindingMalicious 恶意合规：构造出了能过全部 AC 却违背意图的实现。
	// #492「只写测试没实现」是这个失效模式的真实案例。
	FindingMalicious = "malicious_compliance"
	// FindingUncovered 场景分支未覆盖。
	FindingUncovered = "uncovered_branch"
)

// 处置取值。
const (
	// DispositionFix 接受，转 ❓ 补 AC。
	DispositionFix = "fix"
	// DispositionReject 驳回（复核看错了，或方案里已排除）。
	DispositionReject = "reject"
	// DispositionAccept 认可风险但不改（要写理由）。
	DispositionAccept = "accept"
)

// ReviewReport 是一次对抗复核的完整报告，原样进附录 C。
type ReviewReport struct {
	// Findings 是全部发现。空数组表示攻不动 —— 这本身是有价值的结论，
	// 人审 PRD 时能看到「有人试过攻它，攻不动」。
	Findings []ReviewFinding `json:"findings"`
	// Verdict 是复核 agent 的总结。
	Verdict string `json:"verdict"`
	// RawText 是 agent 的原始输出，一字不改地存下来。
	//
	// 为什么留原文：docs/10 §5.5 要求「报告原样进附录 C」。解析后的结构
	// 化字段可能丢掉模型表述里的细节，而人在审「攻不动」这个结论时，
	// 需要能看到它到底试了什么。
	RawText string `json:"rawText,omitempty"`
}

// Unhandled 返回还没标处置的发现条数。
func (r *ReviewReport) Unhandled() int {
	if r == nil {
		return 0
	}
	n := 0
	for _, f := range r.Findings {
		if strings.TrimSpace(f.Disposition) == "" {
			n++
		}
	}
	return n
}

// Marshal 序列化为 jsonb 字节。
func (r *ReviewReport) Marshal() (json.RawMessage, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// ParseReviewReport 从库里的 jsonb 还原。空输入返回 nil（还没跑过复核）。
func ParseReviewReport(raw json.RawMessage) (*ReviewReport, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var r ReviewReport
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// BuildReviewPrompt 拼对抗复核的 prompt。
//
// 只喂 §1–§4 与 §7 —— 调用方不要把 §5/§6 塞进来，那会让复核变成复述。
func BuildReviewPrompt(d *Document) string {
	var b strings.Builder

	b.WriteString(`你是一名对抗性审查者。有人写了一份 PRD，你的工作是**攻击它的验收标准**。

你看不到作者的现状分析与方案 —— 这是刻意的。你只看意图（§1–§4）与
验收标准（§7）。你要回答的问题是：

  **这组验收标准，够不够拦住一个「照着它做但违背意图」的实现？**

红-绿验证的逻辑：一个不能先失败的测试无权声称验证了什么。
同理：一组任何实现都能过的验收标准，无权声称覆盖了意图。

你有两个任务：

**任务 1（主要）：构造恶意合规的实现。**
设计一个实现，它能通过下面每一条验收标准，但任何看到它的人都会说
「这不是我要的东西」。构造得越具体越好：说清它怎么过每一条 AC，
以及它在哪里背离了意图。

真实案例参考：曾有一个任务，agent 只写了测试没写实现，测试全绿 ——
因为验收标准只说了「测试通过」，没说「功能可用」。这类洞就是你要找的。

如果你**确实构造不出来**，明确说出来，并说明你试过哪些方向。
「攻不动」是一个有价值的结论，不要为了交差而编一个牵强的攻击。

**任务 2：找没被覆盖的分支。**
逐个看 §4 的场景，列出其中哪些分支、哪些边界、哪些失败路径没有任何
AC 在验。

`)

	b.WriteString("---\n\n## §1 一句话\n\n")
	b.WriteString(d.OneLiner.Body)
	b.WriteString("\n\n## §2 问题陈述\n\n")
	b.WriteString(d.Problem.Body)
	b.WriteString("\n\n## §3 目标与非目标\n\n")
	b.WriteString(d.Goals.Body)
	b.WriteString("\n")
	for _, g := range d.GoalList {
		fmt.Fprintf(&b, "- %s %s（达成判据：%s）\n", g.ID, g.Text, g.Measure)
	}
	for _, ng := range d.NonGoals {
		fmt.Fprintf(&b, "- 非目标：%s（理由：%s）\n", ng.Text, ng.Reason)
	}

	b.WriteString("\n## §4 场景\n\n")
	b.WriteString(d.Scenarios.Body)
	b.WriteString("\n")
	for _, s := range d.ScenarioList {
		fmt.Fprintf(&b, "- %s %s：Given %s / When %s / Then %s\n",
			s.ID, s.Title, s.Given, s.When, s.Then)
	}

	b.WriteString("\n## §7 验收标准\n\n")
	for _, c := range d.Criteria {
		if c.NA {
			fmt.Fprintf(&b, "- %s [%s] N/A：%s\n", c.ID, c.Category, c.Reason)
			continue
		}
		fmt.Fprintf(&b, "- %s [%s] Given %s / When %s / Then %s\n  判据：%s；验证方式：%s",
			c.ID, c.Category, c.Given, c.When, c.Then, c.Evidence, c.Verify)
		if c.ManualSteps != "" {
			fmt.Fprintf(&b, "；走查步骤：%s", c.ManualSteps)
		}
		b.WriteString("\n")
	}

	b.WriteString(`
---

## 输出格式（严格遵守）

只输出一个 JSON 对象，不要前后解释，不要 markdown 代码围栏。

{
  "findings": [
    {
      "kind": "malicious_compliance",
      "title": "一句话说清这个洞",
      "detail": "具体构造：这个实现长什么样，它怎么过 AC-1/AC-2，它在哪里背离意图",
      "acRefs": ["AC-1"],
      "scenarioRefs": ["S-1"]
    },
    {
      "kind": "uncovered_branch",
      "title": "S-2 的失败分支没有任何 AC",
      "detail": "具体哪个分支，出问题会怎样",
      "scenarioRefs": ["S-2"]
    }
  ],
  "verdict": "总结：你攻下来了还是攻不动；攻不动的话你试过哪些方向"
}

findings 可以是空数组（攻不动）。不要为了交差编牵强的攻击 ——
一条站不住的发现会浪费人的一次判断，比没有发现更糟。
`)

	return b.String()
}

// rawReview 是复核 agent 输出的线格式。
type rawReview struct {
	Findings []ReviewFinding `json:"findings"`
	Verdict  string          `json:"verdict"`
}

// ParseReviewResult 解析复核 agent 的输出。
//
// 同 ParseRoundResult：截 JSON → 反序列化 → 逐字段校验。
// 额外保留原文进 RawText（docs/10 §5.5 要求报告原样进附录 C）。
func ParseReviewResult(text string, d *Document) (*ReviewReport, error) {
	payload, err := extractJSON(text)
	if err != nil {
		return nil, err
	}
	var raw rawReview
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return nil, fmt.Errorf("prd: 复核输出不是合法 JSON: %w", err)
	}

	// 校验引用：复核也会编编号，同样不许悬空。
	acs := map[string]bool{}
	for _, c := range d.Criteria {
		acs[c.ID] = true
	}
	scenarios := map[string]bool{}
	for _, s := range d.ScenarioList {
		scenarios[s.ID] = true
	}

	for i := range raw.Findings {
		f := &raw.Findings[i]
		switch f.Kind {
		case FindingMalicious, FindingUncovered:
		case "":
			return nil, fmt.Errorf("prd: 第 %d 条复核发现缺 kind", i+1)
		default:
			return nil, fmt.Errorf("prd: 第 %d 条复核发现的 kind %q 未知", i+1, f.Kind)
		}
		if strings.TrimSpace(f.Title) == "" {
			return nil, fmt.Errorf("prd: 第 %d 条复核发现缺 title", i+1)
		}
		for _, ref := range f.ACRefs {
			if !acs[ref] {
				return nil, fmt.Errorf("prd: 复核发现「%s」引用了不存在的 %s", f.Title, ref)
			}
		}
		for _, ref := range f.ScenarioRefs {
			if !scenarios[ref] {
				return nil, fmt.Errorf("prd: 复核发现「%s」引用了不存在的场景 %s", f.Title, ref)
			}
		}
		// 复核 agent 不许替人填处置 —— 处置是人的责任（docs/10 §5.5）。
		f.Disposition = ""
		f.DispositionNote = ""
	}

	if raw.Findings == nil {
		raw.Findings = []ReviewFinding{}
	}
	return &ReviewReport{Findings: raw.Findings, Verdict: raw.Verdict, RawText: text}, nil
}

// ValidateDisposition 校验人填的一条处置。
//
// 放在 prd 包而不是 HTTP 层：「accept 必须写理由」是产品规则（docs/10
// §5.5 —— 认可风险但不改的，要留下认可的依据），不是请求格式校验。
// 换个入口（CLI、批量导入）进来同样得守。
func ValidateDisposition(disposition, note string) error {
	switch disposition {
	case DispositionFix, DispositionReject:
		return nil
	case DispositionAccept:
		if strings.TrimSpace(note) == "" {
			return errors.New("prd: 认可风险（accept）必须写明理由")
		}
		return nil
	case "":
		return errors.New("prd: 处置不能为空")
	default:
		return fmt.Errorf("prd: 未知的处置 %q（只能是 fix / reject / accept）", disposition)
	}
}
