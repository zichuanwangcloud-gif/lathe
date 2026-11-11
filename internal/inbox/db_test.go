package inbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
	"github.com/zichuanwangcloud-gif/lathe/internal/testsupport"
)

// 真库测试（docs/features/inbox.md §5.3）。放在 inbox 包而不是 store 包：
// 造数要用 task.Machine，而 store 的同包测试 import task 会成环
// （store → task → tracker → store）。
//
// fixture 一律带随机量、断言只看本 fixture 用户（AGENTS.md §5.5）。

type dbEnv struct {
	st *store.Store
	m  *task.Machine
}

func openDB(t *testing.T) dbEnv {
	t.Helper()
	ctx, cancel := testsupport.ConnectContext(t)
	defer cancel()
	st, err := store.Open(ctx, testsupport.DSN())
	if err != nil {
		testsupport.SkipOrFail(t, "打开 store 失败: %v", err)
	}
	t.Cleanup(st.Close)
	return dbEnv{st: st, m: task.NewMachine(st.Pool())}
}

func nonce() string { return strconv.FormatInt(time.Now().UnixNano(), 10) }

// newUser 建一个独立的 user + repo，测试结束连其名下数据（tasks/task_events/
// prds/inbox_marks 均级联）一起删掉。
func (e dbEnv) newUser(t *testing.T) (userID, repoID int64) {
	t.Helper()
	ctx := context.Background()
	n := nonce()
	email := "inbox-" + strings.ReplaceAll(t.Name(), "/", "_") + "-" + n + "@example.com"
	if err := e.st.Pool().QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`, email).Scan(&userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = e.st.Pool().Exec(context.Background(), `DELETE FROM prds WHERE user_id = $1`, userID)
		_, _ = e.st.Pool().Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})
	if err := e.st.Pool().QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1, $2) RETURNING id`,
		userID, "acme/inbox-"+n).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	return userID, repoID
}

func (e dbEnv) newTask(t *testing.T, userID, repoID int64, key string, dependsOn *int64) *task.Task {
	t.Helper()
	tk, err := e.m.Create(context.Background(), task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: key, DependsOn: dependsOn,
	})
	if err != nil {
		t.Fatalf("建任务 %s 失败: %v", key, err)
	}
	return tk
}

// walk 按给定路径依次转移（每一步都在 state.go 的转移表内）。
func (e dbEnv) walk(t *testing.T, id int64, path []task.State, last *task.TransitionOpts) {
	t.Helper()
	for i, s := range path {
		var opts *task.TransitionOpts
		if i == len(path)-1 {
			opts = last
		}
		if _, err := e.m.Transition(context.Background(), id, s, "system", opts); err != nil {
			t.Fatalf("任务 %d 转到 %s 失败: %v", id, s, err)
		}
	}
}

var toFailed = []task.State{task.StateTriaging, task.StateFailed}

func failOpts(reason string) *task.TransitionOpts {
	return &task.TransitionOpts{FailureReason: &reason}
}

// lastTransitionEventID 是该任务最后一条「真正的转移」事件的 id。
func (e dbEnv) lastTransitionEventID(t *testing.T, id int64) int64 {
	t.Helper()
	evs, err := e.m.Events(context.Background(), id)
	if err != nil {
		t.Fatalf("读事件失败: %v", err)
	}
	var out int64
	for _, ev := range evs {
		if ev.FromState == nil || *ev.FromState != ev.ToState {
			out = ev.ID
		}
	}
	return out
}

func byID(rows []store.InboxTaskRow) map[int64]store.InboxTaskRow {
	m := map[int64]store.InboxTaskRow{}
	for _, r := range rows {
		m[r.ID] = r
	}
	return m
}

func (e dbEnv) candidates(t *testing.T, userID int64) map[int64]store.InboxTaskRow {
	t.Helper()
	rows, err := e.st.InboxTaskCandidates(context.Background(), userID)
	if err != nil {
		t.Fatalf("InboxTaskCandidates 失败: %v", err)
	}
	return byID(rows)
}

func (e dbEnv) service(t *testing.T) *Service {
	t.Helper()
	svc, err := NewService(e.st, identity)
	if err != nil {
		t.Fatalf("NewService 失败: %v", err)
	}
	return svc
}

func TestInboxTaskCandidates(t *testing.T) {
	e := openDB(t)
	ctx := context.Background()
	userID, repoID := e.newUser(t)

	mk := func(key string, path []task.State, last *task.TransitionOpts) *task.Task {
		tk := e.newTask(t, userID, repoID, key, nil)
		e.walk(t, tk.ID, path, last)
		return tk
	}
	mk("Q-1", nil, nil) // queued
	mk("I-1", []task.State{task.StateTriaging, task.StateImplementing}, nil)
	mk("P-1", []task.State{task.StateTriaging, task.StateImplementing, task.StateVerifying, task.StatePROpen}, nil)
	spec := mk("S-1", []task.State{task.StateTriaging, task.StateBlockedSpec},
		&task.TransitionOpts{Payload: map[string]any{"question": "改哪个页面？"}})
	appr := mk("A-1", []task.State{task.StateTriaging, task.StateAwaitingApproval},
		&task.TransitionOpts{Payload: map[string]any{"reason": "等你放行"}})
	failed := mk("F-1", toFailed, failOpts("验证失败"))
	mk("C-1", []task.State{task.StateCancelled}, nil)
	review := mk("R-1", []task.State{task.StateTriaging, task.StateImplementing, task.StateVerifying,
		task.StatePROpen, task.StateReviewFeedback}, nil)

	// blocked_dep 走真实写入点：前驱 P 失败 → PropagateBlocked
	pred := mk("DP-1", toFailed, failOpts("前驱炸了"))
	succ := e.newTask(t, userID, repoID, "DS-1", &pred.ID)
	if _, err := e.m.PropagateBlocked(ctx, pred.ID, "前驱炸了"); err != nil {
		t.Fatalf("PropagateBlocked 失败: %v", err)
	}

	got := e.candidates(t, userID)
	wantStates := map[int64]string{
		spec.ID: "blocked_spec", appr.ID: "awaiting_approval", failed.ID: "failed",
		review.ID: "review_feedback", succ.ID: "blocked_dep", pred.ID: "failed",
	}
	if len(got) != len(wantStates) {
		t.Errorf("候选数: got=%d want=%d (%v)", len(got), len(wantStates), got)
	}
	for id, st := range wantStates {
		r, ok := got[id]
		if !ok {
			t.Errorf("任务 %d(%s) 不在候选中", id, st)
			continue
		}
		if r.State != st {
			t.Errorf("任务 %d 状态: got=%s want=%s", id, r.State, st)
		}
		if want := e.lastTransitionEventID(t, id); r.EventID != want {
			t.Errorf("任务 %d 锚点事件: got=%d want=%d", id, r.EventID, want)
		}
	}

	if r := got[spec.ID]; r.Payload["question"] != "改哪个页面？" {
		t.Errorf("blocked_spec payload 应取自进入事件: got=%v", r.Payload)
	}
	dep := got[succ.ID]
	if dep.PredecessorID == nil || *dep.PredecessorID != pred.ID {
		t.Errorf("blocked_dep PredecessorID: got=%v want=%d", dep.PredecessorID, pred.ID)
	}
	if it, ok := TaskItem(got[review.ID], identity); !ok || it.Kind != KindTaskReviewParked {
		t.Errorf("review_feedback kind: got=%q ok=%v want=%q", it.Kind, ok, KindTaskReviewParked)
	}
	it, ok := TaskItem(dep, identity)
	if !ok || it.Kind != KindTaskBlockedDep {
		t.Errorf("blocked_dep kind: got=%q ok=%v want=%q", it.Kind, ok, KindTaskBlockedDep)
	}
	if !hasLink(it, fmt.Sprintf("/tasks/%d", pred.ID)) {
		t.Errorf("blocked_dep extra: got=%v want 含 /tasks/%d", it.Extra, pred.ID)
	}
}

func TestInboxBlockedDepPredecessorLink(t *testing.T) {
	e := openDB(t)
	ctx := context.Background()

	type setup func(t *testing.T, userA, repoA int64) (succID, predID int64)
	viaPropagate := func(owner func(t *testing.T, userA, repoA int64) (int64, int64)) setup {
		return func(t *testing.T, userA, repoA int64) (int64, int64) {
			pu, pr := owner(t, userA, repoA)
			pred := e.newTask(t, pu, pr, "PRED-"+nonce(), nil)
			e.walk(t, pred.ID, toFailed, failOpts("前驱失败"))
			succ := e.newTask(t, userA, repoA, "SUCC-"+nonce(), &pred.ID)
			if _, err := e.m.PropagateBlocked(ctx, pred.ID, "前驱失败"); err != nil {
				t.Fatalf("PropagateBlocked 失败: %v", err)
			}
			return succ.ID, pred.ID
		}
	}
	sameUser := func(t *testing.T, userA, repoA int64) (int64, int64) { return userA, repoA }
	otherUser := func(t *testing.T, _, _ int64) (int64, int64) { return e.newUser(t) }
	direct := func(blockedBy any) setup {
		return func(t *testing.T, userA, repoA int64) (int64, int64) {
			succ := e.newTask(t, userA, repoA, "SUCC-"+nonce(), nil)
			e.walk(t, succ.ID, []task.State{task.StateBlockedDep},
				&task.TransitionOpts{Payload: map[string]any{"blocked_by": blockedBy}})
			return succ.ID, 0
		}
	}

	cases := []struct {
		name      string
		setup     setup
		deletePre bool
		wantLink  bool
		dirty     bool
	}{
		{name: "前驱属于本人 → 给前驱链接", setup: viaPropagate(sameUser), wantLink: true},
		{name: "前驱属于别人 → 无链接且不泄露对方任务 id", setup: viaPropagate(otherUser)},
		{name: "前驱已被删除 → 无链接", setup: viaPropagate(sameUser), deletePre: true},
		{name: "payload blocked_by 为非数字字符串 → 查询不报错、无链接", setup: direct("abc"), dirty: true},
		{name: "payload blocked_by 为 7.5 → 查询不报错、无链接", setup: direct(7.5), dirty: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			userA, repoA := e.newUser(t)
			// 同一用户名下另一个正常的 blocked_spec：脏 payload 不能拖垮整个收件箱
			ok := e.newTask(t, userA, repoA, "OK-"+nonce(), nil)
			e.walk(t, ok.ID, []task.State{task.StateTriaging, task.StateBlockedSpec}, nil)

			succID, predID := c.setup(t, userA, repoA)
			if c.deletePre {
				if _, err := e.st.Pool().Exec(ctx, `DELETE FROM tasks WHERE id = $1`, predID); err != nil {
					t.Fatalf("删前驱失败: %v", err)
				}
			}

			rows, err := e.st.InboxTaskCandidates(ctx, userA)
			if err != nil {
				t.Fatalf("InboxTaskCandidates err: got=%v want nil", err)
			}
			got := byID(rows)
			if _, present := got[ok.ID]; !present {
				t.Errorf("同用户的正常 blocked_spec 任务 %d 不在结果里", ok.ID)
			}
			r, present := got[succID]
			if !present {
				t.Fatalf("后继 %d 不在候选中（got=%v）", succID, got)
			}
			it, _ := TaskItem(r, identity)
			if c.wantLink {
				if r.PredecessorID == nil || *r.PredecessorID != predID {
					t.Errorf("PredecessorID: got=%v want=%d", r.PredecessorID, predID)
				}
				if !hasLink(it, fmt.Sprintf("/tasks/%d", predID)) {
					t.Errorf("extra: got=%v want 含 /tasks/%d", it.Extra, predID)
				}
				return
			}
			if r.PredecessorID != nil {
				t.Errorf("PredecessorID: got=%d want nil", *r.PredecessorID)
			}
			for _, l := range it.Extra {
				if strings.HasPrefix(l.Href, "/tasks/") {
					t.Errorf("extra 不应含任务链接: got=%v", it.Extra)
				}
			}
			if predID != 0 {
				b, _ := json.Marshal(it.Extra)
				if strings.Contains(string(b), strconv.FormatInt(predID, 10)) {
					t.Errorf("extra 的 JSON 泄露了前驱 id %d: %s", predID, b)
				}
			}
		})
	}
}

func TestInboxFailedSupersededByNewerTask(t *testing.T) {
	e := openDB(t)
	userID, repoID := e.newUser(t)
	old := e.newTask(t, userID, repoID, "SUP-1", nil)
	e.walk(t, old.ID, toFailed, failOpts("第一次失败"))
	newer := e.newTask(t, userID, repoID, "SUP-1", nil) // issue 重开：新任务行，停在 queued

	got := e.candidates(t, userID)
	if _, ok := got[old.ID]; ok {
		t.Errorf("被新行取代的 failed 任务 %d 不应在候选中", old.ID)
	}
	if _, ok := got[newer.ID]; ok {
		t.Errorf("queued 的新行 %d 不应在候选中", newer.ID)
	}

	e.walk(t, newer.ID, toFailed, failOpts("第二次失败"))
	got = e.candidates(t, userID)
	if _, ok := got[old.ID]; ok {
		t.Errorf("新行也失败后，旧行 %d 仍不应在候选中", old.ID)
	}
	if _, ok := got[newer.ID]; !ok {
		t.Errorf("新行 %d 失败后应在候选中", newer.ID)
	}
	if len(got) != 1 {
		t.Errorf("候选数: got=%d want=1", len(got))
	}
}

func itemByKey(t *testing.T, svc *Service, userID int64, subjectID int64) *Item {
	t.Helper()
	p, err := svc.Items(context.Background(), userID, Filter{Show: FilterAll})
	if err != nil {
		t.Fatalf("Items 失败: %v", err)
	}
	for i := range p.Items {
		if p.Items[i].SubjectID == subjectID {
			return &p.Items[i]
		}
	}
	return nil
}

func TestInboxKeyChangesOnReentry(t *testing.T) {
	e := openDB(t)
	ctx := context.Background()
	svc := e.service(t)
	userID, repoID := e.newUser(t)
	tk := e.newTask(t, userID, repoID, "RE-1", nil)
	e.walk(t, tk.ID, toFailed, failOpts("第一次"))

	first := itemByKey(t, svc, userID, tk.ID)
	if first == nil {
		t.Fatal("失败后应出现事项")
	}
	if _, err := svc.Mark(ctx, userID, first.Key, store.InboxMarkRead); err != nil {
		t.Fatalf("标已读失败: %v", err)
	}

	e.walk(t, tk.ID, []task.State{task.StateQueued, task.StateTriaging, task.StateFailed}, failOpts("第二次"))
	second := itemByKey(t, svc, userID, tk.ID)
	if second == nil {
		t.Fatal("再次失败后应出现事项")
	}
	if second.Key == first.Key {
		t.Errorf("再次进入等人状态，键应变化: got=%q (k1=%q)", second.Key, first.Key)
	}
	if second.Read {
		t.Errorf("新键应为未读: got read=true")
	}
}

func TestInboxKeyStableAcrossReap(t *testing.T) {
	e := openDB(t)
	ctx := context.Background()
	svc := e.service(t)
	userID, repoID := e.newUser(t)

	tk := e.newTask(t, userID, repoID, "REAP-1", nil)
	path := "/tmp/lathe-inbox-reap-" + nonce()
	reason := "失败了"
	e.walk(t, tk.ID, toFailed, &task.TransitionOpts{FailureReason: &reason, WorktreePath: &path})

	k1 := e.candidates(t, userID)[tk.ID].EventID
	if k1 == 0 {
		t.Fatal("失败任务不在候选中")
	}
	key1 := taskKey(tk.ID, k1)
	if err := e.st.UpsertInboxMark(ctx, userID, key1, store.InboxSubject{TaskID: tk.ID}, store.InboxMarkDismiss); err != nil {
		t.Fatalf("忽略失败: %v", err)
	}
	_, totalBefore, err := svc.Count(ctx, userID)
	if err != nil {
		t.Fatalf("Count 失败: %v", err)
	}

	cur, err := e.m.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := e.m.ClaimForReap(ctx, tk.ID, path, cur.UpdatedAt, true, "node:test", nil)
	if err != nil || !claimed {
		t.Fatalf("ClaimForReap: got=(%v, %v) want=(true, nil)", claimed, err)
	}
	evs, _ := e.m.Events(ctx, tk.ID)
	last := evs[len(evs)-1]
	if last.FromState == nil || *last.FromState != task.StateFailed || last.ToState != task.StateFailed ||
		last.Payload["kind"] != "worktree_reaped" {
		t.Fatalf("前提不成立：最后一条事件应是 failed→failed worktree_reaped，got=%+v", last)
	}

	if got := e.candidates(t, userID)[tk.ID].EventID; got != k1 {
		t.Errorf("回收后锚点: got=e%d want=e%d", got, k1)
	}
	if it := itemByKey(t, svc, userID, tk.ID); it != nil {
		t.Errorf("已忽略的事项在回收后重新出现: %+v", it)
	}
	if _, total, err := svc.Count(ctx, userID); err != nil || total != totalBefore {
		t.Errorf("Count total: got=(%d, %v) want=(%d, nil)", total, err, totalBefore)
	}
}

func (e dbEnv) countMarks(t *testing.T, where string, args ...any) int {
	t.Helper()
	var n int
	if err := e.st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM inbox_marks WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatalf("数标记失败: %v", err)
	}
	return n
}

func TestInboxMarkConcurrentAndPruned(t *testing.T) {
	e := openDB(t)
	ctx := context.Background()
	userID, repoID := e.newUser(t)
	tk := e.newTask(t, userID, repoID, "CC-1", nil)
	subj := store.InboxSubject{TaskID: tk.ID}
	k1 := taskKey(tk.ID, 1)

	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = e.st.UpsertInboxMark(ctx, userID, k1, subj, store.InboxMarkRead)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: got err=%v want nil", i, err)
		}
	}
	if n := e.countMarks(t, `user_id = $1`, userID); n != 1 {
		t.Errorf("并发标已读后行数: got=%d want=1", n)
	}

	k2 := taskKey(tk.ID, 2)
	if err := e.st.UpsertInboxMark(ctx, userID, k2, subj, store.InboxMarkRead); err != nil {
		t.Fatalf("新键标已读失败: %v", err)
	}
	if n := e.countMarks(t, `user_id = $1 AND task_id = $2`, userID, tk.ID); n != 1 {
		t.Errorf("同一主体换键后行数: got=%d want=1", n)
	}
	var key string
	if err := e.st.Pool().QueryRow(ctx, `SELECT item_key FROM inbox_marks WHERE user_id = $1`, userID).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if key != k2 {
		t.Errorf("剩下的行的键: got=%q want=%q", key, k2)
	}
}

func TestInboxMarksCascadeOnSubjectDelete(t *testing.T) {
	e := openDB(t)
	ctx := context.Background()
	userID, repoID := e.newUser(t)

	t.Run("删任务后其标记级联消失", func(t *testing.T) {
		tk := e.newTask(t, userID, repoID, "DEL-1", nil)
		if err := e.st.UpsertInboxMark(ctx, userID, taskKey(tk.ID, 1), store.InboxSubject{TaskID: tk.ID}, store.InboxMarkRead); err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.Pool().Exec(ctx, `DELETE FROM tasks WHERE id = $1`, tk.ID); err != nil {
			t.Fatal(err)
		}
		if n := e.countMarks(t, `task_id = $1`, tk.ID); n != 0 {
			t.Errorf("got=%d want=0", n)
		}
	})
	t.Run("删 PRD 后其标记级联消失", func(t *testing.T) {
		p, err := e.st.CreatePRD(ctx, store.CreatePRDParams{UserID: userID, RepoID: repoID, PRDType: "feature", OriginalInput: "x"})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.st.UpsertInboxMark(ctx, userID, prdKey(p.ID, "review", 0), store.InboxSubject{PRDID: p.ID}, store.InboxMarkRead); err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.Pool().Exec(ctx, `DELETE FROM prds WHERE id = $1`, p.ID); err != nil {
			t.Fatal(err)
		}
		if n := e.countMarks(t, `prd_id = $1`, p.ID); n != 0 {
			t.Errorf("got=%d want=0", n)
		}
	})
}
