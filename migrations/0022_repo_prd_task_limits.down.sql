-- 回滚 0022：删列即连带删掉两条 CHECK 约束。
-- 完全可撤销 —— 这两列只被规划管线的自检读取，没有别的消费方依赖它们存在。
ALTER TABLE repos
  DROP COLUMN prd_task_max_files,
  DROP COLUMN prd_task_max_lines;
