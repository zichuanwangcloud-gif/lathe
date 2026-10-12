package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// 规划管线的持久化（docs/10-prd-template.md）：模糊任务与它的 PRD 活在
// prds 表，多轮对话历史在 prd_rounds，agent 事件在 prd_events。
// 状态机纯逻辑在 internal/prd/state.go —— 本文件只管读写，转移合法性
// 由调用方（prd.Machine）校验后再落库。

// ErrPRDNotFound 表示 PRD 不存在或不属于当前用户（两者刻意不分，
// 与 ErrIssueNotFound 同一原则：不对非属主暴露存在）。
var ErrPRDNotFound = errors.New("store: PRD 不存在")

// ErrPRDFrozen 表示 PRD 已签字冻结（D10-4），内容不许再写。
// 这是数据库层的兜底 —— 冻结判定的正本在 prd.State.Frozen()。
var ErrPRDFrozen = errors.New("store: PRD 已批准，内容已冻结（要改请开新 PRD 并引用这份）")

// ErrPRDBusy 表示已有一轮对话在跑（processing=true），CAS 抢占失败。
var ErrPRDBusy = errors.New("store: 该 PRD 正有一轮对话在进行中")

// prdStallAfter 是 processing 的僵死阈值。超过它仍挂在 processing 的轮次
// 视为死掉（进程被杀时没机会 CAS 回 false），读时顺手放行。
//
// 取 15 分钟的理由：规划 agent 的超时上限是 10 分钟（同 preview 的
// recommendTimeout），留 5 分钟余量覆盖落库与网络抖动。比这更短会把
// 还在正常思考的轮次误判成僵死，更长则卡着人干等。
const prdStallAfter = 15 * time.Minute

// PRDRow 是 prds 表的一行（含 JSON 标签，直接供 API 层返回）。
//
// 三个 jsonb 字段用 json.RawMessage 而不是 map/结构体：避免
// store/flows.go 踩过的坑（[]byte 走 encoding/json 会被编码成 base64
// 字符串，前端拿到一串乱码）。RawMessage 原样透传。
type PRDRow struct {
	ID       int64  `json:"id"`
	UserID   int64  `json:"userId"`
	RepoID   int64  `json:"repoId"`
	RefPRDID *int64 `json:"refPrdId,omitempty"`

	PRDType string `json:"prdType"`
	State   string `json:"state"`
	Round   int    `json:"round"`

	OriginalInput string `json:"originalInput"`

	Document     json.RawMessage `json:"document,omitempty"`
	TaskBlocks   json.RawMessage `json:"taskBlocks,omitempty"`
	ReviewReport json.RawMessage `json:"reviewReport,omitempty"`

	Processing          bool       `json:"processing"`
	ProcessingStartedAt *time.Time `json:"processingStartedAt,omitempty"`

	GeneratedFlowID *int64 `json:"generatedFlowId,omitempty"`

	ApprovedAt  *time.Time `json:"approvedAt,omitempty"`
	ConvertedAt *time.Time `json:"convertedAt,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// prdColumns 是 prds 的全列清单，SELECT 与 RETURNING 共用一份，
// 避免两处列序漂移（scanPRD 依赖这个顺序）。
const prdColumns = `id, user_id, repo_id, ref_prd_id, prd_type, state, round,
	original_input, document, task_blocks, review_report,
	processing, processing_started_at, generated_flow_id,
	approved_at, converted_at, created_at, updated_at`

func scanPRD(row interface{ Scan(...any) error }) (*PRDRow, error) {
	var p PRDRow
	if err := row.Scan(&p.ID, &p.UserID, &p.RepoID, &p.RefPRDID, &p.PRDType,
		&p.State, &p.Round, &p.OriginalInput, &p.Document, &p.TaskBlocks,
		&p.ReviewReport, &p.Processing, &p.ProcessingStartedAt,
		&p.GeneratedFlowID, &p.ApprovedAt, &p.ConvertedAt,
		&p.CreatedAt, &p.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPRDNotFound
		}
		return nil, fmt.Errorf("store: 读取 PRD 失败: %w", err)
	}
	return &p, nil
}

// CreatePRDParams 是建模糊任务（= 建 PRD）的输入。
//
// 没有 State 字段：一律从 drafting 起步，不给调用方指定初始状态的机会。
type CreatePRDParams struct {
	UserID        int64
	RepoID        int64
	PRDType       string
	OriginalInput string
	// RefPRDID 可选，接续某份已 converted/abandoned 的 PRD。
	RefPRDID *int64
}

// CreatePRD 建模糊任务。仓库归属由调用方校验（同 CreateIssue 的约定），
// 这里只靠外键兜底。
func (s *Store) CreatePRD(ctx context.Context, p CreatePRDParams) (*PRDRow, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO prds (user_id, repo_id, prd_type, original_input, ref_prd_id)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+prdColumns,
		p.UserID, p.RepoID, p.PRDType, p.OriginalInput, p.RefPRDID)
	return scanPRD(row)
}

// GetPRD 按 id 读 PRD；非属主一律 ErrPRDNotFound（404 语义）。
//
// 顺手清理僵死的 processing 标记：超过 prdStallAfter 仍挂着的轮次视为
// 死掉，就地 CAS 回 false 并返回清理后的行。把清理放在读路径上是刻意的
// —— 人看不到这份 PRD 时它卡着也无所谓，一看就自动放行，省掉一个常驻
// goroutine。
func (s *Store) GetPRD(ctx context.Context, id, userID int64) (*PRDRow, error) {
	p, err := s.getPRD(ctx, id, userID)
	if err != nil {
		return nil, err
	}
	if !p.Processing || p.ProcessingStartedAt == nil {
		return p, nil
	}
	if time.Since(*p.ProcessingStartedAt) < prdStallAfter {
		return p, nil
	}
	cleared, err := s.clearStalePRDProcessing(ctx, id, userID, *p.ProcessingStartedAt)
	if err != nil {
		return nil, err
	}
	if cleared != nil {
		return cleared, nil
	}
	return p, nil
}

func (s *Store) getPRD(ctx context.Context, id, userID int64) (*PRDRow, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT `+prdColumns+` FROM prds WHERE id = $1 AND user_id = $2`, id, userID)
	return scanPRD(row)
}

// clearStalePRDProcessing 把僵死的 processing 标记 CAS 回 false。
// 条件带上 processing_started_at 相等，防止与刚刚正常开始的新一轮打架
// —— 若并发下已被别人改过，这里影响 0 行并返回 nil，调用方沿用原行。
func (s *Store) clearStalePRDProcessing(ctx context.Context, id, userID int64, startedAt time.Time) (*PRDRow, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE prds SET processing = false, processing_started_at = NULL
		WHERE id = $1 AND user_id = $2
		  AND processing = true AND processing_started_at = $3
		RETURNING `+prdColumns, id, userID, startedAt)
	p, err := scanPRD(row)
	if errors.Is(err, ErrPRDNotFound) {
		return nil, nil
	}
	return p, err
}

// ListPRDsParams 是 PRD 列表的过滤条件。
type ListPRDsParams struct {
	UserID int64
	// State 为空串表示不过滤。
	State  string
	Limit  int
	Offset int
}

// ListPRDs 按更新时间倒序返回属主名下的 PRD。
//
// 不返回 document/task_blocks/review_report：列表页只要标题栏信息，
// 而这三个 jsonb 是整份文档快照，一页 50 行全带上够传几兆。
// 详情页走 GetPRD 拿完整的。
func (s *Store) ListPRDs(ctx context.Context, p ListPRDsParams) ([]PRDRow, int, error) {
	if p.Limit <= 0 || p.Limit > 200 {
		p.Limit = 50
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	var state any
	if p.State != "" {
		state = p.State
	}

	var total int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM prds
		WHERE user_id = $1 AND ($2::text IS NULL OR state = $2)`,
		p.UserID, state).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: 统计 PRD 数失败: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, repo_id, ref_prd_id, prd_type, state, round,
		       original_input, processing, processing_started_at,
		       generated_flow_id, approved_at, converted_at, created_at, updated_at
		FROM prds
		WHERE user_id = $1 AND ($2::text IS NULL OR state = $2)
		ORDER BY updated_at DESC
		LIMIT $3 OFFSET $4`, p.UserID, state, p.Limit, p.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("store: 查询 PRD 列表失败: %w", err)
	}
	defer rows.Close()

	out := make([]PRDRow, 0, p.Limit)
	for rows.Next() {
		var it PRDRow
		if err := rows.Scan(&it.ID, &it.UserID, &it.RepoID, &it.RefPRDID,
			&it.PRDType, &it.State, &it.Round, &it.OriginalInput,
			&it.Processing, &it.ProcessingStartedAt, &it.GeneratedFlowID,
			&it.ApprovedAt, &it.ConvertedAt, &it.CreatedAt, &it.UpdatedAt); err != nil {
			return nil, 0, fmt.Errorf("store: 读取 PRD 行失败: %w", err)
		}
		out = append(out, it)
	}
	return out, total, rows.Err()
}

// UpdatePRDContentParams 是一轮对话产出的落库输入；nil 字段不动。
//
// 注意没有 OriginalInput：原文是所有推断的根，store 层不提供改它的路径
// （docs/10 §0「原文不可编辑」的代码化 —— 靠不存在的 API 而非运行期检查）。
type UpdatePRDContentParams struct {
	Document     json.RawMessage
	TaskBlocks   json.RawMessage
	ReviewReport json.RawMessage
	// PRDType 允许改：阶段 A 判定的类型，探索后可能发现判错了
	// （报成 feature 实则是 defect）。
	PRDType *string
	// Round 非 nil 时推进轮次。
	Round *int
}

// UpdatePRDContent 写入一轮对话的产物。
//
// 冻结守卫：approved/converted 的 PRD 一律拒写（D10-4）。放在 SQL 的
// WHERE 里而不是先读后写，是为了让并发下的「批准」与「落库」互斥 ——
// 判定与写入在同一条语句里，中间没有别人插进来的窗口。
func (s *Store) UpdatePRDContent(ctx context.Context, id, userID int64, p UpdatePRDContentParams) (*PRDRow, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE prds SET
		  document      = COALESCE($3, document),
		  task_blocks   = COALESCE($4, task_blocks),
		  review_report = COALESCE($5, review_report),
		  prd_type      = COALESCE($6, prd_type),
		  round         = COALESCE($7, round)
		WHERE id = $1 AND user_id = $2
		  AND state NOT IN ('approved', 'converted')
		RETURNING `+prdColumns,
		id, userID, nullableJSON(p.Document), nullableJSON(p.TaskBlocks),
		nullableJSON(p.ReviewReport), p.PRDType, p.Round)
	res, err := scanPRD(row)
	if errors.Is(err, ErrPRDNotFound) {
		// 影响 0 行有两种可能：PRD 真不存在，或它已冻结。分辨一下再报错，
		// 否则「已批准」会被报成「不存在」，人完全看不懂。
		if cur, gerr := s.getPRD(ctx, id, userID); gerr == nil {
			if cur.State == "approved" || cur.State == "converted" {
				return nil, ErrPRDFrozen
			}
		}
		return nil, ErrPRDNotFound
	}
	return res, err
}

// nullableJSON 把空的 RawMessage 转成 SQL NULL，好让 COALESCE 保留原值。
// 直接传 nil RawMessage 给 pgx 也是 NULL，但显式转换免得读的人猜。
func nullableJSON(m json.RawMessage) any {
	if len(m) == 0 {
		return nil
	}
	return []byte(m)
}

// TransitionPRDParams 是一次状态转移的输入。
type TransitionPRDParams struct {
	// From 是期望的当前状态：CAS 的比较项。不匹配则整条不生效，
	// 防止「读到 drafting、写的时候已经被别人放弃了」这类竞态。
	From string
	To   string
	// GeneratedFlowID 仅 → converted 时带上。
	GeneratedFlowID *int64
}

// TransitionPRD 落一次状态转移。转移合法性由调用方（prd.Machine）先校验，
// 这里只做 CAS 与时间戳维护。
//
// approved_at / converted_at 在进入对应状态时写死：它们是签字与生成的
// 时刻凭据，不能被后续更新覆盖（所以用 CASE 而非无条件赋值 —— 虽然
// 转移表已保证 approved 只进一次，但时间戳是审计留痕，多一道保险不亏）。
func (s *Store) TransitionPRD(ctx context.Context, id, userID int64, p TransitionPRDParams) (*PRDRow, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE prds SET
		  state = $4,
		  approved_at  = CASE WHEN $4 = 'approved'  AND approved_at  IS NULL
		                      THEN now() ELSE approved_at  END,
		  converted_at = CASE WHEN $4 = 'converted' AND converted_at IS NULL
		                      THEN now() ELSE converted_at END,
		  generated_flow_id = COALESCE($5, generated_flow_id)
		WHERE id = $1 AND user_id = $2 AND state = $3
		RETURNING `+prdColumns,
		id, userID, p.From, p.To, p.GeneratedFlowID)
	res, err := scanPRD(row)
	if errors.Is(err, ErrPRDNotFound) {
		// 同 UpdatePRDContent：区分「不存在」与「状态已变」，
		// 否则并发下的报错完全没有诊断价值。
		if cur, gerr := s.getPRD(ctx, id, userID); gerr == nil {
			return nil, fmt.Errorf("store: PRD %d 的状态已是 %s，不再是 %s: %w",
				id, cur.State, p.From, ErrPRDStateChanged)
		}
		return nil, ErrPRDNotFound
	}
	return res, err
}

// ErrPRDStateChanged 表示 CAS 比较失败：落库前状态已被别人改掉。
var ErrPRDStateChanged = errors.New("store: PRD 状态已被并发修改")

// BeginPRDRound 用 CAS 抢占一轮对话：processing false→true。
//
// 抢不到返回 ErrPRDBusy。这是「每轮开一次 agent」的唯一闸门 —— 人连点
// 两次「回答」不会起两个 agent 去写同一份文档，否则两轮的快照会互相覆盖，
// 后写的赢，前一轮的 token 全白烧。
//
// 僵死的轮次不在这里处理：GetPRD 的读路径会先清掉，人刷新一下就能重试。
func (s *Store) BeginPRDRound(ctx context.Context, id, userID int64) (*PRDRow, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE prds SET processing = true, processing_started_at = now()
		WHERE id = $1 AND user_id = $2 AND processing = false
		  AND state NOT IN ('approved', 'converted', 'abandoned')
		RETURNING `+prdColumns, id, userID)
	res, err := scanPRD(row)
	if errors.Is(err, ErrPRDNotFound) {
		cur, gerr := s.getPRD(ctx, id, userID)
		if gerr != nil {
			return nil, ErrPRDNotFound
		}
		if cur.Processing {
			return nil, ErrPRDBusy
		}
		return nil, fmt.Errorf("store: PRD %d 处于 %s，不能再起对话轮次: %w",
			id, cur.State, ErrPRDStateChanged)
	}
	return res, err
}

// PRDRoundRow 是 prd_rounds 表的一行：一轮对话的完整留痕。
type PRDRoundRow struct {
	ID    int64  `json:"id"`
	PRDID int64  `json:"prdId"`
	Round int    `json:"round"`
	Stage string `json:"stage"`

	AgentSessionID   *string         `json:"agentSessionId,omitempty"`
	UserInput        string          `json:"userInput"`
	Questions        json.RawMessage `json:"questions,omitempty"`
	DocumentSnapshot json.RawMessage `json:"documentSnapshot,omitempty"`
	// Notes 是本轮摘要（docs/10 附录 A），2~3 行。
	Notes string `json:"notes"`

	CreatedAt time.Time `json:"createdAt"`
}

const prdRoundColumns = `id, prd_id, round, stage, agent_session_id,
	user_input, questions, document_snapshot, notes, created_at`

// AppendPRDRoundParams 是追加一轮对话记录的输入。
//
// prd_rounds 是 append-only 的：没有 Update 路径。每轮的问题清单与快照
// 都是当时的事实，事后修订会让「第 3 轮为什么这么答」失去依据 ——
// 这和 original_input 不可编辑是同一条理由。
type AppendPRDRoundParams struct {
	PRDID int64
	Round int
	Stage string

	AgentSessionID   *string
	UserInput        string
	Questions        json.RawMessage
	DocumentSnapshot json.RawMessage
	// Notes 是本轮摘要（docs/10 附录 A）。
	Notes string
}

// AppendPRDRound 追加一轮对话记录。
//
// UNIQUE(prd_id, round) 会挡住重复写同一轮 —— 那是并发起轮的征兆
// （BeginPRDRound 的 CAS 本该先挡住），所以这里把唯一冲突原样抛出，
// 不做「覆盖」或「自动加一」的补救：掩盖竞态比报错危险。
func (s *Store) AppendPRDRound(ctx context.Context, p AppendPRDRoundParams) (*PRDRoundRow, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO prd_rounds (prd_id, round, stage, agent_session_id,
		                        user_input, questions, document_snapshot, notes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+prdRoundColumns,
		p.PRDID, p.Round, p.Stage, p.AgentSessionID, p.UserInput,
		nullableJSON(p.Questions), nullableJSON(p.DocumentSnapshot), p.Notes)
	return scanPRDRound(row)
}

func scanPRDRound(row interface{ Scan(...any) error }) (*PRDRoundRow, error) {
	var r PRDRoundRow
	if err := row.Scan(&r.ID, &r.PRDID, &r.Round, &r.Stage, &r.AgentSessionID,
		&r.UserInput, &r.Questions, &r.DocumentSnapshot, &r.Notes,
		&r.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrPRDNotFound
		}
		return nil, fmt.Errorf("store: 读取 PRD 对话轮次失败: %w", err)
	}
	return &r, nil
}

// ListPRDRounds 按轮次升序返回一份 PRD 的全部对话历史。
//
// 属主校验由调用方先 GetPRD 做（同 verifications 的约定）：这里只按
// prd_id 查，因为轮次表没有 user_id 列 —— 归属经 prds 的外键传导。
func (s *Store) ListPRDRounds(ctx context.Context, prdID int64) ([]PRDRoundRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+prdRoundColumns+` FROM prd_rounds WHERE prd_id = $1 ORDER BY round`, prdID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询 PRD 对话历史失败: %w", err)
	}
	defer rows.Close()

	var out []PRDRoundRow
	for rows.Next() {
		r, err := scanPRDRound(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// EndPRDRound 把 processing CAS 回 false。
//
// 不校验属主之外的任何条件、也不返回错误给调用方作决策依据：它跑在
// agent 结束后的 defer 里，此时无论成功失败都必须放行，否则一次异常
// 就把这份 PRD 永久锁死（僵死清理要等 15 分钟，那是兜底不是常规路径）。
func (s *Store) EndPRDRound(ctx context.Context, id, userID int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE prds SET processing = false, processing_started_at = NULL
		WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("store: 释放 PRD 对话轮次失败: %w", err)
	}
	return nil
}
