package store

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()

	dsn := os.Getenv("LATHE_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://lathe:lathe@127.0.0.1:55432/lathe?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := Open(ctx, dsn)
	if err != nil {
		t.Skipf("跳过数据库测试（先 make dev-infra && make migrate）: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

func TestClaimDeliveryIsIdempotent(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	id := "deliv-" + t.Name()
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `DELETE FROM webhook_deliveries WHERE delivery_id = $1`, id)
	})

	first, err := st.ClaimDelivery(ctx, id, "linear")
	if err != nil {
		t.Fatalf("首次 ClaimDelivery 失败: %v", err)
	}
	if !first {
		t.Error("首次投递应返回 true（首见）")
	}

	second, err := st.ClaimDelivery(ctx, id, "linear")
	if err != nil {
		t.Fatalf("重投递不应报错: %v", err)
	}
	if second {
		t.Error("重投递应返回 false，否则会重复建任务")
	}

	if _, err := st.ClaimDelivery(ctx, "", "linear"); err == nil {
		t.Error("空 delivery ID 应报错")
	}
}

// 并发重投递：Linear 可能同时投两次，必须恰好一个拿到处理权。
func TestClaimDeliveryConcurrent(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	id := "deliv-concurrent-" + t.Name()
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `DELETE FROM webhook_deliveries WHERE delivery_id = $1`, id)
	})

	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claimed int
	)
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			ok, err := st.ClaimDelivery(ctx, id, "linear")
			if err != nil {
				t.Errorf("ClaimDelivery 报错: %v", err)
				return
			}
			if ok {
				mu.Lock()
				claimed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if claimed != 1 {
		t.Errorf("并发 %d 次投递，拿到处理权的应恰好 1 次，实际 %d", racers, claimed)
	}
}

func TestFinishDelivery(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	okID := "deliv-ok-" + t.Name()
	badID := "deliv-bad-" + t.Name()
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(),
			`DELETE FROM webhook_deliveries WHERE delivery_id = ANY($1)`, []string{okID, badID})
	})

	if _, err := st.ClaimDelivery(ctx, okID, "linear"); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishDelivery(ctx, okID, ""); err != nil {
		t.Fatalf("FinishDelivery 失败: %v", err)
	}
	got, err := st.Delivery(ctx, okID)
	if err != nil {
		t.Fatalf("Delivery 失败: %v", err)
	}
	if !got.Processed {
		t.Error("应标记为已处理")
	}
	if got.Error != "" {
		t.Errorf("成功的投递不应有错误信息，得到 %q", got.Error)
	}

	// 失败也要留痕：便于排障，且避免重投递时再跑一遍已知会失败的处理
	if _, err := st.ClaimDelivery(ctx, badID, "linear"); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishDelivery(ctx, badID, "仓库未配置"); err != nil {
		t.Fatalf("FinishDelivery 失败: %v", err)
	}
	got, err = st.Delivery(ctx, badID)
	if err != nil {
		t.Fatalf("Delivery 失败: %v", err)
	}
	if !got.Processed || got.Error != "仓库未配置" {
		t.Errorf("失败痕迹应被保留: %+v", got)
	}

	// 失败后重投递仍应被去重挡住
	again, err := st.ClaimDelivery(ctx, badID, "linear")
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Error("处理失败过的投递重投时仍应被去重挡住")
	}
}
