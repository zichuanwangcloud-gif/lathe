package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

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

	// Verifications 记录验证步骤；为 nil 时只回帖不落库（测试用）。
	Verifications VerificationRecorder

	// PermissionMode 传给 agent；无人值守通常用 acceptEdits。
	PermissionMode string
	// ExcludeDirs 是仓库级的验证扫描排除目录（如 CloudRouter 的 upstream）。
	ExcludeDirs []string
}

// ExecuteParams 描述一次流水线执行。
type ExecuteParams struct {
	TaskID   int64
	Repo     RepoConfig
	CloneURL string
	IssueID  string // Linear issue 的 UUID
	Actor    string
}

// Execute 跑完整条链路：分诊 → 实现 → 验证 → 开 PR → 回帖。
//
// 任一步失败都会走 fail()：回帖说明原因 + 保留 worktree 现场 + 推送通知，
// 且不自动重试（D4）。
func (p *Pipeline) Execute(ctx context.Context, params ExecuteParams) error {
	tk, err := p.Tasks.Get(ctx, params.TaskID)
	if err != nil {
		return err
	}
	actor := params.Actor
	if actor == "" {
		actor = "system"
	}

	// 凭据可能在任务排队期间被改过，因此每次执行都现取客户端
	lin, err := p.Clients.Linear(ctx)
	if err != nil {
		return fmt.Errorf("获取 Linear 客户端失败（请在设置里配置并验证凭据）: %w", err)
	}
	gh, err := p.Clients.GitHub(ctx)
	if err != nil {
		return fmt.Errorf("获取 GitHub 客户端失败（请在设置里配置并验证凭据）: %w", err)
	}

	// ---------- 分诊 ----------
	if _, err := p.Tasks.Transition(ctx, tk.ID, task.StateTriaging, actor, nil); err != nil {
		return err
	}

	issue, err := lin.Issue(ctx, params.IssueID)
	if err != nil {
		return p.fail(ctx, lin, tk.ID, params, nil, "拉取 issue 失败", err)
	}

	triageSession := p.newID()
	triageRes, err := p.Agent.Run(ctx, agent.RunParams{
		Prompt:         TriagePrompt(issue.Context()),
		SessionID:      triageSession,
		PermissionMode: "plan", // 分诊只读不写
	})
	if err != nil {
		return p.fail(ctx, lin, tk.ID, params, nil, "分诊执行失败", err)
	}

	verdict, err := ParseTriageVerdict(triageRes.Text)
	if err != nil {
		return p.fail(ctx, lin, tk.ID, params, nil, "分诊结果无法解析", err)
	}

	if !verdict.Actionable {
		// 单子不明确：回帖提问并停下，不猜（产品边界）
		body := fmt.Sprintf("**Lathe 暂不能自动处理这个 issue**\n\n%s\n\n补充后重新指派给我即可。", verdict.Question)
		if _, cerr := lin.Comment(ctx, params.IssueID, body); cerr != nil {
			slog.Warn("回帖失败", "task", tk.ID, "err", cerr)
		}
		_, err := p.Tasks.Transition(ctx, tk.ID, task.StateBlockedSpec, actor, &task.TransitionOpts{
			FailureReason: strPtr(verdict.Reason),
			Payload:       map[string]any{"question": verdict.Question},
		})
		return err
	}

	// ---------- 实现 ----------
	kind := verdict.Kind
	wt, err := p.Worktrees.Create(ctx, CreateParams{
		Repo: params.Repo, CloneURL: params.CloneURL,
		Kind: kind, IssueKey: issue.Identifier, Title: issue.Title,
	})
	if err != nil {
		return p.fail(ctx, lin, tk.ID, params, nil, "创建工作区失败", err)
	}

	implSession := p.newID()
	kindStr := string(kind)
	if _, err := p.Tasks.Transition(ctx, tk.ID, task.StateImplementing, actor, &task.TransitionOpts{
		// 会话 ID 在执行前就落库：进程中途崩溃也留下可 --resume 的凭据
		AgentSessionID: &implSession,
		WorktreePath:   &wt.Path,
		BranchName:     &wt.Branch,
		TaskKind:       &kindStr,
	}); err != nil {
		return err
	}

	implRes, err := p.Agent.Run(ctx, agent.RunParams{
		Prompt:         ImplementPrompt(issue.Context(), kind, wt.Branch),
		Dir:            wt.Path,
		SessionID:      implSession,
		PermissionMode: p.PermissionMode,
	})
	if err != nil {
		return p.fail(ctx, lin, tk.ID, params, wt, "实现执行失败", err)
	}
	if implRes.IsError {
		return p.fail(ctx, lin, tk.ID, params, wt, "实现未成功完成",
			fmt.Errorf("agent 返回 %s（%s）", implRes.Subtype, implRes.TerminalReason))
	}

	changed, err := p.Worktrees.HasChanges(ctx, wt)
	if err != nil {
		return p.fail(ctx, lin, tk.ID, params, wt, "检查改动失败", err)
	}
	if !changed {
		return p.fail(ctx, lin, tk.ID, params, wt, "agent 没有产生任何改动",
			errors.New("工作区无改动，视为未完成任务"))
	}

	commitMsg := fmt.Sprintf("%s(%s): %s\n\n%s",
		kind, strings.ToLower(issue.Identifier), issue.Title, truncate(implRes.Text, 1000))
	if err := p.Worktrees.Commit(ctx, wt, commitMsg); err != nil {
		return p.fail(ctx, lin, tk.ID, params, wt, "提交改动失败", err)
	}

	// ---------- 验证 ----------
	// §5.1：档位在 diff 产出后按实际改动面判定，而非接单时按单子文本猜。
	// 判定是确定性规则，理由落进任务事件供人复核。
	changedFiles, err := p.Worktrees.ChangedFiles(ctx, wt)
	if err != nil {
		return p.fail(ctx, lin, tk.ID, params, wt, "列出改动文件失败", err)
	}
	tier, tierReasons := ClassifyTier(changedFiles, OverrideTier(params.Repo.VerifyTierOverride))
	tierStr := string(tier)
	if _, err := p.Tasks.Transition(ctx, tk.ID, task.StateVerifying, actor, &task.TransitionOpts{
		VerifyTier: &tierStr,
		Payload: map[string]any{
			"tier_reasons":  tierReasons,
			"changed_files": len(changedFiles),
		},
	}); err != nil {
		return err
	}

	steps, err := DetectLightProfile(wt.Path, p.ExcludeDirs...)
	if err != nil {
		return p.fail(ctx, lin, tk.ID, params, wt, "无法确定验证步骤", err)
	}

	var report Report
	if tier == TierHeavy {
		report, err = p.runHeavy(ctx, tk.ID, params.Repo.ProviderRepo, wt, steps, changedFiles)
		if err != nil {
			return p.fail(ctx, lin, tk.ID, params, wt, "heavy 档验证执行失败", err)
		}
	} else {
		report = p.Verifier.RunLight(ctx, wt.Path, steps)
	}
	p.persistVerifications(ctx, tk.ID, report)

	// heavy 档的红阶段立不起来（bug 没复现/复现跑不起来）不是任务失败，
	// 是单子没说清 —— §5.3 规定转 blocked_spec 回帖请人补充复现步骤。
	if red := redStepFailure(report); red != nil {
		body := fmt.Sprintf("**Lathe 无法证明这个修复有效，已暂停**\n\n%s\n\n复现测试在改动前的代码上没有失败 —— 可能是 bug 描述与实际不符，或复现条件缺失。请补充复现步骤后重新指派给我。\n\n```\n%s```",
			red.Err, report.Summary())
		if _, cerr := lin.Comment(ctx, params.IssueID, body); cerr != nil {
			slog.Warn("回帖失败", "task", tk.ID, "err", cerr)
		}
		_, err := p.Tasks.Transition(ctx, tk.ID, task.StateBlockedSpec, actor, &task.TransitionOpts{
			FailureReason: strPtr(red.Err.Error()),
			Payload:       map[string]any{"reason": "repro_not_red"},
		})
		return err
	}

	if !report.Passed() {
		reason := "验证未通过"
		cause := report.Summary()
		if f := report.FirstFailure(); f != nil {
			reason = fmt.Sprintf("验证未通过（%s 档）：%s", tier, f.Step.Name)
			// 把首个失败的具体错误放最前 —— 回帖时人先看到原因再看摘要
			if f.Err != nil {
				cause = f.Err.Error() + "\n\n" + report.Summary()
			}
		}
		return p.fail(ctx, lin, tk.ID, params, wt, reason, errors.New(cause))
	}

	// ---------- 开 PR ----------
	if err := p.Worktrees.Push(ctx, wt, params.Repo); err != nil {
		return p.fail(ctx, lin, tk.ID, params, wt, "推送分支失败", err)
	}

	pr, err := gh.CreatePR(ctx, github.PRParams{
		ProviderRepo: params.Repo.ProviderRepo,
		Head:         wt.Branch,
		Base:         wt.BaseBranch,
		Title:        fmt.Sprintf("%s(%s): %s", kind, strings.ToLower(issue.Identifier), issue.Title),
		Body:         github.BuildPRBody(issue.Identifier, issue.URL, report.Summary(), implRes.Text),
	})
	if err != nil {
		return p.fail(ctx, lin, tk.ID, params, wt, "创建 PR 失败", err)
	}

	if _, err := p.Tasks.Transition(ctx, tk.ID, task.StatePROpen, actor, &task.TransitionOpts{
		PRURL:   &pr.URL,
		Payload: map[string]any{"pr_number": pr.Number, "reused": pr.Existing},
	}); err != nil {
		return err
	}

	body := fmt.Sprintf("**Lathe 已完成并开出 PR**\n\n%s\n\n```\n%s```\n\n请人工复核后合并。",
		pr.URL, report.Summary())
	if _, err := lin.Comment(ctx, params.IssueID, body); err != nil {
		slog.Warn("回帖失败", "task", tk.ID, "err", err)
	}

	slog.Info("任务完成", "task", tk.ID, "issue", issue.Identifier, "pr", pr.URL)
	return nil
}

// runHeavy 跑 heavy 档验证：建基线工作区 → 识别复现测试 → 红-绿-回归。
//
// 基线工作区用完即拆（defer Remove force）：它是验证的临时道具，
// 不是失败三件套要保留的现场 —— 现场指任务工作区本身。
func (p *Pipeline) runHeavy(ctx context.Context, taskID int64, providerRepo string, wt *Worktree, lightSteps []Step, changedFiles []string) (Report, error) {
	base, err := p.Worktrees.CreateDetached(ctx, providerRepo, wt.BaseBranch, fmt.Sprintf("task-%d-base", taskID))
	if err != nil {
		return Report{}, err
	}
	defer func() {
		if rerr := p.Worktrees.Remove(ctx, base, true); rerr != nil {
			slog.Warn("回收基线工作区失败", "task", taskID, "err", rerr)
		}
	}()

	repro, err := IdentifyReproTests(wt.Path, changedFiles)
	if err != nil {
		return Report{}, err
	}
	regression := DetectRegression(wt.Path, changedFiles, p.ExcludeDirs...)

	return p.Verifier.RunHeavy(ctx, HeavyParams{
		TaskPath:   wt.Path,
		BasePath:   base.Path,
		Light:      lightSteps,
		Repro:      repro,
		Regression: regression,
	}), nil
}

// redStepFailure 在 heavy 报告里找「红阶段没立起来」的那一步。
// 找到说明 §5.3 的 blocked_spec 路径成立；返回 nil 表示不涉及。
//
// 例外：ErrNoReproTests 是 agent 没交复现测试的契约违例，属于任务
// 失败（走 D4 三件套），不该回帖问提单人要复现步骤。
func redStepFailure(rep Report) *StepResult {
	if rep.Tier != TierHeavy {
		return nil
	}
	for i := range rep.Results {
		s := &rep.Results[i]
		if s.Step.Name == StepReproFail && s.Status != StatusPassed {
			if errors.Is(s.Err, ErrNoReproTests) {
				return nil
			}
			if s.Err == nil {
				s.Err = errors.New("复现测试在改动前的代码上没有失败")
			}
			return s
		}
	}
	return nil
}

// persistVerifications 把报告的每一步落库。落库失败只告警不中断 ——
// 证据缺失不该把一个已验证通过的改动挡在 PR 之外（结论仍在 PR 与回帖里）。
func (p *Pipeline) persistVerifications(ctx context.Context, taskID int64, rep Report) {
	if p.Verifications == nil {
		return
	}
	for _, s := range rep.Results {
		if err := p.Verifications.InsertVerification(ctx, taskID,
			string(rep.Tier), string(s.Step.Name), string(s.Status),
			s.Duration.Milliseconds()); err != nil {
			slog.Warn("验证步骤落库失败", "task", taskID, "step", s.Step.Name, "err", err)
		}
	}
}

// fail 执行 D4 失败三件套：回帖 + 保留现场 + 推送通知；不自动重试。
//
// 刻意不回收 worktree：现场留给人直接 cd 进去接着干。
func (p *Pipeline) fail(ctx context.Context, lin LinearAPI, taskID int64, params ExecuteParams, wt *Worktree, stage string, cause error) error {
	reason := fmt.Sprintf("%s: %v", stage, cause)
	slog.Error("任务失败", "task", taskID, "stage", stage, "err", cause)

	// 1) 回帖到 Linear
	body := fmt.Sprintf("**Lathe 处理失败**\n\n阶段：%s\n\n```\n%s\n```\n", stage, truncate(cause.Error(), 3000))
	if wt != nil {
		body += fmt.Sprintf("\n工作区已保留在 `%s`（分支 `%s`），可直接进去接手。\n", wt.Path, wt.Branch)
	}
	if lin != nil {
		if _, err := lin.Comment(ctx, params.IssueID, body); err != nil {
			slog.Warn("失败回帖也失败了", "task", taskID, "err", err)
		}
	}

	// 2) 保留现场：不调用 Worktrees.Remove

	// 3) 推送通知
	if p.Notifier != nil {
		msg := fmt.Sprintf("Lathe 任务 %d 失败（%s）", taskID, stage)
		if err := p.Notifier.Notify(ctx, msg); err != nil {
			slog.Warn("推送通知失败", "task", taskID, "err", err)
		}
	}

	opts := &task.TransitionOpts{
		FailureReason: &reason,
		Payload:       map[string]any{"stage": stage},
	}
	if _, err := p.Tasks.Transition(ctx, taskID, task.StateFailed, "system", opts); err != nil {
		return fmt.Errorf("任务失败(%s)，且状态转移也失败: %w（原因: %v）", stage, err, cause)
	}
	return fmt.Errorf("任务失败于%s: %w", stage, cause)
}

func (p *Pipeline) newID() string {
	if p.NewID != nil {
		return p.NewID()
	}
	return newUUID()
}

func strPtr(s string) *string { return &s }
