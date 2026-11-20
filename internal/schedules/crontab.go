package schedules

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// 使用者的 crontab（crontab -l 的輸出）讀成排程頁共用的定義（C# 的 CronRules）。純函式。
// 支援五個欄位（分 時 日 月 星期）的數字、範圍、清單、間隔（*/15、1-5、1,15、0-30/10）、月份與星期的英文縮寫，以及 @hourly、@daily、
// @weekly、@monthly、@yearly、@reboot。組合太多（展開超過 CronMaxTriggers 個時刻）的寫成「其他條件」，不亂猜。
// 註解、空行、環境變數（NAME=值）不是排程

// CronLogonType Definition.LogonType 的值
const CronLogonType = "Cron"

// CronMaxTriggers 一行最多展開成幾個觸發條件
const CronMaxTriggers = 48

// CronEntry crontab 的一行排程。Key＝指令的雜湊（改時間不會換一個身分；同一個指令排兩次時後面的加編號）
type CronEntry struct {
	Key        string
	Line       string
	Schedule   string
	Command    string
	Definition Definition
}

var cronAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\s*=`)

var cronMonthNames = []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
var cronDayNames = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// ParseCrontab 解析 crontab 的內容
func ParseCrontab(crontab, userName string) []CronEntry {
	var out []CronEntry
	seen := map[string]int{}
	for _, raw := range strings.Split(strings.ReplaceAll(crontab, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' || cronAssignment.MatchString(line) {
			continue
		}
		schedule, command, ok := splitCronLine(line)
		if !ok || command == "" {
			continue
		}
		program, arguments := SplitCommand(command)
		sum := sha256.Sum256([]byte(command))
		key := hex.EncodeToString(sum[:])[:10]
		seen[key]++
		if seen[key] > 1 {
			key += "-" + strconv.Itoa(seen[key])
		}
		out = append(out, CronEntry{Key: key, Line: line, Schedule: schedule, Command: command, Definition: Definition{
			Triggers:  CronTriggers(schedule),
			Actions:   []Action{{Kind: "Exec", Command: program, Arguments: arguments}},
			RunAs:     userName,
			LogonType: CronLogonType,
			Enabled:   true,
		}})
	}
	return out
}

// CronVariables crontab 裡設的環境變數（NAME=值 的行；值外層的引號拿掉）：立即執行時照樣帶上
func CronVariables(crontab string) map[string]string {
	out := map[string]string{}
	for _, raw := range strings.Split(strings.ReplaceAll(crontab, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' || !cronAssignment.MatchString(line) {
			continue
		}
		eq := strings.IndexByte(line, '=')
		value := strings.TrimSpace(line[eq+1:])
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		out[strings.TrimSpace(line[:eq])] = value
	}
	return out
}

var cronRedirect = regexp.MustCompile(`(^|[^0-9&])>>?\s*([^\s;&|<>]+)`)

// CronLogPath 指令把輸出轉向到哪個檔案（「>> 檔案」「> 檔案」的第一個；/dev/null、&1 這類不算）。cron 不記執行結果，
// 只能從這個檔案最後寫入的時間看出上次大概什麼時候跑過；沒有轉向回空字串
func CronLogPath(command string) string {
	for _, m := range cronRedirect.FindAllStringSubmatch(command, -1) {
		p := strings.Trim(m[2], `"'`)
		if p != "" && !strings.HasPrefix(p, "/dev/") && !strings.HasPrefix(p, "&") {
			return p
		}
	}
	return ""
}

// CronDisplayName 給人看的名稱：程式的檔名加上參數（轉向之前的），最多 60 個字
func CronDisplayName(command string) string {
	program, arguments := SplitCommand(command)
	name := program[strings.LastIndex(program, "/")+1:]
	if arguments != "" {
		cut := strings.IndexAny(arguments, ">|;&")
		end := cut
		// 「2>檔案」的 2 是轉向的一部分（緊貼著 >、前面是空白），不是參數
		if cut > 0 && arguments[cut] == '>' && arguments[cut-1] >= '0' && arguments[cut-1] <= '9' && (cut == 1 || arguments[cut-2] == ' ') {
			end = cut - 1
		}
		shown := arguments
		if cut >= 0 {
			shown = arguments[:end]
		}
		if shown = strings.TrimSpace(shown); shown != "" {
			name += " " + shown
		}
	}
	r := []rune(name)
	if len(r) <= 60 {
		return name
	}
	return string(r[:59]) + "…"
}

// splitCronLine 一行切成「時間」與「指令」；不是排程的寫法回 false
func splitCronLine(line string) (string, string, bool) {
	if strings.HasPrefix(line, "@") {
		i := strings.IndexAny(line, " \t")
		if i < 0 {
			return "", "", false
		}
		return line[:i], strings.TrimSpace(line[i:]), true
	}
	idx := 0
	for field := 0; field < 5; field++ {
		for idx < len(line) && unicode.IsSpace(rune(line[idx])) {
			idx++
		}
		start := idx
		for idx < len(line) && !unicode.IsSpace(rune(line[idx])) {
			idx++
		}
		if idx == start {
			return "", "", false
		}
	}
	if idx >= len(line) {
		return "", "", false
	}
	return strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx:]), true
}

// SplitCommand 指令切成程式與其餘的參數（第一個空白前；引號包著的整個算程式）
func SplitCommand(command string) (string, string) {
	command = strings.TrimSpace(command)
	if command != "" && (command[0] == '"' || command[0] == '\'') {
		if close := strings.IndexByte(command[1:], command[0]); close >= 0 {
			close++
			return command[1:close], strings.TrimSpace(command[close+1:])
		}
	}
	i := strings.IndexAny(command, " \t")
	if i < 0 {
		return command, ""
	}
	return command[:i], strings.TrimSpace(command[i:])
}

// CronTriggers 時間的寫法換成觸發條件；看不懂或太複雜的是一個「其他條件」
func CronTriggers(schedule string) []Trigger {
	unknown := []Trigger{NewTrigger(TriggerUnknown)}
	switch strings.ToLower(strings.TrimSpace(schedule)) {
	case "@reboot":
		return []Trigger{NewTrigger(TriggerBoot)}
	case "@hourly":
		schedule = "0 * * * *"
	case "@daily", "@midnight":
		schedule = "0 0 * * *"
	case "@weekly":
		schedule = "0 0 * * 0"
	case "@monthly":
		schedule = "0 0 1 * *"
	case "@yearly", "@annually":
		schedule = "0 0 1 1 *"
	}
	fields := strings.Fields(schedule)
	if len(fields) != 5 {
		return unknown
	}
	minute := cronField(fields[0], 0, 59, nil)
	hour := cronField(fields[1], 0, 23, nil)
	day := cronField(fields[2], 1, 31, nil)
	month := cronField(fields[3], 1, 12, cronMonthNames)
	weekday := cronField(fields[4], 0, 7, cronDayNames)
	if minute == nil || hour == nil || day == nil || month == nil || weekday == nil {
		return unknown
	}
	times := cronTimes(minute, hour)
	if times == nil {
		return unknown
	}
	var months []int
	if !month.all {
		months = month.values
	}
	wset := map[time.Weekday]bool{}
	for _, w := range weekday.values {
		wset[time.Weekday(w%7)] = true
	}
	var weekdays []time.Weekday
	for w := time.Sunday; w <= time.Saturday; w++ {
		if wset[w] {
			weekdays = append(weekdays, w)
		}
	}
	var out []Trigger
	for _, tm := range times {
		// 日與星期都指定時兩個都算（任何一個符合就執行）
		if !day.all {
			out = append(out, CalendarMakeAt(TriggerMonthly, tm.time, tm.interval, tm.duration, months, day.values, nil, false))
		}
		if !weekday.all {
			if month.all {
				out = append(out, CalendarMakeAt(TriggerWeekly, tm.time, tm.interval, tm.duration, nil, nil, weekdays, false))
			} else {
				out = append(out, CalendarMakeAt(TriggerMonthlyDayOfWeek, tm.time, tm.interval, tm.duration, months, nil, weekdays, true))
			}
		}
		if day.all && weekday.all {
			if month.all {
				out = append(out, CalendarMakeAt(TriggerDaily, tm.time, tm.interval, tm.duration, nil, nil, nil, false))
			} else {
				all := make([]int, 31)
				for i := range all {
					all[i] = i + 1
				}
				out = append(out, CalendarMakeAt(TriggerMonthly, tm.time, tm.interval, tm.duration, months, all, nil, false))
			}
		}
		if len(out) > CronMaxTriggers {
			return unknown
		}
	}
	return out
}

// cronFieldValue 一個欄位：all＝*；step＝從 values[0] 起每隔幾個（*/n、a-b/n 才有）；values 由小到大
type cronFieldValue struct {
	all    bool
	step   int
	values []int
}

func cronField(text string, min, max int, names []string) *cronFieldValue {
	if text == "*" {
		f := &cronFieldValue{all: true, step: 1}
		for v := min; v <= max; v++ {
			f.values = append(f.values, v)
		}
		return f
	}
	set := map[int]bool{}
	step := 0
	parts := strings.Split(text, ",")
	for _, part := range parts {
		rng := part
		by := 1
		slash := strings.IndexByte(part, '/')
		if slash >= 0 {
			n, err := strconv.Atoi(part[slash+1:])
			if err != nil || n <= 0 || !allDigits(part[slash+1:]) {
				return nil
			}
			by = n
			rng = part[:slash]
		}
		var from, to int
		if rng == "*" {
			from, to = min, max
		} else if dash := strings.IndexByte(rng, '-'); dash > 0 {
			a, ok1 := cronNumber(rng[:dash], names, min)
			b, ok2 := cronNumber(rng[dash+1:], names, min)
			if !ok1 || !ok2 {
				return nil
			}
			from, to = a, b
		} else {
			single, ok := cronNumber(rng, names, min)
			if !ok {
				return nil
			}
			// 「5/15」＝從 5 起每 15 個
			from, to = single, single
			if slash >= 0 {
				to = max
			}
		}
		if from < min || to > max || from > to {
			return nil
		}
		for v := from; v <= to; v += by {
			set[v] = true
		}
		// 整個欄位就是一個帶間隔的寫法才算「每隔幾個」
		if slash >= 0 && len(parts) == 1 {
			step = by
		}
	}
	if len(set) == 0 {
		return nil
	}
	f := &cronFieldValue{step: step}
	for v := range set {
		f.values = append(f.values, v)
	}
	sort.Ints(f.values)
	return f
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func cronNumber(text string, names []string, min int) (int, bool) {
	if allDigits(text) {
		n, err := strconv.Atoi(text)
		return n, err == nil
	}
	if names == nil {
		return 0, false
	}
	lower := strings.ToLower(text)
	for i, n := range names {
		if n == lower {
			// 月份的名稱從 1 起算、星期從 0（週日）起算
			if min == 1 {
				return i + 1, true
			}
			return i, true
		}
	}
	return 0, false
}

type cronTime struct {
	time, interval, duration time.Duration
}

// cronTimes 分與時換成一天裡的時刻：「*/15 *」是整天每 15 分鐘；「0 */2」是整天每 2 小時；
// 「*/10 9-17」是那幾個小時各自每 10 分鐘；其他逐一列出。超過 CronMaxTriggers 個回 nil
func cronTimes(minute, hour *cronFieldValue) []cronTime {
	var out []cronTime
	// 分鐘是「每隔幾分鐘」而且一小時除得盡、從頭開始
	minuteEvery := minute.step > 0 && 60%minute.step == 0 && minute.values[0] < minute.step && len(minute.values) == 60/minute.step
	hourEvery := hour.step > 0 && 24%hour.step == 0 && hour.values[0] < hour.step && len(hour.values) == 24/hour.step
	if minuteEvery && hour.all {
		return []cronTime{{time.Duration(minute.values[0]) * time.Minute, time.Duration(minute.step) * time.Minute, 0}}
	}
	if minuteEvery {
		for _, h := range hour.values {
			out = append(out, cronTime{time.Duration(h)*time.Hour + time.Duration(minute.values[0])*time.Minute, time.Duration(minute.step) * time.Minute, time.Hour})
		}
		if len(out) > CronMaxTriggers {
			return nil
		}
		return out
	}
	if hourEvery {
		for _, m := range minute.values {
			out = append(out, cronTime{time.Duration(hour.values[0])*time.Hour + time.Duration(m)*time.Minute, time.Duration(hour.step) * time.Hour, 0})
		}
		if len(out) > CronMaxTriggers {
			return nil
		}
		return out
	}
	if len(minute.values)*len(hour.values) > CronMaxTriggers {
		return nil
	}
	for _, h := range hour.values {
		for _, m := range minute.values {
			out = append(out, cronTime{time.Duration(h)*time.Hour + time.Duration(m)*time.Minute, 0, 0})
		}
	}
	return out
}
