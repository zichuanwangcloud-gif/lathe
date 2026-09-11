package main

// assembly_test.go 钉住装配顺序（B1）。
//
// serve() 本身没有测试覆盖（要真 Postgres + 真 HTTP 端口），这正是当初
// 那个 data race 能活下来的原因：`go q.work(ctx)` 排在
// `pipeline.Stacks = verifyStacks{previewMgr}` 之前 —— main goroutine
// 无同步地写一个 worker goroutine 正在读的 interface 字段。
// `go test -race` 抓不到没有测试跑过的代码。
//
// 修法不是"加注释提醒"，而是让顺序由结构保证。本文件测的就是那个结构：
//
//  1. buildPipeline 返回时 Stacks 必然已装好 —— 拿不到一个"还没装"的
//     Pipeline，所以不存在"装配窗口"可言。
//  2. 后台执行者只有一个启动点（startWorkers），它在 serve 里排在
//     所有装配之后。
//
// 第 2 条靠源码检查断言：它是"顺序"这件事本身，运行时没法在不真起
// 服务的前提下观察到。用 go/parser 读 serve 的语句序列，比读字符串
// 稳（不受注释、换行、格式化影响）。

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/Clouditera/lathe/internal/config"
	"github.com/Clouditera/lathe/internal/runner"
)

// ★ B1 第一层：buildPipeline 返回的 Pipeline 必须已经装好 Stacks。
//
// 这是"结构保证"的核心：Stacks 在构造字面量里赋值，语言层面不可能
// 返回一个 Stacks 为 nil 的 Pipeline。之前它是 buildPipeline 之后
// 在 serve 里补赋值的，于是必然存在一段"已经能被 worker 读到、
// 但还没装"的时间。
//
// 如果 Stacks 为 nil，runHeavy 里 `p.Stacks != nil` 直接为假 →
// 整个起栈分支跳过 → **既不起栈也不留痕**：任务照常跑完、照常回帖
// 「验证通过」，隔离却从没生效过。这是最坏的一种失败：静默。
func TestBuildPipelineReturnsStacksAlreadyWired(t *testing.T) {
	st := testStore(t) // 连不上库就跳过（同本包其余测试）
	cfg := config.Config{
		WorkspaceRoot: t.TempDir(),
		ClaudeBin:     "claude",
		LightSlots:    1,
		HeavySlots:    1,
	}

	p, pm, err := buildPipeline(cfg, st, nil)
	if err != nil {
		t.Fatalf("buildPipeline 失败: %v", err)
	}
	if p.Stacks == nil {
		t.Fatal("buildPipeline 返回时 Stacks 必须已装好 —— 为 nil 会让 runHeavy 静默跳过整个起栈分支：既不起栈也不留痕")
	}
	if pm == nil {
		t.Fatal("buildPipeline 应把 preview.Manager 交回调用方（开机清扫与预览 API 要复用同一个实例）")
	}

	// 同一个 Manager 实例：验证隔离栈与预览环境用的是同一套 docker
	// 能力与同一组资源阈值，两个实例等于两套配置，迟早不一致。
	vs, ok := p.Stacks.(verifyStacks)
	if !ok {
		t.Fatalf("Stacks 应是 verifyStacks 适配器，得到 %T", p.Stacks)
	}
	if vs.m != pm {
		t.Error("Stacks 里的 Manager 必须与返回的是同一个实例（否则阈值/docker 配置两套）")
	}
}

// ★ B1 第二层：serve 里 startWorkers 必须排在所有装配之后。
//
// 具体要压住三条：
//   - startWorkers 在 buildPipeline 之后（Stacks 装好了才让 worker 读）
//   - startWorkers 在 q.Reconcile 之后（既有约束：避免同一任务被两边
//     同时捡起 —— Reconcile 刚把所有在途任务重新入队，那是最热的时刻）
//   - startWorkers 在 SweepVerifyStacks 之后（清扫完才让 worker 起栈，
//     否则清扫可能删掉 worker 刚起的栈）
//
// 也压住"只有一个启动点"：serve 里不该再出现裸的 `go q.work(...)` 或
// `go mergePoller.Run(...)` —— 那会绕过这个结构。
func TestServeStartsWorkersAfterEverythingIsWired(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("解析 main.go 失败: %v", err)
	}

	var serve *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "serve" {
			serve = fd
			break
		}
	}
	if serve == nil {
		t.Fatal("main.go 里找不到 serve 函数")
	}

	// 按源码位置记录每个关注调用的出现顺序
	type hit struct {
		name string
		pos  token.Pos
	}
	var hits []hit
	var goStmts []string

	ast.Inspect(serve.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.GoStmt:
			goStmts = append(goStmts, callName(node.Call.Fun))
		case *ast.CallExpr:
			switch name := callName(node.Fun); name {
			case "buildPipeline", "newQueue", "startWorkers",
				"q.Reconcile", "previewMgr.SweepVerifyStacks":
				hits = append(hits, hit{name, node.Pos()})
			}
		}
		return true
	})

	idx := func(name string) int {
		for i, h := range hits {
			if h.name == name {
				return i
			}
		}
		return -1
	}

	for _, name := range []string{
		"buildPipeline", "newQueue", "q.Reconcile",
		"previewMgr.SweepVerifyStacks", "startWorkers",
	} {
		if idx(name) < 0 {
			t.Fatalf("serve 里应调用 %s，实际调用序列：%v", name, hits)
		}
	}

	start := idx("startWorkers")
	for _, before := range []struct {
		name string
		why  string
	}{
		{"buildPipeline", "Stacks 必须装好之后才让 worker 读（否则 data race + 隔离静默失效）"},
		{"newQueue", "队列要先建出来"},
		{"q.Reconcile", "启动恢复必须在 worker 启动前完成，避免同一任务被两边同时捡起"},
		{"previewMgr.SweepVerifyStacks", "开机清扫要在 worker 起栈之前，否则可能删掉 worker 刚起的栈"},
	} {
		if i := idx(before.name); i > start {
			t.Errorf("%s 必须排在 startWorkers 之前（%s）；实际序列：%v", before.name, before.why, hits)
		}
	}

	// 只有一个启动点：serve 里不该再有裸的 go q.work / go mergePoller.Run
	for _, g := range goStmts {
		if strings.HasPrefix(g, "q.work") || strings.HasPrefix(g, "mergePoller.") {
			t.Errorf("serve 里不该直接 `go %s`：后台执行者只能由 startWorkers 启动，"+
				"那是「装配完成后才启动」这条约束的唯一承载点", g)
		}
	}
}

// startWorkers 必须真的把两条后台循环都起起来 —— 只起 worker 不起
// merge poller 会让 F4.1/F4.3 静默失效（PR 合并检测与后继链 rebase
// 跟进全停，任务永远停在 pr_open）。
func TestStartWorkersLaunchesBothLoops(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("解析 main.go 失败: %v", err)
	}

	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "startWorkers" {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatal("main.go 里找不到 startWorkers 函数")
	}

	var goStmts []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if g, ok := n.(*ast.GoStmt); ok {
			goStmts = append(goStmts, callName(g.Call.Fun))
		}
		return true
	})

	want := map[string]bool{"q.work": false, "mergePoller.Run": false}
	for _, g := range goStmts {
		for k := range want {
			if strings.HasPrefix(g, k) {
				want[k] = true
			}
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("startWorkers 应 `go %s(...)`，实际：%v", k, goStmts)
		}
	}
}

// callName 把 f / x.f / pkg.x.f 拍平成点分名字，便于比较。
func callName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		if base := callName(v.X); base != "" {
			return base + "." + v.Sel.Name
		}
		return v.Sel.Name
	}
	return ""
}

// 编译期确认适配器仍满足 runner 的窄接口（重构时最容易断的一环）。
var _ runner.VerifyStackUp = verifyStacks{}

// ---------------------------------------------------------- 适配器错误翻译

// ★ verifyStacks 适配器必须把 preview 的两种「起不来」翻成 runner 认得的
// 两个**不同**身份，否则 runner 的降级/判死判定就是瞎的。
//
// 最要紧的是 docker 不可用那一条：CheckResources 的 switch 第一分支就是
// !DockerOK → Allowed=false，与水位共用同一个 Allowed=false 出口。
// 若翻译时不先把它拎出来，runner 会走降级、日志写「资源水位不允许」——
// 把人引去调阈值，而真正该做的是把 docker 起来。
func TestVerifyStacksTranslatesErrorIdentities(t *testing.T) {
	st := testStore(t)
	cfg := config.Config{WorkspaceRoot: t.TempDir(), ClaudeBin: "claude"}
	_, pm, err := buildPipeline(cfg, st, nil)
	if err != nil {
		t.Fatalf("buildPipeline 失败: %v", err)
	}
	adapter := verifyStacks{pm}

	// 阈值调到 1%：CheckResources 必然 Allowed=false。docker 在测试机上
	// 可能有也可能没有 —— 两种都要给出正确的身份，所以按实测结果分派。
	pm.Thresholds = func(context.Context) (int, int, error) { return 1, 1, nil }

	rs, rerr := pm.CheckResources(context.Background())
	if rerr != nil {
		t.Skipf("测不出资源水位，跳过: %v", rerr)
	}

	_, err = adapter.UpVerifyStack(context.Background(), 1, []string{"postgres"})
	if err == nil {
		t.Fatal("水位/docker 不允许时应报错")
	}

	if rs.DockerOK {
		// docker 可用 → 水位那一条
		if !errors.Is(err, runner.ErrStackOverThreshold) {
			t.Errorf("水位超阈值应翻成 runner.ErrStackOverThreshold（runner 据此降级），得到 %v", err)
		}
		if errors.Is(err, runner.ErrStackDockerDown) {
			t.Errorf("水位问题不该翻成 docker 不可用，得到 %v", err)
		}
		return
	}

	// docker 不可用 → 必须是 docker 那一条，且**不能**是水位那一条
	if !errors.Is(err, runner.ErrStackDockerDown) {
		t.Errorf("docker 不可用应翻成 runner.ErrStackDockerDown，得到 %v", err)
	}
	if errors.Is(err, runner.ErrStackOverThreshold) {
		t.Errorf("docker 不可用绝不能翻成水位错误 —— 那会让 runner 悄悄降级并把人引去调阈值，得到 %v", err)
	}
}

// 没声明依赖时适配器必须返回真正的 nil 接口值，而不是包着 nil 指针的
// 接口值 —— 后者会让 runHeavy 的 `stack != nil` 意外为真，然后对 nil
// 指针调 StackEnv() 当场 panic（在验证主路径上）。
func TestVerifyStacksReturnsTrueNilWithoutInfra(t *testing.T) {
	st := testStore(t)
	cfg := config.Config{WorkspaceRoot: t.TempDir(), ClaudeBin: "claude"}
	_, pm, err := buildPipeline(cfg, st, nil)
	if err != nil {
		t.Fatalf("buildPipeline 失败: %v", err)
	}

	h, err := verifyStacks{pm}.UpVerifyStack(context.Background(), 1, nil)
	if err != nil {
		t.Fatalf("无依赖不该报错: %v", err)
	}
	if h != nil {
		t.Errorf("无依赖应返回真正的 nil 接口值，得到 %#v（会让调用方对 nil 指针调方法）", h)
	}
}
