# 团队与角色（Team & Roles）

> 功能线 f12 / 分支 `feat/team-roles`。本文是 PRD v5（评审通过版）的**定稿**：§1–§9 为定稿后的
> 方案正文（评审答复留在 PRD 原稿里，不再随文档分发），§11 为实现与验收结果。
>
> **团队共享的是「可见性」和「团队内的组织管理」，不是「执行身份」，也不是「仓库配置的编辑权」。**
> **凭据永不共享**：一个团队仓库的任务永远由「建任务那个人」的凭据执行。
>
> 信息边界的方向性变化（明确承认）：本功能交付后，Lathe 不再是「每个人看到的世界完全独立」的产品。
> 团队成员天然能看到彼此的邮箱、issue key、分支名、PR 链接与验证证据。这是团队功能的全部价值，
> 也是它的代价。
>
> **站内管理员不会自动成为任何团队的成员，团队 owner 也不是站内管理员**（`users.role` 与
> `team_members.role` 是两套正交的角色，§4.1）。
>
> 迁移号说明：`0065`–`0067` 是编排方为 12 条并行线分配的号段，与 dev 现有最大号 `0031` 之间跳号
> （AGENTS §6.6「不跳号」）。**合入 dev 前按 dev 当时最大号 +1 连续重编号，只改文件名、不改内容**
> （AC-1/AC-3 测试里的三个版本号与 AC-3 断言A 的基线号同步改）。

## 1. 问题与目标

### 1.1 为什么 Lathe 需要它

Lathe 在 P1.5 第二步之后是**严格的单租户模型**：`repos.user_id`、`tasks.user_id`、
`integrations.user_id` 三处把数据钉死在一个人名下，所有读接口对非属主一律 404。
这套模型对「一个人接一个仓库」是够用的，但对「一个小组共用一个仓库」是硬墙：

| 场景 | 今天会怎样 | 代价 |
|---|---|---|
| 三个人需要看同一个仓库的任务看板 | 每人都得自己配一份仓库，且**彼此看不到对方跑出来的任务**（任务查询按 `t.user_id`） | 团队看不到全貌与验证证据 |
| 组长要复核组员跑出来的 PR、看验证红绿证据 | 只能借组员的账号登录，或者去 GitHub 看 | 账号共享 = 审计失真（`actorOf` 记的不是真人）；且是安全红线 |
| 新人入职 | 要把仓库配置、分支模式、验证档位、受保护分支一条条手抄给新人 | 抄错 = 静默的验证降级（「受保护分支列表不能为空」就是为挡这类手滑而写的，`internal/httpapi/api.go:716-721`） |

缺的是一层**显式的成员关系**：谁能看见哪些仓库，谁能改哪些配置。这正是竞品 Multica
的团队模型；也是既有 `users.role`（`admin`/`member`，`migrations/0004_user_accounts.up.sql:23-30`）
**不解决**的那部分——`users.role` 是**站内系统角色**（能否进「系统设置」「用户管理」），
与「这个仓库归谁看」是正交的两件事。本功能新增的 `owner/admin/member` 是**团队内角色**，
两者不复用、不合并（见 §4.1）。

**本版明确不解决的两件事**（评审第 10 条，原 v1 的 §1.1 与 §3.6.4 声称解决、实际没有入口）：

- **「同一个 issue 被两个人各跑一遍」**：`repos_user_repo_unique(user_id, provider_repo)`
  保留（§3.1 取舍 3），每个成员仍各有一行 `repos`；活跃任务唯一索引按
  `(repo_id, tracker_provider, external_key)` 区分（`migrations/0021_internal_issues.up.sql:77-79`），
  派发入口 `resolveRepo` 只取属主自己的第一条仓库（`cmd/lathe/queue.go:642-645`）。
  两个成员对同一个 provider 仓库、同一个 issue 仍会各跑一遍。**列为范围外（§4.2），
  计入存量债务**：要根治得让「一个仓库配置归团队所有」并改派发/唯一索引，属独立设计。
- **「团队成员在共享仓库里建任务」**：没有入口。内置工单建单走
  `GetRepo(repoID, user.ID)`（只认属主，`internal/httpapi/issues.go:103-109`），
  Linear 接单走 `resolveRepo(ownerUserID)`。本版**成员对共享仓库只读**；建单仍是仓库属主
  （或成员在自己那份同名仓库配置上）的动作。§3.6.4 里「新成员在共享仓库建任务」的
  不可达场景已删除。

### 1.2 和「已验证 PR」定位的关系

团队的加入**不能**改变验证体系的任何一环。因此本 PRD 的第一条硬约束是：

> **团队共享的是「可见性」和「团队内的组织管理」，不是「执行身份」，也不是「仓库配置的编辑权」。**

任务仍然归属**创建它的人**（`tasks.user_id` 不变），管线仍按 `tk.UserID` 解析凭据
（`internal/runner/pipeline.go:415-419`）、仍按 `tk.UserID` 写事件（`internal/httpapi/api.go:160`）。
换句话说：组员能看到组内任务与验证证据，但**看不到、也用不到别人的凭据**；一个任务
跑起来用的永远是把任务建出来那个人的 Linear/GitHub 令牌。仓库的 `gate_mode` /
`verify_tier_override` / `protected_branches` / `baseline_dir` / `verify_infra` **只有仓库
属主能改**（§3.6.1，本版由 v1 的「admin 可改」反转为「只有属主」）。这条约束同时解决了
「凭据是否共享」——**默认不共享，且本 PRD 范围内不提供任何共享凭据的开关**。

### 1.3 目标（可判定）

1. 用户可创建团队（创建者自动成为 `owner`），**用一次性邀请码邀请他人**；被邀请人在
   「团队」页输入邀请码加入，成为 `member`。邀请**不能**凭「知道某人的邮箱」发起
   （Lathe 不验证邮箱，见 §3.1 的取舍与 §3.5 的安全论证）。未加入前**不可见**团队任何
   仓库或任务。
2. 角色矩阵落地：`owner` / `admin` / `member` 三类，权限按 §3.6 的矩阵执行。
3. `repos` 可被**仓库属主**「共享到团队」；共享后，该团队的成员能在自己的任务看板看到
   这个仓库下**所有**任务，能打开任务详情、事件流、验证证据；能看到仓库配置的**只读**视图。
4. 非成员访问团队仓库 / 团队任务 **仍然是 404**，且与「不存在」不可区分。
   **所有**「可见但无权」的写动作也返回 404（§3.5），全站只有一种拒绝形态。
5. 现有个人模型**零破坏**：不共享的仓库行为一字不变；`repo.team_id IS NULL` 时
   所有既有查询路径与今天完全等价。迁移是纯加法（新表 + 可空列 + 两条 `tasks` 索引）。
6. 角色变更、成员移除、仓库共享/取消共享都在**一个事务内**完成，多节点并发下
   不出现「最后一个 owner 被删掉」「成员已移除仍可读」的窗口。
7. 所有新增配置项都有消费方；**不新增环境变量**（§3.7）。

### 1.4 非目标

见 §4。

---

## 2. 现状取证（file:line）

### 2.1 隔离是「一人一条」的硬边界

| 事实 | 证据 |
|---|---|
| `repos` 主键 + `(user_id, provider_repo)` 唯一；没有「共享」概念 | `migrations/0001_init.up.sql:50-69`（唯一约束 :69） |
| `tasks` 同时带 `user_id` 与 `repo_id`，两个都是 NOT NULL | `migrations/0001_init.up.sql:94-97` |
| 任务唯一活跃索引按 `(repo_id, tracker_provider, external_key)`，对非终态部分生效 | `migrations/0021_internal_issues.up.sql:77-79` |
| `tasks` 上的索引全集：`tasks_state_created(state, created_at)`、`tasks_lease`、`tasks_flow`、`tasks_depends_on`、`tasks_queued_priority`、`tasks_one_active_per_item`。**`tasks` 上没有 `user_id` 索引，也没有 `repo_id` 独立索引** | `migrations/0001_init.up.sql:136`、`:138`；`0016:40-43`；`0021:77-79` |
| 任务列表查询 `WHERE t.user_id = $1`（**当前是 Seq Scan 或靠 state 索引**，本功能新增 `scope=team` 才需要新索引） | `internal/store/query.go:86`、`internal/store/query.go:102` |
| 任务详情 `WHERE t.id = $1 AND t.user_id = $2` | `internal/store/query.go:172` |
| 事件流二次校验 `t.user_id = $2` | `internal/store/agents.go:83` |
| 仓库列表 `WHERE user_id = $1` | `internal/store/query.go:314` |
| 仓库读取 `WHERE id = $1 AND user_id = $2` | `internal/store/query.go:343` |
| 仓库更新 `WHERE id = $1 AND user_id = $2` | `internal/store/query.go:422` |
| 存储层刻意把「不存在」与「不属于你」合并成同一个错误 | `internal/store/query.go:13-14`（`ErrRepoNotFound`）、`internal/store/query.go:334-335`（注释）、`internal/store/query.go:392-393` |
| `RepoRow` **不含** `user_id` 字段，`GetRepo` 也不返回属主 ID | `internal/store/query.go:279-305`、`:336-370` |
| **`GetRepo` 与 `ListRepos` 的列清单不同**：`ListRepos` 的 SELECT 含 `verify_infra`（`:312`），`GetRepo` 的不含（`:340-346`），所以 `GetRepo` 返回的 `VerifyInfra` 永远是 `nil`；`ListRepos` 对默认值 `'{}'` 返回的是**非 nil、len=0** 的切片（JSON `[]`）。存量现象，本线不修（`GetRepo` 原样不动），但本线的取行方式必须绕开它（§3.3） | `internal/store/query.go:308-331`、`:336-355`；`migrations/0019_repo_verify_infra.up.sql:16`（`NOT NULL DEFAULT '{}'`）；本线用 go.mod 里的 pgx v5.10.0 在 `lathe_f12` 实测 `SELECT '{}'::text[]` → `nil=false len=0` |

### 2.2 接口层的 404 分界（本功能**沿用**，不改）

| 事实 | 证据 |
|---|---|
| 任务详情：读失败即 404，注释写「别人的任务与不存在同等处理：对非属主隐瞒存在」 | `internal/httpapi/api.go:125-137` |
| 任务事件：`tk.UserID != CurrentUser(r).ID` → 404 | `internal/httpapi/api.go:149-154` |
| 重试 / 复核续跑 / 批准 / 重试计划 / 取消：同一模式逐处重复（**全是 404**，本功能不引入 403） | `internal/httpapi/api.go:320`、`:400`、`:462`、`:514`、`:604` |
| 预览接口：`TaskDetail` 失败即 404 | `internal/httpapi/preview.go:54-60`、`:100-104`、`:157-161`、`:201-205` |
| 仓库更新越权 → 404（`ErrRepoNotFound`） | `internal/store/query.go:435`；`internal/httpapi/api.go:741-751` |
| `PUT /api/repos/{id}` 可写字段含 `gate_mode`、`verify_tier_override`、`protected_branches`、`baseline_dir`、`verify_infra`（**因此不能给非属主开放**） | `internal/httpapi/api.go:741-751` |
| `baseline_dir` 随后被 docker compose 使用（本机任意绝对路径） | `internal/httpapi/api.go:804` |
| 基线检测/部署读仓库同样只认属主 | `internal/httpapi/api.go:775`、`:804` |
| 每个任务的凭据按 `tk.UserID` 解析，`Factory.ProviderFor(userID)` 带 per-user 缓存 | `internal/runner/pipeline.go:415-419`；`internal/creds/factory.go:43-56` |
| 环境变量兜底只给内置管理员 | `internal/creds/factory.go:11-19`（注释）、`:49-52` |
| 凭据按 `(user_id, kind)` 存取，AES-GCM 加密 | `internal/store/secrets.go:68-91`；`migrations/0001_init.up.sql:33-44` |
| **开放注册、不验证邮箱**；注册接口因此天然可枚举已注册邮箱（产品明确接受的既有妥协） | `internal/httpapi/accounts.go:58-65`（注释原文「不做邮箱验证：产品上选择『谁都能注册』」）；`README.md:93` |
| 派发入口只取属主自己名下的**第一条**仓库 | `cmd/lathe/queue.go:642-645`（`resolveRepo`） |
| 内置工单建单校验仓库归属只认属主 | `internal/httpapi/issues.go:103-109` |
| `net/http.ServeMux` **不能枚举已注册路由**（Go 1.25 无此 API）——权限覆盖测试必须由代码自己维护一张显式路由表 | 本线实测：`mux` 只有 `Handle`，无 `Routes()/Walk()` |

### 2.3 已有的「角色」先例（复用的是**模式**，不是表）

| 事实 | 证据 |
|---|---|
| 站内角色只有 `admin` / `member` 两级，CHECK 约束在库层 | `migrations/0004_user_accounts.up.sql:23-30` |
| 判定入口 `User.IsAdmin()` | `internal/store/users.go:63` |
| 管理员路由 `RequireAdmin`：未登录 401、非管理员 403 | `internal/httpapi/auth.go:352-366` |
| 「最后一个管理员不能被降级/删除」的守卫 | `internal/httpapi/admin.go:139-159`（`guardLastAdmin`） |
| 守卫被降级 / 改角色 / 删除三条路径共用 | `internal/httpapi/admin.go:184`（降级停用）、`:217`（改角色）、`:287`（删除） |
| 用户删除走 `ON DELETE CASCADE` 级联 | `internal/store/users.go:237-246`；外键范式 `migrations/0004_user_accounts.up.sql:57` |
| 邀请类通知可复用 SMTP 通路（先例：密码重置、PRD 搁置提醒）——**本线不用**，见 §3.7 | `internal/mail/mail.go:61-70`；`internal/store/smtp.go:114-160` |
| 现有登录/注册限流器可以直接复用到「邀请码猜码」场景 | `internal/httpapi/limiter.go:20-30` |

### 2.4 迁移与配置纪律的既有做法（本线照抄）

| 事实 | 证据 |
|---|---|
| 迁移按版本号升序应用，缺号不报错 | `internal/store/migrate.go:422-467` |
| 大表建索引必须 `CONCURRENTLY` + 首行 `-- lathe:no-transaction`；down 同样标记 | `migrations/0018_agent_events_result_index.up.sql:1-47`、`.down.sql:1-12`；`internal/store/migrate.go:27`（标记常量） |
| 非事务迁移必须**幂等可重跑**（先 `DROP ... IF EXISTS` 再建） | `migrations/0018_agent_events_result_index.up.sql:31-32`；`internal/store/migrate.go:32-45` |
| 环境变量非法值启动报错，不运行期静默降级 | `internal/config/config.go:49-56`、`:297` |
| README 有逐条变量表，新增必须登记 | `README.md:122-156` |
| 测试依赖真实 Postgres、`make test` 强制 `-p 1` | `Makefile:48-49`；`AGENTS.md` §5.1 |
| **`make test-race` 只覆盖 `./cmd/lathe ./internal/task ./internal/runner ./internal/flow`，不含 `internal/httpapi`、`internal/store`** | `Makefile:58-63` |
| 测试 fixture 的 email 带随机量、断言按归属过滤 | `AGENTS.md` §5.5；`internal/httpapi/api_test.go:427-452` |
| `clean-test-db.sh` 只删 `@example.com` 用户，靠级联带走其数据；默认 DSN 指向共享库 `lathe`；**docker 回退分支把库名写死成 `-d lathe`，完全不看 `LATHE_TEST_DSN`**。所以即使 `LATHE_TEST_DSN` 指向 `lathe_f12`，在本机（无 psql）跑脚本实际连的也是共享库 `lathe` | `scripts/clean-test-db.sh:22-24`、`:37`（默认 DSN）、`:44-45`（docker 回退） |
| AGENTS §4 把 `clean-test-db YES=1`（真删）列为「必须先问人」 | `AGENTS.md` §4 |
| `ListRepos(ctx, userID)` 有两个调用点：仓库页 `listRepos`，以及重试计划预览 `previewRetry`（注释「仓库配置与派发侧同语义：属主名下第一条」，按 `tk.UserID` 取 `repos[0]`）。后者**必须保持只看属主**，否则团队仓库会混进 `repos[0]` | `internal/store/query.go:307-331`；`internal/httpapi/api.go:227`、`:557-558` |
| f06（`agentDriver`）、f07（`vcs_provider`/`vcs_base_url`）都要往 `RepoRow` 加字段、往 `ListRepos`/`GetRepo` 的 SELECT 加列 | `f06-multi-cli.md:144`、`:461`；`f07-vcs-gitlab-gitea.md:112`、`:228` |
| 本线宿主环境**没有 psql**（`command -v psql` 为空）；`make dev-infra` 的 `lathe-postgres-dev` 容器映射 55432 | 本线实测 |

### 2.5 前端现状

| 事实 | 证据 |
|---|---|
| 导航是硬编码列表，无「团队」入口 | `web/src/App.vue:50-62` |
| 路由表集中注册，无团队路由 | `web/src/main.js:29-66` |
| `/api/me` 已经返回 `role` 等身份字段，是团队列表的天然挂载点 | `internal/httpapi/auth.go:289-305` |
| 仓库页以 `api.repos()` 拉取、逐条编辑 | `web/src/views/Repos.vue:50`、`:90`；`web/src/api.js:95` |
| 看板分页 `api.tasks({state, limit, offset})` | `web/src/views/Board.vue:33`；`web/src/api.js:76-83` |

---

## 3. 方案

### 3.1 数据模型（迁移 `0065`、`0066`、`0067`）

#### `0065_teams.up.sql` — 团队、成员、一次性邀请码

```sql
-- 0065_teams.up.sql
CREATE TABLE teams (
  id         bigserial   PRIMARY KEY,
  name       text        NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT teams_name_len_check CHECK (char_length(name) BETWEEN 1 AND 64)
);
-- 名字【不做】全站唯一：团队用 id 区分。理由见下方取舍 1。
CREATE INDEX teams_name_lower ON teams (lower(name));
CREATE TRIGGER teams_updated_at BEFORE UPDATE ON teams
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 成员关系。角色：owner / admin / member
CREATE TABLE team_members (
  team_id    bigint      NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
  user_id    bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role       text        NOT NULL DEFAULT 'member',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (team_id, user_id),
  CONSTRAINT team_members_role_check CHECK (role IN ('owner','admin','member'))
);
CREATE INDEX team_members_user ON team_members (user_id);
CREATE TRIGGER team_members_updated_at BEFORE UPDATE ON team_members
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 一次性邀请码。库里只存 code_hash 与不可逆的 code_hint，明文码只在创建响应里出现一次。
CREATE TABLE team_invitations (
  id          bigserial   PRIMARY KEY,
  team_id     bigint      NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
  code_hash   bytea       NOT NULL,                -- sha256(明文码)，16 字节随机码足够且不可枚举
  code_hint   text        NOT NULL,                -- 明文码末尾 4 位，供 owner 在列表里辨认是哪一条
  invitee_email text,                              -- 仅记录用，不参与判定，可为空
  role        text        NOT NULL DEFAULT 'member',
  invited_by  bigint      REFERENCES users(id) ON DELETE SET NULL,
  state       text        NOT NULL DEFAULT 'pending',
  created_at  timestamptz NOT NULL DEFAULT now(),
  expires_at  timestamptz NOT NULL,
  decided_at  timestamptz,
  accepted_by bigint      REFERENCES users(id) ON DELETE SET NULL,
  CONSTRAINT team_invitations_role_check  CHECK (role IN ('admin','member')),
  CONSTRAINT team_invitations_state_check CHECK (state IN ('pending','accepted','revoked'))
);
CREATE UNIQUE INDEX team_invitations_code_hash ON team_invitations (code_hash);
CREATE INDEX team_invitations_team_pending ON team_invitations (team_id) WHERE state = 'pending';
```

**为什么不做「加入申请 / 我可以申请加入某团队」**：那条接口必须按 `team_id` 判定
「这个团队存不存在、我是不是成员」，非成员调用时返回 404 会挡住功能、返回任何别的码
就等于把 `teams.id` 变成可枚举的存在性探针——与 §3.5 同一类问题。团队加入的唯一通路
因此是**持有属主发的一次性邀请码**（下面取舍 2），`teams` 表也不对外开放任何
「按名字/按 id 探测是否存在」的读接口。

**取舍 1（评审第 11 条）：团队名不做全站唯一。** v1 的 `teams_name_unique ON teams(lower(name))`
让任何登录用户都能用「建团队返回 409」探测别人的团队名是否存在，这与 §3.5/§7
「不泄露存在性」的立场直接冲突，也逼得两个无关小组不能同名。团队用 `id` 区分，
名字只是展示字段；`team_invitations_code_hash` 才承担「唯一标识」的职责。
代价：将来若有「按名字加入团队」的入口（本版没有），必须重新设计成不泄露的形态
（例如只接受精确的 id + 邀请码）。

**取舍 2（评审第 1 条）：邀请不按「目标邮箱」绑定。** Lathe 是开放注册、**不验证邮箱**
（`internal/httpapi/accounts.go:58-65` 原文：「不做邮箱验证：产品上选择『谁都能注册』」），
所以「邮箱 = 身份证明」这个前提在代码里**根本不存在**。v1 的 §3.4/AC-22「只有目标邮箱能接受」
可以被任何人这样打穿：先用被邀请人尚未注册的邮箱注册一个账号（开放注册不拦），
再在 `/api/me` 的待办里看到那条邀请并接受，之后就能读到团队共享仓库下所有任务、
事件流和验证证据。因此邀请改为**一次性邀请码**：

- 创建时生成 16 字节随机码（`crypto/rand`），响应里**只返回一次**明文码；库里只留
  `sha256` 与末 4 位提示。**不发邮件**（发信通路属 f09，见 §4.2）。
- owner/admin 通过**自己信任的带外渠道**（面对面、内部 IM）把码给具体的人。
  持有邀请码 = 被授权加入——这也是为什么码需要 128 位随机性：不可猜、不可枚举。
- 接受时按 `code_hash` 查 pending 且未过期的行，`UPDATE ... WHERE state='pending' AND expires_at > now()`
  单次生效；接受前若该 user 已是成员，视为已处理（幂等，返回同一 404，见 §3.4）。
- 邀请码**有效期 48 小时**、**单次使用**、**可撤销**。猜码路径复用既有 `limiter` **类型**
  （`internal/httpapi/limiter.go:20-30`），但 `TeamAPI` **自建一个私有实例**（`limiterOnce` + `newLimiter()`，
  与 `AccountAPI.lim()`（`internal/httpapi/accounts.go:54-57`）、`Auth.loginLimiter()`（`auth.go:77-80`）同构——
  这两个实例本就各自私有，不注入共享实例，避免改 `AccountAPI` 的字段与构造）。
  阈值：key `invaccept:<clientIP>`，**每 IP 15 分钟 10 次**（每次 accept 请求都计数，不论成败），
  超出 → 429，窗口内正确码也不放行。取值理由：128 位码不可暴力，限流挡的是脚本扫码与日志刷屏；
  10 次/15 分钟远高于人手输错的频率，与登录 `loginip` 30 次/5 分钟同一量级但更紧（accept 没有正当的高频场景）。
  阈值写成 `teams.go` 的常量，不做环境变量（§3.7）。
- `invitee_email` 只作为给 owner 看的备注，**不参与任何判定**，也不出现在别人能读的响应里。
  因此「给某人发邀请成功 = 此人已注册」这个探针消失了：创建邀请接口**完全不碰 users 表**。

被否决的两个替代方案（见 §9-D4 与 §10 第 1 条）：**按 userId 邀请 / 要求账号早于邀请创建**
——它挡不住「抢注者先注册了那个邮箱」这一情形（抢注发生在邀请之前，就满足「账号早于邀请」），
且照样要 owner 人工确认「接单的人是不是我要的那个人」；**先做邮箱验证**——超出本线范围，
且是全站性的产品决策。**接受后由邀请人确认**保留在 §9-D4 的备选里（团队 admin 在成员列表
对每个新成员「确认/移除」），本版不默认做：成员列表配移除按钮已经给了同等的补救能力，
再加一道确认会造出「已加入但不可见」的第三种状态。

#### `0066_repo_sharing.up.sql` — 仓库共享

```sql
-- 0066_repo_sharing.up.sql
ALTER TABLE repos ADD COLUMN team_id bigint REFERENCES teams(id) ON DELETE SET NULL;
-- 语义：NULL = 私有（今天的行为）；非 NULL = 共享给该团队
CREATE INDEX repos_team ON repos (team_id) WHERE team_id IS NOT NULL;
```

三条设计取舍，写进 PRD 也写进迁移注释：

1. **`team_id` 可空、默认 NULL** —— 存量每一行迁移后自动是 NULL，行为与今天逐字节一致，
   满足「现有个人模型平滑迁移」。
2. **`ON DELETE SET NULL` 而不是 `CASCADE`** —— 删团队不能连仓库配置一起删；那会让
   `tasks.repo_id` 挂空（`migrations/0001_init.up.sql:97` 是 `NOT NULL REFERENCES repos`），
   历史任务直接失去上下文。取消共享 = 回退成私有，是唯一安全的方向。
3. **`(user_id, provider_repo)` 唯一约束保留** —— 共享不改变「一个仓库配置只属于一个属主」，
   因此同一个人不能把同一仓库同时共享给两个团队，也不会出现两份配置各自漂移。
   代价（评审第 10 条）：「同一 issue 被两人各跑一遍」不在本功能解决范围（§1.1、§4.2）。

任务行**不加任何列**：任务的可见性由「任务所属仓库的 `team_id`」派生，不冗余
（与 f02 收件箱「尽量从现有状态派生」同一哲学，也避免两处真相源打架）。

#### `0067_tasks_access_index.up.sql` — 团队视图的访问索引

```sql
-- 0067_tasks_access_index.up.sql
-- lathe:no-transaction
-- scope=team 的列表是 (t.user_id = $1) OR (t.repo_id IN (我的团队仓库))，
-- 两臂各要一条索引；tasks 今天没有任何 (user_id, …) 或 (repo_id, …) 索引（§2.1）。
DROP INDEX CONCURRENTLY IF EXISTS tasks_user_updated;
DROP INDEX CONCURRENTLY IF EXISTS tasks_repo_updated;
CREATE INDEX CONCURRENTLY tasks_user_updated ON tasks (user_id, updated_at DESC, id DESC);
CREATE INDEX CONCURRENTLY tasks_repo_updated ON tasks (repo_id, updated_at DESC, id DESC);

-- 建完自检（照抄 0018_agent_events_result_index.up.sql:34-47）：CONCURRENTLY 中途失败
-- 会留下 indisvalid = false 的索引，且不会让别的步骤失败。把它变成响亮的迁移失败。
DO $$
BEGIN
  IF (SELECT count(*) FROM pg_index i
        JOIN pg_class c ON c.oid = i.indexrelid
       WHERE c.relname IN ('tasks_user_updated', 'tasks_repo_updated') AND i.indisvalid) <> 2 THEN
    RAISE EXCEPTION 'tasks_user_updated / tasks_repo_updated 未建成或处于 INVALID 状态，需重跑本迁移';
  END IF;
END
$$;
```

```sql
-- 0067_tasks_access_index.down.sql
-- lathe:no-transaction
DROP INDEX CONCURRENTLY IF EXISTS tasks_repo_updated;
DROP INDEX CONCURRENTLY IF EXISTS tasks_user_updated;
```

为什么单开一条迁移放索引：`CONCURRENTLY` 不能进事务块，而 `0066` 的 `ALTER TABLE`
放在无事务脚本里会失去「全成或全败」的保证。**一次迁移只做一件事**（AGENTS §6.6）。
两条索引的 `ORDER BY updated_at DESC` 与看板查询一致（`internal/store/query.go:102`）。
`tasks_user_updated` 同时是「我的」视图（`WHERE user_id=$1`）的顺带收益：今天那条
查询本就依赖 `tasks_state_created` 或全表扫描。

#### down 脚本

`0066` = `DROP INDEX repos_team; ALTER TABLE repos DROP COLUMN team_id;`；
`0065` = 逆序 `DROP TABLE team_invitations, team_members, teams`
（`DROP TABLE` 自带索引/触发器回收）；`0067` 见上。`Store.MigrateDown` 每次只回滚
**已应用的最高一条**（`internal/store/migrate.go:99-130`），一次 `down` 只执行 `0067` 的 down，
所以 PR 里的实测是**连续 `down` 三次 → 断言回到 `0031` 且三张表与 `repos.team_id` 都已消失 →
连续 `up` 三次**，整轮两遍（AC-3）。
`0065` 的 down 会连带丢失团队关系——这是可接受的：本迁移**不写业务数据**（§6 铁律 4），
团队关系全部来自人的显式操作，回滚后重新建即可。

### 3.2 访问判定的唯一入口（本功能的核心）

新增 `internal/store/team.go` 里的一个函数，**所有**读可见性判定都只调它，不在各 handler
里手写 JOIN：

```go
// RepoRole 是 viewer 对 repo 的关系；空串表示不可见。
type RepoRole string // "owner"(属主本人) | "team_owner" | "team_admin" | "team_member"
func (s *Store) RepoAccess(ctx context.Context, repoID, viewerID int64) (RepoRole, error) // 不可见 → ErrRepoNotFound
```

判定顺序（一条 SQL）：

1. `repos.user_id = viewerID` → `owner`；
2. 否则 `repos.team_id` 非空且 `EXISTS (SELECT 1 FROM team_members WHERE team_id = repos.team_id AND user_id = viewerID)`
   → 按成员的 `role` 返回；
3. 否则 `ErrRepoNotFound`。

任务级的「可见」= `RepoAccess(task.repo_id, viewerID)` 无错，因此团队可见性天然覆盖
列表、详情、事件流，**不需要在四个 API 里各写一遍团队 JOIN**。

**写权限不经过 `RepoAccess` 放宽**：所有承认非属主的写动作都不存在（§3.5、§3.6.1）。
`RepoAccess` 只回答「能不能看」，回答不了「能不能写」——写一律要求 `role == "owner"`。

**为什么不是「给 tasks 加 team_id」**：任务可能被共享/取消共享切换，冗余列会在切换时
留下历史脏数据；从 `repo_id` 派生是单一真相源。

### 3.3 存储层改造（最小改动面）

| 函数 | 今天 | 改成 | 风险 |
|---|---|---|---|
| `Store.ListTasks` (`internal/store/query.go:69-158`) | `WHERE t.user_id = $1` | 增加 `TeamView bool`；默认 `false` = **原 SQL 一字不改**。`true` 时走**另一条** SQL：`UNION ALL` 两臂（下方代码块），每臂各自按索引 `Index Cond` 取前 `limit+offset` 条，外层 `Merge Append` 归并后再 `LIMIT/OFFSET`；`count(*)` 同样拆成两臂相加（否则 `total` 与列表不一致）。**不用** `user_id = $1 OR repo_id IN (…)`：评审 B4 实测该写法只能得到整条索引全扫 + `Filter`、无 `Index Cond`（§10.2 B4） | 中：是热路径。由 `0067` 的两条索引保证，AC-28 用 `Index Cond` + Buffers 上界断言守 |
| `Store.TaskDetail` (`:160-230`) | `WHERE t.id=$1 AND t.user_id=$2` | **原名、原签名、原语义一个字不动**（f03 `f03-usage-stats.md:331`、f11 `f11-team-skills.md:528` 都沿用 `Store.TaskDetail(ctx, id, CurrentUser(r).ID)`，改名会让先合/后合的一方编译失败）。**只新增** `TaskDetailForViewer`：先 `SELECT user_id, repo_id FROM tasks WHERE id=$1`（无行 → 404），`RepoAccess(repo_id, viewer)` 通过后**直接调用 `s.TaskDetail(ctx, id, ownerID)`**，不自带列清单（与 `ListReposForViewer` 同一做法，v5） | 高：**这是唯一放松隔离的地方**。必须补「非成员仍 404」的测试（AC-6~9） |
| `Store.AgentEventsAfter` (`internal/store/agents.go:74-140`) | `JOIN tasks ... t.user_id = $2` | **SQL 不动**。调用方经 `ownedTask`（§3.3 下文、§3.11 f04 行）判定可见后，传入任务属主 ID（`tk.UserID`），只是不再要求它等于 `CurrentUser` | 低：隔离条件仍在 SQL 里，越权查询拿不到数据 |
| `Store.ListRepos` (`:308`) | `WHERE user_id = $1` | **原名、原签名、原函数体一个字节不动**（评审 v3-3）。两个既有调用点照旧：仓库页默认视图 `api.go:227`，重试预览 `api.go:558`（「属主名下第一条」，必须只看属主，否则 `repos[0]` 会混进团队仓库）| 低（不动） |
| `Store.ListReposForViewer` | — | **新增**，放在 `internal/store/team.go`（不放 `query.go`，见下文「为什么复用而不复制 SELECT」）。签名 `ListReposForViewer(ctx, viewerID int64) ([]ViewerRepo, error)`，`type ViewerRepo struct { RepoRow; Owned bool \`json:"owned"\`; TeamID *int64 \`json:"teamId"\` }`（内嵌 `RepoRow`，所以 JSON 是平铺的）。只给 `GET /api/repos?scope=team` 用 | 低 |
| `Store.GetRepo` (`:336`) | 属主 | **原样保留**，所有写路径与后台/基线/派发继续用它 | 低 |
| `Store.UpdateRepo` (`:422`) | `WHERE id=$1 AND user_id=$2` | **一个字都不改**。团队 admin / member 调用它必然 `ErrRepoNotFound` → 404。**不新增**「以属主身份代写」的变体（§3.6.1） | 低（不动） |
| `Store.GetRepoForViewer` | — | **新增**，放在 `internal/store/team.go`，只给读接口：`RepoAccess` 通过后，按属主 ID 调 **`s.ListRepos(ctx, ownerID)`** 并取出 `ID == repoID` 的那一行（**不调 `GetRepo`**：它的 SELECT 漏了 `verify_infra`，§2.1），返回配置行（含 `baselineDir`/`verifyInfra` 等，**不含任何凭据**）。`PUT /api/repos/{id}`、`POST /api/repos/{id}/sharing` 都不用它 | 低 |
| `Store.CreateRepo` (`:455`) | 属主 | 不动：新建的仓库永远私有，共享是第二个显式动作 | 低 |

> **评审第 4 条与本表的对应**：`RepoRow` 没有属主 ID，`GetRepo` 也不返回它——
> 本版**不新增** `RepoOwnerID`，因为「以属主身份代写」这条路径整个删掉了。
> v1 §3.6.1 里 `WHERE id=$1 AND user_id=(SELECT user_id FROM repos WHERE id=$1)`
> （条件恒为真，等于去掉 SQL 层隔离）**作废**。若将来 D2 被拍板成「admin 可写」，
> 必须先补一个只读的 `RepoOwnerID` 查询并在 PR 里单独论证，不在本版范围内。

**为什么 `ListReposForViewer` / `GetRepoForViewer` 复用而不复制 SELECT**（评审 v3-3，与 f06/f07 的接缝）：
f06 要往 `RepoRow` 加 `agentDriver`，f07 要加 `vcsProvider`/`vcsBaseURL`，两线都改
`ListRepos`/`GetRepo` 的 SELECT 列表与 `Scan`（`f06-multi-cli.md:144`、`:461`；`f07-vcs-gitlab-gitea.md:112`、`:228`）。
如果 f12 再抄一份列清单，先合入者加的列在后合入者的副本里就会漏掉，团队视图里那几个字段变成零值，而且不报错。
因此两个新函数**不自带列清单**，并且**只从 `ListRepos` 取行**（v5，评审第四轮 B1）：

> **为什么不再用 `GetRepo`**：`GetRepo` 与 `ListRepos` 今天就不是同一组列——`GetRepo` 的 SELECT 没有
> `verify_infra`（§2.1），返回的 `VerifyInfra` 恒为 `nil`。v4 的团队臂走 `GetRepo`，结果是属主看到真实的
> `verifyInfra`，成员在只读配置视图里看到 `null`（显示成「无隔离栈」）——这是错误的只读视图，
> 恰好错在评审第 3 条点名的验证配置字段上。统一从 `ListRepos(属主)` 取行后，属主行与团队行出自同一个
> 函数、同一组列，「f06/f07 改了列，团队视图自动跟上」的论证才成立。`GetRepo` 本身的缺列是存量现象，
> **本线不修**（它的调用方是写路径、基线、内置工单建单，都不读 `VerifyInfra`；顺手修会扩大本线改动面，
> 并与 f06/f07 改同一段 SELECT 产生文本冲突），写进 §6 风险表，留给后续单独的 fix。

```go
// ListReposForViewer：我的 + 我所在团队共享给我的。属主 ID 只在 store 内部使用，不出现在返回值里。
func (s *Store) ListReposForViewer(ctx context.Context, viewerID int64) ([]ViewerRepo, error) {
	mine, err := s.ListRepos(ctx, viewerID) // 原函数，原样调用
	// + 一条小查询：SELECT id, team_id FROM repos WHERE user_id=$1 AND team_id IS NOT NULL → 给 mine 填 TeamID，Owned=true
	// + 团队臂：SELECT r.id, r.user_id, r.team_id FROM repos r
	//            JOIN team_members m ON m.team_id = r.team_id AND m.user_id = $1
	//           WHERE r.user_id <> $1 ORDER BY r.user_id, r.id
	//   按属主分组：每个属主调一次 s.ListRepos(ctx, ownerUID)（原函数），只取团队臂里出现的 id；
	//   期间被删/被取消共享的 id 在 ListRepos 结果里找不到或已不在团队臂里 → 跳过；Owned=false
	//   **不调 GetRepo**（缺 verify_infra，见上）
}
func (s *Store) GetRepoForViewer(ctx context.Context, repoID, viewerID int64) (*ViewerRepo, error) // RepoAccess → 内部取属主 ID → s.ListRepos(属主) 取 ID==repoID 的行；找不到 → ErrRepoNotFound
```

这样 f06/f07 不论先合还是后合，只要改了 `ListRepos`，新列就自动出现在团队视图里，
**任何一方都不用碰 f12 的函数**；f12 不论先合还是后合，也不碰它们的函数。**接缝的前提写明**：
团队视图跟随的是 `ListRepos` 的列清单，不是 `GetRepo` 的。f06/f07 的 PRD 都要求两处 SELECT 同加列
（`f06-multi-cli.md:144`、`f07-vcs-gitlab-gitea.md:112`），所以对它们没有额外要求；若某条邻线只加到 `GetRepo`
而漏了 `ListRepos`，那是它自己仓库页列表的 bug，团队视图与仓库页列表同错同对。代价有两条：
一是团队臂是「每个属主一次 `ListRepos`」，团队里共享仓库的属主是个位数、每人名下仓库也是个位数，可以接受，不为它加缓存；
二是请求进行中某仓库恰好被取消共享，这一个响应里可能仍带着它，等价于请求早结束一毫秒，下一次请求即不可见（无缓存，§3.9）。
`GET /api/repos` 不带 `scope` 时 handler 仍调 `ListRepos`，响应体与今天逐字节相同（**不加** `owned`/`teamId`）；
带 `scope=team` 才调 `ListReposForViewer`。

**`scope=team` 列表的 SQL 形状**（`TeamView=true` 专用；`TeamView=false` 仍是今天那条 SQL）：

```sql
-- 臂 1：我自己的任务 → tasks_user_updated 上 Index Cond (user_id = $1)
-- 臂 2：我所在团队的共享仓库里【别人】的任务 → tasks_repo_updated 上 Index Cond (repo_id = r2.id)
--       t.user_id <> $1 保证两臂不相交，UNION ALL 不需要去重
-- 每臂 LIMIT $3 + $4（limit+offset），外层再 LIMIT $3 OFFSET $4
SELECT … FROM (
  (SELECT t.* FROM tasks t
    WHERE t.user_id = $1 AND ($2::text[] IS NULL OR t.state = ANY($2))
    ORDER BY t.updated_at DESC, t.id DESC LIMIT $3 + $4)
  UNION ALL
  (SELECT t.* FROM tasks t
     JOIN repos r2 ON r2.id = t.repo_id
     JOIN team_members m ON m.team_id = r2.team_id AND m.user_id = $1
    WHERE t.user_id <> $1 AND ($2::text[] IS NULL OR t.state = ANY($2))
    ORDER BY t.updated_at DESC, t.id DESC LIMIT $3 + $4)
) t
JOIN repos r ON r.id = t.repo_id
LEFT JOIN issues i ON …            -- 与今天同款
ORDER BY t.updated_at DESC, t.id DESC
LIMIT $3 OFFSET $4;
```

`count(*)` 同形：`(SELECT count(*) … 臂 1) + (SELECT count(*) … 臂 2)`。SQL 文本放在
`internal/store/query.go` 的一个包级常量里，`ListTasks` 与 AC-28 的 `EXPLAIN` **引用同一个常量**，
保证测的就是跑的。这条形状已在 `lathe_f12` 回滚事务里实测（§10.2 B4，证据
`/opt/lathe-wt/evidence/f12/prd-b4-explain.log`）：两臂分别出现
`Index Only Scan using tasks_user_updated … Index Cond: (user_id = 1)` 与
`Bitmap Index Scan on tasks_repo_updated … Index Cond: (repo_id = r2.id)`，
40 000 行 `tasks`、结果 50 行时共 52 个 buffer；同一数据上 OR-IN 写法是
`Seq Scan on tasks … Rows Removed by Filter: 36000`、618 个 buffer。

**`TaskRow` 需补一个字段**：`RepoID int64 \`json:"repoId,omitempty"\``（`internal/store/query.go:23-61` 现在的
`TaskRow` 没有仓库 ID）。**谁填它，写死**（v5，评审第四轮 nit 4）：

| 读路径 | `RepoID` | 理由 |
|---|---|---|
| `ListTasks` 默认视图（`TeamView=false`） | **0**（不序列化） | 原 SQL 一字不改；`omitempty` 让默认响应体与今天逐字节相同 |
| `ListTasks` 团队视图（`TeamView=true`） | 填（团队 SQL 的 SELECT 多一列 `t.repo_id`） | 前端按仓库分组/标注「共享自」要用 |
| `TaskDetail`（属主版，预览、`IssueContextFor`、f03/f11 的新接口） | **0** | `TaskDetail` 一字不改，SELECT 不加 `repo_id`；这些调用方都不读它 |
| `TaskDetailForViewer` | 填：先查出的 `repo_id` 在 `s.TaskDetail(...)` 返回后写回 `detail.Task.RepoID` | `RepoAccess` 本来就要它，免费 |

所以「属主版详情里 `RepoID` 是 0」是**有意为之**，不是漏填；任何新调用方要用 `RepoID`，必须走 viewer 版或自己查 `tasks.repo_id`。

**`ListTasks` 团队视图的列清单是一份副本**（评审第四轮 nit 4，如实承认）：`TeamView=false` 的 SQL 一字不改，
`TeamView=true` 的外层 SELECT 只能复制一份列清单与 `Scan`。它不像仓库那样能「调原函数取行」（分页、计数、
UNION 两臂都要在一条 SQL 里）。邻线给 `ListTasks` 加列时会漏掉这份副本，团队视图里的新字段变成零值而且不报错。
处置两条：① **测试守住漂移**：新增 AC-53 `TestListTasksTeamViewRowsMatchDefaultView`——对 viewer 自己的任务，
团队视图返回的行除 `RepoID` 外与默认视图的同一行 `reflect.DeepEqual`；谁往默认 SQL 加了列而没加到团队 SQL，
这条就红，并打印差异字段。② **§3.11 点名**：后合入 `ListTasks` 加列的一方（或 f12 rebase 时）负责同步
`listTasksTeamSQL` 常量与对应的 `Scan`。

**任务详情响应补 `viewerRole`**（`internal/httpapi/api.go:125-137` 的响应体追加一个字段）：
`owner` / `team_owner` / `team_admin` / `team_member` 之一。前端据此只显隐按钮，
**权限判定仍在服务端**（前端禁用态只是体验，不是防线）。

**签名改造：只新增，不改名**（评审 B3）：

```go
// TaskDetail：原样保留，只有属主（预览、IssueContextFor、f03/f11 的新接口 —— 行为与今天逐字节一致）
func (s *Store) TaskDetail(ctx context.Context, id, userID int64) (*TaskDetail, error) // 不动
// TaskDetailForViewer：新增。可见性 = 属主 ∪ 团队成员（只给任务详情、事件流用）
// 实现：SELECT user_id, repo_id FROM tasks WHERE id=$1 → RepoAccess(repo_id, viewerID) → s.TaskDetail(ctx, id, ownerID) → 回填 RepoID
func (s *Store) TaskDetailForViewer(ctx context.Context, id, viewerID int64) (*TaskDetail, error)
```

**任务读接口的归属 helper：沿用 f04 的 `ownedTask`**（v5，评审第四轮 B2；按合入顺序的分工见 §3.11 f04 行）：

```go
// ownedTask：名字与签名由 f04 定（f04-run-replay.md §3.2），两线共用这一个函数，不另起第二个。
// f12 之前的语义：任务存在且 tk.UserID == CurrentUser → (tk, true)；否则写 404「任务不存在」→ false。
// f12 之后的语义：tk.UserID == CurrentUser，或 RepoAccess(tk.RepoID, CurrentUser) 无错 → (tk, true)。
// 只给【读】接口用（events / summary / 验证日志）。写接口（retry/cancel/approve/review-retry）
// 继续内联 tk.UserID != CurrentUser(r).ID，一个字不改——名字里的 owned 是历史名，语义是「当前用户可读」，
// 函数注释必须写明这一句，防止将来有人拿它做写接口的判定（AC-11/19 从行为上守住）。
func (a *API) ownedTask(w http.ResponseWriter, r *http.Request) (*task.Task, bool)
```

`task.Task` 本来就有 `RepoID`（`internal/task/machine.go:20`、`:1068` 已 Scan），helper 不依赖 `TaskRow.RepoID`。
调用方拿到 `tk` 后，store 查询一律传 **`tk.UserID`（任务属主）**，不传 `CurrentUser(r).ID`——
`AgentEventsAfter` 今天就是这么写的（`api.go:160`），f04 新增的 `AgentEventsBefore` 与日志查询沿用同一约定，
SQL 里的 `t.user_id = $2` 第二道隔离因此保留且仍然成立。

f12 对 `api.go` 读接口的改动**只有两处**：`taskDetail` 换成 `TaskDetailForViewer`；`ownedTask` 的函数体
换成团队可见版（f04 未合入时，由 f12 把 `taskEvents` 里内联的 `api.go:149-154` 抽成 `ownedTask`，名字与签名照 f04 的定稿）。

**预览接口刻意留在 `TaskDetail`**：起容器要读 worktree、要 `docker compose up`，
是「在别人的机器上跑东西」的强动作，团队可见性不该把它一起放开（AC-32）。
`cmd/lathe/main.go:339` 的 `IssueContextFor` 同理（它用任务属主的凭据去查 Linear）。
本线对既有调用点的改动是 `internal/httpapi/api.go:130`（任务详情）换成
`TaskDetailForViewer`，以及事件流的归属判定改走 `ownedTask`（上一段）；`preview.go:56/102/159/203`、`cmd/lathe/main.go:339` 一行不改。

**派发路径完全不动**：`cmd/lathe/queue.go:428`、`:656-670` 的 `loadRepoConfig(repoID)` 本来就是
按 `repo_id` 读、不带 user 条件，团队任务照旧派发；`pipeline.setup` 用 `tk.UserID` 解析凭据
（`internal/runner/pipeline.go:415-419`），多人协作时任务仍用**创建者**的凭据。这是刻意
的：共享凭据是 §4.2 明确排除的。

### 3.4 API

新增 `internal/httpapi/teams.go`（独立文件，不改已有 handler 的签名，降低与其他 11 线冲突面）。
全部经 `Auth.RequireFunc`（登录即可），团队内角色在 handler 内判定。

**路由表是显式的**（评审第 5 条：`net/http.ServeMux` 没有枚举 API）：

```go
// teamRoute 是 teams.go 的全部路由，Routes() 与权限覆盖测试（AC-20）都从这张表读。
// 新增路由**必须**在这里加一行，否则 ROUTES-HAVE-NO-DENY-CASE 测试会失败。
type teamRoute struct {
	Method  string
	Pattern string
	Handler func(*TeamAPI, http.ResponseWriter, *http.Request)
	Write   bool // 写路由必须配一条 denyCases 用例（AC-20 断言）
}
var teamRoutes = []teamRoute{ /* … 与下表逐行对应 … */ }
func (a *TeamAPI) Routes(mux *http.ServeMux) { for _, rt := range teamRoutes { … } }
```

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| `GET` | `/api/teams` | 登录 | 我加入的团队（含我的角色） |
| `POST` | `/api/teams` | 登录 | 建团队；创建者 = `owner`；名字 1–64 字符；**不重名也允许**（§3.1 取舍 1） |
| `GET` | `/api/teams/{id}` | 成员 | 团队详情 + 成员列表（邮箱、角色、加入时间） |
| `PATCH` | `/api/teams/{id}` | `owner` | 改团队名 |
| `DELETE` | `/api/teams/{id}` | `owner` | 删团队。**要求先无共享仓库**，否则 409（§3.9 的取舍）；非 owner → 404 |
| `POST` | `/api/teams/{id}/invitations` | `owner`/`admin` | `{role, inviteeEmail?}`；**响应体里带一次性明文邀请码**（仅此一次）；不查 users 表 |
| `GET` | `/api/teams/{id}/invitations` | `owner`/`admin` | 列 pending（`id/role/hint/expiresAt/createdBy`，**不含邮箱以外的任何身份信息**） |
| `DELETE` | `/api/teams/{id}/invitations/{iid}` | `owner`/`admin` | 撤销邀请 |
| `POST` | `/api/team-invitations/accept` | 登录 | body `{code}`；按 `code_hash` 单次生效；未过期且 pending；**已是成员 → 同一 404**（幂等） |
| `PATCH` | `/api/teams/{id}/members/{uid}` | **只有 `owner`** | body `{role}`，`role ∈ {admin, member}`（`owner` 只能经 transfer 产生，传 `owner` → 400）。owner 可把任一非 owner 成员在 admin↔member 间升降；**`admin`/`member` 调用一律 404**（逐格见 §3.6 B5 表）；目标是 owner 本人 → 409「团队至少需要一个 owner，先转让」 |
| `DELETE` | `/api/teams/{id}/members/{uid}` | `uid` = 自己：任一成员（**退出**）；`uid` ≠ 自己：`owner`（可移 admin/member）、`admin`（只可移 member） | 移除或退出。owner 退出 → 409（先转让或删团队）；越权 → 404。**同一事务内**把被移除者名下共享到本团队的仓库置回私有（§3.4.1） |
| `POST` | `/api/teams/{id}/transfer` | `owner` | `{userId}`：把当前 owner 降为 `admin` 并把他升为 `owner`（单事务）；目标必须是本团队成员，否则 404 |
| `POST` | `/api/repos/{id}/sharing` | 仓库属主 **且**（共享时）是目标团队成员 | `{teamId}` 或 `{teamId: null}`。共享：调用者须是 `teamId` 的成员（`owner`/`admin`/`member` 任一角色均可）；取消共享：只要求是仓库属主。非属主 → `404 {"error":"仓库不存在"}`；团队不存在与「不是该团队成员」→ **同一个** `404 {"error":"团队不存在"}`，字节相同（§3.4.1） |
| `GET` | `/api/repos` | 登录 | 新增 query `scope=team` → `ListReposForViewer`，结果行带 `owned`/`teamId`；不带时仍调 `ListRepos`，响应与今天逐字节相同 |

**「最后一个 owner」守卫**：与 `guardLastAdmin`（`internal/httpapi/admin.go:143-159`）同构，
但**必须在事务里对着团队行加 `FOR UPDATE` 锁**（`SELECT ... FROM teams WHERE id=$1 FOR UPDATE`），
否则并发的转让/退出会双双通过检查。这是 §3.9 的并发要求，团队写事务的**全局锁序**见 §3.4.2。
由于 `PATCH` 不接受 `role=owner`、`transfer` 是「降旧升新」的单事务，**每个团队恰好一个 owner**；
守卫因此收敛为「owner 不能降自己、不能退出」两条 409。

#### 3.4.1 仓库共享的成员校验与离队收回（评审 B2）

`POST /api/repos/{id}/sharing` 的处理顺序（一个事务，锁序遵守 §3.4.2）：

1. `GetRepo(id, viewerID)` 失败 → `404 {"error":"仓库不存在"}`（与今天一致；调用者不是属主时不泄露仓库存在性）。
   这一步是无锁读，只用来尽早给出 404。
2. body 是 `{teamId: null}` → `UPDATE repos SET team_id = NULL WHERE id=$1 AND user_id=$2`，200。
   取消共享**不要求**仍是团队成员：数据所有者永远能收回。本步只写 `repos`，不碰 `teams`/`team_members`，
   不会与任何团队写事务成环（`SET team_id = NULL` 不触发外键检查）。
3. body 是 `{teamId: T}`，**按 §3.4.2 的顺序加锁**：
   - 3a. `SELECT 1 FROM teams WHERE id=$T FOR KEY SHARE` —— 先拿团队行。它就是随后 `UPDATE repos SET team_id=T`
     的外键检查本来要拿的那把锁，**提前到第一步显式拿**，消除「先锁成员行、后被外键隐式锁团队行」的倒序（评审 v3-1）。
     无行 → 走 3c 的同一个 404 分支。
   - 3b. `SELECT 1 FROM repos WHERE id=$1 AND user_id=$viewer FOR UPDATE` —— 再拿仓库行。无行（第 1 步之后仓库被删）→ `404 {"error":"仓库不存在"}`。
   - 3c. `SELECT role FROM team_members WHERE team_id=$T AND user_id=$viewer FOR SHARE`。
     无行（不论团队不存在还是自己不是成员）→ `404 {"error":"团队不存在"}`，**两种情况走同一个
     代码分支、同一个响应体**，`teams.id` 因此不再是探针。
4. `UPDATE repos SET team_id=$T WHERE id=$1 AND user_id=$2`，200。外键错误不可能出现
   （3a 已持有 `teams` 行的 `FOR KEY SHARE`，删团队的 `DELETE` 必须等本事务结束）。

**为什么 `member` 也能共享**：共享的是**自己的**数据给**自己所在的**团队，这是数据所有者的权力，
不是团队管理动作；限成 owner/admin 会让普通成员没法把自己的仓库拿给组里看，而这正是功能主体。
防注入的门槛是「你得是这个团队的成员」，不是团队内角色。

**移除 / 退出时收回共享**（`DELETE /api/teams/{id}/members/{uid}`，同一事务，锁序 teams → repos → team_members）：

```sql
SELECT 1 FROM teams WHERE id = $1 FOR UPDATE;                           -- ① 团队行：owner 守卫 + 与所有共享事务的 3a 互斥
UPDATE repos SET team_id = NULL WHERE team_id = $1 AND user_id = $2;   -- ② 仓库行：离队者名下共享到本团队的全部回私有
DELETE FROM team_members WHERE team_id = $1 AND user_id = $2;          -- ③ 成员行
```

v3 的顺序是「先 `DELETE team_members`，再 `UPDATE repos`」；v4 把这两句对调，让移除事务也遵守
teams → repos → team_members。正确性不依赖这个对调：①已经把同一团队上的共享事务挡在 3a 之外，
能与 ② 同时进行的共享事务只能是 ① 之前已拿到 3a 锁、并且已经提交的那些，而 `READ COMMITTED` 下 ② 取的是新快照，看得见它们的结果，不会漏收。
对调的作用是让本事务涉及的每张表都按同一顺序取锁，以后新增语句时不用重新论证。被收回的仓库数写进 `slog.Info`（`action=member_remove unshared=<n>`）。
代价：离队即回私有，剩余成员立刻看不到离队者的仓库与任务——这正是要的结果（与 AC-7 同向）。
D5「删团队前必须先无共享仓库」因此有了解法：owner 移除共享了仓库的成员即可收回其共享；
owner 自己的共享仓库由他自己取消。站内管理员删账号时，账号名下的 `repos` 本来就随
`ON DELETE CASCADE` 一起删除，不留可见残余（该路径的锁序见 §3.4.2 第 6 行）。

#### 3.4.2 团队写事务的全局锁序与 40P01 处置（评审 v3-1）

**评审意见成立，已实测复现**（证据 `/opt/lathe-wt/evidence/f12/prd-v4-lock-order.log`，PG 16.11，`lathe_f12` 临时 schema，
测完已 DROP）。v3 的共享事务先拿 `team_members … FOR SHARE`，再 `UPDATE repos SET team_id=T`，后者的外键检查隐式对
`teams` 行拿 `FOR KEY SHARE`；移除/改角色/转让事务先拿 `teams … FOR UPDATE`，再写 `team_members`。两者交错时
**三种管理动作全部死锁**：V3-1（移除）、V3-2（PATCH 角色）、V3-3（转让），报错都是
`deadlock detected … while locking tuple (0,1) in relation "teams"`。排查时另外发现两条同类的环，评审没有列出：
V3-4，「接受邀请」先改 `team_invitations`、后 `INSERT team_members`（外键隐式锁 teams），与「撤销邀请」（先 teams FOR UPDATE，再改 invitations）成环；
V3-5，共享事务与管理员删账号（`DELETE users` 级联删 `repos`/`team_members`）成环。
所以 v3 关于「两个顺序都安全」的说法不成立，AC-48 在 v3 设计下会因为 500 而失败，不会只表现为残留行。

**全局锁序（唯一规则）**：任何触碰团队数据的写事务，按 **`teams` → `repos` → `team_members` / `team_invitations`**
的顺序取锁；**第一条语句必须显式锁住涉及的 `teams` 行**（管理动作 `FOR UPDATE`，共享/接受邀请 `FOR KEY SHARE`），
不允许让外键检查在事务中途隐式去拿它。一次锁多个团队时 `ORDER BY id`。

| # | 事务 | 语句顺序 |
|---|---|---|
| 1 | 共享 `{teamId:T}` | `teams T FOR KEY SHARE` → `repos R FOR UPDATE` → `team_members(T,me) FOR SHARE` → `UPDATE repos SET team_id=T`（§3.4.1） |
| 2 | 取消共享 | 只有 `UPDATE repos SET team_id=NULL`（不触发外键检查，不碰团队表） |
| 3 | 移除 / 退出 | `teams T FOR UPDATE` → `UPDATE repos SET team_id=NULL WHERE team_id=T AND user_id=$uid` → `DELETE team_members` |
| 4 | PATCH 角色 / 转让 / 改团队名 / 删团队 / 生成邀请码 / 撤销邀请 | `teams T FOR UPDATE` → 锁内重读调用者角色 → 写 `team_members` / `teams` / `team_invitations`；删团队先在锁内数共享仓库（>0 → 409） |
| 5 | 接受邀请 | 无锁读 `SELECT team_id FROM team_invitations WHERE code_hash=$1` 取得 T（无行 → 404）→ `teams T FOR KEY SHARE` → 条件 `UPDATE team_invitations … WHERE code_hash=$1 AND state='pending' AND expires_at>now()`（`RowsAffected=0` → 404）→ `INSERT team_members … ON CONFLICT DO NOTHING` |
| 6 | 管理员删账号（`AdminAPI.remove`） | 新增 `Store.DeleteUserGuarded`（放 `team.go`）：单事务 `SELECT id FROM teams WHERE id IN (SELECT team_id FROM team_members WHERE user_id=$1) ORDER BY id FOR UPDATE` → 锁内逐团队判定（§3.9）：此人是 owner 且团队还有别的成员 → 整个事务回滚、409；此人是团队唯一成员 → `DELETE FROM teams WHERE id=$T`（邀请随级联删除；共享到该团队的只可能是他自己的仓库，随账号级联）→ 最后 `DELETE FROM users`。`Users.Delete` 原样保留（其他调用点不变），`remove` 只把这一次调用换成新函数。**`AdminAPI.Store` 可能为 nil**（`internal/httpapi/admin.go:22-23` 注释「为 nil 时设置端点不可用」，`routes_auth_test.go:19` 就没传）：`remove` 在调用前判 `a.Store == nil` → **503** `{"error":"团队守卫未装配，拒绝删除账号"}`，**不回落**到 `Users.Delete`（回落等于静默绕过守卫，AGENTS §3「不运行期静默降级」）。生产装配 `cmd/lathe/main.go:317-323` 已传 `Store: st`，不受影响；AC-30b 守住 |

实测（同一证据文件第二部分）：按上表实现的 F-1 至 F-9 九种交错，无论哪一方先开始，都**没有出现死锁**，结果都符合预期。
F-1/F-2 是共享与移除，后到的共享拿到 404，先到的共享会被移除收回；F-3/F-4 是共享与改角色、共享与转让，双方都成功；
F-5 是删团队在先，随后的共享 404；F-6 是接受邀请与撤销邀请，后到者 `RowsAffected=0`；F-7/F-8/F-9 是删账号分别与共享、移除交错。

**为什么没选评审给的另一条路「管理事务改 `FOR NO KEY UPDATE`」**：`FOR NO KEY UPDATE` 与 `FOR KEY SHARE` 不冲突，
能解开移除、改角色、转让这三条环，**但删团队本身就是 `DELETE teams`，隐含 `FOR UPDATE`**，照样与 v3 顺序的共享成环
（证据 B-1：`deadlock detected … relation "teams"`），接受邀请和删账号那两条环它也解不开。统一锁序只有一条规则，
而且覆盖全部六类事务，所以选它。`FOR UPDATE` 仍保留在管理事务上：两个并发转让必须互斥（AC-29）。

**40P01 / 40001 的处置**：锁序是防线，重试只是兜底。团队写事务一律经过 `internal/store/team.go` 的
`s.teamTx(ctx, fn)`，它负责 `BEGIN … COMMIT`，并遇到 `pgconn.PgError.Code ∈ {"40P01","40001"}` 时：

- 整个事务重跑，**最多 3 次**（含首次），两次之间退避 10–50 ms（随机）；
- 每次重试 `slog.Warn("team tx retry", "sqlstate", code, "op", op, "attempt", n)`，并给包内计数器 `teamTxRetries`（`atomic.Int64`，只读导出给测试）加一；
- 3 次仍失败 → 返回哨兵 `store.ErrTeamBusy`，handler 映射为 **`503 {"error":"团队数据正忙，请重试"}`**（不是 500，也不泄露存在性：只有通过授权读的调用者才会走到写事务）；
- 其他错误照旧返回，不重试。

`pgconn` 已经在依赖里（`go.mod:7` 是 `pgx/v5`，`internal/store/users.go:123` 已经在用 `pgconn.PgError`），**不引入新依赖**。
AC-48/49/50 守住：AC-48 断言并发下**没有 500/503、`teamTxRetries` 增量为 0**，即锁序本身没有成环，重试从未被用到；
AC-49 用确定性交错复现每一对；AC-50 单测重试与 503 映射。

**`/api/me` 扩展**（只加字段，不改既有字段）：`teams: [{id,name,role}]`。
**没有 `pendingInvitations`**：邀请不再按邮箱投递，所以「我有几条待接受邀请」这个问题
在服务端无从回答（要回答就必须按邮箱查，而那正是评审第 1 条打穿的口子）。

**越权响应形态统一为 404**（评审第 2 条）：团队内的所有越权（`admin` 改团队名、
`member` 改仓库配置、`admin` 动同级 admin、非 owner 转让 owner……）一律
`404 {"error":"团队不存在"}` 或 `404 {"error":"仓库不存在"}`，**不出现 403**。
唯一的非 404 拒绝是「owner 降级/退出自己」的 409 与「还有共享仓库不能删团队」的 409 ——
这两条都是**已通过授权之后**的业务约束，不泄露存在性。

### 3.5 「非属主 404」规则的处理（**默认一律 404**）

规则原文与意图：**不用状态码暴露资源存在性**（`AGENTS.md` §7；`internal/httpapi/api.go:132-133` 注释）。
本版按评审第 2 条**把 v1 的 403 方案反转**：

| 场景 | 今天 | 本功能 | 状态码 |
|---|---|---|---|
| 完全不知道这个仓库/任务的人（非成员） | 404 | 404（`RepoAccess` 返回 `ErrRepoNotFound`） | 404 |
| 成员（能看到该仓库）但无写权限，去改仓库配置（含 `admin`） | 该人根本看不到，无从发起 | 404 | 404 |
| 成员但无权操作**别人的任务**（重试/取消/批准/复核续跑/preview） | 404（非属主） | 404 | 404 |
| 团队 `admin`/`member` 去改团队名、删团队、转让 owner、动同级 | — | 404 | 404 |
| `admin`/`member` 去共享/取消共享别人的仓库 | — | 404 | 404 |
| 已授权的 owner 撞上「最后一个 owner」「还有共享仓库」 | — | 409 | 409（授权之后，不泄露存在性） |

**结论**：全站只有一种拒绝形态——**看不见或不能做，都是 404**。同一个人、同一个任务，
写接口与预览接口给出的状态码完全一致（v1 在这里自相矛盾：AC-11/13/15/16 要 403，
AC-20b 要 404）。不泄露存在性这条规则**一个字没改**，也不需要「可见但无权」这个中间态。
界面代价：无法区分「没这个任务」与「有任务但你不能操作」；缓解手段是用任务详情里的
`viewerRole`（自己可见时可读）来**事先**决定按钮显隐，而不是靠写请求失败后再解释。

> 403 方案（v1 的推荐）降级为 **§9-D3 的备选**，需人拍板才能改回，且**不在本版实现**。

### 3.6 角色权限矩阵（写进 docs/features/team-roles.md 的定稿版）

**团队内角色**（与 `users.role` 的站内 `admin`/`member` 正交）：

| 动作 | owner | admin | member | 仓库属主（若不在团队） | 拒绝时 |
|---|---|---|---|---|---|
| 看团队内共享仓库的任务列表 / 详情 / 事件 / 验证证据 | ✅ | ✅ | ✅ | ✅（自己是属主） | 404 |
| 看团队仓库配置的**只读**视图（分支模式、档位、受保护分支） | ✅ | ✅ | 只读 | ✅ | 404 |
| **改共享仓库配置**（`gate_mode`/`verify_tier_override`/`protected_branches`/`baseline_dir`/`verify_infra`…） | ❌ | ❌ | ❌ | ✅ | 404（§3.6.1） |
| 建任务 / 重试 / 取消 / 批准 / 预览起停 | ❌ | ❌ | ❌ | ✅ | 404（§3.6.2） |
| 共享 / 取消共享仓库 | 自己的仓库 ✅ | 自己的仓库 ✅ | 自己的仓库 ✅ | 共享到**非所在团队** ❌；取消共享 ✅ | 404（§3.4.1、§3.6.3） |
| 改仓库凭据（Linear/GitHub token） | ❌ | ❌ | ❌ | ✅ | 404（§3.6.4） |
| 生成邀请码 / 撤销邀请 | ✅ | ✅ | ❌ | — | 404 |
| 改成员角色（任何方向） | ✅（目标非 owner） | ❌ | ❌ | — | 404；owner 改自己 → 409 |
| 移除他人 | ✅（目标 admin/member） | 仅目标是 member | ❌ | — | 404 |
| 转让 owner / 删团队 / 改团队名 | ✅ | ❌ | ❌ | — | 404 |
| 自我退出团队（`DELETE …/members/{自己}`） | 409（先转让或删团队） | ✅ | ✅ | — | — |

**成员管理逐格判定表**（评审 B5：`TestTeamRoleMatrix` 与 `denyCases` 照此逐行落用例，不留解释空间）。
「目标」指 `{uid}` 当前角色；表外组合（非成员调用任何一行）一律 404。

| 调用者 | 动作 | 目标 = owner | 目标 = admin | 目标 = member | 目标 = 自己 |
|---|---|---|---|---|---|
| owner | `PATCH {role:"admin"}` | —（目标即自己） | 200（幂等） | **200**（升 admin） | 409 |
| owner | `PATCH {role:"member"}` | — | **200**（降 member） | 200（幂等） | 409 |
| owner | `DELETE` | — | 200 | 200 | 409 |
| admin | `PATCH {role:"admin"}` | 404 | 404 | **404**（admin 不能升人：升完即同级，自己再也管不了他） | 404 |
| admin | `PATCH {role:"member"}` | 404 | **404**（不能降同级） | 404 | 404（admin 不能改自己的角色；要降级找 owner，要离开用退出） |
| admin | `DELETE` | 404 | 404 | **200** | **200**（退出） |
| member | `PATCH` 任意 | 404 | 404 | 404 | 404 |
| member | `DELETE` | 404 | 404 | 404 | **200**（退出） |

所有 `PATCH {role:"owner"}` → 400（字段校验，在授权之后；非成员仍先得到 404）。
v2 矩阵里「改成员角色 member↔admin：admin ✅」作废：admin 对角色**没有任何**写权限，
admin 的团队管理权限收敛为「邀请 / 撤销邀请 / 移除 member」三项。

#### 3.6.1 为什么「改共享仓库配置」只给仓库属主（v1 → v2 反转）

评审第 3 条指出的问题成立且严重：`PUT /api/repos/{id}` 会写入
`gate_mode`、`verify_tier_override`、`protected_branches`、`baseline_dir`、`verify_infra`
（`internal/httpapi/api.go:741-751`）。把它开给团队 `admin` 意味着：

- 把 `gate_mode` 改成 `direct` = **拆掉人工闸门**；
- `verify_tier_override` 强制降到 `light` = **削弱验证**（触碰铁律 1）；
- 清空 `protected_branches` = 允许直推 `main`/`dev`；
- `baseline_dir` 指向本机任意绝对路径，随后被 docker compose 使用（`api.go:804`）；
- 之后用**属主自己的 GitHub 凭据**跑任务，按**别人定的规则**推分支、开 PR ——
  与 §3.6.2「组员触发别人的任务 = 用别人的身份」是同一个问题。

v1 的「共享的前提就是团队共识」这个理由不成立：团队共识不等于把**属主仓库的执行
与验证配置**交给别人，而属主随时可以取消共享来收回可见性（§3.6.3）。因此：

- `PUT /api/repos/{id}` 一个字节不改，仍然 `WHERE id=$1 AND user_id=$2` → 非属主 404。
- **不**实现「handler 先判角色、再以属主 ID 调用 `UpdateRepo`」的代写路径（SQL 层隔离保留）。
- 团队 admin 对配置只有**读**（`GetRepoForViewer`），读不到凭据（§3.6.4）。
- 团队内的 admin 权力收敛到**组织管理**：邀请、撤销邀请、移除 member（**不能改任何人的角色**，§3.6 B5 表）。
  这仍然有意义（属主休假时能把人加进来看任务、能把离职的人移出去），且不触碰执行面。
- 审计：成员/邀请/共享变更**只走 `slog.Info`**（`team_id/actor_id/target_id/role/action`）。
  仓库配置变更本身没有新事件（既有路径不变）。**不写 `task_events`**：`task_events.task_id`
  是 `NOT NULL REFERENCES tasks`（`migrations/0001_init.up.sql:144`），仓库配置变更没有
  可挂的任务，库里也没有仓库审计表；v1 那句「审计事件额外记 actor」在数据模型上做不到。
  这与 §4.2「不做审计界面」一致。

#### 3.6.2 为什么成员不能操作别人的任务

重试/取消/批准会触发状态转移（`internal/task/state.go:89` 的合法边表），而每条边都隐含
「这个人的凭据能推这个分支、能开这个 PR」。让组员触发别人的任务 = 用别人的 GitHub 身份
写代码，是 §7 红线的变体。**这条也顺带保住了状态机语义**：本功能**不新增任何状态转移**，
只是把「谁能发起转移」的判断放在 handler 层（判据仍是 `task.user_id == CurrentUser`），
`state.go` 一个字不改。拒绝形态 404（§3.5）。

#### 3.6.3 为什么共享/取消共享只有仓库属主能做

共享决定「谁能看见我的任务」，这必须是**数据所有者**的权力，不能由被动方（团队管理员）
单方面施加或撤销。取消共享等于立刻收回他人的可见性，也必须由所有者发起——
这条保证了「成员被移除后立刻看不见」（§3.9）在反方向同样成立：**属主随时能收回**。
非属主调用 `POST /api/repos/{id}/sharing` → 走 `GetRepo(id, viewerID)` → 404。
属主共享到自己**不在**的团队 → 与团队不存在同一个 404（§3.4.1）——否则任何人都能把自己的仓库
塞进陌生团队，向别人的看板注入数据。属主离队/被移除 → 同一事务内其共享回私有（§3.4.1）。

#### 3.6.4 凭据：默认不共享，且本功能不提供共享开关

- `integrations` 表仍是 `(user_id, kind)` 唯一（`migrations/0001_init.up.sql:44`），**不加 `team_id`**。
- `creds.Factory.ForUser(userID)`（`internal/creds/factory.go:59-61`）不变。
- 界面上的「凭据」页只展示**自己的**（`internal/httpapi/credentials.go:45-48` 的 `owner()` 就是 `CurrentUser(r).ID`）。
- 团队页不出现任何凭据字段；团队仓库详情页刻意不渲染「凭据状态」区块。
- **结论**：一个团队仓库的任务永远由「建任务那个人」的凭据执行。本版成员**不能在共享
  仓库建任务**（§1.1 已删掉那条不可达描述）：建单入口 `internal/httpapi/issues.go:103-109`
  只认属主。成员若想跑任务，只能在自己那份同名仓库配置上下单，那是**他自己的任务**，
  走他自己的凭据 —— 与团队可见性无关，也不产生「借身份执行」。

### 3.7 配置项

**本功能不新增任何环境变量。** 逐条说明（AGENTS.md §1.3「每个可配置字段必须有消费方」）：

| 候选配置 | 决定 | 理由 |
|---|---|---|
| `LATHE_TEAM_MAX_MEMBERS` | **不做** | 唯一的消费方是「邀请时校验」，而真正的滥用防线是站点级登录/注册限流（`internal/httpapi/limiter.go`）。为省一个 `len(members)` 判断引入一个必须登记 README、必须写非法值校验的环境变量，是负收益 |
| `LATHE_TEAM_INVITE_TTL` | **不做** | 邀请码有效期固定 48 小时，写死在 `internal/store/team.go` 的常量 + 注释里（这是业务规则，不是部署差异；与「团队名 1–64 字符」同口径）。它**有**消费方：accept 时的 `expires_at > now()` |
| 团队名长度上限 | **库里 CHECK（1–64）+ handler 校验** | 不是部署配置，是数据约束 |
| 邀请是否发邮件 | **不发** | 发信通路属 f09 边界（§4.2）。邀请码靠带外渠道传递，也避免「邮件里带团队名」这种新的泄露面 |

已有的可配项**一个都不动**：`LATHE_DB_*`、`LATHE_WORKTREE_TTL`、`LATHE_REAP_*`、`LATHE_*_SLOTS`、
`LATHE_AGENT_*`、`LATHE_SECRET_KEY` 的语义与消费方全部不变。README 的变量表因此**只增不改**，
若最终确实新增了变量（评审追加），必须同时登记到 `README.md:122-156` 的表格并写清默认值与非法值行为。

### 3.8 安全

| 红线 | 本功能的落实 |
|---|---|
| 凭据 AES-256-GCM 入库、主密钥库外 | 不动（§3.6.4）。团队表里**没有任何凭据列** |
| 日志/界面/事件流不出明文凭据 | 团队相关接口只返回 `id/name/email/role`。审计日志只记 `team_id/actor_id/target_id/role/action`。`actorOf` 格式不变（`user:<id>`），不引入邮箱到事件流 |
| 非属主 404 | §3.5：**一律 404**，包括「可见但无权」 |
| **邀请不可被抢注** | 邀请码 = 128 位随机一次性凭据，不在库里存明文、不发邮件、不接受「按邮箱投递」。持有码是授权前提（§3.1 取舍 2）；猜码按 IP 限流（TeamAPI 私有实例，10 次/15 分钟）；`code_hash` 唯一索引挡住重放 |
| 不用 Host 拼外发链接 | 本功能不发外发链接（不发邮件） |
| `X-Forwarded-For` 仅可信代理 | 不动；团队接口复用既有 `RequireFunc` |
| docker/compose 复用 `internal/preview` | 不动 |
| 子进程 env 白名单 / 显式 `Dir` | 不动。本功能不新增任何子进程 |
| 账号存在性不泄露 | 建团队不再因重名 409（§3.1 取舍 1）；创建邀请完全不查 users 表；`GET /api/teams/{id}` 只对成员开放（非成员 404，不泄露团队名）；`/api/me` 不再有「待接受邀请」字段 |
| 团队成员的邮箱可见性 | 只对**同团队成员**可见（`GET /api/teams/{id}` 要求成员）。这是团队功能本身的代价，写进 docs（§7） |

### 3.9 并发与多节点

| 风险 | 处置 |
|---|---|
| 并发转让 / owner 降级或退出 | 每个团队恰好一个 owner（`PATCH` 不接受 `role=owner`，只有 transfer 能产生 owner）。所有改角色/移除/转让先 `SELECT ... FROM teams WHERE id=$1 FOR UPDATE` 串行化，锁内重读调用者角色：两个并发转让中后到者发现自己已不是 owner → 404；owner 降/退自己 → 409「团队至少需要一个 owner」 |
| **锁序（v4）** | 全部团队写事务遵守 §3.4.2 的 `teams → repos → team_members/team_invitations`，首句显式锁 `teams`；40P01/40001 由 `teamTx` 重跑至多 3 次，仍失败 → 503。v3 的「两个顺序都安全」已实测不成立（§3.4.2） |
| 共享与移除并发 | 两者都先锁同一个 `teams` 行（共享 `FOR KEY SHARE`、移除 `FOR UPDATE`），互斥。共享先：移除在 ① 等待，共享提交后移除的 ② 取新快照收回。移除先：共享在 3a 等待，移除提交后 3c 查不到成员行 → 404。两个顺序都不会留下「非成员的仓库仍共享在团队里」，**也不会死锁**（实测 F-1/F-2；AC-48/49） |
| 邀请码被并发接受 | 先按 §3.4.2 第 5 行锁 `teams`，再 `UPDATE team_invitations SET state='accepted', accepted_by=$2 WHERE code_hash=$1 AND state='pending' AND expires_at > now() RETURNING …`；`RowsAffected=0` 即视为无效/已用（同一 404） |
| invite / accept 与已是成员的竞态 | 接受在单事务里：`UPDATE` 生效后 `INSERT INTO team_members ... ON CONFLICT DO NOTHING`；若冲突则回滚并把邀请置回 pending（等价于「已处理」），返回同一 404 |
| 成员移除后仍能读（缓存） | **不加任何成员权限缓存**。`RepoAccess` 每次请求现查（一条索引查询）。这是刻意的：缓存会让「移除成员」变成「等缓存过期才生效」，与安全红线的直觉冲突 |
| 取消共享与正在跑的任务 | 派发路径读 `repos` 按 `repo_id`，不受 `team_id` 影响；已存在的任务照旧跑完（属主自己的凭据、自己的仓库配置） |
| 多节点 | 无内存状态、无后台清扫器、无租约。所有判定都是库查询，天然多节点一致 |
| 删团队与共享仓库 | 有共享仓库时 `DELETE /api/teams/{id}` 返回 409（要求先取消共享，或移除共享了仓库的成员——移除会连带收回，§3.4.1），避免 `ON DELETE SET NULL` 把一批仓库在无人知情时变私有 |
| 删用户与团队 | `team_members` 走 `CASCADE`（成员关系随账号消失）；`team_invitations.invited_by/accepted_by` 走 `SET NULL`（邀请记录保留但失去指向）。但**若此人是某团队最后一个 owner**，管理员删账号会留下无主团队。处置：`AdminAPI.remove` 改调 `Store.DeleteUserGuarded`（§3.4.2 第 6 行：同一事务内按 id 序锁住此人所在团队 → 检查 → 删账号），检查不通过返回 409「该用户是团队 X 的 owner，先转让 owner」；此人是唯一成员的团队随账号一起删除（没有别人受影响，不拦）。`guardLastAdmin`（`admin.go:287`）仍在它之前；`AdminAPI.Store == nil` → 503 不回落（§3.4.2 第 6 行）。**离职 owner 的处置路径**见 §6 代价与 §9-D9：站内管理员**不能**管理团队（§4.1），所以 owner 不配合转让时账号删不掉，只能「停用」 |

### 3.10 UI

| 页面 | 内容 |
|---|---|
| `/teams`（新） | 我加入的团队卡片（名字、我的角色、成员数、共享仓库数）；可输入邀请码加入 |
| `/teams/:id`（新） | 成员表（邮箱/角色/加入时间）+ 角色操作（按矩阵显隐，越权按钮**不渲染**而不是点了报 404）；owner/admin 可见「生成邀请码」（明文只展示这一次，带复制按钮）与 pending 邀请列表（只显示 `hint/role/expiresAt`） |
| `/repos`（改） | 每条仓库增「共享」区块：属主看到「共享到团队」下拉；团队成员看到只读的「共享自 X 团队」徽标与**只读**配置视图；**配置编辑表单只对 `owned` 渲染** |
| `/`（看板，改） | 现有 `filter` 旁加一个「范围」切换：`我的`（默认，行为不变）/ `含团队`。切换即传 `scope=team` 到 `/api/tasks` |
| `/tasks/:id`（改） | 只加「创建者」标签（`viewerRole != owner` 时显示「队友创建」），以及按 `viewerRole` 决定的按钮显隐。**不改事件流、不改验证展示**（f04 的地盘） |
| `App.vue` 导航 | 加 `团队` 入口（`web/src/App.vue:50-62`）；`main.js` 加两条路由（`web/src/main.js:29-66`） |

前端只用既有依赖（vue / vue-router / marked / dompurify），**`web/package.json` 不动**。

### 3.11 与其他 11 条并行线的边界（重叠点逐个划清）

| 邻线 | 重叠点 | 本线怎么做 |
|---|---|---|
| f01 运行中插话 | 都改任务详情与「谁能操作任务」 | 插话是「属主可插话」。本线把「属主」的判定**保持为 `task.user_id`**，团队不扩展它。f01 若想在团队场景放开，需要自己的 PR 单独决定 |
| f02 收件箱 | 都按属主聚合待人处理事项 | 收件箱属主隔离不变。本线不派生团队待办；f02 将来若要加，调用本线的 `RepoAccess` 即可（§3.2） |
| f03 用量统计 | 「管理员看全局」 | f03 的「管理员」是 `users.role` 的站内管理员，本线**不碰**。团队内的统计视图不做 |
| f04 运行回放 | ① 都改 `TaskDetail.vue`；② **都改任务读接口的归属判定**：f04 把 `taskEvents` 里内联的 `tk.UserID != CurrentUser(r).ID`（`api.go:149-154`）收进 `a.ownedTask(w, r) (*task.Task, bool)`，它新增的 `events/summary` 与 `verifications/{vid}/log` 也走它（`f04-run-replay.md:193-195`、`:247`、`:534`、`:573`） | ① 前端：本线只加「创建者」标签与按钮显隐；事件流/验证展示/分页过滤全归 f04。② **归属 helper 统一用 f04 定的名字与签名 `ownedTask`，全站只有这一个任务读归属判定**（§3.3）。按合入顺序分工如下，**两种顺序下都由后合入方改，先合入方不预埋对方的东西**：**(a) f04 先合**：`ownedTask` 已存在且是属主语义，三个读接口都调它。f12 rebase 时**只改 `ownedTask` 的函数体**（属主 ∪ `RepoAccess(tk.RepoID, me)`），不在 `taskEvents` 里重新内联、不新起第二个 helper；summary 与日志因此自动获得团队可见性。f12 把 AC-9 的读路由表 `taskReadRoutes` 补上 f04 的两条（AC-54）。**(b) f12 先合**：`taskEvents` 里内联的判定已由 f12 抽成 `ownedTask`（团队可见语义，名字签名照 f04）。f04 rebase 时**复用这个已有的 `ownedTask`、不改它的函数体**，新接口直接调它，并把两条新路由补进 `taskReadRoutes`。两种顺序下 store 查询都传 **`tk.UserID`**（任务属主）而不是 `CurrentUser(r).ID`，`JOIN tasks … t.user_id = $2` 的第二道隔离照旧成立。**写接口与 `retryPlan` 不走 `ownedTask`**，继续内联属主判定。AC-54 的静态检查与读路由表在两种顺序下都成立。f04 PRD 引用的 `f12-team-roles.md:368` 是 v3 旧行号，对应内容现在在 §3.3「任务读接口的归属 helper」段 |
| f05 PAT | 都触碰「身份」 | PAT 的权限 = 该用户本人。团队可见性对 PAT 自动生效（同一 `CurrentUser`）。本线不改 `Auth.resolve` |
| f06 多 CLI | `RepoRow`/`ListRepos`/`GetRepo` 加 `agentDriver`（`f06-multi-cli.md:144`、`:461`） | 不动 runner 的事件模型。f12 **不改** `ListRepos`/`GetRepo`/`RepoRow`；团队视图经 `ListReposForViewer`/`GetRepoForViewer` → 调原函数 **`ListRepos`** 取行（不走 `GetRepo`，它缺 `verify_infra`，§2.1/§3.3），f06 加到 `ListRepos` 的列**自动出现**在团队视图里（AC-52）。先合入者不依赖后合入者：f06 先合，f12 rebase 时不需要改；f12 先合，f06 也不需要改 f12 的函数。`agentDriver` 归入「仓库配置」，按 §3.6 只有属主能改，与 `f06-multi-cli.md:572` 的口径一致 |
| f07 GitLab/Gitea | 都改 `Repos.vue` 与 `UpdateRepo`/`RepoRow` | 本线**不往 `RepoRow` 加字段**：`owned`/`teamId` 在新类型 `ViewerRepo`（内嵌 `RepoRow`）上，且只出现在 `scope=team` 的响应里；`UpdateRepo`、`ListRepos`、`GetRepo`、`CreateRepo` 一个字节不改，所以 f07 对这四个函数的改动与 f12 没有文本冲突，`vcsProvider`/`vcsBaseURL` 只要加进了 `ListRepos` 就会自动进团队视图（AC-52）。`Repos.vue` 两边都改，属于相邻行冲突，后合入者解决 |
| f08 云效 tracker | 无 | 不动 `internal/tracker` |
| f09 通知 | 邀请邮件 / 成员变更通知 | **不做**。出站通知整体归 f09 |
| f10 定时建单 | 「指派给自己」 | 建任务仍归属创建者。定时规则属主隔离不变 |
| f11 团队自定义技能 | ① **都叫「团队」**；② f11 写了「`GET /api/repos/{id}/team-skills` 的 404 谓词改为团队成员，是 f12 的 PR 要做的改动」（`f11-team-skills.md:845-846`、R6 `:1219`） | ① 本线建的是**成员与可见性**的团队（`teams` 表）；f11 的「团队技能」是「仓库级自定义技能」的命名，两者不共表（§9-D1）。② **不接这个承诺（v5）：f12 本版不改 f11 的任何查询、谓词或 handler，团队可见性不覆盖团队技能**——`/api/repos/{id}/team-skills*` 与 `/api/tasks/{id}/team-skills` 在两种合入顺序下都保持「仓库属主 / 任务属主」，团队成员对它们 404（方向是更严，不是泄露）。理由：f11 的读写端点共用 `GetRepo(id, userID)` 定位父行（`f11-team-skills.md:322`、`:715`），只放开读谓词要逐个端点区分读写并补一套团队用例，已超出本线「仓库 + 任务可见性」的范围；按「先合入者不依赖后合入者」原则，留给 f11 或后续 PR 调用本线的 `RepoAccess`（§3.2）单独做。**前端接缝（后合入方负责）**：`Repos.vue` 的团队技能分区只对 `owned !== false` 的行渲染（不带 `scope=team` 时没有 `owned` 键，等价于自己的仓库）；`TaskDetail.vue` 里 f11 的「本次任务钉住的技能」一行在 404 时不渲染（队友查看他人任务的情形）。f11 PRD 的 §4 那句与 R6 需同步改成「f12 本版不改，团队可见性不覆盖团队技能」 |
| **任何给 `ListTasks` 加列的邻线**（f02/f03/f04 等若扩展任务列表） | `ListTasks` 的团队视图 SQL（`listTasksTeamSQL` 常量）复制了一份列清单与 `Scan`（§3.3） | 后合入方负责：若邻线先合并且往默认 SQL 加了列，f12 rebase 时同步到团队 SQL；若 f12 先合，邻线加列时同步改 `listTasksTeamSQL`。AC-53 在两种顺序下都会抓住漏改（团队视图与默认视图同一行 `DeepEqual`） |

---

## 4. 范围外（明确不做）

### 4.1 不合并两套「角色」

`users.role`（站内 `admin`/`member`）与 `team_members.role`（团队内 `owner`/`admin`/`member`）
**是两套独立概念**，命名上刻意撞车是历史原因。文档里必须写一句
「站内管理员不会自动成为任何团队的成员，团队 owner 也不是站内管理员」。
**不做**：站内管理员代管任意团队、团队 owner 获得系统设置权限。

### 4.2 不做的事（逐条）

| 不做 | 理由 |
|---|---|
| **凭据共享 / 团队级凭据** | 需求明确「默认不共享个人凭据」；反向的共享开关一旦存在，必然产生「谁的 token 在驱动谁的任务」这个不可审计的洞。**这是本 PRD 最重要的范围外** |
| **团队 `admin` 改仓库配置**（v1 曾打算做） | §3.6.1：会拆人工闸门、削验证、改受保护分支、改 `baseline_dir`，并用属主凭据执行 |
| **两个成员共享同一个 issue key 的去重** | §1.1：`repos_user_repo_unique` 保留、活跃任务唯一索引按 `repo_id`、`resolveRepo` 只取属主第一条仓库。同一 provider 仓库、同一 issue 仍可能被两个成员各跑一遍。**存量债务**，根治要重做「仓库配置的归属」与派发路由 |
| **成员在共享仓库建任务** | §1.1/§3.6.4：建单入口只认属主，没有可达路径 |
| **任务转让 / 把任务从 A 名下改到 B** | 任务归属绑定执行凭据与工作区，转让 = 换身份执行 |
| **团队成员对他人任务的写操作**（重试/取消/批准/插话） | §3.6.2 |
| **团队级 webhook**（一个团队一个 Linear 回调） | 回调按 `users.webhook_slug` 路由到具体人，团队化要重做路由与验签 |
| **团队内 PRD / 流程 / 工单的可见性** | 本线只放开**仓库与任务**。PRD（`prds.user_id`）、编排图（`flows.user_id`）、工单（`issues.user_id`）仍然严格属主隔离 |
| **团队用量 / 团队成本视图** | f03 的地盘 |
| **邀请邮件、Slack/飞书通知** | f09 的地盘 |
| **邮箱验证** | 全站性产品决策（§3.1 取舍 2）。本线的邀请设计**不依赖**邮箱是身份证明 |
| **SSO / 组织目录同步 / 团队嵌套** | 超出「引入团队 + 角色矩阵」的最小范围；团队是**扁平**的 |
| **审计页 / 操作日志界面** | 本线只 `slog.Info` 落日志 |
| **修改「非属主 404」规则本身** | §3.5：不需要，且默认更严（全 404） |
| **修改任务状态机** | §3.6.2：不需要。`internal/task/state.go` 一字不改 |
| **削弱验证体系** | 团队可见性不改变红绿判定、不改变验证落库、不改变档位路由；配置写权限只在属主手里 |

### 4.3 与其他线的实现隔离

不改 `internal/runner/`、`internal/integration/`、`internal/tracker/`、`skills/`、
`internal/preview/`、`internal/prd/`、`web/package.json`、`go.mod`。

---

## 5. 验收标准（AC）

编号规则：`AC-<n>`。每条给出**可执行命令或测试名 + 期望输出**。

**§5.1–§5.5 是新行为组**（必须先红后绿，§5.8 给出每条真实的红灯信号；其中 AC-11/13/19/44 虽放在对应主题小节里，性质是守卫，§5.8 已逐条标明）；
**§5.6 是回归守卫组**（实现前后都绿，红不起来才是对的——它们的价值是「别改坏」）。
v1 把两组混在一起要求「全部先红」，其中 AC-5/20b/31b 本来就不可能红（评审第 9 条）。

**测试环境事实**（评审第 8 条）：本线宿主**没有 psql**（`command -v psql` 为空），
所有库断言走 **Go 测试**（`Store.Pool()`）或
`docker exec lathe-postgres-dev psql -U lathe -d lathe_f12`（注意库名必须是 `lathe_f12`；
`scripts/clean-test-db.sh` 的 docker 回退分支写死了 `-d lathe`，修好并验证之前**不得运行该脚本**，见 §7 与 AC-43）。

### 5.1 迁移（新行为）

| # | 断言 | 验证方式 |
|---|---|---|
| AC-1 | `0065/0066/0067` 可连续应用，`schema_migrations` 记到 67 | `TestMigrationsApplyTeamSchema`：`lathe_migrate up` 后经 `Store.Pool()` 查 `SELECT count(*) FROM schema_migrations WHERE version IN (65,66,67)`，期望 **3**（只断言三条存在，不断言它们是最高三条：后续线合入或重编号后仍成立；重编号时把三个号同步改成新号，v5 评审 nit 5） |
| AC-2 | 表与列确实建出来，且形态正确 | 同测试：`to_regclass` 存在 `teams/team_members/team_invitations`；`repos.team_id` 存在、`is_nullable='YES'`、无默认；`tasks_user_updated`/`tasks_repo_updated` 存在且 `indisvalid` |
| AC-3 | 三条 down 全部真正执行过、成对可逆且可重跑（评审 B1：`MigrateDown` 一次只回滚最高一条，`migrate.go:99-130`） | 脚本化，日志存 `/opt/lathe-wt/evidence/f12/migrate-roundtrip.log`：`for round in 1 2; do lathe_migrate down && lathe_migrate down && lathe_migrate down && <断言A> && lathe_migrate up && lathe_migrate up && lathe_migrate up && <断言B>; done`，每步退出码 0。**断言A**（三次 down 之后）：`SELECT max(version) FROM schema_migrations` = **31**（dev 基线当前实际最大号，不是 64）；`to_regclass('teams')`、`to_regclass('team_members')`、`to_regclass('team_invitations')` 全为 NULL；`SELECT count(*) FROM information_schema.columns WHERE table_name='repos' AND column_name='team_id'` = 0；`tasks_user_updated`/`tasks_repo_updated` 在 `pg_class` 中不存在。**断言B**（三次 up 之后）：与 AC-1/AC-2 相同（`65/66/67` 三条都在 `schema_migrations`、三表与列存在、两条索引 `indisvalid`）。断言经 `docker exec lathe-postgres-dev psql -U lathe -d lathe_f12 -Atc …` 执行（宿主无 psql）。若 dev 合入其他迁移使基线最大号变化，断言A 的 31 改为「本线第一条迁移的前一个已应用号」，PR 描述里写明实际值 |
| AC-3b | `0067` 的 INVALID 索引自检会响亮失败 | `TestMigration0067ValidityCheck`：从 `migrations.FS` 读出 `0067` up 脚本末尾的 `DO` 块原文；在测试事务里先去掉 `tasks_repo_updated` 这条索引，执行该 `DO` 块，断言报错且错误文本含 `INVALID`；回滚后在两条索引都在的状态下执行同一 `DO` 块，断言无错。（CONCURRENTLY 中途失败留下的 INVALID 索引无法在测试里稳定造出，「索引缺失」与「索引无效」走的是同一个 `count <> 2` 分支。）**新行为**：DO 块加入前此测试 `got nil want error` |
| AC-4 | 迁移里不含业务 DML | `grep -iE "insert into|update .* set" migrations/00{65,66,67}*.up.sql` 无匹配（`DEFAULT` 不算 DML） |
| AC-5 | 存量仓库迁移后仍是私有 | `TestExistingReposStayPrivateAfterMigration`：先 `CreateRepo` 再断言 `SELECT count(*) FROM repos WHERE team_id IS NOT NULL` = 0（用 `Pool()`，不用 psql） |

### 5.2 隔离与可见性（新行为）

| # | 断言 | 测试名 | 期望 |
|---|---|---|---|
| AC-6 | 非成员对**已共享**团队仓库的任务仍然 404 | `TestTeamSharedRepoInvisibleToNonMember` | `GET /api/tasks/{id}` → 404；`GET /api/tasks/{id}/events` → 404；`GET /api/teams/{teamID}` → 404 |
| AC-7 | 被移除的成员立刻失去可见性（无缓存窗口） | `TestTeamMemberRemovalTakesEffectImmediately` | 移除前 200；移除后同一请求 404 |
| AC-8 | 取消共享立刻收回可见性 | `TestRepoUnshareRevokesVisibilityImmediately` | 取消前 200，取消后 404 |
| AC-9 | 成员能看到共享仓库下**他人创建**的任务详情与事件流 | `TestTeamMemberReadsOthersTask` | `GET /api/tasks/{id}` → 200 且 `task.userId == 创建者`、`task.repoId` = 该仓库、`viewerRole` = 成员角色；`GET /api/tasks/{id}/events?after=0` → 200。读路由取自 AC-54 的 `taskReadRoutes` 表，不在测试里另写一份 |
| AC-10 | 列表 `scope=team` 才含团队任务；默认不含，且 `total` 与列表一致 | `TestListTasksScopeTeam` | 不带 `scope` → `total` 只算自己的；带 `scope=team` → `total` 含团队任务且有 `repoId`；两次响应里自己的任务都在 |
| AC-54 | **每一个**任务读接口都经 `ownedTask` 获得团队可见性，且全站只有一个任务读归属 helper（评审第四轮 B2：f04/f12 任一合入顺序下都不回退、不漏一半） | `TestTeamMemberReadsEveryTaskReadRoute` + 两条静态检查 | ① `internal/httpapi/api_test.go` 维护显式表 `taskReadRoutes`（本线落地时两行：`GET /api/tasks/{id}`、`GET /api/tasks/{id}/events`；f04 合入后由后合入方补 `GET /api/tasks/{id}/events/summary`、`GET /api/tasks/{id}/verifications/{vid}/log`），对每一行断言：团队成员读他人任务 → 200，非成员 → 404 且 body 与「任务不存在」字节相同。② `grep -c '^func (a \*API) ownedTask(' internal/httpapi/*.go` 合计 **1**（不出现第二个 helper）。③ `taskEvents` 函数体（`awk '/^func \(a \*API\) taskEvents\(/,/^}/'`）含 `a.ownedTask(`、**不含** `tk.UserID != CurrentUser(r).ID`。**新行为**：空壳 `ownedTask` 只认属主 → 成员 `got 404 want 200`；f04 先合而 f12 rebase 时在 `taskEvents` 重新内联 → ③ 失败；f12 先合而 f04 rebase 时照搬属主判定改写 `ownedTask` → ① 的 events 行 `got 404 want 200`（AC-9 回退被抓住） |
| AC-53 | `ListTasks` 团队视图的列清单副本不漂移（v5 评审 nit 4） | `TestListTasksTeamViewRowsMatchDefaultView` | viewer 自己的 3 条任务（字段尽量填满：`agent_cost_usd`、`pr_url`、内置工单 `issue_id` 等非零值）：`TeamView=true` 结果里这 3 行，把 `RepoID` 置 0 后与 `TeamView=false` 的同 ID 行 `reflect.DeepEqual`；失败信息打印差异字段名。默认视图的 JSON 里**没有** `repoId` 键（`omitempty`，响应与今天逐字节相同）。守的是「邻线往默认 SQL 加列、忘了团队 SQL」 |

### 5.3 写权限（新行为）

| # | 断言 | 测试名 | 期望 |
|---|---|---|---|
| AC-11 | 成员仍不能对他人任务发起任何写操作，**全部 404** | `TestTeamMemberCannotMutateOthersTask` | `POST /api/tasks/{id}/retry`（body **`{"mode":"auto"}`**）、`/cancel`、`/approve`、`/review-retry`（后三者 body `{}`）全部 404，且任务状态在库里不变。**请求体必须合法**：`retryTask` 先解码 body、校验 `mode`，之后才判归属（`internal/httpapi/api.go:306-325`），空 body 或缺 `mode` 拿到的是 400 而不是 404，测的就不是归属了；同测试加一条对照：属主用同一 body 调 `retry` 不是 400 |
| AC-12 | 角色矩阵一张表测完 | `TestTeamRoleMatrix`（表驱动，`t.Run` 每条「调用者角色 × 动作 × 目标角色 → 断言状态码」） | 覆盖 §3.6 主矩阵每一行与「成员管理逐格判定表」的**每一格**（3 调用者 × 3 动作 × 4 目标，`—` 格跳过），期望码逐格照抄该表（200 / 404 / 409），另加 `PATCH {role:"owner"}` → 400 与非成员调用任一行 → 404；失败信息带 `caller/action/target got/want` |
| AC-13 | `member` / `admin` **都不能**改共享仓库配置，字段一个不变 | `TestTeamCannotUpdateRepoConfig` | 两个角色 `PUT /api/repos/{id}`（body 把六个字段都改成与现值不同的合法值，`verifyInfra` 用 `["postgres"]`，fixture 预置为 `["redis"]`）→ 404；随后**经 `Store.Pool()` 直接查** `SELECT gate_mode, verify_tier_override, protected_branches, baseline_dir, verify_infra, default_branch FROM repos WHERE id=$1`，与改前逐字段相等（**这条正面挡住评审第 3 条**）。不用 `GetRepo` 取值：它的 SELECT 不含 `verify_infra`（§2.1），比较的永远是 `nil == nil`，这个字段被改了也测不出来（v5 评审 B1-b）。fixture 预置非空 `verify_infra` 正是为了让「被改」可观测 |
| AC-14 | 团队 `admin` 改不了团队级 owner 动作 | `TestTeamAdminCannotOwn` | `PATCH /api/teams/{id}`、`DELETE /api/teams/{id}`、`POST /api/teams/{id}/transfer` → 均 404；团队名与成员表不变 |
| AC-15 | `admin` 不能动同级 `admin` 或 `owner`，也不能改任何人的角色 | `TestTeamAdminCannotTouchPeers` | admin 对 owner/另一 admin 的 `PATCH`、`DELETE` → 404；admin 把 member `PATCH {role:"admin"}` → 404；库里角色不变 |
| AC-15b | 成员可以退出 | `TestTeamMemberCanLeave` | member、admin 各自 `DELETE /api/teams/{id}/members/{自己的 uid}` → 200，随后 `GET /api/teams/{id}` → 404、共享仓库任务 → 404。空壳下 `got 404 want 200` |
| AC-16 | owner 不能降自己、不能退出 | `TestTeamLastOwnerGuard` | owner `PATCH …/members/{自己} {role:"admin"}` 与 `{role:"member"}` → 409；owner `DELETE …/members/{自己}` → 409；三条后库里 owner 数仍为 1、仍是此人 |
| AC-17 | 转让 owner 单事务：成功时 owner 恰好一个 | `TestTeamTransferOwner` | 转让后旧 owner 是 `admin`、新 owner 是 `owner`、`count(*) where role='owner'` = 1 |
| AC-18 | 只有仓库属主能共享/取消共享；属主是目标团队任一角色的成员即可共享 | `TestRepoSharingOnlyByOwner` | 团队 owner/admin 去共享别人的仓库 → 404，`repos.team_id` 不变；**member 角色**的属主共享自己的仓库到本团队 → 200（空壳下 `got 404 want 200`） |
| AC-45 | 不能把仓库共享进自己不在的团队，且团队存在与否不可区分（评审 B2a/b） | `TestRepoShareRequiresTeamMembership` | 属主甲对乙的团队 T（甲非成员）`POST /api/repos/{id}/sharing {teamId:T}` 与对 `teamId = max(teams.id)+1000`（不存在）各发一次：两次都 404，**响应体字节完全相同**（`bytes.Equal`），`Content-Type` 相同；随后 `repos.team_id` 仍为 NULL，T 的成员 `GET /api/tasks?scope=team` 不含甲的任务。红信号：只校验 `GetRepo` 的实现下前者 `got 200 want 404`、后者是外键错误 `got 500 want 404` |
| AC-46 | 移除成员时同一事务内收回其共享（评审 B2 第三点） | `TestTeamMemberRemovalUnsharesTheirRepos` | 成员乙把自己的仓库 R 共享到 T，丙（T 成员）可读 R 的任务 → 200；owner 移除乙后：`repos.team_id` 为 NULL、丙读同一任务 → 404、乙自己仍可读（属主）；T 中其他人共享的仓库 `team_id` 不变。红信号：不收回时 `team_id got T want NULL`、丙 `got 200 want 404` |
| AC-47 | 退出团队同样收回 | `TestTeamMemberLeaveUnsharesTheirRepos` | 同 AC-46，但乙自己 `DELETE …/members/{乙}`；断言相同 |
| AC-48 | 共享与移除并发时不留下「非成员的仓库仍共享」，**且不出现 500/死锁** | `TestTeamShareRemoveRace` | 同一对（乙、T）上 50 轮，每轮先把乙恢复为成员、仓库恢复为私有：goroutine A 经 HTTP 共享乙的仓库到 T，goroutine B 经 HTTP 移除乙，两者用 `sync.WaitGroup` 同时放行。每轮断言：① A 的状态码 ∈ {200, 404}，B 的状态码 = 200，**没有 500，也没有 503**；② 库里**不存在** `repos.team_id = T AND user_id = 乙 AND NOT EXISTS(乙 ∈ T)` 的行；③ 50 轮结束后 `store.TeamTxRetries()` 的增量 = 0（锁序不成环，所以连重试都不该被用到；这条断言让「死锁被重试悄悄吃掉」也算失败）。`-race` 下跑（AC-38 的 `-run` 已含 `TestTeam` 前缀） |
| AC-49 | §3.4.2 的每一对事务在**确定性交错**下都不死锁（不靠运气撞并发） | `TestTeamTxLockOrderNoDeadlock`（`internal/store`，表驱动） | 每个用例开两个 `pgx.Tx`：tx1 执行到「持有第一把锁」后挂起；tx2 在另一个 goroutine 里执行到被阻塞（用 `pg_stat_activity.wait_event_type = 'Lock'` 轮询确认它已经阻塞，不 sleep）；放行 tx1 提交，等 tx2 结束。用例覆盖证据文件里的 F-1 至 F-9（共享×移除双向、共享×PATCH、共享×转让、删团队×共享、接受×撤销、删账号×共享双向、移除×删账号）。断言：两个 tx 都没有返回 SQLSTATE `40P01`；终态与 §3.4.2 表后的预期一致。红信号：把共享事务改回 v3 顺序（去掉 3a），F-1/F-3/F-4 报 `got 40P01 want nil`；把接受邀请的 `teams FOR KEY SHARE` 去掉，F-6 报 40P01 |
| AC-50 | 40P01/40001 会被重试，耗尽后返回 503（不是 500） | `TestTeamTxRetriesThenBusy`（`internal/store`）+ `TestTeamBusyMapsTo503`（`internal/httpapi`） | store 侧：给 `teamTx` 传入一个前两次返回 `&pgconn.PgError{Code:"40P01"}`、第三次成功的 fn → 返回 nil，`TeamTxRetries` +2；fn 每次都返回 40P01 → 返回 `errors.Is(err, ErrTeamBusy)`，fn 恰好被调 3 次；fn 返回别的错误 → 原样返回，只调 1 次。httpapi 侧：store 返回 `ErrTeamBusy` 时响应 `503`，body 为 `{"error":"团队数据正忙，请重试"}`。红信号：空壳 `teamTx` 不重试 → `calls got 1 want 3`；handler 走 `serverError` → `got 500 want 503` |
| AC-19 | 越权响应不泄露存在性：同一个人对同一个任务，**任何写接口的状态码一致** | `TestTeamAllDenyPathsAre404` | 成员对他人任务遍历 `retry`（body `{"mode":"auto"}`）/`cancel`/`approve`/`review-retry`/`preview/start`/`preview/stop`/`preview/recommend`（其余 body `{}`），全部 404 且响应体 `error` 完全相同。请求体取值理由同 AC-11：body 不合法时 `retry` 在归属判定之前返回 400 |
| AC-20 | 权限矩阵无遗漏：每个写路由都有一条拒绝用例 | `TestTeamEveryWriteRouteHasDenyCase` | 遍历 `teams.go` 导出的 `teamRoutes`（§3.4），对每个 `Write: true` 的路由断言 `denyCases` 里有用例，且该用例实际返回 404。**这条防的是将来新增接口漏判**（评审第 5 条：不依赖 mux 枚举） |

### 5.4 邀请（新行为）

| # | 断言 | 测试名 | 期望 |
|---|---|---|---|
| AC-21 | **知道被邀请人的邮箱不足以加入**（评审第 1 条的核心） | `TestTeamEmailAloneCannotJoin` | 甲建团队、对 `victim@example.com` 发起邀请（拿到 `hint` 但无明文码）；乙**抢注 `victim@example.com` 账号**后：`GET /api/me` 响应里没有任何邀请相关字段、`GET /api/invitations` 路由不存在（404）；`POST /api/team-invitations/accept` 带空码/错码 → 404；乙对团队详情与团队仓库任务仍 404。（v1 设计下这条会红：乙能在待办里看到并接受邀请） |
| AC-22 | 只有持有一次性码的人能加入，码单次生效 | `TestTeamInviteCodeSingleUse` | 正确码 accept → 200/204 成为成员；第二次 accept（任意账号、含同一账号）→ 404；接受后 `team_invitations.state` = `accepted` 且 `accepted_by` 为乙 |
| AC-23 | 创建邀请不泄露账号是否存在 | `TestTeamInviteDoesNotLeakAccountExistence` | 对「已注册邮箱」「未注册邮箱」「空邀请人信息」三种输入，响应状态码与 body 字段形态一致（除 `code/hint/expiresAt` 随机值）；`CreateInvitation` 的 store 函数签名不接收邮箱以外的用户标识，测试断言两次调用前后 `users` 行数不变 |
| AC-24 | 码过期 / 撤销后不能接受 | `TestTeamInviteExpiredOrRevokedRejected` | 过期（把 `expires_at` 改到过去）与撤销后 accept → 均 404 |
| AC-25 | 接受后获得可见性；已是成员时幂等 | `TestTeamInvitationAcceptGrantsVisibility` | 接受前 `GET /api/tasks/{id}`（共享仓库任务）→ 404，接受后 → 200；已是成员再用另一张有效码 accept → 404 且该码仍为 `pending`（未被消耗） |
| AC-26 | 猜码被限流 | `TestTeamAcceptRateLimited` | `TeamAPI` 私有 limiter 实例（不与 `AccountAPI`/`Auth` 共享），key `invaccept:<ip>`，阈值 **10 次 / 15 分钟**（§3.1 取舍 2）：同 IP 前 10 次错码 → 404，第 11 次 → 429，第 12 次用**正确码** → 仍 429 且码仍 `pending`；另一 IP 正确码 → 200。**时间注入方式写死**（v5 评审 nit 6）：`TeamAPI` 带一个包内可见字段 `now func() time.Time`（nil 时用 `time.Now`），accept handler 调 `a.lim().allowAt(key, inviteAcceptLimit, inviteAcceptWindow, a.clock())`（`limiter.allowAt` 已存在，`internal/httpapi/limiter.go:29-30`）；测试把 `api.now` 设成可前进的假时钟，窗口过后（+15m1s）同 IP 正确码 → 200，不 sleep。这个字段不导出、不进 `cmd/lathe` 装配，生产永远是 `time.Now` |

### 5.5 存储层与并发（新行为）

| # | 断言 | 测试名 | 期望 |
|---|---|---|---|
| AC-27 | `RepoAccess` 四种角色的判定 | `TestRepoAccessMatrix` | 属主 / 团队 owner / admin / member / 非成员 分别得到对应结果或 `ErrRepoNotFound` |
| AC-28 | `scope=team` 的列表**用索引缩小扫描范围**，而不只是「计划里出现了索引名」（评审 B4：OR-IN 写法能出现索引名却是整条索引全扫 + Filter） | `TestListTasksScopeTeamUsesIndex` | **数据**：20 个用户各 1 个仓库 × 2000 条任务（`tasks` 共 40 000 行，在测试事务里 `INSERT … SELECT generate_series` 造，结束回滚）；viewer 与另一用户同在团队 T、后者仓库共享到 T，结果集上界 = 2 × 2000，取 `limit=50`。`ANALYZE tasks` 后，**不**设 `enable_seqscan`，对 §3.3 的 `scope=team` SQL 常量（与 `ListTasks` 同一常量）跑 `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)`，解析 JSON 断言：① 存在 `Index Name = tasks_user_updated` 的节点，其 `Index Cond` 含 `user_id`；② 存在 `Index Name = tasks_repo_updated` 的节点，其 `Index Cond` 含 `repo_id`；③ 计划中**没有** `Relation Name = tasks` 的 `Seq Scan` 节点；④ 所有 `Relation Name = tasks` 的节点 `Actual Rows` 之和 ≤ 2 × 2000 + 50（即没有读到结果集之外的用户的行；对照：OR-IN 写法 `Rows Removed by Filter: 36000`）；⑤ 顶层 `Shared Hit Blocks + Shared Read Blocks` ≤ 200（实测 UNION ALL 52、OR-IN 618，见 §10.2 B4；阈值留约 4 倍余量）。失败信息打印整份文本计划 |
| AC-29 | 并发转让后 owner 恰好一个 | `TestTeamLastOwnerGuardUnderConcurrency` | owner 同时发两个 `POST /api/teams/{id}/transfer`（目标分别为 admin 甲、member 乙）：恰好一个 200、另一个 404（锁内重读发现调用者已不是 owner）；最终 `count(*) WHERE role='owner'` = 1，且原 owner 是 `admin`。去掉 `FOR UPDATE` 时两个都 200、owner 数 = 2（红）。**必须在 `-race` 下跑**（见 AC-38） |
| AC-30 | 删用户时 owner 检查生效（v5 细化，§3.9） | `TestAdminDeleteLastTeamOwnerBlocked`（表驱动） | ① 此人是**有其他成员**的团队的 owner → `DELETE /api/admin/users/{id}` 409，账号仍在，团队 owner 数仍为 1；② 此人是某团队的**唯一成员**（也就是 owner）→ 200，账号与该团队**同一事务内**一起删除（`to_regclass` 不适用，查 `SELECT count(*) FROM teams WHERE id=$T` = 0）；③ 此人只是 member/admin → 200，团队仍在、owner 不变 |
| AC-30b | `AdminAPI.Store` 为 nil 时删账号 fail-closed（评审第四轮 nit 7） | `TestAdminRemoveWithoutStoreFailsClosed` | 用不带 `Store` 的 `AdminAPI`（与 `routes_auth_test.go:19` 同形）以管理员身份 `DELETE /api/admin/users/{id}` → **503** `{"error":"团队守卫未装配，拒绝删除账号"}`，账号仍在。**不回落到 `Users.Delete`**：回落就等于绕过「最后一个 owner」守卫。红信号：直接调 `a.Store.DeleteUserGuarded` 时 nil 指针 panic（`httptest` 下表现为连接被重置/500） |

### 5.6 回归守卫（实现前后都绿）

| # | 断言 | 测试名 | 期望 |
|---|---|---|---|
| AC-31 | 非成员对**私有**仓库 + 其任务：列表不含、详情 404、事件 404 | `TestTeamNonMemberStillGets404OnPrivateRepo` | 三条均维持今天的行为 |
| AC-32 | 预览接口仍只认任务属主（团队可见性**不**扩展到预览） | `TestTeamMemberCannotStartPreviewOnOthersTask` | 成员对他人任务 `POST /api/tasks/{id}/preview/start`/`stop`/`recommend` → 404，与本功能前一致 |
| AC-33 | 既有任务接口与用户间隔离未被改坏 | `go test ./internal/httpapi -run 'TestAPIListAndDetail\|TestAPIIsolationBetweenUsers\|TestAPIConfigNeverLeaksSecrets'` | 全过（`TaskRow` 加 `repoId`、新增 `TaskDetailForViewer` 后仍绿）；另 `git diff origin/dev...HEAD -- internal/store/query.go \| grep -E '^[-+].*func \(s \*Store\) TaskDetail\('` **无输出**（`TaskDetail` 签名未动，评审 B3；f03/f11 沿用它） |
| AC-51 | `ListRepos` 签名与函数体未动；它的两个既有调用点仍只看属主（评审 v3-3） | ① `git diff -U0 origin/dev...HEAD -- internal/store/query.go \| grep -E '^[-+].*func \(s \*Store\) ListRepos\('` **无输出**；② 函数体逐字节不变：`f(){ awk '/^func \(s \*Store\) ListRepos\(/,/^}/'; }; diff <(git show $(git merge-base origin/dev HEAD):internal/store/query.go \| f) <(f < internal/store/query.go)` **无输出**（与 merge-base 比，所以 f06/f07 先合入、f12 rebase 后，它们对这个函数的改动算基线，不算 f12 的改动；本线实测基线版本 25 行，脚本可用）；③ `TestListReposStaysOwnerOnly`：乙把仓库共享到甲所在的团队后，`Store.ListRepos(ctx, 甲)` 只返回甲自己的仓库；不带 `scope` 的 `GET /api/repos` 响应 JSON 里**没有** `owned`/`teamId` 键；乙的仓库作为团队任务的 `repo` 时，`previewRetry` 用的 `ListRepos(tk.UserID)` 结果不变 | 守卫：①②实现前后都是空/0；③ 实现前后都绿 |
| AC-52 | 团队视图的仓库行**自动带上邻居新增的列**（f06/f07 与 f12 无论谁先合入都成立） | `TestListReposForViewerMatchesListRepos` | 基准一律是 **`ListRepos(该仓库属主)` 里 ID 相同的那一行**（v5：不再用 `GetRepo` 作基准——它不 SELECT `verify_infra`，`VerifyInfra` 恒为 nil，而 `ListRepos` 对 `'{}'` 给的是非 nil 空切片，属主行与 `*GetRepo(...)` 的 `DeepEqual` 必然为 false，§2.1）。① 甲自己的行：`reflect.DeepEqual(row.RepoRow, ListRepos(甲) 中同 ID 行)`，`Owned=true`；② 团队臂的行：`reflect.DeepEqual(row.RepoRow, ListRepos(乙) 中同 ID 行)`（乙 = 属主，ID 在测试里用 `Pool()` 查），`Owned=false`、`TeamID=T`；③ **`verify_infra` 显式断言**：fixture 让乙的共享仓库 `verify_infra = '{redis}'`，团队臂这一行的 `VerifyInfra` 等于 `[]string{"redis"}`（不是 nil）；`GetRepoForViewer(该仓库, 甲)` 返回的 `RepoRow` 与 ② 的基准 `DeepEqual`。这样无论 `RepoRow` 将来多了哪些字段，只要 `ListRepos` 填了，团队视图就一定填。**新行为**：空壳 `ListReposForViewer` 只返回 `ListRepos(甲)` → 团队行缺失，`got 1 rows want 2`；若实现走 `GetRepo` 取团队行（v4 的写法），③ `VerifyInfra got nil want [redis]` |
| AC-34 | 非属主改仓库配置仍 404（含任何角色） | `TestRepoUpdateStillOwnerOnly` | 非属主 `PUT /api/repos/{id}` → 404，配置不变 |
| AC-35 | 状态机未改 | `git diff origin/dev...HEAD -- internal/task/state.go` | 空输出 |
| AC-36 | 验证体系未削弱 | `git diff origin/dev...HEAD -- internal/runner/verify.go internal/runner/heavy.go internal/store/verifications.go` | 空输出 |
| AC-37 | 历史迁移未改；`.claude/`、`workspaces/`、`skills/` 未碰 | `git diff --name-status origin/dev...HEAD -- migrations/ .claude/ workspaces/ skills/` | 恰好 6 行，每行匹配 `^A\tmigrations/006[5-7]_[a-z_]+\.(up\|down)\.sql$`；机器判定：`… \| grep -vcE '^A\tmigrations/006[5-7]_[a-z_]+\.(up\|down)\.sql$'` 输出 `0`，且 `… \| wc -l` 输出 `6` |

### 5.7 全局纪律与工具链

| # | 断言 | 验证方式 |
|---|---|---|
| AC-38 | 竞态通过，**且真的跑到了 `httpapi`/`store`** | 显式命令：`go test -p 1 -race -count=1 ./internal/httpapi/... ./internal/store/... -run 'TestTeam\|TestRepoAccess\|TestRepoUpdateStillOwnerOnly\|TestListReposForViewer\|TestRepoShare\|TestListTasksTeamView\|TestAdminDeleteLastTeamOwner\|TestAdminRemoveWithoutStore'`，退出码 0。**不依赖 `make test-race`**（`Makefile:58-63` 只覆盖 `cmd/lathe`、`task`、`runner`、`flow`；评审第 6 条）。可选地在 PR 里把这两个包加进 `test-race` 目标，但**不把它当作本 AC 的前提** |
| AC-39 | 全量测试与 lint 通过 | `make lint && make test`（在 `/opt/lathe-wt/f12-roles` 内，`source /opt/lathe-wt/env/f12.sh`），退出码 0；另对新文件单独补跑 `gofmt -l`（`make lint` 只扫 `git ls-files`，未入库文件会漏） |
| AC-40 | 不引入依赖 | `git diff origin/dev...HEAD -- go.mod go.sum web/package.json web/pnpm-lock.yaml` 期望**空输出** |
| AC-41 | 前端构建同步 | `make ui && make build` 成功；`internal/webui/dist` 内出现新 chunk；`.gitkeep` 仍在 |
| AC-42 | 凭据不出现在团队接口响应里 | `TestTeamAPINeverLeaksSecrets`：把 team/repo/task 三个响应体 grep 已配置的 token 明文，期望 0 匹配（照抄 `TestAPIConfigNeverLeaksSecrets` 的形状，`internal/httpapi/api_test.go:376`） |
| AC-43 | 测试库不残留孤儿团队，且**本线全程不执行真删**（评审 v3-2） | 分三步，前一步不过不做后一步。**①（docker 回退修复先落地、先验证）**`TestCleanTestDBTargetsDSNDatabase`（`internal/store`，Go 测试，不真删）：用 `exec.Command("sh", "scripts/clean-test-db.sh")`，**显式设 `Dir` 为仓库根，`Env` 白名单**只含 `PATH` 与 `LATHE_TEST_DSN`（AGENTS §9 / B2-3），干跑（不带 `--yes`）；断言输出第一节打印的 `current_database()` 等于 `LATHE_TEST_DSN` 里的库名（本线即 `lathe_f12`），并且退出码 0、stderr 里没有 `ERROR`。再用一个指向不存在库的 DSN（`…/lathe_f12_nonexistent_<rand>`）跑一次干跑，断言**非零退出**、输出不含 `current_database`，即连不上就失败，不会静默回落到 `lathe`。宿主既没有 `psql`，也没有在跑的 `lathe-postgres-dev` 容器时（CI 有可能这样），`t.Skipf` 并说明原因（AGENTS §5.2）。DSN 解析不出库名时脚本必须 `exit 1`（用例：`LATHE_TEST_DSN=postgres://h:1/` → 非零）。**②（删除 SQL 在回滚事务里验证）**`TestCleanTestDBDeleteSQLInRollback`：从 `scripts/clean-test-db.sh` 里按标记注释 `# lathe:delete-sql-begin` / `# lathe:delete-sql-end` 抽出 `--yes` 段的两条 DELETE 原文（测的就是脚本里的那两句）；在 `lathe_f12` 上 `BEGIN`，造 fixture（一个随机 `…@example.com` 用户建团队并且是唯一成员，另造一个随机非 example.com 用户的团队作对照），按脚本顺序执行两条 DELETE，断言 fixture 团队行已不存在、`SELECT count(*) FROM teams t WHERE NOT EXISTS (…)` = 0、对照团队仍在；最后 **`ROLLBACK`**，并断言回滚后 `SELECT count(*) FROM users WHERE email LIKE '%@example.com'` 与 `SELECT count(*) FROM teams` 都等于 `BEGIN` **之前**（在事务外、另一个连接上）取的值（库没被动过；fixture 是在事务内造的，回滚后本来就不存在，所以不拿「fixture 行」作比较对象，v5 评审 nit 6）。红信号：两句顺序颠倒 → `fixture team got present want deleted`；漏写别名 `t` → `missing FROM-clause entry`。**③（真删 `--yes` 不在本线验收内）**：任何一次真删 `scripts/clean-test-db.sh --yes`（含 `make clean-test-db YES=1`）都按 AGENTS §4 **必须先问人**，并且只能在 ① 已通过之后做；PR 描述里写明「本线未执行 `--yes`」。若人批准真删，只能在 `LATHE_TEST_DSN` 指向 `lathe_f12` 时跑，跑之前先看干跑输出第一行确认库名 |
| AC-44 | 团队名重复不再 409（评审第 11 条） | `TestTeamNameDuplicateAllowed`：两个用户建同名团队都 201；各自 `GET /api/teams/{id}` 只看到自己的，对方的 id → 404 |

> 表格里 `\|` 只是 Markdown 转义。实际执行的命令（Go 正则里 `\|` 是字面竖线，别照抄）：
>
> ```bash
> cd /opt/lathe-wt/f12-roles && source /opt/lathe-wt/env/f12.sh
> go test -p 1 -race -count=1 ./internal/httpapi/... ./internal/store/... -run 'TestTeam|TestRepoAccess|TestRepoUpdateStillOwnerOnly|TestListReposForViewer|TestRepoShare|TestListTasksTeamView|TestAdminDeleteLastTeamOwner|TestAdminRemoveWithoutStore'   # AC-38
> go test -p 1 -count=1 ./internal/httpapi -run 'TestAPIListAndDetail|TestAPIIsolationBetweenUsers|TestAPIConfigNeverLeaksSecrets'          # AC-33
> ```

### 5.8 先红后绿的落点（写进 PR 描述）

**做法**：测试与生产代码分两个提交。第一个提交只加测试 + 编译所需的**空壳**
（新函数/新路由存在但沿用今天的行为：`TaskDetailForViewer` 内部直接调 `TaskDetail`（属主版，原样不动）、
`teamRoutes` 里的 handler 一律 404、`0065–0067` 已建表与索引），在这个提交上跑出下表的
红信号并把日志存进 `/opt/lathe-wt/evidence/f12/red.log`；第二个提交填实现，同一命令转绿。
这样每条红都是**行为断言失败**，不是「编译不过」或「表不存在」。

| AC | 红阶段（空壳提交）的真实失败信号 |
|---|---|
| AC-1/2/3 | 迁移测试在**迁移文件加入前**的提交上：`count(version IN (65,66,67)) got 0 want 3`（AC-3 同理：迁移文件缺失时 `lathe_migrate down` 回滚的是 `0031`，断言B 失败）；`to_regclass('teams')` 为 NULL |
| AC-5 | 在**迁移文件加入前**的提交上 `repos.team_id` 不存在 → `column "team_id" does not exist`。这是 schema 缺失而非行为错（新行为里唯一的弱红），PR 里如实标注；空壳提交上它已是绿 |
| AC-6/9 | 空壳的 viewer 版等价于属主版：**成员**读他人任务 `got 404 want 200`（AC-9 红）；AC-6 的非成员 404 半边本就绿，红信号落在同测试里「成员应 200」的对照断言 |
| AC-7/8 | 「移除前/取消前应 200」的前半段 `got 404 want 200` |
| AC-10 | `scope=team` 被忽略：`total got N_self want N_self+N_team` |
| AC-11/13/19/34 | **不红，属回归守卫性质**：今天非属主写本就 404，空壳也是 404。归入 §5.6 的口径（实现前后都绿）；其价值是防止实现时把 `UpdateRepo` 或任务写接口放宽 |
| AC-12/14/15/15b | 授权侧红：空壳 handler 一律 404，**owner** 改团队名/转让/升 member 为 admin `got 404 want 200`；member 退出 `got 404 want 200` |
| AC-16/17/29 | 守卫未加：先让 owner 操作可用（第二提交的中间态），owner 降/退自己 `got 200 want 409`；并发转让去掉锁时两个都 200、owner 数 `got 2 want 1` |
| AC-45 | 先实现「只校验 `GetRepo`」的共享（第二提交中间态）：非成员团队 `got 200 want 404`；不存在团队 `got 500 want 404`（外键错误） |
| AC-46/47 | 先实现不带收回的移除/退出：`team_id got T want NULL`、剩余成员 `got 200 want 404` |
| AC-48 | 两条红：① 按 v3 锁序实现共享（不加 3a）→ 并发轮里出现 `share status got 500 want 200/404`（40P01，`teamTx` 未接入时直接 500）；② 接入 `teamTx` 但仍用 v3 锁序 → 状态码可能是绿的，但 `TeamTxRetries delta got >0 want 0`。不加任何锁时偶发残留行。若 50 轮都没撞到，如实标注「并发红未复现」，以 AC-49 的确定性红为准 |
| AC-49 | 确定性红：v3 锁序下 F-1/F-3/F-4 `got SQLSTATE 40P01 want nil`；接受邀请不先锁 teams 时 F-6 同样报错（与 `/opt/lathe-wt/evidence/f12/prd-v4-lock-order.log` 第一部分 V3-1 至 V3-4 一一对应） |
| AC-50 | 空壳 `teamTx` 只跑一次：`calls got 1 want 3`；handler 未映射：`got 500 want 503` |
| AC-51 | **不红，回归守卫**（实现前后 diff 都为空、`ListRepos` 行为不变）。价值：挡住把 `IncludeTeam` 塞回 `ListRepos` 的实现 |
| AC-52 | 空壳 `ListReposForViewer` = `ListRepos(viewer)`：`got 1 rows want 2`；团队臂走 `GetRepo`：`VerifyInfra got nil want [redis]`；若实现时自己复制了一份 SELECT 却漏掉列，`DeepEqual` 失败并打印差异字段 |
| AC-53 | 团队 SQL 与默认 SQL 列漂移：先在团队 SQL 的 SELECT 里故意漏掉 `t.agent_cost_usd`（中间态），`AgentCostUSD got <nil> want 0.42` |
| AC-54 | 空壳 `ownedTask` 只认属主：成员读 events `got 404 want 200`；③ 的静态检查在把判定内联回 `taskEvents` 时失败 |
| AC-30b | 直接调 `a.Store.DeleteUserGuarded`、未做 nil 处理：nil 指针 panic → `got 500/连接重置 want 503` |
| AC-43 | ① 修复前（docker 回退写死 `lathe`）：`current_database got "lathe" want "lathe_f12"`。这正是评审指出的越界，**这一步只跑干跑**；② 两条 DELETE 顺序颠倒：`fixture team got present want deleted`（回滚事务内） |
| AC-18 | 授权侧红：**属主**共享自己的仓库 `got 404 want 200`（非属主 404 半边本就绿） |
| AC-20 | `teamRoutes` 里有 `Write:true` 的路由没有 deny case → `route POST /api/teams/{id}/transfer has no deny case`（先故意漏一条验证测试会抓到，再补齐） |
| AC-21 | 在 v1 设计（按邮箱投递的待办）下会红；本版空壳下 accept 一律 404，因此红信号由**正向对照**提供：同测试里「持有正确码的丙 accept 应 200」`got 404 want 200` |
| AC-22/24/25 | 正确码 accept `got 404 want 200`；过期/撤销那半边本就绿 |
| AC-26 | 空壳不限流：第 N+1 次错码 `got 404 want 429` |
| AC-28 | 两条红：① `0067` 加入前，断言 ①② 失败（无 `Index Cond`，计划为 `Seq Scan on tasks`）；② 若 `scope=team` SQL 写成 OR-IN（评审 B4 复现的形状），即使有索引，断言 ②④⑤ 失败（`Rows Removed by Filter: 36000`、Buffers 618 > 200）。PR 里两条都留日志 |
| AC-30 | 删用户未被拦：`DELETE /api/admin/users/{id}` `got 200 want 409` |
| AC-44 | 没有真实红灯（v1 的唯一索引从未落地）。**如实归为守卫**：它防的是将来有人给 `teams.name` 补唯一约束 |

**回归守卫组（§5.6 与上表标注者）不要求红**：PR 描述写「实现前跑一次确认**绿**（基线），
实现后再跑一次确认**仍绿**」。v1 把 AC-5（旧编号，私有仓库非成员 404）、AC-20b（预览只认属主）、
AC-31b 混进「先红」清单是错的；v1 §5.7 写的「AC-3 迁移前无法编译（列不存在）」也不成立——
SQL 不参与 Go 编译（评审第 9 条）。

## 6. 风险与代价

| 风险 | 影响 | 缓解 |
|---|---|---|
| **`TaskDetail`/`AgentEventsAfter` 放松隔离后，若某个调用点误用了 viewer 版，就是隔离穿透** | 高（安全红线） | ① `TaskDetail` **原名、原签名、原语义一个字不动**（评审 B3：f03/f11 都沿用它；既有调用点 `internal/httpapi/preview.go:56/102/159/203`、`cmd/lathe/main.go:339` 继续用它，只有 `internal/httpapi/api.go:130` 换成新增的 `TaskDetailForViewer`）；② `AgentEventsAfter` 的 user 条件保留、改由调用方传入「已判定可见的任务属主 ID」，SQL 不变；③ 任务读归属全站只有 `ownedTask` 一个 helper、只给读接口用，写接口仍内联属主判定（AC-54 静态检查）；④ AC-6/7/8/54 + AC-11/19/32 守住 |
| `ListTasks` 的 `OR IN (子查询)` 拖慢看板（每 5 秒轮询，`web/src/api.js:88-94` 注释） | 中 | 默认 `scope` 是「我的」，走原 SQL（代码路径分叉，不是同一 SQL 加 OR）；`0067` 的两条索引 + AC-28 的计划断言兜底 |
| **邀请码泄露**（owner 发错了人） | 中 | 48 小时 + 单次使用 + 可撤销 + 猜码限流 + 成员列表一键移除。**这是接受邀请码方案的已知代价**：码相当于短期通行证，靠带外渠道的信任传递 |
| 团队可见性与「属主 404」的直觉冲突 | 低 | 本版已按评审收敛成全 404（§3.5），无中间态 |
| 与其他 11 线在同一批文件上冲突 | 中 | 新增文件优先（`internal/httpapi/teams.go`、`internal/store/team.go`、`web/src/views/Teams.vue`）；对 `api.go`/`query.go`/`TaskDetail.vue` 只做**追加** |
| 迁移号段 0065–0067 与合并顺序 | 低 | `loadMigrations` 允许跳号（`internal/store/migrate.go:422-467`），但 `0031 → 0065` **违反 AGENTS §6.6「不跳号」**：这是编排方为 12 条并行线分配的号段，不是本线自选。**PR 描述必须写明**：「迁移号 0065–0067 是并行开发号段，合入 dev 前按 dev 当时最大号 +1 连续重编号（只改文件名、迁移内容与 AC-3 断言A 的基线号同步更新），不做逻辑改动」 |
| 「团队」名字与 f11 的「团队技能」撞概念 | 低 | §3.11 已划界 |
| **离职 owner 的账号删不掉**（评审第四轮 nit 7）：站内管理员不能管理团队（§4.1），`DeleteUserGuarded` 对「有其他成员的团队的 owner」一律 409；而 owner 只能由他本人转让 | 中（运维）：账号与该团队会一直留着 | **处置路径（写进 docs/features/team-roles.md 的运维一节）**：① 离职交接清单加一条「先在每个团队里 transfer owner」，正常路径不会卡住；② owner 已不可联系时，站内管理员**停用**该账号（`POST /api/admin/users/{id}/disable`，立即踢掉会话，`admin.go:174-197`）——他不能再登录、不能再操作团队；团队照常可用，其余成员可见性不变，admin 仍能邀请/移除 member；但**没人能改团队名、删团队、改角色**，他共享进来的仓库仍对团队可见，直到账号最终被删；③ 若团队需要继续演进，剩余成员新建一个团队、各自把自己的仓库改共享到新团队（共享是属主自己的动作，不需要旧 owner），旧团队留作只读残余。**不做**「站内管理员强制转让」：那等于站内管理员代管任意团队，违反 §4.1，若要做按 §9-D9 另立 PR。唯一成员的团队不受影响（随账号一起删，AC-30②） |
| **`GetRepo` 不返回 `verify_infra`**（存量，§2.1） | 低：本线不读它；但任何人将来用 `GetRepo` 做配置只读视图都会显示「无隔离栈」 | 本线团队视图只走 `ListRepos`（§3.3），AC-52③ 守住；`GetRepo` 不在本线修（改它会与 f06/f07 同段 SELECT 冲突），PR 描述里列为已知存量问题，建议单独 fix |

**代价（明确承认）**：本功能交付后，Lathe 不再是「每个人看到的世界完全独立」的产品。
团队成员天然能看到彼此的邮箱、issue key、分支名、PR 链接与验证证据。这是团队功能的
**全部价值**，但也是一次**信息边界的方向性变化**，必须写进 `docs/02-design.md` §8 的分期表与
`docs/features/team-roles.md` 的开头。

---

## 7. 文档改动（PR 内必带）

| 文件 | 改什么 |
|---|---|
| `docs/features/team-roles.md`（**新增**） | 本 PRD 的定稿版：去掉「待评审」状态、把 AC 落成实现后的实测结果、补一条「已实现 / 未实现」对照表。开头必须写清「团队共享的是可见性，不是执行身份，也不是仓库配置的编辑权」与「凭据永不共享」 |
| `docs/02-design.md` | §8 分期表新增一行 `P2 团队与角色`（写明 §3.6 的角色矩阵与「任务归属不变」）；在数据隔离小节补一句「可见性 = 属主 ∪ 团队；凭据、执行身份、仓库配置写权限仍是属主」 |
| `README.md` | 只改功能描述段（团队、角色、邀请码加入）；**变量表不变**（本线不新增环境变量） |
| `CHANGELOG.md` `[Unreleased]` → `### Added` | 「团队与角色」条目：建团队、一次性邀请码加入、角色矩阵、仓库共享到团队、任务可见性按团队扩展；明确写「凭据不共享，任务仍归属创建者」「仓库配置只有属主能改」「存量数据行为不变」 |
| `scripts/clean-test-db.sh` | **必须改**，三处，**落地顺序固定为 ③ → ① → ②**（评审 v3-2：③ 不先修好，脚本在本机实际连的就是共享库 `lathe`，越过本线的环境边界）。③ **docker 回退分支**：今天写死 `-d lathe`（`:45`），完全无视 `LATHE_TEST_DSN`。改为从 DSN 里解析库名 `DBNAME=$(printf '%s' "$DSN" \| sed -nE 's#^postgres(ql)?://[^/]+/([A-Za-z0-9_]+)([?].*)?$#\2#p')`（本线实测：`…/lathe_f12?sslmode=disable` → `lathe_f12`，`…/lathe` → `lathe`，`postgres://h:1/` 与非 URL → 空串），**解析为空就 `exit 1`**，不回落到任何默认库；`-d "$DBNAME"`。两个分支的第一条输出都改为 `SELECT current_database()`，让人和 AC-43 都能看见实际连的是哪个库。① 干跑段补「孤儿团队」计数 `SELECT count(*) FROM teams t WHERE NOT EXISTS (SELECT 1 FROM team_members m WHERE m.team_id = t.id)`。② `--yes` 段**在既有 `DELETE FROM users WHERE email LIKE '%@example.com'` 之后**补 `DELETE FROM teams t WHERE NOT EXISTS (SELECT 1 FROM team_members m WHERE m.team_id = t.id);`（v2 漏写别名 `t`，Postgres 报 `missing FROM-clause entry for table "t"`，评审 B6），两条 DELETE 用 `# lathe:delete-sql-begin` / `# lathe:delete-sql-end` 注释夹住，供 AC-43② 抽取。**顺序不能反**：先删 fixture 用户，`team_members` 随 `ON DELETE CASCADE` 消失，fixture 团队这才成为孤儿；先删孤儿团队的话，fixture 团队此刻仍有成员，会残留。注意这条也会删掉非测试用户留下的空团队（owner 不能退出，正常路径上不会出现空团队，出现了就是残留），所以干跑段把计数一并打印，供人判断。**本线不执行 `--yes`**：验收走 AC-43 的干跑 + 回滚事务；真删按 AGENTS §4 先问人 |

---

## 8. 实施顺序（供开发阶段排期，不构成额外 scope）

1. `0065` + `0066` + `0067` + `internal/store/team.go`（`RepoAccess`、`teamTx` 重试包装、团队 CRUD、邀请码，全部按 §3.4.2 锁序） + `team_test.go`（AC-1~5、AC-27、AC-49、AC-50）
2. `ListTasks` 的 `scope` 分叉 + 计划断言 + 列漂移守卫（AC-10、AC-28、AC-53）；新增 `ListReposForViewer`/`GetRepoForViewer`（只从 `ListRepos(属主)` 取行，`ListRepos`/`GetRepo` 都不动；AC-51、AC-52）
3. 新增 `TaskDetailForViewer`（调 `TaskDetail`，不复制列清单）+ 任务详情调用点 + `ownedTask`（f04 已合入则只改函数体，否则从 `taskEvents` 抽出，§3.11 f04 行）（AC-6~9、AC-33、AC-54）；`TaskDetail` 不动
4. `teams.go` handler + 显式路由表 + 角色判定（AC-11~20）；`AdminAPI.remove` 改调 `DeleteUserGuarded` 并做 nil 处理（AC-30、AC-30b）
5. 邀请码（AC-21~26）
6. 前端（AC-41）
7. 文档 + CHANGELOG + `clean-test-db.sh`：先修 docker 回退并跑 AC-43①，再加孤儿团队 SQL 并跑 AC-43②；**不执行 `--yes`**（§7、AC-43）
8. 全量 `make lint && make test` + 显式 `-race` 命令（AC-38/39）

---

## 9. 需要人拍板的事项

| # | 事项 | 本 PRD 的默认决策 | 若人不同意 |
|---|---|---|---|
| D1 | 「团队」与 f11「团队技能」是否共用 `teams` 表；团队可见性是否覆盖团队技能 | 本线建 `teams`（成员与可见性），f11 不依赖它；**本版团队可见性不覆盖团队技能**（§3.11 f11 行），团队成员对 team-skills 端点 404 | 若要覆盖：另开 PR，让 team-skills 的**读**端点改调 `RepoAccess`，写端点保持属主，并补团队用例 |
| D2 | 共享仓库的**配置编辑权**给不给 `admin` | **不给**（v1 给，已按评审第 3 条反转）：只有仓库属主能改，`UpdateRepo` 一个字节不改 | 要改回必须先补只读的 `RepoOwnerID` 查询、排除 `gate_mode`/`verify_tier_override`/`protected_branches`/`baseline_dir`/`verify_infra` 这五个字段，并补「admin 改这些字段被拒且库里值不变」的 AC |
| D3 | 「可见但无权」返回 403 还是统一 404 | **统一 404**（v1 是 403，已反转）：全站一种拒绝形态，与预览接口一致 | 改回 403 需人拍板；代价是同一任务在不同写接口上状态码不一致（v1 的 AC-11 vs AC-20b 矛盾） |
| D4 | 邀请的防抢注方案 | **一次性邀请码**（§3.1 取舍 2）；不做「申请加入」（会把 `teams.id` 变成存在性探针） | 备选：① 接受后由邀请人确认（成员列表加「确认/移除」，会造出「已加入未确认」的中间态）；② 先做全站邮箱验证（超出本线，另立 PR）；③ 按 userId 邀请（**不推荐**：挡不住邀请前已抢注的情形） |
| D5 | 删团队前是否必须先取消共享 | 是（409）；owner 移除共享了仓库的成员会连带收回其共享（§3.4.1），owner 自己的共享自己取消 | 允许直接删，仓库自动回私有（`ON DELETE SET NULL` 已支持） |
| D7 | 成员离队时其共享仓库怎么处理（评审 B2） | **同一事务内回私有**（§3.4.1） | 备选：拒绝移除并 409「先让他取消共享」——会让 owner 无法踢出拒不配合的成员，被否决 |
| D8 | 共享要求调用者在目标团队里是什么角色 | **任一角色**（owner/admin/member）：共享的是自己的数据 | 收紧为 owner/admin：普通成员就无法把自己的仓库给组里看 |
| D9 | 离职 owner 不配合转让时怎么办（评审第四轮 nit 7） | **不给站内管理员任何团队管理权**（§4.1）；账号只能停用、不能删除；处置路径见 §6 | 加一个只给站内管理员的「强制转让 owner 给该团队最早加入的 admin」端点：等于站内管理员代管团队，需人拍板并另立 PR，且要补审计日志与 AC |
| D6 | 团队名是否允许重名 | **允许**（v1 要求全站唯一，已按评审第 11 条反转）：团队用 id 区分，避免跨租户探测 | 若要唯一，必须给出不泄露的处理方式（例如建团队永远返回 201 + 随机后缀），否则等于把「名字是否存在」变成公开查询 |

**本功能 status = ok**：没有任何需求要点必须通过「改状态机语义 / 改非属主 404 规则 / 引入新依赖 /
削弱验证」才能实现。凭据共享被需求本身排除；任务可见性由仓库派生，不需要动 `tasks` 表；
执行身份与仓库配置写权限始终在属主手里。

---

---

## 11. 实现与验收结果（2026-11-11）

### 11.1 已实现 / 未实现对照

| 项 | 状态 | 落在哪 |
|---|---|---|
| 迁移 0065 团队/成员/邀请码、0066 `repos.team_id`、0067 `tasks` 两条访问索引（`CONCURRENTLY` + INVALID 自检） | 已实现 | `migrations/0065`–`0067` |
| 读可见性唯一入口 `RepoAccess`；`TaskDetailForViewer`（调原 `TaskDetail`，不复制列清单）；`ListReposForViewer` / `GetRepoForViewer`（只从 `ListRepos(属主)` 取行） | 已实现 | `internal/store/team.go` |
| `ListTasks` 的 `TeamView`：UNION ALL 两臂 + 同形计数，SQL 常量 `listTasksTeamSQL` 与 AC-28 共用 | 已实现 | `internal/store/query.go` |
| 团队写事务全局锁序 `teams → repos → team_members/team_invitations`、`teamTx` 40P01/40001 重试 3 次 → `ErrTeamBusy`（503） | 已实现 | `internal/store/team.go` |
| 团队 / 成员 / 邀请码 / 共享接口，显式路由表 `teamRoutes` | 已实现 | `internal/httpapi/teams.go` |
| `ownedTask`（团队可见语义，名字与签名照 f04）；`taskDetail` 换 `TaskDetailForViewer` 并返回 `viewerRole`；`GET /api/tasks?scope=team`、`GET /api/repos?scope=team` | 已实现 | `internal/httpapi/api.go` |
| 管理员删账号改调 `DeleteUserGuarded`；`AdminAPI.Store == nil` → 503 不回落 | 已实现 | `internal/httpapi/admin.go` |
| 前端：`/teams`、`/teams/:id`、看板「范围」切换、仓库页共享下拉与只读视图、任务详情「队友创建」与按钮显隐 | 已实现 | `web/src/views/Teams.vue`、`TeamDetail.vue`、`Board.vue`、`Repos.vue`、`TaskDetail.vue` |
| `scripts/clean-test-db.sh`：docker 回退按 DSN 解析库名（解析不出 exit 1）、首行 `current_database()`、孤儿团队计数与 `--yes` 段清理 | 已实现 | `scripts/clean-test-db.sh` |
| 凭据共享、admin 改仓库配置、成员在共享仓库建任务、团队技能可见性、邀请邮件、同 issue 两人各跑一遍的去重 | **不做**（§4.2） | — |
| `GetRepo` 不 SELECT `verify_infra`（存量） | **未修**，团队视图已绕开（§6） | 建议单独 fix |

`state.go`、`internal/runner/`、`internal/integration/`、`internal/tracker/`、`skills/`、`internal/preview/`、`internal/prd/`、
`go.mod`/`go.sum`/`web/package.json`/`web/pnpm-lock.yaml` 全部未改（见 11.3）。不新增环境变量。

### 11.2 先红后绿

两个提交：第一个只加测试、迁移与空壳（`TaskDetailForViewer` 直接调 `TaskDetail`、`teamRoutes` handler 一律 404、
`teamTx` 不重试、团队 SQL 为 OR-IN 形状），第二个填实现。红灯原文存 `/opt/lathe-wt/evidence/f12/red.log`，摘录：

| AC | 红阶段的真实失败信号 |
|---|---|
| AC-1/2/3b/5 | 迁移未应用：`schema_migrations 里 65/66/67 的条数 got 0 want 3`；`column "team_id" does not exist` |
| AC-6/9/54 | 成员读他人任务 `got 404 want 200` |
| AC-7/8 | `移除前 got 404 want 200`、`取消前 got 404 want 200` |
| AC-10 | `团队 total got 3 want 4`；`RepoID got 0 want <id>` |
| AC-12/14/15b/16/17 | owner 的授权动作 `got 404 want 200` / `want 409` |
| AC-18/45/46/47/48 | 属主共享 `got 404 want 200`；移除 `remove status got 404 want 200` |
| AC-20 | 故意漏掉一条 deny case：`route POST /api/teams/{id}/transfer has no deny case` |
| AC-21~26 | 发邀请 `got 404 want 201`；正确码 accept `got 404 want 200`；限流 `got 404 want 429` |
| AC-27 | `团队 owner/admin/member 看共享仓库 err got store: 仓库不存在 want <nil>` |
| AC-28 | 空壳 OR-IN：计划为 `Seq Scan rel=tasks rows=4000`；实现后另做一次 OR-IN 中间态：①②③ 失败，`buffers 624 ≤ 200` 失败 |
| AC-29 | 空壳下转让 `got 404`（成功数 0 ≠ 1） |
| AC-30/30b | 删账号未拦 `got 200 want 409`；`Store == nil` 时 nil 指针 panic（`EOF`） |
| AC-43① | 修复前用假 docker shim 截获参数（不连任何库）：`psql -U lathe -d lathe`，want `lathe_f12` |
| AC-43② | 两条 DELETE 顺序颠倒：`fixture team got present want deleted` |
| AC-49 | 空壳下 tx1 拿不到首把锁（`tx1 没有到达「持有第一把锁」的挂起点`）。**如实说明**：v3 锁序的 40P01 确定性复现留在 PRD 阶段证据 `prd-v4-lock-order.log`，本线没有再把实现退回 v3 锁序重放一次 |
| AC-50 | `calls got 1 want 3`；`code got 500 want 503` |
| AC-52 | `got 1 rows want 2` |
| AC-53 | 中间态漏 `agent_cost_usd`：`AgentCostUSD got <nil> want 0.42` |
| AC-11/13/19/31/32/34/44/51 | 守卫，实现前后都绿（今天非属主写本就 404） |
| AC-48 并发红 | 空壳下 remove 一律 404；「v3 锁序 → 500」的并发红没有专门复现，以 AC-49 为准 |

### 11.3 验收结果（实现提交上）

| AC | 结果 | 证据 |
|---|---|---|
| AC-1/2/3b/5 | 通过 | `TestMigrationsApplyTeamSchema`、`TestMigration0067ValidityCheck`、`TestExistingReposStayPrivateAfterMigration` |
| AC-3 | 通过：两轮「down×3 → max(version)=31、三表/列/索引不存在 → up×3 → 65/66/67 在、indisvalid」，每步退出码 0 | `/opt/lathe-wt/evidence/f12/migrate-roundtrip.log` |
| AC-4 | 通过：`grep -iE "insert into\|update .* set"` 无匹配 | `static-checks.log` |
| AC-6~10、AC-54① | 通过 | `TestTeamSharedRepoInvisibleToNonMember` … `TestTeamMemberReadsEveryTaskReadRoute` |
| AC-54②③ | 通过：`ownedTask` 定义 1 个；`taskEvents` 调 `a.ownedTask(`、不含内联属主判定 | `TestOwnedTaskIsTheOnlyTaskReadHelper`、`static-checks.log` |
| AC-11~20、AC-45~48 | 通过（AC-12 逐格 33 格 + `role=owner` 4 例 + 主矩阵 7 行 × 4 调用者；AC-48 50 轮无 500/503、`TeamTxRetries` 增量 0） | `internal/httpapi/teams_test.go` |
| AC-21~26 | 通过 | 同上 |
| AC-27、AC-28、AC-49、AC-50、AC-52、AC-53 | 通过（AC-28：两条索引各有 `Index Cond`、无 tasks Seq Scan、buffers ≤ 200；AC-49：F-1~F-9 十个交错无 40P01、重试增量 0） | `internal/store/team_test.go` |
| AC-29 | 通过（10 轮，每轮恰好一个 200） | `TestTeamLastOwnerGuardUnderConcurrency` |
| AC-30/30b | 通过 | `internal/httpapi/teams_admin_test.go` |
| AC-31~34、AC-42、AC-44 | 通过 | `teams_test.go`；AC-33 另跑 `TestAPIListAndDetail\|TestAPIIsolationBetweenUsers\|TestAPIConfigNeverLeaksSecrets` 通过 |
| AC-33/51 静态 | `TaskDetail`/`ListRepos` 签名行 diff 为空；`ListRepos` 函数体与 merge-base 逐字节相同 | `static-checks.log` |
| AC-35/36/40 | `state.go`、验证三文件、依赖四文件 diff 均为空 | `static-checks.log` |
| AC-37 | 恰好 6 行 `A migrations/006[5-7]_…`，不匹配行数 0 | `static-checks.log` |
| AC-38 | `go test -p 1 -race -count=1 ./internal/httpapi/... ./internal/store/... -run '…'` 退出码 0 | `/opt/lathe-wt/evidence/f12/race.log` |
| AC-39 | `LATHE_TEST_REQUIRE_DB=1 make lint && make test` 全绿；`gofmt -l`（含未入库文件）为空；`golangci-lint --new-from-rev=origin/dev` 0 issues | `gate-final.log` |
| AC-41 | `make ui && make build` 成功，`.gitkeep` 仍在 | `ui-build.log` |
| AC-43 | ①② 通过；**本线未执行 `--yes`**（真删按 AGENTS §4 必须先问人） | `TestCleanTestDBTargetsDSNDatabase`、`TestCleanTestDBDeleteSQLInRollback` |

手测（端口 8412，库 `lathe_f12`，从非仓库目录启动、`LATHE_REAP_ENABLED=false`，按 PID 停）：三个账号走完
「建仓库 → 建团队 → 生成邀请码 → 错码 404 → 正确码 200 → 重放 404 → 共享 → 成员 `scope=team` 看到只读仓库行
（`owned:false`）→ 成员改配置 404 → 非成员看团队 404 → 成员退出后不可见 → owner 自退 409 → 有共享仓库删团队 409
→ 取消共享后删团队 200」，记录在 `/opt/lathe-wt/evidence/f12/smoke-8412.log`。界面未截图（本线没有起浏览器会话），
前端只验证了构建与 `/teams` 路由返回 SPA。

### 11.4 实现中发现、未在本线修的存量问题

- `issue_comments.user_id` 是 `ON DELETE SET NULL`，而 `issue_comments_author_check` 要求 `user_id`/`actor` 恰有一个非空：
  fixture 用户写过评论时，`clean-test-db.sh --yes` 既有的 `DELETE FROM users …@example.com` 会撞 SQLSTATE 23514。
  与 f12 无关；`TestCleanTestDBDeleteSQLInRollback` 在回滚事务里先删这些评论再验证两条 DELETE。建议单独修
  （改成 `ON DELETE CASCADE` 或在脚本里先删评论）。
- `GetRepo` 不 SELECT `verify_infra`（§2.1 / §6），本线不修。
