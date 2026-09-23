package prd

import (
	"encoding/json"
	"fmt"
	"sort"
)

// 本文件是 §8 任务拆分的结构化契约（docs/10 §4.1）。
//
// 为什么必须结构化：一键生成是确定性操作，不再经过一次 LLM 解释 ——
// 与 .lathe/repro.json「声明优先于猜测」同一哲学。让智能体输出 Markdown
// 再解析回来，等于把「人已确认的拆分」重新交给概率模型复述一遍。

// TaskKind 是生成出来的正式任务类型，取值与 tasks.task_kind 一致。
type TaskKind string

const (
	// KindFix 缺陷修复：进 heavy 档必须交红-绿复现证明。
	KindFix TaskKind = "fix"
	// KindFeature 功能。
	KindFeature TaskKind = "feature"
	// KindHotfix 热修。
	KindHotfix TaskKind = "hotfix"
)

// Valid 报告任务类型是否已知。
func (k TaskKind) Valid() bool {
	switch k {
	case KindFix, KindFeature, KindHotfix:
		return true
	}
	return false
}

// Estimate 是智能体读代码后的改动量估算，用于 §4.2 的量级自检。
//
// 会有误差，估错最坏是一个偏大的 PR（D10-5 承认的代价）；真实定档仍在
// diff 产出后（02 §5.1），这里只用来在拆分阶段拦住明显过大的任务。
type Estimate struct {
	Lines int `json:"lines"`
	Files int `json:"files"`
}

// TaskBlock 是 §8 的一个任务，也是一键生成时的一个图节点。
type TaskBlock struct {
	// Key 在本 PRD 内唯一（T1、T2…），DependsOn 用它引用前驱。
	Key   string   `json:"key"`
	Title string   `json:"title"`
	Kind  TaskKind `json:"kind"`

	// DependsOn 为空表示根节点。入度 ≤ 1 是编排图的硬约束（07 §F1.2）：
	// depends_on 是单值列，所以一个任务只能有一个前驱，图是森林。
	DependsOn string `json:"dependsOn,omitempty"`

	// Description 要写到能过分诊：目标、涉及范围、**必须交付的复现/验收
	// 测试是什么**。最后这项不是客套 —— 红-绿判决按任务下，description
	// 没写清要交什么测试，实现 agent 就会自己猜。
	Description string `json:"description"`

	// FilesHint 是参考，不约束实现 agent；生成任务时以「参考文件」段落
	// 进正文。写死文件清单反而会把 agent 框在错的地方。
	FilesHint []string `json:"filesHint,omitempty"`

	// Acceptance 是这个任务交付的 AC 编号，非空且必须都存在（§5.1 闭环）。
	Acceptance []string `json:"acceptance"`

	// VerifyHint 仅供人看工作量（light/heavy），真实定档在 diff 后。
	VerifyHint string   `json:"verifyHint,omitempty"`
	Estimate   Estimate `json:"estimate"`
}

// 验证档位提示的取值。
const (
	HintLight = "light"
	HintHeavy = "heavy"
)

// TaskBlocks 是一份 PRD 的全部任务块，带仓库信息，整体存 prds.task_blocks。
type TaskBlocks struct {
	Version int         `json:"version"`
	Repo    string      `json:"repo"`
	Tasks   []TaskBlock `json:"tasks"`
}

// BlocksVersion 是结构块的当前格式版本。
//
// 存下来是为了将来改形状时能识别老数据：approved 的 PRD 不可变，但它的
// task_blocks 可能在格式演进后还要被读（导出、追溯）。
const BlocksVersion = 1

// Marshal 序列化为 jsonb 字节。
func (b *TaskBlocks) Marshal() (json.RawMessage, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// ParseTaskBlocks 从库里的 jsonb 还原。空输入返回零值（还没拆分）。
func ParseTaskBlocks(raw json.RawMessage) (*TaskBlocks, error) {
	var b TaskBlocks
	if len(raw) == 0 {
		return &b, nil
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// ByKey 建立 key → 任务的索引，便于校验引用。
func (b *TaskBlocks) ByKey() map[string]*TaskBlock {
	out := make(map[string]*TaskBlock, len(b.Tasks))
	for i := range b.Tasks {
		out[b.Tasks[i].Key] = &b.Tasks[i]
	}
	return out
}

// Roots 返回无前驱的任务（并行起跑的那批），按 key 排序保证稳定。
func (b *TaskBlocks) Roots() []string {
	var out []string
	for _, t := range b.Tasks {
		if t.DependsOn == "" {
			out = append(out, t.Key)
		}
	}
	sort.Strings(out)
	return out
}

// ChainDepth 返回从根到每个任务的链长（根为 1）。
//
// 有环时返回错误 —— 环会让「等前驱合并」永远等不到，是死锁而非慢。
func (b *TaskBlocks) ChainDepth() (map[string]int, error) {
	index := b.ByKey()
	depth := make(map[string]int, len(b.Tasks))

	var walk func(key string, seen map[string]bool) (int, error)
	walk = func(key string, seen map[string]bool) (int, error) {
		if d, ok := depth[key]; ok {
			return d, nil
		}
		if seen[key] {
			return 0, fmt.Errorf("prd: 任务依赖成环（经过 %s）", key)
		}
		seen[key] = true

		t := index[key]
		if t == nil {
			return 0, fmt.Errorf("prd: 任务 %s 不存在", key)
		}
		if t.DependsOn == "" {
			depth[key] = 1
			return 1, nil
		}
		parent, err := walk(t.DependsOn, seen)
		if err != nil {
			return 0, err
		}
		depth[key] = parent + 1
		return depth[key], nil
	}

	for _, t := range b.Tasks {
		if _, err := walk(t.Key, map[string]bool{}); err != nil {
			return nil, err
		}
	}
	return depth, nil
}

// MaxChainDepth 返回最长链长，供与仓库上限（默认 4）比较。
func (b *TaskBlocks) MaxChainDepth() (int, error) {
	depth, err := b.ChainDepth()
	if err != nil {
		return 0, err
	}
	max := 0
	for _, d := range depth {
		if d > max {
			max = d
		}
	}
	return max, nil
}
