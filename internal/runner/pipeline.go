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

// AgentDriver 驱动 agent 执行。
type AgentDriver interface {
	Run(ctx context.Context, p agent.RunParams) (*agent.Result, error)
}

// Notifier 推送通知给用户（D4 失败三件套之一）。
type Notifier interface {
	Notify(ctx context.Context, message string) error
}

// NewSessionID 生成会话 ID。抽成字段便于测试注入确定值。
type NewSessionID func() string

// Pipeline 把一个任务从 queued 跑到 pr_open。
type Pipeline struct {
	Tasks     *task.Machine
	Worktrees *WorktreeManager
	Verifier  *Verifier
	Agent     AgentDriver
	Linear    LinearAPI
	GitHub    GitHubAPI
	Notifier  Notifier
	NewID     NewSessionID

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

	// ---------- 分诊 ----------
	if _, err := p.Tasks.Transition(ctx, tk.ID, task.StateTriaging, actor, nil); err != nil {
		return err
	}

	issue, err := p.Linear.Issue(ctx, params.IssueID)
	if err != nil {
		return p.fail(ctx, tk.ID, params, nil, "拉取 issue 失败", err)
	}

	triageSession := p.newID()
	triageRes, err := p.Agent.Run(ctx, agent.RunParams{
		Prompt:         TriagePrompt(issue.Context()),
		SessionID:      triageSession,
		PermissionMode: "plan", // 分诊只读不写
	})
	if err != nil {
		return p.fail(ctx, tk.ID, params, nil, "分诊执行失败", err)
	}

	verdict, err := ParseTriageVerdict(triageRes.Text)
	if err != nil {
		return p.fail(ctx, tk.ID, params, nil, "分诊结果无法解析", err)
	}

	if !verdict.Actionable {
		// 单子不明确：回帖提问并停下，不猜（产品边界）
		body := fmt.Sprintf("**Lathe 暂不能自动处理这个 issue**\n\n%s\n\n补充后重新指派给我即可。", verdict.Question)
		if _, cerr := p.Linear.Comment(ctx, params.IssueID, body); cerr != nil {
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
		return p.fail(ctx, tk.ID, params, nil, "创建工作区失败", err)
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
		return p.fail(ctx, tk.ID, params, wt, "实现执行失败", err)
	}
	if implRes.IsError {
		return p.fail(ctx, tk.ID, params, wt, "实现未成功完成",
			fmt.Errorf("agent 返回 %s（%s）", implRes.Subtype, implRes.TerminalReason))
	}

	changed, err := p.Worktrees.HasChanges(ctx, wt)
	if err != nil {
		return p.fail(ctx, tk.ID, params, wt, "检查改动失败", err)
	}
	if !changed {
		return p.fail(ctx, tk.ID, params, wt, "agent 没有产生任何改动",
			errors.New("工作区无改动，视为未完成任务"))
	}

	commitMsg := fmt.Sprintf("%s(%s): %s\n\n%s",
		kind, strings.ToLower(issue.Identifier), issue.Title, truncate(implRes.Text, 1000))
	if err := p.Worktrees.Commit(ctx, wt, commitMsg); err != nil {
		return p.fail(ctx, tk.ID, params, wt, "提交改动失败", err)
	}

	// ---------- 验证 ----------
	tier := string(TierLight)
	if _, err := p.Tasks.Transition(ctx, tk.ID, task.StateVerifying, actor, &task.TransitionOpts{
		VerifyTier: &tier,
	}); err != nil {
		return err
	}

	steps, err := DetectLightProfile(wt.Path, p.ExcludeDirs...)
	if err != nil {
		return p.fail(ctx, tk.ID, params, wt, "无法确定验证步骤", err)
	}
	report := p.Verifier.RunLight(ctx, wt.Path, steps)
	if !report.Passed() {
		reason := "验证未通过"
		if f := report.FirstFailure(); f != nil {
			reason = fmt.Sprintf("验证未通过：%s", f.Step.Name)
		}
		return p.fail(ctx, tk.ID, params, wt, reason, errors.New(report.Summary()))
	}

	// ---------- 开 PR ----------
	if err := p.Worktrees.Push(ctx, wt, params.Repo); err != nil {
		return p.fail(ctx, tk.ID, params, wt, "推送分支失败", err)
	}

	pr, err := p.GitHub.CreatePR(ctx, github.PRParams{
		ProviderRepo: params.Repo.ProviderRepo,
		Head:         wt.Branch,
		Base:         wt.BaseBranch,
		Title:        fmt.Sprintf("%s(%s): %s", kind, strings.ToLower(issue.Identifier), issue.Title),
		Body:         github.BuildPRBody(issue.Identifier, issue.URL, report.Summary(), implRes.Text),
	})
	if err != nil {
		return p.fail(ctx, tk.ID, params, wt, "创建 PR 失败", err)
	}

	if _, err := p.Tasks.Transition(ctx, tk.ID, task.StatePROpen, actor, &task.TransitionOpts{
		PRURL:   &pr.URL,
		Payload: map[string]any{"pr_number": pr.Number, "reused": pr.Existing},
	}); err != nil {
		return err
	}

	body := fmt.Sprintf("**Lathe 已完成并开出 PR**\n\n%s\n\n```\n%s```\n\n请人工复核后合并。",
		pr.URL, report.Summary())
	if _, err := p.Linear.Comment(ctx, params.IssueID, body); err != nil {
		slog.Warn("回帖失败", "task", tk.ID, "err", err)
	}

	slog.Info("任务完成", "task", tk.ID, "issue", issue.Identifier, "pr", pr.URL)
	return nil
}

// fail 执行 D4 失败三件套：回帖 + 保留现场 + 推送通知；不自动重试。
//
// 刻意不回收 worktree：现场留给人直接 cd 进去接着干。
func (p *Pipeline) fail(ctx context.Context, taskID int64, params ExecuteParams, wt *Worktree, stage string, cause error) error {
	reason := fmt.Sprintf("%s: %v", stage, cause)
	slog.Error("任务失败", "task", taskID, "stage", stage, "err", cause)

	// 1) 回帖到 Linear
	body := fmt.Sprintf("**Lathe 处理失败**\n\n阶段：%s\n\n```\n%s\n```\n", stage, truncate(cause.Error(), 3000))
	if wt != nil {
		body += fmt.Sprintf("\n工作区已保留在 `%s`（分支 `%s`），可直接进去接手。\n", wt.Path, wt.Branch)
	}
	if _, err := p.Linear.Comment(ctx, params.IssueID, body); err != nil {
		slog.Warn("失败回帖也失败了", "task", taskID, "err", err)
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
