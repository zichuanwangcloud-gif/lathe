package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/github"
	"github.com/zichuanwangcloud-gif/lathe/internal/runner"
)

// GitHubWebhookTarget 是 GitHub 回调地址里的 slug 解析出的投递目标。
type GitHubWebhookTarget struct {
	// OwnerID 是该回调地址的属主；事件只能推动他名下的任务。
	OwnerID int64
	// Secret 是该用户的 GitHub webhook 签名密钥（设置页按用户配置）。
	Secret string
}

// GitHubTargetResolver 把回调路径里的 slug 解析成投递目标。
//
// 与 Linear 共用用户的 webhook_slug：一个用户一个不可猜的随机段，
// 两个平台各挂一个前缀。没有不带 slug 的兜底路径 —— 这是新入口，
// 没有需要兼容的老部署。
type GitHubTargetResolver interface {
	ResolveGitHub(ctx context.Context, slug string) (*GitHubWebhookTarget, error)
}

// ReviewIntake 把一条 review 意见接进任务状态机（runner.ReviewIntake 实现）。
type ReviewIntake interface {
	AcceptReview(ctx context.Context, ownerID int64, ev *github.ReviewEvent, deliveryID string) (runner.ReviewOutcome, error)
}

// GitHubWebhook 处理来自 GitHub 仓库 webhook 的 PR review 事件
// （pull_request_review_comment / pull_request_review），把 review 意见
// 回流成任务的二轮（docs/02-design.md §7）。
//
// 回调地址 /webhooks/github/{slug}，按用户隔离：用该用户的密钥验签，
// 事件只能推动该用户名下的任务。
type GitHubWebhook struct {
	Resolver   GitHubTargetResolver
	Deliveries DeliveryClaimer
	Reviews    ReviewIntake
}

// deliverySource 是 webhook_deliveries.source 的取值，同时是去重键的前缀：
// 两个平台共用一张表，投递 ID 各自生成，不加前缀理论上可能互撞。
const githubDeliverySource = "github"

// ServeHTTP 实现 http.Handler。
//
// 次序与 Linear 一致：路由 → 验签 → 幂等登记 → 业务判断。幂等登记必须
// 在业务处理之前，否则重投递会重复推动状态。
func (h *GitHubWebhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只接受 POST", http.StatusMethodNotAllowed)
		return
	}

	slug := r.PathValue("slug")
	target, err := h.Resolver.ResolveGitHub(r.Context(), slug)
	if err != nil {
		slog.Warn("GitHub webhook 无法路由", "slug", slug, "err", err, "remote", r.RemoteAddr)
		http.Error(w, "无法路由", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "读取请求体失败", http.StatusBadRequest)
		return
	}
	if err := github.VerifyWebhookSignature(target.Secret, body, r.Header.Get(github.HeaderSignature)); err != nil {
		slog.Warn("GitHub webhook 验签失败", "err", err, "owner", target.OwnerID, "remote", r.RemoteAddr)
		http.Error(w, "签名校验失败", http.StatusUnauthorized)
		return
	}

	event := r.Header.Get(github.HeaderEvent)
	if event == github.EventPing {
		// 建 webhook 时的探测：验签通过就说明密钥配对了。不登记投递 ——
		// 它不推动任何状态，没什么可去重的。
		writeJSON(w, http.StatusOK, map[string]any{"status": "pong"})
		return
	}

	ev, err := github.ParseReviewWebhook(event, body)
	if errors.Is(err, github.ErrUnsupportedEvent) {
		// 仓库 webhook 勾多了事件类型是常见配置，不是错误。
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored", "reason": "不处理的事件类型 " + event})
		return
	}
	if err != nil {
		http.Error(w, "载荷格式错误", http.StatusBadRequest)
		return
	}

	deliveryID := r.Header.Get(github.HeaderDelivery)
	if deliveryID == "" {
		http.Error(w, "缺少 "+github.HeaderDelivery+" 头", http.StatusBadRequest)
		return
	}
	key := githubDeliverySource + ":" + deliveryID

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	fresh, err := h.Deliveries.ClaimDelivery(ctx, key, githubDeliverySource)
	if err != nil {
		slog.Error("登记 GitHub webhook 投递失败", "delivery", deliveryID, "err", err)
		http.Error(w, "内部错误", http.StatusInternalServerError) // 5xx 让 GitHub 重投
		return
	}
	if !fresh {
		writeJSON(w, http.StatusOK, map[string]any{"status": "duplicate", "delivery": deliveryID})
		return
	}

	if ok, reason := ev.Actionable(); !ok {
		_ = h.Deliveries.FinishDelivery(ctx, key, "")
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored", "reason": reason})
		return
	}

	out, err := h.Reviews.AcceptReview(ctx, target.OwnerID, ev, deliveryID)
	if err != nil {
		slog.Error("PR review 回流失败", "repo", ev.Repo, "pr", ev.PRNumber, "owner", target.OwnerID, "err", err)
		_ = h.Deliveries.FinishDelivery(ctx, key, err.Error())
		// 已登记去重，重投也不会再处理，返回 200 避免 GitHub 无谓重试；
		// 失败原因已落投递记录（与 Linear webhook 同一口径）。
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	_ = h.Deliveries.FinishDelivery(ctx, key, "")
	slog.Info("PR review 事件已处理", "repo", ev.Repo, "pr", ev.PRNumber, "owner", target.OwnerID,
		"key", ev.Key, "status", out.Status, "task", out.TaskID, "reason", out.Reason)
	writeJSON(w, http.StatusOK, out)
}
