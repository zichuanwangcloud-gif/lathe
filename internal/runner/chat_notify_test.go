package runner

// chat_notify_test.go 站外通知（f09）与状态机之间的约定：
// 通知是转移之后的副作用 —— notifier 坏了/卡了/panic 了，任务照样落在目标状态；
// 入参是转移那一刻的快照；脱敏在 runner 侧做。

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
	"github.com/zichuanwangcloud-gif/lathe/internal/integration/github"
	"github.com/zichuanwangcloud-gif/lathe/internal/notify"
	"github.com/zichuanwangcloud-gif/lathe/internal/secret"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
	"github.com/zichuanwangcloud-gif/lathe/internal/testsupport"
)

// 编译期：真实现满足窄接口（签名一漂移就编译失败，不用等装配测试）。
var _ ChatNotifier = (*store.NotifyChannels)(nil)

// ---------------------------------------------------------------- 假 notifier

type captureChat struct{ ch chan notify.TaskEvent }

func newCaptureChat() *captureChat { return &captureChat{ch: make(chan notify.TaskEvent, 16)} }

func (c *captureChat) EnqueueTaskEvent(_ context.Context, ev notify.TaskEvent) (int, error) {
	c.ch <- ev
	return 1, nil
}

func (c *captureChat) next(t *testing.T) notify.TaskEvent {
	t.Helper()
	select {
	case ev := <-c.ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("2s 内没收到 TaskEvent")
		return notify.TaskEvent{}
	}
}

type brokenChat struct{ mode string }

func (b brokenChat) EnqueueTaskEvent(ctx context.Context, _ notify.TaskEvent) (int, error) {
	switch b.mode {
	case "panic":
		panic("chat notifier 炸了")
	case "hang":
		select {
		case <-time.After(10 * time.Second):
		case <-ctx.Done():
		}
		return 0, ctx.Err()
	}
	return 0, errors.New("chat notifier 永远失败")
}

// ---------------------------------------------------------------- 真 NC

func realNC(t *testing.T) *store.NotifyChannels {
	t.Helper()
	ctx, cancel := testsupport.ConnectContext(t)
	defer cancel()
	st, err := store.Open(ctx, dsnForTest())
	if err != nil {
		testsupport.SkipOrFail(t, "打开 store 失败: %v", err)
	}
	t.Cleanup(st.Close)
	key := make([]byte, secret.KeySize)
	_, _ = rand.Read(key)
	sealer, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return st.NewNotifyChannels(sealer, notify.AllowedHosts)
}

// addSlack 给用户配一条 slack 渠道。URL 指向白名单 host，但不跑 worker，不会被请求。
func addSlack(t *testing.T, nc *store.NotifyChannels, userID int64) {
	t.Helper()
	if _, err := nc.SaveChannel(context.Background(), userID, store.SaveChannelParams{
		Kind: notify.KindSlack, URL: "https://hooks.slack.com/services/T000/B000/runner-test", Enabled: true,
		Events: notify.EventSwitches{Failed: true, BlockedSpec: true, AwaitingApproval: true, PROpen: true, PRDReady: true},
	}); err != nil {
		t.Fatalf("配置 slack 渠道失败: %v", err)
	}
}

type deliveryRow struct {
	EventKey string
	Payload  notify.Payload
}

// waitDeliveries 轮询（50ms、2s 上限）本用户 eventType 的投递行，直到行数 >= want。
func waitDeliveries(t *testing.T, pool *pgxpool.Pool, userID int64, eventType string, want int) []deliveryRow {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		rows, err := pool.Query(context.Background(), `
			SELECT d.event_key, d.payload FROM notify_deliveries d JOIN notify_channels c ON c.id=d.channel_id
			WHERE c.user_id=$1 AND d.event_type=$2 ORDER BY d.id`, userID, eventType)
		if err != nil {
			t.Fatal(err)
		}
		var out []deliveryRow
		for rows.Next() {
			var r deliveryRow
			var raw []byte
			if err := rows.Scan(&r.EventKey, &raw); err != nil {
				t.Fatal(err)
			}
			_ = json.Unmarshal(raw, &r.Payload)
			out = append(out, r)
		}
		rows.Close()
		if len(out) >= want || time.Now().After(deadline) {
			return out
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func taskOwner(t *testing.T, m *task.Machine, id int64) *task.Task {
	t.Helper()
	tk, err := m.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// ---------------------------------------------------------------- AC-33

func TestStateSubjectMatchesNotifyLabel(t *testing.T) {
	cases := []struct {
		s    task.State
		want string
	}{
		{task.StateFailed, "处理失败"},
		{task.StatePROpen, "已开 PR，待你评审"},
		{task.StateAwaitingApproval, "验证已通过，等你放行"},
		{task.StateMerged, "已合并"},
		{task.StateCancelled, "已取消"},
		{task.StateBlockedSpec, "需求不明确，已回帖提问"},
		{task.State("weird"), "weird"},
	}
	for _, c := range cases {
		t.Run(string(c.s), func(t *testing.T) {
			if got := stateSubject(c.s); got != c.want {
				t.Errorf("stateSubject got %q, want %q", got, c.want)
			}
			if got := notify.StateLabel(string(c.s)); got != c.want {
				t.Errorf("notify.StateLabel got %q, want %q", got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------- AC-3a

func TestChatTerminalRedactsReasonBeforeEnqueue(t *testing.T) {
	const token = "sekrit-token-9f8e7d6c5b4a"
	cases := []struct {
		name      string
		reason    string
		truncated bool
	}{
		{"长原因：脱敏且按预算截断带后缀", "调用失败 token=" + token + " " + strings.Repeat("中文原因", 4000/12+1), true},
		{"短原因：脱敏不加后缀", "boom " + token + " end", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			chat := newCaptureChat()
			p := &Pipeline{Chat: chat, KnownSecrets: []string{token}}
			r, stage := c.reason, "implement"
			tk := &task.Task{ID: 1, UserID: 2, ExternalKey: "CR-1", State: task.StateFailed,
				FailureReason: &r, FailureStage: &stage, UpdatedAt: time.Now()}
			p.chatTerminal(context.Background(), tk, notify.EventFailed)
			ev := chat.next(t)

			if strings.Contains(ev.Reason, token) {
				t.Errorf("Reason 含明文 token：got %q", ev.Reason)
			}
			if !strings.Contains(ev.Reason, "[REDACTED]") {
				t.Errorf("Reason got %q, want 含 [REDACTED]", ev.Reason)
			}
			if !utf8.ValidString(ev.Reason) {
				t.Error("Reason 不是合法 UTF-8")
			}
			if c.truncated {
				if n := len(ev.Reason); n > notifyReasonBudget || n < notifyReasonBudget-(utf8.UTFMax-1) {
					t.Errorf("len(Reason) got %d, want ∈ [%d, %d]", n, notifyReasonBudget-(utf8.UTFMax-1), notifyReasonBudget)
				}
				if !strings.HasSuffix(ev.Reason, truncatedSuffix) {
					t.Errorf("Reason 应以 %q 结尾（走到了截断分支），got 尾部 %q", truncatedSuffix, ev.Reason[len(ev.Reason)-20:])
				}
			} else if strings.HasSuffix(ev.Reason, truncatedSuffix) {
				t.Errorf("未截断不应加后缀，got %q", ev.Reason)
			}
			if ev.FailureStage != "implement" || ev.TaskID != 1 || ev.UserID != 2 || ev.EventType != notify.EventFailed {
				t.Errorf("快照字段 got %+v", ev)
			}
		})
	}
}

// ---------------------------------------------------------------- AC-19

func TestPipelineFailureStillFailsWithBrokenChatNotifier(t *testing.T) {
	for _, mode := range []string{"error", "panic", "hang"} {
		t.Run("notifier "+mode+" 时任务仍落 failed", func(t *testing.T) {
			_, m, taskID, repo, src := pipelineFixture(t)
			lin := &fakeLinear{issueErr: errors.New("401 unauthorized")}
			p := newPipeline(t, m, lin, &fakeGitHub{}, &fakeAgent{}, &fakeNotifier{})
			p.Chat = brokenChat{mode: mode}

			start := time.Now()
			err := p.Execute(context.Background(), ExecuteParams{TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777"})
			if err == nil {
				t.Fatal("拉 issue 失败应返回错误")
			}
			if el := time.Since(start); el > 5*time.Second {
				t.Errorf("Execute 耗时 %v：被 notifier 拖住了", el)
			}
			for _, bad := range []string{"chat", "通知", "notifier"} {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("Execute 的错误冒出了通知字样 %q：%v", bad, err)
				}
			}
			final := taskOwner(t, m, taskID)
			if final.State != task.StateFailed {
				t.Errorf("state got %s, want failed", final.State)
			}
			var n int
			if err := testPoolForPipeline(t).QueryRow(context.Background(),
				`SELECT count(*) FROM task_events WHERE task_id=$1 AND to_state='failed'`, taskID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Errorf("→failed 事件数 got %d, want 1", n)
			}
		})
	}
}

// ---------------------------------------------------------------- AC-20

type slowChat struct{}

func (slowChat) EnqueueTaskEvent(context.Context, notify.TaskEvent) (int, error) {
	time.Sleep(2 * time.Second)
	return 0, nil
}

func TestChatTerminalIsNonBlocking(t *testing.T) {
	p := &Pipeline{Chat: slowChat{}}
	start := time.Now()
	p.chatTerminal(context.Background(), &task.Task{ID: 1, State: task.StatePROpen}, notify.EventPROpen)
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Errorf("chatTerminal 耗时 got %v, want < 100ms", el)
	}
	t.Run("nil notifier / nil task 直接返回", func(t *testing.T) {
		(&Pipeline{}).chatTerminal(context.Background(), &task.Task{}, notify.EventFailed)
		(&Pipeline{Chat: slowChat{}}).chatTerminal(context.Background(), nil, notify.EventFailed)
	})
}

// ---------------------------------------------------------------- AC-20b

// gateChat 包装真 NC：EnqueueTaskEvent 先阻塞到测试放行，再调真实现。
type gateChat struct {
	release chan struct{}
	inner   ChatNotifier
}

func (g *gateChat) EnqueueTaskEvent(ctx context.Context, ev notify.TaskEvent) (int, error) {
	<-g.release
	return g.inner.EnqueueTaskEvent(ctx, ev)
}

func TestChatTerminalSnapshotsBeforeGoroutine(t *testing.T) {
	pool, m, taskID, _, _ := pipelineFixture(t)
	ctx := context.Background()
	nc := realNC(t)
	tk := taskOwner(t, m, taskID)
	addSlack(t, nc, tk.UserID)

	gated, err := m.Transition(ctx, taskID, task.StateVerifying, "test", nil)
	if err == nil {
		gated, err = m.Transition(ctx, taskID, task.StateAwaitingApproval, "test", nil)
	}
	if err != nil {
		t.Fatalf("推到 awaiting_approval 失败: %v", err)
	}
	before := gated.UpdatedAt

	g := &gateChat{release: make(chan struct{}), inner: nc}
	p := &Pipeline{Chat: g}
	p.chatTerminal(ctx, gated, notify.EventAwaitingApproval)

	// 调用返回后、放行前：改写调用方手里的 tk 与库里的行
	gated.State = task.StateQueued
	gated.UpdatedAt = before.Add(time.Hour)
	if _, err := pool.Exec(ctx, `UPDATE tasks SET state='queued', updated_at=$2 WHERE id=$1`, taskID, before.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	close(g.release)

	rows := waitDeliveries(t, pool, tk.UserID, notify.EventAwaitingApproval, 1)
	if len(rows) != 1 {
		t.Fatalf("投递行 got %d, want 1", len(rows))
	}
	want := fmt.Sprintf("task:%d:awaiting_approval:%s", taskID, before.UTC().Format(time.RFC3339Nano))
	if rows[0].EventKey != want {
		t.Errorf("event_key got %q, want %q", rows[0].EventKey, want)
	}
	if rows[0].Payload.ToState != "awaiting_approval" {
		t.Errorf("payload.to_state got %q, want awaiting_approval", rows[0].Payload.ToState)
	}
}

// ---------------------------------------------------------------- AC-21

func TestPipelineEmitsBlockedSpecAndGateAndPROpen(t *testing.T) {
	type outcome struct {
		state task.State
		event string
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, pool *pgxpool.Pool, taskID int64) (AgentDriver, *fakeGitHub)
		want  outcome
		check func(t *testing.T, pool *pgxpool.Pool, p *Pipeline, tk *task.Task, row deliveryRow)
	}{
		{"分诊 blocked_spec", func(t *testing.T, _ *pgxpool.Pool, _ int64) (AgentDriver, *fakeGitHub) {
			return &fakeAgent{results: []*agent.Result{{Success: true,
				Text: `{"actionable":false,"kind":"fix","reason":"只有一句现象","question":"能补充复现步骤吗？"}`}}}, &fakeGitHub{}
		}, outcome{task.StateBlockedSpec, notify.EventBlockedSpec}, nil},
		{"heavy 红阶段 blocked_spec", func(t *testing.T, _ *pgxpool.Pool, _ int64) (AgentDriver, *fakeGitHub) {
			return &fakeAgent{
				results: []*agent.Result{{Success: true, Text: `{"actionable":true,"kind":"fix"}`}, {Success: true, Text: "加了测试与小改动"}},
				mutate: []func(string) error{nil, func(dir string) error {
					if err := os.WriteFile(filepath.Join(dir, "main_test.go"),
						[]byte("package main\n\nimport \"testing\"\n\nfunc TestMainRuns(t *testing.T) { main() }\n"), 0o644); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(dir, "fix.go"), []byte("package main\n\nfunc touched() {}\n"), 0o644)
				}},
			}, &fakeGitHub{}
		}, outcome{task.StateBlockedSpec, notify.EventBlockedSpec},
			func(t *testing.T, pool *pgxpool.Pool, p *Pipeline, tk *task.Task, row deliveryRow) {
				var reason string
				if err := pool.QueryRow(context.Background(), `SELECT coalesce(payload->>'reason','') FROM task_events
					WHERE task_id=$1 ORDER BY id DESC LIMIT 1`, tk.ID).Scan(&reason); err != nil {
					t.Fatal(err)
				}
				if reason != "repro_not_red" {
					t.Errorf("最后一次转移 payload.reason got %q, want repro_not_red（应走 heavy 红阶段那一处）", reason)
				}
				if tk.FailureReason == nil {
					t.Fatal("failure_reason 应已落库")
				}
				want := truncate(redactText(*tk.FailureReason, p.KnownSecrets, nil), notifyReasonBudget-len(truncatedSuffix))
				if row.Payload.Reason != want {
					t.Errorf("payload.reason got %q, want %q", row.Payload.Reason, want)
				}
				if row.Payload.FailureStage != "" {
					t.Errorf("payload.failure_stage got %q, want 缺省", row.Payload.FailureStage)
				}
			}},
		{"gate awaiting_approval", func(t *testing.T, pool *pgxpool.Pool, taskID int64) (AgentDriver, *fakeGitHub) {
			if _, err := pool.Exec(context.Background(), `UPDATE tasks SET gate_mode='manual' WHERE id=$1`, taskID); err != nil {
				t.Fatal(err)
			}
			return manualGateAgent(), &fakeGitHub{pr: &github.PullRequest{Number: 42, URL: "https://github.com/acme/demo/pull/42"}}
		}, outcome{task.StateAwaitingApproval, notify.EventAwaitingApproval}, nil},
		{"pr_open", func(t *testing.T, _ *pgxpool.Pool, _ int64) (AgentDriver, *fakeGitHub) {
			return manualGateAgent(), &fakeGitHub{pr: &github.PullRequest{Number: 42, URL: "https://github.com/acme/demo/pull/42"}}
		}, outcome{task.StatePROpen, notify.EventPROpen},
			func(t *testing.T, _ *pgxpool.Pool, _ *Pipeline, _ *task.Task, row deliveryRow) {
				if row.Payload.PRURL != "https://github.com/acme/demo/pull/42" {
					t.Errorf("payload.pr_url got %q", row.Payload.PRURL)
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool, m, taskID, repo, src := pipelineFixture(t)
			nc := realNC(t)
			owner := taskOwner(t, m, taskID)
			addSlack(t, nc, owner.UserID)
			ag, gh := c.setup(t, pool, taskID)

			p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, gh, ag, &fakeNotifier{})
			p.SettingSources = "project"
			p.Chat = nc
			if err := p.Execute(context.Background(), ExecuteParams{
				TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
			}); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			final := taskOwner(t, m, taskID)
			if final.State != c.want.state {
				t.Fatalf("state got %s, want %s", final.State, c.want.state)
			}
			rows := waitDeliveries(t, pool, owner.UserID, c.want.event, 1)
			if len(rows) != 1 {
				t.Fatalf("got %d rows, want 1（event_type=%s）", len(rows), c.want.event)
			}
			if prefix := fmt.Sprintf("task:%d:%s:", taskID, c.want.state); !strings.HasPrefix(rows[0].EventKey, prefix) {
				t.Errorf("event_key got %q, want 前缀 %q", rows[0].EventKey, prefix)
			}
			if rows[0].Payload.ExternalKey != final.ExternalKey {
				t.Errorf("payload.external_key got %q, want %q", rows[0].Payload.ExternalKey, final.ExternalKey)
			}
			if c.check != nil {
				c.check(t, pool, p, final, rows[0])
			}
		})
	}
}
