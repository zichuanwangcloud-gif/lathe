-- lathe:no-transaction
-- 0067_tasks_access_index — 团队视图（scope=team）的访问索引（f12）
--
-- scope=team 的任务列表是两臂 UNION ALL：
--   臂 1  t.user_id = $1                     → tasks_user_updated
--   臂 2  t.repo_id = 我所在团队的共享仓库   → tasks_repo_updated
-- tasks 今天没有任何 (user_id, …) 或 (repo_id, …) 索引。两条索引的排序与
-- 看板查询一致（ORDER BY updated_at DESC, id DESC）。
--
-- 为什么单开一条迁移、为什么 CONCURRENTLY + 先 DROP：与 0018 同一理由
-- ——tasks 是热表，普通 CREATE INDEX 会阻塞写入；CONCURRENTLY 不能进事务块；
-- 中途失败会留下 INVALID 索引，重跑必须从干净状态开始。
DROP INDEX CONCURRENTLY IF EXISTS tasks_user_updated;
DROP INDEX CONCURRENTLY IF EXISTS tasks_repo_updated;
CREATE INDEX CONCURRENTLY tasks_user_updated ON tasks (user_id, updated_at DESC, id DESC);
CREATE INDEX CONCURRENTLY tasks_repo_updated ON tasks (repo_id, updated_at DESC, id DESC);

-- 建完自检（照抄 0018）：CONCURRENTLY 中途失败会留下 indisvalid = false 的
-- 索引且不会让别的步骤失败。把它变成响亮的迁移失败。
DO $$
BEGIN
  IF (SELECT count(*) FROM pg_index i
        JOIN pg_class c ON c.oid = i.indexrelid
       WHERE c.relname IN ('tasks_user_updated', 'tasks_repo_updated') AND i.indisvalid) <> 2 THEN
    RAISE EXCEPTION 'tasks_user_updated / tasks_repo_updated 未建成或处于 INVALID 状态，需重跑本迁移';
  END IF;
END
$$;
