# Lathe

> 绑定 Linear 账户后，指派给你的 issue 自动变成一个**已验证**的 PR。

Lathe 的核心不是"调用 agent 写代码"，而是**证明改动有效**：bug 类任务必须先在改动前的代码上复现失败，才有权声称修复。

## 边界

- **不做合并决策** —— 产出 PR，人点合并
- **执行阶段不猜** —— 任务单不明确就回帖提问并停下。需求本身没想清楚的，
  走模糊任务规划（[docs/10](docs/10-prd-template.md)）：智能体陪人把需求写成
  PRD，但**必须人逐节签字**才能生成任务 —— 规划阶段帮人想清楚 ≠ 执行阶段
  替人猜
- **永不 push 受保护分支** —— 一切走 PR

## 文档

| 文档 | 内容 |
|---|---|
| [docs/00-analysis.md](docs/00-analysis.md) | 可行性分析与数据基线 |
| [docs/01-decisions.md](docs/01-decisions.md) | D1–D4 核心决策（验证标准／触发方式／并发／失败处理） |
| [docs/02-design.md](docs/02-design.md) | 系统设计：状态机、数据模型、验证设计、调度 |
| [docs/03-tech-stack.md](docs/03-tech-stack.md) | 技术选型与理由 |
| [docs/09-ci.md](docs/09-ci.md) | CI 门禁与发布流水线 |
| [docs/10-prd-template.md](docs/10-prd-template.md) | 模糊任务 PRD 模板与填写规范 |

## 技术栈

Go 1.25 · pgx + sqlc · PostgreSQL · Vue 3 SPA（`go:embed`）· `claude` CLI (`--output-format stream-json`)

交付物是**单个静态二进制**（含内嵌 UI），新增节点 = 拷一个文件。

## 开发

```bash
make dev-infra    # 起 Postgres（端口 55432，避开常见占用）
make migrate      # 建表
make build        # 编译
make test         # 测试
make run          # 起控制面
```

`make test` 在库连不上时会跳过所有数据库测试，方便在没起基础设施的环境里跑纯逻辑
测试。想按 CI 的口径验一遍（库连不上直接失败）用 `make test-ci`。

推 PR 前本地过一遍 `make lint && make test-ci`。注意 `make lint` 只有
`gofmt` + `go vet`，CI 的 lint job 还会跑 golangci-lint（需另装，本地装不装都能提
PR，但只在本地绿过不代表门禁会绿）。详见 [docs/09-ci.md](docs/09-ci.md)。

## 跑起来

```bash
# 1. 基础设施与建表
make dev-infra && make all && ./bin/lathe migrate up

# 2. 起服务。LATHE_BASE_URL 是本实例的对外地址，密码重置邮件里的链接用它拼
#    （不从请求 Host 头推导 —— 那个头可以伪造，会把重置令牌送去攻击者的域名）
LATHE_BASE_URL=https://lathe.example.com ./bin/lathe serve

# 3. 从启动日志里抄出内置管理员的初始口令（只打印这一次）：
#    WARN 已为内置管理员生成初始口令... email=admin@lathe.local password=xxxxxxxx
#    也可以用 LATHE_ADMIN_PASSWORD 自己指定（此时不打印）
#
# 4. 打开 http://localhost:8200 用它登录 —— 会被要求先改掉初始口令
#
# 5. 在「设置」页配置凭据：
#    - Linear API 令牌：Linear → Settings → Security & access → Personal API keys
#    - GitHub 令牌：GitHub → Settings → Developer settings → Personal access tokens
#    - Linear Webhook 密钥：建 webhook 时 Linear 生成
#    - GitHub Webhook 密钥（可选，PR 评论回流用）：建仓库 webhook 时自己填的 Secret
#    - 邮件发送（SMTP）：用于「忘记密码」，保存时会真投一封测试邮件
#    每项保存后会立即验证连通性。Linear 验证时自动获取你的账号 ID，
#    接单判定直接使用，无需手工填写。
#
# 6. 「设置」页顶部有你的专属 Linear webhook 地址（/webhooks/linear/<你的随机段>），
#    在 Linear → Settings → API → Webhooks 里指过去，勾选 Issue 事件
# 7. 在「仓库配置」页登记目标仓库（owner/repo）并设置分支策略
#
# 7.5（可选）PR 评论回流：「设置」页还有一个 GitHub webhook 地址
#    （/webhooks/github/<同一个随机段>）。在目标仓库 → Settings → Webhooks 新建：
#    Content type 选 application/json，Secret 与上面的「GitHub Webhook 密钥」一致，
#    事件勾 Pull request reviews 与 Pull request review comments。
#    之后他人在 Lathe 开的 PR 上留 review 意见 → 任务 resume 原会话修改 →
#    完整重新验证 → 更新同一个 PR。PR 作者本人的评论与 approve 不触发。
#
# 8.（可选）在「系统设置」页填「标签驱动接单」的标签名（如 lathe:go）：
#    填上之后，issue 被打上这个标签就接单，不必改指派人。留空表示关闭。
```

### 账号

任何人都可以在 `/register` 自助注册（不需要邮箱验证），注册后是普通用户。
管理员在「用户管理」页能看到所有账号与各自的任务计数，可以启用停用、
代重置密码、删除账号及其数据。

忘记密码需要先配好 SMTP。没配的话，管理员可以在用户管理页代为重置。

### 任务预览环境

看板上有工作区的任务可以一键「预览」：扫描 worktree 里的 Dockerfile 与
compose 编排文件，人选要起哪几个。Dockerfile 单镜像可勾选附加基础设施
（postgres/redis/mysql，连接串自动注入）；compose 编排直接 `up -d --build`
（钉死的端口自动重置为随机，`${VAR:?}` 必填变量由人填写注入）。构建
进度实时可见，手动验证完一键停止并清理（容器/网络/自建镜像）。内存/磁盘
占用率达到阈值时禁止启动（阈值在「系统设置」由管理员配置，默认 90%）。
需要本机 docker 与 docker compose 可用。详见 docs/02-design.md §10。

`LATHE_ADMIN_TOKEN` 仍然可用，但已降级为**脚本与应急通道**：带
`Authorization: Bearer <token>` 的请求会被当作内置管理员，供 curl 直接调接口；
同时它也是「SMTP 挂了且管理员把自己锁在门外」时唯一进得去的路。日常登录不用它。

### Docker 部署

```bash
docker pull ghcr.io/zichuanwangcloud-gif/lathe:0.2.0
```

镜像不含 Postgres 与 `claude` CLI：用 `LATHE_DB_*` 指向外部数据库
（容器内先跑 `lathe migrate up` 建表），agent 执行需另行挂载或安装 claude。

| 环境变量 | 作用 |
|---|---|
| `LATHE_BASE_URL` | 对外地址，重置邮件里的链接前缀。不配则退回本机地址并告警 |
| `LATHE_HTTP_ADDR` | 监听地址，默认 `:8200` |
| `LATHE_DB_*` | 数据库连接六元组（HOST/PORT/USER/PASSWORD/NAME/SSLMODE），默认对齐 `make dev-infra`（127.0.0.1:55432，lathe/lathe）；镜像部署用它指向外部 Postgres |
| `LATHE_ADMIN_EMAIL` | 内置管理员邮箱，默认 `admin@lathe.local` |
| `LATHE_ADMIN_PASSWORD` | 内置管理员初始口令，不配则随机生成并打印一次 |
| `LATHE_ADMIN_TOKEN` | 脚本/应急通道的 Bearer 令牌，可不配 |
| `LATHE_LINEAR_TOKEN` | Linear API 令牌的兜底值。**只对内置管理员生效**，且优先级低于设置页（界面里配了就以界面为准，改完即刻生效） |
| `LATHE_LINEAR_WEBHOOK_SECRET` | Linear webhook 签名密钥的兜底值。**只对内置管理员生效**、优先级低于设置页 |
| `LATHE_LINEAR_USER_ID` | 「指派给我了吗」接单判定用的 Linear 用户 ID。**只对内置管理员生效**；设置页配 Linear 令牌时会自动取到账号 ID，正常情况不用手工填 |
| `LATHE_GITHUB_TOKEN` | GitHub 个人访问令牌的兜底值（推分支、开 PR）。**只对内置管理员生效**、优先级低于设置页 |
| `LATHE_COOKIE_SECURE` | 覆盖会话 Cookie 的 Secure 标志，默认按 BaseURL 的协议推断 |
| `LATHE_TRUSTED_PROXY` | 设为 `true` 才信任 `X-Forwarded-For`（限流按它取客户端 IP） |
| `LATHE_WORKTREE_TTL` | 终态任务的工作区现场保留时长，默认 `72h`，**下限 `1h`**（配得更小启动直接报错）。注意 Go 的时长解析**不支持 `d`**，写 `72h` 不能写 `3d` |
| `LATHE_REAP_INTERVAL` | 现场回收的扫描间隔，默认 `1h`，必须为正 |
| `LATHE_REAP_ENABLED` | 现场回收总开关，默认 `true`。设 `false` 时收割机根本不启动 —— 它会删磁盘目录与 git 分支，必须能一键关掉 |
| `LATHE_REAP_DRY_RUN` | 设 `true` 时收割机只打「本轮会删什么」的日志，不碰任何东西。首次启用或调整 TTL 后先干跑一轮 |
| `LATHE_PRD_STALL_TIMEOUT` | 模糊任务（PRD）停在「待回答」多久无人应答算搁置，默认 `168h`（7 天），**下限 `1h`**（配得更小启动直接报错）。同样注意写 `168h` 不能写 `7d`（写成 `7d`、`abc` 等非法时长启动直接报错，不回落默认值）。超期的 PRD 转「已搁置」并发提醒邮件；属主要在 PRD 详情页点「继续这份 PRD」恢复（搁置后直接回答不会唤醒） |
| `LATHE_PRD_STALL_SWEEP_INTERVAL` | 搁置扫描的轮询间隔，默认 `1h`，必须为正；非法时长（如 `1d`）启动直接报错 |
| `LATHE_PRD_STALL_NOTIFY` | 设 `false` 时搁置不发提醒邮件（判定本身照做）。搁置发生在后台，不看列表页就完全无感，邮件是唯一的主动告知，所以默认开 |
| `LATHE_WORKSPACE_ROOT` | worktree 与 `.mirrors/` 的根目录，默认 `/opt/lathe/workspaces`。必须是**至少两层**的专用目录，且不能是 `/`、`/opt`、`/home` 这类系统目录 —— 孤儿清扫会删掉根下无人认领且超期的顶层目录 |
| `LATHE_LIGHT_SLOTS` | light 档验证并发上限，默认 2 |
| `LATHE_HEAVY_SLOTS` | heavy 档验证并发上限，默认 1 |
| `LATHE_SETTING_SOURCES` | agent 加载的配置源，默认 `project`（排除个人插件，见 §9） |
| `LATHE_TRIAGE_CHANNEL` | 分诊的 cc-switch 通道名（模型路由：分诊走便宜通道），空 = 跟随激活通道 |
| `LATHE_IMPLEMENT_CHANNEL` | 实现/修复回路的 cc-switch 通道名（走强通道），空 = 跟随激活通道 |
| `LATHE_GITHUB_WEBHOOK_SECRET` | GitHub 仓库 webhook 签名密钥的兜底值（PR 评论回流验签）。与其它凭据类环境变量一样**只对内置管理员生效**、优先级低于设置页；普通成员须在设置页各自配置 |
| `LATHE_FIX_ATTEMPTS` | 验证失败后的修复轮回数上限（resume 原实现会话就地修复再验证），默认 `2` |
| `LATHE_AGENT_TIMEOUT` | agent 单次执行超时，默认 `45m` |
| `LATHE_AGENT_ENV_EXTRA` | agent 子进程环境白名单的显式开口（逗号分隔变量名）。默认白名单只放行 PATH/HOME 等操作性变量，`LATHE_ADMIN_TOKEN`、数据库口令、GitHub token 不进 agent 进程 |
| `LATHE_CLAUDE_BIN` | claude 二进制名或路径，默认 `claude`（从 PATH 解析） |
| `LATHE_NODE_NAME` | 节点名（事件 actor 前缀、管理界面运行时面板），默认主机名 |
| `LATHE_PNPM_STORE` | 验证器跑前端步骤时共享的 pnpm store 目录，默认 `/opt/lathe/.pnpm-store` |
| `LATHE_MIGRATE_TIMEOUT` | `migrate` 子命令的超时，默认 `10m`。大表上的 `CREATE INDEX CONCURRENTLY` 可能跑很久，慢了就调大 |

口令用 bcrypt（cost 12）哈希；会话与密码重置令牌在库里只存 SHA-256，
明文分别只存在于 Cookie 与邮件里。

凭据以 AES-256-GCM 加密后入库，主密钥保存在数据库之外（`LATHE_SECRET_KEY`
环境变量，或 `$LATHE_DATA_DIR/secret.key`，首次运行自动生成、权限 0600），
因此拿到数据库转储也解不出凭据。

也可继续用环境变量配置凭据（`LATHE_LINEAR_TOKEN` 等，见上表），优先级低于设置页，
且**只对内置管理员生效**（`internal/creds` 的 `Factory.ProviderFor`：环境兜底只发给
部署者自己的账号，不借给普通成员的任务 —— 普通成员在设置页各自配置）。

流程：指派 issue 给自己 → 事件按你的专属 webhook 地址路由到你名下（用你的
webhook 密钥验签）→ 接单 → 分诊 → 实现（fix/feature 必须交复现/验收
测试）→ 按 diff 定档验证（light 构建检查 / heavy 红-绿-回归）→ 开 PR → 回帖。
失败则回帖说明原因、**保留 worktree 现场**、推送通知，不自动重试。

失败会写进**跨任务记忆**（`failure_digests` 表）：同一个需求单（同仓库 +
同平台 + 同编号）再次派发时，把最近一次失败的阶段、原因与输出摘要追加到
分诊与实现的提示词末尾，agent 一上来就知道「上次死在哪」，不必在同一个坑
上再烧一轮 token。注入有硬上限（摘要 2000 字节、原因 600 字节），且措辞
明确标为「历史记录，不是你本次的结果」—— 免得 agent 去修一个已经修好的
东西。续跑场景（resume 原会话）不重复注入：那份失败原因本来就在提示词里。
摘要取自失败步骤输出的**尾部**（结论在后面），落库前先脱敏 —— 验证命令跑在
宿主环境里，输出里回显环境变量与连接串（带口令）是常见形态，摘要在库里、
在提示词里、在详情页都出现，不能带明文凭据。

脱敏分两层，**顺序就是优先级**：

1. **权威层**：平台在运行期**已知**的凭据值按**字节精确替换**，不依赖任何
   「像不像凭据」的猜测 —— 值被截断成残段、host 是域名、挂在自定义变量名下，
   都能精确命中。凭据的主线（子进程回显 env）由它覆盖。登记口径按来源分：
   配置里的 DB DSN/集成 token、进程环境里名字命中词根的变量登记**整串**；
   隔离栈的 `StackEnv` **只登记含 `user:pass@` 的 DSN 值及其口令段** ——
   那张表里还有 `DATABASE_HOST=127.0.0.1`、`DATABASE_PORT`、`DATABASE_USER`
   这类非凭据的运行参数，登记它们既会抹掉正常排障信息（连的是哪个库），
   又会**自伤**：层 1 先把主机抹掉，层 2 随即匹配不上，口令反而留下明文。
   权威层的固有代价是「任意与凭据值同值的正常文本都会被抹」—— 换的是主线上的确定性。
2. **形态表**：盖那些运行期不可知、但看得出是凭据的值（GitHub/Linear/Slack/
   AWS/JWT、`key=value`、连接串口令段）。**尽力而为，非穷举**。

已知边界（**不是通过条件**，列出来免得被当成穷举）：无键值结构的口令、
PEM 私钥块、`file:///…`、裸值 < 8 字节或含 `\`/多字节、`null`/`true`/数字
这类结构字面量刻意不抹、`a:b@c.com` 判为邮箱不抹、无端口无路径的
`user:pw@host` 不抹（`dial lathe:pw@127.0.0.1 timeout`）、无下划线连写的别名
（`MYSQLPWD`/`DBPASS`）不覆盖、尾窗恰好砍在口令内部只剩后缀时接不上。
另外摘要会随提示词作为 **argv** 传给 agent CLI，同机进程可从
`/proc/<pid>/cmdline` 读到。完整清单在 `internal/runner/redact.go` 顶部。

看板上的**平台度量**面板回答「平台改动有没有用」：各环节耗时（排队 /
分诊 / 实现 / 验证 / 端到端）的 p50 与 p95、失败分类直方图。数据源是
`task_events`（append-only 的状态转移流），不是 `tasks.updated_at` ——
后者每次 UPDATE 都被触发器刷新，算不出分段。指标按环节取**首次进入**的
时刻，跳过续跑造成的负差值，与花费面板一样按登录用户隔离。口径是**环境
耗时**（一个单子从接单到开 PR 中间隔了多久），不是「平台实际忙了多久」：
分诊那段里若有人工审批等待、排队那段里若有等前置任务的时间，它们都在里面。

PR 上收到他人的 review 意见时（配了 GitHub webhook 的话）：任务
`pr_open → review_feedback → queued`，worker 以 `--resume` 续原实现会话，把
**全部待处理意见**（行内评论、PR 评论、review 总评，排除 PR 作者本人与已处理过的）
交给 agent 改，再**完整重走一遍验证**后更新同一个 PR。意见按条记账，同一条
不会被处理两次；任务已合并/取消/失败时意见安全忽略，终态不会被拉回来。

也可以完全不走 Linear：「工单」页手动建单（标题 + 需求描述 + 评论区 +
优先级），开跑后走**同一条**状态机与验证链路。agent 的提问（需求不清时）
落在工单评论区，人回复后重试即带着新信息继续 —— 与 Linear 评论回路
逐字节同构（docs/09-internal-issues.md）。

## 状态

**P0 已完成**（单机闭环 / light 档验证）+ 管理界面 + 账号体系（第一步）
+ **内置工单体系 P1**（手动建单 / 评论区 / 开跑与取消联动，docs/09）
+ **模糊任务 PRD 规划**（多轮对话产出 PRD、人逐节签字、一键生成任务与编排图，docs/10）。

**P1 验证基建已交付 4/4**：

- ✅ **§5.1 档位路由**：diff 产出后按实际改动面定档（只碰前端展示层 → light；
  碰到后端/migration/计费或跨前后端 → heavy），仓库配置可强制覆盖（设置页）
- ✅ **heavy 档红-绿复现证明**：agent 交的复现/验收测试先在基线 worktree 上跑，
  必须失败（红），改动后必须通过（绿），受影响模块回归必须通过；红立不起来
  ⇒ 转 blocked_spec 回帖请人补充复现步骤，绝不开 PR。每步落 `verifications` 表，
  任务详情页可查
- ✅ **单机双通道并发**：light/heavy 各自独立配额（默认 2/1，
  `LATHE_LIGHT_SLOTS` / `LATHE_HEAVY_SLOTS`），闸门在验证阶段按定档结果准入
- ✅ **验证依赖隔离栈**（T8）：仓库可声明验证期依赖（`verify_infra`，
  postgres/redis/mysql 目录键），验证阶段为任务起专属依赖容器，与预览环境按
  标签隔离互不误删；红绿测试进程本身仍在 worktree 里跑（进程组隔离），
  完整 compose 进程隔离见 docs/02-design.md §8

账号体系两步都已交付：注册登录、找回密码、角色与用户管理（第一步），以及
**全链路数据隔离**（P1.5 第二步）——每个用户有专属的 webhook 回调地址
（`/webhooks/linear/{随机段}`，设置页可复制），事件按地址路由到本人：用各自的
webhook 密钥验签、任务记在各自名下、队列按属主解析各自的 Linear/GitHub 凭据执行。
任务、仓库、凭据、统计互不可见（对非属主一律 404，不用 403 暴露存在）。
旧路径 `/webhooks/linear` 继续指向内置管理员，兼容既有部署；环境变量凭据只给
内置管理员兜底，不会漏给普通成员。

后续见 [docs/02-design.md](docs/02-design.md) §8：P2 OAuth 绑定与配额 → P3 多节点。

未完成：红绿测试进程的完整 compose 隔离（§8）、多节点。
