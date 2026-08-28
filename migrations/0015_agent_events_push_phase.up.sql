-- 0015_agent_events_push_phase.up.sql — agent_events.phase 放行 'push'
--
-- 推送重试进事件流（任务 #1551：网络抖动判死整个任务，且重试过程在
-- 详情页完全不可见）。推送阶段的重试/成功通知以 phase='push' 落
-- agent_events，沿用 0011 的改法：改 CHECK 比换 enum 干净。
ALTER TABLE agent_events DROP CONSTRAINT agent_events_phase_check;

ALTER TABLE agent_events ADD CONSTRAINT agent_events_phase_check
  CHECK (phase IN ('triage', 'implement', 'verify', 'review', 'push')
         OR phase ~ '^fix-[0-9]+$');
