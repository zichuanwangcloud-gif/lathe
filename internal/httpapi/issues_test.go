package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

// fakeIssueQueue 记录工单动作（开跑/取消联动）的调用。
type fakeIssueQueue struct {
	enqueued   []int64 // 收到 EnqueueInternal 的 issue id
	cancelled  []string
	cancelIDs  []int64
	enqueueErr error
	cancelErr  error
}

func (f *fakeIssueQueue) EnqueueInternal(ctx context.Context, ownerUserID, issueID int64) error {
	if f.enqueueErr != nil {
		return f.enqueueErr
	}
	f.enqueued = append(f.enqueued, issueID)
	return nil
}

func (f *fakeIssueQueue) CancelForInternalIssue(ctx context.Context, ownerUserID int64, issueKey string) ([]int64, error) {
	f.cancelled = append(f.cancelled, issueKey)
	return f.cancelIDs, f.cancelErr
}

func issuesFixture(t *testing.T) (*IssuesAPI, *store.Store, int64, int64) {
	t.Helper()
	st := testStoreForAPI(t)
	userID := mustUser(t, st, "issues-"+t.Name()+"@example.com")

	// provider_repo 必须带随机量：mustUser 的 email 是定值，上一轮被中断的
	// 测试会留下同 user_id 的孤儿行，定值 repo 名会直接撞唯一索引（T9 教训）。
	var repoID int64
	if err := st.Pool().QueryRow(context.Background(),
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		userID, fmt.Sprintf("acme/issues-api-%s-%d", t.Name(), time.Now().UnixNano())).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	api := &IssuesAPI{Store: st, Auth: authAs(userID, "issues-fixture@example.com"), Queue: &fakeIssueQueue{}}
	return api, st, userID, repoID
}

func issuesServer(t *testing.T, api *IssuesAPI) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	api.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func doReq(t *testing.T, srv *httptest.Server, method, path, body string) (*http.Response, map[string]any) {
	t.Helper()
	r, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+apiTestToken)
	resp, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	var v map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return resp, v
}

func TestIssuesCreateListGetFlow(t *testing.T) {
	api, _, _, repoID := issuesFixture(t)
	srv := issuesServer(t, api)

	// 缺标题 / 缺仓库：400
	if resp, _ := doReq(t, srv, "POST", "/api/issues", `{"repoId":1}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺标题应 400，得到 %d", resp.StatusCode)
	}
	if resp, _ := doReq(t, srv, "POST", "/api/issues", `{"title":"x"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺仓库应 400，得到 %d", resp.StatusCode)
	}
	// 别人的仓库（id 不存在于本用户名下）：404 语义
	if resp, _ := doReq(t, srv, "POST", "/api/issues", `{"repoId":99999999,"title":"x"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("非属主仓库应 404，得到 %d", resp.StatusCode)
	}

	resp, v := doReq(t, srv, "POST", "/api/issues",
		fmt.Sprintf(`{"repoId":%d,"title":"登录页报错","description":"点登录 500","priority":2}`, repoID))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("建工单应 201，得到 %d: %v", resp.StatusCode, v)
	}
	it := v["issue"].(map[string]any)
	if it["key"].(string) == "" || it["state"] != "open" {
		t.Fatalf("工单字段不对: %v", it)
	}
	id := int64(it["id"].(float64))

	// 列表应包含
	_, v = doReq(t, srv, "GET", "/api/issues", "")
	found := false
	for _, x := range v["issues"].([]any) {
		if x.(map[string]any)["key"] == it["key"] {
			found = true
		}
	}
	if !found {
		t.Error("列表里没有刚建的工单")
	}
	// 状态过滤：open 有，done 没有
	_, v = doReq(t, srv, "GET", "/api/issues?state=done", "")
	for _, x := range v["issues"].([]any) {
		if x.(map[string]any)["key"] == it["key"] {
			t.Error("done 过滤里不应出现 open 工单")
		}
	}
	if resp, _ := doReq(t, srv, "GET", "/api/issues?state=bogus", ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("非法 state 应 400，得到 %d", resp.StatusCode)
	}

	// 详情：工单 + 评论 + 关联任务三个区块同源返回
	_, v = doReq(t, srv, "GET", "/api/issues/"+strconv.FormatInt(id, 10), "")
	if v["issue"].(map[string]any)["key"] != it["key"] {
		t.Error("详情返回的工单不对")
	}
	if _, ok := v["comments"].([]any); !ok {
		t.Error("详情应带 comments")
	}
	if _, ok := v["tasks"].([]any); !ok {
		t.Error("详情应带 tasks（关联任务区块）")
	}
}

func TestIssuesCommentRoundTrip(t *testing.T) {
	api, st, userID, repoID := issuesFixture(t)
	srv := issuesServer(t, api)

	it, _ := st.CreateIssue(context.Background(), store.CreateIssueParams{UserID: userID, RepoID: repoID, Title: "t"})

	// 空评论 400
	if resp, _ := doReq(t, srv, "POST", fmt.Sprintf("/api/issues/%d/comments", it.ID), `{"body":"  "}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("空评论应 400，得到 %d", resp.StatusCode)
	}

	resp, v := doReq(t, srv, "POST", fmt.Sprintf("/api/issues/%d/comments", it.ID), `{"body":"补充：只在生产环境复现"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("评论应 201，得到 %d: %v", resp.StatusCode, v)
	}
	c := v["comment"].(map[string]any)
	if c["body"] != "补充：只在生产环境复现" {
		t.Errorf("评论内容不对: %v", c)
	}
	if c["actor"] != nil {
		t.Errorf("人发的评论不该带 actor: %v", c)
	}
}

func TestIssuesUpdateGuardAndCancelLinkage(t *testing.T) {
	api, st, userID, repoID := issuesFixture(t)
	fq := api.Queue.(*fakeIssueQueue)
	srv := issuesServer(t, api)

	it, _ := st.CreateIssue(context.Background(), store.CreateIssueParams{UserID: userID, RepoID: repoID, Title: "t"})

	// 非法转移：open → done 直接跳，409
	resp, _ := doReq(t, srv, "PUT", fmt.Sprintf("/api/issues/%d", it.ID), `{"state":"done"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("非法转移应 409，得到 %d", resp.StatusCode)
	}

	// open → cancelled 合法，且触发取消联动
	resp, v := doReq(t, srv, "PUT", fmt.Sprintf("/api/issues/%d", it.ID), `{"state":"cancelled"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("取消应 200，得到 %d: %v", resp.StatusCode, v)
	}
	if len(fq.cancelled) != 1 || fq.cancelled[0] != it.Key {
		t.Errorf("取消联动应以 key %s 调用一次，得到 %v", it.Key, fq.cancelled)
	}

	// cancelled → open（重开）合法
	resp, _ = doReq(t, srv, "PUT", fmt.Sprintf("/api/issues/%d", it.ID), `{"state":"open"}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("重开应 200，得到 %d", resp.StatusCode)
	}
}

func TestIssuesStartEnqueue(t *testing.T) {
	api, st, userID, repoID := issuesFixture(t)
	fq := api.Queue.(*fakeIssueQueue)
	srv := issuesServer(t, api)

	it, _ := st.CreateIssue(context.Background(), store.CreateIssueParams{UserID: userID, RepoID: repoID, Title: "t"})

	resp, v := doReq(t, srv, "POST", fmt.Sprintf("/api/issues/%d/start", it.ID), "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("开跑应 202，得到 %d: %v", resp.StatusCode, v)
	}
	if len(fq.enqueued) != 1 || fq.enqueued[0] != it.ID {
		t.Errorf("开跑应以 issue id %d 入队，得到 %v", it.ID, fq.enqueued)
	}

	// 队列拒绝（已有进行中任务）→ 409 人话
	fq.enqueueErr = errors.New("工单已有进行中的任务")
	resp, _ = doReq(t, srv, "POST", fmt.Sprintf("/api/issues/%d/start", it.ID), "")
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("重复开跑应 409，得到 %d", resp.StatusCode)
	}
}

func TestIssuesIsolation404(t *testing.T) {
	api, st, userID, repoID := issuesFixture(t)
	_ = api // 属主侧不直接请求；工单用 store 层建

	it, _ := st.CreateIssue(context.Background(), store.CreateIssueParams{UserID: userID, RepoID: repoID, Title: "t"})

	// 同一台服务换另一个用户视角：一切 404，不暴露存在。
	// 新起一台服务挂另一个 Auth（同一套路由在不同鉴权上下文下）。
	other := &IssuesAPI{Store: st, Auth: authAs(userID+77777, "other@example.com"), Queue: &fakeIssueQueue{}}
	srv2 := issuesServer(t, other)

	for _, tc := range []struct{ method, path, body string }{
		{"GET", fmt.Sprintf("/api/issues/%d", it.ID), ""},
		{"PUT", fmt.Sprintf("/api/issues/%d", it.ID), `{"title":"劫持"}`},
		{"DELETE", fmt.Sprintf("/api/issues/%d", it.ID), ""},
		{"POST", fmt.Sprintf("/api/issues/%d/comments", it.ID), `{"body":"x"}`},
		{"POST", fmt.Sprintf("/api/issues/%d/start", it.ID), ""},
	} {
		resp, _ := doReq(t, srv2, tc.method, tc.path, tc.body)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s 非属主应 404，得到 %d", tc.method, tc.path, resp.StatusCode)
		}
	}
}

func TestIssuesDeleteBlockedByTasks(t *testing.T) {
	api, st, userID, repoID := issuesFixture(t)
	srv := issuesServer(t, api)

	// 无关联任务：可删
	it, _ := st.CreateIssue(context.Background(), store.CreateIssueParams{UserID: userID, RepoID: repoID, Title: "可删"})
	resp, _ := doReq(t, srv, "DELETE", fmt.Sprintf("/api/issues/%d", it.ID), "")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("无关联任务的工单应可删，得到 %d", resp.StatusCode)
	}

	// 有关联任务：409
	it2, _ := st.CreateIssue(context.Background(), store.CreateIssueParams{UserID: userID, RepoID: repoID, Title: "有任务"})
	if _, err := st.Pool().Exec(context.Background(), `
		INSERT INTO tasks (user_id, repo_id, external_key, tracker_provider, state)
		VALUES ($1, $2, $3, 'internal', 'cancelled')`,
		userID, repoID, it2.Key); err != nil {
		t.Fatal(err)
	}
	resp, _ = doReq(t, srv, "DELETE", fmt.Sprintf("/api/issues/%d", it2.ID), "")
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("有关联任务的工单应 409，得到 %d", resp.StatusCode)
	}
}
