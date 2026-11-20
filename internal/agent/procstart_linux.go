package agent

import (
	"os"
	"strconv"
	"strings"
)

// launchProcStart：程序的開始時間（/proc/<pid>/stat 第 22 欄，開機後的 clock tick）；程序不在回 false。
// 和 PID 一起認一個程序（PID 會被重複使用）
func launchProcStart(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	// 第二欄（程式名稱）在括號裡、可能含空白：從最後一個右括號之後算
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(s[i+1:])
	// 右括號之後第一欄是第 3 欄（state），第 22 欄是 fields[19]
	if len(fields) < 20 {
		return 0, false
	}
	v, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
