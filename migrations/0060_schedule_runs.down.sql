-- 回滚 0060：删运行记录表（索引随表删除）。须先于 0059 回滚。
DROP TABLE schedule_runs;
