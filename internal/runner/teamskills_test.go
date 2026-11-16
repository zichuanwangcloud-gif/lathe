package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
	"github.com/zichuanwangcloud-gif/lathe/internal/teamskills"
	skillsembed "github.com/zichuanwangcloud-gif/lathe/skills"
)

// ---------------------------------------------------------------- 假件与夹具

// memTeamSkills 是内存版 TeamSkillSource：AC-10 的单元测试只测物化与隐藏，
// 技能内容不走数据库（钉版本与库交互由 pipeline_teamskills_test.go 端到端覆盖）。
// 例外是 prepareOnRealTask：失败阶段要经真实 p.fail 落到任务行上才能断言。
type memTeamSkills struct {
	versions map[int64]map[int][]teamskills.File // skillID -> version -> files
	pins     map[int64][]store.TeamSkillPin
	enabled  []store.TeamSkillPin
}

func newMemTeamSkills() *memTeamSkills {
	return &memTeamSkills{versions: map[int64]map[int][]teamskills.File{}, pins: map[int64][]store.TeamSkillPin{}}
}

func (m *memTeamSkills) ListEnabledTeamSkills(ctx context.Context, repoID, userID int64) ([]store.TeamSkillPin, error) {
	return m.enabled, nil
}
func (m *memTeamSkills) TeamSkillPinsLoad(ctx context.Context, taskID int64) ([]store.TeamSkillPin, bool, error) {
	p, ok := m.pins[taskID]
	return p, ok, nil
}
func (m *memTeamSkills) TeamSkillPinsSave(ctx context.Context, taskID, userID, repoID int64, pins []store.TeamSkillPin) error {
	if _, ok := m.pins[taskID]; !ok {
		m.pins[taskID] = append([]store.TeamSkillPin{}, pins...)
	}
	return nil
}
func (m *memTeamSkills) TeamSkillPinsClear(ctx context.Context, taskID int64) error {
	delete(m.pins, taskID)
	return nil
}
func (m *memTeamSkills) TeamSkillVersionFiles(ctx context.Context, userID, skillID int64, version int) ([]teamskills.File, error) {
	if f, ok := m.versions[skillID][version]; ok {
		return f, nil
	}
	return nil, store.ErrTeamSkillVersionNotFound
}

// put 登记一个版本并返回对应的 pin。
func (m *memTeamSkills) put(skillID int64, slug string, version int, files []teamskills.File) store.TeamSkillPin {
	if m.versions[skillID] == nil {
		m.versions[skillID] = map[int][]teamskills.File{}
	}
	m.versions[skillID][version] = files
	return store.TeamSkillPin{SkillID: skillID, Slug: teamskills.DirPrefix + slug, Version: version, SHA256: teamskills.ContentSHA256(files)}
}

const acmeSkillMD = "---\nname: acme-testing\ndescription: 团队测试规范\nlicense: MIT\n---\n\n# 团队测试规范\n\n表驱动。\n"

func acmeFiles(extra ...teamskills.File) []teamskills.File {
	return append([]teamskills.File{{Path: "SKILL.md", Content: acmeSkillMD}}, extra...)
}

// tsGit 在 dir 里跑 git，失败即 Fatal，返回 stdout。
func tsGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=t@e.st",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=t@e.st")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, ee.Stderr)
		}
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

// tsWorktree 造一个与生产同形的工作区（bare mirror + worktree add）。
// layout 在源仓库提交前执行，用来布置「目标仓库跟踪了什么」。
func tsWorktree(t *testing.T, layout func(src string)) (*Pipeline, *Worktree, *memTeamSkills) {
	t.Helper()
	src := goSourceRepo(t)
	if layout != nil {
		layout(src)
		tsGit(t, src, "add", "-A")
		tsGit(t, src, "commit", "--quiet", "--allow-empty", "-m", "布置")
		tsGit(t, src, "branch", "-f", "dev")
	}
	wm, err := NewWorktreeManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := DefaultRepoConfig("acme/ts-unit")
	wt, err := wm.Create(context.Background(), CreateParams{
		Repo: repo, CloneURL: src, Kind: KindFix, IssueKey: "TS-1", Title: "t", TaskID: 1,
	})
	if err != nil {
		t.Fatalf("建工作区失败: %v", err)
	}
	mem := newMemTeamSkills()
	return &Pipeline{Worktrees: wm, TeamSkills: mem}, wt, mem
}

func statusAll(t *testing.T, wt *Worktree) string {
	t.Helper()
	return tsGit(t, wt.Path, "status", "--porcelain", "--untracked-files=all")
}

func excludeSHA(t *testing.T, wt *Worktree) string {
	t.Helper()
	p := strings.TrimSpace(tsGit(t, wt.Path, "rev-parse", "--git-path", "info/exclude"))
	if !filepath.IsAbs(p) {
		p = filepath.Join(wt.Path, p)
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return "<absent>"
	}
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func readT(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", p, err)
	}
	return string(b)
}

func teamDir(wt *Worktree) string {
	return filepath.Join(wt.Path, ".claude", "skills", "team-acme-testing")
}

// ---------------------------------------------------------------- AC-10

// 团队技能物化不碰产品技能：go-testing 与嵌入内容逐字节一致。
func TestTeamSkillMaterialize_DoesNotTouchProductSkill(t *testing.T) {
	p, wt, mem := tsWorktree(t, nil)
	productDir := filepath.Join(wt.Path, ".claude", "skills", "go-testing")
	if err := skillsembed.Materialize("go-testing", "1.0.0", productDir); err != nil {
		t.Fatal(err)
	}
	pin := mem.put(1, "acme-testing", 1, acmeFiles())
	if err := p.materializeTeamSkills(context.Background(), 1, wt, []store.TeamSkillPin{pin}); err != nil {
		// 产品技能目录没有被隐藏（这里没调 excludeSkillsDir），快照前后都在 ?? 里，
		// 不影响相等判断 —— 物化必须成功。
		t.Fatalf("物化失败: %v", err)
	}
	got := readT(t, filepath.Join(productDir, "SKILL.md"))
	want := readT(t, filepath.Join(skillsSourceDir(t), "go-testing", "1.0.0", "SKILL.md"))
	if got != want {
		t.Error("go-testing/SKILL.md 与嵌入内容不一致")
	}
	if _, err := os.Stat(filepath.Join(teamDir(wt), "SKILL.md")); err != nil {
		t.Errorf("团队技能应在 team-acme-testing/: %v", err)
	}
}

// 落盘 SKILL.md 只改 name 一行。
func TestTeamSkillMaterialize_RewritesFrontmatterName(t *testing.T) {
	p, wt, mem := tsWorktree(t, nil)
	pin := mem.put(1, "acme-testing", 1, acmeFiles())
	if err := p.materializeTeamSkills(context.Background(), 1, wt, []store.TeamSkillPin{pin}); err != nil {
		t.Fatal(err)
	}
	got := readT(t, filepath.Join(teamDir(wt), "SKILL.md"))
	want := strings.Replace(acmeSkillMD, "name: acme-testing", "name: team-acme-testing", 1)
	if got != want {
		t.Errorf("SKILL.md got %q, want %q", got, want)
	}
}

// 先清再写：v2 删掉的文件在重新物化后不存在。
func TestTeamSkillMaterialize_RemovesStaleFiles(t *testing.T) {
	p, wt, mem := tsWorktree(t, nil)
	v1 := mem.put(1, "acme-testing", 1, acmeFiles(teamskills.File{Path: "references/a.md", Content: "a"}))
	if err := p.materializeTeamSkills(context.Background(), 1, wt, []store.TeamSkillPin{v1}); err != nil {
		t.Fatal(err)
	}
	v2 := mem.put(1, "acme-testing", 2, acmeFiles())
	if err := p.materializeTeamSkills(context.Background(), 1, wt, []store.TeamSkillPin{v2}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(teamDir(wt), "references", "a.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("v2 已删掉的 references/a.md 仍存在（err=%v）", err)
	}
}

// 目标仓库跟踪了团队技能路径 → 拒绝且不改写该文件。
func TestTeamSkillMaterialize_RefusesTrackedPath(t *testing.T) {
	cases := []struct {
		name string
		path string
		body string
	}{
		{"跟踪了SKILL.md", ".claude/skills/team-acme-testing/SKILL.md", "仓库自己的\n"},
		{"只跟踪了哨兵位置的gitignore", ".claude/skills/team-acme-testing/.gitignore", "!keep\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, wt, mem := tsWorktree(t, func(src string) { writeFile(t, filepath.Join(src, c.path), c.body) })
			pin := mem.put(1, "acme-testing", 1, acmeFiles())
			err := p.materializeTeamSkills(context.Background(), 1, wt, []store.TeamSkillPin{pin})
			if err == nil {
				t.Fatal("目标仓库跟踪了该路径，物化应失败")
			}
			if got := readT(t, filepath.Join(wt.Path, c.path)); got != c.body {
				t.Errorf("被跟踪的文件被改写了：got %q, want %q", got, c.body)
			}
		})
	}
}

// r5：路径上有符号链接 → 拒绝，链接与链接目标都不变，完整 status 不变。
func TestTeamSkillMaterialize_RefusesSymlinkedPath(t *testing.T) {
	outside := t.TempDir()
	cases := []struct {
		name    string
		layout  func(t *testing.T, src string)
		plant   func(t *testing.T, wt *Worktree) // 物化前在现场布置（未跟踪的污染）
		link    string                           // 断言 Readlink 不变的链接（相对 wt）
		target  string                           // 断言内容不变的目标文件（相对 wt；空=不查）
		wantErr bool
	}{
		{
			name: "①被跟踪的链接.claude/skills指向.agents/skills",
			layout: func(t *testing.T, src string) {
				writeFile(t, filepath.Join(src, ".agents/skills/team-acme-testing/SKILL.md"), "目标仓库的\n")
				mustSymlink(t, "../.agents/skills", filepath.Join(src, ".claude/skills"))
			},
			link: ".claude/skills", target: ".agents/skills/team-acme-testing/SKILL.md", wantErr: true,
		},
		{
			name: "②被跟踪的链接team-acme-testing指向other",
			layout: func(t *testing.T, src string) {
				writeFile(t, filepath.Join(src, "other/SKILL.md"), "other 的\n")
				mustSymlink(t, "../../other", filepath.Join(src, ".claude/skills/team-acme-testing"))
			},
			link: ".claude/skills/team-acme-testing", target: "other/SKILL.md", wantErr: true,
		},
		{
			name: "③未跟踪的链接.claude指向外部目录",
			plant: func(t *testing.T, wt *Worktree) {
				mustSymlink(t, outside, filepath.Join(wt.Path, ".claude"))
			},
			link: ".claude", wantErr: true,
		},
		{
			name: "④对照：只跟踪了兄弟目录team-acme-testingx",
			layout: func(t *testing.T, src string) {
				writeFile(t, filepath.Join(src, ".claude/skills/team-acme-testingx/SKILL.md"), "兄弟\n")
			},
		},
		{
			name: "⑤对照：跟踪了.claude/settings.json",
			layout: func(t *testing.T, src string) {
				writeFile(t, filepath.Join(src, ".claude/settings.json"), "{}\n")
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var layout func(string)
			if c.layout != nil {
				layout = func(src string) { c.layout(t, src) }
			}
			p, wt, mem := tsWorktree(t, layout)
			if c.plant != nil {
				c.plant(t, wt)
			}
			var linkBefore, targetBefore string
			if c.link != "" {
				linkBefore, _ = os.Readlink(filepath.Join(wt.Path, c.link))
			}
			if c.target != "" {
				targetBefore = readT(t, filepath.Join(wt.Path, c.target))
			}
			statusBefore := statusAll(t, wt)
			outsideBefore, _ := os.ReadDir(outside)

			pin := mem.put(1, "acme-testing", 1, acmeFiles())
			// 走生产入口 prepareTeamSkills + 真实 p.fail，断言错误落成的失败阶段。
			tk, err := prepareOnRealTask(t, p, mem, wt, []store.TeamSkillPin{pin})
			if c.wantErr && err == nil {
				t.Fatalf("应拒绝物化，实际成功；完整 status:\n%s", statusAll(t, wt))
			}
			if !c.wantErr && err != nil {
				t.Fatalf("对照格应成功（防误拦），got %v", err)
			}
			if c.wantErr {
				if tk.State != task.StateFailed || deref(tk.FailureStage) != string(StageTeamSkillFailed) {
					t.Errorf("流水线应落 failed/%s，got %s/%s", StageTeamSkillFailed, tk.State, deref(tk.FailureStage))
				}
			} else if tk.State != task.StateImplementing || tk.FailureStage != nil {
				t.Errorf("对照格任务应仍在 implementing、无失败阶段，got %s/%s", tk.State, deref(tk.FailureStage))
			}
			if c.link != "" {
				if got, _ := os.Readlink(filepath.Join(wt.Path, c.link)); got != linkBefore {
					t.Errorf("链接被改动：got %q, want %q", got, linkBefore)
				}
			}
			if c.target != "" {
				if got := readT(t, filepath.Join(wt.Path, c.target)); got != targetBefore {
					t.Errorf("链接目标内容被改写：got %q, want %q", got, targetBefore)
				}
			}
			if c.wantErr {
				if got := statusAll(t, wt); got != statusBefore {
					t.Errorf("完整 status 变了：\nbefore:\n%s\nafter:\n%s", statusBefore, got)
				}
			} else if strings.Contains(statusAll(t, wt), "team-acme-testing/") {
				t.Errorf("对照格物化后团队技能对 git 可见:\n%s", statusAll(t, wt))
			}
			if outsideAfter, _ := os.ReadDir(outside); len(outsideAfter) != len(outsideBefore) {
				t.Errorf("worktree 外的目录被写入：%d → %d 项", len(outsideBefore), len(outsideAfter))
			}
		})
	}
}

// prepareOnRealTask 让 prepareTeamSkills 跑在一条真实任务行上（真实 Machine，
// 任务处于 implementing —— 生产里物化发生的状态），返回调用结果与落库后的任务行。
// 失败路径走的是真实的 p.fail，所以返回的 FailureStage 就是流水线会落的阶段。
func prepareOnRealTask(t *testing.T, p *Pipeline, mem *memTeamSkills, wt *Worktree, pins []store.TeamSkillPin) (*task.Task, error) {
	t.Helper()
	e := newTSEnv(t, tsStore(t), "", nil)
	tk, err := e.m.Transition(context.Background(), e.taskID, task.StateImplementing, "test", nil)
	if err != nil {
		t.Fatalf("前置：queued -> implementing 失败: %v", err)
	}
	mem.enabled = pins
	p.Tasks = e.m
	rc := &runCtx{ctx: context.Background(), actor: "test", tk: tk, wt: wt}
	perr := p.prepareTeamSkills(rc)
	return e.final(t), perr
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// r5 故障注入：跳过路径体检，写后快照比较仍能拦下布局①。
func TestTeamSkillMaterialize_SnapshotSelfCheckCatchesOutOfScopeChange(t *testing.T) {
	p, wt, mem := tsWorktree(t, func(src string) {
		writeFile(t, filepath.Join(src, ".agents/skills/team-acme-testing/SKILL.md"), "目标仓库的\n")
		mustSymlink(t, "../.agents/skills", filepath.Join(src, ".claude/skills"))
	})
	teamSkillSkipPathCheck = true
	t.Cleanup(func() { teamSkillSkipPathCheck = false })

	pin := mem.put(1, "acme-testing", 1, acmeFiles())
	err := p.materializeTeamSkills(context.Background(), 1, wt, []store.TeamSkillPin{pin})
	if err == nil {
		t.Fatalf("快照自检应拦下；完整 status:\n%s", statusAll(t, wt))
	}
	if !strings.Contains(err.Error(), ".agents/skills/team-acme-testing/SKILL.md") {
		t.Errorf("原因应列出越界改动的路径，got %v", err)
	}
}

// r4 故障注入：哨兵被写成空文件 → 写后自检命中，原因含「对 git 可见」。
func TestTeamSkillMaterialize_SelfCheckIsARealGate(t *testing.T) {
	p, wt, mem := tsWorktree(t, nil)
	teamSkillSentinelBody = ""
	t.Cleanup(func() { teamSkillSentinelBody = sentinelContent })

	pin := mem.put(1, "acme-testing", 1, acmeFiles())
	err := p.materializeTeamSkills(context.Background(), 1, wt, []store.TeamSkillPin{pin})
	if err == nil || !strings.Contains(err.Error(), "对 git 可见") {
		t.Fatalf("写后自检应命中且原因含「对 git 可见」，got %v", err)
	}
}

// r4：隐藏来自目录内哨兵，不写 mirror 共享的 info/exclude。
func TestTeamSkillMaterialize_HidesWithSentinelNotSharedExclude(t *testing.T) {
	p, wt, mem := tsWorktree(t, nil)
	before := excludeSHA(t, wt)
	pin := mem.put(1, "acme-testing", 1, acmeFiles())
	if err := p.materializeTeamSkills(context.Background(), 1, wt, []store.TeamSkillPin{pin}); err != nil {
		t.Fatal(err)
	}
	if got := readT(t, filepath.Join(teamDir(wt), ".gitignore")); got != "*\n" {
		t.Errorf("哨兵内容 got %q, want %q", got, "*\n")
	}
	if got := statusAll(t, wt); got != "" {
		t.Errorf("物化后 status 应为空，got:\n%s", got)
	}
	if after := excludeSHA(t, wt); after != before {
		t.Errorf("共享 info/exclude 被改写：sha %s → %s", before, after)
	}
}

// r4：agent 在 .claude/skills/ 下新增的其他目录照常可见（会进 PR）。
func TestTeamSkillMaterialize_AgentAddedSkillStillVisible(t *testing.T) {
	p, wt, mem := tsWorktree(t, nil)
	pin := mem.put(1, "acme-testing", 1, acmeFiles())
	if err := p.materializeTeamSkills(context.Background(), 1, wt, []store.TeamSkillPin{pin}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt.Path, ".claude/skills/mine/SKILL.md"), "mine\n")
	if got, want := statusAll(t, wt), "?? .claude/skills/mine/SKILL.md\n"; got != want {
		t.Errorf("status got %q, want %q", got, want)
	}
}

// r4：目标仓库根 .gitignore 的任何否定形态都捞不回哨兵隐藏的文件。
func TestTeamSkillMaterialize_RootNegationCannotUnhide(t *testing.T) {
	for _, neg := range []string{
		"!.claude/skills/", "!.claude/skills/**", "!.claude/skills/*",
		"!.claude/skills/team-acme-testing/", "!.claude/skills/team-acme-testing/**",
	} {
		t.Run(neg, func(t *testing.T) {
			p, wt, mem := tsWorktree(t, func(src string) { writeFile(t, filepath.Join(src, ".gitignore"), neg+"\n") })
			pin := mem.put(1, "acme-testing", 1, acmeFiles())
			if err := p.materializeTeamSkills(context.Background(), 1, wt, []store.TeamSkillPin{pin}); err != nil {
				t.Fatalf("物化应成功: %v", err)
			}
			if got := statusAll(t, wt); strings.Contains(got, "team-acme-testing") {
				t.Errorf("根 .gitignore %q 捞回了团队技能文件:\n%s", neg, got)
			}
		})
	}
}

// r4：hideTeamSkills 复位被删 / 被改的哨兵，隐藏 agent 在目录里新增的文件。
func TestHideTeamSkills_RestoresDeletedOrEditedSentinel(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(t *testing.T, dir string)
	}{
		{"删掉哨兵", func(t *testing.T, dir string) { _ = os.Remove(filepath.Join(dir, ".gitignore")) }},
		{"哨兵改成否定", func(t *testing.T, dir string) { writeFile(t, filepath.Join(dir, ".gitignore"), "!*\n") }},
		{"目录里新增文件", func(t *testing.T, dir string) { writeFile(t, filepath.Join(dir, "extra.md"), "x\n") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, wt, mem := tsWorktree(t, nil)
			pin := mem.put(1, "acme-testing", 1, acmeFiles())
			pins := []store.TeamSkillPin{pin}
			if err := p.materializeTeamSkills(context.Background(), 1, wt, pins); err != nil {
				t.Fatal(err)
			}
			c.tamper(t, teamDir(wt))
			rc := &runCtx{ctx: context.Background(), wt: wt, teamPins: pins, tk: tsTask()}
			if err := p.hideTeamSkills(rc); err != nil {
				t.Fatalf("hideTeamSkills 失败: %v", err)
			}
			if got := readT(t, filepath.Join(teamDir(wt), ".gitignore")); got != "*\n" {
				t.Errorf("哨兵 got %q, want %q", got, "*\n")
			}
			if got := statusAll(t, wt); strings.Contains(got, "team-acme-testing") {
				t.Errorf("复位后仍可见:\n%s", got)
			}
		})
	}
}

// r5：agent 把 dest 换成指向 worktree 外的链接 → hideTeamSkills 拒绝，外部没有哨兵。
func TestHideTeamSkills_RefusesSymlinkPlantedByAgent(t *testing.T) {
	p, wt, mem := tsWorktree(t, nil)
	pin := mem.put(1, "acme-testing", 1, acmeFiles())
	pins := []store.TeamSkillPin{pin}
	if err := p.materializeTeamSkills(context.Background(), 1, wt, pins); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.RemoveAll(teamDir(wt)); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, outside, teamDir(wt))
	rc := &runCtx{ctx: context.Background(), wt: wt, teamPins: pins, tk: tsTask()}
	if err := p.hideTeamSkills(rc); err == nil {
		t.Fatal("dest 是链接时 hideTeamSkills 应返回错误")
	}
	if _, err := os.Stat(filepath.Join(outside, ".gitignore")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("worktree 外的目录被写入了哨兵（err=%v）", err)
	}
}

// 空钉集不落任何文件、不写任何规则。
func TestTeamSkillMaterialize_NoPinsWritesNothing(t *testing.T) {
	p, wt, _ := tsWorktree(t, nil)
	before := excludeSHA(t, wt)
	if err := p.materializeTeamSkills(context.Background(), 1, wt, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(wt.Path, ".claude")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("空钉集不应创建 .claude（err=%v）", err)
	}
	if after := excludeSHA(t, wt); after != before {
		t.Errorf("info/exclude 变了：%s → %s", before, after)
	}
}

// 与 skills/embed_test.go 同形的静态断言：团队技能落盘代码不依赖 $HOME。
func TestTeamSkillSourceDoesNotReferenceHome(t *testing.T) {
	src := readT(t, "teamskills.go")
	for _, banned := range []string{"UserHomeDir", `Getenv("HOME")`} {
		if strings.Contains(src, banned) {
			t.Errorf("internal/runner/teamskills.go 不应出现 %s", banned)
		}
	}
}

func tsTask() *task.Task { return &task.Task{ID: 1, UserID: 1, RepoID: 1} }
