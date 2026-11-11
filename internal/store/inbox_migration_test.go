package store

import (
	"context"
	"testing"
)

// TestInboxMigrationIndexValid 守住收件箱的两条迁移（0035_inbox_marks、
// 0036_tasks_user_state_index）：应用后对象齐全且索引 VALID；按版本号执行
// down 能完全撤销，再 up 能完全重建。
//
// 只碰本功能自己的两个对象，不读不写 schema_migrations：邻居的迁移先合入、
// MigrateDown 退的是谁，都与这条测试无关。
func TestInboxMigrationIndexValid(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	all, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations 失败: %v", err)
	}
	byVersion := map[int]migration{}
	for _, m := range all {
		byVersion[m.Version] = m
	}
	m35, ok35 := byVersion[35]
	m36, ok36 := byVersion[36]
	if !ok35 || !ok36 || m35.Name != "inbox_marks" || m36.Name != "tasks_user_state_index" {
		t.Fatalf("迁移 0035/0036 未就位: 0035=%q 0036=%q", m35.Name, m36.Name)
	}
	if m35.NoTransaction || !m36.NoTransaction || !m36.NoTransactionDown {
		t.Errorf("事务标记: got 0035.up=%v 0036.up=%v 0036.down=%v want false/true/true",
			m35.NoTransaction, m36.NoTransaction, m36.NoTransactionDown)
	}

	exec := func(t *testing.T, sql string, noTx bool) {
		t.Helper()
		if !noTx {
			if _, err := st.pool.Exec(ctx, sql); err != nil {
				t.Fatalf("执行迁移失败: %v", err)
			}
			return
		}
		// 与 applyOneNoTx / rollbackOneNoTx 同路径：切开逐条无事务执行
		stmts, err := splitStatements(sql)
		if err != nil {
			t.Fatalf("切分语句失败: %v", err)
		}
		for _, s := range stmts {
			if _, err := st.pool.Exec(ctx, s); err != nil {
				t.Fatalf("执行语句失败: %v\n%s", err, s)
			}
		}
	}
	up := func(t *testing.T) {
		t.Helper()
		var has bool
		if err := st.pool.QueryRow(ctx, `SELECT to_regclass('inbox_marks') IS NOT NULL`).Scan(&has); err != nil {
			t.Fatal(err)
		}
		if !has {
			exec(t, m35.UpSQL, m35.NoTransaction)
		}
		exec(t, m36.UpSQL, m36.NoTransaction)
	}
	assertApplied := func(t *testing.T) {
		t.Helper()
		var valid bool
		if err := st.pool.QueryRow(ctx, `
			SELECT COALESCE(bool_and(i.indisvalid), false)
			FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
			WHERE c.relname = 'tasks_user_state'`).Scan(&valid); err != nil {
			t.Fatal(err)
		}
		if !valid {
			t.Errorf("tasks_user_state indisvalid: got=false want=true")
		}
		var cons []string
		if err := st.pool.QueryRow(ctx, `
			SELECT COALESCE(array_agg(conname::text ORDER BY conname), '{}')
			FROM pg_constraint
			WHERE conrelid = to_regclass('inbox_marks')
			  AND conname IN ('inbox_marks_one_subject', 'inbox_marks_nonempty')`).Scan(&cons); err != nil {
			t.Fatal(err)
		}
		if len(cons) != 2 {
			t.Errorf("inbox_marks 约束: got=%v want=[inbox_marks_nonempty inbox_marks_one_subject]", cons)
		}
	}
	gone := func(t *testing.T, rel string) {
		t.Helper()
		var isNull bool
		if err := st.pool.QueryRow(ctx, `SELECT to_regclass($1) IS NULL`, rel).Scan(&isNull); err != nil {
			t.Fatal(err)
		}
		if !isNull {
			t.Errorf("to_regclass(%q): got 非 NULL want NULL", rel)
		}
	}

	// 测试退出时无论成败都把 schema 还原到「已应用」
	t.Cleanup(func() { up(t) })

	t.Run("应用后索引 VALID 且约束齐全", func(t *testing.T) { assertApplied(t) })

	t.Run("按版本号 down 0036、0035 后对象全部消失", func(t *testing.T) {
		exec(t, m36.DownSQL, m36.NoTransactionDown)
		exec(t, m35.DownSQL, m35.NoTransactionDown)
		gone(t, "tasks_user_state")
		gone(t, "inbox_marks")
	})

	t.Run("再 up 0035、0036 后条件重新成立", func(t *testing.T) {
		exec(t, m35.UpSQL, m35.NoTransaction)
		exec(t, m36.UpSQL, m36.NoTransaction)
		assertApplied(t)
	})
}
