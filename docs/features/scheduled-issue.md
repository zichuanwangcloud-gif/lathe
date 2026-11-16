# 定时建单（Scheduled Issue）

> 状态：已实现（迁移 0059、0060；代码在 `internal/schedule`、`internal/store/schedules.go`、
> `internal/httpapi/schedules.go`、`internal/integration/linear/schedule.go`、`cmd/lathe/schedule.go`、
> `web/src/views/Schedules.vue`）。

**结论先行**：用户在 Lathe 里配置「每日 / 每周 / 固定间隔」三种定时规则（不做 cron 语法、
不引入依赖）。到点时后台调度器**只做一件事：用属主自己的 Linear 凭据在 Linear 上建一张单**
（指派给属主本人，或打上系统设置里的接单标签）。之后这张单**和人手工建的单完全一样**：
经属主的 Linear webhook 回到 Lathe → 现有接单闸门 → 分诊 → 实现 → 验证 → PR。调度器
不建任务行、不碰 worktree、不驱动 agent。同一个计划时刻只建一次单，靠的是
`schedule_runs (schedule_id, scheduled_for)` 唯一约束，外加 Linear `issueCreate` 预生成 UUID
做幂等重放。停机期间错过的时刻**合并成一次补偿**，前提是落在补偿窗口内（默认 6h），
窗口外的那次记为 `skipped`，界面上看得到。


---

## 1. 问题与目标

### 1.1 为什么 Lathe 需要它

Lathe 的定位是「Linear issue → 已验证 PR」。现在每张单都得有人当场去建，可工程上有一类
活本来就是周期性的，比如每周一升级依赖并跑测试，每天清一遍 lint 告警，每月轮换一次
fixture、刷一次 vendored 数据。这类活今天只能靠人记着去 Linear 建单。竞品 Multica
提供了定时触发 agent 的能力，但它是**定时直接跑 agent 改代码**。Lathe 不能照搬：

- **铁律 1（验证体系不可削弱）**：如果定时直接驱动 agent，就绕开了「单子 → 分诊明确度判定 → 验证档位」这条主线；
  分诊不过时应回帖提问并停下（铁律 2），定时任务没有「单子」可回帖。
- **铁律 2（单子不明确就回帖提问）**：只有存在一张真实的 Linear 单，`blocked_spec` 回帖才有落点。

所以 Lathe 的做法是**只把定时用在「建单」上**：到点就建一张单，后面的一切走现有流程，
产品边界不受影响。用户拿到的是「每周一自动出现一个依赖升级 PR，且附带红绿验证证据」，
调度器本身不碰代码。

### 1.2 目标

| # | 目标 |
|---|---|
| G1 | 用户可在界面上增删改自己的定时规则：每日 HH:MM、每周（选星期几）HH:MM、每 N 小时（N≥1）；带 IANA 时区 |
| G2 | 到点用属主的 Linear 凭据建单：标题/正文模板（白名单变量）、优先级、团队；接单方式二选一：指派给本人 / 打接单标签 |
| G3 | 建单后走**完全现有的**接单→分诊→验证流程；调度器代码路径不写 `tasks` 表、不 import `runner`/`task` |
| G4 | 多节点、重启、并发 tick 下，同一 `(规则, 计划时刻)` 在 Linear 上只产生一张单 |
| G5 | 错过时刻的补偿策略明确、可配、有测试 |
| G6 | 失败可见：每次触发一条运行记录（成功/失败/跳过及原因）；连续失败自动暂停并在界面标出；建了单但没被接单（webhook 没配好）也能看出来 |

### 1.3 非目标（详见 §4）

不做 cron 表达式；不支持内置工单 / 云效 / GitLab 作为建单目标；不做通知推送；不做团队共享规则；
不做按规则选仓库（Linear 接单路径本就是「属主第一个仓库」，见 §2.4）。

---

## 2. 现状取证（行号按本功能合入后的代码）

### 2.1 Linear 客户端：只有读和评论，没有建单

- `internal/integration/linear/linear.go:25-51`：`Client{apiURL, token, http}`，`NewClientWithURL` 可注入 httptest 地址（测试先例）。
- `linear.go:96` `AssignedIssues`、`linear.go:162` `Issue`、`linear.go:240` `Comment`（`commentMutation` 在 `:232`）：**没有 `issueCreate`、没有 teams/labels 查询**。
- `linear.go:274-328` `do()`：GraphQL 200+errors 的处理；HTTP 非 200 时错误里带 `truncate(string(raw), 500)`。
- **`linear.go:523-532` `truncate` 按字节切 `s[:n]`**，会切断多字节 UTF-8。本功能要把 Linear 错误落进 `schedule_runs.error`，
  正好踩中 AGENTS.md §9「事件落库 UTF-8 截断 → SQLSTATE 22021」。**本 PR 顺手修成按 rune 截断**（最小改动，见 AC-14）。
- `internal/integration/linear/verify.go:16-35`：`viewer { id name email }`，`Verify` 返回的 `ID` 就是「指派给我」用的 Linear 用户 ID。

### 2.2 Tracker 窄接口：刻意只有两个方法

- `internal/tracker/tracker.go:8-10`：「接口方法就是这两个，禁止在没有真实消费方之前加新方法」。
- `tracker.go:40-46`：`Issue` / `Comment`。
- **结论**：建单**不进** `tracker.Tracker` 接口（f08 云效也在动这个接口，避免冲突），只加在具体类型 `*linear.Client` 上，
  调度器通过本包自己声明的窄接口消费（§3.2.3）。

### 2.3 接单路径（定时建出来的单将走这里，零改动）

- `cmd/lathe/main.go:263-269`：`POST /webhooks/linear/{slug}` → `httpapi.LinearWebhook`。
- `internal/httpapi/webhook.go:87-182` `ServeHTTP`：路由 → 验签 → `ClaimDelivery` 幂等 → 取消联动 → 接单闸门 → `Tasks.Enqueue`。
- `webhook.go:163`：闸门 `ev.IsAssignedTo(target.LinearUserID) || ev.IsLabelTriggered(target.TriggerLabel)`。
- `linear.go:410-436` `IsAssignedTo`：`action == "create"` 且 assignee 是本人就直接接单，**定时建单时直接带 `assigneeId` 正好命中**。
- `linear.go:447-467` `IsLabelTriggered`：`create` 且带触发标签时同样命中；标签名为空就恒为 false。
- `cmd/lathe/main.go:632-658` `webhookResolver.Resolve`：`LinearUserID = p.LinearUserID(ctx)`、`TriggerLabel = settings.WebhookTriggerLabel(ctx)`（现取）。
- `internal/creds/creds.go:162-167` `LinearUserID`：优先取验证凭据时落库的外部账号 ID，取不到才回退到 env（只对管理员）。
- `internal/creds/creds.go:140-147` `WebhookSecret`：未配置返回空串；`Resolve` 据此拒绝路由（`main.go:646-649`）。
- `internal/store/settings.go:26-35, 125-131`：`webhook_trigger_label` 系统设置，空串表示关闭。
- `internal/store/webhook.go:13-29` `ClaimDelivery`：`webhook_deliveries` 主键去重（docs/02-design.md §3 约束③）。

### 2.4 建任务与仓库解析（调度器**不碰**，只用来说明边界）

- `cmd/lathe/queue.go:91-111` `Enqueue`：`resolveRepo` → `tasks.Create`，撞上活任务唯一索引就报 warn。
- `cmd/lathe/queue.go:642-650` `resolveRepo`：`SELECT id, gate_mode FROM repos WHERE user_id=$1 ORDER BY id LIMIT 1`。
  Linear 路径**目前不能按单选仓库**（「按 team/project 路由」是 docs/02 §8 的后续项）。所以定时规则**不提供仓库字段**，免得出现一个没人消费的配置（铁律 3）。
- `migrations/0001_init.up.sql:131-133`：`tasks_one_active_per_issue` 部分唯一索引（现名 `(repo_id, external_key)`，见 0021:64）。
- `internal/task/machine.go:179-224` `Create`、`:830-857` `ClaimReady`（`FOR UPDATE SKIP LOCKED` 领单先例）。

### 2.5 后台循环的装配先例

- `cmd/lathe/main.go:511-566` `startWorkers`：worker、`MergePoller`（45s）、`prd.StallSweeper` 都从这里启动；
  `cmd/lathe/assembly_test.go:79-171` 用 go/parser 断言「后台执行者只从 startWorkers 启动」，`TestStartWorkersLaunchesAllLoops`（`:179` 起，`:173-178` 是其注释）断言每条循环都起了。
- `internal/prd/stall.go:131-157`：`StallSweeper{Store, Interval, Now func() time.Time}`，`Run` 用 ticker + ctx.Done（`:180-206`），
  `SweepOnce` 导出给测试直接驱动（`:211`）。**调度器照这个形状写**。
- `internal/runner/reaper.go:160-185`：同一形状；它的开关 `LATHE_REAP_ENABLED` 是先例。

### 2.6 配置与严格解析

- `internal/config/config.go:264-322` `Load`；`:509-520` `envDurationStrict`（非法时长启动报错，不静默回落）。
- `config.go:211-216` `validateBoolEnv`（只认 true/false/空）。
- `config.go:325-404` `Validate`：下限、范围校验的先例（`MinWorktreeTTL` `:192`、`MinPRDStallTimeout` `:204`）。
- README 变量表：`README.md:122-155`。

### 2.7 属主隔离与 404 先例

- `internal/httpapi/issues.go:40-49`：「全部按 CurrentUser 隔离：非属主一律 404」；`issues.go:317-319` `issueNotFound`。
- `internal/store/issues.go:37-39` `ErrIssueNotFound`「不存在或不属于当前用户，两者刻意不分」；`GetIssue` 在 `store/issues.go:102-106`，用 `WHERE id=$1 AND user_id=$2`。
- `internal/httpapi/issues_test.go:221-245` `TestIssuesIsolation404`：「换另一个用户视角，一切 404」的测试模板。

### 2.8 迁移与 UUID

- `migrations/`：现存最大号 0031，已有空号（0026/0027/0029）。`internal/store/migrate.go:422-466` 按版本排序加载，**允许空号**。
- `internal/store/migrate_test.go:59-78` `TestLoadMigrations`：每条迁移都必须有 up 和 down。
- `migrations/0023_prds.up.sql:20-74`：新表 + `set_updated_at` 触发器 + 属主索引的写法先例。
- `internal/prd/uuid.go:9-25`：本仓刻意不引 `google/uuid`，`crypto/rand` 手写 v4（「三处复制的代价小于一条依赖」）。本功能**再复制一份**。

### 2.9 前端

- `web/src/main.js:28-60` 路由表；`web/src/App.vue:49-59` 导航（`hasLinearToken()` 控制「Linear 任务」可见，`web/src/auth.js:38`）。
- `web/src/views/Issues.vue`：列表 + `BaseDialog` 新建弹窗的写法先例；`web/src/api.js:56` 起 `api` 对象。

### 2.10 时区

- 本仓目前不存在 `time.LoadLocation` 调用。`Dockerfile:44` 装了 `tzdata`。
- 实测（Go 1.25，`America/New_York`）：`time.Date(2026,3,8,2,30,…)`（春季跳过的墙钟）得到 `01:30-05:00`，即**往前**归一化；
  `time.Date(2026,11,1,1,30,…)`（秋季重复的墙钟）得到 `-04:00`，即两个里的**较早**那个。Go 文档写明这两种情况「不保证」，
  所以本功能**自己定规则并测试**（§3.1.3），不依赖 Go 的归一化行为。

---

## 3. 方案

### 3.1 规则模型与时间语义

#### 3.1.1 三种规则（不做 cron）

| kind | 字段 | 语义 |
|---|---|---|
| `daily` | `minute_of_day`（0–1439） | 规则时区内每天该墙钟时刻 |
| `weekly` | `minute_of_day` + `weekdays`（位掩码 1–127，bit0=周日 … bit6=周六，与 `time.Weekday` 对齐） | 选中的星期几的该墙钟时刻 |
| `interval` | `interval_minutes`（60–10080，且是 60 的整数倍） + `anchor_at`（timestamptz） | `anchor_at + k·interval`（k≥0），按绝对时长计，与 DST 无关 |

下限 60 分钟是产品常量（`schedule.MinInterval`），不做成配置。理由是每次触发都会在 Linear 建一张单、并在 Lathe 里跑一轮 agent，
间隔小于 1 小时只会制造噪声和成本，想不出合理用法。上限 7 天，再长就该用 weekly。

为什么不写 cron 子集解析器：三种规则已经覆盖目标用例（G1），而 cron 的 `L`/`W`/`?`/步长/日与周的或语义每一处都是
边界条件，「最小子集」最后也会被要求越做越全。三种结构化规则在 UI 上更直观，校验也更好写。代价见 §6 R5。

#### 3.1.2 计划时刻的确定性（多节点幂等的前提）

计划时刻 `scheduled_for` 是**规则定义的纯函数**，与「哪台节点、什么时候算」无关：

- `Spec.Next(after time.Time) time.Time`：严格大于 `after` 的第一个计划时刻；
- `Spec.Prev(atOrBefore time.Time) (time.Time, bool)`：≤ `atOrBefore` 的最后一个计划时刻（interval 规则在 anchor 之前没有）。

所有时刻对齐到整分钟、秒和纳秒为 0。两台节点对同一规则算出的同一个时刻，`timestamptz` 值逐位相同，
唯一约束才有意义。节点时钟漂移只会影响「什么时候发现该触发了」，不会改变「触发的是哪个时刻」。

#### 3.1.3 DST 规则（自己定，自己测）

只影响 daily/weekly（interval 按绝对时长计）。对每个本地日期 D 和墙钟 HH:MM，按下面的规则解析出**恰好一个**时刻：

实现**不写死 1 小时**，按当天前后的 UTC 偏移求候选，对任意跳变量（30 分钟的 `Australia/Lord_Howe`、2 小时的 `Antarctica/Troll`）都成立：

```go
// resolve 把本地日期 D 的墙钟 HH:MM 解析成恰好一个时刻。
probe := time.Date(y, m, d, 12, 0, 0, 0, loc)               // 当天正午，仅用来取前后偏移
_, offA := probe.Add(-36 * time.Hour).Zone()                  // 跳变前的偏移
_, offB := probe.Add(+36 * time.Hour).Zone()                  // 跳变后的偏移
// 候选：用两种偏移各把「D HH:MM」当作固定偏移时刻解释一次，再换回 loc 看墙钟是否吻合
cands := 把 off ∈ {offA, offB}（去重）下 time.Date(y,m,d,hh,mm,0,0,time.FixedZone("",off)).In(loc) 中墙钟 == (D,HH,MM) 的收集起来
```

1. **重复的墙钟**（秋季拨慢，两个候选都吻合）：取**较早**那个。NY 2026-11-01 01:30 → `-04:00`（`05:30Z`）；
   Lord_Howe 2026-04-05 01:45 → `+11:00`（`2026-04-04T14:45Z`）；Troll 2026-10-25 01:30 → `+02:00`（`2026-10-24T23:30Z`）。
2. **跳过的墙钟**（春季拨快，没有候选吻合）：取「**用跳变前的偏移 `offA` 解释该墙钟**」得到的那个绝对时刻，即
   `time.Date(y,m,d,hh,mm,0,0,time.FixedZone("",offA))`。它恰好落在跳变之后、与请求墙钟相差跳变量的位置：
   NY 02:30 → 03:30 EDT（`07:30Z`，跳 60m）；Lord_Howe 2026-10-04 02:15 → 02:45 `+11:00`（`2026-10-03T15:45Z`，跳 30m）；
   Troll 2026-03-29 01:30 → 03:30 `+02:00`（`01:30Z`，跳 120m）。语义是「跳过的那段里的任务，推迟跳变量那么久」，1h 跳变时与 Vixie cron 一致。
   旧稿的 `t.Add(time.Hour)` 在 Lord_Howe 上会得到 03:15（多推 30 分钟）、在 Troll 上会得到 02:30（仍是不存在的墙钟），已废弃。
3. 前提：一个本地日期内最多一次偏移跳变（tzdata 全量里成立；`±36h` 探针是为了跨过跳变所在的那一天）。若某天两个偏移相同但仍无候选吻合
   （理论上不会发生），`resolve` 返回错误，调度器把该规则记 `failed` 并带出时区名，不猜。
4. 按本地日期迭代：一个本地日期最多产生一个计划时刻，保证不会一天触发两次。
5. 期望值来自 go1.25.7 + `time/tzdata` 的实测探针，固化为 `internal/schedule/spec_test.go` 的 `TestSpecNext` 子用例。

#### 3.1.4 时区

- 字段 `timezone`（IANA 名，默认 `UTC`；UI 默认填浏览器的 `Intl.DateTimeFormat().resolvedOptions().timeZone`）。
- 保存时用 `time.LoadLocation` 校验，失败返回 400。
- `internal/schedule/spec.go` 加 `import _ "time/tzdata"`（**Go 标准库**，`go.mod` 不变，二进制约增 450KB）。原因是多节点下不能
  一台机器有 zoneinfo、另一台没有，导致同一规则在不同节点上算出不同时刻。
  **放在 `internal/schedule` 自身而不是 `cmd/lathe/main.go`**：`go test ./internal/schedule` 起的测试二进制不链接 `cmd/lathe`，
  放在 main 里测试就只能依赖宿主机 `/usr/share/zoneinfo`，「内嵌 tzdata」这条保证没有任何测试覆盖。放进本包后，
  凡是 import `internal/schedule` 的二进制（lathe 本体、本包测试、httpapi 测试）都带上同一份时区库。
  Go 的 `LoadLocation` 查找顺序是 `ZONEINFO` 环境变量 → 系统目录 → 内嵌库，测试里屏蔽不了系统目录，所以「内嵌」用依赖图机器断言
  （AC-4 末条：`go list -deps ./internal/schedule` 含 `time/tzdata`），`Mars/Base` 报错则与宿主机无关（任何 tzdata 版本里都没有这个名字）。

### 3.2 后台调度器 `internal/schedule`

#### 3.2.1 包结构与依赖纪律

```
internal/schedule/
  spec.go       Spec{Kind, MinuteOfDay, Weekdays, IntervalMinutes, Anchor, Loc}；Validate/Next/Prev/Describe
  template.go   Render(tpl, vars) / ValidateTemplate；白名单占位符
  runner.go     Runner{Store, Linear, Now, Tick, CatchUpWindow, Node}；Run(ctx) / TickOnce(ctx) (TickResult, error) / FireNow / Drain(ctx) / Close()
                TickResult{Claimed, Recovered []RunRef}，RunRef{RunID, ScheduleID}：本轮本节点认领/回收到的 run，供调用方按归属过滤
  exec.go       执行器：有界并发执行 pending（阶段 B），与认领循环解耦（§3.2.2）；节点级心跳与围栏
  uuid.go       newUUID（复制 prd/uuid.go，理由同其注释）
```

- **依赖白名单**：只 import `internal/store`、`internal/integration/linear`（仅用类型）和标准库。**不得 import
  `internal/task`、`internal/runner`、`internal/creds`**（creds 依赖 runner，见 `internal/creds/factory.go:59`）。AC-9 用 `go list -deps` 机器断言。
- Linear 能力经本包声明的窄接口注入：

```go
type IssueCreator interface {
    CreateIssue(ctx context.Context, in linear.CreateIssueInput) (*linear.CreatedIssue, error)
    // 查到 → (issue, nil)；确认不存在 → (nil, nil)；其余一切（HTTP 非 200、网络错误、
    // 无法归类为 not-found 的 GraphQL 错误）→ (nil, err)。判定依据见 §3.2.3「IssueByID 的 not-found 判定」。
    IssueByID(ctx context.Context, id string) (*linear.CreatedIssue, error)
    LabelIDByName(ctx context.Context, teamID, name string) (string, error)
}

// LinearHandle 是一次建单所需的全部 Linear 侧能力。
type LinearHandle struct {
    Client IssueCreator
    UserID string              // 「指派给我」用的 Linear 用户 ID，可能为空
    // Redact 把本次用到的凭据值精确替换成 [REDACTED]。由装配方用它手里的 token 构造，
    // 调度器本身永远拿不到 token 明文——它只拿到一个「会抹掉 token 的函数」。
    Redact func(string) string
}
type LinearFor func(ctx context.Context, userID int64) (LinearHandle, error)
```

  在 `cmd/lathe` 里装配（`linearFor(factory)`，`creds` 包零改动）。`linearFor` 写在新文件 `cmd/lathe/schedule.go`，签名是
  `func linearFor(f *creds.Factory) schedule.LinearFor`，**参数是具体类型 `*creds.Factory`**，不是 `runner.ClientFactory`：
  后者只有 `ForUser`（`internal/runner/pipeline.go:51-53`），没有 `ProviderFor`；`ProviderFor` 只在 `*creds.Factory` 上（`creds/factory.go:43`）。
  所以它只能在 `serve()` 里调用（那里的 `factory` 是 `creds.NewFactory` 的返回值，`main.go:173`），不能在 `startWorkers` 里调用（那里的
  `factory` 形参已被收窄成 `runner.ClientFactory`，`main.go:511-512`）。Runner 在哪里构造、怎么交给 `startWorkers`，见 §3.10。

```go
p := factory.ProviderFor(uid)
token, _, err := p.Token(ctx, store.KindLinear)   // 与 Provider.Linear 内部走同一条路（creds.go:122-128）
c, err := linear.NewClient(token)                 // 等价于 p.Linear(ctx)，只是把 token 留在装配方
return schedule.LinearHandle{
    Client: c, UserID: p.LinearUserID(ctx),
    Redact: strings.NewReplacer(token, "[REDACTED]").Replace,
}, nil
```

  和执行队列走同一条凭据通路（同一个 `Provider` 缓存），界面上改完凭据立即生效。`linear.Client.token` 仍是非导出字段，
  **不给 `linear.Client` 加 token 访问器**（那会让任何拿到客户端的代码都能取出明文）。

#### 3.2.2 一次 tick（`TickOnce`，tick 间隔为常量 30s）

**总体结构：认领循环与执行解耦**

`TickOnce` **只做数据库工作**：阶段 A（认领到期规则、判定 status/trigger、插入 run）和「回收」（原子拿过期租约），
然后把本轮拿到的 pending 交给执行器（`exec.go`）就返回，**不等任何 Linear 请求**。阶段 B（网络调用）在执行器的工作 goroutine 里跑，
并发上限为常量 `ExecConcurrency = 4`；执行器的等待队列不设上限（自然上界见下），排队中的 run 由节点级心跳持续续约（见「租约」）。

- 后果：`Run` 循环的下一次 tick 永远按 30s 节拍到来，与上一轮阶段 B 有多慢无关。一条规则被认领时的
  `lag = now − latest` 只受「tick 节拍 + 阶段 A 自身耗时」影响；阶段 A 是每条规则一个毫秒级短事务，
  所以单节点、库正常时 `lag ≤ Tick + ε`，落在 `OnTimeGrace = 2·Tick` 之内。**别的规则的 Linear 再慢，也不会让这条规则被判成 catchup 或 skipped**。
- status/trigger 在阶段 A 插入时就定死，之后执行排队多久都不改它：排队 10 分钟才真正调 Linear 的准点 run，记录仍是 `trigger=schedule`。
  执行延迟体现在 `created_at`（认领时刻）与 `finished_at` 的差上，UI 的运行记录可见。
- **准点保证的精确表述**：「准点 = 认领时 `lag ≤ OnTimeGrace`」。它在以下前提下成立：节点在跑、数据库可用、单轮到期规则数 ≤ `ClaimLimit`。
  `ClaimLimit = 200`（常量，阶段 A 每 tick 的认领上限；阶段 A 只碰库，200 条约百毫秒级）。超过上限的规则顺延一个 tick，lag 至多再加 30s，
  仍在 60s 宽限内；连续两轮都满载（> 400 条同时到期）或数据库本身卡顿 > 30s 时，迟到的那部分会按迟到处理（窗口内 catchup，`CatchUpWindow=0` 时 skipped）。
  这是如实写明的残余边界，不再说「永远不会」。
- 等待队列的自然上界：每条规则至多每 60 分钟一个时刻（最小间隔），每人至多 20 条规则，队列长度 ≤ 活跃规则数；执行器还会把队列长度
  打进 `slog.Warn`（> `ClaimLimit` 时），不做丢弃（丢弃会把已判准点的 run 变成「结果未知」）。
- **执行器与心跳的生命周期**：执行器的工作 goroutine 与节点级心跳 goroutine 是 Runner 的内部设施，
  **惰性启动**：第一次调用 `TickOnce` 或 `FireNow` 时由 `sync.Once` 起好（`Run` 只是循环调 `TickOnce`，所以也走同一个入口），
  挂在 Runner 自己的生命期 ctx 上（构造时不存在，`ensureStarted` 时由 `context.WithCancel(context.Background())` 生成），**不挂在调用方的 ctx 上**——
  否则一次 HTTP 请求结束或一次 `TickOnce(ctx)` 的 ctx 取消就会把整个执行器带走。结束只有一个入口 `Close()`：取消生命期 ctx、等执行器与心跳退出；幂等，未启动过时直接返回。
  `Run(ctx)` 在 `ctx` 取消后先停 tick 循环、再调 `Close()` 才返回，所以生产上「随 `startWorkers` 的 ctx 结束」仍然成立。
  - 后果：只调 `TickOnce` / `FireNow`、从不调 `Run` 的测试，同样有执行器（`Drain` 能等到终态）和心跳（租约会被续约）。测试一律 `t.Cleanup(r.Close)`。
  - `newScheduler` / 结构体字面量构造仍然纯构造：不起 goroutine、不碰库。
- 关停（`Close()`，或 `Run` 的 `ctx` 取消后由 `Run` 调 `Close()`）：执行器取消所有进行中的请求，排队中的 run 不再开始；心跳退出，它们的租约不再续约，至多 `LeaseTTL` 后被任一节点回收（见「回收」，
  未开始的 run 按全新执行处理，不会被误标 interrupted）。`Drain(ctx)` 只给测试用：等执行器把**已交付**的 run（含 `FireNow` 交付的）全部写完终态；未启动时立即返回；`ctx` 到期返回 `ctx.Err()`，不会无限卡住。

**阶段 A：认领到期规则（每条规则一个短事务）**

```sql
BEGIN;
SELECT ... FROM schedules
 WHERE enabled AND next_fire_at <= $now
 ORDER BY next_fire_at, id
 FOR UPDATE SKIP LOCKED LIMIT 1;
-- Go 侧：latest := spec.Prev(now)；baseline := max(last_slot_at, enabled_at)
--        missed := (baseline, latest) 区间内的计划时刻数（计数封顶 1000）
INSERT INTO schedule_runs (schedule_id, user_id, scheduled_for, trigger, status, reason,
                           missed_count, linear_issue_uuid, node,
                           lease_token, lease_node, lease_until)          -- 仅 pending 行带租约，skipped 行三列为 NULL
VALUES (..., $tok0, $node, now() + $leaseTTL)
ON CONFLICT (schedule_id, scheduled_for) DO NOTHING
RETURNING id;                                                       -- 0 行 = 冲突
-- ↓ 无论上面的 INSERT 插入 1 行还是因冲突插入 0 行，这条 UPDATE 都**无条件执行**
UPDATE schedules SET next_fire_at = spec.Next(now), last_slot_at = latest WHERE id = $id;
COMMIT;
```

- **INSERT 冲突时规则照样前移**（这一条是阶段 A 的硬要求，不是实现细节）：`ON CONFLICT DO NOTHING` 只决定「这一轮有没有新 run 要执行」，
  **不决定规则推不推进**。同一事务里的 `UPDATE schedules` 不看 INSERT 的结果，总是把 `next_fire_at` 推到 `spec.Next(now)`、`last_slot_at` 推到 `latest`。
  会走到冲突分支的真实路径：09:00:10 点「立即运行」，`scheduled_for` 截到分钟 = 09:00，正好占掉每日 09:00 规则的时刻（§3.5）；
  09:00:20 的 tick 认领到这条规则，INSERT 冲突。若此时不前移，`next_fire_at` 会停在 09:00，之后每 30s 被重新认领一次、每次占掉一个
  20 条认领名额，一直到次日 09:00——这正是要避免的空转。冲突的那一行由插入它的人（手动运行或别的节点）按租约执行，
  本轮**只是不再为它新建 run、不进入阶段 B**。语义上：同一分钟里先到的手动运行即视为该计划时刻已触发，不再另建一张单。
  AC-6 `TestTickAfterManualRunTakesSlotStillAdvances` 钉住这一条。

- **前移防御**（「无条件执行 UPDATE」的唯一例外，同样与 INSERT 是否冲突无关）：写 `next_fire_at` 前断言 `next := spec.Next(now); next.After(now)`。不成立（理论上只可能是 Next/Prev 在 DST 边界的实现 bug）时
  不写这个值，改为：本行 run 照常落库，规则 `enabled=false, next_fire_at=NULL, paused_reason='内部错误：下次触发时刻未前进（<规则描述>，now=<RFC3339>）'`，
  并 `slog.Error`。宁可停下让人看见，也不让这条规则在同一 tick 里被反复认领空转（ON CONFLICT 不插入、`next_fire_at` 也不前进）。

- 插入的 `status`，**按下面的顺序判定，命中第一条就停**（`lag := now - latest`，`OnTimeGrace := 2·Tick = 60s`，常量）：
  1. 属主已停用（`users.disabled_at IS NOT NULL`）：插 `skipped`，`reason='owner_disabled'`。
  2. **`lag ≤ OnTimeGrace`：插 `pending`，不受 `CatchUpWindow` 约束**。这是「准点触发」：tick 每 30s 一次且**不被阶段 B 阻塞**（见本节开头「总体结构」），
     单节点、库正常时 `lag ≤ Tick + 阶段 A 耗时`；留一倍余量吸收 tick 抖动与 `ClaimLimit` 顺延一轮。`trigger` 在 `missed == 0` 时记 `schedule`，否则记 `catchup`（例如停机后恰好在某个时刻前后起来）。
  3. `lag ≤ CatchUpWindow`：插 `pending`，`trigger='catchup'`。
  4. 否则插 `skipped`，`reason='missed_window'`，不调 Linear。
  
  所以 `CatchUpWindow` 只管「认领时迟到超过 `OnTimeGrace` 的那次补不补」，不参与准点判定；`CatchUpWindow=0` 的含义是
  「只有准点触发，迟到一律 skipped」。准点本身的成立前提见本节开头「准点保证的精确表述」：阶段 B 再慢也不影响它，
  只有节点停机、数据库卡顿 > 30s、或连续两轮到期规则数超过 `ClaimLimit` 才会让认领时 lag 超过宽限。`CatchUpWindow < OnTimeGrace` 时第 3 条自然不可能命中，不需要额外特判，也不需要给窗口加下限。
- `ON CONFLICT DO NOTHING` 没插进去（0 行），说明别的节点、上一轮或手动运行已经占了这个时刻：**不新建 run、不进入阶段 B，但上面的
  `UPDATE schedules` 照常执行**（见本节第一条）。**唯一约束是硬保证**，`SKIP LOCKED` 只是减少无谓的竞争。
- `linear_issue_uuid` 在**认领时**用 `crypto/rand` 生成 v4 并落库，它是阶段 B 幂等重放的钥匙。
- 每 tick 最多认领 `ClaimLimit = 200` 条规则（常量；阶段 A 只碰库，不受 Linear 快慢影响），积压分多轮排空，与 `stallScanLimit` 的立场一致。

**租约（lease）：谁在执行这条 pending，由数据库里的一行说了算**

阶段 B 的每一条 pending 在执行期间都由一个租约持有。租约是 `schedule_runs` 上的三列（`lease_token uuid`、`lease_node text`、`lease_until timestamptz`，见 §3.4 0060），
不是一张新表。规则：

- **租约时间一律用数据库时钟**（`now()`），不用节点 `Now()`：判「过期」的是另一个节点，两边时钟漂移不能参与判定。
- **一轮 tick 一个 token**：阶段 A 本轮插入的所有 pending（以及本轮回收的）共用同一个 `lease_token`（本轮开始时 `crypto/rand` 生成），初始 `lease_until = now() + LeaseTTL`。
  token 交给执行器后，由执行器登记进「本节点持有的 token 集合」，该 token 名下的 run 全部写完终态后移出。`FireNow` 的独立 token 同样登记进**这一个**集合（§3.5），没有第二套心跳。
- **心跳（节点级，一个 goroutine）**：与执行器一起惰性启动（见「总体结构」的生命周期条），每 `LeaseHeartbeat` 执行一次
  `UPDATE schedule_runs SET lease_until = now() + $ttl WHERE lease_token = ANY($held) AND status = 'pending'`，
  一条语句续上本节点**全部**还没写终态的 run（包括在执行器队列里还没开始的那些）。所以阶段 B 积压多久都不影响正确性：
  只要节点活着、能连上库，租约就一直有效，**陈旧阈值不再需要从最坏执行时长推算**。
- **自我围栏（fencing）**：心跳 goroutine 记录最后一次续约成功时的本地单调时钟 `t_ok`。若到 `t_ok + LeaseTTL − FenceMargin` 仍没有续约成功
  （库连不上、语句报错），就**取消执行器当前一代的 ctx 并清空持有集合**：正在进行的 Linear 请求随 ctx 中止，队列里的 run 不再开始，
  它们留给租约过期后的回收。之后的 tick 照常认领（新 token、新一代 ctx），库恢复后自然恢复。
  这样在别的节点能认定租约过期（`lease_until < now()`）之前至少 `FenceMargin` 秒，本节点已经停手，不会出现「B 已经接手、A 还在建单」。
- **常量**（`internal/schedule`，不做配置，理由见 §3.7）：`LeaseTTL = 2m`、`LeaseHeartbeat = 20s`、`FenceMargin = 45s`。
  约束关系写成 `init` 里的断言并有单测：`LeaseTTL ≥ 6·LeaseHeartbeat`（容忍连续 5 次心跳失败前不围栏）、`FenceMargin > linear 客户端超时 30s`
  （围栏触发后，最后一个已发出的请求在租约真正过期前已经超时返回）、`LeaseTTL − FenceMargin > LeaseHeartbeat`。
  `Runner` 上有同名的未导出覆盖字段，**只给测试缩短时间用**，零值取常量；不对应任何环境变量。

**阶段 B：执行 pending（事务外，网络调用）**

本轮要执行的 run 有两个来源：

- **新认领**：阶段 A 本轮插入成功（`RETURNING id` 有值）的 pending，已经带着本轮的租约。
- **回收**：租约已过期的 pending（节点崩溃或被围栏后遗留），在 `TickOnce` 里用**原子认领**拿过来，每 tick 最多 `ClaimLimit` 条，和新认领一起交给执行器：

```sql
UPDATE schedule_runs r
   SET lease_token = $tok, lease_node = $node, lease_until = now() + $ttl,
       attempts = r.attempts + 1
 WHERE r.id IN (SELECT id FROM schedule_runs
                 WHERE status = 'pending' AND lease_until < now()
                 ORDER BY lease_until, id
                 LIMIT $claimLimit FOR UPDATE SKIP LOCKED)
   AND r.status = 'pending' AND r.lease_until < now()   -- 子查询之外再判一次：Read Committed 下重新评估，防止拿到刚被续约的行
RETURNING r.*;
```

  两个节点同时回收同一行：`SKIP LOCKED` 让后到的跳过；即使绕过（例如前一个已提交），外层 `lease_until < now()` 在新租约下为假，后到的拿到 0 行。
  **正在执行的 run 不会被当成陈旧**：它的租约一直在被心跳续着，`lease_until` 永远在未来。

`attempts` 的含义是「为执行它而取得租约的次数」：阶段 A / `FireNow` 插入 pending 时显式写 1（skipped 行保持默认 0），每次回收 +1。

`create_sent_at`（0060 的一列）：执行步骤 4 在**发出 `issueCreate` 之前**用条件 UPDATE 写入
（`SET create_sent_at = now() WHERE id=$run AND status='pending' AND lease_token=$tok`，0 行 = 已丢租约，本条直接放弃、不发请求）。
它把回收来的 run 分成两类：`create_sent_at IS NULL` 说明上一个持有者**从没发出过建单请求**（例如在执行器队列里排着时节点崩溃），
按全新执行处理（不查单、与 `ReplayStalePending` 无关）；非 NULL 才是「结果未知」，走第 3 步的查单。这样排队积压后崩溃不会被误标 `interrupted`。

每条 run 的执行步骤（新认领与回收共用；执行器拿到 run 时才执行，不论它排队了多久）：

1. **前置检查（只读库，不取凭据、不调 Linear）**：重新读属主 `users.disabled_at` 与规则 `schedules.enabled`。
   - 属主已停用：`create_sent_at IS NULL` → 记 `skipped`、`reason='owner_disabled'`；非 NULL（结果未知）→ 记 `failed`、`reason='owner_disabled'`、
     `error='属主已停用，未查询 Linear；上次建单结果未知，请管理员到 Linear 确认'`。两种情况都**不调 `LinearFor`、不调 `IssueByID`、不调 `issueCreate`**。
   - 规则已停用（人手停用或自动暂停）且 `trigger <> 'manual'`：同上分两种，`reason='schedule_disabled'`。`manual` 是人明确点的「立即运行」，不看规则开关，只看属主。
   - 这一步对新认领同样执行：执行器排队期间属主被停用，同样不该再用他的令牌。规则被删除时 run 已级联删除，终态写入拿到 0 行，按「丢租约」处理（slog.Warn）。
   - `owner_disabled` / `schedule_disabled` 不计入 `consecutive_failures`、不改 `last_error`。
2. `LinearFor(uid)` 拿 `LinearHandle`；拿不到就记 `failed`，`error="Linear 凭据不可用：…"`（过兜底层脱敏）。之后本条 run 的所有错误串都先过 `handle.Redact` 再过兜底层（§3.6）。
3. **回收来的、`create_sent_at` 非 NULL 的 run 先查**：`IssueByID(linear_issue_uuid)`，三种结果（判定依据见 §3.2.3）：
   - 查到：说明上次建单其实成功了，直接补记 `created`（写入三字段），**不再调 issueCreate**；
   - 确认不存在 `(nil, nil)`：按下面第 7 步处理；
   - 返回 error：**不重放、不当作不存在**，记 `failed`、`reason='interrupted'`，
     `error='结果未知（按 UUID 查单失败：<脱敏后的原错误>），请到 Linear 确认是否已建单'`（先过 Redact + 兜底层，再按 rune 截到 1000）。
     与 `interrupted` 同样不计入 `consecutive_failures`。理由：查不清就不该押宝重放，退化为「至多一次 + 界面可见」。
4. 渲染模板（§3.3），组装 `CreateIssueInput{ID: uuid, TeamID, Title, Description, Priority, AssigneeID | LabelIDs}`：
   - `intake_mode='assign'`：`AssigneeID = handle.UserID`；为空就记 `failed`，提示去设置页验证 Linear 令牌。
   - `intake_mode='label'`：`name = settings.WebhookTriggerLabel`，为空就记 `failed`，提示管理员未开启标签接单。
     否则 `LabelIDByName(team, name)`，找不到也记 `failed`。
   - 组装成功后、调 `CreateIssue` 之前写 `create_sent_at`（条件 UPDATE，0 行即放弃）。重放时该列已非 NULL，不再改写。
5. `CreateIssue` 成功：`status='created'`，写入 `linear_issue_id/identifier/url`、`finished_at`；
   规则行 `consecutive_failures=0, last_error=NULL`。
6. 失败：`status='failed'`，`error` 先脱敏（§3.6）再按 rune 截到 1000；规则行 `consecutive_failures+1, last_error=…`。
   `consecutive_failures ≥ 5`（常量）时**自动暂停**：`enabled=false, next_fire_at=NULL, paused_reason='连续 5 次建单失败，已自动暂停：<最近错误>'`。
7. 回收来的 run 在第 3 步**确认不存在**时，行为由包级常量 `ReplayStalePending` 决定（默认 `false`，见 §6 R1）：
   - `false`（退路，至多一次）：不调 `issueCreate`，直接记 `failed`、`reason='interrupted'`，`error='结果未知，请到 Linear 确认是否已建单'`；
   - `true`（R1 取证成功后）：按同一个 `linear_issue_uuid` 重放；回收时 `attempts > 3` 就不再重放，记 `failed`、`reason='interrupted'`。
   - `interrupted` **不计入** `consecutive_failures`、不改 `last_error`：它是节点崩溃的后果，不是这条规则的错，不应把规则推向自动暂停。

**终态写入：条件更新，规则计数只跟着「真的改到了」走**

每一次终态写入（`created` / `failed`，含 `interrupted`；以及阶段 B 第 1 步写的 `skipped`）都是一个短事务：

```sql
BEGIN;
UPDATE schedule_runs SET status = $st, reason = $reason, error = $err, linear_issue_id = ..., finished_at = now()
 WHERE id = $run AND status = 'pending' AND lease_token = $tok;     -- 只有当前租约持有者能写终态
-- Go 侧：RowsAffected() == 1 才执行下面这条；== 0 说明租约已被别人接手或该行已有终态 → ROLLBACK，slog.Warn，不动规则行
UPDATE schedules SET consecutive_failures = ..., last_error = ..., [enabled=false, next_fire_at=NULL, paused_reason=...] WHERE id = $sid;
COMMIT;
```

- 两次写入不可能都生效：同一行只有一个 `lease_token` 能匹配，且第一次写入后 `status` 不再是 `pending`。**最终状态与写入先后无关**——
  它只取决于写入时谁持有租约。
- 规则计数（`consecutive_failures`、`last_error`、自动暂停）与 run 终态在**同一事务**里、且只在 run 那条 UPDATE 真的改到一行时才写，
  所以不会出现「run 没改成、计数却 +1」或「同一次失败被两个节点各算一次」。
- 被围栏或丢了租约的节点，如果其 Linear 请求其实已经成功：它的 `created` 写入拿到 0 行，`slog.Error` 记下 `issue`（identifier）与 `run`。
  接手节点在第 2 步按 UUID 查单——因为围栏让 A 至少提前 `FenceMargin`（> 请求超时）停手，A 发出的最后一个请求在 B 查询之前已有结果，
  B 能查到单并补记 `created`。这把「A 实际建了单、库里却记 interrupted」压缩到「Linear 已受理但查询还查不到」这种外部一致性窗口，
  且退路下的文案本来就是「结果未知，请到 Linear 确认」，不会谎报。

**为什么不在 tick 里自动重试 failed**：失败多半是凭据失效或团队无权限这类持续性错误，重试只会刷屏。
时刻性的东西错过就是错过了，用户可以点「立即运行」补一张。只有**结果未知**的崩溃遗留（租约过期后被回收的 pending）才可能重放（且仅在 `ReplayStalePending=true` 时），而且有 UUID 幂等兜底。

#### 3.2.3 Linear 侧新增（`internal/integration/linear/schedule.go`，新文件，避免与邻居改同一文件）

```graphql
mutation($input: IssueCreateInput!) {
  issueCreate(input: $input) { success issue { id identifier url } }
}
query { teams(first: 100) { nodes { id key name } } }
query($name: String!) {
  issueLabels(first: 50, filter: { name: { eqIgnoreCase: $name } }) { nodes { id name team { id } } }
}
query($id: String!) { issue(id: $id) { id identifier url } }  # 复用 Issue 的形状但只取三个字段
```

- `CreateIssueInput.ID` 映射到 `IssueCreateInput.id`：Linear 文档允许客户端传入 UUID v4 作为 issue id。
  用同一个 id 重放时，要么查到已有的单，要么服务端拒绝重复，所以「同一时刻只建一张单」在 Linear 侧也成立。
  这一点**需要对真 Linear 取证**（§6 R1），取证结论落在常量 `ReplayStalePending`（默认 `false`）；取证失败或未取证时的退路是：陈旧 pending 不重放，直接记 `failed reason='interrupted'`
  并提示「结果未知，请到 Linear 确认」，从至少一次退化为至多一次，仍然不会重复建单。
- `LabelIDByName`：优先取 `team.id == teamID` 的团队标签，其次取 `team == null` 的工作区标签。
- **`IssueByID` 的 not-found 判定**：
  - 现状：`do()`（`linear.go:309-315`）把所有 GraphQL 错误合并成一条无类型的 `fmt.Errorf("linear: GraphQL 错误: …")`，且 `graphQLError` 只解 `message`
    （`linear.go:268-271`），调用方无法区分「不存在」与「无权限/临时故障」。**不改 `do()` 和 `graphQLError`**（守住 AC-19 的 hunk 断言），
    而是在新文件 `schedule.go` 里写一个本包私有的 `doEnvelope(ctx, query, vars) (data json.RawMessage, errs []gqlErrorDetail, err error)`：
    发请求、HTTP 非 200 照 `do()` 的写法返回 error（错误体经已修复的 `truncate`），200 时把 `data` 与 `errors[]` **原样**交回，
    `gqlErrorDetail{Message string; Path []any; Extensions struct{Code, Type string; UserPresentableMessage string}}`。复制约 25 行请求代码，
    理由同 §2.8 的 UUID：重复的代价小于改一个邻居也在读的既有函数。`CreateIssue`/`Teams`/`LabelIDByName` 仍走 `do()`。
  - 判定规则（**白名单，判不准一律当错误**）：`IssueByID` 返回 `(nil, nil)` 当且仅当 `data.issue` 为 `null`（或 `data` 缺 `issue` 键）**且**以下二者之一：
    ① `errors` 为空；② `errors` 非空，且**每一条**都满足 `extensions.code == "ENTITY_NOT_FOUND"`，或 `extensions.type == "invalid input"` 且 `message` 以 `Entity not found` 开头。
    其余所有情况（HTTP 非 200、网络错误、`errors` 里有任何一条不满足上述条件——例如 `AUTHENTICATION_ERROR`、`FORBIDDEN`、`RATELIMITED`、`INTERNAL_SERVER_ERROR`、
    无 extensions 的未知错误）一律返回 `(nil, err)`，err 串带上各条 `extensions.code` 与 `message`。
  - 为什么是白名单：两种误判的代价不对称。把「不存在」误判为错误 → 记 `interrupted`、不重放，至多一次、界面可见；
    把「临时故障/无权限」误判为不存在 → `ReplayStalePending=true` 时会重放，安全性全押在 R1 上。所以只放过能确认的形状。
  - 取证：真 Linear 对不存在 id 的确切载荷属于 R1 同一次取证（非生产工作区）。取证拿到的形状若不在上面白名单里，
    就把它加进白名单并在测试里加一条 fixture；**没取证时白名单按上面的写法落地**，最坏后果是「不存在」被当错误处理（安全侧）。
    `ReplayStalePending=true` 的前提（AC-13 末条 shell 断言）同步扩成「两个取证文件都存在」：只有确认了 not-found 形状，重放才有意义。
- 修 `linear.go:523` 的 `truncate`：改成按 rune 截断，并在截断前 `strings.ToValidUTF8`。
- **`tracker.Tracker` 接口不动。**

#### 3.2.4 绝不直接改代码（G3）的结构保证

- 调度器的全部副作用只有三处：写 `schedules`/`schedule_runs` 两张表，以及调 Linear `issueCreate`。
- 代码层面：不 import `task`/`runner`（AC-9）；`internal/schedule` 与 `internal/store/schedules.go` 里没有 `INSERT INTO tasks` / `UPDATE tasks`（AC-9 用 grep 断言）。
- 运行层面：AC-10 跑完整 tick 后断言该用户 `tasks` 行数为 0。
- 任务行只可能经由 webhook → `Enqueue` 产生，这是现有路径，本 PR 不做改动（AC-11 用现有测试守住）。

### 3.3 模板

- 占位符**白名单**（`{{name}}` 形式，前后可有空格），用 `strings.Replacer` 替换，**不用 `text/template`**：
  模板由用户输入，`text/template` 能调用方法、做循环，等于开了一个小型执行面，而这里只需要变量替换。

| 占位符 | 值（按规则时区） |
|---|---|
| `{{date}}` | `2026-10-19` |
| `{{time}}` | `09:00` |
| `{{weekday}}` | `周一` |
| `{{datetime}}` | RFC3339，如 `2026-10-19T09:00:00+08:00` |
| `{{week}}` | ISO 周，如 `2026-W43` |
| `{{schedule}}` | 规则名 |

- 保存时 `ValidateTemplate`：出现白名单以外的 `{{…}}` 返回 400，并指出是哪个占位符，免得到点才发现标题里有一个原样的 `{{dat}}`。
- 长度上限（按 rune 计）：规则名 ≤100，标题模板 ≤200，正文模板 ≤8000。渲染后标题按 rune 截到 255（Linear 标题上限）。
- 正文末尾固定追加一行溯源：`\n\n---\n由 Lathe 定时规则 #<id>「<name>」按计划时刻 <datetime> 创建。`
  这行字会进入分诊上下文（`tracker.Issue.Context` 会带上描述），可以接受：它告诉 agent 这张单从哪来，不含凭据。

### 3.4 数据模型（迁移 0059、0060）

#### 0059_schedules.up.sql

```sql
-- 0059_schedules — 定时建单规则（docs/features/scheduled-issue.md）
CREATE TABLE schedules (
  id                bigserial PRIMARY KEY,
  user_id           bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name              text        NOT NULL,
  enabled           boolean     NOT NULL DEFAULT true,
  -- 启用（含新建、编辑、重新启用）时刻：补偿的基线下限，停用期间的时刻不补
  enabled_at        timestamptz NOT NULL DEFAULT now(),

  kind              text        NOT NULL,
  minute_of_day     int,                  -- daily/weekly
  weekdays          smallint,             -- weekly：bit0=周日 … bit6=周六
  interval_minutes  int,                  -- interval
  anchor_at         timestamptz,          -- interval
  timezone          text        NOT NULL DEFAULT 'UTC',

  linear_team_id    text        NOT NULL,
  linear_team_key   text        NOT NULL DEFAULT '',   -- 展示用
  intake_mode       text        NOT NULL DEFAULT 'assign',
  title_template    text        NOT NULL,
  body_template     text        NOT NULL DEFAULT '',
  priority          smallint    NOT NULL DEFAULT 0,

  next_fire_at      timestamptz,          -- 停用时为 NULL
  last_slot_at      timestamptz,          -- 最近一次已处理（含 skipped）的计划时刻
  consecutive_failures int      NOT NULL DEFAULT 0,
  last_error        text,
  paused_reason     text,                 -- 自动暂停原因；人重新启用时清空

  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT schedules_kind_check CHECK (kind IN ('daily', 'weekly', 'interval')),
  CONSTRAINT schedules_intake_check CHECK (intake_mode IN ('assign', 'label')),
  CONSTRAINT schedules_priority_check CHECK (priority BETWEEN 0 AND 4),
  CONSTRAINT schedules_shape_check CHECK (
    (kind = 'daily'    AND minute_of_day BETWEEN 0 AND 1439 AND weekdays IS NULL AND interval_minutes IS NULL)
 OR (kind = 'weekly'   AND minute_of_day BETWEEN 0 AND 1439 AND weekdays BETWEEN 1 AND 127 AND interval_minutes IS NULL)
 OR (kind = 'interval' AND minute_of_day IS NULL AND weekdays IS NULL
                       AND interval_minutes BETWEEN 60 AND 10080 AND interval_minutes % 60 = 0
                       AND anchor_at IS NOT NULL)),
  -- 双向等价：启用 ⇔ 有下次触发时刻。只写 `enabled OR next_fire_at IS NULL` 会放过
  -- enabled=true AND next_fire_at IS NULL —— 到期扫描 `next_fire_at <= $now` 对 NULL 为假，
  -- 这条规则界面上显示「启用」却永远不触发，正是 §3.7「不静默降级」要防的那类，所以在写入时就挡掉。
  CONSTRAINT schedules_enabled_next CHECK (enabled = (next_fire_at IS NOT NULL)),
  CONSTRAINT schedules_user_name_unique UNIQUE (user_id, name)
);
CREATE TRIGGER schedules_updated_at BEFORE UPDATE ON schedules
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- 调度器扫描到期规则
CREATE INDEX schedules_due ON schedules (next_fire_at) WHERE enabled;
CREATE INDEX schedules_owner ON schedules (user_id, id);
```

down：`DROP TABLE schedules;`（触发器与索引随表删除）。

#### 0060_schedule_runs.up.sql

```sql
-- 0060_schedule_runs — 每个计划时刻的触发记录；(schedule_id, scheduled_for) 唯一 = 多节点只触发一次
CREATE TABLE schedule_runs (
  id                bigserial PRIMARY KEY,
  schedule_id       bigint      NOT NULL REFERENCES schedules(id) ON DELETE CASCADE,
  user_id           bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,  -- 冗余，供属主过滤
  scheduled_for     timestamptz NOT NULL,
  trigger           text        NOT NULL,           -- schedule | catchup | manual
  status            text        NOT NULL DEFAULT 'pending',
  reason            text,                           -- missed_window | owner_disabled | schedule_disabled | interrupted
  missed_count      int         NOT NULL DEFAULT 0, -- 本次补偿合并掉的更早时刻数
  linear_issue_uuid uuid        NOT NULL,           -- 认领时预生成，issueCreate.input.id
  linear_issue_id   text,
  linear_issue_identifier text,
  linear_issue_url  text,
  error             text,                           -- 已脱敏、已按 rune 截断
  attempts          int         NOT NULL DEFAULT 0,
  node              text        NOT NULL DEFAULT '',-- 最初认领节点（LATHE_NODE_NAME），排障用
  -- 执行租约（§3.2.2）：pending 行必有；终态行保留最后一个持有者，供排障
  lease_token       uuid,                           -- 一轮 tick（或一次立即运行）一个 token
  lease_node        text,                           -- 当前/最后持有者节点
  lease_until       timestamptz,                    -- 数据库时钟；心跳续约；< now() 即可被回收
  create_sent_at    timestamptz,                    -- 发出 issueCreate 前写入；NULL = 从未发出过，回收时按全新执行（§3.2.2）
  created_at        timestamptz NOT NULL DEFAULT now(),
  finished_at       timestamptz,

  CONSTRAINT schedule_runs_trigger_check CHECK (trigger IN ('schedule', 'catchup', 'manual')),
  CONSTRAINT schedule_runs_status_check CHECK (status IN ('pending', 'created', 'failed', 'skipped')),
  CONSTRAINT schedule_runs_slot_unique UNIQUE (schedule_id, scheduled_for),
  CONSTRAINT schedule_runs_uuid_unique UNIQUE (linear_issue_uuid),
  -- pending 必须有人持有租约，否则它既不会被执行也不会被回收（回收条件是 lease_until < now()，NULL 为假）
  CONSTRAINT schedule_runs_pending_leased CHECK (status <> 'pending' OR (lease_token IS NOT NULL AND lease_until IS NOT NULL))
);
CREATE INDEX schedule_runs_history ON schedule_runs (schedule_id, scheduled_for DESC);
CREATE INDEX schedule_runs_pending ON schedule_runs (lease_until) WHERE status = 'pending';   -- 回收扫描
CREATE INDEX schedule_runs_lease ON schedule_runs (lease_token) WHERE status = 'pending';    -- 心跳续约
```

down：`DROP TABLE schedule_runs;`

- 两张都是**新的空表**，不需要 `CONCURRENTLY`，也不用非事务标记（§6.3 只针对大表）。
- 一条迁移只做一件事：0059 建规则表，0060 建运行记录表。0061 不用。
- 不改任何历史迁移，也不改 `tasks` / `integrations` 的约束。
- 「建了单但没被接单」**不另存字段**，从 `tasks` 派生（§3.5 runs 列表做 LATERAL 只读 join：
  `tasks.user_id = r.user_id AND tasks.tracker_provider = 'linear' AND tasks.external_id = r.linear_issue_id`）。
  这样不会多出一个和 `tasks` 状态打架的真相源。

#### 关于号段与「不跳号」（§6.6）

0059 前面的 0032–0058 由并行线 f01–f09 占用。按协调好的号段合入后整体递增、不复用；若某条线最终没合入，就会留下空号。
仓库已有空号先例（0026/0027/0029），`loadMigrations` 允许空号。如果审阅者要求严格连续，合入前在本分支把号**往前挪**
（未发布的迁移允许改号，不算回改历史迁移）。

### 3.5 HTTP API（`internal/httpapi/schedules.go`，新文件）

全部 `Auth.RequireFunc`，按 `CurrentUser` 隔离。**非属主与不存在一律 404**（`ErrScheduleNotFound`，store 层 `WHERE id=$1 AND user_id=$2`）。

| 方法 路径 | 说明 | 主要响应 |
|---|---|---|
| `GET /api/schedules` | 我的规则 + 每条最近一次 run 摘要 | 200 `{schedules:[…]}` |
| `POST /api/schedules` | 新建 | 201 规则行；400 校验失败；409 同名规则已存在；502 Linear 不可达（校验团队/标签时，不保存） |
| `GET /api/schedules/{id}` | 详情 | 200 / 404 |
| `PUT /api/schedules/{id}` | 全量更新（含 `enabled`） | 200 / 400 / 404 / 409 改名撞同名 / 502 Linear 不可达（不保存） |
| `DELETE /api/schedules/{id}` | 删除（runs 级联删除；Linear 上已建的单不动） | 204 / 404 |
| `POST /api/schedules/{id}/run` | 立即运行：`scheduled_for = now` 截到分钟，`trigger='manual'`，同步走阶段 B | 202 run 行；409 同一分钟已有记录；404；503 调度器已关闭（`LATHE_SCHEDULER_ENABLED=false`，在 404 判定之后） |
| `GET /api/schedules/{id}/runs?limit=50` | 运行记录 + 派生的 `task:{id,state}\|null` | 200 / 404 |
| `POST /api/schedules/preview` | 无副作用：给定规则返回接下来 5 个时刻及人读描述 | 200 / 400 |
| `GET /api/linear/teams` | 当前用户 Linear 可见的团队（加在 `LinearAPI` 里，只读） | 200 / 400 凭据不可用 / 502 |

**保存校验**（POST/PUT，`enabled=true` 时全部生效；`enabled=false` 只做形状校验）：

1. 形状：`Spec.Validate`（kind、范围、weekdays≠0、interval 是 60 的倍数且 ≥60、时区能加载），模板白名单与长度，`priority ∈ 0..4`，名称非空且本人名下唯一（冲突返回 409）。
2. 数量：每个用户最多 20 条规则（常量），超出返回 400。
3. 接单前提，缺了就 400 并说明怎么修：
   - 属主 Linear 令牌可用（`ProviderFor(uid).Linear` 成功）；
   - 属主 Linear webhook 密钥已配置（`WebhookSecret != ""`）。**否则单建出来也不会被接**；
   - `assign`：`LinearUserID != ""`；`label`：系统设置 `webhook_trigger_label` 非空。
4. 团队在 `Teams()` 结果里（防止填错 id）；`label` 模式下 `LabelIDByName` 能找到。Linear 不可达时返回 502，不保存。
5. 启用语义：新建、编辑、或由停用转启用时，`enabled_at = now`、`last_slot_at = now`、`next_fire_at = Next(now)`、
   `consecutive_failures = 0`、`paused_reason = NULL`。**不补停用期间和编辑之前的时刻**，否则改个标题就可能立刻建一张单，违反直觉。
   停用（人手停用或自动暂停）时 `next_fire_at = NULL`。`enabled` 与 `next_fire_at` **必须在同一条 UPDATE 里一起写**，
   否则会撞 `schedules_enabled_next`（`enabled = (next_fire_at IS NOT NULL)`）——这条 CHECK 就是本条逻辑的数据库兜底：
   哪个代码路径忘了算 `next_fire_at`，写入时就报 23514，而不是留下一条「启用但永不触发」的行。
6. `anchor_at`（interval）：请求没给时取 now 截到整点。

`POST /run` 与调度器复用同一个 `Runner.FireNow`，同样受唯一约束保护：一分钟内连点两次，第二次返回 409。
`FireNow` 先 `ensureStarted`（与 `TickOnce` 同一个 `sync.Once`，§3.2.2「总体结构」生命周期条），插入的 pending 同样带租约（独立 token，登记进**节点级心跳**的持有集合，不另起心跳），在请求 goroutine 里同步执行阶段 B（与执行器同一个执行函数，不占执行器的并发名额；执行用的 ctx 同时受请求 ctx 与执行器当前一代 ctx 约束，所以围栏同样能中止它），终态写入同样带 `WHERE status='pending' AND lease_token=$tok`；写完终态后从持有集合移出；
请求中途断开或节点崩溃时，这一行与 tick 遗留的 pending 一样由租约过期后回收。它占掉的时刻会让随后的 tick 在 INSERT 处冲突，但规则照常前移（§3.2.2 阶段 A 第一条）。

`api.js` 增加对应方法；响应字段用 camelCase，与现有 API 一致。

### 3.6 安全

- **凭据**：本功能不新增任何凭据，只复用属主已有的 `linear`、`linear_webhook`（AES-256-GCM 入库，`internal/creds`）。
  不复用管理员兜底：`ProviderFor(uid)` 本来就只给 `adminID` 发 env 兜底（`internal/creds/factory.go:49-52`）。
- **不出明文**：`slog` 只记 `schedule`、`run`、`slot`、`issue`（identifier）、`node`、`err`。错误串在落库和日志之前过 `schedule.redactErr`，两层，顺序即优先级
  （照 `internal/runner/redact.go` 的分层立场，但不 import runner，形态正则复制一行）：
  1. **权威层：精确替换**。调 `LinearHandle.Redact`（§3.2.1），把本次用到的 Linear token 值逐字节替换成 `[REDACTED]`。
     不依赖 token 的形态：用户配的是 OAuth token 或别的格式也照样命中。承载点是 `LinearFor` 的返回值，token 明文只存在于装配方闭包里。
  2. **兜底层：形态替换**。`\blin_api_[A-Za-z0-9]{16,}\b` → `lin_api_[REDACTED]`（与 `runner/redact.go:389` 同一条正则）。
     盖「错误里出现了别的 Linear key、不是本次那把」的情况；尽力而为，不是通过条件。
  - `LinearFor` 本身失败（凭据取不到）时没有 Redact 可用，错误串只来自 `creds`（`ErrNotConfigured` / 解密失败），不含 token 值；仍过一遍兜底层。
  - Linear 的错误响应一般不回显 token，但「一般不」不算防线。另外要求 `CreateIssue` 失败的错误不带请求体（请求体里有模板内容，但没有 token）。
    AC-13 分别断言两层：非 `lin_api_` 形态的 token 被精确层抹掉；另一把 `lin_api_` key 被形态层抹掉。
- **属主隔离**：所有读写都带 `user_id` 条件；runs 的派生 task join 也带 `tasks.user_id = r.user_id`，不会把别人的任务挂进来。
  调度器用 `schedules.user_id` 的凭据，不存在拿 A 的令牌给 B 建单的路径。
- **停用用户**：调度器不调 Linear，**两道检查都在 `LinearFor` 之前**：阶段 A 认领时（插 `skipped/owner_disabled`），以及阶段 B 每条 run 执行前的第 1 步
  （§3.2.2；新认领、回收、`FireNow` 的同步执行共用这一步：从未发出过建单的记 `skipped/owner_disabled`，结果未知的记 `failed/owner_disabled`）。
  所以「认领之后、执行或回收之前被停用」的属主，其令牌不会再被取出解密，也不会有 `IssueByID`/`issueCreate`。
  规则在此期间被停用或自动暂停同理（`schedule_disabled`，`manual` 除外）。
- **输入**：模板只做字面替换，没有执行面；所有长度按 rune 计；请求体复用 `decodeJSON`（`maxJSONBody` 1MB）。
- **字符串截断按 rune**（§9）：`error`、渲染后的标题都按 rune 截断；同时修复 `linear.truncate` 的字节截断（AC-14 先红后绿）。

### 3.7 配置项（每项写明消费方；都登记 README 变量表；非法值启动报错）

| 环境变量 | 默认 | 合法值 | 消费方 | 非法时 |
|---|---|---|---|---|
| `LATHE_SCHEDULER_ENABLED` | `true` | `true` / `false` | `cmd/lathe/main.go` `startWorkers`：为 `false` 时不启动 `schedule.Runner`，并打一条 Warn。API 照常可用（可以先配规则），`POST /run` 返回 503「调度器已关闭」 | `config.Validate` 用 `validateBoolEnv` 报错 |
| `LATHE_SCHEDULE_CATCHUP_WINDOW` | `6h` | Go duration，`0` ≤ x ≤ `168h`；`0` 表示**只准点触发、迟到（`lag > 2·tick`）一律 skipped**，准点触发不受影响 | `schedule.Runner.CatchUpWindow`（阶段 A 判定第 3 条：迟到超过 `OnTimeGrace` 的那次是 pending 还是 skipped，见 §3.2.2） | `envDurationStrict` 解析失败或越界时 `Load` 报错（提示不支持 `d` 单位） |

**刻意不做成配置的常量**（写在 `internal/schedule` 里，带注释说明理由）：tick 30s、准点宽限 `OnTimeGrace = 2·tick = 60s`、`ReplayStalePending`（默认 false，见 R1）、租约 `LeaseTTL = 2m` / 心跳 `LeaseHeartbeat = 20s` / 围栏余量 `FenceMargin = 45s`（三者的约束关系见 §3.2.2「租约」，不再有按 `created_at` 推算的陈旧阈值）、
每条 run 最多执行 3 次（含首次，`attempts > 3` 不再重放）、连续失败 5 次自动暂停、每 tick 最多认领 `ClaimLimit = 200` 条、执行并发 `ExecConcurrency = 4`、每人最多 20 条规则、最小间隔 60m。这些值不同部署之间没有调整的理由，
做成配置只会多出没人用的配置项（铁律 3）。

为什么补偿窗口默认 6h：典型用例是「每天 09:00」。凌晨重启、停机几小时后起来仍会补一次；停机超过 6h，这张单大概率已经过时，
用户也会注意到服务停过，所以记一条 `skipped` 而不是事后补一张单。

### 3.8 补偿（missed）策略：写清楚

| 场景 | 行为 |
|---|---|
| 正常 | 计划时刻后 ≤30s 内认领（`lag ≤ 2·tick` 一律 pending，与窗口无关），`trigger=schedule`。认领不等阶段 B，别的规则的 Linear 再慢也不推迟这一步 |
| 停机或积压，错过 1 个以上时刻，最近一个错过的时刻距今 ≤ 窗口 | **只为最近那个时刻建一张单**，`trigger=catchup`，`missed_count` = 被合并掉的更早时刻数；更早的时刻不单独留记录 |
| 最近错过的时刻距今 > max(窗口, 2·tick) | 为那个时刻记一条 `skipped/missed_window`，不建单；`next_fire_at` 前移 |
| `LATHE_SCHEDULE_CATCHUP_WINDOW=0` | 准点（`lag ≤ 2·tick`）照常建单；晚于 2·tick 的一律 `skipped`。三处（§3.2.2 判定顺序、§3.7、本表）同一口径 |
| 规则停用期间 | 不补；重新启用时基线重置为启用时刻 |
| 编辑规则 | 不补；基线重置为编辑时刻 |
| 节点在认领后、建单前崩溃 | 心跳停止，租约至多 `LeaseTTL`（2m）后过期；任一节点用条件 UPDATE 原子回收（§3.2.2）：先过停用检查；`create_sent_at IS NULL`（从未发出建单）按全新执行；否则按 UUID 查单（查单报错 → `failed/interrupted`，不重放），查到补记 `created`；查不到时 `ReplayStalePending=false`（默认）记 `failed/interrupted`，`=true` 则按 UUID 幂等重放（含首次最多执行 3 次）。`interrupted` 不计入连续失败 |
| 阶段 B 积压（例如 Linear 慢，20 条 × 每条三个 30s 超时 ≈ 10+ 分钟） | 认领循环照常每 30s 一轮（阶段 B 在执行器里异步跑），积压期间到期的规则仍按准点认领、`trigger=schedule`；执行延迟只体现在 `finished_at`。节点级心跳每 20s 续约本节点全部未完成的 run（含队列里未开始的），别的节点看不到「过期」，不会接手；只有节点失联（续约失败）才会围栏停手、由别人回收 |
| 单节点准点的残余边界 | 节点停机、数据库卡顿 > 30s、或连续两轮到期规则 > `ClaimLimit`（200）时，认领时 lag 可能超过 60s，按迟到处理（窗口内 catchup，窗口 0 时 skipped）。不是「永远准点」，是「不因 Linear 慢而失准」 |
| 认领后、执行或回收前属主被停用 / 规则被停用 | 阶段 B 第 1 步拦下，不取凭据、不调 Linear：未发出过建单 → `skipped`，结果未知 → `failed`，`reason` 为 `owner_disabled` / `schedule_disabled`；不计入连续失败 |
| 回收时按 UUID 查单报错（限流、无权限、5xx） | 不重放、不当不存在：`failed/interrupted`，错误脱敏后落库（§3.2.2 第 3 步） |
| 节点与库失联但进程还活着 | 续约失败满 `LeaseTTL − FenceMargin` 即取消执行器当前一代的全部阶段 B；它之后的终态写入因 `lease_token` 不匹配写 0 行，不覆盖接手节点的结果、不动规则计数 |
| 多节点同时 tick | `SKIP LOCKED` 让它们各认领不同规则；同一规则同一时刻靠唯一约束只成一次 |

「只补最近一次」的理由：定时单是「这个周期该做的事」，停机三天错过三次每日依赖升级，补三张内容一样的单只会让 agent 跑三轮、开三个重复 PR。

### 3.9 UI（`web/src/views/Schedules.vue`，路由 `/schedules`，导航「定时建单」，`hasLinearToken()` 时可见）

- **列表**：名称、规则描述（如「每周一、三 09:00（Asia/Shanghai）」）、下次触发时间、最近一次结果徽标
  （created=ok / failed=bad / skipped=warn / pending=run）、启用开关。被自动暂停的规则整行标 warn，并显示 `paused_reason`。
- **新建/编辑弹窗**（`BaseDialog`）：名称；类型单选；时间（`<input type="time">`）；星期复选；间隔小时数；时区（默认取浏览器时区）；
  Linear 团队下拉（`GET /api/linear/teams`）；接单方式单选（label 选项显示当前系统标签名，未配置时置灰并说明）；
  标题/正文模板（下方列出可用占位符）；优先级。编辑时调用 `POST /api/schedules/preview`，实时显示接下来 5 次触发时间。
- **运行记录**（展开行）：计划时刻、触发方式、状态、Linear 单链接、派生任务链接（`/tasks/:id` + 状态），或者在
  「created 超过 10 分钟仍无任务」时显示提示：「单已建好但还没被接单：检查 Linear webhook 是否指向你的回调地址并勾选了 Issue 事件」。
  失败时显示错误原文（已脱敏），`missed_count > 0` 时显示「合并了 N 次错过的时刻」。
- 「立即运行」按钮。
- 改完执行 `make ui && make build`，PR 附手测截图（§3 前端行）。

### 3.10 装配（`cmd/lathe/main.go` + 新文件 `cmd/lathe/schedule.go`）

**Runner 在 `serve()` 里构造，作为新参数传进 `startWorkers`；`startWorkers` 只负责 `go scheduler.Run(ctx)`。**
不能在 `startWorkers` 里构造，有两个独立的原因：

1. **类型**：`linearFor` 需要 `*creds.Factory`（要调 `ProviderFor`，§3.2.1），而 `startWorkers` 的 `factory` 形参是 `runner.ClientFactory`
   （`main.go:511-512`），那个接口只有 `ForUser`（`internal/runner/pipeline.go:51-53`）。在 `startWorkers` 里写 `linearFor(factory)` 编译不过。
   把形参改成 `*creds.Factory` 能编译，但会让 `MergePoller.ClientFactory` 等既有消费方看到的类型变宽，改动面反而更大，不选。
2. **顺序**：`SchedulesAPI{Fire: scheduler}` 要在路由注册时拿到 Runner，而路由注册在 `serve()` 里、`startWorkers` 调用之前
   （`linearAPI` 在 `main.go:292`，`startWorkers` 在 `main.go:480`）。

具体改法：

- 新文件 `cmd/lathe/schedule.go`：
  ```go
  // newScheduler 只构造，不启动任何 goroutine。启动唯一入口是 startWorkers。
  func newScheduler(cfg config.Config, st *store.Store, f *creds.Factory) *schedule.Runner {
      return &schedule.Runner{Store: st, Linear: linearFor(f), Settings: st, Now: time.Now,
          CatchUpWindow: cfg.ScheduleCatchUpWindow, Node: cfg.NodeName}
  }
  func linearFor(f *creds.Factory) schedule.LinearFor { … §3.2.1 … }
  ```
- `serve()`：紧接 `linearAPI.Routes(mux)`（`main.go:298`）之后、`accountAPI := …` 之前，追加一段：先一行 `scheduler := newScheduler(cfg, st, factory)`，再接 `SchedulesAPI` 的注册块。
  `newScheduler` 是纯构造，`linearFor` 每次调用时才 `ProviderFor(uid)`，所以它放在哪个装配语句之后在语义上无所谓，只需满足「构造在路由注册（`SchedulesAPI`）之前、在 `startWorkers` 之前」。
  `SchedulesAPI` 的注册块：
  `(&httpapi.SchedulesAPI{Store: st, Auth: auth, Fire: scheduler, SchedulerEnabled: cfg.SchedulerEnabled(), Preconditions: …}).Routes(mux)`；`LinearAPI` 加 teams 路由。
- **`startWorkers` 签名变更**（既有代码改动，计入 R7）：在参数表**末尾**追加一个参数，
  ```go
  func startWorkers(ctx context.Context, q *queue, pipeline *runner.Pipeline, st *store.Store,
      factory runner.ClientFactory, cfg config.Config, secrets *store.Secrets, scheduler *schedule.Runner)
  ```
  唯一调用点 `main.go:480` 同步改为 `startWorkers(ctx, q, pipeline, st, factory, cfg, secrets, scheduler)`（`grep -rn 'startWorkers(' cmd/` 只有这一处）。
  函数体末尾追加：`if cfg.SchedulerEnabled() { go scheduler.Run(ctx) } else { slog.Warn(...) }`。函数注释里「启动后台执行者：…」一句补上「定时建单调度器」。
  追加在末尾而不是插在中间，是为了和同样要给 `startWorkers` 加参数/加循环的并行线（f09 的 outbox worker）在 rebase 时只撞同一行参数表、
  冲突一眼可解；谁后合入谁把对方的参数并进来。

**与「只有 startWorkers 启动后台循环」这条结构约束怎么对上**：这条约束管的是**启动**（`go` 语句），不管**构造**。现有先例就是这样：
`pipeline`、`q` 都在 `serve()` 里构造（`buildPipeline`、`newQueue`），只在 `startWorkers` 里 `go q.work(ctx)`。Runner 照同一个形状：
`newScheduler` 是纯构造（不起 goroutine、不碰网络、不查库），`serve()` 里没有 `go scheduler.…`。具体到测试：

- `TestServeStartsWorkersAfterEverythingIsWired`：①「禁止裸 go」名单加上 `scheduler.` 前缀（serve 里出现 `go scheduler.Run(...)` 即失败）；
  ② 关注调用序列加上 `newScheduler`，断言它出现在 serve 里且排在 `startWorkers` 之前（`Runner` 必须先构造好再交给启动点）。
- `TestStartWorkersLaunchesAllLoops`：源码里 `startWorkers` 必须有 `go scheduler.Run(`。
- 执行器的工作 goroutine 与节点级心跳由 Runner 在第一次 `TickOnce` / `FireNow` 时惰性起（`sync.Once`），由 `Close()` 结束；`Run` 在其 ctx 取消后调 `Close()`（§3.2.2「总体结构」生命周期条）。
  不在 `cmd/lathe` 里出现新的 `go` 语句，与 `q.work` 内部再起 goroutine 的现状同形，结构约束不变。`newScheduler` 仍是纯构造；
  生产上第一次 `TickOnce` 来自 `startWorkers` 里的 `go scheduler.Run`，`FireNow` 先到（`go` 语句已执行但 goroutine 还没跑到第一轮）时由同一个 `sync.Once` 起，不会起两份。
- `TestStartWorkersSchedulerGatedByConfig`：见 AC-17。
- `FireNow`（「立即运行」）由 HTTP handler 在请求 goroutine 里同步调用，不是后台循环；HTTP 服务在 `startWorkers` 之后才 `ListenAndServe`
  （`main.go:482-487`），所以请求不可能先于 `startWorkers` 到达。`FireNow` **不另起心跳**：它的 token 登记进节点级心跳的持有集合（§3.5）。
  `LATHE_SCHEDULER_ENABLED=false` 时 handler 在调 `Fire` 之前就返回 503，`FireNow` 不会被调用，执行器与心跳不会被惰性起。

**`LATHE_SCHEDULER_ENABLED=false`**：Runner 照样构造（`FireNow` 需要一个非 nil 的接收者来判 503 之前的属主校验，且构造无副作用），
只是 `startWorkers` 不 `go scheduler.Run`。`SchedulesAPI.SchedulerEnabled=false` 时 `POST /api/schedules/{id}/run` 在属主校验（404）**之后**、调 `Fire` **之前**返回 503
`{"error":"调度器已关闭（LATHE_SCHEDULER_ENABLED=false）"}`，不写 `schedule_runs`。先 404 后 503 的顺序是为了不让 503 泄露别人规则的存在性。
其余 API（CRUD、preview、runs）照常可用。

### 3.11 并发 / 多节点小结

- 「这个时刻建不建」的唯一真相是 `schedule_runs_slot_unique`；「这条 pending 现在归谁执行」的唯一真相是该行的 `lease_token`/`lease_until`（§3.2.2）。
  节点之间不需要相互感知，租约是 `schedule_runs` 上的列，不另建租约表。
- 所有终态写入都带 `WHERE status='pending' AND lease_token=$tok`，规则计数只在这条写入改到一行时才在同一事务里更新——结果与写入顺序无关。
- 认领与执行解耦：`TickOnce` 只碰库，阶段 B 在执行器里有界并发地跑；准点判定只依赖认领循环的节拍，不依赖 Linear 的快慢（§3.2.2）。
- `FOR UPDATE SKIP LOCKED` 把同一规则的计算串行化，避免两个节点同时推进 `next_fire_at`，与 `ClaimReady`（`machine.go:830`）同一手法。
- 网络调用不在事务里，事务只持有毫秒级的行锁。租约时间全部取数据库 `now()`，节点时钟漂移不影响「过期」判定。
- Linear 侧用 `input.id = linear_issue_uuid` 防重复建单（R1 取证后生效，否则退化为至多一次）。
- 时钟：比较用节点 `Now()`，时刻由规则推出。节点间时钟漂移 Δ 只会让触发早或晚 Δ，不会造成重复（唯一约束）或丢失（`next_fire_at ≤ now` 迟早成立）。
- `-race`：`internal/schedule` 测试里有两个 Runner 并发 tick 的用例，`make test-race` 的包清单**不改**（Makefile 那行是别的线也在用的公共配置）。
  本线额外执行 `go test -race -p 1 ./internal/schedule/...`，作为 AC-8 的一部分。

---

## 4. 范围外（明确不做）

| 不做 | 理由 / 归属 |
|---|---|
| cron 表达式 | 三种结构化规则已覆盖目标用例，代价见 R5 |
| 定时直接建 Lathe 任务行或直接跑 agent | 违背 G3 与铁律 1/2 |
| 建内置工单（LT-）/ 云效 / GitLab / Gitea 的单 | Linear 是唯一要求；云效 Tracker 是 **f08**，代码平台是 **f07**。本 PR 不在 `tracker.Tracker` 上加方法，后续要扩只需给 `IssueCreator` 加实现 |
| 失败推送（Slack / 飞书 / 邮件） | 外部通知是 **f09**。本 PR 只保证失败在 Lathe 界面和 API 里可见，不提供事件钩子，也不预留 f09 的接口 |
| 把失败或暂停的规则汇入收件箱 | 收件箱是 **f02**。f02 若要接，可直接从 `schedules.paused_reason` / `schedule_runs.status='failed'` 派生，本 PR 不动 f02 的代码 |
| 团队共享规则、按团队角色授权 | 团队与角色是 **f12**。本 PR 的规则严格按个人 `user_id` 归属 |
| 用量或费用统计、按规则聚合成本 | **f03** |
| PAT 鉴权 | **f05**。本 API 用的是通用 `Auth`，f05 合入后自然支持，本 PR 不做任何处理 |
| 运行回放或事件流呈现 | **f04**。定时建出来的单产生的任务，事件流与普通任务完全一样 |
| 按规则选目标仓库 | 现有 Linear 接单路径固定取属主第一个仓库（`queue.go:642`）。加了字段也没有消费方（铁律 3）。按 team/project 路由是 docs/02 §8 的后续项 |
| 改接单闸门、`Enqueue`、状态机、验证 | 不需要，也不允许 |
| 自动关闭或去重 Linear 上的旧定时单 | 那是改别人的单，人的决策不归调度器管 |
| 修改 `Makefile` 的 `test-race` 包清单 | 公共配置，避免与并行线冲突 |

---

## 5. 验收与测试对照

| 验收点 | 测试 / 断言 |
|---|---|
| 迁移成对可逆（0059/0060，回滚顺序 60 → 59） | 在新建空库上 up → 循环 down 至 59/60 退出 → up 回原样；`internal/store/migrate_test.go` 守 up/down 成对 |
| 规则时间计算与 DST（NY 60m、Lord_Howe 30m、Troll 120m 两个方向） | `internal/schedule` `TestSpecNext` / `TestSpecPrev` / `TestSpecDSTFiresOncePerLocalDay`（全年每个本地日期恰好一次、`Next` 严格递增） |
| 规则校验与内嵌 tzdata | `TestSpecValidate`；`go list -deps ./internal/schedule` 含 `time/tzdata` |
| 模板白名单、按 rune 截断 | `TestRenderTemplate` / `TestValidateTemplate` |
| 到点只建一次（单节点、重启、DST 当天、手动先占时刻仍前移） | `TestTickFiresDueScheduleOnce` / `TestTickRestartDoesNotRefire` / `TestTickAdvancesAcrossDST` / `TestTickAfterManualRunTakesSlotStillAdvances` |
| 多节点只触发一次 | `TestTickConcurrentRunnersFireOnce`（两个 Runner × 8 goroutine） |
| 租约：并发回收一次、进行中/排队中不被接手、从未发出按全新执行、丢租约者写不进、心跳失败先于过期围栏、常量约束 | `TestLease*` 七条 |
| 慢阶段 B 不让别的规则失准（窗口 0 / 6h） | `TestTickSlowStageBDoesNotDelayOtherSchedules`（唯一用 `Run` 驱动的用例） |
| 竞态 | `go test -race -p 1 ./internal/schedule/...` |
| 绝不直接改代码 | `go list -deps ./internal/schedule` 不含 task/runner/creds/preview；`internal/schedule` 与 `internal/store/schedules.go` 无 `INSERT INTO tasks`；`TestTickNeverCreatesTasks` |
| 现有接单路径不变、定时单命中闸门 | `TestIsAssignedToMatchesScheduledCreatePayload`；webhook.go / queue.go / internal/task 零改动 |
| 补偿策略 | `TestTickCatchUp*` / `TestTickOnTimeIgnoresWindow` / `TestTickReenableDoesNotCatchUp` |
| 失败可见、自动暂停、两层脱敏、停用属主不取凭据、回收查单 | `TestTickLinearFailureVisible` / `TestTickAutoPausesAfterConsecutiveFailures` / `TestTickErrorRedactsToken` / `TestTickSkipsDisabledOwner` / `TestTickRecoversStalePending` |
| Linear 客户端（建单、团队、标签、not-found 白名单、截断按 rune） | `internal/integration/linear` `TestCreateIssue` / `TestTeams` / `TestLabelIDByName` / `TestIssueByID` / `TestTruncateKeepsValidUTF8` |
| API 与属主 404、503 在 404 之后、502 不保存 | `internal/httpapi` `TestSchedules*` / `TestLinearTeamsEndpoint` |
| 配置严格解析 | `internal/config` `TestSchedulerConfig` |
| 装配：只由 startWorkers 启动、受开关门控 | `cmd/lathe` `TestServeStartsWorkersAfterEverythingIsWired` / `TestStartWorkersLaunchesAllLoops` / `TestStartWorkersSchedulerGatedByConfig` |
| 端到端：定时单经真 webhook 处理器变成任务，runs API 派生出它 | `cmd/lathe` `TestScheduledIssueFlowsThroughWebhookIntake` |

`ReplayStalePending` 当前取值：**`false`**（R1 未对真 Linear 取证，走至多一次退路）。

---

## 6. 风险与代价

| # | 风险 | 缓解 / 代价 |
|---|---|---|
| R1 | Linear `IssueCreateInput.id` 的行为（是否接受客户端 UUID、重复时报什么错）没对真 API 取证 | 开发期如能拿到**非生产**的 Linear 测试工作区令牌，就跑一次取证并把载荷存档（不得使用 :8200/:8300 的凭据）。拿不到就启用退路：陈旧 pending 不重放，记 `failed/interrupted`，提示「结果未知，请到 Linear 确认」。退路下保证的是**至多一次**，不会重复建单，代价是崩溃窗口（认领到建单之间约 1s）里的那次可能丢失，界面上看得到。**两种模式由 `internal/schedule/runner.go` 的包级常量 `ReplayStalePending` 切换，默认 `false`（退路）**；只有取证文件存在且证明「同 id 重放不会建第二张单」时才改成 `true`（AC-13 的 shell 断言守着这条对应关系）。不做成环境变量：它是对外部 API 行为的事实判断，不是部署偏好（铁律 3）。PR 写明实际取值。无论哪种模式，新建 issue 时 `input.id` 都照传（Linear 公开 schema 的 `IssueCreateInput` 有 `id` 字段）；若取证发现真 API 拒绝客户端 id，就改为不传，同时 `ReplayStalePending` 只能是 `false`，并在 PR 写明 |
| R2 | Linear 是否对 API 创建且指派给本人的 issue 投递 `create` webhook | Linear webhook 不区分变更来源，按现有 `IsAssignedTo` 的 create 分支应当命中。万一不命中，runs 列表的派生 task 为空时 UI 会给出提示，用户可以点「Linear 任务」页手动开跑。这是可见的降级，不会静默失败 |
| R3 | 每次触发都会跑一轮 agent，有费用 | 最小间隔 1h、每人最多 20 条、连续失败自动暂停；分诊仍可能 `blocked_spec` 并回帖提问（正确行为）。不做预算拦截（f03 也不做） |
| R4 | 定时单在 Linear 上堆积（上周的依赖升级 PR 还没合，这周又来一张） | 本 PR 不做去重：「上一张没处理完就不建」是产品决策，而且需要读 Linear 状态。先在文档里写明，留作后续；UI 的 runs 列表能看到派生任务的状态，人自己判断要不要暂停 |
| R5 | 只有三种规则，表达不了「每月 1 号」「工作日每 2 小时」 | 有意取舍（§3.1.1）。如有需求，后续可以加 `monthly`，不影响现有数据（`kind` CHECK 只增不改） |
| R6 | 迁移号与并行线不连续 | 见 §3.4 末段，合入前可改号 |
| R7 | 改到的**既有**文件与边界 | `cmd/lathe/main.go`：`startWorkers` 参数表末尾加 `scheduler *schedule.Runner`、唯一调用点加实参、函数体末尾加受开关控制的 `go scheduler.Run`，`serve()` 在 `linearAPI.Routes(mux)` 之后构造 Runner 并注册 `SchedulesAPI`；`cmd/lathe/assembly_test.go` 只改两条既有结构测试的名单与断言；`internal/config/config.go` 末尾追加两个字段与 `SchedulerEnabled()`；`internal/httpapi/linear.go` 只追加 teams 路由与 `listTeams`；`internal/integration/linear/linear.go` 唯一改动是 `truncate` 函数体（字节 → rune，签名、后缀、调用方上限 500 不变）。其余业务代码全在新文件：`internal/schedule/*`、`cmd/lathe/schedule.go`、`internal/store/schedules.go`、`internal/httpapi/schedules.go`、`internal/integration/linear/schedule.go`、`web/src/views/Schedules.vue` |
| R8 | `time/tzdata` 让二进制增大约 450KB | 换来多节点时区一致。它是标准库，不改 `go.mod`；审阅者如果反对，可以去掉，改为「节点缺 zoneinfo 时该规则记 failed」 |
| R9 | 修 `linear.truncate` 会改变现有错误文案在截断边界处的内容 | 计量改为 rune 后，只影响 >500 rune 的错误体；纯 ASCII 错误体的截断结果与原来逐字节相同；结果保证 `utf8.ValidString`；先红后绿（`red-linear-truncate.txt`） |
| R10 | 调度器 tick 30s，触发精度为 +0~30s | 对「建单」这个动作足够；从建单到 PR 的耗时本来就是分钟级 |
| R11 | 删除规则会级联删掉运行记录（审计痕迹） | Linear 上的单和 Lathe 的任务都保留，审计主线在那两边。想留记录可以只停用不删除，UI 删除确认框里写明 |

---
