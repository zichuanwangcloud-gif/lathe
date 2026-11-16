package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/agent"
	"github.com/zichuanwangcloud-gif/lathe/internal/runner"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

func interjectPath(id int64) string     { return "/api/tasks/" + itoa(id) + "/interject" }
func interjectionsPath(id int64) string { return "/api/tasks/" + itoa(id) + "/interjections" }

func msgBody(s string) string {
	b, _ := json.Marshal(map[string]string{"message": s})
	return string(b)
}

// AC-11：状态码与落库。
func TestInterjectStatusCodes(t *testing.T) {
	type tc struct {
		name       string
		state      string
		live       func(ij *runner.Interjections) *fakeLive // nil = 无活会话
		supported  func(*task.Task) bool
		msg        string
		wantCode   int
		wantStatus string // 库里最后一行；"" = 不查
		wantErr    string // 响应 error 或库里 error 应含
		wantReason string
		check      func(t *testing.T, r ijDBRow)
	}
	ack := func(ij *runner.Interjections) *fakeLive { return &fakeLive{ij: ij, ack: true} }
	cases := []tc{
		{name: "implementing_活会话", state: "implementing", live: ack, wantCode: 200, wantStatus: "delivered",
			check: func(t *testing.T, r ijDBRow) {
				if r.deliveryID == "" || r.err != "" {
					t.Errorf("delivery_id 应非空、error 应为空：%+v", r)
				}
			}},
		{name: "verifying_修复轮在跑_活会话", state: "verifying", live: ack, wantCode: 200, wantStatus: "delivered"},
		{name: "implementing_回显2s内未到", state: "implementing",
			live: func(ij *runner.Interjections) *fakeLive { return &fakeLive{ij: ij} }, wantCode: 200, wantStatus: "delivered",
			check: func(t *testing.T, r ijDBRow) {
				if r.err != "no_replay_ack" {
					t.Errorf("error got=%q want=no_replay_ack", r.err)
				}
			}},
		{name: "implementing_无活会话", state: "implementing", wantCode: 202, wantStatus: "queued", wantReason: "no_live_session"},
		{name: "implementing_会话已出result", state: "implementing",
			live: func(ij *runner.Interjections) *fakeLive { return &fakeLive{ij: ij, closed: true} }, wantCode: 202, wantStatus: "queued"},
		{name: "verifying_无活会话", state: "verifying", wantCode: 202, wantStatus: "queued"},
		{name: "implementing_背压", state: "implementing",
			live:     func(ij *runner.Interjections) *fakeLive { return &fakeLive{ij: ij, err: agent.ErrDeliverBackpressure} },
			wantCode: 503, wantStatus: "failed", wantErr: "投递通道拥塞"},
		{name: "verifying_背压", state: "verifying",
			live:     func(ij *runner.Interjections) *fakeLive { return &fakeLive{ij: ij, err: agent.ErrDeliverBackpressure} },
			wantCode: 503, wantStatus: "failed", wantErr: "投递通道拥塞"},
		{name: "implementing_驱动不支持", state: "implementing", live: ack, supported: func(*task.Task) bool { return false },
			wantCode: 422, wantStatus: "failed", wantErr: "不支持运行中插话", wantReason: "driver_unsupported"},
		{name: "pr_open", state: "pr_open", wantCode: 409, wantStatus: "failed", wantErr: "没有运行中的 agent"},
		{name: "超长8193", state: "implementing", msg: strings.Repeat("a", 8193), wantCode: 413},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api, st, m, repoID := apiFixture(t)
			tk := ijTask(t, st, m, repoID, c.state)
			ij := wireInterjectorForOwnerTest(t, api)
			ij.Supported = c.supported
			var live *fakeLive
			if c.live != nil {
				live = c.live(ij)
				ij.Register(tk.ID, live, "implement", "sess")
			}
			srv := apiServer(t, api)
			msg := c.msg
			if msg == "" {
				msg = "别动 migration"
			}
			resp := do(t, srv, "POST", interjectPath(tk.ID), msgBody(msg), true)
			body := decode(t, resp)
			if resp.StatusCode != c.wantCode {
				t.Fatalf("status got=%d want=%d body=%v", resp.StatusCode, c.wantCode, body)
			}
			if c.wantReason != "" && body["reason"] != c.wantReason {
				t.Errorf("reason got=%v want=%s", body["reason"], c.wantReason)
			}
			if c.wantCode == 413 {
				if n := countInterjections(t, st, tk.ID); n != 0 {
					t.Errorf("413 不应落库，got %d 行", n)
				}
				return
			}
			r := lastInterjection(t, st, tk.ID)
			if r.status != c.wantStatus {
				t.Errorf("库里 status got=%s want=%s", r.status, c.wantStatus)
			}
			if c.wantErr != "" {
				msg, _ := body["error"].(string)
				if !strings.Contains(msg, c.wantErr) || !strings.Contains(r.err, c.wantErr) {
					t.Errorf("响应 error=%q 库里 error=%q，都应含 %q", msg, r.err, c.wantErr)
				}
			}
			if c.check != nil {
				c.check(t, r)
			}
			if c.wantCode == 422 && live.callCount() != 0 {
				t.Errorf("驱动不支持时 Deliver 调用次数 got=%d want=0", live.callCount())
			}
		})
	}

	t.Run("窗口内第6条_429", func(t *testing.T) {
		api, st, m, repoID := apiFixture(t)
		tk := ijTask(t, st, m, repoID, "implementing")
		wireInterjectorForOwnerTest(t, api)
		srv := apiServer(t, api)
		for i := 1; i <= 6; i++ {
			resp := do(t, srv, "POST", interjectPath(tk.ID), msgBody(fmt.Sprintf("第 %d 条", i)), true)
			want := http.StatusAccepted
			if i == 6 {
				want = http.StatusTooManyRequests
			}
			if resp.StatusCode != want {
				t.Errorf("第 %d 条 status got=%d want=%d", i, resp.StatusCode, want)
			}
		}
		if n := countInterjections(t, st, tk.ID); n != 5 {
			t.Errorf("DB 行数 got=%d want=5（第 6 条不落库）", n)
		}
	})

	t.Run("Interjector为nil_503", func(t *testing.T) {
		api, st, m, repoID := apiFixture(t)
		tk := ijTask(t, st, m, repoID, "implementing")
		srv := apiServer(t, api)
		for _, c := range []struct{ method, path, body string }{
			{"POST", interjectPath(tk.ID), msgBody("x")},
			{"GET", interjectionsPath(tk.ID), ""},
		} {
			if resp := do(t, srv, c.method, c.path, c.body, true); resp.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("%s status got=%d want=503", c.method, resp.StatusCode)
			}
		}
	})
}

// AC-12：配置项真的被消费 —— 限流 2 条、长度上限 10。
func TestInterjectRateLimitAndConfig(t *testing.T) {
	api, st, m, repoID := apiFixture(t)
	tk := ijTask(t, st, m, repoID, "implementing")
	wireInterjectorForOwnerTest(t, api)
	api.InterjectLimits = InterjectLimits{MaxChars: 10, RateLimit: 2, Window: time.Minute}
	srv := apiServer(t, api)

	cases := []struct {
		name string
		msg  string
		want int
	}{
		{"10 字放行", strings.Repeat("字", 10), http.StatusAccepted},
		{"11 字 413", strings.Repeat("字", 11), http.StatusRequestEntityTooLarge},
		{"第 2 条放行", "ok", http.StatusAccepted},
		{"第 3 条 429", "ok", http.StatusTooManyRequests},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := do(t, srv, "POST", interjectPath(tk.ID), msgBody(c.msg), true).StatusCode; got != c.want {
				t.Errorf("status got=%d want=%d", got, c.want)
			}
		})
	}
}

// AC-18：状态是闸门 —— 有活会话但任务已 cancelled → 409、落 failed、Deliver 0 次。
func TestInterjectRejectedAfterCancelEvenWithLiveSession(t *testing.T) {
	api, st, m, repoID := apiFixture(t)
	tk := ijTask(t, st, m, repoID, "implementing")
	ij := wireInterjectorForOwnerTest(t, api)
	live := &fakeLive{ij: ij, ack: true}
	ij.Register(tk.ID, live, "implement", "s")
	if _, err := m.Transition(context.Background(), tk.ID, task.StateCancelled, "user:x", nil); err != nil {
		t.Fatal(err)
	}
	srv := apiServer(t, api)
	resp := do(t, srv, "POST", interjectPath(tk.ID), msgBody("还在吗"), true)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status got=%d want=409", resp.StatusCode)
	}
	if r := lastInterjection(t, st, tk.ID); r.status != "failed" {
		t.Errorf("库里 got=%s want=failed", r.status)
	}
	if n := live.callCount(); n != 0 {
		t.Errorf("Deliver 调用次数 got=%d want=0", n)
	}
}

// AC-21：驱动不支持 —— 任务 pr_open 时状态先判 → 409 而不是 422。
func TestInterjectDriverUnsupported(t *testing.T) {
	api, st, m, repoID := apiFixture(t)
	tk := ijTask(t, st, m, repoID, "pr_open")
	ij := wireInterjectorForOwnerTest(t, api)
	ij.Supported = func(*task.Task) bool { return false }
	srv := apiServer(t, api)
	if got := do(t, srv, "POST", interjectPath(tk.ID), msgBody("x"), true).StatusCode; got != http.StatusConflict {
		t.Errorf("pr_open + 驱动不支持 status got=%d want=409（状态先判）", got)
	}
	// implementing + 不支持 → 422 由 TestInterjectStatusCodes 的同名行覆盖
}

// AC-13 的 GET 出口：GET /interjections 响应不含明文。
func TestInterjectRedactsInListResponse(t *testing.T) {
	api, st, m, repoID := apiFixture(t)
	tk := ijTask(t, st, m, repoID, "implementing")
	ij := wireInterjectorForOwnerTest(t, api)
	known := "kn0wn-" + uniqueIssueKey("secret")
	ij.KnownSecrets = []string{known}
	srv := apiServer(t, api)
	ghp := "ghp_" + strings.Repeat("Q7w", 12)
	bearer := strings.Repeat("Mn4", 7)[:20]
	msg := "token " + ghp + " Authorization: Bearer " + bearer + " 口令 " + known
	if got := do(t, srv, "POST", interjectPath(tk.ID), msgBody(msg), true).StatusCode; got != http.StatusAccepted {
		t.Fatalf("status got=%d want=202", got)
	}
	raw := rawBody(t, do(t, srv, "GET", interjectionsPath(tk.ID)+"?after_rev=0", "", true))
	for _, sec := range []string{ghp, bearer, known} {
		if strings.Contains(raw, sec) {
			t.Errorf("② GET 响应含明文 %q：%s", sec, raw)
		}
	}
	if !strings.Contains(raw, "[REDACTED]") {
		t.Errorf("② GET 响应应含 [REDACTED]：%s", raw)
	}
}

func rawBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

type listResp struct {
	Interjections []struct {
		ID     int64   `json:"id"`
		Status string  `json:"status"`
		Error  *string `json:"error"`
		Rev    int64   `json:"rev"`
	} `json:"interjections"`
	Rev  int64 `json:"rev"`
	More bool  `json:"more"`
}

func getList(t *testing.T, srvURL string, client *http.Client, id int64, after string) (int, listResp) {
	t.Helper()
	req, _ := http.NewRequest("GET", srvURL+interjectionsPath(id)+"?after_rev="+after, nil)
	req.Header.Set("Authorization", "Bearer "+apiTestToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var lr listResp
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, lr
}

// AC-17：/interjections 增量游标。
func TestInterjectionsRevCursor(t *testing.T) {
	api, st, m, repoID := apiFixture(t)
	tk := ijTask(t, st, m, repoID, "verifying")
	ij := wireInterjectorForOwnerTest(t, api)
	srv := apiServer(t, api)
	ctx := context.Background()

	// 1. 插 1 条（queued）
	if got := do(t, srv, "POST", interjectPath(tk.ID), msgBody("一"), true).StatusCode; got != http.StatusAccepted {
		t.Fatalf("status got=%d", got)
	}
	_, first := getList(t, srv.URL, srv.Client(), tk.ID, "0")
	if len(first.Interjections) != 1 {
		t.Fatalf("首次 got=%d 行 want=1", len(first.Interjections))
	}
	r1 := first.Rev
	// 2. 空批原样回显
	_, empty := getList(t, srv.URL, srv.Client(), tk.ID, itoa(r1))
	if len(empty.Interjections) != 0 || empty.Rev != r1 {
		t.Errorf("空批 got=(%d 行, rev=%d) want=(0, %d)", len(empty.Interjections), empty.Rev, r1)
	}
	// 3. 仅由结算把它改成 failed（agent_events 游标完全空闲）
	if _, err := st.Pool().Exec(ctx, `UPDATE tasks SET state='pr_open' WHERE id=$1`, tk.ID); err != nil {
		t.Fatal(err)
	}
	ij.SettleTask(ctx, tk.ID)
	_, changed := getList(t, srv.URL, srv.Client(), tk.ID, itoa(r1))
	if len(changed.Interjections) != 1 || changed.Interjections[0].ID != first.Interjections[0].ID ||
		changed.Interjections[0].Status != "failed" || changed.Rev <= r1 {
		t.Errorf("结算后 got=%+v want 同一 id、failed、rev>%d", changed, r1)
	}
	// 4. 205 条 → 200 + more，再拉 5 + !more
	if _, err := st.Pool().Exec(ctx, `UPDATE tasks SET state='verifying' WHERE id=$1`, tk.ID); err != nil {
		t.Fatal(err)
	}
	cursor := changed.Rev
	for i := 0; i < 205; i++ {
		if _, _, err := st.InsertInterjectionGated(ctx, tk.ID, tk.UserID, fmt.Sprintf("批量 %d", i), func(string) string { return "" }); err != nil {
			t.Fatal(err)
		}
	}
	_, p1 := getList(t, srv.URL, srv.Client(), tk.ID, itoa(cursor))
	_, p2 := getList(t, srv.URL, srv.Client(), tk.ID, itoa(p1.Rev))
	if len(p1.Interjections) != 200 || !p1.More || len(p2.Interjections) != 5 || p2.More {
		t.Errorf("分页 got=(%d,%v)+(%d,%v) want=(200,true)+(5,false)", len(p1.Interjections), p1.More, len(p2.Interjections), p2.More)
	}
	// 5. 非法 after_rev
	for _, bad := range []string{"-1", "abc"} {
		if code, _ := getList(t, srv.URL, srv.Client(), tk.ID, bad); code != http.StatusBadRequest {
			t.Errorf("after_rev=%s got=%d want=400", bad, code)
		}
	}
}

// AC-17 第 6 条：两个 goroutine 各插 50 条 + 每 10ms 轮询，upsert 出的集合 = 库里 100 行。
func TestInterjectionsRevCursorConcurrent(t *testing.T) {
	api, st, m, repoID := apiFixture(t)
	tk := ijTask(t, st, m, repoID, "implementing")
	wireInterjectorForOwnerTest(t, api)
	srv := apiServer(t, api)
	ctx := context.Background()

	seen := map[int64]bool{}
	var cursor int64
	stop := make(chan struct{})
	pollDone := make(chan struct{})
	poll := func() {
		for {
			_, lr := getList(t, srv.URL, srv.Client(), tk.ID, itoa(cursor))
			for _, r := range lr.Interjections {
				seen[r.ID] = true
			}
			cursor = lr.Rev
			if !lr.More {
				return
			}
		}
	}
	go func() {
		defer close(pollDone)
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
				poll()
			}
		}
	}()
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if _, _, err := st.InsertInterjectionGated(ctx, tk.ID, tk.UserID, fmt.Sprintf("g%d-%d", g, i), func(string) string { return "" }); err != nil {
					t.Error(err)
				}
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	<-pollDone
	poll() // 收尾一拉
	if got := len(seen); got != 100 {
		t.Errorf("客户端 upsert 出的集合 got=%d want=100（游标漏行）", got)
	}
}

// AC-19（R3 行）：Execute 不再运行 —— 无会话插 1 条后任务回到 queued，
// 直接调真 cancelTask，GET /interjections 读时结算出 failed「已取消」。
func TestInterjectSettledOnReadAfterCancelTask(t *testing.T) {
	api, st, m, repoID := apiFixture(t)
	tk := ijTask(t, st, m, repoID, "implementing")
	wireInterjectorForOwnerTest(t, api)
	srv := apiServer(t, api)
	resp := do(t, srv, "POST", interjectPath(tk.ID), msgBody("取消前说的"), true)
	inserted := decode(t, resp)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status got=%d", resp.StatusCode)
	}
	insRev := int64(inserted["rev"].(float64))
	// Execute 返回时任务被重新入队（行保留），不起 worker
	if _, err := st.Pool().Exec(context.Background(), `UPDATE tasks SET state='queued' WHERE id=$1`, tk.ID); err != nil {
		t.Fatal(err)
	}
	if got := do(t, srv, "POST", "/api/tasks/"+itoa(tk.ID)+"/cancel", "", true).StatusCode; got != http.StatusOK {
		t.Fatalf("cancel status got=%d", got)
	}
	_, lr := getList(t, srv.URL, srv.Client(), tk.ID, "0")
	if len(lr.Interjections) != 1 {
		t.Fatalf("got %d 行", len(lr.Interjections))
	}
	r := lr.Interjections[0]
	if r.Status != "failed" || r.Error == nil || !strings.Contains(*r.Error, "已取消") || r.Rev <= insRev {
		t.Errorf("got=%+v want failed + 已取消 + rev>%d", r, insRev)
	}
}

// AC-20：客户端断开不留 delivering。
func TestSubmitSurvivesClientDisconnect(t *testing.T) {
	type hookSet struct {
		before, afterMark, awaiting bool
	}
	cases := []struct {
		name       string
		state      string
		live       func(ij *runner.Interjections) *fakeLive
		cancelAt   hookSet
		toPROpen   bool // 断开后、Deliver 前把任务转走
		wantRows   int
		wantStatus string
		wantErr    string
	}{
		{name: "等ack时断开", state: "implementing",
			live:     func(ij *runner.Interjections) *fakeLive { return &fakeLive{ij: ij} }, // 不回显
			cancelAt: hookSet{awaiting: true}, wantRows: 1, wantStatus: "delivered", wantErr: "no_replay_ack"},
		{name: "Deliver前断开_会话收尾_任务已pr_open", state: "implementing",
			live:     func(ij *runner.Interjections) *fakeLive { return &fakeLive{ij: ij, err: agent.ErrSessionClosed} },
			cancelAt: hookSet{afterMark: true}, toPROpen: true, wantRows: 1, wantStatus: "failed", wantErr: "没有后续 agent 轮次"},
		{name: "事务A提交前断开", state: "implementing",
			live:     func(ij *runner.Interjections) *fakeLive { return &fakeLive{ij: ij, ack: true} },
			cancelAt: hookSet{before: true}, wantRows: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api, st, m, repoID := apiFixture(t)
			tk := ijTask(t, st, m, repoID, c.state)
			ij := wireInterjectorForOwnerTest(t, api)
			ij.Register(tk.ID, c.live(ij), "implement", "s")

			reqCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			handlerDone := make(chan struct{})
			hit := make(chan struct{}, 1)
			signal := func() {
				cancel()
				select {
				case hit <- struct{}{}:
				default:
				}
			}
			if c.cancelAt.before {
				ij.Hooks.BeforeInsert = func(ctx context.Context) {
					signal()
					<-ctx.Done() // 等请求 ctx 真的被取消，事务 A 才会失败
				}
			}
			if c.cancelAt.afterMark {
				ij.Hooks.AfterMarkDelivering = func() {
					signal()
					if c.toPROpen {
						_, _ = st.Pool().Exec(context.Background(), `UPDATE tasks SET state='pr_open' WHERE id=$1`, tk.ID)
					}
				}
			}
			if c.cancelAt.awaiting {
				ij.Hooks.AwaitingAck = signal
			}

			mux := http.NewServeMux()
			api.Routes(mux)
			wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() {
					if strings.HasSuffix(r.URL.Path, "/interject") {
						close(handlerDone)
					}
				}()
				mux.ServeHTTP(w, r)
			})
			srv := newTestServer(t, http.NewServeMux())
			srv.srv.Config.Handler = wrapped

			start := time.Now()
			req, _ := http.NewRequestWithContext(reqCtx, "POST", srv.srv.URL+interjectPath(tk.ID), strings.NewReader(msgBody("断开测试")))
			req.Header.Set("Authorization", "Bearer "+apiTestToken)
			go func() {
				resp, err := srv.srv.Client().Do(req)
				if err == nil {
					_ = resp.Body.Close()
				}
			}()
			select {
			case <-hit:
			case <-time.After(5 * time.Second):
				t.Fatal("hook 没被触发")
			}
			select {
			case <-handlerDone:
			case <-time.After(5 * time.Second):
				t.Fatal("handler 5s 内没返回")
			}
			if el := time.Since(start); el > 3*time.Second {
				t.Errorf("handler 耗时 got=%v want≤3s（ack 超时 2s + 写库）", el)
			}
			if n := countInterjections(t, st, tk.ID); n != c.wantRows {
				t.Fatalf("行数 got=%d want=%d", n, c.wantRows)
			}
			if c.wantRows == 0 {
				return
			}
			r := lastInterjection(t, st, tk.ID)
			if r.status != c.wantStatus || !strings.Contains(r.err, c.wantErr) {
				t.Errorf("got=(%s,%q) want=(%s,含%q)", r.status, r.err, c.wantStatus, c.wantErr)
			}
		})
	}
}

// 迟到回显（AC-22 实测：CLI 只在当前步骤 —— 一次 API 往返或一次工具调用 —— 结束、
// 把排队的输入并进对话时才回显，--resume 的修复轮里常常超过 2s）。
// 给定：活会话写入成功但 2s 内不回显 → 200 delivered + no_replay_ack；
// 断言：回显随后到达时 error 被清空且 rev 前进（界面轮询能拿到）；
// 会话已注销后到达的回显不再改行。
func TestInterjectLateReplayAckClearsError(t *testing.T) {
	type row struct {
		status, err, deliveryID string
		rev                     int64
	}
	read := func(t *testing.T, api *API, taskID int64) row {
		t.Helper()
		var r row
		if err := api.Store.Pool().QueryRow(context.Background(), `
			SELECT status, coalesce(error,''), coalesce(delivery_id::text,''), rev
			FROM task_interjections WHERE task_id=$1 ORDER BY id DESC LIMIT 1`, taskID).
			Scan(&r.status, &r.err, &r.deliveryID, &r.rev); err != nil {
			t.Fatalf("读插话行失败: %v", err)
		}
		return r
	}
	cases := []struct {
		name         string
		unregister   bool // 回显到达前会话已注销（进程已退出）
		wantErrAfter string
	}{
		{name: "会话仍在_迟到回显清空no_replay_ack", wantErrAfter: ""},
		{name: "会话已注销_迟到回显不改行", unregister: true, wantErrAfter: "no_replay_ack"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api, st, m, repoID := apiFixture(t)
			tk := ijTask(t, st, m, repoID, "implementing")
			ij := wireInterjectorForOwnerTest(t, api)
			ij.Register(tk.ID, &fakeLive{ij: ij}, "fix-1", "sess") // 不回显
			srv := apiServer(t, api)

			resp := do(t, srv, "POST", interjectPath(tk.ID), msgBody("迟到回显"), true)
			body := decode(t, resp)
			if resp.StatusCode != 200 || body["error"] != "no_replay_ack" {
				t.Fatalf("got status=%d error=%v want 200 + no_replay_ack", resp.StatusCode, body["error"])
			}
			before := read(t, api, tk.ID)
			if before.status != "delivered" || before.err != "no_replay_ack" || before.deliveryID == "" {
				t.Fatalf("回显前 got=%+v want delivered/no_replay_ack/有 delivery_id", before)
			}
			if c.unregister {
				ij.Unregister(tk.ID)
			}
			ij.Ack(before.deliveryID) // 迟到的 isReplay 回显（读协程上调用）

			var after row
			deadline := time.Now().Add(3 * time.Second)
			for {
				after = read(t, api, tk.ID)
				if after.err == c.wantErrAfter && (c.wantErrAfter != "" || after.rev > before.rev) {
					break
				}
				if time.Now().After(deadline) {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if c.unregister {
				time.Sleep(300 * time.Millisecond)
				after = read(t, api, tk.ID)
			}
			if after.status != "delivered" || after.err != c.wantErrAfter {
				t.Errorf("回显后 got=(%s,%q) want=(delivered,%q)", after.status, after.err, c.wantErrAfter)
			}
			if c.wantErrAfter == "" && after.rev <= before.rev {
				t.Errorf("清空 error 后 rev 应前进：got=%d before=%d", after.rev, before.rev)
			}
			if c.wantErrAfter != "" && after.rev != before.rev {
				t.Errorf("不改行时 rev 不应变：got=%d want=%d", after.rev, before.rev)
			}
		})
	}
}

// ackDuringDeliveredStore 包住真 store：MarkInterjectionDelivered 在按
// no_replay_ack 收尾时先注入那条投递的回显、再慢一拍才真写 —— 模拟回显恰好
// 落在 2s 超时之后、收尾写完成之前。MarkInterjectionReplayAcked 计次。
type ackDuringDeliveredStore struct {
	*store.Store
	ij *runner.Interjections

	mu         sync.Mutex
	deliveryID string
	acked      int
}

func (s *ackDuringDeliveredStore) MarkInterjectionDelivering(ctx context.Context, taskID, id int64, deliveryID, phase, sessionID string) (*store.Interjection, error) {
	s.mu.Lock()
	s.deliveryID = deliveryID
	s.mu.Unlock()
	return s.Store.MarkInterjectionDelivering(ctx, taskID, id, deliveryID, phase, sessionID)
}

func (s *ackDuringDeliveredStore) MarkInterjectionDelivered(ctx context.Context, taskID, id int64, errText string) (*store.Interjection, error) {
	if errText == store.InterjectionNoReplayAck {
		s.mu.Lock()
		d := s.deliveryID
		s.mu.Unlock()
		s.ij.Ack(d) // 读协程上的回显，落在超时之后、收尾之前
		time.Sleep(200 * time.Millisecond)
	}
	return s.Store.MarkInterjectionDelivered(ctx, taskID, id, errText)
}

func (s *ackDuringDeliveredStore) MarkInterjectionReplayAcked(ctx context.Context, taskID, id int64, deliveryID string) (*store.Interjection, error) {
	s.mu.Lock()
	s.acked++
	s.mu.Unlock()
	return s.Store.MarkInterjectionReplayAcked(ctx, taskID, id, deliveryID)
}

func (s *ackDuringDeliveredStore) ackedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acked
}

// 给定：活会话写入成功、2s 内不回显，回显在慢的 MarkInterjectionDelivered 里
// 到达（超时之后、收尾之前）。断言：200 响应体里 error 已是 null（当场确认，
// 不必等界面下一次轮询）；只写一次「清空 no_replay_ack」；之后同一 deliveryID
// 的重复回显不再起迟到写入（没有登记进 late 表）。
func TestInterjectAckDuringSlowMarkDelivered(t *testing.T) {
	api, st, m, repoID := apiFixture(t)
	tk := ijTask(t, st, m, repoID, "implementing")
	wrapped := &ackDuringDeliveredStore{Store: st}
	ij := &runner.Interjections{Store: wrapped, Tasks: api.Tasks}
	wrapped.ij = ij
	api.Interjector = ij
	ij.Register(tk.ID, &fakeLive{ij: ij}, "fix-1", "sess") // 不回显
	defer ij.Unregister(tk.ID)
	srv := apiServer(t, api)

	resp := do(t, srv, "POST", interjectPath(tk.ID), msgBody("收尾途中的回显"), true)
	body := decode(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("got status=%d want 200（body=%v）", resp.StatusCode, body)
	}
	if v, ok := body["error"]; !ok || v != nil {
		t.Errorf("响应体 error got=%v（存在=%v）want=null", v, ok)
	}
	if body["status"] != "delivered" {
		t.Errorf("响应体 status got=%v want=delivered", body["status"])
	}
	if got := wrapped.ackedCount(); got != 1 {
		t.Errorf("响应返回时清空写 got=%d 次 want=1", got)
	}

	wrapped.mu.Lock()
	d := wrapped.deliveryID
	wrapped.mu.Unlock()
	ij.Ack(d) // 重复回显：若登记进了 late 表，这里会再起一次迟到写
	time.Sleep(300 * time.Millisecond)
	if got := wrapped.ackedCount(); got != 1 {
		t.Errorf("重复回显后清空写 got=%d 次 want=1（不应再起迟到写入）", got)
	}
	var errCol *string
	if err := st.Pool().QueryRow(context.Background(),
		`SELECT error FROM task_interjections WHERE task_id=$1 ORDER BY id DESC LIMIT 1`, tk.ID).Scan(&errCol); err != nil {
		t.Fatalf("读插话行失败: %v", err)
	}
	if errCol != nil {
		t.Errorf("库里 error got=%q want=NULL", *errCol)
	}
}
