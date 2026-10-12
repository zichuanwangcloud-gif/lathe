package prd

import (
	"fmt"
	"strings"
	"time"
)

// 本文件是 §7 导出：把一份 PRD 渲染成可回写进目标仓库 docs/ 的 Markdown，
// 或结构化 JSON 供外部工具与下一份 PRD 引用。
//
// 两种导出在任何状态下都可用（approved 之后内容本就冻结，导出自然也冻结）。
// Lathe 不自动 push 导出结果 —— 要不要提交进仓库是人的决定。
//
// 节状态标记以文本形式保留（`[已确认]` 等）而不是丢掉：一份导出的 PRD
// 脱离界面之后，读者仍要能看出哪几节是智能体推断、哪几节人拍过板。

// ExportInput 是渲染一份 PRD 所需的全部材料。
//
// 刻意不直接吃 store.PRDRow：轮次里的 questions 是 jsonb，解析它是调用方
// 的事；本文件只管排版，不碰序列化格式。这样渲染逻辑能纯内存单测。
type ExportInput struct {
	PRDID         int64      `json:"prdId"`
	Repo          string     `json:"repo"`
	Type          Type       `json:"type"`
	State         string     `json:"state"`
	OriginalInput string     `json:"originalInput"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	ApprovedAt    *time.Time `json:"approvedAt,omitempty"`
	ConvertedAt   *time.Time `json:"convertedAt,omitempty"`
	RefPRDID      *int64     `json:"refPrdId,omitempty"`
	FlowID        *int64     `json:"generatedFlowId,omitempty"`

	Document *Document      `json:"document,omitempty"`
	Blocks   *TaskBlocks    `json:"taskBlocks,omitempty"`
	Review   *ReviewReport  `json:"reviewReport,omitempty"`
	Rounds   []RoundSummary `json:"rounds,omitempty"`
}

// RoundSummary 是附录 A 要的一轮对话记录。
type RoundSummary struct {
	Round     int       `json:"round"`
	Stage     string    `json:"stage"`
	UserInput string    `json:"userInput,omitempty"`
	Questions []string  `json:"questions,omitempty"`
	Notes     string    `json:"notes,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// sectionStatusLabel 把节标记转成导出用的文本形式。
func sectionStatusLabel(s SectionStatus) string {
	switch s {
	case StatusConfirmed:
		return "[已确认]"
	case StatusInferred:
		return "[推断]"
	case StatusPending:
		return "[待答]"
	default:
		return "[未标记]"
	}
}

// RenderMarkdown 把一份 PRD 渲染成 Markdown。
func RenderMarkdown(in ExportInput) string {
	var sb strings.Builder
	doc := in.Document
	if doc == nil {
		doc = &Document{}
	}

	title := strings.TrimSpace(doc.OneLiner.Body)
	if title == "" {
		title = strings.TrimSpace(in.OriginalInput)
	}
	fmt.Fprintf(&sb, "# PRD #%d · %s\n\n", in.PRDID, oneLine(title))

	renderMeta(&sb, in)
	renderOriginalInput(&sb, in.OriginalInput)

	// 十节按模板顺序。结构化内容紧跟在对应节正文之后，人读导出件时
	// 不用在文末来回翻。
	for _, s := range doc.Sections() {
		fmt.Fprintf(&sb, "## %s %s\n\n", s.Title, sectionStatusLabel(s.Section.Status))
		if body := strings.TrimSpace(s.Section.Body); body != "" {
			sb.WriteString(body)
			sb.WriteString("\n\n")
		}
		if len(s.Section.Questions) > 0 {
			fmt.Fprintf(&sb, "> 本节挂着未决问题：%s\n\n", strings.Join(s.Section.Questions, "、"))
		}
		renderSectionExtras(&sb, s.Key, doc, in.Blocks)
	}

	renderAppendixA(&sb, in.Rounds)
	renderAppendixB(&sb, doc.Evidences)
	renderAppendixC(&sb, in.Review)

	return sb.String()
}

func renderMeta(sb *strings.Builder, in ExportInput) {
	sb.WriteString("| 项 | 值 |\n|---|---|\n")
	fmt.Fprintf(sb, "| 编号 | #%d |\n", in.PRDID)
	fmt.Fprintf(sb, "| 类型 | %s |\n", in.Type)
	fmt.Fprintf(sb, "| 状态 | %s |\n", in.State)
	if in.Repo != "" {
		fmt.Fprintf(sb, "| 目标仓库 | %s |\n", in.Repo)
	}
	fmt.Fprintf(sb, "| 创建 | %s |\n", in.CreatedAt.Format(time.DateOnly))
	fmt.Fprintf(sb, "| 最后更新 | %s |\n", in.UpdatedAt.Format(time.DateOnly))
	if in.ApprovedAt != nil {
		fmt.Fprintf(sb, "| 批准 | %s |\n", in.ApprovedAt.Format(time.DateOnly))
	}
	if in.ConvertedAt != nil {
		fmt.Fprintf(sb, "| 生成任务 | %s |\n", in.ConvertedAt.Format(time.DateOnly))
	}
	if in.FlowID != nil {
		fmt.Fprintf(sb, "| 编排图 | #%d |\n", *in.FlowID)
	}
	if in.RefPRDID != nil {
		fmt.Fprintf(sb, "| 接续自 | PRD #%d |\n", *in.RefPRDID)
	}
	sb.WriteString("\n")
}

func renderOriginalInput(sb *strings.Builder, input string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return
	}
	// 原始需求不可编辑（它是这份 PRD 的出发点），导出时原样引用 ——
	// 读者要能对比「最初说的」与「最后写成的」。
	sb.WriteString("> **原始需求**（不可编辑）\n>\n")
	for _, line := range strings.Split(input, "\n") {
		fmt.Fprintf(sb, "> %s\n", line)
	}
	sb.WriteString("\n")
}

// renderSectionExtras 渲染某一节对应的结构化内容。
func renderSectionExtras(sb *strings.Builder, key string, doc *Document, blocks *TaskBlocks) {
	switch key {
	case "goals":
		renderGoals(sb, doc)
	case "scenarios":
		renderScenarios(sb, doc.ScenarioList)
	case "acceptance":
		renderCriteria(sb, doc)
	case "taskSplit":
		renderTaskBlocks(sb, doc, blocks)
	case "risks":
		renderQuestions(sb, doc.Questions)
	case "decisionLogSect":
		renderDecisions(sb, doc.Decisions)
	}
}

func renderGoals(sb *strings.Builder, doc *Document) {
	if len(doc.GoalList) > 0 {
		sb.WriteString("### 目标\n\n")
		for _, g := range doc.GoalList {
			fmt.Fprintf(sb, "- **%s** %s\n", g.ID, g.Text)
			if g.Measure != "" {
				fmt.Fprintf(sb, "  - 达成判据：%s\n", g.Measure)
			}
		}
		sb.WriteString("\n")
	}
	if len(doc.NonGoals) > 0 {
		sb.WriteString("### 非目标\n\n")
		for _, n := range doc.NonGoals {
			fmt.Fprintf(sb, "- %s", n.Text)
			if n.Reason != "" {
				fmt.Fprintf(sb, " —— %s", n.Reason)
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
}

func renderScenarios(sb *strings.Builder, list []Scenario) {
	for _, s := range list {
		fmt.Fprintf(sb, "### %s %s\n\n", s.ID, s.Title)
		if s.Given != "" {
			fmt.Fprintf(sb, "- **Given** %s\n", s.Given)
		}
		if s.When != "" {
			fmt.Fprintf(sb, "- **When** %s\n", s.When)
		}
		if s.Then != "" {
			fmt.Fprintf(sb, "- **Then** %s\n", s.Then)
		}
		if s.ExistingTest != "" {
			fmt.Fprintf(sb, "- **守着它的现有测试** `%s`\n", s.ExistingTest)
		}
		sb.WriteString("\n")
	}
}

func renderCriteria(sb *strings.Builder, doc *Document) {
	for i := range doc.Criteria {
		c := &doc.Criteria[i]
		fmt.Fprintf(sb, "### %s（%s）\n\n", c.ID, c.Category)
		if c.NA {
			fmt.Fprintf(sb, "不适用：%s\n\n", c.Reason)
			continue
		}
		renderCriterion(sb, c)

		if len(c.GoalRefs) > 0 || len(c.ScenarioRefs) > 0 {
			refs := append(append([]string{}, c.GoalRefs...), c.ScenarioRefs...)
			fmt.Fprintf(sb, "- **回指** %s\n", strings.Join(refs, " / "))
		}
		if c.TaskKey != "" {
			fmt.Fprintf(sb, "- **交付任务** %s\n", c.TaskKey)
		}
		if c.ReverseConfirm != "" {
			fmt.Fprintf(sb, "- **反向确认** %s", c.ReverseConfirm)
			switch {
			case c.ReverseAccepted == nil:
				sb.WriteString("（人还没答）")
			case *c.ReverseAccepted:
				sb.WriteString("（人已接受）")
			default:
				sb.WriteString("（人不接受）")
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	if len(doc.DoD) > 0 {
		// 上线条件与 AC 分开列（§5.6）：过了 AC ≠ 能上线。
		sb.WriteString("### 上线条件（系统附加，非 AC）\n\n")
		for _, d := range doc.DoD {
			fmt.Fprintf(sb, "- %s\n", d)
		}
		sb.WriteString("\n")
	}
}

func renderTaskBlocks(sb *strings.Builder, doc *Document, blocks *TaskBlocks) {
	tasks := doc.Tasks
	if blocks != nil && len(blocks.Tasks) > 0 {
		tasks = blocks.Tasks
	}
	for i := range tasks {
		b := &tasks[i]
		fmt.Fprintf(sb, "### %s %s\n\n", b.Key, b.Title)
		fmt.Fprintf(sb, "- **类型** %s\n", b.Kind)
		if b.DependsOn != "" {
			fmt.Fprintf(sb, "- **依赖** %s\n", b.DependsOn)
		} else {
			sb.WriteString("- **依赖** （无，可直接开工）\n")
		}
		fmt.Fprintf(sb, "- **估算** 约 %d 行 / %d 个文件\n", b.Estimate.Lines, b.Estimate.Files)
		if b.VerifyHint != "" {
			fmt.Fprintf(sb, "- **验证档位提示** %s（真实定档在 diff 产出后）\n", b.VerifyHint)
		}
		if len(b.Acceptance) > 0 {
			fmt.Fprintf(sb, "- **交付 AC** %s\n", strings.Join(b.Acceptance, " / "))
		}
		if len(b.FilesHint) > 0 {
			fmt.Fprintf(sb, "- **参考文件** `%s`\n", strings.Join(b.FilesHint, "`、`"))
		}
		sb.WriteString("\n")
		if d := strings.TrimSpace(b.Description); d != "" {
			sb.WriteString(d)
			sb.WriteString("\n\n")
		}
	}
}

func renderQuestions(sb *strings.Builder, qs []Question) {
	if len(qs) == 0 {
		return
	}
	sb.WriteString("### 未决问题\n\n")
	for _, q := range qs {
		fmt.Fprintf(sb, "- **%s** %s\n", q.ID, q.Text)
	}
	sb.WriteString("\n")
}

func renderDecisions(sb *strings.Builder, ds []Decision) {
	if len(ds) == 0 {
		return
	}
	for i, d := range ds {
		renderDecision(sb, i, d)
	}
}

// renderDecision 渲染一条决策记录。
//
// Supersedes 指向被推翻的条目：人改主意是允许的，但走「修订」而不是被
// 重新问一遍（§1.3 纪律 3）。导出时要看得见这条链，否则读者会以为两条
// 矛盾的决策同时有效。
func renderDecision(sb *strings.Builder, idx int, d Decision) {
	fmt.Fprintf(sb, "### D%d（第 %d 轮 · %s）\n\n", idx+1, d.Round, d.Kind)
	if d.Question != "" {
		fmt.Fprintf(sb, "- **问的是** %s\n", d.Question)
	}
	if d.Decision != "" {
		fmt.Fprintf(sb, "- **定的是** %s\n", d.Decision)
	}
	if d.Reason != "" {
		fmt.Fprintf(sb, "- **为什么** %s\n", d.Reason)
	}
	if d.Supersedes != nil {
		fmt.Fprintf(sb, "- **推翻了** 第 D%d 条\n", *d.Supersedes+1)
	}
	sb.WriteString("\n")
}

func renderAppendixA(sb *strings.Builder, rounds []RoundSummary) {
	if len(rounds) == 0 {
		return
	}
	sb.WriteString("## 附录 A 对话记录\n\n")
	for _, r := range rounds {
		fmt.Fprintf(sb, "### 第 %d 轮（%s）\n\n", r.Round, r.Stage)
		if r.UserInput != "" {
			sb.WriteString("**人的回答**\n\n")
			for _, line := range strings.Split(strings.TrimSpace(r.UserInput), "\n") {
				fmt.Fprintf(sb, "> %s\n", line)
			}
			sb.WriteString("\n")
		}
		if len(r.Questions) > 0 {
			sb.WriteString("**本轮提出的问题**\n\n")
			for _, q := range r.Questions {
				fmt.Fprintf(sb, "- %s\n", q)
			}
			sb.WriteString("\n")
		}
		if r.Notes != "" {
			fmt.Fprintf(sb, "**本轮摘要** %s\n\n", r.Notes)
		}
	}
}

func renderAppendixB(sb *strings.Builder, ev []Evidence) {
	if len(ev) == 0 {
		return
	}
	// 附录 B 由全部 Evidence 去重汇总（§2 / §5 的 file:line 证据）。
	sb.WriteString("## 附录 B 代码证据索引\n\n")
	seen := make(map[string]bool, len(ev))
	for _, e := range ev {
		if e.Ref == "" || seen[e.Ref] {
			continue
		}
		seen[e.Ref] = true
		fmt.Fprintf(sb, "- `%s`", e.Ref)
		if e.Note != "" {
			fmt.Fprintf(sb, " —— %s", e.Note)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
}

func renderAppendixC(sb *strings.Builder, rep *ReviewReport) {
	if rep == nil {
		return
	}
	sb.WriteString("## 附录 C 对抗复核报告\n\n")
	if rep.Verdict != "" {
		fmt.Fprintf(sb, "**结论** %s\n\n", rep.Verdict)
	}
	if len(rep.Findings) == 0 {
		// 「攻不动」本身是有价值的结论，不能因为列表空就整节省掉。
		sb.WriteString("复核没有找到可乘之隙 —— 有人试过攻这组 AC，攻不动。\n\n")
	}
	for i := range rep.Findings {
		f := &rep.Findings[i]
		fmt.Fprintf(sb, "### 发现 %d（%s）%s\n\n", i+1, f.Kind, f.Title)
		if f.Detail != "" {
			sb.WriteString(f.Detail)
			sb.WriteString("\n\n")
		}
		if len(f.ACRefs) > 0 {
			fmt.Fprintf(sb, "- **相关 AC** %s\n", strings.Join(f.ACRefs, " / "))
		}
		if len(f.ScenarioRefs) > 0 {
			fmt.Fprintf(sb, "- **相关场景** %s\n", strings.Join(f.ScenarioRefs, " / "))
		}
		if f.Disposition == "" {
			sb.WriteString("- **处置** 尚未处置\n")
		} else {
			fmt.Fprintf(sb, "- **处置** %s", f.Disposition)
			if f.DispositionNote != "" {
				fmt.Fprintf(sb, " —— %s", f.DispositionNote)
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
}

// oneLine 把多行标题压成一行，避免撑坏 Markdown 标题。
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.Join(strings.Fields(s), " ")
}

// ExportFilename 是导出件的建议文件名。
//
// 只用编号不用标题：标题含空格、斜杠与中文标点，拼进文件名要额外转义，
// 而这个名字会直接进 Content-Disposition。
func ExportFilename(prdID int64, ext string) string {
	return fmt.Sprintf("prd-%d.%s", prdID, ext)
}
