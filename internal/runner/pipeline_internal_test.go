package runner

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Clouditera/lathe/internal/integration/agent"
	"github.com/Clouditera/lathe/internal/integration/github"
	"github.com/Clouditera/lathe/internal/store"
	"github.com/Clouditera/lathe/internal/task"
	"github.com/Clouditera/lathe/internal/tracker"
)

// providerDispatchClients 按 provider 分派的假客户端：internal 走真
// LocalTracker（DB 支撑），linear 一旦被动用就报错 —— 内置任务「零外发」
//（09 §3 F4-AC6）就靠这个硬断言守着。
type providerDispatchClients struct {
	st      *store.Store
	ownerID int64
	gh      GitHubAPI
	// linearTouched 记录 linear 分支被动过几次（期望恒为 0）
	linearTouched int
}

func (c *providerDispatchClients) Tracker(ctx context.Context, provider string) (tracker.Tracker, error) {
	if provider == tracker.ProviderInternal {
		return tracker.NewLocalTracker(c.st, c.ownerID), nil
	}
	c.linearTouched++
	return nil, errLinearShouldNotBeUsed
}

var errLinearShouldNotBeUsed = errorString("内置任务不该动用 Linear 通道")

type errorString string

func (e errorString) Error() string { return string(e) }

func (c *providerDispatchClients) GitHub(ctx context.Context) (GitHubAPI, error) {
	return c.gh, nil
}

// TestPipelineInternalIssueHappyPath 是 09 §3 F4-AC6 + §5 P1 出口条件的
// 集成形态：内置工单从建单跑到 pr_open，全程无 Linear；
// PR 链接落在内置评论区（与 Linear 回帖同一段 pipeline 代码）；
// 分诊上下文来自工单行自身（标题/描述/评论）。
func TestPipelineInternalIssueHappyPath(t *testing.T) {
	pool, m, _, repo, src := pipelineFixture(t) // 建 user/repo/任务行的通用夹具
	ctx := context.Background()

	// 上面的夹具建的是 Linear 任务（CR-777），本测试自己建内置工单与任务。
	// 取夹具里的 user/repo：任务的属主与仓库必须自洽。
	var userID, repoID int64
	if err := pool.QueryRow(ctx,
		`SELECT user_id, repo_id FROM tasks WHERE external_key = 'CR-777'
		 ORDER BY id DESC LIMIT 1`).Scan(&userID, &repoID); err != nil {
		t.Fatalf("读夹具任务失败: %v", err)
	}
	st, err := store.Open(ctx, dsnForTest())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	it, err := st.CreateIssue(ctx, store.CreateIssueParams{
		UserID: userID, RepoID: repoID,
		Title: "导入按钮无响应", Description: "点击导入没反应，期望弹出文件选择框",
	})
	if err != nil {
		t.Fatalf("建内置工单失败: %v", err)
	}
	// 工单上先有一条人的补充评论 —— 分诊上下文必须带上它（评论即需求）
	if _, err := st.AddComment(ctx, it.ID, &userID, nil, "补充：只在生产环境复现"); err != nil {
		t.Fatal(err)
	}

	tk, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: it.Key,
		TrackerProvider: tracker.ProviderInternal,
	})
	if err != nil {
		t.Fatalf("建内置任务失败: %v", err)
	}

	gh := &fakeGitHub{pr: &github.PullRequest{Number: 77, URL: "https://github.com/acme/demo/pull/77"}}
	ag := &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象和期望","question":""}`},
			{Success: true, Text: "补了 greet 与复现测试"},
		},
		mutate: []func(string) error{
			nil,
			func(dir string) error {
				if err := os.WriteFile(filepath.Join(dir, "main_test.go"),
					[]byte("package main\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif greet() != \"hello\" {\n\t\tt.Fatalf(\"got %q\", greet())\n\t}\n}\n"), 0o644); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(dir, "fix.go"),
					[]byte("package main\n\nfunc greet() string { return \"hello\" }\n"), 0o644)
			},
		},
	}
	clients := &providerDispatchClients{st: st, ownerID: userID, gh: gh}

	wm, err := NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &Pipeline{
		Tasks: m, Worktrees: wm,
		Verifier:       NewVerifier(3*time.Minute, ""),
		Agent:          ag,
		Clients:        clients,
		Notifier:       &fakeNotifier{},
		PermissionMode: "acceptEdits",
		SettingSources: "project",
		Verifications:  &fakeVerifications{},
	}

	if err := p.Execute(ctx, ExecuteParams{
		TaskID: tk.ID, Repo: repo, CloneURL: src, IssueRef: it.Key, Actor: "node:test",
	}); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}

	final, err := m.Get(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != task.StatePROpen {
		t.Fatalf("内置任务应跑到 pr_open，终态 = %s", final.State)
	}

	// F4-AC6：Linear 通道一次都没被动过
	if clients.linearTouched != 0 {
		t.Errorf("内置任务动用了 Linear 通道 %d 次，期望 0", clients.linearTouched)
	}

	// 分支名用内置 key 走 branch_pattern（09 §3 F4-AC5）。
	// 中文标题 slug 为空，{key}-{slug} 会收掉尾横杠，故断言前缀即可。
	if final.BranchName == nil || !strings.HasPrefix(*final.BranchName, "fix/"+strings.ToLower(it.Key)) {
		t.Errorf("分支名不符合 pattern（fix/{key}[-{slug}]）: %v", *final.BranchName)
	}

	// PR 回帖落在内置评论区，署名 task-<id>（与 Linear 回帖同一段代码的产物）
	comments, err := st.ListComments(ctx, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundPR := false
	for _, c := range comments {
		if c.Actor != nil && *c.Actor == "task-"+itoa(final.ID) && strings.Contains(c.Body, "pull/77") {
			foundPR = true
		}
	}
	if !foundPR {
		t.Errorf("内置评论区应出现任务 #%d 的 PR 回帖: %+v", final.ID, comments)
	}

	// 分诊上下文必须来自工单行自身：标题、描述、人的补充评论
	triagePrompt := ag.calls[0].Prompt
	for _, want := range []string{it.Key, "导入按钮无响应", "弹出文件选择框", "只在生产环境复现"} {
		if !strings.Contains(triagePrompt, want) {
			t.Errorf("分诊上下文缺少 %q", want)
		}
	}
}

// dsnForTest 与 pipeline_test.go 的 testPoolForPipeline 口径一致。
func dsnForTest() string {
	if dsn := os.Getenv("LATHE_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://lathe:lathe@127.0.0.1:55432/lathe?sslmode=disable"
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// TestPipelineInternalBlockedSpecRoundTrip 覆盖 09 §3 F2-AC2/AC3 的完整回路：
// 分诊判定需求不清 → 提问写内置评论区（署名 task-<id>）→ 人在评论区补充
// → 重试时分诊上下文带上问答双方。与 Linear 路径逐字节同一段代码。
func TestPipelineInternalBlockedSpecRoundTrip(t *testing.T) {
	pool, m, _, repo, src := pipelineFixture(t)
	ctx := context.Background()

	var userID, repoID int64
	if err := pool.QueryRow(ctx,
		`SELECT user_id, repo_id FROM tasks WHERE external_key = 'CR-777'
		 ORDER BY id DESC LIMIT 1`).Scan(&userID, &repoID); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, dsnForTest())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 故意写得含糊的单
	it, err := st.CreateIssue(ctx, store.CreateIssueParams{
		UserID: userID, RepoID: repoID, Title: "登录有问题",
	})
	if err != nil {
		t.Fatal(err)
	}
	tk, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: it.Key,
		TrackerProvider: tracker.ProviderInternal,
	})
	if err != nil {
		t.Fatal(err)
	}

	gh := &fakeGitHub{}
	ag := &fakeAgent{results: []*agent.Result{
		{Success: true, Text: `{"actionable":false,"kind":"fix","reason":"只有一句现象","question":"能补充一下复现步骤和期望行为吗？"}`},
	}}
	clients := &providerDispatchClients{st: st, ownerID: userID, gh: gh}

	wm, _ := NewWorktreeManager(t.TempDir())
	p := &Pipeline{
		Tasks: m, Worktrees: wm,
		Verifier: NewVerifier(3*time.Minute, ""), Agent: ag,
		Clients: clients, Notifier: &fakeNotifier{},
		PermissionMode: "acceptEdits", SettingSources: "project",
	}

	if err := p.Execute(ctx, ExecuteParams{
		TaskID: tk.ID, Repo: repo, CloneURL: src, IssueRef: it.Key, Actor: "node:test",
	}); err != nil {
		t.Fatalf("blocked_spec 是正常出口: %v", err)
	}

	final, _ := m.Get(ctx, tk.ID)
	if final.State != task.StateBlockedSpec {
		t.Fatalf("应转 blocked_spec，得到 %s", final.State)
	}

	// 提问落在内置评论区，署名 task-<id>
	comments, _ := st.ListComments(ctx, it.ID)
	if len(comments) != 1 || comments[0].Actor == nil ||
		*comments[0].Actor != "task-"+itoa(tk.ID) ||
		!strings.Contains(comments[0].Body, "复现步骤") {
		t.Fatalf("提问评论不符: %+v", comments)
	}
	if clients.linearTouched != 0 {
		t.Errorf("内置任务动用了 Linear 通道 %d 次", clients.linearTouched)
	}

	// 人在评论区补充（与 Linear 上「在 issue 下回复」同一动作语义）
	if _, err := st.AddComment(ctx, it.ID, &userID, nil, "复现步骤：登录页输任意账号点登录；期望：跳转首页，实际：停留不动"); err != nil {
		t.Fatal(err)
	}

	// 重试（fresh）：分诊重跑，上下文必须带上「提问 + 人的回答」双方
	gh.pr = &github.PullRequest{Number: 78, URL: "https://github.com/acme/demo/pull/78"}
	ag2 := &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有复现步骤与期望行为","question":""}`},
			{Success: true, Text: "补了 greet 与复现测试"},
		},
		mutate: []func(string) error{
			nil,
			func(dir string) error {
				if err := os.WriteFile(filepath.Join(dir, "main_test.go"),
					[]byte("package main\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif greet() != \"hello\" {\n\t\tt.Fatalf(\"got %q\", greet())\n\t}\n}\n"), 0o644); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(dir, "fix.go"),
					[]byte("package main\n\nfunc greet() string { return \"hello\" }\n"), 0o644)
			},
		},
	}
	clients2 := &providerDispatchClients{st: st, ownerID: userID, gh: gh}
	p2 := &Pipeline{
		Tasks: m, Worktrees: wm,
		Verifier: NewVerifier(3*time.Minute, ""), Agent: ag2,
		Clients: clients2, Notifier: &fakeNotifier{},
		PermissionMode: "acceptEdits", SettingSources: "project",
	}
	// 模拟人工重试：blocked_spec → queued（合法边），再执行
	if _, err := m.Transition(ctx, tk.ID, task.StateQueued, "test", nil); err != nil {
		t.Fatal(err)
	}
	if err := p2.Execute(ctx, ExecuteParams{
		TaskID: tk.ID, Repo: repo, CloneURL: src, IssueRef: it.Key, Actor: "node:test",
	}); err != nil {
		t.Fatalf("重试执行失败: %v", err)
	}

	prompt := ag2.calls[0].Prompt
	for _, want := range []string{"复现步骤和期望行为吗", "登录页输任意账号", "期望：跳转首页"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("重试的分诊上下文缺少 %q", want)
		}
	}
}
