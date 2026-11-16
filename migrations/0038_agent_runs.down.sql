-- 0038_agent_runs 的回滚：只删表（0040 的索引、0039 的回填行先于本条回滚）。
DROP TABLE IF EXISTS agent_runs;
