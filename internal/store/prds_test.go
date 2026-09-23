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
