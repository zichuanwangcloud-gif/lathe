package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Clouditera/lathe/internal/integration/agent"
	"github.com/Clouditera/lathe/internal/integration/github"
	"github.com/Clouditera/lathe/internal/task"
)

// profileFixture 是 pipelineFixture 的变体：接受 profile 原始字节，
// 建独立的 user/repo/task，供 F7.1 的画像消费测试用——不复用
// pipelineFixture 本身，避免为了这几个测试改动既有测试共用的夹具。
func profileFixture(t *testing.T, issueKey string, profile []byte) (*task.Machine, int64, RepoConfig, string) {
	t.Helper()
	pool := testPoolForPipeline(t)
	m := task.NewMachine(pool)
	ctx := context.Background()

	var userID, repoID int64
	email := "pipe-profile-" + t.Name() + "-" + issueKey + "@example.com"
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email) VALUES ($1) ON CONFLICT (email) DO UPDATE SET updated_at=now() RETURNING id`,
		email).Scan(&userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2)
		 ON CONFLICT (user_id, provider_repo) DO UPDATE SET updated_at=now() RETURNING id`,
		userID, "acme/demo-profile").Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})

	tk, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: issueKey,
		Profile: profile,
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	return m, tk.ID, DefaultRepoConfig("acme/demo-profile"), goSourceRepo(t)
}

// implFixAgent 返回一套「分诊可行 + 实现产出 fix.go/main_test.go 改动」
// 的固定剧本，与 TestPipelineHappyPath 用的是同一套，抽出来给下面
// 几个 F7.1 测试复用，避免重复敲同一段 mutate 闭包。改动碰 .go 文件
// 且带复现测试，heavy 档能走完整红-绿证明。
func implFixAgent() *fakeAgent {
	return &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象和期望行为","question":""}`},
			{Success: true, Text: "补了 greet 函数与复现测试"},
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
}

// F7.1-AC2：per-node model_channel 生效，不同节点可路由到不同通道
// （沿用 B2-2 机制）——本测试跑两个独立节点，A 设了画像覆盖，B 没设，
// 断言二者实现阶段收到的 LATHE_AGENT_CHANNEL 确实不同：A 用画像值，
// B 落回 p.ImplementChannel（现有行为，即 F7.1-AC4 的另一半）。
func TestPipelineProfileModelChannelPerNode(t *testing.T) {
	// 节点 A：画像覆盖实现阶段通道
	mA, idA, repoA, srcA := profileFixture(t, "CR-PROF-A",
		[]byte(`{"model_channel":"channel-x"}`))
	linA := &fakeLinear{issue: demoIssue()}
	ghA := &fakeGitHub{pr: &github.PullRequest{Number: 1, URL: "https://github.com/acme/demo-profile/pull/1"}}
	agA := implFixAgent()
	pA := newPipeline(t, mA, linA, ghA, agA, &fakeNotifier{})
	pA.ImplementChannel = "fallback-channel"

	if err := pA.Execute(context.Background(), ExecuteParams{
		TaskID: idA, Repo: repoA, CloneURL: srcA, IssueID: "uuid-777", Actor: "test",
	}); err != nil {
		t.Fatalf("节点 A Execute 失败: %v", err)
	}
	if len(agA.calls) < 2 {
		t.Fatalf("节点 A 应至少有 2 次 agent 调用（分诊+实现），实际 %d", len(agA.calls))
	}
	implCallA := agA.calls[1]
	if len(implCallA.ExtraEnv) != 1 || implCallA.ExtraEnv[0] != "LATHE_AGENT_CHANNEL=channel-x" {
		t.Errorf("节点 A 实现阶段通道应为画像指定的 channel-x，实际: %v", implCallA.ExtraEnv)
	}

	// 节点 B：未设画像，应落回 pipeline 级 ImplementChannel
	mB, idB, repoB, srcB := profileFixture(t, "CR-PROF-B", nil)
	linB := &fakeLinear{issue: demoIssue()}
	ghB := &fakeGitHub{pr: &github.PullRequest{Number: 2, URL: "https://github.com/acme/demo-profile/pull/2"}}
	agB := implFixAgent()
	pB := newPipeline(t, mB, linB, ghB, agB, &fakeNotifier{})
	pB.ImplementChannel = "fallback-channel"

	if err := pB.Execute(context.Background(), ExecuteParams{
		TaskID: idB, Repo: repoB, CloneURL: srcB, IssueID: "uuid-777", Actor: "test",
	}); err != nil {
		t.Fatalf("节点 B Execute 失败: %v", err)
	}
	implCallB := agB.calls[1]
	if len(implCallB.ExtraEnv) != 1 || implCallB.ExtraEnv[0] != "LATHE_AGENT_CHANNEL=fallback-channel" {
		t.Errorf("节点 B 实现阶段通道应回落到 fallback-channel，实际: %v", implCallB.ExtraEnv)
	}

	// 分诊阶段两个节点都不该读画像 —— 分诊走便宜通道是既有设计意图。
	for name, calls := range map[string][]agent.RunParams{"A": agA.calls, "B": agB.calls} {
		if len(calls[0].ExtraEnv) != 0 {
			t.Errorf("节点 %s 分诊阶段不应受画像影响，实际 ExtraEnv=%v", name, calls[0].ExtraEnv)
		}
	}
}

// F7.1-AC3：per-node verify_tier 可覆盖自动定档，优先级高于自动分类
// 结果（即使改动面按 §5.1 规则应判 heavy，画像强制 light 时仍走 light）。
func TestPipelineProfileVerifyTierOverridesAutoClassification(t *testing.T) {
	m, taskID, repo, src := profileFixture(t, "CR-PROF-TIER", []byte(`{"verify_tier":"light"}`))

	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{pr: &github.PullRequest{Number: 3, URL: "https://github.com/acme/demo-profile/pull/3"}}
	ag := implFixAgent() // 产出一个 .go 改动：ClassifyTier 会判 heavy
	verifs := &fakeVerifications{}
	p := newPipeline(t, m, lin, gh, ag, &fakeNotifier{})
	p.Verifications = verifs

	if err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777", Actor: "test",
	}); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}

	final, err := m.Get(context.Background(), taskID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if final.VerifyTier == nil || *final.VerifyTier != "light" {
		t.Fatalf("VerifyTier = %v，期望画像强制的 light（尽管 .go 改动本会自动判 heavy）", final.VerifyTier)
	}
	if final.State != task.StatePROpen {
		t.Fatalf("终态 = %s，期望 pr_open", final.State)
	}

	// 验证步骤应落 light 档（build/lint），不应出现 heavy 档的
	// repro_fail/repro_pass/regression。
	for _, row := range verifs.rows {
		if strings.HasPrefix(row, "heavy/") {
			t.Errorf("画像强制 light 后仍出现 heavy 档验证步骤: %v", verifs.rows)
		}
	}
	hasLightBuild := false
	for _, row := range verifs.rows {
		if strings.HasPrefix(row, "light/build/") {
			hasLightBuild = true
		}
	}
	if !hasLightBuild {
		t.Errorf("应落 light 档 build 步骤，实际: %v", verifs.rows)
	}

	// 事件流里定档理由应说明"节点画像强制指定"
	events, _ := m.Events(context.Background(), taskID)
	found := false
	for _, e := range events {
		if e.ToState != task.StateVerifying {
			continue
		}
		if reasons, ok := e.Payload["tier_reasons"].([]any); ok {
			for _, r := range reasons {
				if s, ok := r.(string); ok && strings.Contains(s, "节点画像") {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("verifying 事件的 tier_reasons 应说明是节点画像强制指定，事件: %+v", events)
	}
}

// F7.1：画像数据损坏或字段非法（这里是非法的 verify_tier 取值）时，
// 任务应以可读原因失败，不静默忽略、也不能把非法值带进 stageVerify
// 后再离奇地炸掉。
func TestPipelineProfileInvalidFailsTaskWithReadableReason(t *testing.T) {
	m, taskID, repo, src := profileFixture(t, "CR-PROF-BAD", []byte(`{"verify_tier":"medium"}`))

	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{}
	ag := implFixAgent()
	no := &fakeNotifier{}
	p := newPipeline(t, m, lin, gh, ag, no)

	err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777", Actor: "test",
	})
	if err == nil {
		t.Fatal("非法画像应导致 Execute 返回错误")
	}

	final, gerr := m.Get(context.Background(), taskID)
	if gerr != nil {
		t.Fatalf("Get 失败: %v", gerr)
	}
	if final.State != task.StateFailed {
		t.Fatalf("终态 = %s，期望 failed", final.State)
	}
	if final.FailureStage == nil || *final.FailureStage != string(StageProfileInvalid) {
		t.Errorf("FailureStage = %v，期望 %q", final.FailureStage, StageProfileInvalid)
	}
	if final.FailureReason == nil || !strings.Contains(*final.FailureReason, "verify_tier") {
		t.Errorf("FailureReason 应可读地提及 verify_tier，实际: %v", final.FailureReason)
	}

	if len(lin.comments) == 0 || !strings.Contains(lin.comments[len(lin.comments)-1], "verify_tier") {
		t.Errorf("失败回帖应包含可读原因，实际: %v", lin.comments)
	}
}
