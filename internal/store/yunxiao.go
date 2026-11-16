package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zichuanwangcloud-gif/lathe/internal/secret"
)

// 云效连接（f08，docs/features/tracker-yunxiao.md §3.1）的存储层。
//
// 与 integrations 表分开存（理由见 migration 0054 的注释），加密口径完全
// 复用 secret.Sealer：token_enc、webhook_secret_enc 都是 AES-256-GCM 密文，
// 明文只在 Get 的返回值里出现，Status 只给掩码。

var (
	// ErrYunxiaoNotConfigured 表示该用户尚未配置云效连接。
	ErrYunxiaoNotConfigured = errors.New("store: 云效连接未配置")
	// ErrYunxiaoTokenRequired 表示首次保存时没给令牌。
	ErrYunxiaoTokenRequired = errors.New("store: 首次保存云效连接必须填写令牌")
)

// SettingYunxiaoAllowedHosts 是 Region 版实例域名允许列表的系统设置键
// （JSON 字符串数组）。消费方：httpapi 保存连接时的 ValidateEndpoint，与
// creds 每次构造云效客户端时的 NewClient —— 两处都现取，管理员撤掉某域名
// 后已存连接下一次调用即失效。默认空 = 只放行中心版。
const SettingYunxiaoAllowedHosts = "yunxiao_allowed_hosts"

// YunxiaoConnection 是解密后的连接（只在服务端内部流转，不序列化给界面）。
type YunxiaoConnection struct {
	UserID         int64
	Endpoint       string
	OrganizationID string
	Token          string
	WebhookSecret  string // 空 = 未配置 webhook 密钥
	AccountID      string // 验证回写；「指派给我」判定用
	AccountName    string
	TriggerLabel   string
	StatusStarted  string
	StatusPROpen   string
	StatusMerged   string
	StatusCancel   string
	VerifiedAt     *time.Time
	VerifyError    *string
}

// YunxiaoSaveParams 是一次保存的输入（已由调用方校验）。
//
// Token / WebhookSecret 为空串表示「保留原值」；其余字段按给定值覆盖。
type YunxiaoSaveParams struct {
	Endpoint       string
	OrganizationID string
	Token          string
	WebhookSecret  string
	TriggerLabel   string
	StatusStarted  string
	StatusPROpen   string
	StatusMerged   string
	StatusCancel   string
}

// YunxiaoStatus 是可安全展示给界面的连接状态：任何字段都不含明文。
type YunxiaoStatus struct {
	Configured          bool       `json:"configured"`
	Endpoint            string     `json:"endpoint,omitempty"`
	OrganizationID      string     `json:"organizationId,omitempty"`
	TokenMasked         string     `json:"tokenMasked,omitempty"`
	WebhookSecretMasked string     `json:"webhookSecretMasked,omitempty"`
	HasWebhookSecret    bool       `json:"hasWebhookSecret"`
	AccountName         string     `json:"accountName,omitempty"`
	TriggerLabel        string     `json:"triggerLabel"`
	StatusStarted       string     `json:"statusStarted"`
	StatusPROpen        string     `json:"statusPrOpen"`
	StatusMerged        string     `json:"statusMerged"`
	StatusCancel        string     `json:"statusCancel"`
	VerifiedAt          *time.Time `json:"verifiedAt"`
	VerifyError         *string    `json:"verifyError"`
}

// YunxiaoConnections 读写云效连接与接单快照。
type YunxiaoConnections struct {
	sealer secret.Sealer
	store  *Store
}

// NewYunxiaoConnections 构造云效连接读写器。
func (s *Store) NewYunxiaoConnections(sealer secret.Sealer) *YunxiaoConnections {
	return &YunxiaoConnections{sealer: sealer, store: s}
}

type yunxiaoRow struct {
	endpoint, org                  string
	tokEnc, secEnc                 []byte
	accountID, accountName         *string
	label, started, prOpen, merged string
	cancel                         string
	verifiedAt                     *time.Time
	verifyError                    *string
}

func (y *YunxiaoConnections) load(ctx context.Context, userID int64) (*yunxiaoRow, error) {
	var r yunxiaoRow
	var org *string
	err := y.store.pool.QueryRow(ctx, `
		SELECT endpoint, organization_id, token_enc, webhook_secret_enc, account_id, account_name,
		       trigger_label, status_started, status_pr_open, status_merged, status_cancel,
		       verified_at, verify_error
		FROM yunxiao_connections WHERE user_id = $1`, userID,
	).Scan(&r.endpoint, &org, &r.tokEnc, &r.secEnc, &r.accountID, &r.accountName,
		&r.label, &r.started, &r.prOpen, &r.merged, &r.cancel, &r.verifiedAt, &r.verifyError)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrYunxiaoNotConfigured
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读取云效连接失败: %w", err)
	}
	if org != nil {
		r.org = *org
	}
	return &r, nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Get 解密读取连接。未配置返回 ErrYunxiaoNotConfigured。
func (y *YunxiaoConnections) Get(ctx context.Context, userID int64) (*YunxiaoConnection, error) {
	r, err := y.load(ctx, userID)
	if err != nil {
		return nil, err
	}
	tok, err := y.sealer.Open(r.tokEnc)
	if err != nil {
		return nil, fmt.Errorf("store: 云效令牌无法解密（主密钥可能已更换），请重新配置: %w", err)
	}
	c := &YunxiaoConnection{
		UserID: userID, Endpoint: r.endpoint, OrganizationID: r.org, Token: tok,
		AccountID: deref(r.accountID), AccountName: deref(r.accountName),
		TriggerLabel: r.label, StatusStarted: r.started, StatusPROpen: r.prOpen,
		StatusMerged: r.merged, StatusCancel: r.cancel,
		VerifiedAt: r.verifiedAt, VerifyError: r.verifyError,
	}
	if len(r.secEnc) > 0 {
		sec, err := y.sealer.Open(r.secEnc)
		if err != nil {
			return nil, fmt.Errorf("store: 云效 webhook 密钥无法解密（主密钥可能已更换），请重新配置: %w", err)
		}
		c.WebhookSecret = sec
	}
	return c, nil
}

// Save 写入连接。Token / WebhookSecret 为空串保留原值（首次保存 Token 必填）；
// Token 换了新值时同句把 account_id / account_name / verified_at 置 NULL ——
// 旧账号的接单判定对新令牌不再成立。
func (y *YunxiaoConnections) Save(ctx context.Context, userID int64, p YunxiaoSaveParams) error {
	var tokEnc, secEnc []byte
	var err error
	if p.Token != "" {
		if tokEnc, err = y.sealer.Seal(p.Token); err != nil {
			return err
		}
	}
	if p.WebhookSecret != "" {
		if secEnc, err = y.sealer.Seal(p.WebhookSecret); err != nil {
			return err
		}
	}
	var org *string
	if p.OrganizationID != "" {
		org = &p.OrganizationID
	}

	if tokEnc == nil {
		// 只能更新已有行：没有令牌就没有可用的连接。
		tag, err := y.store.pool.Exec(ctx, `
			UPDATE yunxiao_connections SET
				endpoint = $2, organization_id = $3,
				webhook_secret_enc = COALESCE($4, webhook_secret_enc),
				trigger_label = $5, status_started = $6, status_pr_open = $7,
				status_merged = $8, status_cancel = $9
			WHERE user_id = $1`,
			userID, p.Endpoint, org, secEnc, p.TriggerLabel, p.StatusStarted, p.StatusPROpen, p.StatusMerged, p.StatusCancel)
		if err != nil {
			return fmt.Errorf("store: 保存云效连接失败: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrYunxiaoTokenRequired
		}
		return nil
	}

	_, err = y.store.pool.Exec(ctx, `
		INSERT INTO yunxiao_connections (
			user_id, endpoint, organization_id, token_enc, webhook_secret_enc,
			trigger_label, status_started, status_pr_open, status_merged, status_cancel)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (user_id) DO UPDATE SET
			endpoint           = EXCLUDED.endpoint,
			organization_id    = EXCLUDED.organization_id,
			token_enc          = EXCLUDED.token_enc,
			webhook_secret_enc = COALESCE(EXCLUDED.webhook_secret_enc, yunxiao_connections.webhook_secret_enc),
			trigger_label      = EXCLUDED.trigger_label,
			status_started     = EXCLUDED.status_started,
			status_pr_open     = EXCLUDED.status_pr_open,
			status_merged      = EXCLUDED.status_merged,
			status_cancel      = EXCLUDED.status_cancel,
			account_id         = NULL,
			account_name       = NULL,
			verified_at        = NULL,
			verify_error       = NULL`,
		userID, p.Endpoint, org, tokEnc, secEnc, p.TriggerLabel, p.StatusStarted, p.StatusPROpen, p.StatusMerged, p.StatusCancel)
	if err != nil {
		return fmt.Errorf("store: 保存云效连接失败: %w", err)
	}
	return nil
}

// Delete 在一个事务里删除连接行与该用户【全部】接单快照（幂等）。
//
// 快照必须一起删：留着旧快照，重新连接后第一次投递会被当成
// 「已指派→已指派」吞掉，不接单。
func (y *YunxiaoConnections) Delete(ctx context.Context, userID int64) error {
	tx, err := y.store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM yunxiao_trigger_state WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("store: 删除云效接单快照失败: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM yunxiao_connections WHERE user_id = $1`, userID); err != nil {
		return fmt.Errorf("store: 删除云效连接失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: 提交删除失败: %w", err)
	}
	return nil
}

// Status 返回可展示的连接状态（只有掩码）。未配置返回 Configured=false。
func (y *YunxiaoConnections) Status(ctx context.Context, userID int64) (*YunxiaoStatus, error) {
	r, err := y.load(ctx, userID)
	if errors.Is(err, ErrYunxiaoNotConfigured) {
		return &YunxiaoStatus{}, nil
	}
	if err != nil {
		return nil, err
	}
	st := &YunxiaoStatus{
		Configured: true, Endpoint: r.endpoint, OrganizationID: r.org,
		AccountName: deref(r.accountName), TriggerLabel: r.label,
		StatusStarted: r.started, StatusPROpen: r.prOpen, StatusMerged: r.merged, StatusCancel: r.cancel,
		VerifiedAt: r.verifiedAt, VerifyError: r.verifyError,
		HasWebhookSecret: len(r.secEnc) > 0,
	}
	if plain, err := y.sealer.Open(r.tokEnc); err == nil {
		st.TokenMasked = secret.Mask(plain)
	} else {
		msg := "凭据无法解密（主密钥可能已更换），请重新配置"
		st.VerifyError = &msg
	}
	if len(r.secEnc) > 0 {
		if plain, err := y.sealer.Open(r.secEnc); err == nil {
			st.WebhookSecretMasked = secret.Mask(plain)
		}
	}
	return st, nil
}

// MarkVerified 记录一次成功验证：回写账号、验证时间，清空失败原因。
func (y *YunxiaoConnections) MarkVerified(ctx context.Context, userID int64, accountID, accountName string) error {
	if _, err := y.store.pool.Exec(ctx, `
		UPDATE yunxiao_connections SET
			account_id = $2, account_name = $3, verified_at = now(), verify_error = NULL
		WHERE user_id = $1`, userID, accountID, accountName); err != nil {
		return fmt.Errorf("store: 记录云效验证结果失败: %w", err)
	}
	return nil
}

// MarkVerifyFailed 记录一次失败验证：写失败原因（只存分类人话）、清空
// verified_at；account_id 保留旧值 —— 网络抖动不该让接单判定失灵。
func (y *YunxiaoConnections) MarkVerifyFailed(ctx context.Context, userID int64, reason string) error {
	if _, err := y.store.pool.Exec(ctx, `
		UPDATE yunxiao_connections SET verify_error = $2, verified_at = NULL
		WHERE user_id = $1`, userID, reason); err != nil {
		return fmt.Errorf("store: 记录云效验证失败原因失败: %w", err)
	}
	return nil
}

// ObserveTrigger 记下工作项这一次的「指派给我 / 带触发标签」快照，返回
// 是否出现上升沿（任一项 false→true；首见即为真也算上升沿）。
func (y *YunxiaoConnections) ObserveTrigger(ctx context.Context, userID int64, workitemID string, assigned, labeled bool) (bool, error) {
	tx, err := y.store.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 三步写法（§3.6，评审 B1）。READ COMMITTED 即可，不依赖可串行化：
	//
	// ① 占位：保证这一行存在，之后才有东西可锁。首见时并发的 N 个事务里
	//    只有一个真正插入，其余在 PK 上等它提交后走 DO NOTHING。占位值
	//    (false,false) 就是「从没见过」的语义（表的列默认值）。
	//
	// 不能写成「SELECT … FOR UPDATE（无行视为 false）→ UPSERT」：Postgres
	// 没有间隙锁，FOR UPDATE 锁不住还不存在的行，首见时 N 个事务都会读到
	// 「无行」、都算出上升沿（实测 20 并发最多 20 个，red-AC-8-naive）。
	if _, err := tx.Exec(ctx, `
		INSERT INTO yunxiao_trigger_state (user_id, workitem_id) VALUES ($1, $2)
		ON CONFLICT (user_id, workitem_id) DO NOTHING`, userID, workitemID); err != nil {
		return false, fmt.Errorf("store: 占位接单快照失败: %w", err)
	}
	// ② 锁行读旧值：行此时一定存在，FOR UPDATE 把同一工作项的并发观察排成串。
	var prevA, prevL bool
	if err := tx.QueryRow(ctx, `
		SELECT assigned, labeled FROM yunxiao_trigger_state
		WHERE user_id = $1 AND workitem_id = $2 FOR UPDATE`, userID, workitemID).Scan(&prevA, &prevL); err != nil {
		return false, fmt.Errorf("store: 读取接单快照失败: %w", err)
	}
	// ③ 写回新快照。
	if _, err := tx.Exec(ctx, `
		UPDATE yunxiao_trigger_state SET assigned = $3, labeled = $4, updated_at = now()
		WHERE user_id = $1 AND workitem_id = $2`, userID, workitemID, assigned, labeled); err != nil {
		return false, fmt.Errorf("store: 写入接单快照失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: 提交接单快照失败: %w", err)
	}
	return (assigned && !prevA) || (labeled && !prevL), nil
}

// YunxiaoAllowedHosts 现取 Region 版实例域名允许列表。未配置或值损坏时
// 返回空 —— 退到最保守的行为（只放行中心版）。
func (s *Store) YunxiaoAllowedHosts(ctx context.Context) []string {
	v, err := s.Setting(ctx, SettingYunxiaoAllowedHosts)
	if err != nil || strings.TrimSpace(v) == "" {
		return nil
	}
	var hosts []string
	if err := json.Unmarshal([]byte(v), &hosts); err != nil {
		return nil
	}
	return hosts
}

// SetYunxiaoAllowedHosts 覆盖写允许列表（调用方已校验每一项）。
func (s *Store) SetYunxiaoAllowedHosts(ctx context.Context, hosts []string) error {
	if hosts == nil {
		hosts = []string{}
	}
	b, err := json.Marshal(hosts)
	if err != nil {
		return err
	}
	return s.SetSetting(ctx, SettingYunxiaoAllowedHosts, string(b))
}
