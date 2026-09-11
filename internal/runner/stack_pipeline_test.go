package runner

// stack_pipeline_test.go 在 runHeavy 这一层钉 T8 的三条行为：
//   - 降级时标记进 Report（回帖能看见），并把 stackEnv 留空
//   - 判死时错误身份是 ErrVerifyStackUp（不冒充复现阶段错误）
//   - 起栈成功时 Down 必被调用，且**根 ctx 已取消也照样调**
//
// 这一层比 stack_test.go 的纯错误分类更靠前：那些测的是判定函数，
// 这些测的是 runHeavy 里的实际接线（switch 分支、defer、字段传递）。
//
// 需要真 Postgres + git（复用本包既有的 pipelineFixture / goSourceRepo）。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// heavyStackFixture 建好 mirror 与任务工作区，返回可直接喂 runHeavy 的料。
func heavyStackFixture(t *testing.T) (*Pipeline, int64, string, *Worktree) {
	t.Helper()
	_, m, taskID, repo, src := pipelineFixture(t)

	p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, &fakeGitHub{}, &fakeAgent{}, &fakeNotifier{})
	ctx := context.Background()
	if _, err := p.Worktrees.EnsureMirror(ctx, repo.ProviderRepo, src); err != nil {
		t.Fatalf("建 mirror 失败: %v", err)
	}
	wt, err := p.Worktrees.Create(ctx, CreateParams{
		Repo: repo, CloneURL: src, Kind: KindFix, IssueKey: "CR-777", Title: "t",
	})
	if err != nil {
		t.Fatalf("建任务工作区失败: %v", err)
	}
	t.Cleanup(func() { _ = p.Worktrees.Remove(context.Background(), wt, true) })
	return p, taskID, repo.ProviderRepo, wt
}

// ★ 水位超阈值 → 降级：验证照常给出结论，但 Report 必须带降级标记，
// 且 stackEnv 为空（没有栈可注入）。
//
// 标记必须进 Report 而不只落事件流：Summary() 是回帖到 Linear 的那份，
// 人最常看到的就是它。只落 agent_events 的话，一次「验证通过」看起来
// 与隔离完好时一模一样。
func TestRunHeavyDegradesAndMarksReport(t *testing.T) {
	p, taskID, providerRepo, wt := heavyStackFixture(t)
	rec := &captureEvents{}
	p.AgentEvents = rec
	p.Stacks = &fakeStackUp{
		upErr: fmt.Errorf("%w: 磁盘占用 95%% 已达阈值 90%%", ErrStackOverThreshold),
	}

	rep, err := p.runHeavy(context.Background(), taskID, providerRepo, wt,
		[]Step{{Name: StepBuild, Cmd: []string{"true"}}}, nil, nil, []string{"postgres"})
	if err != nil {
		t.Fatalf("水位超阈值应降级而不是返回错误: %v", err)
	}
	if !rep.StackDegraded {
		t.Error("降级必须在 Report 上留标记 —— 否则回帖里看不出这轮跑在共享环境上")
	}
	if !containsNote(rep.Summary()) {
		t.Errorf("回帖摘要应带降级警示：\n%s", rep.Summary())
	}
	// 事件流那条也要有（两条腿都要）
	if len(rec.entries) == 0 {
		t.Error("降级也应落一条 agent_events")
	}
	// 没有栈时不该给步骤注入任何连接串
	for _, r := range rep.Results {
		if len(r.Step.StackEnv) != 0 {
			t.Errorf("降级时步骤不该带连接串，%s 上却有 %v", r.Step.Name, r.Step.StackEnv)
		}
	}
}

// ★ 非水位错误 → 判死，且错误身份是 ErrVerifyStackUp。
//
// docker 不可用是这里最要紧的一种：CheckResources 的第一分支就是
// !DockerOK → Allowed=false。若它冒充水位错误，就会走上面那条降级路径，
// 日志写「资源水位不允许」，把人引去调阈值。
func TestRunHeavyFailsHardOnStackErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"未知依赖名", errors.New("未知的基础设施 \"postgre\"")},
		{"镜像拉不下来", errors.New("启动验证依赖 postgres 失败: pull access denied")},
		{"docker 守护进程不可用", fmt.Errorf("%w: docker 守护进程不可用", ErrStackDockerDown)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, taskID, providerRepo, wt := heavyStackFixture(t)
			p.Stacks = &fakeStackUp{upErr: tc.err}

			_, err := p.runHeavy(context.Background(), taskID, providerRepo, wt,
				[]Step{{Name: StepBuild, Cmd: []string{"true"}}}, nil, nil, []string{"postgres"})
			if err == nil {
				t.Fatal("应判死而不是降级")
			}
			if !errors.Is(err, ErrVerifyStackUp) {
				t.Errorf("错误身份应是 ErrVerifyStackUp，得到 %v", err)
			}
			// 绝不能冒充复现阶段的错误 —— 那会让人去查一个没问题的测试命令
			if isReproContractErr(err) {
				t.Errorf("起栈失败不该被当成复现契约违例：%v", err)
			}
		})
	}
}

// ★ 起栈成功：连接串注入每个步骤，且 Down 必被调用（生命周期对称）。
func TestRunHeavyInjectsStackEnvAndTearsDown(t *testing.T) {
	p, taskID, providerRepo, wt := heavyStackFixture(t)
	f := &fakeStackUp{env: map[string]string{"DATABASE_URL": "postgres://127.0.0.1:49173/app"}}
	p.Stacks = f

	rep, err := p.runHeavy(context.Background(), taskID, providerRepo, wt,
		[]Step{{Name: StepBuild, Cmd: []string{"true"}}}, nil, nil, []string{"postgres"})
	if err != nil {
		t.Fatalf("runHeavy 失败: %v", err)
	}
	if rep.StackDegraded {
		t.Error("起栈成功不该标降级")
	}
	if containsNote(rep.Summary()) {
		t.Errorf("起栈成功的回帖不该带降级警示：\n%s", rep.Summary())
	}
	if f.downCalls != 1 {
		t.Errorf("Down 应被调一次（起—拆对称），实际 %d", f.downCalls)
	}
	// light 步骤上应带着连接串
	var seen bool
	for _, r := range rep.Results {
		if r.Step.Name == StepBuild {
			seen = true
			if r.Step.StackEnv["DATABASE_URL"] != "postgres://127.0.0.1:49173/app" {
				t.Errorf("步骤应带隔离栈连接串，得到 %v", r.Step.StackEnv)
			}
		}
	}
	if !seen {
		t.Fatalf("报告里应有 light 步骤：%+v", rep.Results)
	}
}

// ★ B2 在 pipeline 这一层的形态：栈起来之后根 ctx 被取消，Down 仍被调用。
//
// runHeavy 的 `defer stack.Down(ctx)` 用的就是收到的那个 ctx（node.work
// 的根 ctx，SIGINT/SIGTERM 时被取消）。这里断言 Down 确实被调到，
// 并且它收到的正是那个已取消的 ctx —— 也就是说「让拆栈仍然生效」这件事
// 只能由 Down 内部自己解绑（preview.VerifyStack.Down 做的），
// 调用点无从代劳。verifystack_test.go 的
// TestVerifyStackDownSurvivesCancelledContext 钉的是解绑那一半。
//
// 取消必须发生在**起栈之后**：预先取消的话 CreateDetached（git worktree
// add）先失败，runHeavy 根本走不到起栈那一步 —— 那种情形没有栈可泄漏，
// 不是这条 bug 的现场。真实现场是「heavy 验证正在跑时 Ctrl-C」。
func TestRunHeavyTearsDownEvenWhenRootCtxCancelled(t *testing.T) {
	p, taskID, providerRepo, wt := heavyStackFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeStackUp{
		env: map[string]string{"DATABASE_URL": "x"},
		// 栈刚起来就收到 SIGINT：这正是容器泄漏的现场
		onUp: cancel,
	}
	p.Stacks = f

	_, _ = p.runHeavy(ctx, taskID, providerRepo, wt,
		[]Step{{Name: StepBuild, Cmd: []string{"true"}}}, nil, nil, []string{"postgres"})

	if f.downCalls != 1 {
		t.Fatalf("根 ctx 取消也必须拆栈（否则容器永久留在机器上），Down 调用次数 %d", f.downCalls)
	}
	if len(f.downCtxDone) != 1 || !f.downCtxDone[0] {
		t.Errorf("Down 收到的应是那个已取消的 ctx（解绑取消只能在 Down 内部做），实际 %v", f.downCtxDone)
	}
}

// Stacks 为 nil（未装配）时完全走老路：不起栈、不留痕、不报错。
// 这是「默认空 = 无隔离的等价性」，本 PR 刻意保留的行为。
func TestRunHeavyWithoutStacksIsUnchanged(t *testing.T) {
	p, taskID, providerRepo, wt := heavyStackFixture(t)
	rec := &captureEvents{}
	p.AgentEvents = rec
	p.Stacks = nil

	rep, err := p.runHeavy(context.Background(), taskID, providerRepo, wt,
		[]Step{{Name: StepBuild, Cmd: []string{"true"}}}, nil, nil, []string{"postgres"})
	if err != nil {
		t.Fatalf("未装配 Stacks 时不该报错: %v", err)
	}
	if rep.StackDegraded {
		t.Error("未装配 Stacks 不算「降级」—— 那是本字段引入之前的常态形态，不该每条回帖都挂警示")
	}
	if len(rec.entries) != 0 {
		t.Errorf("未装配 Stacks 不该留降级痕，实际落了 %d 条", len(rec.entries))
	}
}

// 仓库没声明依赖（repoInfra 为空）时不起栈，连 UpVerifyStack 都不该调。
func TestRunHeavySkipsStackWithoutRepoInfra(t *testing.T) {
	p, taskID, providerRepo, wt := heavyStackFixture(t)
	f := &fakeStackUp{}
	p.Stacks = f

	rep, err := p.runHeavy(context.Background(), taskID, providerRepo, wt,
		[]Step{{Name: StepBuild, Cmd: []string{"true"}}}, nil, nil, nil)
	if err != nil {
		t.Fatalf("没声明依赖不该报错: %v", err)
	}
	if len(f.upCalls) != 0 {
		t.Errorf("没声明依赖时不该调 UpVerifyStack，实际调了 %v", f.upCalls)
	}
	if rep.StackDegraded {
		t.Error("没声明依赖不算降级")
	}
}

// containsNote 判断回帖摘要里有没有那行降级警示。
func containsNote(s string) bool { return strings.Contains(s, StackDegradedNote) }
