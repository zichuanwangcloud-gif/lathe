package prd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/vcs"
)

// recWorktrees 记录 EnsureMirror/CreateDetached 收到的键、地址与鉴权。
type recWorktrees struct {
	fakeWorktrees
	mu          sync.Mutex
	ensureKeys  []string
	ensureURLs  []string
	ensureAuths []vcs.GitAuth
	detachKeys  []string
}

func (r *recWorktrees) EnsureMirror(_ context.Context, key, cloneURL string, auth vcs.GitAuth) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensureKeys, r.ensureURLs, r.ensureAuths = append(r.ensureKeys, key), append(r.ensureURLs, cloneURL), append(r.ensureAuths, auth)
	return "/mirrors/x", nil
}

func (r *recWorktrees) CreateDetached(ctx context.Context, key, base, name string) (*Checkout, error) {
	r.mu.Lock()
	r.detachKeys = append(r.detachKeys, key)
	r.mu.Unlock()
	return r.fakeWorktrees.CreateDetached(ctx, key, base, name)
}

type gitAuthCall struct {
	userID int64
	target vcs.Target
}

type fakeGitAuth struct {
	auth  vcs.GitAuth
	err   error
	calls []gitAuthCall
}

func (f *fakeGitAuth) GitAuth(_ context.Context, userID int64, t vcs.Target) (vcs.GitAuth, error) {
	f.calls = append(f.calls, gitAuthCall{userID, t})
	return f.auth, f.err
}

func checkoutRound(t *testing.T, repo RepoInfo, ga GitAuthSource, cloneURL func(RepoInfo) string) (*recWorktrees, *fakeStore, error) {
	t.Helper()
	st, wt := newFakeStore(), &recWorktrees{}
	st.repo = repo
	st.rounds = []store.PRDRoundRow{{Round: 1, Stage: string(StageUnderstand)}}
	st.row.Round = 1
	st.row.Document = json.RawMessage(`{"type":"feature"}`)
	ag := &fakeAgent{results: []*agent.Result{ok(`{"type":"feature","document":{"problem":{"body":"x"}}}`)}}
	svc := &Service{Store: st, Agent: ag, Worktrees: wt, Channel: "strong", CloneURL: cloneURL}
	if ga != nil {
		svc.GitAuth = ga
	}
	_, err := svc.RunRound(context.Background(), RunRoundParams{PRDID: 1, UserID: 7})
	return wt, st, err
}

// AC-21：PRD 规划路径按仓库平台算镜像键与克隆地址，鉴权来自 Service.GitAuth（按 PRD 属主）。
func TestPRDCloneURLUsesRepoProvider(t *testing.T) {
	gl := RepoInfo{ProviderRepo: "group/sub/demo", DefaultBranch: "main", VCSProvider: vcs.ProviderGitLab, VCSBaseURL: "https://gitlab.example.com"}
	gh := RepoInfo{ProviderRepo: "acme/demo", DefaultBranch: "main"}
	tok := vcs.NewGitAuth("oauth2", "tok-prd")

	t.Run("gitlab 仓库：键/地址按平台，auth 来自假件且按属主取", func(t *testing.T) {
		ga := &fakeGitAuth{auth: tok}
		wt, _, err := checkoutRound(t, gl, ga, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(wt.ensureKeys) != 1 || wt.ensureKeys[0] != "gitlab.example.com/group/sub/demo" ||
			wt.ensureURLs[0] != "https://gitlab.example.com/group/sub/demo.git" || wt.ensureAuths[0] != tok {
			t.Errorf("EnsureMirror 收到 key=%v url=%v auth非零=%v", wt.ensureKeys, wt.ensureURLs, len(wt.ensureAuths) > 0 && !wt.ensureAuths[0].IsZero())
		}
		if len(wt.detachKeys) != 1 || wt.detachKeys[0] != "gitlab.example.com/group/sub/demo" {
			t.Errorf("CreateDetached 键 = %v", wt.detachKeys)
		}
		if len(ga.calls) != 1 || ga.calls[0].userID != 7 ||
			ga.calls[0].target != (vcs.Target{Provider: vcs.ProviderGitLab, BaseURL: "https://gitlab.example.com"}) {
			t.Errorf("GitAuth 调用 = %+v，want 一次 (7, {gitlab, https://gitlab.example.com})", ga.calls)
		}
	})
	t.Run("github 仓库：键与地址不变，auth 零值，假件不被调用", func(t *testing.T) {
		ga := &fakeGitAuth{auth: tok}
		wt, _, err := checkoutRound(t, gh, ga, nil)
		if err != nil {
			t.Fatal(err)
		}
		if wt.ensureKeys[0] != "acme/demo" || wt.ensureURLs[0] != "git@github.com:acme/demo.git" || !wt.ensureAuths[0].IsZero() {
			t.Errorf("github：key=%v url=%v", wt.ensureKeys, wt.ensureURLs)
		}
		if len(ga.calls) != 0 {
			t.Errorf("github 仓库不应调 GitAuth，得到 %d 次", len(ga.calls))
		}
	})
	t.Run("注入的 CloneURL 仍以它为准", func(t *testing.T) {
		wt, _, err := checkoutRound(t, gh, nil, func(r RepoInfo) string { return "git@example.com:" + r.ProviderRepo + ".git" })
		if err != nil {
			t.Fatal(err)
		}
		if wt.ensureURLs[0] != "git@example.com:acme/demo.git" {
			t.Errorf("url = %v", wt.ensureURLs)
		}
	})
	t.Run("GitAuth 未装配 + gitlab → 本轮失败且不 EnsureMirror", func(t *testing.T) {
		wt, _, err := checkoutRound(t, gl, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "未装配代码平台鉴权") {
			t.Fatalf("err = %v，want 含「未装配代码平台鉴权」", err)
		}
		if len(wt.ensureKeys) != 0 {
			t.Errorf("EnsureMirror 被调用 %d 次，want 0", len(wt.ensureKeys))
		}
	})
	t.Run("实例不一致 → 本轮失败", func(t *testing.T) {
		mis := vcs.InstanceMismatch(vcs.ProviderGitLab, "https://gitlab.example.com", "https://other.example")
		wt, _, err := checkoutRound(t, gl, &fakeGitAuth{err: mis}, nil)
		if !errors.Is(err, vcs.ErrInstanceMismatch) {
			t.Fatalf("err = %v，want ErrInstanceMismatch", err)
		}
		if len(wt.ensureKeys) != 0 {
			t.Errorf("EnsureMirror 被调用 %d 次，want 0", len(wt.ensureKeys))
		}
	})
	t.Run("阶段 A 不检出 → 不取凭据", func(t *testing.T) {
		st, wt := newFakeStore(), &recWorktrees{}
		st.repo = gl
		ga := &fakeGitAuth{auth: tok}
		ag := &fakeAgent{results: []*agent.Result{ok(`{"type":"feature","document":{"oneLiner":{"body":"x"}},"questions":["?"]}`)}}
		svc := &Service{Store: st, Agent: ag, Worktrees: wt, GitAuth: ga}
		if _, err := svc.RunRound(context.Background(), RunRoundParams{PRDID: 1, UserID: 7}); err != nil {
			t.Fatal(err)
		}
		if len(ga.calls) != 0 || len(wt.ensureKeys) != 0 {
			t.Errorf("阶段 A：GitAuth 调用 %d、EnsureMirror %d，want 0/0", len(ga.calls), len(wt.ensureKeys))
		}
	})
	t.Run("对抗复核不检出 → 不取凭据", func(t *testing.T) {
		st, wt := newFakeStore(), &recWorktrees{}
		st.repo = gl
		st.row.Document = json.RawMessage(reviewableDoc)
		ga := &fakeGitAuth{auth: tok}
		ag := &fakeAgent{results: []*agent.Result{ok(`{"findings":[],"verdict":"ok"}`)}}
		svc := &Service{Store: st, Agent: ag, Worktrees: wt, GitAuth: ga}
		if _, err := svc.RunReview(context.Background(), 1, 7); err != nil {
			t.Fatal(err)
		}
		if len(ga.calls) != 0 || len(wt.ensureKeys) != 0 {
			t.Errorf("复核：GitAuth 调用 %d、EnsureMirror %d，want 0/0", len(ga.calls), len(wt.ensureKeys))
		}
	})
}
