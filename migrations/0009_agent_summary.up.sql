-- 0009_agent_summary.up.sql — 实现阶段终局摘要落库（docs/04-agent-visibility.md §3.5）
--
-- Result.Text（模型自述的交付小结）此前只被截断塞进 commit message 与
-- PR body，界面上看不到。这四列是详情页摘要卡的主展示字段，因此用
-- 一等列而非 payload jsonb。
--
-- 落库时机：实现阶段的 agent 执行结束后（含 fail 路径 —— 有 result 就存，
-- 失败现场的自述同样是排障信息）。分诊阶段的 result 不存：它的正文是
-- 结构化 JSON 判定，不是人读摘要。

ALTER TABLE tasks
  ADD COLUMN agent_summary     text,     -- Result.Text 全文（模型按四小节输出）
  ADD COLUMN agent_cost_usd    numeric,  -- 本次实现的花费
  ADD COLUMN agent_duration_ms bigint,   -- 墙钟耗时
  ADD COLUMN agent_num_turns   integer;  -- 对话轮数
