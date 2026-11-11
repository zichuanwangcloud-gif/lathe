# f07 — GitLab / Gitea 代码平台

> 状态：**定稿并已实现**（PRD 设计稿 v6 原样收录于 §1–§9；实现与验收结果、与 PRD 的出入见 §10）· 分支 `feat/vcs-gitlab-gitea` · 迁移号段 0050–0052
> 关联：`docs/02-design.md` §1（产品边界）、§4（数据模型）、§7（PR 评论回流）；`AGENTS.md` §1/§4/§5/§6/§7
> 下文 §2 的行号取证基于 `cff5332`（实现前的 dev），实现之后的代码位置以 §10.2 为准。

**结论先行**：仓库登记时可选 GitHub / GitLab / Gitea（含自建实例），平台与实例地址钉在 `repos` 行上；
PR/MR、合并检测、review 回流、克隆鉴权都按仓库所在平台走。GitHub 路径的 API 请求、git argv 与环境
逐字节不变（AC-1/AC-5c）；GitLab/Gitea 带 token 的 git 调用在 Lathe 私有、无 config 文件的 transport
GIT_DIR 里运行，agent 写进 mirror/worktree 的任何配置与 hook 都碰不到 token（AC-5b）。验证体系一行未改。

---

## 1. 问题与目标

### 1.1 问题

Lathe 的产品定义是「指派 issue → 出来一个**已验证的 PR**」。这三步里，「已验证」是 Lathe
自己的，与平台无关；而「PR」「合并状态」「review 意见回流」「克隆鉴权」四件事，目前
**硬编码为 GitHub**：

- 分支推上去之后的 PR 创建与合并检测只有一套 GitHub 实现；
- 克隆 URL 在三个地方各自拼成 `git@github.com:<owner>/<repo>.git`（证据见 §2.3），
  自建 GitLab / Gitea 实例一律连不上；
- review 回流只认 GitHub 的 `X-Hub-Signature-256` 与 `author_association`；
- 凭据表里只有 `github` / `github_webhook` 两种 kind，且没有「实例地址」概念。

结果是：**用 GitLab 或 Gitea 的团队无法把 Lathe 用在自己的仓库上**。这不是体验问题，
是「产品在这类团队里完全不可用」。竞品 Multica 把「代码平台」做成可选项，Lathe 要守住
「已验证 PR」的定位，就必须让 PR 这件事发生在用户自己的代码平台上。

### 1.2 目标

1. 把「代码平台」抽成一个接口（`internal/vcs`）：开 PR/MR、查合并状态、拉 review 意见、
   提供克隆 URL 与 git 鉴权环境。
2. GitHub 成为该接口的**第一个实现，行为逐字节不变**（含 API 路径、PR 正文、合并检测
   轮询、webhook 验签与信任名单）。
3. 新增 GitLab（含自建实例）与 Gitea（含自建实例）两个实现。
4. 仓库配置可选平台；平台在**仓库登记时选定、之后不可改**（与 `provider_repo` 现有语义
   一致）。
5. 凭据仍走现有的 AES-256-GCM 加密入库 + 保存时验证连通性；新增「实例地址」字段。
6. 「永不 push 受保护分支」的约束在新平台同样成立，且是**同一处代码**保证的。
7. 测试只用 `httptest` 假服务器，不引入新依赖、不引入新的外部服务。

### 1.3 与「已验证 PR」定位的关系

平台抽象只替换**出口**（PR 落在哪个平台、意见从哪个平台回来）。验证体系（`verifications`
表、红绿复现、隔离栈、档位判定）**一行不改**：验证在本地 worktree 里完成，与代码平台
无关。这一点是本设计的护栏——任何让验证「因为换了平台而变松」的实现方向都直接作废。

---

## 2. 现状取证

以下全部在 `/opt/lathe-wt/f07-vcs`（分支 `feat/vcs-gitlab-gitea`，基于 `origin/dev` @ `cff5332`）上核对。

### 2.1 代码平台客户端（GitHub 内嵌）

| 事实 | 位置 |
|---|---|
| 构造函数只吃 token；测试用 `NewClientWithBaseURL` 注入桩地址 | `internal/integration/github/github.go:25`、`:37` |
| `SplitRepo` **强制恰好两段** `owner/name`（GitLab 子组路径会直接报错） | `internal/integration/github/github.go:54-60` |
| 开 PR：先查同 head→base 的开放 PR，有则复用；422 + `No commits between` 映射为 `ErrNoCommits` | `internal/integration/github/github.go:89-137`、`:83` |
| 合并状态读取 + 精简摘要（Merged/State/BaseRef/HeadSHA），供 F4.1/F4.4 消费 | `internal/integration/github/github.go:163-206` |
| review 意见全量拉取（行内 + 普通 + review 总评，翻页上限 10 页） | `internal/integration/github/webhook.go:337-410` |
| 验签：`X-Hub-Signature-256`，`hmac.Equal` 常数时间比较，拒绝 SHA-1 降级 | `internal/integration/github/webhook.go:26-34`、`:54-71` |
| 事件模型 `ReviewEvent` + `Actionable()` 两道守卫（作者关系白名单 / 自身回环） | `internal/integration/github/webhook.go:77-111`、`:165-217` |
| 信任名单是**白名单**：OWNER/MEMBER/COLLABORATOR/CONTRIBUTOR，未知取值一律不可信 | `internal/integration/github/webhook.go:128-142` |
| PR 正文生成器本身与平台无关（issue 链接 + 改动说明 + 验证结果 + 署名） | `internal/integration/github/github.go:275-293` |
| 凭据校验：`/user` + `X-OAuth-Scopes` 检查 `repo` 权限 | `internal/integration/github/verify.go:21-54` |

### 2.2 流水线消费面

| 事实 | 位置 |
|---|---|
| 流水线自己的窄接口 `GitHubAPI{CreatePR, GetPRInfo}` | `internal/runner/pipeline.go:23-32` |
| `Clients` 接口只有两个方法：`Tracker(provider)` 与 `GitHub(ctx)` | `internal/runner/pipeline.go:38-44` |
| 每次执行现取客户端（凭据可运行期修改） | `internal/runner/pipeline.go:402-440`，取 GitHub 在 `:436` |
| 开 PR 时传 `github.PRParams` 与 `github.BuildPRBody`，随后 `SetPRNumber` 落库 | `internal/runner/pipeline.go:1139-1157` |
| 合并检测轮询：`clients.GitHub` → `gh.GetPRInfo` → merged/closed/open 三态推进 | `internal/runner/mergepoll.go:158-193`、`:176` |
| 合并检测的仓库配置由 `NewRepoLookup` 独立查 `repos`（与 `cmd/lathe/queue.go` 各维护一份 SQL） | `internal/runner/mergepoll.go:26-70` |
| F4.3 rebase 跟进里另拼一次 clone URL | `internal/runner/mergepoll.go:342`、`:358-360`、`:477` |
| review 回流的可选能力用类型断言 `ReviewFeedbackSource` 取，取不到退回事件流兜底 | `internal/runner/review.go:289-302`、`:321-360` |
| 回流 actor 常量 `system:github-review`（审计口径） | `internal/runner/review.go:27`、`:132`、`:179` |
| 意见落事件 payload 时按 rune 截断 4000 | `internal/runner/review.go:28-30`、`:211-229` |

### 2.3 克隆与推送（三处各自拼 URL）

| 事实 | 位置 |
|---|---|
| 派发时拼 `"git@github.com:" + providerRepo + ".git"` | `cmd/lathe/queue.go:686` |
| 合并检测 / rebase 跟进里再拼一次（`cloneURLFor`） | `internal/runner/mergepoll.go:354-360`、`:342`、`:477` |
| PRD 规划路径默认拼 `git@github.com:` + providerRepo | `internal/prd/service.go:169-174`（`CloneURL` 字段在 `:109-111`） |
| 镜像路径只按 `providerRepo` 打平（`/`→`-`），**不含实例主机** | `internal/runner/worktree.go:62-66` |
| `clone --mirror` 后立刻解除 `remote.origin.mirror`（否则镜像推送会覆盖受保护分支） | `internal/runner/worktree.go:89-125`，安全注释在 `:105-111` |
| git 子进程环境：禁交互凭据、注入提交身份；**没有任何远端鉴权注入点** | `internal/runner/worktree.go:949-988` |
| 推送前最后一道闸门：`ValidatePushTarget` + 基线分支检查 | `internal/runner/branch.go:108-121`；调用点 `internal/runner/worktree.go:172-190`、`:240-258` |
| `RepoConfig` 目前只有分支策略字段，无平台概念 | `internal/runner/branch.go:33-56` |

### 2.4 凭据、配置与 webhook

| 事实 | 位置 |
|---|---|
| `integrations` 表：`(user_id, kind)` 唯一，`secret_enc` 加密列，`kind` 由 CHECK 约束限制取值 | `migrations/0001_init.up.sql:31-48`；kind 取值在 `migrations/0028_github_webhook_secret.up.sql:14-26` |
| 凭据 kind 常量与 `KnownKinds`（设置页据此渲染） | `internal/store/secrets.go:14-24` |
| 保存即验证：`save` → `runVerify` → `MarkVerified/MarkVerifyFailed` | `internal/httpapi/credentials.go:86-114`、`:153-170` |
| `Verifier` 接口签名是 `(ctx, kind, token)`，**没有实例地址的位置** | `internal/httpapi/credentials.go:24-26` |
| 环境兜底只给内置管理员；`EnvFallback` 是固定字段集 | `internal/creds/creds.go:35-41`、`:107-119`；`internal/creds/factory.go:43-57` |
| `Provider.Token` 缓存 30s，按 kind；`Invalidate` 由凭据保存触发 | `internal/creds/creds.go:25-29`、`:79-105`；`internal/creds/verifier.go`；`cmd/lathe/main.go:379` |
| `Provider.Ready` 只检查 `linear` + `github` | `internal/creds/creds.go:169-178` |
| GitHub webhook 路由 `/webhooks/github/{slug}`，按 slug 解析属主与密钥 | `cmd/lathe/main.go:274`、`:642-664`；`internal/httpapi/github_webhook.go:57-141` |
| 投递去重键加平台前缀，共用 `webhook_deliveries` 表 | `internal/httpapi/github_webhook.go:49-51`、`:106-111`；`internal/store/webhook.go:13-33` |
| 按 PR 找任务时**两处属主过滤各自独立成立**（安全边界） | `internal/task/machine.go:488-495` |
| 脱敏权威层登记的是 `config.KnownSecrets()`，其中**没有** `GitHubWebhookSecret` | `internal/config/config.go:429-444`；消费方 `internal/runner/redact.go:540-560` |
| 仓库登记 API 只收 `providerRepo/defaultBranch/hotfixBase`，`providerRepo` 只校验「含 `/`」 | `internal/httpapi/api.go:624-660`（校验在 `:639`） |
| 仓库唯一约束 `(user_id, provider_repo)` | `migrations/0001_init.up.sql:69` |
| `RepoRow` 字段集与四处 SQL（list/get/update/create） | `internal/store/query.go:279-306`、`:308-332`、`:336-355`、`:394-441`、`:455-479` |
| 设置页凭据卡片由 `META[kind]` 渲染，未知 kind 直接显示原串 | `web/src/views/PersonalSettings.vue:10-20`、`:36-62`、`:206-262` |
| 仓库页只提交分支策略字段 | `web/src/views/Repos.vue:32-44`、`:90-104` |

### 2.5 现状结论（驱动设计的四条事实）

1. **平台耦合点集中在四处**：`runner.Clients.GitHub`、`mergepoll` 的轮询、`github` 包的
   类型、三处 clone URL 拼接。其余（验证、状态机、调度、回执）完全平台无关。
2. **已有「抽接口 + 类型别名」先例**：`internal/integration/linear` 用 `type Issue = tracker.Issue`
   适配 `internal/tracker`，包内部与老调用方零改动（`internal/integration/linear/linear.go:53-61`）。
   本设计照抄这个形态，把 `runner` 与 test 侧的改动压到最小。**例外**：带方法的类型不能做别名
   （Go 不允许给非本包类型定义方法；`github.ReviewEvent` 有 `Actionable()`，`webhook.go:165`）。
   实测见 `/opt/lathe-wt/evidence/f07/prd-v2-alias-compile-probe.log`
   （`cannot define new methods on non-local type ReviewEvent`）。别名只用于**无方法**的纯数据类型：
   `PRParams/PullRequest/PRInfo/ReviewComment/ReviewFeedback`（`grep 'func (.*\*\?\(PRParams\|PRInfo\|PullRequest\|ReviewComment\|ReviewFeedback\))'`
   在 `internal/integration/github` 下零命中）。
3. **克隆鉴权目前不存在**：GitHub 走宿主机 SSH（`git@github.com:`），新平台用 token 只能
   走 HTTPS，而 `git()` 没有任何凭据注入点（`internal/runner/worktree.go:958-977`）。
4. **信任判定是安全边界的核心**：GitLab/Gitea 没有 `author_association`，必须用「成员/
   协作者查询」等价替换，且必须**失败即拒**（fail-closed）。

---

## 3. 方案

### 3.1 分层与包结构

```
internal/vcs/                    ← 新：契约包（零依赖，只 import 标准库）
  vcs.go         Provider 枚举、Client 接口、PRParams/PullRequest/PRInfo/
                 ReviewComment/ReviewFeedback/ReviewEvent、BuildPRBody、
                 SplitRef、错误哨兵（ErrNoCommits 等）
  remote.go      RepoKey/CloneURL 纯函数、Target；GitAuth（不透明、脱敏 String()、IsZero()）
                 —— transport 的构造（私有 GIT_DIR、白名单环境、fd 管道）在 runner/worktree.go，见 §3.3.2
  gitlab/        ← 新：GitLab 实现（API + webhook + verify）
  gitea/         ← 新：Gitea 实现（同上）
internal/integration/github/     ← 保留原位：GitHub 实现 + 指向 vcs 的类型别名
```

**为什么 GitHub 不搬家**：`internal/integration/github` 被 19 个测试文件 import
（`grep -rl '"github.com/zichuanwangcloud-gif/lathe/internal/integration/github"' --include=*_test.go internal | wc -l`
→ 19）。搬家会制造
一次与功能无关的大规模改动，评审无法分辨「行为是否真变了」。改为：`vcs` 定义契约，
`github` 包保留 `type PRParams = vcs.PRParams`、`type PullRequest = vcs.PullRequest`、
`type PRInfo = vcs.PRInfo`、`type ReviewComment = vcs.ReviewComment`、
`type ReviewFeedback = vcs.ReviewFeedback`、`var ErrNoCommits = vcs.ErrNoCommits`
（五个类型均无方法，别名合法）。`github.Client` 满足 `vcs.Client`（编译期断言
`var _ vcs.Client = (*Client)(nil)`）。

**`ReviewEvent` 不做别名（v2 修订，评审 blocker 1）**：

- `github.ReviewEvent` **原样保留为 github 包的本地 struct**，字段、`Actionable()`、
  `TrustedAssociation()`、`trustedAssociations` 白名单、`describeAssociation()` 一个字符都不动
  （`internal/integration/github/webhook.go:77-227`）。GitHub 的信任判定仍住在 github 包，
  不进契约包，严格程度与今天逐字节相同。
- `vcs` 包另定义**平台中立**的 `vcs.ReviewEvent`：`Provider, Event, Action, Repo, PRNumber,
  PRAuthor, Key, Author, Body, Path, Line, URL, ReviewState`——**没有** `AuthorAssociation`，
  也没有任何信任方法。它表达的是「已经通过本平台信任判定的一条意见」，不是原始载荷。
- github 包**新增**一个转换方法 `func (e *ReviewEvent) VCS() *vcs.ReviewEvent`（新增方法，
  不改既有方法；`Provider` 填 `github`）。
- `runner.ReviewIntake.AcceptReview` 的参数从 `*github.ReviewEvent` 改为 `*vcs.ReviewEvent`。
  它的契约本来就是「调用方已验签、已去重、已判定为可处理」（`internal/runner/review.go:81-89`），
  所以中立事件不带信任字段是对的：信任判定只发生在各平台 handler 里，`AcceptReview` 之前。
- GitHub handler 的唯一改动：`Actionable()` 照旧先调（`internal/httpapi/github_webhook.go:122`
  调用点与返回值处理不变），通过后传 `ev.VCS()` 给 `AcceptReview`。
- 二轮全量拉取（`ReviewFeedback`）里 GitHub 的 `TrustedAssociation` 过滤
  （`webhook.go:356/376/399`）不动；`vcs.ReviewComment` 保留 `AuthorAssociation` 字段（别名后仍是
  同一个字段，纯数据），GitLab/Gitea 实现在拉取时用各自的成员查询过滤，不填该字段。

**`Provider` 是必填字段（v5，第四轮 blocker 3）**：`AcceptReview` 按 `(属主, 平台, 路径, 编号)` 找任务（§3.5.4），
所以 `vcs.ReviewEvent.Provider` 为空串时既不能当 github 猜（那等于在一道跨平台过滤上设了静默默认值——漏填 `Provider`
的 handler 会被当成 GitHub 投递路由，恰好是这道过滤要挡的串扰），也不能让它查不到任务后静默 `ReviewIgnored`（漏填的
handler 会把所有回流悄悄吞掉，投递记录只写「没有对应的任务」，看起来像正常的无关 PR）。规则：`AcceptReview` 第一步
`if !ev.Provider.Valid() { return ReviewOutcome{}, fmt.Errorf("runner: review 事件缺少或带了未知的代码平台 %q", ev.Provider) }`，
**fail-closed 且可见**（handler 收到 error 走既有路径：`slog.Error`、`FinishDelivery(key, err.Error())` 把原因落进投递记录、
响应 `{"status":"error"}`，`internal/httpapi/github_webhook.go:128-135`）。`Provider.Valid()` 定义在 `internal/vcs/vcs.go`，只认三个枚举值。三个 handler 都**必须**填 `Provider`：
GitHub 经 `ev.VCS()` 填 `github`（AC-2b 断言）；GitLab/Gitea handler 构造事件时填各自的值（AC-17 断言）。

**这条修订的影响面（如实列出，v5 补全）**：`AcceptReview` 签名变化与 `Provider` 必填一起波及 5 处测试代码，**另有 3 处
需要补 `Provider: vcs.ProviderGitHub`**（否则按 §3.5.4 的 SQL 查不到任务，返回的不是它们断言的 `ReviewQueued/ReviewParked`）：

| 位置 | 改动 | 补 `Provider` |
|---|---|---|
| `internal/runner/review_test.go:73-79`（`inlineEvent` 构造器，该文件 17 处 `AcceptReview` 调用全部经它或其返回值（`:130`）构造事件） | 返回类型与字面量 `github.ReviewEvent` → `vcs.ReviewEvent`；`Event: github.EventReviewComment` 保留（`Event` 是 `string`，常量照用） | **是** |
| `internal/runner/mergepoll_test.go:198`（期望 `ReviewParked`） | 字面量类型替换 | **是** |
| `cmd/lathe/queue_review_test.go:66`（期望 `ReviewQueued`） | 字面量类型替换 | **是** |
| `internal/httpapi/github_webhook_test.go:36`（`acceptCall.ev` 字段类型） | `github.ReviewEvent` → `vcs.ReviewEvent` | 否（事件由被测 handler 经 `ev.VCS()` 产生） |
| `internal/httpapi/github_webhook_test.go:46`（`fakeReviewIntake.AcceptReview` 签名） | 参数类型替换 | 否 |

3 处补字段都是新增一行，不改任何既有断言（AC-24 的「断言零删改」核对仍成立）；`github_webhook_test.go:179-182` 读的是
`Repo/PRNumber/Key/Body`，两种类型同名同型，不改。这 5 处计入 §3.3.2 签名表的测试改动合计。
`grep -rn 'github.ReviewEvent{\|AcceptReview(' --include=*_test.go internal cmd` 在 `internal/integration/github/` 之外只有上面这些
（`review_test.go` 其余 `AcceptReview` 调用都经 `inlineEvent` 或其返回值）；邻居 PRD（f01–f12）里没有任何一条构造
`ReviewEvent` 或调用 `AcceptReview`（`grep -n 'AcceptReview\|ReviewEvent\|TasksByPR' /opt/lathe-wt/prd/*.md`，f07 之外 0 命中），
所以必填 `Provider` 不给邻居留编译或语义接缝。`internal/integration/github/` 下三个既有测试文件**一行不改**（AC-1 不变）。

**接口形状**（沿用「方法就是消费方需要的那些」这一纪律，`internal/tracker/tracker.go:6-10`）：

```go
type Client interface {
    CreatePR(ctx context.Context, p PRParams) (*PullRequest, error)
    GetPRInfo(ctx context.Context, repoRef string, number int) (*PRInfo, error)
}

// 可选能力，保持现状（runner 用类型断言消费）：
//   不并进 Client，避免让每个测试假件都实现它（现状理由见
//   internal/runner/review.go:289-296）。三个实现都提供。
type ReviewFeedbackSource interface {
    ReviewFeedback(ctx context.Context, repoRef string, number int) (*ReviewFeedback, error)
}

// 远端（克隆/推送）不做成结构体：镜像键与 clone URL 是 repos 行上的纯函数
// （vcs.RepoKey/vcs.CloneURL，§3.3.1），GitAuth 由 creds.Clients.VCS 单独返回，
// 不与可打印的配置放在一起（§6）。
type Target struct {
    Provider Provider
    BaseURL  string // GitHub 为空
}
```

`Provider` 取值 `github | gitlab | gitea`；`SplitRef(provider, s)`：
GitHub 仍要求**恰好两段**（保持 `SplitRepo` 语义）；GitLab/Gitea 允许 ≥2 段（子组），
每段限 `[A-Za-z0-9._-]`、非空、不以 `.` 开头、总长 ≤ 200、拒绝 `..`。GitHub 侧
`SplitRepo` 保留为 `SplitRef(ProviderGitHub, …)` 的薄包装，错误文案不变（现有测试断言含
`owner/name` 字样，`internal/integration/github/github_test.go:45-55`）。

### 3.2 数据模型（迁移 0050–0052）

> v2 修订（评审 blocker 2/3/4/5）：**不再给 `tasks` 加列、不再动 `repos_user_repo_unique`**；
> 平台与实例地址落在 `repos` 行上，登记时写入、之后不可改，任务经 `repo_id` 继承。
> 理由与证据见 §9 评审答复 B2–B5。

**谁写、谁读（先回答 AGENTS §1.3）**：

| 列 | 唯一写入方 | 读取方 |
|---|---|---|
| `repos.vcs_provider` / `repos.vcs_base_url` | `POST /api/repos` → `store.CreateRepo`（`internal/store/query.go:455-479` 的 INSERT 增加两列）。`vcs_base_url` **不由请求体提供**，而是从该用户同平台凭据的 `integrations.base_url` 复制一份钉死 | `cmd/lathe/queue.go` `loadRepoConfig`、`runner.NewRepoLookup`（reaper 与 mergepoll 都经它）、`store.GetRepo/ListRepos`（→ `prd.StoreAdapter.RepoForPRD`、仓库页）、webhook 路由按仓库找实例 |
| `integrations.base_url` | `PUT /api/integrations/{gitlab,gitea}` | 保存时验证（`Verifier`）、登记仓库时复制、`resolveVCS`/`creds.Clients.VCS` 派发时与仓库钉死值比对 |

`UPDATE repos` 在非测试代码里只有一处（`internal/store/query.go:402`），本线**不把两列加进
它的 SET 子句**，`PUT /api/repos/{id}` 传这两个字段 → 400（§3.5.2）。所以「平台/实例不可改」
由「没有任何写路径」保证，而不是靠触发器（触发器是迁移里写业务逻辑，违反 AGENTS §6.4）。

**任务的平台从哪来（blocker 2）**：`tasks.repo_id` 在创建时钉死（`internal/task/machine.go:198-205`），
而仓库的平台/实例不可变 ⇒ `repo_id` 已经唯一决定了平台。三个建任务入口
（`cmd/lathe/queue.go:101` Linear、`:142` 内置工单、`internal/flow/service.go:238` 编排图）
**无需改动且天然正确**：它们写的是 `repo_id`，派发时两个 RepoConfig 加载器从 `repos` 读出
`vcs_provider/vcs_base_url`。v1 的 `tasks.vcs_provider` 是一份可能与 `repos` 不一致的冗余，
且确实没有写入方（评审指出的就是这个），删去。AC-19 改为经三条真实入口建任务验收。

#### 0050_repo_vcs_platform

```sql
-- 仓库所在的代码平台实例：(平台, 实例地址) 是一个属性，两列只在一起才有意义。
ALTER TABLE repos
  ADD COLUMN vcs_provider text NOT NULL DEFAULT 'github',
  ADD COLUMN vcs_base_url text;

ALTER TABLE repos ADD CONSTRAINT repos_vcs_platform_check CHECK (
  (vcs_provider = 'github' AND vcs_base_url IS NULL)
  OR (vcs_provider IN ('gitlab', 'gitea')
      AND vcs_base_url IS NOT NULL
      AND length(vcs_base_url) BETWEEN 8 AND 300)
);
```

- 存量行取 `'github'` + `NULL`，满足约束，零回填（AGENTS §6.2）。
- **`repos_user_repo_unique (user_id, provider_repo)` 不动**（blocker 4/5）。代价：同一用户不能把
  同一路径同时登记在两个平台（写进 §4 与 §6）。收益：9 个测试辅助函数的
  `ON CONFLICT (user_id, provider_repo)`（`internal/httpapi/api_test.go:27`、`flows_test.go:25`、
  `internal/runner/pipeline_test.go:1105`、`stacked_pr_pipeline_test.go:54`、`mergepoll_test.go:40`、
  `pipeline_profile_test.go:33`、`orchestration_m1_test.go:61`、`rebase_followup_test.go:121`、
  `internal/flow/service_test.go:80`）一行不改仍成立；`store.CreateRepo` 的 23505 注释
  「这里只可能是 (user_id, provider_repo)」（`internal/store/query.go:472`）仍然为真；
  f12 依赖的约束名与语义不变。
- 为什么两列一条迁移（AGENTS §6.6「一次迁移只做一件事」的口径）：这里的「一件事」是
  「仓库有平台实例」，两列由**同一条 CHECK** 绑定，拆成两条迁移会出现「有 gitlab 平台、没有
  实例地址约束」的中间版本。仓库内先例：`0016_flow_orchestration` 一次加 7 列服务一个意图、
  `0003_integration_secrets` 一次加 4 列 + CHECK、`0021:69-71` 列与其 CHECK 同条迁移。
- `repos` 是每用户几行的配置表，`ADD CONSTRAINT CHECK` 的扫描与短暂锁可接受，不涉及索引，
  无需 `CONCURRENTLY`。
- **down（完全撤销 up；有依赖数据时拒绝而非破坏）**：

```sql
-- 有 GitLab/Gitea 仓库时拒绝回滚：直接删列会把它们静默变成 github 仓库，下一次派发
-- 会拿 git@github.com:<同路径> 去推——这是错误平台推送，比回滚失败严重得多。
-- 也不能像 0028 那样 DELETE：repos 被 internal_issues/prds 以 ON DELETE RESTRICT 引用
-- （migrations/0021_internal_issues.up.sql:21、0023_prds.up.sql:25），且删仓库会级联删任务。
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM repos WHERE vcs_provider <> 'github') THEN
    RAISE EXCEPTION '0050 回滚被拒：仍有 GitLab/Gitea 仓库（%），请先在仓库页删除或改由人工处理',
      (SELECT string_agg(provider_repo, ', ') FROM repos WHERE vcs_provider <> 'github');
  END IF;
END $$;

ALTER TABLE repos DROP CONSTRAINT repos_vcs_platform_check;
ALTER TABLE repos DROP COLUMN vcs_base_url;
ALTER TABLE repos DROP COLUMN vcs_provider;
```

  空库/全 GitHub 库上 down 精确还原 up 之前的 schema；`RAISE EXCEPTION` 先例
  `migrations/0018_agent_events_result_index.up.sql:44`，`DO $$` 块由迁移切分器支持
  （`internal/store/migrate.go:248`、`:308-320`）。不涉及唯一约束，所以 v1 那个「恢复
  UNIQUE(user_id, provider_repo) 时撞重复」的问题不存在。

#### 0051_integration_base_url

```sql
ALTER TABLE integrations ADD COLUMN base_url text;
ALTER TABLE integrations ADD CONSTRAINT integrations_base_url_shape
  CHECK (base_url IS NULL OR length(base_url) BETWEEN 8 AND 300);
```

- 一件事：凭据有「实例地址」这一列（列 + 它自身的值域约束，同 `0021:69-71` 的形态）。
- down：`ALTER TABLE integrations DROP CONSTRAINT integrations_base_url_shape; ALTER TABLE integrations DROP COLUMN base_url;`
  此时 0052 已回滚、四类新凭据行已删，剩余行的 `base_url` 必为 NULL（API 对 github/linear 传
  baseUrl 返回 400），删列不丢信息。
- **不**为 `github` 强制 `base_url IS NULL`：将来接 GitHub Enterprise 时不至于改约束。

#### 0052_integration_vcs_kinds

```sql
ALTER TABLE integrations DROP CONSTRAINT IF EXISTS integrations_kind_check;
ALTER TABLE integrations ADD CONSTRAINT integrations_kind_check
  CHECK (kind IN (
    'linear', 'linear_webhook', 'github', 'github_webhook',
    'gitlab',          -- GitLab 个人访问令牌
    'gitlab_webhook',  -- GitLab 项目 webhook 的 Secret Token
    'gitea',           -- Gitea 访问令牌
    'gitea_webhook',   -- Gitea 仓库 webhook 的 Secret
    'linear_oauth', 'github_app', 'cloudrouter'   -- 既有预留
  ));

-- 新种类的定义的一部分：平台令牌必须带实例地址（webhook 密钥是投到我们这里的，不需要）。
ALTER TABLE integrations ADD CONSTRAINT integrations_vcs_base_url_required
  CHECK (kind NOT IN ('gitlab', 'gitea') OR base_url IS NOT NULL);
```

- 一件事：放开四个新凭据种类（含「这两类必须有地址」这条只对新种类成立的约束）。
  CHECK 只能整句重建，与 `0028` 同形。
- 与邻居的交集：`integrations_kind_check` 只有本线改。f08 明确不碰它（`f08-tracker-yunxiao.md:208`、`:381`），
  f09 单开 `notify_channels` 表（`f09-notify-channels.md:203`）。
- down：先 `DELETE FROM integrations WHERE kind IN ('gitlab','gitlab_webhook','gitea','gitea_webhook')`
  （同 `migrations/0028_github_webhook_secret.down.sql:1-8`：回滚必须删存量行，否则重建 CHECK
  失败；删掉的是可重新配置的凭据），再 `DROP CONSTRAINT integrations_vcs_base_url_required`、
  按 0028 的取值重建 `integrations_kind_check`。

#### 不新增表、不新增索引的理由

- 平台级信息全落在既有表的列上；review 回流沿用 `webhook_deliveries`（去重键带平台前缀）
  与任务事件流（意见去重按 Key），与现状同构（`docs/02-design.md:86`）。
- 镜像/工作区键变化只影响磁盘路径命名，不进库。

### 3.3 远端（克隆 / 推送）与鉴权

#### 3.3.1 实例地址从哪来、调用链怎么拿到（v2 blocker 3；v3 blocker 2 修订 clone URL 注入点）

**结论**：主机在**仓库登记时**从凭据复制进 `repos.vcs_base_url` 并钉死；之后所有需要镜像键或
clone URL 的地方都只读 `repos` 行，**不再碰凭据**。

- 登记：`POST /api/repos {vcsProvider:"gitlab", providerRepo:"g/sub/demo"}` → handler 用
  `a.Store.Secrets` 读当前用户的 `gitlab` 凭据行的 `base_url`（非密列，不解密）；没有该凭据 →
  400「请先在个人设置里配置 GitLab 令牌与实例地址」。有 → 把规范化后的地址写进
  `repos.vcs_base_url`。
- 派生：`runner.RepoConfig` 增加 `VCSProvider vcs.Provider` 与 `VCSBaseURL string` 两个**非密**字段，
  由两个加载器（`cmd/lathe/queue.go:656` `loadRepoConfig`、`internal/runner/mergepoll.go:30`
  `NewRepoLookup`）多 SELECT 两列填入；`prd.RepoInfo`（`internal/prd/service.go:70-74`）同样增加
  两字段，`prd.StoreAdapter.RepoForPRD`（`internal/prd/adapter.go:37-58`）从 `store.RepoRow` 抄过去。
  三者都只依赖 `repos` 行，**谁都不需要凭据访问**。
- 由此纯函数得到镜像键与 clone URL（`internal/vcs/remote.go`）：

```go
// RepoKey 与 CloneURL 只吃 repos 行上的三个非密字段，可在任何持有 RepoConfig 的地方调用。
func RepoKey(p Provider, baseURL, providerRepo string) string
func CloneURL(p Provider, baseURL, providerRepo string) string
```

  `RepoConfig.RepoKey()` / `RepoConfig.CloneURL()` 是它们的薄包装；`prd.RepoInfo` 同名方法亦然。

| 平台 | `RepoKey` | `CloneURL` |
|---|---|---|
| GitHub | `provider_repo`（**与今天逐字节相同**，现网 mirror 与在途 worktree 零迁移） | `git@github.com:<repo>.git`（不变） |
| GitLab | `<host[:port][/path-prefix]>/<provider_repo>`（如 `gitlab.example.com/acme/sub/demo`） | `<base>/<provider_repo>.git` |
| Gitea | 同上 | `<base>/<owner>/<repo>.git` |

- 镜像键：`MirrorPath(key string)` 的参数从「providerRepo」改为「RepoKey」。GitHub 的 `RepoKey()` 就是
  `ProviderRepo` 本身，所以所有 GitHub 调用点**传的值不变**；需要改的只是 GitLab/Gitea 能走到的生产
  调用点改传 `repoCfg.RepoKey()`：reaper（`internal/runner/reaper.go:484` Inspect、`:516` Discard）、
  `cmd/lathe/queue.go:480` Discard、`:631` Inspect、mergepoll（`:326`、`:417` MirrorPath）、pipeline
  （`:480` MirrorPath、`:1245` runHeavy 的 CreateDetached，入参从 `ProviderRepo` 改为 `RepoKey()`）、
  `Reattach`（`pipeline.go:678`）、PRD 规划（`internal/prd/service.go:521` CreateDetached）。
  这些调用点都已经持有仓库配置（`reaper.go:448`、`queue.go:428`、`mergepoll.go:171`、`params.Repo`、`spec.repo`）。

**clone URL 的注入点：保留 `ExecuteParams.CloneURL` 与 `CreateParams.CloneURL`（v3，blocker 2）**

v2 写的「三处拼接全部删除、`EnsureMirror` 从 `repo` 派生 URL」与测试现实冲突：`internal` 与 `cmd` 下
`*_test.go` 一共 128 处 `CloneURL:`（`grep -rn 'CloneURL:' --include=*_test.go internal cmd | wc -l`；
`pipeline_test.go` 18、`worktree_test.go` 17、`memory_test.go` 12、`stacked_pr_test.go` 8、
`rebase_followup_test.go` 8……共 26 个文件），几乎全部把**本地 fixture 仓库路径**作为远端注入；
如果 URL 从 `repo` 派生，GitHub fixture 会变成 `git@github.com:acme/demo.git`，这些测试全都克隆不到
本地的 src。v3 定下的规则：

| 字段 / 注入点 | 状态 | 谁写（生产） | 谁读 | 测试怎么注入本地远端 |
|---|---|---|---|---|
| `runner.ExecuteParams.CloneURL`（`pipeline.go:175`） | **保留，语义不变**（「这次执行用的远端」） | `cmd/lathe/queue.go:487` ← `loadRepoConfig` 返回值；`mergepoll.go:477` ← `cloneURLFor` | `pipeline.go:506`、`:618`（传给 `CreateParams`）、`:674` | 照旧填本地路径（128 处一行不改） |
| `runner.CreateParams.CloneURL`（`worktree.go:411`） | **保留** | `pipeline.go:618` 透传 | `worktree.go:436` → `EnsureMirror` | 照旧 |
| `EnsureMirror(ctx, key, cloneURL string)` 的 `cloneURL` | **保留第三个参数**，第二个参数改名为 `key` | 上面两条 + `mergepoll.go:342-343` + `prd/service.go:516` | 首次 clone / 每次刷新 | 照旧 |
| `prd.Service.CloneURL func(providerRepo string) string`（`prd/service.go:111`） | **保留，改签名**为 `func(RepoInfo) string`（GitLab 需要 `VCSBaseURL`） | 生产不设（nil → 默认 `RepoInfo.CloneURL()`） | `prd/service.go:170` | `service_test.go:257` 一行跟改 |

改的只是**三处拼接的实现**（单一真相源），不是注入点：

- `cmd/lathe/queue.go:686` 的 `return cfg, "git@github.com:" + providerRepo + ".git", nil` →
  `return cfg, cfg.CloneURL(), nil`（`loadRepoConfig` 签名不变，`:428`、`:604` 两个调用点不改）；
- `mergepoll.go:358-360` 的 `cloneURLFor(providerRepo string)` → `cloneURLFor(repo RepoConfig) string { return repo.CloneURL() }`，
  两个调用点（`:342`、`:477`）把 `repoCfg.ProviderRepo` 改为 `repoCfg`；
- `prd/service.go:169-174` 的默认分支 → `return repo.CloneURL()`。

GitHub 仓库上 `RepoConfig.CloneURL()` 返回的正是 `"git@github.com:" + ProviderRepo + ".git"`（AC-5 的
`TestGitHubRemoteUnchanged` 钉死），所以三处的生产输出对 GitHub 逐字节不变。

**「GitHub 路径逐字节不变」重新论证（AC-1/AC-24 的前提）**：v3 里它成立靠的是以下四点，每点都可机械核对：

1. GitHub 的 `RepoKey()` == `ProviderRepo`，`CloneURL()` == 原来的拼接字符串（AC-5）——生产调用点传给
   `MirrorPath/EnsureMirror` 的值与 `cff5332` 相同。
2. 128 处测试注入不改，它们继续走「本地路径 + 零值 `GitAuth`」，即原有的 `m.git()` 路径。
3. 零值 `GitAuth` 时 `EnsureMirror/Push/ForcePush/RebaseOnto/Reattach` 的执行序列与 `cff5332` 完全一致
   （同样的 git 子命令、同样的 `origin`、同样的环境）；新的带鉴权分支只在 `auth.IsZero()==false` 时走，
   GitHub 永远不走（`resolveVCS` 对 GitHub 只调 `Clients.GitHub` 并返回零值 `GitAuth`，§3.4）。AC-5c 用记录 argv 的假 `git` 对 GitHub 仓库断言这一点。
4. `internal/integration/github/` 下三个既有测试文件零改动（AC-1）。

所以 v3 的「逐字节不变」是指：**GitHub 仓库的生产调用产生的 git argv 与环境不变、API 请求不变**。
注入点签名会变（下面列了），但对 GitHub 而言是同值透传。

**`base_url` 改了怎么办**：仓库上钉死的是登记那一刻的地址，凭据上的地址事后被改，
**在途 worktree 的 Mirror/Inspect/Discard 与 review Reattach 仍按仓库行上的旧键运作**，不会找错
mirror（键只从 `repos` 来）。会变化的只有「拿 token 去推/拉」这一步：派发时 `resolveVCS`/`creds.Clients.VCS`
（§3.4）比对 `credential.base_url` 与 `repoCfg.VCSBaseURL`，**不一致即拒绝**并以可读错误失败
（「仓库登记在 https://a，而你的 GitLab 凭据现在指向 https://b；token 不会发往与仓库不同的
实例。请改回凭据地址，或删除并重新登记仓库」）。理由：把 A 实例的 token 发给 B 实例是凭据外泄，
静默跟随新地址会让在途任务在一个从未克隆过的实例上推分支。**落点（v4，第三轮 blocker 2 修订）**：
`setup` 里只**记下**这个错误（`rc.vcsErr`），不返回——`setup` 失败时任务留在 `queued`，而 `queued`
没有 `→failed` 边；真正的 `p.fail` 发生在各入口阶段**第一次合法状态转移之后、任何带 token 的操作之前**，
全程只走 `state.go` 已有的边，见 §3.4.1。mergepoll 的 `pollTask` 遇到不一致则报错并跳过该任务本轮
（`pr_open` 状态不变）；rebase 跟进遇到不一致走既有的 `failRebaseFollowup`，见 §3.4.2。

#### 3.3.2 克隆 URL 鉴权与 git 子进程加固（v3，blocker 1 重写）

**威胁（已在本机复现）**：worktree 的 `.git` 是指向 mirror 的 gitdir 文件，mirror 的 `config`、
`hooks/`、`info/` 对 agent 全部可写。v2 的思路是「仍在 mirror 上跑带鉴权的 git，用 `GIT_CONFIG_COUNT`
注入的配置覆盖仓库 config」——**这个前提不成立**：

- `http.<url>.*` 按 URL 匹配，**更具体的 URL 优先，与配置来源无关**。agent 写
  `http.https://gitlab.example.com/g/x.git.proxy=http://127.0.0.1:<agent 监听>` 与同前缀的
  `sslVerify=false`，环境注入的 `http.https://gitlab.example.com.sslVerify=true` 与空 `http.proxy` 都盖不住：
  `git config --get-urlmatch` 仍返回 `false` 与 agent 的代理（`prd-v3-urlmatch-bypass-probe.log` A1）。
  token 会以 Basic auth 经 agent 的代理发出、TLS 不校验。
- `include.path` 引入的 `http.sslCAInfo` / `http.proxy` 生效（同上 A1、B0）。
- `url.<x>.insteadOf` 会改写**显式写在命令行上的 URL**（A2：显式 `https://gitlab.example.com/g/x.git`
  被改写到 `evil.example.com`），v2 的「显式 URL」也挡不住；主机限定的 helper 让 token 不会发给改写后的主机，
  但代理劫持（上一条）不需要改主机。
- 新版 git（镜像 `alpine:3.21`，git 2.47）还有 `http.curloptResolve`，可以把钉死的主机名解析到 agent 的地址；
  本机 2.34 没有这个键（A3），但这说明「逐键列黑名单」本身不可行：每个 git 版本都可能加新的网络相关键。

对照实测（B0）：v2 形态在 mirror 上 push 显式 URL，agent 的「代理」收到了 receive-pack 请求。

**v3 设计：带鉴权的 git 调用根本不读 agent 可写的任何 config**（评审建议的第一种做法）

把「与远端通信」从 mirror 里拿出来，放进 Lathe 私有的 **transport 仓库**：

1. **私有 transport GIT_DIR**：每次带鉴权的操作 `os.MkdirTemp("", "lathe-git-transport-*")`（0700，位于
   系统临时目录，**不在** `WorkspaceRoot` 之下，agent 的工作目录与 `--add-dir` 都碰不到；与
   `prd.Service.NeutralDir` 选系统临时目录同一立场），里面只建 `HEAD` 与 `refs/`，**不写 config 文件、
   没有 hooks 目录**。用完 `defer os.RemoveAll`。
2. **对象直接读写 mirror 的对象库**：`GIT_OBJECT_DIRECTORY=<mirror>/objects`。fetch 下来的对象落在 mirror，
   push 时从 mirror 读对象。对象是内容寻址的，agent 在对象库里塞的东西只能是「多几个对象」，无法改变 Lathe
   按 sha 指定要推的内容。
3. **配置来源只有 Lathe**：环境用白名单从零构造（不 `os.Environ()`）：`PATH`、`HOME=<transport>/home`（空目录）、
   `GIT_DIR`、`GIT_OBJECT_DIRECTORY`、`GIT_CONFIG_NOSYSTEM=1`、`GIT_CONFIG_GLOBAL=/dev/null`、
   `GIT_TERMINAL_PROMPT=0`、`GIT_ASKPASS=`、`GIT_PROTOCOL_FROM_USER=0`，再加 `GIT_CONFIG_COUNT` 下列键：

| 键 | 值 | 作用 |
|---|---|---|
| `core.hooksPath` | `/dev/null` | transport 本来没有 hooks；防御性再关一次 |
| `credential.helper` | 空串 | 清空 helper 列表 |
| `credential.<base>.helper` | fd 读取 helper | 只对仓库钉死的实例给口令 |
| `credential.useHttpPath` | `false` | 同实例内不按路径区分 |
| `protocol.allow` / `protocol.<scheme>.allow` | `never` / `always` | 只放行 base 的 scheme（https；loopback 联调为 http） |
| `core.fsmonitor` | `false` | 防御性 |
| `gc.auto` | `0` | **v5（第四轮 blocker 1）**：关掉 transport 语境下的 `gc --auto`。见下方「transport 里不得跑 gc」 |
| `maintenance.auto` | `false` | 同上，新版 git 的 fetch 结束时跑的是 `maintenance --auto` |

   另外 transport 里的每条 `fetch` 在 argv 上带 `--no-auto-maintenance`（2.34 与 2.47 都认；`--no-auto-gc` 是它的旧别名），
   与上面两个键互为冗余：键管住所有子命令，argv 管住「某个版本的 fetch 不按配置判断」这一类意外。

   **transport 里不得跑 gc（v5，第四轮 blocker 1；v4 的数据丢失缺陷）**：transport 的 `GIT_DIR` 只有 `HEAD` 与 transport
   自己的 ref，但 `GIT_OBJECT_DIRECTORY` 是 mirror 的对象库。`git fetch` 结束时默认跑 `gc --auto`（松散对象 > `gc.auto`=6700 或
   pack 数 > `gc.autoPackLimit`=50 时触发），在 transport 语境下它的可达性根只有 transport 的 ref，**看不到** mirror 的
   `refs/heads/<任务分支>`、各 worktree 的 `HEAD`/`index`、reflog，于是把 mirror 对象库里只被这些引用、且老过
   `gc.pruneExpire`（默认 2 周）的对象删掉——在途任务未推送的提交就没了。本机 git 2.34.1 复现：
   `/opt/lathe-wt/evidence/f07/prd-v5-transport-gc-negotiation-probe.{sh,log}` 的 `MODE=v4`（任务提交 mtime 设为 30 天前、
   transport 里只加 `gc.autoPackLimit=2` 与 `gc.autoDetach=false` 两个加速器，3 次刷新后 `cat-file -t <任务提交>` →
   `could not get object info`、`git -C wt log` → `fatal: bad object HEAD`）；`MODE=gconly`/`v5`（加了 `gc.auto=0`、
   `maintenance.auto=false`，同样的加速器）任务提交、blob、worktree HEAD 全部完好、`fsck --connectivity-only` 0 异常。
   加速器 `gc.autoDetach=false` 只是让 gc 在前台跑完：默认的后台 gc 与「用完即删 transport 目录」赛跑，结果不确定，**不能当作安全**。

   **测试注入口（v6，第五轮 blocker 1）**：transport 不读 mirror config（没有 repo config 文件，system/global 指向 `/dev/null`，
   配置来源只有 command line），所以「把加速器写进 mirror config」对 transport 里的 fetch **不起作用**——它用的是 git 默认阈值
   （`gc.auto=6700`、`gc.autoPackLimit=50`、`gc.autoDetach=true`），测试里 3 次小增量既不会触发 `gc --auto`，触发了也转后台，
   回归形态下照样绿（`prd-v6-ac5d-accelerator-probe.log` 的 `MODE=v4 ACCEL=mirror`：任务提交完好、fsck 0、transport 里没跑过 gc）。
   要在测试里把 transport 语境下的 gc 逼出来，加速器必须进 **transport 的 `GIT_CONFIG_COUNT`**。为此 `worktree.go` 留两个
   **仅供测试**的包内变量（生产恒为零值，不由任何配置项、环境变量或 API 设置，所以不算 AGENTS §1.3 的「可配置字段」）：
   - `var transportExtraConfig [][2]string`：`transport()` 构造 `GIT_CONFIG_COUNT` 时**排在 Lathe 固定键之前**。同名单值键以后出现的为准，
     所以固定键（`gc.auto=0`、`maintenance.auto=false`、`protocol.*` …）永远压过注入口；另外 `transport()` 对注入口做键白名单
     `{fetch.unpackLimit, gc.autoPackLimit, gc.autoDetach}`，出现其他键直接 panic（`credential.*` 这类多值键不靠「后者胜出」，靠白名单挡住）。
     注入口不能削弱加固，只能在固定键缺席时（回归形态）显出效果。AC-5d 用它注入 `fetch.unpackLimit=1`（种了 have 的小增量本来落成松散对象，
     不增 pack 数；改成落 pack 才能让下一条生效）、`gc.autoPackLimit=1`、`gc.autoDetach=false`（前台跑完，结果确定）。
   - `var transportBeforeRemove func(dir string)`：`transport()` 在 `os.RemoveAll` 之前调用（nil 则跳过），让测试在目录被删之前
     检查 transport 里有没有 gc 的痕迹。gc 会顺带 `pack-refs`，在 transport 里留下 `packed-refs`（运行中还有 `gc.pid`）；
     Lathe 自己在 transport 里只做 `update-ref --stdin`（种 have）与 `fetch`/`push`，都写松散 ref，不产生 `packed-refs`。探针实测：
     transport 里跑过 gc 的三种回归形态每次刷新都出现 `packed-refs`，v5 形态 0 次。
   **不用 PATH 前置的记录型 `git` 包装观测 gc**：git 派生内部子命令（`gc --auto`、`maintenance run --auto`）时，`setup_path()` 会把
   exec-path（`/usr/lib/git-core`）放到 `PATH` 最前，子进程不经过包装器。探针里 transport 中确实跑了 gc、删了任务提交的那几次，
   包装记录里 `gc`/`maintenance` 仍是 0 条——「包装器没看到 gc」恒真，证明不了任何事（`prd-v6-ac5d-accelerator-probe.log` 各段末尾）。

   **mirror 自己的维护换到 mirror 语境里做**：v4 之前（以及 GitHub 原路径）`fetch` 是在 mirror 上跑的，顺带的 `gc --auto` 看得见
   全部 ref 与 worktree，是安全的。transport 关掉 gc 后，GitLab/Gitea 的 mirror 要由 Lathe 显式补上：每次 `importRefs` 成功后
   `m.git(ctx, mirror, "-c", "core.hooksPath=/dev/null", "gc", "--auto", "--quiet")`——本地、无 token、无网络、仍在 mirror 锁内，
   可达性根与原路径一致（探针在这一步换成最激进的 `gc --prune=now`，任务对象仍在）。它读 mirror config（agent 可写 `gc.*`），
   这与 GitHub 原路径 `fetch` 顺带的 gc 是**同一个存量暴露面**，没有 token 参与，不在本线扩大。

   **刷新必须是增量的（v5，第四轮 blocker 2；v4 的全量重传缺陷）**：v4 的 transport 没有任何 ref，fetch 协商发不出 `have`，
   服务端只能回全量 pack；对象库只增不减，pack 数也更快逼近上一条的 gc 阈值。探针 `MODE=gconly`（约 2MB、200 个文件的仓库，
   上游每次只改一个小文件）：mirror 对象库 2012K → 3984K → 5956K → 7928K，每次新增约 206–212 个对象条目，而上游只新增 3 个
   （与评审实测的 3.5M → 5.4M → 7.4M 同形）。改法：**fetch 前在 transport 里种 have**——
   `git --git-dir=<mirror> for-each-ref --format='create refs/lathe-have/%(refname:lstrip=3) %(objectname)' refs/remotes/origin/`
   的输出经 `git update-ref --stdin` 写进 transport（transport 环境、**不挂 fd 3、不联网**；对象本来就在共享对象库里，只是补上 ref）。
   放在独立命名空间 `refs/lathe-have/*` 而不是 `refs/remotes/origin/*`：后者会被 fetch 原地更新，上游删掉的分支在 transport 里
   仍然留着种进去的旧值，`importRefs` 的 prune 就失效了。`importRefs` 只读 transport 的 `refs/remotes/origin/*`，与种 have 互不干扰。
   探针 `MODE=v5`：每次刷新新增对象条目 = 3、对象库每次 +24–28K、pack 数恒为 1。只种 `refs/remotes/origin/*`、不种任务分支
   `refs/heads/*`：后者会把**未推送**提交的 sha 作为 `have` 发给远端，没有必要。不选 `--negotiation-tip`：它只从**已有** ref 里挑
   协商起点，transport 里没有 ref 时同样发不出 have，仍然要先种。
   `Push/ForcePush` 不需要种：push 时客户端按服务端广告的 ref 计算差集，广告里的对象在共享对象库里都找得到，只发缺的
   （原型 `prd-v3-design-e2e-probe.log` 第 3、5 节的推送已经是增量的）。

   因为既没有 system/global，也没有 repo config 文件，`git config --list --show-origin` 在这个环境里**只有
   `command line:` 一种来源**（`prd-v3-configless-transport-probe.log`、`prd-v3-design-e2e-probe.log` 第 6 节）。
   这是一条**与 git 版本无关**的总断言：不管将来 git 加了什么网络键（`curloptResolve` 之类），agent 都没有
   任何可写的位置能把它送进带鉴权的调用里。`include.path`、`url.*.insteadOf`、`http.<url>.*` 一并失效。
   注意 `HTTP_PROXY/HTTPS_PROXY` 也**不**进白名单：带 token 的流量不经任何代理（与 §6 的代价一起登记）。
4. **token 只经继承的管道 fd**（v2 已有，不变）：`exec.Cmd.ExtraFiles` 放只含 token 一行的管道读端（fd 3），
   helper 只在 `get` 时读出；token 不进任何进程环境。
5. **远端 URL 只来自 `repos` 行**：命令行上显式写 `RepoConfig.CloneURL()`；transport 里没有 `remote.origin`，
   也就不存在「agent 改 `remote.origin.url`」这条路。

**各带鉴权操作的形态**（全部在 mirror 锁内；只在 `!auth.IsZero()` 时走，GitHub 零值走原路径）：

| 操作 | transport 内（带 token、联网） | 回到 mirror（本地、无 token、无网络） |
|---|---|---|
| `EnsureMirror` 首次 | — | Lathe 自己 `git init --bare <mirror>`，写 `remote.origin.url=<cloneURL>`、`remote.origin.fetch=mirrorFetchRefspec`（**不**设 `remote.origin.mirror`，所以 v2 的 `--unset-all` 那一步在这条路径上天然不需要） |
| `EnsureMirror` 刷新 / `RebaseOnto` 与 `Reattach` 内的 fetch | 先种 have（本地 `update-ref --stdin` 写 `refs/lathe-have/*`，无 token、无网络），再 `git fetch --no-auto-maintenance <cloneURL> +refs/heads/*:refs/remotes/origin/*` | 读 transport 的 `refs/remotes/origin/*`，经 `git update-ref --stdin` 写入 mirror（含 prune：mirror 有而 transport 没有的 `refs/remotes/origin/*` 删除）。语义与原 `fetch --prune origin` 相同，命名空间仍只动 `refs/remotes/origin/*`（任务 #494 的不变量）；随后在 mirror 语境下 `gc --auto`（见上） |
| `Push` | `git push <cloneURL> <sha>:refs/heads/<branch>`（sha 由 Lathe 在 worktree 里 `rev-parse` 得到） | `update-ref refs/remotes/origin/<branch> <sha>`；`branch.<b>.remote=origin`、`branch.<b>.merge=refs/heads/<b>`（等价于原 `--set-upstream` 的可观察状态，探针第 2 节 `@{u}` = `origin/feat/x`） |
| `ForcePush` | `git push --force-with-lease=refs/heads/<b>:<mirror 记录的 refs/remotes/origin/<b>> <cloneURL> <sha>:refs/heads/<b>` | 同 Push |

   - `--force-with-lease` 必须带**显式期望值**（transport 里没有 remote-tracking ref 可隐式取），期望值就是
     原实现隐式使用的那个值（`RebaseOnto` 刷新后的 `refs/remotes/origin/<b>`），语义不变。refspec **不能带 `+`**：
     `+` 会让该 ref 无视 lease 直接强推——原型首轮就踩到了，已写进探针注释（`prd-v3-transport-ops-probe.log` 第 3 节：
     期望值匹配时成功，远端被别人改过时 `! [rejected] (stale info)`）。
   - 受保护分支闸门不动：`ValidatePushTarget` 与基线检查仍在 `Push/ForcePush` 入口、在任何 git 调用之前（AC-18）。
   - 本地的 `rev-parse`、`update-ref`、`config` 等**无鉴权**调用仍走 `m.git()`，会照常读 mirror config、
     执行 mirror hooks（例如 `reference-transaction`）——这是**存量**行为，那些调用里没有 token，环境里也没有
     （原型实测：hook 记录 18 次，全部发生在本地调用，transport 期间 0 次，hook 环境里 token 出现 0 次）。
     Lathe 本地写 mirror 时额外带 `-c core.hooksPath=/dev/null`，不给 agent 的 hook 白送执行时机。

端到端原型（= AC-5b 的 shell 版）：`/opt/lathe-wt/evidence/f07/prd-v3-design-e2e-probe.{sh,log}`。
agent 在 worktree 里写了最具体 URL 的 `proxy`/`sslVerify`、`include.path`（`sslCAInfo`+`proxy`）、
`insteadOf`、两种 `credential.helper`、`remote.origin.url` 与三个 hook 之后，首次建 mirror / fetch / push /
force-with-lease 全部成功落在原远端：agent 代理收到 0 个连接、恶意 helper 未执行、远端收到带
`Basic base64(oauth2:tok-…)` 的请求。

**GitHub 路径不加这套**（理由同 v2：GitHub 走宿主 SSH，git 子进程里没有 Lathe 的 token；hook 可执行代码是存量
暴露面，写进 §6，不在本线修）。

**代码形态**：`vcs.GitAuth` 不透明，`String()/Format()` 返回 `[REDACTED]`，`IsZero()`；
`worktree.go` 新增 `transport(ctx, mirror, auth, args...)`（建私有目录、构造白名单环境与 ExtraFiles、跑完删目录）
、`seedHaves(ctx, mirror, transportDir)`（v5）与 `importRefs(ctx, mirror, transportDir)`（末尾跑 mirror 语境的 `gc --auto`，v5）；
两个仅供测试的包内变量 `transportExtraConfig`（排在固定键之前）、`transportBeforeRemove`（删目录前回调），见上「测试注入口」（v6）；
现有 `git()` 一行不改。`transport()` 对 `fetch` 子命令**自己**插入 `--no-auto-maintenance`，调用方不必记得（AC-5d 断言 argv）。

**签名变化与影响面**（按 cff5332 实测计数：`grep -rn '<方法>(' --include=*_test.go internal cmd | grep -v 'func '`，排除方法定义行；
`Create` 一行按 AST 计数，见下）：

| 方法 / 结构 | 新签名 | 生产调用 | 测试调用 | 测试怎么改 |
|---|---|---|---|---|
| `EnsureMirror` | `(ctx, key, cloneURL string, auth vcs.GitAuth)` | 6（`pipeline.go:506/674`、`worktree.go:436`、`mergepoll.go:343`、`prd/service.go:516`、`prdwiring.go:26`）+ `prd.Worktrees` 接口声明 1 | 6（`worktree_test.go:60/69/77/106`、`push_test.go:34`、`stack_pipeline_test.go:28`）+ `prd/service_test.go:184` 假件 | 末尾补 `vcs.GitAuth{}`，第二、三参不变 |
| **`Create`（v4 补漏，第三轮 blocker 1）** | **签名不变** `(ctx, CreateParams)`；`CreateParams` 新增字段 `Auth vcs.GitAuth`，`worktree.go:436` 改为 `m.EnsureMirror(ctx, p.Repo.RepoKey(), p.CloneURL, p.Auth)` | 1（`pipeline.go:617`，字面量补 `Auth: rc.auth`） | 49 处调用、46 个 `CreateParams{…}` 字面量，**0 处要改**：46 个字面量全部是键值形式（AST 实测 `keyed=46 unkeyed=0`），新字段取零值 = 原无鉴权路径 | — |
| `Push` | `(ctx, wt, repo, auth, onProgress)` | 1（`pipeline.go:1124`） | 17 | 补 `vcs.GitAuth{}` |
| `ForcePush` | `(ctx, wt, repo, auth, onProgress)` | 1（`mergepoll.go:434`） | 3（`rebase_followup_worktree_test.go:173/206/209`） | 同上 |
| `RebaseOnto` | `(ctx, wt, oldBaseTip, newBaseBranch string, cloneURL string, auth)` | 1（`mergepoll.go:429`） | 2（`rebase_followup_worktree_test.go:78/136`） | 补本地路径 `src`（fixture 已有）+ 零值；零值时 `cloneURL` 不使用 |
| `Reattach` | `(ctx, key, path, branch, cloneURL string, auth)` | 1（`pipeline.go:678`） | 3 | 同上 |
| `MirrorPath/Inspect/Discard/CreateDetached/Prune/List` | 第一个参数语义改为 key，**类型不变** | 见 §3.3.1 | 0 处需改（测试传的就是 `ProviderRepo`，对 GitHub fixture 即 key） | — |
| `ExecuteParams` | 不变（`CloneURL` 保留，见 §3.3.1；**不加** auth——auth 在 `setup` 里按属主现取，见 §3.4.1） | — | 128 处 `CloneURL:` 不改 | — |

测试侧机械改动合计仍是 **32 处**（6+1+17+3+2+3），`Create` 的 49 处调用一处不改。全部是「末尾补一个零值参数」或
「补 fixture 已有的 src 路径」，没有任何测试的断言改动。

**另计（v5，第四轮 blocker 3）**：`AcceptReview` 改吃 `*vcs.ReviewEvent` 与 `Provider` 必填带来 5 处测试改动，其中 3 处补一行
`Provider: vcs.ProviderGitHub`（`review_test.go:73-79` 的 `inlineEvent`、`mergepoll_test.go:198`、`queue_review_test.go:66`），2 处只换类型
（`github_webhook_test.go:36/46`），明细见 §3.1 表。**全部测试侧改动 = 本表 32 处 + §3.1 的 5 处 = 37 处**，仍没有任何断言改动。`Create` 的计数证据：
`/opt/lathe-wt/evidence/f07/prd-v4-createparams-keyed-literals.{go,log}`（`go/ast` 扫 `internal/runner` 与 `cmd/lathe`：
`runner.CreateParams literals: keyed=46 unkeyed=0; WorktreeManager.Create-shaped calls=49`）。评审估的「约 88 处」
是 `grep 'Create('` 的数字，混入了 `task.Machine.Create`（`task.CreateParams`）与 `Users.Create` 等无关调用。

**为什么 auth 进 `CreateParams` 却不进 `RepoConfig`（v4 修订 v3 的一刀切说法）**：v3 写的「auth 不放进
`RepoConfig`/`CreateParams`」把两种结构混为一谈，结果是 `Create` 拿不到鉴权（评审 blocker 1 属实）。区别在于生命周期：

- `RepoConfig` 是**被到处复制的配置**：两个加载器产出、塞进 `ExecuteParams`、被 `RepoLookup` 返回给 reaper/mergepoll、
  被 `execRepo := repoCfg` 复制（`mergepoll.go:471`），日志里也打印过它的字段。秘密放进去，就要审计它所有的
  复制与打印路径。**仍然不放。**
- `CreateParams` 是 **`Create` 一次调用的入参**：生产只在 `pipeline.go:617` 构造一次，作为值传进 `Create` 后不保存、
  不返回（`Worktree` 结构不含它）、不进任何日志（`grep` `CreateParams` 与 `Sprintf/slog/%v` 同行零命中）。它和
  `EnsureMirror` 的 `auth` 参数是同一种东西，只是因为 `Create` 本来就用结构体传参。
- 兜底：`vcs.GitAuth` 自带 `String/GoString/Format/LogValue`，嵌在父结构里被 `%v/%+v/%#v/%s`、`slog` 文本与 JSON
  handler、`encoding/json` 打印时都不出明文（字段不导出，JSON 为 `{}`）。实测：
  `/opt/lathe-wt/evidence/f07/prd-v4-createparams-auth-redaction-probe.{go,log}` → `RESULT: NO-LEAK`。AC-5 增加同形断言。

替代方案及不选的理由：①改 `Create` 签名为 `(ctx, CreateParams, auth)`——49 处测试调用全要跟改，零收益；
②让 `Create` 不调 `EnsureMirror`、由 pipeline 先调——改变 `Create` 的既有契约（「更新 mirror → 建 worktree」，
`worktree.go:425`），worktree 测试依赖它首次建 mirror；③`WorktreeManager` 持有一个按 key 查 auth 的回调——
把属主解析塞进一个与用户无关的组件，且并发任务共享同一个 manager，否决。

**「永不 push 受保护分支」在新平台的落地方式**：不改 `ValidatePushTarget`
（`internal/runner/branch.go:108-121`），它只处理分支名；`Push/ForcePush` 入口的闸门在选择 transport 或原路径
**之前**，三个平台走同一段代码；新增的只是「三个平台都有一份同样的拒绝测试」（AC-18）。

### 3.4 `Clients` 接口与客户端分派（v3，blocker 3：与 f09 的编译期兼容）

v2 把 `runner.Clients.GitHub(ctx) (GitHubAPI, error)` 改成 `VCS(ctx, target)`、并把 `runCtx.gh` 改名。
f09 的新测试 `e2eClients`（`f09-notify-channels.md:1125-1127`）正是只实现 `Tracker` 与 `GitHub`、返回
「嵌入 `runner.GitHubAPI` 的空结构体」。两条线各自能单独合入 dev，但后合的那条会让 dev 编译失败。
v3 的兼容策略：**`runner.Clients`、`runner.GitHubAPI`、`runCtx.gh` 三个名字与方法集保持 `cff5332` 原样，
新平台走一个可选接口**。

```go
// 不变（cff5332 原样）：
type GitHubAPI interface {
    CreatePR(ctx context.Context, p github.PRParams) (*github.PullRequest, error)
    GetPRInfo(ctx context.Context, providerRepo string, number int) (*github.PRInfo, error)
}
type Clients interface {
    Tracker(ctx context.Context, provider string) (tracker.Tracker, error)
    GitHub(ctx context.Context) (GitHubAPI, error)
}

// 新增：可选能力，与 ReviewFeedbackSource 同一个「类型断言取」形态（review.go:289-302）。
type VCSClients interface {
    VCS(ctx context.Context, target vcs.Target) (GitHubAPI, vcs.GitAuth, error)
}

// 唯一的分派入口（pipeline setup 与 mergepoll pollTask 都调它）：
func resolveVCS(ctx context.Context, c Clients, t vcs.Target) (GitHubAPI, vcs.GitAuth, error) {
    if t.Provider == vcs.ProviderGitHub {           // 含存量行的默认值
        gh, err := c.GitHub(ctx)                    // 与 cff5332 完全同一调用
        return gh, vcs.GitAuth{}, err
    }
    v, ok := c.(VCSClients)
    if !ok {
        return nil, vcs.GitAuth{}, fmt.Errorf("runner: 客户端不支持代码平台 %q", t.Provider)
    }
    return v.VCS(ctx, t)
}
```

- 因为 `github.PRParams/PullRequest/PRInfo` 是 `vcs` 同名类型的**别名**（§3.1），`GitHubAPI` 与 `vcs.Client`
  方法集完全相同、可以互相赋值；`VCS` 返回 `GitHubAPI` 而不是 `vcs.Client` 只是为了 `runCtx.gh` 字段类型不变，
  两者在编译器眼里是同一个方法集（编译探针：`/opt/lathe-wt/evidence/f07/prd-v3-clients-compat-compile-probe.log`，
  含 `var _ vcs.Client = GitHubAPI(nil)` 与 `var _ GitHubAPI = vcs.Client(nil)` 两个方向的断言，以及照
  f09 形态写的 `e2eClients` 照常编译）。
- **GitHub 路径的调用序列不变**：GitHub 仓库仍是 `clients.GitHub(ctx)` 一次调用，失败文案
  「获取 GitHub 客户端失败（请在设置里配置并验证凭据）」不变。
- `creds.Clients`（`internal/creds/verifier.go`）**新增** `VCS` 方法，`GitHub` 方法（`:128`）一行不改：按
  `target.Provider` 取 `gitlab|gitea` 凭据；凭据行不存在或无 `base_url` 返回 `vcs.ErrCredentialMissing`；**比对凭据
  `base_url` 与 `target.BaseURL`，不一致返回 `vcs.ErrInstanceMismatch`**（§3.3.1）；一致才构造 API client 与 `GitAuth`。
  两个哨兵定义在 `internal/vcs/vcs.go`，是 §3.4.1 区分「确定性配置错误（落 failed）」与「暂时性错误（留 queued）」的唯一依据。
- 调用方（v4 逐一列全，第三轮 blocker 2/3）：pipeline `setup`（§3.4.1）、mergepoll 的 `pollTask` 与 rebase 跟进
  （§3.4.2）、PRD 规划路径（§3.4.3，经 `cmd/lathe` 的适配器，`prd` 包不依赖 `runner`）。`runCtx.gh` **不改名**（`pipeline.go:212`）。
  需要带鉴权的网络调用**全集**（`grep -n '"fetch"\|"push"\|"clone"' internal/runner/*.go` 的 5 处 + 它们的上层入口）与 auth 来源：

| 带网络的 git 调用 | 上层入口（生产） | 谁的凭据 | auth 从哪来 |
|---|---|---|---|
| `EnsureMirror` 首次/刷新（`worktree.go:104/121`） | `Create`（`pipeline.go:617`） | 任务属主 | `CreateParams.Auth = rc.auth`（§3.3.2） |
| 同上 | `setup` 续跑刷新（`pipeline.go:506`）、Rebuild 重挂（`:674`） | 任务属主 | `rc.auth` |
| 同上 | `onMerged → resolveMirrorTip`（`mergepoll.go:306/343`） | **已合并任务**属主 | `pollTask` 解出的 auth 经 `handleMerged → onMerged → resolveMirrorTip` 透传（§3.4.2） |
| 同上 | `prd.Service.runAgent`（`prd/service.go:516`） | **PRD 属主** | 新字段 `prd.Service.GitAuth`（§3.4.3） |
| `RebaseOnto` 内 fetch（`worktree.go:214`） | `rebaseFollowupOne`（`mergepoll.go:429`） | **后继任务**属主 | `rebaseFollowupOne` 内 `ClientFactory.ForUser(succ.UserID)` + `resolveVCS`（§3.4.2） |
| `ForcePush`（`worktree.go:253`） | `rebaseFollowupOne`（`mergepoll.go:434`） | 后继任务属主 | 同上，同一个 auth |
| `Push`（`worktree.go:186`） | `stagePushAndPR`（`pipeline.go:1124`） | 任务属主 | `rc.auth` |
| `Reattach` 内 fetch（`worktree.go:556`） | `stageImplement` Rebuild（`pipeline.go:678`） | 任务属主 | `rc.auth` |
| （重验）`Pipeline.Execute` | `rebaseFollowupOne`（`mergepoll.go:474`） | 后继任务属主 | `Execute → setup` 自己按 `tk.UserID` 解析（与派发同一路径，无需 mergepoll 传入） |

#### 3.4.1 pipeline：凭据错误在哪一刻落 `failed`（v4，第三轮 blocker 2）

**事实**：`setup` 失败时 `Execute` 直接 `return err`（`pipeline.go:298-301`），此时 `rc` 还不存在，也调不了 `p.fail`；
注释写明 setup 失败「不转 failed、任务留在 queued」（`:400-401`）；`state.go:90` 里 `queued` 没有 `→failed` 边。v3 说
「实例不一致走 `p.fail`」在 `setup` 里做不到。**评审属实。**

**选择：(b)，把失败挪到每个入口阶段的第一次合法转移之后**，只用 `state.go` 已有的边，状态机一行不改。理由：实例
不一致与「平台凭据未配」是**确定性的配置错误**，不会自己好；按 (a) 留在 `queued`，任务每 2 分钟被重新领一次
（租约 `claimLeaseDuration`，`cmd/lathe/queue.go:27`）、失败只进日志（`queue.go:494`）——这正是 AGENTS §9「卡排队」的形状，
而且实例不一致是一次**安全拒绝**，人必须看得见原因。GitHub 的存量行为不动（硬约束「GitHub 路径不变」）。

分类（`resolveVCS` 返回之后、在 `setup` 里判定）：

| 平台 | 错误 | 处理 |
|---|---|---|
| github | 任何错误 | **与 cff5332 相同**：`return nil, "", fmt.Errorf("获取 GitHub 客户端失败…")`，任务留在 `queued` |
| gitlab/gitea | `errors.Is(err, vcs.ErrInstanceMismatch)` 或 `errors.Is(err, vcs.ErrCredentialMissing)`（凭据行不存在 / 无 `base_url`） | **不返回**：`rc.vcsErr = err`，`rc.gh = nil`、`rc.auth` 零值，`setup` 照常构造 `rc` 并返回 |
| gitlab/gitea | 其它（读库失败、解密失败等暂时性错误） | 与 GitHub 同口径：`return` 错误、留在 `queued`、靠租约过期自动重领（会自愈的错误不判死） |

`rc.vcsErr != nil` 时 `setup` 还要**跳过**两件会用到远端/平台的事：`:505-509` 的续跑 mirror 刷新（否则零值 auth 拿 https
URL 无鉴权 fetch，报一个难懂的 git 错误，又回到 `return err`），以及 `:493-503` 的整个 review 收集块（`collectReview` 要调
`rc.gh` 的 `ReviewFeedback`）。整块跳过而不是「收集为空」：后者会触发 `:495-502` 的降级把 entry 改成 `EntryVerify`，失败阶段就不对了；
`rc.review` 保持零值，`:640-648` 写进 payload 的 `review_keys` 为空——**这一轮不会把任何 review 意见标记为「已处理」**，
修好凭据重试后意见照常被收集。

然后由一个 helper 在**每个入口阶段的第一次转移之后、任何网络操作之前**落状态：

```go
// vcsGate 在已经站到「可以合法转 failed」的状态上之后调用。
func (p *Pipeline) vcsGate(rc *runCtx) error {
    if rc.vcsErr == nil { return nil }
    return p.fail(rc, StageVCSCredential, rc.vcsErr)
}
```

| `Execute` 入口 | 第一次转移（触发者 `rc.actor`，即 `node:<名>` 或 `system:rebase-followup`） | `vcsGate` 位置 | 落 `failed` 的边（触发者 `system`，`fail()` 内 `:1499`） | 门之前发生过什么 |
|---|---|---|---|---|
| `EntryTriage`（新任务 / Fresh 重试） | `stageTriage` `queued→triaging`（`:525`） | 紧接其后，**在拉 issue、跑分诊 agent 之前** | `triaging→failed` | 无网络、无 agent |
| `EntryImplement` 续跑（含 review 二轮、Rebuild） | `stageImplement` `queued→implementing`（`:650`） | 紧接其后，**在 `EnsureMirror`/`Reattach`（`:673-681`）之前** | `implementing→failed` | 无网络 |
| `EntryCommit` | `stageCommit` `queued→implementing`（`:868`） | 紧接其后，`HasChanges` 之前 | `implementing→failed` | 无网络 |
| `EntryVerify`（含 rebase 跟进重验） | `stageVerify` `→verifying`（`:936`） | 紧接其后 | `verifying→failed` | 只有本地 `ChangedFiles`（`:912`） |
| `EntryPush`（人工闸门放行 / 推送失败续跑） | `stagePushAndPR` `queued→verifying`（`:1115`） | 紧接其后，**在 `Push` 之前** | `verifying→failed` | 无网络 |

- `EntryTriage` 之后接着走的 `stageImplement` 非续跑分支（先 `Create` 后转移，`:617`→`:650`）只可能紧跟在 `stageTriage`
  之后（`resume` 为假 ⇔ 本轮从分诊进来，`:601`），那时门已经检查过，所以「`Create` 在转移之前」不构成漏口。
- 新阶段码 `StageVCSCredential Stage = "vcs_credential"` 定义在新文件 `internal/runner/vcs.go`（同 `skills.go:18`、
  `profile.go:77` 的「常量放在消费方旁边」先例）；`stage.go` 的 `label()` 加一个 case「代码平台凭据不可用」，**插在
  `StageCreatePR` 与 `StageRebaseConflict` 之间**（f11 在 `StageSkillMissing` 之后加 `StageTeamSkillFailed`，两处不相邻，
  合并不冲突）。**不进 `stageOrder`**（同 f11 的理由：改 `stageOrder` 会改变 `before()` 对其它阶段的比较）。
  `tasks.failure_stage` 无 CHECK（`stage.go:52-55` 注释），无需迁移。
- 人修好凭据后点重试：`failed→queued`（既有人工边），`PlanRetry` 对未知阶段走 `default`（`retry.go:332-348`）：
  无现场 → Fresh；有提交且干净 → `EntryVerify`。代价：`EntryPush` 入口失败的任务重试时会多跑一次验证（不是少跑），
  不为它改 `retry.go`。
- 失败三件套照常：回帖（经 tracker，不需要代码平台）、通知、失败记忆、失败传播（`PropagateBlocked`）。`failure_reason` =
  `代码平台凭据不可用: 仓库登记在 https://a.example，而你的 GitLab 凭据现在指向 https://b.example；…`（不含 token，
  `ErrInstanceMismatch` 的文案只拼两个地址）。

#### 3.4.2 mergepoll：每个调用点的 auth（v4，第三轮 blocker 3(1)）

`MergePoller` 里带鉴权的网络调用不止 `pollTask`。逐个定下来源（函数都是包内私有，测试直接调用的只有 `rebaseFollowup`
（`rebase_followup_test.go:272/529/780` 等 4 处），**它的签名不变**；`handleMerged/onMerged/resolveMirrorTip/rebaseFollowupOne/
failRebaseFollowup` 测试直接调用 0 处，`grep -rnE '\.(handleMerged|onMerged|resolveMirrorTip|rebaseFollowupOne|failRebaseFollowup)\(' --include=*_test.go internal cmd` → 0）：

1. **`pollTask`（`:158`）**：`RepoLookup` 挪到取客户端之前；`gh, auth, err := resolveVCS(ctx, clients, repoCfg.Target())`。
   任何错误（含实例不一致）→ `return` 错误，`pollOnce` 记 warn，**任务停在 `pr_open` 不动**（合并检测本来就是「查不到下一轮再查」，
   不在这里判死；属主修好凭据后下一轮自然收敛）。
2. **`handleMerged → onMerged → resolveMirrorTip`（`:183`、`:271`、`:306`）**：刷新的是**已合并任务**那个仓库的 mirror，用的是
   该任务属主的凭据，也就是 `pollTask` 刚解出的 `auth`。三个函数各加一个 `auth vcs.GitAuth` 参数透传；`resolveMirrorTip`
   改为 `EnsureMirror(ctx, repoCfg.RepoKey(), repoCfg.CloneURL(), auth)`。
3. **`rebaseFollowupOne`（`:391`）**：后继 `succ` 由 `TasksWithBaseRef` 查出，**不假设与前驱同属主**。在 `RepoLookup`（`:407`）之后、
   捕获 HEAD（`:423`）与任何改写之前：

   ```go
   clients, err := p.ClientFactory.ForUser(ctx, succ.UserID)
   ...
   _, auth, err := resolveVCS(ctx, clients, repoCfg.Target())
   switch {
   case err == nil:
   case errors.Is(err, vcs.ErrInstanceMismatch), errors.Is(err, vcs.ErrCredentialMissing):
       p.failRebaseFollowup(ctx, succ, StageVCSCredential, "解析代码平台凭据", err, vcs.GitAuth{})
       return
   default: // 暂时性错误：与 :407 RepoLookup 失败同口径，跳过本次跟进、不改状态
       slog.Warn("解析后继任务的代码平台凭据失败，跳过 rebase 跟进", "task", succ.ID, "err", err)
       return
   }
   ```

   随后 `RebaseOnto(ctx, wt, oldBaseTip, newBaseBranch, repoCfg.CloneURL(), auth)`、`ForcePush(ctx, wt, repoCfg, auth, nil)`
   用**同一个** `auth`；`:477` 的 `CloneURL: cloneURLFor(repoCfg)` 不变。`:474` 的 `Execute` 不传 auth——`setup` 自己按
   `succ.UserID` 解析（与派发同一路径，§3.4.1 的门同样生效）。GitHub 仓库上 `resolveVCS` 调的是 `clients.GitHub(ctx)`：这是
   rebase 跟进里**新增**的一次调用；它只构造 client、不发请求（`creds/verifier.go:128` 只读凭据），但会在 GitHub 凭据缺失时让
   跟进提前跳过。为不改变 GitHub 行为，**GitHub 仓库在这里不调 `resolveVCS`、直接用零值 auth**（GitHub 走宿主 SSH，本来就不需要）。
4. **`failRebaseFollowup`（`:585`）**：签名加 `stage Stage` 与 `auth vcs.GitAuth` 两个参数；既有两处调用（`:430`、`:435`）传
   `StageRebaseConflict` 与上面那个 `auth`，行为不变；`auth` 用于脱敏 `reason`（§3.9-2）。`StageRebaseConflict` 的文案
   「自动 rebase 冲突」不适用于凭据错误，所以凭据错误传 `StageVCSCredential`。
   既有限制（不在本线修）：`succ` 若停在 `queued`（有 worktree 却回到了排队），`queued→failed` 不合法，`Transition` 失败只记日志
   ——这是 `failRebaseFollowup` 对 rebase 冲突的**存量**行为（`:621-629`），凭据错误沿用，不新增状态机边。
5. **`checkForcePush`（`:220`）**：只调 `rebaseFollowup`，后继各自在第 3 步解析 auth，无需改动。

#### 3.4.3 PRD 规划路径：`prd.Service` 新增鉴权依赖（v4，第三轮 blocker 3(2)）

**事实**：`prd.Service`（`prd/service.go:78-120`，装配在 `main.go:425-434`）没有任何凭据或 `ClientFactory` 依赖；`runSpec`
（`:480-490`）也没有属主。v3 的 AC-21 同时要求「`RepoForPRD` 不注入凭据」与「GitLab 仓库 `EnsureMirror` 收到的 auth 非空」，
而又没说 auth 从哪来。**评审属实。**两句话本身不矛盾（`RepoForPRD` 只读 `repos` 行，凭据走另一条依赖），矛盾在于那条依赖不存在。补上：

```go
// internal/prd/service.go —— prd 只依赖 vcs（零依赖契约包），不依赖 runner/creds。
type GitAuthSource interface {
    GitAuth(ctx context.Context, userID int64, t vcs.Target) (vcs.GitAuth, error)
}
type Service struct {
    ...
    // GitAuth 为规划检出解析代码平台鉴权（按 PRD 属主）。nil：GitHub 仓库照旧
    // （零值 auth，走宿主 SSH）；GitLab/Gitea 仓库直接报错，不做无鉴权克隆。
    GitAuth GitAuthSource
}
```

- **属主**：`runSpec` 加 `userID int64`。两个构造点都有现成的 `userID`：`runRound`（`:440`，取 `p.UserID`，与 `:390/:395` 读 PRD 用的是同一个）
  与 `review`（`:852`，函数参数 `userID`）。用 PRD 属主而不是 `repos.user_id`：规划是这个人发起的，与「任务按任务属主取凭据」同一立场；
  也不把属主塞进 `RepoInfo`（f12 会让仓库可被团队共享，`RepoInfo` 的属主语义会变）。
- **调用**：`runAgent` 只在 `needCheckout` 时取（`:513-516`）：
  `auth, err := s.gitAuth(ctx, spec.userID, spec.repo.Target())` → `s.Worktrees.EnsureMirror(ctx, spec.repo.RepoKey(), s.cloneURL(spec.repo), auth)`。
  `s.gitAuth`：GitHub → 零值、**不调** `GitAuth`；非 GitHub 且字段为 nil → `prd: 未装配代码平台鉴权（%s）`；否则转调。错误走
  `runAgent` 的既有失败路径（`FailPRDRun`，PRD 自己的状态，不涉及任务状态机）。阶段 A 与对抗复核不检出（`needCheckout=false`），不取凭据。
- **装配**：`cmd/lathe/prdwiring.go` 新增适配器，`main.go:425-434` 的字面量加一行 `GitAuth: prdGitAuth{f: factory}`（`factory` 是
  `serve()` 里 `:172` 的 `*creds.Factory`，同一作用域）：

  ```go
  type prdGitAuth struct{ f runner.ClientFactory }
  func (a prdGitAuth) GitAuth(ctx context.Context, userID int64, t vcs.Target) (vcs.GitAuth, error) {
      c, err := a.f.ForUser(ctx, userID)
      if err != nil { return vcs.GitAuth{}, err }
      v, ok := c.(runner.VCSClients)
      if !ok { return vcs.GitAuth{}, fmt.Errorf("prd: 客户端不支持代码平台 %q", t.Provider) }
      _, auth, err := v.VCS(ctx, t)   // 同一处实例比对：不一致返回 vcs.ErrInstanceMismatch
      return auth, err
  }
  ```

  同文件 `prdWorktrees.EnsureMirror` 加 `auth` 参数转发。
- **与邻居的接缝（同一个字面量）**：f03 在这里加 `Usage: st`（`f03-usage-stats.md:450`），f09 要把这个字面量提成 `buildPRDService`
  （`f09-notify-channels.md:1303`）。按「先合入者不依赖后合入者」：f07 先合入 → 字面量多一行 `GitAuth`，f09 提函数时原样带走并给
  `buildPRDService` 加 `factory runner.ClientFactory` 参数（或由调用方在返回值上补赋值）；f09 先合入 → f07 把这一行加进
  `buildPRDService` 并自己给它加参数、改唯一调用点。f03 只是相邻字段，文本冲突按「两行都留」解决。f07 **不依赖**这两条线的任何符号。
- 未知 provider 返回明确错误（与 `Tracker` 的「不静默路由到错误平台」同一立场，`verifier.go:113-127`）。

**测试假件影响面：零**。`fakeClients`（`pipeline_test.go:114`）、`countingClients`（`:1062`，嵌入前者）、
`providerDispatchClients`（`pipeline_internal_test.go:44`）一行不改；f09 的 `e2eClients` 也不需要补方法。
只有 f07 自己的新测试（AC-19/AC-20）用的假件额外实现 `VCS`。

**与邻居的合入顺序**：在这个形态下 f07 与 f09 **任意先后合入 dev 都能编译**，PR 描述里不需要约定顺序；
PR 描述写明「f07 不改 `runner.Clients`/`GitHubAPI`/`runCtx.gh`，邻居的假件无需跟改」。若 f07 评审中被要求
回到「改名为 VCS」的形态，则必须改为：f07 后合，并在同一 PR 里补 f09 的 `e2eClients.VCS`——本 PRD 不采用该形态。
f08 只在 `Clients.Tracker` 加 case（`f08-tracker-yunxiao.md:434-435` 明确不碰 `GitHub()`），与本线不重叠。

### 3.5 API

#### 3.5.1 凭据（扩展，不新增路由）

`PUT /api/integrations/{kind}` 请求体增加可选字段：

```json
{ "token": "glpat-…", "baseUrl": "https://gitlab.example.com" }
```

- `kind ∈ {gitlab, gitlab_webhook, gitea, gitea_webhook}` 之外传 `baseUrl` → **400**（不静默忽略，
  避免「配了没生效」）。
- `gitlab|gitea` 令牌缺 `baseUrl` → 400；`baseUrl` 校验：`url.Parse` 成功、scheme 为 `https`
  （仅 loopback `localhost`/`127.0.0.1`/`[::1]` 允许 `http`，供自建实例本地联调）、无
  userinfo、无 query/fragment、长度 8..300、去掉末尾 `/` 后入库；**允许路径前缀**（GitLab
  可装在 `https://host/gitlab`）。
- `GET /api/integrations` 的 `IntegrationStatus` 增加 `baseUrl`（非密，可展示）。
- `Verifier` 接口改为 `Verify(ctx context.Context, req VerifyRequest) VerifyResult`，
  `VerifyRequest{Kind, Token, BaseURL string}`。实现：GitHub 走原 `verifyGitHub` 逻辑不变；
  GitLab `GET <base>/api/v4/user`（读 `username`）；Gitea `GET <base>/api/v1/user`（读 `login`）。
  Webhook 密钥两类照旧「无法主动验证」，但说明文案按平台改写（GitLab：项目 → Settings →
  Webhooks 的 Test 投递；Gitea：仓库 → Settings → Webhooks 的 Test Delivery）。

环境变量：**本轮不新增**。理由：实例地址是「某人的某个实例」，用环境变量表达会与设置页
形成两个真相源，且管理员兜底只对内置管理员生效（`internal/creds/factory.go:43-57`），
无法覆盖成员。README 变量表因此**无新增行**。

#### 3.5.2 仓库配置

- `POST /api/repos` 增加 `"vcsProvider": "github|gitlab|gitea"`（缺省 `github`）；
  未知值 400；`providerRepo` 校验改为 `vcs.SplitRef(provider, s)`（GitHub 仍要求两段）。
- `vcsProvider ∈ {gitlab, gitea}` 时，handler 从当前用户同平台凭据行读 `base_url`（非密列）写入
  `repos.vcs_base_url`；没有该凭据或其 `base_url` 为空 → 400（§3.3.1）。请求体**不收** `vcsBaseUrl`
  （传了 400）：实例地址只有一个真相源（凭据），仓库只是登记那一刻的快照。
- 同一用户同一路径已登记（无论平台）→ 409，沿用 `store.ErrRepoExists` 与现有唯一约束；
  错误文案补一句「同一路径不能同时登记在两个平台」。
- `PUT /api/repos/{id}` **不接受** `vcsProvider`、`vcsBaseUrl` 与 `providerRepo`：迁移平台/实例需要
  删除后重新登记。传了就 400 并说明理由（`store.UpdateRepo` 的 SET 子句也不含这两列）。
- `GET /api/repos` / `GET /api/repos/{id}` 返回 `vcsProvider` 与 `vcsBaseUrl`（非密）。
- 非属主访问仍是 **404**（`GetRepo` 带 `user_id` 过滤，`internal/store/query.go:336-355`）——
  新字段不改变这条规则。

#### 3.5.3 Webhook（review 回流）

新增路由（与 GitHub 同形，按用户 slug 隔离）：

```
POST /webhooks/gitlab/{slug}
POST /webhooks/gitea/{slug}
```

处理次序沿抄 GitHub 版（`internal/httpapi/github_webhook.go:53-141`）：路由 → 验签 →
幂等登记 → 平台解析 → 信任判定 → `ReviewIntake.AcceptReview` → 回响应/落投递结果。

| | GitLab | Gitea |
|---|---|---|
| 验签头 | `X-Gitlab-Token`（明文比对，用 `subtle.ConstantTimeCompare`） | `X-Gitea-Signature`（HMAC-SHA256 hex） |
| 事件头 | `X-Gitlab-Event: Note Hook` | `X-Gitea-Event: issue_comment` **且**载荷 `is_pull == true`（v6，见下「Gitea 事件识别」） |
| 投递 ID | `X-Gitlab-Event-UUID`（缺失则用 `object_attributes.id` + `updated_at` 拼） | `X-Gitea-Delivery` |
| 去重前缀 | `gitlab:` | `gitea:` |
| 意见 Key | `gitlab:note:<id>` | `gitea:comment:<id>` |
| 动作 | （本轮不变） | `action == "created"`（`edited`/`deleted` 忽略，与 GitHub 只处理新建同口径） |
| MR 编号 | `merge_request.iid`（项目内序号，与 UI 显示一致；**不是**全局 `id`） | `issue.number`（PR 与 issue 共用编号空间，即 PR 号） |
| 意见字段 | （本轮不变，`object_attributes`/`merge_request`） | `comment.body`、`comment.id`、`comment.user.login`（`IssueCommentPayload`） |
| 自身判定 | `comment.author_id == merge_request.author_id` → 忽略 | `comment.user.login == issue.user.login`（PR 的发起人）→ 忽略。**不用** `pull_request.user`：该字段 Gitea 1.20/1.22 的 `IssueCommentPayload` 里没有，只在较新版本以 `omitempty` 出现 |

**Gitea 事件识别（v6，第五轮 blocker 2；v5 把事件头写错了）**：Gitea 的投递头 `X-Gitea-Event` 取的是
`HookEventType.Event()`（`modules/webhook/type.go`），同时另带 `X-Gitea-Event-Type: <HookEventType 原值>`（`services/webhook/deliver.go`）。
两种 PR 相关评论的实际发送形态（1.20、1.22、main 一致，源码摘录见 `/opt/lathe-wt/evidence/f07/prd-v6-gitea-webhook-event-source.log`）：

| 用户在 Gitea 里做了什么 | `HookEventType`（= `X-Gitea-Event-Type`） | `X-Gitea-Event` | 载荷 |
|---|---|---|---|
| 在 PR 对话里发普通评论（webhook 设置里的「Pull Request Comment」） | `pull_request_comment` | **`issue_comment`** | `IssueCommentPayload`：`action`、`issue`、`comment{id,body,user}`、`is_pull: true` |
| 在普通 issue 里评论 | `issue_comment` | `issue_comment` | 同上，`is_pull: false` |
| 提交一个「评论」类 review（含行内评论） | `pull_request_review_comment` | **`pull_request_comment`** | `PullRequestPayload` + `review{type,content}`：没有 `comment.id`/`comment.user`，也不带各条行内评论 |
| 提交「批准」/「请求修改」review | `pull_request_review_approved` / `_rejected` | `pull_request_approved` / `pull_request_rejected` | 同上 |

所以 v5 表里的 `X-Gitea-Event: pull_request_comment` 会把**所有普通 PR 评论**当成无关事件丢掉，真正收到的 review 事件又按 `issue/comment`
字段解析失败。v6 的识别规则（写在 `internal/vcs/gitea` 的解析函数里，handler 只传头与 body）：
1. `X-Gitea-Event == "issue_comment"`，且载荷 `is_pull == true` → 按 `IssueCommentPayload` 解析，进入动作/自身/信任判定。以载荷 `is_pull` 为准，
   因为它与 `X-Gitea-Event` 同在所有受支持版本里；`X-Gitea-Event-Type` 若存在且不是 `pull_request_comment`，按不一致拒绝（`ignored`，原因写投递记录）。
2. `X-Gitea-Event == "issue_comment"` 且 `is_pull == false` → `ignored`（普通 issue 评论，与 PR 无关）。
3. `X-Gitea-Event ∈ {pull_request_comment, pull_request_approved, pull_request_rejected}`（review 事件）→ `ignored`，投递记录写
   「Gitea review 事件本轮不触发二轮；在 PR 对话里发一条评论即可触发，review 总评与行内评论会在二轮全量拉取时一并带上」。
   **范围界定**：Gitea 的 review（总评 + 行内评论）**不作为触发源**（理由：载荷不含逐条评论与评论 ID，无法给出意见 Key，也拿不到每条行内
   评论的作者做信任判定；要支持就得在 webhook 处理里再回拉 API，属于新增能力），**只由 `ReviewFeedback` 二轮全量拉取覆盖**：
   `GET /api/v1/repos/<o>/<r>/issues/<n>/comments`（普通评论；Gitea 这个接口只返回 `CommentTypeComment`，**不含**行内评论）
   + `GET .../pulls/<n>/reviews`（review 总评，正文非空的才收）+ 对每个 review `GET .../pulls/<n>/reviews/<id>/comments`（行内评论，填 `Path/Line`），
   三者合并、各自按作者走同一道协作者权限过滤、总翻页上限沿用 10 页。GitHub 的 review/行内评论触发行为不变。
4. 其余 `X-Gitea-Event` → `ignored`（与 GitHub 的 `ErrUnsupportedEvent` 同口径）。

**信任判定（安全核心，必须 fail-closed）**：

- GitLab：`GET <base>/api/v4/projects/<url-encoded path>/members/all/<user_id>`，`access_level >= 20`
  （Reporter）才可信；404/403/超时 → **拒绝**并把原因写进投递记录。
- Gitea：`GET <base>/api/v1/repos/<owner>/<repo>/collaborators/<login>/permission`，
  `permission ∈ {write, admin, owner}` 才可信；非 200 → 拒绝。
- 两道守卫与 GitHub 完全同构：先「不是 MR 作者本人」，再「有仓库关系」。GitHub 的
  `author_association` 白名单一行不改。
- 信任判定写在各自平台包里（`internal/vcs/gitlab`、`internal/vcs/gitea` 的 `Actionable(ctx, …)`，
  需要成员查询所以带 ctx 与 client），通过后才构造 `vcs.ReviewEvent` 交给 `AcceptReview`——与 GitHub
  的「平台包判信任、`vcs.ReviewEvent` 只承载已放行意见」同一形状（§3.1）。契约包里没有任何信任逻辑。

载荷字段名（`object_attributes`/`merge_request`、`issue`/`comment`/`is_pull`）按两个平台官方
webhook 文档建模，Gitea 部分已对照上游源码（`modules/webhook/type.go`、`services/webhook/deliver.go`、`modules/structs/hook.go`，
摘录见 `prd-v6-gitea-webhook-event-source.log`）；**实现时以官方文档与真实载荷为准核对**，httptest 桩里的**头与 JSON** 必须与上述
形状一致——Gitea 桩的普通 PR 评论必须带 `X-Gitea-Event: issue_comment`、`X-Gitea-Event-Type: pull_request_comment`、`is_pull: true`，
不得照抄 v5 表里写错的头（这是本 PRD 唯一一处依赖外部文档、而不依赖本仓代码的断言，故显式标注）。

`webhook_deliveries.source` 新取值 `gitlab` / `gitea`；该列无 CHECK 约束
（`migrations/0001_init.up.sql:179-187`），无需迁移。

#### 3.5.4 按 PR 找任务

新增 `Machine.TasksByVCSPR(ctx, userID, provider, providerRepo, prNumber)`，SQL 在现有
`TasksByPR`（`internal/task/machine.go:488-495`）基础上增加 `AND r.vcs_provider = $4`；
`TasksByPR` 保留原签名，改为 `TasksByVCSPR(…, "github", …)` 的薄包装——现有
`TestMachineTasksByPRIsolatesOwners`（`internal/task/machine_test.go:689-750`）**一行不改**仍全绿。
`AcceptReview` 先校验 `ev.Provider.Valid()`（空或未知 → 返回错误，不猜 github，§3.1），再调 `TasksByVCSPR(ctx, ownerID, ev.Provider, ev.Repo, ev.PRNumber)`。
**两处属主过滤（`tasks.user_id` 与 `r.user_id`）必须原样保留**。平台过滤的意义：唯一约束保证
同一用户一个路径只在一个平台上，所以它挡的是「GitHub webhook 带着一个 GitLab 仓库的路径与
编号投进来」这种跨平台串扰，而不是同名仓库消歧。

### 3.6 合并状态映射（三平台 → 统一三态）

| `PRInfo` 字段 | GitHub | GitLab | Gitea |
|---|---|---|---|
| `Merged` | `pr.merged` | `state == "merged"` | `merged == true` |
| `State` | `open`/`closed` | `opened`→`open`；`closed`/`merged`→`closed`；`locked`→`open` | `open`/`closed` |
| `BaseRef` | `base.ref` | `target_branch` | `base.ref` |
| `HeadSHA` | `head.sha` | `sha`（缺省退回 `diff_refs.head_sha`） | `head.sha` |

映射后 `mergepoll` 的三态分支（merged / closed 未合并 / open + force-push 检测）
`internal/runner/mergepoll.go:180-192` **一行不改**，F4.3/F4.4 逻辑自动适用于新平台。

### 3.7 UI

1. **仓库页**（`web/src/views/Repos.vue`）：新增「代码平台」下拉（GitHub / GitLab / Gitea）。
   已有仓库显示当前平台且**只读**（不可迁移，见 3.5.2），旁边一行说明「需要换平台请重新登记
   仓库」。新建表单的平台选择与 `providerRepo` 输入提示（GitLab 允许 `group/subgroup/repo`）。
2. **个人设置页**（`web/src/views/PersonalSettings.vue`）：`META` 增加四项
   （`gitlab`/`gitlab_webhook`/`gitea`/`gitea_webhook`），每项的 `desc`/`where` 写清获取位置；
   `gitlab`/`gitea` 卡片多一个「实例地址」输入框（`https://gitlab.example.com`），与 token
   一起提交；顶部新增两个 webhook 地址展示（`/webhooks/gitlab/{slug}`、`/webhooks/gitea/{slug}`），
   与现有 `webhookFor('github')` 同形（`web/src/views/PersonalSettings.vue:10-20`）。
3. `web/src/api.js` 的 `saveIntegration` 增加第二个参数 `baseUrl`。
4. 改动后必须 `make ui && make build`（AGENTS §2），PR 里附手工走查路径与截图。

### 3.8 配置项与消费方（AGENTS §1.3）

| 配置 | 消费方 | 非法值 |
|---|---|---|
| `repos.vcs_provider` | 写：`store.CreateRepo`；读：`cmd/lathe/queue.go:loadRepoConfig`、`runner.NewRepoLookup`、`prd.StoreAdapter.RepoForPRD` → `RepoKey()/CloneURL()`、`resolveVCS`/`creds.Clients.VCS` 选客户端、`TasksByVCSPR`、仓库页 | DB CHECK + API 400 |
| `repos.vcs_base_url` | 写：`store.CreateRepo`（复制自凭据）；读：同上三个加载器 → `RepoKey()/CloneURL()`、`resolveVCS`/`creds.Clients.VCS` 的实例比对 | DB CHECK（与平台绑定 + 长度） |
| `integrations.base_url` | 写：`PUT /api/integrations/{gitlab,gitea}`；读：`Verifier`、仓库登记、`resolveVCS`/`creds.Clients.VCS`（API client + 实例比对） | DB CHECK（长度、新 kind 必填）+ API 400（scheme/形状，§3.5.1） |
| `integrations.kind` 新取值 | `store.KnownKinds` → 设置页渲染 → 派发时按平台取凭据 | DB CHECK |
| 新环境变量 | 无 | —（不新增，见 3.5.1） |

### 3.9 安全

1. **凭据加密**：`gitlab|gitea` token 与两类 webhook secret 全部走既有 `Secrets.Save`
   （AES-256-GCM、`secret_enc`；`internal/store/secrets.go:68-93`）。`base_url` 非密，明文列。
2. **日志/事件/界面永不出明文**：`IntegrationStatus.Masked` 沿用 `secret.Mask`；`GitAuth` 自带
   脱敏 `String()`；git 错误文案只经 `truncate(stderr, 600)` 输出，URL 不含 token。

   **运行期解出的凭据怎么登记进脱敏权威层（v2，blocker 6.2）**。现状：`Pipeline.KnownSecrets`
   是装配时固定的 `[]string`（`internal/runner/pipeline.go:120-129`，`cmd/lathe/main.go:587` 赋
   `cfg.KnownSecrets()`），被所有并发任务共享；唯一消费点是 `failureExcerpt`
   （`internal/runner/redact.go:559`）。往共享切片里追加「当前用户」的 token 有数据竞争，且会让
   A 的 token 出现在 B 任务的替换表里（无害但无界增长）。设计：

   - **不改** `Pipeline.KnownSecrets` 的类型与语义（装配期、只读、共享）。
   - `runCtx` 增加 `secrets []string`：`setup` 在 `resolveVCS`/`creds.Clients.VCS` 成功后，把**本任务属主**解出的
     vcs token（`GitAuth` 内部值，经 `vcs.GitAuth.RedactValues()` 取出，仅包内与 runner 可见）
     追加进去。**每任务一份**，随 `runCtx` 生命周期结束，无共享可变状态，无锁。
   - `failureExcerpt(report)` 改为 `failureExcerpt(rc, report)`，替换表 =
     `append(p.KnownSecrets, rc.secrets...)`（新切片，不改原切片）。唯一调用点
     `internal/runner/pipeline.go:1099` 跟改。
   - mergepoll 的 rebase 跟进（`internal/runner/mergepoll.go:640` 调 `recordFailureMemoryFor`，
     excerpt 为空串、reason 经 `redactSecrets(…, nil)`）：reason 来自 git 错误文案，可能含远端
     回显。改为 `redactText(reason, append(p.Pipeline.KnownSecrets, auth.RedactValues()...), nil)`，
     `auth` 是 `failRebaseFollowup` 的新参数，即 `rebaseFollowupOne` 按**后继任务属主**解出、并用于该次
     `RebaseOnto/ForcePush` 的同一个 `GitAuth`（来源见 §3.4.2 第 3、4 条；凭据解析本身失败时传零值，此时也没有 token 可回显）。
   - webhook 密钥（`github_webhook/gitlab_webhook/gitea_webhook`）**不进 runner 的替换表**：它们只在
     httpapi 验签时使用，从不进入 agent、验证输出或失败记忆（全仓 `grep -n 'WebhookSecret'` 消费方只有
     `internal/creds/creds.go:116/150` 与 webhook handler）。登记它们没有可脱敏的输出面，
     v1 这句是多余的需求，删去；替代保证是 AC-12b：handler 的日志与响应体不含密钥。
   - 环境变量兜底的 `GitHubWebhookSecret` 补进 `config.KnownSecrets()`（`internal/config/config.go:429-444`
     漏了它）：一行改动，保留，理由是 `KnownSecrets` 的注释自称覆盖「集成 token、webhook 密钥」。

3. **属主 404**：新路由没有「无 slug 兜底」；未知 slug → 404（`internal/httpapi/github_webhook.go:63-69`
   同形）。`TasksByPR` 的两处属主过滤保留。`/api/repos` 与 `/api/integrations` 均按
   `CurrentUser` 过滤。
4. **信任判定 fail-closed**：见 3.5.3，任何成员查询异常都拒绝回流（拒绝理由落投递记录，
   不进响应体，避免探测他人仓库成员关系）。
5. **agent 拿不到 token（v3 重写，第二轮 blocker 1）**：v2 依赖「环境注入的配置覆盖仓库 config」，
   被 `http.<更具体 URL>.*`、`include.path`、`url.*.insteadOf` 绕过（§3.3.2）。现在的保证：
   (a) token 只经管道 fd 交给 git 的 credential helper，不进任何进程环境；
   (b) 带鉴权的 git 调用在 Lathe 私有、每次新建、无 config 文件的 transport GIT_DIR 里运行，
   `GIT_CONFIG_NOSYSTEM=1`、`GIT_CONFIG_GLOBAL=/dev/null`、`HOME` 为空目录、环境白名单从零构造——
   **配置来源只有 Lathe 的 `GIT_CONFIG_COUNT`**，agent 可写的 mirror/worktree config 与 hooks 一项都不读；
   (c) 远端地址取自 `repos` 行，显式写在命令行；transport 里没有 `remote.origin`；
   (d) ref 回写 mirror 是本地 `update-ref`，不带 token、不联网；
   (e)（v5，完整性而非保密性）transport 里关掉 `gc --auto`/`maintenance --auto`，mirror 的 gc 只在 mirror 语境下跑，
   不会因为 transport 看不到任务 ref 而删掉在途提交（§3.3.2，AC-5d）。
   `GitAuth` 不进 `agent.RunParams`（`internal/integration/agent/agent.go:230`、`:467-498` 白名单不变）。
   残余假设（写进 §6）：agent 与 Lathe 同一 OS 用户运行时，同用户进程本来就能读 Lathe 的内存与 `/proc/<pid>/`
   （这是存量的部署前提，不是本线引入）；本线保证的是「agent 不能**借 git 之手**拿到 token」。
6. **不引入新的入站暴露面**：webhook 依赖「知道 slug + 知道密钥」两件，与现有一致；载荷
   仍受 `maxWebhookBody`（4MB）限制（`internal/httpapi/webhook.go:15-16`）。

### 3.10 并发 / 多节点

- **投递去重**跨节点安全：`webhook_deliveries.delivery_id` 是主键，`ON CONFLICT DO NOTHING`
  （`internal/store/webhook.go:21-30`）。三平台共用一张表，键带平台前缀避免互撞。
- **合并检测**仍是每节点轮询（`mergepoll` 无 webhook 主路径，设计如此）；多节点重复轮询
  不会重复推进状态：`TransitionFrom` 在行锁下失败的一方拿到 `ErrStateMismatch` 按忽略处理
  （`internal/runner/review.go:126-140`、`:197-206`）。
- **凭据缓存**：`Provider` 的 30s 缓存的条目从「值」扩为「值 + 实例地址」，键仍是 kind；
  保存/删除触发 `Invalidate`（`cmd/lathe/main.go:379` → `creds/factory.go:67`）。跨节点靠
  TTL 收敛（≤30s），与今天公开的行为一致。
- **平台不可中途变更**：任务经 `tasks.repo_id` 钉死仓库，仓库的 `vcs_provider/vcs_base_url` 没有任何
  更新路径（§3.2），所以在途任务不可能换平台/实例开 PR；凭据地址事后改动由 `resolveVCS`/`creds.Clients.VCS` 的
  实例比对拦截（§3.3.1）。
- **成员查询不做缓存**：webhook 频率低（人工评审），一次额外 API 调用可接受；缓存会引入
  「刚被移出项目仍能驱动二轮」的窗口。

---

## 4. 范围外（明确不做）

| 不做 | 归属 / 理由 |
|---|---|
| Bitbucket、Azure DevOps、Gitee 等其它平台 | 本轮只要 GitLab/Gitea；`vcs.Provider` 留枚举扩展点 |
| 同一用户把**同一路径**同时登记在两个平台（如 `acme/demo` 既在 GitHub 又在 GitLab） | 保留 `repos_user_repo_unique (user_id, provider_repo)`（§3.2）换取：既有测试与 f12 零冲突、down 可干净回滚。需要时属主可以给其中一个起不同路径（fork/改名）。真要支持需要改唯一约束，应与 f12 的仓库归属改造一起做，见 §9 B5 |
| 改 `repos_user_repo_unique` 约束、改 `TasksByPR` 签名 | 与 f12 的边界：f12 声明该约束保留（`f12-team-roles.md:36`、`:279`、`:731`）并给 `repos` 加 `team_id`（`:267`，迁移 0066）。本线只给 `repos` **加两列 + 一条 CHECK**，不改、不删、不重命名任何既有约束；与 f12 的列名（`team_id`）、迁移号（0065–0067 vs 0050–0052）均不相交，任意合并顺序都可独立合入 dev。f06 加 `repos.agent_driver`（`f06-multi-cli.md:382`）同理不相交 |
| 同一用户接**两个** GitLab/Gitea 实例 | 凭据是 `(user_id, kind)` 唯一；两个实例需要两条凭据。本轮用「一个平台一个实例」换取数据模型简单，写进限制 |
| GitHub Enterprise Server（自定义 base URL） | 约束已为它留了空间（3.2 不强制 `base_url IS NULL`），本轮不实现 |
| 合并决策 / 自动合并 / merge queue | §1.2 产品边界：人点合并 |
| Gitea review 事件（`X-Gitea-Event: pull_request_comment/approved/rejected`）作为二轮**触发源** | v6（第五轮 blocker 2）：载荷没有逐条评论与评论 ID、没有行内评论作者，做不了意见 Key 与逐条信任判定。收到时 `ignored` 并在投递记录说明；review 总评与行内评论由 `ReviewFeedback` 二轮全量拉取带上（§3.5.3「Gitea 事件识别」第 3 条、AC-9）。要支持需在 webhook 里回拉 API，另立条目 |
| GitLab/Gitea 的 issue 事件接单（tracker） | f08 云效 tracker 与本线的边界：本线只碰**代码平台**（MR/评论/克隆），不碰需求平台 |
| 出站通知（Slack/飞书） | f09。编译期边界：本线**不改** `runner.Clients`、`runner.GitHubAPI` 的方法集与 `runCtx.gh` 字段名（§3.4），f09 的 `e2eClients`（`f09-notify-channels.md:1125-1127`）无需跟改，两线任意顺序合入 dev 都能编译 |
| 用量统计、运行回放、插话、收件箱、PAT、团队角色、自定义技能、定时建单 | f03/f04/f01/f02/f05/f12/f11/f10 |
| 多 CLI 驱动（Codex） | f06；本线不碰 `internal/integration/agent` |
| 改 `internal/task/state.go` 的状态或转移 | 硬约束：状态机语义不变 |
| 改「非属主 404」规则 | 硬约束 |
| 削减弱化验证体系（档位、红绿、隔离栈） | 硬约束；换平台不改变验证任何一步 |
| 迁移已登记的仓库到另一个平台 | 见 3.5.2：只允许重新登记 |
| 新增环境变量 / 新依赖 | 硬约束 |

---

## 5. 验收标准（AC）

> **先红后绿是硬要求**（AGENTS §5.4）：每条 AC 对应的测试必须在实现前先跑出失败，
> 失败输出存到 `/opt/lathe-wt/evidence/f07/red-<测试名>.log`，实现通过后把同一命令的输出
> 存到 `green-<测试名>.log`。PR 里给出这组证据的路径。
> 所有命令前先：`cd /opt/lathe-wt/f07-vcs && source /opt/lathe-wt/env/f07.sh`。

### 契约与 GitHub 不变

**AC-1（GitHub 行为不变，编译级）**
`go build ./...` 退出码 0；`go test ./internal/integration/github/... -count=1` 全绿，且
`git diff --stat cff5332 -- internal/integration/github/github_test.go
internal/integration/github/verify_test.go internal/integration/github/webhook_test.go` 输出为空
（三个既有测试文件一行未改；新增的 `alias_test.go` 不计入此项）。
（现有 23 个 GitHub 测试函数**一行未改**仍通过：github_test.go 12 + verify_test.go 5 +
webhook_test.go 6）。

**AC-2（类型别名成立）**
`TestGitHubTypesAreVCSAliases`（`internal/integration/github/alias_test.go`）：
`var _ vcs.Client = (*Client)(nil)` 与 `var _ vcs.ReviewFeedbackSource = (*Client)(nil)` 编译通过；
断言 `github.PRParams{}` 可赋给 `vcs.PRParams`（同一类型）；`ReviewEvent` **不是**别名
（`reflect.TypeOf(github.ReviewEvent{}) != reflect.TypeOf(vcs.ReviewEvent{})`）。

**AC-2b（GitHub 信任判定一字不松，blocker 1）**
`TestReviewEventVCSConversionKeepsGitHubTrust`（`internal/integration/github/alias_test.go`）表驱动：
`author_association ∈ {OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR, FIRST_TIME_CONTRIBUTOR, FIRST_TIMER,
MANNEQUIN, NONE, "", "owner", "UNKNOWN_X"}` × 作者是否 == PRAuthor，断言 `Actionable()` 的结果与
`cff5332` 版本的期望表逐项相同（白名单四个、大小写不敏感、未知与缺失拒绝）；`ev.VCS()` 的
`Provider=="github"`、其余字段逐一等于源事件。
`TestGitHubWebhookNonActionableNeverReachesIntake`（`internal/httpapi`）：`NONE` 关系的评论投递后
`fakeReviewIntake.calls` 为 0（Actionable 仍在 AcceptReview 之前）。另外
`git diff cff5332 -- internal/integration/github/webhook.go` 只有新增的 `VCS()` 方法与 import，
`trustedAssociations/TrustedAssociation/Actionable/describeAssociation` 四个符号的行无改动
（PR 里贴这段 diff）。

**AC-3（仓库标识解析）**
`TestSplitRefByProvider`（`internal/vcs/vcs_test.go`）表驱动：
- github：`acme/demo` ok；`a/b/c`、`/b`、`a/`、`` → 错误且文案含 `owner/name`；
- gitlab/gitea：`acme/demo`、`group/sub/demo` ok；`..`、`a//b`、`-x/y`、`.x/y`、
  含 `%2f`、长度 201 → 错误。

**AC-4（PR 正文与平台无关）**
`TestBuildPRBodyPlatformNeutral`：`vcs.BuildPRBody("CR-1","https://x","验证摘要","改动说明")`
输出含 `关联 issue`、`## 改动说明`、`## 验证结果`、`**合并前请人工复核。**`；
与 `internal/integration/github/github_test.go:274` 现有断言的期望逐字一致。

**AC-5（git 鉴权不泄明文）**
`TestGitAuthRedactedString`：`String()`、`fmt.Sprintf("%v"/"%+v"/"%#v", a)` 均不含 token 子串；**嵌套形态**（v4）：
`runner.CreateParams{…, Auth: a}` 经 `%v/%+v/%#v/%s`、`slog` 文本与 JSON handler、`json.Marshal` 输出都不含 token 子串
（原型 `/opt/lathe-wt/evidence/f07/prd-v4-createparams-auth-redaction-probe.log`）；`reflect` 断言 `RepoConfig` 与 `ExecuteParams`
**没有**类型为 `vcs.GitAuth` 的字段（auth 只进一次性入参 `CreateParams`，§3.3.2）。
`TestCreateEnsuresMirrorWithAuth`（`internal/runner`，第三轮 blocker 1）：用 AC-5b 的 `git http-backend` 桩（要求 Basic 鉴权，
无鉴权返回 401）对一个 gitlab 仓库调 `Create(ctx, CreateParams{…, Auth: a})` → 首次建 mirror 成功、桩收到带 token 的请求；
同参数但 `Auth` 为零值 → `Create` 返回错误（证明 auth 真的经 `Create` 到达 `EnsureMirror`，而不是靠别的路径）。先红：现实现下
`CreateParams` 没有 `Auth` 字段，编译失败即红。
`TestTransportEnvIsWhitelistAndTokenFree`（`internal/runner`）：`transport()` 构造的 `exec.Cmd` 的
`Env` **恰好**是白名单那几项（`PATH/HOME/GIT_DIR/GIT_OBJECT_DIRECTORY/GIT_CONFIG_NOSYSTEM/GIT_CONFIG_GLOBAL/
GIT_TERMINAL_PROMPT/GIT_ASKPASS/GIT_PROTOCOL_FROM_USER/GIT_CONFIG_COUNT/GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n`），
不含 `HTTP_PROXY/HTTPS_PROXY/SSH_AUTH_SOCK` 及任何 `LATHE_*`；`GIT_DIR` 不在 `WorkspaceRoot` 之下且权限 0700；
env 与 argv 中没有任何一项含 token 子串；token 只出现在 `ExtraFiles[0]` 管道中；调用返回后该目录已不存在。
`TestGitHubRemoteUnchanged`：`vcs.RepoKey(github, "", "acme/demo") == "acme/demo"`、
`vcs.CloneURL(github, "", "acme/demo") == "git@github.com:acme/demo.git"`、github 的 `GitAuth.IsZero()`；
`loadRepoConfig` 对 github 仓库返回的 cloneURL 与 `cff5332` 拼接结果逐字相等。

**AC-5b（agent 投毒的 config / hook 拿不到 token，第二轮 blocker 1）**
`TestAuthedGitIgnoresAgentConfig`（`internal/runner/worktree_auth_test.go`，真实 git + 本地 `git http-backend`
作 httptest 远端——`/usr/lib/git-core/git-http-backend` 随 git 安装，不是新依赖；不可用时 `t.Skipf`；另起一个
`net.Listen` 的「agent 代理」只记录连接）。表驱动，**每个子测试只投一种毒**，便于先红逐项定位：

| 子测试 | agent 写入（`git -C <worktree> config …`，即 mirror config） | 断言 |
|---|---|---|
| `proxy_most_specific_url` | `http.<cloneURL>.proxy=http://127.0.0.1:<代理>` | 代理收到 0 个连接 |
| `proxy_base_prefix` | `http.<base>/.proxy=…` | 同上 |
| `sslverify_off` | `http.<cloneURL>.sslVerify=false` | transport 环境下 `git config --get-urlmatch http.sslVerify <cloneURL>` 输出为空；（HTTPS 子用例）对自签证书的 `httptest.NewTLSServer` 推送**失败**——证明没有被关掉校验 |
| `sslcainfo_via_include` | `include.path=<agent 文件>`，文件里写 `http.sslCAInfo=<agent CA>` 与 `http.proxy=<代理>` | 代理 0 连接；`--get-urlmatch http.sslCAInfo` 为空 |
| `insteadof_rewrite` | `url.http://127.0.0.1:<代理>/.insteadOf=<base>/` | 代理 0 连接，推送落在原桩 |
| `credential_helper` | `credential.helper` 与 `credential.<base>.helper`（把 stdin 写进 `$T/helper.out`） | `helper.out` 不存在 |
| `remote_origin_url` | `remote.origin.url=http://127.0.0.1:<代理>/x.git` | 代理 0 连接 |
| `hooks` | `<mirror>/hooks/{pre-push,reference-transaction,post-update}` 记录 `env` 与 `$GIT_DIR` | 记录里没有任何一条的 `GIT_DIR` 是 transport 目录；所有记录都不含 token |
| `config_origin_only_command_line` | 以上全部同时投放 | 在 `transport()` 的同一环境下跑 `git config --list --show-origin`，每一行都以 `command line:` 开头（与 git 版本无关的总断言，覆盖 `http.curloptResolve` 等将来的新键） |

每个子测试依次执行 `EnsureMirror`（首次与刷新）、`Push`、`RebaseOnto`、`ForcePush`、`Reattach`，并共同断言：
① 原桩收到 `Authorization: Basic base64(oauth2:tok-<随机>)`，且远端 `refs/heads/<branch>` 等于 worktree 的 HEAD
（证明鉴权仍生效、不是靠「不推」通过）；② mirror 的 `refs/remotes/origin/*` 与远端一致（fetch + 本地导入正确，
含 prune）；③ `ForcePush` 在远端被第三方改动后返回错误（lease 生效）。
base 取 `http://127.0.0.1:<port>` 与带路径前缀的 `http://127.0.0.1:<port>/gitlab` 各跑一遍。
**先红**：同一测试先在「v2 形态」（mirror 上跑 git + `GIT_CONFIG_COUNT` 注入）的实现上跑，
`proxy_most_specific_url`、`proxy_base_prefix`、`sslverify_off`、`sslcainfo_via_include`、`insteadof_rewrite`、
`config_origin_only_command_line` 必须失败（红日志存 `red-TestAuthedGitIgnoresAgentConfig.log`）；
shell 版原型与预期结果见 `/opt/lathe-wt/evidence/f07/prd-v3-urlmatch-bypass-probe.log`（B0 红、B1 绿）与
`prd-v3-design-e2e-probe.log`。

**AC-5c（零值 GitAuth 走原路径，GitHub 逐字节不变的机械证明）**
`TestZeroAuthKeepsLegacyGitArgv`（`internal/runner`，`PATH` 前置一个记录 argv 与环境的包装 `git`）：
对 github 仓库依次调 `EnsureMirror/Push/ForcePush/RebaseOnto/Reattach`（零值 `GitAuth`），记录到的 git 子命令序列
与在 `cff5332` 上跑同一测试得到的序列逐行相同（期望表写死在测试里：`clone --mirror --quiet <url> <mirror>`、
`config --unset-all remote.origin.mirror`、`fetch --prune --quiet origin`、`push --set-upstream origin <refspec>`、
`push --force-with-lease --set-upstream origin <refspec>` …），且没有任何一次调用的环境里出现 `GIT_DIR` 指向
`lathe-git-transport-*`。

**AC-5d（transport 刷新不删 mirror/worktree 独有对象、且是增量的，v5；v6 按第五轮 blocker 1 重写加速器与观测手段）**
`TestTransportFetchKeepsMirrorOnlyObjects`（`internal/runner/worktree_auth_test.go`，同 AC-5b 的真实 git + `git http-backend` 桩，
不可用时 `t.Skipf`）：gitlab 仓库建 mirror 与 worktree，在 worktree 里提交一个**只被** mirror `refs/heads/<任务分支>`、worktree `HEAD`
引用的提交（含一个新 blob），把 mirror 对象库下全部松散对象的 mtime 设为 30 天前（`os.Chtimes`，老过默认 `gc.pruneExpire`=2 周）。
两处加速/投毒，**各管一个语境**：
- **transport 语境（加速器，测试注入口）**：`transportExtraConfig = {fetch.unpackLimit=1, gc.autoPackLimit=1, gc.autoDetach=false}`
  （`t.Cleanup` 复位为 nil）。transport 不读 mirror config，加速器只有经这里才对 transport 里的 fetch 生效（§3.3.2「测试注入口」）。
- **mirror 语境（投毒）**：mirror 的 config 里写 `gc.autoPackLimit=1`、`gc.auto=1`、`gc.pruneExpire=now`、`gc.autoDetach=false`（模拟 agent
  改坏 mirror 的 gc 配置）。它只作用于 `importRefs` 之后那次 **mirror 语境**的 `gc --auto`——这次 gc 必然被触发且以 `prune=now` 前台跑，
  用来证明 mirror 语境下的 gc 看得见任务分支与 worktree、不删在途对象。
上游连推 3 个各改一个文件的提交，每次后调 `EnsureMirror(…, auth)` 刷新，再各调一次 `RebaseOnto` 与 `Reattach` 的刷新。断言
① 每次刷新后 `git -C <mirror> cat-file -t <任务提交>` == `commit`、该 blob == `blob`、`git -C <worktree> log -1` 成功；
② 最后 `git -C <mirror> fsck --connectivity-only` 无 `missing`/`broken`；
③ `transportBeforeRemove` 回调在每次 transport 目录删除前检查：`<dir>/packed-refs` 与 `<dir>/gc.pid` **都不存在**（transport 语境下没跑过 gc；
注入口里的 `gc.autoPackLimit=1` 在 v5 形态下 pack 数已到 4 仍不触发 = `gc.auto=0` 压过注入口，探针 `MODE=v5 ACCEL=transport`）；
④ 记录型 `git` 包装（AC-5c 的手法，只看 Lathe **直接**发起的调用）看到：transport 环境下的每次 `fetch` argv 含 `--no-auto-maintenance`，
环境里 `gc.auto=0` 与 `maintenance.auto=false` 出现在 `transportExtraConfig` 各键**之后**（下标更大）。**不**用包装器断言「没有 gc 子进程」：
git 派生的 gc/maintenance 经 exec-path 直接执行、不经过包装器，该断言恒真（§3.3.2）。
**先红**：保留注入口与种 have，只去掉两个配置键与 argv 开关（= v5 只删 gc 开关的回归形态）跑同一测试，① 必须失败（`could not get object info` /
`bad object HEAD`）、③ 必须看到 `packed-refs`，红日志存 `red-TestTransportFetchKeepsMirrorOnlyObjects.log`；再去掉种 have（v4 原样）同样必须红。
shell 版：`/opt/lathe-wt/evidence/f07/prd-v6-ac5d-accelerator-probe.{sh,log}`——`MODE=v4 ACCEL=mirror`（v5 PRD 的写法）**绿**，证明旧 AC 测不到缺陷；
`MODE=v4 ACCEL=transport` 与 `MODE=seedonly ACCEL=transport` **红**（任务提交丢失、fsck 5 行异常、每次刷新 transport 里出现 `packed-refs`，而包装器记录的
gc 子命令为 0）；`MODE=v5 ACCEL=transport`、`MODE=v5 ACCEL=mirror` **绿**。
`TestTransportFetchIsIncremental`（同文件）：约 2MB、≥200 个文件（对象数超过 `fetch.unpackLimit`，保证 fetch 落成 pack）的上游，首次建 mirror
之后上游每次只改一个文件、共 3 次，每次刷新前后取 `git count-objects -v` 的 `count + in-pack`；断言每次增量 ≤ 10 个条目（上游新增 3 个），
且桩记录的 `git-upload-pack` 响应体字节数每次 < 首次克隆的 10%。**先红**：去掉种 have（v4 形态）时每次增量约 200 个条目、响应体与首次同量级，
必须失败（探针 `MODE=gconly`：+206/+209/+212 条目、对象库 2012K→7928K）。
`TestTransportSeedHavesDoesNotBreakPrune`：上游删掉一个分支后刷新，mirror 的对应 `refs/remotes/origin/<b>` 被删（种在 `refs/lathe-have/*`
的旧值不影响 prune），mirror 里不出现任何 `refs/lathe-have/*`。

### 两个新平台的客户端（httptest 假服务器）

**AC-6（GitLab 开 MR）**
`TestGitLabCreateMR`：桩断言 `POST /api/v4/projects/acme%2Fdemo/merge_requests`、
JSON 含 `source_branch/target_branch/title/description`、头 `PRIVATE-TOKEN`；返回 iid=7 时
`PullRequest{Number:7, URL:<web_url>}`。`TestGitLabCreateMRReusesExisting`：先 `GET
.../merge_requests?state=opened&source_branch=…&target_branch=…` 命中则发 `Existing:true`，
且不再 POST。`TestGitLabCreateMRNoCommits`：400 + `"without any commits"` → `errors.Is(err, vcs.ErrNoCommits)`。

**AC-7（GitLab 合并状态映射）**
`TestGitLabGetPRInfoMapsState`：`opened`→`{Merged:false,State:"open"}`、
`closed`→`{false,"closed"}`、`merged`→`{true,"closed"}`、`locked`→`{false,"open"}`；
`BaseRef == target_branch`、`HeadSHA == sha`。

**AC-8（GitLab review 意见）**
`TestGitLabReviewFeedback`：`GET .../merge_requests/7/notes`（普通评论）与 `.../discussions`
（行内）合并；Key 前缀 `gitlab:note:`；`System:true` 的笔记被跳过；超过 10 页截断。

**AC-9（Gitea 开 PR / 状态）**
`TestGiteaCreatePR`（`POST /api/v1/repos/acme/demo/pulls`，头 `Authorization: token …`）、
`TestGiteaCreatePRReusesExisting`、`TestGiteaGetPRInfoMapsState`
（`merged:true`→Merged；`state:closed`→`closed`；`base.ref`/`head.sha` 映射）、
`TestGiteaReviewFeedback`（v6 扩充）：`GET .../issues/7/comments`（Key 前缀 `gitea:comment:`）、`GET .../pulls/7/reviews`（正文非空的总评，Key 前缀 `gitea:review:`；正文为空的被跳过）与每个 review 的 `GET .../pulls/7/reviews/<id>/comments`（行内，Key 前缀 `gitea:review-comment:`，`Path/Line` 来自 `path`/`position`）三者合并；非协作者作者的条目被过滤；超过 10 页截断。

**AC-10（保存时验证连通性）**
`TestVerifyGitLabIdentity`（桩 `/api/v4/user` 返回 `username`，`VerifyResult.OK` 且
`AccountName` 为该用户名）、`TestVerifyGiteaIdentity`（`/api/v1/user` → `login`）、
`TestVerifyGitLabUnauthorizedSurfacesMessage`（401 → `OK:false` 且错误文案含「令牌」）。

### 凭据与配置

**AC-11（base URL 校验与入库）**
`TestSaveIntegrationStoresBaseURL`：`PUT /api/integrations/gitlab {"token":"glpat-x","baseUrl":"https://gitlab.example.com/"}` → 200；
库里该行 `base_url = 'https://gitlab.example.com'`（去尾斜杠）、`secret_enc` 不含明文
（`SELECT secret_enc::text NOT LIKE '%glpat-x%'`）。
`TestBaseURLValidation`：gitlab 缺 baseUrl → 400；`http://evil.example.com` → 400；
`https://u:p@h` → 400；`https://h?x=1` → 400；`http://127.0.0.1:8080` → 200；
给 `github` 传 baseUrl → 400。

**AC-12（不泄明文）**
`TestIntegrationStatusNeverReturnsPlaintext`：`GET /api/integrations` 响应体不含任何已保存
token 的明文（含 gitlab/gitea/github）。
`TestKnownSecretsIncludesWebhookSecrets`：`config.KnownSecrets()` 含
`LATHE_GITHUB_WEBHOOK_SECRET` 的值（补上 §3.9 的既有缺口）。

**AC-12a（运行期 vcs token 进本任务的脱敏表，且不串任务，blocker 6.2）**
`TestFailureExcerptRedactsPerTaskVCSToken`（`internal/runner`）：两个用户 A/B 各配 GitLab 令牌
`glpat-A-<随机>`、`glpat-B-<随机>`（形态表认不出的随机串），各跑一个验证失败的任务，验证命令输出
里回显**本任务属主**的 token；断言 `failure_digests.excerpt` 对 A/B 都不含各自明文、含 `[REDACTED`；
且 `Pipeline.KnownSecrets` 在两次执行前后长度不变（未被追加）。在 `make test-race` 下两任务并发
跑同一测试无竞态报告。
`TestRebaseFailureReasonRedactsVCSToken`（`internal/runner`，mergepoll rebase 跟进路径）：桩 git 错误
文案回显 token → `failure_digests.reason` 不含明文。
先红：当前实现下前一测试的 excerpt 含明文。

**AC-12b（webhook 密钥不出现在日志与响应中）**
`TestVCSWebhookSecretNeverLoggedOrEchoed`：三平台各投一次验签失败与一次成功，捕获 `slog` 输出与
响应体，断言都不含所配密钥。

**AC-13（仓库平台字段）**
`TestCreateRepoWithVCSProvider`：先配 `gitlab` 凭据（`baseUrl: https://gitlab.example.com/`），
`POST /api/repos {"providerRepo":"group/sub/demo","vcsProvider":"gitlab"}` → 201，`vcsProvider=="gitlab"`、
`vcsBaseUrl=="https://gitlab.example.com"`；库里 `repos.vcs_base_url` 同值。未配 gitlab 凭据时 → 400；
请求体带 `vcsBaseUrl` → 400；未知平台 → 400；github + `a/b/c` → 400。
`TestRepoPathUniqueAcrossProviders`：同一用户已登记 `{github, acme/demo}`，再登记 `{gitlab, acme/demo}`
→ 409（文案含「两个平台」）；另一用户登记 `{gitlab, acme/demo}` → 201。
`TestUpdateRepoRejectsPlatformChange`：`PUT /api/repos/{id}` 带 `vcsProvider` 或 `vcsBaseUrl` → 400，
库里两列不变。
`TestRepoPlatformCheckConstraint`（`internal/store`）：直接 INSERT `('github', 'https://x')`、
`('gitlab', NULL)`、`('bitbucket', …)` 均被 CHECK 拒（SQLSTATE 23514）。

### Webhook 与信任边界

**AC-14（验签）**
`TestGitLabWebhookTokenMismatch401`（`X-Gitlab-Token` 错 → 401）、
`TestGitLabWebhookMissingToken401`、`TestGiteaWebhookSignature`（正确 HMAC 200 / 错误 401 / 缺头 401）、
`TestVCSWebhookUnknownSlug404`、`TestVCSWebhookRejectsNonPost`（405）。
`TestGiteaWebhookEventRecognition`（v6，第五轮 blocker 2；表驱动，桩的头与载荷按 §3.5.3「Gitea 事件识别」表的真实形状构造，验签正确）：
① `X-Gitea-Event: issue_comment` + `X-Gitea-Event-Type: pull_request_comment` + `is_pull:true` + `action:created` → `AcceptReview` 被调用一次，
`ev.PRNumber == issue.number`、`ev.Key == "gitea:comment:<comment.id>"`、`ev.Author == comment.user.login`；② 同上但不带 `X-Gitea-Event-Type`（旧版本）→ 同样被调用；
③ `issue_comment` + `is_pull:false` → `ignored`、未调用；④ `X-Gitea-Event: pull_request_comment` + `PullRequestPayload{review:{type:"pull_request_review_comment"}}`
→ `ignored`、投递记录原因含「review 事件本轮不触发」、**不是** `error`（证明没有按 issue/comment 字段去解析它）；⑤ `action:edited` → `ignored`；
⑥ `X-Gitea-Event: issue_comment` 但 `X-Gitea-Event-Type: issue_comment` 而载荷 `is_pull:true`（头与载荷不一致）→ `ignored`。
**先红**：按 v5 表的「`X-Gitea-Event: pull_request_comment`」实现时 ①② 必须失败（被当作无关事件）、④ 必须失败（解析错误或被调用）。

**AC-15（投递幂等）**
`TestVCSWebhookDeliveryDedupe`：同一 `X-Gitea-Delivery` 的普通 PR 评论（头与载荷同 AC-14 ①）连投两次，第二次响应
`{"status":"duplicate"}`，且 `AcceptReview` 只被调用一次。

**AC-16（信任判定 fail-closed）**
`TestGitLabReviewerWithoutMembershipRejected`（成员接口 404 → 不回流，投递记录写原因）、
`TestGitLabReviewerMembershipAPIErrorFailsClosed`（成员接口 500/超时 → 不回流）、
`TestGitLabReporterAccepted`（`access_level:20` → 回流）、
`TestGiteaReviewerNotCollaboratorRejected`、`TestGiteaReviewerWritePermissionAccepted`、
`TestSelfCommentIgnoredOnAllProviders`（三平台各一例：评论者 == MR/PR 作者 → 忽略；Gitea 例按 `comment.user.login == issue.user.login` 构造，
载荷**不含** `pull_request` 字段，即 1.20/1.22 形状）。
AC-16 的 Gitea 用例与 AC-17 的 `TestVCSWebhookCannotReachOtherOwnersTasks`、`TestVCSWebhookHandlersSetProvider` 的 Gitea 投递，一律使用 AC-14 ① 的头与载荷形状。

**AC-17（属主隔离）**
`TestVCSWebhookCannotReachOtherOwnersTasks`：slug-A 的 webhook 携带指向 B 名下任务的
仓库/编号 → 响应 `ignored`，B 的任务状态不变。
`TestAcceptReviewRequiresProvider`（`internal/runner`，v5，第四轮 blocker 3）：表驱动 `Provider ∈ {"", "bitbucket"}`，其余字段与
`inlineEvent` 相同、任务确实在 `pr_open` → `AcceptReview` 返回非 nil 错误（文案含「代码平台」）、任务状态不变、事件流无新增；
`Provider: github` 的同一事件 → `ReviewQueued`。先红：现实现没有 `Provider` 字段，编译失败即红。
`TestVCSWebhookHandlersSetProvider`（`internal/httpapi`）：三个 handler 各投一条可处理的评论，`fakeReviewIntake.calls[0].ev.Provider`
分别为 `github`/`gitlab`/`gitea`。
`TestTasksByVCSPRFiltersProvider`：用户 A 的 `acme/demo` 登记在 gitlab 且有 PR #5 的任务；按
`(github, acme/demo, 5)` 查 → 空；按 `(gitlab, acme/demo, 5)` 查 → 命中；用户 B 同参数查 → 空。
现有 `TestMachineTasksByPRIsolatesOwners` 一行不改仍全绿。

### 流水线与状态

**AC-18（受保护分支闸门，三平台一致）**
`TestProtectedBranchGateAllProviders`：provider ∈ {github,gitlab,gitea}、
branch ∈ {dev,test,main} → `ErrProtectedBranch`；且 `Push`/`ForcePush` 在这条路径上
**不调用 git**（用记录调用的假 `git` 断言调用次数为 0）。

**AC-19（经真实建任务入口，PR 开到仓库所在平台，blocker 2）**
`TestEnqueuePathsCarryRepoPlatform`（`cmd/lathe`，用 `queue_test.go` 的 `fakePipeline` 记录
`ExecuteParams`）表驱动三条入口——`queue.Enqueue`（Linear，`cmd/lathe/queue.go:101`）、
`queue.EnqueueInternal`（内置工单，`:142`）、`flow.Service.CreateFlow`（编排图，
`internal/flow/service.go:238`）——各自对一个 `{gitlab, group/sub/demo, https://gitlab.example.com}`
仓库建任务并让队列领单派发，断言 `ExecuteParams.Repo.VCSProvider=="gitlab"`、
`Repo.VCSBaseURL=="https://gitlab.example.com"`、`Repo.RepoKey()=="gitlab.example.com/group/sub/demo"`、
`Repo.CloneURL()=="https://gitlab.example.com/group/sub/demo.git"`。**测试不直接写任何平台列**——
平台只来自 `POST /api/repos` 登记（或等价的 `store.CreateRepo`）。
`TestPipelineOpensPROnRepoPlatform`（`internal/runner`）：同一仓库配置跑完整流水线，GitLab 桩收到
`POST /api/v4/projects/group%2Fsub%2Fdemo/merge_requests`，GitHub 桩收到 0 请求；
`tasks.pr_number`/`pr_url`（含 GitLab 主机）落库。
`TestPipelineRejectsCredentialInstanceMismatch`（v4 改写，第三轮 blocker 2）：仓库钉在 `https://a.example`，用户凭据改为
`https://b.example`。表驱动五个入口（`EntryTriage` 新任务、`EntryImplement` 续跑（Rebuild）、`EntryCommit`、`EntryVerify`、`EntryPush`），
每个子用例先把任务置到该入口对应的现场（沿用 `pipeline_resume_test.go` 的手法），再 `Execute`：断言
① `Execute` 返回 nil（失败已在 pipeline 内落库，与其它 `p.fail` 路径一致）；② 任务 `state=='failed'`、
`failure_stage=='vcs_credential'`、`failure_reason` 同时含 `https://a.example` 与 `https://b.example`、不含 token；
③ 事件流里该任务的转移序列**只由 `state.go` 已有的边组成**：分别是 `queued→triaging→failed`、`queued→implementing→failed`、
`queued→implementing→failed`、`queued→verifying→failed`（经 `stageVerify` 的 `→verifying`）、`queued→verifying→failed`，`failed` 那次的
actor 为 `system`；④ 两个桩收到 0 次请求、假 agent 被调用 0 次、`.mirrors/` 下 `EnsureMirror/Reattach/Push` 调用 0 次（记录型 `git`）；
⑤ 若带 review 意见（`EntryImplement` 子用例），`payload.review_keys` 未被写入（意见没有被标记为已处理）。
`TestPipelineGitHubCredentialErrorStaysQueued`：github 仓库、`Clients.GitHub` 返回错误 → `Execute` 返回错误，任务仍是 `queued`，
错误文案「获取 GitHub 客户端失败（请在设置里配置并验证凭据）」逐字不变（GitHub 存量行为的回归）。
`TestPipelineVCSTransientErrorStaysQueued`：gitlab 仓库、`VCS` 返回非哨兵错误（如 `errors.New("db down")`）→ 同上留在 `queued`。
`git diff cff5332 -- internal/task/state.go` 输出为空。

**AC-20（合并检测跨平台）**
`TestMergePollUsesRepoPlatform`（仓库 gitlab，查 GitLab 桩，状态推进到 merged）、
`TestMergePollGitLabClosedUnmerged`（→ blocked_dep，走既有 F2.3-AC2 路径）、
`TestForcePushDetectedOnGitLab`（先后两次 HeadSHA 不一致 → 触发 F4.4 rebase 跟进）、
`TestMergePollSkipsOnInstanceMismatch`（凭据地址与仓库不一致 → 本轮报错跳过，`pr_open` 不变，GitLab 桩 0 次请求）。
**v4 新增（第三轮 blocker 3(1)）**——用 AC-5b 的 `git http-backend` 桩当 GitLab 远端（只接受各自 token 的 Basic 鉴权），
`fakeClients` 额外实现 `VCSClients` 并按 userID 返回不同 token：
`TestGitLabMergedFollowupRebasesSuccessorWithItsOwnersAuth`：前驱 P（属主 A，gitlab）与后继 S（属主 B，同仓库、`base_ref=P` 分支，`pr_open`）；
GitLab API 桩对 P 报 merged → 一轮 `pollOnce` 之后断言 ① `resolveMirrorTip` 的 fetch 带 **A** 的 token；② S 的 `RebaseOnto` fetch 与
`ForcePush` 带 **B** 的 token，远端 `refs/heads/<S 分支>` 等于 rebase 后的 HEAD；③ S 的重验走完（`pr_open`），其 `Execute` 的
setup 以 B 的身份调到 `VCS`；④ 桩上没有任何一次请求带了「另一个属主」的 token。
`TestGitLabRebaseFollowupInstanceMismatchFailsSuccessor`：B 的凭据地址改成别的实例 → S 落 `failed`、`failure_stage=='vcs_credential'`，
S 的 worktree 与远端分支都未改动，`PropagateBlocked` 照常执行。
`TestGitHubRebaseFollowupUnchanged`：既有 `rebase_followup_test.go` 全部用例不改仍绿，且 github 仓库的跟进路径上 `Clients.GitHub`
调用次数与 cff5332 相同（不新增调用，§3.4.2 第 3 条）。

**AC-20b（凭据地址变更后，在途现场仍落在原 mirror，blocker 3）**
`TestRepoKeyStableAfterCredentialBaseURLChange`（`internal/runner`）：gitlab 仓库建好 mirror 与 worktree
后，把用户凭据 `base_url` 改成另一地址；断言 reaper 的 `Inspect/Discard`、`queue` 重建路径的
`Discard`、mergepoll 的 `Remove`、review 二轮的 `Reattach` 拿到的键都是
`gitlab.example.com/group/sub/demo`（用记录键的假 `Worktrees` 断言），且 `.mirrors/` 下没有新建第二个
mirror 目录。

**AC-21（PRD 规划路径的克隆地址与鉴权，v4 改写，第三轮 blocker 3(2)）**
`TestPRDCloneURLUsesRepoProvider`（`internal/prd`）：`RepoForPRD` 只读 `repos` 行（`StoreAdapter` 不碰凭据——这一句保留，指的是
仓库信息的来源）；鉴权来自新字段 `Service.GitAuth`，测试注入一个记录 `(userID, target)` 的假 `GitAuthSource`。仓库
`{gitlab, group/sub/demo, base https://gitlab.example.com}`、PRD 属主 U → `EnsureMirror` 收到的 key 为 `gitlab.example.com/group/sub/demo`、
cloneURL 为 `https://gitlab.example.com/group/sub/demo.git`、auth 非空且等于假件返回值；假件被调用一次，参数 `userID==U`、
`target=={gitlab, https://gitlab.example.com}`。github 仓库 → key == `acme/demo`、cloneURL 仍为 `git@github.com:acme/demo.git`、
auth 零值、假件**调用 0 次**。阶段 A（`needCheckout=false`）与对抗复核 → 假件调用 0 次。
`Service.GitAuth == nil` + gitlab 仓库 → 本轮失败、错误含「未装配代码平台鉴权」，`EnsureMirror` 调用 0 次；假件返回
`vcs.ErrInstanceMismatch` → 本轮失败、`FailPRDRun` 收到含该错误的消息。`newService` 注入的 `CloneURL func(RepoInfo) string`
非 nil 时仍以它为准（`service_test.go:257` 的替换语义不变）。
`TestPRDGitAuthWiring`（`cmd/lathe`，`go/parser` 检查，沿用 `assembly_test.go` 的手法）：`serve` 里构造 `prd.Service` 的字面量
（或 f09 合入后的 `buildPRDService`）含 `GitAuth:` 键，值为 `prdGitAuth{…}`；`prdGitAuth.GitAuth` 对一个用户 A 的 gitlab 凭据经
真实 `creds.Factory` 返回非零 auth，对用户 B 的同一调用返回 B 自己的（不串属主）。

### 迁移与工程纪律

**AC-22（迁移成对且可往返）**
```
lathe_migrate up && lathe_migrate down && lathe_migrate down && lathe_migrate down \
  && lathe_migrate up && lathe_migrate up && lathe_migrate up
```
期望：每次调用退出码均为 0；三次 down 之后 `pg_dump --schema-only` 与本线迁移之前（dev 基线，当前最大号 0031）的 schema
dump `diff` 为空；最后 `\d repos` 含 `vcs_provider`、`vcs_base_url` 与原样的 `repos_user_repo_unique`。
`TestMigration0050DownRefusesWithNonGitHubRepos`（`internal/store`）：存在一条 gitlab 仓库时执行
0050 down → 报错且信息含该仓库路径，`repos` 表结构与数据不变（事务回滚）。
`grep -rn 'ON CONFLICT (user_id, provider_repo)' --include=*_test.go internal | wc -l` 仍为 9，且这
9 个文件相对 `cff5332` 在该行无改动（证明唯一约束未动、既有 fixture 不受影响）。
另外 `go test ./internal/store/ -run 'TestLoadMigrations|TestSplitRealMigrations' -count=1`
全绿——前者对**每一条**内嵌迁移断言 up/down 非空（`internal/store/migrate_test.go:59-92`），
后者对真实脚本做语句切分自检（`internal/store/migrate_notx_test.go:121`）；
新增的三条迁移一旦漏写 down，前者直接失败。

**AC-23（不引入依赖）**
`git diff --stat cff5332 -- go.mod go.sum web/package.json web/pnpm-lock.yaml` 输出为空。

**AC-24（基线全绿 + 竞态）**
`make lint` 退出码 0；`make test` 全绿；`make test-race` 全绿
（涉及 `internal/runner`、`cmd/lathe`，按 AGENTS §3 的并发条款）。
前提核对（GitHub 路径不变的依据，§3.3.1 末）：`git diff cff5332 -- '*_test.go' | grep '^[-+].*CloneURL:'` 输出为空
（128 处测试注入一处未改）；`git diff cff5332 -- '*_test.go' | grep -c '^-.*\(t\.Errorf\|t\.Fatalf\|want\)'` 为 0
（既有测试的断言一行未删改，测试侧只有 §3.3.2 表里的补参改动与 §3.1 表里的 5 处 `ReviewEvent` 改动，后者 3 处为新增 `Provider:` 一行）。
`git diff cff5332 -- '*_test.go' | grep '^+.*Provider: *vcs.ProviderGitHub' | wc -l` 为 3（§3.1 表的三处，不多不少）。

**AC-24b（与 f09 的编译期兼容，第二轮 blocker 3）**
`TestClientsInterfaceUnchanged`（`internal/runner`）：`reflect.TypeOf((*Clients)(nil)).Elem()` 的方法集恰好是
`{GitHub, Tracker}`，`GitHub` 的签名为 `func(context.Context) (GitHubAPI, error)`；`GitHubAPI` 的方法集恰好是
`{CreatePR, GetPRInfo}`。同文件放一个照 f09 形态写的假件
`type f09StyleClients struct{}`（只实现 `Tracker` 与 `GitHub`，后者返回嵌入 `GitHubAPI` 的空结构体）并
`var _ Clients = f09StyleClients{}`——编译通过即证明 f09 的 `e2eClients` 在 f07 合入后的 dev 上仍能编译。
`git diff cff5332 -- internal/runner/pipeline_test.go internal/runner/pipeline_internal_test.go | grep -c 'func (.*Clients) '` 为 0。
`TestResolveVCSDispatch`：github → 调到 `GitHub(ctx)` 且 auth 零值；gitlab + 未实现 `VCSClients` 的假件 → 错误含「不支持代码平台」；
gitlab + 实现了的假件 → 调到 `VCS` 且 target 原样传入。

**AC-25（前端产物同步）**
`make ui && make build` 退出码 0；`internal/webui/dist` 有更新；PR 附仓库页与设置页截图
各一张（平台下拉 + 实例地址输入框 + 两个新 webhook 地址）。

---

## 6. 风险与代价

| 风险 | 代价 / 缓解 |
|---|---|
| **方法签名变更**（EnsureMirror/Push/ForcePush/RebaseOnto/Reattach 增加 auth 参数；`CreateParams` 加 `Auth` 字段；生产 12 处 + `pipeline.go:617` 一个字面量，测试 32 处；另有 `AcceptReview` 的 5 处（3 处补 `Provider`），合计 37 处，§3.3.2 表与 §3.1 表） | `ExecuteParams.CloneURL`/`CreateParams.CloneURL` 保留，128 处测试注入不改；`Create` 签名不变，46 个 `CreateParams` 字面量全是键值形式，49 处测试调用不改（AST 实测）；零值 `GitAuth` 的执行序列与今天完全一致（AC-5c）。把 auth 塞进 `RepoConfig`（被到处复制）仍否决；放进一次性入参 `CreateParams` 的理由与脱敏实测见 §3.3.2 |
| **GitLab/Gitea 的凭据错误分两类处理**（§3.4.1） | 实例不一致 / 凭据缺失 → 在入口阶段第一次合法转移后落 `failed`（`vcs_credential`）；暂时性错误 → 与 GitHub 同口径留 `queued`。代价：①多一个阶段码，`PlanRetry` 对它走 `default`，`EntryPush` 入口失败的任务重试时会多跑一次验证；②GitHub 的「凭据未配留在 queued、只进日志」存量行为**不改**（改它是 GitHub 行为变化，超出本线），两平台对「凭据缺失」的可见性不对称——建议另立债务；③`stageVerify` 的 `ChangedFiles`（`pipeline.go:912`）在转移之前，现场缺失时会先以 `list_changes` 失败、掩盖凭据错误（存量顺序，不调） |
| **rebase 跟进按后继属主解析凭据** | 每个 GitLab/Gitea 后继多一次 `ForUser`+`VCS`（读库 + 解密，30s 缓存）。GitHub 后继不新增调用（§3.4.2 第 3 条），保持逐字节不变。`succ` 停在 `queued` 时凭据错误转 `failed` 不合法，沿用 `failRebaseFollowup` 的存量处理（只记日志），不新增状态机边 |
| **GitLab/Gitea 信任模型只能靠成员查询**（无 `author_association`） | 每次回流多一次 API 调用；token 缺成员读权限时**所有**回流被拒（fail-closed）。缓解：保存凭据时的验证文案提示所需最小权限；投递记录写拒绝原因，面板可查。这是安全的正确取舍，不是缺陷 |
| **一个用户只能接一个 GitLab/Gitea 实例** | 数据模型（`(user_id, kind)` 唯一）决定的。多实例需求出现时要给凭据加实例维度（新迁移 + 缓存键变更），本轮明确不做 |
| **凭据 `base_url` 改了** | 仓库行上钉死登记时的地址，镜像键不变，在途现场的 Inspect/Discard/Remove/Reattach 仍落在原 mirror（AC-20b）；带 token 的操作因实例不一致被拒（AC-19），不会把 A 实例的 token 发给 B。代价：实例真的迁移了（换域名）时，属主要删除并重新登记仓库，旧 mirror 成为孤儿目录（不误删，人工清理） |
| **同一用户同一路径不能登记在两个平台** | 保留 `repos_user_repo_unique` 的代价（§3.2、§4）。镜像键里有主机，所以磁盘层面并不冲突，限制完全来自唯一约束；放开它要与 f12 的仓库归属改造一起做 |
| **0050 down 在有 GitLab/Gitea 仓库时拒绝执行** | 回滚需要人先处理这些仓库。比「静默把它们变成 GitHub 仓库、下一次派发推错平台」安全；也不能自动删（被 `internal_issues/prds` RESTRICT 引用、级联删任务） |
| **带 token 的 git 流量不走任何 HTTP 代理** | transport 环境白名单不含 `HTTP(S)_PROXY`（否则代理本身就是外泄面）。需要出网代理才能访问自建 GitLab/Gitea 的部署本轮不支持；将来要支持应做成 Lathe 侧配置项（有消费方、登记 README），不读环境/仓库 config |
| **transport 的 fetch 前后各多一步本地 ref 搬运 + 一次 mirror 语境 gc**（v5 改写；v4 写的「多一步本地 ref 导入，毫秒级」漏了协商——没有 ref 的 transport 每次全量重传，见 §3.3.2） | 刷新前 `for-each-ref` + `update-ref --stdin` 种 have（ref 数量级的本地操作）、刷新后同样的方式导入并 prune、再 `gc --auto`（多数时候只是阈值检查立即返回；触发时与 GitHub 原路径 fetch 顺带的那次 gc 是同一份开销）。网络与磁盘与原 `fetch --prune origin` 同量级（AC-5d 断言增量）。若种 have 的步骤将来被误删，AC-5d 的「不重传历史对象」会红；若 transport 的 gc 开关被误删，AC-5d 的「旧对象仍在」会红——两种 v4 缺陷都有测试兜住。prune 语义由 Lathe 自己实现，AC-5b ② 覆盖。不选「每次带鉴权操作前在锁内校验并重写 mirror/worktree config」：要穷举所有可能影响网络的键，且随 git 版本增长（`curloptResolve` 即反例），无法证明完备 |
| **同 OS 用户的残余面** | agent 与 Lathe 同一 uid 时，同用户进程可读 `/proc/<lathe pid>/` 与内存——存量部署前提，非本线引入；本线只保证 agent 不能借 git 拿 token。更强隔离（独立 uid / 沙箱）另立债务 |
| **git 加固只覆盖带 token 的调用** | GitLab/Gitea 路径全部加固；GitHub 路径为保持逐字节不变不加，但其 hook 可执行代码、可用宿主 SSH agent 是**存量**暴露面（与本线无关，本线不放大它）。建议另立债务条目 |
| **`credential.<base>.helper` 按 URL 前缀匹配** | 键只来自 Lathe 的 `GIT_CONFIG_COUNT`（transport 里没有别的来源），base 带路径前缀（`https://host/gitlab`）时按前缀生效；AC-5b 覆盖带前缀与不带前缀两种 |
| **三份仓库配置读取必须同步**（`cmd/lathe/queue.go` 、`runner.NewRepoLookup`、`store.GetRepo`→`prd.StoreAdapter`） | 既有技术债（`internal/runner/mergepoll.go:29-36` 注释已承认）。两列都要三处都读，遗漏会让 GitLab 仓库在某条路径上退化成 GitHub。AC-19（派发）、AC-20/AC-20b（mergepoll、reaper）、AC-21（PRD）各覆盖一处 |
| **GitLab/Gitea webhook 载荷字段按文档建模** | 无法在本仓内取证真实载荷。缓解：桩 JSON 与官方文档逐字段对照；PR 附一次真实实例的投递记录（若评审者能提供），否则在 docs 里标注「字段依据官方文档，实测待补」 |
| **`Provider.Ready` 不检查新平台凭据** | 只在启动时给管理员一条告警，不含新平台。缺失会在派发时以明确错误暴露（`creds: 凭据未配置（gitlab）`）。不扩 `Ready` 是为了不让「配了 GitHub 但没配 GitLab」的部署在启动时告警 |
| **合并检测对多平台是串行 API 调用** | 每个 `pr_open` 任务一轮一次调用，间隔 45s（`internal/runner/mergepoll.go:71-76`）。任务量增长后需要按平台分批/并发，本轮不做 |
| **PRD 规划路径需要平台化** | `prd.Service.CloneURL` 注入点保留，签名改为 `func(RepoInfo) string`，影响 `service_test.go:257` 一处构造；`prd.Worktrees.EnsureMirror` 加 auth 参数，影响 `prdwiring.go:25` 与 `service_test.go:184` 假件；新增 `prd.Service.GitAuth`（接口 `GitAuthSource`）与 `runSpec.userID`，装配一行在 `main.go:425-434`；与 f03（同字面量加 `Usage`）、f09（把字面量提成 `buildPRDService`）是文本相邻，按 §3.4.3 末的口径合并；AC-21 覆盖 |

---

## 7. 文档改动（PR 内一并提交）

1. **新增 `docs/features/vcs-gitlab-gitea.md`** —— 本 PRD 的定稿版（结论先行、理由、代价）。
   `docs/features/` 目录当前不存在，本 PR 一并建立。
2. **`docs/02-design.md`**
   - §2 系统组件图下的 Integrations 一行补 `GitLab / Gitea token`；
   - §4 数据模型：`integrations` 注释补 `base_url` 与新 kind；`repos` 加 `vcs_provider`、
     `vcs_base_url`（登记时钉死，任务经 `repo_id` 继承平台）；
   - §7 PR 评论回流：补「三个平台，验签与信任判定各自实现、回流入口共用」。
3. **`README.md`**
   - §5「在设置页配置凭据」补 GitLab / Gitea 令牌与实例地址；
   - §7.5 补 `/webhooks/gitlab/{slug}` 与 `/webhooks/gitea/{slug}` 的配置步骤
     （GitLab：项目 Settings → Webhooks，勾 Note events，填 Secret Token；
     Gitea：仓库 Settings → Webhooks，选「自定义事件」勾 **Pull Request Comment**（Gitea 以 `X-Gitea-Event: issue_comment` + `is_pull` 发出，
     Lathe 按此识别），填 Secret；并写明「在 PR 对话里发评论触发二轮；Gitea 的 review（含行内评论）本身不触发，但会在二轮时一并带给 agent」）；
   - 环境变量表**不新增行**（本轮无新变量），但在凭据段落说明「实例地址在设置页填，不走环境变量」。
4. **`CHANGELOG.md`** 的 `[Unreleased] / Added` 写一条用户可感知的变更：
   「仓库可选代码平台（GitHub / GitLab / Gitea），支持自建实例；GitHub 行为不变」。
5. **迁移文件注释**：0050/0051/0052 各写清「为什么是这一列、为什么不做回填、down 的语义」
   （0050 写清 down 为何拒绝而不删）。
6. **号段说明**：0050 与 dev 当前最大号 0031 之间跳号，是编排方给并行线分配的号段；PR 描述写明
   「合入时按 dev 当时最大号 +1 重编号，只改文件名不改内容」（与 f12 同一口径）。

---

## 8. 建议的实现顺序（给执行阶段的参考）

1. `internal/vcs` 契约 + GitHub 别名 + `ReviewEvent.VCS()` + 编译期断言（AC-1..AC-4、AC-2b）；`AcceptReview` 换类型并要求 `Provider`，同时给 §3.1 表的 3 处测试补 `Provider`（`TestAcceptReviewRequiresProvider`）。**先红后绿从这里开始。**
2. 迁移 0050/0051/0052 + store/API 字段（AC-11..AC-13、AC-22）。
3. git 鉴权与 transport 隔离（AC-5（含 `TestCreateEnsuresMirrorWithAuth`）、AC-5b、AC-5c、AC-5d；先把 AC-5b 在 v2 形态上、AC-5d 在「只删 gc 开关」与 v4 两种形态上经 `transportExtraConfig` 跑红再实现），然后 `resolveVCS`/`creds.Clients.VCS` 分派与两个错误哨兵（AC-24b）、`setup` 的错误分类与 `vcsGate`（AC-19 的 mismatch 三条）、运行期脱敏（AC-12a）。
3b. PRD 规划路径：`GitAuthSource`、`runSpec.userID`、`prdGitAuth` 装配（AC-21）。
4. GitLab 实现（AC-6..AC-8、AC-10）+ Gitea 实现（AC-9）。
5. webhook 路由 + 信任判定（AC-14..AC-17）；Gitea 先写 `TestGiteaWebhookEventRecognition` 并按 v5 的错误头跑红。
6. 合并检测跨平台：`pollTask`、`handleMerged→onMerged→resolveMirrorTip` 透传、`rebaseFollowupOne` 按后继属主解析、`failRebaseFollowup` 加参（AC-20 全部，含 v4 新增三条；AC-20b）+ 受保护分支闸门回归（AC-18）。
7. 前端（AC-25）与文档（§7）。
8. `make lint && make test && make test-race`（AC-24），收证据到 `/opt/lathe-wt/evidence/f07/`。

---

## 9. 评审答复

### 9.1 第一轮（v2）


> 每条：结论 → 改了哪里 → 证据。全部采纳；其中 B6.2 的「webhook 密钥运行期登记」一半不采纳，理由在该条。

| # | 评审意见 | 结论 | 改动位置 | 证据 |
|---|---|---|---|---|
| B1 | `type ReviewEvent = vcs.ReviewEvent` 后 `Actionable()` 不能编译 | **采纳**。`ReviewEvent` 不做别名，保留为 github 本地类型，`Actionable/TrustedAssociation/白名单` 一字不动；新增 `VCS()` 转成中立事件；`AcceptReview` 改吃 `*vcs.ReviewEvent`（契约本就是「已判可处理」）。不选「搬进 vcs 当方法」（信任逻辑进契约包）也不选「改自由函数」（改调用点、违反 AC-1）。代价如实列了 5 处测试改动，都在 `integration/github` 之外 | §2.5-2、§3.1、§3.5.3、AC-2、AC-2b | `/opt/lathe-wt/evidence/f07/prd-v2-alias-compile-probe.log`：v1 形态报 `cannot define new methods on non-local type ReviewEvent`；v2 形态编译运行通过 |
| B2 | `tasks.vcs_provider` 没有写入方 | **采纳，并换方案**：删掉 `tasks.vcs_provider`。平台与实例落在 `repos` 行上、只在登记时写、无更新路径；`tasks.repo_id` 创建时钉死 ⇒ 平台被唯一决定，三个 `Create` 入口不用改且天然正确。新 AC-19 经 Linear / 内置工单 / 编排图三条真实入口验收，测试不直接写平台列 | §3.2 开头、§3.4、§3.8、§3.10、AC-19 | 建任务只写 `repo_id`：`internal/task/machine.go:198-205`；`UPDATE repos` 非测试代码只有 `internal/store/query.go:402` 一处 |
| B3 | 镜像键 / clone URL 要的主机从哪来，调用链拿不到凭据 | **采纳**。主机在登记时从凭据复制进 `repos.vcs_base_url`；`RepoConfig`/`prd.RepoInfo` 加两个非密字段，三个加载器只读 `repos`；`RepoKey()/CloneURL()` 是纯函数。凭据地址事后改动：键不变（在途现场 Inspect/Discard/Remove/Reattach 仍落原 mirror），带 token 的操作因实例不一致被拒并落 failed。v1 §6「旧 mirror 保留」那条已替换 | §3.3.1、§3.4、§3.5.2、§6、AC-19、AC-20、AC-20b、AC-21 | 调用点：`reaper.go:448/484/516`、`queue.go:428/480`、`mergepoll.go:171/328/342`、`pipeline.go:506/674/678`、`prd/service.go:516-521`、`prd/adapter.go:37-58` 都已持有仓库行 |
| B4 | 删 `repos_user_repo_unique` 打断 9 个测试辅助；0050 down 没写；0051 一次做了三件事 | **采纳**。①唯一约束不动（代价：同一用户同一路径不能跨平台登记，写进 §4/§6）。②0050 down 写全：有非 GitHub 仓库时 `RAISE EXCEPTION` 拒绝（删列会让它们静默变 GitHub 仓库、推错平台；DELETE 又被 RESTRICT 外键挡且级联删任务），否则精确还原。③拆分：0050 = 仓库平台实例（两列由同一条 CHECK 绑定，不可拆）；0051 = 凭据加 `base_url` 列；0052 = 放开新 kind（含只对新 kind 成立的必填约束）。也顺带消掉了 v1 的「0052 给 tasks 加列」 | §3.2、AC-22 | 9 处 `ON CONFLICT (user_id, provider_repo)` 已逐一核对（评审所列行号无误）；外键 RESTRICT：`0021_internal_issues.up.sql:21`、`0023_prds.up.sql:25`；`DO $$` 由 `internal/store/migrate.go:308-320` 支持 |
| B5 | 与 f12 的边界 | **采纳**。本线不再改/删/重命名任何既有约束，只给 `repos` 加两列 + 一条新 CHECK；f12 只加 `team_id`（0066）与索引；列名、约束名、迁移号段全不相交，任意合并顺序都能单独合入。`TasksByPR` 签名也保留（新增 `TasksByVCSPR`），避免与任何邻居在同一函数上冲突。已写进 §4 | §4、§3.5.4 | `f12-team-roles.md:36`、`:267`、`:279`、`:731`；`f06-multi-cli.md:382`（`agent_driver`，同样不相交）；`f08-tracker-yunxiao.md:208/:381` 明确不碰 `integrations_kind_check` |
| B6.1 | agent 写 mirror 的 `pre-push` hook 读到 `LATHE_VCS_TOKEN` | **采纳，且问题比描述的更大**：本机复现还发现仓库级 `credential.helper` 能在 `approve` 时拿到口令、agent 可改 `remote.origin.url`。改为：token 经管道 fd 交给 helper，不进任何环境变量；带鉴权的 git 调用强制 `core.hooksPath=/dev/null`、重置 `credential.helper`、只对钉死的实例给口令、`protocol.allow=never`+仅 https、清空 `core.sshCommand/fsmonitor/askPass`；远端 URL 显式取自 `repos` 行。新 AC-5b 用真实 git + `git http-backend` 端到端验收，含「先红」 | §3.3.2、§3.9-5、AC-5、AC-5b、§6 | `/opt/lathe-wt/evidence/f07/prd-v2-git-hardening-probe.{sh,log}`（git 2.34.1：未加固 `PRE-PUSH HOOK saw LATHE_VCS_TOKEN=SECRET`、`STOLEN: password=SECRET`、origin 被改；加固后均不发生）；`prd-v2-fd-credential-probe.log`（fd 3 helper 可用） |
| B6.2 | 运行期解出的凭据登记进脱敏层没有设计也没有 AC | **vcs token 部分采纳**：不改共享的 `Pipeline.KnownSecrets`，改为 `runCtx.secrets` 每任务一份，`failureExcerpt` 合并两者；mergepoll rebase 失败的 reason 用同一个 `GitAuth` 的值脱敏。AC-12a 覆盖双用户、不串任务、`-race`。**webhook 密钥运行期登记不采纳**：它们只在 httpapi 验签时使用，从不进入 agent、验证输出或失败记忆，runner 脱敏表里登记它们不保护任何输出面；改为 AC-12b 直接断言 handler 日志与响应体不含密钥。环境兜底的 `GitHubWebhookSecret` 仍补进 `config.KnownSecrets()` | §3.9-2、AC-12、AC-12a、AC-12b | `pipeline.go:120-129`、`main.go:587`、唯一消费点 `redact.go:559`、`mergepoll.go:640`；webhook 密钥消费方只有 `internal/creds/creds.go:116/150` 与 webhook handler |

> 注：B6.1 的 v2 方案（`GIT_CONFIG_COUNT` 覆盖）已被第二轮 blocker 1 证伪，由 §3.3.2 v3 取代；上表保留作为历史。

### 9.2 第二轮（v3）

> 三条全部采纳。结论 → 改了哪里 → 证据。

| # | 评审意见 | 结论 | 改动位置 | 证据 |
|---|---|---|---|---|
| R2-1 | `GIT_CONFIG_COUNT` 注入盖不住 `http.<更具体 URL>.proxy/sslVerify`；`include.path`、`insteadOf`、`curloptResolve` 同类绕过 | **采纳，并按评审建议的第一种做法改设计**：带鉴权的 git 调用在 Lathe 私有、每次新建、**没有 config 文件**的 transport GIT_DIR 里运行（对象经 `GIT_OBJECT_DIRECTORY` 直接读写 mirror），`GIT_CONFIG_NOSYSTEM=1` + `GIT_CONFIG_GLOBAL=/dev/null` + 空 `HOME` + 白名单环境，配置来源只剩 Lathe 的 `GIT_CONFIG_COUNT`；ref 用本地 `update-ref` 回写 mirror；远端 URL 显式取自 `repos` 行。不选「锁内校验并重写 mirror/worktree config」：要穷举所有影响网络的键且随 git 版本增长，`curloptResolve` 就是反例，无法证明完备；而「没有可写来源」可以用一条与版本无关的断言（`--show-origin` 只有 `command line:`）证明。`ForcePush` 改为显式 lease 期望值，refspec 不带 `+`（原型踩到 `+` 会绕过 lease）。AC-5b 改为表驱动、每种毒一个子测试，补齐 proxy、sslVerify、sslCAInfo（经 include.path）、insteadOf、helper、origin.url、hooks 与「配置来源只有命令行」总断言，先红在 v2 形态上跑 | §3.3.2（重写）、§3.9-5、§6、AC-5、AC-5b、AC-5c | `/opt/lathe-wt/evidence/f07/prd-v3-urlmatch-bypass-probe.{sh,log}`：A1 复现评审结论（`sslVerify -> false`、`proxy -> http://127.0.0.1:9`），A2 显式 URL 被 insteadOf 改写到 evil 主机，B0 v2 形态下 agent 代理收到 receive-pack 请求；B1/B2 v3 形态下五项有效值全空、代理 0 连接、远端收到正确 Basic 鉴权。`prd-v3-design-e2e-probe.{sh,log}`：首次建 mirror / fetch / push / force-with-lease 全链路在全部投毒下成功，代理 0 连接、helper 未执行、transport 期间 hook 0 次。`prd-v3-transport-ops-probe.log`：lease 期望值匹配成功、过期被 `rejected (stale info)`、upstream 正确。`prd-v3-configless-transport-probe.log`：无 config 文件时来源只有 `command line:`，写 ref 不会生成 config |
| R2-2 | clone URL 注入点前后矛盾；128 处测试经 `CloneURL:` 注入本地路径；影响面漏算 | **采纳**。`ExecuteParams.CloneURL`、`CreateParams.CloneURL`、`EnsureMirror` 的 `cloneURL` 参数**全部保留**，写明生产谁写（`queue.go:487`、`mergepoll.go:477`、`pipeline.go:618`）谁读（`pipeline.go:506/674`、`worktree.go:436`）；只把三处拼接实现改为调 `RepoConfig.CloneURL()` / `RepoInfo.CloneURL()`（单一真相源），`loadRepoConfig` 签名不变。`prd.Service.CloneURL` 保留、签名改为 `func(RepoInfo) string`。删掉 v2「16 处调用，7 处在测试」的错误计数，按实测重列：生产 12 处、测试 32 处补参，128 处注入不改。「GitHub 逐字节不变」改为以四条可机械核对的前提论证，新增 AC-5c（零值 auth 的 git argv 序列与 cff5332 逐行相同）与 AC-24 的 diff 前提核对 | §3.3.1、§3.3.2 签名表、§6、AC-5、AC-5c、AC-21、AC-24 | `grep -rn 'CloneURL:' --include=*_test.go internal cmd \| wc -l` → 128（26 个文件）；直接读 `params.CloneURL` 的生产点 `worktree.go:436`、`pipeline.go:506/618/674`；签名表每格计数来自 `grep -rn '<方法>(' --include=*_test.go internal cmd \| grep -v 'func '` |
| R2-3 | 改 `Clients.GitHub`/`runCtx.gh` 与 f09 的 `e2eClients` 编译期冲突未登记 | **采纳**。`runner.Clients`、`runner.GitHubAPI`、`runCtx.gh` 保持 cff5332 原样；新平台走可选接口 `VCSClients`（类型断言取，同 `ReviewFeedbackSource` 先例），唯一分派入口 `resolveVCS`，GitHub 仍调 `c.GitHub(ctx)`。因 `PRParams/PullRequest/PRInfo` 是别名，`GitHubAPI` 与 `vcs.Client` 方法集相同、可互相赋值。结果：既有三个假件与 f09 的 `e2eClients` 都零改动，两线任意顺序合入 dev 都能编译，PR 描述写明这一点、不需要约定合入顺序。新增 AC-24b 钉住接口方法集并内置一个 f09 形态的假件 | §3.4（重写）、§4 边界表、AC-24b | `/opt/lathe-wt/evidence/f07/prd-v3-clients-compat-compile-probe.log`：保留原接口 + 新增 `VCSClients` + 双向赋值断言 + f09 形态 `e2eClients`，`go vet && go build` → `BUILD-OK`；`f09-notify-channels.md:1125-1127`；`f08-tracker-yunxiao.md:434-435`（f08 不碰 `GitHub()`） |


### 9.3 第三轮（v4）

> 三条都属实，全部采纳。B1 里「改 `Create` 签名会波及约 88 处」这一估算不采纳（不需要改签名，实测 0 处测试改动），理由与证据在该行。
> 全部取证基于 `/opt/lathe-wt/f07-vcs` @ `cff5332`。

| # | 评审意见 | 结论 | 改动位置 | 证据 |
|---|---|---|---|---|
| R3-1 | `Create` 内部调 `EnsureMirror`（`worktree.go:436`），是新平台首次克隆的主路径，但签名表漏了它；v3 又说 auth 不进 `CreateParams`，于是 `Create` 拿不到鉴权；改签名会波及约 88 处测试，「32 处」不成立 | **采纳缺陷，不采纳影响面估算**。`Create` 签名不变，`CreateParams` 新增 `Auth vcs.GitAuth`，`:436` 改为传 `p.Auth`；生产唯一构造点 `pipeline.go:617` 补 `Auth: rc.auth`。测试侧：49 处 `Create` 调用、46 个 `CreateParams{…}` 字面量**全部是键值形式**，新字段取零值 = 原无鉴权路径，**0 处要改**，「32 处」仍然成立（这次是带着 `Create` 一起算的）。评审的 88 是 `grep 'Create('` 的数字，混入了 `task.Machine.Create`（`task.CreateParams`）与 `Users.Create`。v3「auth 不进 `RepoConfig`/`CreateParams`」改为只对 `RepoConfig`（被到处复制）成立：`CreateParams` 是一次性入参，不保存、不返回、不打印；`GitAuth` 嵌在父结构里被各种格式化与 `slog`/JSON 输出时仍不出明文（实测）。新增 AC-5 的嵌套脱敏断言与 `TestCreateEnsuresMirrorWithAuth`（零值 auth 对要求鉴权的桩必须失败，证明 auth 真经 `Create` 到达） | §3.3.2 签名表（新增 `Create` 行、`ExecuteParams` 行改写）与其后的「为什么进 `CreateParams` 不进 `RepoConfig`」；§3.4 调用点全集表；§6 第一行；AC-5 | `/opt/lathe-wt/evidence/f07/prd-v4-createparams-keyed-literals.{go,log}`：`go/ast` 扫 `internal/runner`、`cmd/lathe` → `keyed=46 unkeyed=0; …calls=49`；`/opt/lathe-wt/evidence/f07/prd-v4-createparams-auth-redaction-probe.{go,log}`（go1.25.7）：`%v/%+v/%#v/%s/&%+v/json/slog-text/slog-json` 全部 `leak=false`，`RESULT: NO-LEAK`；`grep -rnE 'CreateParams' internal/runner cmd/lathe \| grep -E 'Sprintf\|slog\|%v'` → 0 |
| R3-2 | `resolveVCS` 在 `setup` 里；`setup` 失败时 `Execute` 直接 `return err`（`pipeline.go:298-301`），注释写明留在 `queued`，`queued` 没有 `→failed` 边；AC-19 断言的 `failed` 不可达，硬做要改状态机 | **采纳，选 (b)**。`setup` 对 GitLab/Gitea 的两类**确定性**错误（新哨兵 `ErrInstanceMismatch`、`ErrCredentialMissing`）只记进 `rc.vcsErr` 不返回，并跳过续跑 mirror 刷新与 review 收集块；每个入口阶段在**第一次合法转移之后、任何网络操作之前**调 `vcsGate` → `p.fail(rc, StageVCSCredential, …)`。五个入口的转移与触发者逐一列表（`queued→triaging→failed`、`queued→implementing→failed` ×2、`→verifying→failed` ×2；第一次转移由 `rc.actor` 触发，`failed` 由 `fail()` 以 `system` 触发），全部是 `state.go` 已有的边，`state.go` 零改动（AC-19 加 `git diff` 断言）。不选 (a)：确定性配置错误留在 `queued` 会每 2 分钟被重领一次、原因只进日志，正是 AGENTS §9「卡排队」的形状，而实例不一致是一次安全拒绝，人必须看得见。暂时性错误与 GitHub 的任何错误保持 cff5332 行为（留 `queued`），GitHub 不变 | §3.3.1 末段；§3.4.1（新）；§6 新增「凭据错误分两类」；AC-19 的 mismatch 用例改写为五入口表驱动 + 两条留 `queued` 的回归 | `pipeline.go:298-301`、`:400-401`、`:493-509`、`:525`、`:650`、`:868`、`:936`、`:1115`、`:1499`；`task/state.go:90`（`queued` 的出边，无 `failed`）、`:91`（`triaging→failed`）、`:106-107`（`implementing/verifying→failed`）；`cmd/lathe/queue.go:27`（租约 2 分钟）、`:494`（失败只记日志）；阶段码放消费方旁边的先例 `skills.go:18`、`profile.go:77`；不进 `stageOrder` 与 `f11-team-skills.md:639-640` 同口径 |
| R3-3(1) | mergepoll 里需要鉴权的不止 `pollTask`：`onMerged→resolveMirrorTip`、`rebaseFollowupOne` 的 `RebaseOnto/ForcePush`、`:474` 的 `Execute`；后继属主要另行解析；「同一个 `GitAuth`」没说从哪来 | **采纳**。逐个定下：`pollTask` 解出的 auth 经 `handleMerged→onMerged→resolveMirrorTip` 透传（刷的是已合并任务的仓库、用其属主凭据）；`rebaseFollowupOne` 自己 `ForUser(succ.UserID)`+`resolveVCS`（不假设与前驱同属主），同一个 auth 给 `RebaseOnto` 与 `ForcePush`，并作为 `failRebaseFollowup` 的新参数用于脱敏；`Execute` 不传 auth，由 `setup` 按 `succ.UserID` 自行解析，§3.4.1 的门同样生效。GitHub 后继不调 `resolveVCS`（零值 auth），不新增 `Clients.GitHub` 调用。凭据错误：`pollTask` 跳过本轮不改状态；rebase 跟进中走 `failRebaseFollowup(…, StageVCSCredential, …)`。测试直接调用的只有 `rebaseFollowup`，签名不变。新增三条 AC：跨属主端到端（A 的前驱合并、B 的后继 rebase + force-push 各用各的 token）、后继实例不一致落 failed、GitHub 跟进路径不新增调用 | §3.4 调用点全集表；§3.4.2（新）；§3.9-2 mergepoll 条改写；§6 新增「rebase 跟进按后继属主」；AC-20 新增三条 | `mergepoll.go:158-193`、`:271`、`:306`、`:341-343`、`:391-435`、`:474-479`、`:585-629`；`TasksWithBaseRef` 不按属主过滤（`task/machine.go:791-795`）；测试直接调用：`grep -rnE '\.(handleMerged\|onMerged\|resolveMirrorTip\|rebaseFollowupOne\|failRebaseFollowup)\(' --include=*_test.go internal cmd` → 0，`\.rebaseFollowup\(` → 4 |
| R3-3(2) | `prd.Service` 没有凭据 / `ClientFactory` 依赖，AC-21 却断言 auth 非空，同时要求 `RepoForPRD` 不注入凭据 | **采纳**。新增 `prd.GitAuthSource` 接口与 `Service.GitAuth` 字段（`prd` 只依赖零依赖的 `vcs` 包，不依赖 `runner/creds`）；`runSpec` 加 `userID`（`runRound`/`review` 两个构造点都有现成的 PRD 属主 id）；`cmd/lathe/prdwiring.go` 新增 `prdGitAuth{f: factory}`，内部 `ForUser`→`VCSClients.VCS`（同一处实例比对），`main.go:425-434` 字面量加一行。nil 时 GitHub 照旧、GitLab/Gitea 报错不做无鉴权克隆。「`RepoForPRD` 不注入凭据」保留并说清含义：仓库信息只来自 `repos` 行，鉴权是另一条依赖。AC-21 改为注入假 `GitAuthSource` 断言 `(userID, target)` 与 auth，另加装配检查 `TestPRDGitAuthWiring`。与 f03/f09 在同一字面量上的接缝按「先合入者不依赖后合入者」写清 | §3.4.3（新）；§6 PRD 行；AC-21 改写；§8 新增 3b | `prd/service.go:78-120`、`:440`、`:480-490`、`:513-516`、`:852`；`main.go:172`（`factory` 与 PRD 字面量同在 `serve()`）、`:425-434`；`f03-usage-stats.md:450`（`Usage: st`）；`f09-notify-channels.md:1303`（`buildPRDService`） |

**需要邻居同步措辞的地方**（f07 只改自己的 PRD）：
- **f09**：§5 AC-32 提 `buildPRDService` 时，请注明「若 f07 已合入，字面量里的 `GitAuth: prdGitAuth{f: factory}` 原样带走，
  函数为此多一个 `factory runner.ClientFactory` 参数（或调用方返回后补赋值）」；f07 先合入时不依赖 f09 的任何符号，f09 先合入时由 f07 改 `buildPRDService`。
- **f03**：`main.go:425-434` 加 `Usage: st` 与 f07 的 `GitAuth:` 是同一字面量里的相邻行，无语义交集，无需改措辞；如 f03 打算改 `runSpec`
  的构造（`f03-usage-stats.md:527` 改的是 `runOnce` 签名，不是 `runSpec`），两线都只**加字段**，合并时并集即可。
- **f11**：`stage.go` 的 `label()` 两线各加一个 case（f11 在 `StageSkillMissing` 后，f07 在 `StageCreatePR` 与 `StageRebaseConflict` 之间），都不进 `stageOrder`，口径已一致。
- **f06/f01**：本轮改动不触及 `agent.Result.Usage`、`RunParams` 或驱动，与它们无新增接缝。

### 9.4 第四轮（v5）

> 三条都属实，全部采纳；第 1、2 条按评审给的方向改，并各补先红的 AC。全部取证基于 `/opt/lathe-wt/f07-vcs` @ `cff5332`、本机 git 2.34.1。
> 唯一与评审建议有出入的细节：第 2 条不选 `--negotiation-tip`、第 3 条不选 (a)，理由在行内。

| # | 评审意见 | 结论 | 改动位置 | 证据 |
|---|---|---|---|---|
| R4-1 | transport 的 `GIT_DIR` 只有自己的 ref、对象库却是 mirror 的；`fetch` 结束时的 `gc --auto` 在 transport 语境下看不到任务分支、worktree HEAD/index、reflog，会删掉老过 `gc.pruneExpire` 的在途未推送提交；AC-5b/5c 测不到 | **采纳**。①`GIT_CONFIG_COUNT` 加 `gc.auto=0`、`maintenance.auto=false`；②`transport()` 对 `fetch` 自动加 `--no-auto-maintenance`（与 ① 冗余互保）；③关掉之后 mirror 的维护不能就此消失，改为 `importRefs` 之后在 **mirror 语境**跑 `gc --auto`（本地、无 token，可达性根与原路径一致）。新增 AC-5d `TestTransportFetchKeepsMirrorOnlyObjects`：mirror config 故意写坏 `gc.*` 阈值与 `pruneExpire=now`、对象 mtime 设 30 天前，3 次刷新 + RebaseOnto/Reattach 后任务提交与 worktree HEAD 仍在，并断言 transport 里从未出现 gc/maintenance；先红在 v4 形态上 | §3.3.2 配置键表与其后两段（「transport 里不得跑 gc」「mirror 自己的维护」）、操作形态表、代码形态段；§6 transport 行；AC-5d；§8 第 3 步 | `/opt/lathe-wt/evidence/f07/prd-v5-transport-gc-negotiation-probe.{sh,log}`：`MODE=v4` 复现评审结论（`cat-file -t` → `could not get object info`、`git -C wt log` → `fatal: bad object HEAD`、fsck 5 行异常）；`MODE=gconly`/`v5` 同样加速条件下全部完好、fsck 0。附带发现：默认 `gc.autoDetach=true` 时 gc 在后台与「删 transport 目录」赛跑，单次运行可能侥幸不丢——不能据此认为安全，探针与 AC 都固定为前台。`git fetch -h`（2.34.1）列出 `--auto-maintenance`/`--auto-gc`，`--no-auto-maintenance` 与 `--no-auto-gc` 实测均 rc=0 |
| R4-2 | transport 没有 ref，协商发不出 have，每次刷新全量重传；§6「毫秒级」与实际不符 | **采纳**。fetch 前把 mirror 的 `refs/remotes/origin/*` 用本地 `update-ref --stdin` 种进 transport 的 **`refs/lathe-have/*`**（不挂 token fd、不联网）。命名空间与评审建议的「写进 transport 的 `refs/remotes/origin/*`」有一处不同：种在同名空间会让上游已删的分支在 transport 里留着旧值，`importRefs` 的 prune 随之失效，所以分开放（新增 `TestTransportSeedHavesDoesNotBreakPrune` 钉住）。不种任务分支 `refs/heads/*`（不必把未推送 sha 当 have 发给远端）。不选 `--negotiation-tip`：它只从已有 ref 中选起点，transport 没有 ref 时仍然发不出 have，等于还是要先种。push 不受影响（按服务端广告算差集）。§6 那一行重写，承认 v4 的估计错了。新增 AC-5d `TestTransportFetchIsIncremental`（每次增量 ≤10 个对象条目、upload-pack 响应 < 首次 10%），先红在 v4 形态上 | §3.3.2「刷新必须是增量的」段、操作形态表；§6 transport 行；AC-5d | 同一探针：`MODE=gconly`（不种 have）对象库 2012K→3984K→5956K→7928K、每次 +206/+209/+212 个对象条目、pack 数 2→3→4（与评审的 3.5M→5.4M→7.4M 同形）；`MODE=v5` 每次 +3 条目（恰为上游新增的 commit/tree/blob）、+24–28K、pack 恒为 1，且 `origin/main` 每次与远端一致 |
| R4-3 | §3.1 说 5 处测试只换类型，但 §3.5.4 让 `AcceptReview` 按 `ev.Provider` 过滤；测试构造的事件 `Provider` 为空 → `ReviewIgnored`，对 `ReviewQueued/ReviewParked` 的断言全挂 | **采纳，选 (b)**，并在 `AcceptReview` 入口对空/未知 `Provider` **返回错误**（不是 `ReviewIgnored`）。不选 (a)「空当 github」：跨平台过滤上设静默默认值，漏填 `Provider` 的 GitLab/Gitea handler 会被当作 GitHub 投递去路由，恰是这道过滤要挡的串扰；而 (b) 不加入口校验的话，漏填会表现成「没有对应的任务」被悄悄吞掉。现在三个 handler 必须填（GitHub 经 `ev.VCS()`），漏填在测试里立刻以错误暴露。测试侧：`inlineEvent`、`mergepoll_test.go:198`、`queue_review_test.go:66` 三处各补一行 `Provider: vcs.ProviderGitHub`，`github_webhook_test.go:36/46` 只换类型；计入 §3.3.2 合计（32 → 37 处），AC-24 加「新增 `Provider` 行恰为 3」的 diff 核对 | §3.1（新「`Provider` 是必填字段」段与影响面表）；§3.3.2 签名表后「另计」；§6 第一行；AC-17 新增 `TestAcceptReviewRequiresProvider`、`TestVCSWebhookHandlersSetProvider`；AC-24；§8 第 1 步 | `internal/runner/review.go:90-91`（现 `TasksByPR(ctx, ownerID, ev.Repo, ev.PRNumber)`）；`review_test.go` 17 处 `AcceptReview` 调用全部经 `inlineEvent` 或其返回值（`:74`、`:130`）；`mergepoll_test.go:198` 期望 `ReviewParked`、`queue_review_test.go:66` 期望 `ReviewQueued`；`github_webhook.go:128-135` 的错误路径（落投递记录、`status:error`）；邻居 PRD 中 `AcceptReview\|ReviewEvent\|TasksByPR` 0 命中，必填 `Provider` 不产生跨线接缝 |

**需要邻居同步措辞的地方**：本轮三条都只改 f07 自己的 transport 与 review 入口，无新增跨线接缝（见 R4-3 证据列的 grep）。

### 9.5 第五轮（v6）

> 两条都属实，全部采纳，修法与评审给的方向一致。取证：本机 git 2.34.1；Gitea 源码取自上游 `go-gitea/gitea` 的 main、v1.22.0、v1.20.0。
> 第 1 条在落实时多发现一处：只把 `gc.autoPackLimit=1` 注入 transport 还不够——种了 have 之后每次增量只有 3 个对象，低于 `fetch.unpackLimit`，
> 落成松散对象、不增 pack 数，「只删 gc 开关」的回归形态仍是绿的（已实测），所以注入口另加 `fetch.unpackLimit=1`。
> 第 2 条也多发现一处：v5 的 Gitea 自身判定用 `pull_request.user.login`，而 1.20/1.22 的 `IssueCommentPayload` 没有 `pull_request` 字段，改用 `issue.user.login`。

| # | 评审意见 | 结论 | 改动位置 | 证据 |
|---|---|---|---|---|
| R5-1 | AC-5d 把加速器写进 mirror config，但 transport 不读 mirror config，v4 形态下 transport 用默认阈值，① 必然绿、红日志拿不到；③「gc/maintenance 子命令的 `GIT_DIR` 不指向 transport」用 PATH 前置包装观测不到（`setup_path()` 把 exec-path 放到 PATH 最前），恒真 | **采纳**。①新增仅供测试的包内变量 `transportExtraConfig`（排在 Lathe 固定键之前，同名键固定键胜出，注入口无法削弱加固），AC-5d 经它把 `fetch.unpackLimit=1`、`gc.autoPackLimit=1`、`gc.autoDetach=false` 只注入 transport；先红改为「保留注入口与种 have、只删 gc 开关」和「v4 原样」两种形态都必须红；v5 形态下注入口在、pack 数到 4，断言 transport 里仍没跑过 gc（= `gc.auto=0` 生效）。②mirror config 的 `gc.*` 投毒保留，明确只用来验证 `importRefs` 之后 mirror 语境那次 gc 的安全。③删掉包装器断言「没有 gc 子进程」，改为新增 `transportBeforeRemove` 回调在删目录前检查 transport 里没有 `packed-refs`/`gc.pid`（gc 必做 `pack-refs`，Lathe 自己只写松散 ref）；包装器只保留对 Lathe 直接调用的 argv/env 断言，并写明它看不到 git 内部派生的子进程 | §3.3.2 新「测试注入口」段、代码形态段；AC-5d 重写；§8 第 3 步 | `/opt/lathe-wt/evidence/f07/prd-v6-ac5d-accelerator-probe.{sh,log}`：`MODE=v4 ACCEL=mirror`（v5 AC 写法）→ 任务提交 `commit`、fsck 0、transport 无 `packed-refs`——**绿**，评审结论成立；`MODE=v4 ACCEL=transport` 与 `MODE=seedonly ACCEL=transport` → `could not get object info`、`bad object HEAD`、fsck 5、三次刷新都出现 `packed-refs`，同时包装器记录的 gc/maintenance 为 **0**（③ 恒真成立）；`MODE=v5 ACCEL=transport`（pack 数 2→4 仍不触发）与 `MODE=v5 ACCEL=mirror` → 绿。`NOUNPACK=1 MODE=seedonly ACCEL=transport`（注入口不含 `fetch.unpackLimit=1`）是**绿**的（每次增量落成松散对象、pack 恒为 1、transport 无 `packed-refs`），故加入该键；注入口另加键白名单。评审引用的 v5 探针第 9-10、30-31 行确为 transport 内加速 |
| R5-2 | Gitea 普通 PR 评论的事件头是 `X-Gitea-Event: issue_comment`（另带 `X-Gitea-Event-Type: pull_request_comment`、`is_pull: true`）；`X-Gitea-Event: pull_request_comment` 是 review 事件，载荷是 `PullRequestPayload`+`review`、没有 `comment.id/user`。照 v5 表实现会丢掉全部普通 PR 评论，AC 的桩照抄也能绿 | **采纳**。§3.5.3 表改为 `issue_comment` + `is_pull==true`，新增「Gitea 事件识别」段（四种实际形态对照表 + 四条识别规则，`X-Gitea-Event-Type` 存在时做一致性校验但不依赖它）；只处理 `action:created`；自身判定改用 `issue.user.login`。review 事件明确为**非触发源**（写进 §4 范围外，`ignored` 并在投递记录说明），review 总评与行内评论**只由 `ReviewFeedback` 二轮拉取覆盖**——为此 `TestGiteaReviewFeedback` 扩充为 issue 评论 + `pulls/<n>/reviews` + 每个 review 的 `comments`（`issues/<n>/comments` 本身不含行内评论，不扩的话「靠二轮拉取覆盖」是空话）。新增 `TestGiteaWebhookEventRecognition`（6 例，先红在 v5 的错误头上）；AC-15/16/17 的 Gitea 桩统一用真实形状；§7 README 改为勾「Pull Request Comment」并说明 review 不触发、二轮会带上 | §3.5.3 表与「Gitea 事件识别」段、载荷字段段；§4 新行；AC-9、AC-14（新测试）、AC-15、AC-16、AC-17；§7 第 3 条；§8 第 5 步 | `/opt/lathe-wt/evidence/f07/prd-v6-gitea-webhook-event-source.log`：main 的 `type.go` `case HookEventIssueComment, HookEventPullRequestComment: return "issue_comment"`、`case HookEventPullRequestReviewComment: return "pull_request_comment"`（v1.22.0、v1.20.0 同）；`deliver.go` 同时设 `X-Gitea-Event`/`X-Gitea-Event-Type`/`X-Gitea-Signature`/`X-Gitea-Delivery`；`notifier.go` 普通评论 `if c.Issue.IsPull { eventType = HookEventPullRequestComment }` + `IssueCommentPayload{IsPull}`，review 走 `PullRequestPayload{Review{Type,Content}}`；`hook.go` v1.20/v1.22 的 `IssueCommentPayload` 无 `pull_request` 字段；`issue_comment.go` `ListIssueComments` 限定 `CommentTypeComment`；`api.go` 有 `pulls/{index}/reviews` 与 `reviews/{id}/comments` 路由 |

---

## 10. 实现与验收结果

### 10.1 提交

| 提交 | 内容 |
|---|---|
| `feat(migrations)` | 0050–0052（up/down 成对；0050 down 在有 GitLab/Gitea 仓库时拒绝） |
| `feat(vcs)` 契约包 | `internal/vcs`（零依赖）；github 包五个纯数据类型改为别名，`ReviewEvent` 保留本地并新增 `VCS()`；`AcceptReview` 改吃 `*vcs.ReviewEvent` 且 `Provider` 必填；`TasksByVCSPR` |
| `feat(httpapi)` 凭据与仓库 | `baseUrl` 校验与入库、`Verifier(ctx, VerifyRequest)`、`POST /api/repos` 的 `vcsProvider`、`PUT` 拒改平台；`KnownSecrets` 补 GitHub webhook 密钥 |
| `feat(runner)` transport | 私有 transport GIT_DIR、种 have、本地导入 + prune、mirror 语境 gc、显式 lease；签名变化与 `CreateParams.Auth` |
| `feat(vcs)` 两个平台客户端 | `internal/vcs/gitlab`、`internal/vcs/gitea`（只用标准库，共用 `internal/vcs/internal/apiclient`） |
| `feat(runner)` 分派 | `VCSClients`、`resolveVCS`、`setup` 错误分类与 `vcsGate`、`StageVCSCredential`、每任务脱敏表；`creds.Clients.VCS` 与新 Verifier |
| `feat(prd)` 加载器与规划路径 | 三处仓库配置读取带平台两列；`GitAuthSource`/`Service.GitAuth`/`runSpec.userID`；`prdGitAuth` 装配 |
| `feat(httpapi)` webhook | `/webhooks/gitlab/{slug}`、`/webhooks/gitea/{slug}` 与解析器 |
| `feat(runner)` 合并检测 | `pollTask` 经 `resolveVCS`、auth 透传到 `resolveMirrorTip`、后继按自己属主解析、`failRebaseFollowup(stage, auth)` |
| `feat(web)` | 仓库页平台下拉、设置页四类新凭据 + 实例地址 + 两个 webhook 地址 |

### 10.2 关键代码位置

- 契约：`internal/vcs/vcs.go`（Provider、类型、`SplitRef`、`BuildPRBody`、哨兵）、`internal/vcs/remote.go`（`RepoKey/CloneURL/Target`、`NormalizeBaseURL`、`GitAuth`）
- transport：`internal/runner/worktree.go` 文件末尾「带鉴权的 git（transport）」一节（`openTransport/command/run/fetchAuthed/seedHaves/importRefs/pushAuthed`，测试注入口 `transportExtraConfig`/`transportBeforeRemove`）
- 分派与门：`internal/runner/vcs.go`；`setup` 与五个入口阶段的 `vcsGate` 在 `internal/runner/pipeline.go`
- 合并检测：`internal/runner/mergepoll.go`（`pollTask`、`followupAuth`、`failRebaseFollowup`）
- 凭据：`internal/creds/creds.go`（缓存带实例地址）、`internal/creds/verifier.go`（`Clients.VCS`、GitLab/Gitea 验证）
- webhook：`internal/httpapi/vcs_webhook.go`；解析与信任判定在 `internal/vcs/gitlab/webhook.go`、`internal/vcs/gitea/webhook.go`
- 装配：`cmd/lathe/main.go`（两条路由、`vcsWebhookResolver`、`prd.Service.GitAuth`）、`cmd/lathe/prdwiring.go`（`prdGitAuth`）

### 10.3 验收结果

证据目录 `/opt/lathe-wt/evidence/f07/`：每条测试 `red-<测试名>.log`（改动前或回归形态下的失败）与
`green-<测试名>.log`（实现后同一命令）；门禁原样输出在 `gate-*.log`，界面截图在 `ui/`。

| AC | 测试 / 核对 | 结果 | 红的形态 |
|---|---|---|---|
| AC-1 | `go test ./internal/integration/github/...`；三个既有测试文件 `git diff --stat cff5332` 为空 | 通过（`gate-diff-checks.log`） | — |
| AC-2 | `TestGitHubTypesAreVCSAliases` | 通过 | 编译失败（无 vcs 包） |
| AC-2b | `TestReviewEventVCSConversionKeepsGitHubTrust`、`TestGitHubWebhookNonActionableNeverReachesIntake`；`webhook.go` diff 只含 import、`VCS()`、`ReviewFeedback` 别名，四个信任符号行零改动 | 通过 | 编译失败 |
| AC-3 | `TestSplitRefByProvider` | 通过 | 编译失败 |
| AC-4 | `TestBuildPRBodyPlatformNeutral`（逐字比对） | 通过 | 编译失败 |
| AC-5 | `TestGitAuthRedactedString`、`TestGitAuthOnlyInOneShotParams`、`TestCreateEnsuresMirrorWithAuth`、`TestTransportEnvIsWhitelistAndTokenFree`、`TestGitHubRemoteUnchanged`、`TestLoadRepoConfigGitHubCloneURLUnchanged` | 通过 | 编译失败（签名/字段不存在） |
| AC-5b | `TestAuthedGitIgnoresAgentConfig`（9 种毒 × 无前缀/`/gitlab` 前缀）+ `TestAuthedGitKeepsTLSVerification` | 通过 | **v2 形态**（在 mirror 上跑 + `GIT_CONFIG_COUNT` 覆盖）下 18 个子测试全红 |
| AC-5c | `TestZeroAuthKeepsLegacyGitArgv` | 通过 | 编译失败 |
| AC-5d | `TestTransportFetchKeepsMirrorOnlyObjects`、`TestTransportFetchIsIncremental`、`TestTransportSeedHavesDoesNotBreakPrune` | 通过 | **seedonly**（删 gc 开关）与 **v4**（再删种 have）形态下任务提交丢失（`could not get object info`）；**gconly** 形态下每次刷新 +228/+231/+234 个对象、upload-pack 与首次同量级 |
| AC-6/7/8 | `TestGitLabCreateMR*`、`TestGitLabGetPRInfoMapsState`、`TestGitLabReviewFeedback` | 通过 | 编译失败 |
| AC-9 | `TestGiteaCreatePR*`、`TestGiteaGetPRInfoMapsState`、`TestGiteaReviewFeedback` | 通过 | 编译失败 |
| AC-10 | `TestVerifyGitLabIdentity`、`TestVerifyGiteaIdentity`、`TestVerifyGitLabUnauthorizedSurfacesMessage` | 通过 | 编译失败 |
| AC-11 | `TestSaveIntegrationStoresBaseURL`、`TestBaseURLValidation` | 通过 | 编译失败 |
| AC-12 | `TestIntegrationStatusNeverReturnsPlaintext`、`TestKnownSecretsIncludesWebhookSecrets` | 通过 | 后者断言失败（`KnownSecrets() = []`） |
| AC-12a | `TestFailureExcerptRedactsPerTaskVCSToken`（两用户并发）、`TestRebaseFailureReasonRedactsVCSToken` | 通过；`make test-race` 无竞态 | 前者在接入分派、未接每任务脱敏时摘要含明文；后者去掉 reason 脱敏时 `failure_reason`/`failure_digests.reason` 含明文 |
| AC-12b | `TestVCSWebhookSecretNeverLoggedOrEchoed` | 通过 | 编译失败 |
| AC-13 | `TestCreateRepoWithVCSProvider`、`TestRepoPathUniqueAcrossProviders`、`TestUpdateRepoRejectsPlatformChange`、`TestRepoPlatformCheckConstraint` | 通过 | 编译失败 / 迁移前 `column "vcs_provider" does not exist` |
| AC-14 | `TestGitLabWebhookTokenMismatch401`、`TestGitLabWebhookMissingToken401`、`TestGiteaWebhookSignature`、`TestVCSWebhookUnknownSlug404`、`TestVCSWebhookRejectsNonPost`、`TestGiteaWebhookEventRecognition` | 通过 | 后者按 **v5 表**（`X-Gitea-Event: pull_request_comment` 当普通评论）实现时 6 例全红 |
| AC-15 | `TestVCSWebhookDeliveryDedupe` | 通过 | 编译失败 |
| AC-16 | `TestGitLabReviewerWithoutMembershipRejected`、`…MembershipAPIErrorFailsClosed`（500/断连/未接线）、`TestGitLabReporterAccepted`、`TestGiteaReviewerNotCollaboratorRejected`、`TestGiteaReviewerWritePermissionAccepted`、`TestSelfCommentIgnoredOnAllProviders` | 通过 | 编译失败 |
| AC-17 | `TestVCSWebhookCannotReachOtherOwnersTasks`、`TestAcceptReviewRequiresProvider`、`TestVCSWebhookHandlersSetProvider`、`TestTasksByVCSPRFiltersProvider`；`TestMachineTasksByPRIsolatesOwners` 未改仍绿 | 通过 | 编译失败 |
| AC-18 | `TestProtectedBranchGateAllProviders`（三平台 × dev/test/main，闸门前 0 次 git 调用） | 通过 | 编译失败 |
| AC-19 | `TestEnqueuePathsCarryRepoPlatform`（Linear / 内置工单 / 编排图三条真实入口）、`TestPipelineOpensPROnRepoPlatform`、`TestPipelineRejectsCredentialInstanceMismatch`（五入口）、`TestPipelineGitHubCredentialErrorStaysQueued`、`TestPipelineVCSTransientErrorStaysQueued`；`state.go` diff 为空 | 通过 | 前者断言失败（平台字段为空、cloneURL 仍是 `git@github.com:`）；其余编译失败 |
| AC-20 | `TestMergePollUsesRepoPlatform`、`TestMergePollGitLabClosedUnmerged`、`TestForcePushDetectedOnGitLab`、`TestMergePollSkipsOnInstanceMismatch`、`TestGitLabMergedFollowupRebasesSuccessorWithItsOwnersAuth`、`TestGitLabRebaseFollowupInstanceMismatchFailsSuccessor`、`TestGitHubRebaseFollowupUnchanged`；既有 `rebase_followup_test.go` 全绿 | 通过 | 实现前断言失败/panic（`red-step8-mergepoll.log`） |
| AC-20b | `TestRepoKeyStableAfterCredentialBaseURLChange` | 通过 | 编译期前置（同 step8 日志） |
| AC-21 | `TestPRDCloneURLUsesRepoProvider`（7 个子用例）、`TestPRDGitAuthWiring` | 通过 | 前者编译失败；后者去掉 `GitAuth:` 装配行时红 |
| AC-22 | `lathe_migrate up → down×3 → up×3` 全部 rc=0；三次 down 后 schema 与 0050 之前 `diff` 为空（忽略 pg_dump 随机的 `\restrict` 令牌行）；`\d repos` 含两列与原样的唯一约束；`TestMigration0050DownRefusesWithNonGitHubRepos`；`TestLoadMigrations`/`TestSplitRealMigrations` 绿；`ON CONFLICT (user_id, provider_repo)` 仍 9 处且未改 | 通过（`gate-migrate-*.log`）；另有真实数据演示：手测登记的 GitLab 仓库在库时 0050 down 被拒（`gate-migrate-0050-down-refuses.log`） | 迁移前列不存在 |
| AC-23 | `go.mod/go.sum/web/package.json/web/pnpm-lock.yaml` diff 为空 | 通过 | — |
| AC-24 | `make lint`、`make test`、`make test-race` 全绿；既有断言删改 0 处；新增 `Provider: vcs.ProviderGitHub` 恰 3 行 | 通过 | — |
| AC-24b | `TestClientsInterfaceUnchanged`（含 f09 形态假件）、`TestResolveVCSDispatch`；`pipeline_test.go`/`pipeline_internal_test.go` 无新增 `Clients` 方法 | 通过 | 编译失败 |
| AC-25 | `make ui && make build` 退出码 0；`ui/settings.png`、`ui/repos.png`（:8407、库 lathe_f07 手测） | 通过 | — |

### 10.4 与 PRD 的出入（如实列出）

1. **AC-19 ①「`Execute` 返回 nil」**：PRD 的理由是「与其它 `p.fail` 路径一致」，而现有 `p.fail` 返回的是
   `任务失败于…` 错误（`pipeline.go` 的 `failWithExcerpt` 末尾），并不返回 nil。实现取「一致」：`vcsGate` 走
   `p.fail`，`Execute` 返回包裹 `vcs.ErrInstanceMismatch` 的错误；测试断言 `errors.Is(err, ErrInstanceMismatch)`。
   状态、阶段码、转移序列、0 网络/0 agent 等其余断言照 PRD。
2. **`failureExcerpt(rc, report)`**：PRD 写改签名，但 `redact_test.go` 有 5 处直接调用 `failureExcerpt(report)`，改签名
   要动既有测试。实现保留它并新增 `failureExcerptFor(rc, report)`（唯一生产调用点改用后者），替换表语义与 PRD 相同。
   同理，`recordFailureMemory` 对原因也用本任务的替换表（PRD 只要求摘要）。
3. **`Verifier` 改签名带来的测试改动**：`credentials_test.go` 的 `fakeVerifier.Verify` 签名随之改变（函数头 + 一行取字段），
   不在 PRD 统计的 37 处之内；没有断言改动。
4. **AC-24「`CloneURL:` 前提核对输出为空」**：PRD §3.3.1 自己规定 `prd/service_test.go:257` 的 `CloneURL` 注入随签名
   「一行跟改」，所以 diff 里有这 1 行 `-`；其余 128 处注入一处未改（新测试文件里的 `+` 行是新增用例）。
5. **credential helper 的作用域**：PRD 写 `credential.<base>.helper`，实现用实例的 origin
   （`credential.<scheme://host:port>.helper`）。本机 git 2.34 上两种写法对带路径前缀的远端都能匹配（已核对
   `git credential fill`）；取 origin 是为了不依赖各版本对路径前缀匹配的细节差异。作用域收窄的是「对哪个主机给口令」，
   远端 URL 仍是 argv 上显式写死的仓库地址，transport 里也没有别的配置来源；AC-5b 的带前缀子用例通过。
6. **Basic 鉴权用户名**：GitLab/Gitea 都用 `oauth2` + token（两者都接受以 token 作口令）。
7. **AC-20b 的观测手段**：`WorktreeManager` 是具体类型、调用方不经接口，「记录键的假 Worktrees」做不出来。改为用真实的
   `NewRepoLookup` 读仓库行、改凭据地址后再读，断言键不变、`Inspect/Discard` 仍落在原 mirror、`.mirrors/` 下只有一个目录。
8. **Gitea 翻页上限**：按 GitHub 实现的口径「每类意见 10 页」（普通评论、review 列表各 10 页，每个 review 的行内评论各一页），
   不是三类合计 10 页。
9. **投递记录**：GitLab/Gitea 的安全忽略（含信任拒绝）原因写进 `webhook_deliveries.error`，带 `ignored: ` 前缀；信任拒绝在响应体
   里只给统一文案（防探测）。GitHub handler 的行为不变。
10. **GitLab Note Hook 的 `action`**：较新版本的载荷带 `object_attributes.action`（create/update）；存在且不是 `create` 时忽略
    （与 GitHub「只处理新建」同口径）。无 `X-Gitlab-Event-UUID` 时投递 ID 用「评论 ID + updated_at」。
11. **GitLab/Gitea 载荷字段**：按官方文档与 PRD §3.5.3 摘录的上游源码建模，桩的头与 JSON 与之一致；**未在真实实例上实测投递**，
    待有实例时补一次投递记录。

### 10.5 合入说明

- 迁移号段 0050–0052 与 dev 当前最大号 0031 之间跳号，是编排方给并行线分配的号段；合入时按 dev 当时最大号 +1 重编号，
  只改文件名不改内容。
- 本线不改 `runner.Clients`、`runner.GitHubAPI` 的方法集与 `runCtx.gh` 字段名，f09 的 `e2eClients` 无需跟改，两线任意顺序合入
  dev 都能编译（AC-24b）。`prd.Service` 字面量多一行 `GitAuth: prdGitAuth{f: factory}`：f09 提 `buildPRDService` 时原样带走。
- `stage.go` 的 `label()` 在 `StageCreatePR` 与 `StageRebaseConflict` 之间加了 `StageVCSCredential`，与 f11 的新 case 不相邻。
