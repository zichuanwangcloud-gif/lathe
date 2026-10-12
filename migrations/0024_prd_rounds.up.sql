-- 0024_prd_rounds — 多轮对话历史（docs/10 §1.3 对话协议 A–E）
--
-- 为什么需要独立表而不是复用 tasks.agent_session_id：那是【单列】，
-- SetSessionID 每次覆盖，只留最近一次 session，不是历史。而规划管线的
-- 核心形态就是多轮（D10-7），每轮的人类回答、提出的问题、当轮快照都得留下：
-- 1. 下一轮的 prompt 要显式带上历史决策（不 --resume，见下）。
-- 2. §10 决策记录不许重问已拍板的问题 —— 靠的就是这份历史。
-- 3. 人刷新页面要还能看到对话，纯内存的 RecommendOp 模式撑不住。
--
-- append-only：每轮一行，不更新不删除。轮次是既成事实，改写历史等于
-- 让「不重问已拍板问题」失去依据。
--
-- 为什么存 agent_session_id 但不靠它 --resume：只读 worktree 每轮用完即
-- 回收（D10-8），resume 的会话会以为自己还在一个已经删掉的目录里。
-- 每轮开新 session，历史靠 prompt 显式拼接（显式优于隐式）。
-- 这一列留着是为了排障时能把某一轮对上 agent_events 里的原始事件。
CREATE TABLE prd_rounds (
  id      bigserial PRIMARY KEY,
  prd_id  bigint NOT NULL REFERENCES prds(id) ON DELETE CASCADE,
  round   int    NOT NULL,
  -- 对话协议的五个阶段（docs/10 §1.3）。
  stage   text   NOT NULL,

  agent_session_id text,
  -- 本轮人的输入：第一轮为空（原文在 prds.original_input），
  -- 之后是对上一轮提问的回答。
  user_input       text NOT NULL DEFAULT '',
  -- 本轮智能体提出的问题（每轮 ≤ 3 个，D10-7）。
  questions        jsonb,
  -- 本轮结束时的完整 PRD 快照 —— 每轮都整份留档，便于人对比轮次间差异。
  document_snapshot jsonb,
  -- 本轮的「做了什么、最不确定什么」，2~3 行（docs/10 附录 A 对话摘要）。
  -- 独立成列而不是塞进快照：它是关于某一轮的元信息，不是 PRD 内容本身，
  -- 而快照会被下一轮整份替换。
  notes            text NOT NULL DEFAULT '',

  created_at timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT prd_rounds_stage_check
    CHECK (stage IN ('understand', 'explore', 'converge', 'split', 'finalize')),
  CONSTRAINT prd_rounds_round_check CHECK (round >= 1),
  -- 同一份 PRD 的轮次号唯一：并发起轮除了 prds.processing 的 CAS，
  -- 这里再兜一层，重复轮次直接撞唯一索引而不是悄悄写进去。
  CONSTRAINT prd_rounds_unique UNIQUE (prd_id, round)
);

-- 详情页按轮次顺序读整段对话。
CREATE INDEX prd_rounds_prd ON prd_rounds (prd_id, round);
