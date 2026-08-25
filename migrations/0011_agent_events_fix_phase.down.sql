-- 回滚到 0005 的固定四值。注意：若已有 fix-N 行存在，重建旧 CHECK 会失败，
-- 需先清理或迁移那些行。
ALTER TABLE agent_events DROP CONSTRAINT agent_events_phase_check;

ALTER TABLE agent_events ADD CONSTRAINT agent_events_phase_check
  CHECK (phase IN ('triage', 'implement', 'verify', 'review'));
