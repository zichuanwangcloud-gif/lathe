package schedule

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/linear"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// 执行器：有界并发执行 pending（阶段 B），与认领循环解耦；节点级心跳与自我围栏。

// ensureStarted 惰性起执行器与心跳（第一次 TickOnce / FireNow 时），挂在 Runner
// 自己的生命期 ctx 上 —— 一次 HTTP 请求结束或一次 TickOnce 的 ctx 取消都不该把
// 整个执行器带走。
func (r *Runner) ensureStarted() {
	r.startOnce.Do(func() {
		r.life, r.lifeStop = context.WithCancel(context.Background())
		r.wake = make(chan struct{}, 1)
		r.mu.Lock()
		r.gen = newGeneration(r.life)
		r.held = map[string]int{}
		r.okAt = time.Now()
		r.started = true
		r.mu.Unlock()

		n := r.concurrency
		if n <= 0 {
			n = ExecConcurrency
		}
		for i := 0; i < n; i++ {
			r.wg.Add(1)
			go r.worker()
		}
		r.wg.Add(1)
		go r.heartbeatLoop()
	})
}

func newGeneration(parent context.Context) *generation {
	ctx, cancel := context.WithCancel(parent)
	return &generation{ctx: ctx, cancel: cancel}
}

// register 把 token 名下的 n 条 run 登记进本节点持有集合（心跳按它续约）。
func (r *Runner) register(token string, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.held) == 0 {
		// 集合从空变非空：这批租约刚由数据库授予，从此刻起算续约健康度
		r.okAt = time.Now()
	}
	r.held[token] += n
}

// release 在 token 名下一条 run 结束（写完终态或放弃）时调用。
func (r *Runner) release(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.held[token]; ok {
		if c <= 1 {
			delete(r.held, token)
		} else {
			r.held[token] = c - 1
		}
	}
}

func (r *Runner) enqueue(jobs []job) {
	r.mu.Lock()
	r.queue = append(r.queue, jobs...)
	r.inflight += len(jobs)
	qlen := len(r.queue)
	r.mu.Unlock()
	if qlen > ClaimLimit {
		// 不丢弃：丢弃会把已判准点的 run 变成「结果未知」
		r.logger().Warn("定时建单执行队列积压", "queued", qlen, "node", r.Node)
	}
	r.signal()
}

func (r *Runner) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Runner) pop() (job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.queue) == 0 {
		return job{}, false
	}
	j := r.queue[0]
	r.queue = r.queue[1:]
	if len(r.queue) > 0 {
		r.signal()
	}
	return j, true
}

func (r *Runner) worker() {
	defer r.wg.Done()
	for {
		j, ok := r.pop()
		if !ok {
			select {
			case <-r.life.Done():
				return
			case <-r.wake:
				continue
			}
		}
		if r.hold != nil {
			select {
			case <-r.hold:
			case <-r.life.Done():
			}
		}
		r.process(j)
		if r.life.Err() != nil {
			return
		}
	}
}

func (r *Runner) process(j job) {
	defer func() {
		r.mu.Lock()
		r.inflight--
		r.mu.Unlock()
		r.release(j.token)
	}()
	if j.gen.ctx.Err() != nil {
		// 被围栏或已关闭：不再开始，留给租约过期后的回收
		r.logger().Warn("执行器已停手，排队中的定时运行不再开始（等租约过期后回收）", "run", j.runID, "node", r.Node)
		return
	}
	r.execute(j.gen.ctx, j)
}

// heartbeatLoop 是节点级心跳：每 LeaseHeartbeat 用一条语句续上本节点持有的全部
// token 名下还没写终态的 run（含队列里没开始的）。续约连续失败到
// LeaseTTL − FenceMargin 时围栏：取消执行器当前一代的 ctx、清空持有集合 ——
// 在别的节点能认定租约过期之前至少 FenceMargin，本节点已经停手。
func (r *Runner) heartbeatLoop() {
	defer r.wg.Done()
	every := r.hbEvery()
	step := every / 5
	if step < 10*time.Millisecond {
		step = 10 * time.Millisecond
	}
	t := time.NewTicker(step)
	defer t.Stop()
	renewer := r.renewer
	if renewer == nil {
		renewer = r.Store
	}
	var lastAttempt time.Time
	for {
		select {
		case <-r.life.Done():
			return
		case <-t.C:
		}
		r.mu.Lock()
		tokens := make([]string, 0, len(r.held))
		for tok := range r.held {
			tokens = append(tokens, tok)
		}
		if len(tokens) == 0 {
			r.okAt = time.Now()
		}
		r.mu.Unlock()
		if len(tokens) == 0 {
			continue
		}

		if time.Since(lastAttempt) >= every {
			lastAttempt = time.Now()
			ctx, cancel := context.WithTimeout(r.life, every)
			_, err := renewer.RenewLeases(ctx, tokens, r.ttl())
			cancel()
			if err == nil {
				r.mu.Lock()
				r.okAt = lastAttempt
				r.mu.Unlock()
			} else if r.life.Err() == nil {
				r.logger().Warn("定时运行租约续约失败", "err", err, "tokens", len(tokens), "node", r.Node)
			}
		}

		r.mu.Lock()
		if len(r.held) > 0 && time.Since(r.okAt) >= r.ttl()-r.fence() {
			old := r.gen
			r.gen = newGeneration(r.life)
			r.held = map[string]int{}
			r.okAt = time.Now()
			r.mu.Unlock()
			old.cancel()
			r.logger().Error("定时运行租约续约持续失败，已围栏：取消本节点在途建单，留给租约过期后由任一节点回收",
				"node", r.Node, "since_ok", r.ttl()-r.fence())
			continue
		}
		r.mu.Unlock()
	}
}

// ---------------------------------------------------------------- 阶段 B

// execute 执行一条 run（新认领、回收、FireNow 共用）。ctx 被取消（围栏 / 关闭 /
// 请求断开）后不再写失败终态：留给租约过期后的回收。
func (r *Runner) execute(ctx context.Context, j job) {
	log := r.logger().With("schedule", j.scheduleID, "run", j.runID, "node", r.Node)
	rc, err := r.Store.LoadRunContext(ctx, j.runID)
	if err != nil {
		if errors.Is(err, store.ErrScheduleNotFound) {
			log.Warn("定时运行已不存在（规则被删除，记录级联删除），按丢租约处理")
		} else if ctx.Err() == nil {
			log.Warn("读取定时运行失败，留给租约过期后回收", "err", err)
		}
		return
	}
	run, sc := rc.Run, rc.Schedule
	if run.Status != store.RunPending || run.LeaseToken == nil || *run.LeaseToken != j.token {
		log.Warn("定时运行已丢租约或已有终态，本节点不再执行", "status", run.Status)
		return
	}
	sent := run.CreateSentAt != nil

	// 1. 前置检查（只读库，不取凭据、不调 Linear）
	if rc.OwnerDisabled {
		r.stopUnrun(ctx, j, sent, store.ReasonOwnerDisabled, "属主已停用")
		return
	}
	if !sc.Enabled && run.Trigger != store.TriggerManual {
		r.stopUnrun(ctx, j, sent, store.ReasonScheduleDisabled, "规则已停用")
		return
	}

	// 2. 凭据
	h, err := r.Linear(ctx, run.UserID)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		r.finish(ctx, j, store.FinishRunParams{Status: store.RunFailed,
			Error: redactErr(nil, "Linear 凭据不可用："+err.Error()), Effect: store.RuleFailure})
		return
	}
	redact := func(s string) string { return redactErr(h.Redact, s) }

	// 3. 回收来的、发出过建单的 run：先按 UUID 查单
	if j.recovered && sent {
		iss, err := h.Client.IssueByID(ctx, run.LinearIssueUUID)
		if ctx.Err() != nil {
			return
		}
		switch {
		case err != nil:
			r.finish(ctx, j, store.FinishRunParams{Status: store.RunFailed, Reason: store.ReasonInterrupted,
				Error:  redact("结果未知（按 UUID 查单失败：" + err.Error() + "），请到 Linear 确认是否已建单"),
				Effect: store.RuleUntouched})
			return
		case iss != nil:
			r.finish(ctx, j, store.FinishRunParams{Status: store.RunCreated,
				Issue: &store.CreatedIssueRef{ID: iss.ID, Identifier: iss.Identifier, URL: iss.URL}, Effect: store.RuleSuccess})
			return
		case !r.replayStale():
			r.finish(ctx, j, store.FinishRunParams{Status: store.RunFailed, Reason: store.ReasonInterrupted,
				Error: "结果未知，请到 Linear 确认是否已建单", Effect: store.RuleUntouched})
			return
		case run.Attempts > MaxAttempts:
			r.finish(ctx, j, store.FinishRunParams{Status: store.RunFailed, Reason: store.ReasonInterrupted,
				Error:  fmt.Sprintf("结果未知且已达重放上限（%d 次），请到 Linear 确认是否已建单", MaxAttempts),
				Effect: store.RuleUntouched})
			return
		}
		// ReplayStalePending=true 且确认不存在：按同一个 UUID 重放
	}

	// 4. 渲染模板、组装输入
	loc, err := LoadLocation(sc.Timezone)
	if err != nil {
		r.finish(ctx, j, store.FinishRunParams{Status: store.RunFailed, Error: redact(err.Error()), Effect: store.RuleFailure})
		return
	}
	vars := Vars(run.ScheduledFor, loc, sc.Name)
	in := linear.CreateIssueInput{
		ID: run.LinearIssueUUID, TeamID: sc.LinearTeamID, Priority: sc.Priority,
		Title: RenderTitle(sc.TitleTemplate, vars),
		Description: Render(sc.BodyTemplate, vars) + fmt.Sprintf("\n\n---\n由 Lathe 定时规则 #%d「%s」按计划时刻 %s 创建。",
			sc.ID, sc.Name, vars["datetime"]),
	}
	switch sc.IntakeMode {
	case "label":
		name := ""
		if r.Settings != nil {
			name = r.Settings.WebhookTriggerLabel(ctx)
		}
		if name == "" {
			r.finish(ctx, j, store.FinishRunParams{Status: store.RunFailed,
				Error: "管理员未开启标签接单（系统设置的接单标签为空），单建出来也不会被接", Effect: store.RuleFailure})
			return
		}
		id, err := h.Client.LabelIDByName(ctx, sc.LinearTeamID, name)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			r.finish(ctx, j, store.FinishRunParams{Status: store.RunFailed,
				Error: redact("找不到接单标签「" + name + "」：" + err.Error()), Effect: store.RuleFailure})
			return
		}
		in.LabelIDs = []string{id}
	default:
		if h.UserID == "" {
			r.finish(ctx, j, store.FinishRunParams{Status: store.RunFailed,
				Error: "未取得 Linear 用户 ID，无法指派给你：请到「设置」页重新验证 Linear 令牌", Effect: store.RuleFailure})
			return
		}
		in.AssigneeID = h.UserID
	}

	// 发出 issueCreate 之前记下 create_sent_at；0 行 = 已丢租约，不发请求
	ok, err := r.Store.MarkCreateSent(ctx, run.ID, j.token)
	if err != nil || !ok {
		if ctx.Err() == nil {
			log.Warn("写 create_sent_at 时已丢租约，放弃本次建单", "err", err)
		}
		return
	}

	// 5/6. 建单
	iss, err := h.Client.CreateIssue(ctx, in)
	if err != nil {
		if ctx.Err() != nil {
			log.Warn("建单请求被中止（围栏/关闭/请求断开），留给租约过期后回收")
			return
		}
		r.finish(ctx, j, store.FinishRunParams{Status: store.RunFailed, Error: redact(err.Error()), Effect: store.RuleFailure})
		return
	}
	r.finish(ctx, j, store.FinishRunParams{Status: store.RunCreated,
		Issue: &store.CreatedIssueRef{ID: iss.ID, Identifier: iss.Identifier, URL: iss.URL}, Effect: store.RuleSuccess})
}

// stopUnrun 处理前置检查拦下的 run：从未发出过建单 → skipped；结果未知 → failed。
// 都不计入连续失败。
func (r *Runner) stopUnrun(ctx context.Context, j job, sent bool, reason, why string) {
	p := store.FinishRunParams{Status: store.RunSkipped, Reason: reason, Effect: store.RuleUntouched}
	if sent {
		p.Status = store.RunFailed
		p.Error = why + "，未查询 Linear；上次建单结果未知，请到 Linear 确认"
	}
	r.finish(ctx, j, p)
}

// finish 条件写终态。只有当前租约持有者能写；写 0 行说明租约已被别人接手。
func (r *Runner) finish(ctx context.Context, j job, p store.FinishRunParams) {
	log := r.logger().With("schedule", j.scheduleID, "run", j.runID, "node", r.Node)
	if p.Status != store.RunCreated && ctx.Err() != nil {
		return
	}
	p.RunID, p.Token = j.runID, j.token
	p.MaxFailure, p.PausePrefix = MaxConsecutiveFailures, autoPausePrefix
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	ok, err := r.Store.FinishRun(wctx, p)
	switch {
	case err != nil:
		log.Error("写定时运行终态失败", "status", p.Status, "err", err)
	case !ok && p.Status == store.RunCreated:
		issue := ""
		if p.Issue != nil {
			issue = p.Issue.Identifier
		}
		log.Error("建单已成功但本节点已丢租约，终态由接手节点按 UUID 查单补记", "issue", issue)
	case !ok:
		log.Warn("写终态时已丢租约（或该行已有终态），不覆盖接手节点的结果", "status", p.Status)
	default:
		if p.Status == store.RunCreated && p.Issue != nil {
			log.Info("定时建单成功", "issue", p.Issue.Identifier)
		}
	}
}
