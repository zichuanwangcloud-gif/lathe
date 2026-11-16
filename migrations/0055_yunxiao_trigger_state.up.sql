-- 0055_yunxiao_trigger_state —— 云效接单边沿检测的快照（f08）
--
-- 设计依据：docs/features/tracker-yunxiao.md §3.6。云效 webhook 没有
-- Linear 的 updatedFrom，只能记下「上次看到时是否指派给我 / 是否带触发
-- 标签」，false→true 才接单。默认值 (false,false) 即「首见占位行」的语义：
-- ObserveTrigger 先 INSERT … ON CONFLICT DO NOTHING 占位、再 FOR UPDATE
-- 锁行读旧值，并发首见也只有一个上升沿。
--
-- 表小（每个被投递过的工作项一行），不需要 CONCURRENTLY。

CREATE TABLE yunxiao_trigger_state (
  user_id     bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  workitem_id text        NOT NULL,
  assigned    boolean     NOT NULL DEFAULT false,
  labeled     boolean     NOT NULL DEFAULT false,
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, workitem_id)
);
