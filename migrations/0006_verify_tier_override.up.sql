-- 0006_verify_tier_override.up.sql — 仓库级强制验证档位（docs/02-design.md §5.1）
--
-- 用户在 repo 配置里可强制指定 light/heavy，覆盖按 diff 改动面的自动归档。
-- NULL 表示自动判定。

ALTER TABLE repos ADD COLUMN verify_tier_override text;

ALTER TABLE repos ADD CONSTRAINT repos_verify_tier_override_check
  CHECK (verify_tier_override IS NULL OR verify_tier_override IN ('light', 'heavy'));
