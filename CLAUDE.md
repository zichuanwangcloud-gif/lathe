# CLAUDE.md

本仓的 agent 工作规范统一维护在 `AGENTS.md`（唯一本体，勿复制成两份），
通过导入生效：

@AGENTS.md

---

## Claude Code 专属补充

- **技能边界别搞反**：本仓 `skills/` 下的 `go-testing`、`sql-migration` 是
  Lathe 产品**发给目标仓库 worktree** 的技能（打进二进制、运行时物化），
  不是给你用的本仓技能。但它们的内容就是本仓的测试/迁移规范，工作时应遵守；
  修改它们视同修改产品行为（AGENTS.md §3 表格「skills/**」行）。
- **本仓自己的运行现场别当源码**：`.claude/worktrees/`、`workspaces/` 是
  Lathe 跑任务留下的现场与 Claude Code 的本地 worktree，`make lint` 都刻意
  排除它们。搜索代码时若命中这些目录，结果不代表现网行为。
- **起子进程务必显式设 `Dir` 与环境**：本仓自己就是 B2-3 事故的受害者
  （triage 继承 cwd 把本仓 CLAUDE.md 灌进目标仓库分诊上下文），详见
  AGENTS.md §9。
- 驱动 agent 相关的代码路径在 `internal/runner/`（管线、验证）与
  `internal/integration/agent/`（CLI driver），调 agent 行为从这两处入手。
