-- 0023_prds — 模糊任务与它的 PRD（docs/10-prd-template.md §1 生命周期）
--
-- 为什么模糊任务不进 tasks 表：tasks 的每一行都必须能被执行管线领走
-- （external_key NOT NULL、必须挂在工单上、状态机通向 pr_open）。模糊
-- 任务的产物是一份要人签字的文档，不是 PR；硬塞进 tasks 就得给状态机
-- 开一条绕过 verifying 的旁路，那是拆产品根基（铁律 1）。
-- 模糊任务 = prds 表的一行；它拆出来的正式任务才进 tasks。
--
-- 为什么 document 整存 jsonb 而不拆十几张表：
-- 1. 模板的节形状会随变体（defect/feature/refactor）演进，拆表意味着
--    docs/10 改一次模板就要加一条迁移。
-- 2. 对话协议是「每轮交付完整 PRD 快照」（D10-7），本就是整份替换，
--    没有按节增量更新的需求。
-- 代价：§6.1「机器可判」的结构校验落在 Go 层，不靠 CHECK 约束。这是自愿
-- 的取舍 —— 校验规则本身（禁用词表、覆盖矩阵无孤儿）根本不是 SQL 能表达的。
--
-- original_input 不可编辑：它是所有 🤖 推断的根（docs/10 §0 元信息）。
-- 允许改原文等于允许事后重写前提，之前几轮的推断就全部失去依据。
-- 数据库层只保证 NOT NULL，「不可编辑」由 store 层不提供 UPDATE 路径实现。
CREATE TABLE prds (
  id         bigserial PRIMARY KEY,
  user_id    bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  -- 唯一目标仓库（docs/10 §0：跨仓库不在 v1 范围）。
  -- RESTRICT 与 issues 一致：名下还有 PRD 的仓库不许删。
  repo_id    bigint NOT NULL REFERENCES repos(id) ON DELETE RESTRICT,
  -- 接续某份已 converted/abandoned 的 PRD（D10-4：approved 后不可变，
  -- 漏了开新 PRD 并引用旧的）。SET NULL：被引用的 PRD 删了不连带删新的。
  ref_prd_id bigint REFERENCES prds(id) ON DELETE SET NULL,

  prd_type   text NOT NULL,
  state      text NOT NULL DEFAULT 'drafting',
  -- 当前对话轮次，从 0 起（建好还没跑第一轮）。
  round      int  NOT NULL DEFAULT 0,

  original_input text NOT NULL,

  -- 完整 PRD 快照（模板 §2 结构 + 节状态标记 ✅🤖❓）。
  document      jsonb,
  -- §4.1 结构化任务块，定稿后才有；一键生成只读它，不再经 LLM 解释。
  task_blocks   jsonb,
  -- 附录 C：对抗复核报告原样 + 逐条处置。
  review_report jsonb,

  -- processing 是运行态而非状态机的一个状态（docs/10 §1.1 没有它）：
  -- 它防的是「同一份 PRD 被并发起两轮对话」，用 CAS false→true 抢占。
  -- 放进状态机会让转移表里混进与生命周期无关的边。
  -- 僵死兜底：processing_started_at 超阈值视为死掉的轮次，读时顺手 CAS 回
  -- false，不设后台清道夫（多一个常驻 goroutine 不值）。
  processing            boolean NOT NULL DEFAULT false,
  processing_started_at timestamptz,

  -- 一键生成产出的编排图（docs/10 §4.3）。SET NULL：图被删不影响 PRD 记录。
  generated_flow_id bigint REFERENCES flows(id) ON DELETE SET NULL,

  approved_at  timestamptz,
  converted_at timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT prds_type_check
    CHECK (prd_type IN ('defect', 'feature', 'refactor')),
  CONSTRAINT prds_state_check
    CHECK (state IN ('drafting', 'awaiting_answers', 'ready_for_review',
                     'approved', 'converted', 'abandoned')),
  CONSTRAINT prds_round_check CHECK (round >= 0),
  -- 自引用无意义，挡掉最省事的一种脏数据。
  CONSTRAINT prds_ref_not_self CHECK (ref_prd_id IS DISTINCT FROM id)
);

CREATE TRIGGER prds_updated_at BEFORE UPDATE ON prds
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 列表页按属主 + 状态筛，最近更新排前（同 issues_owner 口径）。
CREATE INDEX prds_owner ON prds (user_id, state, updated_at DESC);
