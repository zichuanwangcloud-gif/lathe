package capbuf

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBufferKeepsHeadAndTail(t *testing.T) {
	cases := []struct {
		name       string
		head, tail int
		writes     []string
		want       string // 精确比对；空串表示改用 contains 检查
		contains   []string
	}{
		{name: "未超限时原样返回", head: 4, tail: 4, writes: []string{"abc", "def"}, want: "abcdef"},
		{name: "恰好填满不加省略说明", head: 3, tail: 3, writes: []string{"abcdef"}, want: "abcdef"},
		{name: "单次超长写只留头尾", head: 3, tail: 3, writes: []string{"abcXXXXXXXXdef"},
			contains: []string{"abc", "def", "省略 8 字节"}},
		{name: "多次小写绕环后尾部顺序正确", head: 2, tail: 4,
			writes:   []string{"ab", "1", "2", "3", "4", "5", "6", "7"},
			contains: []string{"ab", "4567", "省略 3 字节"}},
		{name: "head 为 0 只留尾", head: 0, tail: 3, writes: []string{"hello world"},
			contains: []string{"rld"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := New(tc.head, tc.tail)
			var total int
			for _, w := range tc.writes {
				n, err := b.Write([]byte(w))
				if err != nil || n != len(w) {
					t.Fatalf("Write(%q) = %d, %v；want %d, nil", w, n, err, len(w))
				}
				total += len(w)
			}
			got := b.String()
			if tc.want != "" && got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
			for _, c := range tc.contains {
				if !strings.Contains(got, c) {
					t.Errorf("String() = %q, 应包含 %q", got, c)
				}
			}
			if b.Total() != int64(total) {
				t.Errorf("Total() = %d, want %d", b.Total(), total)
			}
		})
	}
}

// 写入远超容量时内存不随输入增长：内部切片长度恒定。
func TestBufferMemoryIsBounded(t *testing.T) {
	b := New(1024, 4096)
	chunk := []byte(strings.Repeat("x", 64<<10))
	for i := 0; i < 256; i++ { // 共 16MB
		_, _ = b.Write(chunk)
	}
	if cap(b.head) > 1024 || len(b.ring) != 4096 {
		t.Errorf("缓冲应有界：head cap=%d ring len=%d", cap(b.head), len(b.ring))
	}
	if got := len(b.String()); got > 1024+4096+64 {
		t.Errorf("String() 长度 %d 超出头尾容量", got)
	}
}

// 切口落在多字节字符中间时，结果仍须是合法 UTF-8（落库要求）。
func TestBufferOutputIsValidUTF8(t *testing.T) {
	for head := 0; head < 6; head++ {
		for tail := 1; tail < 6; tail++ {
			b := New(head, tail)
			_, _ = b.Write([]byte(strings.Repeat("中文输出", 50)))
			if s := b.String(); !utf8.ValidString(s) {
				t.Errorf("head=%d tail=%d 产生非法 UTF-8: %q", head, tail, s)
			}
		}
	}
}

func TestClipMiddle(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		max      int
		contains []string
		exact    bool
	}{
		{name: "未超限原样返回", in: "short", max: 100, contains: []string{"short"}, exact: true},
		{name: "超限保留开头与结尾", in: "BEGIN" + strings.Repeat("-", 1000) + "THE-END", max: 80,
			contains: []string{"BEGIN"[:2], "THE-END", "中间省略"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClipMiddle(tc.in, tc.max)
			if tc.exact && got != tc.in {
				t.Errorf("got %q, want %q", got, tc.in)
			}
			for _, c := range tc.contains {
				if !strings.Contains(got, c) {
					t.Errorf("got %q, 应包含 %q", got, c)
				}
			}
			if !utf8.ValidString(got) {
				t.Errorf("结果不是合法 UTF-8: %q", got)
			}
		})
	}
}
