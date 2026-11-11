-- 0032_interjections — 运行中插话：人给正在跑的 agent 追加的一句话
--
-- 为什么单独一张表而不是复用 agent_events：
--   1. 两者的生命周期不同。agent_events 是 stdout 的提炼（append-only 的
--      「发生了什么」），而本表记的是「人要求什么」，它有一个**投递状态机**
--      （queued → delivering → delivered/failed），要能被人查到。
--   2. agent_events 的 kind CHECK 是「Digest 能产出的全集」（0014），插话
--      不是 stdout 的产物，混进去会打破那条不变的等式。
--   3. 审计视角不同：查 agent_events 是查「agent 做了什么」，查本表是查
--      「人干预过什么」。任务详情页要单独渲染这一段。
--
-- rev 是「这一行最后一次被写」的游标（前端增量轮询用，见 docs/features/interject.md）：
-- INSERT 取默认值，每次 UPDATE 由写入方显式 SET rev = nextval(...)。
-- 写入方都在 pg_advisory_xact_lock(<task_id>) 下取号并提交，所以同一任务内
-- rev 的大小顺序 = 提交顺序，游标不会跳过晚提交的小号。
-- （刻意不用触发器维护 rev：迁移只做 schema，AGENTS.md §6.4。）
CREATE SEQUENCE task_interjections_rev_seq;

CREATE TABLE task_interjections (
  id          bigserial   PRIMARY KEY,
  task_id     bigint      NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  user_id     bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  -- 人写的那句话，**已脱敏**（落库前过 runner 的两层脱敏，§3.7）。上限在 API 层
  -- （LATHE_INTERJECT_MAX_CHARS，默认 8192 个 rune），这里只做保底硬上限 16384。
  body        text        NOT NULL,
  -- 投递状态。text + CHECK 而非 enum（沿用 0001 的约定）。
  --   queued     已落库、尚未进入任何 agent 进程
  --   delivering 正在投递（实时写 stdin 中，或已拼进本轮 prompt、等本轮结束）
  --   delivered  已进入 agent 进程 —— 只代表「CLI 收到了」，不代表模型照做
  --   failed     不会进入模型；error 写明原因
  status      text        NOT NULL DEFAULT 'queued',
  -- failed 时是人读原因（CHECK 强制非空）；delivered 时可为 'no_replay_ack'（写入成功但没等到回显）。
  error       text,
  -- 投递时归属的 agent 会话 ID：事后能对上「这句话进了哪一轮」。
  session_id  text,
  -- 投递进的事件阶段（implement / fix-1 …），对应 agent_events.phase。
  phase       text,
  -- 实时投递时发给 CLI 的帧 uuid（回显确认用）；开场补投的行留 NULL。
  delivery_id uuid,
  created_at  timestamptz NOT NULL DEFAULT now(),
  delivered_at timestamptz,
  rev         bigint      NOT NULL DEFAULT nextval('task_interjections_rev_seq'),

  CONSTRAINT task_interjections_status_check
    CHECK (status IN ('queued', 'delivering', 'delivered', 'failed')),
  -- 时间戳与状态必须自洽：
  --   queued / delivering  没有 delivered_at，也没有 error
  --   delivered            有 delivered_at
  --   failed               有 error（人读原因）
  CONSTRAINT task_interjections_terminal_check CHECK (
    (status IN ('queued', 'delivering') AND delivered_at IS NULL AND error IS NULL)
    OR (status = 'delivered' AND delivered_at IS NOT NULL)
    OR (status = 'failed' AND error IS NOT NULL)
  ),
  CONSTRAINT task_interjections_body_len CHECK (length(body) <= 16384)
);

ALTER SEQUENCE task_interjections_rev_seq OWNED BY task_interjections.rev;

-- 查询模式一：前端增量轮询 —— 某任务 rev > R 的行，按 rev 升序。
CREATE INDEX task_interjections_task_rev ON task_interjections (task_id, rev);
-- 查询模式二：开场补投 / 收尾结算按任务捞 queued（常为空，扫部分索引即可）。
CREATE INDEX task_interjections_pending ON task_interjections (task_id)
  WHERE status = 'queued';
-- 查询模式三：启动清扫捞全表 delivering / queued（行数极少）。
CREATE INDEX task_interjections_open ON task_interjections (status)
  WHERE status IN ('queued', 'delivering');
