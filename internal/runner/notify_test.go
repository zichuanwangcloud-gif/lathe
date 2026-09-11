package runner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Clouditera/lathe/internal/integration/agent"
	"github.com/Clouditera/lathe/internal/integration/github"
	"github.com/Clouditera/lathe/internal/task"
)

// fakeMail 记录投递出去的信；err 非空时模拟发信失败。
//
// 加锁是必需的，不是防御性编程：mailTerminal 现在是【异步】投递
// （见 notify.go 的注释），SendTaskMail 在独立 goroutine 里跑，
// 测试里的读取与它并发。没有互斥的话 -race 会直接报数据竞争。
type fakeMail struct {
	mu   sync.Mutex
	sent []struct {
		TaskID        int64
		Subject, Body string
	}
	err error
}

func (f *fakeMail) SendTaskMail(ctx context.Context, taskID int64, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, struct {
		TaskID        int64
		Subject, Body string
	}{taskID, subject, body})
	return f.err
}

// sentMail 返回已投递信件的快照（拷贝），供断言安全读取。
func (f *fakeMail) sentMail() []struct {
	TaskID        int64
	Subject, Body string
} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]struct {
		TaskID        int64
		Subject, Body string
	}(nil), f.sent...)
}

// waitSent 等到至少 n 封信投递出去，或超时。
//
// 发信改异步之后必须这么等：直接断言会撞上「goroutine 还没来得及跑」，
// 而且这种失败是间歇性的——最坏的一种测试。testMailWait 刻意给得比
// 任何真实断言需要的都宽，它只在真出问题时才耗尽。
func (f *fakeMail) waitSent(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(testMailWait)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		got := len(f.sent)
		f.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待 %d 封通知超时（%s），实际只投递了 %d 封", n, testMailWait, len(f.sentMail()))
}

// testMailWait 是异步投递在测试里的等待上限。
const testMailWait = 5 * time.Second

// 正文渲染是纯函数，能脱离 SMTP 断言「该带的东西都带上了」
// （docs/08-debt-cleanup.md T3 AC4/AC5）。
func TestTerminalMailRendersEssentials(t *testing.T) {
	reason := "验证未通过: 回归测试挂了"
	stage := "verify_failed"
	tk := &task.Task{
		ID: 4242, LinearIssueKey: "CR-999", State: task.StateFailed,
		FailureReason: &reason, FailureStage: &stage,
	}

	subject, body := terminalMail("https://lathe.example.com/", tk, "工作区已保留在 /w/cr-999")

	// 主题里要有 issue key —— 人的收件箱按这个扫
	if !strings.Contains(subject, "CR-999") {
		t.Errorf("主题应含 issue key，得到 %q", subject)
	}
	for _, want := range []string{
		"CR-999",                               // issue key
		"处理失败",                                 // 终态
		reason,                                 // 失败原因
		stage,                                  // 失败分类/阶段
		"工作区已保留在 /w/cr-999",                    // 该状态特有的补充信息
		"https://lathe.example.com/tasks/4242", // 详情页链接（BaseURL 拼，尾斜杠已规整）
	} {
		if !strings.Contains(body, want) {
			t.Errorf("正文缺 %q\n实际正文：\n%s", want, body)
		}
	}
}

// BaseURL 为空时省略链接那一行，而不是拼出一个指向 localhost 的无用链接。
func TestTerminalMailOmitsLinkWithoutBaseURL(t *testing.T) {
	tk := &task.Task{ID: 7, LinearIssueKey: "CR-7", State: task.StatePROpen}
	_, body := terminalMail("", tk, "")
	if strings.Contains(body, "详情") {
		t.Errorf("BaseURL 为空时不该出现详情链接，实际正文：\n%s", body)
	}
	if !strings.Contains(body, "CR-7") {
		t.Errorf("正文仍应含 issue key，实际：\n%s", body)
	}
}

// AC2/AC3：发信失败只记日志，不 panic、不返回错误、不改任何状态。
//
// mailTerminal 刻意不返回 error —— 调用方都在「任务已进终态」之后调它，
// 返回错误只会诱导调用方去处理一个不该影响主流程的东西。
func TestMailTerminalSwallowsSendFailure(t *testing.T) {
	fm := &fakeMail{err: errors.New("smtp 连不上")}
	p := &Pipeline{Mail: fm, BaseURL: "https://x.example"}
	tk := &task.Task{ID: 1, LinearIssueKey: "CR-1", State: task.StateFailed}

	// 不 panic、不返回值即通过
	p.mailTerminal(context.Background(), tk, "")

	fm.waitSent(t, 1)
	if got := len(fm.sentMail()); got != 1 {
		t.Fatalf("应尝试投递一次，实际 %d 次", got)
	}
}

// Mail 为 nil（未装配邮件）时整个机制静默关闭，不 panic。
func TestMailTerminalNoopWithoutMailer(t *testing.T) {
	p := &Pipeline{}
	p.mailTerminal(context.Background(), &task.Task{ID: 1, State: task.StateFailed}, "")
	// 走到这里没 panic 即通过
}

// 各终态的主题都得是人话，不能漏成裸状态名。
func TestStateSubjectCoversTerminalStates(t *testing.T) {
	for _, s := range []task.State{
		task.StateFailed, task.StatePROpen, task.StateAwaitingApproval,
		task.StateMerged, task.StateCancelled, task.StateBlockedSpec,
	} {
		got := stateSubject(s)
		if got == string(s) {
			t.Errorf("状态 %s 没有人读的主题文案，回退成了裸状态名", s)
		}
	}
}

// AC1：任务进 failed 时确实发了信，且信里认得出是哪个任务。
// AC2：SMTP 挂掉不影响状态流转 —— 这是本项最重要的一条断言。
//
// 通知是副作用，不是流程的一部分。把它做成能让任务卡住的东西，
// 等于用一个「锦上添花」的功能给主流程加了一个新的失败点。
func TestPipelineFailureNotifiesOwnerAndSurvivesSMTPOutage(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)
	ctx := context.Background()

	lin := &fakeLinear{issue: demoIssue()}
	gh := &fakeGitHub{}
	// agent 不产出任何改动 → 实现阶段判失败，走 fail() 三件套
	ag := &fakeAgent{results: []*agent.Result{
		{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象","question":""}`},
		{Success: true, Text: "什么都没改"},
	}}
	fm := &fakeMail{err: errors.New("smtp 连不上：模拟邮件服务故障")}

	p := newPipeline(t, m, lin, gh, ag, &fakeNotifier{})
	p.Mail = fm
	p.BaseURL = "https://lathe.example.com"
	p.SettingSources = "project"

	// 失败是预期结果，Execute 返回错误也是预期的
	_ = p.Execute(ctx, ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777", Actor: "node:test",
	})

	final, err := m.Get(ctx, taskID)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	// AC2 的核心：SMTP 全程报错，任务依然干净地落在 failed
	if final.State != task.StateFailed {
		t.Fatalf("AC2：SMTP 故障不该影响状态流转，期望 failed，得到 %s", final.State)
	}

	// AC1：确实尝试投递了，且带的是这个任务
	fm.waitSent(t, 1)
	sent := fm.sentMail()
	if len(sent) == 0 {
		t.Fatal("AC1：任务进 failed 应发终态通知，实际一封都没发")
	}
	last := sent[len(sent)-1]
	if last.TaskID != taskID {
		t.Errorf("发信带的 taskID = %d，期望 %d", last.TaskID, taskID)
	}
	if !strings.Contains(last.Subject, "CR-777") {
		t.Errorf("主题应含 issue key，得到 %q", last.Subject)
	}
	// 失败原因要进正文 —— 不然人还得点开面板才知道为什么挂
	if !strings.Contains(last.Body, "失败原因") {
		t.Errorf("正文应含失败原因，实际：\n%s", last.Body)
	}
}

// AC1：gate_mode=manual 停在 awaiting_approval 时必须发信。
//
// 这一条的价值最高：任务停在闸门上不动，除了等人别的什么都不会发生。
// 不发信就只能靠人主动去翻面板才发现「活早就干完了」。
func TestPipelineManualGateNotifiesOwner(t *testing.T) {
	pool, m, taskID, repo, src := pipelineFixture(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE tasks SET gate_mode='manual' WHERE id=$1`, taskID); err != nil {
		t.Fatalf("设置 gate_mode 失败: %v", err)
	}

	fm := &fakeMail{}
	p := newPipeline(t, m, &fakeLinear{issue: demoIssue()},
		&fakeGitHub{pr: nil}, manualGateAgent(), &fakeNotifier{})
	p.Mail = fm
	p.BaseURL = "https://lathe.example.com"
	p.SettingSources = "project"

	if err := p.Execute(ctx, ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777", Actor: "node:test",
	}); err != nil {
		t.Fatalf("闸门停机是正常终止: %v", err)
	}

	fm.waitSent(t, 1)
	sent := fm.sentMail()
	if len(sent) == 0 {
		t.Fatal("停在 awaiting_approval 应发通知，实际一封都没发")
	}
	last := sent[len(sent)-1]
	if !strings.Contains(last.Subject, "等你放行") {
		t.Errorf("主题应说明是在等放行，得到 %q", last.Subject)
	}
	if !strings.Contains(last.Body, "确认开 PR") {
		t.Errorf("正文应告诉人去哪儿点什么，实际：\n%s", last.Body)
	}
}

// AC1：走到 pr_open 时发信（活干完了，等人评审）。
func TestPipelinePROpenNotifiesOwner(t *testing.T) {
	_, m, taskID, repo, src := pipelineFixture(t)
	ctx := context.Background()

	gh := &fakeGitHub{pr: &github.PullRequest{Number: 42, URL: "https://github.com/acme/demo/pull/42"}}
	fm := &fakeMail{}
	p := newPipeline(t, m, &fakeLinear{issue: demoIssue()}, gh, manualGateAgent(), &fakeNotifier{})
	p.Mail = fm
	p.BaseURL = "https://lathe.example.com"
	p.SettingSources = "project"

	if err := p.Execute(ctx, ExecuteParams{
		TaskID: taskID, Repo: repo, CloneURL: src, IssueID: "uuid-777", Actor: "node:test",
	}); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}

	fm.waitSent(t, 1)
	sent := fm.sentMail()
	if len(sent) == 0 {
		t.Fatal("走到 pr_open 应发通知")
	}
	last := sent[len(sent)-1]
	if !strings.Contains(last.Body, "https://github.com/acme/demo/pull/42") {
		t.Errorf("正文应含 PR 地址，实际：\n%s", last.Body)
	}
}
