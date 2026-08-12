-- 0001_init.up.sql — Lathe 初始 schema
--
-- 设计依据：docs/02-design.md §4
-- 约定：
--   * 状态类字段用 text + CHECK 而非 Postgres enum —— 状态集合会随分期演进，
--     CHECK 约束改起来比 ALTER TYPE 干净。
--   * 所有密钥只存 token_ref（指向外部 secret store），不明文入库。
--   * task_events 是 append-only 事件流，任务状态可由它完整重放。

-- ---------------------------------------------------------------- 通用：updated_at
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
  NEW.updated_at = now();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ---------------------------------------------------------------- users
CREATE TABLE users (
  id         bigserial PRIMARY KEY,
  email      text        NOT NULL UNIQUE,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER users_updated_at BEFORE UPDATE ON users
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------- integrations
-- 每用户对每种外部系统的授权。P0 用静态 token，P2 换 OAuth / GitHub App。
CREATE TABLE integrations (
  id                  bigserial PRIMARY KEY,
  user_id             bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  kind                text        NOT NULL,
  external_account_id text,
  token_ref           text        NOT NULL,   -- 指向 secret store，非明文
  scopes              text[]      NOT NULL DEFAULT '{}',
  expires_at          timestamptz,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT integrations_kind_check
    CHECK (kind IN ('linear_oauth', 'github_app', 'cloudrouter')),
  CONSTRAINT integrations_user_kind_unique UNIQUE (user_id, kind)
);

CREATE TRIGGER integrations_updated_at BEFORE UPDATE ON integrations
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------- repos
CREATE TABLE repos (
  id                bigserial PRIMARY KEY,
  user_id           bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider_repo     text        NOT NULL,               -- 如 Clouditera/CloudRouter
  default_branch    text        NOT NULL DEFAULT 'dev', -- fix/feature 的分叉基线
  hotfix_base       text        NOT NULL DEFAULT 'main',-- hotfix 的分叉基线
  protected_branches text[]     NOT NULL DEFAULT '{dev,test,main}',
  branch_pattern    text        NOT NULL DEFAULT 'fix/{key}-{slug}',
  verify_profile_ref text,                              -- 验证配置（见 §5）
  dep_strategy      text        NOT NULL DEFAULT 'pnpm-store',
  gate_mode         text        NOT NULL DEFAULT 'direct',
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT repos_dep_strategy_check
    CHECK (dep_strategy IN ('pnpm-store', 'none')),
  CONSTRAINT repos_gate_mode_check
    CHECK (gate_mode IN ('direct', 'guarded', 'plan-first', 'manual')),
  CONSTRAINT repos_user_repo_unique UNIQUE (user_id, provider_repo)
);

CREATE TRIGGER repos_updated_at BEFORE UPDATE ON repos
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------- nodes
-- 节点能力声明与心跳。P0 只有一个 local 节点，P3 横向扩展时按 capabilities 匹配。
CREATE TABLE nodes (
  id                bigserial PRIMARY KEY,
  name              text        NOT NULL UNIQUE,
  capabilities      jsonb       NOT NULL DEFAULT '{}',  -- {docker,cpu,mem_mb,disk_mb,repos_cached[]}
  status            text        NOT NULL DEFAULT 'offline',
  last_heartbeat_at timestamptz,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT nodes_status_check
    CHECK (status IN ('online', 'draining', 'offline'))
);

CREATE TRIGGER nodes_updated_at BEFORE UPDATE ON nodes
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------- tasks
CREATE TABLE tasks (
  id                bigserial PRIMARY KEY,
  user_id           bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  repo_id           bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  linear_issue_key  text        NOT NULL,               -- 如 CR-1326
  state             text        NOT NULL DEFAULT 'queued',
  gate_mode         text        NOT NULL DEFAULT 'direct',
  task_kind         text,                               -- fix | feature | hotfix
  verify_tier       text,                               -- §5.1 在 diff 产出后判定
  agent_session_id  text,                               -- ★ review 二轮 --resume 用
  worktree_path     text,
  branch_name       text,
  pr_url            text,
  failure_reason    text,
  node_id           bigint      REFERENCES nodes(id) ON DELETE SET NULL,
  lease_expires_at  timestamptz,                        -- 租约到期即重新派发
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT tasks_state_check CHECK (state IN (
    'queued', 'triaging', 'blocked_spec', 'awaiting_approval',
    'implementing', 'verifying', 'pr_open', 'review_feedback',
    'merged', 'failed', 'cancelled'
  )),
  CONSTRAINT tasks_gate_mode_check
    CHECK (gate_mode IN ('direct', 'guarded', 'plan-first', 'manual')),
  CONSTRAINT tasks_kind_check
    CHECK (task_kind IS NULL OR task_kind IN ('fix', 'feature', 'hotfix')),
  CONSTRAINT tasks_verify_tier_check
    CHECK (verify_tier IS NULL OR verify_tier IN ('light', 'heavy'))
);

CREATE TRIGGER tasks_updated_at BEFORE UPDATE ON tasks
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 同一 issue 不允许有两个「活着的」任务；已终结的历史任务不受限，
-- 因此 issue 重开后可以合法地再建一个任务。
CREATE UNIQUE INDEX tasks_one_active_per_issue
  ON tasks (repo_id, linear_issue_key)
  WHERE state NOT IN ('merged', 'failed', 'cancelled');

-- 调度器扫可领取任务
CREATE INDEX tasks_state_created ON tasks (state, created_at);
-- 租约回收扫超时任务
CREATE INDEX tasks_lease ON tasks (lease_expires_at) WHERE lease_expires_at IS NOT NULL;

-- ---------------------------------------------------------------- task_events
-- append-only 事件流：任务状态可由此完整重放，便于排障与审计。
CREATE TABLE task_events (
  id         bigserial PRIMARY KEY,
  task_id    bigint      NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  from_state text,                                      -- NULL 表示任务创建
  to_state   text        NOT NULL,
  actor      text        NOT NULL,                      -- system | user:<id> | node:<name>
  payload    jsonb       NOT NULL DEFAULT '{}',
  at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX task_events_task_at ON task_events (task_id, at);

-- ---------------------------------------------------------------- verifications
-- 每个验证步骤一行。heavy 档的 repro_fail → repro_pass 是"红-绿证明"的落痕。
CREATE TABLE verifications (
  id          bigserial PRIMARY KEY,
  task_id     bigint      NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  tier        text        NOT NULL,
  step        text        NOT NULL,
  status      text        NOT NULL,
  log_ref     text,                                     -- 日志存储引用
  duration_ms bigint,
  at          timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT verifications_tier_check CHECK (tier IN ('light', 'heavy')),
  CONSTRAINT verifications_step_check CHECK (step IN (
    'build', 'lint', 'typecheck',
    'repro_fail', 'repro_pass', 'regression'
  )),
  CONSTRAINT verifications_status_check
    CHECK (status IN ('passed', 'failed', 'skipped', 'error'))
);

CREATE INDEX verifications_task ON verifications (task_id, at);

-- ---------------------------------------------------------------- webhook_deliveries
-- Linear webhook 幂等去重：先落库再处理，重投递直接命中主键冲突。
CREATE TABLE webhook_deliveries (
  delivery_id text        PRIMARY KEY,
  source      text        NOT NULL DEFAULT 'linear',
  received_at timestamptz NOT NULL DEFAULT now(),
  processed_at timestamptz,
  error       text
);

CREATE INDEX webhook_deliveries_received ON webhook_deliveries (received_at);
