-- 0038_agent_runs — 每次 agent 运行的用量账本（f03，docs/features/usage-stats.md）
--
-- 为什么新开一张表而不是从 agent_events 求和：
-- 1. BatchSink 缓冲满时丢事件（batchsink.go），result 也可能丢 —— 账不能丢。
-- 2. 超时/静默判死没有 result 事件，但那是一次真实的运行、真实的钱。
-- 3. PRD 规划落在 prd_events，预览推荐根本没落库；implement-reentry 的事件
--    被 agent_events_phase_check 整批拒收（f04 修），本表是该阶段唯一的账。
-- 4. token 此前压根没解析；新增字段不该塞进人读事件流的 payload。
--
-- agent_runs 是账本，agent_events / prd_events 是人读时间线，两者口径
-- （一次运行 vs 一条事件）本来就不同，不合并。
--
-- append-only，不设 updated_at、不挂 set_updated_at 触发器（同 agent_events）。
-- 唯一的例外是定档回补：verify_tier 只允许 NULL → light/heavy，其余列写入后不再改。
--
-- 需要 PostgreSQL ≥ 15（UNIQUE NULLS NOT DISTINCT）。dev-infra 与 CI 都是 16。
CREATE TABLE agent_runs (
  id         bigserial PRIMARY KEY,

  -- 归属：run_kind=task/preview 填 task_id，run_kind=prd 填 prd_id，恰有其一。
  -- user_id / repo_id 是冗余快照，由 store 在 INSERT ... SELECT 时从 tasks / prds
  -- 行上取，调用方不传：聚合查询的主过滤条件，漏 JOIN 就是跨用户求和。
  task_id    bigint REFERENCES tasks(id) ON DELETE CASCADE,
  prd_id     bigint REFERENCES prds(id) ON DELETE CASCADE,
  user_id    bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  repo_id    bigint NOT NULL REFERENCES repos(id) ON DELETE CASCADE,

  phase      text NOT NULL,   -- 原始阶段标签：triage/implement/implement-reentry/fix-N/verify/push/plan/plan-review/recommend
  bucket     text NOT NULL,   -- 归并后的阶段桶（store.UsageBucket 是唯一真相源）
  run_kind   text NOT NULL,   -- task | prd | preview
  review_round boolean NOT NULL DEFAULT false,  -- PR review 续跑那一轮（plan.Review），写入时快照

  session_id text,            -- 可为 NULL（假 driver / 早期失败）；去重键里按 NULLS NOT DISTINCT 处理
  resume     boolean NOT NULL DEFAULT false,

  model      text,            -- 主模型（modelUsage 里 costUSD 最大的那个）；未知为 NULL
  models     jsonb NOT NULL DEFAULT '[]',  -- 逐模型明细 [{model,inputTokens,...}]，≤16 项
  input_tokens          bigint NOT NULL DEFAULT 0,
  output_tokens         bigint NOT NULL DEFAULT 0,
  cache_read_tokens     bigint NOT NULL DEFAULT 0,
  cache_creation_tokens bigint NOT NULL DEFAULT 0,

  -- token 是否由 CLI 上报。false 时四个计数为 0 但含义是「未知」，不是「用了 0」。
  -- codex 不报模型名，所以不能用 models<>'[]' 代替它。
  tokens_known boolean NOT NULL DEFAULT false,

  -- 费用（USD）。NULL = 未知（codex 不报费用、或进程没给出 result 就被杀/退出），
  -- 与「确实花了 $0」分开。聚合 SUM 自然跳过 NULL，另出 costKnown 计数。
  cost_usd   numeric(14,6),
  duration_ms bigint NOT NULL DEFAULT 0,

  -- 这次运行发生时它所属尝试的验证档位，写入时快照。NULL = 当时尚未定档。
  -- 只允许 NULL → light/heavy 补一次（定档回补），已有值的行永不改写。
  verify_tier text,
  num_turns  int NOT NULL DEFAULT 0,

  status     text NOT NULL,   -- success | error | no_result | killed
  subtype    text,            -- CLI 的 result.subtype 短标签，≤64 rune

  -- started_at：实时行 = 调用方在 Run 之前取的时钟；回填行 = ended_at - duration_ms。
  -- ended_at  ：实时行 = Run 返回后取的时钟；回填行 = 事件的 at。
  -- at：库时钟的写入时刻。store 的 INSERT 列表里不出现 at，永远取 DEFAULT now()；
  -- 0039 的截止条件只信这一列（与 agent_events.at / prd_events.at 同一个时钟）。
  started_at timestamptz NOT NULL,
  ended_at   timestamptz NOT NULL,
  at         timestamptz NOT NULL DEFAULT now(),
  backfill   boolean NOT NULL DEFAULT false,      -- 0039 从历史事件回填的行

  CONSTRAINT agent_runs_target_check CHECK (
    (run_kind IN ('task','preview') AND task_id IS NOT NULL AND prd_id IS NULL) OR
    (run_kind = 'prd'               AND task_id IS NULL     AND prd_id IS NOT NULL)),
  CONSTRAINT agent_runs_bucket_check CHECK (bucket IN (
    'triage','implement','review','verify','push','prd_plan','prd_review','preview','other')),
  CONSTRAINT agent_runs_kind_check   CHECK (run_kind IN ('task','prd','preview')),
  CONSTRAINT agent_runs_status_check CHECK (status IN ('success','error','no_result','killed')),
  CONSTRAINT agent_runs_time_check   CHECK (ended_at >= started_at),
  CONSTRAINT agent_runs_tier_check   CHECK (verify_tier IS NULL OR verify_tier IN ('light','heavy')),
  CONSTRAINT agent_runs_tier_kind_check CHECK (run_kind <> 'prd' OR verify_tier IS NULL),
  -- 幂等闸门。NULLS NOT DISTINCT 是必需的：target_check 保证每行 task_id/prd_id
  -- 恰有一个 NULL，session_id 也可为 NULL；默认 NULLS DISTINCT 下这条约束永远
  -- 不会冲突。
  CONSTRAINT agent_runs_attempt_uniq UNIQUE NULLS NOT DISTINCT
    (run_kind, task_id, prd_id, phase, session_id, resume, started_at)
);

-- 刻意不在这里建时间索引：表随后会被 0039 回填，索引在空表上建、再被回填
-- 逐行维护是白烧；两个主查询索引统一放 0040 用 CONCURRENTLY 建。
-- 唯一约束自带的那条索引是幂等的前提，必须随表建，不能延后。
