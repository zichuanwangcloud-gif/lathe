-- 回滚 0032：整表丢弃。up 没有改动任何既有表，因此可完全撤销；
-- 丢掉的只有「人插过什么话」这段审计，agent_events 不受影响。
-- 序列 OWNED BY 该列，随表一起删；IF EXISTS 那句是防御性的显式收尾。
DROP TABLE task_interjections;
DROP SEQUENCE IF EXISTS task_interjections_rev_seq;
