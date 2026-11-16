package inbox

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/zichuanwangcloud-gif/lathe/internal/prd"
	"github.com/zichuanwangcloud-gif/lathe/internal/runner"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// 纯函数测试：不连库（docs/features/inbox.md §5.2）。

func identity(s string) string { return s }

func strp(s string) *string { return &s }
func i64p(v int64) *int64   { return &v }

func hasLink(it Item, href string) bool {
	for _, l := range it.Extra {
		if l.Href == href {
			return true
		}
	}
	return false
}

func TestInboxTaskItem(t *testing.T) {
	want := map[task.State]Kind{
		task.StateBlockedSpec:      KindTaskBlockedSpec,
		task.StateAwaitingApproval: KindTaskAwaitingApproval,
		task.StateReviewFeedback:   KindTaskReviewParked,
		task.StateBlockedDep:       KindTaskBlockedDep,
		task.StateFailed:           KindTaskFailed,
	}
	for _, st := range task.AllStates() {
		t.Run("状态 "+string(st)+" 的派生结果", func(t *testing.T) {
			r := store.InboxTaskRow{
				ID: 42, State: string(st), ExternalKey: "ENG-1", TrackerProvider: "linear",
				ProviderRepo: "acme/demo", EventID: 77, Since: time.Unix(1700000000, 0),
				FailureReason: strp("boom"),
			}
			it, ok := TaskItem(r, identity)
			wk, wantOK := want[st]
			if ok != wantOK {
				t.Fatalf("ok: got=%v want=%v", ok, wantOK)
			}
			if !ok {
				return
			}
			if it.Kind != wk {
				t.Errorf("kind: got=%q want=%q", it.Kind, wk)
			}
			if it.Key != "task:42:e77" {
				t.Errorf("key: got=%q want=%q", it.Key, "task:42:e77")
			}
			if it.Link != "/tasks/42" {
				t.Errorf("link: got=%q want=%q", it.Link, "/tasks/42")
			}
			if wantD := st == task.StateFailed; it.Dismissible != wantD {
				t.Errorf("dismissible: got=%v want=%v", it.Dismissible, wantD)
			}
			if it.SubjectType != "task" || it.SubjectID != 42 {
				t.Errorf("subject: got=%s/%d want=task/42", it.SubjectType, it.SubjectID)
			}
		})
	}

	t.Run("内置工单的 Extra 含工单页链接", func(t *testing.T) {
		r := store.InboxTaskRow{ID: 5, State: "blocked_spec", ExternalKey: "LT-9", TrackerProvider: "internal",
			IssueID: i64p(9), IssueTitle: strp("登录页报错"), EventID: 3}
		it, ok := TaskItem(r, identity)
		if !ok || !hasLink(it, "/issues/9") {
			t.Fatalf("got ok=%v extra=%v, want /issues/9 in extra", ok, it.Extra)
		}
		if !strings.Contains(it.Title, "登录页报错") {
			t.Errorf("title: got=%q want 含工单标题", it.Title)
		}
	})
	t.Run("blocked_dep 的前驱属本人时给前驱链接", func(t *testing.T) {
		r := store.InboxTaskRow{ID: 8, State: "blocked_dep", EventID: 3, PredecessorID: i64p(7),
			Payload: map[string]any{"blocked_by": float64(7)}}
		it, ok := TaskItem(r, identity)
		if !ok || !hasLink(it, "/tasks/7") {
			t.Fatalf("got ok=%v extra=%v, want /tasks/7 in extra", ok, it.Extra)
		}
	})
	t.Run("blocked_dep 的 PredecessorID 为空时不读 payload 拼链接", func(t *testing.T) {
		r := store.InboxTaskRow{ID: 8, State: "blocked_dep", EventID: 3, PredecessorID: nil,
			Payload: map[string]any{"blocked_by": float64(7)}}
		it, ok := TaskItem(r, identity)
		if !ok {
			t.Fatal("got ok=false want true")
		}
		if hasLink(it, "/tasks/7") {
			t.Errorf("extra: got=%v，不应含 /tasks/7", it.Extra)
		}
	})
}

func TestInboxTaskItemReason(t *testing.T) {
	cases := []struct {
		name string
		row  store.InboxTaskRow
		want string
		deny string
	}{
		{
			name: "blocked_spec 优先用 payload 的 question",
			row: store.InboxTaskRow{ID: 1, EventID: 1, State: "blocked_spec", FailureReason: strp("分诊判定不明确"),
				Payload: map[string]any{"question": "要改哪个页面？"}},
			want: "要改哪个页面？",
		},
		{
			name: "blocked_spec 没有 question 时用 failure_reason",
			row:  store.InboxTaskRow{ID: 1, EventID: 1, State: "blocked_spec", FailureReason: strp("复现测试没有先红")},
			want: "复现测试没有先红",
		},
		{
			name: "awaiting_approval 取 payload 的 reason",
			row: store.InboxTaskRow{ID: 1, EventID: 1, State: "awaiting_approval",
				Payload: map[string]any{"reason": "验证已通过，等待确认"}},
			want: "验证已通过，等待确认",
		},
		{
			name: "review_feedback 用固定文案、不带评论原文",
			row: store.InboxTaskRow{ID: 1, EventID: 1, State: "review_feedback",
				Payload: map[string]any{"review": map[string]any{"body": "SECRET-REVIEW-BODY"}, "body": "SECRET-REVIEW-BODY"}},
			want: reviewParkedReason,
			deny: "SECRET-REVIEW-BODY",
		},
		{
			name: "failed 取 failure_reason",
			row:  store.InboxTaskRow{ID: 1, EventID: 1, State: "failed", FailureReason: strp("验证失败：go test 红")},
			want: "验证失败：go test 红",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			it, ok := TaskItem(c.row, identity)
			if !ok {
				t.Fatal("got ok=false want true")
			}
			if it.Reason != c.want {
				t.Errorf("reason: got=%q want=%q", it.Reason, c.want)
			}
			if c.deny != "" && strings.Contains(it.Reason, c.deny) {
				t.Errorf("reason 含不该出现的 %q: got=%q", c.deny, it.Reason)
			}
		})
	}
}

func docWith(mut func(d *prd.Document)) *prd.Document {
	d := &prd.Document{}
	for _, s := range d.Sections() {
		s.Section.Status = prd.StatusConfirmed
	}
	if mut != nil {
		mut(d)
	}
	return d
}

func TestInboxPRDItem(t *testing.T) {
	inferred := docWith(func(d *prd.Document) {
		d.CodeAnalysis.Status = prd.StatusInferred
		d.Risks.Status = prd.StatusInferred
	})
	confirmed := docWith(nil)
	base := store.InboxPRDRow{ID: 11, Round: 4, OriginalInput: "预览阈值写死了\n第二行", ProviderRepo: "acme/demo"}

	cases := []struct {
		name     string
		state    string
		proc     bool
		lastErr  *string
		doc      *prd.Document
		wantOK   bool
		wantKind Kind
		check    func(t *testing.T, it Item)
	}{
		{name: "drafting+inferred+未处理 → prd_confirm 并跳到第一个推断节", state: "drafting", doc: inferred,
			wantOK: true, wantKind: KindPRDConfirm, check: func(t *testing.T, it Item) {
				if !strings.HasSuffix(it.Link, "?section=codeAnalysis") {
					t.Errorf("link: got=%q want 以 ?section=codeAnalysis 结尾", it.Link)
				}
				if it.Key != "prd:11:confirm:r4" {
					t.Errorf("key: got=%q want=prd:11:confirm:r4", it.Key)
				}
				if it.Reason != "2 节待确认" {
					t.Errorf("reason: got=%q want=2 节待确认", it.Reason)
				}
			}},
		{name: "drafting+inferred+处理中 → 无", state: "drafting", proc: true, doc: inferred},
		{name: "drafting+全部 confirmed → 无", state: "drafting", doc: confirmed},
		{name: "awaiting_answers+未处理 → prd_answers", state: "awaiting_answers", doc: confirmed,
			wantOK: true, wantKind: KindPRDAnswers, check: func(t *testing.T, it Item) {
				if it.Key != "prd:11:answers:r4" {
					t.Errorf("key: got=%q want=prd:11:answers:r4", it.Key)
				}
			}},
		{name: "awaiting_answers+处理中 → 无", state: "awaiting_answers", proc: true, doc: confirmed},
		{name: "ready_for_review → prd_review", state: "ready_for_review", doc: confirmed,
			wantOK: true, wantKind: KindPRDReview},
		{name: "drafting+last_error+未处理 → prd_run_failed 带 8 位十六进制后缀", state: "drafting",
			lastErr: strp("agent 超时"), doc: inferred, wantOK: true, wantKind: KindPRDRunFailed,
			check: func(t *testing.T, it Item) {
				if !strings.HasPrefix(it.Key, "prd:11:run_failed:r4:") || len(it.Key) != len("prd:11:run_failed:r4:")+8 {
					t.Errorf("key: got=%q want prd:11:run_failed:r4:<8hex>", it.Key)
				}
				if _, err := ParseKey(it.Key); err != nil {
					t.Errorf("派生出的键应能被 ParseKey 接受: %v", err)
				}
			}},
		{name: "awaiting_answers+last_error → prd_run_failed 优先于 answers", state: "awaiting_answers",
			lastErr: strp("agent 超时"), doc: confirmed, wantOK: true, wantKind: KindPRDRunFailed},
		{name: "approved → 无", state: "approved", doc: inferred},
		{name: "converted → 无", state: "converted", doc: inferred},
		{name: "abandoned → 无", state: "abandoned", doc: inferred},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := base
			r.State, r.Processing, r.LastError = c.state, c.proc, c.lastErr
			it, ok := PRDItem(r, c.doc, identity)
			if ok != c.wantOK {
				t.Fatalf("ok: got=%v want=%v (item=%+v)", ok, c.wantOK, it)
			}
			if !ok {
				return
			}
			if it.Kind != c.wantKind {
				t.Errorf("kind: got=%q want=%q", it.Kind, c.wantKind)
			}
			if it.Dismissible {
				t.Error("PRD 事项不可忽略: got dismissible=true")
			}
			if c.check != nil {
				c.check(t, it)
			}
		})
	}

	t.Run("同一 round 下不同 last_error 的键不同", func(t *testing.T) {
		r := base
		r.State = "drafting"
		r.LastError = strp("原因 A")
		a, okA := PRDItem(r, confirmed, identity)
		r.LastError = strp("原因 B")
		b, okB := PRDItem(r, confirmed, identity)
		if !okA || !okB {
			t.Fatalf("ok: got=%v/%v want true/true", okA, okB)
		}
		if a.Key == b.Key {
			t.Errorf("两次失败原因不同，键却相同: %q", a.Key)
		}
	})
}

func TestInboxParseKey(t *testing.T) {
	good := []struct {
		in      string
		subject string
		id      int64
	}{
		{"task:1:e2", "task", 1},
		{"prd:3:confirm:r0", "prd", 3},
		{"prd:3:run_failed:r2:0a1b2c3d", "prd", 3},
		{"prd:9:answers:r12", "prd", 9},
	}
	for _, c := range good {
		t.Run("合法键 "+c.in+" 解析出主体", func(t *testing.T) {
			k, err := ParseKey(c.in)
			if err != nil {
				t.Fatalf("err: got=%v want nil", err)
			}
			if k.SubjectType != c.subject || k.SubjectID != c.id {
				t.Errorf("subject: got=%s/%d want=%s/%d", k.SubjectType, k.SubjectID, c.subject, c.id)
			}
		})
	}
	bad := []string{
		"", "task:0:e1", "task:1:e", "task:1:e0", "prd:1:bogus:r1", "../etc", "task:1:e2 ",
		" task:1:e2", "task:01:e2", "prd:1:confirm:r1:XYZ12345", "task:99999999999999999999:e1",
		"task:9999999999999999999:e1", strings.Repeat("a", 129),
	}
	for _, in := range bad {
		name := in
		if len(name) > 20 {
			name = name[:20] + "…(" + strconv.Itoa(len(in)) + ")"
		}
		t.Run(fmt.Sprintf("非法键 %q 返回错误", name), func(t *testing.T) {
			if _, err := ParseKey(in); err == nil {
				t.Errorf("err: got=nil want 非 nil")
			}
		})
	}
}

func TestInboxReasonTruncatedByRune(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"300 个汉字", strings.Repeat("失", 300)},
		{"第 200 个字符是 4 字节 emoji", strings.Repeat("失", 199) + "😀" + strings.Repeat("败", 50)},
		{"第 201 个字符是 4 字节 emoji", strings.Repeat("失", 200) + "😀😀"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			it, ok := TaskItem(store.InboxTaskRow{ID: 1, EventID: 1, State: "failed", FailureReason: strp(c.in)}, identity)
			if !ok {
				t.Fatal("got ok=false want true")
			}
			if n := utf8.RuneCountInString(it.Reason); n > maxReasonRunes+1 || n < maxReasonRunes {
				t.Errorf("rune 数: got=%d want %d..%d", n, maxReasonRunes, maxReasonRunes+1)
			}
			if !utf8.ValidString(it.Reason) {
				t.Errorf("结果不是合法 UTF-8: %q", it.Reason)
			}
		})
	}
}

func TestInboxReasonRedacted(t *testing.T) {
	const known = "Zq7wPx3Lm9KdR2vT" // 登记的已知凭据：不符合任何形态
	red := func(s string) string { return runner.RedactText(s, []string{known}) }

	ghp := "ghp_" + strings.Repeat("Ab3", 12)      // 36 个字母数字
	lin := "lin_api_" + strings.Repeat("Q9x1", 10) // 40 个字母数字
	injections := map[string]struct{ full, secret string }{
		"形态层 ghp_":     {ghp, strings.TrimPrefix(ghp, "ghp_")},
		"形态层 lin_api_": {lin, strings.TrimPrefix(lin, "lin_api_")},
		"权威层已知值":       {known, known},
	}

	type source struct {
		name  string
		build func(text string) (Item, bool)
	}
	sources := []source{
		{"task_failed 的 failure_reason", func(s string) (Item, bool) {
			return TaskItem(store.InboxTaskRow{ID: 1, EventID: 1, State: "failed", FailureReason: &s}, red)
		}},
		{"task_blocked_dep 的 failure_reason", func(s string) (Item, bool) {
			return TaskItem(store.InboxTaskRow{ID: 1, EventID: 1, State: "blocked_dep", FailureReason: &s}, red)
		}},
		{"task_blocked_spec 的 question", func(s string) (Item, bool) {
			return TaskItem(store.InboxTaskRow{ID: 1, EventID: 1, State: "blocked_spec",
				Payload: map[string]any{"question": s}}, red)
		}},
		{"task_awaiting_approval 的 reason", func(s string) (Item, bool) {
			return TaskItem(store.InboxTaskRow{ID: 1, EventID: 1, State: "awaiting_approval",
				Payload: map[string]any{"reason": s}}, red)
		}},
		{"prd_run_failed 的 last_error", func(s string) (Item, bool) {
			return PRDItem(store.InboxPRDRow{ID: 1, State: "drafting", LastError: &s}, nil, red)
		}},
	}

	for _, src := range sources {
		for iname, inj := range injections {
			t.Run(src.name+" 注入"+iname+" 后不出现明文", func(t *testing.T) {
				it, ok := src.build("推送失败 token=" + inj.full + " 请检查")
				if !ok {
					t.Fatal("got ok=false want true")
				}
				if strings.Contains(it.Reason, inj.secret) {
					t.Errorf("reason 含明文凭据: got=%q", it.Reason)
				}
			})
		}
	}

	t.Run("token 跨越截断边界时先脱敏后截断", func(t *testing.T) {
		for iname, inj := range injections {
			text := strings.Repeat("失", 189) + " " + inj.full + " 尾巴"
			it, ok := TaskItem(store.InboxTaskRow{ID: 1, EventID: 1, State: "failed", FailureReason: &text}, red)
			if !ok {
				t.Fatal("got ok=false want true")
			}
			// 前缀（ghp_/lin_api_）按形态表设计保留，查的是去掉前缀后的凭据本体
			for i := 0; i+8 <= len(inj.secret); i++ {
				if sub := inj.secret[i : i+8]; strings.Contains(it.Reason, sub) {
					t.Errorf("%s: reason 含 token 的 8 字符子串 %q: got=%q", iname, sub, it.Reason)
					break
				}
			}
		}
	})
}
