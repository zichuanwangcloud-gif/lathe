package agent

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 归一化映射表（PRD §3.2）：codex 的每一类行 → 已有种类的 Entry。
func TestDigestCodex(t *testing.T) {
	p1 := strings.Split(strings.TrimSpace(readTestdata(t, "codex_probe1.jsonl")), "\n")
	p2 := strings.Split(strings.TrimSpace(readTestdata(t, "codex_probe2.jsonl")), "\n")
	huge := strings.Repeat("汉", 6000)

	cases := []struct {
		name string
		line string
		meta *codexTurnMeta
		// want 为空切片表示期望不产出条目
		want []Entry
		// check 做额外断言（可为 nil）
		check func(t *testing.T, got []Entry)
	}{
		{
			name: "给定 thread.started 断言产出 init 条目带 threadId",
			line: p1[0],
			want: []Entry{{Kind: KindInit, Body: "会话初始化 · codex · thread 01a1c9c0-5d69-7233-a188-0e41e191b3ee",
				Payload: map[string]any{"driver": "codex", "threadId": "01a1c9c0-5d69-7233-a188-0e41e191b3ee"}}},
		},
		{
			name: "给定 turn.started 断言不产出条目",
			line: p1[2],
			want: []Entry{},
		},
		{
			name: "给定 command_execution 发起 断言 tool_use Bash 且外壳已去掉",
			line: p2[3],
			want: []Entry{{Kind: KindToolUse, Tool: "Bash", Body: "Bash echo hello-f06",
				Payload: map[string]any{"driver": "codex", "toolUseId": "item_1", "driverTool": "command_execution"}}},
		},
		{
			name: "给定 command_execution 完成 exit_code=0 断言 tool_result 带 exitCode=0 isError=false",
			line: p2[4],
			want: []Entry{{Kind: KindToolResult, Body: "hello-f06",
				Payload: map[string]any{"driver": "codex", "toolUseId": "item_1", "exitCode": 0, "isError": false}}},
		},
		{
			name: "给定 command_execution 完成 exit_code=2 断言 isError=true",
			line: codexCmdFailedLine,
			check: func(t *testing.T, got []Entry) {
				if len(got) != 1 || got[0].Kind != KindToolResult || got[0].Payload["isError"] != true || got[0].Payload["exitCode"] != 2 {
					t.Errorf("got %+v", got)
				}
			},
		},
		{
			name: "给定 agent_message 断言 text",
			line: p1[3],
			want: []Entry{{Kind: KindText, Body: "F06STUBOK", Payload: map[string]any{"driver": "codex"}}},
		},
		{
			name: "给定 reasoning 断言 thinking",
			line: `{"type":"item.completed","item":{"id":"item_5","type":"reasoning","text":"先看测试"}}`,
			want: []Entry{{Kind: KindThinking, Body: "先看测试", Payload: map[string]any{"driver": "codex"}}},
		},
		{
			name: "给定 file_change 断言 tool_use Edit 逐行列出路径",
			line: `{"type":"item.completed","item":{"id":"item_6","type":"file_change","changes":[{"path":"a.go","kind":"update"},{"path":"b_test.go","kind":"add"}],"status":"completed"}}`,
			want: []Entry{{Kind: KindToolUse, Tool: "Edit", Body: "Edit a.go (update)\nb_test.go (add)",
				Payload: map[string]any{"driver": "codex", "toolUseId": "item_6", "driverTool": "file_change", "files": 2}}},
		},
		{
			name: "给定 mcp_tool_call 发起 断言 tool_use mcp:server/tool",
			line: `{"type":"item.started","item":{"id":"item_7","type":"mcp_tool_call","server":"docs","tool":"search","arguments":{"q":"x"},"status":"in_progress"}}`,
			check: func(t *testing.T, got []Entry) {
				if len(got) != 1 || got[0].Kind != KindToolUse || got[0].Tool != "mcp:docs/search" {
					t.Errorf("got %+v", got)
				}
			},
		},
		{
			name: "给定 web_search 完成 断言 tool_use WebSearch",
			line: `{"type":"item.completed","item":{"id":"item_8","type":"web_search","query":"go 1.25 release"}}`,
			check: func(t *testing.T, got []Entry) {
				if len(got) != 1 || got[0].Tool != "WebSearch" || got[0].Body != "WebSearch go 1.25 release" {
					t.Errorf("got %+v", got)
				}
			},
		},
		{
			name: "给定 turn.completed 断言 result 条目 payload 带 usage 与 driver",
			line: p1[4],
			meta: &codexTurnMeta{NumTurns: 1, DurationMS: 2500},
			check: func(t *testing.T, got []Entry) {
				if len(got) != 1 || got[0].Kind != KindResult {
					t.Fatalf("got %+v", got)
				}
				e := got[0]
				if e.Body != "执行结束 · 成功 · codex · 1 轮 · 2.5s · 费用未知" {
					t.Errorf("body got %q", e.Body)
				}
				if e.Payload["isError"] != false || e.Payload["subtype"] != "success" || e.Payload["numTurns"] != 1 || e.Payload["durationMs"] != int64(2500) {
					t.Errorf("payload got %+v", e.Payload)
				}
				u, ok := e.Payload["usage"].(map[string]any)
				if !ok || u["input_tokens"] != float64(11) || u["output_tokens"] != float64(3) {
					t.Errorf("usage got %+v", e.Payload["usage"])
				}
			},
		},
		{
			name: "给定 turn.failed 断言 result isError=true 且 body 带错误信息",
			line: codexTurnFailLine,
			check: func(t *testing.T, got []Entry) {
				if len(got) != 1 || got[0].Kind != KindResult || got[0].Payload["isError"] != true ||
					got[0].Payload["subtype"] != "error_turn_failed" || !strings.Contains(got[0].Body, "stream disconnected") ||
					!strings.HasPrefix(got[0].Body, "执行结束 · 失败 · codex") {
					t.Errorf("got %+v", got)
				}
			},
		},
		{name: "给定 item error 断言 raw", line: p1[1], check: wantKinds(KindRaw)},
		{name: "给定顶层 error 断言 raw", line: codexTopErrorLine, check: wantKinds(KindRaw)},
		{name: "给定不认识的 item.type 断言 raw", line: codexUnknownItem, check: wantKinds(KindRaw)},
		{name: "给定坏 JSON 断言 raw 不 panic", line: `{"type":"item.completed","item":`, check: wantKinds(KindRaw)},
		{
			name: "给定 6000 个汉字的 aggregated_output 断言 body 按 rune 截断且是合法 UTF-8",
			line: `{"type":"item.completed","item":{"id":"i","type":"command_execution","command":"x","aggregated_output":"` + huge + `","exit_code":0,"status":"completed"}}`,
			check: func(t *testing.T, got []Entry) {
				if len(got) != 1 {
					t.Fatalf("got %d 条", len(got))
				}
				b := got[0].Body
				if !utf8.ValidString(b) {
					t.Error("body 不是合法 UTF-8")
				}
				if max := maxEntryBody + len("…(已截断)"); len(b) > max {
					t.Errorf("body 长度 got %d want ≤ %d", len(b), max)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := Event{Driver: DriverCodex, Raw: []byte(tc.line), codexTurn: tc.meta}
			got := Digest(ev)
			for _, e := range got {
				if e.Payload["driver"] != DriverCodex {
					t.Errorf("每条 codex 条目的 payload 都应带 driver=codex，got %+v", e.Payload)
				}
			}
			if tc.check != nil {
				tc.check(t, got)
			}
			if tc.want == nil {
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("条目数 got %d want %d：%+v", len(got), len(tc.want), got)
			}
			for i := range tc.want {
				g, w := got[i], tc.want[i]
				if g.Kind != w.Kind || g.Tool != w.Tool || g.Body != w.Body {
					t.Errorf("[%d] got {%s %s %q} want {%s %s %q}", i, g.Kind, g.Tool, g.Body, w.Kind, w.Tool, w.Body)
				}
				if len(g.Payload) != len(w.Payload) {
					t.Errorf("[%d] payload got %+v want %+v", i, g.Payload, w.Payload)
				}
				for k, wv := range w.Payload {
					if g.Payload[k] != wv {
						t.Errorf("[%d] payload[%s] got %#v want %#v", i, k, g.Payload[k], wv)
					}
				}
			}
		})
	}

	t.Run("给定 claude 事件（Driver 为空）断言仍走 claude 分支、payload 不带 driver", func(t *testing.T) {
		got := Digest(Event{Type: EventAssistant, Raw: []byte(assistantLine)})
		if len(got) != 1 || got[0].Kind != KindText {
			t.Fatalf("got %+v", got)
		}
		if _, ok := got[0].Payload["driver"]; ok {
			t.Errorf("claude 条目不应带 driver 键: %+v", got[0].Payload)
		}
	})
}

func wantKinds(kinds ...string) func(t *testing.T, got []Entry) {
	return func(t *testing.T, got []Entry) {
		t.Helper()
		if len(got) != len(kinds) {
			t.Fatalf("条目数 got %d want %d：%+v", len(got), len(kinds), got)
		}
		for i, k := range kinds {
			if got[i].Kind != k {
				t.Errorf("[%d] kind got %s want %s", i, got[i].Kind, k)
			}
		}
	}
}

func TestStripShellWrapper(t *testing.T) {
	cases := []struct{ in, want string }{
		{`/bin/bash -lc 'echo hello-f06'`, "echo hello-f06"},
		{`/bin/bash -lc 'printf '\''a b'\'' > f'`, "printf 'a b' > f"},
		{`bash -c "go test \"./...\""`, `go test "./..."`},
		{"go test ./...", "go test ./..."},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := stripShellWrapper(tc.in); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}
