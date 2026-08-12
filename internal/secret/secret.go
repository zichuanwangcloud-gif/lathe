// Package secret 提供凭据的本地加密存储。
//
// 设计取舍：管理界面需要能配置 Linear / GitHub 凭据，但明文入库意味着
// 数据库一旦泄漏，所有第三方账号随之沦陷。这里用 AES-256-GCM 加密后再
// 落库，主密钥保存在数据库之外（环境变量或独立文件），使得拿到数据库
// 转储的人无法解出凭据。
//
// 这不等同于专业的 secret store（没有轮换、审计、细粒度授权），
// 但它把"明文躺在库里"这个最糟的情况消除了。接入外部 secret store
// 见 docs/02-design.md §4 的 token_ref 字段。
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// KeySize 是主密钥长度（AES-256）。
const KeySize = 32

// ErrDecrypt 表示解密失败：密钥不对，或密文被篡改/损坏。
//
// 刻意不区分这两种情况：对外暴露"密钥错"与"密文坏"的差别，
// 会给攻击者提供额外信息。
var ErrDecrypt = errors.New("secret: 解密失败（主密钥不匹配或数据已损坏）")

// Sealer 加解密凭据。
type Sealer interface {
	Seal(plaintext string) ([]byte, error)
	Open(ciphertext []byte) (string, error)
}

// AESSealer 用 AES-256-GCM 实现 Sealer。
type AESSealer struct {
	aead cipher.AEAD
}

// New 用给定主密钥构造 Sealer。
func New(key []byte) (*AESSealer, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secret: 主密钥须为 %d 字节，得到 %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: 初始化加密器失败: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: 初始化 GCM 失败: %w", err)
	}
	return &AESSealer{aead: aead}, nil
}

// Seal 加密明文。每次调用使用新的随机 nonce，因此同一明文两次加密
// 得到的密文不同 —— 这可防止从密文相等推断出凭据相等。
func (s *AESSealer) Seal(plaintext string) ([]byte, error) {
	if plaintext == "" {
		return nil, fmt.Errorf("secret: 待加密内容为空")
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("secret: 生成 nonce 失败: %w", err)
	}
	// 密文格式：nonce || 密文+认证标签
	return s.aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Open 解密密文。GCM 的认证标签会检出任何篡改。
func (s *AESSealer) Open(ciphertext []byte) (string, error) {
	n := s.aead.NonceSize()
	if len(ciphertext) < n+1 {
		return "", ErrDecrypt
	}
	plaintext, err := s.aead.Open(nil, ciphertext[:n], ciphertext[n:], nil)
	if err != nil {
		return "", ErrDecrypt
	}
	return string(plaintext), nil
}

// LoadKey 按优先级取得主密钥：
//
//  1. LATHE_SECRET_KEY 环境变量（64 位十六进制字符）—— 生产推荐，
//     可由外部密钥管理系统注入，不落盘。
//  2. keyPath 指向的文件 —— 单机部署的默认方式，首次运行自动生成。
//
// 返回的第二个值说明密钥来源，供启动日志与界面展示。
func LoadKey(keyPath string) ([]byte, string, error) {
	if env := strings.TrimSpace(os.Getenv("LATHE_SECRET_KEY")); env != "" {
		key, err := hex.DecodeString(env)
		if err != nil {
			return nil, "", fmt.Errorf("secret: LATHE_SECRET_KEY 须为十六进制字符串: %w", err)
		}
		if len(key) != KeySize {
			return nil, "", fmt.Errorf("secret: LATHE_SECRET_KEY 解码后须为 %d 字节（%d 个十六进制字符），得到 %d",
				KeySize, KeySize*2, len(key))
		}
		return key, "env:LATHE_SECRET_KEY", nil
	}

	if keyPath == "" {
		return nil, "", fmt.Errorf("secret: 未设置 LATHE_SECRET_KEY 且未指定密钥文件路径")
	}

	raw, err := os.ReadFile(keyPath)
	if err == nil {
		key, decErr := hex.DecodeString(strings.TrimSpace(string(raw)))
		if decErr != nil || len(key) != KeySize {
			return nil, "", fmt.Errorf("secret: 密钥文件 %s 内容非法（应为 %d 个十六进制字符）", keyPath, KeySize*2)
		}
		return key, "file:" + keyPath, nil
	}
	if !os.IsNotExist(err) {
		return nil, "", fmt.Errorf("secret: 读取密钥文件失败: %w", err)
	}

	// 首次运行：生成并落盘。权限 0600 —— 这个文件等同于所有凭据本身。
	key := make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, "", fmt.Errorf("secret: 生成主密钥失败: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return nil, "", fmt.Errorf("secret: 创建密钥目录失败: %w", err)
	}
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(key)), 0o600); err != nil {
		return nil, "", fmt.Errorf("secret: 写入密钥文件失败: %w", err)
	}
	return key, "file:" + keyPath + "（本次新建）", nil
}

// Mask 把凭据转成可安全展示的掩码，如 "lin_api_…8f3a"。
//
// 保留尾部几位是为了让人能确认"界面上这条是不是我配的那个"，
// 而不必暴露完整凭据。
func Mask(s string) string {
	if s == "" {
		return ""
	}
	const tail = 4
	if len(s) <= tail+2 {
		return strings.Repeat("•", len(s))
	}
	return "••••••••" + s[len(s)-tail:]
}
