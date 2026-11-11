package yunxiao

import (
	"html"
	"regexp"
	"strings"
)

var (
	// script/style 连内容一起丢：它们不是给人看的正文。
	dropBlockRe = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</(script|style)\s*>`)
	// 换行类标签：<br>、块级元素的结束标签。
	breakTagRe = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|li|h[1-6]|tr|blockquote|pre)\s*>`)
	// 其余标签一律剥掉。属性值里的 > 要整体跳过，所以按引号分段匹配。
	anyTagRe = regexp.MustCompile(`<(?:[^>"']|"[^"]*"|'[^']*')*>`)
	// 连续空行收成一行。
	multiNLRe = regexp.MustCompile(`\n{2,}`)
)

// ToPlainText 把云效评论 / 描述转成交给 agent 的纯文本。
//
// MARKDOWN 原样返回；RICHTEXT（及未标注格式的内容，云效默认就是
// RICHTEXT）用 stdlib 正则做标签剥离 + html.UnescapeString——不引入
// golang.org/x/net/html。全程按字符串操作，不按 byte 截断，多字节安全。
func ToPlainText(content, format string) string {
	if strings.EqualFold(strings.TrimSpace(format), "MARKDOWN") {
		return content
	}
	s := dropBlockRe.ReplaceAllString(content, "")
	s = breakTagRe.ReplaceAllString(s, "\n")
	s = anyTagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	s = strings.Join(lines, "\n")
	s = multiNLRe.ReplaceAllString(s, "\n")
	return strings.Trim(s, "\n ")
}
