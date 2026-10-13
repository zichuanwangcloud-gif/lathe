package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
	"github.com/zichuanwangcloud-gif/lathe/internal/integration/github"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// fakeMemory 是 FailureMemory 的内存实现，供注入相关的用例。
//
// 刻意不用真实 Postgres：注入的正确性（有没有注入、注入了什么、上限是多少）
// 与存储实现无关，塞进库里只会让这些用例多花几秒、多一处可能跳过的依赖。
// store 侧的读写另有 internal/store/memory_test.go 覆盖。
type fakeMemory struct {
	// stored 按定位键保存历史失败（读路径用）
	stored map[store.FailureLookup]*store.FailureDigest
	// written 记录所有写入（写路径断言用）
	written []store.FailureDigest
	// lookups 记录所有查询（断言「谁被查过」用）
	lookups []store.FailureLookup
	// err 非空时让读取报错（验证「查不到不改变流程」）
	err error
}

func newFakeMemory() *fakeMemory {
	return &fakeMemory{stored: map[store.FailureLookup]*store.FailureDigest{}}
}

func (f *fakeMemory) LatestFailureDigest(ctx context.Context, lk store.FailureLookup, excludeTaskID int64) (*store.FailureDigest, error) {
	f.lookups = append(f.lookups, lk)
	if f.err != nil {
		return nil, f.err
	}
	d, ok := f.stored[lk]
	if !ok {
		return nil, nil // 没有历史失败是常态，不是错误
	}
	if excludeTaskID != 0 && d.TaskID == excludeTaskID {
		return nil, nil
	}
	return d, nil
}

func (f *fakeMemory) InsertFailureDigest(ctx context.Context, d store.FailureDigest, lk store.FailureLookup) error {
	f.written = append(f.written, d)
	f.stored[lk] = &d
	return nil
}

// ★ AC3 的记忆闭环，前半：任务失败 → digest 落库。
//
// 用一个会在验证阶段挂掉的任务（agent 不产出任何改动 → implement_no_changes
// 其实更早，这里用验证失败更贴近真实），断言 fail() 之后 fakeMemory 里
// 真的多了一条记录，且带着失败阶段与原因。
func TestFailureMemoryRecordedOnFailure(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issue: demoIssue()}
	ag := &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象","question":""}`},
			// 实现阶段直接失败：agent 报错退出
			{Success: false, IsError: true, Subtype: "error_during_execution", Text: "改不动"},
		},
	}
	mem := newFakeMemory()
	p := newPipeline(t, m, lin, &fakeGitHub{}, ag, &fakeNotifier{})
	p.Memory = mem

	err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
	})
	if err == nil {
		t.Fatal("这条链路应当失败")
	}

	// 状态确实落成了 failed
	final, gerr := m.Get(context.Background(), taskID)
	if gerr != nil {
		t.Fatalf("Get 失败: %v", gerr)
	}
	if final.State != task.StateFailed {
		t.Fatalf("终态 = %s，期望 failed", final.State)
	}

	// 记忆真的落了库
	if len(mem.written) != 1 {
		t.Fatalf("应写下 1 条失败记忆，实际 %d 条: %+v", len(mem.written), mem.written)
	}
	d := mem.written[0]
	if d.TaskID != taskID {
		t.Errorf("记忆的 task_id = %d，期望 %d", d.TaskID, taskID)
	}
	// 实现阶段的失败（agent 报 IsError）走 StageImplementIncomplete
	if d.Stage != string(StageImplementIncomplete) {
		t.Errorf("记忆的阶段 = %q，期望 %q", d.Stage, StageImplementIncomplete)
	}
	if !strings.Contains(d.Reason, "失败") && !strings.Contains(d.Reason, "实现") {
		t.Errorf("记忆的原因应是人读的一句话，得到 %q", d.Reason)
	}
}

// reclaimWorktree 把一次已失败任务的现场按**真实路径**回收掉：先删盘，
// 再让任务行不再认领它。
//
// 这不是测试自造的脚手架，而是生产上的必经步骤：queue 在 plan.Fresh 时
// 调 Discard 删盘，但因为 Discard 只动磁盘不动任务行，Fresh 重建会撞上
// 自己留在 worktree_path 里的旧路径（ClaimedWorktreePaths 把本任务算作
// 认领者），Create 于是拒绝接管 —— 只有 T6 收割机落账（ClaimForReap
// 的 commit 阶段置空 worktree_path）之后，重建才走得通。夹具走这条真实
// 路径，顺带守住「夹具越简单越好的前提是它还在测那条真实路径」。
func reclaimWorktree(t *testing.T, ctx context.Context, m *task.Machine, p *Pipeline, repo RepoConfig, tk *task.Task) {
	t.Helper()
	if tk.WorktreePath == nil || *tk.WorktreePath == "" {
		return
	}
	path, branch := *tk.WorktreePath, ""
	if tk.BranchName != nil {
		branch = *tk.BranchName
	}
	p.Worktrees.Discard(ctx, repo.ProviderRepo, path, branch)

	// 落账用任务的 updated_at 做乐观并发的快照（收割机的做法）
	cur, err := m.Get(ctx, tk.ID)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	ok, err := m.ClaimForReap(ctx, tk.ID, path, cur.UpdatedAt, true, "system",
		map[string]any{"kind": "worktree_reaped", "why": "测试夹具回收现场"})
	if err != nil || !ok {
		t.Fatalf("回收现场失败: ok=%v err=%v（pendingReap 之后任务行被动过？）", ok, err)
	}
}

// ★ AC3 的记忆闭环，后半：同 issue 重试 → 提示词含 digest 摘要；
// 无历史失败 → 提示词不变。
//
// 这条把两半缝在一起验：先让一次执行失败写下记忆，再在同一 task 行上
// 从「全新会话」重入（Fresh 重建 → 走 ImplementPrompt 那条路径），断言
// 第二次的实现提示词里出现了第一次的失败摘要。同时用一条对照组断言
// 「没记忆时提示词逐字不变」—— 少了这一条，一个「无脑往所有提示词里塞
// 一段」的实现也能让前半通过。
//
// 断言落在**实现**那一次调用上（不是「任意一次」）：分诊也注入，
// 「至少一次带上」的写法会让「实现提示词没注入」这个回归静默通过 ——
// 那正是这条用例要守的东西。
func TestFailureMemoryInjectedOnRetry(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)
	ctx := context.Background()

	mem := newFakeMemory()

	lin := &fakeLinear{issue: demoIssue()}
	ag := &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象","question":""}`},
			{Success: false, IsError: true, Subtype: "error_during_execution", Text: "第一轮挂了"},
		},
	}
	p := newPipeline(t, m, lin, &fakeGitHub{}, ag, &fakeNotifier{})
	p.Memory = mem

	// 第一轮：失败，写下记忆
	if err := p.Execute(ctx, ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
	}); err == nil {
		t.Fatal("第一轮应当失败")
	}
	if len(mem.written) != 1 {
		t.Fatalf("第一轮应写下 1 条记忆，实际 %d", len(mem.written))
	}
	firstStage := mem.written[0].Stage
	if firstStage != string(StageImplementIncomplete) {
		t.Fatalf("第一轮的失败阶段 = %q，期望 %q（夹具没走到实现阶段？）",
			firstStage, StageImplementIncomplete)
	}

	// 读出真实定位键：注入用它查表。fakeMemory 按 lookup 存，
	// 所以第一次写入时记下的键就是重试时要查的键。
	if len(mem.lookups) == 0 {
		t.Fatal("第一轮应查询过记忆（注入点）")
	}
	realKey := mem.lookups[0]
	if _, ok := mem.stored[realKey]; !ok {
		t.Fatalf("第一轮写下的记忆应能按该键查到，键=%+v", realKey)
	}

	// 第二轮：按真实的重建路径走 —— 回收现场（收割机落账）→ 转回 queued
	// → 以 Fresh 计划重派同一任务行。
	tk, err := m.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	reclaimWorktree(t, ctx, m, p, repo, tk)

	if _, err := m.Transition(ctx, taskID, task.StateQueued, "user:1", nil); err != nil {
		t.Fatalf("转回 queued 失败: %v", err)
	}

	ag2 := &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象","question":""}`},
			// 这一轮也让它失败 —— 只关心「提示词里有没有历史失败」，
			// 成功与否不影响那条断言，而失败能顺带覆盖 fail 路径。
			{Success: false, IsError: true, Subtype: "error_during_execution", Text: "第二轮也挂了"},
		},
	}
	p.Agent = ag2
	if err := p.Execute(ctx, ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
		Retry: &RetryPlan{Fresh: true, Entry: EntryTriage, Reasons: []string{"测试指定重建"}},
	}); err == nil {
		t.Fatal("第二轮也应当失败（夹具刻意如此）")
	}

	// 前置断言：第二轮**确实走进了实现阶段**。少了它，一个「第二天挂在
	// 建工作区上」的夹具会让后面所有断言都变成空转（这正是这条用例
	// 修前的状态：第二轮死在 create_worktree，注入删掉也照样绿）。
	if len(ag2.calls) < 2 {
		t.Fatalf("第二轮应至少走到实现阶段（分诊 + 实现两次调用），实际 %d 次: %s",
			len(ag2.calls), promptDump(ag2.calls))
	}
	implPrompt := ag2.calls[1].Prompt
	// 真断言（不是 log）：第二次调用必须是**实现**那一次，而不是又一次
	// 分诊 —— 分诊也注入，若夹具退回「第二轮死在建工作区」，这条会在
	// 注入断言之前先红，失败信息也直接指出是夹具没走到实现。
	if !strings.Contains(implPrompt, "当前已在分支") {
		t.Fatalf("第二次调用应是实现阶段（提示词含「当前已在分支 …」），实际：\n%s", implPrompt)
	}
	// 断言：第二轮的实现提示词带上了历史失败。
	if !strings.Contains(implPrompt, "这个需求此前失败过") {
		t.Errorf("重试时实现提示词应含历史失败段，实际：\n%s", implPrompt)
	}
	// 且注入的必须是「历史」那一份（第一轮的失败阶段），不是本次的
	if !strings.Contains(implPrompt, firstStage) {
		t.Errorf("注入的内容应是第一轮那次失败的阶段 %q，实际：\n%s", firstStage, implPrompt)
	}
	if len(mem.written) != 2 {
		t.Errorf("第二轮失败也应写下记忆，实际共 %d 条", len(mem.written))
	}
}

// ★ EntryImplement 重入（现场保留）同样要注入。
//
// 与上一条的区别：这里**不重建**现场，任务在 failed → queued 之后以
// `Retry{Entry: EntryImplement}` 直接重入实现阶段，会话不可续 → 走
// `ReentryImplementPrompt` 那条**提示词**分支。
//
// ⚠ 注意它走的 phase 是 `implement` 而不是 `implement-reentry`：后者是
// resume 失败后的降级分支（见 TestFailureMemoryInjectedOnResumeDegrade）。
// 这条用例守的是「EntryImplement 重入也有注入」，不是降级分支。
func TestFailureMemoryInjectedOnImplementEntry(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)
	ctx := context.Background()

	mem := newFakeMemory()
	lin := &fakeLinear{issue: demoIssue()}
	ag := &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象","question":""}`},
			{Success: false, IsError: true, Subtype: "error_during_execution", Text: "第一轮挂了"},
		},
	}
	p := newPipeline(t, m, lin, &fakeGitHub{}, ag, &fakeNotifier{})
	p.Memory = mem

	if err := p.Execute(ctx, ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
	}); err == nil {
		t.Fatal("第一轮应当失败")
	}
	if len(mem.written) != 1 {
		t.Fatalf("第一轮应写下 1 条记忆，实际 %d", len(mem.written))
	}
	firstStage := mem.written[0].Stage

	// 现场保留（这条正是「保留现场 ≠ 不能回收」的另一半：能续就直接续）
	if _, err := m.Transition(ctx, taskID, task.StateQueued, "user:1", nil); err != nil {
		t.Fatalf("转回 queued 失败: %v", err)
	}
	tk, err := m.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if tk.WorktreePath == nil {
		t.Fatal("这条用例要求现场保留（ReentryImplementPrompt 的前提）")
	}
	// 会话凭据清掉：走「会话丢失 → 同工作区新会话」的那一支，
	// 提示词必定是 ReentryImplementPrompt。
	if err := m.SetSessionID(ctx, taskID, ""); err != nil {
		t.Fatalf("清会话失败: %v", err)
	}

	ag2 := &fakeAgent{
		results: []*agent.Result{
			{Success: false, IsError: true, Subtype: "error_during_execution", Text: "第二轮也挂了"},
		},
	}
	p.Agent = &probeAgent{inner: ag2}
	if err := p.Execute(ctx, ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
		Retry: &RetryPlan{Entry: EntryImplement, Reasons: []string{"测试指定续跑实现"}},
	}); err == nil {
		t.Fatal("第二轮也应当失败（夹具刻意如此）")
	}

	if len(ag2.calls) != 1 {
		t.Fatalf("续跑实现阶段应只调用一次 agent（无分诊），实际 %d 次: %s",
			len(ag2.calls), promptDump(ag2.calls))
	}
	got := ag2.calls[0].Prompt
	// 先确认走的确实是 reentry 那支（不是 resume：resume 分支排除了自记录）
	if !strings.Contains(got, "续跑场景") {
		t.Fatalf("这条用例要走 ReentryImplementPrompt，实际提示词：\n%s", got)
	}
	if !strings.Contains(got, "这个需求此前失败过") {
		t.Errorf("reentry 路径的提示词应含历史失败段，实际：\n%s", got)
	}
	if !strings.Contains(got, firstStage) {
		t.Errorf("注入的内容应是第一轮的失败阶段 %q，实际：\n%s", firstStage, got)
	}
}

// ★ resume 失败降级那条分支（phase = "implement-reentry"）也要注入。
//
// 这是**唯一**会打出 `implement-reentry` 这个 phase 的路径：第一次调用是
// resume（提示词里已经带着本任务的失败原因，因此排除自记录），会话数据
// 丢了之后流水线降级为「同工作区新会话」，而新会话对「上次死在哪」一无所知
// —— 注入必须发生在这第二次调用上。
//
// 为什么单列一条：`implement-reentry` 与 `implement` 是白名单里的两个名字，
// 只留 `implement` 时前面的用例全都照样绿（它们命中的都是 `implement`），
// 这条是那个漏点的看守。
func TestFailureMemoryInjectedOnResumeDegrade(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)
	ctx := context.Background()

	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{pr: &github.PullRequest{URL: "https://x/pr/12", Number: 12}}
	// 第一次调用直接报错（模拟 claude 的会话数据已不在本机），第二次成功
	ag := &fakeAgent{
		errs: []error{errors.New("No conversation found with session ID"), nil},
		results: []*agent.Result{
			nil,
			{Success: true, Text: "新会话收尾完成"},
		},
		mutate: []func(string) error{
			nil,
			func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "README.md"),
					[]byte("# src\n\n半成品 + 新会话补完\n"), 0o644)
			},
		},
	}
	mem := newFakeMemory()
	evts := &captureEvents{}
	p := newPipeline(t, m, lin, gh, &probeAgent{inner: ag}, &fakeNotifier{})
	p.Memory = mem
	p.AgentEvents = evts

	// 造一个「实现阶段失败过、现场保留、有会话 ID」的任务，再把它摆成
	// 「人工续跑」的形态（prepareFailedTask 会建工作区 + 落会话 ID）。
	prepareFailedTask(t, p, m, taskID, src, repo, StageImplementRun, false)

	// 预置本任务那条失败记忆：resume 那一次应当排除它（失败原因已经在
	// 提示词里），而**降级后的新会话**必须读到它。
	//
	// 定位键从任务行现取 —— 注入侧就是按这几个字段查的，写别的键等于
	// 写了也读不到。
	const wantMarker = "上一次：main.go:12: undefined: greet"
	tk, err := m.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	realKey := store.FailureLookup{
		UserID: tk.UserID, RepoID: tk.RepoID,
		TrackerProvider: tk.TrackerProvider, ExternalKey: tk.ExternalKey,
	}
	if err := mem.InsertFailureDigest(ctx, store.FailureDigest{
		TaskID: taskID, Stage: string(StageImplementIncomplete),
		Reason: "实现中断", Excerpt: wantMarker,
	}, realKey); err != nil {
		t.Fatalf("铺历史记忆失败: %v", err)
	}
	mem.written = nil // 上面那次插入只是铺记忆，不算「本次执行写下的」

	if err := p.Execute(ctx, ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777",
		Retry: &RetryPlan{Entry: EntryImplement, ResumeSession: true, Reasons: []string{"续跑原会话"}},
	}); err != nil {
		t.Fatalf("降级续跑应成功: %v", err)
	}

	if len(ag.calls) != 2 {
		t.Fatalf("应两次调用 agent（resume 失败 + 降级新会话），实际 %d: %s",
			len(ag.calls), promptDump(ag.calls))
	}
	if !ag.calls[0].Resume {
		t.Fatal("第一次调用应是 resume 尝试（否则这条用例没走到降级分支）")
	}
	// resume 那次：本任务的失败原因已在提示词里，不该再注入历史段
	if strings.Contains(ag.calls[0].Prompt, "这个需求此前失败过") {
		t.Errorf("resume 那次应排除自记录，实际注入了：\n%s", ag.calls[0].Prompt)
	}
	// 降级那次（phase = implement-reentry）：必须注入 —— 新会话没有第一轮
	// 的记忆，这份历史就是它唯一的线索。
	got := ag.calls[1].Prompt
	if ag.calls[1].Resume {
		t.Fatal("降级后不应再 resume")
	}
	if !strings.Contains(got, "这个需求此前失败过") {
		t.Errorf("降级新会话的提示词应含历史失败段，实际：\n%s", got)
	}
	if !strings.Contains(got, wantMarker) {
		t.Errorf("注入的应是那段历史输出摘要 %q，实际：\n%s", wantMarker, got)
	}

	// phase 名是**跨分支的接口约定**（injectFailureMemory 的白名单、A2 的
	// review 二轮都按它写），而它同时喂给 newEventSink 作事件流的阶段标签
	// —— 把调用点的 phase 改成 "implement" 会让注入照旧、事件标签静默变样。
	// 事件是按批落库的（见 captureEvents.shownPhases 里的说明），所以这里
	// 断言的是「**展示侧能看到的**阶段名」，正是这个约定真正关心的那件事。
	// 取前两个（后面还有验证阶段自己的批次）：这次的两次调用必须是
	// implement 与 implement-reentry，按这个顺序。
	shown := evts.shownPhases()
	if len(shown) < 2 || shown[0] != "implement" || shown[1] != "implement-reentry" {
		t.Errorf("前两次 agent 调用的阶段标签应为 [implement implement-reentry]，得到 %v", shown)
	}
}

// probeAgent 在每次 Run 时先投一条事件再交给内层假件。
//
// 存在的理由：「阶段名」既是注入决策的依据，也是事件流的标签（喂给
// newEventSink）。要断言后者，agent 必须真的产出事件 —— 而 fakeAgent
// 默认不投（不投的话 EventSink 一个批次都不落，事件流的阶段完全不可观测）。
// 用装饰器而不是改 fakeAgent：改公共假件会影响所有可见性用例的计数断言。
type probeAgent struct{ inner *fakeAgent }

func (a *probeAgent) Run(ctx context.Context, p agent.RunParams) (*agent.Result, error) {
	if p.OnEvent != nil {
		p.OnEvent(agent.Event{Type: agent.EventType("probe"), Raw: []byte(`{}`)})
	}
	return a.inner.Run(ctx, p)
}

// ★ AC3 的对照：没有历史失败时提示词逐字不变。
//
// 少了这一条，一个「无条件往所有提示词末尾追加一段模板」的实现也能让
// 上一个用例通过 —— 而那种实现会把无关内容灌给每一个全新任务。
func TestFailureMemoryAbsentLeavesPromptUnchanged(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issue: demoIssue()}
	ag := &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象","question":""}`},
			{Success: false, IsError: true, Subtype: "error_during_execution", Text: "挂了"},
		},
	}
	mem := newFakeMemory() // 空：没有任何历史失败
	p := newPipeline(t, m, lin, &fakeGitHub{}, ag, &fakeNotifier{})
	p.Memory = mem

	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
	}); err == nil {
		t.Fatal("这条链路应当失败")
	}

	if len(ag.calls) < 2 {
		t.Fatalf("至少应有分诊与实现两次调用，实际 %d", len(ag.calls))
	}
	// 分诊 prompt 应逐字等于未注入的版本
	if got, want := ag.calls[0].Prompt, TriagePrompt(demoIssue().Context()); got != want {
		t.Errorf("无历史失败时分诊提示词应原样不变：\n得到: %q\n期望: %q", got, want)
	}
	// 实现 prompt：无记忆时也不该多出任何注入段落
	if strings.Contains(ag.calls[1].Prompt, "此前失败过") {
		t.Errorf("无历史失败时不该注入任何段落:\n%s", ag.calls[1].Prompt)
	}
}

// ★ AC4：超大失败输出注入时不超上限。
//
// 上限的意义是「防把提示词撑爆」（brief 第 3 条「注入要克制」）——
// 所以断言必须是「最终长度有界」，而不是「某处调过截断函数」。
func TestFailureMemoryTruncatesLargeExcerpt(t *testing.T) {
	// 造一份远超上限的验证输出（模拟把整个构建日志灌进来）：内容用中文，
	// 让「按字节硬切」有机会落进某个字的中间 —— 那正是要防的形态。
	huge := strings.Repeat("编译错误：某个很长的行\n", 5000)
	if len(huge) <= maxFailureExcerpt {
		t.Fatalf("夹具不够大：%d 字节，上限 %d", len(huge), maxFailureExcerpt)
	}

	got := renderFailureMemory(&store.FailureDigest{
		Stage:   "verify_failed",
		Reason:  strings.Repeat("原因", 5000), // 这一份也超 maxDigestReason
		Excerpt: huge,
		At:      "2026-01-01 00:00",
	})
	if got == "" {
		t.Fatal("有内容时不该渲染成空串")
	}

	// 上限 = 摘要上限 + 原因上限 + 渲染骨架本身（标题、说明、时间、围栏）
	// 的宽松余量。用「骨架余量」而不是精确值：精确匹配会把注释里的措辞
	// 改动变成测试失败，而这里真正要守住的是「有界」。
	const skeletonAllowance = 800
	limit := maxFailureExcerpt + maxDigestReason + skeletonAllowance
	if len(got) > limit {
		t.Errorf("注入段落长度 = %d 字节，超过上限 %d（提示词会被撑爆）", len(got), limit)
	}
	// 截断必须按 rune 边界回退：切坏多字节字符的产物落库会撞
	// SQLSTATE 22021（AGENTS.md §9），而它**此刻**已经是给 agent 看的
	// 提示词内容 —— 注入侧先坏，落库侧再炸。
	if !utf8.ValidString(got) {
		t.Errorf("注入段落不是合法 UTF-8（截断切坏了多字节字符），长度 %d", len(got))
	}
	// 被截断时必须留下痕迹 —— 静默截断会让人以为失败输出就这么长
	if !strings.Contains(got, "已截断") {
		t.Error("超长摘要被截断时应留下明显标记")
	}
	// 取的是尾部：结尾那几行（结论）必须在
	if !strings.HasSuffix(got, "```\n") {
		t.Errorf("摘要应保留原串尾部，渲染以围栏收尾，实际结尾：%q", got[len(got)-40:])
	}
}

// 注入的措辞必须把「这是历史，不是你本次的结果」讲清楚 ——
// 否则 agent 会去修一个可能已经修好的问题。
func TestFailureMemoryFramingIsHistorical(t *testing.T) {
	got := renderFailureMemory(&store.FailureDigest{
		Stage:  "verify_failed",
		Reason: "build 挂了",
		At:     "2026-01-01 00:00",
	})
	for _, want := range []string{
		"这不是你本次执行的结果",   // 明确否定「这是我刚犯的错」
		"仍然成立",          // 给出行动指引
		"verify_failed", // 阶段代码原样透出，agent 能对号入座
		"build 挂了",      // 原因
	} {
		if !strings.Contains(got, want) {
			t.Errorf("注入措辞应含 %q，实际：\n%s", want, got)
		}
	}
}

// 读取失败绝不改变流程：记忆是辅助信息，取不到就当作没有。
func TestFailureMemoryReadErrorIsNonFatal(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issue: demoIssue()}
	ag := &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象","question":""}`},
			{Success: false, IsError: true, Subtype: "error_during_execution", Text: "挂了"},
		},
	}
	mem := newFakeMemory()
	mem.err = context.DeadlineExceeded // 模拟查询超时/库不可达
	p := newPipeline(t, m, lin, &fakeGitHub{}, ag, &fakeNotifier{})
	p.Memory = mem

	err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
	})
	if err == nil {
		t.Fatal("这条链路应当失败（实现阶段挂了）")
	}
	// 失败原因必须是实现阶段的问题，不能是「读记忆失败」
	if !strings.Contains(err.Error(), "实现") {
		t.Errorf("失败原因不该被记忆读取的错误顶掉，得到: %v", err)
	}
}

// 续跑路径（resume 原会话）要**排除本任务自己**的那条记录：那份失败原因
// 已经由 ResumeFixPrompt 带在提示词里，再注入一遍是同义反复。
//
// 但排除自记录 ≠ 不注入：同一单子可能有更早的任务行（issue 关掉重开后
// 新建的那行），它的失败对当前这次执行是真历史。
func TestFailureMemoryExcludesSelfOnResumePath(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)
	ctx := context.Background()

	mem := newFakeMemory()
	p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, &fakeGitHub{}, &fakeAgent{}, &fakeNotifier{})
	p.Memory = mem

	// 先跑一轮：让失败记忆里出现【本任务】的记录，同时任务落到 failed
	ag := &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象","question":""}`},
			{Success: false, IsError: true, Subtype: "error_during_execution", Text: "挂了"},
		},
	}
	p.Agent = ag
	p.Verifications = &fakeVerifications{}
	if err := p.Execute(ctx, ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
	}); err == nil {
		t.Fatal("第一轮应当失败")
	}
	if len(mem.written) != 1 {
		t.Fatalf("第一轮应写下 1 条记忆，实际 %d", len(mem.written))
	}
	selfStage := mem.written[0].Stage

	// 手动把任务摆成「验证未通过、可 resume」的续跑形态：
	// 失败阶段 = verify_failed，且持有 agent_session_id。
	if _, err := m.Transition(ctx, taskID, task.StateQueued, "user:1", &task.TransitionOpts{
		FailureStage:  strPtr(string(StageVerifyFailed)),
		FailureReason: strPtr("验证未通过：build 挂了"),
	}); err != nil {
		t.Fatalf("转 queued 失败: %v", err)
	}
	if err := m.SetSessionID(ctx, taskID, "sess-resume"); err != nil {
		t.Fatalf("设会话失败: %v", err)
	}
	// 记录这一轮的查询，供下面断言「查过记忆」
	mem.lookups = nil
	ag2 := &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象","question":""}`},
			{Success: false, IsError: true, Subtype: "error_during_execution", Text: "第二轮也挂"},
		},
	}
	p.Agent = ag2
	// 这一轮成功与否不影响断言 —— 只验提示词里出现了什么
	_ = p.Execute(ctx, ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
		Retry: &RetryPlan{Entry: EntryImplement, ResumeSession: true, Reasons: []string{"测试指定续跑"}},
	})

	// 续跑路径必须走 ResumeFixPrompt（本任务失败原因已在其中）
	if len(ag2.calls) == 0 {
		t.Fatal("第二轮应有 agent 调用")
	}
	implCall := ag2.calls[0]
	if !strings.Contains(implCall.Prompt, "验证未通过：build 挂了") {
		t.Errorf("续跑提示词应带本任务的失败原因:\n%s", implCall.Prompt)
	}
	// 且**不**再注入一条同样内容的「历史失败」段（本任务是自记录，被排除）
	if strings.Contains(implCall.Prompt, "这个需求此前失败过") {
		t.Errorf("续跑路径应排除本任务自记录，不该注入历史段:\n%s", implCall.Prompt)
	}

	// 直接验排除语义：记忆里只有本任务那一条，按 taskID 排除后应读不到。
	// （fakeMemory 的 LatestFailureDigest 会挡掉 TaskID == excludeTaskID 的行。）
	if len(mem.lookups) == 0 {
		t.Fatal("续跑时也应查询记忆（用来找更早任务行的历史）")
	}
	d, gerr := mem.LatestFailureDigest(ctx, mem.lookups[0], taskID)
	if gerr != nil {
		t.Fatalf("查询失败: %v", gerr)
	}
	if d != nil {
		t.Errorf("排除自记录后不该读到本任务那条（stage=%s），得到 %+v", selfStage, d)
	}
}

// pipeline 未接 Memory（未装配/老部署/测试）时，提示词必须一字不差。
func TestFailureMemoryNilIsNoop(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issue: demoIssue()}
	ag := &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象","question":""}`},
			{Success: false, IsError: true, Subtype: "error_during_execution", Text: "挂了"},
		},
	}
	p := newPipeline(t, m, lin, &fakeGitHub{}, ag, &fakeNotifier{})
	// 刻意不设 p.Memory

	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
	}); err == nil {
		t.Fatal("这条链路应当失败")
	}
	if got, want := ag.calls[0].Prompt, TriagePrompt(demoIssue().Context()); got != want {
		t.Error("未接 Memory 时分诊提示词应逐字不变")
	}
}

// promptDump 把一组调用的提示词拼起来，失败信息里好看清实际注入了什么。
func promptDump(calls []agent.RunParams) string {
	var b strings.Builder
	for i, c := range calls {
		b.WriteString("\n--- 第 ")
		b.WriteString(string(rune('1' + i)))
		b.WriteString(" 次 ---\n")
		b.WriteString(c.Prompt)
	}
	return b.String()
}

// 测试用的假凭据。故意做成「看起来像真的」：GitHub PAT 的形态
// （ghp_ + 36 位）与验证子进程环境里一个敏感变量名下的值。
const (
	fakeGitHubToken   = "ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	fakeEnvSecretName = "LATHE_FAKE_VERIFY_API_TOKEN"
	fakeEnvSecretVal  = "env-secret-q9w8e7r6t5y4u3i2"
	excerptMarker     = "MARKER-OUT-7781"
)

// ★ MAJOR-1 的端到端：验证失败后，digest 的 excerpt 必须真的含失败步骤的
// **输出**（不只是步骤名 + ✓/✗ + 耗时），且输出里的凭据已被脱敏。
//
// 夹具：agent 交一个「测试对、实现错」的改动（heavy 档，绿阶段必挂），
// 失败的测试把一个 GitHub token 形态的串和验证子进程环境里的一个敏感
// 变量值打进输出 —— 两种泄漏形态各一个。修复回路关着（MaxFixAttempts=0），
// 首轮验证失败就落 verify_failed。
func TestFailureMemoryVerifyExcerptCarriesRedactedOutput(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)
	// 验证子进程继承宿主环境（verify.go 的 stepEnv），这正是要防的泄漏面
	t.Setenv(fakeEnvSecretName, fakeEnvSecretVal)

	testSrc := "package main\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\n" +
		"func TestGreet(t *testing.T) {\n" +
		"\tt.Logf(\"env=%s\", os.Getenv(\"" + fakeEnvSecretName + "\"))\n" +
		"\tif greet() != \"hello\" {\n" +
		"\t\tt.Fatalf(\"" + excerptMarker + " 中文输出 got %q auth=" + fakeGitHubToken + "\", greet())\n" +
		"\t}\n}\n"

	lin := &fakeLinear{issue: demoIssue()}
	ag := &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象","question":""}`},
			{Success: true, Text: "实现了（答错了）"},
		},
		mutate: []func(string) error{
			nil,
			func(dir string) error {
				if err := os.WriteFile(filepath.Join(dir, "main_test.go"), []byte(testSrc), 0o644); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(dir, "fix.go"),
					[]byte("package main\n\nfunc greet() string { return \"wrong\" }\n"), 0o644)
			},
		},
	}
	mem := newFakeMemory()
	p := newPipeline(t, m, lin, &fakeGitHub{}, ag, &fakeNotifier{})
	p.Memory = mem

	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
	}); err == nil {
		t.Fatal("绿阶段应当失败")
	}

	if len(mem.written) != 1 {
		t.Fatalf("应写下 1 条失败记忆，实际 %d 条", len(mem.written))
	}
	d := mem.written[0]
	if d.Stage != string(StageVerifyFailed) {
		t.Fatalf("记忆阶段 got %q, want %q（excerpt=%q）", d.Stage, StageVerifyFailed, d.Excerpt)
	}
	if !strings.Contains(d.Excerpt, excerptMarker) {
		t.Errorf("excerpt 应含失败步骤的输出片段 %q，got:\n%s", excerptMarker, d.Excerpt)
	}
	for _, leak := range []string{fakeGitHubToken, fakeEnvSecretVal} {
		if strings.Contains(d.Excerpt, leak) {
			t.Errorf("excerpt 泄漏了凭据 %q:\n%s", leak, d.Excerpt)
		}
		if strings.Contains(d.Reason, leak) {
			t.Errorf("reason 泄漏了凭据 %q:\n%s", leak, d.Reason)
		}
	}
	if !strings.Contains(d.Excerpt, redactedMark) {
		t.Errorf("脱敏处应留下 %q 标记，got:\n%s", redactedMark, d.Excerpt)
	}
	if len(d.Excerpt) > maxDigestStoredExcerpt {
		t.Errorf("excerpt 长度 got %d 字节, want <= %d（写入侧截断）", len(d.Excerpt), maxDigestStoredExcerpt)
	}
	if !utf8.ValidString(d.Excerpt) {
		t.Error("excerpt 不是合法 UTF-8（落库会撞 SQLSTATE 22021）")
	}
}
