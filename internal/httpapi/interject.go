package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/zichuanwangcloud-gif/lathe/internal/runner"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// interject.go —— 运行中插话的两个接口（docs/features/interject.md §3.3）：
//
//	POST /api/tasks/{id}/interject           落库 + 尝试投递；body {"message": "..."}
//	GET  /api/tasks/{id}/interjections?after_rev=R  增量列出（按 rev 升序，至多 200 行）
//
// 插话不扩展 GET /events：插话行的状态会在事后变化（queued → delivered/failed），
// 而 /events 的游标是 agent_events.id，插话行的变化不会让它前进。

// interjectPageSize 是 /interjections 每页上限（实现参数，不是运维旋钮）。
const interjectPageSize = 200

// Interjector 把一条插话落库并尝试投递（实现在 runner：注册表 + store + 脱敏）。
// 接口定义在这里是为了测试注入假件。
type Interjector interface {
	Submit(ctx context.Context, taskID, userID int64, body string) (runner.SubmitResult, error)
	ListSince(ctx context.Context, taskID int64, afterRev int64, limit int) ([]store.Interjection, error)
}

// InterjectLimits 是插话的长度与限流参数（来自 LATHE_INTERJECT_*，config 已校验）。
type InterjectLimits struct {
	MaxChars  int
	RateLimit int
	Window    time.Duration
}

func (l InterjectLimits) orDefault() InterjectLimits {
	if l.MaxChars <= 0 {
		l.MaxChars = 8192
	}
	if l.RateLimit <= 0 {
		l.RateLimit = 5
	}
	if l.Window <= 0 {
		l.Window = 10 * time.Second
	}
	return l
}

// interjectOwned 做属主判定：任务不存在或不是自己的 → 404（不用 403 暴露存在性）。
func (a *API) interjectOwned(w http.ResponseWriter, r *http.Request) (int64, int64, bool) {
	id, ok := pathID(w, r)
	if !ok {
		return 0, 0, false
	}
	tk, err := a.Tasks.Get(r.Context(), id)
	if err != nil || tk.UserID != CurrentUser(r).ID {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "任务不存在"})
		return 0, 0, false
	}
	return id, tk.UserID, true
}

func (a *API) interjectTask(w http.ResponseWriter, r *http.Request) {
	id, userID, ok := a.interjectOwned(w, r)
	if !ok {
		return
	}
	if a.Interjector == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "运行中插话未启用"})
		return
	}
	var body struct {
		Message string `json:"message"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	lim := a.InterjectLimits.orDefault()
	// 长度按 rune、按脱敏前的原文计 —— 人看到的就是这么多字。
	n := utf8.RuneCountInString(body.Message)
	if n == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "插话内容不能为空"})
		return
	}
	if n > lim.MaxChars {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"error": fmt.Sprintf("插话过长：%d 字，上限 %d 字", n, lim.MaxChars),
		})
		return
	}
	if !a.interjectLimiter().allow("task:"+strconv.FormatInt(id, 10), lim.RateLimit, lim.Window) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": fmt.Sprintf("插话太频繁：每 %v 至多 %d 条", lim.Window, lim.RateLimit),
		})
		return
	}

	res, err := a.Interjector.Submit(r.Context(), id, userID, body.Message)
	if err != nil {
		if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
			return // 客户端在落库前断开：事务已回滚，没有给出任何承诺
		}
		serverError(w, "插话失败", err)
		return
	}
	row := res.Row
	switch res.Outcome {
	case runner.OutcomeDelivered:
		writeJSON(w, http.StatusOK, map[string]any{
			"id": row.ID, "status": row.Status, "deliveredAt": row.DeliveredAt,
			"deliveryId": row.DeliveryID, "phase": row.Phase, "error": row.Error, "rev": row.Rev,
		})
	case runner.OutcomeQueued:
		writeJSON(w, http.StatusAccepted, map[string]any{
			"id": row.ID, "status": row.Status, "reason": "no_live_session", "rev": row.Rev,
		})
	case runner.OutcomeRejectedState:
		writeJSON(w, http.StatusConflict, map[string]any{"error": res.Message, "id": row.ID, "rev": row.Rev})
	case runner.OutcomeDriverUnsupported:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": res.Message, "id": row.ID, "reason": "driver_unsupported", "rev": row.Rev,
		})
	case runner.OutcomeBackpressure:
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": res.Message, "id": row.ID, "rev": row.Rev})
	default:
		serverError(w, "插话失败", fmt.Errorf("未知结局 %d", res.Outcome))
	}
}

func (a *API) listInterjections(w http.ResponseWriter, r *http.Request) {
	id, _, ok := a.interjectOwned(w, r)
	if !ok {
		return
	}
	if a.Interjector == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "运行中插话未启用"})
		return
	}
	var after int64
	if v := r.URL.Query().Get("after_rev"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "after_rev 必须是非负整数"})
			return
		}
		after = n
	}
	rows, err := a.Interjector.ListSince(r.Context(), id, after, interjectPageSize)
	if err != nil {
		serverError(w, "查询插话失败", err)
		return
	}
	rev := after // 空批原样回显：客户端游标永不后退
	if len(rows) > 0 {
		rev = rows[len(rows)-1].Rev
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"interjections": rows, "rev": rev, "more": len(rows) == interjectPageSize,
	})
}

func (a *API) interjectLimiter() *limiter {
	a.interjectMu.Lock()
	defer a.interjectMu.Unlock()
	if a.interjectLim == nil {
		a.interjectLim = newLimiter()
	}
	return a.interjectLim
}
