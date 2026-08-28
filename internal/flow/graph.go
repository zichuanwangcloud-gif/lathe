// Package flow 实现"一键批量入队"（PRD F1.4）与建图时的核心校验（F1.2）。
//
// 本文件只含纯逻辑：校验一批"待建节点"的依赖声明是否合法，不依赖数据库，
// 因此可被完整单测覆盖。落库与幂等见 service.go。
//
// 设计依据：docs/07-prd-orchestration.md F1.2/F1.4，docs/06-orchestration.md §1。
package flow

import "fmt"

// MaxNodes 是单批次允许建的节点数硬上限。
//
// 原调度器的"queueDepth=64 channel 已满即拒绝"语义随 DB 领单改造已经不
// 存在——领单不再经过内存 channel。这里改为按批次大小设上限，是同一防
// 护目的（不让单次请求建出海量任务行）在新架构下的等价物，而不是照搬
// 旧的 64 这个数字：批量建图的合理批次远小于调度器曾经的排队深度，
// 200 留了充分余量的同时仍能挡住"整仓库 issue 一次性灌进来"这种误用。
const MaxNodes = 200

// Node 是 Validate 所需的最小视图：一个待建节点的依赖声明。
//
// DependsOnIndex 是该节点依赖的前驱在本批次里的下标（0-based）；
// nil 表示独立根，无前驱。
//
// 契约：调用方必须按拓扑序提交节点，DependsOnIndex 只能指向"更早提交
// 的节点"（严格小于自己的下标）——不支持声明指向自己或之后的节点。
// 这个契约本身就排除了环的可能：图上每条边都从下标更大的节点指向
// 下标更小的节点，不存在能沿边走回起点的路径，因此不需要图遍历意义
// 上的成环检测（对应 F1.2-AC3；见本文件顶部与 graph_test.go 的等价性
// 说明）。
//
// 入度 ≤ 1（F1.2 另一条校验规则）由 DependsOnIndex 是标量 *int 而非切片
// 这一数据结构事实天然保证——每个节点只有一个"依赖谁"的字段可填。
// TestNodeDependsOnIndexIsScalarNotSlice 用反射钉死这一点，防止将来
// 有人把字段悄悄改成切片时无声破坏这个约束。
type Node struct {
	DependsOnIndex *int
}

// ErrEmpty 表示提交的节点列表为空。
var ErrEmpty = fmt.Errorf("flow: 节点列表不能为空")

// ErrTooMany 表示批次节点数超过 MaxNodes。
type ErrTooMany struct {
	Count int
	Max   int
}

func (e ErrTooMany) Error() string {
	return fmt.Sprintf("flow: 单批节点数 %d 超过上限 %d，请拆成多批提交", e.Count, e.Max)
}

// ErrInvalidIndex 表示某个节点的依赖下标非法。
//
// 非法的两种情形都落在这里：下标越界（指向不存在的节点）与下标指向
// "自己或之后"的节点（既是越界的一种，也是"环"在这个 API 形状下的
// 表现——见 Node 的文档注释）。
type ErrInvalidIndex struct {
	// NodeIndex 是出问题的节点在本批次里的下标。
	NodeIndex int
	// DependsOnIndex 是该节点声明的（非法的）依赖下标。
	DependsOnIndex int
}

func (e ErrInvalidIndex) Error() string {
	if e.NodeIndex == 0 {
		return fmt.Sprintf(
			"flow: 第 0 个节点不能声明依赖（它是批次里的第一个节点，没有更早提交的节点可依赖），"+
				"但它声明了 dependsOnIndex=%d", e.DependsOnIndex)
	}
	return fmt.Sprintf(
		"flow: 第 %d 个节点的 dependsOnIndex=%d 非法——必须指向 0..%d 之间"+
			"已提交的节点（按拓扑序提交，下标不能指向自己或更后面的节点）",
		e.NodeIndex, e.DependsOnIndex, e.NodeIndex-1)
}

// ChainDepths 计算每个节点在其所在依赖链上的深度（从 1 起；独立根深度
// 为 1，其余节点的深度 = 前驱深度 + 1）。
//
// 这是 F3.3（链长约束）的核心计算：链长超过配置上限时，CreateFlow
// （service.go）不拒绝创建，只把超限节点报成 warnings（F3.3-AC1 的
// "UI 警告"本次没有 UI，能落到的最大程度是 API 响应里能看到）。
//
// 前提：nodes 必须已经过 Validate——每个 DependsOnIndex 只能指向严格
// 更早的下标。因此按下标从小到大遍历一次即可：轮到第 i 个节点时，它
// 的前驱（下标严格小于 i）的深度必然已经算好。传入未经校验的 nodes
// （下标越界或指向自己/之后）会 panic。
func ChainDepths(nodes []Node) []int {
	depths := make([]int, len(nodes))
	for i, n := range nodes {
		if n.DependsOnIndex == nil {
			depths[i] = 1
			continue
		}
		depths[i] = depths[*n.DependsOnIndex] + 1
	}
	return depths
}

// Validate 校验一批待建节点的依赖声明。
//
// 规则：
//  1. 节点数必须在 (0, MaxNodes] 区间内；
//  2. 每个节点的 DependsOnIndex（非 nil 时）必须严格小于自己的下标，
//     且不小于 0——否则拒绝并说明是哪个节点、哪个非法下标。
func Validate(nodes []Node) error {
	if len(nodes) == 0 {
		return ErrEmpty
	}
	if len(nodes) > MaxNodes {
		return ErrTooMany{Count: len(nodes), Max: MaxNodes}
	}
	for i, n := range nodes {
		if n.DependsOnIndex == nil {
			continue
		}
		idx := *n.DependsOnIndex
		if idx < 0 || idx >= i {
			return ErrInvalidIndex{NodeIndex: i, DependsOnIndex: idx}
		}
	}
	return nil
}
