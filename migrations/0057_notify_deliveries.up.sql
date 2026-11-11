-- 0057_notify_deliveries — 站外通知的 outbox（投递记录）
--
-- 为什么是 outbox 而不是「在 goroutine 里直接发」：站外渠道要重试、要按事件
-- 去重、要多节点不重复投递，这三件事都需要一条落库的记录。入队只是往这张
-- 旁路表插一行（失败只记日志），状态机看不到它 —— 通知永不参与判定。
--
-- 新表，不是大表：索引无需 CONCURRENTLY。
CREATE TABLE notify_deliveries (
  id          bigserial PRIMARY KEY,
  channel_id  bigint      NOT NULL REFERENCES notify_channels(id) ON DELETE CASCADE,
  -- 前五个与渠道开关列一一对应；test 是保存/重发时的测试消息（只由请求内同步发送）。
  event_type  text        NOT NULL,
  -- 去重键：task:<id>:<to_state>:<updated_at> / prd:<id>:ready_for_review:<updated_at> / test:<uuid>。
  -- 用「转移那一刻的 updated_at」而不是当前时间：同一转移重放得到同一个键。
  event_key   text        NOT NULL,
  target_task_id bigint REFERENCES tasks(id) ON DELETE CASCADE,
  target_prd_id  bigint REFERENCES prds(id)  ON DELETE CASCADE,
  -- 转移时刻的快照，worker 渲染的**全部**输入（不回读 tasks / prds）。
  -- reason 是 runner 侧已脱敏、截断过的文本；刻意不存路径、分支、会话 ID、凭据。
  payload     jsonb       NOT NULL DEFAULT '{}',
  status      text        NOT NULL DEFAULT 'pending',
  -- 已经失败过的发送次数：只在一次失败被记下时 +1。
  attempts    int         NOT NULL DEFAULT 0,
  -- 退避：worker 只领 next_attempt_at <= 传入 now 的行。
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  -- 领用租约：sending 且 claimed_at 早于 now - lease 视为死掉的尝试。
  claimed_at  timestamptz,
  last_error  text,
  sent_at     timestamptz,
  created_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT notify_deliveries_status_check
    CHECK (status IN ('pending', 'sending', 'sent', 'skipped', 'gave_up')),
  CONSTRAINT notify_deliveries_event_check
    CHECK (event_type IN ('failed','blocked_spec','awaiting_approval','pr_open',
                          'prd_ready_for_review','test'))
);

-- 去重的唯一保证（多节点/重启/重复调用）
CREATE UNIQUE INDEX notify_deliveries_dedup ON notify_deliveries (channel_id, event_key);
-- worker 领取热路径
CREATE INDEX notify_deliveries_due ON notify_deliveries (next_attempt_at)
  WHERE status = 'pending';
-- 死掉的 sending 回收
CREATE INDEX notify_deliveries_stale ON notify_deliveries (claimed_at)
  WHERE status = 'sending';
