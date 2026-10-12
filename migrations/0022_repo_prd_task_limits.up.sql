-- 0022_repo_prd_task_limits — 单任务量级的仓库级上限（docs/10-prd-template.md D10-5 / §4.2）
--
-- 消费方（铁律 3：每个可配置字段必须有消费方）：规划智能体的定稿自检
-- （docs/10 §6.1）。PRD 的 §8 任务拆分里每个任务带 estimate{lines,files}，
-- 任一任务超过本仓库的上限就拒绝进入 ready_for_review，除非人在 §10
-- 决策记录里显式留下「接受超限」并指向该任务。
--
-- 为什么是仓库级而不是全局常量：400 行 / 8 文件是 review 质量陡降的经验
-- 阈值（docs/10 §4.2），但它依赖仓库的代码密度与 review 习惯 —— 一个
-- 生成代码占多数的仓库和一个手写业务逻辑的仓库不该共用一个数。
--
-- 为什么给默认值而不是允许 NULL：没有「不限制」这个语义。拆分失控是
-- 规划管线最主要的风险面（D10-1 的代价），存量仓库必须直接落在出厂值上，
-- 而不是先进入一个不设限的状态等人来配。
--
-- CHECK (> 0)：非法值在写入时就拒绝，不留到运行期静默降级
-- （先例 LATHE_WORKTREE_TTL 的下限校验）。store.UpdateRepo 另有一层
-- 同口径校验，保证 API 层给出可读错误而不是裸 SQLSTATE 23514。
ALTER TABLE repos
  ADD COLUMN prd_task_max_lines int NOT NULL DEFAULT 400,
  ADD COLUMN prd_task_max_files int NOT NULL DEFAULT 8;

ALTER TABLE repos
  ADD CONSTRAINT repos_prd_task_max_lines_check CHECK (prd_task_max_lines > 0),
  ADD CONSTRAINT repos_prd_task_max_files_check CHECK (prd_task_max_files > 0);
