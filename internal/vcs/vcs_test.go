package vcs

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// AC-3：仓库标识按平台解析。GitHub 仍要求恰好两段；GitLab/Gitea 允许子组。
func TestSplitRefByProvider(t *testing.T) {
	long := strings.Repeat("a", 100) + "/" + strings.Repeat("b", 100) // 201
	cases := []struct {
		name     string
		provider Provider
		in       string
		wantNS   string
		wantName string
		wantErr  string // 非空：期望报错且文案含该子串
	}{
		{"github 两段 ok", ProviderGitHub, "acme/demo", "acme", "demo", ""},
		{"github 三段拒绝", ProviderGitHub, "a/b/c", "", "", "owner/name"},
		{"github 缺 owner", ProviderGitHub, "/b", "", "", "owner/name"},
		{"github 缺 name", ProviderGitHub, "a/", "", "", "owner/name"},
		{"github 空串", ProviderGitHub, "", "", "", "owner/name"},
		{"gitlab 两段 ok", ProviderGitLab, "acme/demo", "acme", "demo", ""},
		{"gitlab 子组 ok", ProviderGitLab, "group/sub/demo", "group/sub", "demo", ""},
		{"gitea 两段 ok", ProviderGitea, "acme/demo", "acme", "demo", ""},
		{"gitea 子组形态 ok", ProviderGitea, "group/sub/demo", "group/sub", "demo", ""},
		{"gitlab 拒绝 ..", ProviderGitLab, "..", "", "", "仓库路径"},
		{"gitlab 拒绝 ../ 段", ProviderGitLab, "a/../b", "", "", "仓库路径"},
		{"gitlab 拒绝空段", ProviderGitLab, "a//b", "", "", "仓库路径"},
		{"gitlab 拒绝 - 开头", ProviderGitLab, "-x/y", "", "", "仓库路径"},
		{"gitlab 拒绝 . 开头", ProviderGitLab, ".x/y", "", "", "仓库路径"},
		{"gitea 拒绝编码斜杠", ProviderGitea, "a%2fb/c", "", "", "仓库路径"},
		{"gitlab 拒绝超长", ProviderGitLab, long, "", "", "仓库路径"},
		{"gitlab 拒绝单段", ProviderGitLab, "demo", "", "", "仓库路径"},
		{"未知平台", Provider("bitbucket"), "a/b", "", "", "代码平台"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ns, name, err := SplitRef(tc.provider, tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("SplitRef(%s, %q) err = %v，want 含 %q", tc.provider, tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SplitRef(%s, %q) err = %v，want nil", tc.provider, tc.in, err)
			}
			if ns != tc.wantNS || name != tc.wantName {
				t.Errorf("SplitRef(%s, %q) = (%q, %q)，want (%q, %q)", tc.provider, tc.in, ns, name, tc.wantNS, tc.wantName)
			}
		})
	}
}

func TestProviderValid(t *testing.T) {
	for _, tc := range []struct {
		p    Provider
		want bool
	}{{ProviderGitHub, true}, {ProviderGitLab, true}, {ProviderGitea, true}, {"", false}, {"bitbucket", false}, {"GitHub", false}} {
		if got := tc.p.Valid(); got != tc.want {
			t.Errorf("Provider(%q).Valid() = %v，want %v", tc.p, got, tc.want)
		}
	}
}

// AC-4：PR 正文与平台无关，期望与 github 包现有断言逐字一致。
func TestBuildPRBodyPlatformNeutral(t *testing.T) {
	body := BuildPRBody("CR-1", "https://x", "验证摘要", "改动说明")
	for _, want := range []string{"关联 issue", "## 改动说明", "## 验证结果", "**合并前请人工复核。**", "[CR-1](https://x)"} {
		if !strings.Contains(body, want) {
			t.Errorf("BuildPRBody 输出应含 %q\n实际:\n%s", want, body)
		}
	}
	want := "关联 issue: [CR-1](https://x)\n\n## 改动说明\n\n改动说明\n\n## 验证结果\n\n```\n验证摘要```\n\n" +
		"---\n由 [Lathe](https://github.com/zichuanwangcloud-gif/lathe) 自动生成。**合并前请人工复核。**\n"
	if body != want {
		t.Errorf("BuildPRBody 逐字不一致\ngot:  %q\nwant: %q", body, want)
	}
}

// AC-5（vcs 部分）：GitAuth 在各种格式化下都不出明文。
func TestGitAuthRedactedString(t *testing.T) {
	tok := fmt.Sprintf("tok-%d-secret", 424242)
	a := NewGitAuth("oauth2", tok)
	if a.IsZero() {
		t.Fatal("带 token 的 GitAuth 不应 IsZero")
	}
	type holder struct {
		Name string
		Auth GitAuth
	}
	h := holder{Name: "x", Auth: a}
	outs := map[string]string{
		"String": a.String(),
		"%v":     fmt.Sprintf("%v", a),
		"%+v":    fmt.Sprintf("%+v", a),
		"%#v":    fmt.Sprintf("%#v", a),
		"%s":     fmt.Sprintf("%s", a),
		"nested": fmt.Sprintf("%v %+v %#v", h, h, h),
	}
	js, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	outs["json"] = string(js)
	for k, v := range outs {
		if strings.Contains(v, tok) {
			t.Errorf("%s 输出泄露了 token: %s", k, v)
		}
	}
	if !(GitAuth{}).IsZero() {
		t.Error("零值 GitAuth 应 IsZero")
	}
	if got := a.RedactValues(); len(got) != 1 || got[0] != tok {
		t.Errorf("RedactValues = %v，want [token]", got)
	}
	if got := (GitAuth{}).RedactValues(); len(got) != 0 {
		t.Errorf("零值 RedactValues = %v，want 空", got)
	}
}

// AC-5（TestGitHubRemoteUnchanged 的 vcs 部分）+ GitLab/Gitea 的镜像键与克隆地址。
func TestRepoKeyAndCloneURL(t *testing.T) {
	cases := []struct {
		name      string
		p         Provider
		base      string
		repo      string
		wantKey   string
		wantClone string
	}{
		{"github 与 cff5332 逐字节相同", ProviderGitHub, "", "acme/demo", "acme/demo", "git@github.com:acme/demo.git"},
		{"gitlab 子组", ProviderGitLab, "https://gitlab.example.com", "group/sub/demo",
			"gitlab.example.com/group/sub/demo", "https://gitlab.example.com/group/sub/demo.git"},
		{"gitlab 带端口与路径前缀", ProviderGitLab, "http://127.0.0.1:8080/gitlab", "g/x",
			"127.0.0.1:8080/gitlab/g/x", "http://127.0.0.1:8080/gitlab/g/x.git"},
		{"gitea", ProviderGitea, "https://gitea.example.com", "acme/demo",
			"gitea.example.com/acme/demo", "https://gitea.example.com/acme/demo.git"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RepoKey(tc.p, tc.base, tc.repo); got != tc.wantKey {
				t.Errorf("RepoKey = %q，want %q", got, tc.wantKey)
			}
			if got := CloneURL(tc.p, tc.base, tc.repo); got != tc.wantClone {
				t.Errorf("CloneURL = %q，want %q", got, tc.wantClone)
			}
		})
	}
}

func TestNormalizeBaseURL(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"https://gitlab.example.com/", "https://gitlab.example.com", false},
		{"https://host/gitlab/", "https://host/gitlab", false},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080", false},
		{"http://localhost:3000", "http://localhost:3000", false},
		{"http://[::1]:3000", "http://[::1]:3000", false},
		{"http://evil.example.com", "", true},
		{"https://u:p@h", "", true},
		{"https://h?x=1", "", true},
		{"https://h#f", "", true},
		{"ftp://h", "", true},
		{"https://", "", true},
		{"", "", true},
		{"https://" + strings.Repeat("a", 300), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := NormalizeBaseURL(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("NormalizeBaseURL(%q) err = %v，wantErr %v", tc.in, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("NormalizeBaseURL(%q) = %q，want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestInstanceMismatchErrorNamesBothHosts(t *testing.T) {
	err := InstanceMismatch(ProviderGitLab, "https://a.example", "https://b.example")
	if !errors.Is(err, ErrInstanceMismatch) {
		t.Fatalf("应包裹 ErrInstanceMismatch: %v", err)
	}
	for _, want := range []string{"https://a.example", "https://b.example", "GitLab"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误文案应含 %q: %v", want, err)
		}
	}
}
