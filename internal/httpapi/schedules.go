package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/linear"
	"github.com/zichuanwangcloud-gif/lathe/internal/schedule"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// SchedulesAPI 是定时建单的 HTTP 面（docs/features/scheduled-issue.md §3.5）。
//
// 全部按 CurrentUser 隔离：非属主与不存在一律 404（store 层 WHERE id AND user_id）。
// 规则到点只在 Linear 上建一张单，之后走现有接单流程；本 API 不碰 tasks。
type SchedulesAPI struct {
	Store *store.Store
	Auth  *Auth
	// Fire 是「立即运行」的执行者（生产上是 *schedule.Runner，与后台调度器同一个实例）。
	Fire ScheduleFirer
	// SchedulerEnabled 为 false（LATHE_SCHEDULER_ENABLED=false）时「立即运行」返回 503；
	// 其余接口照常可用（可以先配规则）。
	SchedulerEnabled bool
	// Preconditions 检查接单前提（凭据、webhook 密钥、Linear 用户、系统标签）。
	Preconditions ScheduleIntake
	// Now 为 nil 时取 time.Now（测试注入固定时钟）。
	Now func() time.Time
}

// ScheduleFirer 是「立即运行」的窄接口。
type ScheduleFirer interface {
	FireNow(ctx context.Context, scheduleID, userID int64) (*store.ScheduleRunRow, error)
}

// ScheduleLinear 是保存校验时对 Linear 的只读需求，*linear.Client 满足它。
type ScheduleLinear interface {
	Teams(ctx context.Context) ([]linear.Team, error)
	LabelIDByName(ctx context.Context, teamID, name string) (string, error)
}

// ScheduleIntake 回答「这条规则建出来的单会不会被接」。走注入而不是直接依赖
// creds：creds 已经依赖本包，反过来 import 会成环（同 LinearClientFor 的理由）。
type ScheduleIntake interface {
	// Linear 取属主的 Linear 客户端；凭据不可用返回错误。
	Linear(ctx context.Context, userID int64) (ScheduleLinear, error)
	// WebhookSecretConfigured 报告属主是否配了 Linear webhook 签名密钥 —— 没配，单建出来也不会被接。
	WebhookSecretConfigured(ctx context.Context, userID int64) bool
	// LinearUserID 是「指派给我」用的 Linear 用户 ID（验证令牌时落库）。
	LinearUserID(ctx context.Context, userID int64) string
	// TriggerLabel 是系统设置里的接单标签；空串 = 标签接单未开启。
	TriggerLabel(ctx context.Context) string
}

// Routes 注册定时建单接口。
func (a *SchedulesAPI) Routes(mux *http.ServeMux) {
	mux.Handle("GET /api/schedules", a.Auth.RequireFunc(a.list))
	mux.Handle("POST /api/schedules", a.Auth.RequireFunc(a.create))
	mux.Handle("POST /api/schedules/preview", a.Auth.RequireFunc(a.preview))
	mux.Handle("GET /api/schedules/{id}", a.Auth.RequireFunc(a.get))
	mux.Handle("PUT /api/schedules/{id}", a.Auth.RequireFunc(a.update))
	mux.Handle("DELETE /api/schedules/{id}", a.Auth.RequireFunc(a.remove))
	mux.Handle("POST /api/schedules/{id}/run", a.Auth.RequireFunc(a.runNow))
	mux.Handle("GET /api/schedules/{id}/runs", a.Auth.RequireFunc(a.runs))
}

func (a *SchedulesAPI) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// scheduleView 是规则行加上给人读的描述。
type scheduleView struct {
	store.ScheduleRow
	Description string `json:"description"`
}

func viewOf(sc *store.ScheduleRow) scheduleView {
	v := scheduleView{ScheduleRow: *sc}
	if spec, err := schedule.SpecOf(sc); err == nil {
		v.Description = spec.Describe()
	}
	return v
}

func scheduleNotFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{"error": "定时规则不存在"})
}

func (a *SchedulesAPI) list(w http.ResponseWriter, r *http.Request) {
	rows, err := a.Store.ListSchedules(r.Context(), CurrentUser(r).ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]scheduleView, 0, len(rows))
	for i := range rows {
		out = append(out, viewOf(&rows[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": out})
}

func (a *SchedulesAPI) get(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	sc, err := a.Store.GetSchedule(r.Context(), id, CurrentUser(r).ID)
	if err != nil {
		scheduleNotFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedule": viewOf(sc)})
}

func (a *SchedulesAPI) remove(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := a.Store.DeleteSchedule(r.Context(), id, CurrentUser(r).ID); err != nil {
		if errors.Is(err, store.ErrScheduleNotFound) {
			scheduleNotFound(w)
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// scheduleBody 是 POST/PUT 的请求体（camelCase，与响应一致）。
type scheduleBody struct {
	Name            string     `json:"name"`
	Enabled         *bool      `json:"enabled"`
	Kind            string     `json:"kind"`
	MinuteOfDay     *int       `json:"minuteOfDay"`
	Weekdays        *int       `json:"weekdays"`
	IntervalMinutes *int       `json:"intervalMinutes"`
	AnchorAt        *time.Time `json:"anchorAt"`
	Timezone        string     `json:"timezone"`
	LinearTeamID    string     `json:"linearTeamId"`
	IntakeMode      string     `json:"intakeMode"`
	TitleTemplate   string     `json:"titleTemplate"`
	BodyTemplate    string     `json:"bodyTemplate"`
	Priority        int        `json:"priority"`
}

// httpErr 是带状态码的校验错误。
type httpErr struct {
	code int
	msg  string
}

func (e *httpErr) Error() string { return e.msg }

func bad(format string, args ...any) *httpErr {
	return &httpErr{code: http.StatusBadRequest, msg: fmt.Sprintf(format, args...)}
}

// spec 把请求体里的时间定义还原成 Spec 并做形状校验；同时返回落库用的规范化字段。
func (b *scheduleBody) spec(now time.Time) (schedule.Spec, store.ScheduleParams, *httpErr) {
	var p store.ScheduleParams
	tz := strings.TrimSpace(b.Timezone)
	if tz == "" {
		tz = "UTC"
	}
	loc, err := schedule.LoadLocation(tz)
	if err != nil {
		return schedule.Spec{}, p, bad("%s", err.Error())
	}
	s := schedule.Spec{Kind: b.Kind, Loc: loc}
	p.Kind, p.Timezone = b.Kind, tz
	switch b.Kind {
	case schedule.KindDaily, schedule.KindWeekly:
		if b.MinuteOfDay == nil {
			return s, p, bad("缺少每日时刻 minuteOfDay（0–1439）")
		}
		s.MinuteOfDay, p.MinuteOfDay = *b.MinuteOfDay, b.MinuteOfDay
		if b.Kind == schedule.KindWeekly {
			if b.Weekdays == nil {
				return s, p, bad("每周规则至少选一天（weekdays）")
			}
			s.Weekdays, p.Weekdays = *b.Weekdays, b.Weekdays
		}
	case schedule.KindInterval:
		if b.IntervalMinutes == nil {
			return s, p, bad("缺少间隔 intervalMinutes")
		}
		s.IntervalMinutes, p.IntervalMinutes = *b.IntervalMinutes, b.IntervalMinutes
		anchor := now.Truncate(time.Hour)
		if b.AnchorAt != nil {
			anchor = b.AnchorAt.Truncate(time.Minute)
		}
		s.Anchor, p.AnchorAt = anchor, &anchor
	}
	if err := s.Validate(); err != nil {
		return s, p, bad("%s", err.Error())
	}
	return s, p, nil
}

// validate 做全部保存校验，返回落库参数。existing 非 nil 表示更新。
func (a *SchedulesAPI) validate(ctx context.Context, uid int64, b *scheduleBody, existing *store.ScheduleRow) (store.ScheduleParams, *httpErr) {
	now := a.now()
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" {
		return store.ScheduleParams{}, bad("规则名不能为空")
	}
	if n := utf8.RuneCountInString(b.Name); n > schedule.MaxNameRunes {
		return store.ScheduleParams{}, bad("规则名过长（got %d 个字符, want ≤%d）", n, schedule.MaxNameRunes)
	}
	spec, p, herr := b.spec(now)
	if herr != nil {
		return p, herr
	}
	if strings.TrimSpace(b.TitleTemplate) == "" {
		return p, bad("标题模板不能为空")
	}
	if n := utf8.RuneCountInString(b.TitleTemplate); n > schedule.MaxTitleTplRunes {
		return p, bad("标题模板过长（got %d 个字符, want ≤%d）", n, schedule.MaxTitleTplRunes)
	}
	if n := utf8.RuneCountInString(b.BodyTemplate); n > schedule.MaxBodyTplRunes {
		return p, bad("正文模板过长（got %d 个字符, want ≤%d）", n, schedule.MaxBodyTplRunes)
	}
	for _, tpl := range []string{b.TitleTemplate, b.BodyTemplate} {
		if err := schedule.ValidateTemplate(tpl); err != nil {
			return p, bad("%s", err.Error())
		}
	}
	if b.Priority < 0 || b.Priority > 4 {
		return p, bad("优先级必须是 0–4（got %d）", b.Priority)
	}
	if b.IntakeMode == "" {
		b.IntakeMode = "assign"
	}
	if b.IntakeMode != "assign" && b.IntakeMode != "label" {
		return p, bad("接单方式必须是 assign（指派给我）或 label（打接单标签），got %q", b.IntakeMode)
	}
	if strings.TrimSpace(b.LinearTeamID) == "" {
		return p, bad("必须选择 Linear 团队")
	}
	enabled := b.Enabled == nil || *b.Enabled

	p.Name, p.Enabled, p.LinearTeamID, p.IntakeMode = b.Name, enabled, b.LinearTeamID, b.IntakeMode
	p.TitleTemplate, p.BodyTemplate, p.Priority, p.Now = b.TitleTemplate, b.BodyTemplate, b.Priority, now
	if existing != nil && existing.LinearTeamID == b.LinearTeamID {
		p.LinearTeamKey = existing.LinearTeamKey
	}

	if enabled {
		key, herr := a.checkIntake(ctx, uid, b.IntakeMode, b.LinearTeamID)
		if herr != nil {
			return p, herr
		}
		p.LinearTeamKey = key
		next := spec.Next(now)
		p.NextFireAt = &next
	}
	return p, nil
}

// checkIntake 校验接单前提：缺了就 400 并说明怎么修；Linear 不可达 502（不保存）。
func (a *SchedulesAPI) checkIntake(ctx context.Context, uid int64, mode, teamID string) (string, *httpErr) {
	if a.Preconditions == nil {
		return "", &httpErr{code: http.StatusServiceUnavailable, msg: "接单前提检查未装配"}
	}
	lc, err := a.Preconditions.Linear(ctx, uid)
	if err != nil {
		return "", bad("Linear 凭据不可用，请先到「设置」页配置并验证（%s）", err.Error())
	}
	if !a.Preconditions.WebhookSecretConfigured(ctx, uid) {
		return "", bad("尚未配置 Linear webhook 签名密钥：定时建出来的单不会被接，请先到「设置」页配置 webhook")
	}
	label := ""
	if mode == "assign" {
		if a.Preconditions.LinearUserID(ctx, uid) == "" {
			return "", bad("未取得你的 Linear 用户 ID，无法指派给你：请到「设置」页重新验证 Linear 令牌")
		}
	} else {
		label = a.Preconditions.TriggerLabel(ctx)
		if label == "" {
			return "", bad("管理员未开启标签接单（系统设置的接单标签为空），请改用「指派给我」")
		}
	}
	teams, err := lc.Teams(ctx)
	if err != nil {
		return "", &httpErr{code: http.StatusBadGateway, msg: "Linear 不可达，未保存：" + err.Error()}
	}
	key, found := "", false
	for _, t := range teams {
		if t.ID == teamID {
			key, found = t.Key, true
		}
	}
	if !found {
		return "", bad("Linear 团队 %q 不在你可见的团队里", teamID)
	}
	if mode == "label" {
		if _, err := lc.LabelIDByName(ctx, teamID, label); err != nil {
			if errors.Is(err, linear.ErrLabelNotFound) {
				return "", bad("团队里找不到接单标签「%s」，请先在 Linear 建好这个标签", label)
			}
			return "", &httpErr{code: http.StatusBadGateway, msg: "Linear 不可达，未保存：" + err.Error()}
		}
	}
	return key, nil
}

func writeHTTPErr(w http.ResponseWriter, e *httpErr) {
	writeJSON(w, e.code, map[string]any{"error": e.msg})
}

func (a *SchedulesAPI) create(w http.ResponseWriter, r *http.Request) {
	var b scheduleBody
	if err := decodeJSON(r, &b); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	uid := CurrentUser(r).ID
	n, err := a.Store.CountSchedules(r.Context(), uid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if n >= schedule.MaxSchedulesPerUser {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("每人最多 %d 条定时规则（已有 %d 条）", schedule.MaxSchedulesPerUser, n)})
		return
	}
	p, herr := a.validate(r.Context(), uid, &b, nil)
	if herr != nil {
		writeHTTPErr(w, herr)
		return
	}
	sc, err := a.Store.CreateSchedule(r.Context(), uid, p)
	if err != nil {
		a.writeSaveErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"schedule": viewOf(sc)})
}

func (a *SchedulesAPI) update(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	uid := CurrentUser(r).ID
	existing, err := a.Store.GetSchedule(r.Context(), id, uid)
	if err != nil {
		scheduleNotFound(w)
		return
	}
	var b scheduleBody
	if err := decodeJSON(r, &b); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	p, herr := a.validate(r.Context(), uid, &b, existing)
	if herr != nil {
		writeHTTPErr(w, herr)
		return
	}
	sc, err := a.Store.UpdateSchedule(r.Context(), id, uid, p)
	if err != nil {
		a.writeSaveErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedule": viewOf(sc)})
}

func (a *SchedulesAPI) writeSaveErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrScheduleNameTaken):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "已有同名定时规则"})
	case errors.Is(err, store.ErrScheduleNotFound):
		scheduleNotFound(w)
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
	}
}

// runNow：POST /api/schedules/{id}/run —— 先属主校验（404），再判调度器开关（503），
// 顺序不能反：503 不能泄露别人规则的存在性。
func (a *SchedulesAPI) runNow(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	uid := CurrentUser(r).ID
	if _, err := a.Store.GetSchedule(r.Context(), id, uid); err != nil {
		scheduleNotFound(w)
		return
	}
	if !a.SchedulerEnabled || a.Fire == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "调度器已关闭（LATHE_SCHEDULER_ENABLED=false）"})
		return
	}
	run, err := a.Fire.FireNow(r.Context(), id, uid)
	switch {
	case errors.Is(err, store.ErrRunSlotTaken):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "这一分钟已经触发过一次，请稍后再试"})
	case errors.Is(err, store.ErrScheduleNotFound):
		scheduleNotFound(w)
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
	default:
		writeJSON(w, http.StatusAccepted, map[string]any{"run": run})
	}
}

func (a *SchedulesAPI) runs(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	uid := CurrentUser(r).ID
	if _, err := a.Store.GetSchedule(r.Context(), id, uid); err != nil {
		scheduleNotFound(w)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := a.Store.ListScheduleRuns(r.Context(), id, uid, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": rows})
}

// preview：POST /api/schedules/preview —— 无副作用，返回接下来 5 个时刻与人读描述。
func (a *SchedulesAPI) preview(w http.ResponseWriter, r *http.Request) {
	var b scheduleBody
	if err := decodeJSON(r, &b); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	now := a.now()
	spec, _, herr := b.spec(now)
	if herr != nil {
		writeHTTPErr(w, herr)
		return
	}
	next := make([]time.Time, 0, 5)
	for t := now; len(next) < 5; {
		t = spec.Next(t)
		if t.IsZero() {
			break
		}
		next = append(next, t)
	}
	writeJSON(w, http.StatusOK, map[string]any{"description": spec.Describe(), "next": next})
}
