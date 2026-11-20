// Package schedules 是排程頁的規則與讀取器（對應 C# 的 Myriagent.Protocol/Schedules、Myriagent/Schedules、Myriagent.Mac/Schedules）：
// 這台電腦的排程（macOS：launchd 的工作與 crontab；Linux：systemd 的使用者計時器與 crontab）讀成共用的定義，
// 加上 Myriagent 自己的定時訊息（TimedMessage）。時間一律是本機時間（time.Local，測試可以換）。
package schedules

import (
	"context"
	"strings"
	"time"
)

// TriggerKind 觸發條件的種類（沿用 Windows 工作排程器的分類，macOS／Linux 的讀取器換成同樣的種類）
type TriggerKind int

const (
	TriggerOnce TriggerKind = iota
	TriggerDaily
	TriggerWeekly
	TriggerMonthly
	TriggerMonthlyDayOfWeek
	TriggerLogon
	TriggerBoot
	TriggerIdle
	TriggerEvent
	TriggerRegistration
	TriggerSessionStateChange
	TriggerUnknown
)

// Trigger 一個觸發條件
type Trigger struct {
	Kind    TriggerKind
	Enabled bool
	// Start 開始時間（本機）；時刻取它的時分秒
	Start *time.Time
	End   *time.Time
	// RepeatInterval 重複間隔；不重複是 0
	RepeatInterval time.Duration
	// RepeatDuration 重複持續多久；0＋有間隔＝無限期
	RepeatDuration  time.Duration
	DaysInterval    int
	WeeksInterval   int
	DaysOfWeek      []time.Weekday
	Months          []int
	DaysOfMonth     []int
	LastDayOfMonth  bool
	WeeksOfMonth    []int
	LastWeekOfMonth bool
	Delay           time.Duration
	UserID          string
	StateChange     string
}

// NewTrigger 預設值同 C#（啟用、每 1 天、每 1 週）
func NewTrigger(kind TriggerKind) Trigger {
	return Trigger{Kind: kind, Enabled: true, DaysInterval: 1, WeeksInterval: 1}
}

// Action 一個動作。Exec 才有指令
type Action struct {
	Kind             string
	Command          string
	Arguments        string
	WorkingDirectory string
}

// Definition 工作的定義
type Definition struct {
	Author      string
	Description string
	Created     *time.Time
	Triggers    []Trigger
	Actions     []Action
	RunAs       string
	LogonType   string
	Elevated    bool
	Enabled     bool

	Hidden                     bool
	DisallowStartIfOnBatteries bool
	StopIfGoingOnBatteries     bool
	StartWhenAvailable         bool
	RunOnlyIfNetworkAvailable  bool
	RunOnlyIfIdle              bool
	WakeToRun                  bool
	TimeLimit                  time.Duration
	MultipleInstances          string
}

// TaskState 狀態（同工作排程器的 TASK_STATE）
type TaskState int

const (
	StateUnknown TaskState = iota
	StateDisabled
	StateQueued
	StateReady
	StateRunning
)

// TaskInfo 讀到的一個排程（讀取器在背景讀；Definition 讀不到時 Error 寫原因）
type TaskInfo struct {
	Path       string
	XML        string // 完整定義（plist、crontab 的一行、systemd 的單元檔）；讀不到是空
	Definition *Definition
	Error      string
	State      TaskState
	Enabled    bool
	LastRun    *time.Time
	LastResult int
	NextRun    *time.Time
	MissedRuns int
	RepoRoot   string
	// DisplayName 給人看的名稱（launchd 的標籤、crontab 的指令、systemd 的計時器名稱）；空＝路徑最後一段
	DisplayName string
}

// Name 名稱
func (t TaskInfo) Name() string {
	if t.DisplayName != "" {
		return t.DisplayName
	}
	return t.Path[strings.LastIndexAny(t.Path, `\/`)+1:]
}

// Folder 所在的資料夾（plist 所在的資料夾；crontab 的是「crontab」）
func (t TaskInfo) Folder() string {
	if i := strings.LastIndexAny(t.Path, `\/`); i > 0 {
		return t.Path[:i]
	}
	if strings.HasPrefix(t.Path, "/") {
		return "/"
	}
	return `\`
}

// Scan 一次掃描
type Scan struct {
	ScannedAt time.Time
	Tasks     []TaskInfo
	Error     string
	ElapsedMs int64
	Env       Environment
}

// Reader 讀與操作這台的排程（中樞只經這個介面；測試用假的）。在背景呼叫，失敗回錯誤（訊息給使用者看）
type Reader interface {
	Scan(ctx context.Context) Scan
	// Run 立即執行（不管觸發條件）
	Run(path string) error
	// Stop 停止執行中的這個排程
	Stop(path string) error
	SetEnabled(path string, enabled bool) error
}

func ptrTime(t time.Time) *time.Time { return &t }
