package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/zichuanwangcloud-gif/lathe/internal/creds"
	"github.com/zichuanwangcloud-gif/lathe/internal/secret"
	"github.com/zichuanwangcloud-gif/lathe/internal/vcs"
)

// AC-21：serve 里构造 prd.Service 的字面量带 GitAuth: prdGitAuth{…}（或 f09 合入后的
// buildPRDService）；prdGitAuth 经真实 creds.Factory 按属主解析，不串属主。
func TestPRDGitAuthWiring(t *testing.T) {
	t.Run("装配：prd.Service 字面量含 GitAuth: prdGitAuth{…}", func(t *testing.T) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "main.go", nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		found, wired := false, false
		ast.Inspect(f, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := cl.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Service" {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "prd" {
				return true
			}
			found = true
			for _, e := range cl.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "GitAuth" {
					if v, ok := kv.Value.(*ast.CompositeLit); ok {
						if id, ok := v.Type.(*ast.Ident); ok && id.Name == "prdGitAuth" {
							wired = true
						}
					}
				}
			}
			return true
		})
		if !found {
			t.Fatal("main.go 里找不到 prd.Service 字面量")
		}
		if !wired {
			t.Error("prd.Service 字面量缺 GitAuth: prdGitAuth{…}：GitLab/Gitea 仓库的规划检出会直接报错")
		}
	})

	t.Run("运行：按属主取各自的令牌", func(t *testing.T) {
		st := testStore(t)
		key := make([]byte, secret.KeySize)
		for i := range key {
			key[i] = byte(i + 7)
		}
		sealer, err := secret.New(key)
		if err != nil {
			t.Fatal(err)
		}
		secrets := st.NewSecrets(sealer)
		userA, _ := fixture(t, st)
		userB, _ := fixture(t, st)
		ctx := context.Background()
		base := "https://gitlab.example.com"
		tokA, tokB := uniqueKey("glpat-a"), uniqueKey("glpat-b")
		if err := secrets.SaveWithBaseURL(ctx, userA, "gitlab", tokA, base); err != nil {
			t.Fatal(err)
		}
		if err := secrets.SaveWithBaseURL(ctx, userB, "gitlab", tokB, base); err != nil {
			t.Fatal(err)
		}
		ga := prdGitAuth{f: creds.NewFactory(secrets, creds.EnvFallback{}, 0, st)}
		target := vcs.Target{Provider: vcs.ProviderGitLab, BaseURL: base}
		for _, c := range []struct {
			user int64
			want string
		}{{userA, tokA}, {userB, tokB}} {
			auth, err := ga.GitAuth(ctx, c.user, target)
			if err != nil {
				t.Fatalf("GitAuth(%d): %v", c.user, err)
			}
			if got := auth.RedactValues(); len(got) != 1 || got[0] != c.want {
				t.Errorf("用户 %d 拿到的令牌不是自己的", c.user)
			}
		}
		if _, err := ga.GitAuth(ctx, userA, vcs.Target{Provider: vcs.ProviderGitLab, BaseURL: "https://other.example"}); err == nil {
			t.Error("实例不一致应报错")
		}
	})
}
