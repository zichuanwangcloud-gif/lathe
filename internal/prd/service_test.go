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

// 编排层的测试。全部用内存假实现，不连 Postgres —— 这一层要验的是
// 「轮次怎么推进、失败时状态干净不干净」，跟 SQL 无关。store 层自己的
// 契约（CAS、冻结）在 store/prds_test.go 里对着真库验。

// fakeStore 是 Service.Store 的内存实现。
type fakeStore struct {
	row    store.PRDRow
	rounds []store.PRDRoundRow
	repo   RepoInfo

	// events 按 phase 累计落库的事件数，验「事件有没有落」。
	events map[string]int

	busy      bool // processing 标记
	beginErr  error
	beginCnt  int
	updateCnt int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		row: store.PRDRow{
			ID: 1, UserID: 7, RepoID: 3,
			PRDType:       string(TypeFeature),
			State:         string(StateDrafting),
			OriginalInput: "预览阈值写死了，想让管理员能配",
		},
		repo: RepoInfo{
			ProviderRepo:  "acme/lathe",
			DefaultBranch: "main",
			Limits:        Limits{MaxLines: 400, MaxFiles: 8, MaxChain: 4},
		},
		events: map[string]int{},
	}
}

func (f *fakeStore) GetPRD(context.Context, int64, int64) (*store.PRDRow, error) {
	cp := f.row
	return &cp, nil
}

func (f *fakeStore) UpdatePRDContent(_ context.Context, _, _ int64, p store.UpdatePRDContentParams) (*store.PRDRow, error) {
	f.updateCnt++
	if f.row.State == string(StateApproved) || f.row.State == string(StateConverted) {
		return nil, store.ErrPRDFrozen
	}
	if p.Document != nil {
		f.row.Document = p.Document
	}
	if p.TaskBlocks != nil {
		f.row.TaskBlocks = p.TaskBlocks
	}
	if p.ReviewReport != nil {
		f.row.ReviewReport = p.ReviewReport
	}
	if p.PRDType != nil {
		f.row.PRDType = *p.PRDType
	}
	if p.Round != nil {
		f.row.Round = *p.Round
	}
	cp := f.row
	return &cp, nil
}

func (f *fakeStore) TransitionPRD(_ context.Context, _, _ int64, p store.TransitionPRDParams) (*store.PRDRow, error) {
	if f.row.State != p.From {
		return nil, store.ErrPRDStateChanged
	}
	f.row.State = p.To
	cp := f.row
	return &cp, nil
}

func (f *fakeStore) BeginPRDRound(context.Context, int64, int64) (*store.PRDRow, error) {
	f.beginCnt++
	if f.beginErr != nil {
		return nil, f.beginErr
	}
	if f.busy {
		return nil, store.ErrPRDBusy
	}
	f.busy = true
	cp := f.row
	return &cp, nil
}

func (f *fakeStore) EndPRDRound(context.Context, int64, int64) error {
	f.busy = false
	return nil
}

func (f *fakeStore) AppendPRDRound(_ context.Context, p store.AppendPRDRoundParams) (*store.PRDRoundRow, error) {
	for _, r := range f.rounds {
		if r.Round == p.Round {
			return nil, errors.New("fake: 同轮次重复")
		}
	}
	row := store.PRDRoundRow{
		ID: int64(len(f.rounds) + 1), PRDID: p.PRDID, Round: p.Round,
		Stage: p.Stage, AgentSessionID: p.AgentSessionID,
		UserInput: p.UserInput, Questions: p.Questions,
		DocumentSnapshot: p.DocumentSnapshot, Notes: p.Notes,
	}
	f.rounds = append(f.rounds, row)
	return &row, nil
}

func (f *fakeStore) ListPRDRounds(context.Context, int64) ([]store.PRDRoundRow, error) {
	return f.rounds, nil
}

func (f *fakeStore) InsertPRDEvents(_ context.Context, _ int64, _ *int, phase string, e []agent.Entry) error {
	f.events[phase] += len(e)
	return nil
}

func (f *fakeStore) RepoForPRD(context.Context, int64, int64) (RepoInfo, error) {
	return f.repo, nil
}

// fakeWorktrees 记账「检出了几次、回收了几次」——D10-8 要求用完即回收，
// 这两个数不相等就是漏了现场。
type fakeWorktrees struct {
	created  int
	removed  int
	lastBase string
	// removedMirrors 记下每次回收拿到的 Mirror。空串意味着回收时 git 会在
	// serve 的 cwd 里跑（AGENTS.md §9 的 B2-3 形状），必须能被测出来。
	removedMirrors []string
	createErr      error
}

func (f *fakeWorktrees) EnsureMirror(context.Context, string, string) (string, error) {
	return "/mirrors/acme", nil
}

func (f *fakeWorktrees) CreateDetached(_ context.Context, _, base, name string) (*Checkout, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created++
	f.lastBase = base
	// 带上 Mirror：回收时 git 必须在所属 mirror 里跑，假实现照实模拟，
	// 否则「Mirror 丢了」这类缺陷在单测里看不出来。
	return &Checkout{Path: "/verify/" + name, Mirror: "/mirrors/acme"}, nil
}

func (f *fakeWorktrees) RemoveDetached(_ context.Context, c *Checkout) error {
	f.removed++
	if c != nil {
		f.removedMirrors = append(f.removedMirrors, c.Mirror)
	}
	return nil
}

// fakeAgent 按调用次序返回预置结果，并记下每次拿到的 RunParams
// （验只读模式、验 prompt 里有没有该有的上下文）。
type fakeAgent struct {
	results []*agent.Result
	errs    []error
	calls   []agent.RunParams
}

func (f *fakeAgent) Run(_ context.Context, p agent.RunParams) (*agent.Result, error) {
	i := len(f.calls)
	f.calls = append(f.calls, p)

	// 真驱动会在读流的过程中回调 OnEvent；假驱动也必须回调，否则
	// 「事件有没有落库」这条断言测的是假实现的惰性，不是产品行为。
	if p.OnEvent != nil {
		p.OnEvent(agent.Event{
			Type:      agent.EventAssistant,
			SessionID: p.SessionID,
			Raw: json.RawMessage(
				`{"type":"assistant","message":{"content":[{"type":"text","text":"在读 internal/preview/gate.go"}]}}`),
		})
	}

	if i < len(f.errs) && f.errs[i] != nil {
		return nil, f.errs[i]
	}
	if i >= len(f.results) {
		return nil, errors.New("fake: 预置结果已用尽")
	}
	return f.results[i], nil
}

func ok(text string) *agent.Result {
	return &agent.Result{SessionID: "sess-1", Success: true, Text: text}
}

func newService(st *fakeStore, ag *fakeAgent, wt *fakeWorktrees) *Service {
	return &Service{
		Store: st, Agent: ag, Worktrees: wt,
		Channel:  "strong",
		CloneURL: func(r string) string { return "git@example.com:" + r + ".git" },
	}
}

// 阶段 A 明令先不读代码：不该检出仓库（省一次检出与回收）。
func TestRunRoundStageAUsesNoCheckout(t *testing.T) {
	st, wt := newFakeStore(), &fakeWorktrees{}
	ag := &fakeAgent{results: []*agent.Result{ok(
		`{"type":"feature","document":{"oneLiner":{"body":"让管理员配预览阈值"}},
		  "questions":["阈值按内存还是磁盘算？"]}`)}}
	svc := newService(st, ag, wt)

	res, err := svc.RunRound(context.Background(), RunRoundParams{PRDID: 1, UserID: 7})
	if err != nil {
		t.Fatalf("首轮应成功: %v", err)
	}

	if wt.created != 0 {
		t.Errorf("阶段 A 不该检出仓库，got created=%d", wt.created)
	}
	if len(res.Questions) != 1 {
		t.Errorf("问题清单应落下来，got=%v", res.Questions)
	}
	if st.rounds[0].Stage != string(StageUnderstand) {
		t.Errorf("首轮阶段应为 understand，got=%q", st.rounds[0].Stage)
	}
	// 有问题要问 ⇒ 挂起等人答。
	if st.row.State != string(StateAwaitingAnswers) {
		t.Errorf("有未答问题应转 awaiting_answers，got=%q", st.row.State)
	}
}

// 只读纪律：PermissionMode=plan，且不 --resume（每轮新会话）。
func TestRunRoundIsReadOnlyAndFreshSession(t *testing.T) {
	st, wt := newFakeStore(), &fakeWorktrees{}
	st.rounds = []store.PRDRoundRow{{Round: 1, Stage: string(StageUnderstand)}}
	st.row.Round = 1
	st.row.Document = json.RawMessage(`{"type":"feature"}`)
	ag := &fakeAgent{results: []*agent.Result{ok(
		`{"type":"feature","document":{"problem":{"body":"阈值写死在 gate.go:31"}}}`)}}
	svc := newService(st, ag, wt)

	if _, err := svc.RunRound(context.Background(), RunRoundParams{PRDID: 1, UserID: 7}); err != nil {
		t.Fatalf("第二轮应成功: %v", err)
	}

	p := ag.calls[0]
	if p.PermissionMode != "plan" {
		t.Errorf("规划 agent 必须只读（plan 模式），got=%q", p.PermissionMode)
	}
	if p.SessionID == "" {
		t.Error("每轮必须给新 SessionID：worktree 已回收，续跑的会话会以为自己还在已删目录里")
	}
	if p.Resume {
		t.Error("不该 --resume：worktree 每轮回收（D10-8）")
	}
	if p.Dir == "" {
		t.Error("必须显式设 Dir，否则继承 serve 的 cwd（AGENTS.md §9 的 B2-3 事故）")
	}
	if wt.created != 1 || wt.removed != 1 {
		t.Errorf("阶段 B 应检出一次并回收一次，got created=%d removed=%d", wt.created, wt.removed)
	}
	if wt.lastBase != "main" {
		t.Errorf("应从默认分支检出，got=%q", wt.lastBase)
	}
	// 回收必须带上 mirror：git worktree remove 要在 bare mirror 里执行，
	// Mirror 丢了 git 就在 serve 的 cwd 里跑（AGENTS.md §9 B2-3 事故）。
	for i, m := range wt.removedMirrors {
		if m == "" {
			t.Errorf("第 %d 次回收没带 Mirror：git 会跑在 serve 的 cwd 里", i+1)
		}
	}
	if st.events[store.PRDPhasePlan] == 0 {
		t.Error("规划事件应落库，否则人看不到 agent 在干什么")
	}
}

// 解析失败这一轮等于没发生：不推进轮次、不落快照，但现场必须回收。
func TestRunRoundRejectsMalformedOutputWithoutPersisting(t *testing.T) {
	st, wt := newFakeStore(), &fakeWorktrees{}
	st.rounds = []store.PRDRoundRow{{Round: 1}}
	st.row.Round = 1
	ag := &fakeAgent{results: []*agent.Result{ok("我需要更多信息，没法给出 JSON。")}}
	svc := newService(st, ag, wt)

	_, err := svc.RunRound(context.Background(), RunRoundParams{PRDID: 1, UserID: 7})
	if err == nil {
		t.Fatal("输出里没有 JSON，应当报错")
	}
	if len(st.rounds) != 1 {
		t.Errorf("失败的一轮不该落进对话历史，got=%d 轮", len(st.rounds))
	}
	if st.row.Round != 1 {
		t.Errorf("失败不该推进轮次，got=%d", st.row.Round)
	}
	if wt.removed != wt.created {
		t.Errorf("失败路径也必须回收现场，got created=%d removed=%d", wt.created, wt.removed)
	}
	if st.busy {
		t.Error("失败后必须释放 processing，否则这份 PRD 卡到僵死阈值才解锁")
	}
}

// 并发起轮只能有一个赢（靠 store 的 CAS，不靠进程内锁）。
func TestRunRoundRespectsBusyFlag(t *testing.T) {
	st, wt := newFakeStore(), &fakeWorktrees{}
	st.busy = true
	svc := newService(st, &fakeAgent{}, wt)

	_, err := svc.RunRound(context.Background(), RunRoundParams{PRDID: 1, UserID: 7})
	if !errors.Is(err, store.ErrPRDBusy) {
		t.Errorf("已有轮次在跑时应报 ErrPRDBusy，got=%v", err)
	}
	if wt.created != 0 {
		t.Error("抢占失败不该已经检出仓库")
	}
}

// 人的回答与上一轮决策必须进下一轮 prompt，否则智能体会重问已拍板的问题。
func TestRunRoundCarriesAnswerAndDecisionsIntoPrompt(t *testing.T) {
	st, wt := newFakeStore(), &fakeWorktrees{}
	st.rounds = []store.PRDRoundRow{{Round: 1}, {Round: 2}}
	st.row.Round = 2
	st.row.Document = json.RawMessage(`{"type":"feature","decisions":[
		{"id":"D-1","kind":"option_choice","question":"阈值放哪","decision":"系统设置","reason":"管理员可改"}]}`)
	ag := &fakeAgent{results: []*agent.Result{ok(
		`{"type":"feature","document":{"goals":{"body":"G-1 阈值可配"}}}`)}}
	svc := newService(st, ag, wt)

	_, err := svc.RunRound(context.Background(), RunRoundParams{
		PRDID: 1, UserID: 7, Answer: "内存和磁盘都要算",
	})
	if err != nil {
		t.Fatalf("第三轮应成功: %v", err)
	}

	prompt := ag.calls[0].Prompt
	if !strings.Contains(prompt, "内存和磁盘都要算") {
		t.Error("人的回答没进 prompt：智能体看不到答案等于白问")
	}
	if !strings.Contains(prompt, "系统设置") {
		t.Error("已拍板的决策没进 prompt：会重问已决的问题（docs/10 §1.3 纪律 3）")
	}
	if !strings.Contains(prompt, st.row.OriginalInput) {
		t.Error("原文必须每轮都带上：它是所有推断的根")
	}
	if st.rounds[2].UserInput != "内存和磁盘都要算" {
		t.Errorf("人的回答应留痕在本轮记录里，got=%q", st.rounds[2].UserInput)
	}
}

// sentinelAnalysis 是塞进 §5 正文的哨兵：复核 prompt 里出现它就说明
// 泄露了作者的现状分析（review_test.go 用同一手法验 prompt 构造层，
// 这里验的是 Service 真的把该挡的挡住了）。
const sentinelAnalysis = "SENTINEL_ANALYSIS_SVC"

var reviewableDoc = `{"type":"feature",
	"codeAnalysis":{"status":"inferred","body":"阈值常量在 gate.go:31，` + sentinelAnalysis + `"},
	"criteria":[
	{"id":"AC-1","category":"happy_path","given":"阈值 75","when":"占用 80%",
	 "then":"返回 409","evidence":"HTTP 409","verify":"auto_test",
	 "goalRefs":["G-1"],"scenarioRefs":["S-1"]}],
	"goalList":[{"id":"G-1","text":"阈值可配","measure":"改完生效"}],
	"scenarioList":[{"id":"S-1","title":"改阈值","given":"已登录","when":"改 75","then":"80% 被拒"}]}`

// 对抗复核：不检出仓库（不给它读代码，免得被作者推理带偏），报告落库。
func TestRunReviewDoesNotCheckoutAndPersistsReport(t *testing.T) {
	st, wt := newFakeStore(), &fakeWorktrees{}
	st.row.Document = json.RawMessage(reviewableDoc)
	ag := &fakeAgent{results: []*agent.Result{ok(
		`{"findings":[{"kind":"malicious_compliance","title":"只写测试不实现",
		   "detail":"实现里直接 return 409，不读配置也能过 AC-1","acRefs":["AC-1"]}],
		  "verdict":"AC-1 有洞"}`)}}
	svc := newService(st, ag, wt)

	rep, err := svc.RunReview(context.Background(), 1, 7)
	if err != nil {
		t.Fatalf("复核应成功: %v", err)
	}

	if wt.created != 0 {
		t.Error("对抗复核不该检出仓库：它只看 §1–§4 与 §7")
	}
	if len(rep.Findings) != 1 || rep.Findings[0].Kind != FindingMalicious {
		t.Fatalf("恶意合规的发现没解析出来: %+v", rep.Findings)
	}
	if rep.Findings[0].Disposition != "" {
		t.Error("处置必须留空由人填 —— 让复核自己判「这条不用改」等于没复核")
	}
	if rep.RawText == "" {
		t.Error("原文必须留下：docs/10 §5.5 要求报告原样进附录 C")
	}
	if len(st.row.ReviewReport) == 0 {
		t.Error("复核报告应落库")
	}
	if st.events[store.PRDPhasePlanReview] == 0 {
		t.Error("复核事件应以 plan-review 落库，与规划事件分开")
	}
	// prompt 不该泄露 §5/§6 的正文 —— 只喂 §1–§4 与 §7。用哨兵串而不是
	// 搜「现状分析」这四个字：prompt 本身就要告诉 agent「你看不到作者的
	// 现状分析」，搜关键词会命中那句说明。
	if strings.Contains(ag.calls[0].Prompt, sentinelAnalysis) {
		t.Error("复核 prompt 泄露了 §5 现状分析正文：会被作者的推理带偏")
	}
}

// 拆分阶段产出的任务块要带上仓库名：一键生成读 task_blocks 建图，
// Repo 空了就不知道往哪个仓库下单。
func TestRunRoundStoresRepoInTaskBlocks(t *testing.T) {
	st, wt := newFakeStore(), &fakeWorktrees{}
	st.rounds = []store.PRDRoundRow{{Round: 1}, {Round: 2}, {Round: 3}}
	st.row.Round = 3
	st.row.Document = json.RawMessage(`{"type":"feature"}`)
	// AC 必须与任务同在一份快照里 —— 解析层会拦「引用不存在的 AC」。
	ag := &fakeAgent{results: []*agent.Result{ok(
		`{"type":"feature","document":{
		  "goalList":[{"id":"G-1","text":"阈值可配","measure":"改完生效"}],
		  "scenarioList":[{"id":"S-1","title":"改阈值","given":"已登录","when":"改 75","then":"80% 被拒"}],
		  "criteria":[{"id":"AC-1","category":"happy_path","given":"阈值 75","when":"占用 80%",
		    "then":"返回 409","evidence":"HTTP 409","verify":"auto_test",
		    "goalRefs":["G-1"],"scenarioRefs":["S-1"]}],
		  "tasks":[
		  {"key":"T1","title":"阈值改配置项","kind":"feature",
		   "description":"改常量为读设置；交付测试证明可配且非法值被拒",
		   "acceptance":["AC-1"],"estimate":{"lines":120,"files":3}}]}}`)}}
	svc := newService(st, ag, wt)

	if _, err := svc.RunRound(context.Background(), RunRoundParams{
		PRDID: 1, UserID: 7, Stage: StageSplit,
	}); err != nil {
		t.Fatalf("拆分阶段应成功: %v", err)
	}

	blocks, err := ParseTaskBlocks(st.row.TaskBlocks)
	if err != nil {
		t.Fatalf("解析任务块失败: %v", err)
	}
	if blocks.Repo != "acme/lathe" {
		t.Errorf("任务块必须带仓库名，got=%q", blocks.Repo)
	}
	if blocks.Version != BlocksVersion {
		t.Errorf("格式版本应落下来，got=%d", blocks.Version)
	}
	if len(blocks.Tasks) != 1 || blocks.Tasks[0].Key != "T1" {
		t.Errorf("任务块内容丢了: %+v", blocks.Tasks)
	}
}

// 没有 AC 时复核无从下手，应当拒绝而不是烧一次 token。
func TestRunReviewRefusesWithoutCriteria(t *testing.T) {
	st := newFakeStore()
	st.row.Document = json.RawMessage(`{"type":"feature"}`)
	svc := newService(st, &fakeAgent{}, &fakeWorktrees{})

	_, err := svc.RunReview(context.Background(), 1, 7)
	if !errors.Is(err, ErrStageUnavailable) {
		t.Errorf("没有 AC 应报 ErrStageUnavailable，got=%v", err)
	}
}

// 自检不过时返回逐条发现，且状态不动 —— 前端要按 Code 跳到出问题的节。
func TestSubmitForReviewBlocksOnChecklist(t *testing.T) {
	st := newFakeStore()
	st.row.Document = json.RawMessage(reviewableDoc)
	svc := newService(st, &fakeAgent{}, &fakeWorktrees{})

	rep, err := svc.SubmitForReview(context.Background(), 1, 7)
	if err == nil {
		t.Fatal("没跑对抗复核、没有任务块，自检不该通过")
	}
	if rep == nil || len(rep.Blocking) == 0 {
		t.Fatal("应返回逐条阻塞项供前端定位")
	}
	if st.row.State != string(StateDrafting) {
		t.Errorf("自检不过状态不该动，got=%q", st.row.State)
	}

	var codes []string
	for _, f := range rep.Blocking {
		codes = append(codes, f.Code)
	}
	joined := strings.Join(codes, ",")
	if !strings.Contains(joined, CodeNoReviewReport) {
		t.Errorf("必须拦住「没跑对抗复核」（D10-6 必跑），got codes=%v", codes)
	}
}
