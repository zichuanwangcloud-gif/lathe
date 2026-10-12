# Lathe — 05 · 优化路线图

> 2026-08-17 · 状态：执行中（2026-10-12 按代码证据逐条回写状态，改动处附代码位置）
> 引用约定：代码位置优先写「文件 + 符号名」（如 `internal/runner/pipeline.go` 的
> `stageVerify`），不写行号 —— 行号会随每次合入漂移，符号名不会。
> 前置：[02-design.md](./02-design.md)（系统设计）、[04-agent-visibility.md](./04-agent-visibility.md)（执行可见性）
> 依据：本文每条方向都标注了代码证据或实战事故编号（任务 #466/#479/#492 等），
> 不接受"感觉应该做"。

---

## 0. 一个贯穿性观察：「配置了但没接线」是复发性疾病

本次会话（CR-1363 全链路打通）实战撞见的断供清单：

| 字段/组件 | 状态 | 发现方式 |
|---|---|---|
| `repos.exclude_dirs` | 字段存在、配置层不 SELECT → 已修复（dd1cb09） | 任务 #316 验证挂在不维护目录 |
| `repos.gate_mode` | ~~可配、pipeline 从不读~~ → 已接线（2026-09-09，08 T2）| 代码走查 |
| `notify_email` | 字段有了、`internal/mail` 只接了密码重置 | 代码走查 |
| `agent_session_id` | 注释明说为 `--resume` 留的、无 resume 逻辑 | pipeline.go:201 |
| `internal/scheduler/` | 空目录 | §6 调度设计的占位 |
| `verifications.log_ref` | ~~从不写入~~ → 已接线（2026-09-09，08 T4）| 任务 #466 排障时无日志可查 |

**结论：每个可配置字段必须有消费方。** 把这条加进 PR 自查清单，比任何单点修复都值钱。

## 1. 历史欠账盘点

### 1.1 设计文档里承诺了但未交付的

| 来源 | 承诺 | 现状 |
|---|---|---|
| 02-design §5 | 修复回路（验证失败带输出续跑 agent，限次数） | ✅ **已交付**：`Pipeline.MaxFixAttempts`（`internal/runner/pipeline.go` 的 `stageVerify`）驱动修复回路，`cmd/lathe/main.go` 的 `buildPipeline` 接线 `cfg.FixAttempts`（`LATHE_FIX_ATTEMPTS`，默认 2 轮），续跑走 `--resume` 原会话 |
| 02-design §6.4 | 租约与故障恢复 | ✅ **单机形态已交付**：启动时 `q.Reconcile`（`cmd/lathe/main.go` 的 serve 启动段）把在途任务回 queued 重派；断点续跑凭 `InterruptedState` 推导入口（`internal/runner/retry.go` 的 `PlanRetry`）。多节点租约仍欠（P3 范畴） |
| 02-design §7 | `PR 评论 → review_feedback → implementing(--resume) → 重新验证` | ✅ **已交付**：`POST /webhooks/github/{slug}`（用户级签名密钥验 `X-Hub-Signature-256`，`X-GitHub-Delivery` 去重）接 review 评论，任务转 `review_feedback → queued` 后以 `--resume` 续原实现会话处理意见、重新验证并更新同一个 PR；现场不可用的任务由 `POST /api/tasks/{id}/review-retry` 走出 |
| 02-design §7 | `合并 → merged → 回收 worktree` | ✅ **已交付**：mergepoll 的 `handleMerged`（F4.1）转 merged；`onMerged` 回收现场（F4.2），分支被未合并后继依赖时暂不回收 |
| 02-design §8 P1 | per-task compose 隔离（红绿阶段） | ◐ **部分交付**：`repos.verify_infra` 依赖隔离栈（08 T8，`internal/runner/pipeline.go` 的 `stageVerify` 经 `runHeavy` 消费，与预览容器按标签隔离互不误删）；红绿测试进程仍在 worktree 里跑，完整进程隔离仍欠 |
| 02-design §8 P3 | 多节点：`cmd/lathe-runner` 节点代理 | ~~54 行骨架，TODO 未装配~~ —— **已决：删除**，见 [08-debt-cleanup.md](./08-debt-cleanup.md) D8-1 |

### 1.2 本次会话诊断出但未修的

| 缺陷 | 事故证据 |
|---|---|
| ~~重试端点语义断裂：failed→queued 后走 `tasks.Create` 撞唯一索引~~ —— **已修复**：`/retry` 转 queued 后 `Queue.Requeue` 重派**原任务行**（`internal/httpapi/api.go` retryTask，代码注释留 #313 教训），resume 预检失败返回 409 与理由 | 任务 #313 |
| ~~前置失败不落痕：pre-flight 裸 `return` 绕过 `p.fail`~~ —— **已修复**：拉单/分诊/建区/实现各阶段失败一律走 `p.fail` 落痕（`pipeline.go` StageFetchIssue…StageCommit 十余处） | 最初"卡排队"病根 |
| ~~worktree 尸体阻塞同 issue 重试（失败保留现场但无回收策略）~~ —— 已修复：`Create` 回收同名尸体再建（目录/分支/普通残留三形态），重试端点 Fresh 计划先 `Discard` | #345、#466 各撞一次 |
| ~~EventSink 落库 UTF-8 截断 bug~~ —— **已修复**：`truncate` 回退到 rune 边界再截（`integration/agent/agent.go`，注释引 SQLSTATE 22021） | 54075f4 commit body 已记录 |
| ~~agent 进程 env 泄漏~~ —— **已修复**：`sanitizedEnv` 改白名单制（`integration/agent/agent.go:338`），只放行 PATH/HOME 等操作性变量，`LATHE_ADMIN_TOKEN`、数据库口令、GitHub token 不进 agent 进程；确需追加走 `LATHE_AGENT_ENV_EXTRA` | 代码走查 |
| 分诊工作目录污染：triage 未设 `Dir` → 继承 serve 的 cwd=`/opt/lathe` → `--setting-sources project` 把 Lathe 自己的 CLAUDE.md 灌进目标仓库的分诊上下文 | 代码走查 |

~~已修复~~（B2-3）：分诊改在中立目录执行（默认 `$TMPDIR/lathe-triage`，位于任何项目树之外，`Pipeline.TriageDir` 可覆盖）。

### 1.3 数据残留

- ~~`repos` id=241（`acme/member-repo`，user 535 的占位行）~~ —— 已清理（2026-09-09）
- ~~task #217（SMOKE-2 排队尸体）~~ —— 已清理，无需处理

**2026-09-09 补记**：这类残留不是偶发，是**测试 fixture 被中断后的必然产物**
（测试进程被杀 → `t.Cleanup` 不执行 → user/repo/task 三张表的行原样留下）。
而且它会让下一轮测试变红，因为 `ClaimReady`／`Reconcile` 都是全局查询 ——
实测过两种崩法，取证见 [08-debt-cleanup.md](./08-debt-cleanup.md) §7。

治法分两层：测试侧已经免疫（fixture 加随机量、断言按归属过滤、领单前排空），
库里的存量用 `make clean-test-db` 清（判别条件是 email 以 `@example.com` 结尾，
RFC 2606 保留域；默认干跑，`YES=1` 才真删）。

### 1.4 已纠正的误判

- ~~"Linear webhook 未实现"~~ —— **已实现**（`internal/httpapi/webhook.go`：按用户 slug 路由、验签、幂等去重）。缺的是更深的联动语义，见 §3.4。

## 2. 已验证有效的资产（不要动）

本次 CR-1363 实战（#466 → #479 → #492）证明以下机制真实工作：

- 红-绿复现证明：#492 绿阶段 9.6s 真实执行，拦截了"agent 只写测试没实现"的交付
- 仓库级验证排除（exclude_dirs）：不维护目录不再拖死任务
- 包级回归收敛：存量坏测试不再让每个任务替历史还债
- cc-switch wrapper 通道：分诊/实现跟随用户通道切换，无需重启服务
- agent 执行可见性（04）：事件落库 + 摘要卡 + 日志面板

## 3. 优化方向（按自用价值分组）

### 3.1 闭环可靠性 —— 第一梯队

平台当前只能"发现问题"，不能"自我纠正"。

1. ~~**§5 修复回路**~~（已交付，见 §1.1）：验证失败 → `--resume` 原会话就地修复 → 再验证，限 `LATHE_FIX_ATTEMPTS` 轮（默认 2）；每轮验证日志分目录留存，不互相覆盖。
2. ~~**重试语义修复**~~（已交付）：retry 重派原任务行而非新建。
3. ~~**启动 reconcile**~~（已交付）：启动时 `q.Reconcile` 在途任务回 queued 重派（`cmd/lathe/main.go:189`）。
4. ~~**前置失败落痕**~~（已交付）：pre-flight 错误一律走 `p.fail`。

### 3.2 成本与效率 —— token 就是钱

5. ~~**成本核算面板**~~（已交付，08 T5）：`GET /api/stats/cost` 聚合视图（`internal/httpapi/api.go` costStats）+ 看板 `CostPanel.vue`，每任务/每日成本可查。
6. ~~**模型路由**~~（已交付，B2-2）：`LATHE_TRIAGE_CHANNEL` / `LATHE_IMPLEMENT_CHANNEL` 配 cc-switch 通道名，pipeline 按阶段以 `LATHE_AGENT_CHANNEL` 注入子进程，wrapper（`scripts/claude-cc-switch`）从 `~/.cc-switch/config.json` 现取该通道的 BASE_URL/TOKEN/MODEL；通道不存在时告警回落激活通道。未配置则全局跟随 cc-switch 激活通道。
7. ~~**分诊目录中立化**~~（已交付，B2-3）。

### 3.3 生命周期与现场管理

8. ~~**worktree 收割机**~~（已交付，08 T6）：终态任务的现场按 TTL 回收（默认 72h）。
    实际比原估的多一块：DB 驱动的回收看不见「磁盘上有、没有任务行指向」的孤儿目录
    （任务 649 是 failed 但 `worktree_path` 为 NULL，`cr-1367` 因此永远没人回收），
    所以补了一条按 mtime 判超期的孤儿清扫。那个 TTL 不只是策略是**安全机制**：
    worktree 目录先被 Create 建出来、之后才把路径写进任务行，两步之间无人认领，
    不看 mtime 就删会让在途任务突然找不到自己的工作区。
9. ~~**通知闭环**~~（已交付，08 T3）：任务终态（失败/待审批）接 `internal/mail` 发 `notify_email`（`internal/runner/notify.go`）。
10. ~~**GateMode 接线**~~（已交付，08 T2）：`gate=manual` 时验证通过后停在
    `awaiting_approval`，人在详情页点「确认开 PR」才推分支开 PR。
    放行走的是与手动重试同一条成熟通道（转回 `queued` + payload 里
    `mode=approved` → `PlanRetry` 给出 `EntryPush`，只补 push + 开 PR，均幂等），
    不在 HTTP 处理器里同步跑 git／调 GitHub。
    真正的断点原来不在 pipeline 而在 `Enqueue`：它从不把 `repos.gate_mode`
    复制进任务行，所以 `tasks.gate_mode` 永远是 `direct`。
    另三个取值（`guarded`/`plan-first`）**仍按 `direct` 处理并在代码里写明** ——
    不给它们编造语义，那又是一次「配了没接线」。

~~**任务预览环境**~~（已交付）：看板一键在 worktree 里构建 Dockerfile 镜像、起容器、
随机端口映射，人手动点完一键停止并清理；内存/磁盘占用超阈值（系统设置可配，
默认 90%）禁止启动。见 02-design §10。

### 3.4 集成深度 —— 从手动触发到事件驱动

11. ~~**webhook 联动加深**~~（已交付，08 T7）：标签驱动接单 + issue 取消联动。
    两处实现上的要点：
    - 标签名在系统设置里配，**默认空串即关闭** —— 不能让存量部署因为升级
      就突然开始按标签接单（某个仓库可能早就在用 `lathe:go` 表示别的意思）
    - 取消分流必须在接单闸门**之前**：取消事件不是指派事件，走到那个闸门
      会被当成「非指派事件」直接 ignored 掉
    - `completed` **刻意不算取消**：人手工标完成不代表平台任务该作废 ——
      那个任务可能正在跑，或已开出 PR 等人合并
    **已知边界**：取消只改数据库状态，**不会停下正在跑的 agent**（runner
    全包没有任何地方在执行中途回读 task state）。在途 agent 会白跑完一轮，
    然后在下一次状态转移时因 `cancelled` 是无出边终态而被 `Validate` 拒绝。
    真正的取消信号传播列为后续项。
12. **PR 生命周期回流**（§7 已设计）：(a) ✅ PR review comment → `review_feedback` → resume 续跑修复（`POST /webhooks/github/{slug}` + 每用户密钥验签、投递去重；已知边界：二轮进行中到达的意见不单独回流，等任务回到 `pr_open` 后的下一条意见触发时一并处理）；(b) ✅ PR merged → 任务 `merged` + worktree 回收（`internal/runner/mergepoll.go` 的 `handleMerged`/`onMerged`，F4.1/F4.2）；(c) ◐ **仅编排后继链** —— `rebaseFollowup` 由两条路径触发，两条都只服务「编排链里的后继任务」：①前驱任务被合并（`onMerged` → 顶层调用 `rebaseFollowup`）；②前驱 PR 被 force-push 改内容（`checkForcePush` → 复用同一入口）。判定后继用的 `TasksWithBaseRef` 条件是 `base_ref = <前驱分支名>`，而 `base_ref` 只由 `cmd/lathe/queue.go` 的 `fillBaseRef` 给 `depends_on` 非空的后继任务写。因此**独立 PR 的 base 分支前进（其余人往 main 推了新提交）不会自动 rebase**，需要「观察 base 落后量 → 主动 rebase 并重验」的独立机制。

### 3.5 安全与隔离 —— 自用也要防手滑

13. ~~**agent 进程 env 白名单**~~（已交付）：`sanitizedEnv` 白名单制（`integration/agent/agent.go:338`），agent 子进程拿不到 `LATHE_ADMIN_TOKEN`。
14. **执行隔离**：中期方向 bubblewrap/容器；P1 分期里的 per-task compose 隔离仍欠着。

### 3.6 度量与迭代

15. ~~**平台自度量**：成功率、各环节时长分布、失败分类直方图 —— 迭代平台本身时回答"改动有没有用"。~~ **已交付**：`GET /api/stats/metrics` + 看板「平台度量」面板。各环节（排队/分诊/实现/验证/端到端）的 p50/p95/最大/累计，失败分类直方图带中文标签。数据源 `task_events`，取首次进入的里程碑并丢弃续跑产生的负差值。**口径是「环境耗时」不是「平台忙时」**：分诊段包含 awaiting_approval 的人工等待、排队长包含 blocked_dep 的依赖等待（`internal/store/metrics.go` 的 segmentMarksCTE 注释里有取舍理由）。**注意成功率已有**（`Stats.SuccessRate`，看板顶部），本次没有重复做。
16. ~~**跨任务记忆**：失败 digest 入库，同 issue 重试时注入"上次死在哪"。与修复回路互补（回路管单次任务内，记忆管跨任务）。~~ **已交付**：`failure_digests` 表（migration 0030，append-only）+ 注入点在 runner 的 prompt 构造外层：**实现链路**的注入决策收敛到 `runAgent` 入口的 `injectFailureMemory`，分诊阶段是独立旁路（走 `stageTriage` 的 `Agent.Run`，不经过 `runAgent`），带上限与「这是历史」的措辞。摘要是失败步骤输出的**尾部**，落库前脱敏（`redactText`：**权威层**——平台已知凭据值按字节精确替换（配置 DSN/集成 token/名字命中词根的环境变量登记整串；隔离栈 `StackEnv` 只登记含 `user:pass@` 的 DSN 值与其口令段）；形态表兜底，非穷举，边界登记在 `redact.go` 顶部）。定位键是 `(user_id, repo_id, tracker_provider, external_key)` —— 按单子而非任务行，因为 issue 关掉重开会新建任务行。

## 4. 建议批次

| 批次 | 内容 | 理由 |
|---|---|---|
| ~~**B1**~~ | ~~修复回路（1）+ 重试语义（2）+ 启动 reconcile（3）+ env 白名单（13）+ EventSink UTF8 修复~~ | **已清账**（2026-10-12 按代码核对：5 项全部交付，证据见 §1.1/§3） |
| ~~**B2**~~ | ~~模型路由（6）~~ + ~~分诊目录（7）~~ + ~~成本面板（5，08 T5）~~ + ~~通知（9，08 T3）~~ + ~~worktree reaper（8，08 T6）~~ | **已清账** |
| **B3** | ~~GateMode（10，08 T2）~~ + ~~webhook 联动（11，08 T7）~~；PR 回流（12）的 (a)(b) 已交付 | 剩唯一欠项：(c) 独立 PR 的 base 前进自动 rebase |
| ~~**持续**~~ | ~~度量（15）+ 跨任务记忆（16）~~ | **已清账**（2026-10-12 按代码核对：两项均交付，证据见 §3.6） |

## 5. 未决（继承 02-design §9 并增补）

- [ ] 多用户下同仓库的并发改动冲突（继承）
- [ ] P2 租户隔离安全模型（继承）
- [ ] 准入档位实测校准（继承）
- [ ] `light` 档是否强制要求新增测试（继承）
- [ ] heavy 档磁盘余量纳入闸门（继承）
- [x] ~~`cmd/lathe-runner` 骨架：推进 P3 还是删除~~ —— **已决：删除**（08 D8-1）。
      当前无多机场景，且它违反 §0 纪律的加强版：不是「字段无消费方」，是整个二进制无消费方。
      真有多机需求时从 git 历史捞回。注意 `config.NodeName` **保留**——它的两个消费方
      （task_events 的 actor 前缀、管理界面运行时面板）与多节点无关。
- [~] 修复回路的次数上限与升级策略：上限已交付（`LATHE_FIX_ATTEMPTS` 默认 2，超限按当前验证结果收尾）；
      **升级策略仍欠** —— N 次失败后是放弃还是换模型/换思路重试（新增）
- [ ] 跨包破坏的回归覆盖：包级收敛后跨包破坏靠仓库自己的 CI，平台是否要可选的"全模块回归"开关（新增）
