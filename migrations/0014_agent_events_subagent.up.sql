-- 0014_agent_events_subagent.up.sql — 把 subagent 的内部活动纳入事件流
--
-- 背景：agent 派生 subagent（Agent 工具）时，subagent 的内部步骤**不出现在**
-- claude 的 stdout stream-json 里 —— 父会话只看到一次 tool_use，和一条 8–13KB
-- 的汇总结果。实测本机 16 次历史执行里有 7 次派生过 subagent，1093 行内部
-- 活动（占已记录活动的 21%）在详情页上完全不可见：界面只有
-- 「Agent Locate group model test code」一行，中间它找了什么、试错在哪，全黑。
-- 单看 cr-1363 那轮实现，被藏起来的是 228 行 / 共 523 行 —— 44%。
--
-- 这些活动 claude 落在
--   ~/.claude/projects/<cwd-slug>/<session-id>/subagents/agent-<agentId>.jsonl
-- 因此数据源是**文件**而非管道。读取逻辑见 internal/integration/agent/transcript.go，
-- 与 stdout 通路并存而不是取代它：transcript 的 JSONL 是 Claude Code 的内部
-- 格式，无文档、可能随版本变化，不能让主可见性依赖它。
--
-- 三个取舍：
--
-- 1. agent_id 用可空列，而不是塞进 payload。它是界面分组的一等维度
--    （subagent 的事件要缩进到发起它的那条 tool_use 下面），与 tool 列同性质。
--    NULL = 主 agent，老数据因此无需回填。
--
-- 2. 不为 agent_id 建索引。唯一访问模式仍是 SSE 的
--    WHERE task_id = $1 AND id > $2 ORDER BY id（0005 建的 agent_events_task_id
--    已覆盖），agent_id 只是随行返回给前端做分组，不进 WHERE。
--
-- 3. 新增 kind='agent_start'：subagent 文件的首行是派给它的任务描述，
--    若按 user 事件的老路提炼会被误标成「工具结果」。它同时充当分组头 ——
--    界面靠它显示「这个 subagent 被派去干什么」。

ALTER TABLE agent_events ADD COLUMN agent_id text;

-- 扩 kind 的取值集合（沿用 0005 的 text + CHECK 约定：集合随分期演进，
-- 改 CHECK 比 ALTER TYPE 干净）。
ALTER TABLE agent_events DROP CONSTRAINT agent_events_kind_check;

ALTER TABLE agent_events ADD CONSTRAINT agent_events_kind_check CHECK (kind IN (
  'init', 'text', 'thinking', 'tool_use', 'tool_result',
  'result', 'verify_step', 'raw', 'agent_start'
));
