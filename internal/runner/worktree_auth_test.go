package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/vcs"
)

// ---------------------------------------------------------------- AC-5b

// poison 是 agent 在 worktree 里能做的一种投毒（写的是 mirror config / hooks）。
type poison struct {
	name  string
	apply func(t *testing.T, env poisonEnv)
}

type poisonEnv struct {
	wt, mirror, base, cloneURL, proxy, scratch string
}

var allPoisons = []poison{
	{"proxy_most_specific_url", func(t *testing.T, e poisonEnv) {
		gitOut(t, e.wt, "config", "http."+e.cloneURL+".proxy", e.proxy)
	}},
	{"proxy_base_prefix", func(t *testing.T, e poisonEnv) {
		gitOut(t, e.wt, "config", "http."+e.base+"/.proxy", e.proxy)
	}},
	{"sslverify_off", func(t *testing.T, e poisonEnv) {
		gitOut(t, e.wt, "config", "http."+e.cloneURL+".sslVerify", "false")
	}},
	{"sslcainfo_via_include", func(t *testing.T, e poisonEnv) {
		inc := filepath.Join(e.scratch, "evil.inc")
		body := fmt.Sprintf("[http]\n\tsslCAInfo = %s/evil-ca.pem\n\tproxy = %s\n", e.scratch, e.proxy)
		if err := os.WriteFile(inc, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		gitOut(t, e.wt, "config", "include.path", inc)
	}},
	{"insteadof_rewrite", func(t *testing.T, e poisonEnv) {
		gitOut(t, e.wt, "config", "url."+e.proxy+"/.insteadOf", e.base+"/")
	}},
	{"credential_helper", func(t *testing.T, e poisonEnv) {
		out := filepath.Join(e.scratch, "helper.out")
		gitOut(t, e.wt, "config", "credential.helper", "!f() { cat >> "+out+"; }; f")
		gitOut(t, e.wt, "config", "credential."+e.base+".helper", "!f() { cat >> "+out+"; }; f")
	}},
	{"remote_origin_url", func(t *testing.T, e poisonEnv) {
		gitOut(t, e.wt, "config", "remote.origin.url", e.proxy+"/x.git")
	}},
	{"hooks", func(t *testing.T, e poisonEnv) {
		rec := filepath.Join(e.scratch, "hook.out")
		for _, h := range []string{"pre-push", "reference-transaction", "post-update"} {
			script := fmt.Sprintf("#!/bin/sh\n{ echo \"HOOK %s GIT_DIR=$GIT_DIR\"; env; } >> %s\n", h, rec)
			if err := os.WriteFile(filepath.Join(e.mirror, "hooks", h), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}},
}

// TestAuthedGitIgnoresAgentConfig：agent 写进 mirror/worktree 的 config 与
// hooks，一项都影响不到带 token 的 git 调用。每个子测试只投一种毒（最后一个
// 全投），依次走 EnsureMirror（首次与刷新）、Push、RebaseOnto、ForcePush、Reattach。
func TestAuthedGitIgnoresAgentConfig(t *testing.T) {
	cases := append([]poison(nil), allPoisons...)
	cases = append(cases, poison{"config_origin_only_command_line", func(t *testing.T, e poisonEnv) {
		for _, p := range allPoisons {
			p.apply(t, e)
		}
	}})
	for _, prefix := range []string{"", "/gitlab"} {
		for _, pz := range cases {
			name := pz.name
			if prefix != "" {
				name += "/path_prefix"
			}
			t.Run(name, func(t *testing.T) { runPoisonCase(t, prefix, pz) })
		}
	}
}

func runPoisonCase(t *testing.T, prefix string, pz poison) {
	ctx := context.Background()
	tok := randToken(t, "tok-")
	stub := newGitHTTPStub(t, prefix, false, tok)
	const repoPath = "g/sub/x"
	src := stub.seedRepo(t, repoPath)
	proxy := newConnCounter(t)
	m := newManager(t)
	repo := authRepo(stub, repoPath)
	cloneURL := repo.CloneURL()
	auth := vcs.NewGitAuth("oauth2", tok)
	scratch := t.TempDir()

	// ① 首次建 mirror（经 Create：auth 由 CreateParams 带到 EnsureMirror）
	wt, err := m.Create(ctx, CreateParams{
		Repo: repo, CloneURL: cloneURL, Kind: KindFix, IssueKey: "CR-1", Title: "auth", TaskID: 1, Auth: auth,
	})
	if err != nil {
		t.Fatalf("Create（首次建 mirror）失败: %v", err)
	}
	pe := poisonEnv{wt: wt.Path, mirror: wt.Mirror, base: stub.base, cloneURL: cloneURL, proxy: proxy.url(), scratch: scratch}
	pz.apply(t, pe)

	// 远端删掉 tmp 分支：刷新后 mirror 的 origin/tmp 必须被 prune。
	gitOut(t, "", "--git-dir="+stub.bare(repoPath), "update-ref", "-d", "refs/heads/tmp")
	if _, err := m.EnsureMirror(ctx, repo.RepoKey(), cloneURL, auth); err != nil {
		t.Fatalf("EnsureMirror（刷新）失败: %v", err)
	}
	assertMirrorMatchesRemote(t, wt.Mirror, stub.bare(repoPath))

	// ② Push
	head := commitIn(t, wt.Path, "a.txt", "task\n")
	if err := m.Push(ctx, wt, repo, auth, nil); err != nil {
		t.Fatalf("Push 失败: %v", err)
	}
	if got := remoteHeads(t, stub.bare(repoPath))[wt.Branch]; got != head {
		t.Fatalf("远端 %s = %s，want worktree HEAD %s", wt.Branch, got, head)
	}

	// ③ RebaseOnto（内含带鉴权 fetch）+ ForcePush
	oldBase := gitOut(t, "", "--git-dir="+wt.Mirror, "rev-parse", MirrorBaseRef("dev"))
	stub.upstreamCommit(t, src, repoPath, "dev", "up.txt", "upstream\n")
	if err := m.RebaseOnto(ctx, wt, oldBase, "dev", cloneURL, auth); err != nil {
		t.Fatalf("RebaseOnto 失败: %v", err)
	}
	assertMirrorMatchesRemote(t, wt.Mirror, stub.bare(repoPath))
	rebased := gitOut(t, wt.Path, "rev-parse", "HEAD")
	if err := m.ForcePush(ctx, wt, repo, auth, nil); err != nil {
		t.Fatalf("ForcePush 失败: %v", err)
	}
	if got := remoteHeads(t, stub.bare(repoPath))[wt.Branch]; got != rebased {
		t.Fatalf("ForcePush 后远端 %s = %s，want %s", wt.Branch, got, rebased)
	}

	// ④ lease：第三方改了远端分支之后，ForcePush 必须被拒。
	devTip := remoteHeads(t, stub.bare(repoPath))["dev"]
	gitOut(t, "", "--git-dir="+stub.bare(repoPath), "update-ref", "refs/heads/"+wt.Branch, devTip)
	commitIn(t, wt.Path, "b.txt", "again\n")
	if err := m.ForcePush(ctx, wt, repo, auth, nil); err == nil {
		t.Fatal("远端被第三方改动后 ForcePush 应失败（force-with-lease 生效）")
	}
	if got := remoteHeads(t, stub.bare(repoPath))[wt.Branch]; got != devTip {
		t.Fatalf("lease 失败后远端分支被改动了：%s，want %s", got, devTip)
	}

	// ⑤ Reattach（内含带鉴权 fetch）：以远端为权威重挂。
	if err := m.Reattach(ctx, repo.RepoKey(), wt.Path, wt.Branch, cloneURL, auth); err != nil {
		t.Fatalf("Reattach 失败: %v", err)
	}
	if got := gitOut(t, wt.Path, "rev-parse", "HEAD"); got != devTip {
		t.Fatalf("Reattach 后 HEAD = %s，want 远端 tip %s", got, devTip)
	}
	assertMirrorMatchesRemote(t, wt.Mirror, stub.bare(repoPath))

	// 共同断言：鉴权真的生效（不是靠「不推」通过），且毒全部无效。
	if n := stub.sawAuth("oauth2", tok); n == 0 {
		t.Errorf("远端应收到 Basic base64(oauth2:%s…) 的请求", tok[:6])
	}
	if n := proxy.n.Load(); n != 0 {
		t.Errorf("agent 代理收到了 %d 个连接，want 0", n)
	}
	if _, err := os.Stat(filepath.Join(scratch, "helper.out")); err == nil {
		b, _ := os.ReadFile(filepath.Join(scratch, "helper.out"))
		t.Errorf("agent 的 credential helper 被执行了：%q", b)
	}
	if b, err := os.ReadFile(filepath.Join(scratch, "hook.out")); err == nil {
		s := string(b)
		if strings.Contains(s, tok) {
			t.Error("hook 记录里出现了 token")
		}
		for _, line := range strings.Split(s, "\n") {
			if strings.HasPrefix(line, "HOOK ") && strings.Contains(line, "lathe-git-transport-") {
				t.Errorf("transport 期间触发了 agent 的 hook：%s", line)
			}
		}
	}

	// 有效配置：在 transport 的同一环境下核对。
	tr, err := m.openTransport(wt.Mirror, cloneURL, auth)
	if err != nil {
		t.Fatalf("openTransport: %v", err)
	}
	defer tr.close()
	for _, key := range []string{"http.sslVerify", "http.proxy", "http.sslCAInfo"} {
		out, _ := tr.run(ctx, false, "", "config", "--get-urlmatch", key, cloneURL)
		if strings.TrimSpace(out) != "" {
			t.Errorf("transport 环境下 %s 对 %s 的有效值 = %q，want 空", key, cloneURL, out)
		}
	}
	list, err := tr.run(ctx, false, "", "config", "--list", "--show-origin")
	if err != nil {
		t.Fatalf("config --list --show-origin: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(list), "\n") {
		if line != "" && !strings.HasPrefix(line, "command line:") {
			t.Errorf("transport 环境的配置来源只能是 command line，得到：%s", line)
		}
	}
}

func assertMirrorMatchesRemote(t *testing.T, mirror, bare string) {
	t.Helper()
	got, want := mirrorRemotes(t, mirror), remoteHeads(t, bare)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mirror refs/remotes/origin/* = %v，want 与远端一致 %v", got, want)
	}
	if extra := refMap(t, mirror, "refs/lathe-have/", ""); len(extra) != 0 {
		t.Errorf("mirror 里不应出现 refs/lathe-have/*：%v", extra)
	}
}

// AC-5b（sslverify_off 的 HTTPS 子用例）：agent 关掉校验也没用，自签证书的推送/拉取必须失败。
func TestAuthedGitKeepsTLSVerification(t *testing.T) {
	ctx := context.Background()
	tok := randToken(t, "tok-")
	stub := newGitHTTPStub(t, "", true, tok)
	stub.seedRepo(t, "g/x")
	m := newManager(t)
	repo := authRepo(stub, "g/x")

	// 预先建一个 mirror 并在它的 config 里投毒，再让 Lathe 刷新它。
	mirror := m.MirrorPath(repo.RepoKey())
	gitOut(t, "", "init", "--quiet", "--bare", mirror)
	gitOut(t, "", "--git-dir="+mirror, "config", "http."+repo.CloneURL()+".sslVerify", "false")
	gitOut(t, "", "--git-dir="+mirror, "config", "http.sslVerify", "false")
	if _, err := m.EnsureMirror(ctx, repo.RepoKey(), repo.CloneURL(), vcs.NewGitAuth("oauth2", tok)); err == nil {
		t.Fatal("自签证书的远端应拉取失败：agent 写的 sslVerify=false 不得生效")
	}
	if n := stub.sawAuth("oauth2", tok); n != 0 {
		t.Errorf("TLS 未通过校验时不应有任何带 token 的请求到达，得到 %d", n)
	}
}

// ---------------------------------------------------------------- AC-5

// TestCreateEnsuresMirrorWithAuth：auth 真经 CreateParams 到达 EnsureMirror。
func TestCreateEnsuresMirrorWithAuth(t *testing.T) {
	ctx := context.Background()
	tok := randToken(t, "tok-")
	stub := newGitHTTPStub(t, "", false, tok)
	stub.seedRepo(t, "group/sub/demo")
	repo := authRepo(stub, "group/sub/demo")

	t.Run("带 auth 首次建 mirror 成功", func(t *testing.T) {
		m := newManager(t)
		if _, err := m.Create(ctx, CreateParams{
			Repo: repo, CloneURL: repo.CloneURL(), Kind: KindFix, IssueKey: "CR-2", Title: "x", TaskID: 2,
			Auth: vcs.NewGitAuth("oauth2", tok),
		}); err != nil {
			t.Fatalf("Create 失败: %v", err)
		}
		if stub.sawAuth("oauth2", tok) == 0 {
			t.Error("桩应收到带 token 的请求")
		}
	})
	t.Run("零值 auth 必须失败", func(t *testing.T) {
		m := newManager(t)
		if _, err := m.Create(ctx, CreateParams{
			Repo: repo, CloneURL: repo.CloneURL(), Kind: KindFix, IssueKey: "CR-3", Title: "x", TaskID: 3,
		}); err == nil {
			t.Fatal("要求鉴权的远端 + 零值 auth，Create 应返回错误")
		}
	})
}

// TestTransportEnvIsWhitelistAndTokenFree：transport 的子进程环境从零构造。
func TestTransportEnvIsWhitelistAndTokenFree(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	t.Setenv("LATHE_GITHUB_TOKEN", "ghp-should-not-leak")
	ctx := context.Background()
	m := newManager(t)
	mirror := filepath.Join(m.Root(), ".mirrors", "x.git")
	gitOut(t, "", "init", "--quiet", "--bare", mirror)
	tok := randToken(t, "tok-")
	url := "http://127.0.0.1:1/g/x.git"

	tr, err := m.openTransport(mirror, url, vcs.NewGitAuth("oauth2", tok))
	if err != nil {
		t.Fatal(err)
	}
	dir := tr.dir
	cmd, cleanup, err := tr.command(ctx, true, "", "fetch", url, "+refs/heads/*:refs/remotes/origin/*")
	if err != nil {
		t.Fatal(err)
	}

	allowed := map[string]bool{"PATH": true, "HOME": true, "GIT_DIR": true, "GIT_OBJECT_DIRECTORY": true,
		"GIT_CONFIG_NOSYSTEM": true, "GIT_CONFIG_GLOBAL": true, "GIT_TERMINAL_PROMPT": true, "GIT_ASKPASS": true,
		"GIT_PROTOCOL_FROM_USER": true, "GIT_CONFIG_COUNT": true}
	seen := map[string]bool{}
	for _, kv := range cmd.Env {
		k, v, _ := strings.Cut(kv, "=")
		seen[k] = true
		if !allowed[k] && !strings.HasPrefix(k, "GIT_CONFIG_KEY_") && !strings.HasPrefix(k, "GIT_CONFIG_VALUE_") {
			t.Errorf("环境里出现白名单外的变量 %s", k)
		}
		if strings.Contains(v, tok) {
			t.Errorf("环境变量 %s 含 token", k)
		}
	}
	for k := range allowed {
		if !seen[k] {
			t.Errorf("白名单变量 %s 缺失", k)
		}
	}
	for _, a := range cmd.Args {
		if strings.Contains(a, tok) {
			t.Errorf("argv 含 token：%q", a)
		}
	}
	if !contains(cmd.Args, "--no-auto-maintenance") {
		t.Errorf("transport 的 fetch 应自动带 --no-auto-maintenance：%v", cmd.Args)
	}
	if strings.HasPrefix(dir, m.Root()) {
		t.Errorf("transport 目录 %s 不应位于工作区根 %s 之下", dir, m.Root())
	}
	if !strings.Contains(filepath.Base(dir), "lathe-git-transport-") {
		t.Errorf("transport 目录名 = %s，want lathe-git-transport-*", dir)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("transport 目录权限 = %v（%v），want 0700", fi.Mode().Perm(), err)
	}
	if len(cmd.ExtraFiles) != 1 {
		t.Fatalf("ExtraFiles = %d，want 1（token 管道）", len(cmd.ExtraFiles))
	}
	b, _ := io.ReadAll(cmd.ExtraFiles[0])
	if !strings.Contains(string(b), tok) {
		t.Error("token 应只经 ExtraFiles[0] 管道交给 helper")
	}
	cleanup()
	tr.close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("close 后 transport 目录应已删除：%v", err)
	}

	// 不带 token 的本地调用：不挂管道
	tr2, _ := m.openTransport(mirror, url, vcs.NewGitAuth("oauth2", tok))
	defer tr2.close()
	cmd2, cleanup2, err := tr2.command(ctx, false, "", "for-each-ref")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup2()
	if len(cmd2.ExtraFiles) != 0 {
		t.Errorf("本地调用不应挂 token 管道")
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// TestGitAuthOnlyInOneShotParams：auth 只进一次性入参 CreateParams，
// 不进被到处复制的 RepoConfig/ExecuteParams；嵌套打印也不出明文。
func TestGitAuthOnlyInOneShotParams(t *testing.T) {
	authType := reflect.TypeOf(vcs.GitAuth{})
	for _, typ := range []reflect.Type{reflect.TypeOf(RepoConfig{}), reflect.TypeOf(ExecuteParams{})} {
		for i := 0; i < typ.NumField(); i++ {
			if typ.Field(i).Type == authType {
				t.Errorf("%s.%s 的类型是 vcs.GitAuth：auth 不得进被到处复制的配置", typ.Name(), typ.Field(i).Name)
			}
		}
	}

	tok := randToken(t, "tok-")
	p := CreateParams{Repo: DefaultRepoConfig("g/x"), CloneURL: "https://h/g/x.git", Kind: KindFix,
		IssueKey: "CR-9", Title: "t", Auth: vcs.NewGitAuth("oauth2", tok)}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "params", p, "auth", p.Auth)
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "params", p, "auth", p.Auth)
	js, _ := json.Marshal(p)
	outs := []string{fmt.Sprintf("%v", p), fmt.Sprintf("%+v", p), fmt.Sprintf("%#v", p), fmt.Sprintf("%s", p.Auth),
		fmt.Sprintf("%+v", &p), buf.String(), string(js)}
	for i, o := range outs {
		if strings.Contains(o, tok) {
			t.Errorf("第 %d 种输出泄露了 token：%s", i, o)
		}
	}
}

// TestGitHubRemoteUnchanged（runner 部分）：GitHub 仓库的键与地址与 cff5332 逐字节相同。
func TestGitHubRemoteUnchanged(t *testing.T) {
	cfg := DefaultRepoConfig("acme/demo")
	if got := cfg.RepoKey(); got != "acme/demo" {
		t.Errorf("RepoKey = %q，want acme/demo", got)
	}
	if got := cfg.CloneURL(); got != "git@github.com:acme/demo.git" {
		t.Errorf("CloneURL = %q，want git@github.com:acme/demo.git", got)
	}
	if got := cfg.Target(); got != (vcs.Target{Provider: vcs.ProviderGitHub}) {
		t.Errorf("Target = %+v，want {github, \"\"}", got)
	}
	if got := vcs.RepoKey(vcs.ProviderGitHub, "", "acme/demo"); got != "acme/demo" {
		t.Errorf("vcs.RepoKey(github) = %q", got)
	}
	if got := vcs.CloneURL(vcs.ProviderGitHub, "", "acme/demo"); got != "git@github.com:acme/demo.git" {
		t.Errorf("vcs.CloneURL(github) = %q", got)
	}
	if !(vcs.GitAuth{}).IsZero() {
		t.Error("github 的 GitAuth 应为零值")
	}
	m := newManager(t)
	if got, want := m.MirrorPath(cfg.RepoKey()), filepath.Join(m.Root(), ".mirrors", "acme-demo.git"); got != want {
		t.Errorf("GitHub 的 mirror 路径 = %s，want %s（与 cff5332 相同）", got, want)
	}
}
