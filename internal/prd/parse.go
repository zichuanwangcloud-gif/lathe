package prd

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 本文件把 agent 的文本回答解析成结构化快照，形状照搬
// internal/preview/recommend.go 的 parseRecommendation：
// 截 JSON → 反序列化 → **逐字段校验幻觉**。
//
// 第三步是重点。模型很乐意编一个 G-9 出来，或者给一条 category 写成
// "happy path"（带空格）的 AC。这些错如果直接落库，会在几轮之后以
// 「自检莫名不过」的形式爆出来，届时没人知道是哪一轮写坏的。
// 在入口处拒绝，错误就指向它真正的来源。

// RoundResult 是一轮对话的解析结果。
type RoundResult struct {
	// Document 是本轮交付的完整快照。
	Document *Document
	// Questions 是本轮要问人的问题（≤ maxQuestionsPerRound）。
	Questions []string
	// Notes 是给人看的一段话：本轮做了什么、最不确定什么。
	Notes string
}

// rawRound 是 agent 输出的线格式。
//
// Patch 与 Document 二选一：Patch 是现行格式（只交改动，见 patch.go），
// Document 是旧的完整快照格式，仍然接受 —— 模型偶尔会照旧交整份，
// 拒掉它只会白白多一次修正往返。两个都给时以 Patch 为准。
type rawRound struct {
	Type      string          `json:"type"`
	Patch     json.RawMessage `json:"patch"`
	Document  json.RawMessage `json:"document"`
	Questions []string        `json:"questions"`
	Notes     string          `json:"notes"`
}

// RoundParse 是解析一轮输出所需的上下文。
type RoundParse struct {
	// Stage 用于放宽早期阶段的校验（见 ParseRoundResult）。
	Stage Stage
	// Base 是上一轮的完整快照（首轮为 nil），patch 合并进它。
	Base *Document
	// Round 是本轮轮次，给新决策补轮号。
	Round int
	// MaxQuestions 是本轮问题上限，0 取 maxQuestionsPerRound。
	MaxQuestions int
}

// ParseRoundResult 解析一轮的 agent 输出（无上一版快照、按默认问题上限）。
//
// stage 用于放宽早期阶段的校验：阶段 A 还没读代码，不可能有 file:line
// 证据，也不该要求 AC 表齐全 —— 那些是阶段 C 之后的事。定稿闸门在
// Check()，不在这里。这里只拦「结构性说不通」的东西。
func ParseRoundResult(text string, stage Stage) (*RoundResult, error) {
	return ParseRound(text, RoundParse{Stage: stage})
}

// ParseRound 解析一轮的 agent 输出：截 JSON → 合并补丁（或接收完整快照）
// → 对合并结果做结构校验。
func ParseRound(text string, in RoundParse) (*RoundResult, error) {
	payload, err := extractJSON(text)
	if err != nil {
		return nil, err
	}

	var raw rawRound
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return nil, fmt.Errorf("prd: agent 输出不是合法 JSON: %w", err)
	}

	var doc *Document
	switch {
	case len(raw.Patch) > 0 && string(raw.Patch) != "null":
		var p DocPatch
		if err := json.Unmarshal(raw.Patch, &p); err != nil {
			return nil, fmt.Errorf("prd: 解析 patch 失败: %w", err)
		}
		doc, err = in.Base.Apply(&p, in.Round)
		if err != nil {
			return nil, err
		}
	case len(raw.Document) > 0:
		doc, err = ParseDocument(raw.Document)
		if err != nil {
			return nil, fmt.Errorf("prd: 解析 document 失败: %w", err)
		}
		normalizeFullSnapshot(doc, in.Base)
	default:
		return nil, fmt.Errorf("prd: agent 输出缺 patch 字段")
	}

	// 类型：顶层与 document 内不一致时以顶层为准（顶层是模型的「结论」，
	// document.type 往往是它照抄模板忘了改的）。
	if raw.Type != "" {
		doc.Type = Type(raw.Type)
	}
	if doc.Type != "" && !doc.Type.Valid() {
		return nil, fmt.Errorf("prd: PRD 类型 %q 不在 defect/feature/refactor 内", doc.Type)
	}

	if err := validateDocument(doc, in.Stage); err != nil {
		return nil, err
	}

	limit := in.MaxQuestions
	if limit <= 0 {
		limit = maxQuestionsPerRound
	}
	qs := make([]string, 0, len(raw.Questions))
	for _, q := range raw.Questions {
		if q = strings.TrimSpace(q); q != "" {
			qs = append(qs, q)
		}
	}
	if len(qs) > limit {
		// 截断而不是报错：模型多问了不是致命错误，但不能让它绕过
		// 问题预算 —— 一轮 10 个问题的结果是人不答。
		qs = qs[:limit]
	}

	return &RoundResult{Document: doc, Questions: qs, Notes: raw.Notes}, nil
}

// normalizeFullSnapshot 处理旧格式（完整快照）的节状态。
//
// 模型不许自封「人已确认」—— 那是人的动作，只能由逐节确认接口写入。
// 但「原样放回」上一轮人已确认的节时，正文没变就沿用人的确认：原来一律
// 降级，人逐节点过的确认下一轮就全没了。
func normalizeFullSnapshot(d, base *Document) {
	prev := map[string]Section{}
	if base != nil {
		for _, s := range base.Sections() {
			prev[s.Key] = *s.Section
		}
	}
	for _, s := range d.Sections() {
		switch s.Section.Status {
		case "":
			// 没给 status 视作推断 —— 缺字段不能默默当成已确认。
			s.Section.Status = StatusInferred
		case StatusConfirmed:
			old, ok := prev[s.Key]
			if !ok || old.Status != StatusConfirmed || old.Body != s.Section.Body {
				s.Section.Status = StatusInferred
			}
		}
	}
}

// extractJSON 从可能夹带解释文字的回答里截出 JSON 对象。
//
// 与 recommend.go 同策略：取第一个 { 到最后一个 }。模型偶尔会在 JSON 前后
// 加一句「好的，这是我的分析：」，直接 Unmarshal 会失败。
func extractJSON(text string) (string, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return "", fmt.Errorf("prd: agent 输出里找不到 JSON 对象（前 200 字：%s）", head(text, 200))
	}
	return text[start : end+1], nil
}

func head(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// validateDocument 逐字段拦幻觉。
//
// 只拦「结构上说不通」的：未知枚举值、编号引用悬空、同编号重复。
// 「内容不够好」（AC 写得虚、证据不带行号）交给 Check()，因为那些在
// 早期阶段是正常的中间态。
func validateDocument(d *Document, stage Stage) error {
	for _, s := range d.Sections() {
		if s.Section.Status == "" {
			s.Section.Status = StatusInferred
			continue
		}
		if !s.Section.Status.Valid() {
			return fmt.Errorf("prd: %s 的 status %q 未知（应为 confirmed/inferred/pending）",
				s.Title, s.Section.Status)
		}
	}

	goals := map[string]bool{}
	for i, g := range d.GoalList {
		if g.ID == "" {
			return fmt.Errorf("prd: 第 %d 条目标缺 id", i+1)
		}
		if goals[g.ID] {
			return fmt.Errorf("prd: 目标编号 %s 重复", g.ID)
		}
		goals[g.ID] = true
	}

	scenarios := map[string]bool{}
	for i, s := range d.ScenarioList {
		if s.ID == "" {
			return fmt.Errorf("prd: 第 %d 条场景缺 id", i+1)
		}
		if scenarios[s.ID] {
			return fmt.Errorf("prd: 场景编号 %s 重复", s.ID)
		}
		scenarios[s.ID] = true
	}

	acs := map[string]bool{}
	for i, c := range d.Criteria {
		if c.ID == "" {
			return fmt.Errorf("prd: 第 %d 条 AC 缺 id", i+1)
		}
		if acs[c.ID] {
			return fmt.Errorf("prd: AC 编号 %s 重复", c.ID)
		}
		acs[c.ID] = true

		if c.Category == "" {
			return fmt.Errorf("prd: %s 缺 category", c.ID)
		}
		if !categoryKnown(c.Category) {
			return fmt.Errorf("prd: %s 的 category %q 未知", c.ID, c.Category)
		}
		if !c.NA && c.Verify != "" && !c.Verify.Valid() {
			return fmt.Errorf("prd: %s 的 verify %q 未知（应为 auto_test/script/manual）", c.ID, c.Verify)
		}
		// 引用悬空：模型编一个 G-9 出来是最常见的幻觉形态。
		for _, ref := range c.GoalRefs {
			if !goals[ref] {
				return fmt.Errorf("prd: %s 引用了不存在的目标 %s", c.ID, ref)
			}
		}
		for _, ref := range c.ScenarioRefs {
			if !scenarios[ref] {
				return fmt.Errorf("prd: %s 引用了不存在的场景 %s", c.ID, ref)
			}
		}
	}

	keys := map[string]bool{}
	for i, t := range d.Tasks {
		if t.Key == "" {
			return fmt.Errorf("prd: 第 %d 个任务缺 key", i+1)
		}
		if keys[t.Key] {
			return fmt.Errorf("prd: 任务 key %s 重复", t.Key)
		}
		keys[t.Key] = true
		if t.Kind != "" && !t.Kind.Valid() {
			return fmt.Errorf("prd: 任务 %s 的 kind %q 不在 fix/feature/hotfix 内", t.Key, t.Kind)
		}
		for _, ac := range t.Acceptance {
			if !acs[ac] {
				return fmt.Errorf("prd: 任务 %s 引用了不存在的 %s", t.Key, ac)
			}
		}
	}
	// 依赖引用要在收齐全部 key 之后再查（前向引用是合法的）。
	for _, t := range d.Tasks {
		if t.DependsOn != "" && !keys[t.DependsOn] {
			if strings.ContainsAny(t.DependsOn, ",， ") {
				// 实测最常见的形态：模型把多个前驱写进一个字段。报错里直接
				// 讲清规则，修正回合才知道该怎么改（编排图入度 ≤ 1）。
				return fmt.Errorf("prd: 任务 %s 的 dependsOn %q 写了多个前驱 —— dependsOn 只能填一个 key（编排图入度 ≤ 1）；"+
					"要么只留真正用到其代码的那一个，要么把这几个前驱串成链、或合并成一个任务", t.Key, t.DependsOn)
			}
			return fmt.Errorf("prd: 任务 %s 依赖的 %s 不存在", t.Key, t.DependsOn)
		}
	}

	for i, q := range d.Questions {
		if q.ID == "" {
			return fmt.Errorf("prd: 第 %d 条未决问题缺 id", i+1)
		}
		switch q.Status {
		case "", QuestionOpen, QuestionAnswered, QuestionAcceptedUnanswered:
		default:
			return fmt.Errorf("prd: %s 的 status %q 未知", q.ID, q.Status)
		}
	}

	// 拆分阶段必须真的给出任务，否则这一轮什么也没推进。
	if stage == StageSplit && len(d.Tasks) == 0 {
		return fmt.Errorf("prd: 拆分阶段没有产出任何任务块")
	}
	return nil
}

func categoryKnown(c ACCategory) bool {
	for _, k := range append(StandardCategories(), RefactorCategories()...) {
		if c == k {
			return true
		}
	}
	return false
}
