-- 0030_failure_digests — 跨任务失败记忆（docs/05-roadmap.md §3.6 第 16 条）
--
-- 问题：任务失败后只留下 tasks.failure_reason / failure_stage 两列，且它们
-- 会被下一次重试覆盖。同 issue 再次派发时，agent 对「上次死在哪」一无所知
-- —— 它可能又踩同一个坑，或者（更常见）在上次已经证明走不通的路上再烧一轮
-- token。修复回路管的是单次任务内的事（docs/02-design.md §5），这条管跨任务。
--
-- ---------------------------------------------------------------------------
-- 为什么不直接读 tasks / task_events 派生
-- ---------------------------------------------------------------------------
--
-- 都读得到「最近一次失败的原因」，但都不够：
--
--   * tasks.failure_reason 是【单行、会被覆盖】的当前值。重试把它转回
--     queued 后旧值还留着（那正是重试决策的依据），可一旦这一轮成功就
--     没意义了；而且它是给人看的自由文本，没有结构。
--   * task_events 里的 payload 有 stage，但验证输出摘要在 cause 里，
--     没有单独落库（fail() 只把它拼进 failure_reason 与回帖正文）。
--
-- 更重要的是：**「这个单子失败过几次、每次死在哪」本身是自度量要的数据**。
-- 派生视图只能给出「最近一次」，落成表则天然是审计流。
--
-- ---------------------------------------------------------------------------
-- 为什么按 (repo_id, external_key) 而不是 task_id 查
-- ---------------------------------------------------------------------------
--
-- 人工重试走的是【原任务行】（retryTask 不新建 —— 任务 #313 的教训），
-- 但 issue 关掉重开、或内置工单重新开跑时会【新建任务行】。两种形态都要
-- 能读到历史，所以定位键必须是「这个单子」，不是「这一行」。
--
-- tracker_provider 一并进键：external_key 是各平台自己的编号空间
-- （Linear 的 CR-1326、内置工单的 LT-1042），不按平台分开就会把两个
-- 平台的同号单子混成一条记忆。
--
-- ---------------------------------------------------------------------------
-- 多租户
-- ---------------------------------------------------------------------------
--
-- 显式带 user_id（而不是靠 repo_id 隐式限定属主）：这是数据隔离边界，
-- 查询侧不必 JOIN 就能过滤，少一处可以漏掉 JOIN 的地方 —— 与 issues 表
-- 的处理一致（0021）。
--
-- ---------------------------------------------------------------------------
-- append-only
-- ---------------------------------------------------------------------------
--
-- 每次失败落一行，不 upsert。理由有二：一是与 task_events / agent_events /
-- prd_events 的既有形态一致；二是「同一单子反复失败」这件事必须留痕 ——
-- upsert 会把「失败了 5 次」压成「最近失败过」，而那正是最该被看见的信号。
-- 读取侧取最近一次即可，多余的历史行将来给自度量用。
--
-- 量级不是问题：每个失败任务一行，与 tasks 同阶，远小于 agent_events
--（每任务成百上千行）。
CREATE TABLE failure_digests (
  id               bigserial   PRIMARY KEY,
  user_id          bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  repo_id          bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  -- 产生这次失败的任务行。保留它是为了让人能从记忆跳回现场。
  task_id          bigint      NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  external_key     text        NOT NULL,
  tracker_provider text        NOT NULL,
  -- 机器可读的失败阶段代码（runner.Stage；'rebase_conflict' 是
  -- mergepoll 那条路径的代码）。可空：0012 之前的失败路径没记过它。
  -- 不加 CHECK —— 与 tasks.failure_stage 同理，集合随流水线演进。
  failure_stage    text,
  -- 给人看的一句话原因（含阶段标签前缀），已按 rune 截断。
  failure_reason   text,
  -- 验证输出摘要。这是「上次死在哪」最有价值的部分：阶段只说明了
  -- 「哪一步」，摘要说明「为什么」。它在**写入侧**已做两件事：
  --   1) 按字节上限截断（runner 的 maxDigestStoredExcerpt，含截断标记，
  --      按 rune 边界回退，不会切出非法 UTF-8 —— 落库撞 SQLSTATE 22021
  --      就是那么来的）。取的是失败步骤输出的**尾部**：结论写在最后。
  --   2) 脱敏（runner 的 redactSecrets）：验证命令继承宿主环境，输出里
  --      回显凭据/带口令的连接串是常见形态，而这份文本会进提示词与详情页。
  -- 注入侧还有一道更紧的字节上限（防提示词被撑爆），两道各有各的理由。
  excerpt          text,
  at               timestamptz NOT NULL DEFAULT now()
);

-- 唯一的访问模式：按「这个单子」取最近一次失败。
-- DESC 排序让 Planner 直接走索引取第一行，不必排序全部分组。
CREATE INDEX failure_digests_lookup
  ON failure_digests (user_id, repo_id, tracker_provider, external_key, at DESC);

-- 任务行被级联删除（用户注销、仓库解绑）时记忆随之消失 —— 上面三个
-- REFERENCES 都是 ON DELETE CASCADE，行为与 task_events 一致：
-- 记忆是任务的派生物，本体没了就没有留着的理由。
