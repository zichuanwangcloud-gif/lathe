package prd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// 本文件是一键生成（docs/10 §4.3）：把签过字的 PRD 落成内置工单 + 编排图。
//
// 这是 PRD 体系与既有任务体系唯一的衔接点。在此之前这一步是断的 ——
// prds 表早有 generated_flow_id / converted_at 两列，却没有任何代码写它们，
// PRD 走到 approved 就没有下文。
//
// 与 Worktrees 同一手法：只声明「建工单」「建图」两个窄接口，真实实现
// （store.CreateIssue / flow.Service.CreateFlow）由 cmd 层适配进来。这样
// prd 包不依赖 flow / task，而拓扑排序、正文渲染、AC 回填这些真正容易
// 写错的逻辑全都能脱离数据库单测。

// IssueSpec 是要建的一张内置工单。
type IssueSpec struct {
	UserID      int64
	RepoID      int64
	Title       string
	Description string
}

// NodeSpec 是编排图里的一个节点。
//
// 只带 IssueKey：一键生成出来的全是内置工单，没有 Linear 那边的 UUID。
type NodeSpec struct {
	IssueKey string
	Title    string
	// DependsOnIndex 指向本批次里更早的节点下标，nil 表示根节点。
	DependsOnIndex *int
}

// FlowSpec 是建一整张图的入参。
type FlowSpec struct {
	UserID int64
	RepoID int64
	Name   string
	Nodes  []NodeSpec
}

// CreatedTask 是生成出来的一个任务的精简视图。
type CreatedTask struct {
	ID       int64  `json:"id"`
	IssueKey string `json:"issueKey"`
	State    string `json:"state"`
}

// FlowResult 是一键生成的结果。
type FlowResult struct {
	FlowID   int64         `json:"flowId"`
	Tasks    []CreatedTask `json:"tasks"`
	Warnings []string      `json:"warnings"`
}

// Issues 是建内置工单的能力。返回工单 key（LT-n）。
type Issues interface {
	Create(ctx context.Context, s IssueSpec) (key string, err error)
}

// Flows 是建编排图的能力。
type Flows interface {
	Create(ctx context.Context, s FlowSpec) (*FlowResult, error)
}

// ErrNotApproved 表示 PRD 还没签字，不能生成任务。
var ErrNotApproved = errors.New("prd: 只有 approved 的 PRD 能一键生成任务")

// ErrAlreadyConverted 表示这份 PRD 已经生成过任务。
var ErrAlreadyConverted = errors.New("prd: 该 PRD 已生成过任务")

// ErrNoTasks 表示任务块为空。
var ErrNoTasks = errors.New("prd: 任务块为空，没有可生成的任务")

// ErrConvertUnavailable 表示一键生成没装配好。
var ErrConvertUnavailable = errors.New("prd: 一键生成未装配")

// Convert 把 approved 的 PRD 落成内置工单与编排图（docs/10 §4.3）。
//
// 每个任务块建一张内置工单，工单正文带上它要交付的 AC **全文**，然后
// 把这批工单按依赖关系连成一张图入队。
func (s *Service) Convert(ctx context.Context, prdID, userID int64) (*FlowResult, error) {
	if s.Issues == nil || s.Flows == nil {
		return nil, ErrConvertUnavailable
	}

	row, err := s.Store.GetPRD(ctx, prdID, userID)
	if err != nil {
		return nil, err
	}
	if row.State != string(StateApproved) {
		return nil, fmt.Errorf("%w：当前状态 %s", ErrNotApproved, row.State)
	}
	if row.GeneratedFlowID != nil {
		return nil, fmt.Errorf("%w（编排图 #%d）", ErrAlreadyConverted, *row.GeneratedFlowID)
	}

	doc, err := ParseDocument(row.Document)
	if err != nil {
		return nil, fmt.Errorf("prd: 解析快照失败: %w", err)
	}
	blocks, err := ParseTaskBlocks(row.TaskBlocks)
	if err != nil {
		return nil, fmt.Errorf("prd: 解析任务块失败: %w", err)
	}
	if blocks == nil || len(blocks.Tasks) == 0 {
		return nil, ErrNoTasks
	}
	ordered, err := topoOrder(blocks)
	if err != nil {
		return nil, err
	}

	// 先 CAS 占住 converted，再建工单与图。
	//
	// 顺序是刻意的：converted 是终态，CAS 成功等于拿到本次生成的独占权，
	// 并发的第二个请求会在这里就失败，而不是去建第二批任务。反过来「先
	// 建图再 CAS」，两个请求会各建一批真的会跑起来的任务，然后其中一个
	// 才发现自己输了 —— 代价是同一份需求被执行两遍，比多一条孤儿记录
	// 严重得多。
	//
	// 代价是建图失败时 PRD 已经离开 approved，所以下面跟着补偿回滚。
	if _, err := s.Store.TransitionPRD(ctx, prdID, userID, store.TransitionPRDParams{
		From: string(StateApproved),
		To:   string(StateConverted),
	}); err != nil {
		return nil, err
	}

	res, err := s.buildFlow(ctx, row, doc, ordered)
	if err != nil {
		// 补偿：把 PRD 放回 approved，让人能重试。
		//
		// 直接用 store 的 CAS 而不走 Validate —— converted → approved 不是
		// 合法的产品转移（D10-4 的冻结语义），它只是这次失败生成的撤销动作，
		// 不该进转移表被别处复用。
		//
		// 用 WithoutCancel：请求已经被取消也要把状态放回去，否则 PRD 卡在
		// converted 却没有图，人既不能重试也不能改（approved 之后已冻结）。
		if _, rerr := s.Store.TransitionPRD(context.WithoutCancel(ctx), prdID, userID,
			store.TransitionPRDParams{
				From: string(StateConverted),
				To:   string(StateApproved),
			}); rerr != nil {
			slog.Error("一键生成失败后回滚 PRD 状态也失败，PRD 卡在 converted",
				"prd", prdID, "err", rerr)
		}
		return nil, err
	}

	// 回填图号。
	//
	// 这一步失败不算生成失败：任务与图都已经真的建出来了，回滚它们比留
	// 一个没记住图号的 PRD 危险得多（任务可能已经被领走开跑）。记 error
	// 日志，人能从 flows 表按 repo/时间找回来。
	if _, err := s.Store.TransitionPRD(ctx, prdID, userID, store.TransitionPRDParams{
		From:            string(StateConverted),
		To:              string(StateConverted),
		GeneratedFlowID: &res.FlowID,
	}); err != nil {
		slog.Error("回填 generated_flow_id 失败（任务已生成，不影响执行）",
			"prd", prdID, "flow", res.FlowID, "err", err)
	}

	return res, nil
}

// buildFlow 逐个建工单，再把它们连成图。
func (s *Service) buildFlow(ctx context.Context, row *store.PRDRow, doc *Document, ordered []*TaskBlock) (*FlowResult, error) {
	indexOf := make(map[string]int, len(ordered))
	nodes := make([]NodeSpec, 0, len(ordered))

	for i, b := range ordered {
		key, err := s.Issues.Create(ctx, IssueSpec{
			UserID:      row.UserID,
			RepoID:      row.RepoID,
			Title:       b.Title,
			Description: renderIssueBody(row.ID, b, doc),
		})
		if err != nil {
			return nil, fmt.Errorf("prd: 为任务块 %s 建工单失败: %w", b.Key, err)
		}

		var dep *int
		if b.DependsOn != "" {
			idx, ok := indexOf[b.DependsOn]
			if !ok {
				// 拓扑序保证前驱已经处理过；走到这里说明 DependsOn 指向
				// 一个不存在的 key —— 自检清单本该拦下（CodeTaskCycle 之外
				// 的悬空引用），这里兜底成明确错误而不是建出半张图。
				return nil, fmt.Errorf("prd: 任务块 %s 依赖的 %s 不存在", b.Key, b.DependsOn)
			}
			d := idx
			dep = &d
		}
		indexOf[b.Key] = i
		nodes = append(nodes, NodeSpec{IssueKey: key, Title: b.Title, DependsOnIndex: dep})
	}

	return s.Flows.Create(ctx, FlowSpec{
		UserID: row.UserID,
		RepoID: row.RepoID,
		Name:   flowName(row.ID, doc),
		Nodes:  nodes,
	})
}

// topoOrder 把任务块排成合法的提交顺序：前驱一定排在后继前面。
//
// 直接复用 ChainDepth —— 入度 ≤ 1 的森林里按链深度升序排就是一个合法
// 拓扑序，而且它自带环检测（有环原样上抛：环会让「等前驱放行」永远等
// 不到，是死锁不是慢）。
//
// 同深度按 Key 排序，保证同一份 PRD 每次生成的节点顺序一致 —— 顺序不
// 稳定会让失败重试后的图下标对不上，排查时白费功夫。
func topoOrder(b *TaskBlocks) ([]*TaskBlock, error) {
	depth, err := b.ChainDepth()
	if err != nil {
		return nil, err
	}
	out := make([]*TaskBlock, 0, len(b.Tasks))
	for i := range b.Tasks {
		out = append(out, &b.Tasks[i])
	}
	sort.SliceStable(out, func(i, j int) bool {
		di, dj := depth[out[i].Key], depth[out[j].Key]
		if di != dj {
			return di < dj
		}
		return out[i].Key < out[j].Key
	})
	return out, nil
}

// flowName 给生成的图起名：PRD 的一句话 + 编号。
//
// 截断按 rune 不按 byte —— 按 byte 切会把多字节字符切成半个，正是
// AGENTS.md §9 里那条「事件落库 UTF-8 截断整批丢弃」的形状。
func flowName(prdID int64, doc *Document) string {
	title := strings.TrimSpace(doc.OneLiner.Body)
	if r := []rune(title); len(r) > 48 {
		title = string(r[:48]) + "…"
	}
	if title == "" {
		return fmt.Sprintf("PRD #%d", prdID)
	}
	return fmt.Sprintf("PRD #%d %s", prdID, title)
}

// renderIssueBody 把一个任务块渲染成工单正文。
//
// 关键是把这个任务要交付的 AC **全文**抄进来，而不是只留编号：实现
// agent 拿到的是工单，它看不到 PRD。正文里只写「满足 AC-3」等于没写，
// agent 只能自己猜要交什么测试 —— 而红-绿判决是按任务下的。
func renderIssueBody(prdID int64, b *TaskBlock, doc *Document) string {
	var sb strings.Builder

	if d := strings.TrimSpace(b.Description); d != "" {
		sb.WriteString(d)
		sb.WriteString("\n\n")
	}

	sb.WriteString("## 必须交付的验收标准\n\n")
	if len(b.Acceptance) == 0 {
		// 定稿自检要求每个任务至少交付一条 AC（§5.1 闭环），走到这里说明
		// 数据是从别的路径进来的。不静默略过：正文里留下痕迹，人能看见。
		sb.WriteString("_本任务没有关联任何 AC —— 这是定稿自检本该拦下的情况，请回查 PRD。_\n\n")
	}

	crit := make(map[string]*Criterion, len(doc.Criteria))
	for i := range doc.Criteria {
		crit[doc.Criteria[i].ID] = &doc.Criteria[i]
	}
	for _, id := range b.Acceptance {
		c, ok := crit[id]
		if !ok {
			fmt.Fprintf(&sb, "### %s\n\n_PRD 快照里找不到这条 AC，请回查。_\n\n", id)
			continue
		}
		renderCriterion(&sb, c)
	}

	if len(b.FilesHint) > 0 {
		sb.WriteString("## 参考文件\n\n")
		sb.WriteString("（仅供起步参考，不约束实现范围）\n\n")
		for _, f := range b.FilesHint {
			fmt.Fprintf(&sb, "- `%s`\n", f)
		}
		sb.WriteString("\n")
	}

	fmt.Fprintf(&sb, "---\n\n本工单由 PRD #%d 的任务块 `%s` 一键生成（类型 %s，估算 %d 行 / %d 文件）。\n",
		prdID, b.Key, b.Kind, b.Estimate.Lines, b.Estimate.Files)
	sb.WriteString("需求有变请新开一份 PRD 引用原件，不要直接改本工单 —— 已签字的 PRD 是冻结的。\n")

	return sb.String()
}

// renderCriterion 渲染一条 AC。
func renderCriterion(sb *strings.Builder, c *Criterion) {
	fmt.Fprintf(sb, "### %s（%s）\n\n", c.ID, c.Category)

	if c.NA {
		fmt.Fprintf(sb, "不适用：%s\n\n", c.Reason)
		return
	}

	if c.Given != "" {
		fmt.Fprintf(sb, "- **Given** %s\n", c.Given)
	}
	if c.When != "" {
		fmt.Fprintf(sb, "- **When** %s\n", c.When)
	}
	if c.Then != "" {
		fmt.Fprintf(sb, "- **Then** %s\n", c.Then)
	}
	if c.Evidence != "" {
		fmt.Fprintf(sb, "- **判据** %s\n", c.Evidence)
	}
	if c.Verify != "" {
		fmt.Fprintf(sb, "- **验证方式** %s\n", c.Verify)
	}
	if c.ManualSteps != "" {
		fmt.Fprintf(sb, "- **人工步骤** %s\n", c.ManualSteps)
	}

	for _, ex := range c.Examples {
		label := "反例"
		if ex.Positive {
			label = "正例"
		}
		fmt.Fprintf(sb, "- **%s** %s", label, ex.Text)
		if ex.Verdict != "" {
			fmt.Fprintf(sb, "（人判定：%s）", ex.Verdict)
		}
		sb.WriteString("\n")
	}

	sb.WriteString("\n")
}
