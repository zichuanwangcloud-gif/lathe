// Package tracker 定义「需求平台」的抽象：pipeline 对平台的全部消费面
// 就是本包的 Tracker 接口（两个方法）与 Issue/Comment 两个类型。
//
// 设计依据：docs/09-internal-issues.md §1.3 —— 内置工单体系就是给这个
// 窄接口写第二个实现（DB 支撑，见 local.go）；Linear 是第一个实现
// （internal/integration/linear 以类型别名适配，其包内部零改动）。
//
// 纪律（05-roadmap §0）：接口方法就是这两个，禁止在没有真实消费方之前
// 加新方法；07 F6.2 的 Caps() 能力位刻意不做——内置 tracker 全能力、
// 无 webhook、无凭据，等第二个外部平台（云效）探针落地时再谈。
package tracker

import (
	"context"
	"fmt"
	"strings"
)

// 需求来源平台的取值域，与 tasks.tracker_provider 的 CHECK 约束一致
// （migration 0021）。新增平台时两边一起改。
const (
	// ProviderLinear 需求载体是 Linear issue（external_key 形如 CR-1326，
	// external_id 是 Linear 的 issue UUID）。
	ProviderLinear = "linear"
	// ProviderInternal 需求载体是内置 issues 表（external_key 形如 LT-1042，
	// external_id 为 NULL——内置 tracker 按 (属主, key) 解析，不需要
	// 第二个标识）。
	ProviderInternal = "internal"
)

// Valid 报告 p 是否为已知平台标识。
func ValidProvider(p string) bool {
	return p == ProviderLinear || p == ProviderInternal
}

// Tracker 是 pipeline 对需求平台的窄接口。
//
// ref 的语义按实现而定：Linear 是 issue UUID（也接受 identifier）；
// 内置实现是工单 key（LT-1042）。
type Tracker interface {
	// Issue 拉取工单详情（标题、描述、评论），供分诊/续跑拼上下文。
	Issue(ctx context.Context, ref string) (*Issue, error)
	// Comment 在工单下追加一条评论（提问、失败说明、PR 链接回帖），
	// 返回评论 ID（不需要 ID 的实现可返回空串）。
	Comment(ctx context.Context, ref, body string) (string, error)
}

// AttributedTracker 是可携带「发言人」的 Tracker 扩展接口。
//
// 内置实现用它把 agent 的评论署名为 "task-<id>"（评论区据此渲染
// 「lathe · 任务 #id」并可跳任务详情）；Linear 没有这个概念（评论
// 身份就是 token 持有人），不实现它，pipeline 用类型断言可选消费。
type AttributedTracker interface {
	Tracker
	// WithActor 返回一个以 actor 署名评论的新句柄；原句柄不受影响。
	WithActor(actor string) Tracker
}

// Issue 是 Lathe 需要的工单字段（平台无关）。
//
// 这个类型住在 tracker 包而不是各平台自己的包里：它是 pipeline 与
// 平台实现之间的契约，两边都依赖它不构成环。
type Issue struct {
	ID          string
	Identifier  string // 形如 CR-1326（Linear）或 LT-1042（内置）
	Title       string
	Description string
	URL         string
	StateName   string
	Priority    int
	Labels      []string
	AssigneeID  string
	Comments    []Comment
}

// Comment 是工单下的一条评论。
type Comment struct {
	ID       string `json:"id"`
	Body     string `json:"body"`
	UserName string `json:"userName"`
}

// Context 把工单及其评论拼成交给 agent 的任务描述。
//
// 评论必须带上：真实工作中补充的复现步骤、澄清、变更要求
// 往往在评论里而不在正文里。
func (i Issue) Context() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s: %s\n\n", i.Identifier, i.Title)
	if len(i.Labels) > 0 {
		fmt.Fprintf(&b, "标签: %s\n", strings.Join(i.Labels, ", "))
	}
	if i.URL != "" {
		fmt.Fprintf(&b, "链接: %s\n", i.URL)
	}
	b.WriteString("\n## 描述\n\n")
	if strings.TrimSpace(i.Description) == "" {
		b.WriteString("（无描述）\n")
	} else {
		b.WriteString(strings.TrimSpace(i.Description) + "\n")
	}
	if len(i.Comments) > 0 {
		b.WriteString("\n## 评论\n\n")
		for _, c := range i.Comments {
			name := c.UserName
			if name == "" {
				name = "（未知）"
			}
			fmt.Fprintf(&b, "**%s**: %s\n\n", name, strings.TrimSpace(c.Body))
		}
	}
	return b.String()
}
