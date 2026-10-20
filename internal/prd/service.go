package prd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
// parse.go、补丁怎么合在 patch.go、能不能定稿在 checklist.go。本文件不含
// 任何判断 PRD 好坏的逻辑，那些都要能脱离数据库单测。

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
	TouchPRDRound(ctx context.Context, id, userID int64) error
	EndPRDRound(ctx context.Context, id, userID int64) error
	FailPRDRun(ctx context.Context, id, userID int64, msg, input string) error
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

	// SettingSources 传给 agent 的 --setting-sources（与 Pipeline 同一个
	// 配置 LATHE_SETTING_SOURCES，默认 "project"）：规划 agent 不该带上执行
	// 者个人环境里的插件与技能（docs/02-design.md §9）。
	SettingSources string

	// NeutralDir 是不检出仓库的调用（阶段 A、对抗复核）的工作目录。
	//
	// 与 Pipeline.TriageDir 同一个理由（路线图 B2-3）：不指定的话子进程
	// 继承 serve 的 cwd（生产是 Lathe 自己的仓库根），claude 会向上收集
	// 祖先目录的 CLAUDE.md，把 Lathe 的配置灌进目标仓库的规划上下文。
	// 为空时用系统临时目录下的固定位置 —— 必须在任何项目树之外，
	// WorkspaceRoot 默认在仓库根之下，不合格。
	NeutralDir string

	// CloneURL 由 ProviderRepo 推导，与 queue.loadRepoConfig 同一套规则。
	// 留成字段是为了单测能替换掉。
	CloneURL func(providerRepo string) string

	// Now 可注入，便于测试。
	Now func() time.Time

	// bg 记账后台运行（StartRound / StartReview），供测试与优雅退出等待。
	bg sync.WaitGroup
}

// 一轮的时间预算。三个值与 store.prdStallAfter（20 分钟）必须保持
// 「roundBudget + 落库余量 < prdStallAfter」，否则僵死判定会把还在正常
// 修正的轮次判死、放第二个请求进来。
const (
	// roundTimeout 是首跑的上限：只读探索 + 出结构化结论，与预览推荐的
	// recommendTimeout 一致。
	roundTimeout = 10 * time.Minute
	// repairTimeout 是单次输出修正的上限：会话与检出都还在，只是改 JSON。
	repairTimeout = 3 * time.Minute
	// roundBudget 是含修正在内整轮的硬上限。
	roundBudget = 14 * time.Minute
	// minRepairWindow 是剩余预算不足以做一次修正时直接放弃的门槛。
	minRepairWindow = 45 * time.Second
)

// maxRepairs 是一轮之内输出修正的最多次数。
//
// 实测拒收的主因是结构性小错（dependsOn 写了两个前驱、引用了刚删掉的
// AC），把校验错误原样回喂给同一个会话，模型一次就能改对。原来的做法是
// 整轮作废、人重试 —— 那意味着把 4 分钟的读代码与推理全部重来一遍，
// 而且重来一遍未必不犯同一个错。给两次：第二次兜「改了这处又碰坏那处」。
const maxRepairs = 2

// maxConvergeRounds 是阶段 C 最多停留的轮数，过了就自动进入拆分。
//
// 原来「第 3 轮之后一直停在 C，直到调用方显式指定 D」：界面上指定阶段
// 藏在一个下拉框里，实测一份 PRD 在 C 里转了 9 轮，每轮都能找到一个新的
// 细节来问。收敛要有尽头：两轮写不完的，拆分与定稿阶段还能继续补。
const maxConvergeRounds = 2

// maxChainedRounds 是一次发起（StartRound）里最多连跑的轮数。
//
// 一轮没有问题要问时，让人点一下「继续」再跑下一轮是纯粹的空等 ——
// 实测 11 轮里有 4 轮人的回答只是「继续」「A」。所以没有问题就自动推进
// 到下一阶段。阶段单调前进（A→B→C→D→E），最多 5 轮就到头；这个上限只是
// 防御，防止阶段推断出 bug 时无限烧 token。
const maxChainedRounds = 5

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
	// Stage 为空时由已跑过的轮次自动推进（见 nextStage）。
	Stage Stage
}

// nextStage 按已跑过的轮次推断本轮阶段（docs/10 §1.3 的 A→E）。
//
// 为什么由系统推而不是让模型自报：阶段决定 prompt 里给什么约束（阶段 A
// 明令「先不读代码」、阶段 D 要求必须产出任务块）。让模型自己说在哪个
// 阶段，等于让它自己决定要不要受约束。
//
// 收敛（C）在两种情况下结束、进入拆分（D）：上一轮 C 没有再提问（它
// 自己也认为想清楚了），或者已经在 C 里待满 maxConvergeRounds 轮。
// D 之后进入定稿（E）并停在 E —— 之后人的修改意见都在 E 里处理。
func nextStage(rounds []store.PRDRoundRow) Stage {
	if len(rounds) == 0 {
		return StageUnderstand
	}
	last := rounds[len(rounds)-1]
	switch Stage(last.Stage) {
	case StageUnderstand:
		return StageExplore
	case StageExplore:
		return StageConverge
	case StageConverge:
		converge := 0
		for _, r := range rounds {
			if Stage(r.Stage) == StageConverge {
				converge++
			}
		}
		if questionCount(last.Questions) == 0 || converge >= maxConvergeRounds {
			return StageSplit
		}
		return StageConverge
	case StageSplit, StageFinalize:
		return StageFinalize
	}
	// 历史数据里没有阶段（或是未知值）：按最保守的收敛处理。
	return StageConverge
}

func questionCount(raw json.RawMessage) int {
	var qs []string
	if len(raw) == 0 || json.Unmarshal(raw, &qs) != nil {
		return 0
	}
	return len(qs)
}

// RunRound 同步跑一轮规划对话：抢占 → 检出 → 起 agent → 校验（必要时就地
// 修正）→ 落库。失败也会落到 prds.last_error。
//
// 界面走 StartRound（后台跑）；同步入口留给测试与将来的命令行调用。
// 并发安全靠 store.BeginPRDRound 的 CAS（processing false→true），不靠
// 进程内的锁 —— 多节点部署时进程锁挡不住第二个节点。
func (s *Service) RunRound(ctx context.Context, p RunRoundParams) (*RoundResult, error) {
	if _, err := s.Store.BeginPRDRound(ctx, p.PRDID, p.UserID); err != nil {
		return nil, err
	}
	// 无论成败都要把 processing 放回去，否则这份 PRD 卡到僵死阈值才解锁。
	defer s.endRound(ctx, p.PRDID, p.UserID)

	res, _, err := s.round(ctx, p)
	if err != nil {
		s.recordFailure(ctx, p.PRDID, p.UserID, "跑规划轮次失败", err, p.Answer)
		return nil, err
	}
	return res, nil
}

// StartRound 抢占一轮并在后台跑，立即返回。
//
// 为什么要后台跑：一轮 3~10 分钟，原来挂在同步 HTTP 请求上 —— 刷新、
// 关标签页、网络抖一下，请求的 ctx 一取消 agent 就被杀，几分钟的工作
// 白跑，失败原因也只有发起请求的那个页面看得到。现在请求只负责抢占
// （CAS 冲突照旧同步返回 ErrPRDBusy），之后的进度靠界面轮询 processing、
// 结果看 prd_rounds、失败看 prds.last_error。
//
// 一轮没有问题要问时自动推进到下一阶段（见 maxChainedRounds）；推进到
// 定稿且没有问题时顺带跑对抗复核（D10-6 必跑，原来要人再点一次）。
// 整串共用一次抢占，每轮开始续期，人在中途发不进第二个请求。
func (s *Service) StartRound(ctx context.Context, p RunRoundParams) error {
	if _, err := s.Store.BeginPRDRound(ctx, p.PRDID, p.UserID); err != nil {
		return err
	}
	s.background(ctx, p.PRDID, p.UserID, func(bg context.Context) {
		s.chain(bg, p)
	})
	return nil
}

// StartReview 抢占并在后台跑对抗复核，立即返回（理由同 StartRound）。
func (s *Service) StartReview(ctx context.Context, prdID, userID int64) error {
	if _, err := s.Store.BeginPRDRound(ctx, prdID, userID); err != nil {
		return err
	}
	s.background(ctx, prdID, userID, func(bg context.Context) {
		if _, err := s.review(bg, prdID, userID); err != nil {
			s.recordFailure(bg, prdID, userID, "对抗复核失败", err, "")
		}
	})
	return nil
}

// Wait 等所有后台运行结束（测试用；优雅退出时也可调用）。
func (s *Service) Wait() { s.bg.Wait() }

// background 在后台跑 fn，结束（含 panic）时释放抢占。
//
// ctx 去掉取消：发起请求的连接断开不该杀掉这一轮 —— 那正是要修的问题。
// 进程退出时 agent 子进程随之结束，processing 由僵死判定兜底放行。
func (s *Service) background(ctx context.Context, prdID, userID int64, fn func(context.Context)) {
	bg := context.WithoutCancel(ctx)
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		defer s.endRound(bg, prdID, userID)
		defer func() {
			if r := recover(); r != nil {
				slog.Error("PRD 后台运行 panic", "prd", prdID, "panic", r)
				s.recordFailure(bg, prdID, userID, "规划管线内部错误", fmt.Errorf("%v", r), "")
			}
		}()
		fn(bg)
	}()
}

// chain 跑一轮，没有问题就接着推进，直到有问题要问、到达定稿或出错。
func (s *Service) chain(ctx context.Context, p RunRoundParams) {
	for i := 0; i < maxChainedRounds; i++ {
		if i > 0 {
			if err := s.Store.TouchPRDRound(ctx, p.PRDID, p.UserID); err != nil {
				slog.Warn("续期 PRD 轮次失败", "prd", p.PRDID, "err", err)
			}
		}
		res, stage, err := s.round(ctx, p)
		if err != nil {
			s.recordFailure(ctx, p.PRDID, p.UserID, "跑规划轮次失败", err, p.Answer)
			return
		}
		if len(res.Questions) > 0 {
			return // 球在人这边
		}
		if stage == StageFinalize {
			s.autoReview(ctx, p.PRDID, p.UserID)
			return
		}
		// 人中途放弃了（或别处把它推进了评审）：不再自动往下跑。
		row, err := s.Store.GetPRD(ctx, p.PRDID, p.UserID)
		if err != nil || State(row.State) != StateDrafting {
			return
		}
		p.Answer, p.Stage = "", ""
	}
}

// autoReview 在定稿轮之后顺带跑对抗复核。没有 AC 时安静跳过（不是失败）。
func (s *Service) autoReview(ctx context.Context, prdID, userID int64) {
	if err := s.Store.TouchPRDRound(ctx, prdID, userID); err != nil {
		slog.Warn("续期 PRD 轮次失败", "prd", prdID, "err", err)
	}
	if _, err := s.review(ctx, prdID, userID); err != nil {
		if errors.Is(err, ErrStageUnavailable) {
			return
		}
		s.recordFailure(ctx, prdID, userID, "对抗复核失败", err, "")
	}
}

func (s *Service) endRound(ctx context.Context, prdID, userID int64) {
	if err := s.Store.EndPRDRound(context.WithoutCancel(ctx), prdID, userID); err != nil {
		slog.Warn("释放 PRD 轮次标记失败", "prd", prdID, "err", err)
	}
}

// recordFailure 把失败落到 prds.last_error 并写日志。
//
// 原来规划轮次的失败只回给发起请求的那个页面：服务端日志不记、库里不留，
// 事后查「第 6 轮为什么没跑成」只能翻用户自己贴回来的截图。
func (s *Service) recordFailure(ctx context.Context, prdID, userID int64, what string, err error, input string) {
	msg := head(what+": "+err.Error(), 2000)
	slog.Warn("PRD 后台运行失败", "prd", prdID, "what", what, "err", err)
	if ferr := s.Store.FailPRDRun(context.WithoutCancel(ctx), prdID, userID, msg, input); ferr != nil {
		slog.Warn("记录 PRD 运行失败本身失败", "prd", prdID, "err", ferr)
	}
}

// round 在已持有抢占的前提下跑一轮，返回解析结果与本轮阶段。
func (s *Service) round(ctx context.Context, p RunRoundParams) (*RoundResult, Stage, error) {
	row, err := s.Store.GetPRD(ctx, p.PRDID, p.UserID)
	if err != nil {
		return nil, "", err
	}

	repo, err := s.Store.RepoForPRD(ctx, p.PRDID, p.UserID)
	if err != nil {
		return nil, "", err
	}

	rounds, err := s.Store.ListPRDRounds(ctx, p.PRDID)
	if err != nil {
		return nil, "", err
	}

	stage := p.Stage
	if stage == "" {
		stage = nextStage(rounds)
	}
	if !stage.Valid() {
		return nil, "", fmt.Errorf("prd: 未知阶段 %q", stage)
	}

	doc, err := ParseDocument(row.Document)
	if err != nil {
		return nil, "", fmt.Errorf("prd: 解析上一轮快照失败: %w", err)
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

	var parsed *RoundResult
	_, sessionID, err := s.runAgent(ctx, runSpec{
		prdID:  p.PRDID,
		round:  &round,
		phase:  store.PRDPhasePlan,
		prompt: BuildRoundPrompt(in),
		repo:   repo,
		// 阶段 A 明令先不读代码，省一次检出与回收（docs/10 §1.3）。
		needCheckout: stage != StageUnderstand,
		accept: func(text string) error {
			r, err := ParseRound(text, RoundParse{
				Stage: stage, Base: prev, Round: round, MaxQuestions: QuestionBudget(stage),
			})
			if err != nil {
				return err
			}
			parsed = r
			return nil
		},
	})
	if err != nil {
		// 修正也救不回来：不推进轮次、不落快照，这一轮等于没发生。
		return nil, stage, fmt.Errorf("prd: 第 %d 轮没有跑成: %w", round, err)
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
		return nil, stage, err
	}
	return parsed, stage, nil
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
	// accept 校验终局文本；返回错误时在同一会话里要求修正（见 maxRepairs）。
	accept func(text string) error
}

// ErrOutputRejected 表示 agent 跑完了，但输出经修正仍未通过校验。
var ErrOutputRejected = errors.New("prd: 输出不合规")

// runAgent 起一次只读 agent，落事件，校验输出（必要时就地修正），返回
// 终局结果与会话 ID。
//
// 四条纪律，前三条是 preview/recommend.go 立的先例：
//  1. PermissionMode="plan" —— 只读，不许改目标仓库任何东西。
//  2. 每轮新 SessionID，不跨轮 --resume —— worktree 每轮回收，跨轮续跑的
//     会话会以为自己还在已删目录里（D10-8 的直接后果）。
//  3. 事件走 agent.BatchSink 边跑边落库（与任务执行链路同一套有界缓冲）。
//  4. **同一轮之内**的输出修正用 --resume 续同一个会话：检出此时还在
//     （defer 回收在修正之后），模型带着刚才读过的全部上下文改 JSON，
//     几十秒就够；重新起一轮则要把读代码与推理全部重来。
func (s *Service) runAgent(ctx context.Context, spec runSpec) (*agent.Result, string, error) {
	var dir string
	if !spec.needCheckout {
		// 不检出也不能留空：留空即继承 serve 的 cwd（见 NeutralDir）
		d, err := s.neutralDir()
		if err != nil {
			return nil, "", err
		}
		dir = d
	} else {
		if _, err := s.Worktrees.EnsureMirror(ctx, spec.repo.ProviderRepo, s.cloneURL(spec.repo.ProviderRepo)); err != nil {
			return nil, "", fmt.Errorf("prd: 准备镜像失败: %w", err)
		}

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

	// 有界缓冲 + 批量刷库。Close 必须在判错之前：跑挂了的那一轮，
	// 事件流正是人要看的东西（Close 会 drain 完剩余事件才返回）。
	sink := agent.NewBatchSink(ctx, func(wctx context.Context, entries []agent.Entry) error {
		return s.Store.InsertPRDEvents(wctx, spec.prdID, spec.round, spec.phase, entries)
	}, fmt.Sprintf("prd=%d phase=%s", spec.prdID, spec.phase))
	defer sink.Close()

	budgetCtx, cancelBudget := context.WithTimeout(ctx, roundBudget)
	defer cancelBudget()

	params := agent.RunParams{
		Prompt:         spec.prompt,
		Dir:            dir,
		SessionID:      sessionID,
		PermissionMode: "plan",
		SettingSources: s.SettingSources,
		ExtraEnv:       channelEnv(s.Channel),
		OnEvent:        sink.OnEvent,
	}

	timeout := roundTimeout
	for attempt := 0; ; attempt++ {
		res, err := s.runOnce(budgetCtx, params, timeout)
		if err != nil {
			return nil, sessionID, err
		}
		if spec.accept == nil {
			return res, sessionID, nil
		}
		verr := spec.accept(res.Text)
		if verr == nil {
			return res, sessionID, nil
		}

		slog.Warn("PRD agent 输出未通过校验", "prd", spec.prdID, "phase", spec.phase,
			"attempt", attempt+1, "err", verr, "head", head(res.Text, 300))
		if attempt >= maxRepairs {
			return nil, sessionID, fmt.Errorf("%w（已就地修正 %d 次）: %v", ErrOutputRejected, attempt, verr)
		}
		if dl, ok := budgetCtx.Deadline(); ok && time.Until(dl) < minRepairWindow {
			return nil, sessionID, fmt.Errorf("%w（时间预算不足以再修正）: %v", ErrOutputRejected, verr)
		}

		params.Resume = true
		params.Prompt = repairPrompt(verr)
		timeout = repairTimeout
	}
}

// runOnce 跑一次 agent（首跑或修正），把各种失败形态统一成错误。
func (s *Service) runOnce(ctx context.Context, p agent.RunParams, timeout time.Duration) (*agent.Result, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, runErr := s.Agent.Run(runCtx, p)
	if runErr != nil {
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("prd: agent 超时（%s 内没有交出结果）: %w", timeout, runErr)
		}
		return nil, fmt.Errorf("prd: agent 执行失败: %w", runErr)
	}
	if res == nil {
		return nil, errors.New("prd: agent 没有返回结果")
	}
	if res.IsError || !res.Success {
		return nil, fmt.Errorf("prd: agent 报告失败（%s）: %s", res.TerminalReason, head(res.Text, 200))
	}
	return res, nil
}

// repairPrompt 是同一会话里要求修正输出的指令。
//
// 强调「整个替代」：模型的直觉是只回一个小补丁修那一处，而系统会把这次
// 输出当作本轮的全部改动 —— 只回修那一处的话，上一条里其他改动就丢了。
func repairPrompt(verr error) string {
	return fmt.Sprintf(`你上一条输出没有通过系统校验，被拒收了：

%s

请重新输出一个 JSON 对象，格式与要求同上一条指令。它会**整个替代**你上一条输出
（不是叠加在它上面），所以要包含上一条里所有仍然要的改动，只修正上面指出的问题。
不需要重新读代码。只输出 JSON，不要解释。`, head(verr.Error(), 1500))
}

// neutralDir 返回不检出仓库时的中立工作目录（惰性创建）。
func (s *Service) neutralDir() (string, error) {
	dir := s.NeutralDir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "lathe-prd")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("prd: 创建中立目录失败: %w", err)
	}
	return dir, nil
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
	// 一轮要跑几分钟，期间人可能在文档面板上确认了几节、答复了几条反向
	// 确认。补丁是合并进「本轮开始时」的快照的，不把这些动作搬过来，落库
	// 就会把它们静默覆盖掉。
	if cur, err := s.Store.GetPRD(ctx, spec.prdID, spec.userID); err == nil {
		if latest, err := ParseDocument(cur.Document); err == nil {
			carryHumanEdits(spec.result.Document, latest)
		}
	}

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

	// 有问题要问就挂起等人答（drafting → awaiting_answers），没有就回到
	// 起草（awaiting_answers → drafting）。这两个状态的区别是给人看的：
	// awaiting_answers 意味着「球在你这边」。
	//
	// 原来只有前一条边：人答完之后再没有任何代码把状态转回 drafting，
	// 而提交评审要求 drafting —— 一份 PRD 只要问过一次问题，就永远提交
	// 不了评审（dev 实例那份 11 轮的 PRD 就卡在这里）。
	from, to := StateAwaitingAnswers, StateDrafting
	if len(spec.result.Questions) > 0 {
		from, to = StateDrafting, StateAwaitingAnswers
	}
	if _, err := s.Store.TransitionPRD(ctx, spec.prdID, spec.userID, store.TransitionPRDParams{
		From: string(from), To: string(to),
	}); err != nil && !errors.Is(err, store.ErrPRDStateChanged) {
		// CAS 失败说明状态本来就是目标状态，或已经被人改了（比如同时点了
		// 放弃）—— 都不是错误，这一轮的产物已经落库了。
		return err
	}
	return nil
}

// carryHumanEdits 把 latest（库里的当前快照）上人做的动作搬到 next 上：
//   - 节的确认：next 里该节正文与 latest 相同（本轮没改它）而 latest 是
//     confirmed，就沿用确认；
//   - AC 的反向确认答复：反向确认原文相同、next 里还没有答复，就沿用。
//
// 只搬「本轮没改」的部分：模型改过的节必须重新过目，这是逐节确认的意义。
func carryHumanEdits(next, latest *Document) {
	if next == nil || latest == nil {
		return
	}
	cur := map[string]Section{}
	for _, s := range latest.Sections() {
		cur[s.Key] = *s.Section
	}
	for _, s := range next.Sections() {
		if c, ok := cur[s.Key]; ok && c.Status == StatusConfirmed && c.Body == s.Section.Body {
			s.Section.Status = StatusConfirmed
		}
	}

	answers := map[string]Criterion{}
	for _, c := range latest.Criteria {
		answers[c.ID] = c
	}
	for i := range next.Criteria {
		c := &next.Criteria[i]
		if old, ok := answers[c.ID]; ok && c.ReverseAccepted == nil &&
			old.ReverseAccepted != nil && old.ReverseConfirm == c.ReverseConfirm {
			v := *old.ReverseAccepted
			c.ReverseAccepted = &v
		}
	}
}

// RunReview 同步跑对抗复核（docs/10 §5.5），是进 ready_for_review 的必经步骤。
// 界面走 StartReview（后台跑），同步入口留给测试。
func (s *Service) RunReview(ctx context.Context, prdID, userID int64) (*ReviewReport, error) {
	if _, err := s.Store.BeginPRDRound(ctx, prdID, userID); err != nil {
		return nil, err
	}
	defer s.endRound(ctx, prdID, userID)

	rep, err := s.review(ctx, prdID, userID)
	if err != nil && !errors.Is(err, ErrStageUnavailable) {
		s.recordFailure(ctx, prdID, userID, "对抗复核失败", err, "")
	}
	return rep, err
}

// review 在已持有抢占的前提下跑对抗复核。
//
// 三条纪律与规划轮次不同，都是刻意的：
//  1. **不检出仓库。** 复核只看 §1–§4 与 §7，不看 §5 §6 —— 连代码都不给它
//     读，是为了不被作者的推理带偏。它要回答的是「这组 AC 本身有没有洞」，
//     不是「这个方案对不对」。
//  2. **不进轮次序列。** round 传 nil：它是定稿前的一次性动作，不是对话的
//     一轮。事件流里按 phase=plan-review 与规划事件分开显示。
//  3. **报告不带处置。** Disposition 由人在 UI 填 —— 处置是人的责任，
//     让复核自己给自己判「这条不用改」等于没复核。
func (s *Service) review(ctx context.Context, prdID, userID int64) (*ReviewReport, error) {
	row, err := s.Store.GetPRD(ctx, prdID, userID)
	if err != nil {
		return nil, err
	}
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

	var report *ReviewReport
	if _, _, err := s.runAgent(ctx, runSpec{
		prdID:  prdID,
		phase:  store.PRDPhasePlanReview,
		prompt: BuildReviewPrompt(doc),
		repo:   repo,
		// 不检出：复核不读代码（见上）。
		needCheckout: false,
		accept: func(text string) error {
			r, err := ParseReviewResult(text, doc)
			if err != nil {
				return err
			}
			report = r
			return nil
		},
	}); err != nil {
		return nil, err
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
