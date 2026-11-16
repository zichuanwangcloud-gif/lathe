-- 0035_inbox_marks — 收件箱的视图状态（docs/features/inbox.md §3.2）
--
-- 收件箱事项本身不入库：它们从 tasks / task_events / prds 实时派生，
-- 任务与 PRD 状态的真相源仍是原表，这里不复制任何状态（不做第二份真相源）。
-- 这张表只存别处没有的那一位信息：某人对某条派生事项「读过 / 忽略过」。
--
-- item_key 是派生事项的稳定键（task:<id>:e<eventID> / prd:<id>:<kind>:r<round>…），
-- 同一主体再次进入等人状态时键会变，旧键的标记随下一次写入被剪掉，
-- 所以每人每个主体至多一行，表的量级不超过 tasks + prds。
CREATE TABLE inbox_marks (
  user_id      bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  item_key     text        NOT NULL,
  -- 主体二选一。带外键是为了主体删除时标记随之级联消失，
  -- 不留下指向不存在主体的孤儿标记（同 failure_digests 的 CASCADE 立场）。
  task_id      bigint      REFERENCES tasks(id) ON DELETE CASCADE,
  prd_id       bigint      REFERENCES prds(id)  ON DELETE CASCADE,
  read_at      timestamptz,
  dismissed_at timestamptz,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (user_id, item_key),
  CONSTRAINT inbox_marks_one_subject CHECK ((task_id IS NULL) <> (prd_id IS NULL)),
  -- 既没读也没忽略的行没有意义：「标为未读」= 删行（若未忽略）。
  CONSTRAINT inbox_marks_nonempty    CHECK (read_at IS NOT NULL OR dismissed_at IS NOT NULL),
  CONSTRAINT inbox_marks_key_len     CHECK (char_length(item_key) BETWEEN 1 AND 128)
);

CREATE TRIGGER inbox_marks_updated_at BEFORE UPDATE ON inbox_marks
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 外键级联删除的反查路径（否则删一个任务要全表扫 inbox_marks）。
CREATE INDEX inbox_marks_task ON inbox_marks (task_id) WHERE task_id IS NOT NULL;
CREATE INDEX inbox_marks_prd  ON inbox_marks (prd_id)  WHERE prd_id  IS NOT NULL;
