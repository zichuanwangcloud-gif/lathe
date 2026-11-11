-- 0062_team_skills — 团队自定义技能（f11，docs/features/team-skills.md）：技能本体
--
-- 用户按仓库登记自己的技能。内容存在 0063 的不可变版本行里，这张表只放
-- 「名字、开关、归档」这类可变的元数据。
--
-- user_id 与 repo_id 都存：repo_id 决定作用于哪个仓库，user_id 是属主隔离的
-- 边界本身。两者由服务端从 repos 行抄一份，不经 API 传入。
--
-- slug 的 CHECK 只挡字符集与长度（允许 a--b 这类连续连字符），完整的规范化
-- 规则只在 Go 侧（internal/teamskills）实现，两层各有职责。
--
-- 归档而不是删除：archived_at 非空的行不占名字（部分唯一索引），但它的版本
-- 行仍在，已经钉住它的任务续跑时照常能解析。
CREATE TABLE team_skills (
  id          bigserial   PRIMARY KEY,
  user_id     bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  repo_id     bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  slug        text        NOT NULL,
  title       text        NOT NULL DEFAULT '',
  enabled     boolean     NOT NULL DEFAULT true,
  archived_at timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT team_skills_slug_check
    CHECK (slug ~ '^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$')
);

-- 同一仓库的在役技能名唯一；归档的不占名字。
CREATE UNIQUE INDEX team_skills_active_slug
  ON team_skills (repo_id, slug) WHERE archived_at IS NULL;
CREATE INDEX team_skills_user_repo ON team_skills (user_id, repo_id);

CREATE TRIGGER team_skills_updated_at BEFORE UPDATE ON team_skills
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();
