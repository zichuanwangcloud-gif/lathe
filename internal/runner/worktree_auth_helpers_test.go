package runner

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/vcs"
)

// 本文件是带鉴权 git（transport）测试的公共夹具：一个真实的 git smart-http
// 远端（git http-backend 作 CGI，挂在 httptest 上，要求 Basic 鉴权），一个
// 只记录连接数的「agent 代理」，以及按平台构造的仓库配置。git-http-backend
// 随 git 安装（/usr/lib/git-core），不是新依赖；不可用时 t.Skipf。

const gitHTTPBackend = "/usr/lib/git-core/git-http-backend"

func randToken(t *testing.T, prefix string) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return prefix + hex.EncodeToString(b)
}

// gitHTTPStub 是要求 Basic 鉴权的 git smart-http 远端。
type gitHTTPStub struct {
	srv    *httptest.Server
	root   string // GIT_PROJECT_ROOT
	prefix string // 路径前缀（如 /gitlab），空表示无
	base   string // 实例地址（含前缀）

	mu         sync.Mutex
	tokens     map[string]bool // 接受的口令
	authSeen   []string        // 收到的 "user:pass"
	unauth     int             // 无鉴权/错误鉴权的请求数
	uploadPack []int64         // 每次 git-upload-pack 响应体的字节数
}

func newGitHTTPStub(t *testing.T, prefix string, useTLS bool, tokens ...string) *gitHTTPStub {
	t.Helper()
	if _, err := os.Stat(gitHTTPBackend); err != nil {
		t.Skipf("本机没有 %s，跳过带鉴权 git 的端到端测试", gitHTTPBackend)
	}
	s := &gitHTTPStub{root: t.TempDir(), prefix: prefix, tokens: map[string]bool{}}
	for _, tok := range tokens {
		s.tokens[tok] = true
	}
	backend := &cgi.Handler{
		Path: gitHTTPBackend,
		Root: prefix + "/",
		Env: []string{
			"GIT_PROJECT_ROOT=" + s.root,
			"GIT_HTTP_EXPORT_ALL=1",
			"REMOTE_USER=oauth2",
			"GIT_CONFIG_PARAMETERS='http.receivepack=true'",
		},
		InheritEnv: []string{"PATH"},
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		s.mu.Lock()
		if ok {
			s.authSeen = append(s.authSeen, user+":"+pass)
		}
		accepted := ok && s.tokens[pass]
		if !accepted {
			s.unauth++
		}
		s.mu.Unlock()
		if !accepted {
			w.Header().Set("WWW-Authenticate", `Basic realm="stub"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !strings.HasPrefix(r.URL.Path, prefix+"/") {
			http.NotFound(w, r)
			return
		}
		cw := &countingWriter{ResponseWriter: w}
		backend.ServeHTTP(cw, r)
		if strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
			s.mu.Lock()
			s.uploadPack = append(s.uploadPack, cw.n)
			s.mu.Unlock()
		}
	})
	if useTLS {
		s.srv = httptest.NewTLSServer(h)
	} else {
		s.srv = httptest.NewServer(h)
	}
	t.Cleanup(s.srv.Close)
	s.base = s.srv.URL + prefix
	return s
}

type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(b []byte) (int, error) {
	n, err := c.ResponseWriter.Write(b)
	c.n += int64(n)
	return n, err
}

func (s *gitHTTPStub) sawAuth(user, pass string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, a := range s.authSeen {
		if a == user+":"+pass {
			n++
		}
	}
	return n
}

func (s *gitHTTPStub) uploadPackSizes() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.uploadPack...)
}

// bare 返回远端裸仓库路径。
func (s *gitHTTPStub) bare(repo string) string { return filepath.Join(s.root, repo+".git") }

// seedRepo 建远端裸仓库并从一个本地工作仓库推入 main/dev 与一个临时分支 tmp，
// 返回那个工作仓库（测试用它模拟「上游有人推了新提交」）。
func (s *gitHTTPStub) seedRepo(t *testing.T, repo string) string {
	t.Helper()
	bare := s.bare(repo)
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	gitOut(t, "", "init", "--quiet", "--bare", bare)
	src := sourceRepo(t) // main + dev
	gitOut(t, src, "branch", "tmp")
	gitOut(t, src, "push", "--quiet", bare, "main", "dev", "tmp")
	return src
}

// upstreamCommit 在 src 的 branch 上改一个文件、提交并推到远端，返回新 tip。
func (s *gitHTTPStub) upstreamCommit(t *testing.T, src, repo, branch, file, content string) string {
	t.Helper()
	gitOut(t, src, "checkout", "-q", branch)
	if err := os.WriteFile(filepath.Join(src, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, src, "add", ".")
	gitOut(t, src, "-c", "user.email=t@e.st", "-c", "user.name=t", "commit", "-qm", "upstream "+file)
	gitOut(t, src, "push", "--quiet", s.bare(repo), branch)
	return gitOut(t, src, "rev-parse", "HEAD")
}

// remoteHeads 列出远端裸仓库 refs/heads/* → sha。
func remoteHeads(t *testing.T, bare string) map[string]string {
	t.Helper()
	return refMap(t, bare, "refs/heads/", "refs/heads/")
}

// mirrorRemotes 列出 mirror 的 refs/remotes/origin/* → sha（键去掉前缀）。
func mirrorRemotes(t *testing.T, mirror string) map[string]string {
	t.Helper()
	return refMap(t, mirror, "refs/remotes/origin/", "refs/remotes/origin/")
}

func refMap(t *testing.T, gitDir, pattern, strip string) map[string]string {
	t.Helper()
	out := gitOut(t, "", "--git-dir="+gitDir, "for-each-ref", "--format=%(objectname) %(refname)", pattern)
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		sha, ref, _ := strings.Cut(line, " ")
		m[strings.TrimPrefix(ref, strip)] = sha
	}
	return m
}

// connCounter 是「agent 代理」：只数连接，不转发。
type connCounter struct {
	ln net.Listener
	n  atomic.Int32
}

func newConnCounter(t *testing.T) *connCounter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &connCounter{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			c.n.Add(1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return c
}

func (c *connCounter) url() string { return "http://" + c.ln.Addr().String() }

// authRepo 是指向桩远端的 GitLab 仓库配置。
func authRepo(s *gitHTTPStub, repo string) RepoConfig {
	cfg := DefaultRepoConfig(repo)
	cfg.VCSProvider = vcs.ProviderGitLab
	cfg.VCSBaseURL = s.base
	return cfg
}

// commitIn 在工作区里改一个文件并提交，返回新 HEAD。
func commitIn(t *testing.T, dir, file, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", ".")
	gitOut(t, dir, "-c", "user.email=t@e.st", "-c", "user.name=t", "commit", "-qm", "change "+file)
	return gitOut(t, dir, "rev-parse", "HEAD")
}

// gitRun 跑 git 并返回 (输出, 错误)，不因失败终止测试。
func gitRun(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
