package store

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/testsupport"
)

// f12 AC-43：scripts/clean-test-db.sh 只碰 LATHE_TEST_DSN 指向的库，且孤儿团队清理
// SQL 正确。全程【不执行 --yes】（AGENTS §4：真删必须先问人）。

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// cleanScriptRunnable 报告宿主能否跑脚本（有 psql，或有在跑的 lathe-postgres-dev 容器）。
func cleanScriptRunnable() (bool, string) {
	if _, err := exec.LookPath("psql"); err == nil {
		return true, ""
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return false, "宿主既没有 psql 也没有 docker"
	}
	out, err := exec.Command("docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil || !regexp.MustCompile(`(?m)^lathe-postgres-dev$`).Match(out) {
		return false, "没有在跑的 lathe-postgres-dev 容器"
	}
	return true, ""
}

// runCleanScript 干跑脚本：显式 Dir、白名单 Env（只含 PATH 与 LATHE_TEST_DSN，AGENTS §9 / B2-3）。
func runCleanScript(t *testing.T, dsn string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := exec.Command("sh", "scripts/clean-test-db.sh")
	cmd.Dir = repoRoot(t)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LATHE_TEST_DSN=" + dsn}
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	return o.String(), e.String(), err
}

func dsnDB(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("解析 DSN 失败: %v", err)
	}
	return strings.TrimPrefix(u.Path, "/")
}

func TestCleanTestDBTargetsDSNDatabase(t *testing.T) {
	if ok, why := cleanScriptRunnable(); !ok {
		t.Skipf("跳过：%s（AGENTS §5.2）", why)
	}
	dsn := testsupport.DSN()
	want := dsnDB(t, dsn)

	t.Run("干跑连的是 DSN 里的库", func(t *testing.T) {
		out, stderr, err := runCleanScript(t, dsn)
		if err != nil {
			t.Fatalf("干跑失败: %v\nstderr=%s", err, stderr)
		}
		if strings.Contains(stderr, "ERROR") {
			t.Errorf("stderr 含 ERROR: %s", stderr)
		}
		m := regexp.MustCompile(`current_database[^\n]*\n[-+\s]*\n\s*(\S+)`).FindStringSubmatch(out)
		got := ""
		if m != nil {
			got = m[1]
		}
		if got != want {
			t.Errorf("current_database got %q want %q\n输出：\n%s", got, want, out)
		}
		if !strings.Contains(out, "孤儿团队") {
			t.Errorf("干跑输出应含孤儿团队计数\n%s", out)
		}
	})

	t.Run("库不存在 → 非零退出，不回落", func(t *testing.T) {
		u, _ := url.Parse(dsn)
		u.Path = "/lathe_f12_nonexistent_" + randSuffix()
		out, _, err := runCleanScript(t, u.String())
		if err == nil {
			t.Errorf("连不存在的库 got exit 0 want 非零\n%s", out)
		}
		if strings.Contains(out, "current_database") {
			t.Errorf("连不上时不应打印 current_database：\n%s", out)
		}
	})

	t.Run("DSN 解析不出库名 → 非零退出", func(t *testing.T) {
		out, _, err := runCleanScript(t, "postgres://h:1/")
		if err == nil {
			t.Errorf("got exit 0 want 非零\n%s", out)
		}
	})
}

// extractDeleteSQL 从脚本里按标记注释抽出 --yes 段的 DELETE 原文。
func extractDeleteSQL(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "clean-test-db.sh"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	i := strings.Index(src, "# lathe:delete-sql-begin")
	j := strings.Index(src, "# lathe:delete-sql-end")
	if i < 0 || j < i {
		t.Fatalf("脚本里没有 lathe:delete-sql-begin/end 标记")
	}
	var out []string
	for _, m := range regexp.MustCompile(`"(DELETE FROM [^"]+)"`).FindAllStringSubmatch(src[i:j], -1) {
		out = append(out, m[1])
	}
	if len(out) != 2 {
		t.Fatalf("标记段内的 DELETE 条数 got %d want 2: %q", len(out), out)
	}
	return out
}

func TestCleanTestDBDeleteSQLInRollback(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	stmts := extractDeleteSQL(t)

	var usersBefore, teamsBefore int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE email LIKE '%@example.com'`).Scan(&usersBefore); err != nil {
		t.Fatal(err)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM teams`).Scan(&teamsBefore); err != nil {
		t.Fatal(err)
	}

	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() { _ = tx.Rollback(ctx) }()
		sfx := randSuffix()
		var fixtureUser, realUser, fixtureTeam, realTeam int64
		mk := func(q string, dst *int64, args ...any) {
			if err := tx.QueryRow(ctx, q, args...).Scan(dst); err != nil {
				t.Fatal(err)
			}
		}
		mk(`INSERT INTO users (email) VALUES ($1) RETURNING id`, &fixtureUser, fmt.Sprintf("f12-clean-%s@example.com", sfx))
		mk(`INSERT INTO users (email) VALUES ($1) RETURNING id`, &realUser, fmt.Sprintf("f12-clean-%s@real.test", sfx))
		mk(`INSERT INTO teams (name) VALUES ('fixture') RETURNING id`, &fixtureTeam)
		mk(`INSERT INTO teams (name) VALUES ('real') RETURNING id`, &realTeam)
		if _, err := tx.Exec(ctx, `INSERT INTO team_members (team_id,user_id,role) VALUES ($1,$2,'owner'),($3,$4,'owner')`,
			fixtureTeam, fixtureUser, realTeam, realUser); err != nil {
			t.Fatal(err)
		}
		// 存量问题（与 f12 无关，本线不修，已在 PR 里报告）：issue_comments.user_id 是
		// ON DELETE SET NULL，而 issue_comments_author_check 要求 user_id / actor 恰有一个
		// 非空 —— fixture 用户写过评论时，脚本既有的 DELETE FROM users 会撞 23514。
		// 这里只在回滚事务里先清掉这些评论，好让断言落在两条 DELETE 的顺序与别名上。
		if _, err := tx.Exec(ctx, `DELETE FROM issue_comments c USING users u
			WHERE c.user_id = u.id AND u.email LIKE '%@example.com'`); err != nil {
			t.Fatal(err)
		}
		for _, s := range stmts {
			if _, err := tx.Exec(ctx, s); err != nil {
				t.Fatalf("执行 %q 失败: %v", s, err)
			}
		}
		var n int
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM teams WHERE id=$1`, fixtureTeam).Scan(&n)
		if n != 0 {
			t.Errorf("fixture team got present want deleted")
		}
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM teams t WHERE NOT EXISTS (SELECT 1 FROM team_members m WHERE m.team_id = t.id)`).Scan(&n)
		if n != 0 {
			t.Errorf("孤儿团队 got %d want 0", n)
		}
		_ = tx.QueryRow(ctx, `SELECT count(*) FROM teams WHERE id=$1`, realTeam).Scan(&n)
		if n != 1 {
			t.Errorf("对照团队 got deleted want present")
		}
	}()

	var usersAfter, teamsAfter int
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE email LIKE '%@example.com'`).Scan(&usersAfter)
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM teams`).Scan(&teamsAfter)
	// -p 1 下同包别的测试不会并发写，这里取等号断言
	if usersAfter != usersBefore || teamsAfter != teamsBefore {
		t.Errorf("回滚后库被动过：users %d→%d teams %d→%d", usersBefore, usersAfter, teamsBefore, teamsAfter)
	}
}
