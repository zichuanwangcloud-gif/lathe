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
//
// ownerUserID 是任务归属（P1.5 第二步）：谁接的单，任务就归谁 ——
// 用谁的 Linear/GitHub 凭据跑、出现在谁的看板上。
type TaskEnqueuer interface {
	Enqueue(ctx context.Context, ownerUserID int64, issueID, issueKey string) error
	// Requeue 重新派发一个已存在的任务（手动重试 / 启动恢复）。
	// 与 Enqueue 的区别：不新建任务行 —— 同一 issue 的活任务唯一索引
	// 会把新建挡掉，重试因此永远卡死（任务 #313 的教训）。
	// mode 是重试模式（auto/resume/fresh，见 runner.RetryMode），
	// 空串按 auto（智能决策）处理。
	Requeue(ctx context.Context, taskID int64, mode string) error
}

// WebhookTarget 是一个 slug 解析出的投递目标。
type WebhookTarget struct {
	// OwnerID 是任务归属用户。
	OwnerID int64
	// Secret 是该用户的 webhook 签名密钥（各自在设置页配置）。
	Secret string
	// LinearUserID 用于「指派给我了吗」的接单判定（D2）。
	LinearUserID string
}

// TargetResolver 把回调路径里的 slug 解析成投递目标。
//
// 空 slug（旧路径 /webhooks/linear）由实现方决定兜底 —— 生产实现
// 映射到内置管理员，保住老部署的既有 webhook 配置。
type TargetResolver interface {
	Resolve(ctx context.Context, slug string) (*WebhookTarget, error)
}

// LinearWebhook 处理来自 Linear 的 webhook。
//
// P1.5 第二步起按用户隔离：每个用户在设置页拿到自己的回调地址
// /webhooks/linear/{slug}，签名密钥与接单判定都按该用户的凭据来。
type LinearWebhook struct {
	// Resolver 按 slug 解析投递目标；为 nil 时一切投递都无法路由。
	Resolver TargetResolver

	Deliveries DeliveryClaimer
	Tasks      TaskEnqueuer
}

// ServeHTTP 实现 http.Handler。
//
// 处理次序刻意如此：路由 → 验签 → 幂等登记 → 业务判断。
// 幂等登记必须在业务处理之前，否则重投递会重复建任务。
func (h *LinearWebhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只接受 POST", http.StatusMethodNotAllowed)
		return
	}

	slug := r.PathValue("slug")
	target, err := h.Resolver.Resolve(r.Context(), slug)
	if err != nil {
		// 未知 slug 与验签失败同等处理：404/401 不透露哪个环节挡掉的。
		// slug 本身是不可猜的随机段，试错成本对攻击者已经足够高。
		slog.Warn("webhook 无法路由", "slug", slug, "err", err, "remote", r.RemoteAddr)
		http.Error(w, "无法路由", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "读取请求体失败", http.StatusBadRequest)
		return
	}

	ev, err := linear.ParseWebhook(target.Secret, body, r.Header.Get(linear.HeaderSignature))
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
	if !ev.IsAssignedTo(target.LinearUserID) {
		_ = h.Deliveries.FinishDelivery(ctx, deliveryID, "")
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored", "reason": "非指派给本用户的事件"})
		return
	}

	if err := h.Tasks.Enqueue(ctx, target.OwnerID, ev.Data.ID, ev.Data.Identifier); err != nil {
		slog.Error("排队任务失败", "issue", ev.Data.Identifier, "owner", target.OwnerID, "err", err)
		_ = h.Deliveries.FinishDelivery(ctx, deliveryID, err.Error())
		// 已登记去重，重投也不会再处理，因此返回 200 避免 Linear 无谓重试；
		// 失败原因已落库，靠告警而非重投来发现
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "error": err.Error()})
		return
	}

	_ = h.Deliveries.FinishDelivery(ctx, deliveryID, "")
	slog.Info("已接单", "issue", ev.Data.Identifier, "owner", target.OwnerID, "delivery", deliveryID)
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
