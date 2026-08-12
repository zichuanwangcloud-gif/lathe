package github

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestVerifySuccess(t *testing.T) {
	c := stubServer(t, map[string]http.HandlerFunc{
		"/user": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-OAuth-Scopes", "repo, read:org, gist")
			_, _ = w.Write([]byte(`{"login":"zichuan","name":"张子川"}`))
		},
	})

	acct, err := c.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify 失败: %v", err)
	}
	if acct.Login != "zichuan" || acct.Name != "张子川" {
		t.Errorf("账号信息不符: %+v", acct)
	}
	if len(acct.Scopes) != 3 || acct.Scopes[0] != "repo" {
		t.Errorf("权限解析不符: %v", acct.Scopes)
	}
}

// 缺 repo 权限时必须明确报出来 —— 否则要等到推分支那一刻才失败。
func TestVerifyDetectsMissingRepoScope(t *testing.T) {
	c := stubServer(t, map[string]http.HandlerFunc{
		"/user": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-OAuth-Scopes", "read:org, gist")
			_, _ = w.Write([]byte(`{"login":"zichuan"}`))
		},
	})

	acct, err := c.Verify(context.Background())
	if err == nil {
		t.Fatal("缺 repo 权限应报错")
	}
	if !strings.Contains(err.Error(), "repo") {
		t.Errorf("错误应指明缺少 repo 权限: %v", err)
	}
	// 即使权限不足也应返回账号信息，便于界面显示「配的是哪个账号」
	if acct == nil || acct.Login != "zichuan" {
		t.Errorf("应仍返回账号信息: %+v", acct)
	}
}

// 细粒度令牌不回报 scope 头，此时不应误判为权限不足。
func TestVerifySkipsScopeCheckForFineGrainedToken(t *testing.T) {
	c := stubServer(t, map[string]http.HandlerFunc{
		"/user": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"login":"zichuan"}`))
		},
	})

	acct, err := c.Verify(context.Background())
	if err != nil {
		t.Fatalf("无 scope 头时不应报错: %v", err)
	}
	if len(acct.Scopes) != 0 {
		t.Errorf("无 scope 头时权限应为空: %v", acct.Scopes)
	}
}

// 令牌无效与网络不通要给出不同提示 —— 处理方式完全不同。
func TestVerifyErrorMessages(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   string
	}{
		{"令牌无效", http.StatusUnauthorized, "重新签发"},
		{"被拒", http.StatusForbidden, "速率限制"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := stubServer(t, map[string]http.HandlerFunc{
				"/user": func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(`{"message":"nope"}`))
				},
			})
			_, err := c.Verify(context.Background())
			if err == nil {
				t.Fatal("应报错")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误提示应含 %q，得到: %v", tc.want, err)
			}
		})
	}
}

func TestHasRepoScope(t *testing.T) {
	yes := [][]string{{"repo"}, {"gist", "repo"}, {"public_repo"}}
	for _, s := range yes {
		if !hasRepoScope(s) {
			t.Errorf("%v 应判为有仓库权限", s)
		}
	}
	no := [][]string{{}, {"gist"}, {"read:org", "user"}, {"repo:status"}}
	for _, s := range no {
		if hasRepoScope(s) {
			t.Errorf("%v 不应判为有仓库权限", s)
		}
	}
}
