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
