package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/yunxiao"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// 云效任务来源（f08，docs/features/tracker-yunxiao.md）的 HTTP 面：
// 成员自己的连接管理 + 手动开跑、管理员的 Region 域名允许列表、以及
// /webhooks/yunxiao/{slug} 接单入口。

var (
	// ErrYunxiaoTaskActive 表示该工作项已有进行中的任务（入队撞活任务唯一
	// 索引）。cmd/lathe 的 EnqueueYunxiao 把 SQLSTATE 23505 包成它，/run 据此回 409。
	ErrYunxiaoTaskActive = errors.New("该工作项已有进行中的任务")
	// ErrYunxiaoNoRepo 表示无法确定任务归属仓库（属主还没登记仓库）。/run 回 400。
	ErrYunxiaoNoRepo = errors.New("无法确定任务归属仓库")
)

// YunxiaoEnqueuer 是云效任务的入队面。刻意是新接口而不是给 TaskEnqueuer
// 加方法：后者的 fake 散在 Linear 测试里，加方法就要改 Linear 测试文件。
type YunxiaoEnqueuer interface {
	// EnqueueYunxiao 建一条 tracker_provider='yunxiao' 的 queued 任务，返回任务 id。
	EnqueueYunxiao(ctx context.Context, ownerUserID int64, workitemID, serial string) (int64, error)
	// CancelForYunxiao 取消该属主名下 key 对应的在途云效任务（只改 DB 状态，
	// 不停正在跑的 agent —— 与 Linear T7 同一已知边界）。
	CancelForYunxiao(ctx context.Context, ownerUserID int64, key string) ([]int64, error)
}

// YunxiaoClientFunc 按连接配置构造云效客户端。生产是 yunxiao.NewClient；
// 测试注入指向 httptest 的实现。
type YunxiaoClientFunc func(cfg yunxiao.Config) (*yunxiao.Client, error)

// clientForConn 用连接与现取的允许列表构造客户端。
func clientForConn(ctx context.Context, st *store.Store, fn YunxiaoClientFunc, c *store.YunxiaoConnection) (*yunxiao.Client, error) {
	if fn == nil {
		fn = yunxiao.NewClient
	}
	var allowed []string
	if st != nil {
		allowed = st.YunxiaoAllowedHosts(ctx)
	}
	return fn(yunxiao.Config{
		Endpoint: c.Endpoint, OrganizationID: c.OrganizationID, Token: c.Token, AllowedHosts: allowed,
	})
}

// ---------------------------------------------------------------- webhook

// YunxiaoWebhookTarget 是 slug 解析出的投递目标。
type YunxiaoWebhookTarget struct {
	OwnerID      int64
	Secret       string
	AccountID    string // 「指派给我」判定用（验证时回写）
	TriggerLabel string // 空 = 标签接单关闭
	StatusCancel string // 空 = 取消联动关闭
	// Client 是用属主令牌回源的客户端；构造失败时为 nil，ClientErr 说明原因。
	Client    *yunxiao.Client
	ClientErr error
}

// YunxiaoTargetResolver 把回调路径里的 slug 解析成投递目标。
type YunxiaoTargetResolver interface {
	ResolveYunxiao(ctx context.Context, slug string) (*YunxiaoWebhookTarget, error)
}

// YunxiaoResolver 是生产用的 resolver：slug → 用户 → 连接，全部现取。
//
// 停用 / 无连接 / 无 webhook 密钥一律报错（→ 404，不透露是哪一环）。
// 没有空 slug 兜底（同 GitHub 新入口的立场）。
type YunxiaoResolver struct {
	Users     *store.Users
	Conns     *store.YunxiaoConnections
	Store     *store.Store
	ClientFor YunxiaoClientFunc
}

// ResolveYunxiao 实现 YunxiaoTargetResolver。
func (r *YunxiaoResolver) ResolveYunxiao(ctx context.Context, slug string) (*YunxiaoWebhookTarget, error) {
	if slug == "" {
		return nil, errors.New("缺少回调地址随机段")
	}
	u, err := r.Users.ByWebhookSlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	if u.Disabled() {
		return nil, errors.New("账号已停用")
	}
	conn, err := r.Conns.Get(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	if conn.WebhookSecret == "" {
		return nil, errors.New("该用户尚未配置云效 webhook 密钥")
	}
	t := &YunxiaoWebhookTarget{
		OwnerID: u.ID, Secret: conn.WebhookSecret, AccountID: conn.AccountID,
		TriggerLabel: strings.TrimSpace(conn.TriggerLabel), StatusCancel: strings.TrimSpace(conn.StatusCancel),
	}
	t.Client, t.ClientErr = clientForConn(ctx, r.Store, r.ClientFor, conn)
	return t, nil
}

// YunxiaoTriggerObserver 是接单边沿检测的快照面（store.YunxiaoConnections 实现）。
type YunxiaoTriggerObserver interface {
	ObserveTrigger(ctx context.Context, userID int64, workitemID string, assigned, labeled bool) (bool, error)
}

// YunxiaoWebhook 处理云效自动化规则投递的 webhook。
type YunxiaoWebhook struct {
	Resolver   YunxiaoTargetResolver
	Deliveries DeliveryClaimer
	Triggers   YunxiaoTriggerObserver
	Tasks      YunxiaoEnqueuer
}

const yunxiaoDeliverySource = "yunxiao"

// ServeHTTP 实现 http.Handler。次序（§3.0）：路由 → 验签 → 只取 id →
// 回源 → 去重登记 → 取消判定 → 边沿检测 → 入队。
//
// 与 Linear 的差别在于「先回源后登记」：云效没有投递 ID，去重键要用回源
// 拿到的 gmtModified；回源失败返回 503 且未登记，对端重投可再处理。
func (h *YunxiaoWebhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只接受 POST", http.StatusMethodNotAllowed)
		return
	}
	target, err := h.Resolver.ResolveYunxiao(r.Context(), r.PathValue("slug"))
	if err != nil {
		// 未知 slug、停用、未配置一律 404，不透露哪一环。slug 本身不进日志
		// 以外的地方；它不是秘密（随机段），但 webhook 密钥绝不记。
		slog.Warn("云效 webhook 无法路由", "err", err, "remote", r.RemoteAddr)
		http.Error(w, "无法路由", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "读取请求体失败", http.StatusBadRequest)
		return
	}
	if err := yunxiao.VerifySecret(target.Secret, r.Header.Get(yunxiao.HeaderSignature)); err != nil {
		slog.Warn("云效 webhook 验签失败", "owner", target.OwnerID, "remote", r.RemoteAddr)
		http.Error(w, "签名校验失败", http.StatusUnauthorized)
		return
	}
	// 载荷除 id 外一字不信：明文共享密钥不保护载荷完整性。
	id, err := yunxiao.ParseWorkitemID(body)
	if err != nil {
		http.Error(w, "载荷缺少合法的工作项 id", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if target.Client == nil {
		slog.Warn("云效 webhook 回源客户端不可用", "owner", target.OwnerID, "err", target.ClientErr)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "error", "error": "云效连接不可用"})
		return
	}
	wi, err := target.Client.GetWorkitem(ctx, id)
	if err != nil {
		// 错误串只有分类人话 + 状态码（客户端保证不含响应体与令牌）。
		slog.Warn("云效 webhook 回源失败", "owner", target.OwnerID, "workitem", id, "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "error", "error": err.Error()})
		return
	}

	key := yunxiaoDeliveryKey(target.OwnerID, wi)
	fresh, err := h.Deliveries.ClaimDelivery(ctx, key, yunxiaoDeliverySource)
	if err != nil {
		slog.Error("登记云效 webhook 投递失败", "owner", target.OwnerID, "workitem", wi.ID, "err", err)
		http.Error(w, "内部错误", http.StatusInternalServerError)
		return
	}
	if !fresh {
		writeJSON(w, http.StatusOK, map[string]any{"status": "duplicate", "workitem": wi.ID})
		return
	}

	// 取消联动在接单闸门之前（同 Linear T7）。
	if target.StatusCancel != "" && yunxiao.MatchStatus(wi.Status, target.StatusCancel) {
		ids, cerr := h.Tasks.CancelForYunxiao(ctx, target.OwnerID, wi.Key())
		if cerr != nil {
			slog.Error("云效工作项取消联动失败", "workitem", wi.ID, "owner", target.OwnerID, "err", cerr)
			_ = h.Deliveries.FinishDelivery(ctx, key, cerr.Error())
			writeJSON(w, http.StatusOK, map[string]any{"status": "error", "error": cerr.Error()})
			return
		}
		_ = h.Deliveries.FinishDelivery(ctx, key, "")
		slog.Info("云效工作项已取消，联动取消在途任务", "workitem", wi.ID, "owner", target.OwnerID, "tasks", ids)
		writeJSON(w, http.StatusOK, map[string]any{"status": "cancelled", "workitem": wi.Key(), "tasks": ids})
		return
	}

	assigned := wi.AssignedTo.ID != "" && wi.AssignedTo.ID == target.AccountID
	labeled := target.TriggerLabel != "" && yunxiao.HasLabel(wi.Labels, target.TriggerLabel)
	rose, err := h.Triggers.ObserveTrigger(ctx, target.OwnerID, wi.ID, assigned, labeled)
	if err != nil {
		slog.Error("云效接单快照写入失败", "workitem", wi.ID, "owner", target.OwnerID, "err", err)
		_ = h.Deliveries.FinishDelivery(ctx, key, err.Error())
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "error": "接单快照写入失败"})
		return
	}
	if wi.LogicalStatus == "archived" {
		_ = h.Deliveries.FinishDelivery(ctx, key, "")
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored", "reason": "工作项已归档"})
		return
	}
	if !rose {
		_ = h.Deliveries.FinishDelivery(ctx, key, "")
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored", "reason": "本次没有新指派给本用户，也没有新打上触发标签"})
		return
	}

	if _, err := h.Tasks.EnqueueYunxiao(ctx, target.OwnerID, wi.ID, wi.Key()); err != nil {
		slog.Error("云效任务排队失败", "workitem", wi.ID, "owner", target.OwnerID, "err", err)
		_ = h.Deliveries.FinishDelivery(ctx, key, err.Error())
		writeJSON(w, http.StatusOK, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	_ = h.Deliveries.FinishDelivery(ctx, key, "")
	slog.Info("已接云效工作项", "workitem", wi.ID, "key", wi.Key(), "owner", target.OwnerID)
	writeJSON(w, http.StatusOK, map[string]any{"status": "queued", "workitem": wi.Key()})
}

// yunxiaoDeliveryKey 是去重键 yunxiao:<uid>:<workitemId>:<gmtModified>。
// 两条自动化规则对同一次修改各投一次时撞同一个键。gmtModified 缺失时
// 退回指派/标签/状态的组合，至少保证「什么都没变」的重投被挡住。
func yunxiaoDeliveryKey(uid int64, wi *yunxiao.Workitem) string {
	ver := wi.GmtModified
	if ver == "" {
		var ls []string
		for _, l := range wi.Labels {
			ls = append(ls, l.Name)
		}
		ver = "s:" + wi.Status.ID + "|a:" + wi.AssignedTo.ID + "|l:" + strings.Join(ls, ",")
	}
	return fmt.Sprintf("%s:%d:%s:%s", yunxiaoDeliverySource, uid, wi.ID, ver)
}

// ---------------------------------------------------------------- 成员 / 管理员 API

// YunxiaoAPI 是云效连接的配置接口：只读写 CurrentUser 自己的连接，没有
// 任何接受他人 ID 的路径。
type YunxiaoAPI struct {
	Conns     *store.YunxiaoConnections
	Store     *store.Store
	Auth      *Auth
	Tasks     YunxiaoEnqueuer
	ClientFor YunxiaoClientFunc
}

// Routes 注册接口。
func (a *YunxiaoAPI) Routes(mux *http.ServeMux) {
	mux.Handle("GET /api/yunxiao/connection", a.Auth.RequireFunc(a.get))
	mux.Handle("PUT /api/yunxiao/connection", a.Auth.RequireFunc(a.save))
	mux.Handle("POST /api/yunxiao/connection/verify", a.Auth.RequireFunc(a.verify))
	mux.Handle("DELETE /api/yunxiao/connection", a.Auth.RequireFunc(a.remove))
	mux.Handle("POST /api/yunxiao/workitems/{id}/run", a.Auth.RequireFunc(a.run))
	mux.Handle("GET /api/admin/yunxiao/allowed-hosts", a.Auth.RequireAdminFunc(a.getHosts))
	mux.Handle("PUT /api/admin/yunxiao/allowed-hosts", a.Auth.RequireAdminFunc(a.putHosts))
}

const (
	yxMaxLabel      = 200
	yxMaxStatusName = 64
	yxMaxSecret     = 512
	yxMinSecret     = 16
)

func badRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg})
}

func (a *YunxiaoAPI) get(w http.ResponseWriter, r *http.Request) {
	u := CurrentUser(r)
	st, err := a.Conns.Status(r.Context(), u.ID)
	if err != nil {
		serverError(w, "读取云效连接失败", err)
		return
	}
	out := struct {
		*store.YunxiaoStatus
		WebhookPath string `json:"webhookPath,omitempty"`
	}{YunxiaoStatus: st}
	if u.WebhookSlug != "" {
		out.WebhookPath = "/webhooks/yunxiao/" + u.WebhookSlug
	}
	writeJSON(w, http.StatusOK, out)
}

// save 校验并保存连接，随后立即验证（验证失败也保存，同凭据接口立场）。
func (a *YunxiaoAPI) save(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Endpoint       string `json:"endpoint"`
		OrganizationID string `json:"organizationId"`
		Token          string `json:"token"`
		WebhookSecret  string `json:"webhookSecret"`
		TriggerLabel   string `json:"triggerLabel"`
		StatusStarted  string `json:"statusStarted"`
		StatusPROpen   string `json:"statusPrOpen"`
		StatusMerged   string `json:"statusMerged"`
		StatusCancel   string `json:"statusCancel"`
	}
	if err := decodeJSON(r, &body); err != nil {
		badRequest(w, "请求体格式错误")
		return
	}
	ctx := r.Context()
	p := store.YunxiaoSaveParams{
		OrganizationID: strings.TrimSpace(body.OrganizationID),
		Token:          strings.TrimSpace(body.Token),
		WebhookSecret:  strings.TrimSpace(body.WebhookSecret),
		TriggerLabel:   strings.TrimSpace(body.TriggerLabel),
		StatusStarted:  strings.TrimSpace(body.StatusStarted),
		StatusPROpen:   strings.TrimSpace(body.StatusPROpen),
		StatusMerged:   strings.TrimSpace(body.StatusMerged),
		StatusCancel:   strings.TrimSpace(body.StatusCancel),
	}
	endpoint := strings.TrimSpace(body.Endpoint)
	if endpoint == "" {
		endpoint = yunxiao.DefaultEndpoint
	}
	norm, err := yunxiao.ValidateEndpoint(endpoint, a.Store.YunxiaoAllowedHosts(ctx))
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	p.Endpoint = norm
	if msg := validateYunxiaoSave(p); msg != "" {
		badRequest(w, msg)
		return
	}

	uid := CurrentUser(r).ID
	if err := a.Conns.Save(ctx, uid, p); err != nil {
		if errors.Is(err, store.ErrYunxiaoTokenRequired) {
			badRequest(w, "首次保存必须填写云效个人访问令牌")
			return
		}
		serverError(w, "保存云效连接失败", err)
		return
	}
	slog.Info("云效连接已保存", "by", actorOf(r), "endpoint", p.Endpoint, "region", p.OrganizationID == "")
	writeJSON(w, http.StatusOK, map[string]any{"saved": true, "verify": a.runVerify(ctx, uid)})
}

// validateYunxiaoSave 校验非接入点字段（§3.5），返回给人看的错误；空 = 通过。
func validateYunxiaoSave(p store.YunxiaoSaveParams) string {
	if p.OrganizationID != "" && !yunxiao.ValidWorkitemID(p.OrganizationID) {
		return "组织 ID 格式不合法（只允许字母、数字、_、-，至多 64 位）"
	}
	if len(p.Token) > yxMaxSecret {
		return "令牌过长（上限 512 字节）"
	}
	if p.WebhookSecret != "" && (len(p.WebhookSecret) < yxMinSecret || len(p.WebhookSecret) > yxMaxSecret) {
		return "webhook 密钥长度须在 16–512 字节之间"
	}
	if utf8.RuneCountInString(p.TriggerLabel) > yxMaxLabel {
		return "触发标签名过长（上限 200 字符）"
	}
	for _, s := range []string{p.StatusStarted, p.StatusPROpen, p.StatusMerged, p.StatusCancel} {
		if utf8.RuneCountInString(s) > yxMaxStatusName {
			return "状态名过长（上限 64 字符）"
		}
	}
	// 自激防护：取消状态与任一同步状态同名时，Lathe 推状态 → 对端 webhook →
	// Lathe 取消自己。
	if p.StatusCancel != "" {
		for _, s := range []string{p.StatusStarted, p.StatusPROpen, p.StatusMerged} {
			if strings.EqualFold(s, p.StatusCancel) {
				return "「取消」状态不能与开跑 / PR 已开 / 已合并的同步状态同名"
			}
		}
	}
	return ""
}

// runVerify 用库里存的令牌调 CurrentUser 并回写结果。
func (a *YunxiaoAPI) runVerify(ctx context.Context, uid int64) map[string]any {
	conn, err := a.Conns.Get(ctx, uid)
	if err != nil {
		return map[string]any{"ok": false, "error": "读取云效连接失败"}
	}
	fail := func(msg string) map[string]any {
		if err := a.Conns.MarkVerifyFailed(ctx, uid, msg); err != nil {
			slog.Warn("记录云效验证失败原因失败", "user", uid, "err", err)
		}
		return map[string]any{"ok": false, "error": msg}
	}
	cl, err := clientForConn(ctx, a.Store, a.ClientFor, conn)
	if err != nil {
		return fail(err.Error())
	}
	me, err := cl.CurrentUser(ctx)
	if err != nil {
		return fail(err.Error())
	}
	name := me.Name
	if name == "" {
		name = me.NickName
	}
	if err := a.Conns.MarkVerified(ctx, uid, me.ID, name); err != nil {
		slog.Warn("记录云效验证结果失败", "user", uid, "err", err)
	}
	return map[string]any{"ok": true, "accountName": name}
}

func (a *YunxiaoAPI) verify(w http.ResponseWriter, r *http.Request) {
	uid := CurrentUser(r).ID
	if _, err := a.Conns.Get(r.Context(), uid); err != nil {
		if errors.Is(err, store.ErrYunxiaoNotConfigured) {
			badRequest(w, "请先配置云效连接")
			return
		}
		serverError(w, "读取云效连接失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"verify": a.runVerify(r.Context(), uid)})
}

func (a *YunxiaoAPI) remove(w http.ResponseWriter, r *http.Request) {
	if err := a.Conns.Delete(r.Context(), CurrentUser(r).ID); err != nil {
		serverError(w, "删除云效连接失败", err)
		return
	}
	slog.Info("云效连接已删除", "by", actorOf(r))
	w.WriteHeader(http.StatusNoContent)
}

// run 按工作项 ID 手动开跑。无副作用的检查在前；不读写接单快照
// （快照只服务 webhook 的边沿检测）。
func (a *YunxiaoAPI) run(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !yunxiao.ValidWorkitemID(id) {
		badRequest(w, "工作项 ID 不合法")
		return
	}
	ctx := r.Context()
	uid := CurrentUser(r).ID
	conn, err := a.Conns.Get(ctx, uid)
	if err != nil {
		if errors.Is(err, store.ErrYunxiaoNotConfigured) {
			badRequest(w, "请先配置云效连接")
			return
		}
		serverError(w, "读取云效连接失败", err)
		return
	}
	cl, err := clientForConn(ctx, a.Store, a.ClientFor, conn)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	wi, err := cl.GetWorkitem(ctx, id)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	taskID, err := a.Tasks.EnqueueYunxiao(ctx, uid, wi.ID, wi.Key())
	switch {
	case errors.Is(err, ErrYunxiaoTaskActive):
		writeJSON(w, http.StatusConflict, map[string]any{"error": ErrYunxiaoTaskActive.Error()})
		return
	case errors.Is(err, ErrYunxiaoNoRepo):
		badRequest(w, err.Error())
		return
	case err != nil:
		serverError(w, "建任务失败", err)
		return
	}
	slog.Info("云效工作项手动开跑", "by", actorOf(r), "workitem", wi.ID, "task", taskID)
	writeJSON(w, http.StatusCreated, map[string]any{"taskId": taskID, "externalKey": wi.Key()})
}

func (a *YunxiaoAPI) getHosts(w http.ResponseWriter, r *http.Request) {
	hosts := a.Store.YunxiaoAllowedHosts(r.Context())
	if hosts == nil {
		hosts = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": hosts})
}

func (a *YunxiaoAPI) putHosts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Hosts []string `json:"hosts"`
	}
	if err := decodeJSON(r, &body); err != nil {
		badRequest(w, "请求体格式错误")
		return
	}
	if len(body.Hosts) > yunxiao.MaxAllowedHosts {
		badRequest(w, fmt.Sprintf("至多登记 %d 个域名", yunxiao.MaxAllowedHosts))
		return
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(body.Hosts))
	for _, h := range body.Hosts {
		n, err := yunxiao.ValidateHostEntry(h)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if err := a.Store.SetYunxiaoAllowedHosts(r.Context(), out); err != nil {
		serverError(w, "保存云效域名允许列表失败", err)
		return
	}
	slog.Info("云效 Region 域名允许列表已更新", "by", actorOf(r), "hosts", out)
	writeJSON(w, http.StatusOK, map[string]any{"hosts": out})
}
