-- 回滚 0031：删掉两列失败记录。
--
-- 可完全撤销：up 只加了两列可空列，没有动任何既有数据。丢掉的只是
-- 「最近一次后台运行为什么失败」这个当前值，事件流本身在 prd_events 里不受影响。
ALTER TABLE prds
  DROP COLUMN last_error_input,
  DROP COLUMN last_error;
