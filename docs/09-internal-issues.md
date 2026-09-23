# Lathe — 09 · 设计：内置工单体系（手动任务入口）

> 2026-09-19 · 状态：**P1 已交付**（迁移 0021 + tracker 窄接口 + HTTP API + UI + 全链路测试）
> 前置：[02-design.md](./02-design.md)（状态机）、[07-prd-orchestration.md](./07-prd-orchestration.md)（F1.5/F6 数据契约）
> 范围替代关系：本文把 07 的 **F1.5「纯本地图」从『无 tracker』升级为『内置 tracker』**——
> 需求描述、评论区、附件由 Lathe 自己持有，而不是"没有平台就什么都没有"。
>
> 交付实况：P1（核心闭环）已上线 —— 工单 CRUD/列表/详情/评论区、开跑/取消联动、
> pipeline 平台解耦（`Clients.Tracker(provider)` 分派）、冒烟验证通过
> （建单 → 开跑 → 分诊失败 → 失败回帖落内置评论区 → 取消工单联动）。
> P2（附件）、P3（编排图接入）未做。

---

## 1. 概述

### 1.1 一句话

**Lathe 内置一套工单（issue）体系：手动建单、写需求、评论区问答、附件上传；
工单可以一键开跑，走与 Linear 完全相同的状态机与验证链路，产出已验证的 PR。**

### 1.2 问题陈述（证据）

| 现状 | 证据 |
|---|---|
| 所有任务入口硬绑 Linear：webhook 指派/标签、看板手动执行、编排图节点 | `api.go:210` triggerTask 缺 issueId 直接 400；`flow/service.go:88` validateNodes 拒绝无 issue 节点 |
| `tasks.linear_issue_key NOT NULL`，任务表无需求描述列 | migration 0001；`flow/service.go:44` "Title…不落库" |
| pipeline 的 setup 拿不到 Linear 客户端直接报错，Linear 凭据失效 = 全平台停摆 | `pipeline.go:361-363` |
| 平台自测被迫在 Linear 建假单，留下尸体 | 05-roadmap §1.3：任务 #217（SMOKE-2 排队尸体） |
| blocked_spec 的问答通道只有 Linear 评论 | `pipeline.go:479` 提问回帖；`linear.go:161` Context() 把评论拼进分诊上下文 |

### 1.3 核心架构判断（为什么这个方案便宜）

pipeline 对需求平台的**全部消费面**是两个方法（`pipeline.go:22-25`）：

```go
type LinearAPI interface {
    Issue(ctx context.Context, id string) (*linear.Issue, error)   // 标题+描述+评论 → 分诊上下文
    Comment(ctx context.Context, issueID, body string) (string, error) // 提问/失败说明/PR 链接回帖
}
```

因此内置工单体系 = **给这个窄接口写一个 Postgres 支撑的第二实现**：

- 分诊上下文：`Issue.Context()`（`linear.go:161`）的拼装格式原样保留，
  内置工单的标题/描述/评论走同一段代码，agent 看到的输入零差别；
- **提问模式不变**：blocked_spec 提问 → 写内置评论区；人在工单详情页回复；
  重试时分诊重新读评论——与 Linear 路径逐字节同构，不引入新状态、新语义；
- 分支命名：`{kind}/{key}-{slug}` 的 `{key}` 换内置 key（`LT-1042`）即可，
  `RepoConfig.BranchPattern` 机制不动（`runner/branch.go:35`）。

**不做"无 tracker 的裸任务"**：任务永远挂在某个工单上（Linear 的或内置的），
`tasks` 表不需要需求描述列——07 的未决项 U8 由此以另一种方式关闭：
描述活在 `issues` 表，与评论、附件同处，单一事实源。

### 1.4 目标

1. 不依赖 Linear 完成「建单 → 写需求 → 开跑 → 问答 → PR」全流程
2. Linear 路径**零行为回归**（迁移前后同一 issue 的分支名、PR、回帖不变）
3. 内置工单有评论区（人+agent 同轨）与附件上传
4. 与 Linear 工单同一防重语义：同一工单不允许两个活任务

### 1.5 非目标（明确不做）

| 非目标 | 理由 |
|---|---|
| Tracker 能力位 `Caps()` / 平台注册表 | 07 F6.2 的 Caps 是为多平台能力协商准备的；内置 tracker 全能力、无 webhook、无凭据，v1 没有消费方（05-roadmap §0 纪律）。云效探针（F6.4）之前不发明接口 |
| 编排图接入内置工单 | 需要 `flows.tracker_provider` 列与画布双源 picker，切到 P3，见 §5 |
| 工单之间的依赖关系（blocked by 等） | 编排图的 `depends_on` 已解决执行依赖；工单层关系无场景 |
| 评论级附件、@提醒、富文本编辑器 | v1 附件挂在工单级；markdown 纯文本输入 + 预览足够 |
| 工单导入导出 / 与 Linear 双向同步 | 无场景；两个体系各自独立，不建同步桥 |

---

## 2. 用户场景

### S1 —— 手动建单开跑（主场景）

人在「工单」页点「新建工单」：选仓库、填标题、markdown 写需求描述、传一张
报错截图。保存后在工单详情点「开始执行」→ 任务 `queued` → 分诊 → 实现 →
验证 → PR。全程不碰 Linear。

### S2 —— 需求不清，问答补充（提问模式不变）

agent 分诊判定单子不明确 → 转 `blocked_spec` 并在工单评论区提问（与 Linear
回帖同一条代码路径）→ 人收到终态邮件（`notify.go:87` 已有），打开工单页在
评论区补充 → 点「重试」→ 分诊重读描述+全部评论，继续。问答记录留在工单上，
成为需求的一部分。

### S3 —— 平台自测 / 冒烟

建一个 LT 工单当试验单，验证平台自身的改动（重试、闸门、预览），
不再向 Linear 倒垃圾（05-roadmap §1.3 的 SMOKE 尸体事故）。

### S4 —— 取消与重来

工单详情页「取消」→ 在途任务转 `cancelled`（复用 T7 取消联动的同一条
`CancelForIssue` 路径，只是触发点从 webhook 换成应用内按钮）。工单回到
`open`，之后可再次开跑（历史任务仍在，唯一索引只挡活任务）。

---

## 3. 功能点与验收标准

**验证方式图例**：`单测` `集成`（假 GitHub + 短路 agent）`端到端` `人工`

### F1 工单 CRUD 与列表

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | 新建工单必填：仓库（下拉）、标题；描述可空（分诊上下文里渲染为"（无描述）"，与 Linear 一致） | 集成 |
| AC2 | key 自动生成，格式 `LT-<n>`，**全库唯一**（不是 per-user），允许因回滚产生空洞 | 单测 + 集成 |
| AC3 | 列表页按属主隔离：非属主访问他人工单/评论/附件一律 404（沿用"不暴露存在"原则） | 集成 |
| AC4 | 工单四态 `open / in_progress / done / cancelled`，人在详情页可改；非法转移被拒 | 单测 |
| AC5 | 有关联任务（含历史）的工单**不可删除**，只能 `cancelled`；无关联的可删，删除级联清评论与附件文件 | 集成 |

### F2 评论区与提问回路

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | 评论区分两种作者：人（用户名）与 agent（标 `lathe · 任务 #id`，可点跳任务详情） | 人工 |
| AC2 | blocked_spec 提问落在内置评论区，与 Linear 回帖是**同一段 pipeline 代码**（`grep` 断言 pipeline 无 `if provider ==` 分支） | 代码走查 |
| AC3 | 人在评论区补充后点重试，分诊上下文包含新评论（`Context()` 输出断言） | 集成 |
| AC4 | 评论 markdown 渲染经 DOMPurify 消毒（复用 `TaskDetail.vue:88` 的 `md()`） | 代码走查 |
| AC5 | 空评论、超长评论（>10k 字符）被拒并返回可读错误 | 单测 |

### F3 附件

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | 上传经鉴权，单文件 ≤ 20MB（系统设置可调），超限返回可读错误 | 集成 |
| AC2 | 文件落在 `$LATHE_DATA_DIR/attachments/<issueID>/<uuid>`，原始文件名只存数据库；**下载路径不拼接用户输入**（防路径穿越） | 单测 + 集成 |
| AC3 | 下载走鉴权端点，非属主 404；响应带 `Content-Disposition` 与 `X-Content-Type-Options: nosniff`；`image/*` 允许 inline 预览 | 集成 |
| AC4 | 分诊上下文末追加「## 附件」清单（文件名 + 本机绝对路径 + content-type），agent 可用 Read 工具查看图片/日志 | 集成 |
| AC5 | 工单删除时磁盘文件一并清理；清理失败落日志不阻塞删除 | 集成 |

### F4 工单 → 任务联动

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | 「开始执行」用**工单上登记的 repo_id**建任务，不走 `resolveRepo` 的"每用户第一个仓库"（`queue.go:92` 的老随机性对内置路径不生效） | 集成 |
| AC2 | 同一工单有活任务时「开始执行」被拒（撞 `tasks_one_active_per_item`，翻译成人话错误，同 `ErrIssueActive`） | 单测 + 集成 |
| AC3 | 任务 `merged` → 工单自动 `done`；任务建出 → 工单自动 `in_progress`；任务取消 → 工单回 `open`（人在 UI 改态优先于自动联动，联动只在这三个方向单调发生） | 集成 |
| AC4 | 工单 `cancelled` → 在途任务走 `CancelForIssue` 同语义取消：**只改库状态，不停 agent**（已知边界与 T7 一致，写入文档不藏着） | 集成 |
| AC5 | 内置任务的分支名符合 `repos.branch_pattern`（如 `fix/lt-1042-xxx`），热修复走 hotfix 基线 | 集成 |
| AC6 | 内置任务**零外发**：不调用 Linear API、不产生 Linear 评论；GitHub 推送/开 PR 正常 | 集成（断言 mock Linear 客户端零调用） |

### F5 pipeline 平台解耦

| # | 验收标准 | 验证 |
|---|---|---|
| AC1 | `LinearAPI` 窄接口改名 `Tracker` 并迁到 `internal/tracker` 包（`Issue`/`Comment`/`Context()` 一并搬家），Linear 客户端以类型别名适配，**Linear 包内部零改动** | 代码走查 |
| AC2 | pipeline/runner 层 `grep -n "linear"` 仅剩：tracker 分派处、迁移兼容注释 | 代码走查（lint 断言） |
| AC3 | `Clients` 接口按 `tasks.tracker_provider` 分派：`linear` 走凭据解析（现逻辑），`internal` 直接用 DB 构造（无凭据概念） | 单测 + 集成 |
| AC4 | 迁移前后各跑一次同一 Linear issue 的任务：分支名、PR 标题、回帖内容完全一致 | 集成（回归） |
| AC5 | `tasks.linear_issue_key`→`external_key`、`linear_issue_id`→`external_id` 改名后，全仓 `grep LinearIssueKey` 为零 | 代码走查 |

---

## 4. 数据契约

### 4.1 新增表（migration 0021）

```sql
-- 内置工单。key 全局唯一（LT-<全局序列>），不按用户分段：
-- 人要念它、分支名要用它，跨用户撞 key 徒增心智负担。
CREATE TABLE issues (
  id          bigserial PRIMARY KEY,
  user_id     bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  repo_id     bigint      NOT NULL REFERENCES repos(id) ON DELETE RESTRICT, -- 有工单的仓库不许删
  key         text        NOT NULL UNIQUE,        -- LT-1042
  title       text        NOT NULL,
  description text        NOT NULL DEFAULT '',
  state       text        NOT NULL DEFAULT 'open',
  priority    int         NOT NULL DEFAULT 0,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT issues_state_check
    CHECK (state IN ('open','in_progress','done','cancelled'))
);
CREATE INDEX issues_owner ON issues (user_id, state, updated_at DESC);

-- key 来源：全局序列，允许回滚空洞（序列语义本就如此，不为空洞补偿）
CREATE SEQUENCE issue_key_seq START 1;

CREATE TABLE issue_comments (
  id         bigserial PRIMARY KEY,
  issue_id   bigint      NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
  -- author 二选一：user_id 非空 = 人；actor 非空 = agent（'task-<id>'，与
  -- task_events 的 actor 前缀同源，UI 据此渲染"lathe · 任务 #id"并可跳转）
  user_id    bigint      REFERENCES users(id) ON DELETE SET NULL,
  actor      text,
  body       text        NOT NULL CHECK (length(body) BETWEEN 1 AND 10000),
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT issue_comments_author_check
    CHECK ((user_id IS NOT NULL) <> (actor IS NOT NULL))
);
CREATE INDEX issue_comments_issue ON issue_comments (issue_id, id);

CREATE TABLE issue_attachments (
  id           bigserial PRIMARY KEY,
  issue_id     bigint      NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
  filename     text        NOT NULL,              -- 原始名，仅展示用
  content_type text        NOT NULL,
  size_bytes   bigint      NOT NULL CHECK (size_bytes <= 20971520), -- 20MB
  storage_name text        NOT NULL,              -- uuid，磁盘上的真实文件名
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX issue_attachments_issue ON issue_attachments (issue_id);
```

### 4.2 `tasks` 列变更（07 §4.2 的 F6.1 部分落地）

| 变更 | 说明 |
|---|---|
| `linear_issue_key` → `external_key` | 改名，**保留 NOT NULL**：内置任务也有 key（LT-xxxx），"无 tracker 裸任务"不存在了（§1.3） |
| `linear_issue_id` → `external_id` | Linear 存 UUID；内置任务存 NULL（内置 tracker 按 `(owner, key)` 解析，不需要第二个标识） |
| 新增 `tracker_provider` | `text NOT NULL DEFAULT 'linear'`，CHECK 限 `'linear'/'internal'`；存量行由 DEFAULT 自动归 linear |

```sql
-- 替换 tasks_one_active_per_issue：同一工单（不分平台）只允许一个活任务
DROP INDEX tasks_one_active_per_issue;
CREATE UNIQUE INDEX tasks_one_active_per_item
  ON tasks (repo_id, tracker_provider, external_key)
  WHERE state NOT IN ('merged','failed','cancelled');
```

> 保留 `repo_id` 在索引里（07 §4.4 草案没有它）：与现有语义逐一对齐，
> 且内置工单本身绑仓库，同一 key 不可能跨仓库出现，多这一列无害。

### 4.3 状态机

**`tasks` 状态机零变更**——这是本文最重要的一句话。blocked_spec、重试、
闸门、修复回路全部复用。工单自己的四态（§F1-AC4）是独立的小状态机，
与任务状态机只通过 F4-AC3 的三条单向联动相连。

---

## 5. 分期与出口条件

| 里程碑 | 内容 | 出口条件 |
|---|---|---|
| **P1 核心闭环** | 4.1/4.2 迁移（不含附件表）、`internal/tracker` 搬家、内置 tracker 实现、F1/F2/F4/F5、工单列表+详情 UI（新建/编辑/评论/开始执行/取消） | 短路 agent + 假 GitHub 的集成测试里，LT 工单从新建跑到 `pr_open`，blocked_spec 问答一个来回；同一 Linear issue 回归任务产物与迁移前 diff 为空 |
| **P2 附件** | `issue_attachments`、上传/下载端点、磁盘存储、分诊上下文注入 | F3 全部 AC；上传一张图片后 agent 事件流里能看到 Read 该文件 |
| **P3 编排图接入** | `flows.tracker_provider`（NULL→linear 默认）、画布 picker 双源、内置节点入队 | 一张含 LT 节点的图跑通 07 的 S1；07 F1.5-AC1/AC2 就此关闭 |

**P1 之前的硬前置**（继承 07 §8.3）：`make dev-infra && make migrate` 必须先做，
迁移测试与领单测试在拿不到 Postgres 时静默 skip，绿灯不算数。

---

## 6. 风险与缓解

| # | 风险 | 缓解 |
|---|---|---|
| R1 | `external_key` 改名是全仓机械改动（store/machine/pipeline/httpapi/flow/UI），漏一处就是线上 500 | 一次性改完不分期；`grep LinearIssueKey` 归零作为验收（F5-AC5）；现有集成测试覆盖 Linear 回归路径 |
| R2 | 内置 tracker 让窄接口悄悄长成"Linear 形状 plus" | 接口就是现有两方法改名，**禁止加方法**；新方法要有真实消费方才准进（05 §0） |
| R3 | 附件撑爆磁盘 | 单文件 20MB 上限；工单删除级联清文件；后续可挂进现有 reaper 的孤儿清扫（P2 只做删除级联，清扫列未决） |
| R4 | 评论 XSS | 复用现有 marked + DOMPurify 管线（`TaskDetail.vue:88`），不引入第二套渲染 |
| R5 | 用户把 Linear 工单和内置工单混着用导致心智分裂 | UI 分两个菜单入口（Linear issues 页保持原样）；不建同步桥（非目标）；后续看真实使用再决定是否合并视图 |

---

## 7. 决策与未决

### 7.1 已定（2026-09-19）

| # | 议题 | 决定 |
|---|---|---|
| N1 | 裸任务（无工单）还是内置工单 | **内置工单**——任务永远有需求载体，U8 关闭，提问回路零新机制 |
| N2 | key 形态 | 全局序列 `LT-<n>`，允许空洞；不做 per-user 分段、不做可配前缀 |
| N3 | 工单是否绑仓库 | **必绑**——内置路径修掉 resolveRepo"每用户第一个仓库"的随机性，分支 pattern 有着落 |
| N4 | Tracker 接口范围 | 现有 `LinearAPI` 两方法改名搬家；不做 Caps()、不做平台注册表 |
| N5 | 提问模式 | 完全复用 blocked_spec + 评论区 + 重试，零新状态零新语义 |
| N6 | 附件 v1 粒度 | 工单级，不进评论；分诊上下文附清单与本机路径 |

### 7.2 未决（需要拍板）

| # | 未决 | 倾向 |
|---|---|---|
| U1 | UI 菜单命名：「工单」还是「任务列表」 | 「工单」——与看板（执行态任务）划界，避免"任务"一词两义 |
| U2 | 附件孤儿文件（工单没删、DB 行没了）是否纳入 reaper 孤儿清扫 | P2 先不做，观察真实磁盘占用再定 |
| U3 | agent 评论是否触发邮件通知（人评论不通知自己，agent 评论呢） | 复用现有终态邮件已够；评论级通知等真实噪音出现再加 |
| U4 | 描述里贴图是否自动插入 markdown 链接 | v1 上传后自动在光标处插入 `![名](/api/issues/.../attachments/...)` |
