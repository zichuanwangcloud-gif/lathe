package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Clouditera/lathe/internal/integration/github"
	"github.com/Clouditera/lathe/internal/task"
	skillsembed "github.com/Clouditera/lathe/skills"
)

// F7.2-AC1/AC3：节点画像声明存在的技能时，Execute 跑完整条链路后
// worktree 的 .claude/skills/<name>/ 必须真的被物化出来（内容与嵌入的
// 技能定义一致），且这些文件必须对 git 不可见——不管是 `git status
// --porcelain` 这种最直接的验证，还是流水线自己用的
// Worktrees.HasChanges/ChangedFiles，都不应该看到 .claude 路径下的东西。
func TestPipelineSkillsMaterializedAndExcludedFromGit(t *testing.T) {
	m, taskID, repo, src := profileFixture(t, "CR-SKILL-OK",
		[]byte(`{"skills":[{"name":"go-testing","version":"1.0.0"}]}`))

	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{pr: &github.PullRequest{Number: 1, URL: "https://github.com/acme/demo-profile/pull/1"}}
	ag := implFixAgent()
	no := &fakeNotifier{}
	p := newPipeline(t, m, lin, gh, ag, no)

	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777", Actor: "test",
	}); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}

	ctx := context.Background()
	final, err := m.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if final.State != task.StatePROpen {
		t.Fatalf("终态 = %s，期望 pr_open", final.State)
	}
	if final.WorktreePath == nil || *final.WorktreePath == "" {
		t.Fatalf("worktree 路径未落库")
	}
	wtPath := *final.WorktreePath

	// 1. 物化出来的文件确实存在，内容与嵌入的技能定义一致。
	gotSkillMD, err := readFileT(t, filepath.Join(wtPath, ".claude", "skills", "go-testing", "SKILL.md"))
	if err != nil {
		t.Fatalf("读取物化后的 SKILL.md 失败: %v", err)
	}
	wantSkillMD, err := readFileT(t, filepath.Join(skillsSourceDir(t), "go-testing", "1.0.0", "SKILL.md"))
	if err != nil {
		t.Fatalf("读取源 SKILL.md 失败: %v", err)
	}
	if gotSkillMD != wantSkillMD {
		t.Errorf("物化后的 SKILL.md 内容与源不一致")
	}
	gotChecklist, err := readFileT(t, filepath.Join(wtPath, ".claude", "skills", "go-testing", "references", "checklist.md"))
	if err != nil {
		t.Fatalf("读取物化后的 references/checklist.md 失败: %v", err)
	}
	if strings.TrimSpace(gotChecklist) == "" {
		t.Errorf("references/checklist.md 物化后为空")
	}

	// 2. .git/info/exclude 里出现了排除规则。
	excludeOut, err := exec.Command("git", "-C", wtPath, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		t.Fatalf("解析 git-path 失败: %v", err)
	}
	excludeContent, err := readFileT(t, strings.TrimSpace(string(excludeOut)))
	if err != nil {
		t.Fatalf("读取 .git/info/exclude 失败: %v", err)
	}
	if !strings.Contains(excludeContent, ".claude/skills/") {
		t.Errorf(".git/info/exclude 内容 = %q，应含 .claude/skills/", excludeContent)
	}

	// 3. F7.2-AC3 最直接的验证：git status --porcelain 看不到 .claude
	// 下任何未跟踪文件（这正是它们不会被 git add -A 提交、不会进 PR
	// diff 的原因）。
	statusOut, err := exec.Command("git", "-C", wtPath, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git status 失败: %v", err)
	}
	if strings.Contains(string(statusOut), ".claude") {
		t.Errorf("git status --porcelain 不应看到 .claude 路径，实际:\n%s", statusOut)
	}

	// 4. 交叉验证：流水线自己用的 Worktrees.HasChanges/ChangedFiles 也应
	// 看不到 .claude 路径。
	//
	// 注意：HasChanges 底层是 `git status --porcelain`，light/heavy 档的
	// build 步骤（`go build ./...`）会在 worktree 根目录留下一个未提交
	// 的可执行文件（模块名同名的二进制，Go 的常见行为，与本次改动无
	// 关），因此 HasChanges 在这个夹具下为 true 是预期的——这里断言的
	// 不是"没有任何改动"，而是"HasChanges 的判断与 git status 直接给出
	// 的结果一致，且那份 git status 里不含 .claude"（第 3 步已验证）。
	// ChangedFiles 走 `git diff` 比对已提交历史，不受未跟踪文件影响，
	// 直接断言其中不含 .claude 路径即可。
	wt := &Worktree{
		Path: wtPath, Branch: deref(final.BranchName),
		BaseBranch: mustBaseBranch(t, repo), Mirror: p.Worktrees.MirrorPath(repo.ProviderRepo),
	}
	hasChanges, err := p.Worktrees.HasChanges(ctx, wt)
	if err != nil {
		t.Fatalf("HasChanges 失败: %v", err)
	}
	wantHasChanges := strings.TrimSpace(string(statusOut)) != ""
	if hasChanges != wantHasChanges {
		t.Errorf("HasChanges = %v，与 git status --porcelain 的结果（非空=%v）不一致", hasChanges, wantHasChanges)
	}
	changedFiles, err := p.Worktrees.ChangedFiles(ctx, wt)
	if err != nil {
		t.Fatalf("ChangedFiles 失败: %v", err)
	}
	for _, f := range changedFiles {
		if strings.HasPrefix(f, ".claude") {
			t.Errorf("ChangedFiles 不应包含 .claude 路径，实际: %v", changedFiles)
		}
	}
}

// F7.2-AC5：声明了不存在的技能（名字或版本任一对不上）时，任务必须以
// 可读原因失败，不静默忽略——FailureStage 落 StageSkillMissing，
// FailureReason 里点名具体是哪个技能名 + 版本号找不到。
func TestPipelineSkillMissingFailsTaskWithReadableReason(t *testing.T) {
	m, taskID, repo, src := profileFixture(t, "CR-SKILL-MISSING",
		[]byte(`{"skills":[{"name":"does-not-exist","version":"9.9.9"}]}`))

	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{}
	ag := implFixAgent()
	no := &fakeNotifier{}
	p := newPipeline(t, m, lin, gh, ag, no)

	err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777", Actor: "test",
	})
	if err == nil {
		t.Fatal("声明不存在的技能应导致 Execute 返回错误")
	}

	final, gerr := m.Get(context.Background(), taskID)
	if gerr != nil {
		t.Fatalf("Get 失败: %v", gerr)
	}
	if final.State != task.StateFailed {
		t.Fatalf("终态 = %s，期望 failed", final.State)
	}
	if final.FailureStage == nil || *final.FailureStage != string(StageSkillMissing) {
		t.Errorf("FailureStage = %v，期望 %q", final.FailureStage, StageSkillMissing)
	}
	if final.FailureReason == nil ||
		!strings.Contains(*final.FailureReason, "does-not-exist") ||
		!strings.Contains(*final.FailureReason, "9.9.9") {
		t.Errorf("FailureReason 应点名具体技能名+版本号，实际: %v", final.FailureReason)
	}
	if len(gh.params) != 0 {
		t.Error("技能缺失应在开 PR 之前就失败，不应创建 PR")
	}

	// worktree 现场仍应保留，供人接手排查（既有失败路径的一致行为）。
	if final.WorktreePath == nil || *final.WorktreePath == "" {
		t.Error("失败时应保留 worktree 现场")
	}
}

// TestPipelineSkillMissingSurvivesRetryAsResume 是本次修复（去掉
// materializeSkills 的 fresh/resume 跳过条件）的核心回归测试。
//
// 复现的真实缺陷（修复前）：技能缺失导致的首次失败发生在 worktree 已经
// 建出、implSession/WorktreePath/BranchName 已经落库【之后】，物化校验
// 【之前】。人工/自动重试时，PlanRetry（纯逻辑决策表）只看失败阶段
// +worktree 现场体检——它不认识 StageSkillMissing，会落进"未知阶段"分支，
// 现场体检看到"worktree 存在、分支存在、但还没有提交"，于是判定为
// EntryImplement + resume（ResumeSession: true）。stageImplement 因此
// 以 fresh=false 调 materializeSkills，旧实现里 fresh=false 直接
// return nil——技能仍然缺失，但校验完全不会再跑，agent 会带着缺失的
// 技能继续往下跑，甚至跑完整条链路开出 PR，这正是 F7.2-AC5 明确禁止的
// "静默忽略"。
//
// 这里的重试构造严格复用生产路径：httpapi.retryTask 的 failed→queued
// 转移方式，以及 cmd/lathe/queue.go 的 planRetry 用 runner.PlanRetry +
// Worktrees.Inspect 凑齐 RetryInput 的方式（mode=auto，Stage 取
// FailureStage，HasSession 取 AgentSessionID 是否非空）——不是发明一套
// 简化版重试逻辑。
func TestPipelineSkillMissingSurvivesRetryAsResume(t *testing.T) {
	m, taskID, repo, src := profileFixture(t, "CR-SKILL-RETRY",
		[]byte(`{"skills":[{"name":"does-not-exist","version":"9.9.9"}]}`))

	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{pr: &github.PullRequest{Number: 1, URL: "https://github.com/acme/demo-profile/pull/1"}}
	// implFixAgent 的第二个结果（实现产出）在旧实现的 bug 场景下会被第
	// 二次 Execute 消费掉——如果修复没生效，agent 会正常"跑完"并写出
	// fix.go/main_test.go，流水线会一路推进到开 PR。断言部分正是要确认
	// 这件事没有发生。
	ag := implFixAgent()
	no := &fakeNotifier{}
	p := newPipeline(t, m, lin, gh, ag, no)

	ctx := context.Background()

	// ---- 第一次执行：技能缺失，按预期失败 ----
	if err := p.Execute(ctx, ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777", Actor: "test",
	}); err == nil {
		t.Fatal("首次 Execute 应因技能缺失失败")
	}

	afterFirst, err := m.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if afterFirst.State != task.StateFailed {
		t.Fatalf("首次失败后状态 = %s，期望 failed", afterFirst.State)
	}
	if afterFirst.FailureStage == nil || *afterFirst.FailureStage != string(StageSkillMissing) {
		t.Fatalf("首次失败阶段 = %v，期望 %q", afterFirst.FailureStage, StageSkillMissing)
	}
	if afterFirst.WorktreePath == nil || *afterFirst.WorktreePath == "" {
		t.Fatal("首次失败应已落库 worktree 路径（技能检查在建 worktree 之后才跑）")
	}

	// ---- 人工点重试：与 httpapi.retryTask 一致，failed -> queued，
	// mode 走事件 payload（这里直达 auto，等价于空 mode 请求体）----
	if _, err := m.Transition(ctx, taskID, task.StateQueued, "test", &task.TransitionOpts{
		Payload: map[string]any{"reason": "manual_retry", "mode": string(RetryAuto)},
	}); err != nil {
		t.Fatalf("failed -> queued 转移失败: %v", err)
	}

	// ---- 与 cmd/lathe/queue.go 的 planRetry 完全一致的方式凑 RetryInput
	// ----
	kind := KindFix
	if afterFirst.TaskKind != nil && *afterFirst.TaskKind != "" {
		kind = TaskKind(*afterFirst.TaskKind)
	}
	base, err := repo.BaseBranch(kind)
	if err != nil {
		t.Fatalf("BaseBranch 失败: %v", err)
	}
	branch := deref(afterFirst.BranchName)
	wtState := p.Worktrees.Inspect(ctx, repo.ProviderRepo, *afterFirst.WorktreePath, branch, base)
	stage := Stage(*afterFirst.FailureStage)
	plan := PlanRetry(RetryAuto, RetryInput{
		Stage:      stage,
		HasSession: afterFirst.AgentSessionID != nil && *afterFirst.AgentSessionID != "",
		WT:         wtState,
	})

	// 决策必须真的落在"续跑实现"这条会触发 bug 的路径上，否则这个回归
	// 测试根本没测到问题场景。
	if plan.Fresh || plan.Entry != EntryImplement {
		t.Fatalf("PlanRetry 决策 = %+v，期望 Fresh=false Entry=implement"+
			"（技能缺失属于 PlanRetry 认不出的未知阶段，worktree 现场体检"+
			"应判定为可续跑）", plan)
	}
	if !plan.ResumeSession {
		t.Fatalf("PlanRetry 决策 = %+v，期望 ResumeSession=true（有会话凭据）", plan)
	}

	// ---- 第二次执行：等价于 queue.go runOneClaimed 把决策喂给
	// pipeline.Execute ----
	execErr := p.Execute(ctx, ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777", Actor: "test",
		Retry: &plan,
	})

	// ---- 核心断言：重试仍必须因技能缺失失败，不能被 resume 悄悄绕过 ----
	if execErr == nil {
		t.Fatal("重试（resume 到实现阶段）仍应因技能缺失失败，实际 Execute 返回 nil")
	}
	final, err := m.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if final.State != task.StateFailed {
		t.Fatalf("重试后终态 = %s，期望 failed（如果是别的终态，说明技能校验被绕过了）", final.State)
	}
	if final.FailureStage == nil || *final.FailureStage != string(StageSkillMissing) {
		t.Fatalf("重试后失败阶段 = %v，期望仍是 %q（不能是别的阶段，"+
			"比如实现/验证/推送——那意味着 agent 带着缺失的技能继续跑下去了）",
			final.FailureStage, StageSkillMissing)
	}
	if len(gh.params) != 0 {
		t.Error("技能缺失应在重试后再次拦在开 PR 之前，不应创建 PR")
	}
	if len(ag.calls) != 1 {
		t.Errorf("agent 的实现调用不应被消费（技能校验应在 runAgent(implement) 之前拦下），"+
			"实际调用次数 = %d", len(ag.calls))
	}
}

func readFileT(t *testing.T, path string) (string, error) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// skillsSourceDir 定位仓库根的 skills/ 目录（与 internal/runner 同级的
// 上三级），供测试比对物化产物与源内容是否一致。
func skillsSourceDir(t *testing.T) string {
	t.Helper()
	// internal/runner -> internal -> 仓库根
	return filepath.Join("..", "..", "skills")
}

func mustBaseBranch(t *testing.T, repo RepoConfig) string {
	t.Helper()
	base, err := repo.BaseBranch(KindFix)
	if err != nil {
		t.Fatalf("BaseBranch 失败: %v", err)
	}
	return base
}

// 编译期断言：确保测试确实链接到了 skills 包（避免手滑删掉 import 后
// 编译器无声无息地不再校验这条依赖关系）。
var _ = skillsembed.ErrSkillNotFound
