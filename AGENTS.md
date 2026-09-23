# AGENTS.md — Lathe 仓库 Agent 工作规范

> **唯一规范本体**。`CLAUDE.md` 只是本文件的入口别名，内容以本文件为准，勿抄成两份。
> 读法：**§1–§3 每次开工必读**；§4–§8 按改动类型对号入座；§9 是事故换来的陷阱清单。

---

## 1. 五条铁律（别的记不住就记这个）

1. **验证体系不可削弱**——红绿复现证明、隔离栈、验证落库（`verifications` 表）是这个
   产品的全部价值。让验证「看起来过了」的改动 = 拆产品根基。
2. **三条产品边界不可碰**：不做合并决策（人点合并）；单子不明确就回帖提问并停下
   （不猜）；永不 push 受保护分支，一切走 PR。对本仓提改动同样适用。
3. **每个可配置字段必须有消费方**；新增环境变量必须登记 README 变量表。
   删掉最后一个消费方时，连配置项一起删。
4. **提交前必过 `make lint && make test`**——不是裸 `go test ./...`（§5 有原因）。
5. **`.claude/`、`workspaces/` 是运行现场，不是源码**——不读、不改、不当参考实现、
   不「顺手清理」。

## 2. 开工自检（会话前 60 秒）

```bash
make dev-infra                              # Postgres on 55432：测试的硬依赖
make build && ./bin/lathe migrate up        # 建表
make lint && make test                      # 基线必须绿
```

- **基线红先修基线，再开工**——不要在红基线上叠改动然后分不清是谁弄坏的。
- 首次或前端依赖变更：`make ui-deps`。**不要裸 `pnpm install`**——esbuild 的安装
  脚本会停在交互确认上（Makefile 有注）。
- 改了 `web/` 必须 `make ui` 同步进 `internal/webui/dist`，否则二进制里是旧界面。
- `internal/webui/dist/.gitkeep` 不能删：删了 `//go:embed all:dist` 找不到目录，
  干净克隆里 build/vet/test 全挂。

## 3. 改动类型 → 动作映射

| 你在改什么 | 必须做的 | 额外验收 |
|---|---|---|
| Go 代码 | `make lint && make test` | 涉及 `task`/`runner`/`flow`/`cmd` 的并发逻辑 → 加跑 `make test-race` |
| 数据库 schema | 新编号 = `migrations/` 现有最大+1；up/down 成对 | §6 铁律；大表索引 `CONCURRENTLY` + 非事务标记（`internal/store/migrate.go`） |
| 前端 `web/` | 改完 `make ui && make build` | UI 变化的手测路径/截图写进 PR |
| 配置项 / 环境变量 | 先答「谁消费它」→ 写消费方 → 登记 README | 非法值启动报错，不运行期静默降级（先例：`LATHE_WORKTREE_TTL` 下限） |
| 任务状态机 `internal/task/state.go` | 对照全状态图确认无死锁、无绕过验证的捷径 | 状态转移是产品逻辑，PR 里写清每次转移的触发者 |
| `skills/**` | 视同改产品行为（会物化到所有目标仓库的 `.claude/skills/`） | 不得破坏 `$HOME` 隔离（`embed_test.go` 守着）；版本目录只新增不篡改 |
| agent 提示词 / 驱动 | 入口在 `internal/runner/pipeline.go`、`internal/integration/agent/` | PR 附一次真实任务的执行证据（事件流/验证报告） |
| docs 里的验收契约 | 逐条按契约验证 | 「不可验证的条目不算标准」（docs/08 原话） |

## 4. 权限边界：自主 vs. 先问

**可自主**：建 `feat/`·`fix/` 分支、改代码、跑测试与 lint、起停 `dev-infra`、
`make clean-test-db` 干跑、跑 `make ui`/`make build`。

**必须先问人**：

- 真删任何东西：`clean-test-db YES=1`、清 `workspaces/` 现场、删分支
- 动产品边界（§1.2）、动状态机语义、删 `skills/` 版本目录
- 改历史迁移文件（已发布的迁移只能新增，不回改）
- 引入新依赖（`go.mod` / `package.json` 变动）
- force-push（即使任务分支也要先说）

## 5. 测试纪律

1. `make test` 已强制 `-p 1`：领单调度（`internal/task.Machine.ClaimReady`）是全局
   查询，多测试包并行跑同一个真实 Postgres 会互相抢行。**不得破坏这个前提**
   （比如给测试加包级并行的假设）。
2. 测试依赖 `make dev-infra` 的真实 Postgres（55432）。新测试不得再引入别的外部
   服务；外部依赖走接口注入假实现，非连不可的要 `t.Skipf` 优雅跳过。
3. 表驱动 + `t.Run`，用例名说清「给定什么、断言什么」，失败信息带 `got/want`。
   完整规范：`skills/go-testing/1.0.0/SKILL.md`（内容即本仓规范，但改它 = 改产品，
   见 §3）。
4. **先红后绿**：补的测试必须先在改动前失败一次，证明它真的在测这件事。
5. fixture 的 email/标识带随机量、断言按归属过滤——`-p 1` 挡不住跨轮次残留
   （进程被杀时 `t.Cleanup` 不执行）。存量残留用 `make clean-test-db` 干跑查看。

## 6. 数据库迁移铁律

完整版：`skills/sql-migration/1.0.0/SKILL.md`。不可协商：

1. 每个 up 有能完全撤销它的 down。
2. 向后兼容：加列先允许 NULL 或带默认值；存量回填拆独立迁移。
3. 大表索引 `CREATE INDEX CONCURRENTLY`，用 `internal/store/migrate.go` 的非事务
   标记（`CONCURRENTLY` 不能进事务块）。
4. 迁移只做 schema，不写业务逻辑。
5. 删列/删表：先停读写观察一个发布周期，下一次迁移再真删。
6. 编号递增、不复用、不跳号；一次迁移只做一件事。

## 7. 安全红线

- 凭据 AES-256-GCM 入库、主密钥在库外；**日志/界面/事件流永不出现明文**凭据、
  会话令牌、webhook 密钥（有前科：`sanitizedEnv()` 曾把 token 漏给 agent 子进程——
  新起子进程时检查 env 是否白名单化）。
- 口令 bcrypt cost 12；会话与重置令牌库里只存 SHA-256。
- 非属主访问他人资源返回 **404 不用 403**——不用状态码暴露资源存在性。
- 不用请求 `Host` 头拼外发链接（重置邮件用 `LATHE_BASE_URL`，Host 可伪造）。
- 仅 `LATHE_TRUSTED_PROXY=true` 时信任 `X-Forwarded-For`。
- docker/compose 编排**复用 `internal/preview/`**，不写第二套——两套清理逻辑
  不一致就会漏容器（docs/08 D8-2 的结论）。

## 8. 文档路由（什么时候读哪本）

| 要动什么 | 先读 |
|---|---|
| 状态机 / 验证设计 / 调度 | `docs/02-design.md` |
| 编排图、多任务依赖 | `docs/07-prd-orchestration.md` → `docs/06-orchestration.md` |
| 「为什么这么选型」 | `docs/03-tech-stack.md` |
| agent 事件流 / 可见性 | `docs/04-agent-visibility.md` |
| 准入模式 / 分诊档位 | `docs/01-decisions.md` |
| 当前欠账与验收契约 | `docs/08-debt-cleanup.md`（最新批次）→ `docs/05-roadmap.md` |

架构决策、分叉点拍板要写进对应 docs（结论先行、给理由、承认代价）；改了行为
同步改文档；文档与代码冲突时**停下确认**，不默默选一个。`CHANGELOG.md` 跟随
Keep a Changelog + 语义化版本，发布时更新。

## 9. 已知陷阱（每条都是事故换来的）

| 陷阱 | 出处 |
|---|---|
| 裸 `go test` 包间并行互踩数据库 → 必须 `make test`（`-p 1`） | Makefile 注释 |
| 子进程不显式设 `Dir` 会继承 serve 的 cwd，把本仓 CLAUDE.md 灌进目标仓库的分诊上下文 | B2-3 / docs/05 |
| 重试路径若走 `tasks.Create` 会撞唯一索引——重试必须走 Fresh 计划先 `Discard` | 任务 #313 |
| 前置失败裸 `return` 绕过 `p.fail`，任务永远「排队中」无痕迹——任何失败都要落状态 | 「卡排队」事故 |
| 事件落库 UTF-8 截断（Digest 切断多字节）→ 整批丢弃 SQLSTATE 22021——字符串截断按 rune 不按 byte | 54075f4 |
| worktree 尸体阻塞同 issue 重试——失败保留现场 ≠ 不回收，重建前回收同名尸体 | #345/#466 |

## 10. Git 与 PR

- 分支：`feat/<topic>`、`fix/<topic>`，从 `main` 切，走 PR 回合，不直接推 `main`。
- 提交：Conventional Commits，中文描述，scope 用模块名，如
  `fix(runner): 收割机不再会删掉正在使用的现场`。
- PR 说清「改了什么、怎么验证的」；动状态机/验证/迁移的附测试证据。
