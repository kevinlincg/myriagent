package schedules

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 觸發條件的中文摘要（排程頁的「何時」）與一段時間內的預定執行時間（「接下來 24 小時」的時間軸、判斷兩次觀察之間有沒有漏看的執行）。
// 時間一律是本機時間。登入、開機、事件這類不是時間觸發的不展開（C# 的 ScheduleTriggerRules）

// MaxOccurrences 一個工作在一段時間內最多展開幾次
const MaxOccurrences = 200

var dayText = []string{"日", "一", "二", "三", "四", "五", "六"}

// DescribeAll 整個工作的摘要：各觸發條件以「；」接起來；沒有觸發條件是「只能手動執行」
func DescribeAll(triggers []Trigger) string {
	if len(triggers) == 0 {
		return "只能手動執行"
	}
	var parts []string
	seen := map[string]bool{}
	for _, t := range triggers {
		d := Describe(t)
		if !seen[d] {
			seen[d] = true
			parts = append(parts, d)
		}
	}
	return strings.Join(parts, "；")
}

// Describe 一個觸發條件的摘要
func Describe(t Trigger) string {
	var text string
	switch t.Kind {
	case TriggerOnce:
		text = describeOnce(t)
	case TriggerDaily:
		p := "每天"
		if t.DaysInterval > 1 {
			p = fmt.Sprintf("每 %d 天", t.DaysInterval)
		}
		text = describeRepeating(t, p)
	case TriggerWeekly:
		text = describeRepeating(t, weeklyText(t))
	case TriggerMonthly:
		text = describeRepeating(t, monthlyText(t))
	case TriggerMonthlyDayOfWeek:
		text = describeRepeating(t, monthlyDowText(t))
	case TriggerLogon:
		if t.UserID != "" {
			text = shortUser(t.UserID) + " 登入時"
		} else {
			text = "任何人登入時"
		}
		text += delayText(t)
	case TriggerBoot:
		text = "開機時" + delayText(t)
	case TriggerIdle:
		text = "電腦閒置時"
	case TriggerEvent:
		text = "發生特定事件時" + delayText(t)
	case TriggerRegistration:
		text = "建立或修改時" + delayText(t)
	case TriggerSessionStateChange:
		text = sessionText(t.StateChange) + delayText(t)
	default:
		text = "其他條件"
	}
	if !t.Enabled {
		text += "（已停用）"
	}
	return text
}

func describeOnce(t Trigger) string {
	if t.Start == nil {
		return "一次"
	}
	if t.RepeatInterval > 0 {
		head := t.Start.Format("1/2 15:04") + " 起" + IntervalText(t.RepeatInterval)
		if t.RepeatDuration > 0 {
			return head + "，持續" + SpanText(t.RepeatDuration)
		}
		return head
	}
	return "一次：" + t.Start.Format("2006/1/2 15:04")
}

// describeRepeating 每天、每週、每月：沒有重複間隔是「每天 02:30」；重複間隔蓋滿整天是「每 1 小時（00:20 起）」，否則「每天 08:00 起每 15 分鐘，持續 10 小時」
func describeRepeating(t Trigger, period string) string {
	tm := ""
	if t.Start != nil {
		tm = t.Start.Format("15:04")
	}
	if t.RepeatInterval <= 0 {
		return strings.TrimRight(period+" "+tm, " ")
	}
	coversDay := t.Kind == TriggerDaily && t.DaysInterval == 1 && (t.RepeatDuration <= 0 || t.RepeatDuration >= 24*time.Hour)
	if coversDay {
		return strings.TrimLeft(IntervalText(t.RepeatInterval), " ") + "（" + tm + " 起）"
	}
	head := period + " " + tm + " 起" + IntervalText(t.RepeatInterval)
	if t.RepeatDuration > 0 {
		return head + "，持續" + SpanText(t.RepeatDuration)
	}
	return head
}

func hasDay(days []time.Weekday, d time.Weekday) bool {
	for _, x := range days {
		if x == d {
			return true
		}
	}
	return false
}

func mondayFirst(days []time.Weekday) []time.Weekday {
	out := append([]time.Weekday{}, days...)
	sort.SliceStable(out, func(i, j int) bool { return (int(out[i])+6)%7 < (int(out[j])+6)%7 })
	return out
}

func weeklyText(t Trigger) string {
	days := t.DaysOfWeek
	var which string
	switch {
	case len(days) == 7:
		which = "每天"
	case len(days) == 5 && !hasDay(days, time.Saturday) && !hasDay(days, time.Sunday):
		which = "平日"
	case len(days) == 2 && hasDay(days, time.Saturday) && hasDay(days, time.Sunday):
		which = "週末"
	default:
		var names []string
		for _, d := range mondayFirst(days) {
			names = append(names, dayText[d])
		}
		which = "週" + strings.Join(names, "、")
	}
	if t.WeeksInterval > 1 {
		return fmt.Sprintf("每 %d 週的%s", t.WeeksInterval, which)
	}
	if which == "每天" || which == "平日" || which == "週末" {
		return which
	}
	return "每" + which
}

func monthlyText(t Trigger) string {
	var parts []string
	for _, d := range t.DaysOfMonth {
		parts = append(parts, strconv.Itoa(d))
	}
	days := ""
	if len(parts) > 0 {
		days = strings.Join(parts, "、") + " 日"
	}
	if t.LastDayOfMonth {
		if days != "" {
			days += "與最後一天"
		} else {
			days = "最後一天"
		}
	}
	prefix := monthsPrefix(t.Months)
	if days != "" && days[0] >= '0' && days[0] <= '9' {
		return prefix + " " + days
	}
	return prefix + days
}

func monthlyDowText(t Trigger) string {
	var weeks []string
	for _, w := range t.WeeksOfMonth {
		switch w {
		case 1:
			weeks = append(weeks, "第一個")
		case 2:
			weeks = append(weeks, "第二個")
		case 3:
			weeks = append(weeks, "第三個")
		case 4:
			weeks = append(weeks, "第四個")
		default:
			weeks = append(weeks, "")
		}
	}
	if t.LastWeekOfMonth {
		weeks = append(weeks, "最後一個")
	}
	var days []string
	for _, d := range mondayFirst(t.DaysOfWeek) {
		days = append(days, "週"+dayText[d])
	}
	return monthsPrefix(t.Months) + strings.Join(weeks, "與") + strings.Join(days, "、")
}

func monthsPrefix(months []int) string {
	if len(months) == 0 || len(months) == 12 {
		return "每月"
	}
	var s []string
	for _, m := range months {
		s = append(s, strconv.Itoa(m))
	}
	return "每年 " + strings.Join(s, "、") + " 月的"
}

func sessionText(state string) string {
	switch state {
	case "SessionLock":
		return "鎖定時"
	case "SessionUnlock":
		return "解除鎖定時"
	case "ConsoleConnect":
		return "本機連線時"
	case "ConsoleDisconnect":
		return "本機中斷連線時"
	case "RemoteConnect":
		return "遠端連線時"
	case "RemoteDisconnect":
		return "遠端中斷連線時"
	}
	return "工作階段變更時"
}

func delayText(t Trigger) string {
	if t.Delay > 0 {
		return "（延遲" + SpanText(t.Delay) + "）"
	}
	return ""
}

func shortUser(user string) string {
	if i := strings.LastIndex(user, `\`); i >= 0 {
		return user[i+1:]
	}
	return user
}

// IntervalText 重複間隔：「 每 15 分鐘」「 每小時」「 每 2 小時」（開頭帶一個空白，接在時間後面）
func IntervalText(interval time.Duration) string {
	if interval < time.Hour {
		return fmt.Sprintf(" 每 %d 分鐘", int(math.Max(1, interval.Minutes())))
	}
	if interval == time.Hour {
		return " 每小時"
	}
	if interval < 24*time.Hour && interval%time.Hour == 0 {
		return fmt.Sprintf(" 每 %d 小時", int(interval.Hours()))
	}
	return " 每" + SpanText(interval)
}

// SpanText 期間的中文：「 10 小時」「 1 天」「 1 小時 30 分」（開頭帶一個空白）
func SpanText(span time.Duration) string {
	var sb strings.Builder
	days := int(span / (24 * time.Hour))
	hours := int(span/time.Hour) % 24
	mins := int(span/time.Minute) % 60
	if days > 0 {
		fmt.Fprintf(&sb, " %d 天", days)
	}
	if hours > 0 {
		fmt.Fprintf(&sb, " %d 小時", hours)
	}
	if mins > 0 {
		fmt.Fprintf(&sb, " %d 分", mins)
	}
	if sb.Len() == 0 {
		fmt.Fprintf(&sb, " %d 秒", int(span.Seconds()))
	}
	return sb.String()
}

// dateOf 那一天的 0 點（同一個時區）
func dateOf(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// atTimeOf 某一天＋另一個時間的時分秒（牆上時間，同 C# 的 day + start.TimeOfDay）
func atTimeOf(day, tod time.Time) time.Time {
	y, m, d := day.Date()
	return time.Date(y, m, d, tod.Hour(), tod.Minute(), tod.Second(), 0, day.Location())
}

// daysBetween 兩個日期差幾天（看日曆，不受夏令時間影響）
func daysBetween(a, b time.Time) int {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return int(time.Date(by, bm, bd, 0, 0, 0, 0, time.UTC).Sub(time.Date(ay, am, ad, 0, 0, 0, 0, time.UTC)).Hours() / 24)
}

// Occurrences 在 [from, to) 之間預定的執行時間（由小到大、去重、最多 max 次）。只展開時間觸發的（一次、每天、每週、每月）；
// 停用的觸發條件、過了到期時間的不算。無限期重複的每天觸發，前一天開始的那串也延續到今天
func Occurrences(triggers []Trigger, from, to time.Time, max int) []time.Time {
	set := map[int64]time.Time{}
	add := func(t Trigger, x time.Time) {
		if x.Before(from) || !x.Before(to) {
			return
		}
		if t.End != nil && x.After(*t.End) {
			return
		}
		set[x.UnixNano()] = x
	}
	for _, t := range triggers {
		if !t.Enabled || t.Start == nil {
			continue
		}
		switch t.Kind {
		case TriggerOnce, TriggerDaily, TriggerWeekly, TriggerMonthly, TriggerMonthlyDayOfWeek:
		default:
			continue
		}
		start := *t.Start
		var bases []time.Time
		if t.Kind == TriggerOnce {
			bases = append(bases, start)
		} else {
			// 前一個基準開始的重複可能延續到 from 之後：往回看重複持續的長度（無限期的看一天）
			var lookBack time.Duration
			if t.RepeatInterval > 0 {
				lookBack = t.RepeatDuration
				if lookBack <= 0 {
					lookBack = 24 * time.Hour
				}
			}
			if lookBack > 31*24*time.Hour {
				lookBack = 31 * 24 * time.Hour
			}
			startDay := dateOf(start.In(from.Location()))
			for day := dateOf(from.Add(-lookBack)); day.Before(to); day = day.AddDate(0, 0, 1) {
				if day.Before(startDay) || !MatchesDay(t, day, start) {
					continue
				}
				bases = append(bases, atTimeOf(day, start.In(from.Location())))
			}
		}
		for _, b := range bases {
			if b.Before(start) {
				continue
			}
			if t.RepeatInterval <= 0 {
				add(t, b)
				continue
			}
			var until time.Time
			switch {
			case t.RepeatDuration > 0:
				until = b.Add(t.RepeatDuration)
			case t.Kind == TriggerOnce:
				until = to
			default:
				until = b.AddDate(0, 0, 1)
			}
			if until.After(to) {
				until = to
			}
			first := b
			if b.Before(from) {
				steps := int64(math.Ceil(float64(from.Sub(b)) / float64(t.RepeatInterval)))
				first = b.Add(time.Duration(steps) * t.RepeatInterval)
			}
			for x := first; x.Before(until); x = x.Add(t.RepeatInterval) {
				add(t, x)
				if len(set) > max*2 {
					break
				}
			}
		}
	}
	out := make([]time.Time, 0, len(set))
	for _, x := range set {
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	if len(out) > max {
		out = out[:max]
	}
	return out
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// MatchesDay 這一天是不是觸發的日子
func MatchesDay(t Trigger, day, start time.Time) bool {
	switch t.Kind {
	case TriggerDaily:
		n := t.DaysInterval
		if n < 1 {
			n = 1
		}
		return daysBetween(start, day)%n == 0
	case TriggerWeekly:
		if !hasDay(t.DaysOfWeek, day.Weekday()) {
			return false
		}
		if t.WeeksInterval <= 1 {
			return true
		}
		startWeek := dateOf(start).AddDate(0, 0, -int(start.Weekday()))
		week := daysBetween(startWeek, dateOf(day).AddDate(0, 0, -int(day.Weekday()))) / 7
		return week%t.WeeksInterval == 0
	case TriggerMonthly:
		if len(t.Months) > 0 && !containsInt(t.Months, int(day.Month())) {
			return false
		}
		last := day.AddDate(0, 0, 1).Month() != day.Month()
		return containsInt(t.DaysOfMonth, day.Day()) || (t.LastDayOfMonth && last)
	case TriggerMonthlyDayOfWeek:
		if len(t.Months) > 0 && !containsInt(t.Months, int(day.Month())) {
			return false
		}
		if !hasDay(t.DaysOfWeek, day.Weekday()) {
			return false
		}
		week := (day.Day()-1)/7 + 1
		lastWeek := day.AddDate(0, 0, 7).Month() != day.Month()
		return containsInt(t.WeeksOfMonth, week) || (t.LastWeekOfMonth && lastWeek)
	}
	return false
}

// ---- 「分、時、日、月、星期」換成觸發條件（launchd 的 StartCalendarInterval、cron；C# 的 CalendarTriggerRules） ----

// BaseDate 時間觸發的基準日（只用它的時刻；要早於任何查詢的日期）
func BaseDate() time.Time { return time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local) }

// CalendarFromFields 沒寫的欄位是萬用字元：分與時都有＝那個時刻；只有分＝每小時的那一分；只有時＝那一小時裡每分鐘；都沒有＝每分鐘。
// 日與星期都指定時兩個都算（任何一個符合就執行，launchd 與 cron 都是這樣）
func CalendarFromFields(minute, hour, day, month, weekday *int) []Trigger {
	var months []int
	if month != nil {
		months = []int{*month}
	}
	var out []Trigger
	if day != nil {
		out = append(out, CalendarMake(TriggerMonthly, hour, minute, months, []int{*day}, nil, false))
	}
	if weekday != nil {
		if month == nil {
			out = append(out, CalendarMake(TriggerWeekly, hour, minute, nil, nil, []time.Weekday{time.Weekday(*weekday)}, false))
		} else {
			out = append(out, CalendarMake(TriggerMonthlyDayOfWeek, hour, minute, months, nil, []time.Weekday{time.Weekday(*weekday)}, true))
		}
	}
	if day == nil && weekday == nil {
		if month == nil {
			out = append(out, CalendarMake(TriggerDaily, hour, minute, nil, nil, nil, false))
		} else {
			all := make([]int, 31)
			for i := range all {
				all[i] = i + 1
			}
			out = append(out, CalendarMake(TriggerMonthly, hour, minute, months, all, nil, false))
		}
	}
	return out
}

// CalendarMake 種類＋時刻（hour、minute 沒有的照萬用字元換成重複間隔）
func CalendarMake(kind TriggerKind, hour, minute *int, months, daysOfMonth []int, daysOfWeek []time.Weekday, everyWeek bool) Trigger {
	var tod, interval, duration time.Duration
	switch {
	case hour != nil && minute != nil:
		tod = time.Duration(*hour)*time.Hour + time.Duration(*minute)*time.Minute
	case minute != nil:
		tod, interval = time.Duration(*minute)*time.Minute, time.Hour
	case hour != nil:
		tod, interval, duration = time.Duration(*hour)*time.Hour, time.Minute, time.Hour
	default:
		interval = time.Minute
	}
	return CalendarMakeAt(kind, tod, interval, duration, months, daysOfMonth, daysOfWeek, everyWeek)
}

// CalendarMakeAt 種類＋時刻＋重複（cron 的 */15 之類由呼叫端算好間隔）
func CalendarMakeAt(kind TriggerKind, tod, interval, duration time.Duration, months, daysOfMonth []int, daysOfWeek []time.Weekday, everyWeek bool) Trigger {
	t := NewTrigger(kind)
	start := BaseDate().Add(tod)
	t.Start = &start
	t.RepeatInterval = interval
	if interval > 0 {
		t.RepeatDuration = duration
	}
	t.Months = months
	t.DaysOfMonth = daysOfMonth
	t.DaysOfWeek = daysOfWeek
	if everyWeek {
		t.WeeksOfMonth = []int{1, 2, 3, 4}
		t.LastWeekOfMonth = true
	}
	return t
}
