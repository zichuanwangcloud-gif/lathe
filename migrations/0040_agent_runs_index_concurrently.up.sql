-- lathe:no-transaction
-- 0040_agent_runs_index_concurrently — 用量聚合的两条主查询索引（f03）
--
-- 放在 0039 回填之后建：索引不必为回填逐行维护。agent_runs 会持续增长，
-- 不带 CONCURRENTLY 的 CREATE INDEX 会拿锁阻塞 runner 的记账写入，所以
-- 本文件首行打了非事务标记，由 internal/store 逐条无事务执行（同 0018）。
--
-- 开头先 DROP：CONCURRENTLY 中途失败会留下 INVALID 索引，而
-- CREATE INDEX CONCURRENTLY IF NOT EXISTS 会因「名字已存在」直接跳过、
-- 永远修不好。先无条件 DROP 再建，重跑从干净状态开始。
DROP INDEX IF EXISTS agent_runs_user_time;
DROP INDEX IF EXISTS agent_runs_repo_time;
CREATE INDEX CONCURRENTLY agent_runs_user_time ON agent_runs (user_id, started_at DESC);
CREATE INDEX CONCURRENTLY agent_runs_repo_time ON agent_runs (repo_id, started_at DESC);

-- 建完自检：把「索引存在但 INVALID」变成一次响亮的迁移失败。
DO $$
BEGIN
  IF (SELECT count(*) FROM pg_index i
        JOIN pg_class c ON c.oid = i.indexrelid
       WHERE c.relname IN ('agent_runs_user_time', 'agent_runs_repo_time') AND i.indisvalid) < 2 THEN
    RAISE EXCEPTION 'agent_runs_user_time / agent_runs_repo_time 未建成或处于 INVALID 状态，需重跑本迁移';
  END IF;
END
$$;
