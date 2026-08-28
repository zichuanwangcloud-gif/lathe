# Lathe — 07 · PRD：并行开发编排图

> 2026-08-27 · 状态：待评审
> 设计依据：[06-orchestration.md](./06-orchestration.md)（形态论证与撤回记录）
> 前置文档：[02-design.md](./02-design.md)（状态机、调度）、[05-roadmap.md](./05-roadmap.md)（欠账）
>
> **本文是验收契约。** 每条验收标准都标注验证方式，不可验证的条目不算标准。

---

## 1. 概述

### 1.1 一句话

**让人一次把一批有依赖关系的 issue 全部丢进 Lathe，平台自己按链路编排执行，
链上的后继向前驱的分支提 PR。**

### 1.2 问题陈述

现状（证据）：

| 现状 | 证据 |
|---|---|
| 调度是 3 worker 抢的纯 FIFO，零依赖感知 | `cmd/lathe/queue.go`：`make(chan job, 64)`，`workers = LightSlots + HeavySlots` |
| 分支基线只能是 repo 配置的两个值 | `internal/runner/branch.go:63` `BaseBranch(kind)` → `default_branch` / `hotfix_base` |
| 任务与 issue 一对一硬绑定，无分组概念 | `tasks.linear_issue_key NOT NULL` + `tasks_one_active_per_issue` 唯一索引 |
| `merged` 状态不可达，无合并检测 | `StateMerged` 在 `internal/task/` 之外零引用；`github.go` 无 merge API |

后果：要跑 `1→2→3`、`4`、`5→6` 这样一批单子，人必须**手工扣住 2、3、6**，
盯着前驱开出 PR 再挨个重新入队，并且自己记住谁等谁。链越长、批次越多，人越容易记错。

### 1.3 目标

1. 一批 issue 的依赖关系可在网页端画出并持久化
2. 平台按依赖自动决定派发时机；无依赖的节点并行跑，不互相等待
3. 链上后继的 worktree 从前驱分支分叉，PR 指向前驱分支（栈式 PR）
4. 前驱合并后，后继链自动跟进（rebase + 重验），不需要人手工救栈
5. 任一节点的失败／PR 被关闭，后继显式阻塞并留痕，不静默卡住
6. 编排能力不依赖任何特定需求平台

### 1.4 非目标（明确不做）

| 非目标 | 理由 |
|---|---|
| **自动合并 PR** | README 核心边界："不做合并决策 —— 产出 PR，人点合并" |
| **一般 DAG（入度 > 1）** | git 的 base 是单值的；多前驱需要集成分支机制，当前无场景（06 §1） |
| **图模板 / 图复用** | 节点是具体 issue，图天生一次性（06 §2.1） |
| **AI 自动拆解成图** | v1 由人画；AI 拆解列入后续（06 §11） |
| **非编码节点（人工闸门 / 脚本节点）** | 每个节点都是一个 issue（06 §2.3） |
| **跨仓库的图** | `resolveRepo` 现为"每用户第一个仓库"，前置改造未做 |
| **一个图内混用多个需求平台** | 平台是互斥替代品，绑在图上（06 §3.1） |
| **多节点分布式执行** | `cmd/lathe-runner` 仍是 54 行骨架，继承 roadmap §5 未决 |

---

## 2. 用户场景

### S1 —— 一批有链路的单子（主场景）

10 个 issue，关系是 `1→2→3`、`4` 独立、`5→6`。人在画布上把 10 张卡片摆好、连 3 条线、
点"全部入队"。之后：

- 1、4、5 同时开跑（受并发配额限制，非全部立即）
- 1 开出 PR 后 2 自动开跑，其 PR 的 base 是 1 的分支
- 全程无人工干预

### S2 —— 纯本地图（无需求平台）

不绑任何平台，人手工新建节点（自己填标题与需求描述），连线，入队。
验证"不能完全靠 Linear"这条约束真的成立。

### S3 —— 前驱在 review 里被改

1 的 PR 收到 review 意见并被修改、force-push。2 和 3 的地基变了，
平台自动重验它们（不重跑 agent），验证失败则进修复回路。

### S4 —— 前驱以 squash 方式合并

1 被 squash 合入 `default_branch`。2 的 base 分支 `branch1` 的 commit 不再是 default 的祖先。
平台自动把 2（及 3）rebase 到 `default_branch`、force-push、重验。
人在 GitHub 上看到的 2 的 diff 仍然只含 2 的改动。

### S5 —— 前驱失败或 PR 被关闭

1 `failed`，或 1 的 PR 被关闭而未合并。2、3 立刻转 `blocked_dep`，
在图上标出、并向对应 issue 回帖说明原因。**不允许静默停在 `queued`。**

---

## 3. 功能点与验收标准

**验证方式图例**：`单测` 纯逻辑可单元测试 · `集成` 用假 tracker + 短路 agent 的集成测试 ·
`端到端` 真 agent 真 GitHub · `人工` 需人工观察

---

### F1 图的创建与编辑

#### F1.1 从平台拉取 issue 并选入图

在已绑定平台的图里，展示可选 issue 列表（复用 `web/src/views/LinearIssues.vue` 的数据源），
勾选后成为图上的节点。

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | 新建图时必须选定一个平台或选"纯本地"；平台下拉只列出**凭据已配置且验证通过**的 provider | 集成 |
| AC2 | 勾选 N 个 issue 后，图上出现 N 个节点，每个节点携带 issue 的 key、标题、priority | 集成 |
| AC3 | 已在**其他未终结图**里出现的 issue 不可再次选入（或明确提示），不产生两个活任务 | 单测 + 集成 |

#### F1.2 连线建立依赖，入度 ≤ 1

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | 从 A 拖到 B 建立"B 依赖 A"，持久化为 `tasks.depends_on` | 集成 |
| AC2 | 给已有入边的节点再连一条入边，**被拒绝**并返回可读错误（不是静默覆盖） | 单测 + 集成 |
| AC3 | 构成环的连线（A→B 已存在时连 B→A）**被拒绝**并返回可读错误 | 单测 |
| AC4 | 保存后重新加载页面，节点位置与连线完全一致 | 人工 |

#### F1.3 布局持久化

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | 节点坐标存入 `tasks.pos_x/pos_y`，刷新后不重排 | 集成 |

#### F1.4 一键批量入队

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | 点击后图内所有节点建出 `tasks` 行，独立根为 `queued`，有前驱的节点也是 `queued`（就绪判定在派发侧，不在入队侧） | 集成 |
| AC2 | 入队请求是幂等的：重复点击不产生重复任务 | 单测 + 集成 |
| AC3 | 队列已满（`queueDepth`）时返回明确错误，已入队部分不回滚但状态一致可续 | 集成 |

#### F1.5 纯本地图

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | `tracker_provider IS NULL` 的图可手工新建节点（标题 + 需求描述），全流程跑通至 `pr_open` | 端到端 |
| AC2 | 纯本地节点的分支名用内部 key（如 `LT-1042`），符合 `repos.branch_pattern` | 单测 |
| AC3 | 纯本地节点不尝试任何回帖，且不因"回帖失败"而失败 | 集成 |

---

### F2 依赖调度

#### F2.1 就绪判定

调度从 channel FIFO 改为 DB 领单，派发前判定前驱状态。

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | `depends_on IS NULL` 的任务立即可被派发 | 单测 |
| AC2 | `depends_on_at='pr_open'` 时，前驱进入 `pr_open` 或 `merged` 后后继才可被派发 | 单测 |
| AC3 | `depends_on_at='merged'` 时，仅前驱 `merged` 后后继才可被派发 | 单测 |
| AC4 | 前驱在 `triaging`/`implementing`/`verifying` 期间，后继**一次都不被派发**（不是派发后再失败） | 集成 |
| AC5 | 同一任务不会被两个 worker 同时领走（`FOR UPDATE SKIP LOCKED`） | 集成（并发压测） |

#### F2.2 并行度

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | S1 场景下 1、4、5 可同时处于在途状态（受配额限制），2、3、6 保持 `queued` | 集成 |
| AC2 | `VerifyGates` 的 light/heavy 配额行为不变，编排不绕过它 | 单测（沿用现有测试） |
| AC3 | 一张 10 节点图从入队到全部 `pr_open`，**人工干预次数 = 0** | 端到端 |

#### F2.3 失败传播

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | 前驱 `failed` → 后继（含间接后继）转 `blocked_dep`，**不停留在 `queued`** | 单测 + 集成 |
| AC2 | 前驱 PR 被关闭而未合并 → 后继转 `blocked_dep` | 集成 |
| AC3 | 每次传播都向对应 issue 回帖说明"因前驱 X 失败而阻塞"（有平台绑定时） | 集成 |
| AC4 | 每次传播写入 `task_events`，可从事件流重放出阻塞原因 | 单测 |
| AC5 | 前驱经人工重试后成功 → 后继自动从 `blocked_dep` 回到 `queued` | 集成 |

> **为什么 AC1 这样写**：[05-roadmap §0](./05-roadmap.md) 记录的复发性缺陷是
> "配置了但没接线 / 前置失败不落痕"，最初的"卡排队"病根正是裸 `return` 绕过 `p.fail`。
> 本条是针对该病史的定向验收。

---

### F3 栈式 PR

#### F3.1 后继从前驱分支分叉

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | 后继任务的 `base_ref` 写入前驱的 `branch_name`，落库可查 | 单测 |
| AC2 | 后继 worktree 的 `HEAD~` 链包含前驱分支的 tip commit | 集成 |
| AC3 | 独立根的 `base_ref` 为 NULL，走 `Repo.BaseBranch(kind)` 原逻辑，行为与现在完全一致 | 单测（回归） |
| AC4 | `MirrorBaseRef` 与 `WorktreeManager.Create` 的签名与实现**不变** | 代码走查 |

#### F3.2 PR 指向前驱分支

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | 后继 PR 的 base == 前驱的 `branch_name`（GitHub API 断言） | 端到端 |
| AC2 | **后继 PR 的 changed files 不含前驱的改动** —— 这是栈式 PR 的核心价值 | 端到端 |
| AC3 | 独立根 PR 的 base == `repos.default_branch` | 端到端 |
| AC4 | 重试／重派时 `findOpenPR` 按 (head, base) 正确复用，不产生重复 PR | 集成 |
| AC5 | 后继与前驱无差异时报 `ErrNoCommits`，任务以可读原因失败，不开空 PR | 集成 |

#### F3.3 链长约束

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | 链长超过配置上限（默认 4）时，连线仍允许但 UI 给出明确警告 | 人工 |
| AC2 | 上限可在系统设置里配置（复用 `system_settings`，migration 0013） | 集成 |

---

### F4 合并闭环

#### F4.1 合并检测

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | PR 被合并后，对应任务在 5 分钟内转 `merged` | 端到端 |
| AC2 | `pr_open → merged` 的转移写入 `task_events` | 单测 |
| AC3 | 检测机制在 webhook 丢失的情况下仍能收敛（轮询兜底或启动时对账） | 集成 |

#### F4.2 现场回收

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | 任务 `merged` 后其 worktree 被回收，磁盘占用释放 | 集成 |
| AC2 | **仍有未合并后继依赖该分支时，不删除该分支**（只回收工作目录） | 单测 + 集成 |

> AC2 是本功能最容易出错的地方：现有 `Remove(force=true)` 的注释写"任务成功合并后才用 force"，
> 但在栈式 PR 下前驱分支还被后继 PR 当作 base，删了会让后继 PR 失效。

#### F4.3 后继链自动跟进（S4）

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | 前驱 squash 合并后，后继自动 `rebase --onto <default> <前驱分支> <后继分支>` 并 force-push | 集成 |
| AC2 | rebase 后后继 PR 的 base 是 `default_branch`，且 **changed files 仍只含后继自己的改动** | 端到端 |
| AC3 | rebase 后自动重验（**不重跑 agent**），验证结果落 `verifications` | 集成 |
| AC4 | rebase 冲突时任务转 `failed` 并保留现场，`failure_stage` 为可读的机器码 | 集成 |
| AC5 | 链式跟进按拓扑序逐级进行（2 完成后才动 3），不并发 rebase 同一条链 | 单测 |
| AC6 | 前驱以 merge commit 或 rebase merge 方式合并时同样收敛（不只支持 squash） | 集成 |

#### F4.4 前驱被改（S3）

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | 前驱分支 force-push 后，后继自动重验 | 集成 |
| AC2 | 重验触发写入 `task_events`，人能看出"为什么又验了一遍" | 单测 |
| AC3 | 重验失败进现有 `MaxFixAttempts` 修复回路，不新建机制 | 代码走查 + 集成 |

---

### F5 运行时图视图

#### F5.1 状态叠加

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | 图上每个节点显示其任务当前状态，与 `Board.vue` 的状态一致（同一数据源） | 人工 |
| AC2 | `failed` 标红、`blocked_dep` 高亮并显示阻塞原因（阻塞于哪个节点） | 人工 |
| AC3 | 点击节点跳转到 `TaskDetail`，保留返回图的路径 | 人工 |
| AC4 | 状态变化在 UI 上于 10 秒内反映（轮询或推送，不要求实时） | 人工 |

#### F5.2 只读优先

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | 只读视图在编辑能力上线之前即可独立交付并可用 | 人工 |
| AC2 | 不引入新的前端重依赖（森林布局手写 SVG 实现） | 代码走查 |

---

### F6 平台无关化

#### F6.1 身份内部化

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | `tasks.external_key` 可为 NULL，纯本地任务全流程跑通 | 集成 |
| AC2 | 迁移后既有任务的分支名、PR 链接、回帖目标全部不变（无行为回归） | 集成（迁移前后对比） |
| AC3 | `tasks_one_active_per_item` 仍能阻止同一工单产生两个活任务 | 单测 |

#### F6.2 Tracker 接口与能力位

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | Linear 迁为 `Tracker` 的第一个实现，行为无变化 | 集成（沿用现有 Linear 测试） |
| AC2 | `Caps()` 的调用点只出现在两处：建图时的能力协商、webhook 验签分派 | 代码走查（可加 lint／grep 断言） |
| AC3 | pipeline 与 stage 层**零** `if caps.X` 分支 | 代码走查 |
| AC4 | 不支持 `Relations` 的平台，"从平台导入依赖"入口不渲染 | 人工 |

#### F6.3 凭据与 webhook

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | 设置页支持按 provider 配置凭据，保存即验证连通性（沿用现有行为） | 人工 |
| AC2 | webhook 路径带 provider，验签按 provider 分派，Linear 旧路径保持兼容或有迁移方案 | 集成 |

#### F6.4 第二个平台（云效）

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | **只读探针**：能用配置的凭据拉取一个云效工作项的标题与描述 | 集成 |
| AC2 | 探针结论落文档：云效缺哪些可选能力、`Transition` 目标值如何取 | 人工 |
| AC3 | 云效绑定的图可跑通 S1 场景（写侧只需回帖，或在缺能力时降级为不回帖） | 端到端 |

> **AC1 是 F6 其余部分的前置**：不先做探针，`Tracker` 接口一定长成 Linear 的形状（06 §3.4）。

---

### F7 节点执行画像

#### F7.1 画像字段生效

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | `profile` 里每一个字段都有消费方，无字段只存不读 | 代码走查（roadmap §0 纪律） |
| AC2 | per-node `model_channel` 生效：不同节点可路由到不同通道（沿用 B2-2 机制） | 集成 |
| AC3 | per-node `verify_tier` 可覆盖自动定档 | 集成 |
| AC4 | 未设画像的节点行为与现在完全一致 | 集成（回归） |

#### F7.2 skills

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | 节点可声明技能列表，agent 执行时确实加载了这些技能（可从 agent 事件流佐证） | 集成 |
| AC2 | **技能不从执行机器继承**：`~/.claude/skills` 的内容不影响执行结果 | 集成（在有／无个人技能的两台环境上结果一致） |
| AC3 | **技能文件不进 PR**：PR 的 changed files 不含 `.claude/` 下任何路径 | 端到端 |
| AC4 | 技能声明按名字 + 版本存储，不存本机目录路径 | 代码走查 |
| AC5 | 声明了不存在的技能时，任务以可读原因失败，不静默忽略 | 集成 |

> AC2 是对 `agent.go:133` 那条既有决策的正面回应：反对加载技能的理由是**不可复现**
> （插件环境下一句话吃 32937 input tokens、结果依赖机器），本条把"可复现"变成可验证的断言。
> AC3 是对 B2-3 事故（分诊工作目录污染）的同类防御。

---

## 4. 数据契约

### 4.1 新增表

```sql
CREATE TABLE flows (
  id               bigserial PRIMARY KEY,
  user_id          bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  repo_id          bigint NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  name             text   NOT NULL,
  tracker_provider text,        -- NULL = 纯本地
  tracker_scope    jsonb,
  created_at       timestamptz NOT NULL DEFAULT now()
);
```

### 4.2 `tasks` 新增列

| 列 | 类型 | 说明 |
|---|---|---|
| `flow_id` | `bigint` | 所属图，NULL = 图外的单发任务（现有路径） |
| `depends_on` | `bigint` | 自引用；NULL = 独立根。**入度 ≤ 1 由此保证** |
| `depends_on_at` | `text` | `pr_open`（默认）\| `merged` |
| `base_ref` | `text` | NULL = 走 `Repo.BaseBranch(kind)`；否则前驱分支 |
| `pos_x` / `pos_y` | `int` | 画布布局 |
| `priority` | `int` | 排序权重（U5 已定：新列，初值从平台带入） |
| `key` | `text` | 内部身份（如 `LT-1042`） |
| `tracker_provider` | `text` | 刻意冗余（部分唯一索引不能跨表） |
| `profile` | `jsonb` | 节点执行画像（F7） |

`linear_issue_key` → 重命名 `external_key`，去掉 `NOT NULL`。

### 4.3 状态机变更

新增 `blocked_dep`：入口 `queued → blocked_dep`，出口 `blocked_dep → queued | cancelled`。
`internal/task/state.go:65` 的转移表加两条边。

**不变式不破**：任何路径仍必须经过 `verifying` 才能到 `pr_open`。

### 4.4 索引

```sql
-- 替换 tasks_one_active_per_issue
CREATE UNIQUE INDEX tasks_one_active_per_item ON tasks (tracker_provider, external_key)
  WHERE external_key IS NOT NULL AND state NOT IN ('merged','failed','cancelled');
```

---

## 5. 分期与出口条件

| 里程碑 | 内容 | 出口条件（全部满足才算完） |
|---|---|---|
| **M1 编排内核**（无 UI） | F2 全部、F1.4、`flows` 表 + `tasks` 列、`blocked_dep` | 用 API 建一张 `1→2→3 / 4 / 5→6` 的图（假 tracker + 短路 agent），全自动跑完，**人工干预 0 次**；杀掉前驱后后继在 `blocked_dep` 且有回帖痕迹 |
| **M2 栈式 PR** | F3 全部 | 真仓库上 2 的 PR base == branch1，且 **2 的 changed files 不含 1 的改动**；独立根行为无回归 |
| **M3 合并闭环** | F4 全部 | 把 1 以 **squash** 合并，2 自动 rebase 到 default、PR 自动 retarget、diff 仍只含 2 的改动、自动重验通过；前驱分支在后继未合并前不被删除 |
| **M4 只读图视图** | F5 全部 | 跑 M1 的图时，人只看图就能说出"哪个在跑、哪个卡了、卡在谁身上" |
| **M5 画布编辑** | F1.1–F1.3、F1.5 | 人在浏览器里完成 S1 全流程（选单 → 连线 → 入队），不碰 API；S2 纯本地图跑通 |
| **M6 平台无关化** | F6 全部 | 云效绑定的图跑通 S1；`grep` 断言 pipeline/stage 层零 `caps` 分支 |
| **M7 节点画像** | F7 全部 | 同一张图的两个节点路由到不同模型通道；技能声明生效且不进 PR |

**M1 之前的硬前置**：F4.1（合并检测）虽然属于 M3，但 `depends_on_at='merged'` 的语义
依赖它。M1 可以只交付 `pr_open` 语义，`merged` 语义随 M3 上线。

---

## 6. 度量（怎么知道这功能有用）

| 指标 | 基线 | 目标 |
|---|---|---|
| 一批 N 个链式单子的人工干预次数 | 每个后继至少 1 次（手工重新入队） | **0** |
| S1 场景（10 节点）从入队到全部 `pr_open` 的墙上时间 | 全串行 | 显著低于串行（并行根 + 链内流水） |
| 因栈失效导致的重验次数与 token 成本 | 无（现在没有栈） | 可观测；用于校准链长上限 |
| `blocked_dep` 停留时长分布 | 无 | 用于回答"人是不是瓶颈" |

> **前置依赖**：第 3、4 项需要成本／时长聚合视图（[roadmap §3.2-5](./05-roadmap.md)：
> `CostUSD/DurationMS/NumTurns` 已落库、缺聚合）。**建议成本面板与 M2 同期或更早交付**，
> 否则 rebase 风暴的代价不可见，链长上限只能靠猜。

---

## 7. 风险与缓解

| # | 风险 | 缓解 | 残余 |
|---|---|---|---|
| R1 | 深链 review 体验递减：看 3 要先看懂 1、2 | 链长上限（F3.3）+ UI 警告 | 产品约束，无法根除 |
| R2 | squash merge 打断栈 | F4.3 自动 rebase | rebase 冲突仍需人介入（F4.3-AC4） |
| R3 | 前驱被改 → 整链重验的 token 成本 | 只重验不重跑 agent；成本面板可见化 | 深链成本随链长线性增长 |
| R4 | 独立根之间改同一文件，互相冲突 | PR body 标注"本图内并行任务" | 继承 02-design §9 未决，本功能放大之 |
| R5 | 调度器从 channel 换 DB 领单引入回归 | `Reconcile` 与租约语义沿用 02-design §6.4；`VerifyGates` 不动 | 并发正确性需压测（F2.1-AC5） |
| R6 | 技能加载重新引入不可复现性 | F7.2-AC2 双环境一致性断言 | 需要真的在两台环境上测 |

---

## 8. 决策与未决

### 8.1 已定（2026-08-27）

| # | 议题 | 决定 | 影响 |
|---|---|---|---|
| U1 | 同一 issue 能否出现在两个未终结的图里 | **不允许** —— `tasks_one_active_per_item` 继续拦。同一 issue 想换图，必须先终结旧任务 | F1.1-AC3、F6.1-AC3 的断言按此写 |
| U3 | 前驱在 review 里被改后的重验范围 | **只重验直接后继，逐级传播** —— #1 改了先只重验 #2，#2 验通过后才重验 #3 | F4.4、F4.3-AC5 的拓扑序要求；成本可中断，#2 挂了就不必浪费 #3 的验证 |
| U5 | `priority` 从哪来 | **`tasks.priority` 新列**，建图时从平台的 priority 取初值，之后可在 Lathe 里改 | §5 领单 SQL 的 `ORDER BY`；纯本地图也有 priority 可用，不依赖平台 |

### 8.2 未决（需要拍板）

| # | 未决 | 阻塞 | 倾向 |
|---|---|---|---|
| U2 | 链长上限取值（3 / 4 / 更多），硬拒还是警告 | F3.3 | 4，仅警告 |
| U4 | 独立根之间是否做文件级冲突预检 | R4 | v1 只标注不预检 |
| U6 | 技能存 Lathe 的 git 仓库 vs DB | F7.2 | git 仓库（可 review、可版本化） |
| U7 | webhook 旧路径（Linear 专用）保持兼容还是强制迁移 | F6.3-AC2 | 保持兼容一个版本 |
| U8 | 纯本地节点的需求描述由谁写、写在哪（DB 字段 vs 附件） | F1.5 | `tasks` 新增 text 列 |

### 8.3 开工前的环境前置

`internal/store/*_test.go` 与 `internal/runner/pipeline_test.go` 在拿不到 Postgres 时
**静默 `t.Skipf`**。本 PRD 的核心改动（迁移、状态机新边、DB 领单）全在这些测试的覆盖范围里，
因此：

> **实施期间必须先 `make dev-infra && make migrate`。** 否则 `make test` 绿了不代表验证过 ——
> 这与 README「证明改动有效」的立场直接冲突。

---

## 9. 与既有边界的一致性自查

| README / 设计约束 | 本 PRD 是否破坏 | 说明 |
|---|---|---|
| 不做合并决策，产出 PR 人点合并 | **不破** | F4 只做合并**检测**与跟进，从不调用 merge API（非目标已明列） |
| 永不 push 受保护分支 | **不破** | 栈式 PR 的 push 目标始终是任务自己的分支；base 是任务分支，不在 `protected_branches` |
| 任何路径必须经 `verifying` 才到 `pr_open` | **不破** | `blocked_dep` 在 `queued` 之前，不新增通往 `pr_open` 的捷径（§4.3） |
| 每个可配置字段必须有消费方 | **不破** | F7.1-AC1 直接把它写成验收标准 |
| 不做需求澄清，不明确就回帖提问并停下 | **不破** | 节点仍各自走 `stageTriage`，`blocked_spec` 语义不变 |
