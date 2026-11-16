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
	"sort"
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

	// Usage 是这次运行的 token 用量（f03）。多条 result 时随末条覆盖，
	// 与 CostUSD 同组，见 Usage 类型注释。
	Usage Usage
	// CostKnown 表示 CostUSD 是 CLI 报的真实账单。false = 未知（如 codex
	// 不报费用），账本落 NULL 而不是 $0。零值即「未知」，未改的驱动默认安全。
	CostKnown bool
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

	// Usage 是主循环的量（单 turn），ModelUsage 是进程累计、含 subagent
	// 的逐模型量。取 ModelUsage 为主，缺它才回落 Usage（f03）。
	Usage      *claudeUsage                `json:"usage"`
	ModelUsage map[string]claudeModelUsage `json:"modelUsage"`
}

// claudeUsage 是 result.usage（snake_case）。
type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// claudeModelUsage 是 result.modelUsage 的一项（camelCase）。
type claudeModelUsage struct {
	InputTokens              int64   `json:"inputTokens"`
	OutputTokens             int64   `json:"outputTokens"`
	CacheReadInputTokens     int64   `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64   `json:"cacheCreationInputTokens"`
	CostUSD                  float64 `json:"costUSD"`
}

// usageOf 从**一条** result 事件取用量：有 modelUsage 用各模型之和（明细
// 按 costUSD 降序、并列按模型名升序，结果确定），没有就回落该条的 usage，
// 两个都没有则 Present=false。不看其他 result —— 跨 result 的取舍由调用方
// 的覆盖赋值决定（末条整组胜出）。
func usageOf(re *resultEvent) Usage {
	var u Usage
	if len(re.ModelUsage) > 0 {
		u.Present = true
		for name, m := range re.ModelUsage {
			u.InputTokens += m.InputTokens
			u.OutputTokens += m.OutputTokens
			u.CacheReadTokens += m.CacheReadInputTokens
			u.CacheCreationTokens += m.CacheCreationInputTokens
			u.Models = append(u.Models, ModelUsage{
				Model: name, InputTokens: m.InputTokens, OutputTokens: m.OutputTokens,
				CacheReadTokens: m.CacheReadInputTokens, CacheCreationTokens: m.CacheCreationInputTokens,
				CostUSD: m.CostUSD,
			})
		}
		sort.Slice(u.Models, func(i, j int) bool {
			if u.Models[i].CostUSD != u.Models[j].CostUSD {
				return u.Models[i].CostUSD > u.Models[j].CostUSD
			}
			return u.Models[i].Model < u.Models[j].Model
		})
		return u
	}
	if re.Usage != nil {
		u.Present = true
		u.InputTokens = re.Usage.InputTokens
		u.OutputTokens = re.Usage.OutputTokens
		u.CacheReadTokens = re.Usage.CacheReadInputTokens
		u.CacheCreationTokens = re.Usage.CacheCreationInputTokens
	}
	return u
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

	// StreamInput 为真时以 --input-format stream-json --replay-user-messages 启动，
	// prompt 作为 stdin 的第一帧写入（运行中插话，docs/features/interject.md）。
	// stdin 的生命周期见 session.go 的包注释：首条 result / ctx 结束或看门狗 /
	// Wait 返回，三者任一先到即关。默认 false（走位置参数，与引入本字段之前
	// 逐行一致）—— 分诊、PRD 规划、预览推荐都不打开它。
	StreamInput bool
	// OnReady 在进程起来、stdin 可写后回调一次，交出可投递句柄。
	// 仅 StreamInput 时生效；为 nil 时插话机制整体消失。
	OnReady func(*Session)
	// OnDelivery 是注入帧的投递回显（--replay-user-messages 的 isReplay 事件）。
	// 在**读协程**里同步调用，契约：只许做内存操作（非阻塞发信号），
	// 禁止任何 I/O —— 尤其是 DB 写。读协程被拖慢会让看门狗与事件流一起变慢。
	OnDelivery func(deliveryID string)
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
	if p.StreamInput && p.Prompt == "" {
		// 流式输入下 prompt 是 stdin 首帧；没有首帧 CLI 只会干等。
		return nil, fmt.Errorf("agent: StreamInput 需要非空 Prompt（作为 stdin 首帧）")
	}

	// 走共用的 supervise（与 codex 同一份进程监管）：总超时 / 静默判死 /
	// 杀整棵进程树 / 管道宽限期都在那里，claude 只决定「怎么起」与「怎么读」。
	// StreamInput 时另起 stdin 管道，会话句柄同时喂给 parse（首条 result 关
	// stdin）与 Started（交出句柄）。
	if p.StreamInput {
		l, sess, err := d.claudeStreamLaunch(p)
		if err != nil {
			return nil, err
		}
		// 兜底关闭点：supervise 的 Exited / BeforeKill / OnResult 之外，
		// 起进程失败这条路也要收掉写端（否则每次失败漏一个 fd）。
		defer sess.closeStdin()
		return supervise(ctx, l,
			superviseLimits{Timeout: d.timeout, Idle: d.idleTimeout()},
			claudeParse(p, sess), p.OnEvent)
	}
	return supervise(ctx, d.claudeLaunch(p),
		superviseLimits{Timeout: d.timeout, Idle: d.idleTimeout()},
		claudeParse(p, nil), p.OnEvent)
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

// claudeStreamLaunch 构造 StreamInput 模式的启动描述（运行中插话）：
//
//   - prompt 不作位置参数，改走 stdin 的第一帧（buildArgs 已按 StreamInput 分支）；
//   - stdin 必须是 *os.File（os.Pipe 读端）。原因见 launch.Stdin 的注释：
//     传普通 io.Reader 时 os/exec 会起一条拷贝协程，cmd.Wait 要等它读到 EOF，
//     而写端要等首条 result 才关 —— 死锁。
//   - 三个生命周期钩子都挂在 Session 上，与 supervise 的三个时机一一对应：
//     Started（交出句柄）/ BeforeKill（ctx 或看门狗，杀进程组之前）/
//     Exited（cmd.Wait 返回后的兜底）。首条 result 的关闭点在 parse 里。
//
// 返回的 Session 必须由调用方兜底 closeStdin（起进程失败时 supervise 不会
// 走到 Exited，写端会漏）。
func (d *ClaudeDriver) claudeStreamLaunch(p RunParams) (launch, *Session, error) {
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return launch{}, nil, fmt.Errorf("agent: 建 stdin 管道失败: %w", err)
	}
	first, err := userFrame(newFrameID(), p.Prompt)
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		return launch{}, nil, err
	}
	sess := newSession(stdinW)
	sess.writeFirst(first)

	l := launch{
		Bin:   d.bin,
		Args:  d.buildArgs(p),
		Dir:   p.Dir,
		Env:   append(sanitizedEnv(), p.ExtraEnv...),
		Stdin: stdinR,
		Started: func() {
			if p.OnReady != nil {
				p.OnReady(sess)
			}
		},
		// 杀进程组之前先关写端：让 CLI 读到 EOF 自己收尾，
		// 而不是被 SIGKILL 打断在写一半的输出上。
		BeforeKill: sess.closeStdin,
		Exited:     sess.closeStdin,
	}
	return l, sess, nil
}

// claudeParse 是 claude 的 supervise parse。sess 非 nil（StreamInput）时
// 首条 result 到达即关 stdin，并把注入帧的回显交给 OnDelivery。
//
// 过滤规则在 parseStreamWith 里（Touch 由它自己调，且在过滤之前 ——
// 被丢弃的 command_lifecycle / replay 回显照样证明进程活着）。
func claudeParse(p RunParams, sess *Session) func(io.Reader, parseHooks) (*Result, error) {
	return func(r io.Reader, h parseHooks) (*Result, error) {
		onResult := func() {}
		if sess != nil {
			onResult = sess.closeStdin
		}
		return parseStreamWith(r, streamHooks{
			Touch:      h.Touch,
			OnEvent:    h.Emit,
			OnDelivery: p.OnDelivery,
			OnResult:   onResult,
			Whitelist:  p.StreamInput,
		})
	}
}

// parseClaude 是 claude 的 supervise parse（非流式）。保留这个名字是为了既有
// 测试与 supervise 的默认调用点；行为与引入 StreamInput 之前逐行相同。
func parseClaude(r io.Reader, h parseHooks) (*Result, error) {
	return claudeParse(RunParams{}, nil)(r, h)
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

	if p.StreamInput {
		// prompt 不作位置参数：它是 stdin 的第一帧（Run 写入）。
		return append(args, "--input-format", "stream-json", "--replay-user-messages")
	}
	if p.Prompt != "" {
		args = append(args, p.Prompt)
	}
	return args
}

// streamHooks 是 parseStreamWith 的回调集合（均可为 nil）。
type streamHooks struct {
	// Touch 每一条可解析的 JSON 行都调，**过滤之前** —— 喂看门狗。
	Touch func()
	// OnEvent 只收放行的事件。
	OnEvent func(Event)
	// OnDelivery 收 isReplay 回显的 uuid（读协程上同步调用，只许做内存操作）。
	OnDelivery func(id string)
	// OnResult 在第一条 result 到达时调一次（StreamInput 用来关 stdin）。
	OnResult func()
	// Whitelist 为真时（StreamInput 模式）只放行 system/assistant/user/result。
	Whitelist bool
}

// replayProbe 只取回显判定需要的两个字段。
type replayProbe struct {
	IsReplay bool   `json:"isReplay"`
	UUID     string `json:"uuid"`
}

// parseStream 逐行解析 NDJSON 事件，返回终局 result。
func parseStream(r io.Reader, onEvent func(Event)) (*Result, error) {
	return parseStreamWith(r, streamHooks{OnEvent: onEvent})
}

// parseStreamWith 是 parseStream 的完整实现。过滤规则（按顺序）：
//
//  0. 每一条 JSON 行先 Touch() —— 过滤在喂看门狗之后，被丢弃的事件照样证明进程活着
//  1. type=user 且 isReplay=true → OnDelivery(uuid)，不交给 OnEvent【所有模式】
//     这是 --replay-user-messages 对我们写进 stdin 的帧的回显：它的 content
//     是 text 块，Digest 会把它记成 KindText —— 人插的话就会以 **agent 自己说的
//     正文** 出现在事件流里，审计上张冠李戴。
//  2. type=command_lifecycle → 丢弃【所有模式】
//     --input-format stream-json 的命令排队/开始/完成通知，是 CLI 的连接性噪声，
//     不是 agent 产出；不丢的话 Digest 的 default 分支会把它退化成 raw。
//  3. Whitelist 为真时：type ∉ {system, assistant, user, result} → 丢弃【仅 StreamInput】
//  4. 其余照旧交给 OnEvent（含非 replay 的 user = 真·工具结果回灌）
//
// 1、2 两种形状只会由流式输入产生，非流式模式下它们根本不出现，过滤是 no-op。
// 3 只在 StreamInput 开：非流式下未知类型仍按 docs/04「提炼不出结构时保底 raw」。
// system/<非 init>（thinking_tokens、api_retry…）一律放行，Digest 照旧记 raw。
//
// 一次运行出现多条 result（result 前一刻写进管道的插话被 CLI 当成新 turn）时：
// NumTurns、DurationMS 是单 turn 量 → 各条求和；其余字段（含进程累计的
// total_cost_usd）取最后一条 —— 会话的终局状态就是最后一个 turn 之后的样子。
func parseStreamWith(r io.Reader, h streamHooks) (*Result, error) {
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
		if h.Touch != nil {
			h.Touch()
		}

		switch t := EventType(env.Type); {
		case t == EventUser:
			var rp replayProbe
			if err := json.Unmarshal(line, &rp); err == nil && rp.IsReplay {
				if h.OnDelivery != nil {
					h.OnDelivery(rp.UUID)
				}
				continue
			}
		case t == "command_lifecycle":
			continue
		case h.Whitelist && t != EventSystem && t != EventAssistant && t != EventResult:
			continue
		}

		if h.OnEvent != nil {
			raw := make(json.RawMessage, len(line))
			copy(raw, line)
			h.OnEvent(Event{
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
		first := result == nil
		next := &Result{
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
			// f03：Usage/CostKnown 与 CostUSD 同一组覆盖赋值 —— 末条胜出，
			// 不跨 result 相加（modelUsage 与 total_cost_usd 都是进程累计）。
			Usage:     usageOf(&re),
			CostKnown: true,
		}
		if !first {
			next.NumTurns += result.NumTurns
			next.DurationMS += result.DurationMS
		}
		result = next
		if first && h.OnResult != nil {
			h.OnResult()
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
