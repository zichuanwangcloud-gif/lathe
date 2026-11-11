package runner

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// goid 取当前 goroutine 的编号（只用于测试断言「写库发生在哪条协程上」）。
func goid() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	f := strings.Fields(strings.TrimPrefix(string(buf[:n]), "goroutine "))
	id, _ := strconv.ParseInt(f[0], 10, 64)
	return id
}

// slowStore 是 AC-6b 的假 store：只有 MarkInterjectionDelivered 慢（3s）——
// 那正是「在读协程里同步写库」的错误实现会调到的一步。每次调用都比对
// OnDelivery 回调所在的协程编号，命中即说明写库发生在读协程上。
type slowStore struct {
	InterjectStore // 未覆盖的方法不该被调到（nil 接口会 panic，正好暴露）
	readerGoid     *atomic.Int64
	t              *testing.T

	mu     sync.Mutex
	nextID int64
	rows   map[int64]*store.Interjection
}

func (s *slowStore) checkGoroutine(op string) {
	if id := s.readerGoid.Load(); id != 0 && id == goid() {
		s.t.Errorf("写库发生在读协程上（%s）", op)
	}
}

func (s *slowStore) InsertInterjectionGated(ctx context.Context, taskID, userID int64, body string, gate func(string) string) (*store.Interjection, string, error) {
	s.checkGoroutine("insert")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	r := &store.Interjection{ID: s.nextID, TaskID: taskID, Status: store.InterjectionQueued, Body: body}
	s.rows[r.ID] = r
	return r, "implementing", nil
}

func (s *slowStore) MarkInterjectionDelivering(ctx context.Context, taskID, id int64, deliveryID, phase, sessionID string) (*store.Interjection, error) {
	s.checkGoroutine("delivering")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[id].Status = store.InterjectionDelivering
	cp := *s.rows[id]
	return &cp, nil
}

func (s *slowStore) MarkInterjectionDelivered(ctx context.Context, taskID, id int64, errText string) (*store.Interjection, error) {
	s.checkGoroutine("delivered")
	time.Sleep(3 * time.Second)
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rows[id]
	r.Status = store.InterjectionDelivered
	if errText != "" {
		r.Error = &errText
	}
	cp := *r
	return &cp, nil
}

// MarkInterjectionReplayAcked 是迟到回显的写（CI 慢到 2s 内没等到回显时，
// 「立即确认」分支或 recordLateAck 会调到）。语义同 store：只清
// delivered + no_replay_ack 的行，其余返回 nil。
func (s *slowStore) MarkInterjectionReplayAcked(ctx context.Context, taskID, id int64, deliveryID string) (*store.Interjection, error) {
	s.checkGoroutine("replay_acked")
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.rows[id]
	if r == nil || r.Status != store.InterjectionDelivered || r.Error == nil || *r.Error != store.InterjectionNoReplayAck {
		return nil, nil
	}
	r.Error = nil
	cp := *r
	return &cp, nil
}

// fakeStreamCLI 写一个假 claude：读首帧；之后每读到一帧注入就按
// --replay-user-messages 的形状回显；读满 n 帧注入后输出 50 行 assistant、
// 一条 result，然后一直等 EOF。可选把收到的帧原文追加到 framesOut。
func fakeStreamCLI(t *testing.T, n int, framesOut string) string {
	t.Helper()
	record := ""
	if framesOut != "" {
		record = fmt.Sprintf(`printf '%%s\n' "$line" >> %q`, framesOut)
	}
	script := fmt.Sprintf(`#!/bin/bash
read -r line
%[2]s
for i in $(seq 1 %[1]d); do
  read -r line
  %[2]s
  uid=$(printf '%%s' "$line" | grep -o '"uuid":"[^"]*"' | head -1 | cut -d'"' -f4)
  echo "{\"type\":\"user\",\"isReplay\":true,\"uuid\":\"$uid\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"echo\"}]}}"
done
for i in $(seq 1 50); do
  echo '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working"}]},"session_id":"s"}'
done
echo '{"type":"result","subtype":"success","is_error":false,"session_id":"s","result":"ok","num_turns":1,"total_cost_usd":0.01,"duration_ms":10}'
cat >/dev/null
`, n, record)
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// AC-6b：慢 DB 不拖慢读协程（G4）。
func TestSlowStoreDoesNotSlowReadGoroutine(t *testing.T) {
	var readerGoid atomic.Int64
	st := &slowStore{readerGoid: &readerGoid, t: t, rows: map[int64]*store.Interjection{}}
	ij := &Interjections{Store: st}
	const taskID = 4242

	var acks atomic.Int32
	var t3, tr atomic.Int64
	var wg sync.WaitGroup
	results := make([]SubmitResult, 3)

	d := agent.NewDriver(fakeStreamCLI(t, 3, ""), 30*time.Second)
	_, err := d.Run(context.Background(), agent.RunParams{
		Prompt: "x", SessionID: "s", StreamInput: true,
		OnReady: func(s *agent.Session) {
			ij.Register(taskID, s, "implement", "s")
			for k := 0; k < 3; k++ {
				wg.Add(1)
				go func(k int) {
					defer wg.Done()
					results[k], _ = ij.Submit(context.Background(), taskID, 1, fmt.Sprintf("插话 %d", k))
				}(k)
			}
		},
		OnDelivery: func(id string) {
			readerGoid.Store(goid())
			ij.Ack(id)
			if acks.Add(1) == 3 {
				t3.Store(time.Now().UnixNano())
			}
		},
		OnEvent: func(e agent.Event) {
			if e.Type == agent.EventResult {
				tr.Store(time.Now().UnixNano())
			}
		},
	})
	ij.Unregister(taskID)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if t3.Load() == 0 || tr.Load() == 0 {
		t.Fatalf("应收到 3 条回显与 result：acks=%d tr=%d", acks.Load(), tr.Load())
	}
	if gap := time.Duration(tr.Load() - t3.Load()); gap >= time.Second {
		t.Errorf("TR−T3 got=%v want<1s（读协程被下游写库拖慢）", gap)
	}

	waited := make(chan struct{})
	go func() { wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("3 个 Submit 10s 内没有全部返回")
	}
	for k, r := range results {
		if r.Outcome != OutcomeDelivered || r.Row == nil || r.Row.Status != store.InterjectionDelivered || r.Row.Error != nil {
			t.Errorf("第 %d 条 got=(%v,%+v) want delivered 且 error 为空（回显已到）", k, r.Outcome, r.Row)
		}
	}
}

// AC-13：单一脱敏点 —— 库里 body、活会话写进 stdin 的帧、下一次 runAgent 的
// prompt、请求期间的 slog 输出，都不含任一原文、都含 [REDACTED]。
// （GET /interjections 响应那一处出口在 internal/httpapi 的同名用例里断言。）
func TestInterjectRedactsAtSingleSubmitPoint(t *testing.T) {
	ghp := "ghp_" + strings.Repeat("A1b2", 9)
	ant := "sk-ant-" + strings.Repeat("x9", 10)
	bearer := strings.Repeat("Zq8", 7)[:20]
	known := "kn0wn-S3cret-" + randTag("v")
	msg := fmt.Sprintf("别动 migration；token %s key %s Authorization: Bearer %s 口令 %s", ghp, ant, bearer, known)
	secrets := []string{ghp, ant, bearer, known}
	assertClean := func(t *testing.T, where, s string) {
		t.Helper()
		for _, sec := range secrets {
			if strings.Contains(s, sec) {
				t.Errorf("%s 含明文 %q：%q", where, sec, s)
			}
		}
		if !strings.Contains(s, "[REDACTED]") {
			t.Errorf("%s 应含 [REDACTED]：%q", where, s)
		}
	}

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	t.Run("活会话路径_stdin帧", func(t *testing.T) {
		pool, m, taskID, _, _ := pipelineFixture(t)
		setTaskState(t, pool, taskID, "implementing")
		ij := newTestInterjections(t, m)
		ij.KnownSecrets = []string{known}
		frames := filepath.Join(t.TempDir(), "frames.ndjson")
		d := agent.NewDriver(fakeStreamCLI(t, 1, frames), 30*time.Second)
		resCh := make(chan SubmitResult, 1)
		_, err := d.Run(context.Background(), agent.RunParams{
			Prompt: "x", SessionID: "s", StreamInput: true,
			OnReady: func(s *agent.Session) {
				ij.Register(taskID, s, "implement", "s")
				uid := taskOwner(t, m, taskID)
				go func() { r, _ := ij.Submit(context.Background(), taskID, uid, msg); resCh <- r }()
			},
			OnDelivery: ij.Ack,
		})
		ij.Unregister(taskID)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case res := <-resCh:
			if res.Outcome != OutcomeDelivered {
				t.Errorf("活会话路径 got=%v want delivered", res.Outcome)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Submit 5s 内没返回")
		}
		raw, _ := os.ReadFile(frames)
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		if len(lines) < 2 {
			t.Fatalf("假 CLI 应收到首帧 + 1 帧注入，got=%d", len(lines))
		}
		assertClean(t, "③ stdin 帧", lines[1])
		rows := ijRows(t, pool, taskID)
		if len(rows) != 1 {
			t.Fatalf("got %d 行", len(rows))
		}
		assertClean(t, "① 库里 body", rows[0].body)
		listed, _ := ij.ListSince(context.Background(), taskID, 0, 200)
		assertClean(t, "② ListSince 返回的 body", listed[0].Body)
	})

	t.Run("无会话路径_补投prompt", func(t *testing.T) {
		pool, m, taskID, _, _ := pipelineFixture(t)
		setTaskState(t, pool, taskID, "verifying")
		ij := newTestInterjections(t, m)
		ij.KnownSecrets = []string{known}
		r, err := ij.Submit(context.Background(), taskID, taskOwner(t, m, taskID), msg)
		if err != nil || r.Outcome != OutcomeQueued {
			t.Fatalf("got=(%v,%v)", r.Outcome, err)
		}
		ag := &fakeAgent{results: []*agent.Result{{Success: true, NumTurns: 1}}}
		p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, &fakeGitHub{}, ag, &fakeNotifier{})
		p.Interjections = ij
		tk, _ := m.Get(context.Background(), taskID)
		_, _ = p.runAgent(&runCtx{ctx: context.Background(), tk: tk, wt: &Worktree{Path: t.TempDir()}}, "fix-1", "FIX", "s", true)
		assertClean(t, "④ 补投 prompt", ag.calls[0].Prompt)
	})

	t.Run("slog输出", func(t *testing.T) {
		out := logBuf.String()
		for _, sec := range secrets {
			if strings.Contains(out, sec) {
				t.Errorf("⑤ slog 输出含明文 %q", sec)
			}
		}
		if !strings.Contains(out, "收到插话") {
			t.Errorf("夹具没走到 Submit 的日志（slog 输出：%q）", out)
		}
	})
}

// ackCountStore 只实现迟到回显那一步写，按行计数（其它方法不该被调到）。
type ackCountStore struct {
	InterjectStore
	mu     sync.Mutex
	writes map[int64]int
}

func (s *ackCountStore) MarkInterjectionReplayAcked(ctx context.Context, taskID, id int64, deliveryID string) (*store.Interjection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes[id]++
	return &store.Interjection{ID: id, TaskID: taskID, Status: store.InterjectionDelivered}, nil
}

func (s *ackCountStore) count(id int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes[id]
}

// 迟到回显的内存表在并发下的不变式（-race 下跑）。给定 N 组已登记「等迟到
// 回显」的投递：一半组只有 3 个并发 Ack（同一 deliveryID 重复回显），另一半
// 组 3 个 Ack 与 1 个 Unregister 并发。断言：结束后 late 表为空；同一行的
// 迟到写至多一次；只有 Ack 的组恰好写一次（登记没被弄丢）。
func TestInterjectLateAckConcurrentUnregister(t *testing.T) {
	const groups = 64
	const acksPerGroup = 3
	st := &ackCountStore{writes: map[int64]int{}}
	ij := &Interjections{Store: st}

	type group struct {
		taskID, rowID int64
		deliveryID    string
		unregister    bool
	}
	gs := make([]group, groups)
	for k := range gs {
		g := group{taskID: int64(k + 1), rowID: int64(1000 + k), deliveryID: fmt.Sprintf("late-%d", k), unregister: k%2 == 1}
		gs[k] = g
		ij.Register(g.taskID, &ackSession{}, "fix-1", "s")
		ij.mu.Lock()
		entry := ij.live[g.taskID]
		ij.mu.Unlock()
		if ij.awaitLateAck(g.deliveryID, make(chan struct{}, 1), lateAck{taskID: g.taskID, rowID: g.rowID, entry: entry}) {
			t.Fatalf("第 %d 组：空 channel 不该当场确认", k)
		}
	}
	ij.mu.Lock()
	if n := len(ij.late); n != groups {
		ij.mu.Unlock()
		t.Fatalf("登记后 late 表 got=%d want=%d", n, groups)
	}
	ij.mu.Unlock()

	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, g := range gs {
		for a := 0; a < acksPerGroup; a++ {
			wg.Add(1)
			go func(id string) { defer wg.Done(); <-start; ij.Ack(id) }(g.deliveryID)
		}
		if g.unregister {
			wg.Add(1)
			go func(taskID int64) { defer wg.Done(); <-start; ij.Unregister(taskID) }(g.taskID)
		}
	}
	close(start)
	wg.Wait()

	ij.mu.Lock()
	left := len(ij.late)
	ij.mu.Unlock()
	if left != 0 {
		t.Errorf("并发 Ack/Unregister 之后 late 表 got=%d want=0", left)
	}

	// 写库在 Ack 起的协程里：等只有 Ack 的组都写到，再多等一会儿让多余的写（若有）现形。
	deadline := time.Now().Add(5 * time.Second)
	for _, g := range gs {
		for !g.unregister && st.count(g.rowID) == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	time.Sleep(300 * time.Millisecond)
	for k, g := range gs {
		got := st.count(g.rowID)
		if got > 1 {
			t.Errorf("第 %d 组（unregister=%v）同一行迟到写 got=%d want≤1", k, g.unregister, got)
		}
		if !g.unregister && got != 1 {
			t.Errorf("第 %d 组（只有 Ack）迟到写 got=%d want=1", k, got)
		}
	}
}
