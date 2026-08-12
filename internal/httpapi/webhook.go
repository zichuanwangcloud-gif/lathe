// Package httpapi 提供 Lathe 控制面的 HTTP 接口。
package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/Clouditera/lathe/internal/integration/linear"
)

// maxWebhookBody 限制 webhook 载荷大小，防止被打爆内存。
const maxWebhookBody = 4 << 20 // 4MB

// DeliveryClaimer 提供 webhook 幂等去重。
type DeliveryClaimer interface {
	ClaimDelivery(ctx context.Context, deliveryID, source string) (bool, error)
	FinishDelivery(ctx context.Context, deliveryID, errMsg string) error
}

// TaskEnqueuer 把一个已确认要处理的 issue 排进执行队列。
type TaskEnqueuer interface {
	Enqueue(ctx context.Context, issueID, issueKey string) error
}

// LinearWebhook 处理来自 Linear 的 webhook。
type LinearWebhook struct {
	// SecretFunc 取签名密钥，UserIDFunc 取接单判定用的 Linear 用户 ID。
	//
	// 用函数而非固定字符串：凭据可在设置页里随时修改，
	// 每次请求现取才能让修改即刻生效，无需重启。
	SecretFunc func() string
	UserIDFunc func() string

	Deliveries DeliveryClaimer
	Tasks      TaskEnqueuer
}

func (h *LinearWebhook) secret() string {
	if h.SecretFunc == nil {
		return ""
	}
	return h.SecretFunc()
}

func (h *LinearWebhook) userID() string {
	if h.UserIDFunc == nil {
		return ""
	}
	return h.UserIDFunc()
}

// ServeHTTP 实现 http.Handler。
//
// 处理次序刻意如此：验签 → 幂等登记 → 业务判断。
// 幂等登记必须在业务处理之前，否则重投递会重复建任务。
func (h *LinearWebhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只接受 POST", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "读取请求体失败", http.StatusBadRequest)
		return
	}

	ev, err := linear.ParseWebhook(h.secret(), body, r.Header.Get(linear.HeaderSignature))
	if err != nil {
		// 验签失败一律 401，不透露具体原因
		slog.Warn("webhook 验签失败", "err", err, "remote", r.RemoteAddr)
		http.Error(w, "签名校验失败", http.StatusUnauthorized)
		return
	}

	deliveryID := r.Header.Get(linear.HeaderDelivery)
	if deliveryID == "" {
		http.Error(w, "缺少 "+linear.HeaderDelivery+" 头", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	fresh, err := h.Deliveries.ClaimDelivery(ctx, deliveryID, "linear")
	if err != nil {
		slog.Error("登记 webhook 投递失败", "delivery", deliveryID, "err", err)
		// 返回 5xx 让 Linear 重投
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return
	}
	if !fresh {
		// 重投递：直接确认，不重复处理
		writeJSON(w, http.StatusOK, map[string]any{"status": "duplicate", "delivery": deliveryID})
		return
	}

	// D2：只处理「指派给绑定用户」的事件；其余一律确认后忽略
	if !ev.IsAssignedTo(h.userID()) {
		_ = h.Deliveries.FinishDelivery(ctx, deliveryID, "")
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored", "reason": "非指派给本用户的事件"})
		return
	}

	if err := h.Tasks.Enqueue(ctx, ev.Data.ID, ev.Data.Identifier); err != nil {
		slog.Error("排队任务失败", "issue", ev.Data.Identifier, "err", err)
		_ = h.Deliveries.FinishDelivery(ctx, deliveryID, err.Error())
		// 已登记去重，重投也不会再处理，因此返回 200 避免 Linear 无谓重试；
		// 失败原因已落库，靠告警而非重投来发现
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "error": err.Error()})
		return
	}

	_ = h.Deliveries.FinishDelivery(ctx, deliveryID, "")
	slog.Info("已接单", "issue", ev.Data.Identifier, "delivery", deliveryID)
	writeJSON(w, http.StatusOK, map[string]any{"status": "queued", "issue": ev.Data.Identifier})
}

// Health 是存活探针。
func Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
