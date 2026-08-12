// Package store 提供 Lathe 的 Postgres 访问层。
//
// 查询手写（见 docs/03-tech-stack.md §6），不引入 ORM 与 codegen。
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store 持有连接池，是所有数据访问的入口。
type Store struct {
	pool *pgxpool.Pool
}

// Open 建立连接池并验证连通性。
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("store: 解析 DSN 失败: %w", err)
	}

	// 控制面需长期驻留：限制池上限并设置健康检查周期，
	// 避免连接在 Postgres 侧被回收后仍被复用。
	cfg.MaxConns = 16
	cfg.MinConns = 2
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: 建立连接池失败: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: 连接数据库失败: %w", err)
	}

	return &Store{pool: pool}, nil
}

// Pool 暴露底层连接池，供需要自行控制事务的调用方使用。
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close 释放连接池。
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}
