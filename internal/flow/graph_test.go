package flow

import (
	"errors"
	"reflect"
	"testing"
)

func idx(i int) *int { return &i }

// TestNodeDependsOnIndexIsScalarNotSlice 用反射钉死 Node.DependsOnIndex
// 是标量 *int 而非切片。
//
// "入度 ≤ 1" 这条 F1.2 规则在当前实现里不需要任何校验代码——它是
// DependsOnIndex 只有一个字段、不是列表这一数据结构事实的直接结果。
// 但这类"靠数据结构成立"的约束最怕的就是将来有人为了支持多前驱把
// 字段悄悄改成 []int：改动本身可能编译通过、看起来无害，实际却让
// 入度 ≤ 1 的保证不声不响地消失。这个测试就是那道"改了字段类型
// 就必须有人看见并做决定"的绊线。
func TestNodeDependsOnIndexIsScalarNotSlice(t *testing.T) {
	field, ok := reflect.TypeOf(Node{}).FieldByName("DependsOnIndex")
	if !ok {
		t.Fatal("Node 应有 DependsOnIndex 字段")
	}
	ft := field.Type
	if ft.Kind() != reflect.Ptr || ft.Elem().Kind() != reflect.Int {
		t.Fatalf(
			"Node.DependsOnIndex 必须是标量 *int（每个节点最多一个依赖），"+
				"得到 %v —— 这个约束保证了入度 <= 1 不需要额外校验代码；"+
				"如果确实要支持多前驱，请同步在 graph.go 补上显式的入度校验，"+
				"不要让这条约束无声消失", ft)
	}
}

func TestValidateRejectsEmpty(t *testing.T) {
	if err := Validate(nil); !errors.Is(err, ErrEmpty) {
		t.Fatalf("空列表应拒绝，得到 %v", err)
	}
	if err := Validate([]Node{}); !errors.Is(err, ErrEmpty) {
		t.Fatalf("空列表应拒绝，得到 %v", err)
	}
}

func TestValidateRejectsTooMany(t *testing.T) {
	nodes := make([]Node, MaxNodes+1)
	err := Validate(nodes)
	var tooMany ErrTooMany
	if !errors.As(err, &tooMany) {
		t.Fatalf("超过上限应返回 ErrTooMany，得到 %v", err)
	}
	if tooMany.Count != MaxNodes+1 || tooMany.Max != MaxNodes {
		t.Errorf("ErrTooMany 字段不对: %+v", tooMany)
	}
}

func TestValidateAcceptsAtExactlyMaxNodes(t *testing.T) {
	nodes := make([]Node, MaxNodes)
	if err := Validate(nodes); err != nil {
		t.Fatalf("恰好等于上限应放行，得到 %v", err)
	}
}

func TestValidateSingleRootNode(t *testing.T) {
	if err := Validate([]Node{{}}); err != nil {
		t.Fatalf("单节点无依赖应放行，得到 %v", err)
	}
}

func TestValidateChainedDependency(t *testing.T) {
	// 1 -> 2 -> 3：链式依赖，每个都指向刚好前一个
	nodes := []Node{
		{DependsOnIndex: nil},
		{DependsOnIndex: idx(0)},
		{DependsOnIndex: idx(1)},
	}
	if err := Validate(nodes); err != nil {
		t.Fatalf("链式依赖应放行，得到 %v", err)
	}
}

// TestValidateGraph1To2To3Plus4Plus5To6 覆盖 M1 出口条件描述的图形状：
// 1→2→3 / 4 / 5→6 —— 三个独立根（1,4,5），其中两条各自链式延伸。
func TestValidateGraph1To2To3Plus4Plus5To6(t *testing.T) {
	nodes := []Node{
		{},       // 1: 根
		{idx(0)}, // 2: 依赖 1
		{idx(1)}, // 3: 依赖 2
		{},       // 4: 根
		{},       // 5: 根
		{idx(4)}, // 6: 依赖 5
	}
	if err := Validate(nodes); err != nil {
		t.Fatalf("1→2→3 / 4 / 5→6 应放行，得到 %v", err)
	}
}

// TestValidateRejectsIndexPointingToSelf 对应 F1.2-AC3："提交时试图让下标
// 指向自己"被拒绝。在"按拓扑序提交 + 下标只能指向更早节点"这个 API 形状
// 下，"指向自己"和"成环"是同一件事的两种说法：一条从节点指向自身的边，
// 就是长度为 1 的环。这里没有做图遍历意义上的成环检测（找不到别的环，
// 因为下标不能指向"更晚"的节点，环无法在这个数据形状里形成），而是把
// 它降级成了一次"下标越界"校验——两者在效果上是等价的：都会在提交时被
// 拒绝，不会有任何行落库。
func TestValidateRejectsIndexPointingToSelf(t *testing.T) {
	nodes := []Node{
		{DependsOnIndex: idx(0)}, // 第 0 个节点指向自己
	}
	err := Validate(nodes)
	var invalid ErrInvalidIndex
	if !errors.As(err, &invalid) {
		t.Fatalf("指向自己应拒绝并返回 ErrInvalidIndex，得到 %v", err)
	}
	if invalid.NodeIndex != 0 || invalid.DependsOnIndex != 0 {
		t.Errorf("ErrInvalidIndex 字段不对: %+v", invalid)
	}
}

// TestValidateRejectsIndexPointingForward 对应 F1.2-AC3
// 的另一种"环变成非法下标"的场景：下标指向"之后"的节点。
// 如果这类声明被放行，两个互相指向对方的节点就能拼出一个双节点环；
// 拒绝"指向之后"因此等价于拒绝了这种环。
func TestValidateRejectsIndexPointingForward(t *testing.T) {
	nodes := []Node{
		{DependsOnIndex: idx(1)}, // 第 0 个节点指向第 1 个（之后）
		{DependsOnIndex: idx(0)},
	}
	err := Validate(nodes)
	var invalid ErrInvalidIndex
	if !errors.As(err, &invalid) {
		t.Fatalf("指向之后的节点应拒绝并返回 ErrInvalidIndex，得到 %v", err)
	}
	if invalid.NodeIndex != 0 || invalid.DependsOnIndex != 1 {
		t.Errorf("ErrInvalidIndex 字段不对: %+v", invalid)
	}
}

func TestValidateRejectsNegativeIndex(t *testing.T) {
	nodes := []Node{
		{DependsOnIndex: idx(-1)},
	}
	err := Validate(nodes)
	var invalid ErrInvalidIndex
	if !errors.As(err, &invalid) {
		t.Fatalf("负数下标应拒绝并返回 ErrInvalidIndex，得到 %v", err)
	}
}

func TestValidateRejectsIndexOutOfRangeBeyondLength(t *testing.T) {
	nodes := []Node{
		{},
		{DependsOnIndex: idx(5)}, // 批次里根本没有第 5 个节点
	}
	err := Validate(nodes)
	var invalid ErrInvalidIndex
	if !errors.As(err, &invalid) {
		t.Fatalf("越界下标应拒绝并返回 ErrInvalidIndex，得到 %v", err)
	}
	if invalid.NodeIndex != 1 || invalid.DependsOnIndex != 5 {
		t.Errorf("ErrInvalidIndex 字段不对: %+v", invalid)
	}
}

// TestChainDepthsLinearChain 覆盖 F3.3 链长约束的深度计算：1→2→3→4→5
// 这条 5 层链，每个节点的深度应该恰好是它在链上的位置（1-based）。
func TestChainDepthsLinearChain(t *testing.T) {
	nodes := []Node{
		{},       // 1: 深度 1
		{idx(0)}, // 2: 深度 2
		{idx(1)}, // 3: 深度 3
		{idx(2)}, // 4: 深度 4
		{idx(3)}, // 5: 深度 5
	}
	got := ChainDepths(nodes)
	want := []int{1, 2, 3, 4, 5}
	if len(got) != len(want) {
		t.Fatalf("深度切片长度应为 %d，得到 %d", len(want), len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个节点深度应为 %d，得到 %d", i, want[i], got[i])
		}
	}
}

// TestChainDepthsMultipleIndependentRoots 覆盖 M1 出口条件同款图形状：
// 1→2→3 / 4 / 5→6——独立根深度恒为 1，不受批次里其它链影响。
func TestChainDepthsMultipleIndependentRoots(t *testing.T) {
	nodes := []Node{
		{},       // 1: 根，深度 1
		{idx(0)}, // 2: 深度 2
		{idx(1)}, // 3: 深度 3
		{},       // 4: 根，深度 1
		{},       // 5: 根，深度 1
		{idx(4)}, // 6: 深度 2
	}
	got := ChainDepths(nodes)
	want := []int{1, 2, 3, 1, 1, 2}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个节点深度应为 %d，得到 %d", i, want[i], got[i])
		}
	}
}
