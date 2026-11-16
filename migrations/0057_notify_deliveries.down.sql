-- 回滚 0057：删掉 outbox。
--
-- 索引随表走。DROP TABLE 语义上不可逆（投递历史没了），但这是唯一「完全撤销」
-- 的形态；渠道配置在 0056 的表里，不受影响。
DROP TABLE notify_deliveries;
