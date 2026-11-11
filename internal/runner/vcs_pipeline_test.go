package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
	"github.com/zichuanwangcloud-gif/lathe/internal/integration/github"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
	"github.com/zichuanwangcloud-gif/lathe/internal/tracker"
	"github.com/zichuanwangcloud-gif/lathe/internal/vcs"
	"github.com/zichuanwangcloud-gif/lathe/internal/vcs/gitlab"
)

// ---------------------------------------------------------------- AC-24b

// f09StyleClients 照 f09 的 e2eClients 形态写：只实现 Tracker 与 GitHub，后者
// 返回嵌入 GitHubAPI 的空结构体。编译通过即证明 f07 合入后的 dev 上它仍能编译。
type f09StyleClients struct{}

type f09StyleGitHub struct{ GitHubAPI }

func (f09StyleClients) Tracker(context.Context, string) (tracker.Tracker, error) { return nil, nil }
func (f09StyleClients) GitHub(context.Context) (GitHubAPI, error)                { return f09StyleGitHub{}, nil }

var _ Clients = f09StyleClients{}

// GitHubAPI 与 vcs.Client 方法集相同，可互相赋值（别名带来的编译期兼容）。
var (
	_ vcs.Client = GitHubAPI(nil)
	_ GitHubAPI  = vcs.Client(nil)
)

func TestClientsInterfaceUnchanged(t *testing.T) {
	methods := func(typ reflect.Type) []string {
		var out []string
		for i := 0; i < typ.NumMethod(); i++ {
			out = append(out, typ.Method(i).Name)
		}
		return out
	}
	clients := reflect.TypeOf((*Clients)(nil)).Elem()
	if got := methods(clients); !reflect.DeepEqual(got, []string{"GitHub", "Tracker"}) {
		t.Errorf("Clients 方法集 = %v，want [GitHub Tracker]（f09 的假件依赖它不变）", got)
	}
	gh, _ := clients.MethodByName("GitHub")
	if got := gh.Type.String(); got != "func(context.Context) (runner.GitHubAPI, error)" {
		t.Errorf("Clients.GitHub 签名 = %s", got)
	}
	if got := methods(reflect.TypeOf((*GitHubAPI)(nil)).Elem()); !reflect.DeepEqual(got, []string{"CreatePR", "GetPRInfo"}) {
		t.Errorf("GitHubAPI 方法集 = %v，want [CreatePR GetPRInfo]", got)
	}
}

// vcsClients 是额外实现 VCSClients 的假件（只给 f07 自己的测试用）。
type vcsClients struct {
	*fakeClients
	client  GitHubAPI
	auth    vcs.GitAuth
	err     error
	targets []vcs.Target
	ghCalls atomic.Int32
}

func (c *vcsClients) GitHub(ctx context.Context) (GitHubAPI, error) {
	c.ghCalls.Add(1)
	return c.fakeClients.GitHub(ctx)
}

func (c *vcsClients) VCS(ctx context.Context, t vcs.Target) (GitHubAPI, vcs.GitAuth, error) {
	c.targets = append(c.targets, t)
	if c.err != nil {
		return nil, vcs.GitAuth{}, c.err
	}
	return c.client, c.auth, nil
}

func TestResolveVCSDispatch(t *testing.T) {
	ctx := context.Background()
	gh := &fakeGitHub{}
	t.Run("github 只调 GitHub，auth 零值", func(t *testing.T) {
		c := &vcsClients{fakeClients: &fakeClients{gh: gh}, auth: vcs.NewGitAuth("u", "should-not-be-used")}
		got, auth, err := resolveVCS(ctx, c, vcs.Target{Provider: vcs.ProviderGitHub})
		if err != nil || got != GitHubAPI(gh) || !auth.IsZero() {
			t.Fatalf("resolveVCS(github) = %v %v %v", got, auth, err)
		}
		if c.ghCalls.Load() != 1 || len(c.targets) != 0 {
			t.Errorf("GitHub 调用 %d 次、VCS 调用 %d 次，want 1/0", c.ghCalls.Load(), len(c.targets))
		}
	})
	t.Run("gitlab + 未实现 VCSClients 的假件 → 不支持", func(t *testing.T) {
		_, _, err := resolveVCS(ctx, &fakeClients{gh: gh}, vcs.Target{Provider: vcs.ProviderGitLab, BaseURL: "https://g"})
		if err == nil || !strings.Contains(err.Error(), "不支持代码平台") {
			t.Fatalf("err = %v，want 含「不支持代码平台」", err)
		}
	})
	t.Run("gitlab + 实现了的假件 → 调 VCS 且 target 原样传入", func(t *testing.T) {
		tok := vcs.NewGitAuth("oauth2", "tok-x")
		c := &vcsClients{fakeClients: &fakeClients{gh: gh}, client: gh, auth: tok}
		target := vcs.Target{Provider: vcs.ProviderGitLab, BaseURL: "https://gitlab.example.com"}
		_, auth, err := resolveVCS(ctx, c, target)
		if err != nil || auth != tok {
			t.Fatalf("resolveVCS = %v %v", auth, err)
		}
		if len(c.targets) != 1 || c.targets[0] != target || c.ghCalls.Load() != 0 {
			t.Errorf("targets = %v ghCalls = %d", c.targets, c.ghCalls.Load())
		}
	})
}

// ---------------------------------------------------------------- AC-19

func gitlabRepoConfig(base string) RepoConfig {
	cfg := DefaultRepoConfig("group/sub/demo")
	cfg.VCSProvider, cfg.VCSBaseURL = vcs.ProviderGitLab, base
	return cfg
}

// gitlabAPIStub 是 GitLab REST 的最小假服务器（开 MR 用）。
type gitlabAPIStub struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []string
}

func newGitLabAPIStub(t *testing.T) *gitlabAPIStub {
	s := &gitlabAPIStub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.reqs = append(s.reqs, r.Method+" "+r.URL.EscapedPath())
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.EscapedPath(), "/merge_requests"):
			_, _ = w.Write([]byte("[]"))
		case r.Method == "POST" && strings.HasSuffix(r.URL.EscapedPath(), "/merge_requests"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"iid": 12, "web_url": "https://gitlab.example.com/group/sub/demo/-/merge_requests/12"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *gitlabAPIStub) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reqs...)
}

// TestPipelineOpensPROnRepoPlatform：仓库在 GitLab 上 → MR 开到 GitLab，GitHub 0 请求。
func TestPipelineOpensPROnRepoPlatform(t *testing.T) {
	_, m, taskID, _, src := pipelineFixture(t)
	repo := gitlabRepoConfig("https://gitlab.example.com")
	api := newGitLabAPIStub(t)
	gl, err := gitlab.New(api.srv.URL, "glpat-pipe")
	if err != nil {
		t.Fatal(err)
	}
	gh := &fakeGitHub{pr: &github.PullRequest{Number: 1, URL: "https://github.com/x/1"}}
	ag := &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"清楚","question":""}`},
			{Success: true, Text: "补了说明"},
		},
		mutate: []func(string) error{nil, func(dir string) error {
			writeFile(t, filepath.Join(dir, "README.md"), "# src\n\n补充说明\n")
			return nil
		}},
	}
	p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, gh, ag, &fakeNotifier{})
	vc := &vcsClients{fakeClients: p.Clients.(*fakeClients), client: gl} // 零值 auth：git 远端用本地 src
	p.Clients = vc

	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	reqs := api.requests()
	want := "POST /api/v4/projects/group%2Fsub%2Fdemo/merge_requests"
	if !contains(reqs, want) {
		t.Errorf("GitLab 桩请求 = %v，want 含 %s", reqs, want)
	}
	if len(gh.params) != 0 || vc.ghCalls.Load() != 0 {
		t.Errorf("GitHub 收到 %d 次 CreatePR、GitHub() 被调 %d 次，want 0", len(gh.params), vc.ghCalls.Load())
	}
	if len(vc.targets) == 0 || vc.targets[0] != (vcs.Target{Provider: vcs.ProviderGitLab, BaseURL: "https://gitlab.example.com"}) {
		t.Errorf("VCS 收到的 target = %v", vc.targets)
	}
	final, _ := m.Get(context.Background(), taskID)
	if final.State != task.StatePROpen || final.PRNumber == nil || *final.PRNumber != 12 ||
		final.PRURL == nil || !strings.Contains(*final.PRURL, "gitlab.example.com") {
		t.Errorf("终态 = %s pr_number=%v pr_url=%v，want pr_open / 12 / GitLab 主机", final.State, final.PRNumber, final.PRURL)
	}
}

// TestPipelineRejectsCredentialInstanceMismatch：凭据实例与仓库不一致 → 在入口阶段的
// 第一次合法转移之后、任何网络操作之前落 failed（vcs_credential），只走 state.go 已有的边。
func TestPipelineRejectsCredentialInstanceMismatch(t *testing.T) {
	mismatch := vcs.InstanceMismatch(vcs.ProviderGitLab, "https://a.example", "https://b.example")
	cases := []struct {
		name    string
		entry   EntryStage
		prepare bool // 先造现场（失败后回到 queued）
		commit  bool
		plan    func() *RetryPlan
		want    []task.State
	}{
		{"EntryTriage 新任务", EntryTriage, false, false, func() *RetryPlan { return nil },
			[]task.State{task.StateTriaging, task.StateFailed}},
		{"EntryImplement 续跑（Rebuild + review）", EntryImplement, true, true, func() *RetryPlan {
			return &RetryPlan{Entry: EntryImplement, ResumeSession: true, Review: true, Rebuild: true}
		}, []task.State{task.StateImplementing, task.StateFailed}},
		{"EntryCommit", EntryCommit, true, false, func() *RetryPlan { return &RetryPlan{Entry: EntryCommit} },
			[]task.State{task.StateImplementing, task.StateFailed}},
		{"EntryVerify", EntryVerify, true, true, func() *RetryPlan { return &RetryPlan{Entry: EntryVerify} },
			[]task.State{task.StateVerifying, task.StateFailed}},
		{"EntryPush", EntryPush, true, true, func() *RetryPlan { return &RetryPlan{Entry: EntryPush} },
			[]task.State{task.StateVerifying, task.StateFailed}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, m, taskID, _, src := pipelineFixture(t)
			repo := gitlabRepoConfig("https://a.example")
			api := newGitLabAPIStub(t)
			gh := &fakeGitHub{}
			ag := &fakeAgent{}
			p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, gh, ag, &fakeNotifier{})
			vc := &vcsClients{fakeClients: p.Clients.(*fakeClients), err: mismatch}
			p.Clients = vc
			if tc.prepare {
				prepareFailedTask(t, p, m, taskID, src, repo, StagePush, tc.commit)
			}
			before := len(retryEvents(t, m, taskID))
			rec := newGitRecorder(t)

			err := p.Execute(context.Background(), ExecuteParams{
				TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test", Retry: tc.plan(),
			})
			// 与其它 p.fail 路径一致：失败已在 pipeline 内落库，Execute 返回包裹原因的错误。
			if !errors.Is(err, vcs.ErrInstanceMismatch) {
				t.Fatalf("Execute 返回 %v，want 包裹 vcs.ErrInstanceMismatch 的失败", err)
			}
			final, _ := m.Get(context.Background(), taskID)
			if final.State != task.StateFailed || final.FailureStage == nil || *final.FailureStage != "vcs_credential" {
				t.Fatalf("终态 = %s stage=%v，want failed / vcs_credential", final.State, final.FailureStage)
			}
			if r := deref(final.FailureReason); !strings.Contains(r, "https://a.example") || !strings.Contains(r, "https://b.example") {
				t.Errorf("failure_reason = %q，want 同时含两个实例地址", r)
			}
			events := retryEvents(t, m, taskID)[before:]
			var got []task.State
			for _, e := range events {
				got = append(got, e.ToState)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("转移序列 = %v，want %v", got, tc.want)
			}
			if last := events[len(events)-1]; last.Actor != "system" {
				t.Errorf("failed 那次的 actor = %s，want system", last.Actor)
			}
			if events[0].FromState == nil || *events[0].FromState != task.StateQueued {
				t.Errorf("第一次转移起点 = %v，want queued", events[0].FromState)
			}
			for _, e := range events {
				if _, ok := e.Payload[payloadReviewKeys]; ok {
					t.Errorf("review 意见不应被标记为已处理：%v", e.Payload)
				}
			}
			if n := len(api.requests()); n != 0 || len(gh.params) != 0 {
				t.Errorf("平台桩收到 %d 次请求、GitHub %d 次，want 0", n, len(gh.params))
			}
			if len(ag.calls) != 0 {
				t.Errorf("假 agent 被调用 %d 次，want 0", len(ag.calls))
			}
			for _, c := range rec.calls(t) {
				for _, bad := range []string{"fetch", "clone", "push", "worktree add"} {
					if strings.HasPrefix(c.args, bad) {
						t.Errorf("凭据被拒后不应有网络/重挂 git 调用：%s", c.args)
					}
				}
			}
		})
	}
}

// TestPipelineGitHubCredentialErrorStaysQueued：GitHub 的存量行为不变。
func TestPipelineGitHubCredentialErrorStaysQueued(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)
	p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, &fakeGitHub{}, &fakeAgent{}, &fakeNotifier{})
	p.Clients = &ghErrClients{fakeClients: p.Clients.(*fakeClients)}
	err := p.Execute(context.Background(), ExecuteParams{TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777"})
	if err == nil || !strings.HasPrefix(err.Error(), "获取 GitHub 客户端失败（请在设置里配置并验证凭据）") {
		t.Fatalf("err = %v，want 「获取 GitHub 客户端失败（请在设置里配置并验证凭据）…」", err)
	}
	if tk, _ := m.Get(context.Background(), taskID); tk.State != task.StateQueued {
		t.Errorf("state = %s，want queued", tk.State)
	}
}

type ghErrClients struct{ *fakeClients }

func (c *ghErrClients) GitHub(context.Context) (GitHubAPI, error) {
	return nil, errors.New("creds: 凭据未配置（github）")
}

// TestPipelineVCSTransientErrorStaysQueued：非哨兵错误（会自愈）与 GitHub 同口径留 queued。
func TestPipelineVCSTransientErrorStaysQueued(t *testing.T) {
	_, m, taskID, _, src := pipelineFixture(t)
	p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, &fakeGitHub{}, &fakeAgent{}, &fakeNotifier{})
	p.Clients = &vcsClients{fakeClients: p.Clients.(*fakeClients), err: errors.New("db down")}
	err := p.Execute(context.Background(), ExecuteParams{TaskID: taskID, Repo: gitlabRepoConfig("https://a.example"), CloneURL: src, IssueRef: "uuid-777"})
	if err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("err = %v，want 透出 db down", err)
	}
	if tk, _ := m.Get(context.Background(), taskID); tk.State != task.StateQueued {
		t.Errorf("state = %s，want queued", tk.State)
	}
}

// ---------------------------------------------------------------- AC-12a

type syncMemory struct {
	mu sync.Mutex
	*fakeMemory
}

func (s *syncMemory) LatestFailureDigest(ctx context.Context, lk store.FailureLookup, ex int64) (*store.FailureDigest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeMemory.LatestFailureDigest(ctx, lk, ex)
}

func (s *syncMemory) InsertFailureDigest(ctx context.Context, d store.FailureDigest, lk store.FailureLookup) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fakeMemory.InsertFailureDigest(ctx, d, lk)
}

type perUserFactory struct{ byUser map[int64]Clients }

func (f perUserFactory) ForUser(_ context.Context, id int64) (Clients, error) {
	c, ok := f.byUser[id]
	if !ok {
		return nil, fmt.Errorf("无此用户 %d", id)
	}
	return c, nil
}

// TestFailureExcerptRedactsPerTaskVCSToken：运行期解出的 vcs token 只进本任务的
// 脱敏表；两个用户的任务并发跑，验证输出回显各自 token，落库摘要都不含明文，
// 且共享的 Pipeline.KnownSecrets 不被追加。
func TestFailureExcerptRedactsPerTaskVCSToken(t *testing.T) {
	ctx := context.Background()
	tokA, tokB := randToken(t, "glpatA"), randToken(t, "glpatB")
	stub := newGitHTTPStub(t, "", false, tokA, tokB)

	type job struct {
		m      *task.Machine
		taskID int64
		userID int64
		repo   RepoConfig
		tok    string
	}
	mkJob := func(path, tok string) job {
		_, m, taskID, _, _ := pipelineFixture(t)
		tk, _ := m.Get(ctx, taskID)
		bare := stub.bare(path)
		gitOut(t, "", "init", "--quiet", "--bare", bare)
		gosrc := goSourceRepo(t)
		gitOut(t, gosrc, "push", "--quiet", bare, "main", "dev")
		return job{m: m, taskID: taskID, userID: tk.UserID, repo: authRepo(stub, path), tok: tok}
	}
	jobs := []job{mkJob("g/a", tokA), mkJob("g/b", tokB)}

	wm := newManager(t)
	mem := &syncMemory{fakeMemory: newFakeMemory()}
	known := []string{"registered-secret-xyz"}
	p := &Pipeline{
		Tasks: jobs[0].m, Worktrees: wm, Verifier: NewVerifier(3*time.Minute, ""), Agent: &fakeAgent{},
		Notifier: &fakeNotifier{}, PermissionMode: "acceptEdits", Memory: mem, KnownSecrets: known,
	}
	byUser := map[int64]Clients{}
	for _, j := range jobs {
		byUser[j.userID] = &vcsClients{fakeClients: &fakeClients{lin: &fakeLinear{issue: demoIssue()}, gh: &fakeGitHub{}},
			client: &fakeGitHub{}, auth: vcs.NewGitAuth("oauth2", j.tok)}
	}
	p.ClientFactory = perUserFactory{byUser: byUser}

	// 造现场：带鉴权建 worktree，提交一个编译失败且错误信息回显 token 的 .go 文件。
	for _, j := range jobs {
		wt, err := wm.Create(ctx, CreateParams{Repo: j.repo, CloneURL: j.repo.CloneURL(), Kind: KindFix,
			IssueKey: "CR-777", Title: "t", TaskID: j.taskID, Auth: vcs.NewGitAuth("oauth2", j.tok)})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		writeFile(t, filepath.Join(wt.Path, "leak.go"), fmt.Sprintf("package main\n\nvar _ int = %q\n", j.tok))
		if err := wm.Commit(ctx, wt, "leak"); err != nil {
			t.Fatal(err)
		}
		sess, kind, tier, reason := "s", "fix", "light", "x"
		if _, err := j.m.Transition(ctx, j.taskID, task.StateImplementing, "test", &task.TransitionOpts{
			AgentSessionID: &sess, WorktreePath: &wt.Path, BranchName: &wt.Branch, TaskKind: &kind, VerifyTier: &tier}); err != nil {
			t.Fatal(err)
		}
		if _, err := j.m.Transition(ctx, j.taskID, task.StateFailed, "test", &task.TransitionOpts{FailureReason: &reason, FailureStage: strPtr(string(StageVerifyGate))}); err != nil {
			t.Fatal(err)
		}
		if _, err := j.m.Transition(ctx, j.taskID, task.StateQueued, "test", nil); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			if err := p.Execute(ctx, ExecuteParams{TaskID: j.taskID, Repo: j.repo, CloneURL: j.repo.CloneURL(),
				IssueRef: "uuid-777", Retry: &RetryPlan{Entry: EntryVerify}}); err == nil {
				t.Errorf("任务 #%d 的验证应失败", j.taskID)
			}
		}(j)
	}
	wg.Wait()

	if len(p.KnownSecrets) != 1 || len(known) != 1 {
		t.Errorf("Pipeline.KnownSecrets 被追加了：%v", p.KnownSecrets)
	}
	mem.mu.Lock()
	defer mem.mu.Unlock()
	if len(mem.written) != 2 {
		t.Fatalf("应写入 2 条失败记忆，得到 %d", len(mem.written))
	}
	for _, d := range mem.written {
		for _, tok := range []string{tokA, tokB} {
			if strings.Contains(d.Excerpt, tok) || strings.Contains(d.Reason, tok) {
				t.Errorf("任务 #%d 的失败记忆含 token 明文：%s", d.TaskID, d.Excerpt)
			}
		}
		if !strings.Contains(d.Excerpt, "[REDACTED") {
			t.Errorf("任务 #%d 的摘要应含 [REDACTED 占位（证明验证输出确实回显过 token）：%s", d.TaskID, d.Excerpt)
		}
	}
}
