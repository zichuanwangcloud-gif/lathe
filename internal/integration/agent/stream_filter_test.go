package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这一组是 f01（运行中插话，docs/features/interject.md）的驱动层「A 类」用例：
// 断言只用改动前就存在的签名（parseStream / buildArgs / Run），因此在改动前
// 就能跑 —— 过滤类的断言改动前必须失败，回归红线改动前后都必须绿。

// sourceKey 是「来源 type[/subtype]」，与 Digest 产出的 kind 拼成计数键。
func sourceKey(e Event) string {
	k := string(e.Type)
	if e.Subtype != "" {
		k += "/" + e.Subtype
	}
	return k
}

// digestCounts 用既有签名 parseStream 解析一份真实捕获，并按
// 「来源 → kind」计数 Digest 的产出。
func digestCounts(t *testing.T, path string) (counts map[string]int, events []Event, res *Result) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开 fixture 失败: %v", err)
	}
	defer f.Close()
	counts = map[string]int{}
	res, err = parseStream(f, func(e Event) {
		events = append(events, e)
		for _, en := range Digest(e) {
			counts[sourceKey(e)+"->"+en.Kind]++
		}
	})
	if err != nil {
		t.Fatalf("parseStream 报错: %v", err)
	}
	return counts, events, res
}

// AC-4：真实捕获（两次 type=user 注入、两条 result）喂给既有签名 parseStream，
// 人插的话不得以 agent 正文出现，command_lifecycle 不得退化成 raw；
// 多条 result 时 NumTurns/DurationMS 求和、Text/CostUSD 取末条。
func TestParseStreamFiltersReplayAndLifecycle(t *testing.T) {
	counts, events, res := digestCounts(t, filepath.Join("testdata", "replay-user.ndjson"))

	if got, want := len(events), 6; got != want {
		t.Errorf("onEvent 调用次数 got=%d want=%d（2 system + 2 assistant + 2 result）", got, want)
	}
	for _, e := range events {
		if e.Type == EventUser || e.Type == "command_lifecycle" {
			t.Errorf("交给 onEvent 的事件里不该有 %q", e.Type)
		}
	}
	userEntries, rawEntries := 0, 0
	for k, n := range counts {
		if strings.HasPrefix(k, "user->") {
			userEntries += n
		}
		if strings.HasSuffix(k, "->raw") {
			rawEntries += n
		}
	}
	if userEntries != 0 {
		t.Errorf("由 type=user 产出的 Digest 条目 got=%d want=0（人插的话不得记成 agent 正文）；计数=%v", userEntries, counts)
	}
	if rawEntries != 0 {
		t.Errorf("kind=raw 条目 got=%d want=0；计数=%v", rawEntries, counts)
	}

	if res == nil {
		t.Fatal("应解析出 result")
	}
	if got, want := res.Text, "SECOND"; got != want {
		t.Errorf("Text got=%q want=%q（末条）", got, want)
	}
	if got, want := res.CostUSD, 0.0976604; got != want {
		t.Errorf("CostUSD got=%v want=%v（进程累计，取末条）", got, want)
	}
	if got, want := res.NumTurns, 2; got != want {
		t.Errorf("NumTurns got=%d want=%d（单 turn 量，求和）", got, want)
	}
	if got, want := res.DurationMS, int64(2759+2499); got != want {
		t.Errorf("DurationMS got=%d want=%d（单 turn 量，求和）", got, want)
	}
}

// AC-4c：真实 CLI 捕获上只消除 f01 引入的两类污染（command_lifecycle→*、
// user(isReplay)→text），既有的 system/<非 init>→raw 与真·工具结果原样保留。
func TestParseStreamKeepsExistingSystemRaw(t *testing.T) {
	type row struct {
		name string
		keys []string // 计数键之和
		a, b int      // a 段 / b 段的 want
	}
	rows := []row{
		{"command_lifecycle→*", []string{"command_lifecycle->raw"}, 0, 0},
		{"user→text", []string{"user->text"}, 0, 0},
		{"user→tool_result", []string{"user->tool_result"}, 0, 1},
		{"system/thinking_tokens→raw", []string{"system/thinking_tokens->raw"}, 18, 0},
		{"system/task_started+task_notification→raw", []string{"system/task_started->raw", "system/task_notification->raw"}, 0, 2},
		{"system/init→init", []string{"system/init->init"}, 1, 1},
	}
	segs := []struct {
		file string
		pick func(row) int
	}{
		{"a-new-session.jsonl", func(r row) int { return r.a }},
		{"b-resume-stream-inject.jsonl", func(r row) int { return r.b }},
	}
	for _, seg := range segs {
		counts, _, _ := digestCounts(t, filepath.Join("testdata", seg.file))
		for _, r := range rows {
			t.Run(seg.file+"/"+r.name, func(t *testing.T) {
				got := 0
				for _, k := range r.keys {
					got += counts[k]
				}
				if want := seg.pick(r); got != want {
					t.Errorf("got=%d want=%d；全部计数=%v", got, want, counts)
				}
			})
		}
	}
}
