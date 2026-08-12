package runner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	if _, err := m.git(ctx, wt.Mirror, args...); err != nil {
		return fmt.Errorf("runner: 回收工作区 %s 失败: %w", wt.Path, err)
	}
	// 顺带删掉任务分支（已合并进 PR，本地副本无保留价值）
	if force {
		_, _ = m.git(ctx, wt.Mirror, "branch", "-D", wt.Branch)
	}
	return nil
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
	return s[:n] + "…(已截断)"
}
