package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v66/github"
)

// webhook.go 是 PR 评论回流（docs/02-design.md §7）的 GitHub 侧：
// 验签、把 webhook 载荷提炼成平台无关的 ReviewEvent、判定哪些事件
// 是「需要 agent 处理的 review 意见」，以及派发二轮时拉取全部待处理意见。
//
// 选型是「每用户配置仓库 webhook + 每用户签名密钥」而不是 GitHub App：
// 与 Linear webhook 的按用户路由先例同形（/webhooks/github/{slug}），
// 凭据仍是现有的个人令牌，不引入 App 私钥、JWT 签名与 installation
// token 生命周期。代价见 ReviewEvent.Actionable 关于「自身」判定的说明。

// Webhook HTTP 头。
const (
	// HeaderSignature 携带 HMAC-SHA256 签名，形如 "sha256=<hex>"。
	// 只认 256 版：旧的 X-Hub-Signature 是 SHA-1，不接受降级。
	HeaderSignature = "X-Hub-Signature-256"
	// HeaderDelivery 是本次投递的 GUID，用于幂等去重。
	HeaderDelivery = "X-GitHub-Delivery"
	// HeaderEvent 是事件类型。
	HeaderEvent = "X-GitHub-Event"
)

// 关心的事件类型。
const (
	EventReviewComment = "pull_request_review_comment"
	EventReview        = "pull_request_review"
	// EventPing 是建 webhook 时 GitHub 发的连通性探测。
	EventPing = "ping"
)

// ErrBadSignature 表示签名不匹配或缺失。
var ErrBadSignature = errors.New("github: webhook 签名校验失败")

// ErrUnsupportedEvent 表示事件类型不在处理范围内（不是错误，调用方应忽略）。
var ErrUnsupportedEvent = errors.New("github: 不处理的 webhook 事件类型")

// VerifyWebhookSignature 校验 X-Hub-Signature-256。
//
// hmac.Equal 常数时间比较，避免时序侧信道。十六进制按小写比较：
// GitHub 文档给的是小写，但这不是协议保证，不该因为大小写把合法投递拒掉。
func VerifyWebhookSignature(secret string, body []byte, signature string) error {
	if secret == "" {
		return fmt.Errorf("github: 未配置 webhook secret")
	}
	hexSig, ok := strings.CutPrefix(strings.TrimSpace(signature), "sha256=")
	if !ok || hexSig == "" {
		return fmt.Errorf("%w（缺少签名头或不是 sha256= 格式）", ErrBadSignature)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expected), []byte(strings.ToLower(hexSig))) {
		return ErrBadSignature
	}
	return nil
}

// ReviewEvent 是一次 PR review 反馈 webhook 提炼后的结果。
//
// 刻意是可比较的纯值类型（无指针/切片）：测试可直接整体比较，
// 落事件 payload 时也不会意外带出载荷里的其它字段。
type ReviewEvent struct {
	Event  string // EventReviewComment / EventReview
	Action string // created / edited / deleted / submitted / dismissed

	Repo     string // owner/name
	PRNumber int
	// PRAuthor 是开 PR 的账号 —— Lathe 用任务属主的令牌开 PR，所以它就是
	// Lathe 在这个 PR 上的身份。
	PRAuthor string

	// Key 是意见的稳定标识（"inline:<id>" / "review:<id>"），幂等去重与
	// 「哪些意见已处理过」都按它记账。带前缀是因为行内评论、普通评论、
	// review 在 GitHub 上是三个独立的 ID 序列，数值可能相撞。
	Key    string
	Author string
	// AuthorAssociation 是评论者与仓库的关系（GitHub 的 author_association）：
	// OWNER / MEMBER / COLLABORATOR / CONTRIBUTOR / FIRST_TIME_CONTRIBUTOR /
	// FIRST_TIMER / MANNEQUIN / NONE。空串表示载荷里没有这个字段。
	//
	// 为什么必须有（A2 审计 B2）：只看 Author != PRAuthor 是不够的。公开仓库
	// 上任何 GitHub 账号都能对 PR 写评论、提交 review，此时
	//   - 该评论会驱动一次二轮：agent 带着【任务属主的凭据】在本机跑、推分支；
	//   - 评论正文原样进 prompt —— 一条精心构造的评论就是对 agent 的提示注入；
	//   - 消耗的是属主的令牌与算力（无授权消耗）。
	// 所以准入必须按「与仓库的关系」而不是「是不是 PR 作者」来判。
	AuthorAssociation string
	AuthorType        string // User / Bot
	Body              string
	Path              string // 仅行内评论
	Line              int    // 仅行内评论
	URL               string
	// ReviewState 仅 pull_request_review：commented / changes_requested /
	// approved / dismissed（统一转小写）。
	ReviewState string
}

// 信任名单：有这些 author_association 的评论者才有资格驱动一次二轮。
//
// 判据刻意写成白名单（而不是「排除 NONE」）：以后 GitHub 新增取值时，
// 默认落在「不可信」这一侧，要放行才必须显式加进来。少一个忘改的地方，
// 就是少一条注入通道。
//
// 为什么是这四个（A2 审计 B2）：驱动二轮意味着评论正文会进 agent 的
// prompt，而 agent 带着【任务属主的凭据】在本机跑、推分支 —— 一条
// 精心构造的评论就是一次提示注入 + 无授权消耗属主的令牌与算力。
// OWNER/MEMBER/COLLABORATOR 是对仓库有写权限的人；CONTRIBUTOR 是已经
// 在这个仓库贡献过（有过合并）的人。这四者的意见值得让 agent 去处理。
//
// FIRST_TIME_CONTRIBUTOR / FIRST_TIMER / MANNEQUIN / NONE 一律拒绝：
// 公开仓库上任何 GitHub 账号都能对 PR 写评论，未建立关系的账号不得
// 驱动带凭据的 agent。属主要采纳这类意见，可以自己转发或让协作者转述。
var trustedAssociations = map[string]bool{
	"OWNER":        true,
	"MEMBER":       true,
	"COLLABORATOR": true,
	"CONTRIBUTOR":  true,
}

// TrustedAssociation 报告 author_association 是否在信任名单里。
//
// 空串（字段缺失）与未知取值都是 false —— 白名单式判据，未知即不可信。
// 入口（Actionable）与二轮派发时的全量拉取（ReviewFeedback）共用它，
// 两处口径必须一致：任一处漏掉，另一处就是绕过守卫的旁路。
func TrustedAssociation(a string) bool {
	return trustedAssociations[strings.ToUpper(strings.TrimSpace(a))]
}

// Actionable 判定这个事件是否是一条需要 agent 处理的 review 意见；
// 不是时给出人读理由（落进投递记录与响应体）。
//
// 两道守卫，缺一不可：
//
//  1. 作者关系（author_association 白名单）—— 谁能驱动二轮；
//  2. 「自身」判定（Author == PRAuthor）—— 评论者就是开 PR 的账号，
//     挡住「Lathe 自己的动作触发 Lathe」这类回环。
//
// 第 2 条的已知代价：个人令牌模型下 Lathe 的 GitHub 身份就是任务属主
// 本人，于是属主在自己（由 Lathe 开出的）PR 上留的评论也会被当作自身而
// 忽略 —— 意见需要由他人（或其它 bot）给出。这与 GitHub 不允许 PR 作者
// approve/request changes 自己的 PR 是一致的协作形态；换 GitHub App
// （独立的 bot 身份）才能消除这个代价。
//
// bot 的处理：「自身」判定已经挡住了 Lathe 自己那个账号（它就是 PR 作者），
// 所以这里不需要额外的 AuthorType == "Bot" 排除；剩余 bot 一律走同一道
// author_association 白名单。CodeRabbit 这类自动评审在配置了仓库权限时
// 会带 MEMBER/COLLABORATOR（或 CONTRIBUTOR），照收 —— 这正是 brief 期望
// 的取舍（自动评审的意见值得回流）。未配置权限的 bot 会带 NONE 而被拒绝，
// 那时让属主按需给它仓库权限，而不是在这里对它开口子。
func (e *ReviewEvent) Actionable() (bool, string) {
	if e == nil {
		return false, "空事件"
	}
	if e.Author != "" && strings.EqualFold(e.Author, e.PRAuthor) {
		return false, "评论来自 PR 作者本身（Lathe 开 PR 所用的账号），不回流给自己"
	}
	// 字段缺失（空串）与未知取值都按最保守处理：拒绝。载荷里没有
	// author_association 只说明上游异常或格式变化，不能据此认为可信。
	if !TrustedAssociation(e.AuthorAssociation) {
		who := e.Author
		if who == "" {
			who = "（未知账号）"
		}
		return false, "评论者 " + who + " 与仓库没有足够的合作关系（author_association=" +
			describeAssociation(e.AuthorAssociation) + "），不驱动二轮：二轮会以任务属主的凭据运行 agent，" +
			"并由评论正文进入 prompt，无授权账号不得触发"
	}

	switch e.Event {
	case EventReviewComment:
		if e.Action != "created" {
			return false, "行内评论动作 " + e.Action + " 不触发二轮（只处理新建）"
		}
		if strings.TrimSpace(e.Body) == "" {
			return false, "空正文的行内评论"
		}
		return true, ""

	case EventReview:
		if e.Action != "submitted" {
			return false, "review 动作 " + e.Action + " 不触发二轮（只处理提交）"
		}
		switch e.ReviewState {
		case "changes_requested":
			// 无总评也触发：结论本身就是意见，具体条目在行内评论里，
			// 二轮派发时会全部拉取。
			return true, ""
		case "commented":
			if strings.TrimSpace(e.Body) == "" {
				return false, "无总评的普通 review（行内评论各自另有事件）"
			}
			return true, ""
		case "approved":
			// 人已经认可了这份 diff，此时返工会让「被 approve 的」和
			// 「最终合并的」不是同一份东西。
			return false, "approve 不触发返工"
		default:
			return false, "review 结论 " + e.ReviewState + " 不触发二轮"
		}
	}
	return false, "不处理的事件类型 " + e.Event
}

// describeAssociation 把 author_association 的原值渲染成人读说明，
// 空值显式写成「缺失」而不是一个空引号 —— 拒绝理由是要给人看的。
func describeAssociation(a string) string {
	if strings.TrimSpace(a) == "" {
		return "字段缺失"
	}
	return a
}

// 载荷里关心的字段。
type webhookUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type webhookPayload struct {
	Action  string `json:"action"`
	Comment *struct {
		ID           int64       `json:"id"`
		Body         string      `json:"body"`
		Path         string      `json:"path"`
		Line         *int        `json:"line"`
		OriginalLine *int        `json:"original_line"`
		HTMLURL      string      `json:"html_url"`
		User         webhookUser `json:"user"`
		// AuthorAssociation 见 ReviewEvent 的同名字段（A2 审计 B2）。
		AuthorAssociation string `json:"author_association"`
	} `json:"comment"`
	Review *struct {
		ID      int64       `json:"id"`
		Body    string      `json:"body"`
		State   string      `json:"state"`
		HTMLURL string      `json:"html_url"`
		User    webhookUser `json:"user"`
		// AuthorAssociation 见 ReviewEvent 的同名字段（A2 审计 B2）。
		AuthorAssociation string `json:"author_association"`
	} `json:"review"`
	PullRequest *struct {
		Number int         `json:"number"`
		User   webhookUser `json:"user"`
	} `json:"pull_request"`
	Repository *struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// ParseReviewWebhook 把（已验签的）载荷解析成 ReviewEvent。
// eventType 来自 X-GitHub-Event 头；不在处理范围内的返回 ErrUnsupportedEvent。
func ParseReviewWebhook(eventType string, body []byte) (*ReviewEvent, error) {
	if eventType != EventReviewComment && eventType != EventReview {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedEvent, eventType)
	}

	var p webhookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("github: 解析 webhook 载荷失败: %w", err)
	}
	if p.Repository == nil || p.Repository.FullName == "" || p.PullRequest == nil || p.PullRequest.Number <= 0 {
		return nil, fmt.Errorf("github: webhook 载荷缺少仓库或 PR 编号")
	}

	ev := &ReviewEvent{
		Event:    eventType,
		Action:   p.Action,
		Repo:     p.Repository.FullName,
		PRNumber: p.PullRequest.Number,
		PRAuthor: p.PullRequest.User.Login,
	}

	switch eventType {
	case EventReviewComment:
		if p.Comment == nil || p.Comment.ID == 0 {
			return nil, fmt.Errorf("github: 行内评论事件缺少 comment")
		}
		c := p.Comment
		ev.Key = fmt.Sprintf("inline:%d", c.ID)
		ev.Author, ev.AuthorType = c.User.Login, c.User.Type
		ev.AuthorAssociation = strings.ToUpper(c.AuthorAssociation)
		ev.Body, ev.Path, ev.URL = c.Body, c.Path, c.HTMLURL
		// line 是评论在当前 diff 里的行号；评论落在已过时的 diff 上时为
		// null，此时退回 original_line —— 给 agent 一个近似定位总比没有强。
		switch {
		case c.Line != nil:
			ev.Line = *c.Line
		case c.OriginalLine != nil:
			ev.Line = *c.OriginalLine
		}
	case EventReview:
		if p.Review == nil || p.Review.ID == 0 {
			return nil, fmt.Errorf("github: review 事件缺少 review")
		}
		r := p.Review
		ev.Key = fmt.Sprintf("review:%d", r.ID)
		ev.Author, ev.AuthorType = r.User.Login, r.User.Type
		ev.AuthorAssociation = strings.ToUpper(r.AuthorAssociation)
		ev.Body, ev.URL = r.Body, r.HTMLURL
		ev.ReviewState = strings.ToLower(r.State)
	}
	return ev, nil
}

// ReviewFeedback 是一个 PR 上全部可能需要处理的意见。
type ReviewFeedback struct {
	// PRAuthor 是开 PR 的账号（Lathe 在该 PR 上的身份），调用方据此排除自身。
	PRAuthor string
	Comments []ReviewComment
}

// maxFeedbackPages 是每类意见最多翻的页数（每页 100 条）。一个 PR 上
// 千条以上的意见已不是自动化二轮能处理的规模，截断比无界翻页更安全。
const maxFeedbackPages = 10

// ReviewFeedback 拉取 PR 上的全部意见：行内评论、普通评论、带正文的
// review 总评（approve 与空正文的 review 不算意见）。
//
// 与 ReviewComments 的区别：多了 review 总评（「要求修改」的结论常常
// 只写在总评里）与 PR 作者，且会翻页。二轮派发时由 runner 消费，
// 排除自身与已处理过的意见后交给 agent。
func (c *Client) ReviewFeedback(ctx context.Context, providerRepo string, number int) (*ReviewFeedback, error) {
	owner, name, err := SplitRepo(providerRepo)
	if err != nil {
		return nil, err
	}

	pr, _, err := c.api.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		return nil, fmt.Errorf("github: 读取 PR #%d 失败: %w", number, err)
	}
	fb := &ReviewFeedback{PRAuthor: pr.GetUser().GetLogin()}

	for page, n := 1, 0; page != 0 && n < maxFeedbackPages; n++ {
		list, resp, err := c.api.PullRequests.ListComments(ctx, owner, name, number,
			&gh.PullRequestListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100, Page: page}})
		if err != nil {
			return nil, fmt.Errorf("github: 读取 PR #%d 行内评论失败: %w", number, err)
		}
		for _, cm := range list {
			if !TrustedAssociation(cm.GetAuthorAssociation()) {
				continue
			}
			fb.Comments = append(fb.Comments, ReviewComment{
				ID: cm.GetID(), Key: fmt.Sprintf("inline:%d", cm.GetID()),
				Author: cm.GetUser().GetLogin(), Body: cm.GetBody(),
				Path: cm.GetPath(), Line: cm.GetLine(), URL: cm.GetHTMLURL(),
				AuthorAssociation: strings.ToUpper(cm.GetAuthorAssociation()),
			})
		}
		page = resp.NextPage
	}

	for page, n := 1, 0; page != 0 && n < maxFeedbackPages; n++ {
		list, resp, err := c.api.Issues.ListComments(ctx, owner, name, number,
			&gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100, Page: page}})
		if err != nil {
			return nil, fmt.Errorf("github: 读取 PR #%d 评论失败: %w", number, err)
		}
		for _, cm := range list {
			if !TrustedAssociation(cm.GetAuthorAssociation()) {
				continue
			}
			fb.Comments = append(fb.Comments, ReviewComment{
				ID: cm.GetID(), Key: fmt.Sprintf("issue:%d", cm.GetID()),
				Author: cm.GetUser().GetLogin(), Body: cm.GetBody(), URL: cm.GetHTMLURL(),
				AuthorAssociation: strings.ToUpper(cm.GetAuthorAssociation()),
			})
		}
		page = resp.NextPage
	}

	for page, n := 1, 0; page != 0 && n < maxFeedbackPages; n++ {
		list, resp, err := c.api.PullRequests.ListReviews(ctx, owner, name, number,
			&gh.ListOptions{PerPage: 100, Page: page})
		if err != nil {
			return nil, fmt.Errorf("github: 读取 PR #%d review 失败: %w", number, err)
		}
		for _, rv := range list {
			state := strings.ToUpper(rv.GetState())
			if strings.TrimSpace(rv.GetBody()) == "" || state == "APPROVED" || state == "DISMISSED" {
				continue
			}
			if !TrustedAssociation(rv.GetAuthorAssociation()) {
				continue
			}
			fb.Comments = append(fb.Comments, ReviewComment{
				ID: rv.GetID(), Key: fmt.Sprintf("review:%d", rv.GetID()),
				Author: rv.GetUser().GetLogin(), Body: rv.GetBody(), URL: rv.GetHTMLURL(),
				AuthorAssociation: strings.ToUpper(rv.GetAuthorAssociation()),
			})
		}
		page = resp.NextPage
	}
	return fb, nil
}
