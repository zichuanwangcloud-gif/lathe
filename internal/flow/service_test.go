package flow

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Clouditera/lathe/internal/store"
	"github.com/Clouditera/lathe/internal/task"
)

// testPool 连接测试库；连不上就跳过（同 internal/task/machine_test.go 的手法）。
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

// testStore 打开一个 *store.Store（与 testPool 连同一个测试库，但走
// store.Open——同一手法见 internal/httpapi/testhelper_test.go 的
// testStoreForAPI），供需要读写 system_settings 的测试用（如
// flow_max_chain_length，F3.3-AC2）。连不上就跳过，理由同 testPool。
func testStore(t *testing.T) *store.Store {
	t.Helper()

	dsn := os.Getenv("LATHE_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://lathe:lathe@127.0.0.1:55432/lathe?sslmode=disable"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Skipf("跳过数据库测试（连接池创建失败）: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// testConcurrentPool 跟 testPool 一样连测试库，只是把连接池上限调高。
//
// acquireSubmissionLock 会在整个 CreateFlow 期间独占一条连接（专门用来
// 持有咨询锁），并发测试里同一时刻有多个 CreateFlow 调用在跑：没拿到
// 锁的那些会各自占着一条连接阻塞在"等锁"上，拿到锁的那个还要另外的
// 连接去查重、建 flow、建 tasks——用 testPool 默认的连接池上限，容易
// 出现"所有连接都在等锁，没有连接可用来把持锁的那个请求做完事情从而
// 释放锁"的连接池级死锁。这里显式调大 MaxConns 把这个人为限制排除掉，
// 让测试断言的是咨询锁本身的正确性，不是连接池容量。
func testConcurrentPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("LATHE_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://lathe:lathe@127.0.0.1:55432/lathe?sslmode=disable"
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Skipf("跳过数据库测试（解析 DSN 失败）: %v", err)
	}
	cfg.MaxConns = 40

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
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

// fixture 建一对独立的 user/repo，测试结束一并清掉其名下数据。
//
// tag 用来在同一个测试函数内区分多个 fixture（比如需要"另一个用户"
// 做隔离断言时）——不传时默认用 t.Name()，传了就用 tag，两次调用
// 若都不传会撞上同一个 email 并通过 ON CONFLICT 拿回同一个 userID，
// 这不是我们要的"两个独立用户"。
func fixture(t *testing.T, pool *pgxpool.Pool, tag ...string) (userID, repoID int64) {
	t.Helper()
	ctx := context.Background()

	name := t.Name()
	if len(tag) > 0 {
		name = t.Name() + "-" + tag[0]
	}

	email := "flow-test-" + name + "@example.com"
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1)
		 ON CONFLICT (email) DO UPDATE SET updated_at = now()
		 RETURNING id`, email).Scan(&userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1, $2)
		 ON CONFLICT (user_id, provider_repo) DO UPDATE SET updated_at = now()
		 RETURNING id`, userID, "acme/flow-"+name).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})
	return userID, repoID
}

func p(i int) *int { return &i }

// TestServiceCreateFlowGraph1To2To3Plus4Plus5To6 覆盖 M1 出口条件的图形状：
// 用 API 建一张 1→2→3 / 4 / 5→6 的图，断言 5 个独立根/非根的依赖关系
// 与状态都对。
func TestServiceCreateFlowGraph1To2To3Plus4Plus5To6(t *testing.T) {
	pool := testPool(t)
	userID, repoID := fixture(t, pool)
	svc := &Service{Pool: pool, Tasks: task.NewMachine(pool)}

	nodes := []NodeInput{
		{IssueKey: "T-1-" + t.Name()},
		{IssueKey: "T-2-" + t.Name(), DependsOnIndex: p(0)},
		{IssueKey: "T-3-" + t.Name(), DependsOnIndex: p(1)},
		{IssueKey: "T-4-" + t.Name()},
		{IssueKey: "T-5-" + t.Name()},
		{IssueKey: "T-6-" + t.Name(), DependsOnIndex: p(4)},
	}

	flowID, created, _, err := svc.CreateFlow(context.Background(), userID, repoID, "graph-"+t.Name(), nodes)
	if err != nil {
		t.Fatalf("建图应成功，得到 %v", err)
	}
	if flowID == 0 {
		t.Fatal("应返回非零 flowID")
	}
	if len(created) != 6 {
		t.Fatalf("应建出 6 个任务，得到 %d", len(created))
	}

	roots := map[int]bool{0: true, 3: true, 4: true}
	for i, tk := range created {
		if tk.State != task.StateQueued {
			t.Errorf("第 %d 个任务状态应为 queued，得到 %s", i, tk.State)
		}
		if tk.FlowID == nil || *tk.FlowID != flowID {
			t.Errorf("第 %d 个任务的 flow_id 应为 %d，得到 %v", i, flowID, tk.FlowID)
		}
		if roots[i] {
			if tk.DependsOn != nil {
				t.Errorf("第 %d 个任务应是独立根，depends_on 应为空，得到 %v", i, *tk.DependsOn)
			}
			continue
		}
		if tk.DependsOn == nil {
			t.Errorf("第 %d 个任务应有前驱，得到 nil", i)
			continue
		}
		wantPredIdx := *nodes[i].DependsOnIndex
		if *tk.DependsOn != created[wantPredIdx].ID {
			t.Errorf("第 %d 个任务的前驱应是第 %d 个任务(id=%d)，得到 %d",
				i, wantPredIdx, created[wantPredIdx].ID, *tk.DependsOn)
		}
	}
}

func TestServiceCreateFlowRejectsInvalidIndex(t *testing.T) {
	pool := testPool(t)
	userID, repoID := fixture(t, pool)
	svc := &Service{Pool: pool, Tasks: task.NewMachine(pool)}

	nodes := []NodeInput{
		{IssueKey: "T-a-" + t.Name(), DependsOnIndex: p(0)}, // 指向自己
	}
	_, _, _, err := svc.CreateFlow(context.Background(), userID, repoID, "bad-"+t.Name(), nodes)
	var invalid ErrInvalidIndex
	if !errors.As(err, &invalid) {
		t.Fatalf("非法下标应拒绝并返回 ErrInvalidIndex，得到 %v", err)
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM flows WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("校验失败不应创建任何 flow 行，得到 %d 个", n)
	}
}

// TestServiceCreateFlowIdempotentDuplicateSubmission 覆盖 F1.4-AC2：
// 同一批次重复提交两次，不应产生第二个 flow。
func TestServiceCreateFlowIdempotentDuplicateSubmission(t *testing.T) {
	pool := testPool(t)
	userID, repoID := fixture(t, pool)
	svc := &Service{Pool: pool, Tasks: task.NewMachine(pool)}

	nodes := []NodeInput{
		{IssueKey: "T-dup-1-" + t.Name()},
		{IssueKey: "T-dup-2-" + t.Name(), DependsOnIndex: p(0)},
	}

	flowID1, created1, _, err := svc.CreateFlow(context.Background(), userID, repoID, "dup-"+t.Name(), nodes)
	if err != nil {
		t.Fatalf("第一次提交应成功，得到 %v", err)
	}

	// 人手抖点了两次：同样的 nodes 再提交一次
	flowID2, created2, _, err := svc.CreateFlow(context.Background(), userID, repoID, "dup-"+t.Name(), nodes)
	if err != nil {
		t.Fatalf("重复提交应被当成幂等成功处理，得到错误 %v", err)
	}
	if flowID2 != flowID1 {
		t.Errorf("重复提交应返回同一个 flowID，得到 %d 与 %d", flowID1, flowID2)
	}
	if len(created2) != len(created1) {
		t.Fatalf("重复提交返回的任务数应与首次相同，得到 %d 与 %d", len(created2), len(created1))
	}
	for i := range created1 {
		if created1[i].ID != created2[i].ID {
			t.Errorf("第 %d 个任务应是同一行，得到 id %d 与 %d", i, created1[i].ID, created2[i].ID)
		}
	}

	var flowCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM flows WHERE user_id = $1`, userID).Scan(&flowCount); err != nil {
		t.Fatal(err)
	}
	if flowCount != 1 {
		t.Errorf("重复提交不应产生第二个 flow，得到 %d 个", flowCount)
	}

	var taskCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tasks WHERE flow_id = $1`, flowID1).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if taskCount != 2 {
		t.Errorf("重复提交不应产生额外任务行，得到 %d 个", taskCount)
	}
}

// TestServiceCreateFlowRejectsWhenIssueActiveOutsideFlow 覆盖 F1.1-AC3：
// issue 已经在一个非 flow 的活任务里占用着，批量建图应拒绝且不建任何行。
func TestServiceCreateFlowRejectsWhenIssueActiveOutsideFlow(t *testing.T) {
	pool := testPool(t)
	userID, repoID := fixture(t, pool)
	m := task.NewMachine(pool)
	svc := &Service{Pool: pool, Tasks: m}

	issueKey := "T-taken-" + t.Name()
	if _, err := m.Create(context.Background(), task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: issueKey,
	}); err != nil {
		t.Fatalf("预置活任务失败: %v", err)
	}

	nodes := []NodeInput{{IssueKey: issueKey}}
	_, _, _, err := svc.CreateFlow(context.Background(), userID, repoID, "taken-"+t.Name(), nodes)
	var conflict ErrIssueActive
	if !errors.As(err, &conflict) {
		t.Fatalf("应拒绝并返回 ErrIssueActive，得到 %v", err)
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM flows WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("冲突时不应创建任何 flow 行，得到 %d 个", n)
	}
}

// TestServiceCreateFlowCompensatesOnMidBatchFailure 覆盖批次中途失败的
// 补偿式回滚：第 0 个节点建成功后，第 1 个节点因 issue 已被别处占用而
// 失败，第 0 个节点应被转为 cancelled，而不是留在 queued 里悬空跑下去。
func TestServiceCreateFlowCompensatesOnMidBatchFailure(t *testing.T) {
	pool := testPool(t)
	userID, repoID := fixture(t, pool)
	m := task.NewMachine(pool)
	svc := &Service{Pool: pool, Tasks: m}

	takenKey := "T-mid-taken-" + t.Name()
	if _, err := m.Create(context.Background(), task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: takenKey,
	}); err != nil {
		t.Fatalf("预置活任务失败: %v", err)
	}

	nodes := []NodeInput{
		{IssueKey: "T-mid-ok-" + t.Name()},
		{IssueKey: takenKey, DependsOnIndex: p(0)},
	}
	_, _, _, err := svc.CreateFlow(context.Background(), userID, repoID, "mid-"+t.Name(), nodes)
	var conflict ErrIssueActive
	if !errors.As(err, &conflict) {
		t.Fatalf("应返回 ErrIssueActive，得到 %v", err)
	}

	var state string
	if err := pool.QueryRow(context.Background(),
		`SELECT state FROM tasks WHERE user_id = $1 AND linear_issue_key = $2`,
		userID, "T-mid-ok-"+t.Name()).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(task.StateCancelled) {
		t.Errorf("已创建的第 0 个节点应被补偿为 cancelled，得到 %s", state)
	}
}

func TestGetFlowNotFoundForOtherUser(t *testing.T) {
	pool := testPool(t)
	userID, repoID := fixture(t, pool)
	other, _ := fixture(t, pool, "other")
	svc := &Service{Pool: pool, Tasks: task.NewMachine(pool)}

	flowID, _, _, err := svc.CreateFlow(context.Background(), userID, repoID, "priv-"+t.Name(),
		[]NodeInput{{IssueKey: "T-priv-" + t.Name()}})
	if err != nil {
		t.Fatalf("建图应成功，得到 %v", err)
	}

	if _, err := svc.GetFlow(context.Background(), other, flowID); !errors.Is(err, ErrFlowNotFound) {
		t.Fatalf("别人的 flow 应报 ErrFlowNotFound，得到 %v", err)
	}

	fs, err := svc.GetFlow(context.Background(), userID, flowID)
	if err != nil {
		t.Fatalf("属主读取应成功，得到 %v", err)
	}
	if len(fs.Tasks) != 1 || fs.Tasks[0].State != string(task.StateQueued) {
		t.Fatalf("应返回 1 个 queued 任务，得到 %+v", fs.Tasks)
	}
}

// TestServiceCreateFlowConcurrentDuplicateSubmissionSingleFlow 覆盖
// F1.4-AC2 的并发口径（审查发现的真实缺口）：多个 goroutine 几乎同时
// 提交完全相同的批次（同一 repo、同一 issue key 序列）。
//
// 修复前，detectDuplicateSubmission 的查重查询与后续建 flow/建 tasks
// 之间没有互斥：所有 goroutine 几乎同时执行查重，都查到"还没有人建过"，
// 于是全部各自往下建，产生多个孤儿 flow，只有 1 个能成功、其余全部撞
// tasks_one_active_per_issue 唯一索引收到裸的 ErrIssueActive。
//
// 修复后（acquireSubmissionLock 咨询锁），并发提交应该只有 1 个
// goroutine 真正建库，其余全部阻塞到它建完、释放锁之后，在查重阶段
// 看到已有的 flow，走幂等分支返回同一个 flowID——所有 goroutine 都应
// 拿到成功（同一个 flowID），不应该有任何一个拿到裸错误。
func TestServiceCreateFlowConcurrentDuplicateSubmissionSingleFlow(t *testing.T) {
	pool := testConcurrentPool(t)
	userID, repoID := fixture(t, pool)
	svc := &Service{Pool: pool, Tasks: task.NewMachine(pool)}

	nodes := []NodeInput{
		{IssueKey: "T-race-1-" + t.Name()},
		{IssueKey: "T-race-2-" + t.Name(), DependsOnIndex: p(0)},
	}

	const workers = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	flowIDs := make([]int64, workers)
	taskCounts := make([]int, workers)
	errs := make([]error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			fid, created, _, err := svc.CreateFlow(context.Background(), userID, repoID, "race-"+t.Name(), nodes)
			flowIDs[i] = fid
			taskCounts[i] = len(created)
			errs[i] = err
		}(i)
	}
	close(start) // 尽量让所有 goroutine 同时起跑，扩大竞态窗口
	wg.Wait()

	var firstFlowID int64
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d 应拿到幂等成功而不是裸错误，得到 %v", i, err)
		}
		if flowIDs[i] == 0 {
			t.Fatalf("goroutine %d 应拿到非零 flowID", i)
		}
		if taskCounts[i] != len(nodes) {
			t.Errorf("goroutine %d 应拿到 %d 个任务，得到 %d", i, len(nodes), taskCounts[i])
		}
		if i == 0 {
			firstFlowID = flowIDs[i]
			continue
		}
		if flowIDs[i] != firstFlowID {
			t.Errorf("goroutine %d 应拿到与其它 goroutine 相同的 flowID，得到 %d，期望 %d",
				i, flowIDs[i], firstFlowID)
		}
	}

	var flowCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM flows WHERE user_id = $1`, userID).Scan(&flowCount); err != nil {
		t.Fatal(err)
	}
	if flowCount != 1 {
		t.Fatalf("并发重复提交应只落库 1 个 flow，得到 %d 个", flowCount)
	}

	var taskCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM tasks WHERE flow_id = $1`, firstFlowID).Scan(&taskCount); err != nil {
		t.Fatal(err)
	}
	if taskCount != len(nodes) {
		t.Fatalf("并发重复提交应只落库 %d 个 task 行，得到 %d 个", len(nodes), taskCount)
	}
}

// resetFlowMaxChainLength 把 flow_max_chain_length 这个系统设置删掉，
// 恢复"从未配置过"的状态。system_settings 是全局共享的一张表，测试
// 结束后必须清掉，不然会污染同一次 go test 运行里其它测试看到的默认值。
func resetFlowMaxChainLength(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM system_settings WHERE key = $1`, store.SettingFlowMaxChainLength)
	})
}

// TestServiceCreateFlowChainWarningUsesDefaultWhenUnconfigured 覆盖
// F3.3-AC2 的"未配置时用默认值"分支：system_settings 里没有
// flow_max_chain_length 这一行时，1→2→3→4→5（深度 5）应按默认上限 4
// 判定，在第 5 个节点产生一条 warning；且即使超限，图依然被正常创建
// （不拒绝——F3.3-AC1"仅警告"精神在无 UI 场景下的落地）。
func TestServiceCreateFlowChainWarningUsesDefaultWhenUnconfigured(t *testing.T) {
	st := testStore(t)
	pool := st.Pool()
	resetFlowMaxChainLength(t, pool)
	userID, repoID := fixture(t, pool)
	svc := &Service{Pool: pool, Tasks: task.NewMachine(pool), Store: st}

	nodes := []NodeInput{
		{IssueKey: "W-1-" + t.Name()},
		{IssueKey: "W-2-" + t.Name(), DependsOnIndex: p(0)},
		{IssueKey: "W-3-" + t.Name(), DependsOnIndex: p(1)},
		{IssueKey: "W-4-" + t.Name(), DependsOnIndex: p(2)},
		{IssueKey: "W-5-" + t.Name(), DependsOnIndex: p(3)},
	}

	flowID, created, warnings, err := svc.CreateFlow(context.Background(), userID, repoID, "warn-"+t.Name(), nodes)
	if err != nil {
		t.Fatalf("超链长不应拒绝创建，得到 %v", err)
	}
	if flowID == 0 || len(created) != 5 {
		t.Fatalf("图应正常建出 5 个任务，得到 flowID=%d, created=%d", flowID, len(created))
	}
	if len(warnings) != 1 {
		t.Fatalf("应恰好产生 1 条 warning，得到 %d 条: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "W-5-"+t.Name()) {
		t.Errorf("warning 应指出第 5 个节点，得到 %q", warnings[0])
	}
}

// TestServiceCreateFlowChainWarningRespectsConfiguredLimit 覆盖
// F3.3-AC2 的核心断言：把 flow_max_chain_length 改成 2 后，1→2→3
// （深度 3）应按新上限判定，在第 3 个节点产生 warning——而不是按默认值
// 4（那样深度 3 不会超限，不应有 warning）。
func TestServiceCreateFlowChainWarningRespectsConfiguredLimit(t *testing.T) {
	st := testStore(t)
	pool := st.Pool()
	resetFlowMaxChainLength(t, pool)
	userID, repoID := fixture(t, pool)
	svc := &Service{Pool: pool, Tasks: task.NewMachine(pool), Store: st}

	if err := st.SetSetting(context.Background(), store.SettingFlowMaxChainLength, "2"); err != nil {
		t.Fatalf("写入 flow_max_chain_length 失败: %v", err)
	}

	nodes := []NodeInput{
		{IssueKey: "L-1-" + t.Name()},
		{IssueKey: "L-2-" + t.Name(), DependsOnIndex: p(0)},
		{IssueKey: "L-3-" + t.Name(), DependsOnIndex: p(1)},
	}

	_, _, warnings, err := svc.CreateFlow(context.Background(), userID, repoID, "limit-"+t.Name(), nodes)
	if err != nil {
		t.Fatalf("超链长不应拒绝创建，得到 %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("上限收紧到 2 后应产生 1 条 warning，得到 %d 条: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "L-3-"+t.Name()) {
		t.Errorf("warning 应指出第 3 个节点，得到 %q", warnings[0])
	}
	if !strings.Contains(warnings[0], "2") {
		t.Errorf("warning 应包含配置的上限 2，得到 %q", warnings[0])
	}
}

// TestServiceCreateFlowNoWarningWhenChainWithinConfiguredLimit 覆盖
// "没有超限就没有 warning"这一半（配置生效同样要能验证"不误报"）：
// 把上限放宽到 10 后，1→2→3→4→5（深度 5）不再超限，不应有任何 warning。
func TestServiceCreateFlowNoWarningWhenChainWithinConfiguredLimit(t *testing.T) {
	st := testStore(t)
	pool := st.Pool()
	resetFlowMaxChainLength(t, pool)
	userID, repoID := fixture(t, pool)
	svc := &Service{Pool: pool, Tasks: task.NewMachine(pool), Store: st}

	if err := st.SetSetting(context.Background(), store.SettingFlowMaxChainLength, "10"); err != nil {
		t.Fatalf("写入 flow_max_chain_length 失败: %v", err)
	}

	nodes := []NodeInput{
		{IssueKey: "H-1-" + t.Name()},
		{IssueKey: "H-2-" + t.Name(), DependsOnIndex: p(0)},
		{IssueKey: "H-3-" + t.Name(), DependsOnIndex: p(1)},
		{IssueKey: "H-4-" + t.Name(), DependsOnIndex: p(2)},
		{IssueKey: "H-5-" + t.Name(), DependsOnIndex: p(3)},
	}

	_, _, warnings, err := svc.CreateFlow(context.Background(), userID, repoID, "high-"+t.Name(), nodes)
	if err != nil {
		t.Fatalf("建图应成功，得到 %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("上限放宽到 10 后深度 5 不应超限，得到 warnings=%v", warnings)
	}
}
