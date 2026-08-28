package store

import (
	"context"
	"testing"
)

// resetFlowMaxChainLength 把 flow_max_chain_length 恢复成"从未配置过"
// 的状态；system_settings 是全局共享表，测试结束必须清掉，避免污染同一
// 次 go test 运行里其它测试看到的默认值。
func resetFlowMaxChainLength(t *testing.T, st *Store) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(),
			`DELETE FROM system_settings WHERE key = $1`, SettingFlowMaxChainLength)
	})
}

// TestFlowMaxChainLengthDefaultsWhenUnconfigured 覆盖 F3.3-AC2 的"未配置
// 用默认值"分支：system_settings 里没有 flow_max_chain_length 这一行时，
// 应回退到 DefaultFlowMaxChainLength（4），且不报错。
func TestFlowMaxChainLengthDefaultsWhenUnconfigured(t *testing.T) {
	st := testStore(t)
	resetFlowMaxChainLength(t, st)

	n, err := st.FlowMaxChainLength(context.Background())
	if err != nil {
		t.Fatalf("未配置时不应报错，得到 %v", err)
	}
	if n != DefaultFlowMaxChainLength {
		t.Errorf("未配置时应回退默认值 %d，得到 %d", DefaultFlowMaxChainLength, n)
	}
}

// TestFlowMaxChainLengthReadsConfiguredValue 覆盖 F3.3-AC2 的核心断言：
// 写入 flow_max_chain_length=2 后，读回来的应该是 2，不是默认值。
func TestFlowMaxChainLengthReadsConfiguredValue(t *testing.T) {
	st := testStore(t)
	resetFlowMaxChainLength(t, st)

	if err := st.SetSetting(context.Background(), SettingFlowMaxChainLength, "2"); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	n, err := st.FlowMaxChainLength(context.Background())
	if err != nil {
		t.Fatalf("读取应成功，得到 %v", err)
	}
	if n != 2 {
		t.Errorf("应读到配置值 2，得到 %d", n)
	}
}

// TestFlowMaxChainLengthFallsBackOnCorruptValue 覆盖"手工改库写坏了"
// 这种边缘情况：值不是合法正整数时回退默认值，不报错、不让建图功能
// 整体挂掉（与 PreviewThresholds 同一原则）。
func TestFlowMaxChainLengthFallsBackOnCorruptValue(t *testing.T) {
	st := testStore(t)
	resetFlowMaxChainLength(t, st)

	if err := st.SetSetting(context.Background(), SettingFlowMaxChainLength, "not-a-number"); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	n, err := st.FlowMaxChainLength(context.Background())
	if err != nil {
		t.Fatalf("损坏值不应报错，得到 %v", err)
	}
	if n != DefaultFlowMaxChainLength {
		t.Errorf("损坏值应回退默认值 %d，得到 %d", DefaultFlowMaxChainLength, n)
	}
}
