# Lathe

> 绑定 Linear 账户后，指派给你的 issue 自动变成一个**已验证**的 PR。

Lathe 的核心不是"调用 agent 写代码"，而是**证明改动有效**：bug 类任务必须先在改动前的代码上复现失败，才有权声称修复。

## 边界

- **不做合并决策** —— 产出 PR，人点合并
- **不做需求澄清** —— 单子不明确就回帖提问并停下，不猜
- **永不 push 受保护分支** —— 一切走 PR

## 文档

| 文档 | 内容 |
|---|---|
| [docs/00-analysis.md](docs/00-analysis.md) | 可行性分析与数据基线 |
| [docs/01-decisions.md](docs/01-decisions.md) | D1–D4 核心决策（验证标准／触发方式／并发／失败处理） |
| [docs/02-design.md](docs/02-design.md) | 系统设计：状态机、数据模型、验证设计、调度 |
| [docs/03-tech-stack.md](docs/03-tech-stack.md) | 技术选型与理由 |

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
#    - 邮件发送（SMTP）：用于「忘记密码」，保存时会真投一封测试邮件
#    每项保存后会立即验证连通性。Linear 验证时自动获取你的账号 ID，
#    接单判定直接使用，无需手工填写。
#
# 6. 「设置」页顶部有你的专属 Linear webhook 地址（/webhooks/linear/<你的随机段>），
#    在 Linear → Settings → API → Webhooks 里指过去，勾选 Issue 事件
# 7. 在「仓库配置」页登记目标仓库（owner/repo）并设置分支策略
```

### 账号

任何人都可以在 `/register` 自助注册（不需要邮箱验证），注册后是普通用户。
管理员在「用户管理」页能看到所有账号与各自的任务计数，可以启用停用、
代重置密码、删除账号及其数据。

忘记密码需要先配好 SMTP。没配的话，管理员可以在用户管理页代为重置。

`LATHE_ADMIN_TOKEN` 仍然可用，但已降级为**脚本与应急通道**：带
`Authorization: Bearer <token>` 的请求会被当作内置管理员，供 curl 直接调接口；
同时它也是「SMTP 挂了且管理员把自己锁在门外」时唯一进得去的路。日常登录不用它。

| 环境变量 | 作用 |
|---|---|
| `LATHE_BASE_URL` | 对外地址，重置邮件里的链接前缀。不配则退回本机地址并告警 |
| `LATHE_ADMIN_EMAIL` | 内置管理员邮箱，默认 `admin@lathe.local` |
| `LATHE_ADMIN_PASSWORD` | 内置管理员初始口令，不配则随机生成并打印一次 |
| `LATHE_ADMIN_TOKEN` | 脚本/应急通道的 Bearer 令牌，可不配 |
| `LATHE_COOKIE_SECURE` | 覆盖会话 Cookie 的 Secure 标志，默认按 BaseURL 的协议推断 |
| `LATHE_TRUSTED_PROXY` | 设为 `true` 才信任 `X-Forwarded-For`（限流按它取客户端 IP） |
| `LATHE_LIGHT_SLOTS` | light 档验证并发上限，默认 2 |
| `LATHE_HEAVY_SLOTS` | heavy 档验证并发上限，默认 1 |
| `LATHE_SETTING_SOURCES` | agent 加载的配置源，默认 `project`（排除个人插件，见 §9） |

口令用 bcrypt（cost 12）哈希；会话与密码重置令牌在库里只存 SHA-256，
明文分别只存在于 Cookie 与邮件里。

凭据以 AES-256-GCM 加密后入库，主密钥保存在数据库之外（`LATHE_SECRET_KEY`
环境变量，或 `$LATHE_DATA_DIR/secret.key`，首次运行自动生成、权限 0600），
因此拿到数据库转储也解不出凭据。

也可继续用环境变量配置凭据（`LATHE_LINEAR_TOKEN` 等），优先级低于界面配置。

流程：指派 issue 给自己 → 事件按你的专属 webhook 地址路由到你名下（用你的
webhook 密钥验签）→ 接单 → 分诊 → 实现（fix/feature 必须交复现/验收
测试）→ 按 diff 定档验证（light 构建检查 / heavy 红-绿-回归）→ 开 PR → 回帖。
失败则回帖说明原因、**保留 worktree 现场**、推送通知，不自动重试。

## 状态

**P0 已完成**（单机闭环 / light 档验证）+ 管理界面 + 账号体系（第一步）。

**P1 验证基建已交付 3/4**：

- ✅ **§5.1 档位路由**：diff 产出后按实际改动面定档（只碰前端展示层 → light；
  碰到后端/migration/计费或跨前后端 → heavy），仓库配置可强制覆盖（设置页）
- ✅ **heavy 档红-绿复现证明**：agent 交的复现/验收测试先在基线 worktree 上跑，
  必须失败（红），改动后必须通过（绿），受影响模块回归必须通过；红立不起来
  ⇒ 转 blocked_spec 回帖请人补充复现步骤，绝不开 PR。每步落 `verifications` 表，
  任务详情页可查
- ✅ **单机双通道并发**：light/heavy 各自独立配额（默认 2/1，
  `LATHE_LIGHT_SLOTS` / `LATHE_HEAVY_SLOTS`），闸门在验证阶段按定档结果准入
- ⬜ per-task compose 隔离栈：红绿阶段目前在 git worktree 里跑（进程组隔离），
  待目标仓库声明服务栈后补

账号体系两步都已交付：注册登录、找回密码、角色与用户管理（第一步），以及
**全链路数据隔离**（P1.5 第二步）——每个用户有专属的 webhook 回调地址
（`/webhooks/linear/{随机段}`，设置页可复制），事件按地址路由到本人：用各自的
webhook 密钥验签、任务记在各自名下、队列按属主解析各自的 Linear/GitHub 凭据执行。
任务、仓库、凭据、统计互不可见（对非属主一律 404，不用 403 暴露存在）。
旧路径 `/webhooks/linear` 继续指向内置管理员，兼容既有部署；环境变量凭据只给
内置管理员兜底，不会漏给普通成员。

后续见 [docs/02-design.md](docs/02-design.md) §8：P2 OAuth 绑定与配额 → P3 多节点。

未完成：per-task compose 隔离、按用户隔离数据、多节点。
