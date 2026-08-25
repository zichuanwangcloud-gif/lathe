// compose 编排支持：仓库里的 docker-compose.yml 是「服务拓扑 + 依赖 +
// 配置」的标准声明。预览直接复用它，而不是发明自己的编排格式。
//
// 三个适配点：
//  1. 端口冲突：编排文件常钉死宿主端口（8088:8080），多任务并存必撞 ——
//     生成 override 文件用 !override 标签把每个服务的 ports 整体替换为
//     随机宿主端口（compose 2.24+，实测 v2.40.3 可用）。
//  2. 必填变量：${VAR:?} 缺值会让 compose 直接失败 —— 启动前静态扫描
//     出来让人填（ScanComposeEnv），写成 --env-file 注入。连不连共享
//     测试库这类有数据风险的决定必须人来拍，不自动探测。
//  3. 项目隔离：compose -p lathe-preview-t<id>，容器/网络/构建镜像
//     自动带 com.docker.compose.project 标签，跨重启可发现、可清理。
package preview

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ComposeProject 是任务预览用的 compose 项目名。
func ComposeProject(taskID int64) string {
	return fmt.Sprintf("lathe-preview-t%d", taskID)
}

// composeConfigJSON 是 docker compose config --format json 输出里
// 我们关心的子集。
type composeConfigJSON struct {
	Services map[string]struct {
		Ports []struct {
			Target    int    `json:"target"`
			Published string `json:"published"`
		} `json:"ports"`
	} `json:"services"`
}

// parseComposeConfig 取出每个服务声明的容器端口（target）。
func parseComposeConfig(out string) (map[string][]int, error) {
	var cfg composeConfigJSON
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		return nil, fmt.Errorf("preview: 解析 compose config 失败: %w", err)
	}
	svcs := map[string][]int{}
	for name, svc := range cfg.Services {
		for _, p := range svc.Ports {
			if p.Target > 0 {
				svcs[name] = append(svcs[name], p.Target)
			}
		}
	}
	return svcs, nil
}

// buildOverrideYAML 生成端口重置 override：每个声明了端口的服务，
// ports 整体替换为 0:<target>（随机宿主端口）。没有任何服务声明
// 端口时返回空串（调用方跳过 override 文件）。
func buildOverrideYAML(servicePorts map[string][]int) string {
	names := make([]string, 0, len(servicePorts))
	for n := range servicePorts {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("# 由 lathe 预览生成：把编排里钉死的宿主端口重置为随机分配，\n")
	b.WriteString("# 否则两个任务同时预览同一仓库必撞端口。\n")
	b.WriteString("services:\n")
	wrote := false
	for _, n := range names {
		ports := servicePorts[n]
		if len(ports) == 0 {
			continue
		}
		wrote = true
		fmt.Fprintf(&b, "  %s:\n    ports: !override\n", yamlQuote(n))
		for _, p := range ports {
			fmt.Fprintf(&b, "      - \"0:%d\"\n", p)
		}
	}
	if !wrote {
		return ""
	}
	return b.String()
}

// yamlQuote 给服务名加引号（名字里通常没有特殊字符，但引号永远安全）。
func yamlQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// envNameRe 约束环境变量名形态（KEY=VALUE 注入与 env 文件共用的底线校验）。
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// writeEnvFile 把人填的变量写成 compose --env-file 格式（0600，可能含
// 数据库口令）。返回文件路径。
func writeEnvFile(dir string, env map[string]string) (string, error) {
	names := make([]string, 0, len(env))
	for k := range env {
		if !envNameRe.MatchString(k) {
			return "", fmt.Errorf("preview: 非法环境变量名 %q", k)
		}
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		// compose env 文件按行解析，值原样使用（不解释引号转义），
		// 含换行的值无法表达 —— 拒绝而不是静默写坏。
		if strings.ContainsAny(env[k], "\n\r") {
			return "", fmt.Errorf("preview: 环境变量 %s 的值不能含换行", k)
		}
		fmt.Fprintf(&b, "%s=%s\n", k, env[k])
	}
	path := filepath.Join(dir, "preview.env")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return "", fmt.Errorf("preview: 写 env 文件失败: %w", err)
	}
	return path, nil
}
