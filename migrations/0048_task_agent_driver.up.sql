-- lathe:no-transaction
-- 0048_task_agent_driver — 任务钉死的执行 agent（f06 多 CLI 驱动）
--
-- tasks.agent_driver 在唯一的 INSERT 点（internal/task/machine.go Create）从
-- repos.agent_driver 复制而来，之后不再改：分诊、实现、修复回路、review 二轮、
-- rebase 跟进重验都走这一个驱动（Pipeline 从任务行读，不经 RepoConfig）。
--
-- NULL = 本迁移之前建的任务 = claude（当时只有 claude，这是事实而不是猜测）。
-- 代码里统一按 COALESCE(agent_driver, 'claude') 读，只写在 taskColumns 一处；
-- 不做存量回填，所以 down 能精确撤销。
--
-- 为什么非事务 + NOT VALID/VALIDATE：tasks 是长期累积的表（每次尝试一行，从不删），
-- 直接 ADD CONSTRAINT … CHECK 会在 ACCESS EXCLUSIVE 下扫全表，期间派发、Transition、
-- 轮询全部阻塞。NOT VALID 只改目录、瞬间完成；VALIDATE 扫表时只拿
-- SHARE UPDATE EXCLUSIVE，不挡读写。两步必须在不同事务里才有意义（同一事务里
-- 前一步的排他锁会持有到提交），所以首行打了非事务标记（先例 0018）。
--
-- 非事务的代价是可能半应用，所以每一步都可重跑：ADD COLUMN IF NOT EXISTS、
-- 先 DROP CONSTRAINT IF EXISTS 再加。框架在所有语句成功后才写版本记录。
-- 无列默认值的 ADD COLUMN 只改元数据，不重写表。
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS agent_driver text;
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_agent_driver_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_agent_driver_check
  CHECK (agent_driver IS NULL OR agent_driver IN ('claude', 'codex')) NOT VALID;
ALTER TABLE tasks VALIDATE CONSTRAINT tasks_agent_driver_check;
