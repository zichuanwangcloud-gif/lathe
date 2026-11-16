-- lathe:no-transaction
-- 0040_agent_runs_index_concurrently 的回滚。DROP INDEX CONCURRENTLY 不能进事务块，
-- 所以 down 同样带非事务标记；失败不留残骸，重跑天然幂等。
DROP INDEX CONCURRENTLY IF EXISTS agent_runs_user_time;
DROP INDEX CONCURRENTLY IF EXISTS agent_runs_repo_time;
