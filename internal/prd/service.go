package prd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// 本文件是规划管线的编排层：把 store（持久化）、agent（只读驱动）与
// worktree（每轮用完即回收的只读检出）串成一轮对话。
//
// 设计上刻意只做「编排」——prompt 怎么拼在 prompt.go、输出怎么验在
// parse.go、能不能定稿在 checklist.go。本文件不含任何判断 PRD 好坏的
// 逻辑，那些都要能脱离数据库单测。

// Worktrees 是规划管线要的只读检出能力（runner.WorktreeManager 的子集）。
//
// 窄接口而非直接吃 *runner.WorktreeManager：一是避免 prd → runner 的包
// 依赖（runner 已经很重），二是单测里用假实现就能跑完整轮次。
type Worktrees interface {
	EnsureMirror(ctx context.Context, providerRepo, cloneURL string) (string, error)
	CreateDetached(ctx context.Context, providerRepo, base, name string) (*Checkout, error)
	RemoveDetached(ctx context.Context, c *Checkout) error
}

// Checkout 是一次只读检出的位置。
//
// Path 是规划 agent 唯一真正需要的东西（在哪个目录里读代码）；Mirror 只为
// 回收而存在 —— git worktree remove 必须在所属 bare mirror 里执行，少了它
// git 会在 serve 的 cwd 里跑，正是 AGENTS.md §9 那条 B2-3 事故的形状。
//
// 刻意不带分支名：detached 检出没有分支，带一个空字段只会引出「顺手
// branch -D」这类本不该存在的动作。
type Checkout struct {
	Path   string
	Mirror string
}

// AgentRunner 是 agent 驱动（agent.Driver 的 Run 方法）。
type AgentRunner interface {
	Run(ctx context.Context, p agent.RunParams) (*agent.Result, error)
}

// Store 是规划管线用到的持久化能力。
type Store interface {
	GetPRD(ctx context.Context, id, userID int64) (*store.PRDRow, error)
	UpdatePRDContent(ctx context.Context, id, userID int64, p store.UpdatePRDContentParams) (*store.PRDRow, error)
	TransitionPRD(ctx context.Context, id, userID int64, p store.TransitionPRDParams) (*store.PRDRow, error)
	BeginPRDRound(ctx context.Context, id, userID int64) (*store.PRDRow, error)
	EndPRDRound(ctx context.Context, id, userID int64) error
	AppendPRDRound(ctx context.Context, p store.AppendPRDRoundParams) (*store.PRDRoundRow, error)
	ListPRDRounds(ctx context.Context, prdID int64) ([]store.PRDRoundRow, error)
	InsertPRDEvents(ctx context.Context, prdID int64, round *int, phase string, entries []agent.Entry) error
	RepoForPRD(ctx context.Context, prdID, userID int64) (RepoInfo, error)
}

// RepoInfo 是一份 PRD 的目标仓库信息（含 §4.2 的量级阈值）。
type RepoInfo struct {
	ProviderRepo  string
	DefaultBranch string
	Limits        Limits
}

// Service 跑规划对话与对抗复核。
type Service struct {
	Store     Store
	Agent     AgentRunner
	Worktrees Worktrees

	// Issues / Flows 是一键生成（convert.go）要的两个窄能力。为 nil 时
	// 只有 Convert 返回 ErrConvertUnavailable，规划对话与复核不受影响 ——
	// 测试装配可以只接前者。
	Issues Issues
	Flows  Flows

	// Channel 是模型通道名（docs/10 §9 已决：走强通道，即 ImplementChannel）。
	//
	// 为什么不给规划单开一个环境变量：多一个可配项就多一处漏配的可能，
	// 而漏配的后果是静默降级到便宜通道 —— 对抗复核「想不出恶意实现」
	// 就等于没跑（D10-6 落空），而且没人会发现。
	Channel string

	// CloneURL 由 ProviderRepo 推导，与 queue.loadRepoConfig 同一套规则。
	// 留成字段是为了单测能替换掉。
	CloneURL func(providerRepo string) string

	// Now 可注入，便于测试。
	Now func() time.Time

	mu sync.Mutex
}

// roundTimeout 是单轮规划对话的上限。
//
// 取 10 分钟与预览推荐的 recommendTimeout 一致：都是「只读探索 + 出结构化
// 结论」的活。store.prdStallAfter（15 分钟）比它宽 5 分钟，正好覆盖超时后
// 落库与收尾的时间 —— 两个值必须保持这个大小关系，否则僵死判定会把还在
// 正常跑的轮次判死。
const roundTimeout = 10 * time.Minute

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) cloneURL(providerRepo string) string {
	if s.CloneURL != nil {
		return s.CloneURL(providerRepo)
	}
	return "git@github.com:" + providerRepo + ".git"
}

// ErrStageUnavailable 表示当前状态不允许跑这一步。
var ErrStageUnavailable = errors.New("prd: 当前状态不允许推进")

// RunRoundParams 是跑一轮对话的入参。
type RunRoundParams struct {
	PRDID  int64
	UserID int64
	// Answer 是人对上一轮问题的回答，首轮为空。
	Answer string
	// Stage 为空时由当前轮次自动推进（见 nextStage）。
	Stage Stage
}

// nextStage 按已跑过的轮数推断本轮阶段（docs/10 §1.3 的 A→E）。
//
// 为什么用轮数而不是让模型自报：阶段决定 prompt 里给什么约束（阶段 A
// 明令「先不读代码」、阶段 D 要求必须产出任务块）。让模型自己说在哪个
// 阶段，等于让它自己决定要不要受约束。
//
// 阶段 C 收敛可能要好几轮（例子法对齐意图是最费来回的一步，docs/10
// §5.4），所以第 3 轮之后一直停在 C，直到调用方显式指定 D。
func nextStage(rounds int) Stage {
	switch rounds {
	case 0:
		return StageUnderstand
	case 1:
		return StageExplore
	default:
		return StageConverge
	}
}

// RunRound 跑一轮规划对话：抢占 → 检出 → 起 agent → 校验 → 落库。
//
// 并发安全靠 store.BeginPRDRound 的 CAS（processing false→true），不靠
// 进程内的锁 —— 多节点部署时进程锁挡不住第二个节点。
func (s *Service) RunRound(ctx context.Context, p RunRoundParams) (*RoundResult, error) {
	row, err := s.Store.BeginPRDRound(ctx, p.PRDID, p.UserID)
	if err != nil {
		return nil, err
	}
	// 无论成败都要把 processing 放回去，否则这份 PRD 卡到僵死阈值才解锁。
	defer func() {
		if err := s.Store.EndPRDRound(context.WithoutCancel(ctx), p.PRDID, p.UserID); err != nil {
			slog.Warn("释放 PRD 轮次标记失败", "prd", p.PRDID, "err", err)
		}
	}()

	repo, err := s.Store.RepoForPRD(ctx, p.PRDID, p.UserID)
	if err != nil {
		return nil, err
	}

	rounds, err := s.Store.ListPRDRounds(ctx, p.PRDID)
	if err != nil {
		return nil, err
	}

	stage := p.Stage
	if stage == "" {
		stage = nextStage(len(rounds))
	}
	if !stage.Valid() {
		return nil, fmt.Errorf("prd: 未知阶段 %q", stage)
	}

	doc, err := ParseDocument(row.Document)
	if err != nil {
		return nil, fmt.Errorf("prd: 解析上一轮快照失败: %w", err)
	}
	// 首轮 document 是 NULL，ParseDocument 给的是零值文档 —— 传 nil 让
	// prompt 层走「首轮」分支，而不是渲染一份全空的快照当上下文。
	var prev *Document
	if len(row.Document) > 0 {
		prev = doc
	}

	round := row.Round + 1
	in := RoundInput{
		OriginalInput: row.OriginalInput,
		Type:          Type(row.PRDType),
		Stage:         stage,
		Round:         round,
		Document:      prev,
		UserAnswer:    p.Answer,
		Repo:          repo.ProviderRepo,
		Limits:        repo.Limits,
	}
	if prev != nil {
		in.Decisions = prev.Decisions
	}

	res, sessionID, err := s.runAgent(ctx, runSpec{
		prdID:  p.PRDID,
		round:  &round,
		phase:  store.PRDPhasePlan,
		prompt: BuildRoundPrompt(in),
		repo:   repo,
		// 阶段 A 明令先不读代码，省一次检出与回收（docs/10 §1.3）。
		needCheckout: stage != StageUnderstand,
	})
	if err != nil {
		return nil, err
	}

	parsed, err := ParseRoundResult(res.Text, stage)
	if err != nil {
		// 解析失败不推进轮次、不落快照：这一轮等于没发生，人重试即可。
		// 把 agent 原文的开头带进错误，便于判断是模型跑偏还是 prompt 有问题。
		return nil, fmt.Errorf("prd: 第 %d 轮输出不合规: %w", round, err)
	}

	if err := s.persistRound(ctx, persistSpec{
		prdID:     p.PRDID,
		userID:    p.UserID,
		round:     round,
		stage:     stage,
		sessionID: sessionID,
		answer:    p.Answer,
		result:    parsed,
		repo:      repo.ProviderRepo,
	}); err != nil {
		return nil, err
	}
	return parsed, nil
}

// runSpec 是一次 agent 调用的全部参数（规划轮次与对抗复核共用）。
type runSpec struct {
	prdID  int64
	round  *int // 对抗复核为 nil：它不在轮次序列里
	phase  string
	prompt string
	repo   RepoInfo
	// needCheckout 为假时不检出仓库（阶段 A 先不读代码）。
	needCheckout bool
}

// runAgent 起一次只读 agent，落事件，返回终局结果与会话 ID。
//
// 三条纪律，都是 preview/recommend.go 立的先例：
//  1. PermissionMode="plan" —— 只读，不许改目标仓库任何东西。
//  2. 每次新 SessionID，不 --resume —— worktree 每轮回收，续跑的会话会
//     以为自己还在已删目录里（D10-8 的直接后果）。
//  3. 事件先在内存里攒着，最后一次性落库 —— 单轮有 10 分钟上限，攒得下；
//     省掉 EventSink 那套 flush 协程与它对 task_id 的依赖。
func (s *Service) runAgent(ctx context.Context, spec runSpec) (*agent.Result, string, error) {
	dir := ""
	if spec.needCheckout {
		mirror, err := s.Worktrees.EnsureMirror(ctx, spec.repo.ProviderRepo, s.cloneURL(spec.repo.ProviderRepo))
		if err != nil {
			return nil, "", fmt.Errorf("prd: 准备镜像失败: %w", err)
		}
		_ = mirror

		name := fmt.Sprintf("prd-%d-%d", spec.prdID, s.now().UnixNano())
		co, err := s.Worktrees.CreateDetached(ctx, spec.repo.ProviderRepo, spec.repo.DefaultBranch, name)
		if err != nil {
			return nil, "", fmt.Errorf("prd: 只读检出失败: %w", err)
		}
		// 用完即回收（D10-8）。用 WithoutCancel：ctx 可能因超时已取消，
		// 但现场必须删 —— 不删就会攒下一堆没人认领的检出。
		defer func() {
			if err := s.Worktrees.RemoveDetached(context.WithoutCancel(ctx), co); err != nil {
				slog.Warn("回收 PRD 只读检出失败", "prd", spec.prdID, "path", co.Path, "err", err)
			}
		}()
		dir = co.Path
	}

	sessionID := newUUID()

	// 事件缓冲：OnEvent 在驱动读协程里同步调用，这里只做加锁 append，
	// 把落库留到最后。
	var (
		mu      sync.Mutex
		entries []agent.Entry
	)
	onEvent := func(ev agent.Event) {
		digested := agent.Digest(ev)
		if len(digested) == 0 {
			return
		}
		mu.Lock()
		entries = append(entries, digested...)
		mu.Unlock()
	}

	runCtx, cancel := context.WithTimeout(ctx, roundTimeout)
	defer cancel()

	res, runErr := s.Agent.Run(runCtx, agent.RunParams{
		Prompt:         spec.prompt,
		Dir:            dir,
		SessionID:      sessionID,
		PermissionMode: "plan",
		ExtraEnv:       channelEnv(s.Channel),
		OnEvent:        onEvent,
	})

	// 事件先落库再判错：跑挂了的那一轮，事件流正是人要看的东西。
	mu.Lock()
	buffered := entries
	mu.Unlock()
	if len(buffered) > 0 {
		if err := s.Store.InsertPRDEvents(context.WithoutCancel(ctx), spec.prdID, spec.round, spec.phase, buffered); err != nil {
			slog.Warn("落 PRD 事件失败", "prd", spec.prdID, "err", err)
		}
	}

	if runErr != nil {
		return nil, sessionID, fmt.Errorf("prd: agent 执行失败: %w", runErr)
	}
	if res == nil {
		return nil, sessionID, errors.New("prd: agent 没有返回结果")
	}
	if res.IsError || !res.Success {
		return nil, sessionID, fmt.Errorf("prd: agent 报告失败（%s）: %s",
			res.TerminalReason, head(res.Text, 200))
	}
	return res, sessionID, nil
}

// channelEnv 把通道名拼成 agent 的环境变量（与 runner 同名函数同一形状，
// 刻意复制而不是导出复用：跨包导出一个两行函数，换来的是 prd → runner
// 的包依赖，不值）。
func channelEnv(channel string) []string {
	if strings.TrimSpace(channel) == "" {
		return nil
	}
	return []string{"LATHE_AGENT_CHANNEL=" + strings.TrimSpace(channel)}
}

type persistSpec struct {
	prdID     int64
	userID    int64
	round     int
	stage     Stage
	sessionID string
	answer    string
	result    *RoundResult
	// repo 是目标仓库（owner/repo），随任务块一起落库供一键生成使用。
	repo string
}

// persistRound 落一轮的产物：对话历史 + 快照 + 状态。
//
// 顺序是刻意的：先 AppendPRDRound（append-only，唯一索引挡住同轮重复），
// 再 UpdatePRDContent。反过来的话，快照更新成功而历史追加失败会留下一份
// 「没有来源轮次的快照」—— 追溯时看不出它是怎么来的。
func (s *Service) persistRound(ctx context.Context, spec persistSpec) error {
	snapshot, err := spec.result.Document.Marshal()
	if err != nil {
		return fmt.Errorf("prd: 序列化快照失败: %w", err)
	}

	questions, err := json.Marshal(spec.result.Questions)
	if err != nil {
		return fmt.Errorf("prd: 序列化问题清单失败: %w", err)
	}

	var sessionID *string
	if spec.sessionID != "" {
		sessionID = &spec.sessionID
	}

	if _, err := s.Store.AppendPRDRound(ctx, store.AppendPRDRoundParams{
		PRDID:            spec.prdID,
		Round:            spec.round,
		Stage:            string(spec.stage),
		AgentSessionID:   sessionID,
		UserInput:        spec.answer,
		Questions:        json.RawMessage(questions),
		DocumentSnapshot: snapshot,
		Notes:            spec.result.Notes,
	}); err != nil {
		return err
	}

	round := spec.round
	upd := store.UpdatePRDContentParams{
		Document: snapshot,
		Round:    &round,
	}
	// 类型可能在探索后被修正（报成 feature 实则是 defect），跟着快照一起写。
	if t := string(spec.result.Document.Type); t != "" {
		upd.PRDType = &t
	}
	// §8 的结构块单独存一份：一键生成读它，不去 document 里翻。
	if len(spec.result.Document.Tasks) > 0 {
		blocks := &TaskBlocks{
			Version: BlocksVersion,
			Repo:    spec.repo,
			Tasks:   spec.result.Document.Tasks,
		}
		raw, err := blocks.Marshal()
		if err != nil {
			return fmt.Errorf("prd: 序列化任务块失败: %w", err)
		}
		upd.TaskBlocks = raw
	}
	if _, err := s.Store.UpdatePRDContent(ctx, spec.prdID, spec.userID, upd); err != nil {
		return err
	}

	// 有问题要问就挂起等人答，没有就留在 drafting 让人继续推进。
	// 这两个状态的区别是给人看的：awaiting_answers 意味着「球在你这边」。
	if len(spec.result.Questions) > 0 {
		if _, err := s.Store.TransitionPRD(ctx, spec.prdID, spec.userID, store.TransitionPRDParams{
			From: string(StateDrafting), To: string(StateAwaitingAnswers),
		}); err != nil && !errors.Is(err, store.ErrPRDStateChanged) {
			// CAS 失败说明状态已经不是 drafting（比如人同时点了放弃），
			// 不是错误 —— 这一轮的产物已经落库了。
			return err
		}
	}
	return nil
}

// RunReview 跑对抗复核（docs/10 §5.5），是进 ready_for_review 的必经步骤。
//
// 三条纪律与规划轮次不同，都是刻意的：
//  1. **不检出仓库。** 复核只看 §1–§4 与 §7，不看 §5 §6 —— 连代码都不给它
//     读，是为了不被作者的推理带偏。它要回答的是「这组 AC 本身有没有洞」，
//     不是「这个方案对不对」。
//  2. **不进轮次序列。** round 传 nil：它是定稿前的一次性动作，不是对话的
//     一轮。事件流里按 phase=plan-review 与规划事件分开显示。
//  3. **报告不带处置。** Disposition 由人在 UI 填 —— 处置是人的责任，
//     让复核自己给自己判「这条不用改」等于没复核。
func (s *Service) RunReview(ctx context.Context, prdID, userID int64) (*ReviewReport, error) {
	row, err := s.Store.BeginPRDRound(ctx, prdID, userID)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := s.Store.EndPRDRound(context.WithoutCancel(ctx), prdID, userID); err != nil {
			slog.Warn("释放 PRD 轮次标记失败", "prd", prdID, "err", err)
		}
	}()

	doc, err := ParseDocument(row.Document)
	if err != nil {
		return nil, fmt.Errorf("prd: 解析快照失败: %w", err)
	}
	if len(doc.Criteria) == 0 {
		return nil, fmt.Errorf("%w: 还没有任何 AC，无从复核", ErrStageUnavailable)
	}

	repo, err := s.Store.RepoForPRD(ctx, prdID, userID)
	if err != nil {
		return nil, err
	}

	res, _, err := s.runAgent(ctx, runSpec{
		prdID:  prdID,
		phase:  store.PRDPhasePlanReview,
		prompt: BuildReviewPrompt(doc),
		repo:   repo,
		// 不检出：复核不读代码（见上）。
		needCheckout: false,
	})
	if err != nil {
		return nil, err
	}

	report, err := ParseReviewResult(res.Text, doc)
	if err != nil {
		return nil, fmt.Errorf("prd: 对抗复核输出不合规: %w", err)
	}

	raw, err := report.Marshal()
	if err != nil {
		return nil, fmt.Errorf("prd: 序列化复核报告失败: %w", err)
	}
	if _, err := s.Store.UpdatePRDContent(ctx, prdID, userID, store.UpdatePRDContentParams{
		ReviewReport: raw,
	}); err != nil {
		return nil, err
	}
	return report, nil
}

// SubmitForReview 跑自检清单并把 PRD 推进 ready_for_review（docs/10 §6.1）。
//
// 自检不过时返回 Report 而不是干巴巴一个错误：前端要能按 Finding.Code
// 逐条跳到出问题的那一节。失败即拒绝、不修正 —— 自动补占位符只会让洞
// 通过检查却仍然存在。
func (s *Service) SubmitForReview(ctx context.Context, prdID, userID int64) (*Report, error) {
	row, err := s.Store.GetPRD(ctx, prdID, userID)
	if err != nil {
		return nil, err
	}
	if row.State != string(StateDrafting) {
		return nil, fmt.Errorf("%w: 只有 drafting 的 PRD 能提交评审，当前 %s",
			ErrStageUnavailable, row.State)
	}

	doc, err := ParseDocument(row.Document)
	if err != nil {
		return nil, fmt.Errorf("prd: 解析快照失败: %w", err)
	}
	blocks, err := ParseTaskBlocks(row.TaskBlocks)
	if err != nil {
		return nil, fmt.Errorf("prd: 解析任务块失败: %w", err)
	}
	report, err := ParseReviewReport(row.ReviewReport)
	if err != nil {
		return nil, fmt.Errorf("prd: 解析复核报告失败: %w", err)
	}

	repo, err := s.Store.RepoForPRD(ctx, prdID, userID)
	if err != nil {
		return nil, err
	}

	check := Check(doc, blocks, repo.Limits, report != nil, report.Unhandled())
	if !check.OK() {
		return check, check.Error()
	}

	if _, err := s.Store.TransitionPRD(ctx, prdID, userID, store.TransitionPRDParams{
		From: string(StateDrafting), To: string(StateReadyForReview),
	}); err != nil {
		return check, err
	}
	return check, nil
}
