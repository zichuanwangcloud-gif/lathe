package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/zichuanwangcloud-gif/lathe/internal/vcs"
)

// vcs.go 是代码平台（GitHub / GitLab / Gitea）在 runner 侧的分派入口
// （docs/features/vcs-gitlab-gitea.md §3.4）。
//
// 兼容策略：Clients、GitHubAPI、runCtx.gh 三个名字与方法集保持原样（并行线 f09
// 的测试假件只实现 Tracker 与 GitHub），新平台走可选接口 VCSClients，与
// ReviewFeedbackSource 同一个「类型断言取」形态。

// StageVCSCredential 是「代码平台凭据不可用」的失败阶段码：GitLab/Gitea 仓库的
// 凭据缺失（vcs.ErrCredentialMissing）或凭据实例与仓库登记的实例不一致
// （vcs.ErrInstanceMismatch）。确定性的配置错误，不会自己好 —— 落 failed 让人
// 看得见，而不是留在 queued 每两分钟被重领一次（AGENTS §9「卡排队」）。
//
// 不进 stageOrder（同 StageSkillMissing 的理由：改 stageOrder 会改变 before()
// 对其它阶段的比较）。tasks.failure_stage 无 CHECK，无需迁移。
const StageVCSCredential Stage = "vcs_credential"

// VCSClients 是 Clients 的可选能力：按仓库所在平台实例取客户端与 git 鉴权。
// creds.Clients 实现它；只实现 Clients 的假件对 GitHub 仓库照常工作。
type VCSClients interface {
	VCS(ctx context.Context, target vcs.Target) (GitHubAPI, vcs.GitAuth, error)
}

// resolveVCS 是唯一的分派入口（pipeline setup、mergepoll pollTask、rebase 跟进）。
//
// GitHub（含存量行的默认值）只调 c.GitHub(ctx)，与引入代码平台之前完全同一调用，
// 返回零值 GitAuth（走宿主 SSH）。
func resolveVCS(ctx context.Context, c Clients, t vcs.Target) (GitHubAPI, vcs.GitAuth, error) {
	if t.Provider == vcs.ProviderGitHub || t.Provider == "" {
		gh, err := c.GitHub(ctx)
		return gh, vcs.GitAuth{}, err
	}
	v, ok := c.(VCSClients)
	if !ok {
		return nil, vcs.GitAuth{}, fmt.Errorf("runner: 客户端不支持代码平台 %q", t.Provider)
	}
	return v.VCS(ctx, t)
}

// isVCSConfigErr 报告错误是否是确定性的代码平台配置错误（落 failed 而不是留 queued）。
func isVCSConfigErr(err error) bool {
	return errors.Is(err, vcs.ErrInstanceMismatch) || errors.Is(err, vcs.ErrCredentialMissing)
}

// vcsGate 在已经站到「可以合法转 failed」的状态上之后调用：setup 记下的代码平台
// 配置错误在这里落 failed。每个入口阶段的第一次合法转移之后、任何网络操作之前
// 调一次，全程只走 state.go 已有的边。
func (p *Pipeline) vcsGate(rc *runCtx) error {
	if rc.vcsErr == nil {
		return nil
	}
	return p.fail(rc, StageVCSCredential, rc.vcsErr)
}
