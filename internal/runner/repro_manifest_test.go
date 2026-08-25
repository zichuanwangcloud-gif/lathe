package runner

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile 已在 verify_test.go 定义，本文件直接复用。

// 声明优先于猜测：根 package.json 识别不出框架时（monorepo 子包），
// 声明说了算 —— 任务 #596 的正解。
func TestResolveReproTests_ManifestTakesPrecedence(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "package.json"), `{"scripts":{},"devDependencies":{"nx":"latest"}}`)
	writeFile(t, filepath.Join(root, ReproManifestPath),
		`{"version":1,"tests":[{"file":"apps/web/src/x.spec.ts","cmd":["pnpm","--filter","web","exec","vitest","run","src/x.spec.ts"],"dir":"apps/web"}]}`)

	tests, err := ResolveReproTests(root, []string{"apps/web/src/x.spec.ts", "apps/web/src/x.ts"})
	if err != nil {
		t.Fatalf("合法声明不应报错: %v", err)
	}
	if len(tests) != 1 {
		t.Fatalf("应解析出 1 条复现测试，实际 %+v", tests)
	}
	rt := tests[0]
	if rt.File != "apps/web/src/x.spec.ts" || rt.Dir != "apps/web" {
		t.Errorf("声明应原样转成 ReproTest: %+v", rt)
	}
	if got := strings.Join(rt.Cmd, " "); got != "pnpm --filter web exec vitest run src/x.spec.ts" {
		t.Errorf("命令应原样保留，实际: %s", got)
	}
}

// 没提交声明时才回落启发式。
func TestResolveReproTests_HeuristicFallback(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module demo\n\ngo 1.25\n")
	writeFile(t, filepath.Join(root, "x_test.go"),
		"package main\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n")

	tests, err := ResolveReproTests(root, []string{"x_test.go"})
	if err != nil {
		t.Fatalf("IdentifyReproTests: %v", err)
	}
	if len(tests) != 1 || !strings.Contains(strings.Join(tests[0].Cmd, " "), "TestX") {
		t.Fatalf("无声明时应回落启发式识别: %+v", tests)
	}
}

// 声明存在但不合法 ⇒ 契约违例（ErrReproManifest），不悄悄回落启发式 ——
// 否则 agent 永远不知道自己的声明写错了。
func TestResolveReproTests_ManifestContractViolations(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
		changed  []string
	}{
		{"JSON 不合法", `{`, []string{"a_test.go"}},
		{"tests 为空", `{"version":1,"tests":[]}`, []string{"a_test.go"}},
		{"version 过高", `{"version":2,"tests":[{"file":"a_test.go","cmd":["go","test"]}]}`, []string{"a_test.go"}},
		{"file 不在 diff", `{"tests":[{"file":"other_test.go","cmd":["go","test"]}]}`, []string{"a_test.go"}},
		{"file 越出工作区", `{"tests":[{"file":"../x_test.go","cmd":["go","test"]}]}`, []string{"a_test.go"}},
		{"file 是绝对路径", `{"tests":[{"file":"/etc/passwd","cmd":["cat"]}]}`, []string{"a_test.go"}},
		{"cmd 缺失", `{"tests":[{"file":"a_test.go"}]}`, []string{"a_test.go"}},
		{"cmd 首元素为空", `{"tests":[{"file":"a_test.go","cmd":[" "]}]}`, []string{"a_test.go"}},
		{"dir 越出工作区", `{"tests":[{"file":"a_test.go","cmd":["go","test"],"dir":"../"}]}`, []string{"a_test.go"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, ReproManifestPath), tc.manifest)
			// 即便启发式本可识别（diff 里有 a_test.go），声明不合法也不回落
			writeFile(t, filepath.Join(root, "go.mod"), "module demo\n\ngo 1.25\n")
			writeFile(t, filepath.Join(root, "a_test.go"),
				"package main\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n")

			_, err := ResolveReproTests(root, tc.changed)
			if !errors.Is(err, ErrReproManifest) {
				t.Errorf("应报 ErrReproManifest，实际: %v", err)
			}
		})
	}
}

// 契约违例要能被路由判定认出（进修复回路，而非 blocked_spec/失败）。
func TestIsReproContractErr(t *testing.T) {
	if !isReproContractErr(ErrNoReproTests) {
		t.Error("ErrNoReproTests 是契约违例")
	}
	if !isReproContractErr(errors.Join(ErrReproManifest, errors.New("细节"))) {
		t.Error("包装后的 ErrReproManifest 仍应被认出")
	}
	if isReproContractErr(errors.New("exec: not found")) {
		t.Error("执行错误不是契约违例")
	}
	if isReproContractErr(nil) {
		t.Error("nil 不是契约违例")
	}
}
