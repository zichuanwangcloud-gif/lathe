package creds

import (
	"context"
	"fmt"

	"github.com/Clouditera/lathe/internal/httpapi"
	"github.com/Clouditera/lathe/internal/integration/github"
	"github.com/Clouditera/lathe/internal/integration/linear"
	"github.com/Clouditera/lathe/internal/runner"
	"github.com/Clouditera/lathe/internal/store"
)

// Verifier 用给定凭据实际连一次外部服务，验证其可用性。
type Verifier struct{}

// Verify 实现 httpapi.Verifier。
func (Verifier) Verify(ctx context.Context, kind, token string) httpapi.VerifyResult {
	switch kind {
	case store.KindLinear:
		return verifyLinear(ctx, token)
	case store.KindGitHub:
		return verifyGitHub(ctx, token)
	case store.KindLinearWebhook:
		// webhook secret 没有可调用的验证接口 —— 它的正确性只能在
		// 真正收到 webhook 时才知道。如实说明，不假装验证过。
		if len(token) < 8 {
			return httpapi.VerifyResult{OK: false, Error: "webhook 密钥过短，请使用 Linear 生成的完整密钥"}
		}
		return httpapi.VerifyResult{
			OK:     true,
			Detail: "已保存。webhook 密钥无法主动验证，需等 Linear 实际投递一次事件才能确认。",
		}
	default:
		return httpapi.VerifyResult{OK: false, Error: "未知凭据类型 " + kind}
	}
}

func verifyLinear(ctx context.Context, token string) httpapi.VerifyResult {
	c, err := linear.NewClient(token)
	if err != nil {
		return httpapi.VerifyResult{OK: false, Error: err.Error()}
	}
	v, err := c.Verify(ctx)
	if err != nil {
		return httpapi.VerifyResult{OK: false, Error: err.Error()}
	}
	return httpapi.VerifyResult{
		OK:          true,
		AccountName: v.Name,
		AccountID:   v.ID,
		Detail: fmt.Sprintf("已连接到 Linear 账号 %s（%s）。接单判定将使用该账号，无需再手工填写用户 ID。",
			v.Name, v.Email),
	}
}

func verifyGitHub(ctx context.Context, token string) httpapi.VerifyResult {
	c, err := github.NewClient(token)
	if err != nil {
		return httpapi.VerifyResult{OK: false, Error: err.Error()}
	}
	acct, err := c.Verify(ctx)
	if err != nil {
		res := httpapi.VerifyResult{OK: false, Error: err.Error()}
		if acct != nil {
			res.AccountName = acct.Login
		}
		return res
	}

	detail := fmt.Sprintf("已连接到 GitHub 账号 %s", acct.Login)
	if len(acct.Scopes) > 0 {
		detail += fmt.Sprintf("，权限：%v", acct.Scopes)
	} else {
		detail += "（细粒度令牌，未回报权限范围；请自行确认其对目标仓库有读写权限）"
	}
	return httpapi.VerifyResult{OK: true, AccountName: acct.Login, AccountID: acct.Login, Detail: detail}
}

// Clients 让 Provider 满足 runner.Clients，使流水线按需取客户端。
type Clients struct {
	p *Provider
}

// NewClients 包装 Provider 供流水线使用。
func NewClients(p *Provider) *Clients { return &Clients{p: p} }

// Linear 实现 runner.Clients。
func (c *Clients) Linear(ctx context.Context) (runner.LinearAPI, error) {
	return c.p.Linear(ctx)
}

// GitHub 实现 runner.Clients。
func (c *Clients) GitHub(ctx context.Context) (runner.GitHubAPI, error) {
	return c.p.GitHub(ctx)
}
