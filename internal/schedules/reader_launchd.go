package schedules

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// LaunchdReader macOS 的排程讀取器（C# 的 MacScheduleReader）：這台的 launchd 工作與目前使用者的 crontab。
//   - launchd：~/Library/LaunchAgents（使用者自己的）、/Library/LaunchAgents、/Library/LaunchDaemons（廠商裝的，要系統管理員才能改）；
//     /System/Library 底下 Apple 自己的不讀。路徑＝.plist 的完整路徑，名稱＝標籤
//   - crontab：crontab -l 的每一行。路徑＝crontab/〈指令的雜湊〉，名稱＝指令
//
// 狀態：launchctl（有沒有載入、在不在跑、結束碼、停用）一般權限就讀得到；上次什麼時候執行讀 launchd 自己的記錄檔
// （只有最近幾小時，每次掃描接著上次讀到的地方往下讀）。系統工具都經 Runner（測試換成假的）
type LaunchdReader struct {
	Runner    Runner
	Home      string
	UID       int
	WorkRoots []string
	// LogDir launchd 的記錄檔所在（預設 /var/log/com.apple.xpc.launchd）
	LogDir string
	// Folders 讀哪些資料夾（預設 ~/Library/LaunchAgents、/Library/LaunchAgents、/Library/LaunchDaemons；測試換成暫存資料夾）
	Folders []LaunchdFolder
	// Uptime 開機到現在多久（「每隔多久」不知道上次開始時從開機起算）
	Uptime func() time.Duration
	// ProcessStart 在跑的程序什麼時候開始（查不到回 nil）
	ProcessStart func(pid int) *time.Time

	mu        sync.Mutex
	runs      map[string]LaunchdRun
	logPrimed bool
	logHead   string
	logOffset int64
}

// LaunchdFolder 一個放 .plist 的資料夾與它的網域
type LaunchdFolder struct {
	Dir    string
	Domain LaunchdDomain
}

const (
	maxPlistBytes = 1 << 20
	maxLogBytes   = 64 << 20
)

// NewLaunchdReader 這台真的 launchd（只在 macOS 上用）
func NewLaunchdReader(workRoots []string) *LaunchdReader {
	home, _ := os.UserHomeDir()
	return &LaunchdReader{Runner: ExecRunner{}, Home: home, UID: os.Getuid(), WorkRoots: workRoots, Uptime: systemUptime, ProcessStart: processStartTime}
}

func (r *LaunchdReader) folders() []LaunchdFolder {
	if r.Folders != nil {
		return r.Folders
	}
	return []LaunchdFolder{
		{filepath.Join(r.Home, "Library", "LaunchAgents"), DomainUserAgent},
		{"/Library/LaunchAgents", DomainGlobalAgent},
		{"/Library/LaunchDaemons", DomainDaemon},
	}
}

func (r *LaunchdReader) logDir() string {
	if r.LogDir != "" {
		return r.LogDir
	}
	return "/var/log/com.apple.xpc.launchd"
}

func (r *LaunchdReader) env() Environment {
	e := CurrentEnvironment(r.WorkRoots)
	if r.Home != "" {
		e.Variables["HOME"] = r.Home
	}
	return e
}

// Scan 讀一次（背景）
func (r *LaunchdReader) Scan(ctx context.Context) Scan {
	started := time.Now()
	env := r.env()
	var tasks []TaskInfo
	errText := ""
	now := time.Now()
	r.mu.Lock()
	r.readLog(ctx)
	seen := map[string]LaunchdRun{}
	for k, v := range r.runs {
		seen[k] = v
	}
	r.mu.Unlock()
	out := func(args ...string) string {
		s, err := r.Runner.Output("/bin/launchctl", args...)
		if err != nil {
			return ""
		}
		return s
	}
	userLoaded := ParseLaunchctlList(out("list"))
	systemLoaded := ParseLaunchctlSystemServices(out("print", "system"))
	userDisabled := ParseLaunchctlDisabled(out("print-disabled", fmt.Sprintf("gui/%d", r.UID)))
	systemDisabled := ParseLaunchctlDisabled(out("print-disabled", "system"))
	boot := now
	if r.Uptime != nil {
		boot = now.Add(-r.Uptime())
	}
	repoCache := map[string]string{}
	for _, f := range r.folders() {
		if ctx.Err() != nil {
			break
		}
		files, err := filepath.Glob(filepath.Join(f.Dir, "*.plist"))
		if err != nil {
			continue
		}
		sort.Strings(files)
		loaded, disabled := userLoaded, userDisabled
		if f.Domain == DomainDaemon {
			loaded, disabled = systemLoaded, systemDisabled
		}
		for _, file := range files {
			tasks = append(tasks, r.readJob(file, f.Domain, env, now, boot, seen, loaded, disabled, repoCache))
		}
	}
	tasks = append(tasks, crontabTasks(r.Runner, env, now, r.Home, repoCache)...)
	sort.SliceStable(tasks, func(i, j int) bool { return strings.ToLower(tasks[i].Path) < strings.ToLower(tasks[j].Path) })
	return Scan{ScannedAt: time.Now(), Tasks: tasks, Error: errText, ElapsedMs: time.Since(started).Milliseconds(), Env: env}
}

func (r *LaunchdReader) readJob(file string, domain LaunchdDomain, env Environment, now, boot time.Time, seen map[string]LaunchdRun,
	loadedJobs map[string]LaunchdLoaded, disabledJobs map[string]bool, repoCache map[string]string) TaskInfo {
	stem := LaunchdStemOf(file)
	text, err := r.readPlistXML(file)
	if err != nil {
		return TaskInfo{Path: file, Error: "讀不到這個工作：" + err.Error(), DisplayName: stem}
	}
	var created *time.Time
	if fi, err := os.Stat(file); err == nil {
		created = fileBirthTime(fi)
	}
	job, err := LaunchdToJob(text, domain, env.UserName, created, &boot)
	if err != nil {
		return TaskInfo{Path: file, XML: text, Error: "定義讀不懂：" + err.Error(), DisplayName: stem}
	}
	label := job.Label
	if label == "" {
		label = stem
	}
	run, hasRun := seen[LaunchdServiceKey(domain, r.UID, label)]
	// 「每隔多久」從上次開始執行的時間起算（記錄檔看得到的話）
	if hasRun && run.LastStart != nil {
		if j, err := LaunchdToJob(text, domain, env.UserName, created, run.LastStart); err == nil {
			job = j
		}
	}
	var loaded *LaunchdLoaded
	if l, ok := loadedJobs[label]; ok {
		loaded = &l
	}
	var disabled *bool
	if d, ok := disabledJobs[label]; ok {
		disabled = &d
	}
	var procStart *time.Time
	if loaded != nil && loaded.Pid != nil && r.ProcessStart != nil {
		procStart = r.ProcessStart(*loaded.Pid)
	}
	var runPtr *LaunchdRun
	if hasRun {
		runPtr = &run
	}
	state := ResolveLaunchdState(job.Definition.Enabled, disabled, loaded, runPtr, procStart, lastWrite(job.StandardOutPath, job.StandardErrorPath))
	var next *time.Time
	if state.Enabled && loaded != nil {
		if up := Occurrences(job.Definition.Triggers, now, now.AddDate(0, 0, 35), 1); len(up) > 0 {
			next = &up[0]
		}
	}
	repo := ""
	for _, p := range CandidatePaths(job.Definition.Actions, env) {
		if repo = RepoRootOf(p, repoCache, fileExists); repo != "" {
			break
		}
	}
	// 檔案在、launchd 卻不知道它：要等下次登入（或開機）才會載入
	note := ""
	if loaded == nil && state.Enabled {
		if domain == DomainDaemon {
			note = "launchd 還沒有載入這個工作：下次開機才會開始（或以 sudo launchctl bootstrap system 載入）"
		} else {
			note = "launchd 還沒有載入這個工作：下次登入才會開始（或按「立即執行」載入）"
		}
	}
	def := job.Definition
	return TaskInfo{Path: file, XML: text, Definition: &def, Error: note, State: state.State, Enabled: state.Enabled, LastRun: state.LastRun,
		LastResult: state.LastResult, NextRun: next, RepoRoot: repo, DisplayName: label}
}

// readPlistXML plist 的內容（XML）；二進位的（bplist 開頭）以 plutil 轉成 XML
func (r *LaunchdReader) readPlistXML(file string) (string, error) {
	fi, err := os.Stat(file)
	if err != nil {
		return "", err
	}
	if fi.Size() > maxPlistBytes {
		return "", errors.New("檔案太大，不像是 launchd 的定義")
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	if bytes.HasPrefix(b, []byte("bplist")) {
		return r.Runner.Output("/usr/bin/plutil", "-convert", "xml1", "-o", "-", file)
	}
	return string(bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})), nil
}

// ---- 操作 ----

// Run 立即執行：crontab 的照 cron 的方式跑；launchd 的沒載入先載入，再 kickstart
func (r *LaunchdReader) Run(path string) error {
	if IsCronPath(path) {
		return runCron(r.Runner, path, r.env().UserName, r.Home)
	}
	t, err := r.locate(path)
	if err != nil {
		return err
	}
	if !r.isLoaded(t.service) {
		if err := r.launchctl(t, "bootstrap", t.domain, path); err != nil {
			return err
		}
	}
	return r.launchctl(t, "kickstart", t.service)
}

// Stop 停止
func (r *LaunchdReader) Stop(path string) error {
	if IsCronPath(path) {
		return errors.New("crontab 的排程不能從這裡停止，請交給代理處理")
	}
	t, err := r.locate(path)
	if err != nil {
		return err
	}
	return r.launchctl(t, "kill", "SIGTERM", t.service)
}

// SetEnabled 停用：卸載（之後不會再觸發；正在跑的那一次會被結束）並記成停用（下次登入也不載入）。啟用：取消停用並載入。
// launchctl disable 自己只影響下次載入，所以要連同卸載、載入一起做
func (r *LaunchdReader) SetEnabled(path string, enabled bool) error {
	if IsCronPath(path) {
		verb := "停用"
		if enabled {
			verb = "啟用"
		}
		return errors.New("crontab 的排程不能從這裡" + verb + "（要改 crontab），請交給代理處理")
	}
	t, err := r.locate(path)
	if err != nil {
		return err
	}
	if enabled {
		if err := r.launchctl(t, "enable", t.service); err != nil {
			return err
		}
		if !r.isLoaded(t.service) {
			return r.launchctl(t, "bootstrap", t.domain, path)
		}
		return nil
	}
	if r.isLoaded(t.service) {
		if err := r.launchctl(t, "bootout", t.service); err != nil {
			return err
		}
	}
	return r.launchctl(t, "disable", t.service)
}

type launchdTarget struct {
	domain, service string
	needsRoot       bool
}

func (r *LaunchdReader) locate(path string) (launchdTarget, error) {
	var domain *LaunchdDomain
	for _, f := range r.folders() {
		if strings.EqualFold(filepath.Dir(path), f.Dir) {
			d := f.Domain
			domain = &d
			break
		}
	}
	notFound := errors.New("找不到這個排程（可能已被刪除）")
	if domain == nil {
		return launchdTarget{}, notFound
	}
	if _, err := os.Stat(path); err != nil {
		return launchdTarget{}, notFound
	}
	label := ""
	if text, err := r.readPlistXML(path); err == nil {
		if job, err := LaunchdToJob(text, *domain, r.env().UserName, nil, nil); err == nil {
			label = job.Label
		}
	}
	if label == "" {
		label = LaunchdStemOf(path)
	}
	where := fmt.Sprintf("gui/%d", r.UID)
	if *domain == DomainDaemon {
		where = "system"
	}
	return launchdTarget{domain: where, service: where + "/" + label, needsRoot: *domain == DomainDaemon}, nil
}

func (r *LaunchdReader) isLoaded(service string) bool {
	_, err := r.Runner.Output("/bin/launchctl", "print", service)
	return err == nil
}

func (r *LaunchdReader) launchctl(t launchdTarget, args ...string) error {
	// 開機載入的工作（/Library/LaunchDaemons）要 root：app 不提高權限
	denied := errors.New("權限不足（這個排程要系統管理員才能改），請交給代理處理")
	if t.needsRoot {
		return denied
	}
	_, err := r.Runner.Output("/bin/launchctl", args...)
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "operation not permitted") || strings.Contains(msg, "not privileged"):
		return denied
	case strings.Contains(msg, "could not find service"):
		return errors.New("launchd 裡沒有這個工作（還沒有載入）")
	case strings.Contains(msg, "disabled"):
		return errors.New("這個排程停用中，請先啟用")
	}
	return err
}

// ---- 記錄檔 ----

// readLog 把 launchd 的記錄檔讀到最新（在鎖裡）：第一次連同輪替出去的兩份一起讀（舊的先），之後接著上次讀到的地方。
// 記錄檔輪替（launchd.log 改名成 launchd.log.1、另開新檔）以「開頭變了」判斷：先把改名的那份剩下的讀完，再從頭讀新的
func (r *LaunchdReader) readLog(ctx context.Context) {
	if r.runs == nil {
		r.runs = map[string]LaunchdRun{}
	}
	dir := r.logDir()
	current := filepath.Join(dir, "launchd.log")
	rotated := filepath.Join(dir, "launchd.log.1")
	if !r.logPrimed {
		r.logPrimed = true
		r.readFrom(ctx, filepath.Join(dir, "launchd.log.2"), 0)
		r.readFrom(ctx, rotated, 0)
		r.logHead = headOf(current)
		r.logOffset = r.readFrom(ctx, current, 0)
		return
	}
	head := headOf(current)
	if head != r.logHead {
		if r.logHead != "" && headOf(rotated) == r.logHead {
			r.readFrom(ctx, rotated, r.logOffset)
		}
		r.logHead = head
		r.logOffset = 0
	}
	r.logOffset = r.readFrom(ctx, current, r.logOffset)
}

// headOf 檔案開頭的一小段（第一行的時間與工作名稱）；沒有檔案、空的是空字串
func headOf(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 96)
	n, _ := io.ReadFull(f, buf)
	return string(buf[:n])
}

// readFrom 從 offset 讀到檔尾，完整的行才算；回傳讀到哪（下一次從那裡接）。只留這個使用者登入後載入的與開機載入的工作
func (r *LaunchdReader) readFrom(ctx context.Context, path string, offset int64) int64 {
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("排程：讀不到 launchd 的記錄檔（上次執行的時間會不知道）：%v", err)
		}
		return 0
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return offset
	}
	if offset > fi.Size() {
		offset = 0
	}
	remaining := fi.Size() - offset
	if remaining > maxLogBytes {
		remaining = maxLogBytes
	}
	if remaining <= 0 {
		return offset
	}
	data := make([]byte, remaining)
	n, _ := f.ReadAt(data, offset)
	data = data[:n]
	userPrefix := fmt.Sprintf("gui/%d/", r.UID)
	start := 0
	for start < len(data) {
		if ctx.Err() != nil {
			break
		}
		nl := bytes.IndexByte(data[start:], '\n')
		if nl < 0 {
			break
		}
		line := data[start : start+nl]
		start += nl + 1
		if !bytes.Contains(line, []byte("exited due to")) && !bytes.Contains(line, []byte("Successfully spawned")) {
			continue
		}
		if e := ParseLaunchdLogLine(string(line)); e != nil && (strings.HasPrefix(e.Service, userPrefix) || strings.HasPrefix(e.Service, "system/")) {
			ApplyLaunchdEvent(r.runs, *e)
		}
	}
	return offset + int64(start)
}
