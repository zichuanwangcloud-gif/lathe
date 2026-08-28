package flow

import (
	"strings"
	"testing"

	"github.com/Clouditera/lathe/internal/store"
)

// TestChainLengthWarningsLinearChainExceedsDefault 覆盖 F3.3 在无 UI 场景
// 下的核心判定：1→2→3→4→5（深度 5）超过默认上限 4，应该只在第 5 个节点
// （链上唯一深度超限的节点）产生一条 warning，文案里带上节点标识、
// 实际深度与上限。
func TestChainLengthWarningsLinearChainExceedsDefault(t *testing.T) {
	nodes := []NodeInput{
		{IssueKey: "N-1"},
		{IssueKey: "N-2", DependsOnIndex: p(0)},
		{IssueKey: "N-3", DependsOnIndex: p(1)},
		{IssueKey: "N-4", DependsOnIndex: p(2)},
		{IssueKey: "N-5", DependsOnIndex: p(3)},
	}

	warnings := chainLengthWarnings(nodes, store.DefaultFlowMaxChainLength)
	if len(warnings) != 1 {
		t.Fatalf("应恰好产生 1 条 warning，得到 %d 条: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "N-5") {
		t.Errorf("warning 应指出第 5 个节点(N-5)，得到 %q", warnings[0])
	}
	if !strings.Contains(warnings[0], "5") {
		t.Errorf("warning 应包含实际链长度 5，得到 %q", warnings[0])
	}
	if !strings.Contains(warnings[0], "4") {
		t.Errorf("warning 应包含上限 4，得到 %q", warnings[0])
	}
}

// TestChainLengthWarningsShortChainNoWarning 覆盖"不超限不警告"这一半：
// 1→2→3（深度 3）不超过默认上限 4，不应该有任何 warning。
func TestChainLengthWarningsShortChainNoWarning(t *testing.T) {
	nodes := []NodeInput{
		{IssueKey: "S-1"},
		{IssueKey: "S-2", DependsOnIndex: p(0)},
		{IssueKey: "S-3", DependsOnIndex: p(1)},
	}

	warnings := chainLengthWarnings(nodes, store.DefaultFlowMaxChainLength)
	if len(warnings) != 0 {
		t.Fatalf("深度 3 不超过默认上限 4，不应有 warning，得到 %v", warnings)
	}
}

// TestChainLengthWarningsExactlyAtLimitNoWarning 覆盖边界：深度恰好等于
// 上限时不算超限（"超过"是严格大于，不是大于等于）。
func TestChainLengthWarningsExactlyAtLimitNoWarning(t *testing.T) {
	nodes := []NodeInput{
		{IssueKey: "E-1"},
		{IssueKey: "E-2", DependsOnIndex: p(0)},
		{IssueKey: "E-3", DependsOnIndex: p(1)},
		{IssueKey: "E-4", DependsOnIndex: p(2)},
	}

	warnings := chainLengthWarnings(nodes, store.DefaultFlowMaxChainLength)
	if len(warnings) != 0 {
		t.Fatalf("深度恰好等于上限 4，不应有 warning，得到 %v", warnings)
	}
}

// TestChainLengthWarningsCustomLowerLimit 覆盖"上限可配置"这条纯逻辑
// 的一半（另一半——真的从 system_settings 读取——见 service_test.go 里
// 连库的测试）：把上限收紧到 2 后，1→2→3 这条深度 3 的链应在第 3 个节点
// 产生 warning。
func TestChainLengthWarningsCustomLowerLimit(t *testing.T) {
	nodes := []NodeInput{
		{IssueKey: "C-1"},
		{IssueKey: "C-2", DependsOnIndex: p(0)},
		{IssueKey: "C-3", DependsOnIndex: p(1)},
	}

	warnings := chainLengthWarnings(nodes, 2)
	if len(warnings) != 1 {
		t.Fatalf("上限收紧到 2 后应产生 1 条 warning，得到 %d 条: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "C-3") {
		t.Errorf("warning 应指出第 3 个节点(C-3)，得到 %q", warnings[0])
	}
}

// TestChainLengthWarningsMultipleIndependentRootsOnlyFlagsExceeding 覆盖
// M1 出口条件同款图形状：1→2→3→4→5 / 6（独立根）混批提交时，只有真正
// 超限的节点才产生 warning，独立根与短链不受影响。
func TestChainLengthWarningsMultipleIndependentRootsOnlyFlagsExceeding(t *testing.T) {
	nodes := []NodeInput{
		{IssueKey: "M-1"},
		{IssueKey: "M-2", DependsOnIndex: p(0)},
		{IssueKey: "M-3", DependsOnIndex: p(1)},
		{IssueKey: "M-4", DependsOnIndex: p(2)},
		{IssueKey: "M-5", DependsOnIndex: p(3)}, // 深度 5，超限
		{IssueKey: "M-6"},                       // 独立根，深度 1
	}

	warnings := chainLengthWarnings(nodes, store.DefaultFlowMaxChainLength)
	if len(warnings) != 1 {
		t.Fatalf("应恰好产生 1 条 warning，得到 %d 条: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "M-5") {
		t.Errorf("warning 应指出第 5 个节点(M-5)，得到 %q", warnings[0])
	}
}
