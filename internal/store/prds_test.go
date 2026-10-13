package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// 规划管线 store 层的契约（docs/10 §1 生命周期、D10-4 冻结）。
// 同 issues_test.go：测试库是共享开发库，fixture 一律带随机量。

func prdFixture(t *testing.T, st *Store) (userID, repoID int64) {
	t.Helper()
	ctx := context.Background()
	nonce := fmt.Sprint(time.Now().UnixNano())
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"prd-"+nonce+"@example.com").Scan(&userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		userID, "acme/prd-"+nonce).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	return userID, repoID
}

func newPRD(t *testing.T, st *Store, userID, repoID int64) *PRDRow {
	t.Helper()
	p, err := st.CreatePRD(context.Background(), CreatePRDParams{
		UserID: userID, RepoID: repoID, PRDType: "feature",
		OriginalInput: "预览阈值写死了，想让管理员能配",
	})
	if err != nil {
		t.Fatalf("建 PRD 失败: %v", err)
	}
	return p
}

func TestCreatePRDDefaults(t *testing.T) {
	st := testStore(t)
	userID, repoID := prdFixture(t, st)
	p := newPRD(t, st, userID, repoID)

	if p.State != "drafting" {
		t.Errorf("新 PRD 应从 drafting 起步，got=%q", p.State)
	}
	if p.Round != 0 {
		t.Errorf("新 PRD 轮次应为 0（还没跑过），got=%d", p.Round)
	}
	if p.Processing {
		t.Error("新 PRD 不该挂着 processing")
	}
	if p.OriginalInput == "" {
		t.Error("原文必须落库：它是所有推断的根")
	}
}

// 非属主一律「不存在」：不用 403 暴露资源存在性（AGENTS.md §7）。
func TestGetPRDHidesFromNonOwner(t *testing.T) {
	st := testStore(t)
	userID, repoID := prdFixture(t, st)
	otherID, _ := prdFixture(t, st)
	p := newPRD(t, st, userID, repoID)

	if _, err := st.GetPRD(context.Background(), p.ID, otherID); !errors.Is(err, ErrPRDNotFound) {
		t.Errorf("非属主读别人的 PRD 应得 ErrPRDNotFound，got=%v", err)
	}
}

// D10-4 的数据库兜底：approved 之后内容冻结。
func TestUpdatePRDContentRejectedAfterApproval(t *testing.T) {
	st := testStore(t)
	userID, repoID := prdFixture(t, st)
	ctx := context.Background()
	p := newPRD(t, st, userID, repoID)

	doc := json.RawMessage(`{"sections":{"one_liner":"让管理员配预览阈值"}}`)
	if _, err := st.UpdatePRDContent(ctx, p.ID, userID, UpdatePRDContentParams{Document: doc}); err != nil {
		t.Fatalf("drafting 阶段落快照应成功: %v", err)
	}

	for _, step := range [][2]string{
		{"drafting", "ready_for_review"},
		{"ready_for_review", "approved"},
	} {
		if _, err := st.TransitionPRD(ctx, p.ID, userID,
			TransitionPRDParams{From: step[0], To: step[1]}); err != nil {
			t.Fatalf("%s → %s 落库失败: %v", step[0], step[1], err)
		}
	}

	_, err := st.UpdatePRDContent(ctx, p.ID, userID,
		UpdatePRDContentParams{Document: json.RawMessage(`{"sections":{"one_liner":"偷改"}}`)})
	if !errors.Is(err, ErrPRDFrozen) {
		t.Errorf("approved 后写内容必须报 ErrPRDFrozen（D10-4），got=%v", err)
	}

	got, err := st.GetPRD(ctx, p.ID, userID)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Sections struct {
			OneLiner string `json:"one_liner"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(got.Document, &parsed); err != nil {
		t.Fatalf("解析快照失败: %v", err)
	}
	if parsed.Sections.OneLiner != "让管理员配预览阈值" {
		t.Errorf("冻结后内容仍被改动，got=%q", parsed.Sections.OneLiner)
	}
	if got.ApprovedAt == nil {
		t.Error("进 approved 必须留签字时刻")
	}
}

// CAS 比较项不匹配时整条不生效，报 ErrPRDStateChanged 而非静默成功。
func TestTransitionPRDIsCompareAndSwap(t *testing.T) {
	st := testStore(t)
	userID, repoID := prdFixture(t, st)
	ctx := context.Background()
	p := newPRD(t, st, userID, repoID)

	_, err := st.TransitionPRD(ctx, p.ID, userID,
		TransitionPRDParams{From: "ready_for_review", To: "approved"})
	if !errors.Is(err, ErrPRDStateChanged) {
		t.Errorf("From 与实际状态不符应报 ErrPRDStateChanged，got=%v", err)
	}

	got, err := st.GetPRD(ctx, p.ID, userID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "drafting" {
		t.Errorf("CAS 失败后状态不该变，got=%q", got.State)
	}
}

// 并发起轮只能有一个赢：否则两轮 agent 会互相覆盖同一份快照。
func TestBeginPRDRoundIsExclusive(t *testing.T) {
	st := testStore(t)
	userID, repoID := prdFixture(t, st)
	ctx := context.Background()
	p := newPRD(t, st, userID, repoID)

	if _, err := st.BeginPRDRound(ctx, p.ID, userID); err != nil {
		t.Fatalf("首次抢占应成功: %v", err)
	}
	if _, err := st.BeginPRDRound(ctx, p.ID, userID); !errors.Is(err, ErrPRDBusy) {
		t.Errorf("已有轮次在跑时应报 ErrPRDBusy，got=%v", err)
	}

	if err := st.EndPRDRound(ctx, p.ID, userID); err != nil {
		t.Fatalf("释放轮次失败: %v", err)
	}
	after, err := st.BeginPRDRound(ctx, p.ID, userID)
	if err != nil {
		t.Fatalf("释放后应能再抢: %v", err)
	}
	if !after.Processing || after.ProcessingStartedAt == nil {
		t.Error("抢占成功必须带上 processing 与起始时刻")
	}
}

// 僵死的轮次（进程被杀，没机会 CAS 回）在读路径上自动放行。
func TestGetPRDClearsStaleProcessing(t *testing.T) {
	st := testStore(t)
	userID, repoID := prdFixture(t, st)
	ctx := context.Background()
	p := newPRD(t, st, userID, repoID)

	if _, err := st.BeginPRDRound(ctx, p.ID, userID); err != nil {
		t.Fatal(err)
	}
	// 把起始时刻推到阈值之外，模拟被杀掉的轮次。
	if _, err := st.pool.Exec(ctx,
		`UPDATE prds SET processing_started_at = now() - $2::interval WHERE id = $1`,
		p.ID, fmt.Sprintf("%d minutes", int(prdStallAfter.Minutes())+1)); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetPRD(ctx, p.ID, userID)
	if err != nil {
		t.Fatalf("读 PRD 失败: %v", err)
	}
	if got.Processing {
		t.Error("超过僵死阈值的 processing 应在读时被放行，否则这份 PRD 永久锁死")
	}
	if _, err := st.BeginPRDRound(ctx, p.ID, userID); err != nil {
		t.Errorf("放行后应能重新起轮: %v", err)
	}
}

// 终态不许再起轮次：abandoned 的 PRD 不该还能烧 token。
func TestBeginPRDRoundRefusesTerminalState(t *testing.T) {
	st := testStore(t)
	userID, repoID := prdFixture(t, st)
	ctx := context.Background()
	p := newPRD(t, st, userID, repoID)

	if _, err := st.TransitionPRD(ctx, p.ID, userID,
		TransitionPRDParams{From: "drafting", To: "abandoned"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginPRDRound(ctx, p.ID, userID); err == nil {
		t.Error("abandoned 的 PRD 不该还能起对话轮次")
	}
}

func TestAppendPRDRoundAndList(t *testing.T) {
	st := testStore(t)
	userID, repoID := prdFixture(t, st)
	ctx := context.Background()
	p := newPRD(t, st, userID, repoID)

	sid := "sess-abc"
	if _, err := st.AppendPRDRound(ctx, AppendPRDRoundParams{
		PRDID: p.ID, Round: 1, Stage: "understand",
		AgentSessionID: &sid, UserInput: "",
		Questions: json.RawMessage(`[{"id":"Q-1","text":"阈值是内存还是磁盘？"}]`),
	}); err != nil {
		t.Fatalf("追加第 1 轮失败: %v", err)
	}
	if _, err := st.AppendPRDRound(ctx, AppendPRDRoundParams{
		PRDID: p.ID, Round: 2, Stage: "explore", UserInput: "两个都要",
	}); err != nil {
		t.Fatalf("追加第 2 轮失败: %v", err)
	}

	rounds, err := st.ListPRDRounds(ctx, p.ID)
	if err != nil {
		t.Fatalf("读对话历史失败: %v", err)
	}
	if len(rounds) != 2 {
		t.Fatalf("应有 2 轮，got=%d", len(rounds))
	}
	if rounds[0].Round != 1 || rounds[1].Round != 2 {
		t.Errorf("必须按轮次升序，got=%d,%d", rounds[0].Round, rounds[1].Round)
	}
	if rounds[0].AgentSessionID == nil || *rounds[0].AgentSessionID != sid {
		t.Error("session id 应原样留痕")
	}
	if rounds[1].UserInput != "两个都要" {
		t.Errorf("人的回答应落库，got=%q", rounds[1].UserInput)
	}

	// 同一轮次写两次是并发起轮的征兆，必须报错而非覆盖。
	if _, err := st.AppendPRDRound(ctx, AppendPRDRoundParams{
		PRDID: p.ID, Round: 2, Stage: "converge",
	}); err == nil {
		t.Error("重复写同一轮次应报唯一冲突，掩盖竞态比报错危险")
	}
}

// 列表页不回传三个大 jsonb：一页 50 行全带上够传几兆。
func TestListPRDsOmitsHeavyDocuments(t *testing.T) {
	st := testStore(t)
	userID, repoID := prdFixture(t, st)
	ctx := context.Background()
	p := newPRD(t, st, userID, repoID)

	if _, err := st.UpdatePRDContent(ctx, p.ID, userID, UpdatePRDContentParams{
		Document: json.RawMessage(`{"sections":{"one_liner":"x"}}`),
	}); err != nil {
		t.Fatal(err)
	}

	list, total, err := st.ListPRDs(ctx, ListPRDsParams{UserID: userID})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if total != 1 || len(list) != 1 {
		t.Fatalf("应恰好 1 份 PRD，total=%d len=%d", total, len(list))
	}
	if len(list[0].Document) != 0 {
		t.Error("列表不该回传 document：详情页才拿完整快照")
	}
	if list[0].OriginalInput == "" {
		t.Error("列表要能显示原文摘要")
	}

	// 状态过滤：按 approved 筛应查不到这份 drafting 的。
	filtered, n, err := st.ListPRDs(ctx, ListPRDsParams{UserID: userID, State: "approved"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(filtered) != 0 {
		t.Errorf("状态过滤失效：按 approved 筛出了 %d 份", n)
	}
}

// ---------------------------------------------------------------- 超期扫描

// awaitFor 把一份 PRD 摆成「停在 awaiting_answers、最后一轮在 ago 之前」。
//
// 最后一轮的时间（prd_rounds.created_at）刻意用 UPDATE 往回推而不是等 ——
// 搁置阈值是天级的，等不起。created_at 是 DEFAULT now() 但没有触发器守着它
// （只有 updated_at 有 set_updated_at），所以这一推是稳的。
func awaitFor(t *testing.T, st *Store, prdID int64, userID int64, ago time.Duration) {
	t.Helper()
	ctx := context.Background()

	// 先落一轮对话记录：超时时钟读的就是它。
	if _, err := st.AppendPRDRound(ctx, AppendPRDRoundParams{
		PRDID: prdID, Round: 1, Stage: "understand",
		Questions: json.RawMessage(`["阈值按内存还是磁盘算？"]`),
	}); err != nil {
		t.Fatalf("落轮次失败: %v", err)
	}
	// 轮次推进与快照写入在真实流程里是同一步（service.persistRound 同时
	// 做这两件事），fixture 照做，别留下生产环境不会出现的 prds.round=0。
	round := 1
	if _, err := st.UpdatePRDContent(ctx, prdID, userID,
		UpdatePRDContentParams{Round: &round}); err != nil {
		t.Fatalf("推进轮次失败: %v", err)
	}
	if _, err := st.TransitionPRD(ctx, prdID, userID,
		TransitionPRDParams{From: "drafting", To: "awaiting_answers"}); err != nil {
		t.Fatalf("转 awaiting_answers 失败: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE prd_rounds SET created_at = now() - $2::interval WHERE prd_id = $1`,
		prdID, fmt.Sprintf("%d seconds", int(ago.Seconds()))); err != nil {
		t.Fatalf("推前轮次时间失败: %v", err)
	}
}

// ★ 超时时钟必须取最后一轮的时间，而不是 prds.updated_at。
//
// 这条是 ListStalledPRDs 的命门，也是唯一必须在真库上验的判据 ——
// 扫描器那层的假件测不到 SQL。构造的正是最容易踩错的那个形状：
// 最后一轮在两周前，但 PRD 行刚刚被写过（人点了几下「确认」，
// 或扫描器自己上一轮的转移）。
//
// 用 updated_at 做时钟的话，这份永远答不完的 PRD 会因为「昨天有写入」
// 而永远不被判搁置 —— 静默失效，没有任何报错。
func TestListStalledPRDsUsesLastRoundTimeNotRowUpdate(t *testing.T) {
	st := testStore(t)
	userID, repoID := prdFixture(t, st)
	ctx := context.Background()

	// 两周没人答。
	stale := newPRD(t, st, userID, repoID)
	awaitFor(t, st, stale.ID, userID, 14*24*time.Hour)
	// 行本身「刚刚」被写过：updated_at 是新鲜的，轮次时间是老的。
	if _, err := st.UpdatePRDContent(ctx, stale.ID, userID, UpdatePRDContentParams{
		Document: json.RawMessage(`{"sections":{}}`),
	}); err != nil {
		t.Fatal(err)
	}

	// 一天前答完的那份不该被选中。
	fresh := newPRD(t, st, userID, repoID)
	awaitFor(t, st, fresh.ID, userID, 24*time.Hour)

	now := time.Now()
	rows, err := st.ListStalledPRDs(ctx, now, now.Add(-168*time.Hour), 100)
	if err != nil {
		t.Fatalf("查询超期 PRD 失败: %v", err)
	}

	ids := map[int64]bool{}
	for _, r := range rows {
		ids[r.ID] = true
	}
	if !ids[stale.ID] {
		t.Error("两周没人答的 PRD 应被判超期 —— 它是按最后一轮的时间算的，" +
			"即使这一行刚刚被写过（人点确认也会推 updated_at）")
	}
	if ids[fresh.ID] {
		t.Error("一天前还在对话的 PRD 不该被判超期")
	}
}

// 正在跑一轮的 PRD 不算搁置（agent 还在写文档）。
//
// 但**僵死**的轮次必须照样算进来：进程被杀时没机会把 processing CAS 回去，
// 若那种标记能让人免于扫描，这份 PRD 就会因为一个永远清不掉的标记而
// 永久留在这里 —— 正是这条能力要治的病。
func TestListStalledPRDsSkipsProcessingButNotStale(t *testing.T) {
	st := testStore(t)
	userID, repoID := prdFixture(t, st)
	ctx := context.Background()

	// (a) 正常跑着的一轮：processing=true，起始时刻就在刚才。
	running := newPRD(t, st, userID, repoID)
	awaitFor(t, st, running.ID, userID, 14*24*time.Hour)
	// awaitFor 之后状态已是 awaiting_answers，BeginPRDRound 能抢到。
	if _, err := st.BeginPRDRound(ctx, running.ID, userID); err != nil {
		t.Fatalf("抢占轮次失败: %v", err)
	}

	// (b) 僵死的轮次：processing=true，但起始时刻远在僵死阈值之外。
	dead := newPRD(t, st, userID, repoID)
	awaitFor(t, st, dead.ID, userID, 14*24*time.Hour)
	if _, err := st.BeginPRDRound(ctx, dead.ID, userID); err != nil {
		t.Fatalf("抢占轮次失败: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE prds SET processing_started_at = now() - $2::interval WHERE id = $1`,
		dead.ID, fmt.Sprintf("%d minutes", int(prdStallAfter.Minutes())+5)); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	rows, err := st.ListStalledPRDs(ctx, now, now.Add(-168*time.Hour), 100)
	if err != nil {
		t.Fatalf("查询超期 PRD 失败: %v", err)
	}

	ids := map[int64]bool{}
	for _, r := range rows {
		ids[r.ID] = true
	}
	if ids[running.ID] {
		t.Error("正在跑一轮的 PRD 不该被判搁置：那一轮跑完还要写快照")
	}
	if !ids[dead.ID] {
		t.Error("僵死的轮次必须照样算进候选 —— 否则一个清不掉的 processing " +
			"标记就能让这份 PRD 永远免于扫描")
	}
}

// 已经不在 awaiting_answers 的一律不进候选（CAS 之外的第二道闸）。
func TestListStalledPRDsOnlyAwaitingAnswers(t *testing.T) {
	st := testStore(t)
	userID, repoID := prdFixture(t, st)
	ctx := context.Background()

	p := newPRD(t, st, userID, repoID)
	// 落一轮但**不**转状态：停在 drafting。
	if _, err := st.AppendPRDRound(ctx, AppendPRDRoundParams{
		PRDID: p.ID, Round: 1, Stage: "understand",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx,
		`UPDATE prd_rounds SET created_at = now() - interval '14 days' WHERE prd_id = $1`,
		p.ID); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	rows, err := st.ListStalledPRDs(ctx, now, now.Add(-168*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == p.ID {
			t.Error("drafting 的 PRD 不该被判搁置：它没有挂着等人的问题")
		}
	}
}

// 搁置之后能唤醒回起草（docs/10 §9 第 1 条：允许复活）。
func TestAbandonedPRDCanResumeToDrafting(t *testing.T) {
	st := testStore(t)
	userID, repoID := prdFixture(t, st)
	ctx := context.Background()
	p := newPRD(t, st, userID, repoID)

	if _, err := st.TransitionPRD(ctx, p.ID, userID,
		TransitionPRDParams{From: "drafting", To: "abandoned"}); err != nil {
		t.Fatalf("放弃失败: %v", err)
	}

	// 唤醒。CAS 的 From 是 abandoned。
	got, err := st.TransitionPRD(ctx, p.ID, userID,
		TransitionPRDParams{From: "abandoned", To: "drafting"})
	if err != nil {
		t.Fatalf("唤醒搁置的 PRD 失败: %v", err)
	}
	if got.State != "drafting" {
		t.Errorf("唤醒后状态 got=%q want=drafting", got.State)
	}

	// 唤醒后能再起轮次：否则「回来接着说」只是一句空话。
	if _, err := st.BeginPRDRound(ctx, p.ID, userID); err != nil {
		t.Errorf("唤醒后应能再起对话轮次: %v", err)
	}
}

// awaitWithRounds 把一份 PRD 摆成「停在 awaiting_answers」，并按 at 逐轮
// 落对话记录、把每一轮的 created_at 钉成给定时刻（at[i] 是第 i+1 轮）。
func awaitWithRounds(t *testing.T, st *Store, prdID, userID int64, at ...time.Time) {
	t.Helper()
	ctx := context.Background()
	for i := range at {
		if _, err := st.AppendPRDRound(ctx, AppendPRDRoundParams{
			PRDID: prdID, Round: i + 1, Stage: "understand",
			Questions: json.RawMessage(`["阈值按内存还是磁盘算？"]`),
		}); err != nil {
			t.Fatalf("落第 %d 轮失败: %v", i+1, err)
		}
	}
	round := len(at)
	if _, err := st.UpdatePRDContent(ctx, prdID, userID,
		UpdatePRDContentParams{Round: &round}); err != nil {
		t.Fatalf("推进轮次失败: %v", err)
	}
	if _, err := st.TransitionPRD(ctx, prdID, userID,
		TransitionPRDParams{From: "drafting", To: "awaiting_answers"}); err != nil {
		t.Fatalf("转 awaiting_answers 失败: %v", err)
	}
	for i, ts := range at {
		if _, err := st.pool.Exec(ctx,
			`UPDATE prd_rounds SET created_at = $3 WHERE prd_id = $1 AND round = $2`,
			prdID, i+1, ts); err != nil {
			t.Fatalf("钉第 %d 轮时间失败: %v", i+1, err)
		}
	}
}

// ★ 超期判据的两个边：取的是【最后一轮】（不是第一轮），且是严格小于 cutoff。
//
// 同一组用例同时压 ListStalledPRDs（候选查询）与 StallPRD（原子转移），
// 两处的判据必须一致 —— 把 ORDER BY r.round DESC 改成 ASC、或把 < 改成 <=，
// 这里都会红。
func TestStalledJudgementLatestRoundAndStrictCutoff(t *testing.T) {
	// 截到微秒：timestamptz 只存到微秒，否则「恰好等于」在库里不成立。
	now := time.Now().UTC().Truncate(time.Microsecond)
	cutoff := now.Add(-168 * time.Hour)

	cases := []struct {
		name   string
		rounds []time.Time
		want   bool // 是否应被判超期 / 被搁置
	}{
		{"第 1 轮很旧、第 2 轮很新 → 不搁置（时钟取最后一轮）",
			[]time.Time{cutoff.Add(-14 * 24 * time.Hour), now.Add(-time.Hour)}, false},
		{"最后一轮恰好等于 cutoff → 不搁置（严格小于）",
			[]time.Time{cutoff}, false},
		{"最后一轮早于 cutoff 一秒 → 搁置（对照组）",
			[]time.Time{cutoff.Add(-time.Second)}, true},
		{"第 1 轮很新、第 2 轮很旧（不按时间而按轮次取最后一轮）→ 搁置",
			[]time.Time{now.Add(-time.Hour), cutoff.Add(-time.Second)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			userID, repoID := prdFixture(t, st)
			p := newPRD(t, st, userID, repoID)
			awaitWithRounds(t, st, p.ID, userID, tc.rounds...)

			rows, err := st.ListStalledPRDs(ctx, now, cutoff, 500)
			if err != nil {
				t.Fatalf("查询超期 PRD 失败: %v", err)
			}
			listed := false
			for _, r := range rows {
				if r.ID == p.ID {
					listed = true
				}
			}
			if listed != tc.want {
				t.Errorf("ListStalledPRDs 选中 got=%v want=%v", listed, tc.want)
			}

			row, err := st.StallPRD(ctx, p.ID, now, cutoff)
			stalled := err == nil
			if err != nil && !errors.Is(err, ErrPRDStateChanged) {
				t.Fatalf("StallPRD 意外错误: %v", err)
			}
			if stalled != tc.want {
				t.Errorf("StallPRD 搁置 got=%v want=%v (err=%v)", stalled, tc.want, err)
			}
			if stalled && row.State != "abandoned" {
				t.Errorf("搁置后状态 got=%q want=abandoned", row.State)
			}
		})
	}
}

// ★ StallPRD 在转移的同一条 UPDATE 里复核判据：list 之后人回来了就不搁置。
//
// 真实的回答一轮（RunRound）全程不改 state —— 有新问题就留在
// awaiting_answers —— 所以只比 state 的 CAS 拦不住这条竞态。
func TestStallPRDRechecksAtUpdate(t *testing.T) {
	cases := []struct {
		name      string
		interfere func(t *testing.T, st *Store, prdID, userID int64)
		want      bool
	}{
		{"list 之后什么都没发生 → 搁置（对照组）", nil, true},
		{"list 之后人答了一轮（新轮次刚落库）→ 不搁置",
			func(t *testing.T, st *Store, prdID, userID int64) {
				ctx := context.Background()
				if _, err := st.AppendPRDRound(ctx, AppendPRDRoundParams{
					PRDID: prdID, Round: 2, Stage: "explore", UserInput: "按内存算",
				}); err != nil {
					t.Fatal(err)
				}
				round := 2
				if _, err := st.UpdatePRDContent(ctx, prdID, userID,
					UpdatePRDContentParams{Round: &round}); err != nil {
					t.Fatal(err)
				}
			}, false},
		{"list 之后人刚提交回答（processing=true）→ 不搁置",
			func(t *testing.T, st *Store, prdID, userID int64) {
				if _, err := st.BeginPRDRound(context.Background(), prdID, userID); err != nil {
					t.Fatal(err)
				}
			}, false},
		{"processing 已僵死（超过僵死阈值）→ 照样搁置",
			func(t *testing.T, st *Store, prdID, userID int64) {
				ctx := context.Background()
				if _, err := st.BeginPRDRound(ctx, prdID, userID); err != nil {
					t.Fatal(err)
				}
				if _, err := st.pool.Exec(ctx,
					`UPDATE prds SET processing_started_at = now() - $2::interval WHERE id = $1`,
					prdID, fmt.Sprintf("%d minutes", int(prdStallAfter.Minutes())+5)); err != nil {
					t.Fatal(err)
				}
			}, true},
		{"list 之后已被别处转走（drafting）→ 不搁置",
			func(t *testing.T, st *Store, prdID, userID int64) {
				if _, err := st.TransitionPRD(context.Background(), prdID, userID,
					TransitionPRDParams{From: "awaiting_answers", To: "drafting"}); err != nil {
					t.Fatal(err)
				}
			}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			userID, repoID := prdFixture(t, st)
			p := newPRD(t, st, userID, repoID)
			awaitFor(t, st, p.ID, userID, 14*24*time.Hour)

			now := time.Now()
			cutoff := now.Add(-168 * time.Hour)
			rows, err := st.ListStalledPRDs(ctx, now, cutoff, 500)
			if err != nil {
				t.Fatal(err)
			}
			listed := false
			for _, r := range rows {
				listed = listed || r.ID == p.ID
			}
			if !listed {
				t.Fatal("前提不成立：两周没人答的 PRD 应先被 list 选中")
			}

			if tc.interfere != nil {
				tc.interfere(t, st, p.ID, userID)
			}

			_, err = st.StallPRD(ctx, p.ID, time.Now(), cutoff)
			if err != nil && !errors.Is(err, ErrPRDStateChanged) {
				t.Fatalf("StallPRD 意外错误: %v", err)
			}
			if got := err == nil; got != tc.want {
				t.Errorf("StallPRD 搁置 got=%v want=%v (err=%v)", got, tc.want, err)
			}
			cur, gerr := st.getPRD(ctx, p.ID, userID)
			if gerr != nil {
				t.Fatal(gerr)
			}
			if gotAbandoned := cur.State == "abandoned"; gotAbandoned != tc.want {
				t.Errorf("库内状态 got=%q，want 搁置=%v", cur.State, tc.want)
			}
		})
	}
}
