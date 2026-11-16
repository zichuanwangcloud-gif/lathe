-- 0041_agent_events_reentry_phase.down.sql — 回滚到 0015 的 phase 集合
--
-- 注意：若已有 phase='implement-reentry' 的行，本回滚会因 CHECK 冲突失败；
-- 那是正确的失败（与 0015 down 同一立场）—— 先清理或保留数据由人决定，
-- 不静默删证据。
ALTER TABLE agent_events DROP CONSTRAINT agent_events_phase_check;

ALTER TABLE agent_events ADD CONSTRAINT agent_events_phase_check
  CHECK (phase IN ('triage', 'implement', 'verify', 'review', 'push')
         OR phase ~ '^fix-[0-9]+$');
