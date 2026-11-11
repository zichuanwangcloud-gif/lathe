package main

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/flow"
	"github.com/zichuanwangcloud-gif/lathe/internal/runner"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
	"github.com/zichuanwangcloud-gif/lathe/internal/vcs"
)

// vcsUser 建一个只登记了一个 GitLab 仓库的用户（平台只来自 store.CreateRepo 登记，
// 测试不直接写任何平台列）。
func vcsUser(t *testing.T, st *store.Store, provider vcs.Provider, base, path string) (userID, repoID int64) {
	t.Helper()
	ctx := context.Background()
	email := "vcs-q-" + t.Name() + "-" + strconv.FormatInt(time.Now().UnixNano(), 10) + "@example.com"
	if err := st.Pool().QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`, email).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.Pool().Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID) })
	repo, err := st.CreateRepo(ctx, userID, store.CreateRepoParams{ProviderRepo: path, VCSProvider: string(provider), VCSBaseURL: base})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return userID, repo.ID
}

// AC-19：三条真实建任务入口建出的任务，派发时 ExecuteParams 带着仓库所在平台。
func TestEnqueuePathsCarryRepoPlatform(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	entries := []struct {
		name   string
		create func(t *testing.T, q *queue, userID, repoID int64)
	}{
		{"Linear（queue.Enqueue）", func(t *testing.T, q *queue, userID, _ int64) {
			k := uniqueKey("VCS-LIN")
			if err := q.Enqueue(ctx, userID, "uuid-"+k, k); err != nil {
				t.Fatal(err)
			}
		}},
		{"内置工单（queue.EnqueueInternal）", func(t *testing.T, q *queue, userID, repoID int64) {
			it, err := st.CreateIssue(ctx, store.CreateIssueParams{UserID: userID, RepoID: repoID, Title: "gitlab 工单"})
			if err != nil {
				t.Fatal(err)
			}
			if err := q.EnqueueInternal(ctx, userID, it.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"编排图（flow.Service.CreateFlow）", func(t *testing.T, q *queue, userID, repoID int64) {
			k := uniqueKey("VCS-FLOW")
			svc := &flow.Service{Pool: st.Pool(), Tasks: task.NewMachine(st.Pool())}
			if _, _, _, err := svc.CreateFlow(ctx, userID, repoID, "vcs", []flow.NodeInput{{IssueKey: k, IssueID: "uuid-" + k}}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, e := range entries {
		t.Run(e.name, func(t *testing.T) {
			userID, repoID := vcsUser(t, st, vcs.ProviderGitLab, "https://gitlab.example.com", "group/sub/demo")
			pipe := &fakePipeline{}
			q := testQueue(st, pipe)
			e.create(t, q, userID, repoID)
			tk := claimOwn(t, q, ctx, userID)
			if tk == nil {
				t.Fatal("没有领到任务")
			}
			q.runOneClaimed(ctx, tk)
			calls := pipe.snapshot()
			if len(calls) != 1 {
				t.Fatalf("pipeline 调用 %d 次，want 1", len(calls))
			}
			got := calls[0]
			checks := []struct{ name, got, want string }{
				{"Repo.VCSProvider", string(got.Repo.VCSProvider), "gitlab"},
				{"Repo.VCSBaseURL", got.Repo.VCSBaseURL, "https://gitlab.example.com"},
				{"Repo.RepoKey()", got.Repo.RepoKey(), "gitlab.example.com/group/sub/demo"},
				{"Repo.CloneURL()", got.Repo.CloneURL(), "https://gitlab.example.com/group/sub/demo.git"},
				{"ExecuteParams.CloneURL", got.CloneURL, "https://gitlab.example.com/group/sub/demo.git"},
			}
			for _, c := range checks {
				if c.got != c.want {
					t.Errorf("%s = %q，want %q", c.name, c.got, c.want)
				}
			}
		})
	}
}

// AC-5（TestGitHubRemoteUnchanged 的 cmd 部分）：GitHub 仓库的 cloneURL 与 cff5332 的拼接逐字相等。
func TestLoadRepoConfigGitHubCloneURLUnchanged(t *testing.T) {
	st := testStore(t)
	_, repoID := fixture(t, st)
	q := testQueue(st, &fakePipeline{})
	cfg, cloneURL, err := q.loadRepoConfig(context.Background(), repoID)
	if err != nil {
		t.Fatal(err)
	}
	if want := "git@github.com:" + cfg.ProviderRepo + ".git"; cloneURL != want {
		t.Errorf("cloneURL = %q，want %q", cloneURL, want)
	}
	if cfg.Target() != (vcs.Target{Provider: vcs.ProviderGitHub}) || cfg.RepoKey() != cfg.ProviderRepo {
		t.Errorf("github 仓库的 Target/RepoKey 变了：%+v %q", cfg.Target(), cfg.RepoKey())
	}
	var _ runner.RepoConfig = cfg
}
