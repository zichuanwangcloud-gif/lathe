-- 0015_agent_events_push_phase.down.sql — 回滚 agent_events.phase 的 'push'
--
-- 注意：若已有 phase='push' 的历史行，本回滚会因 CHECK 冲突失败；
-- 那是正确的失败 —— 先清理或保留数据由人决定，不静默删证据。
ALTER TABLE agent_events DROP CONSTRAINT agent_events_phase_check;

ALTER TABLE agent_events ADD CONSTRAINT agent_events_phase_check
  CHECK (phase IN ('triage', 'implement', 'verify', 'review')
         OR phase ~ '^fix-[0-9]+$');
