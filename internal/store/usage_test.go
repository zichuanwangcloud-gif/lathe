package store

// usage_test.go 用量账本（f03，docs/features/usage-stats.md）store 层的契约。
// 测试库是共享开发库：每条用例新建自己的用户/仓库（email 带随机量），
// 断言一律按归属过滤（AGENTS.md §5.5）。

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
	"github.com/zichuanwangcloud-gif/lathe/migrations"
)

// ---------------------------------------------------------------- 夹具

func usageUser(t *testing.T, st *Store) (userID, repoID int64) {
	t.Helper()
	ctx := context.Background()
	nonce := fmt.Sprint(time.Now().UnixNano())
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"usage-"+nonce+"@example.com").Scan(&userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})
	return userID, usageRepo(t, st, userID)
}

func usageRepo(t *testing.T, st *Store, userID int64) int64 {
	t.Helper()
	var repoID int64
	if err := st.pool.QueryRow(context.Background(),
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		userID, fmt.Sprintf("acme/usage-%d", time.Now().UnixNano())).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	return repoID
}

func usageTask(t *testing.T, st *Store, userID, repoID int64) int64 {
	t.Helper()
	var id int64
	if err := st.pool.QueryRow(context.Background(),
		`INSERT INTO tasks (user_id, repo_id, external_key) VALUES ($1,$2,$3) RETURNING id`,
		userID, repoID, fmt.Sprintf("UT-%d", time.Now().UnixNano())).Scan(&id); err != nil {
		t.Fatalf("建 task 失败: %v", err)
	}
	return id
}

func usagePRD(t *testing.T, st *Store, userID, repoID int64) int64 {
	t.Helper()
	p, err := st.CreatePRD(context.Background(), CreatePRDParams{
		UserID: userID, RepoID: repoID, PRDType: "feature", OriginalInput: "用量统计夹具",
	})
	if err != nil {
		t.Fatalf("建 PRD 失败: %v", err)
	}
	return p.ID
}

// seq 给出严格递增的开始时刻，避免同一用例里两条记录撞上唯一约束。
type seq struct{ t time.Time }

func newSeq(base time.Time) *seq { return &seq{t: base} }
func (s *seq) next() time.Time   { s.t = s.t.Add(time.Second); return s.t }

func paid(usd float64) *agent.Result {
	return &agent.Result{Success: true, Subtype: "success", CostUSD: usd, CostKnown: true, NumTurns: 1, DurationMS: 1000}
}

func record(t *testing.T, st *Store, r agent.RunRecord) {
	t.Helper()
	if r.EndedAt.IsZero() {
		r.EndedAt = r.StartedAt.Add(time.Second)
	}
	if err := st.RecordAgentRun(context.Background(), r); err != nil {
		t.Fatalf("RecordAgentRun(%s/%s) 失败: %v", r.Kind, r.Phase, err)
	}
}

func wideQuery(group string) UsageQuery {
	now := time.Now()
	return UsageQuery{From: now.Add(-365 * 24 * time.Hour), To: now.Add(24 * time.Hour), Group: group, Limit: 20}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func costOf(p *float64) string {
	if p == nil {
		return "null"
	}
	return fmt.Sprintf("%.6f", *p)
}

func groupKeys(gs []UsageGroup) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.Key)
	}
	return out
}

func findGroup(gs []UsageGroup, key string) *UsageGroup {
	for i := range gs {
		if gs[i].Key == key {
			return &gs[i]
		}
	}
	return nil
}

// ---------------------------------------------------------------- 纯函数

// AC-5：阶段桶归并的唯一真相源。
func TestUsageBucket(t *testing.T) {
	cases := []struct {
		phase  string
		review bool
		want   string
	}{
		{"triage", false, "triage"}, {"triage", true, "triage"},
		{"implement", false, "implement"}, {"implement", true, "review"},
		{"implement-reentry", false, "implement"}, {"implement-reentry", true, "review"},
		{"fix-1", false, "implement"}, {"fix-1", true, "review"}, {"fix-12", false, "implement"},
		{"fix-", false, "other"}, {"fix-1a", false, "other"},
		{"verify", false, "verify"}, {"push", false, "push"},
		{"plan", false, "prd_plan"}, {"plan-review", false, "prd_review"},
		{"recommend", false, "preview"}, {"review", false, "review"},
		{"whatever", false, "other"}, {"whatever", true, "other"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("给定(%s,%v)归%s", tc.phase, tc.review, tc.want), func(t *testing.T) {
			if got := UsageBucket(tc.phase, tc.review); got != tc.want {
				t.Errorf("UsageBucket(%q,%v) got %q, want %q", tc.phase, tc.review, got, tc.want)
			}
		})
	}
}

// AC-2：状态判定表，按序首个命中。
func TestRunStatus(t *testing.T) {
	cases := []struct {
		name string
		res  *agent.Result
		err  error
		want string
	}{
		{"静默判死", nil, fmt.Errorf("%w（…）", agent.ErrIdle), "killed"},
		{"判死但 result 已到", &agent.Result{Success: true}, fmt.Errorf("%w（…）", agent.ErrIdle), "killed"},
		{"总超时（%w DeadlineExceeded）", nil, fmt.Errorf("agent: 执行超时（上限 1s），进程树已回收: %w", context.DeadlineExceeded), "killed"},
		{"取消", nil, fmt.Errorf("agent: 执行被取消: %w", context.Canceled), "killed"},
		{"旧形态：只有「执行超时」文本", nil, errors.New("agent: 执行超时（上限 45m0s），进程树已回收"), "killed"},
		{"CLI 没给 result", nil, fmt.Errorf("%w（…）", agent.ErrNoResultEvent), "no_result"},
		{"其余错误", nil, errors.New("agent: 启动 claude 失败"), "error"},
		{"非零退出但有 result", &agent.Result{Success: true}, errors.New("agent: CLI 非零退出"), "error"},
		{"CLI 报失败 is_error", &agent.Result{IsError: true, Subtype: "error_max_turns"}, nil, "error"},
		{"CLI 报非 success", &agent.Result{Subtype: "error_during_execution"}, nil, "error"},
		{"成功", &agent.Result{Success: true, Subtype: "success"}, nil, "success"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RunStatus(tc.res, tc.err); got != tc.want {
				t.Errorf("RunStatus got %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------- 写入

// AC-1b：费用未知落 NULL，与「确实 $0」可区分；聚合侧 costUsd 为 null。
func TestRecordAgentRunCostUnknown(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, repoID := usageUser(t, st)
	unknown := usageTask(t, st, userID, repoID)
	zero := usageTask(t, st, userID, repoID)
	s := newSeq(time.Now().Add(-time.Hour))

	record(t, st, agent.RunRecord{Kind: "task", TaskID: unknown, Phase: "implement", StartedAt: s.next(),
		Result: &agent.Result{Success: true, Subtype: "success", CostUSD: 0, CostKnown: false,
			Usage: agent.Usage{Present: true, InputTokens: 11, OutputTokens: 3}}})
	record(t, st, agent.RunRecord{Kind: "task", TaskID: zero, Phase: "implement", StartedAt: s.next(),
		Result: &agent.Result{Success: true, Subtype: "success", CostUSD: 0, CostKnown: true}})

	cases := []struct {
		name        string
		taskID      int64
		wantCostNil bool
		wantKnown   bool
		wantInput   int64
	}{
		{"CostKnown=false → cost_usd IS NULL、tokens_known=true", unknown, true, true, 11},
		{"CostKnown=true、$0 → cost_usd = 0", zero, false, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cost *float64
			var known bool
			var input int64
			var status string
			if err := st.pool.QueryRow(ctx, `SELECT cost_usd::float8, tokens_known, input_tokens, status
				FROM agent_runs WHERE task_id=$1`, tc.taskID).Scan(&cost, &known, &input, &status); err != nil {
				t.Fatalf("读行失败: %v", err)
			}
			if (cost == nil) != tc.wantCostNil || (cost != nil && *cost != 0) {
				t.Errorf("cost_usd got %s, want nil=%v（否则 0）", costOf(cost), tc.wantCostNil)
			}
			if known != tc.wantKnown || input != tc.wantInput || status != "success" {
				t.Errorf("tokens_known/input/status got %v/%d/%s, want %v/%d/success", known, input, status, tc.wantKnown, tc.wantInput)
			}
		})
	}

	us, err := st.UsageStats(ctx, userID, wideQuery("phase"))
	if err != nil {
		t.Fatal(err)
	}
	if us.Totals.CostKnown != 1 || us.Totals.TokensKnown != 1 || us.Totals.CostUSD == nil || *us.Totals.CostUSD != 0 {
		t.Errorf("两行合查 costKnown/tokensKnown/costUsd got %d/%d/%s, want 1/1/0（codex 行 Present 但另一行没报 token）",
			us.Totals.CostKnown, us.Totals.TokensKnown, costOf(us.Totals.CostUSD))
	}
}

// AC-1b：只有费用未知的行时，totals.costUsd 是 null 而不是 0。
func TestUsageStatsCostUnknownIsNull(t *testing.T) {
	st := testStore(t)
	userID, repoID := usageUser(t, st)
	task := usageTask(t, st, userID, repoID)
	record(t, st, agent.RunRecord{Kind: "task", TaskID: task, Phase: "implement", StartedAt: time.Now().Add(-time.Minute),
		Result: &agent.Result{Success: true, CostKnown: false, Usage: agent.Usage{Present: true, InputTokens: 11}}})
	us, err := st.UsageStats(context.Background(), userID, wideQuery("phase"))
	if err != nil {
		t.Fatal(err)
	}
	if us.Totals.CostUSD != nil || us.Totals.CostKnown != 0 || us.Totals.Runs != 1 {
		t.Errorf("costUsd/costKnown/runs got %s/%d/%d, want null/0/1", costOf(us.Totals.CostUSD), us.Totals.CostKnown, us.Totals.Runs)
	}
}

// AC-2c：PRD 运行的属主与仓库由 store 从 prds 行取；目标不存在报 ErrAgentRunTargetGone。
func TestRecordAgentRunPRDOwnership(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, repoID := usageUser(t, st)
	prdID := usagePRD(t, st, userID, repoID)

	record(t, st, agent.RunRecord{Kind: "prd", PRDID: prdID, Phase: "plan", StartedAt: time.Now().Add(-time.Minute), Result: paid(0.1)})
	var gotUser, gotRepo int64
	var bucket string
	var taskNull bool
	if err := st.pool.QueryRow(ctx, `SELECT user_id, repo_id, bucket, task_id IS NULL FROM agent_runs WHERE prd_id=$1`, prdID).
		Scan(&gotUser, &gotRepo, &bucket, &taskNull); err != nil {
		t.Fatalf("读行失败: %v", err)
	}
	if gotUser != userID || gotRepo != repoID || bucket != "prd_plan" || !taskNull {
		t.Errorf("user/repo/bucket/task_id IS NULL got %d/%d/%s/%v, want %d/%d/prd_plan/true", gotUser, gotRepo, bucket, taskNull, userID, repoID)
	}

	gone := prdID + 1_000_000_000
	err := st.RecordAgentRun(ctx, agent.RunRecord{Kind: "prd", PRDID: gone, Phase: "plan", StartedAt: time.Now(), EndedAt: time.Now()})
	if !errors.Is(err, ErrAgentRunTargetGone) {
		t.Errorf("不存在的 PRD got err=%v, want ErrAgentRunTargetGone", err)
	}
	var n int
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM agent_runs WHERE prd_id=$1`, gone).Scan(&n)
	if n != 0 {
		t.Errorf("不存在目标的行数 got %d, want 0", n)
	}
}

// AC-2d：预览运行的属主、仓库、档位都取任务行。
func TestRecordAgentRunPreviewOwnership(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, repoID := usageUser(t, st)
	task := usageTask(t, st, userID, repoID)
	if _, err := st.pool.Exec(ctx, `UPDATE tasks SET verify_tier='heavy' WHERE id=$1`, task); err != nil {
		t.Fatal(err)
	}
	record(t, st, agent.RunRecord{Kind: "preview", TaskID: task, Phase: "recommend", StartedAt: time.Now().Add(-time.Minute), Result: paid(0.05)})
	var gotUser, gotRepo int64
	var bucket, kind string
	var tier *string
	if err := st.pool.QueryRow(ctx, `SELECT user_id, repo_id, bucket, run_kind, verify_tier FROM agent_runs WHERE task_id=$1`, task).
		Scan(&gotUser, &gotRepo, &bucket, &kind, &tier); err != nil {
		t.Fatalf("读行失败: %v", err)
	}
	if gotUser != userID || gotRepo != repoID || bucket != "preview" || kind != "preview" || tier == nil || *tier != "heavy" {
		t.Errorf("user/repo/bucket/kind/tier got %d/%d/%s/%s/%v, want %d/%d/preview/preview/heavy", gotUser, gotRepo, bucket, kind, tier, userID, repoID)
	}
}

// AC-4：同一次尝试重复写只有一行（含 NULL 列），第二次返回 nil。
func TestRecordAgentRunIsIdempotent(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, repoID := usageUser(t, st)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Microsecond)

	cases := []struct {
		name   string
		mk     func(task, prd int64) (agent.RunRecord, agent.RunRecord)
		filter string
		want   int
	}{
		{"任务运行（prd_id 为 NULL）", func(task, _ int64) (agent.RunRecord, agent.RunRecord) {
			r := agent.RunRecord{Kind: "task", TaskID: task, Phase: "implement", SessionID: "s", StartedAt: t0}
			return r, r
		}, "task_id", 1},
		{"任务运行、session_id 为 NULL", func(task, _ int64) (agent.RunRecord, agent.RunRecord) {
			r := agent.RunRecord{Kind: "task", TaskID: task, Phase: "implement", StartedAt: t0}
			return r, r
		}, "task_id", 1},
		{"PRD 运行（task_id 为 NULL）、session_id 为 NULL", func(_, prd int64) (agent.RunRecord, agent.RunRecord) {
			r := agent.RunRecord{Kind: "prd", PRDID: prd, Phase: "plan", StartedAt: t0}
			return r, r
		}, "prd_id", 1},
		{"预览运行", func(task, _ int64) (agent.RunRecord, agent.RunRecord) {
			r := agent.RunRecord{Kind: "preview", TaskID: task, Phase: "recommend", SessionID: "s2", StartedAt: t0}
			return r, r
		}, "task_id", 1},
		{"不同 started_at 是两次尝试", func(task, _ int64) (agent.RunRecord, agent.RunRecord) {
			r := agent.RunRecord{Kind: "task", TaskID: task, Phase: "implement", SessionID: "s", StartedAt: t0}
			r2 := r
			r2.StartedAt = t0.Add(time.Microsecond)
			return r, r2
		}, "task_id", 2},
		{"不同 resume 是两次尝试", func(task, _ int64) (agent.RunRecord, agent.RunRecord) {
			r := agent.RunRecord{Kind: "task", TaskID: task, Phase: "implement", SessionID: "s", StartedAt: t0}
			r2 := r
			r2.Resume = true
			return r, r2
		}, "task_id", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := usageTask(t, st, userID, repoID)
			prd := usagePRD(t, st, userID, repoID)
			a, b := tc.mk(task, prd)
			a.EndedAt, b.EndedAt = a.StartedAt.Add(time.Second), b.StartedAt.Add(time.Second)
			if err := st.RecordAgentRun(ctx, a); err != nil {
				t.Fatalf("第一次写 err=%v", err)
			}
			if err := st.RecordAgentRun(ctx, b); err != nil {
				t.Fatalf("第二次写 got err=%v, want nil（ON CONFLICT DO NOTHING）", err)
			}
			id := task
			if tc.filter == "prd_id" {
				id = prd
			}
			var n int
			if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM agent_runs WHERE `+tc.filter+`=$1`, id).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != tc.want {
				t.Errorf("行数 got %d, want %d", n, tc.want)
			}
		})
	}
}

// AC-4：墙钟回拨（EndedAt 早于 StartedAt）时钳位，不被 time_check 拒掉。
func TestRecordAgentRunClampsEndedAt(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, repoID := usageUser(t, st)
	t0 := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	cases := []struct {
		name   string
		res    *agent.Result
		wantMS int64
	}{
		{"res==nil 时 duration 0", nil, 0},
		{"有 res 时 duration 取 res.DurationMS", &agent.Result{Success: true, DurationMS: 4321}, 4321},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := usageTask(t, st, userID, repoID)
			if err := st.RecordAgentRun(ctx, agent.RunRecord{Kind: "task", TaskID: task, Phase: "implement",
				StartedAt: t0, EndedAt: t0.Add(-time.Second), Result: tc.res}); err != nil {
				t.Fatalf("写入 got err=%v, want nil", err)
			}
			var eq bool
			var ms int64
			if err := st.pool.QueryRow(ctx, `SELECT ended_at = started_at, duration_ms FROM agent_runs WHERE task_id=$1`, task).Scan(&eq, &ms); err != nil {
				t.Fatal(err)
			}
			if !eq || ms != tc.wantMS {
				t.Errorf("ended_at=started_at / duration got %v/%d, want true/%d", eq, ms, tc.wantMS)
			}
		})
	}
}

// AC-4：约束形态守卫，防止有人把 NULLS NOT DISTINCT「简化」掉。
func TestAgentRunsAttemptUniqIsNullsNotDistinct(t *testing.T) {
	st := testStore(t)
	var got bool
	if err := st.pool.QueryRow(context.Background(), `SELECT indnullsnotdistinct FROM pg_index i
		JOIN pg_class c ON c.oid=i.indexrelid WHERE c.relname='agent_runs_attempt_uniq'`).Scan(&got); err != nil {
		t.Fatalf("查约束失败: %v", err)
	}
	if !got {
		t.Errorf("indnullsnotdistinct got false, want true")
	}
}

// AC-11：模型名与明细按 rune 截断；超过 16 个模型合并成 (other) 且 token 对账。
func TestRecordTruncatesModelsByRune(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, repoID := usageUser(t, st)

	long := strings.Repeat("模", 64) + "x"
	var many []agent.ModelUsage
	for i := 0; i < 20; i++ {
		many = append(many, agent.ModelUsage{Model: fmt.Sprintf("m-%02d", i), InputTokens: int64(i + 1), OutputTokens: 2,
			CacheReadTokens: 3, CacheCreationTokens: 4, CostUSD: float64(i) / 100})
	}
	sumOf := func(ms []agent.ModelUsage) (in, out, cr, cc int64) {
		for _, m := range ms {
			in, out, cr, cc = in+m.InputTokens, out+m.OutputTokens, cr+m.CacheReadTokens, cc+m.CacheCreationTokens
		}
		return
	}
	cases := []struct {
		name      string
		models    []agent.ModelUsage
		wantModel int // length(model)
		wantLen   int // jsonb_array_length(models)
	}{
		{"64 个汉字 + x 的模型名截到 64 字符", []agent.ModelUsage{{Model: long, InputTokens: 1, CostUSD: 0.1}}, 64, 1},
		{"20 个模型 → 16 项、第 16 项为 (other)", many, 4, 16},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := usageTask(t, st, userID, repoID)
			in, out, cr, cc := sumOf(tc.models)
			record(t, st, agent.RunRecord{Kind: "task", TaskID: task, Phase: "implement", StartedAt: time.Now().Add(-time.Minute),
				Result: &agent.Result{Success: true, CostKnown: true, Usage: agent.Usage{Present: true, Models: tc.models,
					InputTokens: in, OutputTokens: out, CacheReadTokens: cr, CacheCreationTokens: cc}}})
			var modelLen, arrLen int
			var model string
			var rowIn, rowOut, rowCR, rowCC, elIn, elOut, elCR, elCC int64
			var last string
			if err := st.pool.QueryRow(ctx, `
				SELECT length(model), model, jsonb_array_length(models),
				       input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens,
				       (SELECT COALESCE(sum((e->>'inputTokens')::bigint),0) FROM jsonb_array_elements(models) e),
				       (SELECT COALESCE(sum((e->>'outputTokens')::bigint),0) FROM jsonb_array_elements(models) e),
				       (SELECT COALESCE(sum((e->>'cacheReadTokens')::bigint),0) FROM jsonb_array_elements(models) e),
				       (SELECT COALESCE(sum((e->>'cacheCreationTokens')::bigint),0) FROM jsonb_array_elements(models) e),
				       models->(jsonb_array_length(models)-1)->>'model'
				  FROM agent_runs WHERE task_id=$1`, task).Scan(&modelLen, &model, &arrLen,
				&rowIn, &rowOut, &rowCR, &rowCC, &elIn, &elOut, &elCR, &elCC, &last); err != nil {
				t.Fatalf("读行失败: %v", err)
			}
			if modelLen != tc.wantModel || !utf8.ValidString(model) {
				t.Errorf("length(model) got %d (valid=%v), want %d", modelLen, utf8.ValidString(model), tc.wantModel)
			}
			if arrLen != tc.wantLen {
				t.Errorf("jsonb_array_length(models) got %d, want %d", arrLen, tc.wantLen)
			}
			if tc.wantLen == 16 && last != "(other)" {
				t.Errorf("第 16 项 model got %q, want (other)", last)
			}
			if [4]int64{elIn, elOut, elCR, elCC} != [4]int64{rowIn, rowOut, rowCR, rowCC} {
				t.Errorf("明细之和 got %v, want 行上四列 %v", [4]int64{elIn, elOut, elCR, elCC}, [4]int64{rowIn, rowOut, rowCR, rowCC})
			}
		})
	}
}

// AC-7b（store 快路径）：定档回补只补 NULL，不动别的任务与 PRD 行。
func TestSetAgentRunTierOnlyFillsNull(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, repoID := usageUser(t, st)
	task := usageTask(t, st, userID, repoID)
	other := usageTask(t, st, userID, repoID)
	prd := usagePRD(t, st, userID, repoID)
	s := newSeq(time.Now().Add(-time.Hour))

	record(t, st, agent.RunRecord{Kind: "task", TaskID: task, Phase: "triage", StartedAt: s.next()})
	record(t, st, agent.RunRecord{Kind: "task", TaskID: task, Phase: "implement", VerifyTier: "light", StartedAt: s.next()})
	record(t, st, agent.RunRecord{Kind: "task", TaskID: task, Phase: "fix-1", StartedAt: s.next()})
	record(t, st, agent.RunRecord{Kind: "task", TaskID: other, Phase: "triage", StartedAt: s.next()})
	record(t, st, agent.RunRecord{Kind: "prd", PRDID: prd, Phase: "plan", StartedAt: s.next()})

	if err := st.SetAgentRunTier(ctx, task, "heavy"); err != nil {
		t.Fatalf("SetAgentRunTier: %v", err)
	}
	read := func(where string, id int64) []string {
		rows, err := st.pool.Query(ctx, `SELECT COALESCE(verify_tier,'NULL') FROM agent_runs WHERE `+where+`=$1 ORDER BY started_at`, id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			out = append(out, s)
		}
		return out
	}
	cases := []struct {
		name  string
		where string
		id    int64
		want  string
	}{
		{"本任务 NULL/light/NULL → heavy/light/heavy", "task_id", task, "heavy,light,heavy"},
		{"另一个任务的 NULL 行不动", "task_id", other, "NULL"},
		{"PRD 行不动", "prd_id", prd, "NULL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(read(tc.where, tc.id), ","); got != tc.want {
				t.Errorf("verify_tier got %s, want %s", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------- 迁移

// AC-6：0040 的两条索引存在且有效。
func TestAgentRunsIndexesValid(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	cases := []struct {
		name string
		sql  string
	}{
		{"pg_indexes 里有两条", `SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND indexname IN ('agent_runs_user_time','agent_runs_repo_time')`},
		{"两条都 indisvalid", `SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
			WHERE c.relname IN ('agent_runs_user_time','agent_runs_repo_time') AND i.indisvalid`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var n int
			if err := st.pool.QueryRow(ctx, tc.sql).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 2 {
				t.Errorf("got %d, want 2", n)
			}
		})
	}
}

const backfillUpFile, backfillDownFile = "0039_agent_runs_backfill.up.sql", "0039_agent_runs_backfill.down.sql"

// backfillUpSQL 读 0039 的 up（与 lathe migrate 同一份 embed 文本，不复制）。
//
// LATHE_F03_BACKFILL_VARIANT=v2|v3 时改读 testdata 里第二、三版的错误 SQL ——
// 只用于给 AC-6 产出红证据（docs/features/usage-stats.md），不进 migrations/。
func backfillUpSQL(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("LATHE_F03_BACKFILL_VARIANT"); v != "" {
		b, err := os.ReadFile(filepath.Join("testdata", "0039_backfill_"+v+".sql"))
		if err != nil {
			t.Fatalf("读错误版本 %s 失败: %v", v, err)
		}
		return string(b)
	}
	b, err := migrations.FS.ReadFile(backfillUpFile)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func execBackfill(t *testing.T, st *Store, up bool) {
	t.Helper()
	sql := backfillUpSQL(t)
	if !up {
		b, err := migrations.FS.ReadFile(backfillDownFile)
		if err != nil {
			t.Fatal(err)
		}
		sql = string(b)
	}
	if _, err := st.pool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("执行 0039 %v 失败: %v", map[bool]string{true: "up", false: "down"}[up], err)
	}
}

func insertTaskResult(t *testing.T, st *Store, taskID int64, phase string, cost float64, durMS int64, at time.Time) int64 {
	t.Helper()
	var id int64
	if err := st.pool.QueryRow(context.Background(), `
		INSERT INTO agent_events (task_id, phase, kind, payload, at)
		VALUES ($1, $2, 'result', jsonb_build_object('costUsd', $3::float8, 'durationMs', $4::bigint,
		        'numTurns', 2, 'subtype', 'success', 'isError', false), $5) RETURNING id`,
		taskID, phase, cost, durMS, at).Scan(&id); err != nil {
		t.Fatalf("插 agent_events result 失败: %v", err)
	}
	return id
}

func insertPRDResult(t *testing.T, st *Store, prdID int64, phase string, cost float64, durMS int64, at time.Time) {
	t.Helper()
	if _, err := st.pool.Exec(context.Background(), `
		INSERT INTO prd_events (prd_id, phase, kind, payload, at)
		VALUES ($1, $2, 'result', jsonb_build_object('costUsd', $3::float8, 'durationMs', $4::bigint,
		        'numTurns', 1, 'subtype', 'success', 'isError', false), $5)`,
		prdID, phase, cost, durMS, at); err != nil {
		t.Fatalf("插 prd_events result 失败: %v", err)
	}
}

// AC-6：0039 回填的阶段桶与 UsageBucket(phase,false) 同构；回填行的时间、归属正确；
// 不经 down 再跑一次行数不变。
func TestBackfillBucketMatchesUsageBucket(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, repoID := usageUser(t, st)
	task := usageTask(t, st, userID, repoID)
	prd := usagePRD(t, st, userID, repoID)
	base := time.Now().Add(-2 * time.Hour).Truncate(time.Microsecond)

	taskPhases := []string{"triage", "implement", "fix-1", "fix-12", "verify", "push", "review"}
	for i, ph := range taskPhases {
		insertTaskResult(t, st, task, ph, 0.01*float64(i+1), 1500, base.Add(time.Duration(i)*time.Minute))
	}
	for i, ph := range []string{"plan", "plan-review"} {
		insertPRDResult(t, st, prd, ph, 0.02, 2500, base.Add(time.Duration(i)*time.Minute))
	}
	execBackfill(t, st, true)

	type row struct {
		phase, bucket      string
		review, backfill   bool
		user, repo         int64
		endEq, startOffset bool
	}
	readRows := func(where string, id int64) map[string]row {
		rows, err := st.pool.Query(ctx, `
			SELECT r.phase, r.bucket, r.review_round, r.backfill, r.user_id, r.repo_id,
			       r.ended_at = COALESCE(ae.at, pe.at),
			       r.started_at = r.ended_at - make_interval(secs => r.duration_ms/1000.0)
			  FROM agent_runs r
			  LEFT JOIN agent_events ae ON r.task_id = ae.task_id AND ae.kind='result' AND ae.phase = r.phase
			  LEFT JOIN prd_events pe ON r.prd_id = pe.prd_id AND pe.kind='result' AND pe.phase = r.phase
			 WHERE r.`+where+`=$1`, id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]row{}
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.phase, &r.bucket, &r.review, &r.backfill, &r.user, &r.repo, &r.endEq, &r.startOffset); err != nil {
				t.Fatal(err)
			}
			out[r.phase] = r
		}
		return out
	}
	got := readRows("task_id", task)
	for k, v := range readRows("prd_id", prd) {
		got[k] = v
	}
	for _, ph := range append(taskPhases, "plan", "plan-review") {
		t.Run("phase="+ph, func(t *testing.T) {
			r, ok := got[ph]
			if !ok {
				t.Fatalf("没有回填行")
			}
			if want := UsageBucket(ph, false); r.bucket != want {
				t.Errorf("bucket got %q, want %q（与 UsageBucket 同构）", r.bucket, want)
			}
			if r.review || !r.backfill || r.user != userID || r.repo != repoID || !r.endEq || !r.startOffset {
				t.Errorf("review/backfill/user/repo/ended=at/started=at-dur got %v/%v/%d/%d/%v/%v, want false/true/%d/%d/true/true",
					r.review, r.backfill, r.user, r.repo, r.endEq, r.startOffset, userID, repoID)
			}
		})
	}

	count := func() int {
		var n int
		_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM agent_runs WHERE task_id=$1 OR prd_id=$2`, task, prd).Scan(&n)
		return n
	}
	before := count()
	execBackfill(t, st, true)
	if after := count(); after != before {
		t.Errorf("不经 down 再跑 0039 行数 got %d, want %d（回填行之间幂等）", after, before)
	}
}

// AC-6：down/up 往返不重复计账；截止条件只比库时钟。
func TestBackfillRoundTripNoDoubleCount(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, repoID := usageUser(t, st)

	dbNow := func() time.Time {
		var n time.Time
		if err := st.pool.QueryRow(ctx, `SELECT now()`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	live := func(task int64, started, ended time.Time, cost float64) {
		record(t, st, agent.RunRecord{Kind: "task", TaskID: task, Phase: "implement", SessionID: "live",
			StartedAt: started, EndedAt: ended, Result: paid(cost)})
	}

	type out struct {
		count    int
		sum      float64
		backfill int
	}
	read := func(where string, id int64) out {
		var o out
		if err := st.pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(cost_usd),0)::float8, count(*) FILTER (WHERE backfill)
			FROM agent_runs WHERE `+where+`=$1`, id).Scan(&o.count, &o.sum, &o.backfill); err != nil {
			t.Fatal(err)
		}
		return o
	}

	cases := []struct {
		name string
		// setup 造历史事件（0039 首次 up 之前）；goLive 模拟上线（首次 up 之后）。
		// 返回断言用的目标列与 id，以及期望。
		run func(t *testing.T) (where string, id int64, want out, check func(t *testing.T))
	}{
		{"任务：历史 1 条 + 上线后 1 次运行", func(t *testing.T) (string, int64, out, func(*testing.T)) {
			task := usageTask(t, st, userID, repoID)
			n := dbNow()
			insertTaskResult(t, st, task, "implement", 0.10, 1000, n.Add(-time.Hour))
			execBackfill(t, st, true)
			s, e := n.Add(-30*time.Second), n
			live(task, s, e, 0.25)
			insertTaskResult(t, st, task, "implement", 0.25, 30000, e)
			return "task_id", task, out{2, 0.35, 1}, nil
		}},
		{"任务：上线后多次运行", func(t *testing.T) (string, int64, out, func(*testing.T)) {
			task := usageTask(t, st, userID, repoID)
			execBackfill(t, st, true)
			n := dbNow()
			live(task, n.Add(-90*time.Second), n.Add(-60*time.Second), 0.20)
			insertTaskResult(t, st, task, "implement", 0.20, 30000, n.Add(-60*time.Second))
			live(task, n.Add(-30*time.Second), n, 0.30)
			insertTaskResult(t, st, task, "implement", 0.30, 30000, n)
			return "task_id", task, out{2, 0.50, 0}, nil
		}},
		{"PRD：同形", func(t *testing.T) (string, int64, out, func(*testing.T)) {
			prd := usagePRD(t, st, userID, repoID)
			n := dbNow()
			insertPRDResult(t, st, prd, "plan", 0.10, 1000, n.Add(-time.Hour))
			execBackfill(t, st, true)
			record(t, st, agent.RunRecord{Kind: "prd", PRDID: prd, Phase: "plan", SessionID: "live",
				StartedAt: n.Add(-30 * time.Second), EndedAt: n, Result: paid(0.25)})
			insertPRDResult(t, st, prd, "plan", 0.25, 30000, n)
			return "prd_id", prd, out{2, 0.35, 1}, nil
		}},
		{"另一目标只有历史", func(t *testing.T) (string, int64, out, func(*testing.T)) {
			task := usageTask(t, st, userID, repoID)
			insertTaskResult(t, st, task, "implement", 0.10, 1000, dbNow().Add(-time.Hour))
			execBackfill(t, st, true)
			return "task_id", task, out{1, 0.10, 1}, nil
		}},
		{"时钟偏差在运行时长内（节点快 5s、运行 30s）", func(t *testing.T) (string, int64, out, func(*testing.T)) {
			task := usageTask(t, st, userID, repoID)
			n := dbNow()
			insertTaskResult(t, st, task, "implement", 0.10, 1000, n.Add(-2*time.Hour))
			execBackfill(t, st, true)
			live(task, n.Add(5*time.Second-30*time.Second), n.Add(5*time.Second), 0.25)
			insertTaskResult(t, st, task, "implement", 0.25, 30000, n)
			return "task_id", task, out{2, 0.35, 1}, nil
		}},
		{"节点时钟快出的量大于运行时长（快 120s、运行 30s）", func(t *testing.T) (string, int64, out, func(*testing.T)) {
			task := usageTask(t, st, userID, repoID)
			n := dbNow()
			insertTaskResult(t, st, task, "implement", 0.10, 1000, n.Add(-2*time.Hour))
			execBackfill(t, st, true)
			live(task, n.Add(90*time.Second), n.Add(120*time.Second), 0.25)
			insertTaskResult(t, st, task, "implement", 0.25, 30000, n.Add(-500*time.Millisecond))
			return "task_id", task, out{2, 0.35, 1}, nil
		}},
		{"节点时钟慢 1h", func(t *testing.T) (string, int64, out, func(*testing.T)) {
			task := usageTask(t, st, userID, repoID)
			n := dbNow()
			execBackfill(t, st, true)
			live(task, n.Add(-time.Hour-30*time.Second), n.Add(-time.Hour), 0.25)
			insertTaskResult(t, st, task, "implement", 0.25, 30000, n.Add(-500*time.Millisecond))
			insertTaskResult(t, st, task, "implement", 0.10, 1000, n.Add(-30*time.Minute))
			return "task_id", task, out{2, 0.35, 1}, nil
		}},
		{"容差边界：首条实时运行开始前 4min 的不补、6min 的补", func(t *testing.T) (string, int64, out, func(*testing.T)) {
			task := usageTask(t, st, userID, repoID)
			n := dbNow()
			start := n.Add(-30 * time.Second)
			in4 := insertTaskResult(t, st, task, "implement", 0.04, 0, start.Add(-4*time.Minute))
			in6 := insertTaskResult(t, st, task, "implement", 0.06, 0, start.Add(-6*time.Minute))
			execBackfill(t, st, true)
			live(task, start, n, 0.25)
			insertTaskResult(t, st, task, "implement", 0.25, 30000, n)
			check := func(t *testing.T) {
				rows, err := st.pool.Query(ctx, `SELECT ae.id FROM agent_runs r JOIN agent_events ae
					ON ae.task_id = r.task_id AND ae.kind='result' AND ae.at = r.ended_at
					WHERE r.task_id=$1 AND r.backfill ORDER BY ae.id`, task)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				var got []int64
				for rows.Next() {
					var id int64
					_ = rows.Scan(&id)
					got = append(got, id)
				}
				if fmt.Sprint(got) != fmt.Sprint([]int64{in6}) {
					t.Errorf("回填的事件 id got %v, want [%d]（%d 在容差内不补）", got, in6, in4)
				}
			}
			return "task_id", task, out{2, 0.31, 1}, check
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			where, id, want, check := tc.run(t)
			execBackfill(t, st, false)
			execBackfill(t, st, true)
			got := read(where, id)
			if got.count != want.count || !approx(got.sum, want.sum) || got.backfill != want.backfill {
				t.Errorf("count/sum/backfill got %d/%.2f/%d, want %d/%.2f/%d", got.count, got.sum, got.backfill, want.count, want.sum, want.backfill)
			}
			if check != nil {
				check(t)
			}
		})
	}
}

// ---------------------------------------------------------------- 聚合

// AC-7：按阶段 / 仓库 / 档位 / 天 / 时间聚合。
func TestUsageStatsAggregates(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, repoA := usageUser(t, st)
	repoB := usageRepo(t, st, userID)
	otherUser, otherRepo := usageUser(t, st)

	light := usageTask(t, st, userID, repoA)
	heavy := usageTask(t, st, userID, repoB)
	none := usageTask(t, st, userID, repoA)
	prd := usagePRD(t, st, userID, repoA)
	foreign := usageTask(t, st, otherUser, otherRepo)

	// 按真实时序：triage/implement 以未定档写 → SetAgentRunTier → fix-1 带档 → recommend。
	s := newSeq(time.Now().Add(-48 * time.Hour))
	flow := func(task int64, tier string, costs [4]float64) {
		record(t, st, agent.RunRecord{Kind: "task", TaskID: task, Phase: "triage", StartedAt: s.next(), Result: paid(costs[0])})
		record(t, st, agent.RunRecord{Kind: "task", TaskID: task, Phase: "implement", StartedAt: s.next(), Result: paid(costs[1])})
		if err := st.SetAgentRunTier(ctx, task, tier); err != nil {
			t.Fatal(err)
		}
		if _, err := st.pool.Exec(ctx, `UPDATE tasks SET verify_tier=$2 WHERE id=$1`, task, tier); err != nil {
			t.Fatal(err)
		}
		record(t, st, agent.RunRecord{Kind: "task", TaskID: task, Phase: "fix-1", VerifyTier: tier, StartedAt: s.next(), Result: paid(costs[2])})
		record(t, st, agent.RunRecord{Kind: "preview", TaskID: task, Phase: "recommend", StartedAt: s.next(), Result: paid(costs[3])})
	}
	flow(light, "light", [4]float64{0.01, 0.10, 0.05, 0.02})
	flow(heavy, "heavy", [4]float64{0.02, 0.40, 0.20, 0.03})
	record(t, st, agent.RunRecord{Kind: "task", TaskID: none, Phase: "triage", StartedAt: s.next(), Result: paid(0.01)})
	record(t, st, agent.RunRecord{Kind: "prd", PRDID: prd, Phase: "plan", StartedAt: s.next(), Result: paid(0.30)})
	record(t, st, agent.RunRecord{Kind: "task", TaskID: foreign, Phase: "implement", StartedAt: s.next(), Result: paid(9.99)})

	q := func(mod func(*UsageQuery)) *UsageStats {
		t.Helper()
		qq := wideQuery("phase")
		mod(&qq)
		us, err := st.UsageStats(ctx, userID, qq)
		if err != nil {
			t.Fatalf("UsageStats: %v", err)
		}
		return us
	}

	t.Run("group=phase：byBucket implement 含 fix-1、byGroup 键是原始 phase", func(t *testing.T) {
		us := q(func(*UsageQuery) {})
		var impl *UsageGroup
		for i := range us.ByBucket {
			if us.ByBucket[i].Key == "implement" {
				impl = &us.ByBucket[i]
			}
		}
		if impl == nil || impl.Runs != 4 || !approx(*impl.CostUSD, 0.10+0.05+0.40+0.20) {
			t.Fatalf("byBucket implement got %+v, want runs 4 cost 0.75", impl)
		}
		fix := findGroup(us.ByGroup, "fix-1")
		if fix == nil || fix.Bucket != "implement" || fix.Runs != 2 {
			t.Errorf("byGroup fix-1 got %+v, want bucket implement runs 2", fix)
		}
		if got, want := strings.Join(groupKeys(us.ByGroup), ","), "implement,plan,fix-1,recommend,triage"; got != want {
			t.Errorf("byGroup 顺序 got %s, want %s（costUsd 降序）", got, want)
		}
		if us.Totals.Runs != 10 {
			t.Errorf("totals.runs got %d, want 10（不含他人）", us.Totals.Runs)
		}
	})
	t.Run("group=repo：两个仓库各自之和", func(t *testing.T) {
		us := q(func(qq *UsageQuery) { qq.Group = "repo" })
		a, b := findGroup(us.ByGroup, fmt.Sprint(repoA)), findGroup(us.ByGroup, fmt.Sprint(repoB))
		if a == nil || b == nil {
			t.Fatalf("byGroup got %v, want 两个仓库", groupKeys(us.ByGroup))
		}
		if a.Runs != 6 || !approx(*a.CostUSD, 0.18+0.01+0.30) || b.Runs != 4 || !approx(*b.CostUSD, 0.65) {
			t.Errorf("repoA runs/cost got %d/%s want 6/0.49；repoB got %d/%s want 4/0.65", a.Runs, costOf(a.CostUSD), b.Runs, costOf(b.CostUSD))
		}
		if a.ProviderRepo == nil || !strings.HasPrefix(*a.ProviderRepo, "acme/usage-") || a.RepoID != repoA {
			t.Errorf("repoA 标签 got repoId=%d providerRepo=%v", a.RepoID, a.ProviderRepo)
		}
	})
	checkTier := func(t *testing.T, us *UsageStats) {
		t.Helper()
		if got := strings.Join(groupKeys(us.ByGroup), ","); got != "light,heavy,none" {
			t.Errorf("tier 顺序 got %s, want light,heavy,none", got)
		}
		cases := []struct {
			key  string
			runs int
			cost float64
		}{{"light", 4, 0.18}, {"heavy", 4, 0.65}, {"none", 2, 0.31}}
		for _, c := range cases {
			g := findGroup(us.ByGroup, c.key)
			if g == nil || g.Runs != c.runs || !approx(*g.CostUSD, c.cost) {
				t.Errorf("tier %s got %+v, want runs %d cost %.2f", c.key, g, c.runs, c.cost)
			}
		}
	}
	t.Run("group=tier：按运行快照分，triage 经回补计入所定档", func(t *testing.T) {
		checkTier(t, q(func(qq *UsageQuery) { qq.Group = "tier" }))
	})
	t.Run("group=tier：改任务行档位后结果不变（不读 tasks.verify_tier）", func(t *testing.T) {
		if _, err := st.pool.Exec(ctx, `UPDATE tasks SET verify_tier='light' WHERE id=$1`, heavy); err != nil {
			t.Fatal(err)
		}
		checkTier(t, q(func(qq *UsageQuery) { qq.Group = "tier" }))
	})
	t.Run("tier=heavy 过滤后 totals.runs == 4", func(t *testing.T) {
		if us := q(func(qq *UsageQuery) { qq.Tier = "heavy" }); us.Totals.Runs != 4 {
			t.Errorf("totals.runs got %d, want 4", us.Totals.Runs)
		}
		if us := q(func(qq *UsageQuery) { qq.Tier = "none" }); us.Totals.Runs != 2 {
			t.Errorf("tier=none totals.runs got %d, want 2", us.Totals.Runs)
		}
	})
	t.Run("repo 过滤", func(t *testing.T) {
		if us := q(func(qq *UsageQuery) { qq.RepoID = repoB }); us.Totals.Runs != 4 {
			t.Errorf("repo=B totals.runs got %d, want 4", us.Totals.Runs)
		}
	})
	t.Run("group=day：按 started_at 的 UTC 日升序", func(t *testing.T) {
		us := q(func(qq *UsageQuery) { qq.Group = "day" })
		keys := groupKeys(us.ByGroup)
		if !sort.StringsAreSorted(keys) || len(keys) == 0 {
			t.Errorf("byGroup day got %v, want 非空升序", keys)
		}
		total := 0
		for _, d := range us.ByDay {
			total += d.Runs
			if _, err := time.Parse("2006-01-02", d.Key); err != nil {
				t.Errorf("day key %q 不是 YYYY-MM-DD", d.Key)
			}
		}
		if total != us.Totals.Runs {
			t.Errorf("Σ byDay.runs got %d, want %d", total, us.Totals.Runs)
		}
	})
	t.Run("from/to 卡边界：to 早于最早一行 → 空且集合是 []", func(t *testing.T) {
		var earliest time.Time
		if err := st.pool.QueryRow(ctx, `SELECT min(started_at) FROM agent_runs WHERE user_id=$1`, userID).Scan(&earliest); err != nil {
			t.Fatal(err)
		}
		us := q(func(qq *UsageQuery) { qq.To = earliest.Add(-time.Second) })
		if us.Totals.Runs != 0 || us.ByBucket == nil || len(us.ByBucket) != 0 || us.ByModel == nil || us.ByDay == nil || us.ByGroup == nil {
			t.Errorf("got runs=%d byBucket=%v byModel=%v byDay=%v, want 0 与非 nil 空集合", us.Totals.Runs, us.ByBucket, us.ByModel, us.ByDay)
		}
	})
}

// AC-7-①：group=task 的排序全序、limit 截断与 groupTotal。
func TestUsageStatsGroupTaskLimit(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, repoID := usageUser(t, st)
	var tk [6]int64
	for i := 1; i <= 5; i++ {
		tk[i] = usageTask(t, st, userID, repoID)
	}
	p1 := usagePRD(t, st, userID, repoID)
	s := newSeq(time.Now().Add(-24 * time.Hour))
	rec := func(task int64, res *agent.Result) {
		record(t, st, agent.RunRecord{Kind: "task", TaskID: task, Phase: "implement", StartedAt: s.next(), Result: res})
	}
	rec(tk[1], paid(0.50))
	rec(tk[2], paid(0.15))
	rec(tk[2], paid(0.15))
	rec(tk[3], paid(0.30))
	rec(tk[4], &agent.Result{Success: true, CostKnown: false})
	rec(tk[5], paid(0.10))
	record(t, st, agent.RunRecord{Kind: "prd", PRDID: p1, Phase: "plan", StartedAt: s.next(), Result: paid(0.20)})

	key := func(i int) string { return fmt.Sprintf("task:%d", tk[i]) }
	all := []string{key(1), key(2), key(3), fmt.Sprintf("prd:%d", p1), key(5), key(4)}
	cases := []struct {
		name      string
		group     string
		limit     int
		wantKeys  []string
		wantTotal int
	}{
		{"默认 limit", "task", 20, all, 6},
		{"截断", "task", 3, all[:3], 6},
		{"上限值", "task", 200, all, 6},
		{"limit 对其它 group 无效", "phase", 1, []string{"implement", "plan"}, 0},
	}
	var full *UsageStats
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			qq := wideQuery(tc.group)
			qq.Limit = tc.limit
			us, err := st.UsageStats(ctx, userID, qq)
			if err != nil {
				t.Fatal(err)
			}
			if got := groupKeys(us.ByGroup); fmt.Sprint(got) != fmt.Sprint(tc.wantKeys) {
				t.Errorf("byGroup keys got %v, want %v", got, tc.wantKeys)
			}
			gotTotal := 0
			if us.GroupTotal != nil {
				gotTotal = *us.GroupTotal
			}
			if gotTotal != tc.wantTotal {
				t.Errorf("groupTotal got %v, want %d", us.GroupTotal, tc.wantTotal)
			}
			if us.Totals.Runs != 7 {
				t.Errorf("totals.runs got %d, want 7（不受 limit 影响）", us.Totals.Runs)
			}
			if tc.group == "task" && tc.limit == 20 {
				full = us
			}
		})
	}
	if full != nil {
		p := findGroup(full.ByGroup, fmt.Sprintf("prd:%d", p1))
		if p == nil || p.Kind != "prd" || p.PRDID != p1 {
			t.Errorf("prd 项 got %+v, want kind=prd prdId=%d", p, p1)
		}
		t4 := findGroup(full.ByGroup, key(4))
		if t4 == nil || t4.CostUSD != nil || t4.CostKnown != 0 {
			t.Errorf("T4 got %+v, want costUsd=nil costKnown=0", t4)
		}
	}

	t.Run("默认 limit=20：再造 21 个任务 → 20 项、groupTotal=27", func(t *testing.T) {
		for i := 0; i < 21; i++ {
			rec(usageTask(t, st, userID, repoID), paid(0.01))
		}
		qq := wideQuery("task")
		qq.Limit = 0 // store 侧 0 = 默认 20
		us, err := st.UsageStats(ctx, userID, qq)
		if err != nil {
			t.Fatal(err)
		}
		if len(us.ByGroup) != 20 || us.GroupTotal == nil || *us.GroupTotal != 27 {
			t.Errorf("len(byGroup)/groupTotal got %d/%v, want 20/27", len(us.ByGroup), us.GroupTotal)
		}
	})
}

// AC-7-②：byModel 展开 models，无明细的行归 (unknown)，token 与 totals 精确对账。
func TestUsageStatsByModel(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	opus := agent.ModelUsage{Model: "opus", InputTokens: 10, OutputTokens: 2, CostUSD: 0.1}
	haiku := agent.ModelUsage{Model: "haiku", InputTokens: 5, CostUSD: 0.01}
	withModels := func(ms ...agent.ModelUsage) *agent.Result {
		u := agent.Usage{Present: true, Models: ms}
		var cost float64
		for _, m := range ms {
			u.InputTokens += m.InputTokens
			u.OutputTokens += m.OutputTokens
			u.CacheReadTokens += m.CacheReadTokens
			u.CacheCreationTokens += m.CacheCreationTokens
			cost += m.CostUSD
		}
		return &agent.Result{Success: true, CostKnown: true, CostUSD: cost, Usage: u}
	}
	single := func(t *testing.T, st *Store, task int64, s *seq) {
		record(t, st, agent.RunRecord{Kind: "task", TaskID: task, Phase: "implement", StartedAt: s.next(), Result: withModels(opus)})
	}
	two := func(t *testing.T, st *Store, task int64, s *seq) {
		record(t, st, agent.RunRecord{Kind: "task", TaskID: task, Phase: "implement", StartedAt: s.next(), Result: withModels(opus, haiku)})
	}
	unknown := func(t *testing.T, st *Store, task int64, s *seq) {
		record(t, st, agent.RunRecord{Kind: "task", TaskID: task, Phase: "implement", StartedAt: s.next(),
			Result: &agent.Result{Success: true, CostKnown: false, Usage: agent.Usage{Present: true, InputTokens: 11}}})
		record(t, st, agent.RunRecord{Kind: "task", TaskID: task, Phase: "implement", StartedAt: s.next(),
			Err: fmt.Errorf("%w（…）", agent.ErrIdle)})
		if _, err := st.pool.Exec(ctx, `INSERT INTO agent_runs (task_id, user_id, repo_id, phase, bucket, run_kind, cost_usd, status, started_at, ended_at, backfill)
			SELECT id, user_id, repo_id, 'implement', 'implement', 'task', 0.05, 'success', $2, $2, true FROM tasks WHERE id=$1`, task, s.next()); err != nil {
			t.Fatal(err)
		}
	}
	var twenty []agent.ModelUsage
	for i := 0; i < 20; i++ {
		twenty = append(twenty, agent.ModelUsage{Model: fmt.Sprintf("m%02d", i), InputTokens: int64(i + 1), OutputTokens: 1, CacheReadTokens: 2, CacheCreationTokens: 3, CostUSD: float64(i) / 1000})
	}

	type modelWant struct {
		key                       string
		runs                      int
		input                     int64
		cost                      *float64
		costKnown, tokensKnown    int
		checkCost, checkKnownCols bool
	}
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name     string
		seed     []func(*testing.T, *Store, int64, *seq)
		want     []modelWant
		wantKeys int
		runsGT   bool // Σ runs > totals.runs
	}{
		{"单模型", []func(*testing.T, *Store, int64, *seq){single},
			[]modelWant{{key: "opus", runs: 1, input: 10, cost: f(0.1), checkCost: true}}, 1, false},
		{"一行两个模型", []func(*testing.T, *Store, int64, *seq){two},
			[]modelWant{{key: "opus", runs: 1}, {key: "haiku", runs: 1}}, 2, true},
		{"无明细的行归 (unknown)", []func(*testing.T, *Store, int64, *seq){unknown},
			[]modelWant{{key: "(unknown)", runs: 3, input: 11, cost: f(0.05), costKnown: 1, tokensKnown: 1, checkCost: true, checkKnownCols: true}}, 1, false},
		{"对账：三例并集", []func(*testing.T, *Store, int64, *seq){single, two, unknown}, nil, 3, true},
		{"截断 (other)：1 行 20 个模型", []func(*testing.T, *Store, int64, *seq){func(t *testing.T, st *Store, task int64, s *seq) {
			record(t, st, agent.RunRecord{Kind: "task", TaskID: task, Phase: "implement", StartedAt: s.next(), Result: withModels(twenty...)})
		}}, []modelWant{{key: "(other)", runs: 1}}, 16, true},
		{"空", nil, nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			userID, repoID := usageUser(t, st)
			task := usageTask(t, st, userID, repoID)
			s := newSeq(time.Now().Add(-time.Hour))
			for _, fn := range tc.seed {
				fn(t, st, task, s)
			}
			us, err := st.UsageStats(ctx, userID, wideQuery("phase"))
			if err != nil {
				t.Fatal(err)
			}
			if us.ByModel == nil || len(us.ByModel) != tc.wantKeys {
				t.Fatalf("byModel 项数 got %d (%v), want %d", len(us.ByModel), us.ByModel, tc.wantKeys)
			}
			var sIn, sOut, sCR, sCC int64
			sRuns := 0
			for _, m := range us.ByModel {
				sIn, sOut, sCR, sCC = sIn+m.InputTokens, sOut+m.OutputTokens, sCR+m.CacheReadTokens, sCC+m.CacheCreationTokens
				sRuns += m.Runs
			}
			tt := us.Totals
			if [4]int64{sIn, sOut, sCR, sCC} != [4]int64{tt.InputTokens, tt.OutputTokens, tt.CacheReadTokens, tt.CacheCreationTokens} {
				t.Errorf("Σ byModel tokens got %v, want totals %v（精确相等）", [4]int64{sIn, sOut, sCR, sCC},
					[4]int64{tt.InputTokens, tt.OutputTokens, tt.CacheReadTokens, tt.CacheCreationTokens})
			}
			if tc.runsGT && sRuns <= tt.Runs {
				t.Errorf("Σ byModel.runs got %d, want > totals.runs %d", sRuns, tt.Runs)
			}
			for _, w := range tc.want {
				var m *UsageModel
				for i := range us.ByModel {
					if us.ByModel[i].Key == w.key {
						m = &us.ByModel[i]
					}
				}
				if m == nil {
					t.Fatalf("byModel 缺 %s，got %+v", w.key, us.ByModel)
				}
				if m.Runs != w.runs {
					t.Errorf("%s runs got %d, want %d", w.key, m.Runs, w.runs)
				}
				if w.input != 0 && m.InputTokens != w.input {
					t.Errorf("%s inputTokens got %d, want %d", w.key, m.InputTokens, w.input)
				}
				if w.checkCost && (m.CostUSD == nil || !approx(*m.CostUSD, *w.cost)) {
					t.Errorf("%s costUsd got %s, want %.4f", w.key, costOf(m.CostUSD), *w.cost)
				}
				if w.checkKnownCols && (m.CostKnown != w.costKnown || m.TokensKnown != w.tokensKnown) {
					t.Errorf("%s costKnown/tokensKnown got %d/%d, want %d/%d", w.key, m.CostKnown, m.TokensKnown, w.costKnown, w.tokensKnown)
				}
			}
		})
	}
}

// AC-7-④：bucket= 按 agent_runs.bucket 等值过滤，各集合同步只含过滤后的行。
func TestUsageStatsBucketFilter(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, repoID := usageUser(t, st)
	task := usageTask(t, st, userID, repoID)
	heavyTask := usageTask(t, st, userID, repoID)
	prd := usagePRD(t, st, userID, repoID)
	s := newSeq(time.Now().Add(-time.Hour))
	r := func(task int64, phase string, review bool, tier string) {
		record(t, st, agent.RunRecord{Kind: "task", TaskID: task, Phase: phase, ReviewRound: review, VerifyTier: tier, StartedAt: s.next(), Result: paid(0.1)})
	}
	r(task, "triage", false, "")
	r(task, "implement", false, "")
	r(heavyTask, "implement", false, "heavy")
	r(task, "fix-1", false, "")
	r(task, "implement", true, "")
	record(t, st, agent.RunRecord{Kind: "prd", PRDID: prd, Phase: "plan", StartedAt: s.next(), Result: paid(0.1)})
	record(t, st, agent.RunRecord{Kind: "preview", TaskID: task, Phase: "recommend", StartedAt: s.next(), Result: paid(0.1)})

	cases := []struct {
		name      string
		bucket    string
		tier      string
		group     string
		wantRuns  int
		wantKeys  string // byBucket（group 非空时为 byGroup）的 key 集合，排序后逗号连接
		fromGroup bool
	}{
		{"bucket=implement 不含 review 轮", "implement", "", "phase", 3, "implement", false},
		{"bucket=review", "review", "", "phase", 1, "review", false},
		{"bucket=prd_plan", "prd_plan", "", "phase", 1, "prd_plan", false},
		{"bucket=verify 合法但无行", "verify", "", "phase", 0, "", false},
		{"bucket=implement&group=phase", "implement", "", "phase", 3, "fix-1,implement", true},
		{"bucket=implement&tier=heavy 是两个过滤的交集", "implement", "heavy", "phase", 1, "implement", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			qq := wideQuery(tc.group)
			qq.Bucket, qq.Tier = tc.bucket, tc.tier
			us, err := st.UsageStats(ctx, userID, qq)
			if err != nil {
				t.Fatal(err)
			}
			if us.Totals.Runs != tc.wantRuns {
				t.Errorf("totals.runs got %d, want %d", us.Totals.Runs, tc.wantRuns)
			}
			src := us.ByBucket
			if tc.fromGroup {
				src = us.ByGroup
			}
			keys := groupKeys(src)
			sort.Strings(keys)
			if got := strings.Join(keys, ","); got != tc.wantKeys {
				t.Errorf("keys got %q, want %q", got, tc.wantKeys)
			}
			dayRuns, modelRuns := 0, 0
			for _, d := range us.ByDay {
				dayRuns += d.Runs
			}
			for _, m := range us.ByModel {
				modelRuns += m.Runs
			}
			if dayRuns != us.Totals.Runs || modelRuns != us.Totals.Runs {
				t.Errorf("Σ byDay.runs / Σ byModel.runs got %d/%d, want %d", dayRuns, modelRuns, us.Totals.Runs)
			}
		})
	}
}

// 管理员视图：byUser 按 user_id 分组，Σ byUser.runs == totals.runs（同一集合）。
func TestAdminUsageStatsByUser(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	a, repoA := usageUser(t, st)
	b, repoB := usageUser(t, st)
	ta, tb := usageTask(t, st, a, repoA), usageTask(t, st, b, repoB)
	s := newSeq(time.Now().Add(-time.Hour))
	record(t, st, agent.RunRecord{Kind: "task", TaskID: ta, Phase: "implement", StartedAt: s.next(), Result: paid(0.30)})
	record(t, st, agent.RunRecord{Kind: "task", TaskID: ta, Phase: "fix-1", StartedAt: s.next(), Result: &agent.Result{Success: true}})
	record(t, st, agent.RunRecord{Kind: "task", TaskID: tb, Phase: "implement", StartedAt: s.next(), Result: paid(0.05)})

	us, err := st.AdminUsageStats(ctx, wideQuery("task"))
	if err != nil {
		t.Fatal(err)
	}
	sum := 0
	var gotA, gotB *UsageUser
	for i := range us.ByUser {
		sum += us.ByUser[i].Runs
		switch us.ByUser[i].UserID {
		case a:
			gotA = &us.ByUser[i]
		case b:
			gotB = &us.ByUser[i]
		}
	}
	if gotA == nil || gotA.Runs != 2 || gotA.CostKnown != 1 || !approx(*gotA.CostUSD, 0.30) {
		t.Errorf("byUser A got %+v, want runs 2 costKnown 1 cost 0.30", gotA)
	}
	if gotB == nil || gotB.Runs != 1 || !approx(*gotB.CostUSD, 0.05) {
		t.Errorf("byUser B got %+v, want runs 1 cost 0.05", gotB)
	}
	if sum != us.Totals.Runs {
		t.Errorf("Σ byUser.runs got %d, want totals.runs %d", sum, us.Totals.Runs)
	}
}
