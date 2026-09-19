-- 0021_internal_issues —— 内置工单体系：issues / issue_comments 表 + tasks 平台无关化
--
-- 设计依据：docs/09-internal-issues.md §4（数据契约）
-- 范围：P1 核心闭环。明确不做（留给后续里程碑，避免"字段无消费方"，见
-- 05-roadmap.md §0 纪律）：
--   - issue_attachments 表（P2 附件，无消费方不建）
--   - flows.tracker_provider（P3 编排图接入内置工单时才需要）
--
-- 与 07 §4.2 草案的差异：external_key 保留 NOT NULL —— 内置工单体系落地后
-- 不存在"无 tracker 的裸任务"，任务永远挂在某个工单（Linear 或内置）上。

-- ---------------------------------------------------------------- issues
-- 内置工单。key 全局唯一（LT-<全局序列>），不按用户分段：人要念它、
-- 分支名要用它，跨用户撞 key 徒增心智负担。
CREATE TABLE issues (
  id          bigserial PRIMARY KEY,
  user_id     bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  -- 工单必绑仓库（09 §7.1 N3）：内置任务的归属仓库不再走
  -- "每用户第一个仓库"的随机解析，分支 pattern 也有着落。
  -- RESTRICT：名下还有工单的仓库不许删。
  repo_id     bigint      NOT NULL REFERENCES repos(id) ON DELETE RESTRICT,
  key         text        NOT NULL UNIQUE,        -- LT-1042
  title       text        NOT NULL,
  description text        NOT NULL DEFAULT '',
  state       text        NOT NULL DEFAULT 'open',
  priority    int         NOT NULL DEFAULT 0,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT issues_state_check
    CHECK (state IN ('open', 'in_progress', 'done', 'cancelled'))
);

CREATE TRIGGER issues_updated_at BEFORE UPDATE ON issues
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 列表页主访问路径：按属主 + 状态过滤，按更新时间倒序
CREATE INDEX issues_owner ON issues (user_id, state, updated_at DESC);

-- key 来源：全局序列，允许回滚空洞（序列语义本就如此，不为空洞补偿）
CREATE SEQUENCE issue_key_seq START 1;

-- ---------------------------------------------------------------- issue_comments
-- 评论区是 blocked_spec 提问回路的载体（09 §3 F2）：agent 的提问与人的
-- 补充写在同一条流里，重试时分诊重新拼进上下文，与 Linear 评论同构。
CREATE TABLE issue_comments (
  id         bigserial PRIMARY KEY,
  issue_id   bigint      NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
  -- 作者二选一：user_id 非空 = 人；actor 非空 = agent/系统
  -- （'task-<id>'，与 task_events 的 actor 前缀同源，UI 据此渲染
  -- "lathe · 任务 #id"并可跳任务详情）。
  user_id    bigint      REFERENCES users(id) ON DELETE SET NULL,
  actor      text,
  body       text        NOT NULL CHECK (length(body) BETWEEN 1 AND 10000),
  created_at timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT issue_comments_author_check
    CHECK ((user_id IS NOT NULL) <> (actor IS NOT NULL))
);

CREATE INDEX issue_comments_issue ON issue_comments (issue_id, id);

-- ---------------------------------------------------------------- tasks 平台无关化
ALTER TABLE tasks RENAME COLUMN linear_issue_key TO external_key;
ALTER TABLE tasks RENAME COLUMN linear_issue_id TO external_id;

-- 任务的需求来源平台。存量行由 DEFAULT 自动归 linear；
-- 'internal' 表示需求载体是内置 issues 表（external_key = LT-xxxx）。
ALTER TABLE tasks ADD COLUMN tracker_provider text NOT NULL DEFAULT 'linear';
ALTER TABLE tasks ADD CONSTRAINT tasks_tracker_provider_check
  CHECK (tracker_provider IN ('linear', 'internal'));

-- 替换 tasks_one_active_per_issue：同一工单（不分平台）只允许一个活任务。
-- 保留 repo_id 在索引里（07 §4.4 草案没有它）：与既有语义逐一对齐，
-- 且内置工单本身绑仓库，同一 key 不可能跨仓库出现，多这一列无害。
DROP INDEX tasks_one_active_per_issue;
CREATE UNIQUE INDEX tasks_one_active_per_item
  ON tasks (repo_id, tracker_provider, external_key)
  WHERE state NOT IN ('merged', 'failed', 'cancelled');
