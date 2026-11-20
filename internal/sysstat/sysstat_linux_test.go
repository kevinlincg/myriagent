//go:build linux

package sysstat

import "testing"

func TestParseLinux(t *testing.T) {
	c, ok := ParseProcStatCPU("cpu  10 2 30 400 5 6 7 8 0 0\ncpu0 1 1 1 1 1 1 1 1\n")
	if !ok || c.Idle != 405 || c.Kernel != 30+6+7+8+405 || c.User != 12 {
		t.Fatalf("%+v", c)
	}
	used, total, ok := ParseMeminfo("MemTotal:  1000 kB\nMemFree: 100 kB\nMemAvailable:   400 kB\n")
	if !ok || total != 1000*1024 || used != 600*1024 {
		t.Fatal("meminfo")
	}
	devs := ParseNetDev("Inter-|   Receive\n face |bytes\n  eth0: 100 1 0 0 0 0 0 0 200 2 0 0 0 0 0 0\n    lo: 5 0 0 0 0 0 0 0 5 0 0 0 0 0 0 0\n")
	if len(devs) != 2 || devs[0].Name != "eth0" || devs[0].Rx != 100 || devs[0].Tx != 200 {
		t.Fatalf("%+v", devs)
	}
}
