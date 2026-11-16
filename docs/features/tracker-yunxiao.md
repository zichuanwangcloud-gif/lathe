# f08 云效任务来源（Tracker：阿里云云效 Projex 工作项）

> 状态：**已实现**（PRD 经两轮评审定稿，B1–B5 的答复见 §8；实现与验收结果见 §9）。
> 迁移 0053–0055。本文由 PRD 定稿版原样落入，去掉了开发线专属的环境信息（worktree、端口、
> 测试库名），其余未改；§5 里的 `ac_run` / 证据路径是验收时用的判定器与证据目录，保留以便复核。

---

## 1. 问题与目标

### 1.1 为什么 Lathe 需要它

Lathe 的定位是「需求单 → **已验证的 PR**」。今天需求单只有两个来源：Linear 与内置工单
（`internal/tracker/tracker.go:21-34`）。国内团队大量用阿里云云效（Projex）管需求与缺陷，
他们要用 Lathe 只能在两套系统间手工搬单——搬单的那一刻，「单子不明确就回帖提问并停下」
这条产品边界就断了：问题问在内置工单里，提单人在云效里看不到。

设计文档早就预见到这一步，并把它列为验证抽象形状的**前置行动项**：

- `docs/06-orchestration.md:173-204`：「接口不能按 Linear 的形状定（云效接进来全是漏洞）…
  做一个云效只读探针」；
- `docs/07-prd-orchestration.md:298-304`：F6.4「第二个平台（云效）」AC1–AC3；
- `internal/tracker/tracker.go:8-10` 包注释：「等第二个外部平台（云效）探针落地时再谈」。

### 1.2 目标

1. **云效工作项可以像 Linear issue 一样驱动整条管线**：接单（指派给我 / 打触发标签）→
   分诊（读正文 + 评论）→ 不明确则在云效回帖提问并 `blocked_spec` → 实现 → 红绿验证 →
   开 PR → 在云效回帖 PR 链接。
2. **状态同步（可选、尽力而为）**：开跑 / PR 已开 / 已合并三个时刻把工作项推到用户配置的
   状态；云效侧把工作项改到用户配置的「取消」状态时，联动取消在途任务（与 Linear 的 T7
   取消联动同语义：只改 DB 状态）。
3. **webhook 接入与验签**：`POST /webhooks/yunxiao/{slug}`，按用户路由、按用户密钥校验。
4. **凭据加密**：云效令牌与 webhook 密钥 AES-256-GCM 入库，界面只显示掩码。
5. **Linear 与内置工单行为逐字节不变**。

### 1.3 与「已验证 PR」定位的关系

本功能**只扩入口，不碰验证**：云效任务与 Linear 任务进的是同一个 `Pipeline.Execute`，
同一个 `stageVerify`、同一张 `verifications` 表、同一条红绿证明。tracker 窄接口的设计初衷
（`docs/09-internal-issues.md:38-55`）就是让平台差异止步于 `Issue()` / `Comment()` 两个方法，
本功能是对这个抽象的第二次实证——如果它需要动验证或状态机才能接进来，说明抽象错了。

---

## 2. 现状取证

### 2.1 tracker 抽象与分派（直接复用）

| 事实 | 位置 |
|---|---|
| 平台常量与 `ValidProvider`（只有 linear / internal）；注释要求与 `tasks.tracker_provider` CHECK「两边一起改」 | `internal/tracker/tracker.go:19-34` |
| `Tracker` 窄接口：`Issue(ctx, ref)` / `Comment(ctx, ref, body)` | `internal/tracker/tracker.go:40-46` |
| 可选扩展接口的先例：`AttributedTracker`，pipeline 用类型断言可选消费 | `internal/tracker/tracker.go:48-57`；消费点 `internal/runner/pipeline.go:433-435` |
| 分诊上下文拼装全平台唯一一份：`Issue.Context()`（链接为空时不输出链接行） | `internal/tracker/tracker.go:87-113` |
| 按 provider 分派 tracker；未知 provider 报错不默认走 Linear；`""` 按 linear | `internal/creds/verifier.go:110-125` |
| pipeline 只认 `Clients.Tracker(provider)`；setup 按任务行钉死的 provider 取 tracker | `internal/runner/pipeline.go:39-42`、`pipeline.go:420-432` |
| `IssueRef` = external_id 优先、退回 external_key | `internal/runner/pipeline.go:176-179`、`pipeline.go:250-255` |
| 失败传播回帖按任务各自 provider 现取 tracker（`commentOnTask`） | `internal/runner/pipeline.go:257-270` |

### 2.2 tracker 的 5 个消费点（云效全部自动获得，无需改 pipeline）

| 消费点 | 位置 |
|---|---|
| 分诊拉单 `Issue()`，失败走 `p.fail(StageFetchIssue)` | `internal/runner/pipeline.go:531-534` |
| 单子不明确回帖提问 → `blocked_spec` | `internal/runner/pipeline.go:571-582` |
| 人工闸门回帖 | `internal/runner/pipeline.go:378-385` |
| heavy 档红阶段立不起来回帖 → `blocked_spec` | `internal/runner/pipeline.go:1054-1067` |
| 开出 PR 回帖 PR 链接 | `internal/runner/pipeline.go:1158-1178` |
| 失败回帖 | `internal/runner/pipeline.go:1475-1483` |

### 2.3 Linear 接入的形态（照抄结构，不改其代码）

| 事实 | 位置 |
|---|---|
| Linear webhook：路由 → 验签 → 幂等登记 → 取消联动 → 接单闸门 → Enqueue | `internal/httpapi/webhook.go:87-181` |
| 投递目标（属主 / 密钥 / 接单用户 ID / 触发标签） | `internal/httpapi/webhook.go:47-61` |
| 未知 slug 与验签失败分别 404 / 401，不透露原因 | `internal/httpapi/webhook.go:93-115` |
| 接单判定只在「这次改了指派/标签」时触发（靠 `updatedFrom`），否则任何编辑都会重复接单 | `internal/integration/linear/linear.go:410-466` |
| 标签判定：名字、大小写不敏感、trim 后为空恒不匹配 | `internal/integration/linear/linear.go:469-487` |
| 取消判定按状态**类型**不按名字 | `internal/integration/linear/linear.go:501-521` |
| Linear 的 `truncate` 按 byte 切（`s[:n]`）——多字节会切坏；本功能新代码不得照抄 | `internal/integration/linear/linear.go:523-529`（AGENTS.md §9 UTF-8 截断事故） |
| resolver：slug → 用户，停用 404，密钥/接单 ID/标签现取 | `cmd/lathe/main.go:602-636` |
| 路由注册：`/webhooks/linear/{slug}`、`/webhooks/github/{slug}` | `cmd/lathe/main.go:260-278` |
| GitHub webhook 的去重键加平台前缀（共表防撞） | `internal/httpapi/github_webhook.go:49-51, 106-111` |
| 去重表 `webhook_deliveries(delivery_id PK, source text)`，source 无 CHECK | `migrations/0001_init.up.sql:179-185`、`internal/store/webhook.go:13-29` |
| 标签接单的标签名是**系统级**设置（管理员配），默认空 = 关闭 | `internal/store/settings.go:26-35`、`internal/httpapi/admin.go:84-91` |

### 2.4 建任务与取消联动

| 事实 | 位置 |
|---|---|
| Linear 入队：`resolveRepo`（属主第一个仓库）→ `tasks.Create(provider=linear)`；建不出时回帖兜底 | `cmd/lathe/queue.go:91-111`、`queue.go:642-650` |
| 内置工单入队另走 `EnqueueInternal`（不改 `TaskEnqueuer` 的 Linear 签名） | `cmd/lathe/queue.go:120-156` |
| 取消联动共用 `cancelActiveTasks`（转 cancelled + 失败传播），**只改 DB 不停 agent** | `cmd/lathe/queue.go:173-203, 227-279` |
| `ActiveByKey(userID, provider, key)` 已存在，可直接用 | `internal/task/machine.go:585` |
| `ActiveByIssueID` 只按 external_id 过滤、不带 provider | `internal/task/machine.go:557-577` |
| `tasks.Create` 空 provider 默认 linear | `internal/task/machine.go:181-185` |
| `tasks.tracker_provider` CHECK 只允许 linear / internal；活任务唯一索引 `(repo_id, tracker_provider, external_key)` | `migrations/0021_internal_issues.up.sql:69-79` |
| `blocked_spec` 只能 → queued / cancelled（本功能不碰） | `internal/task/state.go:94` |
| 内置工单的生命周期联动 `ApplyTaskOutcome`（started / merged / task_cancelled），只给 internal 用 | `internal/store/issues.go:227-260`、`internal/runner/mergepoll.go:88-92, 682-689` |
| merged 检测：`handleMerged` | `internal/runner/mergepoll.go:252-277` |
| 编排图建节点硬编码 Linear | `internal/flow/service.go:241-245` |

### 2.5 凭据

| 事实 | 位置 |
|---|---|
| `integrations` 一行一个 `(user_id, kind)` 加密串；`kind` 有 CHECK，每加一种就要整句重建 CHECK | `migrations/0003_integration_secrets.up.sql:21-30`、`0028_github_webhook_secret.up.sql:11-21` |
| `KnownKinds` 与 CHECK 双写 | `internal/store/secrets.go:13-34` |
| 加密写入 / 解密读取 / 掩码展示 | `internal/store/secrets.go:68-109, 172-226` |
| 凭据 API：`PUT /api/integrations/{kind}` 只收一个 `token` 字段 | `internal/httpapi/credentials.go:51-114` |
| 按用户凭据工厂；环境变量兜底只给内置管理员 | `internal/creds/factory.go:11-61`、`internal/creds/creds.go:35-41, 79-119` |
| agent 子进程环境白名单化（集成 token 不进 agent） | `internal/integration/agent/agent.go:230, 467-477` |
| 设置页凭据卡片元数据与 webhook 地址拼法 | `web/src/views/PersonalSettings.vue:9-17, 36-61` |

### 2.6 云效官方 OpenAPI（2025-11 取证，均为 `x-yunxiao-token` 个人访问令牌鉴权）

接入点：**中心版**统一 `https://openapi-rdc.aliyuncs.com`，路径带 `organizations/{organizationId}`；
**Region 版**用实例域名，路径不带组织段（[服务接入点](https://help.aliyun.com/zh/yunxiao/developer-reference/service-access-point-domain)）。

| 能力 | 方法 + 路径（中心版；Region 版去掉 `organizations/{organizationId}`） | 响应形状要点 | 来源 |
|---|---|---|---|
| 取工作项 | `GET /oapi/v1/projex/organizations/{org}/workitems/{id}` | 裸对象：`id`、`serialNumber`、`subject`、`description`、`formatType`(RICHTEXT/MARKDOWN)、`status{id,name,displayName,nameEn}`、`assignedTo{id,name}`、`labels[{id,name,color}]`、`logicalStatus`(normal/archived)、`gmtModified`… | [GetWorkitem](https://www.alibabacloud.com/help/zh/yunxiao/developer-reference/getworkitem) |
| 评论列表 | `GET …/workitems/{id}/comments` | 裸数组：`id`、`content`、`contentFormat`、`user{id,name}`、`gmtCreate`；**无分页** | [ListWorkitemComments](https://help.aliyun.com/zh/yunxiao/developer-reference/listworkitemcomments) |
| 发评论 | `POST …/workitems/{id}/comments`，body `{"content","formatType"}`（默认 RICHTEXT） | `{"id"}` | [CreateWorkitemComment](https://help.aliyun.com/zh/yunxiao/developer-reference/createworkitemcomment) |
| 改状态 | `PUT /oapi/v1/projex/workitems/{id}`，body `{"status":"<statusId>"}` | 无返回体 | [UpdateWorkitem](https://www.alibabacloud.com/help/zh/yunxiao/developer-reference/updateworkitem.md)（文档只给出 Region 形态路径，见 §6 R2） |
| 工作流（状态名 → id） | `GET …/workitems/{id}/workflow` | `{id,name,defaultStatusId,statuses[{id,name,displayName,nameEn}]}` | [GetWorkitemWorkflowInfo](https://help.aliyun.com/zh/yunxiao/developer-reference/getworkitemworkflowinfo-get-work-item-workflow-information) |
| 当前用户（验令牌、取接单 ID） | `GET /oapi/v1/platform/user` | `{id,name,nickName,email,username,…}` | [GetUserByToken](https://help.aliyun.com/zh/yunxiao/developer-reference/getuserbytoken) |
| 出站 webhook | 项目「自动化规则」→ Webhook 模板，触发「字段变更」/「状态变更」；Body 选【工作项数据】即 GetWorkitem 同形对象；配置 Secret 后请求带 **`X-Projex-Signature: <secret 原值>`**（**不是 HMAC**） | — | [Webhook 配置自动化规则](https://help.aliyun.com/zh/yunxiao/user-guide/webhook-configuration-guide) |

**探针结论（回答 docs/06 §3.4 的预判轴）**：
- 状态由项目模板定义 → 状态**不能硬编码**，按用户配置的**状态名**经 workflow 接口现解析成 id；
- 评论有 RICHTEXT(HTML) 与 MARKDOWN 两种格式 → 读侧要转纯文本，写侧固定发 MARKDOWN；
- webhook 无 `updatedFrom`、无投递 ID、签名是明文共享密钥 → 不能照搬 Linear 的「这次改了什么」
  判定，改为**状态快照 + 边沿检测**（§3.6），且载荷除 `id` 外一律不信，回源 API 拉权威数据；
- 无依赖关系表达 → 与 docs/06 §3.3「编排不依赖可选能力」一致，不影响任何编排功能。

---

## 3. 方案

### 3.0 总览

```
云效自动化规则 ──POST /webhooks/yunxiao/{slug}──► YunxiaoWebhook
   X-Projex-Signature                              │ 1 slug→用户（停用/未配置 404）
                                                   │ 2 常数时间比对密钥（401）
                                                   │ 3 载荷只取 id（400）
                                                   │ 4 用属主令牌回源 GetWorkitem（失败 503，让对端重投）
                                                   │ 5 去重登记 yunxiao:<uid>:<id>:<gmtModified>
                                                   │ 6 取消判定（状态名==配置的取消状态）→ CancelForYunxiao
                                                   │ 7 快照边沿检测（指派/标签 false→true）→ EnqueueYunxiao
                                                   ▼
tasks(provider='yunxiao', external_id=工作项 id, external_key=serialNumber)
   │  ClaimReady → Pipeline.Execute（零改动的主路径）
   ▼
creds.Clients.Tracker("yunxiao") → yunxiao.Tracker{Issue, Comment, SyncStatus}
```

新增代码全部落在新文件里；对既有文件的改动只是「加一个 case / 加一个可选调用 / 加一条路由」。

### 3.1 数据模型（迁移 0053–0055，一次迁移只做一件事）

**0053_tracker_provider_yunxiao** —— 放开 `tasks.tracker_provider` 取值：

```sql
ALTER TABLE tasks DROP CONSTRAINT tasks_tracker_provider_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_tracker_provider_check
  CHECK (tracker_provider IN ('linear', 'internal', 'yunxiao'));
```
down：重建为 `('linear','internal')`。**不删数据**：若库里已有 yunxiao 任务，重建 CHECK 会报
约束冲突、回滚失败——「宁可报错让人看见，也不静默丢数据」（同 `0021_internal_issues.down.sql:9-12`
的立场，但比它更保守：不 DELETE）。存在 yunxiao 任务时须先人工处置再回滚。

**0054_yunxiao_connections** —— 每用户一条云效连接：

```sql
CREATE TABLE yunxiao_connections (
  user_id            bigint PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  endpoint           text   NOT NULL DEFAULT 'https://openapi-rdc.aliyuncs.com',
  organization_id    text,                 -- NULL/空 = Region 版（路径不带组织段）
  token_enc          bytea  NOT NULL,      -- 个人访问令牌，AES-256-GCM（nonce 前置）
  webhook_secret_enc bytea,                -- X-Projex-Signature 共享密钥；NULL = webhook 不可用
  account_id         text,                 -- GetUserByToken.id：「指派给我」判定用
  account_name       text,
  trigger_label      text   NOT NULL DEFAULT '',  -- 空 = 标签接单关闭
  status_started     text   NOT NULL DEFAULT '',  -- 状态名；空 = 该时刻不同步
  status_pr_open     text   NOT NULL DEFAULT '',
  status_merged      text   NOT NULL DEFAULT '',
  status_cancel      text   NOT NULL DEFAULT '',  -- 云效改到此状态 → 取消在途任务；空 = 关闭
  verified_at        timestamptz,
  verify_error       text,
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER yunxiao_connections_updated_at BEFORE UPDATE ON yunxiao_connections
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();
```
down：`DROP TABLE yunxiao_connections;`

**为什么不复用 `integrations` 表**（取舍写清）：
1. 云效连接除了两个密钥还有 7 个非密字段（接入点、组织、标签、4 个状态名），`integrations`
   一行一个 token 的形态与 `PUT /api/integrations/{kind}` 只收 `token` 的 API 都装不下；硬塞进
   `scopes text[]` 是把结构化配置藏进数组，没有类型、没有校验点。
2. **并行合并竞态**：f07（GitLab/Gitea）与 f09（Slack/飞书）大概率都要扩 `integrations_kind_check`，
   而 CHECK 只能整句重建——三条线各自 DROP/ADD，后合入的会**静默抹掉**先合入的取值，运行期才
   在 `Save` 时报 23514。独立表让本线与它们零交集。
3. 代价：凭据存储有了第二个位置（加密口径完全复用同一个 `secret.Sealer`，不另写加解密）；
   界面上云效不在通用凭据列表里，而是单独一张卡片（§3.3）。

**0055_yunxiao_trigger_state** —— 接单边沿检测的快照：

```sql
CREATE TABLE yunxiao_trigger_state (
  user_id     bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  workitem_id text        NOT NULL,
  assigned    boolean     NOT NULL DEFAULT false,   -- 默认值即「首见占位行」的语义（§3.6）
  labeled     boolean     NOT NULL DEFAULT false,
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, workitem_id)
);
```
down：`DROP TABLE yunxiao_trigger_state;`。表小（每个被投递过的工作项一行），无大表索引问题，
不需要 `CONCURRENTLY`。

`webhook_deliveries` 不改：`source` 无 CHECK，直接写 `'yunxiao'`。

**接入点白名单不需要迁移**：管理员登记的 Region 版实例域名存进既有的 `system_settings`
（键 `yunxiao_allowed_hosts`，§3.7），与 `webhook_trigger_label` 同一种存法。

### 3.2 后端代码布局

| 新文件 / 改动 | 内容 |
|---|---|
| `internal/integration/yunxiao/client.go`（新） | `Config{Endpoint, OrganizationID, Token}`、`NewClient(cfg)`（校验见 §3.5）、`NewClientWithHTTP` 供测试注入；方法 `CurrentUser`、`GetWorkitem`、`ListComments`、`CreateComment`、`Workflow`、`UpdateStatus`；`path(…)` 按是否有组织 ID 拼中心版 / Region 版路径，路径段一律 `url.PathEscape`；HTTP 超时 30s、响应读上限 8MB；传输层是 §3.7 的**出站守卫**（host 允许列表 + 拨号期 IP 校验 + 不跟随重定向 + 不走环境代理）；错误分类（401 令牌无效 / 403 缺「项目协作」权限 / 404 不存在或无权 / 429 限流 / 5xx / 其他非 2xx / 2xx 但形状不符），错误串 = 分类人话 + HTTP 状态码，**不含响应体的任何字节**，**永不包含令牌** |
| `internal/integration/yunxiao/guard.go`（新） | 出站守卫（§3.7），只用 stdlib：`ValidateEndpoint(raw, allowedHosts)`（保存时与 `NewClient` 共用）、`guardedTransport()`（`net.Dialer.Control` 校验**实际拨号的** IP；`Proxy: nil`；`CheckRedirect` 一律拒绝）、`isForbiddenIP(netip.Addr)` |
| `internal/integration/yunxiao/richtext.go`（新） | `ToPlainText(content, format)`：MARKDOWN 原样；RICHTEXT 用 stdlib 自写的标签剥离（`<br>`/`</p>`/`</li>`/`</div>` → 换行，`<script>`/`<style>` 连内容丢弃）+ `html.UnescapeString`；不引入 `golang.org/x/net/html` |
| `internal/integration/yunxiao/webhook.go`（新） | `HeaderSignature = "X-Projex-Signature"`；`VerifySecret(secret, got)` 用 `hmac.Equal` 常数时间比较，空密钥 / 缺头 → `ErrBadSignature`；`ParseWorkitemID(body)` 只解析 `id`，校验非空、≤64、字符集 `[A-Za-z0-9_-]` |
| `internal/integration/yunxiao/tracker.go`（新） | `Tracker` 实现 `tracker.Tracker` + `tracker.StatusSyncer`：`Issue(ref)` = GetWorkitem + ListComments → `tracker.Issue{ID:id, Identifier:serialNumber(空则 id), Title:subject, Description:纯文本, StateName:status.displayName, Labels, AssigneeID, Comments}`（URL 留空：OpenAPI 不返回网页链接，不猜拼法）；`Comment(ref, body)` 以 `formatType=MARKDOWN` 发评论；`SyncStatus(ref, ev)` 见 §3.4 |
| `internal/tracker/tracker.go`（改） | 加 `ProviderYunxiao = "yunxiao"`，`ValidProvider` 认它；加**可选**扩展接口 `StatusSyncer{ SyncStatus(ctx, ref string, ev Lifecycle) error }` 与 `Lifecycle` 三个常量（started / pr_open / merged）。**`Tracker` 接口本身一个方法都不加**——Linear 与内置实现零改动 |
| `internal/store/yunxiao.go`（新） | `YunxiaoConnections`（持 `secret.Sealer`）：`Get`（解密）、`Save`（空 token/secret 表示「保留原值」；token 换了新值时同句把 `account_id/account_name/verified_at` 置 NULL）、`Delete`（**一个事务**里删 `yunxiao_connections` 行与该用户全部 `yunxiao_trigger_state` 行）、`Status`（只出掩码，用 `secret.Mask`）、`MarkVerified`/`MarkVerifyFailed`；`ObserveTrigger(ctx, userID, workitemID, assigned, labeled) (rose bool, err)`（§3.6）；`AllowedHosts` / `SetAllowedHosts`（系统设置键 `yunxiao_allowed_hosts`，§3.7） |
| `internal/creds/factory.go`、`verifier.go`（改） | `Factory` 增 `WithYunxiao(*store.YunxiaoConnections)`（不改 `NewFactory` 签名，免得动所有调用方与测试）；`Clients.Tracker` 增 `case tracker.ProviderYunxiao`：读属主连接、构造 `yunxiao.Tracker`；未配置 → `"creds: 云效连接未配置"`。`linear` / `internal` / `""` / 未知分支一行不动 |
| `internal/runner/statussync.go`（新） + 3 处调用（改） | `syncTrackerStatus(ctx, tr, tk, ev)`：`tr` 不实现 `StatusSyncer` 直接返回；实现则调用，**错误只 `slog.Warn`，绝不返回、绝不影响状态机**。调用点：`stageTriage` 转 triaging 成功之后（started）、`stagePushAndPR` 转 pr_open 并回帖之后（pr_open）、`MergePoller.handleMerged` 转 merged 之后（merged，经 `ClientFactory.ForUser → Tracker(tk.TrackerProvider)` 现取） |
| `internal/httpapi/yunxiao.go`（新） | `YunxiaoAPI`（§3.3 的 5 个成员接口 + 2 个管理员接口）与 `YunxiaoWebhook`；依赖的入队面定义为**新接口** `YunxiaoEnqueuer{EnqueueYunxiao, CancelForYunxiao}`——不给 `TaskEnqueuer` 加方法（它的 fake 散在 Linear 测试里，加方法就要改 Linear 测试文件）；哨兵错误 `ErrYunxiaoTaskActive`（入队撞活任务，`/run` 据此回 409） |
| `cmd/lathe/queue.go`（改，只追加） | `EnqueueYunxiao(owner, workitemID, serial)`：`resolveRepo` → `tasks.Create(provider=yunxiao, ExternalID=id, ExternalKey=serial)`；`Create` 返回 SQLSTATE 23505 时包成 `httpapi.ErrYunxiaoTaskActive`（判定只在本函数里，不改 `internal/task`）；`resolveRepo` 失败时 `commentUnresolved(…, tracker.ProviderYunxiao, …)` 回帖兜底（与 Linear 同口径）；`CancelForYunxiao(owner, key)`：`ActiveByKey(owner, "yunxiao", key)` → 复用 `cancelActiveTasks`，actor `system:webhook` |
| `cmd/lathe/main.go`（改，只追加） | 构造 `YunxiaoConnections`、`factory.WithYunxiao`、注册 `POST /webhooks/yunxiao/{slug}` 与 `YunxiaoAPI.Routes`、`yunxiaoWebhookResolver`（slug → 用户，停用 / 无连接 / 无 webhook 密钥一律报错 → 404；**无空 slug 兜底**，同 GitHub 新入口的立场 `main.go:271-273`） |

### 3.3 API 与 UI

**API（全部 `Auth.RequireFunc`，只操作 `CurrentUser` 自己的连接，没有任何接受他人 ID 的路径）**

| 方法 + 路径 | 行为 |
|---|---|
| `GET /api/yunxiao/connection` | `{configured, endpoint, organizationId, tokenMasked, webhookSecretMasked, hasWebhookSecret, accountName, triggerLabel, statusStarted, statusPrOpen, statusMerged, statusCancel, verifiedAt, verifyError, webhookPath}`；未配置 `{configured:false}`。**任何字段都不含明文** |
| `PUT /api/yunxiao/connection` | 保存（校验见 §3.5），token / webhookSecret 传空串 = 保留原值（首次保存 token 必填）；保存后立即调 `CurrentUser` 验证，回写 `account_id/account_name/verified_at`（并清空 `verify_error`）或 `verify_error`（并清空 `verified_at`）；验证失败**也保存**（同 `credentials.go:82-114` 立场）。`verify_error` 只存分类人话（§3.7 第 2 条），不存响应体 |
| `POST /api/yunxiao/connection/verify` | 用已存令牌重新验证：成功 → 200 `{verify:{ok:true, accountName}}`，并回写 `account_id`、`account_name`、`verified_at=now()`、`verify_error=NULL`；失败 → 200 `{verify:{ok:false, error:<分类人话>}}`，回写 `verify_error`、`verified_at=NULL`，**`account_id` 保留旧值**（网络抖动不该让接单判定失灵）；未配置连接 → 400 |
| `DELETE /api/yunxiao/connection` | 一个事务里删除连接行与该用户**全部** `yunxiao_trigger_state` 行 → 204；不碰其他用户的快照；未配置时同样 204（幂等）。理由：留着旧快照，重新连接后第一次投递会被当成「已指派→已指派」吞掉，不接单 |
| `POST /api/yunxiao/workitems/{id}/run` | 手动开跑。顺序与状态码（**无副作用的检查在前**）：① `{id}` 不符 `ParseWorkitemID` 同一规则 → **400**（不发任何出站请求）；② 未配置连接 → **400**「请先配置云效连接」；③ 用属主令牌 `GetWorkitem`，失败 → **502** + 分类人话（不含响应体、不含令牌），不建任务；④ `EnqueueYunxiao(owner, wi.ID, wi.SerialNumber)`：`ErrYunxiaoTaskActive` → **409**「该工作项已有进行中的任务」；`resolveRepo` 失败 → **400** + 原因；⑤ 成功 → **201** `{taskId, externalKey}`，任务行 `tracker_provider='yunxiao'`、`user_id`=当前用户、`external_id`=工作项 id、`external_key`=serialNumber。手动开跑**不读写** `yunxiao_trigger_state`（它只服务 webhook 的边沿检测） |
| `POST /webhooks/yunxiao/{slug}` | §3.0 的 7 步；响应口径同 Linear：`queued` / `ignored` / `duplicate` / `cancelled` / `error` |
| `GET /api/admin/yunxiao/allowed-hosts`、`PUT …`（`RequireAdminFunc`） | 读写 Region 版实例域名允许列表 `{hosts:[…]}`（§3.7）；每项须是合法 DNS 名（小写化、无端口、无通配、≤253）、不得是 IP 字面量或 `localhost`/`*.localhost`，至多 50 项；非法值 400 |

**UI**：新组件 `web/src/components/YunxiaoConnection.vue`，在 `PersonalSettings.vue` 里**只加一行**
引用（降低与 f05 PAT / f09 通知同改设置页的冲突面）。卡片内容：
- 接入点、组织 ID（附「中心版填组织 ID、Region 版留空并把接入点改成实例域名」说明）；
- 令牌、webhook 密钥（`type=password`，已配置时显示掩码与「输入新值以替换」）；
- 触发标签、开跑/PR 已开/已合并/取消 四个状态名（附「填状态的显示名，如『处理中』」）；
- 只读展示专属回调地址 `/webhooks/yunxiao/<slug>` + 复制按钮 + 配置步骤（自动化规则 → Webhook
  模板 → 触发「字段变更」与「状态变更」各一条 → Body 选【工作项数据】→ Secret 填同一串）；
- 「按工作项 ID 开跑」输入框 + 按钮（调 `/run`）。

不改 `App.vue` 菜单、不改 Board / TaskDetail（f02/f04 在改这些页）。云效任务在看板与详情页里
照常以 `externalKey`（serialNumber）展示，`trackerProvider` 字段已在列表接口里（`internal/store/query.go:92`）。

**管理员 UI**：新组件 `web/src/components/YunxiaoAllowedHosts.vue`（每行一个域名的文本框 + 保存），
在 `web/src/views/SystemSettings.vue` 里**只加一行**引用。中心版用户（绝大多数）不需要管理员做任何事。

### 3.4 状态同步（出站）与取消联动（入站）

- **按名字配置、按 id 调用**：`SyncStatus(ref, ev)` 取 ev 对应的状态名；空 → 直接返回 nil（不发任何
  HTTP 请求）；否则 `Workflow(ref)` 拉该工作项所在工作流，按 `displayName` / `name` / `nameEn`
  trim + 大小写不敏感匹配，命中则 `UpdateStatus(ref, statusID)`；**未命中返回 `ErrStatusNotFound`**
  （调用方告警，不重试）。工作项当前已是该状态时跳过 PUT（省一次写、也不触发对端自动化）。
- 三个时刻都在状态机转移**成功之后**、且**尽力而为**：同步失败不回滚、不重试、不改任务状态。
- **入站取消**：webhook 回源拿到的 `status` 名匹配 `status_cancel` → `CancelForYunxiao`。无在途任务
  即无操作（天然幂等）。语义与 Linear T7 完全一致：只改 DB 状态，不停正在跑的 agent
  （`cmd/lathe/queue.go:173-183` 已写明的已知边界，本功能不扩大也不修复它）。
- **自激防护**：保存时校验 `status_cancel` 不得与另外三个同步状态名相同（否则 Lathe 推状态 →
  对端 webhook → Lathe 取消自己）。出站推状态引起的 webhook 回到 Lathe 时，指派/标签快照未变，
  边沿检测判为 `ignored`，不会重复接单。

### 3.5 配置项（每个都写消费方）

**本功能不新增任何环境变量**（README 变量表不需新行；云效不提供环境变量兜底——那条路只为
老部署兼容存在，新平台没有存量）。配置全部经 `PUT /api/yunxiao/connection` 写入 `yunxiao_connections`，
**非法值保存时 400 拒绝**，不在运行期静默降级：

| 字段 | 校验 | 消费方 |
|---|---|---|
| `endpoint` | 必须 `https://`、无 userinfo、无 query/fragment、端口为空或 443；去尾 `/`；≤255；**host（小写化、去尾点后）必须是 `openapi-rdc.aliyuncs.com` 或 `yunxiao_allowed_hosts` 里的某一项**，否则 400「接入点不在允许列表中，Region 版实例域名请联系管理员登记」（§3.7） | `yunxiao.NewClient` 拼请求 URL |
| `organization_id` | 空或 `[A-Za-z0-9_-]{1,64}` | `yunxiao.Client.path`：非空走中心版路径 |
| `token_enc` | 首次必填；trim 后非空；≤512 | 所有 OpenAPI 调用的 `x-yunxiao-token` |
| `webhook_secret_enc` | 空（不改）或 ≥16 字节；≤512 | `YunxiaoWebhook` 验签；为 NULL 时 resolver 返回错误 → 404 |
| `account_id` / `account_name` | 只由验证回写，不接受客户端输入 | 接单判定（`assignedTo.id == account_id`）/ 界面展示 |
| `trigger_label` | trim；≤200；空 = 关闭（同 `admin.go:84-91` 口径） | webhook 接单判定 |
| `status_started` / `status_pr_open` / `status_merged` | trim；≤64；空 = 不同步 | `yunxiao.Tracker.SyncStatus` |
| `status_cancel` | trim；≤64；空 = 关闭；不得与上面三个同名 | webhook 取消判定 |
| 系统设置 `yunxiao_allowed_hosts`（管理员） | JSON 字符串数组；每项合法 DNS 名、小写、无端口无通配、非 IP 字面量、非 `localhost`/`*.localhost`；≤50 项；默认空 = 只放行中心版 | `ValidateEndpoint`（保存连接时）与 `NewClient`（每次构造客户端时现取，管理员撤掉某域名后已存连接下一次调用即失效） |

`yunxiao.NewClient` 自身也重复做 endpoint / organization_id 校验（防线放在使用点，同
`linear.go:470-478` 的教训），测试经 `NewClientWithHTTP(cfg, httpClient, allowInsecure=true)` 注入
httptest 地址——`allowInsecure` 只在测试构造函数上存在，生产路径不可达。**出站守卫的测试不走
`allowInsecure`**：守卫自己的测试直接测 `guardedTransport()` / `ValidateEndpoint` / `isForbiddenIP`（AC-24）。

### 3.6 接单判定：快照边沿检测

云效 webhook 没有 Linear 的 `updatedFrom`，不能知道「这次改的是不是指派」。若只看「当前是否指派给我」，
失败后人改一下标题就会重新接单。方案：

```
assignedNow := wi.AssignedTo.ID != "" && wi.AssignedTo.ID == conn.AccountID
labeledNow  := conn.TriggerLabel != "" && hasLabel(wi.Labels, conn.TriggerLabel)   // 同 Linear 的 trim/大小写规则
rose        := store.ObserveTrigger(uid, wi.ID, assignedNow, labeledNow)
```

`ObserveTrigger` 在**一个事务**里分三步（READ COMMITTED 即可，不依赖可串行化隔离）：

```sql
-- ① 占位：保证这一行存在，之后才有东西可锁。首见时并发的 N 个事务里只有一个真正插入，
--    其余在 PK 上等它提交后走 DO NOTHING。占位值 (false,false) = 「从没见过」的语义。
INSERT INTO yunxiao_trigger_state (user_id, workitem_id) VALUES ($1, $2)
  ON CONFLICT (user_id, workitem_id) DO NOTHING;
-- ② 锁行读旧值：行此时一定存在，FOR UPDATE 把同一工作项的并发观察排成串。
SELECT assigned, labeled FROM yunxiao_trigger_state
  WHERE user_id = $1 AND workitem_id = $2 FOR UPDATE;
-- ③ Go 里算 rose = (assignedNow && !prevAssigned) || (labeledNow && !prevLabeled)，写回新快照。
UPDATE yunxiao_trigger_state SET assigned = $3, labeled = $4, updated_at = now()
  WHERE user_id = $1 AND workitem_id = $2;
```

**为什么不是「`SELECT … FOR UPDATE`（无行视为 false）→ UPSERT」**（初稿写法，评审 B1）：
Postgres 没有间隙锁，`FOR UPDATE` 锁不住一行还不存在的数据。首见时 N 个并发事务都读到「无行」、
都算出 `rose=true`，各自 UPSERT 成功——而「首见」恰恰是一次指派触发两条自动化规则（字段变更 +
状态变更）时的常态。实测（`/opt/lathe-wt/evidence/f08/review/b1-observe-trigger-race-probe.txt`，
本线测试库 上的临时表、20 goroutine × 30 轮）：初稿写法 29/30 轮 rose≠1、单轮最多 20 个 rose；
上面的三步写法 30/30 轮恰 1 个。

不选「单条 `INSERT … ON CONFLICT DO UPDATE … RETURNING`」的原因：dev-infra 与 CI 都是 Postgres 16
（`docker-compose.dev.yml`、`.github/workflows/ci.yml` 的 `postgres:16-alpine`），`RETURNING OLD.*`
要 18 才有；16 上的 `RETURNING` 返回的是**新**行，拿旧值要借 CTE 读快照，而 CTE 里的快照读在并发首见时同样看不到别人未提交的插入，等于把竞态挪了个
位置；三步写法每一步的语义都能直接对着 SQL 读出来。代价是首见多一次往返，可忽略。

首次见到即已指派 = 上升沿（对应 Linear 的 `create`）。`logicalStatus == "archived"` 的工作项只记快照、
不接单。

**与 Linear 行为对齐的地方**：`blocked_spec` 后「补充并重新指派」——取消指派会把快照置 false，再指派
产生上升沿，入队时撞活任务唯一索引被拒（`blocked_spec` 是活状态），人在 Lathe 详情页点重试走
`blocked_spec → queued` 的既有合法边。这与 Linear 今天的实际行为相同，本功能不改变它。

### 3.7 安全

- **凭据加密**：`token_enc`、`webhook_secret_enc` 用装配时同一个 `secret.Sealer`（AES-256-GCM，主密钥在库外）。
- **不出明文**：API 响应只有掩码；`slog` 字段里不放令牌、密钥、请求头；客户端错误串不含响应体
  且断言不含令牌（AC-4）；webhook 验签失败日志只记 owner 与 remote。云效令牌不进 agent 子进程
  （不新增 `ExtraEnv`，白名单机制 `agent.go:467-477` 不动）。
- **验签**：明文共享密钥（官方机制如此）用 `hmac.Equal` 常数时间比较。它**不保护载荷完整性**，
  所以载荷除 `id` 外一字不信——指派人、标签、状态全部用属主令牌回源拉取。伪造者即使知道密钥，
  能做的也只是「让 Lathe 重新看一眼某个工作项」，边沿检测与活任务唯一索引挡住重复接单。
- **属主隔离 / 404**：webhook 未知 slug、停用用户、未配置连接 → 一律 404（不透露哪一环）；
  `/api/yunxiao/*` 只读写 `CurrentUser` 的行；创建出的任务 `user_id = slug 属主`，任务接口的
  「非属主 404」规则（`internal/httpapi/api.go:125-135` 等）原样适用，本功能不改它。
- **SSRF / 出站守卫**（评审 B4）。威胁模型：`endpoint` 由任何普通成员填写，服务端会去访问它；
  初稿只挡 IP 字面量，成员填一个解析到内网的 https 主机名，再配合「响应体回显进 `verify_error`」
  就能让服务端替他探测并读回内网服务——这是**越权读他人资产**，不是自伤。四层防线，只用 stdlib：
  1. **host 允许列表（主防线，保存时 + 每次构造客户端时）**：只放行中心版 `openapi-rdc.aliyuncs.com`
     与管理员在系统设置 `yunxiao_allowed_hosts` 登记的 Region 版实例域名；精确匹配，不支持通配
     （不放 `*.aliyuncs.com`：阿里云 OSS/FC 等用户可控服务也在这个后缀下，通配等于给自定义出口开门）。
     成员不能自己扩列表。
  2. **拨号期 IP 校验（纵深，挡 DNS 重绑定与管理员误登记）**：`http.Transport.DialContext` 用
     `net.Dialer{Control: …}`，在 `connect(2)` 之前拿到**实际要连的** `ip:port`，`isForbiddenIP` 命中即
     返回错误、连接不发生。拒绝：`IsLoopback`、`IsPrivate`（10/8、172.16/12、192.168/16、fc00::/7）、
     `IsLinkLocalUnicast`/`IsLinkLocalMulticast`（含 169.254.169.254 元数据地址、fe80::/10）、
     `IsUnspecified`、`IsMulticast`、100.64.0.0/10（CGNAT）、IPv4-mapped IPv6 先 `Unmap()` 再判。
     因为校验在解析之后、连接之前，TOCTOU 的 DNS 重绑定也挡得住。
  3. **不跟随重定向、不走代理**：`CheckRedirect` 返回 `http.ErrUseLastResponse` 并把 3xx 归为
     「接入点返回了重定向」错误（否则允许列表内的主机可 302 到任意地址）；`Transport.Proxy = nil`
     （不读 `HTTPS_PROXY`，避免拨号校验校验的是代理 IP 而不是目标）。
  4. **响应体永不回显**：非 2xx、2xx 但 JSON 形状不符，一律只产出「分类人话 + HTTP 状态码」，
     例如 `云效返回 404：工作项不存在或令牌无权访问`。响应体只在 `slog.Debug` 里记**长度**，不记内容。
     这条同时覆盖 `verify_error`、`/run` 的 502、webhook 的 `error` 响应与回帖兜底文案。
  后果说明：Region 版用户首次接入需要管理员登记一次域名；这是换取「成员不能让服务端访问任意主机」
  的必要代价（§6 R5）。
- **入站限制**：请求体上限沿用 `maxWebhookBody`（4MB，`internal/httpapi/webhook.go:16`）。

### 3.8 并发 / 多节点

| 竞争 | 防线 |
|---|---|
| 同一工作项的两次投递并发到两个节点（含**首见**：快照行尚不存在） | `ObserveTrigger` 先 `INSERT … ON CONFLICT DO NOTHING` 占位、再 `SELECT … FOR UPDATE`（§3.6）：行先存在、再加锁，首见与非首见都被同一把行锁串成先后，只有一个拿到上升沿（AC-8 并发子用例实测）；兜底是 `tasks_one_active_per_item` 唯一索引 |
| 对端重投同一事件 | 去重键 `yunxiao:<uid>:<workitemId>:<gmtModified>` 进 `webhook_deliveries` PK；重复 → `duplicate` |
| 回源失败 | 先回源后登记：回源失败返回 503 且**未登记**，对端重投可再处理 |
| 状态同步重复 / 乱序 | 「已是目标状态就跳过」+ 同步只是尽力而为，不参与任何判定 |
| 领单调度 | 不改：`ClaimReady` 对 provider 无感 |

`internal/runner` 与 `cmd/lathe` 有改动 → 按 AGENTS.md §3 加跑 `make test-race`。

---

## 4. 范围外（明确不做）

| 不做 | 理由 / 归属 |
|---|---|
| 改 `internal/task/state.go` 的任何状态或转移 | 设计约束；本功能不需要 |
| 改验证体系（`stageVerify`、`verifications`、红绿证明） | 设计约束；云效任务走同一路径 |
| 改「非属主 404」规则 | 设计约束 |
| Linear / 内置工单的任何行为变化；`TaskEnqueuer` / `LinearWebhook` / `linear` 包改动 | 目标 5 |
| `Tracker` 接口加方法、`Caps()` 能力位 / 平台注册表 | 用可选接口 `StatusSyncer`（`AttributedTracker` 先例）即可；`docs/09-internal-issues.md:67` 的非目标继续成立 |
| 编排图（flows / 画布）接入云效节点 | `internal/flow/service.go:241-245` 硬编码 Linear，需要 `flows.tracker_provider`，属 docs/06 M6 |
| 云效工作项列表 / 同步看板（类似 `LinearIssues.vue`） | 入口用 webhook + 按 ID 开跑足够；列表需 `SearchWorkitems` 的 `spaceId` 与 conditions DSL，另开 |
| 轮询接单（无 webhook 时定时 Search） | 需要调度与租约，另开 |
| 按云效项目路由到不同仓库 | 沿用 Linear 的「属主第一个仓库」（`queue.go:642-650`），同属 docs/02 §8 后续项 |
| 云效 OAuth / 企业应用 / AccessKey 签名（老版 devops 2021-06-25 POP API） | 只做个人访问令牌一种鉴权 |
| 云效 Codeup 作为代码平台 | 属 f07 的代码平台抽象范畴；本功能只做需求来源 |
| 通知推送（Slack / 飞书 / 邮件新渠道） | f09；云效回帖是 tracker 评论，不是通知 |
| 收件箱聚合云效阻塞项 | f02 从任务状态派生，云效任务自动出现，无需本线配合 |
| 定时在云效建单 | f10 只在 Linear 建单 |
| 团队共享云效连接 | f12；本功能的连接按个人，默认不共享 |
| 用量统计、插话、回放、PAT、多 CLI、团队技能 | f03 / f01 / f04 / f05 / f06 / f11，无交集 |
| 停下正在跑的 agent（取消信号传播） | 既有已知边界（T7-AC6），本功能不扩大也不修 |

**与邻居的重叠点与边界**：
- f07：都可能动 `internal/creds`。本线只在 `Clients.Tracker` 加一个 case、`Factory` 加一个 setter，
  不碰 `GitHub()` 与 `Verifier.Verify`；不碰 `integrations_kind_check`（见 §3.1 理由 2）。
- f05 / f09：都会改 `PersonalSettings.vue`。本线只加一行组件引用。
- f12 / 任何改 `SystemSettings.vue` 的线：本线只加一行 `YunxiaoAllowedHosts` 组件引用；管理员接口挂在
  `/api/admin/yunxiao/…`，沿用现有 `RequireAdminFunc`，不碰 f12 的团队角色模型。
- f12：本线所有查询按 `user_id` 精确匹配；f12 若引入团队可见性，云效连接仍是个人凭据，不进共享。

---

## 5. 验收标准

### 5.1 约定与 PASS 的定义

所有命令在仓库根目录下执行（验收时为开发线的 worktree），先：

```bash
source <开发线环境脚本：导出 LATHE_TEST_DSN 并提供 lathe_migrate> && source /opt/lathe-wt/evidence/f08/ac_run.sh
```

**PASS 的定义**（初稿写的是「输出含 `ok` 且退出码 0」，它放过了 `ok … [no tests to run]`，评审 B2 实测）：
一条 Go 测试类 AC 判 PASS，当且仅当下面三条**同时**成立：

1. `go test -count=1 -v …` 退出码 0，并且设了 `LATHE_TEST_REQUIRE_DB=1`（连不上库时测试会 Skip，Skip 不算过）；
2. 输出里**没有** `no tests to run`，也没有任何 `--- FAIL:` / `--- SKIP:` 行；
3. AC 里**点名的每一个** Test 与 `Test/子用例` 都有一行 `--- PASS: <名字> (`。

判定器是 `/opt/lathe-wt/evidence/f08/ac_run.sh` 的 `ac_run <证据文件> <逗号分隔的点名> <go test 参数…>`，
已自测：拿初稿的 `-run 'Linear\|Webhook'` 跑会判 FAIL（`no tests to run`），点名一个不存在的用例也判 FAIL。
`-run` 一律写成锚定的 `'^(TestA|TestB)$'`，**不带反斜杠**（Go 正则里 `\|` 是字面量 `|`）。命令全部放在
§5.4 的代码块里，不放进表格，所以不必为了 Markdown 表格转义 `|`。
注意：一次 `go test` 跑多个包时，`-run` 对每个包都生效，而点名检查是对合并后的输出做的。所以点名的用例
如果分散在不同的包里，要么各包的用例名互不相同，要么拆成每个包一次 `ac_run`（AC-18 就是这么拆的）。

**先红后绿**（适用于 AC-1…AC-15、AC-17、AC-22、AC-24…AC-27；AC-16 的红见 §5.3）：每条 AC 的测试先于实现
提交。先写测试，加上只返回 `errors.New("not implemented")` 的桩（或者对既有代码的改动先不做），用 `ac_red`
跑一次，把输出存为 `/opt/lathe-wt/evidence/f08/red-AC-<n>.txt`。`ac_red` 要求每个点名的用例各有一行
`--- FAIL: <名字> (`，出现 `[build failed]`、`[setup failed]` 或 `no tests to run` 就判为无效红。实现之后用
`ac_run` 跑，把输出存为 `green-AC-<n>.txt`。PR 描述里列出这些文件。
**AC-8 多一份红**：`concurrent_first_seen_exactly_one_rise` 要对初稿算法（`SELECT … FOR UPDATE` → UPSERT）
跑一次并失败，存为 `red-AC-8-naive.txt`，证明这个用例测的正是 B1 的竞态。

### 5.2 AC 一览

| AC | 内容 |
|---|---|
| AC-1 | 取工作项：中心版路径 `/oapi/v1/projex/organizations/ORG/workitems/WI`、Region 版 `/oapi/v1/projex/workitems/WI`；请求带 `x-yunxiao-token`；按官方响应形状（fixture `internal/integration/yunxiao/testdata/get_workitem.json`）映射出 Identifier=serialNumber、Title=subject、StateName=status.displayName、Labels、AssigneeID；serialNumber 为空时退回 id |
| AC-2 | 评论：解析裸数组；RICHTEXT 评论转成纯文本；`Tracker.Issue().Context()` 含描述，以及每条评论的作者与正文 |
| AC-3 | 发评论：`POST …/comments` 的 body 恰为 `{"content":<body>,"formatType":"MARKDOWN"}`，返回服务端 id；空 body 本地拒绝，不发请求 |
| AC-4 | 错误分类：401/403/404/429/500/418 与「2xx 但形状不符」各得到对应人话，并带状态码；上游响应体里放一个哨兵串，**错误串里不出现哨兵的任何部分**；错误串是合法 UTF-8；**任何错误串都不含令牌** |
| AC-5 | 富文本转纯文本：`<p>`/`<br>`/`<li>` 换行，实体反转义，`<script>` 连内容丢弃，多字节不损坏（≥6 个子用例） |
| AC-6 | 验签：缺头、错值、空密钥 → `ErrBadSignature`；正确值通过。`ParseWorkitemID` 拒绝空、超长、含 `/`、含 `..` 的 id |
| AC-7 | webhook 处理（fake 入队器 + httptest 云效），每种情形一个子用例（名字见 §5.4） |
| AC-8 | 边沿检测：false→true 才 rose；true→true 不 rose；取消后再置真会再次 rose；**首见并发**：20 个 goroutine 对一个**从没见过**的工作项同时报 `assigned=true`，恰 1 个 rose（重复 20 轮，每轮换新 id）；**已有行并发**：同样恰 1 个；`-race` 下无 `DATA RACE` |
| AC-9 | 凭据加密：保存后 `token_enc`、`webhook_secret_enc` 的字节里不含明文；`Status()` 只含掩码；`GET /api/yunxiao/connection` 的响应体里不含令牌与密钥明文；token 传空串保留原值；换新 token 会清空 `account_id` |
| AC-10 | 鉴权与隔离：5 个成员接口加 2 个管理员接口，未登录全部 401；非管理员访问管理员接口 403（既有 `RequireAdmin` 口径，属于功能级鉴权，不涉及资源存在性）；B 的 GET 看不到 A 的连接（`configured:false`）；B 的 DELETE 不影响 A；A 经 `/run` 建出的任务，B `GET /api/tasks/{id}` → 404 |
| AC-11 | 保存校验：`http://`、`localhost`、`127.0.0.1`、`10.0.0.1`、`[::1]`、不在允许列表里的主机、带 userinfo、端口不是 443 → 400；组织 ID 含 `/` → 400；`status_cancel` 与 `status_merged` 同名 → 400；标签 201 字符 → 400；webhook 密钥不足 16 字节 → 400；中心版合法值 → 200；管理员登记过的 Region 主机 → 200 |
| AC-12 | 分派：`Clients.Tracker("yunxiao")` 返回实现了 `tracker.StatusSyncer` 的 tracker；未配置连接 → 错误含 `云效连接未配置`；`""` 与 `"linear"` 仍走 Linear；`"bogus"` 仍报未知平台；`tracker.ValidProvider("yunxiao")==true` |
| AC-13 | pipeline 状态同步钩子：fake tracker 实现 `StatusSyncer` 时，一次走到 pr_open 的执行按顺序收到 `started`、`pr_open`；`SyncStatus` 返回错误时，任务仍到 `pr_open`，`verifications` 行数与不出错时相同；不实现 `StatusSyncer` 的 fake（Linear 形态）结果与改动前一致 |
| AC-14 | merged 同步：`handleMerged` 对 yunxiao 任务调用 `SyncStatus(merged)`；对 internal 任务仍只走 `ApplyTaskOutcome`，不调 `SyncStatus` |
| AC-15 | 状态名解析：按 displayName/name/nameEn 大小写不敏感命中后，PUT body 恰为 `{"status":"<id>"}`；映射为空 → httptest 收到 0 个请求；名字不存在 → `ErrStatusNotFound` 且无 PUT；已是目标状态 → 无 PUT |
| AC-16 | 迁移成对可逆，**用 go test 断言 schema，不用 psql**（本机没有 `psql`，评审 B3）：在本线库 `本线测试库` 上用真实二进制做 up → down×3 → up，每一步之后跑一次 schema 断言测试：up 后 53–55 已登记，两张新表、CHECK 取值、触发器、主键齐全；down×3 后 53–55 都不在 `schema_migrations`、两张新表都不存在、CHECK 回到只含 linear/internal；**再 up 后上述各项全部恢复**。另外断言存在 yunxiao 任务时，0053 的 down 报 23514，数据不丢 |
| AC-17 | 入队：`EnqueueYunxiao` 建出 `tracker_provider='yunxiao'`、`external_id`=工作项 id、`external_key`=serialNumber 的 queued 行；同一工作项第二次 → `ErrYunxiaoTaskActive`；`CancelForYunxiao` 只取消同属主、同 key 的 yunxiao 任务，不碰同 key 的 linear 任务；`resolveRepo` 失败时回帖兜底 |
| AC-18 | Linear 与内置工单零改动：相关文件 diff 为空；基线 48 个顶层测试（在 `cff5332` 上、改动开始前录下的名单）全部 `--- PASS` |
| AC-19 | 不引入依赖、不改状态机、不动 skills 与历史迁移 |
| AC-20 | 全量基线：`make lint && make test && make test-race`，并补扫未入库文件的 gofmt |
| AC-21 | 前端产物已同步进 `internal/webui/dist` |
| AC-22 | 日志不出明文：用 `slog` 捕获 handler 跑完 AC-7 全部情形与 AC-4 的错误路径，捕获内容里不含令牌、webhook 密钥与上游响应体哨兵 |
| AC-23 | 文档已落地（§7） |
| AC-24 | **SSRF 出站守卫**（评审 B4）：主机名经（注入的）解析器解析到 `127.0.0.1` → 拒绝，httptest 收到 0 个请求；解析到 10/8、169.254.169.254、`::ffff:127.0.0.1`、100.64/10 → 拒绝；`isForbiddenIP` 表驱动；3xx 不跟随，重定向目标收到 0 个请求；`Transport.Proxy == nil`；允许列表只有管理员能改，撤掉某主机后已存连接下一次调用被拒；`/verify` 与 `/run` 遇到上游返回含哨兵的错误页时，HTTP 响应与 `verify_error` 列里都没有哨兵 |
| AC-25 | `POST /api/yunxiao/workitems/{id}/run`（评审 B5）：非法 id → 400，且上游收到 0 个请求；未配置连接 → 400；上游 404/500 → 502，不建任务、不回显响应体；`resolveRepo` 失败 → 400；入队返回 `ErrYunxiaoTaskActive` → 409；**真实入队端到端**：201，库里出现一行 `tracker_provider='yunxiao'`、`user_id`=调用者、`external_id`/`external_key` 正确的 queued 任务；同一工作项再调一次，经真实唯一索引 → 409，活任务仍只有 1 行；`/run` 不写 `yunxiao_trigger_state` |
| AC-26 | `DELETE /api/yunxiao/connection`（评审 B5）：连接行被删；该用户全部 `yunxiao_trigger_state` 行被删；其他用户的快照行不动；未配置时也回 204；删除并重新连接后，第一次「已指派」投递 → `queued`（旧快照不会吞掉它） |
| AC-27 | `POST /api/yunxiao/connection/verify`（评审 B5）：成功时回写 `account_id`、`account_name`、`verified_at`（非空，且晚于请求开始时刻），并清空 `verify_error`；失败时回写 `verify_error`（分类人话）、`verified_at=NULL`，**`account_id` 保留旧值**；用的是库里存的令牌（httptest 断言收到的 `x-yunxiao-token`）；未配置 → 400 |

### 5.3 测试放在哪里

| 测试 | 包 | 依赖 |
|---|---|---|
| AC-1…6、15、24（守卫部分）、AC-22 客户端部分 | `internal/integration/yunxiao` | 纯 httptest，不连库 |
| AC-7、9（API 部分）、10、11、22 webhook/API 部分、24（允许列表与不回显）、25（除端到端）、26、27 | `internal/httpapi` | httptest 云效 + 真实 Postgres（`testStoreForAPI`） |
| AC-8、9（store 部分）、16 | `internal/store` | 真实 Postgres。AC-8 用 `testsupport.PoolWithMaxConns(t, 40)`，保证 20 个 goroutine 真并发、不在连接池上排队 |
| AC-12 | `internal/creds`、`internal/tracker` | — |
| AC-13、14 | `internal/runner` | 既有 pipeline 测试夹具 |
| AC-17、25 端到端 | `cmd/lathe` | 真实 `queue` + 真实 `task.Machine` + httptest 云效 |

**AC-16 的做法**（不建、不删任何数据库）：

- `internal/store/migrate_yunxiao_test.go`（普通构建，进 `make test`）：
  - `TestYunxiaoSchemaPresent`：连 `LATHE_TEST_DSN`，调 `assertYunxiaoSchema(t, pool, true)`。CI 先 `migrate up`
    再跑测试，所以它在 CI 里同样成立。
  - `TestYunxiaoDownRefusesWithYunxiaoTask`：开一个事务，在事务里插测试用户、仓库和一行
    `tracker_provider='yunxiao'` 的任务，再在**同一个事务**里执行 `loadMigrations()` 取出的 0053 down 脚本，
    断言 SQLSTATE 23514，最后整体回滚。不留下任何残留，也不碰真实的迁移记录。
- `internal/store/migrate_yunxiao_absent_test.go`（`//go:build migcheck`，**不进** `make test`）：
  `TestYunxiaoSchemaAbsent` 调 `assertYunxiaoSchema(t, pool, false)`，只在 down×3 之后、用
  `-tags migcheck` 显式跑。用构建标签而不用环境变量来切换，这样不新增配置项（铁律 3）。
- `assertYunxiaoSchema(t, pool, present)` 的断言：
  - `schema_migrations` 里 53、54、55 三个版本**全部存在**（present）或**全部不存在**（absent）。不断言
    `max(version)=55`：邻居线的迁移合入 dev 后最大版本会超过 55，那样断言会无故变红；
  - `to_regclass('yunxiao_connections')`、`to_regclass('yunxiao_trigger_state')` 非空或为空；
  - 对 `tasks_tracker_provider_check` 取 `pg_get_constraintdef`：present 时含 `'yunxiao'`；absent 时不含
    `'yunxiao'`，并且含 `'linear'` 与 `'internal'`；
  - present 时还要断言 `yunxiao_connections_updated_at` 触发器存在（`pg_trigger`），`yunxiao_trigger_state`
    的主键列恰为 `(user_id, workitem_id)`（`pg_index` + `pg_attribute`）。
- 本线库 `本线测试库` 只有 dev 的迁移加本线的 0053–0055，所以 down×3 退掉的恰好是本线的三条（验收前用
  `lathe_migrate up` 的输出确认最高三条就是 0053–0055）。
- 先红：在 0053–0055 文件还不存在时跑 `TestYunxiaoSchemaPresent`，失败输出（缺版本 53 等）存为
  `red-AC-16.txt`。

### 5.4 验收命令

每条都是「命令 → 期望」。`ac_run` 的期望一律是打印 `ac_run: PASS`、退出码 0。

```bash
# AC-1
ac_run /opt/lathe-wt/evidence/f08/green-AC-1.txt \
  TestClientGetWorkitem/center_path,TestClientGetWorkitem/region_path,TestClientGetWorkitem/field_mapping,TestClientGetWorkitem/serial_empty_falls_back_to_id \
  ./internal/integration/yunxiao -run '^TestClientGetWorkitem$'

# AC-2
ac_run /opt/lathe-wt/evidence/f08/green-AC-2.txt \
  TestClientListComments/bare_array,TestClientListComments/richtext_to_plain,TestTrackerIssueContext/description_and_comments \
  ./internal/integration/yunxiao -run '^(TestClientListComments|TestTrackerIssueContext)$'

# AC-3
ac_run /opt/lathe-wt/evidence/f08/green-AC-3.txt \
  TestClientCreateComment/markdown_body_exact,TestClientCreateComment/empty_body_no_request \
  ./internal/integration/yunxiao -run '^TestClientCreateComment$'

# AC-4
ac_run /opt/lathe-wt/evidence/f08/green-AC-4.txt \
  TestClientErrorClassification/status_401,TestClientErrorClassification/status_403,TestClientErrorClassification/status_404,TestClientErrorClassification/status_429,TestClientErrorClassification/status_500,TestClientErrorClassification/status_418_other,TestClientErrorClassification/shape_mismatch_2xx,TestClientErrorClassification/body_never_echoed,TestClientErrorClassification/token_never_in_error,TestClientErrorClassification/valid_utf8 \
  ./internal/integration/yunxiao -run '^TestClientErrorClassification$'

# AC-5
ac_run /opt/lathe-wt/evidence/f08/green-AC-5.txt \
  TestToPlainText/p_newline,TestToPlainText/br_newline,TestToPlainText/li_newline,TestToPlainText/entity_unescape,TestToPlainText/script_dropped,TestToPlainText/multibyte_intact,TestToPlainText/markdown_passthrough \
  ./internal/integration/yunxiao -run '^TestToPlainText$'

# AC-6
ac_run /opt/lathe-wt/evidence/f08/green-AC-6.txt \
  TestVerifySecret/missing_header,TestVerifySecret/wrong_value,TestVerifySecret/empty_secret,TestVerifySecret/correct,TestParseWorkitemID/empty,TestParseWorkitemID/too_long,TestParseWorkitemID/slash,TestParseWorkitemID/dotdot,TestParseWorkitemID/valid \
  ./internal/integration/yunxiao -run '^(TestVerifySecret|TestParseWorkitemID)$'

# AC-7
ac_run /opt/lathe-wt/evidence/f08/green-AC-7.txt \
  TestYunxiaoWebhook/unknown_slug_404,TestYunxiaoWebhook/disabled_user_404,TestYunxiaoWebhook/no_webhook_secret_404,TestYunxiaoWebhook/bad_secret_401,TestYunxiaoWebhook/missing_id_400,TestYunxiaoWebhook/upstream_fail_503_not_recorded,TestYunxiaoWebhook/first_seen_assigned_queued_once,TestYunxiaoWebhook/redelivery_duplicate,TestYunxiaoWebhook/title_edit_still_assigned_ignored,TestYunxiaoWebhook/unassign_then_reassign_queued,TestYunxiaoWebhook/label_rise_queued,TestYunxiaoWebhook/label_trigger_off_ignored,TestYunxiaoWebhook/cancel_status_calls_cancel,TestYunxiaoWebhook/forged_payload_assignee_ignored,TestYunxiaoWebhook/archived_ignored \
  ./internal/httpapi -run '^TestYunxiaoWebhook$'

# AC-8（-race；ac_run 之后再确认没有 DATA RACE）
ac_run /opt/lathe-wt/evidence/f08/green-AC-8.txt \
  TestObserveTrigger/false_to_true_rises,TestObserveTrigger/true_to_true_no_rise,TestObserveTrigger/true_false_true_rises_again,TestObserveTrigger/label_edge_independent,TestObserveTrigger/concurrent_first_seen_exactly_one_rise,TestObserveTrigger/concurrent_existing_row_exactly_one_rise \
  -race ./internal/store -run '^TestObserveTrigger$' \
  && ! grep -q 'DATA RACE' /opt/lathe-wt/evidence/f08/green-AC-8.txt
# 子用例 concurrent_first_seen_exactly_one_rise：20 轮 × 20 goroutine，每轮一个新 workitem_id，
# 用 close(start) 做起跑闸门；失败信息带 "round=%d got rose=%d want 1"。

# AC-9
ac_run /opt/lathe-wt/evidence/f08/green-AC-9.txt \
  TestYunxiaoConnectionEncrypted/token_bytes_no_plaintext,TestYunxiaoConnectionEncrypted/secret_bytes_no_plaintext,TestYunxiaoConnectionEncrypted/status_masked_only,TestYunxiaoConnectionEncrypted/empty_token_keeps_old,TestYunxiaoConnectionEncrypted/new_token_clears_account \
  ./internal/store -run '^TestYunxiaoConnectionEncrypted$'
ac_run /opt/lathe-wt/evidence/f08/green-AC-9-api.txt TestYunxiaoAPINoPlaintext \
  ./internal/httpapi -run '^TestYunxiaoAPINoPlaintext$'

# AC-10
ac_run /opt/lathe-wt/evidence/f08/green-AC-10.txt \
  TestYunxiaoAPIRequiresAuth/get_connection,TestYunxiaoAPIRequiresAuth/put_connection,TestYunxiaoAPIRequiresAuth/verify_connection,TestYunxiaoAPIRequiresAuth/delete_connection,TestYunxiaoAPIRequiresAuth/run_workitem,TestYunxiaoAPIRequiresAuth/admin_hosts_get,TestYunxiaoAPIRequiresAuth/admin_hosts_put,TestYunxiaoAPIRequiresAuth/admin_hosts_member_403,TestYunxiaoAPIIsolation/b_get_sees_unconfigured,TestYunxiaoAPIIsolation/b_delete_keeps_a_connection,TestYunxiaoAPIIsolation/b_cannot_read_a_task_404 \
  ./internal/httpapi -run '^(TestYunxiaoAPIRequiresAuth|TestYunxiaoAPIIsolation)$'

# AC-11
ac_run /opt/lathe-wt/evidence/f08/green-AC-11.txt \
  TestYunxiaoAPISaveValidation/http_scheme_400,TestYunxiaoAPISaveValidation/localhost_400,TestYunxiaoAPISaveValidation/loopback_literal_400,TestYunxiaoAPISaveValidation/private_literal_400,TestYunxiaoAPISaveValidation/ipv6_loopback_400,TestYunxiaoAPISaveValidation/host_not_allowlisted_400,TestYunxiaoAPISaveValidation/userinfo_400,TestYunxiaoAPISaveValidation/non_443_port_400,TestYunxiaoAPISaveValidation/org_slash_400,TestYunxiaoAPISaveValidation/cancel_equals_merged_400,TestYunxiaoAPISaveValidation/label_too_long_400,TestYunxiaoAPISaveValidation/secret_too_short_400,TestYunxiaoAPISaveValidation/center_valid_200,TestYunxiaoAPISaveValidation/allowlisted_region_valid_200 \
  ./internal/httpapi -run '^TestYunxiaoAPISaveValidation$'

# AC-12（两个包的用例名互不相同，可以一次跑）
ac_run /opt/lathe-wt/evidence/f08/green-AC-12.txt \
  TestClientsTracker/yunxiao_is_status_syncer,TestClientsTracker/yunxiao_unconfigured,TestClientsTracker/empty_is_linear,TestClientsTracker/linear_is_linear,TestClientsTracker/bogus_unknown,TestValidProvider/yunxiao,TestValidProvider/linear,TestValidProvider/internal,TestValidProvider/bogus \
  ./internal/creds ./internal/tracker -run '^(TestClientsTracker|TestValidProvider)$'

# AC-13
ac_run /opt/lathe-wt/evidence/f08/green-AC-13.txt \
  TestPipelineStatusSync/order_started_then_pr_open,TestPipelineStatusSync/sync_error_does_not_change_outcome,TestPipelineStatusSync/non_syncer_unchanged \
  ./internal/runner -run '^TestPipelineStatusSync$'

# AC-14
ac_run /opt/lathe-wt/evidence/f08/green-AC-14.txt \
  TestMergePollerStatusSync/yunxiao_syncs_merged,TestMergePollerStatusSync/internal_no_sync \
  ./internal/runner -run '^TestMergePollerStatusSync$'

# AC-15
ac_run /opt/lathe-wt/evidence/f08/green-AC-15.txt \
  TestTrackerSyncStatus/match_display_name,TestTrackerSyncStatus/match_name_case_insensitive,TestTrackerSyncStatus/match_name_en,TestTrackerSyncStatus/empty_mapping_no_request,TestTrackerSyncStatus/not_found_no_put,TestTrackerSyncStatus/already_in_status_no_put \
  ./internal/integration/yunxiao -run '^TestTrackerSyncStatus$'

# AC-16（顺序执行；每步失败即停。前提：本线测试库 里没有 yunxiao 任务残留，
#        有就说明某个测试漏了 t.Cleanup，先修测试再验收）
E=/opt/lathe-wt/evidence/f08
lathe_migrate up > $E/green-AC-16-1-up.txt 2>&1 \
  && ac_run $E/green-AC-16-2-present.txt TestYunxiaoSchemaPresent,TestYunxiaoDownRefusesWithYunxiaoTask \
       ./internal/store -run '^(TestYunxiaoSchemaPresent|TestYunxiaoDownRefusesWithYunxiaoTask)$' \
  && { lathe_migrate down && lathe_migrate down && lathe_migrate down; } > $E/green-AC-16-3-down3.txt 2>&1 \
  && ac_run $E/green-AC-16-4-absent.txt TestYunxiaoSchemaAbsent \
       -tags migcheck ./internal/store -run '^TestYunxiaoSchemaAbsent$' \
  && lathe_migrate up > $E/green-AC-16-5-up.txt 2>&1 \
  && ac_run $E/green-AC-16-6-restored.txt TestYunxiaoSchemaPresent \
       ./internal/store -run '^TestYunxiaoSchemaPresent$'

# AC-17
ac_run /opt/lathe-wt/evidence/f08/green-AC-17.txt \
  TestQueueYunxiao/creates_yunxiao_row,TestQueueYunxiao/second_enqueue_task_active,TestQueueYunxiao/cancel_only_same_owner_key_provider,TestQueueYunxiao/resolve_repo_failure_comments \
  ./cmd/lathe -run '^TestQueueYunxiao$'

# AC-18：diff 为空 + 基线名单全过。名单是改动开始前在 cff5332 上录下的
#（22 个 httpapi 测试 + 26 个 linear/tracker 测试，共 48 个；已在基线上用下面同样的命令实跑判 PASS）。
# 按包拆成两次，因为 -run 对一次调用里的所有包都生效。
E=/opt/lathe-wt/evidence/f08
test -z "$(git diff --stat origin/dev...HEAD -- internal/integration/linear internal/httpapi/webhook.go internal/httpapi/linear.go internal/tracker/local.go internal/httpapi/webhook_test.go internal/httpapi/linear_test.go internal/tracker/local_test.go)" \
  && ac_run $E/green-AC-18-pkgs.txt "$(paste -sd, $E/baseline-AC-18.pkgs.names)" \
       ./internal/integration/linear ./internal/tracker \
  && ac_run $E/green-AC-18-httpapi.txt "$(paste -sd, $E/baseline-AC-18.httpapi.names)" \
       ./internal/httpapi -run '^(TestLinear|TestWebhook)'

# AC-19 → 两条都无输出
git diff --name-only origin/dev...HEAD -- go.mod go.sum web/package.json web/pnpm-lock.yaml internal/task/state.go skills/
git diff --name-only --diff-filter=MD origin/dev...HEAD -- migrations/

# AC-20 → 退出码 0；最后一条无输出
LATHE_TEST_REQUIRE_DB=1 make lint && LATHE_TEST_REQUIRE_DB=1 make test && LATHE_TEST_REQUIRE_DB=1 make test-race
gofmt -l $(git ls-files --others --exclude-standard '*.go')

# AC-21 → 退出码 0，且 grep 至少列出 1 个文件
make ui && make build && grep -rl '云效连接' internal/webui/dist

# AC-22（两个包里各一个测试，名字不同，免得同名 PASS 行互相顶替）
ac_run /opt/lathe-wt/evidence/f08/green-AC-22.txt \
  TestYunxiaoNoSecretInLogs/webhook_paths,TestYunxiaoNoSecretInLogs/api_paths,TestYunxiaoClientNoSecretInLogs/error_paths \
  ./internal/httpapi ./internal/integration/yunxiao -run '^(TestYunxiaoNoSecretInLogs|TestYunxiaoClientNoSecretInLogs)$'

# AC-23 → test -f 成功，三个计数都 ≥1
test -f docs/features/tracker-yunxiao.md
grep -c '云效' CHANGELOG.md
grep -c '/webhooks/yunxiao/' README.md
sed -n '/## \[Unreleased\]/,/## \[0/p' CHANGELOG.md | grep -c '云效'

# AC-24（守卫单测 + API 层允许列表与不回显）
ac_run /opt/lathe-wt/evidence/f08/green-AC-24.txt \
  TestOutboundGuard/resolved_loopback_refused,TestOutboundGuard/resolved_private_refused,TestOutboundGuard/resolved_metadata_refused,TestOutboundGuard/resolved_mapped_v6_loopback_refused,TestOutboundGuard/resolved_cgnat_refused,TestOutboundGuard/redirect_not_followed,TestOutboundGuard/proxy_disabled,TestIsForbiddenIP,TestValidateEndpoint \
  ./internal/integration/yunxiao -run '^(TestOutboundGuard|TestIsForbiddenIP|TestValidateEndpoint)$'
ac_run /opt/lathe-wt/evidence/f08/green-AC-24-api.txt \
  TestYunxiaoAPIEndpointAllowlist/admin_registers_region_host_then_member_saves,TestYunxiaoAPIEndpointAllowlist/admin_removal_blocks_next_call,TestYunxiaoAPIEndpointAllowlist/allowlist_rejects_ip_and_localhost,TestYunxiaoAPINoBodyEcho/verify_error_column,TestYunxiaoAPINoBodyEcho/verify_response,TestYunxiaoAPINoBodyEcho/run_502_response \
  ./internal/httpapi -run '^(TestYunxiaoAPIEndpointAllowlist|TestYunxiaoAPINoBodyEcho)$'
# resolved_loopback_refused 的做法：guardedTransport 接受一个注入的 lookup 函数，测试把
# "intranet.example.test" 映射到 127.0.0.1，httptest.NewTLSServer 监听在 127.0.0.1；
# 断言：请求报错、错误分类为「接入点解析到内网地址」、server 计数器 == 0。
# 注入点只换「主机名 → IP」；Dialer.Control 检查的是实际拨号地址，注入的解析器绕不过它。
# 另有一条不注入的子用例：yx.localhost 走系统解析器（本机实测 Go 把 *.localhost 解析为
# 127.0.0.1 / ::1），先被 ValidateEndpoint 拒，强行构造 Transport 再请求也被 Control 拒。

# AC-25
ac_run /opt/lathe-wt/evidence/f08/green-AC-25.txt \
  TestYunxiaoAPIRun/invalid_id_slash_400_no_upstream,TestYunxiaoAPIRun/invalid_id_dotdot_400_no_upstream,TestYunxiaoAPIRun/invalid_id_too_long_400_no_upstream,TestYunxiaoAPIRun/unconfigured_400,TestYunxiaoAPIRun/upstream_404_502_no_task,TestYunxiaoAPIRun/upstream_500_502_no_echo,TestYunxiaoAPIRun/resolve_repo_fail_400,TestYunxiaoAPIRun/task_active_409,TestYunxiaoAPIRun/no_trigger_state_write \
  ./internal/httpapi -run '^TestYunxiaoAPIRun$'
ac_run /opt/lathe-wt/evidence/f08/green-AC-25-e2e.txt \
  TestYunxiaoRunEndToEnd/created_201_yunxiao_row,TestYunxiaoRunEndToEnd/second_run_409_single_active_row \
  ./cmd/lathe -run '^TestYunxiaoRunEndToEnd$'

# AC-26
ac_run /opt/lathe-wt/evidence/f08/green-AC-26.txt \
  TestYunxiaoAPIDeleteClearsTriggerState/deletes_connection_row,TestYunxiaoAPIDeleteClearsTriggerState/clears_all_own_trigger_rows,TestYunxiaoAPIDeleteClearsTriggerState/keeps_other_users_rows,TestYunxiaoAPIDeleteClearsTriggerState/unconfigured_204,TestYunxiaoAPIDeleteClearsTriggerState/reconnect_first_assigned_delivery_queued \
  ./internal/httpapi -run '^TestYunxiaoAPIDeleteClearsTriggerState$'

# AC-27
ac_run /opt/lathe-wt/evidence/f08/green-AC-27.txt \
  TestYunxiaoAPIVerify/success_writes_account_and_verified_at,TestYunxiaoAPIVerify/success_clears_verify_error,TestYunxiaoAPIVerify/failure_writes_error_clears_verified_at,TestYunxiaoAPIVerify/failure_keeps_account_id,TestYunxiaoAPIVerify/uses_stored_token,TestYunxiaoAPIVerify/unconfigured_400 \
  ./internal/httpapi -run '^TestYunxiaoAPIVerify$'
```

子用例名里的空格会被 `go test` 换成 `_`，所以子用例名一律直接写成下划线形式，点名与 `--- PASS` 行逐字一致。

**上线条件（不是 AC，人工完成，证据进 `/opt/lathe-wt/evidence/f08/probe/`）**：用一个真实云效组织的
令牌在本线的手测实例上走一遍：① 保存连接验证成功且回写的 `account_id` 与该用户被指派时工作项的
`assignedTo.id` 一致；② 配自动化规则投递一次，任务建出；③ 分诊回帖出现在云效评论区；
④ 状态同步 PUT 成功（尤其验证中心版 `PUT` 路径，见 R2）。任何一项不符先改实现再合入。

---

## 6. 风险与代价

| # | 风险 / 代价 | 应对 |
|---|---|---|
| R1 | **测试 fixture 的形状来自官方文档，不是真实抓包**：文档示例值多为占位（`"test"`、带掩码的 id），字段可能缺漏 | fixture 文件头注释写明来源 URL 与取证日期；解析只依赖 §2.6 表中字段，其余忽略；上线条件 ①–④ 用真实租户兜底 |
| R2 | UpdateWorkitem 文档只给出 Region 形态路径 `/oapi/v1/projex/workitems/{id}`，中心版是否需要组织段未写明 | 客户端对 PUT 同样按「有组织 ID 则加组织段」拼（与其余 5 个接口一致）；上线条件 ④ 实测；不符时只改 `path()` 一处 |
| R3 | `GetUserByToken` 的 `id` 与 Projex `assignedTo.id` 是否同一 ID 空间，文档未明说 | 上线条件 ①；若不同，接单判定改为按 `ReadMemberByUser` 映射——只影响 `account_id` 的取法，不影响其余设计 |
| R4 | 明文共享密钥（非 HMAC）不防篡改、不防重放 | 载荷只取 id、权威数据回源、边沿检测 + 唯一索引；文档提示务必 https 回调 |
| R5 | SSRF：成员填写的接入点被服务端访问（初稿把它判为「自伤为主」，评审 B4 指出错误：服务端是在替成员访问内网，并且会读回响应） | §3.7 的四层守卫：host 精确允许列表（成员不能扩）、`Dialer.Control` 拨号期 IP 校验（挡 DNS 重绑定与管理员误登记）、不跟随重定向、不走代理、响应体永不回显。**代价**：Region 版用户首次接入要等管理员登记域名。残余风险：管理员登记的域名本身被攻陷，此时请求仍只能到公网地址，令牌是该成员自己的 |
| R6 | 评论接口无分页：评论极多时分诊上下文过长 | 与 Linear 现状一致（Linear 也全取）；不做截断以免丢掉关键澄清 |
| R7 | 迁移号段 0053–0055 与 dev 当前最大 0031 之间有空号，偏离 §6「不跳号」字面 | 号段由并行编排统一分配以避免 12 条线撞号；loader 已允许空号（dev 上已缺 0026/0027/0029）。合入前若评审要求紧凑编号，按合入顺序重编号（只改文件名，未发布前允许） |
| R8 | 凭据存储出现第二个位置（`yunxiao_connections` 而非 `integrations`） | 加密实现完全复用 `secret.Sealer`；§3.1 写明取舍；将来若做「按 provider 的槽位」统一改造（docs/06 §3.5）再合并 |
| R9 | `StatusSyncer` 在 pipeline 里多了 2 处、mergepoll 1 处可选调用，与 docs/06 §3.3「pipeline 里不该出现 if caps.X」的纪律有张力 | 收敛到单一 helper `syncTrackerStatus`（类型断言只出现在这一处），与 `AttributedTracker` 的既有先例同级；docs/06 §3.3 的 `Caps()` 设计与现行代码（tracker.go:8-10 明说不做 Caps）本就不一致，本 PRD 跟随代码与 docs/09 的现行口径，并在 docs/06 §3.4 回写探针结论供评审确认 |
| R10 | 取消联动不停在途 agent | 既有边界，照写进 docs，不扩大 |
| R11 | 三处状态同步都是外部 HTTP，慢请求会拉长阶段边界 | 客户端 30s 超时；同步在转移成功后执行，不阻塞状态落库；失败只告警 |
| R12 | 错误不回显响应体后，排障信息变少（云效 400 的具体字段错误看不到） | 错误串保留 HTTP 状态码与分类；`slog.Debug` 记响应体**长度**；真要看内容，由运维在服务端用自己的令牌复现。这是 B4 要求的取舍 |
| R13 | AC-16 的 down×3 在本线库上执行，依赖「最高三条就是 0053–0055」 | 本线库只有 dev 的迁移加本线三条，验收前看 `lathe_migrate up` 的输出确认；合入 dev 后如果邻居的迁移号更高，CI 的「最新迁移可回滚」步骤仍按单步往返守 down，本 AC 的 schema 断言按版本号断言，不按 max |

---

## 7. 文档改动（同一个 PR 内）

1. **新增 `docs/features/tracker-yunxiao.md`**：本 PRD 定稿版（去掉 worktree/端口等本线环境信息，保留其余）。
2. **`README.md`**：「在设置页配置凭据」步骤（`README.md:67-80`）追加云效连接一条与回调地址
   `/webhooks/yunxiao/<你的随机段>` 的配置步骤；功能介绍处写「需求来源：Linear / 内置工单 / 云效」；
   写明「Region 版需管理员先在系统设置登记实例域名」。
   **环境变量表不新增行**（本功能无新环境变量），在表下注明「云效只能在设置页配置，无环境变量兜底」。
3. **`CHANGELOG.md` `[Unreleased]` → `### Added`**：「云效（Projex）工作项作为任务来源：指派/标签接单、
   分诊读正文与评论、在云效回帖提问与 PR 链接、可选状态同步与取消联动；webhook 按用户验签；凭据加密存储」。
4. **`docs/06-orchestration.md` §3.4**：追加「探针结论（2026-11）」小节，即本文 §2.6 末段四条；
   并注明 §3.3 的 `Caps()` 未采用、改用可选接口（R9），待评审确认。
5. **`docs/07-prd-orchestration.md` F6.4**：AC1（只读探针）、AC2（探针结论落文档）标为由 f08 交付；
   AC3（云效绑定的图）标注仍未做（编排图接入在范围外）。
6. **`internal/tracker/tracker.go` 包注释**：「两个方法」的纪律不变，补一句可选扩展接口
   `AttributedTracker` / `StatusSyncer` 的消费方与第二个外部平台已落地。

---

## 8. 评审答复（第 1 轮，B1–B5）

五条全部接受，没有不同意的条目。下面每条写明改了哪里、证据在哪。

| # | 结论 | 改动位置 | 证据 |
|---|---|---|---|
| B1 并发首见竞态 | **接受，选 (a)**。改为 `INSERT … (false,false) ON CONFLICT DO NOTHING` → `SELECT … FOR UPDATE` → `UPDATE`。没选 (b)：Postgres 16 的 `RETURNING` 只给新行，`RETURNING OLD` 要到 18；用 CTE 取旧值，在并发首见时同样看不到别人未提交的插入 | §3.1（0055 列补默认值，作为占位语义）、§3.6、§3.8；AC-8 加子用例 `concurrent_first_seen_exactly_one_rise`、`concurrent_existing_row_exactly_one_rise`，并要求对初稿算法另存一份红 `red-AC-8-naive.txt` | 在 本线测试库 的临时表上实测，20 goroutine × 30 轮：初稿 29/30 轮 rose≠1（最坏 20 个），新写法 30/30 轮恰 1 个。`/opt/lathe-wt/evidence/f08/review/b1-observe-trigger-race-probe.txt`，程序 `….go.txt`；临时表已删 |
| B2 AC 空跑通过 | **接受**。复现：`go test ./internal/httpapi -run 'Linear\|Webhook'` 输出 `ok … [no tests to run]`，退出码 0 | §5.1 重新定义 PASS：退出码 0，且无 `no tests to run`/FAIL/SKIP，且每个点名用例都有 `--- PASS`。新增判定器 `ac_run`/`ac_red`。所有命令移进 §5.4 代码块，`-run` 锚定、不带反斜杠。AC-18 改成按名单校验：名单是在基线上录的 48 个测试，按包拆成两次调用，因为 `-run` 对同一调用里的每个包都生效 | `/opt/lathe-wt/evidence/f08/ac_run.sh` 自测：初稿正则判 FAIL，点名不存在的用例判 FAIL，正确命令判 PASS。基线名单 `baseline-AC-18.{pkgs,httpapi}.names`，在 cff5332 上用 §5.4 的 AC-18 命令实跑判 PASS |
| B3 AC-16 无 psql | **接受**。用 go test 查 `schema_migrations`、`to_regclass`、`pg_get_constraintdef`、`pg_trigger`、`pg_index`。down×3 后断言新对象不存在，再 up 后断言全部恢复，不只看版本号 | §5.2 AC-16、§5.3、§5.4。schema 断言按版本 53–55 是否登记来写，不写 `max=55`，免得邻居线合入后无故变红。存在 yunxiao 任务时 down 被拒，这一点在单个事务里执行 0053 down 并回滚来测，不留残留 | `which psql` 无输出（复核）。没用 `docker exec` 跑 psql：容器名属于本机环境，CI 里没有这个容器 |
| B4 SSRF 与回显 | **接受**，并认同初稿 R5「自伤为主」判断错误 | §3.2 新增 `guard.go`；§3.5 endpoint 校验改为精确允许列表，新增系统设置 `yunxiao_allowed_hosts`（存进既有的 `system_settings`，无迁移、无环境变量）；§3.7 四层守卫：允许列表、`Dialer.Control` 拨号期 IP 校验、不跟随重定向且不走代理、响应体不回显；§3.3 新增 2 个管理员接口与 1 个管理员 UI 组件；新增 AC-24，`resolved_loopback_refused` 断言解析到 127.0.0.1 的主机名被拒、上游收到 0 个请求；AC-4 从「截断回显」改为「不回显」；R5 重写，新增 R12 | 允许列表**不用** `*.aliyuncs.com` 通配：这个后缀下有 OSS/FC 等用户可控的服务。Region 实例域名的格式官方文档只在截图里给出，无法写成模式，所以只能由管理员逐个登记。本机实测 Go 把 `yx.localhost` 解析为 127.0.0.1/::1，这一情形已写进 AC-24 |
| B5 需求要点缺 AC | **接受** | §3.3 把 `/run`、`DELETE`、`/verify` 的状态码顺序与回写语义写清：无副作用的检查在前；`/run` 不碰快照；verify 失败保留 `account_id`；`Save` 换 token 时清空 `account_id`。新增 AC-25 `TestYunxiaoAPIRun`（另有 `cmd/lathe` 里经真实唯一索引的端到端测试 `TestYunxiaoRunEndToEnd`）、AC-26 `TestYunxiaoAPIDeleteClearsTriggerState`、AC-27 `TestYunxiaoAPIVerify`，都纳入先红后绿。409 判定靠新哨兵 `ErrYunxiaoTaskActive`，只在 `cmd/lathe/queue.go` 里识别 23505，不改 `internal/task` | — |

**仍遵守的设计约束**：不新增依赖（守卫只用 `net`、`net/netip`、`syscall`）；不新增环境变量；不改状态机、
验证体系与「非属主 404」规则；迁移仍是 0053–0055，没有新增迁移；Linear 文件 diff 仍为空（AC-18）。


---

## 9. 实现与验收结果（2026-11-11）

### 9.1 落地位置

| 部分 | 文件 |
|---|---|
| 云效客户端、出站守卫、富文本转纯文本、webhook 验签、tracker 实现 | `internal/integration/yunxiao/{client,guard,richtext,webhook,tracker}.go` |
| fixture（按官方文档形状手写，非抓包，见 R1） | `internal/integration/yunxiao/testdata/` |
| 平台常量、可选扩展接口 `StatusSyncer` / `Lifecycle` | `internal/tracker/tracker.go` |
| 迁移 | `migrations/0053_tracker_provider_yunxiao`、`0054_yunxiao_connections`、`0055_yunxiao_trigger_state` |
| 连接存储、接单快照（三步写法）、允许列表系统设置 | `internal/store/yunxiao.go` |
| 按 provider 分派 | `internal/creds/{factory,verifier}.go`（`WithYunxiao` + `case tracker.ProviderYunxiao`） |
| 状态同步钩子（唯一类型断言点） | `internal/runner/statussync.go`；调用点在 `pipeline.go` 的 `stageTriage` / `stagePushAndPR` 与 `mergepoll.go` 的 `handleMerged` |
| HTTP：成员 5 个接口、管理员 2 个接口、webhook | `internal/httpapi/yunxiao.go` |
| 入队 / 取消联动、装配 | `cmd/lathe/queue.go`（末尾追加）、`cmd/lathe/main.go` |
| 界面 | `web/src/components/YunxiaoConnection.vue`、`YunxiaoAllowedHosts.vue`；设置页 / 系统设置页各加一行引用；`web/src/api.js` 追加 7 个方法 |

### 9.2 与设计稿的差异（都在设计的约束内）

1. **`/run` 的 `resolveRepo` 失败**：`EnqueueYunxiao` 把它包成哨兵 `httpapi.ErrYunxiaoNoRepo`
   （与 `ErrYunxiaoTaskActive` 对称），`/run` 据此回 400；设计稿只写了「回 400 + 原因」，没定哨兵名。
2. **`YunxiaoEnqueuer.EnqueueYunxiao` 返回任务 id**（`(int64, error)`），供 `/run` 的 201 响应带 `taskId`。
3. **webhook 回源失败的响应体**是 `{"status":"error","error":<分类人话>}` + 503；分类人话由客户端保证
   不含响应体与令牌（AC-4 / AC-24）。
4. **去重键兜底**：回源对象缺 `gmtModified` 时，去重键退回「状态 id + 指派人 + 标签」的组合，至少挡住
   「什么都没变」的重投。正常情况下官方对象带 `gmtModified`。
5. **归档工作项**照样记快照再判 `ignored`（「只记快照、不接单」）。
6. 云效客户端额外校验 `CurrentUser`、`GetWorkitem`、`CreateComment` 返回体里必须有 `id`，否则按
   「2xx 但形状不符」处理。

### 9.3 验收结果

全部 AC 按 §5.4 的命令跑过；Go 测试类 AC 都先用 `ac_red` 取得有效红，再用 `ac_run` 判 PASS。

| AC | 结果 | 先红 | 备注 |
|---|---|---|---|
| AC-1…6 | PASS | 有 | AC-6 的 `correct` 子用例在桩上天然通过，未列入红名单 |
| AC-7 | PASS | 有 | 真实 resolver + 真实快照表 + 真实去重表，入队器是假件 |
| AC-8 | PASS（`-race`，无 DATA RACE） | 有；另有 `red-AC-8-naive.txt`：初稿算法 20/20 轮 rose≠1 | |
| AC-9 | PASS（store + API） | 有 | |
| AC-10 | PASS | 有 | 身份走真实会话 Cookie |
| AC-11 | PASS | 有 | 400 的子用例同时断言库里没有留下连接行 |
| AC-12 | PASS | 有（tracker、creds 各一份） | |
| AC-13、14 | PASS | 有 | `non_syncer_unchanged` / `internal_no_sync` 是回归守卫，改动前即通过 |
| AC-15 | PASS | 有 | |
| AC-16 | PASS：up → present → down×3 → absent（`-tags migcheck`）→ up → restored | 有（迁移文件不存在时） | 本线库最高三条确认为 0053–0055 |
| AC-17 | PASS | 有 | |
| AC-18 | PASS：Linear 相关文件 diff 为空；基线 48 个测试全过 | — | |
| AC-19 | PASS：两条 diff 都无输出 | — | |
| AC-20 | PASS | — | 见 PR 描述里的门禁记录 |
| AC-21 | PASS：`make ui && make build`，`internal/webui/dist` 里能 grep 到「云效连接」 | — | dist 产物不入库（.gitignore），由构建同步 |
| AC-22 | PASS | 有 | |
| AC-23 | 本文件、CHANGELOG、README 已落地 | — | |
| AC-24 | PASS（守卫单测 + API 层允许列表与不回显） | 有 | `system_resolver_localhost_refused` 子用例走系统解析器 |
| AC-25 | PASS（httpapi + `cmd/lathe` 端到端） | 有 | |
| AC-26、27 | PASS | 有 | |

另做了一次本地冒烟：独立实例 + 独立库，覆盖保存校验（http、IP、取消与同步状态同名）、掩码展示、
非法 id、允许列表登记 / 拒绝 IP、未知 slug 404、未登录 401、界面产物含云效卡片、删除 204；
服务日志里没有令牌与密钥明文。

### 9.4 尚未完成：上线条件（人工，需要真实租户）

§5.4 末尾的**上线条件 ①–④** 需要真实的云效组织与令牌，开发环境不连外网，**尚未执行**。合入前要有人
用真实租户走一遍，重点核对 R2（中心版 `PUT` 路径是否需要组织段）与 R3（`GetUserByToken.id` 与
`assignedTo.id` 是否同一 ID 空间）。任何一项不符，先改实现再合入。
