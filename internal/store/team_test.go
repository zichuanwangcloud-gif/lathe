package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---------------------------------------------------------------- fixture

func randSuffix() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// teamTestUser 建一个随机 @example.com 用户，测试结束连其名下数据一起删。
func teamTestUser(t *testing.T, st *Store, prefix string) int64 {
	t.Helper()
	var id int64
	email := fmt.Sprintf("f12-%s-%s@example.com", prefix, randSuffix())
	if err := st.pool.QueryRow(context.Background(),
		`INSERT INTO users (email) VALUES ($1) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// teamTestRepo 建仓库；infra 非空时写进 verify_infra（AC-52③ 要观测非空值）。
func teamTestRepo(t *testing.T, st *Store, uid int64, infra ...string) int64 {
	t.Helper()
	if infra == nil {
		infra = []string{}
	}
	var id int64
	if err := st.pool.QueryRow(context.Background(),
		`INSERT INTO repos (user_id, provider_repo, verify_infra) VALUES ($1, $2, $3) RETURNING id`,
		uid, "acme/f12-"+randSuffix(), infra).Scan(&id); err != nil {
		t.Fatalf("建仓库失败: %v", err)
	}
	return id
}

// teamTestTeam 直接用 SQL 建团队与成员（绕开被测的 store 函数，红灯才落在行为断言上）。
func teamTestTeam(t *testing.T, st *Store, members map[int64]string) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := st.pool.QueryRow(ctx,
		`INSERT INTO teams (name) VALUES ($1) RETURNING id`, "f12-"+randSuffix()).Scan(&id); err != nil {
		t.Fatalf("建团队失败: %v", err)
	}
	t.Cleanup(func() { _, _ = st.pool.Exec(context.Background(), `DELETE FROM teams WHERE id = $1`, id) })
	for uid, role := range members {
		if _, err := st.pool.Exec(ctx,
			`INSERT INTO team_members (team_id, user_id, role) VALUES ($1,$2,$3)`, id, uid, role); err != nil {
			t.Fatalf("加成员失败: %v", err)
		}
	}
	return id
}

func teamTestShare(t *testing.T, st *Store, repoID, teamID int64) {
	t.Helper()
	if _, err := st.pool.Exec(context.Background(),
		`UPDATE repos SET team_id = $2 WHERE id = $1`, repoID, teamID); err != nil {
		t.Fatalf("共享仓库失败: %v", err)
	}
}

func teamTestTask(t *testing.T, st *Store, uid, repoID int64) int64 {
	t.Helper()
	var id int64
	if err := st.pool.QueryRow(context.Background(), `
		INSERT INTO tasks (user_id, repo_id, external_key) VALUES ($1,$2,$3) RETURNING id`,
		uid, repoID, "F12-"+randSuffix()).Scan(&id); err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	return id
}

// ---------------------------------------------------------------- AC-27

func TestRepoAccessMatrix(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	owner := teamTestUser(t, st, "ra-owner")
	tOwner := teamTestUser(t, st, "ra-towner")
	tAdmin := teamTestUser(t, st, "ra-tadmin")
	tMember := teamTestUser(t, st, "ra-tmember")
	stranger := teamTestUser(t, st, "ra-stranger")

	team := teamTestTeam(t, st, map[int64]string{
		owner: TeamRoleMember, tOwner: TeamRoleOwner, tAdmin: TeamRoleAdmin, tMember: TeamRoleMember,
	})
	shared := teamTestRepo(t, st, owner)
	teamTestShare(t, st, shared, team)
	private := teamTestRepo(t, st, owner)

	cases := []struct {
		name    string
		repo    int64
		viewer  int64
		want    RepoRole
		wantErr error
	}{
		{"属主看共享仓库 → owner", shared, owner, RepoRoleOwner, nil},
		{"团队 owner 看共享仓库 → team_owner", shared, tOwner, RepoRoleTeamOwner, nil},
		{"团队 admin 看共享仓库 → team_admin", shared, tAdmin, RepoRoleTeamAdmin, nil},
		{"团队 member 看共享仓库 → team_member", shared, tMember, RepoRoleTeamMember, nil},
		{"非成员看共享仓库 → 不可见", shared, stranger, "", ErrRepoNotFound},
		{"团队成员看属主的私有仓库 → 不可见", private, tOwner, "", ErrRepoNotFound},
		{"属主看私有仓库 → owner", private, owner, RepoRoleOwner, nil},
		{"不存在的仓库 → 不可见", -1, owner, "", ErrRepoNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := st.RepoAccess(ctx, c.repo, c.viewer)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err got %v want %v", err, c.wantErr)
			}
			if got != c.want {
				t.Errorf("role got %q want %q", got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------- AC-51③ / AC-52

func TestListReposStaysOwnerOnly(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	jia := teamTestUser(t, st, "lr-jia")
	yi := teamTestUser(t, st, "lr-yi")
	team := teamTestTeam(t, st, map[int64]string{jia: TeamRoleOwner, yi: TeamRoleMember})
	mine := teamTestRepo(t, st, jia)
	theirs := teamTestRepo(t, st, yi)
	teamTestShare(t, st, theirs, team)

	got, err := st.ListRepos(ctx, jia)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != mine {
		t.Errorf("ListRepos(甲) got %+v want 只有甲自己的仓库 %d", got, mine)
	}
}

func TestListReposForViewerMatchesListRepos(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	jia := teamTestUser(t, st, "lv-jia")
	yi := teamTestUser(t, st, "lv-yi")
	team := teamTestTeam(t, st, map[int64]string{jia: TeamRoleOwner, yi: TeamRoleMember})
	mine := teamTestRepo(t, st, jia)
	theirs := teamTestRepo(t, st, yi, "redis")
	teamTestShare(t, st, theirs, team)
	_ = teamTestRepo(t, st, yi) // 乙的私有仓库不应出现

	baseline := func(owner, id int64) RepoRow {
		t.Helper()
		rows, err := st.ListRepos(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ID == id {
				return r
			}
		}
		t.Fatalf("ListRepos(%d) 里没有仓库 %d", owner, id)
		return RepoRow{}
	}

	rows, err := st.ListReposForViewer(ctx, jia)
	if err != nil {
		t.Fatalf("ListReposForViewer: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows want 2: %+v", len(rows), rows)
	}
	byID := map[int64]ViewerRepo{}
	for _, r := range rows {
		byID[r.ID] = r
	}

	t.Run("甲自己的行与 ListRepos(甲) 同一行一致", func(t *testing.T) {
		r := byID[mine]
		if want := baseline(jia, mine); !reflect.DeepEqual(r.RepoRow, want) {
			t.Errorf("got %+v want %+v", r.RepoRow, want)
		}
		if !r.Owned || r.TeamID != nil {
			t.Errorf("owned/teamId got %v/%v want true/nil", r.Owned, r.TeamID)
		}
	})
	t.Run("团队臂的行与 ListRepos(属主乙) 同一行一致", func(t *testing.T) {
		r, ok := byID[theirs]
		if !ok {
			t.Fatalf("团队仓库 %d 缺失", theirs)
		}
		if want := baseline(yi, theirs); !reflect.DeepEqual(r.RepoRow, want) {
			t.Errorf("got %+v want %+v", r.RepoRow, want)
		}
		if r.Owned || r.TeamID == nil || *r.TeamID != team {
			t.Errorf("owned/teamId got %v/%v want false/%d", r.Owned, r.TeamID, team)
		}
		if !reflect.DeepEqual(r.VerifyInfra, []string{"redis"}) {
			t.Errorf("VerifyInfra got %#v want [redis]", r.VerifyInfra)
		}
	})
	t.Run("GetRepoForViewer 与基准一致", func(t *testing.T) {
		got, err := st.GetRepoForViewer(ctx, theirs, jia)
		if err != nil {
			t.Fatalf("GetRepoForViewer: %v", err)
		}
		if want := baseline(yi, theirs); !reflect.DeepEqual(got.RepoRow, want) {
			t.Errorf("got %+v want %+v", got.RepoRow, want)
		}
	})
	t.Run("非成员 GetRepoForViewer → ErrRepoNotFound", func(t *testing.T) {
		stranger := teamTestUser(t, st, "lv-stranger")
		if _, err := st.GetRepoForViewer(ctx, theirs, stranger); !errors.Is(err, ErrRepoNotFound) {
			t.Errorf("err got %v want ErrRepoNotFound", err)
		}
	})
}

// ---------------------------------------------------------------- AC-10（store 侧） / AC-53

func TestListTasksTeamViewRowsMatchDefaultView(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	viewer := teamTestUser(t, st, "lt-viewer")
	mate := teamTestUser(t, st, "lt-mate")
	team := teamTestTeam(t, st, map[int64]string{viewer: TeamRoleOwner, mate: TeamRoleMember})
	myRepo := teamTestRepo(t, st, viewer)
	mateRepo := teamTestRepo(t, st, mate)
	teamTestShare(t, st, mateRepo, team)

	// 自己的 3 条任务，字段尽量填满（含内置工单 issue_id）
	issue, err := st.CreateIssue(ctx, CreateIssueParams{UserID: viewer, RepoID: myRepo, Title: "f12 issue"})
	if err != nil {
		t.Fatalf("建工单失败: %v", err)
	}
	var mine []int64
	for i := 0; i < 3; i++ {
		var id int64
		key, provider := "F12-"+randSuffix(), "linear"
		if i == 0 {
			key, provider = issue.Key, "internal"
		}
		if err := st.pool.QueryRow(ctx, `
			INSERT INTO tasks (user_id, repo_id, external_key, tracker_provider, state, task_kind, verify_tier,
			                   branch_name, pr_url, failure_reason, failure_stage, worktree_path,
			                   agent_session_id, lease_expires_at, agent_summary, agent_cost_usd,
			                   agent_duration_ms, agent_num_turns)
			VALUES ($1,$2,$3,$4,'failed','fix','light','fix/x','https://example.com/pr/1','boom','verify','/tmp/wt',
			        'sess', now(), 'summary', 0.42, 1234, 7)
			RETURNING id`, viewer, myRepo, key, provider).Scan(&id); err != nil {
			t.Fatalf("建任务失败: %v", err)
		}
		mine = append(mine, id)
	}
	mateTask := teamTestTask(t, st, mate, mateRepo)

	def, defTotal, err := st.ListTasks(ctx, ListTasksParams{UserID: viewer, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	team1, teamTotal, err := st.ListTasks(ctx, ListTasksParams{UserID: viewer, Limit: 50, TeamView: true})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("total：默认只算自己，团队视图含队友", func(t *testing.T) {
		if defTotal != 3 {
			t.Errorf("默认 total got %d want 3", defTotal)
		}
		if teamTotal != 4 {
			t.Errorf("团队 total got %d want 4", teamTotal)
		}
		if len(team1) != teamTotal {
			t.Errorf("团队列表行数 got %d want %d（与 total 一致）", len(team1), teamTotal)
		}
	})

	t.Run("团队视图的自己那几行除 RepoID 外与默认视图逐字段相等", func(t *testing.T) {
		defByID := map[int64]TaskRow{}
		for _, r := range def {
			defByID[r.ID] = r
		}
		for _, r := range team1 {
			if r.ID == mateTask {
				if r.RepoID != mateRepo {
					t.Errorf("队友任务 RepoID got %d want %d", r.RepoID, mateRepo)
				}
				continue
			}
			want, ok := defByID[r.ID]
			if !ok {
				t.Errorf("团队视图多出任务 %d", r.ID)
				continue
			}
			if r.RepoID != myRepo {
				t.Errorf("任务 %d RepoID got %d want %d", r.ID, r.RepoID, myRepo)
			}
			r.RepoID = 0
			if !reflect.DeepEqual(r, want) {
				t.Errorf("任务 %d 字段漂移：%s", r.ID, diffFields(r, want))
			}
		}
	})

	t.Run("默认视图 JSON 里没有 repoId 键", func(t *testing.T) {
		b, _ := json.Marshal(def[0])
		if strings.Contains(string(b), `"repoId"`) {
			t.Errorf("默认视图 JSON 不应带 repoId: %s", b)
		}
	})
	_ = mine
}

// diffFields 列出两个 TaskRow 不相等的字段名，给漂移失败一个可读的定位。
func diffFields(got, want TaskRow) string {
	gv, wv := reflect.ValueOf(got), reflect.ValueOf(want)
	var out []string
	for i := 0; i < gv.NumField(); i++ {
		if !reflect.DeepEqual(gv.Field(i).Interface(), wv.Field(i).Interface()) {
			out = append(out, fmt.Sprintf("%s got %v want %v", gv.Type().Field(i).Name,
				fmtPtr(gv.Field(i)), fmtPtr(wv.Field(i))))
		}
	}
	return strings.Join(out, "; ")
}

func fmtPtr(v reflect.Value) any {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "<nil>"
		}
		return v.Elem().Interface()
	}
	return v.Interface()
}

// ---------------------------------------------------------------- AC-28

// planNode 是 EXPLAIN (FORMAT JSON) 的节点子集。
type planNode struct {
	NodeType     string     `json:"Node Type"`
	RelationName string     `json:"Relation Name"`
	IndexName    string     `json:"Index Name"`
	IndexCond    string     `json:"Index Cond"`
	RecheckCond  string     `json:"Recheck Cond"`
	ActualRows   float64    `json:"Actual Rows"`
	ActualLoops  float64    `json:"Actual Loops"`
	SharedHit    int64      `json:"Shared Hit Blocks"`
	SharedRead   int64      `json:"Shared Read Blocks"`
	Plans        []planNode `json:"Plans"`
}

func walkPlan(n planNode, f func(planNode)) {
	f(n)
	for _, c := range n.Plans {
		walkPlan(c, f)
	}
}

func TestListTasksScopeTeamUsesIndex(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 20 个用户各 1 个仓库 × 2000 条任务；viewer = 0 号，与 1 号同在团队 T，1 号的仓库共享到 T
	const users, perRepo = 20, 2000
	sfx := randSuffix()
	var uids, repos []int64
	for i := 0; i < users; i++ {
		var uid, rid int64
		if err := tx.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`,
			fmt.Sprintf("f12-plan-%d-%s@example.com", i, sfx)).Scan(&uid); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, `INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
			uid, fmt.Sprintf("acme/plan-%d-%s", i, sfx)).Scan(&rid); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO tasks (user_id, repo_id, external_key, state, updated_at)
			SELECT $1, $2, 'P-' || g, 'merged', now() - (g || ' seconds')::interval
			FROM generate_series(1, $3) g`, uid, rid, perRepo); err != nil {
			t.Fatal(err)
		}
		uids, repos = append(uids, uid), append(repos, rid)
	}
	var team int64
	if err := tx.QueryRow(ctx, `INSERT INTO teams (name) VALUES ('plan') RETURNING id`).Scan(&team); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO team_members (team_id, user_id, role) VALUES ($1,$2,'owner'),($1,$3,'member')`,
		team, uids[0], uids[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE repos SET team_id = $1 WHERE id = $2`, team, repos[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `ANALYZE tasks`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `ANALYZE repos`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `ANALYZE team_members`); err != nil {
		t.Fatal(err)
	}

	// 与 ListTasks 同一个常量 —— 测的就是跑的
	var raw []byte
	if err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+listTasksTeamSQL,
		uids[0], nil, 50, 0).Scan(&raw); err != nil {
		t.Fatalf("EXPLAIN 失败: %v", err)
	}
	var plans []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) == 0 {
		t.Fatalf("解析计划失败: %v", err)
	}
	root := plans[0].Plan

	var text strings.Builder
	var userIdx, repoIdx, seq bool
	var tasksRows float64
	walkPlan(root, func(n planNode) {
		fmt.Fprintf(&text, "%s rel=%s idx=%s cond=%s rows=%.0f loops=%.0f\n",
			n.NodeType, n.RelationName, n.IndexName, n.IndexCond, n.ActualRows, n.ActualLoops)
		if n.IndexName == "tasks_user_updated" && strings.Contains(n.IndexCond, "user_id") {
			userIdx = true
		}
		if n.IndexName == "tasks_repo_updated" && strings.Contains(n.IndexCond, "repo_id") {
			repoIdx = true
		}
		if n.RelationName == "tasks" {
			if n.NodeType == "Seq Scan" {
				seq = true
			}
			loops := n.ActualLoops
			if loops == 0 {
				loops = 1
			}
			tasksRows += n.ActualRows * loops
		}
	})
	buffers := root.SharedHit + root.SharedRead
	checks := []struct {
		name string
		ok   bool
	}{
		{"① tasks_user_updated 带 user_id 的 Index Cond", userIdx},
		{"② tasks_repo_updated 带 repo_id 的 Index Cond", repoIdx},
		{"③ 没有 tasks 的 Seq Scan", !seq},
		{fmt.Sprintf("④ tasks 节点实际行数 %.0f ≤ %d", tasksRows, 2*perRepo+50), tasksRows <= 2*perRepo+50},
		{fmt.Sprintf("⑤ 顶层 buffers %d ≤ 200", buffers), buffers <= 200},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("断言失败：%s\n计划：\n%s", c.name, text.String())
		}
	}
}

// ---------------------------------------------------------------- AC-50（store 侧）

func TestTeamTxRetriesThenBusy(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	deadlock := &pgconn.PgError{Code: "40P01"}
	other := errors.New("boom")

	cases := []struct {
		name        string
		fails       int   // 前几次返回 failErr
		failErr     error // 失败时返回的错误
		wantErr     error
		wantCalls   int
		wantRetries int64
	}{
		{"前两次 40P01、第三次成功 → nil，重试 2 次", 2, deadlock, nil, 3, 2},
		{"每次都 40P01 → ErrTeamBusy，恰好调 3 次", 99, deadlock, ErrTeamBusy, 3, 2},
		{"40001 也重试", 1, &pgconn.PgError{Code: "40001"}, nil, 2, 1},
		{"其他错误原样返回，只调 1 次", 99, other, other, 1, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := TeamTxRetries()
			calls := 0
			err := st.teamTx(ctx, "test", func(pgx.Tx) error {
				calls++
				if calls <= c.fails {
					return c.failErr
				}
				return nil
			})
			if !errors.Is(err, c.wantErr) {
				t.Errorf("err got %v want %v", err, c.wantErr)
			}
			if calls != c.wantCalls {
				t.Errorf("calls got %d want %d", calls, c.wantCalls)
			}
			if d := TeamTxRetries() - before; d != c.wantRetries {
				t.Errorf("TeamTxRetries delta got %d want %d", d, c.wantRetries)
			}
		})
	}
}

// ---------------------------------------------------------------- AC-49

// lockFixture 是一次确定性交错用例的数据：团队 T（owner O、member B、member C），
// B 有一个私有仓库 R、一条 pending 邀请码。
type lockFixture struct {
	team, owner, b, c, repo int64
	acceptor                int64 // 尚未入队、持有邀请码的人
	inviteID                int64
	code                    string
}

func newLockFixture(t *testing.T, st *Store) *lockFixture {
	t.Helper()
	f := &lockFixture{}
	f.owner = teamTestUser(t, st, "lk-owner")
	f.b = teamTestUser(t, st, "lk-b")
	f.c = teamTestUser(t, st, "lk-c")
	f.team = teamTestTeam(t, st, map[int64]string{f.owner: TeamRoleOwner, f.b: TeamRoleMember, f.c: TeamRoleMember})
	f.repo = teamTestRepo(t, st, f.b)
	f.acceptor = teamTestUser(t, st, "lk-acc")
	f.code = "lk-" + randSuffix()
	h := hashInviteCode(f.code)
	if err := st.pool.QueryRow(context.Background(), `
		INSERT INTO team_invitations (team_id, code_hash, code_hint, role, expires_at)
		VALUES ($1,$2,'xxxx','member', now() + interval '1 hour') RETURNING id`, f.team, h).Scan(&f.inviteID); err != nil {
		t.Fatal(err)
	}
	return f
}

// waitLockWaiter 轮询 pg_stat_activity，直到本库里出现一个在等锁的后端（不 sleep 判定）。
func waitLockWaiter(t *testing.T, st *Store) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := st.pool.QueryRow(context.Background(), `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("tx2 没有阻塞在锁上：用例的前提（两者争同一把 teams 行锁）不成立")
}

func TestTeamTxLockOrderNoDeadlock(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	share := func(f *lockFixture) error { return st.ShareRepo(ctx, f.repo, f.b, &f.team) }
	remove := func(f *lockFixture) error { _, err := st.RemoveMember(ctx, f.team, f.owner, f.b); return err }
	patch := func(f *lockFixture) error { return st.SetMemberRole(ctx, f.team, f.owner, f.c, TeamRoleAdmin) }
	transfer := func(f *lockFixture) error { return st.TransferOwner(ctx, f.team, f.owner, f.c) }
	delTeam := func(f *lockFixture) error { return st.DeleteTeam(ctx, f.team, f.owner) }
	accept := func(f *lockFixture) error {
		_, err := st.AcceptInvitation(ctx, f.code, f.acceptor)
		return err
	}
	revoke := func(f *lockFixture) error { return st.RevokeInvitation(ctx, f.team, f.inviteID, f.owner) }
	delUser := func(f *lockFixture) error { return st.DeleteUserGuarded(ctx, f.b) }

	repoTeam := func(f *lockFixture) *int64 {
		var tid *int64
		if err := st.pool.QueryRow(ctx, `SELECT team_id FROM repos WHERE id=$1`, f.repo).Scan(&tid); err != nil {
			return nil
		}
		return tid
	}
	exists := func(q string, args ...any) bool {
		var n int
		_ = st.pool.QueryRow(ctx, q, args...).Scan(&n)
		return n > 0
	}
	anyOf := func(errs ...error) func(error) bool {
		return func(err error) bool {
			for _, e := range errs {
				if errors.Is(err, e) {
					return true
				}
			}
			return false
		}
	}
	isNil := func(err error) bool { return err == nil }

	cases := []struct {
		name         string
		op1, op2     string
		tx1, tx2     func(*lockFixture) error
		want1, want2 func(error) bool
		final        func(t *testing.T, f *lockFixture)
	}{
		{"F-1 共享在先 × 移除在后", "share", "", share, remove, isNil, isNil, func(t *testing.T, f *lockFixture) {
			if repoTeam(f) != nil {
				t.Errorf("移除应收回共享：team_id got %v want NULL", *repoTeam(f))
			}
		}},
		{"F-2 移除在先 × 共享在后", "remove", "", remove, share, isNil, anyOf(ErrTeamNotFound), func(t *testing.T, f *lockFixture) {
			if repoTeam(f) != nil {
				t.Errorf("非成员的仓库不应共享：team_id got %v want NULL", *repoTeam(f))
			}
		}},
		{"F-3 共享在先 × 改角色在后", "share", "", share, patch, isNil, isNil, func(t *testing.T, f *lockFixture) {
			if tid := repoTeam(f); tid == nil || *tid != f.team {
				t.Errorf("共享应生效：team_id got %v want %d", tid, f.team)
			}
		}},
		{"F-4 共享在先 × 转让在后", "share", "", share, transfer, isNil, isNil, func(t *testing.T, f *lockFixture) {
			if !exists(`SELECT count(*) FROM team_members WHERE team_id=$1 AND user_id=$2 AND role='owner'`, f.team, f.c) {
				t.Errorf("转让应生效：C 不是 owner")
			}
		}},
		{"F-5 删团队在先 × 共享在后", "delete_team", "", delTeam, share, isNil, anyOf(ErrTeamNotFound), func(t *testing.T, f *lockFixture) {
			if exists(`SELECT count(*) FROM teams WHERE id=$1`, f.team) {
				t.Errorf("团队应已删除")
			}
		}},
		{"F-6 接受在先 × 撤销在后", "accept", "", accept, revoke, isNil, anyOf(ErrInvitationNotFound), func(t *testing.T, f *lockFixture) {
			if !exists(`SELECT count(*) FROM team_invitations WHERE id=$1 AND state='accepted'`, f.inviteID) {
				t.Errorf("邀请应为 accepted")
			}
		}},
		{"F-6' 撤销在先 × 接受在后", "revoke", "", revoke, accept, isNil, anyOf(ErrInviteInvalid), func(t *testing.T, f *lockFixture) {
			if !exists(`SELECT count(*) FROM team_invitations WHERE id=$1 AND state='revoked'`, f.inviteID) {
				t.Errorf("邀请应为 revoked")
			}
		}},
		{"F-7 删账号在先 × 共享在后", "delete_user", "", delUser, share, isNil, anyOf(ErrRepoNotFound, ErrTeamNotFound), func(t *testing.T, f *lockFixture) {
			if exists(`SELECT count(*) FROM users WHERE id=$1`, f.b) {
				t.Errorf("账号应已删除")
			}
		}},
		{"F-8 共享在先 × 删账号在后", "share", "", share, delUser, isNil, isNil, func(t *testing.T, f *lockFixture) {
			if exists(`SELECT count(*) FROM repos WHERE id=$1`, f.repo) {
				t.Errorf("仓库应随账号级联删除")
			}
		}},
		{"F-9 移除在先 × 删账号在后", "remove", "", remove, delUser, isNil, isNil, func(t *testing.T, f *lockFixture) {
			if exists(`SELECT count(*) FROM users WHERE id=$1`, f.b) {
				t.Errorf("账号应已删除")
			}
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newLockFixture(t, st)
			before := TeamTxRetries()

			reached := make(chan struct{})
			release := make(chan struct{})
			var paused atomic.Bool
			hook := func(op string) {
				if op == c.op1 && paused.CompareAndSwap(false, true) {
					close(reached)
					<-release
				}
			}
			teamLockHook.Store(&hook)
			defer teamLockHook.Store(nil)

			var err1, err2 error
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); err1 = c.tx1(f) }()
			select {
			case <-reached:
			case <-time.After(5 * time.Second):
				close(release)
				wg.Done()
				wg.Wait()
				t.Fatalf("tx1 没有到达「持有第一把锁」的挂起点（err1=%v）", err1)
			}
			go func() { defer wg.Done(); err2 = c.tx2(f) }()
			waitLockWaiter(t, st)
			close(release)
			wg.Wait()

			for i, e := range []error{err1, err2} {
				var pg *pgconn.PgError
				if errors.As(e, &pg) && pg.Code == "40P01" {
					t.Errorf("tx%d got SQLSTATE 40P01 want nil", i+1)
				}
			}
			if !c.want1(err1) {
				t.Errorf("tx1 err got %v", err1)
			}
			if !c.want2(err2) {
				t.Errorf("tx2 err got %v", err2)
			}
			if d := TeamTxRetries() - before; d != 0 {
				t.Errorf("TeamTxRetries delta got %d want 0（锁序成环被重试吃掉了）", d)
			}
			c.final(t, f)
		})
	}
}
