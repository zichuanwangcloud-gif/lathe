-- 0004_user_accounts.up.sql — 用户账号体系：密码、角色、会话、密码重置、SMTP
--
-- 背景：P0 的鉴权是单个 LATHE_ADMIN_TOKEN 环境变量换取内存会话，users 表
-- 只有 email 一列，由启动逻辑 upsert 出唯一一行。这次把它变成真正的账号体系：
-- 开放注册、两级角色、忘记密码走邮件。
--
-- 三个设计取舍：
--
-- 1. password_hash 可空。bcrypt 哈希算不出来于 SQL 里，所以内置超管这一行由
--    迁移建出、由启动逻辑补哈希。可空是这个两阶段初始化的必要条件。
--
-- 2. 会话落库而非留在内存。管理员禁用用户、代用户重置密码都必须「立刻」踢掉
--    对方的在线会话；内存 map 既不含用户身份，也无法按 user 批量失效。落库
--    同时让控制面回到无状态（重启不掉线）。
--
-- 3. sessions.id 与 password_reset_tokens.token_hash 存的都是 SHA-256 十六进制，
--    不是原值。拿到数据库转储也无法冒用会话或重置他人密码。

-- ---------------------------------------------------------------- users

ALTER TABLE users
  ADD COLUMN password_hash        text,        -- bcrypt；NULL = 尚未设定（等启动逻辑补）
  ADD COLUMN role                 text        NOT NULL DEFAULT 'member',
  ADD COLUMN disabled_at          timestamptz,  -- NULL = 启用。比布尔多留一个「何时禁的」
  ADD COLUMN must_change_password boolean     NOT NULL DEFAULT false,
  ADD COLUMN last_login_at        timestamptz,
  ADD COLUMN webhook_slug         text;        -- 每用户专属 webhook 回调地址

ALTER TABLE users ADD CONSTRAINT users_role_check
  CHECK (role IN ('admin', 'member'));

-- 部分唯一索引：第一步注册时就填 slug，但历史行为 NULL，普通 UNIQUE 虽然
-- 也允许多个 NULL，部分索引把「只约束已生成的那些」这个意图写明白。
--
-- 这一列第一步不读。现在就加是为了省掉第二步的回填迁移：那时已经有一批
-- 注册用户，要先 UPDATE 填值再两阶段置 NOT NULL。一列的成本换一次迁移。
CREATE UNIQUE INDEX users_webhook_slug_unique
  ON users (webhook_slug) WHERE webhook_slug IS NOT NULL;

-- 把 P0 时期 ensureUser() 建出的那条唯一记录升为管理员。
--
-- 只升最小 id：测试残留或人工插入的行不该被顺手提权。空表时这条是空操作
-- —— 全新安装由启动逻辑按 LATHE_ADMIN_EMAIL 建号，两条路径都收敛到
-- 「有且仅有一个内置管理员」。
--
-- 口令刻意不在这里播种。SQL 算不出 bcrypt，而硬编码一个已知明文的哈希
-- 等于把默认口令写进公开仓库 —— must_change_password 挡不住「抢在管理员
-- 第一次登录之前登进来」，那恰恰是服务刚起、没人盯着的最危险窗口。
UPDATE users
   SET role = 'admin', must_change_password = true
 WHERE id = (SELECT min(id) FROM users);

-- ---------------------------------------------------------------- sessions

CREATE TABLE sessions (
  id         text        PRIMARY KEY,  -- 会话令牌的 SHA-256 十六进制，非原值
  user_id    bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- 按用户批量失效（禁用、改密、代重置）走这个索引
CREATE INDEX sessions_user ON sessions (user_id);
-- 定期清理过期会话走这个
CREATE INDEX sessions_expires ON sessions (expires_at);

-- ---------------------------------------------------------------- 密码重置

CREATE TABLE password_reset_tokens (
  token_hash text        PRIMARY KEY,  -- 同样只存 SHA-256
  user_id    bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at timestamptz NOT NULL,
  used_at    timestamptz,              -- 非空 = 已用过，单次消费
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX password_reset_tokens_user ON password_reset_tokens (user_id);
CREATE INDEX password_reset_tokens_expires ON password_reset_tokens (expires_at);

-- ---------------------------------------------------------------- SMTP

-- 单行配置表：SMTP 是「这套系统怎么发信」，不是「某个人对某个外部系统的授权」。
--
-- 刻意不复用 integrations：那张表的主键语义是 (user_id, kind)，把一份全局配置
-- 挂到超管的 user_id 下会埋一颗雷 —— 第二步删掉那个账号会连带删掉全站发信能力。
-- 代价是这里自带一个 password_enc，复用的是 secret.Sealer（主密钥在库外），
-- 而不是 integrations 的行结构。
--
-- id 锁死为 1，用 upsert 写入，比「取最新一行」更难出错。
CREATE TABLE smtp_settings (
  id           smallint    PRIMARY KEY DEFAULT 1,
  host         text        NOT NULL,
  port         integer     NOT NULL DEFAULT 587,
  username     text        NOT NULL DEFAULT '',  -- 空 = 不做 AUTH（内网匿名中继）
  password_enc bytea,                            -- AES-256-GCM，与 secret_enc 同一把主密钥
  from_addr    text        NOT NULL,
  from_name    text        NOT NULL DEFAULT 'Lathe',
  tls_mode     text        NOT NULL DEFAULT 'starttls',
  verified_at  timestamptz,
  verify_error text,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT smtp_settings_singleton CHECK (id = 1),
  CONSTRAINT smtp_settings_port_check CHECK (port > 0 AND port <= 65535),
  CONSTRAINT smtp_settings_tls_mode_check CHECK (tls_mode IN ('starttls', 'tls', 'none'))
);

CREATE TRIGGER smtp_settings_updated_at BEFORE UPDATE ON smtp_settings
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();
