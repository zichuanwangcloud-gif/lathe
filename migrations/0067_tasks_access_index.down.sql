-- lathe:no-transaction
-- 回滚 0067：删掉两条访问索引。CONCURRENTLY 的 DROP 不阻塞 tasks 写入，
-- 失败不留残骸（要么还在、要么没了），重跑天然幂等。
DROP INDEX CONCURRENTLY IF EXISTS tasks_repo_updated;
DROP INDEX CONCURRENTLY IF EXISTS tasks_user_updated;
