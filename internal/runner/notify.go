package runner

// notify.go 任务终态的邮件通知（docs/08-debt-cleanup.md T3）。
//
// 问题：users.notify_email 这一列从 P1.5 就存在，界面也能填，但
// internal/mail 只接了密码重置 —— roadmap §0 那张「配置了但没接线」的
// 清单里就有它。后果很具体：任务失败或等着人放行时，没人告诉你，
// 你得自己盯面板。
//
// 两个设计约束：
//
//  1. **不 import internal/mail。** runner 只依赖下面这个窄接口，
//     实现放在 cmd/lathe（那里同时拿得到 store 与 mail）。这与
//     VerificationRecorder / AgentEventRecorder 是同一套做法：
//     runner 声明自己需要什么，不关心谁来满足。
//
//  2. **发信绝不影响状态流转。** 通知是副作用，不是流程的一部分。
//     SMTP 没配、投递失败、收件人查不到 —— 一律只记日志，
//     任务该进什么状态还是进什么状态。把通知做成能让任务卡住的东西，
//     等于用一个「锦上添花」的功能给主流程加了一个新的失败点。

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Clouditera/lathe/internal/task"
)

// TaskMail 给任务属主投一封通知信。
//
// 实现方负责解析收件人（users.notify_email，为空回退登录邮箱）与
// SMTP 可用性判断。**SMTP 未配置时应返回 nil（静默跳过）而非错误** ——
// 没配邮件不是异常，是默认状态。
type TaskMail interface {
	SendTaskMail(ctx context.Context, taskID int64, subject, body string) error
}

// terminalMail 渲染终态通知的主题与正文。
//
// 抽成纯函数是为了能脱离 SMTP 测正文里确实带上了该带的东西
// （照 httpapi/accounts.go 里 resetMail 的惯例，那里也写明了同样的理由）。
//
// detail 是该状态特有的补充信息：失败给失败原因，pr_open 给 PR 地址，
// awaiting_approval 给放行提示。空串则该段省略。
func terminalMail(baseURL string, tk *task.Task, detail string) (subject, body string) {
	label := stateSubject(tk.State)
	subject = fmt.Sprintf("[Lathe] %s %s", tk.LinearIssueKey, label)

	var b strings.Builder
	fmt.Fprintf(&b, "任务 #%d（%s）%s。\n", tk.ID, tk.LinearIssueKey, label)

	if tk.FailureReason != nil && *tk.FailureReason != "" {
		fmt.Fprintf(&b, "\n失败原因：\n%s\n", truncate(*tk.FailureReason, 2000))
	}
	if tk.FailureStage != nil && *tk.FailureStage != "" {
		fmt.Fprintf(&b, "\n失败阶段：%s\n", *tk.FailureStage)
	}
	if detail != "" {
		fmt.Fprintf(&b, "\n%s\n", detail)
	}

	// 详情页链接用 BaseURL 拼，不从请求头推导 —— 这里根本没有请求，
	// 而且 BaseURL 是本实例对外地址的唯一权威来源（见 config.PublicURL）。
	if baseURL != "" {
		fmt.Fprintf(&b, "\n详情：%s/tasks/%d\n", strings.TrimRight(baseURL, "/"), tk.ID)
	}

	return subject, b.String()
}

// stateSubject 把状态翻译成邮件主题里那半句人话。
func stateSubject(s task.State) string {
	switch s {
	case task.StateFailed:
		return "处理失败"
	case task.StatePROpen:
		return "已开 PR，待你评审"
	case task.StateAwaitingApproval:
		return "验证已通过，等你放行"
	case task.StateMerged:
		return "已合并"
	case task.StateCancelled:
		return "已取消"
	case task.StateBlockedSpec:
		return "需求不明确，已回帖提问"
	}
	return string(s)
}

// notifyTimeout 是一封终态通知的发信总预算。
//
// 独立于调用方的 ctx 与超时：发信是「已经发生的事」的告知，不该跟着
// 任务 ctx 一起被掐掉（任务失败时 rc.ctx 往往已经取消，而失败通知恰恰
// 是最该发出去的一封）。同 eventsink.go 的 sinkWriteTimeout 一个道理。
//
// 30s 比 mail.sessionTimeout（20s）宽：这里管的是「发信 goroutine 整个
// 生命周期」的上限，底下那 20s 才是 SMTP 会话本身。外层留出余量，
// 让正常路径由内层的协议超时收口（错误信息也更贴切），而不是被外层
// 先一步掐断。
const notifyTimeout = 30 * time.Second

// mailTerminal 是所有终态通知的唯一出口。
//
// 刻意不返回 error：调用方都在「任务已经进了终态」之后调它，
// 此时发信成败与任务无关，返回错误只会诱导调用方去处理一个
// 不该影响主流程的东西。失败在这里就地记日志。
//
// 同样刻意【不阻塞调用方】：投递丢进独立 goroutine，本函数立刻返回。
// 这条不是洁癖 —— mailTerminal 出现在 MergePoller 的串行路径上
// （mergepoll.go 的 pollOnce 用 for 循环逐个 pollTask，handleMerged
// 里 mailTerminal 又排在 onMerged 的现场回收【之前】）。同步发信意味着
// 一个假死（能连上、永不应答）的 SMTP 会把这一轮之后所有用户的 PR
// 合并检测连同现场回收一起停摆，而日志上完全看不出来。同步调用把
// 「锦上添花的功能」变成了主流程的新失败点，这正是本文件开头
// 第 2 条约束要禁止的事。
func (p *Pipeline) mailTerminal(ctx context.Context, tk *task.Task, detail string) {
	mailer := p.Mail
	if mailer == nil || tk == nil {
		return
	}
	subject, body := terminalMail(p.BaseURL, tk, detail)

	// 快照：tk 由调用方持有，goroutine 跑起来时调用方可能已经在改这个
	// 结构体（任务状态机是并发跑的），不能把指针带进去读。mailer 同理
	// 先取出来，这样 goroutine 里不碰 p 的任何字段。
	taskID, state := tk.ID, tk.State

	// ctx 只用于「取用户配置的发信通道是否就绪」这类前置查询——
	// 用一个已取消的 ctx 去查库会立刻失败，所以同样要脱钩。
	bg := context.WithoutCancel(ctx)

	go func() {
		defer func() {
			// 兜 panic：这里是流水线主流程的旁路，一个渲染或
			// 实现的空指针不该把整个进程带走。
			if r := recover(); r != nil {
				slog.Error("终态通知 goroutine panic（已在边界兜住）",
					"task", taskID, "state", state, "panic", r)
			}
		}()

		sendCtx, cancel := context.WithTimeout(bg, notifyTimeout)
		defer cancel()
		if err := mailer.SendTaskMail(sendCtx, taskID, subject, body); err != nil {
			slog.Warn("终态通知发信失败（不影响任务状态）",
				"task", taskID, "state", state, "err", err)
		}
	}()
}
