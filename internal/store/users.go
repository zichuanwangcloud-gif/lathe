package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// 角色。与 users 表的 role CHECK 约束一致。
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// ValidRole 报告角色取值是否合法。
func ValidRole(role string) bool {
	return role == RoleAdmin || role == RoleMember
}

// ErrUserNotFound 表示按 id 或邮箱找不到用户。
var ErrUserNotFound = errors.New("store: 用户不存在")

// ErrEmailTaken 表示邮箱已被注册。
//
// 单独成错误而非透传数据库的唯一约束冲突：注册接口要据此回一句
// 人能看懂的提示，而不是把 SQLSTATE 23505 抛给用户。
var ErrEmailTaken = errors.New("store: 邮箱已被注册")

// User 是一个账号。
//
// PasswordHash 刻意不带 json tag 的导出 —— 这个结构不直接序列化给界面，
// 需要给界面的形状是 UserListRow。
type User struct {
	ID                 int64
	Email              string
	PasswordHash       string
	Role               string
	DisabledAt         *time.Time
	MustChangePassword bool
	LastLoginAt        *time.Time
	CreatedAt          time.Time
}

// Disabled 报告账号是否已被禁用。
func (u *User) Disabled() bool { return u.DisabledAt != nil }

// IsAdmin 报告账号是否为管理员。
func (u *User) IsAdmin() bool { return u.Role == RoleAdmin }

// NormalizeEmail 统一邮箱大小写与首尾空白。
//
// 必须在写入与查询两侧都过一遍，否则 "A@x.com" 注册完再用 "a@x.com"
// 就登不进去 —— 唯一约束是按字节比的。
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// Users 读写账号。
type Users struct {
	store *Store
}

// NewUsers 构造账号读写器。
func (s *Store) NewUsers() *Users { return &Users{store: s} }

const userCols = `id, email, coalesce(password_hash, ''), role,
	disabled_at, must_change_password, last_login_at, created_at`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role,
		&u.DisabledAt, &u.MustChangePassword, &u.LastLoginAt, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读取用户失败: %w", err)
	}
	return &u, nil
}

// randomSlug 生成 webhook 回调地址里的那一段随机标识。
//
// 在 store 层生成而不是让调用方传：每条新用户都必须有 slug，交给调用方
// 就会有人忘。crypto/rand 取不到熵时 panic —— 拿不到随机数就不该继续。
func randomSlug() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("store: 无法获取随机数: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// Create 新建账号。邮箱重复时返回 ErrEmailTaken。
func (u *Users) Create(ctx context.Context, email, passwordHash, role string) (*User, error) {
	if !ValidRole(role) {
		return nil, fmt.Errorf("store: 未知角色 %q", role)
	}
	row := u.store.pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash, role, webhook_slug)
		VALUES ($1, $2, $3, $4)
		RETURNING `+userCols, NormalizeEmail(email), passwordHash, role, randomSlug())

	usr, err := scanUser(row)
	if err != nil {
		var pgErr *pgconn.PgError
		// 23505 = unique_violation，这里只可能是 email 唯一约束
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrEmailTaken
		}
		return nil, err
	}
	return usr, nil
}

// ByEmail 按邮箱查账号。
func (u *Users) ByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(u.store.pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE email = $1`, NormalizeEmail(email)))
}

// ByID 按 id 查账号。
func (u *Users) ByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(u.store.pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE id = $1`, id))
}

// SetPassword 设置密码哈希。
//
// mustChange 为真时要求对方下次登录必须改密 —— 管理员代设密码与
// 超管初始密码都走这条路。
func (u *Users) SetPassword(ctx context.Context, id int64, hash string, mustChange bool) error {
	tag, err := u.store.pool.Exec(ctx, `
		UPDATE users SET password_hash = $2, must_change_password = $3, updated_at = now()
		WHERE id = $1`, id, hash, mustChange)
	if err != nil {
		return fmt.Errorf("store: 更新密码失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// SetRole 改角色。
func (u *Users) SetRole(ctx context.Context, id int64, role string) error {
	if !ValidRole(role) {
		return fmt.Errorf("store: 未知角色 %q", role)
	}
	tag, err := u.store.pool.Exec(ctx,
		`UPDATE users SET role = $2, updated_at = now() WHERE id = $1`, id, role)
	if err != nil {
		return fmt.Errorf("store: 更新角色失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// SetDisabled 启用或禁用账号。
func (u *Users) SetDisabled(ctx context.Context, id int64, disabled bool) error {
	var at any
	if disabled {
		at = time.Now()
	}
	tag, err := u.store.pool.Exec(ctx,
		`UPDATE users SET disabled_at = $2, updated_at = now() WHERE id = $1`, id, at)
	if err != nil {
		return fmt.Errorf("store: 更新账号状态失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// TouchLogin 记录一次成功登录的时间。
func (u *Users) TouchLogin(ctx context.Context, id int64) error {
	if _, err := u.store.pool.Exec(ctx,
		`UPDATE users SET last_login_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("store: 记录登录时间失败: %w", err)
	}
	return nil
}

// Delete 删除账号。
//
// 其名下的任务、仓库配置、凭据、会话、重置令牌都靠外键 ON DELETE CASCADE
// 一并消失 —— 不在这里手工逐表删，免得漏掉将来新增的表。
func (u *Users) Delete(ctx context.Context, id int64) error {
	tag, err := u.store.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: 删除用户失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// UserListRow 是用户管理页的一行。可安全序列化给界面 —— 不含密码哈希。
type UserListRow struct {
	ID                 int64      `json:"id"`
	Email              string     `json:"email"`
	Role               string     `json:"role"`
	Disabled           bool       `json:"disabled"`
	DisabledAt         *time.Time `json:"disabledAt"`
	MustChangePassword bool       `json:"mustChangePassword"`
	HasPassword        bool       `json:"hasPassword"`
	LastLoginAt        *time.Time `json:"lastLoginAt"`
	CreatedAt          time.Time  `json:"createdAt"`

	TaskTotal  int `json:"taskTotal"`
	TaskOK     int `json:"taskOk"`
	TaskFailed int `json:"taskFailed"`
}

// List 返回全部账号及各自的任务计数。
//
// 计数用 FILTER 而非多次子查询：一次扫表出三个数。pr_open 与 merged 都算成功
// —— 产出可合并的 PR 就是 Lathe 的交付标准（见 Stats 的同样处理）。
func (u *Users) List(ctx context.Context) ([]UserListRow, error) {
	rows, err := u.store.pool.Query(ctx, `
		SELECT u.id, u.email, u.role, u.disabled_at, u.must_change_password,
		       u.password_hash IS NOT NULL, u.last_login_at, u.created_at,
		       count(t.id),
		       count(t.id) FILTER (WHERE t.state IN ('merged', 'pr_open')),
		       count(t.id) FILTER (WHERE t.state = 'failed')
		FROM users u
		LEFT JOIN tasks t ON t.user_id = u.id
		GROUP BY u.id
		ORDER BY u.id`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询用户列表失败: %w", err)
	}
	defer rows.Close()

	out := []UserListRow{}
	for rows.Next() {
		var r UserListRow
		if err := rows.Scan(&r.ID, &r.Email, &r.Role, &r.DisabledAt, &r.MustChangePassword,
			&r.HasPassword, &r.LastLoginAt, &r.CreatedAt,
			&r.TaskTotal, &r.TaskOK, &r.TaskFailed); err != nil {
			return nil, fmt.Errorf("store: 读取用户行失败: %w", err)
		}
		r.Disabled = r.DisabledAt != nil
		out = append(out, r)
	}
	return out, rows.Err()
}

// EnsureAdmin 取得（或创建）指定邮箱的管理员账号，并保证它是启用的。
//
// 幂等，每次启动都跑：顺带把被误停用的内置超管救回来 —— 那正是
// 「内置账号」存在的意义，不该有办法把平台彻底锁死。
//
// 不设口令：SQL 算不出 bcrypt，口令由调用方在拿到账号后按需补。
func (u *Users) EnsureAdmin(ctx context.Context, email string) (*User, error) {
	var id int64
	err := u.store.pool.QueryRow(ctx, `
		INSERT INTO users (email, role, webhook_slug) VALUES ($1, $2, $3)
		ON CONFLICT (email) DO UPDATE SET
			role = $2, disabled_at = NULL, updated_at = now()
		RETURNING id`, NormalizeEmail(email), RoleAdmin, randomSlug()).Scan(&id)
	if err != nil {
		return nil, fmt.Errorf("store: 初始化管理员失败: %w", err)
	}
	// 从 P0 升级上来的那条老记录没有 slug，这里补齐
	if err := u.EnsureWebhookSlug(ctx, id); err != nil {
		return nil, err
	}
	return u.ByID(ctx, id)
}

// WorktreePaths 返回某账号名下任务占用的工作区目录。
//
// 删除账号时用：数据库的行会被外键级联带走，磁盘上的目录不会。
// 先把路径捞出来记进日志，人才有办法回收那些空间。
func (u *Users) WorktreePaths(ctx context.Context, id int64) ([]string, error) {
	rows, err := u.store.pool.Query(ctx, `
		SELECT worktree_path FROM tasks
		WHERE user_id = $1 AND worktree_path IS NOT NULL`, id)
	if err != nil {
		return nil, fmt.Errorf("store: 查询工作区路径失败: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Count 返回账号总数。
func (u *Users) Count(ctx context.Context) (int, error) {
	var n int
	if err := u.store.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: 统计用户数失败: %w", err)
	}
	return n, nil
}

// CountAdmins 统计**可用**的管理员数量，excludeID 为正时排除该账号。
//
// 用途是「不许干掉最后一个管理员」的判定：传入待删/待降级的 id，
// 返回 0 就说明操作完就没人进得去设置页了。
//
// 「可用」要求同时满足未停用且有密码。少了密码这个条件，判定会被一个
// 登不进来的账号满足 —— 0004 迁移把 P0 时期那条老 users 行提成了管理员，
// 而它的 password_hash 是 NULL，正是这种账号。
func (u *Users) CountAdmins(ctx context.Context, excludeID int64) (int, error) {
	var n int
	if err := u.store.pool.QueryRow(ctx, `
		SELECT count(*) FROM users
		WHERE role = $1 AND disabled_at IS NULL AND password_hash IS NOT NULL
		  AND id <> $2`,
		RoleAdmin, excludeID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: 统计管理员数失败: %w", err)
	}
	return n, nil
}

// EnsureWebhookSlug 给缺 slug 的账号补一个，已有则不动。
//
// 存在的原因只有一个：0004 迁移把 P0 时期那条老 users 行升成了管理员，
// 而那行没有 slug。新注册的账号在 Create 里就带上了。
func (u *Users) EnsureWebhookSlug(ctx context.Context, id int64) error {
	if _, err := u.store.pool.Exec(ctx, `
		UPDATE users SET webhook_slug = $2, updated_at = now()
		WHERE id = $1 AND webhook_slug IS NULL`, id, randomSlug()); err != nil {
		return fmt.Errorf("store: 补写 webhook 标识失败: %w", err)
	}
	return nil
}
