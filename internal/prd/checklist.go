package prd

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// 本文件是定稿自检清单里「机器可判」那一档（docs/10 §6.1）：
// 结构校验失败即拒绝进 ready_for_review，不修正。
//
// 为什么失败不自动修正：这些校验拦的都是「智能体自己没想周全」的洞
// （孤儿 AC、形容词冒充标准、超限任务）。自动补一个占位符只会让洞通过
// 检查却仍然存在 —— 与红-绿判决同一逻辑，检查的价值全在它真能拦住东西。

// Finding 是一条校验发现。
type Finding struct {
	// Code 是稳定的机器标识，前端据它定位到对应节。
	Code string `json:"code"`
	// Section 是出问题的节（Document.Sections 的 Key），空表示跨节问题。
	Section string `json:"section,omitempty"`
	Message string `json:"message"`
}

func (f Finding) String() string {
	if f.Section != "" {
		return fmt.Sprintf("[%s %s] %s", f.Code, f.Section, f.Message)
	}
	return fmt.Sprintf("[%s] %s", f.Code, f.Message)
}

// 校验码。前端按 Code 决定跳到哪一节，别改字面量。
const (
	CodePendingSection    = "pending_section"
	CodeUnconfirmed       = "unconfirmed_section"
	CodeOneLinerTooLong   = "one_liner_too_long"
	CodeOneLinerEmpty     = "one_liner_empty"
	CodeCategoryMissing   = "ac_category_missing"
	CodeNAWithoutReason   = "ac_na_without_reason"
	CodeACIncomplete      = "ac_incomplete"
	CodeBannedWord        = "ac_banned_word"
	CodeManualNoSteps     = "ac_manual_without_steps"
	CodeOrphanGoal        = "orphan_goal"
	CodeOrphanScenario    = "orphan_scenario"
	CodeOrphanAC          = "orphan_ac"
	CodeDanglingRef       = "dangling_ref"
	CodeTaskKeyDup        = "task_key_duplicate"
	CodeTaskDepMissing    = "task_dep_missing"
	CodeTaskCycle         = "task_cycle"
	CodeTaskNoAcceptance  = "task_no_acceptance"
	CodeTaskKindInvalid   = "task_kind_invalid"
	CodeTaskOversize      = "task_oversize"
	CodeChainTooLong      = "chain_too_long"
	CodeNoReviewReport    = "review_report_missing"
	CodeReviewUnhandled   = "review_finding_unhandled"
	CodeOpenQuestion      = "question_open"
	CodeRefactorFirstNode = "refactor_first_node"
	CodeReverseUnanswered = "reverse_confirm_unanswered"
)

// bannedWords 是 §5.3 的禁用词表：形容词冒充标准。
//
// 出现即打回，除非旁边有数字或判据 —— 所以判定不是「出现就报」，而是
// 「出现且这条 AC 没有 Evidence」才报（见 checkCriteria）。
var bannedWords = []string{
	"正确", "合理", "友好", "快速", "稳定", "完善", "优雅", "健壮",
}

// oneLinerMaxRunes 是 §1 的长度上限（docs/10 §2：≤ 60 字）。
// 按 rune 数而不是 byte 数 —— 中文一个字三个 byte，按 byte 卡等于卡 20 字。
const oneLinerMaxRunes = 60

// Limits 是仓库级的量级阈值（repos.prd_task_max_lines / _max_files）。
type Limits struct {
	MaxLines int
	MaxFiles int
	// MaxChain 是链长上限（system_settings.flow_max_chain_length，默认 4）。
	// 超了是警告不是拒绝（07 §F3.3），所以单独归入 Warnings。
	MaxChain int
}

// Report 是一次自检的完整结果。
type Report struct {
	// Blocking 非空则不许进 ready_for_review。
	Blocking []Finding `json:"blocking"`
	// Warnings 不阻塞，但要显示给人（如链长超限）。
	Warnings []Finding `json:"warnings"`
}

// OK 报告是否可以进评审。
func (r *Report) OK() bool { return len(r.Blocking) == 0 }

// Error 把阻塞项拼成一条可读错误，供 API 层直接回给前端。
func (r *Report) Error() error {
	if r.OK() {
		return nil
	}
	parts := make([]string, 0, len(r.Blocking))
	for _, f := range r.Blocking {
		parts = append(parts, f.String())
	}
	return fmt.Errorf("prd: 自检未过（%d 项）: %s", len(r.Blocking), strings.Join(parts, "; "))
}

// Check 跑完整的机器可判校验。
//
// hasReview 表示对抗复核报告是否已就位（§5.5 必跑，D10-6）；
// reviewUnhandled 是复核发现里还没标处置的条数。
func Check(d *Document, blocks *TaskBlocks, lim Limits, hasReview bool, reviewUnhandled int) *Report {
	r := &Report{Blocking: []Finding{}, Warnings: []Finding{}}

	checkSections(d, r)
	checkOneLiner(d, r)
	checkCriteria(d, r)
	checkTraceability(d, blocks, r)
	checkTasks(d, blocks, lim, r)
	checkQuestions(d, r)
	checkReview(hasReview, reviewUnhandled, r)

	return r
}

// checkSections 对应 §1.2 的进入条件与 §6.3 的人判档：
// 没有 ❓；所有 🤖 都已被人逐节看过转 ✅。
func checkSections(d *Document, r *Report) {
	for _, s := range d.Sections() {
		switch s.Section.Status {
		case StatusPending:
			r.Blocking = append(r.Blocking, Finding{
				Code: CodePendingSection, Section: s.Key,
				Message: fmt.Sprintf("%s 仍挂着未决问题（❓），必须先答或显式接受不答", s.Title),
			})
		case StatusInferred:
			r.Blocking = append(r.Blocking, Finding{
				Code: CodeUnconfirmed, Section: s.Key,
				Message: fmt.Sprintf("%s 还是 🤖 推断，需要人逐节过目确认", s.Title),
			})
		}
	}
}

func checkOneLiner(d *Document, r *Report) {
	body := strings.TrimSpace(d.OneLiner.Body)
	if body == "" {
		r.Blocking = append(r.Blocking, Finding{
			Code: CodeOneLinerEmpty, Section: "oneLiner",
			Message: "§1 一句话为空：写不进一句话说明还没想清楚",
		})
		return
	}
	if n := utf8.RuneCountInString(body); n > oneLinerMaxRunes {
		r.Blocking = append(r.Blocking, Finding{
			Code: CodeOneLinerTooLong, Section: "oneLiner",
			Message: fmt.Sprintf("§1 一句话 %d 字，上限 %d 字", n, oneLinerMaxRunes),
		})
	}
}

// checkCriteria 覆盖 §5.2 类别强制填空、§5.3 可执行性与禁用词、
// 以及 §7.2 反向确认必须有明确答复。
func checkCriteria(d *Document, r *Report) {
	seen := map[ACCategory]bool{}
	for _, c := range d.Criteria {
		seen[c.Category] = true

		if c.NA {
			if strings.TrimSpace(c.Reason) == "" {
				r.Blocking = append(r.Blocking, Finding{
					Code: CodeNAWithoutReason, Section: "acceptance",
					Message: fmt.Sprintf("%s 标了 N/A 但没写理由：显式优于沉默", c.ID),
				})
			}
			continue
		}

		if strings.TrimSpace(c.Then) == "" || strings.TrimSpace(c.Evidence) == "" || !c.Verify.Valid() {
			r.Blocking = append(r.Blocking, Finding{
				Code: CodeACIncomplete, Section: "acceptance",
				Message: fmt.Sprintf("%s 缺 Then/判据/验证方式之一：写不出断言的不是 AC，是愿望", c.ID),
			})
		}
		if c.Verify == VerifyManual && strings.TrimSpace(c.ManualSteps) == "" {
			r.Blocking = append(r.Blocking, Finding{
				Code: CodeManualNoSteps, Section: "acceptance",
				Message: fmt.Sprintf("%s 是人工走查但没写步骤，「走查过了」无从复核", c.ID),
			})
		}
		// 禁用词只在没有判据伴随时才算问题（docs/10 §5.3 的「除非旁边有
		// 数字或判据」）。
		if strings.TrimSpace(c.Evidence) == "" {
			text := c.Given + c.When + c.Then
			for _, w := range bannedWords {
				if strings.Contains(text, w) {
					r.Blocking = append(r.Blocking, Finding{
						Code: CodeBannedWord, Section: "acceptance",
						Message: fmt.Sprintf("%s 含形容词「%s」且无判据伴随", c.ID, w),
					})
					break
				}
			}
		}
		if c.ReverseConfirm != "" && c.ReverseAccepted == nil {
			r.Blocking = append(r.Blocking, Finding{
				Code: CodeReverseUnanswered, Section: "acceptance",
				Message: fmt.Sprintf("%s 的反向确认还没有明确的接受/不接受（隐含期望几乎全藏在这里）", c.ID),
			})
		}
	}

	for _, cat := range RequiredCategories(d.Type) {
		if !seen[cat] {
			r.Blocking = append(r.Blocking, Finding{
				Code: CodeCategoryMissing, Section: "acceptance",
				Message: fmt.Sprintf("AC 表缺「%s」这一行：要么填 AC，要么写 N/A + 理由", cat),
			})
		}
	}
}
