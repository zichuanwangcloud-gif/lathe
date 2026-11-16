-- 0047_repo_agent_driver — 仓库级的执行 agent 选择（f06 多 CLI 驱动）
--
-- repos.agent_driver 决定「之后新建的任务」由哪个 CLI 写代码：claude（默认）或 codex。
-- 任务创建时把它复制进 tasks.agent_driver 钉死（0048），在途任务不受之后修改影响 ——
-- 会话是哪个 CLI 建的，就必须由哪个 CLI 续（review_feedback 的 resume 正确性前提）。
--
-- 默认 'claude'：不配置的部署行为与本迁移之前逐字节一致。
-- 常量默认值的 ADD COLUMN 在 PG11+ 只改元数据；repos 每个用户个位到几十行，
-- CHECK 在 ACCESS EXCLUSIVE 下的全表校验是毫秒级，所以留在事务里、不拆 NOT VALID。
ALTER TABLE repos
  ADD COLUMN agent_driver text NOT NULL DEFAULT 'claude',
  ADD CONSTRAINT repos_agent_driver_check CHECK (agent_driver IN ('claude', 'codex'));
