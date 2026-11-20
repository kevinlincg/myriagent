//go:build darwin || linux

package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// 執行列開程序（照 C# 的 src/Myriagent.Mac/Launch/LaunchProcess.Mac.cs）：沒有 Windows 的工作物件，「一個程式連同它開的子程序」以程序群組代表，
// 對群組送訊號＝停止（SIGKILL）、暫停（SIGSTOP）、繼續（SIGCONT）。
//   - 建置與 Debug 模式：看門的程序（myriagent 自己，環境變數 MYRIAGENT_LAUNCH_WATCH=1 時在 init 裡改當看門的；C# 版是一支 perl 腳本）
//     自成一個程序群組、開出要執行的程式，然後等兩件事：程式結束（傳回結束碼、順手結束它留下的子程序），或標準輸入的管線斷了
//     （agent 結束或當掉：結束整個群組）。
//   - Release 模式：換一個工作階段（setsid，和 agent 脫鉤，自己也成了程序群組的頭），輸出以附加的方式寫進輸出檔，agent 跟著讀；
//     agent 重新啟動後以 PID 加開始時間接回。

// launchProcess：執行列開的一個程序（連同它開出的子程序）
type launchProcess interface {
	// Done 在主程序結束（輸出也讀完）時關閉，之後 Code 是結束碼。Release 模式的程式 Close 之後不會關閉（程式照樣執行）
	Done() <-chan struct{}
	Code() int
	// Kill 結束整個程序樹（暫停中的也一樣）
	Kill()
	// Suspend 暫停整個程序樹；做不到（已經結束）回 false
	Suspend() bool
	Resume() bool
	// Processes：現在屬於它、還在執行的程序（記進 launch/runs.json，agent 重新啟動後接回）。建置與 Debug 模式一律是空的
	Processes() []launchProcRef
	// Close：建置與 Debug 模式＝關掉管線（還在執行的話看門的結束整個群組）；Release 模式＝不再跟著讀，程式照樣執行
	Close()
}

// launchStarter：開程序（中樞持有 h.mu 時呼叫，要很快返回；測試換成假的）。輸出一行一行交給 onLine（別的 goroutine）
type launchStarter interface {
	Build(cmd launchCommand, onLine func(string)) (launchProcess, error)
	// Run：Release 模式，和 agent 脫鉤，輸出寫進 logPath、這裡跟著讀
	Run(cmd launchCommand, logPath string, onLine func(string)) (launchProcess, error)
	// RunAttached：Debug 模式，agent 的子程序，agent 結束時一起結束
	RunAttached(cmd launchCommand, onLine func(string)) (launchProcess, error)
	// Reattach：接回 agent 重新啟動前開的程式（主程序還在、開始時間也對得上才接）；不在了回 nil。輸出檔從頭讀起
	Reattach(procs []launchProcRef, logPath string, onLine func(string)) launchProcess
}

const launchWatchEnv = "MYRIAGENT_LAUNCH_WATCH"

// 看門的程序：myriagent（或測試執行檔）帶著 MYRIAGENT_LAUNCH_WATCH=1 啟動時，不做原本的事，改當看門的
func init() {
	if os.Getenv(launchWatchEnv) == "1" {
		os.Exit(runLaunchWatch(os.Args[1:]))
	}
}

// runLaunchWatch：args[0] 是要執行的程式（完整路徑）。程式結束時把結束碼傳回去（被訊號結束是 128＋訊號）、對群組送 SIGTERM 結束剩下的子程序；
// 標準輸入讀到結尾（agent 結束、當掉或 Close）時對整個群組送 SIGKILL、以 137 結束。找不到程式是 127
func runLaunchWatch(args []string) int {
	_ = os.Unsetenv(launchWatchEnv)
	if len(args) == 0 {
		return 126
	}
	signal.Ignore(syscall.SIGTERM)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr // 程式自己的標準輸入是 /dev/null（等輸入的程式讀到檔案結尾，不會卡住）
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", args[0], err)
		return 127
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := os.Stdin.Read(buf); err != nil {
				_ = unix.Kill(-unix.Getpgrp(), unix.SIGKILL)
				os.Exit(137)
			}
		}
	}()
	_ = cmd.Wait()
	_ = unix.Kill(-unix.Getpgrp(), unix.SIGTERM)
	return launchExitCode(cmd.ProcessState)
}

func launchExitCode(st *os.ProcessState) int {
	if st == nil {
		return -1
	}
	if ws, ok := st.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return st.ExitCode()
}

// launchProcExists：程序還在（送 0 號訊號；EPERM＝在，只是不能對它送訊號）
func launchProcExists(pid int) bool {
	if pid <= 1 {
		return false
	}
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

func launchSignalGroup(pid int, sig unix.Signal) bool {
	return pid > 1 && unix.Kill(-pid, sig) == nil
}

func launchSignal(pid int, sig unix.Signal) bool {
	return pid > 1 && unix.Kill(pid, sig) == nil
}

func launchFileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// launchResolve：要執行的程式的完整路徑；找不到回給使用者看的錯誤
func launchResolve(cmd launchCommand) (string, error) {
	p := launchResolveExecutable(cmd.File, os.Getenv("PATH"), launchFileExists)
	if p == "" {
		return "", fmt.Errorf("找不到 %s（不在 PATH 上）", cmd.File)
	}
	return p, nil
}

// launchEnv：dotnet 的第一次使用說明與更新提示不要混進輸出
func launchEnv() []string {
	env := []string{}
	for _, kv := range os.Environ() {
		if len(kv) > len(launchWatchEnv) && kv[:len(launchWatchEnv)+1] == launchWatchEnv+"=" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "DOTNET_NOLOGO=1")
}

type unixLaunchStarter struct{}

func (unixLaunchStarter) Build(cmd launchCommand, onLine func(string)) (launchProcess, error) {
	return startAttachedLaunch(cmd, onLine)
}

func (unixLaunchStarter) RunAttached(cmd launchCommand, onLine func(string)) (launchProcess, error) {
	return startAttachedLaunch(cmd, onLine)
}

func (unixLaunchStarter) Run(cmd launchCommand, logPath string, onLine func(string)) (launchProcess, error) {
	return startDetachedLaunch(cmd, logPath, onLine)
}

func (unixLaunchStarter) Reattach(procs []launchProcRef, logPath string, onLine func(string)) launchProcess {
	if len(procs) == 0 || !launchRefAlive(procs[0]) {
		return nil
	}
	return newDetachedLaunch(procs[0], nil, logPath, onLine)
}

// ───── 建置與 Debug 模式 ─────

type attachedLaunch struct {
	cmd    *exec.Cmd
	pid    int
	stdin  *os.File
	done   chan struct{}
	code   int
	closed sync.Once
}

func startAttachedLaunch(lc launchCommand, onLine func(string)) (launchProcess, error) {
	exe, err := launchResolve(lc)
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("找不到 myriagent 自己的執行檔（%v）", err)
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, err
	}
	c := exec.Command(self, append([]string{exe}, lc.Args...)...)
	c.Dir = lc.Dir
	c.Env = append(launchEnv(), launchWatchEnv+"=1")
	c.Stdin, c.Stdout, c.Stderr = inR, outW, errW
	// 看門的一開始就自成一個群組（不會有「剛啟動、還沒成群組」的空檔）
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	startErr := c.Start()
	inR.Close()
	outW.Close()
	errW.Close()
	if startErr != nil {
		inW.Close()
		outR.Close()
		errR.Close()
		return nil, fmt.Errorf("找不到或無法執行 %s（%v）", lc.File, startErr)
	}
	p := &attachedLaunch{cmd: c, pid: c.Process.Pid, stdin: inW, done: make(chan struct{})}
	// 標準輸入的管線留著不關：它斷掉（agent 結束或當掉）就是看門的結束整個群組的信號
	pumps := make(chan struct{}, 2)
	for _, r := range []*os.File{outR, errR} {
		go func(r *os.File) {
			launchPump(r, &launchLineSplitter{onLine: onLine})
			r.Close()
			pumps <- struct{}{}
		}(r)
	}
	go func() {
		_ = c.Wait()
		// 殘留的子程序可能還握著管線：最多再等 2 秒讀完
		timeout := time.After(launchOutputDrainTimeout)
		for i := 0; i < 2; i++ {
			select {
			case <-pumps:
			case <-timeout:
				i = 2
			}
		}
		p.code = launchExitCode(c.ProcessState)
		close(p.done)
	}()
	return p, nil
}

func launchPump(r io.Reader, s *launchLineSplitter) {
	buf := make([]byte, 8192)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s.feed(buf[:n])
		}
		if err != nil {
			break
		}
	}
	s.flush()
}

func (p *attachedLaunch) Done() <-chan struct{} { return p.done }
func (p *attachedLaunch) Code() int             { return p.code }

func (p *attachedLaunch) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *attachedLaunch) Kill() {
	if p.exited() {
		return
	}
	// 暫停中的程序也能直接結束（SIGKILL 不能被擋下或延後）
	if !launchSignalGroup(p.pid, unix.SIGKILL) {
		_ = p.cmd.Process.Kill()
	}
}

func (p *attachedLaunch) Suspend() bool {
	if p.exited() || !launchSignalGroup(p.pid, unix.SIGSTOP) {
		return false
	}
	// 看門的要醒著：agent 在暫停期間結束時，它才能結束整個群組
	launchSignal(p.pid, unix.SIGCONT)
	return true
}

func (p *attachedLaunch) Resume() bool {
	return !p.exited() && launchSignalGroup(p.pid, unix.SIGCONT)
}

func (p *attachedLaunch) Processes() []launchProcRef { return nil }

func (p *attachedLaunch) Close() {
	// 關掉管線：還在執行的話看門的會結束整個群組
	p.closed.Do(func() { _ = p.stdin.Close() })
}

// ───── Release 模式（和 agent 脫鉤）─────

type detachedLaunch struct {
	root     launchProcRef
	waitDone chan struct{} // 自己開的：Wait 回來了（nil＝接回的，不是 agent 的子程序）
	waitCode int
	logPath  string
	done     chan struct{}
	code     int
	stop     chan struct{}
	stopOnce sync.Once
	position int64
}

func startDetachedLaunch(lc launchCommand, logPath string, onLine func(string)) (launchProcess, error) {
	exe, err := launchResolve(lc)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, fmt.Errorf("開不了輸出檔 %s（%v）", logPath, err)
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("開不了輸出檔 %s（%v）", logPath, err)
	}
	c := exec.Command(exe, lc.Args...)
	c.Dir = lc.Dir
	c.Env = launchEnv()
	c.Stdout, c.Stderr = f, f // stdin 是 /dev/null
	// 換一個工作階段：和 agent 脫鉤（agent 結束、當掉、終端機的 Ctrl+C 都不影響它），自己也成了程序群組的頭
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	startErr := c.Start()
	f.Close()
	if startErr != nil {
		return nil, fmt.Errorf("找不到或無法執行 %s（%v）", lc.File, startErr)
	}
	created, _ := launchProcStart(c.Process.Pid)
	waitDone := make(chan struct{})
	p := newDetachedLaunch(launchProcRef{Pid: c.Process.Pid, Created: created}, waitDone, logPath, onLine)
	go func() {
		_ = c.Wait()
		p.waitCode = launchExitCode(c.ProcessState)
		close(waitDone)
	}()
	return p, nil
}

func newDetachedLaunch(root launchProcRef, waitDone chan struct{}, logPath string, onLine func(string)) *detachedLaunch {
	p := &detachedLaunch{root: root, waitDone: waitDone, logPath: logPath, done: make(chan struct{}), stop: make(chan struct{})}
	go p.follow(&launchLineSplitter{onLine: onLine})
	return p
}

// launchRefAlive：PID 還在、開始時間也一樣（PID 會被重複使用）
func launchRefAlive(r launchProcRef) bool {
	if !launchProcExists(r.Pid) {
		return false
	}
	started, ok := launchProcStart(r.Pid)
	return ok && (r.Created == 0 || started == r.Created)
}

func (p *detachedLaunch) rootExited() (bool, int) {
	if p.waitDone != nil {
		select {
		case <-p.waitDone:
			return true, p.waitCode
		default:
			return false, 0
		}
	}
	// 接回的不是 agent 的子程序，拿不到結束碼（回 0）
	return !launchRefAlive(p.root), 0
}

// follow：跟著讀輸出檔，直到主程序結束（再讀一次把剩下的讀完）或不再跟（Close）
func (p *detachedLaunch) follow(s *launchLineSplitter) {
	t := time.NewTicker(launchLogPollInterval)
	defer t.Stop()
	for {
		done, code := p.rootExited()
		p.readLog(s)
		if done {
			s.flush()
			// 主程序結束了：同一個群組剩下的子程序一起結束
			launchSignalGroup(p.root.Pid, unix.SIGKILL)
			p.code = code
			close(p.done)
			return
		}
		select {
		case <-p.stop:
			return
		case <-t.C:
		}
	}
}

// readLog：讀輸出檔新增的部分；太大時讀完就清空（程式以附加的方式寫，清空後接著寫在開頭）
func (p *detachedLaunch) readLog(s *launchLineSplitter) {
	if p.logPath == "" {
		return
	}
	f, err := os.OpenFile(p.logPath, os.O_RDWR, 0)
	if err != nil {
		return // 檔案暫時讀不到（被刪、權限）：下一次再試
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return
	}
	size := fi.Size()
	if size < p.position {
		p.position = 0
	}
	if size == p.position {
		return
	}
	if _, err := f.Seek(p.position, io.SeekStart); err != nil {
		return
	}
	buf := make([]byte, 65536)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			s.feed(buf[:n])
			p.position += int64(n)
		}
		if err != nil {
			break
		}
	}
	if p.position > launchMaxLogFileBytes {
		if fi, err := f.Stat(); err == nil && fi.Size() == p.position {
			_ = f.Truncate(0)
			p.position = 0
		}
	}
}

func (p *detachedLaunch) Done() <-chan struct{} { return p.done }
func (p *detachedLaunch) Code() int             { return p.code }

func (p *detachedLaunch) Kill() {
	// 暫停中的程序也能直接結束；群組不在了（主程序已經結束）就只對主程序送
	if !launchSignalGroup(p.root.Pid, unix.SIGKILL) && launchRefAlive(p.root) {
		launchSignal(p.root.Pid, unix.SIGKILL)
	}
}

func (p *detachedLaunch) Suspend() bool {
	return launchRefAlive(p.root) && launchSignalGroup(p.root.Pid, unix.SIGSTOP)
}

func (p *detachedLaunch) Resume() bool {
	return launchRefAlive(p.root) && launchSignalGroup(p.root.Pid, unix.SIGCONT)
}

func (p *detachedLaunch) Processes() []launchProcRef {
	if done, _ := p.rootExited(); done || !launchRefAlive(p.root) {
		return nil
	}
	return []launchProcRef{p.root}
}

func (p *detachedLaunch) Close() { p.stopOnce.Do(func() { close(p.stop) }) }
