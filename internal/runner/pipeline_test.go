package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Clouditera/lathe/internal/integration/agent"
	"github.com/Clouditera/lathe/internal/integration/github"
	"github.com/Clouditera/lathe/internal/integration/linear"
	"github.com/Clouditera/lathe/internal/task"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------- 假件

type fakeLinear struct {
	issue    *linear.Issue
	issueErr error
	comments []string
}

func (f *fakeLinear) Issue(ctx context.Context, id string) (*linear.Issue, error) {
	if f.issueErr != nil {
		return nil, f.issueErr
	}
	return f.issue, nil
}

func (f *fakeLinear) Comment(ctx context.Context, issueID, body string) (string, error) {
	f.comments = append(f.comments, body)
	return "c1", nil
}

type fakeGitHub struct {
	pr     *github.PullRequest
	err    error
	params []github.PRParams
}

func (f *fakeGitHub) CreatePR(ctx context.Context, p github.PRParams) (*github.PullRequest, error) {
	f.params = append(f.params, p)
	if f.err != nil {
		return nil, f.err
	}
	return f.pr, nil
}

// fakeAgent 按调用次序返回预设结果；可选地在工作区里制造改动。
type fakeAgent struct {
	results []*agent.Result
	errs    []error
	mutate  []func(dir string) error
	calls   []agent.RunParams
}

func (f *fakeAgent) Run(ctx context.Context, p agent.RunParams) (*agent.Result, error) {
	i := len(f.calls)
	f.calls = append(f.calls, p)

	if i < len(f.mutate) && f.mutate[i] != nil {
		if err := f.mutate[i](p.Dir); err != nil {
			return nil, err
		}
	}
	if i < len(f.errs) && f.errs[i] != nil {
		return nil, f.errs[i]
	}
	if i < len(f.results) {
		return f.results[i], nil
	}
	return &agent.Result{Success: true, Text: "done"}, nil
}

// fakeClients 让流水线在测试里拿到固定的假客户端。
type fakeClients struct {
	lin LinearAPI
	gh  GitHubAPI
	err error
}

func (f *fakeClients) Linear(ctx context.Context) (LinearAPI, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.lin, nil
}

func (f *fakeClients) GitHub(ctx context.Context) (GitHubAPI, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.gh, nil
}

type fakeNotifier struct{ msgs []string }

func (f *fakeNotifier) Notify(ctx context.Context, m string) error {
	f.msgs = append(f.msgs, m)
	return nil
}

// ---------------------------------------------------------------- 夹具

// goSourceRepo 造一个含可构建 go 模块的仓库，让 light 档验证有真东西可跑。
func goSourceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@e.st",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@e.st")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	writeFile(t, filepath.Join(dir, "go.mod"), "module demo\n\ngo 1.25\n")
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")

	run("init", "--quiet", "--initial-branch=main")
	run("add", ".")
	run("commit", "--quiet", "-m", "初始提交")
	run("branch", "dev")
	return dir
}

func pipelineFixture(t *testing.T) (*pgxpool.Pool, *task.Machine, int64, RepoConfig, string) {
	t.Helper()
	pool := testPoolForPipeline(t)
	m := task.NewMachine(pool)
	ctx := context.Background()

	var userID, repoID int64
	email := "pipe-" + t.Name() + "@example.com"
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) ON CONFLICT (email) DO UPDATE SET updated_at=now() RETURNING id`,
		email).Scan(&userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2)
		 ON CONFLICT (user_id, provider_repo) DO UPDATE SET updated_at=now() RETURNING id`,
		userID, "acme/demo").Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})

	tk, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "CR-777",
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	return pool, m, tk.ID, DefaultRepoConfig("acme/demo"), goSourceRepo(t)
}

func testPoolForPipeline(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("LATHE_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://lathe:lathe@127.0.0.1:55432/lathe?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("跳过数据库测试: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("跳过数据库测试（先 make dev-infra && make migrate）: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func demoIssue() *linear.Issue {
	return &linear.Issue{
		ID: "uuid-777", Identifier: "CR-777",
		Title: "Import fails silently", Description: "点导入没反应，期望弹出文件选择框",
		URL: "https://linear.app/x/CR-777",
	}
}

func newPipeline(t *testing.T, m *task.Machine, lin *fakeLinear, gh *fakeGitHub, ag *fakeAgent, no *fakeNotifier) *Pipeline {
	t.Helper()
	wm, err := NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatalf("建工作区管理器失败: %v", err)
	}
	return &Pipeline{
		Tasks: m, Worktrees: wm,
		Verifier:       NewVerifier(3*time.Minute, ""),
		Agent:          ag,
		Clients:        &fakeClients{lin: lin, gh: gh},
		Notifier:       no,
		PermissionMode: "acceptEdits",
	}
}

// ---------------------------------------------------------------- 主链路

func TestPipelineHappyPath(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{pr: &github.PullRequest{Number: 42, URL: "https://github.com/acme/demo/pull/42"}}
	ag := &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象和期望行为","question":""}`},
			{Success: true, Text: "改了导入按钮的事件绑定"},
		},
		mutate: []func(string) error{
			nil, // 分诊不改文件
			func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "fix.go"),
					[]byte("package main\n\nfunc fixed() {}\n"), 0o644)
			},
		},
	}
	no := &fakeNotifier{}
	p := newPipeline(t, m, lin, gh, ag, no)

	err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777", Actor: "node:test",
	})
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}

	ctx := context.Background()
	final, err := m.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if final.State != task.StatePROpen {
		t.Errorf("终态 = %s，期望 pr_open", final.State)
	}
	if final.PRURL == nil || *final.PRURL != "https://github.com/acme/demo/pull/42" {
		t.Errorf("PR URL 未落库: %v", final.PRURL)
	}
	if final.BranchName == nil || !strings.HasPrefix(*final.BranchName, "fix/cr-777") {
		t.Errorf("分支名不符: %v", final.BranchName)
	}
	if final.AgentSessionID == nil || *final.AgentSessionID == "" {
		t.Error("会话 ID 应已落库（review 二轮要靠它 --resume）")
	}
	if final.VerifyTier == nil || *final.VerifyTier != "light" {
		t.Errorf("验证档位不符: %v", final.VerifyTier)
	}

	// 状态轨迹必须完整且可重放
	replayed, err := m.Replay(ctx, taskID)
	if err != nil {
		t.Fatalf("Replay 失败: %v", err)
	}
	if replayed != task.StatePROpen {
		t.Errorf("Replay = %s", replayed)
	}
	events, _ := m.Events(ctx, taskID)
	var trail []string
	for _, e := range events {
		trail = append(trail, string(e.ToState))
	}
	want := "queued→triaging→implementing→verifying→pr_open"
	if got := strings.Join(trail, "→"); got != want {
		t.Errorf("状态轨迹 = %s，期望 %s", got, want)
	}

	// PR 正文必须带验证证据
	if len(gh.params) != 1 {
		t.Fatalf("应创建 1 个 PR，实际 %d", len(gh.params))
	}
	if !strings.Contains(gh.params[0].Body, "验证通过") {
		t.Errorf("PR 正文应含验证证据:\n%s", gh.params[0].Body)
	}
	if gh.params[0].Base != "dev" {
		t.Errorf("PR base = %q，fix 类应指向 dev", gh.params[0].Base)
	}

	// 分诊必须是只读的
	if ag.calls[0].PermissionMode != "plan" {
		t.Errorf("分诊应以只读模式运行，实际 %q", ag.calls[0].PermissionMode)
	}
	// 两次调用应使用不同会话
	if ag.calls[0].SessionID == ag.calls[1].SessionID {
		t.Error("分诊与实现不应共用会话")
	}

	if len(lin.comments) != 1 || !strings.Contains(lin.comments[0], "pull/42") {
		t.Errorf("应回帖 PR 链接，实际: %v", lin.comments)
	}
	if len(no.msgs) != 0 {
		t.Errorf("成功不应推送失败通知: %v", no.msgs)
	}
}

// 分诊判定不明确：回帖提问、转 blocked_spec、不建工作区、不碰代码。
func TestPipelineBlockedSpec(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issue: &linear.Issue{ID: "uuid-777", Identifier: "CR-777", Title: "登录有问题"}}
	gh := &fakeGitHub{}
	ag := &fakeAgent{results: []*agent.Result{
		{Success: true, Text: `{"actionable":false,"kind":"fix","reason":"只有一句现象，没有复现路径","question":"能补充一下复现步骤和期望行为吗？"}`},
	}}
	no := &fakeNotifier{}
	p := newPipeline(t, m, lin, gh, ag, no)

	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777",
	}); err != nil {
		t.Fatalf("blocked_spec 是正常出口，不应返回错误: %v", err)
	}

	final, _ := m.Get(context.Background(), taskID)
	if final.State != task.StateBlockedSpec {
		t.Errorf("状态 = %s，期望 blocked_spec", final.State)
	}
	if len(ag.calls) != 1 {
		t.Errorf("不明确的单不应进入实现阶段，agent 被调用 %d 次", len(ag.calls))
	}
	if len(gh.params) != 0 {
		t.Error("不应创建 PR")
	}
	if len(lin.comments) != 1 || !strings.Contains(lin.comments[0], "复现步骤") {
		t.Errorf("应回帖具体问题，实际: %v", lin.comments)
	}
	if final.WorktreePath != nil {
		t.Errorf("不应创建工作区: %v", *final.WorktreePath)
	}
}

// ★D4 失败三件套：验证不过时回帖 + 保留现场 + 推送通知，且不自动重试。
func TestPipelineVerificationFailurePreservesScene(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{}
	ag := &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix"}`},
			{Success: true, Text: "改完了"},
		},
		mutate: []func(string) error{
			nil,
			func(dir string) error { // 写入编译不过的代码
				return os.WriteFile(filepath.Join(dir, "broken.go"),
					[]byte("package main\n\nthis is not valid go\n"), 0o644)
			},
		},
	}
	no := &fakeNotifier{}
	p := newPipeline(t, m, lin, gh, ag, no)

	err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777",
	})
	if err == nil {
		t.Fatal("验证不过应返回错误")
	}

	final, _ := m.Get(context.Background(), taskID)
	if final.State != task.StateFailed {
		t.Errorf("状态 = %s，期望 failed", final.State)
	}
	if final.FailureReason == nil || !strings.Contains(*final.FailureReason, "验证未通过") {
		t.Errorf("失败原因应说明验证未通过: %v", final.FailureReason)
	}

	// 三件套逐条核对
	if len(lin.comments) == 0 || !strings.Contains(lin.comments[len(lin.comments)-1], "处理失败") {
		t.Errorf("① 应回帖说明失败: %v", lin.comments)
	}
	if final.WorktreePath == nil {
		t.Fatal("② 工作区路径应已落库")
	}
	if _, statErr := os.Stat(*final.WorktreePath); statErr != nil {
		t.Errorf("② 失败任务必须保留现场，工作区却不在了: %v", statErr)
	}
	if len(no.msgs) != 1 {
		t.Errorf("③ 应推送 1 条通知，实际 %v", no.msgs)
	}

	if len(gh.params) != 0 {
		t.Error("验证不过绝不能开 PR")
	}
}

// agent 跑完却没改任何东西，属于失败而非「空改动的成功」。
func TestPipelineNoChangesIsFailure(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{}
	ag := &fakeAgent{results: []*agent.Result{
		{Success: true, Text: `{"actionable":true,"kind":"fix"}`},
		{Success: true, Text: "我看了一下，没什么要改的"},
	}}
	no := &fakeNotifier{}
	p := newPipeline(t, m, lin, gh, ag, no)

	err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777",
	})
	if err == nil {
		t.Fatal("无改动应判为失败")
	}

	final, _ := m.Get(context.Background(), taskID)
	if final.State != task.StateFailed {
		t.Errorf("状态 = %s，期望 failed", final.State)
	}
	if final.FailureReason == nil || !strings.Contains(*final.FailureReason, "没有产生任何改动") {
		t.Errorf("失败原因应指明无改动: %v", final.FailureReason)
	}
	if len(gh.params) != 0 {
		t.Error("不应开 PR")
	}
}

// 拉 issue 就失败：此时还没有工作区，三件套里的「保留现场」自然缺席，
// 但回帖与通知仍须发出。
func TestPipelineIssueFetchFailure(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issueErr: errors.New("401 unauthorized")}
	gh := &fakeGitHub{}
	ag := &fakeAgent{}
	no := &fakeNotifier{}
	p := newPipeline(t, m, lin, gh, ag, no)

	err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777",
	})
	if err == nil {
		t.Fatal("拉 issue 失败应返回错误")
	}

	final, _ := m.Get(context.Background(), taskID)
	if final.State != task.StateFailed {
		t.Errorf("状态 = %s，期望 failed", final.State)
	}
	if len(ag.calls) != 0 {
		t.Error("拉不到 issue 就不该调 agent")
	}
	if len(no.msgs) != 1 {
		t.Errorf("仍应推送通知: %v", no.msgs)
	}
}

func TestPipelineTriageUnparsable(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issue: demoIssue()}
	ag := &fakeAgent{results: []*agent.Result{{Success: true, Text: "我觉得这单挺清楚的，可以做"}}}
	no := &fakeNotifier{}
	p := newPipeline(t, m, lin, &fakeGitHub{}, ag, no)

	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777",
	}); err == nil {
		t.Fatal("分诊输出无法解析应失败，而不是猜一个结论继续跑")
	}

	final, _ := m.Get(context.Background(), taskID)
	if final.State != task.StateFailed {
		t.Errorf("状态 = %s，期望 failed", final.State)
	}
}

// ---------------------------------------------------------------- 分诊解析

func TestParseTriageVerdict(t *testing.T) {
	v, err := ParseTriageVerdict(`{"actionable":true,"kind":"feature","reason":"有验收标准","question":""}`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !v.Actionable || v.Kind != KindFeature {
		t.Errorf("解析结果不符: %+v", v)
	}

	// 容忍前后附带说明文字
	v, err = ParseTriageVerdict("我的判断如下：\n```json\n{\"actionable\":false,\"kind\":\"fix\",\"reason\":\"缺复现\"}\n```\n以上。")
	if err != nil {
		t.Fatalf("应容忍 JSON 前后的说明文字: %v", err)
	}
	if v.Actionable {
		t.Error("actionable 应为 false")
	}
	// 未给出 question 时应兜底，避免回帖空白
	if v.Question == "" {
		t.Error("不可执行却没给问题时应兜底一句")
	}
	if !strings.Contains(v.Question, "缺复现") {
		t.Errorf("兜底问题应带上理由: %q", v.Question)
	}

	// 类型非法时退化为 fix，不让整单失败
	v, _ = ParseTriageVerdict(`{"actionable":true,"kind":"什么鬼"}`)
	if v.Kind != KindFix {
		t.Errorf("非法类型应退化为 fix，得到 %q", v.Kind)
	}

	for _, bad := range []string{"", "没有 JSON", "{不是合法JSON"} {
		if _, err := ParseTriageVerdict(bad); err == nil {
			t.Errorf("%q 应解析失败", bad)
		}
	}
}

func TestPromptsForbidGitOperations(t *testing.T) {
	// agent 绝不能自己 commit/push —— 提交由流水线统一负责
	impl := ImplementPrompt("issue 内容", KindFix, "fix/cr-1")
	for _, want := range []string{"不要执行 git commit", "fix/cr-1", "issue 内容"} {
		if !strings.Contains(impl, want) {
			t.Errorf("实现 prompt 应含 %q", want)
		}
	}

	review := ReviewPrompt([]string{"这里改一下", "那里也是"})
	if !strings.Contains(review, "不要执行 git commit") {
		t.Error("review prompt 也应禁止 git 操作")
	}
	if !strings.Contains(review, "1. 这里改一下") {
		t.Errorf("review prompt 应逐条列出意见:\n%s", review)
	}

	triage := TriagePrompt("issue 内容")
	if !strings.Contains(triage, "不要修改任何文件") {
		t.Error("分诊 prompt 必须明确禁止改文件")
	}
}

func TestNewUUIDFormat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		u := newUUID()
		if len(u) != 36 {
			t.Fatalf("UUID 长度 = %d，期望 36: %q", len(u), u)
		}
		if u[14] != '4' {
			t.Errorf("应为 v4 UUID: %q", u)
		}
		if !strings.ContainsRune("89ab", rune(u[19])) {
			t.Errorf("variant 位不符: %q", u)
		}
		if seen[u] {
			t.Fatalf("UUID 重复: %q", u)
		}
		seen[u] = true
	}
}

// 凭据未配置时应给出可操作的提示，而不是含糊的失败。
func TestPipelineMissingCredentials(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, &fakeGitHub{}, &fakeAgent{}, &fakeNotifier{})
	p.Clients = &fakeClients{err: errors.New("creds: 凭据未配置（linear）")}

	err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777",
	})
	if err == nil {
		t.Fatal("缺凭据应报错")
	}
	if !strings.Contains(err.Error(), "设置里配置") {
		t.Errorf("错误应指引去设置页配置凭据，得到: %v", err)
	}
}

// 客户端在每次 Execute 时现取 —— 这是「改完凭据无需重启」的基础。
func TestPipelineFetchesClientsPerExecution(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issue: demoIssue()}
	clients := &countingClients{fakeClients: fakeClients{lin: lin, gh: &fakeGitHub{}}}

	p := newPipeline(t, m, lin, &fakeGitHub{}, &fakeAgent{
		results: []*agent.Result{{Success: true, Text: `{"actionable":false,"kind":"fix","reason":"缺复现"}`}},
	}, &fakeNotifier{})
	p.Clients = clients

	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777",
	}); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}

	if clients.linearCalls == 0 {
		t.Error("应在执行时现取 Linear 客户端，而非使用启动时固定的实例")
	}
}

type countingClients struct {
	fakeClients
	linearCalls int
}

func (c *countingClients) Linear(ctx context.Context) (LinearAPI, error) {
	c.linearCalls++
	return c.fakeClients.Linear(ctx)
}
