package schedule

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// day0 是测试的「今天」：2026-10-19（周一），全部按 UTC。
var day0 = time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)

func at(days, h, m, s int) time.Time {
	return day0.AddDate(0, 0, days).Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second)
}

// daily0900 建一条每日 09:00 的规则：下一次 = 今天 09:00，基线 = 昨天 09:00。
func daily0900(f *fx) int64 {
	return f.schedule(schedOpts{minute: 540, next: at(0, 9, 0, 0), lastSlot: at(-1, 9, 0, 0)})
}

// ================================================================ AC-6

func TestTickFiresDueScheduleOnce(t *testing.T) {
	st := testStore(t)
	f := newFx(t, st, "fire")
	sid := daily0900(f)
	guardResidue(t, st, at(0, 9, 0, 30), sid)

	r := newRunner(t, st, linearFor(nil, f), newClock(at(0, 9, 0, 30)), "n1")
	res := tick(t, r)
	if !claimedSchedule(res, sid) {
		t.Fatalf("应认领本 fixture 规则 %d，got %+v", sid, res.Claimed)
	}
	drain(t, r, 0)

	if got := f.lin.creates.Load(); got != 1 {
		t.Errorf("issueCreate 次数: got %d, want 1", got)
	}
	runs := f.runs(sid)
	if len(runs) != 1 || runs[0].Status != store.RunCreated || runs[0].Trigger != store.TriggerSchedule {
		t.Fatalf("runs: got %+v, want 1 行 created/schedule", runs)
	}
	in := f.lin.lastInput()
	if in["id"] != runs[0].LinearIssueUUID || in["assigneeId"] != f.userID || in["teamId"] != "team-1" {
		t.Errorf("建单输入: got %v, want id=run uuid、assigneeId=%s、teamId=team-1", in, f.userID)
	}
	if desc, _ := in["description"].(string); !strings.Contains(desc, fmt.Sprintf("由 Lathe 定时规则 #%d", sid)) {
		t.Errorf("正文应追加溯源行，got %q", desc)
	}
	if runs[0].LinearIssueIdentifier == nil || *runs[0].LinearIssueIdentifier != "ENG-1" {
		t.Errorf("identifier: got %s, want ENG-1", strp(runs[0].LinearIssueIdentifier))
	}
	if sc := f.sched(sid); sc.NextFireAt == nil || !sc.NextFireAt.Equal(at(1, 9, 0, 0)) {
		t.Errorf("next_fire_at: got %v, want %v", sc.NextFireAt, at(1, 9, 0, 0))
	}
}

func TestTickRestartDoesNotRefire(t *testing.T) {
	st := testStore(t)
	f := newFx(t, st, "restart")
	sid := daily0900(f)
	now := at(0, 9, 0, 30)
	guardResidue(t, st, now, sid)

	r1 := newRunner(t, st, linearFor(nil, f), newClock(now), "n1")
	tick(t, r1)
	drain(t, r1, 0)
	r1.Close()

	// 「重启」：同一个 Now，新的 Runner
	r2 := newRunner(t, st, linearFor(nil, f), newClock(now), "n1")
	if res := tick(t, r2); claimedSchedule(res, sid) {
		t.Errorf("重启后同一 Now 不应再认领 %d", sid)
	}
	drain(t, r2, 0)

	if got := f.lin.creates.Load(); got != 1 {
		t.Errorf("issueCreate 次数: got %d, want 1", got)
	}
	if runs := f.runs(sid); len(runs) != 1 || runs[0].Status != store.RunCreated {
		t.Errorf("runs: got %d 行, want 1 行 created", len(runs))
	}
	if sc := f.sched(sid); !sc.NextFireAt.Equal(at(1, 9, 0, 0)) {
		t.Errorf("next_fire_at: got %v, want %v", sc.NextFireAt, at(1, 9, 0, 0))
	}
}

func TestTickAdvancesAcrossDST(t *testing.T) {
	cases := []struct {
		name     string
		tz       string
		hh, mm   int
		slot     time.Time // DST 当天解析出的计划时刻
		nextSlot time.Time // 次日同一墙钟
	}{
		{"NY_spring_0230", "America/New_York", 2, 30, utc("2026-03-08T07:30:00Z"), utc("2026-03-09T06:30:00Z")},
		{"NY_fall_0130", "America/New_York", 1, 30, utc("2026-11-01T05:30:00Z"), utc("2026-11-02T06:30:00Z")},
		{"LordHowe_spring_0215", "Australia/Lord_Howe", 2, 15, utc("2026-10-03T15:45:00Z"), utc("2026-10-04T15:15:00Z")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			f := newFx(t, st, "dst")
			sid := f.schedule(schedOpts{minute: tc.hh*60 + tc.mm, tz: tc.tz, next: tc.slot, lastSlot: tc.slot.AddDate(0, 0, -1)})
			now := tc.slot.Add(30 * time.Second)
			guardResidue(t, st, now, sid)
			r := newRunner(t, st, linearFor(nil, f), newClock(now), "n1")
			for i := 0; i < 3; i++ {
				tick(t, r)
				sc := f.sched(sid)
				if sc.NextFireAt == nil || !sc.NextFireAt.After(now) || !sc.NextFireAt.Equal(tc.nextSlot) {
					t.Fatalf("第 %d 次 tick 后 next_fire_at: got %v, want %v（且 > now）", i+1, sc.NextFireAt, tc.nextSlot)
				}
			}
			drain(t, r, 0)
			if got := f.lin.creates.Load(); got != 1 {
				t.Errorf("issueCreate 次数: got %d, want 1", got)
			}
		})
	}
}

func TestTickAfterManualRunTakesSlotStillAdvances(t *testing.T) {
	st := testStore(t)
	f := newFx(t, st, "manual-slot")
	sid := daily0900(f)
	guardResidue(t, st, at(0, 9, 0, 50), sid)
	c := newClock(at(0, 9, 0, 10))
	r := newRunner(t, st, linearFor(nil, f), c, "n1")

	run, err := r.FireNow(context.Background(), sid, f.uid)
	if err != nil {
		t.Fatalf("FireNow: %v", err)
	}
	if !run.ScheduledFor.Equal(at(0, 9, 0, 0)) || run.Trigger != store.TriggerManual {
		t.Fatalf("手动 run: got slot=%v trigger=%s, want 09:00/manual", run.ScheduledFor, run.Trigger)
	}

	c.Set(at(0, 9, 0, 20))
	res := tick(t, r)
	if !claimedSchedule(res, sid) {
		t.Fatalf("09:00:20 的 tick 应认领规则（随后 INSERT 冲突），got %+v", res.Claimed)
	}
	drain(t, r, 0)

	if got := f.lin.creates.Load(); got != 1 {
		t.Errorf("① issueCreate: got %d, want 1（只有手动那次）", got)
	}
	runs := f.runs(sid)
	if len(runs) != 1 || runs[0].Trigger != store.TriggerManual {
		t.Errorf("② runs: got %+v, want 恰 1 行 manual", runs)
	}
	sc := f.sched(sid)
	if sc.NextFireAt == nil || !sc.NextFireAt.Equal(at(1, 9, 0, 0)) || sc.LastSlotAt == nil || !sc.LastSlotAt.Equal(at(0, 9, 0, 0)) {
		t.Errorf("③ 冲突也前移: got next=%v last=%v, want next=%v last=%v", sc.NextFireAt, sc.LastSlotAt, at(1, 9, 0, 0), at(0, 9, 0, 0))
	}

	c.Set(at(0, 9, 0, 50))
	res = tick(t, r)
	drain(t, r, 0)
	if claimedSchedule(res, sid) {
		t.Errorf("④ 09:00:50 不应再认领 %d（不空转）", sid)
	}
	if got := f.lin.creates.Load(); got != 1 || len(f.runs(sid)) != 1 {
		t.Errorf("④ 计数不变: got creates=%d runs=%d, want 1/1", got, len(f.runs(sid)))
	}
}

// ================================================================ AC-7

func TestTickConcurrentRunnersFireOnce(t *testing.T) {
	st := testStore(t)
	f := newFx(t, st, "concurrent")
	sid := daily0900(f)
	now := at(0, 9, 0, 30)
	guardResidue(t, st, now, sid)

	ra := newRunner(t, st, linearFor(nil, f), newClock(now), "node-a")
	rb := newRunner(t, st, linearFor(nil, f), newClock(now), "node-b")
	var wg sync.WaitGroup
	for _, r := range []*Runner{ra, rb} {
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(r *Runner) {
				defer wg.Done()
				if _, err := r.TickOnce(context.Background()); err != nil {
					t.Errorf("TickOnce: %v", err)
				}
			}(r)
		}
	}
	wg.Wait()
	drain(t, ra, 0)
	drain(t, rb, 0)

	if got := f.lin.creates.Load(); got != 1 {
		t.Errorf("issueCreate: got %d, want 1", got)
	}
	if runs := f.runs(sid); len(runs) != 1 {
		t.Errorf("runs: got %d 行, want 1", len(runs))
	}
}

// ================================================================ AC-7b 租约

func TestLeaseConstants(t *testing.T) {
	if LeaseTTL < 6*LeaseHeartbeat {
		t.Errorf("LeaseTTL=%v 应 ≥ 6·LeaseHeartbeat=%v", LeaseTTL, 6*LeaseHeartbeat)
	}
	if FenceMargin <= 30*time.Second {
		t.Errorf("FenceMargin=%v 应 > linear 客户端超时 30s", FenceMargin)
	}
	if LeaseTTL-FenceMargin <= LeaseHeartbeat {
		t.Errorf("LeaseTTL−FenceMargin=%v 应 > LeaseHeartbeat=%v", LeaseTTL-FenceMargin, LeaseHeartbeat)
	}
	cases := []struct {
		name           string
		ttl, hb, fence time.Duration
		wantErr        bool
	}{
		{"production_ok", LeaseTTL, LeaseHeartbeat, FenceMargin, false},
		{"ttl_too_short_for_heartbeats", time.Minute, 20 * time.Second, 45 * time.Second, true},
		{"fence_not_above_client_timeout", 2 * time.Minute, 20 * time.Second, 30 * time.Second, true},
		{"fence_leaves_no_heartbeat", 2 * time.Minute, 20 * time.Second, 110 * time.Second, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkLeaseConstants(tc.ttl, tc.hb, tc.fence, linearClientTimeout)
			if (err != nil) != tc.wantErr {
				t.Errorf("got %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestLeaseConcurrentRecoverOnce(t *testing.T) {
	st := testStore(t)
	f := newFx(t, st, "recover-once")
	sid := f.schedule(schedOpts{minute: 540, next: at(3650, 9, 0, 0)})
	run := f.pendingRun(sid, at(0, 9, 0, 0), store.TriggerSchedule, 1, true, time.Minute)
	guardResidue(t, st, at(0, 9, 0, 30), sid)

	linA, linB := newFakeLinear(t), newFakeLinear(t)
	ra := newRunner(t, st, linearFor(map[int64]*fakeLinear{f.uid: linA}, f), newClock(at(0, 9, 0, 30)), "node-a")
	rb := newRunner(t, st, linearFor(map[int64]*fakeLinear{f.uid: linB}, f), newClock(at(0, 9, 0, 30)), "node-b")

	var mu sync.Mutex
	recovered := 0
	var wg sync.WaitGroup
	for _, r := range []*Runner{ra, rb} {
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(r *Runner) {
				defer wg.Done()
				res, err := r.TickOnce(context.Background())
				if err != nil {
					t.Errorf("TickOnce: %v", err)
					return
				}
				if recoveredRun(res, run.ID) {
					mu.Lock()
					recovered++
					mu.Unlock()
				}
			}(r)
		}
	}
	wg.Wait()
	drain(t, ra, 0)
	drain(t, rb, 0)

	got := f.run(run.ID)
	if recovered != 1 || got.Attempts != 2 {
		t.Errorf("回收: got recovered=%d attempts=%d, want 1/2", recovered, got.Attempts)
	}
	if ln := strp(got.LeaseNode); ln != "node-a" && ln != "node-b" {
		t.Errorf("lease_node: got %s, want node-a|node-b", ln)
	}
	if n := linA.lookups.Load() + linB.lookups.Load(); n != 1 {
		t.Errorf("IssueByID: got %d, want 1", n)
	}
	creates := linA.creates.Load() + linB.creates.Load()
	if replayStale() {
		if creates != 1 || got.Status != store.RunCreated {
			t.Errorf("replay: got creates=%d status=%s, want 1/created", creates, got.Status)
		}
	} else if creates != 0 || got.Status != store.RunFailed || strp(got.Reason) != store.ReasonInterrupted {
		t.Errorf("至多一次: got creates=%d status=%s reason=%s, want 0/failed/interrupted", creates, got.Status, strp(got.Reason))
	}
	if sc := f.sched(sid); sc.ConsecutiveFailures != 0 || !sc.Enabled {
		t.Errorf("规则: got cf=%d enabled=%v, want 0/true（interrupted 不计入）", sc.ConsecutiveFailures, sc.Enabled)
	}
}

// blockCreate 让第 n 次（n<=0 表示每次）issueCreate 阻塞 d 后成功。
func blockCreate(d time.Duration, which int64) func(w http.ResponseWriter, r *http.Request, in map[string]any, n int64) bool {
	return func(w http.ResponseWriter, r *http.Request, in map[string]any, n int64) bool {
		if which <= 0 || n == which {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return true
			}
		}
		return false
	}
}

func TestLeaseInProgressRunNotTakenOver(t *testing.T) {
	st := testStore(t)
	f := newFx(t, st, "inprogress")
	sid := daily0900(f)
	now := at(0, 9, 0, 30)
	guardResidue(t, st, now, sid)

	linA, linB := newFakeLinear(t), newFakeLinear(t)
	ra := newRunner(t, st, linearFor(map[int64]*fakeLinear{f.uid: linA}, f), newClock(now), "node-a")
	rb := newRunner(t, st, linearFor(map[int64]*fakeLinear{f.uid: linB}, f), newClock(now), "node-b")
	shortLeases(ra)
	shortLeases(rb)
	block := 3 * ra.leaseTTL
	linA.createFn = blockCreate(block, 0)

	res := tick(t, ra)
	var runID int64
	for _, c := range res.Claimed {
		if c.ScheduleID == sid {
			runID = c.RunID
		}
	}
	if runID == 0 {
		t.Fatalf("A 应认领并插入 run，got %+v", res.Claimed)
	}
	deadline := time.Now().Add(block)
	for time.Now().Before(deadline) {
		rr := tick(t, rb)
		if recoveredRun(rr, runID) {
			t.Fatalf("B 接手了 A 正在执行的 run %d", runID)
		}
		if ln := strp(f.run(runID).LeaseNode); ln != "node-a" {
			t.Fatalf("lease_node: got %s, want node-a", ln)
		}
		time.Sleep(200 * time.Millisecond)
	}
	drain(t, ra, block)
	drain(t, rb, 0)
	if linB.lookups.Load() != 0 || linB.creates.Load() != 0 {
		t.Errorf("B 侧: got lookups=%d creates=%d, want 0/0", linB.lookups.Load(), linB.creates.Load())
	}
	got := f.run(runID)
	if got.Status != store.RunCreated || got.Attempts != 1 {
		t.Errorf("终态: got %s attempts=%d, want created/1", got.Status, got.Attempts)
	}
	if sc := f.sched(sid); sc.ConsecutiveFailures != 0 {
		t.Errorf("cf: got %d, want 0", sc.ConsecutiveFailures)
	}
}

func TestLeaseQueuedRunNotTakenOver(t *testing.T) {
	st := testStore(t)
	f := newFx(t, st, "queued")
	s1 := f.schedule(schedOpts{minute: 540, next: at(0, 9, 0, 0), lastSlot: at(-1, 9, 0, 0)})
	s2 := f.schedule(schedOpts{minute: 541, next: at(0, 9, 1, 0), lastSlot: at(-1, 9, 1, 0)})
	now := at(0, 9, 1, 10)
	guardResidue(t, st, now, s1, s2)

	linA, linB := newFakeLinear(t), newFakeLinear(t)
	ra := newRunner(t, st, linearFor(map[int64]*fakeLinear{f.uid: linA}, f), newClock(now), "node-a")
	rb := newRunner(t, st, linearFor(map[int64]*fakeLinear{f.uid: linB}, f), newClock(now), "node-b")
	shortLeases(ra)
	shortLeases(rb)
	ra.concurrency = 1
	block := 3 * ra.leaseTTL
	linA.createFn = blockCreate(block, 1)

	res := tick(t, ra)
	var queued int64
	for _, c := range res.Claimed {
		if c.ScheduleID == s2 {
			queued = c.RunID
		}
	}
	if queued == 0 {
		t.Fatalf("A 应认领 s2，got %+v", res.Claimed)
	}
	deadline := time.Now().Add(block)
	for time.Now().Before(deadline) {
		if rr := tick(t, rb); recoveredRun(rr, queued) {
			t.Fatalf("B 接手了 A 队列里的 run %d", queued)
		}
		if ln := strp(f.run(queued).LeaseNode); ln != "node-a" {
			t.Fatalf("排队 run 的 lease_node: got %s, want node-a", ln)
		}
		time.Sleep(200 * time.Millisecond)
	}
	drain(t, ra, block)
	drain(t, rb, 0)
	if linB.lookups.Load() != 0 || linB.creates.Load() != 0 {
		t.Errorf("B 侧: got lookups=%d creates=%d, want 0/0", linB.lookups.Load(), linB.creates.Load())
	}
	for _, sid := range []int64{s1, s2} {
		runs := f.runs(sid)
		if len(runs) != 1 || runs[0].Status != store.RunCreated || runs[0].Attempts != 1 {
			t.Errorf("规则 %d: got %+v, want 1 行 created/attempts=1", sid, runs)
		}
	}
}

func TestLeaseRecoverNeverSentRunsFresh(t *testing.T) {
	cases := []struct {
		name        string
		sent        bool
		wantLookups int64
		wantCreates int64
		wantStatus  string
	}{
		{"never_sent_runs_fresh", false, 0, 1, store.RunCreated},
		{"sent_goes_lookup_first", true, 1, -1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			f := newFx(t, st, "fresh")
			sid := f.schedule(schedOpts{minute: 540, next: at(3650, 9, 0, 0)})
			run := f.pendingRun(sid, at(0, 9, 0, 0), store.TriggerSchedule, 1, tc.sent, time.Minute)
			guardResidue(t, st, at(0, 9, 0, 30), sid)
			r := newRunner(t, st, linearFor(nil, f), newClock(at(0, 9, 0, 30)), "n1")
			if res := tick(t, r); !recoveredRun(res, run.ID) {
				t.Fatalf("应回收 run %d", run.ID)
			}
			drain(t, r, 0)
			if got := f.lin.lookups.Load(); got != tc.wantLookups {
				t.Errorf("IssueByID: got %d, want %d", got, tc.wantLookups)
			}
			if tc.wantCreates >= 0 {
				if got := f.lin.creates.Load(); got != tc.wantCreates {
					t.Errorf("issueCreate: got %d, want %d", got, tc.wantCreates)
				}
				if got := f.run(run.ID); got.Status != tc.wantStatus {
					t.Errorf("终态: got %s/%s, want %s", got.Status, strp(got.Reason), tc.wantStatus)
				}
			}
		})
	}
}

// pausableRenewer 是可暂停的心跳 Store 包装：暂停期间续约为空操作并返回成功。
type pausableRenewer struct {
	inner  leaseRenewer
	mu     sync.Mutex
	paused bool
}

func (p *pausableRenewer) RenewLeases(ctx context.Context, tokens []string, ttl time.Duration) (int64, error) {
	p.mu.Lock()
	paused := p.paused
	p.mu.Unlock()
	if paused {
		return 0, nil
	}
	return p.inner.RenewLeases(ctx, tokens, ttl)
}

func TestLeaseLostWriterCannotOverwrite(t *testing.T) {
	for _, order := range []string{"taker_first", "loser_first"} {
		t.Run(order, func(t *testing.T) {
			st := testStore(t)
			f := newFx(t, st, "lostwriter")
			sid := daily0900(f)
			now := at(0, 9, 0, 30)
			guardResidue(t, st, now, sid)

			linA, linB := newFakeLinear(t), newFakeLinear(t)
			releaseA, releaseB := make(chan struct{}), make(chan struct{})
			aStarted, bStarted := make(chan struct{}, 1), make(chan struct{}, 1)
			linA.createFn = func(w http.ResponseWriter, r *http.Request, _ map[string]any, _ int64) bool {
				aStarted <- struct{}{}
				<-releaseA
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("A 的请求失败"))
				return true
			}
			linB.issueFn = func(w http.ResponseWriter, r *http.Request, id string) bool {
				bStarted <- struct{}{}
				<-releaseB
				writeFound(w, id)
				return true
			}

			logs := &logCapture{}
			ra := newRunner(t, st, linearFor(map[int64]*fakeLinear{f.uid: linA}, f), newClock(now), "node-a")
			rb := newRunner(t, st, linearFor(map[int64]*fakeLinear{f.uid: linB}, f), newClock(now), "node-b")
			shortLeases(ra)
			shortLeases(rb)
			pause := &pausableRenewer{inner: st}
			ra.renewer = pause
			ra.log = newTestLogger(logs)

			res := tick(t, ra)
			var runID int64
			for _, c := range res.Claimed {
				if c.ScheduleID == sid {
					runID = c.RunID
				}
			}
			<-aStarted
			pause.mu.Lock()
			pause.paused = true
			pause.mu.Unlock()
			if _, err := st.Pool().Exec(context.Background(), `UPDATE schedule_runs SET lease_until = now() - interval '1 second' WHERE id=$1`, runID); err != nil {
				t.Fatal(err)
			}
			if rr := tick(t, rb); !recoveredRun(rr, runID) {
				t.Fatalf("B 应回收 run %d", runID)
			}
			<-bStarted

			if order == "taker_first" {
				close(releaseB)
				waitFor(t, 5*time.Second, "B 写完终态", func() bool { return f.run(runID).Status != store.RunPending })
				close(releaseA)
				drain(t, ra, 0)
			} else {
				close(releaseA)
				drain(t, ra, 0)
				close(releaseB)
			}
			drain(t, rb, 0)

			got := f.run(runID)
			if got.Status != store.RunCreated || strp(got.LeaseNode) != "node-b" {
				t.Errorf("终态: got %s lease_node=%s, want created/node-b（B 写的）", got.Status, strp(got.LeaseNode))
			}
			sc := f.sched(sid)
			if sc.ConsecutiveFailures != 0 || sc.LastError != nil {
				t.Errorf("规则计数只反映 B: got cf=%d last_error=%s, want 0/<nil>", sc.ConsecutiveFailures, strp(sc.LastError))
			}
			if !logs.has("租约") {
				t.Errorf("A 侧应留下丢租约的 Warn/Error，got %v", logs.recs)
			}
		})
	}
}

// failingRenewer 的续约永远失败（模拟与库失联）。
type failingRenewer struct{}

func (failingRenewer) RenewLeases(context.Context, []string, time.Duration) (int64, error) {
	return 0, fmt.Errorf("模拟：数据库连不上")
}

func TestLeaseFencesOnHeartbeatFailure(t *testing.T) {
	st := testStore(t)
	f := newFx(t, st, "fence")
	sid := daily0900(f)
	now := at(0, 9, 0, 30)
	guardResidue(t, st, now, sid)

	cancelled := make(chan time.Time, 1)
	f.lin.createFn = func(w http.ResponseWriter, r *http.Request, _ map[string]any, _ int64) bool {
		<-r.Context().Done()
		cancelled <- time.Now()
		return true
	}
	r := newRunner(t, st, linearFor(nil, f), newClock(now), "node-a")
	shortLeases(r)
	r.renewer = failingRenewer{}

	start := time.Now()
	res := tick(t, r)
	var runID int64
	for _, c := range res.Claimed {
		if c.ScheduleID == sid {
			runID = c.RunID
		}
	}
	var cancelAt time.Time
	select {
	case cancelAt = <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("心跳持续失败时 A 应围栏并取消在途请求")
	}
	want := r.leaseTTL - r.fenceMargin
	if d := cancelAt.Sub(start); d < want-500*time.Millisecond || d > want+500*time.Millisecond {
		t.Errorf("围栏时机: got %v, want %v ± 500ms", d, want)
	}
	var stillValid bool
	if err := st.Pool().QueryRow(context.Background(), `SELECT lease_until > now() FROM schedule_runs WHERE id=$1`, runID).Scan(&stillValid); err != nil {
		t.Fatal(err)
	}
	if !stillValid {
		t.Error("围栏必须先于租约过期：此刻 lease_until 应仍在未来")
	}
	drain(t, r, 0)
	time.Sleep(300 * time.Millisecond)
	if got := f.run(runID); got.Status != store.RunPending {
		t.Errorf("围栏后 A 不得写终态: got %s", got.Status)
	}
}

// ================================================================ AC-7c

func TestTickSlowStageBDoesNotDelayOtherSchedules(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window time.Duration
	}{{"window_0", 0}, {"window_6h", 6 * time.Hour}} {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			f1 := newFx(t, st, "slow-s1")
			f2 := newFx(t, st, "slow-s2")
			s1 := f1.schedule(schedOpts{minute: 540, next: at(0, 9, 0, 0), lastSlot: at(-1, 9, 0, 0)})
			s2 := f2.schedule(schedOpts{minute: 541, next: at(0, 9, 1, 0), lastSlot: at(-1, 9, 1, 0)})
			guardResidue(t, st, at(0, 9, 1, 0), s1, s2)

			release := make(chan struct{})
			s1Started := make(chan struct{}, 1)
			f1.lin.createFn = func(w http.ResponseWriter, r *http.Request, in map[string]any, _ int64) bool {
				s1Started <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
				}
				return false
			}
			c := newClock(at(0, 9, 0, 0))
			r := newRunner(t, st, linearFor(nil, f1, f2), c, "n1")
			r.Tick = 300 * time.Millisecond
			r.CatchUpWindow = tc.window

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { r.Run(ctx); close(done) }()

			select {
			case <-s1Started:
			case <-time.After(5 * time.Second):
				cancel()
				t.Fatal("S1 应在第一个 tick 被认领并进入阻塞")
			}
			c.Set(at(0, 9, 1, 0))
			waitFor(t, 5*time.Second, "S2 在 S1 阻塞期间建单", func() bool {
				rs := f2.runs(s2)
				return len(rs) == 1 && rs[0].Status == store.RunCreated
			})
			rs := f2.runs(s2)
			if rs[0].Trigger != store.TriggerSchedule || rs[0].MissedCount != 0 {
				t.Errorf("S2: got trigger=%s missed=%d, want schedule/0", rs[0].Trigger, rs[0].MissedCount)
			}
			if got := f2.lin.creates.Load(); got != 1 {
				t.Errorf("S2 issueCreate: got %d, want 1", got)
			}
			if r1 := f1.runs(s1); len(r1) != 1 || r1[0].Status != store.RunPending {
				t.Errorf("断言时 S1 应仍阻塞（pending），got %+v", r1)
			}

			close(release)
			drain(t, r, 0)
			cancel()
			<-done
			if r1 := f1.runs(s1); len(r1) != 1 || r1[0].Status != store.RunCreated || r1[0].Trigger != store.TriggerSchedule {
				t.Errorf("S1: got %+v, want created/schedule", r1)
			}
		})
	}
}

// ================================================================ AC-10

func TestTickNeverCreatesTasks(t *testing.T) {
	st := testStore(t)
	f := newFx(t, st, "notasks")
	created := daily0900(f)
	failed := f.schedule(schedOpts{minute: 540, team: "fail-team", next: at(0, 9, 0, 0), lastSlot: at(-1, 9, 0, 0)})
	catchup := f.schedule(schedOpts{minute: 480, next: at(-2, 8, 0, 0), lastSlot: at(-3, 8, 0, 0)})
	f.lin.createFn = func(w http.ResponseWriter, r *http.Request, in map[string]any, _ int64) bool {
		if in["teamId"] == "fail-team" {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	}
	now := at(0, 9, 0, 30)
	guardResidue(t, st, now, created, failed, catchup)
	r := newRunner(t, st, linearFor(nil, f), newClock(now), "n1")
	tick(t, r)
	drain(t, r, 0)

	for sid, want := range map[int64][2]string{
		created: {store.RunCreated, store.TriggerSchedule},
		failed:  {store.RunFailed, store.TriggerSchedule},
		catchup: {store.RunCreated, store.TriggerCatchup},
	} {
		rs := f.runs(sid)
		if len(rs) != 1 || rs[0].Status != want[0] || rs[0].Trigger != want[1] {
			t.Errorf("规则 %d: got %+v, want %s/%s", sid, rs, want[0], want[1])
		}
	}
	var n, wt int
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT count(*), count(worktree_path) FROM tasks WHERE user_id=$1`, f.uid).Scan(&n, &wt); err != nil {
		t.Fatal(err)
	}
	if n != 0 || wt != 0 {
		t.Errorf("调度器绝不建任务: got tasks=%d worktree=%d, want 0/0", n, wt)
	}
}

// ================================================================ AC-12 补偿

func TestTickCatchUpCoalescesMissedSlots(t *testing.T) {
	st := testStore(t)
	f := newFx(t, st, "coalesce")
	sid := f.schedule(schedOpts{minute: 540, next: at(-2, 9, 0, 0), lastSlot: at(-3, 9, 0, 0)})
	now := at(0, 10, 0, 0)
	guardResidue(t, st, now, sid)
	r := newRunner(t, st, linearFor(nil, f), newClock(now), "n1")
	tick(t, r)
	drain(t, r, 0)

	if got := f.lin.creates.Load(); got != 1 {
		t.Errorf("issueCreate: got %d, want 1", got)
	}
	rs := f.runs(sid)
	if len(rs) != 1 || rs[0].Trigger != store.TriggerCatchup || rs[0].MissedCount != 2 || !rs[0].ScheduledFor.Equal(at(0, 9, 0, 0)) {
		t.Errorf("got %+v, want 1 行 catchup/missed=2/今天 09:00", rs)
	}
}

func TestTickCatchUpBeyondWindowSkips(t *testing.T) {
	st := testStore(t)
	f := newFx(t, st, "beyond")
	sid := f.schedule(schedOpts{minute: 540, next: at(-2, 9, 0, 0), lastSlot: at(-3, 9, 0, 0)})
	now := at(0, 16, 0, 0)
	guardResidue(t, st, now, sid)
	r := newRunner(t, st, linearFor(nil, f), newClock(now), "n1")
	tick(t, r)
	drain(t, r, 0)

	if got := f.lin.creates.Load(); got != 0 {
		t.Errorf("issueCreate: got %d, want 0", got)
	}
	rs := f.runs(sid)
	if len(rs) != 1 || rs[0].Status != store.RunSkipped || strp(rs[0].Reason) != store.ReasonMissedWindow {
		t.Errorf("got %+v, want 1 行 skipped/missed_window", rs)
	}
	if sc := f.sched(sid); !sc.NextFireAt.Equal(at(1, 9, 0, 0)) {
		t.Errorf("next_fire_at: got %v, want %v", sc.NextFireAt, at(1, 9, 0, 0))
	}
}

func TestTickCatchUpWindowZero(t *testing.T) {
	cases := []struct {
		name        string
		now         time.Time
		wantCreates int64
		wantStatus  string
		wantTrigger string
	}{
		{"on_time_fires", at(0, 9, 0, 30), 1, store.RunCreated, store.TriggerSchedule},
		{"grace_boundary_fires", at(0, 9, 1, 0), 1, store.RunCreated, store.TriggerSchedule},
		{"late_skips", at(0, 9, 5, 0), 0, store.RunSkipped, store.TriggerSchedule},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			f := newFx(t, st, "window0")
			sid := daily0900(f)
			guardResidue(t, st, tc.now, sid)
			r := newRunner(t, st, linearFor(nil, f), newClock(tc.now), "n1")
			r.CatchUpWindow = 0
			tick(t, r)
			drain(t, r, 0)
			if got := f.lin.creates.Load(); got != tc.wantCreates {
				t.Errorf("issueCreate: got %d, want %d", got, tc.wantCreates)
			}
			rs := f.runs(sid)
			if len(rs) != 1 || rs[0].Status != tc.wantStatus || rs[0].Trigger != tc.wantTrigger {
				t.Fatalf("got %+v, want 1 行 %s/%s", rs, tc.wantStatus, tc.wantTrigger)
			}
			if tc.wantStatus == store.RunSkipped && strp(rs[0].Reason) != store.ReasonMissedWindow {
				t.Errorf("reason: got %s, want missed_window", strp(rs[0].Reason))
			}
		})
	}
}

func TestTickOnTimeIgnoresWindow(t *testing.T) {
	var got [][2]string
	for _, w := range []time.Duration{6 * time.Hour, 0} {
		st := testStore(t)
		f := newFx(t, st, "ontime")
		sid := daily0900(f)
		now := at(0, 9, 0, 30)
		guardResidue(t, st, now, sid)
		r := newRunner(t, st, linearFor(nil, f), newClock(now), "n1")
		r.CatchUpWindow = w
		tick(t, r)
		drain(t, r, 0)
		rs := f.runs(sid)
		if len(rs) != 1 {
			t.Fatalf("窗口 %v: got %d 行, want 1", w, len(rs))
		}
		got = append(got, [2]string{rs[0].Status, rs[0].Trigger})
	}
	if got[0] != got[1] || got[0] != [2]string{store.RunCreated, store.TriggerSchedule} {
		t.Errorf("窗口不参与准点判定: got 6h=%v 0=%v, want 都是 created/schedule", got[0], got[1])
	}
}

func TestTickReenableDoesNotCatchUp(t *testing.T) {
	st := testStore(t)
	f := newFx(t, st, "reenable")
	// 停用了三天
	sid := f.schedule(schedOpts{minute: 540, disabled: true, lastSlot: at(-3, 9, 0, 0)})
	now := at(0, 10, 0, 0)
	spec := Spec{Kind: KindDaily, MinuteOfDay: 540, Loc: time.UTC}
	sc := f.sched(sid)
	next := spec.Next(now)
	minute := 540
	if _, err := st.UpdateSchedule(context.Background(), sid, f.uid, store.ScheduleParams{
		Name: sc.Name, Enabled: true, Kind: KindDaily, MinuteOfDay: &minute, Timezone: "UTC",
		LinearTeamID: "team-1", IntakeMode: "assign", TitleTemplate: sc.TitleTemplate, NextFireAt: &next, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	guardResidue(t, st, at(1, 9, 0, 30), sid)
	c := newClock(now)
	r := newRunner(t, st, linearFor(nil, f), c, "n1")
	tick(t, r)
	drain(t, r, 0)
	if got := f.lin.creates.Load(); got != 0 || len(f.runs(sid)) != 0 {
		t.Fatalf("重新启用不补: got creates=%d runs=%d, want 0/0", got, len(f.runs(sid)))
	}
	c.Set(at(1, 9, 0, 30))
	tick(t, r)
	drain(t, r, 0)
	rs := f.runs(sid)
	if len(rs) != 1 || rs[0].Trigger != store.TriggerSchedule || rs[0].MissedCount != 0 {
		t.Errorf("次日准点: got %+v, want 1 行 schedule/missed=0", rs)
	}
}

func TestTickCatchUpConflictStillAdvances(t *testing.T) {
	st := testStore(t)
	f := newFx(t, st, "catchup-conflict")
	sid := f.schedule(schedOpts{minute: 540, next: at(-2, 9, 0, 0), lastSlot: at(-3, 9, 0, 0)})
	if _, err := st.Pool().Exec(context.Background(), `
		INSERT INTO schedule_runs (schedule_id, user_id, scheduled_for, trigger, status, linear_issue_uuid)
		VALUES ($1,$2,$3,'manual','created',$4)`, sid, f.uid, at(0, 9, 0, 0), newUUID()); err != nil {
		t.Fatal(err)
	}
	now := at(0, 10, 0, 0)
	guardResidue(t, st, now, sid)
	r := newRunner(t, st, linearFor(nil, f), newClock(now), "n1")
	tick(t, r)
	drain(t, r, 0)
	if got := f.lin.creates.Load(); got != 0 {
		t.Errorf("issueCreate: got %d, want 0", got)
	}
	sc := f.sched(sid)
	if !sc.NextFireAt.Equal(at(1, 9, 0, 0)) || !sc.LastSlotAt.Equal(at(0, 9, 0, 0)) {
		t.Errorf("冲突也前移: got next=%v last=%v, want %v / %v", sc.NextFireAt, sc.LastSlotAt, at(1, 9, 0, 0), at(0, 9, 0, 0))
	}
}

// ================================================================ AC-13 失败可见

func fail500(body string) func(w http.ResponseWriter, r *http.Request, in map[string]any, n int64) bool {
	return func(w http.ResponseWriter, r *http.Request, in map[string]any, n int64) bool {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(body))
		return true
	}
}

func TestTickLinearFailureVisible(t *testing.T) {
	st := testStore(t)
	f := newFx(t, st, "fail")
	sid := daily0900(f)
	f.lin.createFn = fail500(strings.Repeat("服务端炸了", 300))
	now := at(0, 9, 0, 30)
	guardResidue(t, st, now, sid)
	r := newRunner(t, st, linearFor(nil, f), newClock(now), "n1")
	tick(t, r)
	drain(t, r, 0)

	rs := f.runs(sid)
	if len(rs) != 1 || rs[0].Status != store.RunFailed || rs[0].Error == nil || *rs[0].Error == "" || !utf8.ValidString(*rs[0].Error) {
		t.Fatalf("got %+v, want 1 行 failed 且 error 非空、合法 UTF-8", rs)
	}
	sc := f.sched(sid)
	if sc.ConsecutiveFailures != 1 || sc.LastError == nil || *sc.LastError == "" {
		t.Errorf("规则: got cf=%d last_error=%s, want 1/非空", sc.ConsecutiveFailures, strp(sc.LastError))
	}
}

func TestTickAutoPausesAfterConsecutiveFailures(t *testing.T) {
	st := testStore(t)
	f := newFx(t, st, "autopause")
	sid := daily0900(f)
	f.lin.createFn = fail500("boom")
	c := newClock(at(0, 9, 0, 30))
	r := newRunner(t, st, linearFor(nil, f), c, "n1")
	for i := 0; i < MaxConsecutiveFailures; i++ {
		c.Set(at(i, 9, 0, 30))
		guardResidue(t, st, c.Now(), sid)
		tick(t, r)
		drain(t, r, 0)
	}
	sc := f.sched(sid)
	if sc.Enabled || sc.NextFireAt != nil || sc.PausedReason == nil || !strings.Contains(*sc.PausedReason, "自动暂停") {
		t.Errorf("got enabled=%v next=%v paused=%s, want false/<nil>/含「自动暂停」", sc.Enabled, sc.NextFireAt, strp(sc.PausedReason))
	}
	if got := f.lin.creates.Load(); got != MaxConsecutiveFailures {
		t.Errorf("issueCreate: got %d, want %d", got, MaxConsecutiveFailures)
	}
}

func TestTickErrorRedactsToken(t *testing.T) {
	otherKey := "lin_api_" + strings.Repeat("Ab3", 10) + "Zz"
	cases := []struct {
		name     string
		token    string
		echo     func(f *fx) string
		credsErr bool
		mustNot  func(f *fx) string
		must     string
	}{
		{"exact_layer", "tok-NOT-lin-shape-" + newUUID(), func(f *fx) string { return "bad token " + f.token }, false,
			func(f *fx) string { return f.token }, "[REDACTED]"},
		{"shape_layer", "", func(*fx) string { return "other key " + otherKey }, false,
			func(*fx) string { return otherKey }, "lin_api_[REDACTED]"},
		{"creds_unavailable", "", nil, true, nil, "Linear 凭据不可用"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			f := newFx(t, st, "redact")
			if tc.token != "" {
				f.token = tc.token
			}
			sid := daily0900(f)
			if tc.echo != nil {
				f.lin.createFn = fail500(tc.echo(f))
			}
			lf := linearFor(nil, f)
			if tc.credsErr {
				lf = func(context.Context, int64) (LinearHandle, error) {
					return LinearHandle{}, fmt.Errorf("store: 凭据未配置")
				}
			}
			now := at(0, 9, 0, 30)
			guardResidue(t, st, now, sid)
			r := newRunner(t, st, lf, newClock(now), "n1")
			tick(t, r)
			drain(t, r, 0)

			rs := f.runs(sid)
			if len(rs) != 1 || rs[0].Status != store.RunFailed {
				t.Fatalf("got %+v, want 1 行 failed", rs)
			}
			runErr, lastErr := strp(rs[0].Error), strp(f.sched(sid).LastError)
			if tc.credsErr {
				if !strings.HasPrefix(runErr, tc.must) {
					t.Errorf("error: got %q, want 以 %q 开头", runErr, tc.must)
				}
				return
			}
			for name, v := range map[string]string{"schedule_runs.error": runErr, "schedules.last_error": lastErr} {
				if strings.Contains(v, tc.mustNot(f)) || !strings.Contains(v, tc.must) {
					t.Errorf("%s: got %q, want 不含密钥且含 %q", name, v, tc.must)
				}
			}
		})
	}
}

func TestTickSkipsDisabledOwner(t *testing.T) {
	type counts struct{ linearFor, lookups, creates, labels int64 }
	zero := counts{}
	read := func(f *fx) counts {
		return counts{f.linearForCalls.Load(), f.lin.lookups.Load(), f.lin.creates.Load(), f.lin.labels.Load()}
	}

	t.Run("at_claim", func(t *testing.T) {
		st := testStore(t)
		f := newFx(t, st, "disabled-claim")
		sid := daily0900(f)
		f.disableOwner()
		now := at(0, 9, 0, 30)
		guardResidue(t, st, now, sid)
		r := newRunner(t, st, linearFor(nil, f), newClock(now), "n1")
		tick(t, r)
		drain(t, r, 0)
		rs := f.runs(sid)
		if len(rs) != 1 || rs[0].Status != store.RunSkipped || strp(rs[0].Reason) != store.ReasonOwnerDisabled {
			t.Errorf("got %+v, want skipped/owner_disabled", rs)
		}
		if c := read(f); c != zero {
			t.Errorf("调用次数: got %+v, want 全 0", c)
		}
		if f.sched(sid).ConsecutiveFailures != 0 {
			t.Error("cf 应不变")
		}
	})

	t.Run("disabled_while_queued", func(t *testing.T) {
		st := testStore(t)
		f := newFx(t, st, "disabled-queued")
		sid := daily0900(f)
		now := at(0, 9, 0, 30)
		guardResidue(t, st, now, sid)
		r := newRunner(t, st, linearFor(nil, f), newClock(now), "n1")
		r.hold = make(chan struct{})
		tick(t, r)
		if rs := f.runs(sid); len(rs) != 1 || rs[0].Status != store.RunPending {
			t.Fatalf("执行前应是 pending，got %+v", rs)
		}
		f.disableOwner()
		close(r.hold)
		drain(t, r, 0)
		rs := f.runs(sid)
		if rs[0].Status != store.RunSkipped || strp(rs[0].Reason) != store.ReasonOwnerDisabled {
			t.Errorf("got %s/%s, want skipped/owner_disabled", rs[0].Status, strp(rs[0].Reason))
		}
		if c := read(f); c != zero {
			t.Errorf("调用次数: got %+v, want 全 0", c)
		}
	})

	for _, replay := range []bool{false, true} {
		t.Run(fmt.Sprintf("recover_lease_expired_owner_disabled/replay=%v", replay), func(t *testing.T) {
			st := testStore(t)
			f := newFx(t, st, "disabled-recover")
			sid := f.schedule(schedOpts{minute: 540, next: at(3650, 9, 0, 0)})
			run := f.pendingRun(sid, at(0, 9, 0, 0), store.TriggerSchedule, 1, true, time.Minute)
			f.disableOwner()
			guardResidue(t, st, at(0, 9, 0, 30), sid)
			r := newRunner(t, st, linearFor(nil, f), newClock(at(0, 9, 0, 30)), "n1")
			r.replay = &replay
			tick(t, r)
			drain(t, r, 0)
			got := f.run(run.ID)
			if got.Status != store.RunFailed || strp(got.Reason) != store.ReasonOwnerDisabled || !strings.Contains(strp(got.Error), "结果未知") {
				t.Errorf("got %s/%s/%s, want failed/owner_disabled/含「结果未知」", got.Status, strp(got.Reason), strp(got.Error))
			}
			if c := read(f); c != zero {
				t.Errorf("调用次数: got %+v, want 全 0", c)
			}
			if f.sched(sid).ConsecutiveFailures != 0 {
				t.Error("cf 应不变")
			}
		})
	}

	t.Run("recover_lease_expired_schedule_disabled", func(t *testing.T) {
		st := testStore(t)
		f := newFx(t, st, "disabled-sched")
		sid := f.schedule(schedOpts{minute: 540, disabled: true})
		run := f.pendingRun(sid, at(0, 9, 0, 0), store.TriggerSchedule, 1, true, time.Minute)

		// 对照组：同样停用的规则，但 run 是人点的「立即运行」→ 不看规则开关，走正常查单
		ctl := newFx(t, st, "disabled-sched-manual")
		csid := ctl.schedule(schedOpts{minute: 540, disabled: true})
		crun := ctl.pendingRun(csid, at(0, 9, 0, 0), store.TriggerManual, 1, true, time.Minute)
		ctl.lin.issueFn = func(w http.ResponseWriter, r *http.Request, id string) bool { writeFound(w, id); return true }

		guardResidue(t, st, at(0, 9, 0, 30), sid, csid)
		r := newRunner(t, st, linearFor(nil, f, ctl), newClock(at(0, 9, 0, 30)), "n1")
		tick(t, r)
		drain(t, r, 0)
		got := f.run(run.ID)
		if got.Status != store.RunFailed || strp(got.Reason) != store.ReasonScheduleDisabled {
			t.Errorf("got %s/%s, want failed/schedule_disabled", got.Status, strp(got.Reason))
		}
		if c := read(f); c != zero {
			t.Errorf("调用次数: got %+v, want 全 0", c)
		}
		if cg := ctl.run(crun.ID); cg.Status != store.RunCreated || ctl.lin.lookups.Load() != 1 {
			t.Errorf("manual 对照: got %s lookups=%d, want created/1", cg.Status, ctl.lin.lookups.Load())
		}
		if f.sched(sid).ConsecutiveFailures != 0 {
			t.Error("cf 应不变")
		}
	})
}

func TestTickRecoversStalePending(t *testing.T) {
	setup := func(t *testing.T, tag string, attempts int) (*store.Store, *fx, int64, *store.ScheduleRunRow) {
		st := testStore(t)
		f := newFx(t, st, tag)
		sid := f.schedule(schedOpts{minute: 540, next: at(3650, 9, 0, 0)})
		run := f.pendingRun(sid, at(0, 9, 0, 0), store.TriggerSchedule, attempts, true, time.Minute)
		guardResidue(t, st, at(0, 9, 0, 30), sid)
		return st, f, sid, run
	}

	t.Run("issue_exists", func(t *testing.T) {
		for _, replay := range []bool{false, true} {
			st, f, _, run := setup(t, "stale-exists", 1)
			f.lin.issueFn = func(w http.ResponseWriter, r *http.Request, id string) bool { writeFound(w, id); return true }
			r := newRunner(t, st, linearFor(nil, f), newClock(at(0, 9, 0, 30)), "n1")
			r.replay = &replay
			tick(t, r)
			drain(t, r, 0)
			if got := f.run(run.ID); got.Status != store.RunCreated || f.lin.creates.Load() != 0 || strp(got.LinearIssueIdentifier) != "ENG-FOUND" {
				t.Errorf("replay=%v: got %s creates=%d id=%s, want created/0/ENG-FOUND", replay, got.Status, f.lin.creates.Load(), strp(got.LinearIssueIdentifier))
			}
		}
	})

	t.Run("recover_lookup_error", func(t *testing.T) {
		for _, kind := range []string{"http_500", "ratelimited"} {
			for _, replay := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/replay=%v", kind, replay), func(t *testing.T) {
					st, f, sid, run := setup(t, "stale-lookup", 1)
					f.lin.issueFn = func(w http.ResponseWriter, r *http.Request, id string) bool {
						if kind == "http_500" {
							w.WriteHeader(http.StatusInternalServerError)
							_, _ = w.Write([]byte("boom " + f.token))
						} else {
							_, _ = fmt.Fprintf(w, `{"data":{"issue":null},"errors":[{"message":"rate limited for %s","extensions":{"code":"RATELIMITED"}}]}`, f.token)
						}
						return true
					}
					r := newRunner(t, st, linearFor(nil, f), newClock(at(0, 9, 0, 30)), "n1")
					r.replay = &replay
					tick(t, r)
					drain(t, r, 0)
					got := f.run(run.ID)
					e := strp(got.Error)
					if f.lin.creates.Load() != 0 || got.Status != store.RunFailed || strp(got.Reason) != store.ReasonInterrupted {
						t.Errorf("got creates=%d %s/%s, want 0/failed/interrupted", f.lin.creates.Load(), got.Status, strp(got.Reason))
					}
					if !strings.Contains(e, "按 UUID 查单失败") || strings.Contains(e, f.token) || !strings.Contains(e, "[REDACTED]") {
						t.Errorf("error: got %q, want 含「按 UUID 查单失败」与 [REDACTED]、不含 token", e)
					}
					if f.sched(sid).ConsecutiveFailures != 0 {
						t.Error("cf 应不变")
					}
				})
			}
		}
	})

	t.Run("issue_missing/replay", func(t *testing.T) {
		if !ReplayStalePending {
			t.Skipf("ReplayStalePending=%v，本支不适用（R1 未取证，走至多一次退路）", ReplayStalePending)
		}
		st, f, _, run := setup(t, "stale-replay", 1)
		r := newRunner(t, st, linearFor(nil, f), newClock(at(0, 9, 0, 30)), "n1")
		tick(t, r)
		drain(t, r, 0)
		got := f.run(run.ID)
		if f.lin.creates.Load() != 1 || f.lin.lastInput()["id"] != run.LinearIssueUUID || got.Status != store.RunCreated {
			t.Errorf("got creates=%d status=%s, want 1/created 且 input.id=uuid", f.lin.creates.Load(), got.Status)
		}
		st2, f2, _, run2 := setup(t, "stale-replay-cap", 3)
		r2 := newRunner(t, st2, linearFor(nil, f2), newClock(at(0, 9, 0, 30)), "n1")
		tick(t, r2)
		drain(t, r2, 0)
		if g := f2.run(run2.ID); f2.lin.creates.Load() != 0 || g.Status != store.RunFailed || strp(g.Reason) != store.ReasonInterrupted {
			t.Errorf("重放上限: got creates=%d %s/%s, want 0/failed/interrupted", f2.lin.creates.Load(), g.Status, strp(g.Reason))
		}
	})

	t.Run("issue_missing/at_most_once", func(t *testing.T) {
		if ReplayStalePending {
			t.Skipf("ReplayStalePending=%v，本支不适用（已取证可重放）", ReplayStalePending)
		}
		st, f, _, run := setup(t, "stale-amo", 1)
		r := newRunner(t, st, linearFor(nil, f), newClock(at(0, 9, 0, 30)), "n1")
		tick(t, r)
		drain(t, r, 0)
		got := f.run(run.ID)
		if f.lin.creates.Load() != 0 || got.Status != store.RunFailed || strp(got.Reason) != store.ReasonInterrupted || !strings.Contains(strp(got.Error), "结果未知，请到 Linear 确认") {
			t.Errorf("got creates=%d %s/%s/%q, want 0/failed/interrupted/含「结果未知，请到 Linear 确认」", f.lin.creates.Load(), got.Status, strp(got.Reason), strp(got.Error))
		}
	})
}
