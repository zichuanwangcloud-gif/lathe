package yunxiao

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/tracker"
)

const workflowBody = `{
  "id": "wf-1", "name": "缺陷流程", "defaultStatusId": "100005",
  "statuses": [
    {"id": "100005", "name": "待处理", "displayName": "待处理", "nameEn": "To do"},
    {"id": "100010", "name": "processing", "displayName": "处理中", "nameEn": "In progress"},
    {"id": "100014", "name": "已修复", "displayName": "已修复", "nameEn": "Fixed"},
    {"id": "100085", "name": "已关闭", "displayName": "已关闭", "nameEn": "Closed"}
  ]
}`

// syncFake 返回工作项（当前状态 currentStatusID）、工作流，并记录 PUT。
func syncFake(t *testing.T, currentStatusID string) (*Client, *recorder) {
	t.Helper()
	srv, rec := fakeYunxiao(t, func(w http.ResponseWriter, r *http.Request, got recorded) {
		switch {
		case strings.HasSuffix(got.Path, "/workflow"):
			_, _ = io.WriteString(w, workflowBody)
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusOK)
		default:
			_, _ = io.WriteString(w, `{"id":"WI","serialNumber":"S-1","subject":"x","status":{"id":"`+currentStatusID+`","name":"n","displayName":"d"}}`)
		}
	})
	return testClient(t, srv, "ORG"), rec
}

func puts(rec *recorder) []recorded {
	var out []recorded
	for _, r := range rec.all() {
		if r.Method == http.MethodPut {
			out = append(out, r)
		}
	}
	return out
}

// AC-15
func TestTrackerSyncStatus(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		mapping StatusMapping
		ev      tracker.Lifecycle
		current string
		wantID  string // 空 = 不应有 PUT
		wantErr error
		noReq   bool
	}{
		{"match_display_name", StatusMapping{Started: "处理中"}, tracker.LifecycleStarted, "100005", "100010", nil, false},
		{"match_name_case_insensitive", StatusMapping{PROpen: "  PROCESSING "}, tracker.LifecyclePROpen, "100005", "100010", nil, false},
		{"match_name_en", StatusMapping{Merged: "fixed"}, tracker.LifecycleMerged, "100005", "100014", nil, false},
		{"empty_mapping_no_request", StatusMapping{}, tracker.LifecycleStarted, "100005", "", nil, true},
		{"not_found_no_put", StatusMapping{Started: "不存在的状态"}, tracker.LifecycleStarted, "100005", "", ErrStatusNotFound, false},
		{"already_in_status_no_put", StatusMapping{Started: "处理中"}, tracker.LifecycleStarted, "100010", "", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cl, rec := syncFake(t, c.current)
			err := NewTracker(cl, c.mapping).SyncStatus(ctx, "WI", c.ev)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Errorf("err got %v want %v", err, c.wantErr)
				}
			} else if err != nil {
				t.Errorf("err got %v want nil", err)
			}
			if c.noReq {
				if n := rec.count.Load(); n != 0 {
					t.Errorf("请求数 got %d want 0", n)
				}
				return
			}
			ps := puts(rec)
			if c.wantID == "" {
				if len(ps) != 0 {
					t.Errorf("PUT 数 got %d want 0: %+v", len(ps), ps)
				}
				return
			}
			if len(ps) != 1 {
				t.Fatalf("PUT 数 got %d want 1", len(ps))
			}
			want, _ := json.Marshal(map[string]string{"status": c.wantID})
			if ps[0].Body != string(want) {
				t.Errorf("PUT body got %s want %s", ps[0].Body, want)
			}
			if ps[0].Path != "/oapi/v1/projex/organizations/ORG/workitems/WI" {
				t.Errorf("PUT path got %q", ps[0].Path)
			}
		})
	}
}

// AC-12 补充：Tracker 实现了可选扩展接口（编译期断言放在测试里，
// 生产代码里另有一份）。
var _ tracker.StatusSyncer = (*Tracker)(nil)
