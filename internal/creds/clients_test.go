package creds

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/linear"
	"github.com/zichuanwangcloud-gif/lathe/internal/integration/yunxiao"
	"github.com/zichuanwangcloud-gif/lathe/internal/secret"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/testsupport"
	"github.com/zichuanwangcloud-gif/lathe/internal/tracker"
)

func credsStore(t *testing.T) *store.Store {
	t.Helper()
	ctx, cancel := testsupport.ConnectContext(t)
	defer cancel()
	st, err := store.Open(ctx, testsupport.DSN())
	if err != nil {
		testsupport.SkipOrFail(t, "打开 store 失败: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func credsUser(t *testing.T, st *store.Store) int64 {
	t.Helper()
	var id int64
	if err := st.Pool().QueryRow(context.Background(), `INSERT INTO users (email) VALUES ($1) RETURNING id`,
		fmt.Sprintf("creds-%d@example.com", time.Now().UnixNano())).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.Pool().Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id) })
	return id
}

// AC-12（creds 部分）
func TestClientsTracker(t *testing.T) {
	st := credsStore(t)
	key := make([]byte, secret.KeySize)
	sealer, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}
	yc := st.NewYunxiaoConnections(sealer)
	ctx := context.Background()

	configured := credsUser(t, st)
	if err := yc.Save(ctx, configured, store.YunxiaoSaveParams{
		Endpoint: yunxiao.DefaultEndpoint, OrganizationID: "org1",
		Token: fmt.Sprintf("tok-%d", time.Now().UnixNano()), StatusStarted: "处理中",
	}); err != nil {
		t.Fatal(err)
	}
	unconfigured := credsUser(t, st)

	// 管理员（configured）带 Linear 环境变量兜底，让 Linear 分支能构造出客户端
	f := NewFactory(st.NewSecrets(sealer), EnvFallback{LinearToken: "lin_api_test"}, configured, st).WithYunxiao(yc)
	clientsFor := func(t *testing.T, uid int64) *Clients {
		t.Helper()
		c, err := f.ForUser(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		return c.(*Clients)
	}

	t.Run("yunxiao_is_status_syncer", func(t *testing.T) {
		tr, err := clientsFor(t, configured).Tracker(ctx, tracker.ProviderYunxiao)
		if err != nil {
			t.Fatalf("got err %v want nil", err)
		}
		if _, ok := tr.(tracker.StatusSyncer); !ok {
			t.Errorf("got %T, want 实现 tracker.StatusSyncer", tr)
		}
		if _, ok := tr.(*yunxiao.Tracker); !ok {
			t.Errorf("got %T want *yunxiao.Tracker", tr)
		}
	})
	t.Run("yunxiao_unconfigured", func(t *testing.T) {
		_, err := clientsFor(t, unconfigured).Tracker(ctx, tracker.ProviderYunxiao)
		if err == nil || !strings.Contains(err.Error(), "云效连接未配置") {
			t.Errorf("got %v want 含「云效连接未配置」", err)
		}
	})
	t.Run("empty_is_linear", func(t *testing.T) {
		tr, err := clientsFor(t, configured).Tracker(ctx, "")
		if _, ok := tr.(*linear.Client); err != nil || !ok {
			t.Errorf("got (%T, %v) want *linear.Client", tr, err)
		}
	})
	t.Run("linear_is_linear", func(t *testing.T) {
		tr, err := clientsFor(t, configured).Tracker(ctx, tracker.ProviderLinear)
		if _, ok := tr.(*linear.Client); err != nil || !ok {
			t.Errorf("got (%T, %v) want *linear.Client", tr, err)
		}
		if _, ok := tr.(tracker.StatusSyncer); ok {
			t.Error("Linear 不应实现 StatusSyncer")
		}
	})
	t.Run("bogus_unknown", func(t *testing.T) {
		_, err := clientsFor(t, configured).Tracker(ctx, "bogus")
		if err == nil || !strings.Contains(err.Error(), "未知需求平台") {
			t.Errorf("got %v want 含「未知需求平台」", err)
		}
	})
}
