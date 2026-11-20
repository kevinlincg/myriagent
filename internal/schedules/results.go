package schedules

import (
	"fmt"
	"strings"
	"time"
)

// ResultLevel 上次執行結果的等級：成功、資訊（執行中、還沒執行過…，不算結果）、警告（被停止）、失敗
type ResultLevel int

const (
	LevelOK ResultLevel = iota
	LevelInfo
	LevelWarn
	LevelFail
)

// LevelName ok／info／warn／fail
func (l ResultLevel) Name() string {
	switch l {
	case LevelOK:
		return "ok"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	}
	return "fail"
}

// Result 結果的說明與等級
type Result struct {
	Code  int
	Level ResultLevel
	Text  string
}

// 工作排程器自己的狀態碼（SCHED_S_*）：macOS／Linux 的讀取器拿它們當記號（執行中、還沒執行過）
const (
	ResultRunning    = 0x41301
	ResultHasNotRun  = 0x41303
	ResultTerminated = 0x41306
)

var knownSched = map[int]struct {
	level ResultLevel
	text  string
}{
	0x00041300: {LevelInfo, "準備就緒"},
	0x00041301: {LevelInfo, "執行中"},
	0x00041302: {LevelInfo, "已停用"},
	0x00041303: {LevelInfo, "還沒執行過"},
	0x00041304: {LevelInfo, "不會再執行"},
	0x00041305: {LevelInfo, "沒有排定的時間"},
	0x00041306: {LevelWarn, "被停止"},
	0x00041307: {LevelWarn, "沒有有效的觸發條件"},
	0x0004131C: {LevelWarn, "以批次登入執行時出了問題"},
}

// knownPosix macOS／Linux（launchd、cron、systemd）的結束碼：程式自己的結束碼是 0～255；被訊號結束記成負的訊號編號（launchctl list 也是這樣寫）。
// 被停止、被強制結束是警告（有人或系統叫它停，不是它自己壞了：launchctl kill、登出、重新載入都會這樣）；當掉的才算失敗
var knownPosix = map[int]struct {
	level ResultLevel
	text  string
}{
	78:  {LevelFail, "launchd 啟動不了（設定有問題：找不到程式、沒有權限或工作資料夾不存在）"},
	126: {LevelFail, "找到檔案但不能執行（沒有執行權限）"},
	127: {LevelFail, "找不到要執行的指令"},
	-1:  {LevelWarn, "終端機關閉而結束（SIGHUP）"},
	-2:  {LevelWarn, "被中斷（SIGINT）"},
	-3:  {LevelWarn, "被要求結束（SIGQUIT）"},
	-4:  {LevelFail, "程式當掉（不合法的指令）"},
	-5:  {LevelFail, "程式當掉（SIGTRAP）"},
	-6:  {LevelFail, "程式自己中止（abort）"},
	-8:  {LevelFail, "程式當掉（算術錯誤）"},
	-9:  {LevelWarn, "被強制結束（SIGKILL）"},
	-10: {LevelFail, "程式當掉（記憶體存取錯誤）"},
	-11: {LevelFail, "程式當掉（記憶體存取錯誤）"},
	-13: {LevelWarn, "輸出的對象關閉了（SIGPIPE）"},
	-15: {LevelWarn, "被停止（SIGTERM）"},
}

// Classify 結果的說明（macOS／Linux 的結束碼；工作排程器自己的狀態碼 0x413xx 也認，讀取器拿它們當記號）
func Classify(code int) Result {
	if code == 0 {
		return Result{0, LevelOK, "成功"}
	}
	if k, ok := knownPosix[code]; ok {
		return Result{code, k.level, k.text}
	}
	if k, ok := knownSched[code]; ok {
		return Result{code, k.level, k.text}
	}
	return Result{code, LevelFail, CodeText(code)}
}

// CodeText 負的是訊號（「被訊號 31 結束」），其他寫十進位
func CodeText(code int) string {
	if code < 0 {
		return fmt.Sprintf("被訊號 %d 結束", -int64(code))
	}
	return fmt.Sprintf("結束碼 %d", code)
}

// IsFinal 這個結果是一次執行的最後結果（不是執行中、還沒執行過這類狀態）：才記進執行紀錄
func IsFinal(code int) bool {
	switch code {
	case ResultRunning, ResultHasNotRun, 0x00041300, 0x00041302, 0x00041304, 0x00041305:
		return false
	}
	return true
}

// ---- 執行紀錄與失敗的規則（C# 的 ScheduleRunRules） ----

// RunRecord Myriagent 觀察到的一次執行（runs.jsonl 的一行）。At＝開始時間；Gap＝和上一筆之間還有預定的執行沒有觀察到
type RunRecord struct {
	Path       string    `json:"Path"`
	At         time.Time `json:"At"`
	Code       int       `json:"Code"`
	ObservedAt time.Time `json:"ObservedAt"`
	Gap        bool      `json:"Gap"`
}

// DiagnoseRecord 一次自動診斷（限制次數用）
type DiagnoseRecord struct {
	Path           string    `json:"Path"`
	At             time.Time `json:"At"`
	ConversationID string    `json:"ConversationId,omitempty"`
}

const (
	// KeepRunsPerTask 每個排程留幾筆執行紀錄
	KeepRunsPerTask = 200
	// AlertWindow 多久以前的失敗不再通知（app 剛開、第一次看到時的舊結果）
	AlertWindow = 12 * time.Hour
	// DiagnosePerTask 同一個排程多久內只自動診斷一次
	DiagnosePerTask = 24 * time.Hour
	// DiagnosePerDay 每台電腦每天最多自動診斷幾次
	DiagnosePerDay = 10
	// DiagnoseMinWeeklyRemaining 每週額度剩不到這個百分比就不自動診斷
	DiagnoseMinWeeklyRemaining = 10.0
)

// Observe 這次掃描看到的上次執行要不要記成新的一筆：有執行過、結果是最後結果（不是執行中）、比上一筆新。
// 和上一筆之間還有預定的執行（前後各留一分鐘的誤差）就標 Gap
func Observe(path string, previous *RunRecord, lastRun *time.Time, code int, triggers []Trigger, now time.Time) *RunRecord {
	if lastRun == nil || lastRun.Year() < 2000 {
		return nil
	}
	if !IsFinal(code) {
		return nil
	}
	at := lastRun.Truncate(time.Second)
	if previous != nil && !at.After(previous.At) {
		return nil
	}
	gap := previous != nil && previous.At.Add(time.Minute).Before(at.Add(-time.Minute)) &&
		len(Occurrences(triggers, previous.At.Add(time.Minute), at.Add(-time.Minute), 1)) > 0
	return &RunRecord{Path: path, At: at, Code: code, ObservedAt: now, Gap: gap}
}

// ConsecutiveFailures 從最後一筆往前數連續失敗幾次
func ConsecutiveFailures(runs []RunRecord) int {
	n := 0
	for i := len(runs) - 1; i >= 0; i-- {
		if Classify(runs[i].Code).Level != LevelFail {
			break
		}
		n++
	}
	return n
}

// ShouldAlert 新的一筆要不要通知：是失敗、上一筆不是失敗（新的一串失敗才通知）、開始時間在 12 小時內
func ShouldAlert(run RunRecord, previous *RunRecord, now time.Time) bool {
	if Classify(run.Code).Level != LevelFail {
		return false
	}
	if previous != nil && Classify(previous.Code).Level == LevelFail {
		return false
	}
	return now.Sub(run.At) <= AlertWindow
}

// IsManaged 只有列入管理的（代理建立、使用者納入管理）才通知與自動診斷；疑似的可能根本不是自己的程式
func IsManaged(o Origin) bool { return o == OriginAgent || o == OriginAdopted }

// DiagnoseBlock 能不能自動請代理診斷；不行時回原因（排程頁的明細顯示）。weeklyRemaining＝要用的帳戶每週額度剩下的百分比（不知道是 nil）
func DiagnoseBlock(path string, origin Origin, settingOn, taskOptOut bool, log []DiagnoseRecord, now time.Time, accountAvailable bool, weeklyRemaining *float64) string {
	if !settingOn {
		return "設定頁關掉了自動診斷"
	}
	if taskOptOut {
		return "這個排程關掉了自動診斷"
	}
	if !IsManaged(origin) {
		return "只有代理建立或納入管理的排程才自動診斷"
	}
	today := 0
	ny, nm, nd := now.Date()
	for _, r := range log {
		if strings.EqualFold(r.Path, path) && now.Sub(r.At) < DiagnosePerTask {
			return "24 小時內已經自動診斷過"
		}
		if y, m, d := r.At.In(now.Location()).Date(); y == ny && m == nm && d == nd {
			today++
		}
	}
	if today >= DiagnosePerDay {
		return fmt.Sprintf("今天已經自動診斷 %d 次", DiagnosePerDay)
	}
	if !accountAvailable {
		return "沒有可用的帳戶"
	}
	if weeklyRemaining != nil && *weeklyRemaining < DiagnoseMinWeeklyRemaining {
		return fmt.Sprintf("每週額度剩不到 %.0f%%", DiagnoseMinWeeklyRemaining)
	}
	return ""
}

// PruneDiagnoses 診斷紀錄只留最近 7 天
func PruneDiagnoses(log []DiagnoseRecord, now time.Time) []DiagnoseRecord {
	out := []DiagnoseRecord{}
	for _, r := range log {
		if now.Sub(r.At) < 7*24*time.Hour {
			out = append(out, r)
		}
	}
	sortByAt(out)
	return out
}

func sortByAt(list []DiagnoseRecord) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].At.Before(list[j-1].At); j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}
