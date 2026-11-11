package runner

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pemSamples 是 AC-5 的三种 PEM 私钥头。拆开拼接：本文件自己不该带一整行
// 像私钥头的字面量（扫描器、编辑器插件都可能误报）。
func pemSamples() map[string]string {
	dash := strings.Repeat("-", 5)
	return map[string]string{
		"PEM_OPENSSH": dash + "BEGIN OPENSSH PRIVATE KEY" + dash,
		"PEM_RSA":     dash + "BEGIN RSA PRIVATE KEY" + dash,
		"PEM_PKCS8":   dash + "BEGIN PRIVATE KEY" + dash,
	}
}

// AC-5：每种形态放在第 3 行 → 命中且行号 = 3；拒绝信息不回显该串。
func TestFindCredential_Shapes(t *testing.T) {
	cases := map[string]string{
		"github_pat":   "ghp_" + strings.Repeat("A1b2", 5),
		"linear_key":   "lin_api_" + strings.Repeat("Zx9y", 5),
		"sk_key":       "sk-" + strings.Repeat("q7W3", 5),
		"jwt":          "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.c2lnbmF0dXJlc2lnbmF0dXJl",
		"postgres_dsn": "postgres://u:pw5Secretxx@h:5432/d",
	}
	for k, v := range pemSamples() {
		cases[k] = v
	}
	for name, secret := range cases {
		t.Run(name, func(t *testing.T) {
			s := "# 标题\n\n用法：" + secret + " 结束\n第四行\n"
			line, ok := FindCredential(s, nil)
			if !ok || line != 3 {
				t.Fatalf("FindCredential = (%d, %v), want (3, true)", line, ok)
			}
			msg := CredentialRejectMessage("SKILL.md", line)
			if strings.Contains(msg, secret) {
				t.Errorf("拒绝信息不得回显凭据：%q", msg)
			}
			if !strings.Contains(msg, "第 3 行") {
				t.Errorf("拒绝信息应含行号：%q", msg)
			}
		})
	}
}

// tokenShapesCountAtF11 是 f11 实现时 tokenShapes 的条目数。谁往表里塞 PEM
// （或任何别的规则）都会让这条锁变红，提醒他同步处理 FindCredential 里的
// 私有 PEM 正则（见 redact.go 的 pemPrivateKeyHeader 注释）。
const tokenShapesCountAtF11 = 10

// AC-5（r3）：PEM 检出只在 FindCredential 侧，脱敏（替换）语义不变。
func TestFindCredential_PEMDoesNotChangeRedaction(t *testing.T) {
	for name, pem := range pemSamples() {
		t.Run(name, func(t *testing.T) {
			s := "a\n" + pem + "\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\n"
			if got := redactText(s, nil, nil); got != s {
				t.Errorf("redactText 改写了 PEM 文本：got %q, want 原样", got)
			}
		})
	}
	if got := len(tokenShapes); got != tokenShapesCountAtF11 {
		t.Errorf("len(tokenShapes) = %d, want %d（改了形态表就同步检查 FindCredential 的 PEM 私有规则）",
			got, tokenShapesCountAtF11)
	}
}

// AC-5：层 1 生效 —— 不在任何形态表里的随机串，登记后能检出。
func TestFindCredential_KnownValueOnly(t *testing.T) {
	val := "plainvalue" + strconv.FormatInt(time.Now().UnixNano(), 36)
	s := "第一行\n第二行 " + val + "\n"
	if line, ok := FindCredential(s, nil); ok {
		t.Fatalf("未登记时不应命中（该值不像任何形态），got line %d", line)
	}
	line, ok := FindCredential(s, []string{val})
	if !ok || line != 2 {
		t.Fatalf("FindCredential = (%d, %v), want (2, true)", line, ok)
	}
}

// AC-5：真实的产品技能文本不被误判。
func TestFindCredential_IgnoresOrdinaryProse(t *testing.T) {
	for _, p := range []string{
		filepath.Join(skillsSourceDir(t), "go-testing", "1.0.0", "SKILL.md"),
		filepath.Join(skillsSourceDir(t), "sql-migration", "1.0.0", "SKILL.md"),
	} {
		t.Run(filepath.Base(filepath.Dir(filepath.Dir(p))), func(t *testing.T) {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if line, ok := FindCredential(string(b), nil); ok {
				t.Errorf("产品技能文本被误判为凭据：第 %d 行", line)
			}
		})
	}
}

// 多处命中取最靠前的行：PEM 在第 2 行、token 在第 4 行 → 2。
func TestFindCredential_EarliestLineWins(t *testing.T) {
	pem := pemSamples()["PEM_RSA"]
	s := "a\n" + pem + "\nb\nghp_" + strings.Repeat("A1b2", 5) + "\n"
	if line, ok := FindCredential(s, nil); !ok || line != 2 {
		t.Fatalf("FindCredential = (%d, %v), want (2, true)", line, ok)
	}
}
