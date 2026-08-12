-- 0003_integration_secrets.down.sql — 回滚凭据加密存储
--
-- 注意：回滚会丢弃 secret_enc 里的凭据。回滚前若有本地加密凭据，
-- 需先在界面里重新配置为外部引用形态，否则集成将失效。

ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_has_secret;

DELETE FROM integrations WHERE token_ref IS NULL;

ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_kind_check;
ALTER TABLE integrations ADD CONSTRAINT integrations_kind_check
  CHECK (kind IN ('linear_oauth', 'github_app', 'cloudrouter'));

ALTER TABLE integrations ALTER COLUMN token_ref SET NOT NULL;

ALTER TABLE integrations
  DROP COLUMN secret_enc,
  DROP COLUMN account_name,
  DROP COLUMN verified_at,
  DROP COLUMN verify_error;
