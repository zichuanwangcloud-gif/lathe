// Package auth 提供密码哈希与口令强度校验。
//
// 单独成包而非塞进 httpapi：哈希逻辑不依赖 HTTP 也不依赖数据库，
// 独立出来才能脱库做单元测试，也避免 store 与 httpapi 互相 import。
package auth

import (
	"crypto/rand"
	"errors"
	"fmt"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost 是哈希强度。
//
// 12 而非默认的 10：登录不是热路径，多花几十毫秒换取更高的离线爆破成本值得。
// 这个值编码在哈希串里，将来调高也不影响老哈希的校验 —— 见 NeedsRehash。
const bcryptCost = 12

// 口令长度边界。
//
// 上限不是防用户，是防 bcrypt 自身的坑：bcrypt 只取前 72 字节，超长口令
// 后面的部分被静默丢弃，会让「改了密码却还能用旧密码登录」这种诡异现象出现。
// 明确拒绝比静默截断好。
const (
	MinPasswordLen = 8
	MaxPasswordLen = 72
)

// ErrPasswordTooShort 与 ErrPasswordTooLong 供上层映射成提示文案。
var (
	ErrPasswordTooShort = fmt.Errorf("密码至少 %d 位", MinPasswordLen)
	ErrPasswordTooLong  = fmt.Errorf("密码不能超过 %d 字节（含中文时一个字算 3 字节）", MaxPasswordLen)
)

// Policy 校验口令是否可用。
//
// 只管长度，不强制大小写数字符号的组合：那类规则把用户推向
// "Passw0rd!" 这种可预测的形状，长度才是真正抗爆破的维度。
func Policy(plain string) error {
	if utf8.RuneCountInString(plain) == 0 {
		return errors.New("密码不能为空")
	}
	// 按字节数判上限（bcrypt 的限制是字节），按字符数判下限（对中文更合理）
	if utf8.RuneCountInString(plain) < MinPasswordLen {
		return ErrPasswordTooShort
	}
	if len(plain) > MaxPasswordLen {
		return ErrPasswordTooLong
	}
	return nil
}

// Hash 计算口令哈希。返回的字符串自带盐与 cost，可直接入库。
func Hash(plain string) (string, error) {
	if err := Policy(plain); err != nil {
		return "", err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("auth: 计算密码哈希失败: %w", err)
	}
	return string(h), nil
}

// Verify 校验口令是否匹配哈希。
//
// 只返回 bool 不返回 error：调用方对「哈希损坏」和「密码不对」的处理完全一样
// —— 都是拒绝登录。区分二者只会诱导上层把细节回给客户端。
//
// bcrypt.CompareHashAndPassword 内部是恒定时间比较，无需额外处理。
func Verify(hash, plain string) bool {
	if hash == "" || plain == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// NeedsRehash 报告哈希是否用低于当前标准的 cost 生成。
//
// 留给「调高 bcryptCost 后，用户下次成功登录时顺手升级哈希」的场景 ——
// 那是唯一能拿到明文口令的时机。
func NeedsRehash(hash string) bool {
	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		// 解不出 cost 说明这不是合法 bcrypt 串，当作需要重算
		return true
	}
	return cost < bcryptCost
}

// RandomPassword 生成一个便于口头转述的随机口令。
//
// 字符集剔掉了 0/O/1/l/I 这些看混的字符：这类口令注定要被人从日志或界面
// 抄到浏览器里，少一次抄错就少一次「密码明明是对的却登不上」。
//
// 16 位 × 53 个字符 ≈ 91 位熵，远超它的用途所需（一次性初始口令，
// 且强制改密）。
func RandomPassword() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("auth: 生成随机口令失败: " + err.Error())
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
