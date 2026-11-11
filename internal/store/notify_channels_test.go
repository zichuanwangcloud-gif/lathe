package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/notify"
	"github.com/zichuanwangcloud-gif/lathe/internal/secret"
)

// T0 远早于「真实现在」：其它测试残留的行时间都在真实现在附近，
// 全局查询（ClaimDueDelivery / ReclaimStale）以 T0 为 now 时天然领不到它们。
var ncT0 = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	slackURL1  = "https://hooks.slack.com/services/T000/B000/XXXXabcd"
	feishuURL1 = "https://open.feishu.cn/open-apis/bot/v2/hook/11111111-aaaa"
	feishuURL2 = "https://open.feishu.cn/open-apis/bot/v2/hook/22222222-bbbb"
)

type ncFix struct {
	st *Store
	nc *NotifyChannels
}

func newNCFixture(t *testing.T) *ncFix {
	t.Helper()
	st := testStore(t)
	key := make([]byte, secret.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	sealer, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}
	return &ncFix{st: st, nc: st.NewNotifyChannels(sealer, notify.AllowedHosts)}
}

func ncNonce(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + hex.EncodeToString(b)
}

// mkUser 建一个带随机 email 的用户与一个仓库（§5.5：fixture 带随机量）。
func (f *ncFix) mkUser(t *testing.T) (userID, repoID int64) {
	t.Helper()
	ctx := context.Background()
	n := ncNonce(t)
	if err := f.st.pool.QueryRow(ctx, `INSERT INTO users (email) VALUES ($1) RETURNING id`,
		"nc-"+n+"@example.com").Scan(&userID); err != nil {
		t.Fatalf("建 user 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.st.pool.Exec(context.Background(), `DELETE FROM users WHERE id=$1`, userID)
	})
	if err := f.st.pool.QueryRow(ctx, `INSERT INTO repos (user_id, provider_repo) VALUES ($1,$2) RETURNING id`,
		userID, "acme/nc-"+n).Scan(&repoID); err != nil {
		t.Fatalf("建 repo 失败: %v", err)
	}
	return userID, repoID
}

func (f *ncFix) mkTask(t *testing.T, userID, repoID int64) int64 {
	t.Helper()
	var id int64
	if err := f.st.pool.QueryRow(context.Background(),
		`INSERT INTO tasks (user_id, repo_id, external_key) VALUES ($1,$2,$3) RETURNING id`,
		userID, repoID, "NC-"+ncNonce(t)).Scan(&id); err != nil {
		t.Fatalf("建 task 失败: %v", err)
	}
	return id
}

func allOn() notify.EventSwitches {
	return notify.EventSwitches{Failed: true, BlockedSpec: true, AwaitingApproval: true, PROpen: true, PRDReady: true}
}

func (f *ncFix) save(t *testing.T, userID int64, p SaveChannelParams) int64 {
	t.Helper()
	if _, err := f.nc.SaveChannel(context.Background(), userID, p); err != nil {
		t.Fatalf("SaveChannel(%s) 失败: %v", p.Kind, err)
	}
	return f.channelID(t, userID, p.Kind)
}

func (f *ncFix) channelID(t *testing.T, userID int64, kind string) int64 {
	t.Helper()
	var id int64
	if err := f.st.pool.QueryRow(context.Background(),
		`SELECT id FROM notify_channels WHERE user_id=$1 AND kind=$2`, userID, kind).Scan(&id); err != nil {
		t.Fatalf("查渠道失败: %v", err)
	}
	return id
}

func (f *ncFix) insertDelivery(t *testing.T, chID int64, eventType, status string, next time.Time, claimed *time.Time, attempts int) int64 {
	t.Helper()
	var id int64
	if err := f.st.pool.QueryRow(context.Background(), `
		INSERT INTO notify_deliveries (channel_id, event_type, event_key, status, attempts, next_attempt_at, claimed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		chID, eventType, "fixture:"+ncNonce(t), status, attempts, next, claimed).Scan(&id); err != nil {
		t.Fatalf("插投递行失败: %v", err)
	}
	return id
}

type ncRow struct {
	Status    string
	Attempts  int
	Next      time.Time
	Claimed   *time.Time
	LastError *string
	SentAt    *time.Time
	EventKey  string
	Payload   []byte
	TaskID    *int64
	PRDID     *int64
	EventType string
}

func (f *ncFix) row(t *testing.T, id int64) ncRow {
	t.Helper()
	var r ncRow
	if err := f.st.pool.QueryRow(context.Background(), `
		SELECT status, attempts, next_attempt_at, claimed_at, last_error, sent_at, event_key, payload,
		       target_task_id, target_prd_id, event_type
		FROM notify_deliveries WHERE id=$1`, id).Scan(
		&r.Status, &r.Attempts, &r.Next, &r.Claimed, &r.LastError, &r.SentAt, &r.EventKey, &r.Payload,
		&r.TaskID, &r.PRDID, &r.EventType); err != nil {
		t.Fatalf("读投递行 %d 失败: %v", id, err)
	}
	return r
}

// claimMine 领一行属于 mine 的到期投递；领到别人的残留（上一轮被杀留下、
// 同样落在 T0 附近的行）就标 skipped 继续领（machine_test 的排空思路）。
func (f *ncFix) claimMine(t *testing.T, now time.Time, mine map[int64]bool) *notify.Delivery {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 1000; i++ {
		d, err := f.nc.ClaimDueDelivery(ctx, now)
		if err != nil {
			t.Fatalf("ClaimDueDelivery 失败: %v", err)
		}
		if d == nil {
			return nil
		}
		if mine[d.ID] {
			return d
		}
		_, _ = f.nc.MarkSkipped(ctx, d.ID, d.ClaimedAt, now, "stale fixture")
	}
	t.Fatal("排空残留行超过上限")
	return nil
}

// AC-11
func TestChannelStoreEncryptsAndMasks(t *testing.T) {
	f := newNCFixture(t)
	ctx := context.Background()
	uid, _ := f.mkUser(t)
	f.save(t, uid, SaveChannelParams{Kind: notify.KindSlack, URL: slackURL1, Enabled: true, Events: allOn()})
	f.save(t, uid, SaveChannelParams{Kind: notify.KindFeishu, URL: feishuURL1, Secret: "sign-sekrit", Enabled: true, Events: allOn()})

	t.Run("库里只有密文", func(t *testing.T) {
		rows, err := f.st.pool.Query(ctx, `SELECT url_enc, coalesce(sign_enc, ''::bytea) FROM notify_channels WHERE user_id=$1`, uid)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var u, s []byte
			if err := rows.Scan(&u, &s); err != nil {
				t.Fatal(err)
			}
			for _, plain := range []string{"XXXXabcd", "hooks.slack.com", "11111111-aaaa", "open.feishu.cn", "sign-sekrit"} {
				if bytes.Contains(u, []byte(plain)) || bytes.Contains(s, []byte(plain)) {
					t.Errorf("密文列里出现明文 %q", plain)
				}
			}
		}
	})
	t.Run("GetChannel 解回原文", func(t *testing.T) {
		ch, err := f.nc.GetChannel(ctx, uid, notify.KindFeishu)
		if err != nil || ch == nil {
			t.Fatalf("GetChannel got %v %v", ch, err)
		}
		if ch.URL != feishuURL1 || ch.SignSecret != "sign-sekrit" {
			t.Errorf("got url=%q secret=%q, want 原文", ch.URL, ch.SignSecret)
		}
	})
	t.Run("ListChannels 只回 scheme+host+省略号", func(t *testing.T) {
		views, err := f.nc.ListChannels(ctx, uid)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]struct {
			masked string
			secret bool
		}{
			notify.KindSlack:  {"https://hooks.slack.com/…", false},
			notify.KindFeishu: {"https://open.feishu.cn/…", true},
		}
		if len(views) != 2 {
			t.Fatalf("views got %d, want 2", len(views))
		}
		for _, v := range views {
			w := want[v.Kind]
			if !v.Configured || v.MaskedURL != w.masked || v.HasSecret != w.secret {
				t.Errorf("%s got configured=%v masked=%q hasSecret=%v, want true/%q/%v", v.Kind, v.Configured, v.MaskedURL, v.HasSecret, w.masked, w.secret)
			}
			if strings.Contains(v.MaskedURL, "abcd") {
				t.Errorf("掩码不得带末 4 位：got %q", v.MaskedURL)
			}
		}
		raw, _ := json.Marshal(views)
		if strings.Contains(string(raw), "sign-sekrit") || strings.Contains(string(raw), "XXXXabcd") {
			t.Errorf("ListChannels JSON 泄漏明文：%s", raw)
		}
	})
	t.Run("未配置用户两种 kind 都是 configured=false", func(t *testing.T) {
		other, _ := f.mkUser(t)
		views, err := f.nc.ListChannels(ctx, other)
		if err != nil {
			t.Fatal(err)
		}
		if len(views) != 2 || views[0].Configured || views[1].Configured {
			t.Errorf("got %+v, want 两条未配置", views)
		}
	})
}

// AC-11b
func TestSaveChannelPartialUpdateKeepsSecrets(t *testing.T) {
	f := newNCFixture(t)
	ctx := context.Background()

	type snap struct {
		url, sign []byte
		testOK    *bool
		testAt    *time.Time
		evFailed  bool
		enabled   bool
	}
	read := func(t *testing.T, uid int64) snap {
		t.Helper()
		var s snap
		if err := f.st.pool.QueryRow(ctx, `SELECT url_enc, sign_enc, test_ok, test_at, event_failed, enabled
			FROM notify_channels WHERE user_id=$1 AND kind='feishu'`, uid).Scan(&s.url, &s.sign, &s.testOK, &s.testAt, &s.evFailed, &s.enabled); err != nil {
			t.Fatalf("读渠道行: %v", err)
		}
		return s
	}
	setup := func(t *testing.T) (int64, snap) {
		t.Helper()
		uid, _ := f.mkUser(t)
		f.save(t, uid, SaveChannelParams{Kind: notify.KindFeishu, URL: feishuURL1, Secret: "s1", Enabled: true, Events: allOn()})
		// 模拟一次已完成的测试结论
		if _, err := f.st.pool.Exec(ctx, `UPDATE notify_channels SET test_ok=true, test_at=$2 WHERE user_id=$1`, uid, ncT0); err != nil {
			t.Fatal(err)
		}
		return uid, read(t, uid)
	}
	noFailed := allOn()
	noFailed.Failed = false

	cases := []struct {
		name        string
		in          SaveChannelParams
		wantErr     error
		wantChanged bool
		check       func(t *testing.T, uid int64, before, after snap)
	}{
		{"只改事件开关：不重封、test_* 不变", SaveChannelParams{Kind: notify.KindFeishu, Enabled: true, Events: noFailed}, nil, false,
			func(t *testing.T, uid int64, before, after snap) {
				if !bytes.Equal(before.url, after.url) || !bytes.Equal(before.sign, after.sign) {
					t.Error("url_enc/sign_enc 字节变了，want 相等（没有重封）")
				}
				if after.evFailed || !after.enabled {
					t.Errorf("got event_failed=%v enabled=%v, want false/true", after.evFailed, after.enabled)
				}
				if after.testOK == nil || !*after.testOK || after.testAt == nil {
					t.Errorf("test_* 应不变，got ok=%v at=%v", after.testOK, after.testAt)
				}
				ch, _ := f.nc.GetChannel(ctx, uid, notify.KindFeishu)
				if ch == nil || ch.URL != feishuURL1 || ch.SignSecret != "s1" {
					t.Errorf("GetChannel got %+v, want u1/s1", ch)
				}
				if ch != nil && (ch.Events.Failed || !ch.Events.BlockedSpec || !ch.Events.AwaitingApproval || !ch.Events.PROpen || !ch.Events.PRDReady) {
					t.Errorf("events got %+v, want 只有 failed 关", ch.Events)
				}
				var n int
				_ = f.st.pool.QueryRow(ctx, `SELECT count(*) FROM notify_deliveries d JOIN notify_channels c ON c.id=d.channel_id WHERE c.user_id=$1`, uid).Scan(&n)
				if n != 0 {
					t.Errorf("投递行 got %d, want 0（store 不发测试）", n)
				}
			}},
		{"只改总开关", SaveChannelParams{Kind: notify.KindFeishu, Enabled: false, Events: allOn()}, nil, false,
			func(t *testing.T, uid int64, before, after snap) {
				if !bytes.Equal(before.url, after.url) || !bytes.Equal(before.sign, after.sign) {
					t.Error("url_enc/sign_enc 字节变了")
				}
				if after.enabled {
					t.Error("enabled got true, want false")
				}
				if after.testOK == nil {
					t.Error("test_ok 不应被清")
				}
			}},
		{"显式清签名", SaveChannelParams{Kind: notify.KindFeishu, ClearSecret: true, Enabled: true, Events: allOn()}, nil, true,
			func(t *testing.T, uid int64, before, after snap) {
				if after.sign != nil {
					t.Error("sign_enc got 非 NULL, want NULL")
				}
				if !bytes.Equal(before.url, after.url) {
					t.Error("url_enc 字节变了")
				}
				if after.testOK != nil || after.testAt != nil {
					t.Errorf("test_* 应置 NULL，got %v %v", after.testOK, after.testAt)
				}
			}},
		{"换 URL", SaveChannelParams{Kind: notify.KindFeishu, URL: feishuURL2, Enabled: true, Events: allOn()}, nil, true,
			func(t *testing.T, uid int64, before, after snap) {
				ch, _ := f.nc.GetChannel(ctx, uid, notify.KindFeishu)
				if ch == nil || ch.URL != feishuURL2 {
					t.Errorf("url got %+v, want u2", ch)
				}
				if !bytes.Equal(before.sign, after.sign) {
					t.Error("sign_enc 字节变了")
				}
			}},
		{"换密钥", SaveChannelParams{Kind: notify.KindFeishu, Secret: "s2", Enabled: true, Events: allOn()}, nil, true,
			func(t *testing.T, uid int64, before, after snap) {
				ch, _ := f.nc.GetChannel(ctx, uid, notify.KindFeishu)
				if ch == nil || ch.SignSecret != "s2" {
					t.Errorf("secret got %+v, want s2", ch)
				}
				if !bytes.Equal(before.url, after.url) {
					t.Error("url_enc 字节变了")
				}
			}},
		{"同时给 secret 与 clearSecret", SaveChannelParams{Kind: notify.KindFeishu, Secret: "s2", ClearSecret: true, Enabled: true, Events: allOn()}, ErrSecretConflict, false,
			func(t *testing.T, uid int64, before, after snap) {
				if !bytes.Equal(before.url, after.url) || !bytes.Equal(before.sign, after.sign) {
					t.Error("出错时行字节不该变")
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			uid, before := setup(t)
			changed, err := f.nc.SaveChannel(ctx, uid, c.in)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("err got %v, want %v", err, c.wantErr)
			}
			if changed != c.wantChanged {
				t.Errorf("credsChanged got %v, want %v", changed, c.wantChanged)
			}
			c.check(t, uid, before, read(t, uid))
		})
	}
	t.Run("首次配置不给 URL", func(t *testing.T) {
		uid, _ := f.mkUser(t)
		_, err := f.nc.SaveChannel(ctx, uid, SaveChannelParams{Kind: notify.KindFeishu, Enabled: true, Events: allOn()})
		if !errors.Is(err, ErrChannelURLRequired) {
			t.Errorf("err got %v, want ErrChannelURLRequired", err)
		}
		var n int
		_ = f.st.pool.QueryRow(ctx, `SELECT count(*) FROM notify_channels WHERE user_id=$1`, uid).Scan(&n)
		if n != 0 {
			t.Errorf("行数 got %d, want 0", n)
		}
	})
	t.Run("slack 给 secret", func(t *testing.T) {
		uid, _ := f.mkUser(t)
		for _, in := range []SaveChannelParams{
			{Kind: notify.KindSlack, URL: slackURL1, Secret: "x", Enabled: true, Events: allOn()},
			{Kind: notify.KindSlack, URL: slackURL1, ClearSecret: true, Enabled: true, Events: allOn()},
		} {
			if _, err := f.nc.SaveChannel(ctx, uid, in); !errors.Is(err, ErrSlackNoSecret) {
				t.Errorf("err got %v, want ErrSlackNoSecret", err)
			}
		}
	})
}

// AC-12
func TestChannelStoreRejectsUnknownKind(t *testing.T) {
	f := newNCFixture(t)
	uid, _ := f.mkUser(t)
	cases := []struct {
		name    string
		in      SaveChannelParams
		wantSub string
		wantIs  error
	}{
		{"未知 kind", SaveChannelParams{Kind: "dingtalk", URL: slackURL1, Enabled: true, Events: allOn()}, "未知", nil},
		{"http URL", SaveChannelParams{Kind: notify.KindSlack, URL: "http://hooks.slack.com/services/x", Enabled: true, Events: allOn()}, "", notify.ErrURLNotHTTPS},
		{"白名单外 host", SaveChannelParams{Kind: notify.KindSlack, URL: "https://evil.example/x", Enabled: true, Events: allOn()}, "", notify.ErrURLHostNotAllowed},
		{"带端口", SaveChannelParams{Kind: notify.KindSlack, URL: "https://hooks.slack.com:8443/x", Enabled: true, Events: allOn()}, "", notify.ErrURLHostNotAllowed},
		{"空 URL 首次", SaveChannelParams{Kind: notify.KindSlack, URL: "", Enabled: true, Events: allOn()}, "", ErrChannelURLRequired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := f.nc.SaveChannel(context.Background(), uid, c.in)
			if err == nil {
				t.Fatal("got nil, want 错误")
			}
			if c.wantSub != "" && !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("err got %v, want 含 %q", err, c.wantSub)
			}
			if c.wantIs != nil && !errors.Is(err, c.wantIs) {
				t.Errorf("err got %v, want %v", err, c.wantIs)
			}
		})
	}
}

// AC-12b
func TestEventConstantsMatchStoreCheck(t *testing.T) {
	raw, err := os.ReadFile("../../migrations/0057_notify_deliveries.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)event_type IN \(([^)]*)\)`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("迁移里找不到 event_type IN (...)")
	}
	var got []string
	for _, s := range regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(m[1], -1) {
		got = append(got, string(s[1]))
	}
	want := append([]string(nil), notify.EventTypes()...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CHECK 取值 got %v, want %v", got, want)
	}
}

// AC-13
func TestEnqueueTaskEventBuildsRowAndIsIdempotent(t *testing.T) {
	f := newNCFixture(t)
	ctx := context.Background()
	uid, repo := f.mkUser(t)
	f.save(t, uid, SaveChannelParams{Kind: notify.KindSlack, URL: slackURL1, Enabled: true, Events: allOn()})
	f.save(t, uid, SaveChannelParams{Kind: notify.KindFeishu, URL: feishuURL1, Enabled: true, Events: allOn()})

	cases := []struct {
		name     string
		at       time.Time
		wantTail string
		wantNext time.Time
	}{
		{"微秒输入（生产形态）", ncT0.Add(123456 * time.Microsecond), "2000-01-01T00:00:00.123456Z", ncT0.Add(123456 * time.Microsecond)},
		{"纳秒输入（键与列不得分叉）", ncT0.Add(123456789), "2000-01-01T00:00:00.123456Z", ncT0.Add(123456 * time.Microsecond)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			taskID := f.mkTask(t, uid, repo)
			ev := notify.TaskEvent{TaskID: taskID, UserID: uid, ExternalKey: "CR-13", ToState: "failed",
				EventType: notify.EventFailed, At: c.at, FailureStage: "implement", Reason: "redacted"}
			n, err := f.nc.EnqueueTaskEvent(ctx, ev)
			if err != nil || n != 2 {
				t.Fatalf("EnqueueTaskEvent got (%d, %v), want (2, nil)", n, err)
			}
			wantKey := "task:" + strconv.FormatInt(taskID, 10) + ":failed:" + c.wantTail
			if k := notify.TaskEventKey(ev); k != wantKey {
				t.Errorf("TaskEventKey got %q, want %q", k, wantKey)
			}
			rows, err := f.st.pool.Query(ctx, `SELECT id FROM notify_deliveries WHERE target_task_id=$1`, taskID)
			if err != nil {
				t.Fatal(err)
			}
			var ids []int64
			for rows.Next() {
				var id int64
				_ = rows.Scan(&id)
				ids = append(ids, id)
			}
			rows.Close()
			if len(ids) != 2 {
				t.Fatalf("行数 got %d, want 2", len(ids))
			}
			for _, id := range ids {
				r := f.row(t, id)
				if r.EventKey != wantKey {
					t.Errorf("event_key got %q, want %q", r.EventKey, wantKey)
				}
				if !r.Next.Equal(c.wantNext) {
					t.Errorf("next_attempt_at got %v, want %v", r.Next, c.wantNext)
				}
				if got := r.Next.UTC().Format(time.RFC3339Nano); !strings.HasSuffix(r.EventKey, got) {
					t.Errorf("列 %q 与键 %q 时间段不一致", got, r.EventKey)
				}
				var p notify.Payload
				if err := json.Unmarshal(r.Payload, &p); err != nil {
					t.Fatal(err)
				}
				if want := notify.TaskPayload(ev); p != want {
					t.Errorf("payload got %+v, want %+v", p, want)
				}
				if r.TaskID == nil || *r.TaskID != taskID || r.Status != "pending" || r.Attempts != 0 || r.EventType != "failed" {
					t.Errorf("row got %+v", r)
				}
			}
			n, err = f.nc.EnqueueTaskEvent(ctx, ev)
			if err != nil || n != 0 {
				t.Errorf("重复入队 got (%d, %v), want (0, nil)", n, err)
			}
			var total int
			_ = f.st.pool.QueryRow(ctx, `SELECT count(*) FROM notify_deliveries WHERE target_task_id=$1`, taskID).Scan(&total)
			if total != 2 {
				t.Errorf("重复入队后行数 got %d, want 2", total)
			}
		})
	}
}

// AC-14
func TestEnqueueSkipsDisabledChannelAndEventType(t *testing.T) {
	f := newNCFixture(t)
	ctx := context.Background()
	noFailed := allOn()
	noFailed.Failed = false

	cases := []struct {
		name  string
		setup func(t *testing.T, uid int64)
	}{
		{"渠道 enabled=false", func(t *testing.T, uid int64) {
			f.save(t, uid, SaveChannelParams{Kind: notify.KindSlack, URL: slackURL1, Enabled: false, Events: allOn()})
		}},
		{"event_failed=false", func(t *testing.T, uid int64) {
			f.save(t, uid, SaveChannelParams{Kind: notify.KindSlack, URL: slackURL1, Enabled: true, Events: noFailed})
		}},
		{"用户未配任何渠道", func(t *testing.T, uid int64) {}},
		{"只有别人配了渠道", func(t *testing.T, uid int64) {
			other, _ := f.mkUser(t)
			f.save(t, other, SaveChannelParams{Kind: notify.KindSlack, URL: slackURL1, Enabled: true, Events: allOn()})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			uid, repo := f.mkUser(t)
			c.setup(t, uid)
			taskID := f.mkTask(t, uid, repo)
			n, err := f.nc.EnqueueTaskEvent(ctx, notify.TaskEvent{TaskID: taskID, UserID: uid, ExternalKey: "CR-14",
				ToState: "failed", EventType: notify.EventFailed, At: ncT0})
			if err != nil || n != 0 {
				t.Errorf("got (%d, %v), want (0, nil)", n, err)
			}
			var total int
			_ = f.st.pool.QueryRow(ctx, `SELECT count(*) FROM notify_deliveries WHERE target_task_id=$1`, taskID).Scan(&total)
			if total != 0 {
				t.Errorf("行数 got %d, want 0", total)
			}
		})
	}
}

// AC-15
func TestClaimDueDeliveryIsExclusive(t *testing.T) {
	f := newNCFixture(t)
	ctx := context.Background()
	uid, _ := f.mkUser(t)
	ch := f.save(t, uid, SaveChannelParams{Kind: notify.KindSlack, URL: slackURL1, Enabled: true, Events: allOn()})

	mine := map[int64]bool{}
	for i := 0; i < 20; i++ {
		mine[f.insertDelivery(t, ch, notify.EventFailed, "pending", ncT0, nil, 0)] = true
	}
	future := f.insertDelivery(t, ch, notify.EventFailed, "pending", ncT0.Add(time.Second), nil, 0)
	testSending := f.insertDelivery(t, ch, notify.EventTest, "sending", ncT0, &ncT0, 0)
	testPending := f.insertDelivery(t, ch, notify.EventTest, "pending", ncT0, nil, 0)

	var mu sync.Mutex
	got := [2][]int64{}
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for {
				d, err := f.nc.ClaimDueDelivery(ctx, ncT0)
				if err != nil {
					t.Errorf("ClaimDueDelivery: %v", err)
					return
				}
				if d == nil {
					return
				}
				if d.ID == future || d.ID == testSending || d.ID == testPending {
					t.Errorf("领到了不该领的行 %d", d.ID)
				}
				if !mine[d.ID] {
					_, _ = f.nc.MarkSkipped(ctx, d.ID, d.ClaimedAt, ncT0, "stale fixture")
					continue
				}
				if !d.ClaimedAt.Equal(ncT0) {
					t.Errorf("claimed_at got %v, want %v", d.ClaimedAt, ncT0)
				}
				mu.Lock()
				got[g] = append(got[g], d.ID)
				mu.Unlock()
			}
		}(g)
	}
	wg.Wait()

	seen := map[int64]int{}
	for _, ids := range got {
		for _, id := range ids {
			seen[id]++
		}
	}
	for id := range mine {
		if seen[id] != 1 {
			t.Errorf("行 %d 被领 %d 次, want 1", id, seen[id])
		}
	}
	if len(seen) != len(mine) {
		t.Errorf("并集 got %d, want %d", len(seen), len(mine))
	}
	if r := f.row(t, future); r.Status != "pending" {
		t.Errorf("未到期行 got %s, want pending", r.Status)
	}
	if r := f.row(t, testPending); r.Status != "pending" {
		t.Errorf("test 行 got %s, want pending（worker 永不领 test）", r.Status)
	}
}

// AC-16
func TestRetryBacksOffAndGivesUpAtFive(t *testing.T) {
	f := newNCFixture(t)
	ctx := context.Background()
	uid, _ := f.mkUser(t)
	ch := f.save(t, uid, SaveChannelParams{Kind: notify.KindSlack, URL: slackURL1, Enabled: true, Events: allOn()})
	id := f.insertDelivery(t, ch, notify.EventFailed, "pending", ncT0, nil, 0)
	mine := map[int64]bool{id: true}

	now := ncT0
	var cum time.Duration
	for k := 1; k <= 5; k++ {
		t.Run("第 "+strconv.Itoa(k)+" 次失败", func(t *testing.T) {
			d := f.claimMine(t, now, mine)
			if d == nil {
				t.Fatalf("now=%v 领不到本行", now)
			}
			ok, err := f.nc.MarkRetry(ctx, d.ID, d.ClaimedAt, now, "500")
			if err != nil || !ok {
				t.Fatalf("MarkRetry got (%v, %v)", ok, err)
			}
			r := f.row(t, id)
			if r.Attempts != k {
				t.Errorf("attempts got %d, want %d", r.Attempts, k)
			}
			if k < 5 {
				cum += notify.BaseBackoff << uint(k-1)
				if r.Status != "pending" || !r.Next.Equal(ncT0.Add(cum)) {
					t.Errorf("got status=%s next=%v, want pending/%v", r.Status, r.Next, ncT0.Add(cum))
				}
				now = r.Next
			} else if r.Status != "gave_up" || !r.Next.Equal(ncT0.Add(cum)) {
				t.Errorf("got status=%s next=%v, want gave_up/%v（未改写）", r.Status, r.Next, ncT0.Add(cum))
			}
		})
	}
	t.Run("gave_up 不再被领", func(t *testing.T) {
		if d := f.claimMine(t, ncT0.Add(time.Hour), mine); d != nil {
			t.Errorf("got %d, want nil", d.ID)
		}
	})
	t.Run("错误的 claimedAt CAS 不命中", func(t *testing.T) {
		id2 := f.insertDelivery(t, ch, notify.EventFailed, "sending", ncT0, &ncT0, 0)
		ok, err := f.nc.MarkRetry(ctx, id2, ncT0.Add(-time.Second), ncT0, "500")
		if err != nil || ok {
			t.Errorf("got (%v, %v), want (false, nil)", ok, err)
		}
		if r := f.row(t, id2); r.Status != "sending" || r.Attempts != 0 {
			t.Errorf("行不该变，got %+v", r)
		}
		ok, err = f.nc.MarkGaveUp(ctx, id2, ncT0, ncT0, "400")
		if err != nil || !ok {
			t.Fatalf("MarkGaveUp got (%v, %v)", ok, err)
		}
		if r := f.row(t, id2); r.Status != "gave_up" || r.Attempts != 1 || r.LastError == nil || *r.LastError != "400" {
			t.Errorf("got %+v, want gave_up/1/400", r)
		}
	})
}

// AC-17
func TestReclaimStaleSending(t *testing.T) {
	f := newNCFixture(t)
	ctx := context.Background()
	uid, _ := f.mkUser(t)
	ch := f.save(t, uid, SaveChannelParams{Kind: notify.KindSlack, URL: slackURL1, Enabled: true, Events: allOn()})
	old := ncT0.Add(-notify.ClaimLease - time.Minute)
	fresh := ncT0.Add(-notify.ClaimLease + time.Minute)

	cases := []struct {
		name         string
		event        string
		claimed      time.Time
		attempts     int
		wantStatus   string
		wantAttempts int
		wantNext     *time.Time
	}{
		{"过期 attempts=0 → pending", notify.EventFailed, old, 0, "pending", 1, ptrTime(ncT0.Add(notify.BaseBackoff))},
		{"过期 attempts=4 → gave_up", notify.EventFailed, old, 4, "gave_up", 5, nil},
		{"未过期 → 不动", notify.EventFailed, fresh, 0, "sending", 0, nil},
		{"test 行过期 → 直接 gave_up", notify.EventTest, old, 0, "gave_up", 1, nil},
	}
	ids := make([]int64, len(cases))
	for i, c := range cases {
		cl := c.claimed
		ids[i] = f.insertDelivery(t, ch, c.event, "sending", ncT0.Add(-time.Hour), &cl, c.attempts)
	}
	n, err := f.nc.ReclaimStale(ctx, ncT0, notify.ClaimLease)
	if err != nil {
		t.Fatal(err)
	}
	if n < 3 {
		t.Errorf("回收计数 got %d, want >= 3", n)
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := f.row(t, ids[i])
			if r.Status != c.wantStatus || r.Attempts != c.wantAttempts {
				t.Errorf("got status=%s attempts=%d, want %s/%d", r.Status, r.Attempts, c.wantStatus, c.wantAttempts)
			}
			if c.wantNext != nil && !r.Next.Equal(*c.wantNext) {
				t.Errorf("next got %v, want %v", r.Next, *c.wantNext)
			}
			if c.wantStatus != "sending" && (r.LastError == nil || *r.LastError != "lease expired") {
				t.Errorf("last_error got %v, want lease expired", r.LastError)
			}
		})
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// AC-22b
func TestEnqueuePRDEventBuildsRow(t *testing.T) {
	f := newNCFixture(t)
	ctx := context.Background()
	uid, repo := prdFixture(t, f.st)
	p := newPRD(t, f.st, uid, repo)
	f.save(t, uid, SaveChannelParams{Kind: notify.KindSlack, URL: slackURL1, Enabled: true, Events: allOn()})

	ev := notify.PRDEvent{PRDID: p.ID, UserID: uid, ToState: "ready_for_review", EventType: notify.EventPRDReady, At: ncT0, Repo: "acme/x"}
	n, err := f.nc.EnqueuePRDEvent(ctx, ev)
	if err != nil || n != 1 {
		t.Fatalf("got (%d, %v), want (1, nil)", n, err)
	}
	var id int64
	if err := f.st.pool.QueryRow(ctx, `SELECT id FROM notify_deliveries WHERE target_prd_id=$1`, p.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	r := f.row(t, id)
	if want := "prd:" + strconv.FormatInt(p.ID, 10) + ":ready_for_review:2000-01-01T00:00:00Z"; r.EventKey != want {
		t.Errorf("event_key got %q, want %q", r.EventKey, want)
	}
	if r.EventType != notify.EventPRDReady || r.TaskID != nil || r.PRDID == nil || *r.PRDID != p.ID {
		t.Errorf("row got %+v", r)
	}
	var pl notify.Payload
	_ = json.Unmarshal(r.Payload, &pl)
	if want := notify.PRDPayload(ev); pl != want {
		t.Errorf("payload got %+v, want %+v", pl, want)
	}

	t.Run("event_prd_ready=false 时 0 行", func(t *testing.T) {
		sw := allOn()
		sw.PRDReady = false
		f.save(t, uid, SaveChannelParams{Kind: notify.KindSlack, Enabled: true, Events: sw})
		ev2 := ev
		ev2.At = ncT0.Add(time.Second)
		if n, err := f.nc.EnqueuePRDEvent(ctx, ev2); err != nil || n != 0 {
			t.Errorf("got (%d, %v), want (0, nil)", n, err)
		}
	})
}
