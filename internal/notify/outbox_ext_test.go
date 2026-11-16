package notify_test

// 外部测试包：import store 不成环（依赖方向是 store → notify）。
// 真 NC（*store.NotifyChannels + lathe_f09）+ 真 Worker + 真 Sender（TLS 假服务器）。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/notify"
	"github.com/zichuanwangcloud-gif/lathe/internal/secret"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/testsupport"
)

var extT0 = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

type extFix struct {
	st     *store.Store
	nc     *store.NotifyChannels
	srv    *httptest.Server
	sender *notify.Sender

	mu     sync.Mutex
	bodies []string
	delay  time.Duration
}

func newExtFix(t *testing.T, delay time.Duration, clientTimeout time.Duration) *extFix {
	t.Helper()
	ctx, cancel := testsupport.ConnectContext(t)
	defer cancel()
	st, err := store.Open(ctx, testsupport.DSN())
	if err != nil {
		testsupport.SkipOrFail(t, "打开 store 失败: %v", err)
	}
	t.Cleanup(st.Close)

	f := &extFix{st: st, delay: delay}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		f.mu.Lock()
		f.bodies = append(f.bodies, string(b))
		f.mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(f.srv.Close)

	key := make([]byte, secret.KeySize)
	_, _ = rand.Read(key)
	sealer, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}
	addr := f.srv.Listener.Addr().String()
	f.nc = st.NewNotifyChannels(sealer, []string{addr})
	c := notify.HardenClient(f.srv.Client())
	if clientTimeout > 0 {
		c.Timeout = clientTimeout
	}
	f.sender = notify.NewSender([]string{addr}, c)
	return f
}

func nonce(t *testing.T) string {
	t.Helper()
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return strconv.FormatInt(time.Now().UnixNano(), 36) + hex.EncodeToString(b)
}

// mkUserWithSlack 建用户 + 仓库 + 一条指向假服务器的 slack 渠道。
func (f *extFix) mkUserWithSlack(t *testing.T) (userID, repoID int64) {
	t.Helper()
	ctx := context.Background()
	pool := f.st.Pool()
	if err := pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"ntx-"+nonce(t)+"@example.com").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID) })
	if err := pool.QueryRow(ctx, `INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		userID, "acme/ntx-"+nonce(t)).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.nc.SaveChannel(ctx, userID, store.SaveChannelParams{
		Kind: notify.KindSlack, URL: f.srv.URL + "/hook/T000/B000/tok", Enabled: true,
		Events: notify.EventSwitches{Failed: true, BlockedSpec: true, AwaitingApproval: true, PROpen: true, PRDReady: true},
	}); err != nil {
		t.Fatal(err)
	}
	return userID, repoID
}

func (f *extFix) mkTask(t *testing.T, userID, repoID int64, key string) int64 {
	t.Helper()
	var id int64
	if err := f.st.Pool().QueryRow(context.Background(),
		`INSERT INTO tasks (user_id, repo_id, external_key) VALUES ($1,$2,$3) RETURNING id`,
		userID, repoID, key).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// pendingOf 数本用户还在 pending/sending 的投递。
func (f *extFix) pendingOf(t *testing.T, userID int64) int {
	t.Helper()
	var n int
	if err := f.st.Pool().QueryRow(context.Background(), `
		SELECT count(*) FROM notify_deliveries d JOIN notify_channels c ON c.id=d.channel_id
		WHERE c.user_id=$1 AND d.status IN ('pending','sending')`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *extFix) drainUntilDone(t *testing.T, w *notify.Worker, userID int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for f.pendingOf(t, userID) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("3s 内本用户的投递没有处理完（还剩 %d 行）", f.pendingOf(t, userID))
		}
		if err := w.DrainOnce(context.Background()); err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}
	}
}

func (f *extFix) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

// AC-3c：worker 只读 payload，tasks 行被改成哨兵值后正文仍是入队那一刻的快照。
func TestWorkerRendersFromPayloadNotTaskRow(t *testing.T) {
	f := newExtFix(t, 0, 0)
	ctx := context.Background()
	uid, repo := f.mkUserWithSlack(t)
	snapKey := "CR-SNAP-" + nonce(t)
	prTask := f.mkTask(t, uid, repo, snapKey+"-pr")
	failTask := f.mkTask(t, uid, repo, snapKey+"-fail")

	for _, ev := range []notify.TaskEvent{
		{TaskID: prTask, UserID: uid, ExternalKey: snapKey + "-pr", ToState: "pr_open", EventType: notify.EventPROpen,
			At: extT0, PRURL: "https://github.com/acme/demo/pull/7"},
		{TaskID: failTask, UserID: uid, ExternalKey: snapKey + "-fail", ToState: "failed", EventType: notify.EventFailed,
			At: extT0, Reason: "redacted-reason", FailureStage: "implement"},
	} {
		if n, err := f.nc.EnqueueTaskEvent(ctx, ev); err != nil || n != 1 {
			t.Fatalf("Enqueue got (%d, %v), want (1, nil)", n, err)
		}
	}
	if _, err := f.st.Pool().Exec(ctx, `UPDATE tasks SET failure_reason='RAW-UNREDACTED', external_key='CR-MUTATED-'||id,
		pr_url='https://evil.example/pr', state='queued' WHERE id = ANY($1)`, []int64{prTask, failTask}); err != nil {
		t.Fatal(err)
	}

	w := notify.NewWorker(f.nc, f.sender, "https://lathe.example.com")
	w.Now = func() time.Time { return extT0 }
	f.drainUntilDone(t, w, uid)

	got := f.received()
	if len(got) != 2 {
		t.Fatalf("假服务器请求数 got %d, want 2：%v", len(got), got)
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{"redacted-reason", snapKey, "/pull/7"} {
		if !strings.Contains(all, want) {
			t.Errorf("正文 got %s, want 含 %q", all, want)
		}
	}
	for _, bad := range []string{"RAW-UNREDACTED", "CR-MUTATED", "evil.example"} {
		if strings.Contains(all, bad) {
			t.Errorf("正文含 tasks 行哨兵 %q（worker 回读了 tasks）：%s", bad, all)
		}
	}
}

// AC-17c：逐行领取 —— 慢服务器 + 短租约 + 另一节点不停回收，每条仍只发一次。
func TestSlowBatchIsNotReclaimedMidRound(t *testing.T) {
	f := newExtFix(t, 150*time.Millisecond, 200*time.Millisecond)
	ctx := context.Background()
	uid, repo := f.mkUserWithSlack(t)
	base := "CR-SLOW-" + nonce(t)
	keys := map[string]bool{}
	for i := 0; i < 10; i++ {
		k := base + "-" + strconv.Itoa(i)
		keys[k] = true
		id := f.mkTask(t, uid, repo, k)
		if _, err := f.nc.EnqueueTaskEvent(ctx, notify.TaskEvent{TaskID: id, UserID: uid, ExternalKey: k,
			ToState: "failed", EventType: notify.EventFailed, At: extT0}); err != nil {
			t.Fatal(err)
		}
	}

	start := time.Now()
	clock := func() time.Time { return extT0.Add(time.Since(start)) }
	a := notify.NewWorker(f.nc, f.sender, "")
	b := notify.NewWorker(f.nc, f.sender, "")
	for _, w := range []*notify.Worker{a, b} {
		w.Now = clock
		w.Lease = time.Second
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				_ = b.DrainOnce(ctx)
			}
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for f.pendingOf(t, uid) > 0 && time.Now().Before(deadline) {
		_ = a.DrainOnce(ctx)
	}
	close(stop)
	wg.Wait()

	count := map[string]int{}
	for _, body := range f.received() {
		var m map[string]string
		_ = json.Unmarshal([]byte(body), &m)
		for k := range keys {
			// 正文形如 "[Lathe] <key> 处理失败"：按 key+空格匹配，避免 -1 命中 -10
			if strings.Contains(m["text"], k+" ") {
				count[k]++
			}
		}
	}
	for k := range keys {
		if count[k] != 1 {
			t.Errorf("%s 收到 %d 次, want 1", k, count[k])
		}
	}
	var sent int
	_ = f.st.Pool().QueryRow(ctx, `SELECT count(*) FROM notify_deliveries d JOIN notify_channels c ON c.id=d.channel_id
		WHERE c.user_id=$1 AND d.status='sent'`, uid).Scan(&sent)
	if sent != 10 {
		t.Errorf("sent 行数 got %d, want 10", sent)
	}
}

// 渠道在排队后被关掉：发送前复查，标 skipped，不发请求。
func TestWorkerSkipsDisabledChannel(t *testing.T) {
	f := newExtFix(t, 0, 0)
	ctx := context.Background()
	uid, repo := f.mkUserWithSlack(t)
	id := f.mkTask(t, uid, repo, "CR-SKIP-"+nonce(t))
	if _, err := f.nc.EnqueueTaskEvent(ctx, notify.TaskEvent{TaskID: id, UserID: uid, ExternalKey: "CR-SKIP",
		ToState: "failed", EventType: notify.EventFailed, At: extT0}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.nc.SaveChannel(ctx, uid, store.SaveChannelParams{Kind: notify.KindSlack, Enabled: false,
		Events: notify.EventSwitches{Failed: true}}); err != nil {
		t.Fatal(err)
	}
	w := notify.NewWorker(f.nc, f.sender, "")
	w.Now = func() time.Time { return extT0 }
	f.drainUntilDone(t, w, uid)
	if got := len(f.received()); got != 0 {
		t.Errorf("请求数 got %d, want 0", got)
	}
	var status string
	_ = f.st.Pool().QueryRow(ctx, `SELECT status FROM notify_deliveries WHERE target_task_id=$1`, id).Scan(&status)
	if status != "skipped" {
		t.Errorf("status got %q, want skipped", status)
	}
}
