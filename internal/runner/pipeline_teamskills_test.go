package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
	"github.com/zichuanwangcloud-gif/lathe/internal/integration/github"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
	"github.com/zichuanwangcloud-gif/lathe/internal/teamskills"
)

// ---------------------------------------------------------------- 夹具

// tsEnv 是一条带团队技能的端到端测试环境：真实 Postgres（store 实现
// TeamSkillSource）、真实 git mirror/worktree、假 agent/GitHub/Linear。
type tsEnv struct {
	st     *store.Store
	m      *task.Machine
	userID int64
	repoID int64
	taskID int64
	repo   RepoConfig
	src    string
	// key 是任务的 issue key；共用 mirror 的两个用户必须用不同的 key，否则
	// 分支名相同，第二个 worktree add -b 会撞上第一个的分支。
	key string
}

func tsStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), dsnForTest())
	if err != nil {
		t.Fatalf("打开 store 失败: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// newTSEnv 建 user（email 带随机量）、repo、空画像任务。providerRepo 为空时
// 用随机名（各用例 mirror 互不相干）；AC-14b 传同一个名字让两个用户共用 mirror。
func newTSEnv(t *testing.T, st *store.Store, providerRepo string, profile []byte) *tsEnv {
	t.Helper()
	return newTSEnvKey(t, st, providerRepo, profile, "CR-777")
}

func newTSEnvKey(t *testing.T, st *store.Store, providerRepo string, profile []byte, key string) *tsEnv {
	t.Helper()
	ctx := context.Background()
	pool := st.Pool()
	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	if providerRepo == "" {
		providerRepo = "acme/ts-" + nonce
	}
	e := &tsEnv{st: st, m: task.NewMachine(pool), repo: DefaultRepoConfig(providerRepo), key: key}
	if err := pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"pipe-ts-"+t.Name()+"-"+nonce+"@example.com").Scan(&e.userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, e.userID) })
	if err := pool.QueryRow(ctx, `INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		e.userID, providerRepo).Scan(&e.repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	if profile == nil {
		profile = []byte("{}")
	}
	tk, err := e.m.Create(ctx, task.CreateParams{UserID: e.userID, RepoID: e.repoID, ExternalKey: key, Profile: profile})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	e.taskID = tk.ID
	return e
}

// addSkill 登记一个团队技能（slug=acme-testing）并返回其 id。
func (e *tsEnv) addSkill(t *testing.T, slug, body string) int64 {
	t.Helper()
	files := []teamskills.File{{Path: "SKILL.md", Content: skillBody(slug, body)}}
	sk, err := e.st.CreateTeamSkill(context.Background(), e.userID, e.repoID, slug, "", store.NewTeamSkillVersion{
		Files: files, SHA256: teamskills.ContentSHA256(files), TotalBytes: len(files[0].Content),
	})
	if err != nil {
		t.Fatalf("登记团队技能失败: %v", err)
	}
	return sk.ID
}

func (e *tsEnv) addVersion(t *testing.T, skillID int64, slug, body string) int {
	t.Helper()
	files := []teamskills.File{{Path: "SKILL.md", Content: skillBody(slug, body)}}
	v, _, err := e.st.AppendTeamSkillVersion(context.Background(), e.userID, e.repoID, skillID, store.NewTeamSkillVersion{
		Files: files, SHA256: teamskills.ContentSHA256(files), TotalBytes: len(files[0].Content),
	})
	if err != nil {
		t.Fatalf("写新版本失败: %v", err)
	}
	return v
}

func skillBody(slug, body string) string {
	return "---\nname: " + slug + "\ndescription: 团队规范\n---\n\n" + body + "\n"
}

func (e *tsEnv) pipeline(t *testing.T, gh *fakeGitHub, ag AgentDriver, wm *WorktreeManager) *Pipeline {
	t.Helper()
	issue := demoIssue()
	if e.key != issue.Identifier {
		issue.ID, issue.Identifier = "uuid-"+e.key, e.key
	}
	p := newPipeline(t, e.m, &fakeLinear{issue: issue}, gh, ag, &fakeNotifier{})
	if wm != nil {
		p.Worktrees = wm
	}
	p.TeamSkills = e.st
	return p
}

func (e *tsEnv) exec(t *testing.T, p *Pipeline, plan *RetryPlan) error {
	t.Helper()
	return p.Execute(context.Background(), ExecuteParams{
		TaskID: e.taskID, Repo: e.repo, CloneURL: e.src, IssueRef: "uuid-777", Actor: "test", Retry: plan,
	})
}

func (e *tsEnv) final(t *testing.T) *task.Task {
	t.Helper()
	tk, err := e.m.Get(context.Background(), e.taskID)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func (e *tsEnv) wtPath(t *testing.T) string {
	t.Helper()
	tk := e.final(t)
	if tk.WorktreePath == nil || *tk.WorktreePath == "" {
		t.Fatal("worktree 路径未落库")
	}
	return *tk.WorktreePath
}

func (e *tsEnv) pinned(t *testing.T) ([]store.TeamSkillPin, bool) {
	t.Helper()
	pins, ok, err := e.st.TeamSkillPinsLoad(context.Background(), e.taskID)
	if err != nil {
		t.Fatal(err)
	}
	return pins, ok
}

// committedFiles 是 <base>..HEAD 的提交里出现的全部路径。
func committedFiles(t *testing.T, wtPath string) string {
	t.Helper()
	return tsGit(t, wtPath, "log", "--name-only", "--format=", MirrorBaseRef("dev")+"..HEAD")
}

func sharedExclude(t *testing.T, wtPath string) (string, string) {
	t.Helper()
	p := strings.TrimSpace(tsGit(t, wtPath, "rev-parse", "--git-path", "info/exclude"))
	if !filepath.IsAbs(p) {
		p = filepath.Join(wtPath, p)
	}
	p, _ = filepath.EvalSymlinks(p)
	b, _ := os.ReadFile(p)
	return p, string(b)
}

// noChangeAgent：分诊可行 + 实现成功但不改任何文件（mutate 可选）。
func scriptedAgent(mutate func(string) error) *fakeAgent {
	return &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"有现象和期望行为","question":""}`},
			{Success: true, Text: "完成"},
		},
		mutate: []func(string) error{nil, mutate},
	}
}

func writeFix(dir string) error {
	if err := os.WriteFile(filepath.Join(dir, "main_test.go"),
		[]byte("package main\n\nimport \"testing\"\n\nfunc TestGreet(t *testing.T) {\n\tif greet() != \"hello\" {\n\t\tt.Fatalf(\"got %q\", greet())\n\t}\n}\n"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "fix.go"), []byte("package main\n\nfunc greet() string { return \"hello\" }\n"), 0o644)
}

func okGH() *fakeGitHub {
	return &fakeGitHub{pr: &github.PullRequest{Number: 1, URL: "https://github.com/acme/demo/pull/1"}}
}

func assertNoClaudeIn(t *testing.T, what, s string) {
	t.Helper()
	if strings.Contains(s, ".claude") {
		t.Errorf("%s 不应含 .claude 路径，got:\n%s", what, s)
	}
}

// ---------------------------------------------------------------- AC-11

// AC-11a：空画像 + 团队技能 → 物化、对 git 不可见、不进提交、钉行属主正确、无伪转移。
func TestPipelineTeamSkillsNoProfileMaterializedAndExcludedFromGit(t *testing.T) {
	st := tsStore(t)
	e := newTSEnv(t, st, "", nil)
	e.src = goSourceRepo(t)
	sid := e.addSkill(t, "acme-testing", "表驱动 + got/want")
	v := e.addVersion(t, sid, "acme-testing", "表驱动 + got/want（v2）")

	p := e.pipeline(t, okGH(), implFixAgent(), nil)
	if err := e.exec(t, p, nil); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if got := e.final(t).State; got != task.StatePROpen {
		t.Fatalf("终态 got %s, want pr_open", got)
	}
	wt := e.wtPath(t)
	md := readT(t, filepath.Join(wt, ".claude/skills/team-acme-testing/SKILL.md"))
	if !strings.Contains(md, "name: team-acme-testing\n") {
		t.Errorf("SKILL.md 的 name 未改写:\n%s", md)
	}
	if _, err := os.Stat(filepath.Join(wt, ".claude/skills/go-testing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("空画像下不应物化产品技能 go-testing（err=%v）", err)
	}
	if _, ex := sharedExclude(t, wt); strings.Contains(ex, ".claude/skills/") {
		t.Errorf("共享 info/exclude 不应含 .claude/skills/（隐藏只来自哨兵），got %q", ex)
	}
	assertNoClaudeIn(t, "git status", statusAllPath(t, wt))
	assertNoClaudeIn(t, "提交内容", committedFiles(t, wt))
	changed, err := p.Worktrees.ChangedFiles(context.Background(), &Worktree{Path: wt, BaseBranch: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	assertNoClaudeIn(t, "ChangedFiles", strings.Join(changed, "\n"))

	var u, r int64
	var raw string
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT user_id, repo_id, pinned::text FROM team_task_skills WHERE task_id=$1`, e.taskID).Scan(&u, &r, &raw); err != nil {
		t.Fatalf("应有一行钉行: %v", err)
	}
	if u != e.userID || r != e.repoID {
		t.Errorf("钉行属主 got (%d,%d), want (%d,%d)", u, r, e.userID, e.repoID)
	}
	pins, _ := e.pinned(t)
	if len(pins) != 1 || pins[0].Version != v || pins[0].SkillID != sid {
		t.Errorf("钉住 got %+v, want skill %d v%d", pins, sid, v)
	}
	var pseudo int
	_ = st.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM task_events WHERE task_id=$1 AND from_state IS NOT DISTINCT FROM to_state`, e.taskID).Scan(&pseudo)
	if pseudo != 0 {
		t.Errorf("task_events 出现 %d 条伪转移，want 0", pseudo)
	}
}

func statusAllPath(t *testing.T, wt string) string {
	t.Helper()
	return tsGit(t, wt, "status", "--porcelain", "--untracked-files=all")
}

// AC-11b：验证体系的守门测试 —— 团队技能文件不能骗过「工作区无改动」判死。
func TestPipelineTeamSkillsDoNotSatisfyNoChangesCheck(t *testing.T) {
	st := tsStore(t)
	e := newTSEnv(t, st, "", nil)
	e.src = goSourceRepo(t)
	e.addSkill(t, "acme-testing", "x")
	gh := okGH()
	verifs := &fakeVerifications{}
	p := e.pipeline(t, gh, scriptedAgent(nil), nil)
	p.Verifications = verifs

	if err := e.exec(t, p, nil); err == nil {
		t.Fatal("agent 没改任何文件，任务应失败")
	}
	tk := e.final(t)
	if tk.State != task.StateFailed || tk.FailureStage == nil || *tk.FailureStage != string(StageImplementNoChanges) {
		t.Fatalf("got state=%s stage=%v, want failed/%s", tk.State, deref(tk.FailureStage), StageImplementNoChanges)
	}
	if len(gh.params) != 0 {
		t.Error("不应开 PR")
	}
	if len(verifs.rows) != 0 {
		t.Errorf("不应有验证落库，got %v", verifs.rows)
	}
}

// AC-11d：agent 删掉哨兵 → 提交前复位；只删哨兵不改别的 → 仍判死。
func TestPipelineTeamSkillsAgentDeletesSentinelStillNotCommitted(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(dir string) error
		wantState task.State
		wantStage Stage
	}{
		{"写代码并删哨兵仍不提交技能文件", func(dir string) error {
			if err := writeFix(dir); err != nil {
				return err
			}
			return os.Remove(filepath.Join(dir, ".claude/skills/team-acme-testing/.gitignore"))
		}, task.StatePROpen, ""},
		{"只删哨兵不改别的仍判无改动", func(dir string) error {
			return os.Remove(filepath.Join(dir, ".claude/skills/team-acme-testing/.gitignore"))
		}, task.StateFailed, StageImplementNoChanges},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := tsStore(t)
			e := newTSEnv(t, st, "", nil)
			e.src = goSourceRepo(t)
			e.addSkill(t, "acme-testing", "x")
			gh := okGH()
			p := e.pipeline(t, gh, scriptedAgent(c.mutate), nil)
			_ = e.exec(t, p, nil)
			tk := e.final(t)
			if tk.State != c.wantState {
				t.Fatalf("终态 got %s (stage %s), want %s", tk.State, deref(tk.FailureStage), c.wantState)
			}
			if c.wantStage != "" && deref(tk.FailureStage) != string(c.wantStage) {
				t.Fatalf("失败阶段 got %s, want %s", deref(tk.FailureStage), c.wantStage)
			}
			wt := e.wtPath(t)
			if c.wantState == task.StatePROpen {
				assertNoClaudeIn(t, "提交内容", committedFiles(t, wt))
				changed, _ := p.Worktrees.ChangedFiles(context.Background(), &Worktree{Path: wt, BaseBranch: "dev"})
				assertNoClaudeIn(t, "ChangedFiles", strings.Join(changed, "\n"))
				if got := readT(t, filepath.Join(wt, ".claude/skills/team-acme-testing/.gitignore")); got != "*\n" {
					t.Errorf("哨兵应已复位，got %q", got)
				}
			} else if len(gh.params) != 0 {
				t.Error("判死时不应开 PR")
			}
		})
	}
}

// AC-11c：画像声明的产品技能与团队技能并存，互不覆盖。
func TestPipelineTeamSkillsCoexistWithProductSkills(t *testing.T) {
	st := tsStore(t)
	e := newTSEnv(t, st, "", []byte(`{"skills":[{"name":"go-testing","version":"1.0.0"}]}`))
	e.src = goSourceRepo(t)
	e.addSkill(t, "acme-testing", "x")
	p := e.pipeline(t, okGH(), implFixAgent(), nil)
	if err := e.exec(t, p, nil); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	wt := e.wtPath(t)
	got := readT(t, filepath.Join(wt, ".claude/skills/go-testing/SKILL.md"))
	want := readT(t, filepath.Join(skillsSourceDir(t), "go-testing", "1.0.0", "SKILL.md"))
	if got != want {
		t.Error("go-testing/SKILL.md 与嵌入内容不一致")
	}
	if _, err := os.Stat(filepath.Join(wt, ".claude/skills/team-acme-testing/SKILL.md")); err != nil {
		t.Errorf("团队技能应并存: %v", err)
	}
	assertNoClaudeIn(t, "git status", statusAllPath(t, wt))
}

// ---------------------------------------------------------------- AC-9 端到端

// AC-9：停用的技能新任务不钉不物化；已钉任务续跑仍按钉住版本物化（归档同语义）。
func TestDisabledSkillNotPinnedForNewTasks(t *testing.T) {
	ctx := context.Background()
	t.Run("停用后新任务不钉不物化", func(t *testing.T) {
		st := tsStore(t)
		e := newTSEnv(t, st, "", nil)
		e.src = goSourceRepo(t)
		e.addSkill(t, "s-one", "1")
		s2 := e.addSkill(t, "s-two", "2")
		off := false
		if _, err := st.UpdateTeamSkill(ctx, e.userID, e.repoID, s2, &off, nil); err != nil {
			t.Fatal(err)
		}
		p := e.pipeline(t, okGH(), implFixAgent(), nil)
		if err := e.exec(t, p, nil); err != nil {
			t.Fatalf("Execute 失败: %v", err)
		}
		pins, _ := e.pinned(t)
		for _, pin := range pins {
			if pin.SkillID == s2 {
				t.Errorf("停用的 s-two 不应被钉，got %+v", pins)
			}
		}
		wt := e.wtPath(t)
		if _, err := os.Stat(filepath.Join(wt, ".claude/skills/team-s-two")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("team-s-two 不应存在（err=%v）", err)
		}
		if _, err := os.Stat(filepath.Join(wt, ".claude/skills/team-s-one/SKILL.md")); err != nil {
			t.Errorf("team-s-one 应存在: %v", err)
		}
	})
	for _, how := range []string{"停用", "归档"} {
		t.Run("已钉任务"+how+"后续跑仍物化", func(t *testing.T) {
			st := tsStore(t)
			e := newTSEnv(t, st, "", nil)
			e.src = goSourceRepo(t)
			sid := e.addSkill(t, "s-two", "钉住的内容")
			p, wtPath := runUntilImplementFails(t, e)
			if how == "停用" {
				off := false
				if _, err := st.UpdateTeamSkill(ctx, e.userID, e.repoID, sid, &off, nil); err != nil {
					t.Fatal(err)
				}
			} else if err := st.ArchiveTeamSkill(ctx, e.userID, e.repoID, sid); err != nil {
				t.Fatal(err)
			}
			_ = os.RemoveAll(filepath.Join(wtPath, ".claude"))
			requeue(t, e)
			_ = e.exec(t, p, &RetryPlan{Entry: EntryImplement})
			got := readT(t, filepath.Join(wtPath, ".claude/skills/team-s-two/SKILL.md"))
			if !strings.Contains(got, "钉住的内容") {
				t.Errorf("续跑应按钉住版本物化，got:\n%s", got)
			}
		})
	}
}

// TestPinnedVersionSurvivesArchive 是 AC-9 的同名断言（PRD 点名）：归档后已钉任务仍能物化。
func TestPinnedVersionSurvivesArchive(t *testing.T) {
	st := tsStore(t)
	e := newTSEnv(t, st, "", nil)
	e.src = goSourceRepo(t)
	sid := e.addSkill(t, "acme-testing", "v1 内容")
	p, wtPath := runUntilImplementFails(t, e)
	if err := st.ArchiveTeamSkill(context.Background(), e.userID, e.repoID, sid); err != nil {
		t.Fatal(err)
	}
	if list, _ := st.ListTeamSkills(context.Background(), e.userID, e.repoID); len(list) != 0 {
		t.Errorf("归档后列表应不含它，got %+v", list)
	}
	_ = os.RemoveAll(filepath.Join(wtPath, ".claude"))
	requeue(t, e)
	_ = e.exec(t, p, &RetryPlan{Entry: EntryImplement})
	if got := readT(t, filepath.Join(wtPath, ".claude/skills/team-acme-testing/SKILL.md")); !strings.Contains(got, "v1 内容") {
		t.Errorf("归档后已钉任务续跑应物化 v1，got:\n%s", got)
	}
}

// runUntilImplementFails 跑一轮：钉住并物化后 agent 实现阶段报错 → failed/implement_run，
// 现场保留、无提交。返回同一条流水线（复用 WorktreeManager）与现场路径。
func runUntilImplementFails(t *testing.T, e *tsEnv) (*Pipeline, string) {
	t.Helper()
	ag := &fakeAgent{
		results: []*agent.Result{
			{Success: true, Text: `{"actionable":true,"kind":"fix","reason":"r","question":""}`},
		},
		errs: []error{nil, errors.New("agent 崩了")},
	}
	p := e.pipeline(t, okGH(), ag, nil)
	_ = e.exec(t, p, nil)
	tk := e.final(t)
	if tk.State != task.StateFailed || deref(tk.FailureStage) != string(StageImplementRun) {
		t.Fatalf("前置：got %s/%s, want failed/%s", tk.State, deref(tk.FailureStage), StageImplementRun)
	}
	if _, ok := e.pinned(t); !ok {
		t.Fatal("前置：第一轮应已钉住")
	}
	// 续跑的 agent：实现阶段写代码（成功与否不影响物化断言）
	p.Agent = &fakeAgent{results: []*agent.Result{{Success: true, Text: "ok"}}, mutate: []func(string) error{writeFix}}
	return p, e.wtPath(t)
}

func requeue(t *testing.T, e *tsEnv) {
	t.Helper()
	if _, err := e.m.Transition(context.Background(), e.taskID, task.StateQueued, "test",
		&task.TransitionOpts{Payload: map[string]any{"reason": "manual_retry"}}); err != nil {
		t.Fatalf("failed -> queued 失败: %v", err)
	}
}

// ---------------------------------------------------------------- AC-12

// 钉版本不被版本更新推动：续跑仍是 v1；从头重建重钉最新版。
func TestPinnedVersionIsStableAcrossResume(t *testing.T) {
	st := tsStore(t)
	e := newTSEnv(t, st, "", nil)
	e.src = goSourceRepo(t)
	sid := e.addSkill(t, "acme-testing", "第一版")
	p, wtPath := runUntilImplementFails(t, e)
	e.addVersion(t, sid, "acme-testing", "第二版")
	requeue(t, e)
	_ = e.exec(t, p, &RetryPlan{Fresh: false, Entry: EntryImplement})
	got := readT(t, filepath.Join(wtPath, ".claude/skills/team-acme-testing/SKILL.md"))
	if !strings.Contains(got, "第一版") || strings.Contains(got, "第二版") {
		t.Errorf("续跑应仍是 v1，got:\n%s", got)
	}
}

func TestFreshRebuildRepinsLatest(t *testing.T) {
	st := tsStore(t)
	e := newTSEnv(t, st, "", nil)
	e.src = goSourceRepo(t)
	ctx := context.Background()
	sid := e.addSkill(t, "acme-testing", "第一版")
	// 前置：任务已钉 v1（与「首次物化钉 v1」同一行内容，直接用生产代码的 ListEnabled + PinsSave 写入）。
	// 不先真跑一轮再 Fresh 重跑：重跑时任务行仍记着旧 worktree_path 且处于 queued，
	// ClaimedWorktreePaths 会把它自己的路径当成「在途任务占用」—— 那是 f11 之外的既有行为，
	// 这里只证 PinsClear 在 PinsLoad 之前生效。
	v1, err := st.ListEnabledTeamSkills(ctx, e.repoID, e.userID)
	if err != nil || len(v1) != 1 || v1[0].Version != 1 {
		t.Fatalf("前置 ListEnabled got %+v %v", v1, err)
	}
	if err := st.TeamSkillPinsSave(ctx, e.taskID, e.userID, e.repoID, v1); err != nil {
		t.Fatal(err)
	}
	v2 := e.addVersion(t, sid, "acme-testing", "第二版")
	p := e.pipeline(t, okGH(), implFixAgent(), nil)
	if err := e.exec(t, p, &RetryPlan{Fresh: true, Entry: EntryTriage}); err != nil {
		t.Fatalf("重建 Execute 失败: %v", err)
	}
	got := readT(t, filepath.Join(e.wtPath(t), ".claude/skills/team-acme-testing/SKILL.md"))
	if !strings.Contains(got, "第二版") {
		t.Errorf("重建应物化 v2，got:\n%s", got)
	}
	pins, _ := e.pinned(t)
	if len(pins) != 1 || pins[0].Version != v2 {
		t.Errorf("重建后钉住 got %+v, want 只有 v%d", pins, v2)
	}
}

// ---------------------------------------------------------------- AC-13

// 钉住的版本行被删 → failed/team_skill_failed，原因点名 slug@version，agent 未被驱动。
func TestPipelineTeamSkillMissingVersionFails(t *testing.T) {
	st := tsStore(t)
	e := newTSEnv(t, st, "", nil)
	e.src = goSourceRepo(t)
	sid := e.addSkill(t, "acme-testing", "x")
	p, _ := runUntilImplementFails(t, e)
	if _, err := st.Pool().Exec(context.Background(), `DELETE FROM team_skill_versions WHERE skill_id=$1`, sid); err != nil {
		t.Fatal(err)
	}
	ag := &fakeAgent{}
	p.Agent = ag
	requeue(t, e)
	if err := e.exec(t, p, &RetryPlan{Entry: EntryImplement, ResumeSession: true}); err == nil {
		t.Fatal("版本行被删，任务应失败")
	}
	tk := e.final(t)
	if deref(tk.FailureStage) != string(StageTeamSkillFailed) {
		t.Fatalf("失败阶段 got %s, want %s", deref(tk.FailureStage), StageTeamSkillFailed)
	}
	if !strings.Contains(deref(tk.FailureReason), "team-acme-testing@1") {
		t.Errorf("失败原因应点名 slug@version，got %q", deref(tk.FailureReason))
	}
	if len(ag.calls) != 0 {
		t.Errorf("物化应在驱动 agent 之前拦下，agent 被调用 %d 次", len(ag.calls))
	}
}

// failingTeamSkills 包装真实 store，按开关注入错误（AGENTS.md §5.2：外部依赖走接口注入）。
type failingTeamSkills struct {
	TeamSkillSource
	failSave, failLoad, failList bool
}

func (f *failingTeamSkills) TeamSkillPinsSave(ctx context.Context, taskID, userID, repoID int64, pins []store.TeamSkillPin) error {
	if f.failSave {
		return errors.New("injected pin save failure")
	}
	return f.TeamSkillSource.TeamSkillPinsSave(ctx, taskID, userID, repoID, pins)
}
func (f *failingTeamSkills) TeamSkillPinsLoad(ctx context.Context, taskID int64) ([]store.TeamSkillPin, bool, error) {
	if f.failLoad {
		return nil, false, errors.New("injected pin load failure")
	}
	return f.TeamSkillSource.TeamSkillPinsLoad(ctx, taskID)
}
func (f *failingTeamSkills) ListEnabledTeamSkills(ctx context.Context, repoID, userID int64) ([]store.TeamSkillPin, error) {
	if f.failList {
		return nil, errors.New("injected list enabled failure")
	}
	return f.TeamSkillSource.ListEnabledTeamSkills(ctx, repoID, userID)
}

// 钉行读/写/列启用技能任一出错 → failed/team_skill_failed，不降级为「无技能」继续跑。
func TestPipelineTeamSkillPinSaveFailureFails(t *testing.T) {
	cases := []struct {
		name string
		fts  failingTeamSkills
	}{
		{"PinsSave出错", failingTeamSkills{failSave: true}},
		{"PinsLoad出错", failingTeamSkills{failLoad: true}},
		{"ListEnabled出错", failingTeamSkills{failList: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := tsStore(t)
			e := newTSEnv(t, st, "", nil)
			e.src = goSourceRepo(t)
			e.addSkill(t, "acme-testing", "x")
			gh := okGH()
			ag := implFixAgent()
			p := e.pipeline(t, gh, ag, nil)
			fts := c.fts
			fts.TeamSkillSource = st
			p.TeamSkills = &fts
			if err := e.exec(t, p, nil); err == nil {
				t.Fatal("应失败")
			}
			tk := e.final(t)
			if tk.State != task.StateFailed || deref(tk.FailureStage) != string(StageTeamSkillFailed) {
				t.Fatalf("got %s/%s, want failed/%s", tk.State, deref(tk.FailureStage), StageTeamSkillFailed)
			}
			if deref(tk.FailureReason) == "" {
				t.Error("failure_reason 应非空")
			}
			if len(ag.calls) != 1 {
				t.Errorf("只应有分诊一次调用（实现未被驱动），got %d", len(ag.calls))
			}
			if m, _ := filepath.Glob(filepath.Join(e.wtPath(t), ".claude/skills/team-*")); len(m) != 0 {
				t.Errorf("不应物化任何团队技能，got %v", m)
			}
			if _, ok := e.pinned(t); ok {
				t.Error("不应留下钉行")
			}
			if len(gh.params) != 0 {
				t.Error("不应开 PR")
			}
		})
	}
}

// review 二轮（分支已有提交）团队技能物化失败 → 重试决策回到 EntryImplement，再失败仍不推送。
func TestPipelineTeamSkillFailureInReviewRoundIsRetriedThroughImplement(t *testing.T) {
	st := tsStore(t)
	e := newTSEnv(t, st, "acme/demo", nil) // ReviewIntake 按 acme/demo + PR 号定位任务
	e.src = goSourceRepo(t)
	sid := e.addSkill(t, "acme-testing", "x")
	ctx := context.Background()

	gh := &fakeGitHub{pr: &github.PullRequest{URL: "https://x/pr/11", Number: reviewPR, Existing: true}}
	p := e.pipeline(t, gh, implFixAgent(), nil)
	if err := e.exec(t, p, nil); err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}
	if e.final(t).State != task.StatePROpen {
		t.Fatalf("第一轮终态 got %s, want pr_open", e.final(t).State)
	}
	prCallsAfterFirst := len(gh.params)

	if out, err := (&ReviewIntake{Tasks: e.m}).AcceptReview(ctx, e.userID,
		inlineEvent("inline:91", "alice", "请补一条日志"), "d-ts"); err != nil || out.Status != ReviewQueued {
		t.Fatalf("回流失败：%+v %v", out, err)
	}
	if _, err := st.Pool().Exec(ctx, `DELETE FROM team_skill_versions WHERE skill_id=$1`, sid); err != nil {
		t.Fatal(err)
	}
	p.Agent = &fakeAgent{}
	if err := e.exec(t, p, &RetryPlan{Entry: EntryImplement, Review: true, ResumeSession: true}); err == nil {
		t.Fatal("二轮物化失败应返回错误")
	}
	tk := e.final(t)
	if tk.State != task.StateFailed || deref(tk.FailureStage) != string(StageTeamSkillFailed) {
		t.Fatalf("二轮 got %s/%s, want failed/%s", tk.State, deref(tk.FailureStage), StageTeamSkillFailed)
	}

	requeue(t, e)
	tk = e.final(t)
	wtState := p.Worktrees.Inspect(ctx, e.repo.ProviderRepo, deref(tk.WorktreePath), deref(tk.BranchName), "dev")
	if !wtState.HasCommits {
		t.Fatal("前置：现场应有第一轮的提交（测的正是「有提交」那格）")
	}
	plan := PlanRetry(RetryAuto, RetryInput{
		Stage:      Stage(deref(tk.FailureStage)),
		HasSession: tk.AgentSessionID != nil && *tk.AgentSessionID != "",
		WT:         wtState,
	})
	if plan.Fresh || plan.Entry != EntryImplement {
		t.Fatalf("PlanRetry got %+v, want Entry=implement（不能跳到提交/验证绕过物化）", plan)
	}
	if err := e.exec(t, p, &plan); err == nil {
		t.Fatal("版本仍缺失，重试应再次失败")
	}
	if got := deref(e.final(t).FailureStage); got != string(StageTeamSkillFailed) {
		t.Fatalf("重试后失败阶段 got %s, want %s", got, StageTeamSkillFailed)
	}
	if len(gh.params) != prCallsAfterFirst {
		t.Errorf("不应再推送/开 PR：CreatePR 调用 %d → %d", prCallsAfterFirst, len(gh.params))
	}
}

// ---------------------------------------------------------------- AC-14

// 功能关闭（TeamSkills == nil）时没有任何副作用。
func TestPipelineWithoutTeamSkillsUnchanged(t *testing.T) {
	st := tsStore(t)
	e := newTSEnv(t, st, "", nil)
	e.src = goSourceRepo(t)
	e.addSkill(t, "acme-testing", "x")
	p := e.pipeline(t, okGH(), implFixAgent(), nil)
	p.TeamSkills = nil
	if err := e.exec(t, p, nil); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if got := e.final(t).State; got != task.StatePROpen {
		t.Fatalf("终态 got %s, want pr_open", got)
	}
	if _, ok := e.pinned(t); ok {
		t.Error("功能关闭时不应写钉行")
	}
	wt := e.wtPath(t)
	if _, ex := sharedExclude(t, wt); strings.Contains(ex, ".claude/skills/") {
		t.Errorf("info/exclude 不应含 .claude/skills/，got %q", ex)
	}
	if _, err := os.Stat(filepath.Join(wt, ".claude")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("功能关闭时不应创建 .claude（err=%v）", err)
	}
}

// AC-14b：两个用户共用同一 provider 仓库的 mirror，A 的团队技能不在 B 的 worktree 留残留。
func TestPipelineTeamSkillsNoCrossUserResidue(t *testing.T) {
	for _, c := range []struct {
		name        string
		aProfile    []byte
		wantBNewHit bool
	}{
		{"团队技能不残留", nil, true},
		// 对照格：锁住既有行为（产品技能路径写共享 exclude），不是 f11 的通过条件。
		// fix/skill-exclude-scope 修掉它时这一格要随那个 PR 翻转，而不是被悄悄删掉。
		{"ProductSkillResidueIsPreexisting", []byte(`{"skills":[{"name":"go-testing","version":"1.0.0"}]}`), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := tsStore(t)
			wm, err := NewWorktreeManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			src := goSourceRepo(t)
			const provider = "acme/demo"

			a := newTSEnv(t, st, provider, c.aProfile)
			a.src = src
			if c.aProfile == nil {
				a.addSkill(t, "acme-testing", "A 的规范")
			}
			pa := a.pipeline(t, okGH(), implFixAgent(), wm)
			if err := a.exec(t, pa, nil); err != nil {
				t.Fatalf("A 执行失败: %v", err)
			}
			if a.final(t).State != task.StatePROpen {
				t.Fatalf("A 终态 got %s", a.final(t).State)
			}
			if c.aProfile == nil {
				if pins, _ := a.pinned(t); len(pins) != 1 {
					t.Fatalf("A 应已钉 1 个技能，got %+v", pins)
				}
			}

			b := newTSEnvKey(t, st, provider, nil, "CR-778")
			b.src = src
			pb := b.pipeline(t, okGH(), scriptedAgent(func(dir string) error {
				if err := writeFix(dir); err != nil {
					return err
				}
				if err := os.MkdirAll(filepath.Join(dir, ".claude/skills/b-new"), 0o755); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(dir, ".claude/skills/b-new/SKILL.md"), []byte("b\n"), 0o644)
			}), wm)
			if err := b.exec(t, pb, nil); err != nil {
				t.Fatalf("B 执行失败: %v", err)
			}
			if b.final(t).State != task.StatePROpen {
				t.Fatalf("B 终态 got %s", b.final(t).State)
			}
			pinsB, okB := b.pinned(t)
			if !okB || len(pinsB) != 0 {
				t.Errorf("B 应有一行空钉，got %+v ok=%v", pinsB, okB)
			}
			aEx, _ := sharedExclude(t, a.wtPath(t))
			bEx, bContent := sharedExclude(t, b.wtPath(t))
			if aEx != bEx {
				t.Fatalf("前置：A/B 的 info/exclude 应解析到同一路径，got %s vs %s", aEx, bEx)
			}
			committed := committedFiles(t, b.wtPath(t))
			if strings.Contains(committed, "team-") {
				t.Errorf("B 的提交不应含任何 team- 路径:\n%s", committed)
			}
			hit := strings.Contains(committed, ".claude/skills/b-new/SKILL.md")
			if hit != c.wantBNewHit {
				t.Errorf("B 的提交含 b-new = %v, want %v；提交:\n%s\n共享 exclude:\n%s", hit, c.wantBNewHit, committed, bContent)
			}
			if gotEx := strings.Contains(bContent, ".claude/skills/"); gotEx == c.wantBNewHit {
				t.Errorf("共享 exclude 含 .claude/skills/ = %v, want %v", gotEx, !c.wantBNewHit)
			}
		})
	}
}
