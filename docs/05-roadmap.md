# Lathe — 05 · 优化路线图

> 2026-08-17 · 状态：待评审
> 前置：[02-design.md](./02-design.md)（系统设计）、[04-agent-visibility.md](./04-agent-visibility.md)（执行可见性）
> 依据：本文每条方向都标注了代码证据或实战事故编号（任务 #466/#479/#492 等），
> 不接受"感觉应该做"。

---

## 0. 一个贯穿性观察：「配置了但没接线」是复发性疾病

本次会话（CR-1363 全链路打通）实战撞见的断供清单：

| 字段/组件 | 状态 | 发现方式 |
|---|---|---|
| `repos.exclude_dirs` | 字段存在、配置层不 SELECT → 已修复（dd1cb09） | 任务 #316 验证挂在不维护目录 |
| `repos.gate_mode` | 可配、pipeline 从不读 | 代码走查 |
| `notify_email` | 字段有了、`internal/mail` 只接了密码重置 | 代码走查 |
| `agent_session_id` | 注释明说为 `--resume` 留的、无 resume 逻辑 | pipeline.go:201 |
| `internal/scheduler/` | 空目录 | §6 调度设计的占位 |
| `verifications.log_ref` | 从不写入 | 任务 #466 排障时无日志可查 |

**结论：每个可配置字段必须有消费方。** 把这条加进 PR 自查清单，比任何单点修复都值钱。

## 1. 历史欠账盘点

### 1.1 设计文档里承诺了但未交付的

| 来源 | 承诺 | 现状 |
|---|---|---|
| 02-design §5 | 修复回路（验证失败带输出续跑 agent，限次数） | 未实现 —— 验证失败即任务死亡 |
| 02-design §6.4 | 租约与故障恢复：崩溃 → 任务回 queued 重派；`implementing` 中断凭 `agent_session_id` 续跑 | 未实现 —— 重启即孤儿（#346 实证） |
| 02-design §7 | `PR 评论 → review_feedback → implementing(--resume) → 重新验证` | 状态机跑到 `pr_open` 即止（pipeline.go:69），review 回路无入口 |
| 02-design §7 | `合并 → merged → 回收 worktree` | `merged` 状态存在但没有代码设置它 |
| 02-design §8 P1 | per-task compose 隔离（红绿阶段） | 未做，红绿目前在 worktree 里直接跑 |
| 02-design §8 P3 | 多节点：`cmd/lathe-runner` 节点代理 | 54 行骨架，TODO(task#5/#6) 未装配 —— **决策点：推进还是删除** |

### 1.2 本次会话诊断出但未修的

| 缺陷 | 事故证据 |
|---|---|
| 重试端点语义断裂：failed→queued 后 `runOne` 走 `tasks.Create` 撞唯一索引，重试=永久卡死 | 任务 #313 |
| 前置失败不落痕：pre-flight 裸 `return` 绕过 `p.fail`，任务永远"排队中"无痕迹 | 最初"卡排队"病根 |
| ~~worktree 尸体阻塞同 issue 重试（失败保留现场但无回收策略）~~ —— 已修复：`Create` 回收同名尸体再建（目录/分支/普通残留三形态），重试端点 Fresh 计划先 `Discard` | #345、#466 各撞一次 |
| EventSink 落库 UTF-8 截断 bug：Digest 切断多字节字符 → 整批事件丢弃（SQLSTATE 22021） | 54075f4 commit body 已记录 |
| agent 进程 env 泄漏：`sanitizedEnv()` 只剔 4 个变量，`LATHE_ADMIN_TOKEN`、GitHub token 原样漏给 agent 子进程 | 代码走查 |
| 分诊工作目录污染：triage 未设 `Dir` → 继承 serve 的 cwd=`/opt/lathe` → `--setting-sources project` 把 Lathe 自己的 CLAUDE.md 灌进目标仓库的分诊上下文 | 代码走查 |

~~已修复~~（B2-3）：分诊改在中立目录执行（默认 `$TMPDIR/lathe-triage`，位于任何项目树之外，`Pipeline.TriageDir` 可覆盖）。

### 1.3 数据残留

- `repos` id=241（`acme/member-repo`，user 535 的占位行）—— 无真实仓库，建议清理
- ~~task #217（SMOKE-2 排队尸体）~~ —— 已清理，无需处理

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

1. **§5 修复回路**（最高价值）：验证失败 → `--resume` 续跑（session id 已落库，比新会话省上下文且 agent 记得自己的思路）→ 再验证，限 N 次。#492 这类"只写测试没实现"的单子有了回路大概率自愈。
2. **重试语义修复**：retry 改为派发已存在的任务行，而非新建。
3. **启动 reconcile**（§6.4 的单机退化版）：启动时 `queued` 重新入队、`in-flight` 标记 `interrupted` 可一键续跑。不需要真持久化队列。
4. **前置失败落痕**：pre-flight 错误一律走 `p.fail`。

### 3.2 成本与效率 —— token 就是钱

5. **成本核算面板**：`CostUSD/DurationMS/NumTurns` 已解析落库（pipeline.go:478 + migration 0009），缺聚合视图：每任务/每 issue/每日成本、分诊 vs 实现占比。这是"什么单子值得交给 agent"的决策数据。
6. ~~**模型路由**~~（已交付，B2-2）：`LATHE_TRIAGE_CHANNEL` / `LATHE_IMPLEMENT_CHANNEL` 配 cc-switch 通道名，pipeline 按阶段以 `LATHE_AGENT_CHANNEL` 注入子进程，wrapper（`scripts/claude-cc-switch`）从 `~/.cc-switch/config.json` 现取该通道的 BASE_URL/TOKEN/MODEL；通道不存在时告警回落激活通道。未配置则全局跟随 cc-switch 激活通道。
7. ~~**分诊目录中立化**~~（已交付，B2-3）。

### 3.3 生命周期与现场管理

8. **worktree 收割机**：失败现场 TTL 回收（解决磁盘占用；同名撞车已由 Create 尸体回收覆盖，本条只剩 TTL 清理价值）。
9. **通知闭环**：任务终态（失败/待 review）接 `internal/mail` 发 `notify_email` —— 不用盯面板。
10. **GateMode 接线**：`gate=manual` 时验证通过后停下、人工确认再推 PR（"开 PR 前让我看一眼"）。

### 3.4 集成深度 —— 从手动触发到事件驱动

11. **webhook 联动加深**（ingress 已有）：标签驱动接单（如 `lathe:go`）、issue 取消联动取消任务。
12. **PR 生命周期回流**（§7 已设计）：(a) PR review comment → `review_feedback` → resume 续跑修复；(b) PR merged → 任务 `merged` + worktree 回收；(c) base 分支前进自动 rebase。

### 3.5 安全与隔离 —— 自用也要防手滑

13. **agent 进程 env 白名单**（最吓人的一条，优先修）：现在 agent 子进程能 `env` 到 `LATHE_ADMIN_TOKEN`。
14. **执行隔离**：中期方向 bubblewrap/容器；P1 分期里的 per-task compose 隔离仍欠着。

### 3.6 度量与迭代

15. **平台自度量**：成功率、各环节时长分布、失败分类直方图 —— 迭代平台本身时回答"改动有没有用"。
16. **跨任务记忆**：失败 digest 入库，同 issue 重试时注入"上次死在哪"。与修复回路互补（回路管单次任务内，记忆管跨任务）。

## 4. 建议批次

| 批次 | 内容 | 理由 |
|---|---|---|
| **B1** | 修复回路（1）+ 重试语义（2）+ 启动 reconcile（3）+ env 白名单（13）+ EventSink UTF8 修复 | 闭环自愈 + 堵安全洞 |
| **B2** | ~~模型路由（6）~~ + ~~分诊目录（7）~~ 已交付；剩成本面板（5）+ 通知（9）+ worktree reaper（8） | 日常用得爽 |
| **B3** | GateMode（10）+ webhook 联动（11）+ PR 回流（12） | 自动化加深 |
| **持续** | 度量（15）先行一点，每批落地后看数据决定下一批 | 数据驱动 |

## 5. 未决（继承 02-design §9 并增补）

- [ ] 多用户下同仓库的并发改动冲突（继承）
- [ ] P2 租户隔离安全模型（继承）
- [ ] 准入档位实测校准（继承）
- [ ] `light` 档是否强制要求新增测试（继承）
- [ ] heavy 档磁盘余量纳入闸门（继承）
- [ ] `cmd/lathe-runner` 骨架：推进 P3 还是删除（新增）
- [ ] 修复回路的次数上限与升级策略：N 次失败后是放弃还是换模型/换思路重试（新增）
- [ ] 跨包破坏的回归覆盖：包级收敛后跨包破坏靠仓库自己的 CI，平台是否要可选的"全模块回归"开关（新增）
