package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func vcsTestUser(t *testing.T, s *Store) int64 {
	t.Helper()
	var id int64
	email := "vcs-" + t.Name() + "-" + strconv.FormatInt(time.Now().UnixNano(), 10) + "@example.com"
	if err := s.Pool().QueryRow(context.Background(), `INSERT INTO users (email) VALUES ($1) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	t.Cleanup(func() { _, _ = s.Pool().Exec(context.Background(), `DELETE FROM users WHERE id=$1`, id) })
	return id
}

// AC-13：repos_vcs_platform_check 拒绝平台与实例地址不配套的行。
func TestRepoPlatformCheckConstraint(t *testing.T) {
	s := testStore(t)
	userID := vcsTestUser(t, s)
	cases := []struct {
		name     string
		provider string
		base     *string
		ok       bool
	}{
		{"github 带地址被拒", "github", strPtr("https://x.example.com"), false},
		{"gitlab 无地址被拒", "gitlab", nil, false},
		{"未知平台被拒", "bitbucket", strPtr("https://x.example.com"), false},
		{"gitlab 地址过短被拒", "gitlab", strPtr("https:/"), false},
		{"gitlab 带地址通过", "gitlab", strPtr("https://gitlab.example.com"), true},
		{"github 无地址通过", "github", nil, true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Pool().Exec(context.Background(),
				`INSERT INTO repos (user_id, provider_repo, vcs_provider, vcs_base_url) VALUES ($1,$2,$3,$4)`,
				userID, "chk/r"+strconv.Itoa(i), tc.provider, tc.base)
			if tc.ok {
				if err != nil {
					t.Fatalf("应插入成功: %v", err)
				}
				return
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
				t.Fatalf("err = %v，want SQLSTATE 23514", err)
			}
		})
	}
}

// AC-22：有 GitLab/Gitea 仓库时 0050 down 拒绝执行，且事务回滚、表结构与数据不变。
func TestMigration0050DownRefusesWithNonGitHubRepos(t *testing.T) {
	s := testStore(t)
	userID := vcsTestUser(t, s)
	path := "grp-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "/down-guard"
	if _, err := s.Pool().Exec(context.Background(),
		`INSERT INTO repos (user_id, provider_repo, vcs_provider, vcs_base_url) VALUES ($1,$2,'gitlab','https://gitlab.example.com')`,
		userID, path); err != nil {
		t.Fatalf("建 gitlab 仓库失败: %v", err)
	}

	migs, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var down string
	for _, m := range migs {
		if m.Version == 50 {
			down = m.DownSQL
		}
	}
	if down == "" {
		t.Fatal("找不到 0050 的 down 脚本")
	}

	ctx := context.Background()
	tx, err := s.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, execErr := tx.Exec(ctx, down)
	_ = tx.Rollback(ctx)
	if execErr == nil || !strings.Contains(execErr.Error(), path) {
		t.Fatalf("0050 down 应报错且信息含仓库路径 %q，得到 %v", path, execErr)
	}

	var p, b string
	if err := s.Pool().QueryRow(ctx, `SELECT vcs_provider, vcs_base_url FROM repos WHERE user_id=$1 AND provider_repo=$2`,
		userID, path).Scan(&p, &b); err != nil {
		t.Fatalf("down 失败后仓库行应原样存在: %v", err)
	}
	if p != "gitlab" || b != "https://gitlab.example.com" {
		t.Errorf("数据被改动：(%s, %s)", p, b)
	}
}

func strPtr(s string) *string { return &s }
