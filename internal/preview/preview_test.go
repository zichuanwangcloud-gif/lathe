package preview

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Dockerfile", "FROM alpine\nEXPOSE 3000\n")
	write("apps/web/Dockerfile", "FROM node\nEXPOSE 3000 8080/tcp\nEXPOSE 3000\n")
	write("docker/Dockerfile.worker", "FROM alpine\n")     // 无 EXPOSE
	write("api.Dockerfile", "FROM alpine\nEXPOSE $PORT\n") // 变量端口无法静态求值
	write("node_modules/dep/Dockerfile", "FROM scratch\n") // 依赖目录必须跳过
	write(".git/Dockerfile", "FROM scratch\n")             // 绝不进 .git
	write("README.md", "EXPOSE 1\n")                       // 非 Dockerfile

	got, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover 报错: %v", err)
	}
	var paths []string
	ports := map[string][]int{}
	for _, c := range got {
		paths = append(paths, c.Path)
		ports[c.Path] = c.Ports
	}
	want := []string{"Dockerfile", "api.Dockerfile", "apps/web/Dockerfile", "docker/Dockerfile.worker"}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("发现结果 = %v，期望 %v", paths, want)
	}
	if !reflect.DeepEqual(ports["apps/web/Dockerfile"], []int{3000, 8080}) {
		t.Errorf("EXPOSE 解析（多端口/去重/剥协议）不符: %v", ports["apps/web/Dockerfile"])
	}
	if len(ports["docker/Dockerfile.worker"]) != 0 {
		t.Errorf("无 EXPOSE 应为空: %v", ports["docker/Dockerfile.worker"])
	}
	if len(ports["api.Dockerfile"]) != 0 {
		t.Errorf("变量端口应跳过: %v", ports["api.Dockerfile"])
	}
}

func TestParseExposes(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    []int
	}{
		{"单端口", "EXPOSE 3000", []int{3000}},
		{"多端口与协议后缀", "EXPOSE 3000/tcp 53/udp", []int{53, 3000}},
		{"变量跳过", "EXPOSE $PORT", nil},
		{"注释行不算", "# EXPOSE 1\nEXPOSE 2", []int{2}},
		{"行内注释截断", "EXPOSE 3000 # web 端口", []int{3000}},
		{"小写指令", "expose 80", []int{80}},
		{"越界端口", "EXPOSE 70000", nil},
		{"跨行去重排序", "EXPOSE 9000\nEXPOSE 80 9000", []int{80, 9000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseExposes(tc.content); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseExposes(%q) = %v，期望 %v", tc.content, got, tc.want)
			}
		})
	}
}

func TestParseMeminfo(t *testing.T) {
	// total 1000k，avail 250k → 已用 75%
	got, err := parseMeminfo(strings.NewReader("MemTotal:    1000 kB\nMemFree: 100 kB\nMemAvailable: 250 kB\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got != 75 {
		t.Errorf("已用百分比 = %d，期望 75", got)
	}
	if _, err := parseMeminfo(strings.NewReader("MemFree: 1 kB\n")); err == nil {
		t.Error("缺 MemTotal/MemAvailable 应报错")
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Dockerfile":             "root",
		"apps/web/Dockerfile":    "apps-web",
		"docker/Dockerfile.api":  "docker-api",
		"worker.Dockerfile":      "worker",
		"Apps/Web_UI/Dockerfile": "apps-web-ui",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q，期望 %q", in, got, want)
		}
	}
}
