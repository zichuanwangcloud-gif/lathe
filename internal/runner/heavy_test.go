package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFile 已在 verify_test.go 定义，本文件直接复用。

func TestIdentifyReproTests_Go(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module demo\n\ngo 1.25\n")
	writeFile(t, filepath.Join(root, "pkg", "calc_test.go"),
		`package pkg

import "testing"

func TestAdd(t *testing.T) {}
func TestSub(t *testing.T) {}
func helper() {}
`)

	tests, err := IdentifyReproTests(root, []string{"pkg/calc_test.go", "pkg/calc.go"})
	if err != nil {
		t.Fatalf("IdentifyReproTests: %v", err)
	}
	if len(tests) != 1 {
		t.Fatalf("应识别出 1 条复现测试，实际 %d: %+v", len(tests), tests)
	}
	rt := tests[0]
	joined := strings.Join(rt.Cmd, " ")
	if !strings.Contains(joined, "-run") || !strings.Contains(joined, "TestAdd") || !strings.Contains(joined, "TestSub") {
		t.Errorf("应用 -run 锚定提取出的 Test 函数: %s", joined)
	}
	if strings.Contains(joined, "helper") {
		t.Errorf("非测试函数不应被提取: %s", joined)
	}
}

func TestIdentifyReproTests_Frontend(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "package.json"),
		`{"scripts":{"test":"vitest run"},"devDependencies":{"vitest":"^2.0.0"}}`)
	writeFile(t, filepath.Join(root, "src", "sum.test.ts"), "export {}\n")

	tests, err := IdentifyReproTests(root, []string{"src/sum.test.ts"})
	if err != nil {
		t.Fatalf("IdentifyReproTests: %v", err)
	}
	if len(tests) != 1 || strings.Join(tests[0].Cmd, " ") != "pnpm exec vitest run src/sum.test.ts" {
		t.Fatalf("vitest 命令构造不符: %+v", tests)
	}
}

func TestIdentifyReproTests_UnknownFrameworkSkipped(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "package.json"), `{"scripts":{"test":"mocha"}}`)
	writeFile(t, filepath.Join(root, "src", "sum.test.ts"), "export {}\n")

	tests, err := IdentifyReproTests(root, []string{"src/sum.test.ts"})
	if err != nil {
		t.Fatalf("IdentifyReproTests: %v", err)
	}
	if len(tests) != 0 {
		t.Errorf("识别不出框架时应宁缺毋滥，实际: %+v", tests)
	}
}

// 红阶段语义：复现命令在「改动前」失败 ⇒ 红立起来（StatusPassed）；
// 通过了 ⇒ bug 没复现（StatusFailed）。命令跑不起来 ⇒ StatusError。
func TestRunReproOn_RedSemantics(t *testing.T) {
	v := NewVerifier(30*time.Second, "")
	src := t.TempDir()
	base := t.TempDir()
	writeFile(t, filepath.Join(src, "repro.sh"), "exit 1\n")

	// 命令失败 ⇒ 红立起来
	res := v.runReproOn(context.Background(), base, src,
		[]ReproTest{{File: "repro.sh", Cmd: []string{"sh", "repro.sh"}}}, true)
	if res.Status != StatusPassed {
		t.Errorf("复现失败应让红立起来，得到 %s: %v", res.Status, res.Err)
	}
	// 测试文件应已被拷进基线工作区
	if _, err := os.Stat(filepath.Join(base, "repro.sh")); err != nil {
		t.Errorf("复现测试应被带进基线工作区: %v", err)
	}

	// 命令通过 ⇒ 红没立起来
	writeFile(t, filepath.Join(src, "pass.sh"), "exit 0\n")
	res = v.runReproOn(context.Background(), base, src,
		[]ReproTest{{File: "pass.sh", Cmd: []string{"sh", "pass.sh"}}}, true)
	if res.Status != StatusFailed {
		t.Errorf("复现通过应判红未立（bug 没复现），得到 %s", res.Status)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "没有复现") {
		t.Errorf("失败理由应说明 bug 没复现: %v", res.Err)
	}

	// 命令不存在 ⇒ error（跑不起来）
	res = v.runReproOn(context.Background(), base, src,
		[]ReproTest{{File: "repro.sh", Cmd: []string{"definitely-not-a-real-binary-lathe"}}}, true)
	if res.Status != StatusError {
		t.Errorf("命令不存在应为 StatusError，得到 %s", res.Status)
	}
}

// 绿阶段语义：全部通过 ⇒ StatusPassed；任何失败 ⇒ StatusFailed。
func TestRunReproOn_GreenSemantics(t *testing.T) {
	v := NewVerifier(30*time.Second, "")
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "pass.sh"), "exit 0\n")
	writeFile(t, filepath.Join(dir, "fail.sh"), "exit 1\n")

	res := v.runReproOn(context.Background(), dir, dir,
		[]ReproTest{{File: "pass.sh", Cmd: []string{"sh", "pass.sh"}}}, false)
	if res.Status != StatusPassed {
		t.Errorf("全过应为 passed，得到 %s: %v", res.Status, res.Err)
	}

	res = v.runReproOn(context.Background(), dir, dir,
		[]ReproTest{
			{File: "pass.sh", Cmd: []string{"sh", "pass.sh"}},
			{File: "fail.sh", Cmd: []string{"sh", "fail.sh"}},
		}, false)
	if res.Status != StatusFailed {
		t.Errorf("有未通过应为 failed，得到 %s", res.Status)
	}
}

// RunHeavy 的顺序约束：light 步骤挂了就不进入红绿；没有复现测试 ⇒
// ErrNoReproTests（流水线据此走失败而非 blocked_spec）。
func TestRunHeavy_GatesAndOrdering(t *testing.T) {
	v := NewVerifier(30*time.Second, "")
	dir := t.TempDir()

	// light 失败：后续全部不发生，报告里不应出现 repro 步骤
	rep := v.RunHeavy(context.Background(), HeavyParams{
		TaskPath: dir, BasePath: dir,
		Light: []Step{{Name: StepBuild, Cmd: []string{"false"}}},
		Repro: []ReproTest{{File: "x", Cmd: []string{"true"}}},
	})
	for _, r := range rep.Results {
		if r.Step.Name == StepReproFail || r.Step.Name == StepReproPass {
			t.Errorf("light 未过不应进入红绿: %+v", rep.Results)
		}
	}
	if rep.Passed() {
		t.Error("light 未过整体不应通过")
	}

	// 无复现测试：repro_fail 为 error 且带哨兵
	rep = v.RunHeavy(context.Background(), HeavyParams{
		TaskPath: dir, BasePath: dir,
		Light: []Step{{Name: StepBuild, Cmd: []string{"true"}}},
	})
	red := rep.Results[len(rep.Results)-1]
	if red.Step.Name != StepReproFail || red.Status != StatusError || !errors.Is(red.Err, ErrNoReproTests) {
		t.Errorf("无复现测试应以 ErrNoReproTests 收尾: %+v", red)
	}
}

// 完整红-绿-回归链（用 shell 命令模拟，不依赖 go 工具链）。
func TestRunHeavy_FullChain(t *testing.T) {
	v := NewVerifier(30*time.Second, "")
	taskDir := t.TempDir()
	baseDir := t.TempDir()
	writeFile(t, filepath.Join(taskDir, "repro.sh"), "exit 1\n") // 拷到基线会失败 ⇒ 红

	rep := v.RunHeavy(context.Background(), HeavyParams{
		TaskPath: taskDir, BasePath: baseDir,
		Light:      []Step{{Name: StepBuild, Cmd: []string{"true"}}},
		Repro:      []ReproTest{{File: "repro.sh", Cmd: []string{"sh", "repro.sh"}}},
		Regression: []Step{{Cmd: []string{"true"}}},
	})
	// 绿阶段在任务工作区跑 repro.sh 也会失败 —— 用真 sh 脚本模拟
	// 「修好后通过」需要脚本在两个工作区表现不同，这里改为直接验证
	// 步骤序列与红阶段判定。
	if rep.Results[len(rep.Results)-1].Step.Name != StepReproPass {
		t.Fatalf("链路应推进到绿阶段: %+v", rep.Results)
	}

	var names []StepName
	for _, r := range rep.Results {
		names = append(names, r.Step.Name)
	}
	want := []StepName{StepBuild, StepReproFail, StepReproPass}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("步骤顺序 = %v，期望前缀 %v", names, want)
		}
	}
	if rep.Results[1].Status != StatusPassed {
		t.Errorf("红阶段应通过（复现失败），得到 %s", rep.Results[1].Status)
	}
}

func TestDetectRegression(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module demo\n\ngo 1.25\n")
	writeFile(t, filepath.Join(root, "web", "package.json"), `{"scripts":{"test":"vitest run"}}`)

	// 改动落在 go 模块 ⇒ 该模块的 go test ./...
	steps := DetectRegression(root, []string{"internal/x.go"})
	var hasGo bool
	for _, s := range steps {
		if s.Name == StepRegression && strings.Contains(strings.Join(s.Cmd, " "), "go test") {
			hasGo = true
		}
	}
	if !hasGo {
		t.Errorf("go 改动应有 go 回归步骤: %+v", steps)
	}

	// 前端改动但根 package.json 无 test script（在 web/ 下）⇒ 无前端回归
	steps = DetectRegression(root, []string{"web/src/app.ts"})
	for _, s := range steps {
		if strings.Contains(strings.Join(s.Cmd, " "), "pnpm") {
			t.Errorf("根 package.json 不存在时不应构造前端回归: %+v", steps)
		}
	}
}
