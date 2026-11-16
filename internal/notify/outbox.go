package notify

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Worker 是 outbox 的后台投递者：每 DrainInterval 一轮，逐行「领 → 发 → 记结果」。
//
// 逐行领取是刻意的：批领 10 行再串行发，第 10 行在领取后约 300s 才发出，恰好
// 撞上租约，另一节点会把它回收重发（同一条投两次）。逐行领时，任一行从
// claimed_at 到发送结束的上界只是一次 SendTimeout。
//
// 日志只打 delivery id / kind / event / status，**从不打 URL**；错误原因只经
// Result.Reason（固定类别串 + 平台错误码，已 scrubURL）。
type Worker struct {
	ob      Outbox
	s       *Sender
	baseURL string

	// Now 是测试旋钮（nil = time.Now）；生产不设（装配测试断言 cmd/lathe 里没有 `.Now =`）。
	Now func() time.Time
	// Lease 是测试旋钮（0 = ClaimLease）；生产不设。
	Lease time.Duration
}

// NewWorker 构造 worker。baseURL 是本实例对外地址（详情链接用），空则省略链接行。
func NewWorker(ob Outbox, s *Sender, baseURL string) *Worker {
	return &Worker{ob: ob, s: s, baseURL: baseURL}
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) lease() time.Duration {
	if w.Lease > 0 {
		return w.Lease
	}
	return ClaimLease
}

// Run 按 DrainInterval 轮询直到 ctx 取消。单轮失败只 warn（后台循环的统一写法）。
func (w *Worker) Run(ctx context.Context) {
	tick := time.NewTicker(DrainInterval)
	defer tick.Stop()
	for {
		if err := w.DrainOnce(ctx); err != nil {
			slog.Warn("站外通知投递一轮失败（下一轮重试）", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// DrainOnce 跑一轮：先回收死掉的 sending，再最多处理 DrainMaxPerRound 行。
func (w *Worker) DrainOnce(ctx context.Context) error {
	if n, err := w.ob.ReclaimStale(ctx, w.now(), w.lease()); err != nil {
		return err
	} else if n > 0 {
		slog.Warn("回收了租约过期的站外通知投递", "count", n)
	}
	for i := 0; i < DrainMaxPerRound; i++ {
		if ctx.Err() != nil {
			return nil
		}
		d, err := w.ob.ClaimDueDelivery(ctx, w.now())
		if err != nil {
			return err
		}
		if d == nil {
			return nil
		}
		w.process(ctx, d)
	}
	return nil
}

// process 处理一行。不返回 error：每种结局都写回库（或因 CAS 落空只 warn）。
func (w *Worker) process(ctx context.Context, d *Delivery) {
	defer func() {
		if r := recover(); r != nil {
			// 不打 panic 值的细节以外的东西；panic 值来自本包代码，不含渠道信息。
			slog.Error("站外通知投递 panic（已在边界兜住，等租约回收）", "delivery", d.ID, "panic", fmt.Sprint(r))
		}
	}()

	ch, err := w.ob.DeliveryChannel(ctx, d.ChannelID)
	if err != nil {
		// 读库失败或密文解不开（主密钥换了）：重试也不会变好，放弃并留下原因。
		w.mark(d, "gave_up", "channel unavailable", func(now time.Time) (bool, error) {
			return w.ob.MarkGaveUp(ctx, d.ID, d.ClaimedAt, now, "channel unavailable")
		})
		return
	}
	if ch == nil {
		slog.Warn("站外通知投递的渠道已删除", "delivery", d.ID)
		return
	}
	if !ch.Enabled || !ch.Events.Allows(d.EventType) {
		w.mark(d, "skipped", "", func(now time.Time) (bool, error) {
			return w.ob.MarkSkipped(ctx, d.ID, d.ClaimedAt, now, "channel or event disabled")
		})
		return
	}

	title, text := Render(d.EventType, FactsFromPayload(d.EventType, d.Payload, w.baseURL))
	sendCtx, cancel := context.WithTimeout(ctx, SendTimeout)
	res := w.s.Send(sendCtx, *ch, Message{Title: title, Text: text})
	cancel()

	switch {
	case res.OK:
		w.mark(d, "sent", "", func(now time.Time) (bool, error) {
			return w.ob.MarkSent(ctx, d.ID, d.ClaimedAt, now)
		})
	case res.Retryable:
		w.mark(d, "retry", res.Reason, func(now time.Time) (bool, error) {
			return w.ob.MarkRetry(ctx, d.ID, d.ClaimedAt, now, res.Reason)
		})
	default:
		w.mark(d, "gave_up", res.Reason, func(now time.Time) (bool, error) {
			return w.ob.MarkGaveUp(ctx, d.ID, d.ClaimedAt, now, res.Reason)
		})
	}
}

func (w *Worker) mark(d *Delivery, outcome, reason string, fn func(now time.Time) (bool, error)) {
	hit, err := fn(w.now())
	switch {
	case err != nil:
		slog.Warn("站外通知投递结果写回失败（等租约回收）", "delivery", d.ID, "event", d.EventType, "outcome", outcome, "err", err)
	case !hit:
		slog.Warn("站外通知投递已被回收或改写，结果不覆盖", "delivery", d.ID, "event", d.EventType, "outcome", outcome)
	case outcome != "sent" && outcome != "skipped":
		slog.Warn("站外通知投递失败", "delivery", d.ID, "event", d.EventType, "outcome", outcome, "reason", reason)
	}
}
