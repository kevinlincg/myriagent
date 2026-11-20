// Package sysstat 讀這台電腦的狀態（電腦狀態面板的 deviceStats；C# 的 SystemStatus/NativeStats、NetworkTotals 與 Protocol 的 DeviceStatsSampler）：
// CPU 使用率、記憶體、磁碟、網路速率、CPU 名稱、開機時間。不用 cgo：Linux 讀 /proc，macOS 用 sysctl 與 libSystem（internal/darwinlibc）。
// 顯示卡與 CPU 溫度 macOS／Linux 沒有便宜又通用的讀法，先不提供（面板上那兩格不顯示）
package sysstat

import (
	"math"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	MaxDisks          = 8
	MaxDiskNameLength = 16
	MaxCPUNameLength  = 80
	MaxBytes          = int64(1) << 60
	MaxRate           = int64(1) << 50
	MaxUptimeSec      = int64(100 * 365 * 24 * 3600)
	// /Volumes 底下比這個小的不列（多半是安裝程式的磁碟映像檔）
	minExtraDiskBytes = int64(1) << 30
)

// CPUTimes：所有核心加總的累計時間（單位不拘，只拿來算比例）。Kernel 含 Idle（同 Windows 的 GetSystemTimes）
type CPUTimes struct {
	Idle, Kernel, User uint64
}

// Disk：一個磁碟的用量（位元組）
type Disk struct {
	Name  string `json:"name"`
	Used  int64  `json:"used"`
	Total int64  `json:"total"`
}

// Stats：一次取樣（stat1 的 DeviceStats；延遲 rttMs 由同步量，這裡沒有）
type Stats struct {
	CPU       float64
	MemUsed   int64
	MemTotal  int64
	Disks     []Disk
	NetRx     int64
	NetTx     int64
	Cores     int
	CPUName   *string
	UptimeSec *int64
}

// CPUPercent：兩次累計之間的 CPU 使用率（0～100，一位小數）：忙碌＝(kernel＋user)−idle。計數倒退、沒有經過時間回 false
func CPUPercent(prev, cur CPUTimes) (float64, bool) {
	if cur.Idle < prev.Idle || cur.Kernel < prev.Kernel || cur.User < prev.User {
		return 0, false
	}
	idle := cur.Idle - prev.Idle
	total := cur.Kernel - prev.Kernel + (cur.User - prev.User)
	if total == 0 {
		return 0, false
	}
	busy := uint64(0)
	if total > idle {
		busy = total - idle
	}
	return round1(math.Max(0, math.Min(100, float64(busy)*100/float64(total)))), true
}

// Rate：累計位元組 → 每秒位元組（四捨五入）。計數倒退、經過時間不是正的回 0；夾在 MaxRate 以內
func Rate(prev, cur int64, elapsed time.Duration) int64 {
	if elapsed <= 0 || cur < prev {
		return 0
	}
	r := math.Round(float64(cur-prev) * float64(time.Second) / float64(elapsed))
	if r >= float64(MaxRate) {
		return MaxRate
	}
	return int64(r)
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// Clean：去掉控制字元與格式字元、前後空白；空的或超過 max 字回空字串（同 C# 的 LabelRules.Clean）
func Clean(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if out == "" || utf8.RuneCountInString(out) > max {
		return ""
	}
	return out
}

// DiskName：磁碟的名稱：清理、超過 16 字截斷加「…」；空的回空字串
func DiskName(label string) string {
	clean := Clean(label, math.MaxInt32)
	if clean == "" {
		return ""
	}
	if utf8.RuneCountInString(clean) <= MaxDiskNameLength {
		return clean
	}
	cut := Clean(string([]rune(clean)[:MaxDiskNameLength-1]), MaxDiskNameLength-1)
	if cut == "" {
		return ""
	}
	return cut + "…"
}

// CPUName：硬體名稱：連續空白併成一個、清理、截斷到 80 字；空的回 nil
func CPUName(raw string) *string {
	collapsed := strings.Join(strings.Fields(raw), " ")
	if collapsed == "" {
		return nil
	}
	if utf8.RuneCountInString(collapsed) > MaxCPUNameLength {
		collapsed = string([]rune(collapsed)[:MaxCPUNameLength])
	}
	if c := Clean(collapsed, MaxCPUNameLength); c != "" {
		return &c
	}
	return nil
}

// Sampler：讀這台的狀態。CPU 與網路以兩次累計的差算，所以每次的值是「上一次取樣到這一次」的平均（第一次是建構到第一次取樣）。
// 不是執行緒安全：呼叫端自己序列化。某一項讀不到時那一項給保守值（0、空或 nil）
type Sampler struct {
	prevCPU *CPUTimes
	prevNet *[2]int64
	prevAt  time.Time
	cpuName *string
	net     *netReader
}

// NewSampler 建一個取樣器（建構時讀一次累計值）
func NewSampler() *Sampler {
	s := &Sampler{prevAt: time.Now(), cpuName: CPUName(readCPUName()), net: newNetReader()}
	if c, ok := readCPUTimes(); ok {
		s.prevCPU = &c
	}
	if rx, tx, ok := s.net.read(); ok {
		s.prevNet = &[2]int64{rx, tx}
	}
	return s
}

// Sample 取一次樣
func (s *Sampler) Sample() Stats {
	now := time.Now()
	elapsed := now.Sub(s.prevAt)
	s.prevAt = now
	st := Stats{Cores: runtime.NumCPU(), CPUName: s.cpuName, Disks: readDisks()}
	if c, ok := readCPUTimes(); ok {
		if s.prevCPU != nil {
			if p, ok := CPUPercent(*s.prevCPU, c); ok {
				st.CPU = p
			}
		}
		s.prevCPU = &c
	}
	if rx, tx, ok := s.net.read(); ok {
		if s.prevNet != nil {
			st.NetRx, st.NetTx = Rate(s.prevNet[0], rx, elapsed), Rate(s.prevNet[1], tx, elapsed)
		}
		s.prevNet = &[2]int64{rx, tx}
	}
	if used, total, ok := readMemory(); ok && total > 0 {
		total = min(total, MaxBytes)
		st.MemUsed, st.MemTotal = max(0, min(used, total)), total
	} else {
		st.MemUsed, st.MemTotal = 0, 1
	}
	if up, ok := readUptime(); ok {
		up = min(up, MaxUptimeSec)
		st.UptimeSec = &up
	}
	return st
}

// NetTotals：整台電腦硬體網卡的累計收送位元組（對話框燈號的網路；每次呼叫重讀）。讀不到 ok 是 false
type NetTotals struct{ r *netReader }

func NewNetTotals() *NetTotals { return &NetTotals{r: newNetReader()} }

func (n *NetTotals) Read() (rx, tx int64, ok bool) { return n.r.read() }

// addDisk：加一個磁碟（名稱重複、太小、讀不到的略過）
func addDisk(disks []Disk, label string, total, free, minBytes int64) []Disk {
	if len(disks) >= MaxDisks || total <= 0 || total < minBytes || total > MaxBytes {
		return disks
	}
	name := DiskName(label)
	if name == "" {
		return disks
	}
	for _, d := range disks {
		if d.Name == name {
			return disks
		}
	}
	return append(disks, Disk{Name: name, Used: max(0, min(total-free, total)), Total: total})
}
