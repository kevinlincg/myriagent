package schedules

import (
	"strings"
	"testing"
	"time"
)

// 測試一律在固定的時區（沒有夏令時間）跑：時刻的展開與 C# 版的實測一樣
func init() { time.Local = time.FixedZone("CST", 8*3600) }

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func TestCronTriggersDescribe(t *testing.T) {
	cases := map[string]string{
		"30 2 * * *":      "每天 02:30",
		"0 9 * * 1-5":     "平日 09:00",
		"*/15 * * * *":    "每 15 分鐘（00:00 起）",
		"0 */2 * * *":     "每 2 小時（00:00 起）",
		"0 3 1 * *":       "每月 1 日 03:00",
		"0 8 * * 1,3":     "每週一、三 08:00",
		"@reboot":         "開機時",
		"@hourly":         "每小時（00:00 起）",
		"*/10 9-10 * * *": "每天 09:00 起 每 10 分鐘，持續 1 小時；每天 10:00 起 每 10 分鐘，持續 1 小時",
		"bad":             "其他條件",
	}
	for spec, want := range cases {
		if got := DescribeAll(CronTriggers(spec)); got != want {
			t.Errorf("%q：得到 %q，要 %q", spec, got, want)
		}
	}
	// 太多時刻的不亂猜
	if got := DescribeAll(CronTriggers("1,2,3,4,5,6,7 1,2,3,4,5,6,7,8 * * *")); got != "其他條件" {
		t.Errorf("組合太多應該是其他條件：%q", got)
	}
}

func TestParseCrontab(t *testing.T) {
	text := "# 註解\nMAILTO=\"\"\nPATH=/usr/local/bin:/usr/bin\n0 3 * * * /Users/me/bin/backup.sh >> /Users/me/backup.log 2>&1\n0 3 * * * /Users/me/bin/backup.sh >> /Users/me/backup.log 2>&1\n@daily \"/Applications/My App/run\" --x\n"
	entries := ParseCrontab(text, "me")
	if len(entries) != 3 {
		t.Fatalf("要 3 行，得到 %d", len(entries))
	}
	if entries[0].Key == entries[1].Key || !strings.HasSuffix(entries[1].Key, "-2") {
		t.Fatalf("同一個指令排兩次要加編號：%q %q", entries[0].Key, entries[1].Key)
	}
	if entries[0].Definition.Actions[0].Command != "/Users/me/bin/backup.sh" {
		t.Fatalf("程式不對：%+v", entries[0].Definition.Actions[0])
	}
	if entries[2].Definition.Actions[0].Command != "/Applications/My App/run" {
		t.Fatalf("引號包著的程式：%+v", entries[2].Definition.Actions[0])
	}
	vars := CronVariables(text)
	if vars["MAILTO"] != "" || vars["PATH"] != "/usr/local/bin:/usr/bin" {
		t.Fatalf("環境變數：%v", vars)
	}
	if CronLogPath(entries[0].Command) != "/Users/me/backup.log" {
		t.Fatalf("記錄檔：%q", CronLogPath(entries[0].Command))
	}
	if CronLogPath("x > /dev/null 2>&1") != "" {
		t.Fatal("/dev/null 不算記錄檔")
	}
	if got := CronDisplayName("/usr/bin/python3 /x/a.py 2>/tmp/e"); got != "python3 /x/a.py" {
		t.Fatalf("名稱：%q", got)
	}
}

func TestOccurrences(t *testing.T) {
	from := at("2026-11-20 10:00")
	to := from.Add(24 * time.Hour)
	// 每 15 分鐘
	up := Occurrences(CronTriggers("*/15 * * * *"), from, to, MaxOccurrences)
	if len(up) != 96 || !up[0].Equal(from) {
		t.Fatalf("每 15 分鐘 24 小時要 96 次，得到 %d（第一次 %v）", len(up), up)
	}
	// 平日 09:00：11/20 是週五 → 下一次是 11/23 週一（範圍外），只有……沒有
	up = Occurrences(CronTriggers("0 9 * * 1-5"), from, to, MaxOccurrences)
	if len(up) != 0 {
		t.Fatalf("週五 10:00 之後 24 小時內沒有平日 09:00：%v", up)
	}
	up = Occurrences(CronTriggers("0 9 * * 1-5"), from, from.AddDate(0, 0, 7), MaxOccurrences)
	if len(up) != 5 || !up[0].Equal(at("2026-11-23 09:00")) {
		t.Fatalf("一週內的平日 09:00：%v", up)
	}
	// 每月最後一天
	tr := NewTrigger(TriggerMonthly)
	s := BaseDate().Add(3 * time.Hour)
	tr.Start, tr.LastDayOfMonth = &s, true
	up = Occurrences([]Trigger{tr}, at("2026-02-01 00:00"), at("2026-04-01 00:00"), 10)
	if len(up) != 2 || !up[0].Equal(at("2026-02-28 03:00")) || !up[1].Equal(at("2026-03-31 03:00")) {
		t.Fatalf("每月最後一天：%v", up)
	}
}

func TestResultRules(t *testing.T) {
	if Classify(0).Level != LevelOK || Classify(1).Level != LevelFail || Classify(1).Text != "結束碼 1" {
		t.Fatal("結束碼 0、1")
	}
	if Classify(-15).Level != LevelWarn || Classify(-11).Level != LevelFail || Classify(-31).Text != "被訊號 31 結束" {
		t.Fatal("訊號")
	}
	if Classify(ResultRunning).Level != LevelInfo || IsFinal(ResultRunning) || IsFinal(ResultHasNotRun) || !IsFinal(1) {
		t.Fatal("執行中、還沒執行過不是最後結果")
	}
}

func TestRunRules(t *testing.T) {
	now := at("2026-11-20 12:00")
	triggers := CronTriggers("0 * * * *")
	last := at("2026-11-20 11:00")
	r := Observe("p", nil, &last, 1, triggers, now)
	if r == nil || r.Gap {
		t.Fatalf("第一筆：%+v", r)
	}
	// 一樣的時間不重記
	if Observe("p", r, &last, 1, triggers, now) != nil {
		t.Fatal("同一次不重記")
	}
	// 中間漏了 09:00、10:00
	prev := RunRecord{Path: "p", At: at("2026-11-20 08:00"), Code: 0}
	r2 := Observe("p", &prev, &last, 0, triggers, now)
	if r2 == nil || !r2.Gap {
		t.Fatalf("中間有沒觀察到的執行要標 gap：%+v", r2)
	}
	// 執行中不記
	if Observe("p", nil, &last, ResultRunning, triggers, now) != nil {
		t.Fatal("執行中不記")
	}
	// 通知只在新的一串失敗開始時
	fail := RunRecord{At: last, Code: 1}
	if !ShouldAlert(fail, &prev, now) || ShouldAlert(fail, &fail, now) || ShouldAlert(RunRecord{At: at("2026-11-19 20:00"), Code: 1}, nil, now) {
		t.Fatal("ShouldAlert")
	}
	runs := []RunRecord{{Code: 0}, {Code: 1}, {Code: -15}, {Code: 2}, {Code: 3}}
	if ConsecutiveFailures(runs) != 2 {
		t.Fatalf("連續失敗：%d", ConsecutiveFailures(runs))
	}
}

func TestDiagnoseBlock(t *testing.T) {
	now := at("2026-11-20 12:00")
	pct := 5.0
	if DiagnoseBlock("p", OriginAgent, false, false, nil, now, true, nil) != "設定頁關掉了自動診斷" {
		t.Fatal("設定")
	}
	if DiagnoseBlock("p", OriginSuspected, true, false, nil, now, true, nil) == "" {
		t.Fatal("疑似的不診斷")
	}
	log := []DiagnoseRecord{{Path: "P", At: now.Add(-time.Hour)}}
	if DiagnoseBlock("p", OriginAgent, true, false, log, now, true, nil) != "24 小時內已經自動診斷過" {
		t.Fatal("24 小時一次（不分大小寫）")
	}
	var many []DiagnoseRecord
	for i := 0; i < 10; i++ {
		many = append(many, DiagnoseRecord{Path: "x", At: now.Add(-time.Duration(i) * time.Minute)})
	}
	if DiagnoseBlock("p", OriginAgent, true, false, many, now, true, nil) != "今天已經自動診斷 10 次" {
		t.Fatal("每天 10 次")
	}
	if DiagnoseBlock("p", OriginAdopted, true, false, nil, now, true, &pct) != "每週額度剩不到 10%" {
		t.Fatal("額度")
	}
	if DiagnoseBlock("p", OriginAdopted, true, false, nil, now, true, nil) != "" {
		t.Fatal("可以診斷")
	}
}

const samplePlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>com.me.backup</string>
	<key>ProgramArguments</key>
	<array><string>/bin/sh</string><string>-c</string><string>/Users/me/repo/backup.sh &gt;&gt; ~/backup.log</string></array>
	<key>StartCalendarInterval</key>
	<array>
		<dict><key>Weekday</key><integer>1</integer><key>Hour</key><integer>9</integer><key>Minute</key><integer>0</integer></dict>
		<dict><key>Weekday</key><integer>2</integer><key>Hour</key><integer>9</integer><key>Minute</key><integer>0</integer></dict>
		<dict><key>Weekday</key><integer>3</integer><key>Hour</key><integer>9</integer><key>Minute</key><integer>0</integer></dict>
		<dict><key>Weekday</key><integer>4</integer><key>Hour</key><integer>9</integer><key>Minute</key><integer>0</integer></dict>
		<dict><key>Weekday</key><integer>5</integer><key>Hour</key><integer>9</integer><key>Minute</key><integer>0</integer></dict>
		<dict><key>Day</key><integer>1</integer><key>Hour</key><integer>3</integer></dict>
	</array>
	<key>StandardOutPath</key><string>/tmp/b.log</string>
	<key>StandardErrorPath</key><string>/tmp/b.log</string>
	<key>RunAtLoad</key><true/>
</dict>
</plist>`

func TestLaunchdToJob(t *testing.T) {
	job, err := LaunchdToJob(samplePlist, DomainUserAgent, "me", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if job.Label != "com.me.backup" || job.StandardOutPath != "/tmp/b.log" {
		t.Fatalf("標籤、輸出：%+v", job)
	}
	d := job.Definition
	if got := DescribeAll(d.Triggers); got != "平日 09:00；每月 1 日 03:00 起 每 1 分鐘，持續 1 小時；me 登入時" {
		t.Fatalf("觸發條件：%q", got)
	}
	a := d.Actions[0]
	if a.Command != "/bin/sh" || a.Arguments != `-c "/Users/me/repo/backup.sh >> ~/backup.log"` {
		t.Fatalf("動作：%+v", a)
	}
	if d.Description != "輸出與錯誤記到 /tmp/b.log" || !d.StartWhenAvailable || d.RunAs != "me" || d.LogonType != AgentLogonType {
		t.Fatalf("定義：%+v", d)
	}
	if _, err := LaunchdToJob("<html/>", DomainUserAgent, "me", nil, nil); err == nil {
		t.Fatal("不是 plist 要錯")
	}
	anchor := at("2026-11-20 21:13")
	iv, _ := LaunchdToJob(`<plist><dict><key>StartInterval</key><integer>3600</integer><key>Program</key><string>/x</string></dict></plist>`, DomainUserAgent, "me", nil, &anchor)
	if got := DescribeAll(iv.Definition.Triggers); got != "每小時（21:13 起）" {
		t.Fatalf("每隔多久：%q", got)
	}
	daemon, _ := LaunchdToJob(`<plist><dict><key>Program</key><string>/x</string><key>KeepAlive</key><true/></dict></plist>`, DomainDaemon, "me", nil, nil)
	if daemon.Definition.RunAs != "root" || !daemon.Definition.Elevated || DescribeAll(daemon.Definition.Triggers) != "開機時" {
		t.Fatalf("daemon：%+v", daemon.Definition)
	}
}

func TestLaunchdState(t *testing.T) {
	runs := map[string]LaunchdRun{}
	for _, line := range []string{
		"2026-11-19 21:13:12.378848 (gui/501/com.me.backup [92888]) <Notice>: Successfully spawned backup[92888] because interval",
		"2026-11-19 21:13:15.000001 (gui/501/com.me.backup [92888]) <Notice>: exited due to exit(3)",
		"2026-11-19 22:00:00.1 (gui/501/com.me.other [1]) <Notice>: exited due to SIGKILL | sent by launchd",
		"2026-11-19 22:00:00 (gui/501/com.me.other) <Notice>: something else",
	} {
		if e := ParseLaunchdLogLine(line); e != nil {
			ApplyLaunchdEvent(runs, *e)
		}
	}
	b := runs["gui/501/com.me.backup"]
	if b.FinishedStart == nil || !b.FinishedStart.Equal(time.Date(2026, 11, 19, 21, 13, 12, 0, time.Local)) || *b.Code != 3 || b.Running() {
		t.Fatalf("backup：%+v", b)
	}
	o := runs["gui/501/com.me.other"]
	if o.Code == nil || *o.Code != -9 || !o.FinishedStart.Equal(*o.FinishedAt) {
		t.Fatalf("沒看到開始的結束：%+v", o)
	}
	list := ParseLaunchctlList("PID\tStatus\tLabel\n-\t0\tcom.me.backup\n123\t-15\tcom.me.run\n")
	if list["com.me.backup"].Pid != nil || *list["com.me.backup"].Status != 0 || *list["com.me.run"].Pid != 123 {
		t.Fatalf("launchctl list：%+v", list)
	}
	sys := ParseLaunchctlSystemServices("system = {\n\tservices = {\n\t\t0\t-\tcom.x.a\n\t\t55\t0\tcom.x.b\n\t}\n}\n")
	if len(sys) != 2 || sys["com.x.a"].Status != nil || *sys["com.x.b"].Pid != 55 {
		t.Fatalf("print system：%+v", sys)
	}
	dis := ParseLaunchctlDisabled("disabled services = {\n\t\"com.me.backup\" => disabled\n\t\"com.me.run\" => enabled\n}\n")
	if !dis["com.me.backup"] || dis["com.me.run"] {
		t.Fatalf("print-disabled：%v", dis)
	}
	loaded := list["com.me.backup"]
	st := ResolveLaunchdState(true, nil, &loaded, &b, nil, nil)
	if st.State != StateReady || st.LastResult != 3 || st.LastRun == nil {
		t.Fatalf("Resolve：%+v", st)
	}
	off := true
	st = ResolveLaunchdState(true, &off, nil, nil, nil, nil)
	if st.State != StateDisabled || st.Enabled || st.LastResult != ResultHasNotRun {
		t.Fatalf("停用：%+v", st)
	}
	written := at("2026-11-20 08:00")
	st = ResolveLaunchdState(true, nil, &loaded, nil, nil, &written)
	if !st.LastRun.Equal(written) || st.LastResult != 0 {
		t.Fatalf("輸出檔的時間配 launchctl 的結束碼：%+v", st)
	}
}

func TestClassify(t *testing.T) {
	env := Environment{UserName: "me", MachineName: "mac", Variables: map[string]string{"HOME": "/Users/me"}, WorkRoots: []string{"/Users/me/Library/Application Support/Myriagent"}}
	sh := []Action{{Kind: "Exec", Command: "/bin/sh", Arguments: `-c "~/repo/run.sh >> ~/x.log"`}}
	if ClassifyOrigin(ClassifyInput{Actions: sh, Env: env}) != OriginSuspected {
		t.Fatal("sh -c 跑使用者資料夾的腳本是疑似")
	}
	vendor := []Action{{Kind: "Exec", Command: "/Library/Application Support/Vendor/updater"}}
	if ClassifyOrigin(ClassifyInput{Actions: vendor, Env: env}) != OriginOther {
		t.Fatal("廠商的不是")
	}
	work := []Action{{Kind: "Exec", Command: "/usr/bin/python3", Arguments: `"/Users/me/Library/Application Support/Myriagent/conversations/c1/workspace/a.py"`}}
	if ClassifyOrigin(ClassifyInput{Actions: work, Env: env}) != OriginSuspected {
		t.Fatal("對話工作區裡的腳本是疑似")
	}
	if ClassifyOrigin(ClassifyInput{Actions: vendor, AnyPathInRepo: true, Env: env}) != OriginSuspected {
		t.Fatal("在 git repo 裡的是疑似")
	}
	if got := WorkFolder(sh, env); got != "/Users/me/repo" {
		t.Fatalf("工作資料夾：%q", got)
	}
	if got := PathsIn(`-c "~/repo/run.sh >> ~/x.log 2>/dev/null"`, env); strings.Join(got, ",") != "/Users/me/repo/run.sh,/Users/me/x.log" {
		t.Fatalf("參數裡的路徑：%v", got)
	}
	if _, ok := Expand("$UNKNOWN/x", env); ok {
		t.Fatal("展不開的變數")
	}
}

func TestOriginParse(t *testing.T) {
	hits := ParseCommandHits("cp com.me.job.plist ~/Library/LaunchAgents/com.me.job.plist && launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.me.job.plist")
	want := []CommandHit{{VerbCreate, "com.me.job"}, {VerbChange, "com.me.job"}}
	if len(hits) != 2 || hits[0] != want[0] || hits[1] != want[1] {
		t.Fatalf("launchd：%+v", hits)
	}
	hits = ParseCommandHits("launchctl kickstart gui/501/com.me.job; launchctl list; rm ~/Library/LaunchAgents/old.plist")
	if len(hits) != 2 || hits[0] != (CommandHit{VerbRun, "com.me.job"}) || hits[1] != (CommandHit{VerbDelete, "old"}) {
		t.Fatalf("執行、刪除：%+v", hits)
	}
	if hits := ParseCommandHits("crontab -l"); len(hits) != 0 {
		t.Fatalf("crontab -l 只是列出：%+v", hits)
	}
	if hits := ParseCommandHits("(crontab -l; echo '0 3 * * * x') | crontab -"); len(hits) != 1 || hits[0].Verb != VerbUnknown {
		t.Fatalf("改 crontab：%+v", hits)
	}
	hits = ParseCommandHits("systemctl --user daemon-reload && systemctl --user enable --now backup.timer")
	if len(hits) != 2 || hits[1] != (CommandHit{VerbCreate, "backup"}) {
		t.Fatalf("systemd：%+v", hits)
	}
	if hits := ParseCommandHits("systemctl restart nginx"); len(hits) != 0 {
		t.Fatalf("系統的服務不算：%+v", hits)
	}
	if !NameMatches("com.me.job", "/Users/me/Library/LaunchAgents/com.me.job.plist") || !NameMatches("backup", "/home/me/.config/systemd/user/backup.timer") ||
		NameMatches("x", "crontab/abc") {
		t.Fatal("NameMatches")
	}
}

func TestSystemdCalendar(t *testing.T) {
	cases := map[string]string{
		"daily":                "0 0 * * *",
		"Mon..Fri 09:00":       "0 9 * * 1-5",
		"*-*-* 03:30:00":       "30 3 * * *",
		"*:0/15":               "0/15 * * * *",
		"Sat,Sun *-*-01 08:00": "0 8 1 * 6,0",
		"weekly":               "0 0 * * 1",
		"*-01,07-01 00:00:00":  "0 0 1 1,7 *",
	}
	for in, want := range cases {
		got, ok := SystemdCalendarToCron(in)
		if !ok || got != want {
			t.Errorf("%q：得到 %q（%v），要 %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"2026-01-01 00:00", "*-*-~1", "*-*-* 00:00:30", "Mon UTC"} {
		if _, ok := SystemdCalendarToCron(bad); ok {
			t.Errorf("%q 應該換不了", bad)
		}
	}
	if d, ok := ParseSystemdSpan("1h 30min"); !ok || d != 90*time.Minute {
		t.Fatalf("時間長度：%v", d)
	}
	if d, ok := ParseSystemdSpan("90"); !ok || d != 90*time.Second {
		t.Fatalf("沒單位是秒：%v", d)
	}
}

func TestSystemdState(t *testing.T) {
	out := "Id=backup.timer\nActiveState=active\nUnitFileState=enabled\nLastTriggerUSec=Thu 2026-11-19 03:00:00 CST\nNextElapseUSecRealtime=Fri 2026-11-20 03:00:00 CST\n\n" +
		"Id=backup.service\nActiveState=inactive\nExecMainStartTimestamp=Thu 2026-11-19 03:00:01 CST\nExecMainExitTimestamp=Thu 2026-11-19 03:00:05 CST\nExecMainCode=1\nExecMainStatus=2\n"
	props := ParseSystemctlShow(out)
	st := ResolveSystemdState(props["backup.timer"], props["backup.service"])
	if st.State != StateReady || !st.Enabled || st.LastResult != 2 || !st.LastRun.Equal(time.Date(2026, 11, 19, 3, 0, 1, 0, time.Local)) || st.NextRun == nil {
		t.Fatalf("狀態：%+v", st)
	}
	props["backup.service"]["ExecMainCode"] = "2"
	props["backup.service"]["ExecMainStatus"] = "15"
	if st := ResolveSystemdState(props["backup.timer"], props["backup.service"]); st.LastResult != -15 {
		t.Fatalf("被訊號結束：%+v", st)
	}
	props["backup.timer"]["UnitFileState"] = "disabled"
	props["backup.timer"]["ActiveState"] = "inactive"
	if st := ResolveSystemdState(props["backup.timer"], props["backup.service"]); st.State != StateDisabled || st.NextRun != nil {
		t.Fatalf("停用：%+v", st)
	}
	u := ParseUnitFile("[Timer]\nOnCalendar=Mon..Fri 09:00\nPersistent=true\n[Install]\nWantedBy=timers.target\n")
	svc := ParseUnitFile("[Unit]\nDescription=備份\n[Service]\nType=oneshot\nExecStart=-/home/me/bin/backup.sh --all\nWorkingDirectory=~\n")
	def := SystemdTimerDefinition(u, svc, "me", "/home/me", BaseDate())
	if DescribeAll(def.Triggers) != "平日 09:00" || def.Actions[0].Command != "/home/me/bin/backup.sh" || def.Actions[0].WorkingDirectory != "/home/me" || !def.StartWhenAvailable || def.Description != "備份" {
		t.Fatalf("定義：%+v", def)
	}
}

func TestTimedParseWhen(t *testing.T) {
	now := at("2026-11-20 10:00")
	if c, _, p := ParseWhen("daily", "09:00", now); c != "0 9 * * *" || p != "" {
		t.Fatalf("daily：%q %q", c, p)
	}
	if c, _, _ := ParseWhen("weekdays", "09:05", now); c != "5 9 * * 1-5" {
		t.Fatalf("weekdays：%q", c)
	}
	if c, _, _ := ParseWhen("every", "30m", now); c != "*/30 * * * *" {
		t.Fatalf("every：%q", c)
	}
	if _, _, p := ParseWhen("every", "7m", now); p == "" {
		t.Fatal("7 分鐘除不盡 60")
	}
	if _, _, p := ParseWhen("every", "3m", now); p == "" {
		t.Fatal("至少 5 分鐘")
	}
	if c, _, _ := ParseWhen("every", "2h", now); c != "0 */2 * * *" {
		t.Fatalf("2h：%q", c)
	}
	if _, a, _ := ParseWhen("at", "09:00", now); a == nil || !a.Equal(at("2026-11-21 09:00")) {
		t.Fatalf("已過的時刻是明天：%v", a)
	}
	if _, a, _ := ParseWhen("at", "14:00", now); a == nil || !a.Equal(at("2026-11-20 14:00")) {
		t.Fatalf("今天：%v", a)
	}
	if _, a, _ := ParseWhen("at", "2026-12-01 08:30", now); a == nil || !a.Equal(at("2026-12-01 08:30")) {
		t.Fatalf("日期：%v", a)
	}
	if _, a, _ := ParseWhen("at", "1/5 08:30", now); a == nil || !a.Equal(at("2027-01-05 08:30")) {
		t.Fatalf("沒寫年份、今年已過是明年：%v", a)
	}
	if _, _, p := ParseWhen("at", "2026-01-01 08:30", now); p != "時間已經過了" {
		t.Fatalf("過去：%q", p)
	}
	if _, _, p := ParseWhen("cron", "* * * * *", now); p == "" {
		t.Fatal("每分鐘太密")
	}
	if c, _, p := ParseWhen("cron", "0  9 * * MON-FRI", now); c != "0 9 * * MON-FRI" || p != "" {
		t.Fatalf("cron：%q %q", c, p)
	}
}

func TestCronSpecDescribeAndNext(t *testing.T) {
	cases := map[string]string{
		"*/15 * * * *": "每 15 分鐘", "0 * * * *": "每小時", "5 * * * *": "每小時的 :05", "0 9 * * *": "每天 09:00",
		"0 9 * * 1-5": "平日 09:00", "0 9 * * 0,6": "週末 09:00", "0 8 * * 1,3": "每週一、三 08:00", "0 3 1,15 * *": "每月 1、15 日 03:00",
		"0 3 1 * 1": "cron：0 3 1 * 1",
	}
	for c, want := range cases {
		s, p := ParseCronSpec(c)
		if s == nil {
			t.Fatalf("%q：%s", c, p)
		}
		if got := s.Describe(); got != want {
			t.Errorf("%q：得到 %q，要 %q", c, got, want)
		}
	}
	s, _ := ParseCronSpec("0 3 1 * 1")
	// 日與星期都有限制：其中一個符合（12/1 週二、11/23 週一）
	n := s.Next(at("2026-11-20 10:00"))
	if !n.Equal(at("2026-11-23 03:00")) {
		t.Fatalf("下一次：%v", n)
	}
	feb30, _ := ParseCronSpec("0 0 30 2 *")
	if feb30.Next(at("2026-11-20 10:00")) != nil {
		t.Fatal("2 月 30 日不存在")
	}
}

func TestTimedDecide(t *testing.T) {
	created := at("2026-11-20 08:00")
	m := &TimedMessage{ID: "tm_1", Text: "整理報表\n細節", Cron: "0 9 * * *", Enabled: true, CreatedAt: created}
	if d, _, _ := Decide(m, at("2026-11-20 08:59")); d != DueWait {
		t.Fatal("還沒到")
	}
	if d, s, late := Decide(m, at("2026-11-20 09:01")); d != DueFire || !s.Equal(at("2026-11-20 09:00")) || late {
		t.Fatalf("準時：%v %v %v", d, s, late)
	}
	// 錯過三天只補一次：記成最後一次錯過的
	if d, s, late := Decide(m, at("2026-11-23 10:00")); d != DueFire || !s.Equal(at("2026-11-23 09:00")) || !late {
		t.Fatalf("補送：%v %v %v", d, s, late)
	}
	one := at("2026-11-20 09:00")
	o := &TimedMessage{Text: "x", At: &one, Enabled: true, CreatedAt: created}
	if d, _, _ := Decide(o, at("2026-11-20 22:00")); d != DueMissed {
		t.Fatal("一次性的錯過超過 12 小時不補")
	}
	if d, _, late := Decide(o, at("2026-11-20 20:00")); d != DueFire || !late {
		t.Fatal("12 小時內補送")
	}
	if LabelOf(m) != "整理報表" || DescribeTimed(m) != "每天 09:00" {
		t.Fatalf("名稱、摘要：%q %q", LabelOf(m), DescribeTimed(m))
	}
	if !strings.HasPrefix(AgentText(m, at("2026-11-20 09:00"), true), "（這是 Myriagent 的定時訊息「整理報表」，每天 09:00，原定 2026/11/20 09:00，電腦或 app 當時沒開，現在補送）\n\n整理報表") {
		t.Fatalf("給代理的文字：%q", AgentText(m, at("2026-11-20 09:00"), true))
	}
	if up := Upcoming(m, at("2026-11-20 10:00"), at("2026-11-21 10:00")); len(up) != 1 || !up[0].Equal(at("2026-11-21 09:00")) {
		t.Fatalf("接下來：%v", up)
	}
	for i := 0; i < 40; i++ {
		AddRun(m, TimedRun{Outcome: "sent"})
	}
	if len(m.Runs) != KeepTimedRuns {
		t.Fatal("留 30 筆")
	}
	if !WaitsForContinuation(false, true, false) || WaitsForContinuation(true, true, true) {
		t.Fatal("WaitsForContinuation")
	}
}
