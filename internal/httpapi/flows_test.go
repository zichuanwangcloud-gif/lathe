package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/flow"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// flowFixture 建一个 FlowAPI 实例并起一个测试服务器，供本文件的集成测试复用。
func flowFixture(t *testing.T) (*httptestServer, int64, int64) {
	t.Helper()

	st := testStoreForAPI(t)
	userID := mustUser(t, st, "flow-"+t.Name()+"@example.com")

	var repoID int64
	if err := st.Pool().QueryRow(context.Background(),
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2)
		 ON CONFLICT (user_id, provider_repo) DO UPDATE SET updated_at=now() RETURNING id`,
		userID, "acme/flow-api-"+t.Name()).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}

	api := &FlowAPI{
		Flow: &flow.Service{Pool: st.Pool(), Tasks: task.NewMachine(st.Pool()), Store: st},
		Auth: authAs(userID, "flow@example.com"),
	}
	mux := http.NewServeMux()
	api.Routes(mux)
	srv := newTestServer(t, mux)
	srv.store = st
	srv.userID = userID
	return srv, userID, repoID
}

// TestFlowCreateGraph1To2To3Plus4Plus5To6 覆盖 M1 出口条件：用 API 建一张
// 1→2→3 / 4 / 5→6 的图（10 个 fake issue），断言 5 个独立根的 depends_on
// 为空、其余 5 个的 depends_on 指向正确的前驱 id，且全部处于 queued。
func TestFlowCreateGraph1To2To3Plus4Plus5To6(t *testing.T) {
	srv, _, repoID := flowFixture(t)

	body := fmt.Sprintf(`{"name":"g1","repoId":%d,"nodes":[
		{"issueKey":"ISS-1"},
		{"issueKey":"ISS-2","dependsOnIndex":0},
		{"issueKey":"ISS-3","dependsOnIndex":1},
		{"issueKey":"ISS-4"},
		{"issueKey":"ISS-5"},
		{"issueKey":"ISS-6","dependsOnIndex":4},
		{"issueKey":"ISS-7","dependsOnIndex":5},
		{"issueKey":"ISS-8"},
		{"issueKey":"ISS-9","dependsOnIndex":7},
		{"issueKey":"ISS-10","dependsOnIndex":8}
	]}`, repoID)

	resp := srv.do(t, "POST", "/api/flows", body, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("创建应返回 201，得到 %d: %s", resp.StatusCode, srv.raw(t, resp))
	}
	respBody := srv.decode(t, resp)

	flowID, ok := respBody["flowId"].(float64)
	if !ok || flowID == 0 {
		t.Fatalf("应返回非零 flowId，得到 %v", respBody["flowId"])
	}
	tasksRaw, ok := respBody["tasks"].([]any)
	if !ok || len(tasksRaw) != 10 {
		t.Fatalf("应建出 10 个任务，得到 %v", respBody["tasks"])
	}

	// 断言全部处于 queued（此时还没有 pipeline 在跑）
	for i, tr := range tasksRaw {
		tm := tr.(map[string]any)
		if tm["state"] != string(task.StateQueued) {
			t.Errorf("第 %d 个任务状态应为 queued，得到 %v", i, tm["state"])
		}
	}

	// 用 GET /api/flows/{id} 取回依赖结构逐一断言
	getResp := srv.do(t, "GET", fmt.Sprintf("/api/flows/%d", int64(flowID)), "", true)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("查询应返回 200，得到 %d: %s", getResp.StatusCode, srv.raw(t, getResp))
	}
	getBody := srv.decode(t, getResp)
	flowTasksRaw, ok := getBody["tasks"].([]any)
	if !ok || len(flowTasksRaw) != 10 {
		t.Fatalf("查询应返回 10 个任务，得到 %v", getBody["tasks"])
	}

	ids := make([]int64, 10)
	for i, tr := range flowTasksRaw {
		tm := tr.(map[string]any)
		id, _ := tm["id"].(float64)
		ids[i] = int64(id)
	}

	roots := map[int]bool{0: true, 3: true, 4: true, 7: true}
	depOn := map[int]int{1: 0, 2: 1, 5: 4, 6: 5, 8: 7, 9: 8}

	for i, tr := range flowTasksRaw {
		tm := tr.(map[string]any)
		if roots[i] {
			if tm["dependsOn"] != nil {
				t.Errorf("第 %d 个任务应是独立根，dependsOn 应为空，得到 %v", i, tm["dependsOn"])
			}
			continue
		}
		wantPred := depOn[i]
		gotDep, ok := tm["dependsOn"].(float64)
		if !ok {
			t.Errorf("第 %d 个任务应有 dependsOn，得到 %v", i, tm["dependsOn"])
			continue
		}
		if int64(gotDep) != ids[wantPred] {
			t.Errorf("第 %d 个任务的 dependsOn 应指向第 %d 个任务(id=%d)，得到 %d",
				i, wantPred, ids[wantPred], int64(gotDep))
		}
	}
}

// TestFlowCreateRejectsInvalidIndex 覆盖 F1.2-AC3 在这个 API 形状下的等价
// 表现：dependsOnIndex 指向自己或之后的节点被拒绝，返回 4xx 而非 500，且
// 不创建任何行。
func TestFlowCreateRejectsInvalidIndex(t *testing.T) {
	srv, _, repoID := flowFixture(t)

	body := fmt.Sprintf(`{"name":"bad","repoId":%d,"nodes":[
		{"issueKey":"BAD-1","dependsOnIndex":0}
	]}`, repoID)

	resp := srv.do(t, "POST", "/api/flows", body, true)
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("非法下标应返回 4xx，得到 %d: %s", resp.StatusCode, srv.raw(t, resp))
	}
	respBody := srv.decode(t, resp)
	if respBody["error"] == nil || respBody["error"] == "" {
		t.Errorf("应返回明确的 error 字段，得到 %v", respBody)
	}
}

// TestFlowCreateTooManyNodesRejected 覆盖 F1.4-AC3 的范围收窄版本：单批
// 节点数超过硬上限时拒绝，不创建任何行。
func TestFlowCreateTooManyNodesRejected(t *testing.T) {
	srv, _, repoID := flowFixture(t)

	nodes := make([]map[string]any, flow.MaxNodes+1)
	for i := range nodes {
		nodes[i] = map[string]any{"issueKey": fmt.Sprintf("MANY-%d", i)}
	}
	reqBody, err := json.Marshal(map[string]any{
		"name": "toomany", "repoId": repoID, "nodes": nodes,
	})
	if err != nil {
		t.Fatal(err)
	}

	resp := srv.do(t, "POST", "/api/flows", string(reqBody), true)
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("超过上限应返回 4xx，得到 %d: %s", resp.StatusCode, srv.raw(t, resp))
	}
}

// TestFlowCreateDuplicateSubmissionIsIdempotent 覆盖 F1.4-AC2：重复提交
// 同一批次不产生第二个 flow。
func TestFlowCreateDuplicateSubmissionIsIdempotent(t *testing.T) {
	srv, userID, repoID := flowFixture(t)

	body := fmt.Sprintf(`{"name":"dup","repoId":%d,"nodes":[
		{"issueKey":"DUP-1"},
		{"issueKey":"DUP-2","dependsOnIndex":0}
	]}`, repoID)

	resp1 := srv.do(t, "POST", "/api/flows", body, true)
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("第一次提交应返回 201，得到 %d: %s", resp1.StatusCode, srv.raw(t, resp1))
	}
	body1 := srv.decode(t, resp1)

	resp2 := srv.do(t, "POST", "/api/flows", body, true)
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("重复提交应被当成幂等成功处理，得到 %d: %s", resp2.StatusCode, srv.raw(t, resp2))
	}
	body2 := srv.decode(t, resp2)

	if body1["flowId"] != body2["flowId"] {
		t.Errorf("重复提交应返回同一个 flowId，得到 %v 与 %v", body1["flowId"], body2["flowId"])
	}

	var flowCount int
	if err := srv.store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM flows WHERE user_id = $1`, userID).Scan(&flowCount); err != nil {
		t.Fatal(err)
	}
	if flowCount != 1 {
		t.Errorf("重复提交不应产生第二个 flow，得到 %d 个", flowCount)
	}
}

// TestFlowCreateChainWarningsAppearInResponseAndFlowStillCreated 覆盖
// F3.3 在无 UI 场景下能落到的最大程度：POST /api/flows 建一张
// 1→2→3→4→5（深度 5）超过默认链长上限 4 的图，响应体里应能看到
// warnings 字段、内容点名超限的节点；且图依然被正常创建
// （201，不拒绝——F3.3-AC1"仅警告"精神在无 UI 场景下的落地）。
func TestFlowCreateChainWarningsAppearInResponseAndFlowStillCreated(t *testing.T) {
	srv, _, repoID := flowFixture(t)

	body := fmt.Sprintf(`{"name":"chain","repoId":%d,"nodes":[
		{"issueKey":"CHAIN-1"},
		{"issueKey":"CHAIN-2","dependsOnIndex":0},
		{"issueKey":"CHAIN-3","dependsOnIndex":1},
		{"issueKey":"CHAIN-4","dependsOnIndex":2},
		{"issueKey":"CHAIN-5","dependsOnIndex":3}
	]}`, repoID)

	resp := srv.do(t, "POST", "/api/flows", body, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("超链长不应拒绝创建，应返回 201，得到 %d: %s", resp.StatusCode, srv.raw(t, resp))
	}
	respBody := srv.decode(t, resp)

	tasksRaw, ok := respBody["tasks"].([]any)
	if !ok || len(tasksRaw) != 5 {
		t.Fatalf("图应正常建出 5 个任务，得到 %v", respBody["tasks"])
	}

	warningsRaw, ok := respBody["warnings"].([]any)
	if !ok {
		t.Fatalf("响应体应带 warnings 字段（数组），得到 %v", respBody["warnings"])
	}
	if len(warningsRaw) != 1 {
		t.Fatalf("应恰好有 1 条 warning，得到 %d 条: %v", len(warningsRaw), warningsRaw)
	}
	warning, ok := warningsRaw[0].(string)
	if !ok {
		t.Fatalf("warning 应是字符串，得到 %T", warningsRaw[0])
	}
	if !strings.Contains(warning, "CHAIN-5") {
		t.Errorf("warning 应指出第 5 个节点(CHAIN-5)，得到 %q", warning)
	}
	if !strings.Contains(warning, "5") || !strings.Contains(warning, "4") {
		t.Errorf("warning 应包含实际链长度 5 与上限 4，得到 %q", warning)
	}
}

// TestFlowCreateNoWarningsWhenChainWithinLimit 覆盖不超限时的对照组：
// 1→2→3（深度 3）不超过默认上限 4，warnings 字段应存在但为空数组。
func TestFlowCreateNoWarningsWhenChainWithinLimit(t *testing.T) {
	srv, _, repoID := flowFixture(t)

	body := fmt.Sprintf(`{"name":"short","repoId":%d,"nodes":[
		{"issueKey":"SHORT-1"},
		{"issueKey":"SHORT-2","dependsOnIndex":0},
		{"issueKey":"SHORT-3","dependsOnIndex":1}
	]}`, repoID)

	resp := srv.do(t, "POST", "/api/flows", body, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("应返回 201，得到 %d: %s", resp.StatusCode, srv.raw(t, resp))
	}
	respBody := srv.decode(t, resp)

	warningsRaw, ok := respBody["warnings"].([]any)
	if !ok {
		t.Fatalf("响应体应带 warnings 字段（数组），得到 %v", respBody["warnings"])
	}
	if len(warningsRaw) != 0 {
		t.Errorf("深度 3 不超过默认上限 4，warnings 应为空，得到 %v", warningsRaw)
	}
}

// TestFlowGetExposesDependsOnAtAndProfile 覆盖画布检视面板需要的两个
// 新字段：GET /api/flows/{id} 的每个任务应带上 dependsOnAt（未指定时
// 落默认值 pr_open）与 profile（原样透传，未设时为 null，不是 base64
// 字符串——这正是审查发现的坑，见 flows.go get handler 的注释）。
func TestFlowGetExposesDependsOnAtAndProfile(t *testing.T) {
	srv, _, repoID := flowFixture(t)

	body := fmt.Sprintf(`{"name":"insp","repoId":%d,"nodes":[
		{"issueKey":"INSP-1","profile":{"modelChannel":"opus-plan","skills":["go-testing"]}},
		{"issueKey":"INSP-2","dependsOnIndex":0,"dependsOnAt":"merged"}
	]}`, repoID)

	createResp := srv.do(t, "POST", "/api/flows", body, true)
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("创建应返回 201，得到 %d: %s", createResp.StatusCode, srv.raw(t, createResp))
	}
	flowID := int64(srv.decode(t, createResp)["flowId"].(float64))

	getResp := srv.do(t, "GET", fmt.Sprintf("/api/flows/%d", flowID), "", true)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("查询应返回 200，得到 %d: %s", getResp.StatusCode, srv.raw(t, getResp))
	}
	tasksRaw := srv.decode(t, getResp)["tasks"].([]any)
	if len(tasksRaw) != 2 {
		t.Fatalf("应有 2 个任务，得到 %v", tasksRaw)
	}

	t0 := tasksRaw[0].(map[string]any)
	if t0["dependsOnAt"] != "pr_open" {
		t.Errorf("第 1 个任务未指定 dependsOnAt 应落默认值 pr_open，得到 %v", t0["dependsOnAt"])
	}
	profile, ok := t0["profile"].(map[string]any)
	if !ok {
		t.Fatalf("profile 应原样解析成 JSON 对象（不是 base64 字符串），得到 %T: %v", t0["profile"], t0["profile"])
	}
	if profile["modelChannel"] != "opus-plan" {
		t.Errorf("profile.modelChannel 应为 opus-plan，得到 %v", profile["modelChannel"])
	}

	t1 := tasksRaw[1].(map[string]any)
	if t1["dependsOnAt"] != "merged" {
		t.Errorf("第 2 个任务的 dependsOnAt 应为 merged，得到 %v", t1["dependsOnAt"])
	}
	// tasks.profile 列的 schema 默认值是 '{}'::jsonb（NOT NULL），不是
	// SQL NULL——未设画像时应读回一个空对象，不是 null。
	if empty, ok := t1["profile"].(map[string]any); !ok || len(empty) != 0 {
		t.Errorf("第 2 个任务未设画像，profile 应为空对象 {}，得到 %T: %v", t1["profile"], t1["profile"])
	}
}

// TestFlowCreateRejectsOtherUsersRepo 覆盖归属校验在 HTTP 层的表现：
// repoId 属于别的账号时应返回 404（对非属主隐瞒存在），不是替别人建出
// 任务。
func TestFlowCreateRejectsOtherUsersRepo(t *testing.T) {
	srv, _, _ := flowFixture(t)

	otherUserID := mustUser(t, srv.store, "flow-other-"+t.Name()+"@example.com")
	var otherRepoID int64
	if err := srv.store.Pool().QueryRow(context.Background(),
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		otherUserID, "acme/flow-other-"+t.Name()).Scan(&otherRepoID); err != nil {
		t.Fatalf("建另一个用户的 repo 失败: %v", err)
	}

	body := fmt.Sprintf(`{"name":"steal","repoId":%d,"nodes":[{"issueKey":"STEAL-1"}]}`, otherRepoID)
	resp := srv.do(t, "POST", "/api/flows", body, true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("别人的 repoId 应返回 404，得到 %d: %s", resp.StatusCode, srv.raw(t, resp))
	}
}

// TestFlowList 覆盖新端点：只列出当前用户名下的图，按创建时间倒序，
// 任务数正确——这是"编排"菜单落地页唯一的数据来源。
func TestFlowList(t *testing.T) {
	srv, _, repoID := flowFixture(t)

	empty := srv.do(t, "GET", "/api/flows", "", true)
	if empty.StatusCode != http.StatusOK {
		t.Fatalf("空列表应返回 200，得到 %d: %s", empty.StatusCode, srv.raw(t, empty))
	}
	if flows := srv.decode(t, empty)["flows"].([]any); len(flows) != 0 {
		t.Fatalf("尚未建图应返回空数组，得到 %v", flows)
	}

	body1 := fmt.Sprintf(`{"name":"la","repoId":%d,"nodes":[{"issueKey":"LST-1"}]}`, repoID)
	r1 := srv.do(t, "POST", "/api/flows", body1, true)
	flowID1 := int64(srv.decode(t, r1)["flowId"].(float64))

	body2 := fmt.Sprintf(`{"name":"lb","repoId":%d,"nodes":[
		{"issueKey":"LST-2a"},{"issueKey":"LST-2b","dependsOnIndex":0}
	]}`, repoID)
	r2 := srv.do(t, "POST", "/api/flows", body2, true)
	flowID2 := int64(srv.decode(t, r2)["flowId"].(float64))

	// 另一个用户建的图不该出现在这次列表里
	otherUserID := mustUser(t, srv.store, "flow-listother-"+t.Name()+"@example.com")
	svc := flow.Service{Pool: srv.store.Pool(), Tasks: task.NewMachine(srv.store.Pool())}
	var otherRepoID int64
	if err := srv.store.Pool().QueryRow(context.Background(),
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		otherUserID, "acme/flow-listother-"+t.Name()).Scan(&otherRepoID); err != nil {
		t.Fatalf("建另一个用户的 repo 失败: %v", err)
	}
	if _, _, _, err := svc.CreateFlow(context.Background(), otherUserID, otherRepoID, "not-mine",
		[]flow.NodeInput{{IssueKey: "LST-OTHER"}}); err != nil {
		t.Fatalf("另一个用户建图应成功，得到 %v", err)
	}

	listResp := srv.do(t, "GET", "/api/flows", "", true)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("列表应返回 200，得到 %d: %s", listResp.StatusCode, srv.raw(t, listResp))
	}
	flowsRaw := srv.decode(t, listResp)["flows"].([]any)
	if len(flowsRaw) != 2 {
		t.Fatalf("应只看到自己的 2 张图，得到 %d 张: %v", len(flowsRaw), flowsRaw)
	}

	got0 := flowsRaw[0].(map[string]any)
	got1 := flowsRaw[1].(map[string]any)
	if int64(got0["id"].(float64)) != flowID2 || int64(got1["id"].(float64)) != flowID1 {
		t.Fatalf("应按创建时间倒序（后建的在前），得到 %v", flowsRaw)
	}
	if int(got0["taskCount"].(float64)) != 2 {
		t.Errorf("第二张图应有 2 个任务，得到 %v", got0["taskCount"])
	}
	if int(got1["taskCount"].(float64)) != 1 {
		t.Errorf("第一张图应有 1 个任务，得到 %v", got1["taskCount"])
	}
}
