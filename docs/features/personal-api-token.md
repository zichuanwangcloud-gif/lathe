# 个人 API 令牌（Personal Access Token, PAT）

> 状态：已实现（f05，分支 `feat/personal-api-token`，迁移 `0044`）。
> 本文是 PRD rev.4 的定稿版：保留问题、方案、边界、验收契约，并附实现与验收结果。

## 1. 为什么要有它

Lathe 的价值在于能被自动化驱动：CI 里建单、脚本查任务、告警侧触发重试。此前只有两条路：

| 做法 | 问题 |
|---|---|
| `LATHE_ADMIN_TOKEN` 的 Bearer | 部署级单一共享密钥，身份恒等于内置管理员；越权面最大、无法按人吊销 |
| 会话 Cookie | 12 小时过期、绑浏览器、是 CSRF 面 |

PAT 是**属于某个真实用户、可命名/吊销/设过期、权限严格等于本人**的长期凭据。
硬约束：用 PAT 做的事与本人用浏览器做的一样——`actorOf` 仍是 `user:<id>`，任务按
`owner_id` 过滤，非属主仍是 404。PAT 不产生新的验证能力，也不改验证体系。

## 2. 行为契约

### 2.1 三条身份来源（`internal/httpapi/auth.go` 的 `resolve`）

1. `Authorization: Bearer <LATHE_ADMIN_TOKEN>` → 内置管理员。**守卫 `a.Token != ""` 一字未改**：
   去掉它，未配置管理员令牌时 `TrimSpace` 会把 NBSP/EM SPACE/NEL 压成空串，与空令牌「相等」，
   未认证者直接提权（`TestAdminTokenEmptyRejectsWhitespaceBearer` 守门）。仍优先于 PAT。
2. `Authorization: Bearer lathe_pat_…` → 签发者本人。守卫是形态前缀，与 `a.Token` 无关
   （没配管理员令牌的部署里 PAT 照样可用）。形态判定用**未 trim** 的值。形态像 PAT 却查不到
   （不存在/吊销/过期/停用）→ 直接 401，**不回落 Cookie**，否则吊销会被浏览器会话掩盖。
3. 会话 Cookie。

不带 `Authorization` 头的请求（绝大多数浏览器流量）零额外开销。令牌只认请求头，
`?token=` / `?access_token=` 无效。鉴权失败一律 401 `未登录`，不区分原因。
PAT 解析出的用户若 `must_change_password`，`Require` 照常回 409。

### 2.2 令牌

- 格式：`lathe_pat_` + 43 个 base64url（32 字节 `crypto/rand`）。
- 库里只存 SHA-256 十六进制（与 `sessions` 同口径：明文是 256 位随机数，无字典可爆破，
  不需要盐与慢哈希；也不用 AES-GCM，因为平台从不需要还原明文）。
- `prefix` 列存明文 `lathe_pat_` 之后的前 8 个字符，仅供列表辨认。
- 过期：可选，非空时必须晚于现在且不超过 365 天（常量 `MaxPATTTL`）。前端默认 90 天。
- 每用户**未吊销且未过期**的令牌最多 20 把（常量 `MaxPATsPerUser`）。并发签发可能偏差到 21 把，接受。
- 最后使用时间：`Touch` 节流 60 秒，判定与写入同一条 UPDATE；`use_count` 因此是近似值。
  唯一调用点是 `Auth.lookupPAT`，失败只 `slog.Warn`，不改鉴权结论。
- 软吊销：`revoked_at` 置值、行保留；`PersonalAccessTokens.GC` 在吊销/过期满 30 天后删行，
  由 `cmd/lathe` 的 `gcAuthRows` 每小时调用（与会话清理同一定时器、独立日志与计数）。

### 2.3 接口（`internal/httpapi/pat.go`）

全部挂在 `Require` 之下，且**只接受会话 Cookie**：请求只要带了 `Authorization` 头就 403
「令牌管理只能用浏览器登录态」——一把泄漏的 PAT 不能自我增殖，`LATHE_ADMIN_TOKEN` 也不例外；
一刀切掉「Bearer 是别人、Cookie 是自己」的身份二义。

| 方法/路径 | 成功 | 失败 |
|---|---|---|
| `GET /api/pat` | 200 `{"tokens":[{id,name,prefix,createdAt,lastUsedAt,useCount,expiresAt,expired}]}`；列出全部**未吊销**令牌，含已过期（`expired:true`） | 401 / 403 |
| `POST /api/pat` 体 `{"name","expiresInDays"\|"expiresAt","currentPassword"}` | 201 `{id,name,prefix,token,createdAt,expiresAt}`，`token` 明文只此一次 | 400 / 403 / 409 / 429 |
| `DELETE /api/pat/{id}` | 200 `{"status":"ok"}` | 404（不存在或不是自己的，不用 403） |

`POST` 的校验顺序写死：`Require`（401/409）→ `Authorization` 头（403）→ 限流（每用户 15 分钟
10 次，放行即记一次，429）→ 解析体 → 当前口令（缺 400、错 **403**——不用 401，前端会把 401
当掉线）→ 名字（去空白后非空、≤64 字节，400）→ 过期（400）→ 上限（409，文案含「吊销」）→ 入库。
限流在验口令之前：被限流的请求不跑 bcrypt，第 11 次带正确口令也是 429。

| 令牌状态 | 出现在列表 | 计入上限 | 能认证 |
|---|---|---|---|
| 有效 | 是 | 是 | 能 |
| 已过期、未吊销 | 是（`expired:true`） | 否 | 否 |
| 已吊销 | 否 | 否 | 否 |

### 2.4 自动吊销（PAT 是「没有 Cookie 的会话」）

| 位置 | 动作 |
|---|---|
| 管理员停用账号 `AdminAPI.disable` | 删会话后 `RevokeUser`；失败 500。`Lookup` SQL 里 `u.disabled_at IS NULL` 是第二道防线 |
| 本人改密 `AccountAPI.changePassword` | `SetPassword` **之前** `RevokeUser`（全部，不区分「当前这把」） |
| 邮件找回重置 `AccountAPI.resetPassword` | `Consume` 之后、`SetPassword` 之前 `RevokeUser` |
| 管理员代重置 `AdminAPI.resetPassword` | `SetPassword` 之前 `RevokeUser`（409 不再是唯一屏障） |

改口令三条路径共用 `revokePATsBeforePasswordChange`：吊销失败或 `PATs` 未接线一律 500
「吊销个人 API 令牌失败，密码未修改，请重试」，口令不改——绝不出现「新口令 + 旧 PAT」。
会话清理的既有 `_ =` 写法未改。

**与 GitHub 的有意分歧**：签发需重新验口令与 GitHub sudo 模式一致；但 GitHub 改密不吊销 PAT，
我们吊销——Lathe 的 PAT 无 scopes、权限等于本人全部权限，「改密码」是用户唯一直觉性的止血动作。
代价：改密后所有脚本要换新令牌（界面卡片固定提示「修改或重置密码会吊销你的全部令牌」）。

### 2.5 其它安全面

- 日志只出现 `user_id`/`token_id`/`prefix`，不记请求头、口令与明文。
- 失败摘要脱敏（`internal/runner/redact.go` 的 `tokenShapes`）新增 `lathe_pat_[A-Za-z0-9_-]{32,}`
  整段抹掉（尾部不加 `\b`，否则末位 `-` 会残留）。PAT 不来自环境变量，不进 agent 子进程。
- 令牌认证不做限流（256 位随机值不可暴力）；签发接口做（它校验口令）。
- 真相源单一，无进程内缓存，吊销在多节点上下一个请求即生效。

## 3. 界面

`web/src/components/PersonalTokens.vue`，在个人设置页凭据卡片之后、通知邮箱之前
（`PersonalSettings.vue` 只多一行 import 与一行 `<PersonalTokens />`）。列表显示名字、
前缀（可复制）、创建、最后使用（「从未使用」灰字）、过期（「永不过期」）、次数、吊销
（二次确认「用这把令牌的脚本会立即失效」）；已过期行灰显并标「已过期」。签发表单：名字、
有效期（7/30/90/365 天/永不过期，默认 90）、当前密码（提交后立即清空）。一次性明文只放组件
`ref`，不进任何存储、URL、`history.state`，关闭或刷新即消失。

## 4. 范围外

不做 scopes、轮换/续签、「最近使用 IP/UA」、新的 `task_events` 类型、开发者文档页；
不改 `LATHE_ADMIN_TOKEN`、`Auth.Enabled()`、非属主 404、状态机与验证体系。
团队令牌（f12）若要做是新表新 PRD，本表加 `team_id` 列即可，不阻碍。
**无新增环境变量**（上限、TTL、节流窗口都是常量，理由见 PRD §3.8）。

## 5. 相关代码

| 文件 | 内容 |
|---|---|
| `migrations/0044_personal_access_tokens.{up,down}.sql` | 表、唯一索引、两个 GC 部分索引 |
| `internal/store/pats.go` | `Create/List/CountActive/Revoke/RevokeUser/Lookup/Touch/GC` |
| `internal/httpapi/auth.go` | `PATLookup`、`WithPATs`、`resolve` 两条支路、`lookupPAT` |
| `internal/httpapi/pat.go` | `PATAPI` 三个接口、`requireCookieIdentity`、`patExpiry` |
| `internal/httpapi/accounts.go`、`admin.go` | `PATs PATRevoker` 字段与四处吊销 |
| `cmd/lathe/main.go` | 接线、`gcAuthRows` |
| `internal/runner/redact.go` | `lathe_pat_` 形态 |

## 6. 验收结果

测试库 `lathe_f05`；证据目录 `/opt/lathe-wt/evidence/f05/`（红阶段在 `red/`）。

| AC | 测试 / 方法 | 红（实现前） | 绿 |
|---|---|---|---|
| AC-1 迁移可逆 | `lathe_migrate up && down && up`；`TestPATSchema` | 未迁移/down 后列集合 `got=[]` | 三段 exit 0；PASS |
| AC-2 库里无明文 | `TestPATCreateStoresOnlyHash` | 接口 404 | PASS |
| AC-3 明文只出现一次 | `TestPATPlaintextReturnedOnlyOnCreate` | 404 | PASS |
| AC-4 等同本人权限 | `TestPATBearerActsAsOwner`（只见自己任务 / 普通 403 / 管理员 200） | 404 | PASS |
| AC-5 非属主 404 | `TestPATRevokeOtherUserReturns404` | 404（签发不可用） | PASS |
| AC-6 停用即失效 | `TestDisabledUserPATRejected` | 未接 RevokeUser：`revoked_at got=nil` | PASS |
| AC-6b 空管理员令牌不提权 | `TestAdminTokenEmptyRejectsWhitespaceBearer`（九行） | 初稿写法：1/2/3/4a/5a 行 200 + bootstrap 管理员；4b/5b/6/7 401（只防回归） | 九行全 401 |
| AC-7 过期与吊销 | `TestPATExpiryAndRevoke`（含已吊销 Bearer + 有效 Cookie 仍 401） | 404 | PASS |
| AC-8 最后使用节流 | `TestPATLastUsedAtRecordedAndThrottled` | 404 | PASS |
| AC-9 管理员令牌不变 | `TestAuthBearerToken`（未改）+ `TestAdminTokenStillWinsOverPAT` | 未接 PAT 支路时第二条 `authenticated` false | PASS |
| AC-10 管理接口拒 Bearer | `TestPATManagementRequiresCookie`（3 方法 × 6 形态） | 404 | PASS |
| AC-11 令牌不进 URL | `TestPATQueryParamIgnored` | 无红阶段（现状即满足，防回归） | PASS |
| AC-12 脱敏 | `TestRedactLathePATShape` | 明文健在 | `[REDACTED]`，短文本不误伤 |
| AC-13 每用户上限 | `TestPATMaxPerUser`（20 把入库预置 + 3 次 HTTP） | 404 | PASS |
| AC-14 名字校验 | `TestPATNameValidation` | 404 | PASS |
| AC-15 列表属主隔离 | `TestPATListIsOwnerScoped` | 404 | PASS |
| AC-16 基线 | `make lint && make test`、`make test-race`、`make ui && make build`；`go.mod`/`web/package.json` 无 diff | — | 全部 exit 0 |
| AC-17 界面两态 | `:8405` 手测，截图 `ac17-1-created-once.png`、`ac17-2-after-reload.png` | — | 明文区块出现 → 刷新后消失，列表显示前缀与「从未使用」 |
| AC-19 改口令即吊销 | `TestPasswordChangeRevokesPATs`、`TestPasswordChangeAbortsIfRevokeFails` | 未接吊销：改密/找回后 200；代重置后 409，再改密后 200 | 三路径全 401；吊销失败 500 且旧口令仍可登录 |
| AC-20 签发需当前口令 | `TestPATCreateRequiresCurrentPassword` | 无口令校验：1/2/3 行 201、6 行 400；无限流：第 11 次 201 | PASS |
| AC-21 新路由挡未认证 | `TestAdminAndSMTPRequireAuth` 补三行 | 404 | 401 |

**迁移号说明**：`0044` 相对现有最大号 `0031` 是跳号，属并行开发期的分段约定（迁移器容忍空洞，
既有 0025→0028 为先例），合入后由集成日复核是否重排。
