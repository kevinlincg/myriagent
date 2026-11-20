//go:build !darwin

package schedules

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// fileBirthTime Linux 的 stat 不一定有建立時間：用最後修改時間代替
func fileBirthTime(fi os.FileInfo) *time.Time {
	t := fi.ModTime()
	return &t
}

// systemUptime /proc/uptime
func systemUptime() time.Duration {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	s, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return time.Duration(s * float64(time.Second))
}

// processStartTime 只有 macOS 的讀取器用到
func processStartTime(pid int) *time.Time { return nil }
