package runner

import (
	"context"
	"testing"

	"github.com/Clouditera/lathe/internal/integration/github"
	"github.com/Clouditera/lathe/internal/task"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 本文件测 F2.3-AC2（PR 被关闭未合并 → blocked_dep）的完整驱动链路：
// MergePoller.pollOnce → pollTask → handleClosedUnmerged。这条逻辑在
// mergepoll.go 里早就写好了，但在本文件新增之前从未被任何测试驱动过。
//
// 不涉及 worktree/git（PR 被关闭未合并这件事本身与工作区状态无关，
// task/state.go 的 queued→blocked_dep 边也不要求有 worktree），因此
// 用比 rebase_followup_test.go 更轻的夹具：只建 user/repo/task，状态
// 用 Transition 直接摆到 pr_open，不跑真实 Pipeline.Execute。

// mergepollFixture 建最小夹具：user/repo，供 pollOnce/handleClosedUnmerged
// 驱动测试用——与 rebaseFollowupFixture 同一套手法，只是不需要真实源
// 仓库。
func mergepollFixture(t *testing.T) (pool *pgxpool.Pool, m *task.Machine, userID, repoID int64, repo RepoConfig) {
	t.Helper()
	pool = testPoolForPipeline(t)
	m = task.NewMachine(pool)
	ctx := context.Background()

	providerRepo := "acme/mergepoll-" + t.Name()
	email := "mergepoll-" + t.Name() + "@example.com"
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) ON CONFLICT (email) DO UPDATE SET updated_at=now() RETURNING id`,
		email).Scan(&userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2)
		 ON CONFLICT (user_id, provider_repo) DO UPDATE SET updated_at=now() RETURNING id`,
		userID, providerRepo).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})
	return pool, m, userID, repoID, DefaultRepoConfig(providerRepo)
}

// toPROpenNoWorktree 把任务从 queued 直接转到 pr_open，跳过
// triaging/implementing 等中间阶段——全程只走 task/state.go 里已经
// 存在的合法边（queued→verifying、verifying→pr_open），本文件的场景
// 不涉及真实 worktree/git，不需要经过完整实现流程。
func toPROpenNoWorktree(t *testing.T, m *task.Machine, id int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := m.Transition(ctx, id, task.StateVerifying, "system", nil); err != nil {
		t.Fatalf("任务 %d 转 verifying 失败: %v", id, err)
	}
	if _, err := m.Transition(ctx, id, task.StatePROpen, "system", nil); err != nil {
		t.Fatalf("任务 %d 转 pr_open 失败: %v", id, err)
	}
}

// TestMergePollHandleClosedUnmergedPropagatesBlockedDep 构造一条
// task1(pr_open, PR 将被关闭未合并)→task2(queued，直接后继)→
// task3(queued，间接后继) 的链，让 fakeGitHub.GetPRInfo 对 task1 返回
// {State: "closed", Merged: false}，调用 MergePoller.pollOnce(ctx)，
// 断言：
//
//   - task1 转 cancelled；
//   - task2（直接后继）、task3（间接后继）都转 blocked_dep；
//   - task2/task3 都收到回帖说明因前驱 PR 被关闭而阻塞。
func TestMergePollHandleClosedUnmergedPropagatesBlockedDep(t *testing.T) {
	_, m, userID, repoID, repo := mergepollFixture(t)
	ctx := context.Background()

	task1, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "MP-1", LinearIssueID: "uuid-mp-1",
	})
	if err != nil {
		t.Fatalf("建 task1 失败: %v", err)
	}
	task2, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "MP-2", LinearIssueID: "uuid-mp-2",
		DependsOn: &task1.ID,
	})
	if err != nil {
		t.Fatalf("建 task2 失败: %v", err)
	}
	task3, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "MP-3", LinearIssueID: "uuid-mp-3",
		DependsOn: &task2.ID,
	})
	if err != nil {
		t.Fatalf("建 task3 失败: %v", err)
	}

	toPROpenNoWorktree(t, m, task1.ID)
	if err := m.SetPRNumber(ctx, task1.ID, 42); err != nil {
		t.Fatalf("设置 task1.pr_number 失败: %v", err)
	}

	lin := newRebaseFollowupLinear()
	lin.add(rebaseFollowupIssue("uuid-mp-1", "MP-1"))
	lin.add(rebaseFollowupIssue("uuid-mp-2", "MP-2"))
	lin.add(rebaseFollowupIssue("uuid-mp-3", "MP-3"))

	gh := &fakeGitHub{prInfo: &github.PRInfo{Number: 42, Merged: false, State: "closed"}}
	notifier := &fakeNotifier{}

	mp := &MergePoller{
		Tasks:         m,
		ClientFactory: fixedClientFactory{clients: &fakeClients{lin: lin, gh: gh}},
		RepoLookup:    repoLookupFixed(repo),
		Notifier:      notifier,
	}

	if err := mp.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce 失败: %v", err)
	}

	// ---- 断言一：task1 转 cancelled ----
	t1After, err := m.Get(ctx, task1.ID)
	if err != nil {
		t.Fatalf("读取 task1 失败: %v", err)
	}
	if t1After.State != task.StateCancelled {
		t.Fatalf("task1 应转 cancelled，实际 state=%s", t1After.State)
	}

	// ---- 断言二：task2（直接后继）转 blocked_dep ----
	t2After, err := m.Get(ctx, task2.ID)
	if err != nil {
		t.Fatalf("读取 task2 失败: %v", err)
	}
	if t2After.State != task.StateBlockedDep {
		t.Fatalf("task2（直接后继）应转 blocked_dep，实际 state=%s", t2After.State)
	}

	// ---- 断言三：task3（间接后继）也转 blocked_dep ----
	t3After, err := m.Get(ctx, task3.ID)
	if err != nil {
		t.Fatalf("读取 task3 失败: %v", err)
	}
	if t3After.State != task.StateBlockedDep {
		t.Fatalf("task3（间接后继）应转 blocked_dep，实际 state=%s", t3After.State)
	}

	// ---- 断言四：task2/task3 都收到回帖，说明因前驱 PR 被关闭而阻塞 ----
	if len(lin.comments["uuid-mp-2"]) == 0 {
		t.Error("task2 应收到阻塞回帖")
	} else {
		body := lin.comments["uuid-mp-2"][len(lin.comments["uuid-mp-2"])-1]
		if !containsAny(body, "关闭", "closed") {
			t.Errorf("阻塞回帖应能看出因前驱 PR 被关闭而阻塞，实际: %s", body)
		}
	}
	if len(lin.comments["uuid-mp-3"]) == 0 {
		t.Error("task3（间接后继）也应收到阻塞回帖")
	}
}
