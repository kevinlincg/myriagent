package schedules

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner 假的系統工具：依「指令 參數」回固定的輸出，記下每一次呼叫（絕不執行真的 launchctl、crontab、systemctl）
type fakeRunner struct {
	mu      sync.Mutex
	outputs map[string]string
	errs    map[string]error
	calls   []string
	started []string
}

func (f *fakeRunner) Output(name string, args ...string) (string, error) {
	key := strings.TrimSpace(filepath.Base(name) + " " + strings.Join(args, " "))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, key)
	if err, ok := f.errs[key]; ok {
		return "", err
	}
	if out, ok := f.outputs[key]; ok {
		return out, nil
	}
	return "", errors.New("Could not find service")
}

func (f *fakeRunner) StartDetached(name string, args, env []string, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, name+" "+strings.Join(args, " ")+" @"+dir+" "+strings.Join(env, ";"))
	return nil
}

func (f *fakeRunner) called(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func TestLaunchdReaderScanAndActions(t *testing.T) {
	home := t.TempDir()
	agents := filepath.Join(home, "Library", "LaunchAgents")
	daemons := filepath.Join(t.TempDir(), "LaunchDaemons")
	logDir := t.TempDir()
	for _, d := range []string{agents, daemons} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(agents, "com.me.backup.plist"), []byte(samplePlist), 0o644)
	_ = os.WriteFile(filepath.Join(agents, "broken.plist"), []byte("<plist><dict>"), 0o644)
	_ = os.WriteFile(filepath.Join(daemons, "com.vendor.d.plist"), []byte(`<plist><dict><key>Label</key><string>com.vendor.d</string><key>Program</key><string>/Library/Vendor/d</string><key>RunAtLoad</key><true/></dict></plist>`), 0o644)
	// 腳本在 git repo 裡
	repo := filepath.Join(home, "repo")
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(logDir, "launchd.log"), []byte(
		"2026-11-19 09:00:00.1 (gui/501/com.me.backup [1]) <Notice>: Successfully spawned sh[1] because calendar\n"+
			"2026-11-19 09:00:04.1 (gui/501/com.me.backup [1]) <Notice>: exited due to exit(1)\n"+
			"2026-11-19 09:00:04.1 (gui/502/com.me.backup [1]) <Notice>: exited due to exit(9)\n"), 0o644)
	plist := strings.Replace(samplePlist, "/Users/me/repo/backup.sh", repo+"/backup.sh", 1)
	_ = os.WriteFile(filepath.Join(agents, "com.me.backup.plist"), []byte(plist), 0o644)

	fr := &fakeRunner{outputs: map[string]string{
		"launchctl list":                               "PID\tStatus\tLabel\n-\t1\tcom.me.backup\n",
		"launchctl print system":                       "services = {\n\t0\t-\tcom.vendor.d\n}\n",
		"launchctl print-disabled gui/501":             "",
		"launchctl print-disabled system":              "",
		"crontab -l":                                   "*/30 * * * * /bin/echo hi\n",
		"launchctl print gui/501/com.me.backup":        "loaded",
		"launchctl kickstart gui/501/com.me.backup":    "",
		"launchctl kill SIGTERM gui/501/com.me.backup": "",
		"launchctl bootout gui/501/com.me.backup":      "",
		"launchctl disable gui/501/com.me.backup":      "",
	}}
	r := &LaunchdReader{Runner: fr, Home: home, UID: 501, LogDir: logDir,
		Folders: []LaunchdFolder{{agents, DomainUserAgent}, {daemons, DomainDaemon}}}
	scan := r.Scan(context.Background())
	if len(scan.Tasks) != 4 {
		t.Fatalf("要 4 個（2 個 agent、1 個 daemon、1 行 crontab），得到 %d：%+v", len(scan.Tasks), scan.Tasks)
	}
	byName := map[string]TaskInfo{}
	for _, x := range scan.Tasks {
		byName[x.Name()] = x
	}
	b := byName["com.me.backup"]
	if b.State != StateReady || b.LastResult != 1 || b.LastRun == nil || b.RepoRoot != repo || b.NextRun == nil || b.Error != "" {
		t.Fatalf("backup：%+v", b)
	}
	if br := byName["broken"]; br.Definition != nil || !strings.HasPrefix(br.Error, "定義讀不懂") {
		t.Fatalf("壞掉的 plist：%+v", br)
	}
	if d := byName["com.vendor.d"]; d.State != StateReady || d.Definition.RunAs != "root" {
		t.Fatalf("daemon：%+v", d)
	}
	var cron TaskInfo
	for _, x := range scan.Tasks {
		if IsCronPath(x.Path) {
			cron = x
		}
	}
	if cron.Name() != "echo hi" || cron.Folder() != "crontab" || cron.NextRun == nil {
		t.Fatalf("crontab：%+v", cron)
	}

	// 操作：只呼叫假的 launchctl
	path := filepath.Join(agents, "com.me.backup.plist")
	if err := r.Run(path); err != nil || !fr.called("launchctl kickstart gui/501/com.me.backup") {
		t.Fatalf("立即執行：%v %v", err, fr.calls)
	}
	if err := r.Stop(path); err != nil || !fr.called("launchctl kill SIGTERM gui/501/com.me.backup") {
		t.Fatalf("停止：%v", err)
	}
	if err := r.SetEnabled(path, false); err != nil || !fr.called("launchctl bootout") || !fr.called("launchctl disable gui/501/com.me.backup") {
		t.Fatalf("停用：%v %v", err, fr.calls)
	}
	if err := r.Run(filepath.Join(daemons, "com.vendor.d.plist")); err == nil || !strings.Contains(err.Error(), "權限不足") {
		t.Fatalf("daemon 要 root：%v", err)
	}
	if err := r.Run(filepath.Join(home, "elsewhere.plist")); err == nil {
		t.Fatal("不在資料夾裡的找不到")
	}
	// crontab 的立即執行：照 cron 的方式在背景跑
	if err := r.Run(cron.Path); err != nil || len(fr.started) != 1 || !strings.Contains(fr.started[0], "/bin/sh -c /bin/echo hi @"+home) || !strings.Contains(fr.started[0], "PATH=/usr/bin:/bin") {
		t.Fatalf("crontab 立即執行：%v %v", err, fr.started)
	}
	if err := r.Stop(cron.Path); err == nil {
		t.Fatal("crontab 不能停止")
	}
}

func TestLaunchdReaderLogRotation(t *testing.T) {
	logDir := t.TempDir()
	cur := filepath.Join(logDir, "launchd.log")
	line := func(ts, code string) string {
		return ts + ".1 (gui/501/com.me.x [1]) <Notice>: exited due to exit(" + code + ")\n"
	}
	_ = os.WriteFile(cur, []byte(line("2026-11-19 09:00:00", "1")), 0o644)
	r := &LaunchdReader{Runner: &fakeRunner{}, UID: 501, LogDir: logDir, Folders: []LaunchdFolder{}}
	r.mu.Lock()
	r.readLog(context.Background())
	r.mu.Unlock()
	// 接著寫一行
	f, _ := os.OpenFile(cur, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(line("2026-11-19 10:00:00", "2"))
	f.Close()
	// 輪替：舊的改名、新檔開始
	_ = os.Rename(cur, filepath.Join(logDir, "launchd.log.1"))
	_ = os.WriteFile(cur, []byte(line("2026-11-19 11:00:00", "3")), 0o644)
	r.mu.Lock()
	r.readLog(context.Background())
	got := r.runs["gui/501/com.me.x"]
	r.mu.Unlock()
	if got.Code == nil || *got.Code != 3 || !got.FinishedAt.Equal(time.Date(2026, 11, 19, 11, 0, 0, 0, time.Local)) {
		t.Fatalf("輪替後要接著讀到最新的：%+v", got)
	}
}

func TestSystemdReaderScanAndActions(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "systemd", "user")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "backup.timer"), []byte("[Unit]\nDescription=每天備份\n[Timer]\nOnCalendar=*-*-* 03:00:00\nPersistent=true\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "backup.service"), []byte("[Service]\nType=oneshot\nExecStart=/home/me/bin/backup.sh\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "orphan.timer"), []byte("[Timer]\nOnUnitActiveSec=1h\n"), 0o644)
	show := "systemctl --user show -p " + systemdShowProps + " backup.timer backup.service orphan.timer orphan.service"
	fr := &fakeRunner{outputs: map[string]string{
		show: "Id=backup.timer\nActiveState=active\nUnitFileState=enabled\nNextElapseUSecRealtime=Sat 2026-11-21 03:00:00 CST\n\n" +
			"Id=backup.service\nActiveState=inactive\nExecMainStartTimestamp=Fri 2026-11-20 03:00:00 CST\nExecMainExitTimestamp=Fri 2026-11-20 03:00:02 CST\nExecMainCode=1\nExecMainStatus=0\n\n" +
			"Id=orphan.timer\nActiveState=inactive\nUnitFileState=disabled\n\nId=orphan.service\nActiveState=inactive\n",
		"crontab -l": "",
		"systemctl --user start --no-block backup.service": "",
		"systemctl --user disable --now backup.timer":      "",
	}}
	r := &SystemdReader{Runner: fr, Home: home, Folders: []string{dir}}
	scan := r.Scan(context.Background())
	if scan.Error != "" || len(scan.Tasks) != 2 {
		t.Fatalf("掃描：%q %+v", scan.Error, scan.Tasks)
	}
	b := scan.Tasks[0]
	if b.Name() != "backup" || b.State != StateReady || b.LastResult != 0 || b.LastRun == nil || b.NextRun == nil ||
		DescribeAll(b.Definition.Triggers) != "每天 03:00" || !strings.Contains(b.XML, "# ---- backup.service ----") {
		t.Fatalf("backup：%+v", b)
	}
	o := scan.Tasks[1]
	if o.State != StateDisabled || !strings.Contains(o.Error, "找不到計時器要啟動的") {
		t.Fatalf("orphan：%+v", o)
	}
	if err := r.Run(b.Path); err != nil || !fr.called("systemctl --user start --no-block backup.service") {
		t.Fatalf("立即執行：%v", err)
	}
	if err := r.SetEnabled(b.Path, false); err != nil || !fr.called("systemctl --user disable --now backup.timer") {
		t.Fatalf("停用：%v", err)
	}
	if err := r.Stop("/etc/passwd"); err == nil {
		t.Fatal("不是計時器")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := LoadStore(dir)
	s.State.Baseline = true
	Put(s.State.Fingerprints, "/A/x.plist", "abc")
	s.State.Ignored = Toggle(s.State.Ignored, "/A/y.plist", true)
	for i := 0; i < 60; i++ {
		s.AddChange(ChangeEntry{Path: "/a/X.plist", Kind: "changed", At: at("2026-11-20 10:00").Add(time.Duration(i) * time.Minute)})
	}
	s.AppendRun(RunRecord{Path: "/A/x.plist", At: at("2026-11-20 09:00"), Code: 1})
	s.Save()
	s2 := LoadStore(dir)
	if !s2.State.Baseline || s2.State.Fingerprints["/A/x.plist"] != "abc" || !ContainsFold(s2.State.Ignored, "/a/y.PLIST") {
		t.Fatalf("state.json：%+v", s2.State)
	}
	if len(s2.State.Changes) != ChangesPerTask {
		t.Fatalf("每個排程留 50 筆變更：%d", len(s2.State.Changes))
	}
	if r := s2.LastRunOf("/a/X.PLIST"); r == nil || r.Code != 1 {
		t.Fatalf("runs.jsonl（不分大小寫）：%+v", r)
	}
	if _, ok := Lookup(s2.State.Fingerprints, "/a/x.plist"); !ok {
		t.Fatal("Lookup 不分大小寫")
	}
}
