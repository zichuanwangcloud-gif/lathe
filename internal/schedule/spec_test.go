package schedule

import (
	"strings"
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func daily(loc *time.Location, hh, mm int) Spec {
	return Spec{Kind: KindDaily, MinuteOfDay: hh*60 + mm, Loc: loc}
}

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestSpecNext(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	lh := mustLoc(t, "Australia/Lord_Howe")
	troll := mustLoc(t, "Antarctica/Troll")
	sh := mustLoc(t, "Asia/Shanghai")

	cases := []struct {
		name  string
		spec  Spec
		after time.Time
		want  time.Time
	}{
		{"daily/next_same_day", daily(time.UTC, 9, 0), utc("2026-10-19T08:00:00Z"), utc("2026-10-19T09:00:00Z")},
		{"daily/next_rolls_to_tomorrow", daily(time.UTC, 9, 0), utc("2026-10-19T09:00:00Z"), utc("2026-10-20T09:00:00Z")},
		// 2026-10-19 是周一；只选周一 → 周一 09:00 之后下一次是下周一
		{"weekly/wraps_to_next_week", Spec{Kind: KindWeekly, MinuteOfDay: 540, Weekdays: 1 << time.Monday, Loc: time.UTC},
			utc("2026-10-19T09:30:00Z"), utc("2026-10-26T09:00:00Z")},
		{"weekly/multi_days", Spec{Kind: KindWeekly, MinuteOfDay: 540, Weekdays: 1<<time.Monday | 1<<time.Wednesday, Loc: time.UTC},
			utc("2026-10-19T09:30:00Z"), utc("2026-10-21T09:00:00Z")},
		{"interval/anchor_aligned", Spec{Kind: KindInterval, IntervalMinutes: 120, Anchor: utc("2026-10-19T00:00:00Z"), Loc: time.UTC},
			utc("2026-10-19T03:10:00Z"), utc("2026-10-19T04:00:00Z")},
		{"interval/before_anchor_is_anchor", Spec{Kind: KindInterval, IntervalMinutes: 60, Anchor: utc("2026-10-19T00:00:00Z"), Loc: time.UTC},
			utc("2026-10-18T03:10:00Z"), utc("2026-10-19T00:00:00Z")},
		{"tz/Asia_Shanghai", daily(sh, 9, 0), utc("2026-10-18T02:00:00Z"), utc("2026-10-19T01:00:00Z")},
		{"dst/NY_spring_forward_0230_fires_0330_EDT", daily(ny, 2, 30), utc("2026-03-08T00:00:00Z"), utc("2026-03-08T07:30:00Z")},
		{"dst/NY_fall_back_0130_fires_earlier_EDT", daily(ny, 1, 30), utc("2026-11-01T00:00:00Z"), utc("2026-11-01T05:30:00Z")},
		{"dst/LordHowe_30m_spring_0215_fires_0245", daily(lh, 2, 15), utc("2026-10-03T00:00:00Z"), utc("2026-10-03T15:45:00Z")},
		{"dst/LordHowe_30m_fall_0145_fires_earlier", daily(lh, 1, 45), utc("2026-04-04T00:00:00Z"), utc("2026-04-04T14:45:00Z")},
		{"dst/Troll_2h_spring_0130_fires_0330", daily(troll, 1, 30), utc("2026-03-28T12:00:00Z"), utc("2026-03-29T01:30:00Z")},
		{"dst/Troll_2h_fall_0130_fires_earlier", daily(troll, 1, 30), utc("2026-10-24T12:00:00Z"), utc("2026-10-24T23:30:00Z")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.spec.Next(tc.after)
			if !got.Equal(tc.want) {
				t.Errorf("Next(%s): got %s, want %s", tc.after.Format(time.RFC3339), got.UTC().Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
			if got.Second() != 0 || got.Nanosecond() != 0 {
				t.Errorf("计划时刻必须对齐整分钟: got %s", got)
			}
		})
	}
}

func TestSpecPrev(t *testing.T) {
	cases := []struct {
		name   string
		spec   Spec
		at     time.Time
		want   time.Time
		wantOK bool
	}{
		{"daily/at_slot_inclusive", daily(time.UTC, 9, 0), utc("2026-10-19T09:00:00Z"), utc("2026-10-19T09:00:00Z"), true},
		{"daily/before_slot_is_yesterday", daily(time.UTC, 9, 0), utc("2026-10-19T08:59:00Z"), utc("2026-10-18T09:00:00Z"), true},
		{"weekly/last_monday", Spec{Kind: KindWeekly, MinuteOfDay: 540, Weekdays: 1 << time.Monday, Loc: time.UTC},
			utc("2026-10-25T12:00:00Z"), utc("2026-10-19T09:00:00Z"), true},
		{"interval/anchor_aligned", Spec{Kind: KindInterval, IntervalMinutes: 120, Anchor: utc("2026-10-19T00:00:00Z"), Loc: time.UTC},
			utc("2026-10-19T03:10:00Z"), utc("2026-10-19T02:00:00Z"), true},
		{"interval/before_anchor_has_no_prev", Spec{Kind: KindInterval, IntervalMinutes: 60, Anchor: utc("2026-10-19T00:00:00Z"), Loc: time.UTC},
			utc("2026-10-18T23:59:00Z"), time.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.spec.Prev(tc.at)
			if ok != tc.wantOK || (ok && !got.Equal(tc.want)) {
				t.Errorf("Prev(%s): got (%s, %v), want (%s, %v)", tc.at.Format(time.RFC3339), got.UTC().Format(time.RFC3339), ok, tc.want.Format(time.RFC3339), tc.wantOK)
			}
		})
	}
}

// 每个本地日期恰好触发一次，且 Next 严格递增（阶段 A 前移的前提）。
func TestSpecDSTFiresOncePerLocalDay(t *testing.T) {
	cases := []struct {
		tz     string
		hh, mm int
	}{
		{"America/New_York", 1, 30}, {"America/New_York", 2, 30},
		{"Australia/Lord_Howe", 1, 45}, {"Australia/Lord_Howe", 2, 15},
		{"Antarctica/Troll", 1, 30}, {"Antarctica/Troll", 2, 30},
	}
	for _, tc := range cases {
		t.Run(strings.ReplaceAll(tc.tz, "/", "_")+"_"+time.Date(0, 1, 1, tc.hh, tc.mm, 0, 0, time.UTC).Format("1504"), func(t *testing.T) {
			loc := mustLoc(t, tc.tz)
			s := daily(loc, tc.hh, tc.mm)
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, loc)
			end := time.Date(2027, 1, 1, 0, 0, 0, 0, loc)
			perDay := map[string]int{}
			cur := start
			for i := 0; i < 1000; i++ {
				next := s.Next(cur)
				if !next.After(cur) {
					t.Fatalf("Next 必须严格递增: Next(%s) = %s", cur, next)
				}
				if !next.Before(end) {
					break
				}
				perDay[next.In(loc).Format("2006-01-02")]++
				cur = next
			}
			days := 0
			for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
				days++
				if n := perDay[d.Format("2006-01-02")]; n != 1 {
					t.Errorf("%s 触发次数: got %d, want 1", d.Format("2006-01-02"), n)
				}
			}
			if days != 365 {
				t.Fatalf("天数: got %d, want 365", days)
			}
		})
	}
}

func TestSpecValidate(t *testing.T) {
	anchor := utc("2026-10-19T00:00:00Z")
	cases := []struct {
		name   string
		spec   Spec
		errHas string
	}{
		{"interval_59", Spec{Kind: KindInterval, IntervalMinutes: 59, Anchor: anchor, Loc: time.UTC}, "59"},
		{"interval_90_not_hour_multiple", Spec{Kind: KindInterval, IntervalMinutes: 90, Anchor: anchor, Loc: time.UTC}, "90"},
		{"interval_10140", Spec{Kind: KindInterval, IntervalMinutes: 10140, Anchor: anchor, Loc: time.UTC}, "10140"},
		{"weekdays_0", Spec{Kind: KindWeekly, MinuteOfDay: 540, Weekdays: 0, Loc: time.UTC}, "0"},
		{"weekdays_128", Spec{Kind: KindWeekly, MinuteOfDay: 540, Weekdays: 128, Loc: time.UTC}, "128"},
		{"minute_1440", Spec{Kind: KindDaily, MinuteOfDay: 1440, Loc: time.UTC}, "1440"},
		{"kind_cron", Spec{Kind: "cron", Loc: time.UTC}, "cron"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if err == nil {
				t.Fatalf("got nil, want 错误含 %q", tc.errHas)
			}
			if !strings.Contains(err.Error(), tc.errHas) || !strings.Contains(err.Error(), "want") {
				t.Errorf("错误信息应带 got/want 与 %q，got %q", tc.errHas, err.Error())
			}
		})
	}

	t.Run("timezone_Mars_Base", func(t *testing.T) {
		_, err := LoadLocation("Mars/Base")
		if err == nil || !strings.Contains(err.Error(), "Mars/Base") {
			t.Errorf("got %v, want 含 Mars/Base 的错误", err)
		}
	})
	t.Run("valid_specs_pass", func(t *testing.T) {
		for _, s := range []Spec{
			daily(time.UTC, 0, 0),
			{Kind: KindWeekly, MinuteOfDay: 1439, Weekdays: 127, Loc: time.UTC},
			{Kind: KindInterval, IntervalMinutes: 10080, Anchor: anchor, Loc: time.UTC},
		} {
			if err := s.Validate(); err != nil {
				t.Errorf("%+v: got %v, want nil", s, err)
			}
		}
	})
}

func TestSpecDescribe(t *testing.T) {
	sh := mustLoc(t, "Asia/Shanghai")
	s := Spec{Kind: KindWeekly, MinuteOfDay: 540, Weekdays: 1<<time.Monday | 1<<time.Wednesday, Loc: sh}
	if got, want := s.Describe(), "每周一、三 09:00（Asia/Shanghai）"; got != want {
		t.Errorf("Describe: got %q, want %q", got, want)
	}
}
