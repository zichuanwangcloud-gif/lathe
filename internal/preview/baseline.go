// 基线目录：仓库配置里登记一次"基线分支在本机的目录"（如已经用
// `pnpm up` 常驻跑着开发环境的 /opt/CloudRouter），任务预览/worktree
// 起服务默认直接连它已经在跑的中间件，不必每次重新建一套 per-task
// 容器、也不必每次人工去 `docker ps` 里挑一个"reuse"的目标。
//
// 检测不猜容器命名规则——直接问 docker compose 自己该项目现在的容器
// 名/运行状态/发布端口（`docker compose -f <file> ps`），比重新实现
// compose 的项目/服务命名规则更稳。
//
// 明确的范围边界：这里只解决"起服务时连哪个中间件"，heavy 档自动化
// 验证（internal/runner/heavy.go）不接入——该建隔离库就建隔离库，
// 两件事刻意不共享。
package preview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// BaselineService 是基线目录里一个 compose 服务的检测结果。
type BaselineService struct {
	ComposeFile   string `json:"composeFile"` // 相对基线目录
	Service       string `json:"service"`
	ContainerName string `json:"containerName"`
	Running       bool   `json:"running"`
	Image         string `json:"image,omitempty"`
	// DBKind: postgres | mysql | redis | mongo | objectstore | ""
	DBKind   string            `json:"dbKind,omitempty"`
	HostPort int               `json:"hostPort,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
}

// BaselineStatus 是一次基线目录检测的完整结果。
type BaselineStatus struct {
	Dir string `json:"dir"`
	// Branch 是目录当前所在分支；拿不到（非 git 仓库/detached）时留空。
	Branch string `json:"branch,omitempty"`
	// HeadMatchesDefault 仅在调用方传入了 defaultBranch 时才有意义。
	HeadMatchesDefault bool `json:"headMatchesDefault"`
	// ComposeFiles 是目录里发现的全部 compose 编排文件（供人选择部署哪个）。
	ComposeFiles []string          `json:"composeFiles"`
	Services     []BaselineService `json:"services"`
}

// DetectBaseline 检测基线目录：扫描其中的 compose 文件，问 docker
// compose 每个文件当前的服务容器状态，Running 的服务顺带 inspect 出
// 镜像/凭据/端口，供 resolveDatabase 的 baseline 策略直接使用。
//
// 目录不存在或不是目录直接报错；git 分支识别失败、单个 compose 文件
// 的 ps/inspect 失败都不算整体错误——留痕告警，跳过该文件继续，宁可
// 结果不全也不让一个坏文件挡住其余中间件的识别。
func (m *Manager) DetectBaseline(ctx context.Context, dir, defaultBranch string) (*BaselineStatus, error) {
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("preview: 基线目录不存在: %s", dir)
	}

	status := &BaselineStatus{Dir: dir}
	if branch, _, err := m.exec(ctx, "git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		status.Branch = strings.TrimSpace(branch)
		if strings.TrimSpace(defaultBranch) != "" {
			status.HeadMatchesDefault = status.Branch == strings.TrimSpace(defaultBranch)
		}
	}

	cands, err := Discover(dir)
	if err != nil {
		return nil, fmt.Errorf("preview: 扫描基线目录失败: %w", err)
	}
	for _, c := range cands {
		if c.Kind == "compose" {
			status.ComposeFiles = append(status.ComposeFiles, c.Path)
		}
	}

	for _, rel := range status.ComposeFiles {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		out, stderr, err := m.exec(ctx, m.DockerBin, "compose", "-f", abs, "ps", "-a", "--format", "json")
		if err != nil {
			slog.Warn("基线检测：compose ps 失败", "dir", dir, "file", rel, "err", tail(stderr, 300))
			continue
		}
		entries, err := parseComposePS(out)
		if err != nil {
			slog.Warn("基线检测：解析 compose ps 输出失败", "dir", dir, "file", rel, "err", err)
			continue
		}
		for _, e := range entries {
			svc := BaselineService{
				ComposeFile:   rel,
				Service:       e.Service,
				ContainerName: e.Name,
				Running:       strings.EqualFold(e.State, "running"),
				Image:         e.Image,
				DBKind:        dbKindOf(e.Image),
			}
			if svc.Running {
				if image, env, ports, ierr := m.inspectContainer(ctx, e.Name); ierr == nil {
					if image != "" {
						svc.Image = image
						svc.DBKind = dbKindOf(image)
					}
					svc.Env = env
					if svc.DBKind != "" {
						svc.HostPort = ports[strconv.Itoa(dbServicePort(svc.DBKind))+"/tcp"]
					}
				} else {
					slog.Warn("基线检测：inspect 容器失败", "container", e.Name, "err", ierr)
				}
			}
			status.Services = append(status.Services, svc)
		}
	}
	return status, nil
}

// composePSEntry 是 `docker compose ps --format json` 单条记录里我们
// 关心的子集。不同 docker compose 版本字段有出入（如 Publishers 的
// 结构），这里只取最稳定的几个字段，端口/凭据另外走 inspect 拿。
type composePSEntry struct {
	Name    string `json:"Name"`
	Service string `json:"Service"`
	State   string `json:"State"`
	Image   string `json:"Image"`
}

// parseComposePS 解析 `compose ps --format json` 的输出。不同版本的
// docker compose 分别输出「一个 JSON 数组」或「每行一个 JSON 对象」
// （NDJSON），两种都容忍；解析失败返回 error 由调用方决定是否跳过。
func parseComposePS(out string) ([]composePSEntry, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	if strings.HasPrefix(out, "[") {
		var arr []composePSEntry
		if err := json.Unmarshal([]byte(out), &arr); err != nil {
			return nil, fmt.Errorf("preview: 解析 compose ps 输出失败: %w", err)
		}
		return arr, nil
	}
	var entries []composePSEntry
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e composePSEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, fmt.Errorf("preview: 解析 compose ps 输出失败: %w", err)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// inspectContainer 取一个在跑容器的镜像、环境变量与宿主端口映射
// （key 形如 "5432/tcp"）。与 evidence.go 的 inspectDBContainer 同源，
// 但不要求调用方预先知道 DBKind——基线检测需要先 inspect 才能判断
// 家族（compose ps 不一定带 Image 字段）。
func (m *Manager) inspectContainer(ctx context.Context, name string) (image string, env map[string]string, ports map[string]int, err error) {
	out, _, err := m.exec(ctx, m.DockerBin, "inspect", "--format",
		`{{.Config.Image}}|{{json .Config.Env}}|{{json .NetworkSettings.Ports}}`, name)
	if err != nil {
		return "", nil, nil, err
	}
	parts := strings.SplitN(strings.TrimSpace(out), "|", 3)
	if len(parts) != 3 {
		return "", nil, nil, fmt.Errorf("preview: 解析 %s 的 inspect 输出失败", name)
	}
	image = parts[0]

	env = map[string]string{}
	var envList []string
	if jerr := json.Unmarshal([]byte(parts[1]), &envList); jerr == nil {
		for _, kv := range envList {
			if i := strings.Index(kv, "="); i > 0 {
				env[kv[:i]] = kv[i+1:]
			}
		}
	}

	ports = map[string]int{}
	var rawPorts map[string][]struct {
		HostPort string `json:"HostPort"`
	}
	if jerr := json.Unmarshal([]byte(parts[2]), &rawPorts); jerr == nil {
		for key, bindings := range rawPorts {
			if len(bindings) == 0 {
				continue
			}
			if p, aerr := strconv.Atoi(bindings[0].HostPort); aerr == nil {
				ports[key] = p
			}
		}
	}
	return image, env, ports, nil
}

// DeployBaseline 把基线目录里人指定的一个 compose 文件跑起来
// （`docker compose -f <file> up -d`）。人工触发，不在任何任务流水线
// 里自动调用——连不连、起不起共享中间件，是有数据风险的决定，
// 一贯交给人拍板（docs/02-design.md §10）。
func (m *Manager) DeployBaseline(ctx context.Context, dir, composeFile string) error {
	rel := strings.TrimSpace(composeFile)
	if rel == "" {
		return errors.New("preview: 未指定要部署的 compose 文件")
	}
	rel = strings.TrimPrefix(filepath.Clean("/"+rel), string(filepath.Separator))
	abs := filepath.Join(dir, rel)
	if _, err := os.Stat(abs); err != nil {
		return fmt.Errorf("preview: %s 不存在: %w", composeFile, err)
	}

	dctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	output, err := m.execStream(dctx, m.DockerBin, []string{"compose", "-f", abs, "up", "-d"}, func(string) {})
	if err != nil {
		return fmt.Errorf("preview: 部署基线 %s 失败: %s", composeFile, tail(output, 2000))
	}
	return nil
}
