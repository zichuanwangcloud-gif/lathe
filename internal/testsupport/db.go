// Package testsupport 为依赖真实 Postgres 的测试提供统一的连库入口。
//
// 为什么要有这个包：各测试包原先各自写「连不上就 t.Skipf」的 helper，
// 本地开发很好用（不带数据库的环境仍能跑纯逻辑测试），但搬进 CI 就是
// 个陷阱 —— Postgres service 起晚一秒、DSN 写错一个字符，整套数据库
// 测试会被静默跳过，流水线全绿却什么都没验证。接 CI 前实测过：把 DSN
// 指向一个连不通的地址，httpapi / task / store / flow / cmd 五个包
// 在 0.1 秒内全部 ok，而其中 httpapi 带库时要跑 17 秒。
//
// 所以这里把「跳过还是失败」收成一个开关：本地不设 LATHE_TEST_REQUIRE_DB
// 时维持跳过，CI 里设成非空值时同一条路径改为直接失败。
//
// 本包只依赖 pgxpool，不导入 internal/store —— store 自己的测试是包内
// 测试（package store），反向依赖会成环。需要 *store.Store 的测试各自
// 调 store.Open，失败时走本包的 SkipOrFail。
package testsupport

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// EnvDSN 覆盖测试库连接串。
	EnvDSN = "LATHE_TEST_DSN"
	// EnvRequireDB 非空时，连不上库不再跳过而是直接失败。CI 必须设置它。
	EnvRequireDB = "LATHE_TEST_REQUIRE_DB"

	// defaultDSN 与 docker-compose.dev.yml 的端口映射对齐，本地 make dev-infra
	// 起完就能直接跑测试，不必额外导出环境变量。
	defaultDSN = "postgres://lathe:lathe@127.0.0.1:55432/lathe?sslmode=disable"

	// connectTimeout 是建池与 Ping 的上限，沿用各 helper 原有的 5 秒。
	connectTimeout = 5 * time.Second
)

// DSN 返回测试库连接串：优先取 LATHE_TEST_DSN，未设则用本地开发库的默认值。
func DSN() string {
	if dsn := os.Getenv(EnvDSN); dsn != "" {
		return dsn
	}
	return defaultDSN
}

// RequireDB 报告当前是否处于「必须连上数据库」的严格模式。
func RequireDB() bool {
	return os.Getenv(EnvRequireDB) != ""
}

// SkipOrFail 在拿不到数据库时收尾：严格模式下失败，否则跳过。
//
// 两条路径都不返回（Fatalf / Skipf 都会终止当前测试），调用方不必再 return。
func SkipOrFail(tb testing.TB, format string, args ...any) {
	tb.Helper()
	if RequireDB() {
		// 严格模式下把 DSN 一并报出来：CI 里最常见的失败原因就是连串写错，
		// 光看 "connection refused" 判断不了是地址错了还是库没起。
		tb.Fatalf("需要数据库但连不上（%s=%s，因 %s 已设置故不跳过）: "+format,
			append([]any{EnvDSN, DSN(), EnvRequireDB}, args...)...)
	}
	tb.Skipf("跳过数据库测试（先 make dev-infra && make migrate）: "+format, args...)
}

// Pool 建一个连到测试库的连接池，并在测试结束时关闭。
func Pool(tb testing.TB) *pgxpool.Pool {
	tb.Helper()
	return PoolWithMaxConns(tb, 0)
}

// PoolWithMaxConns 同 Pool，但可指定连接池上限。
//
// maxConns <= 0 时用 pgx 的默认值。需要调高的场景见 internal/flow 的并发
// 测试：CreateFlow 期间会独占一条连接持有咨询锁，并发跑时容易在默认上限
// 下出现「所有连接都在等锁，没有连接可用来把持锁的那个请求做完事情」的
// 连接池级死锁，调大上限才能让断言落在咨询锁本身而不是池容量上。
func PoolWithMaxConns(tb testing.TB, maxConns int32) *pgxpool.Pool {
	tb.Helper()

	cfg, err := pgxpool.ParseConfig(DSN())
	if err != nil {
		SkipOrFail(tb, "解析 DSN 失败: %v", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		SkipOrFail(tb, "连接池创建失败: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		SkipOrFail(tb, "Ping 失败: %v", err)
	}
	tb.Cleanup(pool.Close)
	return pool
}

// ConnectContext 返回一个带标准超时的 context，供需要自己调 store.Open
// 的测试复用（本包不能导入 store，见包注释）。
func ConnectContext(tb testing.TB) (context.Context, context.CancelFunc) {
	tb.Helper()
	return context.WithTimeout(context.Background(), connectTimeout)
}
