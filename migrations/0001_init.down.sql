-- 0001_init.down.sql — 回滚初始 schema
--
-- 顺序与建表相反，先删有外键依赖的表。

DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS verifications;
DROP TABLE IF EXISTS task_events;
DROP TABLE IF EXISTS tasks;
DROP TABLE IF EXISTS nodes;
DROP TABLE IF EXISTS repos;
DROP TABLE IF EXISTS integrations;
DROP TABLE IF EXISTS users;

DROP FUNCTION IF EXISTS set_updated_at();
