package runner

// teamskills.go 团队自定义技能（f11，docs/features/team-skills.md）的流水线侧：
// 钉版本、物化进 worktree 的 .claude/skills/team-<slug>/、让它对 git 不可见、
// 提交前复位。
//
// 与产品技能（skills.go 的 materializeSkills）是两条独立路径，各自对自己写的
// 文件负责：
//
//   - 产品技能写 .claude/skills/<name>/，靠 excludeSkillsDir 写 mirror 共享的
//     info/exclude 隐藏（既有行为，本文件不调用、不修改它）；
//   - 团队技能写 .claude/skills/team-<slug>/，靠**目录内哨兵** .gitignore（内容
//     "*\n"）隐藏：作用域严格限于这个目录，不动任何共享文件，不影响同 mirror 的
//     其他 worktree、其他用户（PRD §3.5「受影响范围」）。
//
// 为什么隐藏是硬要求：物化文件若对 git 可见，HasChanges 会被它骗成真 ——
// 「工作区无改动」判死（StageImplementNoChanges，验证体系里「agent 没干活」的
// 唯一闸）就被技能文件本身满足，Commit 的 add -A 还会把它带进 PR。
//
// 三道检查（任一不过都以 StageTeamSkillFailed 失败，在驱动 agent 之前拦下）：
//
//  1. 路径体检 checkTeamSkillPath：.claude、.claude/skills、dest 逐级 Lstat，
//     有符号链接或非目录就拒绝；再用不带尾斜杠的 git ls-files 检查这些路径及
//     dest 下有没有被目标仓库跟踪的条目。动任何文件之前完成，所以 RemoveAll
//     不会顺着链接删到目标仓库的文件。
//  2. 哨兵：先写哨兵、后写内容，任何时刻都不存在「文件在、哨兵不在」的窗口。
//  3. 写后自检：物化前后 HasChanges 的结论与完整 git status 明细（不带 pathspec，
//     与判死同源）必须不变。防的是实现被改坏与还没想到的布局。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/teamskills"
)

// StageTeamSkillFailed 是团队技能钉版本 / 物化 / 提交前复位失败的阶段代码。
// 中文标签在 stage.go 的 label()；重试策略在 retry.go 的 PlanRetry（必须回到
// EntryImplement 重新物化，见那里的注释）。
const StageTeamSkillFailed Stage = "team_skill_failed"

// TeamSkillSource 是流水线读写团队技能的窄接口。*store.Store 直接满足它
// （与 FailureMemory 同一形态，不需要适配器）。
type TeamSkillSource interface {
	// ListEnabledTeamSkills 返回仓库当前启用（enabled 且未归档）的技能及最新版本。
	ListEnabledTeamSkills(ctx context.Context, repoID, userID int64) ([]store.TeamSkillPin, error)
	// TeamSkillPinsLoad 读任务钉住的技能；ok=false 表示还没钉。
	TeamSkillPinsLoad(ctx context.Context, taskID int64) ([]store.TeamSkillPin, bool, error)
	// TeamSkillPinsSave 钉住任务的技能版本（先写者赢）。
	TeamSkillPinsSave(ctx context.Context, taskID, userID, repoID int64, pins []store.TeamSkillPin) error
	// TeamSkillPinsClear 清掉任务的钉（从头重建时）。
	TeamSkillPinsClear(ctx context.Context, taskID int64) error
	// TeamSkillVersionFiles 读钉住版本的文件内容（按属主限定）。
	TeamSkillVersionFiles(ctx context.Context, userID, skillID int64, version int) ([]teamskills.File, error)
}

// sentinelContent 是哨兵 .gitignore 的内容：匹配目录下全部条目（包括它自己）。
const sentinelContent = "*\n"

// 测试钩子（只在包内测试里改写，用来证明后两道检查各自是真闸，见 AC-10 的
// 故障注入用例）。生产代码从不修改它们。
var (
	teamSkillSkipPathCheck = false
	teamSkillSentinelBody  = sentinelContent
)

// maxSnapshotDiffPaths 是自检失败原因里最多列出的路径数。
const maxSnapshotDiffPaths = 20

// teamSkillRel 返回团队技能目录相对 worktree 根的路径（以 / 分隔）。
func teamSkillRel(dirName string) string { return ".claude/skills/" + dirName }

// prepareTeamSkills 是 stageImplement 里的钉版本 + 物化入口（在产品技能物化
// 之后、驱动 agent 之前）。任何一步出错都 p.fail(StageTeamSkillFailed) —— 读
// 失败不等于「没钉过」，写失败不能带着未落库的钉继续跑（否则下一次续跑会按
// 「还没钉」重新取最新版，可复现性就没了）。
func (p *Pipeline) prepareTeamSkills(rc *runCtx) error {
	if p.TeamSkills == nil {
		return nil // 功能关闭：行为与引入本功能之前完全一致
	}
	ctx := rc.ctx
	if rc.plan != nil && rc.plan.Fresh {
		// 从头重建 = 用当前最新版重钉。全新任务没有钉行，Clear 是 no-op。
		if err := p.TeamSkills.TeamSkillPinsClear(ctx, rc.tk.ID); err != nil {
			return p.fail(rc, StageTeamSkillFailed, err)
		}
	}
	pins, ok, err := p.TeamSkills.TeamSkillPinsLoad(ctx, rc.tk.ID)
	if err != nil {
		return p.fail(rc, StageTeamSkillFailed, err)
	}
	if !ok {
		active, err := p.TeamSkills.ListEnabledTeamSkills(ctx, rc.tk.RepoID, rc.tk.UserID)
		if err != nil {
			return p.fail(rc, StageTeamSkillFailed, err)
		}
		// 一行；pinned=[] 也算「钉过了」。user_id/repo_id 只取自任务行。
		if err := p.TeamSkills.TeamSkillPinsSave(ctx, rc.tk.ID, rc.tk.UserID, rc.tk.RepoID, active); err != nil {
			return p.fail(rc, StageTeamSkillFailed, err)
		}
		// 再读一次：先写者赢，以库里那份为准（同一任务不会并发执行，这里只是
		// 不让内存里的值与库里的值有机会不同）。
		pins, ok, err = p.TeamSkills.TeamSkillPinsLoad(ctx, rc.tk.ID)
		if err != nil {
			return p.fail(rc, StageTeamSkillFailed, err)
		}
		if !ok {
			return p.fail(rc, StageTeamSkillFailed, errors.New("钉住的技能版本写入后读不回来"))
		}
	}
	if err := p.materializeTeamSkills(ctx, rc.tk.UserID, rc.wt, pins); err != nil {
		return p.fail(rc, StageTeamSkillFailed, err)
	}
	rc.teamPins = pins
	return nil
}

// materializeTeamSkills 把钉住的版本写进 <wt>/.claude/skills/<pin.Slug>/。
// 不依赖画像、不依赖 materializeSkills 是否跑过、不调用 excludeSkillsDir。
func (p *Pipeline) materializeTeamSkills(ctx context.Context, userID int64, wt *Worktree, pins []store.TeamSkillPin) error {
	if len(pins) == 0 {
		return nil // 不落任何文件、不写任何规则
	}

	// 1) 全部 pin 先体检、先取内容，再动任何文件。
	contents := make([][]teamskills.File, len(pins))
	for i, pin := range pins {
		if err := validPinDir(pin.Slug); err != nil {
			return err
		}
		if !teamSkillSkipPathCheck {
			if err := p.checkTeamSkillPath(ctx, wt.Path, pin.Slug); err != nil {
				return err
			}
		}
		files, err := p.TeamSkills.TeamSkillVersionFiles(ctx, userID, pin.SkillID, pin.Version)
		if err != nil {
			if errors.Is(err, store.ErrTeamSkillVersionNotFound) {
				return fmt.Errorf("钉住的团队技能 %s@%d 的版本不存在（可能被人工删除），不能静默跳过", pin.Slug, pin.Version)
			}
			return fmt.Errorf("读取团队技能 %s@%d 失败: %w", pin.Slug, pin.Version, err)
		}
		// 库里的内容在保存时已校验过；这里再过一遍，是不信任库里的数据也会
		// 被直接改写（路径穿越的最后一道）。
		if _, _, err := teamskills.ValidateFiles(files); err != nil {
			return fmt.Errorf("团队技能 %s@%d 的内容不合法: %w", pin.Slug, pin.Version, err)
		}
		if pin.SHA256 != "" && teamskills.ContentSHA256(files) != pin.SHA256 {
			return fmt.Errorf("团队技能 %s@%d 的内容与钉住时的摘要不一致，拒绝物化", pin.Slug, pin.Version)
		}
		contents[i] = files
	}

	// 2) 现场里已有的团队技能目录（上一轮留下的）先补回哨兵，再拍「物化前」快照。
	// 上一轮若在 agent 删掉哨兵后、提交前复位之前失败，目录内容此刻对 git 可见；
	// 不先补回，物化（会重新写哨兵）就会让快照「变了」，把一次正常的续跑误判死。
	for _, pin := range pins {
		if err := p.restoreSentinel(wt.Path, pin.Slug); err != nil {
			return err
		}
	}
	before, err := p.statusSnapshot(ctx, wt)
	if err != nil {
		return err
	}

	// 3) 先清再写：哨兵先于内容。
	for i, pin := range pins {
		if err := writeTeamSkill(filepath.Join(wt.Path, filepath.FromSlash(teamSkillRel(pin.Slug))), pin.Slug, contents[i]); err != nil {
			return fmt.Errorf("物化团队技能 %s@%d 失败: %w", pin.Slug, pin.Version, err)
		}
	}

	// 4) 写后自检：与 HasChanges 同源、不带 pathspec。
	after, err := p.statusSnapshot(ctx, wt)
	if err != nil {
		return err
	}
	if diff := before.diff(after); diff != "" {
		return fmt.Errorf("团队技能物化改变了工作区的 git 状态（物化文件对 git 可见），拒绝继续："+
			"会混进 PR 并骗过『工作区无改动』判定。变化：%s", diff)
	}
	return nil
}

// validPinDir 防御性地确认钉行里的目录名形如 team-<合法 slug>（钉行来自库，
// 不信任它没被改写过）。
func validPinDir(dir string) error {
	slug, ok := strings.CutPrefix(dir, teamskills.DirPrefix)
	if !ok {
		return fmt.Errorf("钉住的团队技能目录名 %q 不合法", dir)
	}
	if got, err := teamskills.NormalizeSlug(slug); err != nil || got != slug {
		return fmt.Errorf("钉住的团队技能目录名 %q 不合法", dir)
	}
	return nil
}

// writeTeamSkill 清空 dest 后按「哨兵 → 内容 → 改写 name」的顺序写入。
// 调用前 dest 及其父路径已通过 checkTeamSkillPath（真实目录或不存在）。
func writeTeamSkill(dest, dirName string, files []teamskills.File) error {
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	if err := writeNew(filepath.Join(dest, teamskills.SentinelFile), teamSkillSentinelBody); err != nil {
		return err
	}
	for _, f := range files {
		content := f.Content
		if f.Path == teamskills.SkillFile {
			c, err := teamskills.RewriteName(content, dirName)
			if err != nil {
				return err
			}
			content = c
		}
		target := filepath.Join(dest, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := writeNew(target, content); err != nil {
			return err
		}
	}
	return nil
}

// writeNew 以 O_EXCL 新建文件：目录刚被清空，任何已存在的同名条目都说明出了
// 意料之外的事，宁可失败也不覆盖。
func writeNew(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// checkTeamSkillPath 是物化与复位前的路径体检（PRD §3.5 r5）。
func (p *Pipeline) checkTeamSkillPath(ctx context.Context, wtPath, dirName string) error {
	rel := teamSkillRel(dirName)
	for _, sub := range []string{".claude", ".claude/skills", rel} {
		fi, err := os.Lstat(filepath.Join(wtPath, filepath.FromSlash(sub)))
		if errors.Is(err, os.ErrNotExist) {
			continue // 不存在的路径之后会由 MkdirAll 建成真实目录
		}
		if err != nil {
			return fmt.Errorf("检查路径 %s 失败: %w", sub, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("路径 %s 是符号链接，团队技能不跟随链接写入", sub)
		}
		if !fi.IsDir() {
			return fmt.Errorf("路径 %s 不是目录，团队技能无法写入", sub)
		}
	}
	// 不带尾斜杠：带尾斜杠的 pathspec 匹配不到「被跟踪的链接本身」。
	out, err := p.Worktrees.git(ctx, wtPath, "ls-files", "-z", "--", ".claude", ".claude/skills", rel)
	if err != nil {
		return fmt.Errorf("检查目标仓库是否跟踪了团队技能路径失败: %w", err)
	}
	for _, e := range strings.Split(out, "\x00") {
		switch {
		case e == "":
		case e == ".claude" || e == ".claude/skills":
			return fmt.Errorf("目标仓库跟踪了 %s 本身（链接、普通文件或子模块），团队技能不写入", e)
		case e == rel:
			return fmt.Errorf("目标仓库跟踪了 %s 本身，团队技能不覆盖", rel)
		case strings.HasPrefix(e, rel+"/"):
			return fmt.Errorf("目标仓库跟踪了 %s 下的文件（%s），团队技能目录属于 Lathe，不覆盖目标仓库的文件",
				rel, truncateRunes(e, 200))
		}
	}
	return nil
}

// restoreSentinel 在 dest 是真实目录时把哨兵重写成 "*\n"（被删就补、被改就盖）。
// 哨兵被换成链接或目录时先删掉它再新建普通文件；以 O_NOFOLLOW 打开，不跟随链接。
// 调用前 dest 及父路径已通过 checkTeamSkillPath。
func (p *Pipeline) restoreSentinel(wtPath, dirName string) error {
	dest := filepath.Join(wtPath, filepath.FromSlash(teamSkillRel(dirName)))
	fi, err := os.Lstat(dest)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("检查 %s 失败: %w", teamSkillRel(dirName), err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("路径 %s 不是目录", teamSkillRel(dirName))
	}
	sentinel := filepath.Join(dest, teamskills.SentinelFile)
	if si, err := os.Lstat(sentinel); err == nil && !si.Mode().IsRegular() {
		if err := os.RemoveAll(sentinel); err != nil {
			return fmt.Errorf("移除异常的哨兵 %s 失败: %w", teamSkillRel(dirName)+"/"+teamskills.SentinelFile, err)
		}
	}
	f, err := os.OpenFile(sentinel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return fmt.Errorf("写入哨兵失败: %w", err)
	}
	if _, err := f.WriteString(teamSkillSentinelBody); err != nil {
		_ = f.Close()
		return fmt.Errorf("写入哨兵失败: %w", err)
	}
	return f.Close()
}

// hideTeamSkills 是提交前的复位（stageCommit 与修复轮各自的 HasChanges 之前）：
// 哨兵在 worktree 里，agent 能删能改，所以在每一个以 HasChanges 决定是否提交的
// 点之前，把它补回来，并用完整 status 自检被钉住的目录没有任何条目可见。
//
// 这里的自检不能用「前后快照相等」：复位本来就会让状态变化（被删掉的哨兵所
// 隐藏的文件从 ?? 列表里消失），那正是它要达到的效果。
func (p *Pipeline) hideTeamSkills(rc *runCtx) error {
	if p.TeamSkills == nil || rc.wt == nil {
		return nil
	}
	pins := rc.teamPins
	if pins == nil {
		// EntryCommit/EntryVerify 续跑没经过 stageImplement，从库里取。
		loaded, ok, err := p.TeamSkills.TeamSkillPinsLoad(rc.ctx, rc.tk.ID)
		if err != nil {
			return fmt.Errorf("读取任务钉住的团队技能失败: %w", err)
		}
		if !ok {
			return nil
		}
		pins = loaded
	}
	if len(pins) == 0 {
		return nil
	}
	for _, pin := range pins {
		if err := validPinDir(pin.Slug); err != nil {
			return err
		}
		// agent 可能把 dest 或父路径换成了链接；跟随链接写哨兵就会写到
		// worktree 外或目标仓库的文件里。
		if err := p.checkTeamSkillPath(rc.ctx, rc.wt.Path, pin.Slug); err != nil {
			return err
		}
		if err := p.restoreSentinel(rc.wt.Path, pin.Slug); err != nil {
			return err
		}
	}
	out, err := p.Worktrees.git(rc.ctx, rc.wt.Path, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("复位团队技能后检查工作区失败: %w", err)
	}
	for _, path := range statusPaths(out) {
		for _, pin := range pins {
			rel := teamSkillRel(pin.Slug)
			if path == rel || strings.HasPrefix(path, rel+"/") {
				return fmt.Errorf("复位后团队技能目录仍对 git 可见（%s），拒绝提交：会把技能文件混进 PR",
					truncateRunes(path, 200))
			}
		}
	}
	return nil
}

// gitSnapshot 是写后自检比较的两项：HasChanges 的结论（与判死同一个函数）与
// 同一棵工作树的完整 status 明细（不带 pathspec）。
type gitSnapshot struct {
	changed bool
	paths   map[string]bool
	raw     string
}

func (p *Pipeline) statusSnapshot(ctx context.Context, wt *Worktree) (gitSnapshot, error) {
	changed, err := p.Worktrees.HasChanges(ctx, wt)
	if err != nil {
		return gitSnapshot{}, err
	}
	out, err := p.Worktrees.git(ctx, wt.Path, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return gitSnapshot{}, fmt.Errorf("读取工作区状态失败: %w", err)
	}
	s := gitSnapshot{changed: changed, raw: out, paths: map[string]bool{}}
	for _, e := range statusEntries(out) {
		s.paths[e] = true
	}
	return s, nil
}

// diff 返回两份快照的差异描述；相同时返回空串。
func (s gitSnapshot) diff(after gitSnapshot) string {
	if s.changed == after.changed && s.raw == after.raw {
		return ""
	}
	var items []string
	for e := range after.paths {
		if !s.paths[e] {
			items = append(items, "新增 "+truncateRunes(e, 200))
		}
	}
	for e := range s.paths {
		if !after.paths[e] {
			items = append(items, "消失 "+truncateRunes(e, 200))
		}
	}
	if len(items) == 0 {
		items = append(items, fmt.Sprintf("HasChanges %v → %v", s.changed, after.changed))
	}
	sort.Strings(items)
	if len(items) > maxSnapshotDiffPaths {
		items = append(items[:maxSnapshotDiffPaths], fmt.Sprintf("…另有 %d 项", len(items)-maxSnapshotDiffPaths))
	}
	return strings.Join(items, "；")
}

// statusEntries 把 `git status --porcelain=v1 -z` 的输出拆成条目（"XY path"，
// 改名/复制条目带上原路径），用于前后比较。
func statusEntries(out string) []string {
	var entries []string
	toks := strings.Split(out, "\x00")
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if len(t) < 4 {
			continue
		}
		e := t
		if t[0] == 'R' || t[0] == 'C' {
			if i+1 < len(toks) {
				e += " <- " + toks[i+1]
				i++
			}
		}
		entries = append(entries, e)
	}
	return entries
}

// statusPaths 返回 status 输出里出现的全部路径（含改名的原路径）。
func statusPaths(out string) []string {
	var paths []string
	toks := strings.Split(out, "\x00")
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if len(t) < 4 {
			continue
		}
		paths = append(paths, t[3:])
		if (t[0] == 'R' || t[0] == 'C') && i+1 < len(toks) {
			paths = append(paths, toks[i+1])
			i++
		}
	}
	return paths
}

// truncateRunes 按 rune 截断（不切断多字节字符，事故 54075f4）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
