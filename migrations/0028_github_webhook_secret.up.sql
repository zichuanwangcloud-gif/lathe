-- 0028_github_webhook_secret — GitHub webhook 签名密钥入凭据表（A2：PR 评论回流）
--
-- PR review 意见经仓库 webhook 投到 /webhooks/github/{slug}，按用户验签：
-- 每个用户在设置页配自己的密钥，与 linear_webhook 同一形态（加密入库、
-- 按 (user_id, kind) 唯一）。只放开 CHECK 取值，不动表结构。
--
-- 选型说明：没有复用 0003 预留的 'github_app'。那是 GitHub App 形态
-- （App 私钥 + installation token），与「个人令牌 + 仓库 webhook 密钥」
-- 是两种凭据，混用一个 kind 会让将来真做 App 时无法区分存量数据。

ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_kind_check;
ALTER TABLE integrations ADD CONSTRAINT integrations_kind_check
  CHECK (kind IN (
    'linear',          -- Linear API token
    'linear_webhook',  -- Linear webhook 签名密钥
    'github',          -- GitHub token
    'github_webhook',  -- GitHub 仓库 webhook 签名密钥（PR 评论回流）
    'linear_oauth',    -- 预留：P2 多租户 OAuth
    'github_app',      -- 预留：P2 GitHub App
    'cloudrouter'      -- 预留：模型入口
  ));
