// Package vcs 是「代码平台」的契约包：开 PR/MR、查合并状态、拉 review 意见、
// 克隆地址与 git 鉴权的平台无关形状。
//
// 零依赖（只 import 标准库）：runner、prd、httpapi 与各平台实现都依赖它，
// 它不依赖任何一方，不会成环。GitHub 实现留在 internal/integration/github，
// 那里的 PRParams/PullRequest/PRInfo/ReviewComment/ReviewFeedback 是本包同名
// 类型的别名；GitLab/Gitea 实现在 internal/vcs/gitlab、internal/vcs/gitea。
//
// 契约包里没有任何信任逻辑（docs/features/vcs-gitlab-gitea.md §3.1）：
// ReviewEvent 表达的是「已经通过本平台信任判定的一条意见」，判定本身住在
// 各平台包里，在 runner.ReviewIntake.AcceptReview 之前完成。
package vcs

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Provider 是代码平台枚举，对应 repos.vcs_provider 的取值。
type Provider string

const (
	ProviderGitHub Provider = "github"
	ProviderGitLab Provider = "gitlab"
	ProviderGitea  Provider = "gitea"
)

// Valid 只认三个枚举值。空串不是 github 的别名：review 事件按
// (属主, 平台, 路径, 编号) 找任务，空平台若被当成 github，漏填平台的
// handler 就会被当作 GitHub 投递去路由 —— 恰好是平台过滤要挡的串扰。
func (p Provider) Valid() bool {
	switch p {
	case ProviderGitHub, ProviderGitLab, ProviderGitea:
		return true
	}
	return false
}

// DisplayName 是给人看的平台名（错误文案、界面）。
func (p Provider) DisplayName() string {
	switch p {
	case ProviderGitHub:
		return "GitHub"
	case ProviderGitLab:
		return "GitLab"
	case ProviderGitea:
		return "Gitea"
	}
	return string(p)
}

// Client 是流水线与合并检测需要的代码平台能力（方法就是消费方需要的那些）。
type Client interface {
	CreatePR(ctx context.Context, p PRParams) (*PullRequest, error)
	GetPRInfo(ctx context.Context, repoRef string, number int) (*PRInfo, error)
}

// ReviewFeedbackSource 是可选能力（runner 用类型断言取），三个实现都提供。
// 不并进 Client：那会让每个测试假件都得实现它。
type ReviewFeedbackSource interface {
	ReviewFeedback(ctx context.Context, repoRef string, number int) (*ReviewFeedback, error)
}

// PRParams 描述要创建的 PR（GitLab 上即 MR）。
type PRParams struct {
	ProviderRepo string // owner/name；GitLab 可为 group/sub/name
	Head         string // 任务分支
	Base         string // 目标分支（dev / main）
	Title        string
	Body         string
	// Draft 为真时开草稿 PR。
	Draft bool
}

// PullRequest 是已创建或已存在的 PR。
type PullRequest struct {
	// Number 是平台 UI 上显示的编号：GitHub/Gitea 的 PR 号，GitLab 的 MR iid
	// （项目内序号，不是全局 id）。
	Number int
	URL    string
	State  string
	// Existing 为真表示这是复用的既有 PR，而非本次新建。
	Existing bool
}

// PRInfo 是 PR 当前状态的精简摘要，三个平台都映射到同一组取值
// （见 docs/features/vcs-gitlab-gitea.md §3.6），合并检测的三态分支因此与平台无关。
type PRInfo struct {
	Number int
	Merged bool
	// State 是 "open" / "closed"。
	State string
	// BaseRef 是 PR 当前指向的 base 分支名。仅用于诊断，不强制消费。
	BaseRef string
	// HeadSHA 是 PR head 分支当前指向的 commit SHA —— F4.4 据此检测
	// 「PR 仍 open 但内容被 force-push 改写」。
	HeadSHA string
}

// ReviewComment 是一条需要 agent 处理的 review 意见。
type ReviewComment struct {
	ID int64
	// Key 是跨类型唯一的稳定标识，带平台/类型前缀（GitHub "inline:<id>"、
	// GitLab "gitlab:note:<id>"、Gitea "gitea:comment:<id>" …）。
	Key    string
	Author string
	// AuthorAssociation 只有 GitHub 填（author_association）。GitLab/Gitea
	// 在拉取时用各自的成员查询过滤，不填该字段。
	AuthorAssociation string
	Body              string
	Path              string
	Line              int
	URL               string
}

// ReviewFeedback 是一个 PR 上全部可能需要处理的意见。
type ReviewFeedback struct {
	// PRAuthor 是开 PR 的账号（Lathe 在该 PR 上的身份），调用方据此排除自身。
	PRAuthor string
	Comments []ReviewComment
}

// ReviewEvent 是一条【已经通过本平台信任判定】的 review 意见。
//
// 平台中立：没有 AuthorAssociation，也没有任何信任方法 —— 信任判定只发生在
// 各平台的 webhook handler 里，通过之后才构造它。刻意是可比较的纯值类型。
type ReviewEvent struct {
	// Provider 必填（AcceptReview 第一步就校验 Valid()）。
	Provider Provider
	Event    string
	Action   string
	Repo     string
	PRNumber int
	PRAuthor string
	Key      string
	Author   string
	Body     string
	Path     string
	Line     int
	URL      string
	// ReviewState 仅 GitHub pull_request_review 有值。
	ReviewState string
}

// ErrNoCommits 表示 head 与 base 无差异，没东西可开 PR。
var ErrNoCommits = errors.New("vcs: head 与 base 无差异，无可提交的改动")

// ErrCredentialMissing 表示该平台的凭据行不存在或没有实例地址 —— 确定性的
// 配置错误，不会自己好（流水线据此在入口阶段落 failed，见 runner.vcsGate）。
var ErrCredentialMissing = errors.New("vcs: 代码平台凭据未配置")

// ErrInstanceMismatch 表示凭据当前指向的实例与仓库登记时钉死的实例不同。
// 这是一次安全拒绝：把 A 实例的 token 发给 B 实例就是凭据外泄。
var ErrInstanceMismatch = errors.New("vcs: 凭据实例与仓库登记的实例不一致")

// InstanceMismatch 构造带两个地址的可读错误（不含 token）。
func InstanceMismatch(p Provider, repoBase, credBase string) error {
	return fmt.Errorf("%w：仓库登记在 %s，而你的 %s 凭据现在指向 %s；token 不会发往与仓库不同的实例。"+
		"请改回凭据地址，或删除并重新登记仓库", ErrInstanceMismatch, repoBase, p.DisplayName(), credBase)
}

// maxRepoPathLen 是 GitLab/Gitea 仓库路径的总长上限。
const maxRepoPathLen = 200

// SplitRef 把仓库标识拆成 (命名空间, 名字)。
//
// GitHub 仍要求【恰好两段】（与 github.SplitRepo 的语义与文案一致）；
// GitLab/Gitea 允许 ≥2 段（子组），每段限 [A-Za-z0-9._-]、非空、不以 . 或 -
// 开头，总长 ≤ 200，拒绝 ..（路径穿越）与任何编码字符（%2f 之类）。
func SplitRef(p Provider, s string) (namespace, name string, err error) {
	switch p {
	case ProviderGitHub:
		parts := strings.Split(strings.TrimSpace(s), "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", "", fmt.Errorf("github: 仓库标识 %q 格式应为 owner/name", s)
		}
		return parts[0], parts[1], nil
	case ProviderGitLab, ProviderGitea:
		bad := func(why string) error {
			return fmt.Errorf("vcs: %s 仓库路径 %q 非法（%s），格式应为 group/name 或 group/sub/name",
				p.DisplayName(), s, why)
		}
		if len(s) > maxRepoPathLen {
			return "", "", bad(fmt.Sprintf("超过 %d 个字符", maxRepoPathLen))
		}
		parts := strings.Split(s, "/")
		if len(parts) < 2 {
			return "", "", bad("至少两段")
		}
		for _, seg := range parts {
			if seg == "" {
				return "", "", bad("含空段")
			}
			if seg == ".." || strings.Contains(seg, "..") {
				return "", "", bad("含 ..")
			}
			if seg[0] == '.' || seg[0] == '-' {
				return "", "", bad("段不能以 . 或 - 开头")
			}
			for _, r := range seg {
				if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
					return "", "", bad(fmt.Sprintf("含不允许的字符 %q", r))
				}
			}
		}
		return strings.Join(parts[:len(parts)-1], "/"), parts[len(parts)-1], nil
	}
	return "", "", fmt.Errorf("vcs: 未知代码平台 %q", p)
}

// BuildPRBody 生成带验证证据的 PR 正文（与平台无关）。
//
// 验证证据必须进 PR 正文：审阅者第一眼要能看到这份改动被验证过什么，
// 这是本产品与「agent 随手产出的 diff」的根本区别。
func BuildPRBody(issueKey, issueURL, verifySummary, agentSummary string) string {
	var b strings.Builder

	if issueKey != "" {
		if issueURL != "" {
			fmt.Fprintf(&b, "关联 issue: [%s](%s)\n\n", issueKey, issueURL)
		} else {
			fmt.Fprintf(&b, "关联 issue: %s\n\n", issueKey)
		}
	}
	if agentSummary != "" {
		fmt.Fprintf(&b, "## 改动说明\n\n%s\n\n", strings.TrimSpace(agentSummary))
	}
	if verifySummary != "" {
		fmt.Fprintf(&b, "## 验证结果\n\n```\n%s```\n\n", verifySummary)
	}
	b.WriteString("---\n由 [Lathe](https://github.com/zichuanwangcloud-gif/lathe) 自动生成。**合并前请人工复核。**\n")
	return b.String()
}
