package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Clouditera/lathe/internal/integration/agent"
	"github.com/Clouditera/lathe/internal/integration/github"
	"github.com/Clouditera/lathe/internal/integration/linear"
	"github.com/Clouditera/lathe/internal/task"
)

// 下面这些窄接口让流水线可被完整单测，无需真实的 Linear/GitHub/claude。

// LinearAPI 是流水线用到的 Linear 能力。
type LinearAPI interface {
	Issue(ctx context.Context, id string) (*linear.Issue, error)
	Comment(ctx context.Context, issueID, body string) (string, error)
}

// GitHubAPI 是流水线用到的 GitHub 能力。
type GitHubAPI interface {
	CreatePR(ctx context.Context, p github.PRParams) (*github.PullRequest, error)
}

// Clients 按需提供各集成的客户端。
//
// 凭据可在界面上随时修改，因此客户端不能在启动时固定 —— 每次执行
// 任务时现取，改完凭据无需重启即可生效。
type Clients interface {
	Linear(ctx context.Context) (LinearAPI, error)
	GitHub(ctx context.Context) (GitHubAPI, error)
}

// ClientFactory 按任务属主解析客户端（P1.5 第二步）。
//
// 多用户之后，任务必须用【属主自己的】Linear/GitHub 凭据跑：issue
// 从谁的 Linear 视角读、PR 以谁的身份开、回帖落在谁的单上。启动时
// 固定一套客户端等于把所有用户的任务都跑在管理员账号下。
type ClientFactory interface {
	ForUser(ctx context.Context, userID int64) (Clients, error)
}

// AgentDriver 驱动 agent 执行。
type AgentDriver interface {
	Run(ctx context.Context, p agent.RunParams) (*agent.Result, error)
}

// Notifier 推送通知给用户（D4 失败三件套之一）。
type Notifier interface {
	Notify(ctx context.Context, message string) error
}

// VerificationRecorder 把每个验证步骤落库（verifications 表）。
//
// heavy 档的 repro_fail → repro_pass 是「红-绿证明」的可审计落痕，
// 任务详情页直接展示。store.Store 实现此接口。
type VerificationRecorder interface {
	InsertVerification(ctx context.Context, taskID int64, tier, step, status string, durationMS int64) error
}

// NewSessionID 生成会话 ID。抽成字段便于测试注入确定值。
type NewSessionID func() string

// Pipeline 把一个任务从 queued 跑到 pr_open。
type Pipeline struct {
	Tasks     *task.Machine
	Worktrees *WorktreeManager
	Verifier  *Verifier
	Agent     AgentDriver
	Clients   Clients
	Notifier  Notifier
	NewID     NewSessionID

	// ClientFactory 非空时优先于 Clients：按任务属主解析客户端。
	// 为 nil 时用静态 Clients（单用户部署与测试）。
	ClientFactory ClientFactory

	// Verifications 记录验证步骤；为 nil 时只回帖不落库（测试用）。
	Verifications VerificationRecorder

	// AgentEvents 记录提炼后的 agent 事件流与实现阶段终局摘要
	// （docs/04-agent-visibility.md）；为 nil 时整个可见性机制关闭。
	AgentEvents AgentEventRecorder

	// Gates 是验证阶段的双通道闸门（§6.2）；为 nil 时不限流。
	// 档位在 diff 产出后才可判定（§5.1），因此闸门落在验证阶段而非
	// 派发时：实现可以并发，真正稀缺的验证资源按档位排队。
	Gates *VerifyGates

	// PermissionMode 传给 agent；无人值守通常用 acceptEdits。
	PermissionMode string
	// SettingSources 传给 agent 的 --setting-sources（§9：收敛上下文
	// 基线成本，只加载目标仓库自己的配置，排除个人插件）。
	SettingSources string
	// MaxFixAttempts 是 §5 修复回路的轮数上限：验证失败 → resume 原
	// 实现会话就地修复 → 重新验证。0 关闭回路（验证一挂即任务失败）。
	MaxFixAttempts int
	// ExcludeDirs 是仓库级的验证扫描排除目录（如 CloudRouter 的 upstream）。
	ExcludeDirs []string

	// TriageDir 是分诊 agent 的工作目录。分诊在建 worktree 之前执行，
	// 没有属于任务的目录；不指定的话子进程继承 serve 的 cwd（通常是
	// Lathe 自己的仓库根），--setting-sources project 会把 Lathe 的
	// CLAUDE.md/配置灌进目标仓库的分诊上下文 —— 既污染判断又白烧
	// token（路线图 B2-3）。为空时用系统临时目录下的固定位置：中立
	// 目录必须位于任何项目树之外（claude 会向上收集祖先目录的
	// CLAUDE.md），WorkspaceRoot 默认在仓库根之下，不合格。
	TriageDir string

	// TriageChannel / ImplementChannel 是 cc-switch 通道名（B2-2 模型
	// 路由）：分诊走便宜通道、实现与修复回路走强通道。非空时以
	// LATHE_AGENT_CHANNEL 注入 agent 子进程，由 claude wrapper 解析
	// 成实际的 BASE_URL/TOKEN；为空则跟随 cc-switch 当前激活通道。
	TriageChannel    string
	ImplementChannel string
}

// ExecuteParams 描述一次流水线执行。
type ExecuteParams struct {
	TaskID   int64
	Repo     RepoConfig
	CloneURL string
	IssueID  string // Linear issue 的 UUID
	Actor    string

	// Retry 非空表示这是一次重试：Fresh 为真时丢弃现场从头重建（等价于
	// 不传 Retry）；否则从 Retry.Entry 断点续跑，复用任务行上的
	// worktree/分支/会话。决策由 runner.PlanRetry 在派发前完成。
	Retry *RetryPlan
}

// errHalt 是阶段正常终止的哨兵错误（如分诊判定单子不明确转 blocked_spec）：
// 不是失败，Execute 把它翻译为 nil。
var errHalt = errors.New("pipeline: 任务正常终止（非失败）")

// runCtx 携带一次流水线执行的上下文，在阶段函数间传递。
type runCtx struct {
	ctx    context.Context
	params ExecuteParams
	actor  string
	lin    LinearAPI
	gh     GitHubAPI

	tk   *task.Task // 任务行（每次转移后刷新）
	plan *RetryPlan // 非空表示断点续跑
	// failStage 是本次重试针对的失败阶段（tasks.failure_stage），
	// 影响续跑 prompt 的选择（实现中断 vs 验证未通过）。
	failStage    Stage
	issue        *linear.Issue
	kind         TaskKind
	wt           *Worktree
	implSession  string
	tier         VerifyTier // 续跑时沿用任务行上的首次定档
	commitMsg    string     // 实现阶段产出，提交阶段消费
	agentSummary string     // 实现阶段终局摘要，写进 PR body
	report       Report

	retryNoted bool // 重试决策是否已落进某次转移的 payload
}

// takeRetryPayload 把重试决策附进续跑后的第一次状态转移（且仅第一次），
// 让决策依据在任务事件流里可见。
func (rc *runCtx) takeRetryPayload() map[string]any {
	if rc.plan == nil || rc.retryNoted {
		return nil
	}
	rc.retryNoted = true
	return map[string]any{"retry": map[string]any{
		"fresh":          rc.plan.Fresh,
		"entry":          string(rc.plan.Entry),
		"resume_session": rc.plan.ResumeSession,
		"reasons":        rc.plan.Reasons,
	}}
}

// Execute 跑完整条链路：分诊 → 实现 → 提交 → 验证 → 开 PR → 回帖。
//
// params.Retry 携带断点续跑决策时，从指定入口进入而非从头跑：
// 失败阶段越靠后，重放的成本越不该重来 —— 创建 PR 抖动不该重烧一遍
// 实现与验证。任一步失败都会走 fail()：回帖说明原因 + 保留 worktree
// 现场 + 推送通知，且不自动重试（D4）。
func (p *Pipeline) Execute(ctx context.Context, params ExecuteParams) error {
	rc, entry, err := p.setup(ctx, params)
	if err != nil {
		return err
	}

	// 阶段编排：entry 决定从哪进入；每个阶段自己完成状态转移，
	// 失败时自己走 fail() 三件套并返回错误。
	if entry == EntryTriage {
		if err := p.stageTriage(rc); err != nil {
			return unwind(err)
		}
		entry = EntryImplement
	}
	if entry == EntryImplement {
		if err := p.stageImplement(rc); err != nil {
			return unwind(err)
		}
		entry = EntryCommit
	}
	if entry == EntryCommit {
		if err := p.stageCommit(rc); err != nil {
			return unwind(err)
		}
		entry = EntryVerify
	}
	if entry == EntryVerify {
		if err := p.stageVerify(rc); err != nil {
			return unwind(err)
		}
	}
	return p.stagePushAndPR(rc)
}

// unwind 把阶段正常终止的哨兵翻译为 nil。
func unwind(err error) error {
	if errors.Is(err, errHalt) {
		return nil
	}
	return err
}

// setup 解析客户端并（续跑时）从任务行重建现场句柄。
//
// 续跑入口的 setup 失败（如 Linear 暂时拉不到 issue）不转 failed：
// 任务留在 queued，人可再次重试 —— 与「凭据未配」的既有处理一致。
func (p *Pipeline) setup(ctx context.Context, params ExecuteParams) (*runCtx, EntryStage, error) {
	tk, err := p.Tasks.Get(ctx, params.TaskID)
	if err != nil {
		return nil, "", err
	}
	actor := params.Actor
	if actor == "" {
		actor = "system"
	}

	// 凭据可能在任务排队期间被改过，因此每次执行都现取客户端。
	// 多用户：按任务属主解析，任务跑在谁的账号下一目了然。
	clients := p.Clients
	if p.ClientFactory != nil {
		clients, err = p.ClientFactory.ForUser(ctx, tk.UserID)
		if err != nil {
			return nil, "", fmt.Errorf("解析任务属主的凭据失败: %w", err)
		}
	}
	lin, err := clients.Linear(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("获取 Linear 客户端失败（请在设置里配置并验证凭据）: %w", err)
	}
	gh, err := clients.GitHub(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("获取 GitHub 客户端失败（请在设置里配置并验证凭据）: %w", err)
	}

	rc := &runCtx{ctx: ctx, params: params, actor: actor, lin: lin, gh: gh, tk: tk}
	entry := EntryTriage
	if params.Retry != nil {
		// 决策全程保留（含 Fresh 重建）：第一次状态转移会把决策理由
		// 落进任务事件流（takeRetryPayload），重试不再是黑盒。
		rc.plan = params.Retry
		if !params.Retry.Fresh {
			entry = params.Retry.Entry
		}
	}
	if tk.FailureStage != nil {
		rc.failStage = Stage(*tk.FailureStage)
	}
	if entry == EntryTriage {
		return rc, entry, nil // 全流程：issue 由分诊阶段拉取
	}

	// ---- 断点续跑：重建现场句柄 ----
	// issue 拉最新：重试间隔里提单人可能补充了信息，PR 标题与回帖都用它。
	issue, err := lin.Issue(ctx, params.IssueID)
	if err != nil {
		return nil, "", fmt.Errorf("续跑前拉取 issue 失败: %w", err)
	}
	rc.issue = issue

	rc.kind = KindFix
	if tk.TaskKind != nil && *tk.TaskKind != "" {
		rc.kind = TaskKind(*tk.TaskKind)
	}
	base, err := params.Repo.BaseBranch(rc.kind)
	if err != nil {
		return nil, "", err
	}
	rc.wt = &Worktree{
		Path:       deref(tk.WorktreePath),
		Branch:     deref(tk.BranchName),
		BaseBranch: base,
		Mirror:     p.Worktrees.MirrorPath(params.Repo.ProviderRepo),
	}
	if tk.AgentSessionID != nil {
		rc.implSession = *tk.AgentSessionID
	}
	if tk.VerifyTier != nil {
		rc.tier = VerifyTier(*tk.VerifyTier)
	}

	// 提交/验证入口的 diff 与基线工作区都解析自 mirror 命名空间，先刷新。
	if entry == EntryCommit || entry == EntryVerify {
		if _, err := p.Worktrees.EnsureMirror(ctx, params.Repo.ProviderRepo, params.CloneURL); err != nil {
			return nil, "", fmt.Errorf("续跑前刷新 mirror 失败: %w", err)
		}
	}
	return rc, entry, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ---------------------------------------------------------------- 阶段：分诊

// stageTriage 判断单子明确度与任务类型。单子不明确时回帖提问并转
// blocked_spec（产品边界：不猜），以 errHalt 正常终止。
func (p *Pipeline) stageTriage(rc *runCtx) error {
	if _, err := p.Tasks.Transition(rc.ctx, rc.tk.ID, task.StateTriaging, rc.actor, &task.TransitionOpts{
		Payload: rc.takeRetryPayload(),
	}); err != nil {
		return err
	}

	issue, err := rc.lin.Issue(rc.ctx, rc.params.IssueID)
	if err != nil {
		return p.fail(rc, StageFetchIssue, err)
	}
	rc.issue = issue

	triageDir, err := p.triageDir()
	if err != nil {
		return p.fail(rc, StageTriageRun, err)
	}
	triageSession := p.newID()
	triageSink := newEventSink(rc.ctx, p.AgentEvents, rc.tk.ID, "triage", triageDir, triageSession)
	triageRes, err := p.Agent.Run(rc.ctx, agent.RunParams{
		Prompt:         TriagePrompt(issue.Context()),
		Dir:            triageDir,
		SessionID:      triageSession,
		PermissionMode: "plan", // 分诊只读不写
		SettingSources: p.SettingSources,
		ExtraEnv:       channelEnv(p.TriageChannel),
		OnEvent:        triageSink.OnEvent,
	})
	// 成功与失败路径都必须 drain：失败时缓冲里恰是排障最关键的现场
	triageSink.Close()
	if err != nil {
		return p.fail(rc, StageTriageRun, err)
	}

	verdict, err := ParseTriageVerdict(triageRes.Text)
	if err != nil {
		return p.fail(rc, StageTriageParse, err)
	}

	if !verdict.Actionable {
		// 单子不明确：回帖提问并停下，不猜（产品边界）
		body := fmt.Sprintf("**Lathe 暂不能自动处理这个 issue**\n\n%s\n\n补充后重新指派给我即可。", verdict.Question)
		if _, cerr := rc.lin.Comment(rc.ctx, rc.params.IssueID, body); cerr != nil {
			slog.Warn("回帖失败", "task", rc.tk.ID, "err", cerr)
		}
		if _, err := p.Tasks.Transition(rc.ctx, rc.tk.ID, task.StateBlockedSpec, rc.actor, &task.TransitionOpts{
			FailureReason: strPtr(verdict.Reason),
			Payload:       map[string]any{"question": verdict.Question},
		}); err != nil {
			return err
		}
		return errHalt
	}

	rc.kind = verdict.Kind
	return nil
}

// ---------------------------------------------------------------- 阶段：实现

// stageImplement 建工作区并驱动 agent 写代码。
//
// 断点续跑（plan.Entry == EntryImplement）时不建新工作区：
//   - ResumeSession：--resume 原实现会话，agent 记得自己的思路；
//   - 否则会话凭据已丢失，在同一工作区开新会话，prompt 带全量需求与
//     现状说明（ReentryImplementPrompt）。
//
// resume 执行失败（claude 会话数据被清理/不在这台机器）不直接判死：
// 降级为新会话续跑，现场是好不容易攒下的，能救就救。
func (p *Pipeline) stageImplement(rc *runCtx) error {
	resume := rc.plan != nil && rc.plan.Entry == EntryImplement

	if !resume {
		wt, err := p.Worktrees.Create(rc.ctx, CreateParams{
			Repo: rc.params.Repo, CloneURL: rc.params.CloneURL,
			Kind: rc.kind, IssueKey: rc.issue.Identifier, Title: rc.issue.Title,
		})
		if err != nil {
			return p.fail(rc, StageCreateWorktree, err)
		}
		rc.wt = wt
		rc.implSession = p.newID()
	}

	useResume := resume && rc.plan.ResumeSession && rc.implSession != ""
	if !useResume {
		// 全新执行与「会话不可续的续跑」都开新会话；ID 在执行前落库，
		// 进程中途崩溃也留下可 --resume 的凭据。
		rc.implSession = p.newID()
	}

	kindStr := string(rc.kind)
	tk, err := p.Tasks.Transition(rc.ctx, rc.tk.ID, task.StateImplementing, rc.actor, &task.TransitionOpts{
		AgentSessionID: &rc.implSession,
		WorktreePath:   &rc.wt.Path,
		BranchName:     &rc.wt.Branch,
		TaskKind:       &kindStr,
		Payload:        rc.takeRetryPayload(),
	})
	if err != nil {
		return err
	}
	rc.tk = tk

	prompt, resumeFlag := p.implementPrompt(rc)
	implRes, err := p.runAgent(rc, "implement", prompt, rc.implSession, resumeFlag)

	// resume 失败降级：会话数据丢失（被清理/不在本机）时 claude --resume
	// 报错退出。降级为同工作区新会话收尾，而不是把可用现场一起判死。
	if err != nil && resumeFlag {
		slog.Warn("resume 会话失败，降级为同工作区新会话", "task", rc.tk.ID, "err", err)
		rc.implSession = p.newID()
		if serr := p.Tasks.SetSessionID(rc.ctx, rc.tk.ID, rc.implSession); serr != nil {
			slog.Warn("降级会话 ID 落库失败", "task", rc.tk.ID, "err", serr)
		}
		implRes, err = p.runAgent(rc, "implement-reentry",
			ReentryImplementPrompt(rc.issue.Context(), rc.kind, rc.wt.Branch), rc.implSession, false)
	}

	// 终局摘要落库含 fail 路径：有 result 就存（docs/04 §3.5）
	p.persistAgentResult(rc.ctx, rc.tk.ID, implRes)
	if err != nil {
		return p.fail(rc, StageImplementRun, err)
	}
	if implRes.IsError {
		return p.fail(rc, StageImplementIncomplete,
			fmt.Errorf("agent 返回 %s（%s）", implRes.Subtype, implRes.TerminalReason))
	}

	rc.commitMsg = fmt.Sprintf("%s(%s): %s\n\n%s",
		rc.kind, strings.ToLower(rc.issue.Identifier), rc.issue.Title, truncate(implRes.Text, 1000))
	rc.agentSummary = implRes.Text
	return nil
}

// implementPrompt 按续跑来源选择实现阶段的 prompt。
func (p *Pipeline) implementPrompt(rc *runCtx) (prompt string, resume bool) {
	if rc.plan == nil || rc.plan.Entry != EntryImplement {
		return ImplementPrompt(rc.issue.Context(), rc.kind, rc.wt.Branch), false
	}
	if rc.plan.ResumeSession && rc.implSession != "" {
		// 验证未通过后的重试：agent 记得实现，把失败原因喂回去修复
		if rc.failStage == StageVerifyFailed && rc.tk.FailureReason != nil {
			return ResumeFixPrompt(*rc.tk.FailureReason), true
		}
		return ContinueImplementPrompt(), true
	}
	return ReentryImplementPrompt(rc.issue.Context(), rc.kind, rc.wt.Branch), false
}

// runAgent 跑一次 agent 并接好事件汇。
//
// 实现与修复回路（同一会话的延续）都走 ImplementChannel：修复是
// 实现的下半场，通道不该中途换。
func (p *Pipeline) runAgent(rc *runCtx, phase, prompt, session string, resume bool) (*agent.Result, error) {
	sink := newEventSink(rc.ctx, p.AgentEvents, rc.tk.ID, phase, rc.wt.Path, session)
	res, err := p.Agent.Run(rc.ctx, agent.RunParams{
		Prompt:         prompt,
		Dir:            rc.wt.Path,
		SessionID:      session,
		Resume:         resume,
		PermissionMode: p.PermissionMode,
		SettingSources: p.SettingSources,
		ExtraEnv:       channelEnv(p.ImplementChannel),
		OnEvent:        sink.OnEvent,
	})
	sink.Close()
	return res, err
}

// channelEnv 把 cc-switch 通道名编成注入 agent 子进程的环境变量；
// 空通道表示跟随 cc-switch 当前激活通道（不注入）。
func channelEnv(channel string) []string {
	if strings.TrimSpace(channel) == "" {
		return nil
	}
	return []string{"LATHE_AGENT_CHANNEL=" + strings.TrimSpace(channel)}
}

// triageDir 返回分诊 agent 的中立工作目录（惰性创建）。
func (p *Pipeline) triageDir() (string, error) {
	dir := p.TriageDir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "lathe-triage")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("runner: 创建分诊中立目录失败: %w", err)
	}
	return dir, nil
}

// ---------------------------------------------------------------- 阶段：提交

// stageCommit 把工作区改动提交到任务分支。
//
// EntryCommit 续跑直接进入本阶段（状态先从 queued 转到 implementing ——
// 提交是实现收尾的语义）。工作区无未提交改动但分支已有提交时（上次已
// 提交过/人工提交过）跳过提交直接进验证；两者皆无才算「没干活」。
func (p *Pipeline) stageCommit(rc *runCtx) error {
	if rc.tk.State == task.StateQueued {
		tk, err := p.Tasks.Transition(rc.ctx, rc.tk.ID, task.StateImplementing, rc.actor, &task.TransitionOpts{
			Payload: rc.takeRetryPayload(),
		})
		if err != nil {
			return err
		}
		rc.tk = tk
	}

	changed, err := p.Worktrees.HasChanges(rc.ctx, rc.wt)
	if err != nil {
		return p.fail(rc, StageCheckChanges, err)
	}
	if !changed {
		ahead, aerr := p.Worktrees.HasCommitsAhead(rc.ctx, rc.wt)
		if aerr == nil && ahead {
			slog.Info("工作区无未提交改动且分支已有提交，跳过提交", "task", rc.tk.ID)
			return nil
		}
		return p.fail(rc, StageImplementNoChanges,
			errors.New("工作区无改动，视为未完成任务"))
	}

	commitMsg := rc.commitMsg
	if commitMsg == "" {
		// 续跑进入（无本轮 agent 摘要）：改动可能来自中断的 agent 或人工接手
		commitMsg = fmt.Sprintf("%s(%s): %s\n\n（断点续跑：提交工作区中遗留的改动）",
			rc.kind, strings.ToLower(rc.issue.Identifier), rc.issue.Title)
	}
	if err := p.Worktrees.Commit(rc.ctx, rc.wt, commitMsg); err != nil {
		return p.fail(rc, StageCommit, err)
	}
	return nil
}

// ---------------------------------------------------------------- 阶段：验证

// stageVerify 定档（续跑沿用首次定档）→ 双通道闸门 → 红绿验证 → 修复回路。
func (p *Pipeline) stageVerify(rc *runCtx) error {
	// §5.1：档位在 diff 产出后按实际改动面判定，而非接单时按单子文本猜。
	// 判定是确定性规则，理由落进任务事件供人复核。续跑时沿用首次定档：
	// 修复轮通常只收敛改动面，升档场景留给从头重跑。
	changedFiles, err := p.Worktrees.ChangedFiles(rc.ctx, rc.wt)
	if err != nil {
		return p.fail(rc, StageListChanges, err)
	}
	tier := rc.tier
	tierReasons := []string{"沿用首次定档（断点续跑）"}
	if tier == "" {
		tier, tierReasons = ClassifyTier(changedFiles, OverrideTier(rc.params.Repo.VerifyTierOverride))
	}
	tierStr := string(tier)
	payload := map[string]any{
		"tier_reasons":  tierReasons,
		"changed_files": len(changedFiles),
	}
	if rp := rc.takeRetryPayload(); rp != nil {
		payload["retry"] = rp["retry"]
	}
	tk, err := p.Tasks.Transition(rc.ctx, rc.tk.ID, task.StateVerifying, rc.actor, &task.TransitionOpts{
		VerifyTier: &tierStr,
		Payload:    payload,
	})
	if err != nil {
		return err
	}
	rc.tk = tk

	// 双通道限流：light/heavy 各自独立配额（§6.2）。等不到槽位且
	// ctx 结束时按失败处理 —— 现场保留，人可重新入队。
	release, err := p.Gates.Acquire(rc.ctx, tier)
	if err != nil {
		return p.fail(rc, StageVerifyGate, err)
	}
	defer release()

	steps, err := DetectLightProfile(rc.wt.Path, mergedExcludeDirs(p.ExcludeDirs, rc.params.Repo.ExcludeDirs)...)
	if err != nil {
		return p.fail(rc, StageVerifyDetect, err)
	}

	// 修复回路里要按最新 diff 重跑验证，抽成闭包共享判定逻辑。
	runVerify := func() (Report, error) {
		if tier == TierHeavy {
			return p.runHeavy(rc.ctx, rc.tk.ID, rc.params.Repo.ProviderRepo, rc.wt, steps, changedFiles, rc.params.Repo.ExcludeDirs)
		}
		return p.Verifier.RunLight(rc.ctx, rc.wt.Path, steps), nil
	}

	report, err := runVerify()
	if err != nil {
		return p.fail(rc, StageVerifyRun, err)
	}
	p.persistVerifications(rc.ctx, rc.tk.ID, report)

	// §5 修复回路：验证失败 → resume 原实现会话就地修复 → 重新验证。
	// 两种红阶段不成立不进回路：bug 没复现（blocked_spec，单子没说清，
	// 修代码没用）与执行环境错误（agent 改代码修不了流水线/环境，空烧轮次）。
	fixAttempts := 0
	for attempt := 1; attempt <= p.MaxFixAttempts && !report.Passed() && redStepFailure(report) == nil && redEnvError(report) == nil; attempt++ {
		f := report.FirstFailure()
		if f == nil {
			break
		}
		fixAttempts = attempt
		slog.Info("验证未通过，进入修复回路", "task", rc.tk.ID, "attempt", attempt, "step", f.Step.Name)

		// 续跑进入的任务可能没有可 resume 的会话（凭据丢失）：
		// 开新会话修复，并把新 ID 落库 —— 再失败的重试还能续上。
		fixSession := rc.implSession
		fixResume := true
		if fixSession == "" {
			fixSession = p.newID()
			fixResume = false
		}
		fixRes, ferr := p.runAgent(rc, fmt.Sprintf("fix-%d", attempt),
			FixPrompt(attempt, p.MaxFixAttempts, f, report.Summary()), fixSession, fixResume)
		p.persistAgentResult(rc.ctx, rc.tk.ID, fixRes)
		if ferr != nil {
			slog.Warn("修复轮执行失败，按当前验证结果收尾", "task", rc.tk.ID, "err", ferr)
			break
		}
		if !fixResume {
			if serr := p.Tasks.SetSessionID(rc.ctx, rc.tk.ID, fixSession); serr != nil {
				slog.Warn("修复会话 ID 落库失败", "task", rc.tk.ID, "err", serr)
			}
			rc.implSession = fixSession
		}
		if fixRes.IsError {
			slog.Warn("修复轮未成功完成", "task", rc.tk.ID, "subtype", fixRes.Subtype)
			break
		}
		// 没产生改动的修复轮等于没修 —— 跳出按失败收尾，不空烧轮次
		hasNew, cerr := p.Worktrees.HasChanges(rc.ctx, rc.wt)
		if cerr != nil {
			slog.Warn("检查修复改动失败", "task", rc.tk.ID, "err", cerr)
			break
		}
		if !hasNew {
			slog.Warn("修复轮没有产生任何改动", "task", rc.tk.ID, "attempt", attempt)
			break
		}
		fixMsg := fmt.Sprintf("fix(%s): 验证修复（第 %d 轮）\n\n%s",
			strings.ToLower(rc.issue.Identifier), attempt, truncate(fixRes.Text, 1000))
		if cerr := p.Worktrees.Commit(rc.ctx, rc.wt, fixMsg); cerr != nil {
			slog.Warn("提交修复改动失败", "task", rc.tk.ID, "err", cerr)
			break
		}
		// 修复可能改变改动面：重算文件清单再验证
		if nf, cerr := p.Worktrees.ChangedFiles(rc.ctx, rc.wt); cerr == nil {
			changedFiles = nf
		}
		report, err = runVerify()
		if err != nil {
			return p.fail(rc, StageVerifyRun, err)
		}
		p.persistVerifications(rc.ctx, rc.tk.ID, report)
	}

	// heavy 档的红阶段立不起来（bug 没复现/复现跑不起来）不是任务失败，
	// 是单子没说清 —— §5.3 规定转 blocked_spec 回帖请人补充复现步骤。
	if red := redStepFailure(report); red != nil {
		body := fmt.Sprintf("**Lathe 无法证明这个修复有效，已暂停**\n\n%s\n\n复现测试在改动前的代码上没有失败 —— 可能是 bug 描述与实际不符，或复现条件缺失。请补充复现步骤后重新指派给我。\n\n```\n%s```",
			red.Err, report.Summary())
		if _, cerr := rc.lin.Comment(rc.ctx, rc.params.IssueID, body); cerr != nil {
			slog.Warn("回帖失败", "task", rc.tk.ID, "err", cerr)
		}
		if _, err := p.Tasks.Transition(rc.ctx, rc.tk.ID, task.StateBlockedSpec, rc.actor, &task.TransitionOpts{
			FailureReason: strPtr(red.Err.Error()),
			Payload:       map[string]any{"reason": "repro_not_red"},
		}); err != nil {
			return err
		}
		return errHalt
	}

	// 红阶段的执行环境错误（命令起不来、超时、目录缺失等）既不是单子
	// 没说清，也不是 agent 改代码能修的 —— 按失败收尾，原因写清是执行
	// 问题，不进修复回路、不转 blocked_spec 骚扰提单人（任务 #596 的
	// 教训：流水线自身能力问题被误路由给 agent，空烧两轮还看不见）。
	if env := redEnvError(report); env != nil {
		cause := env.Err
		if cause == nil {
			cause = errors.New("复现测试未能在验证环境里执行")
		}
		return p.fail(rc, StageVerifyRun, fmt.Errorf("复现测试无法执行（heavy 档）: %w", cause))
	}

	if !report.Passed() {
		cause := report.Summary()
		if f := report.FirstFailure(); f != nil {
			header := fmt.Sprintf("（%s 档）：%s", tier, f.Step.Name)
			if fixAttempts > 0 {
				header = fmt.Sprintf("（%s 档，已尝试 %d 轮修复）：%s", tier, fixAttempts, f.Step.Name)
			}
			// 把首个失败的具体错误放最前 —— 回帖时人先看到原因再看摘要
			if f.Err != nil {
				cause = f.Err.Error() + "\n\n" + report.Summary()
			}
			cause = header + "\n" + cause
		}
		return p.fail(rc, StageVerifyFailed, errors.New(cause))
	}

	rc.report = report
	return nil
}

// ---------------------------------------------------------------- 阶段：推送与开 PR

// stagePushAndPR 推送分支、开 PR、回帖。
//
// EntryPush 续跑直接进入本阶段（验证在失败前已通过）：状态先从 queued
// 经 verifying 中转（状态机无 queued→pr_open 直达边），payload 注明
// 这是跳过重验的续跑。push 与 CreatePR 都是幂等的（后者复用既有 PR）。
func (p *Pipeline) stagePushAndPR(rc *runCtx) error {
	if rc.tk.State == task.StateQueued {
		tk, err := p.Tasks.Transition(rc.ctx, rc.tk.ID, task.StateVerifying, rc.actor, &task.TransitionOpts{
			Payload: rc.takeRetryPayload(),
		})
		if err != nil {
			return err
		}
		rc.tk = tk
	}

	if err := p.Worktrees.Push(rc.ctx, rc.wt, rc.params.Repo); err != nil {
		return p.fail(rc, StagePush, err)
	}

	// EntryPush 续跑没有本轮验证报告与实现摘要（验证在失败前已通过，
	// 证据在 verifications 表与任务详情页）。PR body 如实注明，不伪造摘要。
	verifySummary := ""
	if rc.report.Results != nil {
		verifySummary = rc.report.Summary()
	} else {
		verifySummary = "验证已在重试前通过（断点续跑，证据见任务验证记录）"
	}

	pr, err := rc.gh.CreatePR(rc.ctx, github.PRParams{
		ProviderRepo: rc.params.Repo.ProviderRepo,
		Head:         rc.wt.Branch,
		Base:         rc.wt.BaseBranch,
		Title:        fmt.Sprintf("%s(%s): %s", rc.kind, strings.ToLower(rc.issue.Identifier), rc.issue.Title),
		Body:         github.BuildPRBody(rc.issue.Identifier, rc.issue.URL, verifySummary, rc.agentSummary),
	})
	if err != nil {
		return p.fail(rc, StageCreatePR, err)
	}

	if _, err := p.Tasks.Transition(rc.ctx, rc.tk.ID, task.StatePROpen, rc.actor, &task.TransitionOpts{
		PRURL:   &pr.URL,
		Payload: map[string]any{"pr_number": pr.Number, "reused": pr.Existing},
	}); err != nil {
		return err
	}

	body := fmt.Sprintf("**Lathe 已完成并开出 PR**\n\n%s\n\n```\n%s```\n\n请人工复核后合并。",
		pr.URL, verifySummary)
	if _, err := rc.lin.Comment(rc.ctx, rc.params.IssueID, body); err != nil {
		slog.Warn("回帖失败", "task", rc.tk.ID, "err", err)
	}

	slog.Info("任务完成", "task", rc.tk.ID, "issue", rc.issue.Identifier, "pr", pr.URL)
	return nil
}

// runHeavy 跑 heavy 档验证：建基线工作区 → 识别复现测试 → 红-绿-回归。
//
// 基线工作区用完即拆（defer Remove force）：它是验证的临时道具，
// 不是失败三件套要保留的现场 —— 现场指任务工作区本身。
// mergedExcludeDirs 合并节点级（Pipeline.ExcludeDirs）与仓库级
// （RepoConfig.ExcludeDirs，来自 repos.exclude_dirs）的验证排除目录。
func mergedExcludeDirs(global, repo []string) []string {
	if len(repo) == 0 {
		return global
	}
	out := make([]string, 0, len(global)+len(repo))
	out = append(out, global...)
	return append(out, repo...)
}

func (p *Pipeline) runHeavy(ctx context.Context, taskID int64, providerRepo string, wt *Worktree, lightSteps []Step, changedFiles []string, repoExclude []string) (Report, error) {
	base, err := p.Worktrees.CreateDetached(ctx, providerRepo, wt.BaseBranch, fmt.Sprintf("task-%d-base", taskID))
	if err != nil {
		return Report{}, err
	}
	defer func() {
		if rerr := p.Worktrees.Remove(ctx, base, true); rerr != nil {
			slog.Warn("回收基线工作区失败", "task", taskID, "err", rerr)
		}
	}()

	repro, reproErr := ResolveReproTests(wt.Path, changedFiles)
	regression := DetectRegression(wt.Path, changedFiles, mergedExcludeDirs(p.ExcludeDirs, repoExclude)...)

	// reproErr 是契约违例（没交测试/声明不合法），不是流水线执行错误：
	// 交给报告走红阶段的三分路由（修复回路/blocked_spec/失败），
	// 不走 p.fail 的「验证执行失败」。
	return p.Verifier.RunHeavy(ctx, HeavyParams{
		TaskPath:   wt.Path,
		BasePath:   base.Path,
		Light:      lightSteps,
		Repro:      repro,
		ReproErr:   reproErr,
		Regression: regression,
	}), nil
}

// redStepFailure 在 heavy 报告里找应转 blocked_spec 的红阶段结果：
// 复现测试在改动前的代码上【通过】了（StatusFailed）—— bug 没复现，
// 是单子没说清，§5.3 规定回帖请提单人补充复现步骤。
//
// 红阶段另外两种不成立不走这里：契约违例（ErrNoReproTests /
// ErrReproManifest，agent 能自己修）进修复回路；执行环境错误见
// redEnvError。
func redStepFailure(rep Report) *StepResult {
	if rep.Tier != TierHeavy {
		return nil
	}
	for i := range rep.Results {
		s := &rep.Results[i]
		if s.Step.Name == StepReproFail && s.Status == StatusFailed {
			if s.Err == nil {
				s.Err = errors.New("复现测试在改动前的代码上没有失败")
			}
			return s
		}
	}
	return nil
}

// redEnvError 在 heavy 报告里找「红阶段跑不起来且不是契约违例」的结果：
// 命令不存在、超时、目录缺失等执行环境问题。agent 改代码修不了它，
// 提单人更修不了 —— 由流水线按失败收尾并保留现场。
func redEnvError(rep Report) *StepResult {
	if rep.Tier != TierHeavy {
		return nil
	}
	for i := range rep.Results {
		s := &rep.Results[i]
		if s.Step.Name == StepReproFail && s.Status == StatusError && !isReproContractErr(s.Err) {
			return s
		}
	}
	return nil
}

// isReproContractErr 报告红阶段 error 是否属于 agent 可自己修掉的契约违例。
func isReproContractErr(err error) bool {
	return errors.Is(err, ErrNoReproTests) || errors.Is(err, ErrReproManifest)
}

// persistVerifications 把报告的每一步落库。落库失败只告警不中断 ——
// 证据缺失不该把一个已验证通过的改动挡在 PR 之外（结论仍在 PR 与回帖里）。
//
// 同步写一条 kind=verify_step 的 agent 事件（docs/04 §3.2）：verifications
// 表存结构化结果给红-绿判定，agent_events 存人读时间线，两者同源。
func (p *Pipeline) persistVerifications(ctx context.Context, taskID int64, rep Report) {
	if p.Verifications == nil && p.AgentEvents == nil {
		return
	}
	var entries []agent.Entry
	for _, s := range rep.Results {
		if p.Verifications != nil {
			if err := p.Verifications.InsertVerification(ctx, taskID,
				string(rep.Tier), string(s.Step.Name), string(s.Status),
				s.Duration.Milliseconds()); err != nil {
				slog.Warn("验证步骤落库失败", "task", taskID, "step", s.Step.Name, "err", err)
			}
		}
		entries = append(entries, verifyStepEntry(rep.Tier, s))
	}
	if p.AgentEvents != nil {
		if err := p.AgentEvents.InsertAgentEvents(ctx, taskID, "verify", entries); err != nil {
			slog.Warn("验证时间线落库失败", "task", taskID, "err", err)
		}
	}
}

// verifyStepEntry 把一步验证结果渲染成时间线条目。失败/跑不起来的步骤
// 附带截断后的输出 —— 时间线上最先要看的就是它。
func verifyStepEntry(tier VerifyTier, s StepResult) agent.Entry {
	mark := map[StepStatus]string{
		StatusPassed: "✓", StatusFailed: "✗",
		StatusError: "!", StatusSkipped: "–",
	}[s.Status]
	loc := s.Step.Dir
	if loc == "" {
		loc = "."
	}
	body := fmt.Sprintf("%s %s (%s) · %s", mark, s.Step.Name, loc, s.Duration.Round(time.Millisecond))
	if s.Err != nil {
		body += " · " + s.Err.Error()
	}
	if s.Status != StatusPassed && s.Output != "" {
		body += "\n\n" + truncate(s.Output, 4<<10)
	}
	return agent.Entry{
		Kind: "verify_step",
		Body: body,
		Payload: map[string]any{
			"tier":       string(tier),
			"step":       string(s.Step.Name),
			"status":     string(s.Status),
			"durationMs": s.Duration.Milliseconds(),
		},
	}
}

// persistAgentResult 落实现阶段的终局摘要四列。有 result 就存，与成败
// 无关；落库失败只告警（沿用 persistVerifications 的立场）。
func (p *Pipeline) persistAgentResult(ctx context.Context, taskID int64, res *agent.Result) {
	if p.AgentEvents == nil || res == nil {
		return
	}
	if err := p.AgentEvents.SetAgentSummary(ctx, taskID,
		res.Text, res.CostUSD, res.DurationMS, res.NumTurns); err != nil {
		slog.Warn("agent 摘要落库失败", "task", taskID, "err", err)
	}
}

// fail 执行 D4 失败三件套：回帖 + 保留现场 + 推送通知；不自动重试。
//
// 刻意不回收 worktree：现场留给人直接 cd 进去接着干；同时把机器可读的
// 失败阶段代码落进 tasks.failure_stage —— 人工重试时智能续跑（retry.go）
// 靠它决定从哪个阶段断点续跑，还是丢弃重建。
func (p *Pipeline) fail(rc *runCtx, stage Stage, cause error) error {
	reason := fmt.Sprintf("%s: %v", stage.label(), cause)
	slog.Error("任务失败", "task", rc.tk.ID, "stage", string(stage), "err", cause)

	// 1) 回帖到 Linear
	body := fmt.Sprintf("**Lathe 处理失败**\n\n阶段：%s\n\n```\n%s\n```\n", stage.label(), truncate(cause.Error(), 3000))
	if rc.wt != nil {
		body += fmt.Sprintf("\n工作区已保留在 `%s`（分支 `%s`），可直接进去接手；重试会优先续跑该现场。\n", rc.wt.Path, rc.wt.Branch)
	}
	if rc.lin != nil {
		if _, err := rc.lin.Comment(rc.ctx, rc.params.IssueID, body); err != nil {
			slog.Warn("失败回帖也失败了", "task", rc.tk.ID, "err", err)
		}
	}

	// 2) 保留现场：不调用 Worktrees.Remove

	// 3) 推送通知
	if p.Notifier != nil {
		msg := fmt.Sprintf("Lathe 任务 %d 失败（%s）", rc.tk.ID, stage.label())
		if err := p.Notifier.Notify(rc.ctx, msg); err != nil {
			slog.Warn("推送通知失败", "task", rc.tk.ID, "err", err)
		}
	}

	opts := &task.TransitionOpts{
		FailureReason: &reason,
		FailureStage:  strPtr(string(stage)),
		Payload:       map[string]any{"stage": string(stage)},
	}
	if _, err := p.Tasks.Transition(rc.ctx, rc.tk.ID, task.StateFailed, "system", opts); err != nil {
		return fmt.Errorf("任务失败(%s)，且状态转移也失败: %w（原因: %v）", stage.label(), err, cause)
	}
	return fmt.Errorf("任务失败于%s: %w", stage.label(), cause)
}

func (p *Pipeline) newID() string {
	if p.NewID != nil {
		return p.NewID()
	}
	return newUUID()
}

func strPtr(s string) *string { return &s }
