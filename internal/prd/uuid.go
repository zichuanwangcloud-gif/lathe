package prd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// newUUID 生成 RFC 4122 v4 UUID。
//
// claude CLI 的 --session-id 要求合法 UUID。与 runner/preview 各有一份
// 同样的实现：本仓刻意不为这十行引入 google/uuid（新依赖要先问人，
// AGENTS.md §4），三处复制的代价小于一条依赖。
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败属于系统级异常，此处无从降级。
		panic(fmt.Sprintf("prd: 生成 UUID 失败: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10

	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
