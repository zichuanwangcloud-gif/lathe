package prd

import (
	"encoding/json"
	"fmt"
)

// 本文件实现「增量补丁」：每轮模型只交本轮改动的节与条目，由服务端合并进
// 上一版快照，再对合并结果跑同一套结构校验。
//
// 为什么推翻原来的「每轮交付完整快照」（docs/10 D10-7 的原始形态）：
//
//  1. 输出随文档线性膨胀。实测一份 feature PRD 第 11 轮的快照已经 47KB，
//     模型每轮要把它整份重写一遍 —— 单这一步就要 8 分钟以上，第 12 轮连续
//     两次撞上 10 分钟超时，一个字都没落下。
//  2. 重写即重新出错的机会。模型把没动的节「原样放回」时会顺手改坏编号
//     引用（T8 依赖 T4,T6 这种），整轮被校验拒掉，那几分钟的工作全部作废。
//  3. 人的确认被冲掉。模型被禁止自封 confirmed，原样放回的节会被降级成
//     inferred —— 人逐节点过的确认，下一轮就全没了。
//
// 「人随时能看全貌」的诉求不受影响：合并后的完整快照照样每轮落库
// （prd_rounds.document_snapshot），界面读的一直是完整快照。
//
// 合并语义刻意做得机械、可预期：
//   - 节：给了就整节替换（状态一律回到 inferred，除非模型标 pending），
//     没给就原样保留 —— 包括人的 confirmed。
//   - 有编号的条目（G-n / S-n / AC-n / T-key / Q-n / 证据 ref）：按编号
//     upsert，同编号整条替换，不做字段级合并（字段级合并会让「模型想清空
//     某字段」与「模型没提这个字段」无法区分）。
//   - 唯一例外是人填的字段（AC 的 reverseAccepted）：反向确认原文没变时
//     沿用人的答复 —— 那是人的判断，模型重写一遍 AC 不该把它抹掉。
//   - 非目标没有编号：给了就整表替换。
//   - 决策记录只增不改：已拍板的事不许被模型改写（改主意走 amend 新条目）。
//   - 删除走显式的 remove 清单，没在清单里的条目不会因为「这轮没提」而消失。

// DocPatch 是一轮的增量补丁（模型输出里的 patch 字段）。
type DocPatch struct {
	// Sections 按节 key（Document.Sections 的 Key）给出要替换的节。
	Sections map[string]*Section `json:"sections,omitempty"`

	GoalList     []Goal      `json:"goalList,omitempty"`
	ScenarioList []Scenario  `json:"scenarioList,omitempty"`
	Criteria     []Criterion `json:"criteria,omitempty"`
	Tasks        []TaskBlock `json:"tasks,omitempty"`
	Questions    []Question  `json:"questions,omitempty"`
	Evidences    []Evidence  `json:"evidences,omitempty"`
	Decisions    []Decision  `json:"decisions,omitempty"`
	// NonGoals 为 nil 表示不动；给了（哪怕空数组）就整表替换。
	NonGoals *[]NonGoal `json:"nonGoals,omitempty"`

	Remove PatchRemove `json:"remove,omitempty"`
}

// PatchRemove 是显式删除清单，按编号。
type PatchRemove struct {
	GoalList     []string `json:"goalList,omitempty"`
	ScenarioList []string `json:"scenarioList,omitempty"`
	Criteria     []string `json:"criteria,omitempty"`
	Tasks        []string `json:"tasks,omitempty"`
	Questions    []string `json:"questions,omitempty"`
	Evidences    []string `json:"evidences,omitempty"`
}

// Apply 把补丁合并进 base 的一份深拷贝并返回，base 本身不变。
//
// round 用于给没写轮次的新决策补上「第几轮拍的板」。
// 只拦补丁自身说不通的东西（未知节 key）；合并结果的结构合法性由调用方
// 交给 validateDocument —— 与完整快照走同一道闸门。
func (d *Document) Apply(p *DocPatch, round int) (*Document, error) {
	out, err := d.clone()
	if err != nil {
		return nil, err
	}
	if p == nil {
		return out, nil
	}

	if len(p.Sections) > 0 {
		byKey := map[string]*Section{}
		for _, s := range out.Sections() {
			byKey[s.Key] = s.Section
		}
		for key, sec := range p.Sections {
			dst, ok := byKey[key]
			if !ok {
				return nil, fmt.Errorf("prd: patch 里的节 %q 不存在（应为 oneLiner/problem/goals/…）", key)
			}
			if sec == nil {
				continue
			}
			next := *sec
			// 模型改过的节一律要人重新过目；pending 保留（挂着问题）。
			if next.Status != StatusPending {
				next.Status = StatusInferred
			}
			*dst = next
		}
	}

	out.GoalList = upsert(out.GoalList, p.GoalList, func(g Goal) string { return g.ID })
	out.GoalList = without(out.GoalList, p.Remove.GoalList, func(g Goal) string { return g.ID })

	out.ScenarioList = upsert(out.ScenarioList, p.ScenarioList, func(s Scenario) string { return s.ID })
	out.ScenarioList = without(out.ScenarioList, p.Remove.ScenarioList, func(s Scenario) string { return s.ID })

	prevReverse := map[string]Criterion{}
	for _, c := range out.Criteria {
		prevReverse[c.ID] = c
	}
	incoming := make([]Criterion, len(p.Criteria))
	for i, c := range p.Criteria {
		if old, ok := prevReverse[c.ID]; ok && c.ReverseAccepted == nil &&
			old.ReverseAccepted != nil && old.ReverseConfirm == c.ReverseConfirm {
			c.ReverseAccepted = old.ReverseAccepted
		}
		incoming[i] = c
	}
	out.Criteria = upsert(out.Criteria, incoming, func(c Criterion) string { return c.ID })
	out.Criteria = without(out.Criteria, p.Remove.Criteria, func(c Criterion) string { return c.ID })

	out.Tasks = upsert(out.Tasks, p.Tasks, func(t TaskBlock) string { return t.Key })
	out.Tasks = without(out.Tasks, p.Remove.Tasks, func(t TaskBlock) string { return t.Key })

	out.Questions = upsert(out.Questions, p.Questions, func(q Question) string { return q.ID })
	out.Questions = without(out.Questions, p.Remove.Questions, func(q Question) string { return q.ID })

	out.Evidences = upsert(out.Evidences, p.Evidences, func(e Evidence) string { return e.Ref })
	out.Evidences = without(out.Evidences, p.Remove.Evidences, func(e Evidence) string { return e.Ref })

	if p.NonGoals != nil {
		out.NonGoals = append([]NonGoal(nil), (*p.NonGoals)...)
	}

	for _, dec := range p.Decisions {
		if dec.Round == 0 {
			dec.Round = round
		}
		if hasDecision(out.Decisions, dec) {
			continue
		}
		out.Decisions = append(out.Decisions, dec)
	}
	return out, nil
}

// clone 深拷贝：走一遍 JSON，省得逐字段手抄（字段随模板演进，手抄必漏）。
func (d *Document) clone() (*Document, error) {
	if d == nil {
		return &Document{}, nil
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("prd: 复制快照失败: %w", err)
	}
	var out Document
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("prd: 复制快照失败: %w", err)
	}
	return &out, nil
}

// upsert 按 key 合并：同 key 原位替换（保持原有顺序），新 key 追加在后。
// 空 key 的条目原样追加，交给结构校验去报「缺 id」—— 这里不替它兜底。
func upsert[T any](base, in []T, key func(T) string) []T {
	if len(in) == 0 {
		return base
	}
	idx := map[string]int{}
	for i, it := range base {
		if k := key(it); k != "" {
			idx[k] = i
		}
	}
	out := append([]T(nil), base...)
	for _, it := range in {
		k := key(it)
		if i, ok := idx[k]; ok && k != "" {
			out[i] = it
			continue
		}
		if k != "" {
			idx[k] = len(out)
		}
		out = append(out, it)
	}
	return out
}

func without[T any](base []T, keys []string, key func(T) string) []T {
	if len(keys) == 0 {
		return base
	}
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
	}
	out := base[:0:0]
	for _, it := range base {
		if !drop[key(it)] {
			out = append(out, it)
		}
	}
	return out
}

func hasDecision(list []Decision, d Decision) bool {
	for _, x := range list {
		if x.Kind == d.Kind && x.Question == d.Question && x.Decision == d.Decision {
			return true
		}
	}
	return false
}
