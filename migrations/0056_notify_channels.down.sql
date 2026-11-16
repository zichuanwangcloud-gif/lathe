-- 回滚 0056：删掉通知渠道表。
--
-- 触发器与索引随表走；set_updated_at() 是公共函数，不删。
-- 回滚代价：URL 与签名密钥只存在这张表里，回滚后用户需要重新配置渠道。
DROP TABLE notify_channels;
