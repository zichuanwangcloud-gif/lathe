-- 0007_notify_email.down.sql — 移除个人通知邮箱

ALTER TABLE users DROP COLUMN notify_email;
