-- 0017_repo_baseline_dir.up.sql — 仓库级基线目录
--
-- 背景：部分仓库的基线分支已经在本机某个目录常驻跑着开发环境（如
-- /opt/CloudRouter 的 `pnpm up`：中间件常驻 docker，应用进程常驻 pm2）。
-- 任务预览/worktree 起服务时没必要每次都重新建一套 per-task 中间件容器——
-- 直接连基线目录已经在跑的中间件即可。
--
-- NULL = 未配置基线目录，现有行为不变（向后兼容）。不强制校验目录内容与
-- default_branch 一致，只登记事实，检测时才顺带报告分支是否一致。

ALTER TABLE repos ADD COLUMN baseline_dir text;
