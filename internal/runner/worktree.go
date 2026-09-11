package runner

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// gitTimeout 是单条 git 命令的上限。clone 大仓可能较慢，故给得宽松。
const gitTimeout = 15 * time.Minute

// WorktreeManager 管理任务工作区的生命周期。
//
// 结构：
//
//	<root>/.mirrors/<owner>-<repo>.git   bare mirror，充当对象库
//	<root>/<task-slug>/                  每任务一个 worktree
//
// 用 bare mirror 而非直接克隆的理由：多个 worktree 共享同一份对象库，
// 新任务只需 checkout 工作树而不必重新拉取历史；且这套结构在 P0 单机
// 与 P3 多节点上完全一致 —— 每个节点维护自己的 mirror 缓存即可。
type WorktreeManager struct {
	root string
	// mirrorLocks 串行化同一 mirror 上的 git 管理操作（fetch / worktree
	// add / remove）。P1 双通道并发后，两个任务可能同时操作同一仓库的
	// mirror，而 git 对 ref 与 worktree 注册文件的锁竞争会以难懂的错
	// 失败 —— 那是偶发故障，不是任务本身的问题，不该让任务买单。
	mirrorLocks sync.Map // mirrorPath -> *sync.Mutex
}

// lockMirror 锁住某 mirror 的管理操作，返回解锁函数。
func (m *WorktreeManager) lockMirror(mirror string) func() {
	v, _ := m.mirrorLocks.LoadOrStore(mirror, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// NewWorktreeManager 构造管理器。root 必须是绝对路径。
func NewWorktreeManager(root string) (*WorktreeManager, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("runner: 工作区根目录必须是绝对路径，得到 %q", root)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("runner: 创建工作区根目录失败: %w", err)
	}
	return &WorktreeManager{root: root}, nil
}

// Root 返回工作区根目录。
func (m *WorktreeManager) Root() string { return m.root }

// MirrorPath 返回某仓库的 bare mirror 路径。
func (m *WorktreeManager) MirrorPath(providerRepo string) string {
	safe := strings.ReplaceAll(providerRepo, "/", "-")
	safe = strings.ReplaceAll(safe, string(filepath.Separator), "-")
	return filepath.Join(m.root, ".mirrors", safe+".git")
}

// mirrorFetchRefspec 把远端分支映射进 refs/remotes/origin/* 命名空间。
//
// 为什么不能是 +refs/heads/*:refs/heads/*：任务分支也住在 refs/heads/*，
// 且推送前只存在于本地。那种 refspec 下 fetch --prune 会把「远端没有」
// 的任务分支全部剪掉 —— 即使它正被另一个 worktree 占用（git 的 prune
// 不做 worktree 占用检查）。真实事故（任务 #494）：并发任务启动时的
// fetch 剪掉了在途任务的分支，流水线随后的 commit 变成无父 root commit，
// diff base...HEAD 报 no merge base，任务以一个与根因无关的错误失败。
const mirrorFetchRefspec = "+refs/heads/*:refs/remotes/origin/*"

// MirrorBaseRef 返回基线分支在 mirror 里的全限定 ref。
//
// 任务分支（refs/heads/*）与远端镜像（refs/remotes/origin/*）分属两个
// 命名空间，fetch --prune 只清扫后者，在途任务分支天然免疫。
func MirrorBaseRef(base string) string {
	return "refs/remotes/origin/" + base
}

// EnsureMirror 确保 bare mirror 存在且是最新的。
//
// 首次调用会 clone --mirror；之后只做 fetch --prune。
func (m *WorktreeManager) EnsureMirror(ctx context.Context, providerRepo, cloneURL string) (string, error) {
	if cloneURL == "" {
		return "", fmt.Errorf("runner: 仓库 %s 缺少 clone URL", providerRepo)
	}
	mirror := m.MirrorPath(providerRepo)

	// clone/fetch 都会写 mirror 的 ref 空间，与 worktree 注册一样
	// 属于要串行化的管理操作。锁粒度是单个 mirror，不同仓库互不阻塞。
	unlock := m.lockMirror(mirror)
	defer unlock()

	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err != nil {
		if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
			return "", fmt.Errorf("runner: 创建 mirror 目录失败: %w", err)
		}
		if _, err := m.git(ctx, "", "clone", "--mirror", "--quiet", cloneURL, mirror); err != nil {
			return "", fmt.Errorf("runner: 克隆 mirror %s 失败: %w", providerRepo, err)
		}

		// ★安全：clone --mirror 会设 remote.origin.mirror=true，此时 git push
		// 变成镜像推送 —— 会把本地所有 ref 一并推上远端，包括 dev/test/main。
		// 这与「永不推受保护分支」直接冲突，必须在克隆后立刻解除。
		if _, err := m.git(ctx, mirror, "config", "--unset-all", "remote.origin.mirror"); err != nil {
			return "", fmt.Errorf("runner: 解除 mirror 推送模式失败（不解除会导致镜像推送覆盖受保护分支）: %w", err)
		}
	}

	// refspec 每次重写（幂等）：旧版 mirror 里的 +refs/heads/*:refs/heads/*
	// 配置在这里被就地纠正，存量 mirror 无需手工迁移。
	if _, err := m.git(ctx, mirror, "config", "--replace-all", "remote.origin.fetch", mirrorFetchRefspec); err != nil {
		return "", fmt.Errorf("runner: 设置 fetch refspec 失败: %w", err)
	}
	if _, err := m.git(ctx, mirror, "fetch", "--prune", "--quiet", "origin"); err != nil {
		return "", fmt.Errorf("runner: 更新 mirror %s 失败: %w", providerRepo, err)
	}
	return mirror, nil
}

// HasChanges 报告工作区是否有未提交的改动。
//
// agent 跑完却没有任何改动，说明它实际上没干活 —— 这是一种失败，
// 不能当成"改动为空的成功"放过去。
func (m *WorktreeManager) HasChanges(ctx context.Context, wt *Worktree) (bool, error) {
	out, err := m.git(ctx, wt.Path, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("runner: 检查工作区改动失败: %w", err)
	}
	return strings.TrimSpace(out) != "", nil
}

// Commit 把工作区全部改动提交。
//
// 提交由流水线负责而非 agent：agent 的 prompt 明确禁止它执行 git 操作，
// 这样提交信息格式统一，也避免 agent 意外 push。
func (m *WorktreeManager) Commit(ctx context.Context, wt *Worktree, message string) error {
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("runner: 提交信息为空")
	}
	if _, err := m.git(ctx, wt.Path, "add", "-A"); err != nil {
		return fmt.Errorf("runner: 暂存改动失败: %w", err)
	}
	if _, err := m.git(ctx, wt.Path, "commit", "-q", "-m", message); err != nil {
		return fmt.Errorf("runner: 提交失败: %w", err)
	}
	return nil
}

// Push 把任务分支推到远端。
//
// 两道防线：
//  1. 先校验分支不在受保护列表里；
//  2. 用完整显式 refspec 推送，杜绝任何"顺带推别的 ref"的可能。
//
// 推送是无人值守流水线里必经的公网操作，网络抖动（SSH kex 被掐、DNS
// 抖动、网关 502）不该一次就把整个任务判死 —— 抖动几秒就恢复，而任务
// 从实现烧到验证花了几十分钟（任务 #1551 的教训：kex 断连一次直接
// 判败）。因此对非明确永久的错误做有限次退避重试。push 幂等：若首次
// 其实已推上远端只是响应丢失，重试会得到 "Everything up-to-date" 的
// 成功，不会重复建分支。
//
// onProgress 可为 nil；非 nil 时在每次退避重试前与「重试后成功」时各
// 回调一次，调用方（pipeline）据此把重试过程写进任务事件流 —— 重试
// 不该是黑盒，详情页要看得见。
func (m *WorktreeManager) Push(ctx context.Context, wt *Worktree, repo RepoConfig, onProgress func(PushProgress)) error {
	if wt == nil {
		return fmt.Errorf("runner: 工作区为空")
	}
	if err := repo.ValidatePushTarget(wt.Branch); err != nil {
		return err
	}
	// 基线分支同样不能成为推送目标（防止配置错误把任务分支命名成 dev）
	if base, err := repo.BaseBranch(KindFix); err == nil && wt.Branch == base {
		return ErrProtectedBranch{Branch: wt.Branch, Repo: repo.ProviderRepo}
	}

	refspec := fmt.Sprintf("refs/heads/%s:refs/heads/%s", wt.Branch, wt.Branch)
	push := func() error {
		_, err := m.git(ctx, wt.Path, "push", "--set-upstream", "origin", refspec)
		return err
	}
	return pushWithRetry(ctx, wt.Branch, push, pushBackoff, onProgress)
}

// RebaseOnto 把 wt 当前 checkout 出来的分支从"基于 oldBaseTip 之前的
// 内容"改写成"基于 newBaseBranch 当前内容"——F4.3 rebase 跟进的核心
// git 操作。
//
// 不在 rebase 命令末尾带分支参数：省略时 git 默认操作当前 checkout 出来
// 的分支（也就是 wt.Branch 本身），比显式传分支名更安全——不会有
// "传错分支"的出错面。
//
// 调用前只刷新 mirror（fetch --prune），不走完整 EnsureMirror（不需要
// 也拿不到 cloneURL）：能调用到这里的 wt 必然是已经跑过 Create 的任务
// 现场，mirror 早就存在且配好了 origin，只需要把 newBaseBranch 最新的
// 内容 fetch 进镜像命名空间。
//
// rebase 冲突（或任何其它非零退出）原样返回，不吞不重试——冲突需要人
// 来看，自动重试或悄悄吞掉都会把冲突留在一个半改写的工作区里，比明确
// 报错更危险。
func (m *WorktreeManager) RebaseOnto(ctx context.Context, wt *Worktree, oldBaseTip, newBaseBranch string) error {
	if wt == nil {
		return fmt.Errorf("runner: 工作区为空")
	}

	unlock := m.lockMirror(wt.Mirror)
	_, err := m.git(ctx, wt.Mirror, "fetch", "--prune", "--quiet", "origin")
	unlock()
	if err != nil {
		return fmt.Errorf("runner: 刷新 mirror 失败: %w", err)
	}

	newBaseRef := MirrorBaseRef(newBaseBranch)
	if _, err := m.git(ctx, wt.Path, "rebase", "--onto", newBaseRef, oldBaseTip); err != nil {
		return fmt.Errorf("runner: rebase 分支 %s 到 %s 失败: %w", wt.Branch, newBaseBranch, err)
	}
	return nil
}

// ForcePush 把改写过历史的任务分支强推到远端，覆盖远端的旧历史
// （RebaseOnto 之后分支的提交历史已经变了，普通 Push 会被
// non-fast-forward 拒绝，只能强推）。
//
// 用 --force-with-lease 而不是裸 --force：远端在我们上次 fetch 之后被
// 别人动过时会拒绝，不会悄悄覆盖掉别人刚推的东西——RebaseOnto 调用时
// 已经 fetch 过一次，本地记录的 refs/remotes/origin/<branch> 就是那次
// fetch 看到的远端状态，恰好是 force-with-lease 用来判断"远端有没有被
// 别人动过"的依据。
//
// 结构照抄 Push：先校验推送目标不是受保护分支，再拼完整显式 refspec，
// 复用同一套 pushWithRetry 退避重试——网络抖动不该让 rebase 跟进这一步
// 比普通推送更脆弱。
func (m *WorktreeManager) ForcePush(ctx context.Context, wt *Worktree, repo RepoConfig, onProgress func(PushProgress)) error {
	if wt == nil {
		return fmt.Errorf("runner: 工作区为空")
	}
	if err := repo.ValidatePushTarget(wt.Branch); err != nil {
		return err
	}
	if base, err := repo.BaseBranch(KindFix); err == nil && wt.Branch == base {
		return ErrProtectedBranch{Branch: wt.Branch, Repo: repo.ProviderRepo}
	}

	refspec := fmt.Sprintf("refs/heads/%s:refs/heads/%s", wt.Branch, wt.Branch)
	push := func() error {
		_, err := m.git(ctx, wt.Path, "push", "--force-with-lease", "--set-upstream", "origin", refspec)
		return err
	}
	return pushWithRetry(ctx, wt.Branch, push, pushBackoff, onProgress)
}

// PushProgress 是推送进度的一次回调通知。
type PushProgress struct {
	Attempt     int           // 刚结束的尝试序号（1 起）
	MaxAttempts int           // 尝试上限
	Wait        time.Duration // >0：本次失败，将退避 Wait 后重试；=0 且 Err==nil：重试后成功
	Err         error         // 本次错误；成功通知里为 nil
}

// pushAttempts 是推送的最大尝试次数（含首次）。
const pushAttempts = 4

// pushBackoff 是第 n 次失败后的退避等待，逐次拉长。最坏情况下为任务
// 增加约 22s 延迟 —— 相对重烧实现+验证的代价可忽略。
var pushBackoff = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second}

// pushWithRetry 是推送的重试循环，与 git 调用解耦以便单测。
// 尝试次数 = len(backoff)+1。
func pushWithRetry(ctx context.Context, branch string, push func() error, backoff []time.Duration, onProgress func(PushProgress)) error {
	attempts := len(backoff) + 1
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err = push(); err == nil {
			if attempt > 1 && onProgress != nil {
				onProgress(PushProgress{Attempt: attempt, MaxAttempts: attempts})
			}
			return nil
		}
		if isPermanentPushErr(err) || attempt == attempts {
			break
		}
		wait := backoff[attempt-1]
		slog.Warn("推送失败（疑似网络抖动），退避后重试",
			"branch", branch, "attempt", attempt, "wait", wait, "err", err)
		if onProgress != nil {
			onProgress(PushProgress{Attempt: attempt, MaxAttempts: attempts, Wait: wait, Err: err})
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done(): // 关机/超时：不等了，把原始错误交出去
			timer.Stop()
			return fmt.Errorf("runner: 推送分支 %s 失败: %w", branch, err)
		case <-timer.C:
		}
	}
	return fmt.Errorf("runner: 推送分支 %s 失败: %w", branch, err)
}

// permanentPushErrs 是明确无救的推送错误特征（小写匹配）：同样参数重试
// 只会得到同样的拒绝，应立即失败把原因摆给人看。
//
// 注意：网络断连时 git 也会打印 "Please make sure you have the correct
// access rights" 的兜底提示语，所以「access rights」绝不能进这个列表 ——
// 判永久要靠只在真实拒绝里出现的措辞。
var permanentPushErrs = []string{
	"non-fast-forward", // 远端已分叉，需要人介入 rebase
	"fetch first",      // non-fast-forward 的 hint 文案
	"stale info",       // force-with-lease 竞争
	"permission denied",
	"denied to", // "Permission to x/y.git denied to <user>"
	"authentication failed",
	"returned error: 403", // HTTPS 鉴权/越权拒绝（5xx 才是抖动）
	"repository not found",
	"does not appear to be a git repository",
	"protected branch", // 远端分支保护规则
	"push protection",  // GitHub push protection（如泄密扫描）
	"gh006", "gh013",   // GitHub 保护类拦截的错误码
	"no such ref", "src refspec", // 本地 ref 缺失，属程序错误
}

// isPermanentPushErr 报告推送错误是否明确无救（重试无意义）。
// 不在列表里的错误一律按可重试处理：误重试的代价是几十秒，
// 误判永久（不重试）的代价是整个任务判死 —— 后者贵得多。
func isPermanentPushErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, pat := range permanentPushErrs {
		if strings.Contains(msg, pat) {
			return true
		}
	}
	return false
}

// HasCommitsAhead 报告任务分支相对基线是否已有提交。
//
// 断点续跑的提交阶段用它区分「agent 没干活」（无提交无改动，失败）与
// 「上次已提交过」（有提交无改动，直接进验证）。
func (m *WorktreeManager) HasCommitsAhead(ctx context.Context, wt *Worktree) (bool, error) {
	out, err := m.git(ctx, wt.Path, "rev-list", "--count", MirrorBaseRef(wt.BaseBranch)+"..HEAD")
	if err != nil {
		return false, fmt.Errorf("runner: 统计分支提交失败: %w", err)
	}
	return strings.TrimSpace(out) != "0", nil
}

// ChangedFiles 列出任务分支相对基线的改动文件（新增/修改，相对路径）。
//
// 删除的文件被排除：档位路由与复现测试识别都只关心"现在存在什么"，
// 删掉的文件既不能跑也不能拷。基线用镜像命名空间的全限定 ref 解析，
// 避免与 refs/heads/* 下可能存在的同名残留分支产生歧义。
func (m *WorktreeManager) ChangedFiles(ctx context.Context, wt *Worktree) ([]string, error) {
	out, err := m.git(ctx, wt.Path, "diff", "--name-only", "--diff-filter=AM", MirrorBaseRef(wt.BaseBranch)+"...HEAD")
	if err != nil {
		return nil, fmt.Errorf("runner: 列出改动文件失败: %w", err)
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if f := strings.TrimSpace(line); f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// CreateDetached 在基线分支上建一个临时工作区（detached HEAD，不建分支），
// 供 heavy 档红阶段复现用：在【改动前】的代码上跑 agent 带来的复现测试。
//
// 工作区放在 root 下的隐藏目录 .verify/ 里，与任务工作区（issue 编号命名）
// 隔开，也不会被验证步骤的目录扫描走进去。调用方必须负责 Remove(force=true)。
func (m *WorktreeManager) CreateDetached(ctx context.Context, providerRepo, base, name string) (*Worktree, error) {
	mirror := m.MirrorPath(providerRepo)
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err != nil {
		return nil, fmt.Errorf("runner: 仓库 %s 尚无 mirror，无法建基线工作区", providerRepo)
	}

	unlock := m.lockMirror(mirror)
	defer unlock()

	path := filepath.Join(m.root, ".verify", name)
	if _, err := os.Stat(path); err == nil {
		// 上次崩溃留下的残骸：先清掉再建，不让一次意外卡死后续所有任务
		_, _ = m.git(ctx, mirror, "worktree", "remove", "--force", path)
	}
	if _, err := m.git(ctx, mirror, "worktree", "add", "--quiet", "--detach", path, MirrorBaseRef(base)); err != nil {
		return nil, fmt.Errorf("runner: 创建基线工作区失败（基线 %s）: %w", base, err)
	}
	return &Worktree{Path: path, Branch: "(detached)", BaseBranch: base, Mirror: mirror}, nil
}

// Worktree 是一个已创建的任务工作区。
type Worktree struct {
	Path       string // 工作区绝对路径
	Branch     string // 新建的任务分支
	BaseBranch string // 分叉基线的短名（如 dev；开 PR 用。git 解析时须经 MirrorBaseRef 限定到镜像命名空间）
	Mirror     string // 所属 bare mirror
}

// CreateParams 描述要创建的工作区。
type CreateParams struct {
	Repo     RepoConfig
	CloneURL string
	Kind     TaskKind
	IssueKey string
	Title    string
	// TaskID 参与目录名（<issue-key>-t<taskID>），让每个任务有自己的
	// 槽位。<=0 时退回只有 issue key 的老命名 —— 调用方应尽量传。
	TaskID int64
	// ClaimedPaths 是**其他活任务**当前占用的工作区路径集合
	// （task.Machine.ClaimedWorktreePaths）。Create 接管同名槽位时会
	// 用它判断目标目录是否仍属于在途任务 —— 是则拒绝，而不是把别人
	// 正在跑的工作区当尸体回收。为空表示不做这道检查（测试与单次调用）。
	ClaimedPaths []string
}

// Create 建立任务工作区：更新 mirror → 计算基线与分支名 → 新建 worktree。
func (m *WorktreeManager) Create(ctx context.Context, p CreateParams) (*Worktree, error) {
	base, err := p.Repo.BaseBranch(p.Kind)
	if err != nil {
		return nil, err
	}
	branch, err := p.Repo.BranchName(p.Kind, p.IssueKey, p.Title)
	if err != nil {
		return nil, err
	}

	mirror, err := m.EnsureMirror(ctx, p.Repo.ProviderRepo, p.CloneURL)
	if err != nil {
		return nil, err
	}

	unlock := m.lockMirror(mirror)
	defer unlock()

	// 基线分支必须真实存在，否则 worktree add 会报出难懂的错。
	// 查的是镜像命名空间（refs/remotes/origin/*），不是 refs/heads/* ——
	// 后者可能躺着远端分支的陈旧副本或已失败任务留下的同名分支。
	baseRef := MirrorBaseRef(base)
	if _, err := m.git(ctx, mirror, "rev-parse", "--verify", "--quiet", baseRef+"^{commit}"); err != nil {
		return nil, fmt.Errorf("runner: 基线分支 %q 在仓库 %s 中不存在", base, p.Repo.ProviderRepo)
	}

	path := filepath.Join(m.root, worktreeDirName(p.IssueKey, p.TaskID, branch))

	// 活任务认领检查：目标路径（新命名的本任务槽位，以及老命名的共用
	// 槽位）若正被另一个**非终态**任务占用，说明我们要接管的是别人正在
	// 跑的工作区 —— 绝不能回收，报错让人来看。
	//
	// 为什么需要：老命名下同 issue 的两次尝试共用一个目录，而
	// tasks_one_active_per_issue 只管活任务之间（且带 repo_id 维度），
	// 另一个 repo 或另一个用户的同名 issue 仍是合法活任务。那条路径上
	// Create 会把在途现场当尸体删掉。
	if claimedBy(p.ClaimedPaths, path) {
		return nil, fmt.Errorf(
			"runner: 工作区 %s 仍被一个在途任务占用，拒绝接管（请先处理那个任务，或等它结束）", path)
	}

	// 老命名的共用槽位（存量兼容）：只有它与本任务槽位不是同一个目录时
	// 才需要额外照顾。TaskID<=0 退回老命名，此时两者相同，走单一路径。
	legacy := filepath.Join(m.root, legacyWorktreeDirName(p.IssueKey, branch))
	legacyUsable := legacy != path
	if legacyUsable && claimedBy(p.ClaimedPaths, legacy) {
		// 老槽位被在途任务占着就不碰它，也不认它为可接管的尸体 ——
		// 本任务在自己的新槽位里跑，互不干扰。
		//
		// 注意这里**不**报错：老槽位不是本任务要用的路径，别人在里面跑
		// 与我们无关。报错会让一个无关任务把本任务卡死。
		slog.Info("老命名的共用槽位仍被在途任务占用，跳过它（本任务用自己的槽位）",
			"legacy", legacy, "path", path)
		legacyUsable = false
	}

	// 尸体回收：目标目录或同名分支已存在时，回收后再建，而非报错卡死。
	//
	// D4 的语义：现场一直留到「需要这个槽位的人来了」为止。加 task_id
	// 维度之后，本任务的槽位只可能被**本任务自己**的上一次尝试占用
	// （重跑同一任务），老槽位则可能躺着老命名时代的现场 —— 两种都回收。
	// 分支尸体也要清：目录被人手工删掉后 refs/heads/<branch> 还在，
	// worktree add -b 会报 branch already exists。
	branchExists := false
	if _, err := m.git(ctx, mirror, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}"); err == nil {
		branchExists = true
	}

	var corpse string
	if _, pathErr := os.Stat(path); pathErr == nil {
		corpse = path
	} else if legacyUsable {
		if _, legErr := os.Stat(legacy); legErr == nil {
			corpse = legacy
		}
	}

	if corpse != "" || branchExists {
		slog.Warn("回收同名工作区尸体（上一任务保留的现场）",
			"path", corpse, "branch", branch, "branchExists", branchExists,
			"legacy", corpse != "" && corpse != path)
		m.discardLocked(ctx, mirror, corpse, branch)
	}

	if _, err := m.git(ctx, mirror, "worktree", "add", "--quiet", "-b", branch, path, baseRef); err != nil {
		return nil, fmt.Errorf("runner: 创建工作区失败（分支 %s，基线 %s）: %w", branch, base, err)
	}

	return &Worktree{Path: path, Branch: branch, BaseBranch: base, Mirror: mirror}, nil
}

// Remove 回收工作区。
//
// force 为 false 时，git 会拒绝删除有未提交改动的工作区 —— 这正是
// 失败任务「保留现场」（D4）所需的保护。任务成功合并后才用 force。
func (m *WorktreeManager) Remove(ctx context.Context, wt *Worktree, force bool) error {
	if wt == nil {
		return fmt.Errorf("runner: 工作区为空")
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, wt.Path)

	unlock := m.lockMirror(wt.Mirror)
	defer unlock()

	if _, err := m.git(ctx, wt.Mirror, args...); err != nil {
		return fmt.Errorf("runner: 回收工作区 %s 失败: %w", wt.Path, err)
	}
	// 顺带删掉任务分支（已合并进 PR，本地副本无保留价值）
	if force {
		_, _ = m.git(ctx, wt.Mirror, "branch", "-D", wt.Branch)
	}
	return nil
}

// DiscardResult 报告一次丢弃动作实际做成了什么。
//
// 为什么 Discard 必须返回结果而不是「尽力而为、无返回值」：收割机要
// 记的账是「这个现场被回收了」，而它此前无论 Discard 是否真的碰过磁盘
// 都照样打一条「已回收」并清空 worktree_path。mirror 不存在时 Discard
// 直接 return（什么都没删）、Path 为空时跳过删目录 —— 这些情形下记的
// 账都是假的，日志会告诉运维「清理干净了」，而目录还躺在盘上。
//
// 各字段都是「确实做成了」的布尔事实，不是「尝试过」：
//   - DirRemoved：目录此前存在、现在不存在了
//   - BranchDeleted：此前 refs/heads/<branch> 存在、现在不存在了
//   - MirrorMissing：mirror 不在，什么都没做（调用方据此别记账为已回收）
//   - Err：过程中遇到的非致命错误（已尽力继续），供日志与排障
type DiscardResult struct {
	MirrorMissing bool
	DirRemoved    bool
	BranchDeleted bool
	Errs          []error
}

// Removed 报告这次丢弃是否真的清掉了东西（目录或分支）。
//
// MirrorMissing 为真时恒为 false：连 mirror 都没有，谈不上删除动作，
// 调用方不应当据此清空任务行的 worktree_path（那会让这条路径彻底
// 脱离收割机的视野）。
func (r DiscardResult) Removed() bool {
	return !r.MirrorMissing && (r.DirRemoved || r.BranchDeleted)
}

// Discard 丢弃一个工作区及其分支（重试与启动恢复场景：旧现场作废）。
//
// 与 Remove 的区别在于容错：现场可能是残缺的（目录被手动删过、分支
// 已不存在），Discard 尽力清理每一步并继续，最后 prune 兑底。
// 不清理的话，同名分支会让下一次 worktree add -b 直接失败。
//
// 返回 DiscardResult 说明实际做成了什么 —— 调用方（收割机）据此决定
// 日志与记账，不再无条件宣称「已回收」。
func (m *WorktreeManager) Discard(ctx context.Context, providerRepo, path, branch string) DiscardResult {
	mirror := m.MirrorPath(providerRepo)
	if _, err := os.Stat(mirror); err != nil {
		// 没有 mirror 就没什么可丢的。这**不是**一次成功的回收：
		// 目录可能还完整地待在盘上。
		return DiscardResult{MirrorMissing: true}
	}
	unlock := m.lockMirror(mirror)
	defer unlock()
	return m.discardLocked(ctx, mirror, path, branch)
}

// discardLocked 是 Discard 的持锁版本，调用方必须已持有 mirror 锁
// （Create 的尸体回收在锁内调用；sync.Mutex 不可重入，直接调 Discard
// 会自死锁）。
func (m *WorktreeManager) discardLocked(ctx context.Context, mirror, path, branch string) DiscardResult {
	res := DiscardResult{}
	if path != "" {
		// 删之前先记下「原本在不在」。
		//
		// ⚠ 这一步不能省：`git worktree remove --force` **成功时会自己把
		// 目录删掉**。原实现只在 git remove 之后 os.Stat 一次，把「目录还在」
		// 当作 DirRemoved 的判据 —— 于是最常见的正常路径（git 删成功）反而
		// 被判成「什么都没删」，DirRemoved 恒为 false。后果是收割机主路径
		// 的 res.Removed() 为 false（分支被保留时 BranchDeleted 也是 false），
		// 回收计数永远是 0，日志说「没清掉任何东西」而目录其实已经没了。
		//
		// DirRemoved 的语义是「原本在、现在不在了」，所以要两头各判一次。
		_, statErr := os.Stat(path)
		existedBefore := statErr == nil

		if _, err := m.git(ctx, mirror, "worktree", "remove", "--force", path); err != nil {
			slog.Warn("丢弃工作区失败（继续清理）", "path", path, "err", err)
			res.Errs = append(res.Errs, err)
		}
		// 目录可能不是注册的 worktree（手工建的/半残的），git 拒绝删除；
		// 兜底直接删目录，否则下一步 worktree add 还会撞「目录已存在」。
		if _, err := os.Stat(path); err == nil {
			if rerr := os.RemoveAll(path); rerr != nil {
				slog.Warn("删除工作区目录失败（继续）", "path", path, "err", rerr)
				res.Errs = append(res.Errs, rerr)
			}
		}
		// 现在再判一次：原本在、且此刻不在了，才算真删掉了。
		// 兜底的 RemoveAll 失败时目录仍在，这里自然为 false。
		if existedBefore {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				res.DirRemoved = true
			}
		}
	}
	_, _ = m.git(ctx, mirror, "worktree", "prune")
	if branch != "" {
		// 删之前先看它到底在不在 —— 否则「删掉了」这个记账可能是假的
		// （分支原本就不存在，git branch -D 会报错，而错误被吞掉）。
		branchExisted := false
		if _, err := m.git(ctx, mirror, "rev-parse", "--verify", "--quiet",
			"refs/heads/"+branch+"^{commit}"); err == nil {
			branchExisted = true
		}
		if _, err := m.git(ctx, mirror, "branch", "-D", branch); err != nil {
			slog.Warn("删除残留分支失败（继续）", "branch", branch, "err", err)
			res.Errs = append(res.Errs, err)
		}
		res.BranchDeleted = branchExisted
	}
	return res
}

// WorktreeState 是 Inspect 对一份任务现场的体检结果。
//
// 智能重试（retry.go）据此判断现场能否续跑：目录与分支都在才能谈
// 「续」，否则只能丢弃重建。所有字段都是尽力探测 —— 目录不存在时
// 后续字段保持零值，Inspect 不因此报错。
type WorktreeState struct {
	// Exists 表示工作区目录存在且是一个 git 工作树。
	Exists bool
	// Registered 表示 mirror 的 worktree 注册表里有这个路径
	//（目录被手动删过但注册残留时为 false，需要 prune/重建）。
	Registered bool
	// BranchExists 表示任务分支仍在 mirror 的 refs/heads/ 下。
	BranchExists bool
	// HasCommits 表示任务分支相对基线已有提交（实现阶段出过成果）。
	HasCommits bool
	// Dirty 表示工作区有未提交改动（agent 中断的半成品，或人手工介入）。
	Dirty bool
	// RemoteBranch 表示远端似乎已有该分支（worktree 里存在
	// refs/remotes/origin/<branch> 追踪引用，说明 push 成功过）。
	RemoteBranch bool
	// Commits 是相对基线的提交数（HasCommits 为真时 >0），展示用。
	Commits int
}

// Usable 报告现场是否达到续跑的最低门槛：目录、注册、分支三者俱在。
func (s *WorktreeState) Usable() bool {
	return s != nil && s.Exists && s.Registered && s.BranchExists
}

// Inspect 体检一份任务现场。path/branch 来自任务行，可能已残缺不全；
// 本函数永不返回错误 —— 探测失败只意味着对应字段为 false，决策层
// （PlanRetry）会把「查不出来」当「不可用」处理，安全地降级为重建。
func (m *WorktreeManager) Inspect(ctx context.Context, providerRepo, path, branch, base string) *WorktreeState {
	st := &WorktreeState{}
	mirror := m.MirrorPath(providerRepo)

	if path == "" {
		return st
	}
	if _, err := os.Stat(path); err != nil {
		return st // 目录没了，其余无从谈起
	}
	if _, err := m.git(ctx, path, "rev-parse", "--git-dir"); err != nil {
		return st // 目录在但已不是 git 工作树（被手动清过？）
	}
	st.Exists = true

	if _, err := os.Stat(mirror); err == nil {
		unlock := m.lockMirror(mirror)
		out, err := m.git(ctx, mirror, "worktree", "list", "--porcelain")
		unlock()
		if err == nil {
			for _, line := range strings.Split(out, "\n") {
				p, ok := strings.CutPrefix(strings.TrimSpace(line), "worktree ")
				if ok && filepath.Clean(p) == filepath.Clean(path) {
					st.Registered = true
					break
				}
			}
		}
		if branch != "" {
			if _, err := m.git(ctx, mirror, "rev-parse", "--verify", "--quiet",
				"refs/heads/"+branch+"^{commit}"); err == nil {
				st.BranchExists = true
			}
		}
	}

	// 以下探测都在工作区自身上进行，与 mirror 无关。
	if out, err := m.git(ctx, path, "status", "--porcelain"); err == nil {
		st.Dirty = strings.TrimSpace(out) != ""
	}
	if branch != "" && st.BranchExists {
		baseRef := MirrorBaseRef(base)
		if out, err := m.git(ctx, path, "rev-list", "--count", baseRef+".."+branch); err == nil {
			var n int
			if _, serr := fmt.Sscanf(strings.TrimSpace(out), "%d", &n); serr == nil {
				st.Commits = n
				st.HasCommits = n > 0
			}
		}
		if _, err := m.git(ctx, path, "rev-parse", "--verify", "--quiet",
			"refs/remotes/origin/"+branch+"^{commit}"); err == nil {
			st.RemoteBranch = true
		}
	}
	return st
}

// Prune 清理已消失目录的 worktree 注册记录。
func (m *WorktreeManager) Prune(ctx context.Context, providerRepo string) error {
	mirror := m.MirrorPath(providerRepo)
	if _, err := os.Stat(mirror); err != nil {
		return nil // 没有 mirror 就没什么可清理
	}
	if _, err := m.git(ctx, mirror, "worktree", "prune"); err != nil {
		return fmt.Errorf("runner: prune 失败: %w", err)
	}
	return nil
}

// List 列出某仓库当前注册的 worktree 路径（不含 bare mirror 自身）。
func (m *WorktreeManager) List(ctx context.Context, providerRepo string) ([]string, error) {
	mirror := m.MirrorPath(providerRepo)
	if _, err := os.Stat(mirror); err != nil {
		return nil, nil
	}
	out, err := m.git(ctx, mirror, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("runner: 列出工作区失败: %w", err)
	}

	var paths []string
	for _, line := range strings.Split(out, "\n") {
		p, ok := strings.CutPrefix(strings.TrimSpace(line), "worktree ")
		if !ok {
			continue
		}
		if filepath.Clean(p) == filepath.Clean(mirror) {
			continue // bare mirror 自身
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// claimedBy 报告 path 是否出现在活任务认领的路径集合里。
//
// 两侧都过 filepath.Abs + Clean 再比：数据库里存的是 Create 写进去的
// 绝对路径，而根目录配置可能有尾斜杠或符号链接。比较失败（Abs 出错）
// 一律按「已认领」处理 —— 保守方向是少删。
func claimedBy(claimed []string, path string) bool {
	if len(claimed) == 0 {
		return false
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return true
	}
	target = filepath.Clean(target)
	for _, c := range claimed {
		abs, aerr := filepath.Abs(c)
		if aerr != nil {
			continue
		}
		if filepath.Clean(abs) == target {
			return true
		}
	}
	return false
}

// worktreeDirName 生成工作区目录名。
//
// **目录名必须带维度。** 原实现是 strings.ToLower(issueKey)，于是 issue
// CR-100 的任何一次尝试、任何仓库、任何用户都落在同一个 <root>/cr-100。
// 数据库的 tasks_one_active_per_issue 只约束 (repo_id, issue_key) 上的
// 活任务，两个不同 repo（乃至不同用户）可以同时各有一个活任务、issue
// key 相同 —— 它们会占用同一个目录，而收割机只认字符串路径，看到老任务
// 行指着它就删。加上 task_id 之后每个任务有自己的槽位，跨任务/仓库/用户
// 撞名从根上消失（收割机那道活任务认领闸是第二道防线，不是唯一防线）。
//
// 保留 issue key 前缀（而不是只用 task_id）是为了排障：`ls workspaces/`
// 一眼能看出哪个目录对应哪个单子。
func worktreeDirName(issueKey string, taskID int64, branch string) string {
	name := strings.ToLower(strings.TrimSpace(issueKey))
	if name == "" {
		name = strings.ReplaceAll(branch, "/", "-")
	}
	if taskID <= 0 {
		// 拿不到任务号就没有维度可用。退回老命名而不是拼一个 "-t0"：
		// 老命名的路径是 legacyWorktreeDirName 认得的形状，仍然能被
		// 收割机正常处理。
		return name
	}
	return fmt.Sprintf("%s-t%d", name, taskID)
}

// legacyWorktreeDirName 是加 task_id 维度之前的命名（只有 issue key）。
//
// 存量目录都长这样，而每一条老任务行里存的是**完整路径**，所以老任务
// 找自己的现场不受影响（它们不走命名规则，直接读 worktree_path）。
// 需要这个函数的只有两处：
//
//   - Create 接管同名槽位时顺带探测老路径（老现场可能是上一个失败任务
//     按 D4 保留的，不认它就会遗留一个永远没人回收、也永远挡不住任何
//     东西的目录）；
//   - 收割机的孤儿清扫**不**需要它：老目录本来就在 <root> 下、名字不以
//     . 开头、没人认领、mtime 一过 TTL 就照原规则回收。换句话说，改命名
//     规则不会让任何存量目录被误判 —— 误判的风险在另一个方向（把老目录
//     当"无主"提前删掉），而那一侧靠 mtime + 认领闸兜着。
func legacyWorktreeDirName(issueKey, branch string) string {
	name := strings.ToLower(strings.TrimSpace(issueKey))
	if name == "" {
		name = strings.ReplaceAll(branch, "/", "-")
	}
	return name
}

// git 执行一条 git 命令。dir 为空时在进程当前目录执行。
func (m *WorktreeManager) git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// 禁止 git 弹交互式凭据提示：无人值守环境下会永久挂起
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"GCM_INTERACTIVE=never",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w（%s）",
			strings.Join(args, " "), err, truncate(stderr.String(), 600))
	}
	return stdout.String(), nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	// 回退到 rune 边界：按字节硬切会切断多字节 UTF-8 字符，
	// 落库时 Postgres 拒绝非法 UTF-8（SQLSTATE 22021）。
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…(已截断)"
}
