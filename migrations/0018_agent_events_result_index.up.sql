-- 0018_agent_events_result_index — 成本聚合的访问路径（docs/08-debt-cleanup.md T5）
--
-- 成本面板按 kind='result' 的 payload->>'costUsd' 求和。agent_events 是
-- 全表里最大的一张（每任务成百上千行事件），而 result 事件每任务每阶段
-- 只有一行 —— 部分索引因此极小，却能让聚合避开全表扫描。
--
-- 既有的 agent_events_task_id (task_id, id) 服务的是 SSE 增量拉取
-- （WHERE task_id = $1 AND id > $2），谓词里没有 kind，帮不上聚合。
CREATE INDEX agent_events_result ON agent_events (task_id) WHERE kind = 'result';
