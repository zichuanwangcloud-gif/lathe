-- 0056_notify_channels — 站外通知渠道（Slack incoming webhook / 飞书自定义机器人）
--
-- 每用户每种渠道一行。为什么单开表而不是往 integrations 塞两个 kind：
-- integrations 的语义是「一种凭据」，而渠道需要 5 个事件开关 + 总开关 +
-- 测试结果列；把这些「只对通知有意义」的列加到 integrations，会让那张表的
-- 所有其他读者都要解释它们为什么是 NULL。
--
-- webhook URL 本身就是凭据（Slack 的路径里带 token），与其他凭据同规格：
-- AES-256-GCM 加密入库、主密钥在库外，**没有明文列**。
--
-- 新表，不是大表：索引无需 CONCURRENTLY。
CREATE TABLE notify_channels (
  id             bigserial PRIMARY KEY,
  user_id        bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  -- slack = Slack incoming webhook；feishu = 飞书自定义机器人（也兼容 Lark 域名）
  kind           text        NOT NULL,
  url_enc        bytea       NOT NULL,
  -- 飞书「签名校验」密钥；Slack 不用，为 NULL。
  sign_enc       bytea,
  -- 总开关。false 时不产生投递（已排队的发送前复查，跳过）。
  enabled        boolean     NOT NULL DEFAULT true,
  -- 按事件类型开关。默认全开：嫌吵的人自己关，而不是由我们替他决定哪条不重要。
  event_failed            boolean NOT NULL DEFAULT true,
  event_blocked_spec      boolean NOT NULL DEFAULT true,
  event_awaiting_approval boolean NOT NULL DEFAULT true,
  event_pr_open           boolean NOT NULL DEFAULT true,
  event_prd_ready         boolean NOT NULL DEFAULT true,
  -- 最近一次**完成了的**测试消息结果：notify_deliveries 里 test 行的派生缓存，
  -- 只由 FinishTest 在同一事务里写（界面展示用，不参与投递）。
  test_ok        boolean,
  test_error     text,
  test_at        timestamptz,
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT notify_channels_kind_check CHECK (kind IN ('slack', 'feishu')),
  CONSTRAINT notify_channels_user_kind_unique UNIQUE (user_id, kind)
);

CREATE TRIGGER notify_channels_updated_at BEFORE UPDATE ON notify_channels
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();
