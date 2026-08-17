-- 0008_repo_exclude_dirs.up.sql — 仓库级验证扫描排除目录
--
-- runner.Pipeline.ExcludeDirs 自引入以来只有字段和消费方
-- （DetectLightProfile / DetectRegression），没有任何数据来源 ——
-- 配置层断供。CloudRouter 的 apps/console 是停止维护的遗留目录，
-- 基线代码本身过不了 go vet，全量扫描把每个任务都拖死在别人的
-- 存量问题上（2026-08-17 任务 #316）。设计注释本就写明"应由仓库
-- 配置排除"，这条迁移把配置层补上。
--
-- 语义：元素为相对仓库根的路径或纯目录名（findGoModules 两种都认），
-- 默认空数组 = 只用 DefaultExcludeDirs。

ALTER TABLE repos ADD COLUMN exclude_dirs text[] NOT NULL DEFAULT '{}';
