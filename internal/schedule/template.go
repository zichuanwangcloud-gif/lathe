package schedule

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// 模板只做白名单变量的字面替换，**不用 text/template**：模板由用户输入，
// text/template 能调方法、做循环，等于开了一个小型执行面，而这里只需要替换。

// 长度上限（按 rune 计）。
const (
	MaxNameRunes      = 100
	MaxTitleTplRunes  = 200
	MaxBodyTplRunes   = 8000
	MaxRenderedTitle  = 255 // Linear 标题上限
	maxStoredErrRunes = 1000
)

// Placeholders 是可用占位符（UI 展示用，顺序即展示顺序）。
var Placeholders = []string{"date", "time", "weekday", "datetime", "week", "schedule"}

var placeholderRe = regexp.MustCompile(`\{\{\s*([^{}]*?)\s*\}\}`)

// Vars 按规则时区给出计划时刻 slot 的全部占位符取值。
func Vars(slot time.Time, loc *time.Location, scheduleName string) map[string]string {
	if loc == nil {
		loc = time.UTC
	}
	t := slot.In(loc)
	y, w := t.ISOWeek()
	return map[string]string{
		"date":     t.Format("2006-01-02"),
		"time":     t.Format("15:04"),
		"weekday":  "周" + weekdayNames[t.Weekday()],
		"datetime": t.Format(time.RFC3339),
		"week":     fmt.Sprintf("%04d-W%02d", y, w),
		"schedule": scheduleName,
	}
}

// Render 把模板里的白名单占位符替换成取值；白名单以外的 {{…}} 原样保留
// （保存时 ValidateTemplate 已拒绝它们，这里只是不执行）。
func Render(tpl string, vars map[string]string) string {
	return placeholderRe.ReplaceAllStringFunc(tpl, func(m string) string {
		name := strings.TrimSpace(placeholderRe.FindStringSubmatch(m)[1])
		if v, ok := vars[name]; ok {
			return v
		}
		return m
	})
}

// RenderTitle 渲染标题并按 rune 截到 Linear 上限。
func RenderTitle(tpl string, vars map[string]string) string {
	return truncateRunes(strings.TrimSpace(Render(tpl, vars)), MaxRenderedTitle)
}

// ValidateTemplate 拒绝白名单以外的占位符，并指出是哪一个 ——
// 免得到点才发现标题里有一个原样的 {{dat}}。
func ValidateTemplate(tpl string) error {
	known := map[string]bool{}
	for _, p := range Placeholders {
		known[p] = true
	}
	for _, m := range placeholderRe.FindAllStringSubmatch(tpl, -1) {
		name := strings.TrimSpace(m[1])
		if !known[name] {
			return fmt.Errorf("未知占位符 {{%s}}（可用：%s）", name, "{{"+strings.Join(Placeholders, "}} {{")+"}}")
		}
	}
	return nil
}

// truncateRunes 按 rune 截断（AGENTS.md §9：字符串截断按 rune 不按 byte）。
func truncateRunes(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
