-- 回滚 0051：删列。此时 0052 已回滚、四类新凭据行已删，剩余行（linear/github）
-- 的 base_url 必为 NULL（API 对它们传 baseUrl 返回 400），删列不丢信息。
ALTER TABLE integrations DROP CONSTRAINT integrations_base_url_shape;
ALTER TABLE integrations DROP COLUMN base_url;
