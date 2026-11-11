package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本节点没有这个驱动 / 能力不符：直接失败，不起进程（PRD §3.4-7，AC-21）。
func TestRouterDriverUnavailable(t *testing.T) {
	cases := []struct {
		name        string
		withCodex   bool
		p           RunParams
		wantErr     error
		wantContain string
	}{
		{
			name:    "给定只注册了 claude 的 Router 收到 Driver=codex 断言 ErrDriverUnavailable 且提示 LATHE_CODEX_HOME",
			p:       RunParams{Driver: DriverCodex, Prompt: "x"},
			wantErr: ErrDriverUnavailable, wantContain: "LATHE_CODEX_HOME",
		},
		{
			name:      "给定注册了 codex 但请求 FromPR 断言 ErrUnsupportedParam",
			withCodex: true,
			p:         RunParams{Driver: DriverCodex, Prompt: "x", FromPR: "1"},
			wantErr:   ErrUnsupportedParam, wantContain: "FromPR",
		},
		{
			name:    "给定未知驱动名 断言 ErrDriverUnavailable",
			p:       RunParams{Driver: "gpt", Prompt: "x"},
			wantErr: ErrDriverUnavailable, wantContain: "gpt",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "started")
			script := fmt.Sprintf("touch %s\necho '%s'\n", marker, resultOKLine)
			claude := NewDriver(fakeClaude(t, script), time.Minute)
			var codex Runner
			if tc.withCodex {
				codex = newTestCodex(t, fakeCodex(t, script), time.Minute)
			}
			tc.p.Dir = t.TempDir()
			_, err := NewRouter(claude, codex).Run(context.Background(), tc.p)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %v want %v", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantContain) {
				t.Errorf("错误文本应含 %q，got %v", tc.wantContain, err)
			}
			if _, serr := os.Stat(marker); serr == nil {
				t.Error("不应起任何进程（标记文件不该存在）")
			}
		})
	}

	t.Run("给定 Driver 为空 断言分给 claude", func(t *testing.T) {
		claude := NewDriver(fakeClaude(t, fmt.Sprintf("echo '%s'\n", resultOKLine)), time.Minute)
		res, err := NewRouter(claude, nil).Run(context.Background(), RunParams{Prompt: "x", SessionID: "s"})
		if err != nil || res == nil || !res.Success {
			t.Fatalf("空驱动应走 claude，got %+v %v", res, err)
		}
	})
}

func TestDriverHelpers(t *testing.T) {
	cases := []struct {
		driver, skills string
		subagent       bool
		fromPR         bool
	}{
		{"", ".claude/skills", true, true},
		{DriverClaude, ".claude/skills", true, true},
		{DriverCodex, ".codex/skills", false, false},
	}
	for _, tc := range cases {
		t.Run("driver="+tc.driver, func(t *testing.T) {
			if got := SkillsDir(tc.driver); got != tc.skills {
				t.Errorf("SkillsDir got %q want %q", got, tc.skills)
			}
			got := SubagentDirFor(tc.driver, "/w", "s")
			if want := SubagentDir(ProjectsRoot(), "/w", "s"); tc.subagent && got != want {
				t.Errorf("SubagentDirFor got %q want %q", got, want)
			}
			if !tc.subagent && got != "" {
				t.Errorf("SubagentDirFor got %q want 空串", got)
			}
			if CapabilitiesOf(tc.driver).FromPR != tc.fromPR {
				t.Errorf("CapabilitiesOf(%q).FromPR got %v want %v", tc.driver, !tc.fromPR, tc.fromPR)
			}
		})
	}
}

func TestCheckCodexHomeNotInTempDir(t *testing.T) {
	root := t.TempDir()
	tmp := filepath.Join(root, "tmp")
	for _, d := range []string{filepath.Join(tmp, "ch"), filepath.Join(root, "tmpx", "ch"), filepath.Join(root, "other")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "other", "link")
	if err := os.Symlink(filepath.Join(tmp, "ch"), link); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, home string
		wantErr    bool
	}{
		{"给定在临时目录之下 断言拒绝", filepath.Join(tmp, "ch"), true},
		{"给定等于临时目录 断言拒绝", tmp, true},
		{"给定软链接指向临时目录之下 断言拒绝", link, true},
		{"给定前缀相同的兄弟目录 断言放行", filepath.Join(root, "tmpx", "ch"), false},
		{"给定临时目录之外 断言放行", filepath.Join(root, "other"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckCodexHomeNotInTempDir(tc.home, tmp)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err got %v wantErr %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "Refusing to create helper binaries") {
				t.Errorf("错误应带 codex 原话: %v", err)
			}
		})
	}
}
