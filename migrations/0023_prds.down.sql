-- 回滚 0023：删表连带删掉索引与触发器。
-- set_updated_at() 是 0001 建的公共函数，别的表还在用，不能删。
DROP TABLE prds;
