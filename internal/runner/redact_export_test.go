package runner

import (
	"strings"
	"testing"
)

// TestInboxRedactTextExported 守住收件箱用的导出入口：它就是 redactText 本身，
// 不是第二套规则（docs/features/inbox.md AC-13c）。
func TestInboxRedactTextExported(t *testing.T) {
	const known = "Zq7wPx3Lm9KdR2vT"
	cases := []struct {
		name string
		in   string
	}{
		{"形态层 ghp_", "push 失败：ghp_" + strings.Repeat("Ab3", 12) + " 无权限"},
		{"形态层 lin_api_", "Authorization: lin_api_" + strings.Repeat("Q9x1", 10)},
		{"权威层已知值", "dsn 里带了 " + known + " 这个口令"},
		{"无凭据的普通文本", "验证失败：go test ./... 有 2 个用例红"},
		{"空串", ""},
	}
	reg := []string{known}
	for _, c := range cases {
		t.Run(c.name+" 与 redactText 逐字节相等", func(t *testing.T) {
			got := RedactText(c.in, reg)
			want := redactText(c.in, reg, nil)
			if got != want {
				t.Errorf("got=%q want=%q", got, want)
			}
		})
	}
	t.Run("已知值确实被替换", func(t *testing.T) {
		if got := RedactText("x "+known+" y", reg); strings.Contains(got, known) {
			t.Errorf("got=%q，仍含已知凭据", got)
		}
	})
}
