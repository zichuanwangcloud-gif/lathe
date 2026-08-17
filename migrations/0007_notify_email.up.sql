-- 0007_notify_email.up.sql — 个人通知邮箱
--
-- 通知类邮件（如任务状态通知）发往这里；NULL 表示回退用登录邮箱。
-- 密码重置邮件不受此列影响 —— 找回密码必须证明对账号邮箱的所有权，
-- 永远发往 users.email。

ALTER TABLE users ADD COLUMN notify_email text;
