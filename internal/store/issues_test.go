package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 内置工单体系的 store 层契约（docs/09-internal-issues.md §3 F1/F2/F4）。
// 测试库是共享开发库，fixture 一律带随机量（05-roadmap §1.3 的教训）。

// issueFixture 建一个用户 + 仓库，供工单测试挂接。
func issueFixture(t *testing.T, st *Store) (userID, repoID int64) {
	t.Helper()
	ctx := context.Background()
	nonce := fmt.Sprint(time.Now().UnixNano())
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"issue-"+nonce+"@example.com").Scan(&userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		userID, "acme/issues-"+nonce).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	return userID, repoID
}

func TestIssueCreateAssignsGlobalLTKey(t *testing.T) {
	st := testStore(t)
	userID, repoID := issueFixture(t, st)
	ctx := context.Background()

	a, err := st.CreateIssue(ctx, CreateIssueParams{UserID: userID, RepoID: repoID, Title: "第一个"})
	if err != nil {
		t.Fatalf("建工单失败: %v", err)
	}
	b, err := st.CreateIssue(ctx, CreateIssueParams{UserID: userID, RepoID: repoID, Title: "第二个"})
	if err != nil {
		t.Fatalf("建工单失败: %v", err)
	}
	if !strings.HasPrefix(a.Key, "LT-") || !strings.HasPrefix(b.Key, "LT-") {
		t.Errorf("key 应是 LT-<n> 形态: %q, %q", a.Key, b.Key)
	}
	if a.Key == b.Key {
		t.Errorf("key 必须全局唯一: 两次得到 %q", a.Key)
	}
	if a.State != IssueOpen || a.Priority != 0 || a.Description != "" {
		t.Errorf("默认值不符: %+v", a)
	}
}

func TestIssueUpdateStateTransitionGuard(t *testing.T) {
	st := testStore(t)
	userID, repoID := issueFixture(t, st)
	ctx := context.Background()

	it, err := st.CreateIssue(ctx, CreateIssueParams{UserID: userID, RepoID: repoID, Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	// open → done 不在转移表里（必须先经过 in_progress）
	_, err = st.UpdateIssue(ctx, it.ID, userID, UpdateIssueParams{State: ptr(IssueDone)})
	var te ErrIssueTransition
	if !errors.As(err, &te) {
		t.Fatalf("open→done 应被拒，得到 %v", err)
	}
	// open → in_progress → done 是合法链
	if _, err := st.UpdateIssue(ctx, it.ID, userID, UpdateIssueParams{State: ptr(IssueInProgress)}); err != nil {
		t.Fatalf("open→in_progress 应合法: %v", err)
	}
	if _, err := st.UpdateIssue(ctx, it.ID, userID, UpdateIssueParams{State: ptr(IssueDone)}); err != nil {
		t.Fatalf("in_progress→done 应合法: %v", err)
	}
	// done → open 允许（重开）
	got, err := st.UpdateIssue(ctx, it.ID, userID, UpdateIssueParams{State: ptr(IssueOpen)})
	if err != nil || got.State != IssueOpen {
		t.Fatalf("done→open 应合法: %v, %+v", err, got)
	}
}

func TestIssueApplyTaskOutcomeIsMonotone(t *testing.T) {
	st := testStore(t)
	userID, repoID := issueFixture(t, st)
	ctx := context.Background()

	it, _ := st.CreateIssue(ctx, CreateIssueParams{UserID: userID, RepoID: repoID, Title: "t"})

	// started: open → in_progress
	ok, err := st.ApplyTaskOutcome(ctx, userID, it.Key, OutcomeStarted)
	if !ok || err != nil {
		t.Fatalf("started 联动应生效: ok=%v err=%v", ok, err)
	}
	// 人手动改回 open 后，merged 联动 → done（open 在 merged 的起点集里）
	if _, err := st.UpdateIssue(ctx, it.ID, userID, UpdateIssueParams{State: ptr(IssueOpen)}); err != nil {
		t.Fatal(err)
	}
	ok, _ = st.ApplyTaskOutcome(ctx, userID, it.Key, OutcomeMerged)
	if !ok {
		t.Fatal("merged 联动应生效")
	}
	got, _ := st.GetIssue(ctx, it.ID, userID)
	if got.State != IssueDone {
		t.Errorf("merged 后应为 done，得到 %s", got.State)
	}
	// 人已标 done 的工单，task_cancelled 联动不得把它打回 open
	ok, _ = st.ApplyTaskOutcome(ctx, userID, it.Key, OutcomeTaskCancelled)
	if ok {
		t.Error("done 状态的工单不应被 task_cancelled 联动改动")
	}
	if got, _ := st.GetIssue(ctx, it.ID, userID); got.State != IssueDone {
		t.Errorf("人的终态被联动覆盖: %s", got.State)
	}
}

func TestIssueDeleteBlockedByTasks(t *testing.T) {
	st := testStore(t)
	userID, repoID := issueFixture(t, st)
	ctx := context.Background()

	it, _ := st.CreateIssue(ctx, CreateIssueParams{UserID: userID, RepoID: repoID, Title: "t"})

	// 无关联任务：可删
	if err := st.DeleteIssue(ctx, it.ID, userID); err != nil {
		t.Fatalf("无关联任务的工单应可删: %v", err)
	}

	// 有关联任务（哪怕已终结）：只能取消不能删
	it2, _ := st.CreateIssue(ctx, CreateIssueParams{UserID: userID, RepoID: repoID, Title: "t2"})
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO tasks (user_id, repo_id, external_key, tracker_provider, state)
		VALUES ($1, $2, $3, 'internal', 'failed')`,
		userID, repoID, it2.Key); err != nil {
		t.Fatalf("建关联任务失败: %v", err)
	}
	if err := st.DeleteIssue(ctx, it2.ID, userID); !errors.Is(err, ErrIssueHasTasks) {
		t.Fatalf("有关联任务的工单应报 ErrIssueHasTasks，得到 %v", err)
	}
}

func TestIssueCommentsAuthorXOR(t *testing.T) {
	st := testStore(t)
	userID, repoID := issueFixture(t, st)
	ctx := context.Background()

	it, _ := st.CreateIssue(ctx, CreateIssueParams{UserID: userID, RepoID: repoID, Title: "t"})

	// 人评论
	c1, err := st.AddComment(ctx, it.ID, &userID, nil, "补充：复现步骤是……")
	if err != nil {
		t.Fatalf("人评论失败: %v", err)
	}
	// agent 评论（actor 署名）
	actor := "task-42"
	if _, err := st.AddComment(ctx, it.ID, nil, &actor, "请问期望行为是什么？"); err != nil {
		t.Fatalf("agent 评论失败: %v", err)
	}
	// 两者都给 / 都不给：拒绝
	if _, err := st.AddComment(ctx, it.ID, &userID, &actor, "x"); err == nil {
		t.Error("作者二选一被违反（两者都给）")
	}
	if _, err := st.AddComment(ctx, it.ID, nil, nil, "x"); err == nil {
		t.Error("作者二选一被违反（都不给）")
	}

	comments, err := st.ListComments(ctx, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 2 {
		t.Fatalf("应有 2 条评论，得到 %d", len(comments))
	}
	if comments[0].ID != c1.ID {
		t.Error("评论应按写入顺序排列")
	}
	if comments[0].AuthorName == "" || comments[1].AuthorName != "task-42" {
		t.Errorf("AuthorName 口径不对: %q / %q", comments[0].AuthorName, comments[1].AuthorName)
	}
}

func TestIssueOwnershipIsolation(t *testing.T) {
	st := testStore(t)
	userA, repoA := issueFixture(t, st)
	userB, _ := issueFixture(t, st)
	ctx := context.Background()

	it, _ := st.CreateIssue(ctx, CreateIssueParams{UserID: userA, RepoID: repoA, Title: "A 的单"})

	// B 读 A 的工单：ErrIssueNotFound（不暴露存在）
	if _, err := st.GetIssue(ctx, it.ID, userB); !errors.Is(err, ErrIssueNotFound) {
		t.Errorf("非属主读应 404 语义，得到 %v", err)
	}
	// B 的列表里看不到 A 的单
	list, _, err := st.ListIssues(ctx, ListIssuesParams{UserID: userB})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range list {
		if x.Key == it.Key {
			t.Error("B 的工单列表里出现了 A 的工单")
		}
	}
	// B 删 A 的单：同 404 语义
	if err := st.DeleteIssue(ctx, it.ID, userB); !errors.Is(err, ErrIssueNotFound) {
		t.Errorf("非属主删应 404 语义，得到 %v", err)
	}
}

func ptr[T any](v T) *T { return &v }
