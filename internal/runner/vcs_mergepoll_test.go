package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/github"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
	"github.com/zichuanwangcloud-gif/lathe/internal/vcs"
	"github.com/zichuanwangcloud-gif/lathe/internal/vcs/gitlab"
)

// mrAPIStub 是 GitLab MR API 的假服务器：按 iid 返回预设状态，并支持开 MR。
type mrAPIStub struct {
	srv  *httptest.Server
	mu   sync.Mutex
	mrs  map[int]map[string]any
	reqs []string
}

func newMRAPIStub(t *testing.T) *mrAPIStub {
	s := &mrAPIStub{mrs: map[int]map[string]any{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.reqs = append(s.reqs, r.Method+" "+r.URL.EscapedPath())
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.EscapedPath()
		switch {
		case r.Method == "GET" && strings.HasSuffix(p, "/merge_requests"):
			_, _ = w.Write([]byte("[]"))
			return
		case r.Method == "POST" && strings.HasSuffix(p, "/merge_requests"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"iid": 99, "web_url": "https://gitlab.example.com/x/-/merge_requests/99"})
			return
		}
		i := strings.LastIndex(p, "/merge_requests/")
		if r.Method == "GET" && i >= 0 {
			iid, _ := strconv.Atoi(p[i+len("/merge_requests/"):])
			s.mu.Lock()
			mr, ok := s.mrs[iid]
			s.mu.Unlock()
			if ok {
				_ = json.NewEncoder(w).Encode(mr)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *mrAPIStub) set(iid int, state, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mrs[iid] = map[string]any{"iid": iid, "state": state, "target_branch": "dev", "sha": sha}
}

func (s *mrAPIStub) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reqs...)
}

func (s *mrAPIStub) client(t *testing.T) *gitlab.Client {
	c, err := gitlab.New(s.srv.URL, "glpat-api")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// ---------------------------------------------------------------- AC-20

func TestMergePollUsesRepoPlatform(t *testing.T) {
	_, m, userID, repoID, repo := mergepollFixture(t)
	repo.VCSProvider, repo.VCSBaseURL = vcs.ProviderGitLab, "https://gitlab.example.com"
	ctx := context.Background()
	tk, err := m.Create(ctx, task.CreateParams{UserID: userID, RepoID: repoID, ExternalKey: "MPV-1", ExternalID: "uuid-mpv-1"})
	if err != nil {
		t.Fatal(err)
	}
	toPROpenNoWorktree(t, m, tk.ID)
	if err := m.SetPRNumber(ctx, tk.ID, 42); err != nil {
		t.Fatal(err)
	}
	api := newMRAPIStub(t)
	api.set(42, "merged", "abc")
	gh := &fakeGitHub{prInfo: &github.PRInfo{Number: 42, State: "open"}}
	vc := &vcsClients{fakeClients: &fakeClients{lin: newRebaseFollowupLinear(), gh: gh}, client: api.client(t)}
	mp := &MergePoller{Tasks: m, ClientFactory: perUserFactory{byUser: map[int64]Clients{userID: vc}}, RepoLookup: repoLookupFixed(repo), Notifier: &fakeNotifier{}}

	if err := mp.pollTask(ctx, mustGet(t, m, tk.ID)); err != nil {
		t.Fatalf("pollTask: %v", err)
	}
	if got := mustGet(t, m, tk.ID).State; got != task.StateMerged {
		t.Errorf("state = %s，want merged", got)
	}
	want := fmt.Sprintf("GET /api/v4/projects/%s/merge_requests/42", strings.ReplaceAll(repo.ProviderRepo, "/", "%2F"))
	if !contains(api.requests(), want) {
		t.Errorf("GitLab 桩请求 = %v，want 含 %s", api.requests(), want)
	}
	if vc.ghCalls.Load() != 0 {
		t.Errorf("GitLab 仓库不应调 Clients.GitHub，得到 %d 次", vc.ghCalls.Load())
	}
}

func mustGet(t *testing.T, m *task.Machine, id int64) *task.Task {
	t.Helper()
	tk, err := m.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func TestMergePollGitLabClosedUnmerged(t *testing.T) {
	_, m, userID, repoID, repo := mergepollFixture(t)
	repo.VCSProvider, repo.VCSBaseURL = vcs.ProviderGitLab, "https://gitlab.example.com"
	ctx := context.Background()
	t1, _ := m.Create(ctx, task.CreateParams{UserID: userID, RepoID: repoID, ExternalKey: "MPC-1", ExternalID: "uuid-mpc-1"})
	t2, err := m.Create(ctx, task.CreateParams{UserID: userID, RepoID: repoID, ExternalKey: "MPC-2", ExternalID: "uuid-mpc-2", DependsOn: &t1.ID})
	if err != nil {
		t.Fatal(err)
	}
	toPROpenNoWorktree(t, m, t1.ID)
	_ = m.SetPRNumber(ctx, t1.ID, 43)
	api := newMRAPIStub(t)
	api.set(43, "closed", "abc")
	lin := newRebaseFollowupLinear()
	lin.add(rebaseFollowupIssue("uuid-mpc-2", "MPC-2"))
	vc := &vcsClients{fakeClients: &fakeClients{lin: lin, gh: &fakeGitHub{}}, client: api.client(t)}
	mp := &MergePoller{Tasks: m, ClientFactory: perUserFactory{byUser: map[int64]Clients{userID: vc}}, RepoLookup: repoLookupFixed(repo), Notifier: &fakeNotifier{}}
	if err := mp.pollTask(ctx, mustGet(t, m, t1.ID)); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, m, t1.ID).State; got != task.StateCancelled {
		t.Errorf("前驱 state = %s，want cancelled", got)
	}
	if got := mustGet(t, m, t2.ID).State; got != task.StateBlockedDep {
		t.Errorf("后继 state = %s，want blocked_dep（F2.3-AC2）", got)
	}
}

func TestForcePushDetectedOnGitLab(t *testing.T) {
	_, m, userID, repoID, repo := mergepollFixture(t)
	repo.VCSProvider, repo.VCSBaseURL = vcs.ProviderGitLab, "https://gitlab.example.com"
	ctx := context.Background()
	tk, _ := m.Create(ctx, task.CreateParams{UserID: userID, RepoID: repoID, ExternalKey: "MPF-1", ExternalID: "uuid-mpf-1"})
	branch := "fix/mpf-1-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := m.Transition(ctx, tk.ID, task.StateImplementing, "test", &task.TransitionOpts{AgentSessionID: strPtr("s"), BranchName: &branch}); err != nil {
		t.Fatal(err)
	}
	toPROpenFromImplementing(t, m, tk.ID)
	_ = m.SetPRNumber(ctx, tk.ID, 44)

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	api := newMRAPIStub(t)
	vc := &vcsClients{fakeClients: &fakeClients{lin: newRebaseFollowupLinear(), gh: &fakeGitHub{}}, client: api.client(t)}
	mp := &MergePoller{Tasks: m, ClientFactory: perUserFactory{byUser: map[int64]Clients{userID: vc}}, RepoLookup: repoLookupFixed(repo), Notifier: &fakeNotifier{}}
	api.set(44, "opened", "sha-1")
	if err := mp.pollTask(ctx, mustGet(t, m, tk.ID)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "force-push") {
		t.Fatal("第一次观察只记基线，不应判定 force-push")
	}
	api.set(44, "opened", "sha-2")
	if err := mp.pollTask(ctx, mustGet(t, m, tk.ID)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "检测到前驱 PR 被 force-push") {
		t.Errorf("GitLab MR 的 HeadSHA 变化应触发 F4.4 rebase 跟进：%s", logs.String())
	}
}

func toPROpenFromImplementing(t *testing.T, m *task.Machine, id int64) {
	t.Helper()
	for _, s := range []task.State{task.StateVerifying, task.StatePROpen} {
		if _, err := m.Transition(context.Background(), id, s, "test", nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMergePollSkipsOnInstanceMismatch(t *testing.T) {
	_, m, userID, repoID, repo := mergepollFixture(t)
	repo.VCSProvider, repo.VCSBaseURL = vcs.ProviderGitLab, "https://a.example"
	ctx := context.Background()
	tk, _ := m.Create(ctx, task.CreateParams{UserID: userID, RepoID: repoID, ExternalKey: "MPM-1", ExternalID: "uuid-mpm-1"})
	toPROpenNoWorktree(t, m, tk.ID)
	_ = m.SetPRNumber(ctx, tk.ID, 45)
	api := newMRAPIStub(t)
	api.set(45, "merged", "x")
	vc := &vcsClients{fakeClients: &fakeClients{lin: newRebaseFollowupLinear(), gh: &fakeGitHub{}},
		err: vcs.InstanceMismatch(vcs.ProviderGitLab, "https://a.example", "https://b.example")}
	mp := &MergePoller{Tasks: m, ClientFactory: perUserFactory{byUser: map[int64]Clients{userID: vc}}, RepoLookup: repoLookupFixed(repo), Notifier: &fakeNotifier{}}
	if err := mp.pollTask(ctx, mustGet(t, m, tk.ID)); err == nil {
		t.Fatal("实例不一致应报错跳过本轮")
	}
	if got := mustGet(t, m, tk.ID).State; got != task.StatePROpen {
		t.Errorf("state = %s，want pr_open 不变", got)
	}
	if n := len(api.requests()); n != 0 {
		t.Errorf("GitLab 桩收到 %d 次请求，want 0", n)
	}
}

// ---------------------------------------------------------------- 跨属主 rebase 跟进

type followupEnv struct {
	m          *task.Machine
	wm         *WorktreeManager
	stub       *gitHTTPStub
	api        *mrAPIStub
	repo       RepoConfig
	src        string
	p, s, next *task.Task
	pWT, sWT   *Worktree
	tokA, tokB string
	vcA, vcB   *vcsClients
	mem        *syncMemory
	mp         *MergePoller
}

// newFollowupEnv：前驱 P（属主 A）与后继 S（属主 B，同一 GitLab 仓库、base_ref=P 分支，
// pr_open）各自已带鉴权推到 git http-backend 远端；S 另有一个排队中的后继 next。
// sFile/sContent 是 S 的改动；upstreamConflict 非空时 dev 上同一文件写入不同内容（制造冲突）。
func newFollowupEnv(t *testing.T, tokB, sFile, sContent, upstreamConflict string) *followupEnv {
	t.Helper()
	ctx := context.Background()
	if tokB == "" {
		tokB = randToken(t, "tokB")
	}
	e := &followupEnv{tokA: randToken(t, "tokA"), tokB: tokB}
	e.stub = newGitHTTPStub(t, "", false, e.tokA, e.tokB)
	const path = "group/sub/flw"
	bare := e.stub.bare(path)
	gitOut(t, "", "init", "--quiet", "--bare", bare)
	e.src = goSourceRepo(t)
	gitOut(t, e.src, "push", "--quiet", bare, "main", "dev")
	e.repo = authRepo(e.stub, path)

	pool := testPoolForPipeline(t)
	e.m = task.NewMachine(pool)
	mkUser := func(tag string) (int64, int64) {
		var uid, rid int64
		email := tag + "-" + t.Name() + "-" + strconv.FormatInt(time.Now().UnixNano(), 10) + "@example.com"
		if err := pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`, email).Scan(&uid); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, uid) })
		if err := pool.QueryRow(ctx, `INSERT INTO repos (user_id, provider_repo, vcs_provider, vcs_base_url) VALUES ($1,$2,'gitlab',$3) RETURNING id`,
			uid, path, e.stub.base).Scan(&rid); err != nil {
			t.Fatal(err)
		}
		return uid, rid
	}
	userA, repoA := mkUser("flw-a")
	userB, repoB := mkUser("flw-b")
	var err error
	e.wm, err = NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	authA, authB := vcs.NewGitAuth("oauth2", e.tokA), vcs.NewGitAuth("oauth2", e.tokB)
	e.p, _ = e.m.Create(ctx, task.CreateParams{UserID: userA, RepoID: repoA, ExternalKey: "FLW-P", ExternalID: "uuid-flw-p"})

	// P：建现场、提交、带 A 的 token 推送、摆到 pr_open
	e.pWT, err = e.wm.Create(ctx, CreateParams{Repo: e.repo, CloneURL: e.repo.CloneURL(), Kind: KindFix, IssueKey: "FLW-P", Title: "p", TaskID: e.p.ID, Auth: authA})
	if err != nil {
		t.Fatalf("P Create: %v", err)
	}
	commitIn(t, e.pWT.Path, "p_notes.txt", "p 的产出\n")
	if err := e.wm.Push(ctx, e.pWT, e.repo, authA, nil); err != nil {
		t.Fatalf("P Push: %v", err)
	}
	placePROpen(t, e.m, e.p.ID, e.pWT, 21)

	// S：栈在 P 上，属主 B
	pBranch := e.pWT.Branch
	e.s, _ = e.m.Create(ctx, task.CreateParams{UserID: userB, RepoID: repoB, ExternalKey: "FLW-S", ExternalID: "uuid-flw-s"})
	if err := e.m.SetBaseRef(ctx, e.s.ID, &pBranch); err != nil {
		t.Fatal(err)
	}
	sRepo := e.repo
	sRepo.BaseRefOverride = pBranch
	e.sWT, err = e.wm.Create(ctx, CreateParams{Repo: sRepo, CloneURL: e.repo.CloneURL(), Kind: KindFix, IssueKey: "FLW-S", Title: "s", TaskID: e.s.ID, Auth: authB})
	if err != nil {
		t.Fatalf("S Create: %v", err)
	}
	commitIn(t, e.sWT.Path, sFile, sContent)
	if err := e.wm.Push(ctx, e.sWT, e.repo, authB, nil); err != nil {
		t.Fatalf("S Push: %v", err)
	}
	placePROpen(t, e.m, e.s.ID, e.sWT, 22)
	e.next, _ = e.m.Create(ctx, task.CreateParams{UserID: userB, RepoID: repoB, ExternalKey: "FLW-N", ExternalID: "uuid-flw-n", DependsOn: &e.s.ID})

	// 「P 合并」：dev 经 cherry-pick 拿到 P 的内容（squash 的真实效果）
	pTip := gitOut(t, e.pWT.Path, "rev-parse", "HEAD")
	gitOut(t, e.src, "fetch", "--quiet", bare, pBranch)
	gitOut(t, e.src, "checkout", "-q", "dev")
	gitOut(t, e.src, "-c", "user.email=t@e.st", "-c", "user.name=t", "cherry-pick", "-x", pTip)
	if upstreamConflict != "" {
		writeFile(t, filepath.Join(e.src, sFile), upstreamConflict)
		gitOut(t, e.src, "add", ".")
		gitOut(t, e.src, "-c", "user.email=t@e.st", "-c", "user.name=t", "commit", "-qm", "dev 上的冲突改动")
	}
	gitOut(t, e.src, "push", "--quiet", bare, "dev")

	e.api = newMRAPIStub(t)
	e.api.set(21, "merged", pTip)
	e.api.set(22, "opened", gitOut(t, e.sWT.Path, "rev-parse", "HEAD"))
	lin := newRebaseFollowupLinear()
	for _, k := range []string{"FLW-P", "FLW-S", "FLW-N"} {
		lin.add(rebaseFollowupIssue("uuid-"+strings.ToLower(k), k))
	}
	e.vcA = &vcsClients{fakeClients: &fakeClients{lin: lin, gh: &fakeGitHub{}}, client: e.api.client(t), auth: authA}
	e.vcB = &vcsClients{fakeClients: &fakeClients{lin: lin, gh: &fakeGitHub{}}, client: e.api.client(t), auth: authB}
	factory := perUserFactory{byUser: map[int64]Clients{userA: e.vcA, userB: e.vcB}}
	e.mem = &syncMemory{fakeMemory: newFakeMemory()}
	pipe := &Pipeline{Tasks: e.m, Worktrees: e.wm, Verifier: NewVerifier(3*time.Minute, ""), Agent: &fakeAgent{},
		ClientFactory: factory, Notifier: &fakeNotifier{}, PermissionMode: "acceptEdits", Memory: e.mem}
	e.mp = &MergePoller{Tasks: e.m, Worktrees: e.wm, ClientFactory: factory, Pipeline: pipe, Notifier: &fakeNotifier{},
		RepoLookup: repoLookupFixed(e.repo)}
	e.stub.mu.Lock()
	e.stub.authSeen = nil
	e.stub.mu.Unlock()
	return e
}

func placePROpen(t *testing.T, m *task.Machine, id int64, wt *Worktree, pr int) {
	t.Helper()
	ctx := context.Background()
	sess, kind, tier := "sess", "fix", "light"
	if _, err := m.Transition(ctx, id, task.StateImplementing, "test", &task.TransitionOpts{
		AgentSessionID: &sess, WorktreePath: &wt.Path, BranchName: &wt.Branch, TaskKind: &kind, VerifyTier: &tier}); err != nil {
		t.Fatal(err)
	}
	toPROpenFromImplementing(t, m, id)
	if err := m.SetPRNumber(ctx, id, pr); err != nil {
		t.Fatal(err)
	}
}

func TestGitLabMergedFollowupRebasesSuccessorWithItsOwnersAuth(t *testing.T) {
	e := newFollowupEnv(t, "", "s_notes.txt", "s 的产出\n", "")
	ctx := context.Background()
	if err := e.mp.pollTask(ctx, mustGet(t, e.m, e.p.ID)); err != nil {
		t.Fatalf("pollTask(P): %v", err)
	}
	if got := mustGet(t, e.m, e.p.ID).State; got != task.StateMerged {
		t.Fatalf("P state = %s，want merged", got)
	}
	sAfter := mustGet(t, e.m, e.s.ID)
	if sAfter.State != task.StatePROpen {
		t.Fatalf("S 重验后 state = %s（%v），want pr_open", sAfter.State, deref(sAfter.FailureReason))
	}
	head := gitOut(t, e.sWT.Path, "rev-parse", "HEAD")
	devTip := remoteHeads(t, e.stub.bare("group/sub/flw"))["dev"]
	if !isAncestor(t, e.sWT.Path, devTip, "HEAD") {
		t.Error("S 应已 rebase 到 dev 新 tip 之上")
	}
	if got := remoteHeads(t, e.stub.bare("group/sub/flw"))[e.sWT.Branch]; got != head {
		t.Errorf("远端 S 分支 = %s，want rebase 后的 HEAD %s", got, head)
	}
	// 鉴权的属主顺序：先 A（resolveMirrorTip 刷新已合并任务的仓库），之后全是 B。
	e.stub.mu.Lock()
	seen := append([]string(nil), e.stub.authSeen...)
	e.stub.mu.Unlock()
	firstB, sawA := -1, false
	for i, a := range seen {
		switch {
		case strings.HasSuffix(a, ":"+e.tokA):
			sawA = true
			if firstB >= 0 {
				t.Errorf("第 %d 个请求在 B 的跟进开始后用了 A 的 token", i)
			}
		case strings.HasSuffix(a, ":"+e.tokB):
			if firstB < 0 {
				firstB = i
			}
		}
	}
	if !sawA || firstB < 0 {
		t.Errorf("应先后看到 A 与 B 的 token，seen=%d 条（A=%v B=%v）", len(seen), sawA, firstB >= 0)
	}
	if len(e.vcB.targets) == 0 {
		t.Error("S 的重验 setup 应以 B 的身份调到 VCS")
	}
}

func TestGitLabRebaseFollowupInstanceMismatchFailsSuccessor(t *testing.T) {
	e := newFollowupEnv(t, "", "s_notes.txt", "s 的产出\n", "")
	e.vcB.err = vcs.InstanceMismatch(vcs.ProviderGitLab, e.repo.VCSBaseURL, "https://elsewhere.example")
	ctx := context.Background()
	headBefore := gitOut(t, e.sWT.Path, "rev-parse", "HEAD")
	remoteBefore := remoteHeads(t, e.stub.bare("group/sub/flw"))[e.sWT.Branch]
	if err := e.mp.pollTask(ctx, mustGet(t, e.m, e.p.ID)); err != nil {
		t.Fatalf("pollTask(P): %v", err)
	}
	s := mustGet(t, e.m, e.s.ID)
	if s.State != task.StateFailed || deref(s.FailureStage) != "vcs_credential" {
		t.Fatalf("S = %s / %s，want failed / vcs_credential", s.State, deref(s.FailureStage))
	}
	if got := gitOut(t, e.sWT.Path, "rev-parse", "HEAD"); got != headBefore {
		t.Error("凭据被拒时 S 的工作区不应被改写")
	}
	if got := remoteHeads(t, e.stub.bare("group/sub/flw"))[e.sWT.Branch]; got != remoteBefore {
		t.Error("凭据被拒时 S 的远端分支不应被改动")
	}
	if got := mustGet(t, e.m, e.next.ID).State; got != task.StateBlockedDep {
		t.Errorf("S 的后继 state = %s，want blocked_dep（PropagateBlocked 照常执行）", got)
	}
}

// TestRebaseFailureReasonRedactsVCSToken：rebase 冲突的错误文案回显 token（这里借冲突文件名），
// failRebaseFollowup 用同一个 GitAuth 脱敏，失败原因与失败记忆都不含明文。
func TestRebaseFailureReasonRedactsVCSToken(t *testing.T) {
	tokB := randToken(t, "tokB")
	e := newFollowupEnv(t, tokB, "conflict-"+tokB+".txt", "s 的版本\n", "dev 的版本\n")
	if err := e.mp.pollTask(context.Background(), mustGet(t, e.m, e.p.ID)); err != nil {
		t.Fatal(err)
	}
	s := mustGet(t, e.m, e.s.ID)
	if s.State != task.StateFailed || deref(s.FailureStage) != string(StageRebaseConflict) {
		t.Fatalf("S = %s / %s，want failed / rebase_conflict", s.State, deref(s.FailureStage))
	}
	if strings.Contains(deref(s.FailureReason), e.tokB) {
		t.Errorf("failure_reason 含 token 明文：%s", deref(s.FailureReason))
	}
	e.mem.mu.Lock()
	defer e.mem.mu.Unlock()
	if len(e.mem.written) == 0 {
		t.Fatal("应写入失败记忆")
	}
	for _, d := range e.mem.written {
		if strings.Contains(d.Reason, e.tokB) {
			t.Errorf("failure_digests.reason 含 token 明文：%s", d.Reason)
		}
		if !strings.Contains(d.Reason, "[REDACTED") {
			t.Errorf("原因应含 [REDACTED 占位（证明错误文案确实回显过 token）：%s", d.Reason)
		}
	}
}

// TestGitHubRebaseFollowupUnchanged：GitHub 后继的跟进路径不新增 Clients.GitHub 调用
// （rebaseFollowupOne 不为 GitHub 调 resolveVCS；唯一的一次来自重验 Execute 的 setup）。
func TestGitHubRebaseFollowupUnchanged(t *testing.T) {
	_, m, userID, repoID, repo, src := rebaseFollowupFixture(t)
	ctx := context.Background()
	wm := newManager(t)
	pred, _ := m.Create(ctx, task.CreateParams{UserID: userID, RepoID: repoID, ExternalKey: "GHU-1", ExternalID: "uuid-ghu-1"})
	succ, _ := m.Create(ctx, task.CreateParams{UserID: userID, RepoID: repoID, ExternalKey: "GHU-2", ExternalID: "uuid-ghu-2"})
	pWT, err := wm.Create(ctx, CreateParams{Repo: repo, CloneURL: src, Kind: KindFix, IssueKey: "GHU-1", Title: "p", TaskID: pred.ID})
	if err != nil {
		t.Fatal(err)
	}
	commitIn(t, pWT.Path, "p.txt", "p\n")
	if err := wm.Push(ctx, pWT, repo, vcs.GitAuth{}, nil); err != nil {
		t.Fatal(err)
	}
	pTip := gitOut(t, pWT.Path, "rev-parse", "HEAD")
	b := pWT.Branch
	_ = m.SetBaseRef(ctx, succ.ID, &b)
	sRepo := repo
	sRepo.BaseRefOverride = b
	sWT, err := wm.Create(ctx, CreateParams{Repo: sRepo, CloneURL: src, Kind: KindFix, IssueKey: "GHU-2", Title: "s", TaskID: succ.ID})
	if err != nil {
		t.Fatal(err)
	}
	commitIn(t, sWT.Path, "s.txt", "s\n")
	if err := wm.Push(ctx, sWT, repo, vcs.GitAuth{}, nil); err != nil {
		t.Fatal(err)
	}
	placePROpen(t, m, succ.ID, sWT, 31)
	gitOut(t, src, "checkout", "-q", "dev")
	gitOut(t, src, "-c", "user.email=t@e.st", "-c", "user.name=t", "cherry-pick", "-x", pTip)

	lin := newRebaseFollowupLinear()
	lin.add(rebaseFollowupIssue("uuid-ghu-2", "GHU-2"))
	vc := &vcsClients{fakeClients: &fakeClients{lin: lin, gh: &fakeGitHub{pr: &github.PullRequest{Number: 31, URL: "u"}}}}
	pipe := &Pipeline{Tasks: m, Worktrees: wm, Verifier: NewVerifier(3*time.Minute, ""), Agent: &fakeAgent{},
		Clients: vc, Notifier: &fakeNotifier{}, PermissionMode: "acceptEdits"}
	mp := &MergePoller{Tasks: m, Worktrees: wm, Pipeline: pipe, Notifier: &fakeNotifier{}, RepoLookup: repoLookupFixed(repo),
		ClientFactory: fixedClientFactory{clients: vc}}
	if err := mp.rebaseFollowup(ctx, b, pTip, repo.DefaultBranch); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, m, succ.ID).State; got != task.StatePROpen {
		t.Fatalf("S state = %s，want pr_open", got)
	}
	if n := vc.ghCalls.Load(); n != 1 {
		t.Errorf("GitHub 跟进路径上 Clients.GitHub 调用 %d 次，want 1（只有重验 setup 那一次，与 cff5332 相同）", n)
	}
	if len(vc.targets) != 0 {
		t.Errorf("GitHub 仓库不应调 VCS，得到 %v", vc.targets)
	}
}

// ---------------------------------------------------------------- AC-20b

// TestRepoKeyStableAfterCredentialBaseURLChange：镜像键只从 repos 行来。凭据地址事后
// 被改，仓库配置加载器给出的键不变，在途现场的 Inspect/Discard 仍落在原 mirror，
// 不会新建第二个 mirror 目录。
func TestRepoKeyStableAfterCredentialBaseURLChange(t *testing.T) {
	pool := testPoolForPipeline(t)
	ctx := context.Background()
	var userID, repoID int64
	email := "keystable-" + strconv.FormatInt(time.Now().UnixNano(), 10) + "@example.com"
	if err := pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`, email).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID) })
	if _, err := pool.Exec(ctx, `INSERT INTO integrations (user_id, kind, secret_enc, scopes, base_url) VALUES ($1,'gitlab','\x00','{}','https://gitlab.example.com')`, userID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO repos (user_id, provider_repo, vcs_provider, vcs_base_url)
		SELECT $1, 'group/sub/demo', 'gitlab', base_url FROM integrations WHERE user_id=$1 AND kind='gitlab' RETURNING id`, userID).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	lookup := NewRepoLookup(pool)
	before, err := lookup(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	m := newManager(t)
	src := sourceRepo(t)
	wt, err := m.Create(ctx, CreateParams{Repo: before, CloneURL: src, Kind: KindFix, IssueKey: "KS-1", Title: "k", TaskID: 1})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `UPDATE integrations SET base_url='https://moved.example.com' WHERE user_id=$1 AND kind='gitlab'`, userID); err != nil {
		t.Fatal(err)
	}
	after, err := lookup(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if after.RepoKey() != "gitlab.example.com/group/sub/demo" || after.RepoKey() != before.RepoKey() {
		t.Fatalf("凭据改地址后键 = %q（之前 %q），want gitlab.example.com/group/sub/demo", after.RepoKey(), before.RepoKey())
	}
	if m.MirrorPath(after.RepoKey()) != wt.Mirror {
		t.Errorf("mirror 路径变了：%s vs %s", m.MirrorPath(after.RepoKey()), wt.Mirror)
	}
	if st := m.Inspect(ctx, after.RepoKey(), wt.Path, wt.Branch, "dev"); !st.Usable() {
		t.Errorf("Inspect 应仍认得原现场：%+v", st)
	}
	if res := m.Discard(ctx, after.RepoKey(), wt.Path, wt.Branch); res.MirrorMissing || !res.Removed() {
		t.Errorf("Discard 应落在原 mirror 上：%+v", res)
	}
	entries, _ := os.ReadDir(filepath.Join(m.Root(), ".mirrors"))
	if len(entries) != 1 {
		t.Errorf(".mirrors/ 下有 %d 个目录，want 1（不应新建第二个 mirror）", len(entries))
	}
}
