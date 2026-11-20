package schedules

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Myriagent 的定時訊息（排程頁的 S4；C# 的 TimedMessageRules）：在指定的時間對某個對話送一則訊息，或每次開新對話。
// 存在 <資料根目錄>/schedules/timed.json（欄位名稱同 C# 版，PascalCase；只加欄位）

// TimedRun 一次定時訊息的送出（最多留 30 筆）
type TimedRun struct {
	// ScheduledAt 原定的時間
	ScheduledAt time.Time `json:"ScheduledAt"`
	// FiredAt 真的送出的時間（錯過補送的會晚）；沒送出是 nil
	FiredAt        *time.Time `json:"FiredAt,omitempty"`
	ConversationID string     `json:"ConversationId,omitempty"`
	// Outcome sent（送出、這一輪還沒結束）、queued、completed、failed、interrupted、missed、error
	Outcome string `json:"Outcome"`
	Error   string `json:"Error,omitempty"`
}

// TimedMessage 一則定時訊息
type TimedMessage struct {
	ID    string `json:"Id"`
	Text  string `json:"Text"`
	Label string `json:"Label,omitempty"`
	// Cron 重複：五欄的 cron（本機時間）；一次性的是空
	Cron string `json:"Cron,omitempty"`
	// At 一次性的時間
	At      *time.Time `json:"At,omitempty"`
	Enabled bool       `json:"Enabled"`
	Done    bool       `json:"Done"`
	// ConversationID 送進哪個對話；NewConversation 時是建立時的對話（只記來源）
	ConversationID  string    `json:"ConversationId,omitempty"`
	NewConversation bool      `json:"NewConversation"`
	AccountID       string    `json:"AccountId,omitempty"`
	Model           string    `json:"Model,omitempty"`
	Effort          string    `json:"Effort,omitempty"`
	PermissionMode  string    `json:"PermissionMode,omitempty"`
	Folder          string    `json:"Folder,omitempty"`
	CreatedAt       time.Time `json:"CreatedAt"`
	// CreatedBy 建立它的對話（代理以 myriagent schedule 建立的）；使用者在排程頁新增的是空
	CreatedBy         string `json:"CreatedBy,omitempty"`
	CreatedByTitle    string `json:"CreatedByTitle,omitempty"`
	CreatedByProvider string `json:"CreatedByProvider,omitempty"`
	// LastScheduledAt 上一次原定的時間（下一次從它之後算）
	LastScheduledAt *time.Time `json:"LastScheduledAt,omitempty"`
	Runs            []TimedRun `json:"Runs"`
}

// Due 到點時要做什麼
type Due int

const (
	DueWait Due = iota
	DueFire
	DueMissed
)

const (
	// OneShotGrace 一次性的錯過超過這麼久就不補
	OneShotGrace = 12 * time.Hour
	// MaxTimedMessages 一個 app 最多幾則定時訊息
	MaxTimedMessages = 200
	// MaxTimedText 文字上限（字）
	MaxTimedText = 20000
	// KeepTimedRuns 每則留幾筆送出紀錄
	KeepTimedRuns = 30
	// MinIntervalMinutes 重複的最短間隔
	MinIntervalMinutes = 5
)

// CronSpec 五欄的 cron：分（0-59）時（0-23）日（1-31）月（1-12）星期（0-7，0 與 7 都是週日；也收 SUN…SAT）；
// 每欄收 *、數字、a-b、a,b、*/n、a-b/n。日與星期都有限制時照一般 cron 是「其中一個符合」
type CronSpec struct {
	Text              string
	minutes           [60]bool
	hours             [24]bool
	days              [32]bool
	months            [13]bool
	weekdays          [7]bool
	dayRestricted     bool
	weekdayRestricted bool
}

var cronSpecDays = []string{"SUN", "MON", "TUE", "WED", "THU", "FRI", "SAT"}

// ParseCronSpec 解析；不合格回原因
func ParseCronSpec(text string) (*CronSpec, string) {
	parts := strings.Fields(text)
	if len(parts) != 5 {
		return nil, "cron 要有五欄：分 時 日 月 星期（例如 0 9 * * 1-5）"
	}
	s := &CronSpec{Text: strings.Join(parts, " ")}
	var ok bool
	if !specField(parts[0], 0, 59, s.minutes[:], nil, nil) || !specField(parts[1], 0, 23, s.hours[:], nil, nil) ||
		!specField(parts[2], 1, 31, s.days[:], nil, &s.dayRestricted) || !specField(parts[3], 1, 12, s.months[:], nil, nil) {
		return nil, fmt.Sprintf("cron「%s」有一欄看不懂", text)
	}
	dow := make([]bool, 8)
	ok = specField(parts[4], 0, 7, dow, cronSpecDays, &s.weekdayRestricted)
	if !ok {
		return nil, fmt.Sprintf("cron「%s」的星期看不懂", text)
	}
	for i := 0; i < 7; i++ {
		s.weekdays[i] = dow[i] || (i == 0 && dow[7])
	}
	return s, ""
}

func specField(text string, min, max int, into []bool, names []string, restricted *bool) bool {
	if restricted != nil {
		*restricted = text != "*"
	}
	for _, raw := range strings.Split(text, ",") {
		item := strings.ToUpper(strings.TrimSpace(raw))
		step := 1
		slash := strings.IndexByte(item, '/')
		if slash >= 0 {
			n, err := strconv.Atoi(item[slash+1:])
			if err != nil || n < 1 || !allDigits(item[slash+1:]) {
				return false
			}
			step = n
			item = item[:slash]
		}
		var lo, hi int
		switch {
		case item == "*":
			lo, hi = min, max
		case strings.Contains(item, "-"):
			ab := strings.Split(item, "-")
			var ok1, ok2 bool
			if len(ab) != 2 {
				return false
			}
			lo, ok1 = specValue(ab[0], names)
			hi, ok2 = specValue(ab[1], names)
			if !ok1 || !ok2 {
				return false
			}
		default:
			v, ok := specValue(item, names)
			if !ok {
				return false
			}
			lo, hi = v, v
			if slash >= 0 {
				hi = max
			}
		}
		if lo < min || hi > max || lo > hi {
			return false
		}
		for v := lo; v <= hi; v += step {
			into[v] = true
		}
	}
	return true
}

func specValue(s string, names []string) (int, bool) {
	for i, n := range names {
		if n == s {
			return i, true
		}
	}
	if !allDigits(s) {
		return 0, false
	}
	v, err := strconv.Atoi(s)
	return v, err == nil
}

func (s *CronSpec) dayMatches(d time.Time) bool {
	dom, dow := s.days[d.Day()], s.weekdays[d.Weekday()]
	if s.dayRestricted && s.weekdayRestricted {
		return dom || dow
	}
	return dom && dow
}

// Next 在 after 之後（不含）的下一次；兩年內沒有就是 nil（例如 2 月 30 日）
func (s *CronSpec) Next(after time.Time) *time.Time {
	loc := after.Location()
	t := time.Date(after.Year(), after.Month(), after.Day(), after.Hour(), after.Minute(), 0, 0, loc).Add(time.Minute)
	limit := after.AddDate(2, 0, 0)
	for t.Before(limit) {
		if !s.months[t.Month()] {
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
			continue
		}
		if !s.dayMatches(t) {
			t = dateOf(t).AddDate(0, 0, 1)
			continue
		}
		if !s.hours[t.Hour()] {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc).Add(time.Hour)
			continue
		}
		if !s.minutes[t.Minute()] {
			t = t.Add(time.Minute)
			continue
		}
		return &t
	}
	return nil
}

// Describe 中文摘要：每 15 分鐘、每小時的 :05、每天 09:00、平日 09:00、每週一、三 08:00、每月 1 日 03:00；說不清楚的寫原本的 cron
func (s *CronSpec) Describe() string {
	var mins, hrs []int
	for i := 0; i < 60; i++ {
		if s.minutes[i] {
			mins = append(mins, i)
		}
	}
	for i := 0; i < 24; i++ {
		if s.hours[i] {
			hrs = append(hrs, i)
		}
	}
	allMonths := true
	for i := 1; i <= 12; i++ {
		allMonths = allMonths && s.months[i]
	}
	everyDay := !s.dayRestricted && allMonths
	allWeek := true
	for _, w := range s.weekdays {
		allWeek = allWeek && w
	}
	if everyDay && allWeek && len(hrs) == 24 && len(mins) > 1 {
		step := mins[1] - mins[0]
		even := mins[0] < step && 60%step == 0
		for i := 1; i < len(mins) && even; i++ {
			even = mins[i]-mins[i-1] == step
		}
		if even {
			if step == 1 {
				return "每分鐘"
			}
			return fmt.Sprintf("每 %d 分鐘", step)
		}
	}
	if everyDay && allWeek && len(hrs) == 24 && len(mins) == 1 {
		if mins[0] == 0 {
			return "每小時"
		}
		return fmt.Sprintf("每小時的 :%02d", mins[0])
	}
	if len(mins) != 1 || len(hrs) != 1 || !allMonths {
		return "cron：" + s.Text
	}
	tm := fmt.Sprintf("%02d:%02d", hrs[0], mins[0])
	if !s.dayRestricted {
		if allWeek {
			return "每天 " + tm
		}
		var set []int
		for i := 0; i < 7; i++ {
			if s.weekdays[i] {
				set = append(set, i)
			}
		}
		if fmt.Sprint(set) == "[1 2 3 4 5]" {
			return "平日 " + tm
		}
		if fmt.Sprint(set) == "[0 6]" {
			return "週末 " + tm
		}
		var names []string
		for _, d := range mondayFirst(toWeekdays(set)) {
			names = append(names, dayText[d])
		}
		return "每週" + strings.Join(names, "、") + " " + tm
	}
	if allWeek {
		var days []string
		for i := 1; i <= 31; i++ {
			if s.days[i] {
				days = append(days, strconv.Itoa(i))
			}
		}
		return "每月 " + strings.Join(days, "、") + " 日 " + tm
	}
	return "cron：" + s.Text
}

func toWeekdays(list []int) []time.Weekday {
	out := make([]time.Weekday, len(list))
	for i, v := range list {
		out[i] = time.Weekday(v)
	}
	return out
}

// ParseWhen 把使用者或代理給的時間寫法換成 cron 或一次性的時間：at「2026-10-27 14:00」「14:00」（今天或明天）、daily HH:mm、weekdays HH:mm、
// every 15m／2h、cron 五欄。失敗回原因
func ParseWhen(kind, value string, now time.Time) (cron string, at *time.Time, problem string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil, "少了時間"
	}
	switch kind {
	case "at":
		if h, m, ok := parseHHMM(value); ok {
			today := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
			if !today.After(now) {
				today = today.AddDate(0, 0, 1)
			}
			return "", &today, ""
		}
		t, hasYear, ok := parseAtValue(value, now.Location())
		if !ok {
			return "", nil, "時間看不懂：用「2026-10-27 14:00」或「14:00」"
		}
		// 沒寫年份（10/27 14:00）：今年的已經過了就是明年
		if !hasYear {
			t = time.Date(now.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
			if !t.After(now) {
				t = t.AddDate(1, 0, 0)
			}
		}
		if !t.After(now) {
			return "", nil, "時間已經過了"
		}
		return "", &t, ""
	case "daily", "weekdays":
		h, m, ok := parseHHMM(value)
		if !ok {
			return "", nil, "時間看不懂：用 HH:mm（例如 09:00）"
		}
		dow := "*"
		if kind == "weekdays" {
			dow = "1-5"
		}
		return fmt.Sprintf("%d %d * * %s", m, h, dow), nil, ""
	case "every":
		unit := value[len(value)-1]
		n, err := strconv.Atoi(value[:len(value)-1])
		if err != nil || n < 1 || !allDigits(value[:len(value)-1]) {
			return "", nil, "間隔看不懂：用 15m、2h 這樣的寫法"
		}
		switch unit {
		case 'm', 'M':
			if n < MinIntervalMinutes || 60%n != 0 {
				return "", nil, fmt.Sprintf("每幾分鐘要能整除 60，而且至少 %d 分鐘（例如 5、10、15、30）", MinIntervalMinutes)
			}
			return fmt.Sprintf("*/%d * * * *", n), nil, ""
		case 'h', 'H':
			if n > 23 || 24%n != 0 {
				return "", nil, "每幾小時要能整除 24（例如 1、2、3、4、6、8、12）"
			}
			return fmt.Sprintf("0 */%d * * *", n), nil, ""
		}
		return "", nil, "間隔看不懂：用 15m、2h 這樣的寫法"
	case "cron":
		spec, problem := ParseCronSpec(value)
		if spec == nil {
			return "", nil, problem
		}
		// 太密的拒絕：兩次之間少於 5 分鐘
		if a := spec.Next(now); a != nil {
			if b := spec.Next(*a); b != nil && b.Sub(*a) < MinIntervalMinutes*time.Minute {
				return "", nil, fmt.Sprintf("兩次之間至少要 %d 分鐘", MinIntervalMinutes)
			}
		}
		return spec.Text, nil, ""
	}
	return "", nil, "不認得的時間寫法"
}

// parseHHMM「09:00」（分鐘要兩位數）
func parseHHMM(s string) (int, int, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 || !allDigits(parts[0]) || !allDigits(parts[1]) || len(parts[1]) != 2 {
		return 0, 0, false
	}
	h, _ := strconv.Atoi(parts[0])
	m, _ := strconv.Atoi(parts[1])
	if h > 23 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// parseAtValue 日期＋時間的寫法：yyyy-MM-dd HH:mm、yyyy/M/d HH:mm、yyyy-MM-ddTHH:mm、M/d HH:mm、M-d HH:mm（容許多餘的空白）
func parseAtValue(value string, loc *time.Location) (time.Time, bool, bool) {
	v := strings.Join(strings.Fields(value), " ")
	v = strings.Replace(v, "T", " ", 1)
	sp := strings.Split(v, " ")
	if len(sp) != 2 {
		return time.Time{}, false, false
	}
	h, m, ok := parseHHMM(sp[1])
	if !ok {
		// 時也可以一位數（yyyy-M-d H:mm）
		return time.Time{}, false, false
	}
	date := strings.FieldsFunc(sp[0], func(r rune) bool { return r == '-' || r == '/' })
	for _, p := range date {
		if !allDigits(p) {
			return time.Time{}, false, false
		}
	}
	nums := make([]int, len(date))
	for i, p := range date {
		nums[i], _ = strconv.Atoi(p)
	}
	var y, mo, d int
	hasYear := false
	switch len(nums) {
	case 3:
		if len(date[0]) != 4 {
			return time.Time{}, false, false
		}
		y, mo, d, hasYear = nums[0], nums[1], nums[2], true
	case 2:
		y, mo, d = 2000, nums[0], nums[1]
	default:
		return time.Time{}, false, false
	}
	if mo < 1 || mo > 12 || d < 1 || d > 31 {
		return time.Time{}, false, false
	}
	t := time.Date(y, time.Month(mo), d, h, m, 0, 0, loc)
	if t.Day() != d {
		return time.Time{}, false, false
	}
	return t, hasYear, true
}

// DescribeTimed 時間的中文摘要：一次性的「一次：2026/10/27 14:00」，重複的照 cron
func DescribeTimed(m *TimedMessage) string {
	if m.Cron != "" {
		if s, _ := ParseCronSpec(m.Cron); s != nil {
			return s.Describe()
		}
		return "cron：" + m.Cron
	}
	if m.At != nil {
		return "一次：" + m.At.Format("2006/1/2 15:04")
	}
	return "沒有時間"
}

// LabelOf 名稱：給了就用，沒給取文字的第一行（最多 40 字）
func LabelOf(m *TimedMessage) string {
	if strings.TrimSpace(m.Label) != "" {
		return strings.TrimSpace(m.Label)
	}
	first := "定時訊息"
	for _, l := range strings.Split(m.Text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			first = l
			break
		}
	}
	if r := []rune(first); len(r) > 40 {
		return string(r[:40]) + "…"
	}
	return first
}

// NextDue 下一次原定的時間（停用、做完、沒有時間的是 nil）：從上一次原定的時間（沒有就是建立的時間）之後算
func NextDue(m *TimedMessage) *time.Time {
	if !m.Enabled || m.Done {
		return nil
	}
	if m.Cron == "" {
		return m.At
	}
	s, _ := ParseCronSpec(m.Cron)
	if s == nil {
		return nil
	}
	from := m.CreatedAt
	if m.LastScheduledAt != nil {
		from = *m.LastScheduledAt
	}
	return s.Next(from)
}

// Decide 現在要不要送：還沒到 Wait；到了 Fire（錯過好幾次的重複只補一次，scheduled 是錯過的最後一次）；一次性的錯過超過 12 小時 Missed
func Decide(m *TimedMessage, now time.Time) (Due, time.Time, bool) {
	due := NextDue(m)
	if due == nil || due.After(now) {
		return DueWait, time.Time{}, false
	}
	late := now.Sub(*due) > 2*time.Minute
	if m.Cron == "" {
		if now.Sub(*due) > OneShotGrace {
			return DueMissed, *due, true
		}
		return DueFire, *due, late
	}
	// 重複的：錯過的只補一次，記成最後一次錯過的原定時間（下一次從它之後算）
	s, _ := ParseCronSpec(m.Cron)
	latest := *due
	for i := 0; i < 100000; i++ {
		n := s.Next(latest)
		if n == nil || n.After(now) {
			break
		}
		latest = *n
	}
	return DueFire, latest, late || !latest.Equal(*due)
}

// AgentText 給代理的文字：前面一行說明這是定時訊息（名稱、原定時間、補送的另外說）
func AgentText(m *TimedMessage, scheduled time.Time, late bool) string {
	extra := ""
	if late {
		extra = "，電腦或 app 當時沒開，現在補送"
	}
	return fmt.Sprintf("（這是 Myriagent 的定時訊息「%s」，%s，原定 %s%s）\n\n%s", LabelOf(m), DescribeTimed(m), scheduled.Format("2006/1/2 15:04"), extra, m.Text)
}

// DisplayText 對話裡顯示的文字
func DisplayText(m *TimedMessage, late bool) string {
	tag := ""
	if late {
		tag = "・補送"
	}
	return fmt.Sprintf("（定時訊息%s：%s）\n%s", tag, LabelOf(m), m.Text)
}

// Upcoming [from, to) 之間預定送出的時間（排程頁的時間軸；最多 200 次）
func Upcoming(m *TimedMessage, from, to time.Time) []time.Time {
	var list []time.Time
	if !m.Enabled || m.Done {
		return list
	}
	if m.Cron == "" {
		if m.At != nil && !m.At.Before(from) && m.At.Before(to) {
			list = append(list, *m.At)
		}
		return list
	}
	s, _ := ParseCronSpec(m.Cron)
	if s == nil {
		return list
	}
	t := from.Add(-time.Minute)
	for len(list) < MaxOccurrences {
		n := s.Next(t)
		if n == nil || !n.Before(to) {
			break
		}
		list = append(list, *n)
		t = *n
	}
	return list
}

// AddRun 加一筆送出紀錄（留最近 30 筆）
func AddRun(m *TimedMessage, run TimedRun) *TimedRun {
	m.Runs = append(m.Runs, run)
	if len(m.Runs) > KeepTimedRuns {
		m.Runs = append([]TimedRun{}, m.Runs[len(m.Runs)-KeepTimedRuns:]...)
	}
	return &m.Runs[len(m.Runs)-1]
}

// OutcomeName 一輪的結果寫回送出紀錄的 outcome
func OutcomeName(failed, interrupted bool) string {
	switch {
	case failed:
		return "failed"
	case interrupted:
		return "interrupted"
	}
	return "completed"
}

// WaitsForContinuation 送出的那一輪沒有正常結束，但中樞會自動接著做（額度用完換帳戶、API 過載重試、等額度重置）：先不記結果，
// 等接著做的那一輪結束再記。2026-10-31 實例：帳戶每週額度用完，每次到點第一輪幾秒就失敗、換帳戶後發文都成功，排程頁卻一律記成失敗
func WaitsForContinuation(completed, continues, waitingForQuotaReset bool) bool {
	return !completed && (continues || waitingForQuotaReset)
}

// LevelOfOutcome 結果的等級（排程頁的燈號與長條）
func LevelOfOutcome(outcome string) string {
	switch outcome {
	case "completed":
		return "ok"
	case "sent", "queued":
		return "info"
	case "interrupted", "missed":
		return "warn"
	}
	return "fail"
}

// OutcomeText 結果的說明
func OutcomeText(r TimedRun) string {
	switch r.Outcome {
	case "sent":
		return "已送出"
	case "queued":
		return "已排進佇列"
	case "completed":
		return "完成"
	case "failed":
		return "失敗"
	case "interrupted":
		return "被停止"
	case "missed":
		return "錯過太久沒有補送"
	}
	if r.Error != "" {
		return "沒送出：" + r.Error
	}
	return "沒送出"
}
