package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
	"github.com/zichuanwangcloud-gif/lathe/internal/integration/github"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
	"github.com/zichuanwangcloud-gif/lathe/internal/tracker"
)

// syncingTracker 是实现了 tracker.StatusSyncer 的假 tracker（云效形态）。
type syncingTracker struct {
	*fakeLinear
	mu     sync.Mutex
	events []tracker.Lifecycle
	refs   []string
	err    error
}

func (s *syncingTracker) SyncStatus(ctx context.Context, ref string, ev tracker.Lifecycle) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	s.refs = append(s.refs, ref)
	return s.err
}

func (s *syncingTracker) got() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.events))
	for i, e := range s.events {
		out[i] = string(e)
	}
	return strings.Join(out, ",")
}

// happyAgent 与 TestPipelineHappyPath 同一套：分诊通过、实现带复现测试与修复。
func happyAgent() *fakeAgent {
	return &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象和期望行为","question":""}`},
			{Success: true, Text: "补了 greet 函数与复现测试"},
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
}

// runToPROpen 跑一次走到 pr_open 的执行，返回终态与落库的验证行。
func runToPROpen(t *testing.T, tr tracker.Tracker) (*task.Task, []string) {
	t.Helper()
	_, m, taskID, repo, src := pipelineFixture(t)
	gh := &fakeGitHub{pr: &github.PullRequest{Number: 42, URL: "https://github.com/acme/demo/pull/42"}}
	p := newPipeline(t, m, nil, gh, happyAgent(), &fakeNotifier{})
	p.Clients = &fakeClients{lin: tr, gh: gh}
	verifs := &fakeVerifications{}
	p.Verifications = verifs
	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
	}); err != nil {
		t.Fatalf("Execute: got %v want nil", err)
	}
	final, err := m.Get(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	return final, verifs.rows
}

// AC-13
func TestPipelineStatusSync(t *testing.T) {
	var baselineRows []string

	t.Run("order_started_then_pr_open", func(t *testing.T) {
		st := &syncingTracker{fakeLinear: &fakeLinear{issue: demoIssue()}}
		final, rows := runToPROpen(t, st)
		baselineRows = rows
		if final.State != task.StatePROpen {
			t.Fatalf("终态 got %s want pr_open", final.State)
		}
		if got, want := st.got(), "started,pr_open"; got != want {
			t.Errorf("SyncStatus 时刻 got %q want %q", got, want)
		}
		for _, r := range st.refs {
			if r != "uuid-777" {
				t.Errorf("SyncStatus ref got %q want uuid-777", r)
			}
		}
	})

	t.Run("sync_error_does_not_change_outcome", func(t *testing.T) {
		st := &syncingTracker{fakeLinear: &fakeLinear{issue: demoIssue()}, err: errors.New("云效返回 500：云效服务端错误")}
		final, rows := runToPROpen(t, st)
		if final.State != task.StatePROpen {
			t.Errorf("同步失败时终态 got %s want pr_open", final.State)
		}
		if got, want := st.got(), "started,pr_open"; got != want {
			t.Errorf("SyncStatus 时刻 got %q want %q", got, want)
		}
		if baselineRows == nil {
			t.Fatal("基线子用例没跑出验证行")
		}
		if len(rows) != len(baselineRows) {
			t.Errorf("verifications 行数 got %d want %d（%v vs %v）", len(rows), len(baselineRows), rows, baselineRows)
		}
	})

	t.Run("non_syncer_unchanged", func(t *testing.T) {
		lin := &fakeLinear{issue: demoIssue()}
		final, rows := runToPROpen(t, lin)
		if final.State != task.StatePROpen {
			t.Errorf("终态 got %s want pr_open", final.State)
		}
		if len(lin.comments) != 1 || !strings.Contains(lin.comments[0], "pull/42") {
			t.Errorf("回帖 got %v want 1 条含 PR 链接", lin.comments)
		}
		if baselineRows != nil && len(rows) != len(baselineRows) {
			t.Errorf("verifications 行数 got %d want %d", len(rows), len(baselineRows))
		}
	})
}

// AC-14
func TestMergePollerStatusSync(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, provider string) (*syncingTracker, *task.Task) {
		t.Helper()
		_, m, userID, repoID, repo := mergepollFixture(t)
		key := "YX-MP-" + strings.ReplaceAll(t.Name(), "/", "-")
		tk, err := m.Create(ctx, task.CreateParams{
			UserID: userID, RepoID: repoID, ExternalKey: key, ExternalID: "wi-" + key,
			TrackerProvider: provider,
		})
		if err != nil {
			t.Fatalf("建任务: %v", err)
		}
		toPROpenNoWorktree(t, m, tk.ID)
		if err := m.SetPRNumber(ctx, tk.ID, 88); err != nil {
			t.Fatal(err)
		}
		st := &syncingTracker{fakeLinear: &fakeLinear{}}
		gh := &fakeGitHub{prInfo: &github.PRInfo{Number: 88, State: "closed", Merged: true}}
		outcomes := &recordingOutcomes{}
		mp := &MergePoller{
			Tasks: m, ClientFactory: fixedClientFactory{clients: &fakeClients{lin: st, gh: gh}},
			RepoLookup: repoLookupFixed(repo), Notifier: &fakeNotifier{}, IssueOutcomes: outcomes,
		}
		fresh, err := m.Get(ctx, tk.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := mp.handleMerged(ctx, fresh); err != nil {
			t.Fatalf("handleMerged: %v", err)
		}
		after, _ := m.Get(ctx, tk.ID)
		if after.State != task.StateMerged {
			t.Fatalf("终态 got %s want merged", after.State)
		}
		st.mu.Lock()
		st.refs = append(st.refs, "outcomes:"+strings.Join(outcomes.calls, ","))
		st.mu.Unlock()
		return st, after
	}

	t.Run("yunxiao_syncs_merged", func(t *testing.T) {
		st, tk := run(t, tracker.ProviderYunxiao)
		if got, want := st.got(), "merged"; got != want {
			t.Errorf("SyncStatus 时刻 got %q want %q", got, want)
		}
		if len(st.refs) < 1 || st.refs[0] != *tk.ExternalID {
			t.Errorf("SyncStatus ref got %v want %q", st.refs, *tk.ExternalID)
		}
	})
	t.Run("internal_no_sync", func(t *testing.T) {
		st, tk := run(t, tracker.ProviderInternal)
		if got := st.got(); got != "" {
			t.Errorf("internal 任务不应调 SyncStatus，got %q", got)
		}
		want := "outcomes:" + tk.ExternalKey + "=" + "merged"
		if len(st.refs) != 1 || st.refs[0] != want {
			t.Errorf("internal 任务应只走 ApplyTaskOutcome，got %v want [%s]", st.refs, want)
		}
	})
}

// recordingOutcomes 记录 ApplyTaskOutcome 调用（"key=outcome"）。
type recordingOutcomes struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingOutcomes) ApplyTaskOutcome(ctx context.Context, userID int64, issueKey string, oc store.TaskOutcome) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, issueKey+"="+string(oc))
	return true, nil
}
