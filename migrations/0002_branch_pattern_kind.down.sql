-- 0002_branch_pattern_kind.down.sql — 回滚分支模式默认值

ALTER TABLE repos ALTER COLUMN branch_pattern SET DEFAULT 'fix/{key}-{slug}';

UPDATE repos SET branch_pattern = 'fix/{key}-{slug}'
WHERE branch_pattern = '{kind}/{key}-{slug}';
