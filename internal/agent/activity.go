package agent

import (
	"math"
	"runtime"
	"time"

	"github.com/kevinlincg/myriagent/internal/procs"
	"github.com/kevinlincg/myriagent/internal/sysstat"
)

// 對話框的燈號（C# Hub/AgentHub.Activity.cs、ActivityRules.cs；web-protocol 的 activity）：這台的對話在某個視窗看得到（visible）時取樣——
// 硬碟＝CLI 程序樹讀寫硬碟的位元組（macOS 的 proc_pid_rusage、Linux 的 /proc/<pid>/io，本來就不含管線）、
// CPU 與記憶體＝CLI 程序樹加上它開出、脫離 CLI 的程式（背景程式的掃描找到的，帶這個對話的標記）、網路＝整台電腦硬體網卡的收送量。
// 只量執行中或有背景程式的對話的程序樹。有對話在量時每 250ms、否則每 500ms（只量網路）；上一次做完才排下一次。只送給看得到那個對話的視窗。
// 資料流（app 與 CLI 之間管線的位元組）要各家的 Provider 數，還沒接上：一律 0

const (
	activityBusyMs    = 250
	activityIdleMs    = 500
	processTableEvery = 3 * time.Second
)

// activityIntervalMs：有對話在量程序樹時的取樣間隔較短
func activityIntervalMs(anyMeasured bool) int {
	if anyMeasured {
		return activityBusyMs
	}
	return activityIdleMs
}

// procStamp：一個程序（PID＋建立時間）
type procStamp struct {
	pid   int
	start int64
}

// treeDelta：程序樹的計數器增量：同一個程序比上次多的加總；上次沒有的程序在已經取過基準時整個算進來，還沒取過基準時一律 0。計數器變小當 0
func treeDelta(prev, cur map[procStamp]uint64, primed bool) int64 {
	if !primed {
		return 0
	}
	var sum uint64
	for s, v := range cur {
		if b, ok := prev[s]; ok {
			if v >= b {
				sum += v - b
			}
		} else {
			sum += v
		}
	}
	if sum > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(sum)
}

// treeCPUDelta：程序樹用掉的 CPU 時間（奈秒）的增量：同一個程序算差；上次沒有的程序在已經取過基準時，這段時間（prevAt 到 now，Unix 微秒）才啟動的整個算進來，
// 更早就啟動的依它活著的時間裡這段時間佔的比例攤（程序表每 3 秒才重列，晚看到的工具啟動以來用的 CPU 整個算進這一格會冒出假的尖峰）。還沒取過基準時 0
func treeCPUDelta(prev, cur map[procStamp]uint64, primed bool, prevAt, now int64) int64 {
	if !primed {
		return 0
	}
	window := max(0, now-prevAt)
	var sum float64
	for s, v := range cur {
		if b, ok := prev[s]; ok {
			if v >= b {
				sum += float64(v - b)
			}
		} else if s.start >= prevAt {
			sum += float64(v)
		} else if lived := now - s.start; lived > 0 {
			sum += float64(v) * math.Min(1, float64(window)/float64(lived))
		}
	}
	if sum >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(sum)
}

// activityCPUPercent：這段時間用掉的 CPU 時間除以經過時間乘邏輯處理器數（100% 是所有核心都滿載），0～100、一位小數
func activityCPUPercent(cpuNs int64, elapsed time.Duration, processors int) float64 {
	if elapsed <= 0 || processors <= 0 || cpuNs <= 0 {
		return 0
	}
	return math.Round(math.Min(100, float64(cpuNs)*100/(float64(elapsed)*float64(processors)))*10) / 10
}

// counterDelta：單調遞增計數器的增量：沒有上一個值、或變小時 0
func counterDelta(prev *int64, cur int64) int64 {
	if prev == nil || cur < *prev {
		return 0
	}
	return cur - *prev
}

// activityItem：一個對話這次的量；Cpu、Mem 是 nil＝沒有量
type activityItem struct {
	ID   string   `json:"id"`
	Data int64    `json:"data"`
	Disk int64    `json:"disk"`
	CPU  *float64 `json:"cpu,omitempty"`
	Mem  *int64   `json:"mem,omitempty"`
}

// renderActivity：一個視窗的 activity 訊息：只放有動靜的對話（資料流或硬碟大於 0）與量了程序樹的（cpu 或 mem 有值，0 也放——波形要畫平的），
// 網路是整台電腦的（收送都是 0 時不放）。什麼都沒有時回 nil（不送）
func renderActivity(items []activityItem, net *[2]int64, ms int) map[string]any {
	list := []activityItem{}
	for _, it := range items {
		if it.Data > 0 || it.Disk > 0 || it.CPU != nil || it.Mem != nil {
			list = append(list, it)
		}
	}
	hasNet := net != nil && (net[0] > 0 || net[1] > 0)
	if len(list) == 0 && !hasNet {
		return nil
	}
	var netView any
	if hasNet {
		netView = map[string]any{"rx": net[0], "tx": net[1]}
	}
	return map[string]any{"type": "activity", "ms": max(1, ms), "conversations": list, "net": netView}
}

// ---- 取樣 ----

// activityRoot：要量的一個對話：CLI 的程序（沒有是 0）與它開出、脫離 CLI 的程式
type activityRoot struct {
	convID   string
	cliPid   int
	programs []bgProgram
}

// treeSample：一個對話這次讀到的計數器
type treeSample struct {
	disk, cpu map[procStamp]uint64
	mem       int64
	memRead   bool
	cliStamp  procStamp
}

// activityState：取樣的 goroutine 自己的狀態（不在 h.mu 底下）
type activityState struct {
	table   []procs.Proc
	tableAt time.Time
	prev    map[string]treeSample
	prevAt  time.Time
	net     *sysstat.NetTotals
	netPrev *[2]int64
	usage   func(pid int) (procs.Usage, bool)
	list    func() ([]procs.Proc, error)
	stop    chan struct{}
}

func init() {
	registerStartHook(func(h *Hub) { newActivityState().loop(h) })
}

func newActivityState() *activityState {
	return &activityState{prev: map[string]treeSample{}, net: sysstat.NewNetTotals(), usage: procs.ReadUsage, list: procs.List, stop: make(chan struct{})}
}

// targets：看得到的對話與看得到它的視窗、要量程序樹的對話（持有 h.mu）
func (h *Hub) activityTargets() (byView map[*View][]string, roots []activityRoot) {
	byView = map[*View][]string{}
	seen := map[string]bool{}
	scan := h.bg().scan
	for v := range h.views {
		if !v.ready || v.closed {
			continue
		}
		for id := range v.visible {
			c := h.convs[id]
			if c == nil || c.Meta.IsDraft {
				continue
			}
			byView[v] = append(byView[v], id)
			if seen[id] {
				continue
			}
			seen[id] = true
			programs := scan.Programs[id]
			if !c.running && len(programs) == 0 {
				continue
			}
			r := activityRoot{convID: id, programs: programs}
			if c.sess != nil && c.sess.Alive() {
				r.cliPid = c.sess.Pid()
			}
			roots = append(roots, r)
		}
	}
	return byView, roots
}

func (a *activityState) loop(h *Hub) {
	go func() {
		for {
			ms := a.tick(h)
			select {
			case <-a.stop:
				return
			case <-time.After(time.Duration(ms) * time.Millisecond):
			}
		}
	}()
}

// tick 取一次樣、送出，回下一次要等的毫秒
func (a *activityState) tick(h *Hub) int {
	h.mu.Lock()
	byView, roots := h.activityTargets()
	h.mu.Unlock()
	if len(byView) == 0 {
		a.prevAt, a.netPrev = time.Time{}, nil // 下次重新開始
		a.prev = map[string]treeSample{}
		return activityIdleMs
	}
	now := time.Now()
	ms := activityBusyMs
	if !a.prevAt.IsZero() {
		ms = int(min(10_000, max(1, now.Sub(a.prevAt).Milliseconds())))
	}
	items := map[string]activityItem{}
	if len(roots) > 0 {
		if a.table == nil || now.Sub(a.tableAt) >= processTableEvery || a.newRoot(roots) {
			if t, err := a.list(); err == nil {
				a.table, a.tableAt = t, now
			}
		}
		cur := map[string]treeSample{}
		for _, r := range roots {
			s := a.sampleTree(r)
			cur[r.convID] = s
			prev, had := a.prev[r.convID]
			primed := had && prev.cliStamp == s.cliStamp && !a.prevAt.IsZero()
			it := activityItem{ID: r.convID, Disk: treeDelta(prev.disk, s.disk, primed)}
			if primed {
				cpu := activityCPUPercent(treeCPUDelta(prev.cpu, s.cpu, true, a.prevAt.UnixMicro(), now.UnixMicro()), now.Sub(a.prevAt), runtime.NumCPU())
				it.CPU = &cpu
			}
			if s.memRead {
				m := s.mem
				it.Mem = &m
			}
			items[r.convID] = it
		}
		a.prev = cur
	} else {
		a.prev = map[string]treeSample{}
	}
	a.prevAt = now
	var netDelta *[2]int64
	if rx, tx, ok := a.net.Read(); ok {
		if a.netPrev != nil {
			netDelta = &[2]int64{counterDelta(&a.netPrev[0], rx), counterDelta(&a.netPrev[1], tx)}
		}
		a.netPrev = &[2]int64{rx, tx}
	}
	h.mu.Lock()
	for v, ids := range byView {
		list := make([]activityItem, 0, len(ids))
		for _, id := range ids {
			if it, ok := items[id]; ok {
				list = append(list, it)
			}
		}
		if msg := renderActivity(list, netDelta, ms); msg != nil {
			h.sendTo(v, msg)
		}
	}
	h.mu.Unlock()
	return activityIntervalMs(len(roots) > 0)
}

// newRoot：出現程序表裡沒有的 CLI 程序（剛開始執行的對話、CLI 重開）：要重列程序表才找得到它的子孫
func (a *activityState) newRoot(roots []activityRoot) bool {
	for _, r := range roots {
		if r.cliPid == 0 {
			continue
		}
		found := false
		for _, p := range a.table {
			if p.Pid == r.cliPid {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}
	return false
}

// sampleTree：讀一個對話的程序樹的計數器：CLI 與它的子孫（硬碟、CPU、記憶體），加上它開出的程式與它們的子孫（CPU、記憶體）；每個程序只算一次
func (a *activityState) sampleTree(r activityRoot) treeSample {
	s := treeSample{disk: map[procStamp]uint64{}, cpu: map[procStamp]uint64{}}
	counted := map[procStamp]bool{}
	read := func(p procs.Proc, disk bool) {
		st := procStamp{p.Pid, p.Start}
		if counted[st] {
			return
		}
		counted[st] = true
		u, ok := a.usage(p.Pid)
		if !ok {
			return
		}
		if disk {
			s.disk[st] = u.Disk
		}
		s.cpu[st] = u.CPUNs
		s.mem += int64(u.Mem)
		s.memRead = true
	}
	if r.cliPid != 0 {
		for _, p := range a.table {
			if p.Pid == r.cliPid {
				s.cliStamp = procStamp{p.Pid, p.Start}
				read(p, true)
				for _, d := range procs.Descendants(a.table, p) {
					read(d, true)
				}
				break
			}
		}
	}
	for _, bp := range r.programs {
		root := procs.Proc{Pid: bp.Pid, Ppid: bp.Ppid, Start: bp.Start, Name: bp.Name}
		read(root, false)
		for _, d := range procs.Descendants(a.table, root) {
			read(d, false)
		}
	}
	return s
}
