# Lathe — 08 · 欠账清理（v0.1.1 批次）

> 2026-09-09 · 状态：执行中
> 前置：[05-roadmap.md](./05-roadmap.md)（欠账盘点）、[02-design.md](./02-design.md)（§8 隔离设计）
>
> **本文是这一轮的工作队列与验收契约。** 每条都标注验收方式，
> 不可验证的条目不算标准。loop 的每一轮从本文读状态、回写状态。

---

## 0. 这一轮要治的病

05-roadmap §0 立过一条纪律：**每个可配置字段必须有消费方**。
本轮开工前对代码做了接线核查，结论是**这病没好**——

| 欠账 | 核查证据 | 后果 |
|---|---|---|
| `gate_mode` 零消费 | 9 处引用全在 `httpapi/api.go`、`task/machine.go`；`runner/pipeline.go` 一处都没有。`awaiting_approval` 无写入点 | 「开 PR 前让我看一眼」配了不生效 |
| `notify_email` 零消费 | 3 处全在 `store`/`auth.go` 回显；`internal/mail` 仍只接密码重置 | 任务终态不发信，必须人肉盯面板 |
| `verifications.log_ref` 从不写 | 全仓仅 1 处（定义本身） | 排障无日志指针（#466 老问题） |
| 成本已落库无消费方 | `CostUSD` 16 处已解析落库，`store.Stats` 只做 state 计数 + 成功率 | 「什么单子值得交给 agent」无决策数据 |
| worktree 无 TTL 回收 | 无 reaper 代码；`workspaces/` 躺着 9 个 `cr-*` 遗留目录 | 磁盘只增不减 |
| `cmd/lathe-runner` 死代码 | 54 行骨架，两个 TODO 未装配，却仍被 `make build` 编成 3.2MB 二进制 | 误读为可用能力 |
| webhook 联动浅 | 只有 ingress，无标签接单、无取消联动 | 触发仍偏手动 |
| 验证无隔离 | 红绿仍在 worktree 里直接跑（02-design §8 P1 欠着） | 并发任务互相污染，脏环境影响验证结论 |

一句话概括这一轮：**闭环跑完之后的事没接上——不通知、不算钱、不回收、不给人闸门，
而且验证本身没有隔离。**

## 1. 两个分叉点的决策（本轮已拍板）

### D8-1 `cmd/lathe-runner`：删除

roadmap §5 挂着「推进 P3 还是删除」未决，07-prd §1.4 又把「多节点分布式执行」
列为明确非目标。两者僵持导致骨架一直躺着并被真实编译。

**决策：删除。** 理由：

- 当前无多机场景，产品交付形态是「单个静态二进制，新增节点 = 拷一个文件」，
  控制面自带执行能力，节点代理不是必需路径
- 它违反 §0 纪律的加强版——不只是「字段无消费方」，是**整个二进制无消费方**
- git 历史保留全部内容，真有多机需求时 `git show` 就能捞回来，
  留着一个永远不装配的骨架只会让人误判能力边界

**同时删除**：`Makefile` 的 `RUNNER_BIN` 目标、`config.NodeName` 若无其他消费方则一并清理
（需核查，NodeName 可能被控制面日志用着——**有消费方就留**）。

### D8-2 per-task compose 隔离：本轮做

02-design §8 P1 欠账。红绿验证目前直接在 worktree 里跑，
并发任务共享宿主环境，脏状态会影响验证结论——而**验证结论是这个产品的全部价值**，
它不可信，产品主张就不成立。因此本轮补上，不再推后。

关键设计约束：`internal/preview/` 已有 4.2k 行 docker/compose 能力
（候选扫描、compose up/down、端口随机化、label 打标清理、附加基础设施注入）。
**优先复用而非新写**——新写一套等于制造第二个 docker 编排实现，
两份实现的清理逻辑一旦不一致就会漏容器。

## 2. 工作队列

状态标记：`TODO` / `WIP` / `DONE` / `BLOCKED`

| # | 项 | PR | 状态 |
|---|---|---|---|
| T0 | v0.1.0 发布准备（staged 文件入库） | A | **DONE** |
| T1 | 删除 `cmd/lathe-runner` + roadmap §5 记决策 | A | **DONE** |
| **T9** | **测试基线可信化（前置，见 §7）** | **A** | **DONE** |
| T2 | `gate_mode` 接线：`awaiting_approval` 可达 + 确认端点 + 前端按钮 | B | **DONE** |
| T3 | 任务终态邮件通知（接 `notify_email`） | C | **DONE** |
| T4 | `verifications.log_ref` 落盘写入 | C | **DONE** |
| T5 | 成本聚合面板（store 聚合 + API + 前端） | D | **DONE** |
| T6 | worktree TTL 收割机 | E | **DONE** |
| T7 | webhook 联动：标签接单 + issue 取消联动 | F | **DONE** |
| T8 | per-task compose 隔离（验证阶段） | G | TODO |

## 3. PR 拆分与理由

单 PR 装 9 项会变成不可评审的巨块，且任一项翻车会连坐其余。
按「同类欠账 + 同一模块」聚合成 7 个 PR：

| PR | 内容 | 聚合理由 |
|---|---|---|
| **A** `chore/debt-cleanup-baseline` | T0 发布准备 + T1 删骨架 + T9 测试基线 | 都是清理类，不碰运行逻辑，可最先合；且后面每个 PR 都需要它的可信测试基线 |
| **B** `feat/gate-mode` | T2 闸门 | 动状态机（新增可达状态），独立评审 |
| **C** `feat/notify-and-logref` | T3 通知 + T4 日志指针 | 都是「终态时补一个副作用」，共享 pipeline 终态落点 |
| **D** `feat/cost-panel` | T5 成本面板 | 纯读侧聚合 + 前端，零运行风险 |
| **E** `feat/worktree-reaper` | T6 收割机 | 独立后台轮询器，仿 mergepoll |
| **F** `feat/webhook-triggers` | T7 webhook 联动 | 只动 ingress 层 |
| **G** `feat/verify-isolation` | T8 compose 隔离 | 动验证主路径，风险最高，**最后合** |

顺序：A → B → C → D/E/F（互不依赖）→ G。

**2026-09-09 修正**：原先写的「B/C/D/E/F 互不依赖，可并行推」是错的。
**C 依赖 B**：T3-AC1 把 `awaiting_approval` 列为通知触发状态之一，
而那个状态只有 T2 才让它可达 —— 我在 baseline 上开 C 的分支之后才发现
`gateBeforePush` 根本不在场。C 已改栈到 B 之上。

D/E/F 与 B/C 确实互不依赖（成本面板是读侧、收割机是独立轮询器、
webhook 联动只动 ingress 层），但都 base 在 A 上，因为都需要那个测试基线。
教训：拆 PR 时「模块不重叠」不等于「无依赖」，还得看**验收标准之间**有没有引用。

## 4. 验收标准

每条都必须可机器验证。「看起来对了」不算。

### T1 删除 lathe-runner
- AC1 `cmd/lathe-runner/` 目录不存在，`grep -rn lathe-runner Makefile` 无匹配
- AC2 `make build` 只产出 `bin/lathe`，`go build ./...` 与 `go vet ./...` 全绿
- AC3 `docs/05-roadmap.md` §5 该条从「未决」改为记录 D8-1 决策，不再列为待定
- AC4 `config.NodeName` 若被删，则全仓无残留引用；若保留，则本文记下它的实际消费方

### T2 gate_mode 接线

**开工前探查修正了三处认知错误，验收标准据此重写：**

1. `gate_mode` 不是 auto/manual 二元，**CHECK 约束允许四个值**：
   `direct` / `guarded` / `plan-first` / `manual`。默认值是 **`direct`**，不是 `auto`。
   本轮只赋予 `manual` 语义，其余三个**显式按 `direct` 处理并在代码里写明**
   —— 不能假装实现了 `guarded`/`plan-first`，那又是一次「配了没接线」。
2. `tasks.gate_mode` 列**早就存在**且 `Task.GateMode` 已被 SELECT 出来，
   pipeline 里 `rc.tk.GateMode` 零改动可读。真正的断点是
   `cmd/lathe/queue.go` 的 `Enqueue` 建任务时**没把 `repos.gate_mode` 复制进
   `CreateParams`**，所以该列永远是 `direct`。这才是要接的那一刀。
3. 数据库 `tasks_state_check` **已允许 `awaiting_approval`**（migration 0001），
   无需新迁移。缺的是 Go 侧转移表的边：`StateVerifying` 的出边里没有它，
   `StateAwaitingApproval` 的入边只有 `triaging`，且全仓引用**只在测试里**。

- AC1 `gate_mode = 'manual'` 的仓库，验证通过后任务停在 `awaiting_approval`，
  **不推分支、不开 PR**（单测：断言 GitHub client 的 Push/CreatePR 未被调用）
- AC2 `direct`/`guarded`/`plan-first` 行为与现状逐字节一致；回归测试不得有一条因本项而变
- AC3 存在确认端点，调用后任务继续走到 `pr_open`。**复用现成的 `EntryPush`**
  （`runner/retry.go:59`，"验证已过，只补 push + 开 PR，均幂等"），不新写推送路径。
  注意 `q.planRetry` 按 `failure_stage` 决策，approve 场景该列为空会落到 default
  分支，需显式处理才能拿到 `EntryPush`
- AC4 `verifying → awaiting_approval` 与 `awaiting_approval → {queued|pr_open}`
  两条边都进转移表，且入边有真实写入点
- AC5 前端详情页在该状态下显示确认按钮；其他状态不显示
- AC6 `Enqueue` 复制 `gate_mode` 时，任务创建那刻的配置被**钉死**（之后改 repo
  配置不影响在途任务），与 `repo_id` 的语义保持一致

### T3 终态通知

**探查修正**：`Notifier` 接口（`runner/pipeline.go:62`）签名是 `Notify(ctx, message string)`
—— **只有一个字符串，不带 taskID/userID**，按属主发信拿不到收件人；且 runner 包
目前**没有 `*store.Users` 的任何通路**。本项必然要动接口。倾向在 store 加
`NotifyEmailForTask(ctx, taskID)`（`SELECT COALESCE(u.notify_email, u.email) ... JOIN`），
runner 侧照 `VerificationRecorder` 的写法声明窄接口，**不 import internal/mail**（避免成环）。

- AC1 任务进 `failed` / `pr_open` / `awaiting_approval` / `merged` 时向 owner 投信；
  `notify_email` 为 NULL 时**回退登录邮箱**
- AC2 SMTP 未配置时静默跳过且不影响状态流转（单测：`Ready()` 返回 false，断言任务仍进终态）
- AC3 发信失败只记日志，不重试、不阻塞、不改任务状态
- AC4 信里含 issue key、终态、失败分类（若有）、详情页链接（用 `LATHE_BASE_URL` 拼）
- AC5 正文渲染抽成可独立测试的纯函数，照 `httpapi/accounts.go:273` 的 `resetMail`
  惯例（该处注释写明「单独成函数是为了能脱离 SMTP 测正文」）
- AC6 失败落点**只需插桩 `p.fail`**（`pipeline.go:1073`）—— 探查确认 pipeline
  所有失败路径都汇流到它；另需覆盖 mergepoll 的 `merged`(`:246`)、`failed`(`:583`)、
  `cancelled`(`:642`)
- AC7 人工点的取消（`httpapi/api.go:382`）**不发信** —— 人自己点的不用通知自己

### T4 log_ref

**探查修正**：`log_ref` 列已存在且可空，**无需迁移**。断点在
`store/verifications.go:12` 的 `InsertVerification` —— INSERT 语句里就没有这一列。
更要紧：验证输出在 `runner/verify.go:272` 就已被 `truncate` 到 **16KB**，
且**通过的步骤的输出直接丢弃**（只有失败步骤的前 4KB 进 agent_events）。
所以「落盘完整日志」不是在写库那层加个参数就行，要在 `runStep` 层改成边写文件边留摘要。

- AC1 每次验证执行后 `verifications.log_ref` 非空
- AC2 该路径指向的文件真实存在，且含该步骤**完整**的 stdout/stderr（不是 16KB 截断版）
- AC3 日志落在 `DataDir` 下（现 `DataDir` 唯一消费方是 `secret.key`，本项是第二个），
  路径含 task id 与验证轮次，同任务多轮不互相覆盖
- AC4 **通过的步骤也留日志** —— 排障时最想看的往往是「上次通过时是什么样」
- AC5 worktree 被 T6 reaper 回收后日志仍可读（故日志不得放 worktree 里）
- AC6 落盘失败（磁盘满／权限）**不能让验证失败** —— 降级为 log_ref 留空 + 记 warn

### T5 成本面板

**探查发现一个会让这个面板从一开始就算错的坑，必须记下：**

`tasks.agent_cost_usd` **不是任务累计成本**。`persistAgentResult`（`pipeline.go:1058`）
在实现阶段和**修复回路的每一轮**都调用，底层 `store.SetAgentSummary` 是 `UPDATE`
整行覆盖 —— 这一列最终存的是**最后一轮 fix 的单轮成本**。拿它做「每任务成本」会严重低估。
另外 migration 0009 头注释明说**分诊阶段的 result 不落 tasks 表**，
所以「分诊 vs 实现占比」从这一列根本算不出来。

真正的数据源是 `agent_events`：`kind='result'` 的 `payload->>'costUsd'`
（写入点 `integration/agent/digest.go:252`），按 `phase` 分组（`implement` 与
`fix-N` 归实现，`triage` 归分诊）。`agent_events` **没有 user_id 列**，
多租户隔离必须 `JOIN tasks t ON t.id = ae.task_id WHERE t.user_id = $1`。

- AC1 `/api/stats` 返回成本聚合：累计、按阶段（分诊/实现）、按日
- AC2 成本**从 `agent_events` 求和**，不用 `tasks.agent_cost_usd`；
  单测：造一个跑了 2 轮 fix 的任务，断言聚合值等于各轮之和而非最后一轮
- AC3 聚合按 `user_id` 隔离且走 JOIN；沿用 `httpapi/api_test.go:429` 的跨用户隔离断言套路
- AC4 无成本数据时返回零值而非 `null`（照 `Stats.ByState` 用 `map[string]int{}`
  而非 nil 的既有做法，新字段别用指针）
- AC5 前端有可读的成本视图；**不引入新前端依赖**（package.json 现只有
  dompurify/marked/vue/vue-router，无图表库）。金额口径沿用
  `TaskDetail.vue:335` 的 `$` + `toFixed(4)`

### T6 worktree 收割机

**探查修正与陷阱清单**：

1. `tasks.worktree_path` 列已存在，worktree 与 task 行**可直接关联**，不必靠命名约定反推。
   但该列的写入走 `COALESCE($4, worktree_path)`——**只增不清空**，
   回收后若要把列清掉需要新的 SQL 路径。
2. 目录名是 **issue key 小写化**（`worktreeDirName` 把 `CR-1367` 变成 `cr-1367`），
   **不是 task id**。且 `workspaces/` 下还有 `.mirrors/`（bare mirror）与 `.verify/`
   （heavy 基线）两个隐藏目录——**reaper 扫目录时必须跳过 `.` 开头的目录**，
   误删 `.mirrors/` 等于把所有仓库的镜像清了。
   注意 `preview.Discover` 的跳过列表**不含** `.` 前缀通配，别照抄它。
3. **`time.ParseDuration` 不支持 `d` 单位** —— 天级 TTL 必须写 `168h` 而非 `7d`，
   默认值在代码里写 `7*24*time.Hour` 并在注释注明这一点。
4. **TTL 基准用 `tasks.updated_at`**（已有 BEFORE UPDATE 触发器维护）。
5. **`failed` 可以转回 `queued`** —— 失败任务随时可能被人重试续跑，
   这正是 D4「保留现场」的目的。所以 TTL 默认值必须保守（天级），
   激进的 TTL 会把人正要重试的现场删掉。
6. **删分支前必须查 `HasLiveDependentOnBranch`** —— 栈式 PR 的后继可能还在依赖
   这个分支（F4.2-AC2）。不查就会把别人的 base 删掉。
7. **TTL 会改变 D4 的语义**：从「留到同 issue 下一次尝试需要这个槽位为止」
   变成「按需 + 按时」。02-design 里 D4 的表述需同步更新。
8. **顺手修一处文档错**：02-design §8 的 P0 那行声称「worktree 自动回收」已交付，
   但实际只有「合并后回收」（`mergepoll.go` 的 `onMerged`）与「同名尸体按需回收」
   （`worktree.go` 的 `Create`），失败/取消态确实没有回收策略——
   这与 9 个遗留目录的事实一致。这一行的表述要改。
9. 现成旁证：`store/users.go` 的 `WorktreePaths` 在删账号时只把路径**打进日志让人手工回收**
   （注释写着「数据库的行会被外键级联带走，磁盘上的目录不会」）——
   这就是「无回收策略」的自白。

**可直接抄的模板**：`mergepoll.go` 的 `Run`（ticker + ctx.Done 优雅退出）与
`pollOnce`（单个任务失败只 warn 并继续，不让一个坏任务毁掉整轮）；
最贴近的现存代码是 `onMerged`（合并后回收 worktree，已在用 `RepoLookup` 把
`task.RepoID` 换成 `ProviderRepo`）。查询模板照 `ListOpenPRTasks`。
装配照 `main.go` 的 MergePoller 结构体字面量 + `go X.Run(ctx)`。

- AC1 终态任务的 worktree 超过 TTL 后被回收（目录 + 分支 + git worktree 注册项三形态）
- AC2 非终态任务的 worktree **绝不回收**（单测：`implementing` 中的任务，跑 reaper 后目录仍在）
- AC3 TTL 可配（`LATHE_WORKTREE_TTL`，实际落地为 `72*time.Hour`，下限 `1h`，记进 README 环境变量表）。
  若判断需要运行时可调则改走 `system_settings` 表（照 `PreviewThresholds` 的现取现用套路）
- AC4 回收动作留痕（日志 + task_events）
- AC5 **绝不触碰 `.` 开头的目录**（单测：造一个 `.mirrors/` 与 `.verify/`，跑 reaper 后仍在）
- AC6 **删分支前查 `HasLiveDependentOnBranch`**，有活的后继依赖时只删目录不删分支
  （单测：造一个 base_ref 指向该分支的活任务，断言分支仍在）
- AC7 `workspaces/` 里 9 个存量 `cr-*` 遗留目录被首轮 reaper 清掉，或明确说明为何不该清
- AC8 与 T4 联动：日志不在 worktree 里，回收后仍可读
- AC9 02-design §8 P0 的「worktree 自动回收」表述改成与实现一致

**首版 review 拍出来的四个阻断项与最小修复集**（2026-09-10，随 T6 一并合入）：

首版通过了上面 9 条 AC，但那 9 条没覆盖「误删」这一类失效。逐条如下。

**B1 · 删盘前不校验路径当前归属，而目录名只由 issue key 决定。**
`worktreeDirName` 是 `strings.ToLower(issueKey)`，不含 repo_id / user_id / task_id
—— issue `CR-100` 的任何一次尝试、任何仓库、任何用户都落在同一个 `<root>/cr-100`。
而 `tasks_one_active_per_issue` 是 `(repo_id, linear_issue_key) WHERE state NOT IN (终态)`：
老的 failed 行不受约束，两个不同 repo（乃至不同用户）也可同时各有一个活任务。
误删推演（默认 72h，无需任何配置错误）：任务 A（CR-100，repo X）failed 超期、
`worktree_path` 仍在；issue 重开建出任务 B，`Create` 复用同名目录，B 正在里面跑；
`ListReapableTasks` 返回 A；`reapTask(A)` 通过 `safeToRemove`，
`HasLiveDependentOnBranch` 查的是别的任务的 base_ref（B 不是栈式后继）返回 false，
于是 `Discard` 把 B 正在跑的工作区连同未提交改动删掉并 `git branch -D`。
放大变体：`ClearWorktreePath` 失败时只 warn 留到下一轮，A 的路径永远留在候选里，
收割机**每小时**对那个目录执行一次 Discard —— 定时炸弹。

修复三层：① 目录名加 task_id 维度（`cr-100-t1234`），根治跨任务/仓库/用户共用；
② 删盘前查 `ClaimedWorktreePaths`（**非终态**任务引用的路径集合，与同一轮已在查的
`ReferencedWorktreePaths` 是两个不同的问题：后者判「有没有人指着」，用于孤儿清扫；
前者判「有没有在途任务在用」，用于主路径）；③ `Create` 也吃这个集合，
拒绝接管在途现场而不是把它当尸体删掉。

**B2 · 候选快照与真正删除之间没有 compare-and-swap。**
`failed → queued` 是合法转移，pipeline 的断点续跑复用现场、不经过 `Create`。
「人点重试」与「收割机删除」并发时，正在运行的现场被删、分支被强删、
`worktree_path` 被置 NULL。窗口不是毫秒级：循环里每个任务跑若干条 git 命令
（`gitTimeout` 15 分钟）。

修复：新增 `task.Machine.ClaimForReap`，一条带四项守卫的语句
（`state = ANY(终态)` ∧ `worktree_path = $快照路径` ∧ `updated_at = $快照时刻`
∧ `worktree_claimed_at IS NULL`，行锁 `FOR UPDATE`），**认领成功才碰磁盘**。
`updated_at` 用等值而非不等式：要挡的是「快照之后有人动过这一行」，
等值没有可乘之机，也不依赖时钟精度。新增 `tasks.worktree_claimed_at`
（migration 0020）既作幂等标记，也让「谁在什么时候认领的」可查。

认领分两阶段（`commit` 参数）：校验阶段只读、不改任何状态；落账阶段在删盘
**之后**才置空 `worktree_path` 并写 `task_events`。顺序反了的后果是：校验若顺手
置空了路径，随后因为脏现场决定保留时，那份现场就变成「磁盘上有、没人认领」的
孤儿，被下一轮清扫收走 —— D4 的意图被 TTL 回收路径悄悄取消。

**B3 · 主路径既不看目录 mtime 也不看工作区是否 dirty，`Discard` 用的是 `--force`。**
`worktree.go` 的 `Remove(force=false)` 注释写得很明白：「git 会拒绝删除有未提交
改动的工作区 —— 这正是失败任务『保留现场』（D4）所需的保护」。收割机走 Discard
绕开的就是这条保护。孤儿清扫那条路径反而看了 mtime，两条路径安全标准不对称。

修复：主路径改用与孤儿路径相同的判据（`updated_at < cutoff` **且** 目录
mtime `< cutoff`，两把尺子都超期才删）；删之前用现成的 `Inspect` 体检 ——
`Dirty` 则整份现场保留只告警，`HasCommits && !RemoteBranch`（有未推送提交）
则只删目录、保留分支（那些提交的唯一副本在分支里）。

**B4 · TTL 无下限校验，且没有关停/干跑开关。**
`Validate()` 对 `AgentTimeout` 有 `<= 0` 检查，对 WorktreeTTL / ReapInterval 一个都没有；
`reaper.go` 只把 `<= 0` 兜回默认。把 TTL 配成 `1m`（想「先清一批存量目录」是很自然
的运维动作）会让孤儿清扫失去那层「安全得离谱」的宽裕：`Create` 建目录与写入
`worktree_path` 之间那个无人认领窗口，正好是在途任务的目录会被当成孤儿删掉的时机。
而 `LATHE_WORKTREE_TTL=0` 不是关停而是回落 72h —— 这个最具破坏力的组件没有任何
办法关停。

修复：`config.MinWorktreeTTL = 1h` 硬下限（配得更小启动直接报错，错误信息指向
干跑而不是让人去改代码里的下限）；`ReapInterval` 必须为正；
新增 `LATHE_REAP_ENABLED`（默认 `true`，`false` 时 main 根本不启动收割循环）
与 `LATHE_REAP_DRY_RUN`（只打「本轮会删什么」，零副作用零成本 —— 刻意不做认领
校验也不做工作区体检，代价是干跑列出的候选里可能有几条实跑会被保留，日志里
说明了这点）。两个开关都是「拼错就报错」而非静默失效：对「必须能关掉」的开关，
静默失效意味着以为关了其实还在删。

**一并修掉的四处低成本高收益项：**

- **日志要说真话、且删前就记。** `Discard` 原本无返回值，`os.Stat(mirror)` 失败时
  直接 return，什么都没删却照样打「已回收超期现场」并清空 `worktree_path`。
  现在它返回 `DiscardResult`（`MirrorMissing` / `DirRemoved` / `BranchDeleted` /
  `Errs`），三个布尔都是「确实做成了」而非「尝试过」——`BranchDeleted` 删前先
  `rev-parse` 确认分支真的在。日志改成删前（「即将删除 X，因为 Y」）+ 删后
  （「真删掉了什么」）两条。
- **回收动作进任务事件流。** `ClaimForReap` 的落账阶段写一条 `task_events`
  （`from_state == to_state == 当前状态`，靠 `payload.kind = "worktree_reaped"` 区分），
  payload 带 issue / state / path / branch / ttl_seconds / cutoff / rule /
  dir_removed / branch_deleted / branch_kept_because。actor 是 `node:<NodeName>`，
  与队列派发同形 —— 任务详情页要能答出「现场是谁在什么时候按什么规则删的、
  删成了什么样」。
- **`WorkspaceRoot` 加专用目录校验。** 原先只校验非空 + 绝对路径。配成 `/` 或
  `/opt` 时孤儿清扫会删掉所有「非 `.` 开头、无人认领、mtime 超期」的顶层目录。
  现在要求至少两层，且拒绝一批系统目录黑名单。
- **symlink 越界。** `filepath.Rel` 是纯字符串运算，看不见符号链接：一条
  `<root>/evil → /etc` 会被判成「在根下」，`os.RemoveAll` 顺着它删到根外。
  现在 root 与候选路径各过一次 `EvalSymlinks`（root 也要解析 —— `/opt/lathe/workspaces`
  指向另一块盘是常见挂载手法，只解析候选会让每条正常路径都被误拒，收割机彻底罢工），
  候选不存在时退回「解析已存在的父链 + 保留末段」，堵住
  `<root>/link-to-outside/not-yet-created` 这种形状。

**存量目录名的兼容方案**（改 `worktreeDirName` 带来的唯一迁移面）：

1. **老任务行不受影响。** 它们存的是完整路径 `worktree_path`，找自己的现场从来
   不走命名规则（`Inspect` / 断点续跑 / `Discard` 都直接吃路径）。
2. **孤儿清扫不需要认识老命名。** 它按「在 `<root>` 下 + 名字不以 `.` 开头 +
   没有任何任务行指着 + mtime 超期」四条判，与目录叫什么无关。所以改命名规则
   **不会**让任何存量目录被误判成孤儿而提前清掉；反方向（老目录被无限期遗留）
   才是真风险，见下一条。
3. **`Create` 顺带探测老路径。** 新任务落在 `<issue>-t<id>`，但同 issue 的老现场
   躺在 `<issue>`。若不认它，那个目录会变成「谁都不需要这个槽位、也没人认领」的
   永久垃圾（虽然最终会被孤儿清扫收走，但要多等一个 TTL）。所以 `Create` 在
   接管槽位时同时看新旧两条路径，老路径存在且**没有在途任务占用**时一并回收。
4. **老槽位被在途任务占着时跳过、不报错。** 老槽位不是本任务要用的路径，
   别人在里面跑与我们无关；报错会让一个无关任务把本任务卡死。
5. **`TaskID <= 0` 退回老命名**（而不是拼一个 `-t0`）：退回来的路径是
   `legacyWorktreeDirName` 认得的形状，收割机与 `Create` 都能正常处理。


### T7 webhook 联动

**探查发现两处硬阻碍，AC 据此收紧：**

1. Linear webhook payload 结构体（`integration/linear/linear.go:401`）
   **没有 labels 也没有 labelIds** —— 标签接单必须先给结构体加字段。
   `Data.State` 字段**有**（含 `Type`，`canceled` 即取消判据）但全仓无消费方。
   `update` 事件要靠 `UpdatedFrom["labelIds"]` 判断「这次改的是标签」，
   套路与现成的 `UpdatedFrom["assigneeId"]`（`linear.go:452`）一致。
2. **取消联动做不到「停住在跑的 agent」**：`Transition` 到 `cancelled` 只改库，
   runner 全包没有任何地方在执行中途回读 task state。而 `cancelled` 是无出边终态，
   在途 agent 跑完再转移会被 `Validate` 拒绝并报错。**必须显式决策，不能含糊过去。**

- AC1 issue 打上触发标签（默认 `lathe:go`，可配）时接单
- AC2 issue 被取消（`Data.State.Type == "canceled"`）时其在途任务转 `cancelled`，
  actor 口径照 `mergepoll.go:641` 写成 `"system:webhook"`，并跟着调 `PropagateBlocked`
- AC3 标签名可配；**未配置时行为与现状一致**（不得让存量部署突然开始接单）。
  回归护栏：`webhook_test.go:183` 与 `:199` 两个「忽略」用例必须仍绿
- AC4 幂等：同一事件重放不产生第二个任务。除既有 `ClaimDelivery` 去重外，
  `tasks_one_active_per_issue` 部分唯一索引是第二道防线（标签与指派双触发时靠它挡）
- AC5 验签失败一律拒绝 —— 不得在 ingress 上开任何新的免验签路径
- AC6 **在途 agent 的取消语义必须写明**：本轮接受「只转状态、agent 白跑完一轮」，
  并在 pipeline 转移失败处补一条明确日志（而非现在的裸错误），真正的取消信号
  传播列为后续项。取消联动需要「按 issue 找在途任务」的查询，谓词抄
  `flow/service.go:357`，改按 `user_id + linear_issue_id`
### T8 per-task compose 隔离

**探查结论：AC2 原先写的「复用 preview，不新写」过于乐观，需要修正为分层复用。**
`internal/preview` 的能力边界摸清后，有三条结构性障碍与一条撞车风险：

**撞车风险（必须优先解决，否则会误杀正在跑的容器）**：
`preview.ComposeProject(taskID)` 写死返回 `"lathe-preview-t%d"`，而
`Manager.Stop(taskID)` 的清理方式**不是 `docker compose down`**，是按 `lathe.task=<id>`
标签查出容器/网络/镜像后逐个 `rm -f` / `network rm` / `rmi`。所以如果验证栈沿用
同一套项目名与标签，**人在看板点一次「停止预览」就会把同一任务正在跑的验证容器一起删掉**，
反之亦然。验证栈必须有独立的项目名前缀与独立的 task 标签键。

**结构性障碍**：

1. `Manager.Start` 是**异步 fire-and-forget**（内部 `go m.run(...)`），
   失败只写进 `ops[taskID].Error`，靠 `Status` 轮询发现。验证要的是同步阻塞、
   起不来立即报错。直接复用等于把一个本该同步的调用改写成状态机轮询。
2. `ops map[int64]*Op` 以 taskID 为键且 `ErrBuildInProgress` 会拒绝同 taskID 的
   第二次启动 —— 验证期间人点预览（或反过来）会互相拒绝。
3. `exec` / `execStream` 是**私有字段**，只在 preview 包内测试注入假件。
   runner 包无法为验证栈注入假 docker ⇒ **要么验证栈单测依赖真 docker，
   要么代码必须落在 preview 包内**。这一条直接决定代码放哪。
4. preview 的 `run` 只起服务、**没有「在栈里跑一条命令并收退出码」的能力**。
   最接近的 `waitReady` 里的 `docker exec` 只判成败、不收集输出、硬编码 500ms
   轮询 + 30s 超时。而验证的本质就是「在栈里跑测试命令并要退出码和输出」。
5. `Recommend` 是 agent 推荐 + 人拍板，超时 10 分钟且非确定性 ——
   **不能放进验证路径**。验证是无人值守的，必须机械地决定起哪个 compose。

**据此定的路线**：分层复用而非整体复用。
- **复用（纯逻辑、已经过实战）**：`Discover` / `ScanComposeEnv` / `ParseExposes`
  （候选扫描）、`buildOverrideYAML`（端口随机化，需导出或同包）、
  `CheckResources`（资源闸门）、`resolveDatabase` 的 clone/fresh 策略
- **不复用**：`Manager.Start` / `Stop` 的生命周期编排 —— 它是为「人点按钮、
  异步看进度」设计的，形状不对
- **代码落点**：在 **preview 包内**新增面向验证的**同步**入口（这样能用上私有的
  `exec` 注入点与 override 生成），runner 通过窄接口调用。
  不让 runner 直接吃 `Manager.Start`

**修正后的验收标准**：

- AC1 heavy 档红绿验证在 per-task 隔离环境里执行（light 档不起栈，与现状一致）
- AC2 **分层复用**：候选扫描/端口随机化/资源闸门复用 preview 的现成实现，
  不得出现第二份 override 生成或第二份阈值测量代码；生命周期编排可另写同步版本
- AC3 验证栈的 compose 项目名与 docker 标签**与预览栈完全隔离**。
  单测：同一 taskID 同时存在预览栈与验证栈时，`Stop` 预览不影响验证栈容器
- AC4 任务结束（含失败与超时）后容器/网络/自建镜像全部清理，无泄漏
  （验收方式：跑一轮验证前后 `docker ps -a`、`docker network ls`、`docker images` 计数一致）
- AC5 资源闸门生效：内存/磁盘超阈值时不启动隔离环境，**退化路径显式决策并记录**
  （排队等待 vs 回落无隔离执行）。注意现有阈值键名是 `preview_*`，
  验证栈是复用同一阈值还是另设，也要显式决定
- AC6 无 compose/Dockerfile 的仓库仍能验证（退化到现状路径），隔离不得成为硬门槛
- AC7 **起栈失败必须有独立的错误身份**：不能冒充 `StepReproFail` + `StatusError`，
  否则会被 `redEnvError` 归类为「环境问题、任务失败留现场」——
  语义上恰好正确，但错误信息会误导人以为是复现测试跑不起来。
  需要独立 step name 或哨兵 error
- AC8 红阶段失败的三分路由（`redStepFailure` → `blocked_spec`、
  `redEnvError` → 失败留现场、`isReproContractErr` → 进修复回路）**一条都不能破坏**
- AC9 隔离开销可观测：验证耗时变化落在事件流里，便于判断是否值得
- AC10 生命周期挂点用现成的对称结构：`pipeline.go` 的 `runHeavy` 里
  `CreateDetached` + `defer Remove` 已经是「起-拆」对称，隔离栈挂同一处
- AC11 `queue.go` 的 **worker 数 = light + heavy 槽位之和**这个耦合关系需重新审视
  并在本文记录结论（起隔离栈后每个 heavy 任务的资源占用显著变化）

## 5. 全局约束

1. **TDD**：每项先写失败的测试，再实现。红-绿是这个产品自己的信条，不能只对用户的仓库要求
2. **不碰绿测试**：`make test` 现有用例不得为了让新代码过而修改，除非该用例本身断言了被本轮
   有意改变的行为，且在 PR 描述里说明
3. **每个 PR 自带**：`go vet` + `gofmt` 干净，`make test` 全绿
4. **每个新增可配置项必须有消费方**——本轮就是在还这条纪律的债，不能一边还一边新欠
5. **不做合并决策**：本轮所有 PR 都由人点合并（README 核心边界）
6. **前端改动需 `make ui`** 重建并同步内嵌目录，否则二进制里还是旧界面

## 7. T9 · 测试基线可信化（开工时新发现，前置于其余各项）

### 现象

开工前跑 `make test` 建立基线，`cmd/lathe` 包 FAIL；单独跑 `go test ./cmd/lathe/...` 却通过。
再跑一遍完整套件又过了。典型的「首轮红、之后绿」。

### 根因（已实验证伪一次，以下是取证后的结论）

**第一次假设（错的）**：开发库里 3 条 2026-08-28 的 `SMK-1/2/3` 排队尸体被全局
`ClaimReady`（排序 `priority DESC, id`）优先领走。
**证伪方式**：把这 3 条行 `UPDATE ... SET state='queued'` 还原后单跑 `cmd/lathe`
包 —— **通过了，EXIT=0**。所以陈尸不是原因，至少不是充分原因。教训：
先复现再下结论，我这次是先下结论再复现。

**取证后的真根因**：是**两个不同的**测试隔离缺陷，都源于「测试跑在共享真实
Postgres 上，而生产查询是全局的」，但机制不同：

**缺陷 1 —— `cmd/lathe` 的 `TestRunOneClaimedRecoversInterruptedStateFromEvents`**
（`queue_test.go:367` 断言「应领到恢复后的任务」）：

失败时日志是
```
INFO 在途任务已恢复 task=12460 interrupted_state=implementing
INFO 在途任务已恢复 task=12489 interrupted_state=implementing
INFO 启动恢复完成 requeued_inflight=2
```
`requeued_inflight=2` —— `Reconcile` 把**同包另一个测试遗留的在途任务** 12460
也重新入队了，随后全局 `ClaimReady` 按 id 升序把它优先领走，测试拿到了别人的任务
（断言输出里的 issue key 是 `CR-777`，不是本用例的 fixture）。
注意 12460 与 12489 **都是本轮创建的 id** —— 所以这是**同包测试之间**的污染，
不是跨轮次的历史残留。

**缺陷 2 —— `internal/runner` 的 `TestPipelineHeavyNoReproTestIsFailure`**
（`pipeline_test.go:595`）：
```
建任务失败: task: 创建任务失败: ERROR: duplicate key value violates
unique constraint "tasks_one_active_per_issue" (SQLSTATE 23505)
```
固定的 fixture issue key 撞上了遗留的活任务（部分唯一索引
`(repo_id, linear_issue_key) WHERE state NOT IN 终态`）。

**共同点**：`-p 1` 只挡包间并行，挡不住「同一个包内前一个测试留下的活任务行」
被后一个测试的全局查询捞走。Makefile 里已有的 `-p 1` 注释给了虚假的安全感。

### 为什么必须先修

1. 本文 §5 约束 3 要求「每个 PR 自带 `make test` 全绿」。基线本身首轮必红，
   这条约束无法执行——分不清红是自己改坏的还是陈尸导致的
2. 这类假绿会掩盖真回归：任何人第一次 clone 后跑测试都会看到无关失败，
   于是学会「再跑一遍就好了」，从此对红色脱敏
3. Makefile 里已有注释承认共享库限制并用 `-p 1` 缓解，但 `-p 1` 只挡**包间并行**，
   挡不住**跨轮次的数据残留**——注释给了虚假的安全感

### 修法（已实施）

`ClaimReady` 的全局语义是**生产上正确的**（单机调度器就该看全局队列），
所以**一行生产代码都没改**，隔离责任全在测试侧。三处改动：

**1. fixture 唯一性（治缺陷 2 的根）**

`internal/runner/pipelineFixture` 与 `internal/task/fixture` 的 email 原先是
`"<前缀>-" + t.Name() + "@example.com"` —— **没有随机量** —— 再配
`ON CONFLICT (email) DO UPDATE`，于是被中断那一轮留下的孤儿 user 会被**复用**，
连带它名下的 repo 与那些用固定 issue key（`CR-777` / `CR-1001` / `CR-ORCH-ROOT`）
建的非终态任务，下一轮 `Create` 必然撞部分唯一索引。

修法：email 加 `UnixNano()`，并**去掉两处 `ON CONFLICT`**。
新 user 天然给出新 repo_id（repos 唯一键是 `(user_id, provider_repo)`），
固定 issue key 也就被限定在这个 repo 内，跨轮次不再撞车。
`provider_repo` 保持原值（`acme/demo` / `Clouditera/CloudRouter`）——
调用方的断言依赖它。去掉 `ON CONFLICT` 是刻意的：带了随机量就不该再有冲突，
真撞上了应该大声报错，而不是静默复用别人的行。

对照：`cmd/lathe` 的 `fixture` 本来就用了 `UnixNano()`，是干净的 ——
三个包里两个有病、一个健康，说明这不是设计决定而是各写各的。

**2. 领单断言按归属过滤（治缺陷 1 的根）**

`cmd/lathe/queue_test.go` 新增两个 helper：

- `claimOwn(t, q, ctx, userID)` —— 循环 `ClaimReady` 直到领到属于本 fixture
  的任务，外来任务显式跳过并 `t.Logf` 留痕。收敛性由 `ClaimReady` 自己保证：
  它的 WHERE 里有 `lease_expires_at IS NULL OR lease_expires_at < now()`，
  领走的行会带上租约、不会被重复返回。上界 200 条防呆。
- `assertNoneOwnClaimable(t, q, ctx, userID, ...)` —— 断言「属于自己的任务里
  没有一条可被领取」。原先写成 `if tk, _ := ClaimReady(...); tk != nil { fail }`
  是错的：那条断言会被库里任何一条与被测语义无关的外来行推翻。

跳过外来任务的代价很小：`ClaimReady` 只写 `lease_expires_at` 与 `node_id`、
**不改 state**（它的文档注释明确写了这一点，因为转移表里没有 queued→queued
这条边）。顺带的好处是外来行被租约挡住，后续用例也不会再被它们干扰。

**3. 领单前排空（`internal/task` 包）**

这个包的三个用例（`TestClaimReadyConcurrency` / `TestClaimReadyLeaseExpiry` /
`TestClaimReadyRespectsDependsOnAt`）都断言「领到的就是自己创建的那条」。
这里用 `drainForeignQueue(t, m, ctx)`：在**创建自己的任务之前**把已有候选用
长租约领空，那一刻所有候选都必然不属于本用例。比事后按 owner 过滤改动更小，
而且 `TestClaimReadyLeaseExpiry` 要用 100ms 短租约测过期，事后过滤会把
短租约用在跳过外来任务上、反而不可靠。

**没采用的方案**：包级 `TestMain` 直接删「不属于本轮的 queued 行」。
它对指向真实库的 `LATHE_TEST_DSN` 是危险的 —— 测试代码不该有删生产数据的能力。
清理动作单独做成脚本、要显式 `--yes`（见下）。

### 验收标准

- AC1 **连续两次** `make test` 都全绿，且第一次不依赖前一次的副作用
  （验收方式：手工注入孤儿 `queued` / 在途行 → 跑整套 → 必须绿）
- AC2 `go test -p 1 ./... -count=1` 的**真实退出码**为 0
  （注意：不能用 `go test ... | tail` 判断——那取到的是 `tail` 的退出码，
  本轮开工时正是这样误判了一次基线为绿）
- AC3 测试跑完后开发库里不残留本轮 fixture 造的 `queued` 行
- AC4 `cmd/lathe`、`internal/task`、`internal/runner` 三个包都覆盖
- AC5 生产代码 `Machine.ClaimReady` 的签名与语义不变（本轮零生产代码改动）
- AC6 新增两条**回归测试**，确定性复现原失败：
  `TestPipelineFixtureSurvivesInterruptedRun`（预置同名孤儿 user + 非终态
  `CR-777`，断言 `pipelineFixture` 仍可用）与
  `TestClaimOwnIgnoresForeignInflightTasks`（先造 id 更小的外来在途任务，
  断言仍领到自己那条）。两者在修复前都实测为红

### 顺带清理

`scripts/clean-test-db.sh` —— 可重跑，默认**干跑只报告**，加 `--yes` 才真删。

判别条件是 **email 以 `@example.com` 结尾**：RFC 2606 把该域保留给文档与测试，
真实用户不会用，所以既充分又安全。删 user 会级联带走
repos / tasks / task_events / verifications / agent_events。
不属于 fixture 的非终态任务（如 user_id=1 名下 8 月那几条 `pr_open`）
只报告、不删 —— 那是真实历史数据，该由人判断。

磁盘上的 worktree 目录**不在本脚本职责内**：数据库行会被级联带走，目录不会。
那是 T6 收割机的事。

## 8. 执行记录

> loop 每轮回写：哪项动了、验收过没过、卡在哪。

- 2026-09-09：建立绿色基线时**发现基线本身不可信**——`cmd/lathe` 首轮 FAIL，
  根因是开发库里 3 条 `SMK-*` 排队尸体被全局 `ClaimReady` 优先领走（详见 §7）。
  新增 T9 作为前置项。同时记一笔教训：`go test ... | tail` 取到的是 `tail`
  的退出码，本轮据此误判过一次「全绿」，此后一律用 `$?` 或 `PIPESTATUS[0]` 判定。
- 2026-09-09：T0 已提交（`chore(release): v0.1.0 发布准备`），
  `go build ./...` 与 `go vet ./...` 全绿，Postgres 起、迁移最新。
- 2026-09-09：**T1 完成并提交**（`9fc5992`）。删 `cmd/lathe-runner/` 与 Makefile
  的 `RUNNER_BIN` 目标；`config.NodeName` 按 AC4 **保留**并把注释改成记录它的两个
  真实消费方（task_events 的 actor 前缀、管理界面运行时面板）；同步 6 处文档引用。
  AC1/AC2/AC3/AC4 全部验过：目录已删、Makefile 无引用、`make build` 只产出
  `bin/lathe`、`go build`/`go vet`/`gofmt` 全绿。顺手删掉了陈旧的 `bin/lathe-runner`。
- 2026-09-09：**T9 根因取证完成**（详见 §7）。过程里犯了一个值得记的错：
  先给出「陈尸被全局 ClaimReady 领走」的结论，再去复现，结果**实验把自己的结论证伪了**
  （还原 3 条陈尸后单跑 `cmd/lathe` 通过）。改用「反复跑整套直到红」的取证方式后
  拿到两个具体用例名与各自的失败输出，才定位到两个不同机制的隔离缺陷。
  另记一条观测教训：`go test ... | tail` 取到的是 `tail` 的退出码，
  本轮据此误判过一次「基线全绿」——此后一律用 `$?` 或 `PIPESTATUS[0]`。

- 2026-09-09：**T9 实施完成**。生产代码**零改动** —— `ClaimReady` 的全局语义在
  生产上是对的，隔离责任全在测试侧。三处改动：`internal/runner` 与 `internal/task`
  的 fixture email 加随机量并去掉 `ON CONFLICT DO UPDATE`；`cmd/lathe` 新增
  `claimOwn` / `assertNoneOwnClaimable` 两个归属过滤 helper 并改造 7 处脆弱断言；
  `internal/task` 三个领单用例改为建任务前先 `drainForeignQueue`。
  新增两条回归测试，修复前实测为红、修复后转绿。
  **AC1 验收方式比原定的更严**：不只是插 3 条陈尸，而是注入
  「6 条 queued + 1 条在途 + 4 条 pr_open」的脏库，并且第二轮刻意注入各包的
  **固定 issue key**（`CR-777` / `CR-1001` / `CR-ORCH-ROOT`）—— 正是会撞车的那些。
  两遍都是 `GOTEST_EXIT=0`、15 个包全绿、零 FAIL —— AC1 的「连续两次且第一次
  不依赖前一次副作用」达成，且两遍都跑在故意弄脏的库上（第二遍的脏数据是
  清理脚本执行后新注入的，与第一遍无因果关系）。
  配套产出 `scripts/clean-test-db.sh`（默认干跑，`--yes` 真删，判别条件是
  email 以 `@example.com` 结尾）与 `make clean-test-db` 目标；
  Makefile 里那段「-p 1 就够了」的注释也改掉了 —— 它给的是虚假的安全感。
  顺带结清 roadmap §1.3 的 `repos` id=241 占位行（属 fixture 残留，被清理带走）。

- 2026-09-09：**T2 实现完成**（PR B，分支 `feat/gate-mode`）。
  真正的断点确认在 `Enqueue` 而非 pipeline —— 它从不把 `repos.gate_mode`
  复制进任务行，所以 `tasks.gate_mode` 永远是 `direct`，人在仓库配置页
  选了 manual 也毫无效果。改动：
  - `resolveRepoID` → `resolveRepo`，顺带取出 `gate_mode` 并复制进
    `CreateParams`（任务创建那刻**钉死**，与 `repo_id` 语义一致）
  - `internal/task` 新增 `GateDirect/GateManual/GateGuarded/GatePlanFirst`
    四个常量，并在注释里写明**只有 manual 有实现语义**，另三个按 direct 处理
  - 状态机加两条边：`verifying → awaiting_approval`（闸门落点）与
    `awaiting_approval → queued`（放行路径）
  - `Pipeline.gateBeforePush`：验证通过后按 `tk.GateMode` 决定是否停机；
    停机时转 `awaiting_approval` + 回帖告诉人「活干完了等你点」
  - `RetryApproved` 重试模式 + `PlanRetry` 分支 → `EntryPush`
  - `POST /api/tasks/{id}/approve`：只放行真的停在闸门上的任务
    （其余状态 409 —— 否则「确认」就是个能把任意任务推去开 PR 的后门），
    跨用户返回 404，转回 `queued` 并写 `mode=approved`，重派**原任务行**
  - 前端：详情页「确认开 PR」按钮 + 一张说明卡（`awaiting_approval`
    的状态文案「待放行」本来就在全站状态表里，无需新增）
  - 鉴权回归表补登 `/approve` —— 本仓纪律是「所有 API 端点都必须要求认证」，
    漏登记等于没有护栏

  **一个必须防的死循环**：批准后以 `EntryPush` 重入时闸门不能再次触发，
  否则 `awaiting_approval → queued → awaiting_approval` 无限转圈，
  人点一次批准永远开不出 PR。实现上用 `entry != EntryPush` 守卫，
  并在核心测试里显式断言「批准后恰好开一次 PR」。

  测试：`TestPipelineManualGateHaltsBeforePRThenResumesOnApproval` 覆盖完整
  闸门周期（停住→批准→补开 PR，两半合在一个测试里，只验前者会漏掉
  「停住之后再也走不动了」这种更糟的实现）；
  `TestPipelineNonManualGateModesUnchanged` 对三个非 manual 取值逐个断言
  行为与现状一致（AC2）；`TestEnqueueCopiesRepoGateMode` 含「事后改仓库配置
  不影响在途任务」的钉死语义断言；三条 approve 端点测试（正常/错状态/跨用户）。

  门禁：注入 12 条非终态孤儿行后跑整套，`GOTEST_EXIT=0`、15 包全绿、
  `go vet` 与 `gofmt` 干净、`make ui` 通过。另外顺手核了一次「绿是不是真的」：
  `cmd/lathe` 只用了 0.438s（之前 2.5s）显得可疑 —— 该包连不上库时走
  `t.Skipf`，而 skip 也显示 `ok`。用 `-v` 数了一遍：14 个用例 RUN、
  14 个 PASS、0 个 SKIP，绿是真的。**这一类核对以后每轮都做**，
  本轮已经被假绿骗过两次（`| tail` 取错退出码、无条件打印的「gofmt 干净」）。

- 2026-09-09：记一笔**遗留隐患**（不在本轮范围，但已确认存在）：
  `internal/httpapi` 的 `apiFixture` 与 `mustUser` 也是「确定性 email +
  `ON CONFLICT DO UPDATE` + 固定 repo `acme/api-test` + 固定 issue key
  （`CR-R1`/`CR-EV`/`CR-PV`）」的组合 —— 与 T9 修掉的那两个包同款。
  它目前不红是因为这个包不用全局 `ClaimReady`，但一旦有非终态孤儿残留，
  `Create` 同样会撞 `tasks_one_active_per_issue`。没有混进 PR B 是为了
  不让一个动状态机的 PR 里夹带无关的测试重构。**建议单独一个小 PR 修掉。**

- 2026-09-09：**T3 实现完成**（PR C，分支 `feat/notify-and-logref`，栈在 B 之上）。
  两个设计约束贯穿全程：
  1. **runner 不 import `internal/mail`**。runner 只声明窄接口 `TaskMail`
     （`internal/runner/notify.go`），实现 `taskMailer` 放在 `cmd/lathe`
     —— 那里同时拿得到 store（解析收件人）与 mail（发信）。
     这与既有的 `VerificationRecorder`/`AgentEventRecorder` 是同一套做法。
  2. **发信绝不影响状态流转**。`mailTerminal` 刻意**不返回 error**：
     调用方都在「任务已进终态」之后调它，返回错误只会诱导调用方去处理一个
     不该影响主流程的东西。SMTP 没配、投递失败、收件人查不到 ——
     一律只记日志。把通知做成能让任务卡住的东西，等于用一个「锦上添花」
     的功能给主流程加了一个新的失败点。

  收件人口径（`store.NotifyEmailForTask`）：优先 `notify_email`，
  **NULL 与空串都回退登录邮箱**（界面上清空通知邮箱存下来的是空串，
  不能因此发出一封收件人为 `""` 的信）。与密码重置刻意不同 ——
  重置邮件永远发登录邮箱，因为那封信的意义就是「证明你拥有这个登录邮箱」。
  单次 JOIN 而非「先查任务再查用户」：通知在终态转移后的热路径上。
  查不到收件人返回 `ErrNoRecipient`，调用方据此静默跳过，
  不把「没人可发」当成发信故障刷日志。

  六个落点全接：`p.fail`（pipeline 所有失败路径的汇流处）、
  `stagePushAndPR` 的 `pr_open`、`gateBeforePush` 的 `awaiting_approval`、
  以及 mergepoll 的 `merged` / rebase 冲突 `failed` / PR 关闭 `cancelled`。
  mergepoll 走 `p.Pipeline.mailTerminal` 而不是自己再拼一套 ——
  同一份渲染逻辑只该有一处，否则两边正文迟早不一致。

  **AC7（人工取消不发信）由构造保证**：`httpapi` 侧根本没有 `Mail` 依赖，
  `cancelTask` 无从发信。人自己点的取消不需要通知自己。

  测试：`terminalMail` 是纯函数，脱离 SMTP 断言「issue key／终态／失败原因／
  失败阶段／补充信息／详情链接」都在正文里，以及 BaseURL 为空时**省略**
  链接那一行（而不是拼一个指向 localhost 的无用链接）；
  `TestPipelineFailureNotifiesOwnerAndSurvivesSMTPOutage` 是本项最重要的一条 ——
  SMTP 全程报错，断言任务依然干净落在 `failed`；
  另有闸门通知、pr_open 通知、`stateSubject` 全终态覆盖、
  以及 4 条 store 收件人测试（优先/回退/空串/任务不存在）。

  门禁：注入 14 条非终态孤儿行后跑整套，`GOTEST_EXIT=0`、15 包全绿、
  `go vet` 与 `gofmt` 干净。store 那 4 条用 `-v` 数过：4 RUN / 4 PASS / 0 SKIP。

- 2026-09-09：**修正一处 PR 拆分判断错误**。原先写的「B/C/D/E/F 互不依赖，
  可并行推」是错的：C 依赖 B，因为 T3-AC1 把 `awaiting_approval` 列为通知
  触发状态之一，而那个状态只有 T2 才让它可达。我在 baseline 上开了 C 的分支、
  写到一半才发现 `gateBeforePush` 根本不在场，已改栈到 B 之上。
  教训：拆 PR 时「模块不重叠」不等于「无依赖」，还得看**验收标准之间**
  有没有互相引用。

- 2026-09-09：**T4 实现完成**（PR C 的第二半）。光在 INSERT 里补一列远远不够，
  真正的坑在上游两处：
  1. 输出在 `runStep` 里就已被 `truncate` 到 16KB。那个截断理由是对的
     （别把整个构建日志灌进数据库），但结果是**完整日志从来没在任何地方
     存在过** —— 所以落盘必须发生在截断之前。
  2. **通过的步骤的输出直接丢弃**（只有失败步骤的前 4KB 进 `agent_events`），
     而排障时最想看的往往正是「上一次通过时是什么样」。

  设计选择：
  - **做成注入的 `StepLogger` 而不是给 `Verifier` 加 `logDir` 字段**。
    `Verifier` 是并发任务共用的单实例（`cmd/lathe` 里只 `NewVerifier` 一次），
    加可变字段再在每轮验证前改一下，就是一个货真价实的数据竞争。
    每轮由 `Pipeline.stepLogger(taskID, round)` 现造一个已把 taskID 与轮次
    烘进去的 logger 传进去，`Verifier` 自己保持无状态。
  - **日志落 `DataDir` 而非 worktree**（AC5）。worktree 会被回收
    （合并后回收、同名尸体回收、T6 收割机），而日志的全部价值就在于
    「现场没了之后还能查」。放 worktree 里等于排障时正好没有。
  - **`log_ref` 存相对 `DataDir` 的路径**，不是绝对路径：`DataDir` 可配
    （`LATHE_DATA_DIR`），绝对路径写进库会让部署目录一变、历史记录全失效。
  - **分轮次目录**（AC3）：同任务多轮互相覆盖的话，「第一轮为什么挂」
    这个问题在第二轮跑完之后就永远回答不了了 —— 而那恰恰是修复回路
    最需要回答的问题。
  - **复现阶段的 `log_ref` 是空格分隔的多个路径**：一个复现阶段可能有多条
    测试，每条各自落一份完整日志。合并成一份会丢信息 —— 内层 `runStep`
    返回的 `Output` 已被截断到 16KB，拿它拼出来的合并日志同样残缺。
  - 顺手加了 `sanitizeLogName`：复现测试的步骤名来自测试文件路径，
    直接拼进路径就是一个目录穿越，测试里用 `../../etc/passwd` 这类输入断言。

  **一条值得记的教训**：串参时漏了 `HeavyParams{... Logs: logs}` 这一个字段，
  而**单元测试全绿** —— 是端到端测试（`TestPipelineWritesLogRefForEveryVerifyStep`）
  把它抓出来的。如果只写单元测试，T4 就会变成又一个「有列没消费方」，
  正是这一轮要治的病本身。根因是那次批量替换只对函数签名加了 `assert`、
  对 struct literal 没加，于是静默失败。**以后每处替换都要有断言**，
  否则「改了」和「以为改了」分不开。

  门禁：注入 16 条非终态孤儿行后跑整套，`GOTEST_EXIT=0`、15 包全绿、
  `go vet` 与 `gofmt` 干净。

- 2026-09-09：**T5 实现完成**（PR D，分支 `feat/cost-panel`，base 在 A 上）。
  核心是躲开那个会让面板从第一天起就说谎的坑：**不用 `tasks.agent_cost_usd`**。
  最关键的测试 `TestCostStatsSumsEventsNotTaskColumn` 就是钉这一条 ——
  造「分诊 + 实现 + 2 轮 fix」共 $0.90，把 `tasks.agent_cost_usd` 设成
  最后一轮的 `0.08`，断言聚合值是 0.90 而不是 0.08。

  后端：`store.CostStatsFor` 四个聚合（总计 / 按阶段 / 按日 / 任务排行），
  全部 `JOIN tasks` 按 `user_id` 过滤 —— `agent_events` 没有 `user_id` 列，
  漏掉 JOIN 就是跨用户求和，P1.5 数据隔离的红线。`fix-N` 归入 implement 桶：
  修复回路是实现的延续，分开看没有决策价值。加 migration 0018 的部分索引
  `agent_events (task_id) WHERE kind='result'` —— 既有的
  `agent_events_task_id` 服务 SSE 增量拉取，谓词里没有 kind，帮不上聚合。

  **偏离了我自己写的 AC1**：AC1 说「`/api/stats` 返回成本聚合」，实际做成
  独立端点 `GET /api/stats/cost`。理由：`Board.vue` 每 5 秒轮询 `/api/stats`，
  而 `agent_events` 是全表最大的一张，四个聚合压进那个轮询纯属浪费；
  成本是决策视图，不需要 5 秒新鲜度。理由写进了代码注释。

  前端 `CostPanel.vue`（零新依赖，内联 SVG）。按 dataviz 规范走完整流程：
  - **先定形态再定颜色**：总花费是一个数字 → stat tile，不是「只有一根柱子
    的柱状图」；阶段占比用水平堆叠条不是饼图；任务排行**刻意不按值上色** ——
    那会把条长重复编码成色相，白占掉唯一的空闲通道
  - **配色跑校验器而不是靠眼睛**：按项目**实际** surface（亮 `#ffffff`、
    暗 `#171a21`，与参考默认不同）两个模式都跑，CVD 相邻分离度
    ΔE 9.2/9.4（门限 8）、常视力 27.6/26.5（门限 15）全 PASS。
    亮色下 aqua 对比度 2.82:1 触发 relief 规则 → 堆叠条始终带可见直接标签
    且提供表格视图，颜色永不是唯一通道
  - 沿用项目的暗色优先约定（`:root` 即暗色，亮色走 `prefers-color-scheme`），
    不另立门户

  **没能真截图**：环境里没有无头浏览器也没有 SVG 转换器。退一步把组件的几何
  数学原样搬进 node 脚本做数值核查，覆盖宽屏/窄屏/单日/三日/过千/全零六个场景，
  断言「标签不溢出、不裁切、坐标都在画布内、容器高度含 x 轴文字带」。
  **这一步真查出一个 bug**：y 轴刻度用 `fmtUSD` 会渲染成 `$0.0000`
  （7 字符 ≈46px @11px），右对齐在 `PAD.left-8` 处向左溢出容器 8px。
  已改成专用的 `fmtAxis`（干净刻度 `$0.90`）并把左内边距 46 → 52，
  现在余量 11–17.6px。核查脚本本身也报错过一次（`worst` 初值设成 0，
  取 min 永远 ≤0，把真实余量报成 0.0px）—— 断言逻辑是对的，报告是错的，
  一并修了。

  门禁：注入 18 条非终态孤儿行后跑整套，`GOTEST_EXIT=0`、15 包全绿、
  `go vet` 与 `gofmt` 干净、`make ui` 通过。

- 2026-09-09：**T6 实现完成**（PR E，分支 `feat/worktree-reaper`，base 在 A 上）。

  **AC7 原先的假设被现实推翻。** 我把 9 个存量目录逐个对照了数据库，
  它们分三类而不是「首轮 reaper 全清掉」：
  - `cr-1454 / 1468 / 1469 / 1488`（failed/cancelled + 有路径）→ **会回收**
  - `cr-1460 / 1465 / 1466 / 1467`（**pr_open，非终态**）→ **绝不回收**。
    这是真实待合并的 PR，正是 AC2 在起作用
  - `cr-1367`（failed 但 `worktree_path` 为 **NULL**）→ DB 驱动的回收
    **结构上看不见它**

  最后一条是真缺口，写完主路径才发现，于是补了**孤儿目录清扫**
  （磁盘上有、数据库里没人认领的目录，按 mtime 判超期）。
  **那条清扫里的 TTL 不只是策略，是安全机制**：worktree 目录先被 `Create`
  建出来、之后才在转入 `implementing` 时把路径写进任务行，两步之间目录
  无人认领。不看 mtime 就删，正在跑的任务会突然找不到自己的工作区。
  `TestReaperSweepSparesFreshUnclaimedDirs` 专门钉这一条。

  防呆（每条都有对应测试）：
  - **路径安全用 `filepath.Rel` 判而非字符串前缀** —— 前缀判会把
    `/root/workspaces-backup` 当成 `/root/workspaces` 的子目录。
    测试专门造了这个同前缀兄弟目录
  - **逐段拒绝 `.` 开头**：误删 `.mirrors/` 等于把所有仓库的镜像清了，
    下一个任务要重新 clone
  - **删分支前查 `HasLiveDependentOnBranch`**（F4.2-AC2），查询出错时
    保守处理（只删目录留分支）
  - **用 `Discard` 而非 `Remove`**：尸体可能残缺（目录被手工删过、
    分支已不存在），`Remove` 在这种情形下会报错
  - **孤儿清扫不碰被任务行认领的目录**：否则 `pr_open` 的现场被主路径
    正确排除后又被 mtime 删掉，AC2 形同虚设

  `worktree_path` 置空走专门的 `ClearWorktreePath`：`Transition` 的 UPDATE
  是 `COALESCE` 语义（只增不清空），传 nil 表示「这次不改」。
  刻意不清 `branch_name` —— 分支可能因还有活后继而保留，
  且「这个分支叫什么」排障时仍有用。

  > **⚠ 上一段已过时**（2026-09-10 加固后）：`ClearWorktreePath` 是无守卫的
  > 裸 UPDATE，正是 B2 竞态的一半根因，已连同其测试一并删除，**现在没有
  > 无守卫的置空原语**。置空统一走 `ClaimForReap` 的落账阶段（带
  > state / worktree_path / updated_at 三项守卫 + 行锁）。`COALESCE` 那条
  > schema 事实仍然成立，由 `TestClaimForReapTwoPhases` 两头钉住
  > （校验阶段不动路径、落账后置 NULL）。详见上面的四个阻断项一节。

  AC9 顺带修了 `docs/02-design.md` §8 P0 一句**不实表述**：
  那行声称「worktree 自动回收」已交付，实际只有「合并后回收」与
  「同名尸体按需回收」两个被动触发点，失败/取消的现场只增不减 ——
  那 9 个目录就是这么来的。

  门禁：注入 21 条非终态孤儿行后跑整套，`GOTEST_EXIT=0`、15 包全绿。
  reaper 11 条测试 + task 层 3 条（含 AC2「非终态绝不进候选」的六状态断言）。

- 2026-09-09：**T7 实现完成**（PR F，分支 `feat/webhook-triggers`，base 在 A 上）。
  三个关键判断：
  1. **取消分流必须在接单闸门之前。** 取消事件不是指派事件 —— 放在闸门之后
     会被当成「非指派事件」直接 `ignored` 掉，整个功能静默失效。
  2. **`completed` 刻意不算取消。** 只认 Linear 的 `canceled` 状态类型
     （和 `remove`）。人手工把 issue 标成完成，不代表平台任务该作废 ——
     那个任务可能正在跑，或已开出 PR 等人合并。判据用**状态类型**而非
     状态名：名字是每个团队自己起的（Cancelled / 废弃 / 不做了），
     类型才是 Linear 的稳定枚举。
  3. **标签默认空串即关闭**（AC3 的实现方式）。默认给 `lathe:go` 的话，
     某个早就在用这个标签表示别的意思的仓库会在升级后突然自动接单。
     判定函数里 `label == ""` 直接返回 false。

  payload 侧：`Labels`/`LabelIDs` 原本**根本不在结构体里**，得先加。
  判定用**标签 name 而非 labelIds 里的 UUID**（人在界面上打的是名字），
  且 `update` 事件必须看 `updatedFrom` 里有没有 `labelIds` ——
  否则 issue 停在带标签状态时改个标题就会重复接单。这套路与现成的
  `IsAssignedTo` 看 `assigneeId` 完全一致。

  **AC6 的能力边界如实写进了代码与文档**：取消**只改数据库状态，不会停下
  正在跑的 agent** —— runner 全包没有任何地方在执行中途回读 task state。
  在途 agent 会白跑完一轮，然后在下一次状态转移时因 `cancelled` 是无出边
  终态而被 `Validate` 拒绝。没有假装取消是即时的；真正的取消信号传播
  列为后续项。

  标签名走 `system_settings`（改完即刻生效，不用重启），并在「系统设置」页
  加了输入框 —— 否则又是一个「有设置项但没地方填」的反面。
  管理端刻意**不对标签名做格式校验**：Linear 的标签名可以是任意文本
  （含空格、中文、emoji），定规则只会把合法标签挡在外面。

  测试：18 条（11 条既有 + 7 条新增）。AC3 点名的两个回归护栏
  （`TestWebhookIgnoresNonAssignment`、`TestWebhookIgnoresOtherUsersIssue`）
  仍绿；AC5 那条「伪造签名的取消事件必须被拒绝」—— 取消分流放在验签
  **之后**，所以没有在 ingress 上开新的免验签路径。
  另有 linear 层 20+ 表驱动判定用例与 task 层跨用户隔离断言。

  门禁：注入 23 条非终态孤儿行后跑整套，`GOTEST_EXIT=0`、15 包全绿。

  **合并前审计后的加固（三处，均为「目前够不到但防线该在使用点」）**：
  ① `CancelForIssue` 拒绝空 `issueID` —— 构造 `data:{}` 能让空串进到
  `WHERE linear_issue_id = ''`，眼下打不中是因为存量为 NULL 而 `= ''`
  不匹配 NULL，那是数据凑巧不是防线；② 取消传播补跨属主告警 ——
  `PropagateBlocked` 的递归 CTE 不带 `user_id` 过滤，`pipeline.fail`
  对同一调用有告警而这条没有，且这条挂在**外部可触发**的 webhook 上，
  更需要；③ `hasLabel` 在 trim 后为空时一律不匹配 —— `IsLabelTriggered`
  只判 `label == ""`，纯空白的 `" "` 穿得过去并让**任何空名标签**命中，
  等于在没配置的部署上悄悄打开接单，正是 AC3 要保证的语义。生产上靠
  两道上游 `TrimSpace` 够不到，但这条语义不该寄托在两个上游都记得 trim。

  两条已知项未在本 PR 收口，如实记账：`linear_issue_id` 无索引（取消路径
  在 webhook 热路径上全表扫 `tasks`，属性能项，加索引要新迁移会与同期 PR
  抢编号）；AC6 披露不全 —— 取消发生在 verifying 阶段时在途 agent 仍会
  真的开出 PR（`CreatePR` 在 `Transition(pr_open)` 之前），随后转移被拒，
  于是一个已开的 PR 挂在 `cancelled` 任务上，而 MergePoller 只扫 `pr_open`
  所以它永不被回收。这属于「取消信号传播」后续项，不是本 PR 能收口的。

  **合并前审计后的加固（两个阻断项）**：
  ① **迁移锁表，且 CONCURRENTLY 在本框架下结构性不可用。** 0018 不带
  CONCURRENTLY 会在建索引期间阻塞 `agent_events` 写入 —— 而它是全表最大的
  一张，被挡住的正是所有在跑任务的事件流写入。更根本的是 `applyOne` 把每条
  迁移包在事务里，而 Postgres 禁止在事务块内执行 `CREATE INDEX CONCURRENTLY`，
  所以**后续任何加索引的迁移都得面对同一件事**。修的是框架：新增
  `-- lathe:no-transaction` 标记（必须独占一行，避免注释里提到它的迁移被误判），
  非事务路径逐条发语句（pgx 简单协议发多语句会被服务端当成隐式事务，
  CONCURRENTLY 照样报 25001），版本记录写在全部语句成功之后 —— 这是幂等性的
  支点：中断不记账，下次仍视为待应用。0018 首句无条件 `DROP INDEX IF EXISTS`，
  因为 `CREATE INDEX CONCURRENTLY IF NOT EXISTS` 遇到 INVALID 索引会因
  「名字已存在」直接跳过、**永远修不好**；结尾 DO 块查 `indisvalid`，把
  「索引存在但无效」变成响亮的失败而不是哑巴残骸。`runMigrate` 的 2 分钟超时
  放宽到 10 分钟并可配。
  ② **前端 ResizeObserver 从不挂载。** `ref="plotBox"` 在 `v-else-if="cost"`
  子树里，而 `onMounted` 执行时 `cost` 还是 null、渲染的是「加载中…」，
  `plotBox.value` 必为 null → observer 从未创建 → SVG 宽度永远停在 640。
  改用 `watch(plotBox, cb, { flush: 'post' })`（同步回调，没有 async 版本
  那个「给已移除元素挂 observer」的窗口）。这一条同时修正原自述的一处不实：
  几何核查「覆盖窄屏 360px」核的是几何函数在 `plotW=360` 时的正确性，而运行时
  `plotW` **永远到不了 360** —— 数学验对了，接线没通。

  **合并前审计后的加固（4 条阻断项）。** 审计的判词值得原样记下：原有防呆
  （`filepath.Rel`、逐段拒绝 `.` 开头、活依赖检查）都真实存在且正确，
  但**全部防的是「路径写错了」，没有一条防「这条路径现在还归这条任务行吗」**。
  ① 目录名只由 issue key 决定（`worktreeDirName` 不含 repo_id/user_id/task_id），
  而 `tasks_one_active_per_issue` 只约束非终态行 —— 于是老的 failed 行能与新的
  活任务共存并共用 `<root>/cr-100`，默认 72h 下就能把新任务正在跑的工作区连同
  未提交改动删掉。加 taskID 根治，并在删盘前查 `ClaimedWorktreePaths`（只算
  非终态，与含终态行的 `ReferencedWorktreePaths` 是两个不同问题）、认领后再查
  一次。② 候选快照与删除之间没有 CAS，而 `failed → queued` 合法且断点续跑
  复用现场不经过 `Create` —— 人点重试与收割机并发时现场被删。新增
  `worktree_claimed_at`（0020）与 `ClaimForReap`，四项守卫在行锁内，
  `updated_at` 用**等值**而非不等式（要挡的是「快照之后有人动过」，等值没有
  可乘之机也不依赖时钟精度）；两阶段设计——落账放在删盘之后，否则「决定保留
  脏现场」时那份现场会变成孤儿被清扫收走，D4 的意图被 TTL 路径悄悄取消。
  ③ 主路径既不看 mtime 也不看 dirty，而 `Discard` 用的是 `--force` ——
  恰好绕开 `Remove(force=false)` 那条「git 拒绝删除有未提交改动的工作区」
  保护，也就是 D4 本身。补双尺子 + `Inspect` 体检。④ TTL 无下限，且
  `LATHE_WORKTREE_TTL=0` 不是关闭而是回落 72h —— 这个最具破坏力的组件没有
  任何办法关停。加 1h 硬下限 + `LATHE_REAP_ENABLED` / `LATHE_REAP_DRY_RUN`
  （字符串型、拼错就报错：对「必须能关掉」的开关，静默失效意味着以为关了
  其实还在删）。

  修复过程里修掉两个自己的 bug，都值得记：`discardLocked` 的 `DirRemoved`
  恒为 false —— `git worktree remove --force` 成功时会自己删掉目录，于是
  随后的 `os.Stat` 失败、整个 if 块被跳过；这是**最常见的正常路径**，后果是
  记账全错、`Removed()` 为 false 导致主路径回收计数为 0。另一个是测试夹具
  没还原生产语义（假件的 `ReferencedWorktreePaths` 不含终态行），于是主路径
  正确跳过的现场被同一轮的孤儿清扫删掉 —— 改成让假件返回
  `referenced ∪ reapable 的非空路径`，让「忘了设」从结构上不可能出现。
  新增守卫 `TestReaperSkippedSceneIsNotSweptAsOrphan`（表驱动覆盖四种跳过
  成因）+ 反证 `TestReaperStillSweepsTrulyUnreferencedDir`：这条语义是上面
  全部修复的隐含前提，此前没有任何测试保护。

  顺带删掉 `Machine.ClearWorktreePath`（`ClaimForReap` 的无守卫版本、生产
  零调用方，而 `TestClaimForReapTwoPhases` 已两头钉住那条 schema 事实）。
  留着是个 footgun，而本轮还的正是「配了没接线」这条纪律。
