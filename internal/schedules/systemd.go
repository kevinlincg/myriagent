package schedules

import (
	"strconv"
	"strings"
	"time"
)

// Linux 的排程：systemd 的使用者計時器（~/.config/systemd/user/*.timer＋對應的 .service）。C# 版沒有（只有 Windows 與 macOS），
// 這裡照 launchd 的同一套作法：單元檔讀成共用的 Definition，狀態從 systemctl --user show 取得（systemd 自己記上次執行的時間與結束碼）。
// 純函式（不讀檔、不執行指令）

// SystemdLogonType Definition.LogonType 的值：使用者的 systemd（登入後由使用者的 systemd 執行；開了 linger 才不必登入）
const SystemdLogonType = "SystemdUser"

// UnitFile 單元檔：段落 → 鍵 → 值（同一個鍵可以出現好幾次，照順序）
type UnitFile map[string]map[string][]string

// ParseUnitFile 解析 systemd 的單元檔（[段落]、鍵=值、# 與 ; 開頭的註解、行尾反斜線接下一行）
func ParseUnitFile(text string) UnitFile {
	u := UnitFile{}
	section := ""
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		for strings.HasSuffix(line, `\`) && i+1 < len(lines) {
			i++
			line = strings.TrimSuffix(line, `\`) + " " + strings.TrimSpace(lines[i])
		}
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if line[0] == '[' && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key, value := strings.TrimSpace(line[:eq]), strings.TrimSpace(line[eq+1:])
		if u[section] == nil {
			u[section] = map[string][]string{}
		}
		// 空值＝把之前的清掉（systemd 的規則）
		if value == "" {
			delete(u[section], key)
			continue
		}
		u[section][key] = append(u[section][key], value)
	}
	return u
}

// Get 最後一個值
func (u UnitFile) Get(section, key string) string {
	v := u[section][key]
	if len(v) == 0 {
		return ""
	}
	return v[len(v)-1]
}

// All 全部的值
func (u UnitFile) All(section, key string) []string { return u[section][key] }

var systemdShorthands = map[string]string{
	"minutely":     "*-*-* *:*:00",
	"hourly":       "*-*-* *:00:00",
	"daily":        "*-*-* 00:00:00",
	"monthly":      "*-*-01 00:00:00",
	"weekly":       "Mon *-*-* 00:00:00",
	"yearly":       "*-01-01 00:00:00",
	"annually":     "*-01-01 00:00:00",
	"quarterly":    "*-01,04,07,10-01 00:00:00",
	"semiannually": "*-01,07-01 00:00:00",
}

var systemdDays = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// SystemdCalendarToCron OnCalendar 的寫法換成五欄的 cron（分 時 日 月 星期）：常見的「daily」「Mon..Fri 09:00」「*-*-* 03:00:00」
// 「*:0/15」「Mon,Wed *-*-01 08:30」都換得過來；年份不是 *、秒不是 0、時區、「~」（月底倒數）這些換不了的回 false（排程頁顯示「其他條件」）
func SystemdCalendarToCron(spec string) (string, bool) {
	s := strings.TrimSpace(spec)
	if short, ok := systemdShorthands[strings.ToLower(s)]; ok {
		s = short
	}
	tokens := strings.Fields(s)
	if len(tokens) == 0 {
		return "", false
	}
	dow := "*"
	if c := tokens[0][0]; (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
		d, ok := systemdWeekdays(tokens[0])
		if !ok {
			return "", false
		}
		dow = d
		tokens = tokens[1:]
	}
	date, clock := "*-*-*", "00:00:00"
	for _, t := range tokens {
		switch {
		case strings.Contains(t, ":"):
			clock = t
		case strings.Contains(t, "-"):
			date = t
		default:
			// 時區之類
			return "", false
		}
	}
	if strings.Contains(date, "~") {
		return "", false
	}
	dp := strings.Split(date, "-")
	if len(dp) == 2 {
		dp = append([]string{"*"}, dp...)
	}
	if len(dp) != 3 || dp[0] != "*" {
		return "", false
	}
	cp := strings.Split(clock, ":")
	if len(cp) == 2 {
		cp = append(cp, "00")
	}
	if len(cp) != 3 {
		return "", false
	}
	if sec := strings.TrimLeft(cp[2], "0"); sec != "" {
		return "", false
	}
	fields := []string{cp[1], cp[0], dp[2], dp[1]}
	for i, f := range fields {
		f = systemdComponent(f)
		if f == "" {
			return "", false
		}
		fields[i] = f
	}
	return strings.Join(append(fields, dow), " "), true
}

// systemdComponent 一個欄位：「..」換成 cron 的「-」，數字去掉前面的 0
func systemdComponent(f string) string {
	var parts []string
	for _, p := range strings.Split(f, ",") {
		step := ""
		if i := strings.IndexByte(p, '/'); i >= 0 {
			step, p = p[i:], p[:i]
		}
		p = strings.ReplaceAll(p, "..", "-")
		var nums []string
		for _, n := range strings.Split(p, "-") {
			if n == "*" {
				nums = append(nums, n)
				continue
			}
			v, err := strconv.Atoi(n)
			if err != nil {
				return ""
			}
			nums = append(nums, strconv.Itoa(v))
		}
		parts = append(parts, strings.Join(nums, "-")+step)
	}
	return strings.Join(parts, ",")
}

// systemdWeekdays 「Mon..Fri」「Sat,Sun」換成 cron 的星期欄
func systemdWeekdays(t string) (string, bool) {
	var out []string
	for _, part := range strings.Split(t, ",") {
		ends := strings.Split(part, "..")
		var nums []int
		for _, e := range ends {
			name := strings.ToLower(e)
			if len(name) > 3 {
				name = name[:3]
			}
			d, ok := systemdDays[name]
			if !ok {
				return "", false
			}
			nums = append(nums, d)
		}
		switch len(nums) {
		case 1:
			out = append(out, strconv.Itoa(nums[0]))
		case 2:
			a, b := nums[0], nums[1]
			if b == 0 {
				b = 7 // Mon..Sun
			}
			if a > b {
				return "", false
			}
			out = append(out, strconv.Itoa(a)+"-"+strconv.Itoa(b))
		default:
			return "", false
		}
	}
	return strings.Join(out, ","), true
}

// ParseSystemdSpan systemd 的時間長度（「15min」「1h 30m」「90」＝秒、「2d」）
func ParseSystemdSpan(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	units := []struct {
		names []string
		d     time.Duration
	}{
		{[]string{"usec", "us", "µs"}, time.Microsecond},
		{[]string{"msec", "ms"}, time.Millisecond},
		{[]string{"seconds", "second", "sec", "s"}, time.Second},
		{[]string{"minutes", "minute", "min", "m"}, time.Minute},
		{[]string{"hours", "hour", "hr", "h"}, time.Hour},
		{[]string{"days", "day", "d"}, 24 * time.Hour},
		{[]string{"weeks", "week", "w"}, 7 * 24 * time.Hour},
		{[]string{"months", "month", "M"}, 2629800 * time.Second},
		{[]string{"years", "year", "y"}, 31557600 * time.Second},
	}
	var total time.Duration
	rest := strings.ReplaceAll(s, " ", "")
	for rest != "" {
		i := 0
		for i < len(rest) && (rest[i] >= '0' && rest[i] <= '9' || rest[i] == '.') {
			i++
		}
		if i == 0 {
			return 0, false
		}
		n, err := strconv.ParseFloat(rest[:i], 64)
		if err != nil {
			return 0, false
		}
		rest = rest[i:]
		j := 0
		for j < len(rest) && !(rest[j] >= '0' && rest[j] <= '9') {
			j++
		}
		unit := rest[:j]
		rest = rest[j:]
		if unit == "" {
			total += time.Duration(n * float64(time.Second))
			continue
		}
		found := false
		for _, u := range units {
			for _, name := range u.names {
				if unit == name {
					total += time.Duration(n * float64(u.d))
					found = true
				}
			}
		}
		if !found {
			return 0, false
		}
	}
	return total, true
}

// SystemdTimerDefinition .timer 與 .service 讀成定義。lastTrigger＝上次觸發的時間（「每隔多久」的起算；不知道給開機時間）
func SystemdTimerDefinition(timer, service UnitFile, userName, home string, lastTrigger time.Time) Definition {
	var triggers []Trigger
	for _, cal := range timer.All("Timer", "OnCalendar") {
		if cron, ok := SystemdCalendarToCron(cal); ok {
			triggers = append(triggers, CronTriggers(cron)...)
		} else {
			triggers = append(triggers, NewTrigger(TriggerUnknown))
		}
	}
	for _, key := range []string{"OnBootSec", "OnStartupSec", "OnActiveSec"} {
		for _, v := range timer.All("Timer", key) {
			t := NewTrigger(TriggerLogon)
			if key == "OnBootSec" {
				t.Kind = TriggerBoot
			} else {
				t.UserID = userName
			}
			if d, ok := ParseSystemdSpan(v); ok {
				t.Delay = d
			}
			triggers = append(triggers, t)
		}
	}
	for _, key := range []string{"OnUnitActiveSec", "OnUnitInactiveSec"} {
		for _, v := range timer.All("Timer", key) {
			if d, ok := ParseSystemdSpan(v); ok && d > 0 {
				triggers = append(triggers, IntervalTrigger(d, lastTrigger))
			}
		}
	}
	var notes []string
	if desc := timer.Get("Unit", "Description"); desc != "" {
		notes = append(notes, desc)
	} else if desc := service.Get("Unit", "Description"); desc != "" {
		notes = append(notes, desc)
	}
	if out := service.Get("Service", "StandardOutput"); out != "" {
		notes = append(notes, "輸出："+out)
	}
	wd := service.Get("Service", "WorkingDirectory")
	wd = strings.TrimPrefix(wd, "-")
	if wd == "~" {
		wd = home
	}
	var actions []Action
	for _, exec := range service.All("Service", "ExecStart") {
		exec = strings.TrimLeft(exec, "-@:+!")
		program, args := SplitCommand(exec)
		if program == "" {
			continue
		}
		actions = append(actions, Action{Kind: "Exec", Command: program, Arguments: args, WorkingDirectory: wd})
	}
	persistent := strings.EqualFold(timer.Get("Timer", "Persistent"), "true") || timer.Get("Timer", "Persistent") == "yes"
	return Definition{
		Description:        strings.Join(notes, "；"),
		Triggers:           triggers,
		Actions:            actions,
		RunAs:              userName,
		LogonType:          SystemdLogonType,
		Enabled:            true,
		StartWhenAvailable: persistent,
		WakeToRun:          strings.EqualFold(timer.Get("Timer", "WakeSystem"), "true") || timer.Get("Timer", "WakeSystem") == "yes",
	}
}

// ParseSystemctlShow systemctl show 的輸出（KEY=值，一個單元一段、空行分隔）：以 Id 為鍵
func ParseSystemctlShow(text string) map[string]map[string]string {
	out := map[string]map[string]string{}
	cur := map[string]string{}
	flush := func() {
		if id := cur["Id"]; id != "" {
			out[id] = cur
		}
		cur = map[string]string{}
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if eq := strings.IndexByte(line, '='); eq > 0 {
			cur[line[:eq]] = line[eq+1:]
		}
	}
	flush()
	return out
}

// ParseSystemdTimestamp「Thu 2026-11-19 21:13:12 CST」或「@1763558000」；n/a、空的、0 回 nil。時區照本機
func ParseSystemdTimestamp(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" || s == "n/a" || s == "0" {
		return nil
	}
	if strings.HasPrefix(s, "@") {
		n, err := strconv.ParseInt(s[1:], 10, 64)
		if err != nil || n <= 0 {
			return nil
		}
		t := time.Unix(n, 0)
		return &t
	}
	f := strings.Fields(s)
	if len(f) >= 3 {
		f = f[1:3]
	}
	if len(f) < 2 {
		return nil
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", f[0]+" "+f[1], time.Local)
	if err != nil {
		return nil
	}
	return &t
}

// SystemdState 排程頁要的狀態
type SystemdState struct {
	State      TaskState
	Enabled    bool
	Loaded     bool // 計時器在排程中（active）
	LastRun    *time.Time
	LastResult int
	NextRun    *time.Time
}

// ResolveSystemdState 合出狀態：停用＝計時器的 UnitFileState 是 disabled 或 masked；在跑＝服務 active／activating；
// 上次執行＝服務的 ExecMainStartTimestamp 配結束碼（被訊號結束記成負的訊號編號，同 launchd）；下次＝計時器的 NextElapseUSecRealtime
func ResolveSystemdState(timer, service map[string]string) SystemdState {
	fileState := timer["UnitFileState"]
	enabled := fileState != "disabled" && fileState != "masked" && fileState != "masked-runtime"
	loaded := timer["ActiveState"] == "active"
	active := service["ActiveState"]
	running := active == "active" || active == "activating" || active == "deactivating"
	st := SystemdState{Enabled: enabled, Loaded: loaded, LastResult: ResultHasNotRun}
	switch {
	case running:
		st.State = StateRunning
	case !enabled:
		st.State = StateDisabled
	case loaded:
		st.State = StateReady
	default:
		st.State = StateUnknown
	}
	start := ParseSystemdTimestamp(service["ExecMainStartTimestamp"])
	if start == nil {
		start = ParseSystemdTimestamp(timer["LastTriggerUSec"])
	}
	if running {
		st.LastRun, st.LastResult = start, ResultRunning
	} else if start != nil && ParseSystemdTimestamp(service["ExecMainExitTimestamp"]) != nil {
		status, _ := strconv.Atoi(service["ExecMainStatus"])
		code := status
		// ExecMainCode：1＝正常結束（status 是結束碼）、2＝被訊號結束、3＝當掉（status 是訊號）
		if c := service["ExecMainCode"]; c == "2" || c == "3" {
			code = -status
		}
		st.LastRun, st.LastResult = start, code
	}
	if loaded && enabled {
		st.NextRun = ParseSystemdTimestamp(timer["NextElapseUSecRealtime"])
	}
	return st
}

// SystemdServiceOf 計時器觸發哪個服務（Unit=，沒寫就是同名的 .service）
func SystemdServiceOf(timerName string, timer UnitFile) string {
	if u := timer.Get("Timer", "Unit"); u != "" {
		return u
	}
	return strings.TrimSuffix(timerName, ".timer") + ".service"
}
