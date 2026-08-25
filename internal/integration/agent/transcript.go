// transcript.go —— 从 claude 留在磁盘上的 transcript 里读 subagent 的内部活动。
//
// 为什么需要第二条数据源：agent 派生 subagent（Agent 工具）时，subagent 的
// 内部步骤**不出现在** stdout 的 stream-json 里。父会话只看到一次 tool_use，
// 和一条汇总结果。实测本机 16 次执行里 7 次派生过 subagent，1093 行内部活动
// （21%）因此在界面上完全不可见 —— 详情页只有「Agent 找测试代码」一行，
// 中间它翻了什么、错在哪，全是黑盒。
//
// 这些活动 claude 落在：
//
//	<projects>/<cwd-slug>/<session-id>/subagents/agent-<agentId>.jsonl
//
// 立场：**与 stdout 通路并存，不取代它**。transcript 的 JSONL 是 Claude Code
// 的内部格式，无文档、可能随版本变化（zoetrope 的 README 也这么写）。主可见性
// 必须继续依赖 stdout —— 那是 CLI 的公开契约，且只有它给出 result 的成本与轮数。
// 本文件的一切失败都应降级为「少显示一些东西」，绝不能反过来影响任务执行。
package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ProjectSlug 把工作目录映射成 claude 的项目目录名。
//
// 规则：非字母数字的字符一律换成 '-'。例：
//
//	/opt/lathe/workspaces/cr-1363  →  -opt-lathe-workspaces-cr-1363
//	/opt/CloudRouter/.claude/wt/x  →  -opt-CloudRouter--claude-wt-x
//
// （'.' 也被替换，因此才有连续的两个 '-'。）本机 22 个项目目录逐一比对吻合。
//
// 这是从观测反推出来的规则，不是公开契约。因此调用方必须容忍目录不存在 ——
// 上游改了命名规则时，行为应退化成「读不到 subagent 事件」，而不是报错。
func ProjectSlug(cwd string) string {
	var b strings.Builder
	b.Grow(len(cwd))
	for _, r := range cwd {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// ProjectsRoot 返回 claude 存放会话记录的根目录。
//
// 取 CLAUDE_CONFIG_DIR（Claude Code 自己认这个变量），否则 $HOME/.claude。
// 注意这里读的是 Lathe 进程的环境，而不是 agent 子进程那份白名单环境
// （sanitizedEnv）—— 两者的 HOME 相同，且本函数是控制面在读文件，
// 不涉及给子进程传值。
func ProjectsRoot() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "projects")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// SubagentDir 拼出一次会话的 subagent 记录目录。
func SubagentDir(root, cwd, sessionID string) string {
	if root == "" || cwd == "" || sessionID == "" {
		return ""
	}
	return filepath.Join(root, ProjectSlug(cwd), sessionID, "subagents")
}

// transcriptLine 是 transcript 里一行的公共外壳。
//
// 与 stdout 的 stream-json 不是同一种形状（多了 uuid/timestamp/agentId 这类
// 记账字段），但 type=user/assistant 那两类的 message.content 内层结构一致 ——
// 因此提炼可以直接复用 Digest，不必重写一套块解析。
type transcriptLine struct {
	Type    string `json:"type"`
	AgentID string `json:"agentId"`
	// IsSidechain 为真表示这是 subagent 的记录。subagents/ 目录下的行应当
	// 全都是 true；不做强校验，只用于兜底判断。
	IsSidechain bool `json:"isSidechain"`
	Message     struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// SubagentReader 增量读取一次会话下所有 subagent 的记录。
//
// 只往前读：每个文件记住已消费的字节偏移，每次 Poll 只处理新追加的**完整**行。
// 半行（正在写入）留到下一轮 —— 偏移不前进即可，无需缓冲。
//
// 不是并发安全的：调用方（runner 的 watcher 协程）串行调用 Poll。
type SubagentReader struct {
	dir string

	// offsets 记录每个文件已消费到的字节位置
	offsets map[string]int64
	// started 记录已经发过 agent_start 的 agentId，避免重复发头
	started map[string]bool
}

// NewSubagentReader 构造 reader。dir 不存在是合法的（agent 可能压根没派活），
// Poll 会一直返回空直到目录出现。
func NewSubagentReader(dir string) *SubagentReader {
	return &SubagentReader{
		dir:     dir,
		offsets: map[string]int64{},
		started: map[string]bool{},
	}
}

// SeekToEnd 把已存在文件的偏移移到当前末尾，只读此后追加的内容。
//
// 为什么必须有：修复轮走 --resume，用的是**同一个** session ID，因此
// subagents/ 目录里留着上一轮的记录。而那些事件在上一轮执行时已经由它
// 自己的 EventSink 落过库了 —— 从偏移 0 重读会把它们再灌一遍，详情页
// 上出现两份同样的子 agent 步骤。
//
// 顺带解决一个副作用：首轮 Poll 若一次吐出几百条（实测单个 subagent 目录
// 有 727 条事件），会瞬间冲垮 EventSink 的有界缓冲导致丢事件。
//
// 全新会话里目录还不存在，此时是空操作。
func (r *SubagentReader) SeekToEnd() {
	if r == nil || r.dir == "" {
		return
	}
	names, err := filepath.Glob(filepath.Join(r.dir, "agent-*.jsonl"))
	if err != nil {
		return
	}
	for _, name := range names {
		st, err := os.Stat(name)
		if err != nil {
			continue
		}
		r.offsets[name] = st.Size()
		// 这些文件的分组头早已发过（上一轮），不能再发一次
		id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(name), "agent-"), ".jsonl")
		r.started[id] = true
	}
}

// maxSubagentLine 是单行上限。transcript 的行比 stdout 的 init 事件小得多
// （没有工具清单那一坨），1MB 足够，同时避免异常大文件把内存吃光。
const maxSubagentLine = 1 << 20

// Poll 读取自上次调用以来新增的内容，提炼成事件条目。
//
// 永不返回错误：目录不存在、文件读不了、单行解析失败，都只意味着这一轮
// 少产出一些条目。可见性不该成为任务的故障源（沿用 EventSink 的立场）。
func (r *SubagentReader) Poll() []Entry {
	if r == nil || r.dir == "" {
		return nil
	}
	names, err := filepath.Glob(filepath.Join(r.dir, "agent-*.jsonl"))
	if err != nil || len(names) == 0 {
		return nil
	}
	// 固定顺序：同一轮里多个 subagent 的事件不能因目录枚举顺序而抖动
	sort.Strings(names)

	var out []Entry
	for _, name := range names {
		out = append(out, r.pollFile(name)...)
	}
	return out
}

func (r *SubagentReader) pollFile(path string) []Entry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	off := r.offsets[path]
	if _, err := f.Seek(off, 0); err != nil {
		return nil
	}

	// 扫描前只 stat 一次：下面靠「已消费字节是否触到文件末尾」判断最后一行
	// 是否完整。放进循环里既多花 syscall，又会因为文件边写边读而抖动。
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	size := st.Size()

	// agentId 优先取自文件名（agent-<id>.jsonl）：行内的 agentId 字段不是
	// 每行都有，而分组必须始终有值。
	fallbackID := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "agent-"), ".jsonl")

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxSubagentLine)

	var out []Entry
	consumed := off
	for sc.Scan() {
		raw := sc.Bytes()
		// 只有读到完整的一行（末尾有换行符）才前进偏移。claude 正在写入的
		// 半行会被 Scanner 当成最后一个 token 返回 —— 若就此消费掉，那行的
		// 后半截下一轮就永远补不回来了。
		advance := int64(len(raw)) + 1
		if consumed+advance > size {
			break
		}
		consumed += advance

		line := strings.TrimSpace(string(raw))
		if line == "" || line[0] != '{' {
			continue
		}
		out = append(out, r.digestTranscriptLine([]byte(line), fallbackID)...)
	}
	if err := sc.Err(); err != nil {
		// 读坏了就不推进偏移，下一轮重试这一段
		return out
	}
	r.offsets[path] = consumed
	return out
}

// digestTranscriptLine 把 transcript 的一行提炼成条目，并打上 AgentID。
func (r *SubagentReader) digestTranscriptLine(line []byte, fallbackID string) []Entry {
	var tl transcriptLine
	if err := json.Unmarshal(line, &tl); err != nil {
		return nil
	}

	// 只要消息类的行。transcript 里还混着 file-history-snapshot、summary
	// 之类的记账行，它们不是「agent 干了什么」，提炼出来只是噪声。
	if tl.Type != "user" && tl.Type != "assistant" {
		return nil
	}

	agentID := tl.AgentID
	if agentID == "" {
		agentID = fallbackID
	}

	// subagent 的第一行是派给它的任务描述（type=user，content 是纯文本）。
	// 走 Digest 的 user 路径会被误标成「工具结果」—— 那条路只认 tool_result
	// 块，因为无人值守模式下 user 事件只用于回灌工具结果。这里单独处理，
	// 顺带充当界面上的分组头：「这个 subagent 被派去干什么」。
	if !r.started[agentID] {
		r.started[agentID] = true
		if prompt, ok := asString(tl.Message.Content); ok && strings.TrimSpace(prompt) != "" {
			return []Entry{{
				Kind:    KindAgentStart,
				AgentID: agentID,
				Body:    truncate(prompt, maxEntryBody),
				Payload: map[string]any{"agentId": agentID},
			}}
		}
		// 首行不是纯文本 prompt（格式变了）也要立个头，否则界面没有分组标题
		out := []Entry{{
			Kind:    KindAgentStart,
			AgentID: agentID,
			Body:    fmt.Sprintf("子 agent %s 开始工作", agentID),
			Payload: map[string]any{"agentId": agentID},
		}}
		return append(out, r.reuseDigest(tl, line, agentID)...)
	}

	return r.reuseDigest(tl, line, agentID)
}

// reuseDigest 借 Digest 提炼 message.content，再补上 AgentID。
//
// 复用而非另写一套块解析：text/thinking/tool_use/tool_result 的内层形状与
// stream-json 一致，连 tool_use 的 id 都在同一个字段上 —— 于是主 agent 那套
// 「发起与结果缝成一条」的配对逻辑，对 subagent 天然也成立。
func (r *SubagentReader) reuseDigest(tl transcriptLine, line []byte, agentID string) []Entry {
	entries := Digest(Event{
		Type: EventType(tl.Type),
		Raw:  line,
	})
	out := entries[:0]
	for _, e := range entries {
		// transcript 行里没有 stream-json 的 envelope，提炼不出结构时
		// Digest 会退化成 KindRaw 并把整行原文塞进 body —— 那是几百字节的
		// 记账字段，对界面是纯噪声。这类直接丢，不落库。
		if e.Kind == KindRaw {
			continue
		}
		e.AgentID = agentID
		out = append(out, e)
	}
	return out
}
