package runner

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Clouditera/lathe/internal/integration/github"
	"github.com/Clouditera/lathe/internal/task"
)

// ================================================================
// docs/07-prd-orchestration.md §5 · M3 出口条件端到端集成测试
//
//	"把 1 以 squash 合并，2 自动 rebase 到 default、PR 自动 retarget、
//	 diff 仍只含 2 的改动、自动重验通过；前驱分支在后继未合并前不被
//	 删除"
//
// 场景对应 §2 的 S4："前驱以 squash 方式合并"：squash 合并的本质特征
// 是"前驱分支的提交被压缩成一个新提交直接加到 default_branch 上，
// 前驱分支自己的历史保持不变"——祖先关系因此断裂，收敛靠 F4.3 的自动
// rebase 跟进。
//
// 本文件只消费前面阶段已经交付的东西，不重新实现任何生产逻辑：
//   - MergePoller（mergepoll.go）——合并检测（F4.1）+ rebase 级联
//     （F4.3）+ 安全回收（F4.2）全部接好了，本文件只调用
//     MergePoller.pollOnce，不直接调用 rebaseFollowup（那是
//     rebase_followup_test.go 已经覆盖的更底层单元）。
//   - task.Machine 的 ListOpenPRTasks/HasLiveDependentOnBranch/
//     TasksWithBaseRef/SetPRNumber。
//   - WorktreeManager.RebaseOnto/ForcePush。
//   - 真实 Pipeline.Execute + 真实 git 操作（本地仓库当 origin）。
//
// 与 rebase_followup_test.go 的关键差异：那份测试直接调用
// mp.rebaseFollowup(...)（绕过合并检测本身），且用 cherry-pick 模拟
// "dev 拿到了前驱的内容"。本文件走的是更上层、更贴近生产的入口——
// MergePoller.pollOnce（真正触发 F4.1 合并检测判定 + F4.2 回收时机
// 判定 + F4.3 级联的那个函数），且明确用 git merge --squash 模拟
// squash 合并，直接对应 M3 出口条件原文的措辞。
// ================================================================

// squashMergeGitHub 是本文件专用的 GitHubAPI 假件，支持"按 PR 编号
// 分别配置 GetPRInfo 返回值"——pipeline_test.go 的 fakeGitHub 只有单个
// 固定 prInfo 字段，不管查询的是哪个 PR 编号都返回同一个值，无法在
// 同一轮 MergePoller.pollOnce 里让 task1 的 PR 报"已合并"而 task2 的
// PR 仍报"open"（这一轮里两者都在 pr_open，都会被查询到）。这里新增
// 一个本文件私有的假件类型来支持这个场景，不改动 fakeGitHub 的既有
// 默认行为（其余测试文件继续用它，行为不受影响）。
//
// CreatePR 每次调用分配一个递增的新 PR 编号（而不是像 fakeGitHub/
// rebaseFollowupGitHub 那样固定返回同一个 PR 对象）：task1、task2 的
// 初次开 PR，以及 task2 rebase 跟进重验后再开一次 PR，三次调用必须
// 编号互不相同，GetPRInfo 才能按编号精确区分"已合并的是哪一个"。
type squashMergeGitHub struct {
	params  []github.PRParams
	nextNum int
	// infos 配置每个 PR 编号应返回的状态；未配置的编号默认认为仍是
	// open 未合并——这是 GitHub 真实 PR 在没有人为改动之前的默认状态。
	infos map[int]*github.PRInfo
}

func newSquashMergeGitHub(startNumber int) *squashMergeGitHub {
	return &squashMergeGitHub{nextNum: startNumber, infos: map[int]*github.PRInfo{}}
}

func (f *squashMergeGitHub) CreatePR(ctx context.Context, p github.PRParams) (*github.PullRequest, error) {
	f.params = append(f.params, p)
	n := f.nextNum
	f.nextNum++
	return &github.PullRequest{Number: n, URL: fmt.Sprintf("https://github.com/acme/m3-squash/pull/%d", n)}, nil
}

func (f *squashMergeGitHub) GetPRInfo(ctx context.Context, providerRepo string, number int) (*github.PRInfo, error) {
	if info, ok := f.infos[number]; ok {
		return info, nil
	}
	return &github.PRInfo{Number: number, State: "open", Merged: false}, nil
}

// setMerged 配置某个 PR 编号在后续 GetPRInfo 查询里报"已合并"——这是
// 任务说明要求的"配置 fakeGitHub，让针对 task1 的 PR 号，GetPRInfo
// 返回 Merged=true"在本文件的具体落地。
func (f *squashMergeGitHub) setMerged(number int) {
	f.infos[number] = &github.PRInfo{Number: number, State: "closed", Merged: true}
}

// setOpen 配置某个 PR 编号在后续 GetPRInfo 查询里报"仍 open"且带上
// 指定的 head SHA——F4.4（前驱被改重验）测试专用：模拟 PR 未合并、
// 未关闭，只是被 force-push 改了内容（head 变了）。
func (f *squashMergeGitHub) setOpen(number int, headSHA string) {
	f.infos[number] = &github.PRInfo{Number: number, State: "open", Merged: false, HeadSHA: headSHA}
}

// TestM3SquashMergeCascadeRebasesRetargetsAndKeepsDiffClean 是 M3 出口
// 条件（docs/07-prd-orchestration.md §5 表格）的直接端到端证明。
func TestM3SquashMergeCascadeRebasesRetargetsAndKeepsDiffClean(t *testing.T) {
	// 复用 rebase_followup_test.go 的夹具：独立 user/repo + 可构建源仓库
	// （本地目录当 origin，真实 git 操作，不 fake 任何 git 层面的东西）。
	_, m, userID, repoID, repo, src := rebaseFollowupFixture(t)
	ctx := context.Background()

	// ---- 建图：task1（独立根）、task2（depends_on=task1，栈式 PR） ----
	task1, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "M3-1", LinearIssueID: "uuid-m3-1",
	})
	if err != nil {
		t.Fatalf("建 task1 失败: %v", err)
	}
	task2, err := m.Create(ctx, task.CreateParams{
		UserID: userID, RepoID: repoID, LinearIssueKey: "M3-2", LinearIssueID: "uuid-m3-2",
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
	lin.add(rebaseFollowupIssue("uuid-m3-1", "M3-1"))
	lin.add(rebaseFollowupIssue("uuid-m3-2", "M3-2"))
	gh := newSquashMergeGitHub(9000)
	clients := &fakeClients{lin: lin, gh: gh}

	// task1 的实现改 fileA.txt，task2 的实现改 fileB.txt——诚如任务说明
	// 指定，这两个文件互不相干，"diff 仍只含 2 的改动"这条断言才有意义。
	ag := chainedAgent([][2]string{
		{"fileA.txt", "task1 的产出\n"},
		{"fileB.txt", "task2 的产出\n"},
	})

	// 唯一一份 Pipeline，贯穿 task1/task2 的初次执行【与】后面合并检测
	// 触发的 rebase 跟进重验——跟生产环境的形态一致（MergePoller.Pipeline
	// 是同一份共享 Pipeline），与 rebase_followup_test.go 同一手法。
	verifs := &fakeVerifications{}
	pipe := &Pipeline{
		Tasks: m, Worktrees: wm, Verifier: NewVerifier(3*time.Minute, ""),
		Agent: ag, Clients: clients, Notifier: &fakeNotifier{},
		Verifications: verifs, PermissionMode: "acceptEdits", SettingSources: "project",
	}

	// ---- 步骤 1：task1、task2 走完整调度到 pr_open ----

	// task1：独立根，正常从 dev 分叉。
	if err := pipe.Execute(ctx, ExecuteParams{
		TaskID: task1.ID, Repo: repo, CloneURL: src, IssueID: "uuid-m3-1", Actor: "node:test",
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
	// 必须在任何改写发生之前捕获（下面即将模拟的"改写"是 dev 的
	// squash 合并）——task1 自己此刻还没被任何人动过，直接读它现在的
	// HEAD 就是它在 rebase 跟进里要用的 oldBaseTip。
	task1Tip := gitOut(t, task1WorktreePath, "rev-parse", "HEAD")

	// task2：调度器 fillBaseRef 的等价手工步骤——栈在 task1 上面，
	// 这样它才会在下面被 MergePoller.rebaseFollowup 判定为"task1 的
	// 直接后继"（TasksWithBaseRef 按 base_ref == task1Branch 查找）。
	if err := m.SetBaseRef(ctx, task2.ID, &task1Branch); err != nil {
		t.Fatalf("设置 task2.base_ref 失败: %v", err)
	}
	repo2 := repo
	repo2.BaseRefOverride = task1Branch
	if err := pipe.Execute(ctx, ExecuteParams{
		TaskID: task2.ID, Repo: repo2, CloneURL: src, IssueID: "uuid-m3-2", Actor: "node:test",
	}); err != nil {
		t.Fatalf("task2 Execute 失败: %v", err)
	}
	t2, err := m.Get(ctx, task2.ID)
	if err != nil || t2.State != task.StatePROpen {
		t.Fatalf("task2 应到达 pr_open，实际 state=%v err=%v", t2, err)
	}
	task2Branch := *t2.BranchName
	task2WorktreePath := *t2.WorktreePath

	verifRowsBeforeReverify := len(verifs.rows)

	// ---- 中间态检查：squash 合并之前，task1 的分支仍被 task2（未合并
	// 的活后继）依赖，不该被回收。这正是生产代码 onMerged 在做完 rebase
	// 级联之后才会去问的同一个判定条件（HasLiveDependentOnBranch）——
	// 在这里、级联发生之前提前问一次，得到的答案必须是"仍被依赖"，
	// 这就是 F4.2-AC2 的判定依据确实如实反映了"级联之前"这个时刻的
	// 真实依赖关系（前驱分支在后继未合并前不被删除）。 ----
	if live, err := m.HasLiveDependentOnBranch(ctx, task1Branch); err != nil {
		t.Fatalf("查询 task1 分支活依赖失败: %v", err)
	} else if !live {
		t.Fatalf("squash 合并前，task1 分支 %q 应仍被 task2（base_ref 指向它、状态非终结）判定为被依赖", task1Branch)
	}
	if _, err := os.Stat(task1WorktreePath); err != nil {
		t.Fatalf("squash 合并前，task1 的 worktree 应仍存在: %v", err)
	}

	// ---- 步骤 2：模拟 squash 合并 ----
	//
	// squash 合并的本质特征："前驱分支的提交被压缩成一个新提交直接加到
	// default_branch 上，前驱分支自己的历史保持不变（不是被删除或改写，
	// 只是 default_branch 现在包含了等价的改动）"——用 git merge --squash
	// 在 origin（src，task1 已经把自己的分支推上去了）上手工模拟：
	// dev 上出现一个全新的、独立生成的 commit，SHA 与 task1Tip 完全不同，
	// 但内容等价（fileA.txt）。这正是"祖先关系断裂、需要 rebase 才能
	// 收敛"这个场景的直接原因：task1Tip 之后不再是 dev 的祖先。
	gitOut(t, src, "checkout", "-q", "dev")
	gitOut(t, src, "merge", "--squash", "--quiet", task1Branch)
	gitOut(t, src, "-c", "user.email=t@e.st", "-c", "user.name=t", "commit", "-qm", "squash merge M3-1")
	devTip := gitOut(t, src, "rev-parse", "dev")
	if devTip == task1Tip {
		t.Fatalf("squash 合并应产生一个新的 commit SHA，不应等于 task1 分支自己的 tip(%s)", task1Tip)
	}

	// 配置 fakeGitHub：针对 task1 的 PR 号，GetPRInfo 返回 Merged=true——
	// 这是触发 F4.1 合并检测的唯一信号源，测试代码不直接调用
	// m.Transition 把任务判成 merged。
	gh.setMerged(task1PRNumber)

	mp := &MergePoller{
		Tasks: m, Worktrees: wm, Pipeline: pipe, Notifier: &fakeNotifier{},
		ClientFactory: fixedClientFactory{clients: clients},
		RepoLookup:    repoLookupFixed(repo),
	}

	// ---- 步骤 3：触发合并检测（不等真实 ticker，直接跑一轮） ----
	if err := mp.pollOnce(ctx); err != nil {
		t.Fatalf("pollOnce 失败: %v", err)
	}

	// ---- 断言：task1 转 merged ----
	t1After, err := m.Get(ctx, task1.ID)
	if err != nil {
		t.Fatalf("读取 task1 失败: %v", err)
	}
	if t1After.State != task.StateMerged {
		t.Fatalf("task1 终态 = %s，期望 merged", t1After.State)
	}

	// ---- 断言：task2 的分支历史被自动 rebase 到 dev（squash 提交是
	// 独立生成的新 SHA，"是它的后代"就是"祖先关系已收敛"的直接证据） ----
	if !isAncestor(t, task2WorktreePath, devTip, "HEAD") {
		t.Errorf("task2 的分支历史应以 dev 的新 tip(squash 提交 %s) 为祖先——rebase 应已完成", devTip[:8])
	}

	// ---- 断言：task2.base_ref 被清空（rebase 到了 default_branch，
	// 不是另一个未合并前驱，走的是 F4.3 的"清空"分支） ----
	t2AfterRebase, err := m.Get(ctx, task2.ID)
	if err != nil {
		t.Fatalf("读取 task2 失败: %v", err)
	}
	if t2AfterRebase.BaseRef != nil {
		t.Errorf("task2.base_ref 应被清空（rebase 到 default_branch），实际 %q", *t2AfterRebase.BaseRef)
	}

	// ---- 断言：PR 自动 retarget——task2 重新走了一遍 push 阶段（不是
	// 调用 GitHub 的 retarget API），fakeGitHub 记录的【最后一次】
	// head==task2Branch 的 CreatePR 调用，其 Base 字段应是 dev。 ----
	assertHasCreatePRWithBase(t, gh.params, task2Branch, repo.DefaultBranch)

	// ---- 断言（核心）：diff 仍只含 2 的改动——task2 的 worktree 里，
	// dev（mirror 命名空间）到 HEAD 的改动文件清单只有 fileB.txt，不
	// 应包含 fileA.txt（那已经在 dev 上了，triple-dot diff 天然排除
	// 共同祖先已有的内容）。 ----
	diffOut := gitOut(t, task2WorktreePath, "diff", MirrorBaseRef(repo.DefaultBranch)+"...HEAD", "--name-only")
	gotFiles := map[string]bool{}
	for _, f := range splitNonEmptyLines(diffOut) {
		gotFiles[f] = true
	}
	if gotFiles["fileA.txt"] {
		t.Errorf("task2 的 diff 不应包含前驱 task1 的改动 fileA.txt，实际清单: %v", gotFiles)
	}
	if !gotFiles["fileB.txt"] {
		t.Errorf("task2 的 diff 应包含自己的改动 fileB.txt，实际清单: %v", gotFiles)
	}
	if len(gotFiles) != 1 {
		t.Errorf("task2 的 diff 应恰好只有 1 个改动文件(fileB.txt)，实际: %v", gotFiles)
	}

	// ---- 断言：自动重验通过——task2 终态是 pr_open（不是 failed），
	// 且 fakeVerifications 记录了新的验证行（不是复用旧记录，证明这次
	// 重验真的重新跑了一遍验证逻辑并落了新证据）。 ----
	if t2AfterRebase.State != task.StatePROpen {
		t.Fatalf("task2 重验后终态 = %s，期望 pr_open", t2AfterRebase.State)
	}
	if len(verifs.rows) <= verifRowsBeforeReverify {
		t.Errorf("task2 的 rebase 跟进重验应产生新的验证记录，重验前 %d 行，重验后 %d 行",
			verifRowsBeforeReverify, len(verifs.rows))
	}

	// 顺带确认重验没有重跑 agent（F4.3-AC3 附带效果，与
	// rebase_followup_test.go 同一断言手法）：初次实现两个任务各 2 次
	// 调用，恰好 4 次，重验不应增加。
	if len(ag.calls) != 4 {
		t.Errorf("rebase 跟进的重验不应调用 agent，调用次数应仍是 4，实际 %d", len(ag.calls))
	}

	// ---- 断言：task1 的 worktree 在整个流程完成之后已被回收
	// （F4.2-AC1）——此刻 task2 已经 rebase 完、base_ref 已清空，
	// HasLiveDependentOnBranch(task1Branch) 应转为 false，onMerged 因此
	// 会执行回收。 ----
	if live, err := m.HasLiveDependentOnBranch(ctx, task1Branch); err != nil {
		t.Fatalf("查询 task1 分支活依赖失败: %v", err)
	} else if live {
		t.Errorf("task2 已经 rebase 完并清空 base_ref，task1 分支 %q 不应再被判定为被依赖", task1Branch)
	}
	if _, err := os.Stat(task1WorktreePath); err == nil {
		t.Errorf("task1 的 worktree(%s) 应已被回收（F4.2-AC1），实际仍存在", task1WorktreePath)
	} else if !os.IsNotExist(err) {
		t.Errorf("检查 task1 worktree 是否已回收时出现意外错误: %v", err)
	}
}

// splitNonEmptyLines 按行切分并去掉空行——git 命令输出末尾常带一个
// 空行，直接 strings.Split 会多出一个 "" 元素污染文件清单断言。
func splitNonEmptyLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}
