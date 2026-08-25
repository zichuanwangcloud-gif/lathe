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
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Candidate 是 worktree 里发现的一个可构建镜像。
type Candidate struct {
	// Path 是 Dockerfile 相对 worktree 根的路径（如 apps/web/Dockerfile）。
	Path string `json:"path"`
	// Context 是构建上下文目录（相对 worktree 根；根目录为 "."）。
	Context string `json:"context"`
	// Name 是展示名，取 Path。
	Name string `json:"name"`
	// Ports 是从 EXPOSE 指令解析出的容器端口（去重、升序）。
	// 为空表示 Dockerfile 未声明 —— 启动时需人手工指定，否则
	// 容器起来了也够不着。
	Ports []int `json:"ports"`
}

// discoverSkipDirs 是扫描时要跳过的目录。.git 是底线；其余是
// 依赖与产物目录 —— 把 Dockerfile 藏在那里的唯一可能是示例，
// 扫出来只会干扰选择。
var discoverSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, ".next": true, ".pnpm-store": true,
}

// Discover 扫描 worktree 里的 Dockerfile（含 Dockerfile.* 与
// *.Dockerfile 变体），按路径升序返回。
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
		if !isDockerfile(d.Name()) {
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
		ctxDir := filepath.Dir(rel)
		out = append(out, Candidate{
			Path:    filepath.ToSlash(rel),
			Context: filepath.ToSlash(ctxDir),
			Name:    filepath.ToSlash(rel),
			Ports:   ParseExposes(string(data)),
		})
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
