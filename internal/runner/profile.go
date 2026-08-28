package runner

import (
	"encoding/json"
	"fmt"
	"strings"
)

// profile.go 实现 F7.1（docs/07-prd-orchestration.md）：节点执行画像
// tasks.profile jsonb 列的解析与校验。task 包本身不理解 profile 的内部
// 结构（machine.go 的 Task.Profile 只是原始字节），解析交给上层——
// runner 包是消费方，理解每个字段该怎么用，因此 Profile 结构体的家
// 在这里。
//
// 字段范围严格按 docs/06-orchestration.md §6.1 与 07 §F7.1 的 AC 收紧：
// 只做三个有明确消费方的字段，不多加（roadmap §0 纪律：每个可配置字段
// 必须有消费方）。

// Profile 是节点执行画像，解析自 tasks.profile jsonb 列。
type Profile struct {
	// ModelChannel 非空时覆盖实现阶段（含修复回路）的 cc-switch 通道，
	// 不覆盖分诊阶段——分诊走便宜通道是既有设计意图，不该被节点画像
	// 绕开。消费方：pipeline.go 的 runAgent。
	ModelChannel string `json:"model_channel,omitempty"`
	// VerifyTier 非空时覆盖自动定档（light|heavy）。优先级高于仓库级的
	// RepoConfig.VerifyTierOverride，但低于断点续跑沿用的首次定档
	// （rc.tier）。消费方：pipeline.go 的 stageVerify。
	VerifyTier string `json:"verify_tier,omitempty"`
	// Skills 是节点执行画像声明的技能列表；F7.2（internal/runner/
	// skills.go 的 materializeSkills）在 stageImplement 里逐个物化进
	// worktree 的 .claude/skills/，声明了不存在的技能时任务以
	// StageSkillMissing 失败（F7.2-AC5）。
	Skills []SkillRef `json:"skills,omitempty"`
}

// SkillRef 是一个技能引用：按名字 + 版本声明，不存本机目录路径
// （F7.2-AC4 的要求，提前落进结构体形状，即使本阶段还不消费它）。
type SkillRef struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ParseProfile 解析 tasks.profile 的原始 jsonb 字节。
//
// raw 为空、或恰好是 "{}"/"null" 时返回零值 &Profile{}，不报错——
// 这对应 F7.1-AC4「未设画像的节点行为与现在完全一致」：零值画像的每个
// 字段都是空，消费方看到空值时必须表现为「画像功能不存在」那样。
//
// JSON 本身解析失败（画像数据损坏）时返回清楚的错误，不吞掉——损坏的
// 画像不该被悄悄当成"没设"，那会在流水线更深处以更难懂的方式炸掉。
//
// VerifyTier 非空但不是 "light"/"heavy" 时也报错：这是数据校验，
// 不能放一个非法值进流水线后面才炸（stageVerify 会直接拿它当档位用）。
func ParseProfile(raw []byte) (*Profile, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "{}" || trimmed == "null" {
		return &Profile{}, nil
	}

	var p Profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("runner: 节点画像数据损坏，无法解析: %w", err)
	}

	if p.VerifyTier != "" && p.VerifyTier != string(TierLight) && p.VerifyTier != string(TierHeavy) {
		return nil, fmt.Errorf(
			"runner: 节点画像 verify_tier 取值非法: %q（只能是 %q 或 %q）",
			p.VerifyTier, TierLight, TierHeavy)
	}

	return &p, nil
}

// StageProfileInvalid 是节点画像校验失败的失败阶段代码：JSON 损坏，
// 或字段取值非法（如 verify_tier 不是 light/heavy）。与
// StageRebaseConflict 同理，不进 stage.go 的 label()/stageOrder() 表
// （两者的 default 分支已经兜得住未列出的 Stage），单独放在离消费方
// 最近的文件里。
const StageProfileInvalid Stage = "profile_invalid"
