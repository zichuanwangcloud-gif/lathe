package runner

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// newUUID 生成 RFC 4122 v4 UUID。
//
// claude CLI 的 --session-id 要求合法 UUID；自己生成是为了在执行前
// 就把会话 ID 落库（见 docs/03-tech-stack.md §4），不必等 CLI 回报。
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败属于系统级异常，此处无从降级
		panic(fmt.Sprintf("runner: 生成 UUID 失败: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10

	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
