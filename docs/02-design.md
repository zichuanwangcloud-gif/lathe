# Lathe — 系统设计

> 版本：v1 / 2026-08-12
> 前置：[00-analysis.md](./00-analysis.md)（可行性与基线）、[01-decisions.md](./01-decisions.md)（D1–D4 决策）

---

## 1. 产品定义

**一句话**：绑定 Linear 账户后，指派给你的 issue 自动变成一个已验证的 PR。

**边界**：
- Lathe **不做**合并决策 —— 产出 PR，人点合并
- Lathe **不做**需求澄清 —— 单子不明确就回帖提问并停下，不猜
- Lathe 的核心不是"调用 agent 写代码"，是**证明改动有效**（见 §5）

---

## 2. 系统组件

```
┌─────────────────────────── Control Plane ───────────────────────────┐
│                                                                     │
│  Webhook Ingress ──► Task Store ◄──► Scheduler ──► Job Queue        │
│   (Linear 事件)       (状态机/事件流)   (能力匹配/租约)      │        │
│                            ▲                                │       │
│  Integrations ─────────────┘                                │       │
│   Linear OAuth · GitHub App · CloudRouter Token             │       │
└─────────────────────────────────────────────────────────────┼───────┘
                                                              │
        ┌──────────────────────┬──────────────────────────────┘
        ▼                      ▼
┌───────────────┐      ┌───────────────┐
│  Runner Node  │      │  Runner Node  │   ← 可横向扩（本机 / 阿里云 / ...）
│               │      │               │
│ Worktree Mgr  │      │ Worktree Mgr  │
│ Agent Exec    │      │ Agent Exec    │  claude CLI headless
│ Verify Harness│      │ Verify Harness│  compose 隔离栈
└───────────────┘      └───────────────┘
```

**Control Plane 无状态化**，状态全在 Postgres。Job Queue 用 Postgres（`SELECT ... FOR UPDATE SKIP LOCKED`）即可，此量级不需要引入 MQ。

**Runner 是纯执行体**，不持久化业务状态，崩溃后任务由租约超时重新派发。

---

## 3. 任务状态机

这是整个系统的骨架。**状态必须持久化，且所有转移可从事件流重放。**

| 状态 | 含义 | 出口 |
|---|---|---|
| `queued` | 已接单，等派发 | → `triaging` |
| `triaging` | 判断明确度、定位涉及范围 | → `implementing` / `blocked_spec` |
| `blocked_spec` | 单子不明确，已回帖提问 | ← 人补充后回到 `queued`；或 `cancelled` |
| `awaiting_approval` | 仅 `plan-first`/`manual` 档使用 | → `implementing` / `cancelled` |
| `implementing` | agent 写代码 | → `verifying` / `failed` |
| `verifying` | 按档位跑验证（§5） | → `pr_open` / `failed` |
| `pr_open` | PR 已开，已回帖 Linear | → `review_feedback` / `merged` / `cancelled` |
| `review_feedback` | 收到 review 意见，待二轮 | → `implementing`（**必须 resume 原 session**） |
| `merged` | 终态：已合并，回收工作区 | — |
| `failed` | 终态：保留现场 + 回帖 + 推送（D4） | ← 人工可重新入队 |
| `cancelled` | 终态：人工中止 | — |

### 关键约束

**① `review_feedback` → `implementing` 必须复用原 Claude Code 会话。**
任务表持久化 `agent_session_id`；二轮用 `claude --resume <id>` 续跑，而不是带着 PR 评论重开一个会话。重开会丢掉第一轮的全部推理与代码定位上下文，这是同类系统最常见的失败点。

**② 准入档位（D2 的四档）只影响 `triaging` 的出口。**
`direct` 档下 `triaging` 只做明确度判定，通过即直接进 `implementing`，不产生 `awaiting_approval`。

**③ 幂等。**
Linear webhook 会重投递。用 `webhook_deliveries(delivery_id PK)` 去重表，先落库再处理。

---

## 4. 数据模型

```sql
users(id, email, created_at,
      password_hash,          -- bcrypt cost 12；NULL = 尚未设置，任何情况下不当作空口令
      role,                   -- admin | member
      disabled_at,            -- NULL = 启用
      must_change_password,   -- 初始口令未改前，除改密外一律挡住
      last_login_at,
      webhook_slug)           -- 每用户专属 webhook 回调地址（P1.5 第二步已启用路由）
sessions(id, user_id, expires_at, created_at)
      -- id 是会话令牌的 sha256，明文只存在于 Cookie 里
password_reset_tokens(token_hash, user_id, expires_at, used_at, created_at)
      -- 同样只存哈希；单次消费，认领语义同 webhook_deliveries
smtp_settings(id=1, host, port, username, password_enc, from_addr, from_name,
              tls_mode,       -- starttls | tls | none
              verified_at, verify_error)
      -- 单行全局表。SMTP 是「系统怎么发信」而非「某人的凭据」，
      -- 因此不挂 user_id，也不并入 integrations
integrations(id, user_id, kind, -- linear | linear_webhook | github
                                -- 预留：linear_oauth | github_app | cloudrouter
             external_account_id, token_ref, secret_enc, scopes, expires_at)
repos(id, user_id, provider_repo, default_branch, -- e.g. dev
      branch_pattern,        -- fix/{key}-{slug}
      verify_profile_ref,    -- 见 §5
      dep_strategy)          -- pnpm-store | none
nodes(id, name, capabilities_json, -- {docker, cpu, mem_mb, disk_mb, repos_cached[]}
      last_heartbeat_at, status)
tasks(id, user_id, repo_id, linear_issue_key, state,
      gate_mode,             -- direct | guarded | plan-first | manual
      verify_tier,           -- light | heavy  (§5 判定后写入)
      agent_session_id,      -- ★ resume 用
      worktree_path, branch_name, pr_url,
      node_id, lease_expires_at,
      created_at, updated_at)
task_events(id, task_id, from_state, to_state, actor, payload_json, at)
verifications(id, task_id, tier, step, -- repro_fail | build | lint | test | repro_pass | regression
              status, log_ref, duration_ms)
webhook_deliveries(delivery_id PK, received_at)
```

`token_ref` 指向外部 secret store，**不明文入库**。

---

## 5. 验证设计（本产品的核心）

### 5.1 档位在 diff 产出后判定，而非接单时按单子文本猜

D1 说"按任务类型分级"，落地时的关键细化：**判定时机放在 agent 改完代码之后**，依据是**实际改动面**而不是 issue 文本。

| 判定信号 | 归档 |
|---|---|
| diff 只碰前端展示层（`.tsx/.css`、文案、i18n） | `light` |
| diff 碰到 API / service / DB migration / 计费逻辑 | `heavy` |
| diff 跨越前后端 | `heavy` |
| 用户在 repo 配置里强制指定 | 覆盖上述 |

理由：接单时只有一句自然语言，猜错率高；改完后有确定的文件清单，判定是确定性规则，几乎不会错。

### 5.2 `light` 档

构建通过 + lint 通过 + 类型检查通过。不起栈，不隔离，快。

### 5.3 `heavy` 档：红-绿复现证明

这解决了 01-decisions 里悬着的问题「端到端验证怎么知道原 bug 复现成功了」。

```
1. 起隔离栈（per-task compose project：动态端口 + 独立 DB schema）
2. 在【改动前】的代码上跑复现脚本  ──► 必须失败   (repro_fail)
   └─ 如果通过或跑不起来 ⇒ agent 没能理解这个 bug
      ⇒ 不许继续，转 blocked_spec 回帖说明"无法复现，请补充步骤"
3. 应用改动
4. 重跑同一复现脚本            ──► 必须通过   (repro_pass)
5. 跑受影响范围的回归测试        ──► 必须通过   (regression)
6. 拆栈，回收
```

**第 2 步是整个产品的立足点。** 一个不能先让测试失败的 agent，无权声称自己修好了 bug —— 这条规则同时把"看起来对但其实没修"的 diff 挡在了 PR 之外，也自然地把 spec 不清的单子筛了出去。

复现脚本由 agent 自己写（测试文件或脚本），随 PR 一起提交 —— 顺带给仓库留下一个回归测试。

**运行方式：声明优先于猜测。** 流水线默认从 diff 里启发式识别测试并构造运行命令（go test / vitest / jest），但猜测建立在单包布局假设上，monorepo 子包、其他框架都会猜空（任务 #596）。因此 §5.3 的输出契约包含一份可选的显式声明：agent 随改动提交 `.lathe/repro.json`：

```json
{"version":1,"tests":[{"file":"测试文件路径（必须在本次 diff 里）","cmd":["命令","参数..."],"dir":"工作目录（相对仓库根，可空）"}]}
```

流水线校验后原样执行声明的命令（argv，不经 shell；超时与进程组回收不变）。从 diff 里读 JSON 与读文件清单同样是确定性输入，不解析自然语言。声明存在但不合法（JSON 错误、file 不在 diff、cmd 为空、路径越界）按契约违例处理，不悄悄回落启发式。

**红阶段不成立的三种出口（按“谁有能力修”路由）：**

| 情形 | 出口 |
|---|---|
| 契约违例（没交测试 / 声明不合法） | 修复回路 —— agent 能自己修 |
| 执行环境错误（命令起不来 / 超时 / 目录缺失） | 任务失败留现场 —— agent 与提单人都修不了，不进修复回路空烧、不转 blocked_spec 骚扰提单人 |
| 复现测试在旧代码上通过（bug 没复现） | `blocked_spec` —— 单子没说清，回帖请提单人补充 |

### 5.4 需求类任务（无 bug 可复现）

红-绿不适用。改为：**验收标准 → 测试**。agent 从 issue 的验收标准生成测试，先证明新测试在旧代码上失败（功能确实不存在），再实现至通过。逻辑同构。
若 issue 没有可测的验收标准 ⇒ `blocked_spec`。

---

## 6. 调度与多节点（D3）

### 6.1 能力匹配，不是空闲计数

任务派发前先算需求：
```
required = { docker: (verify_tier == heavy),
             mem_mb: heavy ? 2048 : 512,
             repo: repo_id }
```
调度器只在满足 `required` 的节点中挑选，并优先选**已缓存该仓库**的节点（省一次全量 clone）。
无 docker 的节点只能承接 `light` 档。

### 6.2 双通道限流

`light` 和 `heavy` 各自独立配额，不共用一个数字 —— 资源画像差一个量级。

```
node.limits = { light: f(cpu), heavy: f(mem_free, disk_free) }
```

### 6.3 动态上限

按节点实时水位推导，而非静态常数：
- `heavy` 并发上限 = min(内存余量 / 单栈内存, 磁盘余量 / 单工作区体积)
- 任一水位低于安全阈值 ⇒ 该通道降为 0，只接 `light`
- 用户/租户层另有独立配额上限（Phase 2）

### 6.4 租约与故障恢复

派发即写 `node_id + lease_expires_at`，Runner 定期续租。节点崩溃 → 租约到期 → 任务回到 `queued` 重新派发。`implementing` 中断的任务重派时凭 `agent_session_id` 续跑。

### 6.5 智能重试：断点续跑而非从头重跑

失败任务的重试不是无条件丢弃现场重建。流水线视为阶段链
`triage → implement → commit → verify → push → pr`，重试时按
**失败阶段**（`tasks.failure_stage`，fail 时落库的机器可读代码）与
**现场体检**（`WorktreeManager.Inspect`：目录/注册/分支/提交/脏污五维）
决策续跑入口（`runner.PlanRetry`，纯逻辑可单测）：

| 失败阶段 | 现场条件 | 续跑决策 |
|---|---|---|
| 分诊及之前 | 现场还没建起来 | 从头跑（重跑分诊本来就便宜，且 issue 可能已被补充） |
| 实现中断/未完工 | worktree + 会话俱在 | resume 原会话接着干（agent 记得思路，省一整轮上下文） |
| 验证未通过 | 有未提交改动 | 先提交（人工介入的痕迹）再重验，不动 agent |
| 验证未通过 | 干净 + 会话在 | resume 会话进修复回路 |
| 验证环境错误/槽位超时 | 分支已提交 | 直接重验，不动用 agent |
| push / 开 PR 失败 | 分支已提交 | 只补 push + CreatePR（均幂等） |
| 任何 | 现场损坏/丢失 | 自动降级：丢弃重建 |

**降级链**：resume 会话失败（claude 会话数据被清理/不在本机）→ 同
worktree 开新会话（prompt 带全量需求 + 现状说明）→ 再失败才走 fail。
任何决策误判都不会比「从头重建」这个旧行为更糟。

**调用方式**：`POST /api/tasks/{id}/retry` 带 `{"mode": "auto"\|"resume"\|"fresh"}`
（缺省 auto）。`resume` 是意图承诺：现场不可用时返回 409 拒绝，不静默
重建。`GET /api/tasks/{id}/retry-plan` 只读预览决策（体检结果 + 人读理由），
UI 把它展示在失败卡片上，重试不再是黑盒。决策理由同时落进任务事件流
（续跑后第一次状态转移的 payload）。

**启动恢复同享**：Reconcile 把在途任务转回 queued 时记下中断前状态，
崩溃任务没有 failure_stage，由中断状态推导断点 —— 崩在 verifying 的
任务重启后直接重验，不再重跑 agent。

**状态机新增两条边**：`queued → implementing`、`queued → verifying`，
仅供断点续跑使用；核心不变式不变 —— 任何路径都必须经过 `verifying`
才能到 `pr_open`（push 续跑也经 verifying 中转）。

---

## 7. 主流程时序（direct 档，heavy 档验证）

```
Linear: issue 指派给用户
  └─► Webhook Ingress（去重）→ tasks(queued)
      └─► Scheduler 按能力选节点 → 派发
          └─► Runner:
              triaging     判明确度 + 定位范围
              implementing 建 worktree/分支 → claude CLI headless 实现
              verifying    起隔离栈 → 红(改前失败) → 应用 → 绿(改后通过) → 回归
              pr_open      推分支 → 开 PR → 回帖 Linear（含验证证据）
  ...人 review...
  PR 评论 → review_feedback → implementing(--resume 原 session) → 重新验证
  合并 → merged → 回收 worktree
```

失败任一步 → `failed`：**回帖说明原因 + 保留 worktree 现场 + 推送通知**（D4，不自动重试）。人工重试走 §6.5 的断点续跑。

**保留现场的边界**：现场留到「同 issue 的下一次尝试需要这个槽位」为止。`Create` 只在全新执行时调用（续跑复用现场），而唯一索引保证同 (repo, issue) 没有第二个活任务，因此建工作区时撞见的同名目录/分支必然是已终结任务的尸体 —— 直接回收再建（目录/分支/普通残留目录三种形态都处理），不报错卡死（#345/#466 的教训：尸体阻塞同 issue 重试）。回收动作落 WARN 日志，痕迹可查。

---

## 8. 分期（修订）

| 阶段 | 范围 |
|---|---|
| **P0 闭环** | 单机单账号串行。状态机 + 持久化 + 幂等；`direct` 档；`light` 验证；失败三件套；worktree 自动回收。**刻意不做**：多节点、并发、Web UI |
| **P1 验证基建（已交付 3/4）** | ~~红-绿复现证明~~（红立不起来 ⇒ blocked_spec，证据落 verifications 表）；~~§5.1 档位路由~~（diff 后定档 + 仓库级强制覆盖）；~~单机双通道并发~~（闸门在验证阶段按定档准入，light/heavy 独立配额）；per-task compose 隔离**未做**（红绿阶段目前在 git worktree 里跑，待目标仓库声明服务栈后补） |
| **P1.5 账号体系（第一步已交付）** | 平台账号：开放注册、邮箱口令登录、两级角色、SMTP 找回密码、用户管理与统计。**数据仍共享**，凭据与仓库配置挂在内置管理员名下 |
| ~~**P1.5 数据隔离（第二步）**~~ | 全链路 `owner_id`：任务/仓库/凭据按用户隔离（对非属主一律 404）；每用户专属 webhook 回调地址 `/webhooks/linear/{slug}`（旧路径兜底到内置管理员）；队列按属主解析凭据，环境变量只给超管兜底 ✅ |
| **P2 产品化** | Linear OAuth + GitHub App + CloudRouter 绑定；租户配额 |
| **P3 横向扩展** | 节点注册 + 心跳 + 能力声明；能力路由调度；跨节点仓库缓存与依赖 store |

> **「登录 Lathe」与「Lathe 代表你操作 Linear/GitHub」是两件正交的事。**
> 早期版本把它们混成了一条「多用户 = OAuth」，导致 P0 那行写着「刻意不做多用户」
> 却又在 §4 里给 `users` 建了表。现在拆开：平台账号（P1.5）用本地邮箱口令，
> 外部系统授权（P2）走 OAuth / GitHub App，两者并存互不替代。

---

## 10. 任务预览环境（跑起来给人点）

无论 AI 在代码层面做多少静态测试，前端的实际展示效果人是没有概念的。
预览环境补上这一环：任务看板上一键把任务的 worktree 跑成真实服务，
人手动点完再决定合并。

- **发现**：扫描 worktree 里的 `Dockerfile`（含 `Dockerfile.*` / `*.Dockerfile`
  变体，跳过 `.git`/`node_modules`/`vendor` 等目录），解析 `EXPOSE` 预填端口；
  未声明 EXPOSE 的由人手工指定。起哪几个镜像永远是人的选择，不自动决定。
- **构建与运行**：`docker build`（20 分钟上限）后 `docker run -d`，宿主机端口
  随机分配（`-p 0:<port>`，多任务同时预览不能撞端口），绑 `0.0.0.0`（用户从
  局域网其他机器访问）。容器与镜像一律打 `lathe.preview=1` / `lathe.task=<id>`
  标签 —— 发现、清理、跨 serve 重启的归属全靠标签，不依赖数据库状态。
- **清理**：停止按钮强删该任务的全部预览容器，再删对应镜像。镜像删除失败
  （如被引用）不算整体失败：容器已删，残留镜像只是占盘，由阈值闸门兜住。
- **资源闸门**：启动前测量内存（/proc/meminfo，口径用 MemAvailable）与磁盘
  （statfs，口径用 Bavail）占用率，任一达到阈值即拒绝启动。阈值在系统设置
  里由管理员配置（默认 90%，100 = 不启用），现取现用、改完即刻生效。
- **信任模型**：构建目标仓库的 Dockerfile 与 agent 在 worktree 里执行代码
  同级 —— 平台本来就跑 agent 写的代码，预览不引入新的信任边界。但预览
  端口对局域网可达，仓库即权限。

---

## 9. 未决

- [ ] 多用户下同仓库的并发改动冲突（两人的单动了同一文件）
- [ ] P2 租户隔离安全模型：共享节点上跑不同租户的代码
- [ ] 准入档位实测校准（抽样真实 issue 统计明确率，见 01-decisions §2）
- [ ] `light` 档是否也强制要求新增测试
- [ ] heavy 档红阶段与验证并发共占磁盘：每个 heavy 任务多一份基线 worktree，
      槽位满时应把磁盘余量纳入闸门（§6.3 动态上限的前置）
- [x] ~~**每任务的上下文基线成本需要压缩**~~（已解决）：所有 agent 调用默认传
      `--setting-sources project`（`LATHE_SETTING_SOURCES` 可调），只加载目标仓库
      自己的配置与 CLAUDE.md，个人插件不再注入上下文 —— 既省基线成本，也让任务
      执行可复现（不受某台机器装了什么插件影响）。
