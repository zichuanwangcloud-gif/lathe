package preview

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 本文件是推荐的机械证据层：改动画像、在跑容器清单、口令类识别。
// 这些事实由 Lathe 确定性计算，不靠模型猜 —— 模型只拿到事实后做决策，
// 危险位（有 SQL 变更禁止复用共享库）由校验层机械执行。

// ---------------------------------------------------------------- 改动画像

// ChangeProfile 是 worktree 相对基线的改动画像。
type ChangeProfile struct {
	Base   string   `json:"base"`   // 基线引用（origin/main 等）
	Files  []string `json:"files"`  // 改动文件（截断）
	Apps   []string `json:"apps"`   // 改动涉及的顶层应用（apps/console-v2 等）
	HasSQL bool     `json:"hasSql"` // 是否含迁移/SQL 变更 —— 决定能否复用共享库
}

// sqlPathRe 命中迁移与 schema 文件路径。
var sqlPathRe = regexp.MustCompile(`(?i)(^|/)(migrations?|migrate|schemas?)/|\.sql$|/schema\.(rb|py|prisma)$|db/migrate/`)

// appRootDirs 是「前两段才算应用」的目录；其余取首段。
var appRootDirs = map[string]bool{"apps": true, "services": true, "packages": true, "cmd": true}

// appOf 从文件路径提取应用标识：apps/console-v2/x.go → apps/console-v2；
// internal/foo/x.go → internal；根目录文件 → ""。
func appOf(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) >= 2 && appRootDirs[parts[0]] {
		return parts[0] + "/" + parts[1]
	}
	if len(parts) >= 2 {
		return parts[0]
	}
	return ""
}

// profileFromFiles 由改动文件清单机械推导画像（纯函数，便于测试）。
func profileFromFiles(base string, files []string) *ChangeProfile {
	p := &ChangeProfile{Base: base}
	apps := map[string]bool{}
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		p.Files = append(p.Files, f)
		if a := appOf(f); a != "" {
			apps[a] = true
		}
		if sqlPathRe.MatchString(f) {
			p.HasSQL = true
		}
	}
	for a := range apps {
		p.Apps = append(p.Apps, a)
	}
	sort.Strings(p.Apps)
	// 文件清单截断：prompt 里放全量浪费 token，100 条足够判断
	if len(p.Files) > 100 {
		p.Files = append(p.Files[:100], fmt.Sprintf("……（共 %d 个文件）", len(files)))
	}
	return p
}

// changeProfile 计算 worktree 的改动画像。基线依次尝试
// origin/HEAD → origin/main → origin/master → HEAD~1；
// 都失败退化为 HEAD 单提交（画像缩水但不挡路）。
func (m *Manager) changeProfile(ctx context.Context, absWorktree string) *ChangeProfile {
	base := ""
	for _, cand := range []string{"origin/HEAD", "origin/main", "origin/master", "HEAD~1"} {
		if _, _, err := m.exec(ctx, "git", "-C", absWorktree, "rev-parse", "--verify", cand); err == nil {
			base = cand
			break
		}
	}
	var out string
	var err error
	if base != "" {
		out, _, err = m.exec(ctx, "git", "-C", absWorktree, "diff", "--name-only", base+"...HEAD")
	}
	if base == "" || err != nil {
		base = "HEAD"
		out, _, _ = m.exec(ctx, "git", "-C", absWorktree, "show", "--name-only", "--format=", "HEAD")
	}
	return profileFromFiles(base, strings.Split(out, "\n"))
}

// ---------------------------------------------------------------- 在跑容器清单

// RunningContainer 是一个在跑容器的证据条目。
type RunningContainer struct {
	Name    string `json:"name"`
	Image   string `json:"image"`
	Ports   string `json:"ports"`   // docker ps 原始格式（人读）
	Project string `json:"project"` // com.docker.compose.project 标签
	DBKind  string `json:"dbKind"`  // postgres | mysql | redis | mongo | ""
	// 以下仅 DB 类填充：从容器 env/端口映射机械提取，供 reuse 免填口令。
	Env      map[string]string `json:"env,omitempty"`
	HostPort int               `json:"hostPort,omitempty"` // 服务端口对应的宿主端口
}

// dbKindOf 从镜像名识别 DB 家族。
func dbKindOf(image string) string {
	l := strings.ToLower(image)
	switch {
	case strings.Contains(l, "postgres"):
		return "postgres"
	case strings.Contains(l, "mysql"), strings.Contains(l, "mariadb"):
		return "mysql"
	case strings.Contains(l, "redis"):
		return "redis"
	case strings.Contains(l, "mongo"):
		return "mongo"
	}
	return ""
}

// dbServicePort 是各 DB 家族的容器内服务端口。
func dbServicePort(kind string) int {
	switch kind {
	case "postgres":
		return 5432
	case "mysql":
		return 3306
	case "redis":
		return 6379
	case "mongo":
		return 27017
	}
	return 0
}

// runningContainers 列出本机在跑容器；DB 类额外提取凭据与宿主端口。
// 凭据会进入推荐 prompt 与建议值 —— 信任模型与「仓库即权限」同级：
// 只有任务属主能看到自己任务的推荐结果（接口有归属校验）。
func (m *Manager) runningContainers(ctx context.Context) []RunningContainer {
	out, _, err := m.exec(ctx, "docker", "ps", "--format",
		`{{.Names}}\t{{.Image}}\t{{.Ports}}\t{{.Label "com.docker.compose.project"}}`)
	if err != nil {
		return nil
	}
	var list []RunningContainer
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" {
			continue
		}
		c := RunningContainer{Name: parts[0], Image: parts[1]}
		if len(parts) > 2 {
			c.Ports = parts[2]
		}
		if len(parts) > 3 {
			c.Project = parts[3]
		}
		c.DBKind = dbKindOf(c.Image)
		list = append(list, c)
	}
	// DB 类补凭据（数量少，逐个 inspect 代价可忽略）
	for i := range list {
		if list[i].DBKind == "" {
			continue
		}
		m.inspectDBContainer(ctx, &list[i])
	}
	return list
}

func (m *Manager) inspectDBContainer(ctx context.Context, c *RunningContainer) {
	out, _, err := m.exec(ctx, "docker", "inspect", "--format",
		`{{json .Config.Env}}|{{json .NetworkSettings.Ports}}`, c.Name)
	if err != nil {
		return
	}
	parts := strings.SplitN(strings.TrimSpace(out), "|", 2)
	if len(parts) != 2 {
		return
	}
	var envList []string
	if err := json.Unmarshal([]byte(parts[0]), &envList); err == nil {
		c.Env = map[string]string{}
		for _, kv := range envList {
			if i := strings.Index(kv, "="); i > 0 {
				c.Env[kv[:i]] = kv[i+1:]
			}
		}
	}
	var ports map[string][]struct {
		HostPort string `json:"HostPort"`
	}
	if err := json.Unmarshal([]byte(parts[1]), &ports); err == nil {
		want := strconv.Itoa(dbServicePort(c.DBKind)) + "/tcp"
		for key, bindings := range ports {
			if key == want && len(bindings) > 0 {
				if p, err := strconv.Atoi(bindings[0].HostPort); err == nil {
					c.HostPort = p
				}
			}
		}
	}
}

// ---------------------------------------------------------------- 口令

// passwordClassRe 命中口令类变量名 —— 这类必填变量永远由 Lathe
// 自动生成，不让人填（「还让我手动填数据库密码，这种都不应该存在」）。
var passwordClassRe = regexp.MustCompile(`(?i)(PASSWORD|PASSWD|SECRET|TOKEN|API_?KEY|PRIVATE_?KEY|CREDENTIALS?)`)

// generatePassword 生成随机口令（32 位十六进制）。
func generatePassword() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("preview: 生成口令失败: %v", err))
	}
	return hex.EncodeToString(b[:])
}
