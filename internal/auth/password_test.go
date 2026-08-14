package auth

import (
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	const pw = "correct-horse-battery"

	h, err := Hash(pw)
	if err != nil {
		t.Fatalf("Hash 失败: %v", err)
	}
	if h == pw || strings.Contains(h, pw) {
		t.Fatal("哈希里不该出现明文口令")
	}
	if !Verify(h, pw) {
		t.Fatal("正确口令应当校验通过")
	}
	if Verify(h, pw+"x") {
		t.Fatal("错误口令不该校验通过")
	}
}

// 同一口令两次哈希必须不同 —— bcrypt 每次生成新盐。
// 若这条挂了，说明盐没起作用，彩虹表就能一次撞倒所有同口令的账号。
func TestHashUsesFreshSalt(t *testing.T) {
	a, err := Hash("same-password")
	if err != nil {
		t.Fatalf("Hash 失败: %v", err)
	}
	b, err := Hash("same-password")
	if err != nil {
		t.Fatalf("Hash 失败: %v", err)
	}
	if a == b {
		t.Fatal("两次哈希相同，说明没有随机盐")
	}
	if !Verify(a, "same-password") || !Verify(b, "same-password") {
		t.Fatal("两个哈希都应能校验通过")
	}
}

// 空哈希绝不能被当成「空口令通过」。users.password_hash 可为 NULL
// （超管建号后还没补密码），这条守住那个窗口。
func TestVerifyRejectsEmptyHash(t *testing.T) {
	for _, tc := range []struct{ hash, plain string }{
		{"", ""},
		{"", "anything"},
		{"$2a$12$notarealhash", "anything"},
	} {
		if Verify(tc.hash, tc.plain) {
			t.Errorf("Verify(%q, %q) 应当为 false", tc.hash, tc.plain)
		}
	}
}

func TestPolicy(t *testing.T) {
	cases := []struct {
		name   string
		pw     string
		wantOK bool
	}{
		{"空", "", false},
		{"7 位", "1234567", false},
		{"8 位", "12345678", true},
		{"长口令", strings.Repeat("a", 72), true},
		// bcrypt 只取前 72 字节，超出部分被静默丢弃 —— 必须显式拒绝，
		// 否则会出现「改了密码但旧密码还能登录」的诡异现象
		{"73 字节", strings.Repeat("a", 73), false},
		{"8 个汉字（24 字节）", "密码密码密码密码", true},
		{"7 个汉字", "密码密码密码密", false},
		{"25 个汉字（75 字节）", strings.Repeat("密", 25), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Policy(tc.pw)
			if tc.wantOK && err != nil {
				t.Fatalf("应当通过，却报错: %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatal("应当被拒绝，却通过了")
			}
		})
	}
}

// Hash 必须自己过一遍 Policy：任何绕过策略的入口都是漏洞。
func TestHashEnforcesPolicy(t *testing.T) {
	if _, err := Hash("short"); err == nil {
		t.Fatal("过短的口令不该被哈希")
	}
}

func TestNeedsRehash(t *testing.T) {
	h, err := Hash("a-long-enough-password")
	if err != nil {
		t.Fatalf("Hash 失败: %v", err)
	}
	if NeedsRehash(h) {
		t.Fatal("当前 cost 生成的哈希不该需要重算")
	}
	// cost 4 是 bcrypt 允许的最小值，代表一个远低于现行标准的老哈希
	if !NeedsRehash("$2a$04$Cw1oZ8nGqQZBqOZ8nGqQZefKq1oZ8nGqQZBqOZ8nGqQZBqOZ8nGqQ") {
		t.Fatal("低 cost 的老哈希应当需要重算")
	}
	if !NeedsRehash("这不是一个 bcrypt 串") {
		t.Fatal("无法解析的哈希应当被当作需要重算")
	}
}
