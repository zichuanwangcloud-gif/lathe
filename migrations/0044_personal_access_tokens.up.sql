-- 0044_personal_access_tokens — 个人访问令牌（PAT）
--
-- 与 sessions 同理：库里只存 SHA-256 十六进制，明文只在创建响应里出现一次。
-- 不复用 AES-GCM，因为 PAT 不需要被还原 —— 平台自己签发、自己比对，
-- 哈希就够（见 internal/store/sessions.go 的 hashToken 注释）。
--
-- 软吊销：revoked_at 非空即失效，行保留供审计，由
-- PersonalAccessTokens.GC 在吊销/过期满 30 天后清理。
--
-- 号段说明：0044 是并行开发期的分段号（f05 线占 0044–0046），迁移器容忍空洞。
-- 新表、无回填、无并发写，可在事务块内执行。
CREATE TABLE personal_access_tokens (
  id           bigserial   PRIMARY KEY,
  user_id      bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         text        NOT NULL,          -- 用户起的名字，仅用于辨认
  token_hash   text        NOT NULL,          -- SHA-256(明文) 的十六进制
  prefix       text        NOT NULL,          -- 明文中 lathe_pat_ 之后的头 8 个字符，仅供列表页辨认；不构成凭据
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz,                   -- 节流写入（60 秒）
  expires_at   timestamptz,                   -- NULL = 永不过期
  revoked_at   timestamptz,                   -- 非空 = 已吊销；行保留以便审计
  use_count    bigint      NOT NULL DEFAULT 0, -- 近似值（节流窗口内不计）

  CONSTRAINT personal_access_tokens_name_len
    CHECK (octet_length(name) BETWEEN 1 AND 64),
  CONSTRAINT personal_access_tokens_hash_unique UNIQUE (token_hash)
);

-- 按用户列出/计数（每用户上限、设置页列表）
CREATE INDEX personal_access_tokens_user ON personal_access_tokens (user_id);
-- 供 PersonalAccessTokens.GC 扫描：两个部分索引分别对应 GC 的两个 OR 分支。
CREATE INDEX personal_access_tokens_expires ON personal_access_tokens (expires_at)
  WHERE expires_at IS NOT NULL;
CREATE INDEX personal_access_tokens_revoked ON personal_access_tokens (revoked_at)
  WHERE revoked_at IS NOT NULL;
