# 收件箱：待人处理事项聚合

> 状态：**已实现**（分支 `feat/inbox`，PR 待开）。本文是 f02 PRD 的定稿版，只取 §1–§7。
> 引用约定：代码位置写「文件 + 符号名」，不写行号（与 `docs/08-debt-cleanup.md`、`docs/05-roadmap.md` 一致）。

---

## 1. 问题与目标

### 1.1 为什么 Lathe 需要它

Lathe 的产品承诺是「issue 进来，**已验证的** PR 出去」。这条流水线有五个地方是
**刻意停下来等人**的。它们都是产品边界或验证体系本身的一部分，不是缺陷：

| 停点 | 为什么必须停 | 依据 |
|---|---|---|
| `blocked_spec` | 单子不明确就回帖提问并停下，不猜（铁律 §1.2） | `internal/runner/pipeline.go` 的 `stageTriage` |
| `awaiting_approval` | manual 档验证已过，等人放行才推分支开 PR | `internal/runner/pipeline.go` 的 `gateBeforePush` |
| `failed` | D4 不自动重试，留现场等人处置 | `internal/task/state.go` 的 `transitions`、`internal/runner/pipeline.go` 的 `failWithExcerpt` |
| `review_feedback` 停车（parked） | 约束①：二轮必须 resume 原会话，没会话或现场坏了就不能自动派发 | `internal/runner/review.go` 的 `ReviewIntake.AcceptReview` |
| PRD 逐节签字 / 回答 / 批准 | D10-4：签字后冻结，签字必须是人 | `internal/prd/checklist.go` 的 `checkSections`、`internal/prd/state.go` 的 `transitions` |

现在这些停点散在四个地方：看板的状态筛选（`web/src/views/Board.vue` 的筛选按钮）、
任务详情页的闸门卡片（`web/src/views/TaskDetail.vue`）、PRD 会话侧栏的
状态圆点（`web/src/views/prd/PRDChatLayout.vue`），以及邮件
（`internal/runner/notify.go` 的 `stateSubject`，只覆盖任务、不覆盖 PRD，而且依赖 SMTP 已配置）。
人要知道「现在有几件事在等我」，得把这几处挨个看一遍。

停点没人看见，效果和流水线坏了一样。`blocked_spec` 的提问没人回，单子就一直停在那；
`awaiting_approval` 没人放行，已经验证过的改动就一直不出 PR；PRD 的 🤖 节没人确认，
就永远提交不了评审。没人处理的停点，等于把「已验证 PR」的交付时间往后拖了不知多久。

### 1.2 目标

- **G-1** 一个页面列出**当前登录用户**名下所有在等人处理的事项。每项能一键跳到能处理它的页面。
  判据：AC-2、AC-5、AC-17。
- **G-2** 顶栏显示未读计数徽标，已读/未读可切换。判据：AC-8、AC-9、AC-17。
- **G-3** 事项**全部从现有状态与事件派生**，不新建第二份「谁在等什么」的真相源。
  新表只存「这个人看过没有、忽略了没有」这一份视图状态。判据：AC-11、AC-12。
- **G-4** 按属主隔离，非属主一律 404。判据：AC-6。
- **G-5** 不改状态机、不削弱验证、不加依赖、不加环境变量。判据：AC-11、AC-15、AC-16。

### 1.3 与「已验证 PR」定位的关系

收件箱**只读**状态，不提供任何新的状态转移入口。处理动作（放行、重试、重来一轮
review、回答、确认节、批准）仍然走原来的详情页和原来的端点，详情页上的上下文
（验证报告、重试预览、复核发现）也都还在。收件箱只负责把人领到那个页面。
所以它不会成为一条绕过验证或闸门的捷径（铁律 §1.1）。

---

## 2. 现状取证

> 代码位置按仓库约定写「文件 + 符号名」，不写行号（行号随每次合入漂移）。

### 2.1 任务侧：等人的状态与它们的出口

| 事实 | 位置 |
|---|---|
| 12 个状态常量；`blocked_spec` / `blocked_dep` / `awaiting_approval` / `review_feedback` / `failed` | `internal/task/state.go` 的 `State` 常量块、`AllStates` |
| 转移表：`blocked_spec → {queued, cancelled}`；`blocked_dep → {queued, cancelled}`；`awaiting_approval → {implementing, queued, cancelled}`；`failed → {queued}` | `internal/task/state.go` 的 `transitions` |
| `review_feedback → queued` 只允许 webhook 和显式的人工出口走，通用重试不许走 | `internal/task/state.go` 的 `transitions` 注释第 6 条；`internal/httpapi/api.go` 的 `retryTask` |
| 约束①：`RequiresSession(review_feedback, implementing\|queued)` | `internal/task/state.go` 的 `RequiresSession` |
| 人工出口 `ExplicitReviewRestart` 只认 `review_feedback → queued` | `internal/task/machine.go` 的 `TransitionOpts.ExplicitReviewRestart`、`Machine.transition` |
| **每次转移都在同一个事务里写一行 `task_events`**（from/to/actor/payload/at） | `internal/task/machine.go` 的 `Machine.transition` → `insertEvent`；表定义 `migrations/0001_init.up.sql` 的 `task_events` |
| **但 `task_events` 不全是转移**：`ClaimForReap(commit=true)` 对终态任务写一条 `from_state = to_state` 的记账事件（payload `kind=worktree_reaped`）；建任务的首条事件 `from_state IS NULL` | `internal/task/machine.go` 的 `Machine.ClaimForReap`（注释「非状态转移事件」）、`Machine.Create` |
| 收割机默认开启，TTL 72h、每小时一轮，只收终态（含 `failed`） | `internal/config/config.go` 的 `LATHE_REAP_ENABLED` 默认值；`cmd/lathe/main.go` 的收割机装配 |
| 转移表里没有自环（没有任何状态的出边包含自己），所以 `from_state IS DISTINCT FROM to_state` 恰好等价于「这是一次真正的转移」 | `internal/task/state.go` 的 `transitions`、`Validate` |
| `task_events` 只有 `(task_id, at)` 索引 | `migrations/0001_init.up.sql` 的 `task_events_task_at` |
| 读事件流按 `ORDER BY at, id`（同一事务 now() 相同，靠 id 破平） | `internal/task/machine.go` 的 `Machine.Events` |
| `blocked_spec` 的入口：分诊判不明确，payload 带 `question`；另一入口是验证阶段的需求矛盾（复现测试没先红） | `internal/runner/pipeline.go` 的 `stageTriage`、`stageVerify` |
| `awaiting_approval` 的入口：`gateBeforePush`，payload 带 `gate_mode`/`reason` | `internal/runner/pipeline.go` 的 `gateBeforePush` |
| `failed` 的入口：`failWithExcerpt`，写 `failure_reason`（阶段标签 + cause）、`failure_stage` | `internal/runner/pipeline.go` 的 `failWithExcerpt` |
| **`tasks.failure_reason` 写入时没有脱敏**：`failWithExcerpt` 拼 `stage.label() + cause` 后原样写入；验证失败时 cause 含 `f.Err` 与 `report.Summary()`。`redactSecrets` 只作用于写进 `failure_digests` 的那一份副本。其它无脱敏写入点：分诊 `verdict.Reason`、验证阶段的 `blocked_spec`、mergepoll 的 rebase 跟进、`cmd/lathe/queue.go` 写 cancelled 的原因（收件箱不展示 cancelled）；`blocked_dep` 的原因就是前驱的同一个 reason | `internal/runner/pipeline.go` 的 `failWithExcerpt`、`stageTriage`、`stageVerify`；`internal/runner/memory.go` 的 `recordFailureMemoryFor`；`internal/runner/mergepoll.go`；`internal/task/machine.go` 的 `PropagateBlocked` |
| 既有界面已原样展示该字段（存量欠账，非本功能引入） | `web/src/views/TaskDetail.vue` 的失败卡片、`web/src/views/Board.vue` 的失败行 |
| 脱敏实现：两层，层 1 精确替换平台已知凭据值（`config.KnownSecrets`），层 2 按形态（`ghp_`/`lin_api_`/`sk-`/`xox?-`…）兜底；入口 `redactText` 原本未导出 | `internal/runner/redact.go` 的文件头注释、`tokenShapes`、`redactText`；`internal/config/config.go` 的 `Config.KnownSecrets` |
| `blocked_dep` 的入口：`PropagateBlocked`，payload 带 `blocked_by`（失败的那个祖先任务 id） | `internal/task/machine.go` 的 `Machine.PropagateBlocked`；调用方在 `internal/runner/pipeline.go`、`internal/runner/mergepoll.go` |
| `review_feedback` 停车的两个原因：现场不可用 / 无会话 | `internal/runner/review.go` 的 `ReviewIntake.AcceptReview`；状态常量 `ReviewParked` |
| 停车任务的人工出口 = `POST /api/tasks/{id}/review-retry` | `internal/httpapi/api.go` 的 `reviewRetryTask` |
| 人工放行 = `POST /api/tasks/{id}/approve`（`TransitionFrom` 带前置状态） | `internal/httpapi/api.go` 的 `approveTask` |
| 人工重试走**原任务行**，不新建（任务 #313） | `internal/httpapi/api.go` 的 `retryTask` |
| issue 重开 / 内置工单重新开跑会**新建任务行**，旧 `failed` 行留着 | `migrations/0030_failure_digests.up.sql` 的头注释 |
| 同一工单只允许一个活任务（部分唯一索引） | `migrations/0021_internal_issues.up.sql` 的 `tasks_one_active_per_item` |
| `tasks` 表（本功能之前）**没有任何以 `user_id` 开头的索引**；按属主查询全表扫 | `migrations/0001_init.up.sql`、`0016_flow_orchestration`、`0021_internal_issues` 的全部 `CREATE INDEX` |
| 任务没有标题列；内置工单的标题在 `issues.title`，`ListTasks` 已有 LEFT JOIN 先例 | `internal/store/query.go` 的 `Store.ListTasks`；`migrations/0021_internal_issues.up.sql` 的 `issues` |

### 2.2 PRD 侧：等人的状态

| 事实 | 位置 |
|---|---|
| PRD 6 个状态；`awaiting_answers` = 球在人这边；`ready_for_review` = 等人批准 | `internal/prd/state.go` 的 `State` 常量块 |
| 转移表 | `internal/prd/state.go` 的 `transitions` |
| 一轮跑完：有问题 → `awaiting_answers`，没问题 → `drafting` | `internal/prd/service.go` 的 `Service.persistRound` |
| 自动续推：没问题就接着跑下一轮，直到定稿；`processing=true` 期间人不需要做什么 | `internal/prd/service.go` 的 `Service.chain` |
| 节状态 `confirmed / inferred / pending`；🤖 inferred = 「待人过目」 | `internal/prd/document.go` 的 `SectionStatus` |
| 十个节的固定 key 与顺序（`Sections()`） | `internal/prd/document.go` 的 `Document.Sections` |
| 进评审的闸门：有 inferred 就是 `CodeUnconfirmed`，有 pending 就是 `CodePendingSection` | `internal/prd/checklist.go` 的 `checkSections` |
| 人确认一节 = `POST /api/prds/{id}/sections/{key}/confirm` | `internal/httpapi/prds.go` 的 `PRDAPI.confirmSection` |
| 后台轮次失败：`last_error` / `last_error_input`，下一次抢占时清空 | `migrations/0031_prd_run_failure.up.sql`；`internal/store/prds.go` 的 `Store.FailPRDRun`、`Store.BeginPRDRound` |
| `processing_started_at` 在 `EndPRDRound` 时**置 NULL**，不能当失败锚点 | `internal/store/prds.go` 的 `Store.EndPRDRound` |
| `prd_rounds` 只追加，`(prd_id, round)` 唯一 | `migrations/0024_prd_rounds.up.sql` |
| `ListPRDs` 不返回 document（防止列表页一页传几兆） | `internal/store/prds.go` 的 `Store.ListPRDs` |
| PRD 一律 `user_id` 隔离，`ErrPRDNotFound` 不区分「不存在」和「不是你的」 | `internal/store/prds.go` 的 `ErrPRDNotFound` |
| 前端跳节函数 `gotoSection(key)`，DOM id 为 `sec-<key>`，已 `defineExpose` | `web/src/components/prd/PRDDocPanel.vue` 的 `gotoSection` |

### 2.3 隔离、鉴权、前端外壳

| 事实 | 位置 |
|---|---|
| 读接口全部按 `CurrentUser` 隔离，管理员也不例外，不留跨用户后门 | `internal/httpapi/api.go` 的 `API.Routes` 注释 |
| 非属主与不存在同样回 404 | `internal/httpapi/api.go` 的 `taskDetail`、`reviewRetryTask`；`internal/store/query.go` 的 `Store.TaskDetail` |
| `RequireFunc` 未登录 401 | `internal/httpapi/auth.go` 的 `Auth.Require` |
| actor 格式 `user:<id>` | `internal/httpapi/auth.go` |
| 路由装配处 | `cmd/lathe/main.go` 的 `serve` |
| 前端路由表 / 顶栏导航 | `web/src/main.js` 的 `createRouter`；`web/src/App.vue` 的 `<nav>` |
| 看板轮询先例：5 秒 `setInterval` | `web/src/views/Board.vue` 的 `onMounted` |
| 统一请求封装（401 抛 `UnauthorizedError`，非 2xx 带 `status`/`data`） | `web/src/api.js` 的 `request` |

### 2.4 测试与迁移基础设施

| 事实 | 位置 |
|---|---|
| `make test` = `go test -p 1 ./... -count=1` | `Makefile` 的 `test` 目标 |
| CI：`migrate up` → `migrate down` → `migrate up` → 测试，且 `LATHE_TEST_REQUIRE_DB=1` | `.github/workflows/ci.yml` |
| `migrate down` 一次只回滚最高的一个版本 | `internal/store/migrate.go` 的 `Store.MigrateDown` |
| 非事务迁移标记 `-- lathe:no-transaction`，up/down 各标各的 | `internal/store/migrate.go` 的 `noTransactionMarker` |
| CONCURRENTLY 建索引的标准写法（先 DROP IF EXISTS，建完用 DO 块自检 indisvalid） | `migrations/0018_agent_events_result_index.up.sql` |
| API 测试夹具：`apiServer` / `do` / `authAs` / `mustUser` | `internal/httpapi/api_test.go` |
| 截断按 rune 的既有实现 | `internal/integration/agent/agent.go` 的 `truncate` |
| **`internal/store` 的同包测试 import 不了 `task`**：`task` → `tracker`（用 `store.Store`）→ `store`，同包测试里 import `task` 会被 `go vet` 判 `import cycle not allowed in test` | `internal/task/machine.go` 的 import、`internal/tracker/local.go` |
| 一个 import `store`+`prd` 的新包，其测试文件可以同时 import `task`、`store`、`runner`、`testsupport`；`task`/`runner`/`prd` 都不依赖这个新包 | `go list -deps ./internal/runner ./internal/task ./internal/prd` |
| 共享的真库连接工具在 `internal/testsupport`（`DSN`、`ConnectContext`、`SkipOrFail`），本包不 import `store`；`store.Store.Pool()` 已导出 | `internal/testsupport/db.go`、`internal/store/store.go` |
| 本功能之前迁移最大号 0031；本功能使用 0035、0036（0037 保留不用） | `migrations/` |

---

## 3. 方案

### 3.1 核心思路：事项是派生出来的，只有「看过没有」需要存

一条收件箱事项 = 一个**主体**（任务或 PRD）加上它的一次**停留**（进入当前等人状态的那一刻）。

- **主体和停留都从现有数据实时算出来**，不存。任务状态的真相源仍然是 `tasks.state`
  和 `task_events`，PRD 的仍然是 `prds`。收件箱**不复制状态**，所以不会有两份真相对不上。
- 唯一要落库的是**每个人对每条事项的视图状态**：读过没有、忽略了没有。这份信息在别处
  找不到，不算冗余。
- 停留要有一个稳定的**事项键（item key）**。同一主体再次进入等人状态时（例如失败 →
  重试 → 又失败），键必须变化，这样它会重新变成未读。

#### 3.1.1 事项键与锚点

| 主体 | 键格式 | 锚点为什么稳定、为什么会变 |
|---|---|---|
| 任务 | `task:<taskID>:e<eventID>` | `eventID` 取该任务最新一条**真正的转移事件**，即 `from_state IS DISTINCT FROM to_state` 的那一行（排序与 `Machine.Events` 相反：`at DESC, id DESC`）。每次转移都在同一事务写事件（`Machine.transition` → `insertEvent`），所以这一行的 `to_state` 就是当前状态，`at` 就是进入当前状态的时刻。**不能取「最新一条事件」**：`ClaimForReap` 会对终态任务追加 `failed→failed` 的记账事件（`Machine.ClaimForReap`，收割机默认开启），取最新事件的话，已读/已忽略的 `task_failed` 在现场回收后会换键、重新以未读出现。`IS DISTINCT FROM` 同时覆盖建任务首条事件 `from_state IS NULL` 的情况（NULL 与任何状态都 distinct，算一次进入）。将来别的功能线再往 `task_events` 里追加同状态事件（例如插话、回放的记账），锚点也不受影响。再进入等人状态时是一次新的转移、新的一行，键随之改变 |
| PRD | `prd:<prdID>:<kind>:r<round>` | `round` 只增不减（prd_rounds 只追加）。新的一轮问了新问题、写出了新的 🤖 节，键就会变 |
| PRD 运行失败 | `prd:<prdID>:run_failed:r<round>:<h8>` | `processing_started_at` 跑完会被置 NULL（`Store.EndPRDRound`），不能当锚点。所以在轮次号之外再拼 `sha256(last_error)` 的前 8 个十六进制字符，同一轮第二次失败、原因不同时就是新事项 |

键的语法是封闭的。服务端用正则 `^(task:[1-9][0-9]{0,18}:e[1-9][0-9]{0,18}|prd:[1-9][0-9]{0,18}:(answers|review|confirm|run_failed):r[0-9]{1,9}(:[0-9a-f]{8})?)$`
校验，长度不超过 128。不合法直接 400，既不查库也不回显输入。

#### 3.1.2 事项种类（`kind`）与派生规则

每个主体**最多出一条**事项（主体在同一时刻只有一个状态）。

| kind | 派生条件 | 跳转 | 人在目标页做什么 | 可忽略 |
|---|---|---|---|---|
| `task_blocked_spec` | `tasks.state='blocked_spec'` | `/tasks/<id>`；内置工单额外给 `/issues/<issueId>` | 去工单补充需求后重新指派 / 启动 | 否 |
| `task_awaiting_approval` | `state='awaiting_approval'` | `/tasks/<id>` | 看验证报告后「确认开 PR」 | 否 |
| `task_review_parked` | `state='review_feedback'` | `/tasks/<id>` | 「重来一轮 review」或取消 | 否 |
| `task_blocked_dep` | `state='blocked_dep'` | `/tasks/<id>`；另给前驱 `/tasks/<predecessor_id>`（仅当前驱存在且也属于本人时才给；归属在 SQL 里判，见 §3.3 `predecessor_id`） | 处理前驱，或取消本任务 | 否 |
| `task_failed` | `state='failed'` **且** 它是同一工单 `(repo_id, tracker_provider, external_key)` 在本人名下**最新**的任务行 | `/tasks/<id>` | 重试 / 从头重跑 / 放弃 | **是** |
| `prd_run_failed` | `prds.state IN ('drafting','awaiting_answers') AND NOT processing AND last_error IS NOT NULL` | `/prds/<id>` | 看失败原因后重发 | 否 |
| `prd_answers` | `state='awaiting_answers' AND NOT processing`（且不满足上一行） | `/prds/<id>` | 回答问题 | 否 |
| `prd_review` | `state='ready_for_review'` | `/prds/<id>` | 批准 / 打回 | 否 |
| `prd_confirm` | `state='drafting' AND NOT processing AND last_error IS NULL`，且 document 里至少一节 `status='inferred'` | `/prds/<id>?section=<第一个 inferred 节的 key>` | 逐节确认 | 否 |

PRD 的优先级是 `run_failed > answers > review > confirm`，一份 PRD 只取第一条命中的。

**为什么 `review_feedback` 一律算「停车」**：webhook 回流是在同一个处理函数里先
`pr_open → review_feedback` 再 `review_feedback → queued`（`ReviewIntake.AcceptReview`），
mergepoll 的归一也是连着走两步（`internal/runner/mergepoll.go`）。所以 review_feedback
能被看见的时间，只有两次 UPDATE 之间的几毫秒，或者真的停了车。偶尔看到一闪而过，
代价是一次多余的徽标 +1，下一轮轮询就消失。这比为了区分停车再加一个持久标记便宜，
后者就是冗余真相源。

**为什么 `task_failed` 要排除被取代的行**：issue 重开后会新建任务行
（0030 迁移头注释），旧的 failed 行永远是 failed（failed 只能出到 queued）。
不排除的话，收件箱会无限积累已经没有意义的失败。

**为什么只有 `task_failed` 能忽略**：其它种类都卡在一道闸门或一个问题上。忽略它们
等于把一个永远不会自己走的任务藏起来，收件箱就成了制造「卡排队」的地方
（AGENTS.md §9「任何失败都要落状态」的同类风险）。它们离开收件箱的唯一途径是状态变化。
failed 是终态，人可能就是决定不管它了，所以给它一个出口。忽略也按键记录，
同一任务再失败一次是新键，会重新出现；而现场被收割（同状态记账事件）**不**换键，
忽略不会被系统自己撤销（§3.1.1，AC-7b 守着）。

**不纳入的状态**及理由见 §4。

### 3.2 数据模型（迁移 0035、0036）

#### 0035_inbox_marks（建表，事务内）

```sql
-- 收件箱的视图状态：某人对某条派生事项「读过 / 忽略过」。
-- 事项本身不入库（从 tasks/task_events/prds 实时派生），这里只存别处没有的那一位信息。
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
```

Down：`DROP TABLE inbox_marks;`（触发器和索引随表一起删）。丢掉的只是已读/忽略状态，
回滚后收件箱全部变回未读。这是可接受的信息损失，在 down 文件注释里写明。

**表的规模有上界**：每次写标记时，在同一事务里删掉**同一用户、同一主体、键不同**的
旧标记（旧键对应的停留已经结束，不会再显示）。所以每人每个主体最多一行，表的量级
不超过 `tasks + prds`。

#### 0036_tasks_user_state_index（非事务，CONCURRENTLY）

```sql
-- lathe:no-transaction
DROP INDEX IF EXISTS tasks_user_state;
CREATE INDEX CONCURRENTLY tasks_user_state ON tasks (user_id, state);
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
                 WHERE c.relname = 'tasks_user_state' AND i.indisvalid) THEN
    RAISE EXCEPTION 'tasks_user_state 未建成或处于 INVALID 状态，需重跑本迁移';
  END IF;
END $$;
```

Down：首行写 `-- lathe:no-transaction`，然后 `DROP INDEX CONCURRENTLY IF EXISTS tasks_user_state;`。

为什么要加这个索引：收件箱徽标每 30 秒被每个打开的标签页查一次，查询的形状是
`WHERE user_id=$1 AND state = ANY(...)`。`tasks` 现在没有以 user_id 开头的索引
（§2.1），而这一个索引同时也能顺带改善 `ListTasks` 和 `Stats`（`Store.ListTasks`、`Store.Stats`）。
`tasks` 不算最大的表，但它在 runner 的热路径上（每次转移都 UPDATE）。按 AGENTS.md
§6.3 的谨慎原则走 CONCURRENTLY，写法照抄 0018。
**两件事拆成两条迁移**（§6.6「一次迁移只做一件事」）。0037 不用。

### 3.3 代码结构

```
internal/store/inbox.go        纯 SQL：取候选行 + 标记的读写（不认识 prd 包）
internal/inbox/inbox.go        派生逻辑（纯函数）：候选行 → Item；键的生成与解析
internal/inbox/service.go      组合：Items / Count / Mark / ReadAll（依赖 store + prd）
internal/httpapi/inbox.go      HTTP 面：校验 → 调 service → 映射状态码
internal/runner/redact_export.go  新文件，3 行导出 RedactText（不改 redact.go）
cmd/lathe/main.go              装配 Redactor + InboxAPI.Routes(mux)（约 +5 行）

测试文件（按包拆分，原因见 §2.4 的 import 环取证与 §5.0）：
internal/store/inbox_migration_test.go  package store：只放 AC-1b（要用未导出的 loadMigrations/splitStatements/testStore）
internal/inbox/inbox_test.go            package inbox，纯函数，不连库：AC-2/3/4/5/13/13b
internal/inbox/db_test.go               package inbox，真库：AC-6a/6b/6c/7/7b/10/14 + 自带 fixture
internal/runner/redact_export_test.go   package runner：AC-13c
internal/httpapi/inbox_test.go          package httpapi，真库：AC-6/8/9/9b/11/12
```

为什么要新开 `internal/inbox` 包：`prd_confirm` 需要用 `prd.ParseDocument` 解析
document，而 `prd` 包已经 import 了 `store`（`internal/prd/service.go` 用到
`store.TransitionPRDParams`）。store 再反过来 import prd 会成环。派生规则写成纯函数，
也能在不连数据库的情况下用表驱动测完（§5 测试纪律）。

#### store 层（`internal/store/inbox.go`）

- `InboxTaskCandidates(ctx, userID) ([]InboxTaskRow, error)`：一条 SQL。

  ```sql
  WITH mine AS (
    SELECT t.*, max(t.id) OVER (PARTITION BY t.repo_id, t.tracker_provider, t.external_key) AS newest
    FROM tasks t WHERE t.user_id = $1
  )
  SELECT m.id, m.state, m.external_key, m.tracker_provider, m.failure_reason, m.failure_stage,
         r.provider_repo, i.id AS issue_id, i.title AS issue_title,
         ev.id AS event_id, ev.at AS since, ev.payload,
         p.id AS predecessor_id
  FROM mine m
  JOIN repos r ON r.id = m.repo_id
  LEFT JOIN issues i ON m.tracker_provider = 'internal' AND i.user_id = m.user_id AND i.key = m.external_key
  CROSS JOIN LATERAL (
    -- 锚点 = 进入当前状态的那次转移，跳过收割等同状态记账事件（§3.1.1）
    SELECT id, at, payload FROM task_events
    WHERE task_id = m.id AND from_state IS DISTINCT FROM to_state
    ORDER BY at DESC, id DESC LIMIT 1
  ) ev
  -- 前驱链接只给本人的前驱：归属判断在 SQL 里做（TaskItem 是纯函数，不连库判断不了）。
  -- 前驱属于别人、已被删除、或 payload 里的 blocked_by 不是正整数时，predecessor_id 为 NULL。
  -- 类型转换必须包在 CASE 里：AND 不保证短路，规划器会把 ::bigint 提进 Hash Cond 先算。
  LEFT JOIN tasks p
    ON m.state = 'blocked_dep'
   AND p.id = CASE
         WHEN jsonb_typeof(ev.payload->'blocked_by') = 'number'
          AND ev.payload->>'blocked_by' ~ '^[1-9][0-9]{0,17}$'
         THEN (ev.payload->>'blocked_by')::bigint
       END
   AND p.user_id = $1
  WHERE m.state IN ('blocked_spec','awaiting_approval','review_feedback','blocked_dep','failed')
    AND (m.state <> 'failed' OR m.id = m.newest)
  ```

  这里用窗口函数而不是 `NOT EXISTS`：后者对每个失败行都要按工单键反查，而
  `(repo_id, tracker_provider, external_key)` 上只有一个**部分**唯一索引
  （只覆盖活任务，`tasks_one_active_per_item`），用不上。窗口函数只把本人的任务扫一遍。
  `LATERAL` 用的是 `task_events_task_at (task_id, at)` 倒序扫描，`from_state` 条件是
  回表后的过滤；同状态事件每个任务至多一条（`worktree_claimed_at IS NULL` 守卫保证
  收割只落账一次，`Machine.ClaimForReap`），多扫的行数是常数。
  `payload` 也随锚点取自进入事件，所以 `question`（blocked_spec）、`reason`
  （awaiting_approval）、`blocked_by`（blocked_dep）不会被记账事件的 payload 顶掉。
  `predecessor_id`：`blocked_dep` 只有一个写入点 `Machine.PropagateBlocked`，
  payload 的 `blocked_by` 是**失败的那个祖先**（递归 CTE 找全传递后继，所以它不一定等于
  `tasks.depends_on`），因此按 payload 而不是 `depends_on` 连表。`p.user_id = $1` 让「属于别人」
  和「不存在」都落成 NULL，`InboxTaskRow.PredecessorID *int64` 为 nil；`TaskItem` 只在
  `PredecessorID != nil` 时把 `/tasks/<PredecessorID>` 放进 `Extra`，**从不**直接读 payload 的
  `blocked_by` 拼链接。
  **脏 payload 的守卫**：类型转换只出现在 `CASE WHEN … THEN …::bigint END` 的
  THEN 分支里。Postgres 不保证 `AND` 的求值顺序，「`jsonb_typeof='number' AND
  p.id = (…)::bigint`」的写法会被规划器把转换提进 `Hash Cond` 先算，遇到 `"abc"` 直接报
  `invalid input syntax for type bigint`，整条候选查询失败，该用户的收件箱和徽标都会 500。
  CASE 是 Postgres 文档承诺按顺序求值的结构，条件不成立时转换不会执行，结果为 NULL，
  `p.id = NULL` 不匹配，前驱链接自然落空。两道条件缺一不可：`jsonb_typeof='number'` 排除字符串、
  数组、null；正则 `^[1-9][0-9]{0,17}$` 排除 `7.5`、`7.0`、负数和超过 bigint 范围的数（18 位以内
  一定小于 2^63−1）。`7.5` 能通过 `jsonb_typeof='number'`，只靠第一道会在 `::bigint` 处报错。
  在小表上实测过：AND 写法会被规划器选成 Hash Right Join，转换在 Hash Cond 里先算并报错。等值条件里是 CASE 表达式，仍可作为 `tasks_pkey` 的
  Index Cond 或 Hash Cond，不影响「每个 blocked_dep 行一次索引查找」。
  这次 LEFT JOIN 走 `tasks` 主键，每个 blocked_dep 行一次索引查找。
- `InboxPRDCandidates(ctx, userID) ([]InboxPRDRow, error)`：
  `WHERE user_id=$1 AND state IN ('drafting','awaiting_answers','ready_for_review')`，
  返回 `document`（需要数 inferred 节）、`last_error`、`round`、`processing`、`original_input`、`updated_at`。
  查询范围只包括活 PRD，规模很小。
- `InboxMarks(ctx, userID) (map[string]InboxMark, error)`
- `InboxSubjectOwned(ctx, userID, subj InboxSubject) (bool, error)`：`Mark` 找不到键时用来区分
  「停留已结束（409）」与「不存在 / 不是你的（404）」；后两者都返回 false。
- `UpsertInboxMark(ctx, userID, key string, subj InboxSubject, op InboxMarkOp) error`（`op ∈ read | unread | dismiss`）：
  同一事务里 `INSERT … ON CONFLICT (user_id,item_key) DO UPDATE`，再
  `DELETE … WHERE user_id=$1 AND <subject>=$2 AND item_key <> $3`（剪掉旧锚点）。
  「标为未读」时：若未忽略则 DELETE 该行，已忽略则只把 `read_at` 置 NULL
  （有 CHECK 约束兜底）。
- `MarkInboxRead(ctx, userID, items []InboxKeySubject) error`：「全部已读」走一条
  `INSERT … SELECT unnest(...) ON CONFLICT DO UPDATE SET read_at = COALESCE(inbox_marks.read_at, now())`。

#### inbox 包

```go
type Kind string // 见 §3.1.2 九个常量

type Item struct {
    Key         string    `json:"key"`
    Kind        Kind      `json:"kind"`
    SubjectType string    `json:"subjectType"` // "task" | "prd"
    SubjectID   int64     `json:"subjectId"`
    Title       string    `json:"title"`       // 内置工单标题 / Linear key / PRD 一句话或原文首行
    Repo        string    `json:"repo"`
    Reason      string    `json:"reason"`      // 按 rune 截断到 200（maxReasonRunes）
    Since       time.Time `json:"since"`       // 进入当前停留的时刻
    Link        string    `json:"link"`        // 站内相对路径，不含主机名
    Extra       []Link    `json:"extra,omitempty"` // 内置工单页 / 前驱任务
    Dismissible bool      `json:"dismissible"`
    Read        bool      `json:"read"`
}

// Redactor 是展示前的脱敏函数。由 main.go 装配为
//   func(s string) string { return runner.RedactText(s, cfg.KnownSecrets()) }
// inbox 包自己不 import runner；NewService 收到 nil 直接返回错误（启动即失败，不静默降级）。
type Redactor func(string) string

func TaskItem(r store.InboxTaskRow, red Redactor) (Item, bool)
func PRDItem(r store.InboxPRDRow, doc *prd.Document, red Redactor) (Item, bool)
func ParseKey(s string) (Key, error)  // 封闭正则；返回主体类型、主体 id
func reasonText(raw string, red Redactor) string // = truncateRunes(red(raw), maxReasonRunes)
func truncateRunes(s string, n int) string // 按 rune 截断，结果保证 utf8.ValidString
```

runner 侧只**新增**一个文件 `internal/runner/redact_export.go`（不改 `redact.go`）：

```go
// RedactText 是 redactText 的导出入口：给 runner 之外、要把任务自由文本放上界面的
// 展示面（收件箱）用。两层规则与失败记忆完全相同，没有第二套实现。
func RedactText(s string, registered []string) string { return redactText(s, registered, nil) }
```

依赖方向：`cmd/lathe` → `inbox`、`runner`；`inbox` 不依赖 `runner`，没有成环风险
（`runner` 也不依赖 `inbox`/`httpapi`/`prd`，已用 `go list -deps` 核对）。

- `Reason` 的来源：blocked_spec 取进入事件 payload 的 `question`，没有就用 `failure_reason`；
  failed 和 blocked_dep 取 `failure_reason`；awaiting_approval 取 payload 的 `reason`；
  review_feedback 用固定文案「review 意见已记下，但无法自动续跑二轮，需要你处理」，
  **不带 review 评论原文**（那是第三方写的，详情页才展示）；prd_run_failed 取 `last_error`；
  prd_answers 显示「N 个问题待回答」；prd_confirm 显示「N 节待确认」。
  **所有取自自由文本的原因（`question`、`failure_reason`、payload `reason`、`last_error`）
  一律先过 `Redactor` 再按 rune 截断**（顺序不能反：先截断可能把一个 token 切成形态表
  认不出的残段）。理由见 §3.7：`failure_reason` 写入侧并未脱敏。
- `Link` 只用 `/tasks/<id>`、`/issues/<id>`、`/prds/<id>?section=<key>` 这几种形式，
  `<key>` 只能取 `Document.Sections()` 里的十个固定值。不从请求 Host 拼地址（§7 安全红线）。

`Service` 的方法：

- `Items(ctx, userID, filter)`：候选 + 标记合并 → 过滤已忽略的 → 按 `Since DESC` 排序 →
  再按 `filter=unread|all`、`kind=` 筛 → 分页（`limit` 默认 100，上限 200）。
- `Count(ctx, userID) (unread, total int)`：和 Items 走同一条派生路径（**不另写一条计数
  SQL**，否则两边口径必然漂移），只是不返回事项列表。
- `Mark(ctx, userID, key, op)`：`op ∈ {read, unread, dismiss}`。
  1. `ParseKey` 失败 → `ErrBadKey`（400）。
  2. 重新派生**本人**的当前事项，按键查找。
  3. 没找到：如果键里的主体属于本人（`tasks.user_id` / `prds.user_id` = userID），
     说明停留已经结束 → `ErrStale`（409，附上该主体当前的事项，可能为 null）；
     否则（主体不存在或属于别人）→ `ErrNotFound`（404）。**这两种 404 返回完全相同的
     响应体。**
  4. `dismiss` 只允许 `Dismissible` 的事项，否则 `ErrNotDismissible`（409）。
- `ReadAll(ctx, userID)`：把当前派生出的、未忽略的全部事项标为已读。

### 3.4 API

全部挂在 `Auth.RequireFunc` 之下（未登录 401），`userID` 一律取 `CurrentUser(r).ID`，
没有任何「看别人的收件箱」的参数，管理员也一样（与 `API.Routes` 的立场相同）。

| 方法与路径 | 请求 | 响应 |
|---|---|---|
| `GET /api/inbox?filter=unread\|all&kind=<kind>&limit=&offset=` | — | 200 `{items:[Item], unread:N, total:N}`；`filter`/`kind` 非法时 400 |
| `GET /api/inbox/count` | — | 200 `{unread:N, total:N}`（徽标轮询用，不返回列表） |
| `POST /api/inbox/items/{key}/read` | — | 200 `{item}`；400 / 404 / 409 见 §3.3 |
| `POST /api/inbox/items/{key}/unread` | — | 同上 |
| `POST /api/inbox/items/{key}/dismiss` | — | 同上；不可忽略的种类回 409 |
| `POST /api/inbox/read-all` | — | 200 `{marked:N}` |

- 404 的响应体固定为 `{"error":"事项不存在"}`，不带键，也不带主体是否存在的任何信息。
- 409（停留已结束）的响应体是 `{"error":"该事项已更新","current":Item|null}`。主体
  已确认属于本人，所以返回当前事项不泄露任何东西。
- 收件箱的端点**不改任何 `tasks`/`prds` 行，不写 `task_events`**（AC-11 守着这一点）。
- 和 f05（个人 API 令牌）的关系：f05 会让 PAT 等同于本人身份。收件箱只依赖 `CurrentUser`，
  不需要任何改动就能兼容。

### 3.5 UI

- **新增** `web/src/views/Inbox.vue`，路由 `/inbox`（name `inbox`），要求登录，不需要 admin。
  - 页签「未读 / 全部」，再按 kind 分组（组标题用中文：等你补充需求 / 等你放行 /
    review 停车待处理 / 前驱阻塞 / 失败待处置 / PRD 运行失败 / PRD 等你回答 /
    PRD 等你批准 / PRD 待逐节确认）。
  - 每行显示：种类徽标、标题、仓库、原因（单行，超长用省略号）、相对时间（悬停显示绝对时间）、
    未读圆点。
  - 每行的操作：**「去处理」**（先调 `read`，**不论成功失败都跳转**，标记失败不能挡着人去处理）、
    「标为已读 / 未读」、「忽略」（只在 `dismissible` 时显示，点之前走 `confirm.js` 二次确认）、
    以及 `extra` 里的附加链接。
  - 顶部有「全部标为已读」（二次确认），专门应对上线第一天存量 failed 全部是未读的情况（§6 R-3）。
  - 409 时用返回的 `current` 就地替换这一行，并提示「已更新」。
  - 空状态文案：「没有在等你处理的事」。
- **新增** `web/src/components/InboxBadge.vue`：自己轮询 `GET /api/inbox/count`，
  间隔 30 秒，页面隐藏时（`document.visibilityState`）暂停，路由切换时立刻刷新一次。
  `unread>0` 时显示数字，超过 99 显示 `99+`。
- **最小改动**：
  - `web/src/App.vue`：在 nav 里加一行 `<RouterLink to="/inbox">收件箱 <InboxBadge/></RouterLink>`，
    并 import 组件（+2 行）。
  - `web/src/main.js`：import 和 route 各一行（+2 行）。
  - `web/src/api.js`：加 6 个方法（`inbox`、`inboxCount`、`inboxRead`、`inboxUnread`、
    `inboxDismiss`、`inboxReadAll`）。
  - `web/src/views/prd/PRDConversation.vue`：`onMounted` 里如果有 `route.query.section`，
    并且它是 `SECTIONS` 里的合法 key，就打开文档面板并调 `panel.gotoSection(key)`（约 +8 行）。
    **不改** `TaskDetail.vue`：那是 f04（运行回放）的主战场，打开详情页时自动标已读的
    功能也因此不做，见 §4。
  - 新增 `web/src/inbox.js`：种类 → 组标题的对照表，以及页面标记后让徽标立刻重拉计数的信号。
- 改完 `make ui` 同步到 `internal/webui/dist`（AGENTS.md §2）。

### 3.6 配置项

**不新增任何配置项和环境变量**，README 变量表无需改动。下面这些常量写在代码里，
各自的消费方如下：

| 常量 | 值 | 唯一消费方 | 为什么不开放成配置 |
|---|---|---|---|
| `inbox.maxReasonRunes` | 200 | `inbox.truncateRunes` → `Item.Reason` | 纯展示长度，完整原因看详情页 |
| `inbox.defaultLimit` / `maxLimit` | 100 / 200 | `Service.Items` 分页 | 与 `ListTasks` 的上限 200 一致（`Store.ListTasks`） |
| 徽标轮询间隔 | 30 s | `InboxBadge.vue` | 前端常量；数据库压力由 0036 的索引兜住 |

### 3.7 安全

- **凭据**：收件箱不读也不写任何凭据表，`Item` 里没有 token、webhook 密钥、会话这类字段。
  - **事实**：`tasks.failure_reason` **写入时没有脱敏**。
    `failWithExcerpt`（`internal/runner/pipeline.go`）拼出 `stage.label() + cause` 后原样写入；
    验证失败时 cause 含 `f.Err` 和 `report.Summary()`。失败记忆的 `redactSecrets` 只作用于写进
    `failure_digests` 的副本。mergepoll 的 rebase 跟进、分诊 `verdict.Reason`、验证阶段的
    `blocked_spec` 写入同样未脱敏，`blocked_dep` 的原因就是前驱的同一个 reason。
  - **处置**：`Item.Reason` 在 inbox 包内**先脱敏再截断**（§3.3
    `reasonText`），用的是 runner 里同一套两层规则（层 1 精确替换 `config.KnownSecrets()`
    里的平台凭据值，层 2 按形态兜底），通过新增的导出入口 `runner.RedactText` 调用，
    不复制第二套规则。覆盖全部自由文本来源：`failure_reason`、`question`、payload
    `reason`、PRD `last_error`。AC-13b 守着。
  - **能力边界（承认代价）**：层 2 是形态识别，尽力而为、非穷举（`redact.go` 头注释原话）；
    不在 `KnownSecrets` 里、也不像任何已知形态的随机串，收件箱同样认不出。这与失败记忆
    的保证等同，不更弱也不更强。
  - **存量欠账不在本 PR 修**：任务详情页（`TaskDetail.vue` 的失败卡片）与看板
    （`Board.vue` 的失败行）仍然原样展示未脱敏的 `failure_reason`。修它要么改 TaskDetail.vue
    （f04 的主战场），要么在写入侧加脱敏（改 runner 的失败落库路径，超出本功能）。本 PR
    只做到「收件箱这个新展示面不扩大泄露面」，并把这条欠账登记进 `docs/05-roadmap.md`
    §1（§7 第 5 条）。
  - 另外收件箱刻意**不展示** review 评论原文和验证输出摘要。
- **日志**：handler 只在 500 时打 `serverError`，记错误本身，**不记事项内容和请求的键**。
- **属主 404**：所有派生查询都在 SQL 里带 `user_id = $1`；`Mark` 先在本人范围内派生，
  再判断主体是否属于本人，「不存在」和「不是你的」返回逐字节相同的 404（AC-6）。
  这条规则**原样遵守，没有改动**。
- **输入**：键走封闭正则，长度不超过 128；`kind`/`filter` 走白名单；`limit`/`offset`
  夹在合法区间内。
- **链接**：都是站内相对路径，不用 Host 头拼接。
- **不触碰子进程**：收件箱不起子进程，AGENTS.md §9 的 Dir/env 陷阱与它无关。

### 3.8 并发与多节点

- 没有后台 goroutine，没有进程内缓存，每次请求都从库里派生。多节点读到的是同一份数据库，
  结果天然一致。
- 写标记用 `INSERT … ON CONFLICT DO UPDATE`。多个标签页、多个节点同时点已读，最终只有一行，
  不报错（AC-10）。
- 「剪掉旧锚点」和 upsert 在同一个事务里，不会在中间状态留下两行。
- 派生和标记之间有时间差（读到事项 → 人点已读 → 这时状态已经变了）：`Mark` 会重新派生，
  键已不在时返回 409，不会把已读写到一个过期的键上。即使在 `Mark` 重新派生之后、
  写入之前状态又变了，结果也只是给一个已经消失的键写了一行标记，下一次写同一主体时就被剪掉。
  无害。
- 不碰 `ClaimReady`，也不对 `tasks` 加任何锁（只读 SELECT），对领单调度没有影响。
  测试遵守 `-p 1`，fixture 邮箱带 `t.Name()` 和随机后缀，断言只看本 fixture 用户的事项（§5.5）。

---

## 4. 范围外（不做什么）

| 不做 | 理由 / 归属 |
|---|---|
| 邮件 / Slack / 飞书等任何外发通知，「新事项推送」 | **f09**。收件箱只是站内的拉取式视图。现有的邮件（notify.go）不动 |
| 在收件箱里直接放行、重试、回答、确认节 | 这些决策需要详情页的上下文（验证报告、重试预览、复核发现）。给一个绕过上下文的快捷按钮，等于在弱化闸门。v1 只做跳转 |
| 打开任务详情页时自动标已读 | 要改 `TaskDetail.vue`，那是 **f04** 的主战场。由收件箱的「去处理」按钮标已读就够了 |
| 团队成员看到团队仓库的事项，按角色分派 | **f12**。v1 严格按 `user_id`。派生谓词集中在 `store.InboxTaskCandidates` / `InboxPRDCandidates` 两处，f12 合入后只需改这两处的 WHERE；`inbox_marks` 本来就是按查看者存的，与团队共享兼容 |
| 插话（运行中消息）的未回复提示 | **f01** |
| 用量 / 预算告警事项 | **f03** |
| 定时建单失败事项 | **f10** 自己负责「失败可见」。将来要进收件箱，只需在 `inbox` 包里加一个 kind，不需要迁移 |
| GitLab / Gitea / 云效来源的特殊展示 | **f07/f08**。派生 SQL 对 `tracker_provider` 不做假设，新平台的任务会自动以 `external_key` 作为标题出现 |
| `pr_open`（等人在代码平台上合并） | 合并决策在 GitHub 侧，Lathe 不做合并决策（铁律 §1.2）。PR 已开的提醒属于 f09 |
| `queued/triaging/implementing/verifying` | 不在等人 |
| `cancelled/merged`、PRD 的 `approved/converted/abandoned` | 终态，或在等系统而不是等人（`approved` 等的是一键生成，PRD 页面已有入口；搁置的 PRD 已有邮件提醒和「继续」入口）。v1 不纳入，避免噪音 |
| 「已忽略」列表和「恢复」 | v1 不做。误忽略的失败任务仍然能在看板的「失败」筛选里找到（`Board.vue`），同一任务再失败一次会以新键重新出现 |
| 实时推送（SSE / WebSocket） | 30 秒轮询够用。事件推送是 docs/04 §3.3 的另一条演进线 |
| 改状态机、改验证、改「非属主 404」、加依赖、加环境变量 | 设计约束（§1.2 G-5） |

---

## 5. 验收标准

> 命令都在仓库根执行，测试依赖 `make dev-infra` 的真实 Postgres（`LATHE_TEST_DSN` 指向已迁移的测试库）。
> 测试名是**必须存在的确切名字**。本功能新增的全部测试函数一律以 `TestInbox` 开头，
> `-run '^TestInbox'` 一条过滤即可覆盖。每条后面的「结果」是实现时的实测。

### 5.0 先红后绿（AGENTS.md §5.4）

- **测试放在哪个包**：`internal/store` 的同包测试 import 不了 `task`（§2.4 的环），所以凡是要用
  `task.Machine` 造数、或要调 `inbox.Service` 的真库测试，一律放在 `internal/inbox` 包。

  | 包 | 测试 |
  |---|---|
  | `internal/store`（`package store`） | AC-1b `TestInboxMigrationIndexValid` |
  | `internal/inbox`（纯函数，`inbox_test.go`） | AC-2、3、4、5、13、13b |
  | `internal/inbox`（真库，`db_test.go`） | AC-6a、6b、6c、7、7b、10、14 |
  | `internal/runner` | AC-13c |
  | `internal/httpapi`（真库） | AC-6、8、9、9b、11、12 |

  `internal/inbox/db_test.go` 自带 fixture：`store.Open(testsupport.DSN())`（连不上走 `SkipOrFail`）、
  `task.NewMachine(st.Pool())`；user 的 email 带测试名与纳秒随机量，`t.Cleanup` 删 user（级联带走
  tasks/task_events/inbox_marks，PRD 先删），断言只看本 fixture 用户。
- **AC-0** 分两次提交：第一次只含测试与返回零值的桩（迁移、路由已就位）；第二次才是实现。
  第一次提交上 `go vet ./internal/inbox/ ./internal/store/ ./internal/httpapi/ ./internal/runner/` 退出码 0、无 `import cycle`；
  `go test -p 1 -count=1 -v ./internal/inbox/... ./internal/store/... ./internal/httpapi/... ./internal/runner/... -run '^TestInbox'`
  退出码非 0，没有编译失败，下列 10 个名字都以 `--- FAIL:` 出现：`TestInboxTaskItem`、`TestInboxTaskItemReason`、
  `TestInboxPRDItem`、`TestInboxParseKey`、`TestInboxReasonTruncatedByRune`、`TestInboxReasonRedacted`、
  `TestInboxTaskCandidates`、`TestInboxBlockedDepPredecessorLink`、`TestInboxKeyStableAcrossReap`、
  `TestInboxRedactTextExported`；`--- FAIL: TestInbox` ≥ 13；`internal/store` 一行为 `ok`
  （`TestInboxMigrationIndexValid` 守迁移本身，不在红绿要求内）。第二次提交上同一条命令退出码 0，
  无 `--- FAIL`、无 `--- SKIP: TestInbox`，上述 10 个名字都以 `--- PASS:` 出现。
  - 结果：红阶段 vet 0 个 import cycle；`--- FAIL: TestInbox` 计 86 行（含子用例），10 个必红名字全部命中，
    编译失败 0 行，store 为 ok。绿阶段 0 FAIL、0 SKIP，10 个名字全部 PASS。两次提交的 SHA 与日志摘录在 PR 描述里。
  - 反向约束（实现提交上临时改 SQL 再跑，跑完还原）：CASE 守卫改回 AND 写法 → AC-6c 子用例 5 报
    `invalid input syntax for type bigint: "7.5"`；去掉 `p.user_id = $1` → AC-6c 子用例 2 失败；去掉
    `from_state IS DISTINCT FROM to_state` → AC-7b 失败（回收后换键、已忽略的事项重现）。

### 5.1 迁移

- **AC-1** 号段与历史迁移：`git diff --name-only --diff-filter=A origin/dev...HEAD -- migrations/` 恰好是
  `0035_inbox_marks.{up,down}.sql`、`0036_tasks_user_state_index.{up,down}.sql` 这 4 行；
  `git diff --name-only --diff-filter=MDR origin/dev...HEAD -- migrations/` 为空。
- **AC-1a** 往返：`lathe migrate up && lathe migrate down && lathe migrate up` 退出码为 0。不断言 down 退的是谁
  （`MigrateDown` 只退已应用的最高版本，邻居的迁移合入后退的是邻居的）。
- **AC-1b** `TestInboxMigrationIndexValid`：应用后 `tasks_user_state` 的 `indisvalid = true`，`inbox_marks` 有
  `inbox_marks_one_subject`、`inbox_marks_nonempty` 两个约束；用 `loadMigrations()` **按版本号**取 0036、0035
  依次执行 `DownSQL`（0036 走 `splitStatements` 逐条无事务，同 `rollbackOneNoTx`）后两个对象都 `to_regclass IS NULL`；
  再执行 0035、0036 的 `UpSQL` 后条件重新成立。`t.Cleanup` 兜底补跑 up，不读写 `schema_migrations`。

### 5.2 派生（纯函数，`internal/inbox`，不连库）

- **AC-2** `TestInboxTaskItem`：12 个状态各一个子用例，只有 `blocked_spec`、`awaiting_approval`、`review_feedback`、
  `blocked_dep`、`failed` 返回 `ok=true`，kind 依次为 `task_blocked_spec`、`task_awaiting_approval`、`task_review_parked`、
  `task_blocked_dep`、`task_failed`；`Key == "task:<id>:e<eventID>"`；只有 `task_failed` 可忽略；`Link == "/tasks/<id>"`；
  内置工单 `Extra` 含 `/issues/<issueId>`；`blocked_dep` 在 `PredecessorID = &7` 时 `Extra` 含 `/tasks/7`，
  `PredecessorID = nil` 而 payload `blocked_by = 7` 时**不含**。
- **AC-3** `TestInboxTaskItemReason`：blocked_spec 优先 payload `question`，没有才用 `failure_reason`；review_feedback
  是固定文案，不含注入的 `SECRET-REVIEW-BODY`。
- **AC-4** `TestInboxPRDItem`：`drafting+inferred+!processing → prd_confirm`（Link 以 `?section=<第一个 inferred key>` 结尾）；
  `drafting+inferred+processing → 无`；`drafting+全部 confirmed → 无`；`awaiting_answers+!processing → prd_answers`；
  `ready_for_review → prd_review`；`drafting+last_error+!processing → prd_run_failed`（键带 8 位十六进制后缀）；
  `awaiting_answers+last_error → prd_run_failed`（优先于 answers）；`approved / converted / abandoned → 无`；
  同一 round 下两个不同的 `last_error` 生成的键不同。
- **AC-5** `TestInboxParseKey`：`task:1:e2`、`prd:3:confirm:r0`、`prd:3:run_failed:r2:0a1b2c3d` 解析出正确主体；
  `task:0:e1`、`task:1:e`、`prd:1:bogus:r1`、`../etc`、长度 129 的串、`task:1:e2 ` 等全部报错。
- **AC-13** `TestInboxReasonTruncatedByRune`：300 个「失」→ `RuneCount(Reason) <= 201` 且 `utf8.ValidString`；
  第 200 个字符是 4 字节 emoji 时结果仍合法。
- **AC-13b** `TestInboxReasonRedacted`：用真实的 `runner.RedactText` 装配 `Redactor`，对 `failure_reason`（failed、
  blocked_dep）、`question`、payload `reason`、PRD `last_error` 五种来源分别注入 `ghp_`+36 位、`lin_api_`+40 位、
  登记的已知凭据值（不符合任何形态），结果都不含去掉前缀后的凭据本体；token 放在第 190 个 rune 处跨越截断边界时，
  结果里不出现凭据本体的任何 8 字符子串（先脱敏后截断）。
- **AC-13c** `TestInboxRedactTextExported`：`RedactText(s, reg)` 与 `redactText(s, reg, nil)` 逐字节相等。

### 5.3 store 与派生 SQL（真库，测试位于 `internal/inbox`）

- **AC-6a** `TestInboxTaskCandidates`：9 个任务按合法路径分别转到 `queued`、`implementing`、`pr_open`、`blocked_spec`、
  `awaiting_approval`、`failed`、`cancelled`、`review_feedback`、`blocked_dep`（后者走真实写入点 `PropagateBlocked`，
  前驱 P 也是 failed），候选集的状态集合恰好是五个等人状态；每行的 `event_id` 等于 `Machine.Events` 里最后一条
  真正的转移；blocked_dep 行的 `PredecessorID = P.ID`，喂给 `TaskItem` 后 `Extra` 含 `/tasks/<P.ID>`。
- **AC-6c** `TestInboxBlockedDepPredecessorLink`：前驱属本人 → 有链接；属另一用户 → `PredecessorID == nil`、`Extra`
  无 `/tasks/` 链接、JSON 不含对方任务 id；前驱已删除 → 无链接；payload `blocked_by` 为 `"abc"` 或 `7.5` → 查询
  `err == nil`、无链接，且同用户另一个正常 blocked_spec 任务仍在结果里。
- **AC-6b** `TestInboxFailedSupersededByNewerTask`：同一工单先 failed、再建新行（queued）→ 旧 failed 不在候选；
  新行也 failed 后只剩新行。
- **AC-7** `TestInboxKeyChangesOnReentry`：`queued → triaging → failed` 得 k1 并标已读，再 `queued → triaging → failed`
  得 k2：`k1 != k2`，k2 未读。
- **AC-7b** `TestInboxKeyStableAcrossReap`：failed 任务带现场路径，键 k1 标忽略；`ClaimForReap(commit=true)` 返回
  `(true, nil)` 且最后一条事件是 `failed → failed`、`kind = "worktree_reaped"`；之后键仍是 k1，`Items(all)` 不出现
  该任务，`Count.total` 不变。
- **AC-10** `TestInboxMarkConcurrentAndPruned`：10 个 goroutine 并发 `UpsertInboxMark(read)` 同一键全部成功、只有 1 行；
  同一主体换新键标已读后行数仍为 1、`item_key` 是新键。
- **AC-14** `TestInboxMarksCascadeOnSubjectDelete`：删任务 / 删 PRD 后其标记行数为 0。

### 5.4 HTTP（`internal/httpapi`，真库）

- **AC-6** `TestInboxIsolation`：A 有一个 `awaiting_approval` 任务与一个 `ready_for_review` PRD；以 B 身份 `GET /api/inbox?filter=all`
  不出现 A 的任何 subjectId、`total = 0`；B 对 A 的键和对 `task:999999999:e1` 调 `read` 都回 404，**两次响应体逐字节相同**；
  `inbox_marks` 里该键 0 行，A 看仍是未读。
- **AC-8** `TestInboxReadUnreadDismiss`：`task_failed` 依次 `read`（`read:true`、`unread` 减 1）→ `unread`（`read:false`）→
  `dismiss`（默认列表不再出现、`total` 减 1）；对 `task_awaiting_approval` 调 `dismiss` 回 409，且没有 `dismissed_at` 非空的行。
- **AC-9** `TestInboxReadAllAndCount`：3 个未读 → count `{unread:3,total:3}` → `read-all` 回 `{marked:3}` → count
  `{unread:0,total:3}`，`filter=unread` 的 items 为空。
- **AC-9b** `TestInboxStaleKey409`：failed 键在任务转回 queued 后调用 `read` → 409、`current: null`；再 `triaging → failed`
  后用旧键 → 409、`current.key` 为新键。
- **AC-11** `TestInboxEndpointsDoNotTransition`：6 个端点各调 2 次，`task_events` 计数与 tasks/prds 的 `state, updated_at`
  快照完全不变；`git diff --exit-code origin/dev...HEAD -- internal/task/state.go internal/prd/state.go` 退出码 0。
- **AC-12** `TestInboxRequiresAuth`：6 个端点不带凭据全部 401。`TestInboxBadInput`：`filter`、`kind`、键不合法时 400，
  响应体不回显原始输入。

### 5.5 构建、依赖、文档

- **AC-15**（`git status --porcelain` 为空后）：`git diff --exit-code origin/dev...HEAD -- go.mod go.sum web/package.json web/pnpm-lock.yaml`
  退出码 0；README 的 diff 里没有新的 `| \`LATHE_` 行；改动文件里没有 `skills/`、`.claude/`、`workspaces/`。
- **AC-16** `make lint && make test` 退出码 0；`gofmt -l`（含未入库的新 `.go` 文件）输出为空；`make test-race` 退出码 0。
- **AC-17** `make ui-deps`（仅首次）`&& make ui && go build ./cmd/lathe` 退出码 0；`internal/webui/dist/assets/*.js`
  里能 grep 到「收件箱」。手测：本地起一个 serve 实例（独立库、独立端口、收割机关闭），造出 blocked_spec、
  awaiting_approval、failed 三个任务和一份 ready_for_review 的 PRD：列表四条、徽标为 4；点「去处理」跳到详情页后
  徽标为 3；`count → read → count` 三次请求的原始响应与截图附在 PR 描述里。
  - 结果：通过。另验证 `prd_confirm` 的「去处理」跳到 `/prds/<id>?section=<key>` 并打开文档面板定位到该节；
    failed 原因里的 `ghp_…` 在列表上显示为 `ghp_[REDACTED]`。
- **AC-18** 本文件：§1–§7 七个章节标题；不写行号；不带本机路径、端口、库名；九个 kind、两条迁移名与 `CONCURRENTLY`、
  六个端点、`from_state IS DISTINCT FROM to_state`、`predecessor_id` 都出现；`CHANGELOG.md` 的 `[Unreleased]` 有本功能条目。

---

## 6. 风险与代价

| # | 风险 / 代价 | 处置 |
|---|---|---|
| R-1 | 每次请求都要派生，徽标每 30 秒一次；用户的任务越多，`LATERAL` 取最新事件的成本越高 | 0036 加了 `(user_id, state)` 索引，候选集只取 5 个状态加每个工单最新的 failed 行；`LATERAL` 走 `task_events_task_at`。真遇到瓶颈时，下一步是给 `task_events` 加 `(task_id, at DESC, id DESC)` 索引，**不是**把事项落成表 |
| R-2 | 在 `tasks` 上 CONCURRENTLY 建索引中断后，会留下一个 INVALID 索引 | 照抄 0018：先 DROP IF EXISTS，再用 DO 块自检，重跑即可修复 |
| R-3 | 上线第一天，所有历史 failed（每个工单最新的那一行）都显示为未读，徽标数可能很大 | 提供「全部标为已读」。**不写回填迁移**：迁移只做 schema（§6.4），而且替用户把东西标成已读本身就是一种猜测 |
| R-4 | `review_feedback` 在 webhook 两次转移之间会被短暂看到 | 已接受（§3.1.2）。代价最多是一次多余的徽标 +1，下一轮轮询就消失 |
| R-5 | PRD 被打回 drafting 后，没跑新的一轮就再次提交：`prd_review` 的键还是同一个 round，之前标过已读的话不会重新变成未读 | 已接受。打回和再次提交都是同一个人在 PRD 页面上做的操作，他知道这件事 |
| R-6 | `prd_confirm` 要解析每份活 PRD 的 document（几十 KB 的 jsonb） | 只解析 `drafting AND NOT processing` 的 PRD，数量和人正在推进的 PRD 一样多（通常是个位数）。真成了问题就换成 SQL 里的 `jsonb_path_exists`，不改接口 |
| R-7 | 与 f12（团队）的合并冲突：f12 会改可见性谓词 | 谓词集中在 store 的两个函数里，后合入的一方只需改这两处的 WHERE，并补一个隔离测试 |
| R-8 | 与其它功能线在 `App.vue` nav、`main.js` 路由、`api.js` 上的文本冲突 | 每处都只改 1–2 行，冲突可以机械解决。主体放在新文件里 |
| R-9 | 只有 failed 能忽略，可能有人想忽略 blocked_spec | 刻意的选择（§3.1.2）：那类事项必须通过状态变化离开，取消任务在详情页本来就有 |
| R-10 | 回滚 0035 会丢掉所有已读/忽略状态 | 写在 down 文件的注释里；丢掉的只是视图状态，任务和 PRD 不受影响 |
| R-11 | 别的功能线将来往 `task_events` 写**同状态**事件（插话、回放记账等），会不会再次让键漂移 | 不会：锚点只认 `from_state IS DISTINCT FROM to_state`（§3.1.1）。反过来，若有人引入**自环转移**（转移表里 X→X），锚点会把它当一次新的进入。目前转移表无自环（`internal/task/state.go` 的 `transitions`），引入自环本身是状态机语义变更，须走 §4「先问人」 |
| R-12 | 收件箱的脱敏是形态 + 已知值两层，不是穷举；详情页/看板仍原样展示未脱敏的 `failure_reason` | 收件箱不比失败记忆更弱；存量展示面的欠账登记进 05-roadmap §1.2（§7 第 5 条），不在本 PR 修 |

---

## 7. 文档改动（随同一个 PR）

1. **新增本文件 `docs/features/inbox.md`**：PRD 定稿版，代码位置按「文件 + 符号名」引用，不含评审过程。
2. **`CHANGELOG.md` 的 `[Unreleased]` → `### Added`**：收件箱条目（页面、徽标、可忽略的失败事项、
   迁移 0035/0036、六个新接口）。
3. **`README.md`**：文档索引表加一行 `docs/features/inbox.md`。**环境变量表不改**（没有新变量，AC-15 守着）。
4. **`docs/02-design.md`**：不改。状态机和验证都没变，收件箱是只读视图，不属于设计主干。
5. **`docs/05-roadmap.md` §1.2 加一行存量欠账**：`tasks.failure_reason` 写入侧未脱敏，任务详情页与看板原样展示；
   失败记忆和收件箱各自在副本 / 展示前脱敏，但主字段未修。治法候选是在 `Machine.Transition` 的 `FailureReason`
   落库前统一过 `runner.RedactText`（需要把 `KnownSecrets` 注入 task 层），属于 runner 失败落库路径，不在本功能范围。
   不登记到 `docs/08`：08 已归档，新欠账按其指引进 05-roadmap。
