package linear

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncateKeepsValidUTF8 钉住 AGENTS.md §9「字符串截断按 rune 不按 byte」：
// Linear 的 HTTP 错误体经 do() 截断后要落进 schedule_runs.error，
// 按字节切会切断多字节字符，Postgres 拒收（SQLSTATE 22021）整条写入。
func TestTruncateKeepsValidUTF8(t *testing.T) {
	body := strings.Repeat("错", 600) // 每个汉字 3 字节，500 字节处必然落在字符中间

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	c, err := NewClientWithURL("tok", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Issue(context.Background(), "ENG-1")
	if err == nil {
		t.Fatal("HTTP 500 应返回错误")
	}
	if got := err.Error(); !utf8.ValidString(got) {
		t.Fatalf("错误串必须是合法 UTF-8（按 rune 截断），got 非法串，末尾 20 字节 %q", got[len(got)-20:])
	}

	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"ascii_short_unchanged", "  abc  ", 5, "abc"},
		{"ascii_cut_same_as_bytes", "abcdef", 3, "abc…(已截断)"},
		{"cjk_cut_by_rune", "一二三四", 2, "一二…(已截断)"},
		{"invalid_utf8_sanitized", "a\xffb", 10, "ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncate(tc.in, tc.n); got != tc.want {
				t.Errorf("truncate(%q, %d): got %q, want %q", tc.in, tc.n, got, tc.want)
			}
		})
	}
}
