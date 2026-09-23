-- 0021_internal_issues 回滚
--
-- 警告：若已存在 tracker_provider='internal' 的任务行，回滚会把它们
-- 连同工单数据一起删掉（issues 表 DROP 会级联不到 tasks —— 两张表
-- 无 FK，所以这里显式清理，否则旧索引重建时 key 可能与新行冲突）。

DROP INDEX tasks_one_active_per_item;

-- 内置任务在旧 schema 里没有表达方式（linear_issue_key 语义即 Linear），
-- 回滚前必须显式删除 —— 宁可报错让人看见，也不静默丢数据：
-- 存在 internal 任务时请先在业务上处置（取消/导出）再回滚。
DELETE FROM tasks WHERE tracker_provider = 'internal';

ALTER TABLE tasks DROP CONSTRAINT tasks_tracker_provider_check;
ALTER TABLE tasks DROP COLUMN tracker_provider;

ALTER TABLE tasks RENAME COLUMN external_key TO linear_issue_key;
ALTER TABLE tasks RENAME COLUMN external_id TO linear_issue_id;

CREATE UNIQUE INDEX tasks_one_active_per_issue
  ON tasks (repo_id, linear_issue_key)
  WHERE state NOT IN ('merged', 'failed', 'cancelled');

DROP TABLE issue_comments;
DROP TABLE issues;
DROP SEQUENCE issue_key_seq;
