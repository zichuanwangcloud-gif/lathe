# Slack / 飞书通知渠道（notify channels）

> 状态：**已实现** · 迁移 0056–0057 · 分支 `feat/notify-channels`
> 前置：[08-debt-cleanup.md §T3](../08-debt-cleanup.md)（终态邮件通知，本功能是它的站外延伸）、
> [02-design.md §3](../02-design.md)（「通知是副作用」的定位）

## 1. 问题与目标

Lathe 的产品价值是「已验证的 PR」，且**人点合并**。人必须出手的节点有两类：任务停下来
等人（`blocked_spec`、`awaiting_approval`、PRD `ready_for_review`），以及活干完了等人
（`pr_open`、`failed`）。邮件只覆盖其中一半，且邮件是「一天看两次」的通道。

本功能让每个用户在个人设置里配置 **Slack incoming webhook** 与/或**飞书自定义机器人**
（含签名校验），把这五类事件推过去：

| 事件 | 落点（文件 · 符号） |
|---|---|
| `failed` | `internal/runner/pipeline.go` · `failWithExcerpt`（所有失败路径的汇流处）；`internal/runner/mergepoll.go` · `failRebaseFollowup`（自动 rebase 撞冲突） |
| `blocked_spec` | `internal/runner/pipeline.go` · 分诊判定不可执行；heavy 档红阶段立不起来（`redStepFailure`） |
| `awaiting_approval` | `internal/runner/pipeline.go` · `gateBeforePush` |
| `pr_open` | `internal/runner/pipeline.go` · `stagePushAndPR`（review 二轮复用同一 PR 时同样发） |
| `prd_ready_for_review` | `internal/prd/service.go` · `SubmitForReview` |

**不做**：`merged` / `cancelled` / `review_feedback` 的渠道推送（没有可行动性，或属 f04）；
入站动作（在 Slack 里点批准）；每日摘要；团队共享渠道（f12）；收件箱聚合（f02）；
钉钉/企业微信。

## 2. 核心不变量：通知永不参与判定

- 每个落点都在**状态转移落库之后**追加一行 `chatTerminal` / `notifyReview`，既有邮件调用一行不动。
- `Pipeline.chatTerminal` 与 `mailTerminal` 同形：**不返回 error、不阻塞（goroutine）、独立 3s 预算、
  panic 兜底**。它只往 outbox 插一行；notifier 永远报错 / 卡死 / panic 时任务照样干净落在目标状态
  （`TestPipelineFailureStillFailsWithBrokenChatNotifier`、`TestChatTerminalIsNonBlocking`）。
- **快照在起 goroutine 之前取好**：`notify.TaskEvent` 带齐 payload 需要的全部字段，实现方不回读 `tasks`
  （`TestChatTerminalSnapshotsBeforeGoroutine`、`TestWorkerRendersFromPayloadNotTaskRow`）。
- PRD 路径同步调用（不在串行热路径上），同样 3s 上界、error 只记日志、panic 兜底。

## 3. 数据模型

**`notify_channels`（0056）**：每用户每种渠道一行，`(user_id, kind)` 唯一，`kind ∈ {slack, feishu}`。
`url_enc` / `sign_enc` 是 AES-256-GCM 密文（**没有明文列**）；`enabled` 总开关 + 五个 `event_*` 开关
（默认全开）；`test_ok/test_error/test_at` 是最近一次**完成了的**测试消息结果（投递行的派生缓存）。

**`notify_deliveries`（0057）**：outbox。`(channel_id, event_key)` 唯一索引是**唯一的去重真相源**：

| 事件 | `event_key` |
|---|---|
| 任务 | `task:<task_id>:<to_state>:<task.updated_at RFC3339Nano>` |
| PRD | `prd:<prd_id>:ready_for_review:<prd.updated_at RFC3339Nano>` |
| 测试 | `test:<随机 UUID>` |

用「转移那一刻的 `updated_at`」而不是当前时间：同一转移重放得到同一个键。键与 `next_attempt_at`
都经 `notify.EventTime`（UTC、截到微秒，与 pgx 写 `timestamptz` 的截断口径一致），二者不分叉。

`payload` 是 worker 渲染的**全部**输入（`notify.Payload` 的 9 个键：`task_id`、`external_key`、
`to_state`、`failure_stage`、`reason`、`pr_url`、`prd_id`、`prd_repo`、`test_nonce`）。结构体里
没有 worktree 路径、分支名、会话 ID —— 正文的全部来源就是 payload，所以它们不可能外发
（`TestRenderNeverLeaksInternalPaths` 用反射钉住键集合）。

**回滚**：两条 down 都是 `DROP TABLE`。代价是投递历史没了，且用户需要重新配置渠道。

**迁移编号**：本线分配号段 0056–0058，`origin/dev` 最大是 0031；只用了 0056、0057，0058 留作
集成缓冲。号段空档不参与任何判定（`loadMigrations` 按版本排序，`MigrateDown` 找已应用的最高版本），
是否重排由合并时的人决定。

## 4. 包结构与依赖方向（不引入依赖）

```
internal/notify   ── 只 import 标准库（TestNotifyImportsStdlibOnly）
      ▲   ▲   ▲
      │   │   └── internal/prd      （notify.PRDEvent、常量）
      │   └────── internal/runner   （notify.TaskEvent、常量、notify.StateLabel）
internal/store ──► notify    （实现 notify.Outbox；用 ValidateURL / MaskURL / 拼键与 payload 的纯函数）
internal/httpapi ──► store, notify
cmd/lathe        ──► 全部（只装配）
```

| 位置 | 职责 |
|---|---|
| `internal/notify/notify.go` | 常量（`Kind*`、`Event*`、`Status*`、时间常量）、`TaskEvent`/`PRDEvent`/`Payload`/`Delivery`/`Channel`/`EventSwitches`、`Outbox` 接口、拼键/payload/退避纯函数 |
| `internal/notify/render.go` | `Render`（纯函数，原因按 rune 截 800、正文 ≤ 8 KiB）；**状态/事件中文标签的唯一定义** `StateLabel` / `EventLabel` |
| `internal/notify/sender.go` | `Sender`：host 白名单、Slack/飞书 payload、飞书签名、成败判定、`transportErrClass`、`scrubURL` |
| `internal/notify/outbox.go` | `Worker`：`DrainOnce` 逐行「领 → 发 → 记结果」、`Run` 每 15s 一轮 |
| `internal/store/notify_channels.go` | `NotifyChannels`：渠道 CRUD（部分更新）、入队、测试消息生命周期、`notify.Outbox` 实现 |
| `internal/runner/notify.go` | `ChatNotifier` 窄接口、`chatTerminal`；`stateSubject` 改调 `notify.StateLabel`（邮件输出逐字不变） |
| `internal/prd/service.go` | `ReviewNotifier` 窄接口、`notifyReview` |
| `internal/httpapi/notify.go` | `NotifyAPI` |
| `cmd/lathe/main.go` | `NewNotifyChannels` 一处（白名单 `notify.AllowedHosts`）；`buildPipeline` 的 `Chat`、`buildPRDService` 的 `ReviewNotifier` 接同一个 NC；`startWorkers` 起 `notifyWorker` |

`*store.NotifyChannels` 的方法签名与 `runner.ChatNotifier` / `prd.ReviewNotifier` 一对一同形，
装配直接赋值、不写适配器；三处编译期断言守签名漂移。

## 5. 发送、成败判定与重试

| 渠道 | 请求体 | 签名 |
|---|---|---|
| Slack | `{"text":"…"}` | 无（URL 即授权） |
| 飞书 | `{"msg_type":"text","content":{"text":"…"}}` | 有密钥时加 `timestamp` + `sign = base64(HMAC-SHA256(key=timestamp+"\n"+secret, data=""))` |

**Slack 不回 JSON**：只看状态码与 `TrimSpace(body)=="ok"`，绝不 `Unmarshal`。`429`/`5xx` 可重试，
其余 4xx / 3xx / 200 非 ok 不可重试。**飞书**以 JSON 的 `code` 为准（缺失时看旧的 `StatusCode`）：
`0` 成功；`11232`（限流）可重试；`9499`、`19021`、`19022`、`19024` 与未知码不可重试。
网络错误、超时、`5xx`、`429` 可重试。

**错误串（`last_error` / `test_error`）永不含 URL**：无 HTTP 响应的路径只写固定类别
（`timeout` / `connection refused` / `connection reset` / `dns error` / `tls error` / `network error`），
绝不 `err.Error()` —— Go 的 `*url.Error` 自带完整 URL，内层 `*net.OpError` 带 `host:port`。
有响应时写「状态码 + 平台错误串 / code+msg，前 200 rune」。最后统一过 `scrubURL`。

**退避**（`notify.NextAttempt`，`MarkRetry` 与 `ReclaimStale` 共用）：`attempts` 是已失败次数；
第 n 次失败后等 `min(30s·2^(n-1), 15m)`，第 5 次失败即 `gave_up`。即最多发送 5 次、4 个间隔
（30s/60s/120s/240s）。不可重试失败一次即 `gave_up`。`gave_up` 后要人点「重试」（attempts 归零）。

**多节点**：`ClaimDueDelivery` 一次只领一行（`FOR UPDATE SKIP LOCKED`），领到即发；
所有写回带 CAS（`status='sending' AND claimed_at=<领取值>`）。租约 `ClaimLease=5m ≥ 2×SendTimeout+1m`，
只需覆盖一次发送。死掉的 `sending` 每轮回收：非 test 行走同一套退避，test 行直接 `gave_up`。
**store 与 worker 的时间全部由参数传入，SQL 里不出现 `now()`**。

**发送前复查**：渠道被关掉或事件开关被关掉 → `skipped`（保留记录，解释「为什么没收到」）；
渠道已删 → 行随 CASCADE 消失；密文解不开（主密钥换了）→ `gave_up`（`channel unavailable`）。

## 6. 安全

- **SSRF 收口**：`notify.ValidateURL` 是唯一校验函数，保存与发送都调它：必须 `https`、host 精确命中
  白名单 `hooks.slack.com` / `open.feishu.cn` / `open.larksuite.com`（含端口比较，带端口即拒）、不带 userinfo；
  客户端不跟随重定向。白名单是**代码常量，不是环境变量**：放开它等于给用户一个「让服务器替我 POST
  内网地址」的原语。
- **界面永不显示明文**：URL 只回 `https://<host>/…`（不复用保留末 4 位的 `secret.Mask` —— 末尾正是 token）；
  签名密钥只回 `hasSecret`。
- **日志永不出现 URL**：日志只打 delivery id / kind / event / outcome 与固定类别的原因。
- **正文不含凭据**：失败原因在 runner 侧经 `redactText(…, KnownSecrets)` 脱敏、按 2400 字节（含
  `…(已截断)` 后缀）截断后才进 payload；worker 只读 payload。
- **属主隔离**：渠道按 `(user_id, kind)`；投递按 `WHERE d.id=$1 AND c.user_id=$2`，零行即 **404**。

## 7. API

全部挂在当前登录用户上，未认证 401。

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/api/notify/channels` | `{channels:[{kind,configured,maskedURL,hasSecret,enabled,events,testOk,testError,testAt}], eventLabels:{failed,blockedSpec,awaitingApproval,prOpen,prdReady}}`；两种 kind 都返回 |
| `PUT` | `/api/notify/channels/{kind}` | `{url, secret, clearSecret, enabled, events:{…五键}}`，部分更新（见下）；回 `{saved:true, test:{ok,error,deliveryId}|null}` |
| `POST` | `/api/notify/channels/{kind}/test` | 用已存凭据重发测试消息（新 UUID、新一行） |
| `DELETE` | `/api/notify/channels/{kind}` | 删渠道（投递行随 CASCADE） |
| `GET` | `/api/notify/deliveries?limit=50` | `{deliveries:[{id,kind,eventType,status,attempts,lastError,sentAt,createdAt}]}`，不含 URL 与正文 |
| `POST` | `/api/notify/deliveries/{id}/retry` | 非 test 的 `gave_up` → `pending`；非属主/不存在 404；test 行或非 `gave_up` 409 |

`{kind}` 非法 → 400（渠道种类不是资源存在性问题）。

**PUT 的部分更新语义**：`url`/`secret` 留空即**保留原密文、不重新加密**；首次配置不给 `url` → 400；
`clearSecret:true` 才清签名（绝不因为留空而清掉签名）；`secret` 与 `clearSecret` 同时给 → 400；
Slack 给 `secret`/`clearSecret` → 400；`enabled` 与 `events` 五键**必填**（缺一个 400 并点名，
否则少传一个键就会静默关掉一类通知）。**只有凭据变了才发测试消息**（首次配置、`url`/`secret`
非空或 `clearSecret`），并把 `test_*` 置 NULL；只改开关不发、不写投递行、不刷群。

**测试消息的生命周期**：落一行投递（审计面统一），但**只由请求内同步发送一次**，绝不交给 worker：
`BeginTest` 直接写成 `sending`（不经 `pending`，worker 从结构上领不到）→ `Sender.Send` →
`FinishTest` 一个事务里写投递行（成功 `sent`/attempts=0，失败 `gave_up`/attempts=1）与渠道 `test_*`。
保存时测试失败也保存（同 SMTP 的取舍）。

## 8. 配置项

**不新增任何环境变量**，README 变量表不动：渠道开关与凭据是每用户设置；重试上限/间隔/白名单是行为
常量（`notify.MaxAttempts`、`BaseBackoff`、`MaxBackoff`、`DrainInterval`、`SendTimeout`、`ClaimLease`、
`DrainMaxPerRound`、`AllowedHosts`）；「通知总开关」已由每渠道的 `enabled` 覆盖，再加一个全局开关
就是第二个真相源。

## 9. 界面

个人设置页「通知邮箱」卡片之后两张渠道卡片（`web/src/components/NotifyChannels.vue`）：状态徽标
（未配置 / 已配置 / 测试失败）、URL 与签名密钥输入（`type=password`，留空则保持不变）、「清除签名密钥」
（仅已设签名时显示）、总开关 + 五个事件勾选框（文案取 `eventLabels`，前端不硬编码中文标签）、
「保存」/「保存并发送测试消息」（凭据框有输入时才是后者）、「重发测试」、删除；下方「最近投递」
折叠列表（最近 10 条），`gave_up` 的非测试行带「重试」。

## 10. 后续项

- **投递记录保留期**：`notify_deliveries` 目前不清理（每小时几行量级，先观察）。清理是删数据，
  需要人批准后另开单（AGENTS.md §4）。
- **钉钉 / 企业微信**：同一套 `Sender` 加 kind 与白名单 host 即可，但扩白名单是改产品行为，要在 PR 里说明理由。
- **团队共享渠道**：属 f12（团队与角色）。本功能所有查询都带 `user_id`，f12 引入团队后再决定是否加
  `team_id`，并连带解决「谁能看谁的密钥」。
- **入站动作**（在 Slack/飞书里点批准、回答提问）：**明确不做**。入站要处理验签、幂等、权限映射，
  等于把交互出口开到第三方面前；产品边界「人点合并」也要求动作发生在 Lathe 里。
