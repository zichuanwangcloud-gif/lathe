-- 0039_agent_runs_backfill 的回滚：只删回填行，实时行保留。
-- 再 up 时由截止条件保证不与实时行重复计账。
DELETE FROM agent_runs WHERE backfill;
