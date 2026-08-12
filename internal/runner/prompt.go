package runner

import (
	"encoding/json"
	"fmt"
	"strings"
)

// TriageVerdict 是分诊结论。
type TriageVerdict struct {
	// Actionable 为假表示单子不够明确，应转 blocked_spec 回帖提问。
	Actionable bool
	// Kind 是判定出的任务类型。
	Kind TaskKind
	// Reason 在 Actionable 为假时说明缺什么。
	Reason string
	// Question 是要回帖给提单人的问题。
	Question string
}

// TriagePrompt 生成分诊用的 prompt。
//
// 分诊只读不写：它的唯一职责是判断这单说清楚了没、属于什么类型，
// 绝不能开始改代码 —— 改代码是 implementing 阶段的事。
func TriagePrompt(issueContext string) string {
	return fmt.Sprintf(`你在为一个自动化编码流水线做任务分诊。**只做判断，不要修改任何文件。**

下面是一个 issue。请判断：

1. 这个 issue 是否足够明确，能让人在不追问的情况下动手实现？
   - bug 类需要有可复现的线索（现象、触发路径、期望行为至少占两样）
   - 需求类需要有可验收的标准
   - 只有一句话现象描述、没有任何定位线索的，算不明确
2. 它属于哪种任务类型：fix（修 bug）、feature（做需求）、hotfix（线上紧急故障）

%s

请只输出一个 JSON 对象，不要有其他内容：

{
  "actionable": true 或 false,
  "kind": "fix" 或 "feature" 或 "hotfix",
  "reason": "判断理由，一句话",
  "question": "若 actionable 为 false，这里写要回帖问提单人的具体问题；否则为空字符串"
}`, issueContext)
}

// ImplementPrompt 生成实现阶段的 prompt。
func ImplementPrompt(issueContext string, kind TaskKind, branch string) string {
	var extra string
	if kind == KindFix {
		extra = `
这是一个 bug 修复。请优先做到：
- 先定位根因，再动手；不要只处理表象
- 尽可能补一个能覆盖此 bug 的测试`
	}

	return fmt.Sprintf(`你在一个自动化编码流水线里实现一个任务。

当前已在分支 %s 上，工作区已就绪。请完成下面这个 issue 的代码改动。

要求：
- 遵守仓库既有的代码风格与架构约定（如有 CLAUDE.md 请先读）
- 改动尽量小而集中，只解决这个 issue
- **不要执行 git commit、git push，也不要开 PR** —— 这些由流水线负责
- 完成后用一段话说明你改了什么、为什么这样改%s

%s`, branch, extra, issueContext)
}

// ReviewPrompt 生成 review 二轮的 prompt。
func ReviewPrompt(comments []string) string {
	var b strings.Builder
	b.WriteString(`你之前实现的改动收到了 review 意见。请逐条处理。

要求：
- 逐条回应：采纳的就改，不采纳的说明理由
- **不要执行 git commit、git push** —— 由流水线负责
- 完成后用一段话说明你这轮改了什么

## Review 意见

`)
	for i, c := range comments {
		fmt.Fprintf(&b, "%d. %s\n\n", i+1, strings.TrimSpace(c))
	}
	return b.String()
}

// ParseTriageVerdict 从 agent 输出里解析分诊结论。
//
// 容忍模型在 JSON 前后附带说明文字：截取第一个 { 到最后一个 } 之间的内容。
func ParseTriageVerdict(output string) (*TriageVerdict, error) {
	start := strings.Index(output, "{")
	end := strings.LastIndex(output, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("runner: 分诊输出里找不到 JSON 对象: %s", truncate(output, 300))
	}

	var raw struct {
		Actionable bool   `json:"actionable"`
		Kind       string `json:"kind"`
		Reason     string `json:"reason"`
		Question   string `json:"question"`
	}
	if err := json.Unmarshal([]byte(output[start:end+1]), &raw); err != nil {
		return nil, fmt.Errorf("runner: 解析分诊 JSON 失败: %w（原文: %s）", err, truncate(output, 300))
	}

	kind := TaskKind(strings.ToLower(strings.TrimSpace(raw.Kind)))
	if !kind.Valid() {
		// 类型判断失败不应让整单失败：默认按 fix 处理，代价可控
		kind = KindFix
	}

	v := &TriageVerdict{
		Actionable: raw.Actionable,
		Kind:       kind,
		Reason:     strings.TrimSpace(raw.Reason),
		Question:   strings.TrimSpace(raw.Question),
	}
	// 判定不可执行却没给出问题时，兜底一句，避免回帖空白
	if !v.Actionable && v.Question == "" {
		v.Question = "这个 issue 目前信息不足以自动实现，请补充复现步骤或验收标准。"
		if v.Reason != "" {
			v.Question = v.Reason + "\n\n请补充复现步骤或验收标准。"
		}
	}
	return v, nil
}
