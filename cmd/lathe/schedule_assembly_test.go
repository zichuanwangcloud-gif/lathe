package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// LATHE_SCHEDULER_ENABLED=false 时，startWorkers 的执行路径上不得出现
// `go scheduler.Run`（docs/features/scheduled-issue.md §3.10）。
//
// 与 assembly_test.go 的 B1 两条同一手法：startWorkers 起的是真 worker /
// 真 docker 依赖，单测里没法真跑它观察 goroutine，所以用源码结构断言——
// 每一条 `go scheduler.Run(...)` 都必须在 `if cfg.SchedulerEnabled()` 的
// Body（不是 Else）里，且该 if 的 Else 分支里有一次 slog.Warn。
func TestStartWorkersSchedulerGatedByConfig(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("解析 main.go 失败: %v", err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "startWorkers" {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatal("main.go 里找不到 startWorkers 函数")
	}

	isGate := func(e ast.Expr) bool {
		c, ok := e.(*ast.CallExpr)
		return ok && len(c.Args) == 0 && callName(c.Fun) == "cfg.SchedulerEnabled"
	}
	hasWarn := func(n ast.Node) bool {
		found := false
		ast.Inspect(n, func(x ast.Node) bool {
			if c, ok := x.(*ast.CallExpr); ok && callName(c.Fun) == "slog.Warn" {
				found = true
			}
			return true
		})
		return found
	}

	total, gated := 0, 0
	// 带着「当前所在的 if 链」往下走：只有处在 gate-if 的 Body 里才算受控
	var walk func(n ast.Node, inGateBody bool)
	walk = func(n ast.Node, inGateBody bool) {
		ast.Inspect(n, func(x ast.Node) bool {
			switch v := x.(type) {
			case *ast.IfStmt:
				if v.Init != nil {
					walk(v.Init, inGateBody)
				}
				gate := isGate(v.Cond)
				walk(v.Body, inGateBody || gate)
				if v.Else != nil {
					if gate && !hasWarn(v.Else) {
						t.Errorf("if cfg.SchedulerEnabled() 的 else 分支里应有 slog.Warn（关闭时留痕）")
					}
					// else 分支里不算受控：哪怕外层是 gate 的 else 也一样
					walk(v.Else, inGateBody && !gate)
				} else if gate {
					t.Errorf("if cfg.SchedulerEnabled() 必须有 else 分支并 slog.Warn")
				}
				return false
			case *ast.GoStmt:
				if callName(v.Call.Fun) == "scheduler.Run" {
					total++
					if inGateBody {
						gated++
					} else {
						t.Errorf("`go scheduler.Run` 位于 %s，不在 if cfg.SchedulerEnabled() 的 Body 里",
							fset.Position(v.Pos()))
					}
				}
			}
			return true
		})
	}
	walk(fn.Body, false)

	if total == 0 {
		t.Fatal("startWorkers 里找不到 `go scheduler.Run(...)`：调度器没有启动点")
	}
	if gated != total {
		t.Errorf("受开关控制的 go scheduler.Run: got %d/%d, want 全部", gated, total)
	}
}
