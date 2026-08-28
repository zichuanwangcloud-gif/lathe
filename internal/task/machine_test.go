package task

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testPool 连接测试库；连不上就跳过，让不带数据库的环境仍能跑纯逻辑测试。
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("LATHE_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://lathe:lathe@127.0.0.1:55432/lathe?sslmode=disable"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("跳过数据库测试（连接池创建失败）: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("跳过数据库测试（Ping 失败，先 make dev-infra && make migrate）: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fixture 建一对独立的 user/repo，并在测试结束时连带清掉其任务。
func fixture(t *testing.T, pool *pgxpool.Pool) (userID, repoID int64) {
	t.Helper()
	ctx := context.Background()

	email := "test-" + t.Name() + "@example.com"
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1)
		 ON CONFLICT (email) DO UPDATE SET updated_at = now()
		 RETURNING id`, email).Scan(&userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1, $2)
		 ON CONFLICT (user_id, provider_repo) DO UPDATE SET updated_at = now()
		 RETURNING id`, userID, "Clouditera/CloudRouter").Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}

	t.Cleanup(func() {
		// 级联删除会带走 tasks / task_events / verifications
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})
	return userID, repoID
}

func ptr[T any](v T) *T { return &v }

func TestMachineCreateAndGet(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	created, err := m.Create(ctx, CreateParams{
		UserID: userID, RepoID: repoID,
		LinearIssueKey: "CR-1001", TaskKind: ptr("fix"),
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	if created.State != StateQueued {
		t.Errorf("初始状态 = %s，期望 queued", created.State)
	}
	if created.GateMode != "direct" {
		t.Errorf("GateMode = %q，期望默认 direct", created.GateMode)
	}

	got, err := m.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if got.LinearIssueKey != "CR-1001" {
		t.Errorf("issue key = %q，期望 CR-1001", got.LinearIssueKey)
	}

	if _, err := m.Get(ctx, 999999999); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("Get 不存在的任务应返回 ErrTaskNotFound，得到 %v", err)
	}
}

// F7.1：CreateParams.Profile 设置合法 JSON 时读回来字节内容一致；
// 不设置时（nil）落 schema 默认值 '{}'，不是空字符串/NULL。
func TestMachineCreateProfileRoundtrip(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	withProfile, err := m.Create(ctx, CreateParams{
		UserID: userID, RepoID: repoID,
		LinearIssueKey: "CR-2001",
		Profile:        []byte(`{"model_channel":"channel-x","verify_tier":"light"}`),
	})
	if err != nil {
		t.Fatalf("Create（带 profile）失败: %v", err)
	}
	got, err := m.Get(ctx, withProfile.ID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	// jsonb 落库/读回会规整格式（如冒号后加空格），因此按语义（反序列化
	// 后逐字段比较）而非按字节比较——"原样落库"指内容一致，不是字节流
	// 一致。
	var gotProfile struct {
		ModelChannel string `json:"model_channel"`
		VerifyTier   string `json:"verify_tier"`
	}
	if err := json.Unmarshal(got.Profile, &gotProfile); err != nil {
		t.Fatalf("Profile 反序列化失败: %v（原始字节 %q）", err, string(got.Profile))
	}
	if gotProfile.ModelChannel != "channel-x" || gotProfile.VerifyTier != "light" {
		t.Errorf("Profile = %+v，期望 model_channel=channel-x, verify_tier=light", gotProfile)
	}

	withoutProfile, err := m.Create(ctx, CreateParams{
		UserID: userID, RepoID: repoID,
		LinearIssueKey: "CR-2002",
	})
	if err != nil {
		t.Fatalf("Create（不带 profile）失败: %v", err)
	}
	got2, err := m.Get(ctx, withoutProfile.ID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if string(got2.Profile) != `{}` {
		t.Errorf("未设置 Profile 时应落 schema 默认值 '{}'，得到 %q", string(got2.Profile))
	}
}

// 同一 issue 不允许有两个活任务；终结后可再建（issue 重开场景）。
func TestMachineOneActiveTaskPerIssue(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	first, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-2002"})
	if err != nil {
		t.Fatalf("首个任务应创建成功: %v", err)
	}

	if _, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-2002"}); err == nil {
		t.Fatal("同 issue 的第二个活任务应被拒绝")
	}

	// 终结首个任务后应可再建
	if _, err := m.Transition(ctx, first.ID, StateCancelled, "user:1", nil); err != nil {
		t.Fatalf("取消首个任务失败: %v", err)
	}
	if _, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-2002"}); err != nil {
		t.Errorf("issue 重开后应允许再建任务，却失败: %v", err)
	}
}

func TestMachineTransitionHappyPath(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	tk, err := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-3003"})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	// queued → triaging → implementing → verifying → pr_open → merged
	steps := []struct {
		to   State
		opts *TransitionOpts
	}{
		{StateTriaging, nil},
		{StateImplementing, &TransitionOpts{
			WorktreePath: ptr("/opt/lathe/workspaces/cr-3003"),
			BranchName:   ptr("fix/cr-3003-demo"),
		}},
		{StateVerifying, &TransitionOpts{VerifyTier: ptr("light")}},
		{StatePROpen, &TransitionOpts{PRURL: ptr("https://github.com/x/y/pull/1")}},
		{StateMerged, nil},
	}
	for _, s := range steps {
		if _, err := m.Transition(ctx, tk.ID, s.to, "node:local", s.opts); err != nil {
			t.Fatalf("转移到 %s 失败: %v", s.to, err)
		}
	}

	final, err := m.Get(ctx, tk.ID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if final.State != StateMerged {
		t.Errorf("终态 = %s，期望 merged", final.State)
	}
	// COALESCE 语义：中途写入的字段应保留到最后
	if final.BranchName == nil || *final.BranchName != "fix/cr-3003-demo" {
		t.Errorf("branch_name 应被保留，得到 %v", final.BranchName)
	}
	if final.PRURL == nil {
		t.Error("pr_url 应被保留")
	}

	// 事件流应能重放出与表内一致的状态
	replayed, err := m.Replay(ctx, tk.ID)
	if err != nil {
		t.Fatalf("Replay 失败: %v", err)
	}
	if replayed != final.State {
		t.Errorf("Replay 得到 %s，但表内是 %s —— 事件流与状态不一致", replayed, final.State)
	}

	events, err := m.Events(ctx, tk.ID)
	if err != nil {
		t.Fatalf("Events 失败: %v", err)
	}
	if len(events) != len(steps)+1 { // +1 是创建事件
		t.Errorf("事件数 = %d，期望 %d", len(events), len(steps)+1)
	}
	if events[0].FromState != nil {
		t.Error("首个事件应是创建（from_state 为 NULL）")
	}
}

// 非法转移必须被拒绝，且不产生任何写入（状态与事件都不变）。
func TestMachineIllegalTransitionIsAtomic(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	tk, _ := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-4004"})

	before, _ := m.Events(ctx, tk.ID)

	// queued → pr_open 绕过实现与验证，必须拒绝
	_, err := m.Transition(ctx, tk.ID, StatePROpen, "system", &TransitionOpts{
		PRURL: ptr("https://evil/pull/1"),
	})
	if err == nil {
		t.Fatal("queued → pr_open 应被拒绝")
	}
	var illegal ErrIllegalTransition
	if !errors.As(err, &illegal) {
		t.Errorf("错误类型应为 ErrIllegalTransition，得到 %T: %v", err, err)
	}

	after, _ := m.Events(ctx, tk.ID)
	if len(after) != len(before) {
		t.Errorf("被拒绝的转移不应写事件：事件数从 %d 变成 %d", len(before), len(after))
	}
	cur, _ := m.Get(ctx, tk.ID)
	if cur.State != StateQueued {
		t.Errorf("被拒绝后状态应仍为 queued，得到 %s", cur.State)
	}
	if cur.PRURL != nil {
		t.Errorf("被拒绝的转移不应写入 pr_url，得到 %v", *cur.PRURL)
	}
}

// review 二轮必须已持有 agent_session_id。
func TestMachineReviewFeedbackRequiresSession(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	tk, _ := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-5005"})
	for _, s := range []State{StateTriaging, StateImplementing, StateVerifying, StatePROpen, StateReviewFeedback} {
		if _, err := m.Transition(ctx, tk.ID, s, "system", nil); err != nil {
			t.Fatalf("转移到 %s 失败: %v", s, err)
		}
	}

	// 没有会话 ID，二轮应被拒绝
	if _, err := m.Transition(ctx, tk.ID, StateImplementing, "system", nil); !errors.Is(err, ErrSessionRequired) {
		t.Fatalf("无会话时二轮应返回 ErrSessionRequired，得到 %v", err)
	}

	// 带上会话 ID 即可放行
	if _, err := m.Transition(ctx, tk.ID, StateImplementing, "system", &TransitionOpts{
		AgentSessionID: ptr("sess-abc123"),
	}); err != nil {
		t.Fatalf("带会话 ID 的二轮应成功: %v", err)
	}

	got, _ := m.Get(ctx, tk.ID)
	if got.AgentSessionID == nil || *got.AgentSessionID != "sess-abc123" {
		t.Errorf("agent_session_id 应被持久化，得到 %v", got.AgentSessionID)
	}
}

// 并发转移：行锁必须保证恰好一个成功，另一个因状态已变而被拒绝。
func TestMachineConcurrentTransitionExactlyOneWins(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	tk, _ := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-6006"})

	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		okCount int
		errs    []error
	)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			_, err := m.Transition(ctx, tk.ID, StateTriaging, "system", nil)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				okCount++
			} else {
				errs = append(errs, err)
			}
		}()
	}
	wg.Wait()

	if okCount != 1 {
		t.Errorf("并发 %d 次 queued→triaging，成功次数 = %d，期望恰好 1", racers, okCount)
		for _, e := range errs {
			t.Logf("  失败: %v", e)
		}
	}

	events, _ := m.Events(ctx, tk.ID)
	if len(events) != 2 { // 创建 + 一次成功转移
		t.Errorf("事件数 = %d，期望 2（创建 + 唯一一次成功转移）", len(events))
	}

	replayed, err := m.Replay(ctx, tk.ID)
	if err != nil {
		t.Fatalf("并发后事件流应仍可重放: %v", err)
	}
	if replayed != StateTriaging {
		t.Errorf("Replay = %s，期望 triaging", replayed)
	}
}

// 节点崩溃场景：租约到期后 implementing 可以回到 queued 重新派发。
func TestMachineLeaseExpiryRedispatch(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	tk, _ := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-7007"})
	_, _ = m.Transition(ctx, tk.ID, StateTriaging, "system", nil)

	lease := time.Now().Add(-time.Minute) // 已过期
	if _, err := m.Transition(ctx, tk.ID, StateImplementing, "node:crashed", &TransitionOpts{
		LeaseExpiresAt: &lease,
	}); err != nil {
		t.Fatalf("转到 implementing 失败: %v", err)
	}

	if _, err := m.Transition(ctx, tk.ID, StateQueued, "system", &TransitionOpts{
		Payload: map[string]any{"reason": "lease_expired"},
	}); err != nil {
		t.Fatalf("租约到期后应能回到 queued 重新派发: %v", err)
	}

	got, _ := m.Get(ctx, tk.ID)
	if got.State != StateQueued {
		t.Errorf("状态 = %s，期望回到 queued", got.State)
	}

	events, _ := m.Events(ctx, tk.ID)
	last := events[len(events)-1]
	if last.Payload["reason"] != "lease_expired" {
		t.Errorf("事件 payload 应记录重派原因，得到 %v", last.Payload)
	}
}

// toPROpen 把任务从 queued 一路转到 pr_open，供合并检测相关测试复用。
func toPROpen(t *testing.T, m *Machine, id int64) *Task {
	t.Helper()
	ctx := context.Background()
	if _, err := m.Transition(ctx, id, StateImplementing, "system", nil); err != nil {
		t.Fatalf("转到 implementing 失败: %v", err)
	}
	if _, err := m.Transition(ctx, id, StateVerifying, "system", nil); err != nil {
		t.Fatalf("转到 verifying 失败: %v", err)
	}
	tk, err := m.Transition(ctx, id, StatePROpen, "system", nil)
	if err != nil {
		t.Fatalf("转到 pr_open 失败: %v", err)
	}
	return tk
}

// F4.1：ListOpenPRTasks 只返回真正 pr_open 且已记下 pr_number 的行。
func TestMachineListOpenPRTasks(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	withPR, _ := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-8001"})
	toPROpen(t, m, withPR.ID)
	if err := m.SetPRNumber(ctx, withPR.ID, 101); err != nil {
		t.Fatalf("SetPRNumber 失败: %v", err)
	}

	withoutPR, _ := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-8002"})
	toPROpen(t, m, withoutPR.ID)
	// 故意不调 SetPRNumber：模拟 pr_number 尚未落库的窗口期

	stillQueued, _ := m.Create(ctx, CreateParams{UserID: userID, RepoID: repoID, LinearIssueKey: "CR-8003"})
	if err := m.SetPRNumber(ctx, stillQueued.ID, 103); err != nil {
		// pr_number 与状态无关联，此处仅验证 SetPRNumber 本身不会因状态而拒绝
		t.Fatalf("SetPRNumber 失败: %v", err)
	}

	tasks, err := m.ListOpenPRTasks(ctx)
	if err != nil {
		t.Fatalf("ListOpenPRTasks 失败: %v", err)
	}
	var got []int64
	for _, tk := range tasks {
		if tk.UserID == userID {
			got = append(got, tk.ID)
		}
	}
	if len(got) != 1 || got[0] != withPR.ID {
		t.Errorf("ListOpenPRTasks（本用户范围）= %v，期望只含 %d", got, withPR.ID)
	}
}

func TestMachineSetPRNumberNotFound(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	ctx := context.Background()

	if err := m.SetPRNumber(ctx, -1, 1); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("SetPRNumber 对不存在的任务应返回 ErrTaskNotFound，得到 %v", err)
	}
}

// F4.2-AC2：HasLiveDependentOnBranch 按"当前 base_ref == 分支名 且状态非终结"判定，
// 不看 depends_on 结构 —— 终结状态的任务不算"活的依赖"。
func TestMachineHasLiveDependentOnBranch(t *testing.T) {
	pool := testPool(t)
	m := NewMachine(pool)
	userID, repoID := fixture(t, pool)
	ctx := context.Background()

	branch := "lathe/cr-9001-fix"
	live, err := m.Create(ctx, CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "CR-9002",
		BaseRef: ptr(branch),
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	has, err := m.HasLiveDependentOnBranch(ctx, branch)
	if err != nil {
		t.Fatalf("HasLiveDependentOnBranch 失败: %v", err)
	}
	if !has {
		t.Errorf("非终结状态且 base_ref 匹配的任务应视为活依赖，得到 false")
	}

	// 转到终结状态（cancelled）后不再算活依赖
	if _, err := m.Transition(ctx, live.ID, StateCancelled, "system", nil); err != nil {
		t.Fatalf("转到 cancelled 失败: %v", err)
	}
	has, err = m.HasLiveDependentOnBranch(ctx, branch)
	if err != nil {
		t.Fatalf("HasLiveDependentOnBranch 失败: %v", err)
	}
	if has {
		t.Errorf("终结状态的任务不该算活依赖，得到 true")
	}

	noMatch, err := m.HasLiveDependentOnBranch(ctx, "lathe/no-such-branch")
	if err != nil {
		t.Fatalf("HasLiveDependentOnBranch 失败: %v", err)
	}
	if noMatch {
		t.Errorf("无匹配 base_ref 时应返回 false")
	}
}
