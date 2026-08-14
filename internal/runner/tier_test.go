package runner

import (
	"strings"
	"testing"
)

func TestClassifyTier_OverrideWins(t *testing.T) {
	// 强制 light：即便改动碰了 .go 也按 light 归档
	tier, reasons := ClassifyTier([]string{"internal/api/server.go"}, TierLight)
	if tier != TierLight {
		t.Fatalf("override light 未生效，得到 %s", tier)
	}
	if len(reasons) == 0 || !strings.Contains(reasons[0], "强制指定") {
		t.Fatalf("override 理由缺失: %v", reasons)
	}

	// 强制 heavy：即便纯 CSS 改动也按 heavy 归档
	tier, _ = ClassifyTier([]string{"web/src/style.css"}, TierHeavy)
	if tier != TierHeavy {
		t.Fatalf("override heavy 未生效，得到 %s", tier)
	}
}

func TestClassifyTier_FrontendOnlyIsLight(t *testing.T) {
	cases := [][]string{
		{"web/src/App.vue"},
		{"web/src/style.css", "web/src/theme.scss"},
		{"web/src/components/Form.tsx", "web/src/locales/zh-CN.json"},
		{"docs/usage.md", "web/src/i18n/messages.ts"},
		{"web/README.mdx"},
	}
	for _, files := range cases {
		tier, reasons := ClassifyTier(files, "")
		if tier != TierLight {
			t.Errorf("%v 应归 light，得到 %s（%v）", files, tier, reasons)
		}
	}
}

func TestClassifyTier_BackendIsHeavy(t *testing.T) {
	cases := [][]string{
		{"internal/api/server.go"},                      // Go 后端
		{"web/src/api/client.ts"},                       // TS 逻辑（非展示层）
		{"web/src/utils.ts"},                            // 前端工具逻辑同样不是展示层
		{"package.json"},                                // 依赖清单影响构建与运行
		{"internal/store/query.go", "web/src/App.vue"},  // 跨越前后端
		{"scripts/deploy.sh"},                           // 运维脚本
		{"web/src/App.vue", "migrations/0007_x.up.sql"}, // 碰到 migration
		{"internal/billing/calc.go"},                    // 计费路径
		{"server/schema/user.proto"},                    // schema 路径
		{"web/src/views/payment/Panel.vue"},             // 计费目录下的组件也算
	}
	for _, files := range cases {
		tier, reasons := ClassifyTier(files, "")
		if tier != TierHeavy {
			t.Errorf("%v 应归 heavy，得到 %s", files, tier)
		}
		if len(reasons) < 2 {
			t.Errorf("%v 归 heavy 但未列出文件理由: %v", files, reasons)
		}
	}
}

func TestClassifyTier_UnknownDefaultsHeavy(t *testing.T) {
	// 无清单：保守归 heavy
	if tier, _ := ClassifyTier(nil, ""); tier != TierHeavy {
		t.Fatalf("空清单应保守归 heavy，得到 %s", tier)
	}
	// 拿不准的扩展名：保守归 heavy
	if tier, _ := ClassifyTier([]string{"config/settings.toml"}, ""); tier != TierHeavy {
		t.Fatalf("未识别的文件类型应归 heavy")
	}
}

func TestClassifyTier_WindowsPathNormalized(t *testing.T) {
	if tier, _ := ClassifyTier([]string{`web\src\App.vue`}, ""); tier != TierLight {
		t.Fatalf("反斜杠路径应被归一化后判定为 light")
	}
}

func TestOverrideTier(t *testing.T) {
	for in, want := range map[string]VerifyTier{
		"light":  TierLight,
		"HEAVY":  TierHeavy,
		" heavy": TierHeavy,
		"":       "",
		"auto":   "",
		"weird":  "",
	} {
		if got := OverrideTier(in); got != want {
			t.Errorf("OverrideTier(%q) = %q，期望 %q", in, got, want)
		}
	}
}
