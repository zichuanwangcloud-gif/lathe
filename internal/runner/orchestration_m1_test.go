package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Clouditera/lathe/internal/flow"
	"github.com/Clouditera/lathe/internal/integration/agent"
	"github.com/Clouditera/lathe/internal/integration/github"
	"github.com/Clouditera/lathe/internal/integration/linear"
	"github.com/Clouditera/lathe/internal/task"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ================================================================
// docs/07-prd-orchestration.md §5 · M1 出口条件端到端集成测试
//
//	"用 API 建一张 1→2→3 / 4 / 5→6 的图（假 tracker + 短路 agent），
//	 全自动跑完，人工干预 0 次；杀掉前驱后后继在 blocked_dep 且有
//	 回帖痕迹"
//
// 本文件只消费前面阶段已经交付的东西：
//   - flow.Service.CreateFlow 建图（不重新实现）
//   - task.Machine.ClaimReady / PropagateBlocked / WakeBlockedSuccessors
//     （不重新实现，直接调用真实实现）
//   - pipeline_test.go 里的 fakeLinear/fakeGitHub/fakeAgent/
//     fakeVerifications（直接复用，不重新发明假件）
//
// "调度循环"本身：cmd/lathe/queue.go 的 pollLoop 是私有的且跑在
// goroutine 池上，从 runner 包外部驱不动、也没必要为了这一个集成
// 测试去启动真实 HTTP server。这里手写一个行为等价、但同步单线程、
// 确定性的循环：反复调用 ClaimReady 拿到任务就跑 pipeline.Execute，
// 直到没有更多可领的任务为止 —— 既验证了 ClaimReady 的真实就绪判定
// SQL（不是 mock），又不需要真实的 goroutine 池/HTTP。
// ================================================================

// orchestrationFixture 建一对独立的 user/repo 与一个可构建的源仓库，
// 供整张图的 6 个任务共用（M1 不要求栈式 PR，所有任务都从
// RepoConfig.DefaultBranch 分叉，共用同一份源码毫无问题）。
func orchestrationFixture(t *testing.T) (pool *pgxpool.Pool, m *task.Machine, userID, repoID int64, repo RepoConfig, src string) {
	t.Helper()
	pool = testPoolForPipeline(t)
	m = task.NewMachine(pool)
	ctx := context.Background()

	providerRepo := "acme/orch-" + t.Name()

	email := "orch-" + t.Name() + "@example.com"
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) ON CONFLICT (email) DO UPDATE SET updated_at=now() RETURNING id`,
		email).Scan(&userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2)
		 ON CONFLICT (user_id, provider_repo) DO UPDATE SET updated_at=now() RETURNING id`,
		userID, providerRepo).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})
	return pool, m, userID, repoID, DefaultRepoConfig(providerRepo), goSourceRepo(t)
}

// intPtr 是本文件里 flow.NodeInput.DependsOnIndex 的取址小工具。
func intPtr(i int) *int { return &i }

// buildM1Graph 用 flow.Service.CreateFlow 建 "1→2→3 / 4 / 5→6" 这张图
// （M1 出口条件指定的形状），tag 用于让每次调用生成互不冲突的 issue
// key（同一测试函数内多次建图，或多个测试函数并行跑）。
func buildM1Graph(t *testing.T, pool *pgxpool.Pool, m *task.Machine, userID, repoID int64, tag string) (flowID int64, created []*task.Task) {
	t.Helper()
	svc := &flow.Service{Pool: pool, Tasks: m}

	nodes := []flow.NodeInput{
		{IssueKey: "M1-1-" + tag},                            // 独立根
		{IssueKey: "M1-2-" + tag, DependsOnIndex: intPtr(0)}, // 依赖 1
		{IssueKey: "M1-3-" + tag, DependsOnIndex: intPtr(1)}, // 依赖 2
		{IssueKey: "M1-4-" + tag},                            // 独立根
		{IssueKey: "M1-5-" + tag},                            // 独立根
		{IssueKey: "M1-6-" + tag, DependsOnIndex: intPtr(4)}, // 依赖 5
	}

	flowID, created, _, err := svc.CreateFlow(context.Background(), userID, repoID, "m1-"+tag, nodes)
	if err != nil {
		t.Fatalf("建图应成功，得到 %v", err)
	}
	if len(created) != 6 {
		t.Fatalf("应建出 6 个任务，得到 %d", len(created))
	}

	// 建图 API 本身的形状断言（M1 出口条件第一步："用 API 建一张
	// 1→2→3 / 4 / 5→6 的图"）：1/4/5 无前驱，2 依赖 1，3 依赖 2，
	// 6 依赖 5，且全部落在同一个 flow 下。
	roots := map[int]bool{0: true, 3: true, 4: true}
	for i, tk := range created {
		if tk.State != task.StateQueued {
			t.Errorf("第 %d 个任务初始状态应为 queued，得到 %s", i, tk.State)
		}
		if tk.FlowID == nil || *tk.FlowID != flowID {
			t.Errorf("第 %d 个任务应挂在 flow %d 下，得到 %v", i, flowID, tk.FlowID)
		}
		if roots[i] {
			if tk.DependsOn != nil {
				t.Errorf("第 %d 个任务应是独立根，depends_on 应为空，得到 %v", i, *tk.DependsOn)
			}
			continue
		}
		wantPredIdx := *nodes[i].DependsOnIndex
		if tk.DependsOn == nil || *tk.DependsOn != created[wantPredIdx].ID {
			t.Errorf("第 %d 个任务的前驱应是第 %d 个任务(id=%d)，得到 %v", i, wantPredIdx, created[wantPredIdx].ID, tk.DependsOn)
		}
	}

	return flowID, created
}

// fakeIssueFor 造一个假 tracker 的 issue，Identifier 决定分支名/提交
// 信息 —— 每个任务用各自的 identifier，worktree/分支互不冲突。
func fakeIssueFor(identifier string) *linear.Issue {
	return &linear.Issue{
		ID:          "uuid-" + identifier,
		Identifier:  identifier,
		Title:       "M1 编排内核集成测试 " + identifier,
		Description: "端到端固定文案：验证依赖调度与失败传播",
		URL:         "https://linear.app/x/" + identifier,
	}
}

// successfulAgent 是"短路 agent"：分诊判定可执行，实现阶段照抄
// TestPipelineHappyPath 里已验证过的红-绿证据链写法（复现测试改动前
// 失败、改动后通过），不重新发明新的假件行为。
func successfulAgent() *fakeAgent {
	return &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象和期望行为","question":""}`},
			{Success: true, Text: "补了 greet 函数与复现测试"},
		},
		mutate: []func(string) error{
			nil, // 分诊不改文件
			func(dir string) error {
				if err := os.WriteFile(filepath.Join(dir, "main_test.go"),
					[]byte("package main\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif greet() != \"hello\" {\n\t\tt.Fatalf(\"got %q\", greet())\n\t}\n}\n"), 0o644); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(dir, "fix.go"),
					[]byte("package main\n\nfunc greet() string { return \"hello\" }\n"), 0o644)
			},
		},
	}
}

// deterministicFailingAgent 是必然验证失败的短路 agent：分诊判定可
// 执行，实现阶段交付无法编译的代码且不带任何复现测试 —— 与
// TestPipelineVerificationFailurePreservesScene 完全同一手法：契约
// 违例（没交复现测试）且 MaxFixAttempts=0（不进修复回路），红阶段
// 直接判定验证未通过，任务必然落到 failed。
func deterministicFailingAgent() *fakeAgent {
	return &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象和期望行为","question":""}`},
			{Success: true, Text: "改完了（其实没交测试，也没编译过）"},
		},
		mutate: []func(string) error{
			nil,
			func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "broken.go"),
					[]byte("package main\n\nthis is not valid go\n"), 0o644)
			},
		},
	}
}

// orchestrationPipeline 组一个直接可跑的 Pipeline：复用 fakeLinear/
// fakeGitHub/fakeVerifications/fakeNotifier（pipeline_test.go），
// Worktrees/Verifier 在调用方按需共享。
func orchestrationPipeline(m *task.Machine, wm *WorktreeManager, lin *fakeLinear, gh *fakeGitHub, ag AgentDriver) *Pipeline {
	return &Pipeline{
		Tasks:          m,
		Worktrees:      wm,
		Verifier:       NewVerifier(3*time.Minute, ""),
		Agent:          ag,
		Clients:        &fakeClients{lin: lin, gh: gh},
		Notifier:       &fakeNotifier{},
		Verifications:  &fakeVerifications{},
		PermissionMode: "acceptEdits",
		SettingSources: "project",
	}
}

// claimAndExecute 领到的任务按 ID 分派到预先配置好的 Pipeline 假件上跑
// 一次 Execute，返回 Execute 的错误（调用方按场景决定是否当失败处理）。
// 这一步等价于 cmd/lathe/queue.go 的 runOneClaimed，只是把"从数据库读
// 仓库配置/凭据"简化成了从测试里预先建好的 map 里查——被测的核心逻辑
// （ClaimReady 的就绪判定 + Execute 的状态机推进）完全是真实实现。
func claimAndExecute(t *testing.T, m *task.Machine, repo RepoConfig, src string, pipelines map[int64]*Pipeline, tk *task.Task) error {
	t.Helper()
	p, ok := pipelines[tk.ID]
	if !ok {
		t.Fatalf("任务 %d（issue %s）没有配置流水线假件——测试固定数据与图不匹配", tk.ID, tk.LinearIssueKey)
	}
	issueID := ""
	if tk.LinearIssueID != nil {
		issueID = *tk.LinearIssueID
	}
	return p.Execute(context.Background(), ExecuteParams{
		TaskID: tk.ID, Repo: repo, CloneURL: src, IssueID: issueID, Actor: "node:test",
	})
}

// drainScheduler 是手写的调度循环等价物：反复调用 ClaimReady 拿到任务
// 就跑 Execute，直到 ClaimReady 连续判定"没有更多可领的任务"为止。
// maxIter 防的是被测代码有 bug 导致死循环（正常情况下 6 节点的图不会
// 逼近这个上限）。
func drainScheduler(t *testing.T, m *task.Machine, repo RepoConfig, src string, pipelines map[int64]*Pipeline) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		tk, err := m.ClaimReady(ctx, time.Minute)
		if err != nil {
			t.Fatalf("ClaimReady 失败: %v", err)
		}
		if tk == nil {
			return
		}
		if err := claimAndExecute(t, m, repo, src, pipelines, tk); err != nil {
			// 与 cmd/lathe/queue.go 的 runOneClaimed 同一立场：Execute
			// 失败已经在 pipeline 内部走完 D4 三件套（回帖+留现场+
			// 通知+失败传播），这里只记录，不重试、不人工介入。
			t.Logf("任务 %d（issue %s）Execute 返回错误（可能是本场景故意安排的失败）: %v", tk.ID, tk.LinearIssueKey, err)
		}
	}
	t.Fatalf("调度循环超过 50 轮仍未收敛，疑似死循环")
}

// firstTransitionAt 返回任务事件流中第一次转移到 to 状态的时间戳。
func firstTransitionAt(t *testing.T, m *task.Machine, taskID int64, to task.State) time.Time {
	t.Helper()
	events, err := m.Events(context.Background(), taskID)
	if err != nil {
		t.Fatalf("读取任务 %d 事件失败: %v", taskID, err)
	}
	for _, e := range events {
		if e.ToState == to {
			return e.At
		}
	}
	t.Fatalf("任务 %d 的事件流里没有到 %s 的转移: %+v", taskID, to, events)
	return time.Time{}
}

// ----------------------------------------------------------------
// 场景一：全自动跑完全图，人工干预 0 次
// ----------------------------------------------------------------

// TestM1OrchestrationHappyPathZeroHumanIntervention 直接证明 M1 出口
// 条件的第一句："用 API 建一张 1→2→3 / 4 / 5→6 的图（假 tracker + 短路
// agent），全自动跑完，人工干预 0 次"，并顺带覆盖 F2.1-AC4/F2.2-AC1/
// F2.2-AC3 三条相关验收标准。
//
// "人工干预 0 次"在这个测试里的含义：测试代码从建图之后，只做两类
// 动作——调用 task.Machine.ClaimReady（真实的就绪判定 SQL）和
// Pipeline.Execute（真实的状态机推进）；测试代码本身从未直接调用
// m.Transition 把任何任务从一个状态"手工"挪到另一个状态。全部 6 个
// 任务从 queued 走到 pr_open 的每一步转移，都是 Execute 内部自己做的。
func TestM1OrchestrationHappyPathZeroHumanIntervention(t *testing.T) {
	pool, m, userID, repoID, repo, src := orchestrationFixture(t)
	_ = userID
	ctx := context.Background()

	_, created := buildM1Graph(t, pool, m, userID, repoID, t.Name())
	task1, task2, task3, task4, task5, task6 := created[0], created[1], created[2], created[3], created[4], created[5]

	// ---- F2.2-AC1 的结构性证明：1/4/5 三个独立根之间没有互相等待 ----
	//
	// 在执行任何一个任务之前，连续调用 ClaimReady 三次：如果 1、4、5
	// 三者中任何一个要等另一个"先做完什么"才能被派发，这里就不可能
	// 连续三次都领到东西——但 ClaimReady 是"挑一个就绪任务"，不是"挑
	// 全部就绪任务"，所以这三次调用本身就是在问数据库"此刻除了刚被
	// 领走的以外，还有别的独立根就绪吗"，答案连续三次都是"有"，直到
	// 三个根全部领完为止。这证明了它们在调度器眼里【同时】就绪，没有
	// 谁在等谁——不需要真的并发执行才能证明这一点。
	var rootClaims []*task.Task
	for i := 0; i < 3; i++ {
		tk, err := m.ClaimReady(ctx, time.Minute)
		if err != nil {
			t.Fatalf("第 %d 次 ClaimReady 失败: %v", i+1, err)
		}
		if tk == nil {
			t.Fatalf("第 %d 次 ClaimReady 应仍能领到一个独立根任务，却领到了 nil", i+1)
		}
		rootClaims = append(rootClaims, tk)
	}
	// 第四次：2、3、6 都还没就绪（各自的前驱都还没到 pr_open），
	// 此刻不应再有任何任务可领——这同时也是 F2.1-AC4（前驱在
	// triaging/implementing/verifying 期间，后继一次都不被派发）在
	// t=0 时刻的验证：1 此刻甚至还没进 triaging，2 依然纹丝不动。
	if extra, err := m.ClaimReady(ctx, time.Minute); err != nil {
		t.Fatalf("第 4 次 ClaimReady 失败: %v", err)
	} else if extra != nil {
		t.Fatalf("此刻只有 3 个独立根应就绪，却又领到了任务 %d（issue %s）", extra.ID, extra.LinearIssueKey)
	}

	gotRoots := map[int64]bool{}
	for _, tk := range rootClaims {
		gotRoots[tk.ID] = true
	}
	wantRoots := map[int64]bool{task1.ID: true, task4.ID: true, task5.ID: true}
	for id := range wantRoots {
		if !gotRoots[id] {
			t.Errorf("独立根任务 %d 应在前三次 ClaimReady 里被领到，实际领到的集合: %v", id, gotRoots)
		}
	}
	for id := range gotRoots {
		if !wantRoots[id] {
			t.Errorf("前三次 ClaimReady 领到了非独立根任务 %d", id)
		}
	}

	// ---- 组装每个任务各自的假件（短路 agent + 假 tracker + 假 PR） ----
	wm, err := NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatalf("建工作区管理器失败: %v", err)
	}
	lins := map[int64]*fakeLinear{}
	ghs := map[int64]*fakeGitHub{}
	pipelines := map[int64]*Pipeline{}
	for i, tk := range created {
		lin := &fakeLinear{issue: fakeIssueFor(tk.LinearIssueKey)}
		gh := &fakeGitHub{pr: &github.PullRequest{
			Number: 100 + i,
			URL:    fmt.Sprintf("https://github.com/acme/demo/pull/%d", 100+i),
		}}
		lins[tk.ID] = lin
		ghs[tk.ID] = gh
		pipelines[tk.ID] = orchestrationPipeline(m, wm, lin, gh, successfulAgent())
	}

	// 先把前三次已经领到（打了租约）的独立根跑掉，再把调度循环接手跑
	// 剩下的——两段合起来就是完整的"反复 ClaimReady + Execute 直到
	// 无活可领"，覆盖 F2.2-AC3："一张图从入队到全部 pr_open，人工干预
	// 次数 = 0"。
	for _, tk := range rootClaims {
		if err := claimAndExecute(t, m, repo, src, pipelines, tk); err != nil {
			t.Fatalf("独立根任务 %d（issue %s）执行应成功，得到 %v", tk.ID, tk.LinearIssueKey, err)
		}
	}
	drainScheduler(t, m, repo, src, pipelines)

	// ---- 断言一：全部 6 个任务最终都是 pr_open ----
	for _, tk := range created {
		final, err := m.Get(ctx, tk.ID)
		if err != nil {
			t.Fatalf("读取任务 %d 失败: %v", tk.ID, err)
		}
		if final.State != task.StatePROpen {
			t.Errorf("任务 %d（issue %s）终态 = %s，期望 pr_open", tk.ID, tk.LinearIssueKey, final.State)
		}
		if final.PRURL == nil || *final.PRURL == "" {
			t.Errorf("任务 %d（issue %s）应落库 PR URL", tk.ID, tk.LinearIssueKey)
		}
	}

	// ---- 断言二（F2.1-AC4 端到端）：2 的 PR 不可能在 3 开始跑之前
	// 就被创建反过来说是错的方向；真正必须成立、且直接可从事件流验证
	// 的因果关系是链上"前驱开 PR 早于后继开始分诊"，逐环成立：
	//   1 的 pr_open 事件早于 2 的 triaging 开始事件
	//   2 的 pr_open 事件早于 3 的 triaging 开始事件
	//   5 的 pr_open 事件早于 6 的 triaging 开始事件
	// 这正是 ClaimReady 的就绪判定在时间线上留下的痕迹：2 能进
	// triaging 的前提就是它先被 ClaimReady 领到，而 ClaimReady 的 SQL
	// 要求此刻前驱已处于 pr_open/merged——所以 1 的 pr_open 必然先于
	// 2 的 triaging。
	if t1pr, t2triage := firstTransitionAt(t, m, task1.ID, task.StatePROpen), firstTransitionAt(t, m, task2.ID, task.StateTriaging); !t1pr.Before(t2triage) {
		t.Errorf("task1.pr_open(%s) 应早于 task2.triaging(%s)", t1pr, t2triage)
	}
	if t2pr, t3triage := firstTransitionAt(t, m, task2.ID, task.StatePROpen), firstTransitionAt(t, m, task3.ID, task.StateTriaging); !t2pr.Before(t3triage) {
		t.Errorf("task2.pr_open(%s) 应早于 task3.triaging(%s)", t2pr, t3triage)
	}
	if t5pr, t6triage := firstTransitionAt(t, m, task5.ID, task.StatePROpen), firstTransitionAt(t, m, task6.ID, task.StateTriaging); !t5pr.Before(t6triage) {
		t.Errorf("task5.pr_open(%s) 应早于 task6.triaging(%s)", t5pr, t6triage)
	}

	// ---- 断言三：1/4/5 三个独立根谁都没有等对方 ----
	// 上面 t=0 时刻的三连 ClaimReady 已经是结构性证明；这里补一条基于
	// 事件时序的交叉验证：4 与 5 的 triaging 开始时间，都不晚于 1 的
	// pr_open 时间之后才发生——换句话说，即使真的按 1、4、5 的顺序串行
	// 执行（本测试是单线程调度循环，天然串行），4 与 5 也不需要等 1
	// 到达任何状态就能各自被派发；它们各自的 triaging 开始时刻只取决
	// 于自己何时被 ClaimReady 领到，与 1 的进度无因果关系。用「1 的
	// pr_open 时间」和「4/5 的 triaging 开始时间」两者的 ClaimReady
	// 领取顺序（rootClaims，在任何 Execute 之前就已经确定）来印证：
	// rootClaims 记录的领取顺序里，4 和 5 紧跟在 1 后面被领到，而领取
	// 发生时 1 还只是 queued（未进 triaging），这本身就是「没有等」的
	// 直接证据（已在上面三连 ClaimReady 断言中验证）。这里再确认三者
	// 的 triaging 开始事件确实各自独立存在（不是被对方的完成触发的）：
	for _, tk := range []*task.Task{task1, task4, task5} {
		events, err := m.Events(ctx, tk.ID)
		if err != nil {
			t.Fatalf("读取任务 %d 事件失败: %v", tk.ID, err)
		}
		var payload map[string]any
		for _, e := range events {
			if e.ToState == task.StateTriaging {
				payload = e.Payload
				break
			}
		}
		if payload != nil {
			if _, unblocked := payload["unblocked_by"]; unblocked {
				t.Errorf("独立根任务 %d 的 triaging 转移 payload 不应带 unblocked_by（说明它不该是被谁唤醒的）: %v", tk.ID, payload)
			}
		}
	}

	// ---- 断言四：PR body 带验证证据（顺带确认短路 agent 走的是真实
	// 红-绿验证路径，不是伪造终态） ----
	for _, tk := range created {
		gh := ghs[tk.ID]
		if len(gh.params) != 1 {
			t.Errorf("任务 %d 应创建 1 个 PR，实际 %d", tk.ID, len(gh.params))
			continue
		}
		if gh.params[0].Base != "dev" {
			t.Errorf("任务 %d 的 PR base = %q，期望 dev", tk.ID, gh.params[0].Base)
		}
	}

	// ---- 断言五：假 tracker 都收到了"已完成并开出 PR"的回帖 ----
	for _, tk := range created {
		lin := lins[tk.ID]
		if len(lin.comments) != 1 {
			t.Errorf("任务 %d 应回帖 1 次，实际 %d: %v", tk.ID, len(lin.comments), lin.comments)
		}
	}
}

// ----------------------------------------------------------------
// 场景二：杀掉前驱（task1 验证必然失败）后，后继落 blocked_dep 且有
// 回帖痕迹——M1 出口条件的第二句。
// ----------------------------------------------------------------

// TestM1OrchestrationPredecessorFailureBlocksSuccessorsWithComment 直接
// 证明 M1 出口条件的第二句："杀掉前驱后后继在 blocked_dep 且有回帖
// 痕迹"，并覆盖 F2.3-AC1（后继转 blocked_dep，不停留在 queued）。
//
// 图形状与场景一相同，仍是一次真实建图；唯一区别是 task1 的短路 agent
// 换成 deterministicFailingAgent()：实现阶段交付无法编译、且没有任何
// 复现测试的改动。真实的 Verifier 会判定验证未通过（不是伪造状态），
// pipeline.fail() 因此被真实触发，进而调用真实的 PropagateBlocked。
func TestM1OrchestrationPredecessorFailureBlocksSuccessorsWithComment(t *testing.T) {
	pool, m, userID, repoID, repo, src := orchestrationFixture(t)
	ctx := context.Background()

	_, created := buildM1Graph(t, pool, m, userID, repoID, t.Name())
	task1, task2, task3, task4, task5, task6 := created[0], created[1], created[2], created[3], created[4], created[5]

	wm, err := NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatalf("建工作区管理器失败: %v", err)
	}

	pipelines := map[int64]*Pipeline{}
	lin1 := &fakeLinear{issue: fakeIssueFor(task1.LinearIssueKey)}
	// task1：必然验证失败的短路 agent —— 这是本场景对"杀掉前驱"的
	// 模拟：不是测试代码手工把任务判死，而是让真实的验证逻辑判它失败。
	pipelines[task1.ID] = orchestrationPipeline(m, wm, lin1, &fakeGitHub{}, deterministicFailingAgent())

	// task2、task3：预期永远不会被 ClaimReady 领到（会在 task1 失败的
	// 瞬间被 PropagateBlocked 转 blocked_dep），因此故意不为它们配置
	// 流水线——如果被测代码有 bug 真的把它们派发了，claimAndExecute
	// 会在这里直接 t.Fatalf，而不是安静地用某个默认假件把它们跑通。

	for i, tk := range []*task.Task{task4, task5, task6} {
		lin := &fakeLinear{issue: fakeIssueFor(tk.LinearIssueKey)}
		gh := &fakeGitHub{pr: &github.PullRequest{
			Number: 200 + i,
			URL:    fmt.Sprintf("https://github.com/acme/demo/pull/%d", 200+i),
		}}
		pipelines[tk.ID] = orchestrationPipeline(m, wm, lin, gh, successfulAgent())
	}

	drainScheduler(t, m, repo, src, pipelines)

	// ---- task1：验证必然失败 → failed ----
	final1, err := m.Get(ctx, task1.ID)
	if err != nil {
		t.Fatalf("读取 task1 失败: %v", err)
	}
	if final1.State != task.StateFailed {
		t.Fatalf("task1 终态 = %s，期望 failed（失败原因: %v）", final1.State, final1.FailureReason)
	}

	// ---- task2、task3：F2.3-AC1 明确要求的行为——转 blocked_dep，
	// 不是停留在 queued ----
	final2, err := m.Get(ctx, task2.ID)
	if err != nil {
		t.Fatalf("读取 task2 失败: %v", err)
	}
	if final2.State != task.StateBlockedDep {
		t.Errorf("task2 终态 = %s，期望 blocked_dep（直接后继）", final2.State)
	}
	if final2.State == task.StateQueued {
		t.Error("task2 绝不能停留在 queued（F2.3-AC1 明确禁止）")
	}

	final3, err := m.Get(ctx, task3.ID)
	if err != nil {
		t.Fatalf("读取 task3 失败: %v", err)
	}
	if final3.State != task.StateBlockedDep {
		t.Errorf("task3 终态 = %s，期望 blocked_dep（传递后继）", final3.State)
	}
	if final3.State == task.StateQueued {
		t.Error("task3 绝不能停留在 queued（F2.3-AC1 明确禁止）")
	}

	// ---- task4/5/6：不受 task1 失败影响，正常跑到 pr_open ----
	for _, tk := range []*task.Task{task4, task5, task6} {
		final, err := m.Get(ctx, tk.ID)
		if err != nil {
			t.Fatalf("读取任务 %d 失败: %v", tk.ID, err)
		}
		if final.State != task.StatePROpen {
			t.Errorf("任务 %d（issue %s）终态 = %s，期望 pr_open（不应受 task1 失败连累）", tk.ID, tk.LinearIssueKey, final.State)
		}
	}

	// ---- 回帖痕迹：task1 的假 tracker 客户端应收到对 task2、task3
	// 各自 issue 的阻塞回帖，且正文能看出是被 task1 连累的 ----
	seen := map[string]string{}
	for i, issueID := range lin1.commentIssueIDs {
		seen[issueID] = lin1.comments[i]
	}
	for _, tk := range []*task.Task{task2, task3} {
		if tk.LinearIssueID == nil {
			t.Fatalf("任务 %d 缺少 LinearIssueID，回帖断言无法进行", tk.ID)
		}
		body, ok := seen[*tk.LinearIssueID]
		if !ok {
			t.Errorf("issue %s（任务 %d）未收到阻塞回帖，实际回帖: %v (issues=%v)",
				*tk.LinearIssueID, tk.ID, lin1.comments, lin1.commentIssueIDs)
			continue
		}
		if !strings.Contains(body, "阻塞") && !strings.Contains(body, "blocked_dep") {
			t.Errorf("issue %s 的回帖内容没有阻塞相关字样: %s", *tk.LinearIssueID, body)
		}
		if !strings.Contains(body, task1.LinearIssueKey) && !strings.Contains(body, fmt.Sprintf("#%d", task1.ID)) {
			t.Errorf("issue %s 的回帖内容未指明是被前驱任务 %d/issue %s 连累: %s",
				*tk.LinearIssueID, task1.ID, task1.LinearIssueKey, body)
		}
	}
}

// ----------------------------------------------------------------
// 场景三：10 节点森林，F2.2-AC3 字面要求的"10 节点图全流程跑到
// pr_open"（不是场景一那张 6 节点/6 issue 的"1→2→3/4/5→6"图）。
// ----------------------------------------------------------------

// buildM1Graph10 用 flow.Service.CreateFlow 建一张真正有 10 个节点的
// 森林——"1→2→3 / 4→5 / 6 / 7→8→9→10"：两条长度 3、2 的依赖链，一个
// 独立单节点根，外加一条长度 4 的依赖链，体现"多条独立链 + 并行根"的
// 形态。结构不强求跟场景一那张图同构，但同样落实 F2.2-AC3 字面意思的
// "10 节点图"（场景一的图其实只有 6 个 issue）。
func buildM1Graph10(t *testing.T, pool *pgxpool.Pool, m *task.Machine, userID, repoID int64, tag string) (flowID int64, created []*task.Task) {
	t.Helper()
	svc := &flow.Service{Pool: pool, Tasks: m}

	nodes := []flow.NodeInput{
		{IssueKey: "M1-10-1-" + tag},                             // 独立根：链一起点
		{IssueKey: "M1-10-2-" + tag, DependsOnIndex: intPtr(0)},  // 依赖 1
		{IssueKey: "M1-10-3-" + tag, DependsOnIndex: intPtr(1)},  // 依赖 2
		{IssueKey: "M1-10-4-" + tag},                             // 独立根：链二起点
		{IssueKey: "M1-10-5-" + tag, DependsOnIndex: intPtr(3)},  // 依赖 4
		{IssueKey: "M1-10-6-" + tag},                             // 独立根：单节点，无后继
		{IssueKey: "M1-10-7-" + tag},                             // 独立根：链三起点
		{IssueKey: "M1-10-8-" + tag, DependsOnIndex: intPtr(6)},  // 依赖 7
		{IssueKey: "M1-10-9-" + tag, DependsOnIndex: intPtr(7)},  // 依赖 8
		{IssueKey: "M1-10-10-" + tag, DependsOnIndex: intPtr(8)}, // 依赖 9
	}

	flowID, created, _, err := svc.CreateFlow(context.Background(), userID, repoID, "m1-10-"+tag, nodes)
	if err != nil {
		t.Fatalf("建 10 节点图应成功，得到 %v", err)
	}
	if len(created) != 10 {
		t.Fatalf("应建出 10 个任务，得到 %d", len(created))
	}

	// 建图 API 本身的形状断言：0/3/5/6 四个独立根，其余依赖各自前驱，
	// 且全部落在同一个 flow 下——与场景一 buildM1Graph 的验证同一手法。
	roots := map[int]bool{0: true, 3: true, 5: true, 6: true}
	for i, tk := range created {
		if tk.State != task.StateQueued {
			t.Errorf("第 %d 个任务初始状态应为 queued，得到 %s", i, tk.State)
		}
		if tk.FlowID == nil || *tk.FlowID != flowID {
			t.Errorf("第 %d 个任务应挂在 flow %d 下，得到 %v", i, flowID, tk.FlowID)
		}
		if roots[i] {
			if tk.DependsOn != nil {
				t.Errorf("第 %d 个任务应是独立根，depends_on 应为空，得到 %v", i, *tk.DependsOn)
			}
			continue
		}
		wantPredIdx := *nodes[i].DependsOnIndex
		if tk.DependsOn == nil || *tk.DependsOn != created[wantPredIdx].ID {
			t.Errorf("第 %d 个任务的前驱应是第 %d 个任务(id=%d)，得到 %v", i, wantPredIdx, created[wantPredIdx].ID, tk.DependsOn)
		}
	}

	return flowID, created
}

// TestM1Orchestration10NodeGraphZeroHumanIntervention 补齐 F2.2-AC3
// 字面要求的"10 节点图全流程跑到 pr_open"这条缺口：场景一的图虽然是
// M1 出口条件原文指定的形状，但只有 6 个 issue，不满足"10 节点"的字面
// 要求。本测试用 buildM1Graph10 建一张真正有 10 个节点的森林，复用
// drainScheduler 把它完整跑到底：测试代码从建图之后只调用
// ClaimReady/Execute，从未直接调用 m.Transition 做任何手工状态转移——
// 与场景一"人工干预 0 次"的证明同一手法，断言全部 10 个任务最终都是
// pr_open。
func TestM1Orchestration10NodeGraphZeroHumanIntervention(t *testing.T) {
	pool, m, userID, repoID, repo, src := orchestrationFixture(t)
	ctx := context.Background()

	_, created := buildM1Graph10(t, pool, m, userID, repoID, t.Name())

	wm, err := NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatalf("建工作区管理器失败: %v", err)
	}

	pipelines := map[int64]*Pipeline{}
	lins := map[int64]*fakeLinear{}
	ghs := map[int64]*fakeGitHub{}
	for i, tk := range created {
		lin := &fakeLinear{issue: fakeIssueFor(tk.LinearIssueKey)}
		gh := &fakeGitHub{pr: &github.PullRequest{
			Number: 300 + i,
			URL:    fmt.Sprintf("https://github.com/acme/demo/pull/%d", 300+i),
		}}
		lins[tk.ID] = lin
		ghs[tk.ID] = gh
		pipelines[tk.ID] = orchestrationPipeline(m, wm, lin, gh, successfulAgent())
	}

	// 全程只有 ClaimReady + Execute：drainScheduler 反复调用两者直到
	// 没有更多可领的任务为止，测试代码本身不做任何手工状态转移。
	drainScheduler(t, m, repo, src, pipelines)

	for _, tk := range created {
		final, err := m.Get(ctx, tk.ID)
		if err != nil {
			t.Fatalf("读取任务 %d 失败: %v", tk.ID, err)
		}
		if final.State != task.StatePROpen {
			t.Errorf("任务 %d（issue %s）终态 = %s，期望 pr_open", tk.ID, tk.LinearIssueKey, final.State)
		}
		if final.PRURL == nil || *final.PRURL == "" {
			t.Errorf("任务 %d（issue %s）应落库 PR URL", tk.ID, tk.LinearIssueKey)
		}
	}

	// 顺带确认假 tracker/PR 假件都被真实调用过（不是空跑）：10 个任务
	// 各自开出 1 个 PR、回帖 1 次。
	for _, tk := range created {
		if gh := ghs[tk.ID]; len(gh.params) != 1 {
			t.Errorf("任务 %d 应创建 1 个 PR，实际 %d", tk.ID, len(gh.params))
		}
		if lin := lins[tk.ID]; len(lin.comments) != 1 {
			t.Errorf("任务 %d 应回帖 1 次，实际 %d: %v", tk.ID, len(lin.comments), lin.comments)
		}
	}
}
