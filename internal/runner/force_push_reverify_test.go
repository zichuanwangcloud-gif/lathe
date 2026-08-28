package runner

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Clouditera/lathe/internal/integration/github"
	"github.com/Clouditera/lathe/internal/task"
)

// ================================================================
// F4.4（前驱被改重验）：前驱分支 force-push 后，后继自动重验。
//
// 场景 S3（docs/07-prd-orchestration.md §2）：前驱 PR 还开着（没有
// 合并、没有被关闭），只是内容被 force-push 改写了——最常见的现实
// 触发源是"收到 review 意见后修改并强推"。这与 F4.3（前驱已合并）的
// 区别只在于触发信号（PR 仍 open + head 变化 vs. PR 已 merged）；一旦
// 判定成立，跟进动作完全复用 F4.3 的 rebaseFollowup，只是 oldBaseBranch
// 与 newBaseBranch 填成前驱自己的分支名（分支名没变，只是内容变了）。
// ================================================================

// capturingLogHandler 是一个极简 slog.Handler，把每条日志的 Message
// 连同关键属性拼成一行文本记下来，供测试断言"某条日志确实被打印过"。
//
// 用途：TestMergePollForcePushCacheTriggersRebaseFollowup 需要证明
// "head 没变不触发 / head 变了触发 rebaseFollowup"，而 rebaseFollowup
// 在没有真实 worktree 的后继任务上会走一条早退路径（不产生任何可在
// DB 里观察到的状态变化）——日志是这种情况下唯一可断言的观测点。
type capturingLogHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *capturingLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingLogHandler) Handle(_ context.Context, r slog.Record) error {
	var sb strings.Builder
	sb.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		sb.WriteString(" ")
		sb.WriteString(a.Key)
		sb.WriteString("=")
		sb.WriteString(a.Value.String())
		return true
	})
	h.mu.Lock()
	h.msgs = append(h.msgs, sb.String())
	h.mu.Unlock()
	return nil
}

func (h *capturingLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingLogHandler) WithGroup(string) slog.Handler      { return h }

func (h *capturingLogHandler) contains(sub string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

func (h *capturingLogHandler) reset() {
	h.mu.Lock()
	h.msgs = nil
	h.mu.Unlock()
}

// withCapturedLogs 把 slog 默认 logger 临时换成 capturingLogHandler，
// 测试结束后自动还原——本包其余测试都没有依赖过 slog 的具体输出，
// 临时替换默认 logger 不会影响它们（且本文件的用例都不调用
// t.Parallel，替换期间不会与其它测试并发执行）。
func withCapturedLogs(t *testing.T) *capturingLogHandler {
	t.Helper()
	h := &capturingLogHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

// ---------------------------------------------------------------
// 单测：lastSeenHead 缓存的三段判定逻辑
// ---------------------------------------------------------------

// TestMergePollForcePushCacheTriggersRebaseFollowup 验证 F4.4 检测的
// 缓存判定：第一次观察只记基线不触发；第二次观察 head 不变不触发；
// head 变了才触发 rebaseFollowup。
//
// task2（task1 的直接后继）故意不建 worktree（保持 queued），这样
// rebaseFollowup 一路查到它后会走"后继任务尚未开始，无需 rebase"的
// 早退路径——不需要真实 git 操作就能验证"检测→触发 rebaseFollowup→
// 找到正确的直接后继"这条链路真的接通了；是否触发靠日志断言（早退
// 路径不产生任何可在 DB 里观察到的状态变化）。
func TestMergePollForcePushCacheTriggersRebaseFollowup(t *testing.T) {
	log := withCapturedLogs(t)
	_, m, userID, repoID, repo := mergepollFixture(t)
	ctx := context.Background()

	task1, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "FP-1", LinearIssueID: "uuid-fp-1",
	})
	if err != nil {
		t.Fatalf("建 task1 失败: %v", err)
	}
	branch := "lathe/fp-1"
	if _, err := m.Transition(ctx, task1.ID, task.StateVerifying, "system",
		&task.TransitionOpts{BranchName: &branch}); err != nil {
		t.Fatalf("task1 转 verifying 失败: %v", err)
	}
	if _, err := m.Transition(ctx, task1.ID, task.StatePROpen, "system", nil); err != nil {
		t.Fatalf("task1 转 pr_open 失败: %v", err)
	}
	if err := m.SetPRNumber(ctx, task1.ID, 42); err != nil {
		t.Fatalf("设置 task1.pr_number 失败: %v", err)
	}

	task2, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "FP-2", LinearIssueID: "uuid-fp-2",
		DependsOn: &task1.ID,
	})
	if err != nil {
		t.Fatalf("建 task2 失败: %v", err)
	}
	if err := m.SetBaseRef(ctx, task2.ID, &branch); err != nil {
		t.Fatalf("设置 task2.base_ref 失败: %v", err)
	}
	// task2 故意留在 queued、不建 worktree —— 见函数注释。

	lin := newRebaseFollowupLinear()
	lin.add(rebaseFollowupIssue("uuid-fp-1", "FP-1"))
	lin.add(rebaseFollowupIssue("uuid-fp-2", "FP-2"))
	gh := &fakeGitHub{prInfo: &github.PRInfo{Number: 42, Merged: false, State: "open", HeadSHA: "sha-A"}}

	mp := &MergePoller{
		Tasks:         m,
		ClientFactory: fixedClientFactory{clients: &fakeClients{lin: lin, gh: gh}},
		RepoLookup:    repoLookupFixed(repo),
		Notifier:      &fakeNotifier{},
	}

	// ---- 第一轮：第一次观察到 task1，只记基线，不触发 ----
	if err := mp.pollOnce(ctx); err != nil {
		t.Fatalf("第一轮 pollOnce 失败: %v", err)
	}
	if log.contains("检测到前驱 PR 被 force-push") {
		t.Error("第一次观察到 head 不该触发 F4.4 rebase 跟进")
	}
	mp.lastSeenMu.Lock()
	got := mp.lastSeenHead[task1.ID]
	mp.lastSeenMu.Unlock()
	if got != "sha-A" {
		t.Fatalf("第一轮之后 lastSeenHead[task1] = %q，期望 %q", got, "sha-A")
	}

	// ---- 第二轮：head 未变化，不触发 ----
	log.reset()
	if err := mp.pollOnce(ctx); err != nil {
		t.Fatalf("第二轮 pollOnce 失败: %v", err)
	}
	if log.contains("检测到前驱 PR 被 force-push") {
		t.Error("head 未变化不该触发 F4.4 rebase 跟进")
	}

	// ---- 第三轮：head 变了（模拟前驱 force-push），应触发 rebaseFollowup
	// 并且确实找到 task2 这个直接后继 ----
	log.reset()
	gh.prInfo = &github.PRInfo{Number: 42, Merged: false, State: "open", HeadSHA: "sha-B"}
	if err := mp.pollOnce(ctx); err != nil {
		t.Fatalf("第三轮 pollOnce 失败: %v", err)
	}
	if !log.contains("检测到前驱 PR 被 force-push") {
		t.Error("head 变化后应触发 F4.4 rebase 跟进，但没有看到对应日志")
	}
	if !log.contains("后继任务尚未开始") {
		t.Error("rebaseFollowup 应该查到 task2 这个直接后继（走的是尚未建 worktree 的早退路径）")
	}
	mp.lastSeenMu.Lock()
	got = mp.lastSeenHead[task1.ID]
	mp.lastSeenMu.Unlock()
	if got != "sha-B" {
		t.Fatalf("第三轮之后 lastSeenHead[task1] = %q，期望 %q（应已更新为最新 head）", got, "sha-B")
	}

	// task2 本身没有被动过（早退路径不改状态）——顺带确认这一点，
	// 证明"触发"确实只是走到了 rebaseFollowupOne 而没有产生副作用。
	t2After, err := m.Get(ctx, task2.ID)
	if err != nil {
		t.Fatalf("读取 task2 失败: %v", err)
	}
	if t2After.State != task.StateQueued {
		t.Errorf("task2 应仍留在 queued（没有 worktree，早退不改状态），实际 state=%s", t2After.State)
	}
}

// ---------------------------------------------------------------
// 端到端：task1 的 PR 被真实 force-push 后，task2 自动 rebase + 重验
// ---------------------------------------------------------------

// TestMergePollForcePushCascadeReverifiesSuccessor 是 F4.4 的完整端到
// 端证明：task1（独立根）、task2（栈在 task1 上）都先跑到 pr_open；
// 然后在 task1 的 worktree 里手工新增一个提交并 force-push（模拟"收到
// review 意见后修改重推"），task1 的 PR 依旧 open，只是 head 变了；
// 触发两轮 pollOnce（第一轮建立基线，第二轮检测到变化）后，断言
// task2 被自动 rebase 到 task1 的新 head、diff 仍只含 task2 自己的
// 改动、task2 重验通过、且没有重跑 agent。
func TestMergePollForcePushCascadeReverifiesSuccessor(t *testing.T) {
	_, m, userID, repoID, repo, src := rebaseFollowupFixture(t)
	ctx := context.Background()

	task1, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "FPE-1", LinearIssueID: "uuid-fpe-1",
	})
	if err != nil {
		t.Fatalf("建 task1 失败: %v", err)
	}
	task2, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "FPE-2", LinearIssueID: "uuid-fpe-2",
		DependsOn: &task1.ID,
	})
	if err != nil {
		t.Fatalf("建 task2 失败: %v", err)
	}

	wm, err := NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatalf("建工作区管理器失败: %v", err)
	}
	lin := newRebaseFollowupLinear()
	lin.add(rebaseFollowupIssue("uuid-fpe-1", "FPE-1"))
	lin.add(rebaseFollowupIssue("uuid-fpe-2", "FPE-2"))
	gh := newSquashMergeGitHub(9700)
	clients := &fakeClients{lin: lin, gh: gh}

	ag := chainedAgent([][2]string{
		{"fileA.txt", "task1 的产出\n"},
		{"fileB.txt", "task2 的产出\n"},
	})

	verifs := &fakeVerifications{}
	pipe := &Pipeline{
		Tasks: m, Worktrees: wm, Verifier: NewVerifier(3*time.Minute, ""),
		Agent: ag, Clients: clients, Notifier: &fakeNotifier{},
		Verifications: verifs, PermissionMode: "acceptEdits", SettingSources: "project",
	}

	// ---- task1、task2 走完整调度到 pr_open ----
	if err := pipe.Execute(ctx, ExecuteParams{
		TaskID: task1.ID, Repo: repo, CloneURL: src, IssueID: "uuid-fpe-1", Actor: "node:test",
	}); err != nil {
		t.Fatalf("task1 Execute 失败: %v", err)
	}
	t1, err := m.Get(ctx, task1.ID)
	if err != nil || t1.State != task.StatePROpen {
		t.Fatalf("task1 应到达 pr_open，实际 state=%v err=%v", t1, err)
	}
	task1Branch := *t1.BranchName
	task1WorktreePath := *t1.WorktreePath
	if t1.PRNumber == nil {
		t.Fatal("task1 应落库 pr_number")
	}
	task1PRNumber := *t1.PRNumber
	task1OldHead := gitOut(t, task1WorktreePath, "rev-parse", "HEAD")

	if err := m.SetBaseRef(ctx, task2.ID, &task1Branch); err != nil {
		t.Fatalf("设置 task2.base_ref 失败: %v", err)
	}
	repo2 := repo
	repo2.BaseRefOverride = task1Branch
	if err := pipe.Execute(ctx, ExecuteParams{
		TaskID: task2.ID, Repo: repo2, CloneURL: src, IssueID: "uuid-fpe-2", Actor: "node:test",
	}); err != nil {
		t.Fatalf("task2 Execute 失败: %v", err)
	}
	t2, err := m.Get(ctx, task2.ID)
	if err != nil || t2.State != task.StatePROpen {
		t.Fatalf("task2 应到达 pr_open，实际 state=%v err=%v", t2, err)
	}
	task2WorktreePath := *t2.WorktreePath

	verifRowsBeforeReverify := len(verifs.rows)
	agentCallsBeforeReverify := len(ag.calls)

	mp := &MergePoller{
		Tasks: m, Worktrees: wm, Pipeline: pipe, Notifier: &fakeNotifier{},
		ClientFactory: fixedClientFactory{clients: clients},
		RepoLookup:    repoLookupFixed(repo),
	}

	// ---- 第一轮 pollOnce：task1 的 PR 仍 open，head 未变——只建立
	// lastSeenHead 基线，不触发任何跟进。----
	gh.setOpen(task1PRNumber, task1OldHead)
	if err := mp.pollOnce(ctx); err != nil {
		t.Fatalf("第一轮 pollOnce 失败: %v", err)
	}
	t2AfterFirstPoll, err := m.Get(ctx, task2.ID)
	if err != nil {
		t.Fatalf("读取 task2 失败: %v", err)
	}
	if t2AfterFirstPoll.State != task.StatePROpen {
		t.Fatalf("第一轮之后 task2 不该被动过（还没建立基线），实际 state=%s", t2AfterFirstPoll.State)
	}

	// ---- 手工在 task1 的 worktree 里新增一个提交并 force-push——模拟
	// "收到 review 意见后修改重推"：task1 的 PR 依旧 open，只是 head
	// 变了。----
	if err := os.WriteFile(task1WorktreePath+"/fileA.txt", []byte("task1 按 review 意见修改后的产出\n"), 0o644); err != nil {
		t.Fatalf("写 fileA.txt 失败: %v", err)
	}
	// 只 add 改动的这一个文件，不能用 "git add ."——task1 自己上一次
	// light 档验证（go build）会在它的 worktree 根目录留下一个未跟踪
	// 的编译产物（模块名 "demo"），"git add ." 会把它一并带进这次
	// force-push 的提交，污染 task1Branch 历史，进而在下面的 rebase
	// 里造成与本场景无关的假冲突。
	gitOut(t, task1WorktreePath, "add", "fileA.txt")
	gitOut(t, task1WorktreePath, "-c", "user.email=t@e.st", "-c", "user.name=t",
		"commit", "-qm", "按 review 意见修改")
	gitOut(t, task1WorktreePath, "push", "--force", "origin", "HEAD:"+task1Branch)
	task1NewHead := gitOut(t, task1WorktreePath, "rev-parse", "HEAD")
	if task1NewHead == task1OldHead {
		t.Fatal("force-push 之后 task1 的 head 应该变化")
	}

	// ---- 第二轮 pollOnce：head 与基线不一致，触发 F4.4 rebase 跟进 ----
	gh.setOpen(task1PRNumber, task1NewHead)
	if err := mp.pollOnce(ctx); err != nil {
		t.Fatalf("第二轮 pollOnce 失败: %v", err)
	}

	// ---- 断言：task1 本身仍是 pr_open（PR 没被合并、没被关闭，只是
	// 内容变了，F4.4 场景不改变前驱自己的状态）----
	t1After, err := m.Get(ctx, task1.ID)
	if err != nil {
		t.Fatalf("读取 task1 失败: %v", err)
	}
	if t1After.State != task.StatePROpen {
		t.Fatalf("task1 应仍是 pr_open，实际 state=%s", t1After.State)
	}

	// ---- 断言：task2 的分支历史被自动 rebase 到 task1 的新 head ----
	if !isAncestor(t, task2WorktreePath, task1NewHead, "HEAD") {
		t.Errorf("task2 的分支历史应以 task1 的新 head(%s) 为祖先——F4.4 rebase 跟进应已完成", task1NewHead[:8])
	}

	// ---- 断言：task2.base_ref 仍指向 task1 的分支（前驱自己没合并，
	// 不是 rebase 到 default_branch，不该清空）----
	t2AfterRebase, err := m.Get(ctx, task2.ID)
	if err != nil {
		t.Fatalf("读取 task2 失败: %v", err)
	}
	if t2AfterRebase.BaseRef == nil || *t2AfterRebase.BaseRef != task1Branch {
		t.Errorf("task2.base_ref 应仍指向 %q，实际 %v", task1Branch, t2AfterRebase.BaseRef)
	}

	// ---- 断言：diff 仍只含 task2 自己的改动（fileB.txt），不该带上
	// task1 修改后的 fileA.txt ----
	diffOut := gitOut(t, task2WorktreePath, "diff", MirrorBaseRef(task1Branch)+"...HEAD", "--name-only")
	gotFiles := map[string]bool{}
	for _, f := range splitNonEmptyLines(diffOut) {
		gotFiles[f] = true
	}
	if gotFiles["fileA.txt"] {
		t.Errorf("task2 的 diff 不应包含前驱 task1 的改动 fileA.txt，实际清单: %v", gotFiles)
	}
	if !gotFiles["fileB.txt"] || len(gotFiles) != 1 {
		t.Errorf("task2 的 diff 应恰好只有 fileB.txt，实际: %v", gotFiles)
	}

	// ---- 断言：task2 自动重验通过（pr_open），产生了新的验证记录 ----
	if t2AfterRebase.State != task.StatePROpen {
		t.Fatalf("task2 重验后终态 = %s，期望 pr_open", t2AfterRebase.State)
	}
	if len(verifs.rows) <= verifRowsBeforeReverify {
		t.Errorf("task2 的 F4.4 rebase 跟进重验应产生新的验证记录，重验前 %d 行，重验后 %d 行",
			verifRowsBeforeReverify, len(verifs.rows))
	}

	// ---- 断言：重验没有重跑 agent（F4.3 的机制原样复用，agent 调用
	// 次数不该增加）----
	if len(ag.calls) != agentCallsBeforeReverify {
		t.Errorf("F4.4 rebase 跟进的重验不应调用 agent，调用次数应仍是 %d，实际 %d",
			agentCallsBeforeReverify, len(ag.calls))
	}
}
