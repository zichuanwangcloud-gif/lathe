-- 0004_user_accounts.down.sql — 回滚用户账号体系
--
-- 注意：回滚会丢弃全部密码哈希、角色、在线会话与 SMTP 配置。回滚后鉴权
-- 退回 LATHE_ADMIN_TOKEN 单口令形态，注册过的账号行还在但失去密码与角色，
-- 需要人工清理。回滚前请确认 LATHE_ADMIN_TOKEN 仍然可用，否则管理界面
-- 会彻底进不去。

DROP TABLE IF EXISTS smtp_settings;
DROP TABLE IF EXISTS password_reset_tokens;
DROP TABLE IF EXISTS sessions;

DROP INDEX IF EXISTS users_webhook_slug_unique;

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;

ALTER TABLE users
  DROP COLUMN password_hash,
  DROP COLUMN role,
  DROP COLUMN disabled_at,
  DROP COLUMN must_change_password,
  DROP COLUMN last_login_at,
  DROP COLUMN webhook_slug;
