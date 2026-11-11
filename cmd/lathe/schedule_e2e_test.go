package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/httpapi"
	"github.com/zichuanwangcloud-gif/lathe/internal/integration/linear"
	"github.com/zichuanwangcloud-gif/lathe/internal/schedule"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// fixedTarget 把所有投递路由到同一个属主（e2e 只关心接单路径本身）。
type fixedTarget struct{ target httpapi.WebhookTarget }

func (f fixedTarget) Resolve(context.Context, string) (*httpapi.WebhookTarget, error) {
	t := f.target
	return &t, nil
}

// ★ AC-22：定时建出来的单经**真 webhook 处理器 + 真 queue.Enqueue** 变成任务，
// 调度器自己不建任务。假 Linear 收到 issueCreate 后，用它返回的 id/identifier
// 构造带签名的 create webhook 投给 httpapi.LinearWebhook。
func TestScheduledIssueFlowsThroughWebhookIntake(t *testing.T) {
	st := testStore(t)
	userID, _ := fixture(t, st)
	ctx := context.Background()
	nonce := time.Now().UnixNano()
	viewer := fmt.Sprintf("viewer-%d", nonce)
	const secret = "whsec-e2e"

	var createdID, createdKey string
	lin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var q struct {
			Query     string `json:"query"`
			Variables struct {
				Input map[string]any `json:"input"`
			} `json:"variables"`
		}
		_ = json.Unmarshal(raw, &q)
		if !strings.Contains(q.Query, "issueCreate") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		createdID, createdKey = fmt.Sprint(q.Variables.Input["id"]), fmt.Sprintf("SCH-%d", nonce%100000)
		if q.Variables.Input["assigneeId"] != viewer {
			t.Errorf("assigneeId: got %v, want %s", q.Variables.Input["assigneeId"], viewer)
		}
		_, _ = fmt.Fprintf(w, `{"data":{"issueCreate":{"success":true,"issue":{"id":%q,"identifier":%q,"url":"https://linear.app/x/%s"}}}}`,
			createdID, createdKey, createdKey)
	}))
	t.Cleanup(lin.Close)

	// 规则：每日 09:00（UTC），今天 09:00 到期
	slot := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	var sid int64
	if err := st.Pool().QueryRow(ctx, `
		INSERT INTO schedules (user_id, name, enabled_at, kind, minute_of_day, linear_team_id, title_template, next_fire_at, last_slot_at)
		VALUES ($1, $2, $3, 'daily', 540, 'team-1', '每日巡检 {{date}}', $4, $5) RETURNING id`,
		userID, fmt.Sprint("e2e-", nonce), slot.AddDate(0, 0, -7), slot, slot.AddDate(0, 0, -1)).Scan(&sid); err != nil {
		t.Fatalf("建规则失败: %v", err)
	}

	r := &schedule.Runner{
		Store: st, Settings: st, Now: func() time.Time { return slot.Add(20 * time.Second) }, Node: "e2e",
		CatchUpWindow: 6 * time.Hour,
		Linear: func(ctx context.Context, uid int64) (schedule.LinearHandle, error) {
			if uid != userID {
				return schedule.LinearHandle{}, fmt.Errorf("不是本测试的用户")
			}
			c, err := linear.NewClientWithURL("tok", lin.URL)
			return schedule.LinearHandle{Client: c, UserID: viewer, Redact: func(s string) string { return s }}, err
		},
	}
	t.Cleanup(r.Close)
	if _, err := r.TickOnce(ctx); err != nil {
		t.Fatalf("TickOnce: %v", err)
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := r.Drain(dctx); err != nil {
		t.Fatal(err)
	}
	if createdID == "" {
		t.Fatal("假 Linear 没收到 issueCreate")
	}

	// 调度器跑完：该用户 0 条任务（调度器不建任务）
	countTasks := func() int {
		var n int
		if err := st.Pool().QueryRow(ctx, `SELECT count(*) FROM tasks WHERE user_id=$1`, userID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := countTasks(); n != 0 {
		t.Fatalf("调度器不得建任务: got %d, want 0", n)
	}

	// Linear 投回 create webhook（真处理器 + 真 Enqueue）
	hook := &httpapi.LinearWebhook{
		Resolver:   fixedTarget{httpapi.WebhookTarget{OwnerID: userID, Secret: secret, LinearUserID: viewer}},
		Deliveries: st,
		Tasks:      testQueue(st, &fakePipeline{}),
	}
	body, _ := json.Marshal(map[string]any{
		"action": "create", "type": "Issue",
		"data": map[string]any{"id": createdID, "identifier": createdKey, "title": "每日巡检", "assigneeId": viewer},
	})
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/linear/x", bytes.NewReader(body))
	req.Header.Set(linear.HeaderSignature, hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set(linear.HeaderDelivery, fmt.Sprint("e2e-delivery-", nonce))
	rec := httptest.NewRecorder()
	hook.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "queued") {
		t.Fatalf("webhook: got %d %s, want 200 queued", rec.Code, rec.Body.String())
	}

	var taskID int64
	var state string
	if err := st.Pool().QueryRow(ctx, `SELECT id, state FROM tasks WHERE user_id=$1 AND external_id=$2`, userID, createdID).Scan(&taskID, &state); err != nil {
		t.Fatalf("应恰好有一条 external_id=%s 的任务: %v", createdID, err)
	}
	if n := countTasks(); n != 1 || state != "queued" {
		t.Errorf("任务: got count=%d state=%s, want 1/queued", n, state)
	}

	// runs API 派生出这个任务
	api := &httpapi.SchedulesAPI{Store: st, Auth: httpapi.NewAuth("e2e-token")}
	api.Auth.Bootstrap = &store.User{ID: userID, Email: "e2e@example.com", Role: store.RoleMember}
	mux := http.NewServeMux()
	api.Routes(mux)
	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/schedules/%d/runs", sid), nil)
	req.Header.Set("Authorization", "Bearer e2e-token")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var v struct {
		Runs []struct {
			Status string `json:"status"`
			Task   *struct {
				ID int64 `json:"id"`
			} `json:"task"`
		} `json:"runs"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if len(v.Runs) != 1 || v.Runs[0].Status != "created" || v.Runs[0].Task == nil || v.Runs[0].Task.ID != taskID {
		t.Errorf("runs API: got %s, want 1 行 created 且 task.id=%d", rec.Body.String(), taskID)
	}
}
