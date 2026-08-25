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
//
// 测试要求不是可选项：流水线的验证是红-绿复现证明（docs/02-design.md
// §5.3/§5.4），agent 交出的测试就是红绿判定的唯一依据 —— 没有新增或
// 修改测试文件的 diff，在 heavy 档根本无法给出「修复有效」的证明。
func ImplementPrompt(issueContext string, kind TaskKind, branch string) string {
	var extra string
	switch kind {
	case KindFix, KindHotfix:
		extra = `
这是一个 bug 修复。要求：
- 先定位根因，再动手；不要只处理表象
- **必须先写一个能复现该 bug 的测试**（Go 写在 *_test.go，前端写在 *.test.* / *.spec.*）：
  先跑它确认在现状下失败（bug 复现），再修代码让它通过
- 该测试会随 PR 一起提交，成为仓库的回归测试`
	case KindFeature:
		extra = `
这是一个需求。要求：
- 把 issue 里的验收标准逐条转成测试（Go 写在 *_test.go，前端写在 *.test.* / *.spec.*）
- 这些测试在当前代码上应该失败（功能尚不存在），实现后必须通过
- 若 issue 没有可测试的验收标准，停下来说明缺什么，不要自己猜`
	}

	return fmt.Sprintf(`你在一个自动化编码流水线里实现一个任务。

当前已在分支 %s 上，工作区已就绪。请完成下面这个 issue 的代码改动。

要求：
- 遵守仓库既有的代码风格与架构约定（如有 CLAUDE.md 请先读）
- 改动尽量小而集中，只解决这个 issue
- **不要执行 git commit、git push，也不要开 PR** —— 这些由流水线负责
- 流水线会自动识别你提交的测试并做红-绿验证（改动前必须失败、改动后必须通过）。
  若仓库是 monorepo 子包布局、或测试框架不在仓库根的 package.json / go.mod 体系里，
  自动识别可能够不着 —— 此时必须随改动提交 .lathe/repro.json 显式声明运行方式，
  流水线会原样执行声明的命令：
  {"version":1,"tests":[{"file":"测试文件路径（必须在本次改动里）","cmd":["命令","参数..."],"dir":"工作目录（相对仓库根，可空）"}]}
- 完成后输出交付摘要，固定为以下四个小节（会直接展示在任务详情页）：

## 改了什么
## 为什么这样改
## 涉及的关键文件
## 自验证证据
（跑了什么测试、改动前后的红绿结果）%s

%s`, branch, extra, issueContext)
}

// FixPrompt 生成 §5 修复回路的续跑提示词：验证失败后 resume 原实现
// 会话，把失败步骤的输出喂回去让 agent 就地修复。agent 还记得自己的
// 实现思路，比新会话从零定位省一整轮上下文。
func FixPrompt(attempt, maxAttempts int, f *StepResult, summary string) string {
	loc := f.Step.Dir
	if loc == "" {
		loc = "."
	}
	output := f.Output
	if f.Err != nil {
		output = f.Err.Error() + "\n" + output
	}
	return fmt.Sprintf(`你在一个自动化编码流水线里。刚才的实现没有通过验证（第 %d/%d 轮修复）。

失败步骤：%s
命令：%s（目录 %s）
输出：
%s

完整验证摘要：
%s

要求：
- 先读懂失败输出定位原因，再动手修；改动保持最小，只解决这个失败
- 修实现代码。不要为了让步骤变绿而删测试、放宽断言或注释掉检查 ——
  除非你能论证是测试本身写错了（并在输出里说明论证过程）
- 如果失败原因与本次改动无关（仓库既有问题、环境抖动），明确说出来，
  不要硬改无关代码
- 不要执行 git commit、git push —— 流水线负责
- 完成后输出：修复了什么、为什么之前没考虑到`,
		attempt, maxAttempts, f.Step.Name, strings.Join(f.Step.Cmd, " "), loc,
		truncate(output, 3000), truncate(summary, 1500))
}

// ContinueImplementPrompt 生成断点续跑的实现 prompt（resume 原会话）。
//
// agent 还记得自己的实现思路与已读过的代码，prompt 只需交代「你被中断
// 了，看看现场继续干」，不必重复整份需求 —— 省一整轮上下文正是续跑
// 的意义。测试要求必须重申：中断可能正发生在写测试之前。
func ContinueImplementPrompt() string {
	return `你的上一次执行被中断了（进程崩溃或超时）。工作区里可能留有半成品改动。

请先运行 git status 与 git diff --stat 了解现状，然后继续完成这个 issue 的实现。

要求重申：
- 必须交出能复现问题/验证需求的测试（Go 写在 *_test.go，前端写在 *.test.* / *.spec.*），
  它在改动前的代码上应失败、改动后必须通过
- 不要执行 git commit、git push —— 流水线负责
- 完成后输出交付摘要四小节：## 改了什么 / ## 为什么这样改 / ## 涉及的关键文件 / ## 自验证证据`
}

// ResumeFixPrompt 生成「验证未通过后人工重试」的修复 prompt（resume 原会话）。
//
// 与 FixPrompt 的区别：FixPrompt 携带当次验证的结构化失败步骤；这里是
// 跨进程重试，手头只有落库的失败原因文本。agent 记得自己的实现，失败
// 原因足以让它重新定位；它也可以自己重跑验证命令看现状。
func ResumeFixPrompt(failureReason string) string {
	return fmt.Sprintf(`这个任务的上一轮实现没有通过验证，失败原因：

%s

请修复实现让验证通过。要求：
- 先自己重跑失败的验证命令确认现状（可能已被人工修过一部分），再动手
- 改动保持最小，只解决验证失败；不要删测试、放宽断言来凑绿
- 如果失败原因与本次改动无关（仓库既有问题、环境抖动），明确说出来
- 不要执行 git commit、git push —— 流水线负责
- 完成后输出：修复了什么、为什么之前没通过`, truncate(failureReason, 3000))
}

// ReentryImplementPrompt 生成「会话凭据丢失、在原工作区开新会话」的 prompt。
//
// 新会话没有任何记忆，必须给全量需求；但工作区里可能有上次中断留下的
// 半成品，推倒重来会浪费已有的正确部分，故前置现状说明。
func ReentryImplementPrompt(issueContext string, kind TaskKind, branch string) string {
	return `【续跑场景】这个工作区里可能留有上一次执行中断前的半成品改动（git status / git log 先看清现状）。
在已有成果的基础上继续完成，不要推倒重来；已有改动是错的才改。

` + ImplementPrompt(issueContext, kind, branch)
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
