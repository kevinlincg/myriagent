//go:build darwin

package darwinlibc

import (
	"os"
	"strings"
	"testing"
)

func TestCallsWork(t *testing.T) {
	if _, ok := HostCPULoad(); !ok {
		t.Fatal("讀不到 CPU 累計")
	}
	if vm := HostVMInfo64(38); len(vm) < 36 {
		t.Fatalf("vm_statistics64 太短：%d", len(vm))
	}
	exe, _ := os.Executable()
	if p := ProcPidpath(os.Getpid()); p == "" || !strings.HasSuffix(exe, p[strings.LastIndex(p, "/"):]) {
		t.Fatalf("執行檔路徑 %q（os.Executable %q）", p, exe)
	}
	u, ok := ProcPidRusage(os.Getpid())
	if !ok || u.PhysFootprint == 0 || u.UserNs == 0 {
		t.Fatalf("rusage 不對：%+v", u)
	}
	if Responsible(os.Getpid()) <= 0 {
		t.Fatal("讀不到負責的程序")
	}
}
