-- 0050_repo_vcs_platform — 仓库所在的代码平台实例（f07：GitLab / Gitea）
--
-- 为什么是 repos 上的两列：(平台, 实例地址) 是仓库的一个属性，两列只在一起
-- 才有意义，所以由同一条 CHECK 绑定、放在同一条迁移里（拆开会出现「有 gitlab
-- 平台、没有实例地址约束」的中间版本）。任务经 tasks.repo_id 继承平台：
-- repo_id 创建时钉死、这两列没有任何更新路径（store.UpdateRepo 的 SET 子句
-- 不含它们，PUT /api/repos/{id} 传了就 400），所以在途任务不可能换平台。
--
-- vcs_base_url 不由请求体提供：POST /api/repos 从该用户同平台凭据的
-- integrations.base_url（0051）复制一份钉死。
--
-- 为什么不回填：存量行取默认值 'github' + NULL，恰好满足约束，零回填
-- （AGENTS §6.2）。repos 是每用户几行的配置表，ADD CONSTRAINT 的扫描可接受。
--
-- repos_user_repo_unique (user_id, provider_repo) 不动：同一用户不能把同一
-- 路径同时登记在两个平台（docs/features/vcs-gitlab-gitea.md §4）。
ALTER TABLE repos
  ADD COLUMN vcs_provider text NOT NULL DEFAULT 'github',
  ADD COLUMN vcs_base_url text;

ALTER TABLE repos ADD CONSTRAINT repos_vcs_platform_check CHECK (
  (vcs_provider = 'github' AND vcs_base_url IS NULL)
  OR (vcs_provider IN ('gitlab', 'gitea')
      AND vcs_base_url IS NOT NULL
      AND length(vcs_base_url) BETWEEN 8 AND 300)
);
