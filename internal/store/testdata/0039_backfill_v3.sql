-- 第三版 0039（错误示范，只用于 AC-6 红证据，不进 migrations/）：截止条件拿节点时钟
-- r.started_at 比库时钟 ae.at，节点偏快超过一次运行时长时重复计账、偏慢时漏补。
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
            AND r.started_at <= ae.at)
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
            AND r.started_at <= pe.at)
ON CONFLICT ON CONSTRAINT agent_runs_attempt_uniq DO NOTHING;
