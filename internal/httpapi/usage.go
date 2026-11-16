package httpapi

// usage.go 用量统计接口（f03，docs/features/usage-stats.md §3.3）。
//
//	GET /api/usage            属主视图（只看自己名下）
//	GET /api/tasks/{id}/usage 任务详情页的逐次运行（非属主 404）
//	GET /api/admin/usage      管理员全局视图（RequireAdmin，普通成员 403）
//
// 不复用 /api/stats*：那是看板 5 秒轮询的端点，用量是决策视图。
// 非法参数一律 400 + 中文原因，不静默回落默认值 —— 静默降级会让一个写错的
// 面板长得像「没数据」。未知参数（包括 phase=）忽略，不 400：未知参数 400
// 会让前端加一个新参数就打挂旧后端的滚动升级。

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// usageDefaultDays 是不带 from 时回看的天数。
const usageDefaultDays = 30

// parseUsageTime 解析 RFC3339 或 YYYY-MM-DD（UTC 日界）。dateOnly 报告是否是后者。
func parseUsageTime(s string) (t time.Time, dateOnly bool, ok bool) {
	if v, err := time.Parse(time.RFC3339, s); err == nil {
		return v, false, true
	}
	if v, err := time.Parse("2006-01-02", s); err == nil {
		return v.UTC(), true, true
	}
	return time.Time{}, false, false
}

// parseUsageQuery 校验并解析 /api/usage 与 /api/admin/usage 共用的参数。
// 返回的 errMsg 非空时调用方回 400。repo 的属主校验由调用方做（admin 不校验）。
//
// 时间窗口是半开区间 [from, to)：RFC3339 的 to 即上界；YYYY-MM-DD 的 to 含当天
// （上界取次日 00:00 UTC）。
func parseUsageQuery(r *http.Request) (store.UsageQuery, string) {
	q := r.URL.Query()
	uq := store.UsageQuery{Group: "phase", Limit: store.UsageDefaultLimit}

	uq.To = time.Now().UTC()
	if s := strings.TrimSpace(q.Get("to")); s != "" {
		t, dateOnly, ok := parseUsageTime(s)
		if !ok {
			return uq, "to 不是合法时间（RFC3339 或 YYYY-MM-DD）"
		}
		if dateOnly {
			t = t.AddDate(0, 0, 1)
		}
		uq.To = t
	}
	uq.From = uq.To.AddDate(0, 0, -usageDefaultDays)
	if s := strings.TrimSpace(q.Get("from")); s != "" {
		t, _, ok := parseUsageTime(s)
		if !ok {
			return uq, "from 不是合法时间（RFC3339 或 YYYY-MM-DD）"
		}
		uq.From = t
	}
	if uq.To.Before(uq.From) {
		return uq, "to 早于 from"
	}

	if s := strings.TrimSpace(q.Get("repo")); s != "" {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil || id <= 0 {
			return uq, "repo 必须是仓库 id（正整数）"
		}
		uq.RepoID = id
	}
	if s := strings.TrimSpace(q.Get("tier")); s != "" {
		if !slices.Contains(store.UsageTiers, s) {
			return uq, "tier 只能是 light / heavy / none"
		}
		uq.Tier = s
	}
	if s := strings.TrimSpace(q.Get("bucket")); s != "" {
		if !slices.Contains(store.UsageBuckets, s) {
			return uq, "bucket 不是已知的阶段桶（" + strings.Join(store.UsageBuckets, " / ") + "）"
		}
		uq.Bucket = s
	}
	if s := strings.TrimSpace(q.Get("group")); s != "" {
		if !slices.Contains(store.UsageGroups, s) {
			return uq, "group 只能是 " + strings.Join(store.UsageGroups, " / ")
		}
		uq.Group = s
	}
	// limit 在任何 group 下都校验（只在 group=task 时生效）
	if s := strings.TrimSpace(q.Get("limit")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > store.UsageMaxLimit {
			return uq, "limit 必须是 1–" + strconv.Itoa(store.UsageMaxLimit) + " 的整数"
		}
		uq.Limit = n
	}
	return uq, ""
}

// usage 是属主视图：只看自己名下，按仓库筛选时非属主仓库与不存在同等 404。
func (a *API) usage(w http.ResponseWriter, r *http.Request) {
	uq, msg := parseUsageQuery(r)
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg})
		return
	}
	userID := CurrentUser(r).ID
	if uq.RepoID != 0 {
		if _, err := a.Store.GetRepo(r.Context(), uq.RepoID, userID); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "仓库不存在"})
			return
		}
	}
	us, err := a.Store.UsageStats(r.Context(), userID, uq)
	if err != nil {
		serverError(w, "用量统计失败", err)
		return
	}
	writeJSON(w, http.StatusOK, us)
}

// taskUsage 返回某任务的全部运行（不受时间范围限制，不接受筛选参数）。
// 非属主走 TaskDetail 的既有语义：与不存在同等 404。
func (a *API) taskUsage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	userID := CurrentUser(r).ID
	if _, err := a.Store.TaskDetail(r.Context(), id, userID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "任务不存在"})
		return
	}
	tu, err := a.Store.TaskUsageFor(r.Context(), id, userID)
	if err != nil {
		serverError(w, "任务用量查询失败", err)
		return
	}
	writeJSON(w, http.StatusOK, tu)
}

// adminUsage 是管理员全局视图：显式的跨用户口子，只返回聚合与 user_id，
// 不返回 email 与任何任务/PRD/仓库标签（结构上没有那些字段）。repo 参数
// 只做格式校验、不做属主校验。
func (a *API) adminUsage(w http.ResponseWriter, r *http.Request) {
	uq, msg := parseUsageQuery(r)
	if msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg})
		return
	}
	us, err := a.Store.AdminUsageStats(r.Context(), uq)
	if err != nil {
		serverError(w, "全局用量统计失败", err)
		return
	}
	writeJSON(w, http.StatusOK, us)
}
