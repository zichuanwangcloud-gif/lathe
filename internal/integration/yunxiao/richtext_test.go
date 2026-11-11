package yunxiao

import (
	"testing"
	"unicode/utf8"
)

// AC-5
func TestToPlainText(t *testing.T) {
	cases := []struct {
		name, in, format, want string
	}{
		{"p_newline", "<p>第一段</p><p>第二段</p>", "RICHTEXT", "第一段\n第二段"},
		{"br_newline", "行一<br>行二<br/>行三<BR />行四", "RICHTEXT", "行一\n行二\n行三\n行四"},
		{"li_newline", "<ul><li>甲</li><li>乙</li></ul>", "RICHTEXT", "甲\n乙"},
		{"entity_unescape", "<p>a &lt; b &amp;&amp; c &gt; d &quot;q&quot; &#39;s&#39; &nbsp;x</p>", "RICHTEXT", "a < b && c > d \"q\" 's'  x"},
		{"script_dropped", "<p>正文</p><script>alert('x')</script><style>p{}</style><p>尾</p>", "RICHTEXT", "正文\n尾"},
		{"multibyte_intact", "<div>中文😀测试</div><span>émoji ✓</span>", "RICHTEXT", "中文😀测试\némoji ✓"},
		{"markdown_passthrough", "**粗体** <b>原样</b>", "MARKDOWN", "**粗体** <b>原样</b>"},
		{"attr_with_gt", `<a href="x>y">链接</a>`, "RICHTEXT", "链接"},
		{"empty_format_is_richtext", "<p>默认</p>", "", "默认"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ToPlainText(c.in, c.format)
			if got != c.want {
				t.Errorf("ToPlainText(%q, %q)\n got %q\nwant %q", c.in, c.format, got, c.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("结果不是合法 UTF-8: %q", got)
			}
		})
	}
}
