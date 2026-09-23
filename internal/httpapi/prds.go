package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/zichuanwangcloud-gif/lathe/internal/prd"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// PRDAPI 是模糊任务规划（docs/10）的 HTTP 面。
//
// 职责分工与 FlowAPI 一致：本文件只做「校验入参 → 调 service → 映射错误
// 成状态码」，任何判断 PRD 好坏的逻辑都在 prd 包里（自检在 checklist.go、
// 转移合法性在 state.go）。这里绝不自己判「这份 PRD 能不能定稿」。
type PRDAPI struct {
	Store *store.Store
	Auth  *Auth

	// Planner 跑规划对话、对抗复核、定稿自检与一键生成。为 nil 时这几个
	// 端点返回 503，只读端点照常工作 —— 测试装配可以不接 agent。
	Planner PRDPlanner
}

// PRDPlanner 是本包对规划管线的窄需求（*prd.Service 实现它）。
//
// 定成接口而非直接吃 *prd.Service：httpapi 的测试不该为了测一条路由
// 就真起一个 agent 子进程去读仓库。
type PRDPlanner interface {
	RunRound(ctx context.Context, p prd.RunRoundParams) (*prd.RoundResult, error)
	RunReview(ctx context.Context, prdID, userID int64) (*prd.ReviewReport, error)
	SubmitForReview(ctx context.Context, prdID, userID int64) (*prd.Report, error)
	Convert(ctx context.Context, prdID, userID int64) (*prd.FlowResult, error)
}

// Routes 注册 PRD 接口。全部按 CurrentUser 隔离：非属主一律 404。
func (a *PRDAPI) Routes(mux *http.ServeMux) {
	mux.Handle("GET /api/prds", a.Auth.RequireFunc(a.list))
	mux.Handle("POST /api/prds", a.Auth.RequireFunc(a.create))
	mux.Handle("GET /api/prds/{id}", a.Auth.RequireFunc(a.get))
	mux.Handle("GET /api/prds/{id}/rounds", a.Auth.RequireFunc(a.listRounds))
	mux.Handle("GET /api/prds/{id}/events", a.Auth.RequireFunc(a.events))
	mux.Handle("GET /api/prds/{id}/export", a.Auth.RequireFunc(a.export))

	mux.Handle("POST /api/prds/{id}/rounds", a.Auth.RequireFunc(a.runRound))
	mux.Handle("POST /api/prds/{id}/review", a.Auth.RequireFunc(a.review))
	mux.Handle("PUT /api/prds/{id}/review/dispositions", a.Auth.RequireFunc(a.setDispositions))
	mux.Handle("POST /api/prds/{id}/sections/{key}/confirm", a.Auth.RequireFunc(a.confirmSection))
	mux.Handle("POST /api/prds/{id}/submit", a.Auth.RequireFunc(a.submit))
	mux.Handle("POST /api/prds/{id}/approve", a.Auth.RequireFunc(a.approve))
	mux.Handle("POST /api/prds/{id}/reject", a.Auth.RequireFunc(a.reject))
	mux.Handle("POST /api/prds/{id}/abandon", a.Auth.RequireFunc(a.abandon))
	mux.Handle("POST /api/prds/{id}/convert", a.Auth.RequireFunc(a.convert))
}

// list：GET /api/prds?state=&limit=&offset=
func (a *PRDAPI) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := strings.TrimSpace(q.Get("state"))
	if state != "" && !prd.State(state).Valid() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state 取值非法"})
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	rows, total, err := a.Store.ListPRDs(r.Context(), store.ListPRDsParams{
		UserID: CurrentUser(r).ID,
		State:  state,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		serverError(w, "查询 PRD 列表失败", err)
		return
	}
	// rows 可能是 nil 切片，显式兜成空数组 —— 前端拿到 null 要多写一处判空。
	if rows == nil {
		rows = []store.PRDRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"prds": rows, "total": total})
}

// create：POST /api/prds  {repoId, prdType, originalInput, refPrdId?}
//
// 一律从 drafting 起步（store.CreatePRD 不接受初始状态），建完不自动跑
// 第一轮 —— 起轮要花钱、要占 worktree，该由人显式点。
func (a *PRDAPI) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RepoID        int64  `json:"repoId"`
		PRDType       string `json:"prdType"`
		OriginalInput string `json:"originalInput"`
		RefPRDID      *int64 `json:"refPrdId"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}

	body.OriginalInput = strings.TrimSpace(body.OriginalInput)
	if body.OriginalInput == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "原始需求不能为空"})
		return
	}
	if !prd.Type(body.PRDType).Valid() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "prdType 必须是 defect / feature / refactor"})
		return
	}

	userID := CurrentUser(r).ID
	// 仓库归属必须在这里查 —— store.CreatePRD 只靠外键兜底，不校验属主
	// （它的文档注释写明了这一点）。少了这一查，别人的 repoId 也能建 PRD。
	if _, err := a.Store.GetRepo(r.Context(), body.RepoID, userID); err != nil {
		if errors.Is(err, store.ErrRepoNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "仓库不存在"})
			return
		}
		serverError(w, "查询仓库失败", err)
		return
	}

	row, err := a.Store.CreatePRD(r.Context(), store.CreatePRDParams{
		UserID:        userID,
		RepoID:        body.RepoID,
		PRDType:       body.PRDType,
		OriginalInput: body.OriginalInput,
		RefPRDID:      body.RefPRDID,
	})
	if err != nil {
		serverError(w, "创建 PRD 失败", err)
		return
	}
	writeJSON(w, http.StatusCreated, row)
}

// get：GET /api/prds/{id}，返回含 document / taskBlocks / reviewReport 的全量。
func (a *PRDAPI) get(w http.ResponseWriter, r *http.Request) {
	id, ok := prdPathID(w, r)
	if !ok {
		return
	}
	row, err := a.Store.GetPRD(r.Context(), id, CurrentUser(r).ID)
	if err != nil {
		writePRDError(w, "查询 PRD 失败", err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

// listRounds：GET /api/prds/{id}/rounds —— 附录 A 的对话留痕。
func (a *PRDAPI) listRounds(w http.ResponseWriter, r *http.Request) {
	id, ok := prdPathID(w, r)
	if !ok {
		return
	}
	// 先确认属主，再列轮次：ListPRDRounds 只按 prd_id 查，没有属主条件。
	if _, err := a.Store.GetPRD(r.Context(), id, CurrentUser(r).ID); err != nil {
		writePRDError(w, "查询 PRD 失败", err)
		return
	}
	rounds, err := a.Store.ListPRDRounds(r.Context(), id)
	if err != nil {
		serverError(w, "查询对话轮次失败", err)
		return
	}
	if rounds == nil {
		rounds = []store.PRDRoundRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rounds": rounds})
}

// events：GET /api/prds/{id}/events?after=&limit= —— 规划与复核的事件流。
func (a *PRDAPI) events(w http.ResponseWriter, r *http.Request) {
	id, ok := prdPathID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))

	userID := CurrentUser(r).ID
	// 先判归属再查事件：PRDEventsAfter 的 JOIN 让「别人的 PRD」与「没有
	// 事件」不可区分（它的文档注释写明归属判定归 API 层），少了这一步
	// 非属主会拿到 200 + 空数组，而同一份 PRD 的详情与轮次都回 404 ——
	// 三个端点对同一个问题给出不同答案，本身就是一条信息。
	if _, err := a.Store.GetPRD(r.Context(), id, userID); err != nil {
		writePRDError(w, "查询 PRD 失败", err)
		return
	}

	events, lastID, err := a.Store.PRDEventsAfter(r.Context(), id, userID, after, limit)
	if err != nil {
		writePRDError(w, "查询事件流失败", err)
		return
	}
	if events == nil {
		events = []store.PRDEvent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "lastId": lastID})
}

// runRound：POST /api/prds/{id}/rounds  {answer?, stage?}
//
// 返回跑完后的完整 PRD 行，而不只是本轮增量：一轮跑完 state / round /
// document / taskBlocks 可能全变了，前端与其拼增量不如直接换一份。
func (a *PRDAPI) runRound(w http.ResponseWriter, r *http.Request) {
	id, ok := prdPathID(w, r)
	if !ok {
		return
	}
	if a.Planner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "规划管线未启用"})
		return
	}

	var body struct {
		Answer string `json:"answer"`
		Stage  string `json:"stage"`
	}
	// 空 body 合法（首轮没有回答），所以解码失败只在有内容且不是 JSON 时才算错。
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
			return
		}
	}
	if body.Stage != "" && !prd.Stage(body.Stage).Valid() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "stage 取值非法"})
		return
	}

	userID := CurrentUser(r).ID
	res, err := a.Planner.RunRound(r.Context(), prd.RunRoundParams{
		PRDID:  id,
		UserID: userID,
		Answer: strings.TrimSpace(body.Answer),
		Stage:  prd.Stage(body.Stage),
	})
	if err != nil {
		writePRDError(w, "跑规划轮次失败", err)
		return
	}

	row, err := a.Store.GetPRD(r.Context(), id, userID)
	if err != nil {
		writePRDError(w, "查询 PRD 失败", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"prd":       row,
		"questions": nonNilStrings(res.Questions),
		"notes":     res.Notes,
	})
}

// review：POST /api/prds/{id}/review —— 对抗复核（§5.5）。
//
// 报告不带处置：Disposition 由人在界面里逐条填，让复核自己给自己判
// 「这条不用改」等于没复核（docs/10 §5.5 的原话）。
func (a *PRDAPI) review(w http.ResponseWriter, r *http.Request) {
	id, ok := prdPathID(w, r)
	if !ok {
		return
	}
	if a.Planner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "规划管线未启用"})
		return
	}
	report, err := a.Planner.RunReview(r.Context(), id, CurrentUser(r).ID)
	if err != nil {
		writePRDError(w, "对抗复核失败", err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// submit：POST /api/prds/{id}/submit —— 提交评审前跑定稿自检。
//
// 自检不过返回 422 + 完整报告（不是 400）：请求本身没毛病，是这份 PRD
// 当前内容不满足定稿条件，前端要按 Finding.Code 逐条跳到出问题那一节。
// 失败即拒绝、不自动补占位符 —— 补了只会让洞通过检查却仍然存在。
func (a *PRDAPI) submit(w http.ResponseWriter, r *http.Request) {
	id, ok := prdPathID(w, r)
	if !ok {
		return
	}
	if a.Planner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "规划管线未启用"})
		return
	}

	report, err := a.Planner.SubmitForReview(r.Context(), id, CurrentUser(r).ID)
	if err != nil {
		writePRDError(w, "提交评审失败", err)
		return
	}
	if report != nil && !report.OK() {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "定稿自检未通过",
			"report": report,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state":  string(prd.StateReadyForReview),
		"report": report,
	})
}

// approve：POST /api/prds/{id}/approve —— 人签字，PRD 冻结（D10-4）。
func (a *PRDAPI) approve(w http.ResponseWriter, r *http.Request) {
	a.transition(w, r, prd.StateApproved, "批准 PRD 失败")
}

// reject：POST /api/prds/{id}/reject —— 打回起草。
//
// 不收「打回理由」字段：理由的正确归宿是下一轮对话的回答（人点打回后
// 接着跑一轮，把要改什么说给规划智能体），而不是一个没人读的备注列。
// prd_rounds 是 append-only 的轮次记录，打回不是一轮对话，硬塞进去会
// 让轮次编号与真实对话对不上。
func (a *PRDAPI) reject(w http.ResponseWriter, r *http.Request) {
	a.transition(w, r, prd.StateDrafting, "打回 PRD 失败")
}

// abandon：POST /api/prds/{id}/abandon —— 人放弃，终态。
func (a *PRDAPI) abandon(w http.ResponseWriter, r *http.Request) {
	a.transition(w, r, prd.StateAbandoned, "放弃 PRD 失败")
}

// transition 跑一次状态转移：读当前态 → 查转移表 → CAS 落库。
//
// 合法性由 prd.Validate 判（store.TransitionPRD 的文档注释写明它只做 CAS
// 不查转移表），非法转移返回 409 而不是 400 —— 请求没错，是资源当前状态
// 不允许，与任务状态机的既有做法一致。
func (a *PRDAPI) transition(w http.ResponseWriter, r *http.Request, to prd.State, failMsg string) {
	id, ok := prdPathID(w, r)
	if !ok {
		return
	}
	userID := CurrentUser(r).ID

	row, err := a.Store.GetPRD(r.Context(), id, userID)
	if err != nil {
		writePRDError(w, failMsg, err)
		return
	}
	from := prd.State(row.State)
	if err := prd.Validate(from, to); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}

	updated, err := a.Store.TransitionPRD(r.Context(), id, userID, store.TransitionPRDParams{
		From: string(from),
		To:   string(to),
	})
	if err != nil {
		writePRDError(w, failMsg, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// convert：POST /api/prds/{id}/convert —— 一键生成任务与编排图（§4.3）。
func (a *PRDAPI) convert(w http.ResponseWriter, r *http.Request) {
	id, ok := prdPathID(w, r)
	if !ok {
		return
	}
	if a.Planner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "规划管线未启用"})
		return
	}
	res, err := a.Planner.Convert(r.Context(), id, CurrentUser(r).ID)
	if err != nil {
		writePRDError(w, "一键生成任务失败", err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// prdPathID 取路径里的 PRD id。
func prdPathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "PRD ID 非法"})
		return 0, false
	}
	return id, true
}

// writePRDError 把 prd / store 的错误映射成状态码。
//
// 分档依据：不存在与非属主都走 404（不用状态码暴露资源存在性，安全
// 红线的既定做法）；「当前状态/占用不允许」走 409；装配缺失走 503；
// 其余当服务端错误。
func writePRDError(w http.ResponseWriter, msg string, err error) {
	switch {
	case errors.Is(err, store.ErrPRDNotFound), errors.Is(err, store.ErrRepoNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "PRD 不存在"})
	case errors.Is(err, store.ErrPRDBusy):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "该 PRD 正有一轮对话在进行中，稍后再试"})
	case errors.Is(err, store.ErrPRDStateChanged):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "PRD 状态已被并发修改，请刷新后重试"})
	case errors.Is(err, store.ErrPRDFrozen):
		writeJSON(w, http.StatusConflict, map[string]any{"error": store.ErrPRDFrozen.Error()})
	case errors.Is(err, prd.ErrStageUnavailable),
		errors.Is(err, prd.ErrNotApproved),
		errors.Is(err, prd.ErrAlreadyConverted),
		errors.Is(err, prd.ErrNoTasks):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
	case errors.Is(err, prd.ErrConvertUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
	default:
		var illegal prd.ErrIllegalTransition
		if errors.As(err, &illegal) {
			writeJSON(w, http.StatusConflict, map[string]any{"error": illegal.Error()})
			return
		}
		serverError(w, msg, err)
	}
}

// nonNilStrings 把 nil 切片兜成空数组，省掉前端一处判空。
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// 编译期断言：*prd.Service 满足本包对规划管线的需求。
var _ PRDPlanner = (*prd.Service)(nil)

// confirmSection：POST /api/prds/{id}/sections/{key}/confirm
//
// 把一节从 🤖 推断转成 ✅ 已确认（docs/10 §6.3 的人判档）。
//
// 这是定稿路上绕不开的一步：自检的 CodeUnconfirmed 会拦住任何还留着
// 🤖 的 PRD，而智能体自己不能把自己的推断标成「人确认过」—— 那就是
// 自己给自己签字。所以确认只能从这个端点进来。
//
// ❓ 待答的节不能直接确认：挂着未决问题就说明还没答，跳过去确认等于
// 把 §9 的未决问题清零机制绕开了。
func (a *PRDAPI) confirmSection(w http.ResponseWriter, r *http.Request) {
	id, ok := prdPathID(w, r)
	if !ok {
		return
	}
	key := strings.TrimSpace(r.PathValue("key"))
	userID := CurrentUser(r).ID

	row, err := a.Store.GetPRD(r.Context(), id, userID)
	if err != nil {
		writePRDError(w, "查询 PRD 失败", err)
		return
	}
	doc, err := prd.ParseDocument(row.Document)
	if err != nil {
		serverError(w, "解析 PRD 快照失败", err)
		return
	}

	var target *prd.Section
	for _, s := range doc.Sections() {
		if s.Key == key {
			target = s.Section
			break
		}
	}
	if target == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未知的节：" + key})
		return
	}
	if target.Status == prd.StatusPending {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "本节还挂着未决问题，先在对话里答完再确认",
		})
		return
	}
	target.Status = prd.StatusConfirmed

	raw, err := doc.Marshal()
	if err != nil {
		serverError(w, "序列化 PRD 快照失败", err)
		return
	}
	updated, err := a.Store.UpdatePRDContent(r.Context(), id, userID,
		store.UpdatePRDContentParams{Document: raw})
	if err != nil {
		writePRDError(w, "确认小节失败", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// setDispositions：PUT /api/prds/{id}/review/dispositions
// body: {findings: [{index, disposition, dispositionNote}]}
//
// 逐条填对抗复核发现的处置。发现按**下标**定位而不是 id —— ReviewFinding
// 本来就没有 id 字段，复核报告是 append-once 的整体（跑一次复核覆盖一份
// 报告），下标在一份报告内稳定。
//
// 处置由人填、agent 不产出（docs/10 §5.5）：让复核自己判「这条不用改」
// 等于没复核。校验规则在 prd.ValidateDisposition，不在这里。
func (a *PRDAPI) setDispositions(w http.ResponseWriter, r *http.Request) {
	id, ok := prdPathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Findings []struct {
			Index           int    `json:"index"`
			Disposition     string `json:"disposition"`
			DispositionNote string `json:"dispositionNote"`
		} `json:"findings"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体格式错误"})
		return
	}
	if len(body.Findings) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "findings 不能为空"})
		return
	}

	userID := CurrentUser(r).ID
	row, err := a.Store.GetPRD(r.Context(), id, userID)
	if err != nil {
		writePRDError(w, "查询 PRD 失败", err)
		return
	}
	if len(row.ReviewReport) == 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "这份 PRD 还没跑过对抗复核"})
		return
	}

	var report prd.ReviewReport
	if err := json.Unmarshal(row.ReviewReport, &report); err != nil {
		serverError(w, "解析对抗复核报告失败", err)
		return
	}

	// 先全量校验再落库：一条不合法就整批不写，避免「填了三条、第四条
	// 报错」留下半套处置，人还得回头数哪几条生效了。
	for _, f := range body.Findings {
		if f.Index < 0 || f.Index >= len(report.Findings) {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "findings 下标越界：" + strconv.Itoa(f.Index),
			})
			return
		}
		if err := prd.ValidateDisposition(f.Disposition, f.DispositionNote); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	}
	for _, f := range body.Findings {
		report.Findings[f.Index].Disposition = f.Disposition
		report.Findings[f.Index].DispositionNote = strings.TrimSpace(f.DispositionNote)
	}

	raw, err := json.Marshal(&report)
	if err != nil {
		serverError(w, "序列化对抗复核报告失败", err)
		return
	}
	updated, err := a.Store.UpdatePRDContent(r.Context(), id, userID,
		store.UpdatePRDContentParams{ReviewReport: raw})
	if err != nil {
		writePRDError(w, "保存处置失败", err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// export：GET /api/prds/{id}/export?format=md|json —— §7 导出。
//
// 两种格式在任何状态下都可用（approved 之后内容本就冻结，导出自然冻结）。
// Lathe 只把文件交给人，不自动写进目标仓库、更不 push —— 要不要提交进
// docs/ 是人的决定。
func (a *PRDAPI) export(w http.ResponseWriter, r *http.Request) {
	id, ok := prdPathID(w, r)
	if !ok {
		return
	}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "md"
	}
	if format != "md" && format != "json" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "format 只能是 md 或 json"})
		return
	}

	userID := CurrentUser(r).ID
	row, err := a.Store.GetPRD(r.Context(), id, userID)
	if err != nil {
		writePRDError(w, "查询 PRD 失败", err)
		return
	}
	in, err := a.exportInput(r.Context(), row, userID)
	if err != nil {
		serverError(w, "组装导出内容失败", err)
		return
	}

	// filename 只含 PRD 编号，不拼标题：标题带空格、斜杠与中文标点，
	// 拼进 Content-Disposition 要额外转义，而这个头是直接下发给浏览器的。
	w.Header().Set("Content-Disposition", `attachment; filename="`+prd.ExportFilename(id, format)+`"`)

	if format == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := json.NewEncoder(w).Encode(in); err != nil {
			// 头已经发出去了，这里只能记日志 —— 再写状态码会是
			// "superfluous WriteHeader" 且客户端已经开始收 body。
			slog.Error("写 PRD JSON 导出失败", "prd", id, "err", err)
		}
		return
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	if _, err := io.WriteString(w, prd.RenderMarkdown(in)); err != nil {
		slog.Error("写 PRD Markdown 导出失败", "prd", id, "err", err)
	}
}

// exportInput 把库里的行拼成渲染器要的材料。
//
// 解析 jsonb 放在这一层而不是 prd 包：渲染器只该管排版，不该知道轮次的
// questions 在库里是 jsonb 还是别的什么。
func (a *PRDAPI) exportInput(ctx context.Context, row *store.PRDRow, userID int64) (prd.ExportInput, error) {
	doc, err := prd.ParseDocument(row.Document)
	if err != nil {
		return prd.ExportInput{}, err
	}
	blocks, err := prd.ParseTaskBlocks(row.TaskBlocks)
	if err != nil {
		return prd.ExportInput{}, err
	}

	var review *prd.ReviewReport
	if len(row.ReviewReport) > 0 {
		var rep prd.ReviewReport
		if err := json.Unmarshal(row.ReviewReport, &rep); err != nil {
			return prd.ExportInput{}, err
		}
		review = &rep
	}

	in := prd.ExportInput{
		PRDID:         row.ID,
		Type:          prd.Type(row.PRDType),
		State:         row.State,
		OriginalInput: row.OriginalInput,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
		ApprovedAt:    row.ApprovedAt,
		ConvertedAt:   row.ConvertedAt,
		RefPRDID:      row.RefPRDID,
		FlowID:        row.GeneratedFlowID,
		Document:      doc,
		Blocks:        blocks,
		Review:        review,
	}

	// 仓库名取不到不算致命：导出件少一行元信息，比整个导出 500 要好。
	if repo, err := a.Store.GetRepo(ctx, row.RepoID, userID); err == nil {
		in.Repo = repo.ProviderRepo
	}

	rounds, err := a.Store.ListPRDRounds(ctx, row.ID)
	if err != nil {
		return prd.ExportInput{}, err
	}
	for _, rd := range rounds {
		s := prd.RoundSummary{
			Round:     rd.Round,
			Stage:     rd.Stage,
			UserInput: rd.UserInput,
			Notes:     rd.Notes,
			CreatedAt: rd.CreatedAt,
		}
		if len(rd.Questions) > 0 {
			// 问题清单坏了不该让整份导出失败：附录 A 少一轮的问题，
			// 比人拿不到 PRD 要好。
			if err := json.Unmarshal(rd.Questions, &s.Questions); err != nil {
				slog.Warn("解析轮次问题清单失败，该轮问题不进导出",
					"prd", row.ID, "round", rd.Round, "err", err)
			}
		}
		in.Rounds = append(in.Rounds, s)
	}
	return in, nil
}
