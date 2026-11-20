//go:build darwin

package procs

import (
	"bytes"
	"encoding/binary"
	"os"

	"github.com/kevinlincg/myriagent/internal/darwinlibc"
	"golang.org/x/sys/unix"
)

// macOS 的程序狀態 SZOMB（<sys/proc.h>）
const szomb = 5

// PROC_PIDVNODEPATHINFO 與 proc_vnodepathinfo 的大小、工作目錄路徑的位移：C# 版 2026-11-19 在 macOS 26.5 以 C 實測
const (
	vnodePathInfoFlavor = 9
	vnodePathInfoSize   = 2352
	cwdOffset           = 152
	maxPath             = 1024
)

func startOf(p *unix.KinfoProc) int64 {
	return int64(p.Proc.P_starttime.Sec)*1_000_000 + int64(p.Proc.P_starttime.Usec)
}

func cstring(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// List：這個使用者的所有程序（一次 sysctl）
func List() ([]Proc, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	if err != nil {
		return nil, err
	}
	out := make([]Proc, 0, len(kps))
	for i := range kps {
		kp := &kps[i]
		if kp.Proc.P_stat == szomb {
			continue
		}
		out = append(out, Proc{Pid: int(kp.Proc.P_pid), Ppid: int(kp.Eproc.Ppid), Start: startOf(kp), Name: cstring(kp.Proc.P_comm[:])})
	}
	return out, nil
}

// StartTime：程序的建立時間（Unix 微秒）；不在了回 false
func StartTime(pid int) (int64, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil || int(kp.Proc.P_pid) != pid {
		return 0, false
	}
	return startOf(kp), true
}

func zombie(pid int) bool {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	return err == nil && kp != nil && kp.Proc.P_stat == szomb
}

// Inspect 讀命令列與環境變數（sysctl 的 KERN_PROCARGS2）；別的使用者的、已經結束的回 false。
// macOS 不讓人讀系統內建程式（/bin/sleep、/bin/bash…）的環境變數（C# 版 2026-11-19 實測：讀到的是空的），那時 EnvHidden 是 true
func Inspect(pid int) (Info, bool) {
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil || len(data) < 4 {
		return Info{}, false
	}
	args, env := ParseProcArgs2(data)
	return Info{Args: args, Env: env, EnvHidden: len(env) == 0}, true
}

// ParseProcArgs2（純函式）：KERN_PROCARGS2 的內容 → 參數清單與環境變數。
// 格式：參數個數（int32）、執行檔路徑、一個以上的 0、每個參數各以 0 結尾、接著每個環境變數各以 0 結尾
func ParseProcArgs2(data []byte) (args []string, env []string) {
	if len(data) < 4 {
		return nil, nil
	}
	count := int(int32(binary.LittleEndian.Uint32(data[:4])))
	pos := 4
	for pos < len(data) && data[pos] != 0 {
		pos++
	}
	for pos < len(data) && data[pos] == 0 {
		pos++
	}
	next := func() (string, bool) {
		if pos >= len(data) {
			return "", false
		}
		end := pos
		for end < len(data) && data[end] != 0 {
			end++
		}
		s := string(data[pos:end])
		pos = end + 1
		return s, true
	}
	for len(args) < count {
		s, ok := next()
		if !ok {
			break
		}
		args = append(args, s)
	}
	for {
		s, ok := next()
		if !ok || s == "" {
			break
		}
		env = append(env, s)
	}
	return args, env
}

// ExePath：執行檔的完整路徑；讀不到回空字串
func ExePath(pid int) string { return darwinlibc.ProcPidpath(pid) }

// Cwd：程序目前的工作目錄；讀不到回空字串
func Cwd(pid int) string {
	buf := make([]byte, vnodePathInfoSize)
	if darwinlibc.ProcPidinfo(pid, vnodePathInfoFlavor, 0, buf) != vnodePathInfoSize {
		return ""
	}
	return cstring(buf[cwdOffset : cwdOffset+maxPath])
}

// Responsible：系統記的「負責的程序」（這個 app 開出的程序脫離父程序之後也不會變）；不支援、讀不到回 -1
func Responsible(pid int) int { return darwinlibc.Responsible(pid) }

// ReadUsage：累計用量（proc_pid_rusage）；讀不到回 false
func ReadUsage(pid int) (Usage, bool) {
	u, ok := darwinlibc.ProcPidRusage(pid)
	if !ok {
		return Usage{}, false
	}
	return Usage{CPUNs: u.UserNs + u.SystemNs, Mem: u.PhysFootprint, Disk: u.DiskRead + u.DiskWrit}, true
}

// IsGUIPath：執行檔在 .app 裡（視窗程式）
func IsGUIPath(path string) bool { return bytes.Contains([]byte(path), []byte(".app/Contents/MacOS/")) }
