-- 0053_tracker_provider_yunxiao 回滚
--
-- 刻意【不】DELETE 云效任务：若库里已有 tracker_provider='yunxiao' 的任务，
-- 重建 CHECK 会以 23514 失败、整次回滚中止 —— 宁可报错让人看见，也不静默
-- 丢数据（比 0021 的 down 更保守）。存在云效任务时须先人工处置再回滚。

ALTER TABLE tasks DROP CONSTRAINT tasks_tracker_provider_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_tracker_provider_check
  CHECK (tracker_provider IN ('linear', 'internal'));
