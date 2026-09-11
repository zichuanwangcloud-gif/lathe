-- lathe:no-transaction
-- 0018_agent_events_result_index — 成本聚合的访问路径（docs/08-debt-cleanup.md T5）
--
-- 成本面板按 kind='result' 的 payload->>'costUsd' 求和。agent_events 是
-- 全表里最大的一张（每任务成百上千行事件），而 result 事件每任务每阶段
-- 只有一行 —— 部分索引因此极小，却能让聚合避开全表扫描。
--
-- 既有的 agent_events_task_id (task_id, id) 服务的是 SSE 增量拉取
-- （WHERE task_id = $1 AND id > $2），谓词里没有 kind，帮不上聚合。
--
-- 为什么必须带 CONCURRENTLY，以及为什么开头要 DROP：
--
--   1. 不带 CONCURRENTLY 的 CREATE INDEX 会拿 ACCESS EXCLUSIVE 锁，
--      建索引期间 agent_events 的写入（runner 正在写事件流）全部阻塞。
--      这张表在生产实例上不小，锁的时长等于一次全表扫描 + 排序的时长。
--   2. CONCURRENTLY 不能进事务块，所以本文件首行打了
--      `-- lathe:no-transaction`，由 internal/store 逐条无事务执行。
--   3. CONCURRENTLY 失败（被取消、context 超时、进程被杀）会**留下一条
--      INVALID 索引**：它占着名字却不被查询计划使用，而且
--      `CREATE INDEX CONCURRENTLY IF NOT EXISTS` 会因为「名字已存在」
--      而直接跳过，永远修不好。所以这里先无条件 DROP 再建 —— 无论上次
--      是否失败、失败到哪一步，重跑都从干净状态开始。DROP INDEX
--      (非 CONCURRENTLY) 本身要求排他锁，但它只在索引存在时才有意义，
--      且这里的索引是执行本迁移时自建的，没有并发读写者在用。
--   4. 框架把版本记录放在所有语句成功之后（applyOneNoTx），所以中断的
--      迁移不会被记成已应用，下次 MigrateUp 会从头重跑本文件 ——
--      上面的 DROP 就是为此准备的。
--
-- 正确性核对：DROP 与 CREATE 之间没有窗口会丢数据（索引是从表推导的），
-- 只是那段时间聚合查询走全表扫描而已。
DROP INDEX IF EXISTS agent_events_result;
CREATE INDEX CONCURRENTLY agent_events_result ON agent_events (task_id) WHERE kind = 'result';

-- 建完自检：CONCURRENTLY 若中途失败，索引会以 indisvalid = false 留存，
-- 而这种状态不会让上面的 CREATE 报错之外的任何一步失败。这条 DO 块把
-- 「索引存在但无效」变成一次响亮的迁移失败，而不是留个哑巴残骸。
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_index i
    JOIN pg_class c ON c.oid = i.indexrelid
    WHERE c.relname = 'agent_events_result' AND i.indisvalid
  ) THEN
    RAISE EXCEPTION 'agent_events_result 未建成或处于 INVALID 状态，需重跑本迁移';
  END IF;
END
$$;
