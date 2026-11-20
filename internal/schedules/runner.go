package schedules

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Runner 執行系統工具（launchctl、crontab、systemctl、plutil）；讀取器只經這個介面，測試換成假的，絕不碰真的排程
type Runner interface {
	// Output 執行、回標準輸出；結束碼不是 0 回錯誤（訊息是它的錯誤輸出）
	Output(name string, args ...string) (string, error)
	// StartDetached 在背景啟動、不等它結束（crontab 的「立即執行」）
	StartDetached(name string, args, env []string, dir string) error
}

// ExecRunner 真的執行（10 秒沒回應就砍掉）
type ExecRunner struct{}

const commandTimeout = 10 * time.Second

func (ExecRunner) Output(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("%s 沒有回應", filepath.Base(name))
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			msg := bytes.TrimSpace(stderr.Bytes())
			if len(msg) > 0 {
				return stdout.String(), errors.New(string(msg))
			}
			return stdout.String(), fmt.Errorf("%s 結束碼 %d", filepath.Base(name), exit.ExitCode())
		}
		return "", fmt.Errorf("啟動不了 %s：%v", name, err)
	}
	return stdout.String(), nil
}

func (ExecRunner) StartDetached(name string, args, env []string, dir string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = env
	cmd.Dir = dir
	// 沒有轉向的輸出丟掉（cron 會寄信，這裡不寄）
	devnull, _ := os.Open(os.DevNull)
	cmd.Stdin = devnull
	if err := cmd.Start(); err != nil {
		if devnull != nil {
			devnull.Close()
		}
		return fmt.Errorf("啟動不了 %s：%v", name, err)
	}
	go func() {
		_ = cmd.Wait()
		if devnull != nil {
			devnull.Close()
		}
	}()
	return nil
}

// fileExists 給 RepoRootOf 的檔案系統查詢
func fileExists(p string) (bool, bool) {
	fi, err := os.Stat(p)
	if err != nil {
		return false, false
	}
	return fi.IsDir(), true
}

// lastWrite 這幾個檔案最後寫入的時間（相對路徑、不存在的略過）
func lastWrite(paths ...string) *time.Time {
	var latest *time.Time
	for _, p := range paths {
		if p == "" || p[0] != '/' {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() {
			continue
		}
		t := fi.ModTime()
		if latest == nil || t.After(*latest) {
			latest = &t
		}
	}
	return latest
}

// crontabTasks 目前使用者的 crontab（macOS 與 Linux 共用）：路徑＝crontab/〈指令的雜湊〉，名稱＝指令。
// cron 沒有任何執行紀錄：只知道排了什麼、下次什麼時候，上次執行只能看它轉向的記錄檔最後寫入的時間
func crontabTasks(r Runner, env Environment, now time.Time, home string, repoCache map[string]string) []TaskInfo {
	text, err := r.Output("crontab", "-l")
	if err != nil {
		// 沒有 crontab（結束碼 1）、或系統不讓讀：當作沒有
		return nil
	}
	var out []TaskInfo
	for _, e := range ParseCrontab(text, env.UserName) {
		def := e.Definition
		var next *time.Time
		if up := Occurrences(def.Triggers, now, now.AddDate(0, 0, 35), 1); len(up) > 0 {
			next = &up[0]
		}
		repo := ""
		for _, p := range CandidatePaths(def.Actions, env) {
			if repo = RepoRootOf(p, repoCache, fileExists); repo != "" {
				break
			}
		}
		log := ""
		if target := CronLogPath(e.Command); target != "" {
			if x, ok := Expand(target, env); ok {
				log = x
				if log[0] != '/' {
					log = filepath.Join(home, log)
				}
			}
		}
		d := def
		out = append(out, TaskInfo{Path: CronFolder + "/" + e.Key, XML: e.Line, Definition: &d, State: StateReady, Enabled: true,
			LastRun: lastWrite(log), LastResult: ResultHasNotRun, NextRun: next, RepoRoot: repo, DisplayName: CronDisplayName(e.Command)})
	}
	return out
}

// CronFolder crontab 的排程的路徑前綴
const CronFolder = "crontab"

// IsCronPath crontab 的排程
func IsCronPath(path string) bool {
	return len(path) > len(CronFolder)+1 && path[:len(CronFolder)+1] == CronFolder+"/"
}

// runCron 立即執行 crontab 的一行：照 cron 的方式（/bin/sh -c、在使用者資料夾、很少的環境變數加上 crontab 自己設的），不等它結束
func runCron(r Runner, path, user, home string) error {
	text, err := r.Output("crontab", "-l")
	if err != nil {
		return fmt.Errorf("讀不到 crontab：%v", err)
	}
	for _, e := range ParseCrontab(text, user) {
		if CronFolder+"/"+e.Key != path {
			continue
		}
		env := map[string]string{"HOME": home, "LOGNAME": user, "USER": user, "SHELL": "/bin/sh", "PATH": "/usr/bin:/bin"}
		for k, v := range CronVariables(text) {
			env[k] = v
		}
		var list []string
		for k, v := range env {
			list = append(list, k+"="+v)
		}
		return r.StartDetached("/bin/sh", []string{"-c", e.Command}, sortStrings(list), home)
	}
	return errors.New("找不到這個排程（可能已被刪除）")
}

// CurrentEnvironment 這台電腦的使用者與判斷用的變數（~ 與 $HOME 展開成使用者資料夾）
func CurrentEnvironment(workRoots []string) Environment {
	home, _ := os.UserHomeDir()
	user := os.Getenv("USER")
	if user == "" {
		user = filepath.Base(home)
	}
	host, _ := os.Hostname()
	return Environment{UserName: user, MachineName: host, Variables: map[string]string{"HOME": home, "USER": user, "LOGNAME": user}, WorkRoots: workRoots}
}
