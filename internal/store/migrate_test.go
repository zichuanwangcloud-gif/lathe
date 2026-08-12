package store

import (
	"strings"
	"testing"
)

func TestParseMigrationName(t *testing.T) {
	ok := []struct {
		file      string
		version   int
		name      string
		direction string
	}{
		{"0001_init.up.sql", 1, "init", "up"},
		{"0001_init.down.sql", 1, "init", "down"},
		{"0042_add_nodes.up.sql", 42, "add_nodes", "up"},
		{"0007_multi_word_name.down.sql", 7, "multi_word_name", "down"},
	}
	for _, tc := range ok {
		t.Run(tc.file, func(t *testing.T) {
			v, n, d, err := parseMigrationName(tc.file)
			if err != nil {
				t.Fatalf("parseMigrationName(%q) 报错: %v", tc.file, err)
			}
			if v != tc.version || n != tc.name || d != tc.direction {
				t.Errorf("得到 (%d, %q, %q)，期望 (%d, %q, %q)",
					v, n, d, tc.version, tc.name, tc.direction)
			}
		})
	}

	bad := []struct {
		file    string
		wantErr string
	}{
		{"0001_init.sql", "方向"},          // 缺方向后缀（.sql 剥掉后无 . 分隔）
		{"0001_init.sideways.sql", "非法"}, // 方向不是 up/down
		{"init.up.sql", "结构"},            // 无下划线，先撞"缺少 <版本>_<名字> 结构"
		{"9x_init.up.sql", "版本号"},        // 有下划线但版本号不是数字
		{"_init.up.sql", "结构"},           // 版本号为空
		{"0001.up.sql", "结构"},            // 缺 _名字
		{"0001_.up.sql", "名字"},           // 名字为空
	}
	for _, tc := range bad {
		t.Run("拒绝_"+tc.file, func(t *testing.T) {
			_, _, _, err := parseMigrationName(tc.file)
			if err == nil {
				t.Fatalf("parseMigrationName(%q) 应报错，却通过了", tc.file)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("错误 = %v，期望包含 %q", err, tc.wantErr)
			}
		})
	}
}

// loadMigrations 必须能解析内嵌的真实迁移，且 up/down 成对、版本升序。
func TestLoadMigrations(t *testing.T) {
	ms, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations() 报错: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("loadMigrations() 返回空，内嵌迁移未被打进二进制？")
	}

	for i, m := range ms {
		if i > 0 && ms[i-1].Version >= m.Version {
			t.Errorf("迁移未按版本升序: 第 %d 条 %d 不大于前一条 %d", i, m.Version, ms[i-1].Version)
		}
		if strings.TrimSpace(m.UpSQL) == "" {
			t.Errorf("迁移 %04d_%s 缺 up 脚本", m.Version, m.Name)
		}
		if strings.TrimSpace(m.DownSQL) == "" {
			t.Errorf("迁移 %04d_%s 缺 down 脚本（回滚能力是硬要求）", m.Version, m.Name)
		}
	}

	// 首条迁移必须建出设计文档 §4 约定的全部 8 张表
	first := ms[0]
	for _, table := range []string{
		"users", "integrations", "repos", "nodes",
		"tasks", "task_events", "verifications", "webhook_deliveries",
	} {
		if !strings.Contains(first.UpSQL, "CREATE TABLE "+table) {
			t.Errorf("初始迁移未创建表 %q", table)
		}
	}

	// 状态机的 11 个状态必须全部出现在 CHECK 约束里
	for _, state := range []string{
		"queued", "triaging", "blocked_spec", "awaiting_approval",
		"implementing", "verifying", "pr_open", "review_feedback",
		"merged", "failed", "cancelled",
	} {
		if !strings.Contains(first.UpSQL, "'"+state+"'") {
			t.Errorf("初始迁移的 state CHECK 约束缺少状态 %q", state)
		}
	}
}
