package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/integration/github"
	"github.com/zichuanwangcloud-gif/lathe/internal/runner"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
	"github.com/zichuanwangcloud-gif/lathe/internal/vcs"
	"github.com/zichuanwangcloud-gif/lathe/internal/vcs/gitea"
	"github.com/zichuanwangcloud-gif/lathe/internal/vcs/gitlab"
)

const (
	glSecretA = "gl-secret-user-a-0001"
	gtSecretA = "gt-secret-user-a-0001"
)

// platformStub 是 GitLab/Gitea 成员（协作者）查询的假服务器。
type platformStub struct {
	srv   *httptest.Server
	reply func(w http.ResponseWriter, r *http.Request)
	hits  int
}

func newPlatformStub(t *testing.T, reply func(w http.ResponseWriter, r *http.Request)) *platformStub {
	s := &platformStub{reply: reply}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits++
		s.reply(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func replyJSON(v any) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

func replyCode(code int) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) { http.Error(w, `{"message":"x"}`, code) }
}

// fakeVCSResolver：slug-a → 用户 1（GitLab/Gitea 各自的密钥与成员查询客户端）。
type fakeVCSResolver struct {
	owner     int64
	glMembers gitlab.MemberChecker
	gtCollab  gitea.Collaborators
}

func (f fakeVCSResolver) ResolveVCSWebhook(ctx context.Context, p vcs.Provider, slug string) (*VCSWebhookTarget, error) {
	if slug != "slug-a" {
		return nil, errors.New("未知 slug")
	}
	owner := f.owner
	if owner == 0 {
		owner = 1
	}
	switch p {
	case vcs.ProviderGitLab:
		return &VCSWebhookTarget{OwnerID: owner, Secret: glSecretA, GitLab: f.glMembers}, nil
	case vcs.ProviderGitea:
		return &VCSWebhookTarget{OwnerID: owner, Secret: gtSecretA, Gitea: f.gtCollab}, nil
	}
	return nil, errors.New("未知平台")
}

func glMembersAt(t *testing.T, s *platformStub) gitlab.MemberChecker {
	c, err := gitlab.New(s.srv.URL, "glpat-member-reader")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func gtCollabAt(t *testing.T, s *platformStub) gitea.Collaborators {
	c, err := gitea.New(s.srv.URL, "gitea-member-reader")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func gitlabNoteBody(noteID, authorID, mrAuthorID int64) string {
	return fmt.Sprintf(`{"object_kind":"note","event_type":"note",
 "user":{"id":%d,"username":"alice"},
 "project":{"id":1,"path_with_namespace":"group/sub/demo"},
 "object_attributes":{"id":%d,"note":"这里需要判空","%s_type":"MergeRequest","author_id":%d,
   "url":"https://gitlab.example.com/group/sub/demo/-/merge_requests/5#note_%d","updated_at":"2026-11-11 10:00:00 UTC"},
 "merge_request":{"id":9005,"iid":5,"author_id":%d}}`, authorID, noteID, "note"+"able", authorID, noteID, mrAuthorID) // GitLab 载荷的真实字段名
}

type glReq struct {
	slug, token, uuid, event, body string
	noToken                        bool
}

func postGitLab(t *testing.T, h *GitLabWebhook, r glReq) *httptest.ResponseRecorder {
	t.Helper()
	if r.body == "" {
		r.body = gitlabNoteBody(701, 2, 1)
	}
	if r.event == "" {
		r.event = gitlab.EventNote
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab/"+r.slug, strings.NewReader(r.body))
	req.SetPathValue("slug", r.slug)
	if !r.noToken {
		tok := r.token
		if tok == "" {
			tok = glSecretA
		}
		req.Header.Set(gitlab.HeaderToken, tok)
	}
	req.Header.Set(gitlab.HeaderEvent, r.event)
	if r.uuid != "-" {
		u := r.uuid
		if u == "" {
			u = "gl-uuid-1"
		}
		req.Header.Set(gitlab.HeaderEventUUID, u)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func giteaCommentBody(action string, isPull bool, commenter, prAuthor string) string {
	return fmt.Sprintf(`{"action":%q,
 "issue":{"id":55,"number":7,"title":"fix","user":{"login":%q},"pull_request":null},
 "comment":{"id":881,"body":"这里需要判空","html_url":"https://gitea.example.com/acme/demo/pulls/7#issuecomment-881","user":{"login":%q}},
 "repository":{"full_name":"acme/demo"},
 "sender":{"login":%q},
 "is_pull":%v}`, action, prAuthor, commenter, commenter, isPull)
}

const giteaReviewBody = `{"action":"reviewed","number":7,
 "pull_request":{"number":7,"user":{"login":"lathe-bot"}},
 "review":{"type":"pull_request_review_comment","content":"总评"},
 "repository":{"full_name":"acme/demo"},"sender":{"login":"alice"}}`

type gtReq struct {
	slug, event, eventType, delivery, body string
	signature                              *string
	noEventType                            bool
}

func postGitea(t *testing.T, h *GiteaWebhook, r gtReq) *httptest.ResponseRecorder {
	t.Helper()
	if r.body == "" {
		r.body = giteaCommentBody("created", true, "alice", "lathe-bot")
	}
	if r.event == "" {
		r.event = "issue_comment"
	}
	if r.eventType == "" && !r.noEventType {
		r.eventType = "pull_request_comment"
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/gitea/"+r.slug, strings.NewReader(r.body))
	req.SetPathValue("slug", r.slug)
	if r.signature != nil {
		req.Header.Set(gitea.HeaderSignature, *r.signature)
	} else {
		mac := hmac.New(sha256.New, []byte(gtSecretA))
		mac.Write([]byte(r.body))
		req.Header.Set(gitea.HeaderSignature, hex.EncodeToString(mac.Sum(nil)))
	}
	req.Header.Set(gitea.HeaderEvent, r.event)
	if r.eventType != "" {
		req.Header.Set(gitea.HeaderEventType, r.eventType)
	}
	d := r.delivery
	if d == "" {
		d = "gt-deliv-" + fmt.Sprint(time.Now().UnixNano())
	}
	req.Header.Set(gitea.HeaderDelivery, d)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func trustingGitLab(t *testing.T) *platformStub {
	return newPlatformStub(t, replyJSON(map[string]any{"id": 2, "access_level": 30}))
}

func trustingGitea(t *testing.T) *platformStub {
	return newPlatformStub(t, replyJSON(map[string]any{"permission": "write"}))
}

// ---------------------------------------------------------------- AC-14

func TestGitLabWebhookTokenMismatch401(t *testing.T) {
	in := &fakeReviewIntake{}
	h := &GitLabWebhook{Resolver: fakeVCSResolver{glMembers: glMembersAt(t, trustingGitLab(t))}, Deliveries: newFakeClaimer(), Reviews: in}
	if rec := postGitLab(t, h, glReq{slug: "slug-a", token: "wrong-token-xxxxxxxx"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("错误 token → %d，want 401", rec.Code)
	}
	if len(in.calls) != 0 {
		t.Error("验签失败不应回流")
	}
}

func TestGitLabWebhookMissingToken401(t *testing.T) {
	h := &GitLabWebhook{Resolver: fakeVCSResolver{}, Deliveries: newFakeClaimer(), Reviews: &fakeReviewIntake{}}
	if rec := postGitLab(t, h, glReq{slug: "slug-a", noToken: true}); rec.Code != http.StatusUnauthorized {
		t.Errorf("缺 token → %d，want 401", rec.Code)
	}
}

func TestGiteaWebhookSignature(t *testing.T) {
	bad, empty := strings.Repeat("0", 64), ""
	cases := []struct {
		name string
		sig  *string
		want int
	}{
		{"正确 HMAC", nil, http.StatusOK},
		{"错误 HMAC", &bad, http.StatusUnauthorized},
		{"缺签名头", &empty, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &GiteaWebhook{Resolver: fakeVCSResolver{gtCollab: gtCollabAt(t, trustingGitea(t))}, Deliveries: newFakeClaimer(), Reviews: &fakeReviewIntake{}}
			if rec := postGitea(t, h, gtReq{slug: "slug-a", signature: tc.sig}); rec.Code != tc.want {
				t.Errorf("code = %d，want %d（%s）", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestVCSWebhookUnknownSlug404(t *testing.T) {
	gl := &GitLabWebhook{Resolver: fakeVCSResolver{}, Deliveries: newFakeClaimer(), Reviews: &fakeReviewIntake{}}
	if rec := postGitLab(t, gl, glReq{slug: "nope"}); rec.Code != http.StatusNotFound {
		t.Errorf("gitlab 未知 slug → %d，want 404", rec.Code)
	}
	gt := &GiteaWebhook{Resolver: fakeVCSResolver{}, Deliveries: newFakeClaimer(), Reviews: &fakeReviewIntake{}}
	if rec := postGitea(t, gt, gtReq{slug: "nope"}); rec.Code != http.StatusNotFound {
		t.Errorf("gitea 未知 slug → %d，want 404", rec.Code)
	}
}

func TestVCSWebhookRejectsNonPost(t *testing.T) {
	for _, h := range []http.Handler{
		&GitLabWebhook{Resolver: fakeVCSResolver{}, Deliveries: newFakeClaimer(), Reviews: &fakeReviewIntake{}},
		&GiteaWebhook{Resolver: fakeVCSResolver{}, Deliveries: newFakeClaimer(), Reviews: &fakeReviewIntake{}},
	} {
		req := httptest.NewRequest(http.MethodGet, "/webhooks/x/slug-a", nil)
		req.SetPathValue("slug", "slug-a")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%T GET → %d，want 405", h, rec.Code)
		}
	}
}

// TestGiteaWebhookEventRecognition：按 Gitea 实际发送形态识别事件（v6）。
func TestGiteaWebhookEventRecognition(t *testing.T) {
	cases := []struct {
		name       string
		req        gtReq
		wantStatus string
		wantCalls  int
		wantReason string // 投递记录里应含
	}{
		{"① PR 普通评论（issue_comment + pull_request_comment + is_pull）", gtReq{}, "queued", 1, ""},
		{"② 同上但旧版本不带 X-Gitea-Event-Type", gtReq{noEventType: true}, "queued", 1, ""},
		{"③ 普通 issue 评论（is_pull=false）", gtReq{body: giteaCommentBody("created", false, "alice", "lathe-bot")}, "ignored", 0, "issue"},
		{"④ review 事件（X-Gitea-Event: pull_request_comment）", gtReq{event: "pull_request_comment", eventType: "pull_request_review_comment", body: giteaReviewBody}, "ignored", 0, "review 事件本轮不触发"},
		{"⑤ 编辑评论（action=edited）", gtReq{body: giteaCommentBody("edited", true, "alice", "lathe-bot")}, "ignored", 0, "edited"},
		{"⑥ 头与载荷不一致", gtReq{eventType: "issue_comment"}, "ignored", 0, "不一致"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := &fakeReviewIntake{}
			cl := newFakeClaimer()
			h := &GiteaWebhook{Resolver: fakeVCSResolver{gtCollab: gtCollabAt(t, trustingGitea(t))}, Deliveries: cl, Reviews: in}
			tc.req.slug = "slug-a"
			tc.req.delivery = "gt-rec-1"
			rec := postGitea(t, h, tc.req)
			if rec.Code != http.StatusOK || respStatus(t, rec) != tc.wantStatus {
				t.Fatalf("code=%d status=%q，want 200/%s（%s）", rec.Code, respStatus(t, rec), tc.wantStatus, rec.Body.String())
			}
			if len(in.calls) != tc.wantCalls {
				t.Fatalf("AcceptReview 调用 %d 次，want %d", len(in.calls), tc.wantCalls)
			}
			if tc.wantCalls == 1 {
				ev := in.calls[0].ev
				if ev.PRNumber != 7 || ev.Key != "gitea:comment:881" || ev.Author != "alice" || ev.Provider != vcs.ProviderGitea {
					t.Errorf("事件 = %+v，want PR 7 / gitea:comment:881 / alice / gitea", ev)
				}
			}
			if tc.wantReason != "" {
				if got := cl.finished["gitea:gt-rec-1"]; !strings.Contains(got, tc.wantReason) {
					t.Errorf("投递记录 = %q，want 含 %q", got, tc.wantReason)
				}
			}
		})
	}
}

// ---------------------------------------------------------------- AC-15

func TestVCSWebhookDeliveryDedupe(t *testing.T) {
	in := &fakeReviewIntake{}
	h := &GiteaWebhook{Resolver: fakeVCSResolver{gtCollab: gtCollabAt(t, trustingGitea(t))}, Deliveries: newFakeClaimer(), Reviews: in}
	first := postGitea(t, h, gtReq{slug: "slug-a", delivery: "dup-1"})
	second := postGitea(t, h, gtReq{slug: "slug-a", delivery: "dup-1"})
	if respStatus(t, first) != "queued" || respStatus(t, second) != "duplicate" {
		t.Errorf("两次响应 = %s / %s，want queued / duplicate", respStatus(t, first), respStatus(t, second))
	}
	if len(in.calls) != 1 {
		t.Errorf("AcceptReview 调用 %d 次，want 1", len(in.calls))
	}
}

// ---------------------------------------------------------------- AC-16

func TestGitLabReviewerWithoutMembershipRejected(t *testing.T) {
	in, cl := &fakeReviewIntake{}, newFakeClaimer()
	h := &GitLabWebhook{Resolver: fakeVCSResolver{glMembers: glMembersAt(t, newPlatformStub(t, replyCode(404)))}, Deliveries: cl, Reviews: in}
	rec := postGitLab(t, h, glReq{slug: "slug-a", uuid: "m-404"})
	if respStatus(t, rec) != "ignored" || len(in.calls) != 0 {
		t.Fatalf("非成员：status=%s calls=%d，want ignored/0", respStatus(t, rec), len(in.calls))
	}
	if got := cl.finished["gitlab:m-404"]; !strings.Contains(got, "成员") {
		t.Errorf("投递记录应写原因，得到 %q", got)
	}
	if strings.Contains(rec.Body.String(), "404") {
		t.Errorf("响应体不应透出成员查询细节（防探测）：%s", rec.Body.String())
	}
}

func TestGitLabReviewerMembershipAPIErrorFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply func(http.ResponseWriter, *http.Request)
	}{
		{"500", replyCode(500)},
		{"超时（连接即断）", func(w http.ResponseWriter, r *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "x", 500)
				return
			}
			c, _, _ := hj.Hijack()
			_ = c.Close()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := &fakeReviewIntake{}
			h := &GitLabWebhook{Resolver: fakeVCSResolver{glMembers: glMembersAt(t, newPlatformStub(t, tc.reply))}, Deliveries: newFakeClaimer(), Reviews: in}
			if rec := postGitLab(t, h, glReq{slug: "slug-a"}); respStatus(t, rec) != "ignored" || len(in.calls) != 0 {
				t.Errorf("成员接口异常：status=%s calls=%d，want ignored/0", respStatus(t, rec), len(in.calls))
			}
		})
	}
	t.Run("未接线（无凭据）", func(t *testing.T) {
		in := &fakeReviewIntake{}
		h := &GitLabWebhook{Resolver: fakeVCSResolver{}, Deliveries: newFakeClaimer(), Reviews: in}
		if rec := postGitLab(t, h, glReq{slug: "slug-a"}); respStatus(t, rec) != "ignored" || len(in.calls) != 0 {
			t.Errorf("无成员查询能力：status=%s calls=%d，want ignored/0", respStatus(t, rec), len(in.calls))
		}
	})
}

func TestGitLabReporterAccepted(t *testing.T) {
	in := &fakeReviewIntake{}
	stub := newPlatformStub(t, replyJSON(map[string]any{"id": 2, "access_level": 20}))
	h := &GitLabWebhook{Resolver: fakeVCSResolver{glMembers: glMembersAt(t, stub)}, Deliveries: newFakeClaimer(), Reviews: in}
	rec := postGitLab(t, h, glReq{slug: "slug-a"})
	if respStatus(t, rec) != "queued" || len(in.calls) != 1 {
		t.Fatalf("Reporter：status=%s calls=%d，want queued/1（%s）", respStatus(t, rec), len(in.calls), rec.Body.String())
	}
	ev := in.calls[0].ev
	if ev.Provider != vcs.ProviderGitLab || ev.Repo != "group/sub/demo" || ev.PRNumber != 5 || ev.Key != "gitlab:note:701" {
		t.Errorf("事件 = %+v，want gitlab / group/sub/demo / iid 5 / gitlab:note:701", ev)
	}
}

func TestGiteaReviewerNotCollaboratorRejected(t *testing.T) {
	for _, reply := range []func(http.ResponseWriter, *http.Request){
		replyJSON(map[string]any{"permission": "read"}), replyCode(404), replyCode(500),
	} {
		in := &fakeReviewIntake{}
		h := &GiteaWebhook{Resolver: fakeVCSResolver{gtCollab: gtCollabAt(t, newPlatformStub(t, reply))}, Deliveries: newFakeClaimer(), Reviews: in}
		if rec := postGitea(t, h, gtReq{slug: "slug-a"}); respStatus(t, rec) != "ignored" || len(in.calls) != 0 {
			t.Errorf("非协作者：status=%s calls=%d，want ignored/0", respStatus(t, rec), len(in.calls))
		}
	}
}

func TestGiteaReviewerWritePermissionAccepted(t *testing.T) {
	for _, perm := range []string{"write", "admin", "owner"} {
		in := &fakeReviewIntake{}
		stub := newPlatformStub(t, replyJSON(map[string]any{"permission": perm}))
		h := &GiteaWebhook{Resolver: fakeVCSResolver{gtCollab: gtCollabAt(t, stub)}, Deliveries: newFakeClaimer(), Reviews: in}
		if rec := postGitea(t, h, gtReq{slug: "slug-a"}); respStatus(t, rec) != "queued" || len(in.calls) != 1 {
			t.Errorf("permission=%s：status=%s calls=%d，want queued/1", perm, respStatus(t, rec), len(in.calls))
		}
	}
}

func TestSelfCommentIgnoredOnAllProviders(t *testing.T) {
	t.Run("github", func(t *testing.T) {
		in := &fakeReviewIntake{}
		body := strings.Replace(ghReviewCommentBody, `"login": "alice"`, `"login": "lathe-bot"`, 1)
		rec := postGitHub(t, newGitHubHandler(newFakeClaimer(), in), ghReq{slug: "slug-a", secret: ghSecretA, body: body})
		if respStatus(t, rec) != "ignored" || len(in.calls) != 0 {
			t.Errorf("status=%s calls=%d", respStatus(t, rec), len(in.calls))
		}
	})
	t.Run("gitlab", func(t *testing.T) {
		in := &fakeReviewIntake{}
		stub := trustingGitLab(t)
		h := &GitLabWebhook{Resolver: fakeVCSResolver{glMembers: glMembersAt(t, stub)}, Deliveries: newFakeClaimer(), Reviews: in}
		if rec := postGitLab(t, h, glReq{slug: "slug-a", body: gitlabNoteBody(702, 1, 1)}); respStatus(t, rec) != "ignored" || len(in.calls) != 0 {
			t.Errorf("status=%s calls=%d", respStatus(t, rec), len(in.calls))
		}
		if stub.hits != 0 {
			t.Errorf("自身评论在信任判定之前就该挡下，成员接口被调了 %d 次", stub.hits)
		}
	})
	t.Run("gitea（1.20/1.22 形状：无 pull_request 字段）", func(t *testing.T) {
		in := &fakeReviewIntake{}
		body := giteaCommentBody("created", true, "lathe-bot", "lathe-bot")
		h := &GiteaWebhook{Resolver: fakeVCSResolver{gtCollab: gtCollabAt(t, trustingGitea(t))}, Deliveries: newFakeClaimer(), Reviews: in}
		if rec := postGitea(t, h, gtReq{slug: "slug-a", body: body}); respStatus(t, rec) != "ignored" || len(in.calls) != 0 {
			t.Errorf("status=%s calls=%d", respStatus(t, rec), len(in.calls))
		}
	})
}

// ---------------------------------------------------------------- AC-17

func TestVCSWebhookHandlersSetProvider(t *testing.T) {
	ghIn, glIn, gtIn := &fakeReviewIntake{}, &fakeReviewIntake{}, &fakeReviewIntake{}
	postGitHub(t, newGitHubHandler(newFakeClaimer(), ghIn), ghReq{slug: "slug-a", secret: ghSecretA})
	postGitLab(t, &GitLabWebhook{Resolver: fakeVCSResolver{glMembers: glMembersAt(t, trustingGitLab(t))}, Deliveries: newFakeClaimer(), Reviews: glIn}, glReq{slug: "slug-a"})
	postGitea(t, &GiteaWebhook{Resolver: fakeVCSResolver{gtCollab: gtCollabAt(t, trustingGitea(t))}, Deliveries: newFakeClaimer(), Reviews: gtIn}, gtReq{slug: "slug-a"})
	for _, c := range []struct {
		in   *fakeReviewIntake
		want vcs.Provider
	}{{ghIn, vcs.ProviderGitHub}, {glIn, vcs.ProviderGitLab}, {gtIn, vcs.ProviderGitea}} {
		if len(c.in.calls) != 1 || c.in.calls[0].ev.Provider != c.want {
			t.Errorf("want 一次 Provider=%s 的回流，得到 %+v", c.want, c.in.calls)
		}
	}
}

// TestVCSWebhookCannotReachOtherOwnersTasks：slug-A 的投递带着 B 名下任务的仓库/编号 → ignored，B 的任务不动。
func TestVCSWebhookCannotReachOtherOwnersTasks(t *testing.T) {
	st := testStoreForAPI(t)
	ctx := context.Background()
	userA := mustUser(t, st, "vcs-iso-a-"+nonce()+"@example.com")
	userB := mustUser(t, st, "vcs-iso-b-"+nonce()+"@example.com")
	m := task.NewMachine(st.Pool())
	mkTask := func(user int64, provider, path string, pr int) *task.Task {
		t.Helper()
		var repoID int64
		if err := st.Pool().QueryRow(ctx, `INSERT INTO repos (user_id, provider_repo, vcs_provider, vcs_base_url) VALUES ($1,$2,$3,$4) RETURNING id`,
			user, path, provider, "https://"+provider+".example.com").Scan(&repoID); err != nil {
			t.Fatal(err)
		}
		tk, err := m.Create(ctx, task.CreateParams{UserID: user, RepoID: repoID, ExternalKey: "ISO-" + nonce()})
		if err != nil {
			t.Fatal(err)
		}
		sess := "s"
		if _, err := m.Transition(ctx, tk.ID, task.StateImplementing, "test", &task.TransitionOpts{AgentSessionID: &sess}); err != nil {
			t.Fatal(err)
		}
		for _, s := range []task.State{task.StateVerifying, task.StatePROpen} {
			if _, err := m.Transition(ctx, tk.ID, s, "test", nil); err != nil {
				t.Fatal(err)
			}
		}
		if err := m.SetPRNumber(ctx, tk.ID, pr); err != nil {
			t.Fatal(err)
		}
		return tk
	}
	glB := mkTask(userB, "gitlab", "group/sub/demo", 5)
	gtB := mkTask(userB, "gitea", "acme/demo", 7)

	intake := &runner.ReviewIntake{Tasks: m}
	res := fakeVCSResolver{owner: userA, glMembers: glMembersAt(t, trustingGitLab(t)), gtCollab: gtCollabAt(t, trustingGitea(t))}
	glRec := postGitLab(t, &GitLabWebhook{Resolver: res, Deliveries: newFakeClaimer(), Reviews: intake}, glReq{slug: "slug-a"})
	gtRec := postGitea(t, &GiteaWebhook{Resolver: res, Deliveries: newFakeClaimer(), Reviews: intake}, gtReq{slug: "slug-a"})
	for name, rec := range map[string]*httptest.ResponseRecorder{"gitlab": glRec, "gitea": gtRec} {
		if respStatus(t, rec) != "ignored" {
			t.Errorf("%s：status=%s，want ignored（%s）", name, respStatus(t, rec), rec.Body.String())
		}
	}
	for _, tk := range []*task.Task{glB, gtB} {
		if got, _ := m.Get(ctx, tk.ID); got.State != task.StatePROpen {
			t.Errorf("B 的任务 #%d 被推动到 %s", tk.ID, got.State)
		}
	}
}

// ---------------------------------------------------------------- AC-2b / AC-12b

// TestGitHubWebhookNonActionableNeverReachesIntake：Actionable 仍在 AcceptReview 之前。
func TestGitHubWebhookNonActionableNeverReachesIntake(t *testing.T) {
	in := &fakeReviewIntake{}
	body := strings.Replace(ghReviewCommentBody, `"MEMBER"`, `"NONE"`, 1)
	rec := postGitHub(t, newGitHubHandler(newFakeClaimer(), in), ghReq{slug: "slug-a", secret: ghSecretA, body: body})
	if respStatus(t, rec) != "ignored" || len(in.calls) != 0 {
		t.Errorf("NONE 关系的评论：status=%s calls=%d，want ignored/0", respStatus(t, rec), len(in.calls))
	}
}

func TestVCSWebhookSecretNeverLoggedOrEchoed(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var bodies []string
	bad := "sha256=" + strings.Repeat("1", 64)
	gh := newGitHubHandler(newFakeClaimer(), &fakeReviewIntake{})
	bodies = append(bodies, postGitHub(t, gh, ghReq{slug: "slug-a", signature: &bad}).Body.String(),
		postGitHub(t, gh, ghReq{slug: "slug-a", secret: ghSecretA, delivery: "gh-sec-ok"}).Body.String())
	gl := &GitLabWebhook{Resolver: fakeVCSResolver{glMembers: glMembersAt(t, trustingGitLab(t))}, Deliveries: newFakeClaimer(), Reviews: &fakeReviewIntake{}}
	bodies = append(bodies, postGitLab(t, gl, glReq{slug: "slug-a", token: "not-the-secret-xxxxx"}).Body.String(),
		postGitLab(t, gl, glReq{slug: "slug-a"}).Body.String())
	badHex := strings.Repeat("2", 64)
	gt := &GiteaWebhook{Resolver: fakeVCSResolver{gtCollab: gtCollabAt(t, trustingGitea(t))}, Deliveries: newFakeClaimer(), Reviews: &fakeReviewIntake{}}
	bodies = append(bodies, postGitea(t, gt, gtReq{slug: "slug-a", signature: &badHex}).Body.String(),
		postGitea(t, gt, gtReq{slug: "slug-a"}).Body.String())

	all := logs.String() + strings.Join(bodies, "\n")
	for _, secret := range []string{ghSecretA, glSecretA, gtSecretA} {
		if strings.Contains(all, secret) {
			t.Errorf("日志或响应体里出现了 webhook 密钥 %q…", secret[:6])
		}
	}
	if !strings.Contains(logs.String(), "验签失败") {
		t.Errorf("验签失败应留日志（只是不带密钥）：%s", logs.String())
	}
	_ = github.HeaderSignature
}
