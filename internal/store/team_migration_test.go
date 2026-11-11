package store

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/migrations"
)

// f12 AC-1 / AC-2：三条团队迁移都已应用，且表、列、索引形态正确。
//
// 只断言 65/66/67 三条存在，不断言它们是最高三条：后续线合入或重编号后仍成立
// （重编号时把这里的三个号同步改掉）。
func TestMigrationsApplyTeamSchema(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM schema_migrations WHERE version IN (65,66,67)`).Scan(&n); err != nil {
		t.Fatalf("查 schema_migrations 失败: %v", err)
	}
	if n != 3 {
		t.Fatalf("schema_migrations 里 65/66/67 的条数 got %d want 3", n)
	}

	for _, tbl := range []string{"teams", "team_members", "team_invitations"} {
		t.Run("表存在/"+tbl, func(t *testing.T) {
			var reg *string
			if err := st.pool.QueryRow(ctx, `SELECT to_regclass($1)::text`, tbl).Scan(&reg); err != nil {
				t.Fatal(err)
			}
			if reg == nil {
				t.Errorf("to_regclass(%q) got NULL want 存在", tbl)
			}
		})
	}

	t.Run("repos.team_id 可空且无默认", func(t *testing.T) {
		var nullable string
		var def *string
		err := st.pool.QueryRow(ctx, `
			SELECT is_nullable, column_default FROM information_schema.columns
			WHERE table_name = 'repos' AND column_name = 'team_id'`).Scan(&nullable, &def)
		if err != nil {
			t.Fatalf("repos.team_id 不存在: %v", err)
		}
		if nullable != "YES" || def != nil {
			t.Errorf("repos.team_id got nullable=%s default=%v want YES/<nil>", nullable, def)
		}
	})

	for _, idx := range []string{"tasks_user_updated", "tasks_repo_updated"} {
		t.Run("索引有效/"+idx, func(t *testing.T) {
			var valid bool
			err := st.pool.QueryRow(ctx, `
				SELECT i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
				WHERE c.relname = $1`, idx).Scan(&valid)
			if err != nil {
				t.Fatalf("索引 %s 不存在: %v", idx, err)
			}
			if !valid {
				t.Errorf("索引 %s got indisvalid=false want true", idx)
			}
		})
	}
}

// f12 AC-3b：0067 末尾的 DO 块在索引缺失/无效时响亮失败，两条都在时放行。
//
// CONCURRENTLY 中途失败留下的 INVALID 索引无法在测试里稳定造出；「缺失」与
// 「无效」走的是 DO 块里同一个 count <> 2 分支。
func TestMigration0067ValidityCheck(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	raw, err := fs.ReadFile(migrations.FS, "0067_tasks_access_index.up.sql")
	if err != nil {
		t.Fatalf("读 0067 up 脚本失败: %v", err)
	}
	src := string(raw)
	i := strings.Index(src, "DO $$")
	if i < 0 {
		t.Fatalf("0067 up 脚本里没有 DO 自检块")
	}
	doBlock := strings.TrimSpace(src[i:])

	cases := []struct {
		name    string
		drop    bool
		wantErr bool
	}{
		{"去掉 tasks_repo_updated 后自检报错", true, true},
		{"两条索引都在时自检通过", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tx, err := st.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if c.drop {
				if _, err := tx.Exec(ctx, `DROP INDEX tasks_repo_updated`); err != nil {
					t.Fatalf("事务内去掉索引失败: %v", err)
				}
			}
			_, err = tx.Exec(ctx, doBlock)
			if c.wantErr {
				if err == nil {
					t.Fatalf("got nil want error")
				}
				if !strings.Contains(err.Error(), "INVALID") {
					t.Errorf("错误文本 got %q want 含 INVALID", err.Error())
				}
				return
			}
			if err != nil {
				t.Errorf("got %v want nil", err)
			}
		})
	}
}

// f12 AC-5：迁移后新建/存量仓库默认私有（team_id IS NULL）。
func TestExistingReposStayPrivateAfterMigration(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	uid := teamTestUser(t, st, "private")

	if _, err := st.CreateRepo(ctx, uid, CreateRepoParams{ProviderRepo: "acme/private-" + randSuffix()}); err != nil {
		t.Fatalf("建仓库失败: %v", err)
	}
	var n int
	if err := st.pool.QueryRow(ctx,
		`SELECT count(*) FROM repos WHERE user_id = $1 AND team_id IS NOT NULL`, uid).Scan(&n); err != nil {
		t.Fatalf("查 team_id 失败: %v", err)
	}
	if n != 0 {
		t.Errorf("新建仓库 team_id 非空的条数 got %d want 0", n)
	}
}
