package store

// teamskills.go 团队自定义技能（f11，docs/features/team-skills.md）的数据访问。
//
// 隔离边界是 user_id：每条面向 API 的查询都带 `user_id = $N` 谓词，不属于当前
// 用户与不存在同等处理（返回 ErrTeamSkillNotFound / ErrRepoNotFound），由
// httpapi 统一映射成 404。
//
// 版本行不可变：只 INSERT，永不 UPDATE/DELETE。「内容没变不产生新版本」的
// sha 比较与「下一个版本号」都在同一把行锁（SELECT … FOR UPDATE）里做，
// 并发提交同内容也只产生一个新版本（PRD §3.9）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zichuanwangcloud-gif/lathe/internal/teamskills"
)

var (
	// ErrTeamSkillNotFound 表示技能不存在、已归档（写路径）或不属于当前用户（刻意不分）。
	ErrTeamSkillNotFound = errors.New("store: 团队技能不存在")
	// ErrTeamSkillVersionNotFound 表示指定版本不存在。
	ErrTeamSkillVersionNotFound = errors.New("store: 团队技能版本不存在")
	// ErrTeamSkillSlugTaken 表示同仓库已有同名在役技能（库层 23505 的映射）。
	ErrTeamSkillSlugTaken = errors.New("store: 同仓库已有同名在役技能")
	// ErrTeamSkillLimit 表示仓库在役技能数已达上限。
	ErrTeamSkillLimit = errors.New("store: 仓库在役技能数已达上限")
	// ErrTaskNotFound 表示任务不存在或不属于当前用户。
	ErrTaskNotFound = errors.New("store: 任务不存在")
)

// pinnedCountWindow 是列表里 pinnedTaskCount 的统计窗口。常量不做配置：唯一
// 消费方就是 ListTeamSkills 这条查询（PRD §3.8），以 $3 传进 SQL。
const pinnedCountWindow = 30 * 24 * time.Hour

// TeamSkill 是列表与详情里的技能元数据（不含文件内容）。
type TeamSkill struct {
	ID         int64      `json:"id"`
	RepoID     int64      `json:"repoId"`
	Slug       string     `json:"slug"`
	Title      string     `json:"title"`
	Enabled    bool       `json:"enabled"`
	ArchivedAt *time.Time `json:"archivedAt"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	// LatestVersion 是最新版本号（新建技能即有 v1）。
	LatestVersion int `json:"latestVersion"`
	// PinnedTaskCount 是同一属主、同一仓库近 30 天内钉住过它的任务数（只在列表里填）。
	PinnedTaskCount int `json:"pinnedTaskCount"`
}

// TeamSkillVersion 是一个不可变的版本。列表里 Files 为空（只给元数据）。
type TeamSkillVersion struct {
	Version       int               `json:"version"`
	ContentSHA256 string            `json:"contentSha256"`
	TotalBytes    int               `json:"totalBytes"`
	Note          string            `json:"note"`
	CreatedAt     time.Time         `json:"createdAt"`
	Files         []teamskills.File `json:"files,omitempty"`
}

// TeamSkillPin 是任务钉住的一个技能版本（team_task_skills.pinned 的元素，
// 也是 ListEnabledTeamSkills 的返回形态）。SkillID 必须以 JSON 数字写入：
// 列表计数用 `pinned @> [{"skillId": <bigint>}]` 做部分匹配，类型要一致。
type TeamSkillPin struct {
	SkillID int64 `json:"skillId"`
	// Slug 是物化目录名，即 "team-" + 技能 slug。
	Slug    string `json:"slug"`
	Version int    `json:"version"`
	SHA256  string `json:"sha256"`
}

// NewTeamSkillVersion 是写入一个版本所需的、已经过校验的内容。
type NewTeamSkillVersion struct {
	Files      []teamskills.File
	SHA256     string
	TotalBytes int
	Note       string
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

// ListTeamSkills 列出某仓库的在役技能（含 pinnedTaskCount）。调用方须已确认
// 仓库属于 userID（GetRepo），否则应返回 404 而不是空列表。
//
// pinnedTaskCount 的三条谓词缺一不可（PRD §3.9）：钉行的 user_id/repo_id 与
// 技能一致（防跨租户计数）、只列在役技能、pinned_at 在窗口内。`@>` 对数组里的
// 对象做部分匹配，不必展开；task_id 是主键，每个（技能, 任务）至多命中一行。
func (s *Store) ListTeamSkills(ctx context.Context, userID, repoID int64) ([]TeamSkill, error) {
	rows, err := s.pool.Query(ctx, teamSkillListSQL, userID, repoID, pinnedCountWindow)
	if err != nil {
		return nil, fmt.Errorf("store: 查询团队技能失败: %w", err)
	}
	defer rows.Close()
	out := []TeamSkill{}
	for rows.Next() {
		var t TeamSkill
		if err := rows.Scan(&t.ID, &t.RepoID, &t.Slug, &t.Title, &t.Enabled, &t.ArchivedAt,
			&t.CreatedAt, &t.UpdatedAt, &t.LatestVersion, &t.PinnedTaskCount); err != nil {
			return nil, fmt.Errorf("store: 读取团队技能失败: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// teamSkillListSQL 单独成常量：TestListTeamSkills_PinnedCountUsesOwnerIndex
// 对同一条语句做 EXPLAIN，证明它能走属主索引。
const teamSkillListSQL = `
SELECT s.id, s.repo_id, s.slug, s.title, s.enabled, s.archived_at, s.created_at, s.updated_at,
       COALESCE((SELECT max(v.version) FROM team_skill_versions v WHERE v.skill_id = s.id), 0) AS latest_version,
       count(tts.task_id) AS pinned_task_count
FROM team_skills s
LEFT JOIN team_task_skills tts
       ON tts.user_id   = s.user_id
      AND tts.repo_id   = s.repo_id
      AND tts.pinned_at >= now() - $3::interval
      AND tts.pinned @> jsonb_build_array(jsonb_build_object('skillId', s.id))
WHERE s.user_id = $1 AND s.repo_id = $2 AND s.archived_at IS NULL
GROUP BY s.id
ORDER BY s.slug`

// CreateTeamSkill 新建技能并写入 v1。user_id 与 repo_id 由调用方从已鉴权的
// 请求与路径取得，这里再用 `repos WHERE id AND user_id` 校验一次（防用别人的
// repo_id 挂技能），并锁住仓库行串行化「在役数量上限」检查。
func (s *Store) CreateTeamSkill(ctx context.Context, userID, repoID int64, slug, title string, v NewTeamSkillVersion) (*TeamSkill, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var locked int64
	if err := tx.QueryRow(ctx, `SELECT id FROM repos WHERE id = $1 AND user_id = $2 FOR UPDATE`,
		repoID, userID).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRepoNotFound
		}
		return nil, fmt.Errorf("store: 锁定仓库失败: %w", err)
	}
	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM team_skills WHERE repo_id = $1 AND archived_at IS NULL`,
		repoID).Scan(&active); err != nil {
		return nil, fmt.Errorf("store: 统计在役技能失败: %w", err)
	}
	if active >= teamskills.MaxSkillsPerRepo {
		return nil, ErrTeamSkillLimit
	}

	var t TeamSkill
	err = tx.QueryRow(ctx, `
		INSERT INTO team_skills (user_id, repo_id, slug, title) VALUES ($1, $2, $3, $4)
		RETURNING id, repo_id, slug, title, enabled, archived_at, created_at, updated_at`,
		userID, repoID, slug, title,
	).Scan(&t.ID, &t.RepoID, &t.Slug, &t.Title, &t.Enabled, &t.ArchivedAt, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrTeamSkillSlugTaken
		}
		return nil, fmt.Errorf("store: 新建团队技能失败: %w", err)
	}
	if err := insertTeamSkillVersion(ctx, tx, t.ID, 1, v); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrTeamSkillSlugTaken
		}
		return nil, fmt.Errorf("store: 提交事务失败: %w", err)
	}
	t.LatestVersion = 1
	return &t, nil
}

func insertTeamSkillVersion(ctx context.Context, tx pgx.Tx, skillID int64, version int, v NewTeamSkillVersion) error {
	files, err := json.Marshal(v.Files)
	if err != nil {
		return fmt.Errorf("store: 编码技能文件失败: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO team_skill_versions (skill_id, version, files, content_sha256, total_bytes, note)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		skillID, version, files, v.SHA256, v.TotalBytes, v.Note); err != nil {
		return fmt.Errorf("store: 写入技能版本失败: %w", err)
	}
	return nil
}

// GetTeamSkill 读技能元数据与全部版本的元数据（不含文件内容）。已归档的技能
// 仍可读（审计要能回看）；写路径另行拒绝归档技能。
func (s *Store) GetTeamSkill(ctx context.Context, userID, repoID, skillID int64) (*TeamSkill, []TeamSkillVersion, error) {
	var t TeamSkill
	err := s.pool.QueryRow(ctx, `
		SELECT id, repo_id, slug, title, enabled, archived_at, created_at, updated_at
		FROM team_skills WHERE id = $1 AND repo_id = $2 AND user_id = $3`,
		skillID, repoID, userID,
	).Scan(&t.ID, &t.RepoID, &t.Slug, &t.Title, &t.Enabled, &t.ArchivedAt, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrTeamSkillNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("store: 读取团队技能失败: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT version, content_sha256, total_bytes, note, created_at
		FROM team_skill_versions WHERE skill_id = $1 ORDER BY version DESC`, skillID)
	if err != nil {
		return nil, nil, fmt.Errorf("store: 查询技能版本失败: %w", err)
	}
	defer rows.Close()
	versions := []TeamSkillVersion{}
	for rows.Next() {
		var v TeamSkillVersion
		if err := rows.Scan(&v.Version, &v.ContentSHA256, &v.TotalBytes, &v.Note, &v.CreatedAt); err != nil {
			return nil, nil, fmt.Errorf("store: 读取技能版本失败: %w", err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(versions) > 0 {
		t.LatestVersion = versions[0].Version
	}
	return &t, versions, nil
}

// GetTeamSkillVersion 读某一版的完整内容（属主限定）。
func (s *Store) GetTeamSkillVersion(ctx context.Context, userID, repoID, skillID int64, version int) (*TeamSkillVersion, error) {
	var v TeamSkillVersion
	var files []byte
	err := s.pool.QueryRow(ctx, `
		SELECT v.version, v.content_sha256, v.total_bytes, v.note, v.created_at, v.files
		FROM team_skill_versions v JOIN team_skills s ON s.id = v.skill_id
		WHERE v.skill_id = $1 AND v.version = $2 AND s.repo_id = $3 AND s.user_id = $4`,
		skillID, version, repoID, userID,
	).Scan(&v.Version, &v.ContentSHA256, &v.TotalBytes, &v.Note, &v.CreatedAt, &files)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTeamSkillVersionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读取技能版本失败: %w", err)
	}
	if err := json.Unmarshal(files, &v.Files); err != nil {
		return nil, fmt.Errorf("store: 解析技能版本文件失败: %w", err)
	}
	return &v, nil
}

// AppendTeamSkillVersion 写一个新版本。内容与最新版 content_sha256 相同时不产生
// 新版本，返回 (最新版本号, false)。sha 比较与 max(version)+1 都在技能行的
// FOR UPDATE 锁内：放在锁外会让两个同内容请求都看到「与旧版不同」，产出内容
// 相同的两个新版本（PRD §3.9、AC-8）。
func (s *Store) AppendTeamSkillVersion(ctx context.Context, userID, repoID, skillID int64, v NewTeamSkillVersion) (version int, created bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("store: 开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var locked int64
	if err := tx.QueryRow(ctx, `
		SELECT id FROM team_skills
		WHERE id = $1 AND repo_id = $2 AND user_id = $3 AND archived_at IS NULL
		FOR UPDATE`, skillID, repoID, userID).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, ErrTeamSkillNotFound
		}
		return 0, false, fmt.Errorf("store: 锁定团队技能失败: %w", err)
	}
	var latest int
	var latestSHA string
	err = tx.QueryRow(ctx, `
		SELECT version, content_sha256 FROM team_skill_versions
		WHERE skill_id = $1 ORDER BY version DESC LIMIT 1`, skillID).Scan(&latest, &latestSHA)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, fmt.Errorf("store: 读取最新版本失败: %w", err)
	}
	if latest > 0 && latestSHA == v.SHA256 {
		return latest, false, nil
	}
	if err := insertTeamSkillVersion(ctx, tx, skillID, latest+1, v); err != nil {
		return 0, false, err
	}
	// 父行 updated_at 跟着动：列表按「最近改过」排序/展示时有用，也让触发器有消费方。
	if _, err := tx.Exec(ctx, `UPDATE team_skills SET updated_at = now() WHERE id = $1`, skillID); err != nil {
		return 0, false, fmt.Errorf("store: 更新技能时间失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, false, fmt.Errorf("store: 提交事务失败: %w", err)
	}
	return latest + 1, true, nil
}

// UpdateTeamSkill 改开关或展示名（nil 表示不改）。归档技能不可改。
func (s *Store) UpdateTeamSkill(ctx context.Context, userID, repoID, skillID int64, enabled *bool, title *string) (*TeamSkill, error) {
	var t TeamSkill
	err := s.pool.QueryRow(ctx, `
		UPDATE team_skills SET
		  enabled = COALESCE($4, enabled),
		  title   = COALESCE($5, title)
		WHERE id = $1 AND repo_id = $2 AND user_id = $3 AND archived_at IS NULL
		RETURNING id, repo_id, slug, title, enabled, archived_at, created_at, updated_at,
		  COALESCE((SELECT max(v.version) FROM team_skill_versions v WHERE v.skill_id = team_skills.id), 0)`,
		skillID, repoID, userID, enabled, title,
	).Scan(&t.ID, &t.RepoID, &t.Slug, &t.Title, &t.Enabled, &t.ArchivedAt, &t.CreatedAt, &t.UpdatedAt, &t.LatestVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTeamSkillNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 更新团队技能失败: %w", err)
	}
	return &t, nil
}

// ArchiveTeamSkill 归档技能（不物理删除）：新任务不再钉它，已钉的任务续跑仍能
// 解析到版本行；名字随即可被新技能复用。
func (s *Store) ArchiveTeamSkill(ctx context.Context, userID, repoID, skillID int64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE team_skills SET archived_at = now()
		WHERE id = $1 AND repo_id = $2 AND user_id = $3 AND archived_at IS NULL`,
		skillID, repoID, userID)
	if err != nil {
		return fmt.Errorf("store: 归档团队技能失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTeamSkillNotFound
	}
	return nil
}

// ---------------------------------------------------------------- 流水线侧（runner.TeamSkillSource）

// ListEnabledTeamSkills 返回仓库当前应被新任务钉住的技能及其最新版本。
// 谓词是 enabled AND archived_at IS NULL：停用与归档都排除（AC-9）。
func (s *Store) ListEnabledTeamSkills(ctx context.Context, repoID, userID int64) ([]TeamSkillPin, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.id, s.slug, v.version, v.content_sha256
		FROM team_skills s
		JOIN LATERAL (
		  SELECT version, content_sha256 FROM team_skill_versions
		  WHERE skill_id = s.id ORDER BY version DESC LIMIT 1
		) v ON true
		WHERE s.repo_id = $1 AND s.user_id = $2 AND s.enabled AND s.archived_at IS NULL
		ORDER BY s.slug`, repoID, userID)
	if err != nil {
		return nil, fmt.Errorf("store: 查询启用的团队技能失败: %w", err)
	}
	defer rows.Close()
	out := []TeamSkillPin{}
	for rows.Next() {
		var p TeamSkillPin
		var slug string
		if err := rows.Scan(&p.SkillID, &slug, &p.Version, &p.SHA256); err != nil {
			return nil, fmt.Errorf("store: 读取启用的团队技能失败: %w", err)
		}
		p.Slug = teamskills.DirPrefix + slug
		out = append(out, p)
	}
	return out, rows.Err()
}

// TeamSkillPinsLoad 读任务钉住的技能。ok=false 表示「还没钉」（没有这一行），
// 与 pinned=[]（钉过了，当时没有启用的技能）可分。
func (s *Store) TeamSkillPinsLoad(ctx context.Context, taskID int64) ([]TeamSkillPin, bool, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT pinned FROM team_task_skills WHERE task_id = $1`, taskID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: 读取任务钉住的技能失败: %w", err)
	}
	pins := []TeamSkillPin{}
	if err := json.Unmarshal(raw, &pins); err != nil {
		return nil, false, fmt.Errorf("store: 解析任务钉住的技能失败: %w", err)
	}
	return pins, true, nil
}

// TeamSkillPinsSave 钉住任务的技能版本。userID/repoID 只取自任务行，不来自任何
// API 输入。先写者赢（ON CONFLICT DO NOTHING）：同一任务同一时刻只有一个执行者，
// 即便发生重复物化，版本不可变，写的也是同一批字节。
func (s *Store) TeamSkillPinsSave(ctx context.Context, taskID, userID, repoID int64, pins []TeamSkillPin) error {
	if pins == nil {
		pins = []TeamSkillPin{}
	}
	raw, err := json.Marshal(pins)
	if err != nil {
		return fmt.Errorf("store: 编码钉住的技能失败: %w", err)
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO team_task_skills (task_id, user_id, repo_id, pinned) VALUES ($1, $2, $3, $4)
		ON CONFLICT (task_id) DO NOTHING`, taskID, userID, repoID, raw); err != nil {
		return fmt.Errorf("store: 钉住任务技能失败: %w", err)
	}
	return nil
}

// TeamSkillPinsClear 清掉任务的钉（从头重建时，下一次物化按最新版重钉）。
// 没有这一行时是 no-op。
func (s *Store) TeamSkillPinsClear(ctx context.Context, taskID int64) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM team_task_skills WHERE task_id = $1`, taskID); err != nil {
		return fmt.Errorf("store: 清除任务钉住的技能失败: %w", err)
	}
	return nil
}

// TeamSkillVersionFiles 读钉住版本的文件内容（物化用）。按 userID 限定：任务
// 只能物化自己属主名下的技能。版本行不存在（被人工删过）→ ErrTeamSkillVersionNotFound，
// 调用方据此以可读原因失败任务，不静默降级。归档不影响解析。
func (s *Store) TeamSkillVersionFiles(ctx context.Context, userID, skillID int64, version int) ([]teamskills.File, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT v.files FROM team_skill_versions v JOIN team_skills s ON s.id = v.skill_id
		WHERE v.skill_id = $1 AND v.version = $2 AND s.user_id = $3`, skillID, version, userID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTeamSkillVersionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读取技能版本文件失败: %w", err)
	}
	var files []teamskills.File
	if err := json.Unmarshal(raw, &files); err != nil {
		return nil, fmt.Errorf("store: 解析技能版本文件失败: %w", err)
	}
	return files, nil
}

// TaskTeamSkills 读任务钉住的技能（API 用，属主限定）。任务不存在或不属于
// userID → ErrTaskNotFound；任务存在但还没钉 → (nil, false, nil)。
func (s *Store) TaskTeamSkills(ctx context.Context, taskID, userID int64) ([]TeamSkillPin, bool, error) {
	var owned bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE id = $1 AND user_id = $2)`,
		taskID, userID).Scan(&owned); err != nil {
		return nil, false, fmt.Errorf("store: 读取任务失败: %w", err)
	}
	if !owned {
		return nil, false, ErrTaskNotFound
	}
	return s.TeamSkillPinsLoad(ctx, taskID)
}
