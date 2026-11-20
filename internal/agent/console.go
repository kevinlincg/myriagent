package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// 「開一個終端機視窗執行」（照 C# 的 Cli/TerminalConsoleRules.cs 與 Myriagent.Mac 的 CliRunner.Console.Mac.cs）：登入、登出、安裝這類
// 要使用者在畫面上操作的流程。終端機不是 app 的子程序，所以拆成兩個小腳本放在暫存資料夾：
//   - watch.sh：app 啟動、等待的程序。記下自己的 PID、叫終端機開 run.command，等到 done 檔出現就以它的結束碼結束；
//     run.command 沒寫 done 就不見了（使用者關掉視窗）回 1；終端機一直沒開起來（60 秒）也回 1
//   - run.command：在終端機裡執行。設好環境變數（只寫和 app 不同的：終端機的 shell 有使用者自己的環境；API Key 一律拿掉），
//     執行指令、把結束碼寫進 done。背景盯著 watch.sh：它被結束（登入完成、逾時、取消、移除帳戶）就清掉暫存資料夾、結束整個指令
// macOS 用「終端機」（open -a Terminal）；Linux 依序試 x-terminal-emulator、gnome-terminal、konsole、xterm，都沒有（沒有桌面）就回
// errNoTerminal 與要使用者自己執行的指令。

const (
	consoleWatchFile   = "watch.sh"
	consoleRunFile     = "run.command"
	consoleWatcherPID  = "watcher.pid"
	consoleRunPID      = "run.pid"
	consoleDoneFile    = "done"
	consoleStartTicks  = 300 // 60 秒（每 0.2 秒一次）
	consoleLoginWindow = 10 * time.Minute
)

// errNoTerminal：這台沒有可以開的終端機（Linux 沒有桌面）：呼叫端改成顯示指令、請使用者自己執行
var errNoTerminal = errors.New("找不到可以開啟的終端機")

// consoleProc：開出去的視窗（實際上是 app 這邊等待的程序）
type consoleProc interface {
	// Wait 等它結束，回結束碼；ctx 取消時結束視窗裡的指令並回 ctx 的錯誤
	Wait(ctx context.Context) (int, error)
	Kill()
	// Exited 已經結束了（不等）
	Exited() bool
}

// consoleLauncher：開終端機視窗執行一個指令。env 是完整的環境（已套上帳戶的設定）；
// 回 errNoTerminal 時 manual 是要使用者自己在終端機執行的指令
type consoleLauncher interface {
	Start(path string, args, env []string, title string) (proc consoleProc, manual string, err error)
}

// consoleChanges：設定環境之後和之前差在哪：要設的（新加或改了值）與要拿掉的；名稱排序，sh 不能用的名稱略過。
// alwaysUnset＝不論 app 的環境裡有沒有都要拿掉的（API Key），設定之後有值的不拿
func consoleChanges(before, after map[string]string, alwaysUnset []string) (set [][2]string, unset []string) {
	for k, v := range after {
		if old, ok := before[k]; isShellVarName(k) && (!ok || old != v) {
			set = append(set, [2]string{k, v})
		}
	}
	seen := map[string]bool{}
	for k := range before {
		if _, ok := after[k]; !ok && isShellVarName(k) && !seen[k] {
			seen[k] = true
			unset = append(unset, k)
		}
	}
	for _, k := range alwaysUnset {
		if _, ok := after[k]; !ok && isShellVarName(k) && !seen[k] {
			seen[k] = true
			unset = append(unset, k)
		}
	}
	sort.Slice(set, func(i, j int) bool { return set[i][0] < set[j][0] })
	sort.Strings(unset)
	return set, unset
}

func isShellVarName(s string) bool {
	if s == "" || !(s[0] == '_' || (s[0]|0x20 >= 'a' && s[0]|0x20 <= 'z')) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c == '_' || (c|0x20 >= 'a' && c|0x20 <= 'z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// consoleRunScript：在終端機裡執行的 run.command
func consoleRunScript(folder, title, file string, args []string, set [][2]string, unset []string, workDir string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n# Myriagent 開的視窗：指令結束後這個視窗可以關掉\n")
	b.WriteString("D=" + shellQuote(folder) + "\n")
	b.WriteString(`echo $$ > "$D/` + consoleRunPID + "\"\n")
	if strings.TrimSpace(title) != "" {
		b.WriteString(`printf '\033]0;%s\007' ` + shellQuote(strings.TrimSpace(title)) + "\n")
	}
	b.WriteString(`W=$(cat "$D/` + consoleWatcherPID + `" 2>/dev/null)` + "\n")
	b.WriteString("if [ -n \"$W\" ]; then\n")
	b.WriteString(`  ( while kill -0 "$W" 2>/dev/null; do sleep 1; done; [ -f "$D/` + consoleDoneFile + `" ] || { rm -rf "$D"; kill -TERM 0; } ) &` + "\n")
	b.WriteString("  M=$!\nfi\n")
	for _, kv := range set {
		b.WriteString("export " + kv[0] + "=" + shellQuote(kv[1]) + "\n")
	}
	for _, k := range unset {
		b.WriteString("unset " + k + "\n")
	}
	b.WriteString("cd " + shellQuote(workDir) + " 2>/dev/null\n")
	b.WriteString(shellQuote(file))
	for _, a := range args {
		b.WriteString(" " + shellQuote(a))
	}
	b.WriteString("\nrc=$?\n")
	b.WriteString(`echo "$rc" > "$D/done.tmp" && mv -f "$D/done.tmp" "$D/` + consoleDoneFile + "\"\n")
	b.WriteString(`[ -n "$M" ] && kill "$M" 2>/dev/null` + "\n")
	b.WriteString(`exit "$rc"` + "\n")
	return b.String()
}

// consoleWatchScript：app 啟動、等待的 watch.sh；launch 是叫終端機開 run.command 的那一行（$D 是暫存資料夾）
func consoleWatchScript(folder, launch string) string {
	return "#!/bin/sh\n" +
		"D=" + shellQuote(folder) + "\n" +
		`echo $$ > "$D/` + consoleWatcherPID + "\"\n" +
		launch + ` || { rm -rf "$D"; exit 127; }` + "\n" +
		"n=0\n" +
		"while :; do\n" +
		`  if [ -f "$D/` + consoleDoneFile + `" ]; then rc=$(cat "$D/` + consoleDoneFile + `"); rm -rf "$D"; exit "${rc:-1}"; fi` + "\n" +
		`  if [ -f "$D/` + consoleRunPID + `" ]; then` + "\n" +
		`    p=$(cat "$D/` + consoleRunPID + `")` + "\n" +
		`    if [ -n "$p" ] && ! kill -0 "$p" 2>/dev/null; then` + "\n" +
		`      [ -f "$D/` + consoleDoneFile + `" ] && continue` + "\n" +
		`      rm -rf "$D"; exit 1` + "\n" +
		"    fi\n" +
		"  else\n" +
		`    n=$((n+1)); if [ "$n" -gt ` + itoa(consoleStartTicks) + ` ]; then rm -rf "$D"; exit 1; fi` + "\n" +
		"  fi\n" +
		"  sleep 0.2\n" +
		"done\n"
}

// terminalLaunchLine：叫終端機開 run.command 的那一行；找不到終端機回空字串
func terminalLaunchLine(goos string, lookPath func(string) (string, error)) string {
	if goos == "darwin" {
		return `/usr/bin/open -a Terminal "$D/` + consoleRunFile + `"`
	}
	run := `/bin/sh "$D/` + consoleRunFile + `"`
	candidates := []struct{ name, flag string }{
		{"x-terminal-emulator", "-e"}, {"gnome-terminal", "--"}, {"konsole", "-e"}, {"xterm", "-e"},
	}
	for _, c := range candidates {
		if p, err := lookPath(c.name); err == nil {
			// 有些終端機（xterm）開著視窗就不返回：放到背景，開不起來由 watch.sh 的 60 秒期限收尾
			return "( " + shellQuote(p) + " " + c.flag + " " + run + " >/dev/null 2>&1 & )"
		}
	}
	return ""
}

// manualCommand：沒有終端機時要使用者自己執行的指令（只列和 app 不同的環境變數；值以單引號包起來）
func manualCommand(file string, args []string, set [][2]string) string {
	var parts []string
	for _, kv := range set {
		if kv[0] == "CLAUDE_CONFIG_DIR" || kv[0] == "CODEX_HOME" || kv[0] == "GROK_HOME" || kv[0] == "HOME" {
			parts = append(parts, kv[0]+"="+shellQuote(kv[1]))
		}
	}
	parts = append(parts, shellQuote(file))
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// systemConsole：真的開終端機視窗
type systemConsole struct{}

func (systemConsole) Start(path string, args, env []string, title string) (consoleProc, string, error) {
	before := envMap(os.Environ())
	after := envMap(env)
	set, unset := consoleChanges(before, after, credentialVariables())
	launch := terminalLaunchLine(runtime.GOOS, exec.LookPath)
	if launch == "" || (runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "") {
		return nil, manualCommand(path, args, set), errNoTerminal
	}
	folder := filepath.Join(os.TempDir(), "myriagent-console", newID(""))
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return nil, "", err
	}
	home, _ := os.UserHomeDir()
	if err := os.WriteFile(filepath.Join(folder, consoleRunFile), []byte(consoleRunScript(folder, title, path, args, set, unset, home)), 0o700); err != nil {
		return nil, "", err
	}
	watch := filepath.Join(folder, consoleWatchFile)
	if err := os.WriteFile(watch, []byte(consoleWatchScript(folder, launch)), 0o700); err != nil {
		return nil, "", err
	}
	// 等待的程序本身不需要帳戶的環境（指令在終端機裡跑，環境寫在 run.command）
	cmd := exec.Command("/bin/sh", watch)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("無法開啟%s視窗：%v", orDefault(title, "終端機"), err)
	}
	return newProcWaiter(cmd), "", nil
}

// noConsole：測試預設：不開任何視窗
type noConsole struct{}

func (noConsole) Start(path string, args, env []string, title string) (consoleProc, string, error) {
	return nil, manualCommand(path, args, nil), errNoTerminal
}

// directConsole：不開視窗、直接在背景執行（測試用；行為和終端機裡的指令相同：等它結束、結束碼就是指令的）
type directConsole struct{}

func (directConsole) Start(path string, args, env []string, title string) (consoleProc, string, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	return newProcWaiter(cmd), "", nil
}

// procWaiter：等一個子程序結束（可以等很多次）
type procWaiter struct {
	cmd  *exec.Cmd
	done chan struct{}
	once sync.Once
	code int
}

func newProcWaiter(cmd *exec.Cmd) *procWaiter {
	p := &procWaiter{cmd: cmd, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee):
			p.code = ee.ExitCode()
		case err != nil:
			p.code = -1
		}
		close(p.done)
	}()
	return p
}

func (p *procWaiter) Wait(ctx context.Context) (int, error) {
	select {
	case <-p.done:
		return p.code, nil
	case <-ctx.Done():
		p.Kill()
		return -1, ctx.Err()
	}
}

func (p *procWaiter) Kill() {
	p.once.Do(func() {
		killGroup(p.cmd)
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
		}
	})
}

func (p *procWaiter) Exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}
