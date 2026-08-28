-- 0016_flow_orchestration — 回滚
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_state_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_state_check CHECK (state IN (
  'queued', 'triaging', 'blocked_spec', 'awaiting_approval',
  'implementing', 'verifying', 'pr_open', 'review_feedback',
  'merged', 'failed', 'cancelled'
));

DROP INDEX IF EXISTS tasks_queued_priority;
DROP INDEX IF EXISTS tasks_depends_on;
DROP INDEX IF EXISTS tasks_flow;

ALTER TABLE tasks
  DROP CONSTRAINT IF EXISTS tasks_depends_on_not_self,
  DROP CONSTRAINT IF EXISTS tasks_depends_on_at_check;

ALTER TABLE tasks
  DROP COLUMN IF EXISTS pr_number,
  DROP COLUMN IF EXISTS profile,
  DROP COLUMN IF EXISTS priority,
  DROP COLUMN IF EXISTS base_ref,
  DROP COLUMN IF EXISTS depends_on_at,
  DROP COLUMN IF EXISTS depends_on,
  DROP COLUMN IF EXISTS flow_id;

DROP INDEX IF EXISTS flows_user;
DROP TABLE IF EXISTS flows;
