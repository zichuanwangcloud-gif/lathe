// Package schedule 是定时建单的调度器（docs/features/scheduled-issue.md）。
//
// 它只做一件事：到点用属主自己的 Linear 凭据在 Linear 上建一张单。之后那张
// 单经属主的 webhook 回到 Lathe，走现有接单 → 分诊 → 验证 → PR，与人手工
// 建的单完全一样。本包**不写 tasks 表、不 import task/runner/creds**
// （依赖白名单：internal/store、internal/integration/linear 与标准库）。
package schedule

import (
	"fmt"
	"strings"
	"time"

	// 内嵌时区库：多节点下不能一台机器有 zoneinfo、另一台没有，
	// 导致同一规则在不同节点上算出不同时刻。放在本包而不是 main：
	// 本包的测试二进制同样带上这份时区库。
	_ "time/tzdata"
)

// 规则类型。不做 cron：三种结构化规则覆盖目标用例，且每一种都好校验。
const (
	KindDaily    = "daily"
	KindWeekly   = "weekly"
	KindInterval = "interval"
)

// MinInterval / MaxInterval 是 interval 规则的上下限（产品常量，不做配置）：
// 每次触发都会建一张单、跑一轮 agent，小于 1 小时只制造噪声与成本；
// 长于 7 天就该用 weekly。
const (
	MinInterval = 60 * time.Minute
	MaxInterval = 7 * 24 * time.Hour
)

// Spec 是一条规则的时间定义。计划时刻是它的纯函数，与哪台节点、何时计算无关。
type Spec struct {
	Kind            string
	MinuteOfDay     int // daily/weekly：0–1439
	Weekdays        int // weekly：位掩码 bit0=周日 … bit6=周六（与 time.Weekday 对齐）
	IntervalMinutes int // interval
	Anchor          time.Time
	Loc             *time.Location
}

// LoadLocation 加载 IANA 时区（本包内嵌 tzdata，各节点结果一致）。
func LoadLocation(name string) (*time.Location, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("时区不能为空（got %q, want IANA 时区名，如 Asia/Shanghai）", name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("时区 %q 无法加载（want IANA 时区名，如 Asia/Shanghai）: %w", name, err)
	}
	return loc, nil
}

// Validate 校验规则形状；错误信息带 got/want。
func (s Spec) Validate() error {
	if s.Loc == nil {
		return fmt.Errorf("时区缺失（got nil, want 已加载的时区）")
	}
	switch s.Kind {
	case KindDaily, KindWeekly:
		if s.MinuteOfDay < 0 || s.MinuteOfDay > 1439 {
			return fmt.Errorf("每日时刻越界（got minuteOfDay=%d, want 0–1439）", s.MinuteOfDay)
		}
		if s.Kind == KindWeekly && (s.Weekdays < 1 || s.Weekdays > 127) {
			return fmt.Errorf("星期选择非法（got weekdays=%d, want 1–127，至少选一天）", s.Weekdays)
		}
	case KindInterval:
		iv := time.Duration(s.IntervalMinutes) * time.Minute
		if iv < MinInterval || iv > MaxInterval || s.IntervalMinutes%60 != 0 {
			return fmt.Errorf("间隔非法（got intervalMinutes=%d, want 60–10080 且是 60 的整数倍）", s.IntervalMinutes)
		}
		if s.Anchor.IsZero() {
			return fmt.Errorf("间隔规则缺少锚点（got 零值, want anchorAt）")
		}
	default:
		return fmt.Errorf("规则类型非法（got kind=%q, want daily / weekly / interval）", s.Kind)
	}
	return nil
}

// Next 返回严格大于 after 的第一个计划时刻（对齐整分钟）。
//
// 理论上不会出现的解析失败（某天无候选）时跳过那一天；全部失败返回零值，
// 调度器的前移防御（next.After(now) 不成立 → 暂停规则）会把它拦下来，不猜。
func (s Spec) Next(after time.Time) time.Time {
	if s.Kind == KindInterval {
		iv := time.Duration(s.IntervalMinutes) * time.Minute
		anchor := s.Anchor.Truncate(time.Minute)
		if iv <= 0 {
			return time.Time{}
		}
		if after.Before(anchor) {
			return anchor
		}
		k := after.Sub(anchor) / iv
		return anchor.Add((k + 1) * iv)
	}
	local := after.In(s.Loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	for i := 0; i < 10; i++ {
		d := day.AddDate(0, 0, i)
		if !s.dayMatches(d.Weekday()) {
			continue
		}
		t, err := resolve(d.Year(), d.Month(), d.Day(), s.MinuteOfDay/60, s.MinuteOfDay%60, s.Loc)
		if err != nil {
			continue
		}
		if t.After(after) {
			return t
		}
	}
	return time.Time{}
}

// Prev 返回 ≤ atOrBefore 的最后一个计划时刻；interval 规则在锚点之前没有。
func (s Spec) Prev(atOrBefore time.Time) (time.Time, bool) {
	if s.Kind == KindInterval {
		iv := time.Duration(s.IntervalMinutes) * time.Minute
		anchor := s.Anchor.Truncate(time.Minute)
		if iv <= 0 || atOrBefore.Before(anchor) {
			return time.Time{}, false
		}
		k := atOrBefore.Sub(anchor) / iv
		return anchor.Add(k * iv), true
	}
	local := atOrBefore.In(s.Loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	for i := 0; i < 10; i++ {
		d := day.AddDate(0, 0, -i)
		if !s.dayMatches(d.Weekday()) {
			continue
		}
		t, err := resolve(d.Year(), d.Month(), d.Day(), s.MinuteOfDay/60, s.MinuteOfDay%60, s.Loc)
		if err != nil {
			continue
		}
		if !t.After(atOrBefore) {
			return t, true
		}
	}
	return time.Time{}, false
}

// CountBetween 数 (from, to) 开区间内的计划时刻，封顶 limit。
func (s Spec) CountBetween(from, to time.Time, limit int) int {
	n := 0
	for t := s.Next(from); !t.IsZero() && t.Before(to) && n < limit; t = s.Next(t) {
		n++
	}
	return n
}

func (s Spec) dayMatches(wd time.Weekday) bool {
	if s.Kind != KindWeekly {
		return true
	}
	return s.Weekdays&(1<<uint(wd)) != 0
}

// resolve 把本地日期 D 的墙钟 HH:MM 解析成恰好一个时刻（DST 规则自己定、自己测，
// 不依赖 Go 对不存在/重复墙钟「不保证」的归一化）：
//
//   - 重复的墙钟（秋季拨慢，两个候选都吻合）：取较早那个；
//   - 跳过的墙钟（春季拨快，没有候选吻合）：用跳变前的偏移解释该墙钟，
//     即推迟跳变量那么久（1h 跳变时与 Vixie cron 一致；对 30 分钟、2 小时
//     的跳变同样成立，不写死 1 小时）。
//
// 前提：一个本地日期内最多一次偏移跳变（tzdata 全量里成立）。
func resolve(y int, m time.Month, d, hh, mm int, loc *time.Location) (time.Time, error) {
	probe := time.Date(y, m, d, 12, 0, 0, 0, loc)
	_, offA := probe.Add(-36 * time.Hour).Zone()
	_, offB := probe.Add(36 * time.Hour).Zone()
	offs := []int{offA}
	if offB != offA {
		offs = append(offs, offB)
	}
	var best time.Time
	for _, off := range offs {
		c := time.Date(y, m, d, hh, mm, 0, 0, time.FixedZone("", off)).In(loc)
		if c.Year() == y && c.Month() == m && c.Day() == d && c.Hour() == hh && c.Minute() == mm {
			if best.IsZero() || c.Before(best) {
				best = c
			}
		}
	}
	if !best.IsZero() {
		return best, nil
	}
	if offA == offB {
		return time.Time{}, fmt.Errorf("时区 %s 在 %04d-%02d-%02d %02d:%02d 无法解析出时刻", loc, y, m, d, hh, mm)
	}
	return time.Date(y, m, d, hh, mm, 0, 0, time.FixedZone("", offA)).In(loc), nil
}

var weekdayNames = [7]string{"日", "一", "二", "三", "四", "五", "六"}

// Describe 给人读的规则描述，如「每周一、三 09:00（Asia/Shanghai）」。
func (s Spec) Describe() string {
	tz := "UTC"
	if s.Loc != nil {
		tz = s.Loc.String()
	}
	hm := fmt.Sprintf("%02d:%02d", s.MinuteOfDay/60, s.MinuteOfDay%60)
	switch s.Kind {
	case KindDaily:
		return fmt.Sprintf("每天 %s（%s）", hm, tz)
	case KindWeekly:
		var days []string
		for i := 0; i < 7; i++ {
			// 周一排前，周日放最后，符合中文习惯
			wd := (i + 1) % 7
			if s.Weekdays&(1<<uint(wd)) != 0 {
				days = append(days, weekdayNames[wd])
			}
		}
		return fmt.Sprintf("每周%s %s（%s）", strings.Join(days, "、"), hm, tz)
	case KindInterval:
		return fmt.Sprintf("每 %d 小时（自 %s 起）", s.IntervalMinutes/60, s.Anchor.In(s.Loc).Format("2006-01-02 15:04 MST"))
	}
	return s.Kind
}
