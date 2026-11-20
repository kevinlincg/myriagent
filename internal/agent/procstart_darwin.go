package agent

import "golang.org/x/sys/unix"

// launchProcStart：程序的開始時間（微秒，sysctl kern.proc.pid 的 p_starttime）；程序不在回 false。
// 和 PID 一起認一個程序（PID 會被重複使用）
func launchProcStart(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil || kp.Proc.P_pid != int32(pid) {
		return 0, false
	}
	t := kp.Proc.P_starttime
	return int64(t.Sec)*1_000_000 + int64(t.Usec), true
}
