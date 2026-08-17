package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// heavy.go 实现 docs/02-design.md §5.3 的 heavy 档：红-绿复现证明。
//
// 这是整个产品的立足点：bug 类任务必须先在【改动前】的代码上复现失败，
// 才有权声称修复。一个不能先让测试失败的 agent，无权声称自己修好了 bug。
//
// 流程（§5.3）：
//  1. 先跑 light 步骤（构建/lint/类型）—— 编译都过不了不必谈复现
//  2. repro_fail：把复现测试拷到基线工作区跑 —— 必须失败
//     └─ 通过或跑不起来 ⇒ agent 没能理解这个 bug ⇒ 转 blocked_spec
//  3. repro_pass：同一测试在任务工作区跑 —— 必须通过
//  4. regression：受影响模块的既有测试 —— 必须通过
//
// per-task compose 隔离栈（§5.3 第 1 步）暂未实现：当前红绿两阶段都在
// git worktree 里跑，进程级隔离由验证器的超时与进程组回收保证。
// 待目标仓库声明服务栈后再补，见 docs/02-design.md §8。

// ErrNoReproTests 表示 diff 里识别不出可执行的复现测试。
//
// 这是 agent 没遵守输出契约（§5.3 要求随改动交复现/验收测试），属于
// 任务失败（D4），与「复现测试在旧代码上通过了 ⇒ 单子没说清 ⇒
// blocked_spec」是两种不同出口，流水线靠这个哨兵区分。
var ErrNoReproTests = errors.New(
	"runner: diff 中没有新增或修改的测试文件 —— heavy 档要求随改动提交复现/验收测试（§5.3），没有它红-绿证明无从谈起")

// ReproTest 是从 diff 里识别出的一条复现/验收测试。
//
// 复现脚本由 agent 自己写、随 PR 一起提交（§5.3）—— 顺手给仓库留下
// 一个回归测试。流水线不从 agent 的自然语言输出里解析路径，而是直接
// 从 diff 的文件清单里识别测试文件，这是确定性规则，不依赖模型守约定。
type ReproTest struct {
	// File 是测试文件相对工作区根的路径。
	File string
	// Cmd 是跑这条测试的命令。
	Cmd []string
	// Dir 相对于工作区根；空表示根目录。
	Dir string
}

// goTestFuncRe 提取 Go 测试函数名。
var goTestFuncRe = regexp.MustCompile(`(?m)^func\s+(Test\w+)\s*\(\s*\w+\s+\*testing\.T\s*\)`)

// IdentifyReproTests 从改动文件清单里识别可执行的复现测试。
//
// 只认「新增或修改过的测试文件」—— 它们就是 agent 为这单写下的
// 复现/验收证据。既有测试不算复现证据（它们在改动前本就该通过，
// 红阶段跑它们什么都证明不了）。
//
// 支持：
//   - Go：*_test.go，提取其中的 Test 函数精准运行
//   - 前端：*.test.* / *.spec.*，按 package.json 里的测试框架
//     （vitest | jest）构造单文件运行命令
//
// root 是任务工作区路径（读测试文件内容用）；files 是相对路径清单。
func IdentifyReproTests(root string, files []string) ([]ReproTest, error) {
	var out []ReproTest
	for _, f := range files {
		rel := filepath.ToSlash(f)
		base := filepath.Base(rel)

		switch {
		case strings.HasSuffix(base, "_test.go"):
			tests, err := goReproTests(root, rel)
			if err != nil {
				return nil, err
			}
			out = append(out, tests...)

		case isFrontendTestFile(base):
			rt, ok, err := frontendReproTest(root, rel)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, rt)
			}
		}
	}
	return out, nil
}

// goReproTests 为一个 Go 测试文件构造精准运行命令。
//
// 用 -run 锚定该文件里的 Test 函数，而不是跑整个包：基线代码上可能
// 存在与本单无关的既有失败，锚定后红绿判定只由这单的复现测试决定。
// 提取不到 Test 函数时退化为跑整个包（如只有 TestMain 的角落）。
func goReproTests(root, rel string) ([]ReproTest, error) {
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return nil, fmt.Errorf("runner: 读取测试文件 %s 失败: %w", rel, err)
	}

	names := goTestFuncRe.FindAllStringSubmatch(string(content), -1)
	seen := map[string]bool{}
	var uniq []string
	for _, n := range names {
		if !seen[n[1]] {
			seen[n[1]] = true
			uniq = append(uniq, n[1])
		}
	}
	sort.Strings(uniq)

	cmd := []string{"go", "test", "-count=1"}
	if len(uniq) > 0 {
		cmd = append(cmd, "-run", "^("+strings.Join(uniq, "|")+")$")
	}
	// pkg 必须是【模块相对】路径：命令在模块根执行（Dir=modDir）。
	// 用根相对路径会被拼到模块目录下 —— directory not found，红绿
	// 双阶段一起空转（任务 #466：红在 16ms 里假成立，绿同样秒挂）。
	modDir := goModuleDir(root, rel)
	pkgDir := filepath.ToSlash(filepath.Dir(rel))
	pkg := "."
	switch {
	case modDir == "":
		// 模块在仓库根：根相对即模块相对
		if pkgDir != "." {
			pkg = "./" + pkgDir
		}
	case pkgDir != modDir:
		pkg = "./" + strings.TrimPrefix(pkgDir, modDir+"/")
	}
	cmd = append(cmd, pkg)
	return []ReproTest{{File: rel, Cmd: cmd, Dir: modDir}}, nil
}

// goModuleDir 找测试文件所属 go.mod 所在目录（命令要在模块根跑）。
func goModuleDir(root, rel string) string {
	dir := filepath.Dir(filepath.Join(root, filepath.FromSlash(rel)))
	for strings.HasPrefix(dir, root) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if r, err := filepath.Rel(root, dir); err == nil && r != "." {
				return filepath.ToSlash(r)
			}
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

// isFrontendTestFile 报告文件名是否符合前端测试约定。
func isFrontendTestFile(base string) bool {
	return strings.Contains(base, ".test.") || strings.Contains(base, ".spec.")
}

// frontendReproTest 为前端测试文件构造命令。
//
// 框架从根 package.json 的依赖里识别：vitest 与 jest 的单文件运行
// 方式不同，猜错框架会把红绿判定变成"命令不存在"的噪声，因此识别
// 不出框架时宁缺毋滥 —— 返回 ok=false，由调用方按「无可用复现」处理。
func frontendReproTest(root, rel string) (ReproTest, bool, error) {
	pkg, err := readPackageJSON(filepath.Join(root, "package.json"))
	if err != nil {
		return ReproTest{}, false, err
	}
	if pkg == nil {
		return ReproTest{}, false, nil
	}

	deps := map[string]bool{}
	for k := range pkg.DevDependencies {
		deps[strings.ToLower(k)] = true
	}
	for k := range pkg.Dependencies {
		deps[strings.ToLower(k)] = true
	}

	switch {
	case deps["vitest"]:
		return ReproTest{File: rel, Cmd: []string{"pnpm", "exec", "vitest", "run", rel}}, true, nil
	case deps["jest"]:
		return ReproTest{File: rel, Cmd: []string{"pnpm", "exec", "jest", "--runTestsByPath", rel}}, true, nil
	}
	return ReproTest{}, false, nil
}

// packageJSON 是 package.json 里本包关心的字段。
type packageJSON struct {
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

// readPackageJSON 读取 package.json；文件不存在返回 (nil, nil)。
func readPackageJSON(path string) (*packageJSON, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("runner: 读取 %s 失败: %w", path, err)
	}
	var pkg packageJSON
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return nil, fmt.Errorf("runner: 解析 %s 失败: %w", path, err)
	}
	return &pkg, nil
}

// HeavyParams 是 heavy 档验证的输入。
type HeavyParams struct {
	// TaskPath 是任务工作区（改动后）。
	TaskPath string
	// BasePath 是基线工作区（改动前，detached HEAD）。
	BasePath string
	// Light 是先行的 light 档步骤（构建/lint/类型检查）。
	Light []Step
	// Repro 是识别出的复现测试；为空时 heavy 无法给出红绿证明。
	Repro []ReproTest
	// Regression 是受影响范围的回归步骤。
	Regression []Step
}

// RunHeavy 执行 heavy 档验证。红阶段不过（复现失败）时整体即失败，
// 后续步骤不再执行 —— 没有红就没有资格谈绿。
func (v *Verifier) RunHeavy(ctx context.Context, p HeavyParams) Report {
	rep := Report{Tier: TierHeavy}

	// 1. light 步骤：编译/静态检查先行，早停
	stopped := false
	for _, st := range p.Light {
		if stopped {
			rep.Results = append(rep.Results, StepResult{Step: st, Status: StatusSkipped})
			continue
		}
		res := v.runStep(ctx, p.TaskPath, st)
		rep.Results = append(rep.Results, res)
		if res.Status != StatusPassed {
			stopped = true
		}
	}
	if stopped {
		return rep
	}

	// 2. 红阶段：复现测试在【改动前】代码上必须失败
	if len(p.Repro) == 0 {
		rep.Results = append(rep.Results, StepResult{
			Step:   Step{Name: StepReproFail},
			Status: StatusError,
			Err:    ErrNoReproTests,
		})
		return rep
	}

	redRes := v.runReproOn(ctx, p.BasePath, p.TaskPath, p.Repro, true)
	rep.Results = append(rep.Results, redRes)
	if redRes.Status != StatusPassed {
		// 红没立起来：复现通过（bug 没复现）或跑不起来 ⇒ §5.3 转
		// blocked_spec，由流水线据此回帖，不再继续绿与回归
		return rep
	}

	// 3. 绿阶段：同一测试在改动后必须通过
	greenRes := v.runReproOn(ctx, p.TaskPath, p.TaskPath, p.Repro, false)
	rep.Results = append(rep.Results, greenRes)
	if greenRes.Status != StatusPassed {
		return rep
	}

	// 4. 回归：受影响范围的既有测试必须通过
	if len(p.Regression) == 0 {
		rep.Results = append(rep.Results, StepResult{
			Step:   Step{Name: StepRegression},
			Status: StatusSkipped,
		})
		return rep
	}
	for _, st := range p.Regression {
		st.Name = StepRegression
		res := v.runStep(ctx, p.TaskPath, st)
		rep.Results = append(rep.Results, res)
		if res.Status != StatusPassed {
			return rep
		}
	}
	return rep
}

// runReproOn 在指定工作区上执行全部复现测试，汇总为单一步骤结果。
//
// red=true 时期望测试【失败】（改动前的代码）：全部失败 ⇒ StatusPassed
// （红立起来了）；任何一条通过 ⇒ StatusFailed（bug 没复现）。
// red=false 时期望【通过】：全部通过 ⇒ StatusPassed。
//
// 红阶段会把测试文件的最新版本拷进基线工作区：基线上没有 agent 新写的
// 测试文件，必须带过去。编译错误（如测试引用了还不存在的函数）在红阶段
// 算「失败」—— 功能确实不存在（§5.4）。
func (v *Verifier) runReproOn(ctx context.Context, runRoot, srcRoot string, tests []ReproTest, red bool) StepResult {
	name := StepReproPass
	if red {
		name = StepReproFail
	}
	res := StepResult{Step: Step{Name: name}}

	if red && runRoot != srcRoot {
		for _, rt := range tests {
			if err := copyFile(
				filepath.Join(srcRoot, filepath.FromSlash(rt.File)),
				filepath.Join(runRoot, filepath.FromSlash(rt.File)),
			); err != nil {
				res.Status = StatusError
				res.Err = fmt.Errorf("runner: 把复现测试 %s 带进基线工作区失败: %w", rt.File, err)
				return res
			}
		}
	}

	var outputs strings.Builder
	var failed, passed int
	for _, rt := range tests {
		r := v.runStep(ctx, runRoot, Step{Name: name, Cmd: rt.Cmd, Dir: rt.Dir})
		fmt.Fprintf(&outputs, "$ %s  [%s]\n%s\n---\n",
			strings.Join(rt.Cmd, " "), rt.File, r.Output)
		res.Duration += r.Duration
		if r.Status == StatusError {
			res.Status = StatusError
			res.Err = fmt.Errorf("runner: 复现测试 %s 没能跑起来: %v", rt.File, r.Err)
			res.Output = truncate(outputs.String(), maxStepOutput)
			return res
		}
		if r.Status == StatusPassed {
			passed++
		} else {
			failed++
		}
	}
	res.Output = truncate(outputs.String(), maxStepOutput)

	if red {
		if passed > 0 {
			res.Status = StatusFailed
			res.Err = fmt.Errorf(
				"runner: %d/%d 条复现测试在改动前的代码上【通过】了 —— bug 没有复现，无法证明修复有效（§5.3：转 blocked_spec，请补充复现步骤）",
				passed, passed+failed)
			return res
		}
		res.Status = StatusPassed // 全部失败：红立起来了
		return res
	}

	if failed > 0 {
		res.Status = StatusFailed
		res.Err = fmt.Errorf("runner: %d/%d 条复现测试在改动后仍未通过", failed, passed+failed)
		return res
	}
	res.Status = StatusPassed
	return res
}

// DetectRegression 为受影响范围构造回归步骤。
//
// 范围按改动文件归属的【包】收敛：改动落在哪些 Go 包就跑哪些包的既有
// 测试（go.mod/go.sum 变更升级为全模块回归）。不按模块全量跑 —— 存量
// 坏测试在大型 monorepo 里是常态（任务 #479 挂在与改动无关的 config 包
// 存量失败上），全量回归等于让每个任务替历史还债。代价是跨包破坏不再
// 被回归覆盖：本单正确性由红-绿复现守住，全量测试留给仓库自己的 CI。
// 前端则以根 package.json 的 test script 为准（CI=1 下 vitest/jest 都会
// 单次运行后退出，不会挂 watch）。
func DetectRegression(root string, files []string, exclude ...string) []Step {
	var steps []Step

	goDirs, err := findGoModules(root, exclude)
	if err == nil {
		modPkgs := map[string]map[string]bool{} // 模块目录 → 被改动的包（模块相对）
		modAll := map[string]bool{}             // go.mod/go.sum 变更 → 全模块
		for _, f := range files {
			rel := filepath.ToSlash(f)
			if isExcludedPath(rel, exclude) {
				continue
			}
			mod, ok := owningGoModule(rel, goDirs)
			if !ok {
				continue
			}
			switch base := filepath.Base(rel); {
			case base == "go.mod" || base == "go.sum":
				modAll[mod] = true
			case strings.HasSuffix(base, ".go"):
				pkg := filepath.Dir(rel)
				switch {
				case pkg == mod:
					pkg = "."
				case mod != "":
					pkg = strings.TrimPrefix(pkg, mod+"/")
				}
				if modPkgs[mod] == nil {
					modPkgs[mod] = map[string]bool{}
				}
				modPkgs[mod][pkg] = true
			}
		}
		for _, mod := range goDirs {
			switch {
			case modAll[mod]:
				steps = append(steps, Step{
					Name: StepRegression,
					Cmd:  []string{"go", "test", "-count=1", "./..."},
					Dir:  mod,
				})
			case len(modPkgs[mod]) > 0:
				pkgs := make([]string, 0, len(modPkgs[mod]))
				for p := range modPkgs[mod] {
					pkgs = append(pkgs, p)
				}
				sort.Strings(pkgs)
				cmd := []string{"go", "test", "-count=1"}
				for _, p := range pkgs {
					if p == "." {
						cmd = append(cmd, ".")
					} else {
						cmd = append(cmd, "./"+p)
					}
				}
				steps = append(steps, Step{Name: StepRegression, Cmd: cmd, Dir: mod})
			}
		}
	}

	if scripts, err := readPackageScripts(filepath.Join(root, "package.json")); err == nil {
		if _, ok := scripts["test"]; ok && touchesFrontend(files) {
			steps = append(steps, Step{
				Name: StepRegression,
				Cmd:  []string{"pnpm", "test"},
			})
		}
	}
	return steps
}

// owningGoModule 返回拥有 rel 的模块目录（goDirs 中的最长前缀匹配）；
// 第二个返回值 false 表示该文件不属于任何 Go 模块。
func owningGoModule(rel string, goDirs []string) (string, bool) {
	best, found := "", false
	for _, d := range goDirs {
		if d == "" {
			if !found {
				best, found = "", true // 根模块兜底
			}
			continue
		}
		if rel == d || strings.HasPrefix(rel, d+"/") {
			if !found || len(d) > len(best) {
				best, found = d, true
			}
		}
	}
	return best, found
}

// isExcludedPath 按与 findGoModules 相同的规则判定路径是否被排除：
// 路径形式（含 /）按前缀匹配，纯目录名按任一路径段匹配。
func isExcludedPath(rel string, exclude []string) bool {
	for _, ex := range exclude {
		ex = strings.Trim(strings.TrimSpace(ex), "/")
		if ex == "" {
			continue
		}
		if strings.Contains(ex, "/") {
			if rel == ex || strings.HasPrefix(rel, ex+"/") {
				return true
			}
			continue
		}
		for _, seg := range strings.Split(rel, "/") {
			if seg == ex {
				return true
			}
		}
	}
	return false
}

// touchesFrontend 报告改动是否涉及前端源码（决定是否值得跑前端回归）。
func touchesFrontend(files []string) bool {
	for _, f := range files {
		switch strings.ToLower(filepath.Ext(f)) {
		case ".ts", ".tsx", ".js", ".jsx", ".vue", ".svelte":
			return true
		}
	}
	return false
}

// copyFile 复制单个文件，目标目录不存在时创建。
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, info.Mode().Perm())
}
