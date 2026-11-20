//go:build linux

package sysstat

import (
	"bufio"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// readCPUTimes：/proc/stat 的 cpu 一行（user nice system idle iowait irq softirq steal）：idle＋iowait 是閒置、
// Kernel＝system＋irq＋softirq＋steal＋閒置（同 Windows 的定義）、User＝user＋nice
func readCPUTimes() (CPUTimes, bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return CPUTimes{}, false
	}
	return ParseProcStatCPU(string(b))
}

// ParseProcStatCPU（純函式）：/proc/stat → 累計時間
func ParseProcStatCPU(s string) (CPUTimes, bool) {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || f[0] != "cpu" {
			continue
		}
		v := make([]uint64, 8)
		for i := 1; i < len(f) && i <= 8; i++ {
			v[i-1], _ = strconv.ParseUint(f[i], 10, 64)
		}
		idle := v[3] + v[4]
		return CPUTimes{Idle: idle, Kernel: v[2] + v[5] + v[6] + v[7] + idle, User: v[0] + v[1]}, true
	}
	return CPUTimes{}, false
}

// readMemory：MemTotal − MemAvailable
func readMemory() (used, total int64, ok bool) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	return ParseMeminfo(string(b))
}

// ParseMeminfo（純函式）：/proc/meminfo → 已用、總量（位元組）
func ParseMeminfo(s string) (used, total int64, ok bool) {
	var avail int64 = -1
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			avail = v * 1024
		}
	}
	if total <= 0 || avail < 0 {
		return 0, 0, false
	}
	return max(0, total-avail), total, true
}

func readCPUName() string {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func readUptime() (int64, bool) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	return int64(v), err == nil
}

// readDisks：開機磁碟「/」加上掛在 /media、/mnt、/run/media 底下的實體磁碟（裝置在 /dev 底下、至少 1 GB）
func readDisks() []Disk {
	var disks []Disk
	add := func(mount, label string, minBytes int64) {
		var st syscall.Statfs_t
		if syscall.Statfs(mount, &st) != nil {
			return
		}
		bs := int64(st.Bsize)
		disks = addDisk(disks, label, int64(st.Blocks)*bs, int64(st.Bfree)*bs, minBytes)
	}
	add("/", "/", 0)
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return disks
	}
	defer f.Close()
	var extra []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Fields(sc.Text())
		if len(p) < 3 || !strings.HasPrefix(p[0], "/dev/") || strings.HasPrefix(p[0], "/dev/loop") {
			continue
		}
		m := strings.ReplaceAll(p[1], `\040`, " ")
		if strings.HasPrefix(m, "/media/") || strings.HasPrefix(m, "/mnt/") || strings.HasPrefix(m, "/run/media/") {
			extra = append(extra, m)
		}
	}
	sort.Strings(extra)
	for _, m := range extra {
		add(m, m[strings.LastIndex(m, "/")+1:], minExtraDiskBytes)
	}
	return disks
}

// netReader：實體網卡（/sys/class/net/<名稱>/device 存在的：不含迴路、橋接、虛擬網卡、VPN 通道）的累計收送量（/proc/net/dev）
type netReader struct{}

func newNetReader() *netReader { return &netReader{} }

func (n *netReader) read() (rx, tx int64, ok bool) {
	b, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0, false
	}
	for _, c := range ParseNetDev(string(b)) {
		if _, err := os.Stat("/sys/class/net/" + c.Name + "/device"); err != nil {
			continue
		}
		rx += c.Rx
		tx += c.Tx
	}
	return rx, tx, true
}

// NetDev：/proc/net/dev 的一張網卡
type NetDev struct {
	Name   string
	Rx, Tx int64
}

// ParseNetDev（純函式）：/proc/net/dev → 各網卡的收送位元組（第 1 與第 9 個數字）
func ParseNetDev(s string) []NetDev {
	var out []NetDev
	for _, line := range strings.Split(s, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		rx, err1 := strconv.ParseInt(f[0], 10, 64)
		tx, err2 := strconv.ParseInt(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, NetDev{Name: strings.TrimSpace(name), Rx: rx, Tx: tx})
	}
	return out
}
