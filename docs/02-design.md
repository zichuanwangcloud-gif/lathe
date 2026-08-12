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
users(id, email, created_at)
integrations(id, user_id, kind, -- linear_oauth | github_app | cloudrouter
             external_account_id, token_ref, scopes, expires_at)
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
2. 在【改动前】的代码上跑复现脚本  ──► 必须 FAIL   (repro_fail)
   └─ 如果 PASS 或跑不起来 ⇒ agent 没能理解这个 bug
      ⇒ 不许继续，转 blocked_spec 回帖说明"无法复现，请补充步骤"
3. 应用改动
4. 重跑同一复现脚本            ──► 必须 PASS   (repro_pass)
5. 跑受影响范围的回归测试        ──► 必须 PASS   (regression)
6. 拆栈，回收
```

**第 2 步是整个产品的立足点。** 一个不能先让测试失败的 agent，无权声称自己修好了 bug —— 这条规则同时把"看起来对但其实没修"的 diff 挡在了 PR 之外，也自然地把 spec 不清的单子筛了出去。

复现脚本由 agent 自己写（测试文件或脚本），随 PR 一起提交 —— 顺带给仓库留下一个回归测试。

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

---

## 7. 主流程时序（direct 档，heavy 档验证）

```
Linear: issue 指派给用户
  └─► Webhook Ingress（去重）→ tasks(queued)
      └─► Scheduler 按能力选节点 → 派发
          └─► Runner:
              triaging     判明确度 + 定位范围
              implementing 建 worktree/分支 → claude CLI headless 实现
              verifying    起隔离栈 → 红(改前FAIL) → 应用 → 绿(改后PASS) → 回归
              pr_open      推分支 → 开 PR → 回帖 Linear（含验证证据）
  ...人 review...
  PR 评论 → review_feedback → implementing(--resume 原 session) → 重新验证
  合并 → merged → 回收 worktree
```

失败任一步 → `failed`：**回帖说明原因 + 保留 worktree 现场 + 推送通知**（D4，不自动重试）。

---

## 8. 分期（修订）

| 阶段 | 范围 |
|---|---|
| **P0 闭环** | 单机单用户串行。状态机 + 持久化 + 幂等；`direct` 档；`light` 验证；失败三件套；worktree 自动回收。**刻意不做**：多用户、多节点、并发、Web UI |
| **P1 验证基建** | per-task compose 隔离；红-绿复现证明；§5.1 档位路由；单机双通道并发 |
| **P2 产品化** | Linear OAuth + GitHub App + CloudRouter 绑定；Web UI（任务看板/仓库配置/准入档位设置）；租户配额 |
| **P3 横向扩展** | 节点注册 + 心跳 + 能力声明；能力路由调度；跨节点仓库缓存与依赖 store |

---

## 9. 未决

- [ ] 多用户下同仓库的并发改动冲突（两人的单动了同一文件）
- [ ] P2 租户隔离安全模型：共享节点上跑不同租户的代码
- [ ] 准入档位实测校准（抽样真实 issue 统计明确率，见 01-decisions §2）
- [ ] `light` 档是否也强制要求新增测试
