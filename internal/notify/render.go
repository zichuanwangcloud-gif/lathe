package notify

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// 渲染上限。
const (
	// maxReasonRunes 是正文里失败原因的 rune 上界（防御性：runner 侧已按字节截过）。
	maxReasonRunes = 800
	// maxTextBytes 是正文的字节上界：飞书 9499 = 请求体超过 20 KB，
	// 留出 JSON 包装与转义的余量。
	maxTextBytes = 8 * 1024
)

// StateLabel 把任务状态翻译成半句人话 —— 邮件主题、站外正文、设置页三处
// 的**唯一**字面量来源（runner.stateSubject 调它）。
//
// 六个键的值逐字取自原 runner.stateSubject，一字不改；未知状态回退原串。
// merged / cancelled 本功能不推送，但邮件要用，表必须是全集。
func StateLabel(state string) string {
	switch state {
	case "failed":
		return "处理失败"
	case "pr_open":
		return "已开 PR，待你评审"
	case "awaiting_approval":
		return "验证已通过，等你放行"
	case "merged":
		return "已合并"
	case "cancelled":
		return "已取消"
	case "blocked_spec":
		return "需求不明确，已回帖提问"
	}
	return state
}

// EventLabel 把事件种类翻译成人话。四个任务事件 = StateLabel(对应状态)，不另写一份。
func EventLabel(eventType string) string {
	switch eventType {
	case EventFailed, EventBlockedSpec, EventAwaitingApproval, EventPROpen:
		return StateLabel(eventType)
	case EventPRDReady:
		return "模糊任务待你签字"
	case EventTest:
		return "测试消息"
	}
	return eventType
}

// Facts 是渲染需要的全部输入，只由 FactsFromPayload 构造：
// 每个字段对应 payload 契约的一个键（BaseURL 除外，它是 Worker 构造参数）。
type Facts struct {
	TaskID        int64
	ExternalKey   string
	State         string
	FailureReason string // runner 侧已脱敏；这里再按 rune 截到 maxReasonRunes
	FailureStage  string
	PRURL         string
	PRDID         int64
	PRDRepo       string
	TestNonce     string
	BaseURL       string // 空则省略链接行（同 runner.terminalMail 的既有选择）
}

// Render 渲染一条通知的标题与正文（Slack text 与飞书 content.text 共用纯文本）。
//
// 纯函数：不含任何渠道信息（URL/密钥根本不是入参），截断按 rune。
func Render(eventType string, f Facts) (title, text string) {
	base := strings.TrimRight(f.BaseURL, "/")
	label := EventLabel(eventType)
	var b strings.Builder

	switch eventType {
	case EventPRDReady:
		title = fmt.Sprintf("[Lathe] 模糊任务 #%d %s", f.PRDID, label)
		fmt.Fprintf(&b, "模糊任务 #%d 已通过自检，等你逐节签字。\n", f.PRDID)
		if f.PRDRepo != "" {
			fmt.Fprintf(&b, "仓库：%s\n", f.PRDRepo)
		}
		if base != "" {
			fmt.Fprintf(&b, "\n详情：%s/prds/%d\n", base, f.PRDID)
		}
	case EventTest:
		title = "[Lathe] " + label
		b.WriteString("这是一条来自 Lathe 的测试消息：收到即说明渠道配置可用。\n")
		if f.TestNonce != "" {
			fmt.Fprintf(&b, "测试编号：%s\n", f.TestNonce)
		}
	default:
		stateLabel := StateLabel(f.State)
		title = fmt.Sprintf("[Lathe] %s %s", f.ExternalKey, stateLabel)
		fmt.Fprintf(&b, "任务 #%d（%s）%s。\n", f.TaskID, f.ExternalKey, stateLabel)
		if f.FailureStage != "" {
			fmt.Fprintf(&b, "失败阶段：%s\n", f.FailureStage)
		}
		if r := strings.TrimSpace(f.FailureReason); r != "" {
			fmt.Fprintf(&b, "失败原因：\n%s\n", truncateRunes(r, maxReasonRunes))
		}
		if f.PRURL != "" {
			fmt.Fprintf(&b, "PR：%s\n", f.PRURL)
		}
		if base != "" {
			fmt.Fprintf(&b, "\n详情：%s/tasks/%d\n", base, f.TaskID)
		}
	}
	return title, truncateBytes(b.String(), maxTextBytes)
}

// truncateRunes 按 rune 截到 n 个；不加后缀（截断本身由调用方决定是否说明）。
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// truncateBytes 按字节上界截断，回退到 rune 边界（不切坏多字节字符）。
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
