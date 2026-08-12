# Lathe — 技术选型

> 2026-08-12 · 决策人：结合产品负载特征独立判断，**非**延续团队现有栈

---

## 1. 先定义负载特征（选型的唯一依据）

把 Lathe 剥掉业务描述，它到底是个什么程序？

| 特征 | 判断 |
|---|---|
| 计算特征 | **几乎 100% I/O 与进程编排**。挂钟时间全花在等：agent 跑（分钟级）、compose 起栈（十秒级）、测试跑（分钟级）、git 操作。零 CPU 密集环节 |
| 本质职责 | **一个进程监管器**（process supervisor）：spawn `claude` CLI、流式收日志、超时杀、管 per-task compose 栈、清孤儿进程 |
| 并发规模 | 数十个并发任务，每个大部分时间阻塞在子进程上。不是万级连接 |
| 正确性要求 | 状态转移必须可靠、可重放、幂等（webhook 重投递） |
| 运行形态 | **长期驻留的守护进程** + **需要能随时部署到任意新服务器**（D3 横向扩展） |
| 外部依赖 | Linear GraphQL、GitHub API、docker、git、claude CLI |

**一句话：Lathe 是个进程监管器，不是 Web CRUD，也不是数据处理管道。**

---

## 2. 结论

| 层 | 选择 |
|---|---|
| Control Plane + Runner | **Go 1.25** |
| 数据库访问 | **pgx**（查询手写，暂不引入 sqlc codegen —— 见 §6） |
| 依赖注入 | **手写 constructor 注入**（不用 Wire） |
| Web UI | **Vue 3 + Vite SPA**，`go:embed` 打进二进制 |
| 数据库 | PostgreSQL |
| Agent 驱动 | `claude` CLI + `--output-format stream-json` |
| 部署 | 单静态二进制 + Docker（二选一皆可） |

---

## 3. 为什么是 Go（四条理由，都来自 §1 而非团队习惯）

**① 进程监管是 Go 的正中靶心。**
`os/exec` + `context` 取消 + 显式进程组（`Setpgid`）+ 信号处理，这套组合在杀子进程树、防孤儿、优雅关闭上语义明确。Lathe 一旦崩溃留下游荡的 `claude` 进程和悬空的 compose 栈就是运维事故，而这恰是 Go 最成熟的能力面。

**② 单静态二进制让 D3 的多节点几乎免费。**
新增一台阿里云节点 = `scp lathe-runner` + 跑起来。没有运行时、没有 `node_modules`、没有虚拟环境、没有版本地狱。这是"定位为产品、要能装到任意服务器上"这个要求下最省事的形态。配合 `go:embed` 把 Vue SPA 塞进同一个二进制，**整个产品交付物是一个文件**。

**③ goroutine 精确匹配"数十个各自阻塞在子进程上的任务"。**
每任务一个 goroutine 直写顺序逻辑（起栈→跑红→改→跑绿→拆栈），不需要异步染色，不需要状态机套回调。

**④ 长期驻留的可靠性。**
控制面要跑几周不重启，还要管租约与心跳。这是 Go 服务的日常，不需要额外功夫。

---

## 4. 认真考虑过 TypeScript，为什么最终没选

TS 的两个真实优势：**Claude Agent SDK 原生是 TS**（session 管理/resume/结构化流式事件），以及**和 Vue UI 同语言**。

否决理由：

**① SDK 的优势被 CLI 抵消了。** 实测 `claude` CLI 已提供 `--print` + `--output-format stream-json` + `--resume`，Go 侧读结构化 JSON 事件流即可，不必解析人类可读输出。SDK 剩下的净收益很薄。

> 额外发现：CLI 还有 **`--from-pr`**（resume a session linked to a PR）——[02-design.md](./02-design.md) §3 约束① 要求"review 二轮必须 resume 原会话"，CLI 原生支持，无需自己维护 PR↔session 映射的全部逻辑。

**② Node 的弱项正好是 Lathe 的主战场。** 长期驻留 + 高频 subprocess churn 的 Node 守护进程，在信号处理、孤儿回收上更容易出错，而这是本产品的核心风险面而非边缘功能。

**③ 部署形态与 D3 冲突。** 每个节点要装 Node + `node_modules`，与"扔一个二进制就能加节点"差距明显。

---

## 5. 明确放弃的东西（承认代价）

| 代价 | 缓解 |
|---|---|
| Go 比 TS 啰嗦，开发速度慢一档 | 接受。本产品的难点在验证隔离与状态正确性，不在写 handler 的手速 |
| Linear GraphQL / GitHub 客户端在 TS 生态更顺手 | Linear 是 GraphQL，手写 query + 结构体足够；GitHub 用成熟的 `go-github` |
| 两种语言（Go + Vue） | 边界干净：Go 只出 JSON API，Vue 纯 SPA。不共享类型，用 OpenAPI 生成前端 client |

---

## 6. 刻意不沿用 console 的两个选择

产品定位不同，不该无脑复刻：

- **不用 Ent** → 改 **pgx**，查询手写。Lathe 的数据模型只有 8 张表且形状稳定，Ent 的 codegen 与运行时抽象换不来收益，反而拖慢迭代。迁移脚本是唯一事实来源。
  - *2026-08-12 修订*：原定 pgx + **sqlc**，实作时否决了 sqlc。理由：sqlc 要额外装二进制、维护 `sqlc.yaml`、每次改查询都要重新 codegen，而这套开销要换的"类型安全"在 8 张表、查询集不大的规模下收益很薄。查询本就以 SQL 字符串形式存在，将来查询量涨上来再切 sqlc 几乎无迁移成本 —— 那时再引入。
- **不用 Wire** → 手写 constructor 注入。组件数量在十几个量级，`main.go` 里显式串起来比维护 codegen 更清楚。

*（console 用 Ent + Wire 是它 18+ 实体规模下的合理选择，与 Lathe 的规模不同。）*

---

## 7. 仓库形态

**独立仓**（已定），初始骨架：

```
/opt/lathe
├── cmd/
│   ├── lathe/            # 控制面：API + 调度器 + 内嵌 UI
│   └── lathe-runner/     # 节点代理：监管每任务执行
├── internal/
│   ├── config/
│   ├── store/            # pgx + sqlc
│   ├── task/             # ★ 状态机（核心）
│   ├── scheduler/        # 能力匹配 + 租约
│   ├── runner/           # worktree / agent / verify
│   ├── integration/
│   │   ├── linear/       # OAuth + GraphQL + 回帖
│   │   ├── github/       # GitHub App + PR
│   │   └── agent/        # claude CLI driver (stream-json)
│   └── httpapi/
├── migrations/
├── web/                  # Vue 3 SPA → go:embed
├── docs/
├── go.mod
└── Makefile
```

---

## 8. P0 与 CLAUDE.md 约束的对接

CloudRouter 的 `CLAUDE.md` 规定 `feature/* → dev → test → main` 单向流动，**禁止 push 到受保护分支，一切走 PR**。Lathe 必须内建这条：

- 基线分支按任务类型选：`fix/*`、`feature/*` 从 `dev` 分叉；`hotfix/*` 从 `main` 分叉
- 仓库配置里存 `protected_branches`，Runner 推分支前校验目标不在其中
- Lathe 永不执行 merge —— 只开 PR（与 [02-design.md](./02-design.md) §1 边界一致）
