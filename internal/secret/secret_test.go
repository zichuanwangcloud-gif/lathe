package secret

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testSealer(t *testing.T) *AESSealer {
	t.Helper()
	key := make([]byte, KeySize)
	for i := range key {
		key[i] = byte(i)
	}
	s, err := New(key)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return s
}

func TestSealOpenRoundTrip(t *testing.T) {
	s := testSealer(t)

	for _, plain := range []string{
		"lin_api_abcdef123456",
		"ghp_xxxxxxxxxxxxxxxxxxxx",
		"含中文的凭据",
		"a",
		strings.Repeat("x", 4096),
	} {
		ct, err := s.Seal(plain)
		if err != nil {
			t.Fatalf("Seal(%q) 失败: %v", plain[:min(10, len(plain))], err)
		}
		got, err := s.Open(ct)
		if err != nil {
			t.Fatalf("Open 失败: %v", err)
		}
		if got != plain {
			t.Errorf("往返后不一致: 得到 %q", got[:min(20, len(got))])
		}
	}
}

// 密文里绝不能出现明文片段 —— 否则加密就是摆设。
func TestCiphertextDoesNotContainPlaintext(t *testing.T) {
	s := testSealer(t)
	plain := "lin_api_SUPERSECRET_TOKEN_VALUE"

	ct, err := s.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte(plain)) {
		t.Error("密文包含完整明文")
	}
	if bytes.Contains(ct, []byte("SUPERSECRET")) {
		t.Error("密文包含明文片段")
	}
}

// 同一明文两次加密应得到不同密文，防止从密文相等推断凭据相等。
func TestSealUsesFreshNonce(t *testing.T) {
	s := testSealer(t)

	a, _ := s.Seal("same-token")
	b, _ := s.Seal("same-token")
	if bytes.Equal(a, b) {
		t.Error("同一明文两次加密得到相同密文 —— nonce 未随机化")
	}

	// 但都要能解回原文
	for _, ct := range [][]byte{a, b} {
		if got, err := s.Open(ct); err != nil || got != "same-token" {
			t.Errorf("解密失败: %q %v", got, err)
		}
	}
}

func TestOpenRejectsWrongKey(t *testing.T) {
	s1 := testSealer(t)

	key2 := make([]byte, KeySize)
	for i := range key2 {
		key2[i] = byte(255 - i)
	}
	s2, _ := New(key2)

	ct, _ := s1.Seal("secret-value")
	if _, err := s2.Open(ct); !errors.Is(err, ErrDecrypt) {
		t.Errorf("换密钥应解不开，得到 %v", err)
	}
}

// GCM 的认证标签必须能检出任何篡改。
func TestOpenDetectsTampering(t *testing.T) {
	s := testSealer(t)
	ct, _ := s.Seal("secret-value")

	cases := map[string][]byte{
		"改最后一字节":  append(append([]byte{}, ct[:len(ct)-1]...), ct[len(ct)-1]^0xff),
		"改 nonce": append([]byte{ct[0] ^ 0xff}, ct[1:]...),
		"截断":      ct[:len(ct)-3],
		"空":       {},
		"过短":      {1, 2, 3},
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Open(bad); !errors.Is(err, ErrDecrypt) {
				t.Errorf("篡改的密文应被拒绝，得到 %v", err)
			}
		})
	}
}

func TestNewRejectsBadKeySize(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		if _, err := New(make([]byte, n)); err == nil {
			t.Errorf("%d 字节的密钥应被拒绝", n)
		}
	}
}

func TestSealRejectsEmpty(t *testing.T) {
	s := testSealer(t)
	if _, err := s.Seal(""); err == nil {
		t.Error("空内容应报错 —— 静默存一个空凭据会让人以为配好了")
	}
}

// ---------------------------------------------------------------- 密钥加载

func TestLoadKeyFromEnv(t *testing.T) {
	want := strings.Repeat("ab", KeySize) // 64 个十六进制字符
	t.Setenv("LATHE_SECRET_KEY", want)

	key, source, err := LoadKey("")
	if err != nil {
		t.Fatalf("LoadKey 失败: %v", err)
	}
	if hex.EncodeToString(key) != want {
		t.Error("密钥内容不符")
	}
	if !strings.Contains(source, "env") {
		t.Errorf("来源应标明环境变量，得到 %q", source)
	}
}

func TestLoadKeyRejectsBadEnv(t *testing.T) {
	for _, bad := range []string{"不是十六进制", "abcd", strings.Repeat("ab", 16)} {
		t.Run(bad[:min(8, len(bad))], func(t *testing.T) {
			t.Setenv("LATHE_SECRET_KEY", bad)
			if _, _, err := LoadKey(""); err == nil {
				t.Errorf("非法密钥 %q 应被拒绝", bad)
			}
		})
	}
}

// 首次运行应自动生成密钥文件，且权限必须是 0600 ——
// 这个文件等同于所有凭据本身。
func TestLoadKeyGeneratesFileWithSafePermissions(t *testing.T) {
	t.Setenv("LATHE_SECRET_KEY", "")
	path := filepath.Join(t.TempDir(), "sub", "secret.key")

	key1, source, err := LoadKey(path)
	if err != nil {
		t.Fatalf("LoadKey 失败: %v", err)
	}
	if len(key1) != KeySize {
		t.Errorf("密钥长度 = %d", len(key1))
	}
	if !strings.Contains(source, "新建") {
		t.Errorf("首次应标明新建，得到 %q", source)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("密钥文件未生成: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("密钥文件权限 = %o，必须是 600", perm)
	}

	// 再次加载应读到同一密钥，否则重启后所有已存凭据都解不开
	key2, source2, err := LoadKey(path)
	if err != nil {
		t.Fatalf("二次 LoadKey 失败: %v", err)
	}
	if !bytes.Equal(key1, key2) {
		t.Error("二次加载得到不同密钥 —— 重启后已存凭据将无法解密")
	}
	if strings.Contains(source2, "新建") {
		t.Error("二次加载不应再标记为新建")
	}
}

func TestLoadKeyRejectsCorruptFile(t *testing.T) {
	t.Setenv("LATHE_SECRET_KEY", "")
	path := filepath.Join(t.TempDir(), "secret.key")
	if err := os.WriteFile(path, []byte("这不是合法密钥"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadKey(path); err == nil {
		t.Error("损坏的密钥文件应报错，而不是静默生成新密钥")
	}
}

func TestLoadKeyRequiresSource(t *testing.T) {
	t.Setenv("LATHE_SECRET_KEY", "")
	if _, _, err := LoadKey(""); err == nil {
		t.Error("既无环境变量也无文件路径时应报错")
	}
}

func TestMask(t *testing.T) {
	cases := map[string]string{
		"lin_api_abcdef123456": "••••••••3456",
		"":                     "",
		"abc":                  "•••",
	}
	for in, want := range cases {
		if got := Mask(in); got != want {
			t.Errorf("Mask(%q) = %q，期望 %q", in, got, want)
		}
	}

	// 掩码里不能出现凭据的前缀部分
	if strings.Contains(Mask("lin_api_secret_value"), "lin_api") {
		t.Error("掩码不应暴露凭据前缀")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
