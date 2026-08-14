package creds

import (
	"context"
	"sync"

	"github.com/Clouditera/lathe/internal/runner"
	"github.com/Clouditera/lathe/internal/store"
)

// Factory 按用户产出凭据提供者与客户端（P1.5 第二步数据隔离）。
//
// 与「启动时构造一个全局 Provider」的第一步相比，变化有两点：
//
//  1. 每个用户各有一个 Provider，缓存按 (用户, 凭据种类) 隔离 ——
//     A 改凭据不该碰掉 B 的缓存，更不该看见 B 的凭据。
//  2. 环境变量兜底只给内置管理员。LATHE_LINEAR_TOKEN 这类变量是
//     部署者自己的账号；让普通成员的任务也回退到它们，等于把
//     部署者的身份借给了所有人。
type Factory struct {
	secrets *store.Secrets
	env     EnvFallback
	adminID int64

	mu        sync.Mutex
	providers map[int64]*Provider
}

// NewFactory 构造按用户的凭据工厂。adminID 是内置管理员的用户 ID，
// 只有他的 Provider 带环境变量兜底。
func NewFactory(secrets *store.Secrets, env EnvFallback, adminID int64) *Factory {
	return &Factory{
		secrets: secrets, env: env, adminID: adminID,
		providers: map[int64]*Provider{},
	}
}

// ProviderFor 取某用户的凭据提供者（按用户缓存，重复调用零成本）。
func (f *Factory) ProviderFor(userID int64) *Provider {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.providers[userID]; ok {
		return p
	}
	env := EnvFallback{}
	if userID == f.adminID {
		env = f.env
	}
	p := NewProvider(f.secrets, userID, env)
	f.providers[userID] = p
	return p
}

// ForUser 实现 runner.ClientFactory。
func (f *Factory) ForUser(_ context.Context, userID int64) (runner.Clients, error) {
	return NewClients(f.ProviderFor(userID)), nil
}

// Invalidate 使某用户某类凭据的缓存立即失效；kind 为空清该用户全部。
//
// CredentialAPI 在保存/删除后调它 —— 失效粒度必须到用户，
// 否则 A 保存凭据会把 B 刚缓存好的也清掉（或更糟：A 改了而缓存没清）。
func (f *Factory) Invalidate(userID int64, kind string) {
	f.mu.Lock()
	p, ok := f.providers[userID]
	f.mu.Unlock()
	if ok {
		p.Invalidate(kind)
	}
}
