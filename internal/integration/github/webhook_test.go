package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyWebhookSignature(t *testing.T) {
	const secret = "gh-webhook-secret"
	body := `{"action":"created"}`
	good := sign(secret, body)

	cases := []struct {
		name      string
		secret    string
		body      string
		signature string
		wantErr   bool
		wantBad   bool // 期望是 ErrBadSignature（而不是「未配置」类错误）
	}{
		{"合法签名通过", secret, body, good, false, false},
		{"签名十六进制大写也通过（GitHub 不保证大小写）", secret, body, "sha256=" + strings.ToUpper(strings.TrimPrefix(good, "sha256=")), false, false},
		{"篡改正文被拒", secret, body + " ", good, true, true},
		{"篡改签名被拒", secret, body, good[:len(good)-1] + "0", true, true},
		{"用别人的密钥签的被拒", "other-secret", body, good, true, true},
		{"缺签名头被拒", secret, body, "", true, true},
		{"只有旧版 sha1 前缀被拒（只认 X-Hub-Signature-256）", secret, body, "sha1=abcdef", true, true},
		{"只有 sha256= 前缀、摘要是空串：拒绝（空摘要可能撞上空正文的 HMAC）", secret, body, "sha256=", true, true},
		{"sha256= 前后带空白：按去掉空白后处理，不是合法摘要就拒", secret, body, "  sha256=  ", true, true},
		{"只有空白字符串：与缺签名头同判，拒绝", secret, body, "   ", true, true},
		{"没配密钥一律拒绝", "", body, good, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyWebhookSignature(tc.secret, []byte(tc.body), tc.signature)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v，wantErr = %v", err, tc.wantErr)
			}
			if tc.wantBad && !errors.Is(err, ErrBadSignature) {
				t.Errorf("err = %v，want ErrBadSignature", err)
			}
		})
	}
}

const reviewCommentPayload = `{
  "action": "created",
  "comment": {
    "id": 9001,
    "body": "这里的错误被吞掉了，至少记一条日志",
    "path": "internal/foo/bar.go",
    "line": null,
    "original_line": 42,
    "html_url": "https://github.com/acme/demo/pull/7#discussion_r9001",
    "user": {"login": "alice", "type": "User"},
    "author_association": "MEMBER"
  },
  "pull_request": {"number": 7, "user": {"login": "lathe-bot"}},
  "repository": {"full_name": "acme/demo"}
}`

const reviewPayload = `{
  "action": "submitted",
  "review": {
    "id": 501,
    "body": "整体可以，但测试没覆盖空输入",
    "state": "changes_requested",
    "html_url": "https://github.com/acme/demo/pull/7#pullrequestreview-501",
    "user": {"login": "bob", "type": "User"},
    "author_association": "COLLABORATOR"
  },
  "pull_request": {"number": 7, "user": {"login": "lathe-bot"}},
  "repository": {"full_name": "acme/demo"}
}`

func TestParseReviewWebhook(t *testing.T) {
	t.Run("行内评论事件提炼出定位与正文", func(t *testing.T) {
		ev, err := ParseReviewWebhook(EventReviewComment, []byte(reviewCommentPayload))
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		want := ReviewEvent{
			Event: EventReviewComment, Action: "created",
			Repo: "acme/demo", PRNumber: 7, PRAuthor: "lathe-bot",
			Key: "inline:9001", Author: "alice", AuthorType: "User",
			AuthorAssociation: "MEMBER",
			Body:              "这里的错误被吞掉了，至少记一条日志",
			Path:              "internal/foo/bar.go", Line: 42, // line 为 null 时退回 original_line
			URL: "https://github.com/acme/demo/pull/7#discussion_r9001",
		}
		if *ev != want {
			t.Errorf("got  %+v\nwant %+v", *ev, want)
		}
	})

	t.Run("review 提交事件提炼出总评与结论", func(t *testing.T) {
		ev, err := ParseReviewWebhook(EventReview, []byte(reviewPayload))
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if ev.Key != "review:501" || ev.Author != "bob" || ev.ReviewState != "changes_requested" ||
			ev.Body != "整体可以，但测试没覆盖空输入" || ev.PRNumber != 7 || ev.Repo != "acme/demo" {
			t.Errorf("解析结果不符: %+v", *ev)
		}
	})

	t.Run("不支持的事件类型报 ErrUnsupportedEvent", func(t *testing.T) {
		if _, err := ParseReviewWebhook("issues", []byte(`{}`)); !errors.Is(err, ErrUnsupportedEvent) {
			t.Errorf("err = %v，want ErrUnsupportedEvent", err)
		}
	})

	t.Run("缺仓库或 PR 号的载荷报错而不是产出零值事件", func(t *testing.T) {
		if _, err := ParseReviewWebhook(EventReviewComment, []byte(`{"action":"created","comment":{"id":1}}`)); err == nil {
			t.Error("缺 repository/pull_request 时应报错")
		}
	})

	t.Run("坏 JSON 报错", func(t *testing.T) {
		if _, err := ParseReviewWebhook(EventReview, []byte(`{`)); err == nil {
			t.Error("坏 JSON 应报错")
		}
	})

	// 事件类型是路由的第一道门：空串（头缺失）与未知取值都必须落到
	// ErrUnsupportedEvent（调用方据此回 200 ignored），而不是被当成
	// 某个已知事件继续往下走。
	t.Run("事件类型头缺失（空串）报 ErrUnsupportedEvent", func(t *testing.T) {
		if _, err := ParseReviewWebhook("", []byte(reviewCommentPayload)); !errors.Is(err, ErrUnsupportedEvent) {
			t.Errorf("err = %v，want ErrUnsupportedEvent", err)
		}
	})

	t.Run("未知事件类型报 ErrUnsupportedEvent（含大小写不同）", func(t *testing.T) {
		for _, et := range []string{"pull_request", "issues", "Pull_Request_Review_Comment", "ping"} {
			if _, err := ParseReviewWebhook(et, []byte(reviewCommentPayload)); !errors.Is(err, ErrUnsupportedEvent) {
				t.Errorf("事件类型 %q: err = %v，want ErrUnsupportedEvent", et, err)
			}
		}
	})

	// 事件类型合法但载荷残缺：必须报错，不能产出零值事件 —— 零值事件
	// 会带着空 Repo/PRNumber 一路走到 TasksByPR，在那里被当成「没有对应
	// 的任务」而静默丢掉，真正的问题（GitHub 换了载荷格式）就看不见了。
	t.Run("review 事件缺 review 对象报错而不是产出零值事件", func(t *testing.T) {
		body := []byte(`{"action":"submitted","pull_request":{"number":7,"user":{"login":"lathe-bot"}},"repository":{"full_name":"acme/demo"}}`)
		if _, err := ParseReviewWebhook(EventReview, body); err == nil {
			t.Error("缺 review 对象时应报错")
		}
	})

	t.Run("PR 号非法（0 或缺失）报错", func(t *testing.T) {
		body := []byte(`{"action":"created","comment":{"id":1,"body":"x","user":{"login":"alice"}},` +
			`"pull_request":{"number":0,"user":{"login":"lathe-bot"}},"repository":{"full_name":"acme/demo"}}`)
		if _, err := ParseReviewWebhook(EventReviewComment, body); err == nil {
			t.Error("PR 号为 0 时应报错")
		}
	})

	t.Run("仓库名缺失报错", func(t *testing.T) {
		body := []byte(`{"action":"created","comment":{"id":1,"body":"x","user":{"login":"alice"}},` +
			`"pull_request":{"number":7,"user":{"login":"lathe-bot"}},"repository":{"full_name":""}}`)
		if _, err := ParseReviewWebhook(EventReviewComment, body); err == nil {
			t.Error("仓库名缺失时应报错")
		}
	})
}

func TestReviewEventActionable(t *testing.T) {
	base := func() ReviewEvent {
		return ReviewEvent{
			Event: EventReviewComment, Action: "created",
			Repo: "acme/demo", PRNumber: 7, PRAuthor: "lathe-bot",
			Key: "inline:1", Author: "alice", Body: "改一下",
			AuthorAssociation: "MEMBER",
		}
	}
	cases := []struct {
		name   string
		mutate func(*ReviewEvent)
		want   bool
	}{
		{"他人新建的行内评论要处理", func(*ReviewEvent) {}, true},
		{"编辑评论不触发（同一条意见不应再跑一轮）", func(e *ReviewEvent) { e.Action = "edited" }, false},
		{"删除评论不触发", func(e *ReviewEvent) { e.Action = "deleted" }, false},
		{"PR 作者（即 Lathe 开 PR 用的账号）自己的评论忽略", func(e *ReviewEvent) { e.Author = "lathe-bot" }, false},
		{"自身判定大小写不敏感", func(e *ReviewEvent) { e.Author = "Lathe-Bot" }, false},
		{"有仓库权限的自动评审 bot（如配了权限的 CodeRabbit）照收",
			func(e *ReviewEvent) {
				e.Author = "coderabbitai[bot]"
				e.AuthorType = "Bot"
				e.AuthorAssociation = "CONTRIBUTOR"
			}, true},
		{"空正文行内评论忽略", func(e *ReviewEvent) { e.Body = "  " }, false},
		{"要求修改的 review 即使无总评也触发", func(e *ReviewEvent) {
			e.Event, e.Action, e.Key, e.ReviewState, e.Body = EventReview, "submitted", "review:1", "changes_requested", ""
		}, true},
		{"带总评的普通 review 触发", func(e *ReviewEvent) {
			e.Event, e.Action, e.Key, e.ReviewState = EventReview, "submitted", "review:1", "commented"
		}, true},
		{"无总评的普通 review 忽略（行内评论各自另有事件）", func(e *ReviewEvent) {
			e.Event, e.Action, e.Key, e.ReviewState, e.Body = EventReview, "submitted", "review:1", "commented", ""
		}, false},
		{"approve 不触发返工（人已认可这份 diff）", func(e *ReviewEvent) {
			e.Event, e.Action, e.Key, e.ReviewState = EventReview, "submitted", "review:1", "approved"
		}, false},
		{"review 被撤销不触发", func(e *ReviewEvent) {
			e.Event, e.Action, e.Key, e.ReviewState = EventReview, "dismissed", "review:1", "dismissed"
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := base()
			tc.mutate(&ev)
			ok, reason := ev.Actionable()
			if ok != tc.want {
				t.Errorf("Actionable() = %v（%s），want %v", ok, reason, tc.want)
			}
			if !ok && reason == "" {
				t.Error("忽略时必须给出理由（落进投递记录与响应体）")
			}
		})
	}
}

func TestReviewFeedbackCollectsAllKinds(t *testing.T) {
	c := stubServer(t, map[string]http.HandlerFunc{
		"/repos/acme/demo/pulls/7": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"number":7,"state":"open","user":{"login":"lathe-bot"}}`))
		},
		"/repos/acme/demo/pulls/7/comments": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[{"id":11,"body":"行内意见","path":"a.go","line":3,"user":{"login":"alice"},"author_association":"MEMBER"}]`))
		},
		"/repos/acme/demo/issues/7/comments": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[{"id":22,"body":"顺手把 README 也改了","user":{"login":"bob"},"author_association":"COLLABORATOR"}]`))
		},
		"/repos/acme/demo/pulls/7/reviews": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[
				{"id":33,"body":"缺空输入的测试","state":"CHANGES_REQUESTED","user":{"login":"bob"},"author_association":"MEMBER"},
				{"id":34,"body":"","state":"COMMENTED","user":{"login":"alice"},"author_association":"MEMBER"},
				{"id":35,"body":"LGTM","state":"APPROVED","user":{"login":"carol"},"author_association":"MEMBER"}
			]`))
		},
	})

	fb, err := c.ReviewFeedback(context.Background(), "acme/demo", 7)
	if err != nil {
		t.Fatalf("ReviewFeedback 失败: %v", err)
	}
	if fb.PRAuthor != "lathe-bot" {
		t.Errorf("PRAuthor = %q，want lathe-bot", fb.PRAuthor)
	}
	got := map[string]string{}
	for _, cm := range fb.Comments {
		got[cm.Key] = cm.Body
	}
	want := map[string]string{
		"inline:11": "行内意见",
		"issue:22":  "顺手把 README 也改了",
		"review:33": "缺空输入的测试",
		// review:34 无正文、review:35 是 approve —— 都不是要处理的意见
	}
	if len(got) != len(want) {
		t.Fatalf("got %v，want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q，want %q", k, got[k], v)
		}
	}
}

// ---------------------------------------------------------------- 审计 B2

// B2（BLOCKER，安全）：可处理的判定必须按【评论者与仓库的关系】把关，而不
// 只是「不是 PR 作者」。
//
// 攻击面：公开仓库上任何 GitHub 账号都能对 PR 写评论（author_association
// 为 "NONE"）。修复前这些评论会被判为可处理，于是
//   - agent 带着【任务属主的凭据】在本机跑、推分支；
//   - 评论正文原样进 prompt —— 一条精心构造的评论就是对 agent 的提示注入；
//   - 消耗的是属主的令牌与算力（无授权消耗）。
func TestReviewEventActionableRequiresRepoRelationship(t *testing.T) {
	base := func() ReviewEvent {
		return ReviewEvent{
			Event: EventReviewComment, Action: "created",
			Repo: "acme/demo", PRNumber: 7, PRAuthor: "lathe-bot",
			Key: "inline:1", Author: "alice", Body: "改一下",
			AuthorAssociation: "MEMBER",
		}
	}
	cases := []struct {
		name        string
		association string
		want        bool
	}{
		{"OWNER 放行", "OWNER", true},
		{"MEMBER 放行", "MEMBER", true},
		{"COLLABORATOR 放行", "COLLABORATOR", true},
		{"CONTRIBUTOR 放行", "CONTRIBUTOR", true},
		{"小写也放行（GitHub 不保证大小写）", "member", true},
		{"NONE 拒绝：路人账号不得驱动二轮（公开仓库的核心攻击面）", "NONE", false},
		{"FIRST_TIME_CONTRIBUTOR 拒绝：首次贡献者尚未建立信任关系", "FIRST_TIME_CONTRIBUTOR", false},
		{"FIRST_TIMER 拒绝", "FIRST_TIMER", false},
		{"MANNEQUIN 拒绝", "MANNEQUIN", false},
		{"字段缺失按最保守处理（拒绝），不按「未知即放行」", "", false},
		{"未知取值拒绝（白名单式判据，新取值默认不可信）", "SOMETHING_NEW", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := base()
			ev.AuthorAssociation = tc.association
			ok, reason := ev.Actionable()
			if ok != tc.want {
				t.Errorf("author_association=%q 时 Actionable() = %v（%s），want %v",
					tc.association, ok, reason, tc.want)
			}
			if !ok && reason == "" {
				t.Error("忽略时必须给出理由（落进投递记录与响应体）")
			}
		})
	}

	t.Run("review 事件同口径把关", func(t *testing.T) {
		ev := base()
		ev.Event, ev.Action, ev.Key, ev.ReviewState = EventReview, "submitted", "review:1", "changes_requested"
		ev.AuthorAssociation = "NONE"
		if ok, _ := ev.Actionable(); ok {
			t.Error("review 总评也必须按 author_association 把关，NONE 不得放行")
		}
		ev.AuthorAssociation = "MEMBER"
		if ok, reason := ev.Actionable(); !ok {
			t.Errorf("MEMBER 的 review 总评应放行，得到 %v（%s）", ok, reason)
		}
	})
}

// ReviewFeedback 是二轮派发时拉取全部意见的那条路：它不能把无关账号的
// 意见也带进 prompt（那等于绕开了入口的同一道防线的第二处出口）。
func TestReviewFeedbackFiltersByAuthorAssociation(t *testing.T) {
	c := stubServer(t, map[string]http.HandlerFunc{
		"/repos/acme/demo/pulls/7": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"number":7,"state":"open","user":{"login":"lathe-bot"}}`))
		},
		"/repos/acme/demo/pulls/7/comments": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[
				{"id":11,"body":"行内意见","path":"a.go","line":3,"user":{"login":"alice"},"author_association":"MEMBER"},
				{"id":12,"body":"路过留一句","path":"a.go","line":4,"user":{"login":"randomer"},"author_association":"NONE"}
			]`))
		},
		"/repos/acme/demo/issues/7/comments": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[
				{"id":22,"body":"顺手把 README 也改了","user":{"login":"bob"},"author_association":"COLLABORATOR"},
				{"id":23,"body":"未授权账号的评论","user":{"login":"randomer"},"author_association":"NONE"},
				{"id":24,"body":"字段缺失的评论","user":{"login":"randomer"}}
			]`))
		},
		"/repos/acme/demo/pulls/7/reviews": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[
				{"id":33,"body":"缺空输入的测试","state":"CHANGES_REQUESTED","user":{"login":"bob"},"author_association":"MEMBER"},
				{"id":36,"body":"首次贡献者的意见","state":"CHANGES_REQUESTED","user":{"login":"newbie"},"author_association":"FIRST_TIME_CONTRIBUTOR"}
			]`))
		},
	})

	fb, err := c.ReviewFeedback(context.Background(), "acme/demo", 7)
	if err != nil {
		t.Fatalf("ReviewFeedback 失败: %v", err)
	}
	got := map[string]bool{}
	for _, cm := range fb.Comments {
		got[cm.Key] = true
	}
	for _, want := range []string{"inline:11", "issue:22", "review:33"} {
		if !got[want] {
			t.Errorf("有仓库关系的意见 %s 应被收集，实际 %v", want, got)
		}
	}
	for _, unwanted := range []string{"inline:12", "issue:23", "issue:24", "review:36"} {
		if got[unwanted] {
			t.Errorf("无仓库关系/字段缺失的意见 %s 不该进 prompt，实际 %v", unwanted, got)
		}
	}
}
