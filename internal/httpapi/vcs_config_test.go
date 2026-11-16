package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zichuanwangcloud-gif/lathe/internal/store"
	"github.com/zichuanwangcloud-gif/lathe/internal/task"
)

// vcsConfigServer 把凭据与仓库两组路由挂在同一个用户下：登记 GitLab/Gitea
// 仓库要先在凭据页配好实例地址。
func vcsConfigServer(t *testing.T) (*httptestServer, *fakeVerifier, int64) {
	t.Helper()
	api, ver, ts := credFixture(t)
	repoAPI := &API{Store: ts.store, Tasks: task.NewMachine(ts.store.Pool()), Queue: &fakeEnqueuer{}, Auth: api.Auth}
	mux := http.NewServeMux()
	api.Routes(mux)
	repoAPI.Routes(mux)
	srv := newTestServer(t, mux)
	srv.api, srv.store, srv.userID = api, ts.store, ts.userID
	return srv, ver, ts.userID
}

func nonce() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }

// AC-11：baseUrl 去尾斜杠入库，令牌只以密文存在。
func TestSaveIntegrationStoresBaseURL(t *testing.T) {
	srv, ver, userID := vcsConfigServer(t)
	ver.results[store.KindGitLab] = VerifyResult{OK: true, AccountName: "alice"}
	tok := "glpat-x" + nonce()

	resp := srv.do(t, "PUT", "/api/integrations/gitlab",
		`{"token":"`+tok+`","baseUrl":"https://gitlab.example.com/"}`, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT gitlab = %d %s，want 200", resp.StatusCode, srv.raw(t, resp))
	}
	var base string
	var leaked bool
	if err := srv.store.Pool().QueryRow(context.Background(), `
		SELECT base_url, secret_enc::text LIKE '%'||$3||'%'
		FROM integrations WHERE user_id=$1 AND kind=$2`, userID, "gitlab", tok).Scan(&base, &leaked); err != nil {
		t.Fatalf("查凭据行失败: %v", err)
	}
	if base != "https://gitlab.example.com" {
		t.Errorf("base_url = %q，want https://gitlab.example.com", base)
	}
	if leaked {
		t.Error("secret_enc 含明文令牌")
	}
	if len(ver.calls) != 1 || !strings.Contains(ver.calls[0], "https://gitlab.example.com") {
		t.Errorf("保存即验证时应带实例地址，calls = %v", ver.calls)
	}

	// GET 展示 baseUrl（非密）
	raw := srv.raw(t, srv.do(t, "GET", "/api/integrations", "", true))
	if !strings.Contains(raw, `"baseUrl":"https://gitlab.example.com"`) {
		t.Errorf("状态接口应展示 baseUrl：%s", raw)
	}
}

// AC-11：实例地址校验。
func TestBaseURLValidation(t *testing.T) {
	srv, ver, _ := vcsConfigServer(t)
	ver.results[store.KindGitLab] = VerifyResult{OK: true}
	ver.results[store.KindGitHub] = VerifyResult{OK: true}
	cases := []struct {
		name string
		kind string
		body string
		want int
	}{
		{"gitlab 缺 baseUrl", "gitlab", `{"token":"glpat-1"}`, http.StatusBadRequest},
		{"非 loopback 的 http", "gitlab", `{"token":"glpat-1","baseUrl":"http://evil.example.com"}`, http.StatusBadRequest},
		{"带 userinfo", "gitlab", `{"token":"glpat-1","baseUrl":"https://u:p@h.example"}`, http.StatusBadRequest},
		{"带 query", "gitlab", `{"token":"glpat-1","baseUrl":"https://h.example?x=1"}`, http.StatusBadRequest},
		{"loopback http 允许", "gitlab", `{"token":"glpat-1","baseUrl":"http://127.0.0.1:8080"}`, http.StatusOK},
		{"gitea 缺 baseUrl", "gitea", `{"token":"gt-1"}`, http.StatusBadRequest},
		{"github 传 baseUrl 拒绝", "github", `{"token":"ghp-1","baseUrl":"https://ghe.example.com"}`, http.StatusBadRequest},
		{"linear 传 baseUrl 拒绝", "linear", `{"token":"lin-1","baseUrl":"https://x.example.com"}`, http.StatusBadRequest},
		{"github 不传照旧", "github", `{"token":"ghp-1"}`, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := srv.do(t, "PUT", "/api/integrations/"+tc.kind, tc.body, true)
			if resp.StatusCode != tc.want {
				t.Errorf("PUT %s %s = %d，want %d", tc.kind, tc.body, resp.StatusCode, tc.want)
			}
		})
	}
}

// AC-12：状态接口永不回明文（三个平台）。
func TestIntegrationStatusNeverReturnsPlaintext(t *testing.T) {
	srv, ver, _ := vcsConfigServer(t)
	for _, k := range []string{store.KindGitHub, store.KindGitLab, store.KindGitea, store.KindGitLabWebhook, store.KindGiteaWebhook} {
		ver.results[k] = VerifyResult{OK: true}
	}
	n := nonce()
	toks := map[string]string{
		"github":         "ghp-plain-" + n,
		"gitlab":         "glpat-plain-" + n,
		"gitea":          "gitea-plain-" + n,
		"gitlab_webhook": "glwh-plain-" + n,
		"gitea_webhook":  "gtwh-plain-" + n,
	}
	for kind, tok := range toks {
		body := `{"token":"` + tok + `"}`
		if kind == "gitlab" || kind == "gitea" {
			body = `{"token":"` + tok + `","baseUrl":"https://` + kind + `.example.com"}`
		}
		if resp := srv.do(t, "PUT", "/api/integrations/"+kind, body, true); resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT %s = %d", kind, resp.StatusCode)
		}
	}
	raw := srv.raw(t, srv.do(t, "GET", "/api/integrations", "", true))
	for kind, tok := range toks {
		if strings.Contains(raw, tok) {
			t.Errorf("状态接口泄露了 %s 的明文令牌", kind)
		}
		if !strings.Contains(raw, `"kind":"`+kind+`"`) {
			t.Errorf("状态接口应列出 %s", kind)
		}
	}
}

// AC-13：登记仓库可选平台；实例地址只来自凭据。
func TestCreateRepoWithVCSProvider(t *testing.T) {
	srv, ver, userID := vcsConfigServer(t)
	ver.results[store.KindGitLab] = VerifyResult{OK: true}

	// 未配 gitlab 凭据 → 400
	resp := srv.do(t, "POST", "/api/repos", `{"providerRepo":"group/sub/demo","vcsProvider":"gitlab"}`, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("未配凭据登记 gitlab 仓库 = %d，want 400", resp.StatusCode)
	}

	if resp := srv.do(t, "PUT", "/api/integrations/gitlab",
		`{"token":"glpat-y","baseUrl":"https://gitlab.example.com/"}`, true); resp.StatusCode != http.StatusOK {
		t.Fatalf("配 gitlab 凭据 = %d", resp.StatusCode)
	}
	resp = srv.do(t, "POST", "/api/repos", `{"providerRepo":"group/sub/demo","vcsProvider":"gitlab"}`, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("登记 gitlab 仓库 = %d %s，want 201", resp.StatusCode, srv.raw(t, resp))
	}
	body := srv.decode(t, resp)
	if body["vcsProvider"] != "gitlab" || body["vcsBaseUrl"] != "https://gitlab.example.com" {
		t.Errorf("响应 = %+v，want vcsProvider=gitlab vcsBaseUrl=https://gitlab.example.com", body)
	}
	var p, b string
	if err := srv.store.Pool().QueryRow(context.Background(),
		`SELECT vcs_provider, vcs_base_url FROM repos WHERE user_id=$1 AND provider_repo=$2`,
		userID, "group/sub/demo").Scan(&p, &b); err != nil {
		t.Fatalf("查 repos 失败: %v", err)
	}
	if p != "gitlab" || b != "https://gitlab.example.com" {
		t.Errorf("库里 (%q, %q)，want (gitlab, https://gitlab.example.com)", p, b)
	}

	// GET 列表也带平台字段
	raw := srv.raw(t, srv.do(t, "GET", "/api/repos", "", true))
	if !strings.Contains(raw, `"vcsProvider":"gitlab"`) || !strings.Contains(raw, `"vcsBaseUrl":"https://gitlab.example.com"`) {
		t.Errorf("GET /api/repos 应带平台字段：%s", raw)
	}

	for _, tc := range []struct{ name, body string }{
		{"请求体带 vcsBaseUrl", `{"providerRepo":"g/x","vcsProvider":"gitlab","vcsBaseUrl":"https://evil.example.com"}`},
		{"未知平台", `{"providerRepo":"g/x","vcsProvider":"bitbucket"}`},
		{"github 三段", `{"providerRepo":"a/b/c","vcsProvider":"github"}`},
		{"gitlab 路径非法", `{"providerRepo":"g/../x","vcsProvider":"gitlab"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if resp := srv.do(t, "POST", "/api/repos", tc.body, true); resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s = %d，want 400", tc.body, resp.StatusCode)
			}
		})
	}
}

// AC-13：同一用户同一路径不能跨平台登记；另一用户不受影响。
func TestRepoPathUniqueAcrossProviders(t *testing.T) {
	srv, ver, _ := vcsConfigServer(t)
	ver.results[store.KindGitLab] = VerifyResult{OK: true}
	srv.do(t, "PUT", "/api/integrations/gitlab", `{"token":"glpat-z","baseUrl":"https://gitlab.example.com"}`, true)

	if resp := srv.do(t, "POST", "/api/repos", `{"providerRepo":"acme/demo"}`, true); resp.StatusCode != http.StatusCreated {
		t.Fatalf("登记 github acme/demo = %d", resp.StatusCode)
	}
	resp := srv.do(t, "POST", "/api/repos", `{"providerRepo":"acme/demo","vcsProvider":"gitlab"}`, true)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("同路径跨平台登记 = %d，want 409", resp.StatusCode)
	}
	if raw := srv.raw(t, resp); !strings.Contains(raw, "两个平台") {
		t.Errorf("409 文案应含「两个平台」：%s", raw)
	}

	// 另一用户
	userB := mustUser(t, srv.store, "vcs-b-"+nonce()+"@example.com")
	authB := authAs(userB, "b@example.com")
	credB := &CredentialAPI{Secrets: srv.api.Secrets, Verifier: ver, Auth: authB}
	repoB := &API{Store: srv.store, Tasks: task.NewMachine(srv.store.Pool()), Queue: &fakeEnqueuer{}, Auth: authB}
	mux := http.NewServeMux()
	credB.Routes(mux)
	repoB.Routes(mux)
	srvB := newTestServer(t, mux)
	srvB.do(t, "PUT", "/api/integrations/gitlab", `{"token":"glpat-b","baseUrl":"https://gitlab.example.com"}`, true)
	if resp := srvB.do(t, "POST", "/api/repos", `{"providerRepo":"acme/demo","vcsProvider":"gitlab"}`, true); resp.StatusCode != http.StatusCreated {
		t.Errorf("另一用户登记 gitlab acme/demo = %d，want 201", resp.StatusCode)
	}
}

// AC-13：平台/实例/路径登记后不可改。
func TestUpdateRepoRejectsPlatformChange(t *testing.T) {
	srv, ver, userID := vcsConfigServer(t)
	ver.results[store.KindGitLab] = VerifyResult{OK: true}
	srv.do(t, "PUT", "/api/integrations/gitlab", `{"token":"glpat-u","baseUrl":"https://gitlab.example.com"}`, true)
	resp := srv.do(t, "POST", "/api/repos", `{"providerRepo":"g/u","vcsProvider":"gitlab"}`, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("登记 = %d", resp.StatusCode)
	}
	id := fmt.Sprintf("%.0f", srv.decode(t, resp)["id"].(float64))

	for _, body := range []string{
		`{"vcsProvider":"github"}`,
		`{"vcsBaseUrl":"https://other.example.com"}`,
		`{"providerRepo":"g/other"}`,
		`{"gateMode":"direct","vcsProvider":"gitea"}`,
	} {
		t.Run(body, func(t *testing.T) {
			if resp := srv.do(t, "PUT", "/api/repos/"+id, body, true); resp.StatusCode != http.StatusBadRequest {
				t.Errorf("PUT %s = %d，want 400", body, resp.StatusCode)
			}
		})
	}
	var p, b, gate string
	if err := srv.store.Pool().QueryRow(context.Background(),
		`SELECT vcs_provider, vcs_base_url, gate_mode FROM repos WHERE id=$1 AND user_id=$2`, id, userID).Scan(&p, &b, &gate); err != nil {
		t.Fatal(err)
	}
	if p != "gitlab" || b != "https://gitlab.example.com" || gate != "direct" {
		t.Errorf("库里 (%s, %s, %s)，want 平台与实例不变", p, b, gate)
	}
}
