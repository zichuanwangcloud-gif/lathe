-- 0059_schedules — 定时建单规则（docs/features/scheduled-issue.md）
--
-- 每条规则到点只做一件事：用属主自己的 Linear 凭据在 Linear 上建一张单。
-- 之后那张单和人手工建的单一样走 webhook → 接单闸门 → 分诊 → 验证。
-- 运行记录在 0060 的 schedule_runs。
--
-- 新的空表，不需要 CONCURRENTLY；存量数据不受影响。
CREATE TABLE schedules (
  id                bigserial PRIMARY KEY,
  user_id           bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name              text        NOT NULL,
  enabled           boolean     NOT NULL DEFAULT true,
  -- 启用（含新建、编辑、重新启用）时刻：补偿的基线下限，停用期间的时刻不补
  enabled_at        timestamptz NOT NULL DEFAULT now(),

  kind              text        NOT NULL,
  minute_of_day     int,                  -- daily/weekly
  weekdays          smallint,             -- weekly：bit0=周日 … bit6=周六
  interval_minutes  int,                  -- interval
  anchor_at         timestamptz,          -- interval
  timezone          text        NOT NULL DEFAULT 'UTC',

  linear_team_id    text        NOT NULL,
  linear_team_key   text        NOT NULL DEFAULT '',   -- 展示用
  intake_mode       text        NOT NULL DEFAULT 'assign',
  title_template    text        NOT NULL,
  body_template     text        NOT NULL DEFAULT '',
  priority          smallint    NOT NULL DEFAULT 0,

  next_fire_at      timestamptz,          -- 停用时为 NULL
  last_slot_at      timestamptz,          -- 最近一次已处理（含 skipped）的计划时刻
  consecutive_failures int      NOT NULL DEFAULT 0,
  last_error        text,
  paused_reason     text,                 -- 自动暂停原因；人重新启用时清空

  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT schedules_kind_check CHECK (kind IN ('daily', 'weekly', 'interval')),
  CONSTRAINT schedules_intake_check CHECK (intake_mode IN ('assign', 'label')),
  CONSTRAINT schedules_priority_check CHECK (priority BETWEEN 0 AND 4),
  CONSTRAINT schedules_shape_check CHECK (
    (kind = 'daily'    AND minute_of_day BETWEEN 0 AND 1439 AND weekdays IS NULL AND interval_minutes IS NULL)
 OR (kind = 'weekly'   AND minute_of_day BETWEEN 0 AND 1439 AND weekdays BETWEEN 1 AND 127 AND interval_minutes IS NULL)
 OR (kind = 'interval' AND minute_of_day IS NULL AND weekdays IS NULL
                       AND interval_minutes BETWEEN 60 AND 10080 AND interval_minutes % 60 = 0
                       AND anchor_at IS NOT NULL)),
  -- 双向等价：启用 ⇔ 有下次触发时刻。只写 `enabled OR next_fire_at IS NULL` 会放过
  -- enabled=true AND next_fire_at IS NULL —— 到期扫描 `next_fire_at <= $now` 对 NULL 为假，
  -- 这条规则界面上显示「启用」却永远不触发。写入时就挡掉。
  CONSTRAINT schedules_enabled_next CHECK (enabled = (next_fire_at IS NOT NULL)),
  CONSTRAINT schedules_user_name_unique UNIQUE (user_id, name)
);

CREATE TRIGGER schedules_updated_at BEFORE UPDATE ON schedules
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 调度器扫描到期规则
CREATE INDEX schedules_due ON schedules (next_fire_at) WHERE enabled;
CREATE INDEX schedules_owner ON schedules (user_id, id);
