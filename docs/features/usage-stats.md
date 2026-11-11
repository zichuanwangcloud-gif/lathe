# f03 用量统计（token / 费用 / 时长，按任务、仓库、验证档位、阶段、时间聚合）

> 状态：**已实现**（分支 `feat/usage-stats`，base `origin/dev`）。本文前半是评审通过的 PRD 定稿
> （第六版，正文原样保留，含五轮评审答复），文末「实现记录」写明 AC 实际怎么验的、偏离了哪条、为什么。
> 文中 `/opt/lathe-wt/...` 路径是开发机上的证据目录，PR 描述附了其中的红绿输出。
> 迁移 0038–0040；需要 PostgreSQL ≥ 15。本功能无新增环境变量。

---

## 1. 问题与目标

### 1.1 为什么 Lathe 需要它

Lathe 的定位是「Linear issue → **已验证的 PR**」。这句话里唯一的不可替代品
是「已验证」，而验证是要花 agent 钱的：一次 heavy 档任务 = 分诊 + 实现 +
最多 `LATHE_FIX_ATTEMPTS`（默认 2）轮修复 + PRD 规划若干轮。今天能回答
「这个任务花了多少」的地方只有两处，而且两处都不回答问题的本体：

- `tasks.agent_cost_usd` 是**最后一轮 fix 的单轮成本**（`pipeline.persistAgentResult`
  每轮整行 UPDATE 覆盖，`internal/store/cost.go:9-27` 有完整取证）；
- `CostPanel.vue` 只按「分诊 / 实现 / 其他」三个桶看 30 天花费，**没有 token、
  没有仓库维度、没有验证档位维度、没有时间范围**。

于是「heavy 档的单子是不是真的比 light 档贵 10 倍」「PR review 二轮改一版
值不值这个价」「PRD 规划烧掉的钱有没有换来一份能执行的 PRD」这类问题，
今天只能人肉写 SQL，还得知道 `agent_events.payload` 的内部形状。

### 1.2 目标

1. **每次 agent 运行一行可查的记录**：token（输入/输出/缓存读/缓存写）、
   费用（USD）、墙钟时长、轮数、模型，落在新表 `agent_runs`。
2. **五个维度聚合**：任务、仓库、验证档位、阶段（triage / implement /
   review 续跑 / PRD 规划 / 预览推荐 / verify）、时间范围。
3. **两类可见性**：普通用户只看自己名下；管理员有独立的全局视图
   （显式端点，不是藏在普通端点里的旁路，见 §3.5）。
4. **不改验证体系、不改状态机、不做预算拦截**。这是一条纯读侧 + 一次
   写侧插桩的功能；它度量的对象（验证档位、红绿证明）本身一个字都不动。

### 1.3 与「已验证 PR」定位的关系

验证档位（`light` / `heavy`）是 Lathe 对交付质量的分级承诺。用量统计把
「每一档的单价」和「每个阶段的钱去了哪」变成可查的数字，让「哪些单子值得
交给 agent」从直觉变成数据 —— 这正是 roadmap §3.2「成本与效率 —— token 就是钱」
（`docs/05-roadmap.md:92`）一直缺的那一块，也是 T5 成本面板
（`docs/08-debt-cleanup.md:181-205`）明确留给后续的位置。

---

## 2. 现状取证

### 2.1 已经有的（可直接复用，不重造）

| 事实 | 位置 |
|---|---|
| `result` 事件里的 `total_cost_usd` / `duration_ms` / `num_turns` 已解析进 `agent.Result` | `internal/integration/agent/agent.go:87-104`（`Result` 结构）、`agent.go:106-120`（`resultEvent` 映射）、`agent.go:424-430`（赋值） |
| 每次 `result` 落成一条 `agent_events` 行，`kind='result'`，payload 含 `costUsd` / `durationMs` / `numTurns` / `subtype` / `isError` | `internal/integration/agent/digest.go:236-269` |
| 阶段标签已经有：`triage` / `implement` / `implement-reentry` / `fix-N` / `verify` / `push` | `pipeline.go:542`（triage）、`pipeline.go:698,713`（implement / implement-reentry）、`pipeline.go:1009`（`fix-%d`）、`pipeline.go:1220`（push）、`internal/runner/eventsink.go:35`（`PhaseImplementReentry`） |
| **但 `implement-reentry` 的事件今天根本进不了库**：现网 `agent_events_phase_check` 是 `phase IN (triage,implement,verify,review,push) OR phase ~ '^fix-[0-9]+$'`，不含 `implement-reentry`；`InsertAgentEvents` 走 `CopyFrom`（`internal/store/agents.go:39`），整批被 `23514` 拒收，**含 result 事件**。实测（lathe_f03，PG 16.11）：`INSERT INTO agent_events(task_id,phase,kind) VALUES (1,'implement-reentry','result')` → `violates check constraint "agent_events_phase_check"` | `migrations/0011_agent_events_fix_phase.up.sql`、`0015_agent_events_push_phase.up.sql`；修复归 f04（`f04-run-replay.md` §2.2-2），本线不碰，见 §4 |
| `tasks.verify_tier` **只在转入 `verifying` 时写**（`Transition(..., StateVerifying, {VerifyTier})`）；全新任务的分诊与首轮实现都在它之前，那时该列为 NULL | `pipeline.go:936-937`；CHECK `verify_tier IS NULL OR IN ('light','heavy')` |
| **从头重跑（Fresh）在同一行任务上改档**：Fresh 重试对同一个 `tk.ID` 调 `Execute`（`cmd/lathe/queue.go:472-489`，先 `Discard` 旧现场，不走 `tasks.Create`，AGENTS.md §9）；`setup` 在 `entry==EntryTriage` 时于 `pipeline.go:455-457` 提前返回，**走不到** `:485-487` 的 `rc.tier = tk.VerifyTier`，于是 `stageVerify`（`:914-926`）落到 `default` 分支用 `ClassifyTier` 重新定档，`Transition` 经 `machine.go:362` 的 `COALESCE($7, verify_tier)` 把旧档位**覆盖**。即 light→heavy（或反向）的改档路径今天就存在，且就在同一行上。另：Fresh 期间、重新定档之前，`tasks.verify_tier` 仍是**上一次尝试**的旧值（COALESCE 不会清空） | 同左 |
| `agent.Run` 在静默判死、总超时、取消时返回 **`result, err`**（`result` 可能非 nil：result 事件已到、进程却没退）；总超时与取消**不是** `ErrIdle` 也不是 `ErrNoResultEvent`，总超时是一个没有 `%w` 的裸 `fmt.Errorf`，取消则 `%w` 包了 `ctx.Err()` | `agent.go:330-339`（三个分支）、`agent.go:345-347`（`ErrNoResultEvent`）、`agent.go:352-355`（非零退出但有 result） |
| `preview.Manager` 无 store、无用户、无仓库；`runRecommend` 只知道 `taskID`；`SetRecommender(ag, channel, settingSources)` 没有记账参数；`internal/preview` 今天只 import `internal/integration/agent` | `internal/preview/manager.go:262-291`、`recommend.go:82-86,136`、`cmd/lathe/main.go:211` |
| `prd.runSpec` 有 `prdID` 与 `RepoInfo`，但 `RepoInfo` 只有 `ProviderRepo/DefaultBranch/Limits`，**没有 repo id**；`prd.Store` 接口也没有取属主的方法 | `internal/prd/service.go:55-74,480-490`、`internal/prd/adapter.go:37-61` |
| 依赖方向：`internal/store` 只 import `integration/agent` 与 `secret`；`runner`、`prd` 已 import `store`；`preview` 不 import `store`，`store` 也不 import `preview` | `grep -rn 'lathe/internal/' internal/{store,runner,prd,preview}/*.go` |
| 成本聚合已存在，且**刻意**不用 `tasks.agent_cost_usd` | `internal/store/cost.go:9-35`（取证注释）、`cost.go:127-209`（`CostStatsFor`） |
| 成本聚合的 API 与前端 | `internal/httpapi/api.go:74`（路由）、`api.go:212-223`（handler）、`web/src/components/CostPanel.vue`、`web/src/views/Board.vue:119` |
| 平台自度量（环节耗时分位数 + 失败直方图）已在，可作形态参照 | `internal/store/metrics.go:215+`、`api.go:186-210` |
| 管理员鉴权已存在且普通用户拿 403 | `internal/httpapi/auth.go:352-365`（`RequireAdmin`）、`admin.go:28-35` |
| 按属主隔离的既有纪律 | `internal/store/cost.go:30-35`（`agent_events` 无 `user_id`，必须 JOIN `tasks`）、`api.go:60-63`（读接口全部按 `CurrentUser` 隔离） |

### 2.2 缺的（本功能要补的）

1. **token 根本没解析**。`resultEvent`（`agent.go:106-120`）只映射了
   `total_cost_usd` / `duration_ms` / `num_turns`，**没有 `usage`，也没有
   `modelUsage`**。所以 token 在进程里就丢了，库里也没有。
   取证样本：`/opt/lathe-wt/evidence/f03/claude-result-event-sample.json`
   （本机 claude 2.1.322 实测一次 `-p` 运行的 `result` 事件）。形状：
   ```jsonc
   "total_cost_usd": 0.085283,
   "usage": {"input_tokens":2, "output_tokens":4,
             "cache_creation_input_tokens":17039, "cache_read_input_tokens":0, ...},
   "modelUsage": {"claude-opus-5-5": {"inputTokens":2, "outputTokens":4,
                 "cacheReadInputTokens":0, "cacheCreationInputTokens":17039,
                 "costUSD":0.085283, "canonicalModel":"claude-opus-5-5", "provider":"firstParty"}}
   ```
   `modelUsage` 是**本进程累计**，跨 result 不能相加；一次 `Run` 一个进程
   （`agent.go:228` `exec.Command`）。「按 Run 求和」在这里**只指**两件事：
   ① 一条 result 内把 `modelUsage` 的**各模型**相加得到这次 Run 的四个计数；
   ② 聚合时把**各次 Run**（`agent_runs` 各行）相加。它**不指**把同一次 Run 里
   多条 result 的值相加 —— 一次 Run 可以出现多条 result（f01 的插话会开第二个 turn；
   现网解析循环 `agent.go:418-432` 也已逐条处理多个 result 行，覆盖赋值、末条胜出），
   这时 `Usage` **随最后一条 result 覆盖**（进程累计值即该进程总账），规则与证据见 §3.1
   「多条 result 时 `Usage` 怎么取」（与 f01 §3.2 第 5 条同一张表）。
   `modelUsage` 内含 subagent 的调用，`usage` 是主循环的量 —— 取
   `modelUsage` 各模型之和为主，缺它时回落 `usage`（同样只取末条）。
2. **没有 per-run 的行**。可得的只有：`tasks` 上被覆盖的单轮四列
   （`migrations/0009_agent_summary.up.sql:13-16`）、以及 `agent_events.payload`。
   后者有**两个结构性缺陷**，不是「查询没写」，是「数据源不可靠」：
   - `BatchSink.Offer` 在缓冲满时**丢事件**（`internal/integration/agent/batchsink.go:89-107`，
     `sinkCap=256`，只保 thinking 优先丢，`result` 在极端情况下也可能被丢）；
   - `persistAgentResult` 的落库失败只 `slog.Warn`（`pipeline.go:1442-1455`），
     事件批量写失败同理（`internal/store/agents.go:34-60`，`BatchWriter` 返回
     错误「该批丢弃」`batchsink.go:41-43`）。
   用它做「钱」的口径可以（T5 已经这么做了，且是当时的最小改动）；做
   **token 的权威源**不行 —— 丢一条就是少一笔账。
3. **没有失败/被杀的运行记录**。超时、取消、静默判死（`agent.ErrIdle`，
   `agent.go:330-339`）时 `Run` 返回 `result, err` —— 多数情况 `result==nil`、
   没有任何 `result` 事件；少数情况 result 已到但进程被杀，`result` 非 nil
   却带着 `err`，而调用方按 `err` 走失败分支，那份 result 的花费没人记。
   于是**花了钱的失败运行在库里不存在**。而「预算该花在哪」恰恰要看到这些。
   另：`implement-reentry` 阶段的事件被 CHECK 整批拒收（§2.1），这一阶段
   今天在任何表里都没有花费记录。
4. **PRD 规划与预览推荐不在任何聚合里**。PRD 的 result 落在 `prd_events`
   （`migrations/0025_prd_events.up.sql`，`phase ∈ {plan, plan-review}`），
   预览推荐的 result **只打了日志**（`internal/preview/recommend.go:197-199`
   `slog.Info(... "cost", res.CostUSD)`，`RunParams` 连 `OnEvent` 都没接，
   `recommend.go:169-177`）。
5. **没有时间范围、仓库、档位维度**。`CostStatsFor` 写死 30 天（`cost.go:46`），
   维度只有「阶段桶 / 日 / 任务排行」。
6. **没有管理员全局视图**。现有读接口全部 `CurrentUser(r).ID`
   （`api.go:130,169,187,218`）；`api.go:60-63` 明确写着「管理员也不例外 ——
   跨用户读取的后门宁可没有」。本需求要开这个口子，就必须**显式开**（§3.5）。
7. **任务详情页的徽章会误导**：`web/src/views/TaskDetail.vue:388-389` 直接把
   `task.agentCostUsd` / `agentDurationMs` 当「本轮花费/耗时」展示，而那一列是
   最后一轮 fix 的覆盖值。

---

## 3. 方案

### 3.1 数据模型（迁移 0038 / 0039 / 0040）

**一句话**：新增一张 append-only 的 `agent_runs`，每次 agent 进程运行一行，
由**调用点**（它知道目标 id 与阶段）在 `Run` 返回后同步写一行，属主与仓库由
store 在写入时从目标行上取（§3.1「归属怎么来」）。`agent_events` /
`prd_events` 保持原样（它们是人读的事件流），不改口径、不加列。

为什么不用「继续从 `agent_events` 求和」：§2.2 第 2、3 条 —— 事件可丢、
失败运行不存在、PRD/预览在别的表或根本没表。为什么不做成 `agent_events`
的一列：那是给人看的时间线，不是账本；两张表的口径（一条事件 vs 一次运行）
本来就不同。

#### 0038_agent_runs.up.sql

```sql
-- 0038_agent_runs — 每次 agent 运行的用量账本（f03）
--
-- 为什么新开一张表而不是从 agent_events 求和：
-- 1. BatchSink 缓冲满时丢事件（batchsink.go），result 也可能丢 —— 账不能丢。
-- 2. 超时/静默判死没有 result 事件，但那是一次真实的运行、真实的钱。
-- 3. PRD 规划落在 prd_events，预览推荐根本没落库；implement-reentry 的事件
--    被 agent_events_phase_check 整批拒收（f04 修），本表是该阶段唯一的账。
-- 4. token 此前压根没解析；新增字段不该塞进人读事件流的 payload。
--
-- append-only，不设 updated_at、不挂 set_updated_at 触发器（同 agent_events）。
-- 唯一的例外是定档回补：verify_tier 只允许 NULL → light/heavy（§3.2.1），
-- 其余列写入后不再改。
CREATE TABLE agent_runs (
  id         bigserial PRIMARY KEY,

  -- 归属：run_kind=task/preview 填 task_id，run_kind=prd 填 prd_id，恰有其一。
  -- user_id / repo_id 是**冗余快照**，由 store 在 INSERT 时从 tasks / prds
  -- 行上取（INSERT ... SELECT，§3.1「归属怎么来」），调用方不传：
  -- 聚合查询的主过滤条件，避免每个查询都多两层 JOIN（同 cost.go 的教训，
  -- 漏 JOIN 就是跨用户求和）。
  task_id    bigint REFERENCES tasks(id) ON DELETE CASCADE,
  prd_id     bigint REFERENCES prds(id) ON DELETE CASCADE,
  user_id    bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  repo_id    bigint NOT NULL REFERENCES repos(id) ON DELETE CASCADE,

  phase      text NOT NULL,   -- 原始阶段标签：triage/implement/implement-reentry/fix-N/verify/push/plan/plan-review/recommend
  bucket     text NOT NULL,   -- 归并后的阶段桶（store.UsageBucket 的唯一真相源）
  run_kind   text NOT NULL,   -- task | prd | preview
  review_round boolean NOT NULL DEFAULT false,  -- PR review 续跑那一轮（plan.Review），写入时快照

  session_id text,            -- 可为 NULL（假 driver / 早期失败）；去重键里按 NULLS NOT DISTINCT 处理
  resume     boolean NOT NULL DEFAULT false,

  model      text,            -- 主模型（modelUsage 里 costUSD 最大的那个）；未知为 NULL
  models     jsonb NOT NULL DEFAULT '[]',  -- 逐模型明细 [{model,inputTokens,...}]，≤16 项
  input_tokens         bigint NOT NULL DEFAULT 0,
  output_tokens        bigint NOT NULL DEFAULT 0,
  cache_read_tokens    bigint NOT NULL DEFAULT 0,
  cache_creation_tokens bigint NOT NULL DEFAULT 0,

  -- token 是否由 CLI 上报（agent.Usage.Present）。false 时四个计数为 0 但含义是「未知」，
  -- 不是「用了 0」；聚合的 tokensKnown 数的就是这一列。codex 不报模型名，所以不能用
  -- models<>'[]' 代替它。
  tokens_known boolean NOT NULL DEFAULT false,

  -- 费用（USD）。**NULL = 未知**（codex 不报费用、或进程没给出 result 就被杀/退出），
  -- 与「确实花了 $0」分开。聚合 SUM 自然跳过 NULL，另出 costKnown 计数（§3.3）。
  cost_usd   numeric(14,6),
  duration_ms bigint NOT NULL DEFAULT 0,

  -- 这次运行发生时它所属尝试的验证档位，写入时快照（§3.2.1）。NULL = 当时尚未定档。
  -- 只允许 NULL → light/heavy 补一次（定档回补，§3.2.1），已有值的行永不改写。
  verify_tier text,
  num_turns  int NOT NULL DEFAULT 0,

  status     text NOT NULL,   -- success | error | no_result | killed（判定表见 §3.1「状态判定」）
  subtype    text,            -- CLI 的 result.subtype 短标签，≤64 rune

  -- started_at：实时行 = 调用方在 Run 之前取的时钟；回填行 = ended_at - duration_ms（§0039）。
  -- ended_at  ：实时行 = Run 返回后取的时钟；回填行 = 事件的 at。
  -- at：**库时钟**的写入时刻。store 的 INSERT 列表里**不出现** at，永远取 DEFAULT now()；
  -- 0039 的截止条件只信这一列（与 agent_events.at / prd_events.at 同一个时钟，§0039）。
  started_at timestamptz NOT NULL,
  ended_at   timestamptz NOT NULL,
  at         timestamptz NOT NULL DEFAULT now(),
  backfill   boolean NOT NULL DEFAULT false,      -- 0039 从历史事件回填的行

  CONSTRAINT agent_runs_target_check CHECK (
    (run_kind IN ('task','preview') AND task_id IS NOT NULL AND prd_id IS NULL) OR
    (run_kind = 'prd'               AND task_id IS NULL     AND prd_id IS NOT NULL)),
  CONSTRAINT agent_runs_bucket_check CHECK (bucket IN (
    'triage','implement','review','verify','push','prd_plan','prd_review','preview','other')),
  CONSTRAINT agent_runs_kind_check   CHECK (run_kind IN ('task','prd','preview')),
  CONSTRAINT agent_runs_status_check CHECK (status IN ('success','error','no_result','killed')),
  CONSTRAINT agent_runs_time_check   CHECK (ended_at >= started_at),
  CONSTRAINT agent_runs_tier_check   CHECK (verify_tier IS NULL OR verify_tier IN ('light','heavy')),
  CONSTRAINT agent_runs_tier_kind_check CHECK (run_kind <> 'prd' OR verify_tier IS NULL),
  -- 幂等闸门。NULLS NOT DISTINCT 是**必需**的：target_check 保证每行
  -- task_id/prd_id 恰有一个 NULL，session_id 也可为 NULL；默认 NULLS DISTINCT
  -- 下这条约束永远不会冲突（第一版的缺陷，评审实测 count=2）。
  -- PG15+ 语法；dev-infra / CI 均为 postgres:16-alpine（docker-compose.dev.yml:5、
  -- .github/workflows/ci.yml:101）。run_kind 进键：同一任务的预览推荐与实现
  -- 不可能共享 session，但写进键里让「同一次尝试」的定义不依赖这个巧合。
  CONSTRAINT agent_runs_attempt_uniq UNIQUE NULLS NOT DISTINCT
    (run_kind, task_id, prd_id, phase, session_id, resume, started_at)
);

-- 刻意不在这里建时间索引：表随后会被 0039 回填，索引在空表上建、再被回填
-- 逐行维护是白烧；两个主查询索引统一放 0040 用 CONCURRENTLY 建（§6.3）。
-- 唯一约束自带的那条索引是幂等的前提，必须随表建，不能延后。
```

down：`DROP TABLE IF EXISTS agent_runs;`（0039/0040 先回滚，0038 的 down 只删表）。

- `models` 上限 16 项、`model` 截断到 64 **rune**（AGENTS.md §9：字符串截断按 rune
  不按 byte，先例 `internal/integration/agent/agent.go:502-513`）。超过 16 个模型时**不丢量**：
  按 `costUSD` 降序（并列按模型名升序）保留前 15 个，其余合并成第 16 个元素 `model="(other)"`
  （四个计数与 `costUSD` 为被合并者之和）。这样 `models` 各元素之和恒等于行上四列，`byModel`
  与 `totals` 的 token 对账（§3.3）在截断时也成立。行上四列与 `model`（主模型）按截断**前**的全集算。
- 唯一约束是本功能的幂等闸门：同一次尝试（kind + 目标 + 阶段 + 会话 + resume 标志 +
  开始时刻）重复写只能有一行，写入用 `ON CONFLICT ON CONSTRAINT agent_runs_attempt_uniq
  DO NOTHING`。**为什么选 `NULLS NOT DISTINCT` 而不是两条部分唯一索引**：
  两种都已在 lathe_f03（PG 16.11）实测能去重（含 `session_id IS NULL`）；
  前者是一条约束、`ON CONFLICT` 可以按约束名指定、down 随表一起走，后者要两条
  索引 + `COALESCE(session_id,'')` 表达式、`ON CONFLICT` 必须写出索引谓词才能命中。
  代价是要求 PG15+ —— 现有部署基线就是 16，在 `docs/features/usage-stats.md`
  与 README「镜像部署用外部 Postgres」那一行旁写明「需 PostgreSQL ≥ 15」。
- `ended_at` 钳位：store 写入时取 `max(EndedAt, StartedAt)`。`started_at/ended_at` 是节点的墙钟，
  运行期间若 NTP 把墙钟往回拨，`EndedAt` 可能早于 `StartedAt`，不钳位则 `agent_runs_time_check`
  会把这条真实花费整行拒掉（第四版顺带发现，`TestRecordAgentRunClampsEndedAt` 守，见 AC-4）。
- `started_at` 精度：Go `time.Time` → `timestamptz` 是微秒，同一调用点同一会话
  微秒内连起两次 Run 不存在（每次 Run 至少起一个进程）；真正的重复写只来自
  「同一条记录被提交两次」，这正是唯一约束要挡的。
- 不写 prompt、不写正文、不写错误全文 —— 账本里没有可泄漏的内容（§3.6）。

#### 0039_agent_runs_backfill.up.sql

把历史 `agent_events` / `prd_events` 里**已有的** `kind='result'` 行回填成
`agent_runs`，`backfill=true`。逐字段定义：

| 列 | `agent_events` 来源 | `prd_events` 来源 |
|---|---|---|
| `task_id` / `prd_id` | `ae.task_id` | `pe.prd_id` |
| `user_id` / `repo_id` | JOIN `tasks` | JOIN `prds` |
| `run_kind` | `'task'` | `'prd'` |
| `phase` | `ae.phase` 原样 | `pe.phase` 原样 |
| `review_round` | **恒 `false`**（见下） | `false` |
| `bucket` | CASE，与 `store.UsageBucket(phase, false)` 同构 | 同左 |
| `session_id` | NULL（事件行没有） | NULL |
| `resume` | `false` | `false` |
| `cost_usd` | `payload->>'costUsd'`；**键缺失或 `payload->>'driver'='codex'` → NULL**（未知，§3.1 列注释），不按 0 | 同左（prd 只跑 claude，f06 §4-1） |
| `duration_ms` / `num_turns` | `payload->>'durationMs'` / `'numTurns'`，缺失按 0 | 同左 |
| `status` | `isError=true` 或 `subtype<>'success'` → `error`；否则 `success`（回填行不会出现 `no_result`/`killed`：那两种本来就没有 result 事件） | 同左 |
| `subtype` | `left(payload->>'subtype', 64)`（SQL `left()` 按字符） | 同左 |
| `ended_at` | `ae.at`（result 事件的落库时刻 ≈ 运行结束） | `pe.at` |
| `started_at` | `ae.at - make_interval(secs => duration_ms/1000.0)`（`duration_ms=0` 时等于 `ended_at`） | 同左 |
| `verify_tier` | `t.verify_tier`（**回填那一刻**任务行上的档位；历史事件里没有档位，这是唯一可得的近似，见下） | NULL（CHECK 要求） |
| token 四列 / `tokens_known` | 有 `payload->'usage'`（f06 的 codex result 事件，若 f06 先合）→ 按 §3.7 的 codex 映射取四列、`tokens_known=true`；否则 0 / `false` | 0 / `false` |
| `model` / `models` | NULL / `'[]'` —— 历史 payload 里没有 | 同左 |

**截止条件（第二轮 blocker 2；第三轮 blocker 1 修订）**：回填只补「实时账本还没覆盖到」的事件。
按**目标**截止——某任务（或 PRD）一旦有了实时行，在「它第一条实时行所对应运行的开始时刻（库时钟）
减去容差」之后的该目标事件一律视为已由实时账覆盖，不回填：

```sql
INSERT INTO agent_runs (...)
SELECT ... FROM agent_events ae JOIN tasks t ON t.id = ae.task_id
 WHERE ae.kind = 'result'
   AND NOT EXISTS (
         SELECT 1 FROM agent_runs r
          WHERE r.run_kind = 'task' AND r.task_id = ae.task_id AND NOT r.backfill
            -- 只比库时钟：r.at 与 ae.at 都是同一台 Postgres 的 now()。
            -- (r.ended_at - r.started_at) 是节点墙钟的**差值**，与节点和库之间的偏差无关。
            AND ae.at >= r.at - (r.ended_at - r.started_at) - interval '5 minutes')
ON CONFLICT ON CONSTRAINT agent_runs_attempt_uniq DO NOTHING;
-- prd_events 同形：r.run_kind = 'prd' AND r.prd_id = pe.prd_id，比较 pe.at
```

- **第三版为什么错**：第三版比的是 `r.started_at <= ae.at`，左边是**节点**时钟、右边是**库**时钟。
  节点比库快、且快出的量 > 该目标第一次实时运行的时长时，`R1.started_at > R1 事件的 at`，NOT EXISTS
  成立，R1 自己的 result 事件被回填，**同一次运行记两行**。第三版 §3.1/§6 写的「最坏少补、不会多算」
  是反的。lathe_f03（PG 16.11）临时表复现：节点快 120s、运行 30s → 第三版条件下该任务 3 行 / $0.60
  （want 2 / $0.35）；节点慢 1h 时第三版则会**漏补**一条早于首条实时行 30min 的真实历史。新条件两种都对。
  证据：`/opt/lathe-wt/evidence/f03/review-r3-b1-clock-skew.txt`。
- **为什么新条件只会少补、不会多算**：设同一次运行 R 的库时钟开始/结束为 `S`/`E`，它的 result 事件
  `ae.at ≥ S`（事件只可能在运行开始之后落库），实时行 `r.at = E + d₂`（`d₂` = Run 返回到 `RecordRun`
  的 INSERT 落库之间的延迟；`runAgent` 先 `sink.Close()` 把事件落完再返回，`pipeline.go:784`，所以 `d₂`
  是秒级的非负数）。`r.ended_at - r.started_at` 是节点墙钟测得的时长 `D'`，
  与真实时长 `D = E - S` 只差「运行期间墙钟被 NTP 步进的量」`σ`。条件右边
  `r.at - D' - tol = S + d₂ - σ - tol`。只要 `d₂ - σ ≤ tol`，就有右边 `≤ S ≤ ae.at`，R 自己的事件必被排除。
  这个不等式里**没有**节点与库之间的绝对偏差——那正是第三版的漏洞。
- **容差取 5 分钟的理由**：要盖住 `d₂ + |σ|`。`d₂` 的上界由代码给出：事件 drain 单批写库上限
  `sinkWriteTimeout = 5s`（`batchsink.go:38`）+ `RecordRun` 的 3s 写库超时，量级是秒；`σ` 是运行期间的
  单次墙钟步进，chrony/ntpd 默认只在偏差超过 128ms（ntpd）或启动时才步进，常态是 slew。5 分钟比这两项之和
  大两个数量级，又远小于「旧二进制在同一目标上与新二进制交错跑」的现实间隔（同一任务的相邻两次运行之间
  至少隔一轮验证），所以容差带来的代价只是：**滚动升级窗口里、同一目标、在其首条实时运行开始前 5 分钟内
  由旧节点产生的事件不回填**——少补，方向安全。编译期常量写在 SQL 里，不配置化（§3.5：无消费方不加开关）。
- **为什么按目标、不按全局**：全局截止（`at < min(全表实时行 started_at)`）在多节点滚动升级时会把
  「新节点已在写实时行、旧节点还在跑」那段时间里旧节点的运行整段丢掉；按目标截止只丢「同一个
  目标、在它第一条实时行之后、却由旧二进制跑出来」的运行，范围小得多。两种都**不会重复计账**。
- **索引**：`r.run_kind = 'task' AND r.task_id = ...` 命中唯一约束索引的前两列
  `(run_kind, task_id, ...)`，`at/ended_at/started_at` 在索引命中后的少量行上过滤；回滚 0039 时
  0040 的索引已先被 down 掉，所以不能依赖它们。
- **down/up 往返的不变量**：**任何一次运行在账本里至多一行**。往返后的回填行数**可能变多**
  （例如滚动升级窗口里旧节点产生的、首轮回填时还不存在的事件），但永不重复；AC-6 断言的是
  「金额与行数都不含重复」，不再断言「行数不变」。
- 回填行之间的去重：回填行 `session_id` 为 NULL，同一任务同一 phase 两条 result 若 `at`
  与 `duration_ms` 都一样会被唯一约束合并 —— 现实中不可能（`at` 是 `now()` 的微秒）；仍写
  `ON CONFLICT DO NOTHING`，不经 down 直接重跑 0039 也幂等。
- **`verify_tier` 的回填是近似**：取的是回填时刻任务行的档位，一个历史上被 Fresh 重跑改过档的
  任务，它改档前那几次尝试会被记成新档（历史里没有逐次档位，无从还原）。回填行带
  `backfill=true`，`/usage` 页含回填行时提示「历史数据的档位按回填时的任务档位计」。回填时
  任务尚未定档的（NULL），之后若定档，按 §3.2.1 的定档回补一并补上。
- **`review_round` 无法回填**：`agent_events` 里没有任何「这是 review 续跑」的
  信息（phase 复用 `implement`，`pipeline.go:832-834` 的约定），因此历史上的
  review 轮次全部归入 `implement` 桶。这是回填的已知精度损失，写进 docs；
  AC-6 的「回填与 Go 同构」**只对 `review_round=false` 成立**，也只这样断言。
- **`implement-reentry` 无法回填**：该 phase 的事件从未进过库（§2.1），回填后
  也不会有这一阶段的历史行。
- down（`0039_agent_runs_backfill.down.sql`）：`DELETE FROM agent_runs WHERE backfill;`
  ——只删回填行，实时行保留；再 up 时由上面的截止条件保证不与实时行重复。

#### 0040_agent_runs_index_concurrently.up.sql

在 0039 回填**之后**，用 `CONCURRENTLY` 建两条主查询索引：
`agent_runs_user_time (user_id, started_at DESC)` 与
`agent_runs_repo_time (repo_id, started_at DESC)`。AGENTS.md §6.3 —— 表会持续增长，
索引不能在事务块里建：`-- lathe:no-transaction` 标记、脚本开头 `DROP INDEX
IF EXISTS` 幂等重跑、结尾照 `migrations/0018_agent_events_result_index.up.sql`
的 DO 块校验 `indisvalid`（`pg_index JOIN pg_class ON c.oid=i.indexrelid
WHERE c.relname IN (...) AND i.indisvalid`，不足 2 条就 `RAISE EXCEPTION`），
down 同名反向（`DROP INDEX CONCURRENTLY IF EXISTS`）且同样带标记。

> 拆三条而不是一条：§6.6「一次迁移只做一件事」——建表 / 回填 / 索引。
> 顺序是「建表 → 回填 → 建索引」，索引不用为回填逐行维护。

#### 写入点（谁写、写什么、归属从哪来）

| 调用点 | `run_kind` / `phase` | 目标 id | 依据行 |
|---|---|---|---|
| `Pipeline.stageTriage` | `task` / `triage`，`VerifyTier = rc.tier`（全新任务与 Fresh 时为空） | `rc.tk.ID` | `pipeline.go:542-560` |
| `Pipeline.runAgent`（实现/降级重入/修复，含 review 轮） | `task` / 原样（`implement`/`implement-reentry`/`fix-N`），`review_round = rc.plan != nil && rc.plan.Review`，`VerifyTier = rc.tier` | `rc.tk.ID` | `pipeline.go:766-788`；`rc.plan.Review` 在 `pipeline.go:493` |
| `Pipeline.stageVerify` 定档后（**不是**一次运行，是回补） | `SetAgentRunTier(rc.tk.ID, tier)` | `rc.tk.ID` | `pipeline.go:936-943`，紧跟 `rc.tk = tk`；同时 `rc.tier = tier`，让本次尝试的 fix-N 带上档位（`rc.tier` 今天唯一的读点是 `:914` 的定档入口，已在其后，赋值不改变任何既有行为） |
| `prd.Service.runAgent` 循环里**每次** `runOnce` | `prd` / `spec.phase`（`plan`/`plan-review`），首跑、超时收尾、每次修正**各一行** | `spec.prdID` | `internal/prd/service.go:568-601`（循环）、`:603-616`（`runOnce`） |
| `preview.Manager.runRecommend` | `preview` / `recommend` | `taskID` | `internal/preview/recommend.go:136-199` |

**归属怎么来（blocker 4 的答复）**：调用方**只传目标 id**（`TaskID` 或
`PRDID`），`user_id` / `repo_id` 由 `store` 在写入时从目标行上取。按 `run_kind`
选两条语句之一（task/preview 读 `tasks`，prd 读 `prds`），形态相同，以任务为例：

```sql
WITH tgt AS (SELECT id, user_id, repo_id FROM tasks WHERE id = $1),
     ins AS (
       INSERT INTO agent_runs (task_id, prd_id, user_id, repo_id, run_kind, phase, bucket, ...)
       SELECT tgt.id, NULL, tgt.user_id, tgt.repo_id, $2, $3, $4, ... FROM tgt
       ON CONFLICT ON CONSTRAINT agent_runs_attempt_uniq DO NOTHING
       RETURNING 1)
SELECT (SELECT count(*) FROM tgt), (SELECT count(*) FROM ins)
```

- `tgt=0` → 目标行已不存在（任务/PRD 被删），返回 `store.ErrAgentRunTargetGone`，调用方 `slog.Warn`；
- `tgt=1, ins=0` → 同一次尝试重复写，被唯一约束吞掉，**返回 nil**（AC-4）；
- `tgt=1, ins=1` → 正常写入。

两种「0 行」由 `tgt` 计数区分，不靠猜。`verify_tier` 列：task 行取 `RunRecord.VerifyTier`
（调用方的 `rc.tier`），preview 行取同一条语句里的 `tgt.verify_tier`，prd 行恒 NULL（§3.2.1）。
理由：

- 归属的唯一真相是 `tasks.user_id/repo_id`、`prds.user_id/repo_id`（都是 NOT NULL）；
  让调用方带着去写等于在 4 个调用点各复制一份「谁是属主」，漏一处就是跨用户记账；
- `preview.Manager` 不需要 store、用户、仓库，`prd.RepoInfo` 不需要加 repo id，
  `prd.Store` 接口不需要新方法 —— 改动面最小；
- 不额外查询：SELECT 与 INSERT 是同一条语句，没有读改写窗口。

**记账接口放哪、怎么注入（不造 import 环）**：接口与记录结构放在
`internal/integration/agent`（`agent/usage.go`）—— 这是 store、runner、prd、
preview **四方都已经 import** 的唯一叶子包（它只依赖 `capbuf`），放这里零新增依赖边：

```go
// package agent

// RunRecord 是一次 agent 运行的账本行（调用方视角：只有目标 id，不含属主）。
type RunRecord struct {
    Kind        string // "task" | "prd" | "preview"
    TaskID      int64  // Kind=task/preview
    PRDID       int64  // Kind=prd
    Phase       string
    ReviewRound bool
    VerifyTier  string // 任务运行：本次 Execute 的 rc.tier（空 = 尚未定档 → NULL）；preview/prd 不填，见 §3.2.1
    SessionID   string
    Resume      bool
    StartedAt, EndedAt time.Time
    Result      *Result // 可为 nil
    Err         error   // Run 的返回错误；只用于判 status，不落库
}

// UsageRecorder 落一次 agent 运行的用量账本。store.Store 实现它。
// 落库失败只告警，绝不影响调用方（同 persistVerifications 的立场）。
type UsageRecorder interface {
    RecordAgentRun(ctx context.Context, r RunRecord) error
}

// TierBackfiller 是 runner 额外需要的那一半（定档回补，§3.2.1）。store.Store 实现它；
// 单独成接口是为了 prd / preview 的假实现不必写一个用不到的方法。
type TierBackfiller interface {
    SetAgentRunTier(ctx context.Context, taskID int64, tier string) error
}

// RecordRun 是四个调用点共用的薄包装：rec 为 nil 时什么都不做；
// 用 context.WithoutCancel(ctx) + 3s 超时写，失败 slog.Warn("agent 用量落库失败", ...)。
func RecordRun(ctx context.Context, rec UsageRecorder, r RunRecord)
```

`store` 一侧把 `RunRecord` 翻成列（`UsageBucket`、`RunStatus`、token 汇总、rune 截断都在 store）。
注入点：

| 包 | 注入方式 | 装配（`cmd/lathe/main.go`） |
|---|---|---|
| `runner` | `Pipeline` 新增字段 `Usage agent.UsageRecorder` 与 `UsageTier agent.TierBackfiller`（定档回补，§3.2.1） | `:583` 旁 `Usage: st, UsageTier: st`（与 `AgentEvents: st` 同一处） |
| `prd` | `Service` 新增字段 `Usage agent.UsageRecorder` | `:425-434` 的 `&prd.Service{...}` 加 `Usage: st` |
| `preview` | `Manager` 新增**导出字段** `Usage agent.UsageRecorder`；**不改 `SetRecommender` 签名**（它有既有调用方与测试，改签名会和 f06 的驱动装配顶在一起） | `:211` 的 `SetRecommender(...)` 之后加一行 `previewMgr.Usage = st` |

为 nil 时整个记账消失（测试装配与既有用法不受影响），与 `newEventSink(rec==nil)` 同一立场。

`*agent.Result` 上新增一个字段（**唯一的解析侧改动**，见 §3.7）：

```go
type Usage struct {
    InputTokens, OutputTokens, CacheReadTokens, CacheCreationTokens int64
    Models []ModelUsage // [{Model, Input, Output, CacheRead, CacheCreation, CostUSD}]
    Present bool        // CLI 报没报 usage：没报 ≠ 报了 0
}
// agent.Result 增加两行：
//   Usage     Usage
//   CostKnown bool   // CostUSD 是否是 CLI 报的真实账单。false = 未知（codex），落库 NULL
```

- `Present=false` 时 token 记 0、`tokens_known=false`、`models='[]'`、`model=NULL`：不伪造数字。
- `CostKnown=false` 时 `cost_usd` 落 **NULL**，不落 0（第二轮 blocker 3）。claude 驱动在
  解析到 result 事件时置 `CostKnown=true`（`total_cost_usd` 是 result 事件的必有字段）；
  `res==nil` 的运行 `cost_usd` 同样落 NULL（真花了多少不知道），`status` 照记失败原因。
- **多条 result 时 `Usage` 怎么取（第三轮 blocker 3 / 第四轮 B1，与 f01 的接缝）**：`Usage`（连同 `CostKnown`）
  在解析循环里与 `CostUSD` 放在**同一组覆盖赋值**里，**取最后一条 result 的值，不跨 result 相加**；
  该条有 `modelUsage` 就用它各模型之和，没有就回落**该条**的 `usage`——回落路径同样只取末条，不把
  前几条的 `usage` 加进来，也不在「前一条有 `modelUsage`、末条没有」时混用两条的数据（末条整组胜出）。
  取值表与 f01 §3.2 第 5 条（R4）**逐字相同**，两份 PRD 各存一份、改一处必须同改另一处：

  | 字段 | 多 result 时的取值 | 依据 |
  |---|---|---|
  | `Usage`（整组：四个计数、`Models`、`Present`） | **随最后一条 result 覆盖，不跨 result 相加**；末条有 `modelUsage` 用它，没有就回落**末条**的 `usage`；末条两个键都没有 → `Present=false`、四项 0（末条整组胜出，不拿前几条补，不混用） | `modelUsage` 是进程累计（fixture 第二条 `inputTokens:6 = 2+4`、`cacheCreationInputTokens:18978 = 15746+3232`），末条即全量；回落路径的 `usage` 是单 turn 量，只取末条会少记前几 turn —— f03 §3.1 已写明并接受这个代价（只在缺 `modelUsage` 的旧 CLI 形状上发生、方向是少记） |
  | `CostKnown` | 与 `CostUSD` 同组，随末条覆盖（任一 result 到达即为 `true`） | `total_cost_usd` 是 result 必有字段、进程累计 |

  一句话：**`Usage`/`CostKnown` 在覆盖组，f01 的累加组只有 `NumTurns`/`DurationMS` 两个字段，不再扩。**
  - 依据：f01 fixture（`/opt/lathe-wt/evidence/f01/fixtures/replay-user.ndjson`，claude 2.1.322，同一进程
    两条 result，第 7、13 行）里 `modelUsage` 是进程累计：第二条 `inputTokens 6 = 2+4`、`outputTokens 12 = 6+6`、
    `cacheCreationInputTokens 18978 = 15746+3232`，`total_cost_usd` 0.078858 → 0.0976604；相加就是重复计账。
  - 现网就是这个形状：用本线 worktree 的 `parseStream` 喂这两条 result（不改代码），得到 2 个 result 事件、
    `CostUSD = 0.0976604`（末条）—— 覆盖赋值、末条胜出（`/opt/lathe-wt/evidence/f03/review-r3-b3-two-results-probe.txt`）。
    本线只是把 `Usage` 加进同一处赋值。
  - 回落路径的代价：`usage` 是**单 turn** 量（同一 fixture：`input_tokens` 2 → 4），多 result 且没有
    `modelUsage` 时只取末条会**少记**前几个 turn。接受这个代价：回落只是旧 CLI 形状的防御路径，
    少记方向安全；若要在回落路径上逐条相加，就得在同一个循环里同时维护「累计组」与「单 turn 组」两套
    `Usage` 语义，而 f01 已把「哪些字段相加」定义为 `NumTurns`/`DurationMS` 两个，不再扩。
  - **测试只有一条、只有本线写**：`TestResultEventUsageTakesLastResult`（AC-1），不论合入顺序都由本线交付；
    f01 不写 `Usage` 的测试，只在本线先合入时保证 rebase 后它一行不改仍绿（§3.7 f01 小节）。没有「由后合入者写」的
    第三条测试（f01 R3 的 `TestMultiResultUsageFollowsCumulativeRule` 已在 f01 R4 删除）。
  - `NumTurns` / `DurationMS` 的取法归 f01（f01 §3.2 第 5 条：合入后改为各条求和），本线不定义、只照读
    `res.NumTurns` / `res.DurationMS` 落库；f01 先合或后合都不影响本线的列语义。
  - codex 不受此条影响：codex 的 `turn.completed.usage` 是单 turn 量，§3.7 的 `UsageFromCodex` 对一次 Run 的
    各 turn 相加，那是 codex 驱动内部的映射，不经过 claude 的 result 循环。
- 为什么用一个 bool 而不是把 `CostUSD` 改成 `*float64`：`CostUSD` 有既有消费方
  （`persistAgentResult` → `tasks.agent_cost_usd`、`digest.go` 的 `costUsd`、PRD 的事件），
  改类型会让本线的改动面扩散到与用量无关的代码，也与 f06 的驱动改动顶在一起；
  加字段是纯增量，零值（false）对未改的驱动是「未知」，**默认就是安全的**。

**写入时机**：调用方在 `Run` 之前取 `startedAt := time.Now()`，`Run` 返回后取
`endedAt` 并**立即同步**调 `agent.RecordRun`（在判 `err` / `IsError` 之前），所以
任何失败分支都已经记过账。`context.WithoutCancel` + 3s 超时，保证任务被取消时
账本不跟着丢。落库失败 `slog.Warn` 后继续 —— 可观测性不能反过来成为任务的
故障源（`pipeline.go:1470-1480`）。

#### 状态判定与花费口径（blocker 8 的答复）

`store.RunStatus(res *agent.Result, err error) string`，纯函数，**按序**首个命中：

| # | 条件 | `status` | 说明 |
|---|---|---|---|
| 1 | `errors.Is(err, agent.ErrIdle)` | `killed` | 静默判死 |
| 2 | `errors.Is(err, context.DeadlineExceeded)` 或 `errors.Is(err, context.Canceled)`，或 `err` 文本含 `执行超时` | `killed` | 总超时 / 调用方 deadline / 取消：进程树被我们杀掉。总超时今天是裸 `fmt.Errorf`（`agent.go:337`），本线把它改成 `%w` 包 `context.DeadlineExceeded`（**只加包装，文案不变**，`TestRunTimeoutKillsEntireProcessTree` 的 `"超时"` 断言照过）；文本兜底只为防御同一语义的旧错误 |
| 3 | `errors.Is(err, agent.ErrNoResultEvent)` | `no_result` | CLI 自己退出但没给 result |
| 4 | `err != nil`（其余：启动失败、解析失败、非零退出但有 result、resume 失败…） | `error` | |
| 5 | `res != nil && (res.IsError \|\| !res.Success)` | `error` | CLI 报告失败（如 `error_max_turns`） |
| 6 | 其余 | `success` | |

花费口径：**只要 `res != nil` 就记 `res` 上的 cost / duration / turns / token，与 `err`
是否为 nil 无关**（金额只在 `res.CostKnown` 时落值，否则 NULL；token 只在
`res.Usage.Present` 时落值，否则 0 + `tokens_known=false`）。理由：`res` 非 nil 说明 CLI 已经报出了这次运行的终局账单，钱是真
花了；`status` 记失败原因、金额记真实花费，两件事分开。`res == nil` 时 `cost_usd`
记 **NULL**（钱多半花了，但花了多少不知道——不能记成 $0）、token 四列 0 且
`tokens_known=false`、`duration_ms` 记 `endedAt - startedAt`（墙钟，**不是** CLI 的
`duration_ms`，这是我们唯一知道的量）、`num_turns=0`。`subtype` 取 `res.Subtype`，
`res==nil` 时为 NULL（**不写** `err.Error()`，§3.6）。

PRD 一侧的注意点：`prd.runOnce` 把 `res.IsError || !res.Success` 也翻成 `err`
（`service.go:612-614`），所以记账必须在 `runOnce` **内部**、`s.Agent.Run` 返回的
那一刻做（拿原始 `res, runErr`），而不是在外层循环里拿已经被翻译过的错误 ——
否则一次报了 `error_max_turns`、花了 $0.4 的 plan 运行会被记成 `res=nil`、$0。
为此 `runOnce` 的签名在包内改为 `runOnce(ctx, spec runSpec, p agent.RunParams, timeout)`
（多带 `spec` 拿 `prdID/phase`；未导出函数，没有包外调用方）。`resume` 取
`p.Resume`：首跑 `false`，超时收尾与修正 `true`，三者 `started_at` 不同，各成一行。

### 3.2 阶段桶归并（唯一真相源：`store.UsageBucket`）

`store.UsageBucket(phase string, reviewRound bool) string`，**按序**首个命中：

| # | 条件 | bucket | 说明 |
|---|---|---|---|
| 1 | `reviewRound=true` 且 phase ∈ {`implement`, `implement-reentry`} 或匹配 `^fix-[0-9]+$` | `review` | PR review 那一轮里的**所有**任务侧运行：resume 续跑（`implement`）、resume 失败降级或无会话时的新会话（`implement-reentry`，`pipeline.go:713`、`implementPrompt` `:741-748`）、以及这一轮验证没过进入的修复回路（`fix-N`，`pipeline.go:1009`，修复回路在 review 轮照常运行）。它回答「改一版 review 一共值多少钱」，所以连同这轮的修复一起算 |
| 1b | phase = `review` | `review` | `agent_events_phase_check` 里预留的 `--from-pr` 二轮续跑标签（`0005_agent_events.up.sql:40`），今天无写入方；历史若有这种行，语义就是 review，不进 `other` |
| 2 | phase = `triage` | `triage` | 分诊。review 轮不跑分诊（`EntryImplement` 入口），`reviewRound` 对它无意义，统一归 `triage` |
| 3 | phase ∈ {`implement`, `implement-reentry`} 或匹配 `^fix-[0-9]+$` | `implement` | 修复回路是实现的延续（与 `cost.go:141-147` 同口径）。`implement-reentry` 的事件今天被 `agent_events_phase_check` 拒收、不出现在任何聚合里（§2.1），本表是这一阶段唯一的账 |
| 4 | phase ∈ {`verify`, `push`} | 同名 | 目前没有 agent 运行落在这两个桶（事件不是运行），保留位 |
| 5 | phase = `plan` | `prd_plan` | PRD 规划对话：每轮的首跑、超时收尾、每次修正各一行 |
| 6 | phase = `plan-review` | `prd_review` | PRD 对抗复核（同上，每次尝试一行） |
| 7 | phase = `recommend` | `preview` | 预览推荐 |
| 8 | 其它 | `other` | 兜底，前端显示原文 |

`review_round` 的写入值：`rc.plan != nil && rc.plan.Review`，在 `runAgent` 里读，
**不**看 `rc.review` 是否有意见 —— 「没有意见」时 setup 已把入口降级为
`EntryVerify`（`pipeline.go:493-500`），根本不会调 agent。非任务运行（prd / preview）
恒 `false`。

同时被写入端（决定 `bucket` 列）与查询端（防御旧数据、`bucket=` 参数校验）调用。
0039 回填的 CASE 与它在 **`reviewRound=false` 这半张表上**同构（回填拿不到
review 信息，§3.1/0039），由 AC-6 对拍；`reviewRound=true` 那半张表只由
`TestUsageBucket`（AC-5）守。

### 3.2.1 验证档位维度从哪来（第一轮 blocker 2；第二轮 blocker 1 推翻了原方案）

**选 (b)：写入时按尝试快照，`agent_runs.verify_tier` 列；定档前写下的行在本任务第一次
定档时补一次（只补 NULL，有值的行永不改写）。查询直接按 `agent_runs.verify_tier` 分组，
不 JOIN `tasks`。**

第二版选的是 (a)「查询时 JOIN `tasks.verify_tier`、取任务当前档位」，理由 4 写着
「Fresh 走新任务行、今天没有改档路径」——**这是事实错误**（§2.1 第 5 行）：Fresh 重试
在同一个 `tk.ID` 上重新分诊、重新实现、在 `stageVerify` 里重新 `ClassifyTier`，再经
`COALESCE($7, verify_tier)` 覆盖旧档位。按 (a)，一个第一次定 light、Fresh 后定 heavy 的
任务，它第一次尝试里按 light 档跑的全部运行会整体搬进 heavy 桶——而那几次运行实际承受的
是 light 档的验证强度，「heavy 档花了多少」被高估、「light 档花了多少」被低估，
这个维度就失真了。

**写入规则**（`RunRecord` 新增 `VerifyTier string`，空串写 NULL）：

| 运行 | `verify_tier` 写什么 | 依据 |
|---|---|---|
| 任务运行（triage / implement / implement-reentry / fix-N / review 轮） | `string(rc.tier)`：**本次 Execute 内存里的档位**，不读 `tasks.verify_tier` | Fresh 时 `rc.tier` 为空（`pipeline.go:455-457` 提前返回，`:485-487` 不执行），而 `tasks.verify_tier` 此时还挂着**上一次尝试**的旧档位——读库会把 Fresh 的分诊记成旧档。断点续跑时 `rc.tier` = 首次定档（`:485-487`），修复轮与 review 轮都带着它 |
| 预览推荐（`run_kind=preview`） | store 在 `INSERT … SELECT` 里取 `tasks.verify_tier`（与 user_id/repo_id 同一条语句） | 预览不在流水线里，没有 `rc`；它总在某次尝试**之后**由人触发，任务行上的档位就是它所看的那份 diff 的档位。唯一的偏差是「Fresh 正在跑、尚未重新定档时有人点了推荐」会记成旧档——窗口小、写进 docs |
| PRD 运行 | NULL（`agent_runs_tier_kind_check` 强制） | 与任务无关 |

**定档回补**：`stageVerify` 里 `Transition(…, StateVerifying, {VerifyTier})` 成功之后，
调一次 `agent.UsageRecorder.SetAgentRunTier(ctx, taskID, tier)`：

```sql
UPDATE agent_runs SET verify_tier = $2
 WHERE task_id = $1 AND run_kind IN ('task','preview') AND verify_tier IS NULL
```

- 补的是「本任务里此前没有档位的行」= 本次尝试定档前的分诊/实现/降级重入，以及更早的、
  **从未走到定档**就失败的尝试的行（那几次尝试没有自己的档位，归到第一个给出档位的尝试，
  语义上就是「这张单子为拿到这一档花的前期成本」）；
- **已有档位的行永远不动**：上一次尝试按 light 跑的 fix-1 仍是 light；Fresh 之后定 heavy，
  只补本次的 NULL 行。这就是第二轮 blocker 1 要求的「按运行时刻取档」；
- 与 `persistVerifications` 同一立场：失败只 `slog.Warn`，不影响流水线；没补上的行留 NULL，
  **下一次定档时会被同一条 UPDATE 补上**（规则是幂等的，不存在「补错」只存在「晚补」）；
- 它不是状态转移、不改 `Transition` 的参数与语义，`internal/task/state.go`、
  `machine.go` 零改动；写点就在 `pipeline.go:936-943` 拿到 `rc.tk = tk` 之后一行。

**查询口径**：`tier` 维度 = `COALESCE(agent_runs.verify_tier, 'none')`。`none` = 「这次
运行发生时及之后都还没定过档」（分诊判 `blocked_spec`、实现失败、PRD 运行）。

**仍然保留的取舍与代价**（写进 docs）：

1. 「heavy 单子一共花多少」与「heavy 档的运行花了多少」现在是两个问题。本线回答后者
   （tier 维度按运行）；前者用 `group=task&tier=…` 不能直接得到——一个改过档的任务会同时
   出现在 light 与 heavy 两个桶。这正是改档的真实情况：它确实在两档各跑了一遍。任务详情页
   的「用量」卡逐行显示 `verifyTier`，改过档的任务一眼可见。
2. 定档前的运行不是「当时就有档位」，是事后补的；补的依据是**同一任务第一次在其后定的档**，
   不是任务的最终档。一个「尝试 1 定 light → Fresh → 尝试 2 定 heavy」的任务：尝试 1 的
   分诊/实现/fix 记 light，尝试 2 的分诊/实现/fix 记 heavy；反方向（heavy → light，AC-7b 用的就是这个
   方向，因为它能在测试夹具里真实跑通）同理。
3. 每次定档多一条按 `task_id` 的 UPDATE。`WHERE run_kind IN ('task','preview') AND task_id = $1`
   能走唯一约束索引的前两列 `(run_kind, task_id, …)`（btree 对首列的 `= ANY` 做多次下探），
   一个任务的行数是个位到两位数，不需要新索引。
4. 回填行（0039）的档位只能取回填时刻任务行上的档位（§3.1/0039），对历史上改过档的任务
   不准；回填行带 `backfill=true`，页面提示。

`tier=` 参数：`light` / `heavy` / `none`；`none` = `agent_runs.verify_tier IS NULL`。

### 3.3 API

新增两条（`internal/httpapi/api.go` 的 `Routes`，与 `/api/stats/cost` 并列；
**不复用** `/api/stats*`：那是看板 5 秒轮询的端点，用量是决策视图，理由同
`api.go:177-185`）：

```
GET /api/usage?from=&to=&repo=&tier=&bucket=&group=&limit=
GET /api/tasks/{id}/usage
GET /api/admin/usage?<同上，无属主过滤>
```

`GET /api/usage`

| 参数 | 取值 | 默认 |
|---|---|---|
| `from` / `to` | RFC3339 或 `YYYY-MM-DD`（UTC 日界） | 近 30 天（`to=now`） |
| `repo` | 仓库 id（**非属主 → 404**，不区分「不存在」与「不是你的」） | 全部 |
| `tier` | `light`/`heavy`/`none`（按**运行所属尝试**的档位快照，`none` = 未定档或 PRD 运行，§3.2.1） | 全部 |
| `bucket` | §3.2 的 9 个桶名之一（`triage`/`implement`/`review`/`verify`/`push`/`prd_plan`/`prd_review`/`preview`/`other`），按 `agent_runs.bucket` 等值过滤；未知值 400 | 全部 |
| `group` | `phase` / `repo` / `tier` / `day` / `task` / `model` | `phase` |
| `limit` | 整数 1–200。**任何 `group` 下都校验**（越界、非整数 400），只在 `group=task` 时生效，其它 `group` 下合法值被忽略 | 20 |

**没有 `phase=` 参数**（第三轮 blocker 2-⑤）：第三版的 URL 模板里写了 `phase=` 却没定义。原始 phase
（`fix-1`/`fix-2`…）是 `group=phase` 的**分组键**，不是筛选维度；按阶段筛选用 `bucket=`。带 `phase=` 的
请求按「未知参数」处理——与其它未知参数一样**忽略**（既有读接口的立场：`api.go:93`、`:156` 只读自己认识的键），
不 400：未知参数 400 会让前端加一个新参数就打挂旧后端的滚动升级。AC-10 有一例守「`phase=fix-1` 不改变结果」。

非法参数一律 **400 + 中文原因**（`?from=abc`、`?to<from`、`?limit=0`、`?limit=201`、`?limit=abc`、
未知 `group`、非数字 `repo`、未知 `tier`、未知 `bucket`），不静默回落默认值 —— 静默降级会让一个写错的
面板长得像「没数据」。

响应（所有集合字段空时是 `[]`、对象是 `{}`，**不是 null**，沿用 `cost.go:101-108`
的约定）：

```jsonc
{
  "from": "...", "to": "...",
  "totals": {"runs":12,"costUsd":0.9123,"durationMs":1810000,"turns":48,
             "inputTokens":1200,"outputTokens":8800,
             "cacheReadTokens":0,"cacheCreationTokens":410000,
             "tokensKnown":9,           // tokens_known=true 的运行数（其余是历史/被杀/未上报）
             "costKnown":10},           // cost_usd IS NOT NULL 的运行数；costUsd 只是这些行之和
  "byBucket":  [{"key":"implement","runs":5,"costUsd":0.5,"durationMs":...,"tokens":{...}}],
  "byGroup":   [{"key":"fix-1","runs":1,"costUsd":0.08,...}],
  "byDay":     [{"key":"2026-11-10","runs":3,"costUsd":0.2,...}],
  "byModel":   [{"key":"claude-opus-5-5","runs":12,"costUsd":0.91,...}]
}
```

**`byGroup` 的键与排序**（第三轮 blocker 2-①）：

| `group` | `key` | 每项额外字段 | 排序 | 截断 |
|---|---|---|---|---|
| `phase` | 原始 phase（`triage`/`implement`/`fix-1`/`plan`…） | `bucket` | `costUsd` 降序（NULL 最后）→ `runs` 降序 → `key` 升序 | 不截断 |
| `repo` | 仓库 id 的十进制字符串 | `repoId`、`providerRepo`（属主自己的仓库名；admin 端点不给） | 同上 | 不截断 |
| `tier` | `light` / `heavy` / `none` | — | 固定顺序 `light, heavy, none`，只出现有行的 | 不截断 |
| `day` | `YYYY-MM-DD`（`started_at` 的 UTC 日） | — | `key` 升序 | 不截断 |
| `task` | 运行目标：`task:<id>` 或 `prd:<id>`（PRD 运行没有任务，按 PRD 自成一组，不丢） | `kind`（`task`/`prd`）、`taskId` 或 `prdId`；**标签字段**（来源写死，见下；admin 端点一个都不给）：任务项 `externalKey`、`trackerProvider`、`issueTitle`，PRD 项 `oneLiner` | `costUsd` 降序（NULL 最后）→ `runs` 降序 → `key` 升序 | 取前 `limit` 组；响应加 `"groupTotal": N`（截断前组数），前端据此显示「另有 N−limit 个」 |
| `model` | 同 `byModel` | 同 `byModel` | 同 `byModel` | 不截断 |

「`costUsd` 降序 NULL 最后、再 `key` 升序」让排序**全序**，`limit` 截断的结果确定。

**`group=task` 标签字段的来源**（第五轮 B1）：库里**没有**「任务标题」——`tasks` 只有 `external_key` /
`external_id`（Linear issue 标题从不落库，`TaskRow` 也没有 Title，`internal/store/query.go:20-50`），
`prds` 只有 `original_input` / `document`（`migrations/0023_prds.up.sql`）。本线**不新增标题列、不在读路径上调 Linear**
（读接口调外部平台 = 用量页的可用性绑在 Linear 上，且属主的凭据要进聚合路径）。标签只取库里已有的列，来源逐字段写死：

| 字段 | 出现在 | 来源 | 何时为 `null` |
|---|---|---|---|
| `externalKey` | 任务项 | `tasks.external_key`（NOT NULL，Linear 的 `ENG-123` 或内置的 `LT-1042`） | 不为 null |
| `trackerProvider` | 任务项 | `tasks.tracker_provider`（`linear`/`internal`），前端据此渲染成 Linear 链接或内置工单页链接，与任务列表同口径 | 不为 null |
| `issueTitle` | 任务项（键总在） | `issues.title`，`LEFT JOIN issues i ON t.tracker_provider = 'internal' AND i.user_id = t.user_id AND i.key = t.external_key`——**逐字照抄** `query.go:100-101` 的既有 JOIN 条件，不另写一套 | Linear 任务恒为 `null`；内置工单已删时 `null` |
| `oneLiner` | PRD 项（键总在） | `prds.document->'oneLiner'->>'body'`（`internal/prd/document.go:146` 的 §1 一句话；`export.go:73` 导出时也拿它当标题），Go 侧 `TrimSpace` 后**按 rune 截到 80**（AGENTS.md §9） | `document IS NULL`（还没跑完第一轮）或 body 为空白 |

任务表本身也经 `t.user_id = r.user_id` JOIN（`agent_runs.user_id` 是写入时快照，二者恒等；条件写上是让「标签只来自属主自己的行」
在 SQL 里可见，不靠不变量）。前端显示：任务项 = `externalKey` + （有则）`issueTitle`；PRD 项 = `oneLiner`，为 null 时 `PRD #<prdId>`。
不叫 `title`：同一个键名在 Linear 任务上永远是空的，会让人以为是 bug。

**admin 端点不带任何标签字段**：`/api/admin/usage` 的 `byGroup` 任务项只有 `key`/`kind`/`taskId` 或 `prdId` 与计数，
仓库项只有 `repoId`——**不出现** `externalKey`、`trackerProvider`、`issueTitle`、`oneLiner`、`providerRepo`。
做法是结构上的而不是过滤上的：admin 走的 store 函数**不做**上面三处标签 JOIN，httpapi 用一个不含这些字段的独立响应结构体，
不存在「先查出来再置空」的路径。AC-9 的禁用键清单与这里逐字一致。

**`byModel` 的定义**（第三轮 blocker 2-②）：`byModel` 总是返回、与 `group` 无关；`byGroup` 按 `group`。

- 每行 `agent_runs` 按 `models` 展开：`models` 非空的行，每个元素贡献到它自己的 `key=model`：
  该元素的四个 token 计数、`costUsd` = 该元素的 `costUSD`、`runs` 计 1（同一行有两个模型时**两个 key 各计 1**）；
- `models='[]'` 的行（codex 运行、`res==nil` 的被杀/无结果运行、回填行、`Present=false` 的运行）**不丢**，
  整行贡献到保留键 **`"(unknown)"`**：行上的四个 token 计数、行上的 `cost_usd`（可为 NULL，照常进
  `costKnown`）、`runs` 计 1。`"(unknown)"` 不可能是真实模型名（claude 模型 id 不含括号；codex 不报模型名，
  本来就落这里）；
- 因此恒有：`Σ byModel.{inputTokens,outputTokens,cacheReadTokens,cacheCreationTokens} == totals` 的对应项（**精确相等**，
  写入时行上四列就是 `models` 各元素之和，§3.1）；`Σ byModel.runs ≥ totals.runs`（多模型运行被各计一次，
  只在全是单模型/无模型行时相等）；`Σ byModel.costUsd` 与 `totals.costUsd` 只在「每行各模型 `costUSD` 之和 =
  行上 `total_cost_usd`」时相等——CLI 自报的两个数，本线不强行对齐，前端不在 byModel 上显示「合计」；
- 每项另带 `tokensKnown` / `costKnown`；排序 `costUsd` 降序（NULL 最后）→ `key` 升序，`"(unknown)"` 不特殊排位。

每个分组对象都带同样的 `tokensKnown` / `costKnown`。`costUsd` 是 `SUM(cost_usd)`（自然跳过
NULL）；当 `costKnown == 0` 时 `costUsd` 返回 **`null`** 而不是 `0`——前端据此显示「未知」
而不是「$0」（这是本响应里唯一允许为 null 的数值字段，空集合仍是 `[]`）。

`GET /api/tasks/{id}/usage`（任务详情页用）：
- 非属主 **404**（走 `Store.TaskDetail(ctx, id, CurrentUser(r).ID)` 的既有语义，
  `api.go:130`，不新造规则）；
- 返回 `{"summary": {...同 totals...}, "runs": [{phase,bucket,runKind,reviewRound,verifyTier,status,
  model,models,costUsd,durationMs,numTurns,tokens,tokensKnown,startedAt,endedAt,resume,backfill}]}`
  （第三轮 blocker 2-③ 把语义逐项定死）：
  - 范围 = 该任务的**全部**行（`run_kind IN ('task','preview')`，**不受时间范围限制**，不接受 `from/to` 等参数——
    详情页看的是这张单子一共花了多少）；`summary` 与 `runs` 来自同一个集合，`summary.runs == len(runs)`；
  - `runs` 按 `started_at` 升序，并列时按 `id` 升序（全序）；空任务 → `runs: []`、`summary.runs=0`、`summary.costUsd=null`；
  - `costUsd`：`null` = 费用未知（`cost_usd IS NULL`），`0` = 确实 $0，二者不混；
  - `verifyTier`：`null` = 这次运行写入时与之后都还没定档（§3.2.1），否则 `light`/`heavy`；
  - `reviewRound`：恒为布尔、不为 null；`models`：恒为数组（无明细时 `[]`）；`model`：`null` = 未知；
  - `tokens`：`{input,output,cacheRead,cacheCreation}` 四个整数；`tokensKnown=false` 时前端把它们显示为「—」而不是 0。

`GET /api/admin/usage`：`RequireAdminFunc`（`auth.go:352-365`），普通成员 **403**
（与 `/api/admin/*` 既有行为一致，不新造 404），参数与 `/api/usage` 相同但
**不接受 `repo` 的属主校验**；返回额外含 `byUser: [{userId, runs, costUsd, ...}]`（**不截断**，按 `userId` 升序，
每项带 `tokensKnown`/`costKnown`）。**刻意不含** email 与任何任务/PRD/仓库标签（`externalKey`、`trackerProvider`、
`issueTitle`、`oneLiner`、`providerRepo`，见上文「admin 端点不带任何标签字段」与 §3.6）。

### 3.4 UI

1. **新页面 `/usage`（用量）**：时间范围（今日 / 7 天 / 30 天 / 自定义）+ 仓库 +
   档位 + 阶段桶 四个筛选，`group` 切换（阶段 / 仓库 / 档位 / 天 / 任务 / 模型）。
   - 首次进入按「近 30 天 · 阶段」；
   - 筛选与 `group` 写进 query string（可分享、可刷新不复位）；
   - 数字卡 + 表格为主；趋势用单序列面积图，配色与校验流程照抄
     `CostPanel.vue:1-20` 已经写明的调色结论（blue/orange/aqua 三槽，
     已验证亮暗两模式 CVD 分离度），不新造一套；**表格视图始终可用**（颜色不是唯一通道）；
   - `tokensKnown < runs` 时在 token 列旁显式标注「N 次运行无 token 数据」，
     不把那部分当 0 摊进平均；
   - `costKnown < runs` 时在费用列旁标注「另有 N 次费用未知」（典型：codex 驱动、被杀的运行），
     `costUsd == null` 时费用格显示「未知」——**「未知」与「$0.0000」绝不共用一个渲染**；
   - 空数据渲染 `—` 与「该范围内没有运行记录」，不渲染 0 美元（0 与「没有」是两件事）。
2. **`CostPanel` 保持不动**（它是 T5 的验收物），底部加一行链接「按阶段 / 仓库 /
   档位细看 → /usage」，并注明两者口径差异（见 §4）。
3. **任务详情页**：新增「用量」卡（新组件 `web/src/components/TaskUsage.vue`），
   列出该任务每次运行；同时把 `TaskDetail.vue:388-389` 两个徽章的语义改成
   「最近一轮」并加 `title` 说明（它读的 `agentCostUsd` 是覆盖值，
   只改前端文案，不改 `tasks` 表）。
4. 导航：`web/src/App.vue:49-59` 加 `用量`；路由 `web/src/main.js:32-58` 加 `/usage`；
   管理员在 `/users` 页顶加一个「全局用量」入口指向 `/usage?scope=all`
   （该开关仅对管理员显示，后端仍由 `/api/admin/usage` 强制鉴权）。
5. `web/src/api.js`：`usage(params)` / `adminUsage(params)` / `taskUsage(id)`。

**不引入任何前端依赖**（`web/package.json` 不动），图表继续手写 SVG。

### 3.5 配置项

**本功能不新增任何环境变量或可配项。** 逐项交代「谁消费」：

- 时间范围 / 筛选 / 分组 → 都是**请求参数**，没有服务端默认值可配；
- 回填条数、桶归并规则 → 编译期常量（`store.UsageBucket`），不配置化；
- token / 费用来自 CLI 自报（`total_cost_usd`），**不做价格表**，因此没有「单价配置」；
- 保留期清理 → 明确**不做**（§4）；真要做得另开一条带消费方的需求
  （「删掉最后一个消费方时连配置项一起删」的反向同理：没有消费方就不加开关）。

因此 README 变量表本次**无新增行**（§7 只写一句「无新增变量」）。

### 3.6 安全

1. **账本零敏感内容**：`agent_runs` 只存模型名、计数、时长、CLI 给的短标签
   （`subtype`，≤64 rune）。不存 prompt、不存 agent 输出、不存错误全文 ——
   日志/界面/事件流不出明文的红线（AGENTS.md §7）在数据模型层就不成立。
   `subtype` 只取 CLI 给的短枚举（`success` / `error_max_turns` / `completed` …），
   由 `store` 侧一个 64-rune 白名单式截断函数收敛，**不写入 CLI 的任意文本** ——
   这与 `runner.redactText`（`internal/runner/redact.go:468`，包内小写、不跨包导出）
   的权威层是互补关系：那边管失败摘要，这边压根不给任意文本进入的机会。
2. **属主隔离**：所有普通端点经 `usageScope(userID)` 这一个函数拼
   `WHERE user_id = $n`，**没有 `userID=0` 表示全部的魔法值**；仓库筛选先
   `Store.GetRepo(ctx, id, userID)`（`internal/store/query.go:336`）拿不到就 404。
   AC-8 对每个端点逐一断言非属主 404。
3. **管理员全局视图是显式的、有意的口子**：`api.go:60-63` 那句「跨用户读取的
   后门宁可没有」在本 PR 里被**部分修订**并在注释与 docs 里写明理由：需求要求
   管理员看全局；做法是 `RequireAdminFunc` + 新端点 + **只返回聚合与
   `user_id`**，不返回 email、`externalKey`/`issueTitle`/`oneLiner`/`providerRepo` 等任何标签、任务内容。同期把 `api.go:60-63` 的
   注释改成「除 `/api/admin/*` 的显式聚合外，读接口全部按属主隔离」，避免
   注释与代码打架（AGENTS.md §8：文档与代码冲突时停下确认 —— 这里是把确认
   结论写进代码）。
4. **凭据加密**：本功能不读、不写、不展示任何凭据，`internal/creds`、
   `internal/secret` 零改动。
5. **错误响应**：聚合失败走既有 `serverError`（`api.go:866`），不把 SQL 错误
   原文回给前端。

### 3.7 并发与多节点

- **写**：单条 `INSERT ... SELECT FROM tasks|prds ... ON CONFLICT ON CONSTRAINT
  agent_runs_attempt_uniq DO NOTHING`，无读改写、无显式事务，不存在锁竞争；
  多节点同时写各自的行。唯一约束（`NULLS NOT DISTINCT`，§3.1）按「一次尝试」去重，
  重试（`retry.go` 的断点续跑）会新开 `started_at`，因此**会**记成新的一行 ——
  这是对的：那是一次新的真实花费。
- **读**：纯聚合、无写回、快照读；不在 `/api/stats` 的 5 秒轮询里
  （理由同 `cost.go:122-126`）。
- **失败隔离**：`RecordAgentRun` 失败只告警；已用 `grep` 确认它不在
  `BatchSink` 的热路径上（每 Run 一次，不是每事件一次）。
- **f01（运行中插话）边界**（第三轮 blocker 3 新增；与 f01 PRD §3.2 第 5 条、§4 f03 行同一说法）：
  - **问题**：f01 用 stream-json 输入在运行中插话，插话会让同一个进程开出第二个 turn，一次 `Run`
    就有了两条 result。f01 已用真实 fixture 实测：`total_cost_usd`、`modelUsage` 是进程累计，`usage`、
    `duration_ms`、`num_turns` 是单 turn 量（f01 §2.2 第 7 条）。
  - **口径（两边一致，只有一条；取值表与 f01 §3.2 第 5 条逐字相同，见 §3.1）**：`Result.Usage` 与 `Result.CostKnown`
    放在 claude 解析循环里与 `CostUSD` **同一组覆盖赋值**，**随最后一条 result 覆盖，不跨 result 相加**；末条缺
    `modelUsage` 时回落**末条**的 `usage`；末条两个键都没有 → `Present=false`、四项 0。
    `NumTurns` / `DurationMS` 改成相加由 f01 定（f01 §3.2 第 5 条），本线只照读、不定义。
  - **先合入者不依赖后合入者**（与 f01 §3.2 第 5 条的两段逐条对应）：
    - **f01 先合入**：`dev` 上没有 `Usage`/`CostKnown`，有 f01 的「`NumTurns`/`DurationMS` 累加、其余覆盖」（可能已抽进
      `parseStreamWith`）与 fixture `internal/integration/agent/testdata/replay-user.ndjson`。本线 rebase 时把两个字段加进
      **覆盖组**那一侧，AC-1（含 `TestResultEventUsageTakesLastResult`）原样跑绿。
    - **f03 先合入**：本线在 `agent.go:418-432` 的现有赋值块里加 `Usage`/`CostKnown`，`TestResultEventUsageTakesLastResult`
      用 f01 的 fixture（本线 PR 拷进 `testdata/`，同名同内容；f01 rebase 时复用，不重复拷）在**现网循环**上就能跑——现网循环
      本来就逐条处理多个 result、末条覆盖（§3.1）。f01 rebase 时**只**把 `NumTurns`/`DurationMS` 改成累加，**不得**把
      `Usage`/`CostKnown` 移出覆盖组、不得为回落路径加任何相加逻辑；这条测试**一行不改**仍须绿，f01 PR 里贴它 rebase 后的输出。
    - 任一顺序下：本线的 `TestResultEventParsesUsage`（单 result）、`TestResultEventUsageTakesLastResult`（多 result）与 f01 的
      AC-4（多 result 的 `NumTurns/DurationMS/CostUSD/Text`）**都不改断言**仍须绿。本线不需要 f01 的任何类型或函数，f01 也不需要本线的。
  - **账本侧不受影响**：一次 Run 仍是一个进程、一行 `agent_runs`（`RecordRun` 在 `Run` 返回后调一次）；
    插话不产生额外的行。插话带来的额外 token/cost 已经包含在末条 result 的累计值里。
  - **PR 描述**：本线 PR 描述写「`Usage`/`CostKnown` 随末条 result 覆盖、回落也只取末条、不跨 result 相加（与 f01 §3.2 第 5 条同一张表）」，
    并贴 `TestResultEventUsageTakesLastResult` 的输出——与 f01 §8 第 6 条要求两边都写的那句对应。

- **f06（多 CLI 驱动）边界**（第二轮 blocker 3 修订；f06 PRD §4 边界表同步改成同一说法）：
  - **本线做的驱动层改动**：`agent.Result` 加 `Usage` 与 `CostKnown` 两个字段、claude 解析处一处
    赋值（`agent.go:420-432`，放在末条覆盖组里，见上面 f01 小节；f06 若把这段抽进 `ClaudeDriver`，规则随之搬过去）、总超时补一层 `%w context.DeadlineExceeded`（`agent.go:337`，文案
    不变，§3.1「状态判定」）、新文件 `agent/usage.go`（`RunRecord` / `UsageRecorder` /
    `TierBackfiller` / `RecordRun` / `UsageFromCodex`，纯类型、薄包装与一个纯映射函数）。
  - **codex 的 `Result.Usage` 由两条线里后合入 dev 的一方填**，规则只有一份（下表），两份 PRD
    逐字一致：
    - f03 先合：f06 rebase 后在 codex 驱动构造 `Result` 处调用 `agent.UsageFromCodex(usage)`
      填 `Result.Usage`（含 `Present=true`），`CostKnown` 保持零值 `false`；
    - f06 先合：f03 rebase 时在 f06 的 codex 驱动同一处补这一行赋值，并补 `TestCodexResultUsage`
      （见 AC-1b）；
    - **不论谁先合，`CostKnown` 都不需要 codex 驱动做任何事**：它是本线新加的字段，零值就是
      「未知」，codex 驱动不置位 → `cost_usd` 落 NULL。f06 的 `CostUSD = 0` 保持不变（那是给
      `tasks.agent_cost_usd` 与事件 body 的既有字段），本线不读它来判断「免费」。
  - **codex usage → `agent.Usage` 映射**（`UsageFromCodex`，纯函数，对 `turn.completed.usage` 的
    对象；样本：`/opt/lathe-wt/evidence/f06/probes/transcript.txt` 第 6/15/22 行，codex-cli 0.146.1）：

    | `agent.Usage` | codex `usage` 键 | 说明 |
    |---|---|---|
    | `CacheReadTokens` | `cached_input_tokens` | |
    | `CacheCreationTokens` | `cache_write_input_tokens` | |
    | `InputTokens` | `input_tokens − cached_input_tokens − cache_write_input_tokens`，下限 0 | OpenAI 口径的 `input_tokens` **含**缓存部分，claude 口径的 `input_tokens` **不含**；减掉后两边同义，`input+cacheRead+cacheCreation` 才都等于「总输入」。样本里缓存两项都是 0，这条减法的语义以 codex 文档为准、未被样本证伪，写进 docs 的已知假设 |
    | `OutputTokens` | `output_tokens` | `reasoning_output_tokens` 已含在其中（OpenAI 口径），不另加，避免重复 |
    | `Models` | `nil`（落 `models='[]'`、`model=NULL`） | codex 的 `turn.completed` 不带模型名；不猜 |
    | `Present` | 一次 Run 里至少见到一条带 `usage` 对象的 `turn.completed` → `true` | `turn.failed` 没有 usage；只有失败终局 → `false`（token 未知，不是 0） |

    一次 Run 里多条 `turn.completed`（`NumTurns>1`）时逐条**相加**：每条 `usage` 是该 turn 的量。
    `tokensKnown` 因此会把 codex 运行算进「有 token 数据」——它确实有；`costKnown` 不算——它确实没有。
  - **为什么不让 f06 一并做、也不让本线一并做 codex 驱动**：codex 驱动整体属于 f06（§4），本线
    不实现别人的驱动；而映射口径（与 claude 同义、哪些键相加、未知怎么表示）是账本语义，属于
    本线。所以「规则 + 映射函数」在本线，「一行赋值」在驱动，后合入的一方负责把两者接上，
    且有 AC-1b 守着不让任一方合入时漏接。
  - **费用折算（token × 单价）仍然不做**（§3.5：不做价格表、没有消费方的单价配置不加）。codex
    运行在 `/usage` 上显示 token、费用显示「未知」。

---

## 4. 范围外

| 不做 | 归属 / 理由 |
|---|---|
| **预算拦截**（超预算暂停/拒绝运行） | 需求明说「不做预算拦截」。这是产品边界级改动（要动状态机与验证准入），不属于本条线 |
| 事件流的呈现改造（分组、折叠、高亮、按需加载） | f04 `feat/run-replay`。本功能只读 `agent_runs`，不碰 `agent_events` 的展示 |
| **修 `agent_events_phase_check` 拒收 `implement-reentry`**（既有 bug：该阶段事件含 result 被 `23514` 整批丢弃，CostPanel 看不到、0039 也回填不了） | f04 `feat/run-replay` 已在其 PRD §2.2-2 认领（放宽 CHECK 的迁移在 f04 号段）。本线**不改** `agent_events` 的约束：两条线改同一条 CHECK 必然冲突。本线的立场是 —— `agent_runs` 不依赖事件表，是这一阶段**唯一**的账，f04 合不合都不影响本线的 AC；f04 合入后，也不对 `implement-reentry` 做二次回填（那时事件表里只有 f04 之后的新行，`agent_runs` 已经有它们） |
| 运行中插话本身（stdin 流式输入、注册表、插话的落库与 UI）；多 result 时 `NumTurns`/`DurationMS` 的取法 | f01 `feat/interject`。本线只定义 `Result.Usage`/`CostKnown` 在多 result 下**随末条覆盖、不跨 result 相加**（§3.1、§3.7 f01 小节），取值表与 f01 §3.2 第 5 条逐字相同；谁先合都不依赖对方（接缝只是这两个字段放进覆盖组），由 AC-1 的 `TestResultEventUsageTakesLastResult` 守，该测试只由本线写 |
| 把 `Result` 抽象成统一事件模型、codex 驱动本身 | f06 `feat/multi-cli`。本线在 `agent.Result` 上加 `Usage`、`CostKnown` 两个字段与映射函数 `UsageFromCodex`；codex 驱动里调用它的那一行由**后合入的一方**写（§3.7） |
| codex / 任何驱动的费用折算（token × 单价） | 不做，也不归 f06：没有价格表、没有单价配置的消费方（§3.5）。codex 费用 = **未知（NULL）**，不是 $0 |
| 通知（用量超阈值提醒等） | f09 `feat/notify-channels`。本功能只读不推 |
| 收件箱里暴露「用量异常」事项 | f02 `feat/inbox` |
| 团队 / 角色带来的**共享可见性**（成员看团队仓库其他成员的用量） | f12 `feat/team-roles`。本功能写死「属主 = `agent_runs.user_id`」；f12 落地时把它换成统一的可见性函数即可（§3.6 的 `usageScope` 是唯一入口，正是为此留的） |
| PAT、外部 API 客户端（`/api/usage` 走既有会话/Bearer 鉴权） | f05 `feat/personal-api-token` |
| 保留期 / 归档 / 清理旧 `agent_runs` | 无消费方的配置项不加；数据量与事件表同级，等有了容量实测再提 |
| CSV / 报表导出 | 未在需求里；先把手上的看板做对 |
| 改 `CostStatsFor` / `CostPanel` 的数据源 | 保持 T5 验收物不变。**口径差异**：`CostPanel` 只统计 `agent_events` 里的任务运行，不含 PRD/预览，也会漏掉被 sink 丢弃的行；`/usage` 是完整账本。差异写进 `/usage` 页的说明与 `docs/features/usage-stats.md`，不在本 PR 里合并两者 |
| 改 `tasks.agent_cost_usd` 的写入语义 | `tasks` 表、`persistAgentResult` 不动；只改任务详情页徽章文案（§3.4.3） |
| 回填历史 token | 历史 payload 里就没有；回填行 token 记 0 且 `models='[]'`，前端标注「无 token 数据」 |
| 改任务状态机 / 验证判定 / 非属主 404 | 一律不动。管理员 403 沿用 `/api/admin/*` 既有语义，不是对 404 规则的修改 |

---

## 5. 验收标准

每条都给出**可执行的验证方式**。测试均为表驱动 + `t.Run`，用例名写清
「给定什么、断言什么」，失败信息带 `got/want`（`skills/go-testing/1.0.0/SKILL.md`）。
数据库用例走 `internal/testsupport.SkipOrFail`，`make test`（`-p 1`）
与 `make test-ci`（`LATHE_TEST_REQUIRE_DB=1`，`Makefile:52-56`）两种模式都要能跑。

### 先红后绿（全组前置）

每条 AC 对应的测试必须**先在改动前失败一次**并把失败输出落到
`/opt/lathe-wt/evidence/f03/red-<AC>.txt`，再实现到绿。PR 描述里附
「红证据 → 绿输出」两段。命令模板：

```bash
cd /opt/lathe-wt/f03-usage && source /opt/lathe-wt/env/f03.sh
git stash push -- <实现文件>            # 只留测试
go test ./internal/... -run 'TestUsage' -count=1 2>&1 | tee /opt/lathe-wt/evidence/f03/red-AC1.txt
git stash pop
```

### AC-1 驱动解析 token 用量

- 测试：`internal/integration/agent/agent_test.go::TestResultEventParsesUsage`
- 输入：`/opt/lathe-wt/evidence/f03/claude-result-event-sample.json`（真实样本，
  已脱 session id）作为一行 NDJSON 喂给解析路径。
- 断言：`res.Usage.Present == true`；`InputTokens=2`、`OutputTokens=4`、
  `CacheCreationTokens=17039`、`CacheReadTokens=0`；`len(res.Usage.Models)==1`
  且 `Models[0].Model=="claude-opus-5-5"`、`Models[0].CostUSD` 与
  `total_cost_usd` 差 < 1e-9；`res.CostKnown == true`。
- 反向用例：样本里删掉 `usage` 与 `modelUsage` 两个键 → `Present==false` 且四个
  计数为 0（不 panic、不报错）；`CostKnown` 仍为 `true`（费用照报）。
- **多条 result（第三轮 blocker 3 / 第四轮 B1，与 f01 的接缝）**：`internal/integration/agent/agent_test.go::TestResultEventUsageTakesLastResult`
  （表驱动，经解析入口喂 NDJSON：`parseStream`，f01 先合入时是它改名后的 `parseStreamWith`）。**唯一的多 result `Usage` 测试，
  不论合入顺序都由本线写**；f01 PRD §3.2 第 5 条（R4）引用的就是这个名字和下面四例，f01 不另写、不改它的断言。
  输入：f01 fixture `replay-user.ndjson`（`/opt/lathe-wt/evidence/f01/fixtures/`，两条 result 在第 7、13 行）拷进
  `internal/integration/agent/testdata/replay-user.ndjson`（f01 先合入时它已在，直接复用）。

  | 用例 | 输入 | want（In / Out / CR / CC） |
  |---|---|---|
  | 两条 result、都有 `modelUsage`（fixture 原样） | 第 1 条 `modelUsage` In=2/Out=6/CR=0/CC=15746/$0.078858；第 2 条 In=6/Out=12/CR=12532/CC=18978/$0.0976604 | `Usage` = **6 / 12 / 12532 / 18978**、`Models[0].CostUSD=0.0976604`、`CostUSD=0.0976604`、`CostKnown=true`；**不是**两条之和 8 / 18 / 12532 / 34724 |
  | 两条 result、都只有 `usage`（两条都删掉 `modelUsage` 键） | 第 1 条 `usage` In=2/Out=6/CC=15746；第 2 条 In=4/Out=6/CR=12532/CC=3232 | `Usage` = **4 / 6 / 12532 / 3232**（回落也只取末条）、`Present=true`、`Models==nil`；**不是**逐项相加 6 / 12 / 12532 / 18978 |
  | 第 1 条有 `modelUsage`、第 2 条两个键都没有 | | `Present=false`、四项 0（末条整组胜出，不拿第 1 条补）；`CostKnown=true` |
  | 单条 result | 原 AC-1 样本 | 与上面 AC-1 主断言逐字段一致（回归） |

  失败信息带 got/want 四元组。**红**（两段，都落 `/opt/lathe-wt/evidence/f03/red-AC1-multi-result.txt`）：① 按「各条全部相加」
  写一版实现 → 第 1 例 `got In=8, want 6`；② 按「带 `modelUsage` 覆盖、回落 `usage` 逐项相加、`Present` 取或」（f01 R3 曾写、R4 已删的
  那一版）写一版实现 → 第 2 例 `got In=6 CC=18978, want 4 / 3232`、第 3 例 `got Present=true, want false`。再改成末条覆盖跑绿。
  第 ② 段是本轮新增的：它就是 f01 rebase 时被禁止的那种改法，这条测试必须能把它抓红，才算守得住 §3.7 的「不得为回落路径加相加逻辑」。
  （改动前的代码里 `Usage` 不存在、编译失败同样是红，但证明不了取值规则，所以这条要求的红是上面两段。）
- 红：改动前 `res.Usage` 字段不存在，编译失败即红。

### AC-1b codex 用量映射与「费用未知」（与 f06 的接缝，第二轮 blocker 3）

- 测试：`internal/integration/agent/usage_test.go::TestUsageFromCodex`（纯函数，表驱动）

  | 用例 | 输入（`turn.completed.usage` 序列） | want `Usage` |
  |---|---|---|
  | 单 turn（transcript.txt 第 6 行原文） | `{input_tokens:11, cached_input_tokens:0, cache_write_input_tokens:0, output_tokens:3, reasoning_output_tokens:0}` | `Present=true`, In=11, Out=3, CR=0, CC=0, `Models==nil` |
  | 含缓存 | `{input_tokens:100, cached_input_tokens:30, cache_write_input_tokens:20, output_tokens:5}` | In=**50**, CR=30, CC=20, Out=5 |
  | 缓存大于输入（防御） | `{input_tokens:10, cached_input_tokens:30}` | In=**0**（下限 0，不出负数） |
  | 两个 turn 相加 | 第 6 行 + 第 15 行 | In=72, Out=15 |
  | 没有 usage | 空序列 | `Present=false`，四项 0 |

- 落库：`internal/store/usage_test.go::TestRecordAgentRunCostUnknown`（真实库）——
  `Result{Success:true, CostUSD:0, CostKnown:false, Usage:{Present:true, InputTokens:11, OutputTokens:3}}`
  → 该行 `cost_usd IS NULL`、`tokens_known=true`、`input_tokens=11`、`status='success'`；
  对照行 `CostKnown:true, CostUSD:0` → `cost_usd = 0`（「确实 $0」与「未知」在库里可区分）。
  聚合侧：两行一起查，`totals.costKnown == 1`、`totals.tokensKnown == 2`、`totals.costUsd == 0`；
  只查第一行时 `totals.costUsd` 是 JSON `null`（`TestUsageStatsCostUnknownIsNull`）。
- 接缝守卫（**由后合入的一方补上并跑绿**，§3.7）：`internal/integration/agent/codex_test.go::TestCodexResultUsage`
  ——喂 transcript.txt probe 1 的原始 6 行给 codex 驱动解析路径，断言 `res.Usage.Present==true`、
  `InputTokens==11`、`OutputTokens==3`、`res.CostKnown==false`。f03 先合时这条由 f06 在其 PR 里加
  （f06 PRD AC-2 同步列入）；f06 先合时由本线在 rebase 后加。任一方合入后它都必须存在且绿，
  PR 描述里贴这条的输出。
- 红：改动前 `UsageFromCodex` / `CostKnown` 不存在，编译失败即红；`TestRecordAgentRunCostUnknown`
  在 `cost_usd NOT NULL DEFAULT 0`（第二版 DDL）下 `got=0, want=NULL`。

### AC-2 每次运行动一行（成功 / 失败 / 被杀 / 超时）

- 测试：`internal/runner/pipeline_test.go::TestPipelineRecordsAgentRunPerInvocation`
  （用假 driver 跑「分诊 + 实现 + fix-1」三次 `Run`；`Pipeline.Usage` 接真实 `*store.Store`）
- 断言：`agent_runs` 里该 task 恰好 3 行，`run_kind` 全为 `task`，`phase` 依次为
  `triage, implement, fix-1`；`bucket` 依次 `triage, implement, implement`；
  `status` 全为 `success`；每行 `user_id`/`repo_id` 等于该任务行的 `user_id`/`repo_id`；
  `started_at <= ended_at`。
- 测试：`TestPipelineRecordsRunWhenResultMissing`（表驱动，每例一个新任务，假 driver
  在**实现**那次 `Run` 返回给定的 `(res, err)`）：

  | 用例 | 假 driver 返回 | want `status` | want `cost_usd` / `num_turns` | want 任务终态 |
  |---|---|---|---|---|
  | 无 result | `nil, fmt.Errorf("%w（…）", agent.ErrNoResultEvent)` | `no_result` | NULL / 0 | `failed` |
  | 静默判死 | `nil, fmt.Errorf("%w（…）", agent.ErrIdle)` | `killed` | NULL / 0 | `failed` |
  | **总超时** | `nil, fmt.Errorf("agent: 执行超时（上限 1s），进程树已回收: %w", context.DeadlineExceeded)` | `killed` | NULL / 0 | `failed` |
  | 取消 | `nil, fmt.Errorf("agent: 执行被取消: %w", context.Canceled)` | `killed` | NULL / 0 | `failed` |
  | 判死但 result 已到 | `&Result{CostUSD:0.3, CostKnown:true, NumTurns:7, IsError:false, Success:true}, ErrIdle 包装` | `killed` | **0.3 / 7** | `failed` |
  | CLI 报失败 | `&Result{CostUSD:0.2, CostKnown:true, NumTurns:5, IsError:true, Subtype:"error_max_turns"}, nil` | `error` | 0.2 / 5，`subtype='error_max_turns'` | `failed` |

  （「判死但 result 已到」要求假 driver 同时返回非 nil 的 `res` 与 `err`；既有
  `pipeline_test.go:75` 的 `fakeAgent` 在 `errs[i]!=nil` 时返回 `nil, err`，本线给它加一个
  `both []bool` 开关，默认行为不变。）

  每例另断言：`res==nil` 的行 token 四列为 0、`tokens_known=false`、`models='[]'`、`subtype IS NULL`、
  `duration_ms >= 0`；**任务照常走到 `failed`**（账本不改变流水线语义）。
- 测试：`internal/integration/agent/agent_test.go::TestRunTimeoutKillsEntireProcessTree`
  追加一条断言：`errors.Is(err, context.DeadlineExceeded) == true`（总超时补 `%w` 的守卫）；
  既有的 `strings.Contains(err.Error(), "超时")` 原样保留。
- 测试：`internal/store/usage_test.go::TestRunStatus`（纯函数，表驱动覆盖 §3.1
  「状态判定」6 行，含 `err` 文本仅含「执行超时」而无 `%w` 的旧形态 → `killed`）。
- 红：改动前 `agent_runs` 表不存在，查询报 `42P01`；`TestRunTimeout…` 的新断言在
  `agent.go:337` 未加 `%w` 前失败。

### AC-2b PR review 轮的运行记成 `review` 桶

- 测试：`internal/runner/pipeline_test.go::TestPipelineRecordsReviewRoundRuns`
  （`ExecuteParams.Retry = &RetryPlan{Review:true, Entry:EntryImplement, ResumeSession:true}`，
  review 意见非空；假 driver：第 1 次 `Run`（`implement` resume）返回错误触发降级 →
  第 2 次（`implement-reentry`）成功 → 验证首轮失败 → 第 3 次（`fix-1`）成功）
- 断言：该任务 3 行，`phase` 依次 `implement, implement-reentry, fix-1`，
  `review_round` 全 `true`，`bucket` 全 `review`；第 1 行 `resume=true` 且 `status=error`。
- 红：改动前无表 → `42P01`。

### AC-2c PRD 规划 / 复核的每次尝试各一行

- 测试：`internal/prd/service_test.go::TestRunAgentRecordsUsagePerAttempt`
  （既有 `fakeAgent` + 新增 `fakeUsage`（实现 `agent.UsageRecorder`，只把 `RunRecord`
  追加进切片），`Service.Usage = fakeUsage`；纯内存，不连库）
- 表驱动：

  | 用例 | 假 agent 序列 | want 记录数 | want 逐条 `(Phase, Resume, status)` |
  |---|---|---|---|
  | 规划一次过 | `ok(validJSON)` | 1 | `(plan,false,success)` |
  | 首跑超时 → 收尾成功 | `errs[0]=context.DeadlineExceeded 包装`，`ok(validJSON)` | 2 | `(plan,false,killed)`、`(plan,true,success)` |
  | 两次修正后仍不合规 | `ok("garbage")` × (`maxRepairs`+1) | `maxRepairs`+1 | 首条 `resume=false`，其余 `true`，全 `success`（agent 本身成功，是输出不合规） |
  | CLI 报失败 | `&Result{IsError:true, CostUSD:0.4, Subtype:"error_max_turns"}` | 1 | `(plan,false,error)` 且 `Result.CostUSD==0.4`（记账在 `runOnce` 翻译错误**之前**） |
  | 对抗复核 | `Review` 路径，`ok(validReviewJSON)` | 1 | `(plan-review,false,success)` |

  每条另断言：`Kind=="prd"`、`PRDID==` 该 PRD 的 id、`TaskID==0`、`StartedAt` 非零且
  `!EndedAt.Before(StartedAt)`、同一用例内各条 `StartedAt` 严格递增。
- 落库侧：`internal/store/usage_test.go::TestRecordAgentRunPRDOwnership`（真实库）——
  造用户 U、仓库 R、PRD P，`RecordAgentRun(RunRecord{Kind:"prd", PRDID:P, Phase:"plan"})`，
  断言该行 `user_id=U`、`repo_id=R`、`bucket='prd_plan'`、`task_id IS NULL`；
  `PRDID` 指向不存在的 id → 返回 `ErrAgentRunTargetGone`，表中 0 行。
- 红：改动前 `Service.Usage` 字段不存在，编译失败即红（落红证据为编译错误输出）。

### AC-2d 预览推荐成功与失败都记一行

- 测试：`internal/preview/recommend_test.go::TestRecommendRecordsUsage`
  （既有 `fakeAgent` 扩一个可选 `res *agent.Result`；`m.Usage = fakeUsage`；纯内存）
- 表驱动：

  | 用例 | 假 agent | want `RunRecord` | want `RecommendOp.State` |
  |---|---|---|---|
  | 成功 | `&Result{Success:true, Text:validRec, CostUSD:0.05}` | 1 条，`Kind=preview`、`Phase=recommend`、`TaskID=<taskID>`、status `success` | `done` |
  | agent 报错 | `nil, errors.New("boom")` | 1 条，status `error`，`Result==nil` | `failed` |
  | CLI 报失败 | `&Result{IsError:true, Subtype:"error_during_execution", CostUSD:0.02}` | 1 条，status `error`、cost 0.02 | `failed` |
  | 没有候选 | 不调 agent（`Discover` 为空） | **0 条**（没起 agent 就没有花费） | `failed` |

- 落库侧：`internal/store/usage_test.go::TestRecordAgentRunPreviewOwnership` ——
  `RecordAgentRun(RunRecord{Kind:"preview", TaskID:T, Phase:"recommend"})`，断言
  `user_id/repo_id` 等于任务 T 的、`bucket='preview'`、`run_kind='preview'`。
- 红：改动前 `Manager.Usage` 字段不存在，编译失败即红。

### AC-3 失败不阻断任务

- 测试：`internal/runner/pipeline_test.go::TestUsageRecorderFailureDoesNotFailTask`
- 假 `UsageRecorder` 恒返回 `errors.New("boom")`；断言任务终态仍是 `pr_open`
  （或该用例的既定终态），且日志里出现 1 条 `agent 用量落库失败`（捕获
  `slog` handler）。
- 同形用例在 `internal/prd`（`TestRunAgentUsageFailureIsNonFatal`：规划轮照常成功）
  与 `internal/preview`（`TestRecommendUsageFailureIsNonFatal`：`State=done`）各一条。

### AC-4 幂等：同一次尝试重复写只有一行（含 NULL 列）

- 测试：`internal/store/usage_test.go::TestRecordAgentRunIsIdempotent`（表驱动，真实库）

  | 用例 | 记录 | 写两次后 want 行数 |
  |---|---|---|
  | 任务运行（`prd_id` 为 NULL） | `Kind=task, TaskID=T, Phase=implement, SessionID="s", StartedAt=t0` | 1 |
  | 任务运行、`session_id` 为 NULL | 同上但 `SessionID=""`（store 写 NULL） | 1 |
  | PRD 运行（`task_id` 为 NULL）、`session_id` 为 NULL | `Kind=prd, PRDID=P, Phase=plan, StartedAt=t0` | 1 |
  | 预览运行 | `Kind=preview, TaskID=T, Phase=recommend, SessionID="s2", StartedAt=t0` | 1 |
  | 不同 `started_at` | 第一条 + `StartedAt=t0+1µs` | **2**（是两次尝试） |
  | 不同 `resume` | 第一条 + `Resume=true` | **2** |

  断言 `got/want` 行数，且第二次调用返回 `nil`（`ON CONFLICT DO NOTHING`，不报错）。
- 钳位：`TestRecordAgentRunClampsEndedAt` —— `EndedAt = StartedAt − 1s`（墙钟回拨）→ 写入成功、
  `ended_at = started_at`、`duration_ms` 取 `res.DurationMS`（`res==nil` 时 0），不被 `agent_runs_time_check` 拒掉。
- 约束形态守卫：同文件 `TestAgentRunsAttemptUniqIsNullsNotDistinct` 查
  `SELECT indnullsnotdistinct FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
  WHERE c.relname='agent_runs_attempt_uniq'` → `true`（防止有人把 `NULLS NOT DISTINCT`
  「简化」掉而测试碰巧还绿）。
- 红：改动前无表 → `42P01`。另把第一版 DDL（无 `NULLS NOT DISTINCT`）在临时表上
  跑一遍前三个用例，`got=2, want=1`，作为「这条约束形态确实是必需的」的红证据
  （已落 `/opt/lathe-wt/evidence/f03/review-r1-nulls-distinct.txt`，见「评审答复」#1）。

### AC-5 阶段桶归并

- 测试：`internal/store/usage_test.go::TestUsageBucket`（表驱动，纯函数）
- 用例含：`(triage,false)→triage`、`(triage,true)→triage`、
  `(implement,false)→implement`、`(implement,true)→review`、
  `(implement-reentry,false)→implement`、`(implement-reentry,true)→review`、
  `(fix-1,false)→implement`、`(fix-1,true)→review`、`(fix-12,false)→implement`、
  `(fix-,false)→other`、`(fix-1a,false)→other`、`(verify,false)→verify`、
  `(push,false)→push`、`(plan,false)→prd_plan`、`(plan-review,false)→prd_review`、
  `(recommend,false)→preview`、`(review,false)→review`、
  `(whatever,false)→other`、`(whatever,true)→other`。

### AC-6 迁移 up/down 成对、回填与 Go 端同构

- 命令（本线测试库，跑完再 down/up 一轮。本机**没有** `psql`，库只能经
  `docker exec lathe-postgres-dev psql` 访问；下面的校验 SQL 与 `internal/store`
  测试里的断言是同一条，命令行版只作人工复核）：
  ```bash
  cd /opt/lathe-wt/f03-usage && source /opt/lathe-wt/env/f03.sh
  lathe_migrate up   && echo "UP-OK"
  lathe_migrate down && lathe_migrate down && lathe_migrate down   # 回滚 0040/0039/0038
  lathe_migrate up   && echo "ROUNDTRIP-OK"
  docker exec lathe-postgres-dev psql -U lathe -d lathe_f03 -tAc \
    "SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND indexname IN
       ('agent_runs_user_time','agent_runs_repo_time')"                          # 期望 2
  docker exec lathe-postgres-dev psql -U lathe -d lathe_f03 -tAc \
    "SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
      WHERE c.relname IN ('agent_runs_user_time','agent_runs_repo_time') AND i.indisvalid"  # 期望 2
  docker exec lathe-postgres-dev psql -U lathe -d lathe_f03 -tAc \
    "SELECT indnullsnotdistinct FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
      WHERE c.relname='agent_runs_attempt_uniq'"                                 # 期望 t
  ```
  期望输出：`UP-OK`、`ROUNDTRIP-OK`、`2`、`2`、`t`。输出落
  `/opt/lathe-wt/evidence/f03/ac6-migrate.txt`。（查询形态已在 lathe_f03 上用既有的
  `agent_events_result`/`agent_events_task_id` 两条索引试跑过，返回 `2`、`2`。）
- 机器验证（CI 可跑）：`internal/store/usage_test.go::TestAgentRunsIndexesValid` ——
  经 `testsupport` 的库执行上面第 1、2 条 SQL，断言 `got=2, want=2`。
- 测试：`internal/store/migrate_test.go` 既有对拍自动覆盖「每个 up 有 down、
  版本升序」（`migrate_test.go:58-80`），新增三个版本自动纳入。
- 回填同构：`internal/store/usage_test.go::TestBackfillBucketMatchesUsageBucket`
  —— 造 phase ∈ {`triage`,`implement`,`fix-1`,`fix-12`,`verify`,`push`,`review`} 的
  `agent_events` result 行与 {`plan`,`plan-review`} 的 `prd_events` result 行 → 执行
  0039 的 up SQL（从 `migrations` embed 读出同一份文本，不复制）→ 断言每行 `bucket`
  等于 `store.UsageBucket(phase, false)`、`review_round=false`、`backfill=true`、
  `ended_at = 事件 at`、`started_at = at - duration_ms`、`user_id/repo_id` 等于目标行的。
  **只对 `reviewRound=false` 对拍**：回填拿不到 review 信息（§3.1/0039），
  `implement-reentry` 不在用例里是因为现网 CHECK 不允许造出这种事件行（§2.1）。
  不经 down 直接再跑一次 0039 → 行数不变（回填行之间幂等）。
- **down/up 往返不重复计账**（第二轮 blocker 2）：`internal/store/usage_test.go::TestBackfillRoundTripNoDoubleCount`
  （真实库，表驱动；每例新造任务/PRD，断言按归属过滤，§5.5）。共同步骤：造历史事件 → 跑 0039 up →
  **模拟上线**：经 `RecordAgentRun` 写一条实时行，同时按真实时序给同一次运行写一条 `agent_events`
  result 事件（`at = 实时行 ended_at`，`costUsd` 同值）→ 跑 0039 down → 跑 0039 up → 断言。

  | 用例 | 造数 | want（按该目标过滤） |
  |---|---|---|
  | 任务：历史 1 条 + 上线后 1 次运行 | 历史 result（cost 0.10，`at` 早于实时行）；实时行 + 同次运行的 result 事件（cost 0.25） | `count(*)=2`（1 回填 + 1 实时）、`SUM(cost_usd)=0.35`；**不是** 3 行 / 0.60 |
  | 任务：上线后多次运行 | 实时行 ×2（`started_at` t1<t2）+ 各自的 result 事件 | `count(*)=2`，全 `backfill=false` |
  | PRD：同形 | `prd_events` 的 `plan` result + `RecordAgentRun(Kind:prd)` | `count(*)=2`、`SUM` 不翻倍 |
  | 另一目标只有历史 | 任务 B 只有历史 result，无实时行 | 往返后 B 仍恰好 1 行回填（截止条件只作用于有实时行的目标） |
  | 时钟偏差在运行时长内 | 实时行 `started_at = 事件 at − 30s`（运行 30s），节点时钟比库快 5s | 仍排除，不重复 |
  | **节点时钟快出的量 > 运行时长**（第三轮 blocker 1 的反例） | 先 `SELECT now()` 得库时刻 N。运行 30s、节点比库快 120s：`RecordAgentRun(StartedAt = N+90s, EndedAt = N+120s)`（`at` 不传，由 DEFAULT `now()` 给出 ≈ N）；同次运行的 result 事件显式 `at = N−0.5s`、cost 0.25；历史 result `at = N−2h`、cost 0.10 | `count(*)=2`、`SUM(cost_usd)=0.35`；**不是** 3 / 0.60 |
  | 节点时钟慢 1h | 同上但 `StartedAt = N−1h−30s, EndedAt = N−1h`；历史 result `at = N−30min`、cost 0.10 | `count(*)=2`、`SUM=0.35`（历史那条**补上**——第三版在这里会漏补，得 1 / 0.25） |
  | 容差边界 | 节点无偏差、运行 30s（库时钟开始 S = N−30s）；历史 result 两条，`at = S−4min` 与 `S−6min` | 前者不补、后者补（5 分钟容差的守卫；断言「补了哪些事件 id」，got/want 为 id 集合） |

  执行方式：0039 的 up/down SQL 都从 `migrations` embed 读同一份文本执行（不复制），与
  `lathe_migrate down/up` 走同一条路径；另在 AC-6 命令行段落里，先跑一次上面第一例的造数 SQL
  再做 `down && up`，把 `SELECT count(*), sum(cost_usd) FROM agent_runs WHERE task_id=…` 的前后
  输出落 `/opt/lathe-wt/evidence/f03/ac6-roundtrip-no-double.txt`。
- 红：把第二版的 0039（无 `NOT EXISTS` 截止条件）放进同一测试跑一遍，第一例 `got count=3,
  sum=0.60, want 2 / 0.35`，输出落 `/opt/lathe-wt/evidence/f03/red-AC6-roundtrip.txt`；再把**第三版**的
  0039（`r.started_at <= ae.at`）放进同一测试跑一遍，「节点时钟快出的量 > 运行时长」例 `got 3 / 0.60,
  want 2 / 0.35`、「节点时钟慢 1h」例 `got 1 / 0.25, want 2 / 0.35`，输出落
  `/opt/lathe-wt/evidence/f03/red-AC6-clock-skew.txt`。评审期已用临时表在 lathe_f03 上复现过这两个数
  （`review-r3-b1-clock-skew.txt`）。测试里第二、三版 SQL 以 testdata 常量存放，只用于红证据，不进 `migrations/`。

### AC-7 聚合按任务 / 仓库 / 档位 / 阶段 / 时间（含 `group=task`、`limit`、`byModel`、`bucket=`、任务明细）

- 测试：`internal/store/usage_test.go::TestUsageStatsAggregates`（造 2 个仓库、
  2 个用户、3 个任务：light 1 个、heavy 1 个、**未定档 1 个**（从未 `SetAgentRunTier`，
  模拟分诊即 blocked_spec），外加 1 个 PRD；行含 triage/implement/fix-1/plan/recommend；
  全部经 `RecordAgentRun` / `SetAgentRunTier` 写入，按真实时序：triage/implement 以
  `VerifyTier=""` 写 → `SetAgentRunTier` → fix-1 以 `VerifyTier=<档>` 写 → recommend）
- 断言（逐项，带 got/want）：
  - `group=phase` 的 `byBucket` 里 `implement` 的 `runs`、`costUsd` 等于造数之和；`byGroup` 的 key 是原始 phase
    （`fix-1` 单独一组、带 `bucket="implement"`），排序符合 §3.3 表；
  - `group=repo` 的两个 key 各自之和等于对应任务之行；
  - `group=tier`（§3.2.1：按 `agent_runs.verify_tier` 快照，**不 JOIN `tasks`**）：
    - heavy 任务的 **triage 行**（写入时 `VerifyTier=""` → NULL，随后被 `SetAgentRunTier` 回补）计入 `heavy`；
    - heavy 的 `runs` = 该任务 triage + implement + fix-1 + recommend 共 4、`costUsd` 为四行之和；
    - `light` 同理；
    - `none` = 未定档任务的行 + PRD 的 plan 行；
    - （recommend 写入前该任务行 `verify_tier` 已是该档——预览档位由 store 从任务行取，§3.2.1；
      造数里凡要落金额的 `Result` 都带 `CostKnown:true`）
    - 反向守卫：造数完成后 `UPDATE tasks SET verify_tier='light'` 改掉 heavy 任务的任务行，
      再查一次 `group=tier`，结果**不变**（证明查询不读 `tasks.verify_tier`）；
  - `tier=heavy` 过滤后 `totals.runs == 4`；
  - `group=day` 按 `started_at` 的 UTC 日切分，升序；
  - `from/to` 卡边界：把 `to` 设成最早一行的 `started_at - 1s`，`runs=0` 且
    `byBucket==[]`（不是 null）。
- 红：改动前无表 → `42P01`。

以下四条是第三轮 blocker 2 要求补齐的（①②④ 在 store 层，③ 在 httpapi 层，⑤ 在 AC-10）。每条都是
表驱动 + `t.Run`、失败信息带 got/want；红证据统一为「改动前 `UsageStats` / 路由不存在 → 编译失败或 404」
**再加**一段「行为红」：先写一个只差这一条语义的实现跑一次（下面每条写了是哪种错误实现），输出落
`/opt/lathe-wt/evidence/f03/red-AC7-<子项>.txt`，再改对跑绿。

**AC-7-① `group=task` 与 `limit`**：`internal/store/usage_test.go::TestUsageStatsGroupTaskLimit`
- 造数：同一用户 5 个任务 T1–T5，费用分别 0.50 / 0.30 / 0.30 / NULL（只有一条 `CostKnown:false` 的运行）/ 0.10，
  T2 2 次运行、T3 1 次（同价比 runs）；1 个 PRD P1 费用 0.20。

  | 用例 | 参数 | want `byGroup` 的 key 序列 | want `groupTotal` |
  |---|---|---|---|
  | 默认 limit | `group=task` | `task:T1, task:T2, task:T3, prd:P1, task:T5, task:T4` | 6 |
  | 截断 | `group=task&limit=3` | `task:T1, task:T2, task:T3` | 6 |
  | 上限值 | `group=task&limit=200` | 6 项，同默认 | 6 |
  | limit 对其它 group 无效 | `group=phase&limit=1` | 不截断（项数 = 造数里的 phase 数） | 不出现 |

  另断言：`totals` **不受** `limit` 影响（截断只作用于 `byGroup`）；`prd:P1` 一项的 `kind="prd"`、`prdId=P1`；
  `task:T4` 的 `costUsd == null`、`costKnown == 0`。「默认 limit = 20」：再造 21 个各一次运行的任务 → 不带 `limit` 时
  `len(byGroup)==20`、`groupTotal==21+6`。
  行为红：排序只按 `costUsd DESC`（不带 NULL 处理与次级键）→ 第 1 例 T4 排第一（PG 的 DESC 默认 NULLS FIRST）、T2/T3 顺序不定。
- httpapi 层：`internal/httpapi/usage_test.go::TestUsageGroupTaskHTTP`（第五轮 B1 按 §3.3「标签字段的来源」重写）。
  造数：新建用户 A、B（email 带随机后缀，§5.5），随机串 `r`。B 有一张内置工单 `LT-b`（标题 `B的工单-r`）。A 有：
  `Tl` = Linear 任务，`external_key='ENG-r'`；`Ti` = 内置任务，挂 A 自己的工单 `LT-a`（标题 `内置标题-r`）；
  `Tx` = Linear 任务，`external_key` **故意等于 B 的 `LT-b`**；`P1` = PRD，`document.oneLiner.body='一句话-r'`；
  `P2` = PRD，`document IS NULL`；`P3` = PRD，`oneLiner.body` = 81 个汉字。每个目标各一次运行。
  A 登录 `GET /api/usage?group=task&limit=200`（A 是新用户，响应只含 A 自己的目标，无需过滤）：

  | 项 | want |
  |---|---|
  | `task:Tl` | `externalKey="ENG-r"`、`trackerProvider="linear"`、`issueTitle=null` |
  | `task:Ti` | `externalKey="LT-a"`、`trackerProvider="internal"`、`issueTitle="内置标题-r"` |
  | `task:Tx` | `externalKey="LT-b"`、`trackerProvider="linear"`、`issueTitle=null`（**不是** `B的工单-r`） |
  | `prd:P1` | `oneLiner="一句话-r"` |
  | `prd:P2` | `oneLiner=null`（键存在） |
  | `prd:P3` | `utf8.RuneCountInString(oneLiner)==80` 且 `utf8.ValidString` |

  另断言：任何项都**没有** `title` 键。admin 登录 `GET /api/admin/usage?group=task&limit=200` 与 `?group=repo`：
  共享库上其他用户的目标可能把 A 的挤出前 200，所以**不找 A 的项**，而是对**返回的每一项**断言键集合 ⊆
  `{key, kind, taskId, prdId, repoId, runs, costUsd, durationMs, tokens, tokensKnown, costKnown}`，
  且响应体原文不含 `r`。
  红：① 标签照第五版写成 `title`、Linear/PRD 取空串 → `Tl`/`P1` 两行抓红；② JOIN 漏掉 `t.tracker_provider='internal'`
  或 `i.user_id=t.user_id` → `Tx` 的 `got issueTitle="B的工单-r"`（**跨属主泄露**）；③ 按 byte 截断 → `P3` 非法 UTF-8；
  ④ admin 复用属主侧响应结构 → 键集合越界。①② 两段输出落 `/opt/lathe-wt/evidence/f03/red-AC7-labels.txt`。

**AC-7-② `byModel`**：`internal/store/usage_test.go::TestUsageStatsByModel`

  | 用例 | 造数（同一用户） | want `byModel` |
  |---|---|---|
  | 单模型 | 1 行 `models=[opus:{in 10,out 2,$0.1}]` | `[{key:opus, runs:1, inputTokens:10, costUsd:0.1}]` |
  | 一行两个模型 | 1 行 `models=[opus:{in 10,$0.1}, haiku:{in 5,$0.01}]` | `opus` runs 1、`haiku` runs 1；`Σ runs = 2 > totals.runs = 1` |
  | 无明细的行归 `(unknown)` | codex 行（`Present:true` in 11、`CostKnown:false`）+ `res==nil` 的 killed 行 + 一条回填行（cost 0.05） | `(unknown)`：runs 3、inputTokens 11、`costUsd=0.05`、`costKnown=1`、`tokensKnown=1` |
  | 对账 | 上面三例的并集 | `Σ byModel.inputTokens == totals.inputTokens`（四个 token 字段逐一）——**精确相等** |
  | 截断 `(other)` | 1 行 20 个模型 | `byModel` 16 个 key、含 `(other)`；四个 token 字段之和仍 == `totals` |
  | 空 | 无行 | `byModel == []` |

  行为红：按 `jsonb_array_elements(models)` 直接 JOIN（不处理 `'[]'`）→「无明细」例 `got byModel=[]`、对账例
  （前三例并集：10 + 15 + 11）`got Σ inputTokens = 25, want 36`。

**AC-7-④ `bucket=` 过滤**：`internal/store/usage_test.go::TestUsageStatsBucketFilter`
- 造数：triage 1 行、implement 2 行、fix-1 1 行、review 轮 implement 1 行（`ReviewRound:true`）、plan 1 行、recommend 1 行。

  | 参数 | want `totals.runs` | want `byBucket` 的 key 集合 |
  |---|---|---|
  | `bucket=implement` | 3（implement×2 + fix-1；review 轮那行**不在**） | `{implement}` |
  | `bucket=review` | 1 | `{review}` |
  | `bucket=prd_plan` | 1 | `{prd_plan}` |
  | `bucket=verify` | 0（合法桶、无行） | `[]` |
  | `bucket=implement&group=phase` | 3 | `byGroup` key `{implement, fix-1}` |
  | `bucket=implement&tier=heavy` | 与两个过滤分别查的交集一致 | |

  `byModel`、`byDay` 同样只含过滤后的行（断言 `Σ byDay.runs == totals.runs`）。
  行为红：过滤写在 `phase` 列上（`phase = $bucket`）→ `bucket=implement` 例 `got 2, want 3`、`bucket=prd_plan` `got 0, want 1`。

**AC-7c `GET /api/tasks/{id}/usage` 正向内容（③）**：`internal/httpapi/usage_test.go::TestTaskUsageDetail`（真实库，表驱动）

  | 用例 | 造数（该用户的任务 T） | want |
  |---|---|---|
  | 混合 | 按时序写：triage（`VerifyTier=""`、`CostKnown:true` $0.1）→ implement（`Present:false`、`res==nil` killed）→ `SetAgentRunTier(T,"light")` → fix-1（`ReviewRound:true`、`CostKnown:true` $0.2）→ recommend（preview，`CostKnown:true` $0） | `len(runs)=4`；`phase` 依次 `triage, implement, fix-1, recommend`（按 `startedAt` 升序）；`runs[1].costUsd == null`、`tokensKnown == false`；`runs[0].verifyTier == "light"`（回补后）；`runs[2].reviewRound == true`、`bucket == "review"`；`runs[3].runKind == "preview"`；`summary.runs == 4`、`summary.costUsd == 0.3`、`summary.costKnown == 3` |
  | 从未定档 | triage 一行，之后不 `SetAgentRunTier` | `runs[0].verifyTier == null`（JSON null，不是 `""`） |
  | 同一 `started_at` 并列 | 两行 `StartedAt` 相同、`Phase` 不同 | 顺序按写入（`id`）稳定，跑 10 次结果相同 |
  | 不受时间范围影响 | 一行 `StartedAt` 在 60 天前；请求带 `?from=<昨天>` | 仍返回该行（参数被忽略） |
  | 空任务 | 无行 | `runs == []`、`summary.runs == 0`、`summary.costUsd == null` |
  | 他人任务 | — | 404（同 AC-8，这里再列一次以保证同一个测试文件覆盖正反两面） |

  对 `models`、`reviewRound`、`tokens` 逐行断言「不为 null」（用 `json.RawMessage` 比原文，不靠 Go 零值）。
  行为红：`ORDER BY started_at`（无次级键）+ `verifyTier` 直接序列化 `string` 字段 → 并列例偶发乱序、
  「从未定档」例 `got "", want null`。

### AC-7b Fresh 重跑改档后 `group=tier` 按运行时刻的档位分（第二轮 blocker 1；第四轮 B2 重写夹具）

- 测试：`internal/runner/pipeline_test.go::TestUsageTierFollowsAttemptAcrossFreshRetry`
  （真实库，`Pipeline.Usage = Pipeline.UsageTier = *store.Store`；照 `memory_test.go:156-220`
  `TestFailureMemoryInjectedOnRetry` 的真实重建路径：同一 `taskID`，`reclaimWorktree` →
  `Transition(queued)` → `Execute(Retry: &RetryPlan{Fresh:true, Entry:EntryTriage})`）
- 夹具（第四轮 B2 重写：第四版的「尝试 1 light 失败 / 尝试 2 heavy 一次通过」两段都跑不出来，见文末第四轮答复）：
  `pipelineFixture` + `goSourceRepo`（只有 `go.mod`、`main.go`，无 `package.json`），仓库 `VerifyTierOverride=""`、
  `VerifyInfra` 为空（heavy 不起隔离栈），`Stacks=nil`，`MaxFixAttempts=1`。改档方向是 **heavy → light**。
  - **尝试 1（heavy，修复耗尽 → `failed`）**：照搬 `pipeline_test.go::TestPipelineFixLoopExhausted` 的三次调用——
    分诊 `{"actionable":true,"kind":"fix",…}`；实现 `mutate` 写 `main_test.go`（`TestGreet` 断言 `greet()=="hello"`）
    与 `fix.go`（`greet()` 返回 `"wrong"`）；`fix-1` 的 `mutate` 改写 `fix.go`（换注释、仍返回 `"wrong"`，有改动
    但没修对）。路径：`ClassifyTier([fix.go main_test.go])` 命中「非展示层」→ heavy（`tier.go:66-93`）；
    `ResolveReproTests` 找到 `TestGreet`，红阶段在基线上编译失败 = 红成立，绿阶段 `got "wrong"` 失败 →
    `redStepFailure`/`redEnvError` 都为 nil（`pipeline.go:1346-1381`）→ 进修复回路 → `fix-1` 后仍失败、次数用尽 →
    `failed`，失败原因含「已尝试 1 轮修复」。`len(ag1.calls)==3`。
  - **重派**：照 `memory_test.go:156-220` 的真实重建路径——`reclaimWorktree` → `Transition(queued)` →
    换 `p.Agent = ag2` → `Execute(Retry: &RetryPlan{Fresh:true, Entry:EntryTriage})`，**同一个 `taskID`**。
  - **尝试 2（light，一次通过 → `pr_open`）**：两次调用——分诊同上；实现 `mutate` 只写 `web/App.vue`
    （一个 `<template><div/></template>`）。路径：Fresh 现场从基线重建，`ChangedFiles=[web/App.vue]` →
    `.vue` 在 `lightExts`、不含 heavy 路径信号 → light（`tier.go:46-50`、`:96-123`）；light 步骤只有
    `DetectLightProfile` 给的 `go build` / `go vet`（`verify.go:179-195`，无 `package.json` 不加前端步骤），
    `main.go` 未动，必过 → `report.Passed()`，不进修复回路；`fakeGitHub` 给 PR → `pr_open`。`len(ag2.calls)==2`。
  - 前置断言（防空转，同 `memory_test.go:224-231` 的写法）：尝试 1 之后 `State==failed`、`tasks.verify_tier=='heavy'`、
    `len(ag1.calls)==3`；尝试 2 之后 `State==pr_open`、`tasks.verify_tier=='light'`、`len(ag2.calls)==2`——证明两次尝试
    都走到了定档、改档真的发生在同一行上、且调用次数与下表的行数对得上。
- 断言（`GET` 走 `store.UsageStats(scope=该用户, group=tier)`，按该任务过滤）：

  | 桶 | want `runs` | 来自 |
  |---|---|---|
  | `heavy` | 3 | 尝试 1 的 triage（回补）、implement（回补）、fix-1（写入时就带 heavy） |
  | `light` | 2 | 尝试 2 的 triage（**写入时为 NULL，不是旧档 heavy**，定档后回补）、implement（回补） |
  | `none` | 0 | |

  另断言 `costUsd` 两桶分别等于各自运行之和；`agent_runs` 里尝试 1 的三行 `verify_tier`
  在尝试 2 结束后仍是 `'heavy'`（回补只补 NULL）；尝试 2 的分诊行在 `stageVerify` 回补**之前**（用假 driver 的
  回调在实现调用开始时读一次）`verify_tier IS NULL`。
- 红：在第二版方案 (a)（查询 JOIN `tasks.verify_tier`，即任务当前档位 light）下同一测试
  `got heavy=0, light=5, want 3 / 2`；作为第二道红，把写入点改成读 `tasks.verify_tier` 而非 `rc.tier`，尝试 2 的
  triage / implement 写入时就被记成上一次尝试残留的 heavy（回补只补 NULL，补不回来），
  `got heavy=5, light=0, want 3 / 2`。两段输出落
  `/opt/lathe-wt/evidence/f03/red-AC7b.txt`。
- 纯 store 层补一条（不起流水线，CI 快路径）：`internal/store/usage_test.go::TestSetAgentRunTierOnlyFillsNull`
  ——三行 `verify_tier` 为 `NULL/'light'/NULL` → `SetAgentRunTier(T,'heavy')` → 依次为
  `heavy/light/heavy`；对另一个任务的 NULL 行无影响；PRD 行不受影响。

### AC-8 属主隔离：非属主 404、参数与路径都守

- 测试：`internal/httpapi/usage_test.go::TestUsageIsolatedByOwner`
- A 有数据、B 登录：
  - `GET /api/usage` → 只含 A 之外 B 自己的（B 为空则 `totals.runs=0`）；
  - `GET /api/usage?repo=<A 的仓库 id>` → **404**；
  - `GET /api/tasks/{A 的任务 id}/usage` → **404**；
  - 未登录 → 401（`Auth.Require` 既有行为）。

### AC-9 管理员全局视图

- 测试：`internal/httpapi/usage_test.go::TestAdminUsageGlobal`、
  `TestAdminUsageForbiddenForMember`
- 造数（第五轮 B2，按 AGENTS.md §5.5 带随机量、按归属断言）：新建用户 A、B（email 带随机后缀），A 两次运行
  （0.30 与一条 `CostKnown:false`），B 一次运行（0.05），`started_at` 都落在默认 30 天窗口内。
- 管理员 `GET /api/admin/usage` → 200，**只按归属断言**：
  - `byUser` 里 `userId==A` 的一项：`runs=2`、`costUsd=0.30`、`costKnown=1`；`userId==B` 的一项：`runs=1`、`costUsd=0.05`、`costKnown=1`
    （`byUser` 不截断，§3.3，两项必在）；
  - `totals.runs >= 3`、`totals.costUsd >= 0.35`（浮点按 1e-9 容差）——**不断言相等**：admin 无属主过滤，共享测试库里
    被杀进程留下的残留行（`t.Cleanup` 不执行）、`TestBackfill*` 跑 0039 回填出的行都会合法地进 `totals`；
  - 再断言一条与残留无关的不变量：`totals.runs == Σ byUser.runs`（同一次查询、同一集合，这条在任何库状态下都精确成立）。
  - 行为红：`byUser` 按 `repo_id` 而非 `user_id` 分组 / 漏掉 A 的 NULL 费用行 → A 一项 `got runs=1`。
- 普通成员 → **403**（与 `/api/admin/*` 一致）；
- 响应体断言**不含**以下键（与 §3.3「admin 端点不带任何标签字段」逐字一致）：`email` / `externalKey` / `issueKey` /
  `trackerProvider` / `issueTitle` / `oneLiner` / `providerRepo` / `title`。

### AC-10 非法参数 400、空数据零值

- 测试：`internal/httpapi/usage_test.go::TestUsageRejectsBadParams`（表驱动：
  `from=abc`、`to<from`、`limit=0`、`limit=201`、`limit=abc`、`limit=0&group=phase`（任何 group 下都校验）、
  `group=nope`、`repo=abc`、`tier=medium`、`bucket=nope`、`bucket=fix-1`（phase 名不是桶名）各一条）
  断言状态码 400 且 `error` 非空、为中文；`/api/admin/usage` 跑同一张表，结果相同。
- 测试：`TestUsageIgnoresUnknownParams`（第三轮 blocker 2-⑤）：同一批造数，`GET /api/usage?group=phase` 与
  `GET /api/usage?group=phase&phase=fix-1` 的响应体**逐字节相同**（`phase=` 不是参数，§3.3），`?foo=bar` 同理。
  红：第三版的 URL 模板若被照着实现成「`phase` 过滤」，这条 `got` 只含 fix-1 的行。
- 测试：`TestUsageZeroValue`：空账号 → 200，`totals` 各计数为 0、`costUsd` 为 `null`（`costKnown=0`，§3.3），
  `byBucket/byGroup/byDay/byModel` 为 `[]`（不是 `null`）。

### AC-11 字符串截断按 rune

- 测试：`internal/store/usage_test.go::TestRecordTruncatesModelsByRune`
- 输入模型名 = 64 个汉字 + `"x"`（UTF-8 多字节），断言落库后 `length(model)=64`
  （SQL `length()` 按字符计），且库里值是合法 UTF-8（`pg_client_encoding` 无 22021）。
- 输入 20 个模型 → `jsonb_array_length(models)=16`，第 16 个元素 `model="(other)"`，且 16 个元素的
  四个 token 计数之和 == 行上四列（§3.1 截断规则；AC-7-② 的对账依赖它）。

### AC-12 页面可用（手测）

- 起服务（仅本线端口/库，按 PID 停，禁止 `pkill -f`）：
  ```bash
  cd /opt/lathe-wt/f03-usage && source /opt/lathe-wt/env/f03.sh
  LATHE_DB_HOST=127.0.0.1 LATHE_DB_PORT=55432 LATHE_DB_USER=lathe \
  LATHE_DB_PASSWORD=lathe LATHE_DB_NAME=lathe_f03 LATHE_DB_SSLMODE=disable \
  LATHE_HTTP_ADDR=127.0.0.1:8403 ./bin/lathe-f03 serve & echo $! > /tmp/f03.pid
  ```
- 截图落 `/opt/lathe-wt/evidence/f03/ui/`：`/usage` 默认视图、四筛选各一张、
  `group=task` 一张、任务详情「用量」卡一张、管理员全局一张。
- 断言（截图 + DOM）：切换 `group` 后 URL 带 query 且刷新不丢；空时间范围显示
  「该范围内没有运行记录」而不是 `$0.0000`。

### AC-13 全量门禁

- `make lint && make test` 全绿（AGENTS.md §1.4）。
- `make test-race` 全绿（新增代码涉及聚合写入，按 §3 表格要求跑一次）。
- `cd web && node_modules/.bin/vite build` 通过（`make ui` 的构建段；
  提交后 `make ui && make build` 同步 `internal/webui/dist`）。
- 提交前按 `make lint` 的已知缺口补扫新文件（记忆：`make lint` 只扫 `git ls-files`，
  `gofmt` 不使 make 失败）：`gofmt -l $(git ls-files -o --exclude-standard '*.go')` 输出为空。

---

## 6. 风险与代价

| 风险 | 影响 | 缓解 |
|---|---|---|
| `UNIQUE NULLS NOT DISTINCT` 要求 PostgreSQL ≥ 15 | 外部库低于 15 的部署 0038 直接失败 | 失败是响亮的（迁移报语法错，不会半建）；dev-infra 与 CI 都是 16；README 外部 Postgres 一行与 docs 写明最低版本 |
| tier 按**运行所属尝试**快照 + 定档回补（§3.2.1；第二版的「按任务当前档位 JOIN」因 Fresh 同行改档被推翻） | ① 改过档的任务同时出现在两个档位桶，「一张单子一档」的直觉不成立；② 定档前的运行是事后回补的，回补失败时短暂落 `none`；③ 回填行的档位只是回填时刻的近似 | ① 这是改档的真实情况，任务详情「用量」卡逐行显示 `verifyTier`；② 回补幂等，下次定档补上，失败只告警不影响流水线；③ 回填行 `backfill=true`，页面提示。AC-7b 用真实 Fresh 路径守 |
| 0039 回填与实时账本重叠（down/up 往返） | 无截止条件时，往返会把已有实时行的运行再回填一次，费用翻倍；第三版的截止条件拿节点时钟（`started_at`）比库时钟（事件 `at`），节点偏快超过一次运行时长时同样翻倍 | 按目标截止、**只比库时钟**：`NOT EXISTS` 该目标满足 `事件 at >= r.at − (r.ended_at − r.started_at) − 5min` 的实时行（§3.1/0039）。不等式里不含节点与库的绝对偏差，只含「运行期间墙钟步进量」与秒级写入延迟，都被 5 分钟容差盖住；代价是首条实时运行前 5 分钟内旧节点的事件不回填（少补，方向安全）。AC-6 `TestBackfillRoundTripNoDoubleCount` 含「节点快 120s、运行 30s」与「节点慢 1h」两例，且先在第三版 SQL 上跑红 |
| codex 运行的费用 | 被记成 $0 会把「未知」伪装成「免费」，tier/仓库维度的单价全被拉低 | `cost_usd` 可为 NULL + `Result.CostKnown`（零值 = 未知）+ `costKnown` 计数 + 前端「未知」渲染；AC-1b |
| f03 / f06 的接缝漏接（codex `Result.Usage` 没人填） | codex 运行 token 恒 0 | 规则写死「后合入方接」、两份 PRD 同文；`TestCodexResultUsage` 由后合入方加并贴进 PR；即便漏接，`Present=false` → `tokens_known=false`，页面显示「无 token 数据」而不是 0 |
| 回填丢 `review_round`、没有 `implement-reentry` | 历史 review 花费混在 `implement` 桶 | 回填行有 `backfill=true`，`/usage` 页在含回填行时提示「历史数据不区分 review 轮」 |
| **双源**：`agent_events.payload` 与 `agent_runs` 都记钱，可能对不上 | 两个面板给出不同数字，最伤信任 | `agent_runs` 是账本、`agent_events` 是人读时间线，职责写进迁移头注释与 docs；`/usage` 页面注明与 `CostPanel` 的口径差异；不合并两者（§4） |
| 回填行没有 token | 「平均 token」被历史数据拉低 | `models='[]'` + `tokensKnown` 计数，前端显式标注「N 次无 token 数据」，不摊进平均 |
| CLI 升级改 `modelUsage` 形状 | token 静默变 0 | `Present` 标志 + 回落 `usage` + 「字段缺失不报错」的既有立场（`agent.go:74-77`）；AC-1 用真实样本守 |
| 每次 Run 多一次同步 INSERT | 流水线多一个持久化点 | 不在事件热路径（每 Run 一次），单行无事务，失败只告警（AC-3） |
| 与 f06 在 `agent.Result` 上冲突 | 合并冲突 | 只加 2 个字段（`Usage`、`CostKnown`）+ claude 一处赋值；codex 一处赋值由后合入方补（§3.7）；语义写进 docs |
| 与 f01 的多 result 语义不一致（一方相加、一方覆盖） | 插话开出第二个 turn 后 token 翻倍或漏记 | 规则只有一条：`Usage`/`CostKnown` 与 `CostUSD` 同组、末条覆盖、不跨 result 相加（§3.1 的表与 f01 §3.2 第 5 条逐字相同）；测试只有 `TestResultEventUsageTakesLastResult` 一条、只由本线写，f01 先合后合都只需保持它绿（现网循环已能处理多 result） |
| 管理员全局视图引入跨用户读取 | 隔离纪律出现例外 | 显式端点 + `RequireAdminFunc` + 只给聚合与 `user_id`；同步修订 `api.go:60-63` 注释（§3.6.3） |
| `numeric(14,6)` → `float64` | 大额累计的精度 | 求和与聚合在 SQL 里做（`::float8` 只出现在出口），AC-7 用 `1e-9` 容差断言；金额上限 10^8 美元远超实际 |
| 表持续增长 | 长期容量 | 与 `agent_events` 同级（每 Run 一行 vs 每 Run 上千行），量级反而更小；保留期策略留待实测后另立需求 |
| 一次任务重试会多记行 | 「任务花了多少」比一次成功执行多 | 语义上正确（重试确实花了钱）；前端按 `attempt` 不明示，但 `startedAt` 可区分，docs 里写明 |

---

## 7. 文档改动（同一个 PR）

1. **新增 `docs/features/usage-stats.md`**：本文件的定稿版（去掉状态行与
   「待评审」，补一节「实现记录」，写法同 `docs/08-debt-cleanup.md:774` 的
   「实现完成」条目：AC 实际怎么验的、偏离了哪条 AC、为什么）。`docs/features/`
   是新建目录，与本文件同 PR 引入。
2. **`README.md`**：
   - 「镜像部署用 `LATHE_DB_*` 指向外部 Postgres」一行（`README.md:119-126`）补「需 PostgreSQL ≥ 15（0038 用 `UNIQUE NULLS NOT DISTINCT`）」；
   - 变量表**无新增行**（§3.5），只在「用量统计」相关段落写一句「本功能无新增
     环境变量」——如果用户可见文档里需要一个入口，就在「管理界面」一节加
     `/usage` 的一句话说明；
3. **`CHANGELOG.md` 的 `[Unreleased]` → `Added`**：
   - 「用量统计：按任务、仓库、验证档位、阶段与时间范围聚合 token、费用、时长；
     新增 `/usage` 页面、`GET /api/usage`、`GET /api/tasks/{id}/usage`、
     管理员 `GET /api/admin/usage`（migration 0038–0040）。任务详情页的
     成本/耗时徽章改为标注「最近一轮」。」
4. **`internal/httpapi/api.go:60-63`** 的注释同步修订（代码即文档，§3.6.3）。
5. **`docs/05-roadmap.md` §3.2**：把「token 就是钱」那一节补一句「已交付：
   `agent_runs` + `/usage`」，与 §3.2 现有条目风格一致。
6. 提交规范：`feat(usage): ...`，PR 目标 `dev`，附 AC-1…AC-13（含 AC-2b/2c/2d）的验证证据与
   `/opt/lathe-wt/evidence/f03/` 下的红绿输出路径。

---

## 附：本地执行环境（本线专用）

- worktree：`/opt/lathe-wt/f03-usage`（分支 `feat/usage-stats`）。任务简报写的
  `/opt/lathe-wt/f03-usage-stats` 在本机不存在，实际目录以上面这个为准。
- 每个 Bash 调用：`cd /opt/lathe-wt/f03-usage && source /opt/lathe-wt/env/f03.sh`
- 迁移：`lathe_migrate up|down`（函数已带 `LATHE_DB_*` 到 `lathe_f03`）
- 手测端口 8403；证据目录 `/opt/lathe-wt/evidence/f03/`（已放入
  `claude-result-event-sample.json`）

---

## 评审答复（第一轮，8 条 blocker）

全部采纳，无保留异议。每条先给复核证据，再给 PRD 落点。证据库：lathe_f03，
`PostgreSQL 16.11 on x86_64-pc-linux-musl`，经 `docker exec lathe-postgres-dev psql`（本机无 `psql`）。

| # | 结论 | 复核证据 | PRD 落点 |
|---|---|---|---|
| 1 幂等失效 | **采纳**，改 `UNIQUE NULLS NOT DISTINCT`，并把 `run_kind` 放进键 | 临时表复现：默认 `UNIQUE(...)` 写两条相同的任务行 → `count=2`；`NULLS NOT DISTINCT` → 任务行 `1`、`prd_id` 非空且 `session_id` NULL 的 PRD 行 `1`。两条部分唯一索引 + `COALESCE(session_id,'')` 方案也测过（任务 3 次尝试 → 3 行、PRD 重复 → 合并），可行，但选了前者，理由见 §3.1 | §3.1 DDL + 约束说明；AC-4 改成 6 例表驱动（含 `session_id` NULL、PRD、preview）+ `indnullsnotdistinct` 守卫；§6 加 PG≥15 风险；§7 README 注明最低版本 |
| 2 tier 无数据源 | **采纳 (a)**：查询时 JOIN `tasks.verify_tier`，即任务当前档位 | `pipeline.go:936-937` 是唯一写点；`tasks_verify_tier_check` 允许 NULL；续跑沿用首次定档（`:916-918`）；`machine.go:362` 是 `COALESCE($7, verify_tier)`，不会把已定档改回 NULL | 新增 §3.2.1（取值表、4 条理由、代价）；§3.3 `tier` 参数说明；AC-7 改成「heavy 任务的 triage 行（写入时 tier 为 NULL）计入 heavy」，并加未定档任务 → `none` |
| 3 PRD/预览无 AC | **采纳** | `internal/prd/service_test.go:209` 与 `internal/preview/recommend_test.go:14` 已有 `fakeAgent`，加一个 `fakeUsage` 即可，纯内存 | 新增 AC-2c（PRD：一次过 / 超时收尾 / 修正耗尽 / CLI 报失败 / 复核，逐条断言 phase/resume/status/kind/prdID）、AC-2d（预览：成功 / agent 报错 / CLI 报失败 / 无候选 0 行）；两边各配一条真实库的归属断言；红 = 字段不存在编译失败 |
| 4 归属列填不出来 | **采纳**；方案是**调用方只传目标 id，store 用 `INSERT … SELECT FROM tasks/prds` 取 user_id/repo_id** | `Manager` 字段确认无 store（`manager.go:262-291`）；`RepoInfo` 无 repo id（`service.go:70-74`）；`prds.user_id/repo_id` 与 `tasks.user_id/repo_id` 均 NOT NULL；import 图：`integration/agent` 是 store/runner/prd/preview 共同依赖的叶子包（只依赖 `capbuf`） | §3.1「归属怎么来」：SQL 形态与理由；接口 `agent.UsageRecorder` / `agent.RunRecord` 放 `internal/integration/agent/usage.go`，零新增依赖边；注入表：`Pipeline.Usage`、`prd.Service.Usage`、`preview.Manager.Usage`（导出字段，**不改** `SetRecommender` 签名），装配在 `main.go:425-434`、`:583`、`:211` 旁；`prd.Store` 接口不变 |
| 5 UsageBucket 不全 | **采纳** | review 轮的 `implement-reentry`：`pipeline.go:713` 降级分支、`implementPrompt:741-748` 无会话分支；`fix-N` 在 review 轮照常进修复回路（`:993-1011` 不看 `plan.Review`）；`agent_events` 无 review 信息 | §3.2 改成按序规则表：`reviewRound=true` 时 implement / implement-reentry / fix-N 全进 `review`；顺带收编预留的 `review` phase（CHECK 允许，今天无写入方）；AC-5 加 `(implement-reentry,true)`、`(fix-1,true)`、`(triage,true)`、`(review,false)`、畸形 `fix-` 等；§3.1/0039 与 AC-6 写明同构**只对 `review_round=false` 成立** |
| 6 AC-6 命令跑不了 | **采纳** | `which psql` → 无；`SELECT … FROM pg_indexes WHERE relname IN (…)` 实测 `ERROR: column "relname" does not exist`；改写后的 `indexname` 与 `pg_index.indisvalid` 两条查询用现有两条 `agent_events_*` 索引试跑，均返回 `2` | AC-6 改用 `docker exec lathe-postgres-dev psql -tAc`，`indexname` + `schemaname`，加 `indisvalid` 与 `indnullsnotdistinct` 两条；另加 `TestAgentRunsIndexesValid` 作 CI 可跑版本 |
| 7 事实错误 | **采纳**，原文「`implement-reentry` 此前被错归 `other`」是错的 | `\d agent_events` 的 CHECK 原文已贴进 §2.1；`INSERT … 'implement-reentry','result'` → `violates check constraint "agent_events_phase_check"`；`InsertAgentEvents` 是 `CopyFrom`，整批失败 | §2.1 新增一行事实 + §2.2 第 3 条补述；§3.2 说明改为「本表是该阶段唯一的账」；§4 范围外新增一行：bug 本身归 f04（其 PRD §2.2-2 已认领），本线不改 `agent_events` 约束，也不做二次回填 |
| 8 状态映射与返回值 | **采纳** | `agent.go:330-339`：ErrIdle / 总超时 / 取消三支都是 `return result, …`；总超时是不带 `%w` 的裸 `fmt.Errorf`（`:337`）；`prd.runOnce` 把 `IsError` 翻成 err、丢掉 res（`service.go:612-614`） | §3.1 新增「状态判定与花费口径」：6 行按序判定表（timeout/cancel → `killed`）；`res != nil` 就记钱与 token，与 err 无关；`res == nil` 时 `duration_ms` 取墙钟；PRD 记账放进 `runOnce` 内部；总超时补 `%w context.DeadlineExceeded`（文案不变，既有测试照过）。回填行 `ended_at = at`、`started_at = at − duration_ms`（0039 字段表）。AC-2 加总超时、取消、「判死但 result 已到」、CLI 报失败四例 + `TestRunStatus` |

**在评审意见之外顺带做的调整**（都是修上面几条时暴露出来的）：

- `agent_runs_target_check` 收紧为与 `run_kind` 绑定（task/preview ↔ `task_id`，prd ↔ `prd_id`），否则 `run_kind='prd'` 却挂着 `task_id` 的行合法；
- 加 `agent_runs_time_check (ended_at >= started_at)`；
- 本线对驱动层的改动从「一个字段」扩成「一个字段 + 总超时补 `%w` + 一个新文件 `usage.go`」，§3.7 的 f06 边界已同步。补 `%w` 只多包一层、不改错误文案；f06 若重写这段，只要保持 `errors.Is(err, context.DeadlineExceeded)` 即可。

---

## 评审答复（第二轮，3 条 blocker）

三条**全部采纳**。第 1 条推翻了第二版 §3.2.1 的方案 (a)，不是补丁式修正。代码事实都在本线
worktree（`feat/usage-stats`，HEAD `cff5332` = origin/dev）里重新核对过。

| # | 结论 | 复核证据 | PRD 落点 |
|---|---|---|---|
| 1 Fresh 同行改档，(a) 的理由 4 是事实错误 | **采纳，改为 (b)：写入时按尝试快照 + 定档回补只补 NULL** | `cmd/lathe/queue.go:472-489`：Fresh 先 `Discard` 现场，再以同一个 `tk.ID` 调 `Execute`；`pipeline.go:446-457`：`Fresh` 时 `entry` 保持 `EntryTriage`，`:455-457` 直接 `return`，`:485-487` 的 `rc.tier = tk.VerifyTier` 不执行；`stageVerify` `:914-926` 因 `rc.tier==""` 落到 `ClassifyTier`；`machine.go:362` `COALESCE($7, verify_tier)` 覆盖旧值。附带发现：Fresh 重新定档**之前**，任务行上仍是旧档——所以快照不能读 `tasks.verify_tier`，必须用 `rc.tier`。`rc.tier` 今天只有 `:486` 一个写点、`:914` 一个读点，在定档后补 `rc.tier = tier` 不改变既有行为 | §2.1 新增「Fresh 在同一行上改档」一行；§3.1 DDL 加 `verify_tier` 列与两条 CHECK、`RunRecord.VerifyTier`、`agent.TierBackfiller`；§3.2.1 重写（写入规则表、定档回补 SQL、代价 4 条）；写入点表加 `stageVerify` 回补；§3.3 `tier` 参数与任务明细的 `verifyTier`；§6 改写风险行；**新增 AC-7b**（真实 Fresh 路径、同一行 light→heavy，want light=3/heavy=2，两道红：方案 (a) 与「读任务行」各一段）与 `TestSetAgentRunTierOnlyFillsNull`；AC-7 改成经 `RecordAgentRun/SetAgentRunTier` 造数并加「改任务行档位结果不变」反向守卫 |
| 2 0039 down/up 往返重复计账 | **采纳，按目标截止**：`NOT EXISTS` 同目标、`NOT backfill`、`started_at <= 事件 at` 的实时行 | 同一次运行：实时行 `started_at` = Run 之前的调用方时钟；它的 result 事件 `at` = 运行结束后的库 `now()`，二者相差至少一个运行时长；两行 `session_id`（NULL vs 实际值）与 `started_at` 都不同，唯一约束确实拦不住（第二版 AC-6 的「行数不变」只是测试库里没有实时行） | §3.1/0039 加截止 SQL、为什么按目标不按全局、时钟偏差的最坏情况（少补不多算）、往返不变量改成「每次运行至多一行」；down 说明补一句；AC-6 新增 `TestBackfillRoundTripNoDoubleCount`（5 例，含 PRD、无实时行的目标、时钟偏差）+ 命令行往返证据 + 第二版 0039 的红证据；§6 新增风险行 |
| 3 codex token/费用无人负责 | **采纳**：映射规则与函数（`agent.UsageFromCodex`）、`Result.Usage`/`Result.CostKnown` 字段归 f03；codex 驱动里**那一行赋值**由后合入 dev 的一方写，并加 `TestCodexResultUsage`。费用未知用 `cost_usd = NULL` + `costKnown` 计数表示，零值即「未知」 | f06 PRD 旧版 §3.2 / §4 / f03 行 / §6（本次已改）；codex `turn.completed.usage` 真实形状取自 `/opt/lathe-wt/evidence/f06/probes/transcript.txt` 第 6/15/22 行（`input_tokens`、`cached_input_tokens`、`cache_write_input_tokens`、`output_tokens`、`reasoning_output_tokens`），没有模型名、没有费用 | §3.1 DDL：`cost_usd` 改可空、加 `tokens_known`；`Result.CostKnown` 与为什么不改 `CostUSD` 类型；状态/花费口径改成 `res==nil` 或 `!CostKnown` → NULL；§3.3 `costKnown`、`costUsd` 为 `null` 的规则；§3.4 「未知」与「$0」分开渲染；§3.7 重写 f06 边界（映射表、谁写哪一行、为什么这样分）；§4 两行；0039 对 codex 事件 `cost_usd = NULL`；**新增 AC-1b**；AC-2 期望改成 NULL；**f06 PRD 同步**：§3.2 末条、§3.3 Result 映射、§4 不做第 4 条、§4 f03 行、AC-2 一条、§6 一行，并在其 §8.3 记录 |

**有所保留、说明理由的点**：

- 第 3 条评审写「前端 tokensKnown 会把 codex 算成有 token 数据（`Present` 没人置位）」。按本版规则，codex
  运行接上映射后 `Present=true`、`tokens_known=true`，它**应当**算有 token 数据——它确实报了 token；
  没报的是费用，那由新的 `costKnown` 计数单独表达。没接上（漏接）时 `Present=false`，页面显示「无 token
  数据」而不是 0。所以 `tokensKnown` 的语义保留，只是改为由独立的 `tokens_known` 列计数（codex 没有模型名，
  不能再用 `models<>'[]'` 推断）。
- 第 2 条评审给了两种截止方式。没选「全局：`at < 首条非回填行的 started_at`」：多节点滚动升级时，新节点
  先写实时行，旧节点同期跑完的运行（只有事件、没有实时行）会被整段丢掉；按目标截止只影响同一目标上的这种
  运行。两种都不会重复计账，差别只在少补的范围。
- 第 1 条评审给了两条路：「重新说明代价」或「改写入时快照」。选后者：前者需要承认「一次 Fresh 就让一个
  任务的全部历史运行换档」，而 Fresh 是重试路径的常态（不是边角），这个维度就不可信了。代价是放弃第二版
  理由 1 的「一张单子一档」，§3.2.1 写明了。

---

## 评审答复（第三轮，3 条 blocker）

三条**全部采纳**。第 1 条评审的推导经实测成立，第三版 §3.1/§6 的「最坏少补、不会多算」是错的，已改正；
另外发现第三版在反方向（节点时钟偏慢）还会**漏补**，新条件把这两种都修好了。代码事实在本线 worktree
（`feat/usage-stats`，HEAD `cff5332`）上重新核对过；证据库是 lathe_f03（PG 16.11），经 `docker exec lathe-postgres-dev psql` 访问。

| # | 结论 | 复核证据 | PRD 落点 |
|---|---|---|---|
| 1 时钟偏差下 0039 重复计账 | **采纳**。截止条件改为**只比库时钟**：`ae.at >= r.at − (r.ended_at − r.started_at) − 5min`（`r.at` 是实时行 INSERT 时的库 `now()`，store 永不写这一列）。没选评审给的 `r.started_at <= ae.at + 容差`：它仍拿节点时钟比库时钟，容差必须大于**最大的节点偏差**才安全，而那个上界我们给不出；新条件里只剩「运行期间墙钟步进量」与秒级写入延迟，与节点偏差无关 | 临时表复现（`/opt/lathe-wt/evidence/f03/review-r3-b1-clock-skew.txt`）：节点快 120s、运行 30s → 第三版条件补了 R1 自己的事件，`total_rows=3, total_cost=0.60`（want 2 / 0.35）；节点慢 1h → 第三版**漏补**早 30min 的真实历史（该任务回填 0 条，want 1）；节点快 5s（偏差在运行时长内）两版都对。新条件三例全部 `2 / 0.35` | §3.1/0039 截止 SQL 重写，并写明第三版的错误、新条件的证明（不等式里不含节点偏差）、5 分钟容差的取值理由与代价（少补，方向安全）；§3.1 DDL 给 `at` 加注释「库时钟，store 不写」；顺带加 `ended_at` 钳位（墙钟回拨时不让 `time_check` 拒掉真实花费）+ `TestRecordAgentRunClampsEndedAt`；§6 风险行改正；AC-6 表加「节点快 120s」「节点慢 1h」「容差边界 ±1min」三例，红证据要求在第三版 SQL 上跑出 `3 / 0.60` 与 `1 / 0.25` |
| 2 API 行为缺 AC | **采纳**，五项逐一补齐 | ① 第三版只有 `limit` 的取值范围，没有排序与截断规则；② `byModel` 没定义 `models='[]'` 的行，且 `models` 截到 16 项会丢量，两处都会让 `Σ byModel ≠ totals`；③ 任务明细只有 404 反面；④ `bucket=` 无测试；⑤ URL 模板有 `phase=`、参数表没有 | §3.3：参数表补 `bucket` 合法值与 400、`limit` 在任何 group 下都校验；**删掉 `phase=`**，写明「phase 是分组键不是筛选维度、按阶段筛选用 `bucket=`、未知参数忽略（与既有读接口一致）」；新增 `byGroup` 键/排序/截断表（全序，`group=task` 含 PRD 目标与 `groupTotal`）；`byModel` 定义（`'[]'` 行归保留键 `(unknown)`，token 精确对账，`runs` 与 `costUsd` 何时不等写明）；§3.1 `models` 超 16 项合并成 `(other)` 不丢量；任务明细逐项语义（全量不受时间范围、全序、null 的含义）。AC：新增 AC-7-①（`group=task`/`limit` 四例 + 默认 20）、AC-7-②（`byModel` 六例含对账）、AC-7-④（`bucket=` 六例）、AC-7c（任务明细六例）；AC-10 补 `limit`/`bucket` 的 400 与 `TestUsageIgnoresUnknownParams`（`phase=` 不改变响应）；AC-11 补 `(other)` 断言。每条都给了「只差这一条语义的错误实现」作为行为红 |
| 3 与 f01 的多 result 接缝 | **采纳** | f01 PRD :405-411、:730 的约定；f01 fixture `replay-user.ndjson` 两条 result：`modelUsage` 第二条 In 6 = 2+4、CC 18978 = 15746+3232，`total_cost_usd` 0.078858 → 0.0976604（累计）；`usage` 2 → 4（单 turn）。用本线 worktree 的现网 `parseStream` 喂这两条（临时探针，已删除）：2 个 result 事件、`CostUSD=0.0976604`，即现网就是末条覆盖（`/opt/lathe-wt/evidence/f03/review-r3-b3-two-results-probe.txt`） | §2.2 第 1 条改写「按 Run 求和」的含义（只指一条 result 内各模型相加、聚合时各行相加，不指跨 result 相加）；§3.1 新增「多条 result 时 `Usage` 怎么取」（末条覆盖、回落 `usage` 也取末条、末条整组胜出不混用，回落路径少记前几 turn 的代价与理由）；§3.7 新增 f01 小节（口径、先合/后合各自做什么、账本仍是一 Run 一行、PR 描述的那句话）；§4 新增 f01 行；§6 新增风险行；AC-1 新增 `TestResultEventUsageTakesLastResult`（四例，第一例用 f01 fixture 原文，want 6/12/12532/18978，红 = 「相加版」实现 got 8/…/34724） |

**有所保留、说明理由的点**：

- 第 1 条评审建议的两种改法（`started_at + 容差`、`ended_at + 容差`）都没采用，理由见上表：两种都把节点时钟和库时钟放在同一个不等式里，容差要盖住的是「节点与库的最大偏差」，这个量没有上界，也不归 Lathe 管。新条件里的节点时钟只以**差值**（`ended_at − started_at`）出现，偏差在差值里抵消了。代价是多信一列 `at`，所以 §3.1 写死了「store 的 INSERT 不写 `at`」。
- 第 2 条⑤评审给了两条路：「删掉」或「补定义 + 400」。选了删掉，并把 `phase=` 当未知参数**忽略**，没有回 400：一是既有读接口（`api.go:93`、`:156`）对未知参数都是忽略，单给这一个端点加「未知参数 400」会与既有习惯不一致；二是未知参数 400 会让滚动升级期间新前端带新参数打到旧后端时直接报错。「非法参数 400」只针对**已定义参数的非法值**，这条规则不变。
- 第 3 条评审要求「回落 `usage` 时同样取末条」。照做了，但要指出代价：`usage` 是单 turn 量，多 result 且末条没有 `modelUsage` 时会少记前几个 turn。这只发生在缺 `modelUsage` 的旧 CLI 形状上，方向是少记。没有为它在同一循环里另开一套「单 turn 字段相加」的语义，因为 f01 已经把相加的字段限定为 `NumTurns`/`DurationMS` 两个。

**需要邻居同步的措辞**（我只改本文件）：

- **f01**：§3.2 第 5 条那句「f03 PRD 第 78-79 行『一次 Run 一个进程、按 Run 求和安全』」指的行号已变（现在在 §2.2 第 1 条，并已改成不会被误读的说法），建议改为引用「f03 §2.2 第 1 条 / §3.7 f01 小节」；再补半句「末条缺 `modelUsage` 时回落末条的 `usage`，同样不跨 result 相加」，与本文件逐字一致。
- **f06**：§4 的 f03 行若描述了 claude 解析处的赋值，建议补一句「`Usage`/`CostKnown` 属于末条覆盖组（f01 定义多 result 语义）」，以免 f06 把解析循环抽进 `ClaudeDriver` 时把 `Usage` 放进相加的那一组。

---

## 评审答复（第四轮，2 条 blocker）

两条**全部采纳**，都是逐条对着代码与 fixture 核过之后改的。代码事实在本线 worktree（`feat/usage-stats`，HEAD `cff5332`）上核对。

| # | 结论 | 复核证据 | PRD 落点 |
|---|---|---|---|
| B1 多 result `Usage` 规则与 f01 冲突 | **采纳（冲突属实），收敛到「末条覆盖、回落也只取末条」，测试名 `TestResultEventUsageTakesLastResult`、只由本线写** | 评审所引 f01 14:55 版（R3）确与本文件第四版互斥：R3 回落用例 want `2+4`/`15746+3232`，本文件 AC-1 第 2 例 want 4/3232；R3「`Present` 取或」下第 3 例得 `Present=true`，本文件 want false；测试名与归属也两套。修订时复读 f01，发现它已在 15:25 发布 R4（§3.2 第 5 条、§4 f03 行、§8 第 6 条、§9「评审答复（R4）」B1），**采用了本文件的口径**：删掉「逐项相加」与 `TestMultiResultUsageFollowsCumulativeRule`，写明「`Usage`/`CostKnown` 在覆盖组」「没有由后合入者写的第三条测试 —— 测试只有 f03 那一条」，并列出本文件 AC-1 的四例 want。所以这次收敛方向是 f01 → f03，本文件不改规则、只补齐「逐字一致」与守卫 | §3.1 把 f01 R4 的取值表**原文**（两行 + 「一句话」）嵌进来，写明两份各存一份、改一处同改另一处，并加「测试只有一条、只有本线写」；§3.7 f01 小节的「先合/后合」两段改成与 f01 R4 的两段逐条对应（f03 先合时 fixture 由本线拷进 `testdata/`，f01 复用；f01 rebase 后这条测试一行不改仍须绿、f01 PR 贴输出）；§4、§6、§2.2 的措辞对齐；AC-1 的 `TestResultEventUsageTakesLastResult` 四例 want 不变，第 2 例补「不是逐项相加 6/…/18978」，**红加第 ② 段**：按 R3 的「回落逐项相加 + `Present` 取或」实现必须把第 2、3 例抓红 —— 这正是 f01 rebase 时被禁止的改法 |
| B2 AC-7b 夹具跑不出描述的序列 | **采纳，按评审给的方向重写**：尝试 1 = heavy（`.go` + 复现测试、修复轮仍错，照 `TestPipelineFixLoopExhausted`）→ 修复耗尽 `failed`；Fresh 同一行；尝试 2 = 只改 `.vue` → light 一次通过 `pr_open`。want 改为 heavy=3 / light=2，两道红改为 `got heavy=0, light=5` 与 `got heavy=5, light=0` | (a) light 只有 `go build`/`go vet`（`verify.go:179-195`，`goSourceRepo` 无 `package.json`，`readPackageScripts` 文件不存在返回 nil），只改 `.vue` 必过 → `report.Passed()`，`pipeline.go:993` 的修复循环条件不成立，不会有 fix-1、也到不了 `failed`；(b) 只改 `internal/api/x.go` 走 heavy，`ResolveReproTests` 找不到测试 → `ErrNoReproTests`，`isReproContractErr` 为真，`redStepFailure`/`redEnvError` 都返回 nil（`pipeline.go:1346-1381`）→ 进修复回路调 fix-1，「一次通过」不成立（`TestPipelineContractViolationFixLoopRecovers` 正是这条路径）。新夹具每一步都有现成测试跑过同一路径：尝试 1 = `TestPipelineFixLoopExhausted`（终态 failed、3 次调用），尝试 2 = 只改展示层文件 → light 一次通过 `pr_open`，与 `pipeline_resume_test.go::TestPipelineResumeImplementWithSession`（只改 `README.md`、`goSourceRepo`、终态 `pr_open`）同一条验证路径，`.vue` 与 `.md` 同在 `lightExts`；重派 = `TestFailureMemoryInjectedOnRetry` | AC-7b 夹具、前置断言（加上每次尝试的终态与调用次数）、want 表、两道红全部重写；§3.2.1 代价第 2 条补一句反方向同理 |

**有所保留、说明理由的点**：

- B1：本轮修订的第一稿曾改成跟随 f01 R3（回落逐项相加、`TestMultiResultUsageFollowsCumulativeRule` 由后合入者写）；交稿前复读 f01，
  发现 f01 R4 已朝反方向收敛到本文件口径，若两边都「跟对方」就又会错开一次，于是撤回那一稿，保持第四版规则。代价照旧写明（§3.1）：
  缺 `modelUsage` 的旧 CLI 形状上多 result 时少记前几个 turn，末条两键都没有时记 0。理由也照旧：回落只是防御路径、方向是少记；
  在同一循环里维护「累计组」「单 turn 组」两套 `Usage` 语义，复杂度不值得为一个今天不出现的形状付出。若评审认为少记不可接受，
  正确的改法是**两边一起改、一起换测试**，而不是任一方单改。
- AC-7b 改档方向从 light→heavy 变成 heavy→light。§3.2.1 的写入/回补规则不区分方向，两道红在反方向上同样有效（数字对调），所以不需要再造一个 light 失败的夹具（light 失败需要让 `go build` 挂，就得在 `.vue` 之外改 `.go`，又会变回 heavy）。

**需要邻居同步的措辞**（我只改本文件）：

- **f01**：R4 已与本文件一致，无需再改规则。仅一处措辞：§3.2 第 5 条那张表在本文件 §3.1 有一份原文副本，若 f01 以后改表，请同改 f03 §3.1（反之亦然）。
- **f06**：第三轮答复里建议的那句「`Usage`/`CostKnown` 属于末条覆盖组（f01 定义多 result 语义）」仍然有效，建议补上；f06 抽 `ClaudeDriver` 时这段赋值整段照搬，`TestResultEventUsageTakesLastResult` 守着。

---

## 评审答复（第五轮，2 条 blocker）

两条**全部采纳**。代码事实在本线 worktree（`feat/usage-stats`，HEAD `cff5332`）上核对。

| # | 结论 | 复核证据 | PRD 落点 |
|---|---|---|---|
| B1 `group=task` 的 `title` 无来源 | **采纳（属实）**。不加列、不调 Linear，标签只取库内列，来源逐字段写死；键名不再叫 `title` | `migrations/0001`/`0010`/`0021`：`tasks` 只有 `external_key`/`external_id`（0021 由 `linear_issue_key`/`linear_issue_id` 改名），无标题列；`TaskRow`（`internal/store/query.go:20-50`）无 Title，只有内置任务经 `query.go:100-101` 的 `LEFT JOIN issues` 拿 `IssueID`；`prds`（`0023_prds.up.sql`）只有 `original_input`/`document jsonb`；`Document.OneLiner` 是 `Section{status,body}`（`internal/prd/document.go:90-95,146`），所以路径是 `document->'oneLiner'->>'body'` 而非评审写的 `document->'oneLiner'`（后者是对象），`export.go:73` 已有拿它当标题的先例 | §3.3 `byGroup` 表 `task` 行改为 `externalKey`/`trackerProvider`/`issueTitle`（任务）与 `oneLiner`（PRD）；新增「标签字段的来源」表（来源、null 条件、JOIN 条件逐字照抄 `query.go:100-101`、80 rune 截断）与「admin 端点不带任何标签字段」段（结构上不 JOIN、独立响应结构体）；admin 段与 §3.6 第 3 条措辞同步；AC-7-① 的 `TestUsageGroupTaskHTTP` 按同一来源重写（六项 want + 跨属主 `Tx` 用例 + 四道红），admin 侧改成对每一项断言键集合 |
| B2 AC-9 `totals.runs` 等于 A+B 在共享库上不稳 | **采纳（属实）**，按评审方向改 | `/api/admin/usage` 无属主过滤（§3.3），`totals` 覆盖全库；`TestBackfillRoundTripNoDoubleCount` 等在测试库上跑 0039 up 会给其他任务回填行；进程被杀时残留不清 | AC-9：A、B 带随机后缀；只断言 `byUser` 中 A、B 两项的 `runs`/`costUsd`/`costKnown`；`totals.runs >= 3`、`totals.costUsd >= 0.35`；§3.3 写明 `byUser` 不截断（否则「两项必在」在大库上也不稳） |

**有所保留、说明理由的点**：

- B1：评审建议的「去掉这个字段」未采纳。用量页按任务看时只有 `task:<id>` 无法辨认，`externalKey` 本来就在库里、任务列表也在展示，成本为零；
  PRD 的 `oneLiner` 同理。代价：Linear 任务只显示编号不显示标题（要标题得点链接跳 Linear），写在 §3.3。
- B2：在评审的两条之外多加了一条 `totals.runs == Σ byUser.runs`——它是同一查询内的恒等式，与库里有多少残留无关，补回了「只断言下界」丢掉的那部分对账强度。

**需要邻居同步的措辞**：无。`group=task` 标签与 admin 端点只在本线内，不与其他 PRD 共享字段名。



---

## 实现记录

结论先行：AC-1 … AC-13（含 AC-1b/2b/2c/2d/7b/7c）全部按契约实现并验证；偏离与未做项逐条列在下面，
没有一条削弱验证体系、改状态机语义或改「非属主 404」。

### 落点

| 层 | 文件 | 内容 |
|---|---|---|
| 驱动 | `internal/integration/agent/agent.go` | `Result.Usage` / `Result.CostKnown`；`resultEvent` 解析 `usage` 与 `modelUsage`（`usageOf`，按 costUSD 降序、并列按名升序）；放在与 `CostUSD` 同一组覆盖赋值里（末条胜出）；总超时 `%w context.DeadlineExceeded`（文案前缀不变） |
| 驱动 | `internal/integration/agent/usage.go` | `ModelUsage`、`Usage`、`RunRecord`、`UsageRecorder`、`TierBackfiller`、`RecordRun`（nil 安全、`WithoutCancel`+3s、失败 `slog.Warn("agent 用量落库失败")`）、`CodexUsage` + `UsageFromCodex` |
| 迁移 | `migrations/0038…0040` | 建表 / 回填 / `CONCURRENTLY` 建索引（非事务标记，up/down 都带），列、约束与回填口径同 PRD §3.1 |
| store | `internal/store/usage.go` | `UsageBucket`、`RunStatus`、`RecordAgentRun`（`INSERT … SELECT` 从目标行取归属，`tgt/ins` 计数区分「目标已删」与「重复写」）、`SetAgentRunTier`、`UsageStats`（属主，唯一入口 `usageScope`）、`AdminUsageStats`（不做标签 JOIN、返回类型没有标签字段）、`TaskUsageFor` |
| runner | `internal/runner/pipeline.go` | `Pipeline.Usage` / `UsageTier`；分诊与 `runAgent`（实现 / 降级重入 / fix-N，含 review 轮）在判错前记账；`stageVerify` 定档后 `rc.tier = tier` + 回补 |
| prd | `internal/prd/service.go` | `Service.Usage`；`runOnce(ctx, spec, p, timeout)` 在 `Agent.Run` 返回那一刻用原始 `res, err` 记账 |
| preview | `internal/preview/{manager,recommend}.go` | 导出字段 `Manager.Usage`（`SetRecommender` 签名不变），推荐在判错前记账 |
| httpapi | `internal/httpapi/usage.go`、`api.go` | 三个端点、参数校验（400 + 中文原因）、`api.go` 读接口注释修订 |
| 装配 | `cmd/lathe/main.go` | `Usage: st, UsageTier: st`、`prd.Service{Usage: st}`、`previewMgr.Usage = st` |
| 前端 | `web/src/views/Usage.vue`、`components/TaskUsage.vue` 等 | `/usage` 页、任务详情「用量」卡、导航、CostPanel 底部链接与口径说明、`/users` 页「全局用量」入口、交付摘要徽章标「最近一轮」；未加任何依赖 |

### 验收怎么做的（红 → 绿）

红证据都在 `/opt/lathe-wt/evidence/f03/`：

| AC | 测试 | 红 |
|---|---|---|
| AC-1 | `TestResultEventParsesUsage`、`TestResultEventUsageTakesLastResult` | `red-AC1.txt`（字段不存在、编译失败）；`red-AC1-multi-result.txt`：错误实现①「各条相加」→ `got {8 18 12532 34724}, want {6 12 12532 18978}`；错误实现②「回落 usage 相加、Present 取或」→ 第 2 例 `got {6 12 12532 18978}, want {4 6 12532 3232}`、第 3 例 `got Present=true, want false` |
| AC-1b | `TestUsageFromCodex`、`TestRecordAgentRunCostUnknown`、`TestUsageStatsCostUnknownIsNull` | 编译失败（`red-AC1.txt`、`red-store-compile.txt`） |
| AC-2 | `TestPipelineRecordsAgentRunPerInvocation`、`TestPipelineRecordsRunWhenResultMissing`、`TestRunStatus`、`TestRunTimeoutKillsEntireProcessTree` 新断言 | `red-AC2-runner.txt`（编译失败）；`red-store-42P01.txt`（未迁移时 `42P01`）；`red-AC2-timeout-wrap.txt`（去掉 `%w` → `errors.Is … got false`） |
| AC-2b | `TestPipelineRecordsReviewRoundRuns` | 同 `red-AC2-runner.txt` |
| AC-2c | `TestRunAgentRecordsUsagePerAttempt`、`TestRecordAgentRunPRDOwnership` | `red-AC2c.txt`（`Service.Usage` 不存在） |
| AC-2d | `TestRecommendRecordsUsage`、`TestRecordAgentRunPreviewOwnership` | `red-AC2d.txt`（`Manager.Usage` 不存在） |
| AC-3 | `TestUsageRecorderFailureDoesNotFailTask`、`TestRunAgentUsageFailureIsNonFatal`、`TestRecommendUsageFailureIsNonFatal` | 同上编译失败 |
| AC-4 | `TestRecordAgentRunIsIdempotent`、`TestRecordAgentRunClampsEndedAt`、`TestAgentRunsAttemptUniqIsNullsNotDistinct` | `red-store-42P01.txt`；约束形态的红沿用评审期 `review-r1-nulls-distinct.txt` |
| AC-5 | `TestUsageBucket` | 编译失败 |
| AC-6 | `TestAgentRunsIndexesValid`、`TestBackfillBucketMatchesUsageBucket`、`TestBackfillRoundTripNoDoubleCount`；命令行 `ac6-migrate.txt`、`ac6-roundtrip-no-double.txt` | `red-AC6-roundtrip.txt`（第二版 SQL：首例 `got 3/0.60, want 2/0.35`）；`red-AC6-clock-skew.txt`（第三版 SQL：节点快 120s `got 3/0.60`、节点慢 1h `got 1/0.25`，与评审期复现一致）。两版错误 SQL 存在 `internal/store/testdata/0039_backfill_v{2,3}.sql`，由 `LATHE_F03_BACKFILL_VARIANT` 切换，只用于红证据 |
| AC-7 | `TestUsageStatsAggregates`、`TestUsageStatsGroupTaskLimit`、`TestUsageStatsByModel`、`TestUsageStatsBucketFilter`、`TestUsageGroupTaskHTTP`、`TestTaskUsageDetail` | `red-AC7-group-task.txt`（只按 `cost DESC` → T4 排第一）；`red-AC7-bymodel.txt`（直接展开 `'[]'` → `(unknown)` 消失）；`red-AC7-bucket.txt`（按 phase 过滤 → `prd_plan got 0`）；`red-AC7-labels.txt`（①写成 `title`；②JOIN 两条同时漏 → `Tx got issueTitle="B的工单-r"`）；`red-AC7-labels-join-r2.txt`（两条 JOIN 条件**各自单独**漏掉也抓红，见偏离第 10 条）；`red-AC7c-AC8-AC10-httpapi.txt`（路由不存在 404） |
| AC-7b | `TestUsageTierFollowsAttemptAcrossFreshRetry`、`TestSetAgentRunTierOnlyFillsNull` | `red-AC7b.txt`：方案 (a) JOIN `tasks.verify_tier` → `heavy=0, light=5`；写入点读 `tasks.verify_tier` → `heavy=5, light=0`（与 PRD 预期逐字一致） |
| AC-8 / 9 / 10 | `TestUsageIsolatedByOwner`、`TestAdminUsageGlobal`、`TestAdminUsageForbiddenForMember`、`TestUsageRejectsBadParams`、`TestUsageIgnoresUnknownParams`、`TestUsageZeroValue` | `red-AC7c-AC8-AC10-httpapi.txt` |
| AC-11 | `TestRecordTruncatesModelsByRune` | 编译失败 |
| AC-12 | Playwright 手测（:8403 / `lathe_f03`），截图与 DOM 断言在 `ui/` | — |
| AC-13 | 门禁全绿，输出见 PR 描述 | — |

AC-12 的 DOM 断言（`ui/dom-checks.json`）：点「任务」分组后 URL 变成 `/usage?group=task`，刷新后仍是它且按钮保持选中；
空时间范围显示「该范围内没有运行记录」且页面不含 `$0.0000`；任务详情「用量」卡逐行列出档位、状态、费用（被杀的那行显示「未知」）；
管理员 `?scope=all` 出现「按用户」表；普通成员带 `?scope=all` 访问时页面不显示全局开关、仍是自己的数据。

### 偏离与取舍

1. **`to` 的边界**：PRD 没写开闭。实现是半开区间 `[from, to)`：RFC3339 的 `to` 即上界；`YYYY-MM-DD` 的 `to` 含当天（上界取次日 00:00 UTC）。
2. **分组项的 token 形状**：`byBucket` / `byGroup` / `byDay` / `byUser` 的项用嵌套 `tokens:{input,output,cacheRead,cacheCreation}`（与 AC-9 的键白名单、任务明细一致），
   不带 `turns`；`byModel`（以及 `group=model` 时的 `byGroup`）沿用 PRD 的平铺 `inputTokens…`，好与 `totals` 直接对账，也不带 `durationMs`
   （一行多模型时时长无从拆分）。`totals` / `summary` 平铺。
3. **「含回填行时提示」**：响应里没有「本集合含回填行」的标志，`/usage` 页把回填的三条精度损失（档位近似、不区分 review 轮、无 token）
   写成常驻页脚说明，任务详情「用量」卡逐行标「回填」。
4. **AC-3 的「日志 1 条」**：完整流水线至少两次 Run（分诊 + 实现），所以 runner 侧用「分诊判不明确 → blocked_spec」这条既定终态的路径，
   恰好一次 Run、一条告警，断言 `记账调用 = 告警条数 = 1`。
5. **`UsageFromCodex` 的签名**：`UsageFromCodex(turns []CodexUsage) Usage`，`CodexUsage` 带 snake_case 的 JSON 标签。f06 若先合入、收集的是
   `[]json.RawMessage`，rebase 时逐条 `json.Unmarshal` 进 `CodexUsage` 即可，映射口径不变。
6. **`TestCodexResultUsage` 未写**：f06 尚未合入 dev，本线没有 codex 驱动可测。按 §3.7「后合入的一方补」，f06 rebase 时补这条与那一行赋值。
7. **preview 测试依赖 store**：`TestRecommendRecordsUsage` 用 `store.RunStatus` 判定状态（只在测试里 import，生产代码 `preview` 仍不依赖 `store`）。
8. **额外的测试**：`TestRecordRun`（nil 安全、取消后照写、失败不上抛）、`TestAdminUsageStatsByUser`（store 层的 byUser 不变量）、
   `TestPipelineRecordsAgentRunPerInvocation` 顺带断言三行都回补成 heavy。
9. **AGENTS.md §3「agent 提示词 / 驱动」行要求的真实任务执行证据**：本线不连真实外网、不起真实 claude，没有跑真实任务。驱动侧的证据是
   真实 CLI 样本（`claude-result-event-sample.json`、f01 双 result fixture）喂解析路径，加上假 driver 走完整流水线（含 Fresh 改档、review 轮、
   超时/判死/无结果）落真实库。合入前若要补一次真实运行，看 `agent_runs` 里该任务的行即可。
10. **`TestUsageGroupTaskHTTP` 多两个夹具**（第 1 轮审计后补）：PRD 的红②写的是 JOIN 漏 `t.tracker_provider='internal'` **或**
    `i.user_id=t.user_id` 都要抓红，但只有 `Tx`（Linear 任务、key 撞 B 的工单）时，单独漏属主条件仍是绿的——`Tx` 先被
    provider 条件挡掉了；单独漏 provider 条件也是绿的——`Tx` 会被属主条件挡掉。补 `Ty` = A 的**内置**任务、`external_key` = B 的 `LT-b`
    （只有属主条件挡得住，want `issueTitle=null`，**不是** `B的工单-r`）与 `Tz` = A 的 **Linear** 任务、`external_key` = A 自己的 `LT-a`
    （只有 provider 条件挡得住，want `issueTitle=null`，**不是** `内置标题-r`）。工单号来自全局序列，现实中 `Ty` 撞不上，但 JOIN 条件
    要在 SQL 里可见、不靠这个不变量（§3.3）。三种漏法（只漏属主 / 只漏 provider / 两条都漏）的红与原 SQL 的绿在 `red-AC7-labels-join-r2.txt`。

### 与邻居的接缝（先合入者不依赖后合入者）

- **f01**：`Usage` / `CostKnown` 在覆盖组；本线已把 f01 的 `replay-user.ndjson` 拷进 `internal/integration/agent/testdata/`（同名同内容）。
  f01 rebase 时只把 `NumTurns` / `DurationMS` 改成累加，`TestResultEventUsageTakesLastResult` 一行不改须绿。
- **f04**：本线不碰 `agent_events_phase_check`；`implement-reentry` 的账只在 `agent_runs` 里（`TestPipelineRecordsReviewRoundRuns` 覆盖）。
- **f06**：见上第 5、6 条。
- **f12**：属主过滤只有 `usageScope(userID)` 一个入口，换成统一的可见性函数时只改这一处。
