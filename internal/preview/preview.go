// Package preview 实现任务预览环境：在任务的 worktree 里发现
// Dockerfile、构建镜像、启动容器并映射端口，让人能在合并前手动
// 点一版真实跑起来的服务 —— 静态测试（无论 AI 跑多少）替代不了
// 肉眼确认前端实际展示效果。
//
// 生命周期完全由人驱动：看板上一键启动、手动验证、一键停止并清理
// 镜像与容器。启动前有资源闸门：服务器内存/磁盘占用率超过阈值
// （系统设置可配）时拒绝启动，避免预览把任务执行挤爆。
package preview

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Candidate 是 worktree 里发现的一个可启动单元：一个 Dockerfile
// （单镜像）或一个 compose 编排文件（多服务 + 依赖拓扑）。
type Candidate struct {
	// Path 是相对 worktree 根的路径（如 apps/web/Dockerfile 或
	// deploy/docker-compose.yml）。
	Path string `json:"path"`
	// Kind 是 "dockerfile" 或 "compose"。
	Kind string `json:"kind"`
	// Context 是构建上下文目录（相对 worktree 根；仅 dockerfile 用）。
	Context string `json:"context"`
	// Name 是展示名，取 Path。
	Name string `json:"name"`
	// Ports 是从 EXPOSE 指令解析出的容器端口（去重、升序，仅
	// dockerfile）。compose 的端口由编排文件自己声明，启动时统一
	// 重置为随机宿主端口，不在此预填。
	Ports []int `json:"ports"`
	// Env 是 compose 文件里引用的环境变量（仅 compose）。无默认值
	// 的（${VAR} 或 ${VAR:?提示}）为必填 —— 启动前必须由人填齐，
	// 比如数据库连接串；有没有默认值、默认值是什么都从文件里来。
	Env []EnvVarSpec `json:"env,omitempty"`
}

// EnvVarSpec 是 compose 文件里一个环境变量引用的静态扫描结果。
type EnvVarSpec struct {
	Name     string `json:"name"`
	Default  string `json:"default,omitempty"`
	Required bool   `json:"required"` // 无默认值 = 必填
}

// discoverSkipDirs 是扫描时要跳过的目录。.git 是底线；其余是
// 依赖与产物目录 —— 把 Dockerfile 藏在那里的唯一可能是示例，
// 扫出来只会干扰选择。
var discoverSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, ".next": true, ".pnpm-store": true,
}

// Discover 扫描 worktree 里的可启动单元：Dockerfile（含 Dockerfile.*
// 与 *.Dockerfile 变体）与 compose 编排文件（compose.yml /
// docker-compose.yml 及 docker-compose.*.yml 等变体），按路径升序返回。
func Discover(worktreePath string) ([]Candidate, error) {
	var out []Candidate
	err := filepath.WalkDir(worktreePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != worktreePath && discoverSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		isDF, isCompose := isDockerfile(d.Name()), isComposeFile(d.Name())
		if !isDF && !isCompose {
			return nil
		}
		rel, err := filepath.Rel(worktreePath, path)
		if err != nil {
			return err
		}
		data, err := readHead(path, 1<<20)
		if err != nil {
			return fmt.Errorf("preview: 读取 %s 失败: %w", rel, err)
		}
		c := Candidate{
			Path:    filepath.ToSlash(rel),
			Context: filepath.ToSlash(filepath.Dir(rel)),
			Name:    filepath.ToSlash(rel),
		}
		if isCompose {
			c.Kind = "compose"
			c.Env = ScanComposeEnv(string(data))
		} else {
			c.Kind = "dockerfile"
			c.Ports = ParseExposes(string(data))
		}
		out = append(out, c)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("preview: 扫描 %s 失败: %w", worktreePath, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func isDockerfile(name string) bool {
	return name == "Dockerfile" ||
		strings.HasPrefix(name, "Dockerfile.") ||
		strings.HasSuffix(name, ".Dockerfile")
}

// isComposeFile 识别 compose 编排文件：compose.yml / compose.yaml /
// docker-compose.yml / docker-compose.*.yml 等。compose 是「服务拓扑 +
// 依赖 + 配置」的标准声明，有编排文件的项目优先走编排。
func isComposeFile(name string) bool {
	lower := strings.ToLower(name)
	// 必须先确认是 yaml 文件再看前缀 —— 否则 TrimSuffix 对非 yaml
	// 名原样返回，docker-compose.override.yml.example 这类模板文件
	// 会凭前缀混进来。
	var base string
	for _, ext := range []string{".yml", ".yaml"} {
		if strings.HasSuffix(lower, ext) {
			base = strings.TrimSuffix(lower, ext)
			break
		}
	}
	if base == "" {
		return false
	}
	return base == "compose" || base == "docker-compose" ||
		strings.HasPrefix(base, "docker-compose.") || strings.HasPrefix(base, "compose.")
}

// envRefRe 匹配 compose 文件里的变量插值：${VAR}、${VAR:-默认}、
// ${VAR:?必填提示} 及对应的 :- / :? 简写。$$ 转义（字面 $）不匹配。
var envRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::?([-?])([^}]*))?\}`)

// ScanComposeEnv 静态扫描 compose 文件里的环境变量引用。
// ${VAR} 与 ${VAR:?...} 无默认值 → 必填；${VAR:-x} → 可选并预填 x。
// 按变量名去重，必填优先（同一变量同时以两种形态出现时宁可要求填）。
func ScanComposeEnv(content string) []EnvVarSpec {
	// $$ 是 compose 的字面美元符转义：先中和掉，$${VAR} 就不会被
	// 误识为变量引用（RE2 没有 lookbehind，替换比正则技巧可靠）。
	content = strings.ReplaceAll(content, "$$", "\x00")
	byName := map[string]*EnvVarSpec{}
	var order []string
	for _, m := range envRefRe.FindAllStringSubmatch(content, -1) {
		name, op, arg := m[1], m[2], m[3]
		spec, ok := byName[name]
		if !ok {
			spec = &EnvVarSpec{Name: name}
			byName[name] = spec
			order = append(order, name)
		}
		if op == "-" { // :- 或 - ：有默认值
			if !spec.Required || spec.Default == "" {
				spec.Default = arg
			}
		} else { // :? 、 ? 或无操作符：无默认值 → 必填
			spec.Required = true
		}
	}
	out := make([]EnvVarSpec, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out
}

// readHead 读取文件前 limit 字节（Dockerfile 都是小文件，限额只是防御）。
func readHead(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

// ParseExposes 解析 Dockerfile 里的 EXPOSE 指令，返回数值端口
// （去重、升序）。变量形式（EXPOSE $PORT）无法静态求值，跳过 ——
// 界面上会提示人工指定。/udp 后缀剥离后按端口处理（映射仍按
// tcp 建，够用于预览）。
func ParseExposes(content string) []int {
	seen := map[int]bool{}
	var ports []int
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.EqualFold(fields[0], "EXPOSE") {
			continue
		}
		for _, f := range fields[1:] {
			if strings.HasPrefix(f, "#") {
				break // 行内注释
			}
			f = strings.SplitN(f, "/", 2)[0] // 剥 /tcp /udp
			if n, err := strconv.Atoi(f); err == nil && n > 0 && n <= 65535 && !seen[n] {
				seen[n] = true
				ports = append(ports, n)
			}
		}
	}
	sort.Ints(ports)
	return ports
}

// ResourceStatus 是一次资源水位测量的结果。
type ResourceStatus struct {
	MemUsedPct    int  `json:"memUsedPct"`
	DiskUsedPct   int  `json:"diskUsedPct"`
	MemThreshold  int  `json:"memThreshold"`
	DiskThreshold int  `json:"diskThreshold"`
	DockerOK      bool `json:"dockerOK"`
	// Allowed 为 false 时 Reason 给出人读的原因（哪个水位超了阈值，
	// 或 docker 不可用）。
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

// parseMeminfo 从 /proc/meminfo 内容算出已用内存百分比。
// 口径用 MemAvailable 而非 MemFree：缓存可回收，不算真占用。
func parseMeminfo(r io.Reader) (usedPct int, err error) {
	var total, avail int64 = -1, -1
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, e := strconv.ParseInt(fields[1], 10, 64)
		if e != nil {
			continue
		}
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			total = v
		case "MemAvailable":
			avail = v
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	if total <= 0 || avail < 0 {
		return 0, fmt.Errorf("preview: /proc/meminfo 缺少 MemTotal/MemAvailable")
	}
	return int((total - avail) * 100 / total), nil
}

// diskUsedPct 算出 path 所在文件系统的已用百分比。
// 口径用 Bavail（非 root 可用块）：预览构建跑在普通用户下，
// 保留块对它不可用。
func diskUsedPct(path string) (int, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("preview: statfs %s 失败: %w", path, err)
	}
	if st.Blocks == 0 {
		return 0, fmt.Errorf("preview: statfs %s 返回 0 块", path)
	}
	return int((st.Blocks - st.Bavail) * 100 / st.Blocks), nil
}
