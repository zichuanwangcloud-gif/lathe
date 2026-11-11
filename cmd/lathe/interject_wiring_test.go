package main

import (
	"context"
	"strings"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/config"
	"github.com/zichuanwangcloud-gif/lathe/internal/runner"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// 生产装配永远接插话（没有开关）：buildPipeline 返回时 Interjections 已装好，
// 且登记了脱敏权威层的同一份 KnownSecrets。
func TestBuildPipelineWiresInterjections(t *testing.T) {
	st := testStore(t)
	cfg := config.Config{WorkspaceRoot: t.TempDir(), ClaudeBin: "claude", LightSlots: 1, HeavySlots: 1,
		Database: config.Database{Password: "db-pass-" + uniqueKey("x")}}
	p, _, err := buildPipeline(cfg, st, testSecrets(t, st), nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Interjections == nil {
		t.Fatal("buildPipeline 返回时 Interjections 必须已装好（生产装配永远接）")
	}
	if strings.Join(p.Interjections.KnownSecrets, ",") != strings.Join(p.KnownSecrets, ",") {
		t.Errorf("插话脱敏的登记值应与 Pipeline 同源：got=%d want=%d 项", len(p.Interjections.KnownSecrets), len(p.KnownSecrets))
	}
}

func ijInsert(t *testing.T, st *store.Store, taskID, userID int64, body string) *store.Interjection {
	t.Helper()
	r, _, err := st.InsertInterjectionGated(context.Background(), taskID, userID, body, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// AC-16：Reconcile + 启动清扫后：两条 delivering → failed（含「重启」）；
// pr_open 与 blocked_dep 任务的 queued → failed；implementing（已被 Reconcile
// 转 queued）任务的 queued 仍是 queued；delivered 行逐字节未变。
func TestStartupSweepInterjections(t *testing.T) {
	st := testStore(t)
	userID, repoID := fixture(t, st)
	ctx := context.Background()
	q := testQueue(st, &fakePipeline{})
	ij := &runner.Interjections{Store: st, Tasks: q.tasks}

	mk := func(state string) int64 {
		tk, err := q.tasks.Create(ctx, task.CreateParams{UserID: userID, RepoID: repoID, ExternalKey: uniqueKey("Q-IJ")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool().Exec(ctx, `UPDATE tasks SET state=$2 WHERE id=$1`, tk.ID, state); err != nil {
			t.Fatal(err)
		}
		return tk.ID
	}
	tImpl := mk("implementing")
	d1 := ijInsert(t, st, tImpl, userID, "d1")
	d2 := ijInsert(t, st, tImpl, userID, "d2")
	_, _ = st.MarkInterjectionDelivering(ctx, tImpl, d1.ID, "11111111-1111-4111-8111-111111111111", "implement", "s")
	_, _ = st.MarkInterjectionDelivering(ctx, tImpl, d2.ID, "22222222-2222-4222-8222-222222222222", "implement", "s")
	qImpl := ijInsert(t, st, tImpl, userID, "keep")
	dn := ijInsert(t, st, tImpl, userID, "done")
	_, _ = st.MarkInterjectionDelivering(ctx, tImpl, dn.ID, "33333333-3333-4333-8333-333333333333", "implement", "s")
	before, _ := st.MarkInterjectionDelivered(ctx, tImpl, dn.ID, "")

	tPR := mk("implementing")
	qPR := ijInsert(t, st, tPR, userID, "pr")
	_, _ = st.Pool().Exec(ctx, `UPDATE tasks SET state='pr_open' WHERE id=$1`, tPR)
	tDep := mk("implementing")
	qDep := ijInsert(t, st, tDep, userID, "dep")
	_, _ = st.Pool().Exec(ctx, `UPDATE tasks SET state='blocked_dep' WHERE id=$1`, tDep)

	if err := q.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ij.StartupSweep(ctx); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, want, errPart string
		id                  int64
	}{
		{"delivering#1", "failed", "重启", d1.ID},
		{"delivering#2", "failed", "重启", d2.ID},
		{"pr_open 的 queued", "failed", "没有后续 agent 轮次", qPR.ID},
		{"blocked_dep 的 queued", "failed", "前置任务失败", qDep.ID},
		{"Reconcile 转回 queued 的任务保留 queued", "queued", "", qImpl.ID},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := st.GetInterjection(ctx, c.id)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != c.want || (c.errPart != "" && (got.Error == nil || !strings.Contains(*got.Error, c.errPart))) {
				t.Errorf("got=(%s,%v) want=(%s,含%q)", got.Status, got.Error, c.want, c.errPart)
			}
		})
	}
	after, _ := st.GetInterjection(ctx, dn.ID)
	if after.Status != before.Status || after.Rev != before.Rev || !after.DeliveredAt.Equal(*before.DeliveredAt) || after.Body != before.Body {
		t.Errorf("delivered 行不应被动过：before=%+v after=%+v", before, after)
	}
}

// AC-19（R3 行）：取消走 queue.CancelForIssue（Linear 取消联动），Execute 不再运行，
// 读时结算出 failed「已取消」。
func TestInterjectSettledOnReadAfterCancelForIssue(t *testing.T) {
	st := testStore(t)
	userID, repoID := fixture(t, st)
	ctx := context.Background()
	q := testQueue(st, &fakePipeline{})
	ij := &runner.Interjections{Store: st, Tasks: q.tasks}
	issueID := uniqueKey("uuid-ij")
	tk, err := q.tasks.Create(ctx, task.CreateParams{UserID: userID, RepoID: repoID, ExternalKey: uniqueKey("Q-IJC"), ExternalID: issueID})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = st.Pool().Exec(ctx, `UPDATE tasks SET state='implementing' WHERE id=$1`, tk.ID)
	res, err := ij.Submit(ctx, tk.ID, userID, "取消前说的")
	if err != nil || res.Outcome != runner.OutcomeQueued {
		t.Fatalf("got=(%v,%v)", res.Outcome, err)
	}
	_, _ = st.Pool().Exec(ctx, `UPDATE tasks SET state='queued' WHERE id=$1`, tk.ID)
	if ids, err := q.CancelForIssue(ctx, userID, issueID, tk.ExternalKey); err != nil || len(ids) != 1 {
		t.Fatalf("CancelForIssue got=(%v,%v)", ids, err)
	}
	rows, err := ij.ListSince(ctx, tk.ID, 0, 200)
	if err != nil || len(rows) != 1 {
		t.Fatalf("got=(%d,%v)", len(rows), err)
	}
	if r := rows[0]; r.Status != store.InterjectionFailed || r.Error == nil || !strings.Contains(*r.Error, "已取消") || r.Rev <= res.Row.Rev {
		t.Errorf("got=%+v want failed + 已取消 + rev 前进", r)
	}
}
