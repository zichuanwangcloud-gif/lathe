package runner

// memory.go 跨任务失败记忆的**消费侧**（docs/05-roadmap.md §3.6 第 16 条）。
//
// 与度量（internal/store/metrics.go）同属 §3.6，两条互补：度量回答
// 「平台改动有没有用」，记忆回答「这个单子上次死在哪」。
//
// ---------------------------------------------------------------------------
// 两半必须一起交（AGENTS.md §1.3）
// ---------------------------------------------------------------------------
//
// 「落了库就必须有消费方」：只把失败 digest 存进 failure_digests 而不注入
// 提示词，就是又一个「有列没消费方」—— 与 docs/08-debt-cleanup.md §0 那张
// 清单里的 exclude_dirs、baseline_dir 同类。所以本文件（注入）与 pipeline
// 的 fail() 落库（写入）是一次改动，不分两次交。
//
// ---------------------------------------------------------------------------
// 为什么注入点收敛在这一层，而不是散进各 prompt 构造函数
// ---------------------------------------------------------------------------
//
// prompt.go 的四个构造函数各有参数表，把 digest 塞进每一个的签名会把改动面
// 摊到整个 prompt.go 及其全部调用点，且每个调用点都要判空。改成「在这里
// 统一取一份、统一渲染、统一追加到最终 prompt 末尾」，好处有三：
//
//   - prompt.go 的构造函数签名一个字不用改（它们不知道自己被注入了）
//   - 注入上限、渲染格式、判空只有一处实现
//   - 与 A2 并行线不抢同一段代码（A2 改的是 resume 提示词的内容，这里改的
//     是「提示词之外追加什么」，两层不交叉）
//
// ---------------------------------------------------------------------------
// 为什么接口用 store 的类型而不是自定义一份
// ---------------------------------------------------------------------------
//
// runner 已经依赖 store（MergePoller 的 IssueOutcome 就是这个形态：
// 接口定义在 runner、方法签名用 store.TaskOutcome、*store.Store 天然满足）。
// 记忆沿用同一形态：***store.Store 直接实现 FailureMemory，不需要适配器**。
//
// 早先的写法是 runner 自己定义一对同形结构体 + 一层装配适配器，理由是
// 「runner 不该依赖 store」—— 那个理由在本仓不成立（依赖早就存在），
// 而它的代价是两份结构体要手工保持同步。仓库已有的先例更可取。

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

const (
	// maxFailureExcerpt 是注入时「验证输出摘要」的**字节**上限。
	//
	// 这个上限服务的是**提示词预算**（brief 第 3 条「注入要克制」），与
	// 落库侧的上限是两个不同的约束：库里留长一点是为了排障回看，注入的
	// 必须短。2000 字节的来由：实现 prompt 本身约 1.5KB，加上这份摘要仍在
	// 4KB 附近，相对 agent 的上下文可以忽略；而 2000 字节足以装下一次
	// `go test` 失败的全部关键几行（编译错误 + 断言输出）。
	//
	// 口径是**字节**不是字符：截断函数按字节切（再回退到 rune 边界），
	// 上限服务的是字节预算 —— 按 rune 计还要先整串解码，而截断发生在
	// 每次注入的热路径上。
	maxFailureExcerpt = 2000

	// maxDigestReason 是注入时「一句话原因」的字节上限。failure_reason 在
	// 落库前已被 fail() 截到 3000，这里再收一道 —— 注入是给人读的提醒，
	// 不是完整报告（完整报告在任务详情页与回帖里）。
	maxDigestReason = 600

	// maxDigestStoredReason 是失败原因**落库**的字节上限。与回帖正文的
	// 3000 同量级：回帖是给人扫一眼的，记忆是排障时回看的。两者都不该
	// 无限长 —— failure_reason 是自由文本，验证摘要会把编译错误整个灌进来。
	maxDigestStoredReason = 3000

	// maxDigestStoredExcerpt 是「验证输出摘要」**落库**的字节上限（含截断
	// 标记）。取失败步骤输出的**尾部**：go test / 编译器把结论（FAIL 行、
	// 断言、panic 栈底）写在最后。比注入上限宽，是为了排障回看时多留几行
	// 上下文；它仍远小于 verify 的 maxStepOutput（16KB），单条不会把库撑爆。
	maxDigestStoredExcerpt = 8 << 10 // 8KB
)

// FailureMemory 是流水线用到的失败记忆读写面。
//
// *store.Store 直接实现（方法签名与 store 的两个方法一一对应），所以装配
// 层不需要适配器。定义成接口是为了 Pipeline 的单测能注入假件 ——
// 一个「有历史失败」的用例不该被迫在真实 Postgres 里造两张表。
//
// 一读一写两个方法在同一个接口里，是刻意的：AGENTS.md §1.3 要求「落了库就
// 必须有消费方」，两半拆成两个接口就会有人只接其中一个 —— 只写不读是死
// 数据，只读不写永远读不到东西。
type FailureMemory interface {
	// LatestFailureDigest 返回该单子最近一次失败；没有历史失败时返回
	// (nil, nil)。excludeTaskID 非零时排除该任务自己的记录
	// （见 withFailureMemory 的 alreadyInPrompt 参数）。
	LatestFailureDigest(ctx context.Context, lk store.FailureLookup, excludeTaskID int64) (*store.FailureDigest, error)

	// InsertFailureDigest 追加一条失败记忆。实现必须保证「落库失败不影响
	// 任务收尾」—— 与 InsertAgentEvents 同一纪律。
	InsertFailureDigest(ctx context.Context, d store.FailureDigest, lk store.FailureLookup) error
}

// renderFailureMemory 把一份 digest 渲染成注入提示词的一段。
//
// 措辞的取向：**说清这是历史、不是本次的要求**。agent 看到「上次失败」若
// 误以为是自己刚犯的错，会去修一个可能已经修好的东西 —— 所以开头明说
// 「另一个更早的任务」，并给出行动指引（先确认是否仍然成立）。
//
// 返回空串表示没有可注入的内容（digest 为 nil 或三个字段全空），调用方
// 据此让提示词保持原样。
func renderFailureMemory(d *store.FailureDigest) string {
	if d == nil {
		return ""
	}
	stage := strings.TrimSpace(d.Stage)
	reason := strings.TrimSpace(d.Reason)
	excerpt := strings.TrimSpace(d.Excerpt)
	if stage == "" && reason == "" && excerpt == "" {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\n---\n\n## 这个需求此前失败过（平台的历史记录）\n\n")
	b.WriteString("**注意：这不是你本次执行的结果**，是同一个需求单更早的一次执行留下的记录。\n")
	b.WriteString("先判断这个失败是否仍然成立 —— 已经修好的话不要重复处理；仍然存在的话，绕开它。\n\n")

	if d.At != "" {
		fmt.Fprintf(&b, "- 发生时间（UTC）：%s\n", d.At)
	}
	if stage != "" {
		fmt.Fprintf(&b, "- 失败阶段：%s\n", stage)
	}
	if reason != "" {
		fmt.Fprintf(&b, "- 失败原因：%s\n", truncate(reason, maxDigestReason))
	}
	if excerpt != "" {
		b.WriteString("\n失败时的输出摘要：\n\n```\n")
		// 取**尾部**：摘要是失败步骤输出的后半段（结论在后面），从头部
		// 截会把最有用的几行切掉。tail 按字节切且回退到 rune 边界，
		// 不会切出非法 UTF-8。
		b.WriteString(tail(excerpt, maxFailureExcerpt))
		b.WriteString("\n```\n")
	}
	return b.String()
}

// withFailureMemory 在 prompt 末尾追加历史失败记忆。
//
// 追加在**末尾**而不是开头，是刻意的：agent 读到的第一段应该是本次任务
// 本身，历史放最后作为「注意」而非「前提」—— 它可能已经过时（人可能已经
// 手工修过、或那个失败本就与本次无关）。
//
// alreadyInPrompt 为真时**排除本任务自己的记录**。这条规则的必要性来自
// 重试的实现形态：人工重试走的是原任务行（retryTask 不新建 —— 任务 #313
// 的教训），于是「跨任务记忆」在这条最常见的路径上会读到任务自己写下的
// 那一条。分两种情况：
//
//   - **续跑路径**（resume 原会话）：失败原因已经由 ResumeFixPrompt 带在
//     提示词里了，再注入一遍是同义反复 —— 排除本任务。
//   - **全新会话**（Fresh 重建 / 新任务行）：agent 脑子里一片空白，上一轮
//     的失败对它而言就是货真价实的历史，读自己的记录正是想要的行为
//     —— 不排除。
//
// memory 为 nil、查询失败、或没有历史失败时，prompt 原样返回：记忆是辅助
// 信息，取不到不该改变任何流程。
func (p *Pipeline) withFailureMemory(ctx context.Context, sc failureScope, prompt string, alreadyInPrompt bool) string {
	if p.Memory == nil {
		return prompt
	}
	var exclude int64
	if alreadyInPrompt {
		exclude = sc.taskID
	}
	d, err := p.Memory.LatestFailureDigest(ctx, sc.lookup(), exclude)
	if err != nil {
		// 与「落库失败只告警」同一条纪律：可观测性不该反过来成为任务的
		// 故障源。查不到记忆就是没有记忆，任务照常跑。
		slog.Warn("查询失败记忆失败，本次不注入", "task", sc.taskID, "err", err)
		return prompt
	}
	extra := renderFailureMemory(d)
	if extra == "" {
		return prompt
	}
	slog.Info("注入了失败记忆", "task", sc.taskID, "issue", sc.externalKey, "stage", d.Stage)
	return prompt + extra
}

// recordFailureMemory 把这次失败写进跨任务记忆（§3.6 第 16 条）。
//
// 调用点是 fail() 的状态转移【之后】：只有任务真的落成了 failed，这条记忆
// 才成立 —— 与终态通知同一个理由（「只有状态真的落库了，通知才不会说谎」）。
//
// 任何失败都只告警：记忆是辅助信息，它绝不能反过来成为任务失败的第二个
// 原因，也不能改变 fail() 的返回值语义。
func (p *Pipeline) recordFailureMemory(rc *runCtx, stage Stage, cause error, excerpt string) {
	p.recordFailureMemoryFor(rc.ctx, rc.scope.taskID, rc.scope.lookup(), stage, cause, excerpt)
}

// recordFailureMemoryFor 是 recordFailureMemory 的「无 runCtx」版本。
//
// 存在的理由：MergePoller 的 rebase 跟进链路（failRebaseFollowup）不在
// runCtx 的管辖范围内 —— 它由合并检测驱动，没有流水线上下文。那条路径
// 早就为此单独抄了一份 fail() 的失败传播逻辑（见 failRebaseFollowup 的
// 注释），记忆写入同样需要一个不依赖 runCtx 的入口。
//
// 与上面共用同一段落库逻辑，只是定位字段从入参现取 —— 两条路径的语义
// 不会漂移。
func (p *Pipeline) recordFailureMemoryFor(ctx context.Context, taskID int64, lk store.FailureLookup, stage Stage, cause error, excerpt string) {
	if p.Memory == nil {
		return
	}
	d := store.FailureDigest{
		TaskID: taskID,
		Stage:  string(stage),
		// 原因同样过一遍脱敏：失败原因会把验证错误原样带进来（回帖正文
		// 就是这么拼的），而回帖只给人看一眼，这份要在库里躺着、还会被
		// 注入下一个 agent 的提示词。nil 环境 —— 这条路径拿不到具体步骤。
		Reason:  redactSecrets(truncate(fmt.Sprintf("%s: %v", stage.label(), cause), maxDigestStoredReason), nil),
		Excerpt: excerpt,
	}
	if err := p.Memory.InsertFailureDigest(ctx, d, lk); err != nil {
		slog.Warn("失败记忆落库失败（不影响任务收尾）", "task", taskID, "err", err)
	}
}

// failureScope 是注入与落库所需的定位字段，从任务行摊出来。
//
// 单独一个小值类型而不是直接吃 *task.Task：注入发生在分诊与实现两处，
// 而分诊阶段 rc 里除 tk 之外的东西都还没建起来（没有 worktree、没有
// issue）。摊成值类型让两条路径统一，单测也不必造整个 runCtx。
type failureScope struct {
	taskID      int64
	userID      int64
	repoID      int64
	provider    string
	externalKey string
}

// failureScopeOf 从任务行摊出定位字段。
func failureScopeOf(tk *task.Task) failureScope {
	return failureScope{
		taskID:      tk.ID,
		userID:      tk.UserID,
		repoID:      tk.RepoID,
		provider:    tk.TrackerProvider,
		externalKey: tk.ExternalKey,
	}
}

// lookup 把 scope 转成 store 侧的定位键。
func (sc failureScope) lookup() store.FailureLookup {
	return store.FailureLookup{
		UserID:          sc.userID,
		RepoID:          sc.repoID,
		TrackerProvider: sc.provider,
		ExternalKey:     sc.externalKey,
	}
}
