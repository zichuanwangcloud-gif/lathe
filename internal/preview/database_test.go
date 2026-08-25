package preview

import (
	"context"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- 改动画像

func TestProfileFromFiles(t *testing.T) {
	p := profileFromFiles("origin/main", []string{
		"apps/console-v2/backend/internal/x.go",
		"apps/console-v2/migrations/0042_add_col.up.sql",
		"README.md",
		"internal/runner/pipeline.go",
	})
	if !p.HasSQL {
		t.Error("migrations/*.sql 应识别为 SQL 变更")
	}
	found := map[string]bool{}
	for _, a := range p.Apps {
		found[a] = true
	}
	if !found["apps/console-v2"] || !found["internal"] {
		t.Errorf("应用提取不符: %v", p.Apps)
	}

	// 无 SQL 的普通改动
	p2 := profileFromFiles("origin/main", []string{"apps/portal/x.vue", "docs/02-design.md"})
	if p2.HasSQL {
		t.Error("普通改动不应标记 SQL 变更")
	}
}

// ---------------------------------------------------------------- mapVar

func TestMapVarFamilyAware(t *testing.T) {
	r := &resolvedDatabase{
		plan: DatabasePlan{Strategy: "reuse"},
		host: "host.docker.internal", port: "55432",
		user: "lathe", password: "pw123", dbName: "app",
	}
	cases := []struct {
		name string
		want string
		ok   bool
	}{
		{"DATABASE_HOST", "host.docker.internal", true},
		{"DATABASE_PORT", "55432", true},
		{"DATABASE_USER", "lathe", true},
		{"DATABASE_PASSWORD", "pw123", true},
		{"POSTGRES_PASSWORD", "pw123", true},
		{"DATABASE_NAME", "app", true},
		{"PGHOST", "host.docker.internal", true},
		// 其他中间件家族不被 postgres 策略吃掉
		{"REDIS_HOST", "", false},
		{"REDIS_PASSWORD", "", false},
		{"KAFKA_BROKER", "", false},
		// JWT_SECRET 这类不映射 DB 口令（走自动生成）
		{"JWT_SECRET", "", false},
		// 无关变量不映射
		{"DEPLOY_REGION", "", false},
	}
	for _, c := range cases {
		got, ok := r.mapVar(c.name)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("mapVar(%s) = %q,%v，期望 %q,%v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// ---------------------------------------------------------------- resolveDatabase

// fake 出「一个在跑的 postgres 主部署」的 Manager。
func newDBTestManager(t *testing.T, gitDiff string) (*Manager, string) {
	t.Helper()
	fd := &fakeDocker{outputs: map[string]fakeResult{
		"ps": {stdout: "lathe-postgres-dev\tpostgres:18-alpine\t0.0.0.0:55432->5432/tcp\tlathe\n"},
		"inspect": {stdout: `["POSTGRES_USER=lathe","POSTGRES_PASSWORD=dev-pw","POSTGRES_DB=lathe"]` +
			`|{"5432/tcp":[{"HostIp":"0.0.0.0","HostPort":"55432"}]}` + "\n"},
		"rev-parse": {stdout: "abc123\n"},
		"diff":      {stdout: gitDiff},
	}}
	return newTestManager(t, fd, 100, 100)
}

func TestResolveReuse(t *testing.T) {
	m, wt := newDBTestManager(t, "apps/console-v2/x.go\n") // 无 SQL
	db, err := m.resolveDatabase(context.Background(), 7, wt, DatabasePlan{
		Strategy: "reuse", Source: "lathe-postgres-dev",
	})
	if err != nil {
		t.Fatal(err)
	}
	if db.host != "host.docker.internal" || db.port != "55432" {
		t.Errorf("reuse 应经宿主网关连源库端口: %+v", db)
	}
	if db.user != "lathe" || db.password != "dev-pw" || db.dbName != "lathe" {
		t.Errorf("凭据应从源容器 inspect 读出: %+v", db)
	}
	if !db.hostGateway {
		t.Error("reuse 需要 host-gateway 解析")
	}
	if db.env["DATABASE_URL"] != "postgres://lathe:dev-pw@host.docker.internal:55432/lathe" {
		t.Errorf("DATABASE_URL 不符: %v", db.env["DATABASE_URL"])
	}
}

// 硬护栏：有 SQL 变更时 reuse 在执行前被机械拒绝（不信上游计划）。
func TestResolveReuseRejectedWithSQL(t *testing.T) {
	m, wt := newDBTestManager(t, "apps/x/migrations/0001_init.up.sql\n")
	_, err := m.resolveDatabase(context.Background(), 7, wt, DatabasePlan{
		Strategy: "reuse", Source: "lathe-postgres-dev",
	})
	if err == nil || !strings.Contains(err.Error(), "SQL") {
		t.Fatalf("有 SQL 变更时 reuse 应被拒绝，得到 %v", err)
	}
}

func TestResolveClone(t *testing.T) {
	m, wt := newDBTestManager(t, "apps/x/migrations/0001.up.sql\n") // 有 SQL：正该克隆
	db, err := m.resolveDatabase(context.Background(), 7, wt, DatabasePlan{
		Strategy: "clone", Source: "lathe-postgres-dev",
	})
	if err != nil {
		t.Fatal(err)
	}
	if db.host != "pg" || db.user != "postgres" || db.dbName != "lathe" {
		t.Errorf("克隆库连接参数不符: %+v", db)
	}
	if db.password == "" || db.password == "dev-pw" {
		t.Error("克隆库应用全新生成的口令，不复用源口令")
	}
	if db.srcImage != "postgres:18-alpine" || db.srcDB != "lathe" {
		t.Errorf("克隆源参数不符: %+v", db)
	}
	if db.cloneCname != "lathe-preview-t7-infra-postgres" {
		t.Errorf("克隆容器名不符: %s", db.cloneCname)
	}
}

func TestResolveSourceValidation(t *testing.T) {
	m, wt := newDBTestManager(t, "")
	// 源容器不存在
	if _, err := m.resolveDatabase(context.Background(), 7, wt, DatabasePlan{
		Strategy: "reuse", Source: "不存在",
	}); err == nil {
		t.Error("不存在的源容器应拒绝")
	}
}

// compose 必填变量解析：数据库策略映射 + 口令类自动生成 + 其余拒绝。
func TestResolveComposeEnv(t *testing.T) {
	mkPlan := func() *composePlan {
		return &composePlan{
			sel: Selection{Path: "docker-compose.yml", Kind: "compose",
				Env: map[string]string{"DEPLOY_REGION": "cn"}},
			envs: []EnvVarSpec{
				{Name: "DATABASE_HOST", Required: true},
				{Name: "DATABASE_PASSWORD", Required: true},
				{Name: "JWT_SECRET", Required: true},
				{Name: "DEPLOY_REGION", Required: true},
			},
		}
	}
	db := &resolvedDatabase{
		plan: DatabasePlan{Strategy: "reuse"},
		host: "host.docker.internal", port: "55432",
		user: "lathe", password: "dev-pw", dbName: "lathe",
	}

	cp := mkPlan()
	if err := resolveComposeEnv(cp, db); err != nil {
		t.Fatal(err)
	}
	// 策略映射：host 与 password 来自复用容器
	if cp.sel.Env["DATABASE_HOST"] != "host.docker.internal" {
		t.Errorf("DATABASE_HOST 应映射复用宿主: %v", cp.sel.Env)
	}
	if cp.sel.Env["DATABASE_PASSWORD"] != "dev-pw" {
		t.Errorf("DATABASE_PASSWORD 应取源容器凭据: %v", cp.sel.Env)
	}
	// 口令类但非 DB 口令词（JWT_SECRET）→ 自动生成且留痕
	if v := cp.sel.Env["JWT_SECRET"]; len(v) != 32 {
		t.Errorf("JWT_SECRET 应自动生成 32 位口令，得到 %q", v)
	}
	if len(db.autoFilled) != 1 || db.autoFilled[0] != "JWT_SECRET" {
		t.Errorf("自动生成应留痕: %v", db.autoFilled)
	}
	// 人已填的不动
	if cp.sel.Env["DEPLOY_REGION"] != "cn" {
		t.Errorf("人填的值不应被覆盖: %v", cp.sel.Env)
	}

	// 无数据库策略时，DB 口令类也自动生成（compose 自己起库的场景）
	cp2 := &composePlan{
		sel:  Selection{Path: "c.yml", Kind: "compose"},
		envs: []EnvVarSpec{{Name: "POSTGRES_PASSWORD", Required: true}},
	}
	db2 := &resolvedDatabase{}
	if err := resolveComposeEnv(cp2, db2); err != nil {
		t.Fatal(err)
	}
	if len(cp2.sel.Env["POSTGRES_PASSWORD"]) != 32 {
		t.Error("无策略时 POSTGRES_PASSWORD 应自动生成")
	}

	// 非口令、策略不映射 → 拒绝并点名
	cp3 := &composePlan{
		sel:  Selection{Path: "c.yml", Kind: "compose"},
		envs: []EnvVarSpec{{Name: "DEPLOY_REGION", Required: true}},
	}
	if err := resolveComposeEnv(cp3, &resolvedDatabase{}); err == nil ||
		!strings.Contains(err.Error(), "DEPLOY_REGION") {
		t.Errorf("应拒绝并点名 DEPLOY_REGION: %v", err)
	}
}
