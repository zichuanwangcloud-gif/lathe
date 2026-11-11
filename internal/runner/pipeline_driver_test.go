package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
	"github.com/zichuanwangcloud-gif/lathe/internal/integration/github"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// pipeline_driver_test.go —— f06：驱动从任务行取、codex 的真实会话 ID 落库并
// 被修复回路/review 续跑沿用、节点没有驱动时不碰会话、技能目录与 subagent
// watcher 按驱动选择。

// ---------------------------------------------------------------- 假件

// driverStep 是 driverAgent 第 i 次 Run 的剧本。
type driverStep struct {
	res    *agent.Result
	err    error
	mutate func(dir string) error
	// session 非空时在这次 Run 里经 OnSession 上报（模拟 codex 的 thread.started）
	session string
	// emit 是经 OnEvent 投递的原始 codex 行（Driver 取 RunParams.Driver）
	emit []string
	// hook 在 Run 开头被调用，用来观察那一刻的库状态
	hook func(p agent.RunParams)
}

// driverAgent 按调用次序执行剧本，并记录每次收到的 RunParams。
// 比 fakeAgent 多出 OnSession / OnEvent 两种回调能力。
type driverAgent struct {
	mu    sync.Mutex
	steps []driverStep
	calls []agent.RunParams
}

func (f *driverAgent) Run(ctx context.Context, p agent.RunParams) (*agent.Result, error) {
	f.mu.Lock()
	i := len(f.calls)
	f.calls = append(f.calls, p)
	var st driverStep
	if i < len(f.steps) {
		st = f.steps[i]
	}
	f.mu.Unlock()

	if st.hook != nil {
		st.hook(p)
	}
	if st.mutate != nil {
		if err := st.mutate(p.Dir); err != nil {
			return nil, err
		}
	}
	for _, l := range st.emit {
		if p.OnEvent != nil {
			p.OnEvent(agent.Event{Driver: p.Driver, Type: agent.EventSystem, Subtype: "init", Raw: []byte(l)})
		}
	}
	if st.session != "" && p.OnSession != nil {
		p.OnSession(st.session)
	}
	if st.err != nil {
		return st.res, st.err
	}
	if st.res != nil {
		return st.res, nil
	}
	return &agent.Result{Success: true, Text: "done"}, nil
}

func (f *driverAgent) snapshot() []agent.RunParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.RunParams(nil), f.calls...)
}

// spyRunner 记录每次 Run 后转交给真实的 inner（例如 agent.Router）。
type spyRunner struct {
	inner AgentDriver
	mu    sync.Mutex
	calls []agent.RunParams
}

func (s *spyRunner) Run(ctx context.Context, p agent.RunParams) (*agent.Result, error) {
	s.mu.Lock()
	s.calls = append(s.calls, p)
	s.mu.Unlock()
	return s.inner.Run(ctx, p)
}

// errRunner 是一个永远报同一个错的驱动（注册进 Router 当「本节点的 codex」）。
type errRunner struct{ err error }

func (e errRunner) Run(ctx context.Context, p agent.RunParams) (*agent.Result, error) {
	return nil, e.err
}

// ---------------------------------------------------------------- 夹具

const (
	triageOK   = `{"actionable":true,"kind":"fix","reason":"有现象和期望行为","question":""}`
	greetTest  = "package main\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif greet() != \"hello\" {\n\t\tt.Fatalf(\"got %q\", greet())\n\t}\n}\n"
	greetRight = "package main\n\nfunc greet() string { return \"hello\" }\n"
	greetWrong = "package main\n\nfunc greet() string { return \"wrong\" }\n"
)

func writeGreet(ret string) func(dir string) error {
	return func(dir string) error {
		if err := os.WriteFile(filepath.Join(dir, "main_test.go"), []byte(greetTest), 0o644); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "fix.go"), []byte(ret), 0o644)
	}
}

func writeFix(content string) func(dir string) error {
	return func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "fix.go"), []byte(content), 0o644)
	}
}

func writeReadme(content string) func(dir string) error {
	return func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "README.md"), []byte(content), 0o644)
	}
}

// pinDriver 把任务行的 agent_driver 钉成 driver（等价于「建任务时仓库就是它」）。
func pinDriver(t *testing.T, pool *pgxpool.Pool, taskID int64, driver string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE tasks SET agent_driver = $2 WHERE id = $1`, taskID, driver); err != nil {
		t.Fatalf("钉死任务驱动失败: %v", err)
	}
}

func sessionOf(t *testing.T, pool *pgxpool.Pool, taskID int64) string {
	t.Helper()
	var s *string
	if err := pool.QueryRow(context.Background(),
		`SELECT agent_session_id FROM tasks WHERE id = $1`, taskID).Scan(&s); err != nil {
		t.Fatalf("读会话 ID 失败: %v", err)
	}
	if s == nil {
		return ""
	}
	return *s
}

// prepareVerifyFailingTask 造一个「从 EntryVerify 续跑、验证必挂」的现场：
// worktree 里提交了测试 + 错误实现（heavy 档绿阶段必挂），任务行没有会话，
// 状态 queued。session 非空时同时写进任务行。
func prepareVerifyFailingTask(t *testing.T, p *Pipeline, m *task.Machine, taskID int64, src string, repo RepoConfig, session string) *Worktree {
	t.Helper()
	ctx := context.Background()
	wt, err := p.Worktrees.Create(ctx, CreateParams{
		Repo: repo, CloneURL: src, Kind: KindFix, IssueKey: "CR-777", Title: "t", TaskID: taskID,
	})
	if err != nil {
		t.Fatalf("造现场：Create 失败: %v", err)
	}
	if err := writeGreet(greetWrong)(wt.Path); err != nil {
		t.Fatal(err)
	}
	if err := p.Worktrees.Commit(ctx, wt, "fix: 初版（答错了）"); err != nil {
		t.Fatalf("造现场：Commit 失败: %v", err)
	}
	kind, tier := "fix", "heavy"
	opts := &task.TransitionOpts{WorktreePath: &wt.Path, BranchName: &wt.Branch, TaskKind: &kind, VerifyTier: &tier}
	if session != "" {
		opts.AgentSessionID = &session
	}
	if _, err := m.Transition(ctx, taskID, task.StateImplementing, "test", opts); err != nil {
		t.Fatalf("造现场：转 implementing 失败: %v", err)
	}
	reason := "验证未通过（测试构造）"
	if _, err := m.Transition(ctx, taskID, task.StateFailed, "test", &task.TransitionOpts{
		FailureReason: &reason, FailureStage: strPtr(string(StageVerifyFailed)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Transition(ctx, taskID, task.StateQueued, "test", nil); err != nil {
		t.Fatal(err)
	}
	return wt
}

// ---------------------------------------------------------------- AC-10 / AC-11

// 驱动是任务的属性：仓库此刻已改回 claude、RepoConfig 里没有任何驱动信息，
// 钉死为 codex 的任务分诊与实现都收到 Driver=codex。
func TestPipelineDriverFromTaskRow(t *testing.T) {
	pool, m, taskID, repo, src := pipelineFixture(t)
	pinDriver(t, pool, taskID, agent.DriverCodex)
	if _, err := pool.Exec(context.Background(),
		`UPDATE repos SET agent_driver = 'claude' WHERE id = (SELECT repo_id FROM tasks WHERE id = $1)`, taskID); err != nil {
		t.Fatal(err)
	}

	ag := &driverAgent{steps: []driverStep{
		{res: &agent.Result{Success: true, Text: triageOK}},
		{res: &agent.Result{Success: true, Text: "改完了"}, mutate: writeGreet(greetRight)},
	}}
	p := newPipeline(t, m, &fakeLinear{issue: demoIssue()},
		&fakeGitHub{pr: &github.PullRequest{Number: 61, URL: "https://github.com/acme/demo/pull/61"}}, ag, &fakeNotifier{})

	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
	}); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	calls := ag.snapshot()
	if len(calls) != 2 {
		t.Fatalf("应有分诊+实现 2 次调用，got %d", len(calls))
	}
	for i, c := range calls {
		if c.Driver != agent.DriverCodex {
			t.Errorf("第 %d 次 Run 的 Driver got %q want codex", i, c.Driver)
		}
	}
}

// 没配驱动的仓库：每次 Run 的 Driver 都是 claude，且经 agent.Router 分到的
// claude 驱动构造出的 argv 与改动前完全相同。
func TestPipelineDefaultDriverIsClaude(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	ag := &driverAgent{steps: []driverStep{
		{res: &agent.Result{Success: true, Text: triageOK}},
		{res: &agent.Result{Success: true, Text: "初版"}, mutate: writeGreet(greetWrong)},
		{res: &agent.Result{Success: true, Text: "修好了"}, mutate: writeFix(greetRight)},
	}}
	p := newPipeline(t, m, &fakeLinear{issue: demoIssue()},
		&fakeGitHub{pr: &github.PullRequest{Number: 62, URL: "https://github.com/acme/demo/pull/62"}}, ag, &fakeNotifier{})
	p.MaxFixAttempts = 1
	p.SettingSources = "project"

	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
	}); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	calls := ag.snapshot()
	if len(calls) != 3 {
		t.Fatalf("应有分诊/实现/修复 3 次调用，got %d", len(calls))
	}
	for i, c := range calls {
		if c.Driver != "" && c.Driver != agent.DriverClaude {
			t.Errorf("第 %d 次 Run 的 Driver got %q want 空或 claude", i, c.Driver)
		}
	}

	// 把流水线实现阶段真实收到的 RunParams 交给 Router → claude 驱动，
	// 假 claude 把 argv 原样写出来，与 TestBuildArgs 的既有期望逐项比对。
	argsFile := filepath.Join(t.TempDir(), "argv")
	bin := filepath.Join(t.TempDir(), "claude")
	script := fmt.Sprintf("#!/bin/bash\nprintf '%%s\\n' \"$@\" > %s\necho '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"ok\"}'\n", argsFile)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	impl := calls[1]
	impl.Dir = t.TempDir()
	impl.OnEvent, impl.OnSession = nil, nil
	if _, err := agent.NewRouter(agent.NewDriver(bin, time.Minute), nil).Run(context.Background(), impl); err != nil {
		t.Fatalf("Router→claude 失败: %v", err)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSuffix(string(raw), "\n")
	want := strings.Join([]string{"--print", "--output-format", "stream-json", "--verbose",
		"--session-id", impl.SessionID, "--permission-mode", "acceptEdits", "--setting-sources", "project", impl.Prompt}, "\n")
	if got != want {
		t.Errorf("claude argv\n got %q\nwant %q", got, want)
	}
}

// ---------------------------------------------------------------- AC-8

// 实现轮里 codex 上报的 thread_id 立刻落库，替换掉 Lathe 预分配的 UUID。
func TestPipelineCodexPersistsThreadID(t *testing.T) {
	pool, m, taskID, repo, src := pipelineFixture(t)
	pinDriver(t, pool, taskID, agent.DriverCodex)

	ag := &driverAgent{steps: []driverStep{
		{res: &agent.Result{Success: true, Text: triageOK}},
		{res: &agent.Result{Success: true, Text: "改完了"}, mutate: writeGreet(greetRight), session: "019a-thread"},
	}}
	p := newPipeline(t, m, &fakeLinear{issue: demoIssue()},
		&fakeGitHub{pr: &github.PullRequest{Number: 63, URL: "https://github.com/acme/demo/pull/63"}}, ag, &fakeNotifier{})

	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
	}); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if got := sessionOf(t, pool, taskID); got != "019a-thread" {
		t.Errorf("agent_session_id got %q want 019a-thread", got)
	}
	calls := ag.snapshot()
	if calls[0].OnSession != nil {
		t.Error("分诊会话不落 agent_session_id，不应传 OnSession")
	}
	if calls[1].OnSession == nil {
		t.Error("实现轮应传 OnSession")
	}
}

// 验证第一轮失败进入 fix-1：resume 的是 codex 上报的 thread_id。
func TestPipelineCodexFixLoopResumesThread(t *testing.T) {
	pool, m, taskID, repo, src := pipelineFixture(t)
	pinDriver(t, pool, taskID, agent.DriverCodex)

	ag := &driverAgent{steps: []driverStep{
		{res: &agent.Result{Success: true, Text: triageOK}},
		{res: &agent.Result{Success: true, Text: "初版"}, mutate: writeGreet(greetWrong), session: "019a-thread"},
		{res: &agent.Result{Success: true, Text: "修好了"}, mutate: writeFix(greetRight), session: "019a-thread"},
	}}
	p := newPipeline(t, m, &fakeLinear{issue: demoIssue()},
		&fakeGitHub{pr: &github.PullRequest{Number: 64, URL: "https://github.com/acme/demo/pull/64"}}, ag, &fakeNotifier{})
	p.MaxFixAttempts = 2

	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
	}); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	calls := ag.snapshot()
	if len(calls) != 3 {
		t.Fatalf("应有分诊/实现/fix-1 3 次调用，got %d", len(calls))
	}
	fix := calls[2]
	if fix.Driver != agent.DriverCodex || !fix.Resume || fix.SessionID != "019a-thread" {
		t.Errorf("fix-1 got {Driver:%q Resume:%v SessionID:%q} want {codex true 019a-thread}", fix.Driver, fix.Resume, fix.SessionID)
	}
	if got := sessionOf(t, pool, taskID); got != "019a-thread" {
		t.Errorf("agent_session_id got %q want 019a-thread", got)
	}
}

// fixResume=false 路径：任务没有会话、从 EntryVerify 续跑。fix-1 开新会话，
// codex 上报的 ID 必须同时进库和 rc.implSession（下一轮 fix-2 resume 的就是它），
// 而不是 Lathe 预分配的 UUID。claude 对照：库里是 fix-1 收到的预分配 ID。
func TestPipelineCodexFixLoopAdoptsReportedSession(t *testing.T) {
	cases := []struct {
		name     string
		driver   string
		reported string
	}{
		{name: "给定 codex 且 fix-1 上报 019b-new 断言库与 fix-2 都用 019b-new", driver: agent.DriverCodex, reported: "019b-new"},
		{name: "给定 claude 且不上报 断言库与 fix-2 都用 fix-1 的预分配 ID", driver: agent.DriverClaude},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool, m, taskID, repo, src := pipelineFixture(t)
			pinDriver(t, pool, taskID, tc.driver)

			var dbAtFix2 string
			ag := &driverAgent{steps: []driverStep{
				{res: &agent.Result{Success: true, Text: "fix-1 没修对"}, session: tc.reported,
					mutate: writeFix("package main\n\n// 换个注释不算修\nfunc greet() string { return \"wrong\" }\n")},
				{res: &agent.Result{Success: true, Text: "fix-2 修好了"}, mutate: writeFix(greetRight),
					hook: func(agent.RunParams) { dbAtFix2 = sessionOf(t, pool, taskID) }},
			}}
			p := newPipeline(t, m, &fakeLinear{issue: demoIssue()},
				&fakeGitHub{pr: &github.PullRequest{Number: 65, URL: "https://github.com/acme/demo/pull/65"}}, ag, &fakeNotifier{})
			p.MaxFixAttempts = 2
			prepareVerifyFailingTask(t, p, m, taskID, src, repo, "")

			if err := p.Execute(context.Background(), ExecuteParams{
				TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777",
				Retry: &RetryPlan{Entry: EntryVerify, Reasons: []string{"测试：直接重验"}},
			}); err != nil {
				t.Fatalf("Execute 失败: %v", err)
			}
			calls := ag.snapshot()
			if len(calls) != 2 {
				t.Fatalf("应有 fix-1、fix-2 两次调用，got %d", len(calls))
			}
			fix1, fix2 := calls[0], calls[1]
			if fix1.Resume || fix1.Driver != tc.driver {
				t.Errorf("fix-1 got {Resume:%v Driver:%q} want {false %q}", fix1.Resume, fix1.Driver, tc.driver)
			}
			want := tc.reported
			if want == "" {
				want = fix1.SessionID
			}
			if dbAtFix2 != want {
				t.Errorf("fix-1 返回后 agent_session_id got %q want %q", dbAtFix2, want)
			}
			// fix-2 的 SessionID 取自 rc.implSession：它等于 want 即证明 rc 也采纳了上报值
			if !fix2.Resume || fix2.SessionID != want {
				t.Errorf("fix-2 got {Resume:%v SessionID:%q} want {true %q}（rc.implSession 应采纳上报值）", fix2.Resume, fix2.SessionID, want)
			}
			if got := sessionOf(t, pool, taskID); got != want {
				t.Errorf("结束后 agent_session_id got %q want %q", got, want)
			}
		})
	}
}

// ---------------------------------------------------------------- AC-9

func codexReviewComment() github.ReviewComment {
	return github.ReviewComment{Key: "inline:91", Author: "alice", Body: "失败时至少记一条日志", Path: "README.md", Line: 3}
}

// codexReviewPipeline 把夹具任务带到「codex 实现过、已开 PR、会话是 019a-thread」，
// 返回接好 review 意见源的流水线。
func codexReviewPipeline(t *testing.T, ag AgentDriver) (*pgxpool.Pool, *task.Machine, *Pipeline, int64, RepoConfig, string, *Worktree) {
	t.Helper()
	pool, m, taskID, repo, src := pipelineFixture(t)
	pinDriver(t, pool, taskID, agent.DriverCodex)
	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{pr: &github.PullRequest{URL: "https://x/pr/11", Number: reviewPR, Existing: true}}
	p := newPipeline(t, m, lin, gh, ag, &fakeNotifier{})
	p.Clients = &fakeClients{lin: lin, gh: &feedbackGitHub{fakeGitHub: gh, fb: &github.ReviewFeedback{
		PRAuthor: "lathe-bot", Comments: []github.ReviewComment{codexReviewComment()},
	}}}
	wt := prOpenTask(t, p, m, taskID, src, repo, true)
	if err := m.SetSessionID(context.Background(), taskID, "019a-thread"); err != nil {
		t.Fatal(err)
	}
	return pool, m, p, taskID, repo, src, wt
}

func reviewPlanFor(t *testing.T, mode RetryMode, p *Pipeline, m *task.Machine, taskID int64, repo RepoConfig, wt *Worktree) *RetryPlan {
	t.Helper()
	ctx := context.Background()
	c := codexReviewComment()
	ev := inlineEvent(c.Key, c.Author, c.Body)
	intake := &ReviewIntake{Tasks: m}
	want := ReviewQueued
	if mode == RetryReviewRetry {
		intake.Workspaces = fakeWorkspaces{usable: false}
		want = ReviewParked
	}
	if out, err := intake.AcceptReview(ctx, ownerOf(t, m, taskID), ev, "d-"+string(mode)); err != nil || out.Status != want {
		t.Fatalf("回流失败：%+v %v", out, err)
	}
	if mode == RetryReview {
		return reviewRoundPlan(t, p, m, taskID, repo)
	}
	plan := PlanRetry(RetryReviewRetry, RetryInput{
		HasSession: true,
		WT:         p.Worktrees.Inspect(ctx, repo.ProviderRepo, wt.Path, wt.Branch, deref(baseOf(t, repo))),
	})
	return &plan
}

func assertCodexReviewResume(t *testing.T, call agent.RunParams) {
	t.Helper()
	if call.Driver != agent.DriverCodex || !call.Resume || call.SessionID != "019a-thread" {
		t.Errorf("review 续跑 got {Driver:%q Resume:%v SessionID:%q} want {codex true 019a-thread}", call.Driver, call.Resume, call.SessionID)
	}
	want := reviewPromptOf(&pendingReview{Comments: []github.ReviewComment{codexReviewComment()}, Source: "github"})
	if call.Prompt != want {
		t.Errorf("Prompt 应恰好是 reviewPromptOf(review)（不是 ReentryImplementPrompt）\n got %q\nwant %q", call.Prompt, want)
	}
}

func TestPipelineReviewResumeKeepsCodexSession(t *testing.T) {
	t.Run("给定 codex 任务收到 review 意见 断言 resume 019a-thread 且只喂 review 意见", func(t *testing.T) {
		ag := &driverAgent{steps: []driverStep{
			{res: &agent.Result{Success: true, Text: "按意见改完"}, mutate: writeReadme("# src\n\n第二轮：失败时记日志\n"), session: "019a-thread"},
		}}
		pool, m, p, taskID, repo, src, wt := codexReviewPipeline(t, ag)
		plan := reviewPlanFor(t, RetryReview, p, m, taskID, repo, wt)
		if err := p.Execute(context.Background(), ExecuteParams{
			TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Retry: plan,
		}); err != nil {
			t.Fatalf("二轮执行失败: %v", err)
		}
		calls := ag.snapshot()
		if len(calls) != 1 {
			t.Fatalf("应只调用 1 次 agent，got %d", len(calls))
		}
		assertCodexReviewResume(t, calls[0])
		if got := sessionOf(t, pool, taskID); got != "019a-thread" {
			t.Errorf("agent_session_id got %q want 019a-thread", got)
		}
	})

	t.Run("给定 resume 报 no rollout found 断言降级为新会话带 review 意见且采纳 codex 上报的 019c-reentry", func(t *testing.T) {
		ag := &driverAgent{steps: []driverStep{
			{err: errors.New("agent: CLI 非零退出: no rollout found for thread id 019a-thread")},
			{res: &agent.Result{Success: true, Text: "新会话改完"}, mutate: writeReadme("# src\n\n第二轮（降级）\n"), session: "019c-reentry"},
		}}
		pool, m, p, taskID, repo, src, wt := codexReviewPipeline(t, ag)
		plan := reviewPlanFor(t, RetryReview, p, m, taskID, repo, wt)
		if err := p.Execute(context.Background(), ExecuteParams{
			TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Retry: plan,
		}); err != nil {
			t.Fatalf("降级后应正常完成: %v", err)
		}
		calls := ag.snapshot()
		if len(calls) != 2 {
			t.Fatalf("应调用 2 次（resume 失败 + 降级新会话），got %d", len(calls))
		}
		if calls[1].Resume || calls[1].Driver != agent.DriverCodex {
			t.Errorf("降级那次 got {Resume:%v Driver:%q} want {false codex}", calls[1].Resume, calls[1].Driver)
		}
		if !strings.Contains(calls[1].Prompt, codexReviewComment().Body) {
			t.Error("降级新会话的 Prompt 应带上 review 意见")
		}
		if got := sessionOf(t, pool, taskID); got != "019c-reentry" {
			t.Errorf("agent_session_id got %q want 019c-reentry（不是降级分支预分配的 UUID）", got)
		}
	})
}

func TestPipelineReviewRetryKeepsCodexSession(t *testing.T) {
	ag := &driverAgent{steps: []driverStep{
		{res: &agent.Result{Success: true, Text: "按意见改完"}, mutate: writeReadme("# src\n\n第二轮（显式出口）\n"), session: "019a-thread"},
	}}
	pool, m, p, taskID, repo, src, wt := codexReviewPipeline(t, ag)
	plan := reviewPlanFor(t, RetryReviewRetry, p, m, taskID, repo, wt)
	if !plan.ResumeSession || !plan.Review || plan.Rebuild {
		t.Fatalf("现场可用且有会话时应 resume + review、不重挂，got %+v", plan)
	}
	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Retry: plan,
	}); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	calls := ag.snapshot()
	if len(calls) != 1 {
		t.Fatalf("应只调用 1 次 agent，got %d", len(calls))
	}
	assertCodexReviewResume(t, calls[0])
	if got := sessionOf(t, pool, taskID); got != "019a-thread" {
		t.Errorf("agent_session_id got %q want 019a-thread", got)
	}
}

// ---------------------------------------------------------------- AC-21

// 本节点没有 codex：钉死为 codex 的续跑直接失败，库里的会话 ID 原样保留
// （不走降级分支、不 SetSessionID）；之后在有 codex 的节点上还能 resume。
func TestPipelineCodexDriverUnavailableKeepsSession(t *testing.T) {
	entries := []struct {
		name    string
		prepare func(t *testing.T, ag AgentDriver) (*pgxpool.Pool, *task.Machine, *Pipeline, int64, RepoConfig, string, *RetryPlan)
	}{
		{name: "review_feedback 续跑", prepare: func(t *testing.T, ag AgentDriver) (*pgxpool.Pool, *task.Machine, *Pipeline, int64, RepoConfig, string, *RetryPlan) {
			pool, m, p, taskID, repo, src, wt := codexReviewPipeline(t, ag)
			return pool, m, p, taskID, repo, src, reviewPlanFor(t, RetryReview, p, m, taskID, repo, wt)
		}},
		{name: "普通续跑", prepare: func(t *testing.T, ag AgentDriver) (*pgxpool.Pool, *task.Machine, *Pipeline, int64, RepoConfig, string, *RetryPlan) {
			pool, m, taskID, repo, src := pipelineFixture(t)
			pinDriver(t, pool, taskID, agent.DriverCodex)
			p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, &fakeGitHub{}, ag, &fakeNotifier{})
			prepareFailedTask(t, p, m, taskID, src, repo, StageImplementRun, false)
			if err := m.SetSessionID(context.Background(), taskID, "019a-thread"); err != nil {
				t.Fatal(err)
			}
			return pool, m, p, taskID, repo, src, &RetryPlan{Entry: EntryImplement, ResumeSession: true, Reasons: []string{"续跑原会话"}}
		}},
	}
	routers := []struct {
		name      string
		codex     agent.Runner
		wantCalls int
		check     func(t *testing.T, pool *pgxpool.Pool, m *task.Machine, taskID int64, calls []agent.RunParams)
	}{
		{
			name:  "给定 Router 未注册 codex 断言 failed/implement_run、原因含 LATHE_CODEX_HOME、会话不变、只 Run 1 次",
			codex: nil, wantCalls: 1,
			check: func(t *testing.T, pool *pgxpool.Pool, m *task.Machine, taskID int64, calls []agent.RunParams) {
				final, err := m.Get(context.Background(), taskID)
				if err != nil {
					t.Fatal(err)
				}
				if final.State != task.StateFailed {
					t.Errorf("state got %s want failed", final.State)
				}
				if final.FailureStage == nil || *final.FailureStage != string(StageImplementRun) {
					t.Errorf("failure_stage got %v want implement_run", final.FailureStage)
				}
				if final.FailureReason == nil || !strings.Contains(*final.FailureReason, "LATHE_CODEX_HOME") {
					t.Errorf("failure_reason 应写明哪个驱动不可用，got %v", final.FailureReason)
				}
				if got := sessionOf(t, pool, taskID); got != "019a-thread" {
					t.Errorf("agent_session_id got %q want 019a-thread（不应被降级改写）", got)
				}
			},
		},
		{
			name:  "给定 Router 里的 codex 报 no rollout found（对照）断言照旧降级、第二次 Resume=false",
			codex: errRunner{err: errors.New("no rollout found for thread id 019a-thread")}, wantCalls: 2,
			check: func(t *testing.T, pool *pgxpool.Pool, m *task.Machine, taskID int64, calls []agent.RunParams) {
				if calls[1].Resume {
					t.Error("降级那次应是新会话（Resume=false）")
				}
			},
		},
	}
	for _, e := range entries {
		for _, r := range routers {
			t.Run(e.name+"/"+r.name, func(t *testing.T) {
				// claude 驱动指向一个不存在的可执行文件：被误用就会以启动失败暴露
				spy := &spyRunner{inner: agent.NewRouter(agent.NewDriver("/nonexistent/claude-should-not-run", time.Minute), r.codex)}
				pool, m, p, taskID, repo, src, plan := e.prepare(t, spy)
				_ = p.Execute(context.Background(), ExecuteParams{
					TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Retry: plan,
				})
				if len(spy.calls) != r.wantCalls {
					t.Fatalf("Run 次数 got %d want %d", len(spy.calls), r.wantCalls)
				}
				for _, c := range spy.calls {
					if c.Driver != agent.DriverCodex {
						t.Errorf("Driver got %q want codex", c.Driver)
					}
				}
				r.check(t, pool, m, taskID, spy.calls)
			})
		}
	}
}

// ---------------------------------------------------------------- AC-15

func TestMaterializeSkillsForCodex(t *testing.T) {
	m, taskID, repo, src := profileFixture(t, "CR-SKILL-CODEX",
		[]byte(`{"skills":[{"name":"go-testing","version":"1.0.0"}]}`))
	pinDriver(t, testPoolForPipeline(t), taskID, agent.DriverCodex)

	p := newPipeline(t, m, &fakeLinear{issue: demoIssue()},
		&fakeGitHub{pr: &github.PullRequest{Number: 66, URL: "https://github.com/acme/demo-profile/pull/66"}}, implFixAgent(), &fakeNotifier{})
	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "test",
	}); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	final, err := m.Get(context.Background(), taskID)
	if err != nil || final.WorktreePath == nil {
		t.Fatalf("读任务失败: %v", err)
	}
	if final.State != task.StatePROpen {
		t.Fatalf("终态 got %s want pr_open", final.State)
	}
	wt := *final.WorktreePath

	got, err := readFileT(t, filepath.Join(wt, ".codex", "skills", "go-testing", "SKILL.md"))
	if err != nil {
		t.Fatalf("codex 任务的技能应物化到 .codex/skills/: %v", err)
	}
	want, err := readFileT(t, filepath.Join(skillsSourceDir(t), "go-testing", "1.0.0", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Error("物化后的 SKILL.md 内容与 skills/go-testing/1.0.0/SKILL.md 不一致")
	}
	if _, err := os.Stat(filepath.Join(wt, ".claude", "skills")); !os.IsNotExist(err) {
		t.Errorf("codex 任务不应物化 .claude/skills，stat err = %v", err)
	}
	// 限定到技能目录：验证阶段 go build 产出的二进制（demo）也是未跟踪文件，
	// 与本断言无关（既有 claude 用例同样只看 .claude 路径）。
	out, err := exec.Command("git", "-C", wt, "status", "--porcelain", "--ignored=no", "--", ".codex", ".claude").CombinedOutput()
	if err != nil {
		t.Fatalf("git status 失败: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("git status --porcelain 在技能目录下应为空（.codex/skills/ 已排除），got:\n%s", out)
	}
	if out, err := exec.Command("git", "-C", wt, "check-ignore", "-q", ".codex/skills/go-testing/SKILL.md").CombinedOutput(); err != nil {
		t.Errorf(".codex/skills/ 应被 info/exclude 排除: %v %s", err, out)
	}
}

// ---------------------------------------------------------------- AC-4

func TestEventSinkSkipsSubagentWatchForCodex(t *testing.T) {
	cwd, session := "/opt/lathe/workspaces/cr-f06", "sess-codex"
	dir := subagentFixture(t, cwd, session)
	rec := &fakeEventRecorder{}
	s := newEventSink(context.Background(), rec, 9, "implement", agent.DriverCodex, cwd, session)
	if s.reader != nil {
		t.Fatal("codex 没有 subagent 记录契约，不应起 watcher（reader 应为 nil）")
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-a1.jsonl"),
		[]byte(fixtureSubagentPrompt+"\n"+fixtureSubagentTool+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for _, r := range rec.collected() {
		if r.entry.AgentID != "" {
			t.Errorf("codex 任务不应收到 subagent 事件: %+v", r.entry)
		}
	}
}

func TestEventSinkWatchesSubagentForClaude(t *testing.T) {
	cwd, session := "/opt/lathe/workspaces/cr-f06", "sess-claude"
	dir := subagentFixture(t, cwd, session)
	if got, want := agent.SubagentDirFor(agent.DriverClaude, cwd, session), agent.SubagentDir(agent.ProjectsRoot(), cwd, session); got != want || got != dir {
		t.Fatalf("claude 的 subagent 目录 got %q want %q（= 改动前的 SubagentDir(ProjectsRoot(),cwd,session) = %q）", got, want, dir)
	}
	rec := &fakeEventRecorder{}
	s := newEventSink(context.Background(), rec, 9, "implement", agent.DriverClaude, cwd, session)
	if s.reader == nil {
		t.Fatal("claude 应起 subagent watcher")
	}
	if err := os.WriteFile(filepath.Join(dir, "agent-a1.jsonl"),
		[]byte(fixtureSubagentPrompt+"\n"+fixtureSubagentTool+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Close()
	sub := 0
	for _, r := range rec.collected() {
		if r.entry.AgentID == "a1" {
			sub++
		}
	}
	if sub != 2 {
		t.Errorf("subagent 事件数 got %d want 2", sub)
	}
}
