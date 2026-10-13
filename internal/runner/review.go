package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/github"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// review.go 是 PR 评论回流（docs/02-design.md §7）的 runner 侧：
//
//	GitHub review 意见 → webhook（httpapi.GitHubWebhook，验签 + 投递去重）
//	  → ReviewIntake.AcceptReview：找任务、守卫、pr_open → review_feedback → queued
//	  → DB 领单调度器捡起，PlanRetry(RetryReview) 给出 EntryImplement + resume
//	  → Pipeline：collectReview 拉待处理意见 → resume 原会话改 → 完整重新验证 → pr_open
//
// 「哪些意见已处理过」记在任务事件流里（实现阶段那次转移的 payload 的
// review_comment_keys），与 mode / interrupted_state 同一套「事件流即
// 唯一权威来源」的做法，不另开表。

// reviewActor 是回流路径在任务事件流里的 actor：审计时能看出这次
// 状态变化是 PR 上的 review 意见触发的，不是人在 Lathe 里点的。
const reviewActor = "system:github-review"

// maxReviewBodyRunes 是单条意见落事件 payload / 进 prompt 时的长度上限。
// truncate 按 rune 边界回退（AGENTS.md §9：按 byte 硬切会让整批事件落库失败）。
const maxReviewBodyRunes = 4000

// maxReviewItems 是一轮最多交给 agent 的意见条数。超出的不记为已处理，
// 留给下一轮 —— 一次塞进几十条意见只会让每条都处理得更潦草。
const maxReviewItems = 30

// 事件 payload 的键。
const (
	payloadReview     = "review"              // AcceptReview 记下的那条触发意见
	payloadReviewKeys = "review_comment_keys" // 实现阶段本轮处理的意见 Key 列表
)

// ReviewStatus 是一次回流处理的结论。
type ReviewStatus string

const (
	// ReviewQueued 意见已记下，任务已交给调度器派发二轮。
	ReviewQueued ReviewStatus = "queued"
	// ReviewParked 意见已记下、任务停在 review_feedback，但不能自动派发
	// （没有可 resume 的会话，约束①），等人处理。
	ReviewParked ReviewStatus = "parked"
	// ReviewIgnored 安全忽略（找不到任务 / 任务不在可回流的状态 / 重复意见）。
	ReviewIgnored ReviewStatus = "ignored"
)

// ReviewOutcome 是 AcceptReview 的结果，原样回给 webhook 响应体与投递记录。
type ReviewOutcome struct {
	Status ReviewStatus `json:"status"`
	TaskID int64        `json:"taskId,omitempty"`
	Reason string       `json:"reason,omitempty"`
}

// ReviewWorkspaceChecker 体检任务的工作区现场是否还能续跑二轮。
type ReviewWorkspaceChecker interface {
	ReviewWorkspaceUsable(ctx context.Context, tk *task.Task) bool
}

// ReviewIntake 把一条 review 意见接进任务状态机。
type ReviewIntake struct {
	Tasks *task.Machine
	// Workspaces 非空时，回流前先体检现场：现场已不可用就停在
	// review_feedback 等人处理，而不是交给调度器。
	//
	// 理由：pr_open 任务的分支早已推到远端。现场没了只能从头重建，而重建
	// 出的新分支推送会因非快进被拒 —— 白烧一整轮分诊+实现+验证，最后在
	// push 阶段失败。在入口挡下，不花一分钱，面板上照样看得到「待处理评审」。
	// 为 nil 时不体检（派发侧 PlanRetry 仍有兜底）。
	Workspaces ReviewWorkspaceChecker
}

// AcceptReview 处理一条（已验签、已去重、已判定为可处理的）review 意见。
//
// ownerID 是 webhook 回调地址（slug）解析出的用户；任务查找按它过滤，
// A 的 webhook 投递碰不到 B 的任务（task.Machine.TasksByPR 两遍属主过滤）。
//
// 找不到任务、任务不在 pr_open/review_feedback、意见重复，都是「安全忽略」，
// 返回 (ReviewIgnored, nil) 而不是错误 —— 仓库 webhook 会投来与 Lathe 无关
// 的 PR 的事件，这是常态。终态任务（merged/failed/cancelled）在这里就被
// 挡下，不会走到转移那一步；即使走到了，转移表也会拒绝。
func (in *ReviewIntake) AcceptReview(ctx context.Context, ownerID int64, ev *github.ReviewEvent, deliveryID string) (ReviewOutcome, error) {
	tasks, err := in.Tasks.TasksByPR(ctx, ownerID, ev.Repo, ev.PRNumber)
	if err != nil {
		return ReviewOutcome{}, err
	}
	if len(tasks) == 0 {
		// 「不是你的」与「不存在」同一个回答：不经由响应暴露别人的任务。
		return ReviewOutcome{Status: ReviewIgnored,
			Reason: fmt.Sprintf("没有对应的任务（%s PR #%d）", ev.Repo, ev.PRNumber)}, nil
	}

	var target *task.Task
	for _, tk := range tasks { // 新的在前
		if tk.State == task.StatePROpen || tk.State == task.StateReviewFeedback {
			target = tk
			break
		}
	}
	if target == nil {
		return ReviewOutcome{Status: ReviewIgnored, TaskID: tasks[0].ID,
			Reason: fmt.Sprintf("任务当前处于 %s，不接收 review 回流", tasks[0].State)}, nil
	}

	// 意见级幂等：投递去重挡的是「同一次投递重放」，挡不住同一条意见经
	// 不同投递到达（手工 redeliver、或二轮已结束后旧投递姗姗来迟）。
	// 按意见 Key 再挡一次，已回流过或已处理过的不再开一轮。
	events, err := in.Tasks.Events(ctx, target.ID)
	if err != nil {
		return ReviewOutcome{}, err
	}
	if reviewKeySeen(events, ev.Key) {
		return ReviewOutcome{Status: ReviewIgnored, TaskID: target.ID,
			Reason: "这条意见已经回流过（" + ev.Key + "）"}, nil
	}

	review := reviewPayloadOf(ev)
	recorded := false

	if target.State == task.StatePROpen {
		// TransitionFrom：两条投递并发到达时，行锁下只有一条能把任务从
		// pr_open 带走；另一条拿到 ErrStateMismatch，按忽略处理即可 ——
		// 它的意见会在二轮派发时随全量拉取一并处理。
		tk, err := in.Tasks.TransitionFrom(ctx, target.ID, task.StatePROpen, task.StateReviewFeedback, reviewActor,
			&task.TransitionOpts{Payload: map[string]any{
				"reason":       "pr_review",
				"github_event": ev.Event,
				"delivery":     deliveryID,
				"pr_number":    ev.PRNumber,
				payloadReview:  review,
			}})
		if err != nil {
			if out, ok := raceIgnored(target.ID, err); ok {
				return out, nil
			}
			return ReviewOutcome{}, err
		}
		target, recorded = tk, true
	}

	if in.Workspaces != nil && !in.Workspaces.ReviewWorkspaceUsable(ctx, target) {
		return ReviewOutcome{Status: ReviewParked, TaskID: target.ID,
			Reason: "任务的工作区现场已不可用（目录/分支缺失），review 二轮无法续跑；" +
				"远端分支已有第一轮提交，从头重建的推送会被拒，停在 review_feedback 等人工处理"}, nil
	}

	// 约束①（docs/02-design.md §3）：二轮必须 resume 原会话。转移层也会
	// 以 ErrSessionRequired 拒绝（RequiresSession 覆盖 review_feedback → queued），
	// 这里先判一次，是为了给出可读的结论，而不是把一个预期内的拒绝当错误。
	if target.AgentSessionID == nil || *target.AgentSessionID == "" {
		return ReviewOutcome{Status: ReviewParked, TaskID: target.ID,
			Reason: "任务没有可 resume 的 agent 会话（约束①：review 二轮必须续原会话），停在 review_feedback 等人工处理"}, nil
	}

	payload := map[string]any{
		"reason": "pr_review",
		// 派发侧（cmd/lathe runOneClaimed）从事件流回读 mode 与
		// interrupted_state。interrupted_state 显式置空：否则会回读到本任务
		// 更早一次崩溃恢复留下的旧值，被当成「二轮途中崩溃」。
		"mode":              string(RetryReview),
		"interrupted_state": "",
	}
	if !recorded {
		// 任务本就停在 review_feedback（上次停车，或上次第二步失败）：
		// 意见记在这一次转移上。
		payload["github_event"] = ev.Event
		payload["delivery"] = deliveryID
		payload["pr_number"] = ev.PRNumber
		payload[payloadReview] = review
	}
	if _, err := in.Tasks.TransitionFrom(ctx, target.ID, task.StateReviewFeedback, task.StateQueued, reviewActor,
		&task.TransitionOpts{Payload: payload}); err != nil {
		if errors.Is(err, task.ErrSessionRequired) {
			return ReviewOutcome{Status: ReviewParked, TaskID: target.ID,
				Reason: "任务没有可 resume 的 agent 会话（约束①），停在 review_feedback 等人工处理"}, nil
		}
		if out, ok := raceIgnored(target.ID, err); ok {
			return out, nil
		}
		return ReviewOutcome{}, err
	}

	slog.Info("PR review 意见已回流，任务交给调度器派发二轮",
		"task", target.ID, "repo", ev.Repo, "pr", ev.PRNumber, "key", ev.Key, "author", ev.Author)
	return ReviewOutcome{Status: ReviewQueued, TaskID: target.ID}, nil
}

// raceIgnored 把「并发下状态已被别人改掉」翻译成安全忽略。
func raceIgnored(taskID int64, err error) (ReviewOutcome, bool) {
	var mismatch task.ErrStateMismatch
	if errors.As(err, &mismatch) {
		return ReviewOutcome{Status: ReviewIgnored, TaskID: taskID,
			Reason: fmt.Sprintf("任务状态刚变为 %s，本次意见不单独回流", mismatch.Got)}, true
	}
	var illegal task.ErrIllegalTransition
	if errors.As(err, &illegal) {
		return ReviewOutcome{Status: ReviewIgnored, TaskID: taskID,
			Reason: fmt.Sprintf("任务处于 %s，不接收 review 回流", illegal.From)}, true
	}
	return ReviewOutcome{}, false
}

func reviewPayloadOf(ev *github.ReviewEvent) map[string]any {
	m := map[string]any{
		"key":    ev.Key,
		"author": ev.Author,
		"body":   truncate(ev.Body, maxReviewBodyRunes),
	}
	if ev.Path != "" {
		m["path"] = ev.Path
	}
	if ev.Line > 0 {
		m["line"] = ev.Line
	}
	if ev.URL != "" {
		m["url"] = ev.URL
	}
	if ev.ReviewState != "" {
		m["state"] = ev.ReviewState
	}
	return m
}

// reviewKeySeen 报告事件流里是否已出现过这条意见：回流时记下的，或某轮实现处理过的。
func reviewKeySeen(events []task.Event, key string) bool {
	if key == "" {
		return false
	}
	if processedReviewKeys(events)[key] {
		return true
	}
	for _, c := range intakeReviewComments(events) {
		if c.Key == key {
			return true
		}
	}
	return false
}

// processedReviewKeys 汇总历次实现阶段已处理过的意见 Key。
func processedReviewKeys(events []task.Event) map[string]bool {
	out := map[string]bool{}
	for _, e := range events {
		raw, ok := e.Payload[payloadReviewKeys].([]any)
		if !ok {
			continue
		}
		for _, v := range raw {
			if s, ok := v.(string); ok && s != "" {
				out[s] = true
			}
		}
	}
	return out
}

// intakeReviewComments 还原 AcceptReview 记进事件流的触发意见（按时间序）。
// GitHub API 不可用时，二轮靠它兜底。
func intakeReviewComments(events []task.Event) []github.ReviewComment {
	var out []github.ReviewComment
	for _, e := range events {
		m, ok := e.Payload[payloadReview].(map[string]any)
		if !ok {
			continue
		}
		str := func(k string) string { s, _ := m[k].(string); return s }
		c := github.ReviewComment{
			Key: str("key"), Author: str("author"), Body: str("body"),
			Path: str("path"), URL: str("url"),
		}
		if f, ok := m["line"].(float64); ok { // JSON 数字回读为 float64
			c.Line = int(f)
		}
		if c.Key != "" {
			out = append(out, c)
		}
	}
	return out
}

// ReviewFeedbackSource 是 GitHub 客户端的可选能力（*github.Client 实现）：
// 拉取 PR 上的全部意见。
//
// 不并进 GitHubAPI 接口：那会让每个测试假件都得实现它，而它只在 review
// 二轮用到。类型断言不中（假件、或将来别的实现）时退回事件流里记下的
// 触发意见，二轮照样能跑，只是看不到同一次 review 里的其它条目。
type ReviewFeedbackSource interface {
	ReviewFeedback(ctx context.Context, providerRepo string, number int) (*github.ReviewFeedback, error)
}

// pendingReview 是本轮要交给 agent 的意见。
type pendingReview struct {
	Comments []github.ReviewComment
	// Omitted 是超过 maxReviewItems 未列入本轮的条数（留给下一轮）。
	Omitted int
	// Source 说明意见从哪来："github"（API 全量）或 "events"（兜底）。
	Source string
}

// Keys 是本轮处理的意见 Key，落进实现阶段转移的 payload。
func (r *pendingReview) Keys() []string {
	if r == nil {
		return nil
	}
	keys := make([]string, 0, len(r.Comments))
	for _, c := range r.Comments {
		keys = append(keys, c.Key)
	}
	return keys
}

// collectReview 收集本轮待处理的 review 意见：全部意见 − 自身 − 已处理过的。
func (p *Pipeline) collectReview(rc *runCtx) *pendingReview {
	events, err := p.Tasks.Events(rc.ctx, rc.tk.ID)
	if err != nil {
		slog.Warn("读取任务事件失败，review 意见无法去重", "task", rc.tk.ID, "err", err)
	}
	processed := processedReviewKeys(events)

	var (
		all    []github.ReviewComment
		self   string
		source = "events"
	)
	if src, ok := rc.gh.(ReviewFeedbackSource); ok && rc.tk.PRNumber != nil {
		fb, ferr := src.ReviewFeedback(rc.ctx, rc.params.Repo.ProviderRepo, *rc.tk.PRNumber)
		if ferr == nil {
			all, self, source = fb.Comments, fb.PRAuthor, "github"
		} else {
			slog.Warn("拉取 PR review 意见失败，退回事件流里记下的触发意见",
				"task", rc.tk.ID, "pr", *rc.tk.PRNumber, "err", ferr)
		}
	}
	if source == "events" {
		all = intakeReviewComments(events) // 回流时已排除过自身
	}

	out := &pendingReview{Source: source}
	for _, c := range all {
		if c.Key == "" || processed[c.Key] || strings.TrimSpace(c.Body) == "" {
			continue
		}
		if self != "" && strings.EqualFold(c.Author, self) {
			continue
		}
		if len(out.Comments) >= maxReviewItems {
			out.Omitted++
			continue
		}
		out.Comments = append(out.Comments, c)
	}
	return out
}

// reviewPromptOf 把待处理意见渲染成 review 二轮的 prompt。
func reviewPromptOf(r *pendingReview) string {
	items := make([]string, 0, len(r.Comments))
	for _, c := range r.Comments {
		items = append(items, formatReviewComment(c))
	}
	prompt := ReviewPrompt(items)
	if r.Omitted > 0 {
		prompt += fmt.Sprintf("（另有 %d 条意见未列入本轮，会在下一轮处理，本轮不必理会。）\n", r.Omitted)
	}
	return prompt
}

func formatReviewComment(c github.ReviewComment) string {
	who := c.Author
	if who == "" {
		who = "（未知）"
	}
	var head string
	switch {
	case strings.HasPrefix(c.Key, "inline:") && c.Path != "":
		loc := c.Path
		if c.Line > 0 {
			loc = fmt.Sprintf("%s:%d", c.Path, c.Line)
		}
		head = fmt.Sprintf("@%s 在 `%s` 的行内评论", who, loc)
	case strings.HasPrefix(c.Key, "review:"):
		head = fmt.Sprintf("@%s 的 review 总评", who)
	default:
		head = fmt.Sprintf("@%s 的 PR 评论", who)
	}
	return head + "：\n" + truncate(c.Body, maxReviewBodyRunes)
}
