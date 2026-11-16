-- 0066_repo_sharing — 仓库共享到团队（f12）
--
-- 语义：team_id NULL = 私有（今天的行为）；非 NULL = 共享给该团队，团队成员
-- 获得该仓库下任务的【只读】可见性。三条取舍：
--
--   1. 可空、无默认：存量每一行迁移后自动是 NULL，行为与今天逐字节一致。
--   2. ON DELETE SET NULL 而不是 CASCADE：删团队不能连仓库配置一起删
--      （tasks.repo_id 是 NOT NULL REFERENCES repos，历史任务会失去上下文）。
--      取消共享 = 回退成私有，是唯一安全的方向。
--   3. (user_id, provider_repo) 唯一约束保留：一个仓库配置只属于一个属主，
--      共享不改变归属，也不改变任何写权限（配置仍只有属主能改）。
--
-- 任务行不加列：任务的可见性由所属仓库的 team_id 派生，单一真相源。
ALTER TABLE repos ADD COLUMN team_id bigint REFERENCES teams(id) ON DELETE SET NULL;
CREATE INDEX repos_team ON repos (team_id) WHERE team_id IS NOT NULL;
