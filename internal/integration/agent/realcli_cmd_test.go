package agent

import (
	"strings"
	"testing"
)

// AGENTS.md §9：AC-14 测试里起的每个子进程（git、claude --version）都要显式
// Dir + 白名单 env，不得继承测试进程的 cwd 与全量环境（其中可能有 provider 凭据）。
func TestIsolatedCommandSetsDirAndWhitelistedEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "f01-should-not-leak")
	t.Setenv("PATH", "/usr/bin:/bin")
	cases := []struct {
		name  string
		extra []string
	}{
		{"给定无额外变量：只有 PATH 与 HOME", nil},
		{"给定额外 git 身份变量：追加在白名单之后", []string{"GIT_AUTHOR_NAME=f01"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := isolatedCommand("/tmp/f01-repo", "/tmp/f01-home", tc.extra, "claude", "--version")
			if cmd.Dir != "/tmp/f01-repo" {
				t.Fatalf("Dir: got %q want %q", cmd.Dir, "/tmp/f01-repo")
			}
			want := append([]string{"PATH=/usr/bin:/bin", "HOME=/tmp/f01-home"}, tc.extra...)
			if strings.Join(cmd.Env, "\n") != strings.Join(want, "\n") {
				t.Fatalf("Env: got %q want %q", cmd.Env, want)
			}
			for _, kv := range cmd.Env {
				if strings.Contains(kv, "f01-should-not-leak") {
					t.Fatalf("Env 泄漏了凭据: %q", kv)
				}
			}
		})
	}
}
