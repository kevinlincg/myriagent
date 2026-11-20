//go:build darwin

// darwinlibc：macOS 上 golang.org/x/sys/unix 沒包到、又只能經 libSystem 呼叫的幾個函式（Mach 的主機統計、proc_pidinfo 這類）。
// 不用 cgo：照 x/sys/unix 的作法，以 cgo_import_dynamic 連到 libSystem、組合語言的跳板（libc_darwin.s）跳過去，
// 經 runtime 的 syscall.syscall6 呼叫（它會處理 goroutine 進出系統呼叫）。CGO_ENABLED=0 也能編譯、交叉編譯。
package darwinlibc

import (
	"syscall"
	"unsafe"
)

//go:cgo_import_dynamic libc_mach_host_self mach_host_self "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_host_statistics host_statistics "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_host_statistics64 host_statistics64 "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_proc_pidinfo proc_pidinfo "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_proc_pidpath proc_pidpath "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_proc_pid_rusage proc_pid_rusage "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_mach_timebase_info mach_timebase_info "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_responsibility_get_pid_responsible_for_pid responsibility_get_pid_responsible_for_pid "/usr/lib/libSystem.B.dylib"

//go:linkname syscall_syscall6 syscall.syscall6
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

// 跳板的位址（libc_darwin.s 填）
var (
	libc_mach_host_self_trampoline_addr                             uintptr
	libc_host_statistics_trampoline_addr                            uintptr
	libc_host_statistics64_trampoline_addr                          uintptr
	libc_proc_pidinfo_trampoline_addr                               uintptr
	libc_proc_pidpath_trampoline_addr                               uintptr
	libc_proc_pid_rusage_trampoline_addr                            uintptr
	libc_mach_timebase_info_trampoline_addr                         uintptr
	libc_responsibility_get_pid_responsible_for_pid_trampoline_addr uintptr
)

func call(fn uintptr, a ...uintptr) uintptr {
	var args [6]uintptr
	copy(args[:], a)
	r, _, _ := syscall_syscall6(fn, args[0], args[1], args[2], args[3], args[4], args[5])
	return r
}

// HostCPULoad：所有核心加總的累計 tick（HOST_CPU_LOAD_INFO：user、system、idle、nice）；讀不到 ok 是 false
func HostCPULoad() (ticks [4]uint32, ok bool) {
	host := call(libc_mach_host_self_trampoline_addr)
	count := uint32(4)
	r := call(libc_host_statistics_trampoline_addr, host, 3, uintptr(unsafe.Pointer(&ticks[0])), uintptr(unsafe.Pointer(&count)))
	return ticks, r == 0 && count >= 4
}

// HostVMInfo64：vm_statistics64（以 32 位元整數為單位，最多 n 格）；讀不到回 nil
func HostVMInfo64(n int) []uint32 {
	host := call(libc_mach_host_self_trampoline_addr)
	buf := make([]uint32, n)
	count := uint32(n)
	if r := call(libc_host_statistics64_trampoline_addr, host, 4, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&count))); r != 0 {
		return nil
	}
	return buf[:count]
}

// ProcPidinfo：proc_pidinfo；回系統寫入的位元組數（失敗是 0 或負的）
func ProcPidinfo(pid, flavor int, arg uint64, buf []byte) int {
	if len(buf) == 0 {
		return 0
	}
	return int(int32(call(libc_proc_pidinfo_trampoline_addr, uintptr(pid), uintptr(flavor), uintptr(arg), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))))
}

// ProcPidpath：程序的執行檔完整路徑；讀不到回空字串
func ProcPidpath(pid int) string {
	buf := make([]byte, 4096)
	n := int(int32(call(libc_proc_pidpath_trampoline_addr, uintptr(pid), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))))
	if n <= 0 || n > len(buf) {
		return ""
	}
	return string(buf[:n])
}

// RusageV2 是 rusage_info_v2 裡用得到的欄位（時間已換成奈秒）
type RusageV2 struct {
	UserNs, SystemNs   uint64
	PhysFootprint      uint64
	DiskRead, DiskWrit uint64
}

// ProcPidRusage：proc_pid_rusage(RUSAGE_INFO_V2)；讀不到 ok 是 false。
// 版面（<sys/resource.h>）：uuid 16 位元組之後全是 uint64——user_time@16、system_time@24、phys_footprint@72、diskio_bytesread@144、diskio_byteswritten@152；
// 時間的單位是 Mach 絕對時間（Apple 晶片上不是奈秒），以 mach_timebase_info 換算
func ProcPidRusage(pid int) (RusageV2, bool) {
	var buf [160]byte
	if r := call(libc_proc_pid_rusage_trampoline_addr, uintptr(pid), 2, uintptr(unsafe.Pointer(&buf[0]))); r != 0 {
		return RusageV2{}, false
	}
	u64 := func(off int) uint64 { return *(*uint64)(unsafe.Pointer(&buf[off])) }
	numer, denom := timebase()
	conv := func(t uint64) uint64 { return uint64(float64(t) * float64(numer) / float64(denom)) }
	return RusageV2{UserNs: conv(u64(16)), SystemNs: conv(u64(24)), PhysFootprint: u64(72), DiskRead: u64(144), DiskWrit: u64(152)}, true
}

var tbNumer, tbDenom uint32

func timebase() (uint32, uint32) {
	if tbDenom == 0 {
		var info [2]uint32
		if call(libc_mach_timebase_info_trampoline_addr, uintptr(unsafe.Pointer(&info[0]))) == 0 && info[1] != 0 {
			tbNumer, tbDenom = info[0], info[1]
		} else {
			tbNumer, tbDenom = 1, 1
		}
	}
	return tbNumer, tbDenom
}

// Responsible：系統記的「負責的程序」（app 開出的程序脫離父程序之後也不會變）；讀不到回 -1
func Responsible(pid int) int {
	r := int(int32(call(libc_responsibility_get_pid_responsible_for_pid_trampoline_addr, uintptr(pid))))
	if r <= 0 {
		return -1
	}
	return r
}
