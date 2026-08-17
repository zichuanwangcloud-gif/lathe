-- 0009_agent_summary.down.sql — 移除实现阶段终局摘要列
--
-- 摘要同时存在于 commit message 与 PR body 里，回滚不丢业务数据。

ALTER TABLE tasks
  DROP COLUMN IF EXISTS agent_summary,
  DROP COLUMN IF EXISTS agent_cost_usd,
  DROP COLUMN IF EXISTS agent_duration_ms,
  DROP COLUMN IF EXISTS agent_num_turns;
