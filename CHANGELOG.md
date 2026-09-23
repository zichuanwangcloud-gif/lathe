# Changelog

本项目的所有重要变更都记录在此文件中。

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### Added

- **内置工单体系**（`docs/09-internal-issues.md`）：任务不再只能由 Linear webhook
  产生。`issues` / `issue_comments` 两张表 + HTTP API + 工单列表与详情界面，
  key 形如 `LT-1042`（全局序列）。工单必绑仓库，修掉 `resolveRepo`
  「取该用户第一个仓库」的随机性
- `internal/tracker`：需求平台的窄接口（`Issue` / `Comment` 两方法，返回平台无关
  类型）。Linear 以类型别名适配，其包内部零改动；内置工单是第二个实现
- `AttributedTracker.WithActor`：内置工单把 agent 评论署名为 `task-<id>`，
  评论区渲染「lathe · 任务 #id」并可跳任务详情
- 提问回路复用既有 `blocked_spec` + 评论区 + 重试，零新状态零新语义
- CI 门禁（`.github/workflows/ci.yml`）：静态检查、带真实 Postgres 的测试、
  镜像构建三个 job 并行；测试前跑迁移并校验最新迁移可回滚
- 发布流水线（`.github/workflows/release.yml`）：`v*` tag 触发，校验 tag 与
  CHANGELOG 一致后复用门禁，推多架构镜像到 GHCR、产出三平台二进制、建 Release 草稿
- `internal/testsupport`：数据库测试的统一连库入口，`LATHE_TEST_REQUIRE_DB`
  控制连不上库时是跳过还是失败
- `make test-ci`：按 CI 的严格口径跑测试
- `.golangci.yml`：golangci-lint 配置，`new-from-rev` 只卡新增改动
- [docs/09-ci.md](docs/09-ci.md)：CI 设计说明
- **模糊任务 PRD 规划**（[docs/10-prd-template.md](docs/10-prd-template.md)）：需求不清楚的单子
  先走多轮对话产出结构化 PRD，人逐节签字后再一键生成任务。`prds` / `prd_rounds` /
  `prd_events` 三张表 + `internal/prd` + HTTP API + 「模糊任务」界面
- 一键生成（§4.3）：approved 的 PRD 按任务块建内置工单、按依赖连成编排图入队。
  工单正文带上该任务要交付的 **AC 全文**而不只是编号 —— 实现 agent 看不到 PRD，
  只写「满足 AC-3」等于没写
- 定稿闸门：自检清单不过则拒绝进评审（可追溯闭环、AC 必须可执行、未决问题清零、
  每节都要人确认过），并跑一次对抗复核让另一个 agent 专门找 AC 的洞
- `repos.prd_task_max_lines` / `prd_task_max_files`：PRD 拆出的单任务量级上限
  （出厂 400 行 / 8 文件），消费方是定稿自检 —— 超限即拒绝进评审
- PRD 导出（§7）：`GET /api/prds/{id}/export?format=md|json`，任何状态下都可用。
  Markdown 版保留节状态标记的文本形式（`[已确认]` 等），可回写进目标仓库 `docs/`
  —— 但 Lathe 不自动提交、更不 push，入不入库是人的决定

### Changed

- `tasks.linear_issue_key` / `linear_issue_id` 改名为 `external_key` /
  `external_id`，新增 `tracker_provider`（`linear` / `internal`）。活跃任务
  唯一索引随之改为 `(repo_id, tracker_provider, external_key)`
- Dockerfile 的构建阶段改为 `--platform=$BUILDPLATFORM` + `GOOS/GOARCH` 交叉编译，
  多架构构建不再靠 QEMU 模拟跑 Go 编译

### Fixed

- 数据库测试在库不可达时会静默跳过，导致 CI 可能全绿却什么都没验证 ——
  改为由 `LATHE_TEST_REQUIRE_DB` 强制失败（本地默认行为不变）

## [0.1.1] - 2026-09-19

### Fixed
- Go 模块路径由仓库迁移前的 `github.com/Clouditera/lathe` 更正为 `github.com/zichuanwangcloud-gif/lathe` —— 此前 `go install github.com/zichuanwangcloud-gif/lathe/cmd/lathe@v0.1.0` 会因「声明路径与请求路径不符」报错
- `scripts/claude-cc-switch` 不再写死本机 claude 二进制的绝对路径，改从 `PATH` 解析

## [0.1.0] - 2026-09-19

首个版本。绑定 Linear 账户后，指派给你的 issue 自动变成一个**已验证**的 PR ——
核心不是「调用 agent 写代码」，而是**证明改动有效**：bug 类任务必须先在改动前的
代码上复现失败，才有权声称修复。

交付物是单个静态二进制（含 `go:embed` 内嵌 UI），新增节点 = 拷一个文件。

### Added

#### 任务闭环
- 任务状态机核心，串通 Linear issue → worktree → agent 执行 → 验证 → PR 的端到端闭环
- Linear 集成：webhook 幂等、拉取 issue、回帖
- GitHub 集成：推分支与开 PR
- `claude` CLI driver（`--output-format stream-json`）
- 失败三件套：失败分类、回帖与停机语义

#### 验证体系
- 分档验证路由（§5.1）：light/heavy 档位，heavy 档要求红-绿复现证明
- 修复回路与重试语义（§5）、智能重试断点续跑（§6.5）、启动恢复
- 复现测试显式声明（§5.3）
- 仓库级验证排除目录贯通（`repos.exclude_dirs`）
- 回归测试按改动包收敛，不再全量跑模块
- 分诊目录中立化与按阶段模型路由

#### 并发与调度
- 单机双通道并发，验证阶段按档位准入（§6.2）
- worktree 生命周期管理与 light 档验证

#### 执行可见性
- 事件落库、交付摘要与详情页日志面板
- 从 transcript 读取 subagent 内部活动，经 EventSink 轮询并入同一条事件流
- `agent_events` 落库与读取带上 `agent_id`；详情页把 subagent 收成子块

#### 任务预览环境
- 看板一键起服务，构建进度实时可见，可取消进行中的构建
- compose 编排 + 附加基础设施 + env 注入
- AI 推荐「起哪个候选、变量填什么」
- 数据库策略四层重构：改动识别机械化、复用/克隆、口令免填
- 仓库基线目录：复用基线中间件，起服务无需重建基础设施

#### 并行开发编排
- 并行开发编排：链路图、栈式 PR、合并轮询、profile/skills
- 编排流程图前端：建图画布、查看轮询、菜单入口

#### 账号与多租户
- 账号体系：注册登录、找回密码、用户管理
- 全链路 `owner_id` 数据隔离与每用户专属 webhook（P1.5）
- 凭据可在界面配置，保存时立即验证连通性
- 管理界面：任务看板、详情、仓库配置、设置
- 看板与接单入口分离，设置体系拆分为个人/系统两级
- 看板同步 Linear issue：列表、详情与手动开始执行

#### 成本控制
- `--setting-sources` 收敛 agent 上下文基线成本（§9）

#### 构建
- `make build VERSION=x.y.z` 注入版本号，`lathe version` 自报发布版本（不传仍为 `dev`）
- `make lint` 的 gofmt 只扫入库文件，不再报本地 worktree 检出副本的无关噪声

### Fixed
- preview 状态接口路径前后端不一致，导致弹窗永远看不到容器
- 容器/镜像查询的 label 过滤器漏写 `label=` 前缀
- compose 文件名识别先验 yaml 后缀再验前缀
- preview session ID 改用 UUID，dockerfile 推荐的 env 进额外变量框
- AI 推荐超时放宽到 10 分钟，Driver 超时报实际生效上限
- agent 事件截断回退 rune 边界，避免切坏多字节字符
- agent 子进程 env 改白名单，不再整体透传
- git mirror 远端分支迁入 `refs/remotes/origin/*`，杜绝并发任务被 prune 剪枝
- worktree Create 回收同名尸体，同 issue 重试/重触发不再撞名
- 复现测试包路径改模块相对，pnpm 步骤尊重仓库排除
- 发信失败提示补齐 EOF 与 553 两个分类
- 干净克隆缺少 `internal/webui/dist/` 占位文件，`//go:embed all:dist` 无匹配，导致 `go build`／`vet`／`test` 在新克隆的仓库里全部失败

### 边界（本版本有意不做）
- **不做合并决策** —— 产出 PR，人点合并
- **不做需求澄清** —— 单子不明确就回帖提问并停下，不猜
- **永不 push 受保护分支** —— 一切走 PR

[0.1.1]: https://github.com/zichuanwangcloud-gif/lathe/releases/tag/v0.1.1
[0.1.0]: https://github.com/zichuanwangcloud-gif/lathe/releases/tag/v0.1.0
