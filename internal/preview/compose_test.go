package preview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile 在 worktree 里写测试夹具文件（自动建父目录）。
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	abs := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanComposeEnv(t *testing.T) {
	content := `
services:
  web:
    image: ${REGISTRY:-docker.io}/app:${TAG}
    environment:
      - DB_HOST=${DATABASE_HOST:?必须提供}
      - DB_PORT=${DATABASE_PORT:-5432}
      - CACHE=${REDIS_URL}
      - LITERAL=$${NOT_A_VAR}
`
	envs := ScanComposeEnv(content)
	byName := map[string]EnvVarSpec{}
	for _, e := range envs {
		byName[e.Name] = e
	}

	// ${VAR:?} 与 ${VAR} 无默认值 → 必填
	if !byName["DATABASE_HOST"].Required {
		t.Error("DATABASE_HOST 应必填（:? 形态）")
	}
	if !byName["TAG"].Required {
		t.Error("TAG 应必填（裸 ${VAR} 形态）")
	}
	if !byName["REDIS_URL"].Required {
		t.Error("REDIS_URL 应必填")
	}
	// ${VAR:-x} → 可选并预填默认值
	if byName["DATABASE_PORT"].Required || byName["DATABASE_PORT"].Default != "5432" {
		t.Errorf("DATABASE_PORT 应可选且默认 5432，得到 %+v", byName["DATABASE_PORT"])
	}
	if byName["REGISTRY"].Default != "docker.io" {
		t.Errorf("REGISTRY 默认 docker.io，得到 %+v", byName["REGISTRY"])
	}
	// $$ 是字面美元符转义，不是变量引用
	if _, ok := byName["NOT_A_VAR"]; ok {
		t.Error("$$ 转义不应识别为变量")
	}
}

func TestBuildOverrideYAML(t *testing.T) {
	// 有端口的服务重置为随机宿主端口
	y := buildOverrideYAML(map[string][]int{"web": {8080}, "db": nil})
	if !strings.Contains(y, "!override") || !strings.Contains(y, `"0:8080"`) {
		t.Errorf("override 应含随机端口重置:\n%s", y)
	}
	if strings.Contains(y, `"db"`) {
		t.Errorf("无端口的服务不应出现在 override 里:\n%s", y)
	}
	// 全部服务都没端口 → 不需要 override
	if y := buildOverrideYAML(map[string][]int{"db": nil}); y != "" {
		t.Errorf("无端口时应返回空，得到 %q", y)
	}
}

func TestParseComposeConfig(t *testing.T) {
	out := `{"services":{"web":{"ports":[{"mode":"ingress","target":8080,"published":"8088"}]},"db":{"image":"postgres"}}}`
	svcs, err := parseComposeConfig(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(svcs["web"]) != 1 || svcs["web"][0] != 8080 {
		t.Errorf("web 端口应为 [8080]，得到 %v", svcs["web"])
	}
	if len(svcs["db"]) != 0 {
		t.Errorf("db 无端口，得到 %v", svcs["db"])
	}
}

// compose 选择的完整启动链路：env 文件 → config 解析 → 带 override 的 up。
func TestStartComposeSelection(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{
		"compose": {stdout: `{"services":{"web":{"ports":[{"target":8080,"published":"8088"}]}}}`},
	}}
	m, wt := newTestManager(t, fd, 100, 100)
	writeFile(t, wt, "deploy/docker-compose.yml",
		"services:\n  web:\n    image: app\n    environment:\n      - DB=${DATABASE_HOST:?必填}\n")

	// 必填变量缺失 → 拒绝
	err := m.Start(context.Background(), 7, wt, StartRequest{
		Selections: []Selection{{Path: "deploy/docker-compose.yml", Kind: "compose"}},
	})
	if err == nil || !strings.Contains(err.Error(), "DATABASE_HOST") {
		t.Fatalf("缺必填变量应拒绝并点名变量，得到 %v", err)
	}

	// 填齐后放行
	if err := m.Start(context.Background(), 7, wt, StartRequest{
		Selections: []Selection{{
			Path: "deploy/docker-compose.yml", Kind: "compose",
			Env: map[string]string{"DATABASE_HOST": "pg"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	waitOp(t, m, 7)

	// 验证 compose 调用链：config 与 up 都带项目名与 env-file
	var configCall, upCall []string
	for _, c := range fd.calls {
		if len(c) > 1 && c[1] == "compose" {
			joined := strings.Join(c, " ")
			if strings.Contains(joined, " config ") {
				configCall = c
			}
			if strings.Contains(joined, " up ") {
				upCall = c
			}
		}
	}
	if configCall == nil || upCall == nil {
		t.Fatalf("应有 compose config 与 up 调用: %v", fd.calls)
	}
	for _, want := range []string{"-p", "lathe-preview-t7", "--env-file"} {
		if !strings.Contains(strings.Join(upCall, " "), want) {
			t.Errorf("up 调用应含 %s: %v", want, upCall)
		}
	}
	// 有端口声明 → up 应带 override 文件（-f 出现两次）
	if strings.Count(strings.Join(upCall, "\x00"), "-f") < 2 {
		t.Errorf("有端口声明时应挂 override 文件: %v", upCall)
	}
}

// 附加基础设施：建网络、起 pg/redis、就绪探测、连接串注入应用容器。
func TestStartWithInfra(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{}}
	m, wt := newTestManager(t, fd, 100, 100)
	writeFile(t, wt, "Dockerfile", "FROM alpine\nEXPOSE 3000\n")

	err := m.Start(context.Background(), 7, wt, StartRequest{
		Selections: []Selection{{Path: "Dockerfile", Ports: []int{3000}}},
		Infra:      []string{"postgres", "redis"},
		Env:        map[string]string{"FEATURE_FLAG": "on"},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitOp(t, m, 7)

	joined := func() string {
		var b strings.Builder
		for _, c := range fd.calls {
			b.WriteString(strings.Join(c, " ") + "\n")
		}
		return b.String()
	}()
	// 网络创建（打标签，Stop 可发现）
	if !strings.Contains(joined, "network create") {
		t.Error("应创建任务网络")
	}
	// pg/redis 容器进网络、带别名
	if !strings.Contains(joined, "lathe-preview-t7-infra-postgres") ||
		!strings.Contains(joined, "lathe-preview-t7-infra-redis") {
		t.Error("应启动 postgres 与 redis 基础设施容器")
	}
	// 就绪探测
	if !strings.Contains(joined, "pg_isready") || !strings.Contains(joined, "redis-cli ping") {
		t.Error("应做就绪探测")
	}
	// 应用容器：进网络 + 注入约定连接串 + 人的额外 env
	if !strings.Contains(joined, "DATABASE_HOST=pg") ||
		!strings.Contains(joined, "REDIS_URL=redis://redis:6379") ||
		!strings.Contains(joined, "FEATURE_FLAG=on") {
		t.Error("应用容器应注入基础设施连接串与额外 env")
	}
}

// infra 仅对 Dockerfile 选择生效；未知 infra 拒绝。
func TestStartInfraValidation(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{}}
	m, wt := newTestManager(t, fd, 100, 100)
	writeFile(t, wt, "docker-compose.yml", "services: {}\n")

	err := m.Start(context.Background(), 7, wt, StartRequest{
		Selections: []Selection{{Path: "docker-compose.yml", Kind: "compose"}},
		Infra:      []string{"postgres"},
	})
	if err == nil || !strings.Contains(err.Error(), "Dockerfile") {
		t.Errorf("compose + infra 应拒绝，得到 %v", err)
	}

	writeFile(t, wt, "Dockerfile", "FROM alpine\n")
	err = m.Start(context.Background(), 7, wt, StartRequest{
		Selections: []Selection{{Path: "Dockerfile"}},
		Infra:      []string{"oracle"},
	})
	if err == nil || !strings.Contains(err.Error(), "未知的基础设施") {
		t.Errorf("未知 infra 应拒绝，得到 %v", err)
	}
}

// Stop 应清理网络（两路标签）。
func TestStopRemovesNetworks(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{
		"ps":      {stdout: "c1\n"},
		"network": {stdout: "n1\n"},
	}}
	m, _ := newTestManager(t, fd, 100, 100)
	if _, _, err := m.Stop(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	var sawNetRm bool
	for _, c := range fd.calls {
		if len(c) >= 2 && c[1] == "network" && c[2] == "rm" {
			sawNetRm = true
		}
	}
	if !sawNetRm {
		t.Errorf("Stop 应删除预览网络: %v", fd.calls)
	}
}

// compose 文件名识别：必须先以 .yml/.yaml 结尾再看前缀，
// 否则 .example 模板会凭前缀混入（线上实测扫出了
// docker-compose.override.yml.example）。
func TestIsComposeFile(t *testing.T) {
	yes := []string{"compose.yml", "compose.yaml", "docker-compose.yml",
		"docker-compose.dev.yml", "compose.prod.yaml", "Docker-Compose.YML"}
	no := []string{"docker-compose.override.yml.example", "compose.yml.bak",
		"my-compose.yml", "Dockerfile", "docker-compose.txt"}
	for _, n := range yes {
		if !isComposeFile(n) {
			t.Errorf("%s 应识别为 compose 文件", n)
		}
	}
	for _, n := range no {
		if isComposeFile(n) {
			t.Errorf("%s 不应识别为 compose 文件", n)
		}
	}
}
