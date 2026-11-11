package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// router.go —— 按 RunParams.Driver 把一次执行分派给对应的 CLI 驱动。
//
// runner 的 AgentDriver 接口不变（Run(ctx, RunParams)）；装配时给它一个
// Router，runner 唯一的新动作是在 RunParams 里填 Driver 与 OnSession。

// ErrDriverUnavailable 表示目标驱动在本节点不可用（未配置/未注册），
// 或本节点的环境让它注定跑不起来（如 CODEX_HOME 落在临时目录下，P17）。
// 不起进程。runner 据此跳过「resume 失败降级」，不改写会话 ID。
var ErrDriverUnavailable = errors.New("agent: 驱动在本节点不可用")

// ErrUnsupportedParam 表示 RunParams 要求了目标驱动不支持的能力。不起进程。
var ErrUnsupportedParam = errors.New("agent: 驱动不支持该参数")

// Runner 是一个 CLI 驱动的执行入口（ClaudeDriver / CodexDriver / Router 都实现它）。
type Runner interface {
	Run(ctx context.Context, p RunParams) (*Result, error)
}

// Capabilities 声明一个驱动支持哪些 RunParams 能力。零值 = 全不支持。
//
// 新增一种能力（例如 f01 的 StreamInput）= 在这里加一个 bool 字段 +
// 在 requires() 里加一行；没有显式声明支持的驱动自动拒绝，无需改那个驱动的代码。
type Capabilities struct {
	// FromPR：claude 支持（--from-pr）；codex 不支持。
	FromPR bool
	// PreassignedSession：claude 支持（--session-id）；codex 不支持（P3），
	// 真实 ID 走 OnSession。只用于文档/断言，不拒绝。
	PreassignedSession bool
}

// requires 返回这次执行要求的能力。
func (p RunParams) requires() Capabilities {
	return Capabilities{FromPR: p.FromPR != ""}
}

// CapabilitiesOf 返回驱动的能力表。未知驱动 = 零值（全不支持）。
// runner 打开某项能力之前先问它。
func CapabilitiesOf(driver string) Capabilities {
	switch normalizeDriver(driver) {
	case DriverClaude:
		return Capabilities{FromPR: true, PreassignedSession: true}
	default:
		return Capabilities{}
	}
}

// checkCapabilities 比对 p.requires() 与驱动能力表，缺哪项就报哪项。
func checkCapabilities(driver string, p RunParams) error {
	need, have := p.requires(), CapabilitiesOf(driver)
	if need.FromPR && !have.FromPR {
		return fmt.Errorf("%w: %s 不支持 FromPR", ErrUnsupportedParam, normalizeDriver(driver))
	}
	return nil
}

// normalizeDriver 把空串规整为 claude（零值 = 现在的行为）。
func normalizeDriver(d string) string {
	if strings.TrimSpace(d) == "" {
		return DriverClaude
	}
	return d
}

// Router 按 RunParams.Driver 分派。构造后只读，并发安全。
type Router struct {
	drivers map[string]Runner
}

// NewRouter 构造路由。claude 必填；codex 为 nil 表示本节点未启用 codex
// （未配置 LATHE_CODEX_HOME），钉死为 codex 的任务会得到 ErrDriverUnavailable。
func NewRouter(claude Runner, codex Runner) *Router {
	r := &Router{drivers: map[string]Runner{DriverClaude: claude}}
	if codex != nil {
		r.drivers[DriverCodex] = codex
	}
	return r
}

// Run 分派一次执行。驱动不可用、能力不符时不起进程。
func (r *Router) Run(ctx context.Context, p RunParams) (*Result, error) {
	name := normalizeDriver(p.Driver)
	d, ok := r.drivers[name]
	if !ok || d == nil {
		if name == DriverCodex {
			return nil, fmt.Errorf("%w: codex（本节点未配置 LATHE_CODEX_HOME）", ErrDriverUnavailable)
		}
		return nil, fmt.Errorf("%w: 未知驱动 %q", ErrDriverUnavailable, name)
	}
	if err := checkCapabilities(name, p); err != nil {
		return nil, err
	}
	return d.Run(ctx, p)
}

// Enabled 报告驱动在本路由里是否可用（给 API/界面决定选项可不可选）。
func (r *Router) Enabled(driver string) bool {
	_, ok := r.drivers[normalizeDriver(driver)]
	return ok
}

// SkillsDir 返回驱动读取项目技能的目录（相对 worktree）。
// claude 读 .claude/skills，codex 读 .codex/skills（P8）。
func SkillsDir(driver string) string {
	if normalizeDriver(driver) == DriverCodex {
		return filepath.Join(".codex", "skills")
	}
	return filepath.Join(".claude", "skills")
}

// SubagentDirFor 返回一次会话的 subagent 记录目录；没有这份记录的驱动返回空串。
//
// 只有 claude 把 subagent 的内部活动落在磁盘上（transcript.go）。codex 没有
// 对应的记录契约，明确降级为「只看主会话」。
func SubagentDirFor(driver, cwd, sessionID string) string {
	if normalizeDriver(driver) != DriverClaude {
		return ""
	}
	return SubagentDir(ProjectsRoot(), cwd, sessionID)
}

// CheckCodexHomeNotInTempDir 拒绝位于临时目录 tmpDir 之下（含等于）的 CODEX_HOME。
//
// P17 实测：CODEX_HOME 在 codex 进程的临时目录（TMPDIR，未设时 /tmp）之下时，
// codex 拒绝创建 helper 二进制，shell 工具一条都不执行，turn 却照样以
// turn.completed、退出码 0 收尾 —— 运行期分辨不出来，只能事前拒绝。
// config 的启动校验与 CodexDriver 起进程前的复查共用这一个实现。
//
// 两边都先 Clean 再 EvalSymlinks（解析失败用 Clean 后的原值），按路径分量
// 判断：/tmpx/ch 不算在 /tmp 下。
func CheckCodexHomeNotInTempDir(home, tmpDir string) error {
	h, t := resolvePath(home), resolvePath(tmpDir)
	rel, err := filepath.Rel(t, h)
	if err != nil {
		return nil // 无法相对化（如一边是相对路径）：不在其下
	}
	if rel != "." && (rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return nil
	}
	return fmt.Errorf("LATHE_CODEX_HOME=%s 位于系统临时目录 %s 之下："+
		"codex 会拒绝在临时目录下创建 helper 二进制（\"Refusing to create helper binaries under temporary dir\"），"+
		"shell 工具一条都不会执行而且不报错，任务只会以「没有改动」或验证失败结束。"+
		"请改用临时目录之外的目录（若设置了 TMPDIR，也不能在 TMPDIR 之下）", home, tmpDir)
}

func resolvePath(p string) string {
	c := filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(c); err == nil {
		return r
	}
	return c
}
