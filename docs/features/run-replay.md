# 运行回放体验（f04）

> **状态：定稿**。§1–§8 原样取自评审通过的 PRD v5（其中「设计稿 v5」的版本说明照录未改），
> §9 是实现与验收结果，实现完成后补记。文中的 `/opt/lathe-wt/evidence/f04/` 是本线的
> 本地证据目录（不入库）。

> 状态：设计稿 **v5**（v2 按第一轮评审 B1–B4 修订，见 §8；v3 按第二轮评审 B1–B3 及非阻塞建议修订，见 §8.2；v4 按第三轮评审 B1 修订 `segmentRuns` 的运行内判定，见 §8.3；v5 按第四轮评审 B1–B5 修订 `/events` 兼容契约、v-html 守卫范围、AC-5 用例路径、`lastFailure`、过滤与时间纯函数的用例，见 §8.4）。分支 `feat/run-replay`，base `origin/dev`（`cff5332`）。
> 迁移号段 0041–0043（本 PRD 只用 **0041**，0042–0043 不用、不占）；手测 serve 端口 8404（库 `lathe_f04`）。
> 证据目录 `/opt/lathe-wt/evidence/f04/`。
> **定稿后本文件原样落入 `docs/features/run-replay.md`**（见 §7）。

---

## 1. 问题与目标

### 1.1 为什么 Lathe 需要它

Lathe 的产品价值是「**已验证**的 PR」，而人对验证的信任来自能**事后复盘**：
agent 每一步干了什么、哪一步挂了、红绿证明立没立起来。Lathe 是无人值守批处理，
人几乎都是在任务结束之后才来看详情页（`docs/04-agent-visibility.md:263-271` §7.6
的原话：「人是事后看结论」）。所以详情页的执行日志首先得是一个**回放**工具，
其次才是实时滚动的视图。

今天的执行日志面板是按「实时滚动」设计的，拿来回放有五个硬伤（§2 有取证）：

1. **任务结束后看不全**：终态任务只拉前 400 条事件，之后不再拉取，界面上**也不提示**
   还有没拉到的（§2.2-1）。而失败的现场恰恰在事件流的**尾部**。
2. **整段运行凭空消失**：`implement-reentry` 阶段（resume 失败降级后开的新会话）的
   事件全部被数据库 CHECK 约束拒绝、整批丢弃（§2.2-2，已在 `lathe_f04` 上实测复现）。
3. **没有时间维度**：单条事件不显示时间，看不出「卡了 6 分钟」发生在哪一步（§2.2-3）。
4. **失败要靠人肉找**：报错的工具调用只是一行红色 summary，混在几百行里；没有
   「跳到失败」的入口，失败卡片（`failureReason`）也和日志断开（§2.2-4）。
5. **红绿证据不醒目、完整日志看不到**：`repro_fail` 「通过」到底是红还是绿要靠人脑翻译；
   `verifications.log_ref` 已经把完整验证日志落盘，**但没有任何读取入口**（§2.2-5）。

### 1.2 目标

1. **回放完整**：任意终态任务都能看到全部事件（按需分页加载，不一次塞进内存），
   找回被 CHECK 约束丢掉的那一类事件（只针对之后的新运行）。
2. **每步带时间**：绝对时间（悬停显示）+ 相对任务/阶段起点的偏移 + 与下一步的间隔，
   并如实标出 ±200ms 的粒度。
3. **按阶段分组、可过滤**：阶段 chip（含条数与失败数）、「只看失败」「隐藏思考」
   「只看工具调用」。
4. **工具调用折叠展开**：沿用现有的 tool_use/tool_result 配对，加上「全部展开/全部折叠」；
   折叠时**不渲染**正文。
5. **失败高亮 + 一键定位**：统一的失败判定；「跳到第一个失败 / 上一个 / 下一个」，
   不在内存里的失败也能定位；失败卡片里加「在日志中定位」；`#ev-<id>` 可作为深链接分享。
6. **红绿证据醒目**：验证区按轮次画「红 → 绿 → 回归」证明条，**数据只取 `verifications` 表**
   （证据本体），不取事件流。
7. **长输出按需加载**：老事件分页；验证步骤的完整日志按块加载（经过脱敏）。

### 1.3 硬约束（来自任务边界与 AGENTS.md）

- **不改事件采集口径**：`agent.Digest`、`EventSink`、`BatchSink`、`verifyStepEntry` 一行不改，
  不加采集字段（与 f03 不重叠）。唯一的 schema 改动 0041 是**放宽一条约束**，让流水线
  **本来就在写**的事件不再被拒（§3.1 论证为什么这不算改口径）。
- 不引入依赖（`go.mod`、`web/package.json` 不变）；不改 `internal/task/state.go`；
  不削弱验证；非属主一律 404。
- 以前端为主；后端只补**只读**的分页/过滤/汇总/日志块接口。

---

## 2. 现状取证

### 2.1 已有、直接复用

| 事实 | 位置 |
|---|---|
| 事件表 `agent_events`：`(id bigserial, task_id, phase, kind, tool, body, payload jsonb, at default now())`，游标用 `id` | `migrations/0005_agent_events.up.sql:28-54` |
| 唯一索引是 `(task_id, id)`，服务增量拉取 | `migrations/0005_agent_events.up.sql:57`（`CREATE INDEX agent_events_task_id`） |
| `agent_id` 列（subagent 归属），不进 WHERE、不建索引 | `migrations/0014_agent_events_subagent.up.sql:22-30` |
| 读接口：`AgentEventsAfter(taskID, userID, after, limit)`，`JOIN tasks` 按属主过滤，`limit` 上限 500、默认 200 | `internal/store/agents.go:74-102`（上限在 `:75`） |
| HTTP：`GET /api/tasks/{id}/events?after=&limit=` → `{events, last_id}`；非属主 404 | `internal/httpapi/api.go:72`（路由）、`:143-166`（handler，404 在 `:150-154`） |
| 前端轮询：`api.taskEvents(id, after, limit=200)` | `web/src/api.js:86-87` |
| 自链式轮询 + 内存封顶 500 条 + markdown 缓存 | `web/src/livelog.js:19`（`MAX_LIVE_EVENTS`）、`:23-32`、`:38-51`、`:56-81` |
| 阶段分组、subagent 子块、tool_use/tool_result 配对、耗时（<1s 不显示） | `web/src/views/TaskDetail.vue:92-98`（`PHASE_META`）、`:107-130`、`:141-165`、`:176-182`、`:184-200` |
| 当前阶段推断（含 push 复用 verifying 的处理） | `web/src/views/TaskDetail.vue:208-230` |
| 单条事件渲染（按 kind 分支；tool 输出 `<details>` 折叠；verify_step 红绿色） | `web/src/components/AgentEventItem.vue:62-131`（tool 输出 `:91-96`，verify_step `:124-129`） |
| 失败信号已在 payload 里：`tool_result.payload.isError`、`result.payload.isError/subtype`、`verify_step.payload.status` | `internal/integration/agent/digest.go:209-224`、`:236-269`；`internal/runner/pipeline.go:1412-1438` |
| 溢出记录：`kind='raw'`、`payload.type='overflow'` | `internal/integration/agent/batchsink.go:120-137` |
| `verifications` 表是红绿证据本体，详情 API 已返回 `logRef` | `migrations/0001_init.up.sql:156-175`；`internal/store/query.go:139-147`、`:205-219` |
| 完整验证日志落盘：`<DataDir>/verify-logs/task-<id>/round-<n>/NN-<step>.log`，0600，单文件上限 64MB；`log_ref` 存**相对 DataDir** 的路径；复现阶段是**空格分隔的多个路径** | `internal/runner/verifylog.go:41`、`:48`、`:63-105`、`:193-201`；`internal/runner/heavy.go:349-378` |
| `LogDir` 装配为 `cfg.DataDir` | `cmd/lathe/main.go:580`；`internal/config/config.go:287` |
| 写入侧脱敏两层入口 `redactText(s, registered, extraEnv)`，装配侧登记值 `Pipeline.KnownSecrets` = `cfg.KnownSecrets()` | `internal/runner/redact.go:468-477`；`internal/runner/pipeline.go:120-129`；`cmd/lathe/main.go:587` |
| 截断按 rune 回退（AGENTS.md §9） | `internal/integration/agent/agent.go:502-514`；`internal/capbuf/capbuf.go:113-128` |
| **重派复用原任务行**：`retryTask` 先转 `queued`，再 `Queue.Requeue` 重派**同一行**（不新建，任务 #313） | `internal/httpapi/api.go:356-366`。回到 `queued` 的入口有 `api.go:356`（manual_retry）、`:418`（manual_review_retry）、`:488`（manual_approve）、`internal/runner/review.go:179`（pr_review）、`cmd/lathe/queue.go:325`（restart_reconcile）、`internal/task/machine.go:944`（依赖恢复唤醒）。**但「再跑一次验证」不一定经过 `queued`**（v3 更正，第二轮 B1）：rebase 跟进重验 `mergepoll.go:547-566` 的 `reenterForVerify` 在最常见的情况下（任务停在 `pr_open`）走 `pr_open→review_feedback→implementing`（`:551` 的 payload `reason=rebase_followup_reverify`、actor `system:rebase-followup`），随后 `:472-479` 以 `RetryPlan{Entry: EntryVerify}` 调 `Pipeline.Execute`，由 `stageVerify` 转 `implementing→verifying`（`pipeline.go:936`），再从 `runVerify(0)` 写 round-0（`:983`）。只有任务恰好停在 `verifying` 时才走 `:569` 的 `verifying→queued`。v2 写的「所有重派入口都先转 queued」不成立 |
| **每次「验证运行」都恰好以一次 `to_state='verifying'` 开头**（v3 新增，运行边界的取证依据） | 状态机里进入 `verifying` 只有两条边：`queued→verifying`、`implementing→verifying`（`internal/task/state.go:90`、`:106`）。runner 里只有两处转入：`stageVerify`（`pipeline.go:936`，每次 `Execute` 至多一次）与 `stagePushAndPR`（`:1114-1121`，仅当从 `queued` 直接续跑推送，即 `EntryPush`，这一次运行**不写** verifications）。`verifications` 只由 `persistVerifications` 写入（`pipeline.go:1388-1402`），调用点 `:987`、`:1050` 都在 `stageVerify` 里、在 `:936` 的转移之后；修复回路（`:1000-1050`）留在 `verifying` 里不做状态转移。转移事件与 verifications 的 `at` 都取 DB `now()`，转移事务先于验证结束提交。所以**每一条 verification 归属于 `at` 不晚于它的最后一条 `to_state='verifying'` 事件**——与哪个入口触发无关 |
| **日志路径在重试间复用并被覆盖**：每次 `runVerify(round)` 新建 `fileStepLogger`，序号从 01 起；目录只由 `task-<id>/round-<n>` 决定；文件 `O_TRUNC` 打开 | `internal/runner/verifylog.go:63-105`（`:96`）、`:193-201`；`internal/runner/pipeline.go:972`、`:983`、`:1046`。基线实测：同任务两次运行的 round-0 build 拿到**同一个** `log_ref`，第一次的 ref 读出来已是第二次的内容（`/opt/lathe-wt/evidence/f04/prd-v2-verifylog-overwrite-probe.txt`） |
| **同一次运行、同一轮里步骤名会重复，也会「回退」**（v4 新增，第三轮 B1）：`DetectLightProfile` 对每个 `go.mod` 目录各产出一组 `build`、`lint`；根目录有 `package.json` 时先产出一条 `Name=build` 的 `pnpm install`，再按脚本产出 `build`/`lint`/`typecheck`。Go 双模块 + pnpm 的仓库，一轮 light 就是 `build,lint,build,lint,build,build,lint,typecheck`。heavy 档把同一串步骤当 `HeavyParams.Light` 先跑。所以「步骤按 build→lint→typecheck→repro 的序号只增不减」**不成立**，v3 规则 3/4a 依赖它是错的 | `internal/runner/verify.go:179-237`（Go 模块 `:186-194`，pnpm install `:205`，脚本 `:214-231`）；`internal/runner/pipeline.go:1303`；`internal/runner/heavy.go:255-266`。实测：`/opt/lathe-wt/evidence/f04/prd-v4-step-sequence-probe.txt`（真实 `DetectLightProfile` + 真实 `fileStepLogger`，临时测试文件跑完即删） |
| **运行内真正严格递增的是 `log_ref` 文件名的 `NN` 前缀**（v4 新增）：`fileStepLogger.seq` 每次 `OpenStepLog` 加 1，`NN = %02d`；每次 `runVerify(round)` 都经 `stepLogger` **新建** logger，所以 `NN` 在同一次 `runVerify` 内严格递增（中途落盘失败只会跳号，不会回退），换一轮或换一次运行都从 `01` 重来。复现阶段的多段 `log_ref` 是连续的几个 `NN` | `internal/runner/verifylog.go:73-90`（`seq` 在 `:76`、`Add(1)` 在 `:86`）、`:193-201`；`internal/runner/pipeline.go:970-971`；`internal/runner/heavy.go:343-350`。探针输出中 run1/run2 的 round-0 都是 `01…08`，round-1 同样从 `01` 起 |
| **一轮之内，非 passed 的步骤之后只会跟 skipped**（v4 新增，作为没有 `log_ref` 时的轮次信号）：`RunLight` 与 `RunHeavy` 的 light 段遇到第一个非 passed 就把后续全部记成 skipped（`verify.go:246-258`、`heavy.go:254-269`）；`repro_fail`/`repro_pass`/`regression` 不 passed 时直接 `return`（`heavy.go:276-316`）。每轮的第一条步骤一定会真正执行：`DetectLightProfile` 没有步骤时直接报错（`verify.go:233-235`）。修复回路只在 `!report.Passed()` 且有 `FirstFailure` 时才开下一轮（`pipeline.go:992-997`），所以上一轮一定以一条 failed/error（后面可能跟若干 skipped）结尾。由此得到：「一条**非 skipped** 的行紧跟在一条**非 passed** 的行之后」只会出现在轮次边界上；「light 步骤（build/lint/typecheck）跟在 repro_fail/repro_pass/regression 之后」同样只会出现在边界上 | 同左所列行号 |
| **`verifications` 行只增不删**：全仓没有 `DELETE FROM verifications`；每步完成**之后**才在 `persistVerifications` 里插入（文件已关闭），`at` 取 DB `now()` | `internal/store/verifications.go:15-27`；`internal/runner/pipeline.go:1388-1402`；`migrations/0001_init.up.sql:156-175` |

### 2.2 缺口（本功能要补的）

**2.2-1 终态任务回放不全，而且不提示。**
`TaskDetail.vue:247-261`：挂载时 `loadEvents()` 拉一页（`api.js:86` 默认 `limit=200`），
`evPoller` 2 秒后再拉一页，然后 `return !isTerminal.value` → 终态即停（`:254-258`）。
所以**终态任务最多拉 400 条**。`droppedEvents` 只统计 `appendCapped` 从头部裁掉的条数
（`:79`、`:436-438`），「没拉到」的部分不计入，界面不显示任何提示。
一次带修复回路的 heavy 任务轻松上千条事件，失败现场（`fix-N` 与最后一轮 `verify`）
恰好在被截掉的尾部。注释 `:256`（「终态转移前 sink 已 drain，事件不会再多」）只论证了
「不会再**新增**」，没有论证「已经**拉全**」。

**2.2-2 `implement-reentry` 阶段事件被数据库整批拒收。**
流水线在 resume 失败降级时以 `phase="implement-reentry"` 起 EventSink
（`internal/runner/pipeline.go:713` → `:766-768`；常量 `internal/runner/eventsink.go:35`）。
但 `agent_events_phase_check` 只放行 `triage|implement|verify|review|push|fix-N`
（`migrations/0015_agent_events_push_phase.up.sql:8-10`）。`BatchSink.insert` 落库失败
只记 warn、**丢弃整批**（`internal/integration/agent/batchsink.go:183-189`）。
在 `lathe_f04` 上实测（事务内插入后回滚，没有留下残留数据）：

```
ERROR:  new row for relation "agent_events" violates check constraint "agent_events_phase_check"
DETAIL:  Failing row contains (1, 1, implement-reentry, text, null, probe, {}, ...)
```

这与 0011 修过的事故（`fix-N` 被拒，「任务 #596 看似卡死在验证中」，
`migrations/0011_agent_events_fix_phase.up.sql:1-3`）是同一类问题。常量于 2026-10-13
引入（`git log -S PhaseImplementReentry`），之后没有迁移跟上。前端 `PHASE_META`
（`TaskDetail.vue:92-98`）也不认识 `fix-N` 和 `implement-reentry`，只能显示原始字符串，
排序兜底为 9。

**2.2-3 单条事件不显示时间。**
`at` 已经随 API 返回（`internal/store/agents.go:27`），前端只拿它算工具耗时
（`TaskDetail.vue:158`、`:178-182`），从不展示。`at` 是**入库时刻**，不是发生时刻：
EventSink 每 200ms 批量刷库（`batchsink.go:33`），同批事件的时间戳相同，误差约 ±200ms。
`docs/04` §7.5（`docs/04-agent-visibility.md:248-261`）已经写明这一点，并据此把工具耗时
低于 1s 的一律不显示（`TaskDetail.vue:176`）。

**2.2-4 失败不可定位。**
报错只体现为 summary 的颜色（`AgentEventItem.vue:92-93`、`:101-102`），没有统一的失败判定，
也没有导航。失败卡片 `task.failureReason`（`TaskDetail.vue:356-380`）与日志之间没有链接。
阶段 summary 只显示条数（`:449`）。

**2.2-5 红绿证据不醒目；完整日志没有消费方。**
验证结果是一张平表（`TaskDetail.vue:480-503`）：`repro_fail` 的 status=`passed`
表示「改动前确实失败 = 红立起来了」（`internal/runner/heavy.go:318-328` 的 `runReproOn` 注释），
表格却显示成一个绿色的「✓ 通过」（`:240-245`），要靠人脑把它翻译成「红」。
多轮修复的结果平铺在一起，看不出哪一行属于哪一轮；**重试过的任务更糟**：重试复用任务行、
`verifications` 只增不删，几次运行的行按 `at` 叠在同一张表里，而且 `log_ref` 在运行之间重名
（§2.1 末三行）。`logRef` 字段虽然已经返回
（`query.go:145`），但 `grep -rn "logRef\|log_ref" web/src/` **零命中**，也没有任何 HTTP
接口读取落盘日志 —— 这正是 T4 想要避免的「有列没消费方」
（`docs/08-debt-cleanup.md:764-769`）。

**2.2-6 长输出全量渲染。**
`<details>` 折叠时，`<pre>{{ ev.result.body }}</pre>` 仍然在 DOM 里（`AgentEventItem.vue:91-96`）。
`maxEntryBody` 4KB（`digest.go:15`）× 500 条 ≈ 2MB 文本常驻 DOM。livelog.js 头注释实测过
3 万条事件下浏览器卡死（`livelog.js:4-7`）。

---

## 3. 方案

总览：**一条迁移（放宽 CHECK）+ 三个只读接口（一个扩展、两个新增）+ 一个纯函数前端模块 +
详情页日志面板与验证区重做**。不改任何写入路径。

### 3.1 数据模型与迁移（号段 0041–0043，本 PRD 只用 0041）

**0041_agent_events_reentry_phase**（事务内执行，不需要 no-transaction 标记）

```sql
-- up
ALTER TABLE agent_events DROP CONSTRAINT agent_events_phase_check;
ALTER TABLE agent_events ADD CONSTRAINT agent_events_phase_check
  CHECK (phase IN ('triage', 'implement', 'implement-reentry', 'verify', 'review', 'push')
         OR phase ~ '^fix-[0-9]+$');

-- down（沿用 0015 down 的立场：已有该阶段的行时让回滚失败，不静默删证据）
ALTER TABLE agent_events DROP CONSTRAINT agent_events_phase_check;
ALTER TABLE agent_events ADD CONSTRAINT agent_events_phase_check
  CHECK (phase IN ('triage', 'implement', 'verify', 'review', 'push')
         OR phase ~ '^fix-[0-9]+$');
```

- **为什么这不算改事件采集口径**：采集口径（哪些事件、提炼成什么形状、以什么 phase 标签落库）
  由 `Digest` 和流水线决定，代码早已写入 `implement-reentry`（`pipeline.go:713`）。
  CHECK 拒收是 schema 与代码之间的漂移，修掉它是让既有口径生效。
  `digest.go`、`eventsink.go`、`batchsink.go`、`pipeline.go` 都不改。
- **为什么不顺手把 CHECK 改成正则全放开**：0005 的立场是 phase 集合要显式枚举、随分期演进
  （`0005_agent_events.up.sql:37-48`）。改成全放开会让下一个拼写错误静默入库。
- **锁**：`ADD CONSTRAINT ... CHECK` 会全表扫描校验并持有 ACCESS EXCLUSIVE 锁。
  0011 和 0015 在同一张表上做过一模一样的操作，生产上没有出过问题。备选方案
  `NOT VALID` + `VALIDATE CONSTRAINT` 两步走可以把锁降成 SHARE UPDATE EXCLUSIVE，
  但需要 no-transaction 标记，复杂度与收益不成比例。§6 记为已接受的代价。
- **只救未来**：已经丢掉的历史事件找不回来；界面对历史任务不做任何补偿。
- **不加索引**：§3.2 的新查询全部是 `WHERE task_id=$1 [AND phase=ANY($2)] AND id <|> $3 ORDER BY id`，
  走 `(task_id, id)` 索引后在单任务范围内过滤（单任务事件量是百到千级，`0005` 头注释）。
  AC-4 用 `EXPLAIN` 钉住「走 `agent_events_task_id`」。0042、0043 不使用。

前端同步：`PHASE_META` 增加 `implement-reentry`（「实现（新会话重入）」，order 1.5）；
`fix-N` 显示为「修复第 N 轮」，order 1.75（介于 implement-reentry 与 verify 之间）、同 order 内按 N 的数值排序（取值与排序规则见 §3.3.1 `phaseMeta`/`comparePhases`，AC-6 表驱动守着）。

### 3.2 API（全部只读，全部要求登录，非属主 404）

所有新增和扩展的 handler 共用一个归属判定 helper：`a.ownedTask(w, r) (*task.Task, bool)`，
逻辑照搬 `api.go:149-154`（`a.Tasks.Get` + `tk.UserID != CurrentUser(r).ID` → 404「任务不存在」）。
抽成一处是为了让 f12（团队可见性）落地时只改一个地方（§3.7）。`ownedTask` 只给**读**接口用；store 查询一律传 `tk.UserID`（任务属主），不传 `CurrentUser(r).ID`。

#### 3.2.1 扩展 `GET /api/tasks/{id}/events`（向后兼容）

新增可选参数。**兼容契约（v5 收紧，第四轮 B1）**：请求里**一个新参数都不带**（只有 `after`/`limit` 或什么都不带）时，
行为和响应与现在**完全一致**——响应顶层键集合**恰为** `{events, last_id}`，不多不少。这与 f01 PRD AC-17 的
搭配红线（`f01-interject.md:1086-1089`，在 `TestTaskEventsEndpoint` 里断言「顶层键恰为 `{events, last_id}`」）
是同一条契约，两条线谁先合入都不会让对方的测试变红。新字段 `first_id`/`has_more` **只在**请求带了下表任一参数时出现：

| 参数 | 语义 | 校验 |
|---|---|---|
| `paged=1` | **只开扩展响应、不改查询语义**：与 `after` 搭配时查询与旧接口逐行相同，只多返回 `first_id`/`has_more`。f04 前端**每一个**请求都带它（实时模式是 `after=lastId&paged=1`，§3.3.1），旧客户端与 f01 的测试不带它 | 只接受 `1`，其他值 → 400 |
| `before=<id>` | 反向分页：返回 `id < before` 的**最近** `limit` 条，响应内仍按 `id` 升序排列。`before` 与 `after` 同时传 → 400 | 正整数 |
| `phase=a,b` | 只返回这些阶段 | 每项必须匹配 `^(triage|implement|implement-reentry|verify|review|push|fix-[0-9]+)$`，否则 400（不把任意字符串带进查询，与 `listTasks` 的 state 校验同一立场，`api.go:96-108`） |
| `kind=a,b` | 只返回这些 kind | 必须属于 0014 CHECK 的 9 个值，否则 400 |
| `failures=1` | v5 新增（第四轮 B4）：只返回 §3.2.2 失败判定命中的事件（error 与 overflow 警告，与 summary 的 `failures` 数组同口径），判定是同一份 SQL 片段（一个 Go 常量，summary 与本参数共用）；给失败导航在 `failures` 数组截断后继续 ↑/↓ 用 | 只接受 `1`，其他值 → 400 |

带了上表任一参数时的响应（`events` 和 `last_id` 的语义保持原样，只多两个键；不带时见上方兼容契约，这两个键**不出现**）：

```jsonc
{ "events": [...], "last_id": 12345,
  "first_id": 12001,      // 本批最小 id；本批为空时为 null
  "has_more": true }      // before 模式：是否还有更早的；after 模式：本批是否填满 limit（=还可能有更多）
```

store 新增 `AgentEventsBefore(ctx, taskID, userID, before int64, f EventFilter, limit int)`，
`AgentEventsAfter` 增加可选的 `EventFilter`（零值 = 不过滤）。两者都保留 `JOIN tasks ... t.user_id = $2`
（`agents.go:80-85`）作为第二道隔离。`has_more` 的实现是多取一条（`LIMIT n+1`），不额外跑 count。
handler 里「是否扩展响应」只由一个布尔 `extended := q.Has("paged") || q.Has("before") || q.Has("phase") || q.Has("kind") || q.Has("failures")`
决定；`extended == false` 时走与 base 相同的 `writeJSON(..., map[string]any{"events": events, "last_id": lastID})`。
`web/src/api.js` 的 `taskEvents(id, after, limit)` 签名保留，新增第四个可选参数 `opts`（`{before, phase, kind, failures}`），
f04 的调用方一律经 `createEventWindow` 发出、自动带 `paged=1`。

#### 3.2.2 新增 `GET /api/tasks/{id}/events/summary`

一条聚合 SQL（按 `task_id` 走索引），返回：

```jsonc
{
  "total": 1834, "firstId": 10001, "lastId": 11834,
  "firstAt": "...", "lastAt": "...",
  "phases": [ { "phase": "implement", "count": 912, "firstId": 10040, "lastId": 10951,
                "firstAt": "...", "lastAt": "...", "failures": 3 } ],
  "failures": [ { "id": 10388, "phase": "implement", "kind": "tool_result", "at": "...", "severity": "error" } ],
  "failuresTruncated": false,
  "failureCount": 7,         // severity=error 的总数（不受 200 截断影响）
  "firstFailure": { "id": 10388, "phase": "implement", "kind": "tool_result", "at": "...", "severity": "error" },
  "lastFailure":  { "id": 11790, "phase": "verify",    "kind": "verify_step", "at": "...", "severity": "error" }
}
```

- **失败判定（后端与前端共用同一份定义，AC-6 用同一张表驱动两边）**：
  - `kind='tool_result' AND payload->>'isError' = 'true'`
  - `kind='verify_step' AND payload->>'status' IN ('failed','error')`
  - `kind='result' AND (payload->>'isError' = 'true' OR payload->>'subtype' <> 'success')`
  - `kind='raw' AND payload->>'type' = 'overflow'`（归为「警告」，`severity: "warn"`，参与定位，但不计入失败数）
- `failures` 最多返回 200 条（按 id 升序），超出时 `failuresTruncated=true`，前端如实提示。
- **`firstFailure` / `lastFailure`（v5 新增，第四轮 B4）**：分别是 `severity=error` 的失败里 id **最小**、id **最大**
  的那一条（只算 error，不算 overflow 警告——警告不会导致任务失败），**与 `failures` 的截断无关**：用同一条失败判定
  `WHERE` 各跑一次 `ORDER BY id ASC|DESC LIMIT 1`（走 `(task_id, id)` 索引，AC-4 的 EXPLAIN 一并覆盖）。没有失败时为 `null`。
  `failureCount` 是 error 的总数（`count(*) FILTER (...)`，与各阶段 `failures` 之和相等）。
  失败卡片的「在日志中定位」**只读 `lastFailure`**，不从 `failures` 数组取末项——数组截断时末项是第 200 个失败，不是导致
  任务失败的那一步（§3.3.2）。
- **截断后的失败导航不靠 `failures` 数组走到底**：「↓ 下一个 / ↑ 上一个」以**当前失败的 id** 为游标向服务端要下一页，
  即 `GET /events?after=<当前 id>&paged=1&failures=1&limit=1`（「↑」用 `before=<当前 id>`），`failures=1` 让查询带上
  同一条失败判定 `WHERE`（含 overflow 警告，与 `failures` 数组同口径——导航本来就走警告），见 §3.2.1 参数表。`failures` 数组只用来
  显示列表和前 200 个的快速跳转，导航本身不受 200 的上限约束。
- 不放进 5 秒轮询的 `/api/tasks/{id}`：详情页只在挂载时、终态切换时、以及实时模式下每 15 秒
  拉一次 summary。理由与 `costStats` 独立成端点相同（`api.go:212-216`）。

#### 3.2.3 新增 `GET /api/tasks/{id}/verifications/{vid}/log?part=&offset=&limit=`

完整验证日志按块读取，这是 `verifications.log_ref` 的第一个读取方。

- **归属**：`ownedTask` 通过后，再查 `verifications WHERE id=$vid AND task_id=$id`；
  不存在、不属于该任务、`log_ref IS NULL` → 一律 404（不区分原因）。
- **多段**：`log_ref` 按空格切分成多个片段（`heavy.go:349-378`）；`part` 从 0 开始，越界 → 404。
  响应带 `parts` 总数。
- **路径安全**（防穿越，参照 docs/09 AC2「下载路径不拼接用户输入」，`docs/09-internal-issues.md:132`）：
  1. 用户输入只有 `vid`、`part`、`offset`、`limit` 四个整数，路径**只**取自数据库里的 `log_ref`；
  2. `filepath.Clean` 之后必须是相对路径，并且以 `verify-logs/task-<id>/` 开头（`<id>` 取自路径参数，
     已通过归属校验）；
  3. 拼成 `filepath.Join(LogDir, ref)` 后再做 `filepath.EvalSymlinks`，结果必须仍然位于
     `<LogDir>/verify-logs/task-<id>/` 之下；
  4. 必须是常规文件（`Mode().IsRegular()`）。
  任何一条不满足 → 404，并记 warn（只记 task 与 vid，不记 `log_ref` 原文以外的内容）。
- **重试覆盖判定（v2 新增，B1）**：重试复用任务行，新一次运行把日志写回**同一个**
  `verify-logs/task-<id>/round-<n>/NN-<step>.log`（序号从 01 重来、`O_TRUNC`），而旧的
  `verifications` 行还在（§2.1 末三行，基线实测见 `evidence/f04/prd-v2-verifylog-overwrite-probe.txt`）。
  拿旧 `vid` 读到的会是后一次运行的内容——证据挂错运行，违反铁律 1。所以在路径校验之后、读取之前，
  对**本次请求的那一段**（`part` 对应的路径）做三道判定，任一命中即判「已被覆盖」：
  1. **库内判定（确定性，不依赖时钟）**：
     `EXISTS (SELECT 1 FROM verifications WHERE task_id=$id AND id > $vid AND log_ref IS NOT NULL AND $path = ANY(string_to_array(log_ref, ' ')))`
     ——有更新的行声明了同一个路径，文件就归它。
  2. **时间判定（兜住「后一次运行正在写、还没插行」的窗口）**：`Stat` 得到的
     `ModTime` 晚于 `verifications.at + logMtimeSkew`（5s）。依据：`persistVerifications` 在整份
     `Report` 跑完、每个日志文件都已关闭**之后**才插行（`pipeline.go:1388-1402`，`at` 取 DB `now()`），
     所以本行自己写的文件 `ModTime ≤ at`；文件比记录新，只能是后来者写的。5s 容差吸收 DB 与宿主机的
     时钟偏差；偏差超过 5s 且宿主机更快时会**误判为已覆盖**——那是 fail-closed（藏证据），不会把别人的
     证据挂上来，§6 记为代价。
  3. **读后复核**：块读完后再 `Stat` 一次，`size` 或 `ModTime` 与读前不同 → 同样判为已覆盖，本次读到的内容丢弃。
  判为已覆盖时返回 **404**，body 为 `{"error":"日志已被后一次运行覆盖","code":"log_overwritten"}`。
  这条 404 只可能在 `ownedTask` 和 `verifications.task_id` 都通过之后出现，所以对非属主仍然是统一的
  「任务不存在」，不暴露任何信息（§3.5-1 不变）。前端在拿到 detail 时已经能用同一条规则 1
  离线标出哪些行的日志被覆盖（`segmentRuns` 的 `logOverwritten`，§3.3.1），这些行不显示「完整日志」按钮；
  接口上的判定是第二道防线，不依赖前端。
- **分块与脱敏边界（v2 重写，B2）**：v1 的「向后扩展到下一个换行、最多 4KiB」有两个洞：
  单行超过 4KiB 时块边界仍会落在行内，把凭据切成两半各自脱敏；而且**即使切在换行处也不够**——
  形态层有跨行的写法。基线实测（`evidence/f04/prd-v2-redact-split-probe.txt`，在 `cff5332` 上用现有
  `redactText` 跑）：
  `password:\n  hunter2hunter2` 整段脱敏得 `password:\n  [REDACTED]`，在换行处切开分别脱敏则明文留下；
  引号值内含换行（`{"token": "abc\ndefghijk"}`）同理；登记的 DSN 从口令中间切开，两半都不命中精确层；
  `API_KEY=` 裸值从中间切开，后半截原样输出。所以 v2 把「块边界」定义为**经过验证的安全切点**：
  - **安全切点**：位置 `c` 满足 `c == 0`、或 `c == size`、或同时满足
    (i) `c` 紧跟在 `\n` 之后；
    (ii) 取 `L = data[c-W : c]`、`R = data[c : c+W]`（`W = redactContext`，越过文件头尾时截到头尾），
    `RedactForDisplay(L+R) == RedactForDisplay(L) + RedactForDisplay(R)`。
    即「在这里切开，两边各自脱敏，结果与不切完全相同」——任何跨越 `c` 的凭据、任何需要 `c` 对面上下文
    才能命中的形态规则，都会让等式不成立。这是**按定义**检验切分无害，而不是枚举哪些规则需要上下文，
    以后 `redact.go` 增加规则时这条判定自动跟上。
  - `redactContext = max(16KiB, 2 × 最长登记凭据长度)`：保证任何登记值和它两侧的形态上下文都落在窗口里
    （登记值是 `cfg.KnownSecrets()`，最长的是 DB DSN，百字节级）。
  - **请求的 `offset` 必须是安全切点**，否则 400「offset 不在块边界上」，不返回任何内容。
    前端只会传 0 或上一块返回的 `nextOffset`；这条校验是为了不让手工构造的 offset 从凭据中间开读
    （属主本人也不能在界面上看到明文，AGENTS.md §7）。
  - **块的终点**：标称终点 `e = offset + limit`；`e ≥ size` 时取 `size`。否则从 `e` 起向后找第一个
    安全切点：先试 `e` 之后的第一个换行；不安全时，下一个候选跳到 `上一个候选 + W/4` 之后的第一个换行
    （避免短行密集时逐行检验），每次请求最多检验 `maxCutProbes = 256` 个候选，且终点不超过
    `offset + limit + logLineMax`（`logLineMax = 1MiB`，即「单行上限」）。
  - **找不到安全切点**（单行超过 1MiB，或 1MiB 内全是不安全的切点）：**不返回这一段的任何内容**
    （fail-closed），响应 `{"text":"","withheld":true,"withheldBytes":<从 offset 到文件末尾的字节数>,"nextOffset":null,...}`，
    界面提示「此处有一段超过 1MiB、无法安全切分的内容（常见于压缩输出），为避免泄露凭据已停止显示；
    完整日志在服务器 `<DataDir>/<log_ref>`（0600）」。不跳过这一段继续往后读：跳过后的起点同样不是安全切点，
    接着读会重新引入半截凭据的问题。
  - 每块的返回内容为 `RedactForDisplay(data[offset:c])`。两端都是安全切点，因此只要单个凭据/形态匹配
    不长于 `W`，逐块返回的结果首尾相接就**逐字节等于**对整份文件一次性脱敏的结果（AC-5 用这条等式在
    测试数据上断言；超过 `W` 的匹配见下方「已知边界」）。
  - 安全切点都在 `\n` 之后，天然落在 rune 边界，返回的文本总是合法 UTF-8。
  - 响应：`{ "text": "...", "offset": 0, "nextOffset": 65612, "size": 1048576, "eof": false, "withheld": false, "parts": 2, "part": 0 }`。
    `limit` 默认 64KiB、上限 256KiB；单次响应的 `text` 不超过 `limit + logLineMax` 对应的脱敏结果。
    读取用 `io.NewSectionReader` 只读 `[offset-W, c+W]` 这一段，不整文件进内存。
  - 代价：每块多做最多 `1 + 3 × 检验次数` 次窗口脱敏；常见情况下第一个换行就安全，即 4 次、每次
    几十 KiB。最坏 256 次检验约 25MB 的正则扫描，属单次请求的有界开销。
- **脱敏（AGENTS.md §7 红线：界面永不出现明文凭据）**：落盘日志**没有脱敏**
  （verifylog.go 只做了 0600 权限，`:94-96`）。所以每个块返回前**必须**经过
  `runner.RedactForDisplay(text, knownSecrets)`。这是新导出的一个薄包装，
  内部调用 `redactText(s, registered, nil)`（`redact.go:468-477`），走权威层 + 形态兜底两层；
  安全切点检验也只调用它，不另写一套规则。`httpapi` 已经 import `runner`（`api.go:16`），不新增依赖边。
  `knownSecrets` 由装配侧以 `cfg.KnownSecrets()` 注入 `API.KnownSecrets`（装配位置见 §3.4）。
  已知边界：读取时拿不到那一轮的 `StackEnv`，隔离栈的临时口令只能靠形态层识别 `user:pass@` DSN
  （`redact.go:57-62`）；以及长度超过 `redactContext` 且跨行的单个形态匹配（如 16KiB 以上的多行引号值）
  不受切点检验保护。§6 记为风险。
- **`API.LogDir` 为空**（测试或未装配）→ 503「验证日志未启用」，与 `Baselines == nil` 的处理一致
  （`api.go:771-774`）。生产装配由 AC-8 的装配守卫测试保证非空，不靠手测发现（B4）。

#### 3.2.4 路由与鉴权登记

在 `api.go:70-77` 的读接口区追加两条路由，都包在 `a.Auth.RequireFunc` 里。
把两条新路由加进 `TestAPIRequiresAuth` 的端点表（未认证 → 401）。

### 3.3 前端

#### 3.3.1 新模块 `web/src/replay.js`（纯函数，无 DOM、无 Vue 依赖）

把 `TaskDetail.vue:107-200` 的分组和配对逻辑**原样搬过来**（语义不变，AC-7 有等价测试），并新增：

| 导出 | 作用 |
|---|---|
| `groupSubagents(list)`、`pairTools(list)` | 从 TaskDetail 平移，行为不变 |
| `phaseMeta(phase)` / `comparePhases(a, b)` | 统一的阶段标签与排序，含 `implement-reentry`、`fix-N`。v5 写死取值（第四轮 B5，供 AC-6 表驱动）：返回 `{label, order, sub}`；`triage` 0、`implement` 1、`implement-reentry` 1.5（「实现（新会话重入）」）、`fix-N` 1.75 且 `sub = N`（「修复第 N 轮」，N 按**数值**比较，`fix-2` 排在 `fix-10` 之前）、`verify` 2、`review` 3、`push` 4；不认识的 phase `{label: 原字符串, order: 9, sub: 0}`。`comparePhases` 先比 `order` 再比 `sub`，TaskDetail 的分组排序只用它 |
| `isFailure(ev)` / `severityOf(ev)` | 与 §3.2.2 同一份失败判定；配对后的 tool_use 以 `ev.result` 的判定为准 |
| `withTiming(list, origin)` | 给每条加 `offsetMs`（相对 origin：任务首条事件或阶段首条事件）和 `gapMs`（到下一条的间隔）；`gapMs < 1000` 时置 `null`（与 `TOOL_DURATION_FLOOR_MS` 同一理由，`TaskDetail.vue:167-176`） |
| `formatOffset(ms)` | `+00:42`、`+1:02:07`。v5 写死格式（第四轮 B5）：向下取整到秒；不足 1 小时为 `+MM:SS`（分钟两位补零），满 1 小时为 `+H:MM:SS`（小时不补零）；`ms` 为 `null`/非有限数/负数时返回 `''`（不显示，不伪造 `+00:00`） |
| `silenceLabel(gapMs)` | v5 新增（第四轮 B5：把「≥30s 插静默分隔线」的判定收成纯函数，可机器验证）：`gapMs` 为 `null` 或 `< SILENCE_GAP_MS`（30000）时返回 `null`（不插分隔线）；`≥ 30000` 时返回 `⋯ 静默 <时长>`，时长为 `XmYs`（`4m12s`）、不足 1 分钟为 `Ys`（`30s`）、满 1 小时为 `XhYm`（`1h05m`）。TaskDetail 只在它非空时渲染分隔线，自己不再比较阈值 |
| `applyFilter(list, {phases, onlyFailures, hideThinking, onlyTools})` | 本地过滤；subagent 子块里只要有一条命中就保留整个子块。v5 写死语义（第四轮 B5）：输入是 `pairTools(groupSubagents(...))` 之后的列表；四个条件取**交集**；`phases` 为空数组或缺省 = 不按阶段过滤；`onlyFailures` 用 `isFailure`/`severityOf`（error 与 warn 都保留）；`hideThinking` 去掉 `kind='thinking'`；`onlyTools` 只留 `tool_use`（含已配对的结果）与未配对的 `tool_result`。子块：对 `block.list` 逐条套同样的条件，**有一条命中就原样保留整个子块**（`list` 不裁剪，便于看上下文），一条都不命中则整块去掉；全部条件关闭时返回值逐项 `===` 输入 |
| `segmentRuns(verifications, stateEvents)` | **v2 改名并重写（B1），v3 改运行边界（第二轮 B1），v4 改运行内判定（第三轮 B1）**：先按「运行」切，再在每次运行内按「轮次」切，返回 `[{run, startedAt, cause, boundary, rounds:[{round, rows}]}]`（`boundary` 见规则 3 末尾），规则见下 |
| `parseLogRef(logRef)` | v4 新增：把 `log_ref`（可能是空格分隔的多段）解析为 `{round, seqMin, seqMax}`；任何一段不符合 `…/round-<N>/<NN>-<step>.log` 时返回 `null`（当作没有 `log_ref`，不猜） |
| `runCauseOf(stateEvents, startIdx)` | 一次运行的触发原因（规则 7 的映射表），供运行标题使用 |
| `proofOf(round)` | 一轮的红绿结论：`red`（repro_fail：passed=红已立 / failed=bug 没复现 / error=没跑起来）、`green`、`regression`、`light`，以及总判定。**只读 status，不重新判定**：总判定直接沿用 `Report.Passed` 的语义（`verify.go:110-122`），不发明新规则。v4：所在运行 `boundary === 'order'`，或本轮含 `placementUncertain` 行时，总判定为 `unknown`（界面不显示通过/已证明，只列各步 status）——行的归属是推断的，就不拿它下结论（铁律 1） |
| `createEventWindow({fetchPage, max})` | 事件窗口加载器（下面单独说明） |

**`segmentRuns`**（取代 v1 的 `segmentRounds`）：v1 「优先按 `logRef` 的 `round-N` 分组」在重试后是错的——
第一次运行的 round-0 和重试后的 round-0 拿到同一个 `round-0`（§2.1），会被并进一组，证明条把两次运行的
红、绿、回归混在一起。v2 的切分只用**顺序**做主依据，`round-N` 只当辅助信号：

1. 输入按 `(at, id)` 升序（与 `query.go:206` 的 `ORDER BY at, id` 一致）。
2. **运行边界 A：进入 `verifying`（v3 重写，第二轮 B1）**：`detail.events`（`task_events`）里每条
   `toState === 'verifying'` 的事件是一次新运行的起点；`at` 不早于它、早于下一条 `toState === 'verifying'` 的
   verifications 归入这次运行。依据见 §2.1「每次验证运行都恰好以一次 `to_state='verifying'` 开头」：
   这条规则**不枚举触发入口**，manual_retry、approve、PR review 重跑、重启恢复、rebase 跟进重验
   （`pr_open→review_feedback→implementing→verifying`，全程不经 `queued`）都由同一条规则切开。
   v2 的「`toState === 'queued'` 即新运行」删除：它漏掉 rebase 跟进重验，而且 approve 那条 `queued` 之后的运行
   根本不写 verifications。
3. **运行边界 B：`log_ref` 的 `(round, NN)` 回退（v3 起无条件生效；v4 改用 NN 前缀，第三轮 B1）**：在规则 2 切出的
   每段内，只看 `parseLogRef` 非空的行，每行与**上一条非空的行**比较：后一行 `round` **小于**前一行，或 `round`
   **相等**且后一行 `seqMin` **不大于**前一行 `seqMax`，即在此处切出一次新运行。依据（§2.1 v4 新增两行）：同一次
   运行内 round 号从 0 起只增（`pipeline.go:983`、`:1046`），同一 round 内 NN 严格递增，换一次运行 round-0 的 NN
   又从 `01` 起。v3 用的是「N 相等且 `stepOrder` 回退」，而同一轮里本来就有 `build,lint,build,lint`、
   `build(pnpm install),build` 这种序列（§2.1），会被切出假的「第 2 次运行」——v4 删除 `stepOrder`。
   **这条规则不会误切**：真实的单次运行里不存在 `(round, NN)` 回退；它只在 task_events 缺失或无法对齐时实际起作用。
   `log_ref` 为空的行（skipped、复现契约违例的 `repro_fail error`、落盘失败、T4 之前的旧行）不参与比较、不产生运行边界。
   每次运行带一个 `boundary` 字段说明边界凭什么定：`events`（规则 2 有对应的 `verifying` 事件）、`logRef`
   （没有事件，但这次运行里每条非 skipped 行都有可解析的 `log_ref`）、`order`（两者都没有）。`order` 时界面在
   运行标题旁标注「运行边界按顺序推断，可能把相邻两次运行并在一起」，且**不显示任何一轮的总判定**（只列各步 status）；
   这种运行里规则 4 切出的段落标题写「第 K 段（按顺序推断）」而不是「修复第 N 轮」，因为没有事件和 `log_ref` 时
   「上一次运行失败、重试又跑一遍」与「修复回路的下一轮」在 verifications 里长得一样，不能断言是哪一种。
   生产上 task_events 与状态转移同事务写入（`machine.go:1053`），`order` 只会出现在没有事件的老数据或测试 fixture 里。
4. **轮次边界（后切，在每次运行内）**：满足任一条件即开新一轮（v4 重写，第三轮 B1；删除 v3 的「步骤序号回退」）——
   a. `parseLogRef` 非空，且 `round` 与本轮已见的 `round` 不同（同一次运行内 round 号只增，`pipeline.go:983`、`:1046`）；
   b. 本行不是 skipped，且上一行不是 passed（failed/error/skipped）——一轮之内非 passed 之后只跟 skipped，
      而下一轮的第一条一定真正执行（§2.1 v4 新增第三行）；
   c. 本行是 light 步骤（build/lint/typecheck），上一行是 repro_fail/repro_pass/regression——一轮之内 light 段
      全部排在复现与回归之前（`heavy.go:254-316`）；
   d. `tier` 变化（`tier` 在一次 `stageVerify` 里定死，`pipeline.go:914-927`）。
   同名步骤重复出现（多个 Go 模块的 `build,lint,build,lint`、`pnpm install` 与 `pnpm run build` 两条 `build`、
   连续多条 `regression`）**不是**任何一条的触发条件。a–d 在真实的单次运行里都不会误切：每一条都是
   「只可能出现在轮次边界上」的信号，都有代码依据。b 单独就能找全同一次运行内的轮次边界：修复回路只在上一轮
   失败时才开下一轮（`pipeline.go:992-997`），所以每个轮次边界前一行都是 failed/error/skipped；a、c 是有 `log_ref`
   或跨档时的冗余信号。
   **漏切只剩一种情况**：没有 task_events、没有 `log_ref`、light 档，而且上一次运行全部 passed（a–c 都不触发；
   heavy 档全部 passed 时上一次运行以 `repro_pass`/`regression` 结尾，下一次运行以 light 步骤开头，c 会切开）。
   这时两次运行会并成一轮，规则 3 已把它标成 `boundary: order`，界面不给总判定（见上），不会凭并在一起的行
   宣布「通过」或「已证明」。
   有 `log_ref` 的运行里，`log_ref` 为空且不是 skipped 的行（落盘失败）先归入当前轮，并标 `placementUncertain`；
   含这种行的一轮**不显示总判定**，行上悬停说明「该步日志落盘失败，归属的轮次是按顺序推断的」。
   `repro_fail` 且 `status=error` 的空 `log_ref` 行除外：契约违例（没交复现测试）本来就不跑命令、不落日志
   （`heavy.go:271-282`），而且它只会紧跟本轮的 light 段之后。
5. 每行计算 `logOverwritten`：同一任务里存在 `id` 更大、`logRef` 片段集合与之有交集的行（与 §3.2.3 库内判定
   同一条规则）。被覆盖的行在证明条上照常显示 status（status 是库里的证据本体，未被覆盖），只是不提供
   「完整日志」按钮，悬停说明「该运行的日志已被第 N 次运行覆盖」。
6. **空运行不显示（v3 明确）**：切出来却没有任何 verification 行的运行（例如 approve 后 `EntryPush` 续跑，
   `stagePushAndPR` 经 `queued→verifying` 中转但不写验证；或验证前就失败的运行）不进入返回值，也不参与「第 K 次」编号。
   编号只数有验证行的运行。
7. **运行标题按触发原因写，不再一律写「由 {actor} 重试」（v3，第二轮 B1）**：触发原因取该运行起点
   （规则 2 的 `verifying` 事件）往前、上一条 `verifying` 事件之后的 task_events 里，**最后**一条 `payload.reason`
   属于下表已知取值（或带 `payload.unblocked_by`）的事件；区间里没有这样的事件时取起点事件本身的 actor 走兜底。
   只认表内取值，是因为区间里还会夹着别的带 `reason` 的转移（`awaiting_approval` 的说明文字 `pipeline.go:364`、
   `blocked_spec` 的 `repro_not_red` `:1063`），它们描述的是上一次运行怎么停下的，不是这一次为什么开始。映射表（`runCauseOf`，未知 reason 走兜底，不猜）：

   | 来源事件 | 标题文案 |
   |---|---|
   | 建单事件（`fromState === null`）或第一次运行 | 「首次运行」 |
   | `reason=manual_retry` | 「{actor} 手动重试（{payload.mode}）」 |
   | `reason=manual_review_retry` | 「{actor} 重来一轮 review」 |
   | `reason=pr_review` | 「PR review 意见回流后重跑」 |
   | `reason=restart_reconcile` | 「服务重启后恢复（中断于 {payload.interrupted_state}）」 |
   | `reason=rebase_followup_reverify` | 「前驱合并后 rebase 重验」 |
   | `payload.unblocked_by` | 「依赖任务 #{unblocked_by} 恢复后重跑」 |
   | 其他 | 「重新进入验证（{actor}）」 |

   `manual_approve` 不在表里：approve 之后的运行不写验证，按规则 6 不显示，不存在「由谁重试」的标题。
   完整标题为「第 K 次运行 · {原因} · {startedAt}」。只有一次运行时不显示运行这一层。
8. 视图：最新一次运行在最上面、默认展开；更早的运行折叠。

**`createEventWindow`**：取代「`appendCapped` + 终态即停」，是修复 2.2-1 的核心。

- **实时模式**（非终态）：每 2s 以 `after=lastId&paged=1` 拉取（`paged=1` 是让响应带上 `has_more` 的显式开关，
  §3.2.1 兼容契约；不带它时服务端只回 `{events, last_id}`）；如果一页填满（`has_more`），**同一 tick 内继续拉**，
  直到不满或拉满 5 页为止（防止一次积压把主线程卡住，剩下的留给下一个 tick）。
  窗口尾部保留最近 `max`（500）条，从头部裁掉的部分记为 `olderAvailable=true`
  （不再只显示一个丢弃计数）。
- **回放模式**（终态，或用户点了「加载更早」）：先 `before=∞`（即 `lastId+1`，来自 summary）拉**最后一页**，
  然后由「加载更早 200 条」按钮以 `before=firstId` 逐页向前加载。窗口硬上限 2000 条，
  超出时从**离视口较远的一端**裁掉，并标出这一端还可以再加载。
- **定位**：`jumpTo(id)`：如果 id 已经在窗口里，直接返回；否则以 `before=id+1, limit=100` 拉上文、
  `after=id, limit=100` 拉下文，用这两页**替换**整个窗口（不和旧窗口拼接，避免出现空洞），
  并把窗口标记为「已脱离实时」。界面提供「回到最新」按钮，点击后恢复实时尾部。
- 所有请求都经过注入的 `fetchPage`，可以在 node 里用假实现测试（AC-6）。

#### 3.3.2 `TaskDetail.vue` 执行日志面板

- **工具栏**（面板顶部，吸顶）：
  - 阶段 chip：`分诊 42 · 实现 912 ✗3 · 修复第 1 轮 210 ✗1 · 验证 18 ✗2 …`（条数和失败数来自 summary，
    不受内存窗口影响）；点击 chip 只看该阶段（多选），这时窗口改用 `phase=` 参数向服务端取数。
  - 开关：只看失败、隐藏思考、只看工具调用。
  - 时间显示：相对任务起点 / 相对阶段起点 / 绝对时间（三选一，存进 `localStorage`，读写都包 try/catch）。
  - 失败导航：`✗ 7 个失败  [第一个] [↑] [↓]`（个数取 summary 的 `failureCount`，「第一个」跳 `firstFailure`）；
    summary 的 `failuresTruncated` 为真时列表处显示「仅列前 200 个」，但 ↑/↓ 仍可走到第 200 个之后（按当前 id 向服务端取，§3.2.2）。
  - 「全部展开 / 全部折叠」（只作用于当前窗口内的工具输出和子块）。
- **每一行**：左侧一列 `+mm:ss`（`title` 显示绝对时间，并注明「入库时间，粒度约 ±200ms」）；
  如果与下一条的间隔 ≥ 30s（判定交给 `replay.js` 的 `silenceLabel`，§3.3.1），在两行之间插一条「⋯ 静默 4m12s」分隔线。这是回放时最直接回答「卡在哪」的信号。
- **阶段头**：显示阶段起止时间和时长（来自 summary 的 `firstAt/lastAt`）。
- **失败高亮**：失败行加左侧红条和浅红底（`--bad` 的透明色）；警告用 `--warn`。
  定位时自动展开所在的阶段 `<details>`、subagent 子块和工具输出，`scrollIntoView({block:'center'})`，
  闪烁高亮 1.5s，并把 URL 改为 `#ev-<id>`（`history.replaceState`，不新增历史条目）。
  页面加载时如果 URL 带 `#ev-<id>`，先 `jumpTo` 再滚动过去。
- **失败卡片联动**：`failureReason` 卡片（`TaskDetail.vue:356-380`）增加「在日志中定位」按钮，
  跳到**最后一个**失败事件（任务失败通常由最后一个失败导致），数据源是 summary 的 **`lastFailure`**（v5，第四轮 B4；
  不取 `failures` 数组末项，后者在超过 200 个失败时被截断）；`lastFailure === null` 时不显示按钮。
- **「更早的 N 条已省略」**（`:436-438`）改成可以点击的「加载更早的 200 条」，
  并显示「共 {summary.total} 条，已加载 {窗口条数} 条」。
- `activePhase` 和 `orderedGroups`（`:208-230`）的逻辑保留；分组改用 `replay.js` 的 `phaseMeta`。

#### 3.3.3 `AgentEventItem.vue`

- 工具输出和 thinking 的 `<details>` 改为**受控展开**：`open` 时才渲染 `<pre>`（`v-if`），
  折叠时 DOM 里只有 summary 行（修复 2.2-6）。展开状态由父组件按事件 id 保存（一个 `Set`），
  这样「全部展开」和「定位」可以统一控制。
- 超过 40 行的输出先显示**前 40 行**，下面放「显示全部 N 行」按钮。
- `body` 末尾带 `…(已截断)`（`agent.go:513`）或 `…（中间省略 N 字节）…`（`capbuf.go:126`）时，
  在输出底部注明「事件流里只保留了 4KB 摘要」。verify_step 额外提供「查看完整日志」，
  打开验证区对应那一步的日志查看器（§3.3.4）。
- **渲染安全**：所有日志和输出一律用 `{{ }}` 插值，**禁止** `v-html`。只有 `kind='text'` 和 `result`
  正文沿用现有的 marked + DOMPurify（`AgentEventItem.vue:16-33`）。
- 新增 `props.timing`（`offsetMs`、`gapMs`）和 `props.failed`，用于渲染时间列和高亮。
  未知 kind 仍然走最后的兜底分支（`:131`），兼容 f06（codex 事件）。f01 插话不进事件流（§3.7），与本组件无关。

#### 3.3.4 验证区：红绿证明条 + 日志查看器

把平表（`TaskDetail.vue:480-503`）改成**按运行、再按轮次分块**的视图（`segmentRuns`，§3.3.1）。
最新一次运行、最新一轮在最上面，默认展开；更早的折叠：

```
第 2 轮（修复第 1 轮之后）  heavy  ✓ 已证明
  [构建 ✓] [lint ✓] → [红：改动前确实失败 ✓] → [绿：改动后通过 ✓] → [回归 ✓]
第 1 轮  heavy  ✗ 未通过
  [构建 ✓] [lint ✓] → [红 ✓] → [绿 ✗ 改动后仍失败]   ← 失败步骤加粗加红框
```

- 文案直接说明含义：repro_fail 为 `passed` 显示「红：改动前确实失败 ✓」，`failed` 显示
  「红未立：改动前测试已通过（bug 没复现）」，`error` 显示「红阶段没跑起来」。
  **不改**任何 status 的取值与判定，只改展示文字。
- 每一步可以点开「完整日志」：调用 §3.2.3 的接口，以 `<pre>` 分块追加显示，提供「加载更多」；
  多段的复现日志用 `part` 切换。`logRef` 为空的步骤不显示这个按钮（不伪造「无日志」的入口）。
- 证明条**只读 `detail.verifications`**，不从 `agent_events` 推断。原因：`verifications` 是可审计的
  证据本体（`internal/store/verifications.go:8-14`），而事件流可能因溢出丢事件（`batchsink.go:24-27`）。

### 3.4 配置项与消费方

**不新增任何环境变量**，README 变量表不变。

| 新增的东西 | 消费方 |
|---|---|
| `httpapi.API.LogDir string` | `verificationLog` handler，用来拼日志路径。赋值 `cfg.DataDir`（`DataDir` 新增的第二个消费方，与 `Pipeline.LogDir` 同源，`main.go:580`） |
| `httpapi.API.KnownSecrets []string` | `verificationLog` handler 脱敏与安全切点检验。赋值 `cfg.KnownSecrets()`（与 `Pipeline.KnownSecrets` 同源，`main.go:587`） |
| 代码常量 `eventsPageMax=500`（沿用）、`logChunkDefault=64KiB`、`logChunkMax=256KiB`、`logLineMax=1MiB`、`redactContextMin=16KiB`、`maxCutProbes=256`、`logMtimeSkew=5s`、`summaryFailuresMax=200` | 各自的 handler |
| 前端常量 `MAX_LIVE_EVENTS=500`（沿用）、`REPLAY_MAX_EVENTS=2000`、`SILENCE_GAP_MS=30000` | `replay.js` |

这些上限是产品常量，不对外开放配置（每个可配置字段都必须有消费方和非法值校验，
而这里没有任何一个已知的调参需求）。

**装配点（v2 明确，B4）**：`cmd/lathe/main.go` 的 `serve()` 里，`apiSrv := &httpapi.API{...}` 字面量
（base 上是 `main.go:280-286`，现有 5 个字段 `Store`、`Tasks`、`Queue`、`Auth`、`ConfigStatus`，
**没有** `LogDir` 和 `KnownSecrets`）。在 `ConfigStatus: configStatus(cfg),`（`:285`）之后、收尾的 `}`（`:286`）
之前追加两行，并带一行注释说明与 Pipeline 同源：

```go
	apiSrv := &httpapi.API{
		Store:        st,
		Tasks:        task.NewMachine(st.Pool()),
		Queue:        q,
		Auth:         auth,
		ConfigStatus: configStatus(cfg),
		// 验证日志读取（f04）：与 Pipeline.LogDir / KnownSecrets 同源（buildPipeline 里的同名字段），
		// 漏装时日志接口一律 503、静默不可用 —— TestServeWiresVerificationLogIntoAPI 守着。
		LogDir:       cfg.DataDir,
		KnownSecrets: cfg.KnownSecrets(),
	}
```

不放到 `:371` 那种「字面量之后再补赋值」（`apiSrv.Baselines = previewMgr`）的写法：两个值在 `serve()`
入口就已就绪，没有依赖顺序，放进字面量可以让装配守卫只检查一处。守卫见 AC-8。

### 3.5 安全

1. **非属主 404**：三个接口都先过 `ownedTask`；store 层查询仍然 `JOIN tasks ... user_id`；
   日志接口额外校验 `verifications.task_id = {id}`。AC-3 和 AC-5 断言非属主拿到的状态码是 404，
   并且响应体与「任务不存在」完全一致。
2. **路径穿越**：§3.2.3 的四道校验。
2b. **证据归属（铁律 1）**：日志被后一次运行覆盖时返回 404 `log_overwritten`，不把别的运行的输出挂到这条
   verification 上（§3.2.3 三道判定）；证明条按「运行 → 轮次」切分，不把两次运行合成一轮（§3.3.1）。AC-5 往库里写入 `log_ref='../../../etc/passwd'`、
   绝对路径、以及指向 `LogDir` 之外的符号链接，三种情况都必须 404。
3. **凭据**：日志块统一经过 `RedactForDisplay`，块边界只落在安全切点上（§3.2.3），`offset` 不是安全切点 → 400，
   找不到安全切点 → 整段不返回（fail-closed）。AC-5 在日志里写入 `cfg.KnownSecrets` 的值和一个
   `postgres://u:p4ss@h/db`，并覆盖凭据横跨 64KiB 标称边界、凭据所在行超过 4KiB、跨行形态三类切分场景，
   断言响应里明文都不出现。事件流本身的内容不变
   （它的脱敏状态与现在相同，不在本 PRD 范围内，§4）。
4. **XSS**：日志和工具输出不经过 markdown 渲染。AC-7 用 `grep -rn --include='*.vue' --include='*.js' --exclude='*.test.js' --exclude-dir=testdata v-html web/src` 的**全量白名单**守住
   「`v-html` 只出现在已登记的 markdown 渲染点」，本线新建的任何组件（含日志查看器）出现 `v-html` 都会让守卫失败。
5. **DoS**：`limit` 有上限；`phase`/`kind` 白名单校验；summary 是单任务范围的聚合；
   日志读取用 `io.NewSectionReader`，按块读，不整文件读进内存。
6. **不依赖 Host 头**：新增功能不生成任何外部链接；深链接用的是 URL hash，只在前端生效。

### 3.6 并发

- 三个接口都只读、无锁、无写回。日志文件可能还在被验证步骤写入：读取时先 `Stat` 拿 `size` 快照，
  按快照读，`eof` 语义如实说明（§3.2.3）。
- 前端：`createPoller` 的「上一轮没回来就不发下一轮」保留（`livelog.js:56-81`）。
  `createEventWindow` 内部的请求串行执行：`jumpTo`、`loadOlder` 和实时 tick 共用一个 in-flight 标记，
  后发起的请求等前一个完成。窗口被 `jumpTo` 替换后，旧 tick 晚到的响应按 generation 计数丢弃
  （AC-6 有用例覆盖）。
- 不涉及 `task`/`runner`/`flow`/`cmd` 的并发逻辑，所以 `make test-race` 不是必跑项
  （AGENTS.md §3）。改动只在 `httpapi` 和 `store` 的只读路径上，额外跑一次
  `go test -race -p 1 ./internal/httpapi/...` 作为证据。

### 3.7 与并行功能的边界（取最小改动面）

| 邻居 | 重叠点 | 本线做法 |
|---|---|---|
| **f01 插话** | f01 R1 起**只**在 `TaskDetail.vue` 加 2 行挂独立的 `InterjectPanel`：① `<script setup>` import 区（base `:5` `import AgentEventItem …` 之后）一行 `import InterjectPanel …`；② 模板里执行日志卡片**之前**（base `:429` `<!-- 执行日志面板 …` 注释之前）一行 `<InterjectPanel … />`。f01 **不碰** `/events`、不改 `AgentEventItem.vue`、不产生 `ev-interject` 行、不往时间线合并任何东西（`f01-interject.md:290`、`:458-467`、`:655-659`、`:731`） | 本线**不为插话做任何事**：`/events` 的扩展只是本线自己的加法，与 f01 无关；`replay.js` 不含插话逻辑。冲突面只有 `TaskDetail.vue` 两处相邻行，解法：**(a) import 区**——本线新增的 `import … from '../replay'` 放在 `:7` `import { appendCapped, … } from '../livelog'` **之后**（不插在 `:5` 与 `:6` 之间），与 f01 插在 `:5` 之后的那一行不相邻，git 通常能自动合并；若本线因去掉 `appendCapped` 等改动了 `:7` 这一行而与 f01 形成相邻冲突，后合入方两边都保留即可（f01 的 import 行 + 本线改后的 livelog/replay 行），无语义冲突。**(b) 执行日志卡片之前**——本线只改卡片**内部**（`<div class="card">` 之后的工具栏、列表），**不改** `:429` 那行注释、也不在卡片之前插兄弟节点；如因注释文案更新产生冲突，保留 f01 的 `<InterjectPanel>` 行在注释**之前**、本线的卡片内容在注释之后。两种冲突都只是文本相邻，合并顺序无关；PR 描述里点名这两处。**(c) `/events` 响应形状与 `TestTaskEventsEndpoint`（v5，第四轮 B1）**——f01 AC-17 在既有 `TestTaskEventsEndpoint`（`internal/httpapi/events_test.go:14`）里加断言「不带新参数时响应顶层键集合恰为 `{events, last_id}`」（`f01-interject.md:1086-1089`）。本线**接受这条断言原样成立**：§3.2.1 的兼容契约规定不带新参数时响应就是 `{events, last_id}`，`first_id`/`has_more` 只在带 `paged`/`before`/`phase`/`kind`/`failures` 时出现，前端实时模式显式带 `paged=1`。所以 f01 先合入时本线 rebase 后 `TestTaskEventsEndpoint` 照样绿、本线不改 f01 加的那行；本线先合入时 f01 加的断言在 dev 上同样成立。本线**不修改** `TestTaskEventsEndpoint`，新增的形状测试另起函数（AC-2），避免与 f01 在同一函数里产生文本冲突。f01 无需改动其 PRD |
| **f02 收件箱** | f02 明确把 TaskDetail 留给 f04（`f02-inbox.md:404`、`:456`） | 本线不做「打开详情页即标已读」 |
| **f03 用量统计** | 都读 `agent_events` | 本线不改任何采集逻辑，不显示 token；失败判定只读现有 payload 字段。f03 的 `agent_runs` 与本线无关 |
| **f05 PAT** | 新接口的鉴权 | 只用 `a.Auth.RequireFunc`，f05 扩展鉴权后这些接口自动支持 PAT |
| **f06 多 CLI** | codex 事件可能带来新 kind 或 payload | 失败判定只认现有的 4 种形状；未知形状不会被判成失败（宁可漏判，也不误报），走兜底渲染 |
| **f12 团队与角色** | 归属判定 | 所有新接口都经过同一个 `ownedTask` helper；本线把现有 `taskEvents` 里内联的 `tk.UserID != CurrentUser(r).ID`（`api.go:149-154`）也改成调用它。与 f12 的分工按合入顺序写死（与 `f12-team-roles.md` §3.11 f04 行一致）：**(a) 本线先合**——f12 rebase 时只改 `ownedTask` 的函数体（属主 ∪ 团队成员），不在 `taskEvents` 里重新内联、不另起 helper；**(b) f12 先合**——f12 已把 `taskEvents` 的内联判定抽成同名同签名的 `ownedTask`（团队可见语义），本线 rebase 时**复用它、不改它的函数体**（照搬属主判定改写会让 f12 的 AC-9 回退），新接口直接调它，并把 summary 与日志两条路由补进 f12 的 `taskReadRoutes` 表（f12 AC-54）。两种顺序下 store 查询都传 `tk.UserID`。**PR 描述里点名 `ownedTask`** |

---

## 4. 范围外

| 不做 | 归属 / 理由 |
|---|---|
| 改事件采集：记录事件的**发生时刻**（以消除 ±200ms 误差）、给 verifications 加 `round` 列、改 `Digest` 的提炼形状 | 需求明说不改采集口径。时间粒度在界面上如实标注；轮次靠 `logRef` 和顺序推断（§6 记录了这种推断的脆弱性） |
| 补回 0041 之前已经丢掉的 `implement-reentry` 事件 | 数据已经丢失，无法恢复 |
| 让重试不再覆盖旧日志（日志目录按运行区分，例如 `task-<id>/run-<k>/round-<n>/`，或 `verifications` 加 `run`/`round` 列） | 这是写入侧改动（`verifylog.go`/`pipeline.go`，AC-8 守住不改），并且会和 f03 等改流水线的线冲突。本线在读取侧保证**不挂错**（覆盖判定 + 按运行切分），代价是被覆盖的旧日志在界面上不可读（§6）。根治建议另开 `fix/verify-log-per-run`，本 PRD 的覆盖判定在根治后自然不再触发，无需回改 |
| 读取被覆盖、或无法安全切分的日志原文 | 前者已不存在于磁盘；后者原文在服务器 `<DataDir>/<log_ref>`（0600），排障走服务器，不在界面上冒泄露风险 |
| SSE 推送替代轮询 | `docs/04` §3.3 已经说明暂缓；游标协议不变，以后可以直接升级 |
| 时间轴拖动（time travel）、节点图 | `docs/04` §7.6 的结论不变：没人坐在旁边拖进度条。本 PRD 做的是「定位」，不是「播放」 |
| 事件流本身的脱敏改造 | 事件流现在的脱敏状态不在本线范围内；本线只保证**新开放**的日志读取接口经过脱敏 |
| tool_result 完整原文（超过 4KB 的部分） | 采集时已经截断，库里不存在。界面如实标注「只保留了 4KB 摘要」 |
| 插话（f01）、用量（f03）、收件箱已读（f02）、PAT（f05）、codex 事件（f06）、团队可见性（f12） | 见 §3.7 |
| 前端测试框架（vitest 等） | 不引入依赖；纯函数用 Node 内置的 `node --test` |
| PRD 会话页（`/prds`）的事件流 | 那是 `prd_events`，另一条链路；本线只改任务详情页 |

---

## 5. 验收标准

约定：Go 测试统一用 `make test`（`-p 1`）；「先红」指在**未改实现**的提交上运行同一条命令，必须失败，
并把失败输出存进 `/opt/lathe-wt/evidence/f04/red/`；「后绿」的输出存进 `/opt/lathe-wt/evidence/f04/green/`。
每条用例名都写成「给定什么、断言什么」，失败信息带 got/want（AGENTS.md §5）。

**AC-1 `implement-reentry` 事件可以落库（0041）**
- 测试 `internal/store/agents_test.go::TestInsertAgentEventsAcceptsEveryPipelinePhase`：表驱动，
  覆盖流水线会写的每一个 phase：`triage`、`implement`、`implement-reentry`、`verify`、`push`、`fix-1`、`fix-12`、`review`。
  断言 `InsertAgentEvents` 不报错，并且可以读回。
  **这里只能写字符串字面量**（v2 统一说法，B4）：`internal/runner` 的非测试代码 import 了 `internal/store`
  （`runner/memory.go:55`、`runner/mergepoll.go:12`），store 包内测试再 import runner 会形成 import cycle，编译不过。
  字面量旁注释来源 `runner.PhaseImplementReentry`（`internal/runner/eventsink.go:35`）。
- **字面量与常量的绑定**由另一条测试负责，放在已经 import runner 的 `httpapi` 包里（`api.go:16`）：
  `internal/httpapi/events_test.go::TestEventsPhaseFilterAcceptsRunnerReentryPhase`——用
  `Store.InsertAgentEvents(ctx, id, runner.PhaseImplementReentry, ...)` 插入一条事件，再请求
  `/events?phase=` + `runner.PhaseImplementReentry`，断言 200 且读回这一条。常量哪天改名或改值，
  这条测试和 §3.2.1 的 phase 白名单会一起报错，不会出现「store 测试写死的字面量过时了而全绿」。
  这条测试不改 `internal/runner/` 的任何文件，与 AC-8 的边界一致。
- **先红**：在 base（未加 0041）上运行两条测试：store 测试的 `implement-reentry` 用例报 SQLSTATE 23514
  （已经在 `lathe_f04` 上手工证实，§2.2-2）；httpapi 测试插入失败。输出存 `evidence/f04/red/ac1.txt`。
- 迁移：`lathe_migrate up`、`lathe_migrate down && lathe_migrate up` 都退出码 0；
  `TestLoadMigrations` 通过（up/down 成对）。down 之后用
  `pg_get_constraintdef` 查到的约束与 0015 定义一致。
- **执行顺序（v3，第二轮 B2）**：0041 的 down 有意在库里存在 `implement-reentry` 行时失败（§3.1、§6），
  而本 AC 的 store/httpapi 测试与 AC-9 的种子数据都写进同一个 `lathe_f04`；`t.Cleanup` 在进程被杀时不执行
  （AGENTS.md §5.5），所以不能指望「测试会自己清干净」。本线固定顺序：
  1. **先**在尚未写入任何本线测试数据的库上跑 down/up 检查，证据存 `evidence/f04/green/ac1-migrate.txt`：
     ```bash
     psql "$LATHE_TEST_DSN" -Atc "SELECT count(*) FROM agent_events WHERE phase='implement-reentry'" # 必须为 0，非 0 先走第 3 步
     lathe_migrate up && lathe_migrate down && lathe_migrate up; echo "exit=$?"
     psql "$LATHE_TEST_DSN" -Atc "SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname='agent_events_phase_check'"
     ```
     第一条查询的输出一并写进证据文件，证明 down 是在空数据上跑过的。
  2. **再**跑 `make test`（含 AC-1 的两条测试）与 AC-9 造数据。
  3. **残留处理**：AC-1 两条测试插入的任务带随机标识（`external_key` 形如 `f04-ac1-<rand>`），并在 `t.Cleanup` 里按
     **自己创建的 task_id** 删除（`DELETE FROM tasks WHERE id = $1`，`agent_events` 随 `ON DELETE CASCADE` 一起删，
     `migrations/0005_agent_events.up.sql:29`）；断言按 task_id 过滤，不统计全库。若进程被杀留下残留，
     先用 `make clean-test-db`（干跑）查看，再按 AC-9 末尾的按 ID 清理办法处理，**不**用 `DELETE … WHERE phase=…` 全表删。
  4. 若需要在造过数据之后**再次**验证 down/up（例如修改了 0041），必须先完成 AC-9 的清理并重跑第 1 步的计数查询为 0。
  这条顺序在 PR 的验证说明里原样列出。

**AC-2 `/events` 反向分页与兼容性**
- 测试 `internal/httpapi/events_test.go::TestTaskEventsBeforePaging`：插入 250 条事件；
  `before=<lastId+1>&limit=100` 返回 id 最大的 100 条，升序，`has_more=true`，`first_id`/`last_id` 正确；
  沿 `before=first_id` 连续翻 3 页，并集正好等于 250 条，没有重复、没有遗漏，最后一页 `has_more=false`。
  `before` 和 `after` 同时传 → 400。
- 测试 `TestTaskEventsLegacyShapeUnchanged`（v5 改写，第四轮 B1）：表驱动，用固定数据：
  1. 「不带任何新参数」：`?after=0`、`?after=<last_id>`、`?after=0&limit=1`、无查询串四种请求，断言 `events`、`last_id`
     的值与改动前逐字段一致，并且**响应顶层键集合恰为 `{events, last_id}`**（got 打印实际键集合）——与 f01 AC-17 在
     `TestTaskEventsEndpoint` 里加的断言同一条契约；
  2. 「带 `paged=1`」：`?after=0&paged=1` 的 `events`、`last_id` 与用例 1 的 `?after=0` 逐字段相同，键集合恰为
     `{events, last_id, first_id, has_more}`；`?after=<last_id>&paged=1` → `first_id == null`、`has_more == false`；
  3. 「`paged=2`、`failures=yes`」→ 400。
  本测试是新函数，**不修改**既有 `TestTaskEventsEndpoint`（f01 会往那里加断言，§3.7 f01 行 (c)）。
  用例 1 是回归守卫（base 上就绿，不要求先红）；用例 2、3 在 base 上先红（无 `first_id` 键 / 返回 200）。
- 测试 `TestTaskEventsFailuresParam`（v5 新增，第四轮 B4）：插 3 条失败 + 若干非失败 + 1 条 overflow 警告（id 最大）；
  `?after=0&paged=1&failures=1&limit=1` 返回第 1 条失败，以它的 id 为 `after` 再拉得第 2、3 条、第 4 次得到警告、第 5 次为空；
  `before=<第 3 条失败 id>&failures=1&limit=1` 返回第 2 条失败；非失败事件一条都不出现。
- **先红**：改动前 `before` 参数被忽略、从头返回，第一个断言（返回最大的 100 条）失败；`TestTaskEventsFailuresParam`
  在 base 上 `failures=1` 被忽略、返回非失败事件而失败。

**AC-3 `/events` 过滤与隔离**
- 测试 `TestTaskEventsPhaseKindFilter`：混合插入 3 个 phase、4 种 kind；`phase=verify` 只返回 verify 的事件；
  `phase=implement,fix-1&kind=tool_use` 取交集；`phase=bogus` 和 `kind=bogus` → 400，并且错误信息里带参数名。
- 测试 `TestTaskEventsOtherUserGets404`（回归守卫，不要求先红）：另一个用户的会话请求
  `/events?before=...&phase=...` → 404，响应体为 `{"error":"任务不存在"}`。
- **先红**：改动前过滤参数被忽略，`phase=verify` 用例会返回其他阶段的事件。

**AC-4 `/events/summary`**
- 测试 `TestTaskEventsSummary`：构造的数据包含：implement 阶段 2 个 isError 的 tool_result、
  1 个成功的 tool_result；verify 阶段 status 分别为 failed/error/passed/skipped 的 verify_step 各 1 条；
  1 条 `subtype=error_max_turns` 的 result；1 条 overflow raw。断言 `failures` 恰好是 5 条失败加 1 条警告，
  按 id 升序；各阶段的 `count`、`failures`、`firstId`、`lastId` 正确；`total` 正确；`failureCount == 5`；
  `firstFailure.id`/`lastFailure.id` 分别等于 5 条 error 里 id 最小/最大的那条（overflow 警告即使 id 最大也**不是** `lastFailure`）。
  另起一个任务插入 205 条失败（夹着非失败事件，最后再插 1 条 overflow 警告；v5 改写，第四轮 B4）：断言 `failures` 返回 200 条且
  `failuresTruncated=true`；`failureCount == 205`；
  **`lastFailure.id` 等于库里该任务 severity=error 事件的 `max(id)`**（用一条独立 SQL 算 want），并且
  `lastFailure.id > failures[199].id`（证明它不是从截断数组取的末项）；无失败的任务 `lastFailure == null`。
- 非属主 → 404；未认证 → 401（加入 `TestAPIRequiresAuth` 的端点表）。
- 测试 `TestEventsQueriesUseTaskIndex`：对 `AgentEventsBefore`（带 phase 过滤）和 summary 的 SQL 执行
  `EXPLAIN (FORMAT JSON)`（先 `SET enable_seqscan=off` 以排除小表统计带来的干扰），
  断言计划里出现 `agent_events_task_id`。
- **先红**：路由不存在，404（mux 返回纯文本），断言失败。

**AC-5 验证日志读取：归属、穿越、脱敏、分块、重试覆盖**

实现按三个提交推进，后两个提交的测试在**前一个提交的真实代码**上先跑一次（不写模拟实现）：
提交 A = 路由 + 归属 + 路径校验 + 定长分块逐块脱敏；提交 B = 安全切点（§3.2.3「分块与脱敏边界」）；
提交 C = 重试覆盖判定。每次先红都记录所在提交的 hash。

- 测试 `internal/httpapi/verification_log_test.go`，`API.LogDir` 指向 `t.TempDir()`：
  - **正常**：写入 `verify-logs/task-<id>/round-0/01-build.log`（300KiB，含多字节中文），
    `limit=65536` 沿 `nextOffset` 分块读到 `eof`：每一块都是合法 UTF-8（`utf8.ValidString`）；除最后一块外，
    每块的 `nextOffset` 处的前一个字节是 `\n`；所有块的 `text` 拼起来**逐字节等于**
    `runner.RedactForDisplay(整份文件, KnownSecrets)`。
  - **多段**（v5 改写，第四轮 B3：`log_ref` 用真实格式，否则会先被路径校验第 2 条拒成 404）：写
    `verify-logs/task-<id>/round-0/09-repro_fail.log`（内容 `PART-A`）与 `verify-logs/task-<id>/round-0/10-repro_fail.log`
    （内容 `PART-B`），`log_ref` 为 `"verify-logs/task-<id>/round-0/09-repro_fail.log verify-logs/task-<id>/round-0/10-repro_fail.log"`
    （与 `heavy.go:349-378` 的 `strings.Join(refs, " ")` 一致，每段都是完整的相对 DataDir 路径）。断言 `part=0` 的 `text` 为
    `PART-A`、`part=1` 为 `PART-B`，`parts == 2`；`part=2` → 404。另加一条路径校验的负例：`log_ref` 为 `"a.log b.log"`
    （不在 `verify-logs/task-<id>/` 之下）→ `part=0`、`part=1` 都是 404——守住「多段的每一段都单独过四道路径校验」。
  - **脱敏（基本）**：日志里含 `API.KnownSecrets` 中的值和 `postgres://u:p4ss@h/db`，响应中 `strings.Contains`
    这两个明文都必须为 false。
  - **脱敏（切分，v2 新增，B2）**——表驱动 `TestVerificationLogChunkNeverSplitsSecret`，每个用例都断言：
    拼接结果里不含凭据明文；**任何一块**里都不含凭据的前半截 `sec[:len(sec)/2]`、后半截 `sec[len(sec)/2:]`
    （半截长度都 ≥ 8，避免和普通文本撞车）；拼接结果逐字节等于整份脱敏的结果。
    1. 「登记凭据横跨 64KiB 标称边界」：`KnownSecrets` 里的 DSN 从字节 65520 开始，所在行 200 字节；
    2. 「凭据所在行超过 4KiB」：一行 10KiB 的单行 JSON（`{"a":"…","token":"<32 字节随机串>","b":"…"}`），
       `token` 值横跨 65536；
    3. 「裸值 kv 横跨边界」：`API_KEY=<32 字节随机串>` 横跨 65536（基线探针证实切开后后半截会明文留下）；
    4. 「跨行形态，换行正好在边界后」：`password:\n  <16 字节随机串>`，其中 `\n` 位于 65537；
       断言第一块**不**在这个换行处结束（v1 的换行扩展会在这里切开，探针证实明文留下）；
    5. 「超过 1MiB 的单行」：一行 1.5MiB、中间带一个登记凭据，从它所在块开始读 → `withheld=true`、`text==""`、
       `nextOffset==null`，响应体中不含这一行的任何 64 字节片段；
    6. 「非安全切点的 offset」：`offset` 指向凭据中间 → 400，响应体只含错误信息。
  - **重试覆盖（v2 新增，B1）**——`TestVerificationLogRejectsLogOverwrittenByLaterRun`，表驱动（v5，第四轮 B3：下文 `<id>` 为
    本用例任务 id，所有 `log_ref` 都写完整路径 `verify-logs/task-<id>/round-<n>/<NN>-<step>.log`，与路径校验第 2 条一致）：
    1. 「后一次运行已插行」：写 `verify-logs/task-<id>/round-0/01-build.log` 内容 `RUN-1`，插入 vid1（`log_ref` 为
       `verify-logs/task-<id>/round-0/01-build.log`）；
       再以 `RUN-2` 覆盖同一文件、插入 vid2（同一 `log_ref`）。断言 GET vid1 → 404，`code=="log_overwritten"`，
       body 里不含 `RUN-2`；GET vid2 → 200 且 `text` 为 `RUN-2`。
    2. 「后一次运行正在写、还没插行」：只有 vid1，用 `os.Chtimes` 把文件 `ModTime` 设为 `vid1.at + 10s` → 404 `log_overwritten`；
    3. 「容差内」：`ModTime` 设为 `vid1.at + 2s` → 200（不误杀自己写的文件）；
    4. 「复现阶段多段只覆盖其中一段」：vid1 的 `log_ref` 为
       `"verify-logs/task-<id>/round-0/01-repro_fail.log verify-logs/task-<id>/round-0/02-repro_fail.log"`，
       vid2 的 `log_ref` 只声明 `verify-logs/task-<id>/round-0/01-repro_fail.log` → vid1 的 `part=0` 为 404 `log_overwritten`，
       `part=1` 为 200（两个文件的 `ModTime` 都用 `os.Chtimes` 设为 `vid1.at - 1s`，排除时间判定的干扰，确保 `part=0` 的 404
       来自库内判定）；
    5. 「非属主看不到 log_overwritten」：另一个用户请求情形 1 的 vid1 → 404 且 body 为 `{"error":"任务不存在"}`。
  - **穿越**：`log_ref` 分别为 `../../../etc/passwd`、`/etc/passwd`、`verify-logs/task-<另一个任务>/x.log`、
    以及一个指向 TempDir 之外文件的符号链接 → 全部 404，并且响应体里不含目标文件的任何内容。
  - **归属**：另一个用户 → 404；`vid` 属于另一个任务 → 404；`log_ref IS NULL` → 404；
    `LogDir=""` → 503。
  - **上限**：`limit=10000000` 时 `nextOffset - offset ≤ 256KiB + 1MiB`。
- `internal/runner/redact_test.go::TestRedactForDisplayMatchesRedactText`：表驱动，断言
  `RedactForDisplay(s, reg) == redactText(s, reg, nil)`，覆盖 DSN、kv、空串。
- **先红**（命令都是 `make test` 中对应的 `go test -p 1 -count=1 -run <Name> ./internal/httpapi/`，输出存 `evidence/f04/red/`）：
  - 基础用例在 base 上：路由不存在，全部失败（`ac5-base.txt`）；
  - 切分用例在提交 A 上：用例 1–4 至少一条报出半截凭据（got 含 `<half>`，want 不含），用例 5、6 状态码不符（`ac5-commitA.txt`）；
  - 重试覆盖用例在提交 B 上：用例 1、2、4 返回 200 并带出 `RUN-2`（got 200，want 404）（`ac5-commitB.txt`）。
  三个文件头部各写明所在提交的 hash 与命令；后绿在 PR 头提交上跑同一条命令。

**AC-6 前端纯函数与窗口加载器（`node --test`，不引入依赖）**
- 新文件 `web/src/replay.test.js`，用 `node:test` + `node:assert/strict`。
  运行命令：`node --test web/src/replay.test.js`（本机 Node v22.22.0 已实测这种写法可用，
  探针在 `/tmp/f04nodetest`）。新增 `make ui-test` 目标执行这条命令。**不改 `web/package.json`。**
- **提交顺序（为了先红能在真实旧代码上跑，B3）**：
  - 提交 1「抽取」：把 `TaskDetail.vue` 现有的加载逻辑**原样**搬进 `web/src/replay.js` 的
    `createEventWindow({fetchPage, max, isTerminal})`——行为与 base 完全一致：`after=lastId` 拉一页、
    `appendCapped` 封顶 500、终态拉完这一 tick 即停（`TaskDetail.vue:76-86`、`:247-261`）；同时把
    `groupSubagents`/`pairTools` 原样搬过去。TaskDetail 改为调用它，模板不动。`make ui` 通过。
    这个提交是纯重构，界面行为不变（`pairTools`/`groupSubagents` 快照用例在这个提交上就绿）。
  - 提交 2「回放窗口」：在同一个函数上实现 §3.3.1 的实时/回放/定位三种模式。
  - 测试文件在提交 2 写成，**同一份文件**先在提交 1 的 `replay.js` 上跑（在本线 worktree 里就地换回旧实现，跑完还原）：
    ```bash
    git checkout <提交1> -- web/src/replay.js
    node --test web/src/replay.test.js > /opt/lathe-wt/evidence/f04/red/ac6-commit1.txt 2>&1; echo "exit=$?" >> /opt/lathe-wt/evidence/f04/red/ac6-commit1.txt
    git checkout HEAD -- web/src/replay.js
    ```
    要求 `exit` 非 0；文件头另写提交 hash。`replay.js` 保持纯 ESM、**不 import 任何包或 `livelog.js`**
    （`MAX_LIVE_EVENTS` 作为参数传入），这样 node 能直接加载，换回旧实现时也不会因为依赖解析失败而
    「假红」——失败输出必须是断言失败或 `is not a function`，不能是 `ERR_MODULE_NOT_FOUND`。
    v1 里的 `legacyLoad` 对照用例与「改成 `assert.equal(covered, 400)`」的做法**删除**。
- 用例（表驱动）：
  - `isFailure`：与 AC-4 **同一张**失败判定表（以 JSON fixture `web/src/testdata/failure-cases.json`
    同时被 Go 测试和 node 测试读取，保证前后端判定一致）。
  - `pairTools` 和 `groupSubagents`：用一组固定事件，断言输出与快照逐字段一致（快照在提交 1 上生成并提交）。
  - `withTiming`：`gapMs` 低于 1000 时为 null；`offsetMs` 相对 origin 正确。
  - 以下四组是 v5 新增（第四轮 B5：§1.2 目标 2、3 原来在 AC 里测不到），全部表驱动、用例名写「给定什么、断言什么」：
    - **`applyFilter`**（输入一律先过 `pairTools(groupSubagents(...))`，fixture 含：主流的 text、thinking、成功的 tool_use+result、
      isError 的 tool_use+result、未配对的 tool_result、overflow raw；一个 subagent 子块 S1 内含 1 条 isError 的 tool_use+result 与 2 条 text；
      另一个子块 S2 只有 text 与 thinking；事件分属 implement 与 fix-1 两个阶段）：
      1. 「全部关闭」→ 返回值长度与输入相同，逐项 `===`；
      2. 「只看失败」→ 主流只剩 isError 的 tool_use 与 overflow raw；S1 **整块保留且 `list.length === 3`**（不裁剪）；S2 被去掉；
      3. 「隐藏思考」→ 主流与子块外层都不再有 `kind='thinking'` 的顶层项；S2 因为还有 text 而保留；
      4. 「只看工具调用」→ 主流只剩两个 tool_use 与那条未配对的 tool_result；S1 保留、S2 去掉；
      5. 「`phases=['fix-1']`」→ 只剩 fix-1 的项；`phases=[]` 等同不过滤；
      6. 「只看失败 + 只看工具调用」→ 交集：只剩 isError 的 tool_use 与 S1（overflow raw 被 onlyTools 去掉）。
    - **`phaseMeta` / `comparePhases`**：`triage, implement, implement-reentry, fix-1, fix-2, fix-10, verify, review, push, bogus`
      打乱后用 `comparePhases` 排序，断言结果恰为这个顺序（`fix-10` 在 `fix-2` 之后——按数值不按字典序；
      `implement-reentry` 在 `implement` 之后、`fix-1` 之前）；`phaseMeta('implement-reentry').order === 1.5`；
      `phaseMeta('fix-12')` 为 `{label:'修复第 12 轮', order:1.75, sub:12}`；`phaseMeta('bogus')` 为 `{label:'bogus', order:9, sub:0}`。
    - **`formatOffset`**：`0→'+00:00'`、`999→'+00:00'`、`42000→'+00:42'`、`62000→'+01:02'`、`3599999→'+59:59'`、
      `3600000→'+1:00:00'`、`3727000→'+1:02:07'`、`36000000→'+10:00:00'`、`null→''`、`-1→''`、`NaN→''`。
    - **`silenceLabel`**（≥30s 静默分隔线的判定）：`null→null`、`29999→null`、`30000→'⋯ 静默 30s'`、`252000→'⋯ 静默 4m12s'`、
      `3900000→'⋯ 静默 1h05m'`；另加一条组合用例：`withTiming` 处理 at 为 `t, t+29.9s, t+60s` 的三条事件，
      对每条 `silenceLabel(gapMs)` 断言只有第 2 条（与第 3 条间隔 30.1s）非空。
    - **导入方式（使先红真的是 `is not a function`）**：测试文件一律 `import * as R from './replay.js'`、以 `R.applyFilter(...)` 调用。
      评审期在 Node v22 上实测：`import { phaseMeta } from …` 在导出不存在时整份文件报 `SyntaxError: … does not provide an export named`，
      一个用例都不跑；命名空间导入则只有用到缺失导出的用例报 `TypeError: R.phaseMeta is not a function`，其余用例照常跑
      （`pairTools`/`groupSubagents` 快照在提交 1 上为绿）。
  - `segmentRuns`（v2，B1；v3 改写用例 3、4，新增 7–11，第二轮 B1）。task_events fixture 一律带 `toState`、`fromState`、`actor`、`payload`、`at`，
    形状照搬 `store.EventRow`（`query.go:129-136`）：
    1. 「单次运行、有 logRef」：round-0、round-1 各一组；
    2. 「单次运行、logRef 全空」（v4 改写）：round-0 light 为 build(passed)、lint(failed)、typecheck(skipped)，round-1 为
       build、lint、typecheck 全 passed，事件齐全；断言 1 次运行、2 轮（靠规则 4b 切开）；另一组 heavy round-0 末尾连续 3 条
       regression(passed)：断言不切；
    3. **「手动重试后两次运行的 round-0 不合并」**：task_events 为 建单(queued)→implementing→verifying→failed→
       queued(`reason=manual_retry`,`actor=user:7`)→implementing→verifying；run1 的 `round-0` build/lint/repro_fail(passed)/repro_pass(failed)
       落在第一条 verifying 之后，run2 的 `round-0` build/lint/repro_fail/repro_pass/regression 落在第二条之后；
       断言得到 2 次运行、各 1 轮，run1 的证明条是「绿 ✗」、run2 是「已证明」，任何一轮都不同时含两次运行的行；
       run2 的 `cause` 文案为「user:7 手动重试（…）」；
    4. **「task_events 缺失时靠 round 号不递增兜底」**：同上但 `stateEvents=[]`，断言仍切成两次运行；
    5. **「重试后修复回路」**：run1 = round-0、round-1，run2 = round-0 → 3 轮、2 次运行，顺序正确；
    6. `logOverwritten`：情形 3 中 run1 与 run2 `logRef` 相同的那些行为 true、run2 的行为 false；
       复现阶段多段只重叠一段时也为 true（与 AC-5 重试用例 4 同口径）；
    7. **「rebase 跟进重验不经 queued，切成 2 次运行」（第二轮 B1 要求的用例）**：task_events 为
       建单(queued)→implementing→verifying→pr_open→review_feedback(`reason=rebase_followup_reverify`,`actor=system:rebase-followup`)→
       implementing(同上)→verifying(`actor=system:rebase-followup`)，序列里第一条之后**没有任何** `toState=queued`；
       verifications 为 run1 `round-0` 全套 heavy（全部 passed）、run2 `round-0` 全套 heavy（全部 passed，`logRef` 与 run1 同名）。
       断言：2 次运行、各 1 轮；**没有**「第 2 轮（修复第 1 轮之后）」这种轮次；run2 的 `cause` 为「前驱合并后 rebase 重验」；
    8. **「rebase 重验 + logRef 全空」**：同 7 但所有 `logRef` 为 null——规则 3 无法生效，只靠规则 2 也必须切成 2 次运行
       （证明规则 2 本身覆盖了 v2 漏掉的路径，不是靠 round 号碰巧救回来）；
    9. **「approve 后的空运行不显示」**：…→verifying→awaiting_approval→queued(`reason=manual_approve`)→verifying→pr_open，
       verifications 只在第一条 verifying 之后；断言只返回 1 次运行、不显示运行层，没有任何「由 … 重试」的标题；
    10. **「任务停在 verifying 时的 rebase 重验」**：…→verifying→queued(`reason=rebase_followup_reverify`)→verifying，
       断言 2 次运行，run2 的 `cause` 为「前驱合并后 rebase 重验」；
    11. **「区间里夹着非触发原因的 reason」**：…→verifying→blocked_spec(`reason=repro_not_red`)→queued(`reason=manual_retry`)→…→verifying，
       断言 run2 的 `cause` 是手动重试而不是 `repro_not_red` 的兜底文案。
    以下 12–16 是 v4 新增（第三轮 B1）。每条都用真实 `DetectLightProfile` 会产出的序列（与
    `evidence/f04/prd-v4-step-sequence-probe.txt` 一致），`logRef` 按 `fileStepLogger` 的真实格式
    `verify-logs/task-<id>/round-<n>/<NN>-<step>.log` 生成；每条都跑三个变体：**(α)** 事件齐全 + `logRef` 齐全、
    **(β)** `stateEvents=[]` + `logRef` 齐全、**(γ)** 事件齐全 + `logRef` 全为 null：
    12. **「单次运行，pnpm install 与 build 脚本同名」**（评审指定）：light round-0 = build(`pnpm install`)、build、lint、
        typecheck，全 passed。三个变体都断言 1 次运行、1 轮，`boundary` 为 (α) `events`、(β) `logRef`、(γ) `events`；
    13. **「两个 Go 模块」**（评审指定）：light round-0 = build、lint、build、lint，全 passed。三个变体都断言 1 次运行、1 轮；
    14. **「Go 双模块 + pnpm 的 heavy 整轮」**：round-0 = build、lint、build、lint、build、build、lint、typecheck、repro_fail(passed)、
        repro_pass(passed)、regression、regression，复现阶段 `logRef` 为两段（`09-repro_fail.log 10-repro_fail.log`），
        三个变体都断言 1 次运行、1 轮，证明条为「已证明」；
    15. **「多模块的修复回路」**：round-0 = build、lint、build(failed)、lint(skipped)；round-1 = build、lint、build、lint 全 passed。
        三个变体都断言 1 次运行、2 轮，round-1 的 `NN` 从 01 重来不被当成新运行；再加一次手动重试（run2 round-0 与 run1 round-0
        的 `logRef` 同名）：(α)(β)(γ) 都断言 2 次运行，run1 有 2 轮、run2 有 1 轮；
    16. **已知漏切的边界如实标注**：`stateEvents=[]`、`logRef` 全 null，两次 light 运行都全 passed。断言返回 1 次运行、1 轮
        （承认合并），`boundary === 'order'`，`proofOf` 返回的总判定为 `unknown`（界面不显示「通过」）；
        另一组：`logRef` 齐全但其中一条非 skipped 的行为 null（落盘失败），断言该行 `placementUncertain === true`、
        所在轮总判定为 `unknown`。
  - `proofOf`：repro_fail 为 passed/failed/error 时分别得到 红已立 / bug 未复现 / 没跑起来；
    总判定与 Go 端 `Report.Passed` 的规则一致（skipped 不计入、全部 skipped 判未通过）。
  - **`createEventWindow` 终态回放完整性**：假的 `fetchPage` 持有 1000 条事件，终态模式下
    先拿到最后 200 条，`olderAvailable=true`；连续 `loadOlder()` 4 次后，窗口覆盖全部 1000 条，
    并且 `olderAvailable=false`。在提交 1 上这条用例失败：旧逻辑终态拉完一 tick 即停、没有 `loadOlder`
    （以实际输出为准存档，不预设失败文本）。
  - `jumpTo` 定位不在窗口里的 id：窗口被替换为 id 前后各 100 条；替换之后，旧 generation 的 tick
    晚到的响应被丢弃（窗口内容不变）。
- **先红**：上面「提交顺序」一节的命令在提交 1 上退出码非 0；`segmentRuns`、`proofOf`、`withTiming`，以及 v5 新增的
  `applyFilter`、`phaseMeta`、`comparePhases`、`formatOffset`、`silenceLabel`
  这些在提交 1 上还不存在的导出，失败形态是 `TypeError: R.<name> is not a function`——这是「改动前没有这个能力」
  的真实失败，不是模拟。`ac6-commit1.txt` 里这 8 个名字的 `is not a function` 必须各出现至少一次（`grep -c` 写进证据文件尾部），
  且不出现 `SyntaxError`/`ERR_MODULE_NOT_FOUND`。对 B1 最关键的用例 3、4、7、8 另做**变异检查**，各自独立：
  (i) 只注释掉规则 2（`verifying` 运行边界）→ 用例 3、7、8、10 必须失败；
  (ii) 只注释掉规则 3（`(round, NN)` 回退）→ 用例 4 必须失败；
  (iii) 把规则 2 换回 v2 的「`toState === 'queued'`」→ 用例 7、8 必须失败（证明新用例确实钉住了 v2 的漏洞）；
  (iv) v4 新增：把规则 3、4 换回 v3 的 `stepOrder` 写法（规则 3「N 相等且 `stepOrder` 回退」、规则 4a「`stepOrder` 不大于上一行」）
  → 用例 12、13、14、15 必须失败（证明新用例确实钉住了第三轮 B1 指出的假轮次 / 假运行）；
  (v) v4 新增：只删掉规则 4b → 用例 2、15(γ) 必须失败（证明没有 `log_ref` 时轮次确实靠 4b 切开）。
  五个补丁存 `evidence/f04/red/segmentRuns-mut-{rule2,rule3,v2queued,v3steporder,rule4b}.patch`，输出存
  `ac6-mutation-{rule2,rule3,v2queued,v3steporder,rule4b}.txt`。

**AC-7 TaskDetail 渲染契约（构建 + 静态守卫）**
- `make ui && make build` 退出码 0（只在 worktree 里执行，绝不在 `/opt/lathe` 主目录执行）。
- **v-html 守卫（§3.5-4 引用的就是这一条；v3 改为全量白名单，第二轮非阻塞建议）**：
  `make ui-test` 运行（v5 写死扫描范围，第四轮 B2）：
  ```bash
  grep -rn --include='*.vue' --include='*.js' --exclude='*.test.js' --exclude-dir=testdata 'v-html' web/src \
    | sed -E 's/^([^:]*):[0-9]+:[[:space:]]*/\1 /; s/[[:space:]]+$//'
  ```
  `--exclude-dir=testdata` 排除白名单文件自身（它的每一行都含 `v-html`，不排除就会自引用、永远多出 12 行且无法收敛；
  评审期在 base 的副本上实测：不排除时 24 行，排除后 12 行，与白名单相等）；`--exclude='*.test.js'` 排除测试文件
  （不进 bundle，测试里可能出现 `v-html` 字面量）；`--include` 只扫会被打进界面的源码类型。为了不让「以后有人加 `.ts`/`.jsx`
  文件」悄悄绕过守卫，同一目标另跑 `find web/src -type f \( -name '*.ts' -o -name '*.tsx' -o -name '*.jsx' -o -name '*.html' \) | grep -q . && exit 1`
  （base 上 `web/src` 只有 `.vue`/`.js`/`.css`，评审期实测），命中即失败并提示「扩展 v-html 守卫的 --include」。
  输出的每一行即「文件路径 + 空格 + 去掉首尾空白的行文本」（不含行号，
  避免无关改动挪动行号就误报），与提交进仓的白名单 `web/src/testdata/v-html-allowlist.txt` 逐行比较，**多一行、
  少一行都退出非 0**，并打印差异（got/want）。白名单的初始内容就是 base 上的 12 处（`AgentEventItem.vue` 的
  `mdOf(ev)`、`md(resultText(ev))`，`TaskDetail.vue` 的 `md(task.agentSummary)`，以及 `IssueDetail.vue`、
  `prd/ThinkingBlock.vue`、`prd/PRDDocPanel.vue`、`prd/PRDConversation.vue` 里既有的 9 处，全部是经 DOMPurify 的
  `md(...)`）。本线**不向白名单新增任何行**：新建的日志查看组件、`replay.js` 等若出现 `v-html`，守卫必然失败。
  以后别的线新增合法的 markdown 渲染点时，同一个 PR 里改白名单即可——改动会出现在 diff 里接受评审，
  这正是全量白名单相对「只盯三个文件」的价值。这是回归守卫，base 上同样成立，不要求先红；另做一次变异检查：
  在新建的日志查看组件里临时加一行 `v-html`，`make ui-test` 退出非 0，输出存 `evidence/f04/red/ac7-vhtml-mutation.txt`。
- `grep -c "logRef" web/src/views/TaskDetail.vue` ≥ 1（**先红**：base 上为 0，§2.2-5）。
- `grep -n "implement-reentry" web/src/replay.js` 有命中（阶段标签）。

**AC-8 不越界与装配**
- `git diff --stat origin/dev...HEAD -- internal/task/state.go internal/integration/agent/digest.go internal/integration/agent/batchsink.go internal/runner/eventsink.go internal/runner/verify.go internal/runner/heavy.go internal/runner/verifylog.go internal/runner/pipeline.go go.mod go.sum web/package.json web/pnpm-lock.yaml skills/ .claude/ workspaces/ AGENTS.md CLAUDE.md`
  输出为空（v3 把 `AGENTS.md`、`CLAUDE.md` 加进清单：规范变更只在 PR 描述里提议，§7-4）。
- `git diff --name-only origin/dev...HEAD -- migrations/` 只包含 `0041_agent_events_reentry_phase.{up,down}.sql`。
- **`internal/runner/` 的边界（v2 收紧，B4）**：`git diff --name-only origin/dev...HEAD -- internal/runner/`
  的输出**恰好**是 `internal/runner/redact.go` 与 `internal/runner/redact_test.go` 两行；
  `git diff origin/dev...HEAD -- internal/runner/redact.go | grep -c '^-[^-]'` 输出 `0`（只新增、不改既有行）。
  AC-1 对 `runner.PhaseImplementReentry` 的引用在 `internal/httpapi` 的测试里，不触碰 `internal/runner/`。
- **装配守卫（v2 新增，B4；docs/08 T4「struct literal 漏一个字段而单测全绿」的同类防线）**：
  `cmd/lathe/assembly_test.go::TestServeWiresVerificationLogIntoAPI`，沿用同文件 `TestServeStartsWorkersAfterEverythingIsWired`
  的 `go/parser` 做法：在 `serve` 函数体里找到类型为 `httpapi.API` 的复合字面量（`&httpapi.API{...}`），断言
  - 恰好有一个这样的字面量；
  - 含键 `LogDir`，值的源码为 `cfg.DataDir`；
  - 含键 `KnownSecrets`，值的源码为 `cfg.KnownSecrets()`；
  - 失败信息列出实际找到的键（got）与要求的键（want）。
  再补一条 grep 级的双保险（写进 `make ui-test` 之外的 PR 检查清单，供人复核）：
  `grep -nE '^\s*(LogDir|KnownSecrets):' cmd/lathe/main.go` 命中 4 行（Pipeline 两行 + API 两行）。
  **先红**：base 上字面量里没有这两个键，测试报 `missing keys [LogDir KnownSecrets]`，输出存 `evidence/f04/red/ac8-wiring.txt`。
- `make lint && make test` 全部通过；另外运行 `go test -race -p 1 -count=1 ./internal/httpapi/...` 并通过，
  输出存进证据目录。

**AC-9 端到端手测（:8404，库 `lathe_f04`）**
- **前置**：AC-1「执行顺序」第 1 步（空数据上的 down/up 检查）已完成并存证；本 AC 之后不再跑 down，
  除非先完成本 AC 末尾的清理。
- 在子 shell 里设置 `LATHE_*` 后启动 `/tmp/lathe-f04 serve`（端口 8404），记录 PID，结束时按 PID 停止
  （禁止 `pkill -f`）。
- 用测试 helper 往 `lathe_f04` 造一个终态失败任务：1200 条事件（含 `fix-1`、`implement-reentry`、
  subagent 子块、isError 的工具调用），两轮 heavy 验证（第 1 轮绿阶段失败，第 2 轮通过），
  并在 `LATHE_DATA_DIR` 下写好对应的日志文件。造数据的 helper 给每个种子任务的 `external_key` 加随机后缀
  （`f04-ac9-<rand>-…`），并把创建出的 **task_id 列表**写进 `evidence/f04/ac9-seed-ids.txt`（一行一个）——
  这是后面清理的唯一依据。
- 截图存进 `evidence/f04/`：①打开页面直接看到最后一页和「共 1200 条」；②点「第一个失败」跳到第 1 个失败，
  对应的阶段和工具输出已展开、行被高亮、URL 带 `#ev-<id>`；③用这个 URL 在新标签页打开，定位到同一行；
  ④阶段 chip 过滤为「修复第 1 轮」；④b 打开「只看失败」后只剩失败行与含失败的 subagent 子块（子块内其余行仍可见）；⑤验证区显示两轮证明条，第 1 轮绿阶段为红框；
  ⑥完整日志查看器分块加载，并且脱敏标记可见；⑦另一个账号访问同一任务和日志 URL → 页面显示 404
  （同时附上 curl 输出）。
- **装配实证（B4）**：用真实二进制（不是测试里的 `API{LogDir: t.TempDir()}`）`curl` 一个有 `log_ref` 的 vid，
  状态码为 200（不是 503「验证日志未启用」），输出存证据目录。
- **重试场景（B1；v3 增加 rebase 重验）**：再造两个任务（同样登记进 `ac9-seed-ids.txt`）：
  (a) **手动重试**：第 1 次运行 round-0 绿阶段失败、经 `/api/tasks/{id}/retry`（或等价的 `task_events` 序列）重派、
  第 2 次运行 round-0 通过，第 2 次运行的日志覆盖同名文件。截图⑧：验证区显示「第 2 次运行 · … 手动重试 ✓ 已证明」在上、
  「第 1 次运行 · 首次运行 ✗」折叠在下，两次运行各自的证明条不混；第 1 次运行的步骤没有「完整日志」按钮，悬停有
  「已被第 2 次运行覆盖」说明。`curl` 第 1 次运行的 vid → 404 `log_overwritten`；第 2 次运行的 vid → 200。
  (b) **rebase 跟进重验**：`task_events` 序列 pr_open→review_feedback→implementing→verifying（actor `system:rebase-followup`、
  `reason=rebase_followup_reverify`，与 `mergepoll.go:547-566` 写出的一致，中间不经 `queued`），两次运行各一组 round-0。
  截图⑨：显示为「第 2 次运行 · 前驱合并后 rebase 重验」与「第 1 次运行」两块，**不出现**「第 2 轮（修复第 1 轮之后）」。
- **清理（v3，第二轮 B2）**：全部截图与 curl 完成后、停 serve 之前或之后均可，按 ID 清理种子数据：
  ```bash
  ids=$(paste -sd, /opt/lathe-wt/evidence/f04/ac9-seed-ids.txt)
  psql "$LATHE_TEST_DSN" -c "DELETE FROM tasks WHERE id IN ($ids) AND external_key LIKE 'f04-ac9-%'"   # agent_events/verifications/task_events 级联删除
  psql "$LATHE_TEST_DSN" -Atc "SELECT count(*) FROM agent_events WHERE phase='implement-reentry'"      # 必须为 0
  ```
  `WHERE` 同时按 ID 与随机前缀过滤，误填 ID 也删不到别的行；`LATHE_DATA_DIR` 下的日志目录用的是本 AC 专用的临时目录，
  一并删除。输出存 `evidence/f04/green/ac9-cleanup.txt`。清理后再跑一次 `lathe_migrate down && lathe_migrate up`
  并退出码 0，作为「本线结束时迁移仍可回滚」的收尾证据（同文件）。这里是在库里删数据，删的只是本 AC 自己造的、
  按 ID 登记的行，属于测试 fixture 的收尾，不是 AGENTS.md §4「真删任何东西」所指的清库/清现场；
  **不跑** `make clean-test-db YES=1`。
- 浏览器性能：在 2000 条事件的窗口下，全部折叠时页面 DOM 节点数
  `document.querySelectorAll('*').length` < 12000（截图附带控制台输出）。

---

## 6. 风险与代价

| 风险 / 代价 | 应对 |
|---|---|
| **0041 加 CHECK 时锁全表**（ACCESS EXCLUSIVE，持续一次全表扫描的时间），期间 runner 写事件会阻塞 | 0011 和 0015 在同一张表上做过同样的操作；单次扫描在当前生产规模下是秒级。PR 里写明建议在低峰期执行迁移。被阻塞的写入发生在 `BatchSink` 里，有 5s 超时（`batchsink.go:38`），超时会丢掉那一批并记 warn，不会让任务失败 |
| **0041 down 在已有 reentry 行时会失败** | 有意为之（沿用 0015 down 的立场，`0015_agent_events_push_phase.down.sql:3-4`）：回滚不应该静默删除证据。PR 里写明处理步骤。本线自己的验收按 AC-1「执行顺序」先在空数据上跑 down/up，AC-9 结束时按 ID 清理种子数据再跑一次收尾 down/up，不让本线的测试数据把自己的回滚检查卡住 |
| **0041 可能被评审认为超出「回放体验」的范围** | 它只放宽一条约束，代码零改动，并且是回放完整性的前提（§1.1-2）。如果评审要求拆出去，可以单独成一个 `fix/agent-events-reentry-phase` PR，本 PRD 其余部分不依赖它（前端的 `phaseMeta` 照样认识这个阶段名） |
| **日志读取的脱敏不完整**：读取时拿不到那一轮的 `StackEnv`，隔离栈的临时口令只靠形态层识别 | 临时口令只在单任务生命周期内有效，栈销毁后失效；形态层覆盖 `user:pass@` DSN。这一点写进 `docs/features/run-replay.md` 的「已知边界」。如果评审认为不可接受，备选方案是只对**失败**步骤开放完整日志（更少暴露面），或者暂缓 §3.2.3 |
| **新增一个读取磁盘文件的入口**（攻击面） | 用户输入只有整数，四道路径校验，测试覆盖穿越和符号链接（AC-5）。文件本身是 0600、属于 lathe 进程 |
| **轮次与运行的推断依赖 runner 的两条执行纪律**（v4 改写，第三轮 B1）：同一次 `runVerify` 内 `log_ref` 的 `NN` 严格递增；一轮之内非 passed 之后只跟 skipped、light 段排在复现之前（§2.1 v4 新增三行）。v3 写的「步骤序号只增不减」是错的（多 Go 模块、`pnpm install` 都会让同名步骤重复），已删除 | 两条纪律分别钉在 `verifylog.go:76-90` 与 `verify.go:246-258`、`heavy.go:254-316`；任何人改掉它们（例如让 light 步骤失败后继续跑、或复用 logger 跨轮）都会让 AC-6 用例 12–15 失败。**残余盲区**：没有 task_events、没有 `log_ref`、light 档、上一次运行全 passed 时两次运行会并成一轮（用例 16）；这时 `boundary=order`，界面标注「按顺序推断」并**不给总判定**——宁可不下结论，也不凭可能混在一起的行宣布通过。生产上 task_events 与状态转移同事务写入，这种组合只出现在老数据里。根治需要 verifications 加 `run`/`round` 列，属于采集口径变更，已列入 §4 |
| **运行切分依赖「每次验证运行以一次进入 `verifying` 开头」**（v3 更正：v2 写的「依赖 `queued` 转移」已被证伪——rebase 跟进重验今天就不经 `queued`，`mergepoll.go:547-566`） | v3 的运行边界不再枚举入口，而是钉在唯一能写 verifications 的状态上（§2.1 新增一行的取证）。这个前提只有在「有人新增一条不进 `verifying` 就写 verifications 的路径」或「修复回路改成离开再回到 `verifying`」时才会被打破——两者都是状态机或流水线语义变更，会被 `state.go` 的评审拦住。即便如此，`(round, NN)` 回退（规则 3，v4）仍无条件独立切分，AC-6 用例 4 不给 task_events、用例 8 不给 logRef，分别证明两条规则各自都能切开。v4 更正 v3 的「所有规则只多切、最坏多切一刀」：v3 的 `stepOrder` 规则在常见仓库上**常态误切**（第三轮 B1），这句话当时不成立。v4 的规则 2、3、4a–4d 每一条在真实单次运行里都不误切（用例 12–15 的三个变体守着），唯一的失误方向是用例 16 那种**漏切**，而漏切时界面不给总判定 |
| **被重试覆盖的旧日志在界面上不可读**（404 `log_overwritten`） | 文件内容确实已不是那次运行的，展示它就是挂错证据（铁律 1）。status 仍来自 `verifications` 行、照常显示；根治见 §4 |
| **覆盖的时间判定依赖时钟**：DB `now()` 与宿主机文件 mtime 偏差超过 5s 时可能误判 | 只会往「判为已覆盖」偏（fail-closed，藏证据而非挂错证据）；库内判定（有更新行声明同路径）不依赖时钟、是主判定。Lathe 默认部署 DB 与进程同机或同机房，偏差在毫秒级 |
| **安全切点检验有计算开销**，且对「单个形态匹配长于 `redactContext`（16KiB）且跨行」的内容不提供保证 | 常见情况每块 4 次窗口脱敏；最坏有界（256 次）。超长跨行匹配（如 16KiB 以上的多行引号值）列入 `docs/features/run-replay.md`「已知边界」，与 `redact.go:54-82` 既有的「不是通过条件」清单同一立场 |
| **超过 1MiB 的单行之后的内容不显示**（`withheld`） | fail-closed：宁可让人去服务器看原文，也不在界面上冒半截凭据的风险。压缩输出、整行 JSON 超过 1MiB 在验证日志里罕见；界面如实提示原因和服务器路径 |
| **±200ms 时间粒度**可能被误读成精确值 | 时间列 `title` 明确写「入库时间，粒度约 ±200ms」；小于 1s 的间隔不显示（与现有工具耗时规则一致） |
| **与 f01 在 `TaskDetail.vue` 上的相邻行冲突**（v3 改写：f01 R1 起不碰 `/events` 与时间线） | 冲突面只有两处：import 区（f01 在 `:5` 之后加一行，本线在 `:7` 之后加 / 改 livelog 那行）与执行日志卡片之前（f01 在 `:429` 注释之前加一行，本线只改卡片内部）。解法见 §3.7 f01 行：两边原样保留，无语义冲突，合并顺序无关。PR 描述里点名这两处。另有一处**测试接缝**（v5，第四轮 B1）：f01 在 `TestTaskEventsEndpoint` 断言 `/events` 顶层键恰为 `{events, last_id}`——本线的兼容契约让它在任何合入顺序下都成立（§3.2.1、§3.7 f01 行 (c)），AC-2 用例 1 在本线侧守同一条契约 |
| **`make ui-test` 不在 `make test` 里**，可能被跳过 | `make test` 是 Go 专用（`-p 1` 有它自己的理由，§5.1）。本 PR **不改** `AGENTS.md`（v3 改，第二轮非阻塞建议）：在 PR 描述里**提议**把 `make ui-test` 加进 AGENTS.md §3 表格「前端 `web/`」一行的必做项，由人决定是否采纳、以单独的 docs PR 落地；CI 是否增加 `node --test` 步骤同样留给人决定（ubuntu-latest 自带 Node，不需要新依赖）。本 PR 不改 `ci.yml`，避免和其他 11 条线在 CI 文件上冲突。在规范采纳之前，本线 PR 的验证说明里显式贴出 `make ui-test` 的输出 |
| **Go 与 node 共用 JSON fixture** 增加了跨语言耦合 | 这正是目的：失败判定只有一份真相。fixture 放在 `web/src/testdata/`，Go 测试用相对路径读取 |
| 前端窗口逻辑变复杂（实时/回放/定位三种模式） | 全部收进 `createEventWindow`，用 node 单测覆盖状态转换；TaskDetail 只负责渲染 |

---

## 7. 文档改动（同一个 PR）

1. **`docs/features/run-replay.md`**（新建）：本 PRD 定稿版，包括「已知边界」（时间粒度、日志脱敏边界、
   轮次推断、0041 只救未来的数据）。
2. **`CHANGELOG.md` `[Unreleased]`**：
   - `### Added`：任务详情页执行日志改为回放视图：每步时间与静默间隔、阶段 chip 过滤（含失败数）、
     失败一键定位与可分享的深链接、验证区按轮次显示红绿证明条、完整验证日志按块查看（经过脱敏）；
     新增 `GET /api/tasks/{id}/events/summary`、`GET /api/tasks/{id}/verifications/{vid}/log`，
     `GET /api/tasks/{id}/events` 支持 `paged`/`before`/`phase`/`kind`/`failures`（不带这些参数时响应与旧版完全相同）。
   - `### Fixed`：终态任务的执行日志只加载前 400 条且不提示；`implement-reentry` 阶段的事件被
     数据库约束拒收、整批丢弃（迁移 0041）。
3. **`docs/04-agent-visibility.md`**：§3.3 补充新的查询参数和两个新接口；§3.4 改为指向
   `docs/features/run-replay.md`；新增 §8「回放视图」（结论先行：为什么把定位而不是播放作为核心交互；
   为什么证明条只读 verifications；时间粒度如何标注）；§7.6 表格中「时间轴回拖」一行补一句
   「仍然不做，定位功能见 §8」。
4. **`AGENTS.md`：本 PR 不改**（v3）。PR 描述里单列一节「规范变更提议」：建议把 §3 表格「前端 `web/`」一行的必做项
   改为 `make ui-test && make ui && make build`，附理由（§6「`make ui-test` 不在 `make test` 里」一行），由人决定是否采纳。
5. **README**：不新增环境变量，变量表不变（PR 描述里明确写出「已核对，无需改动」）。
6. **PR 描述的「与邻居的接缝」一节**（v3 新增）：点名 ① `ownedTask` helper——按 §3.7 f12 行的两种合入顺序分工：本线先合则 f12 只改它的函数体；
   f12 先合则本线复用 f12 的 `ownedTask`、不改函数体，并把两条新路由补进 f12 的 `taskReadRoutes`；② 与 f01 在 `TaskDetail.vue` 的两处相邻行及解法（§3.7 f01 行）；③ `/events` 不带新参数时顶层键恰为 `{events, last_id}`，与 f01 AC-17 在 `TestTaskEventsEndpoint` 里的断言同一契约，本线不改该测试函数（§3.7 f01 行 (c)），并贴出 rebase 到对方之后 `TestTaskEventsEndpoint`、`TestTaskEventsLegacyShapeUnchanged` 的输出（谁后合入谁贴）。

---

## 8. 评审答复

### 8.1 第一轮 B1–B4（v2）

> v3 注：下面 B1 第二条里「6 个回到 `queued` 的入口都写这一条」的论证在第二轮被证伪（rebase 跟进重验不经 `queued`），运行边界已改为进入 `verifying`，见 §8.2 B1。

四条全部接受，没有不同意的条目。下面写清每条改了哪里、证据在哪，以及与评审建议不完全一样的地方和理由。

**B1 证据挂错运行（铁律 1）——接受。**
- 先在 base `cff5332` 上复现了评审描述的覆盖：同一任务两次 `stepLogger(id, 0)` 拿到同一个
  `verify-logs/task-42/round-0/01-build.log`，第一次的 ref 读出来已是第二次的内容
  （`/opt/lathe-wt/evidence/f04/prd-v2-verifylog-overwrite-probe.txt`，临时探针、未入库）。§2.1 补了三行取证。
- `segmentRounds` 改为 `segmentRuns`（§3.3.1）：先按运行切（`task_events` 的 `queued` 转移），再在运行内按轮次切
  （步骤序号回退 / round 号变化 / tier 变化）；没有 task_events 时「round 号不递增 = 新运行」兜底。
  与评审建议的差别：评审只要求「序号回退或 round 号不递增时开新组」；我额外用了 `task_events`，
  因为它是重派的直接证据（6 个回到 `queued` 的入口都写这一条），而且两种信号互为兜底；并明确 round 号
  **只能多切、不能合并**，堵死 v1「按 round-N 合并」的那条路。
- 日志接口（§3.2.3）：评审建议比较 `ModTime` 与 `verifications.at`。我采纳，并把它放在第二位，第一位是
  **库内判定**（存在 id 更大、声明同一路径的行），理由是它不依赖时钟、可在测试里确定性构造；`ModTime`
  判定负责「后一次运行正在写、还没插行」的窗口；再加一道读后复核兜住读的过程中被覆盖。被覆盖时 404
  `log_overwritten`（评审给的两个选项里选了 404，而不是返回内容加「已被覆盖」标注：标注后仍然把
  别的运行的内容挂在这条证据下面展示，本质还是挂错）。对非属主仍是统一的「任务不存在」。
- AC-5 新增 5 条重试覆盖用例、AC-6 新增 4 条 `segmentRuns` 重试用例；先红分别在提交 B（真实的未加覆盖判定的
  handler）和提交 1（真实的旧加载逻辑）上跑，另对 `segmentRuns` 的运行边界规则做变异检查。AC-9 增加重试场景截图⑧。

**B2 块边界切开凭据（§7 红线）——接受。**
- 在 base 上用现有 `redactText` 复现了四种切分泄露：DSN 口令中间切开、`API_KEY=` 裸值中间切开、
  **以及切在换行处也会泄露的两种**（`password:\n  值`、引号值内含换行）
  （`/opt/lathe-wt/evidence/f04/prd-v2-redact-split-probe.txt`）。后两种说明评审第二个选项「块边界只落在换行处」
  本身也不够。
- 采用的方案是评审两个选项的合并加强：边界只落在换行之后（选项 2），并且这个换行必须通过「两侧各取
  `W` 上下文窗口，切开脱敏 == 不切脱敏」的检验（选项 1 的重叠窗口，但用来**选择切点**而不是事后剪掉重叠部分）。
  没有照搬「多读一段、脱敏后再剪掉重叠」的原因：脱敏会改变长度（明文换成 `[REDACTED]`），剪掉重叠部分需要
  把原文偏移映射到脱敏后的偏移，而 `redactText` 是多遍替换，没有可靠的偏移映射；在安全切点上切则两侧
  直接拼接，不需要映射，并且可以用「逐块拼接 == 整份脱敏」这一条等式在测试里验证。
- 单行上限 `logLineMax = 1MiB`；找不到安全切点 → `withheld`，整段不返回（fail-closed）；非安全切点的 `offset` → 400。
- AC-5 增加评审要求的两个用例（横跨 64KiB 边界、所在行超过 4KiB），另加裸值 kv、跨行形态、超 1MiB 单行、
  非法 offset 四个用例。

**B3 AC 不可机器验证 / 编号矛盾——接受。**
- 删除 `legacyLoad` 对照用例以及「之后改成 `assert.equal(covered, 400)`」的做法。改为两步提交：提交 1 把
  TaskDetail 的现有加载逻辑原样抽进 `createEventWindow`（纯重构），提交 2 实现回放；同一份 `replay.test.js`
  用 `git checkout <提交1> -- web/src/replay.js` 在旧实现上跑，失败输出带提交 hash 存档（AC-6）。
- v-html 守卫统一为 **AC-7**，§3.5-4 的引用从 AC-8 改为 AC-7；守卫写成 `grep -c` 的确定值并脚本化进 `make ui-test`。
- 顺带统一了先红的口径：每条先红都写明在哪个真实提交上、跑哪条命令、证据文件名。

**B4 runner 边界与装配点——接受。**
- AC-1 的矛盾：store 包测试**无法** import runner（runner 非测试代码 import 了 store，会成环），所以 store 测试只写
  字面量；常量与字面量的绑定改由 `internal/httpapi` 的测试用 `runner.PhaseImplementReentry` 完成（httpapi 已经
  import runner，`api.go:16`），不触碰 `internal/runner/`。
- 装配点写明在 §3.4：`serve()` 里 `apiSrv := &httpapi.API{...}` 字面量，`ConfigStatus` 之后追加
  `LogDir: cfg.DataDir`、`KnownSecrets: cfg.KnownSecrets()`，并给出改后的完整字面量。
- AC-8 新增 `TestServeWiresVerificationLogIntoAPI`（`go/parser` 检查字面量里两个键及其取值，沿用
  `assembly_test.go` 已有的做法），base 上先红；AC-9 增加真实二进制上「非 503」的 curl 实证。
- AC-8 的 runner 边界改为可机器判定的 `git diff --name-only` 恰好两行 + `redact.go` 无删除行；同时把
  `verifylog.go`、`pipeline.go` 加进「必须零改动」清单（B1 的修法完全在读取侧）。

### 8.2 第二轮 B1–B3 与非阻塞建议（v3）

三条 blocker 全部接受；非阻塞建议四条全部采纳。与评审建议不完全一致的地方写在各条里。

**B1 运行边界取证不实——接受。**
- 复核属实：`mergepoll.go:547-566` 的 `reenterForVerify` 在任务停在 `pr_open` 时走 `pr_open→review_feedback→implementing`
  （`:551` payload `reason=rebase_followup_reverify`），`:472-479` 以 `Entry: EntryVerify` 调 `Execute`，`stageVerify` 在
  `pipeline.go:936` 转 `implementing→verifying` 后从 `runVerify(0)` 写 round-0（`:983`）；只有停在 `verifying` 时才走 `:569`
  的 `verifying→queued`。v2 的规则 2 会把这次重验并进上一次运行，再由「步骤序号回退」切成「第 2 轮」，错标成修复回路。
- (a) §2.1 改正：「重派复用原任务行」一行删去「所有重派入口都先转 queued」，补列 `machine.go:944`（依赖唤醒），并写明
  rebase 跟进重验的实际路径；§6 对应行改写为「v2 的前提已被证伪」。
- (b) 两个选项都做了，并且比评审建议再往前一步：
  - 「round-N 不大于上一轮的 N ⇒ 新运行」改为**无条件生效**（§3.3.1 规则 3）。落成逐行判定时把「不大于」精确化为
    「N 变小，或 N 相等且步骤序号回退」：同一轮里 build、lint 等相邻行本来就是同一个 N，逐行套「≤」会把每一行都切开；
    「上一轮」需要先知道轮次边界，而轮次边界又依赖运行边界，循环定义。精确化后语义与评审的意图相同（两次运行的 round-0
    在 repro_pass→build 处必然回退），AC-6 用例 4 守着。
  - 没有采用「把 `reason=rebase_followup_reverify` / `actor=system:rebase-followup` 也认作运行边界」的**枚举**写法，而是把
    运行边界改成 `toState === 'verifying'`（规则 2）。理由：枚举入口正是 v2 出错的方式——今天是 6 个 `queued` 入口漏了 rebase，
    明天再加一个入口又会漏。状态机里能写 verifications 的只有 `verifying`，而每次验证运行恰好进入它一次
    （`state.go:90`、`:106` 只有两条入边；runner 里只有 `pipeline.go:936` 与 `:1115` 两处转入，后者是不写验证的推送续跑；
    `persistVerifications` 的两个调用点 `:987`、`:1050` 都在 `:936` 之后，修复回路不离开 `verifying`），
    取证写进 §2.1 新增的一行。这样 rebase 原因只用于**标题**（规则 7），不用于**切分**。
  - 证明规则 2 本身覆盖了 v2 的漏洞、而不是靠 round 号碰巧救回：AC-6 用例 8 让 `logRef` 全为空（规则 3 失效）也必须切成 2 次运行；
    变异检查 (iii) 把规则 2 换回 v2 的 `queued` 写法，用例 7、8 必须失败。
- (c) AC-6 新增用例 7（评审指定的 pr_open→review_feedback→implementing→verifying、无 queued，断言 2 次运行且不出现
  「第 2 轮（修复第 1 轮之后）」）、8（同 7 + logRef 全空）、9（approve 空运行）、10（停在 verifying 时的 rebase 重验）、
  11（标题取因只认表内 reason）；
  AC-9 新增 (b) rebase 跟进重验场景与截图⑨。
- 运行标题：删除「由 {actor} 重试」，改为按触发原因映射（§3.3.1 规则 7 的表，`runCauseOf`），未知原因走中性兜底
  「重新进入验证（{actor}）」，不猜。approve（`api.go:488`）之后的运行走 `EntryPush`、不写验证，按规则 6「空运行不显示」直接不出现，
  所以不会有任何「由谁重试」的标题挂在它上面。

**B2 迁移可回滚与验收顺序冲突——接受，两种办法都用。**
- 没有改 0041 down 的立场（库里有 reentry 行时失败，不静默删证据，与 0015 down 一致）。冲突在验收顺序，不在迁移本身。
- AC-1 新增「执行顺序」：先在空数据上跑 `lathe_migrate up && down && up`，证据里同时记录 reentry 行计数为 0；再跑 `make test` 与 AC-9。
- AC-1 的测试 fixture 带随机 `external_key`、按自己创建的 task_id 在 `t.Cleanup` 删除（级联删掉 agent_events）、断言按 task_id 过滤。
- AC-9 的种子任务带随机前缀并把 task_id 写进 `evidence/f04/ac9-seed-ids.txt`；结束时 `DELETE … WHERE id IN (…) AND external_key LIKE 'f04-ac9-%'`，
  确认 reentry 计数为 0 后再跑一次收尾 down/up。被杀进程留下的残留先 `make clean-test-db` 干跑查看，不跑 `YES=1`，
  不用按 phase 的全表删除。

**B3 与 f01 的边界描述过时——接受。**
- 复核 f01 PRD R2：`f01-interject.md:290`（不改 agent_events、不扩展 `/events`）、`:458-467`（撤回理由）、`:655-659`
  （TaskDetail 只加 2 行，AgentEventItem 不改）、`:731`（点名本 PRD 两行过时）。
- §3.7 f01 行、§3.3.3 末句、§6 冲突行全部改写为「f01 只在 TaskDetail.vue 加 2 行挂 InterjectPanel，不碰 `/events` 与时间线，
  本线不为插话做任何事」。相邻行冲突的解法写进 §3.7：本线的 import 放在 `:7` livelog 那行之后，不插进 f01 所在的 `:5`/`:6` 之间；
  本线只改执行日志卡片内部，不动 `:429` 注释和卡片之前的兄弟节点；真冲突时两边原样保留。

**非阻塞建议——全部采纳。**
- f12：§3.7 f12 行写明本线会把 `taskEvents` 内联的 `tk.UserID` 判定（`api.go:149-154`）
  收进 `ownedTask`；§7-6 要求 PR 描述点名。（2026-11-11 按 f12 v5 §3.11 同步：两种合入顺序下的分工写进 §3.7 f12 行，原引用的 `f12-team-roles.md:368` 是旧行号，已删。）
- AGENTS.md：本 PR 不改（§7-4），改为在 PR 描述里提议、由人决定；AC-8 零改动清单加入 `AGENTS.md`、`CLAUDE.md`，机器守住。
- v-html：AC-7 改为 `grep -rn v-html web/src` 对照提交进仓的全量白名单（base 上 12 处），多一行少一行都失败，
  并加一次「在日志查看组件里塞一行 v-html」的变异检查；§3.5-4 同步。

### 8.3 第三轮 B1（v4）：证明条会把证据挂错运行

**接受，按评审建议的方向修，并多补了两处。**

- **复核属实，并实测**：`DetectLightProfile` 对每个 `go.mod` 目录各产出 `build`、`lint`（`verify.go:186-194`），
  有 `package.json` 时先产出 `Name=build` 的 `pnpm install`（`:205`），再产出脚本步骤（`:214-231`）；heavy 档把这串步骤
  当 `HeavyParams.Light` 先跑（`pipeline.go:1303`，`heavy.go:255-266`）。在本线 worktree 里用临时测试文件调真实的
  `DetectLightProfile` 和 `fileStepLogger`（跑完即删，`git status` 干净），双 Go 模块 + 带三个脚本的 `package.json`
  得到一轮 `build,lint,build,lint,build,build,lint,typecheck`；两次运行 × 两轮的 `log_ref` 显示 `NN` 在每次 `runVerify`
  内为 `01…08`，换轮、换运行都回到 `01`（`/opt/lathe-wt/evidence/f04/prd-v4-step-sequence-probe.txt`）。v3 规则 4a 会在
  第 3 条、第 6 条处把这一轮切成三轮；规则 3（同 N 且 `stepOrder` 回退）会在同样的位置切出假的「第 2、3 次运行」，
  并且 task_events 齐全时也会切。§6 里「只多切、最坏多切一刀」在这类仓库上不成立。
- **改法**（§3.3.1 规则 3、4；§2.1 新增三行取证）：
  - 规则 3 改用 `parseLogRef` 得到的 `(round, NN)`：round 变小，或 round 相等且 `NN` 不大于上一条，才判新运行。
    `stepOrder` 整个删除。这是评审的原建议。
  - 规则 4（`log_ref` 为空时的轮次边界）：评审建议「repro_fail/repro_pass/regression 之后又出现 light 步骤」或 tier 变化，
    两条都采纳（4c、4d）。**另加 4b**：「非 skipped 行紧跟在非 passed 行之后」。理由：只用评审的两条，light 档的修复回路
    （`build,lint(failed),typecheck(skipped)` 之后是 `build,lint,typecheck`）在 `log_ref` 为空时切不开，这正是 v3 的
    `stepOrder` 原本要覆盖、而 4c 覆盖不到的情况。4b 的依据是两条执行纪律：一轮之内第一个非 passed 之后只跟 skipped
    （`verify.go:246-258`、`heavy.go:254-316`），修复回路只在上一轮失败时才开下一轮（`pipeline.go:992-997`）。
    所以 4b 不会在单次运行里误切，在同一次运行内也能找全所有轮次边界。保留了 4a（有 `log_ref` 时 round 变化）作为冗余信号。
  - 同名步骤重复不再是任何规则的触发条件。
- **v3 说的「只多切」改正为如实陈述残余盲区**：没有 task_events、没有 `log_ref`、light 档、上一次运行全部 passed 时，
  两次运行会并成一轮。这是**漏切**，不是误切。所以 v4 加了 `boundary`（`events`/`logRef`/`order`）：`order` 的运行和含
  `placementUncertain` 行的轮次，`proofOf` 返回 `unknown`，界面不给总判定。宁可不下结论，也不凭可能混在一起的行宣布通过。
  这与铁律 1 一致。§6 对应两行已改写。
- **AC-6**：评审指定的两个用例就是用例 12、13。另加 14（Go 双模块 + pnpm 的 heavy 整轮，复现多段 `log_ref`）、15（多模块修复回路 +
  重试）、16（残余盲区如实标注）。每条都跑 (α) 事件齐全、(β) 无事件、(γ) 无 `log_ref` 三个变体。用例 2 改写为 light 失败轮。
  变异检查新增 (iv)：换回 v3 的 `stepOrder` 写法，用例 12–15 必须失败；新增 (v)：删掉 4b，用例 2、15(γ) 必须失败。
- **§2.1 / §3.3.1 的错误论述已改**：「步骤按序执行」「同 round 内步骤回退只可能来自另一次运行」删除，改为 NN 前缀与
  执行纪律两条有代码依据的事实。
- **与邻居的接缝**：本轮只改 `web/src/replay.js` 的纯函数规则和 AC-6 用例，不涉及任何共享文件、迁移或 API，
  对 f01/f03/f06/f12 的口径没有影响，不需要对方同步措辞。

### 8.4 第四轮 B1–B5（v5）

五条都逐条核实属实，全部接受。下面写改了哪里、与评审建议不完全一样的地方，以及评审期的实测。

**B1 与 f01 的 `/events` 响应形状接缝：接受，选 (a)。**
- 核实：f01 AC-17 搭配红线（`f01-interject.md:1086-1089`）要在 `TestTaskEventsEndpoint`（`internal/httpapi/events_test.go:14`，
  base 上该函数现在只断言 `events`、`last_id` 两个键的值）里加「顶层键恰为 `{events, last_id}`」。v4 §3.2.1 一边说「不传新参数时与现在完全一致」，
  一边无条件加 `first_id`/`has_more`；§3.3.1 的实时模式只传 `after=lastId` 却依赖 `has_more`。两者确实不可兼得，§3.2.1 自相矛盾也属实。
- 选 (a) 而不是 (b) 的理由：(a) 只改本线的 PRD，f01 的断言原样成立，**两条线谁先合入都不需要对方改任何东西**，符合「先合入者不依赖后合入者」；
  (b) 需要 f01 线同步改 PRD 与测试，形成跨线依赖。此外「旧调用方拿到的形状一字不变」本身就是更严的兼容口径。
- 改法：§3.2.1 新增显式开关 `paged=1`（只开扩展响应，不改 `after` 的查询语义），`first_id`/`has_more` 只在带
  `paged`/`before`/`phase`/`kind`/`failures` 之一时出现，并写明 handler 里决定扩展与否的那个布尔；§3.3.1 实时模式改为
  `after=lastId&paged=1`；`api.js` 的 `taskEvents` 签名保留、只加可选第四参。§3.2.1 开头的矛盾句改为「兼容契约」一段。
- AC-2 `TestTaskEventsLegacyShapeUnchanged` 改写：「新增的键只多不少」删除，改为不带新参数时键集合**恰为** `{events, last_id}`（与 f01 同一条契约），
  带 `paged=1` 时恰为四个键；新测试是独立函数，本线**不修改** `TestTaskEventsEndpoint`，与 f01 在同一函数里不产生文本冲突。
- §3.7 f01 行新增 (c)、§6 冲突行、§7-6 PR 描述都补上这一处测试接缝。f01 PRD 不需要改动：它要求的是「不带新参数时恰为两个键」，本线 v5 正好满足。

**B2 v-html 白名单自引用：接受，用排除参数，白名单留在 `web/src/testdata/`。**
- 核实并实测：在 base 的 `web/src` 副本里放进按原规则生成的白名单，`grep -rn 'v-html' web/src` 得 24 行（12 行源码 + 白名单自身 12 行），
  加 `--include='*.vue' --include='*.js' --exclude='*.test.js' --exclude-dir=testdata` 后得 12 行，与白名单相等，收敛。
- AC-7 写死完整命令（含归一用的 `sed`），并说明每个参数排除的是什么；§3.5-4 引用同步。没有把白名单移出 `web/src`：AC-6 的
  `failure-cases.json` 已经约定放在 `web/src/testdata/`，两份 fixture 放在一处便于找，靠 `--exclude-dir=testdata` 一并排除即可。
- `--include` 白名单式扫描会漏掉以后新增的 `.ts`/`.jsx`，所以同一目标另加一条「`web/src` 出现 `.ts/.tsx/.jsx/.html` 即失败并提示扩展 `--include`」，
  防止守卫被新文件类型悄悄绕过（base 上 `web/src` 只有 `.vue`、`.js`、`.css`，实测）。

**B3 AC-5 用例数据与路径校验矛盾：接受。**
- 核实：路径校验第 2 条要求 `filepath.Clean(log_ref)` 以 `verify-logs/task-<id>/` 开头；真实的多段 `log_ref` 是若干**完整**相对路径以空格连接
  （`heavy.go:349-378` 的 `strings.Join(refs, " ")`，每个 ref 来自 `verifylog.go:88` 的 `filepath.Join(l.rel, name)`）。v4 的
  `"a.log b.log"` 与 `"round-0/01-repro_fail.log …"` 会先被第 2 条拒成 404。
- 改法：「多段」用例与「重试覆盖」用例 1、4 全部改为 `verify-logs/task-<id>/round-<n>/<NN>-<step>.log`；用例 1 的「指向它」写成完整路径。
  另把 `"a.log b.log"` 保留为**负例**（每一段都 404），反过来钉住「多段的每一段单独过路径校验」，而不是放宽校验。
  用例 4 额外把两个文件的 `ModTime` 设到 `vid1.at` 之前，使 `part=0` 的 404 只能来自库内判定，`part=1` 的 200 不被时间判定干扰。

**B4 「在日志中定位」在超过 200 个失败时跳错：接受，采用 `lastFailure`，并补上截断后的 ↑/↓。**
- 核实：v4 中按钮唯一的数据源是 summary 的 `failures`，按 id 升序取前 200 条，截断时不含最后一个失败；「↓」也只在该数组里走。
- 改法：summary 新增 `firstFailure`、`lastFailure`（各一条 `ORDER BY id ASC|DESC LIMIT 1`，与截断无关；只算 severity=error，警告不会导致任务失败）
  和 `failureCount`；失败卡片按钮只读 `lastFailure`。选它而不选「头 100 + 尾 100」：后者仍会让中间的失败在导航里断档，而且「最后一个」
  仍要从数组末项推断。为了让截断后 ↑/↓ 能走到底，`/events` 新增 `failures=1` 过滤（与 summary 共用同一个判定 SQL 常量），导航以当前失败的 id
  为游标向服务端要下一条，不受 200 上限约束。
- AC-4 的 205 条用例改为：断言 `lastFailure.id` 等于用独立 SQL 算出的 `max(id)`，并且 `> failures[199].id`；末尾插一条 id 最大的 overflow 警告，
  断言它不是 `lastFailure`；无失败时为 `null`。AC-2 新增 `TestTaskEventsFailuresParam`。

**B5 过滤与时间相关的纯函数没有可机器验证的用例：接受。**
- 核实：v4 的 AC-6 没有 `applyFilter`、`formatOffset` 的用例；`phaseMeta` 只在 AC-7 里 grep 一个字符串；≥30s 静默分隔线的判定写在 TaskDetail 的模板里，
  测不到。
- 改法：§3.3.1 把四者的取值写死（`phaseMeta` 的 order 表与 `fix-N` 的 `sub`，新增 `comparePhases`；`formatOffset` 的格式与非法输入；
  `applyFilter` 的交集语义、「子块整块保留不裁剪」、全关时逐项 `===`），并把静默判定收成纯函数 `silenceLabel(gapMs)`（TaskDetail 不再自己比较阈值）。
  AC-6 补上四组表驱动用例：`applyFilter` 6 条（含 subagent 子块整块保留与整块去掉）、`fix-2`/`fix-10` 按数值排序与 `implement-reentry` = 1.5、
  `formatOffset` 11 个边界值、`silenceLabel` 5 个值加一条与 `withTiming` 的组合用例。AC-9 截图新增 ④b「只看失败」。
- 先红：评审期在 Node v22 上实测，命名导入（`import { phaseMeta } …`）在导出不存在时整份文件报 `SyntaxError: … does not provide an export named`，
  一条用例都不跑，得不到 `is not a function`；所以 AC-6 规定测试文件一律命名空间导入（`import * as R`），在提交 1 上逐个报
  `TypeError: R.<name> is not a function`、其余用例照跑。证据文件必须对 8 个新导出名各出现至少一次 `is not a function`，且不含 `SyntaxError`/`ERR_MODULE_NOT_FOUND`。

**与邻居的接缝**：B1 是本轮唯一涉及邻居的一条，结论是 f01 的 PRD 与测试**一字不改**即可成立（§3.7 f01 行 (c)）。其余四条只改本线的接口、纯函数和 AC，
对 f03/f06/f12 的口径没有影响。`failures=1`、`paged=1` 两个新参数都包在 `ownedTask` 之后，不改变非属主 404 的行为，f12 迁移可见性判定时不需要额外处理。

---

## 9. 实现与验收结果

### 9.1 落地清单

| 部分 | 文件 |
|---|---|
| 迁移 0041（放宽 phase CHECK） | `migrations/0041_agent_events_reentry_phase.{up,down}.sql` |
| store 只读查询：过滤/反向分页/汇总/失败判定 SQL、日志引用与覆盖判定 | `internal/store/replay.go` |
| `/events` 扩展、`/events/summary`、`ownedTask` | `internal/httpapi/api.go` |
| 验证日志按块读取（路径校验、安全切点、覆盖判定） | `internal/httpapi/verification_log.go` |
| `runner.RedactForDisplay`（只新增，不改既有行） | `internal/runner/redact.go` |
| 装配：`API.LogDir` / `API.KnownSecrets` | `cmd/lathe/main.go` |
| 纯函数与事件窗口 | `web/src/replay.js`（测试 `web/src/replay.test.js`） |
| 日志面板与验证区 | `web/src/views/TaskDetail.vue`、`web/src/components/AgentEventItem.vue`、`web/src/components/VerifyLogViewer.vue` |
| 共享 fixture / 白名单 | `web/src/testdata/{failure-cases.json,pair-group-*.json,v-html-allowlist.txt}` |
| `make ui-test` | `Makefile` |

提交顺序按 AC-5、AC-6 的要求拆分：日志接口分 A（归属+路径+定长分块）/ B（安全切点）/
C（覆盖判定）三个提交；前端分「提交 1 纯抽取」（`f54a4ee`）与「提交 2 回放窗口」。

### 9.2 与 PRD 的出入（如实记录）

- **`parseLogRef` 多段跨轮**：PRD 只规定「任何一段不符合格式时返回 `null`」。实现另把
  「多段不在同一个 `round-N`」也当作 `null`（真实数据里复现阶段的多段总在同一轮，跨轮只可能是
  手工数据），属同一「不猜」立场的收紧，有用例。
- **`proofOf` 的返回形状**：PRD 写的是 `red / green / regression / light` 与总判定。实现返回
  `{steps, red, green, regression, light, verdict}`，`verdict ∈ proven | passed | failed | unknown`；
  `proven` = 总判定通过且本轮有 `repro_fail`（heavy 证明立住），`passed` = light 档通过。
- **`createEventWindow` 的实时首拍**：非终态任务首拍同样先取**最后一页**（`before=∞`），之后才
  `after=lastId` 增量；PRD 只写了终态的这一行为。这样打开一个已经跑了上千条的进行中任务，
  也是先看到当前现场，而不是从头拉 200 条再追。
- **定位与本地开关**：`jumpTo` 的目标被「只看工具调用」等本地开关藏起来时，自动关掉开关再滚动；
  被阶段 chip（服务端过滤）挡住时清掉 chip 再取一次。PRD 没写，属交互细节。
- **分页游标写成行比较**：§3.1 写的查询形状是 `id <|> $3 ORDER BY id`。全量 `make test` 下
  `TestEventsQueriesUseTaskIndex` 抓到：`id < $before ORDER BY id DESC LIMIT n` 被规划成主键倒序
  范围扫描再按 `task_id` 过滤（大表上等于从全表最新的事件往回翻）。实现改为
  `(task_id, id) < ($1, $before) ORDER BY task_id DESC, id DESC`，语义不变，只有复合索引能承接。
  测试同时加固：用真实形态的参数（本任务 `max(id)+1`），事务内造跨任务数据并 `ANALYZE`，
  旧写法上稳定复现主键扫描（`red/ac4-explain-hardened.txt`）。
- **AC-9 手测里的一次 fail-closed**：种子数据把 `verifications.at` 回拨到 08:00，而日志文件是
  当下写的，第一次 curl 时覆盖判定的时间规则如实返回 `404 log_overwritten`。把种子日志的 mtime
  设为各自 `at - 1s` 后复测为 200。这正是 §3.2.3 规则 2 的预期行为，不是缺陷；记在这里，
  免得以后有人造数据时踩同一个坑。

### 9.3 验收结果（逐条）

证据在本线本地目录 `/opt/lathe-wt/evidence/f04/`（`red/` 为先红、`green/` 为后绿）。

| AC | 结果 | 证据 |
|---|---|---|
| AC-1 0041 / 每个 phase 可落库 / 常量绑定 | 先红：`implement-reentry` 报 SQLSTATE 23514（store 与 httpapi 两条）；后绿。空数据上 up→down→up 退出码 0，down 后约束与 0015 一致 | `red/ac1.txt`、`green/ac1-migrate.txt` |
| AC-2 反向分页 / 兼容契约 / failures=1 | 先红：`before` 被忽略（got `[56..155]` want `[206..305]`）、`paged=1` 无 `first_id`、`paged=2` 返回 200；后绿。不带新参数时顶层键恰为 `{events,last_id}`（新测试函数，未改 `TestTaskEventsEndpoint`） | `red/ac2-ac4.txt`、`green/ac2-ac4.txt` |
| AC-3 phase/kind 过滤与隔离 | 先红：`phase=verify` 返回其他阶段；后绿。非属主带扩展参数仍 404 `{"error":"任务不存在"}` | 同上 |
| AC-4 summary / EXPLAIN | 先红：路由不存在；后绿。205 条失败时 `failures` 200 条、`failuresTruncated=true`、`failureCount=205`、`lastFailure.id` = 独立 SQL 的 `max(id)` 且大于 `failures[199].id`。六条查询的 EXPLAIN 都走 `agent_events_task_id`（游标改行比较的经过见 §9.2） | 同上，及 `red/ac4-explain-hardened.txt`、`red/gates-first-run-explain-fail.txt` |
| AC-5 日志读取 | 三段先红：base 路由不存在（`red/ac5-base.txt`）；提交 A `6e0e92d` 上切分用例 1–4 报半截凭据、5/6 状态码不符（`red/ac5-commitA.txt`）；提交 B `a5acaf8` 上覆盖用例 1、2、4 返回 200 并带出 `RUN-2`（`red/ac5-commitB.txt`）。后绿：40 个子用例全过 | `green/ac5.txt` |
| AC-6 前端纯函数 | 提交 1 实现上 `node --test` 退出码 1（1 过 98 挂），8 个新导出各自出现 `is not a function`，无 `SyntaxError`/`ERR_MODULE_NOT_FOUND`；后绿 99/99。五个变异补丁都让规定用例失败 | `red/ac6-commit1.txt`、`red/segmentRuns-mut-*.patch`、`red/ac6-mutation-*.txt`、`green/ac6.txt` |
| AC-7 渲染契约 | `make ui && make build` 0；`make ui-test` 白名单一致（12 行，本线未新增）；在日志查看器里临时加一行 `v-html` → 退出非 0 并打印 diff；`logRef` 在 TaskDetail 命中 3 次（base 为 0）；`implement-reentry` 在 replay.js 命中 | `green/ac7.txt`、`red/ac7-vhtml-mutation.txt` |
| AC-8 不越界与装配 | 越界清单 diff 为空；migrations 只有 0041；`internal/runner/` 只改 `redact.go`（仅新增）与 `redact_test.go`；装配守卫先红 `missing keys [LogDir KnownSecrets]` 后绿；`go test -race -p 1 ./internal/httpapi/...` 通过 | `red/ac8-wiring.txt`、`green/ac8-*.txt` |
| AC-9 端到端手测 | 真实二进制 :8404：①「共 1200 条，已加载 200 条」；②「第一个」跳到 `#ev-2367`、阶段与输出已展开并高亮；③ 新标签页打开同一 URL 定位到同一行；④ chip「修复第 1 轮」只剩该阶段；④b「只看失败」只剩失败行与含失败的子块（子块内 3 行都在）；⑤ 两轮证明条、第 1 轮「绿 ✗」红框；⑥ 日志查看器两块、可见 `[REDACTED]`、无明文口令；⑦ 另一账号页面与 5 个接口全部 404「任务不存在」；装配实证 vid 返回 200；⑧ 手动重试显示两次运行、旧运行步骤标「日志已覆盖」无按钮，curl 旧 vid 404 `log_overwritten`、新 vid 200；⑨ rebase 重验显示两次运行、无「修复第 1 轮之后」；2000 条窗口全部折叠时 DOM 115 个节点，展开阶段（输出折叠）10116 个、`<pre>` 为 0。清理后 `implement-reentry` 计数为 0，收尾 down/up 退出码 0 | `ac9-*.png`、`green/ac9-*.txt`、`ac9-seed-ids.txt` |

### 9.4 已知边界（汇总，均为已接受的代价）

- **时间粒度**：`at` 是入库时刻，EventSink 200ms 批量刷库，误差约 ±200ms；界面悬停写明，小于 1s
  的间隔不显示。
- **日志脱敏边界**：读取时拿不到那一轮的 `StackEnv`，隔离栈临时口令只靠形态层识别 `user:pass@`
  DSN；长于 `redactContext`（16KiB）且跨行的单个形态匹配不受切点检验保护。
- **超长单行**：1MiB 内找不到安全切点时整段不返回（`withheld`），原文只能去服务器看。
- **轮次与运行推断**：依赖 runner 的两条执行纪律（§6）。残余盲区是用例 16：无事件、无 `log_ref`、
  light 档、上一次全 passed 时两次运行会并成一轮，此时界面标「按顺序推断」且**不给总判定**。
- **被覆盖的旧日志不可读**：重试复用日志路径，旧运行的日志在界面上只显示 status、不给入口；
  根治需要写入侧按运行分目录（§4，建议另开 `fix/verify-log-per-run`）。
- **0041 只救未来**：0041 之前被拒收的 `implement-reentry` 事件已经丢失，无法补回。

### 9.5 门禁

`LATHE_TEST_REQUIRE_DB=1 make lint && LATHE_TEST_REQUIRE_DB=1 make test` 通过；`gofmt -l`
（含未入库 `.go`）为空；`golangci-lint run --new-from-rev=origin/dev` 0 issues；
`make ui && make build`、`make ui-test` 通过；`lathe_migrate up`、`lathe_migrate down && lathe_migrate up`
退出码 0。原样输出见 `green/gates.txt`。

