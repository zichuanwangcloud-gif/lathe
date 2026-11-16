-- 回滚 0047：删掉仓库级的执行 agent 选择。
--
-- 可完全撤销：up 只加了一列带默认值的列和它的 CHECK，没有动任何既有数据。
ALTER TABLE repos DROP CONSTRAINT repos_agent_driver_check;
ALTER TABLE repos DROP COLUMN agent_driver;
