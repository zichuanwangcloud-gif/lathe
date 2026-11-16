-- 回滚 0062：删掉团队技能的版本表（0063 必须已先回滚；索引随表一起删）。
DROP TABLE team_skill_versions;
