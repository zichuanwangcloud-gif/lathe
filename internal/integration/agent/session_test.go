package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// replayEcho 是假 CLI 的 bash 片段：从一帧 stdin 里抠出 uuid，按
// --replay-user-messages 的形状回显一行 isReplay 事件。
const replayEcho = `uid=$(printf '%s' "$line" | grep -o '"uuid":"[^"]*"' | head -1 | cut -d'"' -f4)
echo "{\"type\":\"user\",\"isReplay\":true,\"uuid\":\"$uid\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"回显\"}]}}"`

const testDeliveryID = "11111111-1111-4111-8111-111111111111"

// AC-5：注入真的到了正在跑的进程，且回显被接住；result 之后会话收尾。
func TestSessionDeliverMidRun(t *testing.T) {
	// 读首帧 → 回显；读第二帧（注入）→ 回显 → result；然后一直等 EOF。
	bin := fakeClaude(t, fmt.Sprintf(`
read -r line; %s
echo '%s'
read -r line; %s
echo '%s'
cat >/dev/null
`, replayEcho, sysInitLine, replayEcho, resultOKLine))

	var (
		mu      sync.Mutex
		waiters = map[string]chan struct{}{}
		acked   atomic.Bool
		sess    *Session
		ready   = make(chan struct{})
	)
	expect := func(id string) chan struct{} {
		mu.Lock()
		defer mu.Unlock()
		ch := make(chan struct{}, 1)
		waiters[id] = ch
		return ch
	}
	onDelivery := func(id string) {
		mu.Lock()
		defer mu.Unlock()
		if ch, ok := waiters[id]; ok {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}

	deliverErr := make(chan error, 1)
	invalidErr := make(chan error, 1)
	d := &Driver{bin: bin, timeout: 30 * time.Second, idle: 10 * time.Second}
	res, err := d.Run(context.Background(), RunParams{
		Prompt: "干活", SessionID: "sess-1", StreamInput: true,
		OnReady: func(s *Session) {
			sess = s
			close(ready)
			go func() {
				invalidErr <- s.Deliver(context.Background(), "not-a-uuid", "x")
				ack := expect(testDeliveryID) // 先 Expect 后 Deliver
				e := s.Deliver(context.Background(), testDeliveryID, "别动 migration")
				select {
				case <-ack:
					acked.Store(true)
				case <-time.After(2 * time.Second):
				}
				deliverErr <- e
			}()
		},
		OnDelivery: onDelivery,
	})
	if err != nil {
		t.Fatalf("Run 报错: %v", err)
	}
	<-ready
	if e := <-invalidErr; !errors.Is(e, ErrInvalidDeliveryID) {
		t.Errorf("非法 deliveryID got=%v want=ErrInvalidDeliveryID", e)
	}
	if e := <-deliverErr; e != nil {
		t.Errorf("Deliver got=%v want=nil", e)
	}
	if !acked.Load() {
		t.Error("调用方传入的 deliveryID 应出现在 OnDelivery 回调里（回显未被接住）")
	}
	// (b) result 之后会话已收尾
	if !sess.Closed() {
		t.Error("result 之后 Session.Closed() got=false want=true")
	}
	if e := sess.Deliver(context.Background(), "22222222-2222-4222-8222-222222222222", "迟到"); !errors.Is(e, ErrSessionClosed) {
		t.Errorf("result 之后 Deliver got=%v want=ErrSessionClosed", e)
	}

	// (c) 单 result 时与 StreamInput=false 逐字段相等
	plain := fakeClaude(t, fmt.Sprintf("echo '%s'\necho '%s'\n", sysInitLine, resultOKLine))
	want, err := NewDriver(plain, 30*time.Second).Run(context.Background(), RunParams{Prompt: "干活", SessionID: "sess-1"})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%+v", res) != fmt.Sprintf("%+v", want) {
		t.Errorf("单 result 字段不一致：\nstream=%+v\nplain =%+v", res, want)
	}
}

// AC-5d：stdin 不死锁，三条路径各带耗时上限。
func TestStreamInputStdinLifecycle(t *testing.T) {
	cases := []struct {
		name    string
		script  string
		timeout time.Duration
		idle    time.Duration
		limit   time.Duration
		check   func(t *testing.T, res *Result, err error)
	}{
		{
			name:    "不读stdin_sleep1后result退出",
			script:  fmt.Sprintf("sleep 1\necho '%s'\n", resultOKLine),
			timeout: 30 * time.Second, idle: 10 * time.Second, limit: 3 * time.Second,
			check: func(t *testing.T, res *Result, err error) {
				if err != nil || res == nil || !res.Success {
					t.Errorf("got=(%+v,%v) want 成功", res, err)
				}
			},
		},
		{
			name:    "result后cat等EOF",
			script:  fmt.Sprintf("echo '%s'\ncat >/dev/null\n", resultOKLine),
			timeout: 30 * time.Second, idle: 5 * time.Second, limit: 2 * time.Second,
			check: func(t *testing.T, res *Result, err error) {
				if errors.Is(err, ErrIdle) {
					t.Fatalf("应是我们关 stdin 让它退出，而不是看门狗：%v", err)
				}
				if err != nil || res == nil || !res.Success {
					t.Errorf("got=(%+v,%v) want 成功", res, err)
				}
			},
		},
		{
			name:    "sleep30_总超时1s",
			script:  "sleep 30\n",
			timeout: time.Second, idle: 10 * time.Second, limit: 3 * time.Second,
			check: func(t *testing.T, res *Result, err error) {
				if err == nil || !strings.Contains(err.Error(), "执行超时") {
					t.Errorf("got err=%v want 执行超时", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakeClaude(t, tc.script)
			d := &Driver{bin: bin, timeout: tc.timeout, idle: tc.idle}
			start := time.Now()
			res, err := d.Run(context.Background(), RunParams{Prompt: "x", SessionID: "s", StreamInput: true})
			if elapsed := time.Since(start); elapsed > tc.limit {
				t.Errorf("Run 耗时 got=%v want<%v", elapsed, tc.limit)
			}
			tc.check(t, res, err)
		})
	}
}

// AC-6：CLI 不读 stdin、管道写满时，Deliver 在 < 3s 内以 ErrDeliverBackpressure 返回。
func TestDeliverBackpressureDoesNotBlock(t *testing.T) {
	bin := fakeClaude(t, "sleep 20\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		err     error
		elapsed time.Duration
	}
	got := make(chan outcome, 1)
	d := &Driver{bin: bin, timeout: 30 * time.Second, idle: 30 * time.Second}
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = d.Run(ctx, RunParams{
			Prompt: "x", SessionID: "s", StreamInput: true,
			OnReady: func(s *Session) {
				go func() {
					big := strings.Repeat("a", 32<<10)
					for i := 0; i < 64; i++ {
						start := time.Now()
						err := s.Deliver(context.Background(), fmt.Sprintf("33333333-3333-4333-8333-%012d", i), big)
						if err != nil {
							got <- outcome{err, time.Since(start)}
							return
						}
					}
					got <- outcome{nil, 0}
				}()
			},
		})
	}()
	select {
	case o := <-got:
		if !errors.Is(o.err, ErrDeliverBackpressure) {
			t.Errorf("写满后 Deliver got=%v want=ErrDeliverBackpressure", o.err)
		}
		if o.elapsed >= 3*time.Second {
			t.Errorf("Deliver 耗时 got=%v want<3s", o.elapsed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Deliver 一直没返回 —— 投递阻塞了")
	}
	cancel()
	<-runDone
}
