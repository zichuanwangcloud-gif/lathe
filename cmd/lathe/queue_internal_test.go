package main

import (
	"context"
	"strings"
	"testing"

	"github.com/Clouditera/lathe/internal/store"
	"github.com/Clouditera/lathe/internal/task"
	"github.com/Clouditera/lathe/internal/tracker"
)

// 内置工单的任务编排（docs/09 §3 F4）：EnqueueInternal 建任务 +
// 状态联动，CancelForInternalIssue 取消联动。

// TestEnqueueInternalCreatesTaskFromIssueRepo 覆盖 F4-AC1/AC3：
// 任务建到工单登记的仓库（不走「每用户第一个仓库」的随机解析），
// provider=internal，优先级从工单带入，工单联动为 in_progress。
func TestEnqueueInternalCreatesTaskFromIssueRepo(t *testing.T) {
	st := testStore(t)
	userID, _ := fixture(t, st)
	ctx := context.Background()
	q := testQueue(st, &fakePipeline{})

	// 第二个仓库：验证任务确实挂到工单登记的那个，而不是「第一个」
	var repo2 int64
	if err := st.Pool().QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo, gate_mode) VALUES ($1,$2,'manual') RETURNING id`,
		userID, "acme/q-internal-"+uniqueKey("r")).Scan(&repo2); err != nil {
		t.Fatal(err)
	}

	it, err := st.CreateIssue(ctx, store.CreateIssueParams{
		UserID: userID, RepoID: repo2, Title: "内部工单", Priority: 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := q.EnqueueInternal(ctx, userID, it.ID); err != nil {
		t.Fatalf("EnqueueInternal 失败: %v", err)
	}

	active, err := q.tasks.ActiveByKey(ctx, userID, tracker.ProviderInternal, it.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 {
		t.Fatalf("应建出 1 条任务，得到 %d", len(active))
	}
	tk := active[0]
	if tk.RepoID != repo2 {
		t.Errorf("任务应挂到工单登记的仓库 %d，得到 %d", repo2, tk.RepoID)
	}
	if tk.TrackerProvider != tracker.ProviderInternal {
		t.Errorf("provider = %q，期望 internal", tk.TrackerProvider)
	}
	if tk.ExternalID != nil {
		t.Errorf("内置任务的 external_id 应为 NULL，得到 %v", *tk.ExternalID)
	}
	if tk.GateMode != "manual" {
		t.Errorf("gate_mode 应从仓库复制（manual），得到 %q", tk.GateMode)
	}
	if tk.Priority != 3 {
		t.Errorf("优先级应从工单带入（3），得到 %d", tk.Priority)
	}

	// 联动：工单 → in_progress
	got, _ := st.GetIssue(ctx, it.ID, userID)
	if got.State != store.IssueInProgress {
		t.Errorf("工单应联动为 in_progress，得到 %s", got.State)
	}

	// 重复开跑：in_progress 闸门拒绝（人话）
	if err := q.EnqueueInternal(ctx, userID, it.ID); err == nil ||
		!strings.Contains(err.Error(), "进行中") {
		t.Errorf("重复开跑应报「进行中」，得到 %v", err)
	}
}

// TestEnqueueInternalRejectsCancelledIssue：已取消工单不能开跑，
// 要先人工重开（防呆：取消是显式决定，不能被一个按钮静默翻案）。
func TestEnqueueInternalRejectsCancelledIssue(t *testing.T) {
	st := testStore(t)
	userID, repoID := fixture(t, st)
	ctx := context.Background()
	q := testQueue(st, &fakePipeline{})

	it, _ := st.CreateIssue(ctx, store.CreateIssueParams{UserID: userID, RepoID: repoID, Title: "t"})
	if _, err := st.UpdateIssue(ctx, it.ID, userID, store.UpdateIssueParams{State: ptr(store.IssueCancelled)}); err != nil {
		t.Fatal(err)
	}

	if err := q.EnqueueInternal(ctx, userID, it.ID); err == nil ||
		!strings.Contains(err.Error(), "已取消") {
		t.Errorf("已取消工单开跑应报「已取消」，得到 %v", err)
	}
	if n, _ := func() (int, error) {
		var n int
		err := st.Pool().QueryRow(ctx,
			`SELECT count(*) FROM tasks WHERE external_key = $1`, it.Key).Scan(&n)
		return n, err
	}(); n != 0 {
		t.Errorf("已取消工单不该建出任务，库里有 %d 条", n)
	}
}

// TestCancelForInternalIssue 覆盖 F4-AC4：取消联动把在途任务转
// cancelled、失败传播照旧、工单回 open；只改库状态，不停在途 agent。
func TestCancelForInternalIssue(t *testing.T) {
	st := testStore(t)
	userID, repoID := fixture(t, st)
	ctx := context.Background()
	q := testQueue(st, &fakePipeline{})

	it, _ := st.CreateIssue(ctx, store.CreateIssueParams{UserID: userID, RepoID: repoID, Title: "t"})
	if err := q.EnqueueInternal(ctx, userID, it.ID); err != nil {
		t.Fatal(err)
	}
	active, _ := q.tasks.ActiveByKey(ctx, userID, tracker.ProviderInternal, it.Key)
	tkID := active[0].ID

	cancelled, err := q.CancelForInternalIssue(ctx, userID, it.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(cancelled) != 1 || cancelled[0] != tkID {
		t.Errorf("应取消任务 %d，得到 %v", tkID, cancelled)
	}
	got, err := q.tasks.Get(ctx, tkID)
	if err != nil || got.State != task.StateCancelled {
		t.Fatalf("任务应转 cancelled: %v, %+v", err, got)
	}

	// 联动：工单回 open
	issue, _ := st.GetIssue(ctx, it.ID, userID)
	if issue.State != store.IssueOpen {
		t.Errorf("工单应回 open，得到 %s", issue.State)
	}

	// 再次取消：无在途任务，幂等空操作
	cancelled2, err := q.CancelForInternalIssue(ctx, userID, it.Key)
	if err != nil || len(cancelled2) != 0 {
		t.Errorf("重复取消应幂等空转: %v, %v", cancelled2, err)
	}

	// 重开后可再次开跑（同一 key 的活任务唯一索引不挡终态历史）
	if _, err := st.UpdateIssue(ctx, it.ID, userID, store.UpdateIssueParams{State: ptr(store.IssueOpen)}); err != nil {
		t.Fatal(err)
	}
	if err := q.EnqueueInternal(ctx, userID, it.ID); err != nil {
		t.Errorf("重开后应能再次开跑: %v", err)
	}
}
