-- 0016_flow_orchestration — 编排图内核：flows 表 + tasks 新列 + blocked_dep 状态
--
-- 设计依据：docs/07-prd-orchestration.md §4、docs/06-orchestration.md §2/§5/§6
-- 范围：仅覆盖 M1(编排内核)/M2(栈式PR)/M3(合并闭环)/M7(节点画像)。
-- 明确不做（留给后续里程碑，避免"字段无消费方"，见 05-roadmap.md §0 纪律）：
--   - F6 平台无关化相关列（tasks.key / tracker_provider / external_key 改名，M6）
--   - F1.3 画布布局列（pos_x/pos_y，M5，当前无 UI 消费方）
--   - flows.tracker_provider/tracker_scope（M1-M3/M7 范围内只有一种 tracker 在跑）

CREATE TABLE flows (
  id         bigserial PRIMARY KEY,
  user_id    bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  repo_id    bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  name       text        NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX flows_user ON flows (user_id);

ALTER TABLE tasks
  ADD COLUMN flow_id       bigint REFERENCES flows(id) ON DELETE SET NULL,
  ADD COLUMN depends_on    bigint REFERENCES tasks(id) ON DELETE SET NULL,
  ADD COLUMN depends_on_at text NOT NULL DEFAULT 'pr_open',
  ADD COLUMN base_ref      text,
  ADD COLUMN priority      int NOT NULL DEFAULT 0,
  ADD COLUMN profile       jsonb NOT NULL DEFAULT '{}'::jsonb,
  -- pr_number：GitHub PR number 目前只落在 task_events.payload（不可查询），
  -- F4.1 合并检测的轮询兜底（webhook 丢失时仍能收敛）需要按 (repo, pr_number)
  -- 遍历 pr_open 任务查 GitHub GetPR —— 没有可查询列就没法写这个轮询任务。
  ADD COLUMN pr_number     int;

ALTER TABLE tasks
  ADD CONSTRAINT tasks_depends_on_at_check
    CHECK (depends_on_at IN ('pr_open', 'merged')),
  -- 入度 ≤ 1 由 depends_on 是标量自引用列这一事实保证（06 §1）；
  -- 这条只挡"指向自己"这种退化环，多节点成环需要遍历祖先链，是应用层职责。
  ADD CONSTRAINT tasks_depends_on_not_self
    CHECK (depends_on IS DISTINCT FROM id);

CREATE INDEX tasks_flow            ON tasks (flow_id)    WHERE flow_id IS NOT NULL;
CREATE INDEX tasks_depends_on      ON tasks (depends_on) WHERE depends_on IS NOT NULL;
-- 调度就绪查询的主访问路径：WHERE state='queued' ORDER BY priority DESC, id
CREATE INDEX tasks_queued_priority ON tasks (priority DESC, id) WHERE state = 'queued';

-- blocked_dep：F2.3 失败传播的落点状态。
-- 入口 queued→blocked_dep，出口 blocked_dep→{queued（前驱恢复）, cancelled（人工中止）}。
ALTER TABLE tasks DROP CONSTRAINT tasks_state_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_state_check CHECK (state IN (
  'queued', 'triaging', 'blocked_spec', 'blocked_dep', 'awaiting_approval',
  'implementing', 'verifying', 'pr_open', 'review_feedback',
  'merged', 'failed', 'cancelled'
));
