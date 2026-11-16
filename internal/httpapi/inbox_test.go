package httpapi

import (
	"bytes"
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

	"github.com/zichuanwangcloud-gif/lathe/internal/inbox"
	"github.com/zichuanwangcloud-gif/lathe/internal/prd"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// 收件箱 HTTP 面（docs/features/inbox.md §5.4）。fixture 一律带随机量，
// 断言只看本 fixture 用户。

type inboxEnv struct {
	st *store.Store
	m  *task.Machine
}

func newInboxEnv(t *testing.T) inboxEnv {
	t.Helper()
	st := testStoreForAPI(t)
	return inboxEnv{st: st, m: task.NewMachine(st.Pool())}
}

// user 建一个带随机量的用户与仓库。
func (e inboxEnv) user(t *testing.T) (userID, repoID int64) {
	t.Helper()
	n := strconv.FormatInt(time.Now().UnixNano(), 10)
	userID = mustUser(t, e.st, "inbox-"+strings.ReplaceAll(t.Name(), "/", "_")+"-"+n+"@example.com")
	t.Cleanup(func() {
		// prds.repo_id 是 RESTRICT：先删 PRD，删 user 才能级联到 repos
		_, _ = e.st.Pool().Exec(context.Background(), `DELETE FROM prds WHERE user_id = $1`, userID)
	})
	if err := e.st.Pool().QueryRow(context.Background(),
		`INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		userID, "acme/inbox-api-"+n).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	return userID, repoID
}

// server 起一个以 userID 身份登录（Bearer 令牌映射到该用户）的收件箱服务。
func (e inboxEnv) server(t *testing.T, userID int64) *httptest.Server {
	t.Helper()
	svc, err := inbox.NewService(e.st, func(s string) string { return s })
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&InboxAPI{Service: svc, Auth: authAs(userID, "inbox@example.com")}).Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (e inboxEnv) taskIn(t *testing.T, userID, repoID int64, path ...task.State) *task.Task {
	t.Helper()
	tk, err := e.m.Create(context.Background(), task.CreateParams{
		UserID: userID, RepoID: repoID, ExternalKey: "IB-" + strconv.FormatInt(time.Now().UnixNano(), 10),
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	for _, s := range path {
		reason := "原因"
		if _, err := e.m.Transition(context.Background(), tk.ID, s, "system",
			&task.TransitionOpts{FailureReason: &reason}); err != nil {
			t.Fatalf("转到 %s 失败: %v", s, err)
		}
	}
	return tk
}

func (e inboxEnv) prdReady(t *testing.T, userID, repoID int64) *store.PRDRow {
	t.Helper()
	ctx := context.Background()
	p, err := e.st.CreatePRD(ctx, store.CreatePRDParams{UserID: userID, RepoID: repoID, PRDType: "feature", OriginalInput: "要一个收件箱"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.TransitionPRD(ctx, p.ID, userID, store.TransitionPRDParams{
		From: string(prd.StateDrafting), To: string(prd.StateReadyForReview)}); err != nil {
		t.Fatal(err)
	}
	return p
}

var (
	toAwaiting = []task.State{task.StateTriaging, task.StateAwaitingApproval}
	toFailedIB = []task.State{task.StateTriaging, task.StateFailed}
	toSpecIB   = []task.State{task.StateTriaging, task.StateBlockedSpec}
)

type inboxResp struct {
	Items  []inbox.Item `json:"items"`
	Unread int          `json:"unread"`
	Total  int          `json:"total"`
}

func reqInbox(t *testing.T, srv *httptest.Server, method, path string, auth bool) (int, []byte) {
	t.Helper()
	resp := do(t, srv, method, path, "", auth)
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

func listInbox(t *testing.T, srv *httptest.Server, query string) inboxResp {
	t.Helper()
	code, b := reqInbox(t, srv, "GET", "/api/inbox"+query, true)
	if code != http.StatusOK {
		t.Fatalf("GET /api/inbox%s: got=%d want=200 body=%s", query, code, b)
	}
	var v inboxResp
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func countInbox(t *testing.T, srv *httptest.Server) (unread, total int) {
	t.Helper()
	code, b := reqInbox(t, srv, "GET", "/api/inbox/count", true)
	if code != http.StatusOK {
		t.Fatalf("GET /api/inbox/count: got=%d want=200 body=%s", code, b)
	}
	var v struct{ Unread, Total int }
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v.Unread, v.Total
}

func findItem(items []inbox.Item, subjectType string, id int64) *inbox.Item {
	for i := range items {
		if items[i].SubjectType == subjectType && items[i].SubjectID == id {
			return &items[i]
		}
	}
	return nil
}

// mustFind 同 findItem，找不到直接判失败（不让后面的取值 panic 掉整个包的测试）。
func mustFind(t *testing.T, items []inbox.Item, subjectType string, id int64) *inbox.Item {
	t.Helper()
	it := findItem(items, subjectType, id)
	if it == nil {
		t.Fatalf("事项 %s/%d 不在列表中: got=%+v", subjectType, id, items)
	}
	return it
}

func (e inboxEnv) marks(t *testing.T, where string, args ...any) int {
	t.Helper()
	var n int
	if err := e.st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM inbox_marks WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestInboxIsolation(t *testing.T) {
	e := newInboxEnv(t)
	userA, repoA := e.user(t)
	userB, _ := e.user(t)
	tk := e.taskIn(t, userA, repoA, toAwaiting...)
	p := e.prdReady(t, userA, repoA)

	srvA := e.server(t, userA)
	srvB := e.server(t, userB)

	itemsA := listInbox(t, srvA, "?filter=all")
	ti := mustFind(t, itemsA.Items, "task", tk.ID)
	mustFind(t, itemsA.Items, "prd", p.ID)

	t.Run("B 的列表里看不到 A 的任何事项", func(t *testing.T) {
		v := listInbox(t, srvB, "?filter=all")
		for _, it := range v.Items {
			if it.SubjectID == tk.ID || it.SubjectID == p.ID {
				t.Errorf("B 看到了 A 的事项: %+v", it)
			}
		}
		if v.Total != 0 {
			t.Errorf("B 的 total: got=%d want=0", v.Total)
		}
	})

	t.Run("B 标记 A 的键与不存在的键，404 响应体逐字节相同", func(t *testing.T) {
		c1, b1 := reqInbox(t, srvB, "POST", "/api/inbox/items/"+ti.Key+"/read", true)
		c2, b2 := reqInbox(t, srvB, "POST", "/api/inbox/items/task:999999999:e1/read", true)
		if c1 != http.StatusNotFound || c2 != http.StatusNotFound {
			t.Fatalf("状态码: got=%d/%d want=404/404", c1, c2)
		}
		if !bytes.Equal(b1, b2) {
			t.Errorf("响应体不同: %q vs %q", b1, b2)
		}
		if bytes.Contains(b1, []byte(strconv.FormatInt(tk.ID, 10))) {
			t.Errorf("404 响应体泄露了主体 id: %s", b1)
		}
	})

	t.Run("B 的请求没有写进任何行，A 仍是未读", func(t *testing.T) {
		if n := e.marks(t, `item_key = $1`, ti.Key); n != 0 {
			t.Errorf("inbox_marks 行数: got=%d want=0", n)
		}
		v := listInbox(t, srvA, "?filter=all")
		if it := findItem(v.Items, "task", tk.ID); it == nil || it.Read {
			t.Errorf("A 的事项: got=%+v want 未读", it)
		}
	})
}

func TestInboxReadUnreadDismiss(t *testing.T) {
	e := newInboxEnv(t)
	userID, repoID := e.user(t)
	failed := e.taskIn(t, userID, repoID, toFailedIB...)
	gated := e.taskIn(t, userID, repoID, toAwaiting...)
	srv := e.server(t, userID)

	key := mustFind(t, listInbox(t, srv, "").Items, "task", failed.ID).Key
	u0, t0 := countInbox(t, srv)

	post := func(op, k string) (int, []byte) {
		return reqInbox(t, srv, "POST", "/api/inbox/items/"+k+"/"+op, true)
	}

	if c, b := post("read", key); c != http.StatusOK {
		t.Fatalf("read: got=%d want=200 body=%s", c, b)
	}
	if it := findItem(listInbox(t, srv, "").Items, "task", failed.ID); it == nil || !it.Read {
		t.Errorf("read 之后: got=%+v want read=true", it)
	}
	if u, _ := countInbox(t, srv); u != u0-1 {
		t.Errorf("read 之后 unread: got=%d want=%d", u, u0-1)
	}

	if c, b := post("unread", key); c != http.StatusOK {
		t.Fatalf("unread: got=%d want=200 body=%s", c, b)
	}
	if it := findItem(listInbox(t, srv, "").Items, "task", failed.ID); it == nil || it.Read {
		t.Errorf("unread 之后: got=%+v want read=false", it)
	}

	if c, b := post("dismiss", key); c != http.StatusOK {
		t.Fatalf("dismiss: got=%d want=200 body=%s", c, b)
	}
	if it := findItem(listInbox(t, srv, "").Items, "task", failed.ID); it != nil {
		t.Errorf("dismiss 之后默认列表仍有该事项: %+v", it)
	}
	if _, tt := countInbox(t, srv); tt != t0-1 {
		t.Errorf("dismiss 之后 total: got=%d want=%d", tt, t0-1)
	}

	gkey := mustFind(t, listInbox(t, srv, "").Items, "task", gated.ID).Key
	if c, b := post("dismiss", gkey); c != http.StatusConflict {
		t.Errorf("对 awaiting_approval 忽略: got=%d want=409 body=%s", c, b)
	}
	if n := e.marks(t, `user_id = $1 AND task_id = $2 AND dismissed_at IS NOT NULL`, userID, gated.ID); n != 0 {
		t.Errorf("不可忽略的事项却落了忽略标记: got=%d want=0", n)
	}
}

func TestInboxReadAllAndCount(t *testing.T) {
	e := newInboxEnv(t)
	userID, repoID := e.user(t)
	e.taskIn(t, userID, repoID, toFailedIB...)
	e.taskIn(t, userID, repoID, toAwaiting...)
	e.prdReady(t, userID, repoID)
	srv := e.server(t, userID)

	check := func(wantU, wantT int) {
		t.Helper()
		if u, tt := countInbox(t, srv); u != wantU || tt != wantT {
			t.Errorf("count: got={unread:%d,total:%d} want={unread:%d,total:%d}", u, tt, wantU, wantT)
		}
	}
	check(3, 3)

	c, b := reqInbox(t, srv, "POST", "/api/inbox/read-all", true)
	if c != http.StatusOK {
		t.Fatalf("read-all: got=%d want=200 body=%s", c, b)
	}
	var v struct{ Marked int }
	_ = json.Unmarshal(b, &v)
	if v.Marked != 3 {
		t.Errorf("marked: got=%d want=3", v.Marked)
	}
	check(0, 3)
	if n := len(listInbox(t, srv, "?filter=unread").Items); n != 0 {
		t.Errorf("filter=unread 的 items 长度: got=%d want=0", n)
	}
}

func TestInboxStaleKey409(t *testing.T) {
	e := newInboxEnv(t)
	ctx := context.Background()
	userID, repoID := e.user(t)
	tk := e.taskIn(t, userID, repoID, toFailedIB...)
	srv := e.server(t, userID)
	old := mustFind(t, listInbox(t, srv, "").Items, "task", tk.ID).Key

	type staleBody struct {
		Error   string      `json:"error"`
		Current *inbox.Item `json:"current"`
	}
	read := func() (int, staleBody) {
		c, b := reqInbox(t, srv, "POST", "/api/inbox/items/"+old+"/read", true)
		var v staleBody
		_ = json.Unmarshal(b, &v)
		return c, v
	}

	if _, err := e.m.Transition(ctx, tk.ID, task.StateQueued, "user:1", nil); err != nil {
		t.Fatal(err)
	}
	c, v := read()
	if c != http.StatusConflict || v.Current != nil {
		t.Errorf("重试排队后用旧键: got=%d current=%+v want=409 current=null", c, v.Current)
	}

	for _, s := range toFailedIB {
		if _, err := e.m.Transition(ctx, tk.ID, s, "system", nil); err != nil {
			t.Fatal(err)
		}
	}
	newKey := mustFind(t, listInbox(t, srv, "").Items, "task", tk.ID).Key
	c, v = read()
	if c != http.StatusConflict || v.Current == nil || v.Current.Key != newKey {
		t.Errorf("再次失败后用旧键: got=%d current=%+v want=409 current.key=%q", c, v.Current, newKey)
	}
}

func TestInboxEndpointsDoNotTransition(t *testing.T) {
	e := newInboxEnv(t)
	ctx := context.Background()
	userID, repoID := e.user(t)
	failed := e.taskIn(t, userID, repoID, toFailedIB...)
	gated := e.taskIn(t, userID, repoID, toAwaiting...)
	spec := e.taskIn(t, userID, repoID, toSpecIB...)
	p := e.prdReady(t, userID, repoID)
	ids := []int64{failed.ID, gated.ID, spec.ID}
	srv := e.server(t, userID)

	snapshot := func() string {
		var evs int
		if err := e.st.Pool().QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id = ANY($1)`, ids).Scan(&evs); err != nil {
			t.Fatal(err)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "events=%d;", evs)
		rows, err := e.st.Pool().Query(ctx, `
			SELECT 'task', id, state, updated_at FROM tasks WHERE id = ANY($1)
			UNION ALL SELECT 'prd', id, state, updated_at FROM prds WHERE id = $2
			ORDER BY 1, 2`, ids, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				kind, state string
				id          int64
				at          time.Time
			)
			if err := rows.Scan(&kind, &id, &state, &at); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&sb, "%s%d=%s@%s;", kind, id, state, at.Format(time.RFC3339Nano))
		}
		return sb.String()
	}

	before := snapshot()
	items := listInbox(t, srv, "?filter=all").Items
	fkey := mustFind(t, items, "task", failed.ID).Key
	pkey := mustFind(t, items, "prd", p.ID).Key
	calls := []struct{ method, path string }{
		{"GET", "/api/inbox?filter=all"},
		{"GET", "/api/inbox/count"},
		{"POST", "/api/inbox/items/" + pkey + "/read"},
		{"POST", "/api/inbox/items/" + pkey + "/unread"},
		{"POST", "/api/inbox/items/" + fkey + "/dismiss"},
		{"POST", "/api/inbox/read-all"},
	}
	for _, c := range calls {
		for i := 0; i < 2; i++ {
			if code, b := reqInbox(t, srv, c.method, c.path, true); code >= 500 {
				t.Errorf("%s %s 第 %d 次: got=%d body=%s", c.method, c.path, i+1, code, b)
			}
		}
	}
	if after := snapshot(); after != before {
		t.Errorf("收件箱端点改动了任务/PRD 或事件流:\nbefore=%s\nafter =%s", before, after)
	}
}

var inboxEndpoints = []struct{ method, path string }{
	{"GET", "/api/inbox"},
	{"GET", "/api/inbox/count"},
	{"POST", "/api/inbox/items/task:1:e1/read"},
	{"POST", "/api/inbox/items/task:1:e1/unread"},
	{"POST", "/api/inbox/items/task:1:e1/dismiss"},
	{"POST", "/api/inbox/read-all"},
}

func TestInboxRequiresAuth(t *testing.T) {
	e := newInboxEnv(t)
	userID, _ := e.user(t)
	srv := e.server(t, userID)
	for _, ep := range inboxEndpoints {
		t.Run(ep.method+" "+ep.path+" 未登录回 401", func(t *testing.T) {
			if code, _ := reqInbox(t, srv, ep.method, ep.path, false); code != http.StatusUnauthorized {
				t.Errorf("got=%d want=401", code)
			}
		})
	}
}

func TestInboxBadInput(t *testing.T) {
	e := newInboxEnv(t)
	userID, _ := e.user(t)
	srv := e.server(t, userID)
	cases := []struct {
		name, method, path, echo string
	}{
		{"filter 非法", "GET", "/api/inbox?filter=xxEVILxx", "xxEVILxx"},
		{"kind 非法", "GET", "/api/inbox?kind=xxEVILxx", "xxEVILxx"},
		{"键不合法", "POST", "/api/inbox/items/xxEVILxx/read", "xxEVILxx"},
		{"键带尾随空格", "POST", "/api/inbox/items/task:1:e2%20/read", "task:1:e2"},
		{"键 id 为 0", "POST", "/api/inbox/items/task:0:e1/dismiss", "task:0"},
	}
	for _, c := range cases {
		t.Run(c.name+" 回 400 且不回显输入", func(t *testing.T) {
			code, b := reqInbox(t, srv, c.method, c.path, true)
			if code != http.StatusBadRequest {
				t.Errorf("got=%d want=400 body=%s", code, b)
			}
			if bytes.Contains(b, []byte(c.echo)) {
				t.Errorf("响应体回显了输入 %q: %s", c.echo, b)
			}
		})
	}
}
