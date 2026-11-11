package main

import (
	"context"
	"strings"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/config"
	"github.com/zichuanwangcloud-gif/lathe/internal/creds"
	"github.com/zichuanwangcloud-gif/lathe/internal/httpapi"
	"github.com/zichuanwangcloud-gif/lathe/internal/integration/linear"
	"github.com/zichuanwangcloud-gif/lathe/internal/schedule"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// 定时建单的装配（docs/features/scheduled-issue.md §3.10）。
//
// Runner 在 serve() 里构造（路由注册要拿到它做「立即运行」），作为参数交给
// startWorkers —— 那里是唯一的启动点（`go scheduler.Run`）。构造本身不起
// goroutine、不碰网络、不查库。

// newScheduler 只构造，不启动任何 goroutine。启动唯一入口是 startWorkers。
func newScheduler(cfg config.Config, st *store.Store, f *creds.Factory) *schedule.Runner {
	return &schedule.Runner{
		Store:         st,
		Linear:        linearFor(f),
		Settings:      st,
		Now:           time.Now,
		CatchUpWindow: cfg.ScheduleCatchUpWindow,
		Node:          cfg.NodeName,
	}
}

// linearFor 用属主自己的凭据构造 Linear 能力，与执行队列走同一条凭据通路
// （同一个 Provider 缓存），界面上改完凭据立即生效。
//
// 参数是具体类型 *creds.Factory：runner.ClientFactory 只有 ForUser，没有 ProviderFor。
// token 明文只留在这个闭包里：调度器拿到的是一个「会抹掉 token 的函数」，
// 不给 linear.Client 加 token 访问器（那会让任何拿到客户端的代码都能取出明文）。
func linearFor(f *creds.Factory) schedule.LinearFor {
	return func(ctx context.Context, userID int64) (schedule.LinearHandle, error) {
		p := f.ProviderFor(userID)
		token, _, err := p.Token(ctx, store.KindLinear) // 与 Provider.Linear 内部同一条路
		if err != nil {
			return schedule.LinearHandle{}, err
		}
		c, err := linear.NewClient(token)
		if err != nil {
			return schedule.LinearHandle{}, err
		}
		return schedule.LinearHandle{
			Client: c,
			UserID: p.LinearUserID(ctx),
			Redact: strings.NewReplacer(token, "[REDACTED]").Replace,
		}, nil
	}
}

// scheduleIntake 实现 httpapi.ScheduleIntake：保存规则时回答「单建出来会不会被接」。
// 判据与 webhookResolver 同源（同一个 Provider、同一个系统设置），不另起一套。
type scheduleIntake struct {
	factory  *creds.Factory
	settings *store.Store
}

func (s scheduleIntake) Linear(ctx context.Context, userID int64) (httpapi.ScheduleLinear, error) {
	c, err := s.factory.ProviderFor(userID).Linear(ctx)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (s scheduleIntake) WebhookSecretConfigured(ctx context.Context, userID int64) bool {
	return s.factory.ProviderFor(userID).WebhookSecret(ctx) != ""
}

func (s scheduleIntake) LinearUserID(ctx context.Context, userID int64) string {
	return s.factory.ProviderFor(userID).LinearUserID(ctx)
}

func (s scheduleIntake) TriggerLabel(ctx context.Context) string {
	return s.settings.WebhookTriggerLabel(ctx)
}
