package schedules

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// macOS 的排程（launchd 的 .plist）讀成共用的 Definition（C# 的 LaunchdPlistRules、LaunchdStateRules）。純函式：不讀檔、不看現在時間。對應關係：
// StartCalendarInterval → 每天／每週／每月（沒寫的欄位是萬用字元：只寫分鐘＝每小時、只寫小時＝那一小時裡每分鐘）；
// StartInterval → 每隔多久；RunAtLoad、KeepAlive → 登入時（LaunchAgent）或開機時（LaunchDaemon）；
// WatchPaths、QueueDirectories、StartOnMount、Sockets、MachServices → 事件。讀不懂的鍵忽略

// LaunchdDomain launchd 的工作放在哪裡（決定誰執行、什麼時候載入、誰能改）
type LaunchdDomain int

const (
	// DomainUserAgent ~/Library/LaunchAgents：這個使用者登入時載入、以他的身分執行，自己就能改
	DomainUserAgent LaunchdDomain = iota
	// DomainGlobalAgent /Library/LaunchAgents：每個使用者登入時載入，要系統管理員才能改
	DomainGlobalAgent
	// DomainDaemon /Library/LaunchDaemons：開機時載入、預設以 root 執行，要系統管理員才能改
	DomainDaemon
)

const (
	// AgentLogonType Definition.LogonType：登入時才載入的工作
	AgentLogonType = "LaunchAgent"
	// DaemonLogonType Definition.LogonType：開機就載入的工作
	DaemonLogonType = "LaunchDaemon"
)

// LaunchdJob 一個 launchd 工作
type LaunchdJob struct {
	Label             string
	Definition        Definition
	StandardOutPath   string
	StandardErrorPath string
}

// ErrNotPlist 不是 plist
var ErrNotPlist = errors.New("不是 plist")

// ParsePlist 解析 XML 寫法的 plist：dict 是 map[string]any、array 是 []any、string／date／data 是 string、integer 是 int64、real 是 float64、
// true／false 是 bool。不是合法的 XML、最外層不是 plist 回錯誤（二進位的 plist 由呼叫端先以 plutil 轉成 XML）。DOCTYPE 略過，不連出去
func ParsePlist(text string) (any, error) {
	d := xml.NewDecoder(strings.NewReader(text))
	d.Strict = true
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return nil, fmt.Errorf("沒有根元素")
		}
		if err != nil {
			return nil, err
		}
		if se, ok := tok.(xml.StartElement); ok {
			if se.Name.Local != "plist" {
				return nil, ErrNotPlist
			}
			// 第一個子元素
			for {
				tok, err := d.Token()
				if err != nil {
					if err == io.EOF {
						return nil, fmt.Errorf("plist 沒有結束")
					}
					return nil, err
				}
				switch t := tok.(type) {
				case xml.StartElement:
					return readPlistValue(d, t)
				case xml.EndElement:
					return nil, nil
				}
			}
		}
	}
}

func readPlistValue(d *xml.Decoder, se xml.StartElement) (any, error) {
	switch se.Name.Local {
	case "dict":
		dict := map[string]any{}
		key := ""
		haveKey := false
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				if t.Name.Local == "key" {
					s, err := readText(d)
					if err != nil {
						return nil, err
					}
					key, haveKey = s, true
					continue
				}
				v, err := readPlistValue(d, t)
				if err != nil {
					return nil, err
				}
				if haveKey {
					dict[key] = v
				}
				haveKey = false
			case xml.EndElement:
				return dict, nil
			}
		}
	case "array":
		list := []any{}
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				v, err := readPlistValue(d, t)
				if err != nil {
					return nil, err
				}
				list = append(list, v)
			case xml.EndElement:
				return list, nil
			}
		}
	case "string", "date", "data":
		return readText(d)
	case "integer":
		s, err := readText(d)
		if err != nil {
			return nil, err
		}
		n, perr := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if perr != nil {
			n = 0
		}
		return n, nil
	case "real":
		s, err := readText(d)
		if err != nil {
			return nil, err
		}
		f, perr := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if perr != nil {
			f = 0
		}
		return f, nil
	case "true":
		return true, d.Skip()
	case "false":
		return false, d.Skip()
	}
	return nil, d.Skip()
}

// readText 讀到這個元素結束為止的文字
func readText(d *xml.Decoder) (string, error) {
	var sb strings.Builder
	depth := 0
	for {
		tok, err := d.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.CharData:
			sb.Write(t)
		case xml.StartElement:
			depth++
		case xml.EndElement:
			if depth == 0 {
				return sb.String(), nil
			}
			depth--
		}
	}
}

// LaunchdToJob plist（XML）讀成工作。userName＝這台登入的使用者（LaunchAgent 的執行身分）；created＝檔案的建立時間（plist 自己沒有記）；
// intervalAnchor＝「每隔多久」（StartInterval）從什麼時候起算（上次開始執行的時間；不知道就給開機時間）
func LaunchdToJob(text string, domain LaunchdDomain, userName string, created, intervalAnchor *time.Time) (LaunchdJob, error) {
	v, err := ParsePlist(text)
	if err != nil {
		return LaunchdJob{}, err
	}
	plist, ok := v.(map[string]any)
	if !ok {
		return LaunchdJob{}, fmt.Errorf("最外層不是 dict")
	}
	daemon := domain == DomainDaemon
	label := pStr(plist, "Label")
	var notes []string

	triggers := CalendarTriggers(plist["StartCalendarInterval"])
	if seconds, ok := pInt(plist, "StartInterval"); ok && seconds > 0 {
		anchor := BaseDate()
		if intervalAnchor != nil {
			anchor = *intervalAnchor
		}
		triggers = append(triggers, IntervalTrigger(time.Duration(seconds)*time.Second, anchor))
	}
	_, keepDict := plist["KeepAlive"].(map[string]any)
	keepAlive := plist["KeepAlive"] == true || keepDict
	if pBool(plist, "RunAtLoad") || keepAlive {
		kind := TriggerLogon
		if daemon {
			kind = TriggerBoot
		}
		t := NewTrigger(kind)
		if !daemon && domain != DomainGlobalAgent {
			t.UserID = userName
		}
		triggers = append(triggers, t)
	}
	if keepAlive {
		notes = append(notes, "保持執行：結束後 launchd 會重新啟動")
	}
	var events []string
	if watch := pStrings(plist, "WatchPaths"); len(watch) > 0 {
		events = append(events, "這些路徑有變動時執行："+strings.Join(watch, "、"))
	}
	if queue := pStrings(plist, "QueueDirectories"); len(queue) > 0 {
		events = append(events, "這些資料夾裡有檔案時執行："+strings.Join(queue, "、"))
	}
	if pBool(plist, "StartOnMount") {
		events = append(events, "掛上磁碟時執行")
	}
	_, s1 := plist["Sockets"]
	_, s2 := plist["MachServices"]
	_, s3 := plist["LaunchEvents"]
	if s1 || s2 || s3 {
		events = append(events, "有程式連進來或發生系統事件時由 launchd 啟動")
	}
	if len(events) > 0 {
		triggers = append(triggers, NewTrigger(TriggerEvent))
		notes = append(notes, events...)
	}
	stdout := pStr(plist, "StandardOutPath")
	stderr := pStr(plist, "StandardErrorPath")
	if stdout != "" && stdout == stderr {
		notes = append(notes, "輸出與錯誤記到 "+stdout)
	} else {
		if stdout != "" {
			notes = append(notes, "輸出記到 "+stdout)
		}
		if stderr != "" {
			notes = append(notes, "錯誤記到 "+stderr)
		}
	}
	runAs := ""
	switch {
	case daemon:
		runAs = pStr(plist, "UserName")
		if runAs == "" {
			runAs = "root"
		}
	case domain == DomainUserAgent:
		runAs = userName
	}
	logon := AgentLogonType
	if daemon {
		logon = DaemonLogonType
	}
	_, hasCalendar := plist["StartCalendarInterval"]
	def := Definition{
		Description: strings.Join(notes, "；"),
		Created:     created,
		Triggers:    triggers,
		Actions:     launchdActions(plist),
		RunAs:       runAs,
		LogonType:   logon,
		Elevated:    daemon && runAs == "root",
		Enabled:     !pBool(plist, "Disabled"),
		// 時間到了電腦在睡，launchd 醒來會補跑一次（cron 不會）
		StartWhenAvailable: hasCalendar,
	}
	return LaunchdJob{Label: label, Definition: def, StandardOutPath: stdout, StandardErrorPath: stderr}, nil
}

// launchdActions 要執行的指令：Program 是程式、ProgramArguments 是 argv（沒有 Program 時第一個就是程式）；都沒有就沒有動作
func launchdActions(plist map[string]any) []Action {
	program := pStr(plist, "Program")
	args := pStrings(plist, "ProgramArguments")
	command := program
	if command == "" && len(args) > 0 {
		command = args[0]
	}
	if strings.TrimSpace(command) == "" {
		return nil
	}
	var rest []string
	if len(args) > 1 {
		rest = args[1:]
	}
	a := Action{Kind: "Exec", Command: command, WorkingDirectory: pStr(plist, "WorkingDirectory")}
	if len(rest) > 0 {
		a.Arguments = JoinArguments(rest)
	}
	return []Action{a}
}

// JoinArguments argv 寫成一行：有空白、引號或是空字串的以雙引號包起來（引號與反斜線加反斜線）
func JoinArguments(args []string) string {
	var sb strings.Builder
	for _, a := range args {
		if sb.Len() > 0 {
			sb.WriteByte(' ')
		}
		if a != "" && !strings.ContainsAny(a, " \t\n\r\"'") {
			sb.WriteString(a)
			continue
		}
		sb.WriteByte('"')
		sb.WriteString(strings.ReplaceAll(strings.ReplaceAll(a, `\`, `\\`), `"`, `\"`))
		sb.WriteByte('"')
	}
	return sb.String()
}

type calendarSpec struct {
	minute, hour, day, weekday, month *int
}

// CalendarTriggers StartCalendarInterval（一個 dict 或一串 dict）。只差在星期幾、時間相同的幾個 dict 併成一個「每週」（平日 09:00 通常寫成五個 dict）
func CalendarTriggers(value any) []Trigger {
	var entries []map[string]any
	switch v := value.(type) {
	case map[string]any:
		entries = append(entries, v)
	case []any:
		for _, x := range v {
			if m, ok := x.(map[string]any); ok {
				entries = append(entries, m)
			}
		}
	}
	var specs []calendarSpec
	for _, e := range entries {
		s := calendarSpec{minute: pRange(e, "Minute", 0, 59), hour: pRange(e, "Hour", 0, 23), day: pRange(e, "Day", 1, 31), month: pRange(e, "Month", 1, 12)}
		if w, ok := pInt(e, "Weekday"); ok && w >= 0 && w <= 7 {
			x := int(w % 7)
			s.weekday = &x
		}
		specs = append(specs, s)
	}
	var out []Trigger
	// 只指定星期幾的：時間相同的併在一起（照第一次出現的順序）
	type hm struct{ h, m int }
	key := func(p *int) int {
		if p == nil {
			return -1
		}
		return *p
	}
	var order []hm
	groups := map[hm]map[time.Weekday]bool{}
	for _, s := range specs {
		if s.weekday != nil && s.day == nil && s.month == nil {
			k := hm{key(s.hour), key(s.minute)}
			if groups[k] == nil {
				groups[k] = map[time.Weekday]bool{}
				order = append(order, k)
			}
			groups[k][time.Weekday(*s.weekday)] = true
		}
	}
	for _, k := range order {
		var days []time.Weekday
		for d := time.Sunday; d <= time.Saturday; d++ {
			if groups[k][d] {
				days = append(days, d)
			}
		}
		out = append(out, CalendarMake(TriggerWeekly, intPtrOrNil(k.h), intPtrOrNil(k.m), nil, nil, days, false))
	}
	for _, s := range specs {
		if !(s.weekday != nil && s.day == nil && s.month == nil) {
			out = append(out, CalendarFromFields(s.minute, s.hour, s.day, s.month, s.weekday)...)
		}
	}
	return out
}

func intPtrOrNil(v int) *int {
	if v < 0 {
		return nil
	}
	return &v
}

// IntervalTrigger 每隔多久（StartInterval）：一天除得盡的寫成「每天、從起算時間開始每隔多久」（排程頁顯示「每小時（21:13 起）」），
// 除不盡的（例如每 7 小時）寫成從起算時間一直重複
func IntervalTrigger(interval time.Duration, anchor time.Time) Trigger {
	kind := TriggerOnce
	if interval <= 24*time.Hour && (24*time.Hour)%interval == 0 {
		kind = TriggerDaily
	}
	t := NewTrigger(kind)
	a := anchor
	t.Start = &a
	t.RepeatInterval = interval
	return t
}

func pStr(d map[string]any, key string) string {
	s, _ := d[key].(string)
	return s
}

func pBool(d map[string]any, key string) bool { return d[key] == true }

func pInt(d map[string]any, key string) (int64, bool) {
	switch v := d[key].(type) {
	case int64:
		return v, true
	case float64:
		return int64(v), true
	}
	return 0, false
}

func pRange(d map[string]any, key string, min, max int64) *int {
	if v, ok := pInt(d, key); ok && v >= min && v <= max {
		x := int(v)
		return &x
	}
	return nil
}

func pStrings(d map[string]any, key string) []string {
	switch v := d[key].(type) {
	case []any:
		var out []string
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		// WatchPaths 之類偶爾寫成單一字串
		if v != "" {
			return []string{v}
		}
	}
	return nil
}

// ---- 狀態（launchctl 與 launchd 的記錄檔；C# 的 LaunchdStateRules） ----

// LaunchdLogEvent 記錄檔的一筆：某個工作開始執行，或結束（Code＝結束碼；被訊號結束是負的訊號編號）
type LaunchdLogEvent struct {
	At      time.Time
	Service string
	Exited  bool
	Code    int
}

// LaunchdRun 一個工作從記錄檔看到的執行：LastStart＝最近一次開始；FinishedStart／FinishedAt／Code＝最近一次「跑完」的那次
type LaunchdRun struct {
	LastStart     *time.Time
	FinishedStart *time.Time
	FinishedAt    *time.Time
	Code          *int
}

// Running 開始之後還沒看到結束
func (r LaunchdRun) Running() bool {
	return r.LastStart != nil && (r.FinishedAt == nil || r.LastStart.After(*r.FinishedAt))
}

// LaunchdLoaded launchctl list／print system 列出的一個已載入的工作：Pid（沒在跑是 nil）、上次的結束碼（沒有是 nil）
type LaunchdLoaded struct {
	Pid    *int
	Status *int
}

// LaunchdState 排程頁要的狀態
type LaunchdState struct {
	State      TaskState
	Enabled    bool
	LastRun    *time.Time
	LastResult int
}

// 2026-11-19 21:13:12.378848 (gui/501/com.example.job [92888]) <Notice>: 訊息
var launchdLogLine = regexp.MustCompile(`^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d)(?:\.\d+)? \(([^ )]+)(?: \[\d+\])?\) <\w+>: (.*)$`)
var launchdExit = regexp.MustCompile(`^exited due to (?:exit\((-?\d+)\)|(SIG[A-Z0-9]+))`)

var signals = map[string]int{
	"SIGHUP": 1, "SIGINT": 2, "SIGQUIT": 3, "SIGILL": 4, "SIGTRAP": 5, "SIGABRT": 6, "SIGFPE": 8, "SIGKILL": 9,
	"SIGBUS": 10, "SIGSEGV": 11, "SIGSYS": 12, "SIGPIPE": 13, "SIGALRM": 14, "SIGTERM": 15,
}

// UnknownSignal 看不出是哪個訊號時用的編號（結束碼記成它的負數）
const UnknownSignal = 99

// LaunchdLogMayMatter 快速篩選：這一行可能是開始或結束
func LaunchdLogMayMatter(line string) bool {
	return strings.Contains(line, "Successfully spawned") || strings.Contains(line, "exited due to")
}

// ParseLaunchdLogLine 解析記錄檔的一行；不是開始或結束回 nil。時間是本機時間
func ParseLaunchdLogLine(line string) *LaunchdLogEvent {
	if !LaunchdLogMayMatter(line) {
		return nil
	}
	m := launchdLogLine.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	at, err := time.ParseInLocation("2006-01-02 15:04:05", m[1], time.Local)
	if err != nil {
		return nil
	}
	service, msg := m[2], m[3]
	if strings.HasPrefix(msg, "Successfully spawned ") {
		return &LaunchdLogEvent{At: at, Service: service}
	}
	e := launchdExit.FindStringSubmatch(msg)
	if e == nil {
		return nil
	}
	if e[1] != "" {
		code, err := strconv.Atoi(e[1])
		if err != nil {
			return nil
		}
		return &LaunchdLogEvent{At: at, Service: service, Exited: true, Code: code}
	}
	sig, ok := signals[e[2]]
	if !ok {
		sig = UnknownSignal
	}
	return &LaunchdLogEvent{At: at, Service: service, Exited: true, Code: -sig}
}

// ApplyLaunchdEvent 把一筆記到各工作的執行狀況上（依記錄檔的順序呼叫）
func ApplyLaunchdEvent(runs map[string]LaunchdRun, e LaunchdLogEvent) {
	run, has := runs[e.Service]
	at := e.At
	if !e.Exited {
		run.LastStart = &at
		runs[e.Service] = run
		return
	}
	// 這次結束對應的開始：上一次結束之後有開始過就是它；沒看到開始（記錄檔已經輪替、啟動就失敗）就當成結束的那一刻
	start := at
	if has && run.LastStart != nil && !run.LastStart.After(at) && (run.FinishedAt == nil || run.LastStart.After(*run.FinishedAt)) {
		start = *run.LastStart
	}
	code := e.Code
	runs[e.Service] = LaunchdRun{LastStart: run.LastStart, FinishedStart: &start, FinishedAt: &at, Code: &code}
}

// LaunchdServiceKey 記錄檔裡的工作名稱：使用者登入後載入的是 gui/〈uid〉/〈標籤〉，開機載入的是 system/〈標籤〉
func LaunchdServiceKey(domain LaunchdDomain, uid int, label string) string {
	if domain == DomainDaemon {
		return "system/" + label
	}
	return fmt.Sprintf("gui/%d/%s", uid, label)
}

func number(text string) *int {
	n, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		return nil
	}
	return &n
}

// ParseLaunchctlList launchctl list 的輸出（PID、Status、Label 三欄，以 Tab 分隔；沒在跑的 PID 是 -）
func ParseLaunchctlList(text string) map[string]LaunchdLoaded {
	out := map[string]LaunchdLoaded{}
	for _, line := range strings.Split(text, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}
		label := strings.TrimSpace(parts[2])
		if label == "" || label == "Label" {
			continue
		}
		var l LaunchdLoaded
		if pid := number(parts[0]); pid != nil && *pid > 0 {
			l.Pid = pid
		}
		l.Status = number(parts[1])
		out[label] = l
	}
	return out
}

// ParseLaunchctlSystemServices launchctl print system 的 services = { … } 那一段（每行：PID、狀態、標籤；沒在跑的 PID 是 0、沒有結束碼的是 -）
func ParseLaunchctlSystemServices(text string) map[string]LaunchdLoaded {
	out := map[string]LaunchdLoaded{}
	inside := false
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if !inside {
			inside = line == "services = {"
			continue
		}
		if line == "}" {
			break
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		var l LaunchdLoaded
		if pid := number(parts[0]); pid != nil && *pid > 0 {
			l.Pid = pid
		}
		l.Status = number(parts[1])
		out[parts[len(parts)-1]] = l
	}
	return out
}

// ParseLaunchctlDisabled launchctl print-disabled 〈網域〉 的輸出：「"標籤" => disabled」是停用、enabled 是明確啟用；沒列到的照 plist
func ParseLaunchctlDisabled(text string) map[string]bool {
	out := map[string]bool{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		arrow := strings.Index(line, "=>")
		if arrow <= 0 || !strings.HasPrefix(line, `"`) {
			continue
		}
		label := strings.Trim(strings.TrimSpace(line[:arrow]), `"`)
		value := strings.TrimSpace(line[arrow+2:])
		if label != "" && (value == "disabled" || value == "enabled" || value == "true" || value == "false") {
			out[label] = value == "disabled" || value == "true"
		}
	}
	return out
}

// ResolveLaunchdState 合出排程頁要的狀態。上次執行：記錄檔有就用它（最準）；沒有時在跑的用程序的開始時間，
// 沒在跑的用輸出檔最後寫入的時間配 launchctl 記的結束碼；都沒有就是「還沒執行過」（其實是不知道：launchd 不記時間）
func ResolveLaunchdState(enabledInPlist bool, disabledOverride *bool, loaded *LaunchdLoaded, run *LaunchdRun, processStart, logWritten *time.Time) LaunchdState {
	enabled := enabledInPlist
	if disabledOverride != nil {
		enabled = !*disabledOverride
	}
	running := loaded != nil && loaded.Pid != nil
	state := StateUnknown
	switch {
	case running:
		state = StateRunning
	case !enabled:
		state = StateDisabled
	case loaded != nil:
		state = StateReady
	}
	if running {
		var last *time.Time
		switch {
		case run != nil && run.Running():
			last = run.LastStart
		case processStart != nil:
			last = processStart
		case run != nil:
			last = run.LastStart
		}
		return LaunchdState{state, enabled, last, ResultRunning}
	}
	if run != nil && run.FinishedStart != nil && run.Code != nil {
		return LaunchdState{state, enabled, run.FinishedStart, *run.Code}
	}
	if loaded != nil && loaded.Status != nil && logWritten != nil {
		return LaunchdState{state, enabled, logWritten, *loaded.Status}
	}
	return LaunchdState{state, enabled, nil, ResultHasNotRun}
}

// LaunchdStemOf .plist 路徑的檔名去掉副檔名（launchd 的慣例：檔名就是標籤）
func LaunchdStemOf(path string) string {
	name := path[strings.LastIndex(path, "/")+1:]
	if strings.HasSuffix(strings.ToLower(name), ".plist") {
		return name[:len(name)-6]
	}
	return name
}

func sortStrings(s []string) []string { sort.Strings(s); return s }
