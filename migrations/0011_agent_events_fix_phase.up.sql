-- 修复回路的事件 phase 是 fix-N（pipeline.go 的 fixSink），0005 的 CHECK
-- 只认四个固定值，修复轮事件全部落库失败（SQLSTATE 23514）被丢弃 ——
-- 任务详情页在修复期间完全黑盒，任务 #596 因此看似「卡死在验证中」。
ALTER TABLE agent_events DROP CONSTRAINT agent_events_phase_check;

ALTER TABLE agent_events ADD CONSTRAINT agent_events_phase_check
  CHECK (phase IN ('triage', 'implement', 'verify', 'review')
         OR phase ~ '^fix-[0-9]+$');
