-- 0051_integration_base_url — 凭据的「实例地址」（f07：自建 GitLab / Gitea）
--
-- 一件事：凭据有实例地址这一列（列 + 它自身的值域约束）。非密，明文列。
-- 写入方只有 PUT /api/integrations/{gitlab,gitea}（API 层校验 scheme/形状，
-- 去尾斜杠后入库）；读取方：保存时验证、仓库登记时复制进 repos.vcs_base_url、
-- 派发时与仓库钉死值比对（不一致即拒绝，token 不发往别的实例）。
--
-- 不回填：存量凭据都是 linear/github，没有实例地址的概念，NULL 即正确值。
-- 不为 github 强制 base_url IS NULL：给将来的 GitHub Enterprise 留空间。
ALTER TABLE integrations ADD COLUMN base_url text;
ALTER TABLE integrations ADD CONSTRAINT integrations_base_url_shape
  CHECK (base_url IS NULL OR length(base_url) BETWEEN 8 AND 300);
