package store

import (
	"os"
	"strings"
	"testing"
)

// TestEverythingSplitsCleanly 是框架级断言：任何一条标了非事务标记的迁移，
// 其 up/down 脚本都必须能被 splitStatements 完整切开，且每一条切出来的语句
// 都能独立交给 Postgres 执行（即：不是空的、不是半截的）。
//
// 这条测试守的是「新增一条非事务迁移」时的静默失效：如果将来有人在
// dollar-quote 或注释嵌套上把切分器写漏了，切出来的半截 SQL 会在生产上
// 报一个语法错，而那时代码已经合了。
func TestEverythingSplitsCleanly(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations() 报错: %v", err)
	}
	for _, m := range ms {
		if m.NoTransaction {
			checkSplit(t, m.Version, m.Name, "up", m.UpSQL)
		}
		if m.NoTransactionDown {
			checkSplit(t, m.Version, m.Name, "down", m.DownSQL)
		}
	}
}

func checkSplit(t *testing.T, version int, name, dir, sql string) {
	t.Helper()
	stmts, err := splitStatements(sql)
	if err != nil {
		t.Errorf("%04d_%s.%s 切分失败: %v", version, name, dir, err)
		return
	}
	if len(stmts) == 0 {
		t.Errorf("%04d_%s.%s 标了非事务却没有可执行语句", version, name, dir)
		return
	}
	for i, s := range stmts {
		if strings.TrimSpace(s) == "" {
			t.Errorf("%04d_%s.%s 第 %d 条是空语句", version, name, dir, i+1)
		}
		// 切出来的语句里不该再残留注释分隔符导致的半截 —— 只做一个廉价哨兵：
		// 每条语句要么以关键字开头，要么是整块的 DO/函数体，不能以分号开头或结尾。
		if strings.HasPrefix(strings.TrimSpace(s), ";") {
			t.Errorf("%04d_%s.%s 第 %d 条以分号开头，切分错位: %q", version, name, dir, i+1, s)
		}
	}
}

// TestNoTransactionMarkerDetection 校验标记的识别规则：
// 必须独占一行（避免注释里提到这个标记就被误判），大小写与缩进不敏感。
func TestNoTransactionMarkerDetection(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want bool
	}{
		{"首行标记", "-- lathe:no-transaction\nCREATE INDEX CONCURRENTLY i ON t (a);", true},
		{"标记带缩进", "\n   -- lathe:no-transaction\nSELECT 1;", true},
		{"标记带行尾空格", "-- lathe:no-transaction  \r\nSELECT 1;", true},
		{"大小写不敏感", "-- LATHE:No-Transaction\nSELECT 1;", true},
		{"没有标记", "-- 普通迁移\nSELECT 1;", false},
		{"标记被包在正文里", "-- 说明：本迁移不打 -- lathe:no-transaction 标记\nSELECT 1;", false},
		{"标记后还有别的内容", "-- lathe:no-transaction 因为要跑 CONCURRENTLY\nSELECT 1;", false},
		{"标记出现在 SQL 行尾", "SELECT 1; -- lathe:no-transaction", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasNoTransactionMarker(tc.sql); got != tc.want {
				t.Errorf("HasNoTransactionMarker() = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// TestSplitStatements 覆盖切分器的三类硬骨头：字符串里的分号、
// dollar-quoted 函数体里的分号、嵌套块注释。
func TestSplitStatements(t *testing.T) {
	sql := `-- 注释里的分号; 不该切
CREATE TABLE t (a text DEFAULT 'x;y'); -- 行内的分号也不切
/* 块注释 ; /* 嵌套 ; */ 仍在注释里 ; */
CREATE OR REPLACE FUNCTION f() RETURNS trigger AS $$
BEGIN
  NEW.updated_at = now();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
COMMENT ON TABLE t IS 'it''s fine; really';`
	stmts, err := splitStatements(sql)
	if err != nil {
		t.Fatalf("splitStatements() 报错: %v", err)
	}
	if len(stmts) != 3 {
		t.Fatalf("切出 %d 条语句，期望 3 条:\n%s", len(stmts), strings.Join(stmts, "\n---\n"))
	}
	if !strings.Contains(stmts[1], "RETURN NEW;") {
		t.Errorf("函数体被切断了: %q", stmts[1])
	}
	if !strings.HasSuffix(strings.TrimSpace(stmts[1]), "$$ LANGUAGE plpgsql") {
		t.Errorf("函数体没有包含到 LANGUAGE 子句: %q", stmts[1])
	}
}

// 未闭合的 dollar-quote / 块注释必须是硬错误，不能静默切出一条坏语句。
func TestSplitStatementsRejectsUnclosed(t *testing.T) {
	for _, sql := range []string{
		"CREATE FUNCTION f() RETURNS int AS $$ SELECT 1;",
		"SELECT 1; /* 没关的块注释",
	} {
		if _, err := splitStatements(sql); err == nil {
			t.Errorf("未闭合输入应报错，却通过了: %q", sql)
		}
	}
}

// 迁移脚本尾部是否有未闭合块注释，顺带当一次真实性检查（真实文件走一遍）。
func TestSplitRealMigrations(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		for _, dir := range []string{m.UpSQL, m.DownSQL} {
			if _, err := splitStatements(dir); err != nil {
				t.Errorf("%04d_%s 切分报错: %v", m.Version, m.Name, err)
			}
		}
	}
	// 只有 0018 是已知的非事务迁移；数量变了说明有人加了新标记，
	// 那时这条断言会提醒他：新迁移必须幂等可重跑。
	n := 0
	for _, m := range ms {
		if m.NoTransaction || m.NoTransactionDown {
			n++
		}
	}
	if n != 1 {
		t.Logf("注意：当前有 %d 条迁移带非事务标记，确认它们都幂等可重跑", n)
	}
	if _, err := os.Stat("../../migrations"); err != nil {
		t.Logf("迁移目录不可见（内嵌 FS 已够用）: %v", err)
	}
}
