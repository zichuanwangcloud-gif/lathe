// Package capbuf 提供只保留头尾的有界缓冲。
//
// 用途：子进程输出（构建日志、CLI 的 stderr）可以任意长，但调用方只需要
// 一段摘要给人看、喂给模型。原来的做法是先整份收进 bytes.Buffer、再截断：
// 内存峰值等于输出全长，一次刷屏的构建就是几百 MB。这里边写边丢中间，
// 内存始终不超过 head+tail。
//
// 为什么留头又留尾：结论通常在尾部（go test 的 FAIL 汇总、编译器的最后
// 一条报错），而头部常有第一个报错的上下文。只留头会把结论截掉——这正是
// 改造前验证摘要的毛病。
package capbuf

import (
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

// Buffer 是并发安全的头尾缓冲：前 head 字节原样保留，之后只保留最后
// tail 字节。实现 io.Writer，Write 永不返回错误（作为子进程的输出端，
// 返回错误会让 exec 的拷贝协程中断、子进程收到 SIGPIPE）。
type Buffer struct {
	mu    sync.Mutex
	head  []byte
	ring  []byte // 容量 tail 的环形缓冲
	pos   int    // ring 的下一个写入位置
	full  bool   // ring 是否已绕过一圈
	hcap  int
	total int64
}

// New 造一个保留前 head、后 tail 字节的缓冲。
func New(head, tail int) *Buffer {
	if head < 0 {
		head = 0
	}
	if tail < 0 {
		tail = 0
	}
	return &Buffer{
		head: make([]byte, 0, head),
		ring: make([]byte, tail),
		hcap: head,
	}
}

// Write 实现 io.Writer。
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.total += int64(n)

	if room := b.hcap - len(b.head); room > 0 {
		k := min(room, len(p))
		b.head = append(b.head, p[:k]...)
		p = p[k:]
	}
	if len(b.ring) == 0 || len(p) == 0 {
		return n, nil
	}
	// 超过环容量的部分只有最后 len(ring) 字节有意义
	if len(p) >= len(b.ring) {
		copy(b.ring, p[len(p)-len(b.ring):])
		b.pos, b.full = 0, true
		return n, nil
	}
	k := copy(b.ring[b.pos:], p)
	if k < len(p) {
		copy(b.ring, p[k:])
		b.full = true
	}
	b.pos = (b.pos + len(p)) % len(b.ring)
	if b.pos == 0 && len(p) > 0 {
		b.full = true
	}
	return n, nil
}

// Total 返回累计写入的字节数（含已丢弃的中间部分）。
func (b *Buffer) Total() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

// String 返回「头 + 省略说明 + 尾」。没丢过东西时就是原文。
// 切口落在 UTF-8 字符边界上：非法 UTF-8 落库会被 Postgres 拒绝
// （SQLSTATE 22021，AGENTS.md §9 有前科）。
func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	var tail []byte
	if b.full {
		tail = make([]byte, 0, len(b.ring))
		tail = append(tail, b.ring[b.pos:]...)
		tail = append(tail, b.ring[:b.pos]...)
	} else {
		tail = b.ring[:b.pos]
	}

	kept := int64(len(b.head) + len(tail))
	if kept >= b.total {
		return strings.ToValidUTF8(string(b.head)+string(tail), "")
	}
	return join(string(b.head), string(tail), b.total-kept)
}

// ClipMiddle 把 s 收敛到约 max 字节：超长时保留开头约 1/8、结尾约 7/8，
// 中间换成省略说明。用于把已在内存里的字符串（如多条输出的拼接）再压一次。
func ClipMiddle(s string, max int) string {
	if len(s) <= max {
		return s
	}
	head := max / 8
	tail := max - head
	return join(s[:head], s[len(s)-tail:], int64(len(s)-head-tail))
}

func join(head, tail string, omitted int64) string {
	head = trimRightToRune(head)
	tail = trimLeftToRune(tail)
	return strings.ToValidUTF8(head, "") +
		fmt.Sprintf("\n…（中间省略 %d 字节）…\n", omitted) +
		strings.ToValidUTF8(tail, "")
}

// trimRightToRune 去掉末尾被切断的半个多字节字符。
func trimRightToRune(s string) string {
	for i := 0; i < utf8.UTFMax && len(s) > 0; i++ {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size != 1 {
			return s
		}
		s = s[:len(s)-1]
	}
	return s
}

// trimLeftToRune 跳过开头的续字节（被切断字符的后半截）。
func trimLeftToRune(s string) string {
	for i := 0; i < utf8.UTFMax && len(s) > 0 && !utf8.RuneStart(s[0]); i++ {
		s = s[1:]
	}
	return s
}
