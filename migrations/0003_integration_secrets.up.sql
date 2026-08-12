-- 0003_integration_secrets.up.sql — 凭据加密存储与验证状态
--
-- 背景：管理界面需要能配置 Linear / GitHub 凭据并验证连通性。
-- 凭据用 AES-256-GCM 加密后存 secret_enc，主密钥保存在数据库之外
-- （环境变量或独立文件），拿到数据库转储也解不出凭据。
--
-- token_ref 保留不动：将来接入外部 secret store 时，凭据引用写在那里，
-- secret_enc 置空。两种模式可以共存。

ALTER TABLE integrations
  ADD COLUMN secret_enc   bytea,        -- AES-256-GCM 密文（nonce 前置）
  ADD COLUMN account_name text,         -- 验证时拿到的账号名，便于确认配的是哪个账号
  ADD COLUMN verified_at  timestamptz,  -- 最后一次验证成功的时间
  ADD COLUMN verify_error text;         -- 最后一次验证失败的原因

-- token_ref 原为 NOT NULL，改为可空：本地加密模式下凭据在 secret_enc，
-- 没有外部引用可填。
ALTER TABLE integrations ALTER COLUMN token_ref DROP NOT NULL;

-- 放开 kind 取值：除 OAuth 形态外，还需要存 API token 形态与 webhook secret。
ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_kind_check;
ALTER TABLE integrations ADD CONSTRAINT integrations_kind_check
  CHECK (kind IN (
    'linear',          -- Linear API token
    'linear_webhook',  -- Linear webhook 签名密钥
    'github',          -- GitHub token
    'linear_oauth',    -- 预留：P2 多租户 OAuth
    'github_app',      -- 预留：P2 GitHub App
    'cloudrouter'      -- 预留：模型入口
  ));

-- 凭据与外部引用至少要有一个，避免存下一条什么都没有的记录
ALTER TABLE integrations ADD CONSTRAINT integrations_has_secret
  CHECK (secret_enc IS NOT NULL OR token_ref IS NOT NULL);
