# 团队自定义技能（team skills）

> **状态：已实现**（分支 `feat/team-skills`，目标 `dev`）· 迁移 0062–0064（并行开发号段，见 §6）
> 设计评审稿：`/opt/lathe-wt/prd/f11-team-skills.md`（r5，含五轮评审答复）。本文是它的定稿：
> 去掉了易漂移的行号（只留「文件 + 符号名」，与 `docs/08-debt-cleanup.md` 的引用约定一致），
> 补上实现落点与验收结果。
> 相关决策：`docs/06-orchestration.md` §6 / §9「skills 存哪」、`docs/07-prd-orchestration.md` §F7.2、
> `docs/02-design.md` §9（`--setting-sources=project`）。

---

## 1. 结论先行

- **产品技能**（Lathe 自己维护、发给所有目标仓库的同一份规范）继续走本仓 `skills/` + `go:embed`，**一字不动**。
- **团队技能**（用户按仓库登记的自家约定）**存 DB、版本不可变**，在任务实现阶段物化进 worktree 的
  `.claude/skills/team-<slug>/`。两者用**目录前缀**分命名空间，结构上不可能互相覆盖。
- 可复现性靠**钉版本**：任务第一次物化时把「用了哪几个技能的哪一版」钉在 `team_task_skills`，之后
  续跑永远用钉住的版本；从头重建（`Fresh`）才清钉、按最新版重钉。
- 对 git 隐藏靠**每个技能目录里的哨兵 `.gitignore`**（内容 `*`），**不写** mirror 共享的
  `info/exclude`。技能文件因此不进 PR，也骗不过「工作区无改动」判死（验证体系里「agent 没干活」的唯一闸）。
- 不新增环境变量、不改状态机（`internal/task/state.go` 零改动）、不改「非属主 404」规则、
  `go.mod` / `web/package.json` / `skills/**` 零改动。

**代价（承认）**：被本任务钉住的 `team-<slug>/` 目录里，agent 的任何改动都不会进 PR（R12）；
保存时的凭据扫描是形态表 + 已知值，非穷举（§4.1）。

## 2. 为什么需要

每个仓库的工程约定不同（A 仓要表驱动测试，B 仓要迁移 `CONCURRENTLY`，C 仓要前端手测路径）。
在此之前只有两条出路，都不好：

1. 提 PR 改 Lathe 的 `skills/<name>/<version>/` —— 要本仓 review、要发版；
2. 写进目标仓库的 `.claude/skills/` —— 落回「`project` 档依赖目标仓库里恰好有什么」，不同分支不一致，
   且技能文件会跟着 agent 的 commit 混进 PR（`docs/06-orchestration.md` §6.2(b) 明确反对）。

## 3. 设计

### 3.1 数据模型

| 表（迁移） | 作用 | 关键约束 |
|---|---|---|
| `team_skills`（0062） | 技能本体：`user_id`、`repo_id`、`slug`、`title`、`enabled`、`archived_at` | `slug` CHECK（字符集 + 长度）；部分唯一索引 `team_skills_active_slug (repo_id, slug) WHERE archived_at IS NULL`（归档不占名字） |
| `team_skill_versions`（0063） | 不可变版本：`files` jsonb、`content_sha256`、`total_bytes`、`note` | 只 INSERT；`UNIQUE (skill_id, version)`；`total_bytes ≤ 262144` |
| `team_task_skills`（0064） | 任务钉版本：一条任务一行，`pinned` jsonb，冗余 `user_id/repo_id` | `pinned = []`（钉过了、当时没启用的技能）与「没有这一行」（还没钉）可分；属主索引 `(user_id, repo_id, pinned_at DESC)` |

`user_id/repo_id` 一律由服务端从 `repos` / `tasks` 行抄来，不接受 API 传入。

### 3.2 校验（保存时，服务端，400 + 中文原因）

实现在 `internal/teamskills`（只依赖标准库）：

| 项 | 规则 |
|---|---|
| slug | `TrimSpace → ToLower → 空白/下划线连续段换成单个 - → 去首尾 -`，再匹配 `^[a-z0-9]+(-[a-z0-9]+)*$`，≤ 40；不允许 `team-` 开头；保留名单 `ReservedSlugs` ⊇ 全部产品技能名（测试守着） |
| 文件路径 | 相对、`/` 分隔、逐段 `^[A-Za-z0-9._-]+$` 且不是 `.`/`..`、无反斜杠、无空段、深度 ≤ 4；顶层 `.gitignore` 是保留路径（哨兵） |
| 数量 / 大小 | ≤ 16 个文件；单文件 ≤ 128 KiB；单版本 ≤ 256 KiB；每仓在役技能 ≤ 20 |
| 编码 | 每个文件 `utf8.Valid` |
| SKILL.md | 必备；首行 `---`，**第一段** frontmatter 里有非空 `name:` 与 `description:` |
| title / note | ≤ 100 / 200 个字符，**按 rune 计、超限拒绝、不截断**（事故 54075f4） |
| 凭据 | `runner.FindCredential` 命中即拒，原因只带文件与行号（§4.1） |
| slug 改名 | 不支持；换名字就新建 |

上限都是常量（唯一消费方是保存校验，做成配置换不来运维价值，AGENTS.md §1.3）。

### 3.3 API（`internal/httpapi/teamskills.go` 的 `TeamSkillAPI`，独立结构体，不改 `api.go`）

| 方法 | 路径 | 语义 |
|---|---|---|
| GET | `/api/repos/{id}/team-skills` | 在役技能列表（含 `latestVersion`、`enabled`、`pinnedTaskCount`），不含内容 |
| POST | `/api/repos/{id}/team-skills` | 新建 + v1 → 201 |
| GET | `/api/repos/{id}/team-skills/{sid}` | 详情 + 版本元数据 |
| GET | `…/{sid}/versions/{v}` | 某一版的完整内容（原样，未改写 name） |
| POST | `…/{sid}/versions` | 新版本 → 201；内容与最新版相同 → **200**、不产生新版本 |
| POST | `…/{sid}/versions/{v}/restore` | 把旧版内容作为新版本提交，不改历史行 |
| PATCH | `/api/repos/{id}/team-skills/{sid}` | `{enabled}` / `{title}`；停用 ≠ 归档（列表仍可见） |
| DELETE | `/api/repos/{id}/team-skills/{sid}` | 归档（`archived_at = now()`），名字可复用 |
| GET | `/api/tasks/{id}/team-skills` | 任务钉住的技能与版本（`pinned` 区分「还没钉」） |

错误码：父资源不存在 / 不是你的 → **404**（不用 403）；同仓库同名在役 → **409**（库层 23505 映射，
不透出约束名）；校验与上限 → 400。版本写入在 `SELECT … FOR UPDATE` 锁内依次做 sha 比较与
`max(version)+1`，并发提交同内容只产生一个新版本。

`pinnedTaskCount` = 同一属主、同一仓库近 30 天内钉住过它的任务数；一条语句（`store.teamSkillListSQL`）
用 `pinned @> [{"skillId": id}]` 部分匹配，谓词按 `(user_id, repo_id)` 限定，走属主索引。

### 3.4 流水线（`internal/runner/teamskills.go`）

`Pipeline.TeamSkills`（`TeamSkillSource` 接口，`*store.Store` 直接满足；nil = 功能关闭）。

**钉版本 + 物化**（`prepareTeamSkills`，在 `stageImplement` 里、产品技能 `materializeSkills` 之后、
驱动 agent 之前）：

```
Fresh → PinsClear
PinsLoad；没钉过 → ListEnabled（enabled AND 未归档）→ PinsSave（先写者赢）→ 再读一次
materializeTeamSkills：
  1. 每个 pin：校验目录名、路径体检 checkTeamSkillPath、读版本内容（不存在 → 点名 slug@version 失败）、
     再校验一遍内容、核对 sha
  2. 已有目录先补回哨兵，再拍「物化前」快照（HasChanges + 完整 status，不带 pathspec）
  3. RemoveAll(dest) → MkdirAll → 先写哨兵 → 再逐文件 O_EXCL 写入 → 改写 SKILL.md 第一段 frontmatter 的 name
  4. 「物化后」快照必须与物化前相同，否则失败并列出变化的路径（≤ 20 条，按 rune 截断）
```

任何一步出错都 `p.fail(StageTeamSkillFailed)`（中文标签「团队技能物化失败」），不回落成「本次不加载技能」。

**路径体检**（`checkTeamSkillPath`）：`.claude`、`.claude/skills`、`dest` 逐级 `os.Lstat`，有符号链接或
存在但不是目录就拒绝；再 `git ls-files -z -- .claude .claude/skills .claude/skills/team-<slug>`（**不带尾斜杠**），
命中父路径本身、`dest` 本身、或 `dest/` 下任何被跟踪的条目就拒绝。全部 pin 体检完才动文件，所以
`RemoveAll` 不会顺着链接删到目标仓库的文件。

**提交前复位**（`hideTeamSkills`）：哨兵在 worktree 里，agent 能删能改。`stageCommit` 与修复轮各自的
`HasChanges` 之前调用：路径体检 → 重写哨兵（`O_NOFOLLOW`；被换成链接/目录时先删）→ 完整 status 里不得
出现钉住的 `dest` 下任何条目。`stageCommit` 里失败 → `StageTeamSkillFailed`；修复轮里失败 → 不提交、
按当前验证结果收尾（与「提交修复改动失败」同款）。续跑入口（`EntryCommit/EntryVerify`）没经过
`stageImplement` 时从库里读钉。

**重试**（`internal/runner/retry.go` `PlanRetry`）：新增一个 `case stage == StageTeamSkillFailed`，现场可用
就一律回到 `EntryImplement`（不看 `HasCommits/Dirty` —— PR review 二轮时分支已有第一轮的提交，落到
`default` 会被送去 `EntryVerify`，技能就被静默绕过了）；现场不可用则从头重建。`default` 分支一字未改。

### 3.5 前端

- 仓库配置页每个仓库卡片里加「团队技能」分区（`web/src/components/TeamSkillCard.vue`）：列表（版本、
  启用状态、近 30 天钉住数）、新建、编辑即写新版本、历史版本查看与恢复、停用/启用、归档。编辑器是
  纯文本域 + 逐文件标签。
- 任务详情「基本信息」里一行「团队技能」（`web/src/components/TaskTeamSkills.vue`，独立组件，便于与
  f04 的详情页重构合并）。

## 4. 安全

### 4.1 凭据不得进入技能内容

`runner.FindCredential(s, registered)`（`internal/runner/redact.go`，与脱敏同一文件）：

1. 复用脱敏总入口 `redactText`（同一张 `tokenShapes` 表，不复制正则）：结果与原文不同，第一处差异所在行即命中行；
2. 补充一条**只供检出用**的私有正则 `pemPrivateKeyHeader`（`-----BEGIN … PRIVATE KEY-----`），**不加入**
   `tokenShapes` —— 脱敏刻意不收 PEM，加进去会改变验证报告/事件流的脱敏结果。
   `TestFindCredential_PEMDoesNotChangeRedaction` 锁住「脱敏对 PEM 原样返回」与 `tokenShapes` 条目数。

已知值 = `config.KnownSecrets()` + 当前用户已保存的集成凭据（只解密、比对、丢弃）。三个写入路径
（新建 / 新版本 / 恢复）每个都扫。拒绝信息形如「SKILL.md 第 4 行像一段凭据（已拦下，不回显）……」。
日志只记 slug/version/sha256/字节数/行号，不记内容。

已知边界：形态表非穷举；PEM 只认标准 BEGIN 头；`token: "x"` 这类带引号 kv 会误伤（改写成占位即可）。

### 4.2 其它

- 非属主一律 404：每个 handler 先 `GetRepo(id, user)`，store 每条查询带 `user_id` 谓词；写技能时再用
  `repos WHERE id AND user_id … FOR UPDATE` 校验一次。
- 不新增 agent 子进程；新增的 git 调用（`ls-files`、`status`）走 `WorktreeManager.git`（显式 `cmd.Dir`、禁交互）。
- `$HOME` 隔离：`skills/embed.go`、`skills/embed_test.go` 未触碰；`TestTeamSkillSourceDoesNotReferenceHome`
  静态断言 `internal/runner/teamskills.go` 不出现 `UserHomeDir` / `Getenv("HOME")`。

## 5. 范围外

团队共享/成员可见性（f12：届时把 `user_id` 谓词换成团队可见性谓词，不改表结构）；按任务挑选技能（画像
扩展）；codex 驱动的技能加载（f06：f11 先合入则写死 `.claude/skills/`，f06 rebase 时换成
`agent.SkillsDir(driver)`，哨兵机制与根目录无关）；技能市场/导入导出；富文本编辑器/diff；手动覆盖钉版本；
技能被 agent 修改后回写库；状态机、非属主 404、历史迁移的任何改动。

## 6. 已知边界与遗留

| # | 内容 |
|---|---|
| R12 | agent 无法通过 PR 新增/修改**被本任务钉住的** `team-<slug>/` 下的文件（哨兵隐藏 + 提交前复位）。作用域只限当前 worktree 的这几个目录 |
| R13（既有，非本功能引入） | 产品技能的 `StageSkillMissing` 不在 `stageOrder` 里，review 二轮有提交时会被 `default` 送去 `EntryVerify`。建议另开 `fix/skill-missing-retry` |
| R14（既有，非本功能引入） | 产品技能路径的 `excludeSkillsDir` 把 `.claude/skills/` 写进 mirror 共享的 `info/exclude`：同 provider 仓库的所有 worktree（跨用户、永久）里 agent 新增的 `.claude/skills/**` 都不进 PR。f11 不调用、不修改它；`TestPipelineTeamSkillsNoCrossUserResidue/ProductSkillResidueIsPreexisting` 把现状固化成可观察事实。建议另开 `fix/skill-exclude-scope` 改用哨兵方案 |
| 迁移号 | 0062–0064 是编排方分配的并行开发号段，与 dev 当前最大号 0031 之间跳号，**违反** AGENTS.md §6.6；合入 dev 时按 dev 当时最大号 +1 连续重编号，只改文件名，不改迁移内容 |
| 重建路径 | `TestFreshRebuildRepinsLatest` 真跑一轮（首次物化钉 v1）→ 保存 v2 → 按生产的回收路径（`Discard` 删盘 + 收割机 `ClaimForReap` 置空 `worktree_path`，与 `memory_test.go` 的 `reclaimWorktree` 同一夹具）→ 重入队 → Fresh 执行。只 `Discard` 不落账时，重建会被 `ClaimedWorktreePaths` 把本任务自己的旧路径算作在途占用而拒绝接管 —— 那是既有行为，与 f11 无关 |

## 7. 验收结果

全部证据在 `/opt/lathe-wt/evidence/f11/`。「先红」= 改动前（或按 PRD 点名的错误实现做变异）失败的输出；
「后绿」= 同一命令在实现后通过。

| AC | 内容 | 红 | 绿 |
|---|---|---|---|
| AC-1 | 迁移可逆（up → down×3 → up） | — | `ac1-migrate.txt`（含 `team_skills_active_slug` 等索引定义） |
| AC-2 | 迁移只做 schema（无 DML 起头语句，阳性对照命中 2 行） | — | `ac2-schema-only.txt` |
| AC-3 | 库层约束：slug CHECK 23514、在役唯一 23505、两类级联、钉行属主 23502 | `red-ac3-constraints.txt`（表不存在） | `make test` |
| AC-4 | slug / 路径 / 上限 / name / rune 计数 / 保留名覆盖 / 产品形态不误杀 | `red-ac4-validate.txt`（桩） | `make test` |
| AC-5 | 凭据：形态 + PEM 第 3 行、PEM 不改脱敏、已知值、产品文本不误判、保存路径 400 不回显、用户集成凭据 | `red-ac5-findcredential.txt`（只走 redactText 时三个 PEM 用例 `ok=false`）；`red-api.txt` | `make test` |
| AC-6 | 第 21 个 400；`pinnedTaskCount` = 2 / 0；走属主索引 | `red-ac6-ac9-store.txt`（漏属主谓词时 S1 得 4） | `make test` |
| AC-7 | 非属主 404；任务钉技能 404；同名 409（顺序 + 并发 {201,409}，不透出约束名） | `red-ac7-409.txt`（无映射时 500） | `make test` |
| AC-8 | 版本递增、同内容 200 不 bump、恢复产生新版本、并发不同内容 {2,3} 且 v2/v3 的 sha 集合 = {sha(C1),sha(C2)} / 同内容 {201,200} 且都是 v2、v2 的 sha = sha(C1)（各 20 轮） | `red-ac8-concurrent.txt`（sha 在锁外时得 {201,201}）；`red-r6-gap3-concurrent-sha.txt`（v2+ 写错 sha 时集合断言红） | `make test` |
| AC-9 | 归档出列表/出钉集合、归档释放名字、停用不钉（store + API + 端到端）、已钉任务停用/归档后续跑仍物化 | `red-ac6-ac9-store.txt`（漏 enabled 时停用无效）；`red-runner-pipeline-unwired.txt` | `make test` |
| AC-10 | 不碰产品技能、改写 name、清掉旧文件、拒绝被跟踪路径、拒绝符号链接（3 格 + 2 对照，每格经 `prepareTeamSkills` + 真实 `p.fail` 断言落 `team_skill_failed`）、快照兜底与哨兵自检的故障注入、哨兵不写共享 exclude、agent 新增技能仍可见、根否定捞不回、复位、agent 植入链接（单测 + 流水线端到端：`team_skill_failed`、无提交、无 PR、外部无哨兵）、空钉不写、无 `$HOME` | `red-ac10-symlink.txt`（r4 体检变异：①③红；② 在该变异下仍被拦，原因已在证据里如实说明）；`red-ac14b.txt`（r3 实现：sha/可见性两格红）；`red-ac11d-hide.txt`；`red-r6-gap1-hide-symlink-e2e.txt`（stageCommit 吞掉复位错误时跑到 pr_open）；`red-r6-gap2-symlink-stage.txt`（物化错误落错阶段时 ①②③ 红） | `make test` |
| AC-11a | 空画像 + 团队技能：pr_open、物化且改写 name、无 go-testing、共享 exclude 不含规则、status/提交/ChangedFiles 不含 `.claude`、钉行属主正确、无伪转移 | `red-runner-pipeline-unwired.txt` | `make test` |
| AC-11b | agent 不改文件 → `implement_no_changes`、无 PR、无验证落库 | `red-ac11b.txt`（不写哨兵时任务继续推进） | `make test` |
| AC-11c | 与产品技能并存 | `red-runner-pipeline-unwired.txt` | `make test` |
| AC-11d | agent 删哨兵：写代码那格 pr_open 且技能不进提交、哨兵已复位；只删哨兵那格仍判死 | `red-ac11d-hide.txt`（无复位时技能进提交 / 跑到 pr_open） | `make test` |
| AC-12 | 续跑仍是 v1；Fresh 重钉 v2 且钉行只有 v2 | `red-ac12-fresh.txt`（不清钉时仍是 v1）；`red-r6-gap4-fresh-real-round.txt`（真跑首轮的前置下，不清钉时仍是 v1） | `make test` |
| AC-13 | 版本行被删 → `team_skill_failed`、点名 slug@version、agent 未驱动；PinsSave/PinsLoad/ListEnabled 出错同样失败；PlanRetry 五格；default 回归锁；review 二轮端到端（Inspect 证实有提交，重试仍回实现、不再推送） | `red-ac13-pinsave.txt`；`red-runner-pipeline-unwired.txt`（PlanRetry 第 2、3 格得 verify / commit） | `make test` |
| AC-14 | 功能关闭零副作用 | —（本条只证「关掉等于没有」） | `make test` |
| AC-14b | 共用 mirror 的用户 B 新增 `.claude/skills/b-new` 照常进 PR、共享 exclude 不含规则；对照格锁住既有残留 | `red-ac14b.txt` | `make test` |
| AC-15 | `make ui && make build`；冒烟：:8411 上走完新建 → v1 → 改一行 v2 → 同内容 200 → 查看 v1/v2 → 恢复 → 凭据 400 → 穿越 400 → 同名 409 → 停用 → 归档 | `red-ac15-ui-task-row-style.txt`（+ `.png`） | `ac15-smoke-api.txt`；界面：`ac15-ui-walk.txt`、`ac15-ui-01…13-*.png`（无头 chromium 逐步截图，含任务详情「团队技能」行） |
| AC-16 | `make lint && make test`、`make test-race`、点名测试清单（76 个 PASS） | — | `gate1-make-lint-test.txt`、`ac16-test-race.txt`、`ac16-named-tests.txt`、`ac16-existing-skill-tests.txt` |
| AC-17 | diff 不含 `go.mod/go.sum/web/package.json/skills/**/state.go/.claude/workspaces` | — | `ac17-diff-guard.txt` |

关于 AC-15 的一处如实说明：PRD 写「`git status` 里 `internal/webui/dist/` 有更新」。本仓 `.gitignore`
忽略 `internal/webui/dist/*`（只保留 `.gitkeep`），构建产物本来就不入库，所以 git status 不会显示它；
同步是否发生以 `make ui` 的输出与冒烟时前端产物里能找到「团队技能」分区为准（见冒烟证据末尾）。
