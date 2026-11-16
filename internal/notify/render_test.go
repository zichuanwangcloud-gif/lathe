package notify

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// AC-1：五类事件的标题/正文都带上该带的东西；超长原因不会把正文撑破飞书 20 KB 上限。
func TestRenderCoversFiveEvents(t *testing.T) {
	const base = "https://lathe.example.com"
	taskCases := []struct {
		event, state string
	}{
		{EventFailed, "failed"},
		{EventBlockedSpec, "blocked_spec"},
		{EventAwaitingApproval, "awaiting_approval"},
		{EventPROpen, "pr_open"},
	}
	for _, c := range taskCases {
		t.Run("给定任务事件 "+c.event+" 断言标题含单号、正文含人话标签与任务链接", func(t *testing.T) {
			f := Facts{TaskID: 42, ExternalKey: "CR-1326", State: c.state, BaseURL: base,
				FailureReason: "boom", FailureStage: "implement", PRURL: "https://github.com/acme/demo/pull/7"}
			title, text := Render(c.event, f)
			if !strings.Contains(title, "CR-1326") {
				t.Errorf("title got %q, want 含 CR-1326", title)
			}
			if want := StateLabel(c.state); !strings.Contains(text, want) {
				t.Errorf("text got %q, want 含 %q", text, want)
			}
			if want := base + "/tasks/42"; !strings.Contains(text, want) {
				t.Errorf("text got %q, want 含 %q", text, want)
			}
		})
	}

	t.Run("给定 PRD 待签字 断言正文含模糊任务编号与 PRD 链接", func(t *testing.T) {
		title, text := Render(EventPRDReady, Facts{PRDID: 9, PRDRepo: "acme/lathe", BaseURL: base})
		if !strings.Contains(text, "模糊任务 #9") {
			t.Errorf("text got %q, want 含 模糊任务 #9", text)
		}
		if want := base + "/prds/9"; !strings.Contains(text, want) {
			t.Errorf("text got %q, want 含 %q", text, want)
		}
		if !strings.Contains(title, EventLabel(EventPRDReady)) {
			t.Errorf("title got %q, want 含 %q", title, EventLabel(EventPRDReady))
		}
	})

	t.Run("给定 20000 rune 的失败原因 断言正文 ≤ 8192 字节", func(t *testing.T) {
		f := Facts{TaskID: 1, ExternalKey: "CR-1", State: "failed", BaseURL: base,
			FailureReason: strings.Repeat("错", 20000)}
		_, text := Render(EventFailed, f)
		if got := len(text); got > 8192 {
			t.Errorf("len(text) got %d, want <= 8192", got)
		}
		if !utf8.ValidString(text) {
			t.Error("text 不是合法 UTF-8")
		}
	})
}

// AC-2：库里 payload 即使混进杂键也不会外发；payload 结构体只有契约里的 9 个键。
func TestRenderNeverLeaksInternalPaths(t *testing.T) {
	t.Run("给定混进 branch/worktree/session 的 payload 断言正文不含它们", func(t *testing.T) {
		raw := `{"task_id":42,"external_key":"CR-42","to_state":"failed","reason":"x",
			"branch":"lathe/task-42","worktree_path":"/var/lathe/workspaces/42","session_id":"sess-abc"}`
		var p Payload
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		title, text := Render(EventFailed, FactsFromPayload(EventFailed, p, "https://x.example"))
		for _, bad := range []string{"lathe/task-42", "/var/lathe/workspaces/42", "sess-abc"} {
			if strings.Contains(title, bad) || strings.Contains(text, bad) {
				t.Errorf("正文泄漏 %q：got title=%q text=%q", bad, title, text)
			}
		}
	})

	t.Run("给定 Payload 结构体 断言 JSON 键集合恰为契约 9 键", func(t *testing.T) {
		want := []string{"external_key", "failure_stage", "pr_url", "prd_id", "prd_repo",
			"reason", "task_id", "test_nonce", "to_state"}
		var got []string
		rt := reflect.TypeOf(Payload{})
		for i := 0; i < rt.NumField(); i++ {
			tag := rt.Field(i).Tag.Get("json")
			got = append(got, strings.Split(tag, ",")[0])
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Payload 键 got %v, want %v", got, want)
		}
	})
}

// AC-3b：渲染侧按 rune 截到 800，不切坏多字节字符。
func TestRenderTruncatesReasonByRune(t *testing.T) {
	reasonSection := func(t *testing.T, text string) string {
		t.Helper()
		const head = "失败原因：\n"
		i := strings.Index(text, head)
		if i < 0 {
			t.Fatalf("正文里没有失败原因段：%q", text)
		}
		rest := text[i+len(head):]
		if j := strings.Index(rest, "\n"); j >= 0 {
			rest = rest[:j]
		}
		return rest
	}
	long := strings.Repeat("中", 2000)
	edge := strings.Repeat("汉", 795) + "…(已截断)"
	cases := []struct {
		name, reason, want string
	}{
		{"给定 2000 rune 中文 断言截到前 800 rune", long, strings.Repeat("中", 800)},
		{"给定 100 字节原因 断言逐字出现", "short reason …(已截断)", "short reason …(已截断)"},
		{"给定 795 汉字+runner 后缀共 801 rune 断言取前 800 rune、不以 (已截断) 结尾", edge, string([]rune(edge)[:800])},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, text := Render(EventFailed, Facts{TaskID: 1, ExternalKey: "CR-1", State: "failed", FailureReason: c.reason})
			if !utf8.ValidString(text) || strings.ContainsRune(text, utf8.RuneError) {
				t.Fatalf("text 非法 UTF-8 或含 U+FFFD：%q", text)
			}
			got := reasonSection(t, text)
			if n := utf8.RuneCountInString(got); n > 800 {
				t.Errorf("原因段 rune 数 got %d, want <= 800", n)
			}
			if got != c.want {
				t.Errorf("原因段 got %q, want %q", got, c.want)
			}
		})
	}
	t.Run("边角：801 rune 不以 (已截断) 结尾", func(t *testing.T) {
		_, text := Render(EventFailed, Facts{TaskID: 1, ExternalKey: "CR-1", State: "failed", FailureReason: edge})
		if got := reasonSection(t, text); strings.HasSuffix(got, "(已截断)") {
			t.Errorf("got %q 以 (已截断) 结尾，want 被 800 rune 这一刀切掉", got)
		}
	})
}

// AC-4：没有 BaseURL 时省略链接行，不拼 localhost。
func TestRenderOmitsLinkWithoutBaseURL(t *testing.T) {
	for _, ev := range []string{EventFailed, EventPROpen, EventPRDReady, EventTest} {
		t.Run("给定空 BaseURL 事件 "+ev+" 断言正文不含 http", func(t *testing.T) {
			_, text := Render(ev, Facts{TaskID: 3, PRDID: 3, ExternalKey: "CR-3", State: ev})
			if strings.Contains(text, "http") {
				t.Errorf("text got %q, want 不含 http", text)
			}
		})
	}
}

func TestLabels(t *testing.T) {
	cases := []struct{ name, got, want string }{
		{"failed", StateLabel("failed"), "处理失败"},
		{"pr_open", StateLabel("pr_open"), "已开 PR，待你评审"},
		{"awaiting_approval", StateLabel("awaiting_approval"), "验证已通过，等你放行"},
		{"merged", StateLabel("merged"), "已合并"},
		{"cancelled", StateLabel("cancelled"), "已取消"},
		{"blocked_spec", StateLabel("blocked_spec"), "需求不明确，已回帖提问"},
		{"未知状态回退原串", StateLabel("weird"), "weird"},
		{"事件 failed = 状态标签", EventLabel(EventFailed), "处理失败"},
		{"事件 prd", EventLabel(EventPRDReady), "模糊任务待你签字"},
		{"事件 test", EventLabel(EventTest), "测试消息"},
		{"未知事件回退原串", EventLabel("weird"), "weird"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %q, want %q", c.got, c.want)
			}
		})
	}
}

func TestEventKeysAndTime(t *testing.T) {
	t0 := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct{ name, got, want string }{
		{"任务键：纳秒输入截到微秒", TaskEventKey(TaskEvent{TaskID: 5, ToState: "failed", At: t0.Add(123456789)}),
			"task:5:failed:2000-01-01T00:00:00.123456Z"},
		{"PRD 键", PRDEventKey(PRDEvent{PRDID: 1, ToState: "ready_for_review", At: time.Unix(1700000000, 0)}),
			"prd:1:ready_for_review:2023-11-14T22:13:20Z"},
		{"测试键", TestEventKey("abc"), "test:abc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.got != c.want {
				t.Errorf("got %q, want %q", c.got, c.want)
			}
		})
	}
	t.Run("EventTime 纳秒截断、微秒恒等", func(t *testing.T) {
		if got, want := EventTime(t0.Add(123456789)), t0.Add(123456*time.Microsecond); !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
		us := t0.Add(123456 * time.Microsecond)
		if got := EventTime(us); !got.Equal(us) {
			t.Errorf("got %v, want %v", got, us)
		}
	})
}

func TestNextAttempt(t *testing.T) {
	now := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		n        int
		wantGap  time.Duration
		wantGive bool
	}{
		{1, BaseBackoff, false},
		{2, 2 * BaseBackoff, false},
		{3, 4 * BaseBackoff, false},
		{4, 8 * BaseBackoff, false},
		{5, 16 * BaseBackoff, true},
		{6, 32 * BaseBackoff, true},
		{10, MaxBackoff, true},
		{1000, MaxBackoff, true},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("给定 n=%d", c.n), func(t *testing.T) {
			next, give := NextAttempt(c.n, now)
			want := c.wantGap
			if want > MaxBackoff {
				want = MaxBackoff
			}
			if got := next.Sub(now); got != want {
				t.Errorf("next-now got %v, want %v", got, want)
			}
			if give != c.wantGive {
				t.Errorf("giveUp got %v, want %v", give, c.wantGive)
			}
		})
	}
}

func TestPayloadRoundTrip(t *testing.T) {
	ev := TaskEvent{TaskID: 4, UserID: 2, ExternalKey: "CR-4", ToState: "failed", EventType: EventFailed,
		FailureStage: "implement", Reason: "r", PRURL: "ignored-for-failed"}
	p := TaskPayload(ev)
	want := Payload{TaskID: 4, ExternalKey: "CR-4", ToState: "failed", FailureStage: "implement", Reason: "r"}
	if p != want {
		t.Errorf("TaskPayload got %+v, want %+v", p, want)
	}
	f := FactsFromPayload(EventFailed, p, "https://b")
	if f.TaskID != 4 || f.ExternalKey != "CR-4" || f.FailureReason != "r" || f.FailureStage != "implement" || f.BaseURL != "https://b" {
		t.Errorf("FactsFromPayload got %+v", f)
	}
	pp := PRDPayload(PRDEvent{PRDID: 8, UserID: 2, Repo: "acme/x"})
	if want := (Payload{PRDID: 8, PRDRepo: "acme/x"}); pp != want {
		t.Errorf("PRDPayload got %+v, want %+v", pp, want)
	}
}

func TestEventSwitchesAllows(t *testing.T) {
	all := EventSwitches{Failed: true, BlockedSpec: true, AwaitingApproval: true, PROpen: true, PRDReady: true}
	cases := []struct {
		name string
		sw   EventSwitches
		ev   string
		want bool
	}{
		{"全开 failed", all, EventFailed, true},
		{"关 failed", EventSwitches{BlockedSpec: true, AwaitingApproval: true, PROpen: true, PRDReady: true}, EventFailed, false},
		{"全关 test 恒 true", EventSwitches{}, EventTest, true},
		{"关 prd", EventSwitches{Failed: true}, EventPRDReady, false},
		{"未知事件 false", all, "weird", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.sw.Allows(c.ev); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// AC-17b：租约覆盖一次发送的上界（留足漂移余量）。
func TestLeaseCoversOneSend(t *testing.T) {
	if got, want := ClaimLease, 2*SendTimeout+time.Minute; got < want {
		t.Errorf("ClaimLease got %v, want >= %v", got, want)
	}
}
