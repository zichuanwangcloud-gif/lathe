package httpapi

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// labelResolver 给出带触发标签配置的投递目标。
type labelResolver struct{ label string }

func (l labelResolver) Resolve(ctx context.Context, slug string) (*WebhookTarget, error) {
	return &WebhookTarget{
		OwnerID: 1, Secret: testSecret, LinearUserID: "user-me", TriggerLabel: l.label,
	}, nil
}

// 打上触发标签就接单，即使指派人不是绑定用户。
//
// 这是给「不想改指派人、只想让平台跑一下」这种用法留的入口。
func TestWebhookQueuesOnTriggerLabel(t *testing.T) {
	q := &fakeEnqueuer{}
	h := &LinearWebhook{Resolver: labelResolver{label: "lathe:go"}, Deliveries: newFakeClaimer(), Tasks: q}

	// 指派给别人，但这次改的是标签，且带着触发标签
	body := `{"action":"update","type":"Issue","data":{"id":"uuid-9","identifier":"CR-900",` +
		`"assigneeId":"someone-else","labels":[{"id":"l1","name":"lathe:go"}]},` +
		`"updatedFrom":{"labelIds":null}}`

	resp := post(t, h, body)
	if resp.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200：%s", resp.Code, resp.Body.String())
	}
	if len(q.issues) != 1 || q.issues[0] != "CR-900" {
		t.Errorf("应按标签接单 CR-900，实际 %v", q.issues)
	}
}

// ★ AC3：标签名未配置时，带标签的事件也不接单 ——
// 行为与本项之前逐字节一致。
//
// 不能让存量部署因为升级就突然开始按标签接单：某个仓库可能早就在用
// lathe:go 这个标签表示别的意思。
func TestWebhookLabelTriggerOffWhenUnconfigured(t *testing.T) {
	q := &fakeEnqueuer{}
	h := &LinearWebhook{Resolver: labelResolver{label: ""}, Deliveries: newFakeClaimer(), Tasks: q}

	body := `{"action":"update","type":"Issue","data":{"id":"uuid-9","identifier":"CR-901",` +
		`"assigneeId":"someone-else","labels":[{"id":"l1","name":"lathe:go"}]},` +
		`"updatedFrom":{"labelIds":null}}`

	resp := post(t, h, body)
	if resp.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.Code)
	}
	if len(q.issues) != 0 {
		t.Errorf("AC3：未配置标签名时不该接单，实际接了 %v", q.issues)
	}
}

// issue 被取消 → 联动取消在途任务。
//
// 关键点：取消分流必须在接单闸门【之前】—— 取消事件不是指派事件，
// 走到那个闸门会被当成「非指派事件」直接 ignored 掉。
func TestWebhookCancelsTasksOnIssueCancelled(t *testing.T) {
	q := &fakeEnqueuer{cancelIDs: []int64{77, 78}}
	h := &LinearWebhook{Resolver: fakeResolver{}, Deliveries: newFakeClaimer(), Tasks: q}

	// 注意：指派人不是绑定用户 —— 取消不该依赖指派关系
	body := `{"action":"update","type":"Issue","data":{"id":"uuid-c","identifier":"CR-CANCEL",` +
		`"assigneeId":"someone-else","state":{"name":"Cancelled","type":"canceled"}},` +
		`"updatedFrom":{"stateId":"old-state"}}`

	resp := post(t, h, body)
	if resp.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200：%s", resp.Code, resp.Body.String())
	}
	if len(q.cancelledIssues) != 1 || q.cancelledIssues[0] != "CR-CANCEL" {
		t.Errorf("应联动取消 CR-CANCEL，实际 %v", q.cancelledIssues)
	}
	// 取消不该顺手接单
	if len(q.issues) != 0 {
		t.Errorf("取消事件不该接单，实际 %v", q.issues)
	}
}

// issue 被删也算取消：目标都不存在了，在途任务没有意义。
func TestWebhookCancelsOnIssueRemoved(t *testing.T) {
	q := &fakeEnqueuer{}
	h := &LinearWebhook{Resolver: fakeResolver{}, Deliveries: newFakeClaimer(), Tasks: q}

	body := `{"action":"remove","type":"Issue","data":{"id":"uuid-r","identifier":"CR-GONE"}}`
	resp := post(t, h, body)
	if resp.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.Code)
	}
	if len(q.cancelledIssues) != 1 {
		t.Errorf("issue 被删应联动取消，实际 %v", q.cancelledIssues)
	}
}

// completed 刻意不算取消：人手工标完成不代表平台任务该作废 ——
// 那个任务可能正在跑，或已开出 PR 等人合并。
func TestWebhookDoesNotCancelOnCompleted(t *testing.T) {
	q := &fakeEnqueuer{}
	h := &LinearWebhook{Resolver: fakeResolver{}, Deliveries: newFakeClaimer(), Tasks: q}

	body := `{"action":"update","type":"Issue","data":{"id":"uuid-d","identifier":"CR-DONE",` +
		`"assigneeId":"someone-else","state":{"name":"Done","type":"completed"}},` +
		`"updatedFrom":{"stateId":"old"}}`

	resp := post(t, h, body)
	if resp.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", resp.Code)
	}
	if len(q.cancelledIssues) != 0 {
		t.Errorf("completed 不该触发取消，实际 %v", q.cancelledIssues)
	}
}

// 取消联动失败返回 200（已登记去重，重投不会再处理），错误落投递记录。
func TestWebhookCancelErrorRecorded(t *testing.T) {
	q := &fakeEnqueuer{cancelErr: errors.New("数据库炸了")}
	cl := newFakeClaimer()
	h := &LinearWebhook{Resolver: fakeResolver{}, Deliveries: cl, Tasks: q}

	body := `{"action":"update","type":"Issue","data":{"id":"uuid-e","identifier":"CR-ERR",` +
		`"state":{"name":"Cancelled","type":"canceled"}},"updatedFrom":{"stateId":"old"}}`

	resp := post(t, h, body)
	if resp.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（已去重，重投无意义）", resp.Code)
	}
	if cl.finished["deliv-1"] == "" {
		t.Errorf("失败原因应落进投递记录，实际 %q", cl.finished["deliv-1"])
	}
}

// ★ AC5：验签失败一律拒绝 —— 本项不得在 ingress 上开任何新的免验签路径。
//
// 取消分流被放在了验签【之后】，所以伪造的取消事件同样进不来。
func TestWebhookCancelStillRequiresSignature(t *testing.T) {
	q := &fakeEnqueuer{}
	h := &LinearWebhook{Resolver: fakeResolver{}, Deliveries: newFakeClaimer(), Tasks: q}

	body := `{"action":"update","type":"Issue","data":{"id":"uuid-f","identifier":"CR-FAKE",` +
		`"state":{"name":"Cancelled","type":"canceled"}},"updatedFrom":{"stateId":"old"}}`

	resp := post(t, h, body, func(r *http.Request) {
		r.Header.Set("linear-signature", "deadbeef")
	})
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("伪造签名的取消事件应被拒绝，得到 %d", resp.Code)
	}
	if len(q.cancelledIssues) != 0 {
		t.Errorf("AC5：验签失败却执行了取消！%v", q.cancelledIssues)
	}
}
