-- 0006_verify_tier_override.down.sql

ALTER TABLE repos DROP CONSTRAINT IF EXISTS repos_verify_tier_override_check;
ALTER TABLE repos DROP COLUMN IF EXISTS verify_tier_override;
