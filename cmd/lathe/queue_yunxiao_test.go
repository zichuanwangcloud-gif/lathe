package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/httpapi"
	"github.com/zichuanwangcloud-gif/lathe/internal/integration/yunxiao"
	"github.com/zichuanwangcloud-gif/lathe/internal/runner"
	"github.com/zichuanwangcloud-gif/lathe/internal/secret"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
	"github.com/zichuanwangcloud-gif/lathe/internal/tracker"
)

// recTracker 记录回帖（按 provider 分派出来的假 tracker）。
type recTracker struct {
	mu       sync.Mutex
	refs     []string
	comments []string
}

func (r *recTracker) Issue(ctx context.Context, ref string) (*tracker.Issue, error) {
	return nil, errors.New("unused")
}

func (r *recTracker) Comment(ctx context.Context, ref, body string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = append(r.refs, ref)
	r.comments = append(r.comments, body)
	return "c", nil
}

type recClients struct {
	tr        *recTracker
	providers []string
}

func (c *recClients) Tracker(ctx context.Context, provider string) (tracker.Tracker, error) {
	c.providers = append(c.providers, provider)
	return c.tr, nil
}

func (c *recClients) GitHub(ctx context.Context) (runner.GitHubAPI, error) {
	return nil, errors.New("unused")
}

type recFactory struct{ c *recClients }

func (f recFactory) ForUser(ctx context.Context, userID int64) (runner.Clients, error) {
	return f.c, nil
}

// AC-17
func TestQueueYunxiao(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	q := testQueue(st, &fakePipeline{})

	t.Run("creates_yunxiao_row", func(t *testing.T) {
		userID, repoID := fixture(t, st)
		wid, serial := uniqueKey("wi"), uniqueKey("YX")
		id, err := q.EnqueueYunxiao(ctx, userID, wid, serial)
		if err != nil {
			t.Fatalf("EnqueueYunxiao: got %v want nil", err)
		}
		tk, err := q.tasks.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if tk.TrackerProvider != tracker.ProviderYunxiao || tk.ExternalID == nil || *tk.ExternalID != wid ||
			tk.ExternalKey != serial || tk.State != task.StateQueued || tk.UserID != userID || tk.RepoID != repoID {
			t.Errorf("任务行 got provider=%s id=%v key=%s state=%s user=%d repo=%d", tk.TrackerProvider, tk.ExternalID, tk.ExternalKey, tk.State, tk.UserID, tk.RepoID)
		}
	})
	t.Run("second_enqueue_task_active", func(t *testing.T) {
		userID, _ := fixture(t, st)
		wid, serial := uniqueKey("wi"), uniqueKey("YX")
		if _, err := q.EnqueueYunxiao(ctx, userID, wid, serial); err != nil {
			t.Fatal(err)
		}
		_, err := q.EnqueueYunxiao(ctx, userID, wid, serial)
		if !errors.Is(err, httpapi.ErrYunxiaoTaskActive) {
			t.Errorf("第二次 got %v want ErrYunxiaoTaskActive", err)
		}
	})
	t.Run("cancel_only_same_owner_key_provider", func(t *testing.T) {
		userID, repoID := fixture(t, st)
		otherID, _ := fixture(t, st)
		key := uniqueKey("SAME")
		yx, err := q.EnqueueYunxiao(ctx, userID, uniqueKey("wi"), key)
		if err != nil {
			t.Fatal(err)
		}
		otherYX, err := q.EnqueueYunxiao(ctx, otherID, uniqueKey("wi"), key)
		if err != nil {
			t.Fatal(err)
		}
		lin, err := q.tasks.Create(ctx, task.CreateParams{
			UserID: userID, RepoID: repoID, ExternalKey: key, ExternalID: "uuid-" + key,
			TrackerProvider: tracker.ProviderLinear,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids, err := q.CancelForYunxiao(ctx, userID, key)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 1 || ids[0] != yx {
			t.Errorf("取消的任务 got %v want [%d]", ids, yx)
		}
		for _, c := range []struct {
			id   int64
			want task.State
		}{{yx, task.StateCancelled}, {otherYX, task.StateQueued}, {lin.ID, task.StateQueued}} {
			tk, _ := q.tasks.Get(ctx, c.id)
			if tk.State != c.want {
				t.Errorf("任务 %d state got %s want %s", c.id, tk.State, c.want)
			}
		}
		if ids, err := q.CancelForYunxiao(ctx, userID, "  "); err != nil || len(ids) != 0 {
			t.Errorf("空 key got (%v, %v) want (nil, nil)", ids, err)
		}
	})
	t.Run("resolve_repo_failure_comments", func(t *testing.T) {
		rc := &recClients{tr: &recTracker{}}
		q2 := testQueue(st, &fakePipeline{})
		q2.clients = recFactory{c: rc}
		var noRepoUser int64
		if err := st.Pool().QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`,
			fmt.Sprintf("queue-yx-norepo-%d@example.com", time.Now().UnixNano())).Scan(&noRepoUser); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = st.Pool().Exec(context.Background(), `DELETE FROM users WHERE id = $1`, noRepoUser) })
		wid := uniqueKey("wi")
		_, err := q2.EnqueueYunxiao(ctx, noRepoUser, wid, "YX-1")
		if !errors.Is(err, httpapi.ErrYunxiaoNoRepo) {
			t.Errorf("got %v want ErrYunxiaoNoRepo", err)
		}
		if len(rc.providers) != 1 || rc.providers[0] != tracker.ProviderYunxiao {
			t.Errorf("回帖应按 yunxiao 分派 tracker，got %v", rc.providers)
		}
		if len(rc.tr.refs) != 1 || rc.tr.refs[0] != wid || !strings.Contains(rc.tr.comments[0], "无法接单") {
			t.Errorf("回帖 got refs=%v comments=%v want 1 条到 %s", rc.tr.refs, rc.tr.comments, wid)
		}
	})
}

// AC-25 端到端：真实 queue + 真实 task.Machine（真实唯一索引）+ httptest 云效。
func TestYunxiaoRunEndToEnd(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	userID, repoID := fixture(t, st)
	q := testQueue(st, &fakePipeline{})

	wid := strings.ReplaceAll(uniqueKey("wi"), "-", "")
	serial := "E2E-" + wid
	yx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/workitems/"+wid) {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": wid, "serialNumber": serial, "subject": "e2e"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(yx.Close)

	key := make([]byte, secret.KeySize)
	sealer, _ := secret.New(key)
	conns := st.NewYunxiaoConnections(sealer)
	if err := conns.Save(ctx, userID, store.YunxiaoSaveParams{
		Endpoint: yunxiao.DefaultEndpoint, OrganizationID: "org1", Token: "tok-" + wid,
	}); err != nil {
		t.Fatal(err)
	}
	auth := httpapi.NewAuth("e2e-token-" + wid)
	auth.Bootstrap = &store.User{ID: userID, Email: "e2e@example.com", Role: store.RoleMember}
	mux := http.NewServeMux()
	(&httpapi.YunxiaoAPI{
		Conns: conns, Store: st, Auth: auth, Tasks: q,
		ClientFor: func(cfg yunxiao.Config) (*yunxiao.Client, error) {
			cfg.Endpoint = yx.URL
			return yunxiao.NewClientWithHTTP(cfg, yx.Client(), true)
		},
	}).Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	run := func(t *testing.T) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/yunxiao/workitems/"+wid+"/run", nil)
		req.Header.Set("Authorization", "Bearer e2e-token-"+wid)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	activeRows := func(t *testing.T) []*task.Task {
		t.Helper()
		rows, err := q.tasks.ActiveByKey(ctx, userID, tracker.ProviderYunxiao, serial)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}

	t.Run("created_201_yunxiao_row", func(t *testing.T) {
		code, body := run(t)
		if code != http.StatusCreated {
			t.Fatalf("got %d %s want 201", code, body)
		}
		rows := activeRows(t)
		if len(rows) != 1 {
			t.Fatalf("活任务 got %d want 1", len(rows))
		}
		tk := rows[0]
		if tk.UserID != userID || tk.RepoID != repoID || tk.ExternalID == nil || *tk.ExternalID != wid ||
			tk.ExternalKey != serial || tk.State != task.StateQueued || tk.TrackerProvider != tracker.ProviderYunxiao {
			t.Errorf("任务行 got user=%d repo=%d id=%v key=%s state=%s provider=%s", tk.UserID, tk.RepoID, tk.ExternalID, tk.ExternalKey, tk.State, tk.TrackerProvider)
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(body), &m)
		if fmt.Sprint(m["taskId"]) != fmt.Sprint(tk.ID) || m["externalKey"] != serial {
			t.Errorf("响应 got %v want taskId=%d externalKey=%s", m, tk.ID, serial)
		}
	})
	t.Run("second_run_409_single_active_row", func(t *testing.T) {
		if code, body := run(t); code != http.StatusConflict {
			t.Errorf("got %d %s want 409", code, body)
		}
		if rows := activeRows(t); len(rows) != 1 {
			t.Errorf("活任务 got %d want 1", len(rows))
		}
	})
}
