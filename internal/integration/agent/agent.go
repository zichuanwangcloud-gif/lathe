// Package agent 驱动编码 agent CLI（claude / codex）执行编码任务。
//
// 设计要点（docs/03-tech-stack.md §3 理由①）：本包是进程监管器。
// 所有子进程放进独立进程组，context 取消或超时时杀整棵进程树，
// 确保 Lathe 退出后不留游荡的 agent 进程。进程监管只有一份（supervise.go），
// 各 CLI 的私有输出由各自的驱动归一成同一个 Event（docs/features/multi-cli.md）。
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// maxLineBytes 是单条 stream-json 事件的上限。
//
// init 事件会把全部工具、技能、插件清单塞进一行，实测已达数 KB；
// 大仓 + 多插件时可能更大，因此远高于 bufio 默认的 64KB。
const maxLineBytes = 16 << 20 // 16MB

// idleTimeout 是「agent 长时间没有任何输出」的判死阈值。
//
// 总超时（LATHE_AGENT_TIMEOUT，默认 45 分钟）挡的是「跑得久」，挡不住
// 「卡住了」：实测规划第 4 轮在 06:48:08 之后整整十分钟没吐一行 stdout
// （thinking_tokens 停在 1150），一直挂到调用方的 10 分钟上限，期间人看到
// 的只是「在跑」，拿到的报错是「超时」——而真实原因是进程不再产出任何东西。
//
// 取 12 分钟：实测一轮里最长的一段静默是单次长推理，几十秒到两三分钟；
// 12 分钟仍无任何一行输出（含每 50 token 一条的 thinking_tokens），就不是
// 「慢」而是「死了」。它只挡「彻底不动」，不挡「跑得久」—— 后者由总超时管。
const idleTimeout = 12 * time.Minute

// ErrIdle 表示 agent 静默超时被判死（区别于总超时）。
var ErrIdle = errors.New("agent: 长时间没有任何输出，判定卡死")

// pipeDrainGrace 是 CLI 退出后、读端等待管道 EOF 的上限。
// 进程退出时管道里剩的内容是毫秒级读完的；等满这段还没 EOF，只能是写端
// 被逃出进程组的后台进程攥着，再等下去也不会有新的有效输出。
const pipeDrainGrace = 5 * time.Second

// stderrHead / stderrTail 是 stderr 的保留量：只进报错信息，头尾各留一段
// （开头常是第一个报错，结尾是退出前的最后状态）。
const (
	stderrHead = 1 << 10
	stderrTail = 3 << 10
)

// EventType 是 stream-json 的事件类型。
type EventType string

const (
	// EventSystem 会话初始化等系统事件（subtype=init 时携带 session_id、工具清单）。
	EventSystem EventType = "system"
	// EventAssistant 模型输出的消息。
	EventAssistant EventType = "assistant"
	// EventUser 工具执行结果回灌给模型的消息。
	EventUser EventType = "user"
	// EventResult 终局事件，携带成败、耗时、成本。
	EventResult EventType = "result"
	// EventOther 不属于前四类的行（codex 的 turn.started、重连 error 等）。
	EventOther EventType = "other"
)

// 驱动名。空串一律按 DriverClaude 处理（向后兼容：零值 = 现在的行为）。
const (
	DriverClaude = "claude"
	DriverCodex  = "codex"
)

// Event 是一条已解析的 agent 事件（各 CLI 的私有输出归一后的形状）。
//
// 只结构化 Lathe 真正要用的字段，其余保留在 Raw 里，
// 避免 CLI 增删字段就把驱动打挂。
type Event struct {
	// Driver 是产出这条事件的驱动（DriverClaude | DriverCodex；零值 = claude）。
	// Digest 按它分派到对应 CLI 的提炼逻辑。
	Driver    string
	Type      EventType
	Subtype   string
	SessionID string
	// Raw 是 CLI 的原始一行（私有格式，只给 Digest 用）。
	Raw json.RawMessage

	// codexTurn 是 codex 驱动在 turn.completed/turn.failed 上附带的运行时统计
	// （本次 Run 到此为止的轮数与墙钟耗时）：单行 JSON 里没有这两个值，
	// Digest 又是逐行无状态的，只能由驱动带过来。其余事件为 nil。
	codexTurn *codexTurnMeta
}

// Result 是一次 agent 执行的终局摘要。
type Result struct {
	SessionID  string
	Success    bool
	IsError    bool
	Subtype    string // success | error_max_turns | error_during_execution ...
	Text       string // 最终输出文本
	NumTurns   int
	CostUSD    float64
	DurationMS int64
	// TerminalReason 是 CLI 给出的终止原因（completed / ...）。
	TerminalReason string
	// PermissionDenials 非空说明 agent 被权限拦住了 —— 这是一种
	// 独立的失败模式，必须与"改不动代码"区分开来上报。
	PermissionDenials []json.RawMessage
	APIErrorStatus    json.RawMessage
}

// resultEvent 映射 type=result 事件里 Lathe 关心的字段。
type resultEvent struct {
	Type              string            `json:"type"`
	Subtype           string            `json:"subtype"`
	IsError           bool              `json:"is_error"`
	SessionID         string            `json:"session_id"`
	Result            string            `json:"result"`
	NumTurns          int               `json:"num_turns"`
	TotalCostUSD      float64           `json:"total_cost_usd"`
	DurationMS        int64             `json:"duration_ms"`
	TerminalReason    string            `json:"terminal_reason"`
	PermissionDenials []json.RawMessage `json:"permission_denials"`
	APIErrorStatus    json.RawMessage   `json:"api_error_status"`
}

// envelope 是所有事件的公共头部。
type envelope struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
}

// ClaudeDriver 按配置启动 claude CLI。
type ClaudeDriver struct {
	bin     string
	timeout time.Duration
	// idle 是静默判死阈值（见 idleTimeout）。留成字段只为测试能调短；
	// NewDriver 一律给 idleTimeout。
	idle time.Duration
}

// Driver 是 ClaudeDriver 的旧名，保留为别名：既有装配与测试一行不改。
type Driver = ClaudeDriver

// NewDriver 构造 claude 驱动。bin 为空时取 "claude"。
func NewDriver(bin string, timeout time.Duration) *Driver {
	if bin == "" {
		bin = "claude"
	}
	if timeout <= 0 {
		timeout = 45 * time.Minute
	}
	return &Driver{bin: bin, timeout: timeout, idle: idleTimeout}
}

// idleTimeout 返回本次运行使用的静默判死阈值（零值退化为默认值）。
func (d *ClaudeDriver) idleTimeout() time.Duration {
	if d.idle > 0 {
		return d.idle
	}
	return idleTimeout
}

// RunParams 描述一次 agent 执行。
type RunParams struct {
	// Prompt 是交给 agent 的任务描述。
	Prompt string
	// Dir 是工作目录（任务的 worktree）。
	Dir string

	// SessionID 预先指定会话 ID（UUID）。
	//
	// 刻意在启动前就定下并落库，而不是从输出里解析：这样即使进程在
	// 中途崩溃，数据库里也已有一个可 --resume 的会话 ID。
	SessionID string
	// Resume 为真时以 --resume SessionID 续跑既有会话。
	Resume bool
	// FromPR 非空时用 --from-pr 续跑与该 PR 关联的会话（review 二轮）。
	// 与 Resume 互斥。
	FromPR string

	// PermissionMode 传给 --permission-mode。无人值守通常用 acceptEdits
	// 或 bypassPermissions；留空则用 CLI 默认。
	PermissionMode string
	// SettingSources 传给 --setting-sources（user,project,local 的子集）。
	//
	// 无人值守批量跑应收敛为 "project"：只加载目标仓库自己的配置与
	// CLAUDE.md，把执行者个人环境里的插件与技能定义排除在外。
	// 实测一句「回答两个字」在装了插件的环境要吃掉 32937 input tokens
	// （docs/02-design.md §9）—— 这笔基线会乘以任务数，且让任务执行
	// 结果依赖某台机器装了什么插件，不可复现。
	SettingSources string
	// ExtraEnv 是调用方显式注入的环境变量（KEY=VALUE），附加在
	// sanitizedEnv 白名单之后（同名后者生效）。用途：按阶段路由模型
	// 通道（LATHE_AGENT_CHANNEL，B2-2）这类「每次执行不同」的值。
	// 与白名单的关系：白名单管「进程环境里什么能漏给 agent」，
	// ExtraEnv 管「调用方明确要给 agent 什么」—— 后者是显式意图，
	// 不受白名单限制，但调用方必须自己保证不注入敏感值。
	ExtraEnv []string
	// ExtraArgs 追加原样传给 CLI 的参数。
	ExtraArgs []string

	// OnEvent 每解析出一条事件就回调一次（可为 nil）。
	// 回调在读取协程里同步执行，实现应尽量轻量。
	OnEvent func(Event)

	// Driver 选择驱动：空串 = claude。Router 按它分派。
	// runner 从任务行（tasks.agent_driver，建任务时从仓库钉死）取值。
	Driver string
	// OnSession 在驱动一拿到「真实会话 ID」时回调（可为 nil）。
	//   claude：ID 是预分配的（SessionID），不回调。
	//   codex：不接受预分配 ID，读到 thread.started 时回调 thread_id。
	// 与 OnEvent 同一份契约：在读取协程里同步执行，实现须并发安全、尽量轻量。
	OnSession func(id string)
}

// ErrNoResultEvent 表示 CLI 退出了但没给出终局 result 事件。
var ErrNoResultEvent = errors.New("agent: CLI 未输出 result 事件（可能被杀或异常退出）")

// Run 执行一次 agent 任务，流式解析事件并返回终局摘要。
//
// 无论成功失败，返回前都保证子进程树已被回收。
func (d *ClaudeDriver) Run(ctx context.Context, p RunParams) (*Result, error) {
	if p.Prompt == "" && !p.Resume && p.FromPR == "" {
		return nil, fmt.Errorf("agent: Prompt 为空且非续跑，无事可做")
	}
	if p.Resume && p.FromPR != "" {
		return nil, fmt.Errorf("agent: Resume 与 FromPR 互斥")
	}
	if p.Resume && p.SessionID == "" {
		return nil, fmt.Errorf("agent: Resume 需要 SessionID")
	}

	return supervise(ctx, d.claudeLaunch(p),
		superviseLimits{Timeout: d.timeout, Idle: d.idleTimeout()},
		parseClaude, p.OnEvent)
}

// claudeLaunch 构造 claude 的启动描述。stdin 不接（nil），与抽出 supervise
// 之前逐字节相同；env = 白名单 + 调用方显式注入（同名后者生效）。
func (d *ClaudeDriver) claudeLaunch(p RunParams) launch {
	return launch{
		Bin:  d.bin,
		Args: d.buildArgs(p),
		Dir:  p.Dir,
		Env:  append(sanitizedEnv(), p.ExtraEnv...),
	}
}

// parseClaude 是 claude 的 supervise parse：每一条可解析的 JSON 行先喂看门狗
// 再投递，不过滤 —— touch 集合与抽出 supervise 之前逐行相同（非 JSON 行、
// 坏 JSON 行不 touch）。f01 的过滤加在这里时，Touch 已经在过滤之前。
func parseClaude(r io.Reader, h parseHooks) (*Result, error) {
	return parseStream(r, func(e Event) {
		h.Touch()
		h.Emit(e)
	})
}

func (d *ClaudeDriver) buildArgs(p RunParams) []string {
	args := []string{"--print", "--output-format", "stream-json", "--verbose"}

	switch {
	case p.FromPR != "":
		args = append(args, "--from-pr", p.FromPR)
	case p.Resume:
		args = append(args, "--resume", p.SessionID)
	case p.SessionID != "":
		args = append(args, "--session-id", p.SessionID)
	}

	if p.PermissionMode != "" {
		args = append(args, "--permission-mode", p.PermissionMode)
	}
	if p.SettingSources != "" {
		args = append(args, "--setting-sources", p.SettingSources)
	}
	args = append(args, p.ExtraArgs...)

	if p.Prompt != "" {
		args = append(args, p.Prompt)
	}
	return args
}

// parseStream 逐行解析 NDJSON 事件，返回终局 result。
func parseStream(r io.Reader, onEvent func(Event)) (*Result, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	var result *Result
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue // 忽略非 JSON 噪声行
		}

		var env envelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue // 单行解析失败不应终止整次执行
		}

		if onEvent != nil {
			raw := make(json.RawMessage, len(line))
			copy(raw, line)
			onEvent(Event{
				Type:      EventType(env.Type),
				Subtype:   env.Subtype,
				SessionID: env.SessionID,
				Raw:       raw,
			})
		}

		if EventType(env.Type) != EventResult {
			continue
		}
		var re resultEvent
		if err := json.Unmarshal(line, &re); err != nil {
			return nil, fmt.Errorf("agent: 解析 result 事件失败: %w", err)
		}
		result = &Result{
			SessionID:         re.SessionID,
			Success:           re.Subtype == "success" && !re.IsError,
			IsError:           re.IsError,
			Subtype:           re.Subtype,
			Text:              re.Result,
			NumTurns:          re.NumTurns,
			CostUSD:           re.TotalCostUSD,
			DurationMS:        re.DurationMS,
			TerminalReason:    re.TerminalReason,
			PermissionDenials: re.PermissionDenials,
			APIErrorStatus:    re.APIErrorStatus,
		}
	}
	if err := sc.Err(); err != nil {
		return result, fmt.Errorf("agent: 读取事件流失败: %w", err)
	}
	return result, nil
}

// killProcessGroup 杀掉整个进程组。先 TERM 给收尾机会，再 KILL 兜底。
func killProcessGroup(pgid int) {
	if pgid <= 0 {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	time.Sleep(200 * time.Millisecond)
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// drainOrClose 等 done 关闭（读协程拿到 EOF）；到 deadline 仍未关闭就
// 强关读端 r 逼读协程退出，再等它收尾。返回是否走了强关。
//
// 读协程在 r 关闭后必然很快返回（Read 立即报错），所以第二次等待是有界的。
func drainOrClose(done <-chan struct{}, r *os.File, deadline time.Time) bool {
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case <-done:
		return false
	case <-t.C:
		_ = r.Close() // 读协程会因此报错退出，关闭本身的错误无可处理
		<-done
		return true
	}
}

// sanitizedEnv 返回 agent 子进程的环境变量。
//
// 白名单制而非黑名单制：serve 进程的环境里带着 LATHE_ADMIN_TOKEN、
// 数据库口令、GitHub token 等敏感值，黑名单漏一个就全漏给 agent
// 执行的任意命令。白名单只放行运行所需的操作性变量；确需追加时
// 用 LATHE_AGENT_ENV_EXTRA（逗号分隔的变量名）显式开口。
//
// 历史：从 Claude Code 会话内启动 claude 时，CLAUDECODE 与
// CLAUDE_CODE_ENTRYPOINT 会泄漏进子进程并导致其异常退出 ——
// 白名单天然把它们挡在门外。
func sanitizedEnv() []string {
	keep := map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true,
		"SHELL": true, "TERM": true, "TMPDIR": true, "TZ": true,
		"LANG": true, "LC_ALL": true, "LC_CTYPE": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
		"http_proxy": true, "https_proxy": true, "no_proxy": true,
		"SSH_AUTH_SOCK": true, // worktree 内的 git 操作需要
		"CI":            true, // 工具链据此关闭 watch/交互模式
	}
	for _, name := range strings.Split(os.Getenv("LATHE_AGENT_ENV_EXTRA"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			keep[name] = true
		}
	}
	src := os.Environ()
	out := make([]string, 0, len(keep)+2)
	for _, kv := range src {
		if i := strings.IndexByte(kv, '='); i > 0 && keep[kv[:i]] {
			out = append(out, kv)
		}
	}
	return out
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	// 回退到 rune 边界：按字节硬切会切断多字节 UTF-8 字符，
	// Postgres 拒绝非法 UTF-8（SQLSTATE 22021）导致整批事件落库失败。
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…(已截断)"
}
