package prd

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/notify"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// 编译期：真实现满足窄接口。
var _ ReviewNotifier = (*store.NotifyChannels)(nil)

type captureReview struct {
	got  []notify.PRDEvent
	err  error
	boom bool
}

func (c *captureReview) EnqueuePRDEvent(_ context.Context, ev notify.PRDEvent) (int, error) {
	c.got = append(c.got, ev)
	if c.boom {
		panic("review notifier 炸了")
	}
	return 1, c.err
}

// submittableStore 造一份能过自检的 PRD（复用 checklist_test 的 okDoc）。
func submittableStore(t *testing.T) *fakeStore {
	t.Helper()
	st := newFakeStore()
	d, blocks := okDoc()
	var err error
	if st.row.Document, err = json.Marshal(d); err != nil {
		t.Fatal(err)
	}
	if st.row.TaskBlocks, err = json.Marshal(blocks); err != nil {
		t.Fatal(err)
	}
	st.row.ReviewReport = json.RawMessage(`{"findings":[],"verdict":"攻不动"}`)
	return st
}

// AC-22：SubmitForReview 转移成功后恰好发一个 PRDEvent；没转移就不发；
// notifier 出错/panic 不影响提交结果。
func TestPRDSubmitForReviewEmitsReadyForReview(t *testing.T) {
	want := notify.PRDEvent{PRDID: 1, UserID: 7, ToState: "ready_for_review", EventType: notify.EventPRDReady,
		At: time.Unix(1700000000, 0), Repo: "acme/lathe"}

	t.Run("给定自检通过 断言收到 1 个事件且键逐字相等", func(t *testing.T) {
		st := submittableStore(t)
		rn := &captureReview{}
		svc := newService(st, &fakeAgent{}, &fakeWorktrees{})
		svc.ReviewNotifier = rn
		if _, err := svc.SubmitForReview(context.Background(), 1, 7); err != nil {
			t.Fatalf("SubmitForReview: %v", err)
		}
		if len(rn.got) != 1 {
			t.Fatalf("事件数 got %d, want 1", len(rn.got))
		}
		if got := rn.got[0]; got != want {
			t.Errorf("event got %+v, want %+v", got, want)
		}
		if got, wantKey := notify.PRDEventKey(rn.got[0]), "prd:1:ready_for_review:2023-11-14T22:13:20Z"; got != wantKey {
			t.Errorf("key got %q, want %q", got, wantKey)
		}
	})
	t.Run("给定自检不过 断言 0 个事件", func(t *testing.T) {
		st := newFakeStore()
		st.row.Document = json.RawMessage(reviewableDoc)
		rn := &captureReview{}
		svc := newService(st, &fakeAgent{}, &fakeWorktrees{})
		svc.ReviewNotifier = rn
		if _, err := svc.SubmitForReview(context.Background(), 1, 7); err == nil {
			t.Fatal("自检应不过")
		}
		if len(rn.got) != 0 {
			t.Errorf("事件数 got %d, want 0", len(rn.got))
		}
	})
	for _, c := range []struct {
		name string
		rn   *captureReview
	}{
		{"notifier 返回 error", &captureReview{err: errors.New("db down")}},
		{"notifier panic", &captureReview{boom: true}},
	} {
		t.Run("给定 "+c.name+" 断言提交仍成功且状态已转", func(t *testing.T) {
			st := submittableStore(t)
			svc := newService(st, &fakeAgent{}, &fakeWorktrees{})
			svc.ReviewNotifier = c.rn
			rep, err := svc.SubmitForReview(context.Background(), 1, 7)
			if err != nil || rep == nil {
				t.Fatalf("got (%v, %v), want (check, nil)", rep, err)
			}
			if st.row.State != string(StateReadyForReview) {
				t.Errorf("state got %q, want ready_for_review", st.row.State)
			}
		})
	}
}
