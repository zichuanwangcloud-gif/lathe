package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Clouditera/lathe/internal/integration/agent"
	"github.com/Clouditera/lathe/internal/integration/github"
	"github.com/Clouditera/lathe/internal/task"
)

// pipeline_resume_test.go 覆盖智能重试的断点续跑路径：
// 失败任务的现场（worktree/分支/提交/会话）经决策后被复用，
// 流水线从失败阶段续跑而不是从头重烧一遍。

// prepareFailedTask 造一个「失败后又回到 queued」的任务及其现场：
// worktree 已建好，任务行走过 implementing→failed→queued，带着
// failure_stage/会话/定档 —— 正是 retryTask + Requeue 之后、
// pipeline.Execute 之前的样子。committed 控制改动是否已提交。
func prepareFailedTask(t *testing.T, p *Pipeline, m *task.Machine, taskID int64, src string, repo RepoConfig, stage Stage, committed bool) *Worktree {
	t.Helper()
	ctx := context.Background()

	wt, err := p.Worktrees.Create(ctx, CreateParams{
		Repo: repo, CloneURL: src, Kind: KindFix, IssueKey: "CR-777", Title: "t",
	})
	if err != nil {
		t.Fatalf("造现场：Create 失败: %v", err)
	}
	// 文档级改动：归 light 档，验证跑得快
	writeFile(t, filepath.Join(wt.Path, "README.md"), "# src\n\n补充：导入失败时的兜底说明\n")
	if committed {
		if err := p.Worktrees.Commit(ctx, wt, "docs: 补充导入失败说明"); err != nil {
			t.Fatalf("造现场：Commit 失败: %v", err)
		}
	}

	sess := "sess-original"
	kind, tier := "fix", "light"
	if _, err := m.Transition(ctx, taskID, task.StateImplementing, "test", &task.TransitionOpts{
		AgentSessionID: &sess, WorktreePath: &wt.Path, BranchName: &wt.Branch,
		TaskKind: &kind, VerifyTier: &tier,
	}); err != nil {
		t.Fatalf("造现场：转 implementing 失败: %v", err)
	}
	reason := "测试构造的失败（阶段 " + string(stage) + "）"
	if _, err := m.Transition(ctx, taskID, task.StateFailed, "test", &task.TransitionOpts{
		FailureReason: &reason, FailureStage: strPtr(string(stage)),
	}); err != nil {
		t.Fatalf("造现场：转 failed 失败: %v", err)
	}
	if _, err := m.Transition(ctx, taskID, task.StateQueued, "test", &task.TransitionOpts{
		Payload: map[string]any{"reason": "manual_retry"},
	}); err != nil {
		t.Fatalf("造现场：转 queued 失败: %v", err)
	}
	return wt
}

func retryEvents(t *testing.T, m *task.Machine, taskID int64) []task.Event {
	t.Helper()
	events, err := m.Events(context.Background(), taskID)
	if err != nil {
		t.Fatalf("读事件流失败: %v", err)
	}
	return events
}

// 推送失败的重试：验证早已通过，只补 push + 开 PR，绝不动用 agent。
func TestPipelineResumeFromPush(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{pr: &github.PullRequest{URL: "https://x/pr/9", Number: 9}}
	ag := &fakeAgent{}
	p := newPipeline(t, m, lin, gh, ag, &fakeNotifier{})

	wt := prepareFailedTask(t, p, m, taskID, src, repo, StagePush, true)

	err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777",
		Retry: &RetryPlan{Entry: EntryPush, Reasons: []string{"验证已通过，仅推送未完成"}},
	})
	if err != nil {
		t.Fatalf("push 续跑失败: %v", err)
	}

	if len(ag.calls) != 0 {
		t.Errorf("push 续跑不应动用 agent，实际调用 %d 次", len(ag.calls))
	}
	if len(gh.params) != 1 {
		t.Fatalf("应创建 1 个 PR，实际 %d", len(gh.params))
	}
	if gh.params[0].Head != wt.Branch {
		t.Errorf("PR 分支 = %s，期望 %s", gh.params[0].Head, wt.Branch)
	}

	final, _ := m.Get(context.Background(), taskID)
	if final.State != task.StatePROpen {
		t.Errorf("状态 = %s，期望 pr_open", final.State)
	}
	// 分支真的推上了远端（src 充当远端）
	gitOut(t, src, "rev-parse", "--verify", "refs/heads/"+wt.Branch+"^{commit}")

	// 决策理由落进了事件流（queued→verifying 的中转转移携带 retry payload）
	found := false
	for _, e := range retryEvents(t, m, taskID) {
		if rp, ok := e.Payload["retry"].(map[string]any); ok {
			found = true
			if rp["entry"] != string(EntryPush) {
				t.Errorf("事件里的续跑入口 = %v，期望 push", rp["entry"])
			}
		}
	}
	if !found {
		t.Error("事件流里应记录重试决策")
	}
}

// 验证没跑成（槽位/环境）的重试：分支提交完好，直接重验，不动用 agent。
func TestPipelineResumeFromVerify(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{pr: &github.PullRequest{URL: "https://x/pr/10", Number: 10}}
	ag := &fakeAgent{}
	p := newPipeline(t, m, lin, gh, ag, &fakeNotifier{})

	prepareFailedTask(t, p, m, taskID, src, repo, StageVerifyGate, true)

	err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777",
		Retry: &RetryPlan{Entry: EntryVerify, Reasons: []string{"直接重新验证"}},
	})
	if err != nil {
		t.Fatalf("verify 续跑失败: %v", err)
	}

	if len(ag.calls) != 0 {
		t.Errorf("verify 续跑不应动用 agent，实际调用 %d 次", len(ag.calls))
	}
	final, _ := m.Get(context.Background(), taskID)
	if final.State != task.StatePROpen {
		t.Errorf("状态 = %s，期望 pr_open", final.State)
	}
	// 沿用首次定档，不重新分类
	if final.VerifyTier == nil || *final.VerifyTier != "light" {
		t.Errorf("定档应沿用 light: %v", final.VerifyTier)
	}
}

// 实现中断的重试：resume 原会话接着干，工作区里的半成品不丢。
func TestPipelineResumeImplementWithSession(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{pr: &github.PullRequest{URL: "https://x/pr/11", Number: 11}}
	ag := &fakeAgent{
		results: []*agent.Result{{Success: true, Text: "接着改完了"}},
		mutate: []func(string) error{
			func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "README.md"),
					[]byte("# src\n\n半成品 + 续跑补完\n"), 0o644)
			},
		},
	}
	p := newPipeline(t, m, lin, gh, ag, &fakeNotifier{})

	// 实现中断的现场：worktree 在、有半成品但未提交
	prepareFailedTask(t, p, m, taskID, src, repo, StageImplementRun, false)

	err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777",
		Retry: &RetryPlan{Entry: EntryImplement, ResumeSession: true, Reasons: []string{"续跑原会话"}},
	})
	if err != nil {
		t.Fatalf("implement 续跑失败: %v", err)
	}

	if len(ag.calls) != 1 {
		t.Fatalf("应只调用 1 次 agent（续跑实现），实际 %d", len(ag.calls))
	}
	call := ag.calls[0]
	if !call.Resume || call.SessionID != "sess-original" {
		t.Errorf("应以 --resume sess-original 续跑，得到 Resume=%v Session=%s", call.Resume, call.SessionID)
	}
	if !strings.Contains(call.Prompt, "被中断") {
		t.Error("续跑 prompt 应说明中断背景")
	}

	final, _ := m.Get(context.Background(), taskID)
	if final.State != task.StatePROpen {
		t.Errorf("状态 = %s，期望 pr_open", final.State)
	}
	// 会话 ID 保持不变（resume 成功，没有降级）
	if final.AgentSessionID == nil || *final.AgentSessionID != "sess-original" {
		t.Errorf("会话应保持 sess-original: %v", final.AgentSessionID)
	}
}

// resume 的会话数据丢失时（claude 报 no conversation），降级为同工作区
// 新会话收尾，而不是把可用现场一起判死。
func TestPipelineResumeImplementDegradesOnResumeFailure(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{pr: &github.PullRequest{URL: "https://x/pr/12", Number: 12}}
	ag := &fakeAgent{
		errs: []error{errors.New("No conversation found with session ID"), nil},
		results: []*agent.Result{
			nil,
			{Success: true, Text: "新会话收尾完成"},
		},
		mutate: []func(string) error{
			nil,
			func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "README.md"),
					[]byte("# src\n\n半成品 + 新会话补完\n"), 0o644)
			},
		},
	}
	p := newPipeline(t, m, lin, gh, ag, &fakeNotifier{})

	prepareFailedTask(t, p, m, taskID, src, repo, StageImplementRun, false)

	err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777",
		Retry: &RetryPlan{Entry: EntryImplement, ResumeSession: true, Reasons: []string{"续跑原会话"}},
	})
	if err != nil {
		t.Fatalf("降级续跑失败: %v", err)
	}

	if len(ag.calls) != 2 {
		t.Fatalf("应调用 2 次 agent（resume 失败 + 新会话降级），实际 %d", len(ag.calls))
	}
	if !ag.calls[0].Resume {
		t.Error("第一次调用应是 resume 尝试")
	}
	second := ag.calls[1]
	if second.Resume {
		t.Error("降级后不应再 resume")
	}
	if second.SessionID == "" || second.SessionID == "sess-original" {
		t.Errorf("降级应换新会话 ID，得到 %q", second.SessionID)
	}
	if !strings.Contains(second.Prompt, "续跑场景") {
		t.Error("降级 prompt 应带全量需求与现状说明")
	}

	final, _ := m.Get(context.Background(), taskID)
	if final.State != task.StatePROpen {
		t.Errorf("状态 = %s，期望 pr_open", final.State)
	}
	// 新会话 ID 已落库：再崩溃也能 resume 新会话
	if final.AgentSessionID == nil || *final.AgentSessionID != second.SessionID {
		t.Errorf("降级后的会话 ID 应落库: %v vs %q", final.AgentSessionID, second.SessionID)
	}
}

// 验证未通过、人进现场手工改了代码后的重试：先提交人的改动再重验，
// 不动用 agent（EntryCommit）。
func TestPipelineResumeFromCommit(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{pr: &github.PullRequest{URL: "https://x/pr/13", Number: 13}}
	ag := &fakeAgent{}
	p := newPipeline(t, m, lin, gh, ag, &fakeNotifier{})

	// 现场：已有提交 + 人手工留下的未提交改动
	prepareFailedTask(t, p, m, taskID, src, repo, StageVerifyFailed, true)
	tk, _ := m.Get(context.Background(), taskID)
	writeFile(t, filepath.Join(*tk.WorktreePath, "README.md"), "# src\n\n补充：人工介入修好了\n")

	err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777",
		Retry: &RetryPlan{Entry: EntryCommit, Reasons: []string{"先提交再重验"}},
	})
	if err != nil {
		t.Fatalf("commit 续跑失败: %v", err)
	}

	if len(ag.calls) != 0 {
		t.Errorf("commit 续跑不应动用 agent，实际 %d 次", len(ag.calls))
	}
	final, _ := m.Get(context.Background(), taskID)
	if final.State != task.StatePROpen {
		t.Errorf("状态 = %s，期望 pr_open", final.State)
	}
}

// 重建决策（Fresh）：走完整全流程，分诊 agent 照常调用，
// 且决策理由落进事件流 —— 重建为什么不能静默。
func TestPipelineFreshRetryRunsFullPipeline(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{pr: &github.PullRequest{URL: "https://x/pr/14", Number: 14}}
	ag := &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix"}`},
			{Success: true, Text: "改完了"},
		},
		mutate: []func(string) error{
			nil,
			func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "README.md"),
					[]byte("# src\n\n全新实现\n"), 0o644)
			},
		},
	}
	p := newPipeline(t, m, lin, gh, ag, &fakeNotifier{})

	// 旧现场在派发侧已被 Discard（queue 的职责），这里直接全新执行
	err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777",
		Retry: &RetryPlan{Fresh: true, Entry: EntryTriage, Reasons: []string{"现场已不可用，从头重建"}},
	})
	if err != nil {
		t.Fatalf("重建执行失败: %v", err)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("重建应跑分诊+实现两次 agent，实际 %d", len(ag.calls))
	}

	found := false
	for _, e := range retryEvents(t, m, taskID) {
		if rp, ok := e.Payload["retry"].(map[string]any); ok {
			found = true
			if rp["fresh"] != true {
				t.Error("事件里应标记这是重建")
			}
		}
	}
	if !found {
		t.Error("重建的决策理由应落进事件流")
	}
}
