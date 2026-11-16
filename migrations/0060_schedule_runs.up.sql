-- 0060_schedule_runs — 每个计划时刻的触发记录（docs/features/scheduled-issue.md）
--
-- (schedule_id, scheduled_for) 唯一 = 多节点、重启、手动运行下同一计划时刻只触发一次。
-- 「这条 pending 现在归谁执行」由行上的租约三列决定，不另建租约表。
--
-- 「建了单但没被接单」不另存字段：runs 列表从 tasks 只读派生，
-- 不多出一个和 tasks 状态打架的真相源。
CREATE TABLE schedule_runs (
  id                bigserial PRIMARY KEY,
  schedule_id       bigint      NOT NULL REFERENCES schedules(id) ON DELETE CASCADE,
  user_id           bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,  -- 冗余，供属主过滤
  scheduled_for     timestamptz NOT NULL,
  trigger           text        NOT NULL,           -- schedule | catchup | manual
  status            text        NOT NULL DEFAULT 'pending',
  reason            text,                           -- missed_window | owner_disabled | schedule_disabled | interrupted
  missed_count      int         NOT NULL DEFAULT 0, -- 本次补偿合并掉的更早时刻数
  linear_issue_uuid uuid        NOT NULL,           -- 认领时预生成，issueCreate.input.id
  linear_issue_id   text,
  linear_issue_identifier text,
  linear_issue_url  text,
  error             text,                           -- 已脱敏、已按 rune 截断
  attempts          int         NOT NULL DEFAULT 0,
  node              text        NOT NULL DEFAULT '',-- 最初认领节点（LATHE_NODE_NAME），排障用
  -- 执行租约：pending 行必有；终态行保留最后一个持有者，供排障
  lease_token       uuid,                           -- 一轮 tick（或一次立即运行）一个 token
  lease_node        text,                           -- 当前/最后持有者节点
  lease_until       timestamptz,                    -- 数据库时钟；心跳续约；< now() 即可被回收
  create_sent_at    timestamptz,                    -- 发出 issueCreate 前写入；NULL = 从未发出过，回收时按全新执行
  created_at        timestamptz NOT NULL DEFAULT now(),
  finished_at       timestamptz,

  CONSTRAINT schedule_runs_trigger_check CHECK (trigger IN ('schedule', 'catchup', 'manual')),
  CONSTRAINT schedule_runs_status_check CHECK (status IN ('pending', 'created', 'failed', 'skipped')),
  CONSTRAINT schedule_runs_slot_unique UNIQUE (schedule_id, scheduled_for),
  CONSTRAINT schedule_runs_uuid_unique UNIQUE (linear_issue_uuid),
  -- pending 必须有人持有租约，否则它既不会被执行也不会被回收（回收条件是 lease_until < now()，NULL 为假）
  CONSTRAINT schedule_runs_pending_leased CHECK (status <> 'pending' OR (lease_token IS NOT NULL AND lease_until IS NOT NULL))
);

CREATE INDEX schedule_runs_history ON schedule_runs (schedule_id, scheduled_for DESC);
CREATE INDEX schedule_runs_pending ON schedule_runs (lease_until) WHERE status = 'pending';   -- 回收扫描
CREATE INDEX schedule_runs_lease ON schedule_runs (lease_token) WHERE status = 'pending';    -- 心跳续约
