package github

import (
	"reflect"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/vcs"
)

// AC-2：五个纯数据类型是 vcs 的别名；ReviewEvent 不是。
var (
	_ vcs.Client               = (*Client)(nil)
	_ vcs.ReviewFeedbackSource = (*Client)(nil)
)

func takesVCSParams(p vcs.PRParams) string { return p.ProviderRepo }

func TestGitHubTypesAreVCSAliases(t *testing.T) {
	// 同一类型才能直接作为 vcs.PRParams 实参传入（不同命名类型在这里编译失败）
	if got := takesVCSParams(PRParams{ProviderRepo: "acme/demo"}); got != "acme/demo" {
		t.Fatalf("PRParams 传给 vcs.PRParams 后字段丢失: %q", got)
	}
	pairs := []struct {
		name     string
		got, ref reflect.Type
	}{
		{"PRParams", reflect.TypeOf(PRParams{}), reflect.TypeOf(vcs.PRParams{})},
		{"PullRequest", reflect.TypeOf(PullRequest{}), reflect.TypeOf(vcs.PullRequest{})},
		{"PRInfo", reflect.TypeOf(PRInfo{}), reflect.TypeOf(vcs.PRInfo{})},
		{"ReviewComment", reflect.TypeOf(ReviewComment{}), reflect.TypeOf(vcs.ReviewComment{})},
		{"ReviewFeedback", reflect.TypeOf(ReviewFeedback{}), reflect.TypeOf(vcs.ReviewFeedback{})},
	}
	for _, tc := range pairs {
		if tc.got != tc.ref {
			t.Errorf("%s 应是 vcs.%s 的别名：got %v，want %v", tc.name, tc.name, tc.got, tc.ref)
		}
	}
	if reflect.TypeOf(ReviewEvent{}) == reflect.TypeOf(vcs.ReviewEvent{}) {
		t.Error("github.ReviewEvent 不应是 vcs.ReviewEvent 的别名（信任判定必须留在 github 包）")
	}
	if ErrNoCommits != vcs.ErrNoCommits {
		t.Error("github.ErrNoCommits 应与 vcs.ErrNoCommits 是同一个哨兵")
	}
}

// AC-2b：Actionable 的结果与 cff5332 的期望表逐项相同；VCS() 原样转换。
func TestReviewEventVCSConversionKeepsGitHubTrust(t *testing.T) {
	trusted := map[string]bool{"OWNER": true, "MEMBER": true, "COLLABORATOR": true, "CONTRIBUTOR": true, "owner": true}
	assocs := []string{"OWNER", "MEMBER", "COLLABORATOR", "CONTRIBUTOR", "FIRST_TIME_CONTRIBUTOR", "FIRST_TIMER",
		"MANNEQUIN", "NONE", "", "owner", "UNKNOWN_X"}
	for _, assoc := range assocs {
		for _, self := range []bool{false, true} {
			name := assoc
			if name == "" {
				name = "（缺失）"
			}
			if self {
				name += "/作者本人"
			}
			t.Run(name, func(t *testing.T) {
				ev := &ReviewEvent{
					Event: EventReviewComment, Action: "created", Repo: "acme/demo", PRNumber: 7,
					PRAuthor: "lathe-bot", Key: "inline:9", Author: "alice", AuthorAssociation: assoc,
					AuthorType: "User", Body: "请改这里", Path: "a.go", Line: 3, URL: "https://x/9",
				}
				if self {
					ev.Author = "LATHE-bot"
				}
				want := trusted[assoc] && !self
				if got, reason := ev.Actionable(); got != want {
					t.Errorf("Actionable() = %v（%s），want %v", got, reason, want)
				}
				v := ev.VCS()
				wantV := vcs.ReviewEvent{
					Provider: vcs.ProviderGitHub, Event: ev.Event, Action: ev.Action, Repo: ev.Repo,
					PRNumber: ev.PRNumber, PRAuthor: ev.PRAuthor, Key: ev.Key, Author: ev.Author,
					Body: ev.Body, Path: ev.Path, Line: ev.Line, URL: ev.URL, ReviewState: ev.ReviewState,
				}
				if v == nil || *v != wantV {
					t.Errorf("VCS() = %+v，want %+v", v, wantV)
				}
			})
		}
	}
	var nilEv *ReviewEvent
	if nilEv.VCS() != nil {
		t.Error("nil 事件的 VCS() 应为 nil")
	}
}
