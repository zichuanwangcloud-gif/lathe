-- 存 Linear issue 的 UUID：重试与启动恢复都靠它重新定位 issue，
-- 不再把 issue key 当 UUID 用（retry 端点曾经的 bug）。
ALTER TABLE tasks ADD COLUMN linear_issue_id text;
