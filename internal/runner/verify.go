package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// VerifyTier 是验证档位（docs/02-design.md §5）。
type VerifyTier string

const (
	// TierLight 轻量档：构建 + lint + 类型检查，不起栈。
	TierLight VerifyTier = "light"
	// TierHeavy 重量档：起隔离栈跑红-绿复现证明（P1 实现）。
	TierHeavy VerifyTier = "heavy"
)

// StepName 是验证步骤名，与 verifications 表的 CHECK 约束一致。
type StepName string

const (
	StepBuild      StepName = "build"
	StepLint       StepName = "lint"
	StepTypecheck  StepName = "typecheck"
	StepReproFail  StepName = "repro_fail"
	StepReproPass  StepName = "repro_pass"
	StepRegression StepName = "regression"
)

// StepStatus 是步骤结果，与 verifications 表的 CHECK 约束一致。
type StepStatus string

const (
	// StatusPassed 步骤通过。
	StatusPassed StepStatus = "passed"
	// StatusFailed 步骤执行完毕但判定不通过（如编译报错）。
	StatusFailed StepStatus = "failed"
	// StatusSkipped 该仓库不适用此步骤。
	StatusSkipped StepStatus = "skipped"
	// StatusError 步骤本身没跑起来（命令不存在、超时等），与 failed 区分：
	// failed 是"代码有问题"，error 是"验证没能给出结论"。
	StatusError StepStatus = "error"
)

// Step 是一条待执行的验证命令。
type Step struct {
	Name StepName
	Cmd  []string
	// Dir 相对于工作区根目录；空表示根目录。
	Dir string
}

// StepResult 是一条验证步骤的执行结果。
type StepResult struct {
	Step     Step
	Status   StepStatus
	Output   string
	Duration time.Duration
	Err      error
}

// Report 是一次分级验证的汇总。
type Report struct {
	Tier    VerifyTier
	Results []StepResult
}

// Passed 报告本次验证是否整体通过。
//
// 只有全部非 skipped 步骤都是 passed 才算通过 —— error 同样不算通过，
// 因为"没能给出结论"不等于"没问题"。
func (r Report) Passed() bool {
	ran := 0
	for _, s := range r.Results {
		if s.Status == StatusSkipped {
			continue
		}
		ran++
		if s.Status != StatusPassed {
			return false
		}
	}
	return ran > 0
}

// FirstFailure 返回第一条未通过的步骤，便于回帖时说明失败在哪。
func (r Report) FirstFailure() *StepResult {
	for i := range r.Results {
		s := &r.Results[i]
		if s.Status == StatusFailed || s.Status == StatusError {
			return s
		}
	}
	return nil
}

// maxStepOutput 限制单步保留的输出长度，避免把整个构建日志灌进数据库。
const maxStepOutput = 16 << 10 // 16KB

// Verifier 执行验证步骤。
type Verifier struct {
	stepTimeout time.Duration
	pnpmStore   string
}

// NewVerifier 构造验证器。
func NewVerifier(stepTimeout time.Duration, pnpmStore string) *Verifier {
	if stepTimeout <= 0 {
		stepTimeout = 15 * time.Minute
	}
	return &Verifier{stepTimeout: stepTimeout, pnpmStore: pnpmStore}
}

// DefaultExcludeDirs 是扫描时默认跳过的目录名。
//
// 除此之外，所有以 "." 开头的隐藏目录一律跳过 —— 这是比逐个拉黑
// 更可靠的规则：可构建的模块不会放在隐藏目录里，而 .git / .worktrees /
// .claude / .emdash 这类目录可能藏着整份嵌套仓库副本，走进去会把
// 一个任务的验证步骤放大几十倍。
var DefaultExcludeDirs = []string{
	"node_modules", "vendor", "dist", "build", "target", "testdata",
}

// maxScanDepth 限制扫描深度，避免在异常目录结构上无限展开。
const maxScanDepth = 6

// DetectLightProfile 扫描工作区，推断出适用的 light 档步骤。
//
// 支持 monorepo：会找出所有 go.mod 所在目录各跑一次构建，
// 前端则依据根 package.json 里实际存在的 script 决定跑哪些。
//
// exclude 为额外要跳过的目录名（相对根的路径或纯目录名）；传 nil 用默认值。
// 例如 CloudRouter 的 upstream/ 是只读上游镜像，不参与构建，应由仓库配置排除。
func DetectLightProfile(root string, exclude ...string) ([]Step, error) {
	var steps []Step

	goDirs, err := findGoModules(root, exclude)
	if err != nil {
		return nil, err
	}
	for _, d := range goDirs {
		steps = append(steps,
			// -buildvcs=false：Lathe 的工作区都是 git worktree，go build 默认会
			// 尝试给二进制打 VCS 戳记，而在 worktree 里这一步常以
			// "error obtaining VCS status: exit status 128" 失败。验证只关心
			// 能不能编译，戳记既无关又脆弱。
			Step{Name: StepBuild, Cmd: []string{"go", "build", "-buildvcs=false", "./..."}, Dir: d},
			Step{Name: StepLint, Cmd: []string{"go", "vet", "./..."}, Dir: d},
		)
	}

	scripts, err := readPackageScripts(filepath.Join(root, "package.json"))
	if err != nil {
		return nil, err
	}
	if scripts != nil {
		// 依赖必须先装，且走共享 store —— 不能每任务装一份
		// （docs/00-analysis.md 风险 1）。install 不过滤：workspace
		// 链接依赖完整安装。
		steps = append(steps, Step{Name: StepBuild, Cmd: []string{"pnpm", "install", "--frozen-lockfile"}})

		// 仓库级排除同样要作用于前端脚本。根脚本通常是 pnpm -r <script>
		// 的递归包装，过滤器注不进去，所以有排除时绕过根脚本直接递归，
		// 把落在排除目录下的包逐个转成负向过滤器（pnpm 的 {dir} 选择器
		// 只认精确的包目录，没有子树语义，必须先枚举包）。
		negFilters := pnpmExcludeFilters(root, exclude)

		for _, s := range []struct {
			script string
			name   StepName
		}{
			{"build", StepBuild},
			{"lint", StepLint},
			{"typecheck", StepTypecheck},
		} {
			if _, ok := scripts[s.script]; !ok {
				continue
			}
			if len(negFilters) == 0 {
				steps = append(steps, Step{Name: s.name, Cmd: []string{"pnpm", "run", s.script}})
				continue
			}
			cmd := append([]string{"pnpm", "-r"}, negFilters...)
			cmd = append(cmd, "run", s.script)
			steps = append(steps, Step{Name: s.name, Cmd: cmd})
		}
	}

	if len(steps) == 0 {
		return nil, fmt.Errorf("runner: 在 %s 未识别出任何可执行的验证步骤（既无 go.mod 也无 package.json）", root)
	}
	return steps, nil
}

// RunLight 顺序执行 light 档步骤，遇到第一个失败即停止后续。
//
// 早停的理由：构建都没过就没必要再跑 lint，且能更快把失败回帖给人。
func (v *Verifier) RunLight(ctx context.Context, root string, steps []Step) Report {
	rep := Report{Tier: TierLight}

	stopped := false
	for _, st := range steps {
		if stopped {
			rep.Results = append(rep.Results, StepResult{Step: st, Status: StatusSkipped})
			continue
		}
		res := v.runStep(ctx, root, st)
		rep.Results = append(rep.Results, res)
		if res.Status != StatusPassed {
			stopped = true
		}
	}
	return rep
}

func (v *Verifier) runStep(ctx context.Context, root string, st Step) StepResult {
	res := StepResult{Step: st}
	if len(st.Cmd) == 0 {
		res.Status = StatusError
		res.Err = fmt.Errorf("runner: 步骤 %s 没有命令", st.Name)
		return res
	}

	dir := root
	if st.Dir != "" {
		dir = filepath.Join(root, st.Dir)
	}
	if _, err := os.Stat(dir); err != nil {
		res.Status = StatusError
		res.Err = fmt.Errorf("runner: 步骤 %s 的目录 %s 不存在: %w", st.Name, dir, err)
		return res
	}

	stepCtx, cancel := context.WithTimeout(ctx, v.stepTimeout)
	defer cancel()

	cmd := exec.Command(st.Cmd[0], st.Cmd[1:]...)
	cmd.Dir = dir
	cmd.Env = v.stepEnv()
	// 与 agent driver 同理：构建工具会派生子进程，用进程组保证能整棵杀掉
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	start := time.Now()
	if err := cmd.Start(); err != nil {
		res.Status = StatusError
		res.Err = fmt.Errorf("runner: 启动 %v 失败: %w", st.Cmd, err)
		res.Duration = time.Since(start)
		return res
	}
	pgid := cmd.Process.Pid

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		res.Duration = time.Since(start)
		res.Output = truncate(buf.String(), maxStepOutput)
		if err != nil {
			// 非零退出 = 代码有问题（failed），而非验证没跑起来（error）
			res.Status = StatusFailed
			res.Err = err
			return res
		}
		res.Status = StatusPassed
		return res

	case <-stepCtx.Done():
		killGroup(pgid)
		<-done // 等 Wait 收敛，避免僵尸
		res.Duration = time.Since(start)
		res.Output = truncate(buf.String(), maxStepOutput)
		res.Status = StatusError
		res.Err = fmt.Errorf("runner: 步骤 %s 超时（上限 %v），进程树已回收", st.Name, v.stepTimeout)
		return res
	}
}

func (v *Verifier) stepEnv() []string {
	env := append(os.Environ(),
		"CI=1",                  // 让构建工具走非交互模式
		"GIT_TERMINAL_PROMPT=0", // 构建脚本里若有 git 操作，不许弹交互
	)
	if v.pnpmStore != "" {
		// 共享依赖 store：所有任务复用同一份包缓存
		env = append(env, "PNPM_HOME="+v.pnpmStore, "npm_config_store_dir="+v.pnpmStore)
	}
	return env
}

func killGroup(pgid int) {
	if pgid <= 0 {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	time.Sleep(200 * time.Millisecond)
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// findGoModules 找出工作区内所有含 go.mod 的目录（相对路径）。
func findGoModules(root string, extraExclude []string) ([]string, error) {
	skip := make(map[string]bool, len(DefaultExcludeDirs)+len(extraExclude))
	for _, d := range DefaultExcludeDirs {
		skip[d] = true
	}
	for _, d := range extraExclude {
		if d = strings.Trim(strings.TrimSpace(d), "/"); d != "" {
			skip[d] = true
		}
	}

	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 单个目录读不了不应中断整次扫描
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}

		if d.IsDir() {
			if path == root {
				return nil
			}
			name := d.Name()
			// 隐藏目录一律跳过：.git / .worktrees / .claude / .emdash 等
			// 可能藏着嵌套的仓库副本
			if strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			if skip[name] || skip[filepath.ToSlash(rel)] {
				return filepath.SkipDir
			}
			if strings.Count(filepath.ToSlash(rel), "/")+1 > maxScanDepth {
				return filepath.SkipDir
			}
			return nil
		}

		if d.Name() != "go.mod" {
			return nil
		}
		modDir := filepath.Dir(rel)
		if modDir == "." {
			modDir = ""
		}
		dirs = append(dirs, filepath.ToSlash(modDir))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("runner: 扫描 go 模块失败: %w", err)
	}
	sort.Strings(dirs)
	return dirs, nil
}

// pnpmExcludeFilters 把落在排除目录下的 pnpm 工作区包枚出来，转成
// pnpm 负向过滤器（--filter=!{dir}）。{dir} 选择器只匹配精确的包目录、
// 没有子树语义，所以必须枚举到包这一级。只处理路径形式的排除项
// （含 /）；纯目录名形式对 pnpm 步骤不适用 —— 名字太泛误伤面大，
// Go 模块扫描那边两种形式都认。
func pnpmExcludeFilters(root string, exclude []string) []string {
	var filters []string
	for _, ex := range exclude {
		ex = strings.Trim(strings.TrimSpace(ex), "/")
		if ex == "" || !strings.Contains(ex, "/") {
			continue
		}
		base := filepath.Join(root, ex)
		_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // 单个目录读不了不中断
			}
			if d.IsDir() {
				if strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Name() != "package.json" {
				return nil
			}
			rel, relErr := filepath.Rel(root, filepath.Dir(path))
			if relErr != nil {
				return nil
			}
			filters = append(filters, "--filter=!{"+filepath.ToSlash(rel)+"}")
			return nil
		})
	}
	sort.Strings(filters)
	return filters
}

// readPackageScripts 读取 package.json 的 scripts 段；文件不存在返回 (nil, nil)。
func readPackageScripts(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("runner: 读取 %s 失败: %w", path, err)
	}

	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return nil, fmt.Errorf("runner: 解析 %s 失败: %w", path, err)
	}
	if pkg.Scripts == nil {
		pkg.Scripts = map[string]string{}
	}
	return pkg.Scripts, nil
}

// Summary 生成可回帖到 Linear 的验证结论摘要。
func (r Report) Summary() string {
	var b strings.Builder
	if r.Passed() {
		fmt.Fprintf(&b, "验证通过（%s 档）\n", r.Tier)
	} else {
		fmt.Fprintf(&b, "验证未通过（%s 档）\n", r.Tier)
	}
	for _, s := range r.Results {
		mark := map[StepStatus]string{
			StatusPassed: "✓", StatusFailed: "✗",
			StatusError: "!", StatusSkipped: "-",
		}[s.Status]
		loc := s.Step.Dir
		if loc == "" {
			loc = "."
		}
		fmt.Fprintf(&b, "  %s %s (%s) %v\n", mark, s.Step.Name, loc, s.Duration.Round(time.Millisecond))
	}
	return b.String()
}
