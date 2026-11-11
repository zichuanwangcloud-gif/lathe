# 多 CLI 驱动（接入 Codex）

> f06 · 分支 `feat/multi-cli` · 迁移 0047、0048 · 测过的版本：codex-cli 0.146.1、claude（本机版本）
>
> 本文是 PRD（`/opt/lathe-wt/prd/f06-multi-cli.md`，r6 定稿）的落地版：结论先行、写明实现与验收结果。
> PRD 阶段的取证脚本与原始日志在 `/opt/lathe-wt/evidence/f06/probes/`，开发阶段的验收证据在
> `/opt/lathe-wt/evidence/f06/`（见文末 §8）。

## 1. 结论

- **写代码的 agent 是可替换的劳动力，验证体系不变。** 仓库可以选 `claude`（默认）或 `codex` 作为执行
  agent。heavy/light、红-绿复现证明、修复回路、闸门、`verifications` 落库对两者一行不差
  （`verify.go / heavy.go / tier.go / gate.go / verifylog.go / repro_manifest.go / stack.go / retry.go /
  stage.go` 与 `internal/task/state.go` 均未改动）。
- **驱动按任务钉死。** 建任务那一刻把 `repos.agent_driver` 复制进 `tasks.agent_driver`，之后分诊、实现、
  `implement-reentry`、`fix-N`、review 二轮、rebase 跟进重验都用这一个驱动——会话是哪个 CLI 建的，就必须由
  哪个 CLI 续。想对某个 issue 换驱动，取消后让它新建任务。
- **不配置的部署行为不变。** `repos.agent_driver` 默认 `claude`；老任务（NULL）读成 `claude`；
  `LATHE_CODEX_HOME` 不配 codex 就不启用。
- **review 意见续跑持有原会话。** codex 用 `codex exec resume <thread_id>`（实测保持上下文，P4/P5）。
- **隔离与 claude 同级。** 同一个环境变量白名单 `sanitizedEnv()`、显式 `cmd.Dir`、同一套进程监管；
  codex 子进程额外强制 `CODEX_HOME=<LATHE_CODEX_HOME>`、`HOME=<LATHE_CODEX_HOME>/home`，个人 `~/.codex` 与
  `~/.agents/skills` 都进不来（真实二进制机检，§8 AC-22）。

## 2. 统一事件模型

```
runner / prd / preview ──▶ AgentDriver.Run(ctx, RunParams)            （接口不变）
                           └─ agent.Router：按 RunParams.Driver 分派（空串 = claude）
                               ├─ ClaudeDriver（原 Driver，保留别名，行为不变）
                               └─ CodexDriver（新增）
                           supervise()：进程组 / 看门狗 / 管道宽限 / 错误分类（两者共用一份）
                           Event{Driver,Type,Subtype,SessionID,Raw} → Digest（按 Driver 分派）→ Entry
```

- `Event` 多一个 `Driver` 字段（零值 = claude），`EventType` 多一个 `other`。`Digest` 先按 `Driver` 分派：
  claude 分支一行不改，codex 走 `digestCodex`。**产出的 `Entry.Kind` 只有既有那几种**，`agent_events`、
  `prd_events` 的 CHECK 约束一个字没改。
- codex 的每条 Entry payload 带 `driver:"codex"`；claude 的不加。

| codex 行（`exec --json`） | Event.Type | Entry |
|---|---|---|
| `thread.started` | system / init，SessionID = thread_id | `init`「会话初始化 · codex · thread <id>」，payload `{driver, threadId}` |
| `turn.started` | other | 不产出 |
| `item.completed` · `agent_message` | assistant | `text` |
| `item.completed` · `reasoning` | assistant | `thinking` |
| `item.started` · `command_execution` | assistant | `tool_use`，Tool=`Bash`，body 去掉 `/bin/bash -lc '…'` 外壳，payload `{toolUseId, driverTool:"command_execution"}` |
| `item.completed` · `command_execution` | user | `tool_result`，body=`aggregated_output`，payload `{toolUseId, exitCode, isError}` |
| `item.completed` · `file_change` | user | `tool_use`，Tool=`Edit`，body 逐行「path (kind)」 |
| `item.*` · `mcp_tool_call` | assistant / user | `tool_use` / `tool_result`，Tool=`mcp:<server>/<tool>` |
| `item.completed` · `web_search` | assistant | `tool_use`，Tool=`WebSearch` |
| `item.*` · `todo_list` | assistant | `text`「计划：…」 |
| `item.completed` · `error`、顶层 `error`、不认识的 type、坏 JSON | other | `raw`（绝不打挂） |
| `turn.completed` | result / success | `result`「执行结束 · 成功 · codex · N 轮 · Xs · 费用未知」，payload `{driver, subtype, isError:false, numTurns, durationMs, usage}` |
| `turn.failed` | result / error_turn_failed | `result`「执行结束 · 失败 · …」+ error.message，`isError:true` |

工具名用 `Bash` / `Edit` 这类 claude 用户熟悉的名字、`toolUseId` 配对、结果带 `exitCode` / `isError`——
界面（含 f04 回放）不用为 codex 写分支。所有 body 按 rune 截断（54075f4 的教训）。

单行 JSON 里没有「到此为止几轮、耗时多少」，而 `Digest` 是逐行无状态的：codex 驱动在 `turn.completed` /
`turn.failed` 上通过 `Event` 的一个未导出字段把这两个值带给 `digestCodex`，其它事件不带。

**Result 映射（codex）**：`Success` = 有 `turn.completed` 且没有 `turn.failed`；`Subtype` = `success` /
`error_turn_failed`；`Text` = 最后一条 `agent_message`（失败时 = error.message）；`NumTurns` = 两种终局事件条数；
`DurationMS` = 驱动量的墙钟；`CostUSD = 0`（codex 不报费用，界面显示「费用未知」，不是 $0）；`SessionID` =
thread_id；两种终局事件都没有 → `ErrNoResultEvent`；退出码规则与 claude 相同（有终局以终局为准）。

**与 f03（用量统计）的接缝**：f06 先合入时，codex 解析器已按 turn 收集 `turn.completed.usage` 原文（未导出，
不进 `Result`），f03 rebase 时在构造 `Result` 处补一行 `Usage = agent.UsageFromCodex(...)` 与
`TestCodexResultUsage` 即可，不用改解析器；`CostKnown` 不需要 codex 驱动置位。总超时错误满足
`errors.Is(err, context.DeadlineExceeded)` 且文案仍是「执行超时（上限 …），进程树已回收」（用一个
`Unwrap` 到 `DeadlineExceeded` 的错误类型实现，文案逐字不变）。

## 3. 驱动契约（RunParams 在两个驱动下的语义）

`RunParams` 新增 `Driver string`（空串 = claude）与 `OnSession func(id string)`（驱动一拿到真实会话 ID
就回调；claude 不回调，codex 在 `thread.started` 时回调 thread_id，同一 ID 只回调一次）。

| 字段 | claude（不变） | codex |
|---|---|---|
| `Prompt` | argv 最后一个参数 | **stdin**，argv 写 `-`（避开单参数上限） |
| `Dir` | `cmd.Dir` | `cmd.Dir`，新会话再传 `-C <Dir>`；**空 Dir 直接报错**（B2-3） |
| `SessionID`（新会话） | `--session-id` | 忽略（codex 不接受预分配 ID）；真实 ID 走 `OnSession` 与 `Result.SessionID` |
| `Resume` | `--resume <id>` | `exec resume --json --skip-git-repo-check … <id> -` |
| `FromPR` | `--from-pr` | 按能力表拒绝：`ErrUnsupportedParam`，不起进程 |
| `PermissionMode` | `--permission-mode` | `plan` → `sandbox_mode="read-only"`；`acceptEdits`/`bypassPermissions`/空 → `workspace-write` + `network_access=true`；其它值报错。始终 `approval_policy="never"` |
| `SettingSources` | `--setting-sources` | 有意忽略（隔离靠专用 `CODEX_HOME`）；**永不加 `--ignore-user-config`** |
| 固定参数 | `--print --output-format stream-json --verbose` | `exec --json --skip-git-repo-check … -c project_doc_fallback_filenames=["CLAUDE.md"]` |
| `ExtraEnv` | 白名单之后追加 | 同左；`CODEX_HOME`、`HOME` 最后追加且各只出现一次，调用方覆盖不了 |
| `ExtraArgs` | 原样追加 | 原样追加（在 `-` / 会话 ID 之前） |
| `OnEvent` | 每行回调 | 每行回调（`Event.Driver = "codex"`） |

新会话 + `acceptEdits` 的完整 argv：
`exec --json --skip-git-repo-check -C <dir> -c approval_policy="never" -c sandbox_mode="workspace-write" -c sandbox_workspace_write.network_access=true -c project_doc_fallback_filenames=["CLAUDE.md"] -`

**能力表，缺省拒绝**：`Capabilities{FromPR, PreassignedSession}`；`Router.Run` 先比对 `p.requires()` 与目标
驱动的能力，缺哪项报哪项。`TestCodexHandlesEveryRunParamsField` 用 reflect 遍历 `RunParams` 全部字段，每个
都必须出现在 `codexConsumes` 或 `codexRejects` 里——任何一方（如 f01 的插话字段）新增字段而没表态，测试当场红。

**supervise 的签名**（与 f01 §3.2 第 6 条 S1–S5 一致）：`launch{Bin, Args, Dir, Env, Stdin, Started,
BeforeKill, Exited}` + `parse func(r io.Reader, h parseHooks{Touch, Emit})`。stdin 归驱动所有（claude 传
nil，逐字节同改动前；codex 传 `strings.Reader`）；`*os.File` 作 stdin 时 supervise 在 Start 后关父进程读端；
`BeforeKill` 在超时/取消/静默判死杀进程组前同步调用恰好一次，正常退出不调；看门狗只认 parse 调的 `Touch`，
过滤发生在 parse 里也不会误判死。两个生产 parse 都对每条可解析 JSON 行先 Touch 再 Emit、不过滤。

## 4. runner 的改动（最小面）

1. **驱动从任务行取**：`task.Task.AgentDriver`（`taskColumns` 里 `COALESCE(agent_driver,'claude')`，一处），
   runner 唯一取值点 `rc.driver()`。`RepoConfig`、`loadRepoConfig`、`NewRepoLookup`、`ExecuteParams` **一个字都没改**
   ——没有地方可填，也就不存在「某个调用点忘了填」。与 `gate_mode` 同一做法。`Execute` 的两个调用点（派发、
   `MergePoller.rebaseFollowup` 的 EntryVerify 重验）因此自动拿到同一个驱动（AC-10、AC-25）。
2. **驱动上报的真实会话 ID 永远优先**：`runAgent` 给 `OnSession` 传闭包，闭包只做 `SetSessionID`（字段记账，
   不是状态转移）+ 原子写；`runAgent` 多返回 `reported`，三个调用点在 runner 自己的协程里收尾：
   `implement` / `implement-reentry` 采纳上报值；`fix-N` 在 `!fixResume` 时用 `reported`（非空）替换 Lathe
   预分配的 UUID 再 `SetSessionID`——否则下一轮 resume 撞 `no rollout found`。claude 下 `reported` 恒为空，行为不变。
3. **本节点没有这个驱动：直接失败，绝不碰会话**。`Router` 对未注册驱动返回 `ErrDriverUnavailable`、能力不符返回
   `ErrUnsupportedParam`；`stageImplement` 的 resume 降级条件排除这两类错误，落到已有的 `p.fail(StageImplementRun)`，
   `agent_session_id` 原样保留，之后在有 codex 的节点上重试仍能 resume。不新增 Stage。
4. **subagent watcher 只对 claude 开**：`newEventSink` 多一个 `driver` 参数，目录取 `agent.SubagentDirFor(driver,…)`
   （codex 返回空串）。runner 不再直接调用 `agent.ProjectsRoot()` / `agent.SubagentDir()`。
5. **技能物化目录按驱动选**：`agent.SkillsDir(driver)`（claude `.claude/skills`，codex `.codex/skills`），对应那一行
   写进 `info/exclude`。`skills/` 包一个字没改（`go-testing@1.0.0` 的 frontmatter 对 codex 兼容，P9）。
6. **review_feedback**：`retry.go`、`implementPrompt` 不改；resume 走任务行上的会话 + 任务钉死的驱动。

## 5. 数据模型

- **0047_repo_agent_driver**：`repos.agent_driver text NOT NULL DEFAULT 'claude'` + `CHECK IN ('claude','codex')`。
  小表，留在事务里。
- **0048_task_agent_driver**（非事务）：`tasks.agent_driver text`（可空，无默认值，只改元数据）+
  `CHECK … NOT VALID` 再 `VALIDATE`（扫表时只拿 SHARE UPDATE EXCLUSIVE，不挡派发与转移）。每一步可重跑
  （`ADD COLUMN IF NOT EXISTS`、先 `DROP CONSTRAINT IF EXISTS`）。NULL = 本功能上线前的任务 = claude，不回填。
- 钉死时机：唯一的 INSERT 点（`task.Machine.Create`）写 `agent_driver = (SELECT agent_driver FROM repos WHERE id = <repo_id>)`。
- 这是任务行上的一列，不是状态、不是转移：`tasks_state_check` 与转移表都没动。

## 6. API / UI / 配置

| 端点 | 改动 |
|---|---|
| `GET /api/repos`、`PUT /api/repos/{id}` 响应 | `RepoRow.agentDriver` |
| `PUT /api/repos/{id}` | body `agentDriver`：不传 = 不改；不在 `claude/codex` → 400；`codex` 但本实例未启用 → 400「本实例未启用 codex 驱动（运维未配置 LATHE_CODEX_HOME）」；非属主照旧 **404** |
| `GET /api/agent-drivers`（新增，需登录） | `{"drivers":[{"name":"claude","enabled":true},{"name":"codex","enabled":<bool>}]}`，不含路径或凭据 |
| `GET /api/tasks/{id}` | `task.agentDriver`（NULL 读成 claude） |
| 管理员运行时面板 | `runtime.codexBin`、`runtime.codexEnabled`；不报 `LATHE_CODEX_HOME` 的路径与内容 |

- 仓库页：「验证档位」后面加「执行 agent」下拉；codex 未启用时选项置灰并说明原因；文案写明「只影响之后新建的任务」。
- 任务详情：标题旁的驱动徽标（只在不是 claude 时显示，默认部署界面不变）；codex 的费用徽标显示「费用未知」。
- 系统设置：运行时面板加「codex 可执行文件」「codex 驱动：已启用 / 未启用」两行。

| 变量 | 默认 | 消费方 | 启动校验（失败直接退出） |
|---|---|---|---|
| `LATHE_CODEX_HOME` | 空 = 不启用 | CodexDriver（子进程 `CODEX_HOME` 与 `HOME=<它>/home`）、`PUT /api/repos` 校验、`GET /api/agent-drivers`、运行时面板 | 非空时：绝对路径、已存在的目录、不等于 `$HOME/.codex`、**不在 `os.TempDir()` 之下**（按路径分量判断，先 EvalSymlinks） |
| `LATHE_CODEX_BIN` | `codex` | CodexDriver、运行时面板 | 仅 codex 启用时 `exec.LookPath` |

启用步骤：`mkdir /srv/lathe-codex && CODEX_HOME=/srv/lathe-codex codex login --with-api-key` → 在
`/srv/lathe-codex/config.toml` 写模型 / provider → `LATHE_CODEX_HOME=/srv/lathe-codex`。模型、provider、推理强度都由
运维写在这个目录里，Lathe 不读也不改。cc-switch 通道（`LATHE_*_CHANNEL`）是 claude 专属机制：codex 下分诊与实现用同一个模型。

## 7. 安全、隔离与降级清单

- **env 白名单**：与 claude 同一个 `sanitizedEnv()`；`OPENAI_API_KEY` 之类不在白名单里——推荐把凭据放在专用 home 的
  `auth.json`（`codex login`），确需环境变量就用 `LATHE_AGENT_ENV_EXTRA` 显式开口。
- **显式 Dir**：`supervise` 统一设 `cmd.Dir`；codex 拒绝空 Dir。
- **`$HOME` 隔离**：`CODEX_HOME` 挡住个人 `~/.codex`（config、hooks 及其信任状态、skills、memories）；`HOME` 换成
  `<LATHE_CODEX_HOME>/home`（启动时 `MkdirAll 0700`）挡住 `~/.agents/skills`（P15）。这个目录对沙箱**只读**（不进
  `writable_roots`），防止某个任务写技能给之后所有任务投毒。代价：agent 在会话里自己跑的工具写 `$HOME/.cache`
  会失败（与 workspace-write 沙箱下写真实 `$HOME` 失败是同一回事）；Lathe 自己跑的验证、提交都不在 agent 子进程里，不受影响。
- **临时目录（P17）**：`CODEX_HOME` 位于 codex 进程的临时目录（`TMPDIR`，未设时 `/tmp`）之下时，codex 拒绝建 helper
  二进制，shell 工具一条都不执行、turn 照样 `turn.completed`——运行期分辨不出来。所以启动时拒绝；驱动起进程前还按
  子进程最终 env 里的 `TMPDIR` 再查一次（防 `ExtraEnv` 改写），命中返回 `ErrDriverUnavailable`、不起进程，任务以可读原因失败。
- **不用 `--ignore-user-config`**：它只是不读 `$CODEX_HOME/config.toml`，对技能、hook 发现无作用，却会让运维配的 provider 失效。
- 系统级 `/etc/codex/…` 属于机器管理员配置（与 claude 的 managed settings 同一地位），不屏蔽。
- **沙箱**：`workspace-write` 只允许写 cwd；worktree 的 `.git` 指向 mirror，agent 在沙箱里 `git commit` 会失败——
  不影响流水线，提交由 `stageCommit` 做。
- **降级（明确不做 1:1 的地方）**：codex 没有 subagent 记录（只看主会话）；codex 不报费用（「费用未知」）；PRD 规划与
  预览推荐继续只用 claude；会话文件节点本地（`CODEX_HOME/sessions`，与 `~/.claude/projects` 一样，跨节点 resume 走现有降级分支，
  回收不在本期）；Linux 沙箱（landlock/seccomp）不可用的容器里命令会失败并以 `implement_run` 失败（不兜底 `danger-full-access`）。
- **非属主 404**：仓库与任务读写仍走 `WHERE … AND user_id`；`/api/agent-drivers` 不涉及属主资源。

## 8. 实现与验收结果

见 §8.1（由验收运行回填）。

### 8.1 验收结果
