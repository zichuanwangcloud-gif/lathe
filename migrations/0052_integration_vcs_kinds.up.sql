-- 0052_integration_vcs_kinds — 放开 GitLab / Gitea 的四个凭据种类（f07）
--
-- 一件事：新凭据种类（含「平台令牌必须带实例地址」这条只对新种类成立的约束）。
-- CHECK 只能整句重建，与 0028 同形。webhook 密钥是投到 Lathe 的，不需要地址。
-- 不回填：新种类，没有存量。
ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_kind_check;
ALTER TABLE integrations ADD CONSTRAINT integrations_kind_check
  CHECK (kind IN (
    'linear', 'linear_webhook', 'github', 'github_webhook',
    'gitlab',          -- GitLab 个人访问令牌
    'gitlab_webhook',  -- GitLab 项目 webhook 的 Secret Token
    'gitea',           -- Gitea 访问令牌
    'gitea_webhook',   -- Gitea 仓库 webhook 的 Secret
    'linear_oauth', 'github_app', 'cloudrouter'   -- 既有预留
  ));

ALTER TABLE integrations ADD CONSTRAINT integrations_vcs_base_url_required
  CHECK (kind NOT IN ('gitlab', 'gitea') OR base_url IS NOT NULL);
