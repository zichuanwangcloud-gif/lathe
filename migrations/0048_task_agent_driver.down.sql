-- 回滚 0048：删掉任务钉死的执行 agent。
--
-- 可完全撤销：up 只加了一列可空列和它的 CHECK，没有回填任何数据；
-- 回滚后所有任务回到「只有 claude」的语义，与 0048 之前一致。
-- DROP 只改目录，留在事务里即可（不带非事务标记）。
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_agent_driver_check;
ALTER TABLE tasks DROP COLUMN IF EXISTS agent_driver;
