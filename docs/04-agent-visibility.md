# Lathe — 04 · Agent 执行可见性

> 2026-08-17 · 状态：已实现（migration 0009 / EventSink / events 端点 / 详情页摘要卡+日志面板）

---

## 1. 问题

任务跑完（或跑了半小时还在跑），人不知道 agent 干了什么。现在界面上唯一的
可见性是状态机的粗粒度迁移（triaging → implementing → verifying），以及失败
时的一封回帖。ssh 到节点上看 `claude` 输出不是产品形态。

诉求拆成两层，解法不同：

| 层 | 诉求 | 数据来源 |
|---|---|---|
| **L0 过程可见** | 执行中与执行后，能看到 agent 每一步在干什么（读了什么文件、跑了什么命令、输出了什么） | stream-json 事件流（**已有，被丢弃**） |
| **L1 结论可见** | 完成后有一份"改了什么、为什么、证据是什么"的交付摘要 | 终局 result 事件的 Text（**已有，未落库**） |

关键事实：这两层需要的数据 **agent 驱动已经全部在产出**，只是 runner 解析完
即丢。问题不是"让模型多产出一份文档"，而是**把已有的产出接通到前端**。

---

## 2. 否决方案：提示词要求模型写报告文件

最初设想：prompt 里要求模型把报告写到某个文件夹，前端按任务 ID 读。否决，
四个理由：

**① 可观测性不能建立在模型自觉上。**
模型可能忘写、写一半、路径写错、格式漂移。Lathe 的核心主张是"证明改动有效"
（README），而证明手段本身必须是确定性的工程管道，不能依赖被观察者自证。

**② PR 污染。**
`WorktreeManager.Commit` 是 `git add -A`（internal/runner/worktree.go）。报告
文件写在 worktree 里的任何位置（如 `.lathe/report.md`）都会被提交进 PR，
污染 diff 与仓库历史。要排除就得给 commit 加特例逻辑 —— 为一个本可避免的
问题引入永久的特殊路径。

**③ 写 worktree 外的路径撞权限模型。**
`acceptEdits` 权限模式下，项目目录外的写入会被 Claude Code 权限层拦截
（表现为 result 事件里的 `permission_denials`）。绕开它要么上
`bypassPermissions`（把一个已收敛的风险面重新打开），要么接受偶发失败 —
— 两种都不可接受。

**④ 冗余的双事实源。**
模型口述的报告与真实执行过程可能不一致（它说自己跑了测试，实际没有）。
stream-json 事件流是**执行实况**，Result.Text 是终局自述；让模型再写一份
文档，等于花双份 token 引入第三个可能互相矛盾的事实源。

---

## 3. 采纳方案：事件流落库 + 终局摘要上卡

### 3.0 现状盘点（大部分积木已备好）

| 积木 | 状态 |
|---|---|
| `agent_events` 表（migrations/0005） | ✅ 已建好。bigserial id 作 SSE/轮询游标、`phase`/`kind` CHECK 约束、append-only，注释里写的就是本方案 |
| `agent.Digest`（integration/agent/digest.go） | ✅ 已能把 stream-json 事件提炼成可读 Entry（text/thinking/tool_use/tool_result/result/raw），body 截断 4KB |
| `RunParams.OnEvent` 回调 | ✅ 驱动已支持逐事件回调 |
| **pipeline 接线** | ✅ 已实现：分诊/实现两次 Run 均接 EventSink（runner/eventsink.go） |
| **store 写入方法** | ✅ 已实现：`InsertAgentEvents` + `AgentEventsAfter`（store/agents.go） |
| **API 端点** | ✅ 已实现：`GET /api/tasks/{id}/events` |
| **前端日志面板** | ✅ 已实现：TaskDetail.vue 摘要卡 + 分阶段日志面板 |

所以这不是新系统，是**一次接线工程**。

### 3.1 数据通路

```
claude CLI ──stdout NDJSON──> agent.Driver ──OnEvent──> EventSink（新增）
                                                              │ Digest 提炼
                                                              ▼
                                                      agent_events 表
                                                              ▲
前端 TaskDetail 日志面板 <── GET /api/tasks/{id}/events ──────┘
```

### 3.2 EventSink：批量落库器

`OnEvent` 在驱动的读协程里**同步**执行（agent.go 注释明确要求实现轻量），
逐行 INSERT 会把 DB 往返串进 stdout 读取回路。因此加一个每任务一个的
EventSink：

- 内部：有界 channel（256）+ 单 flush 协程，每 200ms 或攒满 20 条批量 INSERT
- OnEvent 只做 `Digest` + 非阻塞投递
- 缓冲满时：优先丢弃 `thinking`（界面价值最低、量最大），计数；任务结束时
  补一条 `kind='raw'` 的"溢出 N 条"记录，不静默丢
- **成功与失败路径都必须 drain**：`p.fail()` 时缓冲区里的事件恰是排障最关键
  的现场，比成功路径更不能丢
- phase 取值：`triage` / `implement`（`review` 预留给 --from-pr 二轮，表已预留）
- 验证器步骤输出同时写一条 `kind='verify_step'`：与 verifications 表同源，
  那张表存结构化结果给红-绿判定，这里存人读时间线

### 3.3 API

```
GET /api/tasks/{id}/events?after=<id>&limit=<n>
→ { "events": [...], "last_id": 12345 }
```

- 增量拉取：`WHERE task_id=$1 AND id>$2 ORDER BY id LIMIT $3`，命中 0005 已建
  好的 `(task_id, id)` 索引
- 权限：与 `taskDetail` 同一原则 —— 不是自己的任务按 404 处理
- SSE 暂缓：轮询（2s）已满足"实时滚动"的体感；游标 `id` 严格单调，将来换
  SSE + Last-Event-ID 时客户端协议不变，只是传输升级

### 3.4 前端（TaskDetail.vue）

- **执行日志面板**：按 kind 渲染 —— `text` 正文 markdown；`tool_use` 工具名
  + 参数摘要一行；`tool_result` 默认折叠；`thinking` 灰显折叠；`result`
  渲染成徽章行（耗时 / 成本 / turns）；`verify_step` 带红绿状态色
- 任务进行中 2s 轮询增量（带 `after=last_id`）；进入终态后再拉一次收尾即停
- 阶段分节：按 `phase` 分组（分诊 / 实现 / 验证），当前进行中的阶段置顶展开

### 3.5 终局摘要落库（L1）

模型自述的交付小结其实**已经存在**：`ImplementPrompt` 已要求"完成后用一段话
说明你改了什么、为什么这样改"，这段话就是 `Result.Text`，目前只被截断塞
进 commit message 和 PR body，没有单独存给界面。

- **迁移 0009**：tasks 加列 `agent_summary text`、`agent_cost_usd numeric`、
  `agent_duration_ms bigint`、`agent_num_turns int`（不用 payload jsonb：
  这是详情页主展示字段，值得一等列）
- 实现阶段成功后（含 fail 路径，有 result 就存）落这四列
- prompt 微调：`ImplementPrompt` 尾部的一段话说升级为固定四小节 ——
  **改了什么 / 为什么这样改 / 涉及的关键文件 / 自验证证据**（跑了什么测试、
  红绿结果）。解析逻辑不变，仍整段取 Result.Text
- 前端详情页顶部加**摘要卡**：四小节 + 成本/耗时徽章，下面才是日志面板

---

## 4. 明确不做（及原因）

| 不做 | 原因 |
|---|---|
| 让模型写任何文件作为数据通路 | §2 四条 |
| 归档原始 NDJSON | 单行上限 16MB（init 事件塞全量工具清单），无人读且撑爆磁盘；digest.go 的立场沿用 |
| SSE 推送 | 轮询够用；游标语义已预留，升级不改协议 |
| L2 富报告（完整 markdown 交付报告） | L1 四小节覆盖当前诉求。真需要时由 **runner 用 agent_events + diffstat + verifications 确定性渲染**，仍然不让模型写文件 |
| 日志保留策略 | 每任务数百到上千行、body ≤4KB，量级可接受；等真成问题再加按终态 + 时长的清理任务 |

---

## 5. 多节点演进（预留，不实现）

`lathe-runner` 独立节点形态落地后，EventSink 不能直接写控制面 DB，事件需随
runner → 控制面的上报通道回传（复用心跳/租约连接）。对 API 与前端**完全透明**：
游标仍是控制面 DB 的 bigserial id，轮询协议不变。因此本方案不阻碍 D3 横向
扩展。

---

## 6. 工作量分解

| 件 | 改动 |
|---|---|
| store | `InsertAgentEvents(batch)` + `EventsAfter(taskID, after, limit)` 两个方法 |
| runner | EventSink（约百行）+ pipeline 三次 Run 传 OnEvent + 验证器 verify_step |
| 迁移 | 0009：tasks 加四列（0008 被仓库级排除目录占用） |
| httpapi | 一个端点（权限复用 taskDetail 原则） |
| 前端 | TaskDetail.vue 一个面板 + 一张摘要卡 |
| prompt | ImplementPrompt 尾部四小节化 |

全部为确定性工程，无模型行为依赖；每一层出故障都降级而不阻断流水线
（落库失败只告警，沿用 `persistVerifications` 的既有立场）。

---

## 7. 第二条数据源：subagent 的内部活动（0014）

### 7.1 问题：21% 的活动不在 stdout 上

§3 的通路假设「agent 干的事都在 stdout 的 stream-json 里」。这个假设对
subagent 不成立：agent 用 `Agent` 工具派活时，父会话的 stdout 只有

- 一条 `tool_use`（`Agent <description>`），和
- 一条 8–13KB 的汇总 `tool_result`

中间那几十步 —— 它翻了哪些文件、试错在哪、为什么得出这个结论 —— **一条都没有**。

本机历史数据的体检结果：

| 指标 | 值 |
|---|---|
| agent 会话数 | 16 |
| 其中派生过 subagent 的 | 7（44%） |
| 主 transcript 总行数 | 4072 |
| subagent 内部行数（此前不可见） | 1093（21%） |
| 单轮最差情况（cr-1363 实现轮） | 228 / 523 行不可见（44%） |

也就是说「任务详情页在派活期间是黑盒」不是边缘情况，而是每两次跑批就出现
一次。

### 7.2 数据源：transcript 文件

claude 把 subagent 的记录落在

```
~/.claude/projects/<cwd-slug>/<session-id>/subagents/agent-<agentId>.jsonl
```

`<cwd-slug>` 的规则是「路径里非字母数字的字符一律换成 `-`」（本机 22 个项目
目录逐一比对吻合；`.` 也被替换，因此 `/opt/X/.claude/y` 会出现连续两个 `-`）。
Lathe 恰好满足定位它的三个前提：session ID 自己预分配（`--session-id`）、
worktree 路径已知、且执行期间不换 cwd。

**与 stdout 通路并存，不取代它。** transcript 的 JSONL 是 Claude Code 的内部
格式，无文档、可能随版本变化。主可见性必须继续依赖 stdout —— 那是 CLI 的
公开契约，且只有 result 事件给出成本与轮数。读文件的一切失败都降级为
「少显示一些东西」（见 `internal/integration/agent/transcript.go`）。

### 7.3 三个必须处理的坑

1. **重复落库。** 修复轮走 `--resume`，用的是同一个 session ID，`subagents/`
   里留着上一轮的文件。从偏移 0 读会把上一轮已落库的事件再灌一遍。
   对策：`SubagentReader.SeekToEnd()` 在 sink 构造时把已存在文件的偏移移到
   末尾，只读本轮追加的内容。顺带避免首轮一次吐出几百条冲垮有界缓冲。

2. **半行。** claude 正在写入的行会被 Scanner 当成最后一个 token 返回。
   若就此消费，那行的后半截永远补不回来。对策：只在「已消费字节触到文件
   末尾」时才推进偏移。

3. **首行不是工具结果。** subagent 文件首行是派给它的任务描述
   （`type=user`，content 是纯文本）。走 `Digest` 的 user 路径会被误标成
   「工具结果」（那条路只认 `tool_result` 块）。对策：新增
   `kind='agent_start'`，它同时充当界面上的分组标题。

### 7.4 界面：兄弟块，不是父子连线

subagent 的事件在详情页收成一个默认折叠的子块，插在它首次出现的位置，
标题是派给它的任务描述。

刻意**没有**把子块挂到那条 `Agent` 调用下面：`agentId` 与父调用 `toolUseId`
的对应关系只存在于 transcript 的 `toolUseResult` 字段里，stdout 给不出。
与其按时间顺序猜一个父子关系，不如并排放 —— 子块标题与父调用那行显示的
description 是同一句话，人一眼能对上。真要连线，得**再读父会话的
transcript**（那里的 `toolUseResult` 带 `agentId`/`description`/`status`），
属于后续增量，不在本期。

### 7.5 顺带修掉的：工具调用配对

一次工具调用在事件流里是两条（`tool_use` 发起 + `tool_result` 结果）。
此前只取了结果那侧的 `tool_use_id`，发起侧的 `id` 没留 —— 两行各自漂着，
界面只看得出「调过 Bash」，看不出这步花了多久、成没成。这是执行日志最难
扫的根因，与 subagent 无关。

现在 `tool_use` 带上自身 id，前端按它把结果并进发起那条，给出 ✓/✗ 与耗时。
老数据没有该字段，配不上的退回两行平铺（降级而非崩溃）。

**耗时是粗粒度的**：`at` 取自入库时的 `now()`，而 EventSink 是 200ms 批量
刷库，同一批的发起与结果拿到同一个时间戳，误差约 ±200ms。因此只在 1s
以上才显示 —— 更低的区间与其显示一个自信的错数，不如不显示。要拿到真实
的单次工具耗时，得在事件里记**发生时刻**而非入库时刻（需要加列）。

### 7.6 这一期明确不做

| 不做 | 原因 |
|---|---|
| 节点图 / 小地图 / 相机跟随 | 无人值守批处理，人是事后看结论；两三个 subagent 撑不起一张图，缩进树足够 |
| 时间轴回拖（time travel） | 同上：没人坐在旁边拖进度条。phase 折叠已经回答「现在跑到哪」 |
| 把 stdout 换成读 transcript | 格式无文档、可能变；换过去等于把主可见性押在内部实现上（§7.2） |
| subagent → 父调用的连线 | 需要多读一份父 transcript，收益是「对上一句话」，而标题已经能对上（§7.4） |
| 归档 transcript 原文 | 它本来就留在 `~/.claude` 下，且不随 worktree 清理消失；init 事件已记 sessionId + cwd，任何时候都能回到原始现场 |
