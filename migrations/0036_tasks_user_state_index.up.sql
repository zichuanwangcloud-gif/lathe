-- lathe:no-transaction
-- 0036_tasks_user_state_index — 按属主 + 状态查任务的访问路径
--
-- 收件箱徽标每 30 秒被每个打开的标签页查一次，查询形状是
-- WHERE user_id = $1 AND state IN (...)。tasks 原先没有任何以 user_id 开头的
-- 索引，按属主查询只能全表扫；这一个索引也顺带改善 ListTasks 与 Stats。
--
-- 为什么 CONCURRENTLY + 非事务：tasks 在 runner 的热路径上（每次转移都
-- UPDATE），普通 CREATE INDEX 会在建索引期间阻塞写入（AGENTS.md §6.3）。
-- CONCURRENTLY 不能进事务块，首行标记让 internal/store 逐条无事务执行。
--
-- 写法照抄 0018：先无条件 DROP（清掉上次中断留下的 INVALID 残骸，
-- CREATE ... IF NOT EXISTS 会因名字已存在而跳过、永远修不好），建完用 DO 块
-- 自检 indisvalid，把「索引存在但无效」变成一次响亮的迁移失败。
DROP INDEX IF EXISTS tasks_user_state;
CREATE INDEX CONCURRENTLY tasks_user_state ON tasks (user_id, state);

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_index i
    JOIN pg_class c ON c.oid = i.indexrelid
    WHERE c.relname = 'tasks_user_state' AND i.indisvalid
  ) THEN
    RAISE EXCEPTION 'tasks_user_state 未建成或处于 INVALID 状态，需重跑本迁移';
  END IF;
END
$$;
