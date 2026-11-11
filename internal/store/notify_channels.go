package store

// notify_channels.go 站外通知渠道（Slack / 飞书）与它的 outbox（docs/features/notify-channels.md）。
//
// 与 notify.go（终态邮件的收件人口径）并列。这里是「TaskEvent/PRD 事件 → 入队行」
// 的整层：按 user_id 查渠道、按 enabled 与事件开关过滤、notify.TaskEventKey 拼键、
// notify.TaskPayload 生成 payload、INSERT … ON CONFLICT DO NOTHING。
// runner.ChatNotifier / prd.ReviewNotifier 的方法签名与这里一对一同形，
// cmd/lathe 直接把 *NotifyChannels 赋过去，不写适配器。
//
// 时间一律由调用方传入，SQL 里不出现 now()（列默认值只管建行时刻，不参与判定）。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/zichuanwangcloud-gif/lathe/internal/notify"
	"github.com/zichuanwangcloud-gif/lathe/internal/secret"
)

// 渠道保存的输入错误（httpapi 映射成 400）。
var (
	ErrUnknownChannelKind = errors.New("store: 未知通知渠道")
	ErrChannelURLRequired = errors.New("store: 首次配置需要填写 webhook URL")
	ErrSecretConflict     = errors.New("store: secret 与 clearSecret 不能同时给")
	ErrSlackNoSecret      = errors.New("store: Slack 渠道没有签名密钥")
)

// ErrChannelNotFound 表示该用户没有配置这种渠道。
var ErrChannelNotFound = errors.New("store: 通知渠道未配置")

// ErrDeliveryNotFound：投递不存在或不属于当前用户（httpapi 一律 404，不暴露存在性）。
var ErrDeliveryNotFound = errors.New("store: 投递记录不存在")

// ErrNotRetryable：投递存在但不能人工重试（httpapi 409）。
var ErrNotRetryable = errors.New("store: 只有放弃（gave_up）的投递能重试")

// ErrTestNotRetryable 是 ErrNotRetryable 的特例：测试消息走「重发测试」。
var ErrTestNotRetryable = fmt.Errorf("%w：测试消息请在渠道卡片上点「重发测试」", ErrNotRetryable)

// NotifyChannels 读写渠道与 outbox。实现 notify.Outbox。
type NotifyChannels struct {
	sealer secret.Sealer
	store  *Store
	allow  []string
}

var _ notify.Outbox = (*NotifyChannels)(nil)

// NewNotifyChannels 构造渠道读写器。allowHosts 是 SaveChannel 校验 URL 用的白名单
// （生产传 notify.AllowedHosts；测试传 httptest 的 addr）。空切片 panic：装配错误要早暴露。
func (s *Store) NewNotifyChannels(sealer secret.Sealer, allowHosts []string) *NotifyChannels {
	if len(allowHosts) == 0 {
		panic("store: NewNotifyChannels 需要非空的 host 白名单")
	}
	return &NotifyChannels{sealer: sealer, store: s, allow: append([]string(nil), allowHosts...)}
}

// SaveChannelParams 是 PUT /api/notify/channels/{kind} 的部分更新入参。
type SaveChannelParams struct {
	Kind        string
	URL         string // "" = 保留（已有行）/ 报错（首次）
	Secret      string // "" = 保留
	ClearSecret bool
	Enabled     bool                 // handler 已校验非缺省
	Events      notify.EventSwitches // 同上
}

// SaveChannel 保存渠道。部分更新语义：
//
//   - URL / Secret 为空即保留原密文，**不重新加密**（随机 nonce，重封会换密文）；
//   - ClearSecret 才清签名 —— 绝不因为留空而清掉签名，否则改个勾选框就悄悄关掉了签名校验；
//   - 开关是整体替换。
//
// credsChanged = 首次配置 || URL/Secret 非空 || ClearSecret：凭据变了才由 httpapi 发测试消息，
// 且同一条 UPDATE 把 test_* 置 NULL（旧结论作废）。
func (n *NotifyChannels) SaveChannel(ctx context.Context, userID int64, in SaveChannelParams) (credsChanged bool, err error) {
	if !notify.ValidKind(in.Kind) {
		return false, fmt.Errorf("%w %q", ErrUnknownChannelKind, in.Kind)
	}
	in.URL = strings.TrimSpace(in.URL)
	if in.Secret != "" && in.ClearSecret {
		return false, ErrSecretConflict
	}
	if in.Kind == notify.KindSlack && (in.Secret != "" || in.ClearSecret) {
		return false, ErrSlackNoSecret
	}

	var urlEnc, signEnc []byte
	if in.URL != "" {
		if err := notify.ValidateURL(in.URL, n.allow); err != nil {
			return false, err
		}
		if urlEnc, err = n.sealer.Seal(in.URL); err != nil {
			return false, err
		}
	}
	if in.Secret != "" {
		if signEnc, err = n.sealer.Seal(in.Secret); err != nil {
			return false, err
		}
	}

	tx, err := n.store.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: 保存通知渠道失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id int64
	err = tx.QueryRow(ctx, `SELECT id FROM notify_channels WHERE user_id=$1 AND kind=$2 FOR UPDATE`,
		userID, in.Kind).Scan(&id)
	ev := in.Events
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if urlEnc == nil {
			return false, ErrChannelURLRequired
		}
		credsChanged = true
		if _, err := tx.Exec(ctx, `
			INSERT INTO notify_channels (user_id, kind, url_enc, sign_enc, enabled,
				event_failed, event_blocked_spec, event_awaiting_approval, event_pr_open, event_prd_ready)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			userID, in.Kind, urlEnc, signEnc, in.Enabled,
			ev.Failed, ev.BlockedSpec, ev.AwaitingApproval, ev.PROpen, ev.PRDReady); err != nil {
			return false, fmt.Errorf("store: 保存通知渠道失败: %w", err)
		}
	case err != nil:
		return false, fmt.Errorf("store: 保存通知渠道失败: %w", err)
	default:
		credsChanged = urlEnc != nil || signEnc != nil || in.ClearSecret
		// nil 参数让 COALESCE 保留原密文（同 SaveSMTP 的先例）
		var urlArg, signArg any
		if urlEnc != nil {
			urlArg = urlEnc
		}
		if signEnc != nil {
			signArg = signEnc
		}
		if _, err := tx.Exec(ctx, `
			UPDATE notify_channels SET
				url_enc  = COALESCE($2::bytea, url_enc),
				sign_enc = CASE WHEN $3 THEN NULL ELSE COALESCE($4::bytea, sign_enc) END,
				enabled = $5,
				event_failed = $6, event_blocked_spec = $7, event_awaiting_approval = $8,
				event_pr_open = $9, event_prd_ready = $10,
				test_ok    = CASE WHEN $11 THEN NULL ELSE test_ok END,
				test_error = CASE WHEN $11 THEN NULL ELSE test_error END,
				test_at    = CASE WHEN $11 THEN NULL ELSE test_at END
			WHERE id = $1`,
			id, urlArg, in.ClearSecret, signArg, in.Enabled,
			ev.Failed, ev.BlockedSpec, ev.AwaitingApproval, ev.PROpen, ev.PRDReady, credsChanged); err != nil {
			return false, fmt.Errorf("store: 保存通知渠道失败: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: 保存通知渠道失败: %w", err)
	}
	return credsChanged, nil
}

const channelCols = `id, user_id, kind, url_enc, sign_enc, enabled,
	event_failed, event_blocked_spec, event_awaiting_approval, event_pr_open, event_prd_ready`

// scanChannel 读一行并解密。
func (n *NotifyChannels) scanChannel(row pgx.Row) (*notify.Channel, error) {
	var (
		ch          notify.Channel
		urlE, signE []byte
	)
	e := &ch.Events
	if err := row.Scan(&ch.ID, &ch.UserID, &ch.Kind, &urlE, &signE, &ch.Enabled,
		&e.Failed, &e.BlockedSpec, &e.AwaitingApproval, &e.PROpen, &e.PRDReady); err != nil {
		return nil, err
	}
	u, err := n.sealer.Open(urlE)
	if err != nil {
		return nil, err
	}
	ch.URL = u
	if len(signE) > 0 {
		if ch.SignSecret, err = n.sealer.Open(signE); err != nil {
			return nil, err
		}
	}
	return &ch, nil
}

// GetChannel 读出解密后的渠道；未配置返回 (nil, nil)。
func (n *NotifyChannels) GetChannel(ctx context.Context, userID int64, kind string) (*notify.Channel, error) {
	ch, err := n.scanChannel(n.store.pool.QueryRow(ctx,
		`SELECT `+channelCols+` FROM notify_channels WHERE user_id=$1 AND kind=$2`, userID, kind))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读取通知渠道失败: %w", err)
	}
	return ch, nil
}

// DeleteChannel 删渠道（投递行随 CASCADE 消失）。返回是否真删了一行。
func (n *NotifyChannels) DeleteChannel(ctx context.Context, userID int64, kind string) (bool, error) {
	tag, err := n.store.pool.Exec(ctx, `DELETE FROM notify_channels WHERE user_id=$1 AND kind=$2`, userID, kind)
	if err != nil {
		return false, fmt.Errorf("store: 删除通知渠道失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ChannelView 是可安全展示给界面的渠道状态：URL 只有 scheme+host+省略号，
// 签名密钥只体现为 HasSecret，连掩码都不回。
type ChannelView struct {
	Kind       string               `json:"kind"`
	Configured bool                 `json:"configured"`
	MaskedURL  string               `json:"maskedURL"`
	HasSecret  bool                 `json:"hasSecret"`
	Enabled    bool                 `json:"enabled"`
	Events     notify.EventSwitches `json:"events"`
	TestOK     *bool                `json:"testOk"`
	TestError  *string              `json:"testError"`
	TestAt     *time.Time           `json:"testAt"`
}

// ListChannels 按 notify.Kinds() 的顺序回两种渠道（未配置的 Configured=false、开关默认全开）。
func (n *NotifyChannels) ListChannels(ctx context.Context, userID int64) ([]ChannelView, error) {
	rows, err := n.store.pool.Query(ctx, `
		SELECT kind, url_enc, sign_enc IS NOT NULL, enabled,
		       event_failed, event_blocked_spec, event_awaiting_approval, event_pr_open, event_prd_ready,
		       test_ok, test_error, test_at
		FROM notify_channels WHERE user_id=$1`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询通知渠道失败: %w", err)
	}
	defer rows.Close()
	got := map[string]ChannelView{}
	for rows.Next() {
		var (
			v    ChannelView
			urlE []byte
		)
		e := &v.Events
		if err := rows.Scan(&v.Kind, &urlE, &v.HasSecret, &v.Enabled,
			&e.Failed, &e.BlockedSpec, &e.AwaitingApproval, &e.PROpen, &e.PRDReady,
			&v.TestOK, &v.TestError, &v.TestAt); err != nil {
			return nil, fmt.Errorf("store: 读取通知渠道失败: %w", err)
		}
		v.Configured = true
		v.MaskedURL = "…"
		if u, err := n.sealer.Open(urlE); err == nil {
			v.MaskedURL = notify.MaskURL(u)
		}
		got[v.Kind] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 读取通知渠道失败: %w", err)
	}
	out := make([]ChannelView, 0, len(notify.Kinds()))
	for _, k := range notify.Kinds() {
		v, ok := got[k]
		if !ok {
			v = ChannelView{Kind: k, Events: notify.EventSwitches{Failed: true, BlockedSpec: true,
				AwaitingApproval: true, PROpen: true, PRDReady: true}, Enabled: true}
		}
		out = append(out, v)
	}
	return out, nil
}

// DeliveryView 是投递列表的一行。**不含 URL、payload 正文**。
type DeliveryView struct {
	ID        int64      `json:"id"`
	Kind      string     `json:"kind"`
	EventType string     `json:"eventType"`
	Status    string     `json:"status"`
	Attempts  int        `json:"attempts"`
	LastError *string    `json:"lastError"`
	SentAt    *time.Time `json:"sentAt"`
	CreatedAt time.Time  `json:"createdAt"`
}

// ListDeliveries 回本用户最近的投递（按归属过滤：JOIN 渠道的 user_id）。
func (n *NotifyChannels) ListDeliveries(ctx context.Context, userID int64, limit int) ([]DeliveryView, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := n.store.pool.Query(ctx, `
		SELECT d.id, c.kind, d.event_type, d.status, d.attempts, d.last_error, d.sent_at, d.created_at
		FROM notify_deliveries d JOIN notify_channels c ON c.id = d.channel_id
		WHERE c.user_id = $1
		ORDER BY d.id DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: 查询投递记录失败: %w", err)
	}
	defer rows.Close()
	out := []DeliveryView{}
	for rows.Next() {
		var v DeliveryView
		if err := rows.Scan(&v.ID, &v.Kind, &v.EventType, &v.Status, &v.Attempts, &v.LastError, &v.SentAt, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: 读取投递记录失败: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// eventColumn 把任务/PRD 事件映射到渠道开关列。列名只来自这张白名单，不拼外部输入。
func eventColumn(eventType string) (string, bool) {
	switch eventType {
	case notify.EventFailed:
		return "event_failed", true
	case notify.EventBlockedSpec:
		return "event_blocked_spec", true
	case notify.EventAwaitingApproval:
		return "event_awaiting_approval", true
	case notify.EventPROpen:
		return "event_pr_open", true
	case notify.EventPRDReady:
		return "event_prd_ready", true
	}
	return "", false
}

// enqueue 给 userID 名下所有放行该事件的渠道各插一行；返回实际插入行数。
func (n *NotifyChannels) enqueue(ctx context.Context, userID int64, eventType, key string,
	p notify.Payload, taskID, prdID *int64, at time.Time) (int, error) {
	col, ok := eventColumn(eventType)
	if !ok {
		return 0, fmt.Errorf("store: 不可入队的通知事件 %q", eventType)
	}
	body, err := json.Marshal(p)
	if err != nil {
		return 0, err
	}
	tag, err := n.store.pool.Exec(ctx, `
		INSERT INTO notify_deliveries (channel_id, event_type, event_key, target_task_id, target_prd_id, payload, next_attempt_at)
		SELECT c.id, $2, $3, $4, $5, $6, $7
		FROM notify_channels c
		WHERE c.user_id = $1 AND c.enabled AND c.`+col+`
		ON CONFLICT (channel_id, event_key) DO NOTHING`,
		userID, eventType, key, taskID, prdID, body, notify.EventTime(at))
	if err != nil {
		return 0, fmt.Errorf("store: 通知入队失败: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// EnqueueTaskEvent 实现 runner.ChatNotifier：只按 ev.UserID 查渠道，**不查 tasks 表**
// （ev 是转移那一刻的快照，异步执行时任务可能已经往前走了）。
func (n *NotifyChannels) EnqueueTaskEvent(ctx context.Context, ev notify.TaskEvent) (int, error) {
	if ev.EventType == notify.EventPRDReady {
		return 0, fmt.Errorf("store: 任务事件不能是 %q", ev.EventType)
	}
	id := ev.TaskID
	return n.enqueue(ctx, ev.UserID, ev.EventType, notify.TaskEventKey(ev), notify.TaskPayload(ev), &id, nil, ev.At)
}

// EnqueuePRDEvent 实现 prd.ReviewNotifier。
func (n *NotifyChannels) EnqueuePRDEvent(ctx context.Context, ev notify.PRDEvent) (int, error) {
	if ev.EventType != notify.EventPRDReady {
		return 0, fmt.Errorf("store: PRD 事件只能是 %q", notify.EventPRDReady)
	}
	id := ev.PRDID
	return n.enqueue(ctx, ev.UserID, ev.EventType, notify.PRDEventKey(ev), notify.PRDPayload(ev), nil, &id, ev.At)
}

func newNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // UUID v4 形态，便于人在群里辨认
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

// BeginTest 开一条测试消息：直接写成 sending（不经 pending，worker 从结构上领不到），
// 返回投递行与解密后的渠道。未配置返回 ErrChannelNotFound。
func (n *NotifyChannels) BeginTest(ctx context.Context, userID int64, kind string, now time.Time) (*notify.Delivery, *notify.Channel, error) {
	ch, err := n.GetChannel(ctx, userID, kind)
	if err != nil {
		return nil, nil, err
	}
	if ch == nil {
		return nil, nil, ErrChannelNotFound
	}
	nonce, err := newNonce()
	if err != nil {
		return nil, nil, err
	}
	p := notify.Payload{TestNonce: nonce}
	body, _ := json.Marshal(p)
	d := &notify.Delivery{ChannelID: ch.ID, EventType: notify.EventTest, EventKey: notify.TestEventKey(nonce), Payload: p}
	if err := n.store.pool.QueryRow(ctx, `
		INSERT INTO notify_deliveries (channel_id, event_type, event_key, payload, status, attempts, claimed_at, next_attempt_at)
		VALUES ($1, 'test', $2, $3, 'sending', 0, $4, $4)
		RETURNING id, claimed_at`, ch.ID, d.EventKey, body, now).Scan(&d.ID, &d.ClaimedAt); err != nil {
		return nil, nil, fmt.Errorf("store: 记录测试消息失败: %w", err)
	}
	return d, ch, nil
}

// FinishTest 在一个事务里写测试结果：投递行 CAS（成功 sent/attempts 不变；失败 gave_up/attempts=1）
// + 渠道行的 test_* 派生缓存。行已被回收（CAS 落空）时两者都不写。
func (n *NotifyChannels) FinishTest(ctx context.Context, d *notify.Delivery, now time.Time, ok bool, reason string) error {
	tx, err := n.store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: 记录测试结果失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var tag pgconn.CommandTag
	if ok {
		tag, err = tx.Exec(ctx, `UPDATE notify_deliveries SET status='sent', sent_at=$3, claimed_at=NULL
			WHERE id=$1 AND status='sending' AND claimed_at=$2`, d.ID, d.ClaimedAt, now)
	} else {
		tag, err = tx.Exec(ctx, `UPDATE notify_deliveries SET status='gave_up', attempts=1, last_error=$3, claimed_at=NULL
			WHERE id=$1 AND status='sending' AND claimed_at=$2`, d.ID, d.ClaimedAt, nullIfEmpty(reason))
	}
	if err != nil {
		return fmt.Errorf("store: 记录测试结果失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE notify_channels SET test_ok=$2, test_error=$3, test_at=$4 WHERE id=$1`,
		d.ChannelID, ok, nullIfEmpty(reason), now); err != nil {
		return fmt.Errorf("store: 记录测试结果失败: %w", err)
	}
	return tx.Commit(ctx)
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// RetryDelivery 把本用户一条非 test 的 gave_up 投递改回 pending（attempts 归零）。
// 先按归属查行：零行即 ErrDeliveryNotFound（属主隔离优先，httpapi 回 404）。
func (n *NotifyChannels) RetryDelivery(ctx context.Context, userID, id int64, now time.Time) error {
	tx, err := n.store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: 重试投递失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var eventType, status string
	err = tx.QueryRow(ctx, `
		SELECT d.event_type, d.status FROM notify_deliveries d
		JOIN notify_channels c ON c.id = d.channel_id
		WHERE d.id=$1 AND c.user_id=$2 FOR UPDATE OF d`, id, userID).Scan(&eventType, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDeliveryNotFound
	}
	if err != nil {
		return fmt.Errorf("store: 重试投递失败: %w", err)
	}
	if eventType == notify.EventTest {
		return ErrTestNotRetryable
	}
	if status != notify.StatusGaveUp {
		return ErrNotRetryable
	}
	if _, err := tx.Exec(ctx, `UPDATE notify_deliveries SET status='pending', attempts=0, next_attempt_at=$2, claimed_at=NULL
		WHERE id=$1`, id, now); err != nil {
		return fmt.Errorf("store: 重试投递失败: %w", err)
	}
	return tx.Commit(ctx)
}

// ---------------------------------------------------------------- notify.Outbox

// failureSQL 是 MarkRetry / MarkGaveUp / ReclaimStale 共用的那条写回：
// 带 CAS（status='sending' AND claimed_at=$6），零行即「已被回收/改写」。
// next 为 NULL 时 next_attempt_at 不改写（gave_up 没有下一次）。
const failureSQL = `
	UPDATE notify_deliveries SET status=$2, attempts=$3,
		next_attempt_at = COALESCE($4::timestamptz, next_attempt_at),
		last_error=$5, claimed_at=NULL
	WHERE id=$1 AND status='sending' AND claimed_at=$6`

// recordFailure 按 §3.8.2 写回一次失败：n = attempts+1，giveUp 由 notify.NextAttempt 决定
// （force=true 直接 gave_up：不可重试错误与过期的 test 行）。
func recordFailure(ctx context.Context, tx pgx.Tx, id int64, attemptsBefore int, claimedAt, now time.Time, reason string, force bool) (bool, error) {
	nAtt := attemptsBefore + 1
	next, giveUp := notify.NextAttempt(nAtt, now)
	status, nextArg := notify.StatusPending, any(next)
	if giveUp || force {
		status, nextArg = notify.StatusGaveUp, nil
	}
	tag, err := tx.Exec(ctx, failureSQL, id, status, nAtt, nextArg, nullIfEmpty(reason), claimedAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ReclaimStale 回收死掉的 sending：非 test 行走同一套 NextAttempt（>=5 即 gave_up，否则回 pending），
// test 行直接 gave_up —— 否则 worker 会在人早已离开页面后补发一条测试消息。
func (n *NotifyChannels) ReclaimStale(ctx context.Context, now time.Time, lease time.Duration) (int, error) {
	tx, err := n.store.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: 回收投递失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT id, attempts, event_type, claimed_at FROM notify_deliveries
		WHERE status='sending' AND claimed_at < $1
		ORDER BY id LIMIT 100 FOR UPDATE SKIP LOCKED`, now.Add(-lease))
	if err != nil {
		return 0, fmt.Errorf("store: 回收投递失败: %w", err)
	}
	type stale struct {
		id        int64
		attempts  int
		eventType string
		claimed   time.Time
	}
	var list []stale
	for rows.Next() {
		var s stale
		if err := rows.Scan(&s.id, &s.attempts, &s.eventType, &s.claimed); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: 回收投递失败: %w", err)
		}
		list = append(list, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: 回收投递失败: %w", err)
	}
	count := 0
	for _, s := range list {
		hit, err := recordFailure(ctx, tx, s.id, s.attempts, s.claimed, now, "lease expired", s.eventType == notify.EventTest)
		if err != nil {
			return 0, fmt.Errorf("store: 回收投递失败: %w", err)
		}
		if hit {
			count++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: 回收投递失败: %w", err)
	}
	return count, nil
}

// ClaimDueDelivery 一次只领一行到期投递（与 ClaimReady 同一手法：FOR UPDATE SKIP LOCKED）。
// 领到即写 sending + claimed_at=now；test 行从结构上（不经 pending）与条件上都领不到。
func (n *NotifyChannels) ClaimDueDelivery(ctx context.Context, now time.Time) (*notify.Delivery, error) {
	var (
		d    notify.Delivery
		body []byte
	)
	err := n.store.pool.QueryRow(ctx, `
		UPDATE notify_deliveries SET status='sending', claimed_at=$1
		WHERE id = (
			SELECT id FROM notify_deliveries
			WHERE status='pending' AND event_type <> 'test' AND next_attempt_at <= $1
			ORDER BY next_attempt_at, id LIMIT 1
			FOR UPDATE SKIP LOCKED)
		RETURNING id, channel_id, event_type, event_key, payload, attempts, claimed_at`, now).Scan(
		&d.ID, &d.ChannelID, &d.EventType, &d.EventKey, &body, &d.Attempts, &d.ClaimedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: 领取投递失败: %w", err)
	}
	// payload 解不开不阻断领取：worker 用零值渲染，最多是一条缺字段的通知。
	_ = json.Unmarshal(body, &d.Payload)
	return &d, nil
}

// DeliveryChannel 读投递所属渠道（解密）；渠道已删返回 (nil, nil)。
func (n *NotifyChannels) DeliveryChannel(ctx context.Context, channelID int64) (*notify.Channel, error) {
	ch, err := n.scanChannel(n.store.pool.QueryRow(ctx,
		`SELECT `+channelCols+` FROM notify_channels WHERE id=$1`, channelID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读取投递渠道失败: %w", err)
	}
	return ch, nil
}

// MarkSent 记一次成功（attempts 不变）。
func (n *NotifyChannels) MarkSent(ctx context.Context, id int64, claimedAt, now time.Time) (bool, error) {
	tag, err := n.store.pool.Exec(ctx, `UPDATE notify_deliveries SET status='sent', sent_at=$3, claimed_at=NULL
		WHERE id=$1 AND status='sending' AND claimed_at=$2`, id, claimedAt, now)
	if err != nil {
		return false, fmt.Errorf("store: 记录投递结果失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// MarkSkipped 记一次跳过（渠道在排队后被关掉/事件开关关了）。保留记录便于解释「为什么没收到」。
func (n *NotifyChannels) MarkSkipped(ctx context.Context, id int64, claimedAt, now time.Time, reason string) (bool, error) {
	_ = now // 签名与其余 Mark* 对齐；跳过不写时间列
	tag, err := n.store.pool.Exec(ctx, `UPDATE notify_deliveries SET status='skipped', last_error=$3, claimed_at=NULL
		WHERE id=$1 AND status='sending' AND claimed_at=$2`, id, claimedAt, nullIfEmpty(reason))
	if err != nil {
		return false, fmt.Errorf("store: 记录投递结果失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// MarkRetry 记一次可重试失败：SELECT attempts FOR UPDATE → Go 侧 NextAttempt → CAS UPDATE，一个事务。
func (n *NotifyChannels) MarkRetry(ctx context.Context, id int64, claimedAt, now time.Time, reason string) (bool, error) {
	return n.markFailure(ctx, id, claimedAt, now, reason, false)
}

// MarkGaveUp 记一次不可重试失败：attempts+1，直接 gave_up。
func (n *NotifyChannels) MarkGaveUp(ctx context.Context, id int64, claimedAt, now time.Time, reason string) (bool, error) {
	return n.markFailure(ctx, id, claimedAt, now, reason, true)
}

func (n *NotifyChannels) markFailure(ctx context.Context, id int64, claimedAt, now time.Time, reason string, force bool) (bool, error) {
	tx, err := n.store.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: 记录投递结果失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var attempts int
	err = tx.QueryRow(ctx, `SELECT attempts FROM notify_deliveries
		WHERE id=$1 AND status='sending' AND claimed_at=$2 FOR UPDATE`, id, claimedAt).Scan(&attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: 记录投递结果失败: %w", err)
	}
	hit, err := recordFailure(ctx, tx, id, attempts, claimedAt, now, reason, force)
	if err != nil {
		return false, fmt.Errorf("store: 记录投递结果失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: 记录投递结果失败: %w", err)
	}
	return hit, nil
}
