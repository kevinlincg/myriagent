package schedules

import (
	"fmt"
	"strings"
)

// 排程頁「交給代理」的說明（放進草稿或自動送出；C# 的 ScheduleAgentPromptRules）：排程的資料、最近的執行、完整定義、要做的事、固定的規則。
// task：diagnose（診斷最近一次失敗）、explain（說明它在做什麼）、retime（改執行時間）、retire（停用並清理）、custom（自訂）、auto（自動診斷）

// Brief 交給代理的排程資料（中樞在鎖裡組好）
type Brief struct {
	Task        TaskInfo
	Machine     string
	Origin      Origin
	OriginEntry *OriginEntry
	Runs        []RunRecord
	Changes     []ChangeEntry
	WorkFolder  string
}

// Tasks 交給代理的種類
var Tasks = []string{"diagnose", "explain", "retime", "retire", "custom", "auto"}

// IsTask 認得的種類
func IsTask(task string) bool {
	for _, t := range Tasks {
		if t == task {
			return true
		}
	}
	return false
}

// AutoDisplay 自動診斷在對話裡顯示的那一句（代理看完整說明）
func AutoDisplay(b Brief, run RunRecord) string {
	return fmt.Sprintf("（自動診斷）排程 %s 在 %s 執行失敗（%s），請找出原因，先不要修改。", b.Task.Path, run.At.Format("1/2 15:04"), Classify(run.Code).Text)
}

// IsCron crontab 的排程
func IsCron(t TaskInfo) bool { return t.Definition != nil && t.Definition.LogonType == CronLogonType }

// IsSystemd systemd 的使用者計時器
func IsSystemd(t TaskInfo) bool {
	return t.Definition != nil && t.Definition.LogonType == SystemdLogonType
}

// BuildPrompt 給代理的完整說明
func BuildPrompt(task string, b Brief) string {
	cron, systemd := IsCron(b.Task), IsSystemd(b.Task)
	var sb strings.Builder
	sb.WriteString("這是 Myriagent 排程頁交給你的工作，對象是這台電腦（" + b.Machine)
	switch {
	case cron:
		sb.WriteString("）的 crontab 排程。\n\n")
	case systemd:
		sb.WriteString("）的 systemd 使用者計時器。\n\n")
	default:
		sb.WriteString("）的 launchd 排程。\n\n")
	}
	appendTask(&sb, task, b)
	sb.WriteString("\n## 排程\n\n")
	appendFacts(&sb, b)
	appendRuns(&sb, b)
	appendChanges(&sb, b)
	if strings.TrimSpace(b.Task.XML) != "" {
		switch {
		case cron:
			sb.WriteString("\n## 完整定義（crontab 的這一行）\n\n```\n")
		case systemd:
			sb.WriteString("\n## 完整定義（systemd 的 .timer 與 .service）\n\n```ini\n")
		default:
			sb.WriteString("\n## 完整定義（launchd 的 plist）\n\n```xml\n")
		}
		sb.WriteString(strings.TrimSpace(b.Task.XML))
		sb.WriteString("\n```\n")
	}
	sb.WriteString("\n## 規則\n\n")
	switch {
	case cron:
		sb.WriteString("- 這個排程在目前使用者的 crontab（`crontab -l` 看得到）。改它用 `crontab -l` 取出、改好再以 `crontab 檔案` 寫回，不要動到別行。\n")
		if systemdHost() {
			sb.WriteString("- cron 執行時的環境很少（PATH 只有 /usr/bin:/bin、工作資料夾是使用者資料夾）、電腦關機時不會補跑；需要這些的話建議改成 systemd 的使用者計時器，改之前先問我。\n")
		} else {
			sb.WriteString("- cron 執行時的環境很少（PATH 只有 /usr/bin:/bin、工作資料夾是使用者資料夾）、電腦睡著時不會補跑；需要這些的話建議改成 launchd 的工作，改之前先問我。\n")
		}
		sb.WriteString("- 刪除排程之前，先問我。\n")
	case systemd:
		sb.WriteString("- 這個排程是 systemd 的使用者計時器，定義在上面「名稱」那個 .timer 與它啟動的 .service。改定義就改那兩個檔，改完要 `systemctl --user daemon-reload` 才生效（計時器的時間改了再 `systemctl --user restart 名稱.timer`）。\n")
		sb.WriteString("- 立刻執行一次用 `systemctl --user start 名稱.service`；看狀態與上次的結束碼用 `systemctl --user status 名稱.service`，輸出用 `journalctl --user -u 名稱.service`。\n")
		sb.WriteString("- 刪除排程、改成系統層級（/etc/systemd/system，要 sudo）之前，先問我。\n")
	default:
		sb.WriteString("- 這個排程是 launchd 的工作，定義在上面「名稱」那個 .plist。改定義就改那個檔，改完要重新載入才生效：`launchctl bootout 網域/標籤` 再 `launchctl bootstrap 網域 plist路徑`（使用者的工作網域是 `gui/$(id -u)`，/Library/LaunchDaemons 的是 `system`）。\n")
		sb.WriteString("- 立刻執行一次用 `launchctl kickstart 網域/標籤`；看狀態與上次的結束碼用 `launchctl print 網域/標籤`。\n")
		sb.WriteString("- 刪除排程、改執行身分之前，先問我。/Library 底下的工作要系統管理員（sudo），請先告訴我原因。\n")
	}
	sb.WriteString("- 改完不必再列出排程清單：Myriagent 會在這一輪結束後自動重新掃描，排程頁會記下這個對話做的變更。\n")
	return sb.String()
}

// systemdHost 這台用 systemd（Linux）；測試可換
var systemdHost = func() bool { return goos() == "linux" }

func appendTask(sb *strings.Builder, task string, b Brief) {
	lastText := Classify(b.Task.LastResult).Text
	if n := len(b.Runs); n > 0 {
		last := b.Runs[n-1]
		lastText = last.At.Format("1/2 15:04") + " 的" + Classify(last.Code).Text
	}
	sb.WriteString("## 要做的事\n\n")
	switch task {
	case "diagnose":
		fmt.Fprintf(sb, "這個排程最近一次執行失敗（%s）。請找出原因：先看定義與它執行的程式、腳本與記錄檔（如果有寫記錄），必要時用同樣的指令與工作資料夾手動試跑一次。說明原因並提出修正方式，修正前先跟我確認。\n", lastText)
	case "auto":
		fmt.Fprintf(sb, "這個排程剛剛執行失敗（%s），Myriagent 自動請你診斷。**只診斷、不要修改任何檔案或排程**：看定義、它執行的程式與腳本、記錄檔，必要時可以用同樣的指令手動試跑（試跑本身不能改資料或對外發送）。最後列出原因與建議的修法，等我決定。\n", lastText)
	case "explain":
		sb.WriteString("請說明這個排程在做什麼：它執行的程式或腳本做了哪些事、讀寫哪些資料、多久跑一次、失敗會有什麼影響、有沒有更好的做法。不要修改任何東西。\n")
	case "retime":
		sb.WriteString("我想改這個排程的執行時間。新的時間：（請在這裡寫，例如「每天 03:00」「平日每小時」）\n\n改之前確認新的時間不會和這台電腦的其他排程衝突，改完說明下一次會在什麼時候執行。\n")
	case "retire":
		sb.WriteString("這個排程不再需要了。請先確認沒有其他東西依賴它（別的排程、服務、網站、其他程式），然後停用它；跟我確認之後再刪除排程，並清理只為它存在的檔案（腳本、記錄、設定）。repo 裡有版控的檔案要刪之前先問我。\n")
	default:
		sb.WriteString("我想要：（請在這裡寫）\n")
	}
}

func appendFacts(sb *strings.Builder, b Brief) {
	t := b.Task
	d := t.Definition
	sb.WriteString("- 名稱：`" + t.Path + "`\n")
	sb.WriteString("- 狀態：" + StateText(t) + "\n")
	if d != nil {
		sb.WriteString("- 何時：" + DescribeAll(d.Triggers) + "\n")
		sb.WriteString("- 動作：`" + ActionText(d.Actions) + "`\n")
	}
	if t.NextRun != nil {
		sb.WriteString("- 下次執行：" + t.NextRun.Format("2006/1/2 15:04") + "\n")
	}
	if b.WorkFolder != "" {
		sb.WriteString("- 工作資料夾：`" + b.WorkFolder + "`\n")
	}
	if t.RepoRoot != "" {
		sb.WriteString("- 所在的 git repo：`" + t.RepoRoot + "`\n")
	}
	if d != nil {
		sb.WriteString("- 執行身分：" + RunAsText(*d) + "\n")
	}
	if t.MissedRuns > 0 {
		fmt.Fprintf(sb, "- 錯過的執行：%d 次\n", t.MissedRuns)
	}
	sb.WriteString("- 來源：" + OriginText(b) + "\n")
	if d != nil && d.Description != "" {
		sb.WriteString("- 描述：" + d.Description + "\n")
	}
}

func appendRuns(sb *strings.Builder, b Brief) {
	if len(b.Runs) == 0 {
		if b.Task.LastRun != nil {
			sb.WriteString("\n## 上次執行\n\n- " + b.Task.LastRun.Format("1/2 15:04") + "：" + Classify(b.Task.LastResult).Text + "\n")
		}
		return
	}
	sb.WriteString("\n## 最近的執行（Myriagent 觀察到的，新的在前）\n\n")
	for i, n := len(b.Runs)-1, 0; i >= 0 && n < 10; i, n = i-1, n+1 {
		run := b.Runs[i]
		r := Classify(run.Code)
		sb.WriteString("- " + run.At.Format("1/2 15:04") + "：" + r.Text)
		if r.Level == LevelFail && !strings.HasPrefix(r.Text, "結束碼") {
			sb.WriteString("（" + CodeText(run.Code) + "）")
		}
		if run.Gap {
			sb.WriteString("（和前一次之間還有沒觀察到的執行）")
		}
		sb.WriteString("\n")
	}
}

func appendChanges(sb *strings.Builder, b Brief) {
	if len(b.Changes) == 0 {
		return
	}
	sb.WriteString("\n## 變更紀錄\n\n")
	for i, n := len(b.Changes)-1, 0; i >= 0 && n < 10; i, n = i-1, n+1 {
		c := b.Changes[i]
		sb.WriteString("- " + c.At.Format("2006/1/2 15:04") + " " + ChangeText(c.Kind))
		if c.Title != "" {
			sb.WriteString("（對話「" + c.Title + "」）")
		}
		sb.WriteString("\n")
	}
}

// TaskLabel 要代理做的事的名稱
func TaskLabel(task string) string {
	switch task {
	case "diagnose":
		return "診斷最近一次失敗"
	case "explain":
		return "說明它在做什麼"
	case "retime":
		return "改執行時間"
	case "retire":
		return "停用並清理"
	case "auto":
		return "自動診斷"
	}
	return "自訂"
}

// ChangeText 變更的種類
func ChangeText(kind string) string {
	switch kind {
	case "created":
		return "建立"
	case "changed":
		return "修改定義"
	case "deleted":
		return "刪除"
	case "enabled":
		return "啟用"
	case "disabled":
		return "停用"
	}
	return kind
}

// StateText 狀態的中文
func StateText(t TaskInfo) string {
	if t.Error != "" && t.Definition == nil {
		return "讀不到"
	}
	switch t.State {
	case StateRunning:
		return "執行中"
	case StateDisabled:
		return "已停用"
	case StateQueued:
		return "排隊中"
	case StateReady:
		return "已啟用"
	}
	if t.Enabled {
		return "已啟用"
	}
	return "已停用"
}

// RunAsText 執行身分
func RunAsText(d Definition) string {
	who := d.RunAs
	if who == "" {
		if d.LogonType == AgentLogonType {
			who = "登入的使用者"
		} else {
			who = "（未指定）"
		}
	}
	var when string
	switch d.LogonType {
	case AgentLogonType:
		if d.RunAs != "" {
			when = "只在使用者登入時執行"
		} else {
			when = "每個使用者登入時各自執行"
		}
	case DaemonLogonType:
		when = "開機後就執行，不論使用者是否登入"
	case CronLogonType:
		when = "不論使用者是否登入都執行（cron）"
	case SystemdLogonType:
		when = "使用者的 systemd 執行（沒開 linger 時只在登入期間）"
	case "":
	default:
		when = d.LogonType
	}
	text := who
	if when != "" {
		text = who + "，" + when
	}
	if d.Elevated {
		text += "，以最高權限執行"
	}
	return text
}

// OriginText 來源的說明
func OriginText(b Brief) string {
	if o := b.OriginEntry; o != nil {
		verb := "修改"
		if o.Verb == "created" {
			verb = "建立"
		}
		where := "Myriagent 的對話"
		if o.Source == "transcript" {
			where = "外部 CLI 的工作階段"
		}
		title, provider := "", ""
		if o.Title != "" {
			title = "「" + o.Title + "」"
		}
		if o.Provider != "" {
			provider = "（" + o.Provider + "）"
		}
		return o.At.Format("2006/1/2") + " 由" + where + title + provider + verb
	}
	switch b.Origin {
	case OriginAdopted:
		return "使用者納入管理（不知道是誰建立的）"
	case OriginSuspected:
		return "不知道是誰建立的，看起來是自己開發的（執行的是使用者資料夾裡的程式或腳本）"
	}
	return "不知道是誰建立的"
}

// CreatePrompt 「新增排程」的說明（macOS 是 launchd 的工作，Linux 是 systemd 的使用者計時器）
func CreatePrompt(machine string) string {
	if systemdHost() {
		return "這是 Myriagent 排程頁交給你的工作：幫我在這台電腦（" + machine + "）建立一個排程（systemd 的使用者計時器）。\n\n" +
			"## 要做的事\n\n（請在這裡寫要定時做什麼、多久一次，例如「每天 03:00 備份這個資料夾的資料庫」）\n\n" +
			"## 規則\n\n" +
			"- 建成使用者的 systemd 計時器：`~/.config/systemd/user/名稱.service`（Type=oneshot、ExecStart 用完整路徑）加上同名的 `名稱.timer`（OnCalendar 寫時間、Persistent=true 讓關機錯過的開機後補跑、[Install] WantedBy=timers.target），" +
			"再 `systemctl --user daemon-reload` 與 `systemctl --user enable --now 名稱.timer`；不要用 crontab（Myriagent 看不到它的執行結果）。要執行的腳本放在這個工作資料夾（有 git 就一起提交）。\n" +
			"- 預設只在我登入期間執行；要登出後也跑（`loginctl enable-linger`）或以 root 執行（/etc/systemd/system，要 sudo），先問我。\n" +
			"- 腳本結束碼 0 才代表成功；輸出會進 journal（`journalctl --user -u 名稱.service` 看得到）。systemd 給的環境很少，需要的路徑寫在腳本或 Environment= 裡。\n" +
			"- 建好之後用 `systemctl --user start 名稱.service` 手動執行一次確認成功；Myriagent 會在這一輪結束後自動掃描，排程頁會顯示這個排程與這個對話。\n"
	}
	return "這是 Myriagent 排程頁交給你的工作：幫我在這台電腦（" + machine + "）建立一個排程（macOS 的 launchd 工作）。\n\n" +
		"## 要做的事\n\n（請在這裡寫要定時做什麼、多久一次，例如「每天 03:00 備份這個資料夾的資料庫」）\n\n" +
		"## 規則\n\n" +
		"- 建成使用者的 launchd 工作：定義檔放在 `~/Library/LaunchAgents/標籤.plist`（標籤用反過來的網域寫法，例如 `com.我的名字.備份`），用 `launchctl bootstrap gui/$(id -u) plist路徑` 載入；不要用 crontab（電腦睡著時不會補跑，Myriagent 也看不到它的執行結果）。要執行的腳本放在這個工作資料夾（有 git 就一起提交）。\n" +
		"- 定時用 StartCalendarInterval（幾點幾分、星期幾）或 StartInterval（每隔幾秒）。預設只在我登入時執行；要開機就跑、以 root 執行（/Library/LaunchDaemons，要 sudo），先問我。\n" +
		"- plist 裡設定 StandardOutPath 與 StandardErrorPath（失敗時找得到原因），腳本結束碼 0 才代表成功。launchd 給的環境很少（PATH 只有 /usr/bin:/bin:/usr/sbin:/sbin），需要的路徑寫在腳本或 EnvironmentVariables 裡。\n" +
		"- 建好之後用 `launchctl kickstart gui/$(id -u)/標籤` 手動執行一次確認成功；Myriagent 會在這一輪結束後自動掃描，排程頁會顯示這個排程與這個對話。\n"
}

// Conditions 明細的「條件」：會讓排程不執行或提早停止的設定
func Conditions(d Definition) []string {
	out := []string{}
	if d.DisallowStartIfOnBatteries {
		out = append(out, "使用電池時不啟動")
	}
	if d.StopIfGoingOnBatteries {
		out = append(out, "改用電池時停止")
	}
	if d.RunOnlyIfNetworkAvailable {
		out = append(out, "有網路才執行")
	}
	if d.RunOnlyIfIdle {
		out = append(out, "電腦閒置時才執行")
	}
	if d.StartWhenAvailable {
		out = append(out, "錯過時盡快補跑")
	}
	if d.WakeToRun {
		out = append(out, "喚醒電腦來執行")
	}
	if d.TimeLimit > 0 {
		out = append(out, "逾時"+SpanText(d.TimeLimit))
	}
	switch d.MultipleInstances {
	case "Parallel":
		out = append(out, "可以同時執行多個")
	case "Queue":
		out = append(out, "上一次沒結束就排隊")
	case "StopExisting":
		out = append(out, "上一次沒結束就先停掉")
	}
	if d.Hidden {
		out = append(out, "隱藏")
	}
	return out
}
