-- 回滚 0050：有 GitLab/Gitea 仓库时【拒绝】，而不是删列或删行。
--
-- 直接删列会把它们静默变成 github 仓库，下一次派发会拿 git@github.com:<同路径>
-- 去推 —— 这是错误平台推送，比回滚失败严重得多。也不能像 0028 那样 DELETE：
-- repos 被 internal_issues/prds 以 ON DELETE RESTRICT 引用（0021、0023），
-- 且删仓库会级联删任务。空库/全 GitHub 库上精确还原 up 之前的 schema。
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM repos WHERE vcs_provider <> 'github') THEN
    RAISE EXCEPTION '0050 回滚被拒：仍有 GitLab/Gitea 仓库（%），请先在仓库页删除或改由人工处理',
      (SELECT string_agg(provider_repo, ', ') FROM repos WHERE vcs_provider <> 'github');
  END IF;
END $$;

ALTER TABLE repos DROP CONSTRAINT repos_vcs_platform_check;
ALTER TABLE repos DROP COLUMN vcs_base_url;
ALTER TABLE repos DROP COLUMN vcs_provider;
