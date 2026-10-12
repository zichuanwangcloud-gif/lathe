package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
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

// ★ AC3 的记忆闭环，后半：同 issue 重试 → 提示词含 digest 摘要；
// 无历史失败 → 提示词不变。
//
// 这条把两半缝在一起验：先让一次执行失败写下记忆，再在同一 task 行上
// 从「全新会话」重入（ReentryImplementPrompt 那条路径），断言第二次的
// 提示词里出现了第一次的失败摘要。同时用一条对照组断言「没记忆时提示词
// 逐字不变」—— 少了这一条，一个「无脑往所有提示词里塞一段」的实现也能
// 让前半通过。
func TestFailureMemoryInjectedOnRetry(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)
	ctx := context.Background()

	mem := newFakeMemory()
	// 预置一条历史失败：模拟「这个单子上一轮死过」。Stage 与摘要刻意
	// 用可搜索的独特字符串，便于断言注入了正确的那一份。

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

	// 读出真实定位键：注入用它查表。fakeMemory 按 lookup 存，
	// 所以第一次写入时记下的键就是重试时要查的键。
	if len(mem.lookups) == 0 {
		t.Fatal("第一轮应查询过记忆（注入点）")
	}
	realKey := mem.lookups[0]
	if _, ok := mem.stored[realKey]; !ok {
		t.Fatalf("第一轮写下的记忆应能按该键查到，键=%+v", realKey)
	}

	// 第二轮：任务回到 queued 后重跑。这次走全新会话（Fresh 重建），
	// agent 交出的实现应当被注入历史失败摘要。
	//
	// 重建前必须丢弃旧现场：worktree 的尸体仍在磁盘上，Create 会把它当
	// 在途现场拒绝接管（这正是 mingled 现场保护的设计，见 §9）。真实
	// 路径上这一步由 queue 的 plan.Fresh 分支调 Discard 完成，测试里
	// 手工补上。
	tk, err := m.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	path, branch := "", ""
	if tk.WorktreePath != nil {
		path = *tk.WorktreePath
	}
	if tk.BranchName != nil {
		branch = *tk.BranchName
	}
	p.Worktrees.Discard(ctx, "acme/demo", path, branch)

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

	// 断言：第二轮里至少有一次 agent 调用的提示词带上了历史失败。
	// 分诊与实现两处都注入了，所以只要有一次带上就算成。
	found := false
	for _, c := range ag2.calls {
		if strings.Contains(c.Prompt, "这个需求此前失败过") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("重试时应把历史失败摘要注入提示词，实际调用：%s", promptDump(ag2.calls))
	}
	// 且注入的必须是「历史」那一份（第一轮的失败阶段），不是本次的
	joined := promptDump(ag2.calls)
	if !strings.Contains(joined, string(StageImplementIncomplete)) {
		t.Errorf("注入的内容应是第一轮那次失败的阶段 %q，实际：%s",
			StageImplementIncomplete, joined)
	}
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
	// 造一份远超上限的验证输出（模拟把整个构建日志灌进来）
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
	// 被截断时必须留下痕迹 —— 静默截断会让人以为失败输出就这么长
	if !strings.Contains(got, "已截断") {
		t.Error("超长摘要被截断时应留下明显标记")
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
