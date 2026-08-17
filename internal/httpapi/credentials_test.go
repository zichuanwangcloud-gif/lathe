package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Clouditera/lathe/internal/secret"
	"github.com/Clouditera/lathe/internal/store"
)

// fakeVerifier 按 kind 返回预设结果，避免测试依赖真实外部服务。
type fakeVerifier struct {
	results map[string]VerifyResult
	calls   []string
}

func (f *fakeVerifier) Verify(ctx context.Context, kind, token string) VerifyResult {
	f.calls = append(f.calls, kind+":"+token)
	if r, ok := f.results[kind]; ok {
		return r
	}
	return VerifyResult{OK: false, Error: "未预设结果"}
}

func credFixture(t *testing.T) (*CredentialAPI, *fakeVerifier, *httptestServer) {
	t.Helper()

	st := testStoreForAPI(t)
	key := make([]byte, secret.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	sealer, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}

	userID := mustUser(t, st, "cred-"+t.Name()+"@example.com")

	ver := &fakeVerifier{results: map[string]VerifyResult{}}
	api := &CredentialAPI{
		Secrets:  st.NewSecrets(sealer),
		Verifier: ver, Auth: authAs(userID, "cred@example.com"),
	}

	mux := http.NewServeMux()
	api.Routes(mux)
	ts := newTestServer(t, mux)
	ts.api = api
	ts.store = st
	ts.userID = userID
	return api, ver, ts
}

func TestCredentialSaveAndVerify(t *testing.T) {
	api, ver, srv := credFixture(t)
	ver.results[store.KindGitHub] = VerifyResult{
		OK: true, AccountName: "zichuan", AccountID: "zichuan", Detail: "已连接",
	}

	resp := srv.do(t, "PUT", "/api/integrations/github", `{"token":"ghp_real_token_value"}`, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("保存状态码 = %d", resp.StatusCode)
	}
	body := srv.decode(t, resp)
	if body["saved"] != true {
		t.Error("应报告已保存")
	}
	v, _ := body["verify"].(map[string]any)
	if v["ok"] != true || v["accountName"] != "zichuan" {
		t.Errorf("验证结果不符: %+v", v)
	}

	// 保存后应立即验证，不必再点一次
	if len(ver.calls) != 1 || !strings.Contains(ver.calls[0], "ghp_real_token_value") {
		t.Errorf("保存后应自动验证: %v", ver.calls)
	}

	// 状态里应有掩码与账号，但绝不能有完整凭据
	resp = srv.do(t, "GET", "/api/integrations", "", true)
	raw := srv.raw(t, resp)
	if strings.Contains(raw, "ghp_real_token_value") {
		t.Error("状态接口泄漏了完整凭据")
	}
	if !strings.Contains(raw, "zichuan") {
		t.Error("状态应含账号名，便于确认配的是哪个账号")
	}

	// 存的是密文，读回来应还是原文
	got, err := api.Secrets.Get(context.Background(), srv.userID, store.KindGitHub)
	if err != nil || got != "ghp_real_token_value" {
		t.Errorf("凭据读回不符: %q %v", got, err)
	}
}

// 验证失败也要保存 —— 可能只是网络暂时不通，凭据本身没问题。
func TestCredentialSavesEvenWhenVerifyFails(t *testing.T) {
	api, ver, srv := credFixture(t)
	ver.results[store.KindGitHub] = VerifyResult{OK: false, Error: "令牌无效"}

	resp := srv.do(t, "PUT", "/api/integrations/github", `{"token":"ghp_bad"}`, true)
	body := srv.decode(t, resp)
	if body["saved"] != true {
		t.Error("验证失败也应保存")
	}

	got, err := api.Secrets.Get(context.Background(), srv.userID, store.KindGitHub)
	if err != nil || got != "ghp_bad" {
		t.Errorf("凭据应已保存: %q %v", got, err)
	}

	// 失败原因要落库供界面展示
	resp = srv.do(t, "GET", "/api/integrations", "", true)
	if !strings.Contains(srv.raw(t, resp), "令牌无效") {
		t.Error("失败原因应在状态里可见")
	}
}

// 换新凭据时必须清掉旧的验证结论，否则界面上会显示过期的「已验证」。
func TestCredentialResetsVerificationOnChange(t *testing.T) {
	_, ver, srv := credFixture(t)
	ver.results[store.KindGitHub] = VerifyResult{OK: true, AccountName: "old"}

	srv.do(t, "PUT", "/api/integrations/github", `{"token":"old-token"}`, true)

	// 换成一个验证不了的新凭据
	ver.results[store.KindGitHub] = VerifyResult{OK: false, Error: "新令牌无效"}
	srv.do(t, "PUT", "/api/integrations/github", `{"token":"new-token"}`, true)

	resp := srv.do(t, "GET", "/api/integrations", "", true)
	raw := srv.raw(t, resp)
	if strings.Contains(raw, `"accountName":"old"`) {
		t.Error("换凭据后不应保留旧账号名")
	}
	if !strings.Contains(raw, "新令牌无效") {
		t.Error("应显示新的失败原因")
	}
}

func TestCredentialVerifyExisting(t *testing.T) {
	_, ver, srv := credFixture(t)
	ver.results[store.KindGitHub] = VerifyResult{OK: true, AccountName: "zichuan"}

	srv.do(t, "PUT", "/api/integrations/github", `{"token":"tok"}`, true)
	ver.calls = nil

	resp := srv.do(t, "POST", "/api/integrations/github/verify", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	// 应用已存的凭据去验证，而不是要求重新输入
	if len(ver.calls) != 1 || !strings.Contains(ver.calls[0], "tok") {
		t.Errorf("应使用已存凭据验证: %v", ver.calls)
	}
}

func TestCredentialVerifyUnconfigured(t *testing.T) {
	_, _, srv := credFixture(t)

	resp := srv.do(t, "POST", "/api/integrations/linear/verify", "", true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("未配置的凭据验证应返回 400，得到 %d", resp.StatusCode)
	}
}

func TestCredentialDelete(t *testing.T) {
	api, ver, srv := credFixture(t)
	ver.results[store.KindGitHub] = VerifyResult{OK: true}

	srv.do(t, "PUT", "/api/integrations/github", `{"token":"tok"}`, true)
	resp := srv.do(t, "DELETE", "/api/integrations/github", "", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("删除状态码 = %d", resp.StatusCode)
	}

	if _, err := api.Secrets.Get(context.Background(), srv.userID, store.KindGitHub); err == nil {
		t.Error("删除后不应还能读到凭据")
	}
}

func TestCredentialRejectsBadInput(t *testing.T) {
	_, _, srv := credFixture(t)

	cases := []struct {
		path, body string
		want       int
	}{
		{"/api/integrations/github", `{"token":"   "}`, http.StatusBadRequest},
		{"/api/integrations/github", `{}`, http.StatusBadRequest},
		{"/api/integrations/bogus", `{"token":"x"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		resp := srv.do(t, "PUT", tc.path, tc.body, true)
		if resp.StatusCode != tc.want {
			t.Errorf("PUT %s %s → %d，期望 %d", tc.path, tc.body, resp.StatusCode, tc.want)
		}
	}
}

// 凭据接口同样要鉴权 —— 否则任何人都能改掉或删掉凭据。
func TestCredentialRequiresAuth(t *testing.T) {
	_, _, srv := credFixture(t)

	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/integrations", ""},
		{"PUT", "/api/integrations/github", `{"token":"x"}`},
		{"POST", "/api/integrations/github/verify", ""},
		{"DELETE", "/api/integrations/github", ""},
	} {
		resp := srv.do(t, tc.method, tc.path, tc.body, false)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s 未认证时 = %d，期望 401", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// 凭据变更后必须让缓存失效，否则改完还要等 TTL 过期才生效。
// 失效粒度必须到用户：A 改凭据不该碰 B 的缓存。
func TestCredentialInvalidatesCache(t *testing.T) {
	var invalidated []int64
	_, ver, srv := credFixture(t)
	ver.results[store.KindGitHub] = VerifyResult{OK: true}

	srv.api.OnChange = func(userID int64, kind string) { invalidated = append(invalidated, userID) }

	srv.do(t, "PUT", "/api/integrations/github", `{"token":"tok"}`, true)
	srv.do(t, "DELETE", "/api/integrations/github", "", true)

	if len(invalidated) != 2 {
		t.Fatalf("保存与删除都应触发缓存失效，实际 %v", invalidated)
	}
	for _, id := range invalidated {
		if id != srv.userID {
			t.Errorf("失效应针对当前用户 %d，实际 %d", srv.userID, id)
		}
	}
}

// ★隔离（P1.5 第二步）：B 保存的凭据落在 B 名下，读不到也动不了 A 的。
func TestCredentialIsolationBetweenUsers(t *testing.T) {
	_, ver, srvA := credFixture(t)
	ver.results[store.KindGitHub] = VerifyResult{OK: true, AccountName: "user-a"}

	srvA.do(t, "PUT", "/api/integrations/github", `{"token":"token-of-a"}`, true)

	// B 的视角：同一台服务、同一个 Secrets 后端，只是登录的是 B
	userB := mustUser(t, srvA.store, "cred-b-"+t.Name()+"@example.com")
	apiB := &CredentialAPI{
		Secrets: srvA.api.Secrets, Verifier: ver,
		Auth: authAs(userB, "b@example.com"),
	}
	mux := http.NewServeMux()
	apiB.Routes(mux)
	srvB := newTestServer(t, mux)

	// B 的状态列表里绝不能出现 A 的凭据痕迹
	resp := srvB.do(t, "GET", "/api/integrations", "", true)
	raw := srvB.raw(t, resp)
	if strings.Contains(raw, "user-a") || strings.Contains(raw, "token-of-a") {
		t.Errorf("B 不应看到 A 的凭据状态: %s", raw)
	}

	// B 验证 github → 未配置（A 配了不等于 B 配了）
	resp = srvB.do(t, "POST", "/api/integrations/github/verify", "", true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("B 未配置时应 400，得到 %d", resp.StatusCode)
	}

	// B 保存自己的凭据，互不干扰
	ver.results[store.KindGitHub] = VerifyResult{OK: true, AccountName: "user-b"}
	srvB.do(t, "PUT", "/api/integrations/github", `{"token":"token-of-b"}`, true)

	gotA, err := srvA.api.Secrets.Get(context.Background(), srvA.userID, store.KindGitHub)
	if err != nil || gotA != "token-of-a" {
		t.Errorf("A 的凭据不应被 B 改动: %q %v", gotA, err)
	}
	gotB, err := srvA.api.Secrets.Get(context.Background(), userB, store.KindGitHub)
	if err != nil || gotB != "token-of-b" {
		t.Errorf("B 的凭据应落在 B 名下: %q %v", gotB, err)
	}
}
