package main

// notify_wiring_test.go 钉住站外通知（f09）的装配：
//
//   - serve 里 NewNotifyChannels 只有一处，白名单传的是 notify.AllowedHosts；
//   - buildPipeline / buildPRDService 把同一个 *store.NotifyChannels 接到
//     Pipeline.Chat 与 prd.Service.ReviewNotifier（==，同一指针）；
//   - startWorkers 有且仅有一处 notify.NewWorker 与一处 go notifyWorker.Run；
//   - cmd/lathe 只经 NewProductionSender 构造发送器，生产不设 Worker 的测试旋钮。

import (
	"crypto/rand"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/config"
	"github.com/zichuanwangcloud-gif/lathe/internal/notify"
	"github.com/zichuanwangcloud-gif/lathe/internal/secret"
	"github.com/zichuanwangcloud-gif/lathe/internal/store"
)

func parseMainFunc(t *testing.T, name string) *ast.FuncDecl {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("解析 main.go 失败: %v", err)
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == name {
			return fd
		}
	}
	t.Fatalf("main.go 里找不到 %s", name)
	return nil
}

func TestNotifyWiring(t *testing.T) {
	t.Run("serve 里 NewNotifyChannels 只有一处且白名单是 notify.AllowedHosts", func(t *testing.T) {
		var calls []*ast.CallExpr
		ast.Inspect(parseMainFunc(t, "serve").Body, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok && strings.HasSuffix(callName(c.Fun), ".NewNotifyChannels") {
				calls = append(calls, c)
			}
			return true
		})
		if len(calls) != 1 {
			t.Fatalf("NewNotifyChannels 调用 got %d 处, want 1", len(calls))
		}
		if len(calls[0].Args) != 2 || callName(calls[0].Args[1]) != "notify.AllowedHosts" {
			t.Errorf("第二个实参 got %v, want notify.AllowedHosts", calls[0].Args)
		}
	})

	t.Run("buildPipeline / buildPRDService 接的是同一个 NC", func(t *testing.T) {
		st := testStore(t)
		key := make([]byte, secret.KeySize)
		_, _ = rand.Read(key)
		sealer, err := secret.New(key)
		if err != nil {
			t.Fatal(err)
		}
		nc := st.NewNotifyChannels(sealer, notify.AllowedHosts)
		cfg := config.Config{WorkspaceRoot: t.TempDir(), ClaudeBin: "claude", LightSlots: 1, HeavySlots: 1}
		p, _, err := buildPipeline(cfg, st, testSecrets(t, st), nil, nc)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := p.Chat.(*store.NotifyChannels); !ok || got != nc {
			t.Errorf("Pipeline.Chat got %T(%p), want 同一个 *store.NotifyChannels(%p)", p.Chat, got, nc)
		}
		svc := buildPRDService(cfg, st, p.Worktrees, nil, nc)
		if got, ok := svc.ReviewNotifier.(*store.NotifyChannels); !ok || got != nc {
			t.Errorf("prd.Service.ReviewNotifier got %T(%p), want 同一个 NC(%p)", svc.ReviewNotifier, got, nc)
		}
		// nil NC 不得变成「非 nil 接口里装 nil 指针」：那样 chatTerminal 会在 nil 上调方法。
		p2, _, err := buildPipeline(cfg, st, testSecrets(t, st), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if p2.Chat != nil {
			t.Errorf("nc=nil 时 Pipeline.Chat got %T, want nil 接口", p2.Chat)
		}
		if s2 := buildPRDService(cfg, st, p.Worktrees, nil, nil); s2.ReviewNotifier != nil {
			t.Errorf("nc=nil 时 ReviewNotifier got %T, want nil 接口", s2.ReviewNotifier)
		}
	})

	t.Run("startWorkers 恰好一处 notify.NewWorker 与一处 go notifyWorker.Run", func(t *testing.T) {
		var newWorker, runs int
		ast.Inspect(parseMainFunc(t, "startWorkers").Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CallExpr:
				if callName(v.Fun) == "notify.NewWorker" {
					newWorker++
				}
			case *ast.GoStmt:
				if callName(v.Call.Fun) == "notifyWorker.Run" {
					runs++
				}
			}
			return true
		})
		if newWorker != 1 || runs != 1 {
			t.Errorf("got NewWorker=%d go Run=%d, want 1/1", newWorker, runs)
		}
	})

	t.Run("生产代码不设 Worker 的测试旋钮", func(t *testing.T) {
		for _, src := range cmdSources(t) {
			for _, bad := range []string{".Now =", ".Lease ="} {
				if strings.Contains(src.body, bad) {
					t.Errorf("%s 出现 %q：Worker.Now/Lease 是测试旋钮，生产不设", src.name, bad)
				}
			}
		}
	})
}

type cmdSource struct{ name, body string }

func cmdSources(t *testing.T) []cmdSource {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []cmdSource
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, cmdSource{f, string(b)})
	}
	return out
}

// AC-10b 的装配半：cmd/lathe 只经 NewProductionSender 构造发送器（超时与不跟随重定向由它保证）。
func TestCmdUsesProductionSender(t *testing.T) {
	prod := 0
	for _, src := range cmdSources(t) {
		if strings.Contains(src.body, "notify.NewSender(") {
			t.Errorf("%s 出现 notify.NewSender(，want 只用 notify.NewProductionSender()", src.name)
		}
		prod += strings.Count(src.body, "notify.NewProductionSender(")
	}
	if prod == 0 {
		t.Error("cmd/lathe 里没有 notify.NewProductionSender( 调用")
	}
}
