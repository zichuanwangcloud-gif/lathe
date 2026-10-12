package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/config"
	"github.com/zichuanwangcloud-gif/lathe/internal/runner"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// ---------------------------------------------------------------- 测试夹具

// testStore 连接测试库；连不上就跳过，让不带数据库的环境仍能跑纯逻辑测试。
func testStore(t *testing.T) *store.Store {
	t.Helper()

	dsn := os.Getenv("LATHE_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://lathe:lathe@127.0.0.1:55432/lathe?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Skipf("跳过数据库测试（先 make dev-infra && make migrate）: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// fixture 建一对独立的 user/repo，测试结束时连带清掉其任务（级联）。
func fixture(t *testing.T, st *store.Store) (userID, repoID int64) {
	t.Helper()
	ctx := context.Background()

	email := "queue-test-" + t.Name() + "-" + strconv.FormatInt(time.Now().UnixNano(), 10) + "@example.com"
	if err := st.Pool().QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) RETURNING id`, email).Scan(&userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	if err := st.Pool().QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1, $2) RETURNING id`,
		userID, "acme/queue-test-"+strconv.FormatInt(time.Now().UnixNano(), 10)).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})
	return userID, repoID
}

// uniqueKey 造一个测试内唯一的 issue key/UUID，避免多个测试用例的
// 任务撞上「同一 issue 只能有一条活任务」的唯一索引。
func uniqueKey(prefix string) string {
	return prefix + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

func ptr[T any](v T) *T { return &v }

// ---------------- 全局队列语义下的归属过滤（T9，见 docs/08-debt-cleanup.md §7）

// claimOwn 反复 ClaimReady，直到领到属于 userID 的任务；领不到则返回 nil。
//
// 为什么需要它：ClaimReady 是【全局】查询 —— 单机调度器就该看全局队列，
// 这在生产上是正确语义，绝不能为了测试给它加 owner 过滤。但这让「断言领到
// 自己的 fixture」变得脆弱：开发库里任何一条别人的 queued 行都会按
// (priority DESC, id) 排在前面被优先领走。而别人的行是常态不是异常 ——
// 测试进程被杀（Ctrl-C、CI 超时）时 t.Cleanup 不执行，孤儿行就留下了。
//
// 跳过外来任务的代价很小：ClaimReady 只写 lease_expires_at 与 node_id、
// 不改 state（它的文档注释明确说明了这一点），而且被打上租约的行不会再被
// 重复返回，所以循环一定收敛。顺带的好处是外来行被租约挡住，
// 后续用例也不会再被它们干扰。
func claimOwn(t *testing.T, q *queue, ctx context.Context, userID int64) *task.Task {
	t.Helper()
	// 上界防呆：正常情况下几次就命中，给足余量但不允许真的无界循环。
	const maxSkips = 200
	for i := 0; i < maxSkips; i++ {
		tk, err := q.tasks.ClaimReady(ctx, time.Hour)
		if err != nil {
			t.Fatalf("ClaimReady 失败: %v", err)
		}
		if tk == nil {
			return nil
		}
		if tk.UserID == userID {
			return tk
		}
		t.Logf("跳过不属于本 fixture 的任务 %d（owner=%d，疑似上一轮中断留下的孤儿）", tk.ID, tk.UserID)
	}
	t.Fatalf("连续跳过 %d 条外来任务仍未领到自己的任务，开发库里的孤儿行过多，先跑一次清理", maxSkips)
	return nil
}

// assertNoneOwnClaimable 断言当前【属于 userID 的】任务里没有任何一条可被领取。
//
// 直接写 `if tk, _ := ClaimReady(...); tk != nil { fail }` 是不对的：
// 那条断言会被库里任何一条外来 queued 行推翻，而外来行与被测语义无关。
func assertNoneOwnClaimable(t *testing.T, q *queue, ctx context.Context, userID int64, format string, args ...any) {
	t.Helper()
	const maxDrain = 200
	for i := 0; i < maxDrain; i++ {
		tk, err := q.tasks.ClaimReady(ctx, time.Hour)
		if err != nil {
			t.Fatalf("ClaimReady 失败: %v", err)
		}
		if tk == nil {
			return
		}
		if tk.UserID == userID {
			t.Fatalf(format+"（领到了自己的任务 %d）", append(args, tk.ID)...)
		}
		t.Logf("跳过不属于本 fixture 的任务 %d（owner=%d）", tk.ID, tk.UserID)
	}
	t.Fatalf("排空全局队列时连续跳过 %d 条外来任务，开发库里的孤儿行过多", maxDrain)
}

// fakePipeline 记录每次 Execute 调用，供断言"恰好被跑一次"与
// "ExecuteParams 里的内容对不对"，不需要真正拉起 git/agent/Linear/
// GitHub 的整套环境。
type fakePipeline struct {
	mu    sync.Mutex
	calls []runner.ExecuteParams
	err   error
}

func (f *fakePipeline) Execute(ctx context.Context, params runner.ExecuteParams) error {
	f.mu.Lock()
	f.calls = append(f.calls, params)
	f.mu.Unlock()
	return f.err
}

func (f *fakePipeline) callsFor(taskID int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.calls {
		if p.TaskID == taskID {
			n++
		}
	}
	return n
}

func (f *fakePipeline) snapshot() []runner.ExecuteParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]runner.ExecuteParams, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeWorktrees 是 worktreeInspector 的空实现：本文件的测试任务都没有
// worktree_path（全新任务，或崩溃恢复但从没建过现场），planRetry 里
// 的 Inspect/Discard 调用天然被 path=="" / WorktreePath==nil 的分支
// 挡在前面，不会真正打到这里；给个空实现只是让类型对得上。
type fakeWorktrees struct{}

func (fakeWorktrees) Inspect(ctx context.Context, providerRepo, path, branch, base string) *runner.WorktreeState {
	return nil
}
func (fakeWorktrees) Discard(ctx context.Context, providerRepo, path, branch string) runner.DiscardResult {
	return runner.DiscardResult{}
}

// testQueue 造一个可跑的 queue：真实的 store/task.Machine（连测试库），
// 假的 pipeline（不碰 git/agent/Linear/GitHub）。
func testQueue(st *store.Store, exec pipelineExecutor) *queue {
	return &queue{
		store:     st,
		tasks:     task.NewMachine(st.Pool()),
		pipeline:  exec,
		worktrees: fakeWorktrees{},
		clients:   nil,
		cfg:       config.Config{LightSlots: 2, HeavySlots: 1, NodeName: "test"},
	}
}

// ---------------------------------------------------------------- 1. 创建前移到 Enqueue

// Enqueue 必须让任务行立刻落库、立刻可被领取——不能等 worker 捡到 job
// 才建行，否则 DB 领单调度器在任务真正被处理前永远查不到它。
func TestEnqueueCreatesImmediatelyClaimableTask(t *testing.T) {
	st := testStore(t)
	userID, _ := fixture(t, st)
	ctx := context.Background()
	q := testQueue(st, &fakePipeline{})

	issueKey := uniqueKey("Q-ENQ")
	if err := q.Enqueue(ctx, userID, "uuid-"+issueKey, issueKey); err != nil {
		t.Fatalf("Enqueue 失败: %v", err)
	}

	tk := claimOwn(t, q, ctx, userID)
	if tk == nil || tk.ExternalKey != issueKey {
		t.Fatalf("Enqueue 之后应能立刻领到该任务，得到 %v", tk)
	}
	if tk.State != task.StateQueued {
		t.Errorf("ClaimReady 不该改变 state，得到 %s", tk.State)
	}
}

// Enqueue 在仓库配置解析失败时应把错误往上传（供 webhook.go 记录投递
// 失败原因），而不是像旧实现一样先入队、等 worker 捡到才发现建不了任务。
func TestEnqueueReturnsErrorWhenRepoUnresolved(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	q := testQueue(st, &fakePipeline{})

	// 故意用一个没有任何仓库配置的 user_id
	if err := q.Enqueue(ctx, -1, "uuid-no-repo", uniqueKey("Q-NOREPO")); err == nil {
		t.Fatal("没有仓库配置时 Enqueue 应返回错误")
	}
}

// Requeue 不需要做任何事：调用方（httpapi.retryTask / Reconcile）在调用
// 它之前已经把任务行转回 queued 并把重试依据写进事件流，DB 领单调度器
// 的轮询循环本来就会发现这条 queued 行。
func TestRequeueIsNoOp(t *testing.T) {
	st := testStore(t)
	q := testQueue(st, &fakePipeline{})
	if err := q.Requeue(context.Background(), 999999, "fresh"); err != nil {
		t.Errorf("Requeue 应始终成功返回，得到 %v", err)
	}
}

// ---------------------------------------------------------------- 2. F2.1-AC1~AC4：依赖就绪判定

// AC1：depends_on 为空的独立根，一入队即可被领取。
// AC2：depends_on_at='pr_open' 时，前驱在到达 pr_open 之前
//
//	（queued/triaging/implementing/verifying）绝不能放行后继；到
//	pr_open 后放行。
//
// AC3：depends_on_at='merged' 时，前驱到 pr_open 仍不放行，只有真正
//
//	merged 才放行。
//
// AC4：上面「之前绝不能被领到」覆盖 pr_open 与 merged 两种语义的后继。
func TestClaimReadyDependencyGating(t *testing.T) {
	st := testStore(t)
	userID, repoID := fixture(t, st)
	ctx := context.Background()
	q := testQueue(st, &fakePipeline{})

	pred, err := q.tasks.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: uniqueKey("Q-DEP-PRED"),
		ExternalID: uniqueKey("uuid-dep-pred"),
	})
	if err != nil {
		t.Fatalf("Create 前驱失败: %v", err)
	}
	succPR, err := q.tasks.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: uniqueKey("Q-DEP-SUCC-PR"),
		ExternalID: uniqueKey("uuid-dep-succ-pr"),
		DependsOn:  &pred.ID, DependsOnAt: "pr_open",
	})
	if err != nil {
		t.Fatalf("Create pr_open 语义后继失败: %v", err)
	}
	succMerged, err := q.tasks.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: uniqueKey("Q-DEP-SUCC-MERGED"),
		ExternalID: uniqueKey("uuid-dep-succ-merged"),
		DependsOn:  &pred.ID, DependsOnAt: "merged",
	})
	if err != nil {
		t.Fatalf("Create merged 语义后继失败: %v", err)
	}

	// AC1：独立根（前驱自己）立刻可被领取——先把它领出去，后面单独
	// 通过 Transition 推进它的状态，不再让它参与 ClaimReady 的候选。
	claimedPred := claimOwn(t, q, ctx, userID)
	if claimedPred == nil || claimedPred.ID != pred.ID {
		t.Fatalf("AC1：独立根应立刻可被领取，得到 %v", claimedPred)
	}

	// AC2/AC4：前驱处于 queued（刚被领走，state 未变）/triaging/
	// implementing/verifying 时，两个后继都不该就绪。
	assertNoneOwnClaimable(t, q, ctx, userID,
		"前驱仍处于 queued（只是被打了租约）时，后继不该就绪")
	for _, s := range []task.State{task.StateTriaging, task.StateImplementing, task.StateVerifying} {
		if _, err := q.tasks.Transition(ctx, pred.ID, s, "system", nil); err != nil {
			t.Fatalf("前驱转移到 %s 失败: %v", s, err)
		}
		assertNoneOwnClaimable(t, q, ctx, userID,
			"前驱处于 %s 时任何语义的后继都不该就绪", s)
	}

	// AC2：前驱到 pr_open：pr_open 语义的后继就绪，merged 语义的仍不该
	if _, err := q.tasks.Transition(ctx, pred.ID, task.StatePROpen, "system", nil); err != nil {
		t.Fatalf("前驱转移到 pr_open 失败: %v", err)
	}
	claimed := claimOwn(t, q, ctx, userID)
	if claimed == nil || claimed.ID != succPR.ID {
		t.Fatalf("AC2：前驱 pr_open 后，depends_on_at=pr_open 的后继应就绪，得到 %v", claimed)
	}
	assertNoneOwnClaimable(t, q, ctx, userID,
		"AC3：前驱只 pr_open 未 merged 时，depends_on_at=merged 的后继不该就绪")

	// AC3：前驱真正 merged：merged 语义的后继才就绪
	if _, err := q.tasks.Transition(ctx, pred.ID, task.StateMerged, "system", nil); err != nil {
		t.Fatalf("前驱转移到 merged 失败: %v", err)
	}
	claimed2 := claimOwn(t, q, ctx, userID)
	if claimed2 == nil || claimed2.ID != succMerged.ID {
		t.Fatalf("AC3：前驱 merged 后，depends_on_at=merged 的后继应就绪，得到 %v", claimed2)
	}
}

// ---------------------------------------------------------------- 3. F2.1-AC5：并发压测

// 多个 worker goroutine 同时对同一批就绪任务发起领单 + 处理，任何任务
// 都不该被 Execute 两次。这是本次改造并发正确性的核心断言，必须搭配
// `go test -race` 一起跑。
func TestConcurrentClaimAndDispatchExactlyOnce(t *testing.T) {
	st := testStore(t)
	userID, repoID := fixture(t, st)
	ctx := context.Background()
	pipe := &fakePipeline{}
	q := testQueue(st, pipe)

	const n = 24
	taskIDs := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		tk, err := q.tasks.Create(ctx, task.CreateParams{
			UserID: userID, RepoID: repoID,
			ExternalKey: uniqueKey(fmt.Sprintf("Q-CONC-%d", i)),
			ExternalID:  uniqueKey(fmt.Sprintf("uuid-conc-%d", i)),
		})
		if err != nil {
			t.Fatalf("Create 第 %d 个任务失败: %v", i, err)
		}
		taskIDs = append(taskIDs, tk.ID)
	}

	const workers = 10
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for {
				tk, err := q.tasks.ClaimReady(ctx, time.Hour)
				if err != nil {
					t.Errorf("ClaimReady 失败: %v", err)
					return
				}
				if tk == nil {
					return // 没活干了，正常退出（与 pollLoop 的退避轮询是同一枚硬币的两面：
					// 这里图测试跑得快，不退避直接判定"暂时没有"就收工）
				}
				q.runOneClaimed(ctx, tk)
			}
		}()
	}
	wg.Wait()

	for _, id := range taskIDs {
		if got := pipe.callsFor(id); got != 1 {
			t.Errorf("任务 %d 被 Execute 调用 %d 次，期望恰好 1 次（并发领单/派发不应重复处理）", id, got)
		}
	}
}

// ---------------------------------------------------------------- 4. 崩溃恢复：从事件流回读中断状态

// Reconcile 把 in-flight 任务转回 queued 时，把中断前的状态写进这次
// 转移的事件 payload；runOneClaimed 领到手后应该能把它读回来，喂给
// 智能重试决策——而不是像全新任务一样被判定成"失败阶段未知"。
func TestRunOneClaimedRecoversInterruptedStateFromEvents(t *testing.T) {
	st := testStore(t)
	userID, repoID := fixture(t, st)
	ctx := context.Background()
	pipe := &fakePipeline{}
	q := testQueue(st, pipe)

	tk, err := q.tasks.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: uniqueKey("Q-CRASH"),
		ExternalID: uniqueKey("uuid-crash"),
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	if _, err := q.tasks.Transition(ctx, tk.ID, task.StateTriaging, "system", nil); err != nil {
		t.Fatalf("转 triaging 失败: %v", err)
	}
	if _, err := q.tasks.Transition(ctx, tk.ID, task.StateImplementing, "system", nil); err != nil {
		t.Fatalf("转 implementing 失败: %v", err)
	}

	// 模拟进程重启：任务卡在 implementing，没走过 fail()，没有
	// failure_stage。Reconcile 把它转回 queued，中断状态落进事件 payload。
	if err := q.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile 失败: %v", err)
	}

	claimed := claimOwn(t, q, ctx, userID)
	if claimed == nil || claimed.ID != tk.ID {
		t.Fatalf("应领到恢复后的任务，得到 %v", claimed)
	}
	if claimed.FailureStage != nil {
		t.Fatalf("崩溃恢复的任务不该有 failure_stage，得到 %v", *claimed.FailureStage)
	}

	q.runOneClaimed(ctx, claimed)

	calls := pipe.snapshot()
	if len(calls) != 1 {
		t.Fatalf("Execute 应被调用 1 次，得到 %d", len(calls))
	}
	plan := calls[0].Retry
	if plan == nil {
		t.Fatal("ExecuteParams.Retry 不应为空")
	}
	// 没有 worktree，现场必然不可用，无论走哪条分支都会判 Fresh——
	// 真正要验证的是"决策理由是不是按 implement 阶段算出来的"，
	// 证明 interrupted_state 确实被从事件流回读并喂进了决策，而不是
	// 退化成了"failure_stage 为空、interrupted_state 也没读到"时的
	// 默认兜底分支（决策理由文案不同）。
	if !plan.Fresh {
		t.Fatalf("现场不可用应判定为 Fresh 重建，得到 %+v", plan)
	}
	foundImplementReason := false
	for _, r := range plan.Reasons {
		if strings.Contains(r, "实现阶段") {
			foundImplementReason = true
		}
	}
	if !foundImplementReason {
		t.Errorf("决策理由应体现出中断于实现阶段（interrupted_state=implementing 被正确回读），实际理由 %v", plan.Reasons)
	}
}

// 手动重试（httpapi.retryTask）把 mode 写进转 queued 那次转移的事件
// payload；runOneClaimed 应该把它读回来并按用户指定的模式决策，而不是
// 一律退化成 auto。
func TestRunOneClaimedRecoversModeFromEvents(t *testing.T) {
	st := testStore(t)
	userID, repoID := fixture(t, st)
	ctx := context.Background()
	pipe := &fakePipeline{}
	q := testQueue(st, pipe)

	tk, err := q.tasks.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: uniqueKey("Q-MODE"),
		ExternalID: uniqueKey("uuid-mode"),
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	if _, err := q.tasks.Transition(ctx, tk.ID, task.StateTriaging, "system", nil); err != nil {
		t.Fatalf("转 triaging 失败: %v", err)
	}
	if _, err := q.tasks.Transition(ctx, tk.ID, task.StateImplementing, "system", &task.TransitionOpts{
		WorktreePath: ptr("/tmp/does-not-exist-" + uniqueKey("wt")),
		BranchName:   ptr("fix/does-not-exist"),
	}); err != nil {
		t.Fatalf("转 implementing 失败: %v", err)
	}
	if _, err := q.tasks.Transition(ctx, tk.ID, task.StateFailed, "system", &task.TransitionOpts{
		FailureStage: ptr(string(runner.StageImplementRun)),
	}); err != nil {
		t.Fatalf("转 failed 失败: %v", err)
	}

	// httpapi.retryTask 的行为：转 queued 时把 mode 写进 payload，再调 Requeue
	if _, err := q.tasks.Transition(ctx, tk.ID, task.StateQueued, "user:1", &task.TransitionOpts{
		Payload: map[string]any{"reason": "manual_retry", "mode": "fresh"},
	}); err != nil {
		t.Fatalf("手动重试转 queued 失败: %v", err)
	}
	if err := q.Requeue(ctx, tk.ID, "fresh"); err != nil {
		t.Fatalf("Requeue 失败: %v", err)
	}

	claimed := claimOwn(t, q, ctx, userID)
	if claimed == nil || claimed.ID != tk.ID {
		t.Fatalf("应领到任务，得到 %v", claimed)
	}

	q.runOneClaimed(ctx, claimed)

	calls := pipe.snapshot()
	if len(calls) != 1 {
		t.Fatalf("Execute 应被调用 1 次，得到 %d", len(calls))
	}
	plan := calls[0].Retry
	if plan == nil || !plan.Fresh {
		t.Fatalf("mode=fresh 应强制从头重建，得到 %+v", plan)
	}
	found := false
	for _, r := range plan.Reasons {
		if strings.Contains(r, "指定了从头重跑") {
			found = true
		}
	}
	if !found {
		t.Errorf("应识别出 mode=fresh 是从事件流回读来的，决策理由却是 %v", plan.Reasons)
	}
}

// ---------------------------------------------------------------- 5. F3.1：base_ref 派发时填充

// 后继任务第一次被派发时，若 base_ref 为空，应从前驱当前的分支名落库，
// 并原样传给 pipeline.Execute 的 ExecuteParams.Repo.BaseRefOverride。
func TestFillBaseRefFirstDispatchUsesPredecessorBranch(t *testing.T) {
	st := testStore(t)
	userID, repoID := fixture(t, st)
	ctx := context.Background()
	pipe := &fakePipeline{}
	q := testQueue(st, pipe)

	branch := "fix/" + uniqueKey("q-base-pred")
	pred, err := q.tasks.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: uniqueKey("Q-BASE-PRED"),
		ExternalID: uniqueKey("uuid-base-pred"),
	})
	if err != nil {
		t.Fatalf("Create 前驱失败: %v", err)
	}
	if _, err := q.tasks.Transition(ctx, pred.ID, task.StateTriaging, "system", nil); err != nil {
		t.Fatalf("前驱转 triaging 失败: %v", err)
	}
	if _, err := q.tasks.Transition(ctx, pred.ID, task.StateImplementing, "system", &task.TransitionOpts{
		BranchName: ptr(branch),
	}); err != nil {
		t.Fatalf("前驱转 implementing 失败: %v", err)
	}
	if _, err := q.tasks.Transition(ctx, pred.ID, task.StateVerifying, "system", nil); err != nil {
		t.Fatalf("前驱转 verifying 失败: %v", err)
	}
	if _, err := q.tasks.Transition(ctx, pred.ID, task.StatePROpen, "system", &task.TransitionOpts{
		PRURL: ptr("https://github.com/x/y/pull/1"),
	}); err != nil {
		t.Fatalf("前驱转 pr_open 失败: %v", err)
	}

	succ, err := q.tasks.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: uniqueKey("Q-BASE-SUCC"),
		ExternalID: uniqueKey("uuid-base-succ"),
		DependsOn:  &pred.ID, DependsOnAt: "pr_open",
	})
	if err != nil {
		t.Fatalf("Create 后继失败: %v", err)
	}
	if succ.BaseRef != nil {
		t.Fatalf("后继初始 base_ref 应为 NULL，得到 %v", *succ.BaseRef)
	}

	claimed := claimOwn(t, q, ctx, userID)
	if claimed == nil || claimed.ID != succ.ID {
		t.Fatalf("应领到后继任务，得到 %v", claimed)
	}

	q.runOneClaimed(ctx, claimed)

	got, err := q.tasks.Get(ctx, succ.ID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if got.BaseRef == nil || *got.BaseRef != branch {
		t.Fatalf("base_ref 应被落库为前驱分支 %q，得到 %v", branch, got.BaseRef)
	}

	calls := pipe.snapshot()
	if len(calls) != 1 {
		t.Fatalf("Execute 应被调用 1 次，得到 %d", len(calls))
	}
	if calls[0].Repo.BaseRefOverride != branch {
		t.Errorf("ExecuteParams.Repo.BaseRefOverride = %q，期望 %q", calls[0].Repo.BaseRefOverride, branch)
	}
}

// 已经写过 base_ref 的后继任务重试时，即便前驱当前的分支名已经变了
// （前驱被丢弃重建过），也不该被覆盖——后继当初分叉的那个分支不该
// 跟着变（docs/06-orchestration.md §2.2）。
func TestFillBaseRefNotOverwrittenOnRetry(t *testing.T) {
	st := testStore(t)
	userID, repoID := fixture(t, st)
	ctx := context.Background()
	q := testQueue(st, &fakePipeline{})

	pred, err := q.tasks.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: uniqueKey("Q-BASE-RETRY-PRED"),
		ExternalID: uniqueKey("uuid-base-retry-pred"),
	})
	if err != nil {
		t.Fatalf("Create 前驱失败: %v", err)
	}

	originalBranch := "fix/" + uniqueKey("original")
	succ, err := q.tasks.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: uniqueKey("Q-BASE-RETRY-SUCC"),
		ExternalID: uniqueKey("uuid-base-retry-succ"),
		DependsOn:  &pred.ID, DependsOnAt: "pr_open",
		BaseRef: ptr(originalBranch), // 已经派发过一次，base_ref 已固定
	})
	if err != nil {
		t.Fatalf("Create 后继失败: %v", err)
	}

	// 模拟前驱被丢弃重建：分支名变了
	rebuiltBranch := "fix/" + uniqueKey("rebuilt")
	if _, err := st.Pool().Exec(ctx, `UPDATE tasks SET branch_name = $2 WHERE id = $1`, pred.ID, rebuiltBranch); err != nil {
		t.Fatalf("模拟前驱重建失败: %v", err)
	}

	repoCfg := runner.RepoConfig{}
	q.fillBaseRef(ctx, succ, &repoCfg)

	if succ.BaseRef == nil || *succ.BaseRef != originalBranch {
		t.Errorf("重试场景下 base_ref 不该被覆盖，得到 %v，期望仍是 %q", succ.BaseRef, originalBranch)
	}
	if repoCfg.BaseRefOverride != originalBranch {
		t.Errorf("BaseRefOverride = %q，期望仍是原分支 %q（不随前驱重建而变）", repoCfg.BaseRefOverride, originalBranch)
	}

	got, err := q.tasks.Get(ctx, succ.ID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if got.BaseRef == nil || *got.BaseRef != originalBranch {
		t.Errorf("落库的 base_ref 也不该被覆盖，得到 %v", got.BaseRef)
	}
}

// 前驱已经合并时，fillBaseRef 不该把它的分支名当 override：那个分支
// 很快会被 MergePoller 回收，即使没回收，前驱的改动也已经在
// default_branch 里了，后继直接从 default_branch 分叉更简单也更对。
func TestFillBaseRefSkipsWhenPredecessorMerged(t *testing.T) {
	st := testStore(t)
	userID, repoID := fixture(t, st)
	ctx := context.Background()
	q := testQueue(st, &fakePipeline{})

	branch := "fix/" + uniqueKey("q-base-merged-pred")
	pred, err := q.tasks.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: uniqueKey("Q-BASE-MERGED-PRED"),
		ExternalID: uniqueKey("uuid-base-merged-pred"),
	})
	if err != nil {
		t.Fatalf("Create 前驱失败: %v", err)
	}
	if _, err := q.tasks.Transition(ctx, pred.ID, task.StateImplementing, "system", &task.TransitionOpts{
		BranchName: ptr(branch),
	}); err != nil {
		t.Fatalf("前驱转 implementing 失败: %v", err)
	}
	if _, err := q.tasks.Transition(ctx, pred.ID, task.StateVerifying, "system", nil); err != nil {
		t.Fatalf("前驱转 verifying 失败: %v", err)
	}
	if _, err := q.tasks.Transition(ctx, pred.ID, task.StatePROpen, "system", &task.TransitionOpts{
		PRURL: ptr("https://github.com/x/y/pull/2"),
	}); err != nil {
		t.Fatalf("前驱转 pr_open 失败: %v", err)
	}
	if _, err := q.tasks.Transition(ctx, pred.ID, task.StateMerged, "system", nil); err != nil {
		t.Fatalf("前驱转 merged 失败: %v", err)
	}

	succ, err := q.tasks.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: uniqueKey("Q-BASE-MERGED-SUCC"),
		ExternalID: uniqueKey("uuid-base-merged-succ"),
		DependsOn:  &pred.ID, DependsOnAt: "merged",
	})
	if err != nil {
		t.Fatalf("Create 后继失败: %v", err)
	}

	repoCfg := runner.RepoConfig{}
	q.fillBaseRef(ctx, succ, &repoCfg)

	if repoCfg.BaseRefOverride != "" {
		t.Errorf("前驱已合并时不该设置 BaseRefOverride，得到 %q", repoCfg.BaseRefOverride)
	}
	if succ.BaseRef != nil {
		t.Errorf("前驱已合并时 succ.BaseRef（内存副本）应保持 nil，得到 %v", *succ.BaseRef)
	}

	got, err := q.tasks.Get(ctx, succ.ID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if got.BaseRef != nil {
		t.Errorf("前驱已合并时落库的 base_ref 应保持 NULL，得到 %v，后继应走 default_branch", *got.BaseRef)
	}
}

// 独立根（depends_on 为空）不应触碰 BaseRefOverride，行为与改造前一致。
func TestFillBaseRefNoopForIndependentRoot(t *testing.T) {
	st := testStore(t)
	userID, repoID := fixture(t, st)
	ctx := context.Background()
	q := testQueue(st, &fakePipeline{})

	tk, err := q.tasks.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: uniqueKey("Q-BASE-ROOT"),
		ExternalID: uniqueKey("uuid-base-root"),
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	repoCfg := runner.RepoConfig{}
	q.fillBaseRef(ctx, tk, &repoCfg)

	if repoCfg.BaseRefOverride != "" {
		t.Errorf("独立根不该设置 BaseRefOverride，得到 %q", repoCfg.BaseRefOverride)
	}
	got, err := q.tasks.Get(ctx, tk.ID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if got.BaseRef != nil {
		t.Errorf("独立根的 base_ref 应保持 NULL，得到 %v", *got.BaseRef)
	}
}

// ---------------------------------------------------------------- 6. 需求平台引用俱空的残缺数据

// external_id 与 external_key 俱空的任务无法定位工单，runOneClaimed
// 应该把它取消而不是尝试派发。
//
// 注：旧契约是「缺 Linear UUID 即取消」。0021 之后守卫放宽为「引用俱空
// 才取消」—— Linear 的 issue 查询同时接受 UUID 与 identifier，只有 key
// 的旧数据可以继续跑；内置工单的 external_id 恒为 NULL，key 即全部身份。
func TestRunOneClaimedCancelsTaskWithoutExternalID(t *testing.T) {
	st := testStore(t)
	userID, repoID := fixture(t, st)
	ctx := context.Background()
	pipe := &fakePipeline{}
	q := testQueue(st, pipe)

	tk, err := q.tasks.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: "", // 引用俱空
		// ExternalID 留空，模拟残缺数据
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	if tk.ExternalID != nil {
		t.Fatalf("测试前提：ExternalID 应为 NULL，得到 %v", *tk.ExternalID)
	}

	q.runOneClaimed(ctx, tk)

	got, err := q.tasks.Get(ctx, tk.ID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if got.State != task.StateCancelled {
		t.Errorf("引用俱空的任务应被取消，state = %s", got.State)
	}
	if len(pipe.snapshot()) != 0 {
		t.Errorf("不该走到 pipeline.Execute，却被调用了 %d 次", len(pipe.snapshot()))
	}
}

// ---------------------------------------------------------------- T9 测试隔离回归

// orphanInflight 造一条【别的用户的】在途任务，模拟「上一轮测试被中断留下的孤儿」。
//
// 它刻意在调用方建自己的任务【之前】被创建，因此 id 更小 —— 而 ClaimReady 的
// 排序是 (priority DESC, id)，所以孤儿必然被优先领走。这正是实测失败的成因。
func orphanInflight(t *testing.T, st *store.Store, q *queue) int64 {
	t.Helper()
	ctx := context.Background()
	userID, repoID := fixture(t, st)
	tk, err := q.tasks.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID,
		ExternalKey: uniqueKey("ORPHAN"), ExternalID: uniqueKey("uuid-orphan"),
	})
	if err != nil {
		t.Fatalf("造孤儿任务失败: %v", err)
	}
	if _, err := q.tasks.Transition(ctx, tk.ID, task.StateTriaging, "system", nil); err != nil {
		t.Fatalf("孤儿转 triaging 失败: %v", err)
	}
	if _, err := q.tasks.Transition(ctx, tk.ID, task.StateImplementing, "system", nil); err != nil {
		t.Fatalf("孤儿转 implementing 失败: %v", err)
	}
	return tk.ID
}

// 崩溃恢复的断言必须能在「库里还有别人的在途任务」时成立。
//
// 背景：q.Reconcile 与 tasks.ClaimReady 都是【全局】操作 —— 单机调度器就该看
// 全局队列，这在生产上是正确语义，不能为了测试给它们加 owner 过滤。但这让
// 「断言领到自己的任务」变得脆弱：Reconcile 会把库里所有在途任务一起重新入队
// （实测日志 `启动恢复完成 requeued_inflight=2` 就是铁证），随后 ClaimReady
// 按 id 升序把别人的那条优先领走，测试拿到的不是自己的 fixture。
//
// 开发库里出现别人的在途行是常态，不是异常：测试进程被杀时 t.Cleanup 不执行，
// 孤儿行就留下了。所以隔离责任在测试侧。
func TestClaimOwnIgnoresForeignInflightTasks(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	pipe := &fakePipeline{}
	q := testQueue(st, pipe)

	// 先造孤儿（id 更小，会被 ClaimReady 优先领走）
	orphanID := orphanInflight(t, st, q)

	// 再造自己的任务
	userID, repoID := fixture(t, st)
	tk, err := q.tasks.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID,
		ExternalKey: uniqueKey("Q-ISO"), ExternalID: uniqueKey("uuid-iso"),
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	if _, err := q.tasks.Transition(ctx, tk.ID, task.StateTriaging, "system", nil); err != nil {
		t.Fatalf("转 triaging 失败: %v", err)
	}
	if _, err := q.tasks.Transition(ctx, tk.ID, task.StateImplementing, "system", nil); err != nil {
		t.Fatalf("转 implementing 失败: %v", err)
	}

	if err := q.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile 失败: %v", err)
	}

	claimed := claimOwn(t, q, ctx, userID)
	if claimed == nil {
		t.Fatal("应领到属于本 fixture 的任务")
	}
	if claimed.ID == orphanID {
		t.Fatalf("领到了孤儿任务 %d，说明没有按归属过滤", orphanID)
	}
	if claimed.ID != tk.ID {
		t.Fatalf("应领到 %d，得到 %d", tk.ID, claimed.ID)
	}
}

// ---------------------------------------------------------------- T2 闸门配置复制

// Enqueue 必须把 repos.gate_mode 复制进任务行。
//
// 这是 gate_mode「配了没接线」的真正断点：列早就存在、pipeline 也能读
// tasks.gate_mode，但建任务时从来没人把仓库配置复制过去，所以该列永远是
// 'direct'，人在仓库配置页选了 manual 也毫无效果
// （docs/08-debt-cleanup.md T2 AC6）。
//
// 复制而不是每次现查 repos，是为了把任务创建那一刻的配置【钉死】——
// 与 repo_id 的语义保持一致：任务在途期间人改了仓库配置，
// 不该改变这个任务的行为。
func TestEnqueueCopiesRepoGateMode(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	q := testQueue(st, &fakePipeline{})

	userID, repoID := fixture(t, st)
	if _, err := st.Pool().Exec(ctx,
		`UPDATE repos SET gate_mode='manual' WHERE id=$1`, repoID); err != nil {
		t.Fatalf("设置 repos.gate_mode 失败: %v", err)
	}

	issueKey := uniqueKey("Q-GATE")
	if err := q.Enqueue(ctx, userID, uniqueKey("uuid-gate"), issueKey); err != nil {
		t.Fatalf("Enqueue 失败: %v", err)
	}

	tk := claimOwn(t, q, ctx, userID)
	if tk == nil {
		t.Fatal("应能领到刚建的任务")
	}
	if tk.GateMode != task.GateManual {
		t.Fatalf("AC6：任务的 gate_mode 应从仓库复制为 manual，得到 %q", tk.GateMode)
	}

	// 钉死语义：事后改仓库配置不影响已建的任务
	if _, err := st.Pool().Exec(ctx,
		`UPDATE repos SET gate_mode='direct' WHERE id=$1`, repoID); err != nil {
		t.Fatalf("改回 repos.gate_mode 失败: %v", err)
	}
	again, err := q.tasks.Get(ctx, tk.ID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if again.GateMode != task.GateManual {
		t.Errorf("AC6：仓库配置事后改动不该影响在途任务，任务 gate_mode 变成了 %q", again.GateMode)
	}
}
