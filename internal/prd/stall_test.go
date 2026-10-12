package prd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// 搁置扫描器的测试（docs/10 §9 第 1 条）。全部用假件 + 假时钟，不连库、
// 不 sleep —— 判定是天级的，等不起；而「等不起的东西就一定会被测成
// 近似」，所以时间必须注入。

// fakeStallStore 是 StallStore 的内存实现。
//
// 它忠实模拟了真 store 的两条关键语义，因为整个扫描器的正确性都压在上面：
//   - ListStalledPRDs 只返回「awaiting_answers 且最后一轮早于 cutoff」的行
//     （超期判定）；
//   - TransitionPRD 带 From 守卫（CAS）：状态不匹配就返回 ErrPRDStateChanged。
//
// 第二条尤其不能简化成「无条件改状态」：扫描器的幂等性完全靠它
// （转成功才发信，见 stall() 的注释）。假件若把那道守卫去掉，
// 幂等测试就会假绿。
type fakeStallStore struct {
	rows []store.PRDRow

	// lastRoundAt 是每份 PRD 最后一轮对话的时刻 —— 真 store 在 SQL 里
	// 用 prd_rounds 的最大 round 取它（见 ListStalledPRDs），假件在这里
	// 单独存：超期判据是「最后一轮什么时候发生」，而不是 PRD 行上的
	// 任何时间戳，所以它不属于 PRDRow。
	lastRoundAt map[int64]time.Time

	// candidates 是【本轮查询会返回什么】—— 一份可能已经过期的快照。
	//
	// 它与 state 刻意分开，这是这个假件最要紧的一处设计：真实现里
	// 「查询返回候选」与「拿快照去做 CAS」之间隔着一次数据库往返，
	// 人在那个窗口里答一句，候选就过期了。若两者是同一份数据，
	// 就构造不出这个竞态，CAS 失败那条分支永远测不到 —— 而它恰恰是
	// 幂等与「不误发信」的唯一防线（见 stall() 的注释）。
	//
	// nil 表示「按 state 现算」，便于不关心竞态的测试省一步。
	candidates []store.PRDRow

	transitions []string // 记录每次真的落下去的转移 "from->to"
	state       map[int64]string

	// lastCutoff 记录最近一次查询用的 cutoff，供断言「阈值确实传下去了」。
	lastCutoff time.Time
}

// stalledPRD 是一份「停在 awaiting_answers」的候选，带它的最后一轮时刻。
//
// 刻意不是 store.PRDRow：最后一轮什么时候发生是 prd_rounds 的信息，
// 真实现靠 SQL 的 LATERAL 从那张表取（见 ListStalledPRDs），PRDRow 上
// 没有、也不该有这个字段。夹具用一个平行的 case 表示同一件事。
type stalledPRD struct {
	row store.PRDRow
	// at 是最后一轮对话的时刻（问题提出、球踢给人的那一刻）。
	at time.Time
}

// stalled 造一份「最后一轮发生在 ago 之前」的候选。
//
// **row.UpdatedAt 刻意设成「刚刚」**：这是最关键的一处夹具设计。真库里的
// updated_at 会被任何写入推后（人在详情页点几下「确认」就会动它），
// 而超时时钟必须走轮次时间而不是它。夹具把两者拉开到极端对立 ——
// 最后一轮在 8 天前、行却是刚更新的 —— 若实现误用 updated_at，
// 超期测试会全红。
func stalled(id int64, ago time.Duration, now time.Time) stalledPRD {
	return stalledPRD{
		row: store.PRDRow{
			ID: id, UserID: id * 10, RepoID: 3,
			PRDType: "feature", State: string(StateAwaitingAnswers), Round: 3,
			OriginalInput: "预览阈值写死了，想让管理员能配",
			CreatedAt:     now.Add(-ago - time.Hour),
			UpdatedAt:     now,
		},
		at: now.Add(-ago),
	}
}

// newSweeperWith 造一个扫描器 + 假件，并登记每份候选的最后一轮时刻。
func newSweeperWith(now time.Time, cands ...stalledPRD) (*StallSweeper, *fakeStallStore, *fakeStallNotifier) {
	rows := make([]store.PRDRow, 0, len(cands))
	st := &fakeStallStore{state: map[int64]string{}, lastRoundAt: map[int64]time.Time{}}
	for _, c := range cands {
		rows = append(rows, c.row)
		st.state[c.row.ID] = c.row.State
		st.lastRoundAt[c.row.ID] = c.at
	}
	st.rows = rows

	n := &fakeStallNotifier{}
	return &StallSweeper{
		Store:    st,
		Notifier: n,
		Notify:   true,
		Timeout:  168 * time.Hour,
		BaseURL:  "https://lathe.example.com",
		Now:      func() time.Time { return now },
	}, st, n
}

func (f *fakeStallStore) ListStalledPRDs(_ context.Context, _ time.Time, cutoff time.Time, _ int) ([]store.PRDRow, error) {
	f.lastCutoff = cutoff
	// 显式设了 candidates 就原样返回（哪怕它已经过期）：这就是竞态测试
	// 要的形状 —— 快照在查询那一刻是对的，之后人答了。
	if f.candidates != nil {
		return f.candidates, nil
	}
	out := []store.PRDRow{}
	for _, r := range f.rows {
		// 状态与超期两项都由真 store 判；这里按 lastRoundAt 模拟超期。
		if f.state[r.ID] != string(StateAwaitingAnswers) {
			continue
		}
		if !f.lastRoundAt[r.ID].Before(cutoff) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeStallStore) TransitionPRD(_ context.Context, id, _ int64, p store.TransitionPRDParams) (*store.PRDRow, error) {
	if f.state[id] != p.From {
		return nil, store.ErrPRDStateChanged
	}
	f.state[id] = p.To
	f.transitions = append(f.transitions, p.From+"->"+p.To)
	for i := range f.rows {
		if f.rows[i].ID == id {
			cp := f.rows[i]
			cp.State = p.To
			return &cp, nil
		}
	}
	return nil, store.ErrPRDNotFound
}

// fakeStallNotifier 记录投出去的信，供幂等断言。
type fakeStallNotifier struct {
	sent []struct {
		PRDID         int64
		Subject, Body string
	}
	err error
}

func (f *fakeStallNotifier) NotifyPRDStalled(_ context.Context, prdID int64, subject, body string) error {
	f.sent = append(f.sent, struct {
		PRDID         int64
		Subject, Body string
	}{prdID, subject, body})
	return f.err
}

// ★ 验收标准 2：最后一轮回答时间超过阈值的 PRD 被转 abandoned。
func TestSweepStallsOverduePRD(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	// 8 天前问的问题，阈值 7 天 —— 超了一整天。
	svc, st, _ := newSweeperWith(now, stalled(42, 8*24*time.Hour, now))

	got, err := svc.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if got != 1 {
		t.Errorf("应搁置 1 份，got=%d", got)
	}
	if st.state[42] != string(StateAbandoned) {
		t.Errorf("超期 PRD 的状态 got=%q want=%q", st.state[42], StateAbandoned)
	}
}

// ★ 验收标准 2 的另一半：未超期的不动。
func TestSweepLeavesFreshPRDAlone(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	// 6 天前问的问题，阈值 7 天 —— 还没到。
	svc, st, n := newSweeperWith(now, stalled(42, 6*24*time.Hour, now))

	got, err := svc.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if got != 0 {
		t.Errorf("未超期不该被搁置，got=%d", got)
	}
	if st.state[42] != string(StateAwaitingAnswers) {
		t.Errorf("未超期的 PRD 状态不该变，got=%q", st.state[42])
	}
	if len(n.sent) != 0 {
		t.Errorf("未超期不该发信，got=%d 封", len(n.sent))
	}
}

// 边界：阈值是严格的「早于 cutoff」，正好落在阈值上不算超期。
//
// 断言方向是为了把边界的归属钉死 —— 差一秒的两侧必须各归一边，
// 不能有既算又不算的模糊地带。取「晚一刻才搁置」而不是「到点即搁置」：
// SQL 里是 `created_at < cutoff`（严格小于），搁置这个动作宁可晚一步，
// 也不要在人正好卡着阈值回答的瞬间把它判成搁置。
func TestSweepBoundaryExactlyAtTimeout(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	timeout := 168 * time.Hour

	for _, tc := range []struct {
		name string
		ago  time.Duration
		want bool // 是否应被搁置
	}{
		{"正好等于阈值（严格小于，不搁置）", timeout, false},
		{"差一秒未到", timeout - time.Second, false},
		{"超一秒", timeout + time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, st, _ := newSweeperWith(now, stalled(42, tc.ago, now))
			if _, err := svc.SweepOnce(context.Background()); err != nil {
				t.Fatalf("扫描失败: %v", err)
			}
			got := st.state[42] == string(StateAbandoned)
			if got != tc.want {
				t.Errorf("间隔 %v：got 搁置=%v want=%v", tc.ago, got, tc.want)
			}
		})
	}
}

// ★ 验收标准 3：提醒只发一次（幂等）。
//
// 连扫三轮，且每轮都强行喂同一份**过期候选**（模拟「扫描器每轮都看到它
// 超期」的最坏情形）：第一轮转成功后状态已是 abandoned，后两轮的 CAS
// 必然失配。若把发信挪到 CAS 之前，后两轮会各多发一封 —— 这条测试
// 就是那处挪动的绊线。
func TestSweepNotifiesOnlyOnce(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	svc, st, n := newSweeperWith(now, stalled(42, 8*24*time.Hour, now))
	// 每轮查询都返回这份（第一轮之后它已经过期，正是要压的形状）。
	st.candidates = []store.PRDRow{st.rows[0]}

	for i := 0; i < 3; i++ {
		if _, err := svc.SweepOnce(context.Background()); err != nil {
			t.Fatalf("第 %d 轮扫描失败: %v", i+1, err)
		}
	}

	if len(n.sent) != 1 {
		t.Fatalf("搁置提醒应只发一次，got=%d 封 —— 发信必须排在 CAS 之后（CAS 就是幂等闸门）", len(n.sent))
	}
	if n.sent[0].PRDID != 42 {
		t.Errorf("收件对象 got=%d want=42", n.sent[0].PRDID)
	}
	// 只该有一次转移：后两轮 CAS 都该失配。
	if len(st.transitions) != 1 {
		t.Errorf("转移应只落一次，got=%v", st.transitions)
	}
}

// CAS 失配（人在这两轮之间答了）时，既不重复发信也不报错。
//
// 构造的是真竞态而不是「候选本身就过期」：查询那一刻这份 PRD 确实在
// awaiting_answers（所以它进了候选），之后、CAS 之前人答了一句，
// 状态回到 drafting。真实现里这两步隔着一次数据库往返，人插进来完全
// 正常 —— 扫描器拿的是过期快照，这一点不该被当成错误，更不该据此发信。
func TestSweepSkipsWhenStateChangedAfterQuery(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	svc, st, n := newSweeperWith(now, stalled(42, 8*24*time.Hour, now))

	// 本轮候选：查询时它还在等人回答。
	st.candidates = []store.PRDRow{st.rows[0]}
	// 人抢答了 —— CAS 会用 From=awaiting_answers 去比，而实际已是 drafting。
	st.state[42] = string(StateDrafting)

	got, err := svc.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("人抢答不该让扫描报错: %v", err)
	}
	if got != 0 {
		t.Errorf("状态已变不该算搁置，got=%d", got)
	}
	if len(n.sent) != 0 {
		t.Errorf("状态已变不该发信，got=%d 封 —— 信必须排在 CAS 之后", len(n.sent))
	}
	if len(st.transitions) != 0 {
		t.Errorf("CAS 应失配，不该有任何转移，got=%v", st.transitions)
	}
	if st.state[42] != string(StateDrafting) {
		t.Errorf("人抢答后的状态不该被扫描器改写，got=%q", st.state[42])
	}
}

// 发信失败不影响已经落好的状态转移（notify.go 第 2 条约束）。
func TestSweepMailFailureDoesNotUndoState(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	svc, st, n := newSweeperWith(now, stalled(42, 8*24*time.Hour, now))
	n.err = errors.New("SMTP 炸了")

	got, err := svc.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("发信失败不该让扫描报错: %v", err)
	}
	if got != 1 {
		t.Errorf("状态该转照转，got=%d", got)
	}
	if st.state[42] != string(StateAbandoned) {
		t.Errorf("发信失败不该回滚状态，got=%q", st.state[42])
	}
}

// 关掉通知时只转状态、不发信（LATHE_PRD_STALL_NOTIFY=false）。
func TestSweepNotifyDisabledStillStalls(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	svc, st, n := newSweeperWith(now, stalled(42, 8*24*time.Hour, now))
	svc.Notify = false

	got, err := svc.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if got != 1 {
		t.Errorf("关掉通知仍应判搁置，got=%d", got)
	}
	if len(n.sent) != 0 {
		t.Errorf("通知已关，不该发信，got=%d 封", len(n.sent))
	}
	// 关掉的只是那封信，判定本身照做 —— 否则部署方一关通知，
	// 整条超时能力就跟着静默失效了。
	if st.state[42] != string(StateAbandoned) {
		t.Errorf("通知关掉不影响状态转移，got=%q", st.state[42])
	}
}

// 阈值确实传下去了：cutoff 必须是 now - timeout。
func TestSweepPassesCutoffFromTimeout(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	svc, st, _ := newSweeperWith(now)
	svc.Timeout = 72 * time.Hour

	if _, err := svc.SweepOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := now.Add(-72 * time.Hour)
	if !st.lastCutoff.Equal(want) {
		t.Errorf("cutoff got=%v want=%v", st.lastCutoff, want)
	}
}

// 零值阈值取默认（168h）而不是「立刻超时」。
//
// Sweeper 可能被绕过 config 直接构造，零值必须有意义。
func TestSweeperZeroTimeoutFallsBackToDefault(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	svc, st, _ := newSweeperWith(now)
	svc.Timeout = 0 // 零值：必须解释成「用默认」而不是「立刻超时」

	if _, err := svc.SweepOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if svc.effectiveTimeout() != defaultStallTimeout {
		t.Errorf("零值阈值应取默认 %v，got=%v", defaultStallTimeout, svc.effectiveTimeout())
	}
	if want := now.Add(-defaultStallTimeout); !st.lastCutoff.Equal(want) {
		t.Errorf("cutoff got=%v want=%v（零值绝不能解释成立刻超时）", st.lastCutoff, want)
	}
}

// 邮件正文要带上「多久没答」与详情链接 —— 人靠这两样判断这事有多严重、
// 以及从哪儿回去。照 terminalMail 的做法脱开 SMTP 测正文。
func TestStalledPRDMailContent(t *testing.T) {
	subject, body := StalledPRDMail("https://lathe.example.com/", 42, 168*time.Hour)

	if !strings.Contains(subject, "#42") {
		t.Errorf("主题应带 PRD 编号，got=%q", subject)
	}
	for _, want := range []string{"1 周", "42", "https://lathe.example.com/prds/42"} {
		if !strings.Contains(body, want) {
			t.Errorf("正文应包含 %q，got:\n%s", want, body)
		}
	}
	// baseURL 为空时省略详情行（部署未配 LATHE_BASE_URL 的情形）。
	if _, b := StalledPRDMail("", 42, 168*time.Hour); strings.Contains(b, "/prds/") {
		t.Errorf("baseURL 为空时不该拼详情链接，got:\n%s", b)
	}
}

// 时长说人话：天级阈值不该渲染成 168h0m0s。
func TestHumanDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{time.Hour, "1 小时"},
		{47 * time.Hour, "47 小时"},
		{48 * time.Hour, "2 天"},
		{168 * time.Hour, "1 周"},
		{14 * 24 * time.Hour, "2 周"},
		{30 * 24 * time.Hour, "30 天"},
	}
	for _, tc := range tests {
		if got := humanDuration(tc.d); got != tc.want {
			t.Errorf("humanDuration(%v) got=%q want=%q", tc.d, got, tc.want)
		}
	}
}
