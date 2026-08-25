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
func (m *WorktreeManager) Push(ctx context.Context, wt *Worktree, repo RepoConfig) error {
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
	if _, err := m.git(ctx, wt.Path, "push", "--set-upstream", "origin", refspec); err != nil {
		return fmt.Errorf("runner: 推送分支 %s 失败: %w", wt.Branch, err)
	}
	return nil
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

	path := filepath.Join(m.root, worktreeDirName(p.IssueKey, branch))

	// 尸体回收：目标目录或同名分支已存在时，回收后再建，而非报错卡死。
	//
	// 能走到 Create 的都是全新执行（断点续跑复用现场、不经过这里），
	// 而 tasks_one_active_per_issue 保证同 (repo, issue) 没有第二个活
	// 任务 —— 同名残留只会是上一个失败/取消任务按 D4 保留的现场。
	// D4 的语义不变：现场一直留到「同 issue 的下一次尝试需要这个槽位」
	// 为止；此时不回收，同 issue 的重试与重新触发会永久撞名
	//（任务 #345/#466：worktree 尸体阻塞重试）。分支尸体也要清：目录
	// 被人手工删掉后 refs/heads/<branch> 还在，worktree add -b 会报
	// branch already exists。
	_, pathErr := os.Stat(path)
	branchExists := false
	if _, err := m.git(ctx, mirror, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}"); err == nil {
		branchExists = true
	}
	if pathErr == nil || branchExists {
		slog.Warn("回收同名工作区尸体（上一任务保留的现场）",
			"path", path, "branch", branch,
			"pathExists", pathErr == nil, "branchExists", branchExists)
		m.discardLocked(ctx, mirror, path, branch)
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

// Discard 丢弃一个工作区及其分支（重试与启动恢复场景：旧现场作废）。
//
// 与 Remove 的区别在于容错：现场可能是残缺的（目录被手动删过、分支
// 已不存在），Discard 尽力清理每一步并继续，最后 prune 兑底。
// 不清理的话，同名分支会让下一次 worktree add -b 直接失败。
func (m *WorktreeManager) Discard(ctx context.Context, providerRepo, path, branch string) {
	mirror := m.MirrorPath(providerRepo)
	if _, err := os.Stat(mirror); err != nil {
		return // 没有 mirror 就没什么可丢的
	}
	unlock := m.lockMirror(mirror)
	defer unlock()
	m.discardLocked(ctx, mirror, path, branch)
}

// discardLocked 是 Discard 的持锁版本，调用方必须已持有 mirror 锁
// （Create 的尸体回收在锁内调用；sync.Mutex 不可重入，直接调 Discard
// 会自死锁）。
func (m *WorktreeManager) discardLocked(ctx context.Context, mirror, path, branch string) {
	if path != "" {
		if _, err := m.git(ctx, mirror, "worktree", "remove", "--force", path); err != nil {
			slog.Warn("丢弃工作区失败（继续清理）", "path", path, "err", err)
		}
		// 目录可能不是注册的 worktree（手工建的/半残的），git 拒绝删除；
		// 兜底直接删目录，否则下一步 worktree add 还会撞「目录已存在」。
		if _, err := os.Stat(path); err == nil {
			if rerr := os.RemoveAll(path); rerr != nil {
				slog.Warn("删除工作区目录失败（继续）", "path", path, "err", rerr)
			}
		}
	}
	_, _ = m.git(ctx, mirror, "worktree", "prune")
	if branch != "" {
		if _, err := m.git(ctx, mirror, "branch", "-D", branch); err != nil {
			slog.Warn("删除残留分支失败（继续）", "branch", branch, "err", err)
		}
	}
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

// worktreeDirName 生成工作区目录名：优先用 issue 编号，保证一眼能看出归属。
func worktreeDirName(issueKey, branch string) string {
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
