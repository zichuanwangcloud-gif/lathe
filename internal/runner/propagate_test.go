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

// ---------------------------------------------------------------- F2.3 失败传播

// TestPipelineFailurePropagatesBlockedDep 造一条 task1→task2→task3 的链
// （2 依赖 1，3 依赖 2，均为默认 depends_on_at=pr_open）。task1 在最早的
// StageFetchIssue 就失败（最省成本地触发 fail()），断言：
//   - task1 转 failed；
//   - task2、task3（此前都是 queued）被传播为 blocked_dep（覆盖间接依赖）；
//   - task2、task3 各自的 Linear issue 都收到回帖，且内容能看出是被
//     task1（及其 issue key）连累的。
func TestPipelineFailurePropagatesBlockedDep(t *testing.T) {
	_, m, task1ID, repo, src := pipelineFixture(t)
	ctx := context.Background()

	tk1, err := m.Get(ctx, task1ID)
	if err != nil {
		t.Fatalf("读取 task1 失败: %v", err)
	}

	task2, err := m.Create(ctx, task.CreateParams{
		UserID: tk1.UserID, RepoID: tk1.RepoID,
		LinearIssueKey: "CR-778", LinearIssueID: "issue-778",
		DependsOn: &task1ID,
	})
	if err != nil {
		t.Fatalf("建 task2 失败: %v", err)
	}
	task3, err := m.Create(ctx, task.CreateParams{
		UserID: tk1.UserID, RepoID: tk1.RepoID,
		LinearIssueKey: "CR-779", LinearIssueID: "issue-779",
		DependsOn: &task2.ID,
	})
	if err != nil {
		t.Fatalf("建 task3 失败: %v", err)
	}

	lin := &fakeLinear{issueErr: errors.New("401 unauthorized")}
	gh := &fakeGitHub{}
	ag := &fakeAgent{}
	no := &fakeNotifier{}
	p := newPipeline(t, m, lin, gh, ag, no)

	err = p.Execute(ctx, ExecuteParams{
		TaskID: task1ID, Repo: repo, CloneURL: src, IssueID: "uuid-777",
	})
	if err == nil {
		t.Fatal("task1 拉 issue 失败应返回错误")
	}

	final1, gerr := m.Get(ctx, task1ID)
	if gerr != nil {
		t.Fatalf("读取 task1 失败: %v", gerr)
	}
	if final1.State != task.StateFailed {
		t.Fatalf("task1 状态 = %s，期望 failed", final1.State)
	}

	final2, gerr := m.Get(ctx, task2.ID)
	if gerr != nil {
		t.Fatalf("读取 task2 失败: %v", gerr)
	}
	if final2.State != task.StateBlockedDep {
		t.Errorf("task2 状态 = %s，期望 blocked_dep（直接后继）", final2.State)
	}

	final3, gerr := m.Get(ctx, task3.ID)
	if gerr != nil {
		t.Fatalf("读取 task3 失败: %v", gerr)
	}
	if final3.State != task.StateBlockedDep {
		t.Errorf("task3 状态 = %s，期望 blocked_dep（传递后继）", final3.State)
	}

	// 两个 issue 都要收到回帖，且正文能看出是被 task1/CR-777 连累的
	seen := map[string]string{}
	for i, issueID := range lin.commentIssueIDs {
		if issueID == "issue-778" || issueID == "issue-779" {
			seen[issueID] = lin.comments[i]
		}
	}
	for _, issueID := range []string{"issue-778", "issue-779"} {
		body, ok := seen[issueID]
		if !ok {
			t.Errorf("issue %s 未收到阻塞回帖，实际回帖: %v (issues=%v)", issueID, lin.comments, lin.commentIssueIDs)
			continue
		}
		if !strings.Contains(body, "CR-777") {
			t.Errorf("issue %s 的回帖内容未指明是被 task1/CR-777 连累: %s", issueID, body)
		}
	}
}

// TestPipelineFailurePropagationNoopForRoot 是回归：独立根任务（无
// depends_on、无后继）失败时，PropagateBlocked 应是无操作 —— 不panic、
// 不多回帖、任务本身仍正常转 failed。
func TestPipelineFailurePropagationNoopForRoot(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)

	lin := &fakeLinear{issueErr: errors.New("401 unauthorized")}
	gh := &fakeGitHub{}
	ag := &fakeAgent{}
	no := &fakeNotifier{}
	p := newPipeline(t, m, lin, gh, ag, no)

	err := p.Execute(context.Background(), ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777",
	})
	if err == nil {
		t.Fatal("拉 issue 失败应返回错误")
	}

	final, _ := m.Get(context.Background(), taskID)
	if final.State != task.StateFailed {
		t.Errorf("状态 = %s，期望 failed", final.State)
	}
	// 独立根任务没有后继：只有它自己失败的那 1 条回帖。
	if len(lin.comments) != 1 {
		t.Errorf("独立任务失败传播应无操作（不多产生回帖），实际: %v", lin.comments)
	}
}

// TestPipelineSuccessWakesBlockedSuccessor 覆盖 F2.3-AC5：task1 成功跑到
// pr_open 后，应把此前因故被 blocked_dep 的直接后继 task2 唤醒回 queued。
func TestPipelineSuccessWakesBlockedSuccessor(t *testing.T) {
	_, m, task1ID, repo, src := pipelineFixture(t)
	ctx := context.Background()

	tk1, err := m.Get(ctx, task1ID)
	if err != nil {
		t.Fatalf("读取 task1 失败: %v", err)
	}

	task2, err := m.Create(ctx, task.CreateParams{
		UserID: tk1.UserID, RepoID: tk1.RepoID,
		LinearIssueKey: "CR-780", LinearIssueID: "issue-780",
		DependsOn: &task1ID,
	})
	if err != nil {
		t.Fatalf("建 task2 失败: %v", err)
	}
	// 手工把 task2 置为 blocked_dep（模拟"此前因为别的原因已被阻塞"，
	// 不要求一定是同一次失败传播的产物 —— 唤醒逻辑本身不关心成因）。
	if _, err := m.Transition(ctx, task2.ID, task.StateBlockedDep, "test", nil); err != nil {
		t.Fatalf("预置 task2 为 blocked_dep 失败: %v", err)
	}

	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{pr: &github.PullRequest{Number: 1, URL: "https://github.com/acme/demo/pull/1"}}
	ag := &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象和期望行为","question":""}`},
			{Success: true, Text: "补了 greet 函数与复现测试"},
		},
		mutate: []func(string) error{
			nil, // 分诊不改文件
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
	no := &fakeNotifier{}
	verifs := &fakeVerifications{}
	p := newPipeline(t, m, lin, gh, ag, no)
	p.Verifications = verifs
	p.SettingSources = "project"

	if err := p.Execute(ctx, ExecuteParams{
		TaskID: task1ID, Repo: repo, CloneURL: src, IssueID: "uuid-777", Actor: "node:test",
	}); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}

	final1, err := m.Get(ctx, task1ID)
	if err != nil {
		t.Fatalf("读取 task1 失败: %v", err)
	}
	if final1.State != task.StatePROpen {
		t.Fatalf("task1 状态 = %s，期望 pr_open", final1.State)
	}

	final2, err := m.Get(ctx, task2.ID)
	if err != nil {
		t.Fatalf("读取 task2 失败: %v", err)
	}
	if final2.State != task.StateQueued {
		t.Errorf("task2 状态 = %s，期望被唤醒回 queued", final2.State)
	}
}
