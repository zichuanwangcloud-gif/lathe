package schedule

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRenderTemplate(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	slot := utc("2026-10-19T01:00:00Z") // 上海 2026-10-19 周一 09:00
	vars := Vars(slot, sh, "依赖升级")

	cases := []struct {
		name string
		tpl  string
		want string
	}{
		{"date", "{{date}}", "2026-10-19"},
		{"time", "{{time}}", "09:00"},
		{"weekday", "{{weekday}}", "周一"},
		{"datetime", "{{datetime}}", "2026-10-19T09:00:00+08:00"},
		{"week", "{{week}}", "2026-W43"},
		{"schedule", "{{schedule}}", "依赖升级"},
		{"spaces_allowed", "{{ date }} / {{  time}}", "2026-10-19 / 09:00"},
		{"literal_text_kept", "每周 {{schedule}}：{{date}}", "每周 依赖升级：2026-10-19"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Render(tc.tpl, vars); got != tc.want {
				t.Errorf("Render(%q): got %q, want %q", tc.tpl, got, tc.want)
			}
		})
	}

	t.Run("title_truncated_by_rune", func(t *testing.T) {
		got := RenderTitle(strings.Repeat("汉", 300), vars)
		if !utf8.ValidString(got) || utf8.RuneCountInString(got) > 255 {
			t.Errorf("got valid=%v runes=%d, want valid 且 ≤255", utf8.ValidString(got), utf8.RuneCountInString(got))
		}
	})
}

func TestValidateTemplate(t *testing.T) {
	cases := []struct {
		name   string
		tpl    string
		errHas string
	}{
		{"known_ok", "{{date}} {{ time }} {{weekday}} {{datetime}} {{week}} {{schedule}}", ""},
		{"plain_ok", "没有占位符", ""},
		{"typo_rejected", "标题 {{dat}}", "dat"},
		{"go_template_field_rejected", "{{.Env}}", ".Env"},
		{"go_template_func_rejected", `{{printf "%s" .}}`, "printf"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTemplate(tc.tpl)
			if tc.errHas == "" {
				if err != nil {
					t.Errorf("got %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.errHas) {
				t.Errorf("got %v, want 错误含 %q", err, tc.errHas)
			}
		})
	}

	t.Run("not_executed", func(t *testing.T) {
		vars := Vars(time.Now(), time.UTC, "x")
		if got := Render("{{.Env}}", vars); got != "{{.Env}}" {
			t.Errorf("未知占位符不得被执行/替换: got %q", got)
		}
	})
}
