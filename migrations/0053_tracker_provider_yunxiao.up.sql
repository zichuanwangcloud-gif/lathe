-- 0053_tracker_provider_yunxiao —— 任务来源平台放开 'yunxiao'（f08 云效任务来源）
--
-- 设计依据：docs/features/tracker-yunxiao.md §3.1。与 internal/tracker 的
-- ProviderYunxiao / ValidProvider 两边一起改。只改 CHECK 取值，不动表结构；
-- 活任务唯一索引 tasks_one_active_per_item 已按 (repo_id, tracker_provider,
-- external_key) 建，云效任务天然适用。

ALTER TABLE tasks DROP CONSTRAINT tasks_tracker_provider_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_tracker_provider_check
  CHECK (tracker_provider IN ('linear', 'internal', 'yunxiao'));
