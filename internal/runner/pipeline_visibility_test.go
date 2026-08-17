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

// streamingAgent 包一层 fakeAgent：每次 Run 先通过 OnEvent 吐一条事件，
// 模拟 claude CLI 的 stream-json 输出，验证 pipeline 把事件接进了 sink。
type streamingAgent struct {
	inner *fakeAgent
}

var errNoOnEvent = errors.New("pipeline 应给每次 Run 传 OnEvent")

func (s *streamingAgent) Run(ctx context.Context, p agent.RunParams) (*agent.Result, error) {
	if p.OnEvent == nil {
		return nil, errNoOnEvent
	}
	p.OnEvent(assistantTextEvent("工作中"))
	return s.inner.Run(ctx, p)
}

// 可见性接线 · 成功路径：triage/implement 事件按阶段落库、摘要落库、
// verify_step 与 verifications 表同源（docs/04 §3.2/§3.5）。
func TestPipelineAgentVisibility(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{pr: &github.PullRequest{Number: 42, URL: "https://github.com/acme/demo/pull/42"}}
	ag := &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象和期望行为","question":""}`},
			{Success: true, Text: "补了 greet 函数与复现测试", CostUSD: 0.12, DurationMS: 34000, NumTurns: 9},
		},
		mutate: []func(string) error{
			nil,
			func(dir string) error {
				if err := os.WriteFile(filepath.Join(dir, "main_test.go"),
					[]byte("package main\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif greet() != \"hello\" {\n\t\tt.Fatalf(\"got %q\", greet())\n\t}\n}\n"), 0o644); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(dir, "fix.go"),
					[]byte("package main\n\nfunc greet() string { return \"hello\" }\n"), 0o644)
			},
		},
	}
	rec := &fakeEventRecorder{}
	p := newPipeline(t, m, lin, gh, &streamingAgent{inner: ag}, &fakeNotifier{})
	p.AgentEvents = rec

	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777", Actor: "node:test",
	}); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}

	// 事件按阶段落库：triage / implement 各一条流式事件，
	// verify 阶段是 verify_step（与 verifications 表同源）
	byPhase := map[string]int{}
	verifySteps := 0
	for _, r := range rec.collected() {
		byPhase[r.phase]++
		if r.entry.Kind == "verify_step" {
			verifySteps++
			if r.phase != "verify" {
				t.Errorf("verify_step 应落在 verify 阶段，得到 %s", r.phase)
			}
			if r.entry.Payload["tier"] != "heavy" || r.entry.Payload["status"] == nil {
				t.Errorf("verify_step 缺 tier/status: %+v", r.entry.Payload)
			}
		}
	}
	if byPhase["triage"] != 1 || byPhase["implement"] != 1 {
		t.Errorf("triage/implement 应各落 1 条事件，实际分布: %v", byPhase)
	}
	if verifySteps == 0 {
		t.Error("缺 verify_step 事件（应与 verifications 同源落 agent_events）")
	}

	// 摘要落库：实现阶段的 Result.Text
	if len(rec.summaries) != 1 || rec.summaries[0] != "补了 greet 函数与复现测试" {
		t.Errorf("摘要落库不符: %v", rec.summaries)
	}

	// 接线不该改变主链路行为
	final, err := m.Get(context.Background(), taskID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if final.State != task.StatePROpen {
		t.Errorf("终态 = %s，期望 pr_open", final.State)
	}
}

// 可见性接线 · 失败路径：agent 没产出改动时，实现阶段的事件与摘要
// 仍然落库 —— fail 路径的缓冲恰是排障最关键的现场（docs/04 §3.2）。
func TestPipelineAgentVisibilityFailPath(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{}
	ag := &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"ok","question":""}`},
			{Success: true, Text: "我分析了半天，没改代码", CostUSD: 0.03, DurationMS: 5000, NumTurns: 3},
		},
		// mutate 全 nil：实现阶段不产生任何改动 → 走 fail
	}
	rec := &fakeEventRecorder{}
	p := newPipeline(t, m, lin, gh, &streamingAgent{inner: ag}, &fakeNotifier{})
	p.AgentEvents = rec

	err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777", Actor: "node:test",
	})
	if err == nil {
		t.Fatal("无改动应走失败路径")
	}

	final, _ := m.Get(context.Background(), taskID)
	if final.State != task.StateFailed {
		t.Errorf("终态 = %s，期望 failed", final.State)
	}

	// 失败也要留下现场：triage + implement 两段事件都在
	byPhase := map[string]int{}
	for _, r := range rec.collected() {
		byPhase[r.phase]++
	}
	if byPhase["triage"] != 1 || byPhase["implement"] != 1 {
		t.Errorf("失败路径应落 triage+implement 两段事件，实际分布: %v", byPhase)
	}

	// 有 result 就存摘要，与成败无关
	if len(rec.summaries) != 1 || !strings.Contains(rec.summaries[0], "没改代码") {
		t.Errorf("失败路径的摘要也应落库: %v", rec.summaries)
	}
}
