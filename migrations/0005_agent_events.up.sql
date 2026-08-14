-- 0005_agent_events.up.sql — agent 执行事件流：让任务进度在界面上可见
--
-- 背景：agent 一次跑十几到几十分钟，但界面只能看到状态机的粗粒度迁移
-- （triaging → implementing → verifying）。claude CLI 的 stream-json 事件流
-- 此前被 runner 解析完即丢弃，只留终局 result。这张表把它留下来，供详情页
-- 的执行日志面板做实时滚动与断线补齐。
--
-- 四个设计取舍：
--
-- 1. 不复用 task_events。那张表是状态机转移的审计流，to_state NOT NULL 是它的
--    核心约束，而 agent 的每条 stdout 事件没有状态转移语义，塞不进去。两条流
--    的写入频率也差着两个数量级（前者每任务十余行，后者每任务成百上千行）。
--
-- 2. 存提炼后的可读内容，不存原始 NDJSON。单行事件上限 16MB（见
--    internal/integration/agent/agent.go 的 maxLineBytes：init 事件会把全部
--    工具、技能、插件清单塞进一行），原样入库既浪费空间也没人看得懂。提炼
--    逻辑在 agent.Digest，无法识别的事件退化成 kind='raw' 而不是丢弃。
--
-- 3. 游标用 id 而非 at。批量插入时同一批的 now() 完全相同（task_events 就得靠
--    ORDER BY at, id 破平），而 SSE 的 Last-Event-ID 需要一个严格单调的游标来做
--    断线补齐，bigserial 正好。
--
-- 4. append-only，因此不设 updated_at，也不挂 set_updated_at trigger。

-- ---------------------------------------------------------------- agent_events

CREATE TABLE agent_events (
  id      bigserial   PRIMARY KEY,
  task_id bigint      NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  phase   text        NOT NULL,   -- 哪一段执行产生的
  kind    text        NOT NULL,   -- 事件类别，见下方 CHECK
  tool    text,                   -- kind=tool_use/tool_result 时的工具名
  body    text,                   -- 提炼出的可读内容，已截断（见 agent.maxEntryBody）
  payload jsonb       NOT NULL DEFAULT '{}',  -- 结构化补充（耗时、成本、退出码等）
  at      timestamptz NOT NULL DEFAULT now(),

  -- 状态类字段用 text + CHECK 而非 enum：集合会随分期演进，改 CHECK 比
  -- ALTER TYPE 干净（沿用 0001 的约定）。
  --
  -- 'review' 是 --from-pr 二轮续跑（agent.RunParams.FromPR 已支持，流水线还没
  -- 走到），这里先预留，免得将来为一行改动再加一条迁移 —— 同 users.webhook_slug
  -- 的处理方式。
  CONSTRAINT agent_events_phase_check CHECK (phase IN (
    'triage', 'implement', 'verify', 'review'
  )),

  -- init/text/thinking/tool_use/tool_result/result 对应 stream-json 的事件形态；
  -- verify_step 是验证器的步骤输出（与 verifications 表同源，那张表存结构化结果，
  -- 这里存人读的时间线）；raw 是提炼不出结构时的兜底。
  CONSTRAINT agent_events_kind_check CHECK (kind IN (
    'init', 'text', 'thinking', 'tool_use', 'tool_result',
    'result', 'verify_step', 'raw'
  ))
);

-- SSE 增量拉取的唯一访问模式：WHERE task_id = $1 AND id > $2 ORDER BY id。
CREATE INDEX agent_events_task_id ON agent_events (task_id, id);
