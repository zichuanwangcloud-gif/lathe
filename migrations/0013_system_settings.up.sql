-- system_settings：管理员级的通用键值设置。
--
-- 首个消费方是预览环境的资源阈值闸门（internal/preview）：内存/磁盘
-- 占用率超过阈值时禁止一键起服务。用通用键值表而非专用表 —— 阈值
-- 就两个标量，专用表是过度设计；后续系统级开关（如成本面板口径）
-- 也能复用这张表。
CREATE TABLE system_settings (
  key         text        PRIMARY KEY,
  value       text        NOT NULL,
  updated_at  timestamptz NOT NULL DEFAULT now()
);
