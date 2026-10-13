-- 回滚 0028：收回 'github_webhook' 取值。
--
-- 必须先删存量行，否则重建 CHECK 会因违反约束失败。删掉的只是 GitHub
-- webhook 签名密钥（可在 GitHub 仓库设置里重新生成后再配），回滚后
-- /webhooks/github/{slug} 一律 404（解析不出密钥）。
DELETE FROM integrations WHERE kind = 'github_webhook';

ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_kind_check;
ALTER TABLE integrations ADD CONSTRAINT integrations_kind_check
  CHECK (kind IN (
    'linear',
    'linear_webhook',
    'github',
    'linear_oauth',
    'github_app',
    'cloudrouter'
  ));
