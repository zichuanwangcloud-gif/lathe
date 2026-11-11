package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/linear"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/testsupport"
)

// 调度器测试的公共设施。
//
// 归属隔离（AGENTS.md §5.5）：阶段 A 的到期扫描与回收是全局查询，测试库里
// 可能有别的测试或被 kill 的上一轮留下的规则与 pending。所以：
//   - 每个 fixture 用户配一个自己的计数型假 Linear；测试的 LinearFor 只对本
//     测试创建的用户返回 handle，残留行被认领时只会记「凭据不可用」，永远打
//     不到本 fixture 的假 Linear —— 所有调用次数天然是「本 fixture 的」；
//   - 行数、状态断言一律按 schedule_id / run id 过滤；
//   - 本包测试不用 t.Parallel()：同包测试也共享这张全局扫描。

func testStore(t *testing.T) *store.Store {
	t.Helper()
	ctx, cancel := testsupport.ConnectContext(t)
	defer cancel()
	st, err := store.Open(ctx, testsupport.DSN())
	if err != nil {
		testsupport.SkipOrFail(t, "打开 store 失败: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// ---------------------------------------------------------------- 假 Linear

type fakeLinear struct {
	srv     *httptest.Server
	creates atomic.Int64
	lookups atomic.Int64
	labels  atomic.Int64

	mu     sync.Mutex
	inputs []map[string]any

	// createFn 返回 true 表示已自行写好响应；为 nil 或返回 false 时按默认成功。
	createFn func(w http.ResponseWriter, r *http.Request, input map[string]any, n int64) bool
	// issueFn 同上；默认返回「不存在」（白名单载荷）。
	issueFn func(w http.ResponseWriter, r *http.Request, id string) bool
}

func newFakeLinear(t *testing.T) *fakeLinear {
	t.Helper()
	f := &fakeLinear{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLinear) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var q struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	_ = json.Unmarshal(raw, &q)
	switch {
	case strings.Contains(q.Query, "issueCreate"):
		n := f.creates.Add(1)
		input, _ := q.Variables["input"].(map[string]any)
		f.mu.Lock()
		f.inputs = append(f.inputs, input)
		f.mu.Unlock()
		if f.createFn != nil && f.createFn(w, r, input, n) {
			return
		}
		writeCreated(w, fmt.Sprint(input["id"]), fmt.Sprintf("ENG-%d", n))
	case strings.Contains(q.Query, "issueLabels"):
		f.labels.Add(1)
		_, _ = io.WriteString(w, `{"data":{"issueLabels":{"nodes":[{"id":"label-1","name":"lathe:go","team":null}]}}}`)
	case strings.Contains(q.Query, "issue(id"):
		f.lookups.Add(1)
		id := fmt.Sprint(q.Variables["id"])
		if f.issueFn != nil && f.issueFn(w, r, id) {
			return
		}
		_, _ = io.WriteString(w, `{"data":{"issue":null},"errors":[{"message":"Entity not found: Issue","extensions":{"code":"ENTITY_NOT_FOUND"}}]}`)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func writeCreated(w http.ResponseWriter, id, identifier string) {
	_, _ = fmt.Fprintf(w, `{"data":{"issueCreate":{"success":true,"issue":{"id":%q,"identifier":%q,"url":"https://linear.app/x/issue/%s"}}}}`, id, identifier, identifier)
}

func writeFound(w http.ResponseWriter, id string) {
	_, _ = fmt.Fprintf(w, `{"data":{"issue":{"id":%q,"identifier":"ENG-FOUND","url":"https://linear.app/x/issue/ENG-FOUND"}}}`, id)
}

func (f *fakeLinear) lastInput() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inputs) == 0 {
		return nil
	}
	return f.inputs[len(f.inputs)-1]
}

// ---------------------------------------------------------------- fixture

type fx struct {
	t      *testing.T
	st     *store.Store
	uid    int64
	nonce  string
	token  string
	lin    *fakeLinear
	userID string // Linear 侧「指派给我」用的用户 ID

	linearForCalls atomic.Int64
}

func newFx(t *testing.T, st *store.Store, tag string) *fx {
	t.Helper()
	nonce := fmt.Sprint(time.Now().UnixNano())
	f := &fx{t: t, st: st, nonce: nonce, token: "tok-" + nonce, userID: "viewer-" + nonce, lin: newFakeLinear(t)}
	email := fmt.Sprintf("sched-%s-%s@example.com", tag, nonce)
	if err := st.Pool().QueryRow(context.Background(),
		`INSERT INTO users (email) VALUES ($1) RETURNING id`, email).Scan(&f.uid); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM users WHERE id=$1`, f.uid)
	})
	return f
}

type schedOpts struct {
	kind      string
	minute    int
	weekdays  int
	tz        string
	team      string
	intake    string
	disabled  bool
	next      time.Time
	lastSlot  time.Time
	enabledAt time.Time
	title     string
}

// schedule 直接写一条规则（绕过 API，以便控制 next_fire_at / last_slot_at / enabled_at）。
func (f *fx) schedule(o schedOpts) int64 {
	f.t.Helper()
	if o.kind == "" {
		o.kind = KindDaily
	}
	if o.tz == "" {
		o.tz = "UTC"
	}
	if o.team == "" {
		o.team = "team-1"
	}
	if o.intake == "" {
		o.intake = "assign"
	}
	if o.title == "" {
		o.title = "定时 {{date}}"
	}
	if o.enabledAt.IsZero() {
		o.enabledAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	var weekdays any
	if o.kind == KindWeekly {
		weekdays = o.weekdays
	}
	var next, last any
	if !o.disabled {
		next = o.next
	}
	if !o.lastSlot.IsZero() {
		last = o.lastSlot
	}
	var id int64
	if err := f.st.Pool().QueryRow(context.Background(), `
		INSERT INTO schedules (user_id, name, enabled, enabled_at, kind, minute_of_day, weekdays, timezone,
			linear_team_id, intake_mode, title_template, body_template, next_fire_at, last_slot_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'正文 {{schedule}}',$12,$13) RETURNING id`,
		f.uid, fmt.Sprintf("s-%s-%d", f.nonce, time.Now().UnixNano()), !o.disabled, o.enabledAt, o.kind, o.minute,
		weekdays, o.tz, o.team, o.intake, o.title, next, last).Scan(&id); err != nil {
		f.t.Fatalf("建规则失败: %v", err)
	}
	return id
}

// pendingRun 直接插一条 pending（模拟崩溃遗留）。leaseAgo>0 表示租约已过期这么久。
func (f *fx) pendingRun(scheduleID int64, slot time.Time, trigger string, attempts int, sent bool, leaseAgo time.Duration) *store.ScheduleRunRow {
	f.t.Helper()
	var sentAt any
	if sent {
		sentAt = time.Now().Add(-2 * time.Minute)
	}
	var id int64
	if err := f.st.Pool().QueryRow(context.Background(), `
		INSERT INTO schedule_runs (schedule_id, user_id, scheduled_for, trigger, status, linear_issue_uuid,
			attempts, node, lease_token, lease_node, lease_until, create_sent_at)
		VALUES ($1,$2,$3,$4,'pending',$5,$6,'dead-node',$7,'dead-node', now() - make_interval(secs => $8::float8), $9)
		RETURNING id`, scheduleID, f.uid, slot, trigger, newUUID(), attempts, newUUID(), leaseAgo.Seconds(), sentAt).Scan(&id); err != nil {
		f.t.Fatalf("插 pending 失败: %v", err)
	}
	return f.run(id)
}

func (f *fx) run(id int64) *store.ScheduleRunRow {
	f.t.Helper()
	r, err := f.st.GetScheduleRun(context.Background(), id)
	if err != nil {
		f.t.Fatalf("读 run %d 失败: %v", id, err)
	}
	return r
}

func (f *fx) runs(scheduleID int64) []store.ScheduleRunRow {
	f.t.Helper()
	rs, err := f.st.ListScheduleRuns(context.Background(), scheduleID, f.uid, 200)
	if err != nil {
		f.t.Fatalf("读 runs 失败: %v", err)
	}
	return rs
}

func (f *fx) sched(id int64) *store.ScheduleRow {
	f.t.Helper()
	s, err := f.st.GetSchedule(context.Background(), id, f.uid)
	if err != nil {
		f.t.Fatalf("读规则 %d 失败: %v", id, err)
	}
	return s
}

func (f *fx) disableOwner() {
	f.t.Helper()
	if _, err := f.st.Pool().Exec(context.Background(), `UPDATE users SET disabled_at = now() WHERE id=$1`, f.uid); err != nil {
		f.t.Fatal(err)
	}
}

// linearFor 只给本测试的 fixture 用户发 handle；其余 uid（库里的残留）一律报错。
// 每个 fixture 可以按 runner 换一台假 Linear（servers 里给出），用于分节点计数。
func linearFor(servers map[int64]*fakeLinear, fxs ...*fx) LinearFor {
	byUID := map[int64]*fx{}
	for _, f := range fxs {
		byUID[f.uid] = f
	}
	return func(ctx context.Context, uid int64) (LinearHandle, error) {
		f, ok := byUID[uid]
		if !ok {
			return LinearHandle{}, fmt.Errorf("不是本测试的用户 %d", uid)
		}
		f.linearForCalls.Add(1)
		lin := f.lin
		if s, ok := servers[uid]; ok {
			lin = s
		}
		c, err := linear.NewClientWithURL(f.token, lin.srv.URL)
		if err != nil {
			return LinearHandle{}, err
		}
		return LinearHandle{Client: c, UserID: f.userID, Redact: strings.NewReplacer(f.token, "[REDACTED]").Replace}, nil
	}
}

// ---------------------------------------------------------------- 时钟 / Runner

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *clock { return &clock{t: t} }
func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

type fixedSettings string

func (s fixedSettings) WebhookTriggerLabel(context.Context) string { return string(s) }

func newRunner(t *testing.T, st *store.Store, lf LinearFor, c *clock, node string) *Runner {
	t.Helper()
	r := &Runner{
		Store: st, Linear: lf, Settings: fixedSettings("lathe:go"), Now: c.Now,
		CatchUpWindow: 6 * time.Hour, Node: node,
	}
	t.Cleanup(r.Close)
	return r
}

// shortLeases 把租约常量缩到 3s/500ms/1.5s（只缩时间，不改约束关系）。
func shortLeases(r *Runner) {
	r.leaseTTL, r.heartbeat, r.fenceMargin = 3*time.Second, 500*time.Millisecond, 1500*time.Millisecond
}

func drain(t *testing.T, r *Runner, extra time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second+extra)
	defer cancel()
	if err := r.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
}

func tick(t *testing.T, r *Runner) TickResult {
	t.Helper()
	res, err := r.TickOnce(context.Background())
	if err != nil {
		t.Fatalf("TickOnce: %v", err)
	}
	return res
}

func claimedSchedule(res TickResult, sid int64) bool {
	for _, c := range res.Claimed {
		if c.ScheduleID == sid {
			return true
		}
	}
	return false
}

func recoveredRun(res TickResult, runID int64) bool {
	for _, c := range res.Recovered {
		if c.RunID == runID {
			return true
		}
	}
	return false
}

// guardResidue 残留护栏：非本 fixture 的到期规则或过期 pending 接近 ClaimLimit 时
// 直接失败，免得它们挤掉本 fixture 的认领名额、让失败原因被误读。
func guardResidue(t *testing.T, st *store.Store, now time.Time, mine ...int64) {
	t.Helper()
	ctx := context.Background()
	var due, stale int
	if err := st.Pool().QueryRow(ctx, `SELECT count(*) FROM schedules WHERE enabled AND next_fire_at <= $1 AND NOT (id = ANY($2))`,
		now, mine).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool().QueryRow(ctx, `SELECT count(*) FROM schedule_runs WHERE status='pending' AND lease_until < now() AND NOT (schedule_id = ANY($1))`,
		mine).Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if due >= ClaimLimit-10 || stale >= ClaimLimit-10 {
		t.Fatalf("测试库残留过多（到期规则 %d、过期 pending %d，上限 %d）：先 make clean-test-db 干跑查看", due, stale, ClaimLimit-10)
	}
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待超时（%v）：%s", d, what)
}

// logCapture 捕获 Runner 的日志，用来断言「丢租约者留下了 Warn/Error」。
type logCapture struct {
	mu   sync.Mutex
	recs []string
}

func (l *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (l *logCapture) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.Level >= slog.LevelWarn {
		l.recs = append(l.recs, r.Message)
	}
	return nil
}
func (l *logCapture) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logCapture) WithGroup(string) slog.Handler      { return l }
func (l *logCapture) has(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range l.recs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

func strp(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func newTestLogger(h slog.Handler) *slog.Logger { return slog.New(h) }

// replayStale 读包级常量（R1 取证结论的唯一承载点）。
func replayStale() bool { return ReplayStalePending }
