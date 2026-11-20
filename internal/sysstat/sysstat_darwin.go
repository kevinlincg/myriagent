//go:build darwin

package sysstat

import (
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kevinlincg/myriagent/internal/darwinlibc"
	"golang.org/x/sys/unix"
)

// vm_statistics64 裡各欄位的位置（以 32 位元整數為單位，見 <mach/vm_statistics.h>；C# 版同）
const (
	vmInfo64Count    = 38
	vmWireCount      = 3
	vmPurgeableCount = 22
	vmCompressorPage = 32
	vmInternalPage   = 35
)

// readCPUTimes：HOST_CPU_LOAD_INFO 的 user、system、idle、nice；把 idle 加進 Kernel（同 Windows 的定義）、nice 算在 User
func readCPUTimes() (CPUTimes, bool) {
	t, ok := darwinlibc.HostCPULoad()
	if !ok {
		return CPUTimes{}, false
	}
	user, system, idle, nice := uint64(t[0]), uint64(t[1]), uint64(t[2]), uint64(t[3])
	return CPUTimes{Idle: idle, Kernel: system + idle, User: user + nice}, true
}

// readMemory：已用的實體記憶體，和「活動監視器」的「已使用的記憶體」同一個算法（app 記憶體＋系統核心保留＋已壓縮）
func readMemory() (used, total int64, ok bool) {
	size, err := unix.SysctlUint64("hw.memsize")
	if err != nil || size == 0 {
		return 0, 0, false
	}
	pages := darwinlibc.HostVMInfo64(vmInfo64Count)
	if len(pages) <= vmInternalPage {
		return 0, 0, false
	}
	app := max(0, int64(pages[vmInternalPage])-int64(pages[vmPurgeableCount]))
	usedPages := app + int64(pages[vmWireCount]) + int64(pages[vmCompressorPage])
	total = int64(min(size, uint64(MaxBytes)))
	return max(0, min(usedPages*int64(os.Getpagesize()), total)), total, true
}

func readCPUName() string {
	s, _ := unix.Sysctl("machdep.cpu.brand_string")
	return s
}

func readUptime() (int64, bool) {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil || tv.Sec <= 0 {
		return 0, false
	}
	return max(0, time.Now().Unix()-int64(tv.Sec)), true
}

// readDisks：開機磁碟「/」（名稱取 /Volumes 裡指到它的那個捷徑，例如 Macintosh HD；沒有就叫「/」）加上掛在 /Volumes 底下的其他磁碟（外接硬碟）
func readDisks() []Disk {
	rootLabel := "/"
	var mounts []string
	entries, _ := os.ReadDir("/Volumes")
	for _, e := range entries {
		full := filepath.Join("/Volumes", e.Name())
		if e.Type()&os.ModeSymlink != 0 {
			if t, err := os.Readlink(full); err == nil && t == "/" {
				rootLabel = e.Name()
			}
		} else if e.IsDir() {
			mounts = append(mounts, full)
		}
	}
	sort.Strings(mounts)
	var disks []Disk
	add := func(mount, label string, minBytes int64) {
		var st unix.Statfs_t
		if unix.Statfs(mount, &st) != nil {
			return
		}
		// 只算真的掛在那裡的（沒掛上的空資料夾會讀到開機磁碟）
		if mount != "/" && strings.TrimRight(cstr(st.Mntonname[:]), "/") != mount {
			return
		}
		bs := int64(st.Bsize)
		disks = addDisk(disks, label, int64(st.Blocks)*bs, int64(st.Bfree)*bs, minBytes)
	}
	add("/", rootLabel, 0)
	for _, m := range mounts {
		add(m, filepath.Base(m), minExtraDiskBytes)
	}
	return disks
}

func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// netReader：實體網卡（名稱 en 開頭、正在使用的：有線與 Wi-Fi）的累計收送量；迴路、VPN 的通道（utun）、AirDrop（awdl、llw）、橋接都不算。
// 來源是 sysctl 的 NET_RT_IFLIST2（if_data64）；macOS 給一般程式的位元組計數只有低 32 位元（2026-11-20 實測：和 netstat -ib 差整數倍的 2^32），
// 所以每張網卡記著上一次的值、以 32 位元繞回累加成 64 位元的總量
type netReader struct {
	mu    sync.Mutex
	last  map[string][2]uint64
	total [2]int64
	ready bool
}

func newNetReader() *netReader { return &netReader{last: map[string][2]uint64{}} }

func (n *netReader) read() (rx, tx int64, ok bool) {
	b, err := syscall.RouteRIB(syscall.NET_RT_IFLIST2, 0)
	if err != nil {
		return 0, 0, false
	}
	ifs, err := net.Interfaces()
	if err != nil {
		return 0, 0, false
	}
	names := map[int]net.Interface{}
	for _, i := range ifs {
		names[i.Index] = i
	}
	cur := map[string][2]uint64{}
	for _, c := range parseIfList2(b) {
		i, ok := names[c.index]
		if !ok || !strings.HasPrefix(i.Name, "en") || i.Flags&net.FlagUp == 0 {
			continue
		}
		cur[i.Name] = [2]uint64{c.ibytes, c.obytes}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for name, v := range cur {
		if prev, ok := n.last[name]; ok && n.ready {
			n.total[0] += int64(wrapDelta(prev[0], v[0]))
			n.total[1] += int64(wrapDelta(prev[1], v[1]))
		}
	}
	n.last, n.ready = cur, true
	return n.total[0], n.total[1], true
}

// wrapDelta：計數器的增量；變小時當成 32 位元繞回（值都在 32 位元以內時），否則 0（網卡重設）
func wrapDelta(prev, cur uint64) uint64 {
	if cur >= prev {
		return cur - prev
	}
	if prev < 1<<32 && cur < 1<<32 {
		return cur + (1 << 32) - prev
	}
	return 0
}

type ifCounters struct {
	index          int
	ibytes, obytes uint64
}

// parseIfList2（純函式）：NET_RT_IFLIST2 的內容裡每個 RTM_IFINFO2（if_msghdr2）的介面編號與收送位元組。
// 版面：msglen u16、version、type（0x12）、addrs、flags、index u16@12、…、if_data64@32（ibytes@32+64、obytes@32+72）
func parseIfList2(b []byte) []ifCounters {
	var out []ifCounters
	for off := 0; off+4 <= len(b); {
		l := int(binary.LittleEndian.Uint16(b[off:]))
		if l < 4 || off+l > len(b) {
			break
		}
		if b[off+3] == 0x12 && l >= 112 {
			out = append(out, ifCounters{
				index:  int(binary.LittleEndian.Uint16(b[off+12:])),
				ibytes: binary.LittleEndian.Uint64(b[off+96:]),
				obytes: binary.LittleEndian.Uint64(b[off+104:]),
			})
		}
		off += l
	}
	return out
}
