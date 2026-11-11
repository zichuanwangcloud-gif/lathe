package runner

import (
	"context"
	"sync"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// 改动前这几个钩子是空操作；插话机制落地后，红线用例经由它们把真实装配
// 接上 —— 「装配了 Interjections、库里无插话行」时红线必须仍绿。

func wireInterjectionsForBaseline(t *testing.T, p *Pipeline) {
	p.Interjections = newTestInterjections(t, p.Tasks)
}

func assertTriageNotStreaming(t *testing.T, rp agent.RunParams) {
	t.Helper()
	if rp.StreamInput || rp.OnReady != nil || rp.OnDelivery != nil {
		t.Errorf("分诊不得打开流式输入：StreamInput=%v OnReady=%v OnDelivery=%v",
			rp.StreamInput, rp.OnReady != nil, rp.OnDelivery != nil)
	}
}

// interjectDuringImplement 在实现阶段 agent 运行期间登记一个假活会话并插 n 条。
func interjectDuringImplement(t *testing.T, p *Pipeline, ag *fakeAgent, taskID int64, n int) {
	t.Helper()
	ij := newTestInterjections(t, p.Tasks)
	p.Interjections = ij
	prev := ag.mutate[1]
	ag.mutate[1] = func(dir string) error {
		live := &ackSession{ij: ij}
		ij.Register(taskID, live, "implement", "sess")
		for k := 0; k < n; k++ {
			res, err := ij.Submit(context.Background(), taskID, taskOwner(t, p.Tasks, taskID), "别动 migration")
			if err != nil || res.Outcome != OutcomeDelivered {
				t.Errorf("实现期间插话 got=(%v,%v) want delivered", res.Outcome, err)
			}
		}
		if prev != nil {
			return prev(dir)
		}
		return nil
	}
}

func supportsInterjectionsForVerifyCompare() bool { return true }

// ---------------------------------------------------------------- 共用假件

// newTestInterjections 接真实 store（测试库）。
func newTestInterjections(t *testing.T, m *task.Machine) *Interjections {
	t.Helper()
	return &Interjections{Store: testStoreForRunner(t), Tasks: m}
}

func testStoreForRunner(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), dsnForTest())
	if err != nil {
		t.Fatalf("打开 store 失败: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func taskOwner(t *testing.T, m *task.Machine, taskID int64) int64 {
	t.Helper()
	tk, err := m.Get(context.Background(), taskID)
	if err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	return tk.UserID
}

// ackSession 是活会话假件：每次 Deliver 都立刻回显（模拟 CLI 的 isReplay），
// 并记录收到的正文。
type ackSession struct {
	ij     *Interjections
	mu     sync.Mutex
	bodies []string
	closed bool
	err    error // 非 nil 时 Deliver 直接返回它
}

func (s *ackSession) Deliver(ctx context.Context, id, text string) error {
	s.mu.Lock()
	err := s.err
	if err == nil {
		s.bodies = append(s.bodies, text)
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if s.ij != nil {
		s.ij.Ack(id)
	}
	return nil
}

func (s *ackSession) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *ackSession) delivered() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}
