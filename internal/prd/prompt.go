package prd

import (
	"fmt"
	"strings"
)

// 本文件拼规划智能体的 prompt，形状照搬 internal/preview/recommend.go 的
// 三段式（那是本仓唯一的只读 agent 先例）：
//
//  1. 只读声明 —— 明确告诉模型它不许改任何文件；
//  2. 机械证据 —— 由 Lathe 算好喂进去（原文、当前快照、历史决策、
//     人的回答、仓库阈值），不靠模型即兴发现；
//  3. 严格输出模板 —— 一段 JSON，字段逐条校验，拒绝幻觉。
//
// 为什么每轮重拼历史而不用 --resume：worktree 每轮用完即回收（D10-8），
// resume 的会话会以为自己还在一个已经被删掉的目录里。代价是 prompt 变长、
// token 略增，换来的是「会话状态不依赖磁盘现场」。（同一轮之内的输出修正
// 是例外：检出还在，见 service.go 的 repairPrompt。）
//
// 输出是增量补丁（patch.go），不是完整快照：输入侧把完整快照渲染给模型
// 看（输入便宜、快），输出侧只要它交改动（输出贵、慢、每多写一字就多一次
// 写坏编号的机会）。

// RoundInput 是拼一轮 prompt 所需的全部上下文。
type RoundInput struct {
	// OriginalInput 是模糊任务原文，所有推断的根（docs/10 §0：不可编辑）。
	OriginalInput string
	// Type 是当前判定的 PRD 类型，阶段 A 之后才有。
	Type Type
	// Stage 是本轮要推进到哪个阶段。
	Stage Stage
	// Round 是本轮轮次（从 1 起）。
	Round int
	// Document 是上一轮交付的快照，首轮为 nil。
	Document *Document
	// UserAnswer 是人对上一轮问题的回答，首轮为空。
	UserAnswer string
	// Decisions 是 §10 已拍板的决策 —— 喂进去是为了「不重问已定的事」
	// （docs/10 §1.3 纪律 3）。
	Decisions []Decision
	// Repo 是目标仓库 owner/repo，用于让模型知道自己在读什么。
	Repo string
	// Limits 是仓库阈值，拆分阶段要据它自检任务量级。
	Limits Limits
}

// maxQuestionsPerRound 是单轮问题数的硬上限（D10-7）。
//
// 为什么卡 3：问题轰炸的结果是人不答。这不是礼貌问题 —— 一轮问 10 个
// 问题，人大概率只答前 2 个，剩下 8 个变成沉默的 ❓，最后卡在定稿门口。
const maxQuestionsPerRound = 3

// QuestionBudget 是某阶段一轮最多能问几个问题。
//
// 只有阶段 A 用满 3 个：那时智能体还没读代码，最需要人帮它定方向。
// 读过代码之后（B 起），大多数问题它都能自己给出有依据的默认值 —— 实测
// 一份 PRD 前 5 轮每轮问满 3 个，其中多数是「拿不到会话 ID 时怎么办」
// 「dist 产物算不算进文件阈值」这种给个默认就能往下走的实现细节，人被
// 问烦了只回「可以」「A」。收紧到 B 两个、C 起一个，逼它只问答错会返工的事，
// 其余写成默认值记进决策，人在文档面板上看着不对再改。
func QuestionBudget(s Stage) int {
	switch s {
	case StageUnderstand:
		return maxQuestionsPerRound
	case StageExplore:
		return 2
	default:
		return 1
	}
}

// stageBrief 是每个阶段的任务说明（docs/10 §1.3 对话协议）。
func stageBrief(s Stage) string {
	switch s {
	case StageUnderstand:
		return `本轮是阶段 A（理解）。**先不要读代码。**
要做的：复述你理解到的意图、判定 PRD 类型（defect/feature/refactor）、
写出 §1 一句话草稿，只问「答案会改变方案走向」的问题。`

	case StageExplore:
		return `本轮是阶段 B（探索）。你现在可以读代码（只读）。
要做的：§2 问题陈述与 §5 现状分析，**每条都要带 file:line 证据**。
「感觉应该做」不是证据。同时列出该区域的现有测试 —— 它们是「不变量保持」
类 AC 的来源。§6 方案：给出你推荐的那条路并写清理由与代价；只有两条路
各有硬伤、你确实无法替人取舍时，才把它作为本轮的问题。`

	case StageConverge:
		return `本轮是阶段 C（收敛）。
**代码你在阶段 B 已经读过了**：§5 的 file:line 就在下面的快照里。只做必要的核对
（改动点是否还成立），不要重新通读仓库。
要做的：定 §3 目标（G-n，每条附「怎么知道达成了」）与非目标（明确不做 + 理由）；
写 §4 场景（S-n）；按七类防线写 §7.1 AC 表。
AC 用 Given/When/Then + 具体值 + 判据。自检每条：「这条 AC 我现在能写出
断言吗？」写不出来的不是 AC，是愿望 —— 重写它。
每条 AC 配 1~2 个正例 + 1 个反例 + 一条反向确认（「按此 AC，X 情况不会被
拦，你接受吗？」）。**反向确认写在 AC 的 reverseConfirm 字段里，人在文档
面板上逐条答复，不要把它们当成本轮的问题来问。**
收敛阶段至多两轮：这一轮就尽量把 §3 §4 §7 写全，不要留到下一轮。`

	case StageSplit:
		return `本轮是阶段 D（拆分）。
**不要重新通读仓库。** §5 的现状分析与 §6 的方案都在下面的快照里，每个改点
都带 file:line；estimate 按那份分析估，最多再核对几个关键文件（接口签名、
现有测试），不要为了「估得更准」把整个仓库再扫一遍 —— 实测那样会烧掉一整轮
的时间预算。
要做的：把方案拆成任务（§8 结构化块）。规则：
- 一个任务 = 一个可独立验证的行为变化 = 一个 PR。
- dependsOn **只能填一个** key（入度 ≤ 1，图是森林）。**能并行就不要串链** ——
  只有 B 真的要用 A 的代码才连线。一个任务真依赖两个前驱时，把前驱串成链
  或合并，不要在 dependsOn 里写「T4,T6」。
- 数据库迁移单独成任务，哪怕只有十几行。
- 不要把「纯前端展示」与「后端/迁移」混在一个任务里（定档会被迫升 heavy）。
- description 必须写清**要交付什么复现/验收测试** —— 红-绿判决按任务下，
  没写清实现 agent 就会自己猜。
- estimate 按你读代码的判断给，超仓库阈值就拆开。
- 每条 AC 的 taskKey 指向交付它的任务；每个任务的 acceptance 只引用已存在的 AC。`

	case StageFinalize:
		return `本轮是阶段 E（定稿）。
要做的：补齐 §9 风险与未决、§10 决策记录、附录 B 证据索引；
复查每个 file:line 现在还成立吗（读代码是几轮前的事）；
复查每个新增配置项写了消费方吗；
复查 §3 非目标里有没有对话中人提过、但你没归位的想法；
把每个任务的 description 单独拿出来读一遍：只看这一段，你会判它
「明确、可动手」还是「需求不清楚」？判后者的要就地补写清楚 ——
这些任务生成后都要过分诊，在那里被打回一次的代价远高于你现在多想一分钟。
如果人在本轮提了修改意见，先按意见改，再做上面的复查。`
	}
	return ""
}

// BuildRoundPrompt 拼一轮规划对话的 prompt。
func BuildRoundPrompt(in RoundInput) string {
	var b strings.Builder

	b.WriteString(`你是 Lathe 的规划智能体。你的产出是一份 PRD，它会被人逐节审阅签字，
批准后由系统机械地转成正式开发任务与编排图。

**你现在是只读的。** 不许改、建、删任何文件，不许跑改动状态的命令。
你可以读代码、搜索、看测试。你的全部产出是下面约定的那段 JSON。

写 PRD 的读者首先是分诊器和验证管线，其次才是人：每个拆出的任务要能过
「明确度判定」，每条验收标准要能让实现 agent 写出先红后绿的测试。因此
验收标准与任务描述不能是散文。

`)

	fmt.Fprintf(&b, "目标仓库：%s\n", in.Repo)
	fmt.Fprintf(&b, "当前轮次：第 %d 轮\n", in.Round)
	if in.Type != "" {
		fmt.Fprintf(&b, "PRD 类型：%s\n", in.Type)
	}
	fmt.Fprintf(&b, "任务量级阈值：单任务 ≤ %d 行净改动 / ≤ %d 个文件；链长建议 ≤ %d\n\n",
		in.Limits.MaxLines, in.Limits.MaxFiles, in.Limits.MaxChain)

	b.WriteString("## 模糊任务原文（所有推断的根，不可改写）\n\n")
	b.WriteString(in.OriginalInput)
	b.WriteString("\n\n")

	if in.UserAnswer != "" {
		b.WriteString("## 人对上一轮问题的回答 / 本轮补充\n\n")
		b.WriteString(in.UserAnswer)
		b.WriteString("\n\n")
		b.WriteString("人回答「按默认」「可以」「你定」或只答了部分问题时：没答的按你给过的默认值处理，" +
			"记进 decisions，不要再问一遍。\n\n")
	}

	if len(in.Decisions) > 0 {
		b.WriteString("## 已拍板的决策（不许重问，人改主意会显式给你新决策）\n\n")
		for i, d := range in.Decisions {
			fmt.Fprintf(&b, "%d. [第 %d 轮 · %s] 问：%s → 定：%s", i+1, d.Round, d.Kind, d.Question, d.Decision)
			if d.Reason != "" {
				fmt.Fprintf(&b, "（理由：%s）", d.Reason)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if in.Document != nil {
		if snapshot := renderSnapshot(in.Document); snapshot != "" {
			b.WriteString("## 当前 PRD 快照（你的 patch 会合并进它）\n\n")
			b.WriteString(snapshot)
			b.WriteString("\n")
		}
	}

	b.WriteString("## 本轮任务\n\n")
	b.WriteString(stageBrief(in.Stage))
	b.WriteString("\n\n")

	budget := QuestionBudget(in.Stage)
	fmt.Fprintf(&b, `## 什么时候提问（本轮最多 %d 个）

提问的代价是让人停下来等你一轮。只问**答错了要返工**的事：目标/范围的取舍、
两条方案各有硬伤且你无法替人选。下面这些**不要问**，自己定：
- 实现细节（错误码、重试策略、默认值、字段命名、文件怎么放）；
- 读代码能确定的事实（现在是谁在调用、有没有现成实现）；
- 反向确认（写进 AC 的 reverseConfirm，人在面板上答）。
自己定的事写进 decisions（kind 用 "boundary" 或 "option_choice"，reason 写依据），
并在 notes 里用一两句列出「我替你定了：……，不同意直接说」。
每个问题都要带上你的推荐默认值，格式：「问题？（默认：X）」—— 人回「按默认」就能推进。
没有非问不可的事就给空数组，这是最好的结果。

`, budget)

	b.WriteString(`## 输出格式（严格遵守）

只输出一个 JSON 对象，不要前后解释，不要 markdown 代码围栏。
**只交本轮改动**（patch），没改的节与条目不要重复输出 —— 系统会把 patch
合并进上面的快照。

{
  "type": "defect|feature|refactor",
  "patch": {
    "sections": {
      "<节 key>": {"status": "inferred|pending", "body": "markdown，整节替换"}
    },
    "goalList":     [{"id": "G-1", "text": "", "measure": "怎么知道达成了"}],
    "nonGoals":     [{"text": "", "reason": ""}],
    "scenarioList": [{"id": "S-1", "title": "", "given": "", "when": "", "then": ""}],
    "criteria": [{
      "id": "AC-1",
      "category": "happy_path|failure_path|boundary|invariant|non_functional|observability|compatibility",
      "given": "带具体值", "when": "带具体值", "then": "带具体值",
      "evidence": "观察什么：HTTP 状态/表行/事件/UI 元素/度量值",
      "verify": "auto_test|script|manual",
      "manualSteps": "verify 为 manual 时必填",
      "goalRefs": ["G-1"], "scenarioRefs": ["S-1"], "taskKey": "T1",
      "examples": [{"positive": true, "text": ""}, {"positive": false, "text": ""}],
      "reverseConfirm": "按此 AC，X 情况不会被拦，你接受吗"
    }],
    "tasks": [{
      "key": "T1", "title": "", "kind": "fix|feature|hotfix",
      "dependsOn": "单个前驱 key，根节点留空",
      "description": "写到能过分诊：目标、涉及范围、必须交付的复现/验收测试是什么",
      "filesHint": ["参考文件，不约束实现"],
      "acceptance": ["AC-1"],
      "verifyHint": "light|heavy",
      "estimate": {"lines": 0, "files": 0}
    }],
    "questions": [{"id": "Q-1", "text": "", "owner": "人", "status": "open|answered", "section": "节 key"}],
    "decisions": [{"kind": "boundary|option_choice|example|amend", "question": "", "decision": "", "reason": ""}],
    "evidences": [{"ref": "internal/x/y.go:31", "note": ""}],
    "remove": {"goalList": [], "scenarioList": [], "criteria": [], "tasks": [], "questions": [], "evidences": []}
  },
  "questions": ["本轮要问人的问题，带（默认：X）"],
  "notes": "给人看的一段话：本轮做了什么、替人定了什么、你最不确定的是什么"
}

合并规则：
- 节 key 只能是 oneLiner / problem / goals / scenarios / codeAnalysis / solution /
  acceptance / taskSplit / risks / decisionLogSect。给了就整节替换，status 只能写
  inferred 或 pending（confirmed 是人的动作）。
- 列表按 id（任务按 key、证据按 ref）upsert：改一条就给那一条的**完整**内容。
  新编号接着已有的往后排，不要复用已删除的编号。
- 删除只认 remove 清单。删 AC 前先把引用它的任务/AC 一起改掉，否则会被判悬空引用。
- nonGoals 没有编号：要改就给出完整的新表。
- decisions 只增不改；推翻旧决策用 kind "amend" 新增一条。
- 上一轮问过的问题人答了，把对应 Q-n 的 status 改成 answered（或 remove 掉），
  它挂着 open 就进不了评审。

七类 AC 是**固定行**（重构类例外，只要 behavior_preserved 与 structural_metric
两类）：每一类要么填一条真 AC，要么填一条 {"id","category","na":true,"reason":"为什么不适用"}。
不许留空 —— 逼你对每个维度想过再说不需要。

禁用词：正确、合理、友好、快速、稳定、完善、优雅、健壮。
这些词出现在 AC 里而旁边没有数字或判据，这条 AC 会被自检直接打回。
`)

	return b.String()
}

// renderSnapshot 把当前快照渲染成紧凑文本喂回模型。
//
// 不直接塞 JSON：JSON 里全是转义的换行，模型读起来费劲，且 token 更贵。
// 但凡模型做 upsert 要用到的东西（编号、任务描述、依赖、未决问题）都要
// 渲染出来 —— 它只能在这里看到现有条目，看不到就会编出重复编号，或者
// 改一条任务时把描述写丢。
func renderSnapshot(d *Document) string {
	var b strings.Builder
	for _, s := range d.Sections() {
		if strings.TrimSpace(s.Section.Body) == "" {
			continue
		}
		fmt.Fprintf(&b, "### %s（key=%s）[%s]\n%s\n\n", s.Title, s.Key, s.Section.Status, s.Section.Body)
	}
	if len(d.GoalList) > 0 {
		fmt.Fprintf(&b, "### 已有目标（%d 条）\n", len(d.GoalList))
		for _, g := range d.GoalList {
			fmt.Fprintf(&b, "- %s %s（达成判据：%s）\n", g.ID, g.Text, g.Measure)
		}
		b.WriteString("\n")
	}
	if len(d.NonGoals) > 0 {
		b.WriteString("### 已有非目标\n")
		for _, n := range d.NonGoals {
			fmt.Fprintf(&b, "- %s（理由：%s）\n", n.Text, n.Reason)
		}
		b.WriteString("\n")
	}
	if len(d.ScenarioList) > 0 {
		fmt.Fprintf(&b, "### 已有场景（%d 条）\n", len(d.ScenarioList))
		for _, s := range d.ScenarioList {
			fmt.Fprintf(&b, "- %s %s：Given %s / When %s / Then %s\n", s.ID, s.Title, s.Given, s.When, s.Then)
		}
		b.WriteString("\n")
	}
	if len(d.Criteria) > 0 {
		fmt.Fprintf(&b, "### 已有 AC（%d 条）\n", len(d.Criteria))
		for _, c := range d.Criteria {
			if c.NA {
				fmt.Fprintf(&b, "- %s [%s] N/A：%s\n", c.ID, c.Category, c.Reason)
				continue
			}
			fmt.Fprintf(&b, "- %s [%s] Given %s / When %s / Then %s（判据：%s，验证：%s，目标：%s，场景：%s，任务：%s）\n",
				c.ID, c.Category, c.Given, c.When, c.Then, c.Evidence, c.Verify,
				strings.Join(c.GoalRefs, ","), strings.Join(c.ScenarioRefs, ","), c.TaskKey)
			if c.ReverseConfirm != "" {
				answer := "人未答复"
				if c.ReverseAccepted != nil {
					answer = map[bool]string{true: "人已接受", false: "人不接受，需改 AC"}[*c.ReverseAccepted]
				}
				fmt.Fprintf(&b, "  反向确认：%s（%s）\n", c.ReverseConfirm, answer)
			}
		}
		b.WriteString("\n")
	}
	if len(d.Tasks) > 0 {
		fmt.Fprintf(&b, "### 已有任务拆分（%d 个）\n", len(d.Tasks))
		for _, t := range d.Tasks {
			dep := "根节点"
			if t.DependsOn != "" {
				dep = "依赖 " + t.DependsOn
			}
			fmt.Fprintf(&b, "- %s [%s] %s（%s，AC: %s，估 %d 行/%d 文件）\n  描述：%s\n",
				t.Key, t.Kind, t.Title, dep, strings.Join(t.Acceptance, ","),
				t.Estimate.Lines, t.Estimate.Files, t.Description)
		}
		b.WriteString("\n")
	}
	if len(d.Questions) > 0 {
		b.WriteString("### 未决问题\n")
		for _, q := range d.Questions {
			status := q.Status
			if status == "" {
				status = QuestionOpen
			}
			fmt.Fprintf(&b, "- %s [%s] %s\n", q.ID, status, q.Text)
		}
		b.WriteString("\n")
	}
	if len(d.Evidences) > 0 {
		fmt.Fprintf(&b, "### 已有证据（%d 条，ref 即编号）\n", len(d.Evidences))
		for _, e := range d.Evidences {
			fmt.Fprintf(&b, "- %s %s\n", e.Ref, e.Note)
		}
		b.WriteString("\n")
	}
	return b.String()
}
