-- 0054_yunxiao_connections —— 每用户一条云效连接（f08）
--
-- 设计依据：docs/features/tracker-yunxiao.md §3.1 / §3.5。
-- 不复用 integrations 表：云效除两个密钥外还有 7 个非密字段，integrations
-- 一行一个 token 的形态装不下；且并行线都要整句重建 integrations_kind_check，
-- 后合入的会静默抹掉先合入的取值。代价：凭据存储有了第二个位置（加密口径
-- 复用同一个 secret.Sealer）。

CREATE TABLE yunxiao_connections (
  user_id            bigint      PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  endpoint           text        NOT NULL DEFAULT 'https://openapi-rdc.aliyuncs.com',
  organization_id    text,                            -- NULL/空 = Region 版（路径不带组织段）
  token_enc          bytea       NOT NULL,            -- 个人访问令牌，AES-256-GCM（nonce 前置）
  webhook_secret_enc bytea,                           -- X-Projex-Signature 共享密钥；NULL = webhook 不可用
  account_id         text,                            -- GetUserByToken.id：「指派给我」判定用
  account_name       text,
  trigger_label      text        NOT NULL DEFAULT '', -- 空 = 标签接单关闭
  status_started     text        NOT NULL DEFAULT '', -- 状态显示名；空 = 该时刻不同步
  status_pr_open     text        NOT NULL DEFAULT '',
  status_merged      text        NOT NULL DEFAULT '',
  status_cancel      text        NOT NULL DEFAULT '', -- 云效改到此状态 → 取消在途任务；空 = 关闭
  verified_at        timestamptz,
  verify_error       text,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now()
);

CREATE TRIGGER yunxiao_connections_updated_at BEFORE UPDATE ON yunxiao_connections
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();
