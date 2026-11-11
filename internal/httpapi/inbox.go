package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/zichuanwangcloud-gif/lathe/internal/inbox"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// InboxAPI 是收件箱（docs/features/inbox.md）的 HTTP 面。
//
// 全部挂在 RequireFunc 之下，userID 一律取 CurrentUser —— 没有任何「看别人
// 的收件箱」的参数，管理员也一样。收件箱的端点不改任何 tasks / prds 行，
// 不写 task_events：处理动作仍走原来的详情页与端点。
type InboxAPI struct {
	Service *inbox.Service
	Auth    *Auth
}

// Routes 把收件箱接口注册到 mux。
func (a *InboxAPI) Routes(mux *http.ServeMux) {
	mux.Handle("GET /api/inbox", a.Auth.RequireFunc(a.list))
	mux.Handle("GET /api/inbox/count", a.Auth.RequireFunc(a.count))
	mux.Handle("POST /api/inbox/items/{key}/read", a.Auth.RequireFunc(a.mark(store.InboxMarkRead)))
	mux.Handle("POST /api/inbox/items/{key}/unread", a.Auth.RequireFunc(a.mark(store.InboxMarkUnread)))
	mux.Handle("POST /api/inbox/items/{key}/dismiss", a.Auth.RequireFunc(a.mark(store.InboxMarkDismiss)))
	mux.Handle("POST /api/inbox/read-all", a.Auth.RequireFunc(a.readAll))
}

// inboxNotFoundBody 是 404 的固定响应体：不带键，也不带主体是否存在的任何信息。
var inboxNotFoundBody = map[string]any{"error": "事项不存在"}

// inboxServerError 只记错误本身，不记事项内容与请求的键。
func inboxServerError(w http.ResponseWriter, msg string, err error) {
	slog.Error(msg, "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error": msg})
}

func (a *InboxAPI) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	show := q.Get("filter")
	switch show {
	case "", inbox.FilterAll, inbox.FilterUnread:
	default:
		// 不回显原始输入
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "filter 只能是 unread 或 all"})
		return
	}
	kind := inbox.Kind(q.Get("kind"))
	if kind != "" && !kind.Valid() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "kind 不是已知的事项种类"})
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	page, err := a.Service.Items(r.Context(), CurrentUser(r).ID, inbox.Filter{
		Show: show, Kind: kind, Limit: limit, Offset: offset,
	})
	if err != nil {
		inboxServerError(w, "查询收件箱失败", err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (a *InboxAPI) count(w http.ResponseWriter, r *http.Request) {
	unread, total, err := a.Service.Count(r.Context(), CurrentUser(r).ID)
	if err != nil {
		inboxServerError(w, "统计收件箱失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"unread": unread, "total": total})
}

func (a *InboxAPI) mark(op store.InboxMarkOp) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		it, err := a.Service.Mark(r.Context(), CurrentUser(r).ID, r.PathValue("key"), op)
		var stale *inbox.StaleError
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, map[string]any{"item": it})
		case errors.Is(err, inbox.ErrBadKey):
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "事项键不合法"})
		case errors.Is(err, inbox.ErrNotFound):
			writeJSON(w, http.StatusNotFound, inboxNotFoundBody)
		case errors.As(err, &stale):
			// 主体已确认属于本人，返回它当前的事项不泄露任何东西
			writeJSON(w, http.StatusConflict, map[string]any{"error": "该事项已更新", "current": stale.Current})
		case errors.Is(err, inbox.ErrNotDismissible):
			writeJSON(w, http.StatusConflict, map[string]any{"error": "该事项不可忽略：它要靠处理来离开收件箱"})
		default:
			inboxServerError(w, "更新收件箱标记失败", err)
		}
	}
}

func (a *InboxAPI) readAll(w http.ResponseWriter, r *http.Request) {
	n, err := a.Service.ReadAll(r.Context(), CurrentUser(r).ID)
	if err != nil {
		inboxServerError(w, "全部标为已读失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"marked": n})
}
