package github

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v66/github"
)

// Account 是当前令牌对应的 GitHub 账号。
type Account struct {
	Login string `json:"login"`
	Name  string `json:"name"`
	// Scopes 是令牌实际持有的权限范围（细粒度令牌可能为空）。
	Scopes []string `json:"scopes"`
}

// Verify 校验令牌是否可用，并检查是否具备推分支、开 PR 所需的权限。
func (c *Client) Verify(ctx context.Context) (*Account, error) {
	user, resp, err := c.api.Users.Get(ctx, "")
	if err != nil {
		return nil, classifyVerifyError(err, resp)
	}

	acct := &Account{Login: user.GetLogin(), Name: user.GetName()}

	// 经典令牌会在响应头里回报 scope；细粒度令牌不回报，此时留空并跳过检查
	if raw := resp.Header.Get("X-OAuth-Scopes"); raw != "" {
		for _, s := range strings.Split(raw, ",") {
			if s = strings.TrimSpace(s); s != "" {
				acct.Scopes = append(acct.Scopes, s)
			}
		}
		if !hasRepoScope(acct.Scopes) {
			return acct, fmt.Errorf("令牌有效（账号 %s），但缺少 repo 权限 —— 无法推分支和开 PR。"+
				"当前权限：%s", acct.Login, strings.Join(acct.Scopes, ", "))
		}
	}
	return acct, nil
}

// hasRepoScope 判断权限集合里是否含仓库写权限。
func hasRepoScope(scopes []string) bool {
	for _, s := range scopes {
		// repo 是全量仓库权限；public_repo 只能操作公开仓库，
		// 对私有仓库不够，但不在这里断言 —— 目标仓库是否私有由调用方决定
		if s == "repo" || s == "public_repo" {
			return true
		}
	}
	return false
}

func classifyVerifyError(err error, resp *gh.Response) error {
	if resp != nil {
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return fmt.Errorf("令牌无效或已过期，请在 GitHub → Settings → Developer settings → Personal access tokens 重新签发")
		case http.StatusForbidden:
			return fmt.Errorf("令牌被拒（可能触发了速率限制或组织策略限制）：%w", err)
		}
	}

	msg := err.Error()
	if strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "deadline exceeded") {
		return fmt.Errorf("连不上 GitHub（网络或代理问题）：%w", err)
	}
	return err
}
