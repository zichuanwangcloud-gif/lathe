// Package agent 驱动 claude CLI 执行编码任务。
//
// 设计要点（docs/03-tech-stack.md §3 理由①）：本包是进程监管器。
// 所有子进程放进独立进程组，context 取消或超时时杀整棵进程树，
// 确保 Lathe 退出后不留游荡的 claude 进程。
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
	"os/exec"
	"strings"
	"syscall"
	"unicode/utf8"
	"time"
)

// maxLineBytes 是单条 stream-json 事件的上限。
//
// init 事件会把全部工具、技能、插件清单塞进一行，实测已达数 KB；
// 大仓 + 多插件时可能更大，因此远高于 bufio 默认的 64KB。
const maxLineBytes = 16 << 20 // 16MB

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
)

// Event 是一条已解析的 stream-json 事件。
//
// 只结构化 Lathe 真正要用的字段，其余保留在 Raw 里，
// 避免 CLI 增删字段就把驱动打挂。
type Event struct {
	Type      EventType
	Subtype   string
	SessionID string
	Raw       json.RawMessage
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

// Driver 按配置启动 claude CLI。
type Driver struct {
	bin     string
	timeout time.Duration
}

// NewDriver 构造驱动。bin 为空时取 "claude"。
func NewDriver(bin string, timeout time.Duration) *Driver {
	if bin == "" {
		bin = "claude"
	}
	if timeout <= 0 {
		timeout = 45 * time.Minute
	}
	return &Driver{bin: bin, timeout: timeout}
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
	// ExtraArgs 追加原样传给 CLI 的参数。
	ExtraArgs []string

	// OnEvent 每解析出一条事件就回调一次（可为 nil）。
	// 回调在读取协程里同步执行，实现应尽量轻量。
	OnEvent func(Event)
}

// ErrNoResultEvent 表示 CLI 退出了但没给出终局 result 事件。
var ErrNoResultEvent = errors.New("agent: CLI 未输出 result 事件（可能被杀或异常退出）")

// Run 执行一次 agent 任务，流式解析事件并返回终局摘要。
//
// 无论成功失败，返回前都保证子进程树已被回收。
func (d *Driver) Run(ctx context.Context, p RunParams) (*Result, error) {
	if p.Prompt == "" && !p.Resume && p.FromPR == "" {
		return nil, fmt.Errorf("agent: Prompt 为空且非续跑，无事可做")
	}
	if p.Resume && p.FromPR != "" {
		return nil, fmt.Errorf("agent: Resume 与 FromPR 互斥")
	}
	if p.Resume && p.SessionID == "" {
		return nil, fmt.Errorf("agent: Resume 需要 SessionID")
	}

	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	args := d.buildArgs(p)

	// 刻意不用 exec.CommandContext：它只杀直接子进程，而 claude 会派生
	// 自己的子进程。这里用 Setpgid 建独立进程组，超时后杀整组。
	cmd := exec.Command(d.bin, args...)
	cmd.Dir = p.Dir
	cmd.Env = sanitizedEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("agent: 接管 stdout 失败: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("agent: 启动 %s 失败: %w", d.bin, err)
	}
	pgid := cmd.Process.Pid

	// 监听取消：一旦 ctx 结束就杀整个进程组
	killed := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killProcessGroup(pgid)
		case <-killed:
		}
	}()
	defer close(killed)

	result, parseErr := parseStream(stdout, p.OnEvent)

	waitErr := cmd.Wait()
	// 兜底：即使正常退出也再扫一遍进程组，杀掉可能残留的孙子进程
	killProcessGroup(pgid)

	if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
		return result, fmt.Errorf("agent: 执行超时（上限 %v），进程树已回收", d.timeout)
	} else if ctxErr != nil {
		return result, fmt.Errorf("agent: 执行被取消: %w", ctxErr)
	}

	if parseErr != nil {
		return result, parseErr
	}
	if result == nil {
		return nil, fmt.Errorf("%w（退出错误: %v，stderr: %s）",
			ErrNoResultEvent, waitErr, truncate(stderr.String(), 2000))
	}

	// CLI 以非零码退出但给了 result 事件时，以 result 为准：
	// is_error/subtype 描述得比退出码精确。
	if waitErr != nil && !result.IsError {
		return result, fmt.Errorf("agent: CLI 非零退出（%v），stderr: %s",
			waitErr, truncate(stderr.String(), 2000))
	}
	return result, nil
}

func (d *Driver) buildArgs(p RunParams) []string {
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
		"CI":          true,   // 工具链据此关闭 watch/交互模式
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
