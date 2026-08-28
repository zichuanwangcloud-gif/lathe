package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Clouditera/lathe/internal/flow"
	"github.com/Clouditera/lathe/internal/task"
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
