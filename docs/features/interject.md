# 运行中插话（Interject）

> 定稿：2026-11-11 · 分支 `feat/interject` · 功能线 f01（竞品参照：Multica「运行中与 agent 对话」）
> 本文是评审通过的 PRD（第五轮修订）原文的定稿版，**结论先行**；评审往来（R1–R5 的逐条答复与取证物）
> 不进仓库，留在功能线证据目录。§10 记录实现与验收结果（含未完成项，如实写明）。
> 前置阅读：[02-design.md](../02-design.md)（状态机 / 验证设计）、[04-agent-visibility.md](../04-agent-visibility.md)（事件流）。
> 文中 `evidence/f01/...` 指功能线证据目录 `/opt/lathe-wt/evidence/f01/`（不在仓库内）。

---

## 0. 一句话

任务在跑（`implementing`，以及进入 `verifying` 之后还会跑的修复回路）、agent 进程还活着的时候，
属主能往这个会话里**追加一句话**（「别动 migration」「这个函数别改名」）。话被送进**正在跑的那次 claude 进程**，
同时落库、在任务详情页可见；送不进去要有明确的失败态与人读原因，消息一条都不许静默丢失，
更不许把管线卡住或者让验证被绕过。

---

## 1. 问题与目标

### 1.1 为什么 Lathe 需要它

Lathe 的定位是「Linear issue → **已验证的 PR**」——价值全在验证链上，所以实现阶段允许 agent 跑 10~45 分钟
（`agent.go:42` 的 `idleTimeout` = 12 分钟、`LATHE_AGENT_TIMEOUT` 默认 45 分钟）。这段时间里人**完全没有干预手段**：

- 想改方向（「走方案 B，不要新表」）只能等它跑完 → 看结果 → 不满意 → `POST /api/tasks/{id}/retry` 从头再跑一轮。
  一整轮的成本（`total_cost_usd` 实测单轮 $0.08 起、真实任务几美元）与时间全部沉没。
- 想提醒一条硬约束（「本仓 `make test` 必须 `-p 1`」）只能写在 issue 里，而 issue 在**分诊之后**就不再被重读
  （`pipeline.go:600` 的 `stageImplement` → `implementPrompt`（`:737`）只用 `rc.issue.Context()` 构造提示词）。
- 结果就是：**人越不放心越不敢放任它跑，越不敢放任它跑就越要盯着**。插话把「盯着」变成「能插手」。

这也是「已验证 PR」定位的一部分：插话**不削弱验证**——验证阶段的输入仍然是 diff 本身，插话只是让 diff
在第一次就长成对的样子，减少「验证红 → 修复回路 → 再验证」的空烧（`pipeline.go:1009` 的 fix 回路）。

### 1.2 目标

| # | 目标 | 不做会怎样 |
|---|---|---|
| G1 | 属主在任务详情页发一句话，**注入正在跑的实现会话** | 「干预」= 杀掉重来，成本翻倍 |
| G2 | 消息**先落库再投递**，任务详情页可见（独立面板，§3.5），失败可查且带人读原因 | 丢消息；事后没法复盘「人当时说了什么」 |
| G3 | 会话已结束 / 未运行 / 驱动不支持 → **明确状态码与提示**，不假装成功 | 人以为说出了，实际没进模型 |
| G4 | 投递**不阻塞**流水线（agent 读协程、状态机、验证队列都不等它） | 插话接口抖动 → 任务假死 |
| G5 | 插话**不触碰**验证判定：`verifications` 表、红-绿证据、档位都照旧 | 一旦能影响判定，整个产品的可信度没了 |
| G6 | 只属主可插话，非属主 **404**（不是 403） | 用状态码泄漏任务存在性 |

### 1.3 与「不猜」边界的关系

插话是**人给的信息**，不是 agent 猜的。所以它是「单子不明确就回帖提问并停下」（AGENTS.md §1.2）的**补充通道**：
单子不明确时 agent 仍然要停（状态机不许 `implementing → blocked_spec`，见 `state.go:106`），插话不能把它变成
「边聊边猜」。**插话只增不减**：它不推进任何状态、不改任何状态、也不解除任何阻塞。

---

## 2. 现状取证

### 2.1 代码现状（文件:行号）

| 事实 | 位置 |
|---|---|
| agent 驱动一次执行一个 prompt，`--print --output-format stream-json`，**stdin 完全没接**（`cmd.Stdout/cmd.Stderr` 设了，`cmd.Stdin` 未设） | `internal/integration/agent/agent.go:202`（Run）、`:249`（Stdout/Stderr）、`:359`（buildArgs） |
| 参数构造只有 `--print/--output-format/--verbose` + `--resume/--session-id/--from-pr` + prompt 作为位置参数 | `internal/integration/agent/agent.go:359-384` |
| 事件是**只读解析**：`parseStream` 逐行扫 stdout，回调 `OnEvent`；多条 result 时**后者覆盖前者**（`result = &Result{…}` 每次重新赋值） | `internal/integration/agent/agent.go:386-436`（覆盖在 `:420`） |
| 读协程里先 `idle.touch()` 再调 `OnEvent`：任何一条 stdout 事件都喂看门狗 | `internal/integration/agent/agent.go:292-297` |
| `Run` 收尾时**先判 `idle.firedNow()`** 再判 ctx：进程若在有 result 之后被看门狗杀掉，报的是 `ErrIdle`（即便 result 是 success） | `internal/integration/agent/agent.go:332-335` |
| stdout/stderr 都用 `os.Pipe`（`*os.File`）接子进程，**不用** `io.Reader` —— 现有代码已经知道「非 `*os.File` 会让 exec 起拷贝协程」这一类坑（注释讲的是 stdout EOF） | `internal/integration/agent/agent.go:231-252` |
| Go `os/exec`：`cmd.Stdin` 不是 `*os.File` 时会起一条拷贝协程，`cmd.Wait` **等这条协程结束**；写端不关则 Wait 永不返回，进程被杀也不返回 | 标准库语义；评审期复现见 `evidence/f01/review-r1/stdin-probes/output.txt` |
| 驱动接口只有 `Run(ctx, RunParams)`，没有「运行中」这一概念 | `internal/runner/pipeline.go:55-58` |
| 实现阶段入口 `stageImplement` → `runAgent("implement", …)` | `internal/runner/pipeline.go:600`、`:698` |
| 统一 agent 调用入口 `runAgent`：构造 EventSink + RunParams + `Agent.Run` | `internal/runner/pipeline.go:766-800` |
| 修复回路也走 `runAgent`，phase 名 `fix-N` | `internal/runner/pipeline.go:1009` |
| 事件落库：`InsertAgentEvents`（COPY）、`AgentEventsAfter`（游标读取，JOIN tasks 按属主过滤） | `internal/store/agents.go:34`、`:74` |
| `agent_events` 表：`phase` CHECK 只认 `triage/implement/verify/review/push` 或 `fix-N`；`kind` CHECK 只认 9 个值 | `migrations/0015_agent_events_push_phase.up.sql`、`0014_agent_events_subagent.up.sql`（已在 f01 测试库 `lathe_f01` 用 `pg_get_constraintdef` 核对过） |
| 事件流 UI：`PHASE_META` 固定 5 个 phase，`AgentEventItem.vue` 按 `kind` 渲染 | `web/src/views/TaskDetail.vue:92-98`（PHASE_META）、`:197`（未知 phase 的兜底顺序）、`web/src/components/AgentEventItem.vue:1-60` |
| 事件投递是非阻塞有界缓冲（sinkCap=256，满了丢并计数） | `internal/integration/agent/batchsink.go:24-45`（参数）、`:90-110`（Offer 丢弃） |
| 属主判定：`Tasks.Get` 后比 `tk.UserID != CurrentUser(r).ID` → 404 | `internal/httpapi/api.go:143-156`（taskEvents）、`:598-607`（cancelTask） |
| 任务详情/事件路由 | `internal/httpapi/api.go:70-85` |
| 驱动层 env 白名单（`sanitizedEnv`，白名单外一律不传） | `internal/integration/agent/agent.go:477-500` |
| 会话 ID 在执行前就落库（崩溃后可 `--resume`） | `internal/integration/agent/agent.go:157-166`、`internal/runner/pipeline.go:650-664` |
| 一个任务一个实现会话；`tasks.agent_session_id` 单列，修复轮会覆盖 | `internal/store/agents.go:108`（SetAgentSummary）、`internal/task/machine.go:396`（SetSessionID） |
| 状态机 `implementing` 出边只有 `verifying/queued/failed/cancelled` | `internal/task/state.go:106` |
| 验证落库 `verifications` 与红-绿报告 | `internal/runner/pipeline.go:1388`（persistVerifications） |

### 2.2 CLI 能力实测（这是方案能否成立的前提，已做完）

`claude --help`（2.1.322）明示 `--input-format stream-json` 是「realtime streaming input」，另有
`--replay-user-messages`「Re-emit user messages from stdin back on stdout for acknowledgment」。
我用隔离 `HOME` + 临时 git 仓库跑了 8 组实验，结论与原始记录见 `/opt/lathe-wt/evidence/f01/cli-probes.md`。关键三条：

1. **进行中注入会被吸收**：首条 prompt 让模型 `Bash sleep 8`，3 秒后写第二条 user 消息 → 只产生**一条** result，
   而文本里包含了注入内容（`'INJECTED\n\n1'`）。也就是说：**注入不是新 turn，是当前 turn 的补充输入**。
2. **注入可以被逐条确认**：开 `--replay-user-messages` 后，stdout 会出现 `type=user`、`isReplay` 键、
   `uuid` 与发送时一致的回显（我发 `1111…`/`2222…`，回显完全一致）。**只能确认 CLI 收到了，不能确认模型读了**。
3. **stdin 不 close，会话仍然可 `--resume`**：流式跑完一轮后 `--resume` 同一 session 成功答出暗号。
   这条决定了「插话不破坏 review_feedback 续跑」。

另外两条直接影响实现的事实：

4. **stdin 不 close 时 CLI 是否退出——行为不稳定，设计不得依赖它（R1 勘误）**。
   初稿写的是「不 close 就一直等，所以常开安全」，这个结论**错了**，而且推论方向也错了：
   评审用真实 CLI 2.1.322（隔离 HOME、临时仓库）hold 住 stdin 复测：开 `--replay-user-messages`、hold 90s 时，
   进程约 **90.3s** 才退（即在等 EOF）；其它参数组合在 result 后约 4s 自行退出。两种都出现过，所以：
   - 若 Lathe 常开 stdin、CLI 又恰好在等 EOF，12 分钟看门狗会杀掉一个**已经给出 success result 的**进程，
     `Run` 先判 `idle.firedNow()`（`agent.go:332`）→ 报 `ErrIdle`，成功被报成失败；
   - P1 还表明：result 之后、stdin 关闭之前写入的消息会**开启新 turn、产出第二条 result**。
   唯一稳定成立的是 P5：**close 之后已写入的消息仍会被吸收、进程正常退出**。因此本设计改为
   「**首条 result 即关 stdin**」（§3.2 第 3 条），不再依赖 CLI 对未关 stdin 的任何行为。
5. 开 `--replay-user-messages` 后 stdout 会出现 `type=user`、`isReplay:true` 的回显事件（首帧 prompt 本身也会回显）。
   **R1 勘误**：初稿说「`Digest` 会把它当成 `tool_result`」，这是错的 —— 回显的 `content` 是**块数组**、块类型是
   `text`，走的是 `digestMessage` 的块分支（`digest.go:180-187`），产出的是 **`KindText`**；初稿引用的
   `digest.go:170-176` 只是「content 为字符串」的分支。用现有 `parseStream`+`Digest` 跑 fixture 的实测
   （`evidence/f01/review-r1/code-probes/output.txt`）：`user->text:2`、`command_lifecycle->raw:5`、`tool_result` **0 条**。
   **真实危害比初稿说的更重**：人插的那句话会以 **agent 自己说的正文**（`text` 行，走 markdown 渲染）出现在事件流里
   —— 审计上把「人说的」记成了「agent 说的」。所以必须过滤。
6. `type=command_lifecycle`：**只要用了 `--input-format stream-json` 就会出现**（评审实测不开 `--replay-user-messages`
   也有，每次运行 3 条；本 fixture 开了 replay、两次输入共 5 条）。初稿把它的出现条件写成「开 replay 之后」，是错的；
   但结论不变：`Digest` 的 `default` 分支会把它退化成 `kind=raw`，所以过滤规则写成**白名单**
   （只放行 `system/assistant/result` 与**非 replay 的** `user`），而不是只挡 `isReplay`。
   **过滤发生在 `idle.touch()` 之后**：被丢弃的事件照样喂看门狗（它们是「进程还活着」的真实信号）。
7. **多条 result 时的计费字段语义**（R1 新增，fixture 两条 result 实测）：`total_cost_usd`（0.078858 → 0.0976604）与
   `modelUsage`（第二条 `inputTokens:6 = 2+4`、`cacheCreationInputTokens:18978 = 15746+3232`）是**进程累计**；
   `usage`（第二条 `input_tokens:4`）、`duration_ms`（2759 / 2499）、`num_turns`（1 / 1）是**单 turn**。
   §3.2 第 5 条据此定义一次 `Run` 出现多条 result 时 `Result` 怎么取值。
8. **P10：`--resume <sid>` + `--input-format stream-json` + 运行中注入（R2 新增，回应 B5）**。
   P6 只证明了「流式跑完之后能被 `--resume` 续上」，没有证明「`--resume` 这次运行**本身**也走流式输入」。
   而装配了 Interjections 之后，`fix-N`（`pipeline.go:1009`，`fixResume=true`）、PR review 二轮
   （`implementPrompt`，`pipeline.go:744-745`）、验证失败后的重试（`:750-753`）都是 `--resume` + 流式输入。
   评审期用真 CLI 2.1.322 实测（隔离 `HOME`、临时仓库、`bypassPermissions`，脚本与原始输出见
   `evidence/f01/review-r2/resume-stream/`，`probe.py` + `summary.json` + 两段 `.jsonl/.meta.json`）：
   - a 段：`--session-id <sid>` + 流式输入，让模型记住 `ZEBRA42` → result `OK`；
   - b 段：`--resume <sid> --input-format stream-json --replay-user-messages`，首帧让模型 `Bash sleep 8`，
     **3.004s** 时写入注入帧「最终回答里必须包含 `MARK-6a6b4ed4`」；结论：
     注入帧的 uuid 出现在 `isReplay` 回显里；**只有 1 条** result（15.121s），文本 `'ZEBRA42\n\nMARK-6a6b4ed4'`
     —— 既记得 resume 之前的会话、又吸收了运行中注入；`session_id` 与 a 段相同；
     首条 result 时关 stdin，进程 **0.277s** 后退出码 0；临时仓库 `git status --porcelain` 为空。
   - b 段事件形状：`command_lifecycle` 6、`system` 3、`user(isReplay)` 2、`user`（非 replay，真·工具结果）1、
     `assistant` 2、`result` 1 —— 与 P9 同一族形状，§3.2 第 4 条的白名单过滤原样适用。
   这只是一次裸 CLI 探针，**不替代** AC-14 用 Lathe 驱动跑的 resume 段与 AC-22 的端到端证据。
9. **哪些 `raw` 行是 f01 要消除的、哪些是既有行为（R3 新增，回应 R3 B1）**。用**今天的** `parseStream`+`Digest`
   （cff5332，`go test -overlay` 注入探针，worktree 无改动；`evidence/f01/review-r3/raw-probe/output.txt`）跑三份真实捕获：

   | 捕获 | `command_lifecycle→raw` | `user→text`（回显冒充正文） | `system/<非 init>→raw` |
   |---|---|---|---|
   | `review-r2/resume-stream/a-new-session.jsonl` | 3 | 1 | **18**（`thinking_tokens`） |
   | `review-r2/resume-stream/b-resume-stream-inject.jsonl` | 6 | 2 | **2**（`task_started` 1、`task_notification` 1） |
   | `fixtures/replay-user.ndjson` | 5 | 2 | 0 |

   `system/<非 init>` 变成 `raw` 是 `digestSystem`（`digest.go:87-91`）的**既有、刻意**行为（注释：「api_retry 之类的系统通知
   ……保留原文而不是丢弃」），与 `--input-format stream-json` 无关，非流式运行同样产生（`agent.go:36-40` 的注释讲的就是
   `thinking_tokens`）。**f01 不过滤、不改它们**：§3.2 第 4 条的白名单放行 `system`，`digest.go` 一行不改。
   f01 消除的污染**只有两类**：`command_lifecycle→raw`、`user(isReplay)→text`。AC-4c、AC-22 的断言据此只针对这两类，
   不再写「`kind='raw'` 为 0」（R2 的 AC-22 那一行在真实运行上不可能成立，评审正确）。
10. **`--resume` 一个不存在的会话也会给出 `result`（R3 新增，影响 §3.4「补投结局」的判据）**。
   真 CLI 2.1.322，隔离 `HOME`，位置参数与 `--input-format stream-json` 两种启动各一次（`evidence/f01/review-r3/resume-missing/`）：
   都输出**一条** `result`：`subtype=error_during_execution`、`is_error=true`、**`num_turns=0`**、`total_cost_usd=0`，
   stderr `No conversation found with session ID`，退出码 1。`Run` 对此返回 `(result, nil)`（`agent.go:349-352`：非零退出但有
   result 且 `IsError` → 以 result 为准）。
   含义：**「有 result」不等于「prompt 进了模型」**。若补投行凭「有 result」就判 `delivered`，人会看到「已送达」而模型
   一个字也没读到。§3.4 的判据据此改为 `NumTurns ≥ 1`：这种情形下行退回 `queued`，随后由同一任务的下一次 `runAgent` 补投，
   或（任务因此 `p.fail`）由结算判 `failed`「任务已失败」——两种都是如实的结局。
   **顺带观察（既有行为，f01 不改、不在范围内）**：`stageImplement` 的 resume 降级只在 `err != nil && resumeFlag` 时触发
   （`pipeline.go:703`），而真实 CLI 的「找不到会话」走的是 `err == nil && res.IsError`，于是会落到 `implRes.IsError → p.fail`
   而不是降级；既有测试（`memory_test.go:351`、`pipeline_resume_test.go:213`）用 `errs: []error{errors.New("No conversation found…")}`
   模拟，与真实形状不同。这与插话无关，写进 PR 描述建议单独立项，f01 不顺手修（改它会改变管线的失败语义）。

**真实捕获（本轮新取证，13 行）**：`/opt/lathe-wt/evidence/f01/fixtures/replay-user.ndjson`
（两次 `type=user` 注入，其中一次带自定义 uuid `2222…`；已用正则扫过：不含任何 token 形态的串，
`cwd` 是 `/tmp/f01probe/repo` 这个临时仓库，可安全入库）。事件形状统计：

| type | 条数 | isReplay |
|---|---|---|
| `command_lifecycle` | 5 | 无 |
| `system`(init) | 2 | 无 |
| `user` | 2 | **true** |
| `assistant` | 2 | 无 |
| `result` | 2 | 无 |

对应 `agent_events.kind` 时（现有代码实测，非推算）：2 条 user 回显变成 2 条**冒充 agent 正文的 `text`**，
5 条 `command_lifecycle` 变成 5 条 `raw` —— 一轮就多 7 条错误归属/噪声。这条 fixture 直接进 AC-4。

### 2.3 现状的硬约束（决定了方案的形状）

- **不引入新依赖**（任务书 + AGENTS.md §4）：不能用 websocket 库、不能加消息队列。DB 是唯一共享通道。
- **不改状态机**（`internal/task/state.go`）：插话不进状态机，`implementing` 前后状态不变。
- **不改「非属主 404」**：新增接口沿 `cancelTask` 的写法。
- **驱动只认 stdout**：`Driver.Run` 是同步阻塞到进程退出的，运行中的句柄不在任何可查询的地方 —— 这是本设计的
  **核心改造点**，且要控制面尽量小。

---

## 3. 方案

### 3.0 总体形状（三层，各层职责单一）

```
POST /api/tasks/{id}/interject
        │  ① 属主判定（非属主 404）、长度上限、限流
        │  ② 同一事务内：按任务加锁读 state → state ∉ {implementing, verifying} 直接落 failed 并 409；
        │     否则先落库（queued）—— 脱敏在这里做一次（§3.7，唯一脱敏点）
        ▼
   task_interjections 表（先落库；每次写入推进 rev 游标，§3.1）
        │  ③ 同进程内存注册表里查这个 task 的「活会话」→ 直接投递（不经过 DB 轮询）
        │     查不到 / 会话已收尾（首条 result 后即关 stdin）→ 202，状态保持 queued
        ▼
   agent.Driver 的 stdin（os.Pipe 写端，stream-json user 帧）
        │  ④ --replay-user-messages 的 uuid 回显 → 读协程只发内存信号 → Submit 协程落 delivered
        ▼
   queued 的消费方：下一次 runAgent 开场补投（进模型）；或同一个结算函数 settleQueued 判 failed（带人读原因），
   它有四个不依赖 Execute 的触发点：Execute 收尾 defer / GET /interjections 读时结算 / 周期清扫 / 启动清扫（R3，§3.4）
   前端：独立的插话面板，用 GET /api/tasks/{id}/interjections?after_rev=R 增量轮询（§3.3）
```

设计上最关键的取舍：**投递通道用同进程内存，不用 DB 轮询**。理由：

- Lathe 现在是**单进程形态**（`docs/02-design.md` §6 的多节点是 P3 规划；`nodes` 表只有结构、没有消费方，
  见 `migrations/0001_init.up.sql:77-88`）。同进程直投是零延迟、零轮询的真实实现，只多一张审计表。
- 但「某任务正在哪个进程跑」在 DB 里**没有**权威记录（`tasks.node_id` 没有写入方），所以跨进程投递**现在做不了**。
  与其发明一套节点归属协议（那属于多节点 P3，不是本线的事），不如**显式承认边界**：
  注册表查不到 → `202`，消息留在 `queued`，界面显示「已记录，等下一次 agent 轮次时投递」。
- `queued` 的确切承诺（R1 改写，见 §9 第 5 条；R3 补全触发点，见 §9 R3 B2）：**要么进入模型（开场补投），要么以人读原因判
  `failed`，不存在第三种结局**；并且「判 failed」**不依赖 `Execute` 再次运行**：任务一旦处在不会再跑 agent 的状态，
  下一次有人读这一行（`GET /interjections`）时当场结算，没人读也最迟一个周期清扫间隔（5 分钟）内结算。
  不承诺「一定送达」—— 验证一次通过时根本没有下一轮。

> **替代方案（已否决）**：DB 轮询 —— `runAgent` 起一个 500ms 的 ticker 查 `task_interjections`。
> 否决理由：每个在跑的 agent 都要多一条常驻轮询；轮询间隔决定延迟；且它和内存注册表解决的是同一个问题
> （同进程投递），却以 5~10 倍的写压力换同一结果。**保留**：跨节点的场景（现在不存在）将来若要支持，
> 把注册表换成「DB 租约 + LISTEN/NOTIFY」即可，接口形状不变 —— 这条写进 §3.7 的演进注记。

### 3.1 数据模型（迁移 0032，号段 0032–0034 内，本 PRD 只用 1 条）

**0032_interjections.up.sql**

```sql
-- 0032_interjections — 运行中插话：人给正在跑的 agent 追加的一句话
--
-- 为什么单独一张表而不是复用 agent_events：
--   1. 两者的生命周期不同。agent_events 是 stdout 的提炼（append-only 的
--      「发生了什么」），而本表记的是「人要求什么」，它有一个**投递状态机**
--      （queued → delivering → delivered/failed），要能被人查到。
--   2. agent_events 的 kind CHECK 是「Digest 能产出的全集」（0014），插话
--      不是 stdout 的产物，混进去会打破那条不变的等式。
--   3. 审计视角不同：查 agent_events 是查「agent 做了什么」，查本表是查
--      「人干预过什么」。任务详情页要单独渲染这一段。
--
-- rev 是「这一行最后一次被写」的游标（前端增量轮询用，见 docs/features/interject.md）：
-- INSERT 取默认值，每次 UPDATE 由写入方显式 SET rev = nextval(...)。
-- 写入方都在 pg_advisory_xact_lock(<task_id>) 下取号并提交，所以同一任务内
-- rev 的大小顺序 = 提交顺序，游标不会跳过晚提交的小号。
-- （刻意不用触发器维护 rev：迁移只做 schema，AGENTS.md §6.4。）
CREATE SEQUENCE task_interjections_rev_seq;

CREATE TABLE task_interjections (
  id          bigserial   PRIMARY KEY,
  task_id     bigint      NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  user_id     bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  -- 人写的那句话，**已脱敏**（落库前过 runner 的两层脱敏，§3.7）。上限在 API 层
  -- （LATHE_INTERJECT_MAX_CHARS，默认 8192 个 rune），这里只做保底硬上限 16384。
  body        text        NOT NULL,
  -- 投递状态。text + CHECK 而非 enum（沿用 0001 的约定）。
  --   queued     已落库、尚未进入任何 agent 进程
  --   delivering 正在投递（实时写 stdin 中，或已拼进本轮 prompt、等本轮结束）
  --   delivered  已进入 agent 进程 —— 只代表「CLI 收到了」，不代表模型照做
  --   failed     不会进入模型；error 写明原因
  status      text        NOT NULL DEFAULT 'queued',
  -- failed 时是人读原因（CHECK 强制非空）；delivered 时可为 'no_replay_ack'（写入成功但没等到回显）。
  error       text,
  -- 投递时归属的 agent 会话 ID：事后能对上「这句话进了哪一轮」。
  session_id  text,
  -- 投递进的事件阶段（implement / fix-1 …），对应 agent_events.phase。
  phase       text,
  -- 实时投递时发给 CLI 的帧 uuid（回显确认用）；开场补投的行留 NULL。
  delivery_id uuid,
  created_at  timestamptz NOT NULL DEFAULT now(),
  delivered_at timestamptz,
  rev         bigint      NOT NULL DEFAULT nextval('task_interjections_rev_seq'),

  CONSTRAINT task_interjections_status_check
    CHECK (status IN ('queued', 'delivering', 'delivered', 'failed')),
  -- 时间戳与状态必须自洽：
  --   queued / delivering  没有 delivered_at，也没有 error
  --   delivered            有 delivered_at
  --   failed               有 error（人读原因）
  CONSTRAINT task_interjections_terminal_check CHECK (
    (status IN ('queued', 'delivering') AND delivered_at IS NULL AND error IS NULL)
    OR (status = 'delivered' AND delivered_at IS NOT NULL)
    OR (status = 'failed' AND error IS NOT NULL)
  ),
  CONSTRAINT task_interjections_body_len CHECK (length(body) <= 16384)
);

ALTER SEQUENCE task_interjections_rev_seq OWNED BY task_interjections.rev;

-- 查询模式一：前端增量轮询 —— 某任务 rev > R 的行，按 rev 升序。
CREATE INDEX task_interjections_task_rev ON task_interjections (task_id, rev);
-- 查询模式二：开场补投 / 收尾结算按任务捞 queued（常为空，扫部分索引即可）。
CREATE INDEX task_interjections_pending ON task_interjections (task_id)
  WHERE status = 'queued';
-- 查询模式三：启动清扫捞全表 delivering / queued（行数极少）。
CREATE INDEX task_interjections_open ON task_interjections (status)
  WHERE status IN ('queued', 'delivering');
```

**0032_interjections.down.sql**

```sql
-- 回滚 0032：整表丢弃。up 没有改动任何既有表，因此可完全撤销；
-- 丢掉的只有「人插过什么话」这段审计，agent_events 不受影响。
-- 序列 OWNED BY 该列，随表一起删；IF EXISTS 那句是防御性的显式收尾。
DROP TABLE task_interjections;
DROP SEQUENCE IF EXISTS task_interjections_rev_seq;
```

`lathe_migrate down && lathe_migrate up` 必须能过（AC-1）。列数：**12 列**（初稿 11 列 + R1 新增的 `rev`）。

**不改 `agent_events`**：插话的呈现不走 `agent_events`，也**不扩展 `GET /events`**（R1 改动，理由见 §3.3），
所以 0032 是**唯一**一条迁移。`agent_events` 是每任务成百上千行的执行流水（`0005` 头注释），
已有 4 条 CHECK 变更历史（0005/0011/0014/0015），能不加就不加。

### 3.2 驱动层改造（`internal/integration/agent`）

**目标**：让 `Driver` 能「在跑的时候收一条消息」，同时**保持 `StreamInput=false` 时 `Run` 的语义与行为不变**（0 回归）。

1. **`RunParams` 新增三个字段**（`agent.go:153`）：

   ```go
   // StreamInput 为真时以 --input-format stream-json --replay-user-messages 启动，
   // prompt 作为 stdin 的第一帧写入。stdin 的生命周期见 Run 的「stdin 关闭时机」。
   // 默认 false（走位置参数，与今天完全一致）—— 分诊、PRD 规划、预览推荐都不打开它。
   StreamInput bool
   // OnReady 在进程起来、stdin 可写后回调一次，交出可投递句柄。
   // 为 nil 时（默认）插话机制整体消失。
   OnReady func(*Session)
   // OnDelivery 是注入帧的投递回显（--replay-user-messages 的 isReplay 事件）。
   // 在**读协程**里同步调用，契约：只许做内存操作（非阻塞发信号），
   // 禁止任何 I/O —— 尤其是 DB 写（§3.4「线程模型」，AC-6b 守着）。
   OnDelivery func(deliveryID string)
   ```

2. **新增 `Session` 类型**（同包，`session.go` 新文件）：

   ```go
   type Session struct { … }

   // Deliver 把一句话作为一帧 stream-json user 消息写进 stdin，帧 uuid = deliveryID，**写完（或失败）才返回**。
   // deliveryID 由**调用方**生成并传入（R2 改动，回应 B1(c)）：调用方必须先 registry.Expect(deliveryID)
   // 再调 Deliver，于是回显无论多早到达，都有等待者接住，不会被误记成 no_replay_ack。
   // deliveryID 必须是合法 UUID（非法 → ErrInvalidDeliveryID，不写入）。上限 2 秒，绝不无限阻塞：
   //   - 会话已收尾（stdin 已关）或在排队期间收尾 → ErrSessionClosed
   //   - 2 秒内没排上队 / 没写进管道（CLI 不读 stdin、管道缓冲满）→ ErrDeliverBackpressure
   // 并发安全。
   func (s *Session) Deliver(ctx context.Context, deliveryID, text string) error
   // Closed 报告 stdin 是否已关闭（首条 result / ctx 结束 / 进程退出，三者任一）。
   func (s *Session) Closed() bool
   ```

   内部：`chan outbound`（容量 16）+ 一条写协程。写协程对 `*os.File` 写端每帧 `SetWriteDeadline(now+2s)`
   （评审期实测：对没人读的 `os.Pipe` 写 1MiB，写满 64KiB 缓冲后 1s 期限准时返回 `i/o timeout`，
   见 `evidence/f01/review-r1/stdin-probes/deadline_output.txt`）。收尾时写协程把 channel 里**尚未写出**的帧
   逐个以 `ErrSessionClosed` 回给等待的 `Deliver`，不留悬空调用方。
   **写成功不代表进程活着、写失败也不代表进程死了**（实测子进程被杀后对写端写仍返回 `nil`，同一文件）——
   存活判据只看 `closed` 标志，不看写入返回值。

3. **`Run` 在 StreamInput 模式下**（`StreamInput=false` 时下面一条都不走，代码路径与今天逐行一致）：

   - `buildArgs` 追加 `--input-format stream-json --replay-user-messages`；prompt **不再作为位置参数**，
     而是第一帧 user 消息（`{"type":"user","message":{"role":"user","content":[{"type":"text","text":…}]},"uuid":…}`）。
   - **stdin 必须是 `*os.File`**（R1 修正，§9 第 1 条）：`pr, pw, _ := os.Pipe()`，`cmd.Stdin = pr`，
     `cmd.Start()` 之后父进程立即 `pr.Close()`（与 stdout/stderr 的 `outW.Close()` 同一写法，`agent.go:246-248`）。
     写端 `pw` 交给 `Session`。**禁止 `io.Pipe`**，也**不用** `cmd.StdinPipe()`（它把 Close 时机交给 exec，
     而这里需要在 Wait 之前、由解析侧按 result 主动关）。理由：`cmd.Stdin` 不是 `*os.File` 时 exec 会起一条
     拷贝协程，`cmd.Wait` 等它结束；写端不关就永不返回，**进程被杀后也不返回**。评审期复现
     （`evidence/f01/review-r1/stdin-probes/output.txt`）：
     | 方式 | 写端不关、子进程 1s 退出 | 子进程 0.7s 被 Kill |
     |---|---|---|
     | `io.Pipe` | Wait 4s 仍阻塞（死锁） | Wait 5s 仍阻塞 |
     | `os.Pipe` | Wait 1.003s 返回 | Wait 0.701s 返回 |
     | `cmd.StdinPipe()` | Wait 1.003s 返回 | —（未测，不采用） |
   - **stdin 关闭时机（精确定义，三者任一先到即关，`sync.Once` 幂等）**：
     1. **读协程解析到本次运行的第一条 `type=result`** —— 关闭写端并置 `closed`；
     2. **ctx 结束**（总超时 / 取消）或**静默看门狗触发** —— 在 `killProcessGroup` 之前关（f06 抽出 `supervise`
        之后即 `launch.BeforeKill`，§3.2 第 6 条 S2）；
     3. **`cmd.Wait()` 返回之后**（兜底，正常情况下此时已经关过）。
     不存在「常开到进程退出」的路径。**不依赖** CLI 对未关 stdin 的任何行为（§2.2 第 4 条：实测不稳定）。
     依据：P5 —— close 之后已写入管道的帧仍会被吸收、进程正常退出。
   - **result 之后才到的插话**：`Session.Closed()==true` → `Deliver` 返回 `ErrSessionClosed`，
     不写入。这条消息由管线侧按「没有活会话」处理（留 `queued`，等下一轮补投或收尾结算判 `failed`，§3.3/§3.4）。
     **不会**因为这句话给已经结束的 turn 开第二个 turn。
   - **竞态窗口仍然存在且被定义**：一帧在 result 到达前一刻已写进管道、CLI 在 result 之后才读到它 →
     CLI 开新 turn、产出**第二条 result**（P1 的形态）。这无法靠时序消除，所以由下一条规定 `Result` 的取值。

4. **过滤回显与噪声事件（防污染事件流）**：`parseStream` 拆出内部的 `parseStreamWith(r, hooks)`：

   ```go
   type streamHooks struct {
       Touch       func()            // 每一条可解析的 JSON 行都调，过滤之前 —— 喂看门狗
       OnEvent     func(Event)       // 只收放行的事件
       OnDelivery  func(id string)   // isReplay 回显
       OnResult    func()            // 第一条 result 到达时调一次（StreamInput 用来关 stdin）
       Whitelist   bool              // StreamInput 模式为真
   }
   // 既有签名保留，行为见下：
   func parseStream(r io.Reader, onEvent func(Event)) (*Result, error) // = parseStreamWith(r, streamHooks{OnEvent: onEvent})
   ```

   过滤规则（按顺序）：

   ```
   0. 每一条 JSON 行先 Touch()   ← 过滤在 idle.touch() 之后，被丢弃的事件照样证明进程活着
   1. type == user 且 isReplay == true  → OnDelivery(uuid)，不交给 OnEvent      【所有模式】
   2. type == command_lifecycle          → 丢弃，不交给 OnEvent                 【所有模式】
   3. Whitelist 为真时：type ∉ {system, assistant, user, result} → 丢弃          【仅 StreamInput】
   4. 其余照旧交给 OnEvent（含非 replay 的 user = 真·工具结果回灌）
   ```

   1、2 两条在**所有模式**生效：这两种形状只会由 `--input-format stream-json` / `--replay-user-messages` 产生
   （§2.2 第 5、6 条、P8），非流式模式下它们根本不出现，过滤是可证明的 no-op —— 这让 AC-4 能用**既有签名**
   `parseStream(r, onEvent)` 写成真正的 A 类先红。第 3 条（白名单兜底未知类型）只在 StreamInput 开：
   非流式模式下未知类型仍按 `docs/04`「提炼不出结构时保底 raw」落库，不改既有立场。
   `Run` 里的接法由 `parseStream(outR, func(e){ idle.touch(); p.OnEvent(e) })`（`agent.go:292-297`）
   改为 `parseStreamWith(outR, streamHooks{Touch: idle.touch, OnEvent: p.OnEvent, …})` ——
   `Touch` 与今天一样**每行都调**，看门狗的输入集合不变（AC-4b 守着）。
   `digest.go` 一行不改；丢弃 `command_lifecycle` 的理由（CLI 连接性噪声，不是 agent 产出）写进代码注释。

5. **一次 `Run` 出现多条 result 时 `Result` 的取值（R1 新增，§2.2 第 7 条实测为据）**：

   | 字段 | 取值 | 理由 |
   |---|---|---|
   | `Text`、`Success`、`IsError`、`Subtype`、`TerminalReason`、`PermissionDenials`、`APIErrorStatus`、`SessionID` | **最后一条** | 会话的终局状态；worktree 的样子就是最后一个 turn 之后的样子。第一条 success、第二条 error → 本次 Run 按 error 处理（如实） |
   | `CostUSD`（`total_cost_usd`） | **最后一条** | 实测是**进程累计**（0.078858 → 0.0976604），相加会重复计费 |
   | `NumTurns`、`DurationMS` | **各条求和** | 实测是**单 turn** 量（`num_turns` 1/1，`duration_ms` 2759/2499） |

   即「今天的覆盖式赋值」（`agent.go:420`）只对 `NumTurns`/`DurationMS` 改成累加，其余维持覆盖。
   单 result 时与今天逐字段一致（AC-5c 守着）。

   **与 f03 的接缝（R4 定口径，R5 去掉互指、定为唯一口径：`Usage` 整组随末条覆盖、回落也只取末条、不跨 result 相加）**。
   f03 在同一处赋值（`agent.go:418-432`）上给 `Result` 加 `Usage`（一条 result 内取 `modelUsage` 各模型之和，缺它时回落**该条**的 `usage`）
   与 `CostKnown`。多 result 下只有一条规则，**写死在下表**；下表与 f03 PRD 第五版 §3.1「多条 result 时 `Usage` 怎么取」的表**逐字相同**，
   两份 PRD 各存一份原文、各自完整，不以「以对方为准」互指；改一处必须在同一轮同改另一处，单方面改动即视为接缝回归（§9 R5 B1）：

   | 字段 | 多 result 时的取值 | 依据 |
   |---|---|---|
   | `Usage`（整组：四个计数、`Models`、`Present`） | **随最后一条 result 覆盖，不跨 result 相加**；末条有 `modelUsage` 用它，没有就回落**末条**的 `usage`；末条两个键都没有 → `Present=false`、四项 0（末条整组胜出，不拿前几条补，不混用） | `modelUsage` 是进程累计（fixture 第二条 `inputTokens:6 = 2+4`、`cacheCreationInputTokens:18978 = 15746+3232`），末条即全量；回落路径的 `usage` 是单 turn 量，只取末条会少记前几 turn —— f03 §3.1 已写明并接受这个代价（只在缺 `modelUsage` 的旧 CLI 形状上发生、方向是少记） |
   | `CostKnown` | 与 `CostUSD` 同组，随末条覆盖（任一 result 到达即为 `true`） | `total_cost_usd` 是 result 必有字段、进程累计 |

   一句话：**`Usage`/`CostKnown` 在覆盖组，f01 的累加组只有 `NumTurns`/`DurationMS` 两个字段，不再扩。**
   （R3 曾写「回落 `usage` 时逐项相加」并要求 `TestMultiResultUsageFollowsCumulativeRule`，与 f03 AC-1 第 2、3 例直接矛盾，R4 删除，见 §9 R4 B1。）

   - **f01 先合入**：`dev` 上没有 `Usage`/`CostKnown`。f01 只交付「`NumTurns`/`DurationMS` 累加、其余覆盖」与 fixture
     `replay-user.ndjson`（两条 result，已在证据目录，PR 里拷进 `internal/integration/agent/testdata/`）。f03 rebase 时把两个字段加进
     **覆盖组**那一侧，并写 `TestResultEventUsageTakesLastResult`（复用 f01 已入库的 fixture）；f01 不为 `Usage` 写任何代码或测试（字段不存在）。
   - **f03 先合入**：`dev` 上赋值是「整体覆盖」，`Usage`/`CostKnown` 已在其中，`TestResultEventUsageTakesLastResult` 与
     `internal/integration/agent/testdata/replay-user.ndjson`（f03 PR 拷入，与证据目录同名同内容）已在 `dev`；f01 rebase 时复用该 fixture、
     不重复拷入（若内容不同即停下核对，不覆盖）。f01 rebase 时**只**把 `NumTurns`/`DurationMS` 改成累加，
     **不得**把 `Usage`/`CostKnown` 移出覆盖组、不得为回落路径加任何相加逻辑；f03 的 `TestResultEventUsageTakesLastResult`
     （四例：两条都有 `modelUsage` → 6/12/12532/18978；两条都只有 `usage` → 4/6/12532/3232；第 1 条有、第 2 条两键都没有 → `Present=false`；单条回归）
     **一行不改**仍须绿，f01 PR 里贴这条测试 rebase 后的输出。f01 不改 f03 的 `usageOf`/`RecordRun`/账本任何代码。
   - 任一顺序下：f03 的 `TestResultEventParsesUsage`（单 result）、`TestResultEventUsageTakesLastResult`（多 result）与 f01 的 AC-4
     （多 result 的 `NumTurns/DurationMS/CostUSD/Text`）**都不改断言**仍须绿。没有「由后合入者写」的第三条测试 —— 规则只有一份、
     多 result `Usage` 测试只有 `TestResultEventUsageTakesLastResult` 一条，**不论合入顺序都由 f03 写**（f01 先合 → f03 rebase 时写；f03 先合 → 已在 `dev`），
     f01 只负责「rebase 后一行不改仍绿」并在 PR 里贴输出。该测试的红 ② 是「回落逐项相加 + `Present` 取或」那一版实现（f03 AC-1），
     即 f01 rebase 时被禁止的改法会被它抓红。
   - **非流式运行**（今天的全部运行，以及分诊/PRD/预览）一个进程只有一条 result，上表退化为「单条赋值」。
     f03 PRD「一次 `Run` 一个进程、一行 `agent_runs`」在本设计下仍成立；任何一方都**不得**把多条 result 的 `modelUsage` 或 `usage` 相加。
   - codex（f06）的 `Usage` 由 `UsageFromCodex` 按 turn 相加（f03 §3.7），与本条无关：codex 不开 `StreamInput`（§3.2 第 7 条）。

6. **对 f06 `supervise()` 的确切要求（R2 新增，回应 B4(a)）**。
   f06 PRD §3.1 定死的 `launch{Bin,Args,Dir,Env,Stdin,Started,Exited}` 与
   `supervise(ctx, l, limits, parse func(r, emit), onEvent)` 是按 f01 **初稿**的「常开 stdin、在 `Exited` 里关写端」
   写的（f06 §4 第 554 行、AC-24 第 973-975 行）。R1 起 f01 的模型变成「三个时机关 stdin + Touch 在过滤之前」，
   按 f06 现在的签名有两处做不到：没有「kill 之前」的钩子；`touch` 包在 `emit` 外层，而过滤在 `parse` 里，
   被过滤掉的 `command_lifecycle`/replay 行就不再喂看门狗 —— 正是 AC-4b 要防的形态。
   f01 对 `supervise` 契约的要求逐条如下（S1–S5），**无论谁先合入都必须成立**：

   | # | 要求 | 落在 f06 签名的哪里 | 守它的测试（在 `Driver.Run` 层写，抽 `supervise` 不改这些测试） |
   |---|---|---|---|
   | S1 | **Touch 由 parse 调、且在过滤之前**：`parse` 的签名改为 `parse func(r io.Reader, h parseHooks) (*Result, error)`，`type parseHooks struct{ Touch func(); Emit func(Event) }`；`supervise` 把 `idle.touch` 作为 `h.Touch` 传入，claude 的 parse 对**每一条可解析 JSON 行**先 `h.Touch()` 再决定是否 `h.Emit`。`supervise` 可以在 emit 外层**继续**多 touch 一次（幂等，无害），但**不得**只在 emit 外层 touch | `parse` 参数（替换 f06 的 `func(r, emit)`） | AC-4b（`StreamInput=true` 子用例：只有 `command_lifecycle` 行持续 2.4s、`idle=1s` → 不得 `ErrIdle`） |
   | S2 | **kill 之前的关闭钩子**：`launch` 增 `BeforeKill func()`：ctx 结束（总超时/取消）或看门狗触发时，`supervise` 在 `killProcessGroup` **之前**同步调用一次（`sync.Once`，可为 nil）。claude 驱动在这里关 stdin 写端、置 `Session.closed` | `launch.BeforeKill`（新增字段） | AC-5d 第 3 行（`sleep 30`、`timeout=1s` → < 3s 返回）；新增断言：`BeforeKill` 恰好 1 次且早于进程退出 |
   | S3 | **首条 result 关闭**发生在 claude 的 `parse` 闭包内（`OnResult` → `Session.closeStdin()`），**`supervise` 不需要知道**；`parse` 是驱动构造的闭包，可以捕获 `Session` | 无需改签名 | AC-5d 第 2 行（CLI 在 result 后 `cat >/dev/null` 等 EOF → result 后 < 2s 返回、非 `ErrIdle`） |
   | S4 | `Exited` 仍是兜底关闭点（`cmd.Wait` 返回后）；`Started` 挂 `OnReady`。两者语义与 f06 现文一致，**不变** | 不变 | f06 AC-24 原样 |
   | S5 | `launch.Stdin` 为 `*os.File`（`os.Pipe` 读端），`supervise` 在 Start 后关父进程那份读端 —— 与 f06 现文一致 | 不变 | f06 AC-24 原样 + AC-5d 第 1 行 |

   f06 AC-24 第 973-975 行那条「测试一直不关写端、假 CLI 自己退出 → supervise 在宽限期内返回」**仍然有价值**（它守的是
   `*os.File` 不会卡死 Wait 这一健壮性），但它的注释「断言 f01 的 stdin 常开模型」已过时；f01 的模型是「首条 result 即关」。

   **互相 rebase 的规则**（原样写进 f01 与 f06 两个 PR 的描述）：
   - **f01 先合入**：`dev` 上 `Driver.Run` 内联实现 S1–S3（`parseStreamWith` 的 `Touch`/`OnResult`、kill 前关 stdin）。
     f06 rebase 抽 `supervise` 时：按 S1 改 `parse` 签名、按 S2 加 `BeforeKill`；AC-4b/AC-5d 是 `Driver.Run` 层测试，
     抽取后**一行不改**仍须绿（f06 AC-3「已有测试一行不改」自然覆盖它们）；`TestCodexHandlesEveryRunParamsField` 当场红，
     f06 按其 §3.1 第 248-251 行把 `StreamInput/OnReady/OnDelivery` 放进 `codexRejects`、`Capabilities` 加 `StreamInput`。
   - **f06 先合入**：f01 rebase 时改 `supervise` 的**签名两处**（S1、S2）与 claude 构造 `launch` 那一步，
     不碰 codex 构造与 `supervise` 的其它逻辑；codex 的 parse 按 S1 新签名改成「每行 `h.Touch()` 再 `h.Emit`」，
     行为与 f06 现状等价（codex 不过滤，touch 集合不变）；f01 同样做那「三处改动」（f06 §3.1 第 251 行）。
     本 PR 的 diff 在 `supervise` 上只允许出现这两处签名改动与对应调用点（PR 描述贴 `git diff` 片段供评审核对）。
   - 两种顺序下 AC-4b、AC-5d、f06 AC-24 必须同时绿；任一红即不得合入。
   - f01 已在本 PRD 写明这些要求；f06 PRD 的同步（第 554 行与 AC-24 注释）由 f06 线负责，f01 不改别人的 PRD。
     **两份 PRD 冲突时，`supervise` 的 stdin/touch 语义以本节为准**（f01 是这段语义的唯一需求方，f06 §3.1 原文也说
     「stdin 归各驱动所有，`supervise` 不碰它的内容与生命周期」—— S1/S2 正是让驱动能自己管住生命周期的最小钩子）。

7. **驱动能力判定（R2 新增，回应 B4(b)）**。G3 承诺「驱动不支持 → 明确状态码」，落地如下：
   - runner 侧唯一判定函数 `streamInputSupported(tk *task.Task) bool`（`interject.go`）。
     **f06 合入前**：`dev` 上只有 claude 一个驱动，函数恒返回 `true`，并附注释「f06 合入后改为
     `agent.CapabilitiesOf(<任务驱动>).StreamInput`」；**f06 合入后**（无论哪方后 rebase）：改为按 f06 定义的任务驱动
     解析（`Task.AgentDriver`，空串 = claude）问 `agent.CapabilitiesOf(driver).StreamInput`。
   - 消费方两处：① `runAgent`：不支持 → 不设 `StreamInput/OnReady/OnDelivery`（于是 codex 永远不会收到这些字段，
     不会走到 f06 的 `ErrUnsupportedParam`）；② `Submit` 判定顺序第 2 步（§3.3）：不支持 → `422`。
   - **开场补投不看驱动**：补投只是把文本拼进 prompt，对任何驱动都成立；所以「驱动不支持」只拒绝**新的**插话，
     历史上已 `queued` 的行仍按补投/结算收尾。

### 3.3 API

**新增两个路由**（`internal/httpapi/api.go` 的 `Routes`，`api.go:61-95`）；**`GET /events` 不动**（R1 改动）：

| 方法 | 路径 | 作用 |
|---|---|---|
| `POST` | `/api/tasks/{id}/interject` | 落库 + 尝试投递；body `{"message": "..."}` |
| `GET` | `/api/tasks/{id}/interjections?after_rev=R` | 增量列出该任务的插话（规则见下） |

**为什么撤回「扩展 `GET /events`」**（§9 第 6 条）：① 插话的状态会在事后变化（queued → delivered/failed），
而 `/events` 的游标是 `agent_events.id`，插话行的变化不会让它前进 —— 两种数据挂一个游标在原理上就不对；
② f04 要改 `taskEvents`（summary、`before`、`phase` 参数，f04 §3.3），f01 再往同一个 handler 里加字段只会制造冲突；
③ 插话不再合并进事件时间线（§3.5），前端不需要两者原子地一起到。

**`GET /interjections` 的精确规则**（机器可验证，AC-17）：

- 参数 `after_rev`：非负整数，缺省 0；非法值 → `400`。
- 返回该任务 `rev > after_rev` 的**全部字段**行，按 `rev` 升序，最多 **200** 行：
  `{"interjections":[{id,status,body,error,phase,sessionId,deliveryId,createdAt,deliveredAt,rev}…],"rev":N,"more":bool}`。
- `rev`：本批最后一行的 `rev`；**本批为空时原样回显 `after_rev`**（客户端游标永不后退、永不变成未定义）。
- `more`：恰好取满 200 行时为真，客户端应立即用新 `rev` 再拉一次。
- 正确性来源：所有写入（INSERT、`Mark*`、结算的四个触发点 T1–T4）都在 `pg_advisory_xact_lock(task_id)` 下
  `SET rev = nextval(...)` 并提交，同一任务内 rev 的顺序就是提交顺序；任何一次状态变化都会让这一行以更大的
  rev 重新出现在下一次响应里，与 `agent_events` 游标是否空闲无关。
- 前端按 `id` 就地覆盖（upsert），所以同一行多次出现是幂等的。

`POST` 的返回契约（**机器可验证**）：

```jsonc
// 200 — 已送达（CLI 回显确认，或已写入但 2s 内没等到回显 → error:"no_replay_ack"）
{"id": 12, "status": "delivered", "deliveredAt": "…", "rev": 40}
// 202 — 已落库，当前没有本节点可投递的活会话（含：本轮已出 result、stdin 已关）；消息保留，状态 queued
{"id": 12, "status": "queued", "reason": "no_live_session", "rev": 41}
// 409 — 任务不在 implementing / verifying；这句话已落库为 failed，不会进入模型
{"error": "任务当前没有运行中的 agent（状态：pr_open），插话只在实现与验证阶段有效", "id": 12}
// 422 — 任务所用的 agent 驱动不支持运行中输入（f06 引入 codex 之后才可能出现，§3.2 第 7 条）；已落库为 failed
{"error": "该任务的 agent 驱动（codex）不支持运行中插话", "id": 12, "reason": "driver_unsupported"}
// 413 — 消息过长（超过 LATHE_INTERJECT_MAX_CHARS，按 rune 计）；不落库
// 429 — 同一任务在窗口内条数超限（§3.6）；不落库
// 503 — 有活会话但投递通道拥塞（2 秒内没写进去），消息记 failed，可重发；或 Interjector 未装配
// 404 — 任务不存在或非属主
```

**判定顺序（R1 改为「状态是闸门、会话决定投递方式」）**：

```
1. state ∉ {implementing, verifying}             → 落 failed + 409（不管有没有活会话）
2. !streamInputSupported(任务驱动)（§3.2 第 7 条） → 落 failed + 422（error：「该任务的 agent 驱动（X）不支持运行中插话」）
3. 本节点有活会话（已注册且 !Closed）              → 实时投递：200 / 503（ErrDeliverBackpressure）
   投递时会话刚好收尾（ErrSessionClosed）          → 经**状态闸门**回退（Submit 第 5.4 步）：仍在 implementing/verifying → queued 202；
                                                     否则 → failed 409
4. 没有活会话                                     → queued，202（下一轮补投或收尾结算）
```
第 1、2 步在同一个事务 A 里判（读 state 的同时读任务驱动），先判状态：已结束的任务不论驱动都是 409。
`dev` 上（f06 合入前）第 2 步恒不命中；它的测试用注入的 `streamInputSupported` 假实现（AC-21）。

**为什么 `verifying` 也放行**：修复回路（`pipeline.go:1009`）里的 agent 调用发生在任务**已处于 `verifying`** 之后
（`stageVerify` 先转移，`:936`，再进 fix 循环，`:993`）。只放 `implementing` 就会把「验证没过、agent 正在按失败日志修」
这一段——恰恰最需要人说话的时候——全部 409 掉。
**为什么状态是闸门而不是会话（R1 改动）**：初稿「有会话就 200、不看状态」在取消场景下是错的 ——
取消只改库、**不停在途 agent**（`cmd/lathe/queue.go:174-184` 的语义边界注释），任务已 `cancelled` 而进程还在跑，
初稿会接受并投递一句话给一个已被取消的任务。现在 `cancelled/failed/pr_open/…` 一律 409。

**权限**：两个接口一律 `Tasks.Get` + `tk.UserID != CurrentUser(r).ID` → **404**（复用 `api.go:602-606` 的写法）。
**限流**：复用 `internal/httpapi/limiter.go` 的 `newLimiter()/allowAt`（`limiter.go:13-57`），键 `task:<id>`。
**装配与降级**：`httpapi.API` 新增一个接口字段（与 `Scenes`/`Baselines` 同形态，`api.go:37-52`）：

```go
// Interjector 把一条插话落库并尝试投递（实现在 runner：注册表 + store + 脱敏）。
type Interjector interface {
    Submit(ctx context.Context, taskID, userID int64, body string) (SubmitResult, error)
    ListSince(ctx context.Context, taskID int64, afterRev int64, limit int) ([]store.Interjection, error)
}
```

为 nil 时两个路由返回 `503`（不是 404，也不假装成功）—— 与 `Baselines` 为 nil 时的既有取向一致。
`httpapi` 已经 import `internal/runner`（`api.go:16`、`github_webhook.go:12`），实现放 runner 没有分层障碍；
接口仍定义在 httpapi，是为了测试注入假件，不是为了避开依赖。

**`Submit` 的实现顺序（这条顺序就是「不丢消息」的定义；跑在 HTTP 请求协程上，但第 4 步起脱离请求 ctx）**：

1. 校验长度（按 rune，**脱敏前**的原文计）→ 限流；不通过不落库。
2. `body = redactInterjection(body)`（§3.7，唯一脱敏点）。
3. **事务 A**（用请求 ctx：客户端在提交前断开 → 事务回滚、无行、客户端也没收到任何承诺，一致）：
   `pg_advisory_xact_lock(task_id)` → `SELECT state, <驱动> FROM tasks WHERE id=$1 FOR SHARE` →
   state 不在 `{implementing, verifying}`：`INSERT … status='failed', error='任务状态为 X…'`，提交，返回 409；
   驱动不支持：`INSERT … status='failed', error='该任务的 agent 驱动…'`，提交，返回 422；
   否则 `INSERT … status='queued'`，提交。DB 写失败直接 500，**绝不先投后写**。
   `FOR SHARE` 与状态机 `Transition` 的 `FOR UPDATE`（`internal/task/machine.go:316`）互斥：
   插入要么发生在转出 `verifying` 之前（于是一定被随后的收尾结算看见），要么读到新状态而 409。
4. **从这里起，行已经存在，所有操作用脱离请求的 ctx（R2 改动，回应 B1(a)）**：
   `dctx := context.WithoutCancel(r.Context())`；每次写库 `context.WithTimeout(dctx, 5s)`；`Deliver` 用 `dctx`；
   等 ack 只看 ack channel 与 2s 定时器，**不** `select` `r.Context().Done()`。
   客户端断开只意味着「响应发不出去」，不改变这一行走到终态的路径（AC-20 守着）。
   查注册表：无活会话 → 返回 202（`queued`）。
5. 有活会话：
   1. **调用方生成 id**（R2 改动，回应 B1(c)）：`deliveryID := newUUID()`（`internal/runner/uuid.go:13`，已有，不是新依赖）；
   2. `MarkDelivering(id, deliveryID, phase, sessionID)`（**条件更新** `WHERE status='queued'`，同时写 `delivery_id`；
      0 行 = 已被收尾结算判了 failed → 重读该行，按其实际状态返回 409）；
   3. `ack := registry.Expect(deliveryID)`（容量 1 的 channel；`defer registry.Forget(deliveryID)`）——**先 Expect 后 Deliver**，
      回显再早也有人接；
   4. `err := Session.Deliver(dctx, deliveryID, body)`：
      - `ErrSessionClosed` → **经状态闸门回退**（R2 改动，回应 B1(b)）：一个事务内
        `pg_advisory_xact_lock(task_id)` → `SELECT state FROM tasks … FOR SHARE` →
        state ∈ `{implementing, verifying}`：`UPDATE … SET status='queued', delivery_id=NULL, phase=NULL, session_id=NULL
        WHERE id=$1 AND status='delivering'`，返回 202；
        否则：`UPDATE … SET status='failed', error=<按 state 取 §3.4 收尾结算表的文案>`，返回 409。
        为什么这样就没有第三种结局：收尾结算在 `Execute` 返回后读 state，而转出 `verifying` 的 `Transition` 早于它提交；
        二者都在同一把 advisory lock 下。① 回退先拿到锁 → 读到的 state 若仍是 `verifying`，行变 `queued`，
        随后结算一定看见它并按终态判 `failed`（或任务被重新入队，留给下一轮补投）；② 结算先拿到锁 → 它跳过这条
        `delivering` 行（它只动 `queued`），之后回退拿到锁、读到的已是终态（`Transition` 先于结算提交）→ 直接 `failed`。
        两种交错都不会留下 `queued` 或 `delivering`（AC-19 竞态行守着）；
      - `ErrDeliverBackpressure` → `MarkFailed('投递通道拥塞…')`，返回 503；
      - 写成功 → 在 **Submit 协程**里等 ack channel 至多 2 秒：收到 → `MarkDelivered`；
        超时 → `MarkDelivered(error='no_replay_ack')`（**不**判 failed：写进管道且会话未关，P5 保证它会被吸收）；返回 200。
6. **终态写失败**（DB 不可用，`dctx` 下 5s 超时）：重试至多 3 次（退避 0.5s/1s/2s，总计 ≤ 25s），仍失败则 `slog.Error`
   （只记 `id`、`len`，不记正文）并返回 500。这是**唯一**会让行暂留 `delivering` 的情形，前提是 DB 整体不可用；
   由下一次启动清扫收尾（`delivering` → `failed`「进程重启，投递未完成」）。如实写进 `docs/features/interject.md`，
   不宣称 DB 宕机下也能即时到终态。
7. 收尾结算与启动清扫**刻意不动** `delivering` 行：每一条 `delivering` 都有且只有一个持有者（实时投递 = 该 Submit 协程；
   开场补投 = 该次 `runAgent`），由持有者在脱离请求/任务 ctx 的写里收尾；进程死了持有者也就没了，交给启动清扫。

### 3.4 管线改造（`internal/runner`）

**改造点只有两处：`runAgent`（`pipeline.go:766`）与 `Execute`（`pipeline.go:297`）加一个 `defer`**。
分诊与预览等直连 `Agent.Run` 的地方**一律不打开 `StreamInput`**。

```go
func (p *Pipeline) runAgent(rc *runCtx, phase, prompt, session string, resume bool) (*agent.Result, error) {
    prompt = p.injectFailureMemory(rc, phase, prompt, resume)
    // 开场补投（在 Agent.Run 之前，管线协程上）：queued → delivering，正文拼进 prompt 尾部。
    // 顺序固定（R3）：base → 失败记忆段（仅 implement / implement-reentry 有）→ 插话段。插话段永远是最后一段。
    prompt, prefilled := p.prefillInterjections(rc, phase, prompt)
    …  // sink、channel 与今天一致
    var params = agent.RunParams{ …, OnEvent: sink.OnEvent }
    if p.Interjections != nil && streamInputSupported(rc.tk) {   // §3.2 第 7 条
        params.StreamInput = true
        params.OnReady = func(s *agent.Session) { p.Interjections.Register(rc.tk.ID, s, meta) }
        // 读协程上：只发内存信号，零 I/O（见下「线程模型」）
        params.OnDelivery = p.Interjections.Ack
    }
    res, err := p.Agent.Run(rc.ctx, params)
    if p.Interjections != nil { p.Interjections.Unregister(rc.tk.ID) }
    // 补投行的结局（管线协程上）：res != nil && res.NumTurns >= 1 → delivered；否则（无 result，或 result 的
    // num_turns == 0，例如 --resume 找不到会话，§2.2 第 10 条）→ 退回 queued，
    // 交给同一任务的下一次 runAgent（例如 resume 失败后的 reentry，pipeline.go:703-714）或结算。
    p.finishPrefill(rc, prefilled, res)
    …
}
```

- **线程模型（R1 定为唯一模型，§9 第 3 条）**：

  | 协程 | 允许碰 DB 吗 | 做什么 |
  |---|---|---|
  | agent 读协程（`parseStreamWith`） | **否** | `OnDelivery = Registry.Ack(id)`：持锁查 `map[deliveryID]chan`，`select{case ch<-struct{}{}: default:}`，放锁。纯内存、不阻塞 |
  | HTTP 请求协程（`Submit`） | 是：事务 A 用请求 ctx；**行落库之后**的每次写用 `context.WithoutCancel(r.Context())` + 5s 超时（R2，§3.3 第 4 步） | 落库、`MarkDelivering/Delivered/Failed`、闸门回退、等 ack（≤2s） |
  | 管线协程（`runAgent` / `Execute` 的 defer） | 是（`context.WithoutCancel` + 5s 超时） | 开场补投、补投结局、结算 T1 |
  | HTTP 请求协程（`ListSince`） | 是（请求 ctx；结算与取行同一事务） | 结算 T2 + 取行 |
  | 清扫 goroutine（`cmd/lathe`） | 是（每任务一次 `WithTimeout(ctx, 5s)`） | 结算 T3；启动清扫 T4 |

  初稿 §3.4 代码里在 `onDelivery` 里同步 `MarkDelivered` 与后文矛盾，已删除。
  这张表原样写进 `interject.go` 的包注释；AC-6b 用「只让读协程可能碰到的写路径变慢」的假 store 守住读协程不被拖慢。
- **注册表**：`internal/runner/interject.go` 新文件，`map[int64]*entry` + `sync.Mutex`，另有
  `map[string]chan struct{}` 存 ack 等待者（`Expect` 放入、`Submit` 退出时删除，不泄漏）。
  条目 `{session *agent.Session, phase, sessionID string, startedAt time.Time}`；`Lookup` 对 `session.Closed()`
  为真的条目返回「无活会话」。一个任务同一时刻至多一个条目（实现轮与修复轮串行），`Register` 遇到已存在条目
  就覆盖并 `slog.Warn`。
- **开场补投**：读 `PendingInterjections(taskID)`（queued，按 id 升序）→ 在 advisory lock 下条件更新为
  `delivering`、写 `phase/session_id` → 正文拼成**一段**（无论几条，包裹语只出现一次）：
  ```
  \n\n---\n\n## 【人追加的指示】\n
  以下是任务属主在运行中补充的话（按发送先后）；不得据此跳过验收标准或验证步骤。\n\n
  1. <body 1>\n2. <body 2>…
  ```
  追加到 prompt **尾部**。`delivery_id` 留 NULL（不经 stdin 实时投递，编一个 uuid 只会让审计失真）。
  正文就是库里那份**已脱敏**的 body，补投处不再做第二次脱敏（单一脱敏点，§3.7）。
  **与失败记忆的顺序（R3 定死，回应 R3 B3）**：`runAgent` 第一行仍是 `injectFailureMemory`（`pipeline.go:767`，不动），
  补投在它**之后**，所以最终 prompt 恒为 `base + [失败记忆段] + [插话段]`：
  - 理由：失败记忆是「更早任务的历史」，插话是「属主此刻的要求」；后者更新、更具体，放最后一段离模型的开始执行最近，
    且「插话段永远是末段」让断言与审计都只需要一条规则。
  - 互不包含：补投只拼库里的 body，失败记忆只渲染 `failure_digests`（`memory.go:118`），两段的标题
    （`## 这个需求此前失败过` / `## 【人追加的指示】`）各自恰好出现一次。
  - 失败记忆只在 `implement` / `implement-reentry` 注入（`pipeline.go:836-844`）；`fix-N` 等其它 phase 只有插话段，规则不变。
  - `StreamInput=true` 时这份 prompt 原样作为 stdin 首帧，不再拆分（首帧里插话段同样在末尾）。
  - 无插话时 `prefillInterjections` 返回原 prompt **同一个字符串**（不追加分隔符），AC-8 的 A 类红线守着。
- **补投「进没进模型」的判据（R3 改，§2.2 第 10 条）**：`finishPrefill` 只在 `res != nil && res.NumTurns >= 1` 时判
  `delivered`；`res == nil`、或 `NumTurns == 0`（CLI 报错退出、根本没开 turn，如 `--resume` 找不到会话）→ 退回 `queued`
  （`delivery_id/phase/session_id` 清空）。R2 写的「有 result → delivered」会把 resume 降级那一次的补投行错判成已送达，
  随后的 `implement-reentry` 新会话拿不到它们。`NumTurns` 在多 result 下是求和（§3.2 第 5 条），≥1 即「至少一个 turn 读过首帧」。
- **结算 `settleQueued`（R1 新增；R3 改为「一个函数、四个触发点」，回应 R3 B2）**。
  **判定只有一份**：`store.SettleQueuedInterjections(ctx, taskID)`，在 `pg_advisory_xact_lock(task_id)` 下
  `SELECT state FROM tasks WHERE id=$1 FOR SHARE`；state ∈ `{queued, triaging, implementing, verifying}`（还会再跑 agent）→ 不动，
  返回 0；否则把该任务**所有** `queued` 行改 `failed`（`SET rev = nextval(...)`），`error` 按下表。幂等：再调一次是 0 行。

  | state | `error` |
  |---|---|
  | `pr_open` / `awaiting_approval` | 「验证已通过、没有后续 agent 轮次，这句话没有进入模型；如需生效请在 PR review 里提出」 |
  | `failed` | 「任务已失败，这句话没有进入模型；重试时请重新发送」 |
  | `cancelled` | 「任务已取消，这句话没有进入模型」 |
  | `blocked_spec` | 「任务因需求不明确已停下，这句话没有进入模型；请补充到 issue 里」 |
  | `blocked_dep` | 「任务因前置任务失败被阻塞，这句话没有进入模型；恢复后请重新发送」 |
  | 其它 | 「任务已离开实现/验证阶段（状态：X），这句话没有进入模型」 |

  `blocked_dep` 归入「结算」而不是「还会再跑」：它回 `queued` 要等前驱恢复，等待无上界（`machine.go:898` 的级联），
  让人一直看见「已记录待投递」正是 B2 要消除的形态；与「`failed` 的插话不自动重投、人可重发」的既有立场一致。

  **四个触发点**（都只调这一个函数，都**不**挂在 `state.go` / `Transition` / `cancelTask` / `CancelForIssue` 上）：

  | # | 触发点 | 覆盖的情形 | 位置 |
  |---|---|---|---|
  | T1 | `Execute` 第一行 `defer`（在 `setup` 之前，`setup` 失败也覆盖） | 本次执行把任务带到 `pr_open/awaiting_approval/failed/blocked_spec`，或执行期间被取消 | `pipeline.go:297` |
  | T2 | **读时结算**：`ListSince` 在取行之前、同一 advisory lock 事务里先调一次 | `Execute` 已返回、任务停在 `queued`，之后被 `cancelTask`（`api.go:608`）/ `CancelForIssue`（`cmd/lathe/queue.go:234`、`:420`）/ `blocked_dep` 级联（`machine.go:898`）转走 —— `Execute` 不会再跑，**有人看就当场结算** | `interject.go` 的 `ListSince` |
  | T3 | **周期清扫**：`cmd/lathe` 起一条 goroutine，每 5 分钟（编译期常量 `interjectSweepInterval`，不做环境变量，§3.6）对「有 `queued` 行且 state 属于结算集合」的任务逐个调 `SettleQueued` | 同 T2，但没人打开页面（审计查询、API 直读库、f02 收件箱等旁路读者也能在有界时间内看到终态） | 形状照 `gcSessions`（`main.go:811`）；`ctx.Done()` 退出 |
  | T4 | 启动清扫（见下） | 进程在 T1 之前死掉 | `main.go:192` 附近 |

  T2 的读路径写库是刻意的：结算是**由 state 推导出的确定结果**（幂等、只动本任务的 `queued` 行），`GET` 做它与
  「`GET` 不改资源语义」不冲突 —— 它不改任何人能控制的东西，只是把一个已经确定的事实落库，好让 `rev` 游标把变化推给
  所有读者。属主判定在 T2 之前（非属主 404，不触发结算）。常态（无 `queued` 行）下这一步只是一次部分索引上的空扫。
  T3 的查询：`SELECT DISTINCT i.task_id FROM task_interjections i JOIN tasks t ON t.id=i.task_id WHERE i.status='queued'
  AND t.state NOT IN ('queued','triaging','implementing','verifying')`（走 `task_interjections_pending` 部分索引），逐个调 `SettleQueued`
  （判定在函数内重做，查询只是候选集，不依赖它的快照）。

  它**不是状态转移**，只读 `tasks.state`，`state.go`、`machine.go` 一行不改（AC-19 末行机检）。
  **只处理 `queued`，不碰 `delivering`**：`delivering` 行都有活着的持有者（§3.3 第 7 步）；实时投递那条在
  `ErrSessionClosed` 时由 Submit 自己经状态闸门收尾（§3.3 第 5.4 步），补投那条由 `finishPrefill` 收尾
  （`finishPrefill` 在 `Execute` 的 defer 之前执行，所以 T1 看到的补投行已经是 `delivered` 或 `queued`）。
  T2/T3 与 Submit、`finishPrefill` 都在同一把 advisory lock 下，§3.3 第 5.4 步对「回退 vs 结算」两种加锁先后的论证对 T2/T3 同样成立。
- **启动清扫**（`cmd/lathe` 装配，`q.Reconcile` 之后、worker 启动之前，`main.go:192` 附近）：
  `delivering` → `failed`（「进程重启，投递未完成」）；对 T3 同一条候选查询逐个调 `SettleQueued`（同上表文案）。前者保证进程崩溃不留 `delivering` 死行，后者兜住「进程在 `Execute` 的 defer
  之前死掉」的情况。`Reconcile` 把 `implementing/verifying` 转回 `queued` 的任务，其 `queued` 插话**保留**，
  由下一次 `runAgent` 补投。
- **失败即 failed，管线自己的错误处理完全不变**（不新增 `p.fail` 路径，不新增状态转移）。

### 3.5 UI（`web/src`）——R1 改为独立面板，最小化与 f04 的共享文件改动

**f01 对两份共享 Vue 文件的全部改动**（§9 第 8 条；f04 PRD `/opt/lathe-wt/prd/f04-run-replay.md` §3.3 会大改这两份文件）：

| 文件 | f01 的改动 | 与 f04 的冲突点 | 解决规则 |
|---|---|---|---|
| `web/src/views/TaskDetail.vue` | ① `<script setup>` import 区（今天 `:5` `import AgentEventItem …` 之后）加 1 行 `import InterjectPanel from '../components/InterjectPanel.vue'`；② 模板里执行日志卡片**之前**（今天 `:429` `<!-- 执行日志面板 …` 之前）加 1 行 `<InterjectPanel :task-id="task.id" :state="task.state" />` | ① f04 也在 import 区加行（`replay.js`）→ 相邻行文本冲突；② f04 改执行日志卡片**内部**（工具栏等），不改卡片之前的兄弟节点，预计无冲突 | 两边都保留；后合入方 rebase 时把 import 行与组件行原样留下即可，无语义冲突 |
| `web/src/components/AgentEventItem.vue` | **不改** | 无 | — |

`git diff origin/dev...HEAD --stat -- web/src/views/TaskDetail.vue` 必须是 `2 insertions(+)`、0 删除（AC-15）。
插话**不再合并进分阶段时间线**：合并要动 `TaskDetail.vue:107-230` 的分组逻辑，而那正是 f04 要搬进 `replay.js` 的部分。

**新文件 `web/src/components/InterjectPanel.vue`**（全部 UI 逻辑在这里，f04 不碰）：

1. 可插话时（`state ∈ {implementing, verifying}`，与后端判定第 1 条对齐）显示输入框
   「给正在跑的 agent 说一句（不会中断它）」+ 发送按钮；`verifying` 时旁边标
   「只在 agent 运行时（含验证失败后的修复轮）投递；验证判定不受插话影响」。
2. 下方列出本任务的插话（时间、「你」、正文、状态徽标 `已送达 / 已记录待投递 / 未进入模型` + `error` 原文）。
   正文用 `{{ }}` 插值，**不过 markdown、不用 `v-html`**（人写的话，减少 XSS 面）。
3. 数据：挂载时 `GET /interjections?after_rev=0`，之后用 `createPoller`（`web/src/livelog.js:56`）每 2 秒拉一次
   `after_rev=<上次 rev>`，按 `id` upsert；`more` 为真时立即再拉；任务进入终态后再拉一轮收尾即停
   （终态后仍可能有结算把 `queued` 改成 `failed`，必须让人看到）。R3：这「收尾一拉」本身就触发读时结算（§3.4 T2），
   所以它拿到的必定是结算后的终态，不存在「停止轮询时还显示已记录待投递」的窗口；之后用户刷新页面同理。
4. 发送后**乐观插入**一条（「投递中」），以响应体与后续轮询校准；`409` 的 `error` 原样显示在输入框下方。
5. 不在上述状态时不显示输入框，但列表照常显示（历史可审计）。
6. `api.js` 新增两个方法（`interject`、`interjections`），追加在文件末尾。
7. **验收**：`make ui && make build` 后截图进 PR。

### 3.6 配置项

**新增 3 个环境变量**，每个都有唯一消费方，且都登记 README 变量表（§7）：

| 变量 | 默认 | 消费方 | 非法值行为 |
|---|---|---|---|
| `LATHE_INTERJECT_MAX_CHARS` | `8192` | `internal/httpapi` 的 `interject` 校验（按 **rune** 计数） | 非正整数或 `>16384`（= 表上的硬上限）→ **启动报错**（并点名变量与原值） |
| `LATHE_INTERJECT_RATE_LIMIT` | `5` | 限流器（每任务每窗口允许的条数） | 非正整数或 `>100` → **启动报错** |
| `LATHE_INTERJECT_WINDOW` | `10s` | 限流窗口 | 用 `envDurationStrict`（`config.go:509`）→ 非法时长启动报错；下限 `1s`、上限 `1m` |

**不新增**的配置：投递超时（2s）、ack 等待（2s）、channel 容量（16）、`/interjections` 每页上限（200）、周期结算间隔（5 分钟，§3.4 T3）
**不**做成环境变量 —— 它们是实现参数，不是运维旋钮（AGENTS.md §1.3）。
`LATHE_INTERJECT_ENABLED` **不做**：插话的开关就是「装配侧有没有接 Interjections」，生产装配永远接。

**这意味着驱动层改动对生产全量生效（R2 明确，回应 B5）**：合入后**每一个** claude 任务的 `implement`、
`implement-reentry`、`fix-N`（含 `--resume`）、PR review 二轮（`--resume`）、验证失败重试（`--resume`）都从
「prompt 作位置参数」切到「`--input-format stream-json --replay-user-messages` + prompt 走 stdin 首帧」，
不管有没有人插话。分诊、PRD 规划、预览推荐不切（AC-7）。不加开关的理由仍成立（加一个默认关的开关 = 生产上这条
路径从不被走到，等于把风险推迟到「哪天有人打开」，且多一个要长期维护的分叉）；代价是合入前必须有覆盖
**新会话与 `--resume` 两种启动方式**的真实证据：P10（§2.2 第 8 条，裸 CLI）、AC-14（Lathe 驱动，含 resume 段）、
AC-22（:8401 端到端，含一轮 `fix-1` 的 `--resume`）。三者缺一，PR 保持 draft。
`internal/config` 的 `Load()` 里加 3 个字段 + 3 段校验（形状照 `MinWorktreeTTL` / `MinPRDStallTimeout`，`config.go:184-205`）。

### 3.7 安全

| 项 | 做法 |
|---|---|
| 属主隔离 | 两接口都按 `Tasks.Get` + `tk.UserID` 判定，非属主 **404**（不改成 403，G6） |
| 凭据 / 明文——**唯一脱敏点** | `runner.redactInterjection(s) = bearerShape.redact(redactText(s, p.KnownSecrets, nil))`，只在 `Submit` 第 2 步调用一次，落库的就是脱敏后的 body。此后**所有**出口（实时 `Deliver` 写 stdin、开场补投拼 prompt、`GET /interjections` 响应、界面）都只读这份 body。于是实时投递与补投**都**经过了权威层（`KnownSecrets` 精确替换）+ 形态层，初稿「只有补投走权威层、实时投递有缺口」的问题消失。`redactText` 是包内函数，`Submit` 的实现就在 runner 包（`interject.go`），**不需要导出、也不复制一份**；`httpapi` 只持有 `Interjector` 接口 |
| Bearer 形态 | 现有 `redactText` 不认 `Bearer <opaque>`（评审期实测：`Bearer abcdefghijklmnopqrst` 原样输出，`evidence/f01/review-r1/code-probes/output.txt`）。f01 在 `interject.go` 里加一条**插话专用**形态 `(?i)\bBearer\s+([A-Za-z0-9\-._~+/]{16,}=*)`（group 1 抹掉，`Bearer ` 保留），复用 `tokenShape.redact`（同包）。**最小长度 16**；少于 16 字符的串（如 `Bearer abc`）**不在保证范围内** —— 形态层无法可靠区分短串与普通文字。**不改** `redact.go` 的全局 `tokenShapes`：那是失败记忆/收件箱/回放共用的安全面，f05 也在改同一张表（f05 PRD §3 第 9 条），并行改会撞车；把它提升为全局形态作为后续单独 PR 的建议写进 f01 的 PR 描述 |
| 日志 | 插话正文进日志时只记 `len` 与 `id`，正文只进 DB 与界面（`§7` 红线） |
| 注入风险 | 插话正文进模型上下文，**等价于 issue 正文**，威胁模型不变。但插话可以说得比 issue 更狠（「忽略验证」），所以 prompt 里的包裹语固定写明「不得据此跳过验收标准或验证步骤」；验证由 `stageVerify` 独立执行，agent 说什么都改不了判定（G5） |
| env 白名单 | 不新增子进程 env（`sanitizedEnv` 不动） |
| 子进程 Dir | 插话复用**已有**会话，不新起进程 |

**跨节点演进的注记**（写进 `docs/features/interject.md`）：若将来多节点（P3）落地，把 `Registry` 的查找换成
`tasks.node_id` + DB 租约 + `pg_notify`（pgx 原生支持，**不是新依赖**）即可；表结构与 API 不变。

### 3.8 并发与多节点

- **同进程**：注册表 `Mutex` + 两个 `map`；`Deliver` 与 `Unregister`/stdin 关闭并发安全（关闭先置 `closed`，
  再把排队帧以 `ErrSessionClosed` 回给调用方）。**必须** `make test-race`（AGENTS.md §3：涉及 runner 的并发逻辑）。
- **DB 侧**：所有 `task_interjections` 写入在 `pg_advisory_xact_lock(task_id)` 下进行（rev 单调 + 与收尾结算互斥）；
  `Submit` 的状态判定用 `tasks … FOR SHARE`，与 `Transition` 的 `FOR UPDATE` 互斥。锁顺序固定为
  「advisory → tasks 行」，`Transition` 只拿 tasks 行锁，无环。先例：`internal/flow/service.go:323`。
- **多节点**：本 PRD **明确不支持**（§3.0、§4）。注册表查不到即 `202 + queued`，由补投 / 结算兜底。
- **进程重启**：见 §3.4「启动清扫」。

---

## 4. 范围外（谁的地盘）

| 不做 | 属于谁 | 为什么边界要划清 |
|---|---|---|
| 用量统计（插话带来的 token/cost 归集） | **f03** | 本 PRD 定义多 result 时 `NumTurns`/`DurationMS` 累加、其余覆盖（§3.2 第 5 条）；`Usage`/`CostKnown` 的多 result 口径写死在 §3.2 第 5 条的表（随末条覆盖、回落也只取末条、不跨 result 相加），与 f03 §3.1 的表逐字相同、两边各存原文不互指；测试 `TestResultEventUsageTakesLastResult` 只由 f03 写；聚合视图归 f03 |
| 运行回放（时间线分组、折叠、过滤） | **f04** | f01 **不改** `AgentEventItem.vue`、不改分组逻辑、不改 `GET /events`；对 `TaskDetail.vue` 只加 2 行（§3.5 表）。**f04 PRD 现文与此相反**（§3.7 第 404 行、§6 第 555 行仍写「f01 在 `/events` 响应里加 `interjections`、`ev-interject` 按 `at` 合并进时间线由 f01 负责」），那是按 f01 初稿写的；R1 起 f01 已撤回该做法。**以本 PRD 为准**：f01 不往 `/events` 加任何字段、不产生 `ev-interject` 行，f04 的 `replay.js` 不需要为插话做任何事。f04 线需把这两行同步为「f01 只在 `TaskDetail.vue` 加 2 行挂独立面板，不碰 `/events` 与时间线」；f01 不改 f04 的 PRD。若 f04 不同步，对合入也无实际影响（f04 写的是「由 f01 负责」，f01 不做就是没有这部分逻辑，不会有半截实现），但文档会误导，所以 PR 描述里点名 |
| 多 CLI 驱动（codex） | **f06** | 驱动改造是 **claude 专属**（`--input-format stream-json`）。对 f06 `supervise()` 的确切要求（S1 touch 位置、S2 kill 前关闭钩子、S3 首条 result 关闭在 parse 内）与互相 rebase 规则见 §3.2 第 6 条；驱动能力判定与 `422` 见 §3.2 第 7 条、§3.3。f06 不给 codex 做插话 |
| 通知（插话投递失败推送） | **f09** | 只在界面显示失败；**不发**邮件/IM |
| 团队角色（谁能给团队仓库的任务插话） | **f12** | 只认「任务属主」 |
| PAT 鉴权 | **f05** | 鉴权沿用现有 session/Auth |
| 全局脱敏形态表（`redact.go` 的 `tokenShapes`） | 独立 PR（f05 也在改） | f01 的 Bearer 形态只作用于插话（§3.7） |
| 跨节点投递 | 无人（明确不做） | P3 落地时再按 §3.7 的注记演进 |
| 插话影响/跳过验证 | **永不做** | G5：`verifications` 与红-绿证据的写入路径在 `stageVerify`，本 PRD 不触碰 |

**另外明确不做**：中途取消单条插话、编辑已发送的插话、把 `failed` 的插话自动重投（人可以重发）。

---

## 5. 验收标准（每条可机器验证）

> 全部命令在 `/opt/lathe-wt/f01-interject` 内、先 `source /opt/lathe-wt/env/f01.sh`。
> 「先红」= 在改动**前**跑，必须失败；「后绿」= 改动后必须通过。每条 AC 都注明红/绿。
>
> **先红分两类，必须分清（AGENTS.md §5.4 的落地要求）**：
> - **A 类（优先）**：断言在**改动前的代码**上就能写出来并失败。这类必须真红，PR 贴断言原文。
> - **B 类**：断言引用本 PR 才引入的类型/字段/路由，改动前只能**编译失败**或 404。
>   **B 类必须额外配一条 A 类回归红线**（改动前绿、改动后必须仍绿）。下面每条 B 类 AC 都点名它搭配的红线。

### 数据层

**AC-1（迁移可逆，按 schema 断言）** — A 类
```bash
q() { docker exec lathe-postgres-dev psql -U lathe -d lathe_f01 -Atc "$1"; }
cols="SELECT count(*) FROM information_schema.columns WHERE table_name='task_interjections'"
seq="SELECT count(*) FROM pg_class WHERE relname='task_interjections_rev_seq'"
q "$cols"                       # 改动前：0（评审期已在 lathe_f01 实测为 0，schema 版本 31）
lathe_migrate up   && q "$cols" # 12
lathe_migrate down && q "$cols" && q "$seq"   # 0 与 0（表与序列都没了）
lathe_migrate up   && q "$cols" # 12
```
断言：每条命令 exit 0，四次计数依次为 `0 → 12 → 0/0 → 12`；12 列为
`id/task_id/user_id/body/status/error/session_id/phase/delivery_id/created_at/delivered_at/rev`。
**先红**：改动前 `lathe_migrate up` 无事可做、`$cols` 仍为 0，断言「12」失败（初稿写的「down 报没有可回滚的迁移」
是错的 —— 改动前 down 会正常回滚 0031 并 exit 0，评审已指出；本 AC 不用 down 的退出码做红线）。
**纪律**：先红阶段跑完 `down` 要立刻 `up` 回到 31，别把 `lathe_f01` 留在 30。

**AC-2（CHECK 约束真的挡住了）** — 单测 `TestInterjectStatusCheck`（B 类：表由迁移建，不写 fixture）
- `status='bogus'` / `delivered` 且 `delivered_at IS NULL` / `failed` 且 `error IS NULL` / `queued` 带 `error` /
  `body` 长度 16385 → 全部 SQLSTATE `23514`；`delivered` 带 `error='no_replay_ack'` → **成功**。断言写 `got/want`。
**先红**：先查 `information_schema.columns` 得到 0 行，断言「表存在」失败（跑到断言，不用 `t.Fatal` 提前返回）。
搭配 A 类红线：AC-1。

**AC-3（编号与成对）** — `ls migrations | grep 0032` 恰好是 `0032_interjections.up.sql` + `.down.sql`；
`git diff --name-only origin/dev... -- migrations/` 只有这两个文件。

### 驱动层

**AC-4（stdout 事件不被污染）** — 单测 `TestParseStreamFiltersReplayAndLifecycle`，**A 类**
输入 `/opt/lathe-wt/evidence/f01/fixtures/replay-user.ndjson`（真实捕获，13 行），**只用既有签名**
`parseStream(f, onEvent)`，对每个交给 `onEvent` 的事件跑 `Digest`。断言：
- `onEvent` 被调用 **6** 次（2 system + 2 assistant + 2 result），类型里**没有** `user` 与 `command_lifecycle`；
- 由 `type=user` 事件产出的 `Digest` 条目 **0 条**（任何 kind —— 重点是 `text`：人插的话不得以 agent 正文出现）；
- `kind=raw` 条目 **0 条**；
- 返回的 `Result.Text == "SECOND"`、`CostUSD == 0.0976604`（末条累计）、`NumTurns == 2`、`DurationMS == 2759+2499`。
**先红（评审期已在 cff5332 上实测，`evidence/f01/review-r1/code-probes/output.txt`）**：今天 `onEvent=13`，
`user->text:2`、`command_lifecycle->raw:5`，`NumTurns==1`、`DurationMS==2499` → 前四条断言全部失败。
（初稿断言的「0 条 `tool_result`」今天就成立，不是红线，已删除。）
这份 fixture 里的 `system` 只有 `init`，所以「`kind=raw` 0 条」在**这份输入**上成立；它不推广到真实运行（见 AC-4c）。

**AC-4c（真实捕获：只消除 f01 引入的两类污染，既有 raw 原样保留，R3 新增，回应 R3 B1）** — `TestParseStreamKeepsExistingSystemRaw`，**A 类**
输入 `review-r2/resume-stream/{a-new-session,b-resume-stream-inject}.jsonl`（真实 CLI，PR 里拷进 `testdata/`，入库前已扫 token 形态 0 命中），
只用既有签名 `parseStream`，对每个事件跑 `Digest`，按「来源 `type[/subtype]` → `kind`」计数（形状同 `review-r3/raw-probe/raw_probe_test.go`）。断言：
| 计数 | a 段 want | b 段 want | 今天（cff5332 实测） |
|---|---|---|---|
| `command_lifecycle→*` | 0 | 0 | 3 / 6 → **红** |
| `user→text` | 0 | 0 | 1 / 2 → **红** |
| `user→tool_result`（真·工具结果） | 0 | **1** | 0 / 1（绿，挡「把非 replay 的 user 也过滤掉」） |
| `system/thinking_tokens→raw` | **18** | 0 | 18 / 0（绿，挡「顺手过滤 system」） |
| `system/task_started→raw` + `system/task_notification→raw` | 0 | **2** | 0 / 2（绿，同上） |
| `system/init→init` | 1 | 1 | 1 / 1 |
**先红**：前两行今天失败（`review-r3/raw-probe/output.txt`）；后三行改动前后都绿，是「白名单放行 `system`、`digest.go` 不改」的机检。

**AC-4b（被过滤的事件仍然喂看门狗）** — `TestFilteredEventsStillTouchIdle`，A 类回归红线
假 CLI：每 300ms 输出一行 `{"type":"command_lifecycle",…}`，共 2.4s，然后输出 result；`Driver.idle=1s`。
断言 `Run` 返回成功、**不是** `ErrIdle`。改动前绿（今天每行都经 `onEvent` 包装里的 `idle.touch()`）；改动后必须仍绿 ——
它挡住「把过滤放在 `idle.touch()` 之前」这种实现。另加一条 B 类：`StreamInput=true` 下同样输入同样结论。

**AC-5（注入真的到了正在跑的进程）** — 集成测试 `TestSessionDeliverMidRun`，B 类
用 `fakeClaude`（`agent_test.go:283` 的既有 helper）起假 CLI：逐行读 stdin，对每帧回显
`{"type":"user","isReplay":true,"uuid":<帧 uuid>,…}`；读到第二帧后输出 result，然后 `cat >/dev/null`（**一直等 EOF**）再退出。
断言：(a) 调用方传入的 `deliveryID` 出现在 `OnDelivery` 回调里，且**先 `Expect` 后 `Deliver`** 的顺序下，
即使假 CLI 在读到帧的同一时刻回显（回显早于 `Deliver` 返回），ack 也被接住（不落 `no_replay_ack`）；非法 `deliveryID` → `ErrInvalidDeliveryID`、不写入；
(b) result 之后 `Session.Closed()` 为真，
再 `Deliver` 返回 `ErrSessionClosed`；(c) 单 result 时 `Result` 各字段与 `StreamInput=false` 跑同一 result 行逐字段相等。
搭配 A 类红线 `TestBuildArgsDefaultModeUnchanged`：**改动前就写**，对 `buildArgs(RunParams{Prompt:"x"})`
逐字快照 `--print --output-format stream-json --verbose x`，改动前后都必须绿。

**AC-5d（stdin 不死锁，三条路径）** — `TestStreamInputStdinLifecycle`，B 类，表驱动，每条都断言耗时上限：
| 假 CLI 行为 | 期望 |
|---|---|
| 不读 stdin，`sleep 1` 后输出 result 退出 | `Run` 在 **< 3s** 返回成功（`io.Pipe` 实现在这里会挂到总超时） |
| 输出 result 后 `cat >/dev/null`（等 EOF），`Driver.idle=5s` | `Run` 在 result 之后 **< 2s** 返回成功，`errors.Is(err, ErrIdle)==false` —— 证明是我们关的 stdin 让它退出，而不是看门狗 |
| `sleep 30`，`Driver.timeout=1s` | `Run` 在 **< 3s** 返回「执行超时」（ctx 杀进程组路径能收回 Wait） |
搭配 A 类红线：既有 `TestRunWithFakeBinary` 系列（改动前后都绿）。

**AC-6（投递不阻塞）** — `TestDeliverBackpressureDoesNotBlock`，B 类
假 CLI 不读 stdin；连续 `Deliver` 直到写满管道与 channel，断言某次 `Deliver` 在 **< 3s** 内返回
`ErrDeliverBackpressure`（`time.Since`，`got/want`）。搭配 A 类红线 `TestRunDoesNotWaitForSink`
（**新写、改动前提交**：每次写入 `sleep 200ms` 的假 `AgentEventRecorder`，`Run` 总耗时 ≈ CLI 自身耗时；改动前后都绿）。
AC-6b 同样搭配这条红线（它守的是「读协程不等下游」这一既有性质，AC-6b 把同一性质推广到插话的 ack 路径）。

**AC-6b（慢 DB 不拖慢读协程，G4）** — `TestSlowStoreDoesNotSlowReadGoroutine`，B 类（R2 重写，回应 B2）
R1 原文「每次写库 sleep 3s + `Run` < 1.5s + 3 条都 delivered」自相矛盾：Submit 的 INSERT 与 `MarkDelivering` 都是写库，
每条 Submit 至少 6s 后才调到 `Deliver`，假 CLI 要么等不到帧、要么 `Run` 必然 ≥ 6s。改为**只让「读协程有可能被错误地
拉去做」的那一步变慢**，并把耗时断言换成与 Submit 前序写库无关的量：
- 假 store：`InsertInterjection`、`MarkDelivering` **不 sleep**；`MarkDelivered` 每次 **sleep 3s**（这是 R1 初稿错误实现
  会在读协程里同步调用的那一步）。假 store 在每次调用时检查一个由 `OnDelivery` 回调设置的 goroutine 标记
  （`OnDelivery` 入口记 `goid` 到 `atomic`，假 store 比对），命中即 `t.Error("写库发生在读协程上")`。
- 假 CLI（`fakeClaude`）：读首帧后进入循环，每读到一帧注入就回显 `isReplay`；读满 **3 帧注入**后，
  输出 50 行 assistant，再输出 result，然后 `cat >/dev/null` 等 EOF。
- 驱动：在 `OnReady` 里起 3 个 goroutine 各 `Submit` 一条（每条在 `MarkDelivered` 处各 sleep 3s，与读协程无关）。
- 断言（均 `got/want`）：
  1. 测试侧记录「第 3 条回显到达 `OnDelivery` 的时刻」`T3` 与「`OnEvent` 收到 result 的时刻」`TR`：
     **`TR − T3 < 1.0s`**（50 行 assistant + result 的解析时间；若读协程里同步写库，至少多出 3s）；
  2. `Run` 返回后等 3 个 `Submit` 全部返回（≤ 10s），库里 3 行都是 `delivered`、`error IS NULL`（回显已到，不是 no_replay_ack）；
  3. 读协程标记从未命中。
- 不断言 `Run` 的总耗时（它包含 Submit 前序写库与调度，与 G4 无关）。

### 管线层

**AC-7（非实现阶段不开流式输入，0 回归）** — `TestRunAgentStreamInputOnlyWithRegistry`，B 类
`Interjections == nil` 时 `RunParams.StreamInput == false` 且 `OnReady/OnDelivery == nil`；非 nil 时 `StreamInput == true`；
**分诊**（`stageTriage` 直连 `Agent.Run`）在任何装配下 `StreamInput == false`。
搭配 A 类红线 `TestTriageRunParamsCarryPromptAndNoStream`（假 Agent 捕获分诊 `RunParams`，断言 `Prompt` 非空）。

**AC-8（开场补投：跨轮不丢消息）** — `TestRunAgentPrefillsPendingInterjections`，**B 类**（R2 改分类，回应 B3：
它要预置 `queued` 行，而 `task_interjections` 在改动前不存在，改动前只能报「relation does not exist」，不是 A 类）
预置两条 `queued`，调 `runAgent`，假 Agent 捕获 `RunParams.Prompt`。
- 改动后：prompt 含两条正文与包裹语；假 Agent 返回 result → 两行 `delivered`、`phase` 为本次 phase、`delivery_id IS NULL`；
- 改动后另一子用例：假 Agent 返回 `nil, err` → 两行回到 `queued`（不是 `failed`，也不是卡在 `delivering`）。
**搭配 A 类红线 `TestRunAgentPromptUnchangedWithoutInterjections`**（改动**前**就写并提交，改动前后都绿）：
用 `pipelineFixture` 跑一次 `Execute`（同 `TestFailureMemoryAbsentLeavesPromptUnchanged` 的形状，`memory_test.go:459`），
断言实现阶段 `ag.calls[1].Prompt` **逐字等于** `ImplementPrompt(demoIssue().Context(), kind, branch)`（`pipeline.go:739` 的构造），
且不含「【人追加的指示」。改动后装配 `Interjections`、库里无插话行时这条必须仍绿 —— 它挡住「无插话时也往 prompt 追加空段落/包裹语」的实现。
**R3 澄清（回应 R3 B3）**：这条红线成立的**前提**是 fixture 没有失败记忆——`runAgent` 先调 `injectFailureMemory`（`pipeline.go:767`），
有记忆时 prompt = `ImplementPrompt(...)` + 记忆段，不再逐字相等。所以 fixture 显式 `p.Memory = newFakeMemory()`（空）**或** `nil`，
两种都跑一遍（子用例），与 `TestFailureMemoryAbsentLeavesPromptUnchanged`（`memory_test.go:459`）同一条件。
另一条既有回归 `TestPipelineResumeImplementWithSession`（`pipeline_resume_test.go:157`）改动前后都绿：resume 路径的 prompt 也不被补投逻辑改写。

**AC-8b（补投与失败记忆共存：顺序固定、包裹语恰好一次，R3 新增，回应 R3 B3）** — `TestPrefillAfterFailureMemory`，B 类，表驱动
复用 `TestFailureMemoryInjectedOnRetry`（`memory_test.go:156`）的两轮形状：第一轮失败写下记忆；第二轮之前预置两条 `queued` 插话
（正文 `ALPHA-<rand>`、`BETA-<rand>`），以 Fresh 计划重跑，假 Agent 捕获实现阶段 prompt `P`。设
`iMem = strings.Index(P, "## 这个需求此前失败过")`、`iInj = strings.Index(P, "## 【人追加的指示】")`。断言（`got/want`）：
| 子用例 | 断言 |
|---|---|
| 记忆 + 插话（phase=`implement`） | `iMem > 0`、`iInj > iMem`（**插话段在记忆段之后**）；`strings.Count(P, "## 【人追加的指示】") == 1`、`strings.Count(P, "不得据此跳过验收标准或验证步骤") == 1`、`strings.Count(P, "## 这个需求此前失败过") == 1`；`ALPHA` 的下标 < `BETA` 的下标且都 > `iInj`；`strings.HasPrefix(P, ImplementPrompt(...))`（base 在最前、原样）|
| 记忆 + 插话（phase=`implement-reentry`，复用 `TestFailureMemoryInjectedOnResumeDegrade` 形状：第一次 resume 返回 `nil, errors.New("No conversation found…")`，**行回到 `queued`**，降级新会话再补投） | 降级那次的 prompt 满足上一行全部断言；第一次（resume）调用的 prompt 也含插话段且恰好一次；两行最终 `delivered`、`phase='implement-reentry'` |
| resume 调用返回 `&Result{IsError:true, NumTurns:0, Subtype:"error_during_execution"}` 且 `err==nil`（§2.2 第 10 条的真实形状；既有管线据此 `p.fail`） | 该次 `finishPrefill` 后两行回到 **`queued`**（**不是** `delivered`）；`Execute` 返回、任务 `failed` → T1 结算 → 两行 `failed`、`error` 含「任务已失败」 |
| 仅插话、无记忆（`p.Memory` 空） | `iMem == -1`、`iInj > 0`、`strings.HasPrefix(P, ImplementPrompt(...))`、包裹语恰好一次 |
| 仅记忆、无插话 | `iInj == -1`；`P` 与改动前同一 fixture 的 prompt **逐字相等**（快照在改动前由 `TestFailureMemoryInjectedOnRetry` 的同形测试落盘） |
| `fix-1`（无记忆注入的 phase）+ 插话 | `iMem == -1`、`strings.HasPrefix(P, FixPrompt(...))`、插话段在末尾且恰好一次 |
**先红**：B 类（插话表在改动前不存在）。**搭配 A 类红线**：既有 `TestFailureMemoryInjectedOnRetry`、`TestFailureMemoryInjectedOnResumeDegrade`、
`TestFailureMemoryAbsentLeavesPromptUnchanged`（改动前后都绿、一行不改）——它们证明补投没有改变失败记忆的注入点、内容与「无记忆时不注入」；
以及上面的 `TestRunAgentPromptUnchangedWithoutInterjections`。

**AC-9（验证不受影响）** — **回归**：`make test` 全绿 + `make test-race` 全绿。
`TestStageVerifyUnchangedByInterjections`：同一 fixture 下跑 `stageVerify`，一次无插话、一次在 `implementing` 期间
插 3 条，`verifications` 行的 `(step, status, tier)` 序列逐项相等。先写无插话那一半并落快照（改动前即可跑），
改动后加另一半，快照必须相同。

**AC-18（判定是闸门：取消后不接受）** — `TestInterjectRejectedAfterCancelEvenWithLiveSession`，B 类
注册一个活会话，把任务转 `cancelled`，`POST /interject` → `409`，库里该行 `failed`，假 Session 的 `Deliver` 调用次数 0。
**搭配 A 类红线**（R2 补，回应 B3）：既有 `TestAPIRetryAndCancel`（`internal/httpapi/api_test.go:239`，改动前后都绿）——
它守住 `POST /cancel` 的既有语义（状态转 `cancelled`、重复取消的返回码），证明 f01 没有为了「取消后拒绝插话」去改
`cancelTask` 或状态机；另加 `TestRunContextCancelKillsProcess`（`agent_test.go:450`，改动前后都绿）守住 ctx 取消仍能杀进程。

**AC-19（queued 一定有结局）** — `TestQueuedInterjectionsSettledWhenTaskLeavesVerify`，B 类，表驱动：
| 场景 | 期望 |
|---|---|
| `verifying`、无活会话时插 1 条（202 queued）→ 验证**一次通过** → `Execute` 返回、任务 `pr_open` | 该行 `failed`，`error` 含「没有后续 agent 轮次」；**不是** `queued` |
| 同上但任务 `gate_mode=manual` → `awaiting_approval` | `failed`，同上文案 |
| `verifying` 无会话插 1 条 → 验证失败、修复轮起 | 该行在修复轮 prompt 里出现，`delivered`，`phase='fix-1'` |
| 插 1 条后任务经 `p.fail` 转 `failed` | `failed`，`error` 含「任务已失败」 |
| 插 1 条后任务被转 `cancelled`（在途 agent 跑完、`Execute` 返回） | `failed`，`error` 含「已取消」 |
| 插 1 条后 `Execute` 返回时任务仍是 `queued`（被重新入队） | 仍 `queued`（留给下一轮补投） |
| 竞态：`Submit` 事务 A 与 `verifying → pr_open` 转移并发 100 次（`-race`） | 每次结局只有两种：409（读到新状态）或 `failed`（被结算）；**0 行停在 `queued`** |
| **竞态（R2 新增，B1(b) 的时序）**：有活会话，`Submit` 已 `MarkDelivering`；用注入的假 Session 让 `Deliver` 阻塞在一个闸门 channel 上；此时让 `Execute` 走完（result → 任务 `verifying → pr_open` → `settleInterjections` 返回，它**跳过**这条 `delivering` 行）；再放开闸门让 `Deliver` 返回 `ErrSessionClosed` | 该行 **`failed`**（`error` 含「没有后续 agent 轮次」），Submit 返回 **409**；**不是** `queued`、不是 `delivering` |
| 同上但放开闸门前任务仍是 `verifying`（修复轮之间） | 该行 `queued`（`delivery_id/phase/session_id` 已清空），Submit 返回 202；随后的 `fix-1` 补投它 → `delivered`、`phase='fix-1'` |
| 上面两行交错 100 次（`-race`，用 `sync.WaitGroup` 随机化 `settle` 与回退的先后） | 终局只有 `failed`、或 `queued` 且随后被补投/结算；**结束后 0 行 `queued`、0 行 `delivering`** |
| **R3 新增（B2，`Execute` 不再运行的路径）**：`implementing` 无会话插 1 条（202 queued）→ `Execute` 返回时任务 `queued`（被重新入队，行保留）→ **不起 worker**，直接调真 `cancelTask`（`POST /api/tasks/T/cancel`）→ `GET /interjections?after_rev=0` | 响应里该行 **`failed`**、`error` 含「已取消」、`rev` 大于插入时的 rev；**进程不重启、`Execute` 不再被调用**（假 Agent 的调用计数不变） |
| 同上，但取消走 `queue.CancelForIssue`（内置工单取消联动） | 同上 |
| 同上，但任务被 `blocked_dep` 级联（前驱任务 `p.fail`，`machine.go:898`） | 同上，`error` 含「前置任务失败」 |
| 同上第一行，但**不调** `GET`，直接调周期清扫一次（`sweepInterjectionsOnce(ctx)`，T3 的单次体） | 库里该行 `failed`（无读者也会结算） |
| 同上第一行，但任务被转回 `queued` 后**未被取消** → `GET` | 仍 `queued`（还会再跑，读时结算不误判） |
| 机检（R3）：`git diff origin/dev...HEAD --name-only -- internal/task/ internal/httpapi/api.go cmd/lathe/queue.go` | `internal/task/` 无文件；`api.go` 的 diff 只在 `Routes` 加两行路由与新 handler，`cancelTask` 函数体不变（`git diff -U0` 的 hunk 不落在 `cancelTask` 行区间内，脚本核对）；`queue.go` 无改动 |
搭配 A 类红线：`TestPipelinePROpenNotifiesOwner`（`notify_test.go:251`）等既有 pr_open 路径测试改动前后都绿。

**AC-20（客户端断开不留 `delivering`，R2 新增，回应 B1(a)）** — `TestSubmitSurvivesClientDisconnect`，B 类
`httptest.NewServer` 起真 handler；假 Session 的 `Deliver` 写成功；假 CLI 侧故意**不回显**（于是 Submit 进入「等 ack ≤ 2s」）。
客户端用可取消的 `http.Request`，在服务端进入等 ack 后（测试通过一个 hook channel 得知）**取消请求 / 关闭连接**。断言：
1. 服务端 handler 在 **≤ 3s** 内返回（ack 超时 2s + 写库）；
2. 库里该行 **`delivered`、`error='no_replay_ack'`**（不是 `delivering`）；
3. 第二个子用例：断开发生在 `Deliver` 之前（hook 在 `MarkDelivering` 之后触发），`Deliver` 返回 `ErrSessionClosed` 且任务已 `pr_open`
   → 行 `failed`（闸门回退在 `WithoutCancel` 的 ctx 上照样写进去）；
4. 第三个子用例：断开发生在事务 A 提交**之前**（hook 在 `INSERT` 之前触发并取消）→ 库里**无行**（事务回滚，客户端也没拿到任何承诺）。
**先红**：路由不存在（同 AC-10 的 JSON 断言）。**搭配 A 类红线**：`TestStartRoundSurvivesRequestCancel`
（`internal/prd/service_flow_test.go:374`，既有，改动前后都绿）——仓内同一手法「请求取消后后台写仍完成」的先例，
f01 照抄它的 `context.WithoutCancel` 写法；以及 AC-16 的启动清扫作为 DB 整体不可用时的最后兜底。

**AC-21（驱动不支持 → 422，R2 新增，回应 B4(b)）** — `TestInterjectDriverUnsupported`，B 类
`Interjector` 注入 `streamInputSupported = func(*task.Task) bool { return false }`；任务 `implementing`、注册了活会话。
断言：`POST /interject` → **`422`**，body `reason=="driver_unsupported"`、`error` 含「不支持运行中插话」；库里该行 `failed`；
假 Session 的 `Deliver` 调用 0 次；同一装配下 `runAgent` 捕获的 `RunParams.StreamInput == false`、`OnReady == nil`。
另一子用例：任务 `pr_open` 且驱动不支持 → **409**（状态先判，§3.3 判定顺序）。
`dev` 上（f06 未合入）`streamInputSupported` 的生产实现恒真，由 `TestStreamInputSupportedDefaultsTrue` 断言；
f06 合入后改为 `CapabilitiesOf` 并把该测试改成表驱动（claude → true、codex → false）—— 这一处改动列入 §3.2 第 6 条的 rebase 规则。
**搭配 A 类红线**：AC-5 的 `TestBuildArgsDefaultModeUnchanged`（不开流式时参数逐字不变）。

### API 层

**AC-10（属主 404）** — `TestInterjectNonOwner404`
用户 B 对用户 A 的任务 `POST /interject` 与 `GET /interjections` → `404`，库里无新增行。
**先红**：路由不存在时 mux 回纯文本 `404 page not found`，所以断言同时检查 `Content-Type: application/json`
与 body `{"error":"任务不存在"}`；先红时报「Content-Type = text/plain，期望 application/json」。

**AC-11（状态码与落库）** — `TestInterjectStatusCodes`，表驱动：
| 任务状态 | 会话 | 期望 | DB |
|---|---|---|---|
| `implementing` | 活会话 | `200 delivered` | `delivered`，`delivery_id` 非空 |
| `verifying`（修复轮在跑） | 活会话 | `200 delivered` | `delivered` |
| `implementing` | 活会话但回显 2s 内未到 | `200 delivered` | `delivered`，`error='no_replay_ack'` |
| `implementing` | 无活会话 | `202 queued`（`reason:"no_live_session"`） | `queued` |
| `implementing` | 会话已出 result（`Closed()`） | `202 queued` | `queued` |
| `verifying` | 无活会话（正在跑 `go test`） | `202 queued` | `queued` |
| `implementing` / `verifying` | 有活会话但背压 | `503`（文案含「投递通道拥塞」） | `failed` |
| `implementing` | 活会话，但驱动不支持（注入假判定，AC-21） | `422`（`reason:"driver_unsupported"`） | `failed` |
| `pr_open` | — | `409`（文案含「没有运行中的 agent」） | `failed` |
| 任意 | — | 8193 个 `a` | `413`，DB 无行 |
| 任意 | — | 窗口内第 6 条 | `429`，DB 无第 6 行 |
| `Interjector == nil` | — | `503` | — |
**先红**：路由不存在（JSON 断言同 AC-10）。

**AC-12（限流与配置项消费）** — `TestInterjectRateLimitAndConfig`
`LATHE_INTERJECT_RATE_LIMIT=2` 连发 3 条 → 第 3 条 `429`；`LATHE_INTERJECT_MAX_CHARS=10` 下 11 个字符 → `413`；
`=16385` / `=0` / `LATHE_INTERJECT_WINDOW=1d` → `config.Load()` 报错且**点名变量**。
**先红**：配置校验是 A 类 —— 改动前 `LATHE_INTERJECT_MAX_CHARS=0` 时 `Load` 正常返回，断言「报错」失败；
限流/长度是 B 类（路由不存在）。

**AC-13（单一脱敏点，出口全不出明文）** — `TestInterjectRedactsAtSingleSubmitPoint`，B 类
`Pipeline.KnownSecrets` 登记一个值 `S`（≥8 字节）。发送一条消息，含：`ghp_` + 36 位、`sk-ant-` + 20 位、
`Authorization: Bearer ` + 20 位 `[A-Za-z0-9]`、以及 `S`。断言以下**四个出口**都不含任一原文、都含 `[REDACTED]`：
① 库里 `body`；② `GET /interjections` 响应；③ 活会话路径下假 CLI 从 stdin 收到的帧；
④ 无会话路径下，下一次 `runAgent` 捕获的 prompt。另断言：⑤ 该请求期间 `slog` 输出（`bytes.Buffer` handler）不含原文；
⑥ `Bearer abc`（<16 字符）**不做断言**（不在保证范围，§3.7）。
搭配 A 类红线 `TestRedactTextShapesBaseline`：改动前就写，对既有 `redactText` 断言 `ghp_…`、`sk-ant-…` 被替换
（评审期实测今天就成立）；以及**记录** `Bearer <20 位>` 今天**不**被替换（断言原样输出）—— 它证明 Bearer 那条形态是
f01 新加的，且 f01 没有改动全局 `redactText` 的行为（改动后这条仍须绿）。

**AC-17（`/interjections` 增量游标）** — `TestInterjectionsRevCursor`，B 类
1. 插 1 条（queued），`GET ?after_rev=0` → 返回该行，记 `R1 = rev`；
2. 不产生任何 `agent_events`、也不新增插话，再 `GET ?after_rev=R1` → `interjections:[]`、`rev == R1`（空批原样回显）；
3. 仅把该行由收尾结算改成 `failed`（`agent_events` 游标此时完全空闲）→ `GET ?after_rev=R1` → **返回同一 id、`status=failed`**，`rev > R1`；
4. 插 205 条（绕过限流的 store 直插）→ 首次 `more:true` 且 200 行，再拉一次拿到剩余 5 行、`more:false`；
5. `after_rev=-1` / `abc` → `400`；
6. 并发：两个 goroutine 各插 50 条 + 一个 goroutine 每 10ms 轮询，最终客户端 upsert 出的集合 = 库里 100 行（不漏）。
**搭配 A 类红线**（R2 补，回应 B3）：既有 `TestTaskEventsEndpoint`（`internal/httpapi/events_test.go:14`）与
`TestTaskEventsIsolation`（`:72`）改动前后都绿 —— 前者断言 `/events?after=0` 返回全部、`after=last_id` 返回空且
**`last_id` 原样不动**、响应只有 `events`/`last_id` 两个键之外不新增语义；后者守住 `/events` 的属主过滤。
另在改动**前**给 `TestTaskEventsEndpoint` 加一行断言「响应顶层键集合恰为 `{events, last_id}`」（改动前绿），
它挡住「顺手往 `/events` 里加 `interjections`」的实现（§3.3 撤回扩展 `/events` 的决定由它机检）。

### 真实运行证据（必须）

**AC-14（驱动级真实运行证据，禁止用 dev 实例/真凭据之外的东西）** — 测试 `TestRealCLIInterject` + 包装脚本
`/opt/lathe-wt/evidence/f01/run_real_cli.sh`
```bash
export HOME="$(mktemp -d)"; mkdir -p "$HOME/.claude"
cp ~/.claude/.credentials.json "$HOME/.claude/" && chmod 600 "$HOME/.claude/.credentials.json"
REPO="$(mktemp -d)"; cd "$REPO" && git init -q && git commit -q --allow-empty -m init
MARK="INTERJECT-MARK-$(uuidgen)"
EVIDENCE=/opt/lathe-wt/evidence/f01
# 用 Lathe 自己的驱动（不是裸 CLI）起一次真实运行：
#   go test ./internal/integration/agent -run TestRealCLIInterject -v \
#     -args -bin claude -dir "$REPO" -mark "$MARK" -out "$EVIDENCE/real-cli-$(date +%s)"
# agent.NewDriver + RunParams{StreamInput:true, OnReady:…}；首条 prompt 让模型 Bash sleep 8，
# 在 sleep 窗口里 Deliver("请在你的最终回答里原样包含 <MARK>")。
# 输出：real-cli-<ts>.jsonl（stdout 全量）、real-cli-<ts>.meta.json（delivery id / 回显行号 / result 时刻 / 进程退出时刻）
```
期望（脚本自检、exit 非 0 即失败）：
1. stdout 出现 `isReplay:true` 且 `uuid` 等于注入帧 → 投递被 CLI 确认；
2. 末条 `result` 的文本含 `MARK` → 注入被模型吸收（强于 AC-5 的假 CLI）；
3. **进程在首条 result 之后 ≤ 10s 内退出**（meta 里两个时刻相减），且 `Run` 不是 `ErrIdle` —— 用真实 CLI 证明
   「首条 result 即关 stdin」让进程退出，而不是靠 CLI 自己退或靠看门狗（R1 新增，对应评审 90.3s 的复测）；
4. 同一脚本第二段：在首条 result **之后** `Deliver` 一次 → 返回 `ErrSessionClosed`、stdout 里**只有 1 条** result；
5. **第三段：`--resume` + 流式输入 + 运行中注入（R2 新增，回应 B5）**：用第一段的 `SessionID`，
   `RunParams{SessionID: sid, Resume: true, StreamInput: true, …}`（即 `buildArgs` 产出 `--resume <sid> --input-format stream-json
   --replay-user-messages`，与 `fix-N`/review 二轮在生产上的参数形状一致），首帧「先 `Bash sleep 8`，然后原样回答你第一段
   最终回答里的暗号」，在 sleep 窗口里 `Deliver("最终回答里还要原样包含 <MARK2>")`。期望：
   注入帧回显 uuid 一致；**只有 1 条** result；result 文本同时含第一段的暗号（证明真的续上了会话）与 `MARK2`（证明运行中注入被吸收）；
   result 的 `session_id` 等于 `sid`；进程在 result 之后 ≤ 10s 退出且非 `ErrIdle`。
   裸 CLI 的同形实验已做（P10，§2.2 第 8 条：`ZEBRA42\n\nMARK-6a6b4ed4`、单 result、result 后 0.277s 退出）；本段是用
   **Lathe 自己的驱动**再证一次；
6. 仓库 `git status --porcelain` 除预置文件外无变化（不外推任何仓库）；
7. 证据入库前扫一遍 token 形态串与真实路径。

> 纪律：该测试**默认 `t.Skip`**（要真 CLI 与真凭据，不能进 `make test`），但**必须**在 PR 里贴一次完整输出；
> 当天凭据不可用时**不允许**改写成「假 CLI 通过就算过」：如实写「未取证」并把 PR 标 draft 等人拍板；
> 全程隔离 `HOME`、临时仓库、**不碰** `:8200`/`:8300` 实例与其库、不外推任何 GitHub 仓库。

### UI

**AC-15（界面手测路径 + 共享文件边界）** — PR 里贴截图 + 步骤。手测**在 AC-22 的那次 :8401 运行上做**
（任务来源、仓库、停止点都由 AC-22 的脚本定义；R1 写的「起一个任务、一路到 pr_open」没有可执行来源，已改）：
AC-22 第 5 步任务处于 `implementing` 时打开 `http://127.0.0.1:8401/tasks/<T>`；面板出现 → 发一句 → 列表出现「你」的条目并
变为「已送达」；任务进 `verifying`（`fix-1` 在跑）时输入框仍在并带提示；任务进 `awaiting_approval` 后输入框消失、列表仍在；
`verifying` 期间、修复轮之间（`go vet` 在跑、无活会话）发的一句在 `awaiting_approval` 之后显示「已送达」（被 `fix-1` 补投）
或「未进入模型」与原因（AC-19 的界面面）——两者都截图，各自对上库里该行的 `status/phase/error`。
机器检查：
- `git diff origin/dev...HEAD --numstat -- web/src/views/TaskDetail.vue` = `2	0`；
- `git diff origin/dev...HEAD --name-only -- web/src/components/AgentEventItem.vue` 为空；
- `grep -c 'v-html' web/src/components/InterjectPanel.vue` = 0；
- `grep -c 'InterjectPanel' web/src/views/TaskDetail.vue` = 2。
**先红**：`InterjectPanel.vue` 不存在、`grep -c InterjectPanel TaskDetail.vue` = 0。

### 端到端真实任务证据（AGENTS.md §3「agent 提示词 / 驱动」行）

**AC-22（:8401 上一次真实任务：新会话 implement + `--resume` 的 fix-1 + 运行中插话，R2 新增，回应 B5）**
形状照 f06 AC-23（f06 PRD 第 919-960 行），全程只用本线资源：worktree 的 `bin/lathe`、库 `lathe_f01`、端口 8401、
本机 bare 仓库、隔离 `HOME` 下的真 claude CLI。**不连 GitHub/Linear、不推送任何远端、不碰 :8200/:8300 及其库**。
脚本 `/opt/lathe-wt/evidence/f01/e2e/run.sh` 入证据目录（PR 里贴全量输出），步骤：

0. **前置**：`psql "$LATHE_TEST_DSN" -Atc "SELECT count(*) FROM tasks WHERE state NOT IN ('merged','failed','cancelled')"` 必须是 `0`
   （serve 会领库里所有可领的任务，测试残留会被它跑起来）。不是 0 就**停下问人**，不自己删（AGENTS.md §4）。
   同时记 `P0=$(sha256sum /opt/lathe/bin/lathe)`，结束时比对（生产二进制未被碰）。
1. `make build`（只写本 worktree 的 `bin/`）；`lathe_migrate up`。
2. 临时目录 `E=$(mktemp -d)`：`$E/data`、`$E/ws`、`$E/home`；
   - **隔离 HOME**：`mkdir -p $E/home/.claude && cp ~/.claude/.credentials.json $E/home/.claude/ && chmod 600 …`（P7：不复制则 `Not logged in`）；
     serve 以 `HOME=$E/home` 启动，`sanitizedEnv` 白名单里的 `HOME`（`agent.go:479`）于是把隔离 HOME 传给 claude 子进程；
   - **本地 fixture 仓库** `$E/fixture.git`（bare，`main`），三个文件（R4 改，见 §9 R4 B2）：
     - `go.mod`：`module f01e2e` + `go 1.22`；
     - `calc.go`：`func Add(a, b int) int { return a - b }`，以及同文件里一段**只有全量 `go vet` 会报、`go test` 与 `go build` 都照过**的写法——
       `import "sync"`、`type S struct{ mu sync.Mutex }`、`func Copy(s S) S { return s }`（vet 的 copylocks 检查：`passes lock by value`）；
     - `calc_test.go`：只有 `TestAdd`，断言 `Add(2,3)==5`，失败信息带 got/want；**不放**任何 vet 会报的写法。
     light 档验证对 Go 仓库是 `go build -buildvcs=false ./...` + `go vet ./...`（`verify.go:193`、`verify_test.go:24-41`）。
     **为什么这样能确定性地出 fix-1**：`go test` 编译测试时只跑 vet 的一个子集（atomic/bool/buildtags/directive/errorsas/ifaceassert/
     nilfunc/printf/stringintconv/tests），copylocks 不在其中。实现提示词（`prompt.go:71` 起）只要求 agent 交代跑了什么测试、
     自验证红绿——agent 照做跑 `go test ./...`，修好 `Add` 后看到的是 `ok`，没有任何信号指向 `Copy`；issue 正文只写「修好 Add」、
     不提 `Copy`/vet。于是第一轮 verify 的 `go vet` 红 → `fix-1` 以 `--resume <implSession>` 起（`pipeline.go:1003-1010`），
     `FixPrompt` 把 vet 输出喂回去，修复轮改掉 `Copy` 后 verify 绿。
     **R3 写法作废的原因**：R3 把 `fmt.Printf("%d", "x")` 放在 `calc_test.go`，而 printf 正属于 `go test` 自带的 vet 子集——
     `go test ./...` 直接 `FAIL f01e2e [build failed]`，修好 `Add` 后仍是 build failed，agent 第一轮跑测试就会看到并顺手修掉，
     结构上就出不了 fix-1。两种写法的对比实测（go 1.25.7，A = R3 写法、B = R4 写法，各自跑 test/build/vet，再只修 `Add` 重跑）在
     `/opt/lathe-wt/evidence/f01/review-r4/vet-probe/output.txt`：A 修前修后 `go test` 都是 build failed；B 修前 `go test` 是
     `got -1, want 5` 的正常失败、修后 `ok`，`go build` 始终通过，`go vet` 修前修后都报 `calc.go:9:13: Copy passes lock by value`。
     **残余不确定性如实写明**：agent 仍可能主动跑 `go vet` 或读 `calc.go` 时顺手改掉 `Copy`（模型自由度，不再是结构性必然）。
     此时验证一次通过、没有 fix-1：脚本把这一事实写进 `summary.json`（`fixRound:false`），**不判通过**，换一个 issue 措辞重跑一次；
     两次都没有 fix-1 → 如实报告并标 draft，不伪造、不改断言；
   - **预置 mirror**：`git clone --mirror $E/fixture.git $E/ws/.mirrors/f01-e2e.git`，`git -C … remote set-url origin $E/fixture.git`，
     再 `config --unset-all remote.origin.mirror` —— `EnsureMirror` 看到 HEAD 存在就只 `fetch origin`（`worktree.go:89-121`），
     于是 `loadRepoConfig` 写死的 `git@github.com:` 克隆地址（`queue.go:686`）不会被用到。
3. 子 shell 里起 serve（`LATHE_*` 只在子 shell 里设）：`HOME=$E/home`、`LATHE_HTTP_ADDR=127.0.0.1:8401`、`LATHE_DB_NAME=lathe_f01`
   （其余 DB 变量同 `lathe_migrate`）、`LATHE_DATA_DIR=$E/data`、`LATHE_WORKSPACE_ROOT=$E/ws`、`LATHE_REAP_ENABLED=false`、
   `LATHE_ADMIN_PASSWORD=<随机>`、`LATHE_FIX_ATTEMPTS=1`、`LATHE_AGENT_TIMEOUT=15m`、
   `LATHE_GITHUB_TOKEN=stub-not-a-token`（`pipeline.go:436` 的 setup 要求能构造 GitHub 客户端；构造本身不发网络请求，
   这一轮停在 `awaiting_approval`，到不了 `CreatePR`）。记下 PID，结束时 `kill <PID>`（**不用 pkill -f**）。
4. API：登录 → `POST /api/repos {providerRepo:"f01/e2e"}` → `PUT /api/repos/{id}` `{gateMode:"manual", verifyTierOverride:"light"}`
   → `POST /api/issues` 建内置工单 → `POST /api/issues/{id}/start`，得到任务 `T`。
5. **插话一（实时投递，新会话）**：轮询 `GET /api/tasks/T` 到 `state=implementing`，且 `GET /api/tasks/T/events` 里 `implement` 阶段
   已有 ≥1 条事件（进程起来了）→ `POST /api/tasks/T/interject {"message":"请在 calc.go 文件顶部加一行注释 // f01-mark-$R1"}`。
   期望 `200 delivered` 且 `deliveryId` 非空；
6. **插话二（`--resume` 会话里实时投递）**：轮询到 `state=verifying` 且 `fix-1` 阶段已有事件 →
   `POST …/interject {"message":"修复时在 calc_test.go 顶部加一行注释 // f01-mark-$R2"}`。期望 `200 delivered`、`phase=fix-1`；
   若 fix-1 已出 result（`202 queued`），脚本记录并在第 8 步按结算结果断言，不判通过，按第 2 步规则重跑；
7. 轮询直到 `state` 不再变化（上限 20 分钟）。
8. 停 serve（按 PID）；导出证据：`e2e/summary.json`、`e2e/agent_events.ndjson`、`e2e/interjections.tsv`、`e2e/verifications.tsv`、
   `e2e/task.json`、`e2e/worktree.diff`（`git -C <worktree> diff main` —— 现场在 `$E/ws` 里，本地，不推）。

**停在 `awaiting_approval`，原因写明**：推分支/开 PR 需要真实 GitHub 远端与真凭据，本线禁止。`gate_mode=manual` 是产品已有路径
（`pipeline.go:328-338` → `gateBeforePush`），验证通过后、推送之前停下。它覆盖了本功能要证明的全部链路
（新会话 implement + 实时插话 → verify 红 → `--resume` 的 fix-1 + 实时插话 → verify 绿 → 落库）；`stagePushAndPR` 与插话无关。

```bash
T=$(jq -r .taskId /opt/lathe-wt/evidence/f01/e2e/summary.json)
q(){ psql "$LATHE_TEST_DSN" -Atc "$1"; }
q "SELECT state FROM tasks WHERE id=$T"                                                                  # awaiting_approval
q "SELECT phase, status, error IS NULL, delivery_id IS NOT NULL FROM task_interjections WHERE task_id=$T ORDER BY id"
#   implement|delivered|t|t
#   fix-1|delivered|t|t
q "SELECT count(*) FROM agent_events WHERE task_id=$T AND phase='fix-1'"                                # ≥1（真的跑了 --resume 修复轮）
q "SELECT count(*) FROM verifications WHERE task_id=$T AND status IN ('failed','error')"                 # ≥1（第一轮 go vet 的 copylocks 红）
q "SELECT status FROM verifications WHERE task_id=$T ORDER BY id DESC LIMIT 1"                           # passed（取值见 0001_init.up.sql:171-172）
grep -c "f01-mark-$R1" /opt/lathe-wt/evidence/f01/e2e/worktree.diff                                     # ≥1（插话一进了模型并被执行）
grep -c "f01-mark-$R2" /opt/lathe-wt/evidence/f01/e2e/worktree.diff                                     # ≥1（--resume 会话里的插话也进了模型）
# 事件流没被污染（R3 改，只断言 f01 要消除的两类，§2.2 第 9 条）：
#   ① 没有 command_lifecycle 退化出的 raw（rawEntry 的 payload.type 就是事件 type，digest.go:317）
q "SELECT count(*) FROM agent_events WHERE task_id=$T AND kind='raw' AND payload->>'type'='command_lifecycle'"   # 0
#   ② 没有 type=user 事件产出的 text 行。text 行不带来源 type（digestMessage 不写 payload），所以按「user 回显的确切正文」反查：
#      replay 回显的是**我们发出的帧原文**——首帧 prompt（implement 与 fix-1 都以 prompt.go 的固定开头「你在一个自动化编码流水线里」起头）
#      与两条插话（逐字等于 POST 的 message）。agent 自己的正文不会逐字等于它们，也不会以该开头起句。
q "SELECT count(*) FROM agent_events WHERE task_id=$T AND kind='text' AND (
     body LIKE '你在一个自动化编码流水线里%'
     OR body = '请在 calc.go 文件顶部加一行注释 // f01-mark-$R1'
     OR body = '修复时在 calc_test.go 顶部加一行注释 // f01-mark-$R2')"                                   # 0
#   ③ 对照（不是断言，记进 summary.json 的 systemRawRows 供评审看）：既有的 system/<非 init> raw 照常存在，f01 不过滤
q "SELECT payload->>'subtype', count(*) FROM agent_events WHERE task_id=$T AND kind='raw' AND payload->>'type'='system' GROUP BY 1"
#      例如 thinking_tokens|N —— 既有行为（digest.go:87-91），数量随模型而变，不设断言
# fix-1 的确是 --resume：会话 ID 未换
jq -r '.fixResumeSessionEqualsImpl' /opt/lathe-wt/evidence/f01/e2e/summary.json                          # true（fix-1 的 init 事件 session_id == implement 的）
q "SELECT count(*) FROM agent_events WHERE task_id=$T AND (body LIKE '%stub-not-a-%' OR payload::text LIKE '%stub-not-a-%')"  # 0
[ "$(sha256sum /opt/lathe/bin/lathe | cut -d' ' -f1)" = "$P0" ] && echo prod-untouched                    # prod-untouched
ss -ltn | grep -c ':8401 '                                                                               # 0（serve 已停）
```
`worktree.diff` 里的标记断言是最强的一档证据（强于回显）：它证明插话文本到达了模型并影响了产出；如果 agent 没照做（模型自由度），
脚本记 `markApplied:false`，这两行断言即为**失败**，AC-22 不算通过；PR 里如实贴出（同时附 `isReplay` 回显 = 「CLI 收到了」的证据），换措辞重跑至多一次，仍不过则标 draft 等人拍板。不得放宽断言。
纪律同 AC-14：要真 CLI 与真凭据；当天不可用就**不**以假 CLI 替代，PR 标 draft 等人拍板。

### 启动恢复

**AC-16（进程重启不留下死行）** — `TestStartupSweepInterjections`，B 类（store 新方法）
预置：两条 `delivering`；一条 `queued` 挂在 `pr_open` 任务上；一条 `queued` 挂在 `blocked_dep` 任务上（R3）；一条 `queued` 挂在 `implementing` 任务上；
一条 `delivered`。跑「`Reconcile` + 启动清扫」后断言：两条 `delivering` → `failed`（`error` 含「重启」）；
`pr_open` 与 `blocked_dep` 任务的 `queued` → `failed`；`implementing`（已被 `Reconcile` 转 `queued`）任务的 `queued` **仍是 `queued`**；
`delivered` 行逐字节未变。
搭配 A 类红线 `TestReconcileDoesNotTouchTerminalTasks`（**新写、改动前提交**：既有 `Reconcile` 只动
`triaging/implementing/verifying`，`queue.go:296-299`，改动前后都绿；仓里今天没有同名测试，R1 写「既有」是笔误），
连同既有 `queue_review_test.go:35-42` 的崩溃恢复用例，
以及装配顺序测试（`cmd/lathe/assembly_test.go:140-155` 的顺序表里加上清扫，须在 `q.Reconcile` 之后、worker 之前）。

---

## 6. 风险与代价

| 风险 | 影响 | 缓解 |
|---|---|---|
| 注入被「吸收」而非「新轮」——模型可能忽略 | 人以为说了，模型没照做 | 文案「已送达给 agent 进程」而不是「已采纳」；不做「已生效」的假承诺 |
| result 前一刻写入的帧触发第二个 turn | 多一轮 token；`Result` 取值变化 | §3.2 第 5 条定义取值；AC-4 用两 result 的真实 fixture 守；f03 约定写进双方 PR |
| CLI 对未关 stdin 的行为随版本变化 | 进程不退 → 看门狗误杀 | 不再依赖：首条 result 即关（§3.2 第 3 条）；AC-5d、AC-14 第 3 条守 |
| `--replay-user-messages` 引入新事件形状 | 事件流被污染、人话被记成 agent 正文 | 过滤（§3.2 第 4 条）；AC-4 用真实 fixture 守 |
| 无会话时发的话最终没进模型 | 人以为「排上队就会生效」 | 202 文案写明「下一次 agent 轮次时投递，若没有下一轮会标为未进入模型」；结算给人读原因；界面显示 |
| 任务在 `Execute` 之外离开可运行状态（取消/级联阻塞） | `queued` 行无人结算、界面长期「已记录待投递」 | 读时结算 T2 + 5 分钟周期清扫 T3（§3.4），AC-19 R3 行守 |
| 读接口里写库（T2） | `GET` 有副作用 | 只写由 state 推导的确定结果、幂等、仅限本任务 `queued` 行；无 `queued` 行时是部分索引空扫 |
| 与 f04 的模板冲突 | 合并时手工解决 | 只动 `TaskDetail.vue` 2 行；冲突点与规则见 §3.5 |
| 与 f06（多 CLI）的接口耦合 | 并行开发撞车；`supervise` 抽取后 touch 位置/kill 前关闭丢失 → 看门狗误判或 Wait 卡住 | §3.2 第 6 条 S1–S5 + rebase 规则；AC-4b、AC-5d 在 `Driver.Run` 层守，两种合入顺序都必须绿 |
| 驱动改动对生产全量生效（含 `--resume`） | 所有实现/修复轮换了启动方式 | P10 + AC-14 resume 段 + AC-22 端到端；缺一保持 draft（§3.6） |
| 插话专用 Bearer 形态与全局形态表不一致 | 失败记忆里的 Bearer 仍不脱敏 | 既有缺口，不在本 PR 扩大或修复；PR 描述里建议独立 PR 提升为全局形态 |
| 跨节点不支持 | 多节点下插话退化为 `queued` → 结算 | 文档与文案写明；P3 时按注记演进 |
| 插话被滥用来「催」 | 限流（默认 5 条/10 秒） | 429 |

**代价总结**：驱动层多了一个「运行中句柄」与 stdin 生命周期（约 250 行新代码 + 测试），管线改 `runAgent` 一处
+ `Execute` 一个 defer，数据层一张表（含 rev 游标），API 两个路由、一个接口字段，UI 一个新组件 + `TaskDetail.vue` 2 行。
**不引入任何依赖**。

**最需要评审盯的三点**：
1. §3.3「`verifying` 也可插话」—— 需求原文只说了 implementing，这是我做的范围判断（理由见 §3.3）。
2. §3.2 第 3 条「首条 result 即关 stdin」意味着：result 之后的话**进不了这一轮**，只能等下一轮或被判 failed。
   这是为了不让一句迟到的话凭空多开一个 turn（多烧钱、改变 `Result`）而做的取舍。
3. AC-14、AC-22 依赖机器上有可用凭据；没有的话 PR 只能标 draft。
4. （R2）驱动改动对生产**全量**生效、无开关（§3.6）—— 所有 claude 实现/修复轮（含 `--resume`）都换成流式输入启动。
   证据链：P10（裸 CLI `--resume`+流式+注入）→ AC-14 第三段（Lathe 驱动）→ AC-22（:8401 端到端，含 `--resume` 的 fix-1）。
5. （R2）§3.2 第 6 条对 f06 `supervise()` 的两处签名要求（S1 parse 收 `Touch`、S2 `BeforeKill`）—— 需 f06 线确认。

---

## 7. 文档改动（同一个 PR 里交）

1. **新建目录 `docs/features/` 并新增 `docs/features/interject.md`**（`dev` 上该目录今天不存在；其它功能线也可能各自新建
   同一目录下的不同文件，git 合并不冲突）—— 本 PRD 的定稿版（结论先行，含 §2.2 的实测表、§3.2 的 stdin 生命周期、
   §3.7 的演进注记）。
2. **`README.md` 变量表** —— 新增三行：`LATHE_INTERJECT_MAX_CHARS`、`LATHE_INTERJECT_RATE_LIMIT`、
   `LATHE_INTERJECT_WINDOW`（形状照 `README.md:124-156` 的表）。
3. **`CHANGELOG.md` 的 `[Unreleased] / Added`** —— 「运行中插话：属主可在实现阶段（及验证期的修复轮）给正在跑的 agent
   追加指示；消息落库可查，没进入模型的会标明原因；不改变验证判定。」
4. **`docs/04-agent-visibility.md`** —— 补一节：插话数据来源（`task_interjections` 而非 `agent_events`）；
   流式输入下 `user(isReplay)` 与 `command_lifecycle` 被过滤的原因（前者会被 `Digest` 记成 agent 的 `text`）；
   一次运行多条 result 时 `Result` 的取值规则。
5. **`docs/02-design.md`** —— §6「调度与多节点」补一句「跨节点插话不支持，见 docs/features/interject.md」。
6. **PR 描述**（不是仓库文档，但必须有）：与 f06 的 S1–S5 与互相 rebase 规则（§3.2 第 6 条）、与 f03 的多 result 约定（`Usage`/`CostKnown` 随末条覆盖、回落也只取末条、不跨 result 相加，贴 §3.2 第 5 条取值表原文，与 f03 §3.1 逐字相同；写明本线是先合入还是后合入；f03 先合入时贴 rebase 后 `TestResultEventUsageTakesLastResult` 的输出，§3.2 第 5 条）、resume 降级只看 `err` 的既有行为观察（§2.2 第 10 条，建议单独立项）、与 f04 的分歧与「以本 PRD 为准」（§4 f04 行）、AC-14 与 AC-22 的完整输出。

---

## 8. 设计约束自检（对 AGENTS.md §4 与任务书）

| 事项 | 是否触碰 | 说明 |
|---|---|---|
| 删东西 | 否 | 只有新增 |
| 改状态机语义 | 否 | `internal/task/state.go`、`machine.go` 一行不改；结算 T1–T4 只**读** `tasks.state`，不做转移、不挂在 `Transition`/`cancelTask`/`CancelForIssue` 上（AC-19 R3 机检行） |
| 削弱验证 | 否 | AC-9 专门守；插话包裹语禁止据此跳过验收 |
| 删 `skills/` 版本目录 / 改 skills | 否 | 不碰 |
| 改历史迁移 | 否 | 只新增 0032（号段 0032–0034 内） |
| 新依赖 | 否 | `go.mod`/`web/package.json` 不变 |
| force-push / 合并 / 删分支 / 推 dev·main | 否 | 不做 |
| 打 tag / Release | 否 | 不做 |
| 改「非属主 404」 | 否 | 沿既有写法 |
| 改全局脱敏 `redact.go` | 否 | 插话专用形态放 `interject.go`（§3.7） |
| 改 f04 的地盘 | 最小 | `TaskDetail.vue` +2 行，`AgentEventItem.vue` 与 `GET /events` 不动（§3.5） |
| 改 f06 的地盘 | 最小 | f06 先合入时只改 `supervise` 签名两处（S1/S2，§3.2 第 6 条）；f01 先合入时不碰 f06 任何文件 |
| 改别人的 PRD | 否 | f04/f06 PRD 与本 PRD 的分歧在本 PRD 写明以谁为准，由对方线同步（§4、§3.2 第 6 条） |
| 每个配置项有消费方 | 是 | §3.6 |

**status: ok** —— 所有需求要点都能在不触碰上述边界的前提下实现。

---

---

## 10. 实现与验收结果（2026-11-11，分支 `feat/interject`）

> 原始输出都在功能线证据目录 `/opt/lathe-wt/evidence/f01/`（`dev/`：先红与门禁；`real-cli/`：AC-14；`e2e/`：AC-22 脚本与尝试记录）。

### 10.1 落点（与 §3 的对应）

| 层 | 文件 | 说明 |
|---|---|---|
| 数据 | `migrations/0032_interjections.{up,down}.sql`、`internal/store/interjections.go` | 迁移原样按 §3.1；所有写都在 `pg_advisory_xact_lock(task_id)` 下推进 `rev`，读 `tasks.state` 一律 `FOR SHARE`。结算 `SettleQueuedInterjections` 只读状态 |
| 驱动 | `internal/integration/agent/agent.go`、`session.go` | `RunParams.StreamInput/OnReady/OnDelivery`；stdin 用 `os.Pipe`，首条 result / ctx 结束或看门狗（kill 之前）/ Wait 返回三处关闭；`parseStreamWith` 的过滤在 `Touch` 之后；多 result 时 `NumTurns/DurationMS` 求和、其余取末条 |
| 管线 | `internal/runner/interject.go`、`pipeline.go` | `Interjections`（注册表 + `Submit` + 开场补投 + 结算）；`runAgent` 补投在 `injectFailureMemory` 之后；`Execute` 第一行 `defer` 结算（T1）。`streamInputSupported` 恒真（f06 合入后改为 `CapabilitiesOf`） |
| API | `internal/httpapi/interject.go`、`api.go`（`Routes` +2 行、`API` +3 字段） | 非属主 404；`cancelTask` 函数体未动 |
| 配置 | `internal/config/config.go` | 三个 `LATHE_INTERJECT_*`，非法值 `Load()` 报错并点名变量 |
| 装配 | `cmd/lathe/main.go` | 同一个 `Interjections` 挂在 Pipeline 与 API；启动清扫（T4）在 `q.Reconcile` 之后、`startWorkers` 之前（`assembly_test.go` 机检）；周期结算（T3）每 5 分钟 |
| UI | `web/src/components/InterjectPanel.vue`（新）、`TaskDetail.vue`（+2 行）、`api.js`（末尾 +2 方法） | 按 `rev` 增量轮询，按 `id` upsert，纯文本插值 |

### 10.2 AC 逐条

| AC | 结果 | 证据 |
|---|---|---|
| AC-1 迁移可逆 | 通过：`0 → 12 → 0/0 → 12`，12 列名与 §3.1 一致 | `dev/03-red-ac1.txt`（改动前 0）、`dev/07-green-ac1.txt` |
| AC-2 CHECK | 通过（先红：列数 0） | `dev/06-red-ac2.txt`；`TestInterjectStatusCheck` |
| AC-3 编号与成对 | 通过：`migrations/` 只新增 0032 两个文件 | `dev/22-…machine-checks.txt` |
| AC-4 / AC-4c 事件流不被污染 | 通过（A 类真红：`onEvent=13`、`user->text:2`、`command_lifecycle->raw:5`、`NumTurns=1`、`DurationMS=2499`；AC-4c 前两行红、后三行改动前后都绿） | `dev/01-red-driver-a.txt`；`TestParseStreamFiltersReplayAndLifecycle`、`TestParseStreamKeepsExistingSystemRaw`，fixture 已拷入 `internal/integration/agent/testdata/` |
| AC-4b 被过滤事件仍喂看门狗 | 通过（A 类红线改动前已提交并绿；流式子用例新增） | `TestFilteredEventsStillTouchIdle` |
| AC-5 / AC-5d / AC-6 | 通过（`-race`） | `TestSessionDeliverMidRun`、`TestStreamInputStdinLifecycle`（三条各 < 3s / < 2s / < 3s）、`TestDeliverBackpressureDoesNotBlock`；红线 `TestBuildArgsDefaultModeUnchanged`、`TestRunDoesNotWaitForSink` |
| AC-6b 慢 DB 不拖慢读协程 | 通过：`TR−T3 < 1s`，3 行 delivered、error 为空，读协程标记未命中 | `TestSlowStoreDoesNotSlowReadGoroutine` |
| AC-7 / AC-21（runner 侧） | 通过 | `TestRunAgentStreamInputOnlyWithRegistry`、`TestStreamInputSupportedDefaultsTrue`；红线 `TestTriageRunParamsCarryPromptAndNoStream` |
| AC-8 / AC-8b 开场补投 | 通过（6 个子用例，含 `num_turns=0` 的真实 resume 失败形状） | `TestRunAgentPrefillsPendingInterjections`、`TestPrefillAfterFailureMemory`；红线 `TestRunAgentPromptUnchangedWithoutInterjections`（改动前提交，装配后仍绿）与既有失败记忆三条 |
| AC-9 验证不受影响 | 通过：无插话 / 实现期间插 3 条，`verifications` 序列逐项相等；`make test-race` 绿 | `TestStageVerifyUnchangedByInterjections`；`dev/21-race.txt` |
| AC-10 属主 404 | 通过（先红：`Content-Type got=text/plain`） | `dev/04-red-ac10.txt` |
| AC-11 / AC-12 状态码、限流、配置 | 通过（配置校验 A 类先红：10 个非法值改动前 `Load()` 不报错） | `dev/02-red-a-class-other.txt`；`TestInterjectStatusCodes`、`TestInterjectRateLimitAndConfig`、`TestLoadRejectsInvalidInterjectConfig` |
| AC-13 单一脱敏点 | 通过：库 / GET 响应 / stdin 帧 / 补投 prompt / slog 均无明文；全局 `redactText` 对 Bearer 仍不替换（红线） | `TestInterjectRedactsAtSingleSubmitPoint`、`TestInterjectRedactsInListResponse`、`TestRedactTextShapesBaseline` |
| AC-16 启动清扫 | 通过 | `TestStartupSweepInterjections`（cmd/lathe）、store 同名用例；红线 `TestReconcileDoesNotTouchTerminalTasks`（改动前提交） |
| AC-17 rev 游标 | 通过（含 205 条分页与 2×50 并发轮询不漏） | `TestInterjectionsRevCursor`、`TestInterjectionsRevCursorConcurrent`；`/events` 顶层键红线 |
| AC-18 取消后不接受 | 通过 | `TestInterjectRejectedAfterCancelEvenWithLiveSession` |
| AC-19 queued 一定有结局 | 通过（管线 6 场景、两组竞态各 100 次、cancelTask / CancelForIssue / blocked_dep 级联 / 周期清扫 / 不误判）；机检：`internal/task/`、`queue.go` 无改动，`api.go` 三个 hunk 都不在 `cancelTask`（610–634）内 | `TestQueuedInterjectionsSettled*`、`TestInterjectRace*`、`TestInterjectSettledOnReadAfterCancel*`；`dev/22-…` |
| AC-20 客户端断开 | 通过（三个子用例，handler ≤ 3s） | `TestSubmitSurvivesClientDisconnect` |
| AC-14 驱动级真实证据 | **未取证**（真凭据不可用，见 10.3）。驱动机制项在真实 claude 二进制上全部成立（stub provider 补充证据 12/12；真 provider 那次 8/12，失败的 4 项都是「未登录」导致模型没跑） | `run_real_cli.sh`、`real-cli/real-20261111-172800/`、`real-cli/`（stub） |
| AC-15 UI | 机检通过（`TaskDetail.vue` numstat `2 0`、`AgentEventItem.vue` 无 diff、`v-html` 0、`InterjectPanel` 2 次）；`make ui && make build` 成功。**截图未做**：手测按 PRD 定义在 AC-22 那次 :8401 运行上做，见 10.4 | `dev/09-ui-build.txt` |
| AC-22 :8401 端到端 | **未执行**，见 10.4（两个前置都不满足：库里残留、真凭据不可用） | `e2e/run.sh`、`e2e/run-attempt-1.txt` |

### 10.3 AC-14：真实 claude 2.1.322（真 provider 未取证；stub provider 补充证据）

**真 provider：未取证。** 包装脚本 `/opt/lathe-wt/evidence/f01/run_real_cli.sh` 按 PRD 形态建隔离 `HOME`、复制
`~/.claude/.credentials.json`、建临时仓库、`uuidgen` 生成 `INTERJECT-MARK-…`，再以 `-real-home/-real-dir/-real-mark` 跑
`TestRealCLIInterject`（给了 `-real-home` 就不注入 stub 的 `ANTHROPIC_BASE_URL`/假 token）。2026-11-11 17:28 那次两段的 result 都是
`Not logged in · Please run /login`：本机凭据文件里只有 `mcpOAuth.*`（只看了键名），**没有 `claudeAiOauth`**，P7「复制即登录」的
前提今天不成立。按 PRD 纪律，不拿 stub 结果顶替，也没有挪用当前 agent 会话环境里的 `ANTHROPIC_*`（那不是 PRD 规定的凭据来源）。
那次仍然成立的机制项：首条 result 后 0.536s / 0.257s 退出、非 `ErrIdle`；result 后 `Deliver` → `ErrSessionClosed`；单条 result；
`--resume` 参数形状；`session_id` 不变；仓库无改动（`real-cli/real-20261111-172800/`，含 `ATTEMPT.md`、完整 go test 输出、secret-scan 0 命中）。
**需要人**：提供可复制的 Claude 登录凭据（或指定凭据来源）后重跑脚本；补齐前 PR 保持 draft。

**补充证据（stub provider，可离线复现，不算 AC-14 通过）：**
`TestRealCLIInterject`（默认跳过）用 Lathe 自己的 `Driver` 起真实 `claude` 二进制：显式 `Dir`（`t.TempDir()` 里的 git 仓库）、`sanitizedEnv` 白名单 + `ExtraEnv`
（隔离 `HOME`/`CLAUDE_CONFIG_DIR`、`ANTHROPIC_BASE_URL` 指向测试进程内的 httptest stub、假 token），不用真凭据、不连外网。
stub 对「F01-SLEEP」回一个 `Bash sleep 8` 的 tool_use，之后的请求里看到什么标记就回什么 —— 所以「result 含 MARK」证明的是**注入帧被
真实 CLI 并进了同一个 turn 的下一次模型请求**，不是模型的理解能力（与 PRD 写的真凭据版本相比，这一档证据弱在「模型照做」那一层，
强在完全可复现）。12 项自检全部通过（`real-cli/real-cli-20261111-165750-*`）：

- a 段（新会话）：注入帧 `isReplay` 回显 uuid 一致；末条 result `DONE ZEBRA-… MARK1-…`；**首条 result 后 0.228s 退出**、非 `ErrIdle`；
  result 之后 `Deliver` → `ErrSessionClosed`，stdout 只有 1 条 result。
- c 段（`--resume <sid> --input-format stream-json --replay-user-messages`，与 fix-N 生产参数同形）：回显一致；只有 1 条 result；
  result 同时含第一段暗号与 `MARK2`；`session_id` = sid；result 后 0.225s 退出。
- 临时仓库 `git status --porcelain` 为空；交给 `OnEvent` 的事件里没有 `command_lifecycle` / replay。
- 证据已扫：无 token 形态串，`cwd` 只有 `/tmp/TestRealCLIInterject…` 临时仓库。

### 10.4 未完成项（如实）

1. **AC-22（:8401 端到端）未执行**。PRD 第 0 步要求 `lathe_f01` 里非终态任务数为 0，实测为 6（脚本运行时 10）：
   全部是既有测试 `TestPipelineInternalIssueHappyPath` / `TestPipelineInternalBlockedSpecRoundTrip` 的残留（每跑一次 `make test` 多 2 行，
   `queued` 的 CR-777 与 `pr_open` 的 LT-xx）。serve 会领 queued 行、merge poller 会拿 stub token 轮询 pr_open 行，不能带着残留起实例；
   按 AGENTS.md §4「真删任何东西必须先问人」停下。另：`make clean-test-db` 在宿主无 psql 时走 docker 分支并写死 `-d lathe`，
   **清不到 `lathe_f01`**（只读查询，未改任何东西），清理方式需人决定。脚本 `e2e/run.sh` 已就绪，前置满足后可直接跑。
   第 2 轮复查（17:30）残留仍在（10 行，id 197–1324），仍未获清理授权。此外 `run.sh` 用的也是复制 `~/.claude/.credentials.json`
   的隔离 HOME，而本机该文件里没有 Claude 登录凭据（见 10.3），即使清掉残留，实例里的 claude 也会 `Not logged in`。
2. **AC-15 截图**依赖 AC-22 的那次运行，同上未做。
3. **AC-14 真 provider 未取证**（见 10.3）。
4. 因此按 §3.6 的约定（P10 + AC-14 + AC-22 缺一即 draft），PR 保持 draft，等人决定：① `lathe_f01` 残留怎么清；② 用哪份凭据。

### 10.5 与并行线的接缝（本线先合入的情形）

- **f03**：dev 上没有 `Usage/CostKnown`；本线只交付「`NumTurns/DurationMS` 累加、其余覆盖」与 fixture
  `internal/integration/agent/testdata/replay-user.ndjson`。f03 rebase 时把两字段加进覆盖组并写 `TestResultEventUsageTakesLastResult`。
- **f06**：`Driver.Run` 内联实现 S1–S3（`parseStreamWith` 的 `Touch` 在过滤之前、`OnResult` 关 stdin、ctx/看门狗在 kill 之前关 stdin）。
  f06 抽 `supervise` 时按 §3.2 第 6 条改两处签名，AC-4b/AC-5d 是 `Driver.Run` 层测试、抽取后应一行不改仍绿。
- **f04**：本线只在 `TaskDetail.vue` 加 2 行，不碰 `/events`、`AgentEventItem.vue` 与时间线分组。

### 10.6 实现中的取舍（PRD 未写死、这里补记）

- `Session.Deliver` 在写协程被大首帧占住时也遵守 2s 上限：排队超时的帧以 CAS 标记「调用方已放弃」，写协程跳过它，不会出现
  「已报背压、事后又被写进去」；半帧写超时则直接收尾会话（宁可走「下一轮补投 / 结算」，也不发出粘连的坏帧）。
- `StreamInput=true` 且 `Prompt` 为空时 `Run` 直接报错：流式输入下 prompt 是 stdin 首帧，没有首帧 CLI 只会干等到看门狗。
- 观察到的既有问题（不在本线范围，未修）：AC-22 第 0 步撞上的测试残留（见 10.4）；`make clean-test-db` 的 docker 分支忽略
  `LATHE_TEST_DSN` 的库名；resume 降级只看 `err` 的既有行为（§2.2 第 10 条）。
