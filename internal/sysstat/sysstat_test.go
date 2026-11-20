package sysstat

import (
	"testing"
	"time"
)

func TestRules(t *testing.T) {
	if p, ok := CPUPercent(CPUTimes{Idle: 100, Kernel: 200, User: 100}, CPUTimes{Idle: 150, Kernel: 300, User: 200}); !ok || p != 75 {
		t.Fatalf("CPUPercent %v %v", p, ok)
	}
	if _, ok := CPUPercent(CPUTimes{Idle: 100}, CPUTimes{Idle: 50}); ok {
		t.Fatal("計數倒退應該不算")
	}
	if Rate(1000, 3000, 2*time.Second) != 1000 || Rate(3000, 1000, time.Second) != 0 || Rate(0, 10, 0) != 0 {
		t.Fatal("Rate")
	}
	if DiskName("Macintosh HD") != "Macintosh HD" || DiskName("Happy 2026.11.4-arm64") != "Happy 2026.11.4…" || DiskName(" \x01 ") != "" {
		t.Fatalf("DiskName %q", DiskName("Happy 2026.11.4-arm64"))
	}
	if n := CPUName("  Intel(R)   Core(TM) i9 \t "); n == nil || *n != "Intel(R) Core(TM) i9" {
		t.Fatal("CPUName")
	}
	if CPUName("   ") != nil {
		t.Fatal("空的 CPU 名稱")
	}
}

func TestSamplerReadsThisMachine(t *testing.T) {
	s := NewSampler()
	time.Sleep(300 * time.Millisecond)
	st := s.Sample()
	if st.MemTotal <= 1 || st.MemUsed <= 0 || st.MemUsed > st.MemTotal {
		t.Fatalf("記憶體不對：%d/%d", st.MemUsed, st.MemTotal)
	}
	if st.CPU < 0 || st.CPU > 100 || st.Cores < 1 {
		t.Fatalf("CPU 不對：%v（%d 核）", st.CPU, st.Cores)
	}
	if len(st.Disks) == 0 || st.Disks[0].Total <= 0 {
		t.Fatalf("磁碟不對：%+v", st.Disks)
	}
	if st.UptimeSec == nil || *st.UptimeSec <= 0 {
		t.Fatal("開機時間")
	}
	if _, _, ok := NewNetTotals().Read(); !ok {
		t.Fatal("讀不到網路")
	}
}
