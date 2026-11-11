package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
	"github.com/zichuanwangcloud-gif/lathe/internal/integration/github"
)

// f01（运行中插话）的 runner 侧 A 类回归红线：改动前写、改动前后都必须绿。

// AC-8 红线：无插话时实现阶段 prompt 逐字等于 ImplementPrompt(...) 的构造，
// 不追加空段落/包裹语。前提是没有失败记忆：Memory 为 nil 与空各跑一遍。
func TestRunAgentPromptUnchangedWithoutInterjections(t *testing.T) {
	cases := []struct {
		name string
		mem  FailureMemory
	}{
		{"Memory为nil", nil},
		{"Memory为空", newFakeMemory()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, m, taskID, repo, src := pipelineFixture(t)
			ag := &fakeAgent{results: []*agent.Result{
				{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象","question":""}`},
				{Success: false, IsError: true, Subtype: "error_during_execution", Text: "挂了"},
			}}
			p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, &fakeGitHub{}, ag, &fakeNotifier{})
			p.Memory = tc.mem
			wireInterjectionsForBaseline(t, p)
			_ = p.Execute(context.Background(), ExecuteParams{
				TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
			})
			if len(ag.calls) < 2 {
				t.Fatalf("应有分诊与实现两次调用，got=%d", len(ag.calls))
			}
			tk, err := m.Get(context.Background(), taskID)
			if err != nil || tk.BranchName == nil {
				t.Fatalf("读任务分支失败: %v", err)
			}
			want := ImplementPrompt(demoIssue().Context(), KindFix, *tk.BranchName)
			if got := ag.calls[1].Prompt; got != want {
				t.Errorf("实现 prompt 应逐字不变：\ngot=%q\nwant=%q", got, want)
			}
			if strings.Contains(ag.calls[1].Prompt, "【人追加的指示") {
				t.Errorf("无插话时不该出现插话段: %q", ag.calls[1].Prompt)
			}
		})
	}
}

// AC-7 红线：分诊直连 Agent.Run 时 Prompt 非空。
func TestTriageRunParamsCarryPromptAndNoStream(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)
	ag := &fakeAgent{results: []*agent.Result{
		{Success: true, Text: `{"actionable":false,"kind":"fix","reason":"不清楚","question":"要什么？"}`},
	}}
	p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, &fakeGitHub{}, ag, &fakeNotifier{})
	wireInterjectionsForBaseline(t, p)
	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("应只有分诊一次调用，got=%d", len(ag.calls))
	}
	if ag.calls[0].Prompt == "" {
		t.Error("分诊 RunParams.Prompt 不应为空")
	}
	assertTriageNotStreaming(t, ag.calls[0])
}

// AC-13 红线：既有 redactText 的 ghp_/sk-ant- 形态今天就成立；
// Bearer <20 位> 今天**不**被替换 —— 证明 Bearer 形态是插话专用新增的，
// 且全局 redactText 行为没被改动。
func TestRedactTextShapesBaseline(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		redacted bool
	}{
		{"ghp", "token ghp_" + strings.Repeat("a", 36), true},
		{"sk-ant", "key sk-ant-" + strings.Repeat("b", 20), true},
		{"Bearer不在全局形态表", "Authorization: Bearer " + strings.Repeat("c", 20), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactText(tc.in, nil, nil)
			if has := strings.Contains(got, redactedMark); has != tc.redacted {
				t.Errorf("redactText(%q) = %q，含标记 got=%v want=%v", tc.in, got, has, tc.redacted)
			}
		})
	}
}

// AC-9：验证落库不受插话影响。先写「无插话」那一半并落快照；改动后
// 加「implementing 期间插 3 条」的另一半，(tier, step, status) 序列必须相等。
func TestStageVerifyUnchangedByInterjections(t *testing.T) {
	run := func(t *testing.T, interject bool) []string {
		_, m, taskID, repo, src := pipelineFixture(t)
		gh := &fakeGitHub{pr: &github.PullRequest{Number: 1, URL: "https://x/pr/1"}}
		ag := &fakeAgent{
			results: []*agent.Result{
				{Success: true, Text: `{"actionable":true,"kind":"feature","reason":"r","question":""}`},
				{Success: true, NumTurns: 1, Text: "改了 README"},
			},
			mutate: []func(string) error{nil, func(dir string) error {
				writeFile(t, dir+"/README.md", "# x\n")
				return nil
			}},
		}
		verifs := &fakeVerifications{}
		p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, gh, ag, &fakeNotifier{})
		p.Verifications = verifs
		if interject {
			interjectDuringImplement(t, p, ag, taskID, 3)
		}
		if err := p.Execute(context.Background(), ExecuteParams{
			TaskID: taskID, Repo: repo, CloneURL: src, IssueRef: "uuid-777", Actor: "node:test",
		}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return verifs.rows
	}
	base := run(t, false)
	if len(base) == 0 {
		t.Fatal("无插话那一轮应有验证落库")
	}
	if !supportsInterjectionsForVerifyCompare() {
		t.Logf("快照（无插话）: %v", base)
		return
	}
	with := run(t, true)
	if strings.Join(base, ",") != strings.Join(with, ",") {
		t.Errorf("verifications 序列被插话改变：\nwithout=%v\nwith=%v", base, with)
	}
}
