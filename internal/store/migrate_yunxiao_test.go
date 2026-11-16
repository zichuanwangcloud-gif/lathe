package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zichuanwangcloud-gif/lathe/internal/testsupport"
)

// yunxiaoVersions 是本功能的三条迁移（0053–0055）。
var yunxiaoVersions = []int{53, 54, 55}

// assertYunxiaoSchema 断言云效迁移的 schema 全部存在（present）或全部不存在。
//
// 按版本号断言而不是 max(version)=55：邻居线的迁移合入后最大版本会更高。
func assertYunxiaoSchema(t *testing.T, pool *pgxpool.Pool, present bool) {
	t.Helper()
	ctx := context.Background()

	for _, v := range yunxiaoVersions {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version = $1`, v).Scan(&n); err != nil {
			t.Fatalf("查 schema_migrations: %v", err)
		}
		if got, want := n == 1, present; got != want {
			t.Errorf("schema_migrations 里版本 %d 已登记 got %v want %v", v, got, want)
		}
	}
	for _, tbl := range []string{"yunxiao_connections", "yunxiao_trigger_state"} {
		var reg *string
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1)::text`, tbl).Scan(&reg); err != nil {
			t.Fatalf("to_regclass(%s): %v", tbl, err)
		}
		if got, want := reg != nil, present; got != want {
			t.Errorf("表 %s 存在 got %v want %v", tbl, got, want)
		}
	}

	var def string
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conname = 'tasks_tracker_provider_check' AND conrelid = 'tasks'::regclass`).Scan(&def); err != nil {
		t.Fatalf("取 tasks_tracker_provider_check: %v", err)
	}
	if got := strings.Contains(def, "'yunxiao'"); got != present {
		t.Errorf("CHECK 含 'yunxiao' got %v want %v（%s）", got, present, def)
	}
	for _, v := range []string{"'linear'", "'internal'"} {
		if !strings.Contains(def, v) {
			t.Errorf("CHECK 缺 %s（%s）", v, def)
		}
	}
	if !present {
		return
	}

	var trig int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_trigger
		WHERE tgname = 'yunxiao_connections_updated_at' AND tgrelid = 'yunxiao_connections'::regclass`).Scan(&trig); err != nil {
		t.Fatalf("查触发器: %v", err)
	}
	if trig != 1 {
		t.Errorf("触发器 yunxiao_connections_updated_at got %d want 1", trig)
	}

	rows, err := pool.Query(ctx, `
		SELECT a.attname FROM pg_index i
		JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		WHERE i.indrelid = 'yunxiao_trigger_state'::regclass AND i.indisprimary
		ORDER BY k.ord`)
	if err != nil {
		t.Fatalf("查主键: %v", err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	if got, want := strings.Join(cols, ","), "user_id,workitem_id"; got != want {
		t.Errorf("yunxiao_trigger_state 主键列 got %q want %q", got, want)
	}

	var pk string
	if err := pool.QueryRow(ctx, `
		SELECT a.attname FROM pg_index i
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
		WHERE i.indrelid = 'yunxiao_connections'::regclass AND i.indisprimary`).Scan(&pk); err != nil {
		t.Fatalf("查 yunxiao_connections 主键: %v", err)
	}
	if pk != "user_id" {
		t.Errorf("yunxiao_connections 主键 got %q want user_id", pk)
	}
}

// AC-16：up 之后 schema 齐全（CI 先 migrate up 再跑测试，所以它在 CI 里同样成立）。
func TestYunxiaoSchemaPresent(t *testing.T) {
	assertYunxiaoSchema(t, testsupport.Pool(t), true)
}

// AC-16：存在 yunxiao 任务时 0053 的 down 报 23514，不静默删数据。
// 在单个事务里插数据、跑 down、整体回滚，不留残留、不碰迁移记录。
func TestYunxiaoDownRefusesWithYunxiaoTask(t *testing.T) {
	pool := testsupport.Pool(t)
	ctx := context.Background()

	var down string
	ms, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range ms {
		if m.Version == 53 {
			down = m.DownSQL
		}
	}
	if down == "" {
		t.Fatal("找不到 0053 的 down 脚本")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	var uid, rid int64
	if err := tx.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"yx-down-"+nonce+"@example.com").Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO repos (user_id, provider_repo) VALUES ($1, $2) RETURNING id`,
		uid, "acme/yx-down-"+nonce).Scan(&rid); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO tasks (user_id, repo_id, external_key, external_id, tracker_provider)
		VALUES ($1, $2, $3, $3, 'yunxiao')`, uid, rid, "YX-"+nonce); err != nil {
		t.Fatalf("插 yunxiao 任务: %v", err)
	}

	_, err = tx.Exec(ctx, down)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("0053 down got err %v want SQLSTATE 23514", err)
	}
}
