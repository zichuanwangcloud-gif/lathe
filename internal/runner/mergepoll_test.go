package runner

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zichuanwangcloud-gif/lathe/internal/integration/github"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// 本文件测 F2.3-AC2（PR 被关闭未合并 → blocked_dep）的完整驱动链路：
// MergePoller.pollOnce → pollTask → handleClosedUnmerged。这条逻辑在
// mergepoll.go 里早就写好了，但在本文件新增之前从未被任何测试驱动过。
//
// 不涉及 worktree/git（PR 被关闭未合并这件事本身与工作区状态无关，
// task/state.go 的 queued→blocked_dep 边也不要求有 worktree），因此
// 用比 rebase_followup_test.go 更轻的夹具：只建 user/repo/task，状态
// 用 Transition 直接摆到 pr_open，不跑真实 Pipeline.Execute。

// mergepollFixture 建最小夹具：user/repo，供 pollOnce/handleClosedUnmerged
// 驱动测试用——与 rebaseFollowupFixture 同一套手法，只是不需要真实源
// 仓库。
func mergepollFixture(t *testing.T) (pool *pgxpool.Pool, m *task.Machine, userID, repoID int64, repo RepoConfig) {
	t.Helper()
	pool = testPoolForPipeline(t)
	m = task.NewMachine(pool)
	ctx := context.Background()

	providerRepo := "acme/mergepoll-" + t.Name()
	email := "mergepoll-" + t.Name() + "@example.com"
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) ON CONFLICT (email) DO UPDATE SET updated_at=now() RETURNING id`,
		email).Scan(&userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2)
		 ON CONFLICT (user_id, provider_repo) DO UPDATE SET updated_at=now() RETURNING id`,
		userID, providerRepo).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})
	return pool, m, userID, repoID, DefaultRepoConfig(providerRepo)
}

// toPROpenNoWorktree 把任务从 queued 直接转到 pr_open，跳过
// triaging/implementing 等中间阶段——全程只走 task/state.go 里已经
// 存在的合法边（queued→verifying、verifying→pr_open），本文件的场景
// 不涉及真实 worktree/git，不需要经过完整实现流程。
func toPROpenNoWorktree(t *testing.T, m *task.Machine, id int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := m.Transition(ctx, id, task.StateVerifying, "system", nil); err != nil {
		t.Fatalf("任务 %d 转 verifying 失败: %v", id, err)
	}
	if _, err := m.Transition(ctx, id, task.StatePROpen, "system", nil); err != nil {
		t.Fatalf("任务 %d 转 pr_open 失败: %v", id, err)
	}
}

// TestMergePollHandleClosedUnmergedPropagatesBlockedDep 构造一条
// task1(pr_open, PR 将被关闭未合并)→task2(queued，直接后继)→
// task3(queued，间接后继) 的链，让 fakeGitHub.GetPRInfo 对 task1 返回
// {State: "closed", Merged: false}，调用 MergePoller.pollOnce(ctx)，
// 断言：
//
//   - task1 转 cancelled；
//   - task2（直接后继）、task3（间接后继）都转 blocked_dep；
//   - task2/task3 都收到回帖说明因前驱 PR 被关闭而阻塞。
func TestMergePollHandleClosedUnmergedPropagatesBlockedDep(t *testing.T) {
	_, m, userID, repoID, repo := mergepollFixture(t)
	ctx := context.Background()

	task1, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: "MP-1", ExternalID: "uuid-mp-1",
	})
	if err != nil {
		t.Fatalf("建 task1 失败: %v", err)
	}
	task2, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: "MP-2", ExternalID: "uuid-mp-2",
		DependsOn: &task1.ID,
	})
	if err != nil {
		t.Fatalf("建 task2 失败: %v", err)
	}
	task3, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: "MP-3", ExternalID: "uuid-mp-3",
		DependsOn: &task2.ID,
	})
	if err != nil {
		t.Fatalf("建 task3 失败: %v", err)
	}

	toPROpenNoWorktree(t, m, task1.ID)
	if err := m.SetPRNumber(ctx, task1.ID, 42); err != nil {
		t.Fatalf("设置 task1.pr_number 失败: %v", err)
	}

	lin := newRebaseFollowupLinear()
	lin.add(rebaseFollowupIssue("uuid-mp-1", "MP-1"))
	lin.add(rebaseFollowupIssue("uuid-mp-2", "MP-2"))
	lin.add(rebaseFollowupIssue("uuid-mp-3", "MP-3"))

	gh := &fakeGitHub{prInfo: &github.PRInfo{Number: 42, Merged: false, State: "closed"}}
	notifier := &fakeNotifier{}

	mp := &MergePoller{
		Tasks:         m,
		ClientFactory: fixedClientFactory{clients: &fakeClients{lin: lin, gh: gh}},
		RepoLookup:    repoLookupFixed(repo),
		Notifier:      notifier,
	}

	if err := mp.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce 失败: %v", err)
	}

	// ---- 断言一：task1 转 cancelled ----
	t1After, err := m.Get(ctx, task1.ID)
	if err != nil {
		t.Fatalf("读取 task1 失败: %v", err)
	}
	if t1After.State != task.StateCancelled {
		t.Fatalf("task1 应转 cancelled，实际 state=%s", t1After.State)
	}

	// ---- 断言二：task2（直接后继）转 blocked_dep ----
	t2After, err := m.Get(ctx, task2.ID)
	if err != nil {
		t.Fatalf("读取 task2 失败: %v", err)
	}
	if t2After.State != task.StateBlockedDep {
		t.Fatalf("task2（直接后继）应转 blocked_dep，实际 state=%s", t2After.State)
	}

	// ---- 断言三：task3（间接后继）也转 blocked_dep ----
	t3After, err := m.Get(ctx, task3.ID)
	if err != nil {
		t.Fatalf("读取 task3 失败: %v", err)
	}
	if t3After.State != task.StateBlockedDep {
		t.Fatalf("task3（间接后继）应转 blocked_dep，实际 state=%s", t3After.State)
	}

	// ---- 断言四：task2/task3 都收到回帖，说明因前驱 PR 被关闭而阻塞 ----
	if len(lin.comments["uuid-mp-2"]) == 0 {
		t.Error("task2 应收到阻塞回帖")
	} else {
		body := lin.comments["uuid-mp-2"][len(lin.comments["uuid-mp-2"])-1]
		if !containsAny(body, "关闭", "closed") {
			t.Errorf("阻塞回帖应能看出因前驱 PR 被关闭而阻塞，实际: %s", body)
		}
	}
	if len(lin.comments["uuid-mp-3"]) == 0 {
		t.Error("task3（间接后继）也应收到阻塞回帖")
	}
}

// B1：停在 review_feedback 的任务被合并后，必须照常走 merged 并推进后继。
//
// 修复前 ListOpenPRTasks 只查 state='pr_open'，合并检测对停在
// review_feedback 的任务完全不可见 —— 人在 GitHub 上合并了 PR，任务却
// 永远停在 review_feedback：worktree 不回收、depends_on 后继不推进、
// 看板永远显示待处理，唯一出口是人工取消。这条用例锁死「合并检测对
// review_feedback 同样收敛」。
func TestMergePollMergesTaskParkedInReviewFeedback(t *testing.T) {
	pool, m, userID, repoID, repo := mergepollFixture(t)
	ctx := context.Background()

	// task1：PR 已开出，回流时因无会话停在 review_feedback（parked）
	task1, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: "MP-R1", ExternalID: "uuid-mp-r1",
	})
	if err != nil {
		t.Fatalf("建 task1 失败: %v", err)
	}
	// task2：前驱「合并」才放行的后继 —— 用 depends_on_at='merged' 让
	// 「推进后继」这件事有可观测的判据（若是 pr_open 放行，task2 早在
	// task1 开 PR 时就该放行了，看不出本次推进）。
	task2, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: "MP-R2", ExternalID: "uuid-mp-r2",
		DependsOn: &task1.ID, DependsOnAt: "merged",
	})
	if err != nil {
		t.Fatalf("建 task2 失败: %v", err)
	}

	toPROpenNoWorktree(t, m, task1.ID)
	if err := m.SetPRNumber(ctx, task1.ID, 77); err != nil {
		t.Fatalf("设置 task1.pr_number 失败: %v", err)
	}
	// 回流：pr_open → review_feedback。无会话，所以会停在 parked。
	out, err := (&ReviewIntake{Tasks: m}).AcceptReview(ctx, userID, &github.ReviewEvent{
		Event: github.EventReviewComment, Action: "created",
		Repo: repo.ProviderRepo, PRNumber: 77, PRAuthor: "lathe-bot",
		Key: "inline:900", Author: "alice", Body: "改一下",
	}, "d-parked")
	if err != nil {
		t.Fatalf("回流失败: %v", err)
	}
	if out.Status != ReviewParked {
		t.Fatalf("无会话的任务应停在 parked，得到 %+v", out)
	}

	lin := newRebaseFollowupLinear()
	lin.add(rebaseFollowupIssue("uuid-mp-r1", "MP-R1"))
	lin.add(rebaseFollowupIssue("uuid-mp-r2", "MP-R2"))
	gh := &fakeGitHub{prInfo: &github.PRInfo{Number: 77, State: "closed", Merged: true}}

	wm, err := NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatalf("建工作区管理器失败: %v", err)
	}
	mp := &MergePoller{
		Tasks:         m,
		Worktrees:     wm,
		ClientFactory: fixedClientFactory{clients: &fakeClients{lin: lin, gh: gh}},
		RepoLookup:    repoLookupFixed(repo),
		Notifier:      &fakeNotifier{},
	}
	if err := mp.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce 失败: %v", err)
	}

	// ---- 断言一：parked 的任务被合并检测纳入并转 merged（不再驻留）----
	t1After, err := m.Get(ctx, task1.ID)
	if err != nil {
		t.Fatalf("读取 task1 失败: %v", err)
	}
	if t1After.State != task.StateMerged {
		t.Fatalf("停在 review_feedback 的任务被合并后应转 merged，实际 state=%s（合并检测看不见它 → 死胡同）",
			t1After.State)
	}

	// ---- 断言二：后继真的被放行了（不再是"等前驱合并"）----
	//
	// 判据走真实的就绪判定 ClaimReady，不是读一次状态：depends_on_at=
	// 'merged' 的后继排在 queued 里，放行由 ClaimReady 的 SQL 决定
	// （`p.state = 'merged'`）。这样断言的是「后继现在能开工了」这个
	// 事实本身，而不是某个中间表示。
	//
	// ClaimReady 是全局查询（不按属主过滤），上一轮被中断的测试可能留下
	// queued 行（-p 1 只挡包间并行，挡不住跨轮次残留，见 AGENTS.md §5.5）。
	// 把它们的租约推远一小时，让本次断言只可能领到本用例的任务 —— 不改
	// 它们的状态、不动数据，租约到期后自然回到可领取状态。
	if _, err := pool.Exec(ctx, `
		UPDATE tasks SET lease_expires_at = now() + interval '1 hour'
		WHERE state = 'queued' AND id <> $1`, task2.ID); err != nil {
		t.Fatalf("隔离队列残留失败: %v", err)
	}

	claimed, err := m.ClaimReady(ctx, time.Hour)
	if err != nil {
		t.Fatalf("ClaimReady 失败: %v", err)
	}
	if claimed == nil || claimed.ID != task2.ID {
		t.Errorf("前驱合并后，depends_on_at='merged' 的后继应可被领取（后继被放行 = 合并检测的推进真的生效了），得到 %v", claimed)
	}
}

// MAJOR-2（A2 审计）：rebase 跟进的状态归一不能与 PR 评论回流抢同一行。
//
// 两条路径都要经过 review_feedback：
//
//	rebase 跟进：pr_open → review_feedback → implementing
//	评论回流：   pr_open → review_feedback → queued（mode=review）
//
// 修复前 reenterForVerify 用裸 Transition（不带前置状态），而
// `queued → implementing` 本身是合法边，于是「回流在两步之间把任务转去
// queued」时第二步会【成功】：rebase 跟进把刚被回流送进 queued 的任务
// 又拽到 implementing。结果是 review 二轮被抹掉（任务已不在 queued，
// worker 领不到它），任务停在 implementing 却没有任何执行者在跑它。
//
// 断言：第二步以 ErrStateMismatch 拒绝、任务留在 queued、且没有第二次
// implementing 转移。
func TestMergePollReenterDoesNotStealTaskFromReviewIntake(t *testing.T) {
	_, m, userID, repoID, repo := mergepollFixture(t)
	ctx := context.Background()

	tk, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: "MR-1", ExternalID: "uuid-mr-1",
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	// succ 走过一次真实实现（有话），停在 pr_open —— F4.3 场景里的常态
	sess := "sess-succ"
	if _, err := m.Transition(ctx, tk.ID, task.StateImplementing, "test",
		&task.TransitionOpts{AgentSessionID: &sess}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []task.State{task.StateVerifying, task.StatePROpen} {
		if _, err := m.Transition(ctx, tk.ID, s, "test", nil); err != nil {
			t.Fatal(err)
		}
	}
	succ, err := m.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}

	wm, err := NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatalf("建工作区管理器失败: %v", err)
	}
	p := &MergePoller{Tasks: m, Worktrees: wm, RepoLookup: repoLookupFixed(repo)}

	// 第一步照 reenterForVerify 的真实实现走，拿到它内部那个 cur
	cur, err := m.Transition(ctx, succ.ID, task.StateReviewFeedback, "system:rebase-followup",
		&task.TransitionOpts{Payload: map[string]any{"reason": "rebase_followup_reverify"}})
	if err != nil {
		t.Fatalf("第一步失败: %v", err)
	}

	// 回流在这两步之间插进来：review_feedback → queued（带 review 模式）
	if _, err := m.TransitionFrom(ctx, succ.ID, task.StateReviewFeedback, task.StateQueued,
		"system:github-review", &task.TransitionOpts{
			Payload: map[string]any{"reason": "pr_review", "mode": string(RetryReview)},
		}); err != nil {
		t.Fatalf("回流失败: %v", err)
	}

	// 第二步：用那个已经过期的 cur
	if _, err := p.reenterForVerify(ctx, cur); err == nil {
		t.Error("回流已经把任务带去 queued，第二步必须因前置状态不符而拒绝（否则 review 二轮被抢走且无人执行）")
	}

	after, err := m.Get(ctx, succ.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != task.StateQueued {
		t.Errorf("任务应留在 queued 等 worker 领走（review 二轮），实际 state=%s", after.State)
	}

	// 事件流里只有一条进入 implementing 的记录（第一次真实实现），
	// 证明 rebase 跟进没有插入第二次
	events, err := m.Events(ctx, succ.ID)
	if err != nil {
		t.Fatal(err)
	}
	impl := 0
	for _, e := range events {
		if e.ToState == task.StateImplementing {
			impl++
		}
	}
	if impl != 1 {
		t.Errorf("进入 implementing 应只有第一次真实实现那一次，实际 %d 次", impl)
	}
}

// 正常的 rebase 跟进路径不受这次收口影响：任务还在 pr_open 时两步照常走完。
func TestMergePollReenterStillWorksWhenUndisturbed(t *testing.T) {
	_, m, userID, repoID, repo := mergepollFixture(t)
	ctx := context.Background()

	tk, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: "MR-2", ExternalID: "uuid-mr-2",
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	sess := "sess-succ2"
	if _, err := m.Transition(ctx, tk.ID, task.StateImplementing, "test",
		&task.TransitionOpts{AgentSessionID: &sess}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []task.State{task.StateVerifying, task.StatePROpen} {
		if _, err := m.Transition(ctx, tk.ID, s, "test", nil); err != nil {
			t.Fatal(err)
		}
	}
	succ, _ := m.Get(ctx, tk.ID)

	wm, err := NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatalf("建工作区管理器失败: %v", err)
	}
	p := &MergePoller{Tasks: m, Worktrees: wm, RepoLookup: repoLookupFixed(repo)}

	got, err := p.reenterForVerify(ctx, succ)
	if err != nil {
		t.Fatalf("无人争抢时状态归一应成功: %v", err)
	}
	if got.State != task.StateImplementing {
		t.Errorf("pr_open 的任务应被归一到 implementing，实际 %s", got.State)
	}
}
