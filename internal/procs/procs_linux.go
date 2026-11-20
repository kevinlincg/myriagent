//go:build linux

package procs

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// Linux 的 USER_HZ（/proc 的 tick）：實務上一律 100（sysconf(_SC_CLK_TCK) 要 cgo 才讀得到）
const clkTck = 100

var (
	bootOnce sync.Once
	bootSec  int64
)

func bootTime() int64 {
	bootOnce.Do(func() {
		b, err := os.ReadFile("/proc/stat")
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "btime ") {
				bootSec, _ = strconv.ParseInt(strings.TrimSpace(line[6:]), 10, 64)
			}
		}
	})
	return bootSec
}

// statFields：/proc/<pid>/stat 的名稱與右括號之後的欄位（rest[0] 是狀態、rest[1] 父 PID、rest[11]/[12] utime/stime、rest[19] starttime）
func statFields(pid int) (name string, rest []string, ok bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", nil, false
	}
	return ParseStat(string(b))
}

// ParseStat（純函式）：/proc/<pid>/stat 的一行 → 名稱與右括號之後的欄位（名稱裡可能有空白與括號，以最後一個右括號為界）
func ParseStat(s string) (name string, rest []string, ok bool) {
	open := strings.IndexByte(s, '(')
	close := strings.LastIndexByte(s, ')')
	if open < 0 || close < open {
		return "", nil, false
	}
	rest = strings.Fields(s[close+1:])
	if len(rest) < 20 {
		return "", nil, false
	}
	return s[open+1 : close], rest, true
}

func startFrom(rest []string) int64 {
	ticks, _ := strconv.ParseInt(rest[19], 10, 64)
	return bootTime()*1_000_000 + ticks*1_000_000/clkTck
}

// List：這個使用者的所有程序（讀 /proc）
func List() ([]Proc, error) {
	dirs, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	uid := uint32(os.Getuid())
	out := make([]Proc, 0, len(dirs))
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		var st syscall.Stat_t
		if syscall.Stat("/proc/"+d.Name(), &st) != nil || st.Uid != uid {
			continue
		}
		name, rest, ok := statFields(pid)
		if !ok || rest[0] == "Z" {
			continue
		}
		ppid, _ := strconv.Atoi(rest[1])
		out = append(out, Proc{Pid: pid, Ppid: ppid, Start: startFrom(rest), Name: name})
	}
	return out, nil
}

// StartTime：程序的建立時間（Unix 微秒）；不在了回 false
func StartTime(pid int) (int64, bool) {
	_, rest, ok := statFields(pid)
	if !ok {
		return 0, false
	}
	return startFrom(rest), true
}

func zombie(pid int) bool {
	_, rest, ok := statFields(pid)
	return ok && rest[0] == "Z"
}

func splitNul(b []byte) []string {
	var out []string
	for _, s := range strings.Split(string(b), "\x00") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Inspect 讀命令列與環境變數（/proc/<pid>/cmdline、environ）；讀不到（別的使用者、已經結束）回 false
func Inspect(pid int) (Info, bool) {
	dir := "/proc/" + strconv.Itoa(pid)
	cmd, err := os.ReadFile(dir + "/cmdline")
	if err != nil {
		return Info{}, false
	}
	env, err := os.ReadFile(dir + "/environ")
	if err != nil {
		return Info{}, false
	}
	return Info{Args: splitNul(cmd), Env: splitNul(env)}, true
}

// ExePath：執行檔的完整路徑；讀不到回空字串
func ExePath(pid int) string {
	p, _ := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	return strings.TrimSuffix(p, " (deleted)")
}

// Cwd：程序目前的工作目錄；讀不到回空字串
func Cwd(pid int) string {
	p, _ := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	return p
}

// Responsible：Linux 沒有「負責的程序」：一律 -1
func Responsible(pid int) int { return -1 }

// ReadUsage：累計用量（/proc/<pid>/stat、status、io）；程序不在了回 false。io 讀不到（權限）時 Disk 是 0
func ReadUsage(pid int) (Usage, bool) {
	_, rest, ok := statFields(pid)
	if !ok {
		return Usage{}, false
	}
	ut, _ := strconv.ParseUint(rest[11], 10, 64)
	stt, _ := strconv.ParseUint(rest[12], 10, 64)
	u := Usage{CPUNs: (ut + stt) * (1_000_000_000 / clkTck)}
	dir := "/proc/" + strconv.Itoa(pid)
	if b, err := os.ReadFile(dir + "/status"); err == nil {
		u.Mem = statusKB(string(b), "RssAnon:")
		if u.Mem == 0 {
			u.Mem = statusKB(string(b), "VmRSS:")
		}
	}
	if b, err := os.ReadFile(dir + "/io"); err == nil {
		u.Disk = fieldValue(string(b), "read_bytes:") + fieldValue(string(b), "write_bytes:")
	}
	return u, true
}

// statusKB：status 裡「Key:   123 kB」這種一行的值，換成位元組
func statusKB(s, key string) uint64 { return fieldValue(s, key) * 1024 }

// fieldValue：「Key: 123」這種一行的數字（沒有這一行是 0）
func fieldValue(s, key string) uint64 {
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, key) {
			continue
		}
		f := strings.Fields(line[len(key):])
		if len(f) == 0 {
			return 0
		}
		v, _ := strconv.ParseUint(f[0], 10, 64)
		return v
	}
	return 0
}

// IsGUIPath：Linux 看不出執行檔是不是視窗程式：一律 false（當成主控台程式）
func IsGUIPath(path string) bool { return false }
