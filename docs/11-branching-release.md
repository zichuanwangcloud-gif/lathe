# 11 · 分支管理与发版规范

> 结论先行：**`dev` 是日常集成分支（也是 GitHub 默认分支），`main` 只承载已发布
> 的代码**。功能/修复都从 `dev` 切、PR 回 `dev`；发版从 `dev` 切 `release/X.Y.Z`，
> 冻结、出 RC、验收，再经 PR 合入 `main` 并在 `main` 上打正式 tag；线上紧急问题
> 走 `hotfix/X.Y.Z`。任何人（含管理员、含 agent）都不能直接 push `main` / `dev`。

## 1. 为什么要改

到 v0.2.0 为止的做法是「所有分支直接 PR 进 main，想发版时在 main 上打 tag」，
暴露了三个问题：

1. **main 不等于可发布**：v0.2.0 的 CHANGELOG 段写好、合进 main 两天后仍没打
   tag，期间 #20、#21 又合进来 —— 「main 上的哪个提交才是 0.2.0」已经说不清。
2. **main 没有任何保护**：仓库未设分支保护，CI 红着也能合、也能直接 push。
   「永不 push 受保护分支，一切走 PR」是产品边界（AGENTS.md §1.2），本仓自己
   却没受保护的分支。
3. **发版没有冻结期**：tag 打在一个仍在持续合入的分支上，验收的和发出去的未必
   是同一份代码；v0.1.1 发了 tag 却没写日志（release.yml 已为此加了校验）。

代价：多一条长期分支、每次发版多一次回合（main → dev），单人维护时略显繁琐。
换来的是 main 上每个提交都对应一个版本、发布内容可复现。

## 2. 分支模型

| 分支 | 从哪切 | 合到哪 | 生命周期 | 说明 |
|---|---|---|---|---|
| `main` | — | — | 永久 | 只放已发布代码；每个合入点都打 `vX.Y.Z` tag |
| `dev` | `main` | — | 永久，**默认分支** | 日常集成；永远保持 CI 绿 |
| `feat/<topic>` `fix/<topic>` `chore/` `docs/` `refactor/` `test/` | `dev` | `dev` | 合入即删 | 一事一分支，Conventional Commits |
| `release/X.Y.Z` | `dev` | `main`，再回合 `dev` | 发版期间 | 冻结：只收修复与发布元数据 |
| `hotfix/X.Y.Z` | `main` 上的上一个 tag | `main`，再回合 `dev` | 一次修复 | 只修线上问题，不夹带功能 |

```
feat/x ──┐      fix/y ──┐
         ▼              ▼
dev ──●──●──────●───────●──────────────●── (回合)
              \                       /
release/0.3.0  ●──fix──●(rc.1)──●(rc.2)
                                  \
main ──●(v0.2.0)───────────────────●(v0.3.0)──●(v0.3.1)
                                    \        /
hotfix/0.3.1                         ●──fix─●
```

### 2.1 强制手段

规则写在文档里不算数，下面三层都是实际生效的：

| 层 | 机制 | 管什么 |
|---|---|---|
| GitHub 规则集 | `main 保护`、`dev 保护`、`版本 tag 不可变`（不设任何绕过者，管理员同样受限） | 禁直接 push / force-push / 删除；必须 PR；必须过 CI；tag 打了就不能改不能删 |
| CI `分支流向` job（`ci.yml`） | 目标是 `main` 的 PR，源分支必须是 `release/X.Y.Z` 或 `hotfix/X.Y.Z` | 挡住「feat 分支直接 PR 进 main」 |
| 本地 git 钩子（`make hooks` 安装一次） | `pre-commit` 拒绝在 `main`/`dev` 上提交；`pre-push` 拒绝推这两个分支 | 在本地就拦下，不用等远端拒绝 |
| `release.yml` 校验 | 正式 tag 必须落在 `main` 上，RC tag 必须落在 `release/*` 或 `hotfix/*` 上 | 挡住「在功能分支上打正式 tag」 |

规则集的具体参数：

| 规则集 | 目标 | 规则 |
|---|---|---|
| `main 保护` | `refs/heads/main` | 禁删除、禁 force-push；必须 PR，只允许 merge commit；必需检查 `静态检查` `测试（真实 Postgres）` `构建镜像` `分支流向`，且要求与 main 同步（strict） |
| `dev 保护` | `refs/heads/dev` | 禁删除、禁 force-push；必须 PR，允许 merge / squash；必需检查同上（不要求 strict，减少无意义的 rebase） |
| `版本 tag 不可变` | `refs/tags/v*` | 禁删除、禁移动（update）、禁 force |

PR 审批数设为 0：目前是单人维护，GitHub 不允许给自己的 PR 点批准，设 1 会把
所有 PR 卡死。**「人点合并」仍然成立**——合并按钮只有人能点，CI 必须全绿。
团队扩大后把 `main 保护` 的审批数调到 1。

## 3. 日常开发

```bash
make hooks                                  # 每个克隆执行一次
git switch dev && git pull
git switch -c feat/<topic>
# ……改代码，make lint && make test ……
git push -u origin feat/<topic>
gh pr create --base dev                     # 默认分支已是 dev，--base 可省
```

- 合并方式：普通功能用 merge commit 或 squash 均可；squash 时标题必须仍是
  Conventional Commits，它会成为 CHANGELOG 的素材。
- **每个用户可感知的变更，在同一个 PR 里写进 `CHANGELOG.md` 的 `[Unreleased]`**
  （Added / Changed / Fixed / Removed / Security）。发版时只做改名，不临时补写。
- 合入后删除功能分支。

## 4. 版本号规则

遵循[语义化版本](https://semver.org/lang/zh-CN/)。项目处于 0.x 阶段，规则如下：

| 变更 | 0.x 期间 | 1.0 之后 |
|---|---|---|
| 只有缺陷修复，无新迁移、无配置语义变化 | PATCH（0.2.**1**） | PATCH |
| 新功能 | MINOR（0.**3**.0） | MINOR |
| **新增数据库迁移** | 至少 MINOR | 至少 MINOR |
| 不兼容变更：删/改名环境变量、原本接受的值改为启动报错、API 删字段、迁移不可在线执行 | MINOR + CHANGELOG「升级须知」 | MAJOR |

新增迁移强制至少 MINOR，是因为升级它需要运维额外执行 `lathe migrate up`
（以及确认能回滚）；PATCH 必须是「换二进制即可」的升级。

预发布号统一 `X.Y.Z-rc.N`（N 从 1 起）。不用 `-rc1`、`-beta` 之类的变体，
保持 `sort -V` 与镜像 tag 排序一致。

## 5. 正式发版流程

以发布 `0.3.0` 为例。所有命令在仓库根目录执行。

### 5.1 冻结

```bash
git switch dev && git pull
git switch -c release/0.3.0
```

在 `release/0.3.0` 上提交一次发布元数据（`chore(release): 0.3.0`）：

1. `CHANGELOG.md`：把 `## [Unreleased]` 下的内容移到新段落
   `## [0.3.0] - YYYY-MM-DD`，`[Unreleased]` 留空标题；更新文末对比链接
   （`[Unreleased]: …/compare/v0.3.0...HEAD`、`[0.3.0]: …/releases/tag/v0.3.0`）。
2. 有迁移或不兼容变更时，在该段落开头加 `### 升级须知`，写清：要跑哪些迁移、
   哪些变量改了、回滚到上一版需要做什么。
3. 跑就绪度核查：

```bash
make release-check VERSION=0.3.0
```

它检查：分支名与版本一致、工作区干净且与远端同步、tag 未被占用、版本号大于
上一个 tag、CHANGELOG 段落带日期且 `[Unreleased]` 已清空、有新迁移时版本号
至少升了 MINOR 且写了升级须知。

```bash
git push -u origin release/0.3.0
```

**冻结期间 `release/0.3.0` 只接受**：修复验收中发现的问题（`fix/*` 从
`release/0.3.0` 切、PR 回 `release/0.3.0`）、CHANGELOG / 文档订正。新功能继续
合进 `dev`，赶下一班车。

### 5.2 出 RC 并验收

```bash
git tag -a v0.3.0-rc.1 -m "Lathe v0.3.0-rc.1"
git push origin v0.3.0-rc.1
```

`release.yml` 会跑完整门禁、推送预发布镜像（不覆盖 `latest`）和二进制，生成
预发布草稿。用 RC 产物逐条验收：

- [ ] 用**上一个正式版的数据库**原地升级：`lathe migrate up` 成功
- [ ] `lathe migrate down` 回退一步再 `up`，均成功
- [ ] 冒烟实例上跑一个真实任务走完全程：分诊 → 红绿复现 → 修复 → 验证落库
      （`verifications` 表有记录）→ 开出 PR
- [ ] CHANGELOG 中每条 Added / Changed 在 RC 上可见、行为与描述一致
- [ ] 「升级须知」里的步骤照着做一遍能走通
- [ ] 二进制 `lathe version` 自报 `lathe 0.3.0-rc.1`

发现问题 → 在 `release/0.3.0` 上修 → 打 `v0.3.0-rc.2` → 重新验收。验收结论
（RC 号、勾选结果、执行证据链接）写进后面合入 main 的 PR 描述。

### 5.3 合入 main 并打正式 tag

```bash
gh pr create --base main --head release/0.3.0 \
  --title "release: v0.3.0" --body "<验收结论>"
```

CI 全绿后由人点合并（**merge commit，不 squash**——squash 会让 main 与 dev 的
历史分叉，回合时每次都冲突）。合并后：

```bash
git switch main && git pull
git tag -a v0.3.0 -m "Lathe v0.3.0"
git push origin v0.3.0
```

`release.yml` 校验 tag 在 main 上、CHANGELOG 有 `## [0.3.0]` 段，然后构建镜像
（`0.3.0`、`0.3`、`latest`）与二进制并生成 **Release 草稿**。人工过目说明与
产物后点「Publish」。

### 5.4 回合与清理

```bash
gh pr create --base dev --head main --title "chore(release): v0.3.0 回合到 dev"
```

冲突时（冻结期间 dev 改了同一处）：从 `dev` 切 `chore/backmerge-v0.3.0`，
`git merge origin/main` 解决冲突后 PR 到 `dev`。回合 PR 合入后删除
`release/0.3.0`。

## 6. 热修复流程

只用于**已发布版本**上影响使用的缺陷，以修 `0.3.0` 为例：

```bash
git switch -c hotfix/0.3.1 v0.3.0
# 修复 + 测试 + CHANGELOG 新增 ## [0.3.1] - YYYY-MM-DD 段落
make release-check VERSION=0.3.1
git push -u origin hotfix/0.3.1
gh pr create --base main --head hotfix/0.3.1 --title "release: v0.3.1"
# 合入后同 §5.3 打 v0.3.1、§5.4 回合 dev
```

约束：

- **hotfix 禁止新增迁移**。dev 上很可能已经用掉了下一个迁移号，hotfix 再占一个
  会让编号冲突（AGENTS.md §6.6「不复用、不跳号」）。确需改 schema 的问题，走
  正常发版。
- 只修一个问题，不夹带功能与重构。
- 改动可以先打 `v0.3.1-rc.1` 验收；极小的修复可直接合入，但 PR 里必须附复现
  与验证证据。
- 如果当时有 `release/*` 正在冻结，同一修复也要 PR 进该 release 分支。

## 7. 谁来做、什么必须人来做

| 动作 | agent 可自主 | 必须人确认 |
|---|---|---|
| 切 `feat/*` `fix/*`，PR 到 `dev` | ✓ | |
| 切 `release/*`、写发布元数据、跑 `release-check` | ✓（被要求发版时） | |
| 打 RC tag | ✓（被要求发版时） | |
| 合并任何 PR | | ✓ 人点合并 |
| 打正式 `vX.Y.Z` tag | | ✓ |
| 点 Release「Publish」 | | ✓ |
| 删除 `release/*` `hotfix/*` 分支 | | ✓ |
| 调整规则集 | | ✓ |

## 8. 常见问题

**能不能在 main 上直接打补丁再 tag？** 不能。规则集禁止直接 push main，
只能经 `hotfix/*` 的 PR。

**tag 打错了怎么办？** `v*` tag 不可删、不可移动。打错了就发下一个号
（例如 `v0.3.1`），CHANGELOG 里说明。这条是故意的：已经被拉取过的镜像/二进制
对应的 tag 一旦改指向，就无法再追溯「用户手里跑的到底是哪份代码」。

**PR 不小心开到了 main？** `分支流向` 检查会失败、无法合并。在 PR 页把 base
改成 `dev` 即可。

**本地需要临时绕过钩子？** `git commit --no-verify` / `git push --no-verify`。
但远端规则集照样会拒绝，钩子只是让你早点知道。
