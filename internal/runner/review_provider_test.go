package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/task"
	"github.com/zichuanwangcloud-gif/lathe/internal/vcs"
)

// AC-17：review 事件的代码平台是必填字段。空串或未知值 fail-closed 且可见
// （返回错误，不猜 github、也不悄悄 ignored）；填了 github 的同一事件照常回流。
func TestAcceptReviewRequiresProvider(t *testing.T) {
	cases := []struct {
		name       string
		provider   vcs.Provider
		wantErr    bool
		wantStatus ReviewStatus
	}{
		{"缺平台拒绝", "", true, ""},
		{"未知平台拒绝", "bitbucket", true, ""},
		{"github 照常回流", vcs.ProviderGitHub, false, ReviewQueued},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, m, taskID, repo, src := pipelineFixture(t)
			prOpenTask(t, nil, m, taskID, src, repo, true)
			owner := ownerOf(t, m, taskID)
			before := len(retryEvents(t, m, taskID))

			ev := inlineEvent("inline:p1", "alice", "请改")
			ev.Provider = tc.provider
			out, err := (&ReviewIntake{Tasks: m}).AcceptReview(context.Background(), owner, ev, "deliv-p")

			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "代码平台") {
					t.Fatalf("err = %v（out=%+v），want 含「代码平台」的错误", err, out)
				}
				tk, _ := m.Get(context.Background(), taskID)
				if tk.State != task.StatePROpen {
					t.Errorf("state = %s，want pr_open（不应被推动）", tk.State)
				}
				if got := len(retryEvents(t, m, taskID)); got != before {
					t.Errorf("事件数 = %d，want %d（不应新增事件）", got, before)
				}
				return
			}
			if err != nil || out.Status != tc.wantStatus {
				t.Fatalf("out = %+v err = %v，want status %s", out, err, tc.wantStatus)
			}
		})
	}
}
