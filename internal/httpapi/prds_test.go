package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/prd"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// PRD 路由的测试。对着真库跑：这一层要验的是隔离（非属主 404）、状态
// 转移的闸门、以及两个写入口的校验 —— 都是 SQL 与 HTTP 交界处的事。
// 规划管线本身（跑 agent）不在这测，见 internal/prd 的内存测试。

// fakePlanner 是 PRDPlanner 的假件：不起 agent，只记调用并回可控结果。
type fakePlanner struct {
	roundCalls   int
	reviewCalls  int
	submitCalls  int
	convertCalls int

	roundErr   error
	submitRep  *prd.Report
	submitErr  error
	convertRes *prd.FlowResult
	convertErr error
}

func (f *fakePlanner) RunRound(context.Context, prd.RunRoundParams) (*prd.RoundResult, error) {
	f.roundCalls++
	if f.roundErr != nil {
		return nil, f.roundErr
	}
	return &prd.RoundResult{Questions: []string{"阈值上限该定多少？"}, Notes: "本轮问了一个问题"}, nil
}

func (f *fakePlanner) RunReview(context.Context, int64, int64) (*prd.ReviewReport, error) {
	f.reviewCalls++
	return &prd.ReviewReport{Verdict: "攻不动"}, nil
}

func (f *fakePlanner) SubmitForReview(context.Context, int64, int64) (*prd.Report, error) {
	f.submitCalls++
	if f.submitErr != nil {
		return nil, f.submitErr
	}
	if f.submitRep != nil {
		return f.submitRep, nil
	}
	return &prd.Report{Blocking: []prd.Finding{}, Warnings: []prd.Finding{}}, nil
}

func (f *fakePlanner) Convert(context.Context, int64, int64) (*prd.FlowResult, error) {
	f.convertCalls++
	if f.convertErr != nil {
		return nil, f.convertErr
	}
	if f.convertRes != nil {
		return f.convertRes, nil
	}
	return &prd.FlowResult{FlowID: 42, Tasks: []prd.CreatedTask{{ID: 1, IssueKey: "LT-1", State: "queued"}}}, nil
}

func prdFixture(t *testing.T) (*PRDAPI, *fakePlanner, *store.Store, int64, int64) {
	t.Helper()
	st := testStoreForAPI(t)
	userID := mustUser(t, st, "prds-"+t.Name()+"@example.com")

	// repo 名带随机量：被中断的上一轮会留下同 user_id 的孤儿行，
	// 定值 repo 名会直接撞唯一索引（§5.5）。
	var repoID int64
	if err := st.Pool().QueryRow(context.Background(),
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		userID, fmt.Sprintf("acme/prd-api-%s-%d", t.Name(), time.Now().UnixNano())).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}

	planner := &fakePlanner{}
	api := &PRDAPI{Store: st, Auth: authAs(userID, "prd-fixture@example.com"), Planner: planner}
	return api, planner, st, userID, repoID
}

func prdServer(t *testing.T, api *PRDAPI) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	api.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// mustPRD 直接建一份 PRD，跳过 HTTP（测转移/确认时不想每次都走建单流程）。
func mustPRD(t *testing.T, st *store.Store, userID, repoID int64) *store.PRDRow {
	t.Helper()
	row, err := st.CreatePRD(context.Background(), store.CreatePRDParams{
		UserID:        userID,
		RepoID:        repoID,
		PRDType:       string(prd.TypeFeature),
		OriginalInput: "预览阈值写死了，想让管理员能配",
	})
	if err != nil {
		t.Fatalf("建 PRD 失败: %v", err)
	}
	return row
}

func TestCreatePRDValidation(t *testing.T) {
	api, _, _, _, repoID := prdFixture(t)
	srv := prdServer(t, api)

	cases := []struct {
		name     string
		body     string
		wantCode int
	}{
		{
			name:     "原始需求为空要拒绝",
			body:     fmt.Sprintf(`{"repoId":%d,"prdType":"feature","originalInput":"   "}`, repoID),
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "未知类型要拒绝",
			body:     fmt.Sprintf(`{"repoId":%d,"prdType":"chore","originalInput":"随便改改"}`, repoID),
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "不存在的仓库返回 404",
			body:     `{"repoId":99999999,"prdType":"feature","originalInput":"随便改改"}`,
			wantCode: http.StatusNotFound,
		},
		{
			name:     "合法请求建单成功",
			body:     fmt.Sprintf(`{"repoId":%d,"prdType":"feature","originalInput":"预览阈值想可配"}`, repoID),
			wantCode: http.StatusCreated,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := doReq(t, srv, http.MethodPost, "/api/prds", tc.body)
			if resp.StatusCode != tc.wantCode {
				t.Errorf("状态码 got=%d want=%d，响应=%v", resp.StatusCode, tc.wantCode, body)
			}
			if tc.wantCode == http.StatusCreated && body["state"] != string(prd.StateDrafting) {
				t.Errorf("新建的 PRD 应从 drafting 起步，got=%v", body["state"])
			}
		})
	}
}

// 非属主访问返回 404 而不是 403：不用状态码暴露资源存在性（安全红线）。
func TestPRDHiddenFromNonOwner(t *testing.T) {
	_, _, st, userID, repoID := prdFixture(t)
	row := mustPRD(t, st, userID, repoID)

	otherID := mustUser(t, st, "prds-other-"+t.Name()+"@example.com")
	otherAPI := &PRDAPI{Store: st, Auth: authAs(otherID, "other@example.com"), Planner: &fakePlanner{}}
	srv := prdServer(t, otherAPI)

	for _, path := range []string{
		"/api/prds/" + strconv.FormatInt(row.ID, 10),
		"/api/prds/" + strconv.FormatInt(row.ID, 10) + "/rounds",
		"/api/prds/" + strconv.FormatInt(row.ID, 10) + "/events",
	} {
		t.Run(path, func(t *testing.T) {
			resp, _ := doReq(t, srv, http.MethodGet, path, "")
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("非属主访问 %s 应 404（不暴露存在性），got=%d", path, resp.StatusCode)
			}
		})
	}
}

func TestPRDTransitionGuards(t *testing.T) {
	api, _, st, userID, repoID := prdFixture(t)
	srv := prdServer(t, api)

	t.Run("drafting 直接批准要拒绝", func(t *testing.T) {
		row := mustPRD(t, st, userID, repoID)
		resp, body := doReq(t, srv, http.MethodPost,
			"/api/prds/"+strconv.FormatInt(row.ID, 10)+"/approve", "")
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("状态码 got=%d want=409（非法转移是状态冲突不是请求错误），响应=%v",
				resp.StatusCode, body)
		}
	})

	t.Run("drafting 可以放弃", func(t *testing.T) {
		row := mustPRD(t, st, userID, repoID)
		resp, body := doReq(t, srv, http.MethodPost,
			"/api/prds/"+strconv.FormatInt(row.ID, 10)+"/abandon", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 got=%d want=200，响应=%v", resp.StatusCode, body)
		}
		if body["state"] != string(prd.StateAbandoned) {
			t.Errorf("state got=%v want=%s", body["state"], prd.StateAbandoned)
		}
	})

	t.Run("终态不能再转", func(t *testing.T) {
		row := mustPRD(t, st, userID, repoID)
		id := strconv.FormatInt(row.ID, 10)
		if resp, _ := doReq(t, srv, http.MethodPost, "/api/prds/"+id+"/abandon", ""); resp.StatusCode != http.StatusOK {
			t.Fatalf("放弃失败：%d", resp.StatusCode)
		}
		resp, body := doReq(t, srv, http.MethodPost, "/api/prds/"+id+"/approve", "")
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("已放弃的 PRD 不能批准，got=%d 响应=%v", resp.StatusCode, body)
		}
	})
}

func TestConfirmSection(t *testing.T) {
	api, _, st, userID, repoID := prdFixture(t)
	srv := prdServer(t, api)

	// 造一份 §1 是 🤖 推断、§2 挂着 ❓ 的快照。
	doc := &prd.Document{
		Type:     prd.TypeFeature,
		OneLiner: prd.Section{Status: prd.StatusInferred, Body: "让管理员能配预览阈值"},
		Problem:  prd.Section{Status: prd.StatusPending, Questions: []string{"Q-1"}},
	}
	raw, err := doc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	row := mustPRD(t, st, userID, repoID)
	if _, err := st.UpdatePRDContent(context.Background(), row.ID, userID,
		store.UpdatePRDContentParams{Document: raw}); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(row.ID, 10)

	t.Run("推断节可以确认", func(t *testing.T) {
		resp, body := doReq(t, srv, http.MethodPost, "/api/prds/"+id+"/sections/oneLiner/confirm", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 got=%d want=200，响应=%v", resp.StatusCode, body)
		}
		got := prdSectionStatus(t, st, row.ID, userID, "oneLiner")
		if got != prd.StatusConfirmed {
			t.Errorf("§1 状态 got=%q want=%q", got, prd.StatusConfirmed)
		}
	})

	t.Run("待答节不许直接确认", func(t *testing.T) {
		resp, body := doReq(t, srv, http.MethodPost, "/api/prds/"+id+"/sections/problem/confirm", "")
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("挂着未决问题的节应 409（不能绕开 §9 的清零），got=%d 响应=%v",
				resp.StatusCode, body)
		}
		if got := prdSectionStatus(t, st, row.ID, userID, "problem"); got != prd.StatusPending {
			t.Errorf("被拒绝后 §2 状态不该变，got=%q", got)
		}
	})

	t.Run("未知节名要拒绝", func(t *testing.T) {
		resp, _ := doReq(t, srv, http.MethodPost, "/api/prds/"+id+"/sections/nosuch/confirm", "")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("未知节名应 400，got=%d", resp.StatusCode)
		}
	})
}

// prdSectionStatus 读回某一节当前的状态标记。
func prdSectionStatus(t *testing.T, st *store.Store, prdID, userID int64, key string) prd.SectionStatus {
	t.Helper()
	row, err := st.GetPRD(context.Background(), prdID, userID)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := prd.ParseDocument(row.Document)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range doc.Sections() {
		if s.Key == key {
			return s.Section.Status
		}
	}
	t.Fatalf("找不到节 %q", key)
	return ""
}

func TestSetDispositions(t *testing.T) {
	api, _, st, userID, repoID := prdFixture(t)
	srv := prdServer(t, api)

	row := mustPRD(t, st, userID, repoID)
	id := strconv.FormatInt(row.ID, 10)

	t.Run("没跑过复核时拒绝", func(t *testing.T) {
		resp, _ := doReq(t, srv, http.MethodPut, "/api/prds/"+id+"/review/dispositions",
			`{"findings":[{"index":0,"disposition":"fix"}]}`)
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("没有复核报告时应 409，got=%d", resp.StatusCode)
		}
	})

	// 塞一份两条发现的复核报告。
	report := prd.ReviewReport{
		Findings: []prd.ReviewFinding{
			{Kind: prd.FindingMalicious, Title: "只写测试不实现也能过"},
			{Kind: prd.FindingUncovered, Title: "并发改同一行没有 AC 覆盖"},
		},
		Verdict: "找到两个洞",
	}
	raw, err := json.Marshal(&report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdatePRDContent(context.Background(), row.ID, userID,
		store.UpdatePRDContentParams{ReviewReport: raw}); err != nil {
		t.Fatal(err)
	}

	t.Run("下标越界要拒绝", func(t *testing.T) {
		resp, _ := doReq(t, srv, http.MethodPut, "/api/prds/"+id+"/review/dispositions",
			`{"findings":[{"index":5,"disposition":"fix"}]}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("越界下标应 400，got=%d", resp.StatusCode)
		}
	})

	t.Run("accept 缺理由要拒绝", func(t *testing.T) {
		resp, body := doReq(t, srv, http.MethodPut, "/api/prds/"+id+"/review/dispositions",
			`{"findings":[{"index":0,"disposition":"accept"}]}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("accept 不写理由应 400，got=%d 响应=%v", resp.StatusCode, body)
		}
	})

	t.Run("一条非法则整批不写", func(t *testing.T) {
		resp, _ := doReq(t, srv, http.MethodPut, "/api/prds/"+id+"/review/dispositions",
			`{"findings":[{"index":0,"disposition":"fix"},{"index":1,"disposition":"accept"}]}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("第二条非法应整批拒绝，got=%d", resp.StatusCode)
		}
		// 第一条合法的也不该落库。
		if n := prdUnhandled(t, st, row.ID, userID); n != 2 {
			t.Errorf("整批拒绝后未处置数 got=%d want=2（第一条不该被写进去）", n)
		}
	})

	t.Run("合法处置写入成功", func(t *testing.T) {
		resp, body := doReq(t, srv, http.MethodPut, "/api/prds/"+id+"/review/dispositions",
			`{"findings":[{"index":0,"disposition":"fix"},{"index":1,"disposition":"accept","dispositionNote":"§6 方案已排除"}]}`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 got=%d want=200，响应=%v", resp.StatusCode, body)
		}
		if n := prdUnhandled(t, st, row.ID, userID); n != 0 {
			t.Errorf("处置完未处置数 got=%d want=0", n)
		}
	})
}

// prdUnhandled 读回还没标处置的发现条数。
func prdUnhandled(t *testing.T, st *store.Store, prdID, userID int64) int {
	t.Helper()
	row, err := st.GetPRD(context.Background(), prdID, userID)
	if err != nil {
		t.Fatal(err)
	}
	var rep prd.ReviewReport
	if err := json.Unmarshal(row.ReviewReport, &rep); err != nil {
		t.Fatal(err)
	}
	return rep.Unhandled()
}

// 自检不过要回 422 + 完整报告，前端靠 Finding.Code 逐条跳节。
func TestSubmitReturnsReportWhenChecklistFails(t *testing.T) {
	api, planner, st, userID, repoID := prdFixture(t)
	srv := prdServer(t, api)
	planner.submitRep = &prd.Report{
		Blocking: []prd.Finding{{Code: "unconfirmed_section", Section: "goals", Message: "§3 还是 🤖"}},
	}

	row := mustPRD(t, st, userID, repoID)
	resp, body := doReq(t, srv, http.MethodPost,
		"/api/prds/"+strconv.FormatInt(row.ID, 10)+"/submit", "")

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("自检不过应 422（请求没错，是内容不满足），got=%d 响应=%v", resp.StatusCode, body)
	}
	rep, ok := body["report"].(map[string]any)
	if !ok {
		t.Fatalf("响应里应带 report，got=%v", body)
	}
	blocking, ok := rep["blocking"].([]any)
	if !ok || len(blocking) != 1 {
		t.Fatalf("report.blocking 应有 1 条，got=%v", rep["blocking"])
	}
	first := blocking[0].(map[string]any)
	if first["code"] != "unconfirmed_section" {
		t.Errorf("阻塞项 code got=%v want=unconfirmed_section", first["code"])
	}
}

// 规划管线没装配时，写端点返回 503 而不是 500 或 panic。
func TestPRDPlannerNotWired(t *testing.T) {
	api, _, st, userID, repoID := prdFixture(t)
	api.Planner = nil
	srv := prdServer(t, api)
	row := mustPRD(t, st, userID, repoID)
	id := strconv.FormatInt(row.ID, 10)

	for _, path := range []string{"/rounds", "/review", "/submit", "/convert"} {
		t.Run(path, func(t *testing.T) {
			resp, _ := doReq(t, srv, http.MethodPost, "/api/prds/"+id+path, "")
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("未装配时 %s 应 503，got=%d", path, resp.StatusCode)
			}
		})
	}

	t.Run("只读端点仍然可用", func(t *testing.T) {
		resp, _ := doReq(t, srv, http.MethodGet, "/api/prds/"+id, "")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("未装配规划管线不该影响只读端点，got=%d", resp.StatusCode)
		}
	})
}

func TestConvertReturnsFlow(t *testing.T) {
	api, planner, st, userID, repoID := prdFixture(t)
	srv := prdServer(t, api)
	row := mustPRD(t, st, userID, repoID)

	resp, body := doReq(t, srv, http.MethodPost,
		"/api/prds/"+strconv.FormatInt(row.ID, 10)+"/convert", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("状态码 got=%d want=201，响应=%v", resp.StatusCode, body)
	}
	if body["flowId"] != float64(42) {
		t.Errorf("flowId got=%v want=42", body["flowId"])
	}
	if planner.convertCalls != 1 {
		t.Errorf("Convert 应被调一次，got=%d", planner.convertCalls)
	}
}

// doRaw 发请求并取回原始响应体 —— 导出是 Markdown / JSON 文件，
// doReq 那种「顺手 JSON 解码」的拿法读不到 md。
func doRaw(t *testing.T, srv *httptest.Server, path string) (*http.Response, string) {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+apiTestToken)
	resp, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(b)
}

func TestExportPRD(t *testing.T) {
	api, _, st, userID, repoID := prdFixture(t)
	srv := prdServer(t, api)

	doc := &prd.Document{
		Type:     prd.TypeFeature,
		OneLiner: prd.Section{Status: prd.StatusConfirmed, Body: "让管理员能配预览阈值"},
		Criteria: []prd.Criterion{{
			ID: "AC-1", Category: prd.ACCategory("happy_path"),
			Given: "管理员已登录", When: "改成 5", Then: "立即生效",
			Verify: prd.VerifyAuto,
		}},
	}
	raw, err := doc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	row := mustPRD(t, st, userID, repoID)
	if _, err := st.UpdatePRDContent(context.Background(), row.ID, userID,
		store.UpdatePRDContentParams{Document: raw}); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(row.ID, 10)

	t.Run("默认导出 Markdown", func(t *testing.T) {
		resp, body := doRaw(t, srv, "/api/prds/"+id+"/export")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 got=%d want=200", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
			t.Errorf("Content-Type got=%q want text/markdown", ct)
		}
		if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "prd-"+id+".md") {
			t.Errorf("Content-Disposition got=%q，应带 .md 文件名", cd)
		}
		for _, want := range []string{"# PRD #" + id, "让管理员能配预览阈值", "### AC-1", "§7 验收标准"} {
			if !strings.Contains(body, want) {
				t.Errorf("导出内容缺少 %q", want)
			}
		}
	})

	t.Run("JSON 导出可解析且含结构化全量", func(t *testing.T) {
		resp, body := doRaw(t, srv, "/api/prds/"+id+"/export?format=json")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("状态码 got=%d want=200", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type got=%q want application/json", ct)
		}
		var out prd.ExportInput
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("JSON 导出不可解析: %v\n%s", err, body)
		}
		if out.PRDID != row.ID {
			t.Errorf("prdId got=%d want=%d", out.PRDID, row.ID)
		}
		if out.Document == nil || len(out.Document.Criteria) != 1 {
			t.Errorf("JSON 导出应带完整 document（含 AC 表），got=%+v", out.Document)
		}
		if out.Repo == "" {
			t.Error("JSON 导出应带仓库名")
		}
	})

	t.Run("未知格式要拒绝", func(t *testing.T) {
		resp, _ := doRaw(t, srv, "/api/prds/"+id+"/export?format=pdf")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("未知格式应 400，got=%d", resp.StatusCode)
		}
	})

	t.Run("非属主拿不到", func(t *testing.T) {
		otherID := mustUser(t, st, "prds-exp-other-"+t.Name()+"@example.com")
		otherSrv := prdServer(t, &PRDAPI{Store: st, Auth: authAs(otherID, "o@example.com")})
		resp, _ := doRaw(t, otherSrv, "/api/prds/"+id+"/export")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("非属主导出应 404，got=%d", resp.StatusCode)
		}
	})
}
