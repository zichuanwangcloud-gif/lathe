package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// f06：LATHE_CODEX_HOME / LATHE_CODEX_BIN 的启动校验。非法值启动即报错，
// 不运行期静默降级；不设 LATHE_CODEX_HOME 时 codex 不启用、什么都不校验。
func TestLoadCodexConfig(t *testing.T) {
	root := t.TempDir()
	tmp := filepath.Join(root, "tmp")
	other := filepath.Join(root, "other")
	mk := func(dirs ...string) {
		for _, d := range dirs {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk(filepath.Join(tmp, "ch"), filepath.Join(other, "ch"), filepath.Join(root, "tmp-sibling", "ch"),
		filepath.Join(other, "fakehome", ".codex"))
	bin := filepath.Join(other, "bin", "codex")
	mk(filepath.Dir(bin))
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(other, "link")
	if err := os.Symlink(filepath.Join(tmp, "ch"), link); err != nil {
		t.Fatal(err)
	}
	sysTmp, err := os.MkdirTemp("/tmp", "f06-codex-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sysTmp) })

	cases := []struct {
		name string
		home string // LATHE_CODEX_HOME（空 = 不设）
		bin  string // LATHE_CODEX_BIN（空 = 用上面的合法假 codex）
		// sysTmpdir 为真时 TMPDIR 置空（os.TempDir 回落 /tmp），否则 TMPDIR=root/tmp
		sysTmpdir bool
		// wantErr 期望 Load 报错；wantContain 是错误文本必须包含的片段
		wantErr     bool
		wantContain []string
		wantEnabled bool
	}{
		{name: "给定相对路径 断言报错", home: "relative/dir", wantErr: true, wantContain: []string{"绝对路径"}},
		{name: "给定不存在的绝对路径 断言报错", home: filepath.Join(other, "nope"), wantErr: true, wantContain: []string{"LATHE_CODEX_HOME"}},
		{name: "给定等于 $HOME/.codex 断言报错", home: filepath.Join(other, "fakehome", ".codex"), wantErr: true, wantContain: []string{"个人目录"}},
		{name: "给定目录合法但 LATHE_CODEX_BIN 不存在 断言报错", home: filepath.Join(other, "ch"), bin: "/nonexistent/codex",
			wantErr: true, wantContain: []string{"LATHE_CODEX_BIN"}},
		{name: "给定不设 LATHE_CODEX_HOME 且 BIN 不存在 断言不报错且 codex 未启用", bin: "/nonexistent/codex"},
		{name: "给定在 TMPDIR 之下 断言报错且错误含变量名、临时目录与 codex 原话", home: filepath.Join(tmp, "ch"),
			wantErr: true, wantContain: []string{"LATHE_CODEX_HOME", tmp, "Refusing to create helper binaries"}},
		{name: "给定等于 TMPDIR 本身 断言报错", home: tmp, wantErr: true, wantContain: []string{"Refusing to create helper binaries"}},
		{name: "给定软链接指向 TMPDIR 之下 断言报错（EvalSymlinks 生效）", home: link, wantErr: true,
			wantContain: []string{"Refusing to create helper binaries"}},
		{name: "给定前缀相同的兄弟目录 断言不报错（按路径分量判断）", home: filepath.Join(root, "tmp-sibling", "ch"), wantEnabled: true},
		{name: "给定临时目录之外的合法目录 断言不报错（阳性对照）", home: filepath.Join(other, "ch"), wantEnabled: true},
		{name: "给定 TMPDIR 未设（回落 /tmp）且 home 在 /tmp 下 断言报错", home: sysTmp, sysTmpdir: true, wantErr: true,
			wantContain: []string{"Refusing to create helper binaries"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", filepath.Join(other, "fakehome"))
			if tc.sysTmpdir {
				t.Setenv("TMPDIR", "")
			} else {
				t.Setenv("TMPDIR", tmp)
			}
			t.Setenv("LATHE_CODEX_HOME", tc.home)
			b := tc.bin
			if b == "" {
				b = bin
			}
			t.Setenv("LATHE_CODEX_BIN", b)

			c, err := Load()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Load err got %v wantErr %v", err, tc.wantErr)
			}
			for _, w := range tc.wantContain {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("错误应含 %q，got %v", w, err)
				}
			}
			if err == nil && c.CodexEnabled() != tc.wantEnabled {
				t.Errorf("CodexEnabled got %v want %v", c.CodexEnabled(), tc.wantEnabled)
			}
		})
	}

	t.Run("给定默认环境 断言 LATHE_CODEX_BIN 默认 codex 且 codex 未启用", func(t *testing.T) {
		t.Setenv("LATHE_CODEX_HOME", "")
		t.Setenv("LATHE_CODEX_BIN", "")
		_ = os.Unsetenv("LATHE_CODEX_BIN")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.CodexBin != "codex" || c.CodexEnabled() {
			t.Errorf("got CodexBin=%q enabled=%v want codex/false", c.CodexBin, c.CodexEnabled())
		}
	})
}
