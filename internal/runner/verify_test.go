package runner

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetectLightProfileGoOnly(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module x\n\ngo 1.25\n")

	steps, err := DetectLightProfile(root)
	if err != nil {
		t.Fatalf("DetectLightProfile 失败: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("应得到 build + lint 两步，得到 %d: %+v", len(steps), steps)
	}
	if strings.Join(steps[0].Cmd, " ") != "go build -buildvcs=false ./..." {
		t.Errorf("首步应为 go build -buildvcs=false（worktree 里 VCS 戳记会失败），得到 %v", steps[0].Cmd)
	}
	if strings.Join(steps[1].Cmd, " ") != "go vet ./..." {
		t.Errorf("次步应为 go vet，得到 %v", steps[1].Cmd)
	}
}

// monorepo：多个 go.mod 各跑一次；隐藏目录与默认排除目录必须跳过。
func TestDetectLightProfileMonorepo(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "apps/console/backend/go.mod"), "module a\n")
	writeFile(t, filepath.Join(root, "apps/guardrail/go.mod"), "module b\n")
	// 默认排除目录
	writeFile(t, filepath.Join(root, "vendor/evil/go.mod"), "module evil\n")
	writeFile(t, filepath.Join(root, "node_modules/pkg/go.mod"), "module pkg\n")
	// 隐藏目录：真实仓库里 .worktrees 藏着整份嵌套仓库副本，
	// 走进去会把验证步骤放大几十倍（曾在 /opt/CloudRouter 上实测到 53 步）
	writeFile(t, filepath.Join(root, ".worktrees/cr-1/apps/console/backend/go.mod"), "module nested\n")
	writeFile(t, filepath.Join(root, ".claude/x/go.mod"), "module c\n")

	steps, err := DetectLightProfile(root)
	if err != nil {
		t.Fatalf("DetectLightProfile 失败: %v", err)
	}

	dirs := map[string]bool{}
	for _, s := range steps {
		dirs[s.Dir] = true
	}
	for _, want := range []string{"apps/console/backend", "apps/guardrail"} {
		if !dirs[want] {
			t.Errorf("应包含目录 %q，实际: %v", want, dirs)
		}
	}
	for _, bad := range []string{
		"vendor/evil", "node_modules/pkg",
		".worktrees/cr-1/apps/console/backend", ".claude/x",
	} {
		if dirs[bad] {
			t.Errorf("不应包含被排除的目录 %q", bad)
		}
	}
	if len(steps) != 4 { // 2 个模块 × (build + lint)
		t.Errorf("应恰好 4 步，得到 %d: %v", len(steps), dirs)
	}
}

// 仓库级的额外排除：CloudRouter 的 upstream/ 是只读上游镜像，不参与构建。
// 这属于仓库配置而非通用规则，故需显式传入。
func TestDetectLightProfileExtraExclude(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "apps/console/backend/go.mod"), "module a\n")
	writeFile(t, filepath.Join(root, "upstream/newapi/go.mod"), "module up\n")

	// 不排除时，upstream 会被扫到
	steps, err := DetectLightProfile(root)
	if err != nil {
		t.Fatalf("DetectLightProfile 失败: %v", err)
	}
	if len(steps) != 4 {
		t.Errorf("默认应扫到 2 个模块共 4 步，得到 %d", len(steps))
	}

	// 显式排除后只剩业务模块
	steps, err = DetectLightProfile(root, "upstream")
	if err != nil {
		t.Fatalf("DetectLightProfile 失败: %v", err)
	}
	for _, s := range steps {
		if strings.HasPrefix(s.Dir, "upstream") {
			t.Errorf("upstream 应被排除，仍出现: %s", s.Dir)
		}
	}
	if len(steps) != 2 {
		t.Errorf("排除后应剩 2 步，得到 %d", len(steps))
	}
}

func TestDetectLightProfileNode(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "package.json"), `{
	  "name": "x",
	  "scripts": {"build": "vite build", "lint": "eslint .", "test": "vitest"}
	}`)

	steps, err := DetectLightProfile(root)
	if err != nil {
		t.Fatalf("DetectLightProfile 失败: %v", err)
	}

	var joined []string
	for _, s := range steps {
		joined = append(joined, strings.Join(s.Cmd, " "))
	}
	all := strings.Join(joined, " | ")

	if !strings.Contains(all, "pnpm install") {
		t.Errorf("应先装依赖，实际: %s", all)
	}
	if !strings.Contains(all, "pnpm run build") {
		t.Errorf("应含 build，实际: %s", all)
	}
	if !strings.Contains(all, "pnpm run lint") {
		t.Errorf("应含 lint，实际: %s", all)
	}
	// package.json 里没有 typecheck script，就不该凭空加
	if strings.Contains(all, "typecheck") {
		t.Errorf("不存在的 script 不应被加入，实际: %s", all)
	}
	// test 不属于 light 档
	if strings.Contains(all, "run test") {
		t.Errorf("light 档不应跑测试，实际: %s", all)
	}
}

func TestDetectLightProfileEmpty(t *testing.T) {
	root := t.TempDir()
	if _, err := DetectLightProfile(root); err == nil {
		t.Error("既无 go.mod 也无 package.json 时应报错")
	}
}

func TestDetectLightProfileBadPackageJSON(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "package.json"), "{ 这不是合法 JSON ")
	if _, err := DetectLightProfile(root); err == nil {
		t.Error("package.json 不合法时应报错而非静默忽略")
	}
}

// ---------------------------------------------------------------- 执行

func TestRunLightAllPass(t *testing.T) {
	root := t.TempDir()
	v := NewVerifier(30*time.Second, "")

	rep := v.RunLight(context.Background(), root, []Step{
		{Name: StepBuild, Cmd: []string{"true"}},
		{Name: StepLint, Cmd: []string{"true"}},
	})

	if !rep.Passed() {
		t.Errorf("全部通过时 Passed() 应为 true: %s", rep.Summary())
	}
	if rep.FirstFailure() != nil {
		t.Errorf("不应有失败步骤: %+v", rep.FirstFailure())
	}
	if rep.Tier != TierLight {
		t.Errorf("Tier = %q", rep.Tier)
	}
}

// 首个失败后应早停，后续步骤记为 skipped。
func TestRunLightStopsAtFirstFailure(t *testing.T) {
	root := t.TempDir()
	v := NewVerifier(30*time.Second, "")

	rep := v.RunLight(context.Background(), root, []Step{
		{Name: StepBuild, Cmd: []string{"false"}},
		{Name: StepLint, Cmd: []string{"true"}},
		{Name: StepTypecheck, Cmd: []string{"true"}},
	})

	if rep.Passed() {
		t.Error("有失败步骤时 Passed() 应为 false")
	}
	if rep.Results[0].Status != StatusFailed {
		t.Errorf("首步应为 failed，得到 %s", rep.Results[0].Status)
	}
	for i := 1; i < len(rep.Results); i++ {
		if rep.Results[i].Status != StatusSkipped {
			t.Errorf("失败后第 %d 步应为 skipped，得到 %s", i, rep.Results[i].Status)
		}
	}

	f := rep.FirstFailure()
	if f == nil || f.Step.Name != StepBuild {
		t.Errorf("FirstFailure 应指向 build，得到 %+v", f)
	}
}

// 命令不存在属于 error（验证没跑起来），不是 failed（代码有问题）。
func TestRunStepMissingBinaryIsError(t *testing.T) {
	root := t.TempDir()
	v := NewVerifier(30*time.Second, "")

	rep := v.RunLight(context.Background(), root, []Step{
		{Name: StepBuild, Cmd: []string{"definitely-not-a-real-binary-xyz"}},
	})

	if got := rep.Results[0].Status; got != StatusError {
		t.Errorf("命令不存在应为 error，得到 %s", got)
	}
	if rep.Passed() {
		t.Error("error 不应算作通过 —— 没给出结论不等于没问题")
	}
}

func TestRunStepMissingDirIsError(t *testing.T) {
	root := t.TempDir()
	v := NewVerifier(30*time.Second, "")

	rep := v.RunLight(context.Background(), root, []Step{
		{Name: StepBuild, Cmd: []string{"true"}, Dir: "no/such/dir"},
	})
	if rep.Results[0].Status != StatusError {
		t.Errorf("目录不存在应为 error，得到 %s", rep.Results[0].Status)
	}
}

func TestRunStepEmptyCmdIsError(t *testing.T) {
	v := NewVerifier(time.Second, "")
	rep := v.RunLight(context.Background(), t.TempDir(), []Step{{Name: StepBuild}})
	if rep.Results[0].Status != StatusError {
		t.Errorf("空命令应为 error，得到 %s", rep.Results[0].Status)
	}
}

func TestRunStepCapturesOutput(t *testing.T) {
	root := t.TempDir()
	v := NewVerifier(30*time.Second, "")

	rep := v.RunLight(context.Background(), root, []Step{
		{Name: StepBuild, Cmd: []string{"sh", "-c", "echo 到标准输出; echo 到标准错误 >&2; exit 1"}},
	})

	out := rep.Results[0].Output
	if !strings.Contains(out, "到标准输出") || !strings.Contains(out, "到标准错误") {
		t.Errorf("stdout 与 stderr 都应被捕获，得到: %q", out)
	}
}

// ★验证步骤超时同样必须回收整棵进程树。
func TestRunStepTimeoutKillsProcessTree(t *testing.T) {
	root := t.TempDir()
	pidFile := filepath.Join(root, "child.pid")
	v := NewVerifier(800*time.Millisecond, "")

	start := time.Now()
	rep := v.RunLight(context.Background(), root, []Step{{
		Name: StepBuild,
		Cmd:  []string{"sh", "-c", "sleep 120 & echo $! > " + pidFile + "; sleep 120"},
	}})
	elapsed := time.Since(start)

	if rep.Results[0].Status != StatusError {
		t.Errorf("超时应记为 error，得到 %s", rep.Results[0].Status)
	}
	if elapsed > 10*time.Second {
		t.Errorf("超时后应迅速返回，实际 %v", elapsed)
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Skipf("未写出子进程 pid，跳过孤儿检查: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Skipf("pid 不可解析: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return // 已消失，符合预期
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("子进程 %d 在验证步骤超时后仍存活 —— 进程树未回收", pid)
}

// 共享 pnpm store 必须通过环境变量传给构建工具，否则每任务装一份依赖。
func TestVerifierSharesPnpmStore(t *testing.T) {
	v := NewVerifier(time.Minute, "/opt/lathe/.pnpm-store")
	env := strings.Join(v.stepEnv(), "\n")

	if !strings.Contains(env, "npm_config_store_dir=/opt/lathe/.pnpm-store") {
		t.Error("应设置 npm_config_store_dir 指向共享 store")
	}
	if !strings.Contains(env, "CI=1") {
		t.Error("应设置 CI=1 让构建工具走非交互模式")
	}

	// 未配置 store 时不应注入空值
	v2 := NewVerifier(time.Minute, "")
	if strings.Contains(strings.Join(v2.stepEnv(), "\n"), "npm_config_store_dir=") {
		t.Error("未配置 store 时不应注入 npm_config_store_dir")
	}
}

func TestReportSummary(t *testing.T) {
	rep := Report{
		Tier: TierLight,
		Results: []StepResult{
			{Step: Step{Name: StepBuild, Dir: "apps/console/backend"}, Status: StatusPassed, Duration: 1500 * time.Millisecond},
			{Step: Step{Name: StepLint}, Status: StatusFailed, Duration: 300 * time.Millisecond},
			{Step: Step{Name: StepTypecheck}, Status: StatusSkipped},
		},
	}

	s := rep.Summary()
	if !strings.Contains(s, "验证未通过") {
		t.Errorf("摘要应说明未通过: %s", s)
	}
	if !strings.Contains(s, "apps/console/backend") {
		t.Errorf("摘要应带上步骤所在目录: %s", s)
	}
	for _, mark := range []string{"✓", "✗", "-"} {
		if !strings.Contains(s, mark) {
			t.Errorf("摘要应含标记 %q: %s", mark, s)
		}
	}
}

// 全部 skipped 不能算通过 —— 那意味着什么都没验。
func TestReportAllSkippedIsNotPassed(t *testing.T) {
	rep := Report{Tier: TierLight, Results: []StepResult{
		{Step: Step{Name: StepBuild}, Status: StatusSkipped},
	}}
	if rep.Passed() {
		t.Error("全部 skipped 不应算通过")
	}

	empty := Report{Tier: TierLight}
	if empty.Passed() {
		t.Error("空报告不应算通过")
	}
}
