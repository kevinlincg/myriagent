//go:build darwin

package schedules

import (
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// fileBirthTime 檔案的建立時間（plist 自己沒有記；排程頁的「建立」）
func fileBirthTime(fi os.FileInfo) *time.Time {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		t := time.Unix(st.Birthtimespec.Sec, st.Birthtimespec.Nsec)
		return &t
	}
	return nil
}

// systemUptime 開機到現在多久（kern.boottime）
func systemUptime() time.Duration {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return 0
	}
	return time.Since(time.Unix(tv.Sec, int64(tv.Usec)*1000))
}

// processStartTime 程序開始的時間（kern.proc.pid）
func processStartTime(pid int) *time.Time {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp.Proc.P_pid != int32(pid) {
		return nil
	}
	tv := kp.Proc.P_starttime
	t := time.Unix(tv.Sec, int64(tv.Usec)*1000)
	return &t
}
