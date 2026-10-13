package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/github"
	"github.com/zichuanwangcloud-gif/lathe/internal/runner"
)

const ghSecretA, ghSecretB = "gh-secret-user-a", "gh-secret-user-b"

// fakeGitHubResolver：slug-a → 用户 1，slug-b → 用户 2，各自不同的签名密钥。
type fakeGitHubResolver struct{}

func (fakeGitHubResolver) ResolveGitHub(ctx context.Context, slug string) (*GitHubWebhookTarget, error) {
	switch slug {
	case "slug-a":
		return &GitHubWebhookTarget{OwnerID: 1, Secret: ghSecretA}, nil
	case "slug-b":
		return &GitHubWebhookTarget{OwnerID: 2, Secret: ghSecretB}, nil
	}
	return nil, errors.New("未知 slug")
}

type acceptCall struct {
	owner    int64
	ev       github.ReviewEvent
	delivery string
}

type fakeReviewIntake struct {
	calls []acceptCall
	out   runner.ReviewOutcome
	err   error
}

func (f *fakeReviewIntake) AcceptReview(ctx context.Context, owner int64, ev *github.ReviewEvent, delivery string) (runner.ReviewOutcome, error) {
	f.calls = append(f.calls, acceptCall{owner, *ev, delivery})
	if f.err != nil {
		return runner.ReviewOutcome{}, f.err
	}
	if f.out.Status == "" {
		return runner.ReviewOutcome{Status: runner.ReviewQueued, TaskID: 99}, nil
	}
	return f.out, nil
}

const ghReviewCommentBody = `{
  "action": "created",
  "comment": {"id": 777, "body": "这里需要判空", "path": "a.go", "line": 9,
              "user": {"login": "alice", "type": "User"}},
  "pull_request": {"number": 11, "user": {"login": "lathe-bot"}},
  "repository": {"full_name": "acme/demo"}
}`

type ghReq struct {
	slug, secret, event, delivery, body string
	signature                           *string // nil = 用 secret 正常签
}

func postGitHub(t *testing.T, h *GitHubWebhook, r ghReq) *httptest.ResponseRecorder {
	t.Helper()
	if r.event == "" {
		r.event = github.EventReviewComment
	}
	if r.body == "" {
		r.body = ghReviewCommentBody
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github/"+r.slug, strings.NewReader(r.body))
	req.SetPathValue("slug", r.slug)
	if r.signature != nil {
		req.Header.Set(github.HeaderSignature, *r.signature)
	} else {
		mac := hmac.New(sha256.New, []byte(r.secret))
		mac.Write([]byte(r.body))
		req.Header.Set(github.HeaderSignature, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	req.Header.Set(github.HeaderEvent, r.event)
	if r.delivery != "-" {
		d := r.delivery
		if d == "" {
			d = "gh-deliv-1"
		}
		req.Header.Set(github.HeaderDelivery, d)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func newGitHubHandler(c *fakeClaimer, in *fakeReviewIntake) *GitHubWebhook {
	return &GitHubWebhook{Resolver: fakeGitHubResolver{}, Deliveries: c, Reviews: in}
}

func respStatus(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	s, _ := body["status"].(string)
	return s
}

// A2 验收 1：合法签名过、篡改签名拒、重放幂等。
func TestGitHubWebhookSignatureAndReplay(t *testing.T) {
	bad := "sha256=" + strings.Repeat("0", 64)
	empty := ""
	cases := []struct {
		name       string
		req        ghReq
		wantCode   int
		wantAccept bool
	}{
		{"合法签名通过并回流", ghReq{slug: "slug-a", secret: ghSecretA}, http.StatusOK, true},
		{"篡改签名拒绝", ghReq{slug: "slug-a", signature: &bad}, http.StatusUnauthorized, false},
		{"缺签名拒绝", ghReq{slug: "slug-a", signature: &empty}, http.StatusUnauthorized, false},
		{"用 B 的密钥签、投到 A 的地址：拒绝（密钥按用户校验）", ghReq{slug: "slug-a", secret: ghSecretB}, http.StatusUnauthorized, false},
		{"篡改正文（签名对不上）拒绝", ghReq{slug: "slug-a", secret: ghSecretA, body: strings.Replace(ghReviewCommentBody, "判空", "删库", 1),
			signature: func() *string {
				mac := hmac.New(sha256.New, []byte(ghSecretA))
				mac.Write([]byte(ghReviewCommentBody))
				s := "sha256=" + hex.EncodeToString(mac.Sum(nil))
				return &s
			}()}, http.StatusUnauthorized, false},
		{"未知 slug 404", ghReq{slug: "nope", secret: ghSecretA}, http.StatusNotFound, false},
		{"缺投递 ID 400", ghReq{slug: "slug-a", secret: ghSecretA, delivery: "-"}, http.StatusBadRequest, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := &fakeReviewIntake{}
			rec := postGitHub(t, newGitHubHandler(newFakeClaimer(), in), tc.req)
			if rec.Code != tc.wantCode {
				t.Fatalf("code = %d，want %d（body %s）", rec.Code, tc.wantCode, rec.Body)
			}
			if got := len(in.calls) == 1; got != tc.wantAccept {
				t.Errorf("回流调用 %d 次，want accept=%v", len(in.calls), tc.wantAccept)
			}
		})
	}

	t.Run("同一投递重放：只处理一次，第二次报 duplicate", func(t *testing.T) {
		claimer, in := newFakeClaimer(), &fakeReviewIntake{}
		h := newGitHubHandler(claimer, in)
		first := postGitHub(t, h, ghReq{slug: "slug-a", secret: ghSecretA, delivery: "dup-1"})
		second := postGitHub(t, h, ghReq{slug: "slug-a", secret: ghSecretA, delivery: "dup-1"})
		if first.Code != http.StatusOK || second.Code != http.StatusOK {
			t.Fatalf("codes = %d/%d，重放应同样确认 200（否则 GitHub 会一直重投）", first.Code, second.Code)
		}
		if len(in.calls) != 1 {
			t.Errorf("重放导致回流调用 %d 次，want 1", len(in.calls))
		}
		if s := respStatus(t, second); s != "duplicate" {
			t.Errorf("第二次 status = %q，want duplicate", s)
		}
		// 去重键带来源前缀：与 Linear 共用一张 webhook_deliveries 表，不能互相撞
		if !claimer.claimed["github:dup-1"] {
			t.Errorf("投递应以 github: 前缀登记，claimed = %v", claimer.claimed)
		}
	})
}

// 路由：事件按回调地址归到对应用户（跨用户隔离的入口一侧）。
func TestGitHubWebhookRoutesToSlugOwner(t *testing.T) {
	in := &fakeReviewIntake{}
	h := newGitHubHandler(newFakeClaimer(), in)
	postGitHub(t, h, ghReq{slug: "slug-a", secret: ghSecretA, delivery: "r-a"})
	postGitHub(t, h, ghReq{slug: "slug-b", secret: ghSecretB, delivery: "r-b"})
	if len(in.calls) != 2 || in.calls[0].owner != 1 || in.calls[1].owner != 2 {
		t.Fatalf("应按 slug 路由到用户 1、2，得到 %+v", in.calls)
	}
	ev := in.calls[0].ev
	if ev.Repo != "acme/demo" || ev.PRNumber != 11 || ev.Key != "inline:777" || ev.Body != "这里需要判空" {
		t.Errorf("交给 intake 的事件不符: %+v", ev)
	}
	if in.calls[0].delivery != "r-a" {
		t.Errorf("delivery = %q，want r-a", in.calls[0].delivery)
	}
}

// 不该回流的事件：安全忽略（200），不调 intake，但投递要记为已处理。
func TestGitHubWebhookIgnoresNonActionable(t *testing.T) {
	selfBody := strings.Replace(ghReviewCommentBody, `"login": "alice"`, `"login": "lathe-bot"`, 1)
	editBody := strings.Replace(ghReviewCommentBody, `"action": "created"`, `"action": "edited"`, 1)
	approveBody := `{"action":"submitted","review":{"id":5,"body":"LGTM","state":"approved","user":{"login":"bob"}},
		"pull_request":{"number":11,"user":{"login":"lathe-bot"}},"repository":{"full_name":"acme/demo"}}`

	cases := []struct {
		name, event, body string
	}{
		{"bot 自己（PR 作者）的评论", github.EventReviewComment, selfBody},
		{"编辑评论", github.EventReviewComment, editBody},
		{"approve", github.EventReview, approveBody},
		{"不处理的事件类型", "push", `{"ref":"refs/heads/main"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claimer, in := newFakeClaimer(), &fakeReviewIntake{}
			rec := postGitHub(t, newGitHubHandler(claimer, in),
				ghReq{slug: "slug-a", secret: ghSecretA, event: tc.event, body: tc.body, delivery: "ig"})
			if rec.Code != http.StatusOK {
				t.Fatalf("code = %d，want 200（body %s）", rec.Code, rec.Body)
			}
			if s := respStatus(t, rec); s != "ignored" {
				t.Errorf("status = %q，want ignored", s)
			}
			if len(in.calls) != 0 {
				t.Errorf("不应调用 intake，实际 %d 次", len(in.calls))
			}
		})
	}
}

// ping：建 webhook 时 GitHub 的连通性探测。验签通过即回 pong，
// 让人在 GitHub 界面上第一时间看到「密钥配对了」。
func TestGitHubWebhookPing(t *testing.T) {
	in := &fakeReviewIntake{}
	h := newGitHubHandler(newFakeClaimer(), in)
	body := `{"zen":"Keep it logically awesome.","hook_id":1}`

	ok := postGitHub(t, h, ghReq{slug: "slug-a", secret: ghSecretA, event: github.EventPing, body: body})
	if ok.Code != http.StatusOK || respStatus(t, ok) != "pong" {
		t.Errorf("ping 应回 200 pong，得到 %d %s", ok.Code, ok.Body)
	}
	bad := postGitHub(t, h, ghReq{slug: "slug-a", secret: ghSecretB, event: github.EventPing, body: body})
	if bad.Code != http.StatusUnauthorized {
		t.Errorf("密钥不对的 ping 应 401，得到 %d", bad.Code)
	}
}

// intake 返回的结论原样回给 GitHub，且落进投递记录。
func TestGitHubWebhookReportsIntakeOutcome(t *testing.T) {
	t.Run("忽略结论原样回给响应体", func(t *testing.T) {
		claimer := newFakeClaimer()
		in := &fakeReviewIntake{out: runner.ReviewOutcome{Status: runner.ReviewIgnored, Reason: "任务当前处于 merged"}}
		rec := postGitHub(t, newGitHubHandler(claimer, in), ghReq{slug: "slug-a", secret: ghSecretA, delivery: "o1"})
		if rec.Code != http.StatusOK || respStatus(t, rec) != "ignored" || !strings.Contains(rec.Body.String(), "merged") {
			t.Errorf("得到 %d %s", rec.Code, rec.Body)
		}
		if msg, done := claimer.finished["github:o1"]; !done || msg != "" {
			t.Errorf("忽略不是失败：投递应记为成功处理，得到 done=%v msg=%q", done, msg)
		}
	})
	t.Run("intake 出错：200 + 错误落投递记录（已登记去重，重投也不会再处理）", func(t *testing.T) {
		claimer := newFakeClaimer()
		in := &fakeReviewIntake{err: errors.New("数据库抖了一下")}
		rec := postGitHub(t, newGitHubHandler(claimer, in), ghReq{slug: "slug-a", secret: ghSecretA, delivery: "o2"})
		if rec.Code != http.StatusOK || respStatus(t, rec) != "error" {
			t.Errorf("得到 %d %s", rec.Code, rec.Body)
		}
		if msg := claimer.finished["github:o2"]; !strings.Contains(msg, "数据库抖了一下") {
			t.Errorf("错误应落投递记录，得到 %q", msg)
		}
	})
	t.Run("登记投递失败：5xx 让 GitHub 重投", func(t *testing.T) {
		claimer := newFakeClaimer()
		claimer.err = errors.New("db down")
		in := &fakeReviewIntake{}
		rec := postGitHub(t, newGitHubHandler(claimer, in), ghReq{slug: "slug-a", secret: ghSecretA, delivery: "o3"})
		if rec.Code != http.StatusInternalServerError || len(in.calls) != 0 {
			t.Errorf("得到 %d，intake 调用 %d 次", rec.Code, len(in.calls))
		}
	})
}

func TestGitHubWebhookRejectsNonPost(t *testing.T) {
	h := newGitHubHandler(newFakeClaimer(), &fakeReviewIntake{})
	req := httptest.NewRequest(http.MethodGet, "/webhooks/github/slug-a", nil)
	req.SetPathValue("slug", "slug-a")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("code = %d，want 405", rec.Code)
	}
}
