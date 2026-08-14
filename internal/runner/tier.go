package runner

import (
	"path"
	"sort"
	"strings"
)

// tier.go 实现 docs/02-design.md §5.1 的档位路由：
// 判定时机放在 agent 改完代码之后，依据是实际改动面而不是 issue 文本。
//
// 归档规则（确定性，不靠猜）：
//   - 仓库配置强制指定 ⇒ 覆盖一切（OverrideTier）
//   - diff 只碰前端展示层（组件/样式/文案/i18n/文档）⇒ light
//   - 碰到 API / service / DB migration / 计费逻辑 ⇒ heavy
//   - 跨越前后端 ⇒ heavy（自然蕴含于上一条：并非全部文件都是展示层）
//
// lightEligible 判定刻意保守：拿不准的一律归 heavy。错归 light 会把
// 没经过红-绿证明的改动放进 PR，错归 heavy 只是多跑一轮验证 ——
// 不对称的代价决定了保守的方向。

// OverrideTier 是仓库配置里的强制档位（repos.verify_tier_override）。
// 返回归一化后的值；空串表示未指定。
func OverrideTier(s string) VerifyTier {
	switch VerifyTier(strings.ToLower(strings.TrimSpace(s))) {
	case TierLight:
		return TierLight
	case TierHeavy:
		return TierHeavy
	default:
		return ""
	}
}

// heavyPathSignals 命中即归 heavy 的路径关键词（小写匹配）。
var heavyPathSignals = []string{
	// DB migration（docs/02-design.md §5.1 明确点名）
	"migration", "migrations", "db/migrate", "schema",
	// 计费逻辑（同上）
	"billing", "payment", "invoice", "pricing", "计费",
	// API / service 层常见目录
	"api/", "/api/", "service/", "/service/", "services/", "rpc/", "grpc",
}

// lightExts 是前端展示层与文案类文件的扩展名。
var lightExts = map[string]bool{
	".tsx": true, ".jsx": true, ".vue": true, ".svelte": true,
	".css": true, ".scss": true, ".sass": true, ".less": true,
	".md": true, ".mdx": true, ".txt": true,
}

// i18nPathSignals 是文案/i18n 文件的路径特征；命中后再看扩展名。
var i18nPathSignals = []string{"i18n", "l10n", "locale", "locales", "messages"}

// i18nExts 是 i18n 资源文件可用的扩展名。
var i18nExts = map[string]bool{
	".json": true, ".ts": true, ".js": true, ".yaml": true, ".yml": true, ".po": true, ".ftl": true,
}

// ClassifyTier 按改动文件清单判定验证档位。
//
// files 是 diff 产出的相对路径清单（任务分支 vs 基线）。
// override 是仓库配置里的强制档位（空串表示未指定），命中即返回。
//
// 返回档位与归档理由，理由会写进任务事件与 PR 描述，让人能复核
// 归档是否符实 —— 档位判定是验证强度的闸门，必须可审计。
func ClassifyTier(files []string, override VerifyTier) (VerifyTier, []string) {
	if override == TierLight || override == TierHeavy {
		return override, []string{"仓库配置强制指定为 " + string(override)}
	}

	if len(files) == 0 {
		// 没有清单就没有证据，保守归 heavy —— 宁可多验证不可少验证
		return TierHeavy, []string{"无改动文件清单，保守归档 heavy"}
	}

	var heavy []string
	for _, f := range files {
		if !lightEligible(f) {
			heavy = append(heavy, f)
		}
	}
	if len(heavy) == 0 {
		return TierLight, []string{"改动仅触及前端展示层/文案/i18n/文档"}
	}

	sort.Strings(heavy)
	reasons := []string{"改动触及展示层之外的文件，归档 heavy："}
	for _, f := range heavy {
		reasons = append(reasons, "  - "+f)
	}
	return TierHeavy, reasons
}

// lightEligible 报告单个文件是否属于「前端展示层/文案」。
func lightEligible(file string) bool {
	p := strings.ToLower(strings.TrimSpace(filepathSlash(file)))
	if p == "" {
		return false
	}

	// heavy 信号优先： migrations / 计费 / API / service 目录下的
	// 任何文件（哪怕是 .md 说明）都让整单归 heavy
	for _, sig := range heavyPathSignals {
		if strings.Contains(p, sig) {
			return false
		}
	}

	ext := strings.ToLower(path.Ext(p))
	if lightExts[ext] {
		return true
	}

	// i18n 资源文件：路径命中特征且扩展名是资源格式
	if i18nExts[ext] {
		for _, sig := range i18nPathSignals {
			if strings.Contains(p, sig) {
				return true
			}
		}
	}
	return false
}

// filepathSlash 统一分隔符，让 Windows 风格路径也能匹配规则。
func filepathSlash(p string) string {
	return strings.ReplaceAll(p, "\\", "/")
}
