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

## P0 跑起来

```bash
# 1. 基础设施与建表
make dev-infra && make build && ./bin/lathe migrate up

# 2. 起服务（管理令牌是你自定义的口令，用于登录管理界面）
LATHE_ADMIN_TOKEN=$(openssl rand -hex 32) ./bin/lathe serve

# 3. 打开 http://localhost:8200 ，在「设置」页配置凭据：
#    - Linear API 令牌：Linear → Settings → Security & access → Personal API keys
#    - GitHub 令牌：GitHub → Settings → Developer settings → Personal access tokens
#    - Linear Webhook 密钥：建 webhook 时 Linear 生成
#    每项保存后会立即验证连通性。Linear 验证时自动获取你的账号 ID，
#    接单判定直接使用，无需手工填写。
#
# 4. 在「仓库配置」页设置目标仓库与分支策略，把 Linear webhook 指到
#    /webhooks/linear
```

凭据以 AES-256-GCM 加密后入库，主密钥保存在数据库之外（`LATHE_SECRET_KEY`
环境变量，或 `$LATHE_DATA_DIR/secret.key`，首次运行自动生成、权限 0600），
因此拿到数据库转储也解不出凭据。

也可继续用环境变量配置凭据（`LATHE_LINEAR_TOKEN` 等），优先级低于界面配置。

流程：指派 issue 给自己 → webhook 接单 → 分诊 → 实现 → light 档验证 → 开 PR → 回帖。
失败则回帖说明原因、**保留 worktree 现场**、推送通知，不自动重试。

## 状态

**P0 已完成**（单机 / 单用户 / 串行 / light 档验证）+ 管理界面，168 项测试。

后续见 [docs/02-design.md](docs/02-design.md) §8：
P1 验证基建（per-task compose 隔离 + 红-绿复现证明）→ P2 多租户 → P3 多节点。

未完成：heavy 档验证（红-绿复现证明）、多用户、多节点。
