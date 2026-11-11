-- 回滚 0065：删掉团队三张表（索引与触发器随 DROP TABLE 一并回收）。
--
-- 团队关系全部来自人的显式操作，本迁移不写业务数据；回滚后重新建团队即可。
-- 依赖顺序：0066 的 repos.team_id 外键引用 teams，回滚链会先执行 0066 的 down。
DROP TABLE team_invitations;
DROP TABLE team_members;
DROP TABLE teams;
