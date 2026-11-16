-- 回滚 0059：删规则表（触发器与索引随表删除）。
-- 0060 的 schedule_runs 外键引用本表，回滚顺序必须是 0060 先于 0059。
DROP TABLE schedules;
