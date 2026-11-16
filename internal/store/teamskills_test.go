package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zichuanwangcloud-gif/lathe/internal/teamskills"
)

// tsFixture 建一个带随机量的 user + repo（AGENTS.md §5.5：跨轮次残留不串）。
func tsFixture(t *testing.T, st *Store, tag string) (userID, repoID int64) {
	t.Helper()
	ctx := context.Background()
	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := st.pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"ts-"+tag+"-"+t.Name()+"-"+nonce+"@example.com").Scan(&userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})
	if err := st.pool.QueryRow(ctx, `INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		userID, "acme/ts-"+tag+"-"+nonce).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	return userID, repoID
}

// rawSkill 绕开 API 直接插技能行（AC-3：证明约束在库这一层）。
func rawSkill(ctx context.Context, st *Store, userID, repoID int64, slug string) (int64, error) {
	var id int64
	err := st.pool.QueryRow(ctx,
		`INSERT INTO team_skills (user_id, repo_id, slug) VALUES ($1,$2,$3) RETURNING id`,
		userID, repoID, slug).Scan(&id)
	return id, err
}

func sqlState(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

// AC-3：slug 的 CHECK 在库层生效。
func TestTeamSkills_SlugCheckRejectsBadSlug(t *testing.T) {
	st := testStore(t)
	userID, repoID := tsFixture(t, st, "slug")
	ctx := context.Background()
	for _, bad := range []string{"../x", "a/b", "A-B", "-lead", "trail-", strings.Repeat("a", 42), ""} {
		t.Run("拒绝"+bad, func(t *testing.T) {
			_, err := rawSkill(ctx, st, userID, repoID, bad)
			if got := sqlState(err); got != "23514" {
				t.Errorf("INSERT slug=%q: SQLSTATE got %q (err %v), want 23514", bad, got, err)
			}
		})
	}
	if _, err := rawSkill(ctx, st, userID, repoID, "acme-testing"); err != nil {
		t.Errorf("合法 slug 应能插入: %v", err)
	}
}

// AC-3：同仓库在役 slug 唯一；归档后可复用。
func TestTeamSkills_ActiveSlugUnique(t *testing.T) {
	st := testStore(t)
	userID, repoID := tsFixture(t, st, "uniq")
	ctx := context.Background()
	first, err := rawSkill(ctx, st, userID, repoID, "acme-testing")
	if err != nil {
		t.Fatalf("第一次插入失败: %v", err)
	}
	if _, err := rawSkill(ctx, st, userID, repoID, "acme-testing"); sqlState(err) != "23505" {
		t.Fatalf("第二次在役插入: SQLSTATE got %q (err %v), want 23505", sqlState(err), err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE team_skills SET archived_at = now() WHERE id=$1`, first); err != nil {
		t.Fatal(err)
	}
	if _, err := rawSkill(ctx, st, userID, repoID, "acme-testing"); err != nil {
		t.Errorf("归档后同名插入应成功: %v", err)
	}
}

// AC-3：删仓库 → 技能与版本行级联消失。
func TestTeamSkills_CascadeOnRepoDelete(t *testing.T) {
	st := testStore(t)
	userID, repoID := tsFixture(t, st, "casc")
	ctx := context.Background()
	sid, err := rawSkill(ctx, st, userID, repoID, "acme-testing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO team_skill_versions (skill_id, version, files, content_sha256, total_bytes)
		VALUES ($1, 1, '[]', 'x', 1)`, sid); err != nil {
		t.Fatalf("插版本失败: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM repos WHERE id=$1`, repoID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM team_skills WHERE id=$1) +
		(SELECT count(*) FROM team_skill_versions WHERE skill_id=$1)`, sid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("删仓库后残留 %d 行，want 0", n)
	}
}

// AC-3：删任务 → 钉行级联消失。
func TestTeamTaskSkills_CascadeOnTaskDelete(t *testing.T) {
	st := testStore(t)
	userID, repoID := tsFixture(t, st, "tcasc")
	ctx := context.Background()
	taskID := metricsTask(t, st, userID, repoID, "TS-CASC")
	if _, err := st.pool.Exec(ctx, `INSERT INTO team_task_skills (task_id, user_id, repo_id, pinned) VALUES ($1,$2,$3,'[]')`,
		taskID, userID, repoID); err != nil {
		t.Fatalf("插钉行失败: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM tasks WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM team_task_skills WHERE task_id=$1`, taskID).Scan(&n)
	if n != 0 {
		t.Errorf("删任务后钉行残留 %d 行，want 0", n)
	}
}

// AC-3：钉行的属主列 NOT NULL。
func TestTeamTaskSkills_OwnerColumnsRequired(t *testing.T) {
	st := testStore(t)
	userID, repoID := tsFixture(t, st, "own")
	ctx := context.Background()
	taskID := metricsTask(t, st, userID, repoID, "TS-OWN")
	cases := []struct {
		name       string
		user, repo any
	}{
		{"user_id为NULL", nil, repoID},
		{"repo_id为NULL", userID, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := st.pool.Exec(ctx, `INSERT INTO team_task_skills (task_id, user_id, repo_id, pinned) VALUES ($1,$2,$3,'[]')`,
				taskID, c.user, c.repo)
			if got := sqlState(err); got != "23502" {
				t.Errorf("SQLSTATE got %q (err %v), want 23502", got, err)
			}
		})
	}
}

func tsVersion() NewTeamSkillVersion {
	files := []teamskills.File{{Path: "SKILL.md", Content: "---\nname: x\ndescription: d\n---\n"}}
	return NewTeamSkillVersion{Files: files, SHA256: teamskills.ContentSHA256(files), TotalBytes: len(files[0].Content)}
}

func tsPin(t *testing.T, st *Store, taskID, userID, repoID int64, pinned string, age time.Duration) {
	t.Helper()
	if _, err := st.pool.Exec(context.Background(), `
		INSERT INTO team_task_skills (task_id, user_id, repo_id, pinned, pinned_at)
		VALUES ($1, $2, $3, $4::jsonb, now() - $5::interval)`, taskID, userID, repoID, pinned, age); err != nil {
		t.Fatalf("插钉行失败: %v", err)
	}
}

// AC-6：pinnedTaskCount 按（属主, 仓库）限定、只算在役技能、近 30 天窗口。
func TestListTeamSkills_PinnedTaskCount(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userA, repo1 := tsFixture(t, st, "pcA")
	var repo2 int64
	if err := st.pool.QueryRow(ctx, `INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		userA, "acme/ts-pc2-"+strconv.FormatInt(time.Now().UnixNano(), 36)).Scan(&repo2); err != nil {
		t.Fatal(err)
	}
	userB, repoB := tsFixture(t, st, "pcB")

	s1, err := st.CreateTeamSkill(ctx, userA, repo1, "s1", "", tsVersion())
	if err != nil {
		t.Fatal(err)
	}
	s2, err := st.CreateTeamSkill(ctx, userA, repo1, "s2", "", tsVersion())
	if err != nil {
		t.Fatal(err)
	}
	s3, err := st.CreateTeamSkill(ctx, userA, repo1, "s3", "", tsVersion())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ArchiveTeamSkill(ctx, userA, repo1, s3.ID); err != nil {
		t.Fatal(err)
	}
	pin := func(id int64, v int) string {
		return `[{"skillId":` + strconv.FormatInt(id, 10) + `,"slug":"team-x","version":` + strconv.Itoa(v) + `,"sha256":"h"}]`
	}
	tsPin(t, st, metricsTask(t, st, userA, repo1, "PC-1"), userA, repo1, pin(s1.ID, 1), 0)               // 计入
	tsPin(t, st, metricsTask(t, st, userA, repo1, "PC-2"), userA, repo1, pin(s1.ID, 2), 0)               // 计入
	tsPin(t, st, metricsTask(t, st, userA, repo1, "PC-3"), userA, repo1, `[]`, 0)                        // 空钉不计
	tsPin(t, st, metricsTask(t, st, userB, repoB, "PC-4"), userB, repoB, pin(s1.ID, 1), 0)               // 他人伪造不计
	tsPin(t, st, metricsTask(t, st, userA, repo2, "PC-5"), userA, repo2, pin(s1.ID, 1), 0)               // 他仓不计
	tsPin(t, st, metricsTask(t, st, userA, repo1, "PC-6"), userA, repo1, pin(s1.ID, 1), 31*24*time.Hour) // 窗口外不计

	got, err := st.ListTeamSkills(ctx, userA, repo1)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"s1": 2, "s2": 0}
	if len(got) != len(want) {
		t.Fatalf("List 返回 %d 个技能 %+v，want 只有 s1、s2（s3 已归档）", len(got), got)
	}
	for _, sk := range got {
		w, ok := want[sk.Slug]
		if !ok {
			t.Errorf("不该出现技能 %s", sk.Slug)
			continue
		}
		if sk.PinnedTaskCount != w {
			t.Errorf("技能 %s pinnedTaskCount got %d, want %d", sk.Slug, sk.PinnedTaskCount, w)
		}
		if sk.LatestVersion != 1 {
			t.Errorf("技能 %s latestVersion got %d, want 1", sk.Slug, sk.LatestVersion)
		}
	}
	_ = s2
}

// AC-6：列表语句的谓词形态能走属主索引。
func TestListTeamSkills_PinnedCountUsesOwnerIndex(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, repoID := tsFixture(t, st, "idx")
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query(ctx, "EXPLAIN "+teamSkillListSQL, userID, repoID, pinnedCountWindow)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		_ = rows.Scan(&line)
		plan.WriteString(line + "\n")
	}
	rows.Close()
	if !strings.Contains(plan.String(), "team_task_skills_owner_repo") {
		t.Errorf("EXPLAIN 未用到 team_task_skills_owner_repo：\n%s", plan.String())
	}
}

// AC-9（store 层）：停用的技能不进 ListEnabled，但仍在列表里；重新启用恢复。
func TestDisabledSkillNotPinnedForNewTasks_Store(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, repoID := tsFixture(t, st, "dis")
	s1, err := st.CreateTeamSkill(ctx, userID, repoID, "s1", "", tsVersion())
	if err != nil {
		t.Fatal(err)
	}
	s2, err := st.CreateTeamSkill(ctx, userID, repoID, "s2", "", tsVersion())
	if err != nil {
		t.Fatal(err)
	}
	slugs := func() []string {
		pins, err := st.ListEnabledTeamSkills(ctx, repoID, userID)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, p := range pins {
			out = append(out, p.Slug)
		}
		return out
	}
	off, on := false, true
	if _, err := st.UpdateTeamSkill(ctx, userID, repoID, s2.ID, &off, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(slugs(), ","); got != "team-s1" {
		t.Errorf("停用 s2 后 ListEnabled got %q, want team-s1", got)
	}
	list, _ := st.ListTeamSkills(ctx, userID, repoID)
	var sawDisabled bool
	for _, sk := range list {
		if sk.ID == s2.ID && !sk.Enabled {
			sawDisabled = true
		}
	}
	if !sawDisabled {
		t.Errorf("停用不是归档：列表里应仍有 s2 且 enabled=false，got %+v", list)
	}
	if _, err := st.UpdateTeamSkill(ctx, userID, repoID, s2.ID, &on, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(slugs(), ","); got != "team-s1,team-s2" {
		t.Errorf("重新启用后 ListEnabled got %q, want team-s1,team-s2", got)
	}
	_ = s1
}

// 钉行：pinned=[] 与「没有这一行」可分；先写者赢；Clear 后回到「没钉」。
func TestTeamSkillPins_SaveLoadClear(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, repoID := tsFixture(t, st, "pins")
	taskID := metricsTask(t, st, userID, repoID, "PINS-1")
	if _, ok, err := st.TeamSkillPinsLoad(ctx, taskID); err != nil || ok {
		t.Fatalf("新任务 PinsLoad got ok=%v err=%v, want ok=false", ok, err)
	}
	if err := st.TeamSkillPinsSave(ctx, taskID, userID, repoID, nil); err != nil {
		t.Fatal(err)
	}
	pins, ok, err := st.TeamSkillPinsLoad(ctx, taskID)
	if err != nil || !ok || len(pins) != 0 {
		t.Fatalf("钉空集后 got pins=%v ok=%v err=%v, want [] true nil", pins, ok, err)
	}
	if err := st.TeamSkillPinsSave(ctx, taskID, userID, repoID, []TeamSkillPin{{SkillID: 1, Slug: "team-x", Version: 1}}); err != nil {
		t.Fatal(err)
	}
	if pins, _, _ := st.TeamSkillPinsLoad(ctx, taskID); len(pins) != 0 {
		t.Errorf("先写者赢：第二次 Save 不应覆盖，got %v", pins)
	}
	if err := st.TeamSkillPinsClear(ctx, taskID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.TeamSkillPinsLoad(ctx, taskID); ok {
		t.Error("Clear 后应回到「没钉」")
	}
}

// PATCH 的返回值带上最新版本号（界面直接用它刷新，不能是 0）。
func TestUpdateTeamSkill_ReturnsLatestVersion(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, repoID := tsFixture(t, st, "upd")
	sk, err := st.CreateTeamSkill(ctx, userID, repoID, "s1", "", tsVersion())
	if err != nil {
		t.Fatal(err)
	}
	off := false
	got, err := st.UpdateTeamSkill(ctx, userID, repoID, sk.ID, &off, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.LatestVersion != 1 || got.Enabled {
		t.Errorf("got latestVersion=%d enabled=%v, want 1 false", got.LatestVersion, got.Enabled)
	}
}
