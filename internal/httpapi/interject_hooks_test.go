package httpapi

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/runner"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

func uniqueIssueKey(prefix string) string {
	return prefix + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// wireInterjectorForOwnerTest 装上真实的 runner.Interjections（测试库）。
func wireInterjectorForOwnerTest(t *testing.T, api *API) *runner.Interjections {
	t.Helper()
	ij := &runner.Interjections{Store: api.Store, Tasks: api.Tasks}
	api.Interjector = ij
	return ij
}

func assertNoInterjectionRows(t *testing.T, st *store.Store, id int64) {
	t.Helper()
	if n := countInterjections(t, st, id); n != 0 {
		t.Errorf("库里插话行 got=%d want=0", n)
	}
}

func countInterjections(t *testing.T, st *store.Store, taskID int64) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM task_interjections WHERE task_id=$1`, taskID).Scan(&n); err != nil {
		t.Fatalf("数插话行失败: %v", err)
	}
	return n
}

type ijDBRow struct {
	status, err, phase, deliveryID string
}

func lastInterjection(t *testing.T, st *store.Store, taskID int64) ijDBRow {
	t.Helper()
	var r ijDBRow
	if err := st.Pool().QueryRow(context.Background(), `
		SELECT status, coalesce(error,''), coalesce(phase,''), coalesce(delivery_id::text,'')
		FROM task_interjections WHERE task_id=$1 ORDER BY id DESC LIMIT 1`, taskID).
		Scan(&r.status, &r.err, &r.phase, &r.deliveryID); err != nil {
		t.Fatalf("读插话行失败: %v", err)
	}
	return r
}

// ijTask 建一个属于 api 当前用户的任务，并把它摆到 state。
func ijTask(t *testing.T, st *store.Store, m *task.Machine, repoID int64, state string) *task.Task {
	t.Helper()
	ctx := context.Background()
	var userID int64
	if err := st.Pool().QueryRow(ctx, `SELECT user_id FROM repos WHERE id=$1`, repoID).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	tk, err := m.Create(ctx, task.CreateParams{UserID: userID, RepoID: repoID, ExternalKey: uniqueIssueKey("CR-IJ")})
	if err != nil {
		t.Fatal(err)
	}
	if state != "" && state != string(task.StateQueued) {
		if _, err := st.Pool().Exec(ctx, `UPDATE tasks SET state=$2 WHERE id=$1`, tk.ID, state); err != nil {
			t.Fatal(err)
		}
	}
	return tk
}

// fakeLive 是活会话假件：ack 为真时立刻回显；err 非 nil 时 Deliver 返回它；
// gate 非 nil 时 Deliver 先阻塞在 gate 上。
type fakeLive struct {
	ij     *runner.Interjections
	ack    bool
	err    error
	closed bool

	mu     sync.Mutex
	calls  int
	bodies []string
}

func (f *fakeLive) Deliver(ctx context.Context, id, text string) error {
	f.mu.Lock()
	f.calls++
	if f.err == nil {
		f.bodies = append(f.bodies, text)
	}
	f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if f.ack {
		f.ij.Ack(id)
	}
	return nil
}

func (f *fakeLive) Closed() bool { return f.closed }

func (f *fakeLive) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}
