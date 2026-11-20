package schedules

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SystemdReader Linux 的排程讀取器：systemd 的使用者計時器（~/.config/systemd/user、/etc/systemd/user 的 *.timer）與目前使用者的 crontab。
// 路徑＝.timer 的完整路徑，名稱＝計時器的名稱。狀態一次以 systemctl --user show 讀（一般權限）。系統工具都經 Runner（測試換成假的）
type SystemdReader struct {
	Runner    Runner
	Home      string
	WorkRoots []string
	// Folders 讀哪些資料夾（預設 ~/.config/systemd/user、/etc/systemd/user）
	Folders []string
	Uptime  func() time.Duration
}

// NewSystemdReader 這台真的 systemd（只在 Linux 上用）
func NewSystemdReader(workRoots []string) *SystemdReader {
	home, _ := os.UserHomeDir()
	return &SystemdReader{Runner: ExecRunner{}, Home: home, WorkRoots: workRoots, Uptime: systemUptime}
}

func (r *SystemdReader) folders() []string {
	if r.Folders != nil {
		return r.Folders
	}
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(r.Home, ".config")
	}
	return []string{filepath.Join(cfg, "systemd", "user"), "/etc/systemd/user"}
}

func (r *SystemdReader) env() Environment {
	e := CurrentEnvironment(r.WorkRoots)
	if r.Home != "" {
		e.Variables["HOME"] = r.Home
	}
	return e
}

var systemdShowProps = "Id,ActiveState,SubState,UnitFileState,LastTriggerUSec,NextElapseUSecRealtime,Result,ExecMainStatus,ExecMainCode,ExecMainStartTimestamp,ExecMainExitTimestamp,MainPID"

type systemdTimerFile struct {
	path, name, service, text, serviceText string
	timer, svc                             UnitFile
}

// Scan 讀一次（背景）
func (r *SystemdReader) Scan(ctx context.Context) Scan {
	started := time.Now()
	env := r.env()
	now := time.Now()
	var files []systemdTimerFile
	seen := map[string]bool{}
	for _, dir := range r.folders() {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.timer"))
		sort.Strings(matches)
		for _, p := range matches {
			name := filepath.Base(p)
			// 同名的以使用者自己的優先（systemd 的覆蓋順序）
			if seen[name] {
				continue
			}
			seen[name] = true
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			f := systemdTimerFile{path: p, name: name, text: string(b), timer: ParseUnitFile(string(b))}
			f.service = SystemdServiceOf(name, f.timer)
			for _, d := range r.folders() {
				if sb, err := os.ReadFile(filepath.Join(d, f.service)); err == nil {
					f.serviceText = string(sb)
					break
				}
			}
			f.svc = ParseUnitFile(f.serviceText)
			files = append(files, f)
		}
	}
	errText := ""
	props := map[string]map[string]string{}
	if len(files) > 0 {
		args := []string{"--user", "show", "-p", systemdShowProps}
		for _, f := range files {
			args = append(args, f.name, f.service)
		}
		out, err := r.Runner.Output("systemctl", args...)
		if err != nil {
			errText = "讀不到 systemd 的狀態：" + err.Error()
		}
		props = ParseSystemctlShow(out)
	}
	boot := now
	if r.Uptime != nil {
		boot = now.Add(-r.Uptime())
	}
	repoCache := map[string]string{}
	var tasks []TaskInfo
	for _, f := range files {
		if ctx.Err() != nil {
			break
		}
		st := ResolveSystemdState(props[f.name], props[f.service])
		anchor := boot
		if t := ParseSystemdTimestamp(props[f.name]["LastTriggerUSec"]); t != nil {
			anchor = *t
		}
		def := SystemdTimerDefinition(f.timer, f.svc, env.UserName, r.Home, anchor)
		def.Enabled = st.Enabled
		if fi, err := os.Stat(f.path); err == nil {
			def.Created = fileBirthTime(fi)
		}
		repo := ""
		for _, p := range CandidatePaths(def.Actions, env) {
			if repo = RepoRootOf(p, repoCache, fileExists); repo != "" {
				break
			}
		}
		note := ""
		switch {
		case f.serviceText == "":
			note = "找不到計時器要啟動的 " + f.service
		case !st.Loaded && st.Enabled:
			note = "systemd 還沒有啟動這個計時器：按「啟用」或執行 systemctl --user enable --now " + f.name
		}
		text := f.text
		if f.serviceText != "" {
			text += "\n# ---- " + f.service + " ----\n" + f.serviceText
		}
		d := def
		tasks = append(tasks, TaskInfo{Path: f.path, XML: text, Definition: &d, Error: note, State: st.State, Enabled: st.Enabled,
			LastRun: st.LastRun, LastResult: st.LastResult, NextRun: st.NextRun, RepoRoot: repo, DisplayName: strings.TrimSuffix(f.name, ".timer")})
	}
	tasks = append(tasks, crontabTasks(r.Runner, env, now, r.Home, repoCache)...)
	sort.SliceStable(tasks, func(i, j int) bool { return strings.ToLower(tasks[i].Path) < strings.ToLower(tasks[j].Path) })
	return Scan{ScannedAt: time.Now(), Tasks: tasks, Error: errText, ElapsedMs: time.Since(started).Milliseconds(), Env: env}
}

// locate .timer 的路徑 → 計時器與服務的名稱
func (r *SystemdReader) locate(path string) (string, string, error) {
	notFound := errors.New("找不到這個排程（可能已被刪除）")
	ok := false
	for _, d := range r.folders() {
		if filepath.Dir(path) == d {
			ok = true
		}
	}
	if !ok || !strings.HasSuffix(path, ".timer") {
		return "", "", notFound
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", notFound
	}
	name := filepath.Base(path)
	return name, SystemdServiceOf(name, ParseUnitFile(string(b))), nil
}

func (r *SystemdReader) systemctl(args ...string) error {
	_, err := r.Runner.Output("systemctl", append([]string{"--user"}, args...)...)
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "access denied") || strings.Contains(msg, "authentication required") || strings.Contains(msg, "permission denied"):
		return errors.New("權限不足（這個排程要系統管理員才能改），請交給代理處理")
	case strings.Contains(msg, "not found") || strings.Contains(msg, "not loaded"):
		return errors.New("systemd 裡沒有這個單元（還沒有載入：先執行 systemctl --user daemon-reload）")
	}
	return err
}

// Run 立即執行（啟動計時器的服務，不等它跑完）
func (r *SystemdReader) Run(path string) error {
	if IsCronPath(path) {
		return runCron(r.Runner, path, r.env().UserName, r.Home)
	}
	_, svc, err := r.locate(path)
	if err != nil {
		return err
	}
	return r.systemctl("start", "--no-block", svc)
}

// Stop 停止執行中的服務
func (r *SystemdReader) Stop(path string) error {
	if IsCronPath(path) {
		return errors.New("crontab 的排程不能從這裡停止，請交給代理處理")
	}
	_, svc, err := r.locate(path)
	if err != nil {
		return err
	}
	return r.systemctl("stop", svc)
}

// SetEnabled 啟用＝enable --now（之後登入也啟動）；停用＝disable --now（停掉計時器，正在跑的那一次不動）
func (r *SystemdReader) SetEnabled(path string, enabled bool) error {
	if IsCronPath(path) {
		verb := "停用"
		if enabled {
			verb = "啟用"
		}
		return errors.New("crontab 的排程不能從這裡" + verb + "（要改 crontab），請交給代理處理")
	}
	timer, _, err := r.locate(path)
	if err != nil {
		return err
	}
	if enabled {
		return r.systemctl("enable", "--now", timer)
	}
	return r.systemctl("disable", "--now", timer)
}

// NewReader 這台電腦的讀取器：macOS 是 launchd＋crontab，Linux 是 systemd 使用者計時器＋crontab；其他系統回 nil（不顯示排程頁）
func NewReader(goos string, workRoots []string) Reader {
	switch goos {
	case "darwin":
		return NewLaunchdReader(workRoots)
	case "linux":
		return NewSystemdReader(workRoots)
	}
	return nil
}
