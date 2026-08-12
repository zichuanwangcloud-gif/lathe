package runner

import (
	"errors"
	"strings"
	"testing"
)

func TestBaseBranchByKind(t *testing.T) {
	c := DefaultRepoConfig("Clouditera/CloudRouter")

	cases := []struct {
		kind TaskKind
		want string
	}{
		{KindFix, "dev"},
		{KindFeature, "dev"},
		{KindHotfix, "main"}, // CLAUDE.md：hotfix 只能从 main 分叉
	}
	for _, tc := range cases {
		got, err := c.BaseBranch(tc.kind)
		if err != nil {
			t.Errorf("BaseBranch(%s) 报错: %v", tc.kind, err)
			continue
		}
		if got != tc.want {
			t.Errorf("BaseBranch(%s) = %q，期望 %q", tc.kind, got, tc.want)
		}
	}

	if _, err := c.BaseBranch(TaskKind("bogus")); err == nil {
		t.Error("未知任务类型应报错")
	}

	missing := RepoConfig{ProviderRepo: "x/y"}
	if _, err := missing.BaseBranch(KindFix); err == nil {
		t.Error("未配置默认分支时应报错")
	}
	if _, err := missing.BaseBranch(KindHotfix); err == nil {
		t.Error("未配置 hotfix 基线时应报错")
	}
}

// 受保护分支拦截 —— 产品边界「永不 push 受保护分支」的最后一道闸门。
func TestValidatePushTarget(t *testing.T) {
	c := DefaultRepoConfig("Clouditera/CloudRouter")

	blocked := []string{
		"dev", "test", "main",
		"DEV", "Main", // 大小写不敏感
		"refs/heads/dev", // 带 ref 前缀
		"  main  ",       // 带空白
	}
	for _, b := range blocked {
		err := c.ValidatePushTarget(b)
		if err == nil {
			t.Errorf("%q 应被拦截", b)
			continue
		}
		var pe ErrProtectedBranch
		if !errors.As(err, &pe) {
			t.Errorf("%q 的错误类型应为 ErrProtectedBranch，得到 %T", b, err)
		}
	}

	allowed := []string{
		"fix/cr-1326-portable-import",
		"feature/cr-1130-invoice",
		"hotfix/cr-999-crash",
		"development", // 只是前缀相同，不该被误伤
		"mainline",
	}
	for _, b := range allowed {
		if err := c.ValidatePushTarget(b); err != nil {
			t.Errorf("%q 不应被拦截: %v", b, err)
		}
	}

	if err := c.ValidatePushTarget(""); err == nil {
		t.Error("空分支名应报错")
	}
}

func TestBranchName(t *testing.T) {
	c := DefaultRepoConfig("Clouditera/CloudRouter")

	cases := []struct {
		name  string
		kind  TaskKind
		key   string
		title string
		want  string
	}{
		{
			name: "修bug", kind: KindFix, key: "CR-1326",
			title: "Portable import fails", want: "fix/cr-1326-portable-import-fails",
		},
		{
			name: "需求", kind: KindFeature, key: "CR-1130",
			title: "Invoice feature", want: "feature/cr-1130-invoice-feature",
		},
		{
			name: "紧急修复", kind: KindHotfix, key: "CR-999",
			title: "Crash on login", want: "hotfix/cr-999-crash-on-login",
		},
		{
			// 纯中文标题 → slug 为空 → 退化为只用编号，不留尾巴
			name: "纯中文标题", kind: KindFix, key: "CR-1152",
			title: "模型列表中的关联分组显示错误", want: "fix/cr-1152",
		},
		{
			name: "中英混合", kind: KindFix, key: "CR-1250",
			title: "Key 折扣下拉框宽度异常", want: "fix/cr-1250-key",
		},
		{
			name: "标点与多空格", kind: KindFix, key: "CR-1",
			title: "Fix:  the   thing!! (again)", want: "fix/cr-1-fix-the-thing-again",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.BranchName(tc.kind, tc.key, tc.title)
			if err != nil {
				t.Fatalf("BranchName 报错: %v", err)
			}
			if got != tc.want {
				t.Errorf("BranchName = %q，期望 %q", got, tc.want)
			}
			// 生成的分支名必须能通过受保护分支校验
			if err := c.ValidatePushTarget(got); err != nil {
				t.Errorf("生成的分支名却被拦截: %v", err)
			}
		})
	}
}

// 超长标题必须被截断，且不留尾部横杠。
func TestBranchNameTruncatesLongTitle(t *testing.T) {
	c := DefaultRepoConfig("x/y")
	long := strings.Repeat("verylongword ", 30)

	got, err := c.BranchName(KindFix, "CR-1", long)
	if err != nil {
		t.Fatalf("BranchName 报错: %v", err)
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("分支名不应以 - 结尾: %q", got)
	}
	if len(got) > maxSlugLen+32 {
		t.Errorf("分支名过长(%d): %q", len(got), got)
	}
}

func TestBranchNameRejectsBadInput(t *testing.T) {
	c := DefaultRepoConfig("x/y")

	if _, err := c.BranchName(TaskKind("bogus"), "CR-1", "t"); err == nil {
		t.Error("未知任务类型应报错")
	}
	if _, err := c.BranchName(KindFix, "", "t"); err == nil {
		t.Error("空 issue 编号应报错")
	}
	if _, err := c.BranchName(KindFix, "   ", "t"); err == nil {
		t.Error("空白 issue 编号应报错")
	}
}

// 若仓库把分支模式配成了受保护分支本身，必须在生成时就拒绝，
// 而不是等到推送才发现。
func TestBranchNameRejectsProtectedPattern(t *testing.T) {
	c := DefaultRepoConfig("x/y")
	c.BranchPattern = "main"

	if _, err := c.BranchName(KindFix, "CR-1", "whatever"); err == nil {
		t.Fatal("模式生成出受保护分支名时应拒绝")
	} else {
		var pe ErrProtectedBranch
		if !errors.As(err, &pe) {
			t.Errorf("错误类型应为 ErrProtectedBranch，得到 %T: %v", err, err)
		}
	}
}

func TestBranchNameCustomPattern(t *testing.T) {
	c := DefaultRepoConfig("x/y")
	c.BranchPattern = "zichuanwang/{key}-{slug}" // 团队里确实有这种个人前缀风格

	got, err := c.BranchName(KindFix, "CR-1152", "callable groups")
	if err != nil {
		t.Fatalf("BranchName 报错: %v", err)
	}
	if got != "zichuanwang/cr-1152-callable-groups" {
		t.Errorf("BranchName = %q", got)
	}
}

func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Portable import fails", "portable-import-fails"},
		{"UPPER Case", "upper-case"},
		{"multiple   spaces", "multiple-spaces"},
		{"trailing-", "trailing"},
		{"-leading", "leading"},
		{"under_score/slash.dot", "under-score-slash-dot"},
		{"纯中文", ""},
		{"", ""},
		{"!!!", ""},
		{"a!!!b", "a-b"},
		{"CR-1326", "cr-1326"},
	}
	for _, tc := range cases {
		if got := Slugify(tc.in); got != tc.want {
			t.Errorf("Slugify(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateBranchNameRejectsGitIllegal(t *testing.T) {
	bad := []string{
		"", "-leading", "/leading",
		"has space", "has..dots", "has//slashes",
		"tilde~", "caret^", "colon:", "question?", "star*",
		"bracket[", "back\\slash", "at@{brace", "ends.lock",
	}
	for _, b := range bad {
		if err := validateBranchName(b); err == nil {
			t.Errorf("%q 应被判为非法分支名", b)
		}
	}

	good := []string{
		"fix/cr-1-slug", "feature/x", "hotfix/y-z", "a",
	}
	for _, g := range good {
		if err := validateBranchName(g); err != nil {
			t.Errorf("%q 应为合法分支名，得到: %v", g, err)
		}
	}
}
