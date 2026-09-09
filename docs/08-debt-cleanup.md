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
| T3 | 任务终态邮件通知（接 `notify_email`） | C | TODO |
| T4 | `verifications.log_ref` 落盘写入 | C | TODO |
| T5 | 成本聚合面板（store 聚合 + API + 前端） | D | TODO |
| T6 | worktree TTL 收割机 | E | TODO |
| T7 | webhook 联动：标签接单 + issue 取消联动 | F | TODO |
| T8 | per-task compose 隔离（验证阶段） | G | TODO |

## 3. PR 拆分与理由

单 PR 装 9 项会变成不可评审的巨块，且任一项翻车会连坐其余。
按「同类欠账 + 同一模块」聚合成 7 个 PR：

| PR | 内容 | 聚合理由 |
|---|---|---|
| **A** `chore/release-and-prune` | T0 发布准备 + T1 删骨架 | 都是清理类，不碰运行逻辑，可最先合 |
| **B** `feat/gate-mode` | T2 闸门 | 动状态机（新增可达状态），独立评审 |
| **C** `feat/notify-and-logref` | T3 通知 + T4 日志指针 | 都是「终态时补一个副作用」，共享 pipeline 终态落点 |
| **D** `feat/cost-panel` | T5 成本面板 | 纯读侧聚合 + 前端，零运行风险 |
| **E** `feat/worktree-reaper` | T6 收割机 | 独立后台轮询器，仿 mergepoll |
| **F** `feat/webhook-triggers` | T7 webhook 联动 | 只动 ingress 层 |
| **G** `feat/verify-isolation` | T8 compose 隔离 | 动验证主路径，风险最高，**最后合** |

顺序：A → B/C/D/E/F（互不依赖，可并行推）→ G。

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
- AC3 TTL 可配（`LATHE_WORKTREE_TTL`，默认 `7*24*time.Hour`，记进 README 环境变量表）。
  若判断需要运行时可调则改走 `system_settings` 表（照 `PreviewThresholds` 的现取现用套路）
- AC4 回收动作留痕（日志 + task_events）
- AC5 **绝不触碰 `.` 开头的目录**（单测：造一个 `.mirrors/` 与 `.verify/`，跑 reaper 后仍在）
- AC6 **删分支前查 `HasLiveDependentOnBranch`**，有活的后继依赖时只删目录不删分支
  （单测：造一个 base_ref 指向该分支的活任务，断言分支仍在）
- AC7 `workspaces/` 里 9 个存量 `cr-*` 遗留目录被首轮 reaper 清掉，或明确说明为何不该清
- AC8 与 T4 联动：日志不在 worktree 里，回收后仍可读
- AC9 02-design §8 P0 的「worktree 自动回收」表述改成与实现一致

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
