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
	"unicode/utf8"
	"sync"
	"time"
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

	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err == nil {
		// 已存在：只更新
		if _, err := m.git(ctx, mirror, "fetch", "--prune", "--quiet", "origin"); err != nil {
			return "", fmt.Errorf("runner: 更新 mirror %s 失败: %w", providerRepo, err)
		}
		return mirror, nil
	}

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
	if _, err := m.git(ctx, mirror, "config", "remote.origin.fetch", "+refs/heads/*:refs/heads/*"); err != nil {
		return "", fmt.Errorf("runner: 设置 fetch refspec 失败: %w", err)
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

// ChangedFiles 列出任务分支相对基线的改动文件（新增/修改，相对路径）。
//
// 删除的文件被排除：档位路由与复现测试识别都只关心"现在存在什么"，
// 删掉的文件既不能跑也不能拷。
func (m *WorktreeManager) ChangedFiles(ctx context.Context, wt *Worktree) ([]string, error) {
	out, err := m.git(ctx, wt.Path, "diff", "--name-only", "--diff-filter=AM", wt.BaseBranch+"...HEAD")
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
	if _, err := m.git(ctx, mirror, "worktree", "add", "--quiet", "--detach", path, base); err != nil {
		return nil, fmt.Errorf("runner: 创建基线工作区失败（基线 %s）: %w", base, err)
	}
	return &Worktree{Path: path, Branch: "(detached)", BaseBranch: base, Mirror: mirror}, nil
}

// Worktree 是一个已创建的任务工作区。
type Worktree struct {
	Path       string // 工作区绝对路径
	Branch     string // 新建的任务分支
	BaseBranch string // 分叉基线
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

	// 基线分支必须真实存在，否则 worktree add 会报出难懂的错
	if _, err := m.git(ctx, mirror, "rev-parse", "--verify", "--quiet", base+"^{commit}"); err != nil {
		return nil, fmt.Errorf("runner: 基线分支 %q 在仓库 %s 中不存在", base, p.Repo.ProviderRepo)
	}

	path := filepath.Join(m.root, worktreeDirName(p.IssueKey, branch))
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("runner: 工作区 %s 已存在（上一任务未回收？）", path)
	}

	if _, err := m.git(ctx, mirror, "worktree", "add", "--quiet", "-b", branch, path, base); err != nil {
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
	if path != "" {
		if _, err := m.git(ctx, mirror, "worktree", "remove", "--force", path); err != nil {
			slog.Warn("丢弃工作区失败（继续清理）", "path", path, "err", err)
		}
	}
	_, _ = m.git(ctx, mirror, "worktree", "prune")
	if branch != "" {
		if _, err := m.git(ctx, mirror, "branch", "-D", branch); err != nil {
			slog.Warn("删除残留分支失败（继续）", "branch", branch, "err", err)
		}
	}
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
