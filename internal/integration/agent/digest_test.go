package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// digestLine 按 parseStream 的方式把一行 NDJSON 包成 Event 再提炼。
//
// 刻意复刻 parseStream 的封装步骤（envelope → Event），这样 SessionID
// 之类"由外层填、被 Digest 使用"的字段，其接线也在测试覆盖范围内。
func digestLine(t *testing.T, line string) []Entry {
	t.Helper()
	var env envelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("测试数据不是合法 JSON: %v", err)
	}
	return Digest(Event{
		Type:      EventType(env.Type),
		Subtype:   env.Subtype,
		SessionID: env.SessionID,
		Raw:       json.RawMessage(line),
	})
}

func onlyEntry(t *testing.T, entries []Entry) Entry {
	t.Helper()
	if len(entries) != 1 {
		t.Fatalf("应提炼出 1 条，得到 %d 条: %+v", len(entries), entries)
	}
	return entries[0]
}

// ------------------------------------------------------- tool_use ↔ tool_result

// 界面要把「发起」与「结果」缝成一条带耗时与成败的记录，靠的就是这个 id。
func TestDigestToolUseCarriesID(t *testing.T) {
	line := `{"type":"assistant","session_id":"s1","message":{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"toolu_01ABC","name":"Bash","input":{"command":"go test ./..."}}]}}`

	e := onlyEntry(t, digestLine(t, line))
	if e.Kind != KindToolUse {
		t.Errorf("Kind = %q，期望 %q", e.Kind, KindToolUse)
	}
	if e.Tool != "Bash" {
		t.Errorf("Tool = %q，期望 Bash", e.Tool)
	}
	if got := e.Payload["toolUseId"]; got != "toolu_01ABC" {
		t.Errorf("payload.toolUseId = %v，期望 toolu_01ABC", got)
	}
	if !strings.Contains(e.Body, "go test") {
		t.Errorf("Body 应含入参摘要，得到 %q", e.Body)
	}
}

// 同一次调用的两侧必须给出同一个 key，否则界面拼不起来。
func TestDigestToolUseAndResultShareKey(t *testing.T) {
	useLine := `{"type":"assistant","session_id":"s1","message":{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"toolu_X","name":"Read","input":{"file_path":"/opt/lathe/go.mod"}},` +
		`{"type":"tool_use","id":"toolu_Y","name":"Grep","input":{"pattern":"func main"}}]}}`
	resLine := `{"type":"user","session_id":"s1","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"toolu_Y","content":"cmd/lathe/main.go:20"}]}}`

	uses := digestLine(t, useLine)
	if len(uses) != 2 {
		t.Fatalf("两个 tool_use 块应提炼出 2 条，得到 %d", len(uses))
	}
	if uses[0].Payload["toolUseId"] != "toolu_X" || uses[1].Payload["toolUseId"] != "toolu_Y" {
		t.Fatalf("两条 tool_use 的 id 不对: %v / %v",
			uses[0].Payload["toolUseId"], uses[1].Payload["toolUseId"])
	}

	res := onlyEntry(t, digestLine(t, resLine))
	if res.Kind != KindToolResult {
		t.Errorf("Kind = %q，期望 %q", res.Kind, KindToolResult)
	}
	// 结果应能对上第二次调用，而不是笼统地"某次调用"
	if res.Payload["toolUseId"] != uses[1].Payload["toolUseId"] {
		t.Errorf("结果的 toolUseId = %v，应等于第二次调用的 %v",
			res.Payload["toolUseId"], uses[1].Payload["toolUseId"])
	}
}

// CLI 改字段或老数据没有 id 时，不能丢事件 —— 退回两行平铺即可。
func TestDigestToolUseWithoutIDDegrades(t *testing.T) {
	line := `{"type":"assistant","session_id":"s1","message":{"role":"assistant","content":[` +
		`{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`

	e := onlyEntry(t, digestLine(t, line))
	if e.Kind != KindToolUse || e.Tool != "Bash" {
		t.Errorf("缺 id 也应正常提炼，得到 %+v", e)
	}
	if _, ok := e.Payload["toolUseId"]; ok {
		t.Errorf("缺 id 时不该凭空造一个 key，payload = %v", e.Payload)
	}
}

func TestDigestToolResultError(t *testing.T) {
	line := `{"type":"user","session_id":"s1","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"toolu_Z","is_error":true,"content":"exit status 1"}]}}`

	e := onlyEntry(t, digestLine(t, line))
	if e.Payload["isError"] != true {
		t.Errorf("报错的结果应带 isError=true，payload = %v", e.Payload)
	}
	if e.Payload["toolUseId"] != "toolu_Z" {
		t.Errorf("payload.toolUseId = %v，期望 toolu_Z", e.Payload["toolUseId"])
	}
}

// ------------------------------------------------------------------ init

// 会话 ID 按阶段留档：cwd + sessionId 才能定位 claude 留下的完整 transcript。
func TestDigestInitCarriesSessionAndCWD(t *testing.T) {
	line := `{"type":"system","subtype":"init","session_id":"4ec71c82-abd6-422e-aa9c-ff6b4ab84358",` +
		`"cwd":"/opt/lathe/workspaces/cr-1363","model":"claude-opus-5","permissionMode":"acceptEdits",` +
		`"tools":["Bash","Read","Agent"]}`

	e := onlyEntry(t, digestLine(t, line))
	if e.Kind != KindInit {
		t.Fatalf("Kind = %q，期望 %q", e.Kind, KindInit)
	}
	if got := e.Payload["sessionId"]; got != "4ec71c82-abd6-422e-aa9c-ff6b4ab84358" {
		t.Errorf("payload.sessionId = %v，期望完整会话 ID", got)
	}
	if got := e.Payload["cwd"]; got != "/opt/lathe/workspaces/cr-1363" {
		t.Errorf("payload.cwd = %v，期望 worktree 路径", got)
	}
	if got := e.Payload["toolCount"]; got != 3 {
		t.Errorf("payload.toolCount = %v，期望 3", got)
	}
	// 工具清单本身不该入库：它是这类事件动辄数 KB 的唯一原因
	if _, ok := e.Payload["tools"]; ok {
		t.Errorf("不该保留完整工具清单，payload = %v", e.Payload)
	}
}

// 没有 session_id 的 init（异常输出）不该造出空字符串 key。
func TestDigestInitWithoutSessionID(t *testing.T) {
	line := `{"type":"system","subtype":"init","cwd":"/tmp","model":"m"}`

	e := onlyEntry(t, digestLine(t, line))
	if _, ok := e.Payload["sessionId"]; ok {
		t.Errorf("缺 session_id 时不该写入该 key，payload = %v", e.Payload)
	}
}

// ------------------------------------------------------------------ 兜底

func TestDigestUnknownEventFallsBackToRaw(t *testing.T) {
	line := `{"type":"system","subtype":"api_retry","attempt":2}`

	e := onlyEntry(t, digestLine(t, line))
	if e.Kind != KindRaw {
		t.Errorf("Kind = %q，期望 %q（提炼不出结构时保留原文）", e.Kind, KindRaw)
	}
	if !strings.Contains(e.Body, "api_retry") {
		t.Errorf("Body 应保留原文，得到 %q", e.Body)
	}
}
