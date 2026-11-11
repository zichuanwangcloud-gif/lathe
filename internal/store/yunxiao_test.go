package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/secret"
	"github.com/zichuanwangcloud-gif/lathe/internal/testsupport"
)

func yxSealer(t *testing.T) secret.Sealer {
	t.Helper()
	key := make([]byte, secret.KeySize)
	for i := range key {
		key[i] = byte(i*7 + 3)
	}
	s, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// yxUser 建一个带随机量邮箱的用户，测试结束级联清掉（连同云效连接与快照）。
func yxUser(t *testing.T, st *Store, prefix string) int64 {
	t.Helper()
	var id int64
	email := fmt.Sprintf("%s-%d@example.com", prefix, time.Now().UnixNano())
	if err := st.pool.QueryRow(context.Background(),
		`INSERT INTO users (email) VALUES ($1) RETURNING id`, email).Scan(&id); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = st.pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// AC-9（store 部分）
func TestYunxiaoConnectionEncrypted(t *testing.T) {
	st := testStore(t)
	yc := st.NewYunxiaoConnections(yxSealer(t))
	ctx := context.Background()
	nonce := strconv.FormatInt(time.Now().UnixNano(), 36)
	token := "pt-token-" + nonce
	whsec := "whsec-0123456789-" + nonce

	save := func(t *testing.T, uid int64, tok, sec string) {
		t.Helper()
		if err := yc.Save(ctx, uid, YunxiaoSaveParams{
			Endpoint: "https://openapi-rdc.aliyuncs.com", OrganizationID: "org1",
			Token: tok, WebhookSecret: sec, TriggerLabel: "lathe:go",
		}); err != nil {
			t.Fatalf("Save: got %v want nil", err)
		}
	}
	rawCols := func(t *testing.T, uid int64) (tokEnc, secEnc []byte) {
		t.Helper()
		if err := st.pool.QueryRow(ctx,
			`SELECT token_enc, webhook_secret_enc FROM yunxiao_connections WHERE user_id = $1`, uid,
		).Scan(&tokEnc, &secEnc); err != nil {
			t.Fatalf("读原始列: %v", err)
		}
		return
	}

	t.Run("token_bytes_no_plaintext", func(t *testing.T) {
		uid := yxUser(t, st, "yx-enc-tok")
		save(t, uid, token, whsec)
		tokEnc, _ := rawCols(t, uid)
		if len(tokEnc) == 0 || bytes.Contains(tokEnc, []byte(token)) || bytes.Contains(tokEnc, []byte(nonce)) {
			t.Errorf("token_enc 含明文或为空: len=%d", len(tokEnc))
		}
		got, err := yc.Get(ctx, uid)
		if err != nil || got.Token != token {
			t.Errorf("Get().Token got (%v, %v) want %q", got, err, token)
		}
	})
	t.Run("secret_bytes_no_plaintext", func(t *testing.T) {
		uid := yxUser(t, st, "yx-enc-sec")
		save(t, uid, token, whsec)
		_, secEnc := rawCols(t, uid)
		if len(secEnc) == 0 || bytes.Contains(secEnc, []byte(whsec)) {
			t.Errorf("webhook_secret_enc 含明文或为空: len=%d", len(secEnc))
		}
		got, err := yc.Get(ctx, uid)
		if err != nil || got.WebhookSecret != whsec {
			t.Errorf("Get().WebhookSecret got (%v, %v) want %q", got, err, whsec)
		}
	})
	t.Run("status_masked_only", func(t *testing.T) {
		uid := yxUser(t, st, "yx-enc-status")
		save(t, uid, token, whsec)
		s, err := yc.Status(ctx, uid)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		b, _ := json.Marshal(s)
		if bytes.Contains(b, []byte(token)) || bytes.Contains(b, []byte(whsec)) || bytes.Contains(b, []byte(nonce)) {
			t.Errorf("Status 含明文: %s", b)
		}
		if !s.Configured || s.TokenMasked != secret.Mask(token) || s.WebhookSecretMasked != secret.Mask(whsec) || !s.HasWebhookSecret {
			t.Errorf("Status got %+v want 掩码 %q/%q", s, secret.Mask(token), secret.Mask(whsec))
		}
		none, err := yc.Status(ctx, yxUser(t, st, "yx-enc-none"))
		if err != nil || none.Configured {
			t.Errorf("未配置 Status got (%+v, %v) want configured=false", none, err)
		}
	})
	t.Run("empty_token_keeps_old", func(t *testing.T) {
		uid := yxUser(t, st, "yx-enc-keep")
		if err := yc.Save(ctx, uid, YunxiaoSaveParams{Endpoint: "https://openapi-rdc.aliyuncs.com"}); err == nil {
			t.Error("首次保存不带令牌: got nil want ErrYunxiaoTokenRequired")
		}
		save(t, uid, token, whsec)
		if err := yc.MarkVerified(ctx, uid, "acct-1", "张三"); err != nil {
			t.Fatal(err)
		}
		if err := yc.Save(ctx, uid, YunxiaoSaveParams{Endpoint: "https://openapi-rdc.aliyuncs.com", TriggerLabel: "新标签"}); err != nil {
			t.Fatalf("Save 空 token: %v", err)
		}
		got, err := yc.Get(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		if got.Token != token || got.WebhookSecret != whsec {
			t.Errorf("空串应保留原值: got token=%q secret=%q", got.Token, got.WebhookSecret)
		}
		if got.TriggerLabel != "新标签" || got.OrganizationID != "" {
			t.Errorf("非密字段应按新值覆盖: got label=%q org=%q", got.TriggerLabel, got.OrganizationID)
		}
		if got.AccountID != "acct-1" {
			t.Errorf("令牌未换时 account_id 应保留: got %q want acct-1", got.AccountID)
		}
	})
	t.Run("new_token_clears_account", func(t *testing.T) {
		uid := yxUser(t, st, "yx-enc-clear")
		save(t, uid, token, whsec)
		if err := yc.MarkVerified(ctx, uid, "acct-1", "张三"); err != nil {
			t.Fatal(err)
		}
		save(t, uid, token+"-new", "")
		got, err := yc.Get(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		if got.AccountID != "" || got.AccountName != "" || got.VerifiedAt != nil {
			t.Errorf("换新 token 应清空账号与验证时间: got id=%q name=%q at=%v", got.AccountID, got.AccountName, got.VerifiedAt)
		}
		if got.Token != token+"-new" || got.WebhookSecret != whsec {
			t.Errorf("got token=%q secret=%q", got.Token, got.WebhookSecret)
		}
	})
}

// AC-8
func TestObserveTrigger(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	yc := st.NewYunxiaoConnections(yxSealer(t))
	uid := yxUser(t, st, "yx-observe")
	seq := 0
	newID := func() string {
		seq++
		return fmt.Sprintf("wi-%d-%d", time.Now().UnixNano(), seq)
	}
	step := func(t *testing.T, wid string, a, l, want bool) {
		t.Helper()
		got, err := yc.ObserveTrigger(ctx, uid, wid, a, l)
		if err != nil {
			t.Fatalf("ObserveTrigger: %v", err)
		}
		if got != want {
			t.Errorf("ObserveTrigger(%v,%v) rose got %v want %v", a, l, got, want)
		}
	}

	t.Run("false_to_true_rises", func(t *testing.T) {
		wid := newID()
		step(t, wid, false, false, false)
		step(t, wid, true, false, true)
	})
	t.Run("true_to_true_no_rise", func(t *testing.T) {
		wid := newID()
		step(t, wid, true, false, true) // 首见即已指派 = 上升沿
		step(t, wid, true, false, false)
		step(t, wid, true, false, false)
	})
	t.Run("true_false_true_rises_again", func(t *testing.T) {
		wid := newID()
		step(t, wid, true, false, true)
		step(t, wid, false, false, false)
		step(t, wid, true, false, true)
	})
	t.Run("label_edge_independent", func(t *testing.T) {
		wid := newID()
		step(t, wid, true, false, true)
		step(t, wid, true, true, true) // 指派不变、标签上升
		step(t, wid, false, true, false)
		step(t, wid, true, true, true)
	})

	race := func(t *testing.T, pool *YunxiaoConnections, wid string) int {
		t.Helper()
		const n = 20
		start := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		rose, errs := 0, 0
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				r, err := pool.ObserveTrigger(ctx, uid, wid, true, false)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs++
					return
				}
				if r {
					rose++
				}
			}()
		}
		close(start)
		wg.Wait()
		if errs > 0 {
			t.Errorf("并发观察出错 %d 次", errs)
		}
		return rose
	}
	// 20 个 goroutine 必须真并发：给足连接，别让它们在池上排队。
	wide := &Store{pool: testsupport.PoolWithMaxConns(t, 40)}
	wyc := wide.NewYunxiaoConnections(yxSealer(t))

	t.Run("concurrent_first_seen_exactly_one_rise", func(t *testing.T) {
		for round := 0; round < 20; round++ {
			if got := race(t, wyc, newID()); got != 1 {
				t.Errorf("round=%d got rose=%d want 1", round, got)
			}
		}
	})
	t.Run("concurrent_existing_row_exactly_one_rise", func(t *testing.T) {
		for round := 0; round < 20; round++ {
			wid := newID()
			step(t, wid, false, false, false)
			if got := race(t, wyc, wid); got != 1 {
				t.Errorf("round=%d got rose=%d want 1", round, got)
			}
		}
	})
}
