-- 回滚 0014。先清掉新 kind 的行，否则重建旧 CHECK 会失败
-- （同 0011 down 的注意事项，这里直接把清理做掉）。
DELETE FROM agent_events WHERE kind = 'agent_start';

ALTER TABLE agent_events DROP CONSTRAINT agent_events_kind_check;

ALTER TABLE agent_events ADD CONSTRAINT agent_events_kind_check CHECK (kind IN (
  'init', 'text', 'thinking', 'tool_use', 'tool_result',
  'result', 'verify_step', 'raw'
));

-- subagent 的事件行本身保留（kind 合法），只是丢掉归属信息 —— 回滚后
-- 它们会平铺在主 agent 的时间线里。
ALTER TABLE agent_events DROP COLUMN agent_id;
