package store

// usage.go agent 用量账本 agent_runs 的写入与聚合（f03，docs/features/usage-stats.md）。
//
// 与 cost.go 的关系：cost.go 从 agent_events 的 result 事件求和（T5 验收物，
// 保持不动）；这里是完整账本 —— 每次 agent 进程运行一行，含失败/被杀的运行、
// PRD 规划与预览推荐、token 四项。两者口径不同，不合并。
//
// 多租户隔离：普通查询的唯一过滤入口是 usageScope(userID)，没有「userID=0
// 表示全部」的魔法值；管理员全局视图走独立的 AdminUsageStats，它结构上不做
// 任何标签 JOIN、返回类型里也没有标签字段。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
)

// ErrAgentRunTargetGone 表示记账时目标任务/PRD 行已不存在（被删了）。
var ErrAgentRunTargetGone = errors.New("store: 记账目标已不存在")

const (
	// usageMaxModels 是 models 明细的上限；超出时前 15 个照留、其余合并成 (other)。
	usageMaxModels = 16
	// usageTextRunes 是 model / subtype 的 rune 上限（AGENTS.md §9：按 rune 截断）。
	usageTextRunes = 64
	// UsageDefaultLimit 是 group=task 的默认组数。
	UsageDefaultLimit = 20
	// UsageMaxLimit 是 limit 参数的上限。
	UsageMaxLimit = 200
	// usageOtherModel / usageUnknownModel 是 byModel 的两个保留键（真实模型名不含括号）。
	usageOtherModel   = "(other)"
	usageUnknownModel = "(unknown)"
)

var fixPhase = regexp.MustCompile(`^fix-[0-9]+$`)

// UsageBuckets 是全部阶段桶（与 agent_runs_bucket_check 一致），bucket= 参数据此校验。
var UsageBuckets = []string{"triage", "implement", "review", "verify", "push", "prd_plan", "prd_review", "preview", "other"}

// UsageGroups 是 group= 的合法取值。
var UsageGroups = []string{"phase", "repo", "tier", "day", "task", "model"}

// UsageTiers 是 tier= 的合法取值；none = 运行时及之后都未定档（含 PRD 运行）。
var UsageTiers = []string{"light", "heavy", "none"}

// UsageBucket 把原始阶段标签归并成阶段桶，按序首个命中。写入端（决定 bucket 列）
// 与 0039 回填的 CASE（reviewRound=false 那半张表）同构，AC-6 对拍。
//
// reviewRound=true 时 PR review 那一轮里的所有任务侧运行（resume 续跑、降级
// 重入、修复回路）都归 review —— 它回答「改一版 review 一共值多少钱」。
func UsageBucket(phase string, reviewRound bool) string {
	isImpl := phase == "implement" || phase == "implement-reentry" || fixPhase.MatchString(phase)
	switch {
	case reviewRound && isImpl:
		return "review"
	case phase == "review":
		return "review"
	case phase == "triage":
		return "triage"
	case isImpl:
		return "implement"
	case phase == "verify" || phase == "push":
		return phase
	case phase == "plan":
		return "prd_plan"
	case phase == "plan-review":
		return "prd_review"
	case phase == "recommend":
		return "preview"
	default:
		return "other"
	}
}

// RunStatus 判定一次运行的终局状态，按序首个命中：
// 静默判死 / 总超时 / 取消 → killed；CLI 没给 result → no_result；其余错误 → error；
// CLI 报失败 → error；否则 success。
func RunStatus(res *agent.Result, err error) string {
	switch {
	case errors.Is(err, agent.ErrIdle):
		return "killed"
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return "killed"
	case err != nil && strings.Contains(err.Error(), "执行超时"):
		// 兜底同一语义的旧错误形态（agent.go 补 %w 之前是裸 fmt.Errorf）
		return "killed"
	case errors.Is(err, agent.ErrNoResultEvent):
		return "no_result"
	case err != nil:
		return "error"
	case res != nil && (res.IsError || !res.Success):
		return "error"
	default:
		return "success"
	}
}

// truncRunes 按 rune 截到 n 个字符（不加省略号：账本里的短标签要可分组）。
func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// usageModels 收敛逐模型明细：排序（costUSD 降序、并列按名升序）、名字按 rune
// 截断、超过 16 个时把第 16 个起合并成 (other) —— 不丢量，明细之和恒等于行上四列。
// 返回主模型（截断前全集里 costUSD 最大的那个）与 JSON。
func usageModels(in []agent.ModelUsage) (main *string, js string) {
	if len(in) == 0 {
		return nil, "[]"
	}
	ms := append([]agent.ModelUsage(nil), in...)
	sort.SliceStable(ms, func(i, j int) bool {
		if ms[i].CostUSD != ms[j].CostUSD {
			return ms[i].CostUSD > ms[j].CostUSD
		}
		return ms[i].Model < ms[j].Model
	})
	m0 := truncRunes(ms[0].Model, usageTextRunes)
	main = &m0
	for i := range ms {
		ms[i].Model = truncRunes(ms[i].Model, usageTextRunes)
	}
	if len(ms) > usageMaxModels {
		other := agent.ModelUsage{Model: usageOtherModel}
		for _, m := range ms[usageMaxModels-1:] {
			other.InputTokens += m.InputTokens
			other.OutputTokens += m.OutputTokens
			other.CacheReadTokens += m.CacheReadTokens
			other.CacheCreationTokens += m.CacheCreationTokens
			other.CostUSD += m.CostUSD
		}
		ms = append(ms[:usageMaxModels-1], other)
	}
	b, err := json.Marshal(ms)
	if err != nil {
		return main, "[]"
	}
	return main, string(b)
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// RecordAgentRun 落一次 agent 运行的账本行（实现 agent.UsageRecorder）。
//
// 归属只从目标行取：调用方只给 TaskID/PRDID，user_id/repo_id（与预览运行的
// verify_tier）由同一条 INSERT ... SELECT 从 tasks/prds 读出，没有读改写窗口。
// 同一次尝试重复写被唯一约束吞掉、返回 nil；目标行已不存在返回 ErrAgentRunTargetGone。
func (s *Store) RecordAgentRun(ctx context.Context, r agent.RunRecord) error {
	var targetSQL string
	var targetID int64
	switch r.Kind {
	case "task", "preview":
		targetSQL, targetID = `SELECT id, NULL::bigint AS prd_id, id AS task_id, user_id, repo_id, verify_tier FROM tasks WHERE id = $1`, r.TaskID
	case "prd":
		targetSQL, targetID = `SELECT id, id AS prd_id, NULL::bigint AS task_id, user_id, repo_id, NULL::text AS verify_tier FROM prds WHERE id = $1`, r.PRDID
	default:
		return fmt.Errorf("store: 未知的运行类别 %q", r.Kind)
	}

	started := r.StartedAt
	ended := r.EndedAt
	if ended.Before(started) {
		// 运行期间墙钟被往回拨：钳位，不让 time_check 把一笔真实花费整行拒掉
		ended = started
	}

	res := r.Result
	var (
		model     *string
		models    = "[]"
		in, out   int64
		cr, cc    int64
		tokKnown  bool
		cost      *float64
		duration  = ended.Sub(started).Milliseconds()
		turns     int
		subtype   *string
		tierParam = nullIfEmpty(r.VerifyTier)
	)
	if res != nil {
		// res 非 nil 说明 CLI 已报出这次运行的终局账单，与 err 无关照记
		duration = res.DurationMS
		turns = res.NumTurns
		subtype = nullIfEmpty(truncRunes(res.Subtype, usageTextRunes))
		if res.CostKnown {
			c := res.CostUSD
			cost = &c
		}
		if res.Usage.Present {
			tokKnown = true
			in, out = res.Usage.InputTokens, res.Usage.OutputTokens
			cr, cc = res.Usage.CacheReadTokens, res.Usage.CacheCreationTokens
			model, models = usageModels(res.Usage.Models)
		}
	}
	if duration < 0 {
		duration = 0
	}
	if r.Kind == "prd" {
		tierParam = nil
	}

	var tgt, ins int
	err := s.pool.QueryRow(ctx, `
		WITH tgt AS (`+targetSQL+`),
		ins AS (
		  INSERT INTO agent_runs (
		    task_id, prd_id, user_id, repo_id, phase, bucket, run_kind, review_round,
		    session_id, resume, model, models,
		    input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, tokens_known,
		    cost_usd, duration_ms, verify_tier, num_turns, status, subtype, started_at, ended_at)
		  SELECT tgt.task_id, tgt.prd_id, tgt.user_id, tgt.repo_id, $2::text, $3::text, $4::text, $5::boolean,
		         $6::text, $7::boolean, $8::text, $9::jsonb,
		         $10::bigint, $11::bigint, $12::bigint, $13::bigint, $14::boolean,
		         $15::float8, $16::bigint,
		         CASE WHEN $4::text = 'preview' THEN tgt.verify_tier ELSE $17::text END,
		         $18::int, $19::text, $20::text, $21::timestamptz, $22::timestamptz
		    FROM tgt
		  ON CONFLICT ON CONSTRAINT agent_runs_attempt_uniq DO NOTHING
		  RETURNING 1)
		SELECT (SELECT count(*) FROM tgt), (SELECT count(*) FROM ins)`,
		targetID, r.Phase, UsageBucket(r.Phase, r.ReviewRound), r.Kind, r.ReviewRound,
		nullIfEmpty(r.SessionID), r.Resume, model, models,
		in, out, cr, cc, tokKnown,
		cost, duration, tierParam,
		turns, RunStatus(res, r.Err), subtype, started, ended,
	).Scan(&tgt, &ins)
	if err != nil {
		return fmt.Errorf("store: 写 agent 用量失败: %w", err)
	}
	if tgt == 0 {
		return ErrAgentRunTargetGone
	}
	return nil
}

// SetAgentRunTier 是定档回补（实现 agent.TierBackfiller）：本任务里此前没有档位的
// 行补成本次定下的档。只补 NULL，已有档位的行永不改写；幂等，失败了下次定档再补。
func (s *Store) SetAgentRunTier(ctx context.Context, taskID int64, tier string) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE agent_runs SET verify_tier = $2
		 WHERE run_kind IN ('task','preview') AND task_id = $1 AND verify_tier IS NULL`,
		taskID, tier); err != nil {
		return fmt.Errorf("store: 定档回补失败: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- 聚合

// UsageQuery 是聚合的筛选与分组（参数校验在 httpapi，这里只信任合法值）。
type UsageQuery struct {
	From, To time.Time // 半开区间 [From, To)
	RepoID   int64     // 0 = 全部
	Tier     string    // "" = 全部；light/heavy/none
	Bucket   string    // "" = 全部
	Group    string    // phase/repo/tier/day/task/model；"" 视为 phase
	Limit    int       // 只对 group=task 生效；<=0 视为默认 20
}

// UsageTokens 是四项 token 计数（分组项与任务明细里的嵌套形状）。
type UsageTokens struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheRead     int64 `json:"cacheRead"`
	CacheCreation int64 `json:"cacheCreation"`
}

// UsageTotals 是一个集合的合计（totals / 任务明细的 summary）。
//
// CostUSD 是 cost_usd 非 NULL 行之和；CostKnown==0 时为 null（「未知」不渲染成 $0）。
type UsageTotals struct {
	Runs                int      `json:"runs"`
	CostUSD             *float64 `json:"costUsd"`
	DurationMS          int64    `json:"durationMs"`
	Turns               int64    `json:"turns"`
	InputTokens         int64    `json:"inputTokens"`
	OutputTokens        int64    `json:"outputTokens"`
	CacheReadTokens     int64    `json:"cacheReadTokens"`
	CacheCreationTokens int64    `json:"cacheCreationTokens"`
	TokensKnown         int      `json:"tokensKnown"`
	CostKnown           int      `json:"costKnown"`
}

// UsageGroup 是一个分组项（byBucket / byGroup / byDay）。属主视图专用：带标签字段。
type UsageGroup struct {
	Key         string
	Runs        int
	CostUSD     *float64
	DurationMS  int64
	Tokens      UsageTokens
	TokensKnown int
	CostKnown   int

	// group=phase
	Bucket string
	// group=repo
	RepoID       int64
	ProviderRepo *string
	// group=task：Kind=task 时 TaskID 与三个任务标签，Kind=prd 时 PRDID 与 OneLiner。
	Kind            string
	TaskID          int64
	PRDID           int64
	ExternalKey     *string
	TrackerProvider *string
	IssueTitle      *string
	OneLiner        *string

	// shape 决定 JSON 里出现哪些额外字段（"" = 只有公共字段）。
	shape string
}

func (g UsageGroup) common() map[string]any {
	return map[string]any{
		"key": g.Key, "runs": g.Runs, "costUsd": g.CostUSD, "durationMs": g.DurationMS,
		"tokens": g.Tokens, "tokensKnown": g.TokensKnown, "costKnown": g.CostKnown,
	}
}

// MarshalJSON 按分组维度输出各自的额外字段；标签字段的来源见 docs §3.3。
func (g UsageGroup) MarshalJSON() ([]byte, error) {
	m := g.common()
	switch g.shape {
	case "phase":
		m["bucket"] = g.Bucket
	case "repo":
		m["repoId"] = g.RepoID
		m["providerRepo"] = g.ProviderRepo
	case "task":
		m["kind"] = g.Kind
		if g.Kind == "prd" {
			m["prdId"] = g.PRDID
			m["oneLiner"] = g.OneLiner
		} else {
			m["taskId"] = g.TaskID
			m["externalKey"] = g.ExternalKey
			m["trackerProvider"] = g.TrackerProvider
			m["issueTitle"] = g.IssueTitle
		}
	case "model":
		return json.Marshal(g.asModel())
	}
	return json.Marshal(m)
}

func (g UsageGroup) asModel() UsageModel {
	return UsageModel{Key: g.Key, Runs: g.Runs, CostUSD: g.CostUSD,
		InputTokens: g.Tokens.Input, OutputTokens: g.Tokens.Output,
		CacheReadTokens: g.Tokens.CacheRead, CacheCreationTokens: g.Tokens.CacheCreation,
		TokensKnown: g.TokensKnown, CostKnown: g.CostKnown}
}

// UsageModel 是 byModel 的一项：token 平铺，与 totals 同名，Σ 可直接对账。
type UsageModel struct {
	Key                 string   `json:"key"`
	Runs                int      `json:"runs"`
	CostUSD             *float64 `json:"costUsd"`
	InputTokens         int64    `json:"inputTokens"`
	OutputTokens        int64    `json:"outputTokens"`
	CacheReadTokens     int64    `json:"cacheReadTokens"`
	CacheCreationTokens int64    `json:"cacheCreationTokens"`
	TokensKnown         int      `json:"tokensKnown"`
	CostKnown           int      `json:"costKnown"`
}

// UsageStats 是 /api/usage 的响应（属主视图）。集合字段恒非 nil。
type UsageStats struct {
	From     time.Time    `json:"from"`
	To       time.Time    `json:"to"`
	Totals   UsageTotals  `json:"totals"`
	ByBucket []UsageGroup `json:"byBucket"`
	ByGroup  []UsageGroup `json:"byGroup"`
	ByDay    []UsageGroup `json:"byDay"`
	ByModel  []UsageModel `json:"byModel"`
	// GroupTotal 只在 group=task 时出现：截断前的组数。
	GroupTotal *int `json:"groupTotal,omitempty"`
}

// AdminUsageGroup 是管理员视图的分组项：结构上没有任何标签字段
// （externalKey / trackerProvider / issueTitle / oneLiner / providerRepo）。
type AdminUsageGroup struct {
	Key         string      `json:"key"`
	Kind        string      `json:"kind,omitempty"`
	TaskID      int64       `json:"taskId,omitempty"`
	PRDID       int64       `json:"prdId,omitempty"`
	RepoID      int64       `json:"repoId,omitempty"`
	Runs        int         `json:"runs"`
	CostUSD     *float64    `json:"costUsd"`
	DurationMS  int64       `json:"durationMs"`
	Tokens      UsageTokens `json:"tokens"`
	TokensKnown int         `json:"tokensKnown"`
	CostKnown   int         `json:"costKnown"`

	model bool
}

// MarshalJSON 在 group=model 时输出与 byModel 同形的结构，其余照结构体标签输出。
func (g AdminUsageGroup) MarshalJSON() ([]byte, error) {
	if g.model {
		return json.Marshal(UsageModel{Key: g.Key, Runs: g.Runs, CostUSD: g.CostUSD,
			InputTokens: g.Tokens.Input, OutputTokens: g.Tokens.Output,
			CacheReadTokens: g.Tokens.CacheRead, CacheCreationTokens: g.Tokens.CacheCreation,
			TokensKnown: g.TokensKnown, CostKnown: g.CostKnown})
	}
	type plain AdminUsageGroup
	return json.Marshal(plain(g))
}

// UsageUser 是管理员视图 byUser 的一项：只有 user_id，不带 email。
type UsageUser struct {
	UserID      int64       `json:"userId"`
	Runs        int         `json:"runs"`
	CostUSD     *float64    `json:"costUsd"`
	DurationMS  int64       `json:"durationMs"`
	Tokens      UsageTokens `json:"tokens"`
	TokensKnown int         `json:"tokensKnown"`
	CostKnown   int         `json:"costKnown"`
}

// AdminUsageStats 是 /api/admin/usage 的响应。
type AdminUsageStats struct {
	From       time.Time         `json:"from"`
	To         time.Time         `json:"to"`
	Totals     UsageTotals       `json:"totals"`
	ByBucket   []AdminUsageGroup `json:"byBucket"`
	ByGroup    []AdminUsageGroup `json:"byGroup"`
	ByDay      []AdminUsageGroup `json:"byDay"`
	ByModel    []UsageModel      `json:"byModel"`
	ByUser     []UsageUser       `json:"byUser"`
	GroupTotal *int              `json:"groupTotal,omitempty"`
}

// usageWhere 是 agent_runs 的过滤条件与参数（scope + 筛选）。
type usageWhere struct {
	sql  string
	args []any
}

func (w *usageWhere) add(cond string, arg any) {
	w.args = append(w.args, arg)
	w.sql += fmt.Sprintf(" AND "+cond, len(w.args))
}

// usageScope 是属主视图唯一的过滤入口：WHERE user_id = $1。没有「0 表示全部」。
func usageScope(userID int64) *usageWhere {
	return &usageWhere{sql: "r.user_id = $1", args: []any{userID}}
}

// adminScope 是管理员全局视图的入口：显式无属主过滤（只给 AdminUsageStats 用）。
func adminScope() *usageWhere {
	return &usageWhere{sql: "TRUE"}
}

func (w *usageWhere) filter(q UsageQuery) *usageWhere {
	w.add("r.started_at >= $%d", q.From)
	w.add("r.started_at < $%d", q.To)
	if q.RepoID != 0 {
		w.add("r.repo_id = $%d", q.RepoID)
	}
	switch q.Tier {
	case "":
	case "none":
		w.sql += " AND r.verify_tier IS NULL"
	default:
		w.add("r.verify_tier = $%d", q.Tier)
	}
	if q.Bucket != "" {
		w.add("r.bucket = $%d", q.Bucket)
	}
	return w
}

// aggCols 是各分组共用的聚合列（顺序与 scanAggInto 对应；别名供 group=task 的外层查询引用）。
const aggCols = `count(*)::int AS runs, sum(r.cost_usd)::float8 AS cost, COALESCE(sum(r.duration_ms),0)::bigint AS dur,
	COALESCE(sum(r.input_tokens),0)::bigint AS tin, COALESCE(sum(r.output_tokens),0)::bigint AS tout,
	COALESCE(sum(r.cache_read_tokens),0)::bigint AS tcr, COALESCE(sum(r.cache_creation_tokens),0)::bigint AS tcc,
	(count(*) FILTER (WHERE r.tokens_known))::int AS tk, count(r.cost_usd)::int AS ck`

// aggRefs 是外层查询按别名取回聚合列（与 aggCols 同序）。
const aggRefs = `runs, cost, dur, tin, tout, tcr, tcc, tk, ck`

// costOrderSQL 让排序全序：costUsd 降序（NULL 最后）→ runs 降序 → key 升序。
// 与 Go 侧 sortGroups 同口径；group=task 要在 SQL 里排好才能 LIMIT。
const costOrderSQL = `cost DESC NULLS LAST, runs DESC, k ASC`

// prefixed 给逗号分隔的列名逐个加前缀。
func prefixed(prefix, cols string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = prefix + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}

func scanAggInto(g *UsageGroup) []any {
	return []any{&g.Runs, &g.CostUSD, &g.DurationMS, &g.Tokens.Input, &g.Tokens.Output,
		&g.Tokens.CacheRead, &g.Tokens.CacheCreation, &g.TokensKnown, &g.CostKnown}
}

// queryGroups 跑一个 `SELECT <key>, <agg>, <extra...> FROM agent_runs r <join> WHERE ... GROUP BY ...`。
func (s *Store) queryGroups(ctx context.Context, sql string, args []any, shape string, extra func(g *UsageGroup) []any) ([]UsageGroup, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageGroup{}
	for rows.Next() {
		g := UsageGroup{shape: shape}
		dest := append([]any{&g.Key}, scanAggInto(&g)...)
		if extra != nil {
			dest = append(dest, extra(&g)...)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func groupedSQL(keyExpr, where, extraCols, joins, groupBy, order string) string {
	cols := keyExpr + " AS k, " + aggCols
	if extraCols != "" {
		cols += ", " + extraCols
	}
	return `SELECT ` + cols + ` FROM agent_runs r ` + joins + ` WHERE ` + where +
		` GROUP BY ` + groupBy + ` ORDER BY ` + order
}

// usageCostLess 是 Go 侧的全序比较：costUsd 降序（nil 最后）→ runs 降序 → key 升序。
func usageCostLess(a, b *float64, ra, rb int, ka, kb string) bool {
	switch {
	case a != nil && b == nil:
		return true
	case a == nil && b != nil:
		return false
	case a != nil && b != nil && *a != *b:
		return *a > *b
	}
	if ra != rb {
		return ra > rb
	}
	return ka < kb
}

// usageCore 是属主与管理员共用的聚合：totals / byBucket / byDay / byModel，
// 以及不带标签的 group=phase/tier/day/model。group=repo/task 由调用方各自查
// （属主带标签 JOIN，管理员不带）。
type usageCore struct {
	totals  UsageTotals
	buckets []UsageGroup
	days    []UsageGroup
	models  []UsageModel
}

func (s *Store) usageCore(ctx context.Context, w *usageWhere) (*usageCore, error) {
	c := &usageCore{}
	t := &c.totals
	if err := s.pool.QueryRow(ctx, `SELECT count(*)::int, sum(r.cost_usd)::float8,
		COALESCE(sum(r.duration_ms),0)::bigint, COALESCE(sum(r.num_turns),0)::bigint,
		COALESCE(sum(r.input_tokens),0)::bigint, COALESCE(sum(r.output_tokens),0)::bigint,
		COALESCE(sum(r.cache_read_tokens),0)::bigint, COALESCE(sum(r.cache_creation_tokens),0)::bigint,
		(count(*) FILTER (WHERE r.tokens_known))::int, count(r.cost_usd)::int
		FROM agent_runs r WHERE `+w.sql, w.args...).Scan(
		&t.Runs, &t.CostUSD, &t.DurationMS, &t.Turns, &t.InputTokens, &t.OutputTokens,
		&t.CacheReadTokens, &t.CacheCreationTokens, &t.TokensKnown, &t.CostKnown); err != nil {
		return nil, fmt.Errorf("store: 统计用量合计失败: %w", err)
	}
	var err error
	if c.buckets, err = s.queryGroups(ctx, groupedSQL("r.bucket", w.sql, "", "", "1", "1"), w.args, "", nil); err != nil {
		return nil, fmt.Errorf("store: 统计阶段桶用量失败: %w", err)
	}
	c.buckets = sortGroups(c.buckets)
	dayKey := `to_char(r.started_at AT TIME ZONE 'UTC', 'YYYY-MM-DD')`
	if c.days, err = s.queryGroups(ctx, groupedSQL(dayKey, w.sql, "", "", "1", "1 ASC"), w.args, "", nil); err != nil {
		return nil, fmt.Errorf("store: 统计日用量失败: %w", err)
	}
	if c.models, err = s.usageModels(ctx, w); err != nil {
		return nil, err
	}
	return c, nil
}

func sortGroups(gs []UsageGroup) []UsageGroup {
	sort.SliceStable(gs, func(i, j int) bool {
		return usageCostLess(gs[i].CostUSD, gs[j].CostUSD, gs[i].Runs, gs[j].Runs, gs[i].Key, gs[j].Key)
	})
	return gs
}

// usageModels 按 models 展开：有明细的行每个元素贡献到自己的模型键，
// models='[]' 的行整行贡献到 (unknown)（codex、被杀、回填、未上报的运行不丢）。
func (s *Store) usageModels(ctx context.Context, w *usageWhere) ([]UsageModel, error) {
	rows, err := s.pool.Query(ctx, `
		WITH f AS (SELECT r.* FROM agent_runs r WHERE `+w.sql+`),
		x AS (
		  SELECT e->>'model' AS k,
		         COALESCE((e->>'inputTokens')::bigint,0) AS i, COALESCE((e->>'outputTokens')::bigint,0) AS o,
		         COALESCE((e->>'cacheReadTokens')::bigint,0) AS cr, COALESCE((e->>'cacheCreationTokens')::bigint,0) AS cc,
		         (e->>'costUsd')::numeric AS cost, f.tokens_known AS tk
		    FROM f CROSS JOIN LATERAL jsonb_array_elements(f.models) e
		   WHERE f.models <> '[]'::jsonb
		  UNION ALL
		  SELECT $`+fmt.Sprint(len(w.args)+1)+`::text, f.input_tokens, f.output_tokens, f.cache_read_tokens, f.cache_creation_tokens,
		         f.cost_usd, f.tokens_known
		    FROM f WHERE f.models = '[]'::jsonb)
		SELECT k, count(*)::int, sum(cost)::float8, sum(i)::bigint, sum(o)::bigint, sum(cr)::bigint, sum(cc)::bigint,
		       (count(*) FILTER (WHERE tk))::int, count(cost)::int
		  FROM x GROUP BY k`, append(append([]any(nil), w.args...), usageUnknownModel)...)
	if err != nil {
		return nil, fmt.Errorf("store: 统计模型用量失败: %w", err)
	}
	defer rows.Close()
	out := []UsageModel{}
	for rows.Next() {
		var m UsageModel
		if err := rows.Scan(&m.Key, &m.Runs, &m.CostUSD, &m.InputTokens, &m.OutputTokens,
			&m.CacheReadTokens, &m.CacheCreationTokens, &m.TokensKnown, &m.CostKnown); err != nil {
			return nil, fmt.Errorf("store: 读取模型用量失败: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.CostUSD != nil && b.CostUSD == nil:
			return true
		case a.CostUSD == nil && b.CostUSD != nil:
			return false
		case a.CostUSD != nil && b.CostUSD != nil && *a.CostUSD != *b.CostUSD:
			return *a.CostUSD > *b.CostUSD
		}
		return a.Key < b.Key
	})
	return out, nil
}

func modelGroups(ms []UsageModel) []UsageGroup {
	out := make([]UsageGroup, 0, len(ms))
	for _, m := range ms {
		out = append(out, UsageGroup{Key: m.Key, Runs: m.Runs, CostUSD: m.CostUSD,
			Tokens:      UsageTokens{Input: m.InputTokens, Output: m.OutputTokens, CacheRead: m.CacheReadTokens, CacheCreation: m.CacheCreationTokens},
			TokensKnown: m.TokensKnown, CostKnown: m.CostKnown, shape: "model"})
	}
	return out
}

func tierGroups(gs []UsageGroup) []UsageGroup {
	rank := map[string]int{"light": 0, "heavy": 1, "none": 2}
	sort.SliceStable(gs, func(i, j int) bool { return rank[gs[i].Key] < rank[gs[j].Key] })
	return gs
}

func usageLimit(n int) int {
	if n <= 0 {
		return UsageDefaultLimit
	}
	return n
}

// taskKeyExpr 是 group=task 的键：PRD 运行没有任务，按 PRD 自成一组。
const taskKeyExpr = `CASE WHEN r.run_kind = 'prd' THEN 'prd:' || r.prd_id ELSE 'task:' || r.task_id END`

// UsageStats 聚合指定用户名下的用量（/api/usage）。
func (s *Store) UsageStats(ctx context.Context, userID int64, q UsageQuery) (*UsageStats, error) {
	w := usageScope(userID).filter(q)
	core, err := s.usageCore(ctx, w)
	if err != nil {
		return nil, err
	}
	us := &UsageStats{From: q.From, To: q.To, Totals: core.totals,
		ByBucket: core.buckets, ByDay: core.days, ByModel: core.models}

	switch q.Group {
	case "", "phase":
		us.ByGroup, err = s.queryGroups(ctx, groupedSQL("r.phase", w.sql, "min(r.bucket)", "", "r.phase", "1"), w.args, "phase",
			func(g *UsageGroup) []any { return []any{&g.Bucket} })
		us.ByGroup = sortGroups(us.ByGroup)
	case "repo":
		// 标签只来自属主自己的仓库行（JOIN 条件里显式带 user_id）
		us.ByGroup, err = s.queryGroups(ctx, groupedSQL("r.repo_id::text", w.sql, "r.repo_id, min(rp.provider_repo)",
			"LEFT JOIN repos rp ON rp.id = r.repo_id AND rp.user_id = r.user_id", "r.repo_id", "1"), w.args, "repo",
			func(g *UsageGroup) []any { return []any{&g.RepoID, &g.ProviderRepo} })
		us.ByGroup = sortGroups(us.ByGroup)
	case "tier":
		us.ByGroup, err = s.queryGroups(ctx, groupedSQL("COALESCE(r.verify_tier,'none')", w.sql, "", "", "1", "1"), w.args, "", nil)
		us.ByGroup = tierGroups(us.ByGroup)
	case "day":
		us.ByGroup = core.days
	case "model":
		us.ByGroup = modelGroups(core.models)
	case "task":
		us.ByGroup, us.GroupTotal, err = s.usageTaskGroups(ctx, w, usageLimit(q.Limit))
	default:
		return nil, fmt.Errorf("store: 未知分组 %q", q.Group)
	}
	if err != nil {
		return nil, fmt.Errorf("store: 统计分组用量失败: %w", err)
	}
	if us.ByGroup == nil {
		us.ByGroup = []UsageGroup{}
	}
	return us, nil
}

// usageTaskGroups 是属主视图的 group=task：先分组排序截断，再 JOIN 库内写死来源的标签。
//
//	externalKey / trackerProvider ← tasks（经 t.user_id = g.user_id）
//	issueTitle ← issues，JOIN 条件逐字照抄 query.go 的既有写法
//	oneLiner   ← prds.document->'oneLiner'->>'body'（Go 侧 TrimSpace + 按 rune 截 80）
func (s *Store) usageTaskGroups(ctx context.Context, w *usageWhere, limit int) ([]UsageGroup, *int, error) {
	args := append(append([]any(nil), w.args...), limit)
	rows, err := s.pool.Query(ctx, `
		WITH g AS (
		  SELECT `+taskKeyExpr+` AS k, min(r.run_kind) = 'prd' AS is_prd,
		         min(r.task_id) AS task_id, min(r.prd_id) AS prd_id, min(r.user_id) AS user_id, `+aggCols+`
		    FROM agent_runs r WHERE `+w.sql+`
		   GROUP BY 1),
		top AS (
		  SELECT g.*, count(*) OVER () AS total FROM g
		   ORDER BY `+costOrderSQL+`
		   LIMIT $`+fmt.Sprint(len(args))+`)
		SELECT top.k, `+prefixed("top.", aggRefs)+`,
		       top.is_prd, COALESCE(top.task_id,0), COALESCE(top.prd_id,0), top.total::int,
		       t.external_key, t.tracker_provider, i.title, p.document->'oneLiner'->>'body'
		  FROM top
		  LEFT JOIN tasks t ON t.id = top.task_id AND t.user_id = top.user_id
		  LEFT JOIN issues i ON t.tracker_provider = 'internal'
		         AND i.user_id = t.user_id AND i.key = t.external_key
		  LEFT JOIN prds p ON p.id = top.prd_id AND p.user_id = top.user_id
		 ORDER BY top.cost DESC NULLS LAST, top.runs DESC, top.k ASC`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := []UsageGroup{}
	total := 0
	for rows.Next() {
		g := UsageGroup{shape: "task"}
		var isPRD bool
		var oneLiner *string
		dest := append([]any{&g.Key}, scanAggInto(&g)...)
		dest = append(dest, &isPRD, &g.TaskID, &g.PRDID, &total, &g.ExternalKey, &g.TrackerProvider, &g.IssueTitle, &oneLiner)
		if err := rows.Scan(dest...); err != nil {
			return nil, nil, err
		}
		g.Kind = "task"
		if isPRD {
			g.Kind = "prd"
			g.OneLiner = oneLinerLabel(oneLiner)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(out) == 0 {
		// 截断后没有行时窗口函数也没有行可读，组数就是 0
		total = 0
	}
	return out, &total, nil
}

// oneLinerLabel：TrimSpace 后按 rune 截到 80；空白或缺失为 nil。
func oneLinerLabel(s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.TrimSpace(*s)
	if v == "" {
		return nil
	}
	v = truncRunes(v, 80)
	return &v
}

// AdminUsageStats 是管理员全局视图（/api/admin/usage）：显式无属主过滤，
// 只返回聚合与 user_id。不做任何标签 JOIN，返回类型里也没有标签字段。
func (s *Store) AdminUsageStats(ctx context.Context, q UsageQuery) (*AdminUsageStats, error) {
	w := adminScope().filter(q)
	core, err := s.usageCore(ctx, w)
	if err != nil {
		return nil, err
	}
	us := &AdminUsageStats{From: q.From, To: q.To, Totals: core.totals,
		ByBucket: adminGroups(core.buckets), ByDay: adminGroups(core.days), ByModel: core.models}

	var gs []UsageGroup
	switch q.Group {
	case "", "phase":
		gs, err = s.queryGroups(ctx, groupedSQL("r.phase", w.sql, "", "", "r.phase", "1"), w.args, "", nil)
		gs = sortGroups(gs)
	case "repo":
		gs, err = s.queryGroups(ctx, groupedSQL("r.repo_id::text", w.sql, "r.repo_id", "", "r.repo_id", "1"), w.args, "",
			func(g *UsageGroup) []any { return []any{&g.RepoID} })
		gs = sortGroups(gs)
	case "tier":
		gs, err = s.queryGroups(ctx, groupedSQL("COALESCE(r.verify_tier,'none')", w.sql, "", "", "1", "1"), w.args, "", nil)
		gs = tierGroups(gs)
	case "day":
		gs = core.days
	case "model":
		us.ByGroup = make([]AdminUsageGroup, 0, len(core.models))
		for _, g := range modelGroups(core.models) {
			ag := adminGroup(g)
			ag.model = true
			us.ByGroup = append(us.ByGroup, ag)
		}
	case "task":
		var total int
		gs, total, err = s.adminTaskGroups(ctx, w, usageLimit(q.Limit))
		us.GroupTotal = &total
	default:
		return nil, fmt.Errorf("store: 未知分组 %q", q.Group)
	}
	if err != nil {
		return nil, fmt.Errorf("store: 统计分组用量失败: %w", err)
	}
	if us.ByGroup == nil {
		us.ByGroup = adminGroups(gs)
	}

	ug, err := s.queryGroups(ctx, groupedSQL("r.user_id::text", w.sql, "r.user_id", "", "r.user_id", "r.user_id ASC"), w.args, "",
		func(g *UsageGroup) []any { return []any{&g.TaskID} }) // 借 TaskID 承载 user_id
	if err != nil {
		return nil, fmt.Errorf("store: 统计用户用量失败: %w", err)
	}
	us.ByUser = make([]UsageUser, 0, len(ug))
	for _, g := range ug {
		us.ByUser = append(us.ByUser, UsageUser{UserID: g.TaskID, Runs: g.Runs, CostUSD: g.CostUSD, DurationMS: g.DurationMS,
			Tokens: g.Tokens, TokensKnown: g.TokensKnown, CostKnown: g.CostKnown})
	}
	return us, nil
}

// adminTaskGroups 是管理员视图的 group=task：只有键与 id，没有标签 JOIN。
func (s *Store) adminTaskGroups(ctx context.Context, w *usageWhere, limit int) ([]UsageGroup, int, error) {
	args := append(append([]any(nil), w.args...), limit)
	rows, err := s.pool.Query(ctx, `
		WITH g AS (
		  SELECT `+taskKeyExpr+` AS k, `+aggCols+`, min(r.run_kind) = 'prd' AS is_prd,
		         COALESCE(min(r.task_id),0) AS task_id, COALESCE(min(r.prd_id),0) AS prd_id
		    FROM agent_runs r WHERE `+w.sql+`
		   GROUP BY 1)
		SELECT k, `+aggRefs+`, is_prd, task_id, prd_id, (count(*) OVER ())::int FROM g
		 ORDER BY `+costOrderSQL+`
		 LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []UsageGroup{}
	total := 0
	for rows.Next() {
		var g UsageGroup
		var isPRD bool
		dest := append([]any{&g.Key}, scanAggInto(&g)...)
		dest = append(dest, &isPRD, &g.TaskID, &g.PRDID, &total)
		if err := rows.Scan(dest...); err != nil {
			return nil, 0, err
		}
		g.Kind = "task"
		if isPRD {
			g.Kind, g.TaskID = "prd", 0
		} else {
			g.PRDID = 0
		}
		out = append(out, g)
	}
	return out, total, rows.Err()
}

func adminGroup(g UsageGroup) AdminUsageGroup {
	return AdminUsageGroup{Key: g.Key, Kind: g.Kind, TaskID: g.TaskID, PRDID: g.PRDID, RepoID: g.RepoID,
		Runs: g.Runs, CostUSD: g.CostUSD, DurationMS: g.DurationMS, Tokens: g.Tokens,
		TokensKnown: g.TokensKnown, CostKnown: g.CostKnown}
}

func adminGroups(gs []UsageGroup) []AdminUsageGroup {
	out := make([]AdminUsageGroup, 0, len(gs))
	for _, g := range gs {
		out = append(out, adminGroup(g))
	}
	return out
}

// ---------------------------------------------------------------- 任务明细

// TaskUsageRun 是任务详情页「用量」卡的一行。
type TaskUsageRun struct {
	Phase       string          `json:"phase"`
	Bucket      string          `json:"bucket"`
	RunKind     string          `json:"runKind"`
	ReviewRound bool            `json:"reviewRound"`
	VerifyTier  *string         `json:"verifyTier"`
	Status      string          `json:"status"`
	Model       *string         `json:"model"`
	Models      json.RawMessage `json:"models"`
	CostUSD     *float64        `json:"costUsd"`
	DurationMS  int64           `json:"durationMs"`
	NumTurns    int             `json:"numTurns"`
	Tokens      UsageTokens     `json:"tokens"`
	TokensKnown bool            `json:"tokensKnown"`
	StartedAt   time.Time       `json:"startedAt"`
	EndedAt     time.Time       `json:"endedAt"`
	Resume      bool            `json:"resume"`
	Backfill    bool            `json:"backfill"`
}

// TaskUsage 是 GET /api/tasks/{id}/usage 的响应：该任务全部行，不受时间范围限制。
type TaskUsage struct {
	Summary UsageTotals    `json:"summary"`
	Runs    []TaskUsageRun `json:"runs"`
}

// TaskUsageFor 返回某任务（属主自己的）全部运行，按 started_at、id 升序（全序）。
// 属主校验由调用方先走 TaskDetail（非属主 404）；这里仍按 user_id 过滤兜底。
func (s *Store) TaskUsageFor(ctx context.Context, taskID, userID int64) (*TaskUsage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.phase, r.bucket, r.run_kind, r.review_round, r.verify_tier, r.status, r.model, r.models,
		       r.cost_usd::float8, r.duration_ms, r.num_turns,
		       r.input_tokens, r.output_tokens, r.cache_read_tokens, r.cache_creation_tokens, r.tokens_known,
		       r.started_at, r.ended_at, r.resume, r.backfill
		  FROM agent_runs r
		 WHERE r.user_id = $1 AND r.task_id = $2 AND r.run_kind IN ('task','preview')
		 ORDER BY r.started_at ASC, r.id ASC`, userID, taskID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询任务用量失败: %w", err)
	}
	defer rows.Close()
	tu := &TaskUsage{Runs: []TaskUsageRun{}}
	sum := &tu.Summary
	for rows.Next() {
		var r TaskUsageRun
		var models []byte
		if err := rows.Scan(&r.Phase, &r.Bucket, &r.RunKind, &r.ReviewRound, &r.VerifyTier, &r.Status, &r.Model, &models,
			&r.CostUSD, &r.DurationMS, &r.NumTurns,
			&r.Tokens.Input, &r.Tokens.Output, &r.Tokens.CacheRead, &r.Tokens.CacheCreation, &r.TokensKnown,
			&r.StartedAt, &r.EndedAt, &r.Resume, &r.Backfill); err != nil {
			return nil, fmt.Errorf("store: 读取任务用量失败: %w", err)
		}
		r.Models = json.RawMessage(models)
		if len(r.Models) == 0 {
			r.Models = json.RawMessage("[]")
		}
		tu.Runs = append(tu.Runs, r)

		sum.Runs++
		if r.CostUSD != nil {
			c := *r.CostUSD
			if sum.CostUSD != nil {
				c += *sum.CostUSD
			}
			sum.CostUSD = &c
			sum.CostKnown++
		}
		sum.DurationMS += r.DurationMS
		sum.Turns += int64(r.NumTurns)
		sum.InputTokens += r.Tokens.Input
		sum.OutputTokens += r.Tokens.Output
		sum.CacheReadTokens += r.Tokens.CacheRead
		sum.CacheCreationTokens += r.Tokens.CacheCreation
		if r.TokensKnown {
			sum.TokensKnown++
		}
	}
	return tu, rows.Err()
}
