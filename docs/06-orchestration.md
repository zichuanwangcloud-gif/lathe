# Lathe — 06 · 并行开发编排图

> 2026-08-27 · 状态：待评审
> 前置：[02-design.md](./02-design.md)（状态机、数据模型、调度）、[05-roadmap.md](./05-roadmap.md)（欠账盘点）
> 本文每条结论都标注代码证据（`文件:行号`）或继承的未决项，不接受"感觉应该做"。

---

## 0. 形态确认

需求：一批 issue（例如 10 个）之间存在链路关系，希望在 Lathe 网页端画出来，
平台按链路自动编排执行；未来平台可能不是 Linear（云效等），不能依赖平台自己的依赖表达。

**典型形态（用户原话的例子）：**

```
1 → 2 → 3        链一
4                独立
5 → 6            链二
```

- 独立根（1、4、5）各自向 `default_branch` 提 PR
- 链上的后继（2、3、6）向**前驱的分支**提 PR（栈式 PR）

**与其说是链路图，不如说是并行开发的编排图** —— 这个定位比"任务依赖 DAG"更准，
它决定了本文后面所有的取舍。

### 0.1 价值在哪（比我上一版的说法更实在）

现在 `cmd/lathe/queue.go` 是 `make(chan job, 64)` + 3 个 worker 抢的纯 FIFO，
**零依赖感知**。所以今天要跑上面这 10 个单子，人必须手工扣住 2、3、6，
等前驱开出 PR 再挨个重新入队 —— 而且得自己记住谁等谁。

编排图要消灭的就是这件事：**人一次把 10 个单子全丢进去，平台自己知道哪些现在能跑、
哪些要等谁。** 4 和 5 不必等 1。

这个价值不依赖于"解决人 review 瓶颈"（上一版我押在那上面，是个更难兑现的赌注）。

---

## 1. 形态是森林，不是一般 DAG —— 这个约束是最大的简化

用户的例子里每个节点**最多一个前驱**。这不是巧合，也不该被"泛化"成一般 DAG：

> **git 的 base 是单值的。** 一个节点有两个前驱，就没有单一的 base 分支可用 ——
> 你必须先把两个前驱合到一条集成分支上，才有地方分叉。

所以"每个节点入度 ≤ 1"不是偷懒，是**与分支拓扑同构**。一般 DAG 需要一个额外机制
（显式的合流节点 + 集成分支），而那个机制在当前需求里没有场景。

**v1 决策：保存图时校验入度 ≤ 1，超了直接拒绝并提示。**
将来真要表达"依赖 2 和 5"，正确做法是加一个显式的 `merge` 节点（它建集成分支），
让这件事在图上看得见 —— 而不是让 base 变成多值。

---

## 2. v1 数据模型：一张表 + 五列，不是五张表

### 2.1 撤回「定义／实例分离」

我上一版说"定义与实例必须分离，这是最容易做错的地方"（`flow_defs` / `flow_def_nodes` /
`flow_def_edges` / `flow_runs` / `flow_run_nodes` 五张表）。**在本文确认的形态下这是过度设计。**

理由：本形态里**节点就是具体的 issue**。一张由具体 issue 组成的图**天生是一次性的** ——
它不可能被"再跑一遍"，也不可能作为模板复用（模板要求节点是参数化的角色，不是具体单据）。
于是分离所要解决的两个问题都不存在：

| 分离要解决的问题 | 在本形态下 |
|---|---|
| 跑一半改图污染在途实例 | 只有一个实例；改图就是改这次执行本身，正是人要的 |
| 模板复用 | 节点是具体 issue，无法参数化，模板不适用 |

模板只对"对这 8 个包各来一遍"那类**可重复**流程有意义，那是另一个用例（§9 未决）。

### 2.2 实际需要的

森林结构用**一个自引用外键**就能完整表达：

```sql
-- 分组：让这 10 个任务在 UI 上是一张图，而不是看板里散落的 10 行
CREATE TABLE flows (
  id               bigserial PRIMARY KEY,
  user_id          bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  repo_id          bigint NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
  name             text   NOT NULL,
  tracker_provider text,                    -- §3.1：建图时选定，NULL = 纯本地
  tracker_scope    jsonb,                   -- Linear team / 云效项目空间
  created_at       timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE tasks
  ADD COLUMN flow_id        bigint REFERENCES flows(id) ON DELETE SET NULL,
  ADD COLUMN depends_on     bigint REFERENCES tasks(id),  -- 自引用；NULL = 独立根
  ADD COLUMN depends_on_at  text NOT NULL DEFAULT 'pr_open', -- 就绪条件，见 §5.1
  ADD COLUMN base_ref       text,           -- NULL = 用 repos 配置；否则前驱分支
  ADD COLUMN pos_x          int,            -- 画布布局：人的意图，不自动重排
  ADD COLUMN pos_y          int;
```

**1 张表 + 6 列**，替代原来的 5 张表。

- `depends_on` 是自引用而非边表 —— 因为入度 ≤ 1（§1），边表在这里只会引入非法状态。
- `base_ref` 冗余存一份而不是每次从 `depends_on` 现推：前驱分支名在前驱被丢弃重建
  （`Create` 的尸体回收）后可能变，而后继的 base 必须是它**当初分叉的那个** ——
  存下来才能诊断"我的地基是谁"。
- `pos_x/pos_y` 存下来：自动布局每次重排会让人失去空间记忆（人记得"卡住那个在右下角"）。

**升级路径不堵**：`flows` 将来可升为 `flow_runs`；真需要模板时再加 def 表；
真需要合流时把 `depends_on` 换成边表 + `merge` 节点类型。都不是 v1 的事。

### 2.3 撤回「节点类型」

上一版设计了 `agent` / `gate` / `await_merge` / `script` / `fanout` 五种节点类型。
本形态下**每个节点都是一个 issue，也就都是 agent 节点** —— 类型系统 v1 不需要。

其中 `await_merge` 尤其要撤回：它是我给"第一个 PR 没合怎么办"的答案，
而用户的答案是"向上一个链路提 pr"（栈式），**从根上绕开了这个问题**。
"等前驱真合并"退化成 `depends_on_at = 'merged'` 一个取值（§5.1），不是一种节点。

---

## 3. 平台无关化

### 3.1 一个 flow 只绑一个平台

Linear 与云效都是**需求记录工具**，是互斥的替代品。**平台在新建 flow 时选定，绑在 flow 上。**

不存在"一个节点是 Linear 工单、另一个是云效工作项"的场景 —— 那不只是没意义，而是**有害**：
它会迫使每个调用点（拉单、回帖、状态同步、幂等去重）都做 provider 分支，
把平台差异从一处扩散到全身。

`tracker_provider` 可为 NULL，即**纯本地 flow**（手绘、不挂需求工具）。
这是"不能完全靠 Linear"的极端情形，是一等公民而非降级路径。

### 3.2 身份解耦：`linear_issue_key` 现在是系统的身份证

它不只是外部引用，被当作**主身份**在用：

| 占用点 | 证据 |
|---|---|
| `tasks.linear_issue_key text NOT NULL` | `migrations/0001_init.up.sql:94` |
| `tasks_one_active_per_issue` 部分唯一索引 | `0001_init.up.sql` 尾部 |
| 分支名 `{kind}/{key}-{slug}` 的 `{key}` | `0002_branch_pattern_kind.up.sql:7`、`internal/runner/branch.go` |
| 回帖目标（进度／提问／PR 链接） | `internal/runner/pipeline.go` `p.fail` / `stagePushAndPR` |
| PR body 的 issue 链接 | `internal/integration/github/github.go:229` `BuildPRBody` |
| webhook 幂等去重 | `webhook_deliveries`、`internal/httpapi/webhook.go` |
| 二级 ID | `migrations/0010_*.up.sql` 的 `linear_issue_id`（UUID） |

改造（注意：外部引用是**单一**的，不是一对多 —— 上一版设计的
`task_external_refs(task_id, provider)` 一对多表是为一个不存在的场景付账，已撤回）：

```sql
ALTER TABLE tasks ADD COLUMN key text;                     -- 内部身份，如 LT-1042
ALTER TABLE tasks RENAME COLUMN linear_issue_key TO external_key;
ALTER TABLE tasks ALTER COLUMN external_key DROP NOT NULL;
ALTER TABLE tasks ADD COLUMN tracker_provider text;        -- 刻意冗余，见下
```

`tasks.tracker_provider` 是**刻意的冗余**：值来自所属 flow、创建时冻结、永不变更。
唯一理由是 Postgres 部分唯一索引不能跨表 —— 不冗余这列，"同一工单不许两个活任务"
就得退化成应用层加锁，那是主动拆掉一条已在生效的数据库级防线。

```sql
-- 外部侧：防重复接单
CREATE UNIQUE INDEX tasks_one_active_per_item ON tasks (tracker_provider, external_key)
  WHERE external_key IS NOT NULL AND state NOT IN ('merged','failed','cancelled');
```

`{key}` 语义改为"有外部单据用外部 key，否则用内部 key" —— 分支名保持人可读，这是它存在的唯一理由。

### 3.3 Tracker 接口：能力在建 flow 时解析一次

接口不能按 Linear 的形状定（云效接进来全是漏洞），也不能取所有平台的交集
（退化成只剩"拉个标题"，现有的回帖、提问、状态联动全废）。用**可选能力 + 能力位**：

```go
type Tracker interface {
    Kind() string
    Fetch(ctx context.Context, key string) (Issue, error)   // 必需
    Caps() Caps
    Comment(ctx context.Context, key, body string) error    // 按 Caps 判定后调用
    Relations(ctx context.Context, key string) ([]Relation, error)
    Transition(ctx context.Context, key, state string) error
}
type Caps struct { Comment, Relations, Webhook, Transition bool }
```

因为平台在建 flow 时就定了，**能力集在那一刻已知且不变**：

| | 若允许节点级混搭（错） | 实际（flow 级绑定） |
|---|---|---|
| 能力检查位置 | 每个调用点 | **建 flow 时一次** |
| 不支持时的表现 | 跑到一半才撞墙 | UI 上那功能直接不渲染 |
| 平台差异扩散范围 | 全身 | webhook ingress 的验签 |

> **纪律：`Caps()` 的合法调用位置只有两处** —— 建 flow 时的能力协商、webhook ingress 的
> 验签分派。pipeline 或 stage 里出现任何 `if caps.X` 都是设计退化的信号。
> （同 [05-roadmap §0](./05-roadmap.md)「每个可配置字段必须有消费方」，属同类可自查纪律。）

**编排功能不得依赖任何可选能力。** `Relations` 只是"从平台导入图"的一个可选来源（§7）。

### 3.4 抽象必须先被实证

**行动项，排在编辑器之前：做一个云效只读探针 —— 能拉下一个工作项就算成功。**

可预判的差异轴（需探针确认）：工作项**类型体系可配置**（Linear 的 issue 语义固定）；
状态流转由项目模板定义，`Transition` 的目标值不可硬编码；评论富文本格式；
webhook 验签与投递语义（`internal/integration/linear/linear.go:408` 那套 `updatedFrom`
判定是 Linear 特有的）；是否有依赖关系表达（很可能没有，正好验证上面那条纪律）。

不做探针就定接口是闭门造车，抽象错了上层白做。

### 3.5 连带改造

- **凭据**：设置页的单一「Linear API 令牌」变成**按 provider 的槽位**。建 flow 时
  **只能选已配置且验证通过的平台** —— 现有"保存即验证连通性"的行为正好是这个下拉框的
  数据源，不需要新机制。
- **webhook ingress**：路径带 provider（`/api/webhook/{provider}/{userSlug}`），
  验签按 provider 分派。这是唯一必须做 provider 分支的地方，因为它在**边界上**；
  边界之内不该再有分支。
- **`LinearIssues.vue` 要改名**（`web/src/views/`）—— 视图名里带平台名，是耦合的征兆。

---

## 4. 堆叠机制：比预估便宜得多

上一版我估计"从另一个任务的分支分叉"需要放宽 `MirrorBaseRef` 的假设。**这个估计是错的。**

### 4.1 为什么便宜：前驱分支已经是 origin ref

`EnsureMirror`（`internal/runner/worktree.go:89`）**每次调用都跑**
`git fetch --prune --quiet origin`（:121），不只在首次 clone。

而 `stagePushAndPR`（`pipeline.go:744`）在开 PR 之前必然先把前驱分支 push 到 origin。
所以当后继任务建 worktree 时，`refs/remotes/origin/<前驱分支>` **已经在 mirror 的 ref 空间里**。

结论：`Create`（`worktree.go:351`）的
`git worktree add -b <branch> <path> refs/remotes/origin/<base>` 和 `MirrorBaseRef`
**一行都不用改** —— 只要把传进去的 `base` 从"repo 配置值"换成"前驱分支名"。

### 4.2 PR 侧同样零改动

`PRParams.Base`（`internal/integration/github/github.go:63`）本来就是参数，
`CreatePR` 完全 base-agnostic：

- `Head == Base` 有校验，`ErrNoCommits`（head 与 base 无差异）有专门错误
- `findOpenPR` 的幂等按 **(head, base) 配对** —— 栈式 PR 下每一对仍然正确
- `repos.protected_branches` 默认 `{dev,test,main}`，任务分支作 base 不受
  `ValidatePushTarget` 影响（push 目标仍是任务自己的分支）

### 4.3 于是实际改动清单

| 改动 | 位置 |
|---|---|
| base 来源：`tasks.base_ref` 非空则用它，否则走 `Repo.BaseBranch(kind)` | `internal/runner/branch.go:63` 的调用方 |
| 建 worktree 时传入该 base | `worktree.go` `Create` 的调用方（不改 `Create` 本身） |
| 开 PR 时传入该 base | `pipeline.go` `stagePushAndPR` → `PRParams.Base` |
| 就绪门 | 调度器（§5） |

**没有一处是在改既有抽象，全是在给已有参数喂不同的值。**

### 4.4 但 squash merge 会打断栈 —— 这是真成本所在

1 的 PR 若以 **squash** 合入 `default_branch`，`branch1` 上的原始 commit 不是
`default_branch` 的祖先。GitHub 会把 PR2 自动 retarget 到 `default_branch`，
但 PR2 的 diff 会重新显示 1 的改动（或直接冲突）。

**必需能力：前驱合并后，自动把后继链 `git rebase --onto default_branch branch1 branch2`，
然后 force-push、重验。**

这让 P0（PR 合并检测）从"编排的前置"升级为"**栈式 PR 的必需品**"：
没有它，每次合并都要人手工救栈。

---

## 5. 调度：就绪集

上一版已定：channel FIFO 必须换成 DB 领单 —— channel 无法 peek、无法条件出队，
装不进"就绪集"。且 `0001_init.up.sql` 的 `lease_expires_at`（注释："租约到期即重新派发"）
与 `tasks(state, created_at)` 索引说明原设计（02-design §6.4）本来就走这条路，
只是单机形态用重启 `Reconcile` 顶替了。

森林结构让领单 SQL 极简 —— **不需要对边表做 `NOT EXISTS`**：

```sql
SELECT t.* FROM tasks t
LEFT JOIN tasks p ON p.id = t.depends_on
WHERE t.state = 'queued'
  AND ( t.depends_on IS NULL                                    -- 独立根
     OR (t.depends_on_at = 'pr_open' AND p.state IN ('pr_open','merged'))
     OR (t.depends_on_at = 'merged'  AND p.state = 'merged') )
ORDER BY t.priority DESC NULLS LAST, t.id
FOR UPDATE OF t SKIP LOCKED
LIMIT 1;
```

**沿用不动**：`internal/runner/gate.go` 的 `VerifyGates`。它已经把"稀缺资源限流"
从"派发并发"里解耦了（light/heavy 独立配额在验证阶段排队，因为档位要等 diff 才可判定）。
新调度器不需要重新设计资源模型。

### 5.1 `depends_on_at` 的两个取值

| 值 | 语义 | 代价 | 适用 |
|---|---|---|---|
| `pr_open`（默认） | 前驱开出 PR 即放行，向前驱分支提 PR | 栈式 PR，前驱被改则整链重验 | 用户描述的形态；最大并行 |
| `merged` | 前驱真合并才放行，向 `default_branch` 提 PR | 零 rebase 风暴 | 前驱改动风险大、不想返工时 |

默认 `pr_open`（用户的形态），但**要让人能按边改** —— 这是上一版 `await_merge`
节点类型的正确形态：一个取值，不是一种节点。

### 5.2 失败传播

前驱 `failed`、或前驱 PR 被**关闭而未合并**，后继的地基就没了。
必须显式转 `blocked_dep` 并回帖，不能静默卡着 ——
[05-roadmap §0](./05-roadmap.md)「配置了但没接线」的病在这里最容易复发。

状态机（`internal/task/state.go:65` 的转移表）加 `blocked_dep`：
入口来自 `queued`，出口 → `queued`（前驱恢复）/ `cancelled`。
`blocked_spec` 已是现成先例，`Board.vue` 的 filter 与 `api.js:147` 的 label 照抄。

---

## 6. 节点执行画像与 skills

### 6.1 driver 侧已经就绪

`internal/integration/agent/agent.go:112` 的 `RunParams` 已经是画像的形状：
`Prompt` / `PermissionMode` / `SettingSources` / `ExtraEnv` / `ExtraArgs`。
现在这些值由 `pipeline.go` 按阶段硬编码，**画像化 = 把它们提到节点配置里**，driver 不用改。

所以不要设计一个 `skills` 字段，设计 `tasks.profile jsonb`：

```jsonc
{
  "model_channel": "...",   // LATHE_TRIAGE/IMPLEMENT_CHANNEL 的推广（B2-2 已有机制）
  "skills": ["go-testing", "sql-migration"],
  "verify_tier": "heavy",   // 覆盖 §5.1 的自动定档
  "gate_mode": "guarded",
  "max_fix_attempts": 3,    // 已有：stageVerify 的修复回路
  "prompt_template": "...",
  "extra_args": []
}
```

每个字段必须有消费方 —— jsonb 让加字段变成零成本，这条纪律在这里最容易被绕过。

### 6.2 skills 的三个难点

**(a) 与既有决策正面冲突，但可以顺着它的理由解决。**

`agent.go:133` 明确写了刻意把 `SettingSources` 收敛为 `"project"`：

> 只加载目标仓库自己的配置与 CLAUDE.md，把执行者个人环境里的插件与技能定义排除在外。
> 实测一句「回答两个字」在装了插件的环境要吃掉 32937 input tokens（02-design §9）——
> 这笔基线会乘以任务数，且让任务执行结果依赖某台机器装了什么插件，**不可复现**。

per-node skills 是在往回加载技能。但反对理由是**不可复现**，不是"技能没用"。所以：

> **技能由 Lathe 声明并版本化，不从执行机器继承。** 存 Lathe 侧，执行前物化到受控目录，
> 仍然不读 `~/.claude/skills`。

这样"同一个 flow 在任何执行节点上跑，技能集完全一致"成立 ——
可复现性反而比现在更强（现在 `project` 档依赖目标仓库里恰好有什么）。

**(b) 别污染 worktree。**
技能物化进 worktree 的 `.claude/skills/` 会被 agent 的 commit 带进 PR。这和已踩过的
B2-3 事故同类（triage 未设 `Dir` → 继承 `cwd=/opt/lathe` → 把 Lathe 自己的 CLAUDE.md
灌进目标仓库的分诊上下文）。倾向物化到 **worktree 之外**，`--setting-sources` 指向它。
退路是写进 worktree 但 `stageCommit`（`pipeline.go:527`）显式排除 ——
但"靠 commit 阶段记得排除"是脆的，同类 bug 会复发。

**(c) 可移植性陷阱，和本文主题同构。**
skills 是 Claude Code 特有概念。字段若存目录路径，将来换 agent（codex / gemini）就废了
—— **这就是平台耦合问题换了一层**。存"能力引用"（名字 + 版本），由 driver 层翻译成具体
CLI 的加载方式。`agent.Driver` 已在正确的抽象位置上。

---

## 7. UI：画布是 issue 列表的上层，不是通用图编辑器

本形态下 UI 的定位比上一版清楚得多：

**已有 `LinearIssues.vue` 是 issue 列表 → 画布就是它的上层**：拉一批 issue → 拖到画布 →
连线（每个节点最多一条入边）→ 一键全部入队。

### 7.1 撤回「画一次就不想画第二次」的风险评级

上一版我把手绘列为最大产品风险。**在本形态下这个风险大幅下降**，因为：

- 人不是在**创作内容**，只是在 10 张已存在的卡片之间画 5 条线
- 替代方案（手工扣住 2、3、6，等前驱开 PR 再挨个重新入队，还得自己记住谁等谁）明显更痛
- 一次性的图不需要复用，也就没有"第二次"的问题

模板与 AI 拆解仍然有价值，但降级为增强项而非必需品（§9 未决）。

### 7.2 运行时视图仍然优先于编辑器

节点状态叠在图上（失败标红、卡在 `blocked_dep` 的高亮、点节点进 TaskDetail）
的日常价值高于编辑能力，而且它就是编辑器的渲染层。
`Board.vue`（看板 + 状态统计）与 `TaskDetail.vue`（日志面板、agent 事件流）是它的下层。

**先做只读视图，再做交互。** 反过来会有一段"能画但看不出在跑什么"的尴尬期。

### 7.3 技术选型

现有 web 依赖极简：`vue`、`vue-router`、`marked`、`dompurify`（`web/package.json`）。

| 方案 | 适用 | 判断 |
|---|---|---|
| 手写 SVG | 只读视图；森林布局（分层 + 贝塞尔连线）非常简单 | **森林形态下手写完全够**，不引依赖 |
| `@vue-flow/core` | 拖拽连线、缩放、选中 | 只在编辑器体验不够时再引 |

森林比一般 DAG 好画得多（不用处理交叉边路由）—— 这是 §1 约束的又一处红利。
两者都是构建时打包，`go:embed` 单二进制形态不受影响（`docs/03-tech-stack.md` 的核心约束不破）。

---

## 8. 风险

1. **深链的 review 体验递减。** 3 的 reviewer 面对的 base 是 `branch2`，
   要看懂 3 得先看懂 1 和 2。**建议链长上限 3–4**，UI 上超了就提示人拆成独立单子。
   这是产品约束，不是技术限制 —— 技术上可以无限深，但没人 review 得动。
2. **前驱在 review 里被大改 → 整链的验证地基变了。** 必须重验（不重跑 agent）。
   好消息：`stageVerify`（`pipeline.go:567`）的 `MaxFixAttempts` 回路和
   02-design §6.5 的"直接重验"决策路径都已存在，复用即可，成本低。
3. **squash merge 打断栈**（§4.4）—— 必须有"前驱合并后自动 rebase 后继链"。
4. **独立根之间的并发冲突被放大。** 本形态天然鼓励 1、4、5 三根同时跑同一仓库；
   改到同一文件就互相冲突。继承 02-design §9 / roadmap §5 的未决项，
   但编排让它从"偶发"变成"常态"。最低限度：PR body 里标注"本图内并行任务：#4 #5"，
   让 reviewer 知道有并发改动。
5. **`tasks_one_active_per_issue` 的语义变化要想清楚**（§3.2）——
   同一 issue 在两个不同 flow 里各建一个任务，应该被挡还是被允许？倾向仍然挡。

---

## 9. 对前几轮的撤回与升级

| 结论 | 处置 | 理由 |
|---|---|---|
| 编排图从 Linear 读，别自己造 | **撤回** | Linear 降级为可选导入源之一（§3.3） |
| DAG 节点用 Linear sub-issue 建模以绕开唯一索引 | **撤回** | 节点身份必须内部化（§3.2） |
| `task_external_refs(task_id, provider)` 一对多 | **撤回** | 平台是互斥替代品，绑在 flow 上（§3.1） |
| 定义／实例分离（5 张表） | **撤回** | 节点是具体 issue，图天生一次性，模板不适用（§2.1） |
| 节点类型 `agent`/`gate`/`await_merge`/`script`/`fanout` | **撤回** | 每个节点都是 issue；"等合并"退化为 `depends_on_at` 取值（§2.3、§5.1） |
| 一般 DAG + 边表 + `NOT EXISTS` 就绪查询 | **收窄** | 森林 + 自引用外键 + LEFT JOIN（§1、§5） |
| 「`MirrorBaseRef` 的假设要放宽」 | **撤回，估错了** | `EnsureMirror` 每次 fetch，前驱分支已是 origin ref（§4.1） |
| 「手绘一次就不想画第二次」是最大风险 | **降级** | 只是在已有卡片间连线，且替代方案更痛（§7.1） |
| `task_deps.kind ∈ order/exclusive/code` | **收窄** | `order` → `depends_on`；`code` → `base_ref`；`exclusive` 归入风险 4 |

**保持不变**：PR 合并闭环是硬前置（`merged`/`review_feedback` 现为死状态，
`internal/task/` 之外零引用；`github.go` 无任何 merge 检测）；DB 领单调度器；
`VerifyGates` 沿用不动。

---

## 10. TODO 分批

| 批次 | 内容 | 为什么排这个位置 |
|---|---|---|
| **P0** | PR 合并闭环：GitHub webhook → `pr_open → merged`、worktree 回收 | 栈式 PR 的必需品（§4.4，前驱合并要触发后继 rebase），且本来就是欠账（roadmap §3.4-12b），零件已在盒子里只缺接线 |
| **P1** | DB 领单调度器 + `blocked_dep` 状态 + `flows` 表 + `tasks` 六列 | 编排内核。森林让就绪查询极简（§5） |
| **P2** | 堆叠：`base_ref` 喂给 worktree 与 `PRParams.Base` | 改动极小（§4.3），P1 之后立即可用 |
| **P3** | 后继链自动 rebase（前驱合并／被改时）+ 重验 | 让 P2 在 squash merge 下不崩（§4.4） |
| **P4** | 只读图视图（手写 SVG，叠实时状态） | 独立可交付，且是编辑器的渲染层（§7.2） |
| **P5** | 画布编辑：连线 + 一键入队（`LinearIssues.vue` 上层） | 有只读视图再补交互（§7） |
| **P6** | 云效**只读探针** | 验证抽象形状；可与 P0–P5 并行（§3.4） |
| **P7** | 身份解耦：`tasks.key` + `external_key` + `tracker_provider` + 索引改造 + 凭据槽位 + webhook 带 provider | 去平台化地基（§3.2、§3.5） |
| **P8** | `Tracker` 接口 + 能力位，Linear 迁为第一个实现 | 有 P6 实证再定形状（§3.3） |
| **P9** | 节点执行画像（含 skills，物化在 worktree 之外） | 消费点在 pipeline，图能跑起来才有意义（§6） |

**排序变化说明**：上一版把去平台化（原 P1–P3）排在编排前面。本版把它移到 P6–P8，
因为**编排价值可以在单一平台上先兑现**，而多平台是"未来可能"。
但 P6（只读探针）要早做 —— 它便宜，且它的结论会影响 P7 的表结构。

---

## 11. 未决（需要拍板）

- [ ] **同一 issue 能否出现在两个 flow 里**（§8.5）—— 影响 `tasks_one_active_per_item` 的定义
- [ ] **链长上限**：3？4？硬拒还是只警告（§8.1）
- [ ] **前驱被改后的重验粒度**：整链重验，还是只重验直接后继（成本差异是链长倍数）
- [ ] **独立根之间的冲突预检**：做文件级预检、只在 PR body 标注、还是不管（§8.4）
- [ ] **priority 从哪来**：`tasks.priority` 新列，还是沿用平台的 priority
      （Linear 的已在拉：`internal/integration/linear/linear.go:93`）？
      拓扑序之内如何用它排（§5 的 `ORDER BY`）
- [ ] **模板与 AI 拆解**（§7.1 降级为增强项）：还做不做？AI 拆解有现成先例
      （02-design §10 的"AI 推荐 + 人确认"，commit `41334f0`/`9834bae`），
      且 `stageTriage`（`pipeline.go:329`）本来就在做只读拆解
- [ ] **skills 存哪**：Lathe 自己的 git 仓库（可 review、可版本化，倾向）vs DB（部署简单）
- [ ] **flow 能否跨仓库**：`resolveRepo`（`cmd/lathe/queue.go:353`）现在是"每用户第一个仓库"
- [ ] **`cmd/lathe-runner` 骨架**（继承 roadmap §5）：并行编排后多节点执行的价值上升，是否重启 P3？
