package prd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// 本文件守「交互少、跑得完」这一批改动（docs/10 §1.3 / §1.4 的 2026-10-20 修订）：
// 输出修正、阶段自动推进、后台运行与失败落库、awaiting_answers 回流。
// 每条都对应 dev 实例那份 11 轮 PRD 上实测到的一个毛病。

// 一份已经走到收敛阶段的 PRD：有目标、场景、一条 AC，用来跑后续阶段。
const convergedDoc = `{"type":"feature",
	"oneLiner":{"status":"inferred","body":"让管理员配预览阈值"},
	"goalList":[{"id":"G-1","text":"阈值可配","measure":"改完生效"}],
	"scenarioList":[{"id":"S-1","title":"改阈值","given":"已登录","when":"改 75","then":"80% 被拒"}],
	"criteria":[{"id":"AC-1","category":"happy_path","given":"阈值 75","when":"占用 80%",
	  "then":"返回 409","evidence":"HTTP 409","verify":"auto_test","goalRefs":["G-1"],"scenarioRefs":["S-1"]}]}`

func convergedStore(rounds ...store.PRDRoundRow) *fakeStore {
	st := newFakeStore()
	st.row.Document = json.RawMessage(convergedDoc)
	st.rounds = rounds
	st.row.Round = len(rounds)
	return st
}

func qs(n int) json.RawMessage {
	list := make([]string, n)
	for i := range list {
		list[i] = "q"
	}
	raw, _ := json.Marshal(list)
	return raw
}

const (
	splitOK = `{"type":"feature","patch":{"tasks":[{"key":"T1","title":"阈值改配置项","kind":"feature",
		"description":"改常量为读设置；交付测试证明可配","acceptance":["AC-1"],"estimate":{"lines":120,"files":3}}]},
		"questions":[],"notes":"拆成一个任务"}`
	// 实测第 6 轮被拒的形态：dependsOn 里写了两个前驱。
	splitBadDeps = `{"type":"feature","patch":{"tasks":[
		{"key":"T1","title":"a","kind":"feature","acceptance":["AC-1"]},
		{"key":"T2","title":"b","kind":"feature","acceptance":["AC-1"]},
		{"key":"T3","title":"c","kind":"feature","acceptance":["AC-1"],"dependsOn":"T1,T2"}]},"questions":[]}`
	noChange   = `{"type":"feature","patch":{},"questions":[],"notes":"无改动"}`
	reviewNone = `{"findings":[],"verdict":"攻不动"}`
)

// 输出没过校验时，在同一会话里就地修正：--resume 同一个 SessionID、
// 把校验错误回喂，检出直到修正结束才回收。原来是整轮作废、人重试。
func TestRunRoundRepairsRejectedOutputInSameSession(t *testing.T) {
	st, wt := convergedStore(
		store.PRDRoundRow{Round: 1, Stage: "understand"},
		store.PRDRoundRow{Round: 2, Stage: "explore"},
		store.PRDRoundRow{Round: 3, Stage: "converge"},
	), &fakeWorktrees{}
	removedDuringRepair := -1
	ag := &fakeAgent{results: []*agent.Result{ok(splitBadDeps), ok(splitOK)}}
	ag.during = func(p agent.RunParams) {
		if p.Resume {
			removedDuringRepair = wt.removed
		}
	}
	svc := newService(st, ag, wt)

	res, err := svc.RunRound(context.Background(), RunRoundParams{PRDID: 1, UserID: 7})
	if err != nil {
		t.Fatalf("修正后应成功: %v", err)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("应跑 2 次（首跑 + 一次修正），got=%d", len(ag.calls))
	}
	first, fix := ag.calls[0], ag.calls[1]
	if first.Resume || !fix.Resume {
		t.Errorf("首跑不该 resume、修正必须 resume，got first=%v fix=%v", first.Resume, fix.Resume)
	}
	if fix.SessionID != first.SessionID {
		t.Errorf("修正必须续同一个会话，got %q vs %q", fix.SessionID, first.SessionID)
	}
	if fix.Dir != first.Dir {
		t.Errorf("修正必须在同一个目录里跑（会话按目录存），got %q vs %q", fix.Dir, first.Dir)
	}
	if !strings.Contains(fix.Prompt, "只能填一个") {
		t.Errorf("修正指令里必须带上校验错误原文，got=%q", fix.Prompt)
	}
	if removedDuringRepair != 0 {
		t.Errorf("修正时检出必须还在，got 已回收 %d 次", removedDuringRepair)
	}
	if wt.created != 1 || wt.removed != 1 {
		t.Errorf("整轮只检出一次、回收一次，got created=%d removed=%d", wt.created, wt.removed)
	}
	if len(res.Document.Tasks) != 1 || len(st.rounds) != 4 {
		t.Errorf("修正后的产物应落库，got tasks=%d rounds=%d", len(res.Document.Tasks), len(st.rounds))
	}
}

// 修正次数用尽：不落快照、不推进轮次，失败原因与人的回答落到行上。
func TestRunRoundGivesUpAfterMaxRepairs(t *testing.T) {
	st, wt := convergedStore(store.PRDRoundRow{Round: 1, Stage: "converge", Questions: qs(1)}), &fakeWorktrees{}
	ag := &fakeAgent{results: []*agent.Result{ok("不是 JSON"), ok("还不是"), ok("依然不是"), ok(splitOK)}}
	svc := newService(st, ag, wt)

	_, err := svc.RunRound(context.Background(), RunRoundParams{PRDID: 1, UserID: 7, Answer: "1. A"})
	if !errors.Is(err, ErrOutputRejected) {
		t.Fatalf("应报 ErrOutputRejected，got=%v", err)
	}
	if got, want := len(ag.calls), 1+maxRepairs; got != want {
		t.Errorf("agent 调用次数 got %d, want %d（首跑 + %d 次修正）", got, want, maxRepairs)
	}
	if len(st.rounds) != 1 || st.row.Round != 1 {
		t.Errorf("失败不该推进轮次，got rounds=%d round=%d", len(st.rounds), st.row.Round)
	}
	if st.row.LastError == nil || !strings.Contains(*st.row.LastError, "跑规划轮次失败") {
		t.Errorf("失败原因必须落库，got=%v", st.row.LastError)
	}
	if st.row.LastErrorInput == nil || *st.row.LastErrorInput != "1. A" {
		t.Errorf("人的回答必须留下供界面放回输入框，got=%v", st.row.LastErrorInput)
	}
	if st.busy || wt.removed != wt.created {
		t.Error("失败也必须释放 processing、回收检出")
	}
}

// agent 本身失败（超时、报错）不做输出修正：那不是 JSON 写错，续会话没用。
func TestRunRoundDoesNotRepairAgentFailure(t *testing.T) {
	st, wt := convergedStore(store.PRDRoundRow{Round: 1, Stage: "converge", Questions: qs(1)}), &fakeWorktrees{}
	ag := &fakeAgent{errs: []error{errors.New("signal: killed")}, results: []*agent.Result{nil, ok(splitOK)}}
	svc := newService(st, ag, wt)

	if _, err := svc.RunRound(context.Background(), RunRoundParams{PRDID: 1, UserID: 7}); err == nil {
		t.Fatal("agent 失败应报错")
	}
	if len(ag.calls) != 1 {
		t.Errorf("agent 失败不该触发修正，got 调用 %d 次", len(ag.calls))
	}
}

// awaiting_answers 必须能回到 drafting：原来没有任何代码走这条边，
// 问过一次问题的 PRD 就永远提交不了评审（提交要求 drafting）。
func TestPersistRoundMovesStateBothWays(t *testing.T) {
	cases := []struct {
		name   string
		from   State
		output string
		want   State
	}{
		{"有问题 → 挂起等人答", StateDrafting,
			`{"type":"feature","patch":{},"questions":["阈值按内存算？（默认：是）"]}`, StateAwaitingAnswers},
		{"人答完、本轮没有新问题 → 回到起草", StateAwaitingAnswers, noChange, StateDrafting},
		{"人答完、又有新问题 → 继续挂起", StateAwaitingAnswers,
			`{"type":"feature","patch":{},"questions":["q？（默认：A）"]}`, StateAwaitingAnswers},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, wt := convergedStore(store.PRDRoundRow{Round: 1, Stage: "converge", Questions: qs(1)}), &fakeWorktrees{}
			st.row.State = string(tc.from)
			svc := newService(st, &fakeAgent{results: []*agent.Result{ok(tc.output)}}, wt)
			if _, err := svc.RunRound(context.Background(), RunRoundParams{PRDID: 1, UserID: 7, Answer: "按默认"}); err != nil {
				t.Fatal(err)
			}
			if st.row.State != string(tc.want) {
				t.Errorf("state got %q, want %q", st.row.State, tc.want)
			}
		})
	}
}

// 阶段推断：收敛不再无限停留。
func TestNextStage(t *testing.T) {
	r := func(stage string, q int) store.PRDRoundRow {
		return store.PRDRoundRow{Stage: stage, Questions: qs(q)}
	}
	cases := []struct {
		name   string
		rounds []store.PRDRoundRow
		want   Stage
	}{
		{"首轮 → 理解", nil, StageUnderstand},
		{"理解之后 → 探索", []store.PRDRoundRow{r("understand", 3)}, StageExplore},
		{"探索之后 → 收敛", []store.PRDRoundRow{r("understand", 3), r("explore", 2)}, StageConverge},
		{"收敛一轮还有问题 → 继续收敛",
			[]store.PRDRoundRow{r("understand", 3), r("explore", 2), r("converge", 1)}, StageConverge},
		{"收敛一轮没有问题 → 拆分",
			[]store.PRDRoundRow{r("understand", 3), r("explore", 2), r("converge", 0)}, StageSplit},
		{"收敛满两轮即使还有问题 → 拆分",
			[]store.PRDRoundRow{r("understand", 3), r("explore", 2), r("converge", 1), r("converge", 1)}, StageSplit},
		{"拆分之后 → 定稿", []store.PRDRoundRow{r("split", 0)}, StageFinalize},
		{"定稿之后停在定稿", []store.PRDRoundRow{r("split", 0), r("finalize", 1)}, StageFinalize},
		{"历史轮次没有阶段 → 收敛", []store.PRDRoundRow{r("", 0)}, StageConverge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextStage(tc.rounds); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// 问题预算按阶段收紧：收敛阶段模型问了 3 个，只留 1 个。
func TestRunRoundAppliesStageQuestionBudget(t *testing.T) {
	st, wt := convergedStore(
		store.PRDRoundRow{Round: 1, Stage: "understand"},
		store.PRDRoundRow{Round: 2, Stage: "explore"},
	), &fakeWorktrees{}
	ag := &fakeAgent{results: []*agent.Result{ok(`{"type":"feature","patch":{},"questions":["a？","b？","c？"]}`)}}
	svc := newService(st, ag, wt)

	res, err := svc.RunRound(context.Background(), RunRoundParams{PRDID: 1, UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(res.Questions), QuestionBudget(StageConverge); got != want {
		t.Errorf("收敛阶段问题数 got %d, want %d", got, want)
	}
	if !strings.Contains(ag.calls[0].Prompt, "本轮最多 1 个") {
		t.Error("prompt 里必须告诉模型本轮的问题预算")
	}
}

// 后台运行：StartRound 同步抢占、立即返回；没有问题就自动推进到定稿，
// 定稿后顺带跑对抗复核；整串共用一次抢占，每轮续期，结束释放。
func TestStartRoundChainsUntilFinalizeAndReviews(t *testing.T) {
	st, wt := convergedStore(
		store.PRDRoundRow{Round: 1, Stage: "understand", Questions: qs(3)},
		store.PRDRoundRow{Round: 2, Stage: "explore", Questions: qs(2)},
		store.PRDRoundRow{Round: 3, Stage: "converge", Questions: qs(1)},
	), &fakeWorktrees{}
	st.row.State = string(StateAwaitingAnswers)
	ag := &fakeAgent{results: []*agent.Result{ok(noChange), ok(splitOK), ok(noChange), ok(reviewNone)}}
	svc := newService(st, ag, wt)

	if err := svc.StartRound(context.Background(), RunRoundParams{PRDID: 1, UserID: 7, Answer: "按默认"}); err != nil {
		t.Fatalf("StartRound 应成功: %v", err)
	}
	svc.Wait()

	var stages []string
	for _, r := range st.rounds[3:] {
		stages = append(stages, r.Stage)
	}
	if got, want := strings.Join(stages, ","), "converge,split,finalize"; got != want {
		t.Errorf("自动推进的阶段 got %q, want %q", got, want)
	}
	if st.rounds[3].UserInput != "按默认" || st.rounds[4].UserInput != "" {
		t.Error("人的回答只属于第一轮，自动续推的轮次不该重复带上")
	}
	if len(st.row.ReviewReport) == 0 {
		t.Error("定稿之后应顺带跑对抗复核并落库")
	}
	if st.row.State != string(StateDrafting) {
		t.Errorf("没有挂着的问题，应回到 drafting 以便提交评审，got %q", st.row.State)
	}
	if st.beginCnt != 1 {
		t.Errorf("整串只抢占一次，got %d", st.beginCnt)
	}
	if st.touchCnt < 3 {
		t.Errorf("后续每轮（含复核）都要续期，got %d 次", st.touchCnt)
	}
	if st.busy {
		t.Error("跑完必须释放 processing")
	}
	if st.row.LastError != nil {
		t.Errorf("全程成功不该有失败记录，got %q", *st.row.LastError)
	}
}

// 有问题要问就停下等人，不再往下推。
func TestStartRoundStopsWhenQuestionsAsked(t *testing.T) {
	st, wt := convergedStore(
		store.PRDRoundRow{Round: 1, Stage: "understand", Questions: qs(3)},
		store.PRDRoundRow{Round: 2, Stage: "explore", Questions: qs(2)},
	), &fakeWorktrees{}
	ag := &fakeAgent{results: []*agent.Result{ok(`{"type":"feature","patch":{},"questions":["q？（默认：A）"]}`), ok(splitOK)}}
	svc := newService(st, ag, wt)

	if err := svc.StartRound(context.Background(), RunRoundParams{PRDID: 1, UserID: 7}); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	if len(ag.calls) != 1 {
		t.Errorf("有问题要问时应停下，got agent 调用 %d 次", len(ag.calls))
	}
	if st.row.State != string(StateAwaitingAnswers) {
		t.Errorf("应挂起等人答，got %q", st.row.State)
	}
}

// 抢占冲突同步返回（前端立刻能提示「已有一轮在跑」），不起后台运行。
func TestStartRoundBusyIsSynchronous(t *testing.T) {
	st := convergedStore()
	st.busy = true
	ag := &fakeAgent{}
	svc := newService(st, ag, &fakeWorktrees{})

	if err := svc.StartRound(context.Background(), RunRoundParams{PRDID: 1, UserID: 7}); !errors.Is(err, store.ErrPRDBusy) {
		t.Fatalf("应同步报 ErrPRDBusy，got=%v", err)
	}
	svc.Wait()
	if len(ag.calls) != 0 {
		t.Error("抢占失败不该起 agent")
	}
}

// 发起请求的连接断开不该杀掉后台这一轮 —— 那正是后台化要修的问题。
func TestStartRoundSurvivesRequestCancel(t *testing.T) {
	st, wt := convergedStore(store.PRDRoundRow{Round: 1, Stage: "converge", Questions: qs(1)}), &fakeWorktrees{}
	ctx, cancel := context.WithCancel(context.Background())
	var ctxErrSeen error
	ag := &fakeAgent{results: []*agent.Result{ok(`{"type":"feature","patch":{},"questions":["q？"]}`)}}
	ag.during = func(agent.RunParams) { cancel() }
	ag.ctxCheck = func(c context.Context) { ctxErrSeen = c.Err() }
	svc := newService(st, ag, wt)

	if err := svc.StartRound(ctx, RunRoundParams{PRDID: 1, UserID: 7}); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	if ctxErrSeen != nil {
		t.Errorf("请求取消不该传到后台 agent，got ctx.Err()=%v", ctxErrSeen)
	}
	if len(st.rounds) != 2 {
		t.Errorf("这一轮应照常落库，got rounds=%d", len(st.rounds))
	}
}

// 后台运行 panic 也要释放抢占并留下失败记录，否则这份 PRD 卡到僵死阈值。
func TestStartRoundPanicReleasesAndRecords(t *testing.T) {
	st, wt := convergedStore(store.PRDRoundRow{Round: 1, Stage: "converge", Questions: qs(1)}), &fakeWorktrees{}
	ag := &fakeAgent{}
	ag.during = func(agent.RunParams) { panic("boom") }
	svc := newService(st, ag, wt)

	if err := svc.StartRound(context.Background(), RunRoundParams{PRDID: 1, UserID: 7}); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	if st.busy {
		t.Error("panic 后必须释放 processing")
	}
	if st.row.LastError == nil || !strings.Contains(*st.row.LastError, "boom") {
		t.Errorf("panic 应落成失败记录，got=%v", st.row.LastError)
	}
}

// 轮次进行中人在面板上确认了一节：本轮没改这节，落库不能把确认冲掉。
func TestPersistRoundKeepsConfirmationMadeDuringRun(t *testing.T) {
	st, wt := convergedStore(store.PRDRoundRow{Round: 1, Stage: "converge", Questions: qs(1)}), &fakeWorktrees{}
	ag := &fakeAgent{results: []*agent.Result{ok(
		`{"type":"feature","patch":{"sections":{"problem":{"body":"补了证据 gate.go:31"}}},"questions":["q？"]}`)}}
	ag.during = func(agent.RunParams) {
		doc, _ := ParseDocument(st.row.Document)
		doc.OneLiner.Status = StatusConfirmed // 人在面板上点了确认
		st.row.Document, _ = doc.Marshal()
	}
	svc := newService(st, ag, wt)

	if _, err := svc.RunRound(context.Background(), RunRoundParams{PRDID: 1, UserID: 7}); err != nil {
		t.Fatal(err)
	}
	doc, _ := ParseDocument(st.row.Document)
	if doc.OneLiner.Status != StatusConfirmed {
		t.Errorf("本轮没改的节，人的确认必须保留，got %q", doc.OneLiner.Status)
	}
	if doc.Problem.Status != StatusInferred {
		t.Errorf("本轮改过的节要人重新过目，got %q", doc.Problem.Status)
	}
}

// 后台复核失败落库；没有 AC 时同步入口照旧报 ErrStageUnavailable 而不记失败。
func TestStartReviewRecordsFailure(t *testing.T) {
	st := convergedStore()
	ag := &fakeAgent{results: []*agent.Result{ok("不是 JSON"), ok("还不是"), ok("依然不是")}}
	svc := newService(st, ag, &fakeWorktrees{})

	if err := svc.StartReview(context.Background(), 1, 7); err != nil {
		t.Fatal(err)
	}
	svc.Wait()
	if st.row.LastError == nil || !strings.Contains(*st.row.LastError, "对抗复核失败") {
		t.Errorf("复核失败应落库，got=%v", st.row.LastError)
	}
	if st.busy {
		t.Error("必须释放 processing")
	}
}
