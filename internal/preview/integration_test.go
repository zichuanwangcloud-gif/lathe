package preview

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// 真实 docker 的端到端验证。假件测不出 compose 子命令、!override 标签、
// 项目标签发现这些「与真实 daemon 交互」的行为 —— 2026-08-25 的
// label 过滤器 bug 就是假件全绿、真机全崩的教训。
//
// 跳过条件：docker 不可用，或依赖的镜像未缓存（测试不拉取 ——
// 拉取走网络，不稳定且慢）。

func dockerAvailable(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(t.TempDir(), func(context.Context) (int, int, error) { return 100, 100, nil })
	if _, _, err := m.exec(context.Background(), "docker", "info"); err != nil {
		t.Skip("docker 不可用，跳过集成测试")
	}
	return m
}

func imageCached(t *testing.T, m *Manager, image string) {
	t.Helper()
	out, _, err := m.exec(context.Background(), "docker", "image", "ls", "-q", image)
	if err != nil || strings.TrimSpace(out) == "" {
		t.Skipf("镜像 %s 未缓存，跳过（测试不拉取）", image)
	}
}

// compose 全生命周期：必填变量注入、钉死端口被重置为随机、项目标签
// 可发现、停止后容器与网络清干净、拉取的共享镜像不被误删。
func TestIntegrationComposeLifecycle(t *testing.T) {
	m := dockerAvailable(t)
	imageCached(t, m, "redis:8.4-alpine")

	wt := t.TempDir()
	compose := `services:
  cache:
    image: redis:8.4-alpine
    command: ["redis-server", "--port", "6379"]
    environment:
      - REDIS_PASSWORD=${REDIS_PASSWORD:?必须设置}
      - DEPLOY_REGION=${DEPLOY_REGION:?必须提供}
    ports:
      - "16379:6379"
`
	if err := os.WriteFile(wt+"/docker-compose.yml", []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	taskID := time.Now().UnixNano() // 避开与本机真实预览任务撞项目名

	// 非口令类必填变量缺失 → 拒绝（口令类由系统自动生成，见下）
	if err := m.Start(context.Background(), taskID, wt, StartRequest{
		Selections: []Selection{{Path: "docker-compose.yml", Kind: "compose"}},
	}); err == nil || !strings.Contains(err.Error(), "DEPLOY_REGION") {
		t.Fatalf("缺非口令必填变量应拒绝，得到 %v", err)
	}

	if err := m.Start(context.Background(), taskID, wt, StartRequest{
		Selections: []Selection{{
			Path: "docker-compose.yml", Kind: "compose",
			Env: map[string]string{"DEPLOY_REGION": "test"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	waitOp(t, m, taskID)

	// 状态：容器在跑，端口是随机宿主端口而不是钉死的 16379
	st, err := m.Status(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Containers) != 1 {
		t.Fatalf("应有 1 个容器，得到 %+v", st.Containers)
	}
	c := st.Containers[0]
	if c.State != "running" {
		t.Errorf("容器应 running，得到 %s", c.State)
	}
	if len(c.Ports) != 1 || c.Ports[0].Container != 6379 {
		t.Fatalf("端口映射异常: %+v", c.Ports)
	}
	if c.Ports[0].Host == 16379 || c.Ports[0].Host == 0 {
		t.Errorf("宿主端口应被重置为随机（非钉死的 16379），得到 %d", c.Ports[0].Host)
	}
	// 随机端口真实可连（redis 进程就绪需要一瞬间，轮询而不是假设）
	var probe string
	var probeErr error
	for i := 0; i < 20; i++ {
		var out string
		out, _, probeErr = m.exec(context.Background(), "docker", "exec", c.Name,
			"redis-cli", "-a", "test-pw", "ping")
		if probeErr == nil && strings.Contains(out, "PONG") {
			probe = out
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if probe == "" {
		t.Errorf("redis 应连通: err=%v", probeErr)
	}

	// 停止：容器、网络清掉；redis 镜像是拉取的共享镜像，不能删
	nc, ni, err := m.Stop(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if nc != 1 {
		t.Errorf("应删 1 个容器，得到 %d", nc)
	}
	if ni != 0 {
		t.Errorf("拉取的共享镜像不应被删，得到 %d", ni)
	}
	imageCached(t, m, "redis:8.4-alpine") // 仍在

	st, err = m.Status(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Containers) != 0 {
		t.Errorf("停止后应无容器: %+v", st.Containers)
	}
	if nets, _ := m.networkNames(context.Background(), taskID); len(nets) != 0 {
		t.Errorf("停止后应无网络: %v", nets)
	}
}

// Dockerfile + 附加基础设施：应用容器与 pg 同网络，约定连接串注入，
// pg 就绪后应用才启动。
func TestIntegrationDockerfileWithInfra(t *testing.T) {
	m := dockerAvailable(t)
	imageCached(t, m, "alpine:3.21")
	imageCached(t, m, "postgres:18-alpine")

	wt := t.TempDir()
	// 应用容器只常驻；连通性由测试主动验证（不依赖构建期网络）
	df := `FROM alpine:3.21
CMD ["sleep", "300"]
`
	if err := os.WriteFile(wt+"/Dockerfile", []byte(df), 0o644); err != nil {
		t.Fatal(err)
	}
	taskID := time.Now().UnixNano() + 1

	err := m.Start(context.Background(), taskID, wt, StartRequest{
		Selections: []Selection{{Path: "Dockerfile"}},
		Infra:      []string{"postgres"},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, m, taskID)

	st, err := m.Status(context.Background(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Containers) != 2 {
		t.Fatalf("应有应用 + pg 两个容器，得到 %+v", st.Containers)
	}
	var appName string
	for _, c := range st.Containers {
		if c.State != "running" {
			t.Errorf("容器 %s 应 running，得到 %s", c.Name, c.State)
		}
		if !strings.Contains(c.Name, "infra-") {
			appName = c.Name
		}
	}
	// 约定连接串已注入应用容器
	out, _, err := m.exec(context.Background(), "docker", "inspect", "--format",
		"{{range .Config.Env}}{{println .}}{{end}}", appName)
	if err != nil || !strings.Contains(out, "DATABASE_URL=postgres://lathe:") ||
		!strings.Contains(out, "@pg:5432/app") {
		t.Errorf("应用容器应注入 DATABASE_URL（口令每次启动生成）: out=%q err=%v", out, err)
	}
	// 应用容器能按别名连通 pg（网络 + 别名 + 就绪等待全部生效）
	if _, stderr, err := m.exec(context.Background(), "docker", "exec", appName,
		"nc", "-z", "pg", "5432"); err != nil {
		t.Errorf("应用容器 nc pg:5432 应连通: %s (%v)", stderr, err)
	}

	if _, _, err := m.Stop(context.Background(), taskID); err != nil {
		t.Fatal(err)
	}
	st, _ = m.Status(context.Background(), taskID)
	if len(st.Containers) != 0 {
		t.Errorf("停止后应无容器: %+v", st.Containers)
	}
	fmt.Println("集成测试通过")
}

// 克隆策略全生命周期：源库造数据 → 克隆启动 → 应用连的是克隆库且
// 数据在 → 源库零改动 → 停止清理。这是「有 SQL 变更就拷贝一个库」
// 的端到端证明。
func TestIntegrationCloneDatabase(t *testing.T) {
	m := dockerAvailable(t)
	imageCached(t, m, "postgres:18-alpine")
	imageCached(t, m, "alpine:3.21")

	ctx := context.Background()
	// 造一个带测试数据的「主部署」源库（名字带测试前缀，Stop 不碰它，
	// 由测试自己清理）
	srcName := fmt.Sprintf("lathe-test-clonesrc-%d", time.Now().UnixNano())
	if _, stderr, err := m.exec(ctx, "docker", "run", "-d", "--name", srcName,
		"-e", "POSTGRES_PASSWORD=src-pw", "-e", "POSTGRES_DB=shop", srcNameImage()); err != nil {
		t.Fatalf("起源库失败: %s (%v)", stderr, err)
	}
	t.Cleanup(func() {
		m.exec(context.Background(), "docker", "rm", "-f", srcName)
	})
	// 等就绪并灌一行标志性数据
	ready := false
	for i := 0; i < 60; i++ {
		if _, _, err := m.exec(ctx, "docker", "exec", srcName, "pg_isready", "-U", "postgres"); err == nil {
			ready = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !ready {
		t.Fatal("源库未就绪")
	}
	// pg_isready 可能在 init 临时服务阶段就返回成功（此时自建库还没
	// 建出来），造数据要重试到 init 完成
	var lastErr error
	for i := 0; i < 30; i++ {
		_, stderr, err := m.exec(ctx, "docker", "exec", srcName,
			"psql", "-U", "postgres", "-d", "shop", "-c",
			"CREATE TABLE marker(id int); INSERT INTO marker VALUES (42);")
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = fmt.Errorf("%s (%w)", stderr, err)
		time.Sleep(time.Second)
	}
	if lastErr != nil {
		t.Fatalf("源库造数据失败: %v", lastErr)
	}

	wt := t.TempDir()
	if err := os.WriteFile(wt+"/Dockerfile", []byte("FROM alpine:3.21\nCMD [\"sleep\", \"300\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	taskID := time.Now().UnixNano() + 2

	err := m.Start(ctx, taskID, wt, StartRequest{
		Selections: []Selection{{Path: "Dockerfile"}},
		Database:   &DatabasePlan{Strategy: "clone", Source: srcName},
	})
	if err != nil {
		t.Fatal(err)
	}
	if op := waitOp(t, m, taskID); op != nil && op.State == "failed" {
		t.Fatalf("克隆启动失败: %s", op.Error)
	}

	// 克隆库里应有源库的标志性数据
	cloneName := fmt.Sprintf("lathe-preview-t%d-infra-postgres", taskID)
	out, _, err := m.exec(ctx, "docker", "exec", cloneName,
		"psql", "-U", "postgres", "-d", "shop", "-tAc", "SELECT id FROM marker")
	if err != nil || strings.TrimSpace(out) != "42" {
		t.Errorf("克隆库应含源库数据 marker=42: out=%q err=%v", out, err)
	}
	// 应用容器注入的是克隆库的连接串（pg 别名 + 生成口令），不是源库
	st, _ := m.Status(ctx, taskID)
	var appName string
	for _, c := range st.Containers {
		if !strings.Contains(c.Name, "infra-") {
			appName = c.Name
		}
	}
	envOut, _, _ := m.exec(ctx, "docker", "inspect", "--format",
		"{{range .Config.Env}}{{println .}}{{end}}", appName)
	if !strings.Contains(envOut, "DATABASE_URL=postgres://postgres:") ||
		!strings.Contains(envOut, "@pg:5432/shop") {
		t.Errorf("应用应连克隆库: %s", envOut)
	}
	if strings.Contains(envOut, "src-pw") {
		t.Error("应用不应拿到源库口令")
	}

	if _, _, err := m.Stop(ctx, taskID); err != nil {
		t.Fatal(err)
	}
	// 源库必须还活着且数据在（克隆是拷贝不是剪切）
	out, _, err = m.exec(ctx, "docker", "exec", srcName,
		"psql", "-U", "postgres", "-d", "shop", "-tAc", "SELECT id FROM marker")
	if err != nil || strings.TrimSpace(out) != "42" {
		t.Errorf("源库应不受影响: out=%q err=%v", out, err)
	}
}

func srcNameImage() string { return "postgres:18-alpine" }
