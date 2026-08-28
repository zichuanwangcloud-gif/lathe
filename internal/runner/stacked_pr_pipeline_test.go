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
	"github.com/Clouditera/lathe/internal/integration/linear"
	"github.com/Clouditera/lathe/internal/task"
)

// 本文件补 docs/07-prd-orchestration.md F3.2 的两个缺口：
//
//   - F3.2-AC1：栈式 PR 最核心的效果——后继 PR 的 base == 前驱的
//     branch_name——从未被任何测试端到端走完整 Pipeline.Execute 断言过。
//     stacked_pr_test.go 测的是 WorktreeManager 这一层（分叉关系、diff
//     排除前驱改动）；orchestration_m1_test.go 的全链路测试测的是 M1
//     独立执行语义（PR base 全部是 "dev"，即"没有堆叠"）。两者都不是
//     "堆叠场景下 PR base 正确"本身。
//   - F3.2-AC5：ErrNoCommits 走到 pipeline 层之后的行为
//     （p.fail 走 StageCreatePR、不产生 PR URL、不开空 PR）此前只有
//     github 包测过错误本身的构造，从未验证过 pipeline 收到它之后
//     做了什么。
//
// 复用 pipeline_test.go 的 fakeLinear/fakeGitHub/fakeAgent/
// fakeVerifications 与 newPipeline/testPoolForPipeline/goSourceRepo，
// 不新造测试假件。

// stackedPipelineFixture 造两个真实 Postgres 任务行：后继通过
// CreateParams.DependsOn 声明依赖前驱，与调度器建图的形态一致
// （docs/06-orchestration.md）。测的是 Pipeline 这一侧，因此这里直接
// 手工构造 RepoConfig.BaseRefOverride 传给 Execute，不经过调度器的
// fillBaseRef —— 那一侧 cmd/lathe/queue_test.go 已经测过。
func stackedPipelineFixture(t *testing.T) (*task.Machine, int64, int64, RepoConfig, string) {
	t.Helper()
	pool := testPoolForPipeline(t)
	m := task.NewMachine(pool)
	ctx := context.Background()

	var userID, repoID int64
	email := "stacked-" + t.Name() + "@example.com"
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) ON CONFLICT (email) DO UPDATE SET updated_at=now() RETURNING id`,
		email).Scan(&userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2)
		 ON CONFLICT (user_id, provider_repo) DO UPDATE SET updated_at=now() RETURNING id`,
		userID, "acme/stacked").Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})

	pred, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "CR-300",
	})
	if err != nil {
		t.Fatalf("建前驱任务失败: %v", err)
	}
	succ, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "CR-301",
		DependsOn: &pred.ID,
	})
	if err != nil {
		t.Fatalf("建后继任务失败: %v", err)
	}
	return m, pred.ID, succ.ID, DefaultRepoConfig("acme/stacked"), goSourceRepo(t)
}

func stackedIssue(id, key, title string) *linear.Issue {
	return &linear.Issue{
		ID: id, Identifier: key, Title: title,
		Description: "栈式 PR 测试任务", URL: "https://linear.app/x/" + key,
	}
}

// stackedAgent 造一个"分诊通过 + 实现写一个纯文案文件"的 fakeAgent —— 只
// 关心档位落在 light（避免 heavy 档红-绿证明的复杂度和本测试要断言的
// 事情无关），不关心具体改了什么。
func stackedAgent(fileName, content string) *fakeAgent {
	return &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"ok","question":""}`},
			{Success: true, Text: "实现完成"},
		},
		mutate: []func(string) error{
			nil, // 分诊不改文件
			func(dir string) error {
				return os.WriteFile(filepath.Join(dir, fileName), []byte(content), 0o644)
			},
		},
	}
}

// TestPipelineStackedPRBaseMatchesPredecessorBranch 补 F3.2-AC1：真正走
// 两次完整 Pipeline.Execute（不是只测 BaseBranch() 纯函数短路，也不是只测
// git 祖先关系），用同一个 fakeGitHub 记录两次 CreatePR 调用，直接断言
// 第二次调用（后继）的 PRParams.Base 等于第一次调用（前驱）落库的
// branch_name —— 这是栈式 PR 最核心的效果，此前从未被端到端断言过。
func TestPipelineStackedPRBaseMatchesPredecessorBranch(t *testing.T) {
	m, predID, succID, repo, src := stackedPipelineFixture(t)
	ctx := context.Background()

	// 前驱与后继共用同一个 fakeGitHub：gh.params 按调用顺序追加，
	// gh.params[0] 必是前驱的 CreatePR 调用，gh.params[1] 必是后继的。
	gh := &fakeGitHub{pr: &github.PullRequest{Number: 100, URL: "https://github.com/acme/stacked/pull/100"}}

	// ---- 前驱：正常从 dev 分叉（RepoConfig.BaseRefOverride 为空） ----
	predLin := &fakeLinear{issue: stackedIssue("uuid-300", "CR-300", "predecessor issue")}
	predAgent := stackedAgent("pred_notes.txt", "前驱产出\n")
	predP := newPipeline(t, m, predLin, gh, predAgent, &fakeNotifier{})
	predP.Verifications = &fakeVerifications{}

	if err := predP.Execute(ctx, ExecuteParams{
		TaskID: predID, Repo: repo, CloneURL: src, IssueID: "uuid-300", Actor: "node:test",
	}); err != nil {
		t.Fatalf("前驱 Execute 失败: %v", err)
	}

	predFinal, err := m.Get(ctx, predID)
	if err != nil {
		t.Fatalf("读取前驱任务失败: %v", err)
	}
	if predFinal.State != task.StatePROpen {
		t.Fatalf("前驱终态 = %s，期望 pr_open", predFinal.State)
	}
	if predFinal.BranchName == nil {
		t.Fatal("前驱分支名应已落库")
	}
	predBranch := *predFinal.BranchName

	if len(gh.params) != 1 {
		t.Fatalf("此时应已创建 1 个 PR（前驱），实际 %d", len(gh.params))
	}
	if gh.params[0].Base != "dev" {
		t.Fatalf("前驱 PR base = %q，期望 \"dev\"（未做栈式穿透，独立根任务）", gh.params[0].Base)
	}
	if gh.params[0].Head != predBranch {
		t.Fatalf("前驱 PR head = %q，期望等于前驱分支名 %q", gh.params[0].Head, predBranch)
	}

	// ---- 后继：调度器已把前驱分支名填进 tasks.base_ref → BaseRefOverride
	// （对应 cmd/lathe/queue.go 的 fillBaseRef；这里直接手工构造带
	// BaseRefOverride 的 RepoConfig，因为测的是 Pipeline 这一侧的行为，
	// 不是调度器那一侧——那一侧 cmd/lathe/queue_test.go 已经测过）。
	succRepo := repo
	succRepo.BaseRefOverride = predBranch

	succLin := &fakeLinear{issue: stackedIssue("uuid-301", "CR-301", "successor issue")}
	succAgent := stackedAgent("succ_notes.txt", "后继产出\n")
	succP := newPipeline(t, m, succLin, gh, succAgent, &fakeNotifier{})
	succP.Verifications = &fakeVerifications{}

	if err := succP.Execute(ctx, ExecuteParams{
		TaskID: succID, Repo: succRepo, CloneURL: src, IssueID: "uuid-301", Actor: "node:test",
	}); err != nil {
		t.Fatalf("后继 Execute 失败: %v", err)
	}

	succFinal, err := m.Get(ctx, succID)
	if err != nil {
		t.Fatalf("读取后继任务失败: %v", err)
	}
	if succFinal.State != task.StatePROpen {
		t.Fatalf("后继终态 = %s，期望 pr_open", succFinal.State)
	}
	if succFinal.BranchName == nil {
		t.Fatal("后继分支名应已落库")
	}
	succBranch := *succFinal.BranchName
	if succBranch == predBranch {
		t.Fatalf("前驱与后继分支名不应相同: %q", succBranch)
	}

	if len(gh.params) != 2 {
		t.Fatalf("此时应已创建 2 个 PR（前驱+后继），实际 %d", len(gh.params))
	}

	// ★核心断言（F3.2-AC1）：后继 PR 的 base == 前驱的 branch_name，
	// 不是 "dev"。这是栈式 PR 最核心的效果，此前从未被任何测试
	// 端到端断言过——现有测试各自证明了 base_ref 落库、git 祖先关系、
	// BaseBranch() 短路是对的，但没有一个测试真正走完整
	// Pipeline.stagePushAndPR 断言 fakeGitHub 收到的 PRParams.Base
	// 就是前驱的分支名。
	if gh.params[1].Base != predBranch {
		t.Errorf("后继 PR base = %q，期望等于前驱的分支名 %q（不是 %q）",
			gh.params[1].Base, predBranch, repo.DefaultBranch)
	}

	// 顺带断言：后继 PR 的 head 是后继自己的分支名（不会跟前驱的分支
	// 名搞反——base 与 head 一旦弄反，栈式 PR 就会变成把自己的分支
	// 拿去当基线）。
	if gh.params[1].Head != succBranch {
		t.Errorf("后继 PR head = %q，期望等于后继自己的分支名 %q", gh.params[1].Head, succBranch)
	}
}

// TestPipelineErrNoCommitsFailsWithoutPR 补 F3.2-AC5：后继与前驱无差异
// 时，github 客户端返回 ErrNoCommits——github 包自己已经测过这个错误的
// 构造（github_test.go），但 pipeline 收到它之后的行为从未被验证过：
// 任务应以可读原因失败在 StageCreatePR、不产生 PR URL（没有真的开出
// 空 PR）、并照 D4 三件套回帖说明。
func TestPipelineErrNoCommitsFailsWithoutPR(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{err: github.ErrNoCommits}
	ag := stackedAgent("notes.txt", "改动\n")
	no := &fakeNotifier{}
	p := newPipeline(t, m, lin, gh, ag, no)
	p.Verifications = &fakeVerifications{}

	err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777", Actor: "node:test",
	})
	if err == nil {
		t.Fatal("ErrNoCommits 应导致任务失败，不应被当成成功放过去")
	}
	if !errors.Is(err, github.ErrNoCommits) {
		t.Errorf("Execute 返回的错误应包裹 ErrNoCommits，得到: %v", err)
	}

	final, gerr := m.Get(context.Background(), taskID)
	if gerr != nil {
		t.Fatalf("读取任务失败: %v", gerr)
	}
	if final.State != task.StateFailed {
		t.Errorf("状态 = %s，期望 failed", final.State)
	}
	if final.FailureStage == nil || Stage(*final.FailureStage) != StageCreatePR {
		t.Errorf("失败阶段 = %v，期望 %q（StageCreatePR，说明失败发生在创建 PR 这一步）",
			final.FailureStage, StageCreatePR)
	}
	if final.FailureReason == nil ||
		!(strings.Contains(*final.FailureReason, "无差异") || strings.Contains(*final.FailureReason, "无可提交")) {
		t.Errorf("失败原因应能看出是无差异/无提交（ErrNoCommits），实际: %v", final.FailureReason)
	}
	if final.PRURL != nil {
		t.Errorf("不应产生 PR URL——没有真的开出 PR，实际: %v", *final.PRURL)
	}
	if len(gh.params) != 1 {
		t.Fatalf("应尝试创建 1 次 PR，实际 %d", len(gh.params))
	}

	// D4 三件套之一：回帖说明失败。
	if len(lin.comments) == 0 || !strings.Contains(lin.comments[len(lin.comments)-1], "处理失败") {
		t.Errorf("应回帖说明处理失败，实际: %v", lin.comments)
	}
}
