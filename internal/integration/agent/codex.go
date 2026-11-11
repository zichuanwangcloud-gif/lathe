package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// codex.go —— codex CLI 驱动（`codex exec --json`，测过的版本 codex-cli 0.146.1）。
//
// 与 claude 驱动的差别全部收在「怎么起」（codexLaunch）与「怎么读」
// （codexParseState.parse）两处，进程监管走同一个 supervise。事实依据是
// PRD 阶段对真实 codex 二进制的取证（docs/features/multi-cli.md §实测）：
//
//   - 输出是 JSONL：thread.started → turn.started → item.* → turn.completed/turn.failed（P1）；
//   - 不接受预分配会话 ID，真实 ID 只能从 thread.started.thread_id 读（P3）→ OnSession；
//   - `codex exec resume <id> -` 保持原会话（P4/P5），resume 没有 -C（P7）；
//   - prompt 走 stdin（参数 `-`，P12），避开 argv 单参数上限；
//   - 隔离要同时覆盖 CODEX_HOME 与 HOME（P11/P15），且 CODEX_HOME 不得在
//     子进程的临时目录下（P17：codex 会静默拒绝执行 shell 工具）。

// CodexDriver 按配置启动 codex CLI。
type CodexDriver struct {
	bin string
	// home 是专用 CODEX_HOME（LATHE_CODEX_HOME）：绝对路径、已存在、不是
	// 个人 ~/.codex、不在临时目录下 —— 由 config 在启动时校验。
	home    string
	timeout time.Duration
	// idle 同 ClaudeDriver.idle：留成字段只为测试能调短。
	idle time.Duration
}

// NewCodexDriver 构造 codex 驱动。bin 为空时取 "codex"。
func NewCodexDriver(bin, home string, timeout time.Duration) *CodexDriver {
	if bin == "" {
		bin = "codex"
	}
	if timeout <= 0 {
		timeout = 45 * time.Minute
	}
	return &CodexDriver{bin: bin, home: filepath.Clean(home), timeout: timeout, idle: idleTimeout}
}

func (d *CodexDriver) idleTimeout() time.Duration {
	if d.idle > 0 {
		return d.idle
	}
	return idleTimeout
}

// codexConsumes / codexRejects 是 codex 驱动对 RunParams 每个字段的显式表态。
//
// TestCodexHandlesEveryRunParamsField 用 reflect 遍历 RunParams 的全部字段，
// 每一个都必须出现在这两张表之一：任何一方（如 f01 的插话）新增字段而没在
// 这里表态，测试当场红 —— 不靠「后合入的一方记得处理」。
//
// SettingSources 列为「消费」：codex 下它被有意忽略（隔离靠专用 CODEX_HOME，
// 不加「不读 $CODEX_HOME/config.toml」的那个 exec 参数，见 docs/features/multi-cli.md §7），忽略也是
// 一种明确的处理。
var (
	codexConsumes = []string{
		"Prompt", "Dir", "SessionID", "Resume", "PermissionMode", "SettingSources",
		"ExtraEnv", "ExtraArgs", "OnEvent", "Driver", "OnSession",
	}
	codexRejects = []string{"FromPR"}
)

// Run 执行一次 codex 任务。无论成功失败，返回前都保证子进程树已回收。
func (d *CodexDriver) Run(ctx context.Context, p RunParams) (*Result, error) {
	if err := checkCapabilities(DriverCodex, p); err != nil {
		return nil, err
	}
	l, err := d.codexLaunch(p)
	if err != nil {
		return nil, err
	}
	st := &codexParseState{onSession: p.OnSession, start: time.Now()}
	if p.Resume {
		st.resumeID = p.SessionID
	}
	res, err := supervise(ctx, l,
		superviseLimits{Timeout: d.timeout, Idle: d.idleTimeout()},
		st.parse, p.OnEvent)
	if res != nil {
		res.DurationMS = time.Since(st.start).Milliseconds()
	}
	return res, err
}

// codexLaunch 构造 codex 的启动描述：argv、显式 Dir、白名单 env + 强制隔离项、
// prompt 走 stdin。AC-22 的真实二进制隔离测试直接用它产出的 Env/Dir。
func (d *CodexDriver) codexLaunch(p RunParams) (launch, error) {
	if p.Prompt == "" && !p.Resume {
		return launch{}, fmt.Errorf("agent: Prompt 为空且非续跑，无事可做")
	}
	if p.Resume && p.SessionID == "" {
		return launch{}, fmt.Errorf("agent: Resume 需要 SessionID")
	}
	if p.Dir == "" {
		// claude 下空 Dir 退回继承 cwd；codex 下这正是 B2-3 的事故形态
		// （把 serve 的 cwd 当成目标仓库），直接拒绝。
		return launch{}, fmt.Errorf("agent: codex 需要显式的工作目录（RunParams.Dir）")
	}
	args, err := codexBuildArgs(p)
	if err != nil {
		return launch{}, err
	}
	env, err := d.codexEnv(p)
	if err != nil {
		return launch{}, err
	}
	return launch{
		Bin:   d.bin,
		Args:  args,
		Dir:   p.Dir,
		Env:   env,
		Stdin: strings.NewReader(p.Prompt),
	}, nil
}

// codexHomeDir 是子进程的 HOME：专用 CODEX_HOME 下的一个子目录。
//
// P15：专用 CODEX_HOME 挡不住 $HOME/.agents/skills，必须连 HOME 一起换。
// 这个目录对沙箱只读（不进 writable_roots）：所有任务共用它，可写就等于
// 任何一个任务能给之后的所有任务投毒技能。
func (d *CodexDriver) codexHomeDir() string { return filepath.Join(d.home, "home") }

// codexEnv 拼子进程环境：sanitizedEnv() 白名单 → ExtraEnv → CODEX_HOME、HOME
// 最后追加（调用方覆盖不了）。CODEX_HOME、HOME 在结果里各只出现一次，不靠
// 「同名后者生效」的未定义行为。
//
// 起进程前按最终 env 里的 TMPDIR 再查一次 CODEX_HOME 的位置（P17）：
// 启动校验只看得到 serve 自己的 os.TempDir()，ExtraEnv 可以改写 TMPDIR。
func (d *CodexDriver) codexEnv(p RunParams) ([]string, error) {
	base := append(sanitizedEnv(), p.ExtraEnv...)
	env := make([]string, 0, len(base)+2)
	for _, kv := range base {
		switch envKey(kv) {
		case "CODEX_HOME", "HOME":
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "CODEX_HOME="+d.home, "HOME="+d.codexHomeDir())

	tmp := "/tmp" // 与 codex / os.TempDir 同一取法：TMPDIR 未设或为空时是 /tmp
	for _, kv := range env {
		if envKey(kv) == "TMPDIR" {
			if v := kv[len("TMPDIR="):]; v != "" {
				tmp = v
			} else {
				tmp = "/tmp"
			}
		}
	}
	if err := CheckCodexHomeNotInTempDir(d.home, tmp); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDriverUnavailable, err)
	}

	hd := d.codexHomeDir()
	if err := os.MkdirAll(hd, 0o700); err != nil {
		return nil, fmt.Errorf("agent: 建 codex 子进程 HOME %s 失败: %w", hd, err)
	}
	if err := os.Chmod(hd, 0o700); err != nil {
		return nil, fmt.Errorf("agent: 设置 %s 权限失败: %w", hd, err)
	}
	return env, nil
}

func envKey(kv string) string {
	if i := strings.IndexByte(kv, '='); i > 0 {
		return kv[:i]
	}
	return kv
}

// codexBuildArgs 构造 codex 的参数。prompt 永远不进 argv（走 stdin，P12）。
//
// 永不加「不读用户配置」的那个 exec 参数（help 原文 Do not load $CODEX_HOME/config.toml，P11）：
// 对技能、hook 的发现毫无作用，却会让运维在专用 home 里配的 provider/模型失效。
func codexBuildArgs(p RunParams) ([]string, error) {
	if p.FromPR != "" {
		return nil, fmt.Errorf("%w: codex 不支持 FromPR", ErrUnsupportedParam)
	}
	perm, err := codexPermissionArgs(p.PermissionMode)
	if err != nil {
		return nil, err
	}
	// 分诊的中立目录不是 git 仓库，所以要 --skip-git-repo-check；
	// codex 默认只读 AGENTS.md，加 fallback 让目标仓库的 CLAUDE.md 也可见（P10）。
	config := append(perm, "-c", `project_doc_fallback_filenames=["CLAUDE.md"]`)

	var args []string
	if p.Resume {
		// resume 没有 -C（P7）：cwd 靠 cmd.Dir；权限走 -c，在 resume 上同样生效。
		args = []string{"exec", "resume", "--json", "--skip-git-repo-check"}
		args = append(args, config...)
		args = append(args, p.ExtraArgs...)
		return append(args, p.SessionID, "-"), nil
	}
	args = []string{"exec", "--json", "--skip-git-repo-check", "-C", p.Dir}
	args = append(args, config...)
	args = append(args, p.ExtraArgs...)
	return append(args, "-"), nil
}

// codexPermissionArgs 把 claude 语义的 PermissionMode 翻译成 codex 的沙箱配置。
// approval_policy 永远是 never：无人值守，没有人能点「批准」。
func codexPermissionArgs(mode string) ([]string, error) {
	args := []string{"-c", `approval_policy="never"`}
	switch mode {
	case "plan":
		return append(args, "-c", `sandbox_mode="read-only"`), nil
	case "", "acceptEdits", "bypassPermissions":
		return append(args, "-c", `sandbox_mode="workspace-write"`,
			"-c", "sandbox_workspace_write.network_access=true"), nil
	default:
		return nil, fmt.Errorf("agent: codex 不认识 PermissionMode %q（可选 plan / acceptEdits / bypassPermissions）", mode)
	}
}

// ---------------------------------------------------------------- 解析

// codexLine 是 codex JSONL 一行里 Lathe 关心的字段。
type codexLine struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"`
	Item     *codexItem      `json:"item"`
	Usage    json.RawMessage `json:"usage"`
	Error    *codexError     `json:"error"`
	// Message 是顶层 error 行的文本（{"type":"error","message":...}）。
	Message string `json:"message"`
}

type codexError struct {
	Message string `json:"message"`
}

// codexItem 收 item.* 各类型的字段（按 type 分别有值，与 contentBlock 同一手法）。
type codexItem struct {
	ID   string `json:"id"`
	Type string `json:"type"`

	Text string `json:"text"` // agent_message / reasoning

	// command_execution
	Command          string `json:"command"`
	AggregatedOutput string `json:"aggregated_output"`
	ExitCode         *int   `json:"exit_code"`
	Status           string `json:"status"`

	// file_change
	Changes []struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	} `json:"changes"`

	// mcp_tool_call
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Result    json.RawMessage `json:"result"`
	Error     *codexError     `json:"error"`

	Query string `json:"query"` // web_search

	// todo_list
	Items []struct {
		Text      string `json:"text"`
		Completed bool   `json:"completed"`
	} `json:"items"`

	Message string `json:"message"` // error item
}

// codexTurnMeta 是 codex 驱动随终局事件带给 Digest 的运行时统计（见 Event.codexTurn）。
type codexTurnMeta struct {
	NumTurns   int
	DurationMS int64
}

// codexParseState 是一次 Run 的解析状态。
type codexParseState struct {
	onSession func(string)
	start     time.Time
	// resumeID 是 resume 时调用方给的会话 ID：codex 的 resume 也会先吐一行
	// thread.started（P4 实测），这里只用于兜底 Result.SessionID。
	resumeID string

	threadID  string
	reported  string
	lastText  string
	failedMsg string
	completed int
	failed    int
	// usages 按 turn 收集 turn.completed.usage 的原文（不导出、不进 Result）。
	// f03（用量统计）合入后，在构造 Result 处加一行 Usage = UsageFromCodex(usages)，
	// 解析器不用改（f06 PRD §4.1）。
	usages []json.RawMessage
}

// codexEventType 把一行 codex 输出归入统一事件类型。
func codexEventType(l *codexLine) (EventType, string) {
	switch l.Type {
	case "thread.started":
		return EventSystem, "init"
	case "turn.completed":
		return EventResult, "success"
	case "turn.failed":
		return EventResult, "error_turn_failed"
	case "item.started", "item.updated", "item.completed":
		if l.Item == nil {
			return EventOther, l.Type
		}
		done := l.Type == "item.completed"
		switch l.Item.Type {
		case "agent_message", "reasoning", "web_search", "todo_list":
			return EventAssistant, l.Item.Type
		case "command_execution", "mcp_tool_call":
			if done {
				return EventUser, l.Item.Type
			}
			return EventAssistant, l.Item.Type
		case "file_change":
			return EventUser, l.Item.Type
		default:
			return EventOther, l.Item.Type
		}
	default:
		return EventOther, l.Type
	}
}

// parse 是 codex 的 supervise parse：逐行 JSONL，每一条可解析的行先喂看门狗
// 再投递，不过滤（与 claude 的 touch 规则相同）。
func (s *codexParseState) parse(r io.Reader, h parseHooks) (*Result, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue // 忽略非 JSON 噪声行（如「Reading prompt from stdin...」）
		}
		var cl codexLine
		if err := json.Unmarshal(line, &cl); err != nil {
			continue // 单行解析失败不应终止整次执行
		}
		h.Touch()

		typ, sub := codexEventType(&cl)
		raw := make(json.RawMessage, len(line))
		copy(raw, line)
		ev := Event{Driver: DriverCodex, Type: typ, Subtype: sub, Raw: raw}

		switch cl.Type {
		case "thread.started":
			ev.SessionID = cl.ThreadID
			if cl.ThreadID != "" {
				s.threadID = cl.ThreadID
				if s.onSession != nil && s.reported != cl.ThreadID {
					s.reported = cl.ThreadID
					s.onSession(cl.ThreadID)
				}
			}
		case "item.completed":
			if cl.Item != nil && cl.Item.Type == "agent_message" && strings.TrimSpace(cl.Item.Text) != "" {
				s.lastText = cl.Item.Text
			}
		case "turn.completed":
			s.completed++
			if len(cl.Usage) > 0 && string(cl.Usage) != "null" {
				u := make(json.RawMessage, len(cl.Usage))
				copy(u, cl.Usage)
				s.usages = append(s.usages, u)
			}
			ev.codexTurn = s.turnMeta()
		case "turn.failed":
			s.failed++
			if cl.Error != nil {
				s.failedMsg = cl.Error.Message
			}
			ev.codexTurn = s.turnMeta()
		}
		h.Emit(ev)
	}
	if err := sc.Err(); err != nil {
		return s.result(), fmt.Errorf("agent: 读取 codex 事件流失败: %w", err)
	}
	return s.result(), nil
}

func (s *codexParseState) turnMeta() *codexTurnMeta {
	return &codexTurnMeta{NumTurns: s.completed + s.failed, DurationMS: time.Since(s.start).Milliseconds()}
}

// result 按 PRD §3.3「Result 映射（codex）」构造终局；两种终局事件都没出现
// 时返回 nil（supervise 据此报 ErrNoResultEvent，与 claude 同一规则）。
func (s *codexParseState) result() *Result {
	if s.completed == 0 && s.failed == 0 {
		return nil
	}
	sid := s.threadID
	if sid == "" {
		sid = s.resumeID
	}
	res := &Result{
		SessionID: sid,
		Success:   s.completed > 0 && s.failed == 0,
		IsError:   s.failed > 0,
		Subtype:   "success",
		Text:      s.lastText,
		NumTurns:  s.completed + s.failed,
		// codex 不报费用：CostUSD 保持 0，result 条目 body 写「费用未知」。
		CostUSD: 0,
	}
	if res.IsError {
		res.Subtype = "error_turn_failed"
		res.Text = s.failedMsg
	}
	return res
}
