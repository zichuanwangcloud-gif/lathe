package tracker_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Clouditera/lathe/internal/store"
	"github.com/Clouditera/lathe/internal/tracker"
)

// 内置 tracker 的契约（docs/09 §3 F2/F5）：
// 与 Linear 实现共用同一份 Issue.Context()，行为对齐是结构性的；
// 这里要断言的是 DB 支撑实现自身的正确性 —— 写进去的评论
// 必须能在下一次 Issue() 里读回来（blocked_spec 提问回路的闭环）。

func localFixture(t *testing.T) (context.Context, *store.Store, int64, *store.IssueRow) {
	t.Helper()
	dsn := testDSN()
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Skipf("跳过数据库测试（先 make dev-infra && make migrate）: %v", err)
	}
	t.Cleanup(st.Close)

	nonce := fmt.Sprint(time.Now().UnixNano())
	var userID, repoID int64
	if err := st.Pool().QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"tracker-"+nonce+"@example.com").Scan(&userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.Pool().Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})
	if err := st.Pool().QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		userID, "acme/tracker-"+nonce).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}

	it, err := st.CreateIssue(ctx, store.CreateIssueParams{
		UserID: userID, RepoID: repoID,
		Title: "导入失败", Description: "点导入没反应，期望弹出文件选择框",
	})
	if err != nil {
		t.Fatalf("建工单失败: %v", err)
	}
	return ctx, st, userID, it
}

func TestLocalTrackerIssueRoundTrip(t *testing.T) {
	ctx, st, userID, it := localFixture(t)
	tr := tracker.NewLocalTracker(st, userID)

	got, err := tr.Issue(ctx, it.Key)
	if err != nil {
		t.Fatalf("Issue 失败: %v", err)
	}
	if got.Identifier != it.Key || got.Title != it.Title || got.Description != it.Description {
		t.Errorf("工单字段回读不符: %+v", got)
	}
	if got.StateName != store.IssueOpen {
		t.Errorf("StateName = %q，期望 open", got.StateName)
	}

	// agent 提问（署名）→ 人回复 → 再拉必须全读到（提问回路闭环）
	trT := tr.WithActor("task-99")
	if _, err := trT.Comment(ctx, it.Key, "请问期望行为是什么？"); err != nil {
		t.Fatalf("agent 提问写库失败: %v", err)
	}
	if _, err := st.AddComment(ctx, it.ID, &userID, nil, "期望是弹文件选择框"); err != nil {
		t.Fatalf("人回复写库失败: %v", err)
	}

	got2, err := tr.Issue(ctx, it.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2.Comments) != 2 {
		t.Fatalf("应读到 2 条评论，得到 %d", len(got2.Comments))
	}
	if got2.Comments[0].UserName != "task-99" {
		t.Errorf("agent 评论应署名 task-99，得到 %q", got2.Comments[0].UserName)
	}

	// Context() 必须包含描述与双方评论 —— 这是分诊重试时的全部依据
	c := got2.Context()
	for _, want := range []string{it.Key, it.Title, "弹文件选择框", "请问期望行为", "task-99"} {
		if !strings.Contains(c, want) {
			t.Errorf("Context() 缺少 %q:\n%s", want, c)
		}
	}
}

func TestLocalTrackerOwnerIsolation(t *testing.T) {
	ctx, st, userID, it := localFixture(t)

	// 另一个用户视角的 tracker：按 (属主, key) 解析，读不到别人的单
	other := tracker.NewLocalTracker(st, userID+99999)
	if _, err := other.Issue(ctx, it.Key); err == nil {
		t.Error("非属主应读不到工单")
	}
	if _, err := other.Comment(ctx, it.Key, "x"); err == nil {
		t.Error("非属主应不能评论")
	}
}

// testDSN 与 internal/store、internal/httpapi 的测试口径一致。
func testDSN() string {
	if dsn := strings.TrimSpace(os.Getenv("LATHE_TEST_DSN")); dsn != "" {
		return dsn
	}
	return "postgres://lathe:lathe@127.0.0.1:55432/lathe?sslmode=disable"
}
