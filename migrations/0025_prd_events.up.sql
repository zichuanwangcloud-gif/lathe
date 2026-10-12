-- 0025_prd_events — 规划管线的 agent 事件流（docs/10 §1.3）
--
-- 为什么不复用 agent_events：那张表 task_id 是 NOT NULL 且外键 tasks。
-- 模糊任务不进 tasks（见 0023 注释），要复用就得把 task_id 放开为可空 ——
-- 而「每条 agent 事件都属于某个可执行任务」是执行管线的强约束，不该为
-- 规划管线破例。平行表的代价是两套读取代码，但它们共用同一个轮询协议
-- （?after=last_id），前端 AgentEventItem.vue 照样复用，重复量很小。
--
-- kind 取值 = agent.Digest() 能产出的全集，逐个核对过（digest.go）：
-- init / text / thinking / tool_use / tool_result / result / raw。
-- 比 agent_events 少两个，都是故意的：
--   verify_step  规划管线只读代码，不跑验证，没有这类事件。
--   agent_start  只由 transcript.go 读 subagent 的 JSONL 时产出，而规划
--                与复核都不派 subagent（只读探索用不上），走不到那条路。
-- 这里少一个值的后果不是丢一条事件而是**整批 COPY 被拒**（同 §9 那次
-- UTF-8 截断事故的失效形态）。所以若将来让规划 agent 派 subagent，或改用
-- transcript 通路，必须先加一条迁移放开这个 CHECK。
--
-- phase 只有两个：
--   plan        对话式规划的每一轮（A 理解 / B 探索 / C 收敛 / D 拆分 / E 定稿
--               都归这里；轮次与阶段在 prd_rounds 里，事件流不重复记）
--   plan-review 对抗复核（§5.5）—— 独立上下文、只喂 §1–§4 与 §7 的那次只读
--               调用。单独成一个 phase 是为了让人在事件流里一眼分清
--               「作者说的」和「攻击者说的」。
CREATE TABLE prd_events (
  id      bigserial   PRIMARY KEY,
  prd_id  bigint      NOT NULL REFERENCES prds(id) ON DELETE CASCADE,
  -- 哪一轮产生的。对抗复核不属于任何轮次，留空。
  round   int,
  phase   text        NOT NULL,
  kind    text        NOT NULL,
  tool    text,
  body    text,
  payload jsonb       NOT NULL DEFAULT '{}',
  at      timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT prd_events_phase_check CHECK (phase IN ('plan', 'plan-review')),
  CONSTRAINT prd_events_kind_check CHECK (kind IN (
    'init', 'text', 'thinking', 'tool_use', 'tool_result',
    'result', 'raw'
  )),
  CONSTRAINT prd_events_round_check CHECK (round IS NULL OR round >= 1)
);

-- 增量轮询的主索引：?after=<last_id> 按 (prd_id, id) 走。
CREATE INDEX prd_events_prd_id ON prd_events (prd_id, id);
