// Package procs 列出這台電腦（這個使用者）的程序、讀它們的命令列與環境變數、量用量、結束程序樹。
// 不用 cgo：Linux 讀 /proc，macOS 用 sysctl（golang.org/x/sys/unix）與 libSystem 的 proc_pidinfo 這類函式（internal/darwinlibc）。
// 程序一律以 PID＋建立時間認（PID 會被重複使用）。對應 C# 的 Background/NativeProcessSnapshot、NativeProgramControl 與 Agents/ProcessTreeIo。
package procs

import (
	"os"
	"strings"
	"syscall"
)

// Proc 是程序表的一列。Start 是建立時間（Unix 微秒；Linux 的精度是 10 毫秒），和 PID 一起認程序
type Proc struct {
	Pid   int
	Ppid  int
	Start int64
	Name  string
}

// Usage：一個程序的累計用量。CPUNs 是使用者＋核心時間（奈秒）、Mem 是記憶體（macOS 的 phys_footprint、Linux 的私人常駐 RssAnon）、
// Disk 是讀寫硬碟的累計位元組（不含管線與網路）
type Usage struct {
	CPUNs uint64
	Mem   uint64
	Disk  uint64
}

// Info：Inspect 的結果。Env 是 NAME=VALUE 的清單；EnvHidden＝系統不給讀這個程序的環境變數（macOS 的系統內建程式，見 Inspect）
type Info struct {
	Args      []string
	Env       []string
	EnvHidden bool
}

// EnvValue 找環境變數的值（名稱分大小寫，Unix 的環境變數）；沒有回空字串
func EnvValue(env []string, name string) string {
	prefix := name + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):]
		}
	}
	return ""
}

// Alive：那個 PID 現在還是同一個程序（建立時間相同）而且還在執行（不是殭屍）
func Alive(pid int, start int64) bool {
	s, ok := StartTime(pid)
	return ok && s == start && !zombie(pid)
}

// Descendants：程序表裡 root 的子孫（父程序要建立得比子程序早才算，PID 被重複使用時不會誤接），不含 root 自己
func Descendants(table []Proc, root Proc) []Proc {
	children := map[int][]Proc{}
	for _, p := range table {
		if p.Pid != p.Ppid {
			children[p.Ppid] = append(children[p.Ppid], p)
		}
	}
	var out []Proc
	seen := map[int]bool{root.Pid: true}
	queue := []Proc{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, ch := range children[cur.Pid] {
			if seen[ch.Pid] || ch.Start < cur.Start {
				continue
			}
			seen[ch.Pid] = true
			out = append(out, ch)
			queue = append(queue, ch)
		}
	}
	return out
}

// KillTree 結束這個程式與它的子孫（SIGKILL，同 C# 的 TerminateProcess，不送 Ctrl+C）：先擷取樹（根還活著，子孫才接得上）再結束。
// 那個 PID 已經不是它、已經結束或結束不了回 false
func KillTree(pid int, start int64) bool {
	if !Alive(pid, start) {
		return false
	}
	var tree []Proc
	if table, err := List(); err == nil {
		tree = Descendants(table, Proc{Pid: pid, Start: start})
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		return false
	}
	for _, p := range tree {
		if Alive(p.Pid, p.Start) {
			_ = syscall.Kill(p.Pid, syscall.SIGKILL)
		}
	}
	return true
}

var ownPid = os.Getpid()

// Self：這個程序的 PID
func Self() int { return ownPid }
