-- 回滚 0052：收回四个新凭据种类。
--
-- 必须先删存量行，否则重建 CHECK 会因违反约束失败（同 0028 down）。删掉的是
-- 可重新配置的凭据（令牌与 webhook 密钥都能在平台上重新生成）。
DELETE FROM integrations WHERE kind IN ('gitlab', 'gitlab_webhook', 'gitea', 'gitea_webhook');

ALTER TABLE integrations DROP CONSTRAINT integrations_vcs_base_url_required;
ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_kind_check;
ALTER TABLE integrations ADD CONSTRAINT integrations_kind_check
  CHECK (kind IN (
    'linear',
    'linear_webhook',
    'github',
    'github_webhook',
    'linear_oauth',
    'github_app',
    'cloudrouter'
  ));
