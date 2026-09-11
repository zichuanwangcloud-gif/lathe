package store

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/Clouditera/lathe/migrations"
)

// noTransactionMarker 是「非事务迁移」的显式开关。
//
// 迁移脚本里任意一行（行首允许缩进，比较时忽略大小写）只写这个标记，
// applyOne 就会逐条语句、无事务地执行它 —— 这是需要的逃生口：
// Postgres 明确禁止在事务块里执行 CREATE INDEX CONCURRENTLY，
// 而大表（agent_events 这类每任务成百上千行）建索引不带 CONCURRENTLY
// 会把该表的写入阻塞到索引建完为止。
//
// 默认行为不变：没写标记的迁移照旧整条包在事务里，
// DDL 与版本记录同时提交，不存在「SQL 跑了但没记账」的半应用状态。
const noTransactionMarker = "-- lathe:no-transaction"

// migration 是一条已解析的迁移脚本。
type migration struct {
	Version int
	Name    string
	UpSQL   string
	DownSQL string

	// NoTransaction 表示该迁移的 up 脚本被标记为无事务执行。
	//
	// 代价要写清楚：无事务就没有「全成或全败」的保证，中断可能留下
	// 半应用状态（对 CONCURRENTLY 而言就是一条 INVALID 索引）。
	// 因此带这个标记的迁移必须自己写成**幂等可重跑**的 ——
	// 重跑时从头再来一遍，由迁移脚本内的 DROP ... IF EXISTS 清掉残骸。
	NoTransaction bool

	// NoTransactionDown 是 down 脚本上的同类标记。
	//
	// 与 up 分开读、不共用：down 里的 DROP INDEX CONCURRENTLY 同样不能进
	// 事务块，而只标了 up 的迁移（比如在大表上建普通索引、回滚用普通 DROP）
	// 没理由因此失去 down 的事务保护。上例 0018 两个方向都带标记。
	NoTransactionDown bool
}

const schemaMigrationsDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version     integer     PRIMARY KEY,
  name        text        NOT NULL,
  applied_at  timestamptz NOT NULL DEFAULT now()
)`

// MigrateUp 按版本号顺序应用所有未应用的迁移。
//
// 默认每条迁移在独立事务中执行：DDL 与版本记录同时提交，
// 中途失败不会留下"SQL 跑了但没记账"的半应用状态。
//
// 例外是标了 noTransactionMarker 的迁移（大表建索引这类不能进事务块的
// 语句需要），它们逐条无事务执行，代价与要求见 applyOneNoTx。
func (s *Store) MigrateUp(ctx context.Context) error {
	all, err := loadMigrations()
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, schemaMigrationsDDL); err != nil {
		return fmt.Errorf("store: 创建 schema_migrations 失败: %w", err)
	}

	applied, err := s.appliedVersions(ctx)
	if err != nil {
		return err
	}

	pending := 0
	for _, m := range all {
		if applied[m.Version] {
			continue
		}
		if strings.TrimSpace(m.UpSQL) == "" {
			return fmt.Errorf("store: 迁移 %04d_%s 缺少 up 脚本", m.Version, m.Name)
		}
		if err := s.applyOne(ctx, m); err != nil {
			return err
		}
		slog.Info("迁移已应用", "version", m.Version, "name", m.Name)
		pending++
	}

	if pending == 0 {
		slog.Info("数据库已是最新，无待应用迁移")
	}
	return nil
}

// MigrateDown 回滚最近一条已应用的迁移。
func (s *Store) MigrateDown(ctx context.Context) error {
	all, err := loadMigrations()
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, schemaMigrationsDDL); err != nil {
		return fmt.Errorf("store: 创建 schema_migrations 失败: %w", err)
	}

	applied, err := s.appliedVersions(ctx)
	if err != nil {
		return err
	}

	// 找出已应用的最高版本
	var target *migration
	for i := len(all) - 1; i >= 0; i-- {
		if applied[all[i].Version] {
			target = &all[i]
			break
		}
	}
	if target == nil {
		slog.Info("没有已应用的迁移可回滚")
		return nil
	}
	if strings.TrimSpace(target.DownSQL) == "" {
		return fmt.Errorf("store: 迁移 %04d_%s 缺少 down 脚本，无法回滚", target.Version, target.Name)
	}

	if target.NoTransactionDown {
		return s.rollbackOneNoTx(ctx, *target)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, target.DownSQL); err != nil {
		return fmt.Errorf("store: 回滚迁移 %04d_%s 失败: %w", target.Version, target.Name, err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, target.Version); err != nil {
		return fmt.Errorf("store: 删除迁移记录失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: 提交回滚失败: %w", err)
	}

	slog.Info("迁移已回滚", "version", target.Version, "name", target.Name)
	return nil
}

// rollbackOneNoTx 回滚一条非事务迁移。
//
// 与 applyOneNoTx 对称：逐条无事务执行 down 脚本，全部成功后才删版本记录。
// 顺序不能反 —— 若先删记录再执行 down 而 down 中途失败，下次 MigrateUp 会
// 把这条迁移当「未应用」重跑 up，而残留的索引还在（up 脚本的
// DROP INDEX IF EXISTS 兜得住，但让 down 也保持「删记录在最后」更一致）。
func (s *Store) rollbackOneNoTx(ctx context.Context, m migration) error {
	stmts, err := splitStatements(m.DownSQL)
	if err != nil {
		return fmt.Errorf("store: 迁移 %04d_%s 的 down 语句切分失败: %w", m.Version, m.Name, err)
	}
	for i, stmt := range stmts {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("store: 非事务回滚迁移 %04d_%s 的第 %d/%d 条语句失败（可安全重跑）: %w",
				m.Version, m.Name, i+1, len(stmts), err)
		}
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, m.Version); err != nil {
		return fmt.Errorf("store: 删除迁移记录失败: %w", err)
	}

	slog.Info("迁移已回滚（非事务模式）", "version", m.Version, "name", m.Name)
	return nil
}

func (s *Store) applyOne(ctx context.Context, m migration) error {
	if m.NoTransaction {
		return s.applyOneNoTx(ctx, m)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, m.UpSQL); err != nil {
		return fmt.Errorf("store: 应用迁移 %04d_%s 失败: %w", m.Version, m.Name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`,
		m.Version, m.Name,
	); err != nil {
		return fmt.Errorf("store: 记录迁移 %04d 失败: %w", m.Version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: 提交迁移 %04d 失败: %w", m.Version, err)
	}
	return nil
}

// applyOneNoTx 逐条语句、无事务地执行迁移（见 noTransactionMarker）。
//
// 两条实现约束，都是 CONCURRENTLY 逼出来的：
//
//  1. 语句必须**一条一条**发。pgx 走简单查询协议发多语句时，服务端会把
//     整批当成一个隐式事务 —— 那就等于没退出事务块，CONCURRENTLY 照样报
//     "cannot run inside a transaction block"。所以这里不把整个文件丢给
//     Exec，而是先切开再逐条执行。
//  2. 版本记录必须在**全部语句成功之后**单独插入。中断（context 超时、
//     进程被杀）时不会留下「半应用却已记账」的迁移：脚本没跑完 ⇒ 没记账
//     ⇒ 下次 MigrateUp 仍视其为待应用并从头重跑。幂等性因此由迁移脚本
//     自己保证（见 noTransactionMarker 的注释）。
func (s *Store) applyOneNoTx(ctx context.Context, m migration) error {
	stmts, err := splitStatements(m.UpSQL)
	if err != nil {
		return fmt.Errorf("store: 迁移 %04d_%s 的语句切分失败: %w", m.Version, m.Name, err)
	}
	if len(stmts) == 0 {
		return fmt.Errorf("store: 迁移 %04d_%s 标记了非事务执行，却没有可执行语句", m.Version, m.Name)
	}

	slog.Info("以非事务模式应用迁移（逐条执行，中断需靠脚本自身幂等恢复）",
		"version", m.Version, "name", m.Name, "statements", len(stmts))

	for i, stmt := range stmts {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("store: 非事务应用迁移 %04d_%s 的第 %d/%d 条语句失败（该迁移可安全重跑）: %w",
				m.Version, m.Name, i+1, len(stmts), err)
		}
	}

	if _, err := s.pool.Exec(ctx,
		`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`,
		m.Version, m.Name,
	); err != nil {
		return fmt.Errorf("store: 记录迁移 %04d 失败（SQL 已执行，重跑前需确认脚本幂等）: %w", m.Version, err)
	}
	return nil
}

// splitStatements 把一段 SQL 脚本按语句切开，返回去掉注释与空白后的语句。
//
// 只在非事务迁移里用得上，所以刻意保持朴素：它需要正确跳过字符串字面量、
// 注释与 dollar-quoted 块（本项目 0001 的 plpgsql 函数体就是这么包的），
// 但不做语法解析 —— 真正的语法检查交给 Postgres。
//
// 不能只按 ';' 切：`COMMENT ON ... IS 'a;b'` 或函数体里的分号会被切断。
func splitStatements(sql string) ([]string, error) {
	var stmts []string
	var cur strings.Builder
	runes := []rune(sql)

	for i := 0; i < len(runes); {
		c := runes[i]

		// 行注释：跳到行尾（保留换行，语句之间的分隔靠它）
		if c == '-' && i+1 < len(runes) && runes[i+1] == '-' {
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
			continue
		}
		// 块注释：Postgres 的块注释可嵌套
		if c == '/' && i+1 < len(runes) && runes[i+1] == '*' {
			depth := 1
			i += 2
			for i < len(runes) && depth > 0 {
				switch {
				case runes[i] == '/' && i+1 < len(runes) && runes[i+1] == '*':
					depth++
					i += 2
				case runes[i] == '*' && i+1 < len(runes) && runes[i+1] == '/':
					depth--
					i += 2
				default:
					i++
				}
			}
			if depth > 0 {
				return nil, fmt.Errorf("块注释未闭合")
			}
			continue
		}
		// 字符串字面量 / 引号标识符：'' 与 "" 是转义写法
		if c == '\'' || c == '"' {
			quote := c
			cur.WriteRune(c)
			i++
			for i < len(runes) {
				cur.WriteRune(runes[i])
				if runes[i] == quote {
					if i+1 < len(runes) && runes[i+1] == quote {
						cur.WriteRune(runes[i+1])
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		}
		// dollar-quoted 块：$tag$ ... $tag$
		//
		// 全程用 rune 下标：中文注释在迁移文件里到处都是，而 strings.Index
		// 返回的是字节偏移 —— 两者混用会在「块内出现多字节字符」时切错位置
		// （注释里的中文把字节偏移推得比 rune 下标大，末尾就会越界 panic）。
		if c == '$' {
			if tag, ok := dollarTagAt(runes, i); ok {
				tagRunes := []rune(tag)
				cur.WriteString(tag)
				i += len(tagRunes)
				end := indexRunes(runes[i:], tagRunes)
				if end < 0 {
					return nil, fmt.Errorf("dollar-quoted 块 %s 未闭合", tag)
				}
				cur.WriteString(string(runes[i : i+end]))
				cur.WriteString(tag)
				i += end + len(tagRunes)
				continue
			}
		}
		// 语句分隔符
		if c == ';' {
			if s := strings.TrimSpace(cur.String()); s != "" {
				stmts = append(stmts, s)
			}
			cur.Reset()
			i++
			continue
		}
		cur.WriteRune(c)
		i++
	}

	if s := strings.TrimSpace(cur.String()); s != "" {
		stmts = append(stmts, s)
	}
	return stmts, nil
}

// indexRunes 在 runes 中查找 needle 首次出现的 **rune 下标**（不是字节偏移）。
// 用 strings.Index 得到字节偏移再拿去切 []rune 会在多字节字符上错位。
func indexRunes(runes, needle []rune) int {
	if len(needle) == 0 {
		return 0
	}
	for i := 0; i+len(needle) <= len(runes); i++ {
		match := true
		for j := range needle {
			if runes[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// dollarTagAt 判断 pos 处是否是一个 dollar-quote 起始标记（$$ 或 $tag$），
// 是则返回标记本身。标记内只允许字母数字与下划线，且不能以数字开头。
func dollarTagAt(runes []rune, pos int) (string, bool) {
	if runes[pos] != '$' {
		return "", false
	}
	for j := pos + 1; j < len(runes); j++ {
		switch r := runes[j]; {
		case r == '$':
			return string(runes[pos : j+1]), true
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			continue
		default:
			return "", false
		}
	}
	return "", false
}

// HasNoTransactionMarker 报告脚本里是否出现了非事务标记。
//
// 逐行比对而非全文件子串搜索：标记必须独占一行，避免「在注释里提到这个
// 标记」（比如本文件的说明、或某条迁移解释自己为什么不带标记）被误判成
// 真的开了非事务模式。行尾的 CR 与左右空白一并忽略。
func HasNoTransactionMarker(sql string) bool {
	for _, line := range strings.Split(sql, "\n") {
		if strings.EqualFold(strings.TrimSpace(line), noTransactionMarker) {
			return true
		}
	}
	return false
}

func (s *Store) appliedVersions(ctx context.Context) (map[int]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询已应用迁移失败: %w", err)
	}
	defer rows.Close()

	applied := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("store: 读取迁移版本失败: %w", err)
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// loadMigrations 解析 embed.FS 中的迁移脚本，按版本号升序返回。
//
// 文件名格式：<版本>_<名字>.(up|down).sql，例如 0001_init.up.sql
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("store: 读取内嵌迁移目录失败: %w", err)
	}

	byVersion := make(map[int]*migration)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, direction, err := parseMigrationName(e.Name())
		if err != nil {
			return nil, err
		}

		content, err := fs.ReadFile(migrations.FS, e.Name())
		if err != nil {
			return nil, fmt.Errorf("store: 读取 %s 失败: %w", e.Name(), err)
		}

		m, ok := byVersion[version]
		if !ok {
			m = &migration{Version: version, Name: name}
			byVersion[version] = m
		}
		if m.Name != name {
			return nil, fmt.Errorf("store: 版本 %04d 存在两个不同名字 %q 与 %q", version, m.Name, name)
		}
		switch direction {
		case "up":
			m.UpSQL = string(content)
			m.NoTransaction = HasNoTransactionMarker(string(content))
		case "down":
			m.DownSQL = string(content)
			m.NoTransactionDown = HasNoTransactionMarker(string(content))
		}
	}

	out := make([]migration, 0, len(byVersion))
	for _, m := range byVersion {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func parseMigrationName(filename string) (version int, name, direction string, err error) {
	base := strings.TrimSuffix(filename, ".sql")

	dot := strings.LastIndex(base, ".")
	if dot < 0 {
		return 0, "", "", fmt.Errorf("store: 迁移文件名 %q 缺少方向后缀（应为 .up.sql 或 .down.sql）", filename)
	}
	direction = base[dot+1:]
	if direction != "up" && direction != "down" {
		return 0, "", "", fmt.Errorf("store: 迁移文件名 %q 的方向 %q 非法（只允许 up/down）", filename, direction)
	}

	rest := base[:dot]
	us := strings.Index(rest, "_")
	if us <= 0 {
		return 0, "", "", fmt.Errorf("store: 迁移文件名 %q 缺少 <版本>_<名字> 结构", filename)
	}
	version, err = strconv.Atoi(rest[:us])
	if err != nil {
		return 0, "", "", fmt.Errorf("store: 迁移文件名 %q 的版本号无法解析: %w", filename, err)
	}
	name = rest[us+1:]
	if name == "" {
		return 0, "", "", fmt.Errorf("store: 迁移文件名 %q 缺少名字", filename)
	}
	return version, name, direction, nil
}
