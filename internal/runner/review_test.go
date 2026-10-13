package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
	"github.com/zichuanwangcloud-gif/lathe/internal/integration/github"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// review_test.go 覆盖 PR 评论回流（docs/02-design.md §7，A2）：
// ReviewIntake 的守卫与幂等，以及从 pr_open 出发的 review 二轮端到端。

const reviewPR = 11

// prOpenTask 把 fixture 任务带到「第一轮已开出 PR」的真实样子：
// worktree 在、改动已提交、持有会话与 pr_number、状态 pr_open。
// 返回 nil 现场句柄时表示调用方只测状态机（不建 worktree）。
func prOpenTask(t *testing.T, p *Pipeline, m *task.Machine, taskID int64, src string, repo RepoConfig, withSession bool) *Worktree {
	t.Helper()
	ctx := context.Background()

	opts := &task.TransitionOpts{}
	var wt *Worktree
	if p != nil {
		var err error
		wt, err = p.Worktrees.Create(ctx, CreateParams{
			Repo: repo, CloneURL: src, Kind: KindFix, IssueKey: "CR-777", Title: "t", TaskID: taskID,
		})
		if err != nil {
			t.Fatalf("造现场：Create 失败: %v", err)
		}
		writeFile(t, filepath.Join(wt.Path, "README.md"), "# src\n\n第一轮：补充导入失败说明\n")
		if err := p.Worktrees.Commit(ctx, wt, "docs: 第一轮"); err != nil {
			t.Fatalf("造现场：Commit 失败: %v", err)
		}
		kind, tier := "fix", "light"
		opts = &task.TransitionOpts{WorktreePath: &wt.Path, BranchName: &wt.Branch, TaskKind: &kind, VerifyTier: &tier}
	}
	if withSession {
		sess := "sess-original"
		opts.AgentSessionID = &sess
	}
	if _, err := m.Transition(ctx, taskID, task.StateImplementing, "test", opts); err != nil {
		t.Fatalf("转 implementing 失败: %v", err)
	}
	for _, s := range []task.State{task.StateVerifying, task.StatePROpen} {
		if _, err := m.Transition(ctx, taskID, s, "test", nil); err != nil {
			t.Fatalf("转 %s 失败: %v", s, err)
		}
	}
	if err := m.SetPRNumber(ctx, taskID, reviewPR); err != nil {
		t.Fatalf("SetPRNumber 失败: %v", err)
	}
	return wt
}

func ownerOf(t *testing.T, m *task.Machine, taskID int64) int64 {
	t.Helper()
	tk, err := m.Get(context.Background(), taskID)
	if err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	return tk.UserID
}

func inlineEvent(key, author, body string) *github.ReviewEvent {
	return &github.ReviewEvent{
		Event: github.EventReviewComment, Action: "created",
		Repo: "acme/demo", PRNumber: reviewPR, PRAuthor: "lathe-bot",
		Key: key, Author: author, Body: body, Path: "README.md", Line: 3,
	}
}

// A2 验收 2：评论事件 → pr_open → review_feedback → queued，任务上下文
// （事件流）含评论正文，且派发侧回读得到 review 模式。
func TestReviewIntakeQueuesTaskWithCommentBody(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)
	prOpenTask(t, nil, m, taskID, src, repo, true)
	in := &ReviewIntake{Tasks: m}

	out, err := in.AcceptReview(context.Background(), ownerOf(t, m, taskID),
		inlineEvent("inline:1", "alice", "错误被吞掉了，至少记一条日志"), "deliv-1")
	if err != nil {
		t.Fatalf("AcceptReview 失败: %v", err)
	}
	if out.Status != ReviewQueued || out.TaskID != taskID {
		t.Fatalf("outcome = %+v，want queued 任务 #%d", out, taskID)
	}

	tk, _ := m.Get(context.Background(), taskID)
	if tk.State != task.StateQueued {
		t.Errorf("state = %s，want queued（交给领单调度器）", tk.State)
	}

	events := retryEvents(t, m, taskID)
	var sawFeedback bool
	for _, e := range events {
		if e.ToState == task.StateReviewFeedback {
			sawFeedback = true
			rv, _ := e.Payload["review"].(map[string]any)
			if rv["body"] != "错误被吞掉了，至少记一条日志" || rv["author"] != "alice" || rv["key"] != "inline:1" {
				t.Errorf("review_feedback 事件应含评论正文/作者/Key，得到 %v", e.Payload)
			}
		}
	}
	if !sawFeedback {
		t.Error("应经过 pr_open → review_feedback（状态机 §7 的语义边），事件流里没有")
	}
	if got := lastStr(events, "mode"); got != string(RetryReview) {
		t.Errorf("事件流最近的 mode = %q，want review（派发侧据此走二轮）", got)
	}
	if got, ok := lastVal(events, "interrupted_state"); !ok || got != "" {
		t.Errorf("回流必须把 interrupted_state 显式置空，防止回读到旧的崩溃恢复值，得到 %v/%v", got, ok)
	}
}

// A2 验收 1（业务层）：同一条意见经不同投递再次到达，不产生第二次转移。
func TestReviewIntakeSameCommentTwiceIsIdempotent(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)
	prOpenTask(t, nil, m, taskID, src, repo, true)
	in := &ReviewIntake{Tasks: m}
	owner := ownerOf(t, m, taskID)
	ev := inlineEvent("inline:2", "alice", "改一下")

	if out, err := in.AcceptReview(context.Background(), owner, ev, "deliv-a"); err != nil || out.Status != ReviewQueued {
		t.Fatalf("首次应 queued：%+v %v", out, err)
	}
	before := len(retryEvents(t, m, taskID))

	// 让任务回到 pr_open（模拟二轮早已结束），再投递同一条意见。
	// 只靠状态守卫挡不住这一种：任务确实又站在 pr_open 了。
	for _, s := range []task.State{task.StateImplementing, task.StateVerifying, task.StatePROpen} {
		if _, err := m.Transition(context.Background(), taskID, s, "test", nil); err != nil {
			t.Fatalf("转 %s 失败: %v", s, err)
		}
	}
	before += 3

	out, err := in.AcceptReview(context.Background(), owner, ev, "deliv-b")
	if err != nil {
		t.Fatalf("重复意见不应报错: %v", err)
	}
	if out.Status != ReviewIgnored {
		t.Errorf("重复意见应被忽略，得到 %+v", out)
	}
	if after := len(retryEvents(t, m, taskID)); after != before {
		t.Errorf("重复意见产生了 %d 次额外转移", after-before)
	}
	if tk, _ := m.Get(context.Background(), taskID); tk.State != task.StatePROpen {
		t.Errorf("state = %s，重复意见不应把任务再拉走", tk.State)
	}
}

// A2 验收 3：终态任务收到评论事件 → 忽略且不报错，状态不变（终态不得复活）。
func TestReviewIntakeIgnoresTerminalTasks(t *testing.T) {
	for _, terminal := range []task.State{task.StateMerged, task.StateCancelled, task.StateFailed} {
		t.Run(string(terminal), func(t *testing.T) {
			_, m, taskID, repo, src := pipelineFixture(t)
			prOpenTask(t, nil, m, taskID, src, repo, true)
			if _, err := m.Transition(context.Background(), taskID, terminal, "test", nil); err != nil {
				t.Fatalf("转 %s 失败: %v", terminal, err)
			}
			before := len(retryEvents(t, m, taskID))

			out, err := (&ReviewIntake{Tasks: m}).AcceptReview(context.Background(), ownerOf(t, m, taskID),
				inlineEvent("inline:3", "alice", "合并后才看到的意见"), "deliv-t")
			if err != nil {
				t.Fatalf("终态任务收到评论不应报错: %v", err)
			}
			if out.Status != ReviewIgnored || !strings.Contains(out.Reason, string(terminal)) {
				t.Errorf("outcome = %+v，want ignored 且理由点明 %s", out, terminal)
			}
			tk, _ := m.Get(context.Background(), taskID)
			if tk.State != terminal {
				t.Errorf("state = %s，终态 %s 被复活了", tk.State, terminal)
			}
			if after := len(retryEvents(t, m, taskID)); after != before {
				t.Errorf("终态任务产生了 %d 条新事件", after-before)
			}
		})
	}
}

// 非 pr_open/review_feedback 的活任务（如二轮正在跑）收到评论：安全忽略。
func TestReviewIntakeIgnoresInFlightTask(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)
	prOpenTask(t, nil, m, taskID, src, repo, true)
	owner := ownerOf(t, m, taskID)
	in := &ReviewIntake{Tasks: m}
	if out, _ := in.AcceptReview(context.Background(), owner, inlineEvent("inline:4", "alice", "第一条"), "d1"); out.Status != ReviewQueued {
		t.Fatalf("首条应 queued：%+v", out)
	}
	out, err := in.AcceptReview(context.Background(), owner, inlineEvent("inline:5", "bob", "第二条"), "d2")
	if err != nil || out.Status != ReviewIgnored {
		t.Errorf("任务已在 queued，第二条应安全忽略（二轮派发时会全量拉取），得到 %+v %v", out, err)
	}
}

// A2 验收 4：A 用户的 PR 事件碰不到 B 用户的任务 —— 即便两人登记了同名
// 仓库、PR 号也相同。
func TestReviewIntakeCrossOwnerIsolation(t *testing.T) {
	_, m, taskA, repo, src := pipelineFixture(t)
	_, _, taskB, _, _ := pipelineFixture(t) // 另一个用户，同样是 acme/demo
	prOpenTask(t, nil, m, taskA, src, repo, true)
	prOpenTask(t, nil, m, taskB, src, repo, true) // 同号 PR #11
	ownerA, ownerB := ownerOf(t, m, taskA), ownerOf(t, m, taskB)
	if ownerA == ownerB {
		t.Fatal("fixture 应给出两个不同用户")
	}
	in := &ReviewIntake{Tasks: m}

	// A 的回调地址收到事件：只能动 A 的任务
	out, err := in.AcceptReview(context.Background(), ownerA, inlineEvent("inline:6", "alice", "A 的意见"), "dA")
	if err != nil || out.Status != ReviewQueued || out.TaskID != taskA {
		t.Fatalf("A 的事件应只回流到 A 的任务 #%d，得到 %+v %v", taskA, out, err)
	}
	if tk, _ := m.Get(context.Background(), taskB); tk.State != task.StatePROpen {
		t.Errorf("B 的任务被 A 的事件推动了：state = %s", tk.State)
	}

	// 不属于该用户的仓库：与「不存在」同一个回答
	stranger := ownerA + ownerB + 1_000_000
	out, err = in.AcceptReview(context.Background(), stranger, inlineEvent("inline:7", "eve", "x"), "dX")
	if err != nil || out.Status != ReviewIgnored || out.TaskID != 0 {
		t.Errorf("非属主的事件应忽略且不透露任务 id，得到 %+v %v", out, err)
	}
	if tk, _ := m.Get(context.Background(), taskB); tk.State != task.StatePROpen {
		t.Errorf("B 的任务被陌生用户的事件推动了：state = %s", tk.State)
	}
}

// 约束①：没有会话可 resume 的任务停在 review_feedback，不自动派发。
func TestReviewIntakeParksWithoutSession(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)
	prOpenTask(t, nil, m, taskID, src, repo, false)

	out, err := (&ReviewIntake{Tasks: m}).AcceptReview(context.Background(), ownerOf(t, m, taskID),
		inlineEvent("inline:8", "alice", "改一下"), "d")
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if out.Status != ReviewParked {
		t.Errorf("无会话应停车，得到 %+v", out)
	}
	tk, _ := m.Get(context.Background(), taskID)
	if tk.State != task.StateReviewFeedback {
		t.Errorf("state = %s，want review_feedback（意见已记下，等人处理）", tk.State)
	}
}

type fakeWorkspaces struct{ usable bool }

func (f fakeWorkspaces) ReviewWorkspaceUsable(ctx context.Context, tk *task.Task) bool {
	return f.usable
}

// 现场已不可用的任务：停在 review_feedback 等人处理，不交给调度器 ——
// 交出去只会从头重建、白跑一整轮后在推送时撞上远端已有的第一轮提交。
func TestReviewIntakeParksWhenWorkspaceGone(t *testing.T) {
	cases := []struct {
		name      string
		usable    bool
		wantState task.State
		want      ReviewStatus
	}{
		{"现场可用：交给调度器", true, task.StateQueued, ReviewQueued},
		{"现场已不可用：停车等人，意见照样记下", false, task.StateReviewFeedback, ReviewParked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, m, taskID, repo, src := pipelineFixture(t)
			prOpenTask(t, nil, m, taskID, src, repo, true)
			in := &ReviewIntake{Tasks: m, Workspaces: fakeWorkspaces{usable: tc.usable}}

			out, err := in.AcceptReview(context.Background(), ownerOf(t, m, taskID),
				inlineEvent("inline:w", "alice", "改一下"), "d")
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if out.Status != tc.want {
				t.Errorf("status = %s（%s），want %s", out.Status, out.Reason, tc.want)
			}
			if tk, _ := m.Get(context.Background(), taskID); tk.State != tc.wantState {
				t.Errorf("state = %s，want %s", tk.State, tc.wantState)
			}
			if !reviewKeySeen(retryEvents(t, m, taskID), "inline:w") {
				t.Error("无论是否停车，意见都应记进事件流（人接手时要看得到）")
			}
		})
	}
}

// 同一次 review 的多条投递并发到达：恰好一条把任务带走，其余安全忽略，
// 全程不报错（-race 下跑）。
func TestReviewIntakeConcurrentDeliveries(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)
	prOpenTask(t, nil, m, taskID, src, repo, true)
	in := &ReviewIntake{Tasks: m}
	owner := ownerOf(t, m, taskID)

	const n = 6
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		queued int
	)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			out, err := in.AcceptReview(context.Background(), owner,
				inlineEvent("inline:c"+string(rune('0'+i)), "alice", "并发意见"), "dc")
			if err != nil {
				t.Errorf("并发投递 %d 报错: %v", i, err)
				return
			}
			if out.Status == ReviewQueued {
				mu.Lock()
				queued++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if queued != 1 {
		t.Errorf("恰好一条投递应把任务交给调度器，实际 %d 条", queued)
	}
	if tk, _ := m.Get(context.Background(), taskID); tk.State != task.StateQueued {
		t.Errorf("state = %s，want queued", tk.State)
	}
}

// ---------------------------------------------------------------- 二轮端到端

// feedbackGitHub 在 fakeGitHub 之上实现可选的 ReviewFeedbackSource。
type feedbackGitHub struct {
	*fakeGitHub
	fb  *github.ReviewFeedback
	err error
}

func (f *feedbackGitHub) ReviewFeedback(ctx context.Context, repo string, n int) (*github.ReviewFeedback, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.fb, nil
}

// reviewRoundPlan 按派发侧的真实路径产出二轮计划：PlanRetry(RetryReview, 体检结果)。
func reviewRoundPlan(t *testing.T, p *Pipeline, m *task.Machine, taskID int64, repo RepoConfig) *RetryPlan {
	t.Helper()
	tk, _ := m.Get(context.Background(), taskID)
	base, _ := repo.BaseBranch(KindFix)
	plan := PlanRetry(RetryReview, RetryInput{
		HasSession: tk.AgentSessionID != nil && *tk.AgentSessionID != "",
		WT:         p.Worktrees.Inspect(context.Background(), repo.ProviderRepo, deref(tk.WorktreePath), deref(tk.BranchName), base),
	})
	return &plan
}

func TestPipelineReviewRoundResumesWithComments(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{pr: &github.PullRequest{URL: "https://x/pr/11", Number: reviewPR, Existing: true}}
	ag := &fakeAgent{
		results: []*agent.Result{{Success: true, Text: "按意见补了日志说明"}},
		mutate: []func(string) error{func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "README.md"),
				[]byte("# src\n\n第一轮：补充导入失败说明\n\n第二轮：失败时记一条日志\n"), 0o644)
		}},
	}
	p := newPipeline(t, m, lin, gh, ag, &fakeNotifier{})
	// API 全量：除触发的那条外还有同一次 review 的另一条、自身的一条、已处理过的一条
	p.Clients = &fakeClients{lin: lin, gh: &feedbackGitHub{fakeGitHub: gh, fb: &github.ReviewFeedback{
		PRAuthor: "lathe-bot",
		Comments: []github.ReviewComment{
			{Key: "inline:21", Author: "alice", Body: "错误被吞掉了，至少记一条日志", Path: "README.md", Line: 3},
			{Key: "review:31", Author: "alice", Body: "整体方向对，补完日志即可"},
			{Key: "issue:41", Author: "Lathe-Bot", Body: "（自身发言，必须排除）"},
			{Key: "inline:20", Author: "bob", Body: "（上一轮已处理，必须排除）"},
		},
	}}}

	prOpenTask(t, p, m, taskID, src, repo, true)
	// 「上一轮已处理过 inline:20」：在事件流里留下记账
	if _, err := m.Transition(context.Background(), taskID, task.StateReviewFeedback, "test",
		&task.TransitionOpts{Payload: map[string]any{"review_comment_keys": []string{"inline:20"}}}); err != nil {
		t.Fatalf("造历史记账失败: %v", err)
	}
	if _, err := m.Transition(context.Background(), taskID, task.StateImplementing, "test", nil); err != nil {
		t.Fatal(err)
	}
	for _, s := range []task.State{task.StateVerifying, task.StatePROpen} {
		if _, err := m.Transition(context.Background(), taskID, s, "test", nil); err != nil {
			t.Fatal(err)
		}
	}

	out, err := (&ReviewIntake{Tasks: m}).AcceptReview(context.Background(), ownerOf(t, m, taskID),
		inlineEvent("inline:21", "alice", "错误被吞掉了，至少记一条日志"), "deliv-e2e")
	if err != nil || out.Status != ReviewQueued {
		t.Fatalf("回流失败：%+v %v", out, err)
	}

	plan := reviewRoundPlan(t, p, m, taskID, repo)
	if !plan.Review || !plan.ResumeSession || plan.Entry != EntryImplement {
		t.Fatalf("派发计划应为 review 二轮 + resume，得到 %+v", plan)
	}
	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Retry: plan,
	}); err != nil {
		t.Fatalf("二轮执行失败: %v", err)
	}

	// 1) agent：恰好一次，--resume 原会话，prompt 带全部待处理意见、不带排除项
	if len(ag.calls) != 1 {
		t.Fatalf("应只调用 1 次 agent，实际 %d", len(ag.calls))
	}
	call := ag.calls[0]
	if !call.Resume || call.SessionID != "sess-original" {
		t.Errorf("应 --resume sess-original，得到 Resume=%v Session=%s", call.Resume, call.SessionID)
	}
	for _, want := range []string{"review 意见", "错误被吞掉了，至少记一条日志", "`README.md:3`", "整体方向对", "@alice"} {
		if !strings.Contains(call.Prompt, want) {
			t.Errorf("二轮 prompt 应包含 %q\n---\n%s", want, call.Prompt)
		}
	}
	for _, unwanted := range []string{"自身发言", "上一轮已处理"} {
		if strings.Contains(call.Prompt, unwanted) {
			t.Errorf("二轮 prompt 不应包含 %q", unwanted)
		}
	}

	// 2) 重新验证后回到 pr_open，会话未被换掉
	final, _ := m.Get(context.Background(), taskID)
	if final.State != task.StatePROpen {
		t.Errorf("state = %s，want pr_open", final.State)
	}
	if final.AgentSessionID == nil || *final.AgentSessionID != "sess-original" {
		t.Errorf("会话应保持 sess-original，得到 %v", final.AgentSessionID)
	}
	events := retryEvents(t, m, taskID)
	var sawVerify bool
	for _, e := range events[len(events)-3:] {
		if e.ToState == task.StateVerifying {
			sawVerify = true
		}
	}
	if !sawVerify {
		t.Error("二轮必须重新经过 verifying 才能回到 pr_open（验证体系不可削弱）")
	}

	// 3) 本轮处理的意见记为已处理
	processed := processedReviewKeys(events)
	for _, k := range []string{"inline:21", "review:31"} {
		if !processed[k] {
			t.Errorf("意见 %s 应记为已处理，processed = %v", k, processed)
		}
	}
	if processed["issue:41"] {
		t.Error("自身发言不应被记为已处理（它根本不该进 prompt）")
	}

	// 4) 回帖措辞：更新 PR，而非「开出 PR」
	if len(lin.comments) == 0 || !strings.Contains(lin.comments[len(lin.comments)-1], "按 review 意见更新 PR") {
		t.Errorf("二轮结束的回帖应说明是按 review 意见更新，得到 %v", lin.comments)
	}
}

// GitHub API 不可用时，二轮退回事件流里记下的触发意见，照样能跑。
func TestPipelineReviewRoundFallsBackToRecordedComment(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)
	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{pr: &github.PullRequest{URL: "https://x/pr/11", Number: reviewPR, Existing: true}}
	ag := &fakeAgent{
		results: []*agent.Result{{Success: true, Text: "改好了"}},
		mutate: []func(string) error{func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "README.md"), []byte("# src\n\n二轮兜底\n"), 0o644)
		}},
	}
	p := newPipeline(t, m, lin, gh, ag, &fakeNotifier{}) // fakeGitHub 不实现 ReviewFeedbackSource
	prOpenTask(t, p, m, taskID, src, repo, true)

	if out, err := (&ReviewIntake{Tasks: m}).AcceptReview(context.Background(), ownerOf(t, m, taskID),
		inlineEvent("inline:51", "alice", "只有事件流里有这条"), "d"); err != nil || out.Status != ReviewQueued {
		t.Fatalf("回流失败：%+v %v", out, err)
	}
	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Retry: reviewRoundPlan(t, p, m, taskID, repo),
	}); err != nil {
		t.Fatalf("二轮执行失败: %v", err)
	}
	if len(ag.calls) != 1 || !strings.Contains(ag.calls[0].Prompt, "只有事件流里有这条") {
		t.Fatalf("应以事件流兜底的意见驱动 agent，calls=%d", len(ag.calls))
	}
	if final, _ := m.Get(context.Background(), taskID); final.State != task.StatePROpen {
		t.Errorf("state = %s，want pr_open", final.State)
	}
}

// 没有待处理意见（都已处理过 / 触发的那条被删了）：不唤起 agent，只重新验证。
func TestPipelineReviewRoundWithoutPendingCommentsOnlyReverifies(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)
	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{pr: &github.PullRequest{URL: "https://x/pr/11", Number: reviewPR, Existing: true}}
	ag := &fakeAgent{}
	p := newPipeline(t, m, lin, gh, ag, &fakeNotifier{})
	p.Clients = &fakeClients{lin: lin, gh: &feedbackGitHub{fakeGitHub: gh,
		fb: &github.ReviewFeedback{PRAuthor: "lathe-bot"}}} // 触发的意见在 GitHub 上已被删
	prOpenTask(t, p, m, taskID, src, repo, true)

	if out, err := (&ReviewIntake{Tasks: m}).AcceptReview(context.Background(), ownerOf(t, m, taskID),
		inlineEvent("inline:61", "alice", "随后被删掉的意见"), "d"); err != nil || out.Status != ReviewQueued {
		t.Fatalf("回流失败：%+v %v", out, err)
	}
	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Retry: reviewRoundPlan(t, p, m, taskID, repo),
	}); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if len(ag.calls) != 0 {
		t.Errorf("没有待处理意见时不应唤起 agent，实际调用 %d 次", len(ag.calls))
	}
	if final, _ := m.Get(context.Background(), taskID); final.State != task.StatePROpen {
		t.Errorf("state = %s，want pr_open（重新验证后回到 pr_open）", final.State)
	}
}

func lastVal(events []task.Event, key string) (any, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		if v, ok := events[i].Payload[key]; ok {
			return v, true
		}
	}
	return nil, false
}

func lastStr(events []task.Event, key string) string {
	v, _ := lastVal(events, key)
	s, _ := v.(string)
	return s
}
