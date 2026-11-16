package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/vcs"
	"github.com/zichuanwangcloud-gif/lathe/internal/vcs/gitea"
	"github.com/zichuanwangcloud-gif/lathe/internal/vcs/gitlab"
)

// vcs_webhook.go 是 GitLab / Gitea 的 PR 评论回流入口（/webhooks/gitlab/{slug}、
// /webhooks/gitea/{slug}），与 GitHub 版同形：路由 → 验签 → 幂等登记 → 平台解析
// → 信任判定 → ReviewIntake.AcceptReview。信任判定写在各平台包里（成员/协作者
// 查询，fail-closed），通过之后才构造平台中立的 vcs.ReviewEvent。
//
// 与 GitHub 版的一处刻意差异：安全忽略（含信任拒绝）的原因也落进投递记录
// （webhook_deliveries.error，带 "ignored: " 前缀），而信任拒绝的细节不进响应体
// —— 不让响应变成探测他人仓库成员关系的手段。

// VCSWebhookTarget 是回调地址里的 slug 解析出的投递目标。
type VCSWebhookTarget struct {
	// OwnerID 是该回调地址的属主；事件只能推动他名下的任务。
	OwnerID int64
	// Secret 是该用户该平台的 webhook 密钥（GitLab Secret Token / Gitea Secret）。
	Secret string
	// GitLab / Gitea 是按属主凭据构造的成员（协作者）查询能力；nil 时一律按不可信拒绝。
	GitLab gitlab.MemberChecker
	Gitea  gitea.Collaborators
}

// VCSTargetResolver 把 slug 解析成某平台的投递目标。未知 slug、未配密钥 → 错误（404）。
type VCSTargetResolver interface {
	ResolveVCSWebhook(ctx context.Context, provider vcs.Provider, slug string) (*VCSWebhookTarget, error)
}

const (
	gitlabDeliverySource = "gitlab"
	giteaDeliverySource  = "gitea"
)

// trustRejectedReason 是信任拒绝时给响应体的统一文案（细节只进投递记录）。
const trustRejectedReason = "评论者未通过仓库成员关系校验，不驱动二轮（原因已记入投递记录）"

// vcsWebhookBase 是两个平台 handler 的公共部分。
type vcsWebhookBase struct {
	provider   vcs.Provider
	source     string
	Resolver   VCSTargetResolver
	Deliveries DeliveryClaimer
	Reviews    ReviewIntake
}

// begin 做「方法 → 路由 → 读体」三步；返回 nil 表示已写响应。
func (b *vcsWebhookBase) begin(w http.ResponseWriter, r *http.Request) (*VCSWebhookTarget, []byte) {
	if r.Method != http.MethodPost {
		http.Error(w, "只接受 POST", http.StatusMethodNotAllowed)
		return nil, nil
	}
	slug := r.PathValue("slug")
	target, err := b.Resolver.ResolveVCSWebhook(r.Context(), b.provider, slug)
	if err != nil || target == nil {
		slog.Warn("代码平台 webhook 无法路由", "provider", b.provider, "slug", slug, "err", err, "remote", r.RemoteAddr)
		http.Error(w, "无法路由", http.StatusNotFound)
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "读取请求体失败", http.StatusBadRequest)
		return nil, nil
	}
	return target, body
}

// claim 幂等登记；返回 false 表示已写响应（重复或出错）。
func (b *vcsWebhookBase) claim(ctx context.Context, w http.ResponseWriter, key, deliveryID string) bool {
	fresh, err := b.Deliveries.ClaimDelivery(ctx, key, b.source)
	if err != nil {
		slog.Error("登记代码平台 webhook 投递失败", "provider", b.provider, "delivery", deliveryID, "err", err)
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return false
	}
	if !fresh {
		writeJSON(w, http.StatusOK, map[string]any{"status": "duplicate", "delivery": deliveryID})
		return false
	}
	return true
}

// ignore 安全忽略：原因落投递记录；trust 拒绝的细节不进响应体。
func (b *vcsWebhookBase) ignore(ctx context.Context, w http.ResponseWriter, key, reason string, trust bool) {
	_ = b.Deliveries.FinishDelivery(ctx, key, "ignored: "+reason)
	shown := reason
	if trust {
		shown = trustRejectedReason
		slog.Info("代码平台 review 回流被信任判定拒绝", "provider", b.provider, "delivery", key, "reason", reason)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ignored", "reason": shown})
}

// accept 交给 ReviewIntake 并写响应（与 GitHub 版同一口径）。
func (b *vcsWebhookBase) accept(ctx context.Context, w http.ResponseWriter, owner int64, key, deliveryID string, ev *vcs.ReviewEvent) {
	out, err := b.Reviews.AcceptReview(ctx, owner, ev, deliveryID)
	if err != nil {
		slog.Error("PR review 回流失败", "provider", b.provider, "repo", ev.Repo, "pr", ev.PRNumber, "owner", owner, "err", err)
		_ = b.Deliveries.FinishDelivery(ctx, key, err.Error())
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	_ = b.Deliveries.FinishDelivery(ctx, key, "")
	slog.Info("PR review 事件已处理", "provider", b.provider, "repo", ev.Repo, "pr", ev.PRNumber, "owner", owner,
		"key", ev.Key, "status", out.Status, "task", out.TaskID, "reason", out.Reason)
	writeJSON(w, http.StatusOK, out)
}

// GitLabWebhook 处理 GitLab 项目 webhook 的 Note Hook（MR 上的评论）。
type GitLabWebhook struct {
	Resolver   VCSTargetResolver
	Deliveries DeliveryClaimer
	Reviews    ReviewIntake
}

func (h *GitLabWebhook) base() *vcsWebhookBase {
	return &vcsWebhookBase{provider: vcs.ProviderGitLab, source: gitlabDeliverySource,
		Resolver: h.Resolver, Deliveries: h.Deliveries, Reviews: h.Reviews}
}

// ServeHTTP 实现 http.Handler。
func (h *GitLabWebhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b := h.base()
	target, body := b.begin(w, r)
	if target == nil {
		return
	}
	if err := gitlab.VerifyToken(target.Secret, r.Header.Get(gitlab.HeaderToken)); err != nil {
		slog.Warn("GitLab webhook 验签失败", "err", err, "owner", target.OwnerID, "remote", r.RemoteAddr)
		http.Error(w, "签名校验失败", http.StatusUnauthorized)
		return
	}

	event := r.Header.Get(gitlab.HeaderEvent)
	ev, perr := gitlab.ParseNoteHook(event, body)
	deliveryID := r.Header.Get(gitlab.HeaderEventUUID)
	if deliveryID == "" {
		if ev == nil {
			// 没有 UUID 又解析不出评论：既无从去重，也无事可做。
			if errors.Is(perr, gitlab.ErrUnsupportedEvent) {
				writeJSON(w, http.StatusOK, map[string]any{"status": "ignored", "reason": "不处理的事件 " + event})
				return
			}
			http.Error(w, "缺少 "+gitlab.HeaderEventUUID+" 头且载荷无法解析", http.StatusBadRequest)
			return
		}
		deliveryID = ev.DeliveryID("")
	}
	key := gitlabDeliverySource + ":" + deliveryID

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if !b.claim(ctx, w, key, deliveryID) {
		return
	}
	if perr != nil {
		if errors.Is(perr, gitlab.ErrUnsupportedEvent) {
			b.ignore(ctx, w, key, perr.Error(), false)
			return
		}
		_ = h.Deliveries.FinishDelivery(ctx, key, perr.Error())
		http.Error(w, "载荷格式错误", http.StatusBadRequest)
		return
	}
	rev, reason, trust := gitlab.Actionable(ctx, target.GitLab, ev)
	if rev == nil {
		b.ignore(ctx, w, key, reason, trust)
		return
	}
	b.accept(ctx, w, target.OwnerID, key, deliveryID, rev)
}

// GiteaWebhook 处理 Gitea 仓库 webhook（PR 对话里的普通评论）。
type GiteaWebhook struct {
	Resolver   VCSTargetResolver
	Deliveries DeliveryClaimer
	Reviews    ReviewIntake
}

func (h *GiteaWebhook) base() *vcsWebhookBase {
	return &vcsWebhookBase{provider: vcs.ProviderGitea, source: giteaDeliverySource,
		Resolver: h.Resolver, Deliveries: h.Deliveries, Reviews: h.Reviews}
}

// ServeHTTP 实现 http.Handler。
func (h *GiteaWebhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b := h.base()
	target, body := b.begin(w, r)
	if target == nil {
		return
	}
	if err := gitea.VerifySignature(target.Secret, body, r.Header.Get(gitea.HeaderSignature)); err != nil {
		slog.Warn("Gitea webhook 验签失败", "err", err, "owner", target.OwnerID, "remote", r.RemoteAddr)
		http.Error(w, "签名校验失败", http.StatusUnauthorized)
		return
	}
	deliveryID := r.Header.Get(gitea.HeaderDelivery)
	if deliveryID == "" {
		http.Error(w, "缺少 "+gitea.HeaderDelivery+" 头", http.StatusBadRequest)
		return
	}
	key := giteaDeliverySource + ":" + deliveryID

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if !b.claim(ctx, w, key, deliveryID) {
		return
	}
	ev, ignore, err := gitea.Parse(r.Header.Get(gitea.HeaderEvent), r.Header.Get(gitea.HeaderEventType), body)
	if err != nil {
		_ = h.Deliveries.FinishDelivery(ctx, key, err.Error())
		http.Error(w, "载荷格式错误", http.StatusBadRequest)
		return
	}
	if ev == nil {
		b.ignore(ctx, w, key, ignore, false)
		return
	}
	rev, reason, trust := gitea.Actionable(ctx, target.Gitea, ev)
	if rev == nil {
		b.ignore(ctx, w, key, reason, trust)
		return
	}
	b.accept(ctx, w, target.OwnerID, key, deliveryID, rev)
}
