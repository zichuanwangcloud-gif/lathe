package task

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

// 新列的默认值与显式赋值都应被正确读写。
func TestMachineCreateWithOrchestrationFields(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	root, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-ORCH-ROOT"})
	if err != nil {
		t.Fatalf("Create 独立根失败: %v", err)
	}
	if root.FlowID != nil {
		t.Errorf("默认 flow_id 应为 NULL，得到 %v", *root.FlowID)
	}
	if root.DependsOn != nil {
		t.Errorf("默认 depends_on 应为 NULL，得到 %v", *root.DependsOn)
	}
	if root.DependsOnAt != "pr_open" {
		t.Errorf("空 DependsOnAt 应落成 schema 默认值 pr_open，得到 %q", root.DependsOnAt)
	}
	if root.Priority != 0 {
		t.Errorf("默认 priority 应为 0，得到 %d", root.Priority)
	}
	if string(root.Profile) != "{}" {
		t.Errorf("默认 profile 应为空对象，得到 %s", root.Profile)
	}
	if root.BaseRef != nil {
		t.Errorf("默认 base_ref 应为 NULL，得到 %v", *root.BaseRef)
	}
	if root.PRNumber != nil {
		t.Errorf("默认 pr_number 应为 NULL，得到 %v", *root.PRNumber)
	}

	child, err := m.Create(ctx, CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "CR-ORCH-CHILD",
		DependsOn: &root.ID, DependsOnAt: "merged", Priority: 5, BaseRef: ptr("fix/cr-orch-root-base"),
	})
	if err != nil {
		t.Fatalf("Create 后继失败: %v", err)
	}
	if child.DependsOn == nil || *child.DependsOn != root.ID {
		t.Errorf("depends_on = %v，期望 %d", child.DependsOn, root.ID)
	}
	if child.DependsOnAt != "merged" {
		t.Errorf("depends_on_at = %q，期望 merged", child.DependsOnAt)
	}
	if child.Priority != 5 {
		t.Errorf("priority = %d，期望 5", child.Priority)
	}
	if child.BaseRef == nil || *child.BaseRef != "fix/cr-orch-root-base" {
		t.Errorf("base_ref = %v，期望 fix/cr-orch-root-base", child.BaseRef)
	}
}

// SetBaseRef 不经过状态机：只改列，不动 state，找不到任务报 ErrTaskNotFound。
func TestMachineSetBaseRef(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	tk, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-SETBASE-1"})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	if err := m.SetBaseRef(ctx, tk.ID, ptr("fix/cr-1000-stack")); err != nil {
		t.Fatalf("SetBaseRef 失败: %v", err)
	}
	got, err := m.Get(ctx, tk.ID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if got.BaseRef == nil || *got.BaseRef != "fix/cr-1000-stack" {
		t.Errorf("base_ref = %v，期望 fix/cr-1000-stack", got.BaseRef)
	}
	if got.State != StateQueued {
		t.Errorf("SetBaseRef 不应改变 state，得到 %s", got.State)
	}

	// 清空
	if err := m.SetBaseRef(ctx, tk.ID, nil); err != nil {
		t.Fatalf("清空 SetBaseRef 失败: %v", err)
	}
	got, _ = m.Get(ctx, tk.ID)
	if got.BaseRef != nil {
		t.Errorf("清空后 base_ref 应为 NULL，得到 %v", *got.BaseRef)
	}

	if err := m.SetBaseRef(ctx, 999999999, ptr("x")); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("SetBaseRef 不存在的任务应返回 ErrTaskNotFound，得到 %v", err)
	}
}

// 并发正确性核心断言：N 个就绪任务被 G 个并发调用者领取，每个任务恰好被
// 一个调用者领到，不多不少。
func TestClaimReadyConcurrency(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	// 本用例断言「领到的就是自己创建的那条」，所以先把已有候选排空 ——
	// 否则开发库里任何一条外来 queued 行都会被优先领走。见 machine_test.go
	// 的 drainForeignQueue 注释。
	drainForeignQueue(t, m, ctx)

	const n = 12
	ids := make(map[int64]bool, n)
	for i := 0; i < n; i++ {
		tk, err := m.Create(ctx, CreateParams{
			UserID: userID, RepoID: repoID,
			LinearIssueKey: "CR-CLAIM-" + time.Now().Format("150405.000000") + "-" + strconv.Itoa(i),
		})
		if err != nil {
			t.Fatalf("Create 第 %d 个任务失败: %v", i, err)
		}
		ids[tk.ID] = true
	}

	const workers = 6
	var (
		mu      sync.Mutex
		claimed = map[int64]int{} // taskID -> 被领取次数，用来断言"恰好一次"
		wg      sync.WaitGroup
	)
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for {
				tk, err := m.ClaimReady(ctx, time.Hour) // 长租约：本测试只关心归属，不测过期
				if err != nil {
					t.Errorf("ClaimReady 失败: %v", err)
					return
				}
				if tk == nil {
					return // 没活干了，正常退出
				}
				mu.Lock()
				if ids[tk.ID] {
					claimed[tk.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(claimed) != n {
		t.Fatalf("本 fixture 的任务里被领取的有 %d 个，期望全部 %d 个都被领到", len(claimed), n)
	}
	for id, count := range claimed {
		if count != 1 {
			t.Errorf("任务 %d 被领取 %d 次，期望恰好 1 次（并发领单不应重复）", id, count)
		}
		got, err := m.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%d) 失败: %v", id, err)
		}
		if got.State != StateQueued {
			t.Errorf("ClaimReady 不应改变 state，任务 %d 的 state = %s", id, got.State)
		}
		if got.LeaseExpiresAt == nil {
			t.Errorf("任务 %d 应已打上租约", id)
		}
	}
}

// 带租约、未过期的任务不能被重复领取；租约过期后应能被重新领取。
func TestClaimReadyLeaseExpiry(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	// 本用例断言「领到的就是自己创建的那条」，所以先把已有候选排空 ——
	// 否则开发库里任何一条外来 queued 行都会被优先领走。见 machine_test.go
	// 的 drainForeignQueue 注释。
	drainForeignQueue(t, m, ctx)

	tk, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-LEASE-1"})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	got, err := m.ClaimReady(ctx, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("首次 ClaimReady 失败: %v", err)
	}
	if got == nil || got.ID != tk.ID {
		t.Fatalf("首次 ClaimReady 应领到任务 %d，得到 %v", tk.ID, got)
	}
	if got.State != StateQueued {
		t.Errorf("ClaimReady 不应改变 state，得到 %s", got.State)
	}

	again, err := m.ClaimReady(ctx, time.Hour)
	if err != nil {
		t.Fatalf("第二次 ClaimReady 失败: %v", err)
	}
	if again != nil && again.ID == tk.ID {
		t.Errorf("租约未过期时任务 %d 不应被重复领取", tk.ID)
	}

	time.Sleep(150 * time.Millisecond) // 让 100ms 的租约过期

	recl, err := m.ClaimReady(ctx, time.Hour)
	if err != nil {
		t.Fatalf("租约过期后 ClaimReady 失败: %v", err)
	}
	if recl == nil || recl.ID != tk.ID {
		t.Fatalf("租约过期后应能重新领取任务 %d，得到 %v", tk.ID, recl)
	}
}

// 前驱放行判定：depends_on_at=pr_open 时前驱到 pr_open 即放行，
// depends_on_at=merged 时必须前驱真 merged 才放行。
func TestClaimReadyRespectsDependsOnAt(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	// 本用例断言「领到的就是自己创建的那条」，所以先把已有候选排空 ——
	// 否则开发库里任何一条外来 queued 行都会被优先领走。见 machine_test.go
	// 的 drainForeignQueue 注释。
	drainForeignQueue(t, m, ctx)

	pred, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-DEP-PRED"})
	if err != nil {
		t.Fatalf("Create 前驱失败: %v", err)
	}
	for _, s := range []State{StateTriaging, StateImplementing, StateVerifying, StatePROpen} {
		if _, err := m.Transition(ctx, pred.ID, s, "system", nil); err != nil {
			t.Fatalf("前驱转移到 %s 失败: %v", s, err)
		}
	}

	succMerged, err := m.Create(ctx, CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "CR-DEP-SUCC-MERGED",
		DependsOn: &pred.ID, DependsOnAt: "merged",
	})
	if err != nil {
		t.Fatalf("Create merged 语义后继失败: %v", err)
	}
	succOpen, err := m.Create(ctx, CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "CR-DEP-SUCC-OPEN",
		DependsOn: &pred.ID, DependsOnAt: "pr_open",
	})
	if err != nil {
		t.Fatalf("Create pr_open 语义后继失败: %v", err)
	}

	// 前驱只到 pr_open：pr_open 语义的后继就绪，merged 语义的后继不就绪
	claimed := map[int64]bool{}
	for i := 0; i < 10; i++ {
		tk, err := m.ClaimReady(ctx, time.Hour)
		if err != nil {
			t.Fatalf("ClaimReady 失败: %v", err)
		}
		if tk == nil {
			break
		}
		claimed[tk.ID] = true
	}
	if !claimed[succOpen.ID] {
		t.Errorf("前驱 pr_open 时，depends_on_at=pr_open 的后继应可被领取")
	}
	if claimed[succMerged.ID] {
		t.Errorf("前驱只 pr_open 未 merged 时，depends_on_at=merged 的后继不该被领取")
	}

	// 前驱真正 merged 后，merged 语义的后继才应就绪
	if _, err := m.Transition(ctx, pred.ID, StateMerged, "system", nil); err != nil {
		t.Fatalf("前驱转移到 merged 失败: %v", err)
	}
	tk, err := m.ClaimReady(ctx, time.Hour)
	if err != nil {
		t.Fatalf("ClaimReady 失败: %v", err)
	}
	if tk == nil || tk.ID != succMerged.ID {
		t.Errorf("前驱 merged 后，depends_on_at=merged 的后继应可被领取，得到 %v", tk)
	}
}

// F2.3：1→2→3、2→4（4 依赖 2）两条链，1 失败后 2/3/4（含间接后继 3）
// 全部转 blocked_dep 且事件记录 blocked_by；独立根 5 不受影响。
func TestPropagateBlocked(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	t1, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-PROP-1"})
	if err != nil {
		t.Fatalf("Create t1 失败: %v", err)
	}
	t2, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-PROP-2", DependsOn: &t1.ID})
	if err != nil {
		t.Fatalf("Create t2 失败: %v", err)
	}
	t3, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-PROP-3", DependsOn: &t2.ID})
	if err != nil {
		t.Fatalf("Create t3 失败: %v", err)
	}
	t4, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-PROP-4", DependsOn: &t2.ID})
	if err != nil {
		t.Fatalf("Create t4 失败: %v", err)
	}
	t5, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-PROP-5"})
	if err != nil {
		t.Fatalf("Create t5 失败: %v", err)
	}

	// t1 走到终结状态 failed（调用方保证的前置条件）
	if _, err := m.Transition(ctx, t1.ID, StateTriaging, "system", nil); err != nil {
		t.Fatalf("t1 转 triaging 失败: %v", err)
	}
	if _, err := m.Transition(ctx, t1.ID, StateFailed, "system", nil); err != nil {
		t.Fatalf("t1 转 failed 失败: %v", err)
	}

	blocked, err := m.PropagateBlocked(ctx, t1.ID, "前驱失败：CR-PROP-1")
	if err != nil {
		t.Fatalf("PropagateBlocked 失败: %v", err)
	}
	if len(blocked) != 3 {
		t.Fatalf("应有 3 个后继被阻塞（2/3/4），得到 %d 个", len(blocked))
	}
	gotIDs := map[int64]bool{}
	for _, tk := range blocked {
		gotIDs[tk.ID] = true
		if tk.State != StateBlockedDep {
			t.Errorf("返回的任务 %d state = %s，期望 blocked_dep", tk.ID, tk.State)
		}
	}
	for _, want := range []int64{t2.ID, t3.ID, t4.ID} {
		if !gotIDs[want] {
			t.Errorf("任务 %d 应在被阻塞列表中", want)
		}
	}

	for _, want := range []struct {
		id   int64
		name string
	}{{t2.ID, "t2"}, {t3.ID, "t3"}, {t4.ID, "t4"}} {
		got, err := m.Get(ctx, want.id)
		if err != nil {
			t.Fatalf("Get(%s) 失败: %v", want.name, err)
		}
		if got.State != StateBlockedDep {
			t.Errorf("%s 的 state = %s，期望 blocked_dep", want.name, got.State)
		}

		events, err := m.Events(ctx, want.id)
		if err != nil {
			t.Fatalf("Events(%s) 失败: %v", want.name, err)
		}
		last := events[len(events)-1]
		if last.ToState != StateBlockedDep {
			t.Errorf("%s 最后一个事件 to_state = %s，期望 blocked_dep", want.name, last.ToState)
		}
		blockedBy, ok := last.Payload["blocked_by"]
		if !ok {
			t.Fatalf("%s 的事件 payload 应含 blocked_by，得到 %v", want.name, last.Payload)
		}
		if n, ok := blockedBy.(float64); !ok || int64(n) != t1.ID {
			t.Errorf("%s 的 blocked_by = %v，期望 %d", want.name, blockedBy, t1.ID)
		}
	}

	// 独立根 t5 不受影响
	got5, err := m.Get(ctx, t5.ID)
	if err != nil {
		t.Fatalf("Get(t5) 失败: %v", err)
	}
	if got5.State != StateQueued {
		t.Errorf("独立根 t5 不该被失败传播影响，state = %s", got5.State)
	}
}

// F2.3-AC5：只唤醒直接 blocked_dep 后继，非 blocked_dep 的兄弟与间接
// 后继（孙节点）都不受影响。
func TestWakeBlockedSuccessors(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	root, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-WAKE-ROOT"})
	if err != nil {
		t.Fatalf("Create root 失败: %v", err)
	}
	c1, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-WAKE-C1", DependsOn: &root.ID})
	if err != nil {
		t.Fatalf("Create c1 失败: %v", err)
	}
	c2, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-WAKE-C2", DependsOn: &root.ID})
	if err != nil {
		t.Fatalf("Create c2 失败: %v", err)
	}
	grandchild, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-WAKE-G", DependsOn: &c1.ID})
	if err != nil {
		t.Fatalf("Create 孙节点失败: %v", err)
	}

	// c1、grandchild 落入 blocked_dep；c2 走另一条路（cancelled），
	// 用来验证"非 blocked_dep 状态的子不受影响"。
	if _, err := m.Transition(ctx, c1.ID, StateBlockedDep, "system", nil); err != nil {
		t.Fatalf("c1 转 blocked_dep 失败: %v", err)
	}
	if _, err := m.Transition(ctx, grandchild.ID, StateBlockedDep, "system", nil); err != nil {
		t.Fatalf("grandchild 转 blocked_dep 失败: %v", err)
	}
	if _, err := m.Transition(ctx, c2.ID, StateCancelled, "system", nil); err != nil {
		t.Fatalf("c2 转 cancelled 失败: %v", err)
	}

	// root 到达"后继可以恢复"的状态（调用方保证；具体状态由调用方决定，
	// 本方法不关心 root 自身的 state）
	if _, err := m.Transition(ctx, root.ID, StateTriaging, "system", nil); err != nil {
		t.Fatalf("root 转 triaging 失败: %v", err)
	}
	if _, err := m.Transition(ctx, root.ID, StateImplementing, "system", nil); err != nil {
		t.Fatalf("root 转 implementing 失败: %v", err)
	}
	if _, err := m.Transition(ctx, root.ID, StateVerifying, "system", nil); err != nil {
		t.Fatalf("root 转 verifying 失败: %v", err)
	}
	if _, err := m.Transition(ctx, root.ID, StatePROpen, "system", nil); err != nil {
		t.Fatalf("root 转 pr_open 失败: %v", err)
	}

	woken, err := m.WakeBlockedSuccessors(ctx, root.ID)
	if err != nil {
		t.Fatalf("WakeBlockedSuccessors 失败: %v", err)
	}
	if len(woken) != 1 || woken[0].ID != c1.ID {
		t.Fatalf("应恰好唤醒直接后继 c1（%d），得到 %+v", c1.ID, woken)
	}
	if woken[0].State != StateQueued {
		t.Errorf("被唤醒的任务 state = %s，期望 queued", woken[0].State)
	}

	gotC1, err := m.Get(ctx, c1.ID)
	if err != nil {
		t.Fatalf("Get(c1) 失败: %v", err)
	}
	if gotC1.State != StateQueued {
		t.Errorf("c1 的 state = %s，期望 queued（已被唤醒）", gotC1.State)
	}

	events, err := m.Events(ctx, c1.ID)
	if err != nil {
		t.Fatalf("Events(c1) 失败: %v", err)
	}
	last := events[len(events)-1]
	if unblockedBy, ok := last.Payload["unblocked_by"]; !ok {
		t.Errorf("c1 的唤醒事件 payload 应含 unblocked_by，得到 %v", last.Payload)
	} else if n, ok := unblockedBy.(float64); !ok || int64(n) != root.ID {
		t.Errorf("c1 的 unblocked_by = %v，期望 %d", unblockedBy, root.ID)
	}

	gotC2, err := m.Get(ctx, c2.ID)
	if err != nil {
		t.Fatalf("Get(c2) 失败: %v", err)
	}
	if gotC2.State != StateCancelled {
		t.Errorf("非 blocked_dep 的兄弟 c2 不该受影响，state = %s，期望仍是 cancelled", gotC2.State)
	}

	gotGrandchild, err := m.Get(ctx, grandchild.ID)
	if err != nil {
		t.Fatalf("Get(grandchild) 失败: %v", err)
	}
	if gotGrandchild.State != StateBlockedDep {
		t.Errorf("间接后继（孙节点）不该被这次调用唤醒，state = %s，期望仍是 blocked_dep", gotGrandchild.State)
	}
}

// ---------------------------------------------------------------- T6 可回收查询

// ★ T6-AC2：非终态任务的现场绝不进回收候选。
//
// failed 可以转回 queued —— 人随时可能重试续跑，而 D4 保留现场的
// 全部目的就是让人能接手。把在跑的任务的现场删了是最坏的一种 bug：
// 它会让一个正在工作的任务突然找不到自己的工作区。
func TestListReapableOnlyTerminalStates(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	wt := "/tmp/reap-scene"
	mk := func(key string, path []State) *Task {
		tk, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: key})
		if err != nil {
			t.Fatalf("建任务 %s 失败: %v", key, err)
		}
		for i, st := range path {
			opts := &TransitionOpts{}
			if i == 0 {
				opts.WorktreePath = &wt
			}
			if _, err := m.Transition(ctx, tk.ID, st, "test", opts); err != nil {
				t.Fatalf("%s 转 %s 失败: %v", key, st, err)
			}
		}
		return tk
	}

	// 三个终态 + 三个非终态，都带 worktree_path
	failedTk := mk("RP-FAILED", []State{StateTriaging, StateImplementing, StateFailed})
	cancelledTk := mk("RP-CANCELLED", []State{StateTriaging, StateCancelled})
	implTk := mk("RP-IMPL", []State{StateTriaging, StateImplementing})
	verifyTk := mk("RP-VERIFY", []State{StateTriaging, StateImplementing, StateVerifying})
	prTk := mk("RP-PROPEN", []State{StateTriaging, StateImplementing, StateVerifying, StatePROpen})

	// cutoff 取未来，让所有行都算「超期」，把状态过滤单独隔离出来
	got, err := m.ListReapableTasks(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("ListReapableTasks 失败: %v", err)
	}
	in := map[int64]bool{}
	for _, tk := range got {
		in[tk.ID] = true
	}

	for _, tk := range []*Task{failedTk, cancelledTk} {
		if !in[tk.ID] {
			t.Errorf("终态任务 %d（%s）应进回收候选", tk.ID, tk.LinearIssueKey)
		}
	}
	for _, tk := range []*Task{implTk, verifyTk, prTk} {
		if in[tk.ID] {
			t.Errorf("AC2：非终态任务 %d（%s）绝不该进回收候选", tk.ID, tk.LinearIssueKey)
		}
	}
}

// TTL 未到的现场不进候选。
func TestListReapableRespectsCutoff(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	wt := "/tmp/reap-fresh"
	tk, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "RP-FRESH"})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	if _, err := m.Transition(ctx, tk.ID, StateTriaging, "test", &TransitionOpts{WorktreePath: &wt}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Transition(ctx, tk.ID, StateFailed, "test", nil); err != nil {
		t.Fatal(err)
	}

	// cutoff 取过去：刚刚更新过的行不该被选中
	got, err := m.ListReapableTasks(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ListReapableTasks 失败: %v", err)
	}
	for _, g := range got {
		if g.ID == tk.ID {
			t.Errorf("TTL 未到的现场 %d 不该进候选", tk.ID)
		}
	}
}

// ---------------------------------------------------------------- 现场认领（B1/B2）

// reapFixture 造一条「终态、留现场、可被回收」的任务行，返回它与快照
// 时刻的 updated_at（认领的等值守卫要用）。
func reapFixture(t *testing.T, m *Machine, userID, repoID int64, key, path, branch string, final State) *Task {
	t.Helper()
	ctx := context.Background()
	tk, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: key})
	if err != nil {
		t.Fatalf("建任务 %s 失败: %v", key, err)
	}
	if _, err := m.Transition(ctx, tk.ID, StateTriaging, "test",
		&TransitionOpts{WorktreePath: &path, BranchName: &branch}); err != nil {
		t.Fatal(err)
	}
	if final == StateFailed {
		if _, err := m.Transition(ctx, tk.ID, StateImplementing, "test", nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := m.Transition(ctx, tk.ID, final, "test", nil)
	if err != nil {
		t.Fatalf("%s 转 %s 失败: %v", key, final, err)
	}
	return got
}

// ★ B2：认领是两阶段的 —— 校验（commit=false）不改任何状态，
// 落账（commit=true）才置空 worktree_path 并写事件。
//
// 顺序必须是「校验 → 删盘 → 落账」：校验若顺手置空了路径，随后因为脏
// 现场决定保留时，那份现场就变成了「磁盘上有、没人认领」的孤儿，会被
// 下一轮清扫收走 —— D4 保留现场的意图被 TTL 回收路径悄悄取消。
func TestClaimForReapTwoPhases(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	path, branch := "/tmp/claim-two-phase", "fix/claim-two-phase"
	tk := reapFixture(t, m, userID, repoID, "CLM-TWO", path, branch, StateFailed)

	// 阶段一：只校验，不改状态
	ok, err := m.ClaimForReap(ctx, tk.ID, path, tk.UpdatedAt, false, "node:test", nil)
	if err != nil {
		t.Fatalf("认领校验失败: %v", err)
	}
	if !ok {
		t.Fatal("终态、路径匹配、updated_at 匹配时校验应通过")
	}
	after, err := m.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.WorktreePath == nil || *after.WorktreePath != path {
		t.Errorf("校验阶段不该动 worktree_path，得到 %v", after.WorktreePath)
	}
	if !after.UpdatedAt.Equal(tk.UpdatedAt) {
		t.Error("校验阶段不该推进 updated_at（否则紧跟的落账会自己失配）")
	}

	// 阶段二：落账
	ok, err = m.ClaimForReap(ctx, tk.ID, path, tk.UpdatedAt, true, "node:test",
		map[string]any{"ttl_seconds": int64(259200)})
	if err != nil {
		t.Fatalf("认领落账失败: %v", err)
	}
	if !ok {
		t.Fatal("落账应成功")
	}
	after, err = m.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.WorktreePath != nil {
		t.Errorf("落账后 worktree_path 应为 NULL，得到 %q", *after.WorktreePath)
	}
	// 分支名刻意保留（排障时仍然有用）
	if after.BranchName == nil || *after.BranchName != branch {
		t.Errorf("branch_name 不该被一起清掉，得到 %v", after.BranchName)
	}

	// ★ 回收动作要进任务事件流：「现场是谁在什么时候按什么规则删的」
	events, err := m.Events(ctx, tk.ID)
	if err != nil {
		t.Fatalf("Events 失败: %v", err)
	}
	last := events[len(events)-1]
	if last.Payload["kind"] != "worktree_reaped" {
		t.Errorf("最后一条事件应是 worktree_reaped，得到 payload=%v", last.Payload)
	}
	if last.Actor != "node:test" {
		t.Errorf("事件的 actor 应是收割者，得到 %q", last.Actor)
	}
	if last.FromState == nil || *last.FromState != StateFailed || last.ToState != StateFailed {
		t.Errorf("回收不是状态转移，from/to 都该是当前状态，得到 %v → %v", last.FromState, last.ToState)
	}
	if _, has := last.Payload["ttl_seconds"]; !has {
		t.Errorf("规则依据（ttl_seconds）应落进 payload，得到 %v", last.Payload)
	}

	// 幂等：worktree_claimed_at 已盖，同一行不再被认领
	ok, err = m.ClaimForReap(ctx, tk.ID, path, tk.UpdatedAt, false, "node:test", nil)
	if err != nil {
		t.Fatalf("重复认领不该报错: %v", err)
	}
	if ok {
		t.Error("已认领的行不该被再认一次")
	}
}

// ★ B2 核心：failed → queued 的重试与收割并发时，认领必须失配。
//
// 这一条钉的是最危险的竞态：候选快照取出后、删盘之前，人点了重试，
// 任务回到 queued 并复用现场续跑。没有 CAS 时收割机会把正在运行的现场
// 删掉、分支强删、worktree_path 置 NULL。
func TestClaimForReapLostToRetry(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	path, branch := "/tmp/claim-retry", "fix/claim-retry"
	tk := reapFixture(t, m, userID, repoID, "CLM-RETRY", path, branch, StateFailed)
	snapshot := tk.UpdatedAt

	// 人点重试：failed → queued（state.go 允许这条边）
	if _, err := m.Transition(ctx, tk.ID, StateQueued, "user:1", nil); err != nil {
		t.Fatalf("重试转移失败: %v", err)
	}

	// 收割机拿着旧快照来认领：必须失配
	ok, err := m.ClaimForReap(ctx, tk.ID, path, snapshot, false, "node:test", nil)
	if err != nil {
		t.Fatalf("认领不该报错: %v", err)
	}
	if ok {
		t.Fatal("B2：重试已把任务转回 queued，认领必须失败（否则会删掉正在续跑的现场）")
	}

	// 现场必须完好：路径还在，等着 pipeline 复用
	after, err := m.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.WorktreePath == nil || *after.WorktreePath != path {
		t.Errorf("认领失败时不该动 worktree_path，得到 %v", after.WorktreePath)
	}
}

// updated_at 变了（任务行被任何写入动过）就失配 —— 不必是重试。
func TestClaimForReapLostToAnyUpdate(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	path, branch := "/tmp/claim-touched", "fix/claim-touched"
	tk := reapFixture(t, m, userID, repoID, "CLM-TOUCH", path, branch, StateFailed)
	snapshot := tk.UpdatedAt

	// 一次与状态无关的字段记账（SetPRNumber 走裸 UPDATE，触发器推进 updated_at）
	if err := m.SetPRNumber(ctx, tk.ID, 4242); err != nil {
		t.Fatalf("SetPRNumber 失败: %v", err)
	}

	ok, err := m.ClaimForReap(ctx, tk.ID, path, snapshot, false, "node:test", nil)
	if err != nil {
		t.Fatalf("认领不该报错: %v", err)
	}
	if ok {
		t.Error("updated_at 已变（快照过期），认领必须失败")
	}
}

// worktree_path 被换成别的路径时失配 —— 决不能删「现在那条路径」。
func TestClaimForReapLostToPathChange(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	path, branch := "/tmp/claim-old", "fix/claim-path"
	tk := reapFixture(t, m, userID, repoID, "CLM-PATH", path, branch, StateFailed)

	ok, err := m.ClaimForReap(ctx, tk.ID, "/tmp/claim-somewhere-else", tk.UpdatedAt, false, "node:test", nil)
	if err != nil {
		t.Fatalf("认领不该报错: %v", err)
	}
	if ok {
		t.Error("认领的路径与任务行不符时必须失败")
	}
}

// 不存在的任务：返回 (false, nil)，不报错。
func TestClaimForReapMissingTask(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	ctx := context.Background()

	ok, err := m.ClaimForReap(ctx, 999999999, "/tmp/nope", time.Now(), false, "node:test", nil)
	if err != nil {
		t.Errorf("任务不存在不该报错，得到: %v", err)
	}
	if ok {
		t.Error("任务不存在时认领必须失败")
	}
}

// ★ B1 的判据：ClaimedWorktreePaths 只算非终态任务。
//
// 收割机删盘前要问的是「这条路径现在有没有在途任务在用」。用
// ReferencedWorktreePaths（任何状态）会把主路径要回收的终态行自己算进来，
// 主路径就永远不敢动手；只算非终态才是正确的闸。
func TestClaimedWorktreePathsOnlyLiveTasks(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	// 同一条路径：一个 failed（老尝试）+ 一个 pr_open（在途）
	shared := "/tmp/claimed-shared"
	branch := "fix/claimed-shared"
	deadTk := reapFixture(t, m, userID, repoID, "CLM-DEAD", shared, branch, StateFailed)

	liveTk, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CLM-LIVE"})
	if err != nil {
		t.Fatal(err)
	}
	livePath := shared
	if _, err := m.Transition(ctx, liveTk.ID, StateTriaging, "test",
		&TransitionOpts{WorktreePath: &livePath}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Transition(ctx, liveTk.ID, StateImplementing, "test", nil); err != nil {
		t.Fatal(err)
	}

	claimed, err := m.ClaimedWorktreePaths(ctx)
	if err != nil {
		t.Fatalf("ClaimedWorktreePaths 失败: %v", err)
	}
	found := false
	for _, p := range claimed {
		if p == shared {
			found = true
		}
	}
	if !found {
		t.Errorf("在途任务 %d 认领的路径应出现在集合里，得到 %v", liveTk.ID, claimed)
	}

	// 把在途任务推进终态后，这条路径就不再算「被认领」
	if _, err := m.Transition(ctx, liveTk.ID, StateCancelled, "test", nil); err != nil {
		t.Fatal(err)
	}
	claimed, err = m.ClaimedWorktreePaths(ctx)
	if err != nil {
		t.Fatalf("ClaimedWorktreePaths 失败: %v", err)
	}
	for _, p := range claimed {
		if p == shared {
			t.Errorf("全是终态时这条路径不该算被认领（否则主路径永远不敢动手），得到 %v", claimed)
		}
	}

	// 而 ReferencedWorktreePaths（判孤儿用）仍然认它 —— 两个问题不同
	referenced, err := m.ReferencedWorktreePaths(ctx)
	if err != nil {
		t.Fatalf("ReferencedWorktreePaths 失败: %v", err)
	}
	found = false
	for _, p := range referenced {
		if p == shared {
			found = true
		}
	}
	if !found {
		t.Errorf("终态行引用的路径仍是「有人指着」，孤儿清扫不该收它，得到 %v", referenced)
	}
	_ = deadTk
}
