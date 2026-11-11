-- 0064_team_task_skills — 团队自定义技能（f11）：任务钉版本
--
-- 任务在实现阶段第一次物化团队技能时，把「用了哪几个技能的哪一版」钉在这里，
-- 之后续跑一律用钉住的版本（从头重建时清掉重钉）。一条任务一行：
-- pinned = [] 表示「钉过了，当时没有启用的技能」，与「没有这一行（还没钉）」可分。
--
--   pinned  [{"skillId":12,"slug":"team-acme-testing","version":3,"sha256":"…"}]
--
-- user_id/repo_id 从 tasks 行抄来（建单后不变，不存在漂移），让「按属主 + 仓库」
-- 的聚合（列表里的 pinnedTaskCount）有索引可走，不必 join tasks。
-- 新建的空表，普通 CREATE INDEX 即可，不涉及 CONCURRENTLY。
CREATE TABLE team_task_skills (
  task_id   bigint      PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
  user_id   bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  repo_id   bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  pinned    jsonb       NOT NULL,
  pinned_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX team_task_skills_owner_repo
  ON team_task_skills (user_id, repo_id, pinned_at DESC);
