# Lathe — 可行性分析与架构决策

> 起草日期：2026-08-12
> 一句话：把 Linear 单当毛坯，自动装夹到 worktree、加工成 PR，并**自动证明它修好了**。

---

## 1. 现状实证（2026-08-12 采集自 /opt）

| 指标 | 值 |
|---|---|
| CloudRouter git worktree 注册总数 | **551**（全部实体存在，0 个可 prune 的僵尸注册） |
| ├ `.claude/worktrees` | 341 |
| ├ `/opt` 下同级目录 | 112 |
| └ `.worktrees` / `.emdash` / 其他 | 98 |
| 其中已全部合入 `origin/dev`（可回收） | **503** |
| 尚有未合入内容（不可动） | 48 |
| 对应 Linear 单号（`cr-XXXX`）的 `/opt` 目录 | 57 |
| 单日峰值新开工作区 | 12（2026-08-06） |
| 近 30 天提交 | 1080（4 位提交者：zichuan / liuxin / xu / merge-test） |
| 单 worktree 体积 | 131M ~ 1.3G；**40 个自带独立 node_modules（各 ~1.3G）** |
| 磁盘 | 486G / 497G = **98%**，剩余 11G |
| 分支命名约定 | `fix/cr-1326-portable-import`、`feature/cr-1130-invoice-feature`、`<user>/cr-1152-<标题>` |
| PR 流程 | GitHub PR 合入 dev（近期 #2725–#2736），`gh` 已认证 |
| Linear MCP | **已挂**：配置为 `https://mcp.linear.app/sse`，返回 404 |

**结论：流水线已客观存在，规范已手工固化。缺的是执行器 + 调度器 + 验证基建。**

---

## 2. 核心判断

### 2.1 瓶颈不在"开窗口"

真实成本集中在两头：

- **装夹**：读单 → 判断说清楚没 → 定位代码 → 起 worktree → 配环境
- **验收**：跑起来 → 复现原问题 → 确认修好 → 确认没搞坏别的 → 开 PR → 处理 review

只自动化中间的"写代码"，结果是**产出一堆待人工验收的 PR**：瓶颈从"开窗口的人"平移成"PR 审核队列"，总吞吐不变甚至更差。

### 2.2 因此本产品的核心竞争力是验证基建

判据：**能否自动证明"这单修好了且没搞坏别的"。** 编排（webhook/队列/状态机）是必要但不稀缺的部分。

### 2.3 任务分诊是第二高价值动作

单子质量不合格（无复现步骤、无验收标准）时**直接打回 Linear 提问**，比硬跑一遍再产出垃圾 PR 划算得多。
预估适配率 **30–50%**，平台必须能干脆地退回人工。

---

## 3. 阶段可自动化度

| 阶段 | 可自动化 | 备注 |
|---|---|---|
| Linear 单 → worktree/分支 | ★★★★★ | 确定性代码，零 LLM |
| 单子质量分诊 | ★★★★ | LLM 判断，不合格回帖提问 |
| 定位代码 | ★★★★ | Claude Code 强项 |
| 实现 | ★★★ | 按任务类型差异极大 |
| 自动验证 | ★★ | **最难 / 最值钱 / 最易被跳过** |
| 开 PR、回写 Linear 进度 | ★★★★★ | 纯管道 |
| Review 第二轮 | ★★★ | 必须 `--resume` 原 session，不可重开 |
| 合并决策 | ★ | **不自动化**，留给人 |

### 任务类型路由

- **适合**：有复现步骤的 bug、文案/i18n、字段加减、UI 微调、补测试、依赖升级、上游同步（`merge-test` 那条线）
- **不适合**：需求未定、跨服务架构改动、需与产品/设计对齐的

---

## 4. 三个致命风险

1. **工作区与依赖的生命周期** — Lathe 自己创建的 worktree 必须自己回收（任务终结即释放），依赖目录必须走共享 store（pnpm store / 软链），不能每任务装一份。这不是运维优化，而是**决定单个节点能同时挂多少任务**，直接约束 D3 的动态并发上限。
   *（注：当前宿主机已有的历史 worktree 属机器维护，非产品范围，不在此讨论。）*
2. **并发验证的环境隔离** — 验证需真跑 CloudRouter 全栈（postgres/redis/rustfs）。多 agent 并发 = 端口冲突 + 数据污染。
   → 需 per-task compose project + 动态端口 + 独立 DB schema。**本项目最重的工程量，与 LLM 无关。**
3. **状态机持久化** — 任务生命周期跨进程重启续跑 + Linear webhook 重投递幂等。老实后端活。

---

## 5. Build vs Buy

Linear 原生 **Coding Sessions** 已支持：委派 issue → Claude Code/Codex 跑 → 出 PR 和 diff → Triage 自动化按 label 自动接单。

**仍自建的三个决定性理由：**

1. **验证跑不了** — Linear sandbox 内没有本地依赖栈，只能给"看起来对"的 diff，而验证是本产品全部价值所在。
2. **二开私有约定** — CloudRouter 带上游同步，既有偏好为「宁可复刻上游私有逻辑，也不改上游一行」，此类约束只能自己喂 skills/hooks。
3. **成本** — 原生方案消耗 workspace AI credits；自建走自有订阅 / CloudRouter。

**决策：自建执行层，复用 Linear Agent SDK 做会话展示，不自造进度 UI。**

参考实现：**Cyrus**（开源，Claude Code 接成 Linear 可指派 agent，自托管）。
已知坑：从 Claude Code session 内启动会因 `CLAUDECODE` / `CLAUDE_CODE_ENTRYPOINT` 环境变量泄漏导致子进程 exit 1，需 unset 包装。

---

## 6. 命名

**Lathe**（车床）

- 毛坯（Linear 单）上卡盘 → 按图纸加工 → 出成品（PR）；一次装夹一个工件 = 一个 worktree 一个任务。
- 隐含正确分工：人设参数、机器执行、**人验收精度**。
- CLI 友好：`lathe run CR-1326` / `lathe watch` / `lathe gc`；开发工具圈无明显撞名。

备选：Sluice（水闸／分诊调流）、Foreman（撞 Ruby foreman）。

---

## 7. 待定问题

- [ ] 验证的最低可接受标准是什么？（编译通过 / 单测 / 端到端复现原 bug）
- [ ] 自动触发的授权边界：label 触发 vs Triage 全自动
- [ ] agent 并发上限（受磁盘和 16 核 / 31G 内存约束）
- [ ] 失败任务的重试策略与人工接管入口
