package store

// 团队与角色（f12，docs/features/team-roles.md）—— 红阶段空壳。
//
// 本文件此版本只提供签名与「沿用今天行为」的空壳实现，供先红后绿的第一个
// 提交编译通过；第二个提交填实现。

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

// 团队内角色。与 users.role（站内 admin/member）正交，不复用、不合并。
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

// InviteTTL 是邀请码有效期。业务规则而非部署差异，因此是常量不是环境变量。
const InviteTTL = 48 * time.Hour

var (
	// ErrTeamNotFound：团队不存在、不是成员、或无权执行该动作 —— 三者刻意不分。
	ErrTeamNotFound = errors.New("store: 团队不存在")
	// ErrInviteInvalid：邀请码无效、已用、已撤销、已过期或接受者已是成员 —— 不分。
	ErrInviteInvalid = errors.New("store: 邀请码无效")
	// ErrInvitationNotFound：已授权的管理者撤销一条不存在的邀请。
	ErrInvitationNotFound = errors.New("store: 邀请不存在")
	// ErrTeamNeedsOwner：owner 不能降级/退出自己（每个团队恰好一个 owner）。
	ErrTeamNeedsOwner = errors.New("store: 团队至少需要一个 owner，先转让")
	// ErrTeamHasSharedRepos：删团队前必须先无共享仓库。
	ErrTeamHasSharedRepos = errors.New("store: 团队还有共享仓库，先取消共享")
	// ErrInvalidTeamRole：角色取值非法（PATCH 不接受 owner）。
	ErrInvalidTeamRole = errors.New("store: 角色须为 admin 或 member")
	// ErrInvalidTeamName：团队名须 1–64 字符。
	ErrInvalidTeamName = errors.New("store: 团队名须为 1–64 个字符")
	// ErrTeamBusy：团队写事务在锁冲突上重试耗尽，映射为 503。
	ErrTeamBusy = errors.New("store: 团队数据正忙，请重试")
	// ErrTaskNotVisible：任务不存在或当前用户不可见（两者不分）。
	ErrTaskNotVisible = errors.New("store: 任务不存在")
)

// UserOwnsTeamError：要删的账号是某个仍有其他成员的团队的 owner。
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

// TeamMember 是团队成员表的一行（邮箱只对同团队成员可见）。
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

// Invitation 是一条邀请。Code 只在创建时填充（明文只出现这一次）。
type Invitation struct {
	ID           int64     `json:"id"`
	Role         string    `json:"role"`
	Hint         string    `json:"hint"`
	InviteeEmail *string   `json:"inviteeEmail,omitempty"`
	CreatedBy    *int64    `json:"createdBy"`
	ExpiresAt    time.Time `json:"expiresAt"`
	Code         string    `json:"code,omitempty"`
}

// ViewerRepo 是团队视图里的一行仓库：内嵌 RepoRow（JSON 平铺），
// 加上「是不是我的」与「共享到哪个团队」。属主 ID 不出现在返回值里。
type ViewerRepo struct {
	RepoRow
	Owned  bool   `json:"owned"`
	TeamID *int64 `json:"teamId"`
}

var teamTxRetries atomic.Int64

// TeamTxRetries 返回团队写事务因 40P01/40001 重试的累计次数（只读，供测试断言）。
func TeamTxRetries() int64 { return teamTxRetries.Load() }

// teamTx 是团队写事务的唯一入口（空壳：只跑一次，不重试）。
func (s *Store) teamTx(ctx context.Context, op string, fn func(pgx.Tx) error) error {
	_ = op
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RepoAccess 回答 viewer 能不能看见 repo（空壳：只认属主）。
func (s *Store) RepoAccess(ctx context.Context, repoID, viewerID int64) (RepoRole, error) {
	if _, err := s.GetRepo(ctx, repoID, viewerID); err != nil {
		return "", ErrRepoNotFound
	}
	return RepoRoleOwner, nil
}

// TaskDetailForViewer（空壳：等价属主版）。
func (s *Store) TaskDetailForViewer(ctx context.Context, id, viewerID int64) (*TaskDetail, error) {
	return s.TaskDetail(ctx, id, viewerID)
}

// ListReposForViewer（空壳：只返回自己的）。
func (s *Store) ListReposForViewer(ctx context.Context, viewerID int64) ([]ViewerRepo, error) {
	mine, err := s.ListRepos(ctx, viewerID)
	if err != nil {
		return nil, err
	}
	out := make([]ViewerRepo, 0, len(mine))
	for _, r := range mine {
		out = append(out, ViewerRepo{RepoRow: r, Owned: true})
	}
	return out, nil
}

// GetRepoForViewer（空壳：只认属主）。
func (s *Store) GetRepoForViewer(ctx context.Context, repoID, viewerID int64) (*ViewerRepo, error) {
	return nil, ErrRepoNotFound
}

// CreateTeam（空壳）。
func (s *Store) CreateTeam(ctx context.Context, ownerID int64, name string) (*Team, error) {
	return nil, ErrTeamNotFound
}

// ListMyTeams（空壳）。
func (s *Store) ListMyTeams(ctx context.Context, userID int64) ([]TeamSummary, error) {
	return []TeamSummary{}, nil
}

// GetTeam（空壳）。
func (s *Store) GetTeam(ctx context.Context, teamID, viewerID int64) (*TeamDetail, error) {
	return nil, ErrTeamNotFound
}

// RenameTeam（空壳）。
func (s *Store) RenameTeam(ctx context.Context, teamID, actorID int64, name string) error {
	return ErrTeamNotFound
}

// DeleteTeam（空壳）。
func (s *Store) DeleteTeam(ctx context.Context, teamID, actorID int64) error {
	return ErrTeamNotFound
}

// CreateInvitation（空壳）。
func (s *Store) CreateInvitation(ctx context.Context, teamID, actorID int64, role, inviteeEmail string) (*Invitation, error) {
	return nil, ErrTeamNotFound
}

// ListInvitations（空壳）。
func (s *Store) ListInvitations(ctx context.Context, teamID, actorID int64) ([]Invitation, error) {
	return nil, ErrTeamNotFound
}

// RevokeInvitation（空壳）。
func (s *Store) RevokeInvitation(ctx context.Context, teamID, invitationID, actorID int64) error {
	return ErrTeamNotFound
}

// AcceptInvitation（空壳）。
func (s *Store) AcceptInvitation(ctx context.Context, code string, userID int64) (int64, error) {
	return 0, ErrInviteInvalid
}

// SetMemberRole（空壳）。
func (s *Store) SetMemberRole(ctx context.Context, teamID, actorID, targetID int64, role string) error {
	return ErrTeamNotFound
}

// RemoveMember（空壳）。
func (s *Store) RemoveMember(ctx context.Context, teamID, actorID, targetID int64) (int64, error) {
	return 0, ErrTeamNotFound
}

// TransferOwner（空壳）。
func (s *Store) TransferOwner(ctx context.Context, teamID, actorID, targetID int64) error {
	return ErrTeamNotFound
}

// ShareRepo（空壳：一律仓库不存在）。
func (s *Store) ShareRepo(ctx context.Context, repoID, ownerID int64, teamID *int64) error {
	return ErrRepoNotFound
}

// DeleteUserGuarded（空壳：不做团队检查，直接删）。
func (s *Store) DeleteUserGuarded(ctx context.Context, userID int64) error {
	return s.NewUsers().Delete(ctx, userID)
}

// teamLockHook 是测试钩子：团队写事务在拿到首把锁后调用它（nil 时不调用）。
// AC-49 用它让 tx1 停在「持有第一把锁」处，构造确定性交错。生产永远为 nil。
var teamLockHook atomic.Pointer[func(op string)]

func lockStep(op string) {
	if h := teamLockHook.Load(); h != nil {
		(*h)(op)
	}
}

var _ = lockStep

// hashInviteCode 是邀请码在库里的形态：sha256(明文码)（空壳）。
func hashInviteCode(code string) []byte { return []byte(code) }
