package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// maxEntryBody 限制单条 Entry 的可读内容长度。
//
// 事件是给人看进度用的，不是完整日志归档：一条 4KB 的工具输出已经足够
// 判断"这步干了什么、成没成"，再长的部分对界面没有价值，却会把
// agent_events 表撑成整个库里最大的一张。同类截断的先例见
// runner/verify.go 的 maxStepOutput 与 pipeline.go 里的 truncate 调用。
const maxEntryBody = 4 << 10

// Entry 的 Kind 取值，与 agent_events 表的 CHECK 约束一致。
const (
	// KindInit 会话初始化，携带模型与工作目录。
	KindInit = "init"
	// KindText 模型输出的正文。
	KindText = "text"
	// KindThinking 模型的思考过程。
	KindThinking = "thinking"
	// KindToolUse 模型发起的一次工具调用。
	KindToolUse = "tool_use"
	// KindToolResult 工具执行结果。
	KindToolResult = "tool_result"
	// KindResult 终局事件，携带成败、耗时、成本。
	KindResult = "result"
	// KindRaw 提炼不出结构时的兜底，保留截断后的原文。
	KindRaw = "raw"
)

// Entry 是一条提炼后的可读事件，对应 agent_events 表的一行。
//
// 刻意不保留原始 NDJSON：单行上限 16MB（见 maxLineBytes），原样入库既浪费
// 空间也没人看得懂。Payload 只放界面真正会用到的结构化补充。
type Entry struct {
	Kind    string
	Tool    string
	Body    string
	Payload map[string]any
}

// Digest 把一条 stream-json 事件提炼成零到多条可读条目。
//
// 一条 assistant 事件的 message.content 可以同时含正文与多个工具调用，
// 因此返回切片而非单值。
//
// 永不返回错误：CLI 增删字段、输出非预期形状时退化成 KindRaw，让界面
// 至少还能看到"发生了什么"。这与 Event.Raw 的立场一致 —— 驱动不能被
// 上游的格式变动打挂。
func Digest(ev Event) []Entry {
	switch ev.Type {
	case EventSystem:
		return digestSystem(ev)
	case EventAssistant:
		return digestMessage(ev, true)
	case EventUser:
		return digestMessage(ev, false)
	case EventResult:
		return digestResult(ev)
	default:
		return []Entry{rawEntry(ev)}
	}
}

// initEvent 映射 type=system subtype=init 里值得留下的字段。
//
// 刻意不收 tools / mcp_servers / slash_commands 那几份清单：它们是这类事件
// 动辄数 KB 的唯一原因（实测单条 5.4KB），而界面只需要知道"用哪个模型、
// 在哪个目录、带了多少工具"。
type initEvent struct {
	Model          string   `json:"model"`
	CWD            string   `json:"cwd"`
	PermissionMode string   `json:"permissionMode"`
	Tools          []string `json:"tools"`
}

func digestSystem(ev Event) []Entry {
	if ev.Subtype != "init" {
		// api_retry 之类的系统通知：说不出结构，但"CLI 正在重试 API"本身
		// 就是有价值的进度信息，因此保留原文而不是丢弃。
		return []Entry{rawEntry(ev)}
	}

	var ie initEvent
	if err := json.Unmarshal(ev.Raw, &ie); err != nil {
		return []Entry{rawEntry(ev)}
	}

	body := fmt.Sprintf("会话就绪 · 模型 %s", orDash(ie.Model))
	if ie.CWD != "" {
		body += " · " + ie.CWD
	}
	return []Entry{{
		Kind: KindInit,
		Body: truncate(body, maxEntryBody),
		Payload: map[string]any{
			"model":          ie.Model,
			"cwd":            ie.CWD,
			"permissionMode": ie.PermissionMode,
			"toolCount":      len(ie.Tools),
		},
	}}
}

// messageEvent 是 assistant / user 事件的公共外壳。
type messageEvent struct {
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// contentBlock 是 message.content 里的一个块。
//
// 各字段按 type 分别有值，用一个结构体全收比按 type 分别 unmarshal 省事，
// 且对未知 type 天然宽容。
type contentBlock struct {
	Type string `json:"type"`

	Text     string `json:"text"`     // type=text
	Thinking string `json:"thinking"` // type=thinking

	// type=tool_use
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// type=tool_result
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// digestMessage 提炼 assistant 与 user 事件。
//
// assistant 带正文、思考与工具调用；user 事件在无人值守模式下只用于把工具
// 结果回灌给模型，因此只关心其中的 tool_result 块。
func digestMessage(ev Event, assistant bool) []Entry {
	var me messageEvent
	if err := json.Unmarshal(ev.Raw, &me); err != nil {
		return []Entry{rawEntry(ev)}
	}

	// content 可以是字符串（纯文本消息）而非块数组，两种形状都要接。
	var blocks []contentBlock
	if err := json.Unmarshal(me.Message.Content, &blocks); err != nil {
		if s, ok := asString(me.Message.Content); ok && strings.TrimSpace(s) != "" {
			kind := KindText
			if !assistant {
				kind = KindToolResult
			}
			return []Entry{{Kind: kind, Body: truncate(s, maxEntryBody)}}
		}
		return []Entry{rawEntry(ev)}
	}

	out := make([]Entry, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			out = append(out, Entry{Kind: KindText, Body: truncate(b.Text, maxEntryBody)})

		case "thinking":
			if strings.TrimSpace(b.Thinking) == "" {
				continue
			}
			out = append(out, Entry{Kind: KindThinking, Body: truncate(b.Thinking, maxEntryBody)})

		case "tool_use":
			out = append(out, Entry{
				Kind: KindToolUse,
				Tool: b.Name,
				Body: truncate(toolSummary(b.Name, b.Input), maxEntryBody),
			})

		case "tool_result":
			body, _ := asString(b.Content)
			if body == "" {
				// content 是块数组时（图片、结构化结果）取其中的文本块
				body = textOfBlocks(b.Content)
			}
			payload := map[string]any{"toolUseId": b.ToolUseID}
			if b.IsError {
				payload["isError"] = true
			}
			out = append(out, Entry{
				Kind:    KindToolResult,
				Body:    truncate(body, maxEntryBody),
				Payload: payload,
			})
		}
	}

	// 事件里一个可提炼的块都没有（例如只含图片）时，不要静默吞掉。
	if len(out) == 0 {
		return nil
	}
	return out
}

func digestResult(ev Event) []Entry {
	var re resultEvent
	if err := json.Unmarshal(ev.Raw, &re); err != nil {
		return []Entry{rawEntry(ev)}
	}

	verdict := "成功"
	if re.IsError || re.Subtype != "success" {
		verdict = "失败"
	}
	body := fmt.Sprintf("执行结束 · %s · %s · %d 轮 · %.1fs · $%.4f",
		verdict, orDash(re.Subtype), re.NumTurns,
		float64(re.DurationMS)/1000, re.TotalCostUSD)
	if strings.TrimSpace(re.Result) != "" {
		body += "\n\n" + re.Result
	}

	payload := map[string]any{
		"subtype":    re.Subtype,
		"isError":    re.IsError,
		"numTurns":   re.NumTurns,
		"durationMs": re.DurationMS,
		"costUsd":    re.TotalCostUSD,
	}
	if n := len(re.PermissionDenials); n > 0 {
		// 被权限拦住是一种独立的失败模式，必须能在界面上和"改不动代码"区分开
		payload["permissionDenials"] = n
	}
	return []Entry{{
		Kind:    KindResult,
		Body:    truncate(body, maxEntryBody),
		Payload: payload,
	}}
}

// toolSummary 把工具入参压成一行摘要。
//
// 按工具挑最能说明"这步在动什么"的那个参数；未知工具退回整个入参的
// JSON 截断，这样新增工具不必改这里也能显示得过去。
func toolSummary(name string, input json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil || len(m) == 0 {
		return name
	}

	var key string
	switch name {
	case "Read", "Write", "Edit", "NotebookEdit":
		key = "file_path"
	case "Bash", "BashOutput":
		key = "command"
	case "Glob", "Grep":
		key = "pattern"
	case "Task", "Agent":
		key = "description"
	case "WebFetch":
		key = "url"
	case "WebSearch":
		key = "query"
	case "Skill":
		key = "skill"
	}

	if key != "" {
		if s, ok := m[key].(string); ok && s != "" {
			return name + " " + oneLine(s)
		}
	}

	compact, err := json.Marshal(m)
	if err != nil {
		return name
	}
	return name + " " + oneLine(string(compact))
}

// rawEntry 是提炼失败时的兜底：保留类型与截断原文，绝不丢事件。
func rawEntry(ev Event) Entry {
	label := string(ev.Type)
	if ev.Subtype != "" {
		label += "/" + ev.Subtype
	}
	payload := map[string]any{"type": string(ev.Type)}
	if ev.Subtype != "" {
		payload["subtype"] = ev.Subtype
	}
	return Entry{
		Kind:    KindRaw,
		Body:    truncate(label+": "+string(ev.Raw), maxEntryBody),
		Payload: payload,
	}
}

// asString 把 JSON 值当字符串取，不是字符串则返回 false。
func asString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// textOfBlocks 从块数组里拼出所有文本块，用于 tool_result 的结构化内容。
func textOfBlocks(raw json.RawMessage) string {
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(b.Text)
		}
		// 已经够长就不必继续拼，后面反正要被截断
		if sb.Len() > maxEntryBody {
			break
		}
	}
	return sb.String()
}

// oneLine 把多行压成一行，便于在列表里单行显示。
func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i]) + " …"
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
