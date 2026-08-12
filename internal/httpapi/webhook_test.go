package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Clouditera/lathe/internal/integration/linear"
)

type fakeClaimer struct {
	claimed  map[string]bool
	finished map[string]string
	err      error
}

func newFakeClaimer() *fakeClaimer {
	return &fakeClaimer{claimed: map[string]bool{}, finished: map[string]string{}}
}

func (f *fakeClaimer) ClaimDelivery(ctx context.Context, id, source string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	if f.claimed[id] {
		return false, nil
	}
	f.claimed[id] = true
	return true, nil
}

func (f *fakeClaimer) FinishDelivery(ctx context.Context, id, errMsg string) error {
	f.finished[id] = errMsg
	return nil
}

type fakeEnqueuer struct {
	issues []string
	err    error
}

func (f *fakeEnqueuer) Enqueue(ctx context.Context, issueID, issueKey string) error {
	if f.err != nil {
		return f.err
	}
	f.issues = append(f.issues, issueKey)
	return nil
}

const testSecret = "webhook-secret"

func post(t *testing.T, h http.Handler, body string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/linear", strings.NewReader(body))

	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(body))
	req.Header.Set(linear.HeaderSignature, hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set(linear.HeaderDelivery, "deliv-1")

	for _, o := range opts {
		o(req)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func newHandler(c *fakeClaimer, e *fakeEnqueuer) *LinearWebhook {
	return &LinearWebhook{Secret: testSecret, UserID: "user-me", Deliveries: c, Tasks: e}
}

const assignedBody = `{"action":"update","type":"Issue","data":{"id":"uuid-1","identifier":"CR-777","assigneeId":"user-me"},"updatedFrom":{"assigneeId":null}}`

func TestWebhookQueuesAssignedIssue(t *testing.T) {
	c, e := newFakeClaimer(), &fakeEnqueuer{}
	rec := post(t, newHandler(c, e), assignedBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "queued") {
		t.Errorf("响应应表明已排队: %s", rec.Body)
	}
	if len(e.issues) != 1 || e.issues[0] != "CR-777" {
		t.Errorf("应排队 CR-777，实际 %v", e.issues)
	}
	if c.finished["deliv-1"] != "" {
		t.Errorf("成功处理不应记录错误: %q", c.finished["deliv-1"])
	}
}

// ★重投递必须被幂等挡住，否则同一 issue 会被重复接单。
func TestWebhookDuplicateDelivery(t *testing.T) {
	c, e := newFakeClaimer(), &fakeEnqueuer{}
	h := newHandler(c, e)

	first := post(t, h, assignedBody)
	second := post(t, h, assignedBody)

	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("两次都应 200，得到 %d / %d", first.Code, second.Code)
	}
	if !strings.Contains(second.Body.String(), "duplicate") {
		t.Errorf("第二次应被识别为重投递: %s", second.Body)
	}
	if len(e.issues) != 1 {
		t.Errorf("重投递不应重复排队，实际排了 %d 次: %v", len(e.issues), e.issues)
	}
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	c, e := newFakeClaimer(), &fakeEnqueuer{}
	rec := post(t, newHandler(c, e), assignedBody, func(r *http.Request) {
		r.Header.Set(linear.HeaderSignature, "deadbeef")
	})

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("坏签名应返回 401，得到 %d", rec.Code)
	}
	if len(e.issues) != 0 {
		t.Error("验签失败不应排队")
	}
	if len(c.claimed) != 0 {
		t.Error("验签失败不应登记投递 —— 否则攻击者可用伪造请求占掉 delivery ID")
	}
}

func TestWebhookRequiresDeliveryHeader(t *testing.T) {
	c, e := newFakeClaimer(), &fakeEnqueuer{}
	rec := post(t, newHandler(c, e), assignedBody, func(r *http.Request) {
		r.Header.Del(linear.HeaderDelivery)
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("缺 delivery 头应返回 400，得到 %d", rec.Code)
	}
}

// 只改标题这类事件应被确认后忽略，不接单。
func TestWebhookIgnoresNonAssignment(t *testing.T) {
	c, e := newFakeClaimer(), &fakeEnqueuer{}
	body := `{"action":"update","type":"Issue","data":{"id":"uuid-1","identifier":"CR-777","assigneeId":"user-me"},"updatedFrom":{"title":"旧标题"}}`

	rec := post(t, newHandler(c, e), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ignored") {
		t.Errorf("应被忽略: %s", rec.Body)
	}
	if len(e.issues) != 0 {
		t.Errorf("只改标题不应接单，实际 %v", e.issues)
	}
}

func TestWebhookIgnoresOtherUsersIssue(t *testing.T) {
	c, e := newFakeClaimer(), &fakeEnqueuer{}
	body := `{"action":"create","type":"Issue","data":{"id":"u","identifier":"CR-1","assigneeId":"someone-else"}}`

	rec := post(t, newHandler(c, e), body)
	if rec.Code != http.StatusOK || len(e.issues) != 0 {
		t.Errorf("指派给别人的 issue 不应接单: code=%d issues=%v", rec.Code, e.issues)
	}
}

// 去重登记失败应返回 5xx 让 Linear 重投 —— 此时还没处理业务，重投是安全的。
func TestWebhookClaimErrorReturns500(t *testing.T) {
	c, e := newFakeClaimer(), &fakeEnqueuer{}
	c.err = errors.New("数据库挂了")

	rec := post(t, newHandler(c, e), assignedBody)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("登记失败应返回 500 触发重投，得到 %d", rec.Code)
	}
}

// 排队失败已登记去重，重投也不会再处理，因此返回 200 并把原因落库。
func TestWebhookEnqueueErrorRecorded(t *testing.T) {
	c, e := newFakeClaimer(), &fakeEnqueuer{}
	e.err = errors.New("仓库未配置")

	rec := post(t, newHandler(c, e), assignedBody)
	if rec.Code != http.StatusOK {
		t.Errorf("状态码 = %d，期望 200（重投无意义）", rec.Code)
	}
	if got := c.finished["deliv-1"]; !strings.Contains(got, "仓库未配置") {
		t.Errorf("失败原因应落库，得到 %q", got)
	}
}

func TestWebhookRejectsNonPost(t *testing.T) {
	c, e := newFakeClaimer(), &fakeEnqueuer{}
	req := httptest.NewRequest(http.MethodGet, "/webhooks/linear", nil)
	rec := httptest.NewRecorder()
	newHandler(c, e).ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET 应返回 405，得到 %d", rec.Code)
	}
}

func TestHealth(t *testing.T) {
	rec := httptest.NewRecorder()
	Health(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("健康检查异常: %d %s", rec.Code, rec.Body)
	}
}
