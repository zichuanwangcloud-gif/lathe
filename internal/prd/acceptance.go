package prd

// 本文件是 §7 验收标准与 §9/§10 的结构化形状。
//
// 为什么 AC 必须结构化而 §2/§5/§6 可以是 Markdown：AC 的读者首先是分诊器
// 和验证管线（docs/10 §0.3 第 1 条）。每条 AC 要能被覆盖矩阵检查孤儿、
// 被禁用词表扫描、被绑到交付任务上 —— 散文做不到这些。

// ACCategory 是 §5.2 的七类防线之一。
//
// 这七行在 AC 表里是固定行，每行要么填 AC，要么写 N/A + 理由，不许留空。
// 强制填空逼智能体对每个维度想过再说不需要 —— 与「配置了但没接线」教训
// 同一逻辑：显式优于沉默。
type ACCategory string

const (
	// CatHappyPath 正向路径：功能按场景走通。
	CatHappyPath ACCategory = "happy_path"
	// CatFailurePath 失败路径：非法输入拒绝；非属主访问返 404 不返 403。
	CatFailurePath ACCategory = "failure_path"
	// CatBoundary 边界与幂等：空集合、上限、并发；webhook 重投递不重复建任务。
	CatBoundary ACCategory = "boundary"
	// CatInvariant 不变量保持：改动后任何路径到 pr_open 仍必经 verifying。
	CatInvariant ACCategory = "invariant"
	// CatNonFunctional 非功能：超时上限、凭据不进日志/事件流、迁移可 down。
	CatNonFunctional ACCategory = "non_functional"
	// CatObservability 可观测性：状态转移落 task_events，失败有 failure_stage。
	CatObservability ACCategory = "observability"
	// CatCompatibility 兼容与默认：新功能对存量部署默认关闭。
	CatCompatibility ACCategory = "compatibility"

	// 以下两类是 refactor 变体专用（docs/10 §7 重构类变体）：
	// 重构不引入新行为，七类里大半都填不出东西，硬填只会得到七行 N/A。

	// CatBehaviorPreserved 行为保持：现有测试全绿；未覆盖的先补特征测试，
	// 且特征测试要先证明能在旧代码上绿、被破坏时能红。
	CatBehaviorPreserved ACCategory = "behavior_preserved"
	// CatStructuralMetric 结构度量：依赖方向、文件/函数数、重复代码量，
	// 目标值写死。
	CatStructuralMetric ACCategory = "structural_metric"
)

// StandardCategories 是 feature/defect 变体的七类固定行（docs/10 §5.2）。
func StandardCategories() []ACCategory {
	return []ACCategory{
		CatHappyPath, CatFailurePath, CatBoundary, CatInvariant,
		CatNonFunctional, CatObservability, CatCompatibility,
	}
}

// RefactorCategories 是 refactor 变体允许的两类（docs/10 §7 重构类变体）。
func RefactorCategories() []ACCategory {
	return []ACCategory{CatBehaviorPreserved, CatStructuralMetric}
}

// RequiredCategories 返回该类型 PRD 的 AC 表固定行。
func RequiredCategories(t Type) []ACCategory {
	if t == TypeRefactor {
		return RefactorCategories()
	}
	return StandardCategories()
}

// VerifyMethod 是 AC 的验证方式（三选一）。
type VerifyMethod string

const (
	// VerifyAuto 自动测试：绝大多数 AC 该是这个。
	VerifyAuto VerifyMethod = "auto_test"
	// VerifyScript 脚本：能跑但不进测试套件的（如迁移 down 验证）。
	VerifyScript VerifyMethod = "script"
	// VerifyManual 人工走查：必须写清步骤，否则「走查过了」无从复核。
	VerifyManual VerifyMethod = "manual"
)

// Valid 报告验证方式是否已知。
func (m VerifyMethod) Valid() bool {
	switch m {
	case VerifyAuto, VerifyScript, VerifyManual:
		return true
	}
	return false
}

// Example 是 §7.2 的一个例子或反例。
//
// 人不擅长审抽象标准，擅长判断具体案例 —— 例子是对齐意图的主要手段
// （docs/10 §5.4），也是对话里最该花时间的地方。
type Example struct {
	// Positive 为 true 表示正例（按此 AC 应当通过）。
	Positive bool   `json:"positive"`
	Text     string `json:"text"`
	// Verdict 是人对这个例子的判定，空串表示还没判。
	Verdict string `json:"verdict,omitempty"`
}

// Criterion 是 §7.1 的一条验收标准（AC-n）。
type Criterion struct {
	ID       string     `json:"id"`
	Category ACCategory `json:"category"`

	// NA 为 true 表示这一类不适用，此时 Reason 必须非空、其余字段可空。
	NA     bool   `json:"na,omitempty"`
	Reason string `json:"reason,omitempty"`

	// Given/When/Then 必须带具体值。形容词冒充标准会被禁用词表拦下
	// （docs/10 §5.3）。
	Given string `json:"given,omitempty"`
	When  string `json:"when,omitempty"`
	Then  string `json:"then,omitempty"`

	// Evidence 是判据：观察什么（HTTP 状态 / 表行 / 事件 / UI 元素 / 度量值）。
	// 「这条 AC 我现在能写出断言吗？」写不出来的不是 AC，是愿望。
	Evidence string       `json:"evidence,omitempty"`
	Verify   VerifyMethod `json:"verify,omitempty"`
	// ManualSteps 仅 Verify 为 manual 时必填。
	ManualSteps string `json:"manualSteps,omitempty"`

	// GoalRefs / ScenarioRefs 是可追溯闭环（§5.1）：每条 AC 回指 G-n 与 S-n，
	// 任何方向出现孤儿都是缺陷。
	GoalRefs     []string `json:"goalRefs,omitempty"`
	ScenarioRefs []string `json:"scenarioRefs,omitempty"`
	// TaskKey 是交付这条 AC 的任务（§8 的 T-key）。
	TaskKey string `json:"taskKey,omitempty"`

	// Examples 是 §7.2 的正例/反例。
	Examples []Example `json:"examples,omitempty"`
	// ReverseConfirm 是反向确认：「按这份 AC，X 情况不会被拦，你接受吗？」
	// 隐含期望几乎全藏在这里。
	ReverseConfirm string `json:"reverseConfirm,omitempty"`
	// ReverseAccepted 为 nil 表示人还没答；定稿前必须有明确的接受/不接受。
	ReverseAccepted *bool `json:"reverseAccepted,omitempty"`
}

// Question 是 §9 的一条未决问题（Q-n）。
//
// 定稿前必须清零 —— 要么答了，要么人显式标「接受不答」并进 §10。
type Question struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	// Owner 是谁答（通常是人，也可能是「读代码可定」）。
	Owner string `json:"owner,omitempty"`
	// Status: open / answered / accepted_unanswered
	Status string `json:"status"`
	Answer string `json:"answer,omitempty"`
	// Section 是这个问题挂在哪一节（对应 Document.Sections 的 Key）。
	Section string `json:"section,omitempty"`
}

// 未决问题的状态取值。
const (
	// QuestionOpen 还没答，挂着 ❓，不许进评审。
	QuestionOpen = "open"
	// QuestionAnswered 已答，答案进 §10。
	QuestionAnswered = "answered"
	// QuestionAcceptedUnanswered 人显式接受不答（docs/10 §9）。
	QuestionAcceptedUnanswered = "accepted_unanswered"
)

// Decision 是 §10 的一条决策记录。
//
// 后续轮次不许重问已拍板的问题（docs/10 §1.3 纪律 3）：人改主意可以，
// 但要走 DecisionAmend 而非「被重新问到」。
type Decision struct {
	// Round 是拍板发生在第几轮。
	Round int    `json:"round"`
	Kind  string `json:"kind"`
	// Question 是当时问的什么，Decision 是定了什么，Reason 是为什么。
	Question string `json:"question"`
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
	// Supersedes 仅 DecisionAmend 用：指向被推翻的条目下标。
	Supersedes *int `json:"supersedes,omitempty"`
}

// 决策记录的条目类型（docs/10 §10）。
const (
	// DecisionOptionChoice 选项选择：§6 方案分叉时人选了哪条。
	DecisionOptionChoice = "option_choice"
	// DecisionBoundary 边界确认：§7.2 反向确认的结果。
	DecisionBoundary = "boundary"
	// DecisionExample 例子判定：§7.2 正例/反例的判定。
	DecisionExample = "example"
	// DecisionAcceptOpen 接受未决：§9 显式接受不答。
	DecisionAcceptOpen = "accept_open"
	// DecisionAcceptOversize 接受超限：§8 某任务估算超仓库阈值但人认了
	// （docs/10 §6.1 自检清单的唯一豁免口）。
	DecisionAcceptOversize = "accept_oversize"
	// DecisionAmend 修改决策：推翻早前条目。
	DecisionAmend = "amend"
)
