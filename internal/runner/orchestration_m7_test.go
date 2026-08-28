package runner

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Clouditera/lathe/internal/flow"
	"github.com/Clouditera/lathe/internal/integration/github"
	"github.com/Clouditera/lathe/internal/task"
)

// ================================================================
// docs/07-prd-orchestration.md §5 · M7 出口条件端到端集成测试
//
//	"同一张图的两个节点路由到不同模型通道；技能声明生效且不进 PR"
//
// 本文件只消费前两阶段已经交付的东西：
//   - flow.Service.CreateFlow 建图（不重新实现，与 orchestration_m1_
//     test.go 同一手法，节点画像走 flow.NodeInput.Profile 原样落库）
//   - runner.ParseProfile / stageMaterializeSkills 对 model_channel /
//     verify_tier / skills 字段的真实消费逻辑（pipeline.go、skills.go，
//     不重新发明假件行为）
//   - orchestration_m1_test.go 的 orchestrationFixture/fakeIssueFor/
//     orchestrationPipeline/drainScheduler 与 pipeline_profile_test.go
//     的 implFixAgent、pipeline_skills_test.go 的 readFileT/
//     skillsSourceDir/mustBaseBranch（同包直接复用，不重新造假件）
// ================================================================

// ----------------------------------------------------------------
// 场景一：同一张图的两个节点路由到不同模型通道
// ----------------------------------------------------------------

// TestM7OrchestrationModelChannelPerNodeAcrossGraph 直接证明 M7 出口
// 条件第一句："同一张图的两个节点路由到不同模型通道"。
//
// 用 flow.Service.CreateFlow 建一张图，两个独立根节点：A 的画像设
// model_channel="channel-a"；B 不设画像。两个 Pipeline 假件都把
// ImplementChannel（仓库/流水线级默认通道）配成同一个值
// "pipeline-default-channel"——如果不这样配，B 落回默认通道后仍可能
// 恰好等于某个随手写的字符串，不足以证明"取的确实是画像覆盖值，不是
// 别的什么"。全程走 ClaimReady/Execute 真实调度到 pr_open（drain
// Scheduler，同 M1 手法），断言 A 实现阶段收到的 ExtraEnv 是
// channel-a，B 收到的是 pipeline 级默认通道——二者不同，且各自可
// 追溯到期望的来源。
func TestM7OrchestrationModelChannelPerNodeAcrossGraph(t *testing.T) {
	pool, m, userID, repoID, repo, src := orchestrationFixture(t)
	ctx := context.Background()

	svc := &flow.Service{Pool: pool, Tasks: m}
	nodes := []flow.NodeInput{
		{IssueKey: "M7-CHAN-A-" + t.Name(), Profile: []byte(`{"model_channel":"channel-a"}`)},
		{IssueKey: "M7-CHAN-B-" + t.Name()}, // 不设画像
	}
	_, created, _, err := svc.CreateFlow(ctx, userID, repoID, "m7-chan-"+t.Name(), nodes)
	if err != nil {
		t.Fatalf("建图应成功，得到 %v", err)
	}
	if len(created) != 2 {
		t.Fatalf("应建出 2 个任务，得到 %d", len(created))
	}
	taskA, taskB := created[0], created[1]
	if taskA.DependsOn != nil || taskB.DependsOn != nil {
		t.Fatalf("A/B 都应是独立根，得到 A.DependsOn=%v B.DependsOn=%v", taskA.DependsOn, taskB.DependsOn)
	}

	wm, err := NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatalf("建工作区管理器失败: %v", err)
	}

	linA := &fakeLinear{issue: fakeIssueFor(taskA.LinearIssueKey)}
	ghA := &fakeGitHub{pr: &github.PullRequest{Number: 501, URL: "https://github.com/acme/demo/pull/501"}}
	agA := implFixAgent()
	pA := orchestrationPipeline(m, wm, linA, ghA, agA)
	pA.ImplementChannel = "pipeline-default-channel"

	linB := &fakeLinear{issue: fakeIssueFor(taskB.LinearIssueKey)}
	ghB := &fakeGitHub{pr: &github.PullRequest{Number: 502, URL: "https://github.com/acme/demo/pull/502"}}
	agB := implFixAgent()
	pB := orchestrationPipeline(m, wm, linB, ghB, agB)
	pB.ImplementChannel = "pipeline-default-channel"

	pipelines := map[int64]*Pipeline{taskA.ID: pA, taskB.ID: pB}

	// 全程只调用 ClaimReady/Execute（drainScheduler），与 M1 场景一
	// 同一手法：证明这条路径不需要任何手工状态转移就能各自跑到
	// pr_open，画像的影响完全体现在 Execute 内部真实调用 agent 时
	// 带的 ExtraEnv 上。
	drainScheduler(t, m, repo, src, pipelines)

	finalA, err := m.Get(ctx, taskA.ID)
	if err != nil {
		t.Fatalf("读取任务 A 失败: %v", err)
	}
	if finalA.State != task.StatePROpen {
		t.Fatalf("任务 A 终态 = %s，期望 pr_open", finalA.State)
	}
	finalB, err := m.Get(ctx, taskB.ID)
	if err != nil {
		t.Fatalf("读取任务 B 失败: %v", err)
	}
	if finalB.State != task.StatePROpen {
		t.Fatalf("任务 B 终态 = %s，期望 pr_open", finalB.State)
	}

	// "含所有对 fakeAgent 的调用"：implFixAgent 是一次成功剧本（分诊+
	// 实现各一次，不进修复回路），calls[1:] 就是全部实现阶段调用；
	// 用循环而不是硬编码 calls[1] 是为了在剧本以后被改成带修复回路时
	// 这条断言依然成立（修复回路的每一跳都该沿用同一条通道）。
	if len(agA.calls) < 2 {
		t.Fatalf("节点 A 应至少有 2 次 agent 调用（分诊+实现），实际 %d", len(agA.calls))
	}
	for i, call := range agA.calls[1:] {
		if len(call.ExtraEnv) != 1 || call.ExtraEnv[0] != "LATHE_AGENT_CHANNEL=channel-a" {
			t.Errorf("节点 A 第 %d 次实现阶段调用的通道应为画像指定的 channel-a，实际: %v", i+1, call.ExtraEnv)
		}
	}
	if len(agB.calls) < 2 {
		t.Fatalf("节点 B 应至少有 2 次 agent 调用（分诊+实现），实际 %d", len(agB.calls))
	}
	for i, call := range agB.calls[1:] {
		if len(call.ExtraEnv) != 1 || call.ExtraEnv[0] != "LATHE_AGENT_CHANNEL=pipeline-default-channel" {
			t.Errorf("节点 B 第 %d 次实现阶段调用应回落到 pipeline 级默认通道 pipeline-default-channel，实际: %v", i+1, call.ExtraEnv)
		}
	}

	// 核心断言：同一张图（同一个 flowID）的两个节点，实现阶段实际收到
	// 的通道确实不同——这正是 M7 出口条件第一句的字面意思。
	if agA.calls[1].ExtraEnv[0] == agB.calls[1].ExtraEnv[0] {
		t.Fatalf("A/B 应路由到不同模型通道，实际都是 %q", agA.calls[1].ExtraEnv[0])
	}

	// 分诊阶段两个节点都不该受画像影响（既有设计意图的回归，F7.1-AC4）。
	if len(agA.calls[0].ExtraEnv) != 0 {
		t.Errorf("节点 A 分诊阶段不应受画像影响，实际 ExtraEnv=%v", agA.calls[0].ExtraEnv)
	}
	if len(agB.calls[0].ExtraEnv) != 0 {
		t.Errorf("节点 B 分诊阶段不应受画像影响，实际 ExtraEnv=%v", agB.calls[0].ExtraEnv)
	}
}

// ----------------------------------------------------------------
// 场景二：技能声明生效，物化文件确实存在，且不进 PR
// ----------------------------------------------------------------

// TestM7OrchestrationSkillDeclaredMaterializesAndExcludedFromPR 证明
// M7 出口条件第二句的正面情形："技能声明生效...不进 PR"。节点通过
// flow.Service.CreateFlow 建出，画像声明上一阶段建好的示例技能
// go-testing@1.0.0，跑完整 Execute 到 pr_open：
//  1. worktree 里 .claude/skills/go-testing/ 下的文件确实被物化，内容
//     与仓库 skills/ 源目录一致；
//  2. 这个任务开出的"PR"对应的 fakeGitHub.params 确实被调用了一次
//     （证明流程真的走到了开 PR 这一步，不是在半路失败）；
//  3. 物化之后 worktree 的 git status --porcelain 里看不到任何
//     .claude 路径——只有 fakeAgent 制造的业务改动（fix.go/
//     main_test.go，已被流水线提交）和 `go build ./...` 留下的同名
//     可执行文件（既有已知行为，与技能无关）；
//  4. 用 Worktrees.ChangedFiles（PR diff 实际依据的比对）断言技能路径
//     不在其中，同时断言业务改动确实在其中——不是"整个 diff 是空的"
//     这种廉价满足。
func TestM7OrchestrationSkillDeclaredMaterializesAndExcludedFromPR(t *testing.T) {
	pool, m, userID, repoID, repo, src := orchestrationFixture(t)
	ctx := context.Background()

	svc := &flow.Service{Pool: pool, Tasks: m}
	nodes := []flow.NodeInput{
		{IssueKey: "M7-SKILL-OK-" + t.Name(), Profile: []byte(`{"skills":[{"name":"go-testing","version":"1.0.0"}]}`)},
	}
	_, created, _, err := svc.CreateFlow(ctx, userID, repoID, "m7-skill-ok-"+t.Name(), nodes)
	if err != nil {
		t.Fatalf("建图应成功，得到 %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("应建出 1 个任务，得到 %d", len(created))
	}
	tk := created[0]

	wm, err := NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatalf("建工作区管理器失败: %v", err)
	}
	lin := &fakeLinear{issue: fakeIssueFor(tk.LinearIssueKey)}
	gh := &fakeGitHub{pr: &github.PullRequest{Number: 601, URL: "https://github.com/acme/demo/pull/601"}}
	ag := implFixAgent()
	p := orchestrationPipeline(m, wm, lin, gh, ag)

	issueID := ""
	if tk.LinearIssueID != nil {
		issueID = *tk.LinearIssueID
	}
	if err := p.Execute(ctx, ExecuteParams{
		TaskID: tk.ID, Repo: repo, CloneURL: src, IssueID: issueID, Actor: "node:test",
	}); err != nil {
		t.Fatalf("Execute 应成功，得到 %v", err)
	}

	final, err := m.Get(ctx, tk.ID)
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

	// ---- 1. 技能声明生效：物化文件确实存在，内容与源一致 ----
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

	// ---- 2. 这个任务确实开出了 PR（fakeGitHub 记录的调用）----
	if len(gh.params) != 1 {
		t.Fatalf("任务应创建 1 个 PR，实际 %d", len(gh.params))
	}

	// ---- 3. 物化之后 worktree 的 git status 看不到 .claude 路径 ----
	statusOut, err := exec.Command("git", "-C", wtPath, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git status 失败: %v", err)
	}
	if strings.Contains(string(statusOut), ".claude") {
		t.Errorf("git status --porcelain 不应看到 .claude 路径，实际:\n%s", statusOut)
	}

	// ---- 4. m.ChangedFiles（此处 p.Worktrees，PR diff 的真实依据）
	// 断言技能路径不在里面，业务改动在里面 ----
	wt := &Worktree{
		Path: wtPath, Branch: deref(final.BranchName),
		BaseBranch: mustBaseBranch(t, repo), Mirror: p.Worktrees.MirrorPath(repo.ProviderRepo),
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
	wantBusiness := map[string]bool{"fix.go": false, "main_test.go": false}
	for _, f := range changedFiles {
		if _, ok := wantBusiness[f]; ok {
			wantBusiness[f] = true
		}
	}
	for f, seen := range wantBusiness {
		if !seen {
			t.Errorf("ChangedFiles 应包含 fakeAgent 制造的业务改动 %s，实际: %v", f, changedFiles)
		}
	}
}

// ----------------------------------------------------------------
// 场景三：声明不存在的技能，任务失败且失败原因点名该技能
// ----------------------------------------------------------------

// TestM7OrchestrationSkillMissingFailsTaskNamingSkill 证明 M7 出口
// 条件第二句的负面情形：声明一个不存在的技能时，任务必须以可读原因
// 失败——失败阶段落 StageSkillMissing，失败原因点名具体是哪个技能名 +
// 版本号找不到，且失败发生在开 PR 之前（fakeGitHub 未被调用）。
func TestM7OrchestrationSkillMissingFailsTaskNamingSkill(t *testing.T) {
	pool, m, userID, repoID, repo, src := orchestrationFixture(t)
	ctx := context.Background()

	svc := &flow.Service{Pool: pool, Tasks: m}
	nodes := []flow.NodeInput{
		{IssueKey: "M7-SKILL-MISS-" + t.Name(), Profile: []byte(`{"skills":[{"name":"does-not-exist-m7","version":"9.9.9"}]}`)},
	}
	_, created, _, err := svc.CreateFlow(ctx, userID, repoID, "m7-skill-miss-"+t.Name(), nodes)
	if err != nil {
		t.Fatalf("建图应成功，得到 %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("应建出 1 个任务，得到 %d", len(created))
	}
	tk := created[0]

	wm, err := NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatalf("建工作区管理器失败: %v", err)
	}
	lin := &fakeLinear{issue: fakeIssueFor(tk.LinearIssueKey)}
	gh := &fakeGitHub{}
	ag := implFixAgent()
	p := orchestrationPipeline(m, wm, lin, gh, ag)

	issueID := ""
	if tk.LinearIssueID != nil {
		issueID = *tk.LinearIssueID
	}
	err = p.Execute(ctx, ExecuteParams{
		TaskID: tk.ID, Repo: repo, CloneURL: src, IssueID: issueID, Actor: "node:test",
	})
	if err == nil {
		t.Fatal("声明不存在的技能应导致 Execute 返回错误")
	}

	final, gerr := m.Get(ctx, tk.ID)
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
		!strings.Contains(*final.FailureReason, "does-not-exist-m7") ||
		!strings.Contains(*final.FailureReason, "9.9.9") {
		t.Errorf("FailureReason 应点名具体技能名+版本号，实际: %v", final.FailureReason)
	}
	if len(gh.params) != 0 {
		t.Error("技能缺失应在开 PR 之前就失败，不应创建 PR")
	}
}
