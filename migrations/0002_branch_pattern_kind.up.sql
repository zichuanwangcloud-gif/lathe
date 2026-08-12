-- 0002_branch_pattern_kind.up.sql — 修正分支模式默认值
--
-- 0001 里的默认值写死 fix/ 前缀，会让 feature 与 hotfix 任务也生成
-- fix/ 开头的分支名，与 CloudRouter 的分支约定不符。
-- {kind} 占位符由 runner.RepoConfig.BranchName 展开为 fix/feature/hotfix。

ALTER TABLE repos ALTER COLUMN branch_pattern SET DEFAULT '{kind}/{key}-{slug}';

-- 迁移已有记录：只改仍是旧默认值的那些，用户自定义过的不动
UPDATE repos SET branch_pattern = '{kind}/{key}-{slug}'
WHERE branch_pattern = 'fix/{key}-{slug}';
