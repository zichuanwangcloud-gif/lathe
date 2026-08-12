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

// migration 是一条已解析的迁移脚本。
type migration struct {
	Version int
	Name    string
	UpSQL   string
	DownSQL string
}

const schemaMigrationsDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version     integer     PRIMARY KEY,
  name        text        NOT NULL,
  applied_at  timestamptz NOT NULL DEFAULT now()
)`

// MigrateUp 按版本号顺序应用所有未应用的迁移。
//
// 每条迁移在独立事务中执行：DDL 与版本记录同时提交，
// 中途失败不会留下"SQL 跑了但没记账"的半应用状态。
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

func (s *Store) applyOne(ctx context.Context, m migration) error {
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
		case "down":
			m.DownSQL = string(content)
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
