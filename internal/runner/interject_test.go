package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
	"github.com/zichuanwangcloud-gif/lathe/internal/integration/github"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// f01 运行中插话的管线侧 B 类用例（docs/features/interject.md §5）。

// ijRow 是断言用的行快照（直接查表，不经 ListSince —— 后者会触发读时结算）。
type ijRow struct {
	id         int64
	status     string
	err        string
	phase      string
	deliveryID string
	body       string
}

func ijRows(t *testing.T, pool *pgxpool.Pool, taskID int64) []ijRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT id, status, coalesce(error,''), coalesce(phase,''), coalesce(delivery_id::text,''), body
		FROM task_interjections WHERE task_id=$1 ORDER BY id`, taskID)
	if err != nil {
		t.Fatalf("查插话行失败: %v", err)
	}
	defer rows.Close()
	var out []ijRow
	for rows.Next() {
		var r ijRow
		if err := rows.Scan(&r.id, &r.status, &r.err, &r.phase, &r.deliveryID, &r.body); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func setTaskState(t *testing.T, pool *pgxpool.Pool, taskID int64, state string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE tasks SET state=$2 WHERE id=$1`, taskID, state); err != nil {
		t.Fatalf("摆任务状态失败: %v", err)
	}
}

// insertQueued 绕过 API 直接落一条 queued 行（预置用）。
func insertQueued(t *testing.T, ij *Interjections, taskID, userID int64, body string) *store.Interjection {
	t.Helper()
	row, _, err := ij.Store.InsertInterjectionGated(context.Background(), taskID, userID, body,
		func(string) string { return "" })
	if err != nil || row.Status != store.InterjectionQueued {
		t.Fatalf("预置 queued 行失败: %+v %v", row, err)
	}
	return row
}

func randTag(prefix string) string {
	return prefix + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// submitOnVerify 是 VerificationRecorder 假件：第 round 次（0 起）落验证行时
// （任务此刻处于 verifying、agent 不在跑 —— 无活会话）插一条话。
type submitOnVerify struct {
	fakeVerifications
	once   sync.Once
	submit func()
}

func (s *submitOnVerify) InsertVerification(ctx context.Context, taskID int64, tier, step, status string, d int64, logRef string) error {
	s.once.Do(s.submit)
	return s.fakeVerifications.InsertVerification(ctx, taskID, tier, step, status, d, logRef)
}

// greetAgent 是「分诊 → 实现（绿阶段必挂）→ 修复轮改对」的 fakeAgent。
// 每次 result 都带 NumTurns=1：补投判 delivered 的判据是「至少一个 turn」。
func greetAgent(implOK bool) *fakeAgent {
	impl := "wrong"
	if implOK {
		impl = "hello"
	}
	return &fakeAgent{
		results: []*agent.Result{
			{Success: true, NumTurns: 1, Text: `{"actionable":true,"kind":"fix","reason":"r","question":""}`},
			{Success: true, NumTurns: 1, Text: "初版实现"},
			{Success: true, NumTurns: 1, Text: "修复"},
		},
		mutate: []func(string) error{
			nil,
			func(dir string) error {
				if err := os.WriteFile(filepath.Join(dir, "main_test.go"),
					[]byte("package main\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif greet() != \"hello\" {\n\t\tt.Fatalf(\"got %q\", greet())\n\t}\n}\n"), 0o644); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(dir, "fix.go"),
					[]byte("package main\n\nfunc greet() string { return \""+impl+"\" }\n"), 0o644)
			},
			func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "fix.go"),
					[]byte("package main\n\nfunc greet() string { return \"hello\" }\n"), 0o644)
			},
		},
	}
}

// ---------------------------------------------------------------- AC-7 / AC-21

// AC-7：只有装配了 Interjections 的实现/修复轮才开流式输入；分诊永远不开。
func TestRunAgentStreamInputOnlyWithRegistry(t *testing.T) {
	cases := []struct {
		name       string
		wire       bool
		supported  func(*task.Task) bool
		wantStream bool
	}{
		{"未装配", false, nil, false},
		{"已装配", true, nil, true},
		{"已装配但驱动不支持(AC-21)", true, func(*task.Task) bool { return false }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, m, taskID, repo, src := pipelineFixture(t)
			ag := &fakeAgent{results: []*agent.Result{
				{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"r","question":""}`},
				{Success: false, IsError: true, Subtype: "error_during_execution"},
			}}
			p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, &fakeGitHub{}, ag, &fakeNotifier{})
			if tc.wire {
				p.Interjections = newTestInterjections(t, m)
				p.Interjections.Supported = tc.supported
			}
			_ = p.Execute(context.Background(), ExecuteParams{TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777"})
			if len(ag.calls) < 2 {
				t.Fatalf("应有分诊+实现两次调用，got=%d", len(ag.calls))
			}
			assertTriageNotStreaming(t, ag.calls[0])
			impl := ag.calls[1]
			if impl.StreamInput != tc.wantStream || (impl.OnReady != nil) != tc.wantStream || (impl.OnDelivery != nil) != tc.wantStream {
				t.Errorf("实现轮 StreamInput=%v OnReady=%v OnDelivery=%v，want 全为 %v",
					impl.StreamInput, impl.OnReady != nil, impl.OnDelivery != nil, tc.wantStream)
			}
		})
	}
}

// AC-21：dev 上（f06 未合入）驱动能力判定恒真。
func TestStreamInputSupportedDefaultsTrue(t *testing.T) {
	for _, tk := range []*task.Task{nil, {}, {ID: 1}} {
		if !streamInputSupported(tk) {
			t.Errorf("streamInputSupported(%+v) got=false want=true", tk)
		}
	}
}

// ---------------------------------------------------------------- AC-8 / AC-8b

// AC-8：开场补投 —— queued 行拼进下一次 runAgent 的 prompt；有 result 且至少
// 一个 turn → delivered（phase 为本次 phase、delivery_id 为 NULL）；没有
// result → 退回 queued（不是 failed，也不是卡在 delivering）。
func TestRunAgentPrefillsPendingInterjections(t *testing.T) {
	cases := []struct {
		name       string
		res        *agent.Result
		err        error
		wantStatus string
	}{
		{"有result", &agent.Result{Success: true, NumTurns: 1}, nil, store.InterjectionDelivered},
		{"无result", nil, errors.New("进程崩了"), store.InterjectionQueued},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool, m, taskID, _, _ := pipelineFixture(t)
			ij := newTestInterjections(t, m)
			uid := taskOwner(t, m, taskID)
			a, b := randTag("ALPHA"), randTag("BETA")
			insertQueued(t, ij, taskID, uid, a)
			insertQueued(t, ij, taskID, uid, b)

			ag := &fakeAgent{results: []*agent.Result{tc.res}, errs: []error{tc.err}}
			p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, &fakeGitHub{}, ag, &fakeNotifier{})
			p.Interjections = ij
			tk, _ := m.Get(context.Background(), taskID)
			rc := &runCtx{ctx: context.Background(), tk: tk, wt: &Worktree{Path: t.TempDir()}}
			_, _ = p.runAgent(rc, "fix-1", "BASE", "sess-x", true)

			got := ag.calls[0].Prompt
			for _, want := range []string{"BASE", a, b, interjectHeading, interjectGuard} {
				if !strings.Contains(got, want) {
					t.Errorf("prompt 应含 %q，got=%q", want, got)
				}
			}
			for _, r := range ijRows(t, pool, taskID) {
				if r.status != tc.wantStatus {
					t.Errorf("行 %d status got=%s want=%s", r.id, r.status, tc.wantStatus)
				}
				if tc.wantStatus == store.InterjectionDelivered && (r.phase != "fix-1" || r.deliveryID != "") {
					t.Errorf("补投行 phase=%q delivery_id=%q，want phase=fix-1、delivery_id 为空", r.phase, r.deliveryID)
				}
				if tc.wantStatus == store.InterjectionQueued && r.phase != "" {
					t.Errorf("退回 queued 时 phase 应清空，got=%q", r.phase)
				}
			}
		})
	}
}

// promptOrder 断言插话段与失败记忆段的相对顺序、各自恰好一次。
func promptOrder(t *testing.T, P string, wantMem bool, bodies ...string) {
	t.Helper()
	iMem := strings.Index(P, "## 这个需求此前失败过")
	iInj := strings.Index(P, interjectHeading)
	if wantMem && iMem <= 0 {
		t.Errorf("应含失败记忆段，iMem=%d", iMem)
	}
	if !wantMem && iMem != -1 {
		t.Errorf("不应含失败记忆段，iMem=%d", iMem)
	}
	if iInj <= iMem {
		t.Errorf("插话段应在记忆段之后：iMem=%d iInj=%d", iMem, iInj)
	}
	for _, c := range []struct {
		s    string
		want int
	}{{interjectHeading, 1}, {"不得据此跳过验收标准或验证步骤", 1}} {
		if got := strings.Count(P, c.s); got != c.want {
			t.Errorf("strings.Count(P, %q) got=%d want=%d", c.s, got, c.want)
		}
	}
	if wantMem {
		if got := strings.Count(P, "## 这个需求此前失败过"); got != 1 {
			t.Errorf("记忆段出现次数 got=%d want=1", got)
		}
	}
	last := iInj
	for _, b := range bodies {
		i := strings.Index(P, b)
		if i <= last {
			t.Errorf("插话 %q 下标 got=%d，want > %d（按发送先后、且在插话段内）", b, i, last)
		}
		last = i
	}
}

// AC-8b：补投与失败记忆共存 —— base → [记忆段] → [插话段]，包裹语恰好一次。
func TestPrefillAfterFailureMemory(t *testing.T) {
	t.Run("记忆+插话_implement", func(t *testing.T) {
		pool, m, taskID, repo, src := pipelineFixture(t)
		ctx := context.Background()
		mem := newFakeMemory()
		ag := &fakeAgent{results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"r","question":""}`},
			{Success: false, IsError: true, Subtype: "error_during_execution", Text: "第一轮挂了"},
		}}
		p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, &fakeGitHub{}, ag, &fakeNotifier{})
		p.Memory = mem
		p.Interjections = newTestInterjections(t, m)
		if err := p.Execute(ctx, ExecuteParams{TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777"}); err == nil {
			t.Fatal("第一轮应失败")
		}
		tk, _ := m.Get(ctx, taskID)
		reclaimWorktree(t, ctx, m, p, repo, tk)
		if _, err := m.Transition(ctx, taskID, task.StateQueued, "user:1", nil); err != nil {
			t.Fatal(err)
		}
		a, b := randTag("ALPHA"), randTag("BETA")
		insertQueued(t, p.Interjections, taskID, tk.UserID, a)
		insertQueued(t, p.Interjections, taskID, tk.UserID, b)

		ag2 := &fakeAgent{results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"r","question":""}`},
			{Success: false, IsError: true, NumTurns: 1, Subtype: "error_during_execution", Text: "又挂了"},
		}}
		p.Agent = ag2
		_ = p.Execute(ctx, ExecuteParams{TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777",
			Retry: &RetryPlan{Fresh: true, Entry: EntryTriage}})
		if len(ag2.calls) < 2 {
			t.Fatalf("第二轮应走到实现阶段，got=%d 次调用", len(ag2.calls))
		}
		P := ag2.calls[1].Prompt
		tk2, _ := m.Get(ctx, taskID)
		if !strings.HasPrefix(P, ImplementPrompt(demoIssue().Context(), KindFix, *tk2.BranchName)) {
			t.Errorf("base 应在最前且原样")
		}
		promptOrder(t, P, true, a, b)
		for _, r := range ijRows(t, pool, taskID) {
			if r.status != store.InterjectionDelivered || r.phase != "implement" {
				t.Errorf("行 %d got status=%s phase=%s want delivered/implement", r.id, r.status, r.phase)
			}
		}
	})

	t.Run("记忆+插话_implement-reentry", func(t *testing.T) {
		pool, m, taskID, repo, src := pipelineFixture(t)
		ctx := context.Background()
		ag := &fakeAgent{
			errs:    []error{errors.New("No conversation found with session ID"), nil},
			results: []*agent.Result{nil, {Success: true, NumTurns: 1, Text: "新会话收尾"}},
			mutate: []func(string) error{nil, func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "README.md"), []byte("# src\n\n补完\n"), 0o644)
			}},
		}
		mem := newFakeMemory()
		p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, &fakeGitHub{pr: &github.PullRequest{URL: "https://x/pr/1", Number: 1}}, ag, &fakeNotifier{})
		p.Memory = mem
		p.Interjections = newTestInterjections(t, m)
		prepareFailedTask(t, p, m, taskID, src, repo, StageImplementRun, false)
		tk, _ := m.Get(ctx, taskID)
		_ = mem.InsertFailureDigest(ctx, store.FailureDigest{TaskID: taskID, Stage: string(StageImplementIncomplete), Reason: "实现中断", Excerpt: "上一次的输出"},
			store.FailureLookup{UserID: tk.UserID, RepoID: tk.RepoID, TrackerProvider: tk.TrackerProvider, ExternalKey: tk.ExternalKey})
		a, b := randTag("ALPHA"), randTag("BETA")
		insertQueued(t, p.Interjections, taskID, tk.UserID, a)
		insertQueued(t, p.Interjections, taskID, tk.UserID, b)

		if err := p.Execute(ctx, ExecuteParams{TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777",
			Retry: &RetryPlan{Entry: EntryImplement, ResumeSession: true}}); err != nil {
			t.Fatalf("降级续跑应成功: %v", err)
		}
		if len(ag.calls) != 2 || !ag.calls[0].Resume || ag.calls[1].Resume {
			t.Fatalf("应是 resume 失败 + 降级新会话两次调用，got=%d", len(ag.calls))
		}
		if got := strings.Count(ag.calls[0].Prompt, interjectHeading); got != 1 {
			t.Errorf("resume 那次 prompt 也应含插话段且恰好一次，got=%d", got)
		}
		promptOrder(t, ag.calls[1].Prompt, true, a, b)
		for _, r := range ijRows(t, pool, taskID) {
			if r.status != store.InterjectionDelivered || r.phase != PhaseImplementReentry {
				t.Errorf("行 %d got status=%s phase=%s want delivered/%s", r.id, r.status, r.phase, PhaseImplementReentry)
			}
		}
	})

	t.Run("resume给出num_turns为0的error_result", func(t *testing.T) {
		pool, m, taskID, repo, src := pipelineFixture(t)
		ctx := context.Background()
		ag := &fakeAgent{results: []*agent.Result{{IsError: true, NumTurns: 0, Subtype: "error_during_execution"}}}
		p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, &fakeGitHub{}, ag, &fakeNotifier{})
		ij := newTestInterjections(t, m)
		p.Interjections = ij
		prepareFailedTask(t, p, m, taskID, src, repo, StageImplementRun, false)
		tk, _ := m.Get(ctx, taskID)
		insertQueued(t, ij, taskID, tk.UserID, randTag("ALPHA"))
		insertQueued(t, ij, taskID, tk.UserID, randTag("BETA"))

		// finishPrefill 单独验：num_turns==0 → 退回 queued
		rc := &runCtx{ctx: ctx, tk: tk, wt: &Worktree{Path: t.TempDir()}}
		_, _ = p.runAgent(rc, "implement", "BASE", "sess-original", true)
		for _, r := range ijRows(t, pool, taskID) {
			if r.status != store.InterjectionQueued {
				t.Errorf("num_turns=0 后行 %d got=%s want=queued（不是 delivered）", r.id, r.status)
			}
		}
		// 完整管线：p.fail → T1 结算 → failed「任务已失败」
		ag.calls, ag.results = nil, []*agent.Result{{IsError: true, NumTurns: 0, Subtype: "error_during_execution"}}
		_ = p.Execute(ctx, ExecuteParams{TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777",
			Retry: &RetryPlan{Entry: EntryImplement, ResumeSession: true}})
		for _, r := range ijRows(t, pool, taskID) {
			if r.status != store.InterjectionFailed || !strings.Contains(r.err, "任务已失败") {
				t.Errorf("行 %d got=(%s,%q) want failed+任务已失败", r.id, r.status, r.err)
			}
		}
	})

	t.Run("仅插话无记忆", func(t *testing.T) {
		_, m, taskID, repo, src := pipelineFixture(t)
		ag := &fakeAgent{results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"r","question":""}`},
			{Success: false, IsError: true, NumTurns: 1, Subtype: "error_during_execution"},
		}}
		p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, &fakeGitHub{}, ag, &fakeNotifier{})
		p.Memory = newFakeMemory()
		p.Interjections = newTestInterjections(t, m)
		a := randTag("ALPHA")
		insertQueued(t, p.Interjections, taskID, taskOwner(t, m, taskID), a)
		_ = p.Execute(context.Background(), ExecuteParams{TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777"})
		tk, _ := m.Get(context.Background(), taskID)
		P := ag.calls[1].Prompt
		if !strings.HasPrefix(P, ImplementPrompt(demoIssue().Context(), KindFix, *tk.BranchName)) {
			t.Error("base 应在最前且原样")
		}
		promptOrder(t, P, false, a)
	})

	t.Run("仅记忆无插话", func(t *testing.T) {
		run := func(wire bool) string {
			_, m, taskID, repo, src := pipelineFixture(t)
			mem := newFakeMemory()
			ag := &fakeAgent{results: []*agent.Result{
				{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"r","question":""}`},
				{Success: false, IsError: true, Subtype: "error_during_execution"},
			}}
			p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, &fakeGitHub{}, ag, &fakeNotifier{})
			p.Memory = mem
			tk, _ := m.Get(context.Background(), taskID)
			_ = mem.InsertFailureDigest(context.Background(), store.FailureDigest{TaskID: -1, Stage: "implement_run", Reason: "旧账", Excerpt: "x"},
				store.FailureLookup{UserID: tk.UserID, RepoID: tk.RepoID, TrackerProvider: tk.TrackerProvider, ExternalKey: tk.ExternalKey})
			if wire {
				p.Interjections = newTestInterjections(t, m)
			}
			_ = p.Execute(context.Background(), ExecuteParams{TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777"})
			tk, _ = m.Get(context.Background(), taskID)
			return strings.ReplaceAll(ag.calls[1].Prompt, *tk.BranchName, "<BRANCH>")
		}
		without, with := run(false), run(true)
		if strings.Contains(with, interjectHeading) {
			t.Error("无插话时不应有插话段")
		}
		if !strings.Contains(with, "## 这个需求此前失败过") {
			t.Error("夹具应带出失败记忆段")
		}
		if with != without {
			t.Errorf("装配插话、无插话行时 prompt 应与未装配逐字相等：\nwith=%q\nwithout=%q", with, without)
		}
	})

	t.Run("fix-1加插话", func(t *testing.T) {
		pool, m, taskID, repo, src := pipelineFixture(t)
		ag := greetAgent(false)
		p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, &fakeGitHub{pr: &github.PullRequest{URL: "https://x/pr/2", Number: 2}}, ag, &fakeNotifier{})
		p.MaxFixAttempts = 1
		p.Memory = newFakeMemory()
		ij := newTestInterjections(t, m)
		p.Interjections = ij
		a := randTag("ALPHA")
		rec := &submitOnVerify{}
		rec.submit = func() {
			res, err := ij.Submit(context.Background(), taskID, taskOwner(t, m, taskID), a)
			if err != nil || res.Outcome != OutcomeQueued {
				t.Errorf("verifying 无会话插话 got=(%v,%v) want queued", res.Outcome, err)
			}
		}
		p.Verifications = rec
		if err := p.Execute(context.Background(), ExecuteParams{TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777"}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if len(ag.calls) != 3 {
			t.Fatalf("应有分诊/实现/修复三次调用，got=%d", len(ag.calls))
		}
		P := ag.calls[2].Prompt
		if !strings.HasPrefix(P, "你在一个自动化编码流水线里。刚才的实现没有通过验证（第 1/1 轮修复）") {
			t.Errorf("fix-1 的 base 应是 FixPrompt 且在最前，got=%q", P[:min(80, len(P))])
		}
		promptOrder(t, P, false, a)
		if !strings.HasSuffix(P, a) {
			t.Errorf("插话段应在末尾，got 尾部=%q", P[max(0, len(P)-80):])
		}
		rows := ijRows(t, pool, taskID)
		if len(rows) != 1 || rows[0].status != store.InterjectionDelivered || rows[0].phase != "fix-1" {
			t.Errorf("got=%+v want 1 行 delivered/fix-1", rows)
		}
	})
}

// ---------------------------------------------------------------- AC-19

// AC-19（管线侧场景）：queued 一定有结局。
func TestQueuedInterjectionsSettledWhenTaskLeavesVerify(t *testing.T) {
	type scenario struct {
		name       string
		gate       string // repos.gate_mode
		agent      func() *fakeAgent
		fix        int
		submitAt   string // verify | implement
		after      func(t *testing.T, m *task.Machine, taskID int64)
		wantStatus string
		wantErr    string
		wantPhase  string
	}
	scenarios := []scenario{
		{name: "verifying插话_验证一次通过_pr_open", agent: func() *fakeAgent { return greetAgent(true) }, submitAt: "verify",
			wantStatus: store.InterjectionFailed, wantErr: "没有后续 agent 轮次"},
		{name: "verifying插话_验证一次通过_awaiting_approval", gate: "manual", agent: func() *fakeAgent { return greetAgent(true) }, submitAt: "verify",
			wantStatus: store.InterjectionFailed, wantErr: "没有后续 agent 轮次"},
		{name: "verifying插话_验证失败_修复轮补投", agent: func() *fakeAgent { return greetAgent(false) }, fix: 1, submitAt: "verify",
			wantStatus: store.InterjectionDelivered, wantPhase: "fix-1"},
		{name: "插话后任务经p.fail转failed", submitAt: "implement",
			agent: func() *fakeAgent {
				return &fakeAgent{results: []*agent.Result{
					{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"r","question":""}`},
					{IsError: true, NumTurns: 1, Subtype: "error_during_execution"},
				}, mutate: []func(string) error{nil, nil}}
			},
			wantStatus: store.InterjectionFailed, wantErr: "任务已失败"},
		{name: "插话后任务被取消_在途agent跑完", submitAt: "implement",
			agent: func() *fakeAgent { return greetAgent(true) },
			after: func(t *testing.T, m *task.Machine, id int64) {
				if _, err := m.Transition(context.Background(), id, task.StateCancelled, "user:x", nil); err != nil {
					t.Fatal(err)
				}
			},
			wantStatus: store.InterjectionFailed, wantErr: "已取消"},
		{name: "插话后任务被重新入队_Execute返回时仍queued", submitAt: "implement",
			// 在途 agent 随后崩溃：p.fail 想转 failed，但 queued → failed 不是合法边，
			// Execute 返回时任务仍是 queued（Reconcile 式的重新入队）。
			agent: func() *fakeAgent {
				return &fakeAgent{results: []*agent.Result{
					{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"r","question":""}`},
				}, mutate: []func(string) error{nil, func(string) error { return errors.New("agent 进程崩溃") }}}
			},
			after: func(t *testing.T, m *task.Machine, id int64) {
				if _, err := m.Transition(context.Background(), id, task.StateQueued, "system", nil); err != nil {
					t.Fatal(err)
				}
			},
			wantStatus: store.InterjectionQueued},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			pool, m, taskID, repo, src := pipelineFixture(t)
			if sc.gate != "" {
				if _, err := pool.Exec(context.Background(), `UPDATE tasks SET gate_mode=$2 WHERE id=$1`, taskID, sc.gate); err != nil {
					t.Fatal(err)
				}
			}
			ag := sc.agent()
			p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, &fakeGitHub{pr: &github.PullRequest{URL: "https://x/pr/3", Number: 3}}, ag, &fakeNotifier{})
			p.MaxFixAttempts = sc.fix
			ij := newTestInterjections(t, m)
			p.Interjections = ij
			uid := taskOwner(t, m, taskID)
			submit := func() {
				res, err := ij.Submit(context.Background(), taskID, uid, "别动 migration")
				if err != nil || res.Outcome != OutcomeQueued {
					t.Errorf("无会话插话 got=(%v,%v) want queued(202)", res.Outcome, err)
				}
			}
			if sc.submitAt == "verify" {
				rec := &submitOnVerify{submit: submit}
				p.Verifications = rec
			} else {
				prev := ag.mutate[1]
				ag.mutate[1] = func(dir string) error {
					submit()
					if sc.after != nil {
						sc.after(t, m, taskID)
					}
					if prev != nil {
						return prev(dir)
					}
					return nil
				}
			}
			_ = p.Execute(context.Background(), ExecuteParams{TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777"})
			rows := ijRows(t, pool, taskID)
			if len(rows) != 1 {
				t.Fatalf("应有 1 行插话，got=%d", len(rows))
			}
			r := rows[0]
			if r.status != sc.wantStatus || !strings.Contains(r.err, sc.wantErr) || (sc.wantPhase != "" && r.phase != sc.wantPhase) {
				t.Errorf("got=(%s,%q,%q) want=(%s,含%q,%q)", r.status, r.err, r.phase, sc.wantStatus, sc.wantErr, sc.wantPhase)
			}
		})
	}
}

// AC-19 竞态：Submit 的事务 A 与 verifying → pr_open（+ 收尾结算）并发 100 次，
// 结局只有两种：409（读到新状态）或被结算判 failed；0 行停在 queued。
func TestInterjectRaceSubmitVsLeaveVerify(t *testing.T) {
	pool, m, taskID, _, _ := pipelineFixture(t)
	ij := newTestInterjections(t, m)
	uid := taskOwner(t, m, taskID)
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		setTaskState(t, pool, taskID, "verifying")
		var wg sync.WaitGroup
		var res SubmitResult
		var serr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			res, serr = ij.Submit(ctx, taskID, uid, fmt.Sprintf("race-%d", i))
		}()
		go func() {
			defer wg.Done()
			if _, err := m.Transition(ctx, taskID, task.StatePROpen, "system", nil); err != nil {
				t.Errorf("转 pr_open 失败: %v", err)
			}
			ij.SettleTask(ctx, taskID)
		}()
		wg.Wait()
		if serr != nil {
			t.Fatalf("第 %d 次 Submit: %v", i, serr)
		}
		if res.Outcome != OutcomeQueued && res.Outcome != OutcomeRejectedState {
			t.Errorf("第 %d 次结局 got=%v want queued 或 409", i, res.Outcome)
		}
	}
	for _, r := range ijRows(t, pool, taskID) {
		if r.status != store.InterjectionFailed {
			t.Errorf("行 %d got=%s want=failed（0 行停在 queued）", r.id, r.status)
		}
	}
}

// gatedSession 的 Deliver 阻塞在闸门上，放开后返回 release 里给的错误。
type gatedSession struct {
	gate    chan error
	entered chan struct{}
	calls   int
	mu      sync.Mutex
}

func newGatedSession() *gatedSession {
	return &gatedSession{gate: make(chan error, 1), entered: make(chan struct{}, 1)}
}

func (g *gatedSession) Deliver(ctx context.Context, id, text string) error {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	g.entered <- struct{}{}
	return <-g.gate
}
func (g *gatedSession) Closed() bool { return false }

// AC-19（R2 竞态行）：有活会话、Submit 已 MarkDelivering，Deliver 卡在闸门上；
// 此时任务离开 verifying 并结算（结算跳过 delivering 行），再放开闸门返回
// ErrSessionClosed → 行 failed + 409；若放开时仍是 verifying → 行 queued + 202，
// 随后的 fix-1 补投它 → delivered/fix-1。交错 100 次后 0 queued、0 delivering。
func TestInterjectRaceRevertVsSettle(t *testing.T) {
	pool, m, taskID, _, _ := pipelineFixture(t)
	ctx := context.Background()
	uid := taskOwner(t, m, taskID)

	t.Run("放开前任务已pr_open", func(t *testing.T) {
		setTaskState(t, pool, taskID, "verifying")
		ij := newTestInterjections(t, m)
		g := newGatedSession()
		ij.Register(taskID, g, "fix-1", "s")
		done := make(chan SubmitResult, 1)
		go func() {
			r, _ := ij.Submit(ctx, taskID, uid, "late")
			done <- r
		}()
		<-g.entered
		if _, err := m.Transition(ctx, taskID, task.StatePROpen, "system", nil); err != nil {
			t.Fatal(err)
		}
		ij.SettleTask(ctx, taskID)
		g.gate <- agent.ErrSessionClosed
		r := <-done
		if r.Outcome != OutcomeRejectedState || r.Row.Status != store.InterjectionFailed || !strings.Contains(*r.Row.Error, "没有后续 agent 轮次") {
			t.Errorf("got=(%v,%+v) want 409 + failed + 没有后续 agent 轮次", r.Outcome, r.Row)
		}
	})

	t.Run("放开前仍verifying_随后fix-1补投", func(t *testing.T) {
		setTaskState(t, pool, taskID, "verifying")
		ij := newTestInterjections(t, m)
		g := newGatedSession()
		ij.Register(taskID, g, "fix-1", "s")
		done := make(chan SubmitResult, 1)
		go func() {
			r, _ := ij.Submit(ctx, taskID, uid, "between-rounds")
			done <- r
		}()
		<-g.entered
		g.gate <- agent.ErrSessionClosed
		r := <-done
		if r.Outcome != OutcomeQueued || r.Row.Status != store.InterjectionQueued || r.Row.DeliveryID != nil || r.Row.Phase != nil || r.Row.SessionID != nil {
			t.Fatalf("got=(%v,%+v) want 202 + queued 且 delivery_id/phase/session_id 清空", r.Outcome, r.Row)
		}
		ag := &fakeAgent{results: []*agent.Result{{Success: true, NumTurns: 1}}}
		p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, &fakeGitHub{}, ag, &fakeNotifier{})
		p.Interjections = ij
		tk, _ := m.Get(ctx, taskID)
		_, _ = p.runAgent(&runCtx{ctx: ctx, tk: tk, wt: &Worktree{Path: t.TempDir()}}, "fix-1", "FIX", "s", true)
		got, _ := ij.Store.GetInterjection(ctx, r.Row.ID)
		if got.Status != store.InterjectionDelivered || got.Phase == nil || *got.Phase != "fix-1" {
			t.Errorf("补投后 got=%+v want delivered/fix-1", got)
		}
	})

	t.Run("交错100次", func(t *testing.T) {
		pool2, m2, taskID2, _, _ := pipelineFixture(t)
		uid2 := taskOwner(t, m2, taskID2)
		ij := newTestInterjections(t, m2)
		for i := 0; i < 100; i++ {
			setTaskState(t, pool2, taskID2, "verifying")
			g := newGatedSession()
			ij.Register(taskID2, g, "fix-1", "s")
			done := make(chan struct{})
			go func() {
				_, _ = ij.Submit(ctx, taskID2, uid2, fmt.Sprintf("x-%d", i))
				close(done)
			}()
			<-g.entered
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); g.gate <- agent.ErrSessionClosed }()
			go func() {
				defer wg.Done()
				if i%2 == 0 {
					time.Sleep(time.Duration(i%5) * time.Millisecond)
				}
				_, _ = m2.Transition(ctx, taskID2, task.StatePROpen, "system", nil)
				ij.SettleTask(ctx, taskID2)
			}()
			wg.Wait()
			<-done
			// 回退可能在结算之后才拿到锁并读到 pr_open（→ failed），也可能先拿到锁读到
			// verifying（→ queued，随后结算一定看见它）。这里补一次结算模拟 T3。
			ij.SettleTask(ctx, taskID2)
		}
		for _, r := range ijRows(t, pool2, taskID2) {
			if r.status == store.InterjectionQueued || r.status == store.InterjectionDelivering {
				t.Errorf("行 %d 停在 %s", r.id, r.status)
			}
		}
	})
	_ = pool
}

// AC-19（R3 行，Execute 不再运行的路径）：blocked_dep 级联 / 周期清扫 /
// 读时结算不误判。cancelTask 与 CancelForIssue 两行分别在 httpapi 与 cmd/lathe。
func TestQueuedInterjectionsSettledWithoutExecute(t *testing.T) {
	ctx := context.Background()
	t.Run("blocked_dep级联后读时结算", func(t *testing.T) {
		pool, m, taskID, _, _ := pipelineFixture(t)
		uid := taskOwner(t, m, taskID)
		// 前驱任务：同一用户/仓库下另建一个，后继 = 本任务
		var repoID int64
		_ = pool.QueryRow(ctx, `SELECT repo_id FROM tasks WHERE id=$1`, taskID).Scan(&repoID)
		pred, err := m.Create(ctx, task.CreateParams{UserID: uid, RepoID: repoID, ExternalKey: randTag("CR-PRED")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE tasks SET depends_on=$2 WHERE id=$1`, taskID, pred.ID); err != nil {
			t.Fatal(err)
		}
		setTaskState(t, pool, taskID, "implementing")
		ij := newTestInterjections(t, m)
		first, err := ij.Submit(ctx, taskID, uid, "dep")
		if err != nil || first.Outcome != OutcomeQueued {
			t.Fatalf("got=(%v,%v)", first.Outcome, err)
		}
		setTaskState(t, pool, taskID, "queued") // Execute 返回时被重新入队
		if _, err := m.PropagateBlocked(ctx, pred.ID, "前驱失败"); err != nil {
			t.Fatal(err)
		}
		rows, err := ij.ListSince(ctx, taskID, 0, 200)
		if err != nil || len(rows) != 1 {
			t.Fatalf("ListSince got=(%d,%v)", len(rows), err)
		}
		if r := rows[0]; r.Status != store.InterjectionFailed || r.Error == nil || !strings.Contains(*r.Error, "前置任务失败") || r.Rev <= first.Row.Rev {
			t.Errorf("got=%+v want failed + 前置任务失败 + rev 前进", r)
		}
	})
	t.Run("无读者_周期清扫一次", func(t *testing.T) {
		pool, m, taskID, _, _ := pipelineFixture(t)
		setTaskState(t, pool, taskID, "implementing")
		ij := newTestInterjections(t, m)
		r, _ := ij.Submit(ctx, taskID, taskOwner(t, m, taskID), "sweep")
		setTaskState(t, pool, taskID, "cancelled")
		if _, err := ij.SweepOnce(ctx); err != nil {
			t.Fatal(err)
		}
		got, _ := ij.Store.GetInterjection(ctx, r.Row.ID)
		if got.Status != store.InterjectionFailed || !strings.Contains(*got.Error, "已取消") {
			t.Errorf("got=%+v want failed + 已取消", got)
		}
	})
	t.Run("转回queued未取消_读时结算不误判", func(t *testing.T) {
		pool, m, taskID, _, _ := pipelineFixture(t)
		setTaskState(t, pool, taskID, "implementing")
		ij := newTestInterjections(t, m)
		_, _ = ij.Submit(ctx, taskID, taskOwner(t, m, taskID), "keep")
		setTaskState(t, pool, taskID, "queued")
		rows, _ := ij.ListSince(ctx, taskID, 0, 200)
		if len(rows) != 1 || rows[0].Status != store.InterjectionQueued {
			t.Errorf("got=%+v want 仍 queued", rows)
		}
	})
}
