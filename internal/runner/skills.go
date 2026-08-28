package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Clouditera/lathe/skills"
)

// StageSkillMissing 是节点画像（tasks.profile 的 skills 字段，F7.1）声明的
// 技能在 Lathe 自己的技能仓库里找不到（名字或版本任一对不上）时的失败阶段
// 代码。对应 F7.2-AC5：声明了不存在的技能时任务以可读原因失败，不静默忽略。
//
// 与 profile.go 的 StageProfileInvalid、pipeline.go 的 StageRebaseConflict
// 同理，不进 stage.go 的 label()/stageOrder() 表（两者的 default 分支已经
// 兜得住未列出的 Stage），单独放在离消费方最近的文件里。
const StageSkillMissing Stage = "skill_missing"

// materializeSkills 把 rc.profile.Skills 声明的每个技能物化进 worktree 的
// .claude/skills/<name>/，并把 .claude/skills/ 排进 worktree 的
// .git/info/exclude，不让它进 git status/diff/add -A（F7.2-AC3）。
//
// 无条件对每个声明的技能调用一次，不区分全新执行与断点续跑：曾经有
// fresh==false（续跑）时直接跳过的"优化"，但 PlanRetry
// 只按【失败阶段 + worktree 现场体检】决策续跑入口，并不知道"技能是否
// 已经物化过"——技能缺失导致的首次失败发生在 worktree 已建出、但物化/
// 校验之前，重试时现场体检看到"worktree 存在"就会判定为续跑
// （EntryImplement），届时 fresh 恒为 false，校验会被永久跳过，技能仍然
// 缺失却带着任务继续往下走，这正是 F7.2-AC5 禁止的"静默忽略"。物化操作
// 本身是幂等的（Materialize 对同一 name/version 反复调用产出内容完全
// 相同，目标目录已存在也不报错），无条件重跑的代价可忽略，换来"技能缺失
// 不可能被任何执行路径绕过"这个更强的正确性保证。
func (p *Pipeline) materializeSkills(rc *runCtx) error {
	if rc.profile == nil || len(rc.profile.Skills) == 0 {
		return nil
	}

	for _, ref := range rc.profile.Skills {
		dest := filepath.Join(rc.wt.Path, ".claude", "skills", ref.Name)
		if err := skills.Materialize(ref.Name, ref.Version, dest); err != nil {
			return fmt.Errorf("技能 %s@%s 物化失败: %w", ref.Name, ref.Version, err)
		}
	}
	return excludeSkillsDir(rc.wt.Path)
}

// excludeSkillsDir 往 worktree 的 .git/info/exclude 追加 ".claude/skills/"
// 一行，让物化进 worktree 的技能文件对 git 不可见——git status/diff/add -A
// 都看不到它们，stageCommit 完全不需要感知这件事（B2-3 同类教训：靠 commit
// 阶段记得排除是脆的，容易在将来被绕开，见 docs/06-orchestration.md §6.2(b)）。
//
// worktree 的 .git 是一个指向 mirror 内 worktrees/<name> 目录的文件，不是
// 真正的 git 目录，因此不能直接拼 <worktree>/.git/info/exclude——那样会把
// ".git" 当目录用，在真实 worktree 上必定失败。用 `git rev-parse
// --git-path info/exclude` 让 git 自己解析出实际路径（info/exclude 是
// worktree 间共享的，通常落在 mirror 自己的 git 目录下；这样同一个 mirror
// 下的其它 worktree 也自动受益，不需要逐个 worktree 重复追加）。
func excludeSkillsDir(worktreePath string) error {
	const rule = ".claude/skills/"

	out, err := exec.Command("git", "-C", worktreePath, "rev-parse", "--git-path", "info/exclude").Output()
	if err != nil {
		return fmt.Errorf("解析 worktree 的 git 目录失败: %w", err)
	}
	excludePath := strings.TrimSpace(string(out))
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(worktreePath, excludePath)
	}

	existing, err := os.ReadFile(excludePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("读取 %s 失败: %w", excludePath, err)
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == rule {
			return nil // 已经排除过（可能是同 mirror 下更早的任务加的），不重复加
		}
	}

	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", filepath.Dir(excludePath), err)
	}

	var content strings.Builder
	content.Write(existing)
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		content.WriteByte('\n')
	}
	content.WriteString(rule)
	content.WriteByte('\n')

	if err := os.WriteFile(excludePath, []byte(content.String()), 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", excludePath, err)
	}
	return nil
}
