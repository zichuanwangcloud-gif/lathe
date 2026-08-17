// Package runner 在节点上执行任务：管理 worktree 生命周期、
// 驱动 agent、跑分级验证。
package runner

import (
	"fmt"
	"strings"
	"unicode"
)

// TaskKind 是任务类型，决定基线分支与分支前缀。
type TaskKind string

const (
	// KindFix 修 bug，从默认分支（dev）分叉。
	KindFix TaskKind = "fix"
	// KindFeature 做需求，从默认分支（dev）分叉。
	KindFeature TaskKind = "feature"
	// KindHotfix 紧急修复，从 main 分叉（CloudRouter CLAUDE.md 的规定）。
	KindHotfix TaskKind = "hotfix"
)

// Valid 报告 k 是否为已知任务类型。
func (k TaskKind) Valid() bool {
	switch k {
	case KindFix, KindFeature, KindHotfix:
		return true
	}
	return false
}

// RepoConfig 是仓库级的分支策略配置，对应 repos 表。
type RepoConfig struct {
	ProviderRepo      string   // 如 Clouditera/CloudRouter
	DefaultBranch     string   // fix/feature 的分叉基线，通常是 dev
	HotfixBase        string   // hotfix 的分叉基线，通常是 main
	ProtectedBranches []string // 禁止直接推送的分支
	BranchPattern     string   // 如 {kind}/{key}-{slug}
	// ExcludeDirs 是验证扫描要跳过的目录（相对根路径或纯目录名），
	// 对应 repos.exclude_dirs —— 停止维护的目录（如 CloudRouter 的
	// apps/console）在这里排除，存量问题不再拖死新任务。
	ExcludeDirs []string
	// VerifyTierOverride 强制验证档位（light|heavy）；空表示按 §5.1 规则
	// 在 diff 产出后自动判定。对应 repos.verify_tier_override。
	VerifyTierOverride string
}

// DefaultRepoConfig 返回符合 CloudRouter 约定的默认配置。
func DefaultRepoConfig(providerRepo string) RepoConfig {
	return RepoConfig{
		ProviderRepo:      providerRepo,
		DefaultBranch:     "dev",
		HotfixBase:        "main",
		ProtectedBranches: []string{"dev", "test", "main"},
		BranchPattern:     "{kind}/{key}-{slug}",
	}
}

// BaseBranch 返回该类型任务应从哪个分支分叉。
//
// 依据 CloudRouter CLAUDE.md：代码单向流动 feature/* → dev → test → main，
// 功能分支只能从 dev 创建，hotfix 分支只能从 main 创建。
func (c RepoConfig) BaseBranch(kind TaskKind) (string, error) {
	switch kind {
	case KindFix, KindFeature:
		if c.DefaultBranch == "" {
			return "", fmt.Errorf("runner: 仓库 %s 未配置默认分支", c.ProviderRepo)
		}
		return c.DefaultBranch, nil
	case KindHotfix:
		if c.HotfixBase == "" {
			return "", fmt.Errorf("runner: 仓库 %s 未配置 hotfix 基线分支", c.ProviderRepo)
		}
		return c.HotfixBase, nil
	default:
		return "", fmt.Errorf("runner: 未知任务类型 %q", kind)
	}
}

// ErrProtectedBranch 表示试图推送到受保护分支。
type ErrProtectedBranch struct {
	Branch string
	Repo   string
}

func (e ErrProtectedBranch) Error() string {
	return fmt.Sprintf("runner: 拒绝推送到受保护分支 %s（仓库 %s）—— Lathe 只开 PR，永不直接推受保护分支",
		e.Branch, e.Repo)
}

// ValidatePushTarget 在推送前校验目标分支不是受保护分支。
//
// 这是 CloudRouter CLAUDE.md「绝对禁止 push origin dev/test/main」的代码化，
// 也是 docs/02-design.md §1 产品边界「永不 push 受保护分支」的最后一道闸门。
func (c RepoConfig) ValidatePushTarget(branch string) error {
	b := strings.TrimSpace(branch)
	if b == "" {
		return fmt.Errorf("runner: 推送目标分支为空")
	}
	// 容忍 refs/heads/ 前缀
	b = strings.TrimPrefix(b, "refs/heads/")
	for _, p := range c.ProtectedBranches {
		if strings.EqualFold(b, strings.TrimSpace(p)) {
			return ErrProtectedBranch{Branch: b, Repo: c.ProviderRepo}
		}
	}
	return nil
}

// maxSlugLen 限制 slug 长度，避免分支名过长。
const maxSlugLen = 48

// BranchName 按仓库模式生成分支名。
//
// 占位符：{kind} 任务类型、{key} 小写的 issue 编号、{slug} 标题派生的短串。
//
// slug 只保留 ASCII 字母数字：团队较新的分支（fix/cr-1326-portable-import）
// 都是英文 slug，而早期由工具生成的中文分支名会给 CI、URL 与命令行带来
// 转义麻烦。纯中文标题会得到空 slug，此时退化为只用 issue 编号。
func (c RepoConfig) BranchName(kind TaskKind, issueKey, title string) (string, error) {
	if !kind.Valid() {
		return "", fmt.Errorf("runner: 未知任务类型 %q", kind)
	}
	key := strings.ToLower(strings.TrimSpace(issueKey))
	if key == "" {
		return "", fmt.Errorf("runner: issue 编号为空")
	}

	pattern := c.BranchPattern
	if pattern == "" {
		pattern = "{kind}/{key}-{slug}"
	}

	name := strings.NewReplacer(
		"{kind}", string(kind),
		"{key}", key,
		"{slug}", Slugify(title),
	).Replace(pattern)

	// slug 为空时会留下形如 fix/cr-1326- 的尾巴，清掉
	name = strings.TrimRight(name, "-/")
	name = collapseRepeats(name, '-')

	if err := validateBranchName(name); err != nil {
		return "", err
	}
	if err := c.ValidatePushTarget(name); err != nil {
		return "", err
	}
	return name, nil
}

// Slugify 把标题转成分支名可用的短串。
func Slugify(title string) string {
	var b strings.Builder
	lastDash := true // 前导横杠也算重复，直接吃掉
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case unicode.IsSpace(r), r == '-', r == '_', r == '/', r == '.':
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		default:
			// 中文等非 ASCII 字符：当作分隔符，不写入
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
		if b.Len() >= maxSlugLen {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// validateBranchName 拦掉 git 不接受或容易惹麻烦的分支名。
func validateBranchName(name string) error {
	if name == "" {
		return fmt.Errorf("runner: 生成的分支名为空")
	}
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") {
		return fmt.Errorf("runner: 分支名 %q 不能以 - 或 / 开头", name)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("runner: 分支名 %q 不能含 ..", name)
	}
	if strings.Contains(name, "//") {
		return fmt.Errorf("runner: 分支名 %q 不能含 //", name)
	}
	if strings.HasSuffix(name, ".lock") {
		return fmt.Errorf("runner: 分支名 %q 不能以 .lock 结尾", name)
	}
	for _, bad := range []string{" ", "~", "^", ":", "?", "*", "[", "\\", "@{"} {
		if strings.Contains(name, bad) {
			return fmt.Errorf("runner: 分支名 %q 含非法字符 %q", name, bad)
		}
	}
	return nil
}

func collapseRepeats(s string, c byte) string {
	var b strings.Builder
	var prev byte
	for i := 0; i < len(s); i++ {
		if s[i] == c && prev == c {
			continue
		}
		b.WriteByte(s[i])
		prev = s[i]
	}
	return b.String()
}
