package preview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- parseComposePS

func TestParseComposePSArray(t *testing.T) {
	out := `[{"Name":"cloudrouter-postgres","Service":"postgres","State":"running","Image":"rd.clouditera.com/docker/postgres:16-alpine"},` +
		`{"Name":"cloudrouter-redis","Service":"redis","State":"exited","Image":"rd.clouditera.com/docker/redis:8.2.3"}]`
	entries, err := parseComposePS(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("应解析出 2 条，得到 %d", len(entries))
	}
	if entries[0].Name != "cloudrouter-postgres" || entries[0].State != "running" {
		t.Errorf("第一条解析不符: %+v", entries[0])
	}
	if entries[1].State != "exited" {
		t.Errorf("第二条应为 exited: %+v", entries[1])
	}
}

func TestParseComposePSNDJSON(t *testing.T) {
	out := `{"Name":"a","Service":"a","State":"running","Image":"img-a"}` + "\n" +
		`{"Name":"b","Service":"b","State":"running","Image":"img-b"}` + "\n"
	entries, err := parseComposePS(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[1].Name != "b" {
		t.Errorf("NDJSON 解析不符: %+v", entries)
	}
}

func TestParseComposePSEmpty(t *testing.T) {
	entries, err := parseComposePS("  \n")
	if err != nil || entries != nil {
		t.Errorf("空输出应返回 (nil, nil)，得到 (%v, %v)", entries, err)
	}
}

func TestParseComposePSBadJSON(t *testing.T) {
	if _, err := parseComposePS("not json"); err == nil {
		t.Error("非法 JSON 应报错，而不是静默吞掉")
	}
}

// ---------------------------------------------------------------- DetectBaseline

// writeComposeFile 在 dir 下写一个最小 compose 文件（Discover 只关心
// 文件名与静态扫描的 env 引用，不关心 services 内容——那部分由
// `docker compose ps` 的 fake 输出决定）。
func writeComposeFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name),
		[]byte("services:\n  postgres:\n    image: postgres:16-alpine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetectBaselineDirNotExist(t *testing.T) {
	fd := &fakeDocker{outputs: map[string]fakeResult{}}
	m, _ := newTestManager(t, fd, 100, 100)
	if _, err := m.DetectBaseline(context.Background(), "/no/such/dir", ""); err == nil {
		t.Error("目录不存在应报错")
	}
}

func TestDetectBaselineRunningService(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "infra"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeComposeFile(t, dir, "infra/docker-compose.yml")

	fd := &fakeDocker{outputs: map[string]fakeResult{
		"rev-parse": {stdout: "dev\n"},
		"ps": {stdout: `[{"Name":"cloudrouter-postgres","Service":"postgres","State":"running",` +
			`"Image":"rd.clouditera.com/docker/postgres:16-alpine"}]`},
		"inspect": {stdout: `rd.clouditera.com/docker/postgres:16-alpine` +
			`|["POSTGRES_USER=cloudrouter","POSTGRES_PASSWORD=devpassword","POSTGRES_DB=cloudrouter"]` +
			`|{"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"5434"}]}`},
	}}
	m, _ := newTestManager(t, fd, 100, 100)

	status, err := m.DetectBaseline(context.Background(), dir, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if status.Branch != "dev" || !status.HeadMatchesDefault {
		t.Errorf("分支识别不符: %+v", status)
	}
	if len(status.ComposeFiles) != 1 {
		t.Fatalf("应发现 1 个 compose 文件: %v", status.ComposeFiles)
	}
	if len(status.Services) != 1 {
		t.Fatalf("应发现 1 个服务: %+v", status.Services)
	}
	svc := status.Services[0]
	if !svc.Running || svc.DBKind != "postgres" || svc.HostPort != 5434 {
		t.Errorf("服务检测不符: %+v", svc)
	}
	if svc.Env["POSTGRES_PASSWORD"] != "devpassword" {
		t.Errorf("凭据应从 inspect 提取: %+v", svc.Env)
	}
}

func TestDetectBaselineStoppedServiceHasNoCredentials(t *testing.T) {
	dir := t.TempDir()
	writeComposeFile(t, dir, "docker-compose.yml")

	fd := &fakeDocker{outputs: map[string]fakeResult{
		"ps": {stdout: `[{"Name":"cloudrouter-postgres","Service":"postgres","State":"exited",` +
			`"Image":"postgres:16-alpine"}]`},
	}}
	m, _ := newTestManager(t, fd, 100, 100)

	status, err := m.DetectBaseline(context.Background(), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Services) != 1 || status.Services[0].Running {
		t.Fatalf("停止的服务不应标记为 running: %+v", status.Services)
	}
	if status.Services[0].DBKind != "postgres" {
		t.Errorf("即使没在跑，也应能从 compose ps 的 Image 字段识别家族: %+v", status.Services[0])
	}
	if status.Services[0].Env != nil {
		t.Errorf("没在跑的服务不应有凭据: %+v", status.Services[0])
	}
}

// ---------------------------------------------------------------- DeployBaseline

func TestDeployBaselineRunsComposeUp(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "infra"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeComposeFile(t, dir, "infra/docker-compose.yml")

	fd := &fakeDocker{outputs: map[string]fakeResult{}}
	m, _ := newTestManager(t, fd, 100, 100)

	if err := m.DeployBaseline(context.Background(), dir, "infra/docker-compose.yml"); err != nil {
		t.Fatal(err)
	}
	if !fd.has("compose", "-f", filepath.Join(dir, "infra/docker-compose.yml"), "up", "-d") {
		t.Errorf("应执行 docker compose -f <file> up -d，实际调用: %v", fd.calls)
	}
}

func TestDeployBaselineRejectsMissingFile(t *testing.T) {
	dir := t.TempDir()
	fd := &fakeDocker{outputs: map[string]fakeResult{}}
	m, _ := newTestManager(t, fd, 100, 100)

	err := m.DeployBaseline(context.Background(), dir, "no-such-compose.yml")
	if err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Errorf("不存在的 compose 文件应报错，得到 %v", err)
	}
}

func TestDeployBaselineRejectsEmptyComposeFile(t *testing.T) {
	dir := t.TempDir()
	fd := &fakeDocker{outputs: map[string]fakeResult{}}
	m, _ := newTestManager(t, fd, 100, 100)

	if err := m.DeployBaseline(context.Background(), dir, ""); err == nil {
		t.Error("未指定 compose 文件应报错")
	}
}
