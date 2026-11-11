package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/notify"
	"github.com/zichuanwangcloud-gif/lathe/internal/runner"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
	"github.com/zichuanwangcloud-gif/lathe/internal/tracker"
)

// e2eClients：需求平台永远拉不到工单（走 StageFetchIssue 失败路径），GitHub 不会被调到。
type e2eClients struct{}

type e2eTracker struct{}

func (e2eTracker) Issue(context.Context, string) (*tracker.Issue, error) {
	return nil, errors.New("401 unauthorized")
}
func (e2eTracker) Comment(context.Context, string, string) (string, error) { return "", nil }

type e2eGitHub struct{ runner.GitHubAPI }

func (e2eClients) Tracker(context.Context, string) (tracker.Tracker, error) { return e2eTracker{}, nil }
func (e2eClients) GitHub(context.Context) (runner.GitHubAPI, error)         { return e2eGitHub{}, nil }

// AC-23：全链路真实现 —— 保存渠道（测试消息）→ 任务失败 → 入队 → worker 投递 → 投递列表。
func TestNotifyEndToEndFeishu(t *testing.T) {
	hook := newFakeWebhook(t, feishuOK)
	f := newNotifyFix(t, hook.srv.Listener.Addr().String(), hook.srv.Client())
	ctx := context.Background()
	const signSecret = "e2e-sign-secret"

	// 1. 保存即测
	v := f.srv.decode(t, f.srv.do(t, "PUT", "/api/notify/channels/feishu", putBody(hook.srv.URL+"/hook", signSecret, ""), true))
	tr := testResult(t, v)
	if tr == nil || tr["ok"] != true || tr["deliveryId"] == nil {
		t.Fatalf("① got %v, want test.ok=true 带 deliveryId", v)
	}
	if hook.count() != 1 || !strings.Contains(hook.body(0), `"msg_type":"text"`) || !strings.Contains(hook.body(0), `"sign"`) {
		t.Fatalf("① 假服务器 got %d 次 %v", hook.count(), hook.bodies)
	}
	testID := int64(tr["deliveryId"].(float64))
	if d := f.delivery(t, testID); d.EventType != "test" || d.Status != "sent" || d.Attempts != 0 || d.SentAt == nil {
		t.Errorf("① 测试行 got %+v, want test/sent/0/sent_at 非空", d)
	}
	var testOK *bool
	_ = f.st.Pool().QueryRow(ctx, `SELECT test_ok FROM notify_channels WHERE user_id=$1`, f.userID).Scan(&testOK)
	if testOK == nil || !*testOK {
		t.Errorf("① test_ok got %v, want true", testOK)
	}

	// 2. worker 不补发测试行
	worker := notify.NewWorker(f.nc, f.sender, "")
	if err := worker.DrainOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if hook.count() != 1 {
		t.Errorf("② 请求数 got %d, want 1", hook.count())
	}

	// 3. 真 Pipeline 跑一个注定失败的任务
	m := task.NewMachine(f.st.Pool())
	var repoID int64
	if err := f.st.Pool().QueryRow(ctx, `INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		f.userID, "acme/e2e-"+randHex(t)).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	extKey := "CR-E2E-" + randHex(t)
	tk, err := m.Create(ctx, task.CreateParams{UserID: f.userID, RepoID: repoID, ExternalKey: extKey})
	if err != nil {
		t.Fatal(err)
	}
	wm, err := runner.NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &runner.Pipeline{Tasks: m, Worktrees: wm, Clients: e2eClients{}, Chat: f.nc}
	if err := p.Execute(ctx, runner.ExecuteParams{TaskID: tk.ID, Repo: runner.DefaultRepoConfig("acme/e2e"), IssueRef: "x"}); err == nil {
		t.Fatal("③ Execute 应返回错误")
	}
	if got, _ := m.Get(ctx, tk.ID); got == nil || got.State != task.StateFailed {
		t.Fatalf("③ state got %v, want failed", got)
	}

	// 4. 等入队，再循环 DrainOnce 直到本行处理完
	deadline := time.Now().Add(2 * time.Second)
	for f.countDeliveries(t, f.userID, `d.event_type='failed'`) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("④ 2s 内没有 failed 投递行")
		}
		time.Sleep(50 * time.Millisecond)
	}
	f.drainMine(t, worker)

	// 5.
	if hook.count() != 2 || !strings.Contains(hook.body(1), extKey) {
		t.Fatalf("⑤ 请求数 got %d, want 2 且第 2 次含 %s", hook.count(), extKey)
	}
	if n := f.countDeliveries(t, f.userID, `d.event_type='failed' AND d.status='sent' AND d.attempts=0`); n != 1 {
		t.Errorf("⑤ failed 行 sent/0 got %d, want 1", n)
	}

	// 6. 投递列表
	resp := f.srv.do(t, "GET", "/api/notify/deliveries", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("⑥ status %d", resp.StatusCode)
	}
	raw := f.srv.raw(t, resp)
	if strings.Contains(raw, "/hook") || strings.Contains(raw, signSecret) {
		t.Errorf("⑥ 投递列表泄漏 URL/密钥：%s", raw)
	}
	lv := f.srv.decode(t, f.srv.do(t, "GET", "/api/notify/deliveries", "", true))
	list, _ := lv["deliveries"].([]any)
	if len(list) != 2 {
		t.Fatalf("⑥ 条数 got %d, want 2：%v", len(list), lv)
	}
	types := map[string]bool{}
	for _, it := range list {
		m := it.(map[string]any)
		types[m["eventType"].(string)] = true
		if m["status"] != "sent" || m["attempts"].(float64) != 0 {
			t.Errorf("⑥ got %v, want sent/0", m)
		}
	}
	if !types["test"] || !types["failed"] {
		t.Errorf("⑥ eventType got %v, want test+failed", types)
	}
}
