package store

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

// notifyFixture 造一对 user/repo 与一条任务，返回 taskID 与登录邮箱。
//
// email 带随机量：确定性 email 配 ON CONFLICT 会复用上一轮被中断留下的
// 孤儿行，连带它名下的非终态任务撞唯一索引（docs/08-debt-cleanup.md §7 的教训）。
func notifyFixture(t *testing.T, st *Store, notifyEmail *string) (taskID int64, login string) {
	t.Helper()
	ctx := context.Background()
	nonce := strconv.FormatInt(time.Now().UnixNano(), 10)
	login = "notify-" + t.Name() + "-" + nonce + "@example.com"

	var userID int64
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO users (email, notify_email) VALUES ($1, $2) RETURNING id`,
		login, notifyEmail).Scan(&userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})

	var repoID int64
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1, $2) RETURNING id`,
		userID, "acme/notify-"+nonce).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO tasks (user_id, repo_id, external_key) VALUES ($1, $2, $3) RETURNING id`,
		userID, repoID, "NT-"+nonce).Scan(&taskID); err != nil {
		t.Fatalf("建 task 失败: %v", err)
	}
	return taskID, login
}

// 配了 notify_email 就发到它上面。
func TestNotifyEmailForTaskPrefersNotifyEmail(t *testing.T) {
	st := testStore(t)
	want := "alerts-" + t.Name() + "@example.com"
	taskID, login := notifyFixture(t, st, &want)

	got, err := st.NotifyEmailForTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("NotifyEmailForTask 失败: %v", err)
	}
	if got != want {
		t.Errorf("收件人 = %q，期望 notify_email %q（而不是登录邮箱 %q）", got, want, login)
	}
}

// notify_email 为 NULL 时回退登录邮箱（T3 AC1）。
func TestNotifyEmailForTaskFallsBackToLogin(t *testing.T) {
	st := testStore(t)
	taskID, login := notifyFixture(t, st, nil)

	got, err := st.NotifyEmailForTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("NotifyEmailForTask 失败: %v", err)
	}
	if got != login {
		t.Errorf("收件人 = %q，期望回退到登录邮箱 %q", got, login)
	}
}

// 空串与 NULL 同义：界面上把通知邮箱清空存下来的就是空串，
// 不能因此发出一封收件人为 "" 的信。
func TestNotifyEmailForTaskTreatsEmptyAsUnset(t *testing.T) {
	st := testStore(t)
	empty := ""
	taskID, login := notifyFixture(t, st, &empty)

	got, err := st.NotifyEmailForTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("NotifyEmailForTask 失败: %v", err)
	}
	if got != login {
		t.Errorf("空串的 notify_email 应视为未设置并回退登录邮箱，得到 %q", got)
	}
}

// 任务不存在时返回 ErrNoRecipient，而不是一个空收件人 ——
// 调用方据此静默跳过，不会把「没人可发」当成发信故障刷日志。
func TestNotifyEmailForTaskMissingTask(t *testing.T) {
	st := testStore(t)
	_, err := st.NotifyEmailForTask(context.Background(), 999999999)
	if !errors.Is(err, ErrNoRecipient) {
		t.Errorf("期望 ErrNoRecipient，得到 %v", err)
	}
}
