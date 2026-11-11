package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// interjectionFixture 建「用户 + 仓库 + 任务」，email/仓库名带随机量。
func interjectionFixture(t *testing.T, st *Store, state string) (userID, taskID int64) {
	t.Helper()
	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	userID, taskID = agentEventFixture(t, st, "ij-"+t.Name()+"-"+nonce+"@example.com")
	if state != "" {
		if _, err := st.pool.Exec(context.Background(), `UPDATE tasks SET state=$2 WHERE id=$1`, taskID, state); err != nil {
			t.Fatalf("摆任务状态失败: %v", err)
		}
	}
	return userID, taskID
}

// AC-2：0032 的 CHECK 约束真的挡住了自相矛盾的行。
func TestInterjectStatusCheck(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	var cols int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns WHERE table_name='task_interjections'`).Scan(&cols); err != nil {
		t.Fatalf("查 schema 失败: %v", err)
	}
	if cols != 12 {
		t.Errorf("task_interjections 列数 got=%d want=12（表不存在？）", cols)
		return
	}
	userID, taskID := interjectionFixture(t, st, "")

	cases := []struct {
		name    string
		sql     string
		args    []any
		wantErr string // 期望的 SQLSTATE；空 = 应成功
	}{
		{"status 非法", `INSERT INTO task_interjections (task_id,user_id,body,status) VALUES ($1,$2,'x','bogus')`, nil, "23514"},
		{"delivered 无 delivered_at", `INSERT INTO task_interjections (task_id,user_id,body,status) VALUES ($1,$2,'x','delivered')`, nil, "23514"},
		{"failed 无 error", `INSERT INTO task_interjections (task_id,user_id,body,status) VALUES ($1,$2,'x','failed')`, nil, "23514"},
		{"queued 带 error", `INSERT INTO task_interjections (task_id,user_id,body,status,error) VALUES ($1,$2,'x','queued','e')`, nil, "23514"},
		{"body 超 16384", `INSERT INTO task_interjections (task_id,user_id,body) VALUES ($1,$2,$3)`, []any{strings.Repeat("a", 16385)}, "23514"},
		{"delivered 带 no_replay_ack", `INSERT INTO task_interjections (task_id,user_id,body,status,delivered_at,error) VALUES ($1,$2,'x','delivered',now(),'no_replay_ack')`, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := st.pool.Exec(ctx, tc.sql, append([]any{taskID, userID}, tc.args...)...)
			var pgErr *pgconn.PgError
			got := ""
			if errors.As(err, &pgErr) {
				got = pgErr.Code
			} else if err != nil {
				t.Fatalf("非 PG 错误: %v", err)
			}
			if got != tc.wantErr {
				t.Errorf("SQLSTATE got=%q want=%q（err=%v）", got, tc.wantErr, err)
			}
		})
	}
}

// rev 不变量：每次写都推进 rev；结算幂等；只动 queued。
func TestInterjectionRevAndSettle(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, taskID := interjectionFixture(t, st, "verifying")

	open := func(string) string { return "" }
	a, state, err := st.InsertInterjectionGated(ctx, taskID, userID, "A", open)
	if err != nil || state != "verifying" || a.Status != InterjectionQueued {
		t.Fatalf("插入 got=(%+v,%q,%v)", a, state, err)
	}
	b, _, _ := st.InsertInterjectionGated(ctx, taskID, userID, "B", open)
	if b.Rev <= a.Rev {
		t.Errorf("rev 应单调：a=%d b=%d", a.Rev, b.Rev)
	}
	d, err := st.MarkInterjectionDelivering(ctx, taskID, b.ID, "11111111-1111-4111-8111-111111111111", "fix-1", "s")
	if err != nil || d == nil || d.Status != InterjectionDelivering || d.Rev <= b.Rev {
		t.Fatalf("MarkDelivering got=(%+v,%v)", d, err)
	}

	// 还在 verifying：结算不动
	if n, err := st.SettleQueuedInterjections(ctx, taskID); err != nil || n != 0 {
		t.Errorf("verifying 下结算 got=(%d,%v) want=(0,nil)", n, err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE tasks SET state='pr_open' WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	if n, err := st.SettleQueuedInterjections(ctx, taskID); err != nil || n != 1 {
		t.Errorf("pr_open 下结算 got=(%d,%v) want=(1,nil)", n, err)
	}
	if n, _ := st.SettleQueuedInterjections(ctx, taskID); n != 0 {
		t.Errorf("结算应幂等：第二次 got=%d want=0", n)
	}
	ga, _ := st.GetInterjection(ctx, a.ID)
	if ga.Status != InterjectionFailed || ga.Error == nil || !strings.Contains(*ga.Error, "没有后续 agent 轮次") || ga.Rev <= d.Rev {
		t.Errorf("a got=%+v want failed+没有后续 agent 轮次+rev 前进", ga)
	}
	gb, _ := st.GetInterjection(ctx, b.ID)
	if gb.Status != InterjectionDelivering {
		t.Errorf("结算不得动 delivering 行：got=%s", gb.Status)
	}
	// 回退：任务已 pr_open → failed
	rb, seen, err := st.RevertInterjectionGated(ctx, taskID, b.ID)
	if err != nil || seen != "pr_open" || rb == nil || rb.Status != InterjectionFailed {
		t.Errorf("回退 got=(%+v,%q,%v) want failed", rb, seen, err)
	}
}

// AC-16（store 侧）：启动清扫 = delivering → failed（含「重启」）；pr_open /
// blocked_dep 的 queued → failed；implementing（Reconcile 后为 queued）的 queued
// 保留；delivered 行逐字节不变。
func TestStartupSweepInterjectionsStore(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	open := func(string) string { return "" }

	uid, tDeliv := interjectionFixture(t, st, "implementing")
	d1, _, _ := st.InsertInterjectionGated(ctx, tDeliv, uid, "d1", open)
	d2, _, _ := st.InsertInterjectionGated(ctx, tDeliv, uid, "d2", open)
	_, _ = st.MarkInterjectionDelivering(ctx, tDeliv, d1.ID, "11111111-1111-4111-8111-111111111111", "implement", "s")
	_, _ = st.MarkInterjectionDelivering(ctx, tDeliv, d2.ID, "22222222-2222-4222-8222-222222222222", "implement", "s")
	done, _, _ := st.InsertInterjectionGated(ctx, tDeliv, uid, "done", open)
	_, _ = st.MarkInterjectionDelivering(ctx, tDeliv, done.ID, "33333333-3333-4333-8333-333333333333", "implement", "s")
	doneRow, _ := st.MarkInterjectionDelivered(ctx, tDeliv, done.ID, "")

	uid2, tPR := interjectionFixture(t, st, "implementing")
	qPR, _, _ := st.InsertInterjectionGated(ctx, tPR, uid2, "pr", open)
	uid3, tDep := interjectionFixture(t, st, "implementing")
	qDep, _, _ := st.InsertInterjectionGated(ctx, tDep, uid3, "dep", open)
	uid4, tImpl := interjectionFixture(t, st, "implementing")
	qImpl, _, _ := st.InsertInterjectionGated(ctx, tImpl, uid4, "impl", open)

	for id, s := range map[int64]string{tPR: "pr_open", tDep: "blocked_dep", tImpl: "queued"} {
		if _, err := st.pool.Exec(ctx, `UPDATE tasks SET state=$2 WHERE id=$1`, id, s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.FailDeliveringInterjections(ctx); err != nil {
		t.Fatalf("FailDelivering: %v", err)
	}
	ids, err := st.InterjectionSettleCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := st.SettleQueuedInterjections(ctx, id); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name    string
		id      int64
		status  string
		errPart string
	}{
		{"delivering#1", d1.ID, InterjectionFailed, "重启"},
		{"delivering#2", d2.ID, InterjectionFailed, "重启"},
		{"pr_open 的 queued", qPR.ID, InterjectionFailed, "没有后续 agent 轮次"},
		{"blocked_dep 的 queued", qDep.ID, InterjectionFailed, "前置任务失败"},
		{"queued 任务的 queued 保留", qImpl.ID, InterjectionQueued, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := st.GetInterjection(ctx, tc.id)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tc.status {
				t.Errorf("status got=%s want=%s", got.Status, tc.status)
			}
			if tc.errPart != "" && (got.Error == nil || !strings.Contains(*got.Error, tc.errPart)) {
				t.Errorf("error got=%v want 含 %q", got.Error, tc.errPart)
			}
		})
	}
	after, _ := st.GetInterjection(ctx, done.ID)
	if after.Status != doneRow.Status || after.Rev != doneRow.Rev || !after.DeliveredAt.Equal(*doneRow.DeliveredAt) {
		t.Errorf("delivered 行不应被动过：before=%+v after=%+v", doneRow, after)
	}
}

// 迟到回显的 store 侧：只有「delivered + no_replay_ack + delivery_id 对得上」的行
// 被清空 error 并前进 rev；其它情形一律不动（返回 nil）。
func TestMarkInterjectionReplayAcked(t *testing.T) {
	const did = "22222222-2222-4222-8222-222222222222"
	const other = "33333333-3333-4333-8333-333333333333"
	open := func(string) string { return "" }
	cases := []struct {
		name      string
		errText   string // MarkDelivered 时的 error；"-" = 停在 delivering
		ackID     string
		wantClear bool
	}{
		{name: "no_replay_ack_同一delivery_id_清空", errText: InterjectionNoReplayAck, ackID: did, wantClear: true},
		{name: "no_replay_ack_别的delivery_id_不动", errText: InterjectionNoReplayAck, ackID: other},
		{name: "已确认_不动", errText: "", ackID: did},
		{name: "仍在delivering_不动", errText: "-", ackID: did},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			userID, taskID := interjectionFixture(t, st, "implementing")
			r, _, err := st.InsertInterjectionGated(ctx, taskID, userID, "x", open)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.MarkInterjectionDelivering(ctx, taskID, r.ID, did, "fix-1", "s"); err != nil {
				t.Fatal(err)
			}
			if c.errText != "-" {
				if _, err := st.MarkInterjectionDelivered(ctx, taskID, r.ID, c.errText); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := st.GetInterjection(ctx, r.ID)
			got, err := st.MarkInterjectionReplayAcked(ctx, taskID, r.ID, c.ackID)
			if err != nil {
				t.Fatalf("err got=%v want nil", err)
			}
			after, _ := st.GetInterjection(ctx, r.ID)
			if c.wantClear {
				if got == nil || after.Error != nil || after.Status != InterjectionDelivered || after.Rev <= before.Rev {
					t.Errorf("got=%+v after=%+v want delivered、error=NULL、rev>%d", got, after, before.Rev)
				}
				return
			}
			if got != nil || after.Rev != before.Rev || after.Status != before.Status {
				t.Errorf("不应改行：got=%+v before=%+v after=%+v", got, before, after)
			}
		})
	}
}
