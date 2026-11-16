-- 0041_agent_events_reentry_phase.up.sql — agent_events.phase 放行 'implement-reentry'
--
-- 流水线在 resume 失败降级、开新会话重入实现时以 phase='implement-reentry'
-- 起 EventSink（internal/runner/eventsink.go 的 PhaseImplementReentry），
-- 但 CHECK 没跟上：整批事件被拒（SQLSTATE 23514）、BatchSink 只记 warn 丢弃，
-- 详情页里整段运行凭空消失。与 0011（fix-N 被拒）同一类漂移，沿用同一改法。
--
-- 只放宽这一个取值：phase 集合仍显式枚举（0005 的立场），不改成全放开 ——
-- 否则下一个拼写错误会静默入库。已经丢掉的历史事件找不回来，本迁移只救未来。
ALTER TABLE agent_events DROP CONSTRAINT agent_events_phase_check;

ALTER TABLE agent_events ADD CONSTRAINT agent_events_phase_check
  CHECK (phase IN ('triage', 'implement', 'implement-reentry', 'verify', 'review', 'push')
         OR phase ~ '^fix-[0-9]+$');
