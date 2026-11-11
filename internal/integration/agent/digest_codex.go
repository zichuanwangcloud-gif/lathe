package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// digest_codex.go —— 把 codex `exec --json` 的一行提炼成已有种类的 Entry。
//
// 立场与 claude 侧完全相同：只产出 agent_events CHECK 约束里已有的 kind，
// 永不返回错误，认不出的形状退化成 KindRaw。工具名用 Bash / Edit 这类
// claude 用户熟悉的名字、发起与结果用 toolUseId 配对、结果带 exitCode/isError
// —— 界面现有的工具摘要与配对不用为 codex 写分支。
//
// 每一条 codex 产出的 Entry，payload 都带 driver:"codex"（库里按驱动筛事件
// 靠它）；claude 的 Entry 不加这个键（claude 分支一行不改）。

func digestCodex(ev Event) []Entry {
	var l codexLine
	if err := json.Unmarshal(ev.Raw, &l); err != nil {
		return []Entry{codexRaw(ev)}
	}
	switch l.Type {
	case "thread.started":
		return []Entry{{
			Kind:    KindInit,
			Body:    truncate(fmt.Sprintf("会话初始化 · codex · thread %s", orDash(l.ThreadID)), maxEntryBody),
			Payload: map[string]any{"driver": DriverCodex, "threadId": l.ThreadID},
		}}
	case "turn.started":
		return nil
	case "turn.completed", "turn.failed":
		return []Entry{digestCodexTurn(ev, &l)}
	case "item.started", "item.updated", "item.completed":
		if l.Item == nil {
			return []Entry{codexRaw(ev)}
		}
		return digestCodexItem(ev, l.Type, l.Item)
	default:
		// 顶层 error（重连提示）与不认识的 type：保留原文
		return []Entry{codexRaw(ev)}
	}
}

func digestCodexItem(ev Event, phase string, it *codexItem) []Entry {
	started, done := phase == "item.started", phase == "item.completed"
	switch it.Type {
	case "agent_message":
		if !done || strings.TrimSpace(it.Text) == "" {
			return nil
		}
		return []Entry{codexEntry(KindText, "", it.Text, nil)}

	case "reasoning":
		if !done || strings.TrimSpace(it.Text) == "" {
			return nil
		}
		return []Entry{codexEntry(KindThinking, "", it.Text, nil)}

	case "command_execution":
		if started {
			return []Entry{codexEntry(KindToolUse, "Bash", "Bash "+oneLine(stripShellWrapper(it.Command)),
				map[string]any{"toolUseId": it.ID, "driverTool": "command_execution"})}
		}
		if done {
			payload := map[string]any{"toolUseId": it.ID}
			isErr := it.Status == "failed" || it.Status == "declined"
			if it.ExitCode != nil {
				payload["exitCode"] = *it.ExitCode
				isErr = isErr || *it.ExitCode != 0
			}
			payload["isError"] = isErr
			return []Entry{codexEntry(KindToolResult, "", it.AggregatedOutput, payload)}
		}
		return nil

	case "file_change":
		if !done {
			return nil
		}
		lines := make([]string, 0, len(it.Changes))
		for _, c := range it.Changes {
			lines = append(lines, fmt.Sprintf("%s (%s)", c.Path, orDash(c.Kind)))
		}
		return []Entry{codexEntry(KindToolUse, "Edit", "Edit "+strings.Join(lines, "\n"),
			map[string]any{"toolUseId": it.ID, "driverTool": "file_change", "files": len(it.Changes)})}

	case "mcp_tool_call":
		tool := "mcp:" + it.Server + "/" + it.Tool
		if started {
			body := tool
			if len(it.Arguments) > 0 && string(it.Arguments) != "null" {
				body += " " + oneLine(string(it.Arguments))
			}
			return []Entry{codexEntry(KindToolUse, tool, body,
				map[string]any{"toolUseId": it.ID, "driverTool": "mcp_tool_call"})}
		}
		if done {
			payload := map[string]any{"toolUseId": it.ID}
			body := mcpResultText(it.Result)
			isErr := it.Status == "failed"
			if it.Error != nil && it.Error.Message != "" {
				isErr = true
				body = it.Error.Message
			}
			payload["isError"] = isErr
			return []Entry{codexEntry(KindToolResult, "", body, payload)}
		}
		return nil

	case "web_search":
		if !done {
			return nil
		}
		return []Entry{codexEntry(KindToolUse, "WebSearch", "WebSearch "+oneLine(it.Query),
			map[string]any{"toolUseId": it.ID, "driverTool": "web_search"})}

	case "todo_list":
		if len(it.Items) == 0 {
			return nil
		}
		var sb strings.Builder
		sb.WriteString("计划：")
		for _, t := range it.Items {
			mark := "[ ]"
			if t.Completed {
				mark = "[x]"
			}
			sb.WriteString("\n" + mark + " " + t.Text)
		}
		return []Entry{codexEntry(KindText, "", sb.String(), nil)}

	default:
		// error item（如「Model metadata ... not found」）与不认识的 item.type
		return []Entry{codexRaw(ev)}
	}
}

func digestCodexTurn(ev Event, l *codexLine) Entry {
	failed := l.Type == "turn.failed"
	subtype, verdict := "success", "成功"
	if failed {
		subtype, verdict = "error_turn_failed", "失败"
	}
	turns, durMS := 1, int64(0)
	if ev.codexTurn != nil {
		turns, durMS = ev.codexTurn.NumTurns, ev.codexTurn.DurationMS
	}
	body := fmt.Sprintf("执行结束 · %s · codex · %d 轮 · %.1fs · 费用未知",
		verdict, turns, float64(durMS)/1000)
	if failed && l.Error != nil && strings.TrimSpace(l.Error.Message) != "" {
		body += "\n\n" + l.Error.Message
	}
	payload := map[string]any{
		"driver":     DriverCodex,
		"subtype":    subtype,
		"isError":    failed,
		"numTurns":   turns,
		"durationMs": durMS,
	}
	if len(l.Usage) > 0 && string(l.Usage) != "null" {
		// 原样给人读时间线用；不建表、不聚合（那是 f03 的事）
		var u map[string]any
		if json.Unmarshal(l.Usage, &u) == nil {
			payload["usage"] = u
		}
	}
	return Entry{Kind: KindResult, Body: truncate(body, maxEntryBody), Payload: payload}
}

// codexEntry 构造一条带 driver:"codex" 的条目，body 一律按 rune 截断。
func codexEntry(kind, tool, body string, payload map[string]any) Entry {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["driver"] = DriverCodex
	return Entry{Kind: kind, Tool: tool, Body: truncate(body, maxEntryBody), Payload: payload}
}

func codexRaw(ev Event) Entry {
	e := rawEntry(ev)
	e.Payload["driver"] = DriverCodex
	return e
}

// stripShellWrapper 去掉 codex 给命令套的 `/bin/bash -lc '…'` 外壳，
// 让界面显示 agent 真正想跑的那条命令。认不出外壳就原样返回。
func stripShellWrapper(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	for _, sh := range []string{"/bin/bash", "/usr/bin/bash", "bash", "/bin/zsh", "/usr/bin/zsh", "zsh", "/bin/sh", "sh"} {
		for _, flag := range []string{" -lc ", " -c "} {
			prefix := sh + flag
			if !strings.HasPrefix(cmd, prefix) {
				continue
			}
			if inner, ok := shellUnquote(cmd[len(prefix):]); ok {
				return inner
			}
		}
	}
	return cmd
}

// shellUnquote 解开一个完整的单引号或双引号字符串。
func shellUnquote(s string) (string, bool) {
	if len(s) < 2 {
		return "", false
	}
	switch {
	case s[0] == '\'' && s[len(s)-1] == '\'':
		// 单引号里不能有单引号；shell 的写法是 '\'' 拼接
		return strings.ReplaceAll(s[1:len(s)-1], `'\''`, `'`), true
	case s[0] == '"' && s[len(s)-1] == '"':
		r := strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\$`, `$`, "\\`", "`")
		return r.Replace(s[1 : len(s)-1]), true
	}
	return "", false
}

// mcpResultText 从 mcp 结果里取文本块；取不到就给紧凑 JSON。
func mcpResultText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var r struct {
		Content []contentBlock `json:"content"`
	}
	if json.Unmarshal(raw, &r) == nil && len(r.Content) > 0 {
		var parts []string
		for _, b := range r.Content {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return string(raw)
}
