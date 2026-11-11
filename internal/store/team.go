package store

// 团队与角色（f12，docs/features/team-roles.md）。
//
// 一句话：团队共享的是【可见性】和团队内的组织管理，不是执行身份，也不是仓库
// 配置的编辑权。任务仍归属创建者（tasks.user_id 不变），凭据仍按 tk.UserID 解析，
// 仓库配置只有仓库属主能改（UpdateRepo 一字不改）。
//
// 读可见性只有一个判定入口：RepoAccess。任务可见 = 任务所属仓库可见。
//
// 写事务的全局锁序（docs/features/team-roles.md §3.4.2，唯一规则）：
//
//	teams → repos → team_members / team_invitations
//
// 第一条语句必须显式锁住涉及的 teams 行（管理动作 FOR UPDATE，共享 / 接受邀请
// FOR KEY SHARE），不允许让外键检查在事务中途隐式去拿它；一次锁多个团队时 ORDER BY id。
// 违反这个顺序会与别的团队写事务成环死锁（40P01），PRD 阶段已实测复现。
// 40P01 / 40001 由 teamTx 兜底重跑，耗尽后返回 ErrTeamBusy（503）。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// 团队内角色。与 users.role（站内 admin/member）正交，不复用、不合并：
// 站内管理员不会自动成为任何团队的成员，团队 owner 也不是站内管理员。
const (
	TeamRoleOwner  = "owner"
	TeamRoleAdmin  = "admin"
	TeamRoleMember = "member"
)

// RepoRole 是 viewer 对一个仓库的关系；不可见时 RepoAccess 返回 ErrRepoNotFound。
type RepoRole string

const (
	RepoRoleOwner      RepoRole = "owner"
	RepoRoleTeamOwner  RepoRole = "team_owner"
	RepoRoleTeamAdmin  RepoRole = "team_admin"
	RepoRoleTeamMember RepoRole = "team_member"
)

// InviteTTL 是邀请码有效期。它是业务规则不是部署差异（与「团队名 1–64 字符」
// 同口径），所以是常量而不是环境变量；消费方是 accept 时的 expires_at > now()。
const InviteTTL = 48 * time.Hour

// teamNameMaxRunes 与 0065 的 teams_name_len_check 一致。
const teamNameMaxRunes = 64

// teamTxMaxAttempts 是团队写事务在 40P01/40001 上的最多尝试次数（含首次）。
const teamTxMaxAttempts = 3

var (
	// ErrTeamNotFound：团队不存在、不是成员、或无权执行该动作 —— 三者刻意不分，
	// API 层一律映射成同一个 404「团队不存在」，不让 teams.id 变成存在性探针。
	ErrTeamNotFound = errors.New("store: 团队不存在")
	// ErrInviteInvalid：邀请码无效、已用、已撤销、已过期或接受者已是成员 —— 不分。
	ErrInviteInvalid = errors.New("store: 邀请码无效")
	// ErrInvitationNotFound：已授权的管理者撤销一条不存在（或已不是 pending）的邀请。
	ErrInvitationNotFound = errors.New("store: 邀请不存在")
	// ErrTeamNeedsOwner：owner 不能降级/退出自己（每个团队恰好一个 owner）。
	ErrTeamNeedsOwner = errors.New("store: 团队至少需要一个 owner，先转让")
	// ErrTeamHasSharedRepos：删团队前必须先无共享仓库。
	ErrTeamHasSharedRepos = errors.New("store: 团队还有共享仓库，先取消共享或移除共享了仓库的成员")
	// ErrInvalidTeamRole：角色取值非法（PATCH / 邀请都不接受 owner：owner 只经转让产生）。
	ErrInvalidTeamRole = errors.New("store: 角色须为 admin 或 member")
	// ErrInvalidTeamName：团队名须 1–64 字符。
	ErrInvalidTeamName = errors.New("store: 团队名须为 1–64 个字符")
	// ErrTeamBusy：团队写事务在锁冲突上重试耗尽，映射为 503。
	ErrTeamBusy = errors.New("store: 团队数据正忙，请重试")
	// ErrTaskNotVisible：任务不存在或当前用户不可见（两者不分）。
	ErrTaskNotVisible = errors.New("store: 任务不存在")
)

// UserOwnsTeamError 表示要删的账号是某个仍有其他成员的团队的 owner。
type UserOwnsTeamError struct {
	TeamID   int64
	TeamName string
}

func (e *UserOwnsTeamError) Error() string {
	return "store: 该用户是团队「" + e.TeamName + "」的 owner，先转让 owner"
}

// Team 是团队基本信息。
type Team struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}

// TeamSummary 是「我加入的团队」列表的一行。
type TeamSummary struct {
	Team
	Role        string `json:"role"`
	MemberCount int    `json:"memberCount"`
	SharedRepos int    `json:"sharedRepos"`
}

// TeamMember 是团队成员表的一行。邮箱只对同团队成员可见（GetTeam 要求成员）。
type TeamMember struct {
	UserID   int64     `json:"userId"`
	Email    string    `json:"email"`
	Role     string    `json:"role"`
	JoinedAt time.Time `json:"joinedAt"`
}

// TeamDetail 是团队详情：成员表 + 我的角色。
type TeamDetail struct {
	Team
	MyRole  string       `json:"myRole"`
	Members []TeamMember `json:"members"`
}

// Invitation 是一条邀请。Code 只在 CreateInvitation 的返回值里填充 —— 明文码
// 只出现这一次，库里只有 sha256 与末 4 位提示。
type Invitation struct {
	ID           int64     `json:"id"`
	Role         string    `json:"role"`
	Hint         string    `json:"hint"`
	InviteeEmail *string   `json:"inviteeEmail,omitempty"`
	CreatedBy    *int64    `json:"createdBy"`
	ExpiresAt    time.Time `json:"expiresAt"`
	Code         string    `json:"code,omitempty"`
}

// ViewerRepo 是团队视图里的一行仓库：内嵌 RepoRow（JSON 平铺），加上「是不是
// 我的」与「共享到哪个团队」。属主 ID 只在 store 内部使用，不出现在返回值里。
type ViewerRepo struct {
	RepoRow
	Owned  bool   `json:"owned"`
	TeamID *int64 `json:"teamId"`
}

// ---------------------------------------------------------------- 事务包装

var teamTxRetries atomic.Int64

// TeamTxRetries 返回团队写事务因 40P01/40001 重试的累计次数（只读，供测试断言
// 「锁序本身没有成环，重试从未被用到」）。
func TeamTxRetries() int64 { return teamTxRetries.Load() }

// teamLockHook 是测试钩子：团队写事务在拿到首把 teams 锁之后调用它（nil 时不调用）。
// AC-49 用它让 tx1 停在「持有第一把锁」处，构造确定性交错。生产永远为 nil。
var teamLockHook atomic.Pointer[func(op string)]

func lockStep(op string) {
	if h := teamLockHook.Load(); h != nil {
		(*h)(op)
	}
}

func retryableTxErr(err error) (string, bool) {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && (pg.Code == "40P01" || pg.Code == "40001") {
		return pg.Code, true
	}
	return "", false
}

// teamTx 是团队写事务的唯一入口：BEGIN … COMMIT，遇到 40P01（死锁）/ 40001
// （序列化失败）整个事务重跑，最多 teamTxMaxAttempts 次，两次之间随机退避
// 10–50ms；耗尽返回 ErrTeamBusy。其他错误照旧返回，不重试。
//
// 锁序（见文件头）是防线，这里只是兜底：AC-48 断言并发下重试计数增量为 0。
func (s *Store) teamTx(ctx context.Context, op string, fn func(pgx.Tx) error) error {
	var last error
	for attempt := 1; attempt <= teamTxMaxAttempts; attempt++ {
		err := s.teamTxOnce(ctx, fn)
		code, retry := retryableTxErr(err)
		if !retry {
			return err
		}
		last = err
		if attempt == teamTxMaxAttempts {
			break
		}
		teamTxRetries.Add(1)
		slog.Warn("team tx retry", "sqlstate", code, "op", op, "attempt", attempt)
		n, _ := rand.Int(rand.Reader, big.NewInt(41))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(10+n.Int64()) * time.Millisecond):
		}
	}
	return fmt.Errorf("%w (%s: %v)", ErrTeamBusy, op, last)
}

func (s *Store) teamTxOnce(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// lockTeamForUpdate 是管理动作的首句：锁团队行，并在锁内重读调用者角色。
// 团队不存在或调用者不是成员 → ErrTeamNotFound。
func lockTeamForUpdate(ctx context.Context, tx pgx.Tx, teamID, actorID int64) (string, error) {
	var one int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM teams WHERE id = $1 FOR UPDATE`, teamID).Scan(&one); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrTeamNotFound
		}
		return "", err
	}
	return memberRole(ctx, tx, teamID, actorID)
}

func memberRole(ctx context.Context, tx pgx.Tx, teamID, userID int64) (string, error) {
	var role string
	err := tx.QueryRow(ctx, `SELECT role FROM team_members WHERE team_id = $1 AND user_id = $2`,
		teamID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrTeamNotFound
	}
	return role, err
}

func validTeamName(name string) error {
	n := utf8.RuneCountInString(name)
	if n < 1 || n > teamNameMaxRunes {
		return ErrInvalidTeamName
	}
	return nil
}

// ---------------------------------------------------------------- 读可见性

// RepoAccess 是读可见性的唯一判定入口：viewer 能不能看见 repo，以什么关系。
//
//  1. repos.user_id = viewer → owner；
//  2. 否则 repos.team_id 非空且 viewer 是该团队成员 → team_<role>；
//  3. 否则 ErrRepoNotFound（与「不存在」不可区分）。
//
// 只回答「能不能看」，回答不了「能不能写」—— 写一律要求 role == owner，且写
// 路径继续用带 user_id 条件的原 SQL（UpdateRepo / GetRepo），不经过这里放宽。
// 不加任何缓存：每次请求现查，移除成员立刻生效。
func (s *Store) RepoAccess(ctx context.Context, repoID, viewerID int64) (RepoRole, error) {
	_, role, err := s.repoAccessWithOwner(ctx, repoID, viewerID)
	return role, err
}

// repoAccessWithOwner 同 RepoAccess，另返回属主 ID（只在 store 内部使用）。
func (s *Store) repoAccessWithOwner(ctx context.Context, repoID, viewerID int64) (int64, RepoRole, error) {
	var ownerID int64
	var teamRole *string
	err := s.pool.QueryRow(ctx, `
		SELECT r.user_id, m.role
		FROM repos r
		LEFT JOIN team_members m ON m.team_id = r.team_id AND m.user_id = $2
		WHERE r.id = $1 AND (r.user_id = $2 OR m.user_id IS NOT NULL)`, repoID, viewerID,
	).Scan(&ownerID, &teamRole)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", ErrRepoNotFound
	}
	if err != nil {
		return 0, "", fmt.Errorf("store: 判定仓库 %d 可见性失败: %w", repoID, err)
	}
	if ownerID == viewerID {
		return ownerID, RepoRoleOwner, nil
	}
	return ownerID, RepoRole("team_" + *teamRole), nil
}

// TaskDetailForViewer 读任务详情，可见性 = 属主 ∪ 任务所属仓库共享到的团队的成员。
//
// 不自带列清单：可见性判定通过后直接调属主版 TaskDetail(ctx, id, 属主)，所以
// 谁给 TaskDetail 加了列，这里自动跟上。另回填 RepoID 与 ViewerRole。
// 只给任务详情页用；预览、IssueContextFor 等「在别人机器上跑东西 / 用属主凭据」
// 的路径刻意继续用 TaskDetail（只认属主）。
func (s *Store) TaskDetailForViewer(ctx context.Context, id, viewerID int64) (*TaskDetail, error) {
	var ownerID, repoID int64
	err := s.pool.QueryRow(ctx, `SELECT user_id, repo_id FROM tasks WHERE id = $1`, id).Scan(&ownerID, &repoID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTaskNotVisible
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读取任务 %d 失败: %w", id, err)
	}
	role := RepoRoleOwner
	if ownerID != viewerID {
		if role, err = s.RepoAccess(ctx, repoID, viewerID); err != nil {
			return nil, ErrTaskNotVisible
		}
	}
	detail, err := s.TaskDetail(ctx, id, ownerID)
	if err != nil {
		return nil, err
	}
	detail.Task.RepoID = repoID
	detail.ViewerRole = role
	return detail, nil
}

// ListReposForViewer 返回我的仓库 + 我所在团队共享给我的仓库（GET /api/repos?scope=team）。
//
// 不自带列清单，而且【只从 ListRepos 取行】：属主行与团队行出自同一个函数、同一组列，
// 邻线（f06/f07）往 ListRepos 加列后团队视图自动跟上。刻意不用 GetRepo —— 它的
// SELECT 没有 verify_infra（存量现象），团队成员会在只读配置里看到「无隔离栈」。
// 代价：团队臂是每个属主一次 ListRepos；共享仓库的属主数是个位数，可以接受。
func (s *Store) ListReposForViewer(ctx context.Context, viewerID int64) ([]ViewerRepo, error) {
	mine, err := s.ListRepos(ctx, viewerID)
	if err != nil {
		return nil, err
	}

	// 自己的仓库里哪些共享到了团队
	sharedMine := map[int64]int64{}
	rows, err := s.pool.Query(ctx,
		`SELECT id, team_id FROM repos WHERE user_id = $1 AND team_id IS NOT NULL`, viewerID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询我的共享仓库失败: %w", err)
	}
	for rows.Next() {
		var id, tid int64
		if err := rows.Scan(&id, &tid); err != nil {
			rows.Close()
			return nil, err
		}
		sharedMine[id] = tid
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]ViewerRepo, 0, len(mine))
	for _, r := range mine {
		vr := ViewerRepo{RepoRow: r, Owned: true}
		if tid, ok := sharedMine[r.ID]; ok {
			tid := tid
			vr.TeamID = &tid
		}
		out = append(out, vr)
	}

	// 团队臂：别人共享到我所在团队的仓库，按属主分组
	rows, err = s.pool.Query(ctx, `
		SELECT r.id, r.user_id, r.team_id FROM repos r
		JOIN team_members m ON m.team_id = r.team_id AND m.user_id = $1
		WHERE r.user_id <> $1
		ORDER BY r.user_id, r.id`, viewerID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询团队共享仓库失败: %w", err)
	}
	type shared struct{ id, teamID int64 }
	byOwner := map[int64][]shared{}
	var owners []int64
	for rows.Next() {
		var id, owner, tid int64
		if err := rows.Scan(&id, &owner, &tid); err != nil {
			rows.Close()
			return nil, err
		}
		if _, seen := byOwner[owner]; !seen {
			owners = append(owners, owner)
		}
		byOwner[owner] = append(byOwner[owner], shared{id, tid})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, owner := range owners {
		repos, err := s.ListRepos(ctx, owner)
		if err != nil {
			return nil, err
		}
		byID := make(map[int64]RepoRow, len(repos))
		for _, r := range repos {
			byID[r.ID] = r
		}
		for _, sh := range byOwner[owner] {
			r, ok := byID[sh.id]
			if !ok {
				continue // 两次查询之间被删：跳过
			}
			tid := sh.teamID
			out = append(out, ViewerRepo{RepoRow: r, Owned: false, TeamID: &tid})
		}
	}
	return out, nil
}

// GetRepoForViewer 读单个仓库的配置（只读视图）：RepoAccess 通过后按属主调
// ListRepos 取出 ID 相同的那一行（理由同 ListReposForViewer）。不含任何凭据。
// 写路径（PUT /api/repos/{id}、共享）都不用它。
func (s *Store) GetRepoForViewer(ctx context.Context, repoID, viewerID int64) (*ViewerRepo, error) {
	ownerID, _, err := s.repoAccessWithOwner(ctx, repoID, viewerID)
	if err != nil {
		return nil, err
	}
	repos, err := s.ListRepos(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	var teamID *int64
	if err := s.pool.QueryRow(ctx, `SELECT team_id FROM repos WHERE id = $1`, repoID).Scan(&teamID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRepoNotFound
		}
		return nil, err
	}
	for _, r := range repos {
		if r.ID == repoID {
			return &ViewerRepo{RepoRow: r, Owned: ownerID == viewerID, TeamID: teamID}, nil
		}
	}
	return nil, ErrRepoNotFound
}

// ---------------------------------------------------------------- 团队 CRUD

// CreateTeam 建团队，创建者自动成为 owner。名字不要求全站唯一（团队用 id 区分）。
func (s *Store) CreateTeam(ctx context.Context, ownerID int64, name string) (*Team, error) {
	if err := validTeamName(name); err != nil {
		return nil, err
	}
	var t Team
	err := s.teamTx(ctx, "create_team", func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO teams (name) VALUES ($1) RETURNING id, name, created_at`,
			name).Scan(&t.ID, &t.Name, &t.CreatedAt); err != nil {
			return fmt.Errorf("store: 建团队失败: %w", err)
		}
		_, err := tx.Exec(ctx, `INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, 'owner')`,
			t.ID, ownerID)
		return err
	})
	if err != nil {
		return nil, err
	}
	slog.Info("team", "action", "create", "team_id", t.ID, "actor_id", ownerID)
	return &t, nil
}

// ListMyTeams 返回我加入的团队（含我的角色、成员数、共享仓库数）。
func (s *Store) ListMyTeams(ctx context.Context, userID int64) ([]TeamSummary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.name, t.created_at, me.role,
		       (SELECT count(*) FROM team_members x WHERE x.team_id = t.id),
		       (SELECT count(*) FROM repos r WHERE r.team_id = t.id)
		FROM team_members me JOIN teams t ON t.id = me.team_id
		WHERE me.user_id = $1
		ORDER BY t.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询我的团队失败: %w", err)
	}
	defer rows.Close()
	out := []TeamSummary{}
	for rows.Next() {
		var ts TeamSummary
		if err := rows.Scan(&ts.ID, &ts.Name, &ts.CreatedAt, &ts.Role, &ts.MemberCount, &ts.SharedRepos); err != nil {
			return nil, err
		}
		out = append(out, ts)
	}
	return out, rows.Err()
}

// GetTeam 读团队详情与成员表。非成员 → ErrTeamNotFound（不泄露团队名）。
func (s *Store) GetTeam(ctx context.Context, teamID, viewerID int64) (*TeamDetail, error) {
	d := &TeamDetail{Members: []TeamMember{}}
	err := s.pool.QueryRow(ctx, `
		SELECT t.id, t.name, t.created_at, me.role
		FROM teams t JOIN team_members me ON me.team_id = t.id AND me.user_id = $2
		WHERE t.id = $1`, teamID, viewerID).Scan(&d.ID, &d.Name, &d.CreatedAt, &d.MyRole)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTeamNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读取团队失败: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT m.user_id, u.email, m.role, m.created_at
		FROM team_members m JOIN users u ON u.id = m.user_id
		WHERE m.team_id = $1
		ORDER BY CASE m.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, m.created_at, m.user_id`, teamID)
	if err != nil {
		return nil, fmt.Errorf("store: 读取成员失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m TeamMember
		if err := rows.Scan(&m.UserID, &m.Email, &m.Role, &m.JoinedAt); err != nil {
			return nil, err
		}
		d.Members = append(d.Members, m)
	}
	return d, rows.Err()
}

// RenameTeam 改团队名。只有 owner；其余（含非成员）→ ErrTeamNotFound。
func (s *Store) RenameTeam(ctx context.Context, teamID, actorID int64, name string) error {
	err := s.teamTx(ctx, "rename_team", func(tx pgx.Tx) error {
		role, err := lockTeamForUpdate(ctx, tx, teamID, actorID)
		lockStep("rename_team")
		if err != nil {
			return err
		}
		if role != TeamRoleOwner {
			return ErrTeamNotFound
		}
		if err := validTeamName(name); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE teams SET name = $2 WHERE id = $1`, teamID, name)
		return err
	})
	if err == nil {
		slog.Info("team", "action", "rename", "team_id", teamID, "actor_id", actorID)
	}
	return err
}

// DeleteTeam 删团队。只有 owner；还有共享仓库 → ErrTeamHasSharedRepos（不让
// ON DELETE SET NULL 在无人知情时把一批仓库变私有）。
func (s *Store) DeleteTeam(ctx context.Context, teamID, actorID int64) error {
	err := s.teamTx(ctx, "delete_team", func(tx pgx.Tx) error {
		role, err := lockTeamForUpdate(ctx, tx, teamID, actorID)
		lockStep("delete_team")
		if err != nil {
			return err
		}
		if role != TeamRoleOwner {
			return ErrTeamNotFound
		}
		var shared int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM repos WHERE team_id = $1`, teamID).Scan(&shared); err != nil {
			return err
		}
		if shared > 0 {
			return ErrTeamHasSharedRepos
		}
		_, err = tx.Exec(ctx, `DELETE FROM teams WHERE id = $1`, teamID)
		return err
	})
	if err == nil {
		slog.Info("team", "action", "delete", "team_id", teamID, "actor_id", actorID)
	}
	return err
}

// ---------------------------------------------------------------- 邀请码

// hashInviteCode 是邀请码在库里的形态：sha256(明文码)。
func hashInviteCode(code string) []byte {
	h := sha256.Sum256([]byte(code))
	return h[:]
}

// CreateInvitation 生成一次性邀请码（owner / admin）。
//
// 128 位随机码（crypto/rand），明文只在返回值里出现一次，库里只存 sha256 与末 4 位。
// 【不碰 users 表】：inviteeEmail 只是给 owner 看的备注，不参与任何判定，所以
// 「给某人发邀请成功 = 此人已注册」这个探针不存在。
func (s *Store) CreateInvitation(ctx context.Context, teamID, actorID int64, role, inviteeEmail string) (*Invitation, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("store: 生成邀请码失败: %w", err)
	}
	code := hex.EncodeToString(raw)
	var email *string
	if inviteeEmail != "" {
		email = &inviteeEmail
	}
	inv := &Invitation{Role: role, Hint: code[len(code)-4:], InviteeEmail: email, Code: code}
	err := s.teamTx(ctx, "create_invitation", func(tx pgx.Tx) error {
		myRole, err := lockTeamForUpdate(ctx, tx, teamID, actorID)
		lockStep("create_invitation")
		if err != nil {
			return err
		}
		if myRole != TeamRoleOwner && myRole != TeamRoleAdmin {
			return ErrTeamNotFound
		}
		if role != TeamRoleAdmin && role != TeamRoleMember {
			return ErrInvalidTeamRole
		}
		return tx.QueryRow(ctx, `
			INSERT INTO team_invitations (team_id, code_hash, code_hint, invitee_email, role, invited_by, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, now() + $7::interval)
			RETURNING id, invited_by, expires_at`,
			teamID, hashInviteCode(code), inv.Hint, email, role, actorID,
			fmt.Sprintf("%d seconds", int(InviteTTL/time.Second)),
		).Scan(&inv.ID, &inv.CreatedBy, &inv.ExpiresAt)
	})
	if err != nil {
		return nil, err
	}
	slog.Info("team", "action", "invite", "team_id", teamID, "actor_id", actorID, "role", role)
	return inv, nil
}

// ListInvitations 列 pending 且未过期的邀请（owner / admin）。不含明文码。
func (s *Store) ListInvitations(ctx context.Context, teamID, actorID int64) ([]Invitation, error) {
	var role string
	err := s.pool.QueryRow(ctx, `SELECT role FROM team_members WHERE team_id = $1 AND user_id = $2`,
		teamID, actorID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && role != TeamRoleOwner && role != TeamRoleAdmin) {
		return nil, ErrTeamNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, role, code_hint, invitee_email, invited_by, expires_at
		FROM team_invitations
		WHERE team_id = $1 AND state = 'pending' AND expires_at > now()
		ORDER BY id`, teamID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询邀请失败: %w", err)
	}
	defer rows.Close()
	out := []Invitation{}
	for rows.Next() {
		var inv Invitation
		if err := rows.Scan(&inv.ID, &inv.Role, &inv.Hint, &inv.InviteeEmail, &inv.CreatedBy, &inv.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// RevokeInvitation 撤销一条 pending 邀请（owner / admin）。
func (s *Store) RevokeInvitation(ctx context.Context, teamID, invitationID, actorID int64) error {
	err := s.teamTx(ctx, "revoke", func(tx pgx.Tx) error {
		role, err := lockTeamForUpdate(ctx, tx, teamID, actorID)
		lockStep("revoke")
		if err != nil {
			return err
		}
		if role != TeamRoleOwner && role != TeamRoleAdmin {
			return ErrTeamNotFound
		}
		tag, err := tx.Exec(ctx, `
			UPDATE team_invitations SET state = 'revoked', decided_at = now()
			WHERE id = $1 AND team_id = $2 AND state = 'pending'`, invitationID, teamID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrInvitationNotFound
		}
		return nil
	})
	if err == nil {
		slog.Info("team", "action", "revoke_invite", "team_id", teamID, "actor_id", actorID, "target_id", invitationID)
	}
	return err
}

// AcceptInvitation 用一次性码加入团队，返回团队 id。
//
// 锁序：无锁读出 team_id → teams FOR KEY SHARE → 条件 UPDATE 邀请（单次生效）→
// INSERT 成员。码无效/已用/已撤销/过期，或接受者已是成员（此时码不被消耗）→
// 同一个 ErrInviteInvalid。
func (s *Store) AcceptInvitation(ctx context.Context, code string, userID int64) (int64, error) {
	if code == "" {
		return 0, ErrInviteInvalid
	}
	h := hashInviteCode(code)
	var teamID int64
	err := s.pool.QueryRow(ctx, `SELECT team_id FROM team_invitations WHERE code_hash = $1`, h).Scan(&teamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrInviteInvalid
	}
	if err != nil {
		return 0, err
	}
	var role string
	err = s.teamTx(ctx, "accept", func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx, `SELECT 1 FROM teams WHERE id = $1 FOR KEY SHARE`, teamID).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrInviteInvalid
			}
			return err
		}
		lockStep("accept")
		err := tx.QueryRow(ctx, `
			UPDATE team_invitations SET state = 'accepted', accepted_by = $2, decided_at = now()
			WHERE code_hash = $1 AND state = 'pending' AND expires_at > now()
			RETURNING role`, h, userID).Scan(&role)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInviteInvalid
		}
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO team_members (team_id, user_id, role) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, teamID, userID, role)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// 已是成员：回滚整个事务，码回到 pending（未被消耗）
			return ErrInviteInvalid
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	slog.Info("team", "action", "accept_invite", "team_id", teamID, "actor_id", userID, "role", role)
	return teamID, nil
}

// ---------------------------------------------------------------- 成员管理

// SetMemberRole 在 admin ↔ member 之间改角色。只有 owner；目标是 owner 本人
// → ErrTeamNeedsOwner；role 不是 admin/member → ErrInvalidTeamRole（授权之后校验）。
func (s *Store) SetMemberRole(ctx context.Context, teamID, actorID, targetID int64, role string) error {
	err := s.teamTx(ctx, "set_role", func(tx pgx.Tx) error {
		myRole, err := lockTeamForUpdate(ctx, tx, teamID, actorID)
		lockStep("set_role")
		if err != nil {
			return err
		}
		if myRole != TeamRoleOwner {
			return ErrTeamNotFound
		}
		if role != TeamRoleAdmin && role != TeamRoleMember {
			return ErrInvalidTeamRole
		}
		if targetID == actorID {
			return ErrTeamNeedsOwner
		}
		tag, err := tx.Exec(ctx, `
			UPDATE team_members SET role = $3 WHERE team_id = $1 AND user_id = $2 AND role <> 'owner'`,
			teamID, targetID, role)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrTeamNotFound
		}
		return nil
	})
	if err == nil {
		slog.Info("team", "action", "set_role", "team_id", teamID, "actor_id", actorID, "target_id", targetID, "role", role)
	}
	return err
}

// RemoveMember 移除成员或自己退出，返回被收回共享的仓库数。
//
// 授权（§3.6 逐格判定表）：目标 = 自己时任一成员可退出（owner → ErrTeamNeedsOwner）；
// 否则 owner 可移 admin/member，admin 只可移 member，其余 → ErrTeamNotFound。
//
// 同一事务内把离队者名下共享到本团队的仓库全部回私有，锁序
// teams FOR UPDATE → UPDATE repos → DELETE team_members。
func (s *Store) RemoveMember(ctx context.Context, teamID, actorID, targetID int64) (int64, error) {
	var unshared int64
	err := s.teamTx(ctx, "remove", func(tx pgx.Tx) error {
		myRole, err := lockTeamForUpdate(ctx, tx, teamID, actorID)
		lockStep("remove")
		if err != nil {
			return err
		}
		if targetID == actorID {
			if myRole == TeamRoleOwner {
				return ErrTeamNeedsOwner
			}
		} else {
			targetRole, err := memberRole(ctx, tx, teamID, targetID)
			if err != nil {
				return err
			}
			allowed := (myRole == TeamRoleOwner && targetRole != TeamRoleOwner) ||
				(myRole == TeamRoleAdmin && targetRole == TeamRoleMember)
			if !allowed {
				return ErrTeamNotFound
			}
		}
		tag, err := tx.Exec(ctx, `UPDATE repos SET team_id = NULL WHERE team_id = $1 AND user_id = $2`, teamID, targetID)
		if err != nil {
			return err
		}
		unshared = tag.RowsAffected()
		_, err = tx.Exec(ctx, `DELETE FROM team_members WHERE team_id = $1 AND user_id = $2`, teamID, targetID)
		return err
	})
	if err != nil {
		return 0, err
	}
	slog.Info("team", "action", "member_remove", "team_id", teamID, "actor_id", actorID,
		"target_id", targetID, "unshared", unshared)
	return unshared, nil
}

// TransferOwner 把 owner 转给本团队另一成员：旧 owner 降为 admin、新成员升为 owner，
// 单事务。只有 owner；目标不是本团队成员 → ErrTeamNotFound。两个并发转让由
// teams FOR UPDATE 串行化，后到者在锁内重读发现自己已不是 owner → ErrTeamNotFound。
func (s *Store) TransferOwner(ctx context.Context, teamID, actorID, targetID int64) error {
	err := s.teamTx(ctx, "transfer", func(tx pgx.Tx) error {
		myRole, err := lockTeamForUpdate(ctx, tx, teamID, actorID)
		lockStep("transfer")
		if err != nil {
			return err
		}
		if myRole != TeamRoleOwner || targetID == actorID {
			return ErrTeamNotFound
		}
		if _, err := memberRole(ctx, tx, teamID, targetID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE team_members SET role = 'admin' WHERE team_id = $1 AND user_id = $2`,
			teamID, actorID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE team_members SET role = 'owner' WHERE team_id = $1 AND user_id = $2`,
			teamID, targetID)
		return err
	})
	if err == nil {
		slog.Info("team", "action", "transfer", "team_id", teamID, "actor_id", actorID, "target_id", targetID)
	}
	return err
}

// ---------------------------------------------------------------- 仓库共享

// ShareRepo 把仓库共享到团队（teamID 非 nil）或取消共享（nil）。只有仓库属主。
//
//   - 非属主 → ErrRepoNotFound（与今天 PUT /api/repos/{id} 同一形态）；
//   - 取消共享：只写 repos，不要求仍是团队成员（数据所有者永远能收回）；
//   - 共享：调用者须是目标团队的成员（任一角色）。团队不存在与「不是成员」
//     走同一个分支，都是 ErrTeamNotFound，teams.id 因此不是探针。
//     锁序 teams FOR KEY SHARE → repos FOR UPDATE → team_members FOR SHARE → UPDATE。
//     3a 显式拿的那把锁就是随后外键检查本来要拿的，提前拿掉倒序。
func (s *Store) ShareRepo(ctx context.Context, repoID, ownerID int64, teamID *int64) error {
	// 无锁读，只为尽早给出「仓库不存在」
	if _, err := s.GetRepo(ctx, repoID, ownerID); err != nil {
		return ErrRepoNotFound
	}
	if teamID == nil {
		tag, err := s.pool.Exec(ctx, `UPDATE repos SET team_id = NULL WHERE id = $1 AND user_id = $2`, repoID, ownerID)
		if err != nil {
			return fmt.Errorf("store: 取消共享失败: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrRepoNotFound
		}
		slog.Info("team", "action", "unshare", "actor_id", ownerID, "target_id", repoID)
		return nil
	}
	tid := *teamID
	err := s.teamTx(ctx, "share", func(tx pgx.Tx) error {
		var one int
		err := tx.QueryRow(ctx, `SELECT 1 FROM teams WHERE id = $1 FOR KEY SHARE`, tid).Scan(&one)
		lockStep("share")
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTeamNotFound
		}
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT 1 FROM repos WHERE id = $1 AND user_id = $2 FOR UPDATE`, repoID, ownerID).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRepoNotFound
		}
		if err != nil {
			return err
		}
		var role string
		err = tx.QueryRow(ctx, `SELECT role FROM team_members WHERE team_id = $1 AND user_id = $2 FOR SHARE`,
			tid, ownerID).Scan(&role)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTeamNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE repos SET team_id = $3 WHERE id = $1 AND user_id = $2`, repoID, ownerID, tid)
		return err
	})
	if err == nil {
		slog.Info("team", "action", "share", "team_id", tid, "actor_id", ownerID, "target_id", repoID)
	}
	return err
}

// ---------------------------------------------------------------- 管理员删账号

// DeleteUserGuarded 是管理员删账号的团队守卫版（AdminAPI.remove 专用；
// Users.Delete 原样保留给其他调用点）。单事务：
//
//  1. 按 id 序锁住此人所在的全部团队（FOR UPDATE，遵守全局锁序）；
//  2. 此人是 owner 且团队还有别的成员 → *UserOwnsTeamError（整个事务回滚）；
//  3. 此人是团队唯一成员 → 连团队一起删（共享到它的只可能是他自己的仓库，随账号级联）；
//  4. DELETE FROM users（成员关系、仓库、任务随 ON DELETE CASCADE 消失）。
func (s *Store) DeleteUserGuarded(ctx context.Context, userID int64) error {
	err := s.teamTx(ctx, "delete_user", func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, name FROM teams
			WHERE id IN (SELECT team_id FROM team_members WHERE user_id = $1)
			ORDER BY id FOR UPDATE`, userID)
		if err != nil {
			return err
		}
		type tm struct {
			id   int64
			name string
		}
		var teams []tm
		for rows.Next() {
			var t tm
			if err := rows.Scan(&t.id, &t.name); err != nil {
				rows.Close()
				return err
			}
			teams = append(teams, t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		lockStep("delete_user")
		for _, t := range teams {
			var role string
			var others int
			if err := tx.QueryRow(ctx, `
				SELECT me.role, (SELECT count(*) FROM team_members x WHERE x.team_id = $1 AND x.user_id <> $2)
				FROM team_members me WHERE me.team_id = $1 AND me.user_id = $2`, t.id, userID).Scan(&role, &others); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					continue // 等锁期间已被移出该团队：与此团队无关了
				}
				return err
			}
			switch {
			case others == 0:
				if _, err := tx.Exec(ctx, `DELETE FROM teams WHERE id = $1`, t.id); err != nil {
					return err
				}
			case role == TeamRoleOwner:
				return &UserOwnsTeamError{TeamID: t.id, TeamName: t.name}
			}
		}
		tag, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
		if err != nil {
			return fmt.Errorf("store: 删除用户失败: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrUserNotFound
		}
		return nil
	})
	return err
}
