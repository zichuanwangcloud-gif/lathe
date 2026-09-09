# Changelog

本项目的所有重要变更都记录在此文件中。

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [0.1.0] - 2026-09-09

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

[0.1.0]: https://github.com/zichuanwangcloud-gif/lathe/releases/tag/v0.1.0
