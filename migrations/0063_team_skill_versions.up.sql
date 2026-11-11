-- 0063_team_skill_versions — 团队自定义技能（f11）：版本（不可变）
--
-- 只 INSERT，永不 UPDATE/DELETE：这是「同一任务重跑拿到同一套技能」的全部
-- 依据，所以也不需要 updated_at 触发器。归档发生在父行（team_skills.archived_at）。
--
--   files           [{"path":"SKILL.md","content":"..."}]，按 path 排序存放
--   content_sha256  按 path 排序后 path\0content\0 串联的 SHA-256：
--                   「内容没变就不产生新版本」与审计「这次任务用的是哪一份内容」
--   total_bytes     全部 content 的 UTF-8 字节数，上限 256 KiB（与服务端校验同值）
CREATE TABLE team_skill_versions (
  id             bigserial   PRIMARY KEY,
  skill_id       bigint      NOT NULL REFERENCES team_skills(id) ON DELETE CASCADE,
  version        integer     NOT NULL CHECK (version > 0),
  files          jsonb       NOT NULL,
  content_sha256 text        NOT NULL,
  total_bytes    integer     NOT NULL CHECK (total_bytes > 0 AND total_bytes <= 262144),
  note           text        NOT NULL DEFAULT '',
  created_at     timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT team_skill_versions_uniq UNIQUE (skill_id, version)
);
CREATE INDEX team_skill_versions_skill ON team_skill_versions (skill_id, version DESC);
