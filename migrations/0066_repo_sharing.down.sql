-- 回滚 0066：删掉 repos.team_id（共享关系随之消失，所有仓库回到私有）。
DROP INDEX repos_team;
ALTER TABLE repos DROP COLUMN team_id;
