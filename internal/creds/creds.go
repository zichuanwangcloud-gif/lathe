// Package creds 按需提供各集成的凭据与客户端。
//
// 凭据可在界面上随时修改，因此不能在启动时把客户端固定住 ——
// 那样改完凭据必须重启才生效。这里按需构造并短期缓存：
// 保存凭据时调用 Invalidate 立即失效，避免改完还要等缓存过期。
//
// 取值优先级：数据库（界面配置） → 环境变量（兜底，保持向后兼容）。
// 环境变量优先级更低，是为了让界面上的修改真的能生效；
// 但保留它意味着老部署方式不受影响。
package creds

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Clouditera/lathe/internal/integration/github"
	"github.com/Clouditera/lathe/internal/integration/linear"
	"github.com/Clouditera/lathe/internal/store"
)

// cacheTTL 是凭据缓存时长。
//
// 有缓存是为了避免每个任务都去解密一次；时间短是因为凭据可能刚被改过。
// 显式 Invalidate 覆盖了绝大多数场景，TTL 只是兜底。
const cacheTTL = 30 * time.Second

// ErrNotConfigured 表示该集成尚未配置凭据。
var ErrNotConfigured = errors.New("creds: 凭据未配置")

// EnvFallback 是环境变量兜底值。
type EnvFallback struct {
	LinearToken         string
	LinearWebhookSecret string
	GitHubToken         string
	LinearUserID        string
}

// Provider 按需提供凭据。
type Provider struct {
	secrets *store.Secrets
	userID  int64
	env     EnvFallback

	mu    sync.RWMutex
	cache map[string]cached
}

type cached struct {
	value string
	src   string
	at    time.Time
}

// NewProvider 构造凭据提供者。
func NewProvider(secrets *store.Secrets, userID int64, env EnvFallback) *Provider {
	return &Provider{
		secrets: secrets, userID: userID, env: env,
		cache: map[string]cached{},
	}
}

// Invalidate 使某类凭据的缓存立即失效；kind 为空时清空全部。
func (p *Provider) Invalidate(kind string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if kind == "" {
		p.cache = map[string]cached{}
		return
	}
	delete(p.cache, kind)
}

// Token 取某类凭据，返回值与来源（db / env）。
func (p *Provider) Token(ctx context.Context, kind string) (token, source string, err error) {
	p.mu.RLock()
	c, ok := p.cache[kind]
	p.mu.RUnlock()
	if ok && time.Since(c.at) < cacheTTL {
		return c.value, c.src, nil
	}

	token, err = p.secrets.Get(ctx, p.userID, kind)
	source = "db"
	if err != nil {
		if !errors.Is(err, store.ErrIntegrationNotFound) {
			// 解密失败等真实错误要透出，不能静默退回环境变量 ——
			// 那会让人以为界面上配的凭据在生效
			return "", "", err
		}
		token, source = p.envToken(kind), "env"
	}
	if strings.TrimSpace(token) == "" {
		return "", "", fmt.Errorf("%w（%s）", ErrNotConfigured, kind)
	}

	p.mu.Lock()
	p.cache[kind] = cached{value: token, src: source, at: time.Now()}
	p.mu.Unlock()
	return token, source, nil
}

func (p *Provider) envToken(kind string) string {
	switch kind {
	case store.KindLinear:
		return p.env.LinearToken
	case store.KindLinearWebhook:
		return p.env.LinearWebhookSecret
	case store.KindGitHub:
		return p.env.GitHubToken
	}
	return ""
}

// Linear 构造 Linear 客户端。
func (p *Provider) Linear(ctx context.Context) (*linear.Client, error) {
	token, _, err := p.Token(ctx, store.KindLinear)
	if err != nil {
		return nil, err
	}
	return linear.NewClient(token)
}

// GitHub 构造 GitHub 客户端。
func (p *Provider) GitHub(ctx context.Context) (*github.Client, error) {
	token, _, err := p.Token(ctx, store.KindGitHub)
	if err != nil {
		return nil, err
	}
	return github.NewClient(token)
}

// WebhookSecret 取 Linear webhook 签名密钥。
func (p *Provider) WebhookSecret(ctx context.Context) string {
	token, _, err := p.Token(ctx, store.KindLinearWebhook)
	if err != nil {
		return ""
	}
	return token
}

// LinearUserID 取用于接单判定的 Linear 用户 ID。
//
// 优先用验证凭据时自动获取到的账号 ID —— 用户不必再手工去 Linear 里
// 翻自己的 user id。取不到才回退到环境变量。
func (p *Provider) LinearUserID(ctx context.Context) string {
	if id, err := p.secrets.ExternalAccountID(ctx, p.userID, store.KindLinear); err == nil && id != "" {
		return id
	}
	return p.env.LinearUserID
}

// Ready 报告跑一个完整任务所需的凭据是否齐备。
func (p *Provider) Ready(ctx context.Context) (bool, []string) {
	var missing []string
	for _, kind := range []string{store.KindLinear, store.KindGitHub} {
		if _, _, err := p.Token(ctx, kind); err != nil {
			missing = append(missing, kind)
		}
	}
	return len(missing) == 0, missing
}
