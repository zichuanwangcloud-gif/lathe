-- lathe:no-transaction
-- 0036_tasks_user_state_index.down.sql
--
-- DROP INDEX CONCURRENTLY：普通 DROP 会拿 ACCESS EXCLUSIVE 锁，回滚时同样
-- 阻塞 runner 对 tasks 的写入。CONCURRENTLY 不能进事务块，所以 down 也带
-- 非事务标记。失败不留残骸（要么索引还在，要么没了），重跑天然幂等。
DROP INDEX CONCURRENTLY IF EXISTS tasks_user_state;
