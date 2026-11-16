package httpapi

// notify.go 站外通知渠道（Slack / 飞书）的个人设置接口（docs/features/notify-channels.md）。
//
// 全部挂在 CurrentUser 上：渠道是每用户设置，主键是 (user_id, kind)，路径里没有
// 别人的 ID 可猜；投递按 `WHERE d.id=$1 AND c.user_id=$2` 查，零行即 404（不是 403）。
//
// 错误串（testError / lastError）原样回库里的值 —— 它们只来自 notify.Result.Reason，
// 那一道已经保证不含 URL；这里不另做加工，也绝不把请求里的 URL 拼进任何响应。

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/notify"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// NotifyAPI 管理站外通知渠道与投递记录。
type NotifyAPI struct {
	Channels *store.NotifyChannels
	Sender   *notify.Sender
	Auth     *Auth
	// Now 可注入（nil 取 time.Now）。
	Now func() time.Time
}

// Routes 注册通知渠道接口。
func (a *NotifyAPI) Routes(mux *http.ServeMux) {
	mux.Handle("GET /api/notify/channels", a.Auth.RequireFunc(a.list))
	mux.Handle("PUT /api/notify/channels/{kind}", a.Auth.RequireFunc(a.save))
	mux.Handle("POST /api/notify/channels/{kind}/test", a.Auth.RequireFunc(a.test))
	mux.Handle("DELETE /api/notify/channels/{kind}", a.Auth.RequireFunc(a.remove))
	mux.Handle("GET /api/notify/deliveries", a.Auth.RequireFunc(a.deliveries))
	mux.Handle("POST /api/notify/deliveries/{id}/retry", a.Auth.RequireFunc(a.retry))
}

func (a *NotifyAPI) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// kindOf 取路径里的渠道种类。非法 → 400：渠道种类不是资源存在性问题。
func kindOf(w http.ResponseWriter, r *http.Request) (string, bool) {
	kind := r.PathValue("kind")
	if !notify.ValidKind(kind) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未知通知渠道"})
		return "", false
	}
	return kind, true
}

// eventLabels 是界面勾选框的文案：只来自 notify.EventLabel，前端不硬编码中文。
func eventLabels() map[string]string {
	return map[string]string{
		"failed":           notify.EventLabel(notify.EventFailed),
		"blockedSpec":      notify.EventLabel(notify.EventBlockedSpec),
		"awaitingApproval": notify.EventLabel(notify.EventAwaitingApproval),
		"prOpen":           notify.EventLabel(notify.EventPROpen),
		"prdReady":         notify.EventLabel(notify.EventPRDReady),
	}
}

func (a *NotifyAPI) list(w http.ResponseWriter, r *http.Request) {
	views, err := a.Channels.ListChannels(r.Context(), CurrentUser(r).ID)
	if err != nil {
		serverError(w, "查询通知渠道失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": views, "eventLabels": eventLabels()})
}

// TestResult 是一次测试消息的结论。
type TestResult struct {
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	DeliveryID int64  `json:"deliveryId"`
}

// save 部分更新：URL/密钥留空即保留原密文；enabled 与 events 五键必填（整体替换语义，
// 缺省若按零值 false 处理，少传一个键就会静默关掉一类通知）。
// 只有凭据变了才在请求内同步发一条测试消息。
func (a *NotifyAPI) save(w http.ResponseWriter, r *http.Request) {
	kind, ok := kindOf(w, r)
	if !ok {
		return
	}
	var body struct {
		URL         string `json:"url"`
		Secret      string `json:"secret"`
		ClearSecret bool   `json:"clearSecret"`
		Enabled     *bool  `json:"enabled"`
		Events      *struct {
			Failed           *bool `json:"failed"`
			BlockedSpec      *bool `json:"blockedSpec"`
			AwaitingApproval *bool `json:"awaitingApproval"`
			PROpen           *bool `json:"prOpen"`
			PRDReady         *bool `json:"prdReady"`
		} `json:"events"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	missing := func(name string) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少开关字段：" + name})
	}
	if body.Enabled == nil {
		missing("enabled")
		return
	}
	if body.Events == nil {
		missing("events")
		return
	}
	ev := body.Events
	for _, f := range []struct {
		name string
		v    *bool
	}{
		{"failed", ev.Failed}, {"blockedSpec", ev.BlockedSpec}, {"awaitingApproval", ev.AwaitingApproval},
		{"prOpen", ev.PROpen}, {"prdReady", ev.PRDReady},
	} {
		if f.v == nil {
			missing("events." + f.name)
			return
		}
	}

	userID := CurrentUser(r).ID
	changed, err := a.Channels.SaveChannel(r.Context(), userID, store.SaveChannelParams{
		Kind: kind, URL: strings.TrimSpace(body.URL), Secret: strings.TrimSpace(body.Secret),
		ClearSecret: body.ClearSecret, Enabled: *body.Enabled,
		Events: notify.EventSwitches{
			Failed: *ev.Failed, BlockedSpec: *ev.BlockedSpec, AwaitingApproval: *ev.AwaitingApproval,
			PROpen: *ev.PROpen, PRDReady: *ev.PRDReady,
		},
	})
	if err != nil {
		if msg, bad := saveErrorMessage(err); bad {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg})
			return
		}
		serverError(w, "保存通知渠道失败", err)
		return
	}
	if !changed {
		writeJSON(w, http.StatusOK, map[string]any{"saved": true, "test": nil})
		return
	}
	res, err := a.sendTest(r.Context(), userID, kind)
	if err != nil {
		serverError(w, "发送测试消息失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"saved": true, "test": res})
}

// saveErrorMessage 把输入类错误翻成给人看的固定文案（**不回显入参**：URL 本身就是凭据）。
func saveErrorMessage(err error) (string, bool) {
	switch {
	case errors.Is(err, store.ErrUnknownChannelKind):
		return "未知通知渠道", true
	case errors.Is(err, store.ErrChannelURLRequired):
		return "首次配置需要填写 webhook URL", true
	case errors.Is(err, store.ErrSecretConflict):
		return "secret 与 clearSecret 不能同时给", true
	case errors.Is(err, store.ErrSlackNoSecret):
		return "Slack 渠道没有签名密钥", true
	case errors.Is(err, notify.ErrURLUnparsable), errors.Is(err, notify.ErrURLNotHTTPS), errors.Is(err, notify.ErrURLHostNotAllowed):
		return err.Error() + "（只接受 https://hooks.slack.com、https://open.feishu.cn、https://open.larksuite.com）", true
	}
	return "", false
}

// sendTest 是测试消息的唯一路径（PUT 与 /test 共用）：
// BeginTest 直接写成 sending → 请求内同步发一次（可重试错误也不重试，人就在页面前）→
// FinishTest 一个事务里写投递行与渠道的 test_* 缓存。绝不交给 worker。
func (a *NotifyAPI) sendTest(ctx context.Context, userID int64, kind string) (*TestResult, error) {
	d, ch, err := a.Channels.BeginTest(ctx, userID, kind, a.now())
	if err != nil {
		return nil, err
	}
	title, text := notify.Render(notify.EventTest, notify.FactsFromPayload(notify.EventTest, d.Payload, ""))
	res := a.Sender.Send(ctx, *ch, notify.Message{Title: title, Text: text})
	// 请求被取消也要把行收尾，不让它停在 sending 等租约回收。
	if err := a.Channels.FinishTest(context.WithoutCancel(ctx), d, a.now(), res.OK, res.Reason); err != nil {
		return nil, err
	}
	return &TestResult{OK: res.OK, Error: res.Reason, DeliveryID: d.ID}, nil
}

func (a *NotifyAPI) test(w http.ResponseWriter, r *http.Request) {
	kind, ok := kindOf(w, r)
	if !ok {
		return
	}
	res, err := a.sendTest(r.Context(), CurrentUser(r).ID, kind)
	if errors.Is(err, store.ErrChannelNotFound) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "该渠道尚未配置"})
		return
	}
	if err != nil {
		serverError(w, "发送测试消息失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"test": res})
}

func (a *NotifyAPI) remove(w http.ResponseWriter, r *http.Request) {
	kind, ok := kindOf(w, r)
	if !ok {
		return
	}
	deleted, err := a.Channels.DeleteChannel(r.Context(), CurrentUser(r).ID, kind)
	if err != nil {
		serverError(w, "删除通知渠道失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

func (a *NotifyAPI) deliveries(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := a.Channels.ListDeliveries(r.Context(), CurrentUser(r).ID, limit)
	if err != nil {
		serverError(w, "查询投递记录失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deliveries": items})
}

// retry 把非 test 的 gave_up 行改回 pending。非属主/不存在一律 404；test 行或非 gave_up → 409。
func (a *NotifyAPI) retry(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "投递记录不存在"})
		return
	}
	err = a.Channels.RetryDelivery(r.Context(), CurrentUser(r).ID, id, a.now())
	switch {
	case errors.Is(err, store.ErrDeliveryNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "投递记录不存在"})
	case errors.Is(err, store.ErrTestNotRetryable):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "测试消息请在渠道卡片上点「重发测试」"})
	case errors.Is(err, store.ErrNotRetryable):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "只有已放弃（gave_up）的投递能重试"})
	case err != nil:
		serverError(w, "重试投递失败", err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"retried": true})
	}
}
