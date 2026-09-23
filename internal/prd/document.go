package prd

import "encoding/json"

// 本文件是 PRD 快照（prds.document jsonb）的 Go 形状，逐节对应
// docs/10-prd-template.md §2 模板正文。
//
// 为什么整份存 jsonb 而不拆表：模板的节形状随变体（defect/feature/refactor）
// 演进，拆表则模板改一次就得加一条迁移；而「每轮交付完整 PRD 快照」
// （D10-7）本就是整份替换，不存在部分更新的需求。代价是结构合法性
// （docs/10 §6.1 机器可判那一档）落在 Go 层校验，见 checklist.go。

// SectionStatus 是节状态标记（docs/10 §1.2）。
//
// 它决定 PRD 能否进评审：不许有 ❓，且所有 🤖 都要被人逐节看过变 ✅。
// 「看过一遍」在 UI 上是逐节确认动作，不是整页一个按钮 —— 因为一个按钮
// 的实际效果是没人看。
type SectionStatus string

const (
	// StatusConfirmed ✅ 已确认：人明确拍过板。
	StatusConfirmed SectionStatus = "confirmed"
	// StatusInferred 🤖 推断：智能体从代码或对话推出，待人过目。
	StatusInferred SectionStatus = "inferred"
	// StatusPending ❓ 待答：挂着未决问题。
	StatusPending SectionStatus = "pending"
)

// Valid 报告标记是否已知。
func (s SectionStatus) Valid() bool {
	switch s {
	case StatusConfirmed, StatusInferred, StatusPending:
		return true
	}
	return false
}

// Type 是 PRD 类型，决定 §4 §7 §8 的变体（docs/10 §3 变体差异速查）。
type Type string

const (
	// TypeDefect 缺陷：§4 改为复现路径，§2 证据必须含一次复现。
	TypeDefect Type = "defect"
	// TypeFeature 功能：七类 AC 完整表。
	TypeFeature Type = "feature"
	// TypeRefactor 重构：§4 改为行为保持清单，§7 仅「行为保持」+「结构度量」，
	// §8 首节点必须是补特征测试任务。
	TypeRefactor Type = "refactor"
)

// Valid 报告类型是否已知。
func (t Type) Valid() bool {
	switch t {
	case TypeDefect, TypeFeature, TypeRefactor:
		return true
	}
	return false
}

// Stage 是对话协议的阶段（docs/10 §1.3）。
type Stage string

const (
	// StageUnderstand A 理解：复述意图、判类型、提 ≤3 个关键问题，先不读代码。
	StageUnderstand Stage = "understand"
	// StageExplore B 探索：只读进仓库，出现状分析与问题陈述证据。
	StageExplore Stage = "explore"
	// StageConverge C 收敛：定目标/非目标/场景，按七层防线写验收标准。
	StageConverge Stage = "converge"
	// StageSplit D 拆分：出任务表 + 依赖 + 覆盖矩阵。
	StageSplit Stage = "split"
	// StageFinalize E 定稿：跑对抗复核与自检清单。
	StageFinalize Stage = "finalize"
)

// Valid 报告阶段是否已知。
func (s Stage) Valid() bool {
	switch s {
	case StageUnderstand, StageExplore, StageConverge, StageSplit, StageFinalize:
		return true
	}
	return false
}

// Section 是任意一节的公共外壳：正文 + 状态标记。
//
// Body 是渲染好的 Markdown 而不是再拆结构：§2/§5/§6 这些节的形状因 PRD
// 类型而异，强行结构化只会得到一堆各变体都用不满的字段。需要机器校验的
// 部分（§7 AC 表、§8 任务块）单独建了类型，见下。
type Section struct {
	Status SectionStatus `json:"status"`
	Body   string        `json:"body,omitempty"`
	// Questions 是本节挂着的未决问题编号（Q-n），Status 为 pending 时非空。
	Questions []string `json:"questions,omitempty"`
}

// Goal 是 §3 的一条目标（G-n）。每条必须附「怎么知道达成了」——
// 没有达成判据的目标是愿望。
type Goal struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Measure string `json:"measure"`
}

// NonGoal 是 §3 的一条非目标：明确不做 + 理由。
//
// 这一节是防扩 scope 的主闸门：对话里人每提一个新想法，智能体必须把它
// 归入目标或非目标，不许悬空。
type NonGoal struct {
	Text   string `json:"text"`
	Reason string `json:"reason"`
}

// Scenario 是 §4 的一条场景（S-n）。
//
// 三种变体共用这个形状：feature 是 Given/When/Then 用户场景；defect 是
// 复现路径（Then 写期望 vs 实际）；refactor 是必须保持不变的对外行为
// （ExistingTest 指向现有测试，空则标注需先补特征测试）。
type Scenario struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Given string `json:"given"`
	When  string `json:"when"`
	Then  string `json:"then"`
	// ExistingTest 仅 refactor 变体用：指向守着这条行为的现有测试。
	ExistingTest string `json:"existingTest,omitempty"`
}

// Evidence 是 §2 / §5 的一条代码证据。
//
// Ref 必须是可查的 file:line；附录 B 由全部 Evidence 去重汇总而成。
// 「感觉应该做」不是证据（docs/10 §2）。
type Evidence struct {
	Ref  string `json:"ref"`
	Note string `json:"note,omitempty"`
}

// Document 是一份 PRD 的完整快照。
//
// 字段名与 docs/10 §2 的节号一一对应，方便审阅时对照模板。
type Document struct {
	// Meta 是 §0 元信息里由智能体填的部分（编号、状态等在 prds 表列上，
	// 不在快照里重复 —— 重复就会不一致）。
	Type Type `json:"type"`

	OneLiner        Section `json:"oneLiner"`        // §1 一句话（≤60 字）
	Problem         Section `json:"problem"`         // §2 问题陈述（Why）
	Goals           Section `json:"goals"`           // §3 目标与非目标
	Scenarios       Section `json:"scenarios"`       // §4 用户场景 / 复现路径 / 行为清单
	CodeAnalysis    Section `json:"codeAnalysis"`    // §5 现状分析
	Solution        Section `json:"solution"`        // §6 方案
	Acceptance      Section `json:"acceptance"`      // §7 验收标准
	TaskSplit       Section `json:"taskSplit"`       // §8 任务拆分与依赖
	Risks           Section `json:"risks"`           // §9 风险与未决
	DecisionLogSect Section `json:"decisionLogSect"` // §10 决策记录

	// 以下是需要机器校验的结构化内容，与上面的节正文并存：
	// 节正文给人看，结构化块给自检清单与一键生成用，两者由同一轮产出。

	GoalList     []Goal      `json:"goalList,omitempty"`     // §3 G-n
	NonGoals     []NonGoal   `json:"nonGoals,omitempty"`     // §3 非目标
	ScenarioList []Scenario  `json:"scenarioList,omitempty"` // §4 S-n
	Criteria     []Criterion `json:"criteria,omitempty"`     // §7.1 AC 表
	Tasks        []TaskBlock `json:"tasks,omitempty"`        // §8 结构化任务块
	Questions    []Question  `json:"questions,omitempty"`    // §9 未决 Q-n
	Decisions    []Decision  `json:"decisions,omitempty"`    // §10 决策记录
	Evidences    []Evidence  `json:"evidences,omitempty"`    // 附录 B 代码证据索引

	// DoD 是 §7.3 上线条件，由系统附加、智能体不编写（docs/10 §5.6）。
	// 存进快照是为了让导出的 PRD 自带完整 DoD，而不是渲染时才拼。
	DoD []string `json:"dod,omitempty"`
}

// Sections 按模板顺序返回全部节的指针，供逐节遍历（自检、逐节确认）。
//
// 返回指针而非值：调用方要能就地改 Status（人确认一节 = 把 inferred 改成
// confirmed），拿到副本改了没用。
func (d *Document) Sections() []struct {
	Key     string
	Title   string
	Section *Section
} {
	return []struct {
		Key     string
		Title   string
		Section *Section
	}{
		{"oneLiner", "§1 一句话", &d.OneLiner},
		{"problem", "§2 问题陈述", &d.Problem},
		{"goals", "§3 目标与非目标", &d.Goals},
		{"scenarios", "§4 用户场景", &d.Scenarios},
		{"codeAnalysis", "§5 现状分析", &d.CodeAnalysis},
		{"solution", "§6 方案", &d.Solution},
		{"acceptance", "§7 验收标准", &d.Acceptance},
		{"taskSplit", "§8 任务拆分与依赖", &d.TaskSplit},
		{"risks", "§9 风险与未决", &d.Risks},
		{"decisionLogSect", "§10 决策记录", &d.DecisionLogSect},
	}
}

// Marshal 把快照序列化为 jsonb 可直接入库的字节。
func (d *Document) Marshal() (json.RawMessage, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

// ParseDocument 从库里的 jsonb 还原快照。空输入返回零值文档而不是错误 ——
// 刚建的 PRD 还没跑过任何一轮，document 是 NULL。
func ParseDocument(raw json.RawMessage) (*Document, error) {
	var d Document
	if len(raw) == 0 {
		return &d, nil
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	return &d, nil
}
