-- 0039_agent_runs_backfill — 把历史 agent_events / prd_events 的 result 行回填成 agent_runs（f03）
--
-- 只做数据回填，不改 schema（一次迁移只做一件事）。
--
-- 逐字段口径（docs/features/usage-stats.md §3.1/0039）：
--   bucket        与 store.UsageBucket(phase, false) 同构（回填拿不到 review 信息，
--                 历史 review 轮全部归 implement；AC-6 对拍）
--   cost_usd      payload.costUsd；键缺失或 payload.driver='codex' → NULL（未知，不按 0）
--   status        isError=true 或 subtype<>'success' → error，否则 success
--   started_at    事件 at - duration_ms；ended_at = 事件 at
--   verify_tier   回填那一刻任务行上的档位（历史里没有逐次档位，唯一可得的近似）
--   token 四列    只有 codex 的 result 事件 payload 带 usage（f06），按 UsageFromCodex
--                 同一口径映射；其余 0 / tokens_known=false
--
-- 截止条件：某目标一旦有了实时行，在「它某条实时行所对应运行的开始时刻（库时钟）
-- 减 5 分钟容差」之后的该目标事件一律视为已由实时账覆盖，不回填。只比库时钟：
-- r.at 与 ae.at 都是同一台 Postgres 的 now()；(r.ended_at - r.started_at) 是节点
-- 墙钟的**差值**，与节点和库之间的绝对偏差无关。代价是首条实时运行前 5 分钟内
-- 旧节点的事件不回填 —— 少补，方向安全，永不重复计账。
--
-- ON CONFLICT DO NOTHING：不经 down 直接重跑也幂等。
INSERT INTO agent_runs (
  task_id, prd_id, user_id, repo_id, phase, bucket, run_kind, review_round,
  session_id, resume, model, models,
  input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, tokens_known,
  cost_usd, duration_ms, verify_tier, num_turns, status, subtype,
  started_at, ended_at, backfill)
SELECT ae.task_id, NULL, t.user_id, t.repo_id, ae.phase,
       CASE
         WHEN ae.phase = 'review' THEN 'review'
         WHEN ae.phase = 'triage' THEN 'triage'
         WHEN ae.phase IN ('implement', 'implement-reentry') OR ae.phase ~ '^fix-[0-9]+$' THEN 'implement'
         WHEN ae.phase IN ('verify', 'push') THEN ae.phase
         WHEN ae.phase = 'plan' THEN 'prd_plan'
         WHEN ae.phase = 'plan-review' THEN 'prd_review'
         WHEN ae.phase = 'recommend' THEN 'preview'
         ELSE 'other'
       END,
       'task', false, NULL, false, NULL, '[]'::jsonb,
       CASE WHEN jsonb_typeof(ae.payload->'usage') = 'object' THEN
         GREATEST(COALESCE((ae.payload->'usage'->>'input_tokens')::bigint, 0)
                - COALESCE((ae.payload->'usage'->>'cached_input_tokens')::bigint, 0)
                - COALESCE((ae.payload->'usage'->>'cache_write_input_tokens')::bigint, 0), 0)
       ELSE 0 END,
       CASE WHEN jsonb_typeof(ae.payload->'usage') = 'object' THEN
         COALESCE((ae.payload->'usage'->>'output_tokens')::bigint, 0) ELSE 0 END,
       CASE WHEN jsonb_typeof(ae.payload->'usage') = 'object' THEN
         COALESCE((ae.payload->'usage'->>'cached_input_tokens')::bigint, 0) ELSE 0 END,
       CASE WHEN jsonb_typeof(ae.payload->'usage') = 'object' THEN
         COALESCE((ae.payload->'usage'->>'cache_write_input_tokens')::bigint, 0) ELSE 0 END,
       COALESCE(jsonb_typeof(ae.payload->'usage') = 'object', false),
       CASE WHEN ae.payload ? 'costUsd' AND COALESCE(ae.payload->>'driver', '') <> 'codex'
            THEN (ae.payload->>'costUsd')::numeric ELSE NULL END,
       COALESCE((ae.payload->>'durationMs')::bigint, 0),
       t.verify_tier,
       COALESCE((ae.payload->>'numTurns')::int, 0),
       CASE WHEN COALESCE((ae.payload->>'isError')::boolean, false)
              OR COALESCE(ae.payload->>'subtype', 'success') <> 'success'
            THEN 'error' ELSE 'success' END,
       left(ae.payload->>'subtype', 64),
       ae.at - make_interval(secs => COALESCE((ae.payload->>'durationMs')::bigint, 0) / 1000.0),
       ae.at,
       true
  FROM agent_events ae
  JOIN tasks t ON t.id = ae.task_id
 WHERE ae.kind = 'result'
   AND NOT EXISTS (
         SELECT 1 FROM agent_runs r
          WHERE r.run_kind = 'task' AND r.task_id = ae.task_id AND NOT r.backfill
            AND ae.at >= r.at - (r.ended_at - r.started_at) - interval '5 minutes')
ON CONFLICT ON CONSTRAINT agent_runs_attempt_uniq DO NOTHING;

INSERT INTO agent_runs (
  task_id, prd_id, user_id, repo_id, phase, bucket, run_kind, review_round,
  session_id, resume, model, models,
  input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, tokens_known,
  cost_usd, duration_ms, verify_tier, num_turns, status, subtype,
  started_at, ended_at, backfill)
SELECT NULL, pe.prd_id, p.user_id, p.repo_id, pe.phase,
       CASE
         WHEN pe.phase = 'review' THEN 'review'
         WHEN pe.phase = 'triage' THEN 'triage'
         WHEN pe.phase IN ('implement', 'implement-reentry') OR pe.phase ~ '^fix-[0-9]+$' THEN 'implement'
         WHEN pe.phase IN ('verify', 'push') THEN pe.phase
         WHEN pe.phase = 'plan' THEN 'prd_plan'
         WHEN pe.phase = 'plan-review' THEN 'prd_review'
         WHEN pe.phase = 'recommend' THEN 'preview'
         ELSE 'other'
       END,
       'prd', false, NULL, false, NULL, '[]'::jsonb,
       0, 0, 0, 0, false,
       CASE WHEN pe.payload ? 'costUsd' AND COALESCE(pe.payload->>'driver', '') <> 'codex'
            THEN (pe.payload->>'costUsd')::numeric ELSE NULL END,
       COALESCE((pe.payload->>'durationMs')::bigint, 0),
       NULL,
       COALESCE((pe.payload->>'numTurns')::int, 0),
       CASE WHEN COALESCE((pe.payload->>'isError')::boolean, false)
              OR COALESCE(pe.payload->>'subtype', 'success') <> 'success'
            THEN 'error' ELSE 'success' END,
       left(pe.payload->>'subtype', 64),
       pe.at - make_interval(secs => COALESCE((pe.payload->>'durationMs')::bigint, 0) / 1000.0),
       pe.at,
       true
  FROM prd_events pe
  JOIN prds p ON p.id = pe.prd_id
 WHERE pe.kind = 'result'
   AND NOT EXISTS (
         SELECT 1 FROM agent_runs r
          WHERE r.run_kind = 'prd' AND r.prd_id = pe.prd_id AND NOT r.backfill
            AND pe.at >= r.at - (r.ended_at - r.started_at) - interval '5 minutes')
ON CONFLICT ON CONSTRAINT agent_runs_attempt_uniq DO NOTHING;
