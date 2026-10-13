package store

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// memoryLookup 构造一个定位键。external_key 带随机量：失败记忆按
// (repo, key) 定位，固定 key 会让跨轮次残留的行混进断言。
func memoryLookup(userID, repoID int64, key string) FailureLookup {
	return FailureLookup{
		UserID: userID, RepoID: repoID,
		TrackerProvider: "internal", ExternalKey: key,
	}
}

// ★ AC3 的写侧：任务失败 → digest 落库。
func TestFailureDigestInsertAndLatest(t *testing.T) {
	st := testStore(t)
	userID, repoID := metricsFixture(t, st)
	ctx := context.Background()

	tk1 := metricsTask(t, st, userID, repoID, "MEM-1")
	lk := memoryLookup(userID, repoID, "MEM-1")

	if err := st.InsertFailureDigest(ctx, FailureDigest{
		TaskID: tk1, Stage: "verify_failed",
		Reason: "验证未通过: build 挂了", Excerpt: "main.go:12: undefined: greet",
	}, lk); err != nil {
		t.Fatalf("落库失败: %v", err)
	}

	got, err := st.LatestFailureDigest(ctx, lk, 0)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got == nil {
		t.Fatal("应查到一条失败记忆，得到 nil")
	}
	if got.TaskID != tk1 || got.Stage != "verify_failed" {
		t.Errorf("记忆内容不符: %+v", got)
	}
	if got.Excerpt != "main.go:12: undefined: greet" {
		t.Errorf("摘要未落库: %q", got.Excerpt)
	}
	if got.At == "" {
		t.Error("时间应格式化后返回（前端要直接渲染）")
	}
}

// ★ AC3 的另一半：无历史失败时返回 (nil, nil) —— 「没失败过」是正常状态
// 而不是要判错的路径。调用方（提示词注入）据此让提示词保持原样。
func TestFailureDigestNoneIsNotAnError(t *testing.T) {
	st := testStore(t)
	userID, repoID := metricsFixture(t, st)

	got, err := st.LatestFailureDigest(context.Background(),
		memoryLookup(userID, repoID, "NEVER-FAILED"), 0)
	if err != nil {
		t.Fatalf("没有历史失败不该报错: %v", err)
	}
	if got != nil {
		t.Errorf("没有历史失败应返回 nil，得到 %+v", got)
	}
}

// 同一单子多次失败：读最近一次，历史行全部保留。
//
// append-only 是刻意的（见 0030 迁移注释）：upsert 会把「失败了 5 次」
// 压成「最近失败过」，而那正是最该被看见的信号。
func TestFailureDigestKeepsHistoryAndReturnsLatest(t *testing.T) {
	st := testStore(t)
	userID, repoID := metricsFixture(t, st)
	ctx := context.Background()

	tk := metricsTask(t, st, userID, repoID, "MEM-HIST")
	lk := memoryLookup(userID, repoID, "MEM-HIST")

	// 三次失败，at 显式指定以保证顺序确定（默认 now() 在同一事务/同一
	// 毫秒内会相同，靠 id 破平虽可行，但显式时间更能表达意图）。
	for i, stage := range []string{"triage_run", "implement_run", "verify_failed"} {
		if err := st.InsertFailureDigest(ctx, FailureDigest{
			TaskID: tk, Stage: stage, Reason: "第 " + strconv.Itoa(i+1) + " 次",
		}, lk); err != nil {
			t.Fatalf("落库第 %d 条失败: %v", i+1, err)
		}
		// 让相邻两条的 at 有确定差异
		if _, err := st.pool.Exec(ctx,
			`UPDATE failure_digests SET at = now() + make_interval(secs => $2)
			 WHERE task_id = $1 AND failure_stage = $3`,
			tk, i*10, stage); err != nil {
			t.Fatalf("调整时间失败: %v", err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	got, err := st.LatestFailureDigest(ctx, lk, 0)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got == nil || got.Stage != "verify_failed" {
		t.Fatalf("应取最近一次（verify_failed），得到 %+v", got)
	}

	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM failure_digests WHERE user_id = $1 AND external_key = $2`,
		userID, "MEM-HIST").Scan(&n); err != nil {
		t.Fatalf("数历史行失败: %v", err)
	}
	if n != 3 {
		t.Errorf("历史行数 = %d，期望 3（append-only，不 upsert）", n)
	}
}

// excludeTaskID 排除本任务自己的记录。
//
// 这条规则的必要性：人工重试走的是【原任务行】（retryTask 不新建 ——
// 任务 #313 的教训），于是「跨任务记忆」在续跑路径上会读到任务自己
// 写下的那一条，而那份失败原因已经由 ResumeFixPrompt 带在提示词里了。
func TestFailureDigestExcludesOwnTask(t *testing.T) {
	st := testStore(t)
	userID, repoID := metricsFixture(t, st)
	ctx := context.Background()

	// 同一单子的两行任务：老的那行留下过失败记忆。
	//
	// 老行必须先转终态 —— 同一单子不允许有两个【活着的】任务
	// （tasks_one_active_per_item 部分唯一索引）。这正是真实的
	// 「issue 关掉重开」形态：老任务已终结，新任务另起一行。
	old := metricsTask(t, st, userID, repoID, "MEM-EXCL")
	lk := memoryLookup(userID, repoID, "MEM-EXCL")
	if err := st.InsertFailureDigest(ctx, FailureDigest{
		TaskID: old, Stage: "triage_run", Reason: "老任务失败",
	}, lk); err != nil {
		t.Fatalf("落库失败: %v", err)
	}
	metricsTaskFailed(t, st, old, "triage_run")

	// 当前这行自己也失败过
	cur := metricsTask(t, st, userID, repoID, "MEM-EXCL")
	if err := st.InsertFailureDigest(ctx, FailureDigest{
		TaskID: cur, Stage: "implement_run", Reason: "当前任务失败",
	}, lk); err != nil {
		t.Fatalf("落库失败: %v", err)
	}

	// 不排除：拿到当前任务那一条（最近）
	got, err := st.LatestFailureDigest(ctx, lk, 0)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got == nil || got.TaskID != cur {
		t.Fatalf("不排除时应拿到最近的一条（task %d），得到 %+v", cur, got)
	}

	// 排除当前任务：跳过它，拿到老任务那一条
	got, err = st.LatestFailureDigest(ctx, lk, cur)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got == nil {
		t.Fatal("排除自记录后仍应拿到老任务的历史失败")
	}
	if got.TaskID != old || got.Stage != "triage_run" {
		t.Errorf("排除自记录后应拿到老任务那条，得到 %+v", got)
	}

	// 只有自记录时，排除后就是「没有历史」—— (nil, nil)
	only := metricsTask(t, st, userID, repoID, "MEM-ONLY-SELF")
	lkOnly := memoryLookup(userID, repoID, "MEM-ONLY-SELF")
	if err := st.InsertFailureDigest(ctx, FailureDigest{
		TaskID: only, Stage: "push", Reason: "只有自己失败过",
	}, lkOnly); err != nil {
		t.Fatalf("落库失败: %v", err)
	}
	got, err = st.LatestFailureDigest(ctx, lkOnly, only)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got != nil {
		t.Errorf("只有自记录且被排除时应返回 nil，得到 %+v", got)
	}
}

// ★ AC2 在记忆侧的对应：定位键必须把不同单子、不同平台、不同用户分开。
//
// external_key 是各平台自己的编号空间（Linear 的 CR-1326、内置工单的
// LT-1042），不按平台分开就会把两个平台的同号单子混成一条记忆；不按
// 用户分开就是跨租户串台。
func TestFailureDigestIsolation(t *testing.T) {
	st := testStore(t)
	userA, repoA := metricsFixture(t, st)
	userB, repoB := metricsFixture(t, st)
	ctx := context.Background()

	tkA := metricsTask(t, st, userA, repoA, "ISO-KEY")
	if err := st.InsertFailureDigest(ctx, FailureDigest{
		TaskID: tkA, Stage: "verify_failed", Reason: "A 的失败",
	}, FailureLookup{UserID: userA, RepoID: repoA, TrackerProvider: "internal", ExternalKey: "ISO-KEY"}); err != nil {
		t.Fatalf("落库失败: %v", err)
	}

	cases := []struct {
		name string
		lk   FailureLookup
	}{
		{"另一个用户", FailureLookup{UserID: userB, RepoID: repoB, TrackerProvider: "internal", ExternalKey: "ISO-KEY"}},
		{"另一个平台（同 key）", FailureLookup{UserID: userA, RepoID: repoA, TrackerProvider: "linear", ExternalKey: "ISO-KEY"}},
		{"另一个单子", FailureLookup{UserID: userA, RepoID: repoA, TrackerProvider: "internal", ExternalKey: "ISO-OTHER"}},
		{"另一个仓库", FailureLookup{UserID: userA, RepoID: repoB, TrackerProvider: "internal", ExternalKey: "ISO-KEY"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := st.LatestFailureDigest(ctx, c.lk, 0)
			if err != nil {
				t.Fatalf("查询失败: %v", err)
			}
			if got != nil {
				t.Errorf("%s：不该读到别人的失败记忆，得到 %+v", c.name, got)
			}
		})
	}
}

// ★ AC2 的最严一档：**同 repo_id、不同 user_id** 也必须隔离。
//
// 上一条用例（TestFailureDigestIsolation）里的「另一个用户」走的是
// userB + repoB —— 两个维度一起变，于是 `user_id = $1` 这一条单独去掉
// 也照样绿（repo_id 那一半还挡着）。这条把变量降到只剩 user_id：repo、
// 平台、单号三者全同，只有属主不同。
//
// 造法：B 派发同一个单子、指向 A 的 repo（`tasks` 没有「任务行必须属于
// repo 属主」的约束，这正是「一个工作区被跨用户同名 issue 共用」那条
// 老问题的来源，见 machine.go 的 ClaimedWorktreePaths 注释）。两行任务
// 只能有一个是活着的（tasks_one_active_per_item），所以 A 那行先转终态
// —— 与真实的「B 接了同一个 issue」形态一致：A 已经失败收场。
//
// 0030 迁移的多租户段（显式带 user_id、不靠 repo_id 隐式限定）守的正是
// 这条边界：一旦查询漏了 user_id，B 派发时会读到 A 的记忆。
func TestFailureDigestIsolatesSameRepoDifferentUser(t *testing.T) {
	st := testStore(t)
	userA, repoA := metricsFixture(t, st)
	userB, _ := metricsFixture(t, st)
	ctx := context.Background()

	const key = "MEM-CROSS-USER"
	tkA := metricsTask(t, st, userA, repoA, key)
	// A 的那行先终态：否则 B 建同 (repo, key) 的活任务会撞部分唯一索引
	metricsTaskFailed(t, st, tkA, "verify_failed")
	tkB := metricsTask(t, st, userB, repoA, key)

	if err := st.InsertFailureDigest(ctx, FailureDigest{
		TaskID: tkA, Stage: "verify_failed", Reason: "A 的失败",
	}, memoryLookup(userA, repoA, key)); err != nil {
		t.Fatalf("落库失败: %v", err)
	}
	if err := st.InsertFailureDigest(ctx, FailureDigest{
		TaskID: tkB, Stage: "implement_run", Reason: "B 的失败",
	}, memoryLookup(userB, repoA, key)); err != nil {
		t.Fatalf("落库失败: %v", err)
	}

	gotA, err := st.LatestFailureDigest(ctx, memoryLookup(userA, repoA, key), 0)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if gotA == nil || gotA.Stage != "verify_failed" || gotA.TaskID != tkA {
		t.Fatalf("A 应只看到自己那条（verify_failed / task %d），得到 %+v", tkA, gotA)
	}

	gotB, err := st.LatestFailureDigest(ctx, memoryLookup(userB, repoA, key), 0)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if gotB == nil || gotB.Stage != "implement_run" || gotB.TaskID != tkB {
		t.Fatalf("B 应只看到自己那条（implement_run / task %d），得到 %+v", tkB, gotB)
	}
}

// 删除用户时记忆随之消失（外键 ON DELETE CASCADE）。
//
// 断言的意义在于：这条链上挂了三个外键（user/repo/task），漏掉任何一个
// 的级联都会让「注销用户」变成一次失败，或者留下悬空的记忆行。
func TestFailureDigestCascadesOnUserDelete(t *testing.T) {
	st := testStore(t)
	userID, repoID := metricsFixture(t, st)
	ctx := context.Background()

	tk := metricsTask(t, st, userID, repoID, "MEM-CASCADE")
	if err := st.InsertFailureDigest(ctx, FailureDigest{
		TaskID: tk, Stage: "push", Reason: "待级联删除",
	}, memoryLookup(userID, repoID, "MEM-CASCADE")); err != nil {
		t.Fatalf("落库失败: %v", err)
	}

	// metricsFixture 的 cleanup 也会删，这里显式删一次以断言级联
	if _, err := st.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		t.Fatalf("删用户失败（级联没配好？）: %v", err)
	}

	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM failure_digests WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("数残留行失败: %v", err)
	}
	if n != 0 {
		t.Errorf("用户删除后残留 %d 条失败记忆，级联未生效", n)
	}
}
