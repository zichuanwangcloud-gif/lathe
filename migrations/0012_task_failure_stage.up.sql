-- tasks.failure_stage：机器可读的失败阶段代码（如 implement_run / verify_failed /
-- push），供智能重试做断点续跑决策。failure_reason 是给人看的自由文本，
-- 不适合做机器判定；stage code 集合由代码定义（internal/runner/stage.go），
-- 会随流水线演进而增加，故不加 CHECK 约束。
--
-- 语义约定：仅当 state = 'failed' 时有意义；重试转 queued 后保留旧值
-- （它正是本次重试的决策依据），任务成功后残留的旧值无含义。
ALTER TABLE tasks ADD COLUMN failure_stage text;
