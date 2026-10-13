package main

// stall_wiring_test.go 把「PRD 搁置」这条链路端到端钉在真库上。
//
// 单元测试（internal/prd/stall_test.go）用的是假件，它们验的是扫描器的
// 判定与顺序；store 层测试（internal/store/prds_test.go）验的是 SQL 判据。
// 本文件补上剩下那一段 —— 装配：真的 store + 真的 prdStallMailer（只把
// SMTP 换成假件），跑一轮扫描看它是否真的转状态、真的解析出了收件人。
//
// 之所以值得单独测：prdStallMailer 是 cmd/lathe 里的适配器，它把
// store（查收件人）、internal/mail（发信）与 prd.StallNotifier 三个东西
// 接在一起，而这三者各自的测试都不会覆盖这段接线。接错了的表现是
// 「判定照跑、状态照转，只是信永远发不出去」—— 静默失效。

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/prd"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// stallFixture 建一份「停在 awaiting_answers、最后一轮在 ago 之前」的真 PRD。
func stallFixture(t *testing.T, st *store.Store, ago time.Duration) (prdID, userID int64) {
	t.Helper()
	ctx := context.Background()
	userID, repoID := fixture(t, st)

	p, err := st.CreatePRD(ctx, store.CreatePRDParams{
		UserID: userID, RepoID: repoID, PRDType: "feature",
		OriginalInput: "预览阈值写死了，想让管理员能配",
	})
	if err != nil {
		t.Fatalf("建 PRD 失败: %v", err)
	}
	if _, err := st.AppendPRDRound(ctx, store.AppendPRDRoundParams{
		PRDID: p.ID, Round: 1, Stage: "understand",
		Questions: json.RawMessage(`["阈值按内存还是磁盘算？"]`),
	}); err != nil {
		t.Fatalf("落轮次失败: %v", err)
	}
	// 轮次推进与快照写入在真实流程里是同一步（service.persistRound 同时
	// 做这两件事）。fixture 照做，别留下一个生产环境不会出现的 prds.round=0。
	round := 1
	if _, err := st.UpdatePRDContent(ctx, p.ID, userID,
		store.UpdatePRDContentParams{Round: &round}); err != nil {
		t.Fatalf("推进轮次失败: %v", err)
	}
	if _, err := st.TransitionPRD(ctx, p.ID, userID,
		store.TransitionPRDParams{From: "drafting", To: "awaiting_answers"}); err != nil {
		t.Fatalf("转 awaiting_answers 失败: %v", err)
	}
	// 把最后一轮的时间往回推（created_at 没有触发器守着）。
	if _, err := st.Pool().Exec(ctx,
		`UPDATE prd_rounds SET created_at = now() - $2::interval WHERE prd_id = $1`,
		p.ID, strconv.Itoa(int(ago.Seconds()))+" seconds"); err != nil {
		t.Fatalf("推前轮次时间失败: %v", err)
	}
	return p.ID, userID
}

// ★ 端到端：真 store + 真收件人解析 → 超期 PRD 被转搁置，且解析出了属主邮箱。
//
// 收件人那一段尤其要紧：NotifyEmailForPRD 是新加的方法，它的 JOIN 写错
// （例如误 JOIN tasks）不会被任何别的测试发现 —— 只会表现为「状态转了、
// 但信发不出去」，而发信失败在本实现里是静默的。
func TestStallSweepEndToEndResolvesRecipient(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	prdID, userID := stallFixture(t, st, 14*24*time.Hour)

	// 收件人解析走的就是 prdStallMailer 内部那一步。
	to, err := st.NotifyEmailForPRD(ctx, prdID)
	if err != nil {
		t.Fatalf("应能解析出 PRD 属主的收件人: %v", err)
	}
	if to == "" {
		t.Fatal("收件人不能为空")
	}

	// 驱动一轮真扫描（发信那一层用替身，其余全是真的）。
	var hit []int64
	mailer := stallNotifierFunc(func(_ context.Context, id int64, _, _ string) error {
		hit = append(hit, id)
		return nil
	})
	svc := &prd.StallSweeper{
		Store:    st,
		Notifier: mailer,
		Notify:   true,
		Timeout:  168 * time.Hour,
		BaseURL:  "https://lathe.example.com",
	}

	n, err := svc.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应搁置 1 份，got=%d", n)
	}

	// 状态真的落了库。
	row, err := st.GetPRD(ctx, prdID, userID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != string(prd.StateAbandoned) {
		t.Errorf("状态 got=%q want=%q", row.State, prd.StateAbandoned)
	}
	if len(hit) != 1 || hit[0] != prdID {
		t.Errorf("应恰好通知一次且对象正确，got=%v", hit)
	}
}

// 端到端幂等：连扫两轮只发一封信、只转一次（真库上的 CAS 行为）。
func TestStallSweepEndToEndIdempotent(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	stallFixture(t, st, 14*24*time.Hour)

	var count int
	mailer := stallNotifierFunc(func(context.Context, int64, string, string) error {
		count++
		return nil
	})
	svc := &prd.StallSweeper{
		Store: st, Notifier: mailer, Notify: true, Timeout: 168 * time.Hour,
	}

	for i := 0; i < 3; i++ {
		if _, err := svc.SweepOnce(ctx); err != nil {
			t.Fatalf("第 %d 轮扫描失败: %v", i+1, err)
		}
	}
	if count != 1 {
		t.Errorf("真库上连扫三轮也只该发一封信，got=%d", count)
	}
}

// 发信失败（SMTP 故障）不能让状态回滚 —— 真库上再验一次这条约束。
func TestStallSweepEndToEndMailFailureKeepsState(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	prdID, userID := stallFixture(t, st, 14*24*time.Hour)

	mailer := stallNotifierFunc(func(context.Context, int64, string, string) error {
		return errors.New("SMTP 炸了")
	})
	svc := &prd.StallSweeper{
		Store: st, Notifier: mailer, Notify: true, Timeout: 168 * time.Hour,
	}

	if _, err := svc.SweepOnce(ctx); err != nil {
		t.Fatalf("发信失败不该让扫描报错: %v", err)
	}
	row, err := st.GetPRD(ctx, prdID, userID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != string(prd.StateAbandoned) {
		t.Errorf("发信失败不该回滚状态，got=%q", row.State)
	}
}

// 搁置之后人唤醒 → 再跑一轮 → 又能接着对话（真库上的完整来回）。
func TestStallThenResumeAndAnswerRound(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	prdID, userID := stallFixture(t, st, 14*24*time.Hour)

	svc := &prd.StallSweeper{
		Store:    st,
		Notifier: stallNotifierFunc(func(context.Context, int64, string, string) error { return nil }),
		Notify:   true,
		Timeout:  168 * time.Hour,
	}
	if _, err := svc.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// 人点「继续这份 PRD」。
	row, err := st.TransitionPRD(ctx, prdID, userID,
		store.TransitionPRDParams{From: "abandoned", To: "drafting"})
	if err != nil {
		t.Fatalf("唤醒失败: %v", err)
	}
	if row.State != "drafting" {
		t.Fatalf("唤醒后 got=%q want=drafting", row.State)
	}

	// 回来能接着答一轮（真库上验「球又到了智能体这边」）。
	if _, err := st.BeginPRDRound(ctx, prdID, userID); err != nil {
		t.Errorf("唤醒后应能起新一轮: %v", err)
	}
}

// stallNotifierFunc 把函数适配成 prd.StallNotifier。
type stallNotifierFunc func(ctx context.Context, prdID int64, subject, body string) error

func (f stallNotifierFunc) NotifyPRDStalled(ctx context.Context, prdID int64, subject, body string) error {
	return f(ctx, prdID, subject, body)
}

// raceStallStore 在真 store 外面套一层：ListStalledPRDs 返回之后、扫描器
// 动手转移之前，执行一次 afterList —— 正是「人在 list 与转移之间回答」
// 的那道缝。其余方法全部透传给真 store。
type raceStallStore struct {
	*store.Store
	afterList func()
}

func (r raceStallStore) ListStalledPRDs(ctx context.Context, now, cutoff time.Time, limit int) ([]store.PRDRow, error) {
	rows, err := r.Store.ListStalledPRDs(ctx, now, cutoff, limit)
	if err == nil && r.afterList != nil {
		r.afterList()
	}
	return rows, err
}

// ★ 竞态：扫描器查出候选之后、转移之前，人回答了一轮或刚起了一轮。
//
// 真实的 RunRound 回答一轮时 PRD 一直停在 awaiting_answers（有新问题就
// 留在原状态，persistRound 的 drafting→awaiting CAS 失配是预期内的），
// 所以只比 state 的 CAS 拦不住 —— 转移必须在同一条 UPDATE 里复核
// 「最后一轮仍早于 cutoff」与「没有在跑的轮次」。
func TestStallSweepRaceWithAnswerAfterList(t *testing.T) {
	cases := []struct {
		name string
		// interfere 在 list 之后、转移之前对这份 PRD 做的事。
		interfere func(t *testing.T, st *store.Store, prdID, userID int64)
	}{
		{
			name: "list 之后人答完一轮（新轮次落库、状态仍是 awaiting_answers）→ 不搁置、不发信",
			interfere: func(t *testing.T, st *store.Store, prdID, userID int64) {
				ctx := context.Background()
				if _, err := st.AppendPRDRound(ctx, store.AppendPRDRoundParams{
					PRDID: prdID, Round: 2, Stage: "explore", UserInput: "按内存算",
					Questions: json.RawMessage(`["上限要不要按仓库区分？"]`),
				}); err != nil {
					t.Fatalf("落第 2 轮失败: %v", err)
				}
				round := 2
				if _, err := st.UpdatePRDContent(ctx, prdID, userID,
					store.UpdatePRDContentParams{Round: &round}); err != nil {
					t.Fatalf("推进轮次失败: %v", err)
				}
			},
		},
		{
			name: "list 之后人刚提交回答（BeginPRDRound，processing=true）→ 不搁置、不发信",
			interfere: func(t *testing.T, st *store.Store, prdID, userID int64) {
				if _, err := st.BeginPRDRound(context.Background(), prdID, userID); err != nil {
					t.Fatalf("抢占轮次失败: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := testStore(t)
			ctx := context.Background()
			prdID, userID := stallFixture(t, st, 14*24*time.Hour)

			var hit []int64
			svc := &prd.StallSweeper{
				Store: raceStallStore{Store: st, afterList: func() {
					tc.interfere(t, st, prdID, userID)
				}},
				Notifier: stallNotifierFunc(func(_ context.Context, id int64, _, _ string) error {
					hit = append(hit, id)
					return nil
				}),
				Notify:  true,
				Timeout: 168 * time.Hour,
			}

			if _, err := svc.SweepOnce(ctx); err != nil {
				t.Fatalf("扫描失败: %v", err)
			}
			row, err := st.GetPRD(ctx, prdID, userID)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != string(prd.StateAwaitingAnswers) {
				t.Errorf("状态 got=%q want=%q：人已经回来了，不该被搁置",
					row.State, prd.StateAwaitingAnswers)
			}
			for _, id := range hit {
				if id == prdID {
					t.Errorf("不该给刚回答的 PRD 发搁置信，got 通知=%v", hit)
				}
			}
		})
	}
}
